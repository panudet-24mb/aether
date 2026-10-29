package studio

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Item struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id,omitempty"`
	Kind       string          `json:"kind"`
	Name       string          `json:"name"`
	Brand      string          `json:"brand"`
	Model      string          `json:"model"`
	Version    int             `json:"version"`
	Visibility string          `json:"visibility"`
	Definition json.RawMessage `json:"definition"`
	Revision   int             `json:"revision"`
	Official   bool            `json:"official"`
}
type Definition struct {
	DecodeCode string  `json:"decode_code,omitempty"`
	HTML       string  `json:"html,omitempty"`
	CSS        string  `json:"css,omitempty"`
	Code       string  `json:"code,omitempty"`
	Panels     []Panel `json:"panels,omitempty"`
	// Kinds lists the reading kinds a widget is meant for (environment, motion, tamper, beacon, …);
	// empty means "any". It only drives filtering in the editor, never access.
	Kinds []string `json:"kinds,omitempty"`
	// ProjectID scopes a dashboard's device picker to one project by default; panels may still reference
	// devices of other projects in the same workspace. Empty means "all projects".
	ProjectID string `json:"project_id,omitempty"`
	// Groups are a dashboard's named sections, in display order; a panel joins one by its GroupID. They only
	// arrange the page: whether a viewer has a group collapsed is that viewer's own choice, never stored here.
	Groups []Group `json:"groups,omitempty"`
}

// Group is a named section of a dashboard (Definition.Groups). A group with a ParentID sits inside that group;
// siblings keep their order in Definition.Groups. Width is the columns (1-4) it takes of its level and Height its
// rows of panels (1-4), like a panel's; zero means all columns, and a height that fits the content.
type Group struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// MaxGroups bounds the sections of one dashboard (as many as it has panels); MaxGroupDepth bounds their nesting
// (a top-level group is depth 1), so headers stay readable.
const (
	MaxGroups     = 16
	MaxGroupDepth = 3
)

// validGroups reports a dashboard's groups: unique ids, names, parents that exist, no cycle, at most MaxGroupDepth
// deep.
func validGroups(groups []Group) bool {
	if len(groups) > MaxGroups {
		return false
	}
	parent := map[string]string{}
	for _, g := range groups {
		if _, dup := parent[g.ID]; !Text(g.ID, 64) || dup || !Text(g.Name, 128) || g.Width < 0 || g.Width > 4 || g.Height < 0 || g.Height > 4 {
			return false
		}
		parent[g.ID] = g.ParentID
	}
	for _, g := range groups {
		depth := 1
		for at := g.ParentID; at != ""; at = parent[at] {
			if _, known := parent[at]; !known || at == g.ID {
				return false // a parent that does not exist, or the group is its own ancestor
			}
			if depth++; depth > MaxGroupDepth {
				return false
			}
		}
	}
	return true
}
type Panel struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	WidgetID   string `json:"widget_id"`
	DecoderID  string `json:"decoder_id,omitempty"`
	GatewayID  string `json:"gateway_id"`
	ExternalID string `json:"external_id"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	// Devices are the registered devices (core.devices ids) of a controls panel (WidgetID ControlsWidget).
	Devices []string `json:"devices,omitempty"`
	// GroupID is the dashboard group (Definition.Groups) this panel sits in; empty: above every group.
	GroupID string `json:"group_id,omitempty"`
}

// ControlsWidget is the first-party controls panel: switches for the chosen devices, drawn natively by the app
// (never in the widget sandbox, which runs no script in the browser) through the ordinary command API; a wall
// display gets a static, read-only rendering of their state.
const ControlsWidget = "aether:controls"

// MaxControls bounds the devices of one controls panel.
const MaxControls = 12

// validControls reports a controls panel's device list: 1..MaxControls distinct uuids.
func validControls(ids []string) bool {
	if len(ids) == 0 || len(ids) > MaxControls {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if _, e := uuid.Parse(id); e != nil || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// sourcePattern is a panel's device: a BLE MAC (12 hex digits), a Zigbee IEEE address (0x + 16 hex digits) or a Tuya
// device id (16 to 32 lower-case letters and digits), as each ingest stores it.
var sourcePattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{12}|0x[0-9a-fA-F]{16}|[a-z0-9]{16,32})$`)

// ValidSource reports a panel's device id.
func ValidSource(s string) bool { return sourcePattern.MatchString(s) }

func Text(s string, max int) bool {
	return len(strings.TrimSpace(s)) > 0 && len(s) <= max && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func Validate(i Item) error {
	if !Text(i.Name, 128) || !Text(i.Brand, 64) || !Text(i.Model, 64) || i.Version < 1 || i.Version > 10000 || len(i.Definition) > 65536 || (i.Visibility != "private" && i.Visibility != "community") {
		return fmt.Errorf("invalid catalog metadata")
	}
	var d Definition
	if json.Unmarshal(i.Definition, &d) != nil {
		return fmt.Errorf("invalid definition")
	}
	switch i.Kind {
	case "widget":
		if len(d.HTML) == 0 || len(d.HTML) > 24000 || len(d.CSS) > 12000 || len(d.Code) > 16000 || len(d.DecodeCode) > 16000 || len(d.Kinds) > 8 {
			return fmt.Errorf("widget limits exceeded")
		}
	case "decoder":
		if len(d.Code) == 0 || len(d.Code) > 16000 || len(d.DecodeCode) > 16000 {
			return fmt.Errorf("decoder required")
		}
	case "dashboard":
		if i.Visibility != "private" || len(d.Panels) > 16 {
			return fmt.Errorf("dashboard limit is 16 panels")
		}
		if d.ProjectID != "" {
			if _, e := uuid.Parse(d.ProjectID); e != nil {
				return fmt.Errorf("invalid project")
			}
		}
		if len(d.Groups) > MaxGroups {
			return fmt.Errorf("dashboard limit is %d groups", MaxGroups)
		}
		if !validGroups(d.Groups) {
			return fmt.Errorf("invalid groups")
		}
		groups := map[string]bool{}
		for _, g := range d.Groups {
			groups[g.ID] = true
		}
		seen := map[string]bool{}
		for _, p := range d.Panels {
			if p.GroupID != "" && !groups[p.GroupID] {
				return fmt.Errorf("panel in an unknown group")
			}
			if p.WidgetID == ControlsWidget {
				if !validControls(p.Devices) || p.GatewayID != "" || p.ExternalID != "" {
					return fmt.Errorf("invalid controls panel")
				}
			} else if _, e := uuid.Parse(p.GatewayID); e != nil || !ValidSource(p.ExternalID) || len(p.Devices) > 0 {
				return fmt.Errorf("invalid source")
			}
			if !Text(p.ID, 64) || seen[p.ID] || !Text(p.Title, 128) || p.Width < 1 || p.Width > 4 || p.Height < 1 || p.Height > 4 || !Text(p.WidgetID, 80) || (p.WidgetID != ControlsWidget && !Text(p.GatewayID, 40)) {
				return fmt.Errorf("invalid panel")
			}
			seen[p.ID] = true
		}
	default:
		return fmt.Errorf("invalid kind")
	}
	return nil
}
