// Package simulation creates deterministic, explicitly tagged virtual BLE observations that mimic
// the Minew MHS starter kit as seen through an MG3 gateway (JSON-LONG rows: mac, rawData, rssi, timestamp).
// Frames are generated from the same byte layouts the decoder parses; where the real tag's behaviour
// is undocumented (button presses) the scenario follows the public configuration guide and the
// profile catalog labels it unverified. Nothing here emulates gateway commands.
package simulation

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

const gatewayMAC = "f00000000000"

// Device MACs are deliberately outside any real vendor range.
const (
	macC10   = "f00000000005"
	macB7    = "f00000000006"
	macB10   = "f00000000007"
	macE8S   = "f00000000008"
	macMBT01 = "f00000000009"
	// A second B10 that behaves like the PHYSICAL one in the owner's workspace (c300007b573c): it
	// never broadcasts Eddystone-UID, so Aether's inferred press can never fire for it. See macB10
	// above for the documented-but-unverified behaviour the original simulated B10 keeps.
	macB10Real = "f0000000000a"
)

var simUUID = mustHex("a37e0000c0de4be7aab1e2a5e7e70001") // synthetic iBeacon UUID for the kit

// B10 press encoding follows the reelyActive B10 configuration guide: the wristband switches its
// Eddystone-UID instance to an event code while pressed. UNVERIFIED on hardware.
const (
	b10Namespace = "ae7e5100000000000001"
	b10Idle      = "000000000007"
	b10Pressed   = "00000001f915"
)

func mustHex(s string) []byte {
	b, e := hex.DecodeString(s)
	if e != nil {
		panic(e)
	}
	return b
}

func macBytesLE(mac string) []byte {
	b := mustHex(mac)
	out := make([]byte, 6)
	for i := range b {
		out[5-i] = b[i]
	}
	return out
}

func s88(v float64) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(int16(math.Round(v*256))))
	return b
}

// Minew FFE1 0xA1 frame: AD length, 0x16, e1 ff, a1, version, payload..., MAC (little-endian).
func minewFrame(version byte, mac string, payload []byte) string {
	body := append([]byte{0x16, 0xe1, 0xff, 0xa1, version}, payload...)
	body = append(body, macBytesLE(mac)...)
	return hex.EncodeToString(append([]byte{byte(len(body))}, body...))
}

func thFrame(mac string, battery int, temp, humidity float64) string {
	p := append([]byte{byte(battery)}, s88(temp)...)
	return minewFrame(0x01, mac, append(p, s88(humidity)...))
}
func accelFrame(mac string, battery int, x, y, z float64) string {
	p := append([]byte{byte(battery)}, s88(x)...)
	p = append(p, s88(y)...)
	return minewFrame(0x03, mac, append(p, s88(z)...))
}
func infoFrame(mac string, battery int, name string) string {
	// Info frames carry the MAC before the name (offset 9), unlike the other frames.
	body := append([]byte{0x16, 0xe1, 0xff, 0xa1, 0x08, byte(battery)}, macBytesLE(mac)...)
	body = append(body, []byte(name)...)
	return hex.EncodeToString(append([]byte{byte(len(body))}, body...))
}
func vibrationFrame(mac string, battery int, ts uint32, moving bool) string {
	p := make([]byte, 6)
	p[0] = byte(battery)
	binary.BigEndian.PutUint32(p[1:5], ts)
	if moving {
		p[5] = 1
	}
	return minewFrame(0x18, mac, p)
}
func tamperFrame(mac string, battery int, tampered bool) string {
	p := []byte{byte(battery), 0}
	if tampered {
		p[1] = 1
	}
	return minewFrame(0x20, mac, p)
}
func iBeaconFrame(major, minor int, tx int8) string {
	body := append([]byte{0xff, 0x4c, 0x00, 0x02, 0x15}, simUUID...)
	mm := make([]byte, 4)
	binary.BigEndian.PutUint16(mm[0:2], uint16(major))
	binary.BigEndian.PutUint16(mm[2:4], uint16(minor))
	body = append(body, mm...)
	body = append(body, byte(tx))
	return hex.EncodeToString(append([]byte{byte(len(body))}, body...))
}
func eddystoneUID(namespace, instance string, tx int8) string {
	body := append([]byte{0x16, 0xaa, 0xfe, 0x00, byte(tx)}, mustHex(namespace)...)
	body = append(body, mustHex(instance)...)
	return hex.EncodeToString(append([]byte{byte(len(body))}, body...))
}
func eddystoneTLM(millivolts int, temp float64, advCount, uptimeTenths uint32) string {
	p := make([]byte, 14)
	p[0], p[1] = 0x20, 0x00
	binary.BigEndian.PutUint16(p[2:4], uint16(millivolts))
	copy(p[4:6], s88(temp))
	binary.BigEndian.PutUint32(p[6:10], advCount)
	binary.BigEndian.PutUint32(p[10:14], uptimeTenths)
	body := append([]byte{0x16, 0xaa, 0xfe}, p...)
	return hex.EncodeToString(append([]byte{byte(len(body))}, body...))
}

// Scenario flags derived from the step counter so tests and dashboards can predict them.
func E8SMoving(step int) bool     { return step%30 < 10 }
func MBT01Tampered(step int) bool { return step%40 >= 25 && step%40 < 31 }
func B10Pressed(step int) bool    { return step%15 < 2 }

// B10RealPressed drives the realistic B10 (macB10Real). Its window is deliberately its own, so a
// test can pick a quiet step and a pressed step without colliding with the older B10's schedule.
func B10RealPressed(step int) bool { return step%20 >= 12 && step%20 < 15 }

// pressFrame is the advertisement the realistic B10 adds while the button is held: a Minew FFE1 0xA1
// frame whose version the decoder does not know (0x22), carrying a press flag and a distinctive
// marker. The decoder therefore reports it in Reading.Unknown instead of decoding it — exactly what a
// real undocumented press frame does, and exactly the situation "สอนสัญญาณ" exists to resolve.
// Nothing in Aether decodes this frame; it is only ever recognised by a signature an operator taught.
func pressFrame(mac string) string {
	return minewFrame(0x22, mac, []byte{0x4f, 0x01, 0x53, 0x4f, 0x53})
}

// realB10Rows is what the realistic B10 puts on the air in one uplink: accelerometer, info and
// Eddystone-TLM at rest, plus ONE extra advertisement while pressed. The accelerometer axes and the
// TLM counters move on every step, so a learner that does not normalise them would mistake them for
// the press.
func realB10Rows(step int, rssi int, row func(mac, raw string, rssi int) map[string]any) []map[string]any {
	rows := []map[string]any{
		row(macB10Real, accelFrame(macB10Real, 79, 0.03*math.Sin(float64(step)/3), 0.02*math.Cos(float64(step)/5), 0.99), rssi),
		row(macB10Real, eddystoneTLM(3880-step%300, 26+math.Sin(float64(step)/17), uint32(step)*13+7, uint32(step)*50), rssi),
		row(macB10Real, infoFrame(macB10Real, 79, "B10"), rssi),
	}
	if B10RealPressed(step) {
		rows = append(rows, row(macB10Real, pressFrame(macB10Real), rssi))
	}
	return rows
}

// Wearables (C10 card, B7 and B10 wristbands) walk between two virtual gateways, zone A and zone B, on a
// five-minute loop. RSSI falls with distance and a gateway stops hearing the tag below -90 dBm, so for part
// of the loop both gateways hear it and for part only one does.
const (
	walkSteps   = 60
	gatewayMACB = "f000000000b0"
)

// WalkRSSI returns the wearable's RSSI at zone A and zone B; ok=false means out of that gateway's range.
func WalkRSSI(step, offset int) (a int, aOK bool, b int, bOK bool) {
	pos := (1 - math.Cos(2*math.Pi*float64(step+offset)/walkSteps)) / 2 // 0 = next to A, 1 = next to B
	a = int(math.Round(-52 - 46*pos))
	b = int(math.Round(-52 - 46*(1-pos)))
	return a, a >= -90, b, b >= -90
}

func wearableRows(step int, zoneB bool, row func(mac, raw string, rssi int) map[string]any) []map[string]any {
	rows := []map[string]any{}
	// Body shadowing: a deterministic ±7 dB wobble, opposite in phase at the two gateways, so a wearable standing
	// halfway really does look stronger at A on one uplink and at B on the next.
	heard := func(offset int) (int, bool) {
		a, aOK, b, bOK := WalkRSSI(step, offset)
		wobble := int(math.Round(7 * math.Sin(float64(step)*1.9+float64(offset))))
		if zoneB {
			return b - wobble, bOK
		}
		return a + wobble, aOK
	}
	// C10 card beacon: iBeacon identity + gentle accelerometer noise (carried by a person).
	if rssi, ok := heard(0); ok {
		rows = append(rows, row(macC10, iBeaconFrame(1, 5, -59), rssi))
		rows = append(rows, row(macC10, accelFrame(macC10, 88, 0.05*math.Sin(float64(step)/3), 0.04*math.Cos(float64(step)/4), 0.98), rssi))
		if step%12 == 3 {
			rows = append(rows, row(macC10, infoFrame(macC10, 88, "C10"), rssi))
		}
	}
	// B7 button wristband: iBeacon + accelerometer; button encoding is undocumented, so none is simulated.
	if rssi, ok := heard(20); ok {
		rows = append(rows, row(macB7, iBeaconFrame(1, 6, -59), rssi))
		rows = append(rows, row(macB7, accelFrame(macB7, 82, 0.12*math.Sin(float64(step)/2), 0.1*math.Cos(float64(step)/2.5), 0.95), rssi))
		if step%12 == 6 {
			rows = append(rows, row(macB7, infoFrame(macB7, 82, "B7"), rssi))
		}
	}
	// B10 emergency button: Eddystone-UID whose instance switches to the event code while pressed, plus TLM.
	if rssi, ok := heard(40); ok {
		instance := b10Idle
		if B10Pressed(step) {
			instance = b10Pressed
		}
		rows = append(rows, row(macB10, eddystoneUID(b10Namespace, instance, -20), rssi))
		rows = append(rows, row(macB10, eddystoneTLM(3900-step%400, 24+math.Sin(float64(step)/20), uint32(step)*10+1, uint32(step)*50), rssi))
		if step%12 == 9 {
			rows = append(rows, row(macB10, infoFrame(macB10, 79, "B10"), rssi))
		}
	}
	return rows
}

// ZonePacket is the uplink of the second virtual gateway (zone B). It hears only the wearables.
func ZonePacket(step int, at time.Time) []byte {
	ts := at.UnixMilli()
	row := func(mac, raw string, rssi int) map[string]any {
		return map[string]any{"mac": mac, "rawData": raw, "rssi": rssi, "timestamp": ts, "aether_source": "simulated"}
	}
	rows := []map[string]any{{"type": "Gateway", "mac": gatewayMACB, "nums": 3, "timestamp": ts, "aether_source": "simulated"}}
	rows = append(rows, wearableRows(step, true, row)...)
	b, _ := json.Marshal(rows)
	return b
}

// Packet returns one MG3-style uplink for the whole virtual kit (zone A).
func Packet(step int, at time.Time) []byte {
	ts := at.UnixMilli()
	row := func(mac, raw string, rssi int) map[string]any {
		return map[string]any{"mac": mac, "rawData": raw, "rssi": rssi, "timestamp": ts, "aether_source": "simulated"}
	}
	rows := []map[string]any{{"type": "Gateway", "mac": gatewayMAC, "nums": 9, "timestamp": ts, "aether_source": "simulated"}}

	// Four S1-style environmental sensors (the original virtual inventory).
	for i := 0; i < 4; i++ {
		mac := fmt.Sprintf("f000000000%02x", i+1)
		temp := 22 + float64(i)*2 + math.Sin(float64(step)/12+float64(i))
		humidity := 45 + float64(i)*4 + 3*math.Cos(float64(step)/10+float64(i))
		rows = append(rows, row(mac, thFrame(mac, 96-i*8, temp, humidity), -45-i*8))
		if i == 0 && step%12 == 0 {
			rows = append(rows, row(mac, infoFrame(mac, 96, "S1"), -45))
		}
	}
	rows = append(rows, wearableRows(step, false, row)...)
	// E8S asset tag: still most of the time, then a movement burst with the vibration flag set.
	amp := 0.02
	if E8SMoving(step) {
		amp = 0.45
	}
	rows = append(rows, row(macE8S, accelFrame(macE8S, 91, amp*math.Sin(float64(step)), amp*math.Cos(float64(step)*1.3), 1-amp*0.3), -71))
	rows = append(rows, row(macE8S, vibrationFrame(macE8S, 91, uint32(at.Unix()), E8SMoving(step)), -71))
	if step%12 == 1 {
		rows = append(rows, row(macE8S, infoFrame(macE8S, 91, "E8S"), -71))
	}
	// The realistic B10: stationary, always heard, and the only tag whose press is undocumented.
	rows = append(rows, realB10Rows(step, -68, row)...)
	// MBT01 anti-tamper tag: iBeacon identity + tamper flag raised for a short window.
	rows = append(rows, row(macMBT01, iBeaconFrame(1, 9, -59), -74))
	rows = append(rows, row(macMBT01, tamperFrame(macMBT01, 77, MBT01Tampered(step)), -74))
	if step%12 == 4 {
		rows = append(rows, row(macMBT01, infoFrame(macMBT01, 77, "MBT01"), -74))
	}
	b, _ := json.Marshal(rows)
	return b
}

// ---- MOS smart-office kit (MG4 gateway) ----------------------------------------------------------
//
// MOSPacket is a separate scenario so that Packet's output, which many tests pin, never changes. It
// mimics the Minew MOS starter kit as seen through an MG4: JSON-Long rows exactly like MG3, except that
// the MG4 stamps rows with ISO-8601 strings and adds fields of its own (bleName, battery). Aether
// ignores device timestamps and those extras; the scenario carries them so the parser is exercised.

// MOS device MACs, outside any real vendor range like the rest of the simulation.
const (
	MOSGateway = "f000000000c0"
	MOSS1      = "f000000000c1"
	MOSMSP01   = "f000000000c2"
	MOSC10     = "f000000000c3"
	MOSMBT01   = "f000000000c4"
	MOSS4      = "f000000000c5"
)

// MSP01Motion is true for 3 steps out of every 20: someone walks past the PIR.
func MSP01Motion(step int) bool { return step%20 < 3 }

// S4Open is true for 4 steps out of every 30: the door is opened and closed again.
func S4Open(step int) bool { return step%30 >= 20 && step%30 < 24 }

// pirFrame is FFE1 A1-11 (PIR): battery, a 16-bit field whose bit 0 is motion, MAC.
func pirFrame(mac string, battery int, motion bool) string {
	p := []byte{byte(battery), 0, 0}
	if motion {
		p[2] = 1
	}
	return minewFrame(0x11, mac, p)
}

// s4SyntheticFrame is a CLEARLY SYNTHETIC stand-in for the S4 door sensor's combination frame. The real
// byte layout, service UUID and frame/version bytes are NOT public, so this is not the S4's format and
// nothing in Aether decodes it: it uses an FFE1 A1 version the decoder does not know (0x23), so the frame
// lands in Reading.Unknown exactly as an undocumented real frame would, and a door state can only come
// from a signal the operator teaches. The fields follow the order Minew's SDK names for the combination
// frame: doorSensorAlarmStatus (1 open), tamperProofAlarmStatus, triggerAlarmStatus, openCount,
// closeCount, tamperProofCount (each 0-255).
func s4SyntheticFrame(mac string, battery int, step int) string {
	open := S4Open(step)
	opens, closes := step/30, step/30
	if step%30 >= 20 {
		opens++
	}
	if step%30 >= 24 {
		closes++
	}
	p := []byte{byte(battery), 0, 0, 0, byte(opens % 256), byte(closes % 256), 0}
	if open {
		p[1] = 1
	}
	return minewFrame(0x23, mac, p)
}

// MOSPacket returns one MG4-style uplink for the virtual MOS kit.
func MOSPacket(step int, at time.Time) []byte {
	ts := at.UTC().Format(time.RFC3339)
	row := func(mac, raw string, rssi int) map[string]any {
		return map[string]any{"mac": mac, "rawData": raw, "rssi": rssi, "timestamp": ts, "aether_source": "simulated"}
	}
	named := func(r map[string]any, name string, battery int) map[string]any {
		r["bleName"], r["battery"] = name, battery
		return r
	}
	rows := []map[string]any{{"type": "Gateway", "mac": MOSGateway, "timestamp": ts, "aether_source": "simulated"}}

	// S1 temperature / humidity: the one frame verified on real hardware.
	rows = append(rows, named(row(MOSS1, thFrame(MOSS1, 94, 24.5+math.Sin(float64(step)/12), 52+3*math.Cos(float64(step)/10)), -52), "S1", 94))
	if step%12 == 0 {
		rows = append(rows, row(MOSS1, infoFrame(MOSS1, 94, "S1"), -52))
	}
	// MSP01 PIR: what the owner's real unit sends (A1-01, A1-11, A1-03, A1-08), with motion=1 while
	// someone walks past. The real unit reported motion=0 in all 257 samples, so this part is simulated.
	rows = append(rows, named(row(MOSMSP01, pirFrame(MOSMSP01, 88, MSP01Motion(step)), -60), "MSP01", 88))
	rows = append(rows, row(MOSMSP01, thFrame(MOSMSP01, 88, 25.1+0.5*math.Sin(float64(step)/9), 55), -60))
	rows = append(rows, row(MOSMSP01, accelFrame(MOSMSP01, 88, 0, 0.01, 1), -60))
	if step%12 == 2 {
		rows = append(rows, row(MOSMSP01, infoFrame(MOSMSP01, 88, "MSP01"), -60))
	}
	// C10 card beacon.
	rows = append(rows, row(MOSC10, iBeaconFrame(2, 3, -59), -66))
	rows = append(rows, row(MOSC10, accelFrame(MOSC10, 90, 0.05*math.Sin(float64(step)/3), 0.04*math.Cos(float64(step)/4), 0.98), -66))
	if step%12 == 3 {
		rows = append(rows, row(MOSC10, infoFrame(MOSC10, 90, "C10"), -66))
	}
	// MBT01 anti-removal tag (A1-20 tamper, unverified on hardware).
	rows = append(rows, row(MOSMBT01, iBeaconFrame(2, 4, -59), -70))
	rows = append(rows, row(MOSMBT01, tamperFrame(MOSMBT01, 77, MBT01Tampered(step)), -70))
	if step%12 == 4 {
		rows = append(rows, row(MOSMBT01, infoFrame(MOSMBT01, 77, "MBT01"), -70))
	}
	// S4 door sensor: the synthetic combination frame every uplink (it advertises every second), an
	// info frame now and then. Most uplinks therefore carry nothing Aether can decode for this tag.
	rows = append(rows, named(row(MOSS4, s4SyntheticFrame(MOSS4, 95, step), -63), "S4", 95))
	if step%12 == 5 {
		rows = append(rows, row(MOSS4, infoFrame(MOSS4, 95, "S4"), -63))
	}
	b, _ := json.Marshal(rows)
	return b
}
