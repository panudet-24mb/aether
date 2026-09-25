package zigbee2mqtt

import (
	"aether/backend/internal/domain"
	"encoding/json"
	"errors"
	"testing"
)

// Exposes shaped like zigbee2mqtt.io's definitions: a colour bulb (IKEA LED1545G12-style), a curtain motor, a
// lock, a thermostat and a 2-gang switch, plus a read-only sensor property.
const testExposes = `[
 {"type":"light","features":[
   {"type":"binary","name":"state","property":"state","access":7,"value_on":"ON","value_off":"OFF","value_toggle":"TOGGLE"},
   {"type":"numeric","name":"brightness","property":"brightness","access":7,"value_min":0,"value_max":254},
   {"type":"numeric","name":"color_temp","property":"color_temp","access":7,"unit":"mired","value_min":250,"value_max":454},
   {"type":"composite","name":"color_xy","property":"color","access":7,"features":[
     {"type":"numeric","name":"x","property":"x","access":7},{"type":"numeric","name":"y","property":"y","access":7}]},
   {"type":"composite","name":"color_hs","property":"color","access":7,"features":[
     {"type":"numeric","name":"hue","property":"hue","access":7},{"type":"numeric","name":"saturation","property":"saturation","access":7}]}]},
 {"type":"cover","features":[
   {"type":"enum","name":"state","property":"state_cover","access":3,"values":["OPEN","CLOSE","STOP"]},
   {"type":"numeric","name":"position","property":"position","access":7,"unit":"%","value_min":0,"value_max":100}]},
 {"type":"lock","features":[
   {"type":"binary","name":"state","property":"lock_state","access":7,"value_on":"LOCK","value_off":"UNLOCK"}]},
 {"type":"climate","features":[
   {"type":"numeric","name":"occupied_heating_setpoint","property":"occupied_heating_setpoint","access":7,"unit":"°C","value_min":5,"value_max":35,"value_step":0.5},
   {"type":"enum","name":"system_mode","property":"system_mode","access":7,"values":["off","heat","auto"]},
   {"type":"numeric","name":"local_temperature","property":"local_temperature","access":5,"unit":"°C"}]},
 {"type":"switch","endpoint":"l1","features":[{"type":"binary","name":"state","property":"state_l1","endpoint":"l1","access":7,"value_on":"ON","value_off":"OFF"}]},
 {"type":"switch","endpoint":"l2","features":[{"type":"binary","name":"state","property":"state_l2","endpoint":"l2","access":1,"value_on":"ON","value_off":"OFF"}]},
 {"type":"text","name":"schedule","property":"schedule","access":7},
 {"type":"numeric","name":"linkquality","property":"linkquality","access":1,"value_min":0,"value_max":255}
]`

func reason(e error) string {
	var r domain.ReasonError
	if errors.As(e, &r) {
		return r.Reason
	}
	if e == nil {
		return ""
	}
	return e.Error()
}

func TestValidateCommands(t *testing.T) {
	ok := []struct{ property, value, want string }{
		{"state", `"ON"`, `"ON"`},
		{"brightness", `128`, `128`},
		{"brightness", `0`, `0`},
		{"color_temp", `300`, `300`},
		{"color", `{"x": 0.31, "y": 0.33}`, `{"x":0.31,"y":0.33}`},
		{"color", `{"hue":120,"saturation":80}`, `{"hue":120,"saturation":80}`},
		{"position", `50`, `50`},
		{"state_cover", `"STOP"`, `"STOP"`},
		{"lock_state", `"LOCK"`, `"LOCK"`},
		{"occupied_heating_setpoint", `21.5`, `21.5`},
		{"system_mode", `"heat"`, `"heat"`},
		{"state_l1", `"OFF"`, `"OFF"`},
	}
	for _, c := range ok {
		got, e := Validate(json.RawMessage(testExposes), c.property, json.RawMessage(c.value))
		if e != nil || string(got) != c.want {
			t.Fatalf("%s=%s: %s %v", c.property, c.value, got, e)
		}
	}
	bad := []struct{ property, value, reason string }{
		{"state", `"on"`, "value"},                             // binary: only value_on/value_off
		{"state", `"TOGGLE"`, "value"},                         // never value_toggle: request action toggle instead
		{"state", `true`, "value"},                             // wrong type
		{"brightness", `255`, "value"},                         // above value_max
		{"brightness", `-1`, "value"},                          // below value_min
		{"brightness", `"128"`, "value"},                       // a string is not a number
		{"occupied_heating_setpoint", `21.3`, "value"},         // off the 0.5 step
		{"system_mode", `"cool"`, "value"},                     // not in the enumeration
		{"color", `{"x":0.3}`, "value"},                        // composite needs every part
		{"color", `{"x":0.3,"y":0.3,"brightness":5}`, "value"}, // and nothing else
		{"color", `{"x":"a","y":0.3}`, "value"},                // parts are validated too
		{"local_temperature", `20`, "not_settable"},            // read-only (access 5)
		{"state_l2", `"ON"`, "not_settable"},                   // read-only gang
		{"linkquality", `100`, "not_settable"},                 // published only
		{"schedule", `"x"`, "unsupported_feature"},             // settable text is not commandable
		{"volume", `1`, "unknown_property"},                    // not in the definition
		{"state; DROP", `"ON"`, "unknown_property"},            // not a property name
		{"brightness", ``, "value"},                            // empty
	}
	for _, c := range bad {
		_, e := Validate(json.RawMessage(testExposes), c.property, json.RawMessage(c.value))
		if !errors.Is(e, domain.ErrInvalid) || reason(e) != c.reason {
			t.Fatalf("%s=%s: want %s, got %v", c.property, c.value, c.reason, e)
		}
	}
}

func TestToggleAndMatch(t *testing.T) {
	ex := json.RawMessage(testExposes)
	if v, e := Toggle(ex, "state", json.RawMessage(`"ON"`)); e != nil || string(v) != `"OFF"` {
		t.Fatalf("toggle on: %s %v", v, e)
	}
	if v, e := Toggle(ex, "lock_state", json.RawMessage(`"UNLOCK"`)); e != nil || string(v) != `"LOCK"` {
		t.Fatalf("toggle lock: %s %v", v, e)
	}
	if _, e := Toggle(ex, "state", nil); !errors.Is(e, domain.ErrConflict) || reason(e) != "state_unknown" {
		t.Fatalf("unknown state: %v", e)
	}
	if _, e := Toggle(ex, "brightness", json.RawMessage(`10`)); !errors.Is(e, domain.ErrInvalid) || reason(e) != "not_toggleable" {
		t.Fatalf("numeric toggle: %v", e)
	}
	if _, e := Toggle(ex, "nope", json.RawMessage(`"ON"`)); reason(e) != "unknown_property" {
		t.Fatalf("unknown toggle: %v", e)
	}
	match := []struct {
		property, desired, reported string
		want                        bool
	}{
		{"brightness", `128`, `127`, true},  // within 1% of 0..254
		{"brightness", `128`, `120`, false}, // outside
		{"occupied_heating_setpoint", `21.5`, `21`, true},
		{"occupied_heating_setpoint", `21.5`, `22.5`, false},
		{"state", `"ON"`, `"ON"`, true},
		{"state", `"ON"`, `"OFF"`, false},
		{"color", `{"x":0.31,"y":0.33}`, `{"x":0.31,"y":0.33,"hue":10}`, true},
		{"color", `{"x":0.31,"y":0.33}`, `{"x":0.5,"y":0.33}`, false},
		{"system_mode", `"heat"`, `"heat"`, true},
	}
	for _, c := range match {
		if got := Matches(ex, c.property, json.RawMessage(c.desired), json.RawMessage(c.reported)); got != c.want {
			t.Fatalf("%s %s vs %s: %v", c.property, c.desired, c.reported, got)
		}
	}
}

func TestSettableFeaturesAndValues(t *testing.T) {
	ex := json.RawMessage(testExposes)
	names := []string{}
	for _, f := range SettableFeatures(ex) {
		names = append(names, f.Property)
	}
	want := []string{"state", "brightness", "color_temp", "color", "color", "state_cover", "position", "lock_state", "occupied_heating_setpoint", "system_mode", "state_l1"}
	if len(names) != len(want) {
		t.Fatalf("settable: %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("settable: %v", names)
		}
	}
	got := SettableValues(ex, []byte(`{"state":"ON","brightness":10,"linkquality":80,"local_temperature":19.5,"state_l1":"OFF"}`))
	if len(got) != 3 || string(got["brightness"]) != "10" || got["linkquality"] != nil || got["local_temperature"] != nil {
		t.Fatalf("values: %v", got)
	}
}
