package edge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Zigbee2MQTTImage is the Zigbee2MQTT release the installer pins when the server does not name one (it passes
// --z2m-image from domain.Zigbee2MQTTImage) (2.x: EmberZNet coordinators such as the SMLIGHT
// SLZB-06M/06MU use `adapter: ember`, https://www.zigbee2mqtt.io/guide/adapters/emberznet.html).
const Zigbee2MQTTImage = "ghcr.io/koenkk/zigbee2mqtt:2.14.1"

//go:embed compose.tmpl.yaml
var composeTemplate string

// InstallOptions are the installer's inputs. Code is the one-time install code (passed in the environment by
// install.sh, never on a command line); Zigbee is the SLZB coordinator's LAN address when Zigbee2MQTT is wanted.
type InstallOptions struct {
	Server    string // Aether's public origin
	Code      string
	Dir       string
	Zigbee    string
	ZigbeeUI  string // IPv4 address of this host to publish Zigbee2MQTT's page on; empty = loopback only
	Image     string // aether-edge image reference the compose file pins
	Z2MImage  string // Zigbee2MQTT image reference; Zigbee2MQTTImage when empty
	WebCAFile string // optional: a private root for Aether's web front, to reach /edge/bootstrap
	// BLE turns on the Tuya BLE transport: BLE_ENABLED=true in .env and the host's D-Bus socket mounted read-only
	// (bluetoothd is reached over D-Bus; the container keeps no capability). install.sh checks the host first.
	BLE    bool
	Client *http.Client
	// Out receives what the installer must tell the operator once (the Zigbee2MQTT page token). Nil = discard.
	Out io.Writer
}

// bundle is the bootstrap answer (POST /edge/bootstrap), shown once.
type bundle struct {
	GatewayID string `json:"gateway_id"`
	APIOrigin string `json:"api_origin"`
	HTTP      struct {
		Token string `json:"token"`
	} `json:"http"`
	MQTT struct {
		URL      string `json:"url"`
		TLS      bool   `json:"tls"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"mqtt"`
	CAPEM    string `json:"ca_pem"`
	WebCAPEM string `json:"web_ca_pem"`
	Zigbee   *struct {
		GatewayID string `json:"gateway_id"`
		BaseTopic string `json:"base_topic"`
		Server    string `json:"server"`
		Username  string `json:"username"`
		Password  string `json:"password"`
		TLS       bool   `json:"tls"`
	} `json:"zigbee"`
}

var (
	codePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	hostPattern  = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	imagePattern = regexp.MustCompile(`^[a-z0-9./_-]+(:[A-Za-z0-9._-]+)?(@sha256:[0-9a-f]{64})?$`)
	tokenLine    = regexp.MustCompile(`^auth_token:\s*'([A-Za-z0-9_-]{22,128})'\s*$`)
)

// ValidUIAddress reports an address to publish Zigbee2MQTT's page on: one IPv4 interface of this host, never the
// wildcard (a Docker-published port bypasses the host firewall).
func ValidUIAddress(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && !ip.IsUnspecified() && !ip.IsMulticast()
}

// ValidInstallDir refuses directories an installer must never own: system trees, whole home roots, relative paths
// and anything with "..".
func ValidInstallDir(dir string) bool {
	if !strings.HasPrefix(dir, "/") || strings.Contains(dir, "..") || strings.ContainsAny(dir, "\n\r") {
		return false
	}
	clean := filepath.Clean(dir)
	switch clean {
	case "/", "/var", "/root", "/home", "/opt", "/srv", "/tmp", "/usr/local":
		return false
	}
	for _, sys := range []string{"/etc", "/usr", "/bin", "/sbin", "/boot", "/proc", "/sys", "/dev", "/lib", "/lib64", "/run"} {
		if clean == sys || strings.HasPrefix(clean, sys+"/") {
			if clean != "/usr/local" && !strings.HasPrefix(clean, "/usr/local/") {
				return false
			}
		}
	}
	return true
}

// validOrigin is an http(s) origin without path, query or control characters.
func validOrigin(s string) bool {
	if strings.ContainsAny(s, "\n\r\t '\"") {
		return false
	}
	u, e := url.Parse(s)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Path == "" && u.RawQuery == "" && u.User == nil && u.Fragment == ""
}

// ValidZigbeeHost reports an SLZB coordinator address the installer accepts (an IPv4 address or a host name).
func ValidZigbeeHost(s string) bool {
	if ip := net.ParseIP(s); ip != nil {
		return ip.To4() != nil
	}
	return hostPattern.MatchString(s) && !strings.HasPrefix(s, "-")
}

// Install redeems the code and writes the agent's files into Dir: .env (0600), ca.crt, compose.yaml (0600) and,
// with Zigbee, zigbee2mqtt/. An existing zigbee2mqtt/configuration.yaml is never overwritten (Zigbee2MQTT keeps the
// network key in it; losing it means pairing every device again): credentials live in zigbee2mqtt/secret.yaml,
// which is rewritten on every install.
func Install(ctx context.Context, o InstallOptions) error {
	origin, e := url.Parse(strings.TrimRight(o.Server, "/"))
	if e != nil || (origin.Scheme != "https" && origin.Scheme != "http") || origin.Host == "" || origin.Path != "" {
		return errors.New("the Aether address must be an origin such as https://aether.example.com")
	}
	if !codePattern.MatchString(o.Code) {
		return errors.New("the install code is missing or malformed")
	}
	if o.Zigbee != "" && !ValidZigbeeHost(o.Zigbee) {
		return errors.New("--zigbee must be the coordinator's IPv4 address or host name")
	}
	if o.ZigbeeUI != "" && (o.Zigbee == "" || !ValidUIAddress(o.ZigbeeUI)) {
		return errors.New("--zigbee-ui must be one IPv4 address of this host (and needs --zigbee)")
	}
	if o.Z2MImage == "" {
		o.Z2MImage = Zigbee2MQTTImage
	}
	if !imagePattern.MatchString(o.Image) || !imagePattern.MatchString(o.Z2MImage) {
		return errors.New("the image reference is malformed")
	}
	if !ValidInstallDir(o.Dir) {
		return errors.New("the installation directory must be an absolute path outside system directories")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	client := o.Client
	if client == nil {
		if client, e = installClient(o.WebCAFile); e != nil {
			return e
		}
	}
	b, e := redeem(ctx, client, origin.String(), o.Code)
	if e != nil {
		return e
	}
	if o.Zigbee != "" && b.Zigbee == nil {
		return errors.New("--zigbee was given, but the install code was created without a Zigbee2MQTT gateway: create a new code and choose the Zigbee2MQTT gateway")
	}
	return writeFiles(o, b)
}

func installClient(webCA string) (*http.Client, error) {
	roots, e := x509.SystemCertPool()
	if e != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if webCA != "" {
		pemBytes, e := os.ReadFile(webCA)
		if e != nil || !roots.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("cannot read the web CA file")
		}
	}
	return &http.Client{Timeout: 30 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func redeem(ctx context.Context, client *http.Client, origin, code string) (bundle, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/edge/bootstrap", bytes.NewReader(body))
	if e != nil {
		return bundle{}, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "aether-edge-installer/"+Version)
	resp, e := client.Do(req)
	if e != nil {
		return bundle{}, fmt.Errorf("cannot reach %s: %w", origin, e)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
	case http.StatusUnauthorized:
		return bundle{}, errors.New("the install code was refused: it is wrong, already used or expired (codes last 30 minutes and work once)")
	case http.StatusTooManyRequests:
		return bundle{}, errors.New("too many attempts from this address; wait a minute and try again")
	case http.StatusServiceUnavailable:
		return bundle{}, errors.New("the server cannot hand out its broker certificate yet (see docs/production.md); the code was not used")
	default:
		return bundle{}, errors.New("the server answered " + strconv.Itoa(resp.StatusCode))
	}
	var b bundle
	if e := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&b); e != nil {
		return bundle{}, errors.New("the server's answer is not valid JSON")
	}
	if !uuidPattern.MatchString(b.GatewayID) || !tokenPattern.MatchString(b.HTTP.Token) || b.MQTT.Password == "" {
		return bundle{}, errors.New("the server's answer is incomplete")
	}
	if _, e := brokerURL(b.MQTT.URL); e != nil {
		return bundle{}, errors.New("the server's broker address is malformed")
	}
	if b.MQTT.TLS && !onlyCertificates(b.CAPEM) {
		return bundle{}, errors.New("the server did not send a usable broker certificate")
	}
	if b.WebCAPEM != "" && !onlyCertificates(b.WebCAPEM) {
		b.WebCAPEM = ""
	}
	if b.Zigbee != nil && (!uuidPattern.MatchString(b.Zigbee.GatewayID) || b.Zigbee.Password == "") {
		return bundle{}, errors.New("the server's Zigbee2MQTT credentials are incomplete")
	}
	// The origin written to .env is the one the installer was fetched from unless the server names another valid
	// one: a malformed value must never smuggle a second line into .env.
	if !validOrigin(b.APIOrigin) {
		b.APIOrigin = origin
	}
	return b, nil
}

// onlyCertificates is true for PEM text made of CERTIFICATE blocks only (never a key).
func onlyCertificates(s string) bool {
	rest, n := []byte(s), 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return false
		}
		if _, e := x509.ParseCertificate(block.Bytes); e != nil {
			return false
		}
		n++
	}
	return n > 0 && len(bytes.TrimSpace(rest)) == 0
}

func writeFiles(o InstallOptions, b bundle) error {
	if e := os.MkdirAll(o.Dir, 0o700); e != nil {
		return e
	}
	if e := os.Chmod(o.Dir, 0o700); e != nil {
		return e
	}
	env := []string{
		"# Aether Edge · written by the installer. Holds the gateway's credentials: keep it private (0600).",
		"EDGE_IMAGE=" + o.Image,
		"Z2M_IMAGE=" + o.Z2MImage,
		"AETHER_API=" + b.APIOrigin,
		"GATEWAY_ID=" + b.GatewayID,
		"HTTP_TOKEN=" + b.HTTP.Token,
		"MQTT_URL=" + b.MQTT.URL,
		"MQTT_USERNAME=gw-" + b.GatewayID,
		"MQTT_PASSWORD=" + envValue(b.MQTT.Password),
	}
	if b.CAPEM != "" {
		env = append(env, "MQTT_CA_FILE=/config/ca.crt")
		if e := writeFile(filepath.Join(o.Dir, "ca.crt"), b.CAPEM, 0o644); e != nil {
			return e
		}
	}
	if o.BLE {
		env = append(env, "BLE_ENABLED=true", "BLE_ADAPTER=hci0", "BLE_MAX_CONNECTIONS=1",
			"DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket")
	}
	if b.WebCAPEM != "" {
		env = append(env, "WEB_CA_FILE=/config/web-ca.crt")
		if e := writeFile(filepath.Join(o.Dir, "web-ca.crt"), b.WebCAPEM, 0o644); e != nil {
			return e
		}
	}
	if o.Zigbee != "" {
		bind := "127.0.0.1"
		if o.ZigbeeUI != "" {
			bind = o.ZigbeeUI
		}
		env = append(env, "COMPOSE_PROFILES=zigbee", "Z2M_UI_BIND="+bind)
		token, e := writeZigbee(o, b)
		if e != nil {
			return e
		}
		where := "ssh -L 8080:127.0.0.1:8080 <user>@<this host>, then open http://127.0.0.1:8080"
		if o.ZigbeeUI != "" {
			where = "http://" + o.ZigbeeUI + ":8080"
		}
		fmt.Fprintf(o.Out, "Zigbee2MQTT page: %s\nZigbee2MQTT page token (keep it private; also in zigbee2mqtt/secret.yaml): %s\n", where, token)
	}
	if e := writeFile(filepath.Join(o.Dir, ".env"), strings.Join(env, "\n")+"\n", 0o600); e != nil {
		return e
	}
	var mounts []string
	if b.CAPEM != "" {
		mounts = append(mounts, "      - ./ca.crt:/config/ca.crt:ro")
	}
	if b.WebCAPEM != "" {
		mounts = append(mounts, "      - ./web-ca.crt:/config/web-ca.crt:ro")
	}
	if o.BLE {
		// bluetoothd is reached over the host's system bus; read-only, and nothing else of /run.
		mounts = append(mounts, "      - /run/dbus:/run/dbus:ro")
	}
	volumes := ""
	if len(mounts) > 0 {
		volumes = "    volumes:\n" + strings.Join(mounts, "\n") + "\n"
	}
	compose := strings.NewReplacer("__VOLUMES__\n", volumes).Replace(composeTemplate)
	return writeFile(filepath.Join(o.Dir, "compose.yaml"), compose, 0o600)
}

// writeZigbee writes Zigbee2MQTT's files and returns its page token. The token (at least 128 random bits) is kept
// across installs: an existing secret.yaml's token is reused so the operator's saved token keeps working.
func writeZigbee(o InstallOptions, b bundle) (string, error) {
	dir := filepath.Join(o.Dir, "zigbee2mqtt")
	if e := os.MkdirAll(dir, 0o700); e != nil {
		return "", e
	}
	z := b.Zigbee
	token := existingToken(filepath.Join(dir, "secret.yaml"))
	if token == "" {
		raw := make([]byte, 24)
		if _, e := rand.Read(raw); e != nil {
			return "", e
		}
		token = base64.RawURLEncoding.EncodeToString(raw)
	}
	secret := strings.Join([]string{
		"# Aether · Zigbee2MQTT secrets, rewritten by every install (the page token is kept).",
		"user: " + yamlQuote(z.Username),
		"password: " + yamlQuote(z.Password),
		"auth_token: " + yamlQuote(token),
	}, "\n") + "\n"
	if e := writeFile(filepath.Join(dir, "secret.yaml"), secret, 0o600); e != nil {
		return "", e
	}
	if z.TLS {
		if e := writeFile(filepath.Join(dir, "aether-ca.crt"), b.CAPEM, 0o644); e != nil {
			return "", e
		}
	}
	cfgPath := filepath.Join(dir, "configuration.yaml")
	if existing, e := os.ReadFile(cfgPath); e == nil {
		// Zigbee2MQTT owns this file now (it holds the network key): only make sure its page asks for the token.
		updated := EnsureFrontendAuth(string(existing))
		if updated == string(existing) {
			return token, nil
		}
		return token, writeFile(cfgPath, updated, 0o600)
	}
	lines := []string{
		"# Aether · Zigbee2MQTT 2.x, written once by the Aether Edge installer.",
		"mqtt:",
		"  server: " + yamlQuote(z.Server),
		"  base_topic: " + z.BaseTopic,
		"  user: '!secret user'",
		"  password: '!secret password'",
		"  client_id: " + yamlQuote(z.Username),
	}
	if z.TLS {
		lines = append(lines, "  ca: /app/data/aether-ca.crt", "  reject_unauthorized: true")
	}
	lines = append(lines,
		"  keepalive: 60",
		"  version: 4",
		"  include_device_information: true",
		"serial:",
		"  port: tcp://"+o.Zigbee+":6638",
		"  adapter: ember",
		"homeassistant:",
		"  enabled: false",
		"availability:",
		"  enabled: true",
		"health:",
		"  interval: 10",
		"frontend:",
		"  enabled: true",
		"  port: 8080",
		"  auth_token: '!secret auth_token'",
		"advanced:",
		"  network_key: GENERATE",
		"  pan_id: GENERATE",
		"  ext_pan_id: GENERATE",
	)
	return token, writeFile(cfgPath, strings.Join(lines, "\n")+"\n", 0o600)
}

func existingToken(path string) string {
	f, e := os.Open(path)
	if e != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := tokenLine.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			return m[1]
		}
	}
	return ""
}

const authRef = "auth_token: '!secret auth_token'"

// EnsureFrontendAuth makes a Zigbee2MQTT configuration's page require the token from secret.yaml, touching only the
// top-level frontend block (every other line, the network key included, is kept byte for byte):
//   - an existing frontend mapping gets its auth_token line replaced, or one added as its first child;
//   - a scalar `frontend: true` becomes a mapping with enabled, port and the token;
//   - `frontend: false` (page off) is left alone;
//   - no frontend key at all: nothing is served, nothing to protect.
func EnsureFrontendAuth(cfg string) string {
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != "frontend" {
			continue
		}
		value = strings.TrimSpace(value)
		if hash := strings.Index(value, "#"); hash >= 0 {
			value = strings.TrimSpace(value[:hash])
		}
		switch value {
		case "false", "False", "no":
			return cfg
		case "true", "True", "yes":
			block := []string{"frontend:", "  enabled: true", "  port: 8080", "  " + authRef}
			return strings.Join(append(append(append([]string{}, lines[:i]...), block...), lines[i+1:]...), "\n")
		case "":
		default:
			return cfg // a flow mapping written by hand: leave it, the installer's message says to add the token
		}
		indent := "  "
		for j := i + 1; j < len(lines); j++ {
			child := lines[j]
			if strings.TrimSpace(child) == "" || strings.HasPrefix(strings.TrimSpace(child), "#") {
				continue
			}
			trimmed := strings.TrimLeft(child, " ")
			if len(trimmed) == len(child) {
				break // back at the top level: end of the frontend block
			}
			indent = child[:len(child)-len(trimmed)]
			if strings.HasPrefix(trimmed, "auth_token:") {
				if trimmed == authRef {
					return cfg
				}
				lines[j] = indent + authRef
				return strings.Join(lines, "\n")
			}
		}
		out := append(append(append([]string{}, lines[:i+1]...), indent+authRef), lines[i+1:]...)
		return strings.Join(out, "\n")
	}
	return cfg
}

func writeFile(path, content string, mode os.FileMode) error {
	tmp := path + ".tmp"
	if e := os.WriteFile(tmp, []byte(content), mode); e != nil {
		return e
	}
	if e := os.Chmod(tmp, mode); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}

// envValue keeps a value on one line for a compose .env file.
func envValue(s string) string { return strings.NewReplacer("\n", "", "\r", "").Replace(s) }

func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
