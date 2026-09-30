package alerts

import (
	"aether/backend/internal/domain"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// SMTPSettings come from the deployment environment; tenants only choose recipients.
type SMTPSettings struct {
	Host, From, Username, Password string
	Port                           int
}

func (s SMTPSettings) Configured() bool { return s.Host != "" }

// Sender delivers one notification to one channel. It never logs secrets or message bodies.
type Sender struct {
	HTTP        *http.Client
	SMTP        SMTPSettings
	Environment string // development allows plain-http webhooks to localhost
	// Allowed lists host:port webhook targets the operator vouches for (an on-prem HIS relay). They may be
	// private addresses and plain HTTP; everything else keeps the public-HTTPS-only rule.
	Allowed map[string]bool
}

type allowPrivateKey struct{}

// hostPort normalises a URL host to host:port for the allowlist.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

func blockedIP(ip net.IP) bool {
	cgnat := net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

func NewSender(smtpSettings SMTPSettings, environment string, allowedHosts ...string) *Sender {
	allowed := map[string]bool{}
	for _, h := range allowedHosts {
		allowed[strings.ToLower(strings.TrimSpace(h))] = true
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if environment != "development" {
		// Enforced on the resolved address at connect time, so a public hostname that resolves (or later
		// re-resolves) to an internal address is refused: closes DNS-rebinding style SSRF.
		dialer.ControlContext = func(ctx context.Context, _, address string, _ syscall.RawConn) error {
			if ok, _ := ctx.Value(allowPrivateKey{}).(bool); ok { // target is on the operator's allowlist
				return nil
			}
			host, _, e := net.SplitHostPort(address)
			if e != nil || blockedIP(net.ParseIP(host)) {
				return errors.New("blocked address")
			}
			return nil
		}
	}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	return &Sender{HTTP: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") }}, SMTP: smtpSettings, Environment: environment, Allowed: allowed}
}

// Payload is the JSON document posted to webhooks.
type Payload struct {
	Schema   string             `json:"schema"`
	SentAt   time.Time          `json:"sent_at"`
	Text     string             `json:"text"`
	Alert    domain.Alert       `json:"alert"`
	Event    domain.DeviceEvent `json:"event"`
	Gateway  string             `json:"gateway_name"`
	TenantID string             `json:"tenant_id"`
}

// ValidateChannel checks the public config for a kind; secret rules are enforced separately.
func (s *Sender) ValidateChannel(kind string, config map[string]string, hasSecret bool) error {
	switch kind {
	case "webhook":
		return s.validateWebhookURL(config["url"])
	case "line":
		to := config["to"]
		if !hasSecret || to == "" || len(to) > 64 || strings.ContainsAny(to, " \r\n") {
			return domain.ErrInvalid
		}
	case "email":
		if !s.SMTP.Configured() {
			return errors.New("smtp_not_configured")
		}
		if config["audience"] == AudienceMembers {
			// Recipients are resolved per alert from the members who can see its project.
			if _, ok := MemberRoles(config); !ok || config["to"] != "" {
				return domain.ErrInvalid
			}
			return nil
		}
		if config["audience"] != "" || len(recipients(config)) == 0 {
			return domain.ErrInvalid
		}
	default:
		return domain.ErrInvalid
	}
	return nil
}

// AudienceMembers is the email channel audience resolved at delivery time from the workspace members who
// can see the alert's project (core.alert_recipients); the worker fills config["to"] before sending.
const AudienceMembers = "members"

// DefaultMemberRoles is who a "members" email reaches when the channel names no roles.
var DefaultMemberRoles = []string{"owner", "admin", "operator"}

// MemberRoles parses config["roles"] (comma separated) for a "members" email channel.
func MemberRoles(config map[string]string) ([]string, bool) {
	raw := strings.TrimSpace(config["roles"])
	if raw == "" {
		return DefaultMemberRoles, true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		role := strings.TrimSpace(part)
		if role != "owner" && role != "admin" && role != "operator" && role != "viewer" {
			return nil, false
		}
		if !seen[role] {
			seen[role] = true
			out = append(out, role)
		}
	}
	return out, len(out) > 0
}

// MaxMemberRecipients caps one "members" delivery; the worker logs when more members qualify.
const MaxMemberRecipients = 50

// recipients parses config["to"]. A hand-written list is all-or-nothing, as the operator typed it; a "members"
// list comes from the directory, so one malformed stored address is skipped rather than silencing the rest.
func recipients(config map[string]string) []string {
	var out []string
	members := config["audience"] == AudienceMembers
	limit := 10
	if members {
		limit = MaxMemberRecipients
	}
	for _, part := range strings.Split(config["to"], ",") {
		addr := strings.TrimSpace(part)
		if addr == "" {
			continue
		}
		if len(out) >= limit {
			if members {
				break
			}
			return nil
		}
		if p, e := mail.ParseAddress(addr); e != nil || p.Address != addr {
			if members {
				continue
			}
			return nil
		}
		out = append(out, addr)
	}
	return out
}

func (s *Sender) validateWebhookURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || len(raw) > 2048 {
		return domain.ErrInvalid
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	private := host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".localhost")
	if ip := net.ParseIP(host); ip != nil {
		private = blockedIP(ip)
	}
	if u.Scheme == "https" && !private {
		return nil
	}
	if (u.Scheme == "http" || u.Scheme == "https") && s.Allowed[hostPort(u)] {
		return nil
	}
	// Plain HTTP or private targets are development-only conveniences (local webhook receivers).
	if s.Environment == "development" && (u.Scheme == "http" || u.Scheme == "https") {
		return nil
	}
	return domain.ErrInvalid
}

// Send delivers the message; the returned error is safe to store (no secrets).
func (s *Sender) Send(ctx context.Context, ch domain.NotificationChannel, secret string, payload Payload) error {
	switch ch.Kind {
	case "webhook":
		return s.sendWebhook(ctx, ch.Config["url"], secret, payload)
	case "line":
		return s.sendLine(ctx, secret, ch.Config["to"], payload.Text)
	case "email":
		return s.sendEmail(ctx, ch.Config, payload)
	}
	return domain.ErrInvalid
}

func (s *Sender) sendWebhook(ctx context.Context, target, secret string, payload Payload) error {
	if e := s.validateWebhookURL(target); e != nil {
		return errors.New("webhook url rejected")
	}
	// Third parties do not need internal actor ids.
	payload.Alert.AckedBy, payload.Alert.ResolvedBy = nil, nil
	body, _ := json.Marshal(payload)
	if u, e := url.Parse(target); e == nil && s.Allowed[hostPort(u)] {
		ctx = context.WithValue(ctx, allowPrivateKey{}, true)
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Aether-Alerts/1")
	req.Header.Set("X-Aether-Event", payload.Event.EventType)
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Aether-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	res, e := s.HTTP.Do(req)
	if e != nil {
		return errors.New("webhook request failed")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		if s.Environment == "development" {
			return fmt.Errorf("webhook responded %d", res.StatusCode)
		}
		return errors.New("webhook did not accept the request") // no status oracle for probing internal services
	}
	return nil
}

// LINE Messaging API push (https://developers.line.biz/en/reference/messaging-api/#send-push-message).
func (s *Sender) sendLine(ctx context.Context, token, to, text string) error {
	if token == "" || to == "" {
		return errors.New("line channel incomplete")
	}
	if len(text) > 5000 {
		text = text[:5000]
	}
	body, _ := json.Marshal(map[string]any{"to": to, "messages": []map[string]string{{"type": "text", "text": text}}})
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.line.me/v2/bot/message/push", bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, e := s.HTTP.Do(req)
	if e != nil {
		return errors.New("line request failed")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("line responded %d", res.StatusCode)
	}
	return nil
}

func (s *Sender) sendEmail(ctx context.Context, config map[string]string, payload Payload) error {
	if !s.SMTP.Configured() {
		return errors.New("smtp not configured")
	}
	to := recipients(config)
	if len(to) == 0 {
		return errors.New("no recipients")
	}
	subject := strings.ReplaceAll(strings.ReplaceAll(payload.Alert.Title, "\r", " "), "\n", " ")
	message := func(to string) []byte {
		return []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: [Aether] %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n", s.SMTP.From, to, subject, payload.Text))
	}
	client, closeConn, e := s.dialSMTP(ctx)
	if e != nil {
		return e
	}
	defer closeConn()
	defer client.Close()
	if config["audience"] != AudienceMembers {
		// A hand-written list is one message the recipients see together, as before.
		if e := client.Mail(s.SMTP.From); e != nil {
			return errors.New("smtp sender rejected")
		}
		for _, rcpt := range to {
			if e := client.Rcpt(rcpt); e != nil {
				return errors.New("smtp recipient rejected")
			}
		}
		if e := writeData(client, message(strings.Join(to, ", "))); e != nil {
			return e
		}
		return client.Quit()
	}
	// Members of a workspace do not learn each other's addresses: one message addressed to
	// "undisclosed-recipients:;" with one RCPT per member (a blind copy). A rejected address is skipped; the
	// delivery succeeds when the server accepted at least one member, and is sent once, so a retry never
	// repeats it to the members who already have it.
	if e := client.Mail(s.SMTP.From); e != nil {
		return errors.New("smtp sender rejected")
	}
	accepted, rejected := 0, 0
	for _, rcpt := range to {
		if e := client.Rcpt(rcpt); e != nil {
			rejected++
			continue
		}
		accepted++
	}
	if rejected > 0 {
		// Counts only: the addresses are personal data and stay out of the log.
		slog.Warn("alert email: recipients rejected", "accepted", accepted, "rejected", rejected)
	}
	if accepted == 0 {
		_ = client.Reset()
		return errors.New("smtp recipient rejected")
	}
	if e := writeData(client, message("undisclosed-recipients:;")); e != nil {
		return e
	}
	return client.Quit()
}

// dialSMTP connects with a deadline (net/smtp.SendMail has none; one hung MX would stall the single worker for
// every tenant), upgrades to TLS when offered and authenticates.
func (s *Sender) dialSMTP(ctx context.Context) (*smtp.Client, func(), error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(15 * time.Second)
	}
	conn, e := (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", s.SMTP.Host, s.SMTP.Port))
	if e != nil {
		return nil, nil, errors.New("smtp connection failed")
	}
	_ = conn.SetDeadline(deadline)
	client, e := smtp.NewClient(conn, s.SMTP.Host)
	if e != nil {
		conn.Close()
		return nil, nil, errors.New("smtp handshake failed")
	}
	fail := func(msg string) (*smtp.Client, func(), error) {
		client.Close()
		conn.Close()
		return nil, nil, errors.New(msg)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if e := client.StartTLS(&tls.Config{ServerName: s.SMTP.Host, MinVersion: tls.VersionTLS12}); e != nil {
			return fail("smtp starttls failed")
		}
	}
	if s.SMTP.Username != "" {
		if e := client.Auth(smtp.PlainAuth("", s.SMTP.Username, s.SMTP.Password, s.SMTP.Host)); e != nil {
			return fail("smtp authentication failed")
		}
	}
	return client, func() { conn.Close() }, nil
}

func writeData(client *smtp.Client, msg []byte) error {
	w, e := client.Data()
	if e != nil {
		return errors.New("smtp delivery failed")
	}
	if _, e := w.Write(msg); e != nil {
		return errors.New("smtp delivery failed")
	}
	if e := w.Close(); e != nil {
		return errors.New("smtp delivery failed")
	}
	return nil
}
