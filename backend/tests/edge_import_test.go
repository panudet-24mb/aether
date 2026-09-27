package tests

import (
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// Fake local keys and credentials of the scrubbed Tuya cloud fixtures (internal/adapters/tuyacloud/testdata).
var fakeLocalKeys = []string{"FAKEKEY-sw01-000", "FAKEKEY-pl01-000", "FAKEKEY-zb01-000"}

const fakeAccessID, fakeAccessSecret = "fakeaccessid0001", "fakesecret0000000000000000000001"

// fakeTuyaCloud replays the tuyacloud fixtures. mode "empty" answers an empty device list, "auth" a rejected sign.
func fakeTuyaCloud(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	read := func(name string) string {
		b, e := os.ReadFile(filepath.Join("..", "internal", "adapters", "tuyacloud", "testdata", name))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("sign") == "" || r.Header.Get("client_id") != fakeAccessID {
			w.Write([]byte(`{"success":false,"code":1004,"msg":"sign invalid"}`))
			return
		}
		switch {
		case mode == "auth":
			w.Write([]byte(`{"success":false,"code":1004,"msg":"sign invalid"}`))
		case r.URL.Path == "/v1.0/token":
			w.Write([]byte(`{"success":true,"result":{"access_token":"faketoken-1","expire_time":7200,"uid":"az_fake_uid"}}`))
		case r.URL.Path == "/v1.0/iot-01/associated-users/devices" && mode == "empty":
			w.Write([]byte(`{"success":true,"result":{"has_more":false,"devices":[]}}`))
		case r.URL.Path == "/v1.0/iot-01/associated-users/devices" && r.URL.Query().Get("last_row_key") == "":
			w.Write([]byte(read("devices_page1.json")))
		case r.URL.Path == "/v1.0/iot-01/associated-users/devices":
			w.Write([]byte(read("devices_page2.json")))
		case strings.HasSuffix(r.URL.Path, "/model") && strings.Contains(r.URL.Path, "pl01"):
			w.Write([]byte(`{"success":false,"code":28841101,"msg":"No permissions. This API is not subscribed."}`))
		case strings.HasSuffix(r.URL.Path, "/model"):
			w.Write([]byte(read("model_light.json")))
		case strings.HasSuffix(r.URL.Path, "/specifications"):
			w.Write([]byte(read("specifications_plug.json")))
		default:
			w.WriteHeader(404)
		}
	}))
}

// importRig is a workspace with a fresh Aether Edge gateway whose Tuya cloud is a fake server.
type importRig struct {
	f              *fixture
	api            *fiber.App
	ctx            context.Context
	token          string // owner's bearer
	owner          domain.Principal
	gateway, gwKey string
	bodies         []string // every /api/v1 response body, scanned for leaked keys
	t              *testing.T
}

func newImportRig(t *testing.T, mode string) *importRig {
	t.Helper()
	f := setup(t)
	srv := fakeTuyaCloud(t, mode)
	t.Cleanup(srv.Close)
	f.service.TuyaCloud = func(region, id, secret string) (app.TuyaCloud, error) {
		if _, ok := tuyacloud.Regions[region]; !ok {
			return nil, errors.New("region")
		}
		return tuyacloud.NewForTest(srv.URL, id, secret, srv.Client(), nil), nil
	}
	r := &importRig{f: f, api: busyAPI(f), ctx: context.Background(), t: t}
	_, auth, owner := f.account(t)
	r.token, r.owner = "Bearer "+auth.AccessToken, owner
	g, key, e := f.service.CreateGatewayIn(r.ctx, owner, "Edge import", domain.EdgeGatewayModel, nil)
	if e != nil {
		t.Fatal(e)
	}
	r.gateway, r.gwKey = g.ID, key
	return r
}

func (r *importRig) call(method, path string, payload any) (int, map[string]any) {
	r.t.Helper()
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = strings.NewReader(string(b))
	}
	q := httptest.NewRequest(method, path, body)
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Authorization", r.token)
	res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		r.t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r.bodies = append(r.bodies, string(raw))
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func (r *importRig) importJob(region string) map[string]any {
	r.t.Helper()
	code, job := r.call("POST", "/api/v1/gateways/"+r.gateway+"/tuya/imports", map[string]any{"region": region, "access_id": fakeAccessID, "access_secret": fakeAccessSecret})
	if code != 202 || job["status"] != "running" {
		r.t.Fatalf("start import: %d %v", code, job)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		code, out := r.call("GET", "/api/v1/gateways/"+r.gateway+"/tuya/imports/"+job["id"].(string), nil)
		if code != 200 {
			r.t.Fatalf("poll: %d %v", code, out)
		}
		if out["status"] != "running" {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatal("import did not finish")
	return nil
}

// agent calls the ingest group as the Edge would, with its gateway's HTTP Basic credential.
func (r *importRig) agent(token, etag string) (int, map[string]any, string) {
	r.t.Helper()
	q := httptest.NewRequest("GET", "/ingest/gateways/"+r.gateway+"/edge/config", nil)
	q.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(r.gateway+":"+token)))
	if etag != "" {
		q.Header.Set("If-None-Match", etag)
	}
	res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		r.t.Fatal(e)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out, res.Header.Get("ETag")
}

func (r *importRig) count(q string, args ...any) int {
	r.t.Helper()
	var n int
	if e := r.f.admin.QueryRowContext(r.ctx, q, args...).Scan(&n); e != nil {
		r.t.Fatal(e)
	}
	return n
}

// The whole import: keys sealed and fingerprinted, nothing secret in any API response or stored in the clear,
// the credentials nowhere, the agent's config carrying only registered devices' keys.
func TestTuyaImportAndEdgeConfig(t *testing.T) {
	r := newImportRig(t, "")
	job := r.importJob("us")
	if job["status"] != "done" || job["found"] != float64(3) || job["imported"] != float64(3) || job["with_key"] != float64(3) || job["local_capable"] != float64(2) {
		t.Fatalf("job: %v", job)
	}
	for _, raw := range job["devices"].([]any) {
		d := raw.(map[string]any)
		if d["has_key"] != true || len(d["key_fingerprint"].(string)) != 16 {
			t.Fatalf("device: %v", d)
		}
		if d["tuya_id"] == "bf00000000000000pl01" && d["spec"] != "specifications" {
			t.Fatalf("model fallback: %v", d)
		}
		if d["tuya_id"] == "bf00000000000000zb01" && d["local_capable"] != false {
			t.Fatalf("a hub sub-device is not reachable locally: %v", d)
		}
	}
	code, list := r.call("GET", "/api/v1/gateways/"+r.gateway+"/tuya/devices", nil)
	if code != 200 || len(list["items"].([]any)) != 3 {
		t.Fatalf("devices: %d %v", code, list)
	}
	// Nothing secret stored in the clear: not the local keys, not the Access Secret, anywhere it could land.
	for _, secret := range append([]string{fakeAccessSecret}, fakeLocalKeys...) {
		for _, table := range []string{"core.tuya_devices", "core.audit_logs", "core.gateway_packets", "core.edge_agents", "core.gateways"} {
			if n := r.count(`SELECT count(*) FROM `+table+` x WHERE x::text LIKE '%'||$1||'%'`, secret); n != 0 {
				t.Fatalf("%s found in %s", secret, table)
			}
		}
	}
	if r.count(`SELECT count(*) FROM core.audit_logs WHERE action='tuya.keys_imported' AND target_id=$1`, r.gateway) != 1 {
		t.Fatal("import not audited")
	}
	// The agent gets only registered, keyed, locally reachable devices, with the key in the clear.
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Plug", "bf00000000000000pl01", domain.TuyaWiFiProfile); e != nil {
		t.Fatal(e)
	}
	code, cfg, etag := r.agent(r.gwKey, "")
	devices, _ := cfg["devices"].([]any)
	if code != 200 || len(devices) != 1 || etag == "" {
		t.Fatalf("config: %d %v %q", code, cfg, etag)
	}
	d := devices[0].(map[string]any)
	if d["id"] != "bf00000000000000pl01" || d["key"] != "FAKEKEY-pl01-000" || len(d["refresh_dps"].([]any)) == 0 {
		t.Fatalf("config device: %v", d)
	}
	if code, _, _ := r.agent(r.gwKey, etag); code != 304 {
		t.Fatalf("unchanged config: %d", code)
	}
	if code, _, _ := r.agent("x"+r.gwKey[1:], ""); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	// Forgetting the key drops the device from the agent's config and moves the revision.
	if code, out := r.call("POST", "/api/v1/gateways/"+r.gateway+"/tuya/devices/bf00000000000000pl01/forget", nil); code != 204 {
		t.Fatalf("forget: %d %v", code, out)
	}
	code, cfg, etag2 := r.agent(r.gwKey, etag)
	if code != 200 || etag2 == etag || len(cfg["devices"].([]any)) != 0 {
		t.Fatalf("after forget: %d %v %q", code, cfg, etag2)
	}
	for _, body := range r.bodies {
		for _, secret := range append([]string{fakeAccessSecret}, fakeLocalKeys...) {
			if strings.Contains(body, secret) {
				t.Fatalf("%s leaked in an API response: %s", secret, body)
			}
		}
	}
	// Revoked: the agent is refused.
	if e := r.f.service.RevokeGateway(r.ctx, r.owner, r.gateway); e != nil {
		t.Fatal(e)
	}
	if code, _, _ := r.agent(r.gwKey, ""); code != 401 {
		t.Fatalf("revoked gateway: %d", code)
	}
}

func TestTuyaImportFailuresAndHints(t *testing.T) {
	r := newImportRig(t, "empty")
	job := r.importJob("us")
	if job["status"] != "done" || job["hint"] != "no_devices_try_other_region" {
		t.Fatalf("empty: %v", job)
	}
	if s, _ := job["suggest_regions"].([]any); len(s) == 0 || s[0] != "sg" {
		t.Fatalf("suggestion: %v", job["suggest_regions"])
	}
	a := newImportRig(t, "auth")
	if job := a.importJob("sg"); job["status"] != "failed" || job["error"] != "tuya_auth_failed" {
		t.Fatalf("auth: %v", job)
	}
	for _, body := range []map[string]any{
		{"region": "mars", "access_id": fakeAccessID, "access_secret": fakeAccessSecret},
		{"region": "us", "access_id": "short", "access_secret": fakeAccessSecret},
		{"region": "us", "access_id": fakeAccessID, "access_secret": fakeAccessSecret, "host": "https://evil.example"},
	} {
		if code, out := r.call("POST", "/api/v1/gateways/"+r.gateway+"/tuya/imports", body); code != 400 {
			t.Fatalf("%v: %d %v", body, code, out)
		}
	}
	// Not an Edge gateway.
	g, _, e := r.f.service.CreateGatewayIn(r.ctx, r.owner, "BLE", "minew-mg3", nil)
	if e != nil {
		t.Fatal(e)
	}
	if code, out := r.call("POST", "/api/v1/gateways/"+g.ID+"/tuya/imports", map[string]any{"region": "us", "access_id": fakeAccessID, "access_secret": fakeAccessSecret}); code != 400 || out["error"] != "not_an_edge_gateway" {
		t.Fatalf("ble gateway: %d %v", code, out)
	}
	// Only owner/admin may import, see keys' status or issue install codes.
	viewer := r.owner
	viewer.Role = "viewer"
	if _, e := r.f.service.StartTuyaImport(r.ctx, viewer, r.gateway, "us", fakeAccessID, fakeAccessSecret); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("viewer import: %v", e)
	}
	if _, _, e := r.f.service.CreateEdgeInstallCode(r.ctx, viewer, r.gateway); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("viewer install code: %v", e)
	}
	// Another workspace cannot read this workspace's job.
	job = r.importJob("us")
	other := newImportRig(t, "")
	if code, _ := other.call("GET", "/api/v1/gateways/"+r.gateway+"/tuya/imports/"+job["id"].(string), nil); code != 404 {
		t.Fatalf("cross-tenant job: %d", code)
	}
}

func TestEdgeInstallCodeBootstrap(t *testing.T) {
	t.Setenv("MQTT_PUBLIC_HOST", "mqtt.example.test")
	t.Setenv("MQTT_PUBLIC_PORT", "8883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "ssl")
	// A misconfigured mount: the CA certificate bundled with its private key. Only the certificate may leave.
	certPEM, keyPEM := testCA(t)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if e := os.WriteFile(ca, append(append([]byte{}, certPEM...), keyPEM...), 0o644); e != nil {
		t.Fatal(e)
	}
	r := newImportRig(t, "")
	bootstrap := func(api *fiber.App, code string) (int, map[string]any) {
		b, _ := json.Marshal(map[string]string{"code": code})
		q := httptest.NewRequest("POST", "/edge/bootstrap", strings.NewReader(string(b)))
		q.Header.Set("Content-Type", "application/json")
		res, e := api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	issue := func() string {
		code, out := r.call("POST", "/api/v1/gateways/"+r.gateway+"/edge/install-code", nil)
		if code != 201 || len(out["code"].(string)) != 26 || !strings.Contains(out["install_command"].(string), out["code"].(string)) {
			t.Fatalf("install code: %d %v", code, out)
		}
		return out["code"].(string)
	}
	// Without the broker CA the code is not spent: the deployment cannot hand over a complete bundle.
	first := issue()
	if code, _ := bootstrap(r.api, first); code != 503 {
		t.Fatalf("missing CA: %d", code)
	}
	t.Setenv("MQTT_CA_FILE", ca)
	if r.count(`SELECT count(*) FROM core.edge_install_codes WHERE gateway_id=$1 AND redeemed_at IS NULL`, r.gateway) != 1 {
		t.Fatal("the code was spent by a failed bootstrap")
	}
	code, bundle := bootstrap(r.api, first)
	if code != 201 || bundle["gateway_id"] != r.gateway || bundle["ca_pem"] != string(certPEM) || bundle["notice"] != "previous_agent_must_stop" {
		t.Fatalf("bootstrap: %d %v", code, bundle)
	}
	if raw, _ := json.Marshal(bundle); strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("the CA private key was handed to the installer")
	}
	mqtt := bundle["mqtt"].(map[string]any)
	httpCreds := bundle["http"].(map[string]any)
	if mqtt["url"] != "mqtts://mqtt.example.test:8883" || mqtt["username"] != "gw-"+r.gateway || mqtt["password"] == "" || mqtt["base_topic"] != "aether/edge/"+r.gateway {
		t.Fatalf("mqtt: %v", mqtt)
	}
	// Once only.
	if code, _ := bootstrap(r.api, first); code != 401 {
		t.Fatalf("second redeem: %d", code)
	}
	// The credentials were rotated: the gateway's original HTTP token no longer works, the new one does, and the
	// broker account was (re)issued.
	if code, _, _ := r.agent(r.gwKey, ""); code != 401 {
		t.Fatalf("old token still works: %d", code)
	}
	if code, _, _ := r.agent(httpCreds["token"].(string), ""); code != 200 {
		t.Fatalf("new token: %d", code)
	}
	if r.count(`SELECT count(*) FROM core.mqtt_accounts WHERE gateway_id=$1`, r.gateway) != 1 {
		t.Fatal("no broker account")
	}
	if r.count(`SELECT count(*) FROM core.audit_logs WHERE action='edge.bootstrapped' AND target_id=$1`, r.gateway) != 1 {
		t.Fatal("bootstrap not audited")
	}
	// A newer code replaces an older unused one; an expired code is refused.
	older := issue()
	newer := issue()
	if code, _ := bootstrap(r.api, older); code != 401 {
		t.Fatalf("superseded code: %d", code)
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.edge_install_codes SET expires_at=now()-interval '1 minute' WHERE gateway_id=$1 AND redeemed_at IS NULL`, r.gateway); e != nil {
		t.Fatal(e)
	}
	if code, _ := bootstrap(r.api, newer); code != 401 {
		t.Fatalf("expired code: %d", code)
	}
	for _, junk := range []string{"not-a-code", strings.Repeat("a", 26), "ABCDEFGHIJKLMNOPQRSTUVWXYZ", strings.Repeat("a", 300)} {
		if code, _ := bootstrap(r.api, junk); code != 401 {
			t.Fatalf("%q: %d", junk, code)
		}
	}
	// The code never goes in the path: the old path form is not a route.
	q := httptest.NewRequest("POST", "/edge/bootstrap/"+newer, nil)
	if res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second}); e != nil || res.StatusCode != 404 {
		t.Fatalf("path form still routed: %v %v", res, e)
	}
	// Install codes are only for Aether Edge gateways.
	g, _, e := r.f.service.CreateGatewayIn(r.ctx, r.owner, "BLE", "minew-mg3", nil)
	if e != nil {
		t.Fatal(e)
	}
	if code, out := r.call("POST", "/api/v1/gateways/"+g.ID+"/edge/install-code", nil); code != 400 || out["error"] != "not_an_edge_gateway" {
		t.Fatalf("ble install code: %d %v", code, out)
	}
	// The public route has its own tight per-address limit.
	fresh := busyAPI(r.f)
	limited := false
	for i := 0; i < 12; i++ {
		if code, _ := bootstrap(fresh, strings.Repeat("b", 26)); code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("bootstrap not rate limited")
	}
}

// RotateGatewayToken: the old HTTP token stops working at once.
func TestRotateGatewayToken(t *testing.T) {
	r := newImportRig(t, "")
	if code, _, _ := r.agent(r.gwKey, ""); code != 200 {
		t.Fatalf("before: %d", code)
	}
	token := security.RandomToken()
	if e := r.f.repo.RotateGatewayToken(r.ctx, r.owner, r.gateway, security.Digest(token)); e != nil {
		t.Fatal(e)
	}
	if code, _, _ := r.agent(r.gwKey, ""); code != 401 {
		t.Fatalf("old token: %d", code)
	}
	if code, _, _ := r.agent(token, ""); code != 200 {
		t.Fatalf("new token: %d", code)
	}
	other := newImportRig(t, "")
	if e := other.f.repo.RotateGatewayToken(other.ctx, other.owner, r.gateway, security.Digest("x")); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("another workspace's gateway: %v", e)
	}
}

// testCA makes a throwaway self-signed CA certificate and its private key, PEM encoded.
func testCA(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Aether test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, e := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	keyDER, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
