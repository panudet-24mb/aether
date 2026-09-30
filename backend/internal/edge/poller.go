package edge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Device is one registered device the agent connects to. Key is the local key: kept in memory only, never logged.
type Device struct {
	ID         string `json:"id"`
	Key        string `json:"key"`
	Version    string `json:"version"`
	IP         string `json:"ip"`
	Device22   bool   `json:"dev22"`
	RefreshDPs []int  `json:"refresh_dps"`
	// A Tuya BLE device (Transport "ble"; empty is Wi-Fi), sent only to an agent that announced Bluetooth. SecKey
	// is a secret like Key.
	Transport string            `json:"transport,omitempty"`
	MAC       string            `json:"mac,omitempty"`
	UUID      string            `json:"uuid,omitempty"`
	SecKey    string            `json:"sec_key,omitempty"`
	ProductID string            `json:"product_id,omitempty"`
	Protocol  int               `json:"protocol,omitempty"`
	Mode      string            `json:"mode,omitempty"`
	Poll      int               `json:"poll,omitempty"`
	DPTypes   map[string]string `json:"dp_types,omitempty"`
	ReadOnly  bool              `json:"readonly,omitempty"`
}

// String, GoString and LogValue leave the keys out, so a Device can be printed or logged.
func (d Device) String() string {
	return fmt.Sprintf("edge.Device{ID:%s Transport:%q IP:%s Version:%s MAC:%s UUID:%s ReadOnly:%t}", d.ID, d.Transport, d.IP, d.Version, d.MAC, d.UUID, d.ReadOnly)
}

func (d Device) GoString() string { return d.String() }

func (d Device) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", d.ID), slog.String("transport", d.Transport), slog.String("ip", d.IP), slog.String("mac", d.MAC),
		slog.Bool("read_only", d.ReadOnly))
}

// ErrUnauthorized means the server refused the gateway's HTTP credential: the gateway was re-installed (its
// credentials rotated) or revoked. The agent stops every device connection when it sees it.
var ErrUnauthorized = errors.New("edge: gateway credential refused (install the agent again)")

// errNotModified is the 304 answer: the configuration did not change.
var errNotModified = errors.New("edge: configuration not modified")

// Poller pulls the agent's configuration over HTTPS with the gateway's Basic credential and the last ETag.
type Poller struct {
	url, gateway, token string
	ble                 bool
	client              *http.Client
	etag                string
}

// NewPoller builds the configuration client. The system roots are trusted, plus WEB_CA_FILE when Aether's web front
// uses a private CA.
func NewPoller(c Config) (*Poller, error) {
	roots, e := x509.SystemCertPool()
	if e != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if c.WebCAFile != "" {
		b, e := os.ReadFile(c.WebCAFile)
		if e != nil || !roots.AppendCertsFromPEM(b) {
			return nil, errors.New("cannot read WEB_CA_FILE")
		}
	}
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
	}
	return &Poller{
		url:     c.API + "/ingest/gateways/" + c.GatewayID + "/edge/config",
		gateway: c.GatewayID, token: c.HTTPToken, ble: c.BLEEnabled,
		client: &http.Client{Transport: transport, Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

// Fetch returns the configuration when it changed since the last successful fetch, errNotModified when it did not,
// ErrUnauthorized when the credential is refused.
func (p *Poller) Fetch(ctx context.Context) (int64, []Device, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if e != nil {
		return 0, nil, e
	}
	req.SetBasicAuth(p.gateway, p.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aether-edge/"+Version)
	if p.ble {
		// Only then does the server send BLE devices: an agent without Bluetooth would try them over TCP.
		req.Header.Set("X-Aether-Edge-Caps", "ble")
	}
	if p.etag != "" {
		req.Header.Set("If-None-Match", p.etag)
	}
	resp, e := p.client.Do(req)
	if e != nil {
		return 0, nil, errors.New("edge: configuration request failed")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return 0, nil, errNotModified
	case http.StatusUnauthorized, http.StatusForbidden:
		return 0, nil, ErrUnauthorized
	case http.StatusOK:
	default:
		return 0, nil, errors.New("edge: configuration request answered " + strconv.Itoa(resp.StatusCode))
	}
	var body struct {
		Revision int64    `json:"revision"`
		Devices  []Device `json:"devices"`
	}
	if e := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); e != nil {
		return 0, nil, errors.New("edge: configuration is not valid JSON")
	}
	devices := make([]Device, 0, len(body.Devices))
	seen := map[string]bool{}
	for _, d := range body.Devices {
		d.ID = strings.ToLower(strings.TrimSpace(d.ID))
		if !ValidDevice(d.ID) || len(d.Key) != 16 || seen[d.ID] || (d.Version != "" && !validVersion(d.Version)) {
			continue
		}
		switch d.Transport {
		case "":
		case "ble":
			if !p.ble || !validBLE(&d) {
				continue
			}
		default:
			continue // a transport this agent does not know
		}
		seen[d.ID] = true
		devices = append(devices, d)
		if len(devices) == MaxLAN {
			break
		}
	}
	p.etag = resp.Header.Get("ETag")
	return body.Revision, devices, nil
}

func validVersion(v string) bool {
	switch v {
	case "3.1", "3.2", "3.3", "3.4", "3.5":
		return true
	}
	return false
}

var (
	macPattern      = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	bleTokenPattern = regexp.MustCompile(`^[A-Za-z0-9]{0,64}$`)
	dpTypes         = map[string]bool{"bool": true, "value": true, "enum": true, "string": true, "bitmap": true, "raw": true}
)

// validBLE checks a BLE device's configuration: an address or a uuid to find it by (the uuid is also what the pair
// request carries), a 16-character sec_key when there is one, a known protocol and mode, and data-point types for
// ids 1..255.
func validBLE(d *Device) bool {
	d.MAC = strings.ToLower(strings.TrimSpace(d.MAC))
	if (d.MAC != "" && !macPattern.MatchString(d.MAC)) || !bleTokenPattern.MatchString(d.UUID) || !bleTokenPattern.MatchString(d.ProductID) || (d.MAC == "" && d.UUID == "") {
		return false
	}
	if d.SecKey != "" && len(d.SecKey) != 16 {
		return false
	}
	switch d.Protocol {
	case 0, 2, 3, 4:
	default:
		return false
	}
	switch d.Mode {
	case "", "auto", "on_demand", "persistent":
	default:
		return false
	}
	if d.Poll < 0 || d.Poll > 86400 || len(d.DPTypes) > MaxDPS {
		return false
	}
	for k, t := range d.DPTypes {
		id, e := strconv.Atoi(k)
		if e != nil || id < 1 || id > 255 || strconv.Itoa(id) != k || !dpTypes[t] {
			return false
		}
	}
	return true
}
