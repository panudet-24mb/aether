package edge

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testZigbee = "7a6b5c4d-3e2f-4a1b-9c8d-7e6f5a4b3c2d"

// testCertPEM is a throwaway self-signed certificate, plus (keyPEM) its private key.
func testCertPEM(t *testing.T) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now(),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

func bootstrapServer(t *testing.T, status int, bundle map[string]any, seen *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/edge/bootstrap" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if seen != nil {
			*seen = string(b)
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(bundle)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testBundle(ca string, zigbee bool) map[string]any {
	b := map[string]any{
		"gateway_id": testGateway, "api_origin": "https://aether.example",
		"http":   map[string]any{"token": testToken},
		"mqtt":   map[string]any{"url": "mqtts://aether.example:8883", "tls": true, "username": "gw-" + testGateway, "password": testPass},
		"ca_pem": ca,
	}
	if zigbee {
		b["zigbee"] = map[string]any{"gateway_id": testZigbee, "base_topic": "aether/z2m/" + testZigbee, "server": "mqtts://aether.example:8883",
			"username": "gw-" + testZigbee, "password": "zigbee-secret-it's", "tls": true}
	}
	return b
}

const testCode = "ABCDEFGHJKLMNPQRSTUVWXYZ23"

func TestInstallWritesTheAgentFiles(t *testing.T) {
	ca, _ := testCertPEM(t)
	var body string
	srv := bootstrapServer(t, 201, testBundle(ca, true), &body)
	dir := filepath.Join(t.TempDir(), "edge")
	var out bytes.Buffer
	o := InstallOptions{Server: srv.URL, Code: testCode, Dir: dir, Zigbee: "192.168.1.40", Image: "ghcr.io/panudet-24mb/aether-edge:0.1.0", Client: srv.Client(), Out: &out}
	if e := Install(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	if body != `{"code":"`+testCode+`"}` {
		t.Fatalf("bootstrap body %q", body)
	}
	mode := func(p string) os.FileMode {
		st, e := os.Stat(p)
		if e != nil {
			t.Fatal(e)
		}
		return st.Mode().Perm()
	}
	if m := mode(dir); m != 0o700 {
		t.Fatalf("dir mode %o", m)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	for _, want := range []string{"EDGE_IMAGE=ghcr.io/panudet-24mb/aether-edge:0.1.0", "GATEWAY_ID=" + testGateway, "HTTP_TOKEN=" + testToken,
		"MQTT_URL=mqtts://aether.example:8883", "MQTT_PASSWORD=" + testPass, "MQTT_CA_FILE=/config/ca.crt", "COMPOSE_PROFILES=zigbee",
		"Z2M_IMAGE=" + Zigbee2MQTTImage, "Z2M_UI_BIND=127.0.0.1", "AETHER_API=https://aether.example"} {
		if !strings.Contains(string(env), want+"\n") {
			t.Fatalf(".env lacks %q", want)
		}
	}
	for path, want := range map[string]os.FileMode{".env": 0o600, "compose.yaml": 0o600, "ca.crt": 0o644,
		"zigbee2mqtt/secret.yaml": 0o600, "zigbee2mqtt/configuration.yaml": 0o600} {
		if m := mode(filepath.Join(dir, path)); m != want {
			t.Fatalf("%s mode %o, want %o", path, m, want)
		}
	}
	compose, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	for _, want := range []string{"image: ${EDGE_IMAGE}", "network_mode: host", "read_only: true", "cap_drop: [ALL]",
		"- ./ca.crt:/config/ca.crt:ro", "image: ${Z2M_IMAGE}", "profiles: [zigbee]", `"${Z2M_UI_BIND:-127.0.0.1}:8080:8080"`} {
		if !strings.Contains(string(compose), want) {
			t.Fatalf("compose.yaml lacks %q:\n%s", want, compose)
		}
	}
	if strings.Contains(string(compose), testPass) || strings.Contains(string(compose), "__") {
		t.Fatalf("compose.yaml holds a secret or an unfilled placeholder:\n%s", compose)
	}
	// Both services drop every capability; nothing publishes on every interface.
	if strings.Count(string(compose), "cap_drop: [ALL]") != 2 || strings.Count(string(compose), "no-new-privileges:true") != 2 ||
		strings.Contains(string(compose), `"8080:8080"`) || strings.Contains(string(compose), "0.0.0.0") || strings.Contains(string(compose), "docker.sock") {
		t.Fatalf("compose.yaml hardening:\n%s", compose)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "zigbee2mqtt/configuration.yaml"))
	for _, want := range []string{"base_topic: aether/z2m/" + testZigbee, "user: '!secret user'", "password: '!secret password'",
		"port: tcp://192.168.1.40:6638", "adapter: ember", "ca: /app/data/aether-ca.crt", "availability:", "network_key: GENERATE",
		"auth_token: '!secret auth_token'"} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("configuration.yaml lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(string(cfg), "zigbee-secret") {
		t.Fatal("the Zigbee2MQTT password belongs in secret.yaml only")
	}
	secret, _ := os.ReadFile(filepath.Join(dir, "zigbee2mqtt/secret.yaml"))
	if !strings.Contains(string(secret), "password: 'zigbee-secret-it''s'") {
		t.Fatalf("secret.yaml: %s", secret)
	}
	token := existingToken(filepath.Join(dir, "zigbee2mqtt/secret.yaml"))
	if len(token) < 22 || !strings.Contains(out.String(), token) || !strings.Contains(out.String(), "ssh -L 8080:127.0.0.1:8080") {
		t.Fatalf("page token %q not generated or not shown once: %s", token, out.String())
	}

	// A second install (a new code, e.g. after a rotation) rewrites the credentials but never the Zigbee2MQTT
	// configuration, which by then holds the network key.
	// An older configuration (page without a token, network key already generated) keeps every line and gains only
	// the token reference; the page token itself is kept across installs.
	older := "mqtt:\n  base_topic: aether/z2m/x\nfrontend:\n  enabled: true\n  port: 8080\nadvanced:\n  network_key: [1, 2, 3]\n"
	os.WriteFile(filepath.Join(dir, "zigbee2mqtt/configuration.yaml"), []byte(older), 0o600)
	next := testBundle(ca, true)
	next["zigbee"].(map[string]any)["password"] = "rotated-zigbee-password"
	srv2 := bootstrapServer(t, 201, next, nil)
	o.Server, o.Client, o.ZigbeeUI = srv2.URL, srv2.Client(), "192.168.1.5"
	out.Reset()
	if e := Install(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	want := "mqtt:\n  base_topic: aether/z2m/x\nfrontend:\n  auth_token: '!secret auth_token'\n  enabled: true\n  port: 8080\nadvanced:\n  network_key: [1, 2, 3]\n"
	if cfg, _ := os.ReadFile(filepath.Join(dir, "zigbee2mqtt/configuration.yaml")); string(cfg) != want {
		t.Fatalf("existing Zigbee2MQTT configuration:\n%s", cfg)
	}
	if secret, _ := os.ReadFile(filepath.Join(dir, "zigbee2mqtt/secret.yaml")); !strings.Contains(string(secret), "rotated-zigbee-password") ||
		existingToken(filepath.Join(dir, "zigbee2mqtt/secret.yaml")) != token {
		t.Fatal("secret.yaml not rewritten, or the page token changed")
	}
	if env, _ := os.ReadFile(filepath.Join(dir, ".env")); !strings.Contains(string(env), "Z2M_UI_BIND=192.168.1.5\n") ||
		!strings.Contains(out.String(), "http://192.168.1.5:8080") {
		t.Fatalf("--zigbee-ui not applied: %s", out.String())
	}
}

func TestInstallRefusals(t *testing.T) {
	ca, key := testCertPEM(t)
	dir := t.TempDir()
	base := InstallOptions{Code: testCode, Dir: dir, Image: "ghcr.io/panudet-24mb/aether-edge:0.1.0"}
	cases := []struct {
		name   string
		status int
		bundle map[string]any
		mutate func(*InstallOptions)
		want   string
	}{
		{"used code", 401, map[string]any{}, nil, "refused"},
		{"rate limited", 429, map[string]any{}, nil, "too many"},
		{"no CA yet", 503, map[string]any{}, nil, "not used"},
		{"key in the CA field", 201, testBundle(ca+key, false), nil, "usable broker certificate"},
		{"zigbee asked but not paired", 201, testBundle(ca, false), func(o *InstallOptions) { o.Zigbee = "192.168.1.40" }, "without a Zigbee2MQTT gateway"},
		{"bad code", 201, testBundle(ca, false), func(o *InstallOptions) { o.Code = "bad code!" }, "install code"},
		{"bad zigbee host", 201, testBundle(ca, true), func(o *InstallOptions) { o.Zigbee = "-rf" }, "--zigbee"},
		{"bad image", 201, testBundle(ca, false), func(o *InstallOptions) { o.Image = "Image With Spaces" }, "image"},
		{"page on every interface", 201, testBundle(ca, true), func(o *InstallOptions) { o.Zigbee, o.ZigbeeUI = "192.168.1.40", "0.0.0.0" }, "--zigbee-ui"},
		{"page without zigbee", 201, testBundle(ca, true), func(o *InstallOptions) { o.ZigbeeUI = "192.168.1.5" }, "--zigbee-ui"},
		{"system directory", 201, testBundle(ca, false), func(o *InstallOptions) { o.Dir = "/etc/aether" }, "installation directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := bootstrapServer(t, c.status, c.bundle, nil)
			o := base
			o.Server, o.Client = srv.URL, srv.Client()
			if c.mutate != nil {
				c.mutate(&o)
			}
			e := Install(context.Background(), o)
			if e == nil || !strings.Contains(e.Error(), c.want) {
				t.Fatalf("got %v, want an error about %q", e, c.want)
			}
			if strings.Contains(e.Error(), testPass) || strings.Contains(e.Error(), testToken) {
				t.Fatal("error leaks a secret")
			}
		})
	}
	if _, e := os.Stat(filepath.Join(dir, ".env")); e == nil {
		t.Fatal("a refused install wrote credentials")
	}
}

// The agent binary must not pull in server code: it runs on customer hardware and ships as a small image.
func TestAgentDependencies(t *testing.T) {
	if _, e := exec.LookPath("go"); e != nil {
		t.Fatal("go toolchain not on PATH")
	}
	out, e := exec.Command("go", "list", "-deps", "aether/backend/cmd/aether-edge").CombinedOutput()
	if e != nil {
		t.Fatalf("go list: %v\n%s", e, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case strings.HasPrefix(dep, "aether/backend/"):
			switch dep {
			case "aether/backend/cmd/aether-edge", "aether/backend/internal/edge", "aether/backend/internal/tuyalocal":
			default:
				t.Errorf("agent imports server package %s", dep)
			}
		case strings.HasPrefix(dep, "gorm.io/"), strings.HasPrefix(dep, "github.com/gofiber/"), strings.HasPrefix(dep, "github.com/jackc/"),
			strings.HasPrefix(dep, "github.com/golang-jwt/"):
			t.Errorf("agent imports server dependency %s", dep)
		}
	}
}

// A bundle whose api_origin would smuggle a second line into .env is replaced by the installer's own origin.
func TestInstallRefusesInjectedOrigin(t *testing.T) {
	ca, _ := testCertPEM(t)
	b := testBundle(ca, false)
	b["api_origin"] = "https://evil.example\nMQTT_URL=mqtt://evil:1883"
	srv := bootstrapServer(t, 201, b, nil)
	dir := filepath.Join(t.TempDir(), "edge")
	if e := Install(context.Background(), InstallOptions{Server: srv.URL, Code: testCode, Dir: dir, Image: "ghcr.io/panudet-24mb/aether-edge:0.1.0", Client: srv.Client()}); e != nil {
		t.Fatal(e)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if strings.Contains(string(env), "evil") || !strings.Contains(string(env), "AETHER_API="+srv.URL+"\n") {
		t.Fatalf(".env:\n%s", env)
	}
}

func TestEnsureFrontendAuth(t *testing.T) {
	ref := "  auth_token: '!secret auth_token'"
	cases := []struct{ name, in, want string }{
		{"adds to a mapping", "a: 1\nfrontend:\n  enabled: true\nb: 2\n", "a: 1\nfrontend:\n" + ref + "\n  enabled: true\nb: 2\n"},
		{"replaces a cleartext token", "frontend:\n    port: 8080\n    auth_token: hunter2\nx: 1\n", "frontend:\n    port: 8080\n    auth_token: '!secret auth_token'\nx: 1\n"},
		{"already set", "frontend:\n" + ref + "\n", "frontend:\n" + ref + "\n"},
		{"scalar true", "frontend: true\nadvanced:\n  network_key: [1]\n", "frontend:\n  enabled: true\n  port: 8080\n" + ref + "\nadvanced:\n  network_key: [1]\n"},
		{"page off", "frontend: false\n", "frontend: false\n"},
		{"no page", "mqtt:\n  a: 1\n", "mqtt:\n  a: 1\n"},
		{"nested frontend key untouched", "advanced:\n  frontend: x\n", "advanced:\n  frontend: x\n"},
	}
	for _, c := range cases {
		if got := EnsureFrontendAuth(c.in); got != c.want {
			t.Fatalf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestValidInstallDir(t *testing.T) {
	for _, ok := range []string{"/opt/aether-edge", "/out", "/srv/edge", "/usr/local/aether", "/home/pi/edge", "/var/lib/aether-edge"} {
		if !ValidInstallDir(ok) {
			t.Fatalf("refused %s", ok)
		}
	}
	for _, bad := range []string{"", "relative", "/", "/etc", "/etc/x", "/usr", "/usr/bin/x", "/bin", "/boot/x", "/proc/1", "/sys", "/dev/shm",
		"/var", "/root", "/home", "/home/", "/opt/../etc", "/run/x", "/lib"} {
		if ValidInstallDir(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
