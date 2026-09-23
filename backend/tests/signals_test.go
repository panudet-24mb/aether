package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"testing"
	"time"
)

// The realistic B10 of the simulation behaves like the physical tag in the owner's workspace: it
// never advertises Eddystone-UID, so the inferred button press can never fire for it, and its press
// is a Minew frame version no decoder knows. This proves the whole teaching loop closes that gap:
// record it at rest, record it while pressed, confirm the one candidate, and from then on a press
// raises a button event — once per press, not once per uplink.
const realB10 = "f0000000000a"

// nthStep returns the n-th step of the scenario in which the realistic B10 is (or is not) pressed.
func nthStep(t *testing.T, pressed bool, n int) int {
	t.Helper()
	seen := 0
	for step := 0; step < 400; step++ {
		if simulation.B10RealPressed(step) != pressed {
			continue
		}
		if seen == n {
			return step
		}
		seen++
	}
	t.Fatalf("scenario has fewer than %d steps with pressed=%v", n+1, pressed)
	return 0
}

func TestTeachSignalRaisesButtonEvent(t *testing.T) {
	f := setup(t)
	_, auth, a := f.account(t)
	_, otherAuth, other := f.account(t)
	ctx := context.Background()
	api := httpapi.New(f.cfg, f.service, f.repo)

	gateway, _, e := f.service.CreateGateway(ctx, a, "Ward A", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	// Registered with the B10 profile, which is a *button* profile. The learned signal must fire
	// through the same path without relying on the Eddystone-UID inference that profile exists for.
	if _, e := f.service.CreateDevice(ctx, a, gateway.ID, "ปุ่มฉุกเฉินห้อง 1", realB10, "minew-b10-pending@1"); e != nil {
		t.Fatal(e)
	}
	capture := func(step int) {
		t.Helper()
		if _, e := f.repo.CapturePacket(ctx, a.TenantID, gateway.ID, simulation.Packet(step, time.Now().UTC())); e != nil {
			t.Fatal(e)
		}
	}
	buttons := func() int {
		t.Helper()
		evs, e := f.repo.ListEvents(ctx, a, realB10, 100)
		if e != nil {
			t.Fatal(e)
		}
		return countType(evs, domain.EventButton)
	}

	// The gateway must have heard the tag before it can be taught.
	capture(nthStep(t, false, 0))
	capture(nthStep(t, false, 1))

	// Pressing it changes nothing today: that is the bug this feature exists for.
	capture(nthStep(t, true, 0))
	if n := buttons(); n != 0 {
		t.Fatalf("this tag cannot raise a press through the Eddystone-UID inference, got %d events", n)
	}

	// Phase 1 "ปล่อยนิ่ง". Long windows so nothing expires while the test runs; the clock is pushed
	// forward explicitly below instead of being waited out.
	code, out, _ := req(t, api, "POST", "/api/v1/signals/sessions", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"gateway_id": gateway.ID, "external_id": realB10, "event_type": "button", "label": "ปุ่มฉุกเฉิน", "baseline_sec": 120, "trigger_sec": 120})
	if code != 201 {
		t.Fatalf("start session: %d %v", code, out)
	}
	session, _ := out["id"].(string)
	if session == "" || out["status"] != "baseline" {
		t.Fatalf("session: %v", out)
	}
	for i := 2; i < 6; i++ {
		capture(nthStep(t, false, i))
	}

	// A session of another workspace is invisible, whatever its id.
	if code, _, _ := req(t, api, "GET", "/api/v1/signals/sessions/"+session, "Bearer "+otherAuth.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("a session leaked across tenants: %d", code)
	}

	// Phase 2 "กดปุ่มย้ำ ๆ": the operator is ready early and advances by hand.
	code, out, _ = req(t, api, "POST", "/api/v1/signals/sessions/"+session+"/advance", "Bearer "+auth.AccessToken, "", "", map[string]any{})
	if code != 200 || out["status"] != "trigger" {
		t.Fatalf("advance: %d %v", code, out)
	}
	baseline, _ := out["baseline"].(map[string]any)
	if baseline == nil || baseline["observations"].(float64) == 0 {
		t.Fatalf("the baseline phase must report what it heard: %v", out["baseline"])
	}
	for i := 1; i < 4; i++ {
		capture(nthStep(t, true, i))
	}
	// Close the trigger window without sleeping through it.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.signal_sessions SET trigger_until=now() WHERE id=$1`, session); e != nil {
		t.Fatal(e)
	}

	code, out, _ = req(t, api, "GET", "/api/v1/signals/sessions/"+session, "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 || out["status"] != "finished" {
		t.Fatalf("finish: %d %v", code, out)
	}
	candidates, _ := out["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("the press adds exactly one advertisement, so there is exactly one candidate: %v", out["candidates"])
	}
	first, _ := candidates[0].(map[string]any)
	if first["description"] == "" || first["trigger_hits"].(float64) < 1 {
		t.Fatalf("a candidate must explain itself and carry its evidence: %v", first)
	}
	if matcher, _ := first["matcher"].(map[string]any); matcher["kind"] != "frame" || matcher["frame"] != "ffe1:a1:0x22" {
		t.Fatalf("the press frame is the candidate: %v", first["matcher"])
	}

	// Confirm it for this device.
	code, out, _ = req(t, api, "POST", "/api/v1/signals/sessions/"+session+"/confirm", "Bearer "+auth.AccessToken, "", "", map[string]any{"candidate_index": 0, "scope": "device"})
	if code != 201 {
		t.Fatalf("confirm: %d %v", code, out)
	}
	signalID, _ := out["id"].(string)
	if signalID == "" || out["event_type"] != "button" || out["external_id"] != realB10 {
		t.Fatalf("stored signal: %v", out)
	}

	// From here on a press raises a button event.
	capture(nthStep(t, true, 4))
	if n := buttons(); n != 1 {
		t.Fatalf("a learned press must raise exactly one button event, got %d", n)
	}
	// Held down: the pattern keeps arriving, the event does not repeat.
	capture(nthStep(t, true, 5))
	capture(nthStep(t, true, 6))
	if n := buttons(); n != 1 {
		t.Fatalf("a held button must not repeat the event, got %d", n)
	}
	// Released, then pressed again: a new edge, a new event.
	capture(nthStep(t, false, 6))
	if n := buttons(); n != 1 {
		t.Fatalf("releasing the button raises nothing (button has no cleared event), got %d", n)
	}
	capture(nthStep(t, true, 7))
	if n := buttons(); n != 2 {
		t.Fatalf("a second press is a second event, got %d", n)
	}
	evs, e := f.repo.ListEvents(ctx, a, realB10, 100)
	if e != nil {
		t.Fatal(e)
	}
	for _, ev := range evs {
		if ev.EventType != domain.EventButton {
			continue
		}
		if ev.Detail["learned"] != true || ev.Detail["signal_id"] != signalID {
			t.Fatalf("a learned event must name the signature that raised it: %v", ev.Detail)
		}
	}

	// Replaying recent history is how the owner checks for false positives before trusting it.
	code, out, _ = req(t, api, "POST", "/api/v1/signals/"+signalID+"/test", "Bearer "+auth.AccessToken, "", "", map[string]any{})
	if code != 200 {
		t.Fatalf("test: %d %v", code, out)
	}
	if out["uplinks"].(float64) < out["matches"].(float64) || out["matches"].(float64) == 0 || out["verdict"] == "" {
		t.Fatalf("replay must find the presses it has just seen and explain itself: %v", out)
	}

	// The signature belongs to this workspace only.
	code, out, _ = req(t, api, "GET", "/api/v1/signals", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("own signals: %d %v", code, out)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/signals", "Bearer "+otherAuth.AccessToken, "", "", nil)
	if code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("signals leaked across tenants: %d %v", code, out)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/signals/"+signalID+"/delete", "Bearer "+otherAuth.AccessToken, "", "", map[string]any{}); code != 404 {
		t.Fatalf("another tenant deleted the signature: %d", code)
	}
	if _, e := f.repo.TestSignal(ctx, other, signalID); e == nil {
		t.Fatal("another tenant replayed the signature")
	}

	// Deleting it stops the events without touching the history it already wrote.
	if code, _, _ := req(t, api, "POST", "/api/v1/signals/"+signalID+"/delete", "Bearer "+auth.AccessToken, "", "", map[string]any{}); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	capture(nthStep(t, false, 7))
	capture(nthStep(t, true, 8))
	if n := buttons(); n != 2 {
		t.Fatalf("a deleted signature raises nothing, got %d", n)
	}
}

// A capture in which nothing changes must say so plainly rather than invent a signature, and a
// viewer must not be able to start teaching one at all.
func TestTeachSignalHonestEmptyResultAndViewerCannotTeach(t *testing.T) {
	f := setup(t)
	_, auth, a := f.account(t)
	ctx := context.Background()
	api := httpapi.New(f.cfg, f.service, f.repo)

	gateway, _, e := f.service.CreateGateway(ctx, a, "Ward B", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	capture := func(step int) {
		t.Helper()
		if _, e := f.repo.CapturePacket(ctx, a.TenantID, gateway.ID, simulation.Packet(step, time.Now().UTC())); e != nil {
			t.Fatal(e)
		}
	}
	capture(nthStep(t, false, 0))

	// Teaching a tag the gateway has never heard is refused: it could only produce an empty baseline.
	code, _, _ := req(t, api, "POST", "/api/v1/signals/sessions", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"gateway_id": gateway.ID, "external_id": "f0000000ffff", "event_type": "button", "label": "", "baseline_sec": 20, "trigger_sec": 20})
	if code != 404 {
		t.Fatalf("an unheard identity cannot be taught: %d", code)
	}
	// So is a phase length outside the accepted range.
	code, _, _ = req(t, api, "POST", "/api/v1/signals/sessions", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"gateway_id": gateway.ID, "external_id": realB10, "event_type": "button", "label": "", "baseline_sec": 1, "trigger_sec": 20})
	if code != 400 {
		t.Fatalf("a one-second baseline must be rejected: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/signals/sessions", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"gateway_id": gateway.ID, "external_id": realB10, "event_type": "explode", "label": "", "baseline_sec": 20, "trigger_sec": 20})
	if code != 400 {
		t.Fatalf("an unknown event type must be rejected: %d", code)
	}

	// A real session in which the operator never triggers the device: both phases see the same thing.
	code, out, _ := req(t, api, "POST", "/api/v1/signals/sessions", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"gateway_id": gateway.ID, "external_id": realB10, "event_type": "button", "label": "", "baseline_sec": 120, "trigger_sec": 120})
	if code != 201 {
		t.Fatalf("start: %d %v", code, out)
	}
	session, _ := out["id"].(string)
	capture(nthStep(t, false, 1))
	capture(nthStep(t, false, 2))
	if code, _, _ := req(t, api, "POST", "/api/v1/signals/sessions/"+session+"/advance", "Bearer "+auth.AccessToken, "", "", map[string]any{}); code != 200 {
		t.Fatalf("advance: %d", code)
	}
	capture(nthStep(t, false, 3))
	capture(nthStep(t, false, 4))
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.signal_sessions SET trigger_until=now() WHERE id=$1`, session); e != nil {
		t.Fatal(e)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/signals/sessions/"+session, "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 || out["status"] != "finished" {
		t.Fatalf("finish: %d %v", code, out)
	}
	if len(out["candidates"].([]any)) != 0 {
		t.Fatalf("nothing distinguished the phases, so there must be no candidate: %v", out["candidates"])
	}
	verdict, _ := out["verdict"].(string)
	if verdict == "" {
		t.Fatal("an empty result must explain what to check next, not stay silent")
	}
	// The trigger phase did hear the device; it simply heard nothing new.
	trigger, _ := out["trigger"].(map[string]any)
	if trigger["observations"].(float64) == 0 {
		t.Fatalf("the trigger phase heard packets in this scenario: %v", trigger)
	}
	// Confirming a candidate that does not exist is refused rather than stored as an empty matcher.
	if code, _, _ := req(t, api, "POST", "/api/v1/signals/sessions/"+session+"/confirm", "Bearer "+auth.AccessToken, "", "", map[string]any{"candidate_index": 0, "scope": "device"}); code != 400 {
		t.Fatalf("there is nothing to confirm: %d", code)
	}

	// A viewer may read the signals of the workspace but cannot teach, confirm or delete one.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2`, a.TenantID, a.UserID); e != nil {
		t.Fatal(e)
	}
	if code, _, _ := req(t, api, "GET", "/api/v1/signals", "Bearer "+auth.AccessToken, "", "", nil); code != 200 {
		t.Fatalf("a viewer must still see which signals are active: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/signals/sessions", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"gateway_id": gateway.ID, "external_id": realB10, "event_type": "button", "label": "", "baseline_sec": 20, "trigger_sec": 20})
	if code != 403 {
		t.Fatalf("a viewer started a teaching session: %d", code)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/signals/sessions/"+session+"/cancel", "Bearer "+auth.AccessToken, "", "", map[string]any{}); code != 403 {
		t.Fatalf("a viewer cancelled a session: %d", code)
	}
}
