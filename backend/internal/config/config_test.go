package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func base(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgresql://aether_app:unused@db/aether?sslmode=verify-full")
	t.Setenv("APP_ORIGIN", "https://aether.example")
	t.Setenv("APP_ENV", "production")
	t.Setenv("DEPLOYMENT_MODE", "onprem")
	t.Setenv("ALLOW_REGISTRATION", "false")
	t.Setenv("JWT_SIGNING_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 48))))
}
func TestDeploymentModesSameConfiguration(t *testing.T) {
	for _, mode := range []string{"onprem", "cloud"} {
		t.Run(mode, func(t *testing.T) {
			base(t)
			t.Setenv("DEPLOYMENT_MODE", mode)
			c, e := Load()
			if e != nil || !c.SecureCookies || c.Registration || c.Mode != mode {
				t.Fatal(c.Mode, e)
			}
		})
	}
}
func TestFailClosed(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"JWT_SIGNING_KEY", "short"}, {"APP_ORIGIN", "http://aether.example"}, {"APP_ORIGIN", "https://aether.example/path"}, {"DATABASE_URL", "postgresql://aether_app:unused@db/aether?sslmode=disable"}, {"ALLOW_REGISTRATION", "true"}, {"DEPLOYMENT_MODE", "anything"}, {"APP_ENV", "prod"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			base(t)
			t.Setenv(tc.key, tc.value)
			if _, e := Load(); e == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}
func TestLocalDevelopmentExplicit(t *testing.T) {
	base(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("APP_ORIGIN", "http://localhost:3000")
	t.Setenv("DATABASE_URL", "postgresql://aether_app:unused@localhost/aether?sslmode=disable")
	c, e := Load()
	if e != nil || c.SecureCookies {
		t.Fatal(e)
	}
}

func TestProductionCloudRegistrationGated(t *testing.T) {
	base(t)
	t.Setenv("DEPLOYMENT_MODE", "cloud")
	t.Setenv("ALLOW_REGISTRATION", "true")
	if _, e := Load(); e == nil {
		t.Fatal("unverified public registration enabled in production")
	}
}
