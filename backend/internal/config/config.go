package config

import (
	"aether/backend/internal/security"
	"encoding/base64"
	"errors"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL, Listen, Mode, Environment, Origin, Issuer string
	// AuthDatabaseURL is the login pool (role aether_auth, migration 00040): the only connection that can read a
	// password hash. The API refuses to start without it; ingest and workers never read it.
	AuthDatabaseURL string
	JWTKey          []byte
	Registration    bool
	SecureCookies   bool
	// Optional outbound e-mail for alert notifications; unset means the email channel is unavailable.
	SMTPHost, SMTPUsername, SMTPPassword, SMTPFrom string
	SMTPPort                                       int
	// Requests per minute per client IP on /api (default 120). Several users behind one NAT, or one user with
	// several live pages open, share this budget, so deployments may raise it.
	APIRateLimit int
	// Storage and rollout controls (see docs/production.md).
	SampleRetentionDays  int // decoded samples are kept this long, dropped by weekly partition: up to 7 days more (default 90)
	SampleMinIntervalSec int // store at most one environment sample per stream per interval (0 = every uplink)
	BLEHistoryHours      int // raw BLE advertisement archive for Studio decoders, dropped by daily partition: up to 24 h more (default 24)
	// PrivacyNoticeURL is the deployment's own privacy notice; empty means the built-in Thai template at /privacy.
	PrivacyNoticeURL   string
	DiscoveryLimit     int  // streams per gateway for tags that are NOT registered devices (default 100)
	AlertsShadow       bool // record events but open no alerts, send nothing and run no automations; SOS (button) and hazard still alert
	AutomationCommands bool // automations may command devices (action.command); off by default, see docs/platform/automation.md
	TuyaCloud          bool // Tuya Cloud mode (TUYA_CLOUD): the tuya-cloud gateway model and profile exist; off by default
	// TuyaCloudPublicKey is the tuya-cloud worker's X25519 public key (TUYA_CLOUD_PUBLIC_KEY): the API seals project
	// credentials to it and can never open them. Nil means linking is unavailable (tuya_cloud_unconfigured).
	TuyaCloudPublicKey *[32]byte
	// TuyaCloudLinksPerTenant caps the Tuya Cloud projects one workspace may link (TUYA_CLOUD_MAX_LINKS_PER_TENANT, default 2).
	TuyaCloudLinksPerTenant int
	SealKey                 []byte   // optional CHANNEL_SEAL_KEY; falls back to a key derived from JWT_SIGNING_KEY
	TrustedProxies          []string // CIDRs/IPs of the reverse proxy; only then is X-Forwarded-For believed
	WebhookAllowedHosts     []string // host:port targets exempt from the private-address block (on-prem relays)

	// TuyaCloudEventBudget and TuyaCloudAPIBudget are the monthly Tuya allowances the gateway page shows usage against
	// (TUYA_CLOUD_EVENT_BUDGET / TUYA_CLOUD_API_BUDGET, the same variables the tuya-cloud worker enforces; 0 = no guard).
	TuyaCloudEventBudget, TuyaCloudAPIBudget int
}

func Load() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), Listen: os.Getenv("LISTEN_ADDR"), Mode: os.Getenv("DEPLOYMENT_MODE"), Environment: os.Getenv("APP_ENV"), Origin: os.Getenv("APP_ORIGIN"), Issuer: "aether", SecureCookies: true, AuthDatabaseURL: os.Getenv("AUTH_DATABASE_URL")}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Mode == "" {
		c.Mode = "onprem"
	}
	if c.Environment == "" {
		c.Environment = "production"
	}
	if c.Mode != "cloud" && c.Mode != "onprem" {
		return c, errors.New("DEPLOYMENT_MODE must be cloud or onprem")
	}
	if c.Environment != "production" && c.Environment != "development" && c.Environment != "test" {
		return c, errors.New("invalid APP_ENV")
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL required")
	}
	u, e := url.Parse(c.Origin)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return c, errors.New("APP_ORIGIN must be an exact origin without path")
	}
	if u.Scheme != "https" {
		if c.Environment == "production" || u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
			return c, errors.New("HTTPS required except explicit localhost development")
		}
		c.SecureCookies = false
	}
	c.JWTKey, e = base64.StdEncoding.DecodeString(os.Getenv("JWT_SIGNING_KEY"))
	if e != nil || len(c.JWTKey) < 32 {
		return c, errors.New("JWT_SIGNING_KEY must contain at least 32 base64-encoded random bytes")
	}
	reg := os.Getenv("ALLOW_REGISTRATION")
	if reg != "" && reg != "true" && reg != "false" {
		return c, errors.New("ALLOW_REGISTRATION must be true or false")
	}
	c.Registration = reg == "true"
	intEnv := func(name string, def, lo, hi int) (int, error) {
		raw := os.Getenv(name)
		if raw == "" {
			return def, nil
		}
		n, e := strconv.Atoi(raw)
		if e != nil || n < lo || n > hi {
			return 0, errors.New(name + " out of range")
		}
		return n, nil
	}
	if c.SampleRetentionDays, e = intEnv("SAMPLE_RETENTION_DAYS", 90, 1, 3650); e != nil {
		return c, e
	}
	if c.SampleMinIntervalSec, e = intEnv("SAMPLE_MIN_INTERVAL_SEC", 0, 0, 3600); e != nil {
		return c, e
	}
	if c.BLEHistoryHours, e = intEnv("BLE_HISTORY_HOURS", 24, 1, 720); e != nil {
		return c, e
	}
	if raw := strings.TrimSpace(os.Getenv("PRIVACY_NOTICE_URL")); raw != "" {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > 512 {
			return c, errors.New("PRIVACY_NOTICE_URL must be an https:// URL")
		}
		c.PrivacyNoticeURL = u.String()
	}
	if c.DiscoveryLimit, e = intEnv("DISCOVERY_LIMIT", 100, 0, 5000); e != nil {
		return c, e
	}
	switch os.Getenv("ALERTS_SHADOW") {
	case "", "false":
	case "true":
		c.AlertsShadow = true
	default:
		return c, errors.New("ALERTS_SHADOW must be true or false")
	}
	switch os.Getenv("AUTOMATION_COMMANDS") {
	case "", "false":
	case "true":
		c.AutomationCommands = true
	default:
		return c, errors.New("AUTOMATION_COMMANDS must be true or false")
	}
	switch os.Getenv("TUYA_CLOUD") {
	case "", "false":
	case "true":
		c.TuyaCloud = true
	default:
		return c, errors.New("TUYA_CLOUD must be true or false")
	}
	if raw := os.Getenv("TUYA_CLOUD_PUBLIC_KEY"); raw != "" {
		if c.TuyaCloudPublicKey, e = security.ParseTuyaCloudKey(raw); e != nil {
			return c, errors.New("TUYA_CLOUD_PUBLIC_KEY must be 32 base64-encoded bytes")
		}
	}
	if c.TuyaCloudLinksPerTenant, e = intEnv("TUYA_CLOUD_MAX_LINKS_PER_TENANT", 2, 1, 100); e != nil {
		return c, e
	}
	if c.TuyaCloudEventBudget, e = intEnv("TUYA_CLOUD_EVENT_BUDGET", 68000, 0, 1_000_000_000); e != nil {
		return c, e
	}
	if c.TuyaCloudAPIBudget, e = intEnv("TUYA_CLOUD_API_BUDGET", 26000, 0, 1_000_000_000); e != nil {
		return c, e
	}
	if raw := os.Getenv("CHANNEL_SEAL_KEY"); raw != "" {
		if c.SealKey, e = base64.StdEncoding.DecodeString(raw); e != nil || len(c.SealKey) < 32 {
			return c, errors.New("CHANNEL_SEAL_KEY must contain at least 32 base64-encoded random bytes")
		}
	}
	list := func(name string) []string {
		out := []string{}
		for _, v := range strings.Split(os.Getenv(name), ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	c.TrustedProxies, c.WebhookAllowedHosts = list("TRUSTED_PROXIES"), list("WEBHOOK_ALLOWED_HOSTS")
	c.APIRateLimit = 120
	if raw := os.Getenv("API_RATE_LIMIT"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 30 || n > 6000 {
			return c, errors.New("API_RATE_LIMIT must be between 30 and 6000 requests per minute")
		}
		c.APIRateLimit = n
	}
	if c.SMTPHost = os.Getenv("SMTP_HOST"); c.SMTPHost != "" {
		port, e := strconv.Atoi(os.Getenv("SMTP_PORT"))
		if e != nil || port < 1 || port > 65535 {
			return c, errors.New("SMTP_PORT must be a valid port when SMTP_HOST is set")
		}
		c.SMTPPort, c.SMTPUsername, c.SMTPPassword, c.SMTPFrom = port, os.Getenv("SMTP_USERNAME"), os.Getenv("SMTP_PASSWORD"), os.Getenv("SMTP_FROM")
		if _, e := mail.ParseAddress(c.SMTPFrom); e != nil {
			return c, errors.New("SMTP_FROM must be a valid address")
		}
	}
	if c.Mode == "onprem" && c.Registration {
		return c, errors.New("onprem registration is disabled; provision using the admin command")
	}
	if c.Environment == "production" {
		if c.Registration {
			return c, errors.New("public registration requires email verification; use local provisioning until implemented")
		}
		db, e := url.Parse(c.DatabaseURL)
		if e != nil || !strings.HasPrefix(db.Scheme, "postgres") {
			return c, errors.New("PostgreSQL URL required")
		}
		ssl := db.Query().Get("sslmode")
		if ssl != "verify-full" {
			return c, errors.New("production DATABASE_URL requires sslmode=verify-full")
		}
		if c.AuthDatabaseURL != "" {
			auth, e := url.Parse(c.AuthDatabaseURL)
			if e != nil || !strings.HasPrefix(auth.Scheme, "postgres") || auth.Query().Get("sslmode") != "verify-full" {
				return c, errors.New("production AUTH_DATABASE_URL requires a PostgreSQL URL with sslmode=verify-full")
			}
		}
	}
	return c, nil
}
