package tests

import (
	"context"
	"testing"
)

// Frames on the connect canvas (migration 00052): drawn by owner and admin, scoped to a project like gateways are,
// and nothing but a labelled rectangle (no node is stored with it).

func frameItems(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	list, ok := out["items"].([]any)
	if !ok {
		t.Fatalf("no items in %v", out)
	}
	byName := map[string]map[string]any{}
	for _, v := range list {
		f := v.(map[string]any)
		byName[f["name"].(string)] = f
	}
	return byName
}

func frameBody(name, color string, project any, x, y, w, h float64) map[string]any {
	return map[string]any{"name": name, "color": color, "project_id": project, "x": x, "y": y, "w": w, "h": h}
}

func TestTopologyFramesAreScopedByProjectAndRole(t *testing.T) {
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
	post := func(token, path string, body any) (int, map[string]any) {
		t.Helper()
		code, out, _ := req(t, api, "POST", path, "Bearer "+token, "", "", body)
		return code, out
	}

	// Nothing drawn yet.
	code, out := get(t, api, "/api/v1/topology/frames", ownerAuth.AccessToken)
	if code != 200 || len(frameItems(t, out)) != 0 {
		t.Fatalf("empty frames: %d %v", code, out)
	}

	// The owner draws one frame per project and one without a project.
	code, out = post(ownerAuth.AccessToken, "/api/v1/topology/frames", frameBody(" ห้องน้ำชั้น M ", "amber", projectA.ID, 10.004, 20, 300, 200))
	if code != 201 || out["name"] != "ห้องน้ำชั้น M" || out["project_id"] != projectA.ID || out["x"] != 10.0 || out["view"] != "connect" {
		t.Fatalf("create in A: %d %v", code, out)
	}
	frameA := out["id"].(string)
	if code, out = post(ownerAuth.AccessToken, "/api/v1/topology/frames", frameBody("กลุ่ม B", "blue", projectB.ID, 0, 0, 100, 100)); code != 201 {
		t.Fatalf("create in B: %d %v", code, out)
	}
	if code, out = post(ownerAuth.AccessToken, "/api/v1/topology/frames", frameBody("ส่วนกลาง", "slate", nil, 0, 0, 100, 100)); code != 201 || out["project_id"] != nil {
		t.Fatalf("create without project: %d %v", code, out)
	}
	frameNone := out["id"].(string)

	// Invalid frames are refused: unknown colour, too small, no name, missing size, an archived or unknown project.
	for _, body := range []map[string]any{
		frameBody("x", "pink", nil, 0, 0, 100, 100),
		frameBody("x", "mint", nil, 0, 0, 39, 100),
		frameBody("  ", "mint", nil, 0, 0, 100, 100),
		{"name": "x", "color": "mint", "x": 0, "y": 0, "w": 100},
		frameBody("x", "mint", "00000000-0000-4000-8000-000000000000", 0, 0, 100, 100),
		frameBody("x", "mint", "not-a-uuid", 0, 0, 100, 100),
	} {
		if code, out = post(ownerAuth.AccessToken, "/api/v1/topology/frames", body); code != 400 {
			t.Fatalf("invalid frame %v: %d %v", body, code, out)
		}
	}

	// Members: an admin restricted to project A, an unrestricted operator and an unrestricted viewer.
	adminEmail, operatorEmail, viewerEmail := memberEmail(), memberEmail(), memberEmail()
	addMember(t, api, ownerAuth.AccessToken, adminEmail, "admin", []string{projectA.ID})
	addMember(t, api, ownerAuth.AccessToken, operatorEmail, "operator", []string{})
	addMember(t, api, ownerAuth.AccessToken, viewerEmail, "viewer", []string{})
	adminAuth, _ := changeInitialPassword(t, f, api, adminEmail, owner.TenantID)
	operatorAuth, _ := changeInitialPassword(t, f, api, operatorEmail, owner.TenantID)
	viewerAuth, _ := changeInitialPassword(t, f, api, viewerEmail, owner.TenantID)

	// The restricted admin sees project A's frame only.
	code, out = get(t, api, "/api/v1/topology/frames", adminAuth.AccessToken)
	if items := frameItems(t, out); code != 200 || len(items) != 1 || items["ห้องน้ำชั้น M"] == nil {
		t.Fatalf("restricted admin read: %d %v", code, out)
	}
	// It draws in its own project, but not without a project or in another one.
	if code, out = post(adminAuth.AccessToken, "/api/v1/topology/frames", frameBody("ของ admin", "violet", projectA.ID, 0, 0, 100, 100)); code != 201 {
		t.Fatalf("restricted admin create in A: %d %v", code, out)
	}
	if code, out = post(adminAuth.AccessToken, "/api/v1/topology/frames", frameBody("x", "mint", nil, 0, 0, 100, 100)); code != 403 {
		t.Fatalf("restricted admin create without project: %d %v", code, out)
	}
	if code, out = post(adminAuth.AccessToken, "/api/v1/topology/frames", frameBody("x", "mint", projectB.ID, 0, 0, 100, 100)); code != 400 {
		t.Fatalf("restricted admin create in B (invisible project): %d %v", code, out)
	}
	// It moves project A's frame, and cannot reach the frame without a project.
	code, out = post(adminAuth.AccessToken, "/api/v1/topology/frames/"+frameA+"/update", frameBody("ห้องน้ำชั้น M", "coral", nil, 50, 60, 320, 210))
	if code != 200 || out["x"] != 50.0 || out["color"] != "coral" || out["project_id"] != projectA.ID {
		t.Fatalf("restricted admin update (project unchanged): %d %v", code, out)
	}
	if code, out = post(adminAuth.AccessToken, "/api/v1/topology/frames/"+frameNone+"/update", frameBody("x", "mint", nil, 0, 0, 100, 100)); code != 404 {
		t.Fatalf("restricted admin update outside scope: %d %v", code, out)
	}
	if code, out = post(adminAuth.AccessToken, "/api/v1/topology/frames/"+frameNone+"/delete", nil); code != 404 {
		t.Fatalf("restricted admin delete outside scope: %d %v", code, out)
	}

	// Operators and viewers read frames but never draw them.
	if code, out = get(t, api, "/api/v1/topology/frames", viewerAuth.AccessToken); code != 200 || len(frameItems(t, out)) != 4 {
		t.Fatalf("viewer read: %d %v", code, out)
	}
	for _, token := range []string{operatorAuth.AccessToken, viewerAuth.AccessToken} {
		if code, out = post(token, "/api/v1/topology/frames", frameBody("x", "mint", nil, 0, 0, 100, 100)); code != 403 {
			t.Fatalf("non-admin create: %d %v", code, out)
		}
		if code, out = post(token, "/api/v1/topology/frames/"+frameA+"/update", frameBody("x", "mint", nil, 0, 0, 100, 100)); code != 403 {
			t.Fatalf("non-admin update: %d %v", code, out)
		}
		if code, out = post(token, "/api/v1/topology/frames/"+frameA+"/delete", nil); code != 403 {
			t.Fatalf("non-admin delete: %d %v", code, out)
		}
	}

	// Deleting a frame leaves the layout untouched; a frame deleted twice is gone.
	if code, out = post(ownerAuth.AccessToken, "/api/v1/topology/frames/"+frameNone+"/delete", nil); code != 204 {
		t.Fatalf("owner delete: %d %v", code, out)
	}
	if code, out = post(ownerAuth.AccessToken, "/api/v1/topology/frames/"+frameNone+"/delete", nil); code != 404 {
		t.Fatalf("second delete: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/topology/frames", ownerAuth.AccessToken)
	if items := frameItems(t, out); code != 200 || len(items) != 3 || items["ส่วนกลาง"] != nil {
		t.Fatalf("after delete: %d %v", code, out)
	}
	if code, out = get(t, api, "/api/v1/topology/layout", ownerAuth.AccessToken); code != 200 || out["version"] != 0.0 {
		t.Fatalf("frames never touch the layout version: %d %v", code, out)
	}
}
