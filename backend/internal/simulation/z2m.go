package simulation

import (
	"encoding/json"
	"fmt"
)

// Virtual Zigbee2MQTT bridge: what a coordinator with three Tuya no-neutral touch wall switches (1, 2 and 4 gang)
// and one unsupported device would publish. The shapes follow zigbee2mqtt.io (bridge/devices, device state,
// availability); they are synthetic until a capture from real hardware replaces them in the golden tests.

// Z2MSwitch is one simulated wall switch: its IEEE address, friendly name and the state property of each gang.
type Z2MSwitch struct {
	IEEE, Name, Model, ModelID, Manufacturer string
	Endpoints                                []string // "" for a single output, else l1..l4 or left/center/right
}

func (s Z2MSwitch) property(i int) string {
	if s.Endpoints[i] == "" {
		return "state"
	}
	return "state_" + s.Endpoints[i]
}

var Z2MSwitches = []Z2MSwitch{
	{IEEE: "0xa4c1380000000001", Name: "0xa4c1380000000001", Model: "TS0011", ModelID: "TS0011", Manufacturer: "_TZ3000_simone", Endpoints: []string{""}},
	{IEEE: "0xa4c1380000000002", Name: "ห้องประชุม/ไฟ", Model: "TS0012", ModelID: "TS0012", Manufacturer: "_TZ3000_simtwo", Endpoints: []string{"right", "left"}},
	{IEEE: "0xa4c1380000000004", Name: "โถงหน้า", Model: "TS0014", ModelID: "TS0014", Manufacturer: "_TZ3000_simfour", Endpoints: []string{"l1", "l2", "l3", "l4"}},
}

// Z2MBridgeDevices is the retained bridge/devices document: the coordinator, the switches and an unsupported
// device with no definition.
func Z2MBridgeDevices() []byte {
	type feature struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Property string `json:"property"`
		Endpoint string `json:"endpoint,omitempty"`
		Access   int    `json:"access"`
		ValueOn  string `json:"value_on"`
		ValueOff string `json:"value_off"`
	}
	type expose struct {
		Type     string    `json:"type"`
		Name     string    `json:"name,omitempty"`
		Property string    `json:"property,omitempty"`
		Endpoint string    `json:"endpoint,omitempty"`
		Access   int       `json:"access,omitempty"`
		Features []feature `json:"features,omitempty"`
	}
	devices := []map[string]any{{
		"ieee_address": "0x00124b0000000000", "type": "Coordinator", "friendly_name": "Coordinator", "supported": true, "network_address": 0,
	}}
	for _, s := range Z2MSwitches {
		exposes := []expose{}
		for i, ep := range s.Endpoints {
			exposes = append(exposes, expose{Type: "switch", Endpoint: ep, Features: []feature{{Type: "binary", Name: "state", Property: s.property(i), Endpoint: ep, Access: 7, ValueOn: "ON", ValueOff: "OFF"}}})
		}
		exposes = append(exposes, expose{Type: "numeric", Name: "linkquality", Property: "linkquality", Access: 1})
		devices = append(devices, map[string]any{
			"ieee_address": s.IEEE, "type": "EndDevice", "friendly_name": s.Name, "supported": true, "network_address": 1000,
			"model_id": s.ModelID, "manufacturer": s.Manufacturer, "power_source": "Mains (single phase)", "interview_completed": true, "disabled": false,
			"definition": map[string]any{"model": s.Model, "vendor": "Tuya", "description": "Smart light switch", "exposes": exposes},
		})
	}
	devices = append(devices, map[string]any{
		"ieee_address": "0x00158d00000000ff", "type": "EndDevice", "friendly_name": "unknown-remote", "supported": false,
		"model_id": "lumi.unknown", "manufacturer": "LUMI", "power_source": "Battery", "definition": nil,
	})
	b, _ := json.Marshal(devices)
	return b
}

// Z2MState is one switch's state message; gang i (0-based) is ON when on[i]. A simulated message carries
// aether_source so it can never be mistaken for hardware.
func Z2MState(s Z2MSwitch, on []bool, linkquality int) []byte {
	m := map[string]any{"linkquality": linkquality, "aether_source": "simulated",
		"device": map[string]any{"ieeeAddr": s.IEEE, "friendlyName": s.Name, "model": s.Model}}
	for i := range s.Endpoints {
		state := "OFF"
		if i < len(on) && on[i] {
			state = "ON"
		}
		m[s.property(i)] = state
	}
	b, _ := json.Marshal(m)
	return b
}

// Z2MOnline is an availability or bridge/state payload.
func Z2MOnline(online bool) []byte {
	if online {
		return []byte(`{"state":"online"}`)
	}
	return []byte(`{"state":"offline"}`)
}

// Z2MStep is the simulated state of every switch at a step: gang 1 of the first switch is "pressed on the wall"
// (toggled) every 20 steps, the others hold a fixed pattern.
func Z2MStep(step int) [][]bool {
	out := make([][]bool, len(Z2MSwitches))
	for i, s := range Z2MSwitches {
		on := make([]bool, len(s.Endpoints))
		for g := range on {
			on[g] = (i+g)%2 == 0
		}
		if i == 0 {
			on[0] = (step/20)%2 == 1
		}
		out[i] = on
	}
	return out
}

// Z2MTopic is a device topic under a gateway's base topic.
func Z2MTopic(base string, s Z2MSwitch, suffix string) string {
	if suffix == "" {
		return fmt.Sprintf("%s/%s", base, s.Name)
	}
	return fmt.Sprintf("%s/%s/%s", base, s.Name, suffix)
}

// Z2MSampleBase is the base topic sample-z2m uses when none is given.
const Z2MSampleBase = "aether/z2m/00000000-0000-4000-8000-000000000000"

// Z2MMessage is one publish of the virtual bridge.
type Z2MMessage struct {
	Topic   string
	Payload []byte
	Retain  bool
}

// Z2MMessages is what the virtual bridge publishes at a step. On start (announce) it first publishes the
// retained bridge/state, bridge/devices and each device's availability, as Zigbee2MQTT does when it connects.
func Z2MMessages(base string, step int, announce bool) []Z2MMessage {
	out := []Z2MMessage{}
	if announce {
		out = append(out, Z2MMessage{Topic: base + "/bridge/state", Payload: Z2MOnline(true), Retain: true},
			Z2MMessage{Topic: base + "/bridge/devices", Payload: Z2MBridgeDevices(), Retain: true})
		for _, s := range Z2MSwitches {
			out = append(out, Z2MMessage{Topic: Z2MTopic(base, s, "availability"), Payload: Z2MOnline(true), Retain: true})
		}
	}
	states := Z2MStep(step)
	for i, s := range Z2MSwitches {
		out = append(out, Z2MMessage{Topic: Z2MTopic(base, s, ""), Payload: Z2MState(s, states[i], 90+i*20)})
	}
	return out
}
