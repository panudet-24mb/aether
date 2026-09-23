package domain

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrInvalid      = errors.New("invalid_input")
	ErrConflict     = errors.New("conflict")
	ErrNotFound     = errors.New("not_found")
)

type User struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	PasswordHash string `json:"-"`
}
type Account struct {
	User     User   `json:"user"`
	TenantID string `json:"tenant_id"`
}
type Principal struct {
	UserID    string
	TenantID  string
	SessionID string
	Role      string
	// MustChangePassword is re-read from the membership on every request. While it is set the session may
	// only change its own password, read /me or log out; see PasswordChangeRequired.
	MustChangePassword bool
}

// PasswordChangeRequired is the machine-readable detail clients get while a handed-over password stands.
const PasswordChangeRequired = "password_change_required"

func (p Principal) CanManageDevices() bool { return p.Role == "owner" || p.Role == "admin" }

type Session struct {
	ID        string
	UserID    string
	TenantID  string
	ExpiresAt time.Time
	// MustChangePassword carries core.memberships.must_change_password into the login/refresh response:
	// an on-premise workspace has no email delivery, so an initial or reset password is handed over in
	// person and the UI has to force a replacement before anything else.
	MustChangePassword bool
}
type Gateway struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	// ProjectID is nil for gateways that are not assigned to a project.
	ProjectID *string `json:"project_id"`
}
type Device struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	GatewayID  string    `json:"gateway_id"`
	Name       string    `json:"name"`
	ExternalID string    `json:"external_id"`
	ProfileID  string    `json:"profile_id"`
	CreatedAt  time.Time `json:"created_at"`
	// Roaming devices (wearables) are followed across every gateway of the workspace, not only their home gateway.
	Roaming bool `json:"roaming"`
	// ZoneGatewayID is the stable zone of a roaming device (decided at ingest), nil when unknown.
	ZoneGatewayID *string `json:"zone_gateway_id,omitempty"`
	// RemovedAt is set when the registration was withdrawn; history is kept and it can be restored.
	RemovedAt *time.Time `json:"removed_at,omitempty"`
}
type State struct {
	DeviceID string          `json:"device_id"`
	TS       time.Time       `json:"ts"`
	Metrics  json.RawMessage `json:"metrics"`
}
type Packet struct {
	ID         string          `json:"id"`
	GatewayID  string          `json:"gateway_id"`
	ReceivedAt time.Time       `json:"received_at"`
	Payload    json.RawMessage `json:"payload"`
}

// Canonical event contains only authenticated server-assigned routing identity.
type TelemetryEvent struct {
	Schema     string             `json:"schema"`
	TenantID   string             `json:"tenant_id"`
	DeviceID   string             `json:"device_id"`
	ProfileID  string             `json:"profile_id"`
	TS         time.Time          `json:"ts"`
	ReceivedAt time.Time          `json:"received_at"`
	Metrics    map[string]float64 `json:"metrics"`
}
