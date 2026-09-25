package simulation

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The virtual bridge must produce exactly what the ingest path understands.
func TestZ2MSimulatorParses(t *testing.T) {
	devices, _, e := zigbee2mqtt.ParseBridgeDevices(Z2MBridgeDevices())
	if e != nil || len(devices) != len(Z2MSwitches)+1 {
		t.Fatalf("devices: %v %+v", e, devices)
	}
	byIEEE := map[string]zigbee2mqtt.Device{}
	for _, d := range devices {
		byIEEE[d.IEEE] = d
	}
	// TS0012 lists right before left; gang 1 must still be the left output.
	if g := byIEEE["0xa4c1380000000002"].Gangs; len(g) != 2 || g[0].Property != "state_left" {
		t.Fatalf("TS0012 gangs: %+v", g)
	}
	msgs := Z2MMessages(Z2MSampleBase, 20, true)
	states := 0
	for _, m := range msgs {
		_, routed, e := zigbee2mqtt.Route(m.Topic)
		if e != nil {
			t.Fatalf("%s: %v", m.Topic, e)
		}
		if routed.Kind == zigbee2mqtt.State {
			ieee := zigbee2mqtt.DeviceIEEE(m.Payload)
			r, ok := zigbee2mqtt.ParseState(m.Payload, byIEEE[ieee], time.Now())
			if !ok || r.Source != "simulated" || len(r.Metrics) != len(byIEEE[ieee].Gangs)+1 {
				t.Fatalf("state %s: %+v", m.Payload, r)
			}
			states++
		}
		if routed.Kind == zigbee2mqtt.BridgeDevices && !m.Retain {
			t.Fatal("bridge/devices must be retained")
		}
	}
	if states != len(Z2MSwitches) || !strings.HasSuffix(msgs[0].Topic, "/bridge/state") {
		t.Fatalf("messages: %d states, first %s", states, msgs[0].Topic)
	}
	// The first switch's gang 1 is pressed every 20 steps.
	if Z2MStep(19)[0][0] == Z2MStep(20)[0][0] || Z2MStep(20)[0][0] != Z2MStep(39)[0][0] {
		t.Fatal("press pattern")
	}
}

// The whole virtual network parses, every actuator exposes something Aether can command, and the FakeBridge
// answers a valid /set with the new state (and nothing when told to drop commands).
func TestFakeBridgeAppliesCommands(t *testing.T) {
	devices, _, e := zigbee2mqtt.ParseBridgeDevices(Z2MBridgeDevicesAll())
	if e != nil || len(devices) != len(Z2MSwitches)+1+len(Z2MActuators)+len(Z2MSensors) {
		t.Fatalf("devices: %d %v", len(devices), e)
	}
	for _, d := range devices {
		for _, a := range Z2MActuators {
			if d.IEEE == a.IEEE && len(zigbee2mqtt.SettableFeatures(d.Exposes)) == 0 {
				t.Fatalf("%s exposes nothing settable: %s", a.Model, d.Exposes)
			}
		}
	}
	base := "aether/z2m/22222222-2222-4222-8222-222222222222"
	b := NewFakeBridge(base)
	if first := b.Step(0); !strings.HasSuffix(first[0].Topic, "/bridge/state") || len(first) < 2+len(Z2MSwitches)+2*len(Z2MActuators) {
		t.Fatalf("announce: %d messages", len(first))
	}
	light := Z2MActuators[0]
	out := b.Apply(base+"/"+light.IEEE+"/set", []byte(`{"brightness":128}`))
	if len(out) != 1 || out[0].Topic != base+"/"+light.Name || !strings.Contains(string(out[0].Payload), `"brightness":128`) {
		t.Fatalf("light: %v", out)
	}
	sw := Z2MSwitches[1]
	out = b.Apply(base+"/"+sw.IEEE+"/set", []byte(`{"state_left":"ON"}`))
	if len(out) != 1 || !strings.Contains(string(out[0].Payload), `"state_left":"ON"`) {
		t.Fatalf("switch: %v", out)
	}
	for _, f := range zigbee2mqtt.SettableFeatures(json.RawMessage(Z2MActuators[1].Exposes)) {
		if f.Property == "position" {
			if _, e := zigbee2mqtt.Validate(json.RawMessage(Z2MActuators[1].Exposes), "position", []byte(`50`)); e != nil {
				t.Fatal(e)
			}
		}
	}
	b.DropCommands = true
	if out := b.Apply(base+"/"+light.IEEE+"/set", []byte(`{"brightness":10}`)); out != nil {
		t.Fatalf("dropped command answered: %v", out)
	}
}

// Every simulated sensor parses into the category live ingest derives for it, from the same real definitions.
func TestZ2MSensorsCategories(t *testing.T) {
	devices, _, e := zigbee2mqtt.ParseBridgeDevices(Z2MBridgeDevicesAll())
	if e != nil {
		t.Fatal(e)
	}
	want := map[string]string{"WSDCGQ11LM": "environment", "MCCGQ11LM": "door", "RTCGQ11LM": "occupancy", "SJCGQ11LM": "leak", "TS0215A_sos": "sos", "E1743": "remote", "JTYJ-GD-01LM/BW": "hazard",
		"TS0011": "switch", "TS0012": "switch", "9290022166": "lighting", "TS130F": "cover", "YRD226HA2619": "lock", "TS0601_thermostat": "climate"}
	seen := 0
	for _, d := range devices {
		if c, ok := want[d.Model]; ok {
			seen++
			if d.Category != c || d.SOS != (c == "sos") {
				t.Fatalf("%s: category %q sos %v, want %q", d.Model, d.Category, d.SOS, c)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d models", seen, len(want))
	}
	b := NewFakeBridge("aether/z2m/x")
	if m := b.Set(Z2MSensors[1].IEEE, "contact", false); len(m) != 1 || !strings.Contains(string(m[0].Payload), `"contact":false`) {
		t.Fatalf("set: %v", m)
	}
	if m := Z2MPress("aether/z2m/x", Z2MSensors[4], "emergency"); !strings.Contains(string(m.Payload), `"action":"emergency"`) {
		t.Fatalf("press: %s", m.Payload)
	}
}
