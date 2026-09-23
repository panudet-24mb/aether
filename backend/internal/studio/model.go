package studio

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
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
}

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
		seen := map[string]bool{}
		for _, p := range d.Panels {
			mac, err := hex.DecodeString(p.ExternalID)
			if _, e := uuid.Parse(p.GatewayID); e != nil || err != nil || len(mac) != 6 {
				return fmt.Errorf("invalid source")
			}
			if !Text(p.ID, 64) || seen[p.ID] || !Text(p.Title, 128) || p.Width < 1 || p.Width > 4 || p.Height < 1 || p.Height > 4 || !Text(p.WidgetID, 80) || !Text(p.GatewayID, 40) || len(p.ExternalID) != 12 {
				return fmt.Errorf("invalid panel")
			}
			seen[p.ID] = true
		}
	default:
		return fmt.Errorf("invalid kind")
	}
	return nil
}
