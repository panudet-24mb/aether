package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"testing"
	"time"
)

// A wearable is heard by two gateways: presence follows the stronger one, a button press is recorded once,
// and the device goes offline only when no gateway hears it.
func TestRoamingWearableAcrossGateways(t *testing.T) {
	f := setup(t)
	_, authA, a := f.account(t)
	_, authB, _ := f.account(t)
	ctx := context.Background()
	api := httpapi.New(f.cfg, f.service, f.repo)
	zoneA, _, e := f.service.CreateGateway(ctx, a, "Zone A", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	zoneB, _, e := f.service.CreateGateway(ctx, a, "Zone B", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	const b10 = "f00000000007"
	d, e := f.service.CreateDevice(ctx, a, zoneA.ID, "Nurse call", b10, "minew-b10-pending@1")
	if e != nil {
		t.Fatal(e)
	}
	code, _, _ := req(t, api, "POST", "/api/v1/devices/"+d.ID+"/roaming", "Bearer "+authB.AccessToken, "", "", map[string]any{"roaming": true})
	if code != 404 {
		t.Fatalf("another tenant toggled roaming: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/devices/"+d.ID+"/roaming", "Bearer "+authA.AccessToken, "", "", map[string]any{"roaming": true})
	if code != 204 {
		t.Fatalf("roaming on: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/rules", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "Offline 60", "event_type": "offline", "severity": "warning", "scope": map[string]any{"offline_after_sec": 60}})
	if code != 201 {
		t.Fatalf("rule: %d", code)
	}

	// Find a step where both zones hear the B10 (offset 40), it is idle, and the next step presses the button.
	step := -1
	for s := 1; s < 600; s++ {
		_, aOK, _, bOK := simulation.WalkRSSI(s, 40)
		_, aOK2, _, bOK2 := simulation.WalkRSSI(s+1, 40)
		if aOK && bOK && aOK2 && bOK2 && !simulation.B10Pressed(s) && simulation.B10Pressed(s+1) {
			step = s
			break
		}
	}
	if step < 0 {
		t.Fatal("no overlapping press step in the walk")
	}
	now := time.Now().UTC().Add(-10 * time.Second)
	for i, at := range []time.Time{now, now.Add(5 * time.Second)} {
		if _, e = f.repo.CapturePacket(ctx, a.TenantID, zoneA.ID, simulation.Packet(step+i, at)); e != nil {
			t.Fatal(e)
		}
		if _, e = f.repo.CapturePacket(ctx, a.TenantID, zoneB.ID, simulation.ZonePacket(step+i, at)); e != nil {
			t.Fatal(e)
		}
	}
	events, e := f.repo.ListEvents(ctx, a, b10, 50)
	if e != nil {
		t.Fatal(e)
	}
	presses := 0
	for _, ev := range events {
		if ev.EventType == domain.EventButton {
			presses++
		}
	}
	if presses != 1 {
		t.Fatalf("a press heard by two gateways must be one event, got %d: %+v", presses, events)
	}

	p, e := f.repo.Presence(ctx, a, b10)
	if e != nil || !p.Roaming || len(p.Sightings) != 2 || p.Current == nil {
		t.Fatalf("presence: %+v %v", p, e)
	}
	// The zone is sticky: zone A heard it first, and two uplinks are not enough dwell time to move it.
	if p.Current.GatewayID != zoneA.ID || p.Since == nil {
		t.Fatalf("current zone must be the first gateway until another one is clearly stronger for the dwell time: %+v", p)
	}
	if evs, _ := f.repo.ListEvents(ctx, a, b10, 50); countType(evs, domain.EventZone) != 1 {
		t.Fatalf("the first zone assignment is one event: %+v", evs)
	}
	code, out, _ := req(t, api, "GET", "/api/v1/presence/"+b10, "Bearer "+authB.AccessToken, "", "", nil)
	if code != 200 || len(out["sightings"].([]any)) != 0 {
		t.Fatalf("presence leaked across tenants: %d %v", code, out)
	}

	// Zone A stops hearing it, zone B still does: not offline.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET last_seen=now()-interval '10 minutes' WHERE external_id=$1 AND gateway_id=$2`, b10, zoneA.ID); e != nil {
		t.Fatal(e)
	}
	worker := &alerts.Worker{Store: f.repo, Sender: httpapi.NewSender(f.cfg), Secrets: f.service.Secrets, Batch: 10}
	worker.Tick(ctx, time.Now().UTC())
	if evs, _ := f.repo.ListEvents(ctx, a, b10, 50); countType(evs, domain.EventOffline) != 0 {
		t.Fatalf("roaming device heard elsewhere must not go offline: %+v", evs)
	}
	p, _ = f.repo.Presence(ctx, a, b10)
	if p.Current == nil || p.Current.GatewayID != zoneB.ID {
		t.Fatalf("presence must fall back to the zone that still hears it: %+v", p.Current)
	}
	// Nobody hears it: exactly one offline episode.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET last_seen=now()-interval '5 minutes' WHERE external_id=$1 AND gateway_id=$2`, b10, zoneB.ID); e != nil {
		t.Fatal(e)
	}
	worker.Tick(ctx, time.Now().UTC())
	worker.Tick(ctx, time.Now().UTC())
	if evs, _ := f.repo.ListEvents(ctx, a, b10, 50); countType(evs, domain.EventOffline) != 1 {
		t.Fatalf("one offline episode expected: %+v", evs)
	}
	if p, _ = f.repo.Presence(ctx, a, b10); p.Current != nil {
		t.Fatalf("no current zone when nobody hears it: %+v", p.Current)
	}
	// The person comes back through zone A although the offline episode sits on zone B's stream: one `online`.
	back := -1
	for s := 1; s < 600; s++ {
		if _, aOK, _, _ := simulation.WalkRSSI(s, 40); aOK && !simulation.B10Pressed(s) && !simulation.B10Pressed(s-1) {
			back = s
			break
		}
	}
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, zoneA.ID, simulation.Packet(back, time.Now().UTC())); e != nil {
		t.Fatal(e)
	}
	if evs, _ := f.repo.ListEvents(ctx, a, b10, 50); countType(evs, domain.EventOnline) != 1 {
		t.Fatalf("recovery through a sibling gateway must emit one online event: %+v", evs)
	}
	// A revoked gateway that heard it last must not hide the device from the offline scan.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET last_seen=now()-interval '3 minutes' WHERE external_id=$1 AND gateway_id=$2`, b10, zoneA.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET last_seen=now()-interval '2 minutes' WHERE external_id=$1 AND gateway_id=$2`, b10, zoneB.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.gateways SET revoked_at=now() WHERE id=$1`, zoneB.ID); e != nil {
		t.Fatal(e)
	}
	worker.Tick(ctx, time.Now().UTC())
	if evs, _ := f.repo.ListEvents(ctx, a, b10, 50); countType(evs, domain.EventOffline) != 2 {
		t.Fatalf("offline must be raised on the surviving gateway when the fresher one is revoked: %+v", evs)
	}
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.gateways SET revoked_at=NULL WHERE id=$1`, zoneB.ID); e != nil {
		t.Fatal(e)
	}
	// Turning roaming off restores per-gateway behaviour.
	code, _, _ = req(t, api, "POST", "/api/v1/devices/"+d.ID+"/roaming", "Bearer "+authA.AccessToken, "", "", map[string]any{"roaming": false})
	if code != 204 {
		t.Fatalf("roaming off: %d", code)
	}
	worker.Tick(ctx, time.Now().UTC())
	if evs, _ := f.repo.ListEvents(ctx, a, b10, 50); countType(evs, domain.EventOffline) != 3 {
		t.Fatalf("without roaming each silent stream is its own episode: %+v", evs)
	}
}

func countType(events []domain.DeviceEvent, kind string) int {
	n := 0
	for _, ev := range events {
		if ev.EventType == kind {
			n++
		}
	}
	return n
}
