package domain

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Display links (docs/platform/display.md): a wall TV that shows a rotating, read-only view of the workspace and
// takes over the screen on an SOS or hazard alert. A display is its own principal, never a member.

// DisplayViewKinds are the views a playlist may rotate through.
var DisplayViewKinds = []string{"overview", "alerts", "floorplan", "devices", "presence", "studio"}

const (
	MaxDisplays        = 50
	MaxDisplayViews    = 12
	MinDisplaySeconds  = 10
	MaxDisplaySeconds  = 600
	DisplayPairingTTL  = 10 * time.Minute
	DisplayTokenPrefix = "dsp_"
)

// DisplayView is one step of a display's playlist. Ref names the studio dashboard (studio) or the site
// (floorplan, optional: the first site otherwise).
type DisplayView struct {
	Kind    string `json:"kind"`
	Seconds int    `json:"seconds"`
	Ref     string `json:"ref,omitempty"`
}

// ValidPlaylist bounds a playlist: known kinds, a sensible dwell time, a studio view names its dashboard.
func ValidPlaylist(views []DisplayView) bool {
	if len(views) == 0 || len(views) > MaxDisplayViews {
		return false
	}
	for _, v := range views {
		if !in(DisplayViewKinds, v.Kind) || v.Seconds < MinDisplaySeconds || v.Seconds > MaxDisplaySeconds {
			return false
		}
		switch {
		case v.Kind == "studio" && !uuidShape.MatchString(v.Ref):
			return false
		case v.Kind == "floorplan" && v.Ref != "" && !uuidShape.MatchString(v.Ref):
			return false
		case v.Kind != "studio" && v.Kind != "floorplan" && v.Ref != "":
			return false
		}
	}
	return true
}

// DefaultPlaylist is what a new display shows until somebody edits it.
func DefaultPlaylist() []DisplayView {
	return []DisplayView{{Kind: "overview", Seconds: 30}, {Kind: "alerts", Seconds: 20}, {Kind: "floorplan", Seconds: 40}, {Kind: "devices", Seconds: 20}}
}

// Display is what an owner or admin manages. The pairing code and the token are never part of it.
type Display struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	ProjectIDs       []string      `json:"project_ids"`
	Playlist         []DisplayView `json:"playlist"`
	ShowNames        bool          `json:"show_names"`
	AllowAck         bool          `json:"allow_ack"`
	Paired           bool          `json:"paired"`
	PairingExpiresAt *time.Time    `json:"pairing_expires_at"`
	PairedAt         *time.Time    `json:"paired_at"`
	LastSeenAt       *time.Time    `json:"last_seen_at"`
	LastIP           *string       `json:"last_ip"`
	RevokedAt        *time.Time    `json:"revoked_at"`
	CreatedAt        time.Time     `json:"created_at"`
}

// DisplaySettings is a create or update request.
type DisplaySettings struct {
	Name       string
	ProjectIDs []string
	Playlist   []DisplayView
	ShowNames  bool
	AllowAck   bool
}

// DisplayPairing is shown once, to the member who created or re-paired a display.
type DisplayPairing struct {
	Display   Display   `json:"display"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DisplaySession is the authenticated TV: which display, of which workspace, and what it may do.
type DisplaySession struct {
	ID         string        `json:"id"`
	TenantID   string        `json:"-"`
	TenantName string        `json:"tenant_name"`
	Name       string        `json:"name"`
	ProjectIDs []string      `json:"project_ids"`
	Playlist   []DisplayView `json:"playlist"`
	ShowNames  bool          `json:"show_names"`
	AllowAck   bool          `json:"allow_ack"`
}

// Principal is how a display reaches repository methods: no user, the display's workspace. The repository
// recognises the display from the context (WithDisplay), never from this value.
func (d DisplaySession) Principal() Principal {
	return Principal{TenantID: d.TenantID, Role: "display"}
}

// DisplayCtx marks a context as a display's: repository transactions then carry app.display_id instead of a user,
// take their project scope from the display, and are READ ONLY unless Write is set.
type DisplayCtx struct {
	ID    string
	Write bool
}

type displayKey struct{}

func WithDisplay(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, displayKey{}, DisplayCtx{ID: id})
}

// WithDisplayWrite allows the next transactions of a display context to write (its access log, an acknowledgement).
func WithDisplayWrite(ctx context.Context) context.Context {
	d, ok := DisplayFrom(ctx)
	if !ok {
		return ctx
	}
	d.Write = true
	return context.WithValue(ctx, displayKey{}, d)
}

func DisplayFrom(ctx context.Context) (DisplayCtx, bool) {
	d, ok := ctx.Value(displayKey{}).(DisplayCtx)
	return d, ok && d.ID != ""
}

// DisplayBoard is everything the rotating views of a TV draw, narrowed to the display's projects. Wearer names
// are replaced by a neutral label unless the owner allowed names on this display.
type DisplayBoard struct {
	ServerTime time.Time         `json:"server_time"`
	Projects   []DisplayProject  `json:"projects"`
	Gateways   []DisplayGateway  `json:"gateways"`
	Devices    []DisplayDevice   `json:"devices"`
	Alerts     []DisplayAlert    `json:"alerts"`
	Counts     DisplayCounts     `json:"counts"`
	Presence   []DisplayPresence `json:"presence"`
}

type DisplayProject struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type DisplayGateway struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Model     string     `json:"model"`
	ProjectID *string    `json:"project_id"`
	LastSeen  *time.Time `json:"last_seen"`
	Online    bool       `json:"online"`
}

type DisplayDevice struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	ExternalID    string             `json:"external_id"`
	ProfileID     string             `json:"profile_id"`
	GatewayID     string             `json:"gateway_id"`
	Wearable      bool               `json:"wearable"`
	ZoneGatewayID *string            `json:"zone_gateway_id"`
	LastSeen      *time.Time         `json:"last_seen"`
	Online        bool               `json:"online"`
	Kind          string             `json:"kind,omitempty"`
	Reading       map[string]float64 `json:"reading"`
}

// DisplayAlert is the part of an alert a TV shows: no member ids and no operator notes.
type DisplayAlert struct {
	ID          string     `json:"id"`
	GatewayID   string     `json:"gateway_id"`
	ExternalID  string     `json:"external_id"`
	DeviceName  string     `json:"device_name"`
	EventType   string     `json:"event_type"`
	Severity    string     `json:"severity"`
	Title       string     `json:"title"`
	Status      string     `json:"status"`
	OpenedAt    time.Time  `json:"opened_at"`
	AckedAt     *time.Time `json:"acked_at"`
	Takeover    bool       `json:"takeover"`
	GatewayName string     `json:"gateway_name"`
}

type DisplayCounts struct {
	Open         int `json:"open"`
	Acknowledged int `json:"acknowledged"`
	Critical     int `json:"critical"`
	Gateways     int `json:"gateways"`
	GatewaysUp   int `json:"gateways_online"`
	Devices      int `json:"devices"`
	DevicesUp    int `json:"devices_online"`
	LowBattery   int `json:"low_battery"`
}

// DisplayPresence is how many worn tags are in the zone of one gateway (names only when allowed).
type DisplayPresence struct {
	GatewayID   string   `json:"gateway_id"`
	GatewayName string   `json:"gateway_name"`
	Count       int      `json:"count"`
	Names       []string `json:"names,omitempty"`
}

// DisplayAckable: the only alerts a TV may acknowledge (when the owner allowed it) are the ones that take over its
// screen, whatever their state: an SOS (a critical button press) or a hazard.
func DisplayAckable(eventType, severity string) bool {
	return (eventType == EventButton && severity == "critical") || eventType == EventHazard
}

// WornProfileIDs are the catalog profiles carried by people (wearables and emergency buttons). A tag registered with
// one of them, or set to roaming, is personal on a wall display.
func WornProfileIDs() []string {
	out := []string{}
	for _, p := range DeviceProfiles {
		if p.Wearable || p.Button {
			out = append(out, p.ID)
		}
	}
	return out
}

// DisplayWearerLabel is what a TV shows instead of a personal tag's name: the last four characters of its MAC.
func DisplayWearerLabel(external string) string {
	tail := strings.ToUpper(strings.ReplaceAll(external, ":", ""))
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return "ผู้สวมใส่ · " + tail
}

// DisplayMaskedTitle names what happened without naming who.
func DisplayMaskedTitle(eventType string) string {
	switch eventType {
	case EventButton:
		return "กดปุ่มฉุกเฉิน"
	case EventHazard:
		return "ตรวจพบอันตราย"
	case EventZone:
		return "เปลี่ยนพื้นที่"
	case EventTamper:
		return "อุปกรณ์ถูกถอด"
	case EventMotion:
		return "ตรวจพบการเคลื่อนไหว"
	case EventOffline:
		return "อุปกรณ์ขาดการติดต่อ"
	default:
		return "แจ้งเตือนจากอุปกรณ์"
	}
}

// displayTagKey is random per process: a TV that may not show names gets DisplayTag(mac) instead of the MAC. It is
// stable while the API runs, so a board's alerts and devices still match one another, and says nothing about the MAC.
var displayTagKey = func() []byte {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return b
}()

// DisplayTag replaces a MAC on a display that may not show names.
func DisplayTag(tenant, external string) string {
	m := hmac.New(sha256.New, displayTagKey)
	m.Write([]byte(tenant + "|" + strings.ToLower(external)))
	return "t" + hex.EncodeToString(m.Sum(nil))[:15]
}

// DisplayTakeover reports whether an alert takes the whole screen: an open SOS (a critical button press, see
// Alert.SOS) or an open hazard (smoke, gas, CO), the two kinds that also bypass shadow mode.
func DisplayTakeover(eventType, severity, status string) bool {
	return status == "open" && ((eventType == EventButton && severity == "critical") || eventType == EventHazard)
}
