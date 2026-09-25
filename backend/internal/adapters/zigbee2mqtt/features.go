package zigbee2mqtt

import (
	"aether/backend/internal/domain"
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"regexp"
)

// A Zigbee2MQTT definition describes what a device can do as `exposes`: generic features (binary, numeric,
// enum, text, list, composite) either at the top level or grouped inside a specific type (light, switch, fan,
// cover, lock, climate). Every feature names the `property` it appears under in the device's state and /set
// payloads, already suffixed with its endpoint (state_l1, state_left, brightness_l2, ...), and an `access`
// bitmask: 1 = published in state, 2 = settable with /set, 4 = readable with /get. See
// zigbee2mqtt.io/guide/usage/exposes.html.

// Access bits of an expose.
const (
	AccessState = 1
	AccessSet   = 2
	AccessGet   = 4
)

// MaxExposes bounds one device's stored exposes document.
const MaxExposes = 64 * 1024

// Feature is one generic feature. Composite features (color_xy {x,y}, color_hs {hue,saturation}, ...) carry
// their parts in Features.
type Feature struct {
	Type        string            `json:"type"`
	Name        string            `json:"name,omitempty"`
	Label       string            `json:"label,omitempty"`
	Property    string            `json:"property"`
	Endpoint    string            `json:"endpoint,omitempty"`
	Access      int               `json:"access"`
	Unit        string            `json:"unit,omitempty"`
	ValueOn     json.RawMessage   `json:"value_on,omitempty"`
	ValueOff    json.RawMessage   `json:"value_off,omitempty"`
	ValueToggle json.RawMessage   `json:"value_toggle,omitempty"`
	ValueMin    *float64          `json:"value_min,omitempty"`
	ValueMax    *float64          `json:"value_max,omitempty"`
	ValueStep   *float64          `json:"value_step,omitempty"`
	Values      []json.RawMessage `json:"values,omitempty"`
	Features    []Feature         `json:"features,omitempty"`
	// Group is the specific type the feature was found in (light, cover, ...), empty at the top level.
	Group string `json:"group,omitempty"`
}

// Settable reports whether /set accepts the feature.
func (f Feature) Settable() bool { return f.Access&AccessSet != 0 }

var specificTypes = map[string]bool{"light": true, "switch": true, "fan": true, "cover": true, "lock": true, "climate": true}

var genericTypes = map[string]bool{"binary": true, "numeric": true, "enum": true, "text": true, "list": true, "composite": true}

// PropertyPattern is what Aether accepts as a property name (Zigbee2MQTT uses snake_case, sometimes with capitals).
var PropertyPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// Features flattens an exposes document into its generic features, recursing into specific types. Several
// features may share one property (a light exposing both color_xy and color_hs under "color").
func Features(exposes json.RawMessage) ([]Feature, error) {
	if len(exposes) == 0 {
		return nil, nil
	}
	var raw []Feature
	if e := json.Unmarshal(exposes, &raw); e != nil {
		return nil, domain.ErrInvalid
	}
	out := []Feature{}
	var walk func(list []Feature, group string, depth int)
	walk = func(list []Feature, group string, depth int) {
		if depth > 4 {
			return
		}
		for _, f := range list {
			switch {
			case specificTypes[f.Type]:
				walk(f.Features, f.Type, depth+1)
			case genericTypes[f.Type] && PropertyPattern.MatchString(f.Property):
				f.Group = group
				out = append(out, f)
			}
		}
	}
	walk(raw, "", 0)
	return out, nil
}

// SettableFeatures are the features Aether can command, in the order the definition lists them, restricted to
// the types it validates (binary, numeric, enum, and composites made only of those).
func SettableFeatures(exposes json.RawMessage) []Feature {
	all, _ := Features(exposes)
	return settable(all)
}

// MaxSettable bounds how many settable features of one device Aether tracks and offers.
const MaxSettable = 128

func settable(all []Feature) []Feature {
	out := []Feature{}
	for _, f := range all {
		if f.Settable() && commandable(f) {
			if len(out) == MaxSettable {
				break
			}
			out = append(out, f)
		}
	}
	return out
}

func commandable(f Feature) bool {
	switch f.Type {
	case "binary", "numeric", "enum":
		return true
	case "composite":
		if len(f.Features) == 0 {
			return false
		}
		for _, sub := range f.Features {
			if !(sub.Type == "binary" || sub.Type == "numeric" || sub.Type == "enum") || !PropertyPattern.MatchString(sub.Property) {
				return false
			}
		}
		return true
	}
	return false
}

func refused(reason string) error { return domain.Because(domain.ErrInvalid, reason) }

// Validate checks a /set value for one property against the device's definition and returns it compacted.
// Refusals are domain.ErrInvalid with a reason: unknown_property, not_settable, unsupported_feature, value.
func Validate(exposes json.RawMessage, property string, value json.RawMessage) (json.RawMessage, error) {
	if !PropertyPattern.MatchString(property) {
		return nil, refused("unknown_property")
	}
	if len(value) == 0 || len(value) > 1024 || !json.Valid(value) {
		return nil, refused("value")
	}
	all, e := Features(exposes)
	if e != nil {
		return nil, e
	}
	var candidates []Feature
	for _, f := range all {
		if f.Property == property {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return nil, refused("unknown_property")
	}
	reason := "not_settable"
	for _, f := range candidates {
		if !f.Settable() {
			continue
		}
		if !commandable(f) {
			reason = "unsupported_feature"
			continue
		}
		if valid(f, value) {
			var buf bytes.Buffer
			if e := json.Compact(&buf, value); e != nil {
				return nil, refused("value")
			}
			return buf.Bytes(), nil
		}
		reason = "value"
	}
	return nil, refused(reason)
}

func valid(f Feature, value json.RawMessage) bool {
	switch f.Type {
	case "binary":
		// value_toggle is deliberately not accepted: a duplicate delivery would flip the device twice, and no
		// report could ever confirm it. A toggle is requested as such and resolved to ON/OFF (see Toggle).
		return sameJSON(value, f.ValueOn) || sameJSON(value, f.ValueOff)
	case "enum":
		for _, v := range f.Values {
			if sameJSON(value, v) {
				return true
			}
		}
		return false
	case "numeric":
		var n float64
		if json.Unmarshal(value, &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) || !isNumber(value) {
			return false
		}
		if f.ValueMin != nil && n < *f.ValueMin || f.ValueMax != nil && n > *f.ValueMax {
			return false
		}
		if f.ValueStep != nil && *f.ValueStep > 0 {
			base := 0.0
			if f.ValueMin != nil {
				base = *f.ValueMin
			}
			steps := (n - base) / *f.ValueStep
			if math.Abs(steps-math.Round(steps)) > 1e-6 {
				return false
			}
		}
		return true
	case "composite":
		var parts map[string]json.RawMessage
		if json.Unmarshal(value, &parts) != nil || len(parts) == 0 {
			return false
		}
		want := map[string]Feature{}
		for _, sub := range f.Features {
			// A part that only reports (access without SET) is not part of what /set takes.
			if sub.Access == 0 || sub.Settable() {
				want[sub.Property] = sub
			}
		}
		// Every part of the composite must be given (a colour is x AND y), and nothing else.
		if len(parts) != len(want) {
			return false
		}
		for k, v := range parts {
			sub, ok := want[k]
			if !ok || !valid(sub, v) {
				return false
			}
		}
		return true
	}
	return false
}

func isNumber(v json.RawMessage) bool {
	b := bytes.TrimSpace(v)
	return len(b) > 0 && (b[0] == '-' || (b[0] >= '0' && b[0] <= '9'))
}

// Toggle resolves a toggle of a binary property into the explicit opposite of its last reported value. Aether
// never publishes value_toggle: an explicit value makes a duplicate delivery harmless.
func Toggle(exposes json.RawMessage, property string, current json.RawMessage) (json.RawMessage, error) {
	all, e := Features(exposes)
	if e != nil {
		return nil, e
	}
	found := false
	for _, f := range all {
		if f.Property != property {
			continue
		}
		found = true
		if f.Type != "binary" || !f.Settable() {
			continue
		}
		switch {
		case len(current) == 0:
			return nil, domain.Because(domain.ErrConflict, "state_unknown")
		case sameJSON(current, f.ValueOn):
			return compact(f.ValueOff), nil
		case sameJSON(current, f.ValueOff):
			return compact(f.ValueOn), nil
		}
		return nil, domain.Because(domain.ErrConflict, "state_unknown")
	}
	if !found {
		return nil, refused("unknown_property")
	}
	return nil, refused("not_toggleable")
}

// Matches reports whether a reported value satisfies a commanded one: exact for binary and enum values, within
// max(value_step, 1% of the range) for numbers (a light may land on 127 when asked for 128), part by part for
// composites (only the parts that were sent are compared).
func Matches(exposes json.RawMessage, property string, desired, reported json.RawMessage) bool {
	all, _ := Features(exposes)
	return MatchesIn(all, property, desired, reported)
}

// MatchesIn is Matches over features already parsed (once per message, not once per open command).
func MatchesIn(all []Feature, property string, desired, reported json.RawMessage) bool {
	for _, f := range all {
		if f.Property == property && f.Settable() && valid(f, desired) && matches(f, desired, reported) {
			return true
		}
	}
	return false
}

func matches(f Feature, desired, reported json.RawMessage) bool {
	switch f.Type {
	case "numeric":
		var d, r float64
		if json.Unmarshal(desired, &d) != nil || json.Unmarshal(reported, &r) != nil {
			return false
		}
		return math.Abs(d-r) <= Tolerance(f)
	case "composite":
		var want, got map[string]json.RawMessage
		if json.Unmarshal(desired, &want) != nil || json.Unmarshal(reported, &got) != nil {
			return false
		}
		for k, v := range want {
			var sub *Feature
			for i := range f.Features {
				if f.Features[i].Property == k {
					sub = &f.Features[i]
				}
			}
			if sub == nil || !matches(*sub, v, got[k]) {
				return false
			}
		}
		return true
	}
	return sameJSON(desired, reported)
}

// Tolerance is how far a reported number may be from the commanded one and still confirm it.
func Tolerance(f Feature) float64 {
	t := 0.0
	if f.ValueStep != nil && *f.ValueStep > 0 {
		t = *f.ValueStep
	}
	if f.ValueMin != nil && f.ValueMax != nil {
		t = math.Max(t, (*f.ValueMax-*f.ValueMin)*0.01)
	}
	if t == 0 {
		t = 1e-9
	}
	return t
}

func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func compact(v json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, v) != nil {
		return v
	}
	return buf.Bytes()
}

// SettableValues picks, from a device state message, the values of the device's settable properties (those a
// command can target), bounded in size. Anything else in the message (linkquality, a sensor reading) is left out.
func SettableValues(exposes json.RawMessage, payload []byte) map[string]json.RawMessage {
	all, _ := Features(exposes)
	return SettableValuesIn(all, payload)
}

// SettableValuesIn is SettableValues over features already parsed.
func SettableValuesIn(all []Feature, payload []byte) map[string]json.RawMessage {
	var state map[string]json.RawMessage
	if json.Unmarshal(payload, &state) != nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for _, f := range settable(all) {
		if v, ok := state[f.Property]; ok && len(v) <= 1024 && json.Valid(v) {
			out[f.Property] = compact(v)
		}
	}
	return out
}
