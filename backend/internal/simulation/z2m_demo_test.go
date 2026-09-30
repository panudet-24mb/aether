package simulation

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"bytes"
	"encoding/json"
	"regexp"
	"testing"
	"time"
)

// The demo coordinator publishes real zigbee-herdsman-converters definitions, and every state it reports stays
// inside what those definitions expose: the keys, the numeric ranges, the enum values and the binary on/off values.
func TestTwinZigbeeFollowsDefinitions(t *testing.T) {
	x := TwinExtended()
	if len(x.ZigbeeDev) != 52 {
		t.Fatalf("devices: %d", len(x.ZigbeeDev))
	}
	ieee := regexp.MustCompile(`^0x[0-9a-f]{16}$`)
	seen := map[string]bool{}
	for _, d := range x.ZigbeeDev {
		if !ieee.MatchString(d.IEEE) || seen[d.IEEE] || seen[d.Name] {
			t.Fatalf("identity %q %q", d.IEEE, d.Name)
		}
		seen[d.IEEE], seen[d.Name] = true, true
	}
	body := TwinZigbeeBridgeDevices(x)
	if bytes.IndexByte(body, 0) >= 0 || bytes.Contains(body, []byte(`\u0000`)) {
		t.Fatal("bridge/devices carries a NUL: the ingest refuses it")
	}
	devices, truncated, e := zigbee2mqtt.ParseBridgeDevices(body)
	if e != nil || truncated || len(devices) != len(x.ZigbeeDev) { // the coordinator itself is not a device
		t.Fatalf("bridge/devices: %v %v %d", e, truncated, len(devices))
	}
	at := time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC) // 10:00 in Bangkok, the day shift
	for _, d := range x.ZigbeeDev {
		features, e := zigbee2mqtt.Features(d.Def.Exposes)
		if e != nil {
			t.Fatalf("%s exposes: %v", d.Def.Model, e)
		}
		byProp := map[string][]zigbee2mqtt.Feature{}
		for _, f := range features {
			byProp[f.Property] = append(byProp[f.Property], f)
		}
		for step := 0; step < TwinLoopSteps; step += 7 {
			state := TwinZigbeeState(d, step, at.Add(time.Duration(step)*TwinStepSec*time.Second), TwinOverrides{})
			for k, v := range state {
				fs, ok := byProp[k]
				if !ok {
					if k == "linkquality" || k == "last_seen" || k == "battery" {
						continue
					}
					t.Fatalf("%s %s: %q is not in its exposes", d.Def.Model, d.Name, k)
				}
				if !fitsAny(fs, v) {
					t.Fatalf("%s %s step %d: %s=%v outside its exposes", d.Def.Model, d.Name, step, k, v)
				}
			}
		}
	}
}

func fitsAny(fs []zigbee2mqtt.Feature, v any) bool {
	raw, _ := json.Marshal(v)
	for _, f := range fs {
		switch f.Type {
		case "binary":
			if bytes.Equal(raw, f.ValueOn) || bytes.Equal(raw, f.ValueOff) {
				return true
			}
		case "numeric":
			n, ok := v.(float64)
			if i, isInt := v.(int); isInt {
				n, ok = float64(i), true
			}
			if ok && (f.ValueMin == nil || n >= *f.ValueMin) && (f.ValueMax == nil || n <= *f.ValueMax) {
				return true
			}
		case "enum":
			for _, e := range f.Values {
				if bytes.Equal(raw, e) {
					return true
				}
			}
		default:
			return true
		}
	}
	return false
}

// The script is deterministic: the same step and time give the same report.
func TestTwinZigbeeDeterministic(t *testing.T) {
	x := TwinExtended()
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, d := range x.ZigbeeDev {
		a, _ := json.Marshal(TwinZigbeeState(d, 123, at, TwinOverrides{}))
		b, _ := json.Marshal(TwinZigbeeState(d, 123, at, TwinOverrides{}))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: %s != %s", d.Name, a, b)
		}
	}
}
