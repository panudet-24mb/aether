package tests

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aether/backend/internal/adapters/httpapi"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Personal data (migrations 00038 and 00039, docs/platform/privacy.md): the read-access log, exports and erasure.

// raw sends a JSON request and returns the status, the raw body and the Content-Disposition header.
func raw(t *testing.T, api *fiber.App, method, path, token string, payload any) (int, []byte, string) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, e := json.Marshal(payload)
		if e != nil {
			t.Fatal(e)
		}
		body = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	res, e := api.Test(r, fiber.TestConfig{Timeout: 30 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	out, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return res.StatusCode, out, res.Header.Get("Content-Disposition")
}

func accessRows(t *testing.T, f *fixture, tenant, actor, resource string) int {
	t.Helper()
	return count(t, f.admin, `SELECT count(*) FROM core.access_log WHERE tenant_id=$1 AND actor_id=$2 AND resource=$3`, tenant, actor, resource)
}

// Reads of personal data leave one row per actor, resource and subject per 10 minutes; only an owner reads
// the trail, nobody rewrites it, and another workspace sees none of it.
func TestAccessLogIsWrittenAndOwnerOnly(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	a, ownerAuth, owner := f.account(t)
	_, otherAuth, other := f.account(t)
	email := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, email, "viewer", nil)
	viewerAuth, viewer := changeInitialPassword(t, f, api, email, a.TenantID)

	if code, _ := get(t, api, "/api/v1/members", ownerAuth.AccessToken); code != 200 {
		t.Fatal(code)
	}
	if code, _ := get(t, api, "/api/v1/members", ownerAuth.AccessToken); code != 200 {
		t.Fatal(code)
	}
	if n := accessRows(t, f, owner.TenantID, owner.UserID, "members"); n != 1 {
		t.Fatalf("two member-list reads within the window: %d rows", n)
	}
	if code, _ := get(t, api, "/api/v1/alerts", viewerAuth.AccessToken); code != 200 {
		t.Fatal(code)
	}
	if n := accessRows(t, f, owner.TenantID, viewer.UserID, "alerts"); n != 1 {
		t.Fatalf("the viewer's alert read: %d rows", n)
	}
	if code, _ := get(t, api, "/api/v1/events?external_id=c30000393fe5", viewerAuth.AccessToken); code != 200 {
		t.Fatal(code)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.access_log WHERE actor_id=$1 AND resource='events' AND subject_kind='device_identity' AND subject_id='c30000393fe5'`, viewer.UserID); n != 1 {
		t.Fatalf("an event read about one tag names the tag: %d", n)
	}
	// Device lists, one device's state and commands for one device are reads too; the subject is narrowed to the
	// device the request names.
	device := uuid.NewString()
	for path, want := range map[string][3]string{
		"/api/v1/devices":                      {"devices", "device_list", "*"},
		"/api/v1/devices/" + device + "/state": {"device_state", "device", device},
		"/api/v1/commands?device_id=" + device: {"commands", "device", device},
		"/api/v1/notifications":                {"notifications", "alert_list", "*"},
		"/api/v1/discovery":                    {"discovery", "device_list", "*"},
	} {
		code, _ := get(t, api, path, ownerAuth.AccessToken)
		if code >= 400 && code != 404 {
			t.Fatalf("%s: %d", path, code)
		}
		if code == 404 {
			continue // a device that does not exist is not a read
		}
		if n := count(t, f.admin, `SELECT count(*) FROM core.access_log WHERE actor_id=$1 AND resource=$2 AND subject_kind=$3 AND subject_id=$4`, owner.UserID, want[0], want[1], want[2]); n != 1 {
			t.Fatalf("%s: logged %d times as %v", path, n, want)
		}
	}
	// A request that fails is not a read.
	if code, _ := get(t, api, "/api/v1/members", viewerAuth.AccessToken); code != 403 {
		t.Fatal(code)
	}
	if n := accessRows(t, f, owner.TenantID, viewer.UserID, "members"); n != 0 {
		t.Fatalf("a refused request was logged: %d", n)
	}

	// The owner's view, filtered; reading the trail is itself logged.
	code, out := get(t, api, "/api/v1/privacy/access-log?resource=alerts&limit=10", ownerAuth.AccessToken)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["actor_id"] != viewer.UserID || items[0].(map[string]any)["actor_name"] == nil {
		t.Fatalf("owner access log: %d %v", code, out)
	}
	if n := accessRows(t, f, owner.TenantID, owner.UserID, "access_log"); n != 1 {
		t.Fatalf("reading the trail: %d rows", n)
	}
	code, out = get(t, api, "/api/v1/privacy/audit?action=session&limit=100", ownerAuth.AccessToken)
	if items, _ := out["items"].([]any); code != 200 || len(items) == 0 {
		t.Fatalf("owner audit view: %d %v", code, out)
	}
	for _, path := range []string{"/api/v1/privacy/access-log", "/api/v1/privacy/audit", "/api/v1/privacy/erasures"} {
		if code, _ := get(t, api, path, viewerAuth.AccessToken); code != 403 {
			t.Fatalf("viewer reached %s: %d", path, code)
		}
	}
	if code, out := get(t, api, "/api/v1/privacy/access-log?before_id=nope", ownerAuth.AccessToken); code != 400 {
		t.Fatalf("bad cursor: %d %v", code, out)
	}
	// Paging: the cursor of a full page leads to the rest, without repeats.
	code, out = get(t, api, "/api/v1/privacy/access-log?limit=1", ownerAuth.AccessToken)
	next, _ := out["next"].(map[string]any)
	if code != 200 || next == nil {
		t.Fatalf("first page: %d %v", code, out)
	}
	first := out["items"].([]any)[0].(map[string]any)["id"]
	code, out = get(t, api, "/api/v1/privacy/access-log?limit=1&before_at="+strings.ReplaceAll(next["before_at"].(string), "+", "%2B")+"&before_id="+next["before_id"].(string), ownerAuth.AccessToken)
	if items, _ := out["items"].([]any); code != 200 || len(items) != 1 || items[0].(map[string]any)["id"] == first {
		t.Fatalf("second page: %d %v", code, out)
	}

	// Another workspace's owner sees nothing of this one.
	code, out = get(t, api, "/api/v1/privacy/access-log?limit=500", otherAuth.AccessToken)
	for _, item := range out["items"].([]any) {
		if item.(map[string]any)["actor_id"] != other.UserID {
			t.Fatalf("another workspace's row leaked: %v", item)
		}
	}

	// Database level: only an owner reads, the runtime may only append rows about itself, never rewrite.
	tx := asRuntime(t, f, viewer.UserID, viewer.TenantID)
	if n := count(t, tx, `SELECT count(*) FROM core.access_log`); n != 0 {
		t.Fatalf("a viewer reads %d trail rows", n)
	}
	tx.Rollback()
	for _, statement := range []string{
		// A row naming somebody else as the reader, written by the viewer.
		`INSERT INTO core.access_log(tenant_id,id,actor_id,resource,subject_kind,subject_id) VALUES(core.tenant_id(),gen_random_uuid(),'` + owner.UserID + `','x','x','x')`,
		`UPDATE core.access_log SET resource='x'`,
		`DELETE FROM core.access_log`,
		`SELECT count(*) FROM core.access_log_default`,
	} {
		tx := asRuntime(t, f, viewer.UserID, viewer.TenantID)
		if _, e := tx.Exec(statement); sqlState(e) != "42501" {
			t.Fatalf("%s: %v", statement, e)
		}
		tx.Rollback()
		tx = asRuntime(t, f, owner.UserID, owner.TenantID)
		if _, e := tx.Exec(strings.Replace(statement, owner.UserID, viewer.UserID, 1)); sqlState(e) != "42501" {
			t.Fatalf("owner: %s: %v", statement, e)
		}
		tx.Rollback()
	}
	// Somebody who is not a member of the workspace cannot append to its trail, even about themselves.
	tx = asRuntime(t, f, other.UserID, owner.TenantID)
	if _, e := tx.Exec(`INSERT INTO core.access_log(tenant_id,id,actor_id,resource,subject_kind,subject_id) VALUES(core.tenant_id(),gen_random_uuid(),identity.user_id(),'x','x','x')`); sqlState(e) != "42501" {
		t.Fatalf("a non-member appended to the trail: %v", e)
	}
	tx.Rollback()
	tx = asRuntime(t, f, owner.UserID, owner.TenantID)
	if n := count(t, tx, `SELECT count(*) FROM core.access_log`); n < 3 {
		t.Fatalf("the owner reads %d rows", n)
	}
	tx.Rollback()
}

// When the trail cannot be written the data does not leave: 503 and no body.
func TestFailedAccessLogWithholdsTheResponse(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	_, auth, _ := f.account(t)
	if _, e := f.admin.Exec(`REVOKE INSERT ON core.access_log FROM aether_app`); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.admin.Exec(`GRANT INSERT ON core.access_log TO aether_app`) })
	code, body, _ := raw(t, api, "GET", "/api/v1/members", auth.AccessToken, nil)
	if code != 503 || strings.Contains(string(body), "@example.test") || !strings.Contains(string(body), "unavailable") {
		t.Fatalf("members without a log row: %d %s", code, body)
	}
	code, body, disposition := raw(t, api, "GET", "/api/v1/me/export", auth.AccessToken, nil)
	if code != 503 || disposition != "" || strings.Contains(string(body), "@example.test") {
		t.Fatalf("export without a log row: %d %q %s", code, disposition, body)
	}
	if _, e := f.admin.Exec(`GRANT INSERT ON core.access_log TO aether_app`); e != nil {
		t.Fatal(e)
	}
	if code, _, _ := raw(t, api, "GET", "/api/v1/members", auth.AccessToken, nil); code != 200 {
		t.Fatalf("after the grant is back: %d", code)
	}
}

// A member exports their own data; an owner exports and erases a member. The erased member cannot sign in,
// keeps their uuid in the security record, and another workspace is untouched.
func TestMemberExportAndErasure(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	ctx := context.Background()
	a, ownerAuth, owner := f.account(t)
	_, otherAuth, other := f.account(t)
	ownerSelf, e := f.repo.MemberSelf(ctx, owner)
	if e != nil {
		t.Fatal(e)
	}
	email := memberEmail()
	memberID := addMember(t, api, ownerAuth.AccessToken, email, "operator", nil)
	memberAuth, member := changeInitialPassword(t, f, api, email, a.TenantID)
	adminEmail := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, adminEmail, "admin", nil)
	adminAuth, _ := changeInitialPassword(t, f, api, adminEmail, a.TenantID)

	// Self export: the caller's own data and nobody else's.
	code, body, disposition := raw(t, api, "GET", "/api/v1/me/export", memberAuth.AccessToken, nil)
	if code != 200 || !strings.Contains(disposition, "attachment") {
		t.Fatalf("self export: %d %q %s", code, disposition, body)
	}
	var export map[string]any
	if e := json.Unmarshal(body, &export); e != nil {
		t.Fatal(e)
	}
	if export["profile"].(map[string]any)["email"] != email || export["membership"].(map[string]any)["role"] != "operator" {
		t.Fatalf("self export content: %s", body)
	}
	if strings.Contains(string(body), ownerSelf.Email) || strings.Contains(string(body), adminEmail) {
		t.Fatalf("self export names somebody else: %s", body)
	}
	if sessions, _ := export["sessions"].([]any); len(sessions) == 0 {
		t.Fatalf("self export lacks sessions: %s", body)
	}
	if strings.Contains(string(body), "hash") {
		t.Fatalf("token or password material in the export: %s", body)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE actor_id=$1 AND action='privacy.self_exported'`, member.UserID); n != 1 {
		t.Fatalf("self export audited %d times", n)
	}
	if n := accessRows(t, f, a.TenantID, member.UserID, "export"); n != 1 {
		t.Fatalf("self export logged %d times", n)
	}

	// Only an owner of this workspace exports or erases somebody else.
	for _, tc := range []struct {
		token, path string
		want        int
	}{
		{adminAuth.AccessToken, "/api/v1/members/" + memberID + "/export", 403},
		{adminAuth.AccessToken, "/api/v1/members/" + memberID + "/erase", 403},
		{memberAuth.AccessToken, "/api/v1/members/" + memberID + "/erase", 403},
		{otherAuth.AccessToken, "/api/v1/members/" + memberID + "/export", 404},
		{otherAuth.AccessToken, "/api/v1/members/" + memberID + "/erase", 404},
		{ownerAuth.AccessToken, "/api/v1/members/" + owner.UserID + "/erase", 403},
		{ownerAuth.AccessToken, "/api/v1/members/not-a-uuid/erase", 400},
	} {
		if code, body, _ := raw(t, api, "POST", tc.path, tc.token, map[string]any{}); code != tc.want {
			t.Fatalf("%s: %d %s, want %d", tc.path, code, body, tc.want)
		}
	}
	code, body, _ = raw(t, api, "POST", "/api/v1/members/"+memberID+"/export", ownerAuth.AccessToken, map[string]any{})
	if code != 200 || !strings.Contains(string(body), email) {
		t.Fatalf("owner export: %d %s", code, body)
	}

	// A flow the member switched on that commands a device runs with their authority: erasure disarms it.
	flow := uuid.NewString()
	if _, e := f.admin.Exec(`INSERT INTO core.automations(tenant_id,id,name,enabled,enabled_by,definition) VALUES($1,$2::uuid,'Lights '||$2::text,true,$3,
    '{"nodes":[{"id":"n1","type":"action.command","data":{}}],"edges":[]}')`, a.TenantID, flow, member.UserID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.Exec(`INSERT INTO core.automation_triggers(tenant_id,automation_id,node_id,kind) VALUES($1,$2,'n1','event')`, a.TenantID, flow); e != nil {
		t.Fatal(e)
	}
	// Somebody else's read of the member's data carries that reader's IP; the member's export must not show it.
	if code, _ := get(t, api, "/api/v1/members/"+memberID+"/access", ownerAuth.AccessToken); code != 200 {
		t.Fatal(code)
	}
	code, body, _ = raw(t, api, "GET", "/api/v1/me/export", memberAuth.AccessToken, nil)
	var again struct {
		AccessLog []map[string]any `json:"access_log"`
	}
	if e := json.Unmarshal(body, &again); code != 200 || e != nil {
		t.Fatalf("second self export: %d %v", code, e)
	}
	sawOther := false
	for _, row := range again.AccessLog {
		if row["actor_id"] != member.UserID {
			sawOther = true
			if row["client_ip"] != nil {
				t.Fatalf("another reader's IP in the member's export: %v", row)
			}
		} else if row["client_ip"] == nil {
			t.Fatalf("the member's own read lost its IP: %v", row)
		}
	}
	if !sawOther {
		t.Fatalf("the owner's read of the member is missing from the export: %v", again.AccessLog)
	}
	auditBefore := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE actor_id=$1`, member.UserID)
	if auditBefore == 0 {
		t.Fatal("the member left no audit trail to keep")
	}
	code, body, _ = raw(t, api, "POST", "/api/v1/members/"+memberID+"/erase", ownerAuth.AccessToken, map[string]any{})
	if code != 200 || !strings.Contains(string(body), `"erased"`) {
		t.Fatalf("erase: %d %s", code, body)
	}
	if _, e := f.service.Login(ctx, email, chosenPassword, a.TenantID); e == nil {
		t.Fatal("an erased member signed in")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.automations WHERE id=$1 AND NOT enabled`, flow) + count(t, f.admin, `SELECT count(*) FROM core.automation_triggers WHERE automation_id=$1`, flow); n != 1 {
		t.Fatal("the member's command flow is still armed")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action='automation.disarmed' AND target_id=$2 AND actor_id=$3`, a.TenantID, flow, owner.UserID); n != 1 {
		t.Fatal("the disarm was not audited")
	}
	if code, _ := get(t, api, "/api/v1/me", memberAuth.AccessToken); code != 401 {
		t.Fatalf("the erased member's live token still works: %d", code)
	}
	var storedEmail, name string
	var erased *time.Time
	if e := f.admin.QueryRow(`SELECT email,name,erased_at FROM identity.users WHERE id=$1`, member.UserID).Scan(&storedEmail, &name, &erased); e != nil {
		t.Fatal(e)
	}
	if storedEmail == email || !strings.HasSuffix(storedEmail, "@erased.invalid") || erased == nil || name == "Tester" {
		t.Fatalf("identity not anonymised: %s %s %v", storedEmail, name, erased)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE actor_id=$1`, member.UserID); n != auditBefore {
		t.Fatalf("the audit trail lost the member's rows: %d -> %d", auditBefore, n)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.memberships WHERE user_id=$1`, member.UserID) + count(t, f.admin, `SELECT count(*) FROM identity.sessions WHERE user_id=$1`, member.UserID); n != 0 {
		t.Fatalf("%d memberships/sessions left", n)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action='member.erased' AND target_id=$2`, a.TenantID, member.UserID); n != 1 {
		t.Fatalf("erasure audited %d times", n)
	}
	code, out := get(t, api, "/api/v1/privacy/erasures", ownerAuth.AccessToken)
	if items, _ := out["items"].([]any); code != 200 || len(items) != 1 || items[0].(map[string]any)["subject_ref"] != member.UserID {
		t.Fatalf("erasure ledger: %d %v", code, out)
	}
	// The placeholder address of an erased identity cannot bring it back.
	payload := map[string]any{"email": storedEmail, "role": "viewer", "password": memberPassword}
	if code, out, _ := req(t, api, "POST", "/api/v1/members", "Bearer "+ownerAuth.AccessToken, "", "", payload); code != 409 {
		t.Fatalf("re-adding an erased identity: %d %v", code, out)
	}

	// An identity another workspace shares is not this workspace's to erase.
	sharedEmail := memberEmail()
	sharedID := addMember(t, api, ownerAuth.AccessToken, sharedEmail, "viewer", nil)
	if _, e := f.admin.Exec(`INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$2,'viewer')`, other.TenantID, sharedID); e != nil {
		t.Fatal(e)
	}
	code, body, _ = raw(t, api, "POST", "/api/v1/members/"+sharedID+"/erase", ownerAuth.AccessToken, map[string]any{})
	if code != 409 || !strings.Contains(string(body), "shared_identity") {
		t.Fatalf("shared identity: %d %s", code, body)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM identity.users WHERE id=$1 AND erased_at IS NULL`, sharedID); n != 1 {
		t.Fatal("the shared identity was touched")
	}

	// Direct SQL: the definer answers "refused" to a non-owner, and the data functions are not the runtime's.
	tx := asRuntime(t, f, member.UserID, a.TenantID)
	var outcome string
	if e := tx.QueryRow(`SELECT outcome FROM identity.erase_member($1)`, sharedID).Scan(&outcome); e != nil || outcome != "refused" {
		t.Fatalf("erase_member as a non-member: %q %v", outcome, e)
	}
	tx.Rollback()
	for _, statement := range []string{
		`SELECT core.erase_member_data('` + a.TenantID + `','` + sharedID + `',NULL)`,
		`SELECT core.erase_identity_data('` + a.TenantID + `','c30000393fe5',NULL,true)`,
		`SELECT core.set_access_log_retention(30)`,
		`INSERT INTO core.erasure_log(tenant_id,id,subject_kind,subject_ref) VALUES(core.tenant_id(),gen_random_uuid(),'member','x')`,
	} {
		tx := asRuntime(t, f, owner.UserID, owner.TenantID)
		if _, e := tx.Exec(statement); sqlState(e) != "42501" {
			t.Fatalf("%s: %v", statement, e)
		}
		tx.Rollback()
	}
}

// A worn tag's history is erased from every partition (legacy, a weekly or daily range, DEFAULT), within the
// registration's project unless the owner asks for the whole workspace, and never in another workspace. Alerts stay
// as incident records under a generic title; notification errors, flow runs, learned-signal labels, stream names and
// removed registrations lose the wearer. The export streams it all first, as a ZIP.
func TestIdentityHistoryExportAndErasure(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	ctx := context.Background()
	const external = "c3000039aa01"
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, e := f.admin.ExecContext(ctx, statement, args...); e != nil {
			t.Fatalf("%s: %v", statement, e)
		}
	}
	// seed puts a gateway (in project, or in none) and the tag's history on it; live registers the tag there.
	seed := func(tenant string, project *string, live bool) (gateway, device string) {
		t.Helper()
		gateway, device = uuid.NewString(), uuid.NewString()
		event, alert, flow := uuid.NewString(), uuid.NewString(), uuid.NewString()
		exec(`INSERT INTO core.gateways(id,tenant_id,name,model,token_hash,project_id) VALUES($2,$1,'Ward','minew-mg3','x',$3)`, tenant, gateway, project)
		exec(`INSERT INTO core.sensor_streams(tenant_id,gateway_id,external_id,name,last_seen) VALUES($1,$2,$3,'Somchai tag',now())`, tenant, gateway, external)
		removed := "now()"
		if live {
			removed = "NULL"
		}
		exec(`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id,roaming,removed_at) VALUES($3,$1,$2,'Somchai',$4,'minew-b10',true,`+removed+`)`, tenant, gateway, device, external)
		exec(`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading)
      SELECT $1,$2,$3,'k'||i,t,'minew-ffe1-a101@1','{}' FROM (VALUES (1,now()-interval '1 hour'),(2,now()+interval '15 days'),(3,now()+interval '3 years')) v(i,t)`, tenant, gateway, external)
		exec(`INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source)
      SELECT $1,$2,$3,'k'||i,t,'020106','device' FROM (VALUES (1,now()-interval '1 hour'),(2,now()+interval '10 days'),(3,now()+interval '3 years')) v(i,t)`, tenant, gateway, external)
		exec(`INSERT INTO core.presence_state(tenant_id,external_id,gateway_id,since) VALUES($1,$3,$2,now()) ON CONFLICT DO NOTHING`, tenant, gateway, external)
		exec(`INSERT INTO core.device_events(tenant_id,id,gateway_id,external_id,device_name,event_type,detail,occurred_at) VALUES($1,gen_random_uuid(),$2,$3,'Somchai','zone_enter','{"zone":"ward"}',now())`, tenant, gateway, external)
		exec(`INSERT INTO core.device_events(tenant_id,id,gateway_id,external_id,device_name,event_type,detail,occurred_at) VALUES($1,$4,$2,$3,'Somchai','button','{"press":1}',now())`, tenant, gateway, external, event)
		exec(`INSERT INTO core.alerts(tenant_id,id,event_id,gateway_id,external_id,device_name,event_type,severity,title) VALUES($1,$5,$4,$2,$3,'Somchai','button','critical','SOS: Somchai')`, tenant, gateway, external, event, alert)
		exec(`INSERT INTO core.notifications(tenant_id,id,alert_id,status,last_error) VALUES($1,gen_random_uuid(),$2,'failed','LINE echoed: SOS: Somchai')`, tenant, alert)
		exec(`INSERT INTO core.automations(tenant_id,id,name) VALUES($1,$2::uuid,'SOS flow '||$2::text)`, tenant, flow)
		exec(`INSERT INTO core.automation_runs(tenant_id,id,automation_id,external_id,gateway_id,status,detail) VALUES($1,gen_random_uuid(),$3,$4,$2,'fired','{"name":"Somchai"}')`, tenant, gateway, flow, external)
		exec(`INSERT INTO core.signal_sessions(tenant_id,id,gateway_id,external_id,event_type,label,baseline_until,trigger_until) VALUES($1,gen_random_uuid(),$2,$3,'button','Somchai',now(),now()+interval '1 minute')`, tenant, gateway, external)
		t.Cleanup(func() {
			for _, table := range []string{"core.sensor_samples", "core.ble_history"} {
				f.admin.Exec(`DELETE FROM `+table+` WHERE tenant_id=$1`, tenant)
			}
		})
		return gateway, device
	}
	a, ownerAuth, _ := f.account(t)
	b, _, _ := f.account(t)
	project := uuid.NewString()
	exec(`INSERT INTO core.projects(tenant_id,id,name) VALUES($1,$2,'Other ward')`, a.TenantID, project)
	here, deviceA := seed(a.TenantID, nil, true)      // the registration being erased: gateways with no project
	elsewhere, _ := seed(a.TenantID, &project, false) // the same tag seen in another project (a removed registration)
	seed(b.TenantID, nil, true)                       // another workspace
	exec(`INSERT INTO core.device_signals(tenant_id,id,scope,external_id,event_type,matcher,description) VALUES($1,gen_random_uuid(),'device',$2,'button','{}','Somchai presses twice')`, a.TenantID, external)
	if n := count(t, f.admin, `SELECT count(DISTINCT tableoid) FROM core.sensor_samples WHERE tenant_id=$1`, a.TenantID); n != 3 {
		t.Fatalf("the seed spans %d sample partitions, want legacy, a range and DEFAULT", n)
	}
	adminEmail := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, adminEmail, "admin", nil)
	adminAuth, _ := changeInitialPassword(t, f, api, adminEmail, a.TenantID)
	for _, path := range []string{"/privacy-export", "/erase-history"} {
		if code, body, _ := raw(t, api, "POST", "/api/v1/devices/"+deviceA+path, adminAuth.AccessToken, map[string]any{}); code != 403 {
			t.Fatalf("admin %s: %d %s", path, code, body)
		}
	}

	// The export streams every row of the tag in the workspace; manifest.json marks it complete.
	code, body, disposition := raw(t, api, "POST", "/api/v1/devices/"+deviceA+"/privacy-export", ownerAuth.AccessToken, map[string]any{})
	if code != 200 || !strings.Contains(disposition, ".zip") {
		t.Fatalf("export: %d %q %s", code, disposition, body)
	}
	files := unzip(t, body)
	if strings.Count(files["samples.ndjson"], "\n") != 6 || strings.Count(files["ble_history.ndjson"], "\n") != 6 || !strings.Contains(files["summary.json"], "SOS: Somchai") || !strings.Contains(files["manifest.json"], `"complete":true`) {
		t.Fatalf("export content: %v", files)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.access_log WHERE tenant_id=$1 AND resource='export' AND subject_kind='device' AND subject_id=$2`, a.TenantID, deviceA); n != 1 {
		t.Fatalf("export logged %d times", n)
	}

	// Erase within the registration's project (no project), renaming the registration in the same step.
	code, body, _ = raw(t, api, "POST", "/api/v1/devices/"+deviceA+"/erase-history", ownerAuth.AccessToken, map[string]any{"name": "Tag 7"})
	if code != 200 {
		t.Fatalf("erase: %d %s", code, body)
	}
	var result struct {
		Outcome string         `json:"outcome"`
		Counts  map[string]any `json:"counts"`
	}
	if e := json.Unmarshal(body, &result); e != nil || result.Outcome != "erased" || result.Counts["samples"] != 3.0 || result.Counts["ble_history"] != 3.0 || result.Counts["events"] != 1.0 || result.Counts["alerts_anonymised"] != 1.0 {
		t.Fatalf("erase result: %s", body)
	}
	left := func(gateway, table string) int {
		return count(t, f.admin, `SELECT count(*) FROM `+table+` WHERE gateway_id=$1 AND external_id=$2`, gateway, external)
	}
	for _, table := range []string{"core.sensor_samples", "core.ble_history"} {
		if left(here, table) != 0 || left(elsewhere, table) != 3 {
			t.Fatalf("%s: %d here, %d in the other project", table, left(here, table), left(elsewhere, table))
		}
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE tenant_id=$1 AND external_id=$2`, b.TenantID, external); n != 3 {
		t.Fatalf("the other workspace lost rows: %d", n)
	}
	for _, check := range []string{
		`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND (device_name LIKE '%Somchai%' OR title LIKE '%Somchai%' OR title NOT LIKE 'กดปุ่มฉุกเฉิน SOS%')`,
		`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND (device_name LIKE '%Somchai%' OR detail<>'{}')`,
		`SELECT count(*) FROM core.notifications n JOIN core.alerts a ON a.id=n.alert_id WHERE a.gateway_id=$1 AND n.last_error IS NOT NULL`,
		`SELECT count(*) FROM core.automation_runs WHERE gateway_id=$1 AND (external_id<>'' OR detail<>'{}')`,
		`SELECT count(*) FROM core.signal_sessions WHERE gateway_id=$1 AND label<>''`,
		`SELECT count(*) FROM core.sensor_streams WHERE gateway_id=$1 AND name LIKE '%Somchai%'`,
		`SELECT count(*) FROM core.presence_state p JOIN core.gateways g ON g.id=p.gateway_id WHERE g.id=$1`,
	} {
		if n := count(t, f.admin, check, here); n != 0 {
			t.Fatalf("still names the wearer (%d): %s", n, check)
		}
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.alerts WHERE gateway_id=$1`, here); n != 1 {
		t.Fatal("the incident record went")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND title='SOS: Somchai'`, elsewhere); n != 1 {
		t.Fatal("the other project's alert was touched")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.device_signals WHERE tenant_id=$1 AND external_id=$2 AND description<>''`, a.TenantID, external); n != 0 {
		t.Fatal("the learned signal keeps its label")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.devices WHERE id=$1 AND name='Tag 7'`, deviceA); n != 1 {
		t.Fatal("the registration was not renamed")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.devices WHERE gateway_id=$1 AND name='Somchai'`, elsewhere); n != 1 {
		t.Fatal("the other project's removed registration was renamed")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.erasure_log WHERE tenant_id=$1 AND subject_kind='device_identity' AND subject_ref=$2 AND scope->>'tenant_wide'='false' AND scope->'project_id'='null'::jsonb AND scope->>'renamed'='true' AND scope->>'device_id'=$3`, a.TenantID, external, deviceA); n != 1 {
		t.Fatal("no ledger row with the project scope")
	}
	for _, action := range []string{"device.history_exported", "device.history_erased", "device.renamed"} {
		if n := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND target_id=$2 AND action=$3`, a.TenantID, deviceA, action); n != 1 {
			t.Fatalf("%s audited %d times", action, n)
		}
	}
	// A rename the name rules refuse refuses the whole request: nothing is erased either.
	if code, body, _ := raw(t, api, "POST", "/api/v1/devices/"+deviceA+"/erase-history", ownerAuth.AccessToken, map[string]any{"name": "bad\nname", "tenant_wide": true}); code != 400 {
		t.Fatalf("bad name: %d %s", code, body)
	}
	if left(elsewhere, "core.sensor_samples") != 3 {
		t.Fatal("a refused request erased rows")
	}

	// The whole workspace, when the owner asks for it.
	if code, body, _ := raw(t, api, "POST", "/api/v1/devices/"+deviceA+"/erase-history", ownerAuth.AccessToken, map[string]any{"tenant_wide": true}); code != 200 {
		t.Fatalf("tenant-wide erase: %d %s", code, body)
	}
	for _, table := range []string{"core.sensor_samples", "core.ble_history"} {
		if left(elsewhere, table) != 0 {
			t.Fatalf("%s: rows left in the other project", table)
		}
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.devices WHERE gateway_id=$1 AND name LIKE '%Somchai%'`, elsewhere); n != 0 {
		t.Fatal("the removed registration keeps the wearer's name")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE tenant_id=$1 AND external_id=$2`, b.TenantID, external); n != 3 {
		t.Fatalf("the other workspace lost rows: %d", n)
	}

	// Fail closed: without the read-access row the archive never starts.
	if _, e := f.admin.Exec(`REVOKE INSERT ON core.access_log FROM aether_app`); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.admin.Exec(`GRANT INSERT ON core.access_log TO aether_app`) })
	code, body, disposition = raw(t, api, "POST", "/api/v1/devices/"+deviceA+"/privacy-export", ownerAuth.AccessToken, map[string]any{})
	if code != 503 || disposition != "" || bytes.HasPrefix(body, []byte("PK")) {
		t.Fatalf("export without a log row: %d %q", code, disposition)
	}
}

func unzip(t *testing.T, body []byte) map[string]string {
	t.Helper()
	archive, e := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if e != nil {
		t.Fatal(e)
	}
	files := map[string]string{}
	for _, file := range archive.File {
		r, e := file.Open()
		if e != nil {
			t.Fatal(e)
		}
		content, _ := io.ReadAll(r)
		r.Close()
		files[file.Name] = string(content)
	}
	return files
}

// The notice pointer is public; acknowledging it is stored per identity and cannot run ahead of the notice.
func TestPrivacyNoticeAcknowledgement(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.PrivacyNoticeURL = "https://example.test/privacy"
	api := httpapi.New(cfg, f.service, f.repo)
	_, auth, _ := f.account(t)
	code, out, _ := req(t, api, "GET", "/api/v1/notice", "", "", "", nil)
	if code != 200 || out["version"] != 1.0 || out["url"] != cfg.PrivacyNoticeURL {
		t.Fatalf("notice: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/me", auth.AccessToken)
	if code != 200 || out["notice_version"] != 1.0 || out["notice_ack_version"] != 0.0 || out["notice_url"] != cfg.PrivacyNoticeURL {
		t.Fatalf("me: %d %v", code, out)
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/me/notice-ack", "Bearer "+auth.AccessToken, "", "", map[string]any{"version": 2}); code != 400 {
		t.Fatalf("acknowledging a notice that does not exist: %d %v", code, out)
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/me/notice-ack", "Bearer "+auth.AccessToken, "", "", map[string]any{"version": 1}); code != 200 || out["notice_ack_version"] != 1.0 {
		t.Fatalf("ack: %d %v", code, out)
	}
	if _, out := get(t, api, "/api/v1/me", auth.AccessToken); out["notice_ack_version"] != 1.0 {
		t.Fatalf("me after ack: %v", out)
	}
}

// The read-access log is partitioned by month (00038) and kept by the same maintenance as the sample history:
// ranges ahead from the current month, expired months dropped (empty ones always, ones with rows when the data
// agrees), expired rows in DEFAULT pruned. Staged in a transaction that rolls back.
func TestAccessLogPartitions(t *testing.T) {
	f := setup(t)
	steady(t, f.admin)
	month := time.Now().UTC()
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	if n := count(t, f.admin, `SELECT count(*) FROM core.partition_ranges('access_log') WHERE NOT is_default AND lo=$1`, month); n != 1 {
		t.Fatalf("no range for the current month (%s)", month)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.partition_ranges('access_log') WHERE NOT is_default AND lo<$1`, month); n != 0 {
		t.Fatal("ranges before the first month of the trail")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.partition_ranges('access_log') WHERE hi >= now() + interval '31 days'`); n == 0 {
		t.Fatal("less than a month of ranges ahead")
	}
	_, _, a := f.account(t)
	tx, e := f.admin.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := tx.Exec(q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	for _, name := range []string{"access_log_p20000101", "access_log_p20000201"} {
		exec(`CREATE TABLE core.` + name + ` (LIKE core.access_log INCLUDING CONSTRAINTS)`)
		exec(`ALTER TABLE core.` + name + ` OWNER TO aether_owner`)
	}
	exec(`ALTER TABLE core.access_log ATTACH PARTITION core.access_log_p20000101 FOR VALUES FROM ('2000-01-01 00:00:00+00') TO ('2000-02-01 00:00:00+00')`)
	exec(`ALTER TABLE core.access_log ATTACH PARTITION core.access_log_p20000201 FOR VALUES FROM ('2000-02-01 00:00:00+00') TO ('2000-03-01 00:00:00+00')`)
	exec(`INSERT INTO core.access_log(tenant_id,id,at,actor_id,resource,subject_kind,subject_id) VALUES($1,gen_random_uuid(),'2000-02-10',$2,'x','x','x')`, a.TenantID, a.UserID)
	exec(`INSERT INTO core.access_log(tenant_id,id,at,actor_id,resource,subject_kind,subject_id) VALUES($1,gen_random_uuid(),now()-interval '1 hour',$2,'x','x','x')`, a.TenantID, a.UserID)
	// A row older than the retention that no range covers sits in DEFAULT.
	exec(`INSERT INTO core.access_log(tenant_id,id,at,actor_id,resource,subject_kind,subject_id) VALUES($1,gen_random_uuid(),now()-interval '500 days',$2,'x','x','x')`, a.TenantID, a.UserID)
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 1 || steps[0] != "dropped core.access_log_p20000101" {
		t.Fatalf("the empty expired month first, got %v", steps)
	}
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 1 || steps[0] != "dropped core.access_log_p20000201" {
		t.Fatalf("then the one with a row (recent rows confirm the clock), got %v", steps)
	}
	var pruned int
	if e := tx.QueryRow(`SELECT core.prune_history(90,24,1000)`).Scan(&pruned); e != nil || pruned < 1 {
		t.Fatalf("prune: %d %v", pruned, e)
	}
	if n := count(t, tx, `SELECT count(*) FROM core.access_log_default`); n != 0 {
		t.Fatalf("%d expired rows left in DEFAULT", n)
	}
	if n := count(t, tx, `SELECT count(*) FROM core.access_log WHERE tenant_id=$1`, a.TenantID); n != 1 {
		t.Fatalf("the recent row went too: %d", n)
	}
}

// Opening the signal stream is a read (signals name devices, worn tags included): it is logged, and refused
// when the log cannot be written.
func TestRealtimeSubscribeIsLogged(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go api.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	t.Cleanup(func() { api.Shutdown() })
	_, auth, p := f.account(t)
	open := func() string {
		t.Helper()
		var conn *websocket.Conn
		for attempt := 0; ; attempt++ { // the listener starts in its own goroutine
			var res *http.Response
			conn, res, e = websocket.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/ws", http.Header{"Origin": {origin}})
			if e == nil {
				break
			}
			if attempt == 20 {
				status := 0
				if res != nil {
					status = res.StatusCode
				}
				t.Fatalf("dial: %v %d", e, status)
			}
			time.Sleep(50 * time.Millisecond)
		}
		defer conn.Close()
		if e := conn.WriteJSON(map[string]string{"type": "auth", "token": auth.AccessToken}); e != nil {
			t.Fatal(e)
		}
		var msg map[string]any
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if e := conn.ReadJSON(&msg); e != nil {
			t.Fatal(e)
		}
		if msg["type"] == "error" {
			return msg["error"].(string)
		}
		return msg["type"].(string)
	}
	if got := open(); got != "ready" {
		t.Fatalf("subscribe: %s", got)
	}
	if n := accessRows(t, f, p.TenantID, p.UserID, "realtime"); n != 1 {
		t.Fatalf("subscribe logged %d times", n)
	}
	if _, e := f.admin.Exec(`REVOKE INSERT ON core.access_log FROM aether_app`); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.admin.Exec(`GRANT INSERT ON core.access_log TO aether_app`) })
	// Within the dedupe window the second socket needs no row; a fresh API instance has no window.
	api2 := busyAPI(f)
	ln2, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go api2.Listener(ln2, fiber.ListenConfig{DisableStartupMessage: true})
	t.Cleanup(func() { api2.Shutdown() })
	ln = ln2
	if got := open(); got != "unavailable" {
		t.Fatalf("subscribe without a log row: %s", got)
	}
}

// The trail's retention is the owner-only policy row's, set only by the migration role: the runtime calling
// maintenance with the shortest retention it can pass drops nothing of the trail that the policy still keeps.
func TestRuntimeCannotShortenTheAccessLog(t *testing.T) {
	f := setup(t)
	steady(t, f.admin)
	_, _, p := f.account(t)
	month := time.Now().UTC().AddDate(0, 0, -200)
	lo := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	hi := lo.AddDate(0, 1, 0)
	name := "access_log_p" + lo.Format("20060102")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := f.admin.Exec(q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	t.Cleanup(func() {
		f.admin.Exec(`DROP TABLE IF EXISTS core.` + name)
		f.admin.Exec(`SELECT core.set_access_log_retention(400)`)
	})
	exec(`CREATE TABLE core.` + name + ` (LIKE core.access_log INCLUDING CONSTRAINTS)`)
	exec(`ALTER TABLE core.` + name + ` OWNER TO aether_owner`)
	exec(`ALTER TABLE core.access_log ATTACH PARTITION core.` + name + ` FOR VALUES FROM ('` + lo.Format(time.RFC3339) + `') TO ('` + hi.Format(time.RFC3339) + `')`)
	exec(`INSERT INTO core.access_log(tenant_id,id,at,actor_id,resource,subject_kind,subject_id) VALUES($1,gen_random_uuid(),$3,$2,'x','x','x'),($1,gen_random_uuid(),now(),$2,'x','x','x')`, p.TenantID, p.UserID, lo.Add(time.Hour))
	for i := 0; i < 5; i++ {
		tx := asRuntime(t, f, "", "")
		if _, e := tx.Exec(`SELECT * FROM core.maintain_partitions(1,1,true)`); e != nil {
			t.Fatal(e)
		}
		if _, e := tx.Exec(`SELECT core.prune_history(1,1,50000)`); e != nil {
			t.Fatal(e)
		}
		if e := tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.access_log WHERE at >= $1 AND at < $2`, lo, hi); n != 1 {
		t.Fatalf("the runtime shortened the trail: %d rows left in %s", n, name)
	}
	// The runtime can neither read nor change the policy.
	for _, statement := range []string{`SELECT core.set_access_log_retention(30)`, `SELECT * FROM core.retention_policy`, `UPDATE core.retention_policy SET access_days=30`} {
		tx := asRuntime(t, f, p.UserID, p.TenantID)
		if _, e := tx.Exec(statement); sqlState(e) != "42501" {
			t.Fatalf("%s: %v", statement, e)
		}
		tx.Rollback()
	}
	// The migration role can; then the same maintenance drops the month.
	exec(`SELECT core.set_access_log_retention(30)`)
	for i := 0; i < 5; i++ {
		tx := asRuntime(t, f, "", "")
		if _, e := tx.Exec(`SELECT * FROM core.maintain_partitions(90,24,true)`); e != nil {
			t.Fatal(e)
		}
		tx.Commit()
	}
	if n := count(t, f.admin, `SELECT count(*) FROM pg_class WHERE relname=$1`, name); n != 0 {
		t.Fatal("the policy's own retention did not apply")
	}
}
