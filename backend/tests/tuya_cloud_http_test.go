package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/domain"
	"aether/backend/internal/tuyacloudlink/fakecloud"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// cloudProject is a fake Tuya project the service's credential check reaches by its Access ID.
func cloudProject(t *testing.T) (string, *fakecloud.API) {
	t.Helper()
	id := strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	api := fakecloud.NewAPI(t, id)
	cloudAPIs.Store(id, api)
	t.Cleanup(func() { cloudAPIs.Delete(id) })
	return id, api
}

// The Tuya Cloud routes: the status never carries a secret nor the Access ID beyond its hint, a viewer and a member
// without the connect module read but never change the link, the same project in another workspace is a bare
// conflict, unlink wipes the credentials, and with TUYA_CLOUD off link and sync are 404 while the status stays
// readable and unlink still cleans up.
func TestTuyaCloudHTTP(t *testing.T) {
	f := setup(t)
	enableCloud(f)
	api := busyAPI(f)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	g, _, e := f.service.CreateGateway(ctx, owner, "Cloud", domain.TuyaCloudGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	base := "/api/v1/gateways/" + g.ID + "/tuya-cloud"
	as := func(token, method, path string, payload any) (int, map[string]any, string) {
		t.Helper()
		code, out, _ := req(t, api, method, path, "Bearer "+token, "", "", payload)
		raw, _ := json.Marshal(out)
		return code, out, string(raw)
	}
	accessID, cloud := cloudProject(t)
	link := map[string]string{"region": "eu", "channel": "event", "access_id": accessID, "access_secret": cloudSecret}
	noSecrets := func(what, raw string) {
		t.Helper()
		if strings.Contains(raw, cloudSecret) || strings.Contains(raw, accessID) || strings.Contains(raw, accessID[:8]) ||
			strings.Contains(raw, "sealed") || strings.Contains(raw, "digest") {
			t.Fatalf("%s leaks a credential: %s", what, raw)
		}
	}

	// Not linked yet: readable, with this deployment's flag and budgets.
	code, out, _ := as(ownerAuth.AccessToken, "GET", base, nil)
	if code != 200 || out["linked"] != false || out["enabled"] != true || out["events_budget"] == nil || out["api_calls_budget"] == nil {
		t.Fatalf("status before linking: %d %v", code, out)
	}
	// Oversized, unknown fields and bad credentials are refused before anything is stored.
	big := map[string]string{"region": "eu", "channel": "event", "access_id": accessID, "access_secret": strings.Repeat("a", 1100)}
	if code, _, _ := as(ownerAuth.AccessToken, "POST", base+"/link", big); code != 400 {
		t.Fatalf("oversized body: %d", code)
	}
	if code, _, _ := as(ownerAuth.AccessToken, "POST", base+"/link", map[string]string{"region": "eu", "channel": "event", "access_id": accessID, "access_secret": cloudSecret, "extra": "x"}); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	if code, out, raw := as(ownerAuth.AccessToken, "POST", base+"/link", map[string]string{"region": "eu", "channel": "event", "access_id": accessID, "access_secret": "not a secret!"}); code != 400 || out["error"] != "tuya_credentials" {
		t.Fatalf("malformed credentials: %d %v", code, out)
	} else {
		noSecrets("malformed link", raw)
	}
	cloud.RefuseToken(true)
	if code, out, raw := as(ownerAuth.AccessToken, "POST", base+"/link", link); code != 400 || out["error"] != "tuya_auth_failed" {
		t.Fatalf("refused credentials: %d %v", code, out)
	} else {
		noSecrets("refused link", raw)
	}
	cloud.RefuseToken(false)
	if n := countRows(t, f, `SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=$1`, g.ID); n != 0 {
		t.Fatalf("refused credentials stored: %d", n)
	}

	// Link: the answer is the status, with a four-character hint and nothing else of the credentials.
	code, out, raw := as(ownerAuth.AccessToken, "POST", base+"/link", link)
	if code != 200 || out["linked"] != true || out["access_id_hint"] != accessID[:4] || out["region"] != "eu" || out["channel"] != "event" || out["linked_at"] == nil {
		t.Fatalf("link: %d %v", code, out)
	}
	noSecrets("link answer", raw)
	code, out, raw = as(ownerAuth.AccessToken, "GET", base, nil)
	if code != 200 || out["linked"] != true || out["state"] != "linking" || out["sync_requested_at"] == nil {
		t.Fatalf("status after linking: %d %v", code, out)
	}
	noSecrets("status", raw)
	if code, _, _ := as(ownerAuth.AccessToken, "POST", base+"/sync", nil); code != 202 {
		t.Fatalf("sync: %d", code)
	}

	// A viewer reads the status and changes nothing.
	viewerEmail := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, viewerEmail, "viewer", []string{})
	viewer, _ := changeInitialPassword(t, f, api, viewerEmail, owner.TenantID)
	if code, _, raw := as(viewer.AccessToken, "GET", base, nil); code != 200 {
		t.Fatalf("viewer status: %d", code)
	} else {
		noSecrets("viewer status", raw)
	}
	for _, path := range []string{"/link", "/sync", "/unlink"} {
		if code, _, _ := as(viewer.AccessToken, "POST", base+path, link); code != 403 {
			t.Fatalf("viewer %s: %d", path, code)
		}
	}
	// An administrator without the connect module reads the status and changes nothing.
	adminEmail := memberEmail()
	adminID := addMember(t, api, ownerAuth.AccessToken, adminEmail, "admin", []string{})
	admin, _ := changeInitialPassword(t, f, api, adminEmail, owner.TenantID)
	if code, _, _ := as(ownerAuth.AccessToken, "POST", "/api/v1/members/"+adminID+"/access", map[string]string{"connect": "none"}); code != 204 {
		t.Fatalf("restrict the administrator: %d", code)
	}
	if code, _, _ := as(admin.AccessToken, "GET", base, nil); code != 200 {
		t.Fatalf("status without the connect module: %d", code)
	}
	for _, path := range []string{"/link", "/sync", "/unlink"} {
		if code, _, _ := as(admin.AccessToken, "POST", base+path, link); code != 403 {
			t.Fatalf("%s without the connect module: %d", path, code)
		}
	}
	if code, _, _ := as(ownerAuth.AccessToken, "POST", "/api/v1/members/"+adminID+"/access", map[string]string{"connect": "write"}); code != 204 {
		t.Fatalf("give the administrator the connect module: %d", code)
	}
	if code, _, _ := as(admin.AccessToken, "POST", base+"/sync", nil); code != 202 {
		t.Fatalf("administrator with the connect module: %d", code)
	}

	// The same project in another workspace: 409 already_linked, and nothing about the workspace that holds it.
	_, otherAuth, other := f.account(t)
	og, _, e := f.service.CreateGateway(ctx, other, "Theirs", domain.TuyaCloudGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	code, out, raw = as(otherAuth.AccessToken, "POST", "/api/v1/gateways/"+og.ID+"/tuya-cloud/link", link)
	if code != 409 || out["error"] != "already_linked" {
		t.Fatalf("same project in another workspace: %d %v", code, out)
	}
	for _, leak := range []string{owner.TenantID, g.ID, "Cloud\"", accessID} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the conflict reveals %q: %s", leak, raw)
		}
	}
	// Nor can the other workspace read or change this link.
	if code, _, _ := as(otherAuth.AccessToken, "GET", base, nil); code != 404 {
		t.Fatalf("another workspace reads the status: %d", code)
	}
	if code, _, _ := as(otherAuth.AccessToken, "POST", base+"/unlink", nil); code != 404 {
		t.Fatalf("another workspace unlinks: %d", code)
	}

	// Unlink wipes the credentials; the status says so and still carries no secret.
	if code, _, _ := as(ownerAuth.AccessToken, "POST", base+"/unlink", nil); code != 204 {
		t.Fatalf("unlink: %d", code)
	}
	if n := countRows(t, f, `SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=$1 AND credentials_sealed IS NULL AND access_id_digest IS NULL AND state='disabled'`, g.ID); n != 1 {
		t.Fatal("unlink kept the credentials")
	}
	if code, out, _ := as(ownerAuth.AccessToken, "GET", base, nil); code != 200 || out["linked"] != false {
		t.Fatalf("status after unlink: %d %v", code, out)
	}
	// The project is free again for the other workspace.
	if code, out, _ := as(otherAuth.AccessToken, "POST", "/api/v1/gateways/"+og.ID+"/tuya-cloud/link", link); code != 200 || out["linked"] != true {
		t.Fatalf("link after the unlink: %d %v", code, out)
	}

	// TUYA_CLOUD off: link and sync do not exist, the status stays readable, unlink still wipes.
	f.service.TuyaCloudEnabled = false
	api = busyAPI(f)
	for _, path := range []string{"/link", "/sync"} {
		if code, _, _ := as(otherAuth.AccessToken, "POST", "/api/v1/gateways/"+og.ID+"/tuya-cloud"+path, link); code != 404 {
			t.Fatalf("%s with the flag off: %d", path, code)
		}
	}
	if code, out, _ := as(otherAuth.AccessToken, "GET", "/api/v1/gateways/"+og.ID+"/tuya-cloud", nil); code != 200 || out["enabled"] != false || out["linked"] != true {
		t.Fatalf("status with the flag off: %d %v", code, out)
	}
	if code, _, _ := as(otherAuth.AccessToken, "POST", "/api/v1/gateways/"+og.ID+"/tuya-cloud/unlink", nil); code != 204 {
		t.Fatalf("unlink with the flag off: %d", code)
	}
	if n := countRows(t, f, `SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=$1 AND credentials_sealed IS NULL`, og.ID); n != 1 {
		t.Fatal("unlink with the flag off kept the credentials")
	}
	// A gateway of another model has no cloud link.
	edge, _, e := f.service.CreateGateway(ctx, owner, "Edge", domain.EdgeGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	if code, out, _ := as(ownerAuth.AccessToken, "GET", "/api/v1/gateways/"+edge.ID+"/tuya-cloud", nil); code != 400 || out["error"] != "not_a_cloud_gateway" {
		t.Fatalf("status of an Edge gateway: %d %v", code, out)
	}
}

// With TUYA_CLOUD off (RefuseTuyaCloud) a registration with the cloud profile can be neither restored nor moved;
// renaming it still works. With the mode on both work, and a move still has to fit the target gateway.
func TestTuyaCloudRegistrationGuard(t *testing.T) {
	f := setup(t)
	enableCloud(f)
	api := busyAPI(f)
	ctx := context.Background()
	_, auth, owner := f.account(t)
	one, _, e := f.service.CreateGateway(ctx, owner, "Cloud one", domain.TuyaCloudGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	two, _, e := f.service.CreateGateway(ctx, owner, "Cloud two", domain.TuyaCloudGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	mg3, _, e := f.service.CreateGateway(ctx, owner, "BLE", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	d, e := f.service.CreateDevice(ctx, owner, one.ID, "Hall switch", cloudSwitch, domain.TuyaCloudProfile)
	if e != nil {
		t.Fatal(e)
	}
	post := func(path string, payload any) (int, map[string]any) {
		t.Helper()
		code, out, _ := req(t, api, "POST", "/api/v1/devices/"+d.ID+path, "Bearer "+auth.AccessToken, "", "", payload)
		return code, out
	}
	// The cloud profile never fits a BLE gateway.
	if code, _ := post("/update", map[string]string{"gateway_id": mg3.ID}); code != 400 {
		t.Fatalf("cloud device moved to a BLE gateway: %d", code)
	}
	if code, _ := post("/remove", nil); code != 204 {
		t.Fatalf("remove: %d", code)
	}

	f.repo.Configure(postgres.Options{RefuseTuyaCloud: true, DiscoveryLimit: 100})
	if code, out := post("/restore", nil); code != 400 || out["error"] != "tuya_cloud_disabled" {
		t.Fatalf("restore with the mode off: %d %v", code, out)
	}
	f.repo.Configure(postgres.Options{DiscoveryLimit: 100})
	if code, _ := post("/restore", nil); code != 204 {
		t.Fatalf("restore with the mode on: %d", code)
	}
	f.repo.Configure(postgres.Options{RefuseTuyaCloud: true, DiscoveryLimit: 100})
	if code, out := post("/update", map[string]string{"gateway_id": two.ID}); code != 400 || out["error"] != "tuya_cloud_disabled" {
		t.Fatalf("move with the mode off: %d %v", code, out)
	}
	if code, out := post("/update", map[string]string{"name": "Hall"}); code != 200 || out["name"] != "Hall" {
		t.Fatalf("rename with the mode off: %d %v", code, out)
	}
	f.repo.Configure(postgres.Options{DiscoveryLimit: 100})
	if code, out := post("/update", map[string]string{"gateway_id": two.ID}); code != 200 || out["gateway_id"] != two.ID {
		t.Fatalf("move with the mode on: %d %v", code, out)
	}
}
