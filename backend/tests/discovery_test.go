package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/simulation"
	"context"
	"testing"
	"time"
)

func TestDiscoveryRegistrationAndGatewayIsolation(t *testing.T) {
	f := setup(t)
	_, auth, a := f.account(t)
	_, _, other := f.account(t)
	ctx := context.Background()
	g1, _, e := f.service.CreateGateway(ctx, a, "Discovery A", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	g2, _, e := f.service.CreateGateway(ctx, a, "Discovery B", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	for _, g := range []string{g1.ID, g2.ID} {
		if _, e = f.repo.CapturePacket(ctx, a.TenantID, g, simulation.Packet(0, time.Now())); e != nil {
			t.Fatal(e)
		}
	}
	api := httpapi.New(f.cfg, f.service, f.repo)
	code, out, _ := req(t, api, "GET", "/api/v1/discovery", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("discovery: %d %v", code, out)
	}
	found := 0
	for _, raw := range out["items"].([]any) {
		d := raw.(map[string]any)
		if d["external_id"] == "f00000000001" {
			found++
			p, ok := d["profile"].(map[string]any)
			if !ok || p["model"] != "S1" || p["image"] != "/devices/minew-s1.png" {
				t.Fatalf("reported model/photo: %v", d)
			}
		}
	}
	if found != 2 {
		t.Fatalf("expected discovery under both gateways: %d", found)
	}
	d, e := f.service.CreateDevice(ctx, a, g1.ID, "Registered S1", "F00000000001", "minew-s1-pending@1")
	if e != nil {
		t.Fatal(e)
	}
	for _, g := range []string{g1.ID, g2.ID} {
		items, e := f.repo.DiscoverDevices(ctx, a, g, time.Now().Add(-time.Hour))
		if e != nil {
			t.Fatal(e)
		}
		for _, item := range items {
			if item.ExternalID == "f00000000001" {
				t.Fatal("registered device still discovered", g)
			}
		}
	}
	if e = f.service.RemoveDevice(ctx, a, d.ID); e != nil {
		t.Fatal(e)
	}
	items, e := f.repo.DiscoverDevices(ctx, a, g2.ID, time.Now().Add(-time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	found = 0
	for _, item := range items {
		if item.ExternalID == "f00000000001" {
			found++
		}
	}
	if found != 1 {
		t.Fatal("withdrawn device should be discoverable again")
	}
	items, e = f.repo.DiscoverDevices(ctx, other, g1.ID, time.Now().Add(-time.Hour))
	if e != nil || len(items) != 0 {
		t.Fatal("cross tenant discovery", e)
	}
	items, e = f.repo.DiscoverDevices(ctx, a, g1.ID, time.Now().Add(time.Hour))
	if e != nil || len(items) != 0 {
		t.Fatal("discovery window", e)
	}
	// Anything that is not a supported catalog model (phones, foreign beacons) is left out: they rotate
	// their MAC and would otherwise pile up as endless "new devices". The count of hidden ones is reported.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g1.ID, []byte(`[{"mac":"aabbccddeeff","rawData":"020106"}]`)); e != nil {
		t.Fatal(e)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/discovery", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	for _, raw := range out["items"].([]any) {
		if raw.(map[string]any)["external_id"] == "aabbccddeeff" {
			t.Fatal("unknown advertisement shown by default")
		}
	}
	if n, _ := out["hidden_unknown"].(float64); n < 1 || out["window_minutes"] != float64(15) {
		t.Fatalf("hidden count / window: %v %v", out["hidden_unknown"], out["window_minutes"])
	}
	// A real kit beacon (C10/B7) whose latest uplink carried only its iBeacon slot is still recognised:
	// the model survives in the stream name that an earlier info frame set.
	ibeacon := `[{"mac":"c30000aa0001","rawData":"0201061aff4c000215e2c56db5dffb48d2b060d0f5a71096e000010002c5"}]`
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g1.ID, []byte(ibeacon)); e != nil {
		t.Fatal(e)
	}
	shown := func(id string) (map[string]any, bool) {
		code, out, _ := req(t, api, "GET", "/api/v1/discovery", "Bearer "+auth.AccessToken, "", "", nil)
		if code != 200 {
			t.Fatal(code, out)
		}
		for _, raw := range out["items"].([]any) {
			if item := raw.(map[string]any); item["external_id"] == id {
				return item, true
			}
		}
		return nil, false
	}
	if _, ok := shown("c30000aa0001"); ok {
		t.Fatal("bare iBeacon without a model shown by default")
	}
	if _, e = f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET name='Minew C10' WHERE gateway_id=$1 AND external_id='c30000aa0001'`, g1.ID); e != nil {
		t.Fatal(e)
	}
	if item, ok := shown("c30000aa0001"); !ok || item["model"] != "C10" || item["profile"] == nil {
		t.Fatalf("kit beacon with a known model hidden or unmatched: %v", item)
	}
	// The physical S1 reports "PLUS" in its info frame (captured from the real kit on the production MG3):
	// it is a Minew device, listed with the S1 profile.
	plus := `[{"mac":"c30000393fe5","rawData":"0201060303e1ff1016e1ffa10864e53f390000c3504c5553"}]`
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g1.ID, []byte(plus)); e != nil {
		t.Fatal(e)
	}
	if item, ok := shown("c30000393fe5"); !ok || item["model"] != "PLUS" || item["profile"] == nil || item["profile"].(map[string]any)["model"] != "S1" {
		t.Fatalf("real S1 (PLUS) not listed as S1: %v", item)
	}
	if code, _, _ = req(t, api, "GET", "/api/v1/discovery?minutes=0", "Bearer "+auth.AccessToken, "", "", nil); code != 400 {
		t.Fatal("minutes=0 accepted", code)
	}
	// ?all=1 still lists them, without inventing a model.
	code, out, _ = req(t, api, "GET", "/api/v1/discovery?all=1", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	found = 0
	for _, raw := range out["items"].([]any) {
		item := raw.(map[string]any)
		if item["external_id"] == "aabbccddeeff" {
			found++
			if item["profile"] != nil {
				t.Fatal("unknown model misidentified", item)
			}
		}
	}
	if found != 1 {
		t.Fatal("unknown advertisement missing")
	}
}
