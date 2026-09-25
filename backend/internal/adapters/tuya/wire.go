package tuya

import (
	"aether/backend/internal/domain"
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

// State converts raw data points ({1:true, 19:1234}) into property values ({"switch_1":"ON","power":123.4}),
// the shape a Zigbee2MQTT device reports, through the dp map. Data points the map does not know, and values of
// the wrong type, are left out.
func State(dpMap map[string]Ref, dps map[int]json.RawMessage) map[string]json.RawMessage {
	byDP := make(map[int]string, len(dpMap))
	for property, ref := range dpMap {
		byDP[ref.DP] = property
	}
	out := map[string]json.RawMessage{}
	for id, raw := range dps {
		property, ok := byDP[id]
		if !ok {
			continue
		}
		if v, ok := realValue(dpMap[property], raw); ok {
			out[property] = v
		}
	}
	return out
}

func realValue(ref Ref, raw json.RawMessage) (json.RawMessage, bool) {
	switch ref.Kind {
	case "onoff":
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return nil, false
		}
		if b {
			return json.RawMessage(`"ON"`), true
		}
		return json.RawMessage(`"OFF"`), true
	case "bool":
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return nil, false
		}
		return json.RawMessage(strconv.FormatBool(b)), true
	case "enumbool":
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		return json.RawMessage(strconv.FormatBool(s == ref.On)), true
	case "value", "bitmap":
		var n float64
		if json.Unmarshal(raw, &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) || !isNumber(raw) {
			return nil, false
		}
		div := math.Pow(10, float64(ref.Scale))
		if ref.Div > 0 {
			div *= ref.Div
		}
		return json.RawMessage(strconv.FormatFloat(n/div, 'f', -1, 64)), true
	case "enum", "string":
		var s string
		if json.Unmarshal(raw, &s) != nil || len(s) > 256 {
			return nil, false
		}
		b, _ := json.Marshal(s)
		return b, true
	}
	return nil, false
}

// Wire encodes a command's real value (already validated against the exposes) into the data point the device
// takes: {"dps":{"<id>":<raw>}}. Anything that does not fit the data point exactly is refused.
func Wire(dpMap map[string]Ref, property string, value json.RawMessage) (json.RawMessage, error) {
	ref, ok := dpMap[property]
	if !ok {
		return nil, domain.Because(domain.ErrInvalid, "unknown_property")
	}
	if !ref.Writable {
		return nil, domain.Because(domain.ErrInvalid, "not_settable")
	}
	var raw any
	switch ref.Kind {
	case "onoff":
		var s string
		if json.Unmarshal(value, &s) != nil || (s != "ON" && s != "OFF") {
			return nil, domain.Because(domain.ErrInvalid, "value")
		}
		raw = s == "ON"
	case "bool":
		var b bool
		if json.Unmarshal(value, &b) != nil {
			return nil, domain.Because(domain.ErrInvalid, "value")
		}
		raw = b
	case "value":
		var n float64
		if json.Unmarshal(value, &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) || !isNumber(value) {
			return nil, domain.Because(domain.ErrInvalid, "value")
		}
		scaled := n * math.Pow(10, float64(ref.Scale))
		r := math.Round(scaled)
		if math.Abs(scaled-r) > 1e-6*math.Max(1, math.Abs(scaled)) || math.Abs(r) > 1e15 {
			return nil, domain.Because(domain.ErrInvalid, "value")
		}
		v := int64(r)
		if ref.Min != nil && v < *ref.Min || ref.Max != nil && v > *ref.Max {
			return nil, domain.Because(domain.ErrInvalid, "value")
		}
		if ref.Step != nil && *ref.Step > 1 {
			base := int64(0)
			if ref.Min != nil {
				base = *ref.Min
			}
			if (v-base)%*ref.Step != 0 {
				return nil, domain.Because(domain.ErrInvalid, "value")
			}
		}
		raw = v
	case "enum":
		var s string
		if json.Unmarshal(value, &s) != nil || !contains(ref.Values, s) {
			return nil, domain.Because(domain.ErrInvalid, "value")
		}
		raw = s
	default:
		return nil, domain.Because(domain.ErrInvalid, "unsupported_feature")
	}
	b, e := json.Marshal(map[string]map[string]any{"dps": {strconv.Itoa(ref.DP): raw}})
	if e != nil {
		return nil, domain.Because(domain.ErrInvalid, "value")
	}
	return b, nil
}

func isNumber(v json.RawMessage) bool {
	b := bytes.TrimSpace(v)
	return len(b) > 0 && (b[0] == '-' || (b[0] >= '0' && b[0] <= '9'))
}
