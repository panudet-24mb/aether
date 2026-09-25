package app

import (
	"aether/backend/internal/adapters/edge"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"aether/backend/internal/ports"
	"aether/backend/internal/security"
	"github.com/google/uuid"
)

type Service struct {
	Repo         ports.Repository
	Tokens       *security.Tokens
	Registration bool
	// Secrets seals per-tenant notification channel secrets at rest (derived from the signing key).
	Secrets []byte
	// LegacySecrets opens secrets sealed before CHANNEL_SEAL_KEY was introduced (the JWT-derived key).
	LegacySecrets []byte
	dummyHash     string
	hashing       chan struct{}
}
type AuthResult struct {
	AccessToken    string    `json:"access_token"`
	TokenType      string    `json:"token_type"`
	ExpiresIn      int       `json:"expires_in"`
	TenantID       string    `json:"tenant_id"`
	UserID         string    `json:"user_id"`
	RefreshToken   string    `json:"-"`
	RefreshExpires time.Time `json:"-"`
	// MustChangePassword: the password was handed over out of band and has to be replaced through
	// POST /api/v1/auth/password before the workspace is used.
	MustChangePassword bool `json:"must_change_password"`
}

func New(repo ports.Repository, tokens *security.Tokens, registration bool) (*Service, error) {
	dummy, e := security.HashPassword(security.RandomToken())
	return &Service{Repo: repo, Tokens: tokens, Registration: registration, Secrets: tokens.DeriveKey("notification-channels"), dummyHash: dummy, hashing: make(chan struct{}, 2)}, e
}
func validName(s string) bool {
	return utf8.ValidString(s) && len(s) >= 1 && len(s) <= 128 && !strings.ContainsAny(s, "\x00\r\n")
}
func (s *Service) hashSlot(ctx context.Context) (func(), error) {
	select {
	case s.hashing <- struct{}{}:
		return func() { <-s.hashing }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *Service) Register(ctx context.Context, email, password, name, tenantName string, bootstrap bool) (domain.Account, error) {
	if !bootstrap && !s.Registration {
		return domain.Account{}, domain.ErrForbidden
	}
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	tenantName = strings.TrimSpace(tenantName)
	parsed, e := mail.ParseAddress(email)
	if e != nil || parsed.Address != email || len(email) > 254 || !validName(name) || !validName(tenantName) {
		return domain.Account{}, domain.ErrInvalid
	}
	release, e := s.hashSlot(ctx)
	if e != nil {
		return domain.Account{}, e
	}
	defer release()
	hash, e := security.HashPassword(password)
	if e != nil {
		return domain.Account{}, e
	}
	a := domain.Account{User: domain.User{ID: uuid.NewString(), Email: email, Name: name, PasswordHash: hash}, TenantID: uuid.NewString()}
	if e = s.Repo.CreateAccount(ctx, a.User, a.TenantID, tenantName, bootstrap); e != nil {
		return domain.Account{}, e
	}
	return a, nil
}
func (s *Service) Login(ctx context.Context, email, password, tenant string) (AuthResult, error) {
	if len(email) > 254 || len(password) > 128 || (tenant != "" && !security.ValidID(tenant)) {
		return AuthResult{}, domain.ErrUnauthorized
	}
	release, e := s.hashSlot(ctx)
	if e != nil {
		return AuthResult{}, e
	}
	defer release()
	u, e := s.Repo.UserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	hash := u.PasswordHash
	if e != nil {
		hash = s.dummyHash
	}
	valid := security.CheckPassword(password, hash)
	if e != nil && !errors.Is(e, domain.ErrUnauthorized) {
		return AuthResult{}, e
	}
	if e != nil || !valid {
		return AuthResult{}, domain.ErrUnauthorized
	}
	now := time.Now().UTC()
	refresh := security.NewRefresh(u.ID)
	session, e := s.Repo.StartSession(ctx, domain.Session{ID: uuid.NewString(), UserID: u.ID, TenantID: tenant, ExpiresAt: now.Add(30 * 24 * time.Hour)}, security.Digest(refresh))
	if e != nil {
		return AuthResult{}, e
	}
	return s.result(session, refresh, now)
}
func (s *Service) result(session domain.Session, refresh string, now time.Time) (AuthResult, error) {
	token, e := s.Tokens.Access(session, now)
	seconds := min(900, int(session.ExpiresAt.Sub(now).Seconds()))
	return AuthResult{AccessToken: token, TokenType: "Bearer", ExpiresIn: seconds, TenantID: session.TenantID, UserID: session.UserID, RefreshToken: refresh, RefreshExpires: session.ExpiresAt, MustChangePassword: session.MustChangePassword}, e
}
func (s *Service) Refresh(ctx context.Context, raw string) (AuthResult, error) {
	user, e := security.RefreshUser(raw)
	if e != nil {
		return AuthResult{}, e
	}
	next := security.NewRefresh(user)
	now := time.Now().UTC()
	session, e := s.Repo.RotateRefresh(ctx, user, security.Digest(raw), security.Digest(next), now)
	if e != nil {
		return AuthResult{}, e
	}
	return s.result(session, next, now)
}
func (s *Service) Authenticate(ctx context.Context, token string) (domain.Principal, error) {
	p, e := s.Tokens.Verify(token)
	if e != nil {
		return p, e
	}
	return s.Repo.Authorize(ctx, p)
}
func validProject(name, description, color string) bool {
	ok := false
	for _, c := range domain.ProjectColors {
		ok = ok || c == color
	}
	return ok && validName(name) && utf8.ValidString(description) && len(description) <= 500 && !strings.ContainsAny(description, "\x00")
}
func (s *Service) CreateProject(ctx context.Context, p domain.Principal, name, description, color string) (domain.Project, error) {
	if !p.CanManageDevices() {
		return domain.Project{}, domain.ErrForbidden
	}
	if color == "" {
		color = "mint"
	}
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if !validProject(name, description, color) {
		return domain.Project{}, domain.ErrInvalid
	}
	pr := domain.Project{ID: uuid.NewString(), Name: name, Description: description, Color: color, CreatedAt: time.Now().UTC()}
	return pr, s.Repo.CreateProject(ctx, p, pr)
}
func (s *Service) UpdateProject(ctx context.Context, p domain.Principal, id, name, description, color string) (domain.Project, error) {
	if !p.CanManageDevices() {
		return domain.Project{}, domain.ErrForbidden
	}
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if !security.ValidID(id) || !validProject(name, description, color) {
		return domain.Project{}, domain.ErrInvalid
	}
	pr := domain.Project{ID: id, Name: name, Description: description, Color: color}
	return pr, s.Repo.UpdateProject(ctx, p, pr)
}

// CreateGatewayIn is CreateGateway with an optional project assignment.
func (s *Service) CreateGatewayIn(ctx context.Context, p domain.Principal, name, model string, project *string) (domain.Gateway, string, error) {
	if project != nil && !security.ValidID(*project) {
		return domain.Gateway{}, "", domain.ErrInvalid
	}
	return s.createGateway(ctx, p, name, model, project)
}
func (s *Service) CreateGateway(ctx context.Context, p domain.Principal, name, model string) (domain.Gateway, string, error) {
	return s.createGateway(ctx, p, name, model, nil)
}
func (s *Service) createGateway(ctx context.Context, p domain.Principal, name, model string, project *string) (domain.Gateway, string, error) {
	if !p.CanManageDevices() {
		return domain.Gateway{}, "", domain.ErrForbidden
	}
	name = strings.TrimSpace(name)
	if !validName(name) || domain.GatewayModelByID(model) == nil {
		return domain.Gateway{}, "", domain.ErrInvalid
	}
	g := domain.Gateway{ID: uuid.NewString(), TenantID: p.TenantID, Name: name, Model: model, CreatedAt: time.Now().UTC(), ProjectID: project}
	token := security.RandomToken()
	e := s.Repo.CreateGateway(ctx, p, g, security.Digest(token))
	if e != nil {
		return domain.Gateway{}, "", e
	}
	return g, token, nil
}
func (s *Service) RevokeGateway(ctx context.Context, p domain.Principal, id string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if !security.ValidID(id) {
		return domain.ErrInvalid
	}
	return s.Repo.RevokeGateway(ctx, p, id)
}
func (s *Service) CreateDevice(ctx context.Context, p domain.Principal, gateway, name, external, profile string) (domain.Device, error) {
	if !p.CanManageDevices() {
		return domain.Device{}, domain.ErrForbidden
	}
	if !security.ValidID(gateway) || !validName(strings.TrimSpace(name)) || !validName(external) || domain.DeviceProfileByID(profile) == nil {
		return domain.Device{}, domain.ErrInvalid
	}
	d := domain.Device{ID: uuid.NewString(), TenantID: p.TenantID, GatewayID: gateway, Name: strings.TrimSpace(name), ExternalID: external, ProfileID: profile, CreatedAt: time.Now().UTC()}
	e := s.Repo.CreateDevice(ctx, p, d)
	return d, e
}

// UpdateDevice renames and/or moves a registration; at least one change is required.
func (s *Service) UpdateDevice(ctx context.Context, p domain.Principal, id string, name, gateway *string) (domain.Device, error) {
	if !p.CanManageDevices() {
		return domain.Device{}, domain.ErrForbidden
	}
	if !security.ValidID(id) || (name == nil && gateway == nil) {
		return domain.Device{}, domain.ErrInvalid
	}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if !validName(trimmed) {
			return domain.Device{}, domain.ErrInvalid
		}
		name = &trimmed
	}
	if gateway != nil && !security.ValidID(*gateway) {
		return domain.Device{}, domain.ErrInvalid
	}
	return s.Repo.UpdateDevice(ctx, p, id, name, gateway)
}
func (s *Service) RemoveDevice(ctx context.Context, p domain.Principal, id string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if !security.ValidID(id) {
		return domain.ErrInvalid
	}
	return s.Repo.RemoveDevice(ctx, p, id)
}
func (s *Service) RestoreDevice(ctx context.Context, p domain.Principal, id string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if !security.ValidID(id) {
		return domain.ErrInvalid
	}
	return s.Repo.RestoreDevice(ctx, p, id)
}
func (s *Service) Gateway(ctx context.Context, id, token string) (string, error) {
	if !security.ValidID(id) || len(token) != 43 {
		return "", domain.ErrUnauthorized
	}
	return s.Repo.GatewayTenant(ctx, id, security.Digest(token))
}
func (s *Service) Capture(ctx context.Context, tenant, gateway string, body []byte) (string, error) {
	if len(body) == 0 || len(body) > 256*1024 || !json.Valid(body) {
		return "", domain.ErrInvalid
	}
	first := strings.TrimSpace(string(body))
	if first[0] != '{' && first[0] != '[' {
		return "", domain.ErrInvalid
	}
	return s.Repo.CapturePacket(ctx, tenant, gateway, json.RawMessage(body))
}

// CaptureZ2M validates one Zigbee2MQTT message before it is stored. bridge/devices lists the whole network and
// may be large (the broker accepts up to 1 MiB); every other message is a small device or bridge report.
// Availability and bridge/state may be the legacy bare string, so only device state must be a JSON object.
func (s *Service) CaptureZ2M(ctx context.Context, tenant, gateway string, m zigbee2mqtt.Message, body []byte) (string, error) {
	limit := 64 * 1024
	if m.Kind == zigbee2mqtt.BridgeDevices {
		limit = zigbee2mqtt.MaxPacket
		// A broker still on the old 256 KiB max_packet_size drops a document this large before it reaches us;
		// say so while it still fits, so the operator re-renders mosquitto.conf in time.
		if len(body) > 256*1024 {
			slog.Warn("Zigbee2MQTT bridge/devices exceeds 256 KiB; the broker needs max_packet_size 1048576 (re-run setup.py, restart mqtt)", "gateway", gateway, "bytes", len(body))
		}
	}
	if len(body) == 0 || len(body) > limit || !utf8.Valid(body) {
		slog.Warn("Zigbee2MQTT message rejected: empty, not UTF-8 or over the size cap", "topic", m.Topic, "bytes", len(body), "cap", limit)
		return "", domain.ErrInvalid
	}
	// PostgreSQL text and jsonb cannot hold NUL. Such a message would fail in the database on every redelivery,
	// so it is rejected here as permanently invalid (acknowledged and dropped). This also refuses the six-character
	// text \u0000 inside a string, which is harmless: no real Zigbee2MQTT report contains it.
	if bytes.IndexByte(body, 0) >= 0 || bytes.Contains(body, []byte(`\u0000`)) {
		slog.Warn("Zigbee2MQTT message rejected: contains NUL", "topic", m.Topic)
		return "", domain.ErrInvalid
	}
	if m.Kind == zigbee2mqtt.State || m.Kind == zigbee2mqtt.BridgeDevices {
		first := strings.TrimSpace(string(body))
		if !json.Valid(body) || first == "" || (m.Kind == zigbee2mqtt.State && first[0] != '{') || (m.Kind == zigbee2mqtt.BridgeDevices && first[0] != '[') {
			return "", domain.ErrInvalid
		}
	}
	return s.Repo.CaptureZ2M(ctx, tenant, gateway, m, body)
}

// CaptureEdge validates one Aether Edge message before it is stored: bounded, UTF-8, free of NUL (PostgreSQL
// cannot hold it, and it would fail on every redelivery), and a JSON object where one is expected. Status and
// availability may be the bare string.
func (s *Service) CaptureEdge(ctx context.Context, tenant, gateway string, m edge.Message, body []byte) (string, error) {
	limit := 16 * 1024
	if m.Kind == edge.Discovery {
		limit = edge.MaxPacket
	}
	if len(body) == 0 || len(body) > limit || !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 || bytes.Contains(body, []byte(`\u0000`)) {
		slog.Warn("Aether Edge message rejected: empty, over the size cap, not UTF-8 or contains NUL", "topic", m.Topic, "bytes", len(body), "cap", limit)
		return "", domain.ErrInvalid
	}
	first := strings.TrimSpace(string(body))
	switch m.Kind {
	case edge.State, edge.Health:
		if !json.Valid(body) || first == "" || first[0] != '{' {
			return "", domain.ErrInvalid
		}
	case edge.Discovery:
		if !json.Valid(body) || first == "" || first[0] != '[' {
			return "", domain.ErrInvalid
		}
	}
	return s.Repo.CaptureEdge(ctx, tenant, gateway, m, body)
}
func (s *Service) Ingest(ctx context.Context, tenant, gateway, device string, ts time.Time, metrics map[string]float64) (domain.TelemetryEvent, error) {
	if !security.ValidID(device) || len(metrics) == 0 || len(metrics) > 3 || ts.IsZero() || ts.Before(time.Now().Add(-7*24*time.Hour)) || ts.After(time.Now().Add(time.Minute)) {
		return domain.TelemetryEvent{}, domain.ErrInvalid
	}
	for key, v := range metrics {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return domain.TelemetryEvent{}, domain.ErrInvalid
		}
		switch key {
		case "temperature":
			if v < -100 || v > 200 {
				return domain.TelemetryEvent{}, domain.ErrInvalid
			}
		case "humidity", "battery":
			if v < 0 || v > 100 {
				return domain.TelemetryEvent{}, domain.ErrInvalid
			}
		default:
			return domain.TelemetryEvent{}, domain.ErrInvalid
		}
	}
	return s.Repo.StoreTelemetry(ctx, tenant, gateway, device, ts.UTC(), metrics)
}

// Members. Only the Argon2id work lives here (the hashing slot is process-private); every rule that
// depends on database state is decided inside the writing transaction in the postgres adapter.

// AddMember creates the membership, and the identity when the email is new. An email whose identity still
// belongs to some workspace is refused with a plain conflict, the same answer a duplicate member gets, so
// the route cannot be used to ask "does this email have an account anywhere?".
func (s *Service) AddMember(ctx context.Context, p domain.Principal, email, name, role, password string, projects []string) (domain.Member, error) {
	if !p.CanManageMembers() {
		return domain.Member{}, domain.ErrForbidden
	}
	release, e := s.hashSlot(ctx)
	if e != nil {
		return domain.Member{}, e
	}
	defer release()
	hash, e := security.HashPassword(password)
	if e != nil {
		return domain.Member{}, e
	}
	return s.Repo.AddMember(ctx, p, domain.NewMember{Email: email, Name: name, Role: role, PasswordHash: hash, ProjectIDs: projects})
}

// ResetMemberPassword replaces a member's password. The repository refuses (409) when that identity also
// belongs to another workspace, so one owner can never lock a person out of somebody else's organisation.
func (s *Service) ResetMemberPassword(ctx context.Context, p domain.Principal, target, password string) error {
	if !p.CanManageMembers() {
		return domain.ErrForbidden
	}
	if !security.ValidID(target) {
		return domain.ErrInvalid
	}
	release, e := s.hashSlot(ctx)
	if e != nil {
		return e
	}
	defer release()
	hash, e := security.HashPassword(password)
	if e != nil {
		return e
	}
	return s.Repo.ResetMemberPassword(ctx, p, target, hash)
}

// ChangePassword replaces the signed-in user's own password after verifying the current one.
func (s *Service) ChangePassword(ctx context.Context, p domain.Principal, current, next string) error {
	if len(current) > 128 || current == next {
		return domain.ErrInvalid
	}
	release, e := s.hashSlot(ctx)
	if e != nil {
		return e
	}
	defer release()
	hash, e := security.HashPassword(next)
	if e != nil {
		return e
	}
	return s.Repo.ChangeOwnPassword(ctx, p, func(stored string) bool { return security.CheckPassword(current, stored) }, hash)
}
