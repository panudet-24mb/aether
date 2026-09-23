package domain

import (
	"encoding/json"
	"time"
)

// Event types written to the persistent device event log. Rules can subscribe to the first six;
// the *_cleared / online / motion_stopped types are informational counterparts.
const (
	EventTamper         = "tamper"
	EventTamperCleared  = "tamper_cleared"
	EventButton         = "button"
	EventLeak           = "leak"
	EventLeakCleared    = "leak_cleared"
	EventMotion         = "motion"
	EventMotionStopped  = "motion_stopped"
	EventOffline        = "offline"
	EventOnline         = "online"
	EventThreshold      = "threshold"
	EventThresholdClear = "threshold_cleared"
	// EventZone: a roaming wearable settled in another gateway's zone (smoothed RSSI, margin and dwell time).
	EventZone = "zone"
	// Door contact (the `door` metric, 1 = open): edge-triggered like tamper / tamper_cleared.
	EventDoorOpen   = "door_open"
	EventDoorClosed = "door_closed"
	// PIR occupancy: occupied on the transition of `motion` to 1; vacant once no motion has been seen for
	// alerts.OccupancyHoldSec, raised by the periodic worker (never per uplink), once per occupied episode.
	EventOccupied = "occupied"
	EventVacant   = "vacant"
)

// Rule event types that subscribe to a device event of a different name.
const (
	RuleDoor      = "door"      // fires on door_open
	RuleOccupancy = "occupancy" // fires on occupied
)

var RuleEventTypes = []string{EventTamper, EventButton, EventLeak, EventMotion, EventOffline, EventThreshold, EventZone, RuleDoor, RuleOccupancy}
var Severities = []string{"info", "warning", "critical"}
var ChannelKinds = []string{"webhook", "line", "email"}

const DefaultOfflineAfterSec = 300

type RuleScope struct {
	// Empty means every device in the workspace.
	ExternalIDs []string `json:"external_ids,omitempty"`
	// offline rules: seconds without an uplink before the device counts as offline.
	OfflineAfterSec int `json:"offline_after_sec,omitempty"`
	// threshold rules: metric name (temperature, humidity, battery, or a decoded metric key), comparison and value.
	Metric string   `json:"metric,omitempty"`
	Op     string   `json:"op,omitempty"` // > >= < <=
	Value  *float64 `json:"value,omitempty"`
	// zone rules: fire only when the wearable enters one of these gateways' zones (empty = any zone change).
	GatewayIDs []string `json:"gateway_ids,omitempty"`
	// door / occupancy rules: alert only when the event happens inside this local (Asia/Bangkok) window.
	AfterHours *AfterHours `json:"after_hours,omitempty"`
}

// AfterHours is a recurring local-time window, e.g. {from:"18:00", to:"07:00", days:[1,2,3,4,5]}. From > To
// crosses midnight and then belongs to the day it starts on; From == To covers the whole day. Days are
// 0 (Sunday) .. 6 (Saturday); empty means every day.
type AfterHours struct {
	From string `json:"from"`
	To   string `json:"to"`
	Days []int  `json:"days,omitempty"`
}

type AlertRule struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	EventType string    `json:"event_type"`
	Severity  string    `json:"severity"`
	Scope     RuleScope `json:"scope"`
	Channels  []string  `json:"channels"`
	DedupeSec int       `json:"dedupe_sec"`
	// Builtin marks a rule Aether seeded itself (migration 00024 / seedDefaultRules). It is a label only:
	// such a rule can be edited, disabled and deleted like any other.
	Builtin   bool      `json:"builtin"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NotificationChannel never carries its secret over the API; HasSecret says whether one is stored.
type NotificationChannel struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Enabled   bool              `json:"enabled"`
	Config    map[string]string `json:"config"`
	HasSecret bool              `json:"has_secret"`
	CreatedAt time.Time         `json:"created_at"`
}

type DeviceEvent struct {
	ID         string         `json:"id"`
	GatewayID  string         `json:"gateway_id"`
	ExternalID string         `json:"external_id"`
	DeviceName string         `json:"device_name"`
	EventType  string         `json:"event_type"`
	Detail     map[string]any `json:"detail"`
	OccurredAt time.Time      `json:"occurred_at"`
}

type Alert struct {
	ID         string     `json:"id"`
	RuleID     *string    `json:"rule_id"`
	EventID    string     `json:"event_id"`
	GatewayID  string     `json:"gateway_id"`
	ExternalID string     `json:"external_id"`
	DeviceName string     `json:"device_name"`
	EventType  string     `json:"event_type"`
	Severity   string     `json:"severity"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	OpenedAt   time.Time  `json:"opened_at"`
	AckedBy    *string    `json:"acked_by"`
	AckedAt    *time.Time `json:"acked_at"`
	ResolvedBy *string    `json:"resolved_by"`
	ResolvedAt *time.Time `json:"resolved_at"`
	Note       *string    `json:"note"`
}

// SOS reports whether this alert is an emergency button press: a critical alert raised by a `button`
// event. Severity is what the workspace decided the press means (the built-in rule says critical, an
// operator may lower it), so the two together are the whole definition and no caller re-implements it.
// It deliberately says nothing about the lifecycle: a resolved SOS is still an SOS that happened, and
// callers that want the live ones filter on Status, exactly as they do for every other alert.
func (a Alert) SOS() bool { return a.EventType == EventButton && a.Severity == "critical" }

// MarshalJSON adds the derived "sos" field to every alert the API, the WebSocket payloads and the
// webhook body carry, so the frontend never has to know the rule above. Implemented here rather than as
// a stored column because it is a function of two columns that are already there.
func (a Alert) MarshalJSON() ([]byte, error) {
	type plain Alert // no method set, so this does not recurse
	return json.Marshal(struct {
		plain
		SOS bool `json:"sos"`
	}{plain(a), a.SOS()})
}

type Notification struct {
	ID            string     `json:"id"`
	AlertID       string     `json:"alert_id"`
	ChannelID     *string    `json:"channel_id"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	LastError     *string    `json:"last_error"`
	CreatedAt     time.Time  `json:"created_at"`
	SentAt        *time.Time `json:"sent_at"`
}

// NotificationJob is one claimed delivery with everything the sender needs.
type NotificationJob struct {
	Notification Notification
	Channel      NotificationChannel
	SecretEnc    string
	Alert        Alert
	Event        DeviceEvent
	GatewayName  string
}

func (p Principal) CanOperate() bool {
	return p.Role == "owner" || p.Role == "admin" || p.Role == "operator"
}
