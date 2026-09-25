package zigbee2mqtt

import (
	"aether/backend/internal/adapters/minew"
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// Generic ingest: every device Zigbee2MQTT supports is read through its own definition (exposes), not a
// per-model decoder. Well-known features become Aether's canonical metrics so thresholds, the occupancy and
// door logic, floor plans and dashboards work unchanged; every other published numeric or binary feature is
// kept under its own Zigbee2MQTT property name, and enums/text under Values.

// Categories: the reading kind of a Zigbee2MQTT device, derived from what it exposes. The UI picks the card
// from it. "switch", "door", "occupancy", "leak", "environment", "light" (illuminance), "motion" and "tamper"
// are the kinds Minew devices already use, so the same visuals apply.
const (
	CategorySOS         = "sos"
	CategoryHazard      = "hazard"
	CategoryLeak        = "leak"
	CategoryDoor        = "door"
	CategoryOccupancy   = "occupancy"
	CategoryLock        = "lock"
	CategoryCover       = "cover"
	CategoryClimate     = "climate"
	CategoryLighting    = "lighting"
	CategorySwitch      = KindSwitch
	CategoryFan         = "fan"
	CategoryRemote      = "remote"
	CategoryEnvironment = "environment"
	CategoryMetering    = "metering"
	CategoryMotion      = "motion"
	CategoryLight       = "light"
	CategoryTamper      = "tamper"
	CategoryInfo        = "info"
)

// Hazards are the life-safety binaries raised as `hazard` events (smoke detectors, gas and CO sensors).
var Hazards = []string{"smoke", "gas", "carbon_monoxide"}

// sosActions are the action values that mean "call for help": Tuya TS0215A_sos and Heiman remotes send
// emergency, alarm keypads panic, several SOS buttons sos. Ordinary remotes (single, double, on, off, ...)
// never match, so a light switch can never raise an SOS.
var sosActions = map[string]bool{"emergency": true, "sos": true, "panic": true}

// IsSOSAction reports whether an action value is an emergency call.
func IsSOSAction(action string) bool { return sosActions[strings.ToLower(action)] }

// sosBinaries are published (not settable) binaries whose "on" is a press of an SOS button (SEB01ZB, PB206,
// WKE502Z).
var sosBinaries = map[string]bool{"sos": true, "sos_alarm": true}

// semanticBinaries are names whose meaning in Aether (a life-safety alarm, a door, occupancy, a tamper or leak
// flag, SOS) holds only for a binary feature. zigbee-herdsman-converters also has numerics with some of these
// names — numeric("gas") is a gas meter in m³ or %LEL, numeric("vibration") a Tuya vibration strength — and a
// meter reading exactly 1.0 must never look like a gas alarm. Such a numeric is kept as <name>_value.
var semanticBinaries = map[string]bool{"smoke": true, "gas": true, "carbon_monoxide": true, "sos": true, "sos_alarm": true, "tamper": true,
	"water_leak": true, "contact": true, "occupancy": true, "presence": true, "vibration": true, "battery_low": true}

// featureKey is the name a feature is known by in a Profile and in a reading: its own name, or <name>_value
// for a non-binary feature that collides with a semantic binary name.
func featureKey(f Feature) string {
	if semanticBinaries[f.Name] && f.Type != "binary" {
		return f.Name + "_value"
	}
	return f.Name
}

// safetyMetrics are never dropped by the MaxMetrics cap, whatever their position in the definition.
func safetyMetric(name string) bool {
	switch name {
	case "door", "leak", "motion", "tamper", "sos", "smoke", "gas", "carbon_monoxide", "vibration", "battery_low", "battery":
		return true
	}
	return strings.HasPrefix(name, "sw") && len(name) == 3
}

var environmentNames = []string{"temperature", "humidity", "pressure", "co2", "voc", "voc_index", "formaldehyd", "pm25", "pm10"}
var meteringNames = []string{"power", "energy", "current", "gas_value"}

// Profile is the short summary of a definition Aether keeps: the names of its generic features, the specific
// types they sit in, and whether it can call for help. The device catalog is built from the same summary.
type Profile struct {
	Names  map[string]bool
	Groups map[string]bool
	SOS    bool
}

// Summarize reduces a flattened definition to its Profile.
func Summarize(features []Feature) Profile {
	p := Profile{Names: map[string]bool{}, Groups: map[string]bool{}}
	for _, f := range features {
		p.Names[featureKey(f)] = true
		if f.Group != "" {
			p.Groups[f.Group] = true
		}
		if f.Name == "action" && f.Type == "enum" {
			for _, v := range f.Values {
				var s string
				if json.Unmarshal(v, &s) == nil && IsSOSAction(s) {
					p.SOS = true
				}
			}
		}
		if f.Type == "binary" && sosBinaries[f.Name] && !f.Settable() {
			p.SOS = true
		}
	}
	return p
}

// Category picks the reading kind of a device from its Profile. Order matters: a life-safety purpose wins over
// what else the device measures (a smoke detector also reports battery), and an actuator wins over the
// sensors it carries (a plug with metering is a switch, a thermostat with a temperature is climate).
func (p Profile) Category() string {
	has := func(names ...string) bool {
		for _, n := range names {
			if p.Names[n] {
				return true
			}
		}
		return false
	}
	switch {
	case p.SOS:
		return CategorySOS
	case has(Hazards...):
		return CategoryHazard
	case has("water_leak"):
		return CategoryLeak
	case has("contact"):
		return CategoryDoor
	case has("occupancy", "presence"):
		return CategoryOccupancy
	case p.Groups["lock"]:
		return CategoryLock
	case p.Groups["cover"]:
		return CategoryCover
	case p.Groups["climate"]:
		return CategoryClimate
	case p.Groups["light"]:
		return CategoryLighting
	case p.Groups["switch"]:
		return CategorySwitch
	case p.Groups["fan"]:
		return CategoryFan
	case has("action"):
		return CategoryRemote
	case has(environmentNames...):
		return CategoryEnvironment
	case has(meteringNames...):
		return CategoryMetering
	case has("vibration"):
		return CategoryMotion
	case has("illuminance", "illuminance_lux"):
		return CategoryLight
	case has("tamper"):
		return CategoryTamper
	}
	return CategoryInfo
}

// Bounds of one reading: a definition can expose dozens of attributes and a malicious bridge could invent more.
const (
	MaxMetrics = 48
	MaxValues  = 32
	maxValue   = 64
)

// canonical maps a Zigbee2MQTT feature name (on the device's main endpoint) to Aether's metric name. Binaries
// not listed keep their own name; numerics are kept under their own name unless listed.
var canonical = map[string]string{
	"occupancy":       "motion",
	"presence":        "motion",
	"water_leak":      "leak",
	"illuminance_lux": "illuminance",
	"sos_alarm":       "sos",
}

// ParseState turns a device state message into a reading, using the device's definition. ok is false when the
// message carries nothing Aether keeps (an empty update, or only unknown keys).
func ParseState(b []byte, d Device, at time.Time) (minew.Reading, bool) {
	features, _ := Features(d.Exposes)
	return ParseStateWith(b, d, features, at)
}

// ParseStateWith is ParseState with the definition already flattened (the ingest parses it once per message).
func ParseStateWith(b []byte, d Device, features []Feature, at time.Time) (minew.Reading, bool) {
	var obj map[string]json.RawMessage
	if e := json.Unmarshal(b, &obj); e != nil {
		return minew.Reading{}, false
	}
	kind := d.Category
	if kind == "" {
		kind = Summarize(features).Category()
		if len(d.Gangs) > 0 && kind == CategoryInfo {
			kind = CategorySwitch
		}
	}
	r := minew.Reading{Source: "gateway", ReceivedAt: at, Kind: kind, Model: d.Model, Frames: []string{FrameState}, Metrics: map[string]float64{}}
	setMetric := func(name string, v float64) {
		if _, exists := r.Metrics[name]; exists || len(r.Metrics) < MaxMetrics || safetyMetric(name) {
			r.Metrics[name] = v
		}
	}
	// Switch outputs keep the gang metrics sw1..sw4 the switch events are built on.
	gangProperty := map[string]bool{}
	for _, g := range d.Gangs {
		gangProperty[g.Property] = true
		var v string
		if json.Unmarshal(obj[g.Property], &v) != nil {
			continue
		}
		switch strings.ToUpper(v) {
		case "ON":
			setMetric("sw"+strconv.Itoa(g.Gang), 1)
		case "OFF":
			setMetric("sw"+strconv.Itoa(g.Gang), 0)
		}
	}
	if len(features) == 0 {
		features = fallbackFeatures(obj)
	}
	seen := map[string]bool{}
	// Zigbee2MQTT 1.x reports illuminance as a raw value next to illuminance_lux; the lux one is canonical.
	illuminanceLux := false
	for _, f := range features {
		if _, present := obj[f.Property]; present && f.Name == "illuminance_lux" && f.Endpoint == "" {
			illuminanceLux = true
		}
	}
	for _, f := range features {
		raw, present := obj[f.Property]
		// null is "no value" (Zigbee2MQTT publishes it for an attribute it has not read yet), never zero.
		if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || seen[f.Property] || gangProperty[f.Property] || f.Access&AccessState == 0 || f.Category == "config" {
			continue
		}
		seen[f.Property] = true
		main := f.Endpoint == "" && f.Property == f.Name
		name := f.Property
		if main {
			if key := featureKey(f); key != f.Name {
				name = key // a numeric "gas" / "vibration" is a measurement, never the alarm flag
			} else if c, ok := canonical[f.Name]; ok {
				name = c
			}
		}
		switch f.Type {
		case "binary":
			on, ok := binaryOn(f, raw)
			if !ok {
				continue
			}
			v := 0.0
			if on {
				v = 1
			}
			if name == "motion" {
				// occupancy and presence both mean "someone is here": either one set is enough, in any order.
				if before, ok := r.Metrics["motion"]; ok && before == 1 {
					v = 1
				}
			}
			if main && f.Name == "contact" {
				// Zigbee2MQTT: contact=true means the magnet is next to the sensor, i.e. CLOSED. Aether's door metric
				// is 1 = open.
				var closed bool
				if json.Unmarshal(raw, &closed) != nil {
					continue
				}
				name, v = "door", 1
				if closed {
					v = 0
				}
			}
			setMetric(name, v)
		case "numeric":
			var v float64
			if json.Unmarshal(raw, &v) != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			switch {
			case main && f.Name == "battery":
				// Also kept as the battery metric, so 0 % is a reading (the Battery field's 0 means "unknown").
				if v >= 0 && v <= 100 {
					r.Battery = int(math.Round(v))
					setMetric("battery", math.Round(v))
				}
				continue
			case main && f.Name == "voltage" && strings.EqualFold(f.Unit, "mV"):
				v = math.Round(v) / 1000 // battery voltage in mV; Aether's voltage metric is in volts, like Minew's
			case main && f.Name == "illuminance" && illuminanceLux:
				continue
			}
			setMetric(name, v)
		case "enum", "text":
			var s string
			if json.Unmarshal(raw, &s) != nil {
				continue
			}
			s = clip(s, maxValue)
			if main && f.Name == "action" {
				r.Action = s
				continue
			}
			if s == "" {
				continue
			}
			if r.Values == nil {
				r.Values = map[string]string{}
			}
			if len(r.Values) < MaxValues {
				r.Values[f.Property] = s
			}
		}
	}
	var lq *float64
	if json.Unmarshal(obj["linkquality"], &lq) == nil && lq != nil && *lq >= 0 && *lq <= 255 {
		setMetric("linkquality", *lq)
	}
	var source string
	if json.Unmarshal(obj["aether_source"], &source) == nil && source == "simulated" {
		r.Source = "simulated"
	}
	if len(r.Metrics) == 0 && len(r.Values) == 0 && r.Action == "" {
		return minew.Reading{}, false
	}
	return r, true
}

// binaryOn interprets a binary feature's value against its value_on / value_off (true/false, "ON"/"OFF",
// "LOCK"/"UNLOCK", ...). A definition without them falls back to JSON booleans and ON/OFF.
func binaryOn(f Feature, raw json.RawMessage) (bool, bool) {
	value := compactJSON(raw)
	if len(f.ValueOn) > 0 && bytes.Equal(value, compactJSON(f.ValueOn)) {
		return true, true
	}
	if len(f.ValueOff) > 0 && bytes.Equal(value, compactJSON(f.ValueOff)) {
		return false, true
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToUpper(s) {
		case "ON":
			return true, true
		case "OFF":
			return false, true
		}
	}
	return false, false
}

func compactJSON(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return raw
	}
	return buf.Bytes()
}

// fallbackFeatures reads a device whose definition is unknown (not yet interviewed, or unsupported by the
// bridge's Zigbee2MQTT version): only the well-known keys, typed by their JSON value.
func fallbackFeatures(obj map[string]json.RawMessage) []Feature {
	known := append(append(append([]string{"battery", "voltage", "illuminance", "illuminance_lux", "device_temperature", "action"}, environmentNames...), meteringNames...),
		"contact", "occupancy", "presence", "water_leak", "tamper", "vibration", "battery_low", "smoke", "gas", "carbon_monoxide", "sos")
	out := []Feature{}
	for _, name := range known {
		raw, ok := obj[name]
		if !ok {
			continue
		}
		f := Feature{Name: name, Property: name, Access: AccessState}
		// Without a definition there is no unit: a battery voltage above 100 can only be millivolts.
		var number float64
		if name == "voltage" && json.Unmarshal(raw, &number) == nil && number > 100 {
			f.Unit = "mV"
		}
		switch trimmed := bytes.TrimSpace(raw); {
		case bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false")):
			f.Type = "binary"
		case len(trimmed) > 0 && trimmed[0] == '"':
			f.Type = "enum"
		default:
			f.Type = "numeric"
		}
		out = append(out, f)
	}
	return out
}
