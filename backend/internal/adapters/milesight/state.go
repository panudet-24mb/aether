package milesight

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strings"
)

// Milesight's payload codec (in the gateway) and Decode (in Aether) emit a flat object such as
// {"battery":100,"leakage_status":"leak"}. State turns it into the property form Zigbee2MQTT devices use, with a
// synthetic definition, so the shared pipeline applies: the status enums that mean a leak, someone moving or present,
// or light become the binaries Aether's leak, occupancy and daylight logic is built on, and every other number is kept
// under its own name. Milesight's field names and values are from its published decoders (Milesight-IoT/
// SensorDecoders, main branch, 2026-09); no decoder code is used here.

// binaryStatus maps a Milesight status field to the Zigbee2MQTT feature it means and its on/off values. Both
// spellings of the WS202's daylight are accepted: the decoder emits dim/bright, its README says dark/light. The codec
// on a UG65 (firmware 60.0.0.43) emits the same states as numbers instead: "pir": 0/1, "occupancy": 0/1, and the
// WS202's daylight as "light_status": 0/1, so 1 is on and 0 is off for every field here (statusOf).
var binaryStatus = map[string]struct {
	name    string
	on, off []string
}{
	"leakage_status": {"water_leak", []string{"leak"}, []string{"normal"}},
	"pir":            {"occupancy", []string{"trigger"}, []string{"normal", "idle"}},
	"daylight":       {"daylight", []string{"bright", "light"}, []string{"dim", "dark"}},
	"light_status":   {"daylight", []string{"bright", "light"}, []string{"dim", "dark"}},
	"occupancy":      {"presence", []string{"occupied"}, []string{"vacant"}},
}

// statusOf reads a status field as text (its on / off words), as a number (1 / 0) or as a boolean.
func statusOf(raw json.RawMessage, on, off []string) (bool, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return statusValue(s, on, off)
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, true
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && (n == 0 || n == 1) {
		return n == 1, true
	}
	return false, false
}

// units of the numeric fields Milesight decoders emit, by name.
var units = map[string]string{"battery": "%", "temperature": "°C", "humidity": "%", "nh3": "ppm", "h2s": "ppm", "co2": "ppm", "tvoc": "ppb",
	"pm2_5": "µg/m³", "pm10": "µg/m³", "pressure": "hPa", "illuminance": "lx", "distance": "mm"}

// renamed are Milesight numerics whose Aether (Zigbee2MQTT) name differs.
var renamed = map[string]string{"pm2_5": "pm25"}

// device describes the device, not what it measures: firmware, serial number, class, configuration echoed back in
// a downlink response. Never a reading.
var device = map[string]bool{"ipso_version": true, "hardware_version": true, "firmware_version": true, "tsl_version": true, "lorawan_class": true,
	"device_status": true, "reset_event": true, "sn": true, "sensor_id": true, "report_interval": true, "threshold_report_interval": true,
	"reboot": true, "query_life_remain": true, "query_device_status": true, "stop_alarming": true, "buzzer_enable": true, "led_indicator_enable": true,
	"find_device_enable": true, "find_device_time": true, "d2d_enable": true}

// State is the reading an uplink carries, as Zigbee2MQTT-style properties with their definition: its decoded values
// plus the radio figures when the envelope had them. Nested values are skipped.
func State(ev Event) (map[string]any, []zigbee2mqtt.Feature) {
	props := map[string]any{}
	features := []zigbee2mqtt.Feature{}
	add := func(name, typ string, v any) {
		if _, dup := props[name]; dup {
			return
		}
		props[name] = v
		f := zigbee2mqtt.Feature{Type: typ, Name: name, Property: name, Access: zigbee2mqtt.AccessState, Unit: units[name]}
		if typ == "binary" {
			f.ValueOn, f.ValueOff = json.RawMessage("true"), json.RawMessage("false")
		}
		features = append(features, f)
	}
	keys := make([]string, 0, len(ev.Object))
	for k := range ev.Object {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a stable definition order, so the MaxMetrics cap keeps the same metrics every time
	for _, key := range keys {
		raw := bytes.TrimSpace(ev.Object[key])
		if device[key] || !zigbee2mqtt.PropertyPattern.MatchString(key) || len(raw) == 0 {
			continue
		}
		var s string
		if status, ok := binaryStatus[key]; ok {
			if v, known := statusOf(raw, status.on, status.off); known {
				add(status.name, "binary", v)
			}
			continue
		}
		switch raw[0] {
		case '"':
			if json.Unmarshal(raw, &s) == nil && s != "" && s != "unknown" {
				add(key, "enum", s)
			}
		case 't', 'f':
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				add(key, "binary", b)
			}
		case '{', '[', 'n':
			// calibration results, alarm configuration, arrays: not a reading
		default:
			var v float64
			if json.Unmarshal(raw, &v) != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			if status, sentinel := gasSentinel(key, v); sentinel {
				add(key+"_sensor_status", "enum", status)
				continue
			}
			name := key
			if r, ok := renamed[key]; ok {
				name = r
			}
			add(name, "numeric", v)
		}
	}
	if len(props) > 0 && ev.RSSI != nil {
		add("rssi", "numeric", float64(*ev.RSSI))
		features[len(features)-1].Unit = "dBm"
		if ev.SNR != nil {
			add("snr", "numeric", *ev.SNR)
			features[len(features)-1].Unit = "dB"
		}
	}
	return props, features
}

// gasSentinel recognises a GS301 gas reading that is really a sensor status. The sensor sends 0xFFFE while it
// polarizes after power-on and 0xFFFF on a fault; Aether's own decoder turns them into a status, but the codec on a
// UG65 (seen on a GS301, firmware 1.1) divides them like a reading: 655.34 / 655.35 ppm, or 65.534 / 65.535 on the
// /1000 H2S channel. On /100 those values lie far above the sensors' 100 ppm range; on /1000 they are the uint16's
// top two codes, which no reading uses. The comparison is exact: JSON parsing and the division round alike.
func gasSentinel(key string, v float64) (string, bool) {
	scales := map[string][]float64{"nh3": {100}, "h2s": {100, 1000}}[key]
	for _, scale := range scales {
		switch {
		case v == 0xFFFE/scale:
			return "polarizing", true
		case v == 0xFFFF/scale:
			return "device error", true
		}
	}
	return "", false
}

func statusValue(s string, on, off []string) (bool, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case slices.Contains(on, s):
		return true, true
	case slices.Contains(off, s):
		return false, true
	}
	return false, false
}
