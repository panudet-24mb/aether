package domain

import "time"

// Digital twin (docs/platform/digital-twin.md): the live state of one site, drawn over its 3D floor plans.
//
// Positions are what Aether actually knows: devices stand where they were placed on the plan; a worn tag is
// in the zone of the gateway the ingest decided it is near (smoothed RSSI with hysteresis), never at a
// coordinate. People are personal data, so how they are shown is a mode:
//   - off: no people at all (replay only; live keeps anonymous headcounts);
//   - counts: how many worn tags each gateway holds, no identity at all (the default);
//   - tracks: one pseudonymous id per tag, drawn per response (never stable across responses);
//   - named: the device's name (usually the wearer's).
//
// What a principal gets is the mode asked for, capped by the workspace's twin settings (owner-written; a demo
// workspace defaults to named), by role (named needs owner, admin or operator; everyone else at most tracks) and,
// for a wall display, by the settings' display cap. Live never drops below counts.

const (
	TwinPeopleOff    = "off"
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

// ValidTwinPeople reports whether s names a people mode a client may ask for.
func ValidTwinPeople(s string) bool {
	return s == TwinPeopleCounts || s == TwinPeopleTracks || s == TwinPeopleNamed
}

func twinRank(m string) int {
	switch m {
	case TwinPeopleCounts:
		return 1
	case TwinPeopleTracks:
		return 2
	case TwinPeopleNamed:
		return 3
	}
	return 0
}

func twinMin(a, b string) string {
	if twinRank(a) <= twinRank(b) {
		if twinRank(a) == 0 {
			return TwinPeopleOff
		}
		return a
	}
	return b
}

// TwinSettings is a workspace's twin privacy (core.twin_settings; owner-written, audited).
type TwinSettings struct {
	// PeopleReplay caps how people are shown, live and in replay (off: no people in replay, headcounts live).
	PeopleReplay string `json:"people_replay"`
	// PeopleReplayDays is how far back replay shows people (also bounded by the movement history's retention).
	PeopleReplayDays int `json:"people_replay_days"`
	// DisplayPeople caps what wall displays show (at most tracks; a display names nobody).
	DisplayPeople string     `json:"display_people"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	// Stored reports whether the owner saved settings (else these are the defaults).
	Stored bool `json:"stored"`
	Demo   bool `json:"demo"`
	// HistoryDays is the movement history's retention (read-only here; PRESENCE_HISTORY_DAYS at migrate).
	HistoryDays int `json:"history_days"`
}

// DefaultTwinSettings are the settings of a workspace whose owner saved none: counts, a week back; a demo
// workspace (fictional people) shows names.
func DefaultTwinSettings(demo bool) TwinSettings {
	s := TwinSettings{PeopleReplay: TwinPeopleCounts, PeopleReplayDays: 7, DisplayPeople: TwinPeopleCounts, Demo: demo, HistoryDays: 30}
	if demo {
		s.PeopleReplay = TwinPeopleNamed
	}
	return s
}

// Validate checks settings an owner sends.
func (s TwinSettings) Validate() error {
	okReplay := s.PeopleReplay == TwinPeopleOff || ValidTwinPeople(s.PeopleReplay)
	okDisplay := s.DisplayPeople == TwinPeopleOff || s.DisplayPeople == TwinPeopleCounts || s.DisplayPeople == TwinPeopleTracks
	if !okReplay || !okDisplay || s.PeopleReplayDays < 1 || s.PeopleReplayDays > 90 {
		return ErrInvalid
	}
	return nil
}

// TwinPeopleFor is the people mode a principal gets: the one asked for, capped by the settings, the role and (for a
// wall display) the display cap. live keeps anonymous headcounts even when the settings say off.
func TwinPeopleFor(p Principal, want string, s TwinSettings, display, live bool) string {
	if !ValidTwinPeople(want) {
		want = TwinPeopleCounts
	}
	limit := s.PeopleReplay
	if display {
		limit = twinMin(limit, s.DisplayPeople)
	}
	if display || !(p.Role == "owner" || p.Role == "admin" || p.Role == "operator") {
		limit = twinMin(limit, TwinPeopleTracks)
	}
	if live && twinRank(limit) < twinRank(TwinPeopleCounts) {
		limit = TwinPeopleCounts
	}
	return twinMin(want, limit)
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

// Replay (phase P3): the history of one site between two times, as a keyframe at `from` plus changes.

const (
	TwinReplayMaxWindow  = 7 * 24 * time.Hour
	TwinReplayMaxBuckets = 288
	TwinReplayMaxDevices = 1000
	TwinReplayMaxDeltas  = 20000
	TwinReplayMaxAlerts  = 1000
	TwinTimelineMarkers  = 1000
	// TwinReplayMaxCells caps devices x buckets of one response's environment series (about 4 MB of JSON), checked
	// before anything is read: a wider window answers ErrTooDense with a shorter end.
	TwinReplayMaxCells = 150000
	// TwinReplayTimeout bounds one timeline or replay request as a whole (every statement also has 5 s); one that
	// runs out answers ErrTooDense (ask for less).
	TwinReplayTimeout = 8 * time.Second
	// TwinReplayConcurrency is how many timeline / replay requests of one workspace run at once; more answer
	// ErrRateLimited.
	TwinReplayConcurrency = 2
	// TwinRollupSec is the stored bucket (core.sample_rollup); replay buckets are multiples of it.
	TwinRollupSec = 300
)

// TwinReplayBucket picks the bucket for a window: the smallest of 5, 15, 60 minutes, 3 and 6 hours that keeps it
// within TwinReplayMaxBuckets, or asked (a multiple of 5 minutes) when that fits.
func TwinReplayBucket(window time.Duration, asked int) (int, error) {
	if window <= 0 || window > TwinReplayMaxWindow {
		return 0, ErrInvalid
	}
	fits := func(sec int) bool {
		return int((window+time.Duration(sec)*time.Second-1)/(time.Duration(sec)*time.Second)) <= TwinReplayMaxBuckets
	}
	if asked > 0 {
		if asked%TwinRollupSec != 0 || asked > 6*3600 || !fits(asked) {
			return 0, ErrInvalid
		}
		return asked, nil
	}
	for _, sec := range []int{300, 900, 3600, 3 * 3600, 6 * 3600} {
		if fits(sec) {
			return sec, nil
		}
	}
	return 0, ErrInvalid
}

type TwinTimelineAvailable struct {
	EnvFrom     *time.Time `json:"env_from"`
	PeopleFrom  *time.Time `json:"people_from"`
	MarkersFrom *time.Time `json:"markers_from"`
	Now         time.Time  `json:"now"`
}

type TwinMarker struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"` // sos | hazard | alert
	Severity  string    `json:"severity"`
	AlertID   string    `json:"alert_id"`
	GatewayID string    `json:"gateway_id,omitempty"`
	DeviceID  string    `json:"device_id,omitempty"`
	Label     string    `json:"label"`
}

type TwinDensity struct {
	BucketSec int              `json:"bucket_sec"`
	From      time.Time        `json:"from"`
	Lanes     map[string][]int `json:"lanes"` // alerts | door | zone, one count per bucket
}

type TwinTimeline struct {
	SiteID     string                `json:"site_id"`
	From       time.Time             `json:"from"`
	To         time.Time             `json:"to"`
	PeopleMode string                `json:"people_mode"`
	Available  TwinTimelineAvailable `json:"available"`
	Markers    []TwinMarker          `json:"markers"`
	Density    TwinDensity           `json:"density"`
}

// TwinSeries is one device's replay values, one per bucket (null where nothing was reported).
type TwinSeries struct {
	T      []*float64 `json:"t"`
	H      []*float64 `json:"h"`
	Motion []*int     `json:"motion"`
	Door   []*int     `json:"door"`
}

type TwinReplayEnv struct {
	Buckets int                   `json:"buckets"`
	Series  map[string]TwinSeries `json:"series"` // by device id (placements)
}

// TwinReplayPeople: counts mode sends per-gateway headcounts and ±1 changes, so no identity leaves the server;
// tracks and named send one id per person and the gateway each change moved them to (null: not on this site).
type TwinReplayPeople struct {
	Mode string `json:"mode"`
	// counts
	KeyCounts   map[string]int `json:"key_counts,omitempty"`
	CountDeltas [][3]any       `json:"count_deltas,omitempty"` // [at_ms, gateway_id, +1|-1]
	// tracks | named
	Key    map[string]*string `json:"key,omitempty"`
	Names  map[string]string  `json:"names,omitempty"`
	Deltas [][3]any           `json:"deltas,omitempty"` // [at_ms, pid, gateway_id|null]
}

type TwinReplayAlert struct {
	ID         string     `json:"id"`
	Severity   string     `json:"severity"`
	Event      string     `json:"event"`
	SOS        bool       `json:"sos"`
	Hazard     bool       `json:"hazard"`
	GatewayID  string     `json:"gateway_id"`
	DeviceID   string     `json:"device_id,omitempty"`
	Title      string     `json:"title"`
	OpenedAt   time.Time  `json:"opened_at"`
	AckedAt    *time.Time `json:"acked_at,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

type TwinReplayAlerts struct {
	Key   []string          `json:"key"` // open at `from`
	Items []TwinReplayAlert `json:"items"`
}

type TwinReplay struct {
	SiteID      string            `json:"site_id"`
	From        time.Time         `json:"from"`
	To          time.Time         `json:"to"`
	BucketSec   int               `json:"bucket_sec"`
	GeneratedAt time.Time         `json:"generated_at"`
	PeopleMode  string            `json:"people_mode"`
	Env         *TwinReplayEnv    `json:"env,omitempty"`
	People      *TwinReplayPeople `json:"people,omitempty"`
	Alerts      *TwinReplayAlerts `json:"alerts,omitempty"`
}

// TwinTrailPoint is one zone change of one named person.
type TwinTrailPoint struct {
	At        time.Time `json:"at"`
	GatewayID *string   `json:"gateway_id"`
}

// ErrTooDense: a replay window holds more than one response may carry (people changes, alerts, devices x buckets) or
// takes longer than TwinReplayTimeout to read; HintTo is a shorter end to ask for.
type ErrTooDense struct{ HintTo time.Time }

func (e ErrTooDense) Error() string { return "too_dense" }

// TwinReplayQuery is one replay request.
type TwinReplayQuery struct {
	SiteID   string
	From, To time.Time
	Bucket   int
	Layers   []string // env | people | alerts (empty: all)
	People   string
}
