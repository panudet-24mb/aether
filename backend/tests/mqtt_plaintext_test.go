package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"testing"
)

// Production keeps TLS as the advertised default and offers plaintext only when the operator opts in.
func TestMQTTPlaintextIsAnExplicitProductionOptIn(t *testing.T) {
	f := setup(t)
	_, auth, _ := f.account(t)
	t.Setenv("MQTT_PUBLIC_HOST", "aether.example.local")
	t.Setenv("MQTT_PUBLIC_PORT", "8883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "ssl")
	cfg := f.cfg
	cfg.Environment = "production"
	api := httpapi.New(cfg, f.service, f.repo)
	token := "Bearer " + auth.AccessToken

	t.Setenv("MQTT_ALLOW_PLAINTEXT", "")
	code, out, _ := req(t, api, "GET", "/api/v1/mqtt/setup", token, "", "", nil)
	if code != 200 || out["scheme"] != "ssl" || out["plaintext"] != nil {
		t.Fatalf("without the opt-in production must offer TLS only: %d %v", code, out)
	}

	t.Setenv("MQTT_ALLOW_PLAINTEXT", "true")
	t.Setenv("MQTT_PLAIN_PORT", "1883")
	code, out, _ = req(t, api, "GET", "/api/v1/mqtt/setup", token, "", "", nil)
	plain, _ := out["plaintext"].(map[string]any)
	if code != 200 || out["scheme"] != "ssl" || plain["port"] != float64(1883) || plain["scheme"] != "tcp" {
		t.Fatalf("with the opt-in both TLS and plaintext must be offered: %d %v", code, out)
	}

	// The plaintext port can never be the TLS port.
	t.Setenv("MQTT_PLAIN_PORT", "8883")
	if code, _, _ = req(t, api, "GET", "/api/v1/mqtt/setup", token, "", "", nil); code != 503 {
		t.Fatalf("a plaintext port equal to the TLS port must be refused: %d", code)
	}
	// Advertising tcp as the primary scheme stays refused in production.
	t.Setenv("MQTT_PLAIN_PORT", "1883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "tcp")
	if code, _, _ = req(t, api, "GET", "/api/v1/mqtt/setup", token, "", "", nil); code != 503 {
		t.Fatalf("tcp as the primary scheme must stay refused in production: %d", code)
	}
}
