// Package tuya turns a Tuya device's data-point (DP) specification into what Aether already understands. A
// Tuya device reports and accepts numbered data points ({"1":true,"19":1234}); what each one means comes from
// its product's specification, fetched once from the Tuya cloud when the local keys are imported. Aether keeps
// the specification normalised (DP) and translates it into the same exposes shape Zigbee2MQTT devices publish
// (zigbee2mqtt.Feature), plus a dp map from each property back to its data point. Validation, toggles, command
// confirmation and the generic ingest then work on Tuya devices unchanged. See docs/platform/aether-edge.md.
package tuya

import (
	"aether/backend/internal/domain"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
)

// DP is one data point of a device, normalised from either Tuya specification format. Min, Max and Step are raw
// (device) units; the real value is raw / 10^Scale.
type DP struct {
	ID     int      `json:"id"`
	Code   string   `json:"code"`
	Type   string   `json:"type"`   // bool | value | enum | string | bitmap | raw | json
	Access string   `json:"access"` // ro | rw | wr
	Min    *int64   `json:"min,omitempty"`
	Max    *int64   `json:"max,omitempty"`
	Step   *int64   `json:"step,omitempty"`
	Scale  int      `json:"scale,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Range  []string `json:"range,omitempty"`
}

// MaxDPs bounds one device's specification; real devices have a few dozen at most.
const MaxDPs = 128

var codePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// Readable and Writable follow the access mode.
func (d DP) Readable() bool { return d.Access == "ro" || d.Access == "rw" }
func (d DP) Writable() bool { return d.Access == "rw" || d.Access == "wr" }

// ParseModel reads GET /v2.0/cloud/thing/{id}/model: {"result":{"model":"<JSON string>"}}, where the model has
// services[].properties[] with abilityId (the DP id), code, accessMode and typeSpec. The bare model (string or
// object) is accepted too.
func ParseModel(b []byte) ([]DP, error) {
	var envelope struct {
		Result *struct {
			Model json.RawMessage `json:"model"`
		} `json:"result"`
		Model json.RawMessage `json:"model"`
	}
	model := json.RawMessage(b)
	if json.Unmarshal(b, &envelope) == nil {
		switch {
		case envelope.Result != nil && len(envelope.Result.Model) > 0:
			model = envelope.Result.Model
		case len(envelope.Model) > 0:
			model = envelope.Model
		}
	}
	// The model is usually a JSON document inside a string.
	var inner string
	if json.Unmarshal(model, &inner) == nil {
		model = json.RawMessage(inner)
	}
	var m struct {
		Services []struct {
			Properties []struct {
				AbilityID  int             `json:"abilityId"`
				Code       string          `json:"code"`
				AccessMode string          `json:"accessMode"`
				TypeSpec   json.RawMessage `json:"typeSpec"`
			} `json:"properties"`
		} `json:"services"`
	}
	if e := json.Unmarshal(model, &m); e != nil {
		return nil, domain.ErrInvalid
	}
	out := []DP{}
	for _, s := range m.Services {
		for _, p := range s.Properties {
			d, ok := dpFrom(p.AbilityID, p.Code, strings.ToLower(p.AccessMode), p.TypeSpec, "")
			if ok {
				out = append(out, d)
			}
		}
	}
	return normalise(out)
}

// ParseSpecifications reads GET /v1.1/devices/{id}/specifications: {"result":{"category":"kg","functions":[...],
// "status":[...]}}. A DP listed in functions is writable, in status readable. Each entry's values is a JSON
// document inside a string ({"min":0,"max":1000,"scale":0,"step":1,"unit":"%"} or {"range":[...]}). The
// category is returned too.
func ParseSpecifications(b []byte) ([]DP, string, error) {
	type entry struct {
		Code   string          `json:"code"`
		DPID   int             `json:"dp_id"`
		Type   string          `json:"type"`
		Values json.RawMessage `json:"values"`
	}
	type spec struct {
		Category  string  `json:"category"`
		Functions []entry `json:"functions"`
		Status    []entry `json:"status"`
	}
	var envelope struct {
		Result *spec `json:"result"`
	}
	var s spec
	if json.Unmarshal(b, &envelope) == nil && envelope.Result != nil {
		s = *envelope.Result
	} else if e := json.Unmarshal(b, &s); e != nil {
		return nil, "", domain.ErrInvalid
	}
	access := map[int]string{}
	byID := map[int]entry{}
	for _, f := range s.Functions {
		access[f.DPID] = "wr"
		byID[f.DPID] = f
	}
	for _, st := range s.Status {
		if access[st.DPID] == "wr" {
			access[st.DPID] = "rw"
		} else {
			access[st.DPID] = "ro"
		}
		if _, ok := byID[st.DPID]; !ok {
			byID[st.DPID] = st
		}
	}
	out := []DP{}
	for id, en := range byID {
		values := en.Values
		var inner string
		if json.Unmarshal(values, &inner) == nil {
			values = json.RawMessage(inner)
		}
		if d, ok := dpFrom(id, en.Code, access[id], values, en.Type); ok {
			out = append(out, d)
		}
	}
	dps, e := normalise(out)
	return dps, clipCode(s.Category), e
}

// dpFrom builds a DP from a v2.0 typeSpec (legacy empty) or a v1.1 values document plus its type name.
func dpFrom(id int, code, access string, spec json.RawMessage, legacyType string) (DP, bool) {
	if id < 1 || id > 255 || !codePattern.MatchString(code) {
		return DP{}, false
	}
	if access != "ro" && access != "rw" && access != "wr" {
		return DP{}, false
	}
	var ts struct {
		Type  string          `json:"type"`
		Min   *float64        `json:"min"`
		Max   *float64        `json:"max"`
		Step  *float64        `json:"step"`
		Scale *float64        `json:"scale"`
		Unit  string          `json:"unit"`
		Range []string        `json:"range"`
		Label json.RawMessage `json:"label"`
	}
	if len(spec) > 0 {
		_ = json.Unmarshal(spec, &ts)
	}
	typ := strings.ToLower(ts.Type)
	if legacyType != "" {
		typ = map[string]string{"boolean": "bool", "integer": "value", "enum": "enum", "string": "string", "bitmap": "bitmap", "raw": "raw", "json": "json"}[strings.ToLower(legacyType)]
	}
	d := DP{ID: id, Code: code, Type: typ, Access: access, Unit: clipCode(ts.Unit)}
	switch typ {
	case "bool", "string", "raw", "json", "bitmap":
	case "value":
		d.Min, d.Max, d.Step = toInt(ts.Min), toInt(ts.Max), toInt(ts.Step)
		if ts.Scale != nil && *ts.Scale >= 0 && *ts.Scale <= 6 {
			d.Scale = int(*ts.Scale)
		}
		if d.Step != nil && *d.Step <= 0 {
			d.Step = nil
		}
		if d.Min != nil && d.Max != nil && *d.Min > *d.Max {
			d.Min, d.Max = nil, nil
		}
	case "enum":
		for _, v := range ts.Range {
			if len(v) > 0 && len(v) <= 64 && len(d.Range) < 64 {
				d.Range = append(d.Range, v)
			}
		}
		if len(d.Range) == 0 {
			return DP{}, false
		}
	default:
		return DP{}, false
	}
	return d, true
}

func toInt(f *float64) *int64 {
	if f == nil || math.IsNaN(*f) || math.IsInf(*f, 0) || math.Abs(*f) > 1e15 {
		return nil
	}
	v := int64(math.Round(*f))
	return &v
}

// normalise sorts by DP id, drops duplicates and bounds the list.
func normalise(dps []DP) ([]DP, error) {
	sort.Slice(dps, func(i, j int) bool { return dps[i].ID < dps[j].ID })
	out := []DP{}
	seen := map[int]bool{}
	codes := map[string]bool{}
	for _, d := range dps {
		if seen[d.ID] || codes[d.Code] {
			continue
		}
		seen[d.ID], codes[d.Code] = true, true
		out = append(out, d)
		if len(out) == MaxDPs {
			break
		}
	}
	if len(out) == 0 {
		return nil, domain.ErrInvalid
	}
	return out, nil
}

func clipCode(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	if len(s) > 32 {
		return s[:32]
	}
	return s
}

// Validate checks a specification that crosses a trust boundary (a repository caller, the import job) before it is
// stored: every data point well formed, no duplicate ids or codes, at most MaxDPs. It never repairs; anything off
// is domain.ErrInvalid.
func Validate(dps []DP) error {
	if len(dps) > MaxDPs {
		return domain.ErrInvalid
	}
	ids, codes := map[int]bool{}, map[string]bool{}
	for _, d := range dps {
		if d.ID < 1 || d.ID > 255 || !codePattern.MatchString(d.Code) || ids[d.ID] || codes[d.Code] {
			return domain.ErrInvalid
		}
		ids[d.ID], codes[d.Code] = true, true
		if d.Access != "ro" && d.Access != "rw" && d.Access != "wr" || len(d.Unit) > 32 || strings.ContainsRune(d.Unit, 0) {
			return domain.ErrInvalid
		}
		if d.Scale < 0 || d.Scale > 6 {
			return domain.ErrInvalid
		}
		const bound = int64(1e15)
		for _, v := range []*int64{d.Min, d.Max, d.Step} {
			if v != nil && (*v > bound || *v < -bound) {
				return domain.ErrInvalid
			}
		}
		if d.Min != nil && d.Max != nil && *d.Min > *d.Max || d.Step != nil && *d.Step <= 0 {
			return domain.ErrInvalid
		}
		switch d.Type {
		case "bool", "value", "string", "bitmap", "raw", "json":
			if len(d.Range) > 0 {
				return domain.ErrInvalid
			}
		case "enum":
			if len(d.Range) == 0 || len(d.Range) > 64 {
				return domain.ErrInvalid
			}
			seen := map[string]bool{}
			for _, v := range d.Range {
				if v == "" || len(v) > 64 || strings.ContainsRune(v, 0) || seen[v] {
					return domain.ErrInvalid
				}
				seen[v] = true
			}
		default:
			return domain.ErrInvalid
		}
	}
	return nil
}
