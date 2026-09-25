package zigbee2mqtt

import (
	"aether/backend/internal/simulation"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// device returns the parsed bridge/devices entry of a simulated model: the real zigbee-herdsman-converters
// definition, run through the same parser the ingest uses.
func device(t *testing.T, model string) Device {
	t.Helper()
	devices, _, e := ParseBridgeDevices(simulation.Z2MBridgeDevicesAll())
	if e != nil {
		t.Fatal(e)
	}
	for _, d := range devices {
		if d.Model == model {
			return d
		}
	}
	t.Fatalf("no simulated %s", model)
	return Device{}
}

func parse(t *testing.T, d Device, payload string) map[string]float64 {
	t.Helper()
	r, ok := ParseState([]byte(payload), d, time.Now())
	if !ok {
		t.Fatalf("%s: nothing parsed from %s", d.Model, payload)
	}
	return r.Metrics
}

func TestGenericEnvironment(t *testing.T) {
	d := device(t, "WSDCGQ11LM")
	r, ok := ParseState([]byte(`{"temperature":24.61,"humidity":55.2,"pressure":1008.4,"battery":91,"voltage":3005,"linkquality":87,"unknown_key":5}`), d, time.Now())
	if !ok || r.Kind != "environment" || r.Battery != 91 || r.Frames[0] != FrameState {
		t.Fatalf("reading: %+v", r)
	}
	m := r.Metrics
	if m["temperature"] != 24.61 || m["humidity"] != 55.2 || m["pressure"] != 1008.4 || m["voltage"] != 3.005 || m["linkquality"] != 87 {
		t.Fatalf("metrics: %v", m)
	}
	if _, ok := m["unknown_key"]; ok {
		t.Fatal("a key the definition does not expose was kept")
	}
	if m["battery"] != 91 || r.Temperature != 0 {
		t.Fatal("battery is also a metric (so 0 % counts) and temperature lives only in Metrics")
	}
}

// Zigbee2MQTT: contact=true means the magnet is at the sensor (closed). Aether's door metric is 1 = open.
func TestContactIsInvertedToDoor(t *testing.T) {
	d := device(t, "MCCGQ11LM")
	if d.Category != CategoryDoor {
		t.Fatalf("category %q", d.Category)
	}
	if m := parse(t, d, `{"contact":true}`); m["door"] != 0 {
		t.Fatalf("closed: %v", m)
	}
	if m := parse(t, d, `{"contact":false}`); m["door"] != 1 {
		t.Fatalf("open: %v", m)
	}
	if _, ok := parse(t, d, `{"contact":false}`)["contact"]; ok {
		t.Fatal("contact kept under its own name as well")
	}
}

func TestOccupancyLeakSmokeAndBattery(t *testing.T) {
	if m := parse(t, device(t, "RTCGQ11LM"), `{"occupancy":true,"illuminance":120}`); m["motion"] != 1 || m["illuminance"] != 120 {
		t.Fatalf("pir: %v", m)
	}
	if m := parse(t, device(t, "RTCGQ11LM"), `{"occupancy":false}`); m["motion"] != 0 {
		t.Fatalf("pir clear: %v", m)
	}
	if m := parse(t, device(t, "SJCGQ11LM"), `{"water_leak":true,"battery_low":false}`); m["leak"] != 1 || m["battery_low"] != 0 {
		t.Fatalf("leak: %v", m)
	}
	smoke := device(t, "JTYJ-GD-01LM/BW")
	if smoke.Category != CategoryHazard {
		t.Fatalf("smoke category %q", smoke.Category)
	}
	if m := parse(t, smoke, `{"smoke":true,"smoke_density":12,"tamper":false,"battery_low":true}`); m["smoke"] != 1 || m["smoke_density"] != 12 || m["tamper"] != 0 || m["battery_low"] != 1 {
		t.Fatalf("smoke: %v", m)
	}
}

func TestActionsAndSOS(t *testing.T) {
	sos, remote := device(t, "TS0215A_sos"), device(t, "E1743")
	if !sos.SOS || sos.Category != CategorySOS || remote.SOS || remote.Category != CategoryRemote {
		t.Fatalf("sos %v/%s remote %v/%s", sos.SOS, sos.Category, remote.SOS, remote.Category)
	}
	r, ok := ParseState([]byte(`{"action":"emergency","battery":80}`), sos, time.Now())
	if !ok || r.Action != "emergency" || !IsSOSAction(r.Action) {
		t.Fatalf("sos press: %+v", r)
	}
	r, ok = ParseState([]byte(`{"action":"brightness_move_up"}`), remote, time.Now())
	if !ok || r.Action != "brightness_move_up" || IsSOSAction(r.Action) {
		t.Fatalf("remote press: %+v", r)
	}
	// Zigbee2MQTT clears the action with an empty string after a press: not an event.
	if r, _ := ParseState([]byte(`{"action":"","battery":74}`), remote, time.Now()); r.Action != "" {
		t.Fatalf("empty action kept: %+v", r)
	}
	// An SOS binary (SEB01ZB, PB206) marks the definition as able to call for help; a settable one does not.
	for exposes, want := range map[string]bool{
		`[{"type":"binary","name":"sos","property":"sos","access":1,"value_on":true,"value_off":false}]`:       true,
		`[{"type":"binary","name":"sos","property":"sos","access":3,"value_on":true,"value_off":false}]`:       false,
		`[{"type":"enum","name":"action","property":"action","access":1,"values":["single","double","hold"]}]`: false,
		`[{"type":"enum","name":"action","property":"action","access":1,"values":["arm_all_zones","panic"]}]`:  true,
	} {
		f, _ := Features(json.RawMessage(exposes))
		if got := Summarize(f).SOS; got != want {
			t.Fatalf("%s: sos %v", exposes, got)
		}
	}
}

func TestActuatorReadingsKeepStates(t *testing.T) {
	lock := device(t, "YRD226HA2619")
	r, ok := ParseState([]byte(`{"state":"LOCK","lock_state":"locked","battery":87}`), lock, time.Now())
	if !ok || r.Kind != CategoryLock || r.Metrics["state"] != 1 || r.Values["lock_state"] != "locked" || r.Battery != 87 {
		t.Fatalf("lock: %+v", r)
	}
	cover := device(t, "TS130F")
	r, _ = ParseState([]byte(`{"position":40,"state":"STOP"}`), cover, time.Now())
	if r.Kind != CategoryCover || r.Metrics["position"] != 40 || r.Values["state"] != "STOP" {
		t.Fatalf("cover: %+v", r)
	}
	trv := device(t, "TS0601_thermostat")
	r, _ = ParseState([]byte(`{"occupied_heating_setpoint":21.5,"local_temperature":24.5,"system_mode":"heat","child_lock":"UNLOCK"}`), trv, time.Now())
	if r.Kind != CategoryClimate || r.Metrics["occupied_heating_setpoint"] != 21.5 || r.Metrics["local_temperature"] != 24.5 || r.Values["system_mode"] != "heat" || r.Metrics["child_lock"] != 0 {
		t.Fatalf("thermostat: %+v", r)
	}
	// Switches keep the per-gang metrics the switch events are built on.
	sw := device(t, "TS0012")
	if m := parse(t, sw, `{"state_left":"ON","state_right":"OFF"}`); m["sw1"] != 1 || m["sw2"] != 0 {
		t.Fatalf("switch: %v", m)
	}
}

func TestUnexpectedInputNeverCrashes(t *testing.T) {
	d := device(t, "WSDCGQ11LM")
	for _, payload := range []string{`{"temperature":"hot"}`, `{"temperature":null}`, `{"temperature":1e999}`, `[]`, `"x"`, `{"battery":-5}`, `{"humidity":{"a":1}}`} {
		r, ok := ParseState([]byte(payload), d, time.Now())
		if ok && (len(r.Metrics) > 0 || r.Battery != 0) {
			t.Fatalf("%s produced %+v", payload, r)
		}
	}
	// A definition with far more attributes than a reading may carry is bounded.
	var exposes []string
	payload := map[string]float64{}
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("attr_%03d", i)
		exposes = append(exposes, fmt.Sprintf(`{"type":"numeric","name":%q,"property":%q,"access":1}`, name, name))
		payload[name] = float64(i)
	}
	big := Device{Exposes: json.RawMessage("[" + strings.Join(exposes, ",") + "]")}
	b, _ := json.Marshal(payload)
	r, ok := ParseState(b, big, time.Now())
	if !ok || len(r.Metrics) != MaxMetrics {
		t.Fatalf("bounded metrics: %d", len(r.Metrics))
	}
}

// Without a definition (not interviewed yet, or too new for the bridge) the well-known keys still count.
func TestFallbackWithoutDefinition(t *testing.T) {
	r, ok := ParseState([]byte(`{"temperature":21.5,"contact":false,"battery":50,"mystery":1}`), Device{}, time.Now())
	if !ok || r.Metrics["temperature"] != 21.5 || r.Metrics["door"] != 1 || r.Battery != 50 {
		t.Fatalf("fallback: %+v", r)
	}
	if _, ok := r.Metrics["mystery"]; ok {
		t.Fatal("unknown key kept without a definition")
	}
}

// Zigbee2MQTT 1.x reports a raw illuminance next to illuminance_lux: the lux value wins whatever the order.
func TestIlluminanceLuxWins(t *testing.T) {
	for _, exposes := range []string{
		`[{"type":"numeric","name":"illuminance","property":"illuminance","access":1},{"type":"numeric","name":"illuminance_lux","property":"illuminance_lux","access":1,"unit":"lx"}]`,
		`[{"type":"numeric","name":"illuminance_lux","property":"illuminance_lux","access":1,"unit":"lx"},{"type":"numeric","name":"illuminance","property":"illuminance","access":1}]`,
	} {
		r, _ := ParseState([]byte(`{"illuminance":21000,"illuminance_lux":125}`), Device{Exposes: json.RawMessage(exposes)}, time.Now())
		if r.Metrics["illuminance"] != 125 {
			t.Fatalf("%s: %v", exposes, r.Metrics)
		}
	}
}

// numeric("gas") is a gas meter (m³, Develco ZHEMI101 / modernExtend gas metering) or a %LEL concentration, and
// numeric("vibration") a Tuya vibration strength: never the life-safety / motion binaries of the same name.
func TestNumericFeaturesNeverBecomeAlarms(t *testing.T) {
	meter := Device{Exposes: json.RawMessage(`[{"type":"numeric","name":"gas","property":"gas","access":1,"unit":"m³"},{"type":"numeric","name":"power","property":"power","access":1,"unit":"W"}]`)}
	f, _ := Features(meter.Exposes)
	if c := Summarize(f).Category(); c != CategoryMetering {
		t.Fatalf("gas meter category %q", c)
	}
	m := parse(t, meter, `{"gas":1.0,"power":5}`)
	if _, alarm := m["gas"]; alarm || m["gas_value"] != 1 {
		t.Fatalf("gas meter at 1.0 read as an alarm: %v", m)
	}
	lel := Device{Exposes: json.RawMessage(`[{"type":"numeric","name":"gas","property":"gas","access":1,"unit":"%LEL","value_min":0,"value_max":20}]`)}
	if m := parse(t, lel, `{"gas":10}`); m["gas_value"] != 10 {
		t.Fatalf("%%LEL reading: %v", m)
	} else if _, alarm := m["gas"]; alarm {
		t.Fatalf("%%LEL reading became an alarm flag: %v", m)
	}
	vib := Device{Exposes: json.RawMessage(`[{"type":"numeric","name":"vibration","property":"vibration","access":1}]`)}
	f, _ = Features(vib.Exposes)
	if c := Summarize(f).Category(); c == CategoryMotion || c == CategoryHazard {
		t.Fatalf("numeric vibration category %q", c)
	}
	if m := parse(t, vib, `{"vibration":1}`); m["vibration_value"] != 1 {
		t.Fatalf("numeric vibration: %v", m)
	} else if _, flag := m["vibration"]; flag {
		t.Fatalf("numeric vibration became the motion flag: %v", m)
	}
	// Without a definition a numeric gas value is a measurement too.
	if r, _ := ParseState([]byte(`{"gas":1,"voltage":3005}`), Device{}, time.Now()); r.Metrics["gas_value"] != 1 || r.Metrics["voltage"] != 3.005 {
		t.Fatalf("fallback: %v", r.Metrics)
	}
}

func TestOccupancyAndPresenceAgree(t *testing.T) {
	both := Device{Exposes: json.RawMessage(`[{"type":"binary","name":"presence","property":"presence","access":1,"value_on":true,"value_off":false},{"type":"binary","name":"occupancy","property":"occupancy","access":1,"value_on":true,"value_off":false}]`)}
	for _, payload := range []string{`{"presence":true,"occupancy":false}`, `{"presence":false,"occupancy":true}`} {
		if m := parse(t, both, payload); m["motion"] != 1 {
			t.Fatalf("%s: %v", payload, m)
		}
	}
}

// Safety flags survive the metric cap wherever they sit in the definition, and 0 % battery is a reading.
func TestSafetyMetricsSurviveTheCap(t *testing.T) {
	var exposes []string
	payload := map[string]any{}
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("attr_%03d", i)
		exposes = append(exposes, fmt.Sprintf(`{"type":"numeric","name":%q,"property":%q,"access":1}`, name, name))
		payload[name] = i
	}
	exposes = append(exposes, `{"type":"binary","name":"smoke","property":"smoke","access":1,"value_on":true,"value_off":false}`,
		`{"type":"numeric","name":"battery","property":"battery","access":1,"unit":"%"}`)
	payload["smoke"], payload["battery"] = true, 0
	b, _ := json.Marshal(payload)
	r, _ := ParseState(b, Device{Exposes: json.RawMessage("[" + strings.Join(exposes, ",") + "]")}, time.Now())
	if r.Metrics["smoke"] != 1 {
		t.Fatalf("smoke dropped by the cap: %d metrics", len(r.Metrics))
	}
	if v, ok := r.Metrics["battery"]; !ok || v != 0 {
		t.Fatalf("0 %% battery lost: %v", r.Metrics["battery"])
	}
}
