package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/app"
	"aether/backend/internal/config"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const origin = "http://localhost:3000"

type fixture struct {
	repo    *postgres.Repository
	service *app.Service
	admin   *sql.DB
	runtime *sql.DB
	cfg     config.Config
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	adminDSN := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if dsn == "" || adminDSN == "" {
		t.Skip("requires isolated PostgreSQL: TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL")
	}
	for _, s := range []string{dsn, adminDSN} {
		u, e := url.Parse(s)
		if e != nil || u.Path != "/aether_test" {
			t.Fatal("integration tests only run in aether_test")
		}
	}
	admin, e := sql.Open("pgx", adminDSN)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Close() })
	if e = goose.SetDialect("postgres"); e != nil {
		t.Fatal(e)
	}
	if e = goose.Up(admin, "../migrations"); e != nil {
		t.Fatal(e)
	}
	repo, e := postgres.Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { repo.Close() })
	runtime, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	runtime.SetMaxOpenConns(1)
	t.Cleanup(func() { runtime.Close() })
	cfg := config.Config{Origin: origin, Mode: "cloud", Environment: "test", JWTKey: []byte(strings.Repeat("test-key", 8)), Issuer: "aether", Registration: true}
	s, e := app.New(repo, security.NewTokens(cfg.JWTKey, cfg.Issuer), true)
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{repo, s, admin, runtime, cfg}
}
func (f *fixture) account(t *testing.T) (domain.Account, app.AuthResult, domain.Principal) {
	t.Helper()
	email := uuid.NewString() + "@example.test"
	a, e := f.service.Register(context.Background(), email, "correct horse battery staple", "Tester", "Test workspace", false)
	if e != nil {
		t.Fatal(e)
	}
	auth, e := f.service.Login(context.Background(), email, "correct horse battery staple", a.TenantID)
	if e != nil {
		t.Fatal(e)
	}
	p, e := f.service.Authenticate(context.Background(), auth.AccessToken)
	if e != nil {
		t.Fatal(e)
	}
	return a, auth, p
}
func req(t *testing.T, api *fiber.App, method, path, token, cookie, requestOrigin string, payload any) (int, map[string]any, *http.Response) {
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
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	if requestOrigin != "" {
		r.Header.Set("Origin", requestOrigin)
	}
	res, e := api.Test(r, fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	out := map[string]any{}
	if res.StatusCode != 204 {
		if e = json.NewDecoder(res.Body).Decode(&out); e != nil {
			t.Fatal(e)
		}
	}
	return res.StatusCode, out, res
}
func TestTenantIsolationAndIngestion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, _, pa := f.account(t)
	b, _, pb := f.account(t)
	g, secret, e := f.service.CreateGateway(ctx, pa, "Lab gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	d, e := f.service.CreateDevice(ctx, pa, g.ID, "Temperature", "sensor-a", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	tenant, e := f.service.Gateway(ctx, g.ID, secret)
	if e != nil || tenant != a.TenantID {
		t.Fatal(tenant, e)
	}
	if _, e = f.service.Gateway(ctx, g.ID, security.RandomToken()); e == nil {
		t.Fatal("wrong gateway secret accepted")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, e = f.service.Ingest(ctx, tenant, g.ID, d.ID, now, map[string]float64{"temperature": 24.5, "humidity": 50}); e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.Ingest(ctx, tenant, g.ID, d.ID, now.Add(-time.Minute), map[string]float64{"temperature": 10}); e != nil {
		t.Fatal(e)
	}
	duplicate, e := f.service.Ingest(ctx, tenant, g.ID, d.ID, now, map[string]float64{"temperature": 99})
	if e != nil || duplicate.Metrics["temperature"] != 24.5 {
		t.Fatal("duplicate response disagrees with stored event", duplicate, e)
	}
	state, e := f.repo.DeviceState(ctx, pa, d.ID)
	if e != nil || !bytes.Contains(state.Metrics, []byte("24.5")) {
		t.Fatal("state regressed", string(state.Metrics), e)
	}
	if _, e = f.repo.DeviceState(ctx, pb, d.ID); e != domain.ErrNotFound {
		t.Fatal("cross-tenant state visible", e)
	}
	got, e := f.repo.ListDevices(ctx, pb)
	if e != nil || len(got) != 0 {
		t.Fatal("cross-tenant device list", got, e)
	}
	if _, e = f.service.CreateDevice(ctx, pb, g.ID, "Injected", "evil", "generic-environment@1"); e == nil {
		t.Fatal("cross-tenant gateway linked")
	}
	var count int
	if e = f.runtime.QueryRow(`SELECT count(*) FROM core.devices`).Scan(&count); e != nil || count != 0 {
		t.Fatal("missing RLS context must deny", count, e)
	}
	tx, e := f.runtime.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`SELECT set_config('app.tenant_id',$1,true),set_config('app.user_id',$2,true)`, a.TenantID, a.User.ID); e != nil {
		t.Fatal(e)
	}
	// Identity alone is not enough: project access is a second, transaction-local setting (migration
	// 00019), and a user-bound transaction that never computed it sees nothing at all.
	if e = tx.QueryRow(`SELECT count(*) FROM core.devices`).Scan(&count); e != nil || count != 0 {
		t.Fatal("missing project scope must deny", count, e)
	}
	if _, e = tx.Exec(`SELECT set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`); e != nil {
		t.Fatal(e)
	}
	if e = tx.QueryRow(`SELECT count(*) FROM core.devices`).Scan(&count); e != nil || count != 1 {
		t.Fatal("scoped rows", count, e)
	}
	if _, e = tx.Exec(`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id) VALUES($1,$2,$3,'evil','evil','generic-environment@1')`, uuid.NewString(), b.TenantID, g.ID); e == nil {
		t.Fatal("RLS cross-tenant INSERT accepted")
	}
	tx.Rollback()
	if e = f.runtime.QueryRow(`SELECT count(*) FROM core.devices`).Scan(&count); e != nil || count != 0 {
		t.Fatal("pooled connection leaked tenant context", count, e)
	}
	// Composite FK prevents crossing tenants even from a privileged data-maintenance connection.
	if _, e = f.admin.Exec(`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id) VALUES($1,$2,$3,'evil','evil','generic-environment@1')`, uuid.NewString(), b.TenantID, g.ID); e == nil {
		t.Fatal("composite tenant FK missing")
	}
	for i := 0; i < 105; i++ {
		if _, e = f.service.Capture(ctx, tenant, g.ID, []byte(fmt.Sprintf(`{"sample":%d,"tenant_id":"untrusted"}`, i))); e != nil {
			t.Fatal(e)
		}
	}
	if e = f.admin.QueryRow(`SELECT count(*) FROM core.gateway_packets WHERE gateway_id=$1`, g.ID).Scan(&count); e != nil || count != 100 {
		t.Fatal("packet capture not bounded", count, e)
	}
	packets, e := f.repo.ListPackets(ctx, pb, g.ID)
	if e != nil || len(packets) != 0 {
		t.Fatal("cross-tenant packet disclosure")
	}
	// User authorization comes from current DB membership, not stale token roles.
	if _, e = f.admin.Exec(`UPDATE core.memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2`, pa.TenantID, pa.UserID); e != nil {
		t.Fatal(e)
	}
	pa, e = f.repo.Authorize(ctx, pa)
	if e != nil || pa.Role != "viewer" {
		t.Fatal(e)
	}
	if _, _, e = f.service.CreateGateway(ctx, pa, "Forbidden", "minew-mg3"); e != domain.ErrForbidden {
		t.Fatal("viewer can write", e)
	}
	if e = f.service.RevokeGateway(ctx, domain.Principal{UserID: a.User.ID, TenantID: a.TenantID, Role: "owner"}, g.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.Gateway(ctx, g.ID, secret); e != domain.ErrUnauthorized {
		t.Fatal("revoked credential accepted", e)
	}
	if _, e = f.service.Ingest(ctx, tenant, g.ID, d.ID, now.Add(time.Second), map[string]float64{"temperature": 25}); e != domain.ErrUnauthorized {
		t.Fatal("revoked gateway admitted in-flight request", e)
	}
}
func TestRefreshRotationReuseAndLogout(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, auth, p := f.account(t)
	next, e := f.service.Refresh(ctx, auth.RefreshToken)
	if e != nil {
		t.Fatal(e)
	}
	if next.RefreshToken == auth.RefreshToken {
		t.Fatal("refresh did not rotate")
	}
	if _, e = f.service.Refresh(ctx, auth.RefreshToken); e != domain.ErrUnauthorized {
		t.Fatal("reuse accepted", e)
	}
	if _, e = f.service.Refresh(ctx, next.RefreshToken); e != domain.ErrUnauthorized {
		t.Fatal("family revocation rolled back", e)
	}
	if _, e = f.service.Authenticate(ctx, next.AccessToken); e != domain.ErrUnauthorized {
		t.Fatal("revoked access accepted", e)
	}
	_, auth, p = f.account(t)
	if e = f.repo.RevokeSession(ctx, p); e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.Authenticate(ctx, auth.AccessToken); e != domain.ErrUnauthorized {
		t.Fatal("logout did not revoke access")
	}
	if _, e = f.service.Refresh(ctx, auth.RefreshToken); e != domain.ErrUnauthorized {
		t.Fatal("logout did not revoke refresh")
	}
}
func TestConcurrentRefreshReplay(t *testing.T) {
	f := setup(t)
	_, auth, _ := f.account(t)
	results := make(chan app.AuthResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := f.service.Refresh(context.Background(), auth.RefreshToken)
			results <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	success := 0
	for e := range errs {
		if e == nil {
			success++
		} else if e != domain.ErrUnauthorized {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatal("refresh not single-use", success)
	}
	for r := range results {
		if r.AccessToken != "" {
			if _, e := f.service.Authenticate(context.Background(), r.AccessToken); e != domain.ErrUnauthorized {
				t.Fatal("race did not revoke family")
			}
		}
	}
}
func TestAPIBoundaries(t *testing.T) {
	f := setup(t)
	_, auth, p := f.account(t)
	api := httpapi.New(f.cfg, f.service, f.repo)
	defer api.Shutdown()
	if code, _, _ := req(t, api, "GET", "/api/v1/devices", "", "", "", nil); code != 401 {
		t.Fatal(code)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/auth/refresh", "", "aether_refresh_dev="+auth.RefreshToken, "https://evil.example", nil); code != 403 {
		t.Fatal("CSRF origin accepted", code)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/gateways", "Bearer "+auth.AccessToken, "", "", map[string]any{"name": "Gateway", "model": "minew-mg3", "tenant_id": uuid.NewString()}); code != 400 {
		t.Fatal("tenant injection accepted", code)
	}
	code, out, _ := req(t, api, "POST", "/api/v1/gateways", "Bearer "+auth.AccessToken, "", "", map[string]any{"name": "Gateway", "model": "minew-mg3"})
	if code != 201 {
		t.Fatal(code, out)
	}
	g := out["gateway"].(map[string]any)["id"].(string)
	secret := out["token"].(string)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(g+":"+secret))
	code, out, _ = req(t, api, "POST", "/ingest/gateways/"+g+"/packets", basic, "", "", map[string]any{"arbitrary": "sample"})
	if code != 202 || out["decoded"] != false {
		t.Fatal(code, out)
	}
	if code, _, _ := req(t, api, "POST", "/ingest/gateways/"+g+"/packets", "", "", "", map[string]any{}); code != 401 {
		t.Fatal("anonymous capture accepted", code)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/gateways", "Bearer "+auth.AccessToken, "", "", nil)
	b, _ := json.Marshal(out)
	if code != 200 || bytes.Contains(b, []byte(secret)) || bytes.Contains(b, []byte("token_hash")) {
		t.Fatal("credential disclosure")
	}
	// JWT header-selected tenant never influences authenticated routing.
	bad := httptest.NewRequest("GET", "/api/v1/me", nil)
	bad.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	bad.Header.Set("X-Tenant-ID", uuid.NewString())
	res, e := api.Test(bad)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	var me map[string]any
	json.NewDecoder(res.Body).Decode(&me)
	if me["tenant_id"] != p.TenantID {
		t.Fatal("header overrode identity")
	}
	code, _, res = req(t, api, "POST", "/api/v1/auth/refresh", "", "aether_refresh_dev="+auth.RefreshToken, origin, nil)
	if code != 200 {
		t.Fatal(code)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe refresh cookie")
	}
	cfg := f.cfg
	cfg.SecureCookies = true
	cfg.Origin = "https://aether.example"
	prod := httpapi.New(cfg, f.service, f.repo)
	defer prod.Shutdown()
	nextCookie := cookies[0].Value
	code, _, res = req(t, prod, "POST", "/api/v1/auth/refresh", "", "__Host-aether_refresh="+nextCookie, cfg.Origin, nil)
	if code != 200 {
		t.Fatal(code)
	}
	c := res.Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" {
		t.Fatal("production cookie requirements")
	}
}
func TestRejectPrivilegedDatabaseRole(t *testing.T) {
	f := setup(t)
	if repo, e := postgres.Open(os.Getenv("TEST_ADMIN_DATABASE_URL")); e == nil {
		repo.Close()
		t.Fatal("superuser API role accepted")
	}
	_ = f
}
func TestBootstrapNotPublicAndOneTime(t *testing.T) {
	f := setup(t)
	f.service.Registration = false
	if _, e := f.service.Register(context.Background(), "x@example.test", "correct horse battery staple", "Name", "Org", false); e != domain.ErrForbidden {
		t.Fatal("public bootstrap exposed", e)
	}
	f.accountWithRegistration(t)
	if _, e := f.service.Register(context.Background(), "bootstrap@example.test", "correct horse battery staple", "Name", "Org", true); e != domain.ErrConflict {
		t.Fatal("bootstrap can repeat", e)
	}
}
func (f *fixture) accountWithRegistration(t *testing.T) {
	f.service.Registration = true
	f.account(t)
	f.service.Registration = false
}

func TestCloudAndOnpremServeSameAPISafely(t *testing.T) {
	f := setup(t)
	a, auth, _ := f.account(t)
	for _, mode := range []string{"cloud", "onprem"} {
		cfg := f.cfg
		cfg.Mode = mode
		api := httpapi.New(cfg, f.service, f.repo)
		code, out, _ := req(t, api, "GET", "/api/v1/me", "Bearer "+auth.AccessToken, "", "", nil)
		if code != 200 || out["deployment_mode"] != mode || out["tenant_id"] != a.TenantID {
			t.Fatal(code, out)
		}
		api.Shutdown()
	}
}
func TestSuspendedTenantCannotUseGatewayOrSession(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, auth, p := f.account(t)
	g, token, e := f.service.CreateGateway(ctx, p, "Gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.admin.Exec(`UPDATE core.tenants SET status='suspended' WHERE id=$1`, a.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.Gateway(ctx, g.ID, token); e != domain.ErrUnauthorized {
		t.Fatal("suspended gateway accepted", e)
	}
	if _, e = f.service.Authenticate(ctx, auth.AccessToken); e != domain.ErrUnauthorized {
		t.Fatal("suspended session accepted", e)
	}
	if _, e = f.service.Refresh(ctx, auth.RefreshToken); e != domain.ErrUnauthorized {
		t.Fatal("suspended refresh accepted", e)
	}
}
func TestAuthRateLimitAndGenericErrors(t *testing.T) {
	f := setup(t)
	api := httpapi.New(f.cfg, f.service, f.repo)
	defer api.Shutdown()
	for i := 0; i < 11; i++ {
		code, out, _ := req(t, api, "POST", "/api/v1/auth/refresh", "", "aether_refresh_dev=invalid", origin, nil)
		want := 401
		if i == 10 {
			want = 429
		}
		if code != want {
			t.Fatal(i, code, out)
		}
	}
}

func TestLiveDashboardIsolation(t *testing.T) {
	f := setup(t)
	_, auth, p := f.account(t)
	_, other, _ := f.account(t)
	api := httpapi.New(f.cfg, f.service, f.repo)
	defer api.Shutdown()
	g, _, e := f.service.CreateGateway(context.Background(), p, "Live gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.service.Capture(context.Background(), p.TenantID, g.ID, []byte(`[{"type":"Gateway","mac":"111111111111"},{"mac":"aabbccddeeff","rssi":-60,"rawData":"1016e1ffa101641c8a2f51000000000000"}]`))
	if e != nil {
		t.Fatal(e)
	}
	code, out, _ := req(t, api, "GET", "/api/v1/live", "Bearer "+auth.AccessToken, "", "", nil)
	b, _ := json.Marshal(out)
	if code != 200 || !bytes.Contains(b, []byte("28.5390625")) {
		t.Fatal("live reading unavailable", code)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/live", "Bearer "+other.AccessToken, "", "", nil)
	b, _ = json.Marshal(out)
	if code != 200 || bytes.Contains(b, []byte(g.ID)) || bytes.Contains(b, []byte("aabbccddeeff")) {
		t.Fatal("cross tenant disclosure", code)
	}
	if code, _, _ := req(t, api, "GET", "/api/v1/live", "", "", "", nil); code != 401 {
		t.Fatal("anonymous live view", code)
	}
}

func TestPersistentHistoryAndTemplates(t *testing.T) {
	f := setup(t)
	_, _, p := f.account(t)
	_, _, other := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, p, "History gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	payload := []byte(`[{"type":"Gateway","mac":"111111111111"},{"mac":"aabbccddeeff","rawData":"1016e1ffa101641c8a2f51000000000000"}]`)
	for range 2 {
		if _, e = f.service.Capture(ctx, p.TenantID, g.ID, payload); e != nil {
			t.Fatal(e)
		}
	}
	rows, e := f.repo.StreamHistory(ctx, p, g.ID, time.Now().Add(-time.Hour), 200)
	if e != nil || len(rows) != 1 || len(rows[0].History) != 1 {
		t.Fatal("dedupe/history failed", e, len(rows))
	}
	high := 27.0
	template, e := f.service.CreateTemplate(ctx, p, domain.DeviceTemplate{Name: "Server room", Version: 1, DecoderID: "minew-ffe1-a101@1", Definition: domain.TemplateDefinition{TemperatureHigh: &high}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.CreateTemplate(ctx, p, template); e != domain.ErrConflict {
		t.Fatal("immutable version duplicate accepted", e)
	}
	list, e := f.repo.ListTemplates(ctx, p)
	if e != nil || len(list) != 1 || list[0].Definition.TemperatureHigh == nil || *list[0].Definition.TemperatureHigh != 27 {
		t.Fatal("template reload failed", e)
	}
	if e = f.service.AssignTemplate(ctx, p, g.ID, "aabbccddeeff", "Server room sensor", template.ID); e != nil {
		t.Fatal(e)
	}
	if e = f.service.AssignTemplate(ctx, other, g.ID, "aabbccddeeff", "Hacked", template.ID); e == nil {
		t.Fatal("cross tenant assignment")
	}
	if rows, e = f.repo.StreamHistory(ctx, other, g.ID, time.Now().Add(-time.Hour), 200); e != nil || len(rows) != 0 {
		t.Fatal("history leaked")
	}
	// Remove the diagnostic packets by exercising the real pruning path.
	for range 101 {
		if _, e = f.service.Capture(ctx, p.TenantID, g.ID, []byte(`[]`)); e != nil {
			t.Fatal(e)
		}
	}
	rows, e = f.repo.StreamHistory(ctx, p, g.ID, time.Now().Add(-time.Hour), 200)
	if e != nil || len(rows) != 1 || len(rows[0].History) != 1 || rows[0].Name != "Server room sensor" || rows[0].TemplateID == nil || *rows[0].TemplateID != template.ID {
		t.Fatal("history or assignment lost", e)
	}
	viewer := p
	viewer.Role = "viewer"
	if e = f.service.AssignTemplate(ctx, viewer, g.ID, "aabbccddeeff", "Bad", template.ID); e != domain.ErrForbidden {
		t.Fatal("viewer can edit")
	}
	if _, e = f.service.CreateTemplate(ctx, viewer, template); e != domain.ErrForbidden {
		t.Fatal("viewer can create")
	}
	var recorded sql.NullString
	if e = f.admin.QueryRow(`SELECT template_id FROM core.sensor_samples WHERE tenant_id=$1 AND gateway_id=$2`, p.TenantID, g.ID).Scan(&recorded); e != nil || recorded.Valid {
		t.Fatal("assignment changed historic version", e)
	}
}
