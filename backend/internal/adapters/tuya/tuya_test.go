package tuya

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"aether/backend/internal/domain"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/" + name)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func spec(t *testing.T, name string) (Translation, []DP, string) {
	t.Helper()
	dps, category, e := ParseSpecifications(load(t, name))
	if e != nil {
		t.Fatal(e)
	}
	return Translate(category, dps), dps, category
}

// device is how the ingest sees a translated Tuya device.
func device(tr Translation) zigbee2mqtt.Device {
	return zigbee2mqtt.Device{IEEE: "bf1234567890abcdefgh", FriendlyName: "test", Gangs: tr.Gangs, Exposes: tr.Exposes, Category: tr.Category}
}

func state(t *testing.T, tr Translation, dps map[int]json.RawMessage) []byte {
	t.Helper()
	b, e := json.Marshal(State(tr.DPMap, dps))
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func reason(e error) string {
	var r domain.ReasonError
	if errors.As(e, &r) {
		return r.Reason
	}
	return ""
}

func TestSwitchThreeGang(t *testing.T) {
	tr, dps, category := spec(t, "kg_3gang.json")
	if category != "kg" || len(dps) != 5 || tr.Category != zigbee2mqtt.CategorySwitch || !tr.LocalCapable {
		t.Fatalf("%s %d %s %v", category, len(dps), tr.Category, tr.LocalCapable)
	}
	if len(tr.Gangs) != 3 || tr.Gangs[1].Property != "switch_2" || tr.Gangs[1].Gang != 2 || !tr.Gangs[1].Settable {
		t.Fatalf("gangs %+v", tr.Gangs)
	}
	// Commands: validated against the exposes exactly like a Zigbee device, then encoded to the data point.
	if _, e := zigbee2mqtt.Validate(tr.Exposes, "switch_2", json.RawMessage(`"ON"`)); e != nil {
		t.Fatal(e)
	}
	if _, e := zigbee2mqtt.Validate(tr.Exposes, "switch_2", json.RawMessage(`true`)); reason(e) != "value" {
		t.Fatalf("raw bool accepted: %v", e)
	}
	wire, e := Wire(tr.DPMap, "switch_2", json.RawMessage(`"ON"`))
	if e != nil || string(wire) != `{"dps":{"2":true}}` {
		t.Fatal(string(wire), e)
	}
	off, e := zigbee2mqtt.Toggle(tr.Exposes, "switch_1", json.RawMessage(`"ON"`))
	if e != nil || string(off) != `"OFF"` {
		t.Fatal(string(off), e)
	}
	if w, e := Wire(tr.DPMap, "relay_status", json.RawMessage(`"last"`)); e != nil || string(w) != `{"dps":{"14":"last"}}` {
		t.Fatal(string(w), e)
	}
	if _, e := Wire(tr.DPMap, "relay_status", json.RawMessage(`"sometimes"`)); reason(e) != "value" {
		t.Fatal(e)
	}
	// Reports: raw data points become the gang metrics the switch events are built on.
	payload := state(t, tr, map[int]json.RawMessage{1: json.RawMessage(`true`), 2: json.RawMessage(`false`), 3: json.RawMessage(`true`), 99: json.RawMessage(`1`)})
	features, _ := zigbee2mqtt.Features(tr.Exposes)
	r, ok := zigbee2mqtt.ParseStateWith(payload, device(tr), features, time.Now())
	if !ok || r.Metrics["sw1"] != 1 || r.Metrics["sw2"] != 0 || r.Metrics["sw3"] != 1 || r.Kind != zigbee2mqtt.CategorySwitch {
		t.Fatalf("%s -> %+v", payload, r)
	}
}

func TestMeteringPlugScales(t *testing.T) {
	tr, _, _ := spec(t, "cz_metering.json")
	if tr.Category != zigbee2mqtt.CategorySwitch || len(tr.Gangs) != 1 || tr.Gangs[0].Property != "switch_1" {
		t.Fatalf("%s %+v", tr.Category, tr.Gangs)
	}
	payload := state(t, tr, map[int]json.RawMessage{1: json.RawMessage(`true`), 17: json.RawMessage(`15`), 18: json.RawMessage(`1234`),
		19: json.RawMessage(`1003`), 20: json.RawMessage(`2301`), 26: json.RawMessage(`0`)})
	var got map[string]any
	_ = json.Unmarshal(payload, &got)
	if got["current"] != 1.234 || got["power"] != 100.3 || got["voltage"] != 230.1 || got["energy"] != 0.015 || got["fault"] != 0.0 {
		t.Fatalf("%s", payload)
	}
	features, _ := zigbee2mqtt.Features(tr.Exposes)
	r, ok := zigbee2mqtt.ParseStateWith(payload, device(tr), features, time.Now())
	if !ok || r.Metrics["power"] != 100.3 || r.Metrics["current"] != 1.234 || r.Metrics["sw1"] != 1 {
		t.Fatalf("%+v", r.Metrics)
	}
	// A measurement is never a setting.
	for _, p := range []string{"current", "power", "fault"} {
		if _, e := Wire(tr.DPMap, p, json.RawMessage(`1`)); reason(e) != "not_settable" {
			t.Fatalf("%s: %v", p, e)
		}
		if _, e := zigbee2mqtt.Validate(tr.Exposes, p, json.RawMessage(`1`)); e == nil {
			t.Fatalf("%s validated", p)
		}
	}
}

func TestLightV2Model(t *testing.T) {
	dps, e := ParseModel(load(t, "dj_light_v2_model.json"))
	if e != nil {
		t.Fatal(e)
	}
	tr := Translate("dj", dps)
	if tr.Category != zigbee2mqtt.CategoryLighting || len(tr.Gangs) != 0 {
		t.Fatalf("%s %+v", tr.Category, tr.Gangs)
	}
	if tr.DPMap["brightness"].DP != 22 || tr.DPMap["bright_value"].DP != 3 || tr.DPMap["color_temp"].DP != 23 || tr.DPMap["state"].DP != 20 {
		t.Fatalf("renames: %+v", tr.DPMap)
	}
	if _, ok := tr.DPMap["colour_data_v2"]; ok {
		t.Fatal("json data point translated")
	}
	if _, e := zigbee2mqtt.Validate(tr.Exposes, "brightness", json.RawMessage(`5`)); reason(e) != "value" {
		t.Fatalf("below min: %v", e)
	}
	wire, e := Wire(tr.DPMap, "brightness", json.RawMessage(`500`))
	if e != nil || string(wire) != `{"dps":{"22":500}}` {
		t.Fatal(string(wire), e)
	}
	if w, _ := Wire(tr.DPMap, "state", json.RawMessage(`"OFF"`)); string(w) != `{"dps":{"20":false}}` {
		t.Fatal(string(w))
	}
	if w, _ := Wire(tr.DPMap, "work_mode", json.RawMessage(`"colour"`)); string(w) != `{"dps":{"21":"colour"}}` {
		t.Fatal(string(w))
	}
	// Round trip: the device echoes the value and the report confirms the command.
	features, _ := zigbee2mqtt.Features(tr.Exposes)
	echo := State(tr.DPMap, map[int]json.RawMessage{22: json.RawMessage(`500`)})
	if !zigbee2mqtt.MatchesIn(features, "brightness", json.RawMessage(`500`), echo["brightness"]) {
		t.Fatal("echo does not confirm")
	}
	if zigbee2mqtt.MatchesIn(features, "brightness", json.RawMessage(`500`), json.RawMessage(`900`)) {
		t.Fatal("another value confirmed")
	}
}

func TestCurtain(t *testing.T) {
	tr, _, _ := spec(t, "cl_curtain.json")
	if tr.Category != zigbee2mqtt.CategoryCover {
		t.Fatal(tr.Category)
	}
	if w, e := Wire(tr.DPMap, "position", json.RawMessage(`50`)); e != nil || string(w) != `{"dps":{"2":50}}` {
		t.Fatal(string(w), e)
	}
	if w, e := Wire(tr.DPMap, "control", json.RawMessage(`"close"`)); e != nil || string(w) != `{"dps":{"1":"close"}}` {
		t.Fatal(string(w), e)
	}
	if _, e := Wire(tr.DPMap, "position", json.RawMessage(`101`)); reason(e) != "value" {
		t.Fatal(e)
	}
	if _, e := Wire(tr.DPMap, "percent_state", json.RawMessage(`10`)); reason(e) != "not_settable" {
		t.Fatal(e)
	}
	payload := state(t, tr, map[int]json.RawMessage{2: json.RawMessage(`50`), 3: json.RawMessage(`47`), 7: json.RawMessage(`"closing"`)})
	features, _ := zigbee2mqtt.Features(tr.Exposes)
	r, ok := zigbee2mqtt.ParseStateWith(payload, device(tr), features, time.Now())
	if !ok || r.Metrics["position"] != 50 || r.Metrics["percent_state"] != 47 || r.Values["work_state"] != "closing" {
		t.Fatalf("%+v", r)
	}
}

func TestSleeperEnvironment(t *testing.T) {
	tr, _, _ := spec(t, "wsdcg_sleeper.json")
	if tr.LocalCapable || tr.Category != zigbee2mqtt.CategoryEnvironment {
		t.Fatalf("%v %s", tr.LocalCapable, tr.Category)
	}
	payload := state(t, tr, map[int]json.RawMessage{1: json.RawMessage(`235`), 2: json.RawMessage(`61`), 4: json.RawMessage(`0`)})
	features, _ := zigbee2mqtt.Features(tr.Exposes)
	r, ok := zigbee2mqtt.ParseStateWith(payload, device(tr), features, time.Now())
	if !ok || r.Metrics["temperature"] != 23.5 || r.Metrics["humidity"] != 61 || r.Metrics["battery"] != 0 {
		t.Fatalf("%+v", r.Metrics)
	}
}

func TestThermostatScaleStepAndFlags(t *testing.T) {
	tr, _, _ := spec(t, "wk_thermostat.json")
	if tr.Category != zigbee2mqtt.CategoryClimate {
		t.Fatal(tr.Category)
	}
	if _, e := zigbee2mqtt.Validate(tr.Exposes, "temp_set", json.RawMessage(`21.5`)); e != nil {
		t.Fatal(e)
	}
	if _, e := zigbee2mqtt.Validate(tr.Exposes, "temp_set", json.RawMessage(`21.3`)); reason(e) != "value" {
		t.Fatalf("off-step accepted: %v", e)
	}
	if w, e := Wire(tr.DPMap, "temp_set", json.RawMessage(`21.5`)); e != nil || string(w) != `{"dps":{"2":215}}` {
		t.Fatal(string(w), e)
	}
	if _, e := Wire(tr.DPMap, "temp_set", json.RawMessage(`21.3`)); reason(e) != "value" {
		t.Fatal(e)
	}
	// A bare "switch" is gang 1; a door flag reads true = open; an enum PIR becomes the motion flag.
	if len(tr.Gangs) != 1 || tr.Gangs[0].Property != "switch" {
		t.Fatalf("%+v", tr.Gangs)
	}
	payload := state(t, tr, map[int]json.RawMessage{1: json.RawMessage(`true`), 3: json.RawMessage(`198`), 9: json.RawMessage(`true`), 10: json.RawMessage(`"pir"`)})
	features, _ := zigbee2mqtt.Features(tr.Exposes)
	r, ok := zigbee2mqtt.ParseStateWith(payload, device(tr), features, time.Now())
	if !ok || r.Metrics["temperature"] != 19.8 || r.Metrics["door"] != 1 || r.Metrics["motion"] != 1 || r.Metrics["sw1"] != 1 {
		t.Fatalf("%+v", r.Metrics)
	}
	// Values of the wrong type are dropped, never guessed.
	if got := State(tr.DPMap, map[int]json.RawMessage{1: json.RawMessage(`"yes"`), 3: json.RawMessage(`"hot"`), 10: json.RawMessage(`1`)}); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, b := range []string{`[]`, `{"result":{"functions":[{"code":"../x","dp_id":1,"type":"Boolean"}]}}`, `{"result":{"status":[{"code":"a","dp_id":0,"type":"Boolean"}]}}`, `not json`} {
		if _, _, e := ParseSpecifications([]byte(b)); e == nil {
			t.Fatalf("accepted %s", b)
		}
	}
	if _, e := ParseModel([]byte(`{"result":{"model":"{}"}}`)); e == nil {
		t.Fatal("empty model accepted")
	}
	// Unknown types are skipped; enums without a range are skipped.
	dps, _, e := ParseSpecifications([]byte(`{"result":{"category":"x","status":[{"code":"a","dp_id":1,"type":"Weird","values":"{}"},{"code":"b","dp_id":2,"type":"Enum","values":"{}"},{"code":"c","dp_id":3,"type":"Boolean","values":"{}"}]}}`))
	if e != nil || len(dps) != 1 || dps[0].Code != "c" {
		t.Fatal(dps, e)
	}
}

func TestValidateSpec(t *testing.T) {
	dps, _, e := ParseSpecifications(load(t, "kg_3gang.json"))
	if e != nil || Validate(dps) != nil {
		t.Fatal("a parsed specification must validate", e)
	}
	i := func(v int64) *int64 { return &v }
	good := DP{ID: 2, Code: "bright", Type: "value", Access: "rw", Min: i(0), Max: i(100), Step: i(1), Scale: 1}
	cases := map[string]DP{
		"id 0":        {ID: 0, Code: "a", Type: "bool", Access: "rw"},
		"id 256":      {ID: 256, Code: "a", Type: "bool", Access: "rw"},
		"bad code":    {ID: 3, Code: "a/b", Type: "bool", Access: "rw"},
		"scale 7":     {ID: 3, Code: "c", Type: "value", Access: "rw", Scale: 7},
		"negative":    {ID: 3, Code: "c", Type: "value", Access: "rw", Scale: -1},
		"min>max":     {ID: 3, Code: "c", Type: "value", Access: "rw", Min: i(5), Max: i(1)},
		"step 0":      {ID: 3, Code: "c", Type: "value", Access: "rw", Step: i(0)},
		"huge":        {ID: 3, Code: "c", Type: "value", Access: "rw", Max: i(1e16)},
		"no range":    {ID: 3, Code: "c", Type: "enum", Access: "rw"},
		"dup range":   {ID: 3, Code: "c", Type: "enum", Access: "rw", Range: []string{"a", "a"}},
		"type":        {ID: 3, Code: "c", Type: "weird", Access: "rw"},
		"access":      {ID: 3, Code: "c", Type: "bool", Access: "x"},
		"dup id":      {ID: 2, Code: "other", Type: "bool", Access: "rw"},
		"dup code":    {ID: 9, Code: "bright", Type: "bool", Access: "rw"},
		"bool ranged": {ID: 3, Code: "c", Type: "bool", Access: "rw", Range: []string{"x"}},
	}
	for name, bad := range cases {
		if e := Validate([]DP{good, bad}); !errors.Is(e, domain.ErrInvalid) {
			t.Fatalf("%s accepted", name)
		}
	}
	many := make([]DP, MaxDPs+1)
	for n := range many {
		many[n] = DP{ID: n%255 + 1, Code: "c" + strconv.Itoa(n), Type: "bool", Access: "rw"}
	}
	if Validate(many) == nil {
		t.Fatal("too many data points accepted")
	}
}
