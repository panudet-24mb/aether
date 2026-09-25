package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func block(id, kind string, x, y float64, data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	return map[string]any{"id": id, "type": kind, "position": map[string]any{"x": x, "y": y}, "data": data}
}

func link(id, source, target string, handle ...string) map[string]any {
	out := map[string]any{"id": id, "source": source, "target": target}
	if len(handle) == 1 {
		out["sourceHandle"] = handle[0]
	}
	return out
}

func flow(nodes []map[string]any, edges []map[string]any) map[string]any {
	return map[string]any{"nodes": nodes, "edges": edges}
}

func problemCodes(out map[string]any) map[string]bool {
	codes := map[string]bool{}
	list, _ := out["problems"].([]any)
	for _, raw := range list {
		if p, ok := raw.(map[string]any); ok {
			if code, ok := p["code"].(string); ok {
				codes[code] = true
			}
		}
	}
	return codes
}

// countTitles reports how many open alerts carry each title, so a test can prove a branch ran once.
func countTitles(t *testing.T, f *fixture, p domain.Principal) map[string]int {
	t.Helper()
	opened, e := f.repo.ListAlerts(context.Background(), p, "", 200)
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]int{}
	for _, a := range opened {
		out[a.Title]++
	}
	return out
}

// The whole studio path: a tamper flow opens a critical alert and queues its notification exactly once,
// a metric flow branches on another sensor's reading, a dry run changes nothing, and neither tenant can
// see the other's flows.
func TestAutomationStudio(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, authA, a := f.account(t)
	_, authB, b := f.account(t)
	clearBuiltinRules(t, f, a) // the seeded tamper rule would open its own alert beside the flow's
	cfg := f.cfg
	cfg.Environment = "development" // permits the loopback webhook target
	api := httpapi.New(cfg, f.service, f.repo)

	g, _, e := f.service.CreateGateway(ctx, a, "Kit gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	var received atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(204)
	}))
	defer hook.Close()
	code, out, _ := req(t, api, "POST", "/api/v1/channels", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "ops hook", "kind": "webhook", "config": map[string]string{"url": hook.URL + "/alerts"}, "secret": "topsecret"})
	if code != 201 {
		t.Fatalf("channel: %d %v", code, out)
	}
	channelID := out["id"].(string)

	// --- validation -------------------------------------------------------------------------------
	// A flow with a trigger but no action can be drafted, never switched on.
	code, out, _ = req(t, api, "POST", "/api/v1/automations", "Bearer "+authA.AccessToken, "", "", map[string]any{
		"name": "ยังไม่เสร็จ", "enabled": true,
		"definition": flow([]map[string]any{block("t1", "trigger.event", 0, 0, map[string]any{"event_types": []string{"tamper"}})}, nil),
	})
	if code != 400 || !problemCodes(out)["no_action"] {
		t.Fatalf("a trigger-only flow must not be enabled: %d %v", code, out)
	}
	// An unknown block type is refused too.
	code, out, _ = req(t, api, "POST", "/api/v1/automations/validate", "Bearer "+authA.AccessToken, "", "", map[string]any{
		"definition": flow([]map[string]any{block("t1", "trigger.telepathy", 0, 0, nil)}, nil),
	})
	if code != 200 || out["ok"] != false || !problemCodes(out)["unknown_type"] {
		t.Fatalf("validate: %d %v", code, out)
	}
	// An action.notify pointing at a channel of another workspace is refused.
	code, out, _ = req(t, api, "POST", "/api/v1/automations/validate", "Bearer "+authB.AccessToken, "", "", map[string]any{
		"enabled": true,
		"definition": flow([]map[string]any{
			block("t1", "trigger.event", 0, 0, map[string]any{"event_types": []string{"tamper"}}),
			block("n1", "action.notify", 300, 0, map[string]any{"channel_ids": []string{channelID}, "message": "hi"}),
		}, []map[string]any{link("e1", "t1", "n1")}),
	})
	if code != 200 || !problemCodes(out)["unknown_channel"] {
		t.Fatalf("a channel of another workspace must not validate: %d %v", code, out)
	}

	// --- the tamper flow --------------------------------------------------------------------------
	tamperDef := flow([]map[string]any{
		block("t1", "trigger.event", 0, 0, map[string]any{"event_types": []string{"tamper"}}),
		block("a1", "action.alert", 320, -60, map[string]any{"severity": "critical", "title": "ป้ายกันถอดถูกแกะ · {{device}}"}),
		block("n1", "action.notify", 320, 80, map[string]any{"channel_ids": []string{channelID}, "message": "{{device}} ถูกแกะป้าย"}),
	}, []map[string]any{link("e1", "t1", "a1"), link("e2", "t1", "n1")})
	code, out, _ = req(t, api, "POST", "/api/v1/automations", "Bearer "+authA.AccessToken, "", "", map[string]any{
		"name": "ป้ายกันถอดถูกแกะ", "description": "แจ้งเตือนวิกฤต", "enabled": true, "definition": tamperDef,
	})
	if code != 201 || out["enabled"] != true {
		t.Fatalf("create tamper flow: %d %v", code, out)
	}
	tamperID := out["id"].(string)
	if out["revision"].(float64) != 1 {
		t.Fatalf("first revision: %v", out["revision"])
	}

	// --- tenant isolation -------------------------------------------------------------------------
	if code, _, _ = req(t, api, "GET", "/api/v1/automations/"+tamperID, "Bearer "+authB.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("another tenant read the flow: %d", code)
	}
	if code, _, _ = req(t, api, "POST", "/api/v1/automations/"+tamperID+"/delete", "Bearer "+authB.AccessToken, "", "", map[string]any{}); code != 404 {
		t.Fatalf("another tenant deleted the flow: %d", code)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/automations", "Bearer "+authB.AccessToken, "", "", nil)
	if code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("flows leaked across tenants: %d %v", code, out)
	}

	// --- stale revision ---------------------------------------------------------------------------
	save := func(token string, revision int) (int, map[string]any) {
		code, out, _ := req(t, api, "POST", "/api/v1/automations/"+tamperID+"/save", "Bearer "+token, "", "", map[string]any{
			"name": "ป้ายกันถอดถูกแกะ", "description": "แจ้งเตือนวิกฤต", "enabled": true, "definition": tamperDef, "revision": revision,
		})
		return code, out
	}
	if code, out = save(authA.AccessToken, 1); code != 200 || out["revision"].(float64) != 2 {
		t.Fatalf("save: %d %v", code, out)
	}
	if code, out = save(authA.AccessToken, 1); code != 409 {
		t.Fatalf("a stale revision must be a conflict: %d %v", code, out)
	}

	// --- action.command can be drafted, but not enabled while AUTOMATION_COMMANDS is off (the default) ---
	code, out, _ = req(t, api, "POST", "/api/v1/automations", "Bearer "+authA.AccessToken, "", "", map[string]any{
		"name": "สั่งงานอุปกรณ์ (ร่าง)",
		"definition": flow([]map[string]any{
			block("t1", "trigger.event", 0, 0, map[string]any{"event_types": []string{"button"}}),
			block("k1", "action.command", 320, 0, nil),
		}, []map[string]any{link("e1", "t1", "k1")}),
	})
	if code != 201 || out["enabled"] != false {
		t.Fatalf("a draft with a command block must be storable: %d %v", code, out)
	}
	commandID := out["id"].(string)
	code, out, _ = req(t, api, "POST", "/api/v1/automations/"+commandID+"/enable", "Bearer "+authA.AccessToken, "", "", map[string]any{"enabled": true})
	if code != 400 || !problemCodes(out)["command_disabled"] {
		t.Fatalf("with AUTOMATION_COMMANDS off a flow with action.command must not be enabled: %d %v", code, out)
	}

	// --- the metric flow --------------------------------------------------------------------------
	// S1 sensor f00000000001 reports 22-23 °C, f00000000002 reports 24-25 °C.
	metricDef := flow([]map[string]any{
		block("t1", "trigger.metric", 0, 0, map[string]any{"external_id": "f00000000001", "metric": "temperature", "op": ">", "value": 10, "for_sec": 0}),
		block("c1", "cond.device", 300, 0, map[string]any{"external_id": "f00000000002", "metric": "temperature", "op": ">", "value": 10, "max_age_sec": 600}),
		block("a1", "action.alert", 620, -60, map[string]any{"severity": "warning", "title": "ทั้งสองห้องอุ่นเกิน"}),
		block("a2", "action.alert", 620, 80, map[string]any{"severity": "info", "title": "ห้องที่สองยังเย็น"}),
	}, []map[string]any{link("e1", "t1", "c1"), link("e2", "c1", "a1", "true"), link("e3", "c1", "a2", "false")})
	code, out, _ = req(t, api, "POST", "/api/v1/automations", "Bearer "+authA.AccessToken, "", "", map[string]any{
		"name": "ห้องเย็นอุ่นเกิน", "enabled": true, "definition": metricDef,
	})
	if code != 201 {
		t.Fatalf("create metric flow: %d %v", code, out)
	}
	metricID := out["id"].(string)

	// A flow whose only path is the true branch of a condition that is false must do nothing at all.
	quietDef := flow([]map[string]any{
		block("t1", "trigger.metric", 0, 0, map[string]any{"external_id": "f00000000001", "metric": "temperature", "op": ">", "value": 10, "for_sec": 0}),
		block("c1", "cond.device", 300, 0, map[string]any{"external_id": "f00000000002", "metric": "temperature", "op": ">", "value": 500, "max_age_sec": 600}),
		block("a1", "action.alert", 620, 0, map[string]any{"severity": "critical", "title": "ต้องไม่เกิดขึ้น"}),
	}, []map[string]any{link("e1", "t1", "c1"), link("e2", "c1", "a1", "true")})
	code, out, _ = req(t, api, "POST", "/api/v1/automations", "Bearer "+authA.AccessToken, "", "", map[string]any{
		"name": "สาขาเท็จ", "enabled": true, "definition": quietDef,
	})
	if code != 201 {
		t.Fatalf("create false-branch flow: %d %v", code, out)
	}
	quietID := out["id"].(string)

	// --- ingest -----------------------------------------------------------------------------------
	now := time.Now().UTC()
	// Step 0: the tamper flag is clear, the S1 sensors report their temperatures.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(0, now)); e != nil {
		t.Fatal(e)
	}
	// Step 27: the MBT01 anti-tamper tag raises the flag.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(27, now.Add(5*time.Second))); e != nil {
		t.Fatal(e)
	}
	// Step 28: still tampered, still warm — nothing new may fire.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(28, now.Add(10*time.Second))); e != nil {
		t.Fatal(e)
	}

	titles := countTitles(t, f, a)
	tamperAlerts := 0
	opened, e := f.repo.ListAlerts(ctx, a, "", 200)
	if e != nil {
		t.Fatal(e)
	}
	for _, al := range opened {
		if al.ExternalID == "f00000000009" {
			tamperAlerts++
			if al.Severity != "critical" {
				t.Fatalf("the tamper flow opens a critical alert: %+v", al)
			}
			if al.RuleID != nil {
				t.Fatalf("an automation alert belongs to no rule: %+v", al)
			}
		}
	}
	if tamperAlerts != 1 {
		t.Fatalf("one firing per identity per minute, got %d alerts: %+v", tamperAlerts, opened)
	}
	notes, e := f.repo.ListNotifications(ctx, a, 50)
	if e != nil {
		t.Fatal(e)
	}
	if len(notes) != 1 {
		t.Fatalf("the tamper flow queues exactly one notification: %+v", notes)
	}
	// The existing worker delivers it: an automation reuses the alert pipeline, it does not shadow it.
	worker := &alerts.Worker{Store: f.repo, Sender: httpapi.NewSender(cfg), Secrets: f.service.Secrets, Batch: 10}
	worker.Tick(ctx, time.Now().UTC())
	if received.Load() != 1 {
		t.Fatalf("the automation's notification must reach the channel exactly once: %d", received.Load())
	}
	if titles["ทั้งสองห้องอุ่นเกิน"] != 1 {
		t.Fatalf("the metric flow's true branch runs once: %v", titles)
	}
	if titles["ห้องที่สองยังเย็น"] != 0 {
		t.Fatalf("the false branch must not run when the condition holds: %v", titles)
	}
	if titles["ต้องไม่เกิดขึ้น"] != 0 {
		t.Fatalf("a false condition must not reach its true-branch action: %v", titles)
	}
	// The flow that decided nothing still records why.
	code, out, _ = req(t, api, "GET", "/api/v1/automations/"+quietID+"/runs", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("runs: %d %v", code, out)
	}
	runs := out["items"].([]any)
	if len(runs) == 0 || runs[0].(map[string]any)["status"] != "skipped" {
		t.Fatalf("a flow that did not fire records a skipped run: %v", runs)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/automations/"+metricID+"/runs", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 || len(out["items"].([]any)) == 0 {
		t.Fatalf("the metric flow records its firing: %d %v", code, out)
	}
	if code, _, _ = req(t, api, "GET", "/api/v1/automations/"+metricID+"/runs", "Bearer "+authB.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("run history leaked across tenants: %d", code)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/automations/"+tamperID, "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 || out["fire_count"].(float64) != 1 || out["last_fired_at"] == nil {
		t.Fatalf("the counters follow the firing: %d %v", code, out)
	}

	// --- dry run ----------------------------------------------------------------------------------
	before := len(opened)
	code, out, _ = req(t, api, "POST", "/api/v1/automations/"+metricID+"/test", "Bearer "+authA.AccessToken, "", "", map[string]any{
		"external_id": "f00000000001", "metric_value": 30,
	})
	if code != 200 || out["executed"] != false {
		t.Fatalf("dry run: %d %v", code, out)
	}
	trace := out["result"].(map[string]any)["nodes"].(map[string]any)
	if trace["t1"] != "true" || trace["c1"] != "true" || trace["a1"] != "ran" || trace["a2"] != "idle" {
		t.Fatalf("dry-run trace: %v", trace)
	}
	if after, _ := f.repo.ListAlerts(ctx, a, "", 200); len(after) != before {
		t.Fatalf("a dry run must not open alerts: %d then %d", before, len(after))
	}
	if notes, _ := f.repo.ListNotifications(ctx, a, 50); len(notes) != 1 {
		t.Fatalf("a dry run must not queue notifications: %+v", notes)
	}
	// A value that does not satisfy the trigger reaches nothing.
	code, out, _ = req(t, api, "POST", "/api/v1/automations/"+metricID+"/test", "Bearer "+authA.AccessToken, "", "", map[string]any{
		"external_id": "f00000000001", "metric_value": 1,
	})
	if code != 200 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if trace = out["result"].(map[string]any)["nodes"].(map[string]any); trace["t1"] != "idle" || trace["a1"] != "idle" {
		t.Fatalf("an unmatched trigger reaches nothing: %v", trace)
	}
	if code, _, _ = req(t, api, "POST", "/api/v1/automations/"+metricID+"/test", "Bearer "+authB.AccessToken, "", "", map[string]any{"external_id": "f00000000001", "metric_value": 30}); code != 404 {
		t.Fatalf("another tenant dry-ran the flow: %d", code)
	}

	// --- disable stops the pipeline ---------------------------------------------------------------
	for _, flowID := range []string{tamperID, metricID, quietID} {
		if code, out, _ = req(t, api, "POST", "/api/v1/automations/"+flowID+"/enable", "Bearer "+authA.AccessToken, "", "", map[string]any{"enabled": false}); code != 200 || out["enabled"] != false {
			t.Fatalf("disable: %d %v", code, out)
		}
	}
	// Clear the tamper flag, then raise it again: a disabled flow must stay silent.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(35, now.Add(90*time.Second))); e != nil {
		t.Fatal(e)
	}
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(67, now.Add(95*time.Second))); e != nil {
		t.Fatal(e)
	}
	after, e := f.repo.ListAlerts(ctx, a, "", 200)
	if e != nil {
		t.Fatal(e)
	}
	if len(after) != before {
		t.Fatalf("a disabled flow must not act: %d then %d", before, len(after))
	}

	// --- delete -----------------------------------------------------------------------------------
	if code, _, _ = req(t, api, "POST", "/api/v1/automations/"+quietID+"/delete", "Bearer "+authA.AccessToken, "", "", map[string]any{}); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, _, _ = req(t, api, "GET", "/api/v1/automations/"+quietID, "Bearer "+authA.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("a deleted flow is gone: %d", code)
	}
	// Its run history cascaded with it.
	var remaining int64
	if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.automation_runs WHERE automation_id=$1`, quietID).Scan(&remaining); e != nil {
		t.Fatal(e)
	}
	if remaining != 0 {
		t.Fatalf("deleting a flow must take its runs with it: %d left", remaining)
	}
	// Tenant B still has nothing of its own.
	if list, e := f.repo.ListAutomations(ctx, b); e != nil || len(list) != 0 {
		t.Fatalf("tenant isolation: %+v %v", list, e)
	}
}

// A trigger.metric block fires on the rising edge only, and only once `for_sec` has elapsed.
func TestAutomationMetricDwellTime(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, authA, a := f.account(t)
	api := httpapi.New(f.cfg, f.service, f.repo)
	g, _, e := f.service.CreateGateway(ctx, a, "Cold room", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	def := flow([]map[string]any{
		block("t1", "trigger.metric", 0, 0, map[string]any{"external_id": "f00000000001", "metric": "temperature", "op": ">", "value": 10, "for_sec": 300}),
		block("a1", "action.alert", 320, 0, map[string]any{"severity": "warning", "title": "อุ่นเกินมานาน"}),
	}, []map[string]any{link("e1", "t1", "a1")})
	code, out, _ := req(t, api, "POST", "/api/v1/automations", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "ห้องเย็นอุ่นนาน", "enabled": true, "definition": def})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["id"].(string)

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(i, now.Add(time.Duration(i)*5*time.Second))); e != nil {
			t.Fatal(e)
		}
	}
	opened, e := f.repo.ListAlerts(ctx, a, "", 50)
	if e != nil {
		t.Fatal(e)
	}
	if len(opened) != 0 {
		t.Fatalf("for_sec=300 must hold the firing back for five minutes: %+v", opened)
	}
	// Backdate the episode so the dwell time has passed, then send one more uplink.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.automation_state SET since=now()-interval '10 minutes' WHERE automation_id=$1`, id); e != nil {
		t.Fatal(e)
	}
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(3, now.Add(20*time.Second))); e != nil {
		t.Fatal(e)
	}
	if opened, e = f.repo.ListAlerts(ctx, a, "", 50); e != nil || len(opened) != 1 || opened[0].Title != "อุ่นเกินมานาน" {
		t.Fatalf("once the dwell time passes the flow fires once: %+v %v", opened, e)
	}
	// The episode already fired: further uplinks with the same comparison stay silent.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(4, now.Add(25*time.Second))); e != nil {
		t.Fatal(e)
	}
	if opened, e = f.repo.ListAlerts(ctx, a, "", 50); e != nil || len(opened) != 1 {
		t.Fatalf("a rising edge fires once per episode: %+v %v", opened, e)
	}
	// The event the alert points at is a real row of its own.
	events, e := f.repo.ListEvents(ctx, a, "f00000000001", 50)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, ev := range events {
		if ev.ID == opened[0].EventID && ev.EventType == "automation" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a metric firing records its own device event: %+v", events)
	}
	if countType(events, domain.EventThreshold) != 0 {
		t.Fatalf("an automation must not masquerade as a threshold rule: %+v", events)
	}
}

// triggerRows reports what the trigger index holds for one flow: the total row count and the set of
// devices its rows name.
func triggerRows(t *testing.T, f *fixture, id string) (int, map[string]bool) {
	t.Helper()
	rows, e := f.admin.QueryContext(context.Background(),
		`SELECT kind,coalesce(event_type,''),coalesce(external_id,''),coalesce(metric,'') FROM core.automation_triggers WHERE automation_id=$1`, id)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	count, devices := 0, map[string]bool{}
	for rows.Next() {
		var kind, eventType, external, metric string
		if e := rows.Scan(&kind, &eventType, &external, &metric); e != nil {
			t.Fatal(e)
		}
		count++
		devices[external] = true
	}
	if e := rows.Err(); e != nil {
		t.Fatal(e)
	}
	return count, devices
}

// The trigger index is what decides whether an uplink looks at a flow at all, so it may never
// disagree with core.automations: enabling writes rows, disabling removes them, saving replaces
// them, deleting takes them with it — each inside the same transaction as the write itself.
func TestAutomationTriggerIndexStaysInSync(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, auth, a := f.account(t)
	api := httpapi.New(f.cfg, f.service, f.repo)

	watch := func(device string) map[string]any {
		return flow([]map[string]any{
			block("t1", "trigger.event", 0, 0, map[string]any{"event_types": []string{"tamper", "leak"}, "external_ids": []string{device}}),
			block("t2", "trigger.metric", 0, 160, map[string]any{"external_id": device, "metric": "temperature", "op": ">", "value": 28}),
			block("o1", "logic.any", 320, 80, nil),
			block("a1", "action.alert", 620, 80, map[string]any{"severity": "warning", "title": "ดูอยู่"}),
		}, []map[string]any{link("e1", "t1", "o1"), link("e2", "t2", "o1"), link("e3", "o1", "a1")})
	}

	// A draft is not evaluated, so it is not indexed.
	code, out, _ := req(t, api, "POST", "/api/v1/automations", "Bearer "+auth.AccessToken, "", "", map[string]any{
		"name": "ร่าง", "enabled": false, "definition": watch("f00000000001"),
	})
	if code != 201 {
		t.Fatalf("create draft: %d %v", code, out)
	}
	id, revision := out["id"].(string), int(out["revision"].(float64))
	if count, _ := triggerRows(t, f, id); count != 0 {
		t.Fatalf("a disabled flow is not indexed: %d rows", count)
	}

	// Enabling it writes one row per event type plus one for the metric block.
	if code, out, _ = req(t, api, "POST", "/api/v1/automations/"+id+"/enable", "Bearer "+auth.AccessToken, "", "", map[string]any{"enabled": true}); code != 200 {
		t.Fatalf("enable: %d %v", code, out)
	}
	revision = int(out["revision"].(float64))
	count, devices := triggerRows(t, f, id)
	if count != 3 || !devices["f00000000001"] {
		t.Fatalf("enabling indexes both triggers: %d rows, %v", count, devices)
	}

	// Saving a definition that watches another device replaces the rows; nothing of the old one stays.
	code, out, _ = req(t, api, "POST", "/api/v1/automations/"+id+"/save", "Bearer "+auth.AccessToken, "", "", map[string]any{
		"name": "ร่าง", "enabled": true, "revision": revision, "definition": watch("f00000000009"),
	})
	if code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	count, devices = triggerRows(t, f, id)
	if count != 3 || devices["f00000000001"] || !devices["f00000000009"] {
		t.Fatalf("saving replaces the index rows: %d rows, %v", count, devices)
	}

	// Disabling empties the index: an uplink cannot even see the flow.
	if code, _, _ = req(t, api, "POST", "/api/v1/automations/"+id+"/enable", "Bearer "+auth.AccessToken, "", "", map[string]any{"enabled": false}); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	if count, _ = triggerRows(t, f, id); count != 0 {
		t.Fatalf("disabling clears the index: %d rows", count)
	}

	// A flow created enabled is indexed by the create itself.
	code, out, _ = req(t, api, "POST", "/api/v1/automations", "Bearer "+auth.AccessToken, "", "", map[string]any{
		"name": "เปิดตั้งแต่แรก", "enabled": true, "definition": watch("f00000000005"),
	})
	if code != 201 {
		t.Fatalf("create enabled: %d %v", code, out)
	}
	born := out["id"].(string)
	if count, devices = triggerRows(t, f, born); count != 3 || !devices["f00000000005"] {
		t.Fatalf("creating an enabled flow indexes it: %d rows, %v", count, devices)
	}

	// Deleting takes the rows with it.
	if code, _, _ = req(t, api, "POST", "/api/v1/automations/"+born+"/delete", "Bearer "+auth.AccessToken, "", "", map[string]any{}); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if count, _ = triggerRows(t, f, born); count != 0 {
		t.Fatalf("deleting a flow takes its index rows with it: %d left", count)
	}
	if list, e := f.repo.ListAutomations(ctx, a); e != nil || len(list) != 1 {
		t.Fatalf("one flow left: %+v %v", list, e)
	}
}
