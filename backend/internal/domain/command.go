package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrRateLimited is a request refused by a budget (HTTP 429), e.g. the per-workspace command cap.
var ErrRateLimited = errors.New("rate_limited")

// ReasonError narrows a sentinel error (ErrConflict, ErrInvalid, ...) with a machine-readable reason the client
// can act on, e.g. a command refused because the device is "offline". errors.Is still sees the sentinel.
type ReasonError struct {
	Err    error
	Reason string
}

func (e ReasonError) Error() string { return e.Reason }
func (e ReasonError) Unwrap() error { return e.Err }

// Because wraps a sentinel with a reason.
func Because(err error, reason string) error { return ReasonError{Err: err, Reason: reason} }

// CanCommandDevices is who may switch an actuator. Operators run the building day to day; viewers only look.
// Project scope still applies (RLS), and the owner can narrow a member further with the "control" module.
func (p Principal) CanCommandDevices() bool { return p.CanOperate() }

// MayControl applies the member's module access ("control") on top of the role: an owner always may; anyone
// else may unless the owner set control to none or read. Both the HTTP gate and the command service use it.
func (p Principal) MayControl(access map[string]string) bool {
	if !p.CanCommandDevices() {
		return false
	}
	if p.Role == "owner" {
		return true
	}
	level := access["control"]
	return level != "none" && level != "read"
}

// Command statuses. A command is published at most once: pending -> sent -> confirmed | timeout, or
// pending -> expired (never published), or sent -> failed (the broker refused the publish).
const (
	CommandPending   = "pending"
	CommandSent      = "sent"
	CommandConfirmed = "confirmed"
	CommandTimeout   = "timeout"
	CommandExpired   = "expired"
	CommandFailed    = "failed"
)

// Command timing: a pending command older than CommandTTL is never published, and a sent one without a
// matching state report within CommandConfirmWindow times out. A report that arrives up to CommandLateConfirm
// after the timeout still confirms it (no-neutral Tuya switches sometimes report late).
const (
	CommandTTL           = 10 * time.Second
	CommandConfirmWindow = 10 * time.Second
	CommandLateConfirm   = 60 * time.Second
	// CommandTenantPerMinute caps commands per workspace, whatever their source.
	CommandTenantPerMinute = 60
)

// Command is one request to set one property of one device to one value. ID is the client's idempotency key.
// Value is the resolved desired value that is published as {"<property>": <value>}.
type Command struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id"`
	GatewayID    string          `json:"gateway_id"`
	DeviceID     string          `json:"device_id"`
	IEEE         string          `json:"ieee"`
	Property     string          `json:"property"`
	Requested    string          `json:"requested"` // set | toggle
	Value        json.RawMessage `json:"value"`
	Source       string          `json:"source"` // user | automation
	ActorID      *string         `json:"actor_id,omitempty"`
	AutomationID *string         `json:"automation_id,omitempty"`
	Status       string          `json:"status"`
	Error        string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
	SentAt       *time.Time      `json:"sent_at,omitempty"`
	SettledAt    *time.Time      `json:"settled_at,omitempty"`
}

// CommandRequest is what a caller asks for; the repository validates it against the device's definition.
// Action is "set" (Value required) or "toggle" (binary features only; Value must be empty).
type CommandRequest struct {
	ID       string
	DeviceID string
	Property string
	Action   string
	Value    json.RawMessage
}

// DeviceControls is what a Zigbee2MQTT device lets Aether set: its definition's exposes, the last reported value
// of each settable property, and whether it can be reached now (available, bridge online, not offline).
type DeviceControls struct {
	DeviceID  string          `json:"device_id"`
	GatewayID string          `json:"gateway_id"`
	IEEE      string          `json:"ieee"`
	Exposes   json.RawMessage `json:"-"`
	State     json.RawMessage `json:"state"`
	Online    bool            `json:"online"`
}
