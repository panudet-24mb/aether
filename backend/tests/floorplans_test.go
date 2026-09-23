package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"context"
	"encoding/base64"
	"testing"
)

// 1x1 transparent PNG.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func TestFloorPlansAreTenantScopedAndRevisioned(t *testing.T) {
	f := setup(t)
	_, authA, a := f.account(t)
	_, authB, _ := f.account(t)
	ctx := context.Background()
	api := httpapi.New(f.cfg, f.service, f.repo)
	A, B := "Bearer "+authA.AccessToken, "Bearer "+authB.AccessToken
	g, _, e := f.service.CreateGateway(ctx, a, "Ward gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	code, site, _ := req(t, api, "POST", "/api/v1/sites", A, "", "", map[string]any{"name": "อาคารผู้ป่วยใน", "description": ""})
	if code != 201 || len(site["floors"].([]any)) != 1 {
		t.Fatalf("create site: %d %v", code, site)
	}
	siteID := site["id"].(string)
	floor := site["floors"].([]any)[0].(map[string]any)
	floorID := floor["id"].(string)

	layout := map[string]any{
		"walls": []any{map[string]any{"id": "w1", "points": []any{[]float64{0, 0}, []float64{10, 0}, []float64{10, 8}}, "thickness": 0.2}},
		"zones": []any{map[string]any{"id": "z1", "name": "Ward 4A", "kind": "ward", "color": "blue", "points": []any{[]float64{0, 0}, []float64{10, 0}, []float64{10, 8}, []float64{0, 8}}, "gateway_ids": []string{g.ID}}},
		"items": []any{map[string]any{"id": "i1", "type": "door", "x": 5, "y": 0, "w": 1, "h": 0.2, "rot": 0}},
	}
	save := func(token string, revision int, placements []any, l map[string]any) (int, map[string]any) {
		code, out, _ := req(t, api, "POST", "/api/v1/floors/"+floorID+"/save", token, "", "", map[string]any{"name": "ชั้น 1", "level": 1, "width_m": 30, "depth_m": 20, "ceiling_m": 3, "revision": revision, "layout": l, "placements": placements})
		return code, out
	}
	placed := []any{map[string]any{"asset_kind": "gateway", "asset_id": g.ID, "x": 5, "y": 4, "z": 2.4}}
	if code, out := save(B, 1, placed, layout); code != 404 {
		t.Fatalf("another tenant saved the floor: %d %v", code, out)
	}
	code, out := save(A, 1, placed, layout)
	if code != 200 || out["revision"].(float64) != 2 {
		t.Fatalf("save: %d %v", code, out)
	}
	if code, _ := save(A, 1, placed, layout); code != 409 {
		t.Fatalf("stale revision must conflict: %d", code)
	}
	// A gateway of another tenant cannot be placed.
	_, _, pb := f.account(t)
	foreign, _, e := f.service.CreateGateway(ctx, pb, "Foreign", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if code, _ := save(A, 2, []any{map[string]any{"asset_kind": "gateway", "asset_id": foreign.ID, "x": 1, "y": 1, "z": 2}}, layout); code != 404 {
		t.Fatalf("foreign gateway placed: %d", code)
	}
	bad := map[string]any{"walls": []any{}, "zones": []any{map[string]any{"id": "z1", "name": "x", "kind": "nope", "color": "blue", "points": []any{[]float64{0, 0}, []float64{1, 0}, []float64{1, 1}}}}, "items": []any{}}
	if code, _ := save(A, 2, placed, bad); code != 400 {
		t.Fatalf("invalid zone kind accepted: %d", code)
	}
	code, full, _ := req(t, api, "GET", "/api/v1/sites/"+siteID, A, "", "", nil)
	if code != 200 {
		t.Fatalf("get site: %d", code)
	}
	fl := full["floors"].([]any)[0].(map[string]any)
	if len(fl["placements"].([]any)) != 1 || len(fl["layout"].(map[string]any)["zones"].([]any)) != 1 || fl["revision"].(float64) != 2 {
		t.Fatalf("floor content: %v", fl)
	}
	if code, _, _ := req(t, api, "GET", "/api/v1/sites/"+siteID, B, "", "", nil); code != 404 {
		t.Fatalf("site leaked across tenants: %d", code)
	}
	code, list, _ := req(t, api, "GET", "/api/v1/sites", B, "", "", nil)
	if code != 200 || len(list["items"].([]any)) != 0 {
		t.Fatalf("site list leaked: %d %v", code, list)
	}
	// Image: type comes from the bytes; text is refused.
	if code, _, _ := req(t, api, "POST", "/api/v1/floors/"+floorID+"/image", A, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString([]byte("<svg onload=alert(1)>"))}); code != 400 {
		t.Fatalf("non-image accepted: %d", code)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/floors/"+floorID+"/image", A, "", "", map[string]any{"data_base64": tinyPNG}); code != 204 {
		t.Fatalf("image upload: %d", code)
	}
	if code, _, _ := req(t, api, "GET", "/api/v1/floors/"+floorID+"/image", B, "", "", nil); code != 404 {
		t.Fatalf("image leaked across tenants: %d", code)
	}
	// A second floor takes the gateway: an asset stands in one place.
	code, second, _ := req(t, api, "POST", "/api/v1/sites/"+siteID+"/floors", A, "", "", map[string]any{"name": "ชั้น 2", "level": 2, "width_m": 30, "depth_m": 20, "ceiling_m": 3, "revision": 0, "layout": map[string]any{"walls": []any{}, "zones": []any{}, "items": []any{}}, "placements": []any{}})
	if code != 201 {
		t.Fatalf("create floor: %d %v", code, second)
	}
	code, moved, _ := req(t, api, "POST", "/api/v1/floors/"+second["id"].(string)+"/save", A, "", "", map[string]any{"name": "ชั้น 2", "level": 2, "width_m": 30, "depth_m": 20, "ceiling_m": 3, "revision": 1, "layout": map[string]any{"walls": []any{}, "zones": []any{}, "items": []any{}}, "placements": placed})
	if code != 200 {
		t.Fatalf("move placement: %d", code)
	}
	// The floor that lost the gateway gets a new revision, so a stale draft of it cannot put the gateway back.
	if bumped, _ := moved["bumped"].(map[string]any); bumped[floorID] != float64(3) {
		t.Fatalf("losing floor must be bumped to revision 3: %v", moved)
	}
	if code, _ := save(A, 2, placed, layout); code != 409 {
		t.Fatalf("stale draft of the losing floor must conflict: %d", code)
	}
	_, full, _ = req(t, api, "GET", "/api/v1/sites/"+siteID, A, "", "", nil)
	counts := []int{}
	for _, x := range full["floors"].([]any) {
		counts = append(counts, len(x.(map[string]any)["placements"].([]any)))
	}
	if len(counts) != 2 || counts[0] != 0 || counts[1] != 1 {
		t.Fatalf("gateway must have moved to floor 2: %v", counts)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/sites/"+siteID+"/archive", A, "", "", map[string]any{}); code != 204 {
		t.Fatalf("archive: %d", code)
	}
}

func TestFloorPlansHaveIndependentProjectLayouts(t *testing.T) {
	f := setup(t)
	_, auth, owner := f.account(t)
	api := busyAPI(f)
	ctx := context.Background()
	token := "Bearer " + auth.AccessToken
	a, e := f.service.CreateProject(ctx, owner, "Building A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	b, e := f.service.CreateProject(ctx, owner, "Building B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	gateway, _, e := f.service.CreateGatewayIn(ctx, owner, "B gateway", "minew-mg3", &b.ID)
	if e != nil {
		t.Fatal(e)
	}
	sites := []map[string]any{}
	for _, id := range []string{a.ID, b.ID} {
		code, site, _ := req(t, api, "POST", "/api/v1/sites", token, "", "", map[string]any{"name": "Building", "project_id": id})
		if code != 201 {
			t.Fatalf("create project site: %d %v", code, site)
		}
		sites = append(sites, site)
	}
	floor := sites[0]["floors"].([]any)[0].(map[string]any)
	layout := map[string]any{"walls": []any{map[string]any{"id": "wall-a", "points": []any{[]float64{0, 0}, []float64{10, 0}}, "thickness": 0.2}}, "zones": []any{}, "items": []any{}}
	body := map[string]any{"name": "A only", "level": 1, "width_m": 30, "depth_m": 20, "ceiling_m": 3, "revision": 1, "layout": layout, "placements": []any{}}
	code, out, _ := req(t, api, "POST", "/api/v1/floors/"+floor["id"].(string)+"/save", token, "", "", body)
	if code != 200 {
		t.Fatalf("save A: %d %v", code, out)
	}
	body["revision"] = 2
	body["placements"] = []any{map[string]any{"asset_kind": "gateway", "asset_id": gateway.ID, "x": 1, "y": 1, "z": 1}}
	code, _, _ = req(t, api, "POST", "/api/v1/floors/"+floor["id"].(string)+"/save", token, "", "", body)
	if code != 404 {
		t.Fatalf("cross-project placement accepted: %d", code)
	}
	for i, site := range sites {
		code, out, _ := req(t, api, "GET", "/api/v1/sites/"+site["id"].(string), token, "", "", nil)
		if code != 200 {
			t.Fatal(code)
		}
		walls := out["floors"].([]any)[0].(map[string]any)["layout"].(map[string]any)["walls"].([]any)
		expected := 1 - i
		if len(walls) != expected {
			t.Fatalf("project %d: expected %d walls, got %d", i, expected, len(walls))
		}
	}
	email := memberEmail()
	addMember(t, api, auth.AccessToken, email, "viewer", []string{a.ID})
	limited, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
	code, _, _ = req(t, api, "GET", "/api/v1/sites/"+sites[1]["id"].(string), "Bearer "+limited.AccessToken, "", "", nil)
	if code != 404 {
		t.Fatalf("project B layout visible to A member: %d", code)
	}
}
