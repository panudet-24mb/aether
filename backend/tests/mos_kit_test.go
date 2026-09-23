package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"strings"
	"testing"
	"time"
)

// hhmm renders a time in Asia/Bangkok as the "HH:MM" an after-hours window uses.
func hhmm(t time.Time) string { return t.In(time.FixedZone("ICT", 7*3600)).Format("15:04") }

// The MOS smart-office kit through an MG4: ISO-8601 row timestamps, PIR occupancy with a vacancy decided
// by the worker's clock, and an S4 door sensor whose frame Aether cannot decode until an operator
// teaches it what "open" looks like.
func TestMOSKitOccupancyAndLearnedDoor(t *testing.T) {
	f := setup(t)
	_, auth, a := f.account(t)
	_, otherAuth, other := f.account(t)
	ctx := context.Background()
	api := httpapi.New(f.cfg, f.service, f.repo)

	// The catalog carries the new gateway model and profiles.
	code, out, _ := req(t, api, "GET", "/api/v1/catalog", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("catalog: %d", code)
	}
	foundMG4 := false
	for _, g := range out["gateway_models"].([]any) {
		m := g.(map[string]any)
		if m["id"] == "minew-mg4" {
			foundMG4 = m["transport"] == "mqtt" && m["image"] == "/devices/minew-mg4.png" && m["verified"] == false
		}
	}
	profiles := map[string]map[string]any{}
	for _, p := range out["device_profiles"].([]any) {
		m := p.(map[string]any)
		profiles[m["id"].(string)] = m
	}
	if !foundMG4 || profiles["minew-msp01-pending@1"]["occupancy"] != true || profiles["minew-s4-pending@1"]["door"] != true ||
		profiles["minew-s4-pending@1"]["verified"] != false || profiles["minew-msp01-pending@1"]["info_name"] != "MSP01" {
		t.Fatalf("catalog is missing the MOS kit: %v %v", foundMG4, profiles)
	}

	gateway, _, e := f.service.CreateGateway(ctx, a, "Office MG4", "minew-mg4")
	if e != nil || gateway.Model != "minew-mg4" {
		t.Fatalf("MG4 gateway: %v %+v", e, gateway)
	}
	if _, e := f.service.CreateDevice(ctx, a, gateway.ID, "PIR ห้องประชุม", simulation.MOSMSP01, "minew-msp01-pending@1"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.service.CreateDevice(ctx, a, gateway.ID, "ประตูหน้า", simulation.MOSS4, "minew-s4-pending@1"); e != nil {
		t.Fatal(e)
	}
	capture := func(step int) {
		t.Helper()
		if _, e := f.repo.CapturePacket(ctx, a.TenantID, gateway.ID, simulation.MOSPacket(step, time.Now().UTC())); e != nil {
			t.Fatal(e)
		}
	}
	events := func(mac, kind string) int {
		t.Helper()
		evs, e := f.repo.ListEvents(ctx, a, mac, 200)
		if e != nil {
			t.Fatal(e)
		}
		return countType(evs, kind)
	}
	alertsFor := func(ruleID string) int {
		t.Helper()
		list, e := f.repo.ListAlerts(ctx, a, "", 200)
		if e != nil {
			t.Fatal(e)
		}
		n := 0
		for _, al := range list {
			if al.RuleID != nil && *al.RuleID == ruleID {
				n++
			}
		}
		return n
	}
	rule := func(body map[string]any) string {
		t.Helper()
		code, out, _ := req(t, api, "POST", "/api/v1/rules", "Bearer "+auth.AccessToken, "", "", body)
		if code != 201 {
			t.Fatalf("rule %v: %d %v", body["name"], code, out)
		}
		return out["id"].(string)
	}
	now := time.Now()
	insideID := rule(map[string]any{"name": "มีคนตอนกลางคืน", "event_type": "occupancy", "severity": "warning",
		"scope": map[string]any{"after_hours": map[string]any{"from": hhmm(now.Add(-time.Hour)), "to": hhmm(now.Add(time.Hour))}}})
	outsideID := rule(map[string]any{"name": "มีคนนอกช่วงนี้", "event_type": "occupancy", "severity": "warning",
		"scope": map[string]any{"after_hours": map[string]any{"from": hhmm(now.Add(2 * time.Hour)), "to": hhmm(now.Add(3 * time.Hour))}}})
	doorID := rule(map[string]any{"name": "ประตูเปิด", "event_type": "door", "severity": "info", "dedupe_sec": 0})
	// after_hours belongs to door and occupancy only.
	if code, _, _ := req(t, api, "POST", "/api/v1/rules", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"name": "bad", "event_type": "tamper", "severity": "warning", "scope": map[string]any{"after_hours": map[string]any{"from": "18:00", "to": "07:00"}}}); code != 400 {
		t.Fatalf("after_hours on a tamper rule: %d", code)
	}

	// ---- MG4 uplinks with ISO-8601 timestamps are stored and decoded --------------------------------
	before := time.Now().UTC().Add(-time.Second)
	capture(5) // nobody moving, door closed, S4 info frame present
	sensors, e := f.repo.StreamHistory(ctx, a, gateway.ID, time.Now().Add(-time.Hour), 50)
	if e != nil {
		t.Fatal(e)
	}
	byID := map[string]minew.Sensor{}
	for _, s := range sensors {
		byID[s.ID] = s
	}
	if s1 := byID[simulation.MOSS1]; s1.Latest.Kind != minew.KindEnvironment || s1.Latest.Temperature < 20 || s1.Latest.ReceivedAt.Before(before) {
		t.Fatalf("S1 through MG4 (server time, never the device clock): %+v", s1.Latest)
	}
	if pir := byID[simulation.MOSMSP01]; pir.Latest.Metrics["motion"] != 0 || !strings.Contains(strings.Join(pir.Latest.Frames, ","), minew.FramePIR) {
		t.Fatalf("MSP01 PIR frame: %+v", pir.Latest)
	}
	s4 := byID[simulation.MOSS4]
	if len(s4.Latest.Unknown) == 0 || s4.Latest.Unknown[0] != "ffe1:a1:0x23:len=15" {
		t.Fatalf("the undecodable S4 frame must surface in unknown: %+v", s4.Latest)
	}
	if _, has := s4.Latest.Metrics["door"]; has {
		t.Fatalf("no door state before a signal is taught: %+v", s4.Latest)
	}
	var stored int
	if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.ble_history WHERE tenant_id=$1 AND gateway_id=$2`, a.TenantID, gateway.ID).Scan(&stored); e != nil || stored < 10 {
		t.Fatalf("raw advertisements archived: %d %v", stored, e)
	}

	// ---- occupancy: occupied on motion, vacant only from the periodic scan, once ---------------------
	capture(40) // 40%20==0: someone walks past
	if n := events(simulation.MOSMSP01, domain.EventOccupied); n != 1 {
		t.Fatalf("motion must raise exactly one occupied event, got %d", n)
	}
	if alertsFor(insideID) != 1 || alertsFor(outsideID) != 0 {
		t.Fatalf("occupancy rules: inside window %d, outside window %d", alertsFor(insideID), alertsFor(outsideID))
	}
	capture(41) // still moving: same episode
	capture(45) // quiet uplink: never a vacant on its own
	if events(simulation.MOSMSP01, domain.EventOccupied) != 1 || events(simulation.MOSMSP01, domain.EventVacant) != 0 {
		t.Fatal("one episode, and no vacant from an uplink")
	}
	if n, e := f.repo.ScanVacancy(ctx, a.TenantID, time.Now().UTC()); e != nil || n != 0 {
		t.Fatalf("inside the hold time the room is occupied: %d %v", n, e)
	}
	// Age the stored last motion (server time) past the hold; the device clock plays no part.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.stream_state SET last_motion_at=last_motion_at-make_interval(secs => $3) WHERE tenant_id=$1 AND external_id=$2`,
		a.TenantID, simulation.MOSMSP01, alerts.OccupancyHoldSec+60); e != nil {
		t.Fatal(e)
	}
	if n, e := f.repo.ScanVacancy(ctx, other.TenantID, time.Now().UTC()); e != nil || n != 0 {
		t.Fatalf("another workspace's scan must not see this episode: %d %v", n, e)
	}
	worker := &alerts.Worker{Store: f.repo, Sender: httpapi.NewSender(f.cfg), Secrets: f.service.Secrets, Batch: 10}
	worker.Tick(ctx, time.Now().UTC())
	worker.Tick(ctx, time.Now().UTC())
	if n, e := f.repo.ScanVacancy(ctx, a.TenantID, time.Now().UTC()); e != nil || n != 0 {
		t.Fatalf("an episode is vacated once: %d %v", n, e)
	}
	if n := events(simulation.MOSMSP01, domain.EventVacant); n != 1 {
		t.Fatalf("exactly one vacant per episode, got %d", n)
	}
	// A new walk-past after vacancy is a new episode.
	capture(60)
	if n := events(simulation.MOSMSP01, domain.EventOccupied); n != 2 {
		t.Fatalf("a new episode after vacancy, got %d occupied", n)
	}

	// ---- S4: teach "door" (closed at rest, then open), then open/close drive the door state --------
	code, out, _ = req(t, api, "POST", "/api/v1/signals/sessions", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"gateway_id": gateway.ID, "external_id": simulation.MOSS4, "event_type": "door", "label": "ประตูหน้า", "baseline_sec": 120, "trigger_sec": 120})
	if code != 201 {
		t.Fatalf("start door session: %d %v", code, out)
	}
	session, _ := out["id"].(string)
	for step := 66; step <= 70; step++ { // 66..70 %30 = 6..10: closed, no open/close in between
		capture(step)
	}
	code, out, _ = req(t, api, "POST", "/api/v1/signals/sessions/"+session+"/advance", "Bearer "+auth.AccessToken, "", "", map[string]any{})
	if code != 200 || out["status"] != "trigger" {
		t.Fatalf("advance: %d %v", code, out)
	}
	for step := 80; step <= 83; step++ { // 80..83 %30 = 20..23: the door is open
		capture(step)
	}
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.signal_sessions SET trigger_until=now() WHERE id=$1`, session); e != nil {
		t.Fatal(e)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/signals/sessions/"+session, "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 || out["status"] != "finished" {
		t.Fatalf("finish: %d %v", code, out)
	}
	index := -1
	for i, c := range out["candidates"].([]any) {
		m := c.(map[string]any)["matcher"].(map[string]any)
		if m["kind"] == "byte" && m["frame"] == "ffe1:a1:0x23" && m["offset"] == 3.0 && m["mask"] == 255.0 && m["value"] == 1.0 {
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("the door status byte must be a candidate: %v", out["candidates"])
	}
	code, out, _ = req(t, api, "POST", "/api/v1/signals/sessions/"+session+"/confirm", "Bearer "+auth.AccessToken, "", "", map[string]any{"candidate_index": index, "scope": "device"})
	if code != 201 || out["event_type"] != "door" {
		t.Fatalf("confirm: %d %v", code, out)
	}
	signalID := out["id"].(string)
	if n := events(simulation.MOSS4, domain.EventDoorOpen); n != 0 {
		t.Fatalf("teaching itself raises nothing, got %d door_open", n)
	}

	capture(84) // closed: first learned state, no event
	if events(simulation.MOSS4, domain.EventDoorOpen)+events(simulation.MOSS4, domain.EventDoorClosed) != 0 {
		t.Fatal("a closed door at first sight is not an event")
	}
	capture(110) // 110%30 = 20: opened
	if n := events(simulation.MOSS4, domain.EventDoorOpen); n != 1 {
		t.Fatalf("opening must raise one door_open, got %d", n)
	}
	sensors, e = f.repo.StreamHistory(ctx, a, gateway.ID, time.Now().Add(-time.Hour), 50)
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range sensors {
		if s.ID == simulation.MOSS4 && (s.Latest.Metrics["door"] != 1 || s.Latest.Kind != minew.KindDoor) {
			t.Fatalf("the stored reading carries the open state: %+v", s.Latest)
		}
	}
	if alertsFor(doorID) != 1 {
		t.Fatalf("the door rule fires on opening, got %d", alertsFor(doorID))
	}
	capture(111) // still open
	if n := events(simulation.MOSS4, domain.EventDoorOpen); n != 1 {
		t.Fatalf("held open must not repeat, got %d", n)
	}
	capture(115) // 115%30 = 25: closed again
	if n := events(simulation.MOSS4, domain.EventDoorClosed); n != 1 {
		t.Fatalf("closing must raise one door_closed, got %d", n)
	}
	if alertsFor(doorID) != 1 {
		t.Fatal("closing is not an alert")
	}
	evs, e := f.repo.ListEvents(ctx, a, simulation.MOSS4, 50)
	if e != nil {
		t.Fatal(e)
	}
	for _, ev := range evs {
		if (ev.EventType == domain.EventDoorOpen || ev.EventType == domain.EventDoorClosed) && (ev.Detail["learned"] != true || ev.Detail["signal_id"] != signalID) {
			t.Fatalf("a learned door event names its signature: %+v", ev)
		}
	}

	// ---- tenant isolation ----------------------------------------------------------------------------
	for _, mac := range []string{simulation.MOSS4, simulation.MOSMSP01} {
		if evs, e := f.repo.ListEvents(ctx, other, mac, 50); e != nil || len(evs) != 0 {
			t.Fatalf("events leaked across tenants: %d %v", len(evs), e)
		}
	}
	if list, e := f.repo.ListAlerts(ctx, other, "", 50); e != nil || len(list) != 0 {
		t.Fatalf("alerts leaked across tenants: %d %v", len(list), e)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/signals", "Bearer "+otherAuth.AccessToken, "", "", nil)
	if code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("the door signal leaked across tenants: %d %v", code, out)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/rules", "Bearer "+otherAuth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("rules: %d", code)
	}
	for _, r := range out["items"].([]any) {
		if et := r.(map[string]any)["event_type"]; et == "door" || et == "occupancy" {
			t.Fatalf("a door/occupancy rule leaked across tenants: %v", r)
		}
	}
	if sensors, e := f.repo.StreamHistory(ctx, other, gateway.ID, time.Now().Add(-time.Hour), 50); e != nil || len(sensors) != 0 {
		t.Fatalf("stream history leaked across tenants: %d %v", len(sensors), e)
	}
}
