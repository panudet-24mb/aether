package fakecloud

import (
	"aether/backend/internal/adapters/tuyacloud"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Issue is one property issue the fake OpenAPI received.
type Issue struct {
	Device     string
	Properties map[string]json.RawMessage
}

// CloudDevice is one device the fake project lists, with its v1.1 specification document (the "result"). LocalKey,
// UUID and SecKey are listed like Tuya lists a device's (SecKeyCamel spells it secKey, as some listings do); MAC is
// its factory record's address (factory-infos), as Tuya writes it. Sub marks a device behind a Tuya hub.
type CloudDevice struct {
	ID, Name, Category, ProductID string
	Spec                          json.RawMessage
	Properties                    []tuyacloud.Property
	LocalKey, UUID, SecKey, MAC   string
	SecKeyCamel, Sub              bool
}

// API is a fake Tuya OpenAPI for one project.
type API struct {
	Server   *httptest.Server
	AccessID string

	mu      sync.Mutex
	devices []CloudDevice
	issues  []Issue
	calls   map[string]int
	fail    map[string]int // device id -> Tuya error code for issues
	onIssue func(Issue)
	refuse  bool // token requests are refused (wrong secret)
}

// NewAPI starts the fake OpenAPI; it stops with the test.
func NewAPI(t testing.TB, accessID string) *API {
	a := &API{AccessID: accessID, calls: map[string]int{}, fail: map[string]int{}}
	a.Server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.Server.Close)
	return a
}

// Client replaces tuyacloud.New: a client of this fake server.
func (a *API) Client(region, accessID, secret string) (*tuyacloud.Client, error) {
	if _, e := tuyacloud.New(region, accessID, secret); e != nil {
		return nil, e
	}
	return tuyacloud.NewForTest(a.Server.URL, accessID, secret, a.Server.Client(), nil), nil
}

func (a *API) SetDevices(d ...CloudDevice) {
	a.mu.Lock()
	a.devices = d
	a.mu.Unlock()
}

// Fail makes issues to device answer with Tuya error code (0 succeeds again).
func (a *API) Fail(device string, code int) {
	a.mu.Lock()
	a.fail[device] = code
	a.mu.Unlock()
}

// RefuseToken makes token requests fail as for a wrong Access Secret (Tuya code 1004).
func (a *API) RefuseToken(on bool) {
	a.mu.Lock()
	a.refuse = on
	a.mu.Unlock()
}

// OnIssue runs after each successful issue (a test answers with a status report).
func (a *API) OnIssue(fn func(Issue)) {
	a.mu.Lock()
	a.onIssue = fn
	a.mu.Unlock()
}

func (a *API) Issues() []Issue {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Issue(nil), a.issues...)
}

// Calls counts requests per route ("token", "devices", "factory", "specifications", "model", "properties", "issue").
func (a *API) Calls(route string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[route]
}

func reply(w http.ResponseWriter, result any) {
	b, _ := json.Marshal(map[string]any{"success": true, "result": result, "t": time.Now().UnixMilli()})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func refuse(w http.ResponseWriter, code int, msg string) {
	b, _ := json.Marshal(map[string]any{"success": false, "code": code, "msg": msg, "t": time.Now().UnixMilli()})
	_, _ = w.Write(b)
}

func (a *API) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("client_id") != a.AccessID || r.Header.Get("sign") == "" {
		refuse(w, 1004, "sign invalid")
		return
	}
	path := r.URL.Path
	parts := strings.Split(strings.Trim(path, "/"), "/")
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case path == "/v1.0/token" && a.refuse:
		a.calls["token"]++
		refuse(w, 1004, "sign invalid")
	case path == "/v1.0/token":
		a.calls["token"]++
		reply(w, map[string]any{"access_token": "fake-token", "refresh_token": "fake-refresh", "expire_time": 7200, "uid": "fake-uid"})
	case strings.HasPrefix(path, "/v1.0/token/"):
		a.calls["token"]++
		reply(w, map[string]any{"access_token": "fake-token-2", "refresh_token": "fake-refresh-2", "expire_time": 7200, "uid": "fake-uid"})
	case r.Header.Get("access_token") == "":
		refuse(w, 1010, "token invalid")
	case path == "/v1.0/iot-01/associated-users/devices":
		a.calls["devices"]++
		list := []map[string]any{}
		for _, d := range a.devices {
			item := map[string]any{"id": d.ID, "name": d.Name, "category": d.Category, "product_id": d.ProductID, "online": true}
			if d.UUID != "" {
				item["uuid"] = d.UUID
			}
			if d.LocalKey != "" {
				item["local_key"] = d.LocalKey
			}
			if d.SecKey != "" && d.SecKeyCamel {
				item["secKey"] = d.SecKey
			} else if d.SecKey != "" {
				item["sec_key"] = d.SecKey
			}
			if d.Sub {
				item["sub"] = true
			}
			list = append(list, item)
		}
		reply(w, map[string]any{"devices": list, "has_more": false})
	case path == "/v1.0/iot-03/devices/factory-infos":
		a.calls["factory"]++
		ids := strings.Split(r.URL.Query().Get("device_ids"), ",")
		if len(ids) > tuyacloud.FactoryBatch {
			refuse(w, 1109, "param is illegal")
			return
		}
		list := []map[string]any{}
		for _, id := range ids {
			for _, d := range a.devices {
				if d.ID == id && d.MAC != "" {
					list = append(list, map[string]any{"id": d.ID, "uuid": d.UUID, "mac": d.MAC, "sn": "fake-sn"})
				}
			}
		}
		reply(w, list)
	case len(parts) == 5 && parts[0] == "v2.0" && parts[4] == "model":
		a.calls["model"]++
		refuse(w, 1106, "permission deny") // the project may read only v1.1 specifications
	case len(parts) == 4 && parts[0] == "v1.1" && parts[3] == "specifications":
		a.calls["specifications"]++
		for _, d := range a.devices {
			if d.ID == parts[2] {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(d.Spec)
				return
			}
		}
		refuse(w, 2009, "device not found")
	case len(parts) == 6 && parts[4] == "shadow" && parts[5] == "properties" && r.Method == http.MethodGet:
		a.calls["properties"]++
		for _, d := range a.devices {
			if d.ID == parts[3] {
				props := d.Properties
				if props == nil {
					props = []tuyacloud.Property{}
				}
				reply(w, map[string]any{"properties": props})
				return
			}
		}
		refuse(w, 2009, "device not found")
	case len(parts) == 7 && parts[6] == "issue" && r.Method == http.MethodPost:
		a.calls["issue"]++
		if code := a.fail[parts[3]]; code != 0 {
			if code == http.StatusTooManyRequests {
				w.WriteHeader(code)
				return
			}
			refuse(w, code, "refused")
			return
		}
		var body struct {
			Properties string `json:"properties"`
		}
		props := map[string]json.RawMessage{}
		if json.NewDecoder(r.Body).Decode(&body) != nil || json.Unmarshal([]byte(body.Properties), &props) != nil {
			refuse(w, 1109, "param is illegal")
			return
		}
		issue := Issue{Device: parts[3], Properties: props}
		a.issues = append(a.issues, issue)
		reply(w, true)
		if fn := a.onIssue; fn != nil {
			go fn(issue)
		}
	default:
		refuse(w, 1108, "uri path invalid")
	}
}
