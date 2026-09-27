package edge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPollerETagAndCredential(t *testing.T) {
	var auth, ifNone string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ingest/gateways/"+testGateway+"/edge/config" {
			http.NotFound(w, r)
			return
		}
		u, p, _ := r.BasicAuth()
		auth, ifNone = u+":"+p, r.Header.Get("If-None-Match")
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		if ifNone == `"7"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"7"`)
		w.Write([]byte(`{"revision":7,"devices":[
			{"id":"` + testDevice + `","key":"` + testKey + `","version":"3.4","ip":"192.168.1.9","dev22":false,"refresh_dps":[18,19]},
			{"id":"BAD","key":"` + testKey + `","version":"3.4"},
			{"id":"bf00000000000000000a","key":"short","version":"3.4"},
			{"id":"bf00000000000000000b","key":"` + testKey + `","version":"9.9"},
			{"id":"` + testDevice + `","key":"` + testKey + `","version":"3.4"}]}`))
	}))
	defer srv.Close()
	c := testConfig(t)
	c.API = srv.URL
	p, e := NewPoller(c)
	if e != nil {
		t.Fatal(e)
	}
	p.client = srv.Client()
	rev, devices, e := p.Fetch(context.Background())
	if e != nil || rev != 7 || len(devices) != 1 || devices[0].Key != testKey || len(devices[0].RefreshDPs) != 2 {
		t.Fatalf("fetch: %d %+v %v", rev, devices, e)
	}
	if auth != testGateway+":"+testToken || ifNone != "" {
		t.Fatalf("auth %q if-none-match %q", auth, ifNone)
	}
	if _, _, e := p.Fetch(context.Background()); !errors.Is(e, errNotModified) || ifNone != `"7"` {
		t.Fatalf("second fetch: %v (If-None-Match %q)", e, ifNone)
	}
	status = 401
	if _, _, e := p.Fetch(context.Background()); !errors.Is(e, ErrUnauthorized) {
		t.Fatalf("refused credential: %v", e)
	}
}
