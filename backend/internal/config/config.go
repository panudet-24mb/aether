package config

import (
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
	JWTKey                                                 []byte
	Registration                                           bool
	SecureCookies                                          bool
	// Optional outbound e-mail for alert notifications; unset means the email channel is unavailable.
	SMTPHost, SMTPUsername, SMTPPassword, SMTPFrom string
	SMTPPort                                       int
	// Requests per minute per client IP on /api (default 120). Several users behind one NAT, or one user with
	// several live pages open, share this budget, so deployments may raise it.
	APIRateLimit int
	// Storage and rollout controls (see docs/production.md).
	SampleRetentionDays  int      // decoded samples older than this are deleted by the worker (default 90)
	SampleMinIntervalSec int      // store at most one environment sample per stream per interval (0 = every uplink)
	BLEHistoryHours      int      // raw BLE advertisement archive kept for Studio decoders (default 24)
	DiscoveryLimit       int      // streams per gateway for tags that are NOT registered devices (default 100)
	AlertsShadow         bool     // record events but open no alerts, send nothing and run no automations; SOS (button) and hazard still alert
	SealKey              []byte   // optional CHANNEL_SEAL_KEY; falls back to a key derived from JWT_SIGNING_KEY
	TrustedProxies       []string // CIDRs/IPs of the reverse proxy; only then is X-Forwarded-For believed
	WebhookAllowedHosts  []string // host:port targets exempt from the private-address block (on-prem relays)
}

func Load() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), Listen: os.Getenv("LISTEN_ADDR"), Mode: os.Getenv("DEPLOYMENT_MODE"), Environment: os.Getenv("APP_ENV"), Origin: os.Getenv("APP_ORIGIN"), Issuer: "aether", SecureCookies: true}
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
	}
	return c, nil
}
