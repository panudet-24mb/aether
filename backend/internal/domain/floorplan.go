package domain

import (
	"math"
	"regexp"
	"strings"
	"time"
)

// Floor plans are drawn in metres with the origin at the top-left corner of the floor; y grows downwards.

type Site struct {
	ID          string     `json:"id"`
	ProjectID   *string    `json:"project_id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Floors      []Floor    `json:"floors"`
}

type Floor struct {
	ID         string           `json:"id"`
	SiteID     string           `json:"site_id"`
	Name       string           `json:"name"`
	Level      int              `json:"level"`
	WidthM     float64          `json:"width_m"`
	DepthM     float64          `json:"depth_m"`
	CeilingM   float64          `json:"ceiling_m"`
	Revision   int              `json:"revision"`
	HasImage   bool             `json:"has_image"`
	UpdatedAt  time.Time        `json:"updated_at"`
	Layout     FloorLayout      `json:"layout"`
	Placements []FloorPlacement `json:"placements"`
}

type FloorPlacement struct {
	AssetKind string  `json:"asset_kind"` // gateway | device
	AssetID   string  `json:"asset_id"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Z         float64 `json:"z"` // mounting height above the floor, metres
}

type Point [2]float64

type FloorWall struct {
	ID        string  `json:"id"`
	Points    []Point `json:"points"`
	Thickness float64 `json:"thickness"`
	Closed    bool    `json:"closed,omitempty"`
}

type FloorZone struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Color  string  `json:"color"`
	Points []Point `json:"points"`
	// GatewayIDs: gateways that cover this zone. A roaming wearable whose current gateway is listed is shown here.
	GatewayIDs []string `json:"gateway_ids,omitempty"`
	Note       string   `json:"note,omitempty"`
}

type FloorItem struct {
	ID   string  `json:"id"`
	Type string  `json:"type"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
	Rot  float64 `json:"rot"`
	Text string  `json:"text,omitempty"`
}

type FloorBackground struct {
	Opacity float64 `json:"opacity"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	WidthM  float64 `json:"width_m"`
	Locked  bool    `json:"locked,omitempty"`
}

type FloorLayout struct {
	Walls      []FloorWall      `json:"walls"`
	Zones      []FloorZone      `json:"zones"`
	Items      []FloorItem      `json:"items"`
	Background *FloorBackground `json:"background,omitempty"`
}

var (
	ZoneKinds      = []string{"room", "corridor", "ward", "restricted", "storage", "outdoor", "other"}
	FloorItemTypes = []string{"door", "window", "stairs", "elevator", "exit", "label", "desk", "bed", "rack", "extinguisher"}
)

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func in(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < -1000 || v > 2000 {
			return false
		}
	}
	return true
}

func shortID(s string) bool {
	return len(s) > 0 && len(s) <= 40 && !strings.ContainsAny(s, " \t\n\"'<>")
}

// Validate bounds every list and number so a layout cannot grow without limit or carry NaN into the renderers.
func (l *FloorLayout) Validate() error {
	if len(l.Walls) > 500 || len(l.Zones) > 200 || len(l.Items) > 500 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	unique := func(id string) bool {
		if !shortID(id) || seen[id] {
			return false
		}
		seen[id] = true
		return true
	}
	for _, w := range l.Walls {
		if !unique(w.ID) || len(w.Points) < 2 || len(w.Points) > 200 || w.Thickness < 0.05 || w.Thickness > 1 {
			return ErrInvalid
		}
		for _, p := range w.Points {
			if !finite(p[0], p[1]) {
				return ErrInvalid
			}
		}
	}
	for _, z := range l.Zones {
		if !unique(z.ID) || len(z.Points) < 3 || len(z.Points) > 100 || len(z.Name) > 128 || len(z.Note) > 500 || !in(ZoneKinds, z.Kind) || !in(ProjectColors, z.Color) || len(z.GatewayIDs) > 20 {
			return ErrInvalid
		}
		for _, p := range z.Points {
			if !finite(p[0], p[1]) {
				return ErrInvalid
			}
		}
		for _, g := range z.GatewayIDs {
			if !uuidShape.MatchString(g) {
				return ErrInvalid
			}
		}
	}
	for _, it := range l.Items {
		if !unique(it.ID) || !in(FloorItemTypes, it.Type) || len(it.Text) > 128 || !finite(it.X, it.Y, it.W, it.H, it.Rot) || it.W < 0 || it.H < 0 || it.W > 200 || it.H > 200 {
			return ErrInvalid
		}
	}
	if b := l.Background; b != nil {
		if !finite(b.X, b.Y, b.WidthM, b.Opacity) || b.Opacity < 0 || b.Opacity > 1 || b.WidthM < 1 || b.WidthM > 1000 {
			return ErrInvalid
		}
	}
	return nil
}

func (p FloorPlacement) Valid() bool {
	return (p.AssetKind == "gateway" || p.AssetKind == "device") && uuidShape.MatchString(p.AssetID) && finite(p.X, p.Y, p.Z) && p.Z >= 0 && p.Z <= 30
}
