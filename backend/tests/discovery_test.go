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
	// Unknown advertisements are still discoverable without inventing a model.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g1.ID, []byte(`[{"mac":"aabbccddeeff","rawData":"020106"}]`)); e != nil {
		t.Fatal(e)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/discovery", "Bearer "+auth.AccessToken, "", "", nil)
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
