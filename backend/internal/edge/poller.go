package edge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
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
}

// ErrUnauthorized means the server refused the gateway's HTTP credential: the gateway was re-installed (its
// credentials rotated) or revoked. The agent stops every device connection when it sees it.
var ErrUnauthorized = errors.New("edge: gateway credential refused (install the agent again)")

// errNotModified is the 304 answer: the configuration did not change.
var errNotModified = errors.New("edge: configuration not modified")

// Poller pulls the agent's configuration over HTTPS with the gateway's Basic credential and the last ETag.
type Poller struct {
	url, gateway, token string
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
		gateway: c.GatewayID, token: c.HTTPToken,
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
