package tests

import (
	"context"
	"testing"
)

func TestModuleRestrictionsAndScopedAdministrator(t *testing.T) {
	f := setup(t)
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)
	ctx := context.Background()
	project, e := f.service.CreateProject(ctx, owner, "Customer A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	other, e := f.service.CreateProject(ctx, owner, "Customer B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = f.service.CreateGatewayIn(ctx, owner, "A", "minew-mg3", &project.ID)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = f.service.CreateGatewayIn(ctx, owner, "B", "minew-mg3", &other.ID)
	if e != nil {
		t.Fatal(e)
	}
	email := memberEmail()
	id := addMember(t, api, ownerAuth.AccessToken, email, "admin", []string{project.ID})
	auth, p := changeInitialPassword(t, f, api, email, owner.TenantID)
	items, e := f.repo.ListGateways(ctx, p)
	if e != nil || len(items) != 1 || items[0].Name != "A" {
		t.Fatalf("project isolation: %v %v", items, e)
	}
	code, _, _ := req(t, api, "GET", "/api/v1/members", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 403 {
		t.Fatalf("scoped administrator may manage members: %d", code)
	}
	code, out, _ := req(t, api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+ownerAuth.AccessToken, "", "", map[string]string{"floorplan": "none", "automation": "read"})
	if code != 204 {
		t.Fatal(code, out)
	}
	code, _, _ = req(t, api, "GET", "/api/v1/sites", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 403 {
		t.Fatalf("hidden module accessible: %d", code)
	}
	code, _, _ = req(t, api, "GET", "/api/v1/automations", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("read module denied: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/automations", "Bearer "+auth.AccessToken, "", "", map[string]any{"name": "forbidden"})
	if code != 403 {
		t.Fatalf("read-only write accepted: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+ownerAuth.AccessToken, "", "", map[string]string{"floorplan": "write"})
	if code != 204 {
		t.Fatal(code)
	}
	code, _, _ = req(t, api, "GET", "/api/v1/sites", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("updated permission not effective on existing session: %d", code)
	}
}
