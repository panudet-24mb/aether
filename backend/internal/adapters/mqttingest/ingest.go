// Package mqttingest binds broker-authorized exact topics to server-owned gateway
// credentials. Payload fields never select a tenant or gateway.
package mqttingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"aether/backend/internal/domain"
	"aether/backend/internal/security"
)

type Binding struct {
	Topic     string `json:"topic"`
	GatewayID string `json:"gateway_id"`
	Token     string `json:"token"`
}
type Config struct {
	BrokerURL string    `json:"broker_url"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ClientID  string    `json:"client_id"`
	CAFile    string    `json:"ca_file"`
	Bindings  []Binding `json:"bindings"`
}

func Load(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, fmt.Errorf("cannot read MQTT config")
	}
	if len(b) > 65536 {
		return c, fmt.Errorf("MQTT config too large")
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, fmt.Errorf("invalid MQTT config")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	u, e := url.Parse(c.BrokerURL)
	if e != nil || u.Scheme != "ssl" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Path != "" || u.Fragment != "" || c.Username == "" || len(c.Password) < 32 || c.ClientID == "" || c.CAFile == "" {
		return fmt.Errorf("MQTT requires TLS, credentials, CA, client ID and bindings")
	}
	seen := map[string]bool{}
	for _, b := range c.Bindings {
		if b.Topic == "" || strings.ContainsAny(b.Topic, "+#\x00") || seen[b.Topic] || !security.ValidID(b.GatewayID) || len(b.Token) != 43 {
			return fmt.Errorf("invalid or duplicate MQTT binding")
		}
		seen[b.Topic] = true
	}
	return nil
}

type Service interface {
	Gateway(context.Context, string, string) (string, error)
	Capture(context.Context, string, string, []byte) (string, error)
}

func Capture(ctx context.Context, s Service, bindings []Binding, topic string, payload []byte) (string, error) {
	for _, b := range bindings {
		if b.Topic != topic {
			continue
		}
		tenant, e := s.Gateway(ctx, b.GatewayID, b.Token)
		if e != nil {
			return "", e
		}
		return s.Capture(ctx, tenant, b.GatewayID, payload)
	}
	return "", domain.ErrForbidden
}
func Permanent(e error) bool {
	return errors.Is(e, domain.ErrInvalid) || errors.Is(e, domain.ErrUnauthorized) || errors.Is(e, domain.ErrForbidden)
}
