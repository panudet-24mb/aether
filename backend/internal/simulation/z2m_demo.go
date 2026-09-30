package simulation

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// The demo hospital's Zigbee network (demo-twin extend). Every device is a real Zigbee2MQTT definition — vendor,
// model, description and exposes — exported from zigbee-herdsman-converters (MIT) by
// infra/export-z2m-demo-definitions.mjs into z2m_demo_definitions.json. The simulated coordinator publishes
// bridge/devices exactly as Zigbee2MQTT does, and each device's state carries the keys its exposes declare, with
// values inside their ranges, enum values from their lists and binaries as their value_on / value_off.

//go:embed z2m_demo_definitions.json
var z2mDemoJSON []byte

// Z2MDefinition is one exported Zigbee2MQTT definition.
type Z2MDefinition struct {
	Vendor      string   `json:"vendor"`
	Model       string   `json:"model"`
	Description string   `json:"description"`
	ZigbeeModel []string `json:"zigbee_model"`
	Fingerprint []struct {
		ModelID      string `json:"model_id"`
		Manufacturer string `json:"manufacturer"`
	} `json:"fingerprint"`
	Exposes json.RawMessage `json:"exposes"`
}

// z2mManufacturer is the Basic-cluster manufacturer name each vendor's devices report.
var z2mManufacturer = map[string]string{"Tuya": "_TZ3000_aether", "Aqara": "LUMI", "IKEA": "IKEA of Sweden", "Philips": "Signify Netherlands B.V.", "SONOFF": "eWeLink", "Heiman": "HEIMAN"}

// Identity is the model id and manufacturer the device reports (bridge/devices model_id and manufacturer): its
// Zigbee model id, or for Tuya devices matched by fingerprint (every TS0601) the first fingerprint.
func (d Z2MDefinition) Identity() (modelID, manufacturer string) {
	modelID, manufacturer = d.Model, z2mManufacturer[d.Vendor]
	if len(d.ZigbeeModel) > 0 {
		modelID = d.ZigbeeModel[0]
	}
	if len(d.Fingerprint) > 0 && (len(d.ZigbeeModel) == 0 || strings.HasPrefix(d.Model, "TS0601")) {
		modelID, manufacturer = d.Fingerprint[0].ModelID, d.Fingerprint[0].Manufacturer
	}
	return modelID, manufacturer
}

// Z2MDefinitions returns the embedded definitions by model.
func Z2MDefinitions() map[string]Z2MDefinition {
	var doc struct {
		Definitions []Z2MDefinition `json:"definitions"`
	}
	if e := json.Unmarshal(z2mDemoJSON, &doc); e != nil {
		panic("simulation: z2m_demo_definitions.json: " + e.Error())
	}
	out := map[string]Z2MDefinition{}
	for _, d := range doc.Definitions {
		out[d.Model] = d
	}
	return out
}

// TwinZigbee is one device of the coordinator: its identity, its real definition, where it is and what it does in
// the script (Role).
type TwinZigbee struct {
	IEEE, Name string
	Def        Z2MDefinition
	Role       string
	// BaseC / BaseH are the room's usual temperature and humidity for sensors that report them.
	BaseC, BaseH float64
	TwinPlaced
}

// Battery reports a battery device (its definition exposes a battery level or a low-battery flag).
func (d TwinZigbee) Battery() bool {
	for _, f := range z2mFeatures(d.Def.Exposes) {
		if f.Property == "battery" || f.Property == "battery_low" {
			return true
		}
	}
	return false
}

// PowerSource is what Zigbee2MQTT reports for the device.
func (d TwinZigbee) PowerSource() string {
	if d.Battery() {
		return "Battery"
	}
	return "Mains (single phase)"
}

// z2mFeature is one leaf of a definition's exposes.
type z2mFeature struct {
	Type     string          `json:"type"`
	Name     string          `json:"name"`
	Property string          `json:"property"`
	Access   int             `json:"access"`
	Unit     string          `json:"unit"`
	Min      *float64        `json:"value_min"`
	Max      *float64        `json:"value_max"`
	Step     *float64        `json:"value_step"`
	Values   []any           `json:"values"`
	ValueOn  json.RawMessage `json:"value_on"`
	ValueOff json.RawMessage `json:"value_off"`
	Category string          `json:"category"`
	Endpoint string          `json:"endpoint"`
	Features []z2mFeature    `json:"features"`
}

var z2mSpecific = map[string]bool{"light": true, "switch": true, "fan": true, "cover": true, "lock": true, "climate": true}

// z2mFeatures flattens exposes: the features inside light/switch/cover/... groups, plain features, and composites
// kept whole.
func z2mFeatures(raw json.RawMessage) []z2mFeature {
	var top []z2mFeature
	_ = json.Unmarshal(raw, &top)
	out := []z2mFeature{}
	var walk func(list []z2mFeature)
	walk = func(list []z2mFeature) {
		for _, f := range list {
			if z2mSpecific[f.Type] {
				walk(f.Features)
				continue
			}
			if f.Property != "" {
				out = append(out, f)
			}
		}
	}
	walk(top)
	return out
}

func raw(v json.RawMessage) any {
	var out any
	if len(v) == 0 || json.Unmarshal(v, &out) != nil {
		return nil
	}
	return out
}

// twinZigbeeDevices is the demo network: 52 devices from 31 real definitions, placed in the building.
func twinZigbeeDevices() []TwinZigbee {
	defs := Z2MDefinitions()
	oui := map[string]string{"Tuya": "a4c138", "Aqara": "00158d", "IKEA": "842e14", "Philips": "001788", "SONOFF": "00124b", "Heiman": "000d6f"}
	out := []TwinZigbee{}
	add := func(name, model, role string, f, zone int, x, y, c, h float64) {
		def, ok := defs[model]
		if !ok {
			panic("simulation: no Zigbee definition for " + model)
		}
		ieee := fmt.Sprintf("0x%sf1%08x", oui[def.Vendor], len(out)+1)
		out = append(out, TwinZigbee{IEEE: ieee, Name: name, Def: def, Role: role, BaseC: c, BaseH: h, TwinPlaced: TwinPlaced{Floor: f, Zone: zone, X: x, Y: y, Z: 1.4}})
	}
	// Wall switches, 1 to 4 gangs, lit by the hospital's day.
	add("สวิตช์ไฟห้องยา", "TS0001", "light-day", 0, 2, 29, 1, 0, 0)
	add("สวิตช์ไฟห้องเย็น", "TS0001", "light-work", 0, 5, 29, 23, 0, 0)
	add("สวิตช์ไฟห้องเซิร์ฟเวอร์", "TS0001", "light-server", 2, 2, 29, 23, 0, 0)
	add("สวิตช์ไฟหอผู้ป่วย 1", "TS0002", "light-ward", 1, 0, 1, 1, 0, 0)
	add("สวิตช์ไฟหอผู้ป่วย 2", "TS0002", "light-ward", 1, 1, 14, 1, 0, 0)
	add("สวิตช์ไฟหอผู้ป่วย 3", "TS0002", "light-ward", 1, 2, 27, 1, 0, 0)
	add("สวิตช์ไฟ ICU A", "TS0003", "light-icu", 2, 0, 1, 1, 0, 0)
	add("สวิตช์ไฟ ICU B", "TS0003", "light-icu", 2, 1, 21, 1, 0, 0)
	add("สวิตช์ไฟทางเดินชั้น 1", "TS0004", "light-corridor", 0, 3, 1, 11, 0, 0)
	add("สวิตช์ไฟทางเดินชั้น 2", "TS0004", "light-corridor", 1, 3, 1, 11, 0, 0)
	add("สวิตช์ไฟโถงต้อนรับ", "TS0004", "light-lobby", 0, 0, 1, 1, 0, 0)
	// Plugs with metering on the equipment that matters.
	add("ตู้เย็นเก็บเลือด", "TS011F_plug_1", "plug-fridge", 0, 4, 26, 16, 0, 0)
	add("เครื่องให้ยา ICU A", "TS011F_plug_1", "plug-pump", 2, 0, 4, 9, 0, 0)
	add("เครื่องให้ยา ICU B", "TS011F_plug_1", "plug-pump", 2, 1, 24, 9, 0, 0)
	add("ทีวีห้องพักผู้ป่วย 2", "TS011F_plug_1", "plug-tv", 1, 1, 18, 8.5, 0, 0)
	add("เครื่องติดตามสัญญาณชีพ ห้องฉุกเฉิน", "TS011F_plug_1", "plug-monitor", 0, 1, 20, 3, 0, 0)
	add("เครื่องชงกาแฟ เคาน์เตอร์พยาบาล", "TS011F_plug_1", "plug-coffee", 1, 4, 24, 22, 0, 0)
	// Climate, cover, presence and air.
	add("วาล์วอุณหภูมิ ICU A", "TS0601_thermostat", "trv", 2, 0, 19, 6, 22.6, 0)
	add("วาล์วอุณหภูมิ ICU B", "TS0601_thermostat", "trv", 2, 1, 39, 6, 22.9, 0)
	add("ม่านโถงต้อนรับ", "TS0601_cover_1", "curtain", 0, 0, 7, 0.3, 0, 0)
	add("เรดาร์ตรวจคน หอผู้ป่วย 3", "TS0601_human_presence_sensor", "presence-ward", 1, 2, 33, 5, 0, 0)
	add("เรดาร์ตรวจคน ICU B", "TS0601_human_presence_sensor", "presence-icu", 2, 1, 30, 6, 0, 0)
	add("คุณภาพอากาศ หอผู้ป่วย 1", "TS0601_air_quality_sensor", "air", 1, 0, 6, 8, 24.6, 55)
	add("คุณภาพอากาศ ห้องฉุกเฉิน", "TS0601_air_quality_sensor", "air", 0, 1, 26, 9, 25.1, 57)
	add("เครื่องฟอกอากาศ ICU B", "E2007", "purifier", 2, 1, 38, 3, 0, 0)
	// Safety.
	add("ตรวจจับควันห้องแล็บ", "TS0601_smoke_1", "smoke", 0, 4, 12, 19, 0, 0)
	add("ตรวจจับควัน ICU A", "HS1SA-E", "smoke-quiet", 2, 0, 10, 6, 0, 0)
	add("ตรวจจับแก๊สห้องแล็บ", "HS1CG", "gas", 0, 4, 24, 22, 0, 0)
	add("ตรวจจับ CO โถงต้อนรับ", "HS1CA-E", "co", 0, 0, 2, 2, 0, 0)
	add("น้ำรั่วใต้อ่างห้องแล็บ", "TS0207_water_leak_detector", "leak", 0, 4, 4, 22, 0, 0)
	add("น้ำรั่ว ICU A", "SJCGQ11LM", "leak-quiet", 2, 0, 2, 11, 0, 0)
	// Temperature and humidity.
	add("อุณหภูมิ ห้องยา (จอแสดงผล)", "TS0201", "env", 0, 2, 38, 8, 22.4, 48)
	add("อุณหภูมิ เคาน์เตอร์พยาบาล", "TS0201", "env", 1, 4, 16, 16, 25.2, 53)
	add("อุณหภูมิ หอผู้ป่วย 2 (WSD500A)", "WSD500A", "env", 1, 1, 22, 8, 24.8, 56)
	add("อุณหภูมิ ICU A (Aqara)", "WSDCGQ11LM", "env", 2, 0, 15, 9, 22.7, 49)
	add("อุณหภูมิ ICU B (Aqara)", "WSDCGQ11LM", "env", 2, 1, 35, 9, 22.9, 50)
	add("อุณหภูมิ ห้องเซิร์ฟเวอร์", "SNZB-02", "env", 2, 2, 38, 16, 19.4, 40)
	// Doors and cabinets.
	add("ประตูห้องยา", "TS0203", "door-pharmacy", 0, 2, 34, 9.8, 0, 0)
	add("ประตูห้องเย็น", "TS0203", "door-cold", 0, 5, 29, 10.3, 0, 0)
	add("ประตูห้องเซิร์ฟเวอร์", "TS0203", "door-server", 2, 2, 34, 14.2, 0, 0)
	add("ประตูห้องแล็บ", "MCCGQ11LM", "door-lab", 0, 4, 14, 14.2, 0, 0)
	add("ฝาตู้ยาควบคุมพิเศษ", "SNZB-04", "door-cabinet", 0, 2, 37, 2, 0, 0)
	// Motion.
	add("PIR ทางเดินชั้น 1", "TS0202", "motion-a", 0, 3, 14, 12, 0, 0)
	add("PIR ทางเดินชั้น 2", "TS0202", "motion-b", 1, 3, 20, 12, 0, 0)
	add("PIR โถงต้อนรับ (Aqara)", "RTCGQ11LM", "motion-c", 0, 0, 7, 5, 0, 0)
	add("เซนเซอร์ตรวจจับ ทางเดินชั้น 2 (Hue)", "9290012607", "motion-d", 1, 3, 34, 12, 24.9, 0)
	add("PIR ห้องเซิร์ฟเวอร์", "SNZB-03", "motion-server", 2, 2, 30, 22, 0, 0)
	// Lights and remotes.
	add("โคมไฟห้องพักพยาบาล", "LED1623G12", "bulb-lounge", 1, 4, 20, 20, 0, 0)
	add("โคมไฟโถงต้อนรับ", "LED1623G12", "bulb-lobby", 0, 0, 10, 3, 0, 0)
	add("ดิมเมอร์ห้องพักพยาบาล", "ICTC-G-1", "dimmer", 1, 4, 15, 23, 0, 0)
	add("รีโมตไฟ ICU A", "E2001/E2002/E2313", "remote", 2, 0, 10, 11, 0, 0)
	add("ปุ่มเรียกพยาบาล หอผู้ป่วย 2", "SNZB-01", "button", 1, 1, 24, 2, 0, 0)
	return out
}

// Scripted moments of the Zigbee network, in steps of the loop.
const (
	TwinPressRemoteOn  = 4 * 60 / TwinStepSec
	TwinPressDimmer    = 6 * 60 / TwinStepSec
	TwinPressNurseCall = 9 * 60 / TwinStepSec
	TwinPressRemoteOff = 24 * 60 / TwinStepSec
)

// TwinZigbeeBridgeDevices is the coordinator's retained bridge/devices, as Zigbee2MQTT publishes it.
func TwinZigbeeBridgeDevices(x TwinExtras) []byte {
	devices := []map[string]any{{"ieee_address": "0x00124bf100000000", "type": "Coordinator", "friendly_name": "Coordinator", "supported": true, "network_address": 0, "interview_completed": true, "disabled": false}}
	for i, d := range x.ZigbeeDev {
		kind := "Router"
		if d.Battery() {
			kind = "EndDevice"
		}
		modelID, manufacturer := d.Def.Identity()
		devices = append(devices, map[string]any{
			"ieee_address": d.IEEE, "type": kind, "friendly_name": d.Name, "supported": true, "network_address": 0x3000 + i,
			"model_id": modelID, "manufacturer": manufacturer, "power_source": d.PowerSource(), "interview_completed": true, "disabled": false,
			"definition": map[string]any{"model": d.Def.Model, "vendor": d.Def.Vendor, "description": d.Def.Description, "exposes": d.Def.Exposes, "options": []any{}},
		})
	}
	b, _ := json.Marshal(devices)
	return b
}

// TwinZigbeeState is what a device reports at a step and wall-clock time: every published feature of its exposes,
// valued for its role (and the room it is in), within the feature's range.
func TwinZigbeeState(d TwinZigbee, step int, at time.Time, o TwinOverrides) map[string]any {
	s := loopStep(step)
	h := hourOf(at)
	day := h >= 7 && h < 19
	k := 0
	for _, c := range d.IEEE {
		k += int(c)
	}
	wave := func(period float64) float64 { return math.Sin(float64(s)/period + float64(k%17)) }
	m := map[string]any{"linkquality": 90 + k%120}
	switches := []z2mFeature{}
	for _, f := range z2mFeatures(d.Def.Exposes) {
		if f.Access&1 == 0 || f.Type == "composite" || f.Type == "list" || f.Type == "text" || f.Name == "action" {
			continue
		}
		if f.Type == "binary" && f.Name == "state" {
			switches = append(switches, f)
		}
		m[f.Property] = defaultValue(f, d, s, at, k)
	}
	set := func(prop string, v any) {
		if _, ok := m[prop]; ok {
			m[prop] = v
		}
	}
	flag := func(prop string, on bool) {
		for _, f := range z2mFeatures(d.Def.Exposes) {
			if f.Property == prop && f.Type == "binary" {
				if on {
					m[prop] = raw(f.ValueOn)
				} else {
					m[prop] = raw(f.ValueOff)
				}
			}
		}
	}
	lights := func(on ...bool) {
		for i, f := range switches {
			if i < len(on) {
				flag(f.Property, on[i])
			}
		}
	}
	switch d.Role {
	case "light-day":
		lights(h >= 7 && h < 20)
	case "light-work":
		lights(s%100 < 12) // the cold store is lit only while someone works in it
	case "light-server":
		lights((s >= TwinWanderFrom && s < TwinWanderTo) || (s >= 180 && s < 190))
	case "light-ward":
		lights(!(h >= 22 || h < 6), day)
	case "light-icu":
		lights(true, !(h >= 23 || h < 5), day)
	case "light-corridor":
		lights(true, !day, h >= 6 && h < 23, !day)
	case "light-lobby":
		lights(true, true, h >= 6 && h < 21, !day)
	case "plug-fridge":
		on := s%40 < 26
		plug(m, true, map[bool]float64{true: 118, false: 6}[on]+3*wave(5), at, 72)
	case "plug-pump":
		plug(m, true, 18+2*wave(7), at, 19)
	case "plug-tv":
		tv := h >= 8 && h < 22
		w := 0.8
		if tv {
			w = 86 + 4*wave(6)
		}
		plug(m, tv, w, at, 55)
	case "plug-monitor":
		plug(m, true, 42+5*wave(9), at, 43)
	case "plug-coffee":
		brewing := s%60 < 4
		w := 2.5
		if brewing {
			w = 1250 + 30*wave(2)
		}
		plug(m, true, w, at, 60)
	case "trv":
		set("local_temperature", round1(d.BaseC+0.35*wave(29)))
		set("current_heating_setpoint", 23)
		set("position", 25+int(10*(0.5+0.5*wave(40))))
		set("system_mode", pickEnum(d, "system_mode", "auto", "heat"))
		set("running_state", pickEnum(d, "running_state", "idle"))
		set("preset", pickEnum(d, "preset", "comfort", "manual"))
	case "curtain":
		open := h >= 7 && h < 18
		set("position", map[bool]int{true: 100, false: 0}[open])
		set("state", pickEnum(d, "state", map[bool]string{true: "OPEN", false: "CLOSE"}[open]))
	case "presence-ward":
		present := (s+k)%40 < 30
		flag("presence", present)
		set("duration_of_attendance", map[bool]int{true: (s + k) % 40 * 6 / 60, false: 0}[present])
		set("duration_of_absence", map[bool]int{true: 0, false: ((s+k)%40 - 30) * 6 / 60}[present])
	case "presence-icu":
		flag("presence", true)
		set("duration_of_attendance", 40+s*6/60)
	case "air":
		occupied := 0.5 + 0.5*wave(35)
		set("co2", int(540+520*occupied))
		set("voc", round1(0.1+0.25*occupied))
		set("formaldehyd", int(6+8*occupied))
		set("temperature", round1(d.BaseC+0.3*wave(18)))
		set("humidity", round1(d.BaseH+1.2*wave(24)))
	case "purifier":
		pm := int(4 + 5*(0.5+0.5*wave(31)))
		set("pm25", pm)
		set("fan_mode", pickEnum(d, "fan_mode", "auto"))
		set("fan_speed", 1+pm/3)
		set("air_quality", pickEnum(d, "air_quality", "excellent", "good"))
		flag("fan_state", true)
	case "smoke":
		flag("smoke", s >= TwinSmokeFrom && s < TwinSmokeTo)
	case "leak":
		flag("water_leak", s >= TwinLeakFrom && s < TwinLeakTo)
	case "env":
		set("temperature", round1(d.BaseC+0.3*wave(19)))
		set("humidity", round1(d.BaseH+1.4*wave(23)))
	case "door-pharmacy":
		flag("contact", !(s%100 >= 50 && s%100 < 60)) // opened for a minute every ten
	case "door-cold":
		door := at.Before(o.DoorUntil) || (s >= TwinDoorFrom && s < TwinDoorTo)
		flag("contact", !(door && s%10 < 4))
	case "door-server":
		flag("contact", !((s >= TwinWanderFrom && s < TwinWanderFrom+5) || (s >= TwinWanderTo-5 && s < TwinWanderTo)))
	case "door-lab":
		flag("contact", !(s%75 >= 30 && s%75 < 36))
	case "door-cabinet":
		flag("contact", !(s >= TwinTamperFrom && s < TwinTamperFrom+4))
	case "motion-a":
		flag("occupancy", (s+k)%30 < 10)
	case "motion-b":
		flag("occupancy", (s+k*3)%25 < 8)
	case "motion-c":
		flag("occupancy", day && (s+k)%12 < 7)
		set("illuminance", illuminance(day, h, wave(9)))
	case "motion-d":
		flag("occupancy", (s+k)%20 < 6)
		set("illuminance", illuminance(day, h, wave(11))/2)
		set("temperature", round1(d.BaseC+0.2*wave(21)))
	case "motion-server":
		flag("occupancy", s >= TwinWanderFrom && s < TwinWanderTo)
	case "bulb-lounge":
		on := h >= 18 || h < 7
		flag("state", on)
		set("brightness", map[bool]int{true: 180, false: 0}[on])
	case "bulb-lobby":
		flag("state", true)
		set("brightness", map[bool]int{true: 254, false: 90}[day])
	}
	return m
}

func illuminance(day bool, h, wobble float64) int {
	if !day {
		return int(45 + 5*wobble)
	}
	return int(480 + 160*math.Sin((h-7)/12*math.Pi) + 10*wobble)
}

// plug fills a metering plug's state; energy grows with the wall clock at the plug's average draw.
func plug(m map[string]any, on bool, w float64, at time.Time, averageW float64) {
	if w < 0 {
		w = 0
	}
	m["state"] = map[bool]string{true: "ON", false: "OFF"}[on]
	volts := round1(229 + 1.5*math.Sin(float64(at.Unix())/600))
	m["power"] = round1(w)
	if _, ok := m["voltage"]; ok {
		m["voltage"] = volts
	}
	if _, ok := m["current"]; ok {
		m["current"] = math.Round(w/volts*100) / 100
	}
	if _, ok := m["energy"]; ok {
		hours := float64(at.Unix()-1_788_000_000) / 3600 // since 2026-08-30
		m["energy"] = math.Round(averageW*hours) / 1000
	}
}

// pickEnum returns the first of the wanted values the device's enum offers, else its first value.
func pickEnum(d TwinZigbee, prop string, wanted ...string) any {
	for _, f := range z2mFeatures(d.Def.Exposes) {
		if f.Property != prop {
			continue
		}
		for _, w := range wanted {
			for _, v := range f.Values {
				if fmt.Sprint(v) == w {
					return v
				}
			}
		}
		if len(f.Values) > 0 {
			return f.Values[0]
		}
	}
	if len(wanted) > 0 {
		return wanted[0]
	}
	return nil
}

// defaultValue is a feature's value when the role does not set it: binaries off (no alarm, not tampered, not
// low), enums their first value, numerics a plausible reading within range.
func defaultValue(f z2mFeature, d TwinZigbee, s int, at time.Time, k int) any {
	switch f.Type {
	case "binary":
		if f.Name == "state" {
			return raw(f.ValueOn)
		}
		return raw(f.ValueOff)
	case "enum":
		if len(f.Values) > 0 {
			return f.Values[0]
		}
		return nil
	}
	clamp := func(v float64) float64 {
		if f.Min != nil && v < *f.Min {
			v = *f.Min
		}
		if f.Max != nil && v > *f.Max {
			v = *f.Max
		}
		return v
	}
	switch f.Name {
	case "battery":
		return int(clamp(float64(98 - k%31)))
	case "voltage":
		if strings.EqualFold(f.Unit, "mV") {
			return 2900 + k%180
		}
		return 229.4
	case "linkquality":
		return 90 + k%120
	case "device_temperature":
		return 27 + k%6
	case "temperature", "local_temperature":
		base := d.BaseC
		if base == 0 {
			base = 24.5
		}
		return round1(base + 0.3*math.Sin(float64(s)/19+float64(k%7)))
	case "humidity":
		base := d.BaseH
		if base == 0 {
			base = 52
		}
		return round1(base + math.Cos(float64(s)/23+float64(k%5)))
	case "pressure":
		return 1009.4
	case "illuminance", "illuminance_lux":
		return illuminance(hourOf(at) >= 7 && hourOf(at) < 19, hourOf(at), 0)
	case "power_outage_count":
		return 1 + k%3
	case "trigger_count":
		return 12 + k%40
	case "occupancy_timeout":
		return 60
	case "brightness":
		return 200
	}
	if f.Min != nil && f.Max != nil {
		v := *f.Min + (*f.Max-*f.Min)*0.25
		if f.Step != nil && *f.Step > 0 {
			v = math.Round(v / *f.Step) * *f.Step
		} else {
			v = math.Round(v)
		}
		return v
	}
	return 0
}

// TwinZigbeePress is a remote's or button's press at a step ("" when none).
func TwinZigbeePress(d TwinZigbee, step int) string {
	s := loopStep(step)
	want := ""
	switch {
	case d.Role == "remote" && s == TwinPressRemoteOn:
		want = "on"
	case d.Role == "remote" && s == TwinPressRemoteOff:
		want = "off"
	case d.Role == "dimmer" && s == TwinPressDimmer:
		want = "brightness_move_up"
	case d.Role == "button" && s == TwinPressNurseCall:
		want = "single"
	default:
		return ""
	}
	for _, f := range z2mFeatures(d.Def.Exposes) {
		if f.Name != "action" {
			continue
		}
		values := []string{}
		for _, v := range f.Values {
			values = append(values, fmt.Sprint(v))
		}
		sort.Strings(values)
		for _, v := range values {
			if v == want {
				return v
			}
		}
		for _, v := range values {
			if strings.Contains(v, want) {
				return v
			}
		}
		if len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// TwinZigbeeModels lists the definitions the network uses, for reports.
func TwinZigbeeModels(x TwinExtras) []string {
	seen := map[string]int{}
	for _, d := range x.ZigbeeDev {
		seen[d.Def.Vendor+" "+d.Def.Model+" · "+d.Def.Description]++
	}
	out := []string{}
	for k, n := range seen {
		out = append(out, fmt.Sprintf("%s ×%d", k, n))
	}
	sort.Strings(out)
	return out
}
