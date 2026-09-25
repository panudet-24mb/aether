package simulation

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
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
