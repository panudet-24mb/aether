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
	for _, tc := range []struct{ key, value string }{{"JWT_SIGNING_KEY", "short"}, {"APP_ORIGIN", "http://aether.example"}, {"APP_ORIGIN", "https://aether.example/path"}, {"DATABASE_URL", "postgresql://aether_app:unused@db/aether?sslmode=disable"}, {"ALLOW_REGISTRATION", "true"}, {"DEPLOYMENT_MODE", "anything"}, {"APP_ENV", "prod"}, {"AUTOMATION_COMMANDS", "yes"}, {"TUYA_CLOUD", "yes"}, {"TUYA_CLOUD", "1"}, {"TUYA_CLOUD_PUBLIC_KEY", "c2hvcnQ="}, {"TUYA_CLOUD_MAX_LINKS_PER_TENANT", "0"}} {
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

// Device commands from automations are opt-in: absent means off, only "true" turns them on.
func TestAutomationCommandsOptIn(t *testing.T) {
	base(t)
	if c, e := Load(); e != nil || c.AutomationCommands {
		t.Fatal("AUTOMATION_COMMANDS must default to off", e)
	}
	t.Setenv("AUTOMATION_COMMANDS", "true")
	if c, e := Load(); e != nil || !c.AutomationCommands {
		t.Fatal("AUTOMATION_COMMANDS=true", e)
	}
}

func TestTuyaCloudFlag(t *testing.T) {
	base(t)
	c, e := Load()
	if e != nil || c.TuyaCloud || c.TuyaCloudPublicKey != nil || c.TuyaCloudLinksPerTenant != 2 {
		t.Fatalf("defaults: %v %v %v %d", e, c.TuyaCloud, c.TuyaCloudPublicKey, c.TuyaCloudLinksPerTenant)
	}
	t.Setenv("TUYA_CLOUD", "true")
	t.Setenv("TUYA_CLOUD_PUBLIC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("TUYA_CLOUD_MAX_LINKS_PER_TENANT", "5")
	if c, e = Load(); e != nil || !c.TuyaCloud || c.TuyaCloudPublicKey == nil || c.TuyaCloudLinksPerTenant != 5 {
		t.Fatalf("enabled: %v %+v", e, c)
	}
}

// OPS_STATUS_FILE is optional (off in development) and must be a plain absolute path when set.
func TestOpsStatusFile(t *testing.T) {
	base(t)
	if c, e := Load(); e != nil || c.OpsStatusFile != "" {
		t.Fatal("OPS_STATUS_FILE must default to off", e)
	}
	t.Setenv("OPS_STATUS_FILE", "/run/aether-ops/status.json")
	if c, e := Load(); e != nil || c.OpsStatusFile != "/run/aether-ops/status.json" {
		t.Fatal("OPS_STATUS_FILE", e)
	}
	for _, bad := range []string{"status.json", "/run/../etc/passwd", "/run/aether-ops/"} {
		t.Setenv("OPS_STATUS_FILE", bad)
		if _, e := Load(); e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestOpsStatusTenant(t *testing.T) {
	base(t)
	if c, e := Load(); e != nil || c.OpsStatusTenant != "" {
		t.Fatal("OPS_STATUS_TENANT must default to every owner", e)
	}
	t.Setenv("OPS_STATUS_TENANT", "3F2504E0-4F89-11D3-9A0C-0305E82C3301")
	if c, e := Load(); e != nil || c.OpsStatusTenant != "3f2504e0-4f89-11d3-9a0c-0305e82c3301" {
		t.Fatal("OPS_STATUS_TENANT", c.OpsStatusTenant, e)
	}
	for _, bad := range []string{"zenture", "00000000-0000-0000-0000-000000000000", "3f2504e0"} {
		t.Setenv("OPS_STATUS_TENANT", bad)
		if _, e := Load(); e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
