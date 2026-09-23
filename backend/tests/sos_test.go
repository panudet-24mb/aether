package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// The simulated B10 (f00000000007) advertises an Eddystone-UID whose instance switches while the button is
// held (simulation.B10Pressed). Steps 0/15/60 are pressed, 2/17/62 are not, and the tag is in range of the
// zone A gateway at all of them; none of them raises the MBT01 tamper flag, so the only alerts in this test
// belong to the button. The real B10 does not report presses yet — this is the fixture that stands in for it.
const (
	sosMAC     = "f00000000007"
	sosProfile = "minew-b10-pending@1"
)

// clearBuiltinRules removes the rules a workspace is born with (migration 00024 / seedDefaultRules), so a
// test that counts rules or alerts sees only the ones it created itself.
func clearBuiltinRules(t *testing.T, f *fixture, p domain.Principal) {
	t.Helper()
	ids, e := f.repo.BuiltinRuleIDs(context.Background(), p)
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range ids {
		if e := f.repo.DeleteRule(context.Background(), p, id); e != nil {
			t.Fatal(e)
		}
	}
}

func sosRule(t *testing.T, f *fixture, p domain.Principal) domain.AlertRule {
	t.Helper()
	rules, e := f.repo.ListRules(context.Background(), p)
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range rules {
		if r.EventType == domain.EventButton {
			return r
		}
	}
	t.Fatalf("a brand-new workspace has no button rule: %+v", rules)
	return domain.AlertRule{}
}

// A workspace that has configured nothing at all must still shout when somebody presses the emergency button.
func TestSOSNeedsNoConfiguration(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, auth, p := f.account(t)
	api := httpapi.New(f.cfg, f.service, f.repo)

	// The two rules the workspace was born with, and nothing else.
	rules, e := f.repo.ListRules(ctx, p)
	if e != nil || len(rules) != 2 {
		t.Fatalf("seeded rules: %+v %v", rules, e)
	}
	button := sosRule(t, f, p)
	if !button.Enabled || button.Severity != "critical" || button.DedupeSec != 30 {
		t.Fatalf("the built-in button rule must be enabled, critical and deduped: %+v", button)
	}
	code, out, _ := req(t, api, "GET", "/api/v1/rules", "Bearer "+auth.AccessToken, "", "", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 2 {
		t.Fatalf("rules: %d %v", code, out)
	}
	for _, raw := range items {
		if row, _ := raw.(map[string]any); row["builtin"] != true {
			t.Fatalf("a seeded rule must be labelled builtin: %v", raw)
		}
	}

	g, _, e := f.service.CreateGateway(ctx, p, "Ward gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.service.CreateDevice(ctx, p, g.ID, "ปุ่มฉุกเฉินเตียง 3", sosMAC, sosProfile); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	press := func(step int, offset time.Duration) {
		t.Helper()
		if _, e := f.repo.CapturePacket(ctx, p.TenantID, g.ID, simulation.Packet(step, now.Add(offset))); e != nil {
			t.Fatalf("uplink step %d: %v", step, e)
		}
	}
	sosAlerts := func() []domain.Alert {
		t.Helper()
		all, e := f.repo.ListAlerts(ctx, p, "", 100)
		if e != nil {
			t.Fatal(e)
		}
		out := []domain.Alert{}
		for _, a := range all {
			if a.ExternalID == sosMAC {
				out = append(out, a)
			}
		}
		return out
	}

	press(0, 0)             // first sighting: the instance is remembered, nothing happened yet
	press(2, 5*time.Second) // released → instance change → button event
	opened := sosAlerts()
	if len(opened) != 1 {
		t.Fatalf("one press with no configured rule must open exactly one alert: %+v", opened)
	}
	alert := opened[0]
	if alert.Severity != "critical" || alert.EventType != domain.EventButton || alert.Status != "open" || !alert.SOS() {
		t.Fatalf("the alert must be a critical SOS: %+v", alert)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/alerts?status=open&limit=50", "Bearer "+auth.AccessToken, "", "", nil)
	items, _ = out["items"].([]any)
	row, _ := items[0].(map[string]any)
	if code != 200 || len(items) != 1 || row["sos"] != true || row["severity"] != "critical" {
		t.Fatalf("the API must flag the alert as SOS: %d %v", code, out)
	}

	// A second press while the first alert is still open must not stack a second alert on the operator.
	press(15, 10*time.Second)
	if again := sosAlerts(); len(again) != 1 {
		t.Fatalf("a press during an open SOS must not open another alert: %+v", again)
	}

	// Acknowledging is one call, and it is what stops the alert being "open".
	code, _, _ = req(t, api, "POST", "/api/v1/alerts/"+alert.ID+"/ack", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 204 {
		t.Fatalf("ack: %d", code)
	}
	if acked := sosAlerts(); len(acked) != 1 || acked[0].Status != "acknowledged" || acked[0].AckedBy == nil {
		t.Fatalf("after ack: %+v", acked)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/alerts/"+alert.ID+"/resolve", "Bearer "+auth.AccessToken, "", "", map[string]any{"note": "ไปถึงเตียงแล้ว"})
	if code != 204 {
		t.Fatalf("resolve: %d", code)
	}

	// Resolved, but still inside the 30 s dedupe window: the same button must not reopen immediately.
	press(17, 20*time.Second)
	if quiet := sosAlerts(); len(quiet) != 1 {
		t.Fatalf("a press inside the dedupe window must not open a second alert: %+v", quiet)
	}
	// Well past the window it does, because the rule is still doing its job. Ingest always stamps server time
	// (device clocks are never trusted), so the only honest way to leave the window is to age the stored alert.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.alerts SET opened_at=opened_at-interval '10 minutes', resolved_at=resolved_at-interval '10 minutes' WHERE tenant_id=$1`, p.TenantID); e != nil {
		t.Fatal(e)
	}
	press(59, 0)
	press(60, 0)
	later := sosAlerts()
	if len(later) != 2 {
		t.Fatalf("a press after the dedupe window must open a new alert: %+v", later)
	}
	if !later[0].SOS() || later[0].Status != "open" {
		t.Fatalf("the new alert must be an open SOS: %+v", later[0])
	}
	code, _, _ = req(t, api, "POST", "/api/v1/alerts/"+later[0].ID+"/resolve", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 204 {
		t.Fatalf("resolve second: %d", code)
	}

	// The workspace owns the built-in rule: disabling it really does silence the button.
	code, out, _ = req(t, api, "POST", "/api/v1/rules/"+button.ID+"/update", "Bearer "+auth.AccessToken, "", "",
		map[string]any{"name": button.Name, "event_type": button.EventType, "severity": button.Severity, "enabled": false, "dedupe_sec": button.DedupeSec, "channels": []string{}})
	if code != 200 || out["enabled"] != false {
		t.Fatalf("disable the built-in rule: %d %v", code, out)
	}
	press(62, 20*time.Minute)
	if silent := sosAlerts(); len(silent) != 2 {
		t.Fatalf("a disabled rule must open no alert: %+v", silent)
	}
	// The press is still recorded: five instance changes, one per press/release above.
	events, e := f.repo.ListEvents(ctx, p, sosMAC, 50)
	if e != nil || len(events) != 5 || events[0].EventType != domain.EventButton {
		t.Fatalf("button events: %+v %v", events, e)
	}
}

// Workspaces that existed before migration 00024 are given the same rules by its backfill.
func TestBuiltinRulesBackfilledForExistingWorkspace(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, _, p := f.account(t)
	// Make this workspace look like one from before the migration: no rules of any kind.
	rules, e := f.repo.ListRules(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range rules {
		if e := f.repo.DeleteRule(ctx, p, r.ID); e != nil {
			t.Fatal(e)
		}
	}
	// Replay 00024 against that state: this is exactly what the owner's database runs.
	if e := goose.DownTo(f.admin, "../migrations", 23); e != nil {
		t.Fatalf("down to 00023: %v", e)
	}
	if e := goose.Up(f.admin, "../migrations"); e != nil {
		t.Fatalf("up again: %v", e)
	}

	after, e := f.repo.ListRules(ctx, p)
	if e != nil || len(after) != 2 {
		t.Fatalf("backfilled rules: %+v %v", after, e)
	}
	seeded, e := f.repo.BuiltinRuleIDs(ctx, p)
	if e != nil || len(seeded) != 2 {
		t.Fatalf("builtin ids: %+v %v", seeded, e)
	}
	byType := map[string]domain.AlertRule{}
	for _, r := range after {
		byType[r.EventType] = r
	}
	if b := byType[domain.EventButton]; !b.Enabled || b.Severity != "critical" || b.DedupeSec != 30 {
		t.Fatalf("backfilled button rule: %+v", b)
	}
	if tamper := byType[domain.EventTamper]; !tamper.Enabled || tamper.Severity != "warning" {
		t.Fatalf("backfilled tamper rule: %+v", tamper)
	}
}
