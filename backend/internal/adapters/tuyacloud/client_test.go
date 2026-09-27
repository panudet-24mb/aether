package tuyacloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Reference signatures computed by tinytuya 1.20.0 (Cloud._tuyaplatform, new signing algorithm, no nonce) with a
// frozen clock, fake credentials and a captured request (testdata/gen_signatures.py): the token call, then a
// business call whose query tinytuya signs with its keys sorted.
func TestSignMatchesTinytuya(t *testing.T) {
	const id, secret, ts = "testaccessid0000000", "testsecret000000000000000000000a", "1790000000123"
	if got := Sign("GET", "/v1.0/token?grant_type=1", nil, id, secret, "", ts, ""); got != "EA8D4537C486E6F61A43AF03C88F08978562F838A923AE15FD049C5541087427" {
		t.Fatalf("token sign: %s", got)
	}
	q := url.Values{"size": {"50"}, "last_row_key": {"abc"}}
	if got := Sign("GET", signedPath("/v1.0/iot-01/associated-users/devices", q), nil, id, secret, "tok_fixed_000000000000000000000", ts, ""); got != "14960E3657780C1A0715E8FB6AB3D85A91C1E69037D0D80C51C8215E123EB94E" {
		t.Fatalf("business sign: %s", got)
	}
}

func TestNewRejectsUnknownRegionAndBadCredentials(t *testing.T) {
	for _, c := range []struct{ region, id, secret string }{
		{"mars", "abcdefgh1234", "abcdefgh1234abcdefgh1234abcdefgh"},
		{"us", "short", "abcdefgh1234abcdefgh1234abcdefgh"},
		{"us", "abcdefgh1234", "has space in it abcdefgh"},
		{"https://evil.example", "abcdefgh1234", "abcdefgh1234abcdefgh1234abcdefgh"},
	} {
		if _, e := New(c.region, c.id, c.secret); e == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	if _, e := New("sg", "abcdefgh1234", "abcdefgh1234abcdefgh1234abcdefgh"); e != nil {
		t.Fatal(e)
	}
}

// fakeTuya replays scrubbed fixtures and checks every request's signature the way Tuya would.
type fakeTuya struct {
	t        *testing.T
	id, key  string
	token    string
	mu       sync.Mutex
	paths    []string
	respond  func(path string, q url.Values) (int, string)
	now      func() time.Time
	badSigns int
}

func (f *fakeTuya) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	token := r.Header.Get("access_token")
	want := Sign(r.Method, signedPath(r.URL.Path, r.URL.Query()), nil, f.id, f.key, token, r.Header.Get("t"), "")
	if r.Header.Get("client_id") != f.id || r.Header.Get("sign") != want || r.Header.Get("sign_method") != "HMAC-SHA256" {
		f.badSigns++
		w.Write([]byte(`{"success":false,"code":1004,"msg":"sign invalid"}`))
		return
	}
	if r.URL.Path != "/v1.0/token" && token != f.token {
		w.Write([]byte(`{"success":false,"code":1010,"msg":"token invalid"}`))
		return
	}
	code, body := f.respond(r.URL.Path, r.URL.Query())
	w.WriteHeader(code)
	w.Write([]byte(body))
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, e := os.ReadFile("testdata/" + name)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

func newFake(t *testing.T, respond func(string, url.Values) (int, string)) (*fakeTuya, *Client, func()) {
	f := &fakeTuya{t: t, id: "fakeaccessid0001", key: "fakesecret0000000000000000000001", token: "faketoken-1", respond: respond}
	srv := httptest.NewServer(f)
	c := NewForTest(srv.URL, f.id, f.key, srv.Client(), nil)
	return f, c, srv.Close
}

func okToken(path string) (int, string, bool) {
	if path == "/v1.0/token" {
		return 200, `{"success":true,"result":{"access_token":"faketoken-1","expire_time":7200,"uid":"az_fake_uid"}}`, true
	}
	return 0, "", false
}

func TestDevicesPaginatesAndModelsParse(t *testing.T) {
	f, c, done := newFake(t, func(path string, q url.Values) (int, string) {
		if code, body, ok := okToken(path); ok {
			return code, body
		}
		switch path {
		case "/v1.0/iot-01/associated-users/devices":
			if q.Get("last_row_key") == "" {
				return 200, fixture(t, "devices_page1.json")
			}
			if q.Get("last_row_key") != "row-2" {
				t.Errorf("cursor %q", q.Get("last_row_key"))
			}
			return 200, fixture(t, "devices_page2.json")
		case "/v2.0/cloud/thing/bf00000000000000sw01/model":
			return 200, fixture(t, "model_light.json")
		case "/v2.0/cloud/thing/bf00000000000000pl01/model":
			return 200, `{"success":false,"code":28841101,"msg":"No permissions. This API is not subscribed."}`
		case "/v1.1/devices/bf00000000000000pl01/specifications":
			return 200, fixture(t, "specifications_plug.json")
		}
		return 404, `{}`
	})
	defer done()
	ctx := context.Background()
	if e := c.Authenticate(ctx); e != nil {
		t.Fatal(e)
	}
	devices, e := c.Devices(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if len(devices) != 3 || devices[0].ID != "bf00000000000000sw01" || devices[2].Sub != true || devices[0].LocalKey == "" {
		t.Fatalf("devices: %+v", devices)
	}
	raw, kind, e := c.Model(ctx, "bf00000000000000sw01")
	if e != nil || kind != "model" || !json.Valid(raw) {
		t.Fatalf("model: %s %v", kind, e)
	}
	raw, kind, e = c.Model(ctx, "bf00000000000000pl01")
	if e != nil || kind != "specifications" || !strings.Contains(string(raw), "cur_power") {
		t.Fatalf("fallback: %s %v", kind, e)
	}
	if f.badSigns != 0 {
		t.Fatalf("%d requests failed signature verification", f.badSigns)
	}
}

func TestDevicesFallsBackToOlderListings(t *testing.T) {
	_, c, done := newFake(t, func(path string, q url.Values) (int, string) {
		if code, body, ok := okToken(path); ok {
			return code, body
		}
		switch path {
		case "/v1.0/iot-01/associated-users/devices", "/v1.3/iot-03/devices":
			return 200, `{"success":false,"code":1106,"msg":"permission deny"}`
		case "/v1.0/users/az_fake_uid/devices":
			return 200, `{"success":true,"result":[{"id":"bf00000000000000aa01","name":"old api","local_key":"0123456789abcdef","category":"cz"}]}`
		}
		return 404, `{}`
	})
	defer done()
	ctx := context.Background()
	if e := c.Authenticate(ctx); e != nil {
		t.Fatal(e)
	}
	devices, e := c.Devices(ctx)
	if e != nil || len(devices) != 1 || devices[0].Category != "cz" {
		t.Fatalf("%+v %v", devices, e)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{200, `{"success":false,"code":1004,"msg":"sign invalid"}`, ErrAuth},
		{200, `{"success":false,"code":2009,"msg":"clientId is invalid"}`, ErrAuth},
		{200, `{"success":false,"code":28841101,"msg":"No permissions. This API is not subscribed."}`, ErrNotSubscribed},
		{200, `{"success":false,"code":40000309,"msg":"request too frequently"}`, ErrRateLimited},
		{429, `{}`, ErrRateLimited},
		{503, `{}`, ErrUnavailable},
		{200, `not json`, ErrResponse},
	}
	for _, c := range cases {
		_, cl, done := newFake(t, func(path string, q url.Values) (int, string) { return c.status, c.body })
		e := cl.Authenticate(context.Background())
		done()
		if !errors.Is(e, c.want) {
			t.Fatalf("%d %s: got %v, want %v", c.status, c.body, e, c.want)
		}
		if strings.Contains(e.Error(), "fakesecret") {
			t.Fatal("secret leaked into an error")
		}
	}
	// A cloud that does not answer at all.
	c := NewForTest("http://127.0.0.1:1", "fakeaccessid0001", "fakesecret0000000000000000000001", nil, nil)
	if e := c.Authenticate(context.Background()); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}

func TestDevicesCappedAndPagesBounded(t *testing.T) {
	calls := 0
	_, c, done := newFake(t, func(path string, q url.Values) (int, string) {
		if code, body, ok := okToken(path); ok {
			return code, body
		}
		calls++
		var sb strings.Builder
		sb.WriteString(`{"success":true,"result":{"has_more":true,"last_row_key":"row-` + q.Get("last_row_key") + `x","devices":[`)
		for i := 0; i < 50; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`{"id":"bf0000000000000000x` + string(rune('a'+i%26)) + `","local_key":"0123456789abcdef"}`)
		}
		sb.WriteString(`]}}`)
		return 200, sb.String()
	})
	defer done()
	ctx := context.Background()
	if e := c.Authenticate(ctx); e != nil {
		t.Fatal(e)
	}
	devices, e := c.Devices(ctx)
	if e != nil || len(devices) != MaxDevices || calls > maxPages {
		t.Fatalf("devices %d calls %d %v", len(devices), calls, e)
	}
}

// A redirect is never followed: the request stays on the region's host.
func TestRedirectNotFollowed(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed a redirect to another host")
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1.0/token", http.StatusFound)
	}))
	defer srv.Close()
	c := NewForTest(srv.URL, "fakeaccessid0001", "fakesecret0000000000000000000001", srv.Client(), nil)
	if e := c.Authenticate(context.Background()); !errors.Is(e, ErrResponse) {
		t.Fatalf("redirect: %v", e)
	}
}
