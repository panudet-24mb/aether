package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// projectAlertSite is one workspace with two projects, a gateway in each, one outside every project and the
// simulated B10 registered behind all three, so a press can be produced on any of them.
type projectAlertSite struct {
	owner              domain.Principal
	ownerToken         string
	projectA, projectB string
	gwA, gwB, gwNone   string
}

func newProjectAlertSite(t *testing.T, f *fixture) projectAlertSite {
	t.Helper()
	ctx := context.Background()
	_, auth, owner := f.account(t)
	s := projectAlertSite{owner: owner, ownerToken: auth.AccessToken}
	a, e := f.service.CreateProject(ctx, owner, "โครงการ A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	b, e := f.service.CreateProject(ctx, owner, "โครงการ B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	s.projectA, s.projectB = a.ID, b.ID
	gwA, _, e := f.service.CreateGatewayIn(ctx, owner, "GW A", "minew-mg3", &a.ID)
	if e != nil {
		t.Fatal(e)
	}
	gwB, _, e := f.service.CreateGatewayIn(ctx, owner, "GW B", "minew-mg3", &b.ID)
	if e != nil {
		t.Fatal(e)
	}
	gwNone, _, e := f.service.CreateGateway(ctx, owner, "GW ไม่มีโปรเจค", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	s.gwA, s.gwB, s.gwNone = gwA.ID, gwB.ID, gwNone.ID
	for _, gw := range []string{s.gwA, s.gwB, s.gwNone} {
		if _, e := f.service.CreateDevice(ctx, owner, gw, "ปุ่ม SOS", sosMAC, sosProfile); e != nil {
			t.Fatal(e)
		}
	}
	return s
}

// press produces one SOS press on a gateway: steps 0/2 (and 15/17 later) of the simulated B10.
func press(t *testing.T, f *fixture, tenant, gateway string, first int, at time.Time) {
	t.Helper()
	for i, step := range []int{first, first + 2} {
		if _, e := f.repo.CapturePacket(context.Background(), tenant, gateway, simulation.Packet(step, at.Add(time.Duration(i)*5*time.Second))); e != nil {
			t.Fatalf("uplink step %d on %s: %v", step, gateway, e)
		}
	}
}

type alertRow struct {
	gateway, event, severity string
	rule                     *string
}

func sosAlertsOn(t *testing.T, f *fixture, tenant string) []alertRow {
	t.Helper()
	rows, e := f.admin.Query(`SELECT gateway_id::text,event_type,severity,rule_id::text FROM core.alerts WHERE tenant_id=$1 AND external_id=$2 ORDER BY opened_at,id`, tenant, sosMAC)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	out := []alertRow{}
	for rows.Next() {
		var r alertRow
		if e := rows.Scan(&r.gateway, &r.event, &r.severity, &r.rule); e != nil {
			t.Fatal(e)
		}
		out = append(out, r)
	}
	return out
}

func queuedChannels(t *testing.T, f *fixture, tenant string) map[string]int {
	t.Helper()
	rows, e := f.admin.Query(`SELECT channel_id::text FROM core.notifications WHERE tenant_id=$1 AND channel_id IS NOT NULL`, tenant)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		if e := rows.Scan(&id); e != nil {
			t.Fatal(e)
		}
		out[id]++
	}
	return out
}

func createChannel(t *testing.T, f *fixture, token string, name string, project *string) (int, map[string]any) {
	t.Helper()
	payload := map[string]any{"name": name, "kind": "webhook", "config": map[string]string{"url": "https://hooks.example.test/" + name}}
	if project != nil {
		payload["project_id"] = *project
	}
	code, out, _ := req(t, busyAPI(f), "POST", "/api/v1/channels", "Bearer "+token, "", "", payload)
	return code, out
}

func createRule(t *testing.T, f *fixture, token, name string, project *string, channels []string) (int, map[string]any) {
	t.Helper()
	payload := map[string]any{"name": name, "event_type": "button", "severity": "critical", "channels": channels, "dedupe_sec": 30}
	if project != nil {
		payload["project_id"] = *project
	}
	code, out, _ := req(t, busyAPI(f), "POST", "/api/v1/rules", "Bearer "+token, "", "", payload)
	return code, out
}

// A project rule fires only for its project's devices, delivers only to channels it may use, and an SOS
// on a device no rule covers any more still opens a critical alert.
func TestProjectScopedRulesAndSOSFallback(t *testing.T) {
	f := setup(t)
	s := newProjectAlertSite(t, f)
	clearBuiltinRules(t, f, s.owner)

	code, out := createChannel(t, f, s.ownerToken, "hq", nil)
	if code != 201 || out["project_id"] != nil {
		t.Fatalf("workspace channel: %d %v", code, out)
	}
	channelW := out["id"].(string)
	code, out = createChannel(t, f, s.ownerToken, "ward-a", &s.projectA)
	if code != 201 || out["project_id"] != s.projectA {
		t.Fatalf("project A channel: %d %v", code, out)
	}
	channelA := out["id"].(string)
	code, out = createChannel(t, f, s.ownerToken, "ward-b", &s.projectB)
	if code != 201 {
		t.Fatalf("project B channel: %d %v", code, out)
	}
	channelB := out["id"].(string)

	// Which channels a rule may use: a workspace rule only workspace channels, a project rule also its own.
	if code, out := createRule(t, f, s.ownerToken, "ทั้งหมด → A", nil, []string{channelA}); code != 400 {
		t.Fatalf("a workspace rule reached a project channel: %d %v", code, out)
	}
	if code, out := createRule(t, f, s.ownerToken, "A → B", &s.projectA, []string{channelB}); code != 400 {
		t.Fatalf("a project A rule reached project B's channel: %d %v", code, out)
	}
	if code, out := createRule(t, f, s.ownerToken, "A → ?", ptr("8f7c2a8e-4f1d-4c2e-9a55-0b6d1c3e5f70"), nil); code != 400 {
		t.Fatalf("a rule on an unknown project: %d %v", code, out)
	}
	code, out = createRule(t, f, s.ownerToken, "SOS โครงการ A", &s.projectA, []string{channelA, channelW})
	if code != 201 || out["project_id"] != s.projectA {
		t.Fatalf("project A rule: %d %v", code, out)
	}
	ruleA := out["id"].(string)

	now := time.Now().UTC()
	press(t, f, s.owner.TenantID, s.gwA, 0, now)
	press(t, f, s.owner.TenantID, s.gwB, 0, now)
	got := sosAlertsOn(t, f, s.owner.TenantID)
	if len(got) != 2 {
		t.Fatalf("one alert per press expected: %+v", got)
	}
	byGateway := map[string]alertRow{}
	for _, a := range got {
		byGateway[a.gateway] = a
	}
	if a := byGateway[s.gwA]; a.rule == nil || *a.rule != ruleA || a.severity != "critical" {
		t.Fatalf("the press in project A must open the project rule's alert: %+v", a)
	}
	if a := byGateway[s.gwB]; a.rule != nil || a.severity != "critical" || a.event != domain.EventButton {
		t.Fatalf("an SOS in project B, which no rule covers, must still open a critical alert: %+v", a)
	}
	// The fallback keeps one open alert per tag, like the SOS rule; once acknowledged, the next press of
	// that tag (here heard by the gateway outside every project) is a new call for help.
	if _, e := f.admin.Exec(`UPDATE core.alerts SET status='acknowledged' WHERE tenant_id=$1 AND rule_id IS NULL`, s.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	press(t, f, s.owner.TenantID, s.gwNone, 0, now.Add(time.Minute))
	got = sosAlertsOn(t, f, s.owner.TenantID)
	if last := got[len(got)-1]; len(got) != 3 || last.gateway != s.gwNone || last.rule != nil || last.severity != "critical" {
		t.Fatalf("an SOS on a gateway outside every project must still open a critical alert: %+v", got)
	}
	queued := queuedChannels(t, f, s.owner.TenantID)
	if queued[channelA] != 1 || queued[channelW] != 1 || queued[channelB] != 0 || len(queued) != 2 {
		t.Fatalf("deliveries: %v (A=%s W=%s B=%s)", queued, channelA, channelW, channelB)
	}

	// A workspace-wide rule covers every project again: the next press in B belongs to it, no fallback.
	code, out = createRule(t, f, s.ownerToken, "SOS ทั้ง workspace", nil, []string{channelW})
	if code != 201 {
		t.Fatalf("workspace rule: %d %v", code, out)
	}
	ruleW := out["id"].(string)
	press(t, f, s.owner.TenantID, s.gwB, 15, now.Add(2*time.Minute))
	got = sosAlertsOn(t, f, s.owner.TenantID)
	last := got[len(got)-1]
	if len(got) != 4 || last.gateway != s.gwB || last.rule == nil || *last.rule != ruleW {
		t.Fatalf("the workspace rule must take the press in B: %+v", got)
	}
	// A delivery is re-checked when it is queued: edit the workspace rule's channels behind the API's back to
	// point at project B's channel and a press in A must not be delivered there.
	if _, e := f.admin.Exec(`UPDATE core.alert_rules SET channels=jsonb_build_array($1::text) WHERE id=$2`, channelB, ruleW); e != nil {
		t.Fatal(e)
	}
	press(t, f, s.owner.TenantID, s.gwA, 15, now.Add(4*time.Minute))
	if n := queuedChannels(t, f, s.owner.TenantID)[channelB]; n != 0 {
		t.Fatalf("project A's alert was queued to project B's channel %d times", n)
	}
}

// A workspace that never scoped a rule keeps today's behaviour: a disabled built-in SOS rule silences the
// button, the fallback does not bring it back, and existing rules stay workspace-wide.
func TestUnscopedWorkspaceIsUnchanged(t *testing.T) {
	f := setup(t)
	s := newProjectAlertSite(t, f)
	ctx := context.Background()
	rules, e := f.repo.ListRules(ctx, s.owner)
	if e != nil || len(rules) == 0 {
		t.Fatalf("rules: %+v %v", rules, e)
	}
	for _, r := range rules {
		if r.ProjectID != nil {
			t.Fatalf("a seeded rule must be workspace-wide: %+v", r)
		}
	}
	button := sosRule(t, f, s.owner)
	button.Enabled = false
	if e := f.repo.SaveRule(ctx, s.owner, button, false); e != nil {
		t.Fatal(e)
	}
	press(t, f, s.owner.TenantID, s.gwA, 0, time.Now().UTC())
	if got := sosAlertsOn(t, f, s.owner.TenantID); len(got) != 0 {
		t.Fatalf("a disabled workspace rule with no project rules must stay silent: %+v", got)
	}
}

// A member limited to project A: reads workspace rules read-only (device lists trimmed), manages only rules and
// channels of project A, never sees project B's, and cannot use workspace channels' credentials.
func TestRestrictedAdminAlertScope(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := newProjectAlertSite(t, f)
	api := busyAPI(f)
	clearBuiltinRules(t, f, s.owner)

	code, out := createChannel(t, f, s.ownerToken, "hq", nil)
	if code != 201 {
		t.Fatalf("workspace channel: %d %v", code, out)
	}
	channelW := out["id"].(string)
	code, out = createChannel(t, f, s.ownerToken, "ward-b", &s.projectB)
	if code != 201 {
		t.Fatalf("project B channel: %d %v", code, out)
	}
	channelB := out["id"].(string)
	// A workspace rule naming one device of each project, and a project B rule.
	payload := map[string]any{"name": "Tamper สองเครื่อง", "event_type": "tamper", "severity": "warning", "channels": []string{},
		"scope": map[string]any{"external_ids": []string{"f00000000001", "f00000000002"}}}
	if code, out, _ := req(t, api, "POST", "/api/v1/rules", "Bearer "+s.ownerToken, "", "", payload); code != 201 {
		t.Fatalf("workspace tamper rule: %d %v", code, out)
	}
	if _, e := f.service.CreateDevice(ctx, s.owner, s.gwA, "Dev A", "f00000000001", "generic-environment@1"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.service.CreateDevice(ctx, s.owner, s.gwB, "Dev B", "f00000000002", "generic-environment@1"); e != nil {
		t.Fatal(e)
	}
	code, out = createRule(t, f, s.ownerToken, "SOS B", &s.projectB, []string{channelB})
	if code != 201 {
		t.Fatalf("project B rule: %d %v", code, out)
	}
	ruleB := out["id"].(string)
	var workspaceRule string
	rules, _ := f.repo.ListRules(ctx, s.owner)
	for _, r := range rules {
		if r.ProjectID == nil {
			workspaceRule = r.ID
		}
	}

	email := memberEmail()
	addMember(t, api, s.ownerToken, email, "admin", []string{s.projectA})
	auth, admin := changeInitialPassword(t, f, api, email, s.owner.TenantID)
	token := auth.AccessToken

	// Reads: the workspace rule (with only project A's device left in its scope), never project B's rule.
	code, out = get(t, api, "/api/v1/rules", token)
	if code != 200 {
		t.Fatalf("rules: %d %v", code, out)
	}
	seen := rows(out)
	if len(seen) != 1 || seen[0]["id"] != workspaceRule || seen[0]["project_id"] != nil {
		t.Fatalf("a project A admin must see the workspace rule only: %v", seen)
	}
	scope, _ := seen[0]["scope"].(map[string]any)
	if ids, _ := scope["external_ids"].([]any); len(ids) != 1 || ids[0] != "f00000000001" {
		t.Fatalf("the workspace rule's device list must be trimmed to project A: %v", scope)
	}
	// Writes: workspace rows are read-only, project B does not exist for them, project A is theirs.
	if code, out := createRule(t, f, token, "ไม่ระบุโปรเจค", nil, nil); code != 403 {
		t.Fatalf("a project admin created a workspace rule: %d %v", code, out)
	}
	if code, out := createRule(t, f, token, "ของ B", &s.projectB, nil); code != 400 {
		t.Fatalf("a project A admin created a rule in project B: %d %v", code, out)
	}
	code, out = createRule(t, f, token, "ใช้ช่องทาง HQ", &s.projectA, []string{channelW})
	if code != 201 {
		t.Fatalf("a project rule may use a workspace channel: %d %v", code, out)
	}
	ruleHQ := out["id"].(string)
	// An update that leaves project_id out keeps the rule in its project.
	rename := map[string]any{"name": "ใช้ช่องทาง HQ (แก้)", "event_type": "button", "severity": "critical", "channels": []string{channelW}}
	if code, out, _ := req(t, api, "POST", "/api/v1/rules/"+ruleHQ+"/update", "Bearer "+token, "", "", rename); code != 200 || out["project_id"] != s.projectA {
		t.Fatalf("an update without project_id must keep the project: %d %v", code, out)
	}
	var kept string
	if e := f.admin.QueryRow(`SELECT project_id::text FROM core.alert_rules WHERE id=$1`, ruleHQ).Scan(&kept); e != nil || kept != s.projectA {
		t.Fatalf("stored project after update: %q %v", kept, e)
	}
	code, out = createRule(t, f, token, "SOS A", &s.projectA, nil)
	if code != 201 {
		t.Fatalf("project A rule by its admin: %d %v", code, out)
	}
	ruleA := out["id"].(string)
	update := map[string]any{"name": "แก้", "event_type": "tamper", "severity": "info", "channels": []string{}}
	if code, out, _ := req(t, api, "POST", "/api/v1/rules/"+workspaceRule+"/update", "Bearer "+token, "", "", update); code != 403 {
		t.Fatalf("a project admin edited a workspace rule: %d %v", code, out)
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/rules/"+workspaceRule+"/delete", "Bearer "+token, "", "", map[string]any{}); code != 403 {
		t.Fatalf("a project admin deleted a workspace rule: %d %v", code, out)
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/rules/"+ruleB+"/delete", "Bearer "+token, "", "", map[string]any{}); code != 404 {
		t.Fatalf("project B's rule must not exist for a project A admin: %d %v", code, out)
	}
	moveToB := map[string]any{"name": "SOS A", "event_type": "button", "severity": "critical", "channels": []string{}, "project_id": s.projectB}
	if code, out, _ := req(t, api, "POST", "/api/v1/rules/"+ruleA+"/update", "Bearer "+token, "", "", moveToB); code != 400 {
		t.Fatalf("a project A admin moved a rule into project B: %d %v", code, out)
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/rules/"+ruleA+"/delete", "Bearer "+token, "", "", map[string]any{}); code != 204 {
		t.Fatalf("a project A admin deletes their own rule: %d %v", code, out)
	}

	// Channels: the workspace channel is listed with its target blanked, project B's is absent.
	code, out = get(t, api, "/api/v1/channels", token)
	if code != 200 {
		t.Fatalf("channels: %d %v", code, out)
	}
	list := rows(out)
	if len(list) != 1 || list[0]["id"] != channelW {
		t.Fatalf("a project A admin must see only the workspace channel: %v", list)
	}
	if cfg, _ := list[0]["config"].(map[string]any); len(cfg) != 0 {
		t.Fatalf("the workspace channel's target leaked: %v", cfg)
	}
	if code, out := createChannel(t, f, token, "no-project", nil); code != 403 {
		t.Fatalf("a project admin created a workspace channel: %d %v", code, out)
	}
	code, out = createChannel(t, f, token, "ward-a", &s.projectA)
	if code != 201 {
		t.Fatalf("project A channel by its admin: %d %v", code, out)
	}
	for _, path := range []string{"/api/v1/channels/" + channelW + "/test", "/api/v1/channels/" + channelW + "/delete"} {
		if code, out, _ := req(t, api, "POST", path, "Bearer "+token, "", "", map[string]any{}); code != 403 {
			t.Fatalf("%s: %d %v", path, code, out)
		}
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/channels/"+channelW+"/update", "Bearer "+token, "", "", map[string]any{"enabled": false}); code != 403 {
		t.Fatalf("a project admin disabled a workspace channel: %d %v", code, out)
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/channels/"+channelB+"/delete", "Bearer "+token, "", "", map[string]any{}); code != 404 {
		t.Fatalf("project B's channel must not exist for a project A admin: %d %v", code, out)
	}

	// The database refuses the same thing without the API in front of it.
	tx, e := f.runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e := tx.Exec(`SELECT set_config('app.tenant_id',$1,true),set_config('app.user_id',$2,true),set_config('app.project_scope',$3,true)`, admin.TenantID, admin.UserID, s.projectA); e != nil {
		t.Fatal(e)
	}
	if _, e := tx.Exec(`SAVEPOINT s`); e != nil {
		t.Fatal(e)
	}
	_, e = tx.Exec(`INSERT INTO core.alert_rules(tenant_id,id,name,event_type,severity) VALUES($1,gen_random_uuid(),'raw','button','critical')`, admin.TenantID)
	if sqlState(e) != "42501" {
		t.Fatalf("a restricted session inserted a workspace rule: %v", e)
	}
	if _, e := tx.Exec(`ROLLBACK TO SAVEPOINT s`); e != nil {
		t.Fatal(e)
	}
	if _, e := tx.Exec(`SAVEPOINT s2`); e != nil {
		t.Fatal(e)
	}
	_, e = tx.Exec(`UPDATE core.alert_rules SET project_id=NULL WHERE id=$1`, ruleHQ)
	if sqlState(e) != "42501" {
		t.Fatalf("a restricted session moved its project rule to the whole workspace: %v", e)
	}
	if _, e := tx.Exec(`ROLLBACK TO SAVEPOINT s2`); e != nil {
		t.Fatal(e)
	}
	res, e := tx.Exec(`DELETE FROM core.alert_rules WHERE project_id IS NULL`)
	if e != nil {
		t.Fatal(e)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Fatalf("a restricted session deleted %d workspace rules", n)
	}
	var visibleB int
	if e := tx.QueryRow(`SELECT count(*) FROM core.alert_rules WHERE project_id=$1`, s.projectB).Scan(&visibleB); e != nil || visibleB != 0 {
		t.Fatalf("a restricted session read project B's rules: %d %v", visibleB, e)
	}

	// Another workspace sees none of it and cannot aim a rule at this workspace's project.
	_, otherAuth, other := f.account(t)
	otherRules, e := f.repo.ListRules(ctx, other)
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range otherRules {
		if r.ProjectID != nil {
			t.Fatalf("another workspace saw a project rule: %+v", r)
		}
	}
	if code, out := createRule(t, f, otherAuth.AccessToken, "ข้ามบริษัท", &s.projectA, nil); code != 400 {
		t.Fatalf("a rule on another workspace's project: %d %v", code, out)
	}
}

// A "members" email goes to the people who can see the alert's project, decided at delivery time.
func TestMembersEmailFollowsProject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := newProjectAlertSite(t, f)
	api := busyAPI(f)
	clearBuiltinRules(t, f, s.owner)

	add := func(role string, projects []string) string {
		email := memberEmail()
		addMember(t, api, s.ownerToken, email, role, projects)
		return email
	}
	adminA := add("admin", []string{s.projectA})
	operatorB := add("operator", []string{s.projectB})
	operatorAll := add("operator", []string{})
	viewerAll := add("viewer", []string{})
	muted := add("operator", []string{})
	code, out, _ := req(t, api, "POST", "/api/v1/members/"+memberID(t, api, s.ownerToken, muted)+"/access", "Bearer "+s.ownerToken, "", "", map[string]string{"alerts": "none"})
	if code != 204 {
		t.Fatalf("mute a member's alerts module: %d %v", code, out)
	}
	var ownerEmail string
	if e := f.admin.QueryRow(`SELECT email FROM identity.users WHERE id=$1`, s.owner.UserID).Scan(&ownerEmail); e != nil {
		t.Fatal(e)
	}

	// SMTP is not configured in tests, so the channels are stored directly; delivery is not attempted here.
	staff := domain.NotificationChannel{ID: "6b0f4a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b", Name: "staff", Kind: "email", Enabled: true, Config: map[string]string{"audience": "members"}, ProjectID: &s.projectA}
	everyone := domain.NotificationChannel{ID: "7c1a5b4f-2d3e-4f60-9bac-1d2e3f4a5b6c", Name: "everyone", Kind: "email", Enabled: true, Config: map[string]string{"audience": "members", "roles": "owner,admin,operator,viewer"}}
	for _, ch := range []domain.NotificationChannel{staff, everyone} {
		if e := f.repo.CreateChannel(ctx, s.owner, ch, ""); e != nil {
			t.Fatal(e)
		}
	}
	if code, out := createRule(t, f, s.ownerToken, "SOS A แจ้งทีม", &s.projectA, []string{staff.ID, everyone.ID}); code != 201 {
		t.Fatalf("rule: %d %v", code, out)
	}
	press(t, f, s.owner.TenantID, s.gwA, 0, time.Now().UTC())

	// Recipients are decided when the delivery is sent, not when the alert opened: after the press, one
	// unrestricted operator is limited to project B and one viewer's identity is erased.
	moved := memberEmail()
	addMember(t, api, s.ownerToken, moved, "operator", []string{})
	erased := memberEmail()
	addMember(t, api, s.ownerToken, erased, "viewer", []string{})
	press(t, f, s.owner.TenantID, s.gwA, 15, time.Now().UTC().Add(time.Minute))
	code, out, _ = req(t, api, "POST", "/api/v1/members/"+memberID(t, api, s.ownerToken, moved)+"/update", "Bearer "+s.ownerToken, "", "", map[string]any{"role": "operator", "project_ids": []string{s.projectB}})
	if code != 204 {
		t.Fatalf("limit a member to project B: %d %v", code, out)
	}
	if _, e := f.admin.Exec(`UPDATE identity.users SET erased_at=now() WHERE email=$1`, erased); e != nil {
		t.Fatal(e)
	}

	jobs, e := f.repo.ClaimNotifications(ctx, s.owner.TenantID, 10, time.Now().UTC())
	// The second press is inside the rule's dedupe window, so there is one alert and two deliveries.
	if e != nil || len(jobs) != 2 {
		t.Fatalf("claimed %d jobs: %v", len(jobs), e)
	}
	got := map[string][]string{}
	for _, job := range jobs {
		to := strings.Split(job.Channel.Config["to"], ",")
		sort.Strings(to)
		got[job.Channel.ID] = to
	}
	want := func(emails ...string) []string { sort.Strings(emails); return emails }
	if w := want(ownerEmail, adminA, operatorAll); strings.Join(got[staff.ID], ",") != strings.Join(w, ",") {
		t.Fatalf("staff recipients: %v, want %v (not %s, %s, %s, %s)", got[staff.ID], w, operatorB, viewerAll, muted, moved)
	}
	if w := want(ownerEmail, adminA, operatorAll, viewerAll); strings.Join(got[everyone.ID], ",") != strings.Join(w, ",") {
		t.Fatalf("everyone recipients: %v, want %v", got[everyone.ID], w)
	}

	// The lookup only answers for an alert of the current workspace.
	_, _, other := f.account(t)
	var alertID string
	if e := f.admin.QueryRow(`SELECT id::text FROM core.alerts WHERE tenant_id=$1 LIMIT 1`, s.owner.TenantID).Scan(&alertID); e != nil {
		t.Fatal(e)
	}
	tx, e := f.runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e := tx.Exec(`SELECT set_config('app.tenant_id',$1,true),set_config('app.project_scope','*',true)`, other.TenantID); e != nil {
		t.Fatal(e)
	}
	var leaked int
	if e := tx.QueryRow(`SELECT count(*) FROM core.alert_recipients($1::uuid, ARRAY['owner','admin','operator','viewer'])`, alertID).Scan(&leaked); e != nil || leaked != 0 {
		t.Fatalf("another workspace resolved %d recipients: %v", leaked, e)
	}
	// Nor for a project-limited session of this workspace: only the full-scope worker may ask.
	if _, e := tx.Exec(`SELECT set_config('app.tenant_id',$1,true),set_config('app.project_scope',$2,true)`, s.owner.TenantID, s.projectA); e != nil {
		t.Fatal(e)
	}
	if e := tx.QueryRow(`SELECT count(*) FROM core.alert_recipients($1::uuid, ARRAY['owner','admin','operator','viewer'])`, alertID).Scan(&leaked); e != nil || leaked != 0 {
		t.Fatalf("a project-limited session resolved %d recipients: %v", leaked, e)
	}
}

func ptr(s string) *string { return &s }

// Only a rule that really matches the device covers it. In shadow mode, a project rule whose device scope
// excludes the pressed tag (including one a project admin aimed at a device that does not exist) must not
// silence the press; a matching rule takes it with no second, fallback alert.
func TestSOSFallbackIgnoresRulesThatDoNotMatch(t *testing.T) {
	f := setup(t)
	f.repo.Configure(postgres.Options{AlertsShadow: true, DiscoveryLimit: 100})
	s := newProjectAlertSite(t, f)
	api := busyAPI(f)
	clearBuiltinRules(t, f, s.owner)

	email := memberEmail()
	addMember(t, api, s.ownerToken, email, "admin", []string{s.projectA})
	auth, _ := changeInitialPassword(t, f, api, email, s.owner.TenantID)
	decoy := map[string]any{"name": "SOS เฉพาะเครื่องที่ไม่มีอยู่", "event_type": "button", "severity": "critical", "channels": []string{},
		"project_id": s.projectA, "scope": map[string]any{"external_ids": []string{"f0000000dead"}}}
	if code, out, _ := req(t, api, "POST", "/api/v1/rules", "Bearer "+auth.AccessToken, "", "", decoy); code != 201 {
		t.Fatalf("decoy rule: %d %v", code, out)
	}
	now := time.Now().UTC()
	press(t, f, s.owner.TenantID, s.gwA, 0, now)
	got := sosAlertsOn(t, f, s.owner.TenantID)
	if len(got) != 1 || got[0].rule != nil || got[0].severity != "critical" || got[0].gateway != s.gwA {
		t.Fatalf("a project rule that excludes the tag must not silence its SOS: %+v", got)
	}

	// A rule that does match takes the next press (after the fallback alert is acknowledged): one alert, its own.
	code, out := createRule(t, f, s.ownerToken, "SOS อาคาร A ทุกเครื่อง", &s.projectA, nil)
	if code != 201 {
		t.Fatalf("matching rule: %d %v", code, out)
	}
	matching := out["id"].(string)
	if _, e := f.admin.Exec(`UPDATE core.alerts SET status='acknowledged' WHERE tenant_id=$1`, s.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	press(t, f, s.owner.TenantID, s.gwA, 15, now.Add(time.Minute))
	got = sosAlertsOn(t, f, s.owner.TenantID)
	if len(got) != 2 || got[1].rule == nil || *got[1].rule != matching {
		t.Fatalf("the matching rule must take the press, with no fallback beside it: %+v", got)
	}
	// Inside that rule's dedupe window the press is held back, and it still counts as covered: no fallback.
	press(t, f, s.owner.TenantID, s.gwA, 60, now.Add(time.Minute+10*time.Second))
	if got = sosAlertsOn(t, f, s.owner.TenantID); len(got) != 2 {
		t.Fatalf("a deduplicated press must not fall back to a second alert: %+v", got)
	}
}

// The same guarantee for smoke / gas / CO, through the Zigbee path and in shadow mode.
func TestHazardFallbackUnderProjectScoping(t *testing.T) {
	r := newZigbeeRig(t, true)
	ctx := context.Background()
	clearBuiltinRules(t, r.f, r.owner)
	project, e := r.f.service.CreateProject(ctx, r.owner, "อาคาร Z", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.admin.Exec(`UPDATE core.gateways SET project_id=$1 WHERE id=$2`, project.ID, r.g.ID); e != nil {
		t.Fatal(e)
	}
	smoke := sensor("JTYJ-GD-01LM/BW")
	r.register(smoke)
	rule := domain.AlertRule{ID: "4a1c2b3d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", Name: "ควันเฉพาะห้องอื่น", Enabled: true, EventType: domain.EventHazard, Severity: "warning",
		Scope: domain.RuleScope{ExternalIDs: []string{"0x00158d0000000000"}}, Channels: []string{}, DedupeSec: 300, ProjectID: &project.ID}
	if e := r.f.repo.SaveRule(ctx, r.owner, rule, true); e != nil {
		t.Fatal(e)
	}
	r.publish(r.bridge.Set(smoke.IEEE, "smoke", true))
	if n := r.count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id=$2 AND event_type=$3 AND rule_id IS NULL AND severity='critical'`, r.g.ID, smoke.IEEE, domain.EventHazard); n != 1 {
		t.Fatalf("a project hazard rule that excludes the detector must not silence it: %d fallback alerts", n)
	}
}

// The caps count the whole workspace, not what a project-limited admin can see.
func TestAlertCapsCountTheWholeWorkspace(t *testing.T) {
	f := setup(t)
	s := newProjectAlertSite(t, f)
	api := busyAPI(f)
	email := memberEmail()
	addMember(t, api, s.ownerToken, email, "admin", []string{s.projectA})
	auth, _ := changeInitialPassword(t, f, api, email, s.owner.TenantID)

	// Fill the workspace to 100 rules and 20 channels with rows of project B, which this admin cannot see.
	if _, e := f.admin.Exec(`INSERT INTO core.alert_rules(tenant_id,id,name,event_type,severity,project_id)
    SELECT $1, gen_random_uuid(), 'B-'||n, 'tamper', 'info', $2 FROM generate_series(1, 100 - (SELECT count(*) FROM core.alert_rules WHERE tenant_id=$1)::int) n`, s.owner.TenantID, s.projectB); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.Exec(`INSERT INTO core.notification_channels(tenant_id,id,name,kind,config,project_id)
    SELECT $1, gen_random_uuid(), 'B-'||n, 'webhook', '{"url":"https://hooks.example.test/b"}'::jsonb, $2 FROM generate_series(1, 20) n`, s.owner.TenantID, s.projectB); e != nil {
		t.Fatal(e)
	}
	code, out := get(t, api, "/api/v1/rules", auth.AccessToken)
	if code != 200 || len(rows(out)) >= 100 {
		t.Fatalf("the admin should see far fewer than 100 rules: %d %d", code, len(rows(out)))
	}
	if code, out := createRule(t, f, auth.AccessToken, "เกินโควตา", &s.projectA, nil); code != 409 {
		t.Fatalf("a project admin created rule 101: %d %v", code, out)
	}
	if code, out := createChannel(t, f, auth.AccessToken, "over", &s.projectA); code != 409 {
		t.Fatalf("a project admin created channel 21: %d %v", code, out)
	}

	// Over the cap anyway (direct SQL), evaluation still loads the SOS rule first.
	if _, e := f.admin.Exec(`INSERT INTO core.alert_rules(tenant_id,id,name,event_type,severity,project_id,created_at)
    SELECT $1, gen_random_uuid(), 'extra-'||n, 'tamper', 'info', $2, now() - interval '1 day' FROM generate_series(1, 5) n`, s.owner.TenantID, s.projectB); e != nil {
		t.Fatal(e)
	}
	press(t, f, s.owner.TenantID, s.gwA, 0, time.Now().UTC())
	if got := sosAlertsOn(t, f, s.owner.TenantID); len(got) != 1 || got[0].rule == nil {
		t.Fatalf("the built-in SOS rule must survive a workspace over the cap: %+v", got)
	}
}

// action.notify obeys the same channel rule as alert rules, when a flow is saved and when it runs.
func TestAutomationNotifyRespectsChannelProject(t *testing.T) {
	f := setup(t)
	s := newProjectAlertSite(t, f)
	api := busyAPI(f)
	clearBuiltinRules(t, f, s.owner)
	_, out := createChannel(t, f, s.ownerToken, "ward-a", &s.projectA)
	channelA := out["id"].(string)
	_, out = createChannel(t, f, s.ownerToken, "ward-b", &s.projectB)
	channelB := out["id"].(string)
	_, out = createChannel(t, f, s.ownerToken, "hq", nil)
	channelW := out["id"].(string)

	def := func(channels ...string) map[string]any {
		return flow([]map[string]any{
			block("t1", "trigger.event", 0, 0, map[string]any{"event_types": []string{"button"}}),
			block("n1", "action.notify", 300, 0, map[string]any{"channel_ids": channels, "message": "{{device}}"}),
		}, []map[string]any{link("e1", "t1", "n1")})
	}
	create := func(name string, project *string, channels ...string) (int, map[string]any) {
		payload := map[string]any{"name": name, "enabled": true, "definition": def(channels...)}
		if project != nil {
			payload["project_id"] = *project
		}
		code, out, _ := req(t, api, "POST", "/api/v1/automations", "Bearer "+s.ownerToken, "", "", payload)
		return code, out
	}
	if code, out := create("ทั้ง workspace → A", nil, channelA); code != 400 {
		t.Fatalf("a workspace flow saved with a project channel: %d %v", code, out)
	}
	if code, out := create("A → B", &s.projectA, channelB); code != 400 {
		t.Fatalf("a project A flow saved with project B's channel: %d %v", code, out)
	}
	code, out := create("A → A + HQ", &s.projectA, channelA, channelW)
	if code != 201 {
		t.Fatalf("project A flow: %d %v", code, out)
	}
	flowA := out["id"].(string)
	code, out = create("ทั้ง workspace → HQ", nil, channelW)
	if code != 201 {
		t.Fatalf("workspace flow: %d %v", code, out)
	}
	flowW := out["id"].(string)

	// Behind the API's back, both flows point at channels they may not use; at run time those are skipped.
	if _, e := f.admin.Exec(`UPDATE core.automations SET definition=$1::jsonb WHERE id=$2`, mustJSON(t, def(channelA, channelB)), flowA); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.Exec(`UPDATE core.automations SET definition=$1::jsonb WHERE id=$2`, mustJSON(t, def(channelA, channelW)), flowW); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	press(t, f, s.owner.TenantID, s.gwA, 0, now)
	// The same tag is registered behind both gateways, and a flow fires once a minute per tag: forget the
	// runs so the press in B is evaluated too.
	if _, e := f.admin.Exec(`DELETE FROM core.automation_runs WHERE tenant_id=$1`, s.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	press(t, f, s.owner.TenantID, s.gwB, 0, now)
	queued := queuedChannels(t, f, s.owner.TenantID)
	// Press in A: flow A → ward-a (its project); workspace flow → ward-a (the gateway's project) and hq.
	// Press in B: workspace flow → hq only; ward-a is neither the flow's project nor the gateway's.
	if queued[channelB] != 0 || queued[channelA] != 2 || queued[channelW] != 2 {
		t.Fatalf("deliveries: A=%d B=%d HQ=%d", queued[channelA], queued[channelB], queued[channelW])
	}

	// At send time the gateway's current project decides: move GW A to project B and ward-a's deliveries fail.
	if _, e := f.admin.Exec(`UPDATE core.gateways SET project_id=$1 WHERE id=$2`, s.projectB, s.gwA); e != nil {
		t.Fatal(e)
	}
	jobs, e := f.repo.ClaimNotifications(context.Background(), s.owner.TenantID, 20, time.Now().UTC())
	if e != nil {
		t.Fatal(e)
	}
	failed := 0
	for _, job := range jobs {
		if job.Channel.ID == channelA {
			if job.FailReason == "" {
				t.Fatalf("a delivery to ward-a for a gateway now in project B was not failed: %+v", job.Alert)
			}
			failed++
		} else if job.FailReason != "" {
			t.Fatalf("an unrelated delivery failed: %s %s", job.Channel.Name, job.FailReason)
		}
	}
	if failed != 2 {
		t.Fatalf("ward-a deliveries failed at send time: %d", failed)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

// A members email for a gateway with no project reaches only owners and unrestricted members, owners first,
// capped at 50.
func TestMembersEmailCapOrderAndNoProjectGateway(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := newProjectAlertSite(t, f)
	api := busyAPI(f)
	clearBuiltinRules(t, f, s.owner)
	restricted := memberEmail()
	addMember(t, api, s.ownerToken, restricted, "admin", []string{s.projectA})
	// 30 operators and 30 viewers straight into the directory, so the list is longer than the cap.
	if _, e := f.admin.Exec(`WITH people AS (
      SELECT gen_random_uuid() AS id, CASE WHEN n <= 30 THEN 'op' ELSE 'vw' END AS kind, n FROM generate_series(1, 60) n
    ), users AS (
      INSERT INTO identity.users(id,email,password_hash,name) SELECT id, kind||'-'||lpad(n::text,2,'0')||'-'||replace($1::text,'-','')||'@bulk.test', '!', kind FROM people RETURNING id, email
    ) INSERT INTO core.memberships(tenant_id,user_id,role)
      SELECT $1::uuid, u.id, CASE WHEN u.email LIKE 'op-%' THEN 'operator' ELSE 'viewer' END FROM users u`, s.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	var ownerEmail string
	if e := f.admin.QueryRow(`SELECT email FROM identity.users WHERE id=$1`, s.owner.UserID).Scan(&ownerEmail); e != nil {
		t.Fatal(e)
	}
	everyone := domain.NotificationChannel{ID: "8d2b6c5a-3e4f-4a71-8bcd-2e3f4a5b6c7d", Name: "everyone", Kind: "email", Enabled: true, Config: map[string]string{"audience": "members", "roles": "viewer,operator,admin,owner"}}
	if e := f.repo.CreateChannel(ctx, s.owner, everyone, ""); e != nil {
		t.Fatal(e)
	}
	if code, out := createRule(t, f, s.ownerToken, "SOS ทั้ง workspace", nil, []string{everyone.ID}); code != 201 {
		t.Fatalf("rule: %d %v", code, out)
	}
	press(t, f, s.owner.TenantID, s.gwNone, 0, time.Now().UTC())
	jobs, e := f.repo.ClaimNotifications(ctx, s.owner.TenantID, 10, time.Now().UTC())
	if e != nil || len(jobs) != 1 || jobs[0].FailReason != "" {
		t.Fatalf("claim: %+v %v", jobs, e)
	}
	to := strings.Split(jobs[0].Channel.Config["to"], ",")
	if len(to) != 50 || to[0] != ownerEmail {
		t.Fatalf("the list must be capped at 50 with the owner first: %d %v", len(to), to[:2])
	}
	for i, addr := range to {
		if addr == restricted {
			t.Fatal("a member limited to project A received an alert from a gateway with no project")
		}
		if i > 0 && i <= 30 && !strings.HasPrefix(addr, "op-"+twoDigits(i)+"-") {
			t.Fatalf("operators must follow the owner in email order: %d %s", i, addr)
		}
		if i > 30 && !strings.HasPrefix(addr, "vw-") {
			t.Fatalf("viewers come last: %d %s", i, addr)
		}
	}
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// Whether an uncovered SOS falls back depends only on workspace-wide rules, which only full-scope members
// manage: a project admin can neither silence other projects nor undo the owner's decision to silence.
func TestProjectAdminCannotToggleTheFallback(t *testing.T) {
	f := setup(t)
	s := newProjectAlertSite(t, f)
	api := busyAPI(f)
	ctx := context.Background()
	clearBuiltinRules(t, f, s.owner) // no workspace-wide SOS rule at all: an uncovered press falls back
	email := memberEmail()
	addMember(t, api, s.ownerToken, email, "admin", []string{s.projectA})
	auth, admin := changeInitialPassword(t, f, api, email, s.owner.TenantID)
	code, out := createRule(t, f, auth.AccessToken, "SOS A", &s.projectA, nil)
	if code != 201 {
		t.Fatalf("project A rule: %d %v", code, out)
	}
	ruleA := out["id"].(string)
	fallbacks := func(gateway string) int {
		var n int
		if e := f.admin.QueryRow(`SELECT count(*) FROM core.alerts WHERE tenant_id=$1 AND gateway_id=$2 AND fallback AND severity='critical'`, s.owner.TenantID, gateway).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	ackAll := func() {
		if _, e := f.admin.Exec(`UPDATE core.alerts SET status='acknowledged' WHERE tenant_id=$1`, s.owner.TenantID); e != nil {
			t.Fatal(e)
		}
	}
	now := time.Now().UTC()
	// A's admin disables A's rule, then deletes it: B and the gateway outside every project keep falling back.
	rules, _ := f.repo.ListRules(ctx, admin)
	for _, r := range rules {
		if r.ID == ruleA {
			r.Enabled = false
			if e := f.repo.SaveRule(ctx, admin, r, false); e != nil {
				t.Fatal(e)
			}
		}
	}
	press(t, f, s.owner.TenantID, s.gwB, 0, now)
	ackAll() // one open fallback per tag: acknowledge it so the next gateway's press is visible on its own
	press(t, f, s.owner.TenantID, s.gwNone, 15, now.Add(time.Minute))
	if b, none := fallbacks(s.gwB), fallbacks(s.gwNone); b != 1 || none != 1 {
		t.Fatalf("disabling A's rule silenced other projects: B=%d none=%d", b, none)
	}
	ackAll()
	if code, out, _ := req(t, api, "POST", "/api/v1/rules/"+ruleA+"/delete", "Bearer "+auth.AccessToken, "", "", map[string]any{}); code != 204 {
		t.Fatalf("delete A's rule: %d %v", code, out)
	}
	press(t, f, s.owner.TenantID, s.gwB, 60, now.Add(2*time.Minute))
	if b := fallbacks(s.gwB); b != 2 {
		t.Fatalf("deleting A's rule silenced project B: %d", b)
	}

	// The owner silences SOS for the workspace (a workspace-wide rule, disabled). A new project A rule does
	// not undo that for B; it only covers A.
	code, out = createRule(t, f, s.ownerToken, "SOS ทั้ง workspace (ปิด)", nil, nil)
	if code != 201 {
		t.Fatalf("workspace rule: %d %v", code, out)
	}
	rules, _ = f.repo.ListRules(ctx, s.owner)
	for _, r := range rules {
		if r.ProjectID == nil && r.EventType == domain.EventButton {
			r.Enabled = false
			if e := f.repo.SaveRule(ctx, s.owner, r, false); e != nil {
				t.Fatal(e)
			}
		}
	}
	code, out = createRule(t, f, auth.AccessToken, "SOS A ใหม่", &s.projectA, nil)
	if code != 201 {
		t.Fatalf("new project A rule: %d %v", code, out)
	}
	newA := out["id"].(string)
	ackAll()
	before := len(sosAlertsOn(t, f, s.owner.TenantID))
	press(t, f, s.owner.TenantID, s.gwB, 0, now.Add(4*time.Minute))
	press(t, f, s.owner.TenantID, s.gwA, 15, now.Add(5*time.Minute))
	got := sosAlertsOn(t, f, s.owner.TenantID)[before:]
	if len(got) != 1 || got[0].gateway != s.gwA || got[0].rule == nil || *got[0].rule != newA {
		t.Fatalf("the owner's silence must hold for B while A's rule covers A: %+v", got)
	}
}

// An open automation alert (no rule either) must not hold back the next SOS fallback.
func TestFlowAlertDoesNotHoldBackTheFallback(t *testing.T) {
	f := setup(t)
	s := newProjectAlertSite(t, f)
	api := busyAPI(f)
	clearBuiltinRules(t, f, s.owner)
	code, out, _ := req(t, api, "POST", "/api/v1/automations", "Bearer "+s.ownerToken, "", "", map[string]any{
		"name": "บันทึกการกดปุ่ม", "enabled": true,
		"definition": flow([]map[string]any{
			block("t1", "trigger.event", 0, 0, map[string]any{"event_types": []string{"button"}}),
			block("a1", "action.alert", 300, 0, map[string]any{"severity": "info", "title": "มีการกดปุ่ม {{device}}"}),
		}, []map[string]any{link("e1", "t1", "a1")}),
	})
	if code != 201 {
		t.Fatalf("flow: %d %v", code, out)
	}
	count := func(where string) int {
		var n int
		if e := f.admin.QueryRow(`SELECT count(*) FROM core.alerts WHERE tenant_id=$1 AND external_id=$2 AND `+where, s.owner.TenantID, sosMAC).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	now := time.Now().UTC()
	press(t, f, s.owner.TenantID, s.gwA, 0, now)
	if fb, flowAlerts := count(`fallback AND severity='critical'`), count(`NOT fallback AND rule_id IS NULL AND status='open'`); fb != 1 || flowAlerts != 1 {
		t.Fatalf("a press must open the fallback and the flow's alert: fallback=%d flow=%d", fb, flowAlerts)
	}
	// Acknowledge the fallback only; the flow's info alert stays open.
	if _, e := f.admin.Exec(`UPDATE core.alerts SET status='acknowledged' WHERE tenant_id=$1 AND fallback`, s.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	press(t, f, s.owner.TenantID, s.gwA, 15, now.Add(2*time.Minute))
	if fb := count(`fallback AND severity='critical' AND status='open'`); fb != 1 {
		t.Fatalf("the next press must open a new critical fallback beside the open flow alert: %d", fb)
	}
}
