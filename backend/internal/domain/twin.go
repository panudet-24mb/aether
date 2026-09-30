package domain

import "time"

// Digital twin (docs/platform/digital-twin.md): the live state of one site, drawn over its 3D floor plans.
//
// Positions are what Aether actually knows: devices stand where they were placed on the plan; a worn tag is
// in the zone of the gateway the ingest decided it is near (smoothed RSSI with hysteresis), never at a
// coordinate. People are personal data, so how they are shown is a mode:
//   - counts: how many worn tags each gateway holds, no identity at all (the default);
//   - tracks: one pseudonymous id per tag, stable within one response only;
//   - named: the device's name (usually the wearer's).
//
// Until twin_settings exists (phase P2) named is allowed only in a demo workspace (core.tenants.demo) and only
// for owner, admin and operator; everyone else gets counts, whatever they ask for.

const (
	TwinPeopleCounts = "counts"
	TwinPeopleTracks = "tracks"
	TwinPeopleNamed  = "named"

	// TwinMaxPlacements bounds one site's state (the budget is ~500 devices and ~50 worn tags).
	TwinMaxPlacements = 2000
	TwinMaxPeople     = 1000
	TwinMaxAlerts     = 200
	// TwinFreshSec is how long after its last uplink a device still counts as online.
	TwinFreshSec = 120
)

// ValidTwinPeople reports whether s names a people mode.
func ValidTwinPeople(s string) bool {
	return s == TwinPeopleCounts || s == TwinPeopleTracks || s == TwinPeopleNamed
}

// TwinPeopleFor is the people mode a principal gets: the one asked for, clamped by role and workspace.
func TwinPeopleFor(p Principal, want string, demo bool) string {
	switch want {
	case TwinPeopleNamed:
		if demo && (p.Role == "owner" || p.Role == "admin" || p.Role == "operator") {
			return TwinPeopleNamed
		}
		return TwinPeopleCounts
	case TwinPeopleTracks:
		return TwinPeopleTracks
	}
	return TwinPeopleCounts
}

type TwinDevice struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"` // gateway | device
	External string     `json:"ext,omitempty"`
	Name     string     `json:"name"`
	Profile  string     `json:"profile,omitempty"`
	FloorID  string     `json:"floor_id"`
	X        float64    `json:"x"`
	Y        float64    `json:"y"`
	Z        float64    `json:"z"`
	Online   bool       `json:"online"`
	LastAt   *time.Time `json:"last_at"`
	T        *float64   `json:"t,omitempty"`
	H        *float64   `json:"h,omitempty"`
	Battery  *float64   `json:"battery,omitempty"`
	Door     *int       `json:"door,omitempty"`
	MotionAt *time.Time `json:"motion_at,omitempty"`
	Alert    bool       `json:"alert"`
	SOS      bool       `json:"sos"`
}

type TwinGatewayCount struct {
	GatewayID string `json:"gateway_id"`
	N         int    `json:"n"`
}

type TwinPerson struct {
	PID                string     `json:"pid"`
	Name               string     `json:"name,omitempty"`
	GatewayID          *string    `json:"gateway_id"`
	Since              *time.Time `json:"since"`
	CandidateGatewayID *string    `json:"candidate_gateway_id,omitempty"`
	LastAt             *time.Time `json:"last_at"`
	SOS                bool       `json:"sos"`
}

type TwinPresence struct {
	Mode   string             `json:"mode"`
	Counts []TwinGatewayCount `json:"counts"`
	People []TwinPerson       `json:"people,omitempty"`
}

type TwinAlert struct {
	ID       string    `json:"id"`
	Severity string    `json:"severity"`
	Status   string    `json:"status"`
	Event    string    `json:"event"`
	SOS      bool      `json:"sos"`
	Hazard   bool      `json:"hazard"`
	Title    string    `json:"title"`
	DeviceID string    `json:"device_id,omitempty"`
	PID      string    `json:"pid,omitempty"`
	OpenedAt time.Time `json:"opened_at"`
	// GatewayID is where to look: the gateway a worn tag is near now (its zone), else the gateway that raised it.
	GatewayID string `json:"gateway_id"`
}

type TwinState struct {
	ServerTime     time.Time      `json:"server_time"`
	SiteID         string         `json:"site_id"`
	Demo           bool           `json:"demo"`
	LayoutRevision map[string]int `json:"layout_revision"`
	Devices        []TwinDevice   `json:"devices"`
	Presence       TwinPresence   `json:"presence"`
	Alerts         []TwinAlert    `json:"alerts"`
}

// TwinEventTitle is an alert title that names nobody, for the people modes that must not identify a wearer.
func TwinEventTitle(eventType string) string {
	switch eventType {
	case EventButton:
		return "SOS · กดปุ่มฉุกเฉิน"
	case EventHazard:
		return "อันตราย · ควัน แก๊ส หรือ CO"
	case "zone":
		return "เข้าโซนที่เฝ้าดู"
	case "offline":
		return "อุปกรณ์ขาดการติดต่อ"
	}
	return "การแจ้งเตือน"
}
