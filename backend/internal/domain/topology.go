package domain

import (
	"math"
	"regexp"
	"time"
)

// The connect view's canvas layout, shared by everyone in a workspace (migration 00041).

// LayoutViewConnect is the only view today; the key leaves room for others without a schema change.
const LayoutViewConnect = "connect"

// Limits of one save. A workspace's layout is naturally bounded by its inventory: every stored node must be a
// gateway or a device the server knows.
const (
	MaxLayoutEntries = 2000
	MaxLayoutCoord   = 1_000_000
	// MaxLayoutRows caps the stored rows of one view (every member's nodes together); a read returns at most this
	// many, so no live node is ever cut off. Orphans are pruned before the cap is checked.
	MaxLayoutRows = 5000
	// MaxLayoutBody fits the largest valid save: 2000 entries of at most ~105 bytes (a 68-character node id and two
	// coordinates with two decimals), below the server's 256 KiB request limit.
	MaxLayoutBody = 240 * 1024
)

var (
	layoutViewRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	layoutNodeRe = regexp.MustCompile(`^(broker|gw:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|dev:[a-z0-9][a-z0-9._:-]{0,63})$`)
)

func ValidLayoutView(v string) bool  { return layoutViewRe.MatchString(v) }
func ValidLayoutNode(id string) bool { return layoutNodeRe.MatchString(id) }

// CanvasPoint is a node position in canvas coordinates.
type CanvasPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (p CanvasPoint) Valid() bool {
	return !math.IsNaN(p.X) && !math.IsNaN(p.Y) && math.Abs(p.X) <= MaxLayoutCoord && math.Abs(p.Y) <= MaxLayoutCoord
}

// Layout is what the caller may see of one view: nodes of gateways outside their projects are left out.
type Layout struct {
	View      string                 `json:"view"`
	Version   int64                  `json:"version"`
	Positions map[string]CanvasPoint `json:"positions"`
	UpdatedBy *string                `json:"updated_by"`
	UpdatedAt *time.Time             `json:"updated_at"`
}

// LayoutChange is one save: positions to write and nodes to forget, applied only when BaseVersion is still
// current. A save never touches a node it does not name, so a member working under a project filter (or restricted
// to some projects) cannot disturb anyone else's part of the board.
type LayoutChange struct {
	View        string
	BaseVersion int64
	Positions   map[string]CanvasPoint
	Remove      []string
}

// LayoutSaved reports the new version and the nodes the server did not store (unknown to it, or outside the
// caller's projects).
type LayoutSaved struct {
	Layout  Layout   `json:"layout"`
	Ignored []string `json:"ignored"`
}

// TopologyFrame is a named rectangle on the canvas that groups the nodes inside it (migration 00052). It only
// arranges the picture: membership is where a node sits, so nothing is stored per node.
type TopologyFrame struct {
	ID        string    `json:"id"`
	View      string    `json:"view"`
	ProjectID *string   `json:"project_id"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	X         float64   `json:"x"`
	Y         float64   `json:"y"`
	W         float64   `json:"w"`
	H         float64   `json:"h"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Frame limits: per view of a workspace, and the side of a frame in canvas units.
const (
	MaxFrames    = 200
	MinFrameSide = 40
	MaxFrameSide = 20000
)

// ValidGeometry reports whether the frame's position and size are finite and within the limits the table checks.
func (f TopologyFrame) ValidGeometry() bool {
	side := func(v float64) bool { return !math.IsNaN(v) && v >= MinFrameSide && v <= MaxFrameSide }
	return CanvasPoint{X: f.X, Y: f.Y}.Valid() && side(f.W) && side(f.H)
}

// EdgeRoute is the bend points a connection on the canvas is drawn through (migration 00053). Edge is the canvas
// edge id: "link:<gateway id>" (gateway to broker) or "adopt:<registration id>" (registration to its gateway).
type EdgeRoute struct {
	Edge   string        `json:"edge"`
	Points []CanvasPoint `json:"points"`
}

// Route limits: bend points of one line, and stored routes per view of a workspace.
const (
	MaxRoutePoints = 16
	MaxRoutes      = 2000
)

var routeEdgeRe = regexp.MustCompile(`^(link|adopt):([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// ParseRouteEdge splits a routable edge id into its kind and the gateway or registration it names.
func ParseRouteEdge(id string) (kind, ref string, ok bool) {
	m := routeEdgeRe.FindStringSubmatch(id)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// LayoutConflict is returned when BaseVersion is stale; it carries the current layout so the client can merge.
type LayoutConflict struct{ Current Layout }

func (LayoutConflict) Error() string { return "layout version conflict" }
func (LayoutConflict) Unwrap() error { return ErrConflict }
