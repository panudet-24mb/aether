package mqttingest

import (
	"aether/backend/internal/domain"
	"context"
	"errors"
	"testing"
)

type fake struct {
	authErr         error
	tenant, gateway string
	called          bool
}

func (f *fake) Gateway(_ context.Context, id, token string) (string, error) {
	return "trusted-tenant", f.authErr
}
func (f *fake) Capture(_ context.Context, tenant, gateway string, _ []byte) (string, error) {
	f.called = true
	f.tenant = tenant
	f.gateway = gateway
	return "stored", nil
}
func TestRoutingAndRevocation(t *testing.T) {
	b := []Binding{{Topic: "/mg3/test/status", GatewayID: "gw", Token: "secret"}}
	f := &fake{}
	_, e := Capture(context.Background(), f, b, "/mg3/other/status", []byte(`{}`))
	if !errors.Is(e, domain.ErrForbidden) || f.called {
		t.Fatal("unknown topic accepted")
	}
	_, e = Capture(context.Background(), f, b, b[0].Topic, []byte(`{"tenant_id":"attacker","gateway_id":"other"}`))
	if e != nil || f.tenant != "trusted-tenant" || f.gateway != "gw" {
		t.Fatal("payload influenced routing")
	}
	f = &fake{authErr: domain.ErrUnauthorized}
	_, e = Capture(context.Background(), f, b, b[0].Topic, []byte(`{}`))
	if !Permanent(e) || f.called {
		t.Fatal("revoked credential accepted")
	}
	if Permanent(errors.New("database down")) {
		t.Fatal("transient failure must retry")
	}
}
func TestConfigFailsClosed(t *testing.T) {
	c := Config{BrokerURL: "ssl://mqtt:8883", Username: "worker", Password: "12345678901234567890123456789012", ClientID: "worker", CAFile: "ca.crt", Bindings: []Binding{{Topic: "/mg3/test/status", GatewayID: "9ea91aa5-1cf5-4f46-aab4-8066db1cf79a", Token: "1234567890123456789012345678901234567890123"}}}
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, url := range []string{"tcp://mqtt:1883", "ssl://user:password@mqtt:8883", "ssl://mqtt:8883/path"} {
		n := c
		n.BrokerURL = url
		if n.Validate() == nil {
			t.Fatal("unsafe URL accepted")
		}
	}
	n := c
	n.Bindings = append([]Binding{}, c.Bindings...)
	n.Bindings[0].Topic = "/mg3/+/status"
	if n.Validate() == nil {
		t.Fatal("wildcard accepted")
	}
	n = c
	n.Bindings = append(n.Bindings, n.Bindings[0])
	if n.Validate() == nil {
		t.Fatal("duplicate binding accepted")
	}
}
