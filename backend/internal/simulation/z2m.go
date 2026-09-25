package simulation

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

// Z2MActuator is a simulated non-switch device: its definition's exposes (as Zigbee2MQTT publishes them) and its
// state when the bridge starts.
type Z2MActuator struct {
	IEEE, Name, Model, Vendor, ModelID, Manufacturer, PowerSource string
	Description                                                   string
	Exposes                                                       string
	Initial                                                       map[string]any
}

// Z2MActuators are one device of each kind Aether can command besides switches, with realistic exposes taken
// from their zigbee2mqtt.io device pages (synthetic until real captures replace them).
var Z2MActuators = []Z2MActuator{
	{IEEE: "0x0017880100000001", Name: "ห้องประชุม/โคมไฟสี", Model: "9290022166", Vendor: "Philips", ModelID: "LCA001", Manufacturer: "Signify Netherlands B.V.", PowerSource: "Mains (single phase)",
		Exposes: `[{"type":"light","features":[
  {"type":"binary","name":"state","label":"State","property":"state","access":7,"value_on":"ON","value_off":"OFF","value_toggle":"TOGGLE"},
  {"type":"numeric","name":"brightness","label":"Brightness","property":"brightness","access":7,"value_min":0,"value_max":254},
  {"type":"numeric","name":"color_temp","label":"Color temp","property":"color_temp","access":7,"unit":"mired","value_min":153,"value_max":500},
  {"type":"composite","name":"color_xy","label":"Color (X/Y)","property":"color","access":7,"features":[
    {"type":"numeric","name":"x","label":"X","property":"x","access":7},{"type":"numeric","name":"y","label":"Y","property":"y","access":7}]},
  {"type":"composite","name":"color_hs","label":"Color (HS)","property":"color","access":7,"features":[
    {"type":"numeric","name":"hue","label":"Hue","property":"hue","access":7},{"type":"numeric","name":"saturation","label":"Saturation","property":"saturation","access":7}]}]},
 {"type":"enum","name":"effect","label":"Effect","property":"effect","access":2,"values":["blink","breathe","okay","channel_change","finish_effect","stop_effect"]},
 {"type":"numeric","name":"linkquality","label":"Linkquality","property":"linkquality","access":1,"unit":"lqi","value_min":0,"value_max":255}]`,
		Initial: map[string]any{"state": "OFF", "brightness": 0, "color_temp": 370, "color": map[string]any{"x": 0.4573, "y": 0.41}}},
	{IEEE: "0xa4c1380000000010", Name: "ม่านห้องผู้บริหาร", Model: "TS130F", Vendor: "Tuya", ModelID: "TS130F", Manufacturer: "_TZ3000_simcurtain", PowerSource: "Mains (single phase)",
		Exposes: `[{"type":"cover","features":[
  {"type":"enum","name":"state","label":"State","property":"state","access":3,"values":["OPEN","CLOSE","STOP"]},
  {"type":"numeric","name":"position","label":"Position","property":"position","access":7,"unit":"%","value_min":0,"value_max":100}]},
 {"type":"numeric","name":"linkquality","label":"Linkquality","property":"linkquality","access":1,"unit":"lqi","value_min":0,"value_max":255}]`,
		Initial: map[string]any{"position": 0, "state": "CLOSE"}},
	{IEEE: "0x000d6f0000000020", Name: "ประตูห้องเซิร์ฟเวอร์", Model: "YRD226HA2619", Vendor: "Yale", ModelID: "YRD226 TSDB", Manufacturer: "Yale", PowerSource: "Battery",
		Exposes: `[{"type":"lock","features":[
  {"type":"binary","name":"state","label":"State","property":"state","access":7,"value_on":"LOCK","value_off":"UNLOCK"},
  {"type":"enum","name":"lock_state","label":"Lock state","property":"lock_state","access":1,"values":["not_fully_locked","locked","unlocked"]}]},
 {"type":"numeric","name":"battery","label":"Battery","property":"battery","access":5,"unit":"%","value_min":0,"value_max":100},
 {"type":"numeric","name":"linkquality","label":"Linkquality","property":"linkquality","access":1,"unit":"lqi","value_min":0,"value_max":255}]`,
		Initial: map[string]any{"state": "LOCK", "lock_state": "locked", "battery": 87}},
	{IEEE: "0xa4c1380000000030", Name: "หัววาล์วหม้อน้ำ", Model: "TS0601_thermostat", Vendor: "Tuya", ModelID: "TS0601", Manufacturer: "_TZE200_simtrv", PowerSource: "Battery",
		Exposes: `[{"type":"climate","features":[
  {"type":"numeric","name":"occupied_heating_setpoint","label":"Occupied heating setpoint","property":"occupied_heating_setpoint","access":7,"unit":"°C","value_min":5,"value_max":35,"value_step":0.5},
  {"type":"numeric","name":"local_temperature","label":"Local temperature","property":"local_temperature","access":5,"unit":"°C"},
  {"type":"enum","name":"system_mode","label":"System mode","property":"system_mode","access":7,"values":["off","heat","auto"]}]},
 {"type":"binary","name":"child_lock","label":"Child lock","property":"child_lock","access":7,"value_on":"LOCK","value_off":"UNLOCK"},
 {"type":"numeric","name":"linkquality","label":"Linkquality","property":"linkquality","access":1,"unit":"lqi","value_min":0,"value_max":255}]`,
		Initial: map[string]any{"occupied_heating_setpoint": 20, "local_temperature": 24.5, "system_mode": "heat", "child_lock": "UNLOCK"}},
}

// Z2MSensors are battery sensors, buttons and a detector, one of each kind the generic ingest maps, with the
// exposes copied from zigbee-herdsman-converters 26.112.0 (the definitions Zigbee2MQTT publishes for them;
// config-only features left out). Initial is the state each reports on its own; a button's action is only in
// the message of a press (Z2MPress).
var Z2MSensors = []Z2MActuator{
	{IEEE: "0x00158d0000000101", Name: "ห้องประชุม/อุณหภูมิ", Model: "WSDCGQ11LM", Vendor: "Aqara", ModelID: "lumi.weather", Manufacturer: "LUMI", PowerSource: "Battery", Description: "Temperature and humidity sensor",
		Exposes: `[{"name":"battery","access":1,"type":"numeric","property":"battery","category":"diagnostic","unit":"%","value_max":100,"value_min":0},{"name":"temperature","access":1,"type":"numeric","property":"temperature","unit":"°C"},{"name":"humidity","access":1,"type":"numeric","property":"humidity","unit":"%"},{"name":"pressure","access":1,"type":"numeric","property":"pressure","unit":"hPa"},{"name":"voltage","access":1,"type":"numeric","property":"voltage","category":"diagnostic","unit":"mV"}]`,
		Initial: map[string]any{"temperature": 24.6, "humidity": 55.2, "pressure": 1008.4, "battery": 91, "voltage": 3005}},
	{IEEE: "0x00158d0000000102", Name: "ประตูห้องเก็บของ", Model: "MCCGQ11LM", Vendor: "Aqara", ModelID: "lumi.sensor_magnet.aq2", Manufacturer: "LUMI", PowerSource: "Battery", Description: "Door and window sensor",
		Exposes: `[{"name":"battery","access":1,"type":"numeric","property":"battery","category":"diagnostic","unit":"%","value_max":100,"value_min":0},{"name":"contact","access":1,"type":"binary","property":"contact","value_on":false,"value_off":true},{"name":"device_temperature","access":1,"type":"numeric","property":"device_temperature","category":"diagnostic","unit":"°C"},{"name":"voltage","access":1,"type":"numeric","property":"voltage","category":"diagnostic","unit":"mV"},{"name":"power_outage_count","access":1,"type":"numeric","property":"power_outage_count","category":"diagnostic"},{"name":"trigger_count","access":1,"type":"numeric","property":"trigger_count","category":"diagnostic"}]`,
		Initial: map[string]any{"contact": true, "battery": 88, "voltage": 2995, "device_temperature": 27}},
	{IEEE: "0x00158d0000000103", Name: "ทางเดิน/PIR", Model: "RTCGQ11LM", Vendor: "Aqara", ModelID: "lumi.sensor_motion.aq2", Manufacturer: "LUMI", PowerSource: "Battery", Description: "Motion sensor",
		Exposes: `[{"name":"battery","access":1,"type":"numeric","property":"battery","category":"diagnostic","unit":"%","value_max":100,"value_min":0},{"name":"occupancy","access":1,"type":"binary","property":"occupancy","value_on":true,"value_off":false},{"name":"device_temperature","access":1,"type":"numeric","property":"device_temperature","category":"diagnostic","unit":"°C"},{"name":"voltage","access":1,"type":"numeric","property":"voltage","category":"diagnostic","unit":"mV"},{"name":"illuminance","access":1,"type":"numeric","property":"illuminance","unit":"lx"},{"name":"power_outage_count","access":1,"type":"numeric","property":"power_outage_count","category":"diagnostic"}]`,
		Initial: map[string]any{"occupancy": false, "illuminance": 120, "battery": 95, "voltage": 3015}},
	{IEEE: "0x00158d0000000104", Name: "ใต้ซิงก์", Model: "SJCGQ11LM", Vendor: "Aqara", ModelID: "lumi.sensor_wleak.aq1", Manufacturer: "LUMI", PowerSource: "Battery", Description: "Water leak sensor",
		Exposes: `[{"name":"battery","access":1,"type":"numeric","property":"battery","category":"diagnostic","unit":"%","value_max":100,"value_min":0},{"name":"voltage","access":1,"type":"numeric","property":"voltage","category":"diagnostic","unit":"mV"},{"name":"device_temperature","access":1,"type":"numeric","property":"device_temperature","category":"diagnostic","unit":"°C"},{"name":"power_outage_count","access":1,"type":"numeric","property":"power_outage_count","category":"diagnostic"},{"name":"trigger_count","access":1,"type":"numeric","property":"trigger_count","category":"diagnostic"},{"name":"water_leak","access":1,"type":"binary","property":"water_leak","value_on":true,"value_off":false},{"name":"battery_low","access":1,"type":"binary","property":"battery_low","category":"diagnostic","value_on":true,"value_off":false}]`,
		Initial: map[string]any{"water_leak": false, "battery": 100, "voltage": 3025, "battery_low": false}},
	{IEEE: "0xa4c1380000000105", Name: "ปุ่ม SOS ห้องพัก", Model: "TS0215A_sos", Vendor: "Tuya", ModelID: "TS0215A", Manufacturer: "_TZ3000_p6ju8myv", PowerSource: "Battery", Description: "SOS button",
		Exposes: `[{"name":"battery","access":1,"type":"numeric","property":"battery","category":"diagnostic","unit":"%","value_max":100,"value_min":0},{"name":"voltage","access":1,"type":"numeric","property":"voltage","category":"diagnostic","unit":"mV"},{"name":"action","access":1,"type":"enum","property":"action","category":"diagnostic","values":["emergency"]}]`,
		Initial: map[string]any{"battery": 80, "voltage": 2900}},
	{IEEE: "0x000b57fffe000106", Name: "รีโมตไฟห้องประชุม", Model: "E1743", Vendor: "IKEA", ModelID: "TRADFRI on/off switch", Manufacturer: "IKEA of Sweden", PowerSource: "Battery", Description: "TRADFRI on/off switch",
		Exposes: `[{"name":"battery","access":5,"type":"numeric","property":"battery","category":"diagnostic","unit":"%","value_max":100,"value_min":0},{"name":"action","access":1,"type":"enum","property":"action","category":"diagnostic","values":["on","off","brightness_move_up","brightness_move_down","brightness_stop"]}]`,
		Initial: map[string]any{"battery": 74}},
	{IEEE: "0x00158d0000000107", Name: "ห้องครัว/ควัน", Model: "JTYJ-GD-01LM/BW", Vendor: "Xiaomi", ModelID: "lumi.sensor_smoke", Manufacturer: "LUMI", PowerSource: "Battery", Description: "Mijia Honeywell smoke detector",
		Exposes: `[{"name":"smoke","access":1,"type":"binary","property":"smoke","value_on":true,"value_off":false},{"name":"battery_low","access":1,"type":"binary","property":"battery_low","category":"diagnostic","value_on":true,"value_off":false},{"name":"tamper","access":1,"type":"binary","property":"tamper","value_on":true,"value_off":false},{"name":"battery","access":1,"type":"numeric","property":"battery","category":"diagnostic","unit":"%","value_max":100,"value_min":0},{"name":"sensitivity","access":3,"type":"enum","property":"sensitivity","values":["low","medium","high"]},{"name":"smoke_density","access":1,"type":"numeric","property":"smoke_density"},{"name":"selftest","access":2,"type":"enum","property":"selftest","values":[""]},{"name":"voltage","access":1,"type":"numeric","property":"voltage","category":"diagnostic","unit":"mV"},{"name":"test","access":1,"type":"binary","property":"test","value_on":true,"value_off":false},{"name":"device_temperature","access":1,"type":"numeric","property":"device_temperature","category":"diagnostic","unit":"°C"},{"name":"power_outage_count","access":1,"type":"numeric","property":"power_outage_count","category":"diagnostic"}]`,
		Initial: map[string]any{"smoke": false, "battery_low": false}},
}

// z2mDevices is every simulated non-switch device.
func z2mDevices() []Z2MActuator {
	return append(append([]Z2MActuator{}, Z2MActuators...), Z2MSensors...)
}

// Z2MBridgeDevicesAll is bridge/devices for the whole virtual network: the switches of Z2MBridgeDevices plus
// Z2MActuators and Z2MSensors.
func Z2MBridgeDevicesAll() []byte {
	var devices []json.RawMessage
	_ = json.Unmarshal(Z2MBridgeDevices(), &devices)
	for _, a := range z2mDevices() {
		kind, description := "Router", a.Description
		if a.PowerSource == "Battery" {
			kind = "EndDevice"
		}
		if description == "" {
			description = a.Model
		}
		b, _ := json.Marshal(map[string]any{
			"ieee_address": a.IEEE, "type": kind, "friendly_name": a.Name, "supported": true, "network_address": 2000,
			"model_id": a.ModelID, "manufacturer": a.Manufacturer, "power_source": a.PowerSource, "interview_completed": true, "disabled": false,
			"definition": map[string]any{"model": a.Model, "vendor": a.Vendor, "description": description, "exposes": json.RawMessage(a.Exposes)},
		})
		devices = append(devices, b)
	}
	b, _ := json.Marshal(devices)
	return b
}

// FakeBridge is a stateful virtual Zigbee2MQTT bridge: it applies /set commands to its devices and publishes
// the new state, as Zigbee2MQTT does once a device acknowledges. DropCommands makes it ignore every command (a
// device that never answers), so the timeout path can be exercised too.
type FakeBridge struct {
	Base         string
	DropCommands bool

	mu       sync.Mutex
	switches [][]bool
	states   map[string]map[string]any
}

func NewFakeBridge(base string) *FakeBridge {
	b := &FakeBridge{Base: base, switches: Z2MStep(0), states: map[string]map[string]any{}}
	for _, a := range z2mDevices() {
		s := map[string]any{}
		for k, v := range a.Initial {
			s[k] = v
		}
		b.states[a.IEEE] = s
	}
	return b
}

// Step is what the bridge publishes at one step: the retained announcement first (step 0), a press on the wall
// of the first switch's gang 1 every 20 steps, and every device's state.
func (b *FakeBridge) Step(step int) []Z2MMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []Z2MMessage{}
	if step == 0 {
		out = append(out, Z2MMessage{Topic: b.Base + "/bridge/state", Payload: Z2MOnline(true), Retain: true},
			Z2MMessage{Topic: b.Base + "/bridge/devices", Payload: Z2MBridgeDevicesAll(), Retain: true})
		for _, s := range Z2MSwitches {
			out = append(out, Z2MMessage{Topic: Z2MTopic(b.Base, s, "availability"), Payload: Z2MOnline(true), Retain: true})
		}
		for _, a := range z2mDevices() {
			out = append(out, Z2MMessage{Topic: b.Base + "/" + a.Name + "/availability", Payload: Z2MOnline(true), Retain: true})
		}
	} else if step%20 == 0 {
		b.switches[0][0] = !b.switches[0][0]
	}
	for i, s := range Z2MSwitches {
		out = append(out, Z2MMessage{Topic: Z2MTopic(b.Base, s, ""), Payload: Z2MState(s, b.switches[i], 90+i*20)})
	}
	for _, a := range z2mDevices() {
		out = append(out, b.actuatorState(a))
	}
	// Every 30 steps someone walks past the PIR; every 45 the meeting-room remote is pressed.
	if step > 0 && step%30 == 0 {
		out = append(out, b.set(Z2MSensors[2], "occupancy", true))
	} else if step > 0 && step%30 == 15 {
		out = append(out, b.set(Z2MSensors[2], "occupancy", false))
	}
	if step > 0 && step%45 == 0 {
		out = append(out, Z2MPress(b.Base, Z2MSensors[5], "on"))
	}
	return out
}

func (b *FakeBridge) set(a Z2MActuator, key string, value any) Z2MMessage {
	b.states[a.IEEE][key] = value
	return b.actuatorState(a)
}

// Set changes one reported value of a simulated device (a door opening, water, smoke) and returns the state
// message the bridge publishes for it.
func (b *FakeBridge) Set(ieee, key string, value any) []Z2MMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range z2mDevices() {
		if a.IEEE == ieee {
			return []Z2MMessage{b.set(a, key, value)}
		}
	}
	return nil
}

// Z2MPress is the message a button or remote publishes when pressed: its action and battery, once.
func Z2MPress(base string, a Z2MActuator, action string) Z2MMessage {
	m := map[string]any{"action": action, "linkquality": 120, "aether_source": "simulated", "device": map[string]any{"ieeeAddr": a.IEEE, "friendlyName": a.Name, "model": a.Model}}
	if v, ok := a.Initial["battery"]; ok {
		m["battery"] = v
	}
	p, _ := json.Marshal(m)
	return Z2MMessage{Topic: base + "/" + a.Name, Payload: p}
}

func (b *FakeBridge) actuatorState(a Z2MActuator) Z2MMessage {
	m := map[string]any{"linkquality": 150, "aether_source": "simulated", "device": map[string]any{"ieeeAddr": a.IEEE, "friendlyName": a.Name, "model": a.Model}}
	for k, v := range b.states[a.IEEE] {
		m[k] = v
	}
	p, _ := json.Marshal(m)
	return Z2MMessage{Topic: b.Base + "/" + a.Name, Payload: p}
}

// Apply handles one message on <base>/<ieee or friendly name>/set and returns the state the bridge publishes in
// answer (nothing for a dropped command, an unknown device or a malformed payload). Values are applied as
// given; TOGGLE flips a binary state.
func (b *FakeBridge) Apply(topic string, payload []byte) []Z2MMessage {
	rest, ok := strings.CutPrefix(topic, b.Base+"/")
	if !ok || !strings.HasSuffix(rest, "/set") || b.DropCommands {
		return nil
	}
	device := strings.TrimSuffix(rest, "/set")
	var set map[string]any
	if json.Unmarshal(payload, &set) != nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, s := range Z2MSwitches {
		if device != s.IEEE && device != s.Name {
			continue
		}
		for g := range s.Endpoints {
			if v, ok := set[s.property(g)].(string); ok {
				switch strings.ToUpper(v) {
				case "ON":
					b.switches[i][g] = true
				case "OFF":
					b.switches[i][g] = false
				case "TOGGLE":
					b.switches[i][g] = !b.switches[i][g]
				}
			}
		}
		return []Z2MMessage{{Topic: Z2MTopic(b.Base, s, ""), Payload: Z2MState(s, b.switches[i], 90+i*20)}}
	}
	for _, a := range z2mDevices() {
		if device != a.IEEE && device != a.Name {
			continue
		}
		state := b.states[a.IEEE]
		for k, v := range set {
			if v == "TOGGLE" {
				if state[k] == "ON" {
					v = "OFF"
				} else {
					v = "ON"
				}
			}
			state[k] = v
			if k == "position" { // a cover reports where it went and that it stopped there
				state["state"] = "STOP"
			}
		}
		return []Z2MMessage{b.actuatorState(a)}
	}
	return nil
}
