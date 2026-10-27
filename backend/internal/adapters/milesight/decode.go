package milesight

import (
	"encoding/binary"
	"encoding/json"
	"strconv"
)

// Decode reads the raw application payload of a Milesight device into the same fields Milesight's payload codec
// emits, so a gateway without a codec configured still yields readings. It is written from the channel tables Milesight
// publishes for each model (Milesight-IoT/SensorDecoders READMEs; their decoder code, GPL-3.0, is not used).
//
// A frame is a sequence of [channel id][channel type][value], little-endian. Channels 0xFF/0xFE that are not listed
// below are configuration replies whose length Aether does not know, and an unknown channel ends the frame the same
// way: what was read before it is kept. The models share channel 0x03 type 0x00 (WS303 leak, WS202 PIR), so the model
// must be known; for any other model only the battery is read.
func Decode(model string, b []byte) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	set := func(k string, v any) {
		if _, dup := out[k]; !dup {
			out[k], _ = json.Marshal(v)
		}
	}
	gas := func(name string, raw uint16, scale float64) {
		switch raw {
		case 0xFFFE:
			set(name+"_sensor_status", "polarizing")
		case 0xFFFF:
			set(name+"_sensor_status", "device error")
		default:
			set(name, round(float64(raw)/scale, 3))
		}
	}
	for i := 0; i+2 <= len(b); {
		ch, typ, v := b[i], b[i+1], b[i+2:]
		n := 0 // bytes of value
		switch {
		case ch == 0x01 && typ == 0x75 && len(v) >= 1:
			n = 1
			set("battery", int(v[0]))
		// Device information: versions, class, status, reset, serial number. Read past, never a reading.
		case ch == 0xFF && (typ == 0x01 || typ == 0x0F || typ == 0x0B || typ == 0xFE):
			n = 1
		case ch == 0xFF && (typ == 0x09 || typ == 0x0A || typ == 0xFF):
			n = 2
		case ch == 0xFF && typ == 0x16:
			n = 8
		case ch == 0xFF && typ == 0x08:
			n = 6
		case model == "GS301" && ch == 0x02 && typ == 0x67 && len(v) >= 2:
			n = 2
			set("temperature", round(float64(int16(binary.LittleEndian.Uint16(v)))/10, 1))
		case model == "GS301" && ch == 0x03 && typ == 0x68 && len(v) >= 1:
			n = 1
			set("humidity", float64(v[0])/2)
		case model == "GS301" && ch == 0x04 && typ == 0x7D && len(v) >= 2:
			n = 2
			gas("nh3", binary.LittleEndian.Uint16(v), 100)
		case model == "GS301" && ch == 0x05 && typ == 0x7D && len(v) >= 2:
			n = 2
			gas("h2s", binary.LittleEndian.Uint16(v), 100)
		case model == "GS301" && ch == 0x06 && typ == 0x7D && len(v) >= 2: // higher resolution H2S (firmware 1.2+)
			n = 2
			gas("h2s", binary.LittleEndian.Uint16(v), 1000)
		case model == "GS301" && ch == 0x07 && typ == 0xEA: // calibration result
			n = 5
		case model == "GS301" && ch == 0xFF && typ == 0x7C: // sensor id
			n = 43
		case model == "WS303" && ch == 0x03 && typ == 0x00 && len(v) >= 1:
			n = 1
			set("leakage_status", pick(v[0], "normal", "leak"))
		case model == "WS202" && ch == 0x03 && typ == 0x00 && len(v) >= 1:
			n = 1
			set("pir", pick(v[0], "normal", "trigger"))
		case model == "WS202" && ch == 0x04 && typ == 0x00 && len(v) >= 1:
			n = 1
			set("daylight", pick(v[0], "dim", "bright"))
		case model == "VS330" && ch == 0x02 && typ == 0x82 && len(v) >= 2:
			n = 2
			set("distance", int(binary.LittleEndian.Uint16(v)))
		case model == "VS330" && ch == 0x03 && typ == 0x8E && len(v) >= 1:
			n = 1
			set("occupancy", pick(v[0], "vacant", "occupied"))
		case model == "VS330" && ch == 0x04 && typ == 0x8E && len(v) >= 1:
			n = 1
			set("calibration_status", pick(v[0], "failed", "success"))
		}
		if n == 0 || len(v) < n {
			break
		}
		i += 2 + n
	}
	return out
}

func pick(b byte, values ...string) string {
	if int(b) < len(values) {
		return values[b]
	}
	return "unknown"
}

func round(v float64, places int) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', places, 64), 64)
	return f
}
