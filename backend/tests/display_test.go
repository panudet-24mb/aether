package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/domain"
	"aether/backend/internal/realtime"
	"github.com/gofiber/fiber/v3"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgconn"
)

// Display links (docs/platform/display.md): a wall TV pairs with a one-time code, holds a read-only display token
// that no member route accepts, sees only its projects, shows wearer names only when allowed (and logs it), may
// acknowledge only when allowed, and stops the moment it is revoked.

func createDisplay(t *testing.T, api *fiber.App, token string, payload map[string]any) (string, string) {
	t.Helper()
	code, out, _ := req(t, api, "POST", "/api/v1/displays", "Bearer "+token, "", "", payload)
	if code != 201 {
		t.Fatalf("create display: %d %v", code, out)
	}
	d, _ := out["display"].(map[string]any)
	pairing, _ := out["code"].(string)
	if d == nil || d["id"] == "" || len(pairing) != 9 || d["paired"] != false {
		t.Fatalf("created display: %v", out)
	}
	return d["id"].(string), pairing
}

func pairDisplay(t *testing.T, api *fiber.App, pairing string) (int, string) {
	t.Helper()
	code, out, _ := req(t, api, "POST", "/api/v1/kiosk/pair", "", "", "", map[string]any{"code": pairing})
	token, _ := out["token"].(string)
	return code, token
}

func kiosk(t *testing.T, api *fiber.App, method, path, token string) (int, map[string]any) {
	t.Helper()
	code, out, _ := req(t, api, method, "/api/v1/kiosk"+path, "Bearer "+token, "", "", nil)
	return code, out
}

func TestDisplayPairing(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	_, auth, owner := f.account(t)
	id, pairing := createDisplay(t, api, auth.AccessToken, map[string]any{"name": "จอห้อง รปภ."})

	// Codes are typed by people: any case, with or without the dash, spaces allowed.
	if code, _ := pairDisplay(t, api, "AAAA-AAAA"); code != 401 {
		t.Fatalf("an unknown code paired: %d", code)
	}
	code, token := pairDisplay(t, api, " "+strings.ToLower(strings.ReplaceAll(pairing, "-", ""))+" ")
	if code != 200 || !strings.HasPrefix(token, "dsp_") {
		t.Fatalf("pair: %d %q", code, token)
	}
	// Single use: the same code again is refused, and the first token keeps working.
	if code, _ := pairDisplay(t, api, pairing); code != 401 {
		t.Fatalf("a used code paired again: %d", code)
	}
	code, out := kiosk(t, api, "GET", "/session", token)
	if code != 200 || out["display"].(map[string]any)["id"] != id {
		t.Fatalf("session: %d %v", code, out)
	}
	// Neither the code nor the token is stored in the clear, and the runtime cannot read the digests back.
	var stored string
	if e := f.admin.QueryRow(`SELECT coalesce(token_hash,'') FROM core.displays WHERE id=$1`, id).Scan(&stored); e != nil || len(stored) != 64 || strings.Contains(stored, token) {
		t.Fatalf("token digest: %q %v", stored, e)
	}
	_, e := f.runtime.Exec(`SELECT token_hash FROM core.displays`)
	var pgErr *pgconn.PgError
	if !errors.As(e, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("the runtime read token digests: %v", e)
	}
	// The admin page shows where a display was last seen from, starting with the pairing itself.
	if n := count(t, f.admin, `SELECT count(*) FROM core.displays WHERE id=$1 AND last_ip IS NOT NULL AND last_seen_at IS NOT NULL`, id); n != 1 {
		t.Fatal("pairing did not record where the display is")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action='display.paired' AND actor_id=$2`, owner.TenantID, id); n != 1 {
		t.Fatalf("pairing audited %d times", n)
	}

	// An expired code is refused.
	_, late := createDisplay(t, api, auth.AccessToken, map[string]any{"name": "จอหน้าลิฟต์"})
	if _, e := f.admin.Exec(`UPDATE core.displays SET pairing_expires_at=now()-interval '1 second' WHERE tenant_id=$1 AND token_hash IS NULL`, owner.TenantID); e != nil {
		t.Fatal(e)
	}
	if code, _ := pairDisplay(t, api, late); code != 401 {
		t.Fatalf("an expired code paired: %d", code)
	}

	// Guessing is limited per address: the eleventh attempt in a minute is refused before it is tried.
	guesses := busyAPI(f)
	for i := 0; i < 10; i++ {
		if code, _ := pairDisplay(t, guesses, "BBBB-BBBB"); code != 401 {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	if code, _ := pairDisplay(t, guesses, "BBBB-BBBB"); code != 429 {
		t.Fatalf("the eleventh guess: %d", code)
	}

	// Re-pairing issues a new code and ends the old token at once.
	code, out, _ = req(t, api, "POST", "/api/v1/displays/"+id+"/repair", "Bearer "+auth.AccessToken, "", "", nil)
	if code != 200 || len(out["code"].(string)) != 9 {
		t.Fatalf("repair: %d %v", code, out)
	}
	if code, _ := kiosk(t, api, "GET", "/session", token); code != 401 {
		t.Fatalf("the old token survived a re-pair: %d", code)
	}
	if code, fresh := pairDisplay(t, api, out["code"].(string)); code != 200 || fresh == token {
		t.Fatalf("pair after repair: %d", code)
	}
}

func TestDisplayManagementIsForFullScopeAdmins(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	s := newProjectAlertSite(t, f)
	for _, role := range []string{"viewer", "operator"} {
		email := memberEmail()
		addMember(t, api, s.ownerToken, email, role, nil)
		member, _ := changeInitialPassword(t, f, api, email, s.owner.TenantID)
		if code, _, _ := req(t, api, "POST", "/api/v1/displays", "Bearer "+member.AccessToken, "", "", map[string]any{"name": "x"}); code != 403 {
			t.Fatalf("%s created a display: %d", role, code)
		}
		if code, _ := get(t, api, "/api/v1/displays", member.AccessToken); code != 403 {
			t.Fatalf("%s listed displays: %d", role, code)
		}
	}
	email := memberEmail()
	addMember(t, api, s.ownerToken, email, "admin", []string{s.projectA})
	limited, _ := changeInitialPassword(t, f, api, email, s.owner.TenantID)
	if code, _, _ := req(t, api, "POST", "/api/v1/displays", "Bearer "+limited.AccessToken, "", "", map[string]any{"name": "x", "project_ids": []string{s.projectA}}); code != 403 {
		t.Fatalf("a project-limited admin created a display: %d", code)
	}
	// Bad input is refused before anything is stored.
	for _, bad := range []map[string]any{
		{"name": ""},
		{"name": strings.Repeat("ก", 81)},
		{"name": "x", "project_ids": []string{"not-a-uuid"}},
		{"name": "x", "project_ids": []string{"11111111-1111-4111-8111-111111111111"}},
		{"name": "x", "playlist": []map[string]any{{"kind": "overview", "seconds": 5}}},
		{"name": "x", "playlist": []map[string]any{{"kind": "studio", "seconds": 30}}},
		{"name": "x", "playlist": []map[string]any{{"kind": "shell", "seconds": 30}}},
	} {
		if code, out, _ := req(t, api, "POST", "/api/v1/displays", "Bearer "+s.ownerToken, "", "", bad); code != 400 {
			t.Fatalf("accepted %v: %d %v", bad, code, out)
		}
	}
}

// A display token opens the kiosk routes and nothing else: every member route, reads and writes alike, answers
// 403, and a display transaction cannot write even if a code path tried.
func TestDisplayTokenIsReadOnly(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	_, auth, owner := f.account(t)
	id, pairing := createDisplay(t, api, auth.AccessToken, map[string]any{"name": "จอ"})
	_, token := pairDisplay(t, api, pairing)
	tried := 0
	for _, route := range api.GetRoutes(true) {
		path := route.Path
		if !strings.HasPrefix(path, "/api/v1/") || strings.HasPrefix(path, "/api/v1/kiosk") || strings.HasPrefix(path, "/api/v1/auth/") || path == "/api/v1/notice" {
			continue
		}
		if route.Method != "GET" && route.Method != "POST" {
			continue
		}
		for _, param := range route.Params {
			path = strings.Replace(path, ":"+param, "11111111-1111-4111-8111-111111111111", 1)
		}
		code, out, _ := req(t, api, route.Method, path, "Bearer "+token, "", "", map[string]any{})
		if code != 403 || out["detail"] != "display_token" {
			t.Fatalf("%s %s with a display token: %d %v", route.Method, route.Path, code, out)
		}
		tried++
	}
	if tried < 100 {
		t.Fatalf("only %d member routes were tried", tried)
	}
	// Defence in depth: a repository write in a display's context is refused by the database.
	ctx := domain.WithDisplay(context.Background(), id)
	gw, _, e := f.service.CreateGateway(context.Background(), owner, "GW", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if e := f.repo.RevokeGateway(ctx, domain.Principal{TenantID: owner.TenantID, Role: "owner"}, gw.ID); e == nil || !strings.Contains(e.Error(), "read-only") {
		t.Fatalf("a write in a display transaction: %v", e)
	}
	// And a member id cannot ride along with a display id: the scope is then empty.
	var scope *string
	tx, e := f.runtime.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e := tx.Exec(`SELECT set_config('app.tenant_id',$1,true),set_config('app.user_id',$2,true),set_config('app.display_id',$3,true)`, owner.TenantID, owner.UserID, id); e != nil {
		t.Fatal(e)
	}
	if e := tx.QueryRow(`SELECT core.compute_project_scope()`).Scan(&scope); e != nil || scope != nil {
		t.Fatalf("scope of a user with a display id: %v %v", scope, e)
	}
}

func boardOf(t *testing.T, api *fiber.App, token string) map[string]any {
	t.Helper()
	code, out := kiosk(t, api, "GET", "/board", token)
	if code != 200 {
		t.Fatalf("board: %d %v", code, out)
	}
	return out
}

func idsOf(items []map[string]any, field string) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		if v, ok := it[field].(string); ok {
			out[v] = true
		}
	}
	return out
}

func TestDisplayScopeNamesAndAck(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	s := newProjectAlertSite(t, f)
	now := time.Now()
	press(t, f, s.owner.TenantID, s.gwA, 0, now.Add(-2*time.Minute))
	// The same tag behind two gateways: an open alert holds a second one back, so the first is set aside while the
	// press on B opens its own, then reopened.
	if _, e := f.admin.Exec(`UPDATE core.alerts SET status='acknowledged',opened_at=now()-interval '10 minutes' WHERE tenant_id=$1`, s.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	press(t, f, s.owner.TenantID, s.gwB, 15, now.Add(-1*time.Minute))
	if _, e := f.admin.Exec(`UPDATE core.alerts SET status='open' WHERE tenant_id=$1`, s.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.alerts WHERE tenant_id=$1 AND status='open'`, s.owner.TenantID); n != 2 {
		t.Fatalf("expected an open SOS on A and on B, got %d", n)
	}

	idA, pairA := createDisplay(t, api, s.ownerToken, map[string]any{"name": "จออาคาร A", "project_ids": []string{s.projectA}})
	_, tokenA := pairDisplay(t, api, pairA)
	_, pairAll := createDisplay(t, api, s.ownerToken, map[string]any{"name": "จอรวม"})
	_, tokenAll := pairDisplay(t, api, pairAll)

	board := boardOf(t, api, tokenA)
	gateways := idsOf(listOf(board, "gateways"), "id")
	if !gateways[s.gwA] || gateways[s.gwB] || gateways[s.gwNone] || len(gateways) != 1 {
		t.Fatalf("project A display sees gateways %v", gateways)
	}
	alerts := listOf(board, "alerts")
	if len(alerts) != 1 || alerts[0]["gateway_id"] != s.gwA || alerts[0]["takeover"] != true {
		t.Fatalf("project A display alerts: %v", alerts)
	}
	// Names of worn tags are not shown by default: a neutral label and a title that says what, not who.
	if name := alerts[0]["device_name"].(string); !strings.HasPrefix(name, "ผู้สวมใส่ · ") || alerts[0]["title"] != "กดปุ่มฉุกเฉิน" {
		t.Fatalf("masked alert: %v", alerts[0])
	}
	for _, d := range listOf(board, "devices") {
		if d["name"] == "ปุ่ม SOS" {
			t.Fatalf("a wearer's name reached the TV: %v", d)
		}
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.access_log WHERE tenant_id=$1 AND actor_id=$2`, s.owner.TenantID, idA); n != 0 {
		t.Fatalf("masked reads were logged: %d", n)
	}
	all := boardOf(t, api, tokenAll)
	if gw := idsOf(listOf(all, "gateways"), "id"); len(gw) != 3 {
		t.Fatalf("a display of every project sees %v", gw)
	}
	if len(listOf(all, "alerts")) != 2 {
		t.Fatalf("a display of every project sees alerts %v", listOf(all, "alerts"))
	}

	// With names allowed, the TV shows them and every such read is in the access log with the display as actor.
	code, out, _ := req(t, api, "POST", "/api/v1/displays/"+idA+"/update", "Bearer "+s.ownerToken, "", "", map[string]any{
		"name": "จออาคาร A", "project_ids": []string{s.projectA}, "show_names": true, "playlist": []map[string]any{{"kind": "alerts", "seconds": 20}}})
	if code != 200 || out["show_names"] != true {
		t.Fatalf("update: %d %v", code, out)
	}
	board = boardOf(t, api, tokenA)
	if name, _ := listOf(board, "alerts")[0]["device_name"].(string); name == "" || strings.HasPrefix(name, "ผู้สวมใส่") {
		t.Fatalf("names allowed but shown as %q", name)
	}
	named := false
	for _, d := range listOf(board, "devices") {
		named = named || d["name"] == "ปุ่ม SOS"
	}
	if !named {
		t.Fatalf("names allowed but the tag is not named: %v", listOf(board, "devices"))
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.access_log WHERE tenant_id=$1 AND actor_id=$2 AND actor_kind='display' AND resource='display_board'`, s.owner.TenantID, idA); n != 1 {
		t.Fatalf("named reads logged %d times", n)
	}
	// The owner's privacy view names the display as the reader.
	code, out, _ = req(t, api, "GET", "/api/v1/privacy/access-log?resource=display_board", "Bearer "+s.ownerToken, "", "", nil)
	if items := listOf(out, "items"); code != 200 || len(items) != 1 || items[0]["actor_kind"] != "display" || items[0]["actor_name"] != "จอ · จออาคาร A" {
		t.Fatalf("access log view: %d %v", code, out)
	}

	// Acknowledging from the TV is off until the owner allows it; then it names the display, not a member.
	alertA := listOf(board, "alerts")[0]["id"].(string)
	if code, out := kiosk(t, api, "POST", "/alerts/"+alertA+"/ack", tokenA); code != 403 || out["error"] != "display_ack_disabled" {
		t.Fatalf("ack while not allowed: %d %v", code, out)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/displays/"+idA+"/update", "Bearer "+s.ownerToken, "", "", map[string]any{
		"name": "จออาคาร A", "project_ids": []string{s.projectA}, "allow_ack": true, "playlist": []map[string]any{{"kind": "alerts", "seconds": 20}}})
	if code != 200 {
		t.Fatalf("allow ack: %d", code)
	}
	var alertB string
	if e := f.admin.QueryRow(`SELECT id::text FROM core.alerts WHERE tenant_id=$1 AND gateway_id=$2`, s.owner.TenantID, s.gwB).Scan(&alertB); e != nil {
		t.Fatal(e)
	}
	if code, _ := kiosk(t, api, "POST", "/alerts/"+alertB+"/ack", tokenA); code != 404 {
		t.Fatalf("a display acknowledged another project's alert: %d", code)
	}
	if code, out := kiosk(t, api, "POST", "/alerts/"+alertA+"/ack", tokenA); code != 204 {
		t.Fatalf("ack: %d %v", code, out)
	}
	var status, by string
	var member *string
	if e := f.admin.QueryRow(`SELECT status,coalesce(acked_by_display::text,''),acked_by::text FROM core.alerts WHERE id=$1`, alertA).Scan(&status, &by, &member); e != nil || status != "acknowledged" || by != idA || member != nil {
		t.Fatalf("acknowledged alert: %s %s %v %v", status, by, member, e)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND actor_id=$2 AND action='alert.acknowledged' AND target_id=$3`, s.owner.TenantID, idA, alertA); n != 1 {
		t.Fatalf("display ack audited %d times", n)
	}
	if code, _ := kiosk(t, api, "POST", "/alerts/"+alertA+"/ack", tokenA); code != 404 {
		t.Fatalf("a second ack: %d", code)
	}

	// Floor plans follow the same scope; a studio dashboard must be in the playlist.
	siteB, e := f.service.Repo.CreateSite(context.Background(), s.owner, domain.Site{Name: "อาคาร B", ProjectID: &s.projectB})
	if e != nil {
		t.Fatal(e)
	}
	if code, _ := kiosk(t, api, "GET", "/floorplan?site="+siteB.ID, tokenA); code != 404 {
		t.Fatalf("project A display read project B's site: %d", code)
	}
	if code, _ := kiosk(t, api, "GET", "/floorplan?site="+siteB.ID, tokenAll); code != 200 {
		t.Fatalf("a display of every project could not read a site: %d", code)
	}
	if code, _ := kiosk(t, api, "GET", "/studio/11111111-1111-4111-8111-111111111111", tokenA); code != 404 {
		t.Fatalf("a dashboard outside the playlist: %d", code)
	}

	// Another workspace's display sees nothing of this one.
	_, otherAuth, _ := f.account(t)
	_, otherPair := createDisplay(t, api, otherAuth.AccessToken, map[string]any{"name": "จออื่น"})
	_, otherToken := pairDisplay(t, api, otherPair)
	other := boardOf(t, api, otherToken)
	if len(listOf(other, "gateways")) != 0 || len(listOf(other, "alerts")) != 0 {
		t.Fatalf("another workspace's display: %v", other)
	}
	if code, _ := kiosk(t, api, "POST", "/alerts/"+alertB+"/ack", otherToken); code != 403 {
		t.Fatalf("a foreign display acknowledged: %d", code)
	}
}

// Revoking a display ends its token and its open signal stream at once.
func TestDisplayRevokeEndsStream(t *testing.T) {
	f := setup(t)
	hub := realtime.NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Listen(ctx, os.Getenv("TEST_DATABASE_URL"), postgres.SignalChannel)
	cfg := f.cfg
	cfg.APIRateLimit = 600
	api := httpapi.NewWithHub(cfg, f.service, f.repo, hub)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go api.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	t.Cleanup(func() { api.Shutdown() })
	_, auth, _ := f.account(t)
	id, pairing := createDisplay(t, api, auth.AccessToken, map[string]any{"name": "จอ"})
	_, token := pairDisplay(t, api, pairing)

	open := func() *websocket.Conn {
		t.Helper()
		var conn *websocket.Conn
		for attempt := 0; ; attempt++ {
			conn, _, e = websocket.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/ws", http.Header{"Origin": {origin}})
			if e == nil {
				break
			}
			if attempt == 20 {
				t.Fatal(e)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if e := conn.WriteJSON(map[string]string{"type": "auth", "token": token}); e != nil {
			t.Fatal(e)
		}
		var msg map[string]any
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if e := conn.ReadJSON(&msg); e != nil || msg["type"] != "ready" {
			t.Fatalf("display stream: %v %v", msg, e)
		}
		return conn
	}
	conn := open()
	time.Sleep(300 * time.Millisecond) // the listener is up before anything is announced
	// New settings (a new project scope among them): the TV is told, and the socket closes so it reconnects under them.
	if code, _, _ := req(t, api, "POST", "/api/v1/displays/"+id+"/update", "Bearer "+auth.AccessToken, "", "", map[string]any{"name": "จอใหม่", "playlist": []map[string]any{{"kind": "alerts", "seconds": 20}}}); code != 200 {
		t.Fatalf("update: %d", code)
	}
	var msg map[string]any
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if e := conn.ReadJSON(&msg); e != nil || msg["kind"] != "display" {
		t.Fatalf("settings signal: %v %v", msg, e)
	}
	if e := conn.ReadJSON(&msg); e == nil {
		t.Fatalf("the stream stayed open after new settings: %v", msg)
	}
	conn.Close()
	conn = open()
	defer conn.Close()
	time.Sleep(300 * time.Millisecond)
	if code, _, _ := req(t, api, "POST", "/api/v1/displays/"+id+"/revoke", "Bearer "+auth.AccessToken, "", "", nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	started := time.Now()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		msg = map[string]any{}
		if e := conn.ReadJSON(&msg); e != nil {
			break // closed
		}
		if msg["type"] == "error" {
			break
		}
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("the stream outlived the revocation by %s", time.Since(started))
	}
	if code, _ := kiosk(t, api, "GET", "/board", token); code != 401 {
		t.Fatalf("a revoked token: %d", code)
	}
	if code, _, _ := req(t, api, "GET", "/api/v1/displays", "Bearer "+auth.AccessToken, "", "", nil); code != 200 {
		t.Fatalf("list after revoke: %d", code)
	}
}

// insertAlert opens an alert (and its event) directly: masking decisions must not depend on how an alert came to be.
func insertAlert(t *testing.T, f *fixture, tenant, gateway, external, eventType, severity, title, deviceName string) string {
	t.Helper()
	var id string
	if e := f.admin.QueryRow(`WITH ev AS (INSERT INTO core.device_events(tenant_id,id,gateway_id,external_id,device_name,event_type,occurred_at)
      VALUES($1,gen_random_uuid(),$2,$3,$5,$4,now()) RETURNING id)
      INSERT INTO core.alerts(tenant_id,id,event_id,gateway_id,external_id,device_name,event_type,severity,title)
      SELECT $1,gen_random_uuid(),ev.id,$2,$3,$5,$4,$6,$7 FROM ev RETURNING id::text`, tenant, gateway, external, eventType, deviceName, severity, title).Scan(&id); e != nil {
		t.Fatal(e)
	}
	return id
}

func alertByID(t *testing.T, board map[string]any, id string) map[string]any {
	t.Helper()
	for _, a := range listOf(board, "alerts") {
		if a["id"] == id {
			return a
		}
	}
	t.Fatalf("alert %s not on the board", id)
	return nil
}

// Masking fails closed: a tag is nameable only when every live registration of it is a non-worn, non-roaming one.
// Removed wearables, unregistered tags, worn tags beyond any list limit and tags re-registered as worn after the
// alert are all masked, and no MAC reaches a TV that may not show names.
func TestDisplayMaskingFailsClosed(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	s := newProjectAlertSite(t, f)
	tenant, ctx := s.owner.TenantID, context.Background()
	_, pairing := createDisplay(t, api, s.ownerToken, map[string]any{"name": "จอ"})
	_, token := pairDisplay(t, api, pairing)

	// A removed wearable.
	worn, e := f.service.CreateDevice(ctx, s.owner, s.gwA, "คุณสมหญิง", "f000000000a1", "minew-c10-pending@1")
	if e != nil {
		t.Fatal(e)
	}
	if e := f.service.RemoveDevice(ctx, s.owner, worn.ID); e != nil {
		t.Fatal(e)
	}
	removed := insertAlert(t, f, tenant, s.gwA, "f000000000a1", domain.EventTamper, "warning", "คุณสมหญิง · ป้ายถูกถอด", "คุณสมหญิง")
	// A tag nobody registered.
	unknown := insertAlert(t, f, tenant, s.gwA, "f000000000a2", domain.EventTamper, "warning", "คุณสมปอง · ป้ายถูกถอด", "คุณสมปอง")
	// 500 ordinary sensors sorted before one worn tag: the worn one is device #501.
	if _, e := f.admin.Exec(`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id)
      SELECT gen_random_uuid(),$1,$2,'a-sensor-'||lpad(n::text,3,'0'),'e1'||lpad(to_hex(n),10,'0'),'minew-s1-pending@1' FROM generate_series(1,500) n`, tenant, s.gwA); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.Exec(`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id) VALUES(gen_random_uuid(),$1,$2,'zz คุณสมศักดิ์','f000000000a3','minew-c10-pending@1')`, tenant, s.gwA); e != nil {
		t.Fatal(e)
	}
	beyond := insertAlert(t, f, tenant, s.gwA, "f000000000a3", domain.EventTamper, "warning", "คุณสมศักดิ์ · ป้ายถูกถอด", "zz คุณสมศักดิ์")
	// A room sensor: nameable, until it is re-registered as worn after its alert.
	room, e := f.service.CreateDevice(ctx, s.owner, s.gwA, "ห้องยา", "f000000000a4", "minew-s1-pending@1")
	if e != nil {
		t.Fatal(e)
	}
	roomAlert := insertAlert(t, f, tenant, s.gwA, "f000000000a4", domain.EventTamper, "warning", "ห้องยา · ป้ายถูกถอด", "ห้องยา")

	board := boardOf(t, api, token)
	for id, who := range map[string]string{removed: "สมหญิง", unknown: "สมปอง", beyond: "สมศักดิ์"} {
		if a := alertByID(t, board, id); strings.Contains(a["title"].(string)+a["device_name"].(string), who) {
			t.Fatalf("%s named on the TV: %v", who, a)
		}
	}
	if a := alertByID(t, board, roomAlert); a["device_name"] != "ห้องยา" || a["title"] != "ห้องยา · ป้ายถูกถอด" {
		t.Fatalf("a room sensor was masked: %v", a)
	}
	if _, e := f.admin.Exec(`UPDATE core.devices SET profile_id='minew-c10-pending@1' WHERE id=$1`, room.ID); e != nil {
		t.Fatal(e)
	}
	board = boardOf(t, api, token)
	if a := alertByID(t, board, roomAlert); a["device_name"] == "ห้องยา" || strings.Contains(a["title"].(string), "ห้องยา") {
		t.Fatalf("a tag re-registered as worn stayed named: %v", a)
	}
	for _, d := range listOf(board, "devices") {
		if d["id"] == room.ID && d["name"] == "ห้องยา" {
			t.Fatalf("the re-registered tag is named in devices: %v", d)
		}
	}
	// No MAC at all without names.
	raw, _ := json.Marshal(board)
	for _, mac := range []string{"f000000000a1", "f000000000a3", "f000000000a4", sosMAC, "e10000000001"} {
		if strings.Contains(strings.ToLower(string(raw)), mac) {
			t.Fatalf("MAC %s reached a TV that may not show names", mac)
		}
	}
}

// The studio view gets the same treatment: member ids and notes never, names of personal tags only when allowed,
// and never a worn tag's location.
func TestDisplayStudioIsMasked(t *testing.T) {
	// The widget sandbox needs QuickJS; this runner stands in for it and renders the widget's whole input, so the
	// test sees exactly what a widget on the TV could have put on screen.
	if _, e := exec.LookPath("python3"); e != nil {
		t.Skip("requires python3")
	}
	runner := filepath.Join(t.TempDir(), "run.py")
	if e := os.WriteFile(runner, []byte("import json,sys\nreq=json.load(sys.stdin)\nprint(json.dumps({'result':{'html':json.dumps(json.loads(req['input']) if isinstance(req['input'],str) else req['input'],ensure_ascii=False)}},ensure_ascii=False))\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("AETHER_JS_RUNNER", runner)
	f := setup(t)
	api := busyAPI(f)
	s := newProjectAlertSite(t, f)
	press(t, f, s.owner.TenantID, s.gwA, 0, time.Now().Add(-time.Minute))
	if _, e := f.admin.Exec(`UPDATE core.alerts SET title='SOS · คุณสมชาย ใจดี',device_name='คุณสมชาย ใจดี',note='โทรหาภรรยาคุณสมชาย' WHERE tenant_id=$1`, s.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	panel := map[string]any{"id": "11111111-1111-4111-8111-111111111111", "title": "Alerts", "widget_id": "official-alerts-v1", "decoder_id": "", "gateway_id": s.gwA, "external_id": sosMAC, "width": 2, "height": 1}
	code, out, _ := req(t, api, "POST", "/api/v1/studio/items", "Bearer "+s.ownerToken, "", "", map[string]any{"kind": "dashboard", "name": "ห้องควบคุม", "brand": "Any", "model": "Any", "version": 1, "visibility": "private", "definition": map[string]any{"panels": []any{panel}}})
	if code != 201 {
		t.Fatalf("dashboard: %d %v", code, out)
	}
	dash := out["id"].(string)
	id, pairing := createDisplay(t, api, s.ownerToken, map[string]any{"name": "จอ", "playlist": []map[string]any{{"kind": "studio", "seconds": 30, "ref": dash}}})
	_, token := pairDisplay(t, api, pairing)
	render := func() string {
		t.Helper()
		code, out := kiosk(t, api, "GET", "/studio/"+dash, token)
		if code != 200 {
			t.Fatalf("studio: %d %v", code, out)
		}
		panels := listOf(out, "panels")
		if len(panels) != 1 || panels[0]["html"] == nil {
			t.Fatalf("rendered: %v", out)
		}
		return panels[0]["html"].(string)
	}
	html := render()
	for _, leak := range []string{"สมชาย", "ภรรยา", sosMAC, s.owner.UserID} {
		if strings.Contains(html, leak) {
			t.Fatalf("the studio view passed %q to the widget: %s", leak, html)
		}
	}
	// "fresh" appears only in presence sightings: a worn tag's location must not reach the widget.
	if !strings.Contains(html, "กดปุ่มฉุกเฉิน") || strings.Contains(html, "fresh") {
		t.Fatalf("the widget input was not masked as expected: %s", html)
	}
	// With names allowed, the widget may name them (and the read is logged, see TestDisplayScopeNamesAndAck).
	code, _, _ = req(t, api, "POST", "/api/v1/displays/"+id+"/update", "Bearer "+s.ownerToken, "", "", map[string]any{"name": "จอ", "show_names": true, "playlist": []map[string]any{{"kind": "studio", "seconds": 30, "ref": dash}}})
	if code != 200 {
		t.Fatalf("names on: %d", code)
	}
	html = render()
	if !strings.Contains(html, "สมชาย") || strings.Contains(html, "ภรรยา") {
		t.Fatalf("names allowed: the name should show, the operator's note never: %s", html)
	}
}

// Only an owner widens what a TV does (names, acknowledging), each change is its own audit action, and a TV may
// acknowledge only what takes over its screen.
func TestDisplayOwnerSwitchesAndTakeoverAcks(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	s := newProjectAlertSite(t, f)
	email := memberEmail()
	addMember(t, api, s.ownerToken, email, "admin", nil)
	admin, _ := changeInitialPassword(t, f, api, email, s.owner.TenantID)
	for _, flags := range []map[string]any{{"show_names": true}, {"allow_ack": true}} {
		body := map[string]any{"name": "จอ"}
		for k, v := range flags {
			body[k] = v
		}
		if code, out, _ := req(t, api, "POST", "/api/v1/displays", "Bearer "+admin.AccessToken, "", "", body); code != 403 || out["error"] != "owner_only" {
			t.Fatalf("an admin turned on %v: %d %v", flags, code, out)
		}
	}
	id, pairing := createDisplay(t, api, admin.AccessToken, map[string]any{"name": "จอ"})
	update := func(token string, names, ack bool) int {
		code, _, _ := req(t, api, "POST", "/api/v1/displays/"+id+"/update", "Bearer "+token, "", "", map[string]any{"name": "จอ", "show_names": names, "allow_ack": ack, "playlist": []map[string]any{{"kind": "alerts", "seconds": 20}}})
		return code
	}
	if code := update(admin.AccessToken, false, true); code != 403 {
		t.Fatalf("an admin allowed acks: %d", code)
	}
	if code := update(s.ownerToken, true, true); code != 200 {
		t.Fatalf("the owner: %d", code)
	}
	if code := update(admin.AccessToken, true, false); code != 200 {
		t.Fatalf("an admin turning acks off: %d", code)
	}
	for action, want := range map[string]int{"display.names_on": 1, "display.ack_on": 1, "display.ack_off": 1, "display.names_off": 0} {
		if n := count(t, f.admin, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND target_id=$2 AND action=$3`, s.owner.TenantID, id, action); n != want {
			t.Fatalf("%s audited %d times, want %d", action, n, want)
		}
	}
	if code := update(s.ownerToken, true, true); code != 200 {
		t.Fatalf("acks back on: %d", code)
	}
	_, token := pairDisplay(t, api, pairing)
	// Only what takes over the screen may be acknowledged from it.
	tamper := insertAlert(t, f, s.owner.TenantID, s.gwA, "f000000000b1", domain.EventTamper, "critical", "ป้ายถูกถอด", "ป้าย")
	if code, _ := kiosk(t, api, "POST", "/alerts/"+tamper+"/ack", token); code != 404 {
		t.Fatalf("a TV acknowledged a tamper alert: %d", code)
	}
	hazard := insertAlert(t, f, s.owner.TenantID, s.gwA, "f000000000b2", domain.EventHazard, "warning", "ควัน", "เครื่องตรวจควัน")
	if code, _ := kiosk(t, api, "POST", "/alerts/"+hazard+"/ack", token); code != 204 {
		t.Fatalf("a TV could not acknowledge a hazard: %d", code)
	}
}
