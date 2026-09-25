package tuya

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
)

// Ref is how one Aether property maps back to its data point. The wire value of a command and the real value
// of a report are converted through it; the Edge itself never knows what a data point means.
type Ref struct {
	DP   int    `json:"dp"`
	Kind string `json:"kind"` // onoff | bool | enumbool | value | enum | string | bitmap
	// Scale: real = raw / 10^Scale (value). Div is an extra read-only divisor (mA reported, A kept).
	Scale int     `json:"scale,omitempty"`
	Div   float64 `json:"div,omitempty"`
	// Raw bounds of a value, for commands.
	Min  *int64 `json:"min,omitempty"`
	Max  *int64 `json:"max,omitempty"`
	Step *int64 `json:"step,omitempty"`
	// On is the raw enum value that means true (enumbool).
	On       string   `json:"on,omitempty"`
	Values   []string `json:"values,omitempty"`
	Writable bool     `json:"writable,omitempty"`
}

// Translation is what Aether keeps of a device besides its raw specification.
type Translation struct {
	Exposes      json.RawMessage
	DPMap        map[string]Ref
	Gangs        []zigbee2mqtt.Gang
	Category     string
	LocalCapable bool
}

// canonical renames Tuya codes to Aether's canonical property names, so thresholds, cards and floor plans work
// as for every other device. Everything not listed keeps its Tuya code.
var canonical = map[string]string{
	"cur_power":           "power",
	"cur_voltage":         "voltage",
	"cur_current":         "current",
	"add_ele":             "energy",
	"va_temperature":      "temperature",
	"temp_current":        "temperature",
	"va_humidity":         "humidity",
	"humidity_value":      "humidity",
	"switch_led":          "state",
	"bright_value":        "brightness",
	"bright_value_v2":     "brightness",
	"temp_value":          "color_temp",
	"temp_value_v2":       "color_temp",
	"percent_control":     "position",
	"doorcontact_state":   "door",
	"pir_state":           "motion",
	"presence_state":      "motion",
	"watersensor_state":   "leak",
	"smoke_sensor_status": "smoke",
	"gas_sensor_status":   "gas",
	"co_state":            "carbon_monoxide",
	"battery_percentage":  "battery",
}

// preferred wins a rename both of two codes want (a light with bright_value and bright_value_v2).
var preferred = map[string]bool{"bright_value_v2": true, "temp_value_v2": true, "va_temperature": true, "va_humidity": true}

// enumAlarm lists read-only enums that are really a flag: the raw value that means "on".
var enumAlarm = map[string]string{
	"pir_state":           "pir",
	"presence_state":      "presence",
	"watersensor_state":   "alarm",
	"smoke_sensor_status": "alarm",
	"gas_sensor_status":   "alarm",
	"co_state":            "alarm",
}

var gangPattern = regexp.MustCompile(`^switch(?:_([1-6]))?$`)

// categories maps Tuya's product category code to Aether's reading kind (the same kinds Zigbee devices use).
var categories = map[string]string{
	"kg": zigbee2mqtt.CategorySwitch, "cz": zigbee2mqtt.CategorySwitch, "pc": zigbee2mqtt.CategorySwitch, "tdq": zigbee2mqtt.CategorySwitch,
	"dj": zigbee2mqtt.CategoryLighting, "dd": zigbee2mqtt.CategoryLighting, "fwd": zigbee2mqtt.CategoryLighting, "tgq": zigbee2mqtt.CategoryLighting,
	"tgkg": zigbee2mqtt.CategoryLighting, "xdd": zigbee2mqtt.CategoryLighting, "dc": zigbee2mqtt.CategoryLighting,
	"cl": zigbee2mqtt.CategoryCover, "clkg": zigbee2mqtt.CategoryCover,
	"wk": zigbee2mqtt.CategoryClimate, "kt": zigbee2mqtt.CategoryClimate, "qn": zigbee2mqtt.CategoryClimate, "wkf": zigbee2mqtt.CategoryClimate,
	"fs":    zigbee2mqtt.CategoryFan,
	"wsdcg": zigbee2mqtt.CategoryEnvironment, "co2bj": zigbee2mqtt.CategoryEnvironment, "hjjcy": zigbee2mqtt.CategoryEnvironment,
	"mcs": zigbee2mqtt.CategoryDoor,
	"pir": zigbee2mqtt.CategoryOccupancy, "hps": zigbee2mqtt.CategoryOccupancy,
	"ywbj": zigbee2mqtt.CategoryHazard, "rqbj": zigbee2mqtt.CategoryHazard, "cobj": zigbee2mqtt.CategoryHazard,
	"sj":  zigbee2mqtt.CategoryLeak,
	"sos": zigbee2mqtt.CategorySOS,
	"ms":  zigbee2mqtt.CategoryLock, "jtmspro": zigbee2mqtt.CategoryLock,
	"zndb": zigbee2mqtt.CategoryMetering, "dlq": zigbee2mqtt.CategoryMetering,
}

// sleepers are categories whose devices are almost always battery powered: they wake, report to the Tuya cloud
// and sleep, so they are never reachable on the LAN. A heuristic; the Edge's LAN discovery has the last word.
var sleepers = map[string]bool{"wsdcg": true, "mcs": true, "pir": true, "ywbj": true, "sj": true, "sos": true, "ms": true, "jtmspro": true, "cobj": true}

// Translate turns a normalised specification into exposes, the dp map and the gangs of a device of a Tuya
// category. Raw and JSON data points (colour_data, schedules, ...) are left out.
func Translate(category string, dps []DP) Translation {
	t := Translation{DPMap: map[string]Ref{}, Gangs: []zigbee2mqtt.Gang{}}
	// Assign property names: a canonical name goes to the preferred code when two codes want it.
	names := map[int]string{}
	taken := map[string]int{}
	order := append([]DP(nil), dps...)
	sort.SliceStable(order, func(i, j int) bool { return preferred[order[i].Code] && !preferred[order[j].Code] })
	for _, d := range order {
		name := d.Code
		if c, ok := canonical[d.Code]; ok {
			if _, used := taken[c]; !used {
				name = c
			}
		}
		if _, used := taken[name]; used {
			name = d.Code
			if _, used := taken[name]; used {
				continue
			}
		}
		taken[name] = d.ID
		names[d.ID] = name
	}
	gangNo := map[int]int{}
	for _, d := range dps {
		if m := gangPattern.FindStringSubmatch(d.Code); m != nil && d.Type == "bool" {
			n := 1
			if m[1] != "" {
				n, _ = strconv.Atoi(m[1])
			}
			gangNo[d.ID] = n
		}
	}
	features := []zigbee2mqtt.Feature{}
	for _, d := range dps {
		name, ok := names[d.ID]
		if !ok {
			continue
		}
		access := 0
		if d.Readable() {
			access |= zigbee2mqtt.AccessState
		}
		if d.Writable() {
			access |= zigbee2mqtt.AccessSet
		}
		f := zigbee2mqtt.Feature{Name: name, Property: name, Access: access, Unit: d.Unit}
		ref := Ref{DP: d.ID, Writable: d.Writable()}
		switch d.Type {
		case "bool":
			f.Type = "binary"
			_, gang := gangNo[d.ID]
			if d.Writable() || gang || d.Code == "switch_led" {
				// Outputs speak ON/OFF like every other switch Aether knows, so toggles and gang events work.
				f.ValueOn, f.ValueOff = json.RawMessage(`"ON"`), json.RawMessage(`"OFF"`)
				ref.Kind = "onoff"
			} else {
				f.ValueOn, f.ValueOff = json.RawMessage(`true`), json.RawMessage(`false`)
				ref.Kind = "bool"
			}
		case "value":
			f.Type = "numeric"
			ref.Kind, ref.Scale, ref.Min, ref.Max, ref.Step = "value", d.Scale, d.Min, d.Max, d.Step
			div := 1.0
			if d.Code == "cur_current" && (d.Unit == "mA" || d.Unit == "") {
				div, ref.Div, f.Unit = 1000, 1000, "A"
				access &^= zigbee2mqtt.AccessSet // a measurement, never a setting
				f.Access, ref.Writable = access, false
			}
			scale := math.Pow(10, float64(d.Scale)) * div
			if d.Min != nil {
				v := float64(*d.Min) / scale
				f.ValueMin = &v
			}
			if d.Max != nil {
				v := float64(*d.Max) / scale
				f.ValueMax = &v
			}
			if d.Step != nil {
				v := float64(*d.Step) / scale
				f.ValueStep = &v
			}
		case "enum":
			if on, flag := enumAlarm[d.Code]; flag && !d.Writable() && contains(d.Range, on) {
				f.Type = "binary"
				f.ValueOn, f.ValueOff = json.RawMessage(`true`), json.RawMessage(`false`)
				ref.Kind, ref.On = "enumbool", on
				break
			}
			f.Type = "enum"
			ref.Kind, ref.Values = "enum", d.Range
			for _, v := range d.Range {
				raw, _ := json.Marshal(v)
				f.Values = append(f.Values, raw)
			}
		case "string":
			f.Type, ref.Kind = "text", "string"
		case "bitmap":
			f.Type, ref.Kind = "numeric", "bitmap"
			f.Access &^= zigbee2mqtt.AccessSet
			ref.Writable = false
		default:
			continue // raw and json data points are not translated
		}
		features = append(features, f)
		t.DPMap[name] = ref
		if n, gang := gangNo[d.ID]; gang {
			t.Gangs = append(t.Gangs, zigbee2mqtt.Gang{Gang: n, Property: name, Settable: d.Writable()})
		}
	}
	// Gangs in wall order; a bare "switch" is gang 1. At most four, numbered 1..n.
	sort.SliceStable(t.Gangs, func(i, j int) bool { return t.Gangs[i].Gang < t.Gangs[j].Gang })
	if len(t.Gangs) > 4 {
		t.Gangs = t.Gangs[:4]
	}
	for i := range t.Gangs {
		t.Gangs[i].Gang = i + 1
	}
	raw, _ := json.Marshal(features)
	t.Exposes = raw
	t.Category = categories[category]
	if t.Category == "" {
		flat, _ := zigbee2mqtt.Features(raw)
		t.Category = zigbee2mqtt.Summarize(flat).Category()
		if len(t.Gangs) > 0 && t.Category == zigbee2mqtt.CategoryInfo {
			t.Category = zigbee2mqtt.CategorySwitch
		}
	}
	t.LocalCapable = !sleepers[category]
	return t
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
