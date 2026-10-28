package tests

import (
	"context"
	"testing"
)

// Routes of connection lines on the connect canvas (migration 00053): bend points per stable line (gateway to broker,
// registration to gateway), scoped by the line's gateway like node positions, and read back only while the line is
// still the one they were drawn for.

func routesOf(t *testing.T, out map[string]any) map[string][]any {
	t.Helper()
	list, ok := out["items"].([]any)
	if !ok {
		t.Fatalf("no items in %v", out)
	}
	byEdge := map[string][]any{}
	for _, v := range list {
		r := v.(map[string]any)
		byEdge[r["edge"].(string)], _ = r["points"].([]any)
	}
	return byEdge
}

func TestTopologyRoutesAreScopedAndFollowTheLine(t *testing.T) {
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
	gwA, _, e := f.service.CreateGatewayIn(ctx, owner, "GW A", "minew-mg3", &projectA.ID)
	if e != nil {
		t.Fatal(e)
	}
	gwA2, _, e := f.service.CreateGatewayIn(ctx, owner, "GW A2", "minew-mg3", &projectA.ID)
	if e != nil {
		t.Fatal(e)
	}
	gwB, _, e := f.service.CreateGatewayIn(ctx, owner, "GW B", "minew-mg3", &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	devA, e := f.service.CreateDevice(ctx, owner, gwA.ID, "Dev A", "fb0000000001", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	linkA, linkB, adoptA := "link:"+gwA.ID, "link:"+gwB.ID, "adopt:"+devA.ID
	save := func(token string, body map[string]any) int {
		t.Helper()
		code, _, _ := req(t, api, "POST", "/api/v1/topology/routes", "Bearer "+token, "", "", body)
		return code
	}
	pts := func(xy ...float64) []map[string]any {
		out := []map[string]any{}
		for i := 0; i+1 < len(xy); i += 2 {
			out = append(out, pos(xy[i], xy[i+1]))
		}
		return out
	}

	// The owner routes three lines; a point is kept to two decimals.
	for edge, points := range map[string][]map[string]any{linkA: pts(10.004, 20, 30, 40), linkB: pts(1, 2), adoptA: pts(5, 6)} {
		if code := save(ownerAuth.AccessToken, map[string]any{"edge": edge, "points": points}); code != 204 {
			t.Fatalf("owner route %s: %d", edge, code)
		}
	}
	code, out := get(t, api, "/api/v1/topology/routes", ownerAuth.AccessToken)
	routes := routesOf(t, out)
	if code != 200 || len(routes) != 3 || len(routes[linkA]) != 2 || routes[linkA][0].(map[string]any)["x"] != 10.0 {
		t.Fatalf("owner routes: %d %v", code, out)
	}

	// Invalid: an edge that is not routable, too many points, an unknown gateway.
	for _, body := range []map[string]any{
		{"edge": "heard:" + gwA.ID + "/fb0000000001", "points": pts(1, 1)},
		{"edge": "gw:" + gwA.ID, "points": pts(1, 1)},
		{"edge": linkA, "points": pts(0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13, 14, 14, 15, 15, 16, 16)},
		{"edge": linkA, "points": pts(2000000, 0)},
	} {
		if code := save(ownerAuth.AccessToken, body); code != 400 {
			t.Fatalf("invalid route %v: %d", body, code)
		}
	}
	if code := save(ownerAuth.AccessToken, map[string]any{"edge": "link:00000000-0000-4000-8000-000000000000", "points": pts(1, 1)}); code != 404 {
		t.Fatalf("unknown gateway: %d", code)
	}

	// An operator restricted to project A sees and routes only project A's lines; a viewer only reads.
	operatorEmail, viewerEmail := memberEmail(), memberEmail()
	addMember(t, api, ownerAuth.AccessToken, operatorEmail, "operator", []string{projectA.ID})
	addMember(t, api, ownerAuth.AccessToken, viewerEmail, "viewer", []string{})
	opAuth, _ := changeInitialPassword(t, f, api, operatorEmail, owner.TenantID)
	viewerAuth, _ := changeInitialPassword(t, f, api, viewerEmail, owner.TenantID)
	code, out = get(t, api, "/api/v1/topology/routes", opAuth.AccessToken)
	if routes := routesOf(t, out); code != 200 || len(routes) != 2 || routes[linkB] != nil {
		t.Fatalf("restricted read: %d %v", code, out)
	}
	if code := save(opAuth.AccessToken, map[string]any{"edge": linkA, "points": pts(7, 7)}); code != 204 {
		t.Fatalf("restricted route in A: %d", code)
	}
	if code := save(opAuth.AccessToken, map[string]any{"edge": linkB, "points": pts(7, 7)}); code != 404 {
		t.Fatalf("restricted route in B: %d", code)
	}
	if code := save(viewerAuth.AccessToken, map[string]any{"edge": linkA, "points": pts(1, 1)}); code != 403 {
		t.Fatalf("viewer route: %d", code)
	}

	// No points draws the line straight again.
	if code := save(ownerAuth.AccessToken, map[string]any{"edge": linkB, "points": []any{}}); code != 204 {
		t.Fatalf("reset route: %d", code)
	}
	// A registration moved to another gateway leaves its old route behind, unread.
	if _, e := f.service.UpdateDevice(ctx, owner, devA.ID, nil, &gwA2.ID); e != nil {
		t.Fatal(e)
	}
	code, out = get(t, api, "/api/v1/topology/routes", ownerAuth.AccessToken)
	if routes := routesOf(t, out); code != 200 || len(routes) != 1 || routes[linkA] == nil || routes[linkA][0].(map[string]any)["x"] != 7.0 {
		t.Fatalf("after reset and move: %d %v", code, out)
	}
	// Routes never touch the node layout.
	if code, out = get(t, api, "/api/v1/topology/layout", ownerAuth.AccessToken); code != 200 || out["version"] != 0.0 {
		t.Fatalf("routes moved the layout version: %d %v", code, out)
	}
}
