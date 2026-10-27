package milesight

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const gw = "66666666-6666-4666-8666-666666666666"

func TestRoute(t *testing.T) {
	cases := []struct {
		topic string
		kind  Kind
		eui   string
		err   bool
	}{
		{Prefix + gw + "/24e124136d490153/up", Up, "24e124136d490153", false},
		{Prefix + gw + "/24E124136D490153/up", Up, "24e124136d490153", false},
		{Prefix + gw + "/24e124136d490153/down", Ignore, "", false},
		{Prefix + gw + "/24e124136d490153/join", Ignore, "", false},
		{Prefix + gw + "/not-an-eui/up", Ignore, "", true},
		{Prefix + gw + "/$deveui/up", Ignore, "", true}, // the variable left unexpanded: a misconfigured gateway
		{Prefix + "not-a-uuid/24e124136d490153/up", Ignore, "", true},
		{Prefix + gw, Ignore, "", true},
		{"aether/z2m/" + gw + "/x", Ignore, "", true},
	}
	for _, c := range cases {
		g, m, e := Route(c.topic)
		if (e != nil) != c.err {
			t.Fatalf("%s: error %v", c.topic, e)
		}
		if e != nil {
			if !errors.Is(e, domain.ErrForbidden) {
				t.Fatalf("%s: %v", c.topic, e)
			}
			continue
		}
		if g != gw || m.Kind != c.kind || m.DevEUI != c.eui {
			t.Fatalf("%s: %s %+v", c.topic, g, m)
		}
	}
	// The topic handed to the gateway routes back to it once the gateway fills in $deveui.
	if g, m, e := Route(strings.Replace(UplinkTopic(gw), "$deveui", "24e124136d490153", 1)); e != nil || g != gw || m.Kind != Up {
		t.Fatalf("uplink topic: %v %+v", e, m)
	}
}

func frame(t *testing.T, h string) []byte {
	t.Helper()
	b, e := hex.DecodeString(strings.ReplaceAll(h, " ", ""))
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func decoded(t *testing.T, model, h string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for k, v := range Decode(model, frame(t, h)) {
		var x any
		_ = json.Unmarshal(v, &x)
		out[k] = x
	}
	return out
}

// The examples are the ones Milesight's READMEs publish for each model, with the codec output they document.
func TestDecode(t *testing.T) {
	cases := []struct {
		model, frame string
		want         map[string]any
	}{
		{"GS301", "017564 02671C01 036864 047D0000 057D0100", map[string]any{"battery": 100.0, "temperature": 28.4, "humidity": 50.0, "nh3": 0.0, "h2s": 0.01}},
		{"GS301", "067D0A00", map[string]any{"h2s": 0.01}},
		{"GS301", "047DFEFF 057DFFFF", map[string]any{"nh3_sensor_status": "polarizing", "h2s_sensor_status": "device error"}},
		{"GS301", "0267F6FF", map[string]any{"temperature": -1.0}},
		{"WS303", "017564 030000", map[string]any{"battery": 100.0, "leakage_status": "normal"}},
		{"WS303", "017564 030001", map[string]any{"battery": 100.0, "leakage_status": "leak"}},
		// The decoder emits dim/bright for daylight; the README's own example says "dark".
		{"WS202", "017510 030001 040000", map[string]any{"battery": 16.0, "pir": "trigger", "daylight": "dim"}},
		{"WS202", "030000 040001", map[string]any{"pir": "normal", "daylight": "bright"}},
		{"VS330", "017562 02820F00 038E01 048E01", map[string]any{"battery": 98.0, "distance": 15.0, "occupancy": "occupied", "calibration_status": "success"}},
		{"VS330", "038E00 048E00 02822C01", map[string]any{"occupancy": "vacant", "calibration_status": "failed", "distance": 300.0}},
		// 0x03 type 0x8E is VS330 occupancy, not a WS303 leak.
		{"WS303", "017564 038E01", map[string]any{"battery": 100.0}},
		// A device information frame (reset, versions, class, serial number) is read past, never kept.
		{"WS303", "ff0bff ff0101 ff166719d19725490012 ff090100 ff0a0101 ff0f00 ff fe ff 017563", map[string]any{"battery": 99.0}},
		{"WS202", "ff08 6719d1972549 017550", map[string]any{"battery": 80.0}},
		// An unknown channel ends the frame; what came before it is kept. A truncated value is dropped.
		{"WS303", "017564 99 99 030001", map[string]any{"battery": 100.0}},
		{"GS301", "017564 02671C", map[string]any{"battery": 100.0}},
		// Channel 0x03 means a leak on a WS303 and PIR on a WS202: without a model only the battery is read.
		{"", "017564 030001", map[string]any{"battery": 100.0}},
	}
	for _, c := range cases {
		got := decoded(t, c.model, c.frame)
		b1, _ := json.Marshal(got)
		b2, _ := json.Marshal(c.want)
		if string(b1) != string(b2) {
			t.Fatalf("%s %s: %s, want %s", c.model, c.frame, b1, b2)
		}
	}
}

// ug65Envelope is the UG65's own uplink format (no payload codec on the device).
func ug65Envelope(eui, name, data string) []byte {
	return []byte(`{"applicationID":1,"applicationName":"Building A","deviceName":"` + name + `","devEUI":"` + eui + `","time":"2026-10-26T12:39:05.547336Z",
"rxInfo":[{"mac":"24e124fffef021be","rssi":-97,"loRaSNR":-3.5,"name":"local_gateway"},{"mac":"24e124fffef021bf","rssi":-71,"loRaSNR":8.25}],
"txInfo":{"frequency":923200000,"dataRate":{"modulation":"LORA","bandwidth":125,"spreadFactor":7},"adr":true,"codeRate":"4/5"},"fCnt":42,"fPort":85,"data":"` + data + `"}`)
}

func reading(t *testing.T, ev Event) (map[string]float64, map[string]string, string) {
	t.Helper()
	props, features := State(ev)
	b, _ := json.Marshal(props)
	r, ok := zigbee2mqtt.ParseStateWith(b, zigbee2mqtt.Device{IEEE: ev.DevEUI}, features, time.Now())
	if !ok {
		t.Fatalf("no reading from %s", b)
	}
	return r.Metrics, r.Values, r.Kind
}

func TestUplinks(t *testing.T) {
	m := Message{Kind: Up, DevEUI: "24e124136d490153"}
	// Without a codec: the envelope's raw bytes, names and radio figures (the best gateway's).
	ev, e := ParseUplink(m, ug65Envelope("24E124136D490153", "WS303 toilet 2F", base64.StdEncoding.EncodeToString(frame(t, "017564 030001"))))
	if e != nil || ev.Object != nil || hex.EncodeToString(ev.Raw) != "017564030001" || *ev.FCnt != 42 || *ev.FPort != 85 || *ev.RSSI != -71 || *ev.SNR != 8.25 || ev.DeviceName != "WS303 toilet 2F" {
		t.Fatalf("envelope: %v %+v", e, ev)
	}
	if Model(ev) != "WS303" {
		t.Fatalf("model from name: %q", Model(ev))
	}
	ev.Object = Decode(Model(ev), ev.Raw)
	metrics, _, kind := reading(t, ev)
	if kind != zigbee2mqtt.CategoryLeak || metrics["leak"] != 1 || metrics["battery"] != 100 || metrics["rssi"] != -71 || metrics["snr"] != 8.25 {
		t.Fatalf("ws303: %s %v", kind, metrics)
	}

	// With a codec on the gateway: the decoded values only; the model shows in the fields.
	for _, c := range []struct {
		object, model string
		check         func(map[string]float64, map[string]string, string) bool
	}{
		{`{"battery":99,"leakage_status":"normal"}`, "WS303", func(m map[string]float64, _ map[string]string, k string) bool {
			return k == zigbee2mqtt.CategoryLeak && m["leak"] == 0 && m["battery"] == 99
		}},
		{`{"battery":16,"pir":"trigger","daylight":"dim"}`, "WS202", func(m map[string]float64, _ map[string]string, k string) bool {
			return k == zigbee2mqtt.CategoryOccupancy && m["motion"] == 1 && m["daylight"] == 0
		}},
		{`{"battery":16,"pir":"normal","daylight":"light"}`, "WS202", func(m map[string]float64, _ map[string]string, k string) bool {
			return m["motion"] == 0 && m["daylight"] == 1
		}},
		{`{"battery":100,"temperature":28.4,"humidity":50,"nh3":0,"h2s":0.01,"firmware_version":"v1.2","sn":"6719d19725490012","nh3_calibration_result":{"type":"factory"}}`, "GS301",
			func(m map[string]float64, _ map[string]string, k string) bool {
				_, fw := m["firmware_version"]
				return k == zigbee2mqtt.CategoryEnvironment && m["temperature"] == 28.4 && m["h2s"] == 0.01 && m["nh3"] == 0 && !fw
			}},
		{`{"battery":100,"nh3_sensor_status":"polarizing","h2s":0.02}`, "GS301", func(m map[string]float64, v map[string]string, _ string) bool {
			return v["nh3_sensor_status"] == "polarizing" && m["h2s"] == 0.02
		}},
		{`{"battery":98,"distance":15,"occupancy":"occupied","calibration_status":"success"}`, "VS330", func(m map[string]float64, v map[string]string, k string) bool {
			return k == zigbee2mqtt.CategoryOccupancy && m["motion"] == 1 && m["distance"] == 15 && v["calibration_status"] == "success"
		}},
		{`{"battery":98,"distance":1200,"occupancy":"vacant"}`, "VS330", func(m map[string]float64, _ map[string]string, _ string) bool {
			return m["motion"] == 0 && m["distance"] == 1200
		}},
	} {
		ev, e := ParseUplink(m, []byte(c.object))
		if e != nil || ev.Raw != nil || ev.RSSI != nil || Model(ev) != c.model {
			t.Fatalf("%s: %v %+v %q", c.object, e, ev, Model(ev))
		}
		if metrics, values, kind := reading(t, ev); !c.check(metrics, values, kind) {
			t.Fatalf("%s: %s %v %v", c.object, kind, metrics, values)
		}
	}
	_, features := State(Event{Object: Decode("GS301", frame(t, "047D0000 057D0100"))})
	for _, f := range features {
		if f.Unit != "ppm" {
			t.Fatalf("gas unit %+v", f)
		}
	}

	// An envelope for another device than the topic names, bad base64 or a payload that is not an object is refused.
	for _, b := range [][]byte{ug65Envelope("24e124136d490154", "", "AXVk"), ug65Envelope("24e124136d490153", "", "!!"), []byte(`[1]`), []byte(`"x"`)} {
		if _, e := ParseUplink(m, b); !errors.Is(e, domain.ErrInvalid) {
			t.Fatalf("%s: %v", b, e)
		}
	}
}
