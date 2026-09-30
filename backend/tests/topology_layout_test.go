package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

// The connect view's canvas layout lives on the server (migration 00041): one version per workspace view,
// positions per node, each node tagged with its gateway so project scope decides who sees and moves it.

func layoutOf(t *testing.T, out map[string]any) (float64, map[string]map[string]any) {
	t.Helper()
	version, ok := out["version"].(float64)
	if !ok {
		t.Fatalf("no version in %v", out)
	}
	raw, _ := out["positions"].(map[string]any)
	positions := map[string]map[string]any{}
	for id, v := range raw {
		positions[id], _ = v.(map[string]any)
	}
	return version, positions
}

func keys(m map[string]map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

func ignoredOf(out map[string]any) []string {
	list, _ := out["ignored"].([]any)
	ids := []string{}
	for _, v := range list {
		ids = append(ids, v.(string))
	}
	return ids
}

func pos(x, y float64) map[string]any { return map[string]any{"x": x, "y": y} }

func TestTopologyLayoutIsSharedScopedAndVersioned(t *testing.T) {
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
	gwB, _, e := f.service.CreateGatewayIn(ctx, owner, "GW B", "minew-mg3", &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	gwNone, _, e := f.service.CreateGateway(ctx, owner, "GW กลาง", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.CreateDevice(ctx, owner, gwA.ID, "Dev A", "fa0000000001", "generic-environment@1"); e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.CreateDevice(ctx, owner, gwB.ID, "Dev B", "fa0000000002", "generic-environment@1"); e != nil {
		t.Fatal(e)
	}
	nA, nB, nNone := "gw:"+gwA.ID, "gw:"+gwB.ID, "gw:"+gwNone.ID
	devA, devB := "dev:fa0000000001", "dev:fa0000000002"

	// Nothing saved yet: version 0, no positions.
	code, out := get(t, api, "/api/v1/topology/layout", ownerAuth.AccessToken)
	if v, p := layoutOf(t, out); code != 200 || v != 0 || len(p) != 0 {
		t.Fatalf("empty layout: %d %v", code, out)
	}

	// The owner saves the whole board; a node the server does not know is reported, not stored.
	save := func(token string, body map[string]any) (int, map[string]any) {
		t.Helper()
		code, out, _ := req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+token, "", "", body)
		return code, out
	}
	code, out = save(ownerAuth.AccessToken, map[string]any{"version": 0, "positions": map[string]any{
		"broker": pos(0, 0), nA: pos(100, 250), nB: pos(400, 250), nNone: pos(700, 250),
		devA: pos(100, 520.123), devB: pos(400, 520), "dev:ffffffffffff": pos(9, 9),
	}})
	if code != 200 {
		t.Fatalf("owner save: %d %v", code, out)
	}
	layout, _ := out["layout"].(map[string]any)
	v, p := layoutOf(t, layout)
	if v != 1 || !sameSet(keys(p), "broker", nA, nB, nNone, devA, devB) || !sameSet(ignoredOf(out), "dev:ffffffffffff") {
		t.Fatalf("owner save result: %v", out)
	}
	if p[devA]["y"] != 520.12 {
		t.Fatalf("positions are kept to two decimals: %v", p[devA])
	}

	// A stale version is refused with the current layout, so the client can merge and retry.
	code, out = save(ownerAuth.AccessToken, map[string]any{"version": 0, "positions": map[string]any{nA: pos(1, 1)}})
	if code != 409 || out["error"] != "version_conflict" {
		t.Fatalf("stale save: %d %v", code, out)
	}
	if v, p := layoutOf(t, out["layout"].(map[string]any)); v != 1 || p[nA]["x"] != 100.0 {
		t.Fatalf("conflict must carry the current layout unchanged: %v", out)
	}

	// Members: an operator restricted to project A, an unrestricted viewer.
	restricted, viewerEmail := memberEmail(), memberEmail()
	restrictedID := addMember(t, api, ownerAuth.AccessToken, restricted, "operator", []string{projectA.ID})
	addMember(t, api, ownerAuth.AccessToken, viewerEmail, "viewer", []string{})
	opAuth, _ := changeInitialPassword(t, f, api, restricted, owner.TenantID)
	viewerAuth, _ := changeInitialPassword(t, f, api, viewerEmail, owner.TenantID)

	// The restricted operator sees the broker and project A's nodes only.
	code, out = get(t, api, "/api/v1/topology/layout", opAuth.AccessToken)
	if v, p := layoutOf(t, out); code != 200 || v != 1 || !sameSet(keys(p), "broker", nA, devA) {
		t.Fatalf("restricted read: %d %v", code, out)
	}
	// It moves its own nodes; the broker and another project's gateway are skipped, not written.
	code, out = save(opAuth.AccessToken, map[string]any{"version": 1, "positions": map[string]any{nA: pos(150, 260), devA: pos(150, 530), nB: pos(-5, -5), "broker": pos(-5, -5)}})
	if code != 200 || !sameSet(ignoredOf(out), "broker", nB) {
		t.Fatalf("restricted save: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/topology/layout", ownerAuth.AccessToken)
	v, p = layoutOf(t, out)
	if v != 2 || p[nA]["x"] != 150.0 || p[nB]["x"] != 400.0 || p["broker"]["x"] != 0.0 {
		t.Fatalf("after the restricted save the owner sees: %v", out)
	}
	// Removing only reaches what the member can see: another project's gateway and the broker stay.
	code, out = save(opAuth.AccessToken, map[string]any{"version": 2, "positions": map[string]any{nA: pos(10, 10)}, "remove": []string{nB, "broker", devA}})
	if code != 200 {
		t.Fatalf("restricted remove: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/topology/layout", ownerAuth.AccessToken)
	if v, p := layoutOf(t, out); v != 3 || !sameSet(keys(p), "broker", nA, nB, nNone, devB) || p[nA]["x"] != 10.0 || p[nB]["x"] != 400.0 || p["broker"]["x"] != 0.0 {
		t.Fatalf("restricted remove touched other projects: %v", out)
	}
	// Auto layout is gone from the API: a save names every node it touches, nothing else.
	if code, out = save(opAuth.AccessToken, map[string]any{"version": 3, "replace": true, "positions": map[string]any{}}); code != 400 {
		t.Fatalf("replace is no longer accepted: %d %v", code, out)
	}
	// Saving what is already stored changes nothing: same version, no audit, no signal.
	code, out = save(ownerAuth.AccessToken, map[string]any{"version": 3, "positions": map[string]any{nA: pos(10, 10)}})
	if v, _ := layoutOf(t, out["layout"].(map[string]any)); code != 200 || v != 3 {
		t.Fatalf("a no-op save moved the version: %d %v", code, out)
	}

	// Viewers read but never save; a member whose connect module is read-only cannot save either.
	if code, out = get(t, api, "/api/v1/topology/layout", viewerAuth.AccessToken); code != 200 {
		t.Fatalf("viewer read: %d %v", code, out)
	}
	if code, out = save(viewerAuth.AccessToken, map[string]any{"version": 3, "positions": map[string]any{nA: pos(1, 1)}}); code != 403 {
		t.Fatalf("viewer save: %d %v", code, out)
	}
	code, out, _ = req(t, api, "POST", "/api/v1/members/"+restrictedID+"/access", "Bearer "+ownerAuth.AccessToken, "", "", map[string]string{"connect": "read"})
	if code != 204 && code != 200 {
		t.Fatalf("set connect read: %d %v", code, out)
	}
	if code, out = save(opAuth.AccessToken, map[string]any{"version": 3, "positions": map[string]any{nA: pos(1, 1)}}); code != 403 {
		t.Fatalf("connect=read save: %d %v", code, out)
	}
	if code, _ = get(t, api, "/api/v1/topology/layout", opAuth.AccessToken); code != 200 {
		t.Fatalf("connect=read may still read the layout: %d", code)
	}

	// Another workspace sees nothing and cannot place this workspace's nodes.
	_, otherAuth, _ := f.account(t)
	code, out = get(t, api, "/api/v1/topology/layout", otherAuth.AccessToken)
	if v, p := layoutOf(t, out); code != 200 || v != 0 || len(p) != 0 {
		t.Fatalf("other tenant read: %d %v", code, out)
	}
	code, out = save(otherAuth.AccessToken, map[string]any{"version": 0, "positions": map[string]any{nA: pos(1, 1), devA: pos(1, 1)}})
	if code != 200 || !sameSet(ignoredOf(out), nA, devA) {
		t.Fatalf("other tenant placing foreign nodes: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/topology/layout", ownerAuth.AccessToken)
	if v, p := layoutOf(t, out); v != 3 || p[nA]["x"] != 10.0 {
		t.Fatalf("the other workspace changed this layout: %v", out)
	}

	// Saves are audited once per member and view in the window, not once per drag.
	var audits int
	if e := f.admin.QueryRow(`SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action='topology.layout_saved'`, owner.TenantID).Scan(&audits); e != nil {
		t.Fatal(e)
	}
	if audits != 2 { // the owner once, the restricted operator once
		t.Fatalf("layout audit rows: %d", audits)
	}
}

func TestTopologyLayoutValidation(t *testing.T) {
	f := setup(t)
	_, ownerAuth, _ := f.account(t)
	api := busyAPI(f)
	bad := []map[string]any{
		{"positions": map[string]any{"broker": pos(1, 1)}},                            // no version
		{"version": -1, "positions": map[string]any{"broker": pos(1, 1)}},             // negative version
		{"version": 0, "positions": map[string]any{"gw:not-a-uuid": pos(1, 1)}},       // node id
		{"version": 0, "positions": map[string]any{"dev:AB:CD": pos(1, 1)}},           // upper case
		{"version": 0, "positions": map[string]any{"draft:x": pos(1, 1)}},             // drafts stay in the browser
		{"version": 0, "positions": map[string]any{"broker": pos(2e6, 1)}},            // out of range
		{"version": 0, "positions": map[string]any{"broker": map[string]any{"x": 1}}}, // missing y
		{"version": 0, "positions": map[string]any{"broker": pos(1, 1)}, "remove": []string{"broker"}},
		{"version": 0, "view": "Connect!", "positions": map[string]any{}},
		{"version": 0, "positions": map[string]any{}, "extra": true},
	}
	many := map[string]any{}
	for i := 0; i <= domain.MaxLayoutEntries; i++ {
		many[fmt.Sprintf("dev:%012x", i)] = pos(1, 1)
	}
	bad = append(bad, map[string]any{"version": 0, "positions": many})
	for i, b := range bad {
		if code, out, _ := req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+ownerAuth.AccessToken, "", "", b); code != 400 {
			t.Fatalf("case %d accepted: %d %v", i, code, out)
		}
	}
	if code, out := get(t, api, "/api/v1/topology/layout?view=Bad!", ownerAuth.AccessToken); code != 400 {
		t.Fatalf("bad view read: %d %v", code, out)
	}
	// The database refuses what the API would never send.
	for _, stmt := range []string{
		`INSERT INTO core.topology_positions(tenant_id,view,node_id,gateway_id,x,y) VALUES($1,'connect','broker',gen_random_uuid(),0,0)`,
		`INSERT INTO core.topology_positions(tenant_id,view,node_id,gateway_id,x,y) VALUES($1,'connect','dev:aa',NULL,0,0)`,
		`INSERT INTO core.topology_positions(tenant_id,view,node_id,gateway_id,x,y) VALUES($1,'connect','nope',NULL,0,0)`,
	} {
		tx, e := f.admin.Begin()
		if e != nil {
			t.Fatal(e)
		}
		_, _ = tx.Exec(`INSERT INTO core.topology_layouts(tenant_id,view) VALUES($1,'connect') ON CONFLICT DO NOTHING`, ownerTenant(t, f, ownerAuth.AccessToken))
		if _, e = tx.Exec(stmt, ownerTenant(t, f, ownerAuth.AccessToken)); e == nil {
			tx.Rollback()
			t.Fatalf("database accepted: %s", stmt)
		}
		tx.Rollback()
	}
}

func ownerTenant(t *testing.T, f *fixture, token string) string {
	t.Helper()
	p, e := f.service.Authenticate(context.Background(), token)
	if e != nil {
		t.Fatal(e)
	}
	return p.TenantID
}

// A restricted member can meet a stored row it cannot see: the device moved into its project, but the saved
// position still names the old gateway. That one node is skipped; the rest of the save lands.
func TestTopologyLayoutSkipsRowsOutsideScope(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)
	projectA, _ := f.service.CreateProject(ctx, owner, "A", "", "mint")
	projectB, _ := f.service.CreateProject(ctx, owner, "B", "", "blue")
	gwA, _, e := f.service.CreateGatewayIn(ctx, owner, "GW A", "minew-mg3", &projectA.ID)
	if e != nil {
		t.Fatal(e)
	}
	gwB, _, e := f.service.CreateGatewayIn(ctx, owner, "GW B", "minew-mg3", &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.CreateDevice(ctx, owner, gwA.ID, "Moved", "fb0000000001", "generic-environment@1"); e != nil {
		t.Fatal(e)
	}
	// The owner's save stored the device under project B's gateway (as it was before the move).
	if _, e = f.admin.Exec(`INSERT INTO core.topology_layouts(tenant_id,view,version) VALUES($1,'connect',5)`, owner.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.admin.Exec(`INSERT INTO core.topology_positions(tenant_id,view,node_id,gateway_id,x,y) VALUES($1,'connect','dev:fb0000000001',$2,7,7)`, owner.TenantID, gwB.ID); e != nil {
		t.Fatal(e)
	}
	email := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, email, "operator", []string{projectA.ID})
	opAuth, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
	code, out, _ := req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+opAuth.AccessToken, "", "", map[string]any{"version": 5, "positions": map[string]any{"dev:fb0000000001": pos(1, 1), "gw:" + gwA.ID: pos(2, 2)}})
	if code != 200 || !sameSet(ignoredOf(out), "dev:fb0000000001") {
		t.Fatalf("save across a stale row: %d %v", code, out)
	}
	if v, p := layoutOf(t, out["layout"].(map[string]any)); v != 6 || !sameSet(keys(p), "gw:"+gwA.ID) {
		t.Fatalf("layout after the save: %v", out)
	}
	// The owner's next save refreshes the gateway, so the member then sees and moves it.
	code, out = get(t, api, "/api/v1/topology/layout", ownerAuth.AccessToken)
	v, _ := layoutOf(t, out)
	if code, out, _ = req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"version": v, "positions": map[string]any{"dev:fb0000000001": pos(3, 3)}}); code != 200 {
		t.Fatalf("owner refresh: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/topology/layout", opAuth.AccessToken)
	if _, p := layoutOf(t, out); p["dev:fb0000000001"]["x"] != 3.0 {
		t.Fatalf("after the refresh the member should see the device: %v", out)
	}
}

// Every save tells the workspace's open canvases to refetch the layout, through the same NOTIFY channel as the
// other change signals (delivered on commit).
func TestTopologyLayoutSignalsOtherClients(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)
	conn, e := pgx.Connect(ctx, os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	if _, e = conn.Exec(ctx, "LISTEN "+postgres.SignalChannel); e != nil {
		t.Fatal(e)
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"version": 0, "positions": map[string]any{"broker": pos(1, 1)}}); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		n, e := conn.WaitForNotification(wait)
		if e != nil {
			t.Fatalf("no layout signal: %v", e)
		}
		var s domain.Signal
		if json.Unmarshal([]byte(n.Payload), &s) == nil && s.Tenant == owner.TenantID && s.Kind == "layout" {
			if s.Gateway != "" {
				t.Fatalf("a layout signal names no gateway: %v", s)
			}
			return
		}
		if !strings.Contains(n.Payload, "{") {
			t.Fatalf("unexpected payload %q", n.Payload)
		}
	}
}

// Under a project filter the canvas lays out and saves only that project's nodes. The save names exactly those
// nodes, so every other project's position and the broker's stay as they were.
func TestTopologyLayoutFilteredRelayoutKeepsOtherProjects(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)
	projectA, _ := f.service.CreateProject(ctx, owner, "A", "", "mint")
	projectB, _ := f.service.CreateProject(ctx, owner, "B", "", "blue")
	gwA, _, e := f.service.CreateGatewayIn(ctx, owner, "GW A", "minew-mg3", &projectA.ID)
	if e != nil {
		t.Fatal(e)
	}
	gwB, _, e := f.service.CreateGatewayIn(ctx, owner, "GW B", "minew-mg3", &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.CreateDevice(ctx, owner, gwA.ID, "A1", "fc0000000001", "generic-environment@1"); e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.CreateDevice(ctx, owner, gwB.ID, "B1", "fc0000000002", "generic-environment@1"); e != nil {
		t.Fatal(e)
	}
	nA, nB, dA, dB := "gw:"+gwA.ID, "gw:"+gwB.ID, "dev:fc0000000001", "dev:fc0000000002"
	full := map[string]any{"broker": pos(500, 0), nA: pos(100, 250), nB: pos(900, 250), dA: pos(100, 520), dB: pos(900, 520)}
	code, out, _ := req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"version": 0, "positions": full})
	if code != 200 {
		t.Fatalf("full save: %d %v", code, out)
	}
	// Auto layout of project A alone (the broker is left out under a filter).
	code, out, _ = req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"version": 1, "positions": map[string]any{nA: pos(0, 250), dA: pos(0, 520)}})
	if code != 200 {
		t.Fatalf("filtered save: %d %v", code, out)
	}
	v, p := layoutOf(t, out["layout"].(map[string]any))
	if v != 2 || !sameSet(keys(p), "broker", nA, nB, dA, dB) {
		t.Fatalf("filtered relayout lost nodes: %v", out)
	}
	for id, want := range map[string][2]float64{"broker": {500, 0}, nB: {900, 250}, dB: {900, 520}, nA: {0, 250}, dA: {0, 520}} {
		if p[id]["x"] != want[0] || p[id]["y"] != want[1] {
			t.Fatalf("%s moved to %v, want %v", id, p[id], want)
		}
	}
}

// Stored positions follow the inventory: a moved registration takes its row to the new gateway (so project scope
// keeps applying), a removed one that no gateway still hears loses its row, a revoked gateway loses its own, and an
// owner's save prunes rows whose node no longer exists.
func TestTopologyLayoutFollowsInventory(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)
	gwA, _, e := f.service.CreateGateway(ctx, owner, "GW A", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	gwB, _, e := f.service.CreateGateway(ctx, owner, "GW B", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	dev, e := f.service.CreateDevice(ctx, owner, gwA.ID, "Mover", "FD0000000001", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	node := "dev:fd0000000001" // upper case at registration, lower case on the canvas
	code, out, _ := req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"version": 0, "positions": map[string]any{
		"broker": pos(0, 0), "gw:" + gwA.ID: pos(1, 1), "gw:" + gwB.ID: pos(2, 2), node: pos(3, 3)}})
	if code != 200 || len(ignoredOf(out)) != 0 {
		t.Fatalf("save: %d %v", code, out)
	}
	gatewayOf := func(id string) string {
		t.Helper()
		var gw sql.NullString
		e := f.admin.QueryRow(`SELECT gateway_id::text FROM core.topology_positions WHERE tenant_id=$1 AND node_id=$2`, owner.TenantID, id).Scan(&gw)
		if e == sql.ErrNoRows {
			return "<none>"
		}
		if e != nil {
			t.Fatal(e)
		}
		return gw.String
	}
	version := func() int64 {
		t.Helper()
		var v int64
		if e := f.admin.QueryRow(`SELECT version FROM core.topology_layouts WHERE tenant_id=$1 AND view='connect'`, owner.TenantID).Scan(&v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	if gatewayOf(node) != gwA.ID {
		t.Fatalf("stored under %s", gatewayOf(node))
	}
	if _, e = f.service.UpdateDevice(ctx, owner, dev.ID, nil, &gwB.ID); e != nil {
		t.Fatal(e)
	}
	if gatewayOf(node) != gwB.ID || version() != 2 {
		t.Fatalf("after the move: %s v%d", gatewayOf(node), version())
	}
	// A rename changes nothing on the canvas.
	name := "Renamed"
	if _, e = f.service.UpdateDevice(ctx, owner, dev.ID, &name, nil); e != nil {
		t.Fatal(e)
	}
	if version() != 2 {
		t.Fatalf("a rename bumped the layout: v%d", version())
	}
	if e = f.service.RemoveDevice(ctx, owner, dev.ID); e != nil {
		t.Fatal(e)
	}
	if gatewayOf(node) != "<none>" || version() != 3 {
		t.Fatalf("after the removal: %s v%d", gatewayOf(node), version())
	}
	if e = f.service.RevokeGateway(ctx, owner, gwB.ID); e != nil {
		t.Fatal(e)
	}
	if gatewayOf("gw:"+gwB.ID) != "<none>" || gatewayOf("gw:"+gwA.ID) != gwA.ID || gatewayOf("broker") != "" {
		t.Fatalf("after the revocation: B %s A %s broker %q", gatewayOf("gw:"+gwB.ID), gatewayOf("gw:"+gwA.ID), gatewayOf("broker"))
	}
	// An orphan written behind the API's back (a sighting that expired) is pruned by the next owner save.
	if _, e = f.admin.Exec(`INSERT INTO core.topology_positions(tenant_id,view,node_id,gateway_id,x,y) VALUES($1,'connect','dev:0000deadbeef',$2,5,5)`, owner.TenantID, gwA.ID); e != nil {
		t.Fatal(e)
	}
	code, out, _ = req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"version": version(), "positions": map[string]any{"gw:" + gwA.ID: pos(7, 7)}})
	if code != 200 || gatewayOf("dev:0000deadbeef") != "<none>" {
		t.Fatalf("orphan kept: %d %v", code, out)
	}
}

// Every stored row of a view counts toward one cap, including rows a restricted member cannot see, so a read never
// cuts off a live node. A save that would pass the cap is refused whole.
func TestTopologyLayoutRowCap(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)
	projectA, _ := f.service.CreateProject(ctx, owner, "A", "", "mint")
	projectB, _ := f.service.CreateProject(ctx, owner, "B", "", "blue")
	gwA, _, e := f.service.CreateGatewayIn(ctx, owner, "GW A", "minew-mg3", &projectA.ID)
	if e != nil {
		t.Fatal(e)
	}
	gwB, _, e := f.service.CreateGatewayIn(ctx, owner, "GW B", "minew-mg3", &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.admin.Exec(`INSERT INTO core.topology_layouts(tenant_id,view,version) VALUES($1,'connect',1)`, owner.TenantID); e != nil {
		t.Fatal(e)
	}
	// Project B's rows fill the view (a restricted member of A cannot see or prune them).
	if _, e = f.admin.Exec(`INSERT INTO core.topology_positions(tenant_id,view,node_id,gateway_id,x,y)
	  SELECT $1,'connect','dev:'||lpad(to_hex(i),12,'0'),$2,i,i FROM generate_series(1,$3) i`, owner.TenantID, gwB.ID, domain.MaxLayoutRows); e != nil {
		t.Fatal(e)
	}
	email := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, email, "operator", []string{projectA.ID})
	opAuth, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
	code, out, _ := req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+opAuth.AccessToken, "", "", map[string]any{"version": 1, "positions": map[string]any{"gw:" + gwA.ID: pos(1, 1)}})
	if code != 400 || out["error"] != "layout_full" {
		t.Fatalf("a save past the cap: %d %v", code, out)
	}
	// The owner's next save prunes the orphans (none of those devices exists) and the board has room again.
	code, out, _ = req(t, api, "POST", "/api/v1/topology/layout", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"version": 1, "positions": map[string]any{"gw:" + gwA.ID: pos(1, 1)}})
	if v, p := layoutOf(t, out["layout"].(map[string]any)); code != 200 || v != 2 || !sameSet(keys(p), "gw:"+gwA.ID) {
		t.Fatalf("owner save after pruning: %d %v", code, out)
	}
}
