package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// The gateway page reads an Aether Edge's state and counts; nothing secret, and only for a live edge gateway the
// caller can see.
func TestEdgeStatus(t *testing.T) {
	r := newEdgeRig(t, nil)
	get := func(token, gateway string) (int, map[string]any) {
		t.Helper()
		q := httptest.NewRequest("GET", "/api/v1/gateways/"+gateway+"/edge/status", nil)
		q.Header.Set("Authorization", "Bearer "+token)
		res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	// Never connected: empty state, the imported devices counted, all keys ok.
	code, out := get(r.ownerAuth.AccessToken, r.gateway)
	if code != 200 || out["state"] != "" || out["imported"] != float64(len(edgeDevices)) || out["latest_version"] != domain.EdgeImageTag {
		t.Fatalf("fresh status: %d %v", code, out)
	}
	if keys, _ := out["keys"].(map[string]any); keys["ok"] != float64(len(edgeDevices)) || keys["missing"] != float64(0) {
		t.Fatalf("keys: %v", out["keys"])
	}
	r.register("switch")
	r.must(r.capture("status", `{"state":"online"}`))
	r.must(r.capture("health", `{"version":"0.1.1","devices_connected":1,"lan_seen":4}`))
	code, out = get(r.ownerAuth.AccessToken, r.gateway)
	if code != 200 || out["state"] != "online" || out["version"] != "0.1.1" || out["devices_connected"] != float64(1) || out["lan_seen"] != float64(4) ||
		out["registered"] != float64(1) || out["last_health_at"] == nil {
		t.Fatalf("online status: %d %v", code, out)
	}
	for _, secret := range []string{"sealed-test-key", "local_key", "key_sealed"} {
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), secret) {
			t.Fatalf("status leaks %q", secret)
		}
	}
	// Not an edge gateway, another workspace, a malformed id: never answered.
	z, _, e := r.f.service.CreateGateway(r.ctx, r.owner, "Zigbee", domain.Z2MGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	if code, _ := get(r.ownerAuth.AccessToken, z.ID); code != 404 {
		t.Fatalf("zigbee gateway: %d", code)
	}
	_, other, _ := r.f.account(t)
	if code, _ := get(other.AccessToken, r.gateway); code != 404 {
		t.Fatalf("other workspace: %d", code)
	}
	if code, _ := get(r.ownerAuth.AccessToken, "not-a-uuid"); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
}

// Scope: the status answers exactly the members who can see the gateway's project (a viewer and an operator of that
// project), and 404 to a member scoped to another project. "registered" counts a Tuya id registered anywhere in the
// workspace, as discovery does, so a device adopted under another Edge is not shown as waiting for registration.
func TestEdgeStatusScopeAndRegistration(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)
	projectA, e := f.service.CreateProject(ctx, owner, "A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	projectB, e := f.service.CreateProject(ctx, owner, "B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	g, _, e := f.service.CreateGatewayIn(ctx, owner, "Edge B", domain.EdgeGatewayModel, &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	other, _, e := f.service.CreateGatewayIn(ctx, owner, "Edge B2", domain.EdgeGatewayModel, &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile("../internal/adapters/tuya/testdata/kg_3gang.json")
	dps, category, e := tuya.ParseSpecifications(raw)
	if e != nil {
		t.Fatal(e)
	}
	ids := []string{"bf00000000000000st01", "bf00000000000000st02"}
	imports := []postgres.TuyaImport{}
	for i, id := range ids {
		imports = append(imports, postgres.TuyaImport{TuyaID: id, Name: fmt.Sprintf("switch %d", i), Category: category, Spec: dps, LocalKeySealed: "sealed", KeyFingerprint: "00"})
	}
	if _, e := f.repo.SaveTuyaDevices(ctx, owner, g.ID, imports); e != nil {
		t.Fatal(e)
	}
	// The first device is registered under the other Edge: still "registered" for this one.
	if _, e := f.service.CreateDevice(ctx, owner, other.ID, "switch elsewhere", ids[0], domain.TuyaWiFiProfile); e != nil {
		t.Fatal(e)
	}
	path := "/api/v1/gateways/" + g.ID + "/edge/status"
	code, out := get(t, api, path, ownerAuth.AccessToken)
	if code != 200 || out["imported"] != float64(2) || out["registered"] != float64(1) {
		t.Fatalf("owner status: %d %v", code, out)
	}
	member := func(role string, projects []string) string {
		email := memberEmail()
		addMember(t, api, ownerAuth.AccessToken, email, role, projects)
		auth, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
		return auth.AccessToken
	}
	if code, out := get(t, api, path, member("operator", []string{projectA.ID})); code != 404 {
		t.Fatalf("operator of another project: %d %v", code, out)
	}
	for _, role := range []string{"viewer", "operator"} {
		if code, out := get(t, api, path, member(role, []string{projectB.ID})); code != 200 || out["imported"] != float64(2) {
			t.Fatalf("%s of the gateway's project: %d %v", role, code, out)
		}
	}
}
