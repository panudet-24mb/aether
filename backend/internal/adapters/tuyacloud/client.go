// Package tuyacloud is the one-time import side of Aether Edge: it asks the Tuya IoT Platform (OpenAPI) for the
// devices of the app account linked to the user's own cloud project, with their local keys, and for each
// product's data-point model. It is used for the duration of one import and never at runtime: afterwards the
// devices are reached on the LAN by the agent (docs/platform/tuya-local.md).
//
// Hosts come only from the region enum below, never from input, so a request can go nowhere but Tuya. The
// credentials live in the Client for the length of one import and are never logged: errors carry Tuya's code
// and a category, not the request.
//
// Signing follows Tuya's "new signature" algorithm (the same as tinytuya's Cloud._tuyaplatform, MIT):
//
//	stringToSign = METHOD "\n" sha256hex(body) "\n" signed-headers "\n" path[?sorted query]
//	sign = upper(hex(HMAC-SHA256(secret, client_id [+ access_token] + t + nonce + stringToSign)))
package tuyacloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Regions maps each data center the import offers to its OpenAPI host
// (https://developer.tuya.com/en/docs/iot/api-request?id=Ka4a8uuo1j4t4). Existing Thai Smart Life accounts are
// mostly on "us" (Western America); newer ones may be on "sg".
var Regions = map[string]string{
	"us":   "https://openapi.tuyaus.com",
	"us-e": "https://openapi-ueaz.tuyaus.com",
	"eu":   "https://openapi.tuyaeu.com",
	"eu-w": "https://openapi-weaz.tuyaeu.com",
	"cn":   "https://openapi.tuyacn.com",
	"in":   "https://openapi.tuyain.com",
	"sg":   "https://openapi-sg.iotbing.com",
}

// RegionOrder is the order the import suggests when a region returned no devices.
var RegionOrder = []string{"us", "sg", "eu", "in", "us-e", "eu-w", "cn"}

// Limits of one import.
const (
	CallTimeout  = 10 * time.Second
	MaxDevices   = 200
	maxBody      = 2 << 20
	pageSize     = 50
	maxPages     = 8
	credentialRe = `^[A-Za-z0-9]{8,64}$`
)

var credentialPattern = regexp.MustCompile(credentialRe)

// Error categories an import reports to the user. Tuya's numeric code is kept for support.
var (
	ErrAuth          = errors.New("tuya: access id or secret rejected")
	ErrNotSubscribed = errors.New("tuya: the cloud project is not subscribed to the required API service")
	ErrPermission    = errors.New("tuya: permission denied")
	ErrRateLimited   = errors.New("tuya: request quota or rate limit reached")
	ErrUnavailable   = errors.New("tuya: cloud unreachable")
	ErrResponse      = errors.New("tuya: unexpected response")
)

// APIError is a failed Tuya call: its category (one of the errors above) and Tuya's own code.
type APIError struct {
	Kind error
	Code int
}

func (e *APIError) Error() string         { return fmt.Sprintf("%v (code %d)", e.Kind, e.Code) }
func (e *APIError) Unwrap() error         { return e.Kind }
func apiError(kind error, code int) error { return &APIError{Kind: kind, Code: code} }

// classify maps Tuya's error codes (https://developer.tuya.com/en/docs/iot/error-code?id=K989ruxx88swc) to a
// category. Unknown codes are ErrResponse.
func classify(code int, msg string) error {
	switch code {
	case 1004, 1010, 1011, 1012, 1013, 2009, 2017:
		return apiError(ErrAuth, code)
	case 28841101, 28841105, 28841002, 1114:
		return apiError(ErrNotSubscribed, code)
	case 1106, 2008, 2406:
		return apiError(ErrPermission, code)
	case 1100, 40000309, 28841004:
		return apiError(ErrRateLimited, code)
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "frequen"), strings.Contains(lower, "quota"), strings.Contains(lower, "limit"):
		return apiError(ErrRateLimited, code)
	case strings.Contains(lower, "not subscribed"), strings.Contains(lower, "no permission"):
		return apiError(ErrNotSubscribed, code)
	case strings.Contains(lower, "sign"), strings.Contains(lower, "token"), strings.Contains(lower, "client"):
		return apiError(ErrAuth, code)
	}
	return apiError(ErrResponse, code)
}

// Client talks to one region for one import.
type Client struct {
	base   string
	id     string
	secret string
	token  string
	uid    string
	http   *http.Client
	now    func() time.Time
}

// New validates the region and the credentials' shape.
func New(region, accessID, accessSecret string) (*Client, error) {
	base, ok := Regions[region]
	if !ok || !credentialPattern.MatchString(accessID) || !credentialPattern.MatchString(accessSecret) {
		return nil, errors.New("tuya: invalid region or credentials")
	}
	return &Client{base: base, id: accessID, secret: accessSecret, http: noRedirects(&http.Client{Timeout: CallTimeout}), now: time.Now}, nil
}

// noRedirects keeps every request on the region's host: a redirect is answered as the final response (and then
// fails to parse) rather than followed somewhere else.
func noRedirects(h *http.Client) *http.Client {
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return h
}

// NewForTest points a client at a fake server (tests only; production code has no way to choose a host).
func NewForTest(base, accessID, accessSecret string, h *http.Client, now func() time.Time) *Client {
	if h == nil {
		h = &http.Client{Timeout: CallTimeout}
	}
	if now == nil {
		now = time.Now
	}
	return &Client{base: strings.TrimRight(base, "/"), id: accessID, secret: accessSecret, http: noRedirects(h), now: now}
}

// Sign returns Tuya's request signature. token is empty for the token call; nonce may be empty.
func Sign(method, pathAndQuery string, body []byte, clientID, secret, token, t, nonce string) string {
	sum := sha256.Sum256(body)
	stringToSign := method + "\n" + hex.EncodeToString(sum[:]) + "\n" + "" + "\n" + pathAndQuery
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(clientID + token + t + nonce + stringToSign))
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
}

// signedPath is path?k=v&... with the keys sorted and the values unescaped, as Tuya signs it.
func signedPath(path string, query url.Values) string {
	if len(query) == 0 {
		return path
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+query.Get(k))
	}
	return path + "?" + strings.Join(parts, "&")
}

type envelope struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Result  json.RawMessage `json:"result"`
}

// get performs one signed GET and returns the result document.
func (c *Client) get(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	t := strconv.FormatInt(c.now().UnixMilli(), 10)
	sign := Sign(http.MethodGet, signedPath(path, query), nil, c.id, c.secret, c.token, t, "")
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if e != nil {
		return nil, apiError(ErrResponse, 0)
	}
	req.Header.Set("client_id", c.id)
	req.Header.Set("sign", sign)
	req.Header.Set("t", t)
	req.Header.Set("sign_method", "HMAC-SHA256")
	if c.token != "" {
		req.Header.Set("access_token", c.token)
	}
	res, e := c.http.Do(req)
	if e != nil {
		return nil, apiError(ErrUnavailable, 0)
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if e != nil || len(raw) > maxBody {
		return nil, apiError(ErrResponse, 0)
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, apiError(ErrRateLimited, res.StatusCode)
	}
	if res.StatusCode >= 500 {
		return nil, apiError(ErrUnavailable, res.StatusCode)
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil {
		return nil, apiError(ErrResponse, res.StatusCode)
	}
	if !env.Success {
		return nil, classify(env.Code, env.Msg)
	}
	return env.Result, nil
}

// Authenticate gets an access token (GET /v1.0/token?grant_type=1). It must be the first call.
func (c *Client) Authenticate(ctx context.Context) error {
	c.token = ""
	raw, e := c.get(ctx, "/v1.0/token", url.Values{"grant_type": {"1"}})
	if e != nil {
		return e
	}
	var r struct {
		AccessToken string `json:"access_token"`
		UID         string `json:"uid"`
	}
	if json.Unmarshal(raw, &r) != nil || r.AccessToken == "" || len(r.AccessToken) > 512 {
		return apiError(ErrResponse, 0)
	}
	c.token, c.uid = r.AccessToken, r.UID
	return nil
}

// Device is one device of the linked app account, as the import needs it. LocalKey is the secret the agent
// uses on the LAN; the caller seals it at once.
type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	LocalKey  string `json:"local_key"`
	Category  string `json:"category"`
	ProductID string `json:"product_id"`
	Sub       bool   `json:"sub"`
	Online    bool   `json:"online"`
}

// Devices lists the linked account's devices, at most MaxDevices. The associated-users endpoint is Tuya's current
// one for a project's linked app accounts; when the project is not allowed to use it, the older listings by the
// token's uid are tried in turn (the same fallbacks tinytuya uses).
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	out, e := c.paged(ctx, "/v1.0/iot-01/associated-users/devices", url.Values{"size": {strconv.Itoa(pageSize)}}, "last_row_key")
	if e == nil || !fallback(e) || c.uid == "" {
		return out, e
	}
	out, e2 := c.paged(ctx, "/v1.3/iot-03/devices", url.Values{"source_type": {"tuyaUser"}, "source_id": {c.uid}, "page_size": {strconv.Itoa(pageSize)}}, "last_id")
	if e2 == nil || !fallback(e2) {
		return out, e2
	}
	raw, e3 := c.get(ctx, "/v1.0/users/"+url.PathEscape(c.uid)+"/devices", nil)
	if e3 != nil {
		return nil, e // the first endpoint's error explains the most
	}
	var list []Device
	if json.Unmarshal(raw, &list) != nil {
		return nil, apiError(ErrResponse, 0)
	}
	return capped(list), nil
}

func fallback(e error) bool {
	return errors.Is(e, ErrPermission) || errors.Is(e, ErrNotSubscribed)
}

func capped(list []Device) []Device {
	if len(list) > MaxDevices {
		return list[:MaxDevices]
	}
	return list
}

// paged follows a cursor-paged device listing. It accepts both result shapes Tuya uses: {devices, has_more,
// last_row_key} and {list, has_more, last_id}.
func (c *Client) paged(ctx context.Context, path string, base url.Values, cursorParam string) ([]Device, error) {
	out := []Device{}
	cursor := ""
	for page := 0; page < maxPages && len(out) < MaxDevices; page++ {
		q := url.Values{}
		for k, v := range base {
			q[k] = v
		}
		if cursor != "" {
			q.Set(cursorParam, cursor)
		}
		raw, e := c.get(ctx, path, q)
		if e != nil {
			return nil, e
		}
		var r struct {
			Devices    []Device `json:"devices"`
			List       []Device `json:"list"`
			HasMore    bool     `json:"has_more"`
			LastRowKey string   `json:"last_row_key"`
			LastID     string   `json:"last_id"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return nil, apiError(ErrResponse, 0)
		}
		out = append(out, r.Devices...)
		out = append(out, r.List...)
		next := r.LastRowKey
		if next == "" {
			next = r.LastID
		}
		if !r.HasMore || next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return capped(out), nil
}

// Model returns a device's data-point model document: the v2.0 thing model, or, when the project may not read
// it, the v1.1 specifications. Which one it is comes back as the second value ("model" | "specifications"), so
// the caller parses it with tuya.ParseModel or tuya.ParseSpecifications.
func (c *Client) Model(ctx context.Context, deviceID string) (json.RawMessage, string, error) {
	path := url.PathEscape(deviceID)
	raw, e := c.get(ctx, "/v2.0/cloud/thing/"+path+"/model", nil)
	if e == nil {
		return raw, "model", nil
	}
	if !fallback(e) && !errors.Is(e, ErrResponse) {
		return nil, "", e
	}
	raw, e2 := c.get(ctx, "/v1.1/devices/"+path+"/specifications", nil)
	if e2 != nil {
		return nil, "", e
	}
	return raw, "specifications", nil
}
