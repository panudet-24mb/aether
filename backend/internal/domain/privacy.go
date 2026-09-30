package domain

import (
	"encoding/json"
	"time"
)

// NoticeVersion is the version of the privacy notice members acknowledge (identity.users.notice_ack_version).
// Raise it when the notice changes materially: everybody is asked again.
const NoticeVersion = 1

// AccessEntry is one row of the read-access log (core.access_log): who read which personal data, when.
type AccessEntry struct {
	At          time.Time `json:"at"`
	ActorID     string    `json:"actor_id"`
	ActorName   *string   `json:"actor_name"`
	Resource    string    `json:"resource"`
	SubjectKind string    `json:"subject_kind"`
	SubjectID   string    `json:"subject_id"`
	RequestID   *string   `json:"request_id,omitempty"`
	ClientIP    *string   `json:"client_ip"`
	ID          string    `json:"id"`
}

// AccessRead is what the read-access middleware records for one request.
type AccessRead struct {
	Resource    string
	SubjectKind string
	SubjectID   string
	RequestID   string
	ClientIP    string
}

// AuditEntry is one row of core.audit_logs as the owner's privacy view shows it.
type AuditEntry struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	ActorID   string    `json:"actor_id"`
	ActorName *string   `json:"actor_name"`
	Action    string    `json:"action"`
	TargetID  string    `json:"target_id"`
}

// ErasureEntry is one row of core.erasure_log.
type ErasureEntry struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	At          time.Time       `json:"at"`
	ActorID     *string         `json:"actor_id"`
	SubjectKind string          `json:"subject_kind"`
	SubjectRef  string          `json:"subject_ref"`
	Counts      json.RawMessage `json:"counts"`
	Scope       json.RawMessage `json:"scope"`
}

// LogFilter narrows the privacy views. Before is a keyset cursor (the last row of the previous page).
type LogFilter struct {
	ActorID     string
	Resource    string
	SubjectKind string
	SubjectID   string
	Action      string
	From, To    *time.Time
	BeforeAt    *time.Time
	BeforeID    string
	Limit       int
}

// ErasureResult is what an erasure answers: the outcome and how many rows of each kind went.
type ErasureResult struct {
	Outcome string          `json:"outcome"`
	Counts  json.RawMessage `json:"counts"`
}

// MemberExport is everything the workspace holds about one member (PDPA §30-31): the profile, the membership,
// sessions (no token material), what they did (audit trail, alerts they handled, commands they sent, flows they
// enabled) and the read-access log entries by or about them. Each part is the JSON the database produced.
type MemberExport struct {
	GeneratedAt time.Time       `json:"generated_at"`
	TenantID    string          `json:"tenant_id"`
	Profile     json.RawMessage `json:"profile"`
	Membership  json.RawMessage `json:"membership"`
	Sessions    json.RawMessage `json:"sessions"`
	Audit       json.RawMessage `json:"audit_trail"`
	Alerts      json.RawMessage `json:"alerts_handled"`
	Commands    json.RawMessage `json:"commands_sent"`
	Flows       json.RawMessage `json:"flows_enabled"`
	AccessLog   json.RawMessage `json:"access_log"`
	Notes       []string        `json:"notes"`
}

// IdentityExport is the small part of a tag export (registration, presence, events, alerts); the sample and raw
// advertisement history streams after it, at most IdentityExportRows rows each.
type IdentityExport struct {
	GeneratedAt time.Time       `json:"generated_at"`
	ExternalID  string          `json:"external_id"`
	Device      json.RawMessage `json:"device"`
	Presence    json.RawMessage `json:"presence"`
	Events      json.RawMessage `json:"events"`
	Alerts      json.RawMessage `json:"alerts"`
}

// IdentityExportRows caps the sample and advertisement rows one tag export carries (each): the stream must finish
// within the server's write timeout.
const IdentityExportRows = 100000
