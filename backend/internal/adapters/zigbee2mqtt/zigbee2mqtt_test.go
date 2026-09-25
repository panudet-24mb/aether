package zigbee2mqtt

import (
	"aether/backend/internal/domain"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const gw = "8f14e45f-ceea-467a-a1f3-0a1b2c3d4e5f"

func TestRoute(t *testing.T) {
	base := Prefix + gw
	cases := []struct {
		topic  string
		kind   Kind
		device string
	}{
		{base + "/bridge/devices", BridgeDevices, ""},
		{base + "/bridge/state", BridgeState, ""},
		{base + "/bridge/info", BridgeInfo, ""},
		{base + "/bridge/health", Heartbeat, ""},
		{base + "/bridge/definitions", Ignore, ""},
		{base + "/bridge/request/permit_join", Ignore, ""},
		{base + "/bridge/logging", Ignore, ""},
		{base + "/0xa4c1380000000001", State, "0xa4c1380000000001"},
		{base + "/ห้องประชุม/ไฟ", State, "ห้องประชุม/ไฟ"},
		{base + "/ห้องประชุม/ไฟ/availability", Availability, "ห้องประชุม/ไฟ"},
		{base + "/0xa4c1380000000001/set", Ignore, ""},
		{base + "/0xa4c1380000000001/set/state", Ignore, ""},
		{base + "/0xa4c1380000000001/get", Ignore, ""},
	}
	for _, c := range cases {
		g, m, e := Route(c.topic)
		if e != nil || g != gw || m.Kind != c.kind || m.Device != c.device || m.Topic != c.topic {
			t.Fatalf("%s: %q %+v %v", c.topic, g, m, e)
		}
	}
	for _, bad := range []string{"/aether/gateways/" + gw + "/status", Prefix + "not-a-uuid/x", Prefix + gw, Prefix + gw + "/", Prefix + gw + "//availability", base + "/" + strings.Repeat("a", 300)} {
		if _, _, e := Route(bad); !errors.Is(e, domain.ErrForbidden) {
			t.Fatalf("%q accepted: %v", bad, e)
		}
	}
}

func TestParseBridgeDevices(t *testing.T) {
	b, e := os.ReadFile("testdata/bridge_devices_tuya.json")
	if e != nil {
		t.Fatal(e)
	}
	devices, truncated, e := ParseBridgeDevices(b)
	if e != nil || truncated {
		t.Fatal(e, truncated)
	}
	// Coordinator, the disabled device and the malformed IEEE are skipped.
	if len(devices) != 3 {
		t.Fatalf("devices: %+v", devices)
	}
	two := devices[0]
	if two.IEEE != "0xa4c138f0e1d2c3b4" || two.FriendlyName != "ห้องประชุม/ไฟ" || two.Model != "TS0012" || two.ModelID != "TS0012" || !two.Supported {
		t.Fatalf("TS0012: %+v", two)
	}
	if len(two.Gangs) != 2 || two.Gangs[0] != (Gang{Gang: 1, Property: "state_left", Endpoint: "left", Settable: true}) || two.Gangs[1].Property != "state_right" {
		t.Fatalf("TS0012 gangs: %+v", two.Gangs)
	}
	// Exposes out of order are put in wall order; access without the SET bit is not settable.
	three := devices[1]
	if len(three.Gangs) != 3 || three.Gangs[0].Property != "state_l1" || three.Gangs[1].Property != "state_l2" || three.Gangs[2].Property != "state_l3" || three.Gangs[1].Settable || !three.Gangs[0].Settable {
		t.Fatalf("TS0601 gangs: %+v", three.Gangs)
	}
	if sensor := devices[2]; len(sensor.Gangs) != 0 || sensor.Model != "WSDCGQ11LM" {
		t.Fatalf("sensor: %+v", sensor)
	}
	if _, _, e := ParseBridgeDevices([]byte(`{"not":"a list"}`)); e == nil {
		t.Fatal("object accepted as device list")
	}
}

func TestParseState(t *testing.T) {
	d := Device{IEEE: "0xa4c138f0e1d2c3b4", Model: "TS0012", Gangs: []Gang{{Gang: 1, Property: "state_left"}, {Gang: 2, Property: "state_right"}}}
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	r, ok := ParseState([]byte(`{"state_left":"ON","state_right":"off","linkquality":132,"power_on_behavior":"previous"}`), d, at)
	if !ok || r.Kind != KindSwitch || r.Metrics["sw1"] != 1 || r.Metrics["sw2"] != 0 || r.Metrics["linkquality"] != 132 || r.Model != "TS0012" || r.Source != "gateway" || !r.ReceivedAt.Equal(at) || r.Frames[0] != FrameState {
		t.Fatalf("reading: %+v %v", r, ok)
	}
	// A partial update of another attribute carries no gang state.
	if _, ok := ParseState([]byte(`{"power_on_behavior":"on"}`), d, at); ok {
		t.Fatal("reading without gang state")
	}
	if _, ok := ParseState([]byte(`["state_left"]`), d, at); ok {
		t.Fatal("array accepted")
	}
	if r, ok := ParseState([]byte(`{"state_left":"ON","aether_source":"simulated"}`), d, at); !ok || r.Source != "simulated" {
		t.Fatalf("simulated: %+v", r)
	}
	if got := DeviceIEEE([]byte(`{"device":{"ieeeAddr":"0xA4C138F0E1D2C3B4"}}`)); got != "0xa4c138f0e1d2c3b4" {
		t.Fatalf("device ieee: %q", got)
	}
	if got := DeviceIEEE([]byte(`{"device":{"ieeeAddr":"nope"}}`)); got != "" {
		t.Fatalf("bad ieee accepted: %q", got)
	}
}

func TestParseOnline(t *testing.T) {
	for payload, want := range map[string]bool{`{"state":"online"}`: true, `{"state":"offline"}`: false, `online`: true, `offline`: false, `"online"`: true} {
		if got, ok := ParseOnline([]byte(payload)); !ok || got != want {
			t.Fatalf("%s: %v %v", payload, got, ok)
		}
	}
	if _, ok := ParseOnline([]byte(`{"state":"maybe"}`)); ok {
		t.Fatal("unknown state accepted")
	}
	if v := ParseBridgeVersion([]byte(`{"version":"2.1.1","commit":"abc"}`)); v != "2.1.1" {
		t.Fatalf("version %q", v)
	}
}

func TestConfigSnippet(t *testing.T) {
	yaml := ConfigSnippet(true, "mqtt.example.org", 8883, gw, "pa'ss")
	for _, want := range []string{"requires Zigbee2MQTT 2.x", "health:\n  interval: 10", "server: mqtts://mqtt.example.org:8883", "base_topic: aether/z2m/" + gw, "user: gw-" + gw, "client_id: gw-" + gw, "password: 'pa''ss'", "reject_unauthorized: true", "homeassistant:\n  enabled: false", "availability:\n  enabled: true"} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("missing %q in\n%s", want, yaml)
		}
	}
	if plain := ConfigSnippet(false, "localhost", 1883, gw, "x"); !strings.Contains(plain, "server: mqtt://localhost:1883") || strings.Contains(plain, "ca:") {
		t.Fatalf("plaintext snippet:\n%s", plain)
	}
}

func FuzzZigbee2MQTT(f *testing.F) {
	b, _ := os.ReadFile("testdata/bridge_devices_tuya.json")
	f.Add(string(b), `{"state_left":"ON"}`, Prefix+gw+"/x/availability")
	f.Add(`[]`, `online`, Prefix+gw+"/bridge/devices")
	f.Fuzz(func(t *testing.T, devices, state, topic string) {
		list, _, _ := ParseBridgeDevices([]byte(devices))
		if len(list) > MaxDevices {
			t.Fatal("device cap exceeded")
		}
		for _, d := range list {
			if !ValidIEEE(d.IEEE) || len(d.Gangs) > 4 {
				t.Fatalf("invalid device kept: %+v", d)
			}
			ParseState([]byte(state), d, time.Now())
		}
		ParseOnline([]byte(state))
		DeviceIEEE([]byte(state))
		Route(topic)
	})
}

// A list longer than MaxDevices is cut and says so; NUL padding (Tuya model ids) never reaches the database.
func TestParseBridgeDevicesLimitsAndNUL(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i <= MaxDevices; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"ieee_address":"0x%016x","type":"EndDevice","friendly_name":"d%d","model_id":"TS0011\u0000\u0000","manufacturer":"_TZ3000\u0000"}`, i+1, i)
	}
	b.WriteString("]")
	devices, truncated, e := ParseBridgeDevices([]byte(b.String()))
	if e != nil || !truncated || len(devices) != MaxDevices {
		t.Fatalf("truncation: %v %v %d", e, truncated, len(devices))
	}
	if devices[0].ModelID != "TS0011" || devices[0].Manufacturer != "_TZ3000" {
		t.Fatalf("NUL kept: %q %q", devices[0].ModelID, devices[0].Manufacturer)
	}
	exact := strings.Replace(b.String(), fmt.Sprintf(`,{"ieee_address":"0x%016x"`, MaxDevices+1), `,{"ieee_address":"bad"`, 1)
	if _, truncated, _ := ParseBridgeDevices([]byte(exact)); truncated {
		t.Fatal("exactly MaxDevices valid devices reported as truncated")
	}
}
