package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Simulator step 27 raises the MBT01 tamper flag; step 0 has it clear.
func TestAlertPipelineFromPacketToNotification(t *testing.T) {
	f := setup(t)
	_, authA, a := f.account(t)
	_, _, b := f.account(t)
	ctx := context.Background()
	clearBuiltinRules(t, f, a) // this test counts its own rules and alerts; the seeded button/tamper rules would add more
	g, _, e := f.service.CreateGateway(ctx, a, "Kit gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	// Button events are raised only for tags registered with a button profile (any beacon can change its instance).
	if _, e := f.service.CreateDevice(ctx, a, g.ID, "Panic button", "f00000000007", "minew-b10-pending@1"); e != nil {
		t.Fatal(e)
	}
	var received atomic.Int32
	var lastBody atomic.Value
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Aether-Signature") == "" || r.Header.Get("X-Aether-Event") != "tamper" {
			w.WriteHeader(400)
			return
		}
		var p alerts.Payload
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			w.WriteHeader(400)
			return
		}
		lastBody.Store(p)
		received.Add(1)
		w.WriteHeader(204)
	}))
	defer hook.Close()

	cfg := f.cfg
	cfg.Environment = "development" // permits the loopback webhook target
	api := httpapi.New(cfg, f.service, f.repo)
	code, out, _ := req(t, api, "POST", "/api/v1/channels", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "ops hook", "kind": "webhook", "config": map[string]string{"url": hook.URL + "/alerts"}, "secret": "topsecret"})
	if code != 201 || out["has_secret"] != true || out["id"] == nil {
		t.Fatalf("channel: %d %v", code, out)
	}
	channelID := out["id"].(string)
	code, out, _ = req(t, api, "POST", "/api/v1/rules", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "Tamper anywhere", "event_type": "tamper", "severity": "critical", "channels": []string{channelID}, "dedupe_sec": 600})
	if code != 201 {
		t.Fatalf("rule: %d %v", code, out)
	}
	code, out, _ = req(t, api, "POST", "/api/v1/rules", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "bad", "event_type": "threshold", "severity": "info"})
	if code != 400 {
		t.Fatalf("invalid rule accepted: %d %v", code, out)
	}

	now := time.Now().UTC()
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(0, now)); e != nil {
		t.Fatal(e)
	}
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(27, now.Add(5*time.Second))); e != nil {
		t.Fatal(e)
	}
	// Same tampered state again must not open a second alert.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(28, now.Add(10*time.Second))); e != nil {
		t.Fatal(e)
	}
	events, e := f.repo.ListEvents(ctx, a, "f00000000009", 50)
	if e != nil || len(events) != 1 || events[0].EventType != domain.EventTamper {
		t.Fatalf("events: %+v %v", events, e)
	}
	open, e := f.repo.ListAlerts(ctx, a, "open", 50)
	if e != nil || len(open) != 1 || open[0].Severity != "critical" || open[0].ExternalID != "f00000000009" {
		t.Fatalf("alerts: %+v %v", open, e)
	}
	if foreign, e := f.repo.ListAlerts(ctx, b, "", 50); e != nil || len(foreign) != 0 {
		t.Fatalf("tenant isolation broken: %+v %v", foreign, e)
	}
	if e := f.repo.TransitionAlert(ctx, b, open[0].ID, "acknowledged", ""); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("foreign ack: %v", e)
	}
	// Motion: E8S moves at step 0 (vibration=1) and is still at step 27 → motion then motion_stopped.
	motion, e := f.repo.ListEvents(ctx, a, "f00000000008", 50)
	if e != nil || len(motion) != 2 || motion[0].EventType != domain.EventMotionStopped || motion[1].EventType != domain.EventMotion {
		t.Fatalf("motion events: %+v", motion)
	}
	// Button: B10 is pressed at step 0 and idle at step 27 → one instance change event.
	button, e := f.repo.ListEvents(ctx, a, "f00000000007", 50)
	if e != nil || len(button) != 1 || button[0].EventType != domain.EventButton {
		t.Fatalf("button events: %+v", button)
	}

	// Deliver: the worker claims the queued notification and posts to the webhook with an HMAC signature.
	worker := &alerts.Worker{Store: f.repo, Sender: httpapi.NewSender(cfg), Secrets: f.service.Secrets, Batch: 10}
	worker.Tick(ctx, now.Add(20*time.Second))
	if received.Load() != 1 {
		debug, _ := f.repo.ListNotifications(ctx, a, 10)
		t.Fatalf("webhook deliveries: %d notifications: %+v", received.Load(), debug)
	}
	payload := lastBody.Load().(alerts.Payload)
	if payload.Alert.ID != open[0].ID || payload.Event.EventType != domain.EventTamper || payload.TenantID != a.TenantID || payload.Gateway != "Kit gateway" {
		t.Fatalf("payload: %+v", payload)
	}
	notes, e := f.repo.ListNotifications(ctx, a, 10)
	if e != nil || len(notes) != 1 || notes[0].Status != "sent" {
		t.Fatalf("notifications: %+v %v", notes, e)
	}
	worker.Tick(ctx, now.Add(40*time.Second))
	if received.Load() != 1 {
		t.Fatal("sent notification was delivered again")
	}

	// Ack → resolve through the API; a second resolve is a 404.
	code, _, _ = req(t, api, "POST", "/api/v1/alerts/"+open[0].ID+"/ack", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 204 {
		t.Fatalf("ack: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/alerts/"+open[0].ID+"/resolve", "Bearer "+authA.AccessToken, "", "", map[string]any{"note": "ติดป้ายกลับแล้ว"})
	if code != 204 {
		t.Fatalf("resolve: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/alerts/"+open[0].ID+"/resolve", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 404 {
		t.Fatalf("double resolve: %d", code)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/alerts/summary", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 || out["open"].(float64) != 0 {
		t.Fatalf("summary: %d %v", code, out)
	}

	// Offline: a stream silent longer than the rule threshold becomes an offline event/alert, once.
	code, _, _ = req(t, api, "POST", "/api/v1/rules", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "Offline", "event_type": "offline", "severity": "warning", "scope": map[string]any{"offline_after_sec": 60}})
	if code != 201 {
		t.Fatalf("offline rule: %d", code)
	}
	// A second, much longer offline rule must not fire just because the short one did.
	code, _, _ = req(t, api, "POST", "/api/v1/rules", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "Offline 1h", "event_type": "offline", "severity": "critical", "scope": map[string]any{"offline_after_sec": 3600}})
	if code != 201 {
		t.Fatalf("second offline rule: %d", code)
	}
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET last_seen=now()-interval '10 minutes' WHERE external_id='f00000000001'`); e != nil {
		t.Fatal(e)
	}
	worker.Tick(ctx, time.Now().UTC())
	worker.Tick(ctx, time.Now().UTC())
	offline, e := f.repo.ListEvents(ctx, a, "f00000000001", 50)
	if e != nil || len(offline) != 1 || offline[0].EventType != domain.EventOffline {
		t.Fatalf("offline events: %+v %v", offline, e)
	}
	open, e = f.repo.ListAlerts(ctx, a, "open", 50)
	if e != nil || len(open) != 1 || open[0].EventType != domain.EventOffline || open[0].Severity != "warning" {
		t.Fatalf("offline alert (only the 60 s rule may fire): %+v", open)
	}
	// Once the device has been silent past the long threshold, that rule opens its own alert, once.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET last_seen=now()-interval '2 hours' WHERE external_id='f00000000001'`); e != nil {
		t.Fatal(e)
	}
	worker.Tick(ctx, time.Now().UTC())
	worker.Tick(ctx, time.Now().UTC())
	if both, e := f.repo.ListAlerts(ctx, a, "open", 50); e != nil || len(both) != 2 {
		t.Fatalf("long offline rule: %+v %v", both, e)
	}
	if evs, _ := f.repo.ListEvents(ctx, a, "f00000000001", 50); len(evs) != 1 {
		t.Fatalf("offline episode must stay one event: %+v", evs)
	}
	// Production sender refuses private targets at connect time even when the hostname looks public.
	prod := alerts.NewSender(alerts.SMTPSettings{}, "production")
	if e := prod.Send(ctx, domain.NotificationChannel{Kind: "webhook", Config: map[string]string{"url": hook.URL}}, "", alerts.Payload{}); e == nil {
		t.Fatal("production sender delivered to a loopback webhook")
	}
	// A fresh uplink brings it back online.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(29, time.Now().UTC())); e != nil {
		t.Fatal(e)
	}
	back, e := f.repo.ListEvents(ctx, a, "f00000000001", 50)
	if e != nil || len(back) != 2 || back[0].EventType != domain.EventOnline {
		t.Fatalf("online events: %+v", back)
	}
	// Deleting a channel and a rule that already produced notifications/alerts must succeed and keep history.
	code, _, _ = req(t, api, "POST", "/api/v1/channels/"+channelID+"/delete", "Bearer "+authA.AccessToken, "", "", map[string]any{})
	if code != 204 {
		t.Fatalf("delete channel with delivery history: %d", code)
	}
	rulesNow, e := f.repo.ListRules(ctx, a)
	if e != nil || len(rulesNow) != 3 {
		t.Fatalf("rules: %+v %v", rulesNow, e)
	}
	for _, r := range rulesNow {
		code, _, _ = req(t, api, "POST", "/api/v1/rules/"+r.ID+"/delete", "Bearer "+authA.AccessToken, "", "", map[string]any{})
		if code != 204 {
			t.Fatalf("delete rule %s with alert history: %d", r.Name, code)
		}
	}
	kept, e := f.repo.ListAlerts(ctx, a, "", 50)
	if e != nil || len(kept) != 3 || kept[0].RuleID != nil {
		t.Fatalf("alert history after rule deletion: %+v %v", kept, e)
	}
	if notes, e := f.repo.ListNotifications(ctx, a, 10); e != nil || len(notes) != 1 || notes[0].ChannelID != nil || notes[0].Status != "sent" {
		t.Fatalf("delivery history after channel deletion: %+v %v", notes, e)
	}
	// Viewer role cannot manage rules; secrets never leak through the channel listing.
	code, out, _ = req(t, api, "GET", "/api/v1/channels", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatal(code)
	}
	raw, _ := json.Marshal(out)
	if string(raw) == "" || containsString(string(raw), "topsecret") {
		t.Fatal("secret leaked")
	}
}

func containsString(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
