package tests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// Members and project-scoped access. The narrowing is a database rule (migration 00019): every helper
// below goes through the HTTP API or the repository, never through a hand-written query, so a policy that
// stopped applying would show up as a restricted member suddenly seeing another project's rows.

const memberPassword = "handed over in person 1"

// chosenPassword is what a member picks once they are made to replace the handed-over one.
const chosenPassword = "chosen by the member 1"

// busyAPI builds an API whose per-IP budget fits one long test; every test here shares the loopback IP.
func busyAPI(f *fixture) *fiber.App {
	cfg := f.cfg
	cfg.APIRateLimit = 600
	return httpapi.New(cfg, f.service, f.repo)
}

func memberEmail() string { return uuid.NewString() + "@example.test" }

// signIn logs a member in and returns both the HTTP credentials and the principal for repository calls.
func signIn(t *testing.T, f *fixture, email, password, tenant string) (app.AuthResult, domain.Principal) {
	t.Helper()
	auth, e := f.service.Login(context.Background(), email, password, tenant)
	if e != nil {
		t.Fatalf("login %s: %v", email, e)
	}
	p, e := f.service.Authenticate(context.Background(), auth.AccessToken)
	if e != nil {
		t.Fatalf("authenticate %s: %v", email, e)
	}
	return auth, p
}

// addMember creates a membership through the API and returns the created member's user id.
func addMember(t *testing.T, api *fiber.App, token, email, role string, projects []string) string {
	t.Helper()
	payload := map[string]any{"email": email, "role": role, "password": memberPassword, "project_ids": projects}
	code, out, _ := req(t, api, "POST", "/api/v1/members", "Bearer "+token, "", "", payload)
	if code != 201 {
		t.Fatalf("add %s member: %d %v", role, code, out)
	}
	id, _ := out["user_id"].(string)
	if id == "" || out["email"] != email || out["role"] != role || out["must_change_password"] != true {
		t.Fatalf("the created member row: %v", out)
	}
	return id
}

// changeInitialPassword does what every new member has to do before the API answers anything else, and
// proves on the way that a handed-over password really is enforced and not merely advertised.
func changeInitialPassword(t *testing.T, f *fixture, api *fiber.App, email, tenant string) (app.AuthResult, domain.Principal) {
	t.Helper()
	first, _ := signIn(t, f, email, memberPassword, tenant)
	if !first.MustChangePassword {
		t.Fatalf("%s was not asked to replace the handed-over password", email)
	}
	code, out, _ := req(t, api, "GET", "/api/v1/gateways", "Bearer "+first.AccessToken, "", "", nil)
	if code != 403 || out["detail"] != "password_change_required" {
		t.Fatalf("a session owing a password change reached the API: %d %v", code, out)
	}
	code, out, _ = req(t, api, "POST", "/api/v1/auth/password", "Bearer "+first.AccessToken, "", origin, map[string]any{"current_password": memberPassword, "new_password": chosenPassword})
	if code != 204 {
		t.Fatalf("change the initial password of %s: %d %v", email, code, out)
	}
	if code, _ := get(t, api, "/api/v1/gateways", first.AccessToken); code != 200 {
		t.Fatalf("the API stayed closed after the password was replaced: %d", code)
	}
	return signIn(t, f, email, chosenPassword, tenant)
}

// memberID finds a member by email in GET /members.
func memberID(t *testing.T, api *fiber.App, token, email string) string {
	t.Helper()
	code, out, _ := req(t, api, "GET", "/api/v1/members", "Bearer "+token, "", "", nil)
	if code != 200 {
		t.Fatalf("list members: %d %v", code, out)
	}
	for _, raw := range rows(out) {
		if raw["email"] == email {
			return raw["user_id"].(string)
		}
	}
	t.Fatalf("member %s not listed: %v", email, out)
	return ""
}

// listOf turns one array field of a JSON body into typed maps.
func listOf(out map[string]any, field string) []map[string]any {
	list, _ := out[field].([]any)
	items := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		if row, ok := raw.(map[string]any); ok {
			items = append(items, row)
		}
	}
	return items
}

// rows is the {"items": [...]} shape every list endpoint returns.
func rows(out map[string]any) []map[string]any { return listOf(out, "items") }

// values collects one field of every row, so a test can say exactly which ids it expects to see.
func values(out map[string]any, field string) map[string]bool {
	seen := map[string]bool{}
	for _, row := range rows(out) {
		if v, ok := row[field].(string); ok {
			seen[v] = true
		}
	}
	return seen
}

func get(t *testing.T, api *fiber.App, path, token string) (int, map[string]any) {
	t.Helper()
	code, out, _ := req(t, api, "GET", path, "Bearer "+token, "", "", nil)
	return code, out
}

func TestRestrictedMemberSeesOnlyTheirProject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)

	projectA, e := f.service.CreateProject(ctx, owner, "โครงการ A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	projectB, e := f.service.CreateProject(ctx, owner, "โครงการ B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	gatewayA, _, e := f.service.CreateGatewayIn(ctx, owner, "GW A", "minew-mg3", &projectA.ID)
	if e != nil {
		t.Fatal(e)
	}
	gatewayB, _, e := f.service.CreateGatewayIn(ctx, owner, "GW B", "minew-mg3", &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	// A gateway outside every project is invisible to a restricted member and visible to the owner.
	gatewayNone, _, e := f.service.CreateGateway(ctx, owner, "GW ยังไม่จัดโปรเจค", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	deviceA, e := f.service.CreateDevice(ctx, owner, gatewayA.ID, "Dev A", "f00000000001", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	deviceB, e := f.service.CreateDevice(ctx, owner, gatewayB.ID, "Dev B", "f00000000002", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	code, out, _ := req(t, api, "POST", "/api/v1/sites", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"name": "อาคาร A", "project_id": projectA.ID})
	if code != 201 {
		t.Fatalf("site A: %d %v", code, out)
	}
	siteA := out["id"].(string)
	code, out, _ = req(t, api, "POST", "/api/v1/sites", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"name": "อาคาร B", "project_id": projectB.ID})
	if code != 201 {
		t.Fatalf("site B: %d %v", code, out)
	}
	siteB := out["id"].(string)
	code, out, _ = req(t, api, "POST", "/api/v1/sites", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"name": "อาคารกลาง"})
	if code != 201 {
		t.Fatalf("site without a project: %d %v", code, out)
	}
	siteNone := out["id"].(string)
	code, out, _ = req(t, api, "POST", "/api/v1/rules", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"name": "Tamper ทุกที่", "event_type": "tamper", "severity": "critical", "channels": []string{}, "dedupe_sec": 600})
	if code != 201 {
		t.Fatalf("rule: %d %v", code, out)
	}

	restricted, unrestricted := memberEmail(), memberEmail()
	addMember(t, api, ownerAuth.AccessToken, restricted, "viewer", []string{projectA.ID})
	addMember(t, api, ownerAuth.AccessToken, unrestricted, "viewer", []string{})

	viewerAuth, viewer := changeInitialPassword(t, f, api, restricted, owner.TenantID)
	openAuth, _ := changeInitialPassword(t, f, api, unrestricted, owner.TenantID)

	// GET /me reports the role and the scope the database enforces.
	code, out = get(t, api, "/api/v1/me", viewerAuth.AccessToken)
	if code != 200 || out["role"] != "viewer" || out["email"] != restricted || out["must_change_password"] != false {
		t.Fatalf("me (restricted): %d %v", code, out)
	}
	scope, _ := out["project_ids"].([]any)
	if len(scope) != 1 || scope[0] != projectA.ID {
		t.Fatalf("me must report exactly the scoped project: %v", out["project_ids"])
	}
	code, out = get(t, api, "/api/v1/me", openAuth.AccessToken)
	if code != 200 || out["project_ids"] != nil {
		t.Fatalf("an unrestricted member must report a null scope: %d %v", code, out)
	}

	// Ingest into project B while the restricted session exists: system work is never narrowed.
	if _, e := f.repo.CapturePacket(ctx, owner.TenantID, gatewayB.ID, simulation.Packet(27, time.Now().UTC())); e != nil {
		t.Fatalf("ingest into project B: %v", e)
	}
	if _, e := f.repo.StoreTelemetry(ctx, owner.TenantID, gatewayB.ID, deviceB.ID, time.Now().UTC().Add(-time.Minute), map[string]float64{"temperature": 25}); e != nil {
		t.Fatalf("telemetry into project B: %v", e)
	}
	ownerEvents, e := f.repo.ListEvents(ctx, owner, "", 200)
	if e != nil || len(ownerEvents) == 0 {
		t.Fatalf("ingest into project B produced no events: %d %v", len(ownerEvents), e)
	}

	// Projects, gateways and devices.
	code, out = get(t, api, "/api/v1/projects", viewerAuth.AccessToken)
	if code != 200 || len(values(out, "id")) != 1 || !values(out, "id")[projectA.ID] {
		t.Fatalf("projects (restricted): %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/gateways", viewerAuth.AccessToken)
	seen := values(out, "id")
	if code != 200 || len(seen) != 1 || !seen[gatewayA.ID] {
		t.Fatalf("gateways (restricted) must be project A only: %d %v", code, out)
	}
	if seen[gatewayNone.ID] {
		t.Fatal("a gateway without a project must be hidden from a restricted member")
	}
	code, out = get(t, api, "/api/v1/devices", viewerAuth.AccessToken)
	seen = values(out, "id")
	if code != 200 || len(seen) != 1 || !seen[deviceA.ID] {
		t.Fatalf("devices (restricted): %d %v", code, out)
	}

	// Events and alerts follow the gateway that raised them.
	code, out = get(t, api, "/api/v1/events?limit=200", viewerAuth.AccessToken)
	if code != 200 || len(rows(out)) != 0 {
		t.Fatalf("events of project B leaked to a project A member: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/alerts?status=&limit=200", viewerAuth.AccessToken)
	if code != 200 || len(rows(out)) != 0 {
		t.Fatalf("alerts of project B leaked to a project A member: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/alerts/summary", viewerAuth.AccessToken)
	if code != 200 || out["open"] != float64(0) {
		t.Fatalf("alert counts of project B leaked: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/events?limit=200", openAuth.AccessToken)
	if code != 200 || len(rows(out)) == 0 {
		t.Fatalf("an unrestricted viewer must still see every event: %d %v", code, out)
	}

	// Sites, and a direct GET by id of another project's resource.
	code, out = get(t, api, "/api/v1/sites", viewerAuth.AccessToken)
	seen = values(out, "id")
	if code != 200 || len(seen) != 1 || !seen[siteA] {
		t.Fatalf("sites (restricted): %d %v", code, out)
	}
	if seen[siteNone] {
		t.Fatal("a site without a project must be hidden from a restricted member")
	}
	if code, out = get(t, api, "/api/v1/sites/"+siteB, viewerAuth.AccessToken); code != 404 {
		t.Fatalf("site of project B by id: %d %v", code, out)
	}
	if code, _ = get(t, api, "/api/v1/sites/"+siteA, viewerAuth.AccessToken); code != 200 {
		t.Fatalf("site of project A by id: %d", code)
	}

	// Assets (gateways and devices merged with the workspace's own bookkeeping).
	code, out = get(t, api, "/api/v1/assets", viewerAuth.AccessToken)
	seen = values(out, "asset_id")
	if code != 200 || len(seen) != 2 || !seen[gatewayA.ID] || !seen[deviceA.ID] {
		t.Fatalf("assets (restricted) must be project A only: %d %v", code, out)
	}
	if code, out = get(t, api, "/api/v1/assets/gateway/"+gatewayB.ID, viewerAuth.AccessToken); code != 404 {
		t.Fatalf("asset detail of project B by id: %d %v", code, out)
	}

	// Latest device state by id.
	if code, out = get(t, api, "/api/v1/devices/"+deviceB.ID+"/state", viewerAuth.AccessToken); code != 404 {
		t.Fatalf("device state of project B by id: %d %v", code, out)
	}
	if code, out = get(t, api, "/api/v1/devices/"+deviceB.ID+"/state", openAuth.AccessToken); code != 200 {
		t.Fatalf("an unrestricted viewer must read project B state: %d %v", code, out)
	}

	// Presence joins through the gateways that heard the identity. Only project B has heard it so far.
	code, out = get(t, api, "/api/v1/presence/f00000000001", viewerAuth.AccessToken)
	if code != 200 || len(listOf(out, "sightings")) != 0 {
		t.Fatalf("sightings by a project B gateway leaked: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/presence/f00000000001", openAuth.AccessToken)
	if code != 200 || len(listOf(out, "sightings")) != 1 {
		t.Fatalf("an unrestricted viewer must see the project B sighting: %d %v", code, out)
	}
	if _, e := f.repo.CapturePacket(ctx, owner.TenantID, gatewayA.ID, simulation.Packet(27, time.Now().UTC())); e != nil {
		t.Fatal(e)
	}
	code, out = get(t, api, "/api/v1/presence/f00000000001", viewerAuth.AccessToken)
	items := listOf(out, "sightings")
	if code != 200 || len(items) != 1 || items[0]["gateway_id"] != gatewayA.ID {
		t.Fatalf("presence must show the project A gateway only: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/presence/f00000000001", openAuth.AccessToken)
	if code != 200 || len(listOf(out, "sightings")) != 2 {
		t.Fatalf("an unrestricted viewer must see both sightings: %d %v", code, out)
	}

	// The remembered zone of a roaming tag names a gateway. Being allowed to know the tag exists is not
	// being allowed to know it is standing in another project's building, so core.presence_state is
	// visible only when every gateway it names is in scope too. The row is written through the privileged
	// connection so the test is about the policy, not about how many uplinks the zone smoothing needs.
	if e := f.repo.SetDeviceRoaming(ctx, owner, deviceA.ID, true); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.Exec(`INSERT INTO core.presence_state(tenant_id,external_id,gateway_id,since) VALUES($1,$2,$3,now())
    ON CONFLICT(tenant_id,external_id) DO UPDATE SET gateway_id=excluded.gateway_id,since=excluded.since`, owner.TenantID, "f00000000001", gatewayB.ID); e != nil {
		t.Fatal(e)
	}
	code, out = get(t, api, "/api/v1/devices", viewerAuth.AccessToken)
	if code != 200 || len(rows(out)) != 1 {
		t.Fatalf("devices (restricted, roaming): %d %v", code, out)
	}
	for _, row := range rows(out) {
		if row["zone_gateway_id"] != nil {
			t.Fatalf("the remembered zone exposed a project B gateway: %v", row)
		}
	}
	code, out = get(t, api, "/api/v1/devices", ownerAuth.AccessToken)
	zoned := 0
	for _, row := range rows(out) {
		if row["zone_gateway_id"] == gatewayB.ID {
			zoned++
		}
	}
	if code != 200 || zoned != 1 {
		t.Fatalf("the owner must still see the remembered zone: %d %d %v", code, zoned, out)
	}
	code, out = get(t, api, "/api/v1/presence/f00000000001", viewerAuth.AccessToken)
	if code != 200 || out["since"] != nil {
		t.Fatalf("the settled zone of another project leaked through presence: %d %v", code, out)
	}
	for _, row := range listOf(out, "sightings") {
		if row["gateway_id"] == gatewayB.ID {
			t.Fatalf("a project B sighting reached a restricted member: %v", row)
		}
	}
	code, out = get(t, api, "/api/v1/events?limit=200", viewerAuth.AccessToken)
	if code != 200 || len(rows(out)) == 0 {
		t.Fatalf("events of project A must reach its own member: %d %v", code, out)
	}
	for _, row := range rows(out) {
		if row["gateway_id"] != gatewayA.ID {
			t.Fatalf("an event of another project reached a restricted member: %v", row)
		}
	}

	// /api/v1/studio/sources stays owner/admin-only; the readings behind it are scoped all the same.
	// Live values are readable by every member, narrowed by the database to the member's projects.
	code, liveOut := get(t, api, "/api/v1/live?range=1h", viewerAuth.AccessToken)
	if code != 200 {
		t.Fatalf("live must be readable by a viewer: %d", code)
	}
	liveGateways, _ := liveOut["gateways"].([]any)
	if len(liveGateways) != 1 || liveGateways[0].(map[string]any)["gateway"].(map[string]any)["id"] != gatewayA.ID {
		t.Fatalf("live data of another project reached a restricted member: %v", liveOut)
	}
	if code, _ = get(t, api, "/api/v1/studio/sources", viewerAuth.AccessToken); code != 403 {
		t.Fatalf("studio sources is owner/admin only: %d", code)
	}
	packets, e := f.repo.ListPackets(ctx, viewer, gatewayB.ID)
	if e != nil || len(packets) != 0 {
		t.Fatalf("raw packets of project B reached a restricted member: %d %v", len(packets), e)
	}
	history, e := f.repo.StreamHistory(ctx, viewer, gatewayB.ID, time.Now().Add(-time.Hour), 200)
	if e != nil || len(history) != 0 {
		t.Fatalf("decoded history of project B reached a restricted member: %d %v", len(history), e)
	}
	if own, e := f.repo.StreamHistory(ctx, viewer, gatewayA.ID, time.Now().Add(-time.Hour), 200); e != nil || len(own) == 0 {
		t.Fatalf("a restricted member must still read their own project's history: %d %v", len(own), e)
	}

	// A restricted member cannot move a gateway into or out of their scope, or touch another project.
	if e := f.repo.SetGatewayProject(ctx, viewer, gatewayA.ID, &projectB.ID); e == nil {
		t.Fatal("a viewer moved a gateway between projects")
	}
	code, _, _ = req(t, api, "POST", "/api/v1/gateways/"+gatewayB.ID+"/project", "Bearer "+viewerAuth.AccessToken, "", "", map[string]any{"project_id": projectA.ID})
	if code != 403 {
		t.Fatalf("a viewer changed a gateway's project over HTTP: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/projects/"+projectB.ID+"/update", "Bearer "+viewerAuth.AccessToken, "", "", map[string]any{"name": "ยึดโปรเจค", "color": "mint"})
	if code != 403 {
		t.Fatalf("a viewer edited a project outside their scope: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/gateways", "Bearer "+viewerAuth.AccessToken, "", "", map[string]any{"name": "GW ของฉัน", "model": "minew-mg3"})
	if code != 403 {
		t.Fatalf("a viewer created a gateway: %d", code)
	}

	// The owner's own view is unchanged by any of this.
	code, out = get(t, api, "/api/v1/gateways", ownerAuth.AccessToken)
	if code != 200 || len(rows(out)) != 3 {
		t.Fatalf("the owner must still see every gateway: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/sites", ownerAuth.AccessToken)
	if code != 200 || len(rows(out)) != 3 {
		t.Fatalf("the owner must still see every site: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/assets", ownerAuth.AccessToken)
	if code != 200 || len(rows(out)) != 5 {
		t.Fatalf("the owner must still see every asset: %d %v", code, out)
	}

	// Narrowing a member later takes effect on their next request, with no new session.
	openID := memberID(t, api, ownerAuth.AccessToken, unrestricted)
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+openID+"/update", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"role": "viewer", "project_ids": []string{projectB.ID}})
	if code != 204 {
		t.Fatalf("narrow a member: %d", code)
	}
	code, out = get(t, api, "/api/v1/gateways", openAuth.AccessToken)
	seen = values(out, "id")
	if code != 200 || len(seen) != 1 || !seen[gatewayB.ID] {
		t.Fatalf("a narrowed member must see project B only on the next request: %d %v", code, out)
	}
}

func TestMemberRolesAndProtections(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)

	adminEmail, viewerEmail, operatorEmail := memberEmail(), memberEmail(), memberEmail()
	addMember(t, api, ownerAuth.AccessToken, adminEmail, "admin", []string{})
	addMember(t, api, ownerAuth.AccessToken, viewerEmail, "viewer", []string{})
	addMember(t, api, ownerAuth.AccessToken, operatorEmail, "operator", []string{})

	adminAuth, _ := changeInitialPassword(t, f, api, adminEmail, owner.TenantID)
	viewerAuth, _ := changeInitialPassword(t, f, api, viewerEmail, owner.TenantID)
	operatorAuth, _ := changeInitialPassword(t, f, api, operatorEmail, owner.TenantID)

	// Every members route is closed to operator and viewer.
	for _, token := range []string{viewerAuth.AccessToken, operatorAuth.AccessToken} {
		if code, _ := get(t, api, "/api/v1/members", token); code != 403 {
			t.Fatalf("members list must be owner/admin only: %d", code)
		}
		for _, call := range [][2]any{
			{"/api/v1/members", map[string]any{"email": memberEmail(), "role": "viewer", "password": memberPassword, "project_ids": []string{}}},
			{"/api/v1/members/" + uuid.NewString() + "/update", map[string]any{"role": "viewer", "project_ids": []string{}}},
			{"/api/v1/members/" + uuid.NewString() + "/remove", map[string]any{}},
			{"/api/v1/members/" + uuid.NewString() + "/reset-password", map[string]any{"password": memberPassword}},
		} {
			code, _, _ := req(t, api, "POST", call[0].(string), "Bearer "+token, "", "", call[1])
			if code != 403 {
				t.Fatalf("%s must be owner/admin only: %d", call[0], code)
			}
		}
	}

	adminID := memberID(t, api, adminAuth.AccessToken, adminEmail)
	viewerID := memberID(t, api, adminAuth.AccessToken, viewerEmail)
	ownerUserID := owner.UserID

	// An admin can neither grant owner nor touch the owner.
	code, _, _ := req(t, api, "POST", "/api/v1/members", "Bearer "+adminAuth.AccessToken, "", "", map[string]any{"email": memberEmail(), "role": "owner", "password": memberPassword, "project_ids": []string{}})
	if code != 403 {
		t.Fatalf("an admin granted owner on creation: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+viewerID+"/update", "Bearer "+adminAuth.AccessToken, "", "", map[string]any{"role": "owner", "project_ids": []string{}})
	if code != 403 {
		t.Fatalf("an admin promoted somebody to owner: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+ownerUserID+"/remove", "Bearer "+adminAuth.AccessToken, "", "", map[string]any{})
	if code != 403 {
		t.Fatalf("an admin removed the owner: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+ownerUserID+"/reset-password", "Bearer "+adminAuth.AccessToken, "", "", map[string]any{"password": memberPassword})
	if code != 403 {
		t.Fatalf("an admin reset the owner's password: %d", code)
	}

	// The last owner can never be removed or demoted, not even by themselves.
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+ownerUserID+"/update", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"role": "admin", "project_ids": []string{}})
	if code != 403 {
		t.Fatalf("nobody edits their own membership: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+ownerUserID+"/remove", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{})
	if code != 403 {
		t.Fatalf("nobody removes their own membership: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+adminID+"/update", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"role": "owner", "project_ids": []string{}})
	if code != 204 {
		t.Fatalf("an owner may appoint a second owner: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+adminID+"/update", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"role": "viewer", "project_ids": []string{}})
	if code != 204 {
		t.Fatalf("an owner may demote the second owner: %d", code)
	}
	// The last-owner rule sits behind the self rule: an owner can only be demoted or removed by another
	// owner, so the only way to reach "no owner left" would be doing it to oneself, which is refused first.
	if e := f.repo.UpdateMember(ctx, owner, ownerUserID, "viewer", nil); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("self demotion: %v", e)
	}
	if e := f.repo.RemoveMember(ctx, owner, ownerUserID); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("self removal: %v", e)
	}

	// Removing a member ends their session: the access token they already hold stops working.
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+viewerID+"/remove", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{})
	if code != 204 {
		t.Fatalf("remove member: %d", code)
	}
	if code, _ := get(t, api, "/api/v1/gateways", viewerAuth.AccessToken); code != 401 {
		t.Fatalf("a removed member kept a working session: %d", code)
	}
	code, out := get(t, api, "/api/v1/members", ownerAuth.AccessToken)
	if code != 200 || len(rows(out)) != 3 {
		t.Fatalf("members after removal: %d %v", code, out)
	}

	// Handing the workspace over: a second owner is appointed, signs in, and demotes the founding owner.
	operatorID := memberID(t, api, ownerAuth.AccessToken, operatorEmail)
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+operatorID+"/update", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"role": "owner", "project_ids": []string{}})
	if code != 204 {
		t.Fatalf("appoint a second owner: %d", code)
	}
	successorAuth, successor := signIn(t, f, operatorEmail, chosenPassword, owner.TenantID)
	if successor.Role != "owner" {
		t.Fatalf("the successor's role: %q", successor.Role)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+ownerUserID+"/update", "Bearer "+successorAuth.AccessToken, "", "", map[string]any{"role": "viewer", "project_ids": []string{}})
	if code != 204 {
		t.Fatalf("the second owner must be able to demote the first: %d", code)
	}
	// The demotion applies to the token the founding owner already holds, with no new session.
	if code, _ := get(t, api, "/api/v1/members", ownerAuth.AccessToken); code != 403 {
		t.Fatalf("a demoted owner kept access to the members list: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+successor.UserID+"/update", "Bearer "+successorAuth.AccessToken, "", "", map[string]any{"role": "viewer", "project_ids": []string{}})
	if code != 403 {
		t.Fatalf("the only owner left demoted themselves: %d", code)
	}
	code, out = get(t, api, "/api/v1/members", successorAuth.AccessToken)
	left := 0
	for _, row := range rows(out) {
		if row["role"] == "owner" {
			left++
		}
	}
	if code != 200 || left != 1 {
		t.Fatalf("exactly one owner must be left: %d %d %v", code, left, out)
	}
}

func TestSharedIdentityAndPasswordChanges(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	other, _, _ := f.account(t) // somebody who already has an account in another organisation
	api := busyAPI(f)

	// An identity that still belongs to a workspace may not be attached to a second one. Login picks the
	// workspace automatically when an identity has exactly one membership, so linking would silently move
	// where a stranger's next login lands, and a distinguishable answer would make this route a
	// platform-wide "does this email have an account?" oracle. It is the same conflict a duplicate gets.
	payload := map[string]any{"email": other.User.Email, "role": "operator", "password": memberPassword, "project_ids": []string{}}
	code, out, _ := req(t, api, "POST", "/api/v1/members", "Bearer "+ownerAuth.AccessToken, "", "", payload)
	if code != 409 {
		t.Fatalf("an identity of another workspace was linked: %d %v", code, out)
	}
	if out["error"] != "conflict" || out["detail"] != nil {
		t.Fatalf("the refusal must not describe the identity: %v", out)
	}
	// Nothing happened: no membership here, and their own password still opens their own workspace.
	code, out = get(t, api, "/api/v1/members", ownerAuth.AccessToken)
	if code != 200 || len(rows(out)) != 1 {
		t.Fatalf("a refused link created a membership: %d %v", code, out)
	}
	if _, e := f.service.Login(ctx, other.User.Email, "correct horse battery staple", other.TenantID); e != nil {
		t.Fatalf("a refused link changed the other workspace's account: %v", e)
	}
	if _, e := f.service.Login(ctx, other.User.Email, memberPassword, other.TenantID); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("the supplied password was written to a foreign identity: %v", e)
	}
	if _, e := f.service.Login(ctx, other.User.Email, "correct horse battery staple", owner.TenantID); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("the foreign identity gained access to this workspace: %v", e)
	}

	// An identity that belongs to no workspace at all — somebody removed earlier — is adopted again, with
	// a fresh handed-over password that must be replaced like any other.
	returning := memberEmail()
	returningID := addMember(t, api, ownerAuth.AccessToken, returning, "viewer", []string{})
	changeInitialPassword(t, f, api, returning, owner.TenantID)
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+returningID+"/remove", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{})
	if code != 204 {
		t.Fatalf("remove the member who will come back: %d", code)
	}
	if again := addMember(t, api, ownerAuth.AccessToken, returning, "operator", []string{}); again != returningID {
		t.Fatalf("re-adding must adopt the identity left behind: %s vs %s", again, returningID)
	}
	if _, e := f.service.Login(ctx, returning, chosenPassword, owner.TenantID); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("a re-added member kept the password they had chosen before: %v", e)
	}
	back, _ := signIn(t, f, returning, memberPassword, owner.TenantID)
	if !back.MustChangePassword {
		t.Fatal("a re-added member must be asked to replace the handed-over password")
	}

	// The role of the TARGET is part of the SQL contract, not only of the Go path: an admin who reached
	// the database directly still cannot rewrite an owner's password or end an owner's sessions.
	adminEmail := memberEmail()
	adminID := addMember(t, api, ownerAuth.AccessToken, adminEmail, "admin", []string{})
	tx, e := f.runtime.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`SELECT set_config('app.user_id',$1,true),set_config('app.tenant_id',$2,true)`, adminID, owner.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`SELECT set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`); e != nil {
		t.Fatal(e)
	}
	var rewrote bool
	if e = tx.QueryRow(`SELECT identity.set_member_password($1,$2)`, owner.UserID, "$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA").Scan(&rewrote); e != nil {
		t.Fatal(e)
	}
	var logged int
	if e = tx.QueryRow(`SELECT identity.purge_tenant_sessions($1)`, owner.UserID).Scan(&logged); e != nil {
		t.Fatal(e)
	}
	tx.Rollback()
	if rewrote || logged != 0 {
		t.Fatalf("an admin reached an owner's credentials in SQL: rewrote=%v sessions=%d", rewrote, logged)
	}
	if code, _ := get(t, api, "/api/v1/members", ownerAuth.AccessToken); code != 200 {
		t.Fatalf("the owner's own session survived the attempt: %d", code)
	}

	// A member of this workspace only: reset works, flags the password and ends their sessions.
	soleEmail := memberEmail()
	soleID := addMember(t, api, ownerAuth.AccessToken, soleEmail, "viewer", []string{})
	soleAuth, _ := changeInitialPassword(t, f, api, soleEmail, owner.TenantID)
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+soleID+"/reset-password", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"password": "second handover pass"})
	if code != 204 {
		t.Fatalf("reset password: %d", code)
	}
	if code, _ := get(t, api, "/api/v1/me", soleAuth.AccessToken); code != 401 {
		t.Fatalf("a password reset must end the member's sessions: %d", code)
	}
	if _, e := f.service.Login(ctx, soleEmail, chosenPassword, owner.TenantID); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("the old password still works after a reset: %v", e)
	}
	resetAuth, _ := signIn(t, f, soleEmail, "second handover pass", owner.TenantID)
	if !resetAuth.MustChangePassword {
		t.Fatal("a reset password must be flagged for replacement")
	}
	if code, _ := get(t, api, "/api/v1/gateways", resetAuth.AccessToken); code != 403 {
		t.Fatalf("a reset password must close the API until it is replaced: %d", code)
	}

	// Changing one's own password: exact Origin, the current password, other sessions revoked.
	firstAuth, _ := signIn(t, f, soleEmail, "second handover pass", owner.TenantID)
	code, _, _ = req(t, api, "POST", "/api/v1/auth/password", "Bearer "+firstAuth.AccessToken, "", "", map[string]any{"current_password": "second handover pass", "new_password": "my own chosen pass"})
	if code != 403 {
		t.Fatalf("a credential mutation without the exact Origin: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/auth/password", "Bearer "+firstAuth.AccessToken, "", origin, map[string]any{"current_password": "wrong password here", "new_password": "my own chosen pass"})
	if code != 401 {
		t.Fatalf("a wrong current password: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/auth/password", "Bearer "+firstAuth.AccessToken, "", origin, map[string]any{"current_password": "second handover pass", "new_password": "short"})
	if code != 400 {
		t.Fatalf("a password below the 12 byte rule: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/auth/password", "Bearer "+firstAuth.AccessToken, "", origin, map[string]any{"current_password": "second handover pass", "new_password": "my own chosen pass"})
	if code != 204 {
		t.Fatalf("change own password: %d", code)
	}
	if code, _ := get(t, api, "/api/v1/me", resetAuth.AccessToken); code != 401 {
		t.Fatalf("the member's other sessions must be revoked: %d", code)
	}
	code, out = get(t, api, "/api/v1/me", firstAuth.AccessToken)
	if code != 200 || out["must_change_password"] != false {
		t.Fatalf("the change must clear must_change_password for the session that made it: %d %v", code, out)
	}
	if code, _ := get(t, api, "/api/v1/gateways", firstAuth.AccessToken); code != 200 {
		t.Fatalf("the API must open once the member chose their own password: %d", code)
	}
	finalAuth, _ := signIn(t, f, soleEmail, "my own chosen pass", owner.TenantID)
	if finalAuth.MustChangePassword {
		t.Fatal("must_change_password must be clear after the member chose their own password")
	}
}

func TestMembersAreIsolatedBetweenTenants(t *testing.T) {
	f := setup(t)
	_, authA, a := f.account(t)
	_, authB, _ := f.account(t)
	api := busyAPI(f)

	victimEmail := memberEmail()
	addMember(t, api, authA.AccessToken, victimEmail, "viewer", []string{})
	victimID := memberID(t, api, authA.AccessToken, victimEmail)

	// Tenant B's owner sees only their own workspace and cannot address tenant A's member.
	code, out := get(t, api, "/api/v1/members", authB.AccessToken)
	if code != 200 || len(rows(out)) != 1 {
		t.Fatalf("tenant B must list only its own members: %d %v", code, out)
	}
	if values(out, "email")[victimEmail] {
		t.Fatalf("tenant A's member leaked into tenant B's list: %v", out)
	}
	for _, call := range [][2]any{
		{"/api/v1/members/" + victimID + "/update", map[string]any{"role": "admin", "project_ids": []string{}}},
		{"/api/v1/members/" + victimID + "/remove", map[string]any{}},
		{"/api/v1/members/" + victimID + "/reset-password", map[string]any{"password": "cross tenant pass"}},
	} {
		code, _, _ := req(t, api, "POST", call[0].(string), "Bearer "+authB.AccessToken, "", "", call[1])
		if code != 404 {
			t.Fatalf("%s across tenants must not resolve: %d", call[0], code)
		}
	}
	// Tenant A is untouched and the member can still sign in.
	if _, e := f.service.Login(context.Background(), victimEmail, memberPassword, a.TenantID); e != nil {
		t.Fatalf("a cross-tenant attempt changed tenant A: %v", e)
	}
	code, out = get(t, api, "/api/v1/members", authA.AccessToken)
	if code != 200 || len(rows(out)) != 2 {
		t.Fatalf("tenant A's members after the cross-tenant attempts: %d %v", code, out)
	}
	for _, row := range rows(out) {
		if row["email"] == victimEmail && row["role"] != "viewer" {
			t.Fatalf("a cross-tenant update changed a role: %v", row)
		}
	}
}

// The constant half of every scope policy must reach the planner as an InitPlan: evaluated once per
// statement instead of once per row. Without it the ingest path pays a function call for every sample it
// writes, and a full scan pays one for every row it reads.
func TestScopePolicyEvaluatesTheConstantOncePerStatement(t *testing.T) {
	f := setup(t)
	_, _, owner := f.account(t)
	tx, e := f.runtime.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`SELECT set_config('app.user_id',$1,true),set_config('app.tenant_id',$2,true)`, owner.UserID, owner.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`SELECT set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"core.sensor_samples", "core.devices", "core.telemetry", "core.alerts"} {
		rows, e := tx.Query(`EXPLAIN SELECT count(*) FROM ` + table)
		if e != nil {
			t.Fatal(e)
		}
		plan := ""
		for rows.Next() {
			var line string
			if e := rows.Scan(&line); e != nil {
				t.Fatal(e)
			}
			plan += line + "\n"
		}
		if e := rows.Err(); e != nil {
			t.Fatal(e)
		}
		rows.Close()
		if !strings.Contains(plan, "InitPlan") {
			t.Fatalf("the scope constant is evaluated per row on %s:\n%s", table, plan)
		}
	}
}
