package tuyacloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Reference signature computed by tinytuya 1.20.0 for a property issue (testdata/gen_post_signature.py): POST with the
// body tinytuya sends, signed with the business token and no signed headers (tinytuya's content_type=""), which is
// how Aether signs: Tuya checks only the headers named in Signature-Headers, and Aether names none.
func TestPostSignMatchesTinytuya(t *testing.T) {
	const id, secret, ts, token = "testaccessid0000000", "testsecret000000000000000000000a", "1790000000123", "tok_fixed_000000000000000000000"
	body := []byte(`{"properties": "{\"switch_1\":true}"}`)
	if got := Sign("POST", "/v2.0/cloud/thing/bf1111111111111111aa01/shadow/properties/issue", body, id, secret, token, ts, ""); got != "77F115631A0D6E71DCA5967BC5C44C596C0D9F77C868D8ED816B01388D5B5CC6" {
		t.Fatalf("post sign: %s", got)
	}
}

func TestIssueAndReadProperties(t *testing.T) {
	f, c, done := newFake(t, func(path string, q url.Values) (int, string) {
		if code, body, ok := okToken(path); ok {
			return code, body
		}
		switch path {
		case "/v2.0/cloud/thing/bf1111111111111111aa01/shadow/properties/issue":
			return 200, `{"success":true,"result":true}`
		case "/v2.0/cloud/thing/bf1111111111111111aa01/shadow/properties":
			if q.Get("codes") != "switch_1,cur_power" {
				t.Errorf("codes %q", q.Get("codes"))
			}
			return 200, `{"success":true,"result":{"properties":[{"code":"switch_1","dp_id":1,"time":1790000000123,"value":true},{"code":"cur_power","dp_id":19,"time":1790000000123,"value":1234}]}}`
		case "/v2.0/cloud/thing/bf2222222222222222bb02/shadow/properties/issue":
			return 200, `{"success":false,"code":2001,"msg":"device is offline"}`
		case "/v2.0/cloud/thing/bf3333333333333333cc03/shadow/properties/issue":
			return 200, `{"success":false,"code":40000309,"msg":"request frequency too high"}`
		}
		return 404, `{}`
	})
	defer done()
	ctx := context.Background()
	if e := c.Authenticate(ctx); e != nil {
		t.Fatal(e)
	}
	if e := c.IssueProperties(ctx, "bf1111111111111111aa01", map[string]any{"switch_1": true}); e != nil {
		t.Fatal(e)
	}
	if f.method != http.MethodPost || f.contentType != "application/json" {
		t.Fatalf("method %s content-type %s", f.method, f.contentType)
	}
	// The body is {"properties": "<JSON object as a string>"}, as Tuya's Send Property expects.
	var outer map[string]string
	if json.Unmarshal(f.body, &outer) != nil || len(outer) != 1 {
		t.Fatalf("body %s", f.body)
	}
	var inner map[string]any
	if json.Unmarshal([]byte(outer["properties"]), &inner) != nil || inner["switch_1"] != true {
		t.Fatalf("properties %q", outer["properties"])
	}
	props, e := c.Properties(ctx, "bf1111111111111111aa01", "switch_1", "cur_power")
	if e != nil || len(props) != 2 || props[1].Code != "cur_power" || props[1].DPID != 19 || string(props[1].Value) != "1234" {
		t.Fatalf("properties %+v %v", props, e)
	}
	// Tuya's own failure codes are classified; its message text is never echoed.
	e = c.IssueProperties(ctx, "bf2222222222222222bb02", map[string]any{"switch_1": true})
	if !errors.Is(e, ErrResponse) || strings.Contains(e.Error(), "offline") {
		t.Fatalf("offline device: %v", e)
	}
	if e = c.IssueProperties(ctx, "bf3333333333333333cc03", map[string]any{"switch_1": false}); !errors.Is(e, ErrRateLimited) {
		t.Fatalf("rate limit: %v", e)
	}
	if f.badSigns != 0 {
		t.Fatalf("%d requests failed signature checks", f.badSigns)
	}
}

func TestPropertyInputsRefusedBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := NewForTest(srv.URL, "fakeaccessid0001", "fakesecret0000000000000000000001", srv.Client(), nil)
	ctx := context.Background()
	many := map[string]any{}
	for i := 0; i <= maxProperties; i++ {
		many["code_"+strings.Repeat("x", 3)+string(rune('a'+i%26))+strings.Repeat("y", i/26)] = true
	}
	for name, call := range map[string]func() error{
		"bad device":    func() error { return c.IssueProperties(ctx, "../../v1.0/token", map[string]any{"a": 1}) },
		"short device":  func() error { return c.IssueProperties(ctx, "abc", map[string]any{"a": 1}) },
		"no properties": func() error { return c.IssueProperties(ctx, "bf1111111111111111aa01", nil) },
		"bad code":      func() error { return c.IssueProperties(ctx, "bf1111111111111111aa01", map[string]any{"a/b": 1}) },
		"too many":      func() error { return c.IssueProperties(ctx, "bf1111111111111111aa01", many) },
		"huge value": func() error {
			return c.IssueProperties(ctx, "bf1111111111111111aa01", map[string]any{"a": strings.Repeat("x", maxIssue)})
		},
		"bad read code": func() error { _, e := c.Properties(ctx, "bf1111111111111111aa01", "x y"); return e },
		"bad read id":   func() error { _, e := c.Properties(ctx, "bf11#", "a"); return e },
	} {
		if call() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests sent for refused input", hits.Load())
	}
}

// A redirect is answered as the final response, never followed to another host.
func TestIssueRefusesRedirect(t *testing.T) {
	var elsewhere atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := NewForTest(srv.URL, "fakeaccessid0001", "fakesecret0000000000000000000001", srv.Client(), nil)
	if e := c.IssueProperties(context.Background(), "bf1111111111111111aa01", map[string]any{"switch_1": true}); e == nil {
		t.Fatal("redirected issue succeeded")
	}
	if elsewhere.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestTokenExpiryAndRefresh(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	f := &fakeTuya{t: t, id: "fakeaccessid0001", key: "fakesecret0000000000000000000001", token: "faketoken-1"}
	var refreshedWithToken atomic.Bool
	f.respond = func(path string, q url.Values) (int, string) {
		switch path {
		case "/v1.0/token":
			return 200, `{"success":true,"result":{"access_token":"faketoken-1","refresh_token":"fakerefresh-1","expire_time":7200,"uid":"az_fake_uid"}}`
		case "/v1.0/token/fakerefresh-1":
			f.token = "faketoken-2"
			return 200, `{"success":true,"result":{"access_token":"faketoken-2","refresh_token":"fakerefresh-2","expire_time":3600,"uid":"az_fake_uid"}}`
		}
		return 404, `{}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1.0/token/") && r.Header.Get("access_token") != "" {
			refreshedWithToken.Store(true)
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := NewForTest(srv.URL, f.id, f.key, srv.Client(), func() time.Time { return now })
	if !c.ExpiresAt().IsZero() {
		t.Fatal("expiry before authenticating")
	}
	if e := c.RefreshToken(context.Background()); !errors.Is(e, ErrAuth) {
		t.Fatalf("refresh without a token: %v", e)
	}
	if e := c.Authenticate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !c.ExpiresAt().Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("expiry %v", c.ExpiresAt())
	}
	if e := c.RefreshToken(context.Background()); e != nil {
		t.Fatal(e)
	}
	if c.token != "faketoken-2" || !c.ExpiresAt().Equal(now.Add(time.Hour)) || refreshedWithToken.Load() {
		t.Fatalf("after refresh: token %q expiry %v signed-with-token %v", c.token, c.ExpiresAt(), refreshedWithToken.Load())
	}
	if f.badSigns != 0 {
		t.Fatal("refresh signature rejected")
	}
}

func TestDeviceSubFields(t *testing.T) {
	var d Device
	if e := json.Unmarshal([]byte(`{"id":"bf1111111111111111aa01","sub":true,"gateway_id":"bf9999999999999999zz09","node_id":"a4c1380000000001"}`), &d); e != nil {
		t.Fatal(e)
	}
	if !d.Sub || d.GatewayID != "bf9999999999999999zz09" || d.NodeID != "a4c1380000000001" {
		t.Fatalf("%+v", d)
	}
}
