package tests

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/reportjobs"
	"aether/backend/internal/reports"
	"github.com/gofiber/fiber/v3"
)

// Scheduled delivery of saved reports (migration 00050, docs/platform/reports.md "Scheduled delivery").

type scheduleRig struct {
	*reportRig
	api   *fiber.App
	hooks atomic.Int32
	fail  atomic.Bool // the webhook answers 500
	last  atomic.Value
	hook  *httptest.Server
	smtp  *testSMTP
}

func newScheduleRig(t *testing.T) *scheduleRig {
	t.Helper()
	r := &scheduleRig{reportRig: newReportRig(t), smtp: &testSMTP{}}
	r.hook = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.fail.Load() {
			w.WriteHeader(500)
			return
		}
		var body map[string]any
		if req.Header.Get("X-Aether-Event") != "report" || !strings.HasPrefix(req.Header.Get("X-Aether-Signature"), "sha256=") || json.NewDecoder(req.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		r.last.Store(body)
		r.hooks.Add(1)
		w.WriteHeader(204)
	}))
	t.Cleanup(r.hook.Close)
	host, port := r.smtp.serve(t)
	cfg := r.f.cfg
	cfg.Environment = "development" // the loopback webhook target
	cfg.APIRateLimit = 600
	cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPFrom = host, port, "aether@example.test"
	r.api = httpapi.New(cfg, r.f.service, r.f.repo)
	return r
}

// worker delivers this rig's workspace only, with SMTP when smtp is set.
func (r *scheduleRig) worker(smtp bool) *reportjobs.Worker {
	cfg := r.f.cfg
	cfg.Environment = "development"
	if smtp {
		cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPFrom = r.smtp.host, r.smtp.port, "aether@example.test"
	}
	return &reportjobs.Worker{Store: rigStore{r.f.repo, r.owner.TenantID}, Sender: httpapi.NewSender(cfg), Secrets: r.f.service.Secrets, Origin: "https://aether.example.test", RunsPerTick: 20}
}

func (r *scheduleRig) channel(name, kind string, config map[string]string, project *string) string {
	r.t.Helper()
	payload := map[string]any{"name": name, "kind": kind, "config": config}
	if kind == "webhook" {
		payload["secret"] = "hook secret"
	}
	if project != nil {
		payload["project_id"] = *project
	}
	code, out, _ := req(r.t, r.api, "POST", "/api/v1/channels", "Bearer "+r.ownerToken, "", "", payload)
	if code != 201 {
		r.t.Fatalf("channel %s: %d %v", name, code, out)
	}
	return out["id"].(string)
}

func (r *scheduleRig) hookChannel(name string, project *string) string {
	return r.channel(name, "webhook", map[string]string{"url": r.hook.URL + "/reports"}, project)
}

func (r *scheduleRig) define(token string, payload map[string]any) (int, map[string]any) {
	r.t.Helper()
	base := map[string]any{"name": "ห้องเย็น", "sensors": []map[string]string{reportSensor(r.gwA, r.fixedA)}, "temp_min": 2, "temp_max": 8}
	for k, v := range payload {
		base[k] = v
	}
	code, out, _ := req(r.t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+token, "", "", base)
	return code, out
}

func (r *scheduleRig) count(q string, args ...any) int {
	r.t.Helper()
	var n int
	if e := r.f.admin.QueryRow(q, args...).Scan(&n); e != nil {
		r.t.Fatal(e)
	}
	return n
}

func (r *scheduleRig) delivery(run string) (status string, attempts int, lastError string) {
	r.t.Helper()
	var le *string
	if e := r.f.admin.QueryRow(`SELECT status, attempts, last_error FROM core.report_deliveries WHERE run_id=$1`, run).Scan(&status, &attempts, &le); e != nil {
		r.t.Fatal(e)
	}
	if le != nil {
		lastError = *le
	}
	return
}

func TestReportScheduleValidation(t *testing.T) {
	r := newScheduleRig(t)
	ws, onB, off := r.hookChannel("ws", nil), r.hookChannel("b", &r.projB), r.hookChannel("off", nil)
	if code, _, _ := req(t, r.api, "POST", "/api/v1/channels/"+off+"/update", "Bearer "+r.ownerToken, "", "", map[string]any{"enabled": false}); code != 204 {
		t.Fatal(code)
	}
	for _, c := range []struct {
		name    string
		payload map[string]any
		reason  string
	}{
		{"project channel on a workspace report", map[string]any{"schedule": "daily", "schedule_time": "07:30", "channels": []string{onB}}, "invalid_channels"},
		{"disabled channel", map[string]any{"schedule": "daily", "schedule_time": "07:30", "channels": []string{off}}, "invalid_channels"},
		{"unknown channel", map[string]any{"schedule": "daily", "schedule_time": "07:30", "channels": []string{r.gwA}}, "invalid_channels"},
		{"schedule without channels", map[string]any{"schedule": "daily", "schedule_time": "07:30"}, "no_channels"},
		{"bad time", map[string]any{"schedule": "daily", "schedule_time": "24:00", "channels": []string{ws}}, "invalid_schedule"},
		{"signed time", map[string]any{"schedule": "daily", "schedule_time": "+1:00", "channels": []string{ws}}, "invalid_schedule"},
		{"spaced time", map[string]any{"schedule": "daily", "schedule_time": " 1:00", "channels": []string{ws}}, "invalid_schedule"},
		{"weekly without a day", map[string]any{"schedule": "weekly", "schedule_time": "07:30", "channels": []string{ws}}, "invalid_schedule"},
		{"daily with a day", map[string]any{"schedule": "daily", "schedule_weekday": 1, "schedule_time": "07:30", "channels": []string{ws}}, "invalid_schedule"},
		{"project report with another project's channel", map[string]any{"project_id": r.projectA, "schedule": "daily", "schedule_time": "07:30", "channels": []string{onB}}, "invalid_channels"},
	} {
		if code, out := r.define(r.ownerToken, c.payload); code != 400 || out["error"] != c.reason {
			t.Fatalf("%s: %d %v", c.name, code, out)
		}
	}
	before := time.Now()
	code, def := r.define(r.ownerToken, map[string]any{"schedule": "weekly", "schedule_weekday": 1, "schedule_time": "00:00", "channels": []string{ws}})
	if code != 201 || def["schedule"] != "weekly" || def["schedule_weekday"] != 1.0 || def["schedule_time"] != "00:00" {
		t.Fatalf("weekly report: %d %v", code, def)
	}
	next, _ := time.Parse(time.RFC3339, def["next_run_at"].(string))
	want := reports.Schedule{Kind: "weekly", Weekday: 1, Time: "00:00"}.Next(before, reports.Bangkok())
	if !next.Equal(want) || next.In(reports.Bangkok()).Weekday() != time.Monday || next.In(reports.Bangkok()).Hour() != 0 {
		t.Fatalf("next run %s, want %s", next, want)
	}
	// A project report may use its own project's channel; turning the schedule off clears next_run_at.
	if code, out := r.define(r.ownerToken, map[string]any{"project_id": r.projectA, "schedule": "daily", "schedule_time": "06:00", "channels": []string{r.hookChannel("a", &r.projectA)}}); code != 201 {
		t.Fatalf("project channel: %d %v", code, out)
	}
	code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+def["id"].(string)+"/update", "Bearer "+r.ownerToken, "", "", map[string]any{"name": "x", "sensors": []map[string]string{reportSensor(r.gwA, r.fixedA)}, "channels": []string{ws}})
	if code != 200 || out["schedule"] != "none" || out["next_run_at"] != nil || len(out["channels"].([]any)) != 1 {
		t.Fatalf("schedule off: %d %v", code, out)
	}
	// The editor's channel list: names and kinds, no configs, and whether email works on this server.
	code, out = get(t, r.api, "/api/v1/reports/channels", r.ownerToken)
	if code != 200 || out["email_available"] != true || len(rows(out)) != 4 || rows(out)[0]["config"] != nil {
		t.Fatalf("channels: %d %v", code, out)
	}
}

// Two replicas tick at the same moment: one run, one webhook. A later tick sends nothing more; after downtime only
// the latest due period is sent.
func TestReportSchedulerRunsOnceAndDelivers(t *testing.T) {
	r := newScheduleRig(t)
	ctx := context.Background()
	ws := r.hookChannel("ws", nil)
	code, def := r.define(r.ownerToken, map[string]any{"schedule": "daily", "schedule_time": "07:30", "channels": []string{ws}})
	if code != 201 {
		t.Fatal(code, def)
	}
	id := def["id"].(string)
	r.exec(`UPDATE core.report_definitions SET next_run_at = now() - interval '1 minute' WHERE id=$1`, id)
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.worker(false).Tick(ctx, now) }()
	}
	wg.Wait()
	r.worker(false).Tick(ctx, now.Add(time.Second))
	if n := r.count(`SELECT count(*) FROM core.report_runs WHERE report_id=$1`, id); n != 1 || r.hooks.Load() != 1 {
		t.Fatalf("runs %d, webhooks %d", n, r.hooks.Load())
	}
	loc := reports.Bangkok()
	sched := reports.Schedule{Kind: "daily", Time: "07:30"}
	wantFrom, wantTo := sched.Period(sched.Latest(now, loc), loc)
	var from, to time.Time
	var status string
	if e := r.f.admin.QueryRow(`SELECT period_from, period_to, status FROM core.report_runs WHERE report_id=$1`, id).Scan(&from, &to, &status); e != nil {
		t.Fatal(e)
	}
	if !from.Equal(wantFrom) || !to.Equal(wantTo) || status != "done" {
		t.Fatalf("run period %s..%s %s, want %s..%s", from, to, status, wantFrom, wantTo)
	}
	var next time.Time
	if e := r.f.admin.QueryRow(`SELECT next_run_at FROM core.report_definitions WHERE id=$1`, id).Scan(&next); e != nil || !next.Equal(sched.Next(now, loc)) {
		t.Fatalf("next run %s: %v", next, e)
	}
	body := r.last.Load().(map[string]any)
	sensors := body["sensors"].([]any)
	if body["schema"] != "aether.report.v1" || body["report"].(map[string]any)["id"] != id || len(sensors) != 1 || sensors[0].(map[string]any)["name"] != "Fridge A" ||
		!strings.HasPrefix(body["link"].(string), "https://aether.example.test/?reports&") {
		t.Fatalf("webhook body: %v", body)
	}
	// Four days down: one run for the latest period, not four.
	r.exec(`UPDATE core.report_definitions SET next_run_at = now() - interval '4 days' WHERE id=$1`, id)
	later := now.Add(4 * 24 * time.Hour)
	r.worker(false).Tick(ctx, later)
	if n := r.count(`SELECT count(*) FROM core.report_runs WHERE report_id=$1`, id); n != 2 {
		t.Fatalf("after downtime: %d runs", n)
	}
	latestFrom, _ := sched.Period(sched.Latest(later, loc), loc)
	if n := r.count(`SELECT count(*) FROM core.report_runs WHERE report_id=$1 AND period_from=$2`, id, latestFrom); n != 1 {
		t.Fatal("the run after downtime is not the latest period")
	}
}

// A failing webhook is retried with the backoff of alert notifications and fails after 5 attempts; email on a server
// without SMTP is skipped with a reason; a demo workspace's deliveries are skipped (the report still shows on screen).
func TestReportDeliveryRetryAndSkips(t *testing.T) {
	r := newScheduleRig(t)
	ctx := context.Background()
	ws := r.hookChannel("ws", nil)
	mailCh := r.channel("mail", "email", map[string]string{"to": "ops@example.test"}, nil)
	code, def := r.define(r.ownerToken, map[string]any{"channels": []string{ws, mailCh}})
	if code != 201 {
		t.Fatal(code, def)
	}
	id := def["id"].(string)
	r.fail.Store(true)
	code, run, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+id+"/send", "Bearer "+r.ownerToken, "", "", map[string]any{})
	if code != 202 {
		t.Fatal(code, run)
	}
	// Default period: yesterday in Bangkok.
	wantFrom, wantTo := reports.LastFullDay(time.Now(), reports.Bangkok())
	from, _ := time.Parse(time.RFC3339, run["period_from"].(string))
	to, _ := time.Parse(time.RFC3339, run["period_to"].(string))
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("manual period: %v", run)
	}
	// The answer is the run as stored: its time, status and one queued delivery per channel.
	if created, _ := time.Parse(time.RFC3339Nano, run["created_at"].(string)); created.IsZero() || time.Since(created) > time.Minute || run["status"] != "queued" || len(listOf(run, "deliveries")) != 2 || listOf(run, "deliveries")[0]["status"] != "queued" {
		t.Fatalf("send answer: %v", run)
	}
	runID := run["id"].(string)
	w := r.worker(false)
	w.Tick(ctx, time.Now())
	var hookStatus, hookErr, mailStatus, mailErr string
	var hookAttempts int
	scan := func() {
		if e := r.f.admin.QueryRow(`SELECT status, attempts, coalesce(last_error,'') FROM core.report_deliveries WHERE run_id=$1 AND channel_id=$2`, runID, ws).Scan(&hookStatus, &hookAttempts, &hookErr); e != nil {
			t.Fatal(e)
		}
		if e := r.f.admin.QueryRow(`SELECT status, coalesce(last_error,'') FROM core.report_deliveries WHERE run_id=$1 AND channel_id=$2`, runID, mailCh).Scan(&mailStatus, &mailErr); e != nil {
			t.Fatal(e)
		}
	}
	scan()
	if hookStatus != "queued" || hookAttempts != 1 || hookErr == "" || mailStatus != "skipped" || !strings.HasPrefix(mailErr, "smtp_not_configured") {
		t.Fatalf("first pass: hook %s/%d/%q mail %s/%q", hookStatus, hookAttempts, hookErr, mailStatus, mailErr)
	}
	// Not due again before its backoff (30 s after the first attempt).
	w.Tick(ctx, time.Now().Add(10*time.Second))
	if scan(); hookAttempts != 1 {
		t.Fatalf("retried before the backoff: %d", hookAttempts)
	}
	for i := 1; i <= 5; i++ {
		w.Tick(ctx, time.Now().Add(time.Duration(i)*time.Hour))
	}
	scan()
	if hookStatus != "failed" || hookAttempts != 5 {
		t.Fatalf("after five attempts: %s/%d", hookStatus, hookAttempts)
	}
	if n := r.count(`SELECT count(*) FROM core.report_runs WHERE id=$1 AND status='done' AND finished_at IS NOT NULL`, runID); n != 1 {
		t.Fatal("the run is not done once every delivery settled")
	}
	// A demo workspace: queued deliveries are skipped, nothing is sent.
	r.fail.Store(false)
	r.exec(`UPDATE core.tenants SET demo=true WHERE id=$1`, r.owner.TenantID)
	code, run, _ = req(t, r.api, "POST", "/api/v1/reports/definitions/"+id+"/send", "Bearer "+r.ownerToken, "", "", map[string]any{})
	if code != 202 {
		t.Fatal(code, run)
	}
	w.Tick(ctx, time.Now())
	if n := r.count(`SELECT count(*) FROM core.report_deliveries WHERE run_id=$1 AND status='skipped' AND last_error LIKE 'demo workspace%'`, run["id"]); n != 2 || r.hooks.Load() != 0 {
		t.Fatalf("demo: %d skipped, %d webhooks", n, r.hooks.Load())
	}
	// The on-screen summary of the demo workspace still works.
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+r.ownerToken, "", "", r.window(map[string]any{"report_id": id})); code != 200 {
		t.Fatalf("demo summary: %d %v", code, out)
	}
}

// Sending by hand needs write access and is limited per workspace; runs are read through the report's visibility.
func TestReportManualSendLimitAndRunVisibility(t *testing.T) {
	r := newScheduleRig(t)
	ctx := context.Background()
	ws, onA, onB := r.hookChannel("ws", nil), r.hookChannel("a", &r.projectA), r.hookChannel("b", &r.projB)
	_, wsDef := r.define(r.ownerToken, map[string]any{"channels": []string{ws}})
	_, aDef := r.define(r.ownerToken, map[string]any{"name": "A", "project_id": r.projectA, "channels": []string{onA}})
	code, bDef := r.define(r.ownerToken, map[string]any{"name": "B", "project_id": r.projB, "sensors": []map[string]string{reportSensor(r.gwB, r.fixedB)}, "channels": []string{onB}})
	if code != 201 {
		t.Fatal(code, bDef)
	}
	noChannels := func() string {
		_, d := r.define(r.ownerToken, map[string]any{"name": "silent"})
		return d["id"].(string)
	}()
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+noChannels+"/send", "Bearer "+r.ownerToken, "", "", map[string]any{}); code != 400 || out["error"] != "no_channels" {
		t.Fatalf("send without channels: %d %v", code, out)
	}
	send := func(token, id string) (int, map[string]any) {
		code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+id+"/send", "Bearer "+token, "", "", r.window(nil))
		return code, out
	}
	for i := 0; i < 9; i++ {
		if code, out := send(r.ownerToken, wsDef["id"].(string)); code != 202 {
			t.Fatalf("send %d: %d %v", i, code, out)
		}
	}
	if code, out := send(r.ownerToken, bDef["id"].(string)); code != 202 {
		t.Fatalf("send 10: %d %v", code, out)
	}
	if code, out := send(r.ownerToken, aDef["id"].(string)); code != 429 || out["error"] != "too_many_sends" {
		t.Fatalf("send 11: %d %v", code, out)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+aDef["id"].(string)+"/send", "Bearer "+r.ownerToken, "", "", map[string]any{"from": r.from.Format(time.RFC3339), "to": r.from.Add(40 * 24 * time.Hour).Format(time.RFC3339)}); code != 400 || out["error"] != "window_too_long" {
		t.Fatalf("long window: %d %v", code, out)
	}
	r.worker(false).Tick(ctx, time.Now())
	code, out := get(t, r.api, "/api/v1/reports/definitions/"+wsDef["id"].(string)+"/runs", r.ownerToken)
	runs := rows(out)
	if code != 200 || len(runs) != 9 || runs[0]["trigger"] != "manual" || len(listOf(runs[0], "deliveries")) != 1 || listOf(runs[0], "deliveries")[0]["channel_name"] != "ws" || listOf(runs[0], "deliveries")[0]["status"] != "sent" {
		t.Fatalf("runs: %d %v", code, out)
	}
	// An operator limited to project A with read access to reports: no sending; runs of the workspace report and of
	// A's report readable, B's report not there at all.
	email := memberEmail()
	member := addMember(t, r.api, r.ownerToken, email, "operator", []string{r.projectA})
	if code, _, _ := req(t, r.api, "POST", "/api/v1/members/"+member+"/access", "Bearer "+r.ownerToken, "", "", map[string]string{"reports": "read"}); code != 204 {
		t.Fatal(code)
	}
	auth, p := changeInitialPassword(t, r.f, r.api, email, r.owner.TenantID)
	if code, _ := send(auth.AccessToken, aDef["id"].(string)); code != 403 {
		t.Fatalf("read-only send: %d", code)
	}
	if code, out := get(t, r.api, "/api/v1/reports/definitions/"+wsDef["id"].(string)+"/runs", auth.AccessToken); code != 200 || len(rows(out)) != 9 {
		t.Fatalf("member reads workspace runs: %d %v", code, out)
	}
	if code, _ := get(t, r.api, "/api/v1/reports/definitions/"+bDef["id"].(string)+"/runs", auth.AccessToken); code != 404 {
		t.Fatalf("member reads another project's runs: %d", code)
	}
	tx := asRuntime(t, r.f, p.UserID, p.TenantID)
	var n int
	if e := tx.QueryRow(`SELECT count(*) FROM core.report_runs WHERE project_id IS NOT NULL`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("member sees project B's runs at the database: %d %v", n, e)
	}
	if e := tx.QueryRow(`SELECT count(*) FROM core.report_deliveries`).Scan(&n); e != nil || n != 9 {
		t.Fatalf("member's deliveries: %d %v", n, e)
	}
	tx.Rollback()
	_, otherAuth, other := r.f.account(t)
	if code, _ := get(t, r.api, "/api/v1/reports/definitions/"+wsDef["id"].(string)+"/runs", otherAuth.AccessToken); code != 404 {
		t.Fatalf("another workspace reads runs: %d", code)
	}
	tx = asRuntime(t, r.f, other.UserID, other.TenantID)
	if e := tx.QueryRow(`SELECT count(*) FROM core.report_runs`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("another workspace's runs at the database: %d %v", n, e)
	}
	tx.Rollback()
	if n := r.count(`SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action='report.sent'`, r.owner.TenantID); n != 10 {
		t.Fatalf("audit rows: %d", n)
	}
}

// A "members" email reaches the people who may read the report (core.report_recipients) as blind copies, with the
// summary as text and HTML and the hourly CSV attached.
func TestReportEmailToMembers(t *testing.T) {
	r := newScheduleRig(t)
	ctx := context.Background()
	add := func(role string, projects []string, access map[string]string) string {
		email := memberEmail()
		id := addMember(t, r.api, r.ownerToken, email, role, projects)
		if access != nil {
			if code, _, _ := req(t, r.api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+r.ownerToken, "", "", access); code != 204 {
				t.Fatal(code)
			}
		}
		return email
	}
	operator := add("operator", nil, nil)
	limited := add("operator", []string{r.projectA}, nil)             // a workspace report is not theirs
	hidden := add("admin", nil, map[string]string{"reports": "none"}) // the module is hidden from them
	viewer := add("viewer", nil, nil)                                 // not one of the channel's roles
	members := r.channel("members", "email", map[string]string{"audience": "members", "roles": "owner,admin,operator"}, nil)
	_, def := r.define(r.ownerToken, map[string]any{"channels": []string{members}})
	code, run, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+def["id"].(string)+"/send", "Bearer "+r.ownerToken, "", "", r.window(nil))
	if code != 202 {
		t.Fatal(code, run)
	}
	r.worker(true).Tick(ctx, time.Now())
	if status, _, lastError := r.delivery(run["id"].(string)); status != "sent" {
		t.Fatalf("delivery: %s %s", status, lastError)
	}
	sent := r.smtp.sent()
	if len(sent) != 1 {
		t.Fatalf("messages: %d", len(sent))
	}
	got := map[string]bool{}
	for _, rcpt := range sent[0].rcpts {
		got[rcpt] = true
	}
	var owner string
	if e := r.f.admin.QueryRow(`SELECT email FROM identity.users WHERE id=$1`, r.owner.UserID).Scan(&owner); e != nil {
		t.Fatal(e)
	}
	if len(got) != 2 || !got[owner] || !got[operator] || got[limited] || got[hidden] || got[viewer] {
		t.Fatalf("recipients: %v", sent[0].rcpts)
	}
	msg, e := mail.ReadMessage(strings.NewReader(sent[0].data))
	if e != nil {
		t.Fatal(e)
	}
	if msg.Header.Get("To") != "undisclosed-recipients:;" {
		t.Fatalf("To: %q", msg.Header.Get("To"))
	}
	_, params, e := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if e != nil {
		t.Fatal(e)
	}
	parts := multipart.NewReader(msg.Body, params["boundary"])
	alt, _ := parts.NextPart()
	if kind, _, _ := mime.ParseMediaType(alt.Header.Get("Content-Type")); kind != "multipart/alternative" {
		t.Fatalf("first part: %v", alt.Header)
	}
	att, e := parts.NextPart()
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, att))
	csv := readCSV(t, string(raw))
	if !strings.HasSuffix(att.FileName(), "-1h.csv") || len(csv) != 2 || csv[1][1] != "Fridge A" || csv[1][4] != "12" {
		t.Fatalf("attachment %s: %v", att.FileName(), csv)
	}
}

// testSMTP accepts every recipient and records each message (the alerts package has its own for unit tests).
type testSMTP struct {
	mu       sync.Mutex
	host     string
	port     int
	messages []smtpMail
}

type smtpMail struct {
	rcpts []string
	data  string
}

func (s *testSMTP) serve(t *testing.T) (string, int) {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			go s.session(conn)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	s.host, s.port = addr.IP.String(), addr.Port
	return s.host, s.port
}

func (s *testSMTP) session(conn net.Conn) {
	defer conn.Close()
	rd, wr := bufio.NewReader(conn), bufio.NewWriter(conn)
	say := func(line string) { wr.WriteString(line + "\r\n"); wr.Flush() }
	say("220 test")
	var cur smtpMail
	for {
		line, e := rd.ReadString('\n')
		if e != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			say("250 test")
		case strings.HasPrefix(upper, "MAIL FROM"):
			cur = smtpMail{}
			say("250 ok")
		case strings.HasPrefix(upper, "RCPT TO"):
			cur.rcpts = append(cur.rcpts, strings.Trim(strings.TrimSpace(cmd[len("RCPT TO:"):]), "<>"))
			say("250 ok")
		case upper == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, e := rd.ReadString('\n')
				if e != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			cur.data = b.String()
			s.mu.Lock()
			s.messages = append(s.messages, cur)
			s.mu.Unlock()
			say("250 queued")
		case upper == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (s *testSMTP) sent() []smtpMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]smtpMail(nil), s.messages...)
}

// A project report whose gateway moved to another project: neither a scheduled or manual send nor the report on
// screen carries that sensor any more; it is listed as omitted (sensor_outside_project).
func TestReportProjectSensorMovedAway(t *testing.T) {
	r := newScheduleRig(t)
	ctx := context.Background()
	ws := r.hookChannel("ws", nil)
	code, def := r.define(r.ownerToken, map[string]any{"name": "A", "project_id": r.projectA, "channels": []string{ws}})
	if code != 201 {
		t.Fatal(code, def)
	}
	id := def["id"].(string)
	if e := r.f.repo.SetGatewayProject(ctx, r.owner, r.gwA, &r.projB); e != nil {
		t.Fatal(e)
	}
	code, run, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+id+"/send", "Bearer "+r.ownerToken, "", "", r.window(nil))
	if code != 202 {
		t.Fatal(code, run)
	}
	// What every channel would get, rendered from the run's content: no reading of the moved sensor anywhere.
	content, e := r.f.repo.ReportRunContent(ctx, r.owner.TenantID, run["id"].(string))
	if e != nil {
		t.Fatal(e)
	}
	if len(content.Sensors) != 0 || len(content.Omitted) != 1 || content.Omitted[0].Reason != "sensor_outside_project" {
		t.Fatalf("content: %+v", content)
	}
	built, e := reportjobs.Build(ctx, rigStore{r.f.repo, r.owner.TenantID}, r.owner.TenantID, content, "https://aether.example.test", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	d := built.Delivery(time.Now())
	for name, text := range map[string]string{"text/LINE": d.Text, "html": d.HTML, "csv": string(d.Attachment.Data)} {
		if strings.Contains(text, "Fridge A") || strings.Contains(text, r.fixedA) {
			t.Fatalf("%s carries the moved sensor:\n%s", name, text)
		}
	}
	r.worker(false).Tick(ctx, time.Now())
	body := r.last.Load().(map[string]any)
	omitted := body["omitted"].([]any)
	if len(body["sensors"].([]any)) != 0 || len(omitted) != 1 || omitted[0].(map[string]any)["reason"] != "sensor_outside_project" {
		t.Fatalf("webhook: %v", body)
	}
	// The same report on screen, for the owner who can see both projects.
	code, out, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+r.ownerToken, "", "", r.window(map[string]any{"report_id": id}))
	if code != 200 || len(listOf(out, "sensors")) != 0 || listOf(out, "omitted")[0]["reason"] != "sensor_outside_project" {
		t.Fatalf("summary: %d %v", code, out)
	}
}

// Daily on a Tuesday covers Monday; weekly on the next Monday covers that Monday to Sunday: both start on the same
// day, and switching the report from one to the other still sends the weekly run.
func TestReportScheduleSwitchDailyToWeekly(t *testing.T) {
	r := newScheduleRig(t)
	ctx := context.Background()
	ws := r.hookChannel("ws", nil)
	code, def := r.define(r.ownerToken, map[string]any{"schedule": "daily", "schedule_time": "00:00", "channels": []string{ws}})
	if code != 201 {
		t.Fatal(code, def)
	}
	id := def["id"].(string)
	loc := reports.Bangkok()
	r.exec(`UPDATE core.report_definitions SET next_run_at = $2 WHERE id=$1`, id, time.Date(2026, 10, 20, 0, 0, 0, 0, loc))
	r.worker(false).Tick(ctx, time.Date(2026, 10, 20, 0, 5, 0, 0, loc)) // Tuesday: covers Monday the 19th
	code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+id+"/update", "Bearer "+r.ownerToken, "", "", map[string]any{
		"name": "ห้องเย็น", "sensors": []map[string]string{reportSensor(r.gwA, r.fixedA)}, "schedule": "weekly", "schedule_weekday": 1, "schedule_time": "00:00", "channels": []string{ws}})
	if code != 200 {
		t.Fatal(code, out)
	}
	r.exec(`UPDATE core.report_definitions SET next_run_at = $2 WHERE id=$1`, id, time.Date(2026, 10, 26, 0, 0, 0, 0, loc))
	r.worker(false).Tick(ctx, time.Date(2026, 10, 26, 0, 5, 0, 0, loc)) // Monday: covers the 19th to the 25th
	rows, e := r.f.admin.Query(`SELECT period_from, period_to FROM core.report_runs WHERE report_id=$1 ORDER BY period_to`, id)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var periods []string
	for rows.Next() {
		var from, to time.Time
		if e := rows.Scan(&from, &to); e != nil {
			t.Fatal(e)
		}
		periods = append(periods, from.In(loc).Format("01-02")+".."+to.In(loc).Format("01-02"))
	}
	if strings.Join(periods, " ") != "10-19..10-20 10-19..10-26" {
		t.Fatalf("runs: %v", periods)
	}
}

// Saving a report without changing its schedule keeps next_run_at, even when it has passed and the scheduler has not
// picked the report up yet: that period is still sent. Changing the time moves it.
func TestReportEditKeepsDueRun(t *testing.T) {
	r := newScheduleRig(t)
	ctx := context.Background()
	ws := r.hookChannel("ws", nil)
	payload := map[string]any{"name": "ห้องเย็น", "sensors": []map[string]string{reportSensor(r.gwA, r.fixedA)}, "schedule": "daily", "schedule_time": "07:30", "channels": []string{ws}}
	_, def := r.define(r.ownerToken, payload)
	id := def["id"].(string)
	r.exec(`UPDATE core.report_definitions SET next_run_at = now() - interval '1 minute' WHERE id=$1`, id)
	payload["name"] = "ห้องเย็น (แก้ชื่อ)"
	code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+id+"/update", "Bearer "+r.ownerToken, "", "", payload)
	if next, _ := time.Parse(time.RFC3339Nano, out["next_run_at"].(string)); code != 200 || !next.Before(time.Now()) {
		t.Fatalf("an unchanged schedule moved: %d %v", code, out)
	}
	r.worker(false).Tick(ctx, time.Now())
	if n := r.count(`SELECT count(*) FROM core.report_runs WHERE report_id=$1`, id); n != 1 {
		t.Fatalf("the due period was skipped: %d runs", n)
	}
	payload["schedule_time"] = "08:00"
	_, out, _ = req(t, r.api, "POST", "/api/v1/reports/definitions/"+id+"/update", "Bearer "+r.ownerToken, "", "", payload)
	if next, _ := time.Parse(time.RFC3339Nano, out["next_run_at"].(string)); next.In(reports.Bangkok()).Format("15:04") != "08:00" || !next.After(time.Now()) {
		t.Fatalf("a changed time did not move next_run_at: %v", out)
	}
}

// One report that cannot be queued (here its stored channel list is corrupt) does not stop the others: it is retried
// in ten minutes, the rest are queued in the same pass.
func TestReportSchedulerIsolatesAFailingReport(t *testing.T) {
	r := newScheduleRig(t)
	ctx := context.Background()
	ws := r.hookChannel("ws", nil)
	_, good := r.define(r.ownerToken, map[string]any{"schedule": "daily", "schedule_time": "07:30", "channels": []string{ws}})
	_, bad := r.define(r.ownerToken, map[string]any{"name": "broken", "schedule": "daily", "schedule_time": "07:30", "channels": []string{ws}})
	r.exec(`UPDATE core.report_definitions SET next_run_at = now() - interval '2 minutes', channels = '["not a uuid"]' WHERE id=$1`, bad["id"])
	r.exec(`UPDATE core.report_definitions SET next_run_at = now() - interval '1 minute' WHERE id=$1`, good["id"])
	now := time.Now()
	r.worker(false).Tick(ctx, now)
	if n := r.count(`SELECT count(*) FROM core.report_runs WHERE report_id=$1`, good["id"]); n != 1 || r.hooks.Load() != 1 {
		t.Fatalf("the good report: %d runs, %d webhooks", n, r.hooks.Load())
	}
	var next time.Time
	if e := r.f.admin.QueryRow(`SELECT next_run_at FROM core.report_definitions WHERE id=$1`, bad["id"]).Scan(&next); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM core.report_runs WHERE report_id=$1`, bad["id"]); n != 0 || next.Sub(now) < 9*time.Minute || next.Sub(now) > 11*time.Minute {
		t.Fatalf("the broken report: %d runs, next in %s", n, next.Sub(now))
	}
}
