package signals

import (
	"strings"
	"time"
)

// EventTypes are the meanings an operator can teach. The first four are existing alert event types,
// so a learned signal feeds the same rules, alerts and notifications as a decoded tamper or leak
// flag. "custom" only lands in the event log: no alert rule can subscribe to it, which is honest
// about what it does rather than pretending it is wired up.
//
// "door" is a state, not an instant: the matched pattern means OPEN and its absence means CLOSED. A
// learned door signal does not raise events itself; it sets the `door` metric (1/0) on the stored
// reading, and the alert engine turns that metric into door_open / door_closed exactly as it would
// for a decoded door flag (see EventDoor and postgres.applyLearnedDoors).
var EventTypes = []string{"button", "tamper", "leak", "motion", "door", "custom"}

// EventDoor is the teachable meaning whose match drives the `door` metric rather than an event.
const EventDoor = "door"

// Scopes: a signature belongs either to one physical tag or to every tag registered with a profile,
// so teaching one B10 teaches all of them.
const (
	ScopeDevice  = "device"
	ScopeProfile = "profile"
)

// Session phases.
const (
	StatusBaseline  = "baseline"
	StatusTrigger   = "trigger"
	StatusFinished  = "finished"
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
)

// Phase durations the API accepts, in seconds. The defaults (20 s each) are what the wizard offers.
const (
	MinPhaseSec     = 5
	MaxPhaseSec     = 180
	DefaultPhaseSec = 20
)

// Limits per workspace, enforced by the repository under the per-tenant advisory lock.
const (
	MaxSignals      = 200
	MaxOpenSessions = 20
)

// Signal is a confirmed signature, evaluated on every uplink of the identities it covers.
type Signal struct {
	ID          string    `json:"id"`
	Scope       string    `json:"scope"`
	ExternalID  string    `json:"external_id,omitempty"`
	ProfileID   string    `json:"profile_id,omitempty"`
	EventType   string    `json:"event_type"`
	Matcher     Matcher   `json:"matcher"`
	Description string    `json:"description"`
	Verified    bool      `json:"verified"`
	CreatedAt   time.Time `json:"created_at"`
}

// Phase is what one capture window collected, so the operator can see whether the gateway heard
// anything at all before the analysis says "no difference".
type Phase struct {
	// Observations is every archived advertisement row in the window (how much arrived).
	Observations int `json:"observations"`
	// Distinct is how many different raw payloads those rows held (how much of it was new).
	Distinct int       `json:"distinct"`
	From     time.Time `json:"from"`
	Until    time.Time `json:"until"`
	Active   bool      `json:"active"`
}

// Session is one teaching run, as reported to the wizard while it is open.
type Session struct {
	ID         string    `json:"id"`
	GatewayID  string    `json:"gateway_id"`
	ExternalID string    `json:"external_id"`
	EventType  string    `json:"event_type"`
	Label      string    `json:"label"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	Baseline   Phase     `json:"baseline"`
	Trigger    Phase     `json:"trigger"`
	// Candidates is populated once the session is finished. Empty with Verdict explaining why.
	Candidates []Candidate `json:"candidates"`
	// Verdict is the Thai sentence the UI shows when there is nothing to confirm.
	Verdict string `json:"verdict"`
}

// TestResult reports how a signature would have behaved against recorded history, which is how an
// operator checks for false positives before trusting it.
type TestResult struct {
	Uplinks   int        `json:"uplinks"`
	Matches   int        `json:"matches"`
	Since     time.Time  `json:"since"`
	LastMatch *time.Time `json:"last_match,omitempty"`
	Verdict   string     `json:"verdict"`
}

// ValidEventType reports whether an event type may be taught.
func ValidEventType(t string) bool {
	for _, x := range EventTypes {
		if x == t {
			return true
		}
	}
	return false
}

// ClearedEventType is the event raised when a learned signal stops matching, for the meanings that
// have a "back to normal" counterpart. A button press has none: it is an instant, not a state, so
// the edge trigger alone keeps it from repeating while the pattern persists.
func ClearedEventType(eventType string) string {
	switch eventType {
	case "tamper":
		return "tamper_cleared"
	case "leak":
		return "leak_cleared"
	case "motion":
		return "motion_stopped"
	case EventDoor:
		return "door_closed"
	}
	return ""
}

// NoDifferenceVerdict is the honest answer when the two phases look the same. It never invents a
// signature; it says what was not seen and what the operator can check next.
func NoDifferenceVerdict(baseline, trigger Phase) string {
	if trigger.Observations == 0 {
		return "gateway ไม่ได้รับสัญญาณใด ๆ จากอุปกรณ์นี้เลยในช่วงที่กระตุ้น · ตรวจว่าอุปกรณ์อยู่ในระยะ และ MG3 ไม่ได้กรอง MAC นี้ทิ้ง (scan filter)"
	}
	return "gateway ไม่ได้รับสัญญาณที่ต่างจากตอนปล่อยนิ่งเลย · สิ่งที่ควรตรวจ: (1) แท็กอาจต้องตั้งค่าในแอป Minew ให้กระจายสัญญาณตอนกด (2) scan filter ของ MG3 อาจตัดเฟรมนั้นทิ้ง (3) การกดอาจสั้นกว่ารอบอัปโหลดของ gateway — ลองกดย้ำ ๆ ค้างไว้ตลอดช่วงที่สอง"
}

// NormaliseExternal is the identity form used everywhere: lowercase hex, no separators.
func NormaliseExternal(s string) string {
	return strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(s)))
}

// NewSession is what the operator asks for when starting to teach a signal.
type NewSession struct {
	GatewayID   string `json:"gateway_id"`
	ExternalID  string `json:"external_id"`
	EventType   string `json:"event_type"`
	Label       string `json:"label"`
	BaselineSec int    `json:"baseline_sec"`
	TriggerSec  int    `json:"trigger_sec"`
}

// Clamp fills in the defaults and holds the phase durations inside the accepted range.
func (n *NewSession) Clamp() {
	if n.BaselineSec == 0 {
		n.BaselineSec = DefaultPhaseSec
	}
	if n.TriggerSec == 0 {
		n.TriggerSec = DefaultPhaseSec
	}
	n.ExternalID = NormaliseExternal(n.ExternalID)
	n.Label = strings.TrimSpace(n.Label)
}

// Valid reports whether the request can be accepted as-is.
func (n NewSession) Valid() bool {
	return len(n.ExternalID) == 12 && ValidEventType(n.EventType) && len(n.Label) <= 64 &&
		n.BaselineSec >= MinPhaseSec && n.BaselineSec <= MaxPhaseSec &&
		n.TriggerSec >= MinPhaseSec && n.TriggerSec <= MaxPhaseSec
}
