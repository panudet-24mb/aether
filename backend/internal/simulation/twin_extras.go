package simulation

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// The extended demo hospital (`demo-twin extend`): every device family Aether reads, added to the same building
// and driven by the same 30-minute loop. Minew tags are heard by the building's MG3s (plus one MG4 in the lab),
// Zigbee devices report through one Zigbee2MQTT coordinator and Tuya Wi-Fi devices through one Aether Edge, both
// over MQTT. Everything stays a function of the step and the wall clock, so run, backfill and tests agree.

// TwinMinewExtra is one extra Minew tag. Gateway is an index into TwinBuilding.Gateways, or TwinMG4 for the lab MG4.
type TwinMinewExtra struct {
	Name, MAC, Model, Profile string
	Floor, Zone, Gateway      int
	X, Y, Z                   float64
	// Wearable tags walk Route (gateway indices, each held Dwell steps) and are registered roaming.
	Wearable      bool
	Route         []int
	Dwell, Offset int
	BaseC, BaseH  float64
	// Info is the name the tag's info frame carries (S1 tags may say "PLUS").
	Info string
}

// TwinMG4 marks a tag heard by the lab's MG4 instead of one of the MG3s.
const TwinMG4 = -2

// TwinPlaced is where an extra gateway or device sits on the plan.
type TwinPlaced struct {
	Floor, Zone int
	X, Y, Z     float64
}

// TwinTuya is one Tuya Wi-Fi device behind the Aether Edge: its Tuya id, category, role in the script and place.
// Its data point specification lives with the importer (internal/demotwin), which owns the Tuya types.
type TwinTuya struct {
	ID, Name, Category, Role string
	IP, Version              string
	TwinPlaced
}

// TwinExtras is everything `extend` adds.
type TwinExtras struct {
	MG4        TwinPlaced
	MG4Name    string
	MG4MAC     string
	Minew      []TwinMinewExtra
	Zigbee     TwinPlaced
	ZigbeeName string
	ZigbeeDev  []TwinZigbee
	Edge       TwinPlaced
	EdgeName   string
	Tuya       []TwinTuya
}

// TwinExtended is the extended building's additions. The same call always returns the same devices.
func TwinExtended() TwinExtras {
	x := TwinExtras{
		MG4: TwinPlaced{Floor: 0, Zone: 4, X: 26, Y: 22, Z: 2.6}, MG4Name: "GW ห้องแล็บ (MG4)", MG4MAC: "f1a4000000c0",
		Zigbee: TwinPlaced{Floor: 1, Zone: 4, X: 25, Y: 23, Z: 2.4}, ZigbeeName: "Zigbee coordinator อาคารหลัก",
		Edge: TwinPlaced{Floor: 0, Zone: 0, X: 13, Y: 9, Z: 1.2}, EdgeName: "Aether Edge อาคารหลัก",
	}
	n := 0
	mac := func(prefix string) string { n++; return fmt.Sprintf("%s%04x", prefix, n) }
	// Minew: E8S asset tags on equipment, MBT01 anti-removal tags on cabinets, C10 cards and B7 wristbands on
	// people, and in the lab an MG4 with an S1 (reporting as "PLUS"), an MSP01 and an S4 door sensor.
	asset := func(name string, f, z, gw int, xx, yy float64) {
		x.Minew = append(x.Minew, TwinMinewExtra{Name: name, MAC: mac("f1e80000"), Model: "E8S", Profile: "minew-e8s-pending@1", Floor: f, Zone: z, Gateway: gw, X: xx, Y: yy, Z: 1.0, Info: "E8S"})
	}
	asset("ปั๊มให้ยา ICU A", 2, 0, 7, 6, 8)
	asset("ปั๊มให้ยา ICU B", 2, 1, 8, 26, 8)
	asset("เครื่อง ECG ห้องฉุกเฉิน", 0, 1, 1, 18, 7)
	asset("รถเข็นผู้ป่วย โถงต้อนรับ", 0, 0, 0, 10, 7)
	tamper := func(name string, f, z, gw int, xx, yy float64) {
		x.Minew = append(x.Minew, TwinMinewExtra{Name: name, MAC: mac("f1ab0000"), Model: "MBT01", Profile: "minew-mbt01-pending@1", Floor: f, Zone: z, Gateway: gw, X: xx, Y: yy, Z: 1.2, Info: "MBT01"})
	}
	tamper("ตู้ยาควบคุมพิเศษ", 0, 2, 2, 36, 3)
	tamper("ตู้แร็กเซิร์ฟเวอร์", 2, 2, 9, 37, 21)
	wear := func(name, model, profile string, route []int, dwell, offset int) {
		x.Minew = append(x.Minew, TwinMinewExtra{Name: name, MAC: mac("f1c00000"), Model: model, Profile: profile, Floor: -1, Zone: -1, Gateway: route[0], Wearable: true, Route: route, Dwell: dwell, Offset: offset, Info: model})
	}
	wear("รปภ. สมหมาย", "C10", "minew-c10-pending@1", []int{0, 1, 2, 0}, 35, 7)
	wear("รปภ. วีระพงษ์", "C10", "minew-c10-pending@1", []int{7, 8, 6, 0}, 42, 19)
	wear("ผู้ป่วย คุณสมพร", "B7", "minew-b7-pending@1", []int{4, 4, 6}, 45, 3)
	wear("ผู้ป่วย คุณลำดวน", "B7", "minew-b7-pending@1", []int{8}, 0, 0)
	x.Minew = append(x.Minew,
		TwinMinewExtra{Name: "อุณหภูมิตู้บ่มเชื้อ ห้องแล็บ", MAC: mac("f1c10000"), Model: "S1", Profile: "minew-s1-pending@1", Floor: 0, Zone: 4, Gateway: TwinMG4, X: 20, Y: 21, Z: 1.2, BaseC: 36.8, BaseH: 62, Info: "PLUS"},
		TwinMinewExtra{Name: "PIR ห้องแล็บ", MAC: mac("f1c20000"), Model: "MSP01", Profile: "minew-msp01-pending@1", Floor: 0, Zone: 4, Gateway: TwinMG4, X: 14, Y: 15, Z: 2.6, BaseC: 24.8, BaseH: 50, Info: "MSP01"},
		TwinMinewExtra{Name: "เซนเซอร์ประตูห้องแล็บ (S4)", MAC: mac("f1c50000"), Model: "S4", Profile: "minew-s4-pending@1", Floor: 0, Zone: 4, Gateway: TwinMG4, X: 14, Y: 14.1, Z: 2.0, Info: "S4"},
	)

	x.ZigbeeDev = twinZigbeeDevices()

	// Tuya Wi-Fi local, through the Aether Edge.
	t := func(id, name, category, role, ip string, f, zone int, xx, yy float64) {
		x.Tuya = append(x.Tuya, TwinTuya{ID: id, Name: name, Category: category, Role: role, IP: ip, Version: "3.3", TwinPlaced: TwinPlaced{Floor: f, Zone: zone, X: xx, Y: yy, Z: 1.0}})
	}
	t("bfae7f1000000000cz01", "ตู้แช่วัคซีน (Wi‑Fi)", "cz", "plug-freezer", "192.168.10.21", 0, 5, 38, 22)
	t("bfae7f1000000000cz02", "เครื่องฟอกอากาศ หอผู้ป่วย 3", "cz", "plug-purifier", "192.168.10.22", 1, 2, 38, 8)
	t("bfae7f1000000000kg01", "สวิตช์ไฟหน้าห้อง ICU B (Wi‑Fi)", "kg", "switch-3", "192.168.10.23", 2, 1, 21, 1)
	t("bfae7f1000000000dj01", "ไฟห้องแล็บ", "dj", "light", "192.168.10.24", 0, 4, 10, 18)
	t("bfae7f1000000000wk01", "แอร์ ICU B", "wk", "ac", "192.168.10.25", 2, 1, 39, 10)
	t("bfae7f1000000000hj01", "คุณภาพอากาศ หอผู้ป่วย 3 (Wi‑Fi)", "hjjcy", "air", "192.168.10.26", 1, 2, 38, 9)
	for i := range x.Tuya {
		if x.Tuya[i].Category == "wk" || x.Tuya[i].Category == "hjjcy" {
			x.Tuya[i].Version = "3.4"
		}
	}
	return x
}

// TwinMG4MAC is the lab MG4's MAC.
func (x TwinExtras) mg4Row(ts string) map[string]any {
	return map[string]any{"type": "Gateway", "mac": x.MG4MAC, "timestamp": ts, "aether_source": "simulated"}
}

// hourOf is the local hour (Asia/Bangkok, UTC+7) of a wall-clock time: lights and plugs follow the hospital's day.
func hourOf(at time.Time) float64 {
	t := at.UTC().Add(7 * time.Hour)
	return float64(t.Hour()) + float64(t.Minute())/60
}

// loopStep folds a step into the loop.
func loopStep(step int) int { return ((step % TwinLoopSteps) + TwinLoopSteps) % TwinLoopSteps }

// Scripted moments of the extended loop, in steps (on top of the base script's).
const (
	TwinTamperFrom  = 14 * 60 / TwinStepSec // minute 14: someone lifts the controlled-drug cabinet's tag for a minute
	TwinTamperTo    = 15 * 60 / TwinStepSec
	TwinLeakFrom    = 15 * 60 / TwinStepSec // minute 15: water under the lab sink for three minutes
	TwinLeakTo      = 18 * 60 / TwinStepSec
	TwinSmokeFrom   = 27 * 60 / TwinStepSec // minute 27: the lab's smoke detector is tested for 90 seconds
	TwinSmokeTo     = 27*60/TwinStepSec + 15
	TwinRemoteOn    = 4 * 60 / TwinStepSec
	TwinRemoteOff   = 24 * 60 / TwinStepSec
	TwinVibrateFrom = TwinWanderFrom // the server-room visitor bumps the rack
	TwinVibrateTo   = TwinWanderFrom + 10
)

// ExtraMinewRows are the extra Minew tags' rows for one step, by gateway index (TwinMG4 for the lab MG4).
func (b TwinBuilding) ExtraMinewRows(x TwinExtras, step int, at time.Time, iso bool) map[int][]map[string]any {
	s := loopStep(step)
	ts := any(at.UnixMilli())
	if iso {
		ts = at.UTC().Format(time.RFC3339)
	}
	out := map[int][]map[string]any{}
	row := func(gw int, mac, raw string, rssi int) {
		stamp := ts
		if gw == TwinMG4 {
			stamp = at.UTC().Format(time.RFC3339)
		}
		out[gw] = append(out[gw], map[string]any{"mac": mac, "rawData": raw, "rssi": rssi, "timestamp": stamp, "aether_source": "simulated"})
	}
	for i, m := range x.Minew {
		rssi := -58 - i%7*3
		battery := 93 - i%17
		info := (s+i)%20 == 0
		switch m.Model {
		case "E8S":
			moving := (s+i*37)%50 < 6
			amp := 0.02
			if moving {
				amp = 0.4
			}
			row(m.Gateway, m.MAC, accelFrame(m.MAC, battery, amp*math.Sin(float64(s)), amp*math.Cos(float64(s)*1.3), 1-amp*0.3), rssi)
			row(m.Gateway, m.MAC, vibrationFrame(m.MAC, battery, uint32(at.Unix()), moving), rssi)
		case "MBT01":
			tampered := i == firstModel(x, "MBT01") && s >= TwinTamperFrom && s < TwinTamperTo
			row(m.Gateway, m.MAC, iBeaconFrame(3, i+1, -59), rssi)
			row(m.Gateway, m.MAC, tamperFrame(m.MAC, battery, tampered), rssi)
		case "C10", "B7":
			gw := m.Route[0]
			if len(m.Route) > 1 && m.Dwell > 0 {
				gw = m.Route[((s+m.Offset)/m.Dwell)%len(m.Route)]
			}
			wob := int(math.Round(4 * math.Sin(float64(s)*1.1+float64(i))))
			row(gw, m.MAC, iBeaconFrame(4, i+1, -59), -55+wob)
			row(gw, m.MAC, accelFrame(m.MAC, battery, 0.08*math.Sin(float64(s)/2+float64(i)), 0.06*math.Cos(float64(s)/3), 0.97), -55+wob)
			if info {
				row(gw, m.MAC, infoFrame(m.MAC, battery, m.Info), -55+wob)
			}
			continue
		case "S1":
			c := m.BaseC + 0.25*math.Sin(float64(s)/17+float64(i))
			h := m.BaseH + 1.2*math.Cos(float64(s)/21+float64(i))
			row(m.Gateway, m.MAC, thFrame(m.MAC, battery, c, h), rssi)
		case "MSP01":
			row(m.Gateway, m.MAC, pirFrame(m.MAC, battery, (s+i*5)%11 < 3), rssi)
			row(m.Gateway, m.MAC, thFrame(m.MAC, battery, m.BaseC+0.3*math.Sin(float64(s)/13), m.BaseH), rssi)
		case "S4":
			row(m.Gateway, m.MAC, s4SyntheticFrame(m.MAC, battery, s), rssi)
		}
		if info {
			row(m.Gateway, m.MAC, infoFrame(m.MAC, battery, m.Info), rssi)
		}
	}
	return out
}

func firstModel(x TwinExtras, model string) int {
	for i, m := range x.Minew {
		if m.Model == model {
			return i
		}
	}
	return -1
}

// MG4Packet is the lab MG4's uplink for a step (MG4 JSON-Long with ISO-8601 timestamps), or nil when the MG4
// posts only every other step (it keeps the building under the ingest rate limit).
func (b TwinBuilding) MG4Packet(x TwinExtras, step int, at time.Time) []byte {
	rows := []map[string]any{x.mg4Row(at.UTC().Format(time.RFC3339))}
	rows = append(rows, b.ExtraMinewRows(x, step, at, true)[TwinMG4]...)
	p, _ := json.Marshal(rows)
	return p
}

// TwinTuyaDPS are one Tuya device's raw data points at a step and wall-clock time (numbers already scaled the
// way the device sends them).
func TwinTuyaDPS(d TwinTuya, step int, at time.Time) map[string]any {
	s := loopStep(step)
	h := hourOf(at)
	wave := func(period float64, phase int) float64 { return math.Sin(float64(s)/period + float64(phase)) }
	k := int(d.ID[len(d.ID)-1])
	hours := float64(at.Unix()-1_788_000_000) / 3600
	switch d.Role {
	case "plug-freezer":
		on := (s+7)%36 < 24
		w := map[bool]float64{true: 145, false: 9}[on] + 4*wave(5, k)
		return map[string]any{"1": true, "17": int(100 * hours), "18": int(w / 229 * 1000), "19": int(w * 10), "20": int(2290 + 10*wave(40, k))}
	case "plug-purifier":
		w := 32 + 6*wave(12, k)
		return map[string]any{"1": true, "17": int(30 * hours), "18": int(w / 229 * 1000), "19": int(w * 10), "20": int(2295 + 8*wave(33, k))}
	case "switch-3":
		day := h >= 7 && h < 21
		return map[string]any{"1": true, "2": day, "3": !day}
	case "light":
		on := h >= 7 && h < 20
		return map[string]any{"20": on, "21": "white", "22": map[bool]int{true: 850, false: 10}[on], "23": 600}
	case "ac":
		cur := 23.2 + 0.4*wave(26, k)
		return map[string]any{"1": true, "2": 230, "3": int(math.Round(cur * 10)), "4": "cold"}
	case "air":
		occupied := 0.5 + 0.5*wave(30, k)
		return map[string]any{"2": int(10 + 14*occupied), "18": int(math.Round((24.9 + 0.3*wave(19, k)) * 10)), "19": int(math.Round(57 + 2*wave(23, k))),
			"20": int(10 + 8*occupied), "21": int(120 + 200*occupied), "22": int(610 + 520*occupied)}
	}
	return map[string]any{}
}
