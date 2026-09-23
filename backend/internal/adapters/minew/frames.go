package minew

// Advertisement frame decoders for the Minew MHS starter kit and the standard beacon formats
// those tags interleave (iBeacon, Eddystone UID/TLM). Byte layouts follow the public Minew
// FFE1 0xA1 frame table (as documented by reelyactive/advlib-ble-services, MIT) and the Apple
// iBeacon / Google Eddystone specifications. Only FFE1 A1/01 (temperature & humidity) has been
// confirmed against physical hardware in this repository; every other decoder is marked
// Verified=false in the catalog until a captured packet from the real tag is added as a golden test.
//
// Robustness rules (real firmware revisions differ from the PDFs):
//   - every AD structure is walked defensively and one malformed structure never rejects the others;
//   - a known frame version decodes when the payload is at least the documented minimum length,
//     trailing bytes a newer firmware appends are tolerated and the little-endian MAC is located
//     from the end of the payload, not at a fixed offset;
//   - an implausible value is dropped per FIELD, the rest of the frame still decodes, and what was
//     seen but not understood is listed in Reading.Unknown so an operator can see it in the UI/API.

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Decoder ids recorded on stored samples so a later re-decode can tell which rule produced a value.
const (
	FrameTH        = "minew-ffe1-a101@1"
	FrameTHTemp    = "minew-ffe1-a101@1#temp" // A1/01 whose humidity byte pair was out of range
	FrameLight     = "minew-ffe1-a102@1"
	FrameAccel     = "minew-ffe1-a103@1"
	FrameIllum     = "minew-ffe1-a105@1"
	FrameInfo      = "minew-ffe1-a108@1"
	FramePIR       = "minew-ffe1-a111@1"
	FrameTVOC      = "minew-ffe1-a112@1"
	FrameTemp      = "minew-ffe1-a113@1"
	FrameVibration = "minew-ffe1-a118@1"
	FrameTamper    = "minew-ffe1-a120@1"
	FrameLeak      = "minew-ffe1-a121@1"
	FrameIBeacon   = "ibeacon@1"
	FrameEddyUID   = "eddystone-uid@1"
	FrameEddyTLM   = "eddystone-tlm@1"
)

// Reading kinds, ordered by how much they say about what the device is.
const (
	KindEnvironment = "environment"
	KindTamper      = "tamper"
	KindLeak        = "leak"
	KindMotion      = "motion"
	KindLight       = "light"
	KindBeacon      = "beacon"
	KindInfo        = "info"
	// KindDoor: a door/window contact whose `door` metric (1 = open) is known. No frame decoder produces it
	// yet (the S4 layout is not public); a learned signal sets it (see postgres.applyLearnedDoors).
	KindDoor = "door"
)

var kindRank = map[string]int{KindDoor: 7, KindEnvironment: 6, KindTamper: 5, KindLeak: 4, KindMotion: 3, KindLight: 2, KindBeacon: 1, KindInfo: 0}

// Plausibility envelope per field. A value outside it is dropped; the other fields of the same
// frame are kept. The bounds are deliberately wider than any sane indoor reading: they exist to
// catch a misread layout, not to filter real data.
const (
	tempMinC          = -60.0
	tempMaxC          = 120.0
	accelMaxG         = 16.0
	batteryMaxPercent = 100
)

// Unknown descriptors are bounded so a hostile or broken advertiser cannot grow a stored sample.
const (
	maxUnknown     = 8
	maxUnknownDesc = 40
)

// Beacon carries the identity fields of an iBeacon or Eddystone frame.
type Beacon struct {
	Type      string  `json:"type"` // ibeacon | eddystone_uid | eddystone_tlm
	UUID      string  `json:"uuid,omitempty"`
	Major     int     `json:"major,omitempty"`
	Minor     int     `json:"minor,omitempty"`
	Namespace string  `json:"namespace,omitempty"`
	Instance  string  `json:"instance,omitempty"`
	TxPower   int     `json:"tx_power,omitempty"`
	Voltage   float64 `json:"voltage,omitempty"`   // TLM battery, volts
	AdvCount  uint32  `json:"adv_count,omitempty"` // TLM
	Uptime    float64 `json:"uptime_s,omitempty"`  // TLM, seconds
}

// DecodeFrames parses one complete advertisement (hex) and merges every recognised frame into a
// Reading. ok is false when nothing in the advertisement is understood; the returned Reading still
// carries Unknown then, so a caller can report "the tag is talking, Aether does not understand it".
func DecodeFrames(raw string) (Reading, bool) { return DecodeFramesFor(raw, "") }

// DecodeFramesFor is DecodeFrames with the advertiser address the gateway reported, which lets the
// decoder cross-check the MAC a Minew frame carries in its payload.
func DecodeFramesFor(raw, advertiser string) (Reading, bool) {
	if len(raw) > 3300 {
		return Reading{}, false
	}
	data, e := hex.DecodeString(raw)
	if e != nil {
		return Reading{}, false
	}
	adv := normaliseMAC(advertiser)
	r := Reading{Metrics: map[string]float64{}}
	found := false
	for i := 0; i < len(data); {
		n := int(data[i])
		if n == 0 {
			break // zero-length terminator: the rest of the advertisement is padding
		}
		if i+1+n > len(data) {
			// Truncated tail: keep everything already decoded instead of dropping the advertisement.
			r.addUnknown(fmt.Sprintf("ad:truncated:len=%d", n))
			break
		}
		ad := data[i+1 : i+1+n]
		i += n + 1
		adType, body := ad[0], ad[1:]
		var ok bool
		switch {
		case adType == 0x16 && len(body) >= 2 && body[0] == 0xe1 && body[1] == 0xff:
			ok = decodeMinew(body[2:], &r, adv)
		case adType == 0x16 && len(body) >= 2 && body[0] == 0xaa && body[1] == 0xfe:
			ok = decodeEddystone(body[2:], &r)
		case adType == 0xff && len(body) >= 4 && body[0] == 0x4c && body[1] == 0x00 && body[2] == 0x02 && body[3] == 0x15:
			ok = decodeIBeacon(body[4:], &r)
		default:
			if !benignAD(adType) {
				r.addUnknown(fmt.Sprintf("ad:0x%02x:len=%d", adType, len(ad)))
			}
			continue
		}
		if ok {
			found = true // a structure we could not parse is skipped, never fatal to its neighbours
		}
	}
	if !found {
		return Reading{Unknown: r.Unknown}, false
	}
	if len(r.Metrics) == 0 {
		r.Metrics = nil
	}
	return r, true
}

// benignAD lists the standard AD types that carry no sensor payload, so they are not reported as
// "seen but not understood" noise.
func benignAD(t byte) bool {
	switch t {
	case 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0d, 0x10, 0x12, 0x14, 0x15, 0x19, 0x1b, 0x24:
		return true
	}
	return false
}

func normaliseMAC(s string) string {
	s = strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(s)))
	if b, e := hex.DecodeString(s); e != nil || len(b) != 6 {
		return ""
	}
	return s
}

// macFromLE renders the little-endian MAC a Minew frame carries. An all-zero field means the bytes
// were redacted (the S1 golden sample) or the firmware left them empty, never a real address.
func macFromLE(b []byte) string {
	out := make([]byte, 6)
	zero := true
	for i := 0; i < 6; i++ {
		out[i] = b[5-i]
		if b[i] != 0 {
			zero = false
		}
	}
	if zero {
		return ""
	}
	return hex.EncodeToString(out)
}

func (r *Reading) addFrame(id, kind string) {
	r.Frames = append(r.Frames, id)
	if kindRank[kind] >= kindRank[r.Kind] || r.Kind == "" {
		r.Kind = kind
	}
}

// addUnknown records a compact descriptor of something seen but not understood, bounded in both
// count and length so it stays cheap to store and safe to render.
func (r *Reading) addUnknown(desc string) {
	if len(desc) > maxUnknownDesc {
		desc = desc[:maxUnknownDesc]
	}
	for _, x := range r.Unknown {
		if x == desc {
			return
		}
	}
	if len(r.Unknown) >= maxUnknown {
		return
	}
	r.Unknown = append(r.Unknown, desc)
}

func signed88(b []byte) float64 { return float64(int16(binary.BigEndian.Uint16(b))) / 256 }

// minewMinLen is the documented payload length of each FFE1 0xA1 frame version, counting from the
// 0xa1 byte. A frame is accepted at this length or longer; trailing bytes are ignored except for
// the little-endian MAC, which is read from the end.
var minewMinLen = map[byte]int{0x01: 13, 0x02: 10, 0x03: 15, 0x05: 11, 0x08: 9, 0x11: 11, 0x12: 11, 0x13: 11, 0x18: 14, 0x20: 10, 0x21: 10}

func decodeMinew(p []byte, r *Reading, adv string) bool {
	if len(p) < 2 {
		r.addUnknown(fmt.Sprintf("ffe1:short=%d", len(p)))
		return false
	}
	if p[0] != 0xa1 {
		r.addUnknown(fmt.Sprintf("ffe1:0x%02x:len=%d", p[0], len(p)))
		return false
	}
	version := p[1]
	min, known := minewMinLen[version]
	if !known {
		r.addUnknown(fmt.Sprintf("ffe1:a1:0x%02x:len=%d", version, len(p)))
		return false
	}
	if len(p) < min {
		r.addUnknown(fmt.Sprintf("ffe1:a1:0x%02x:short=%d", version, len(p)))
		return false
	}
	// The MAC sits at the end of every version except 0x08 (info), which carries it before the name.
	// Locating it from the end keeps a longer-than-documented frame decodable.
	frameMAC := macFromLE(p[len(p)-6:])
	if version == 0x08 {
		frameMAC = macFromLE(p[3:9])
	}
	if adv != "" && frameMAC != "" && frameMAC != adv {
		// Keep decoding the metrics; only flag that the payload identity and the advertiser disagree.
		r.addUnknown(fmt.Sprintf("ffe1:a1:0x%02x:mac-mismatch", version))
	}
	battery, batteryOK := int(p[2]), true
	if battery > batteryMaxPercent {
		batteryOK = false
		r.addUnknown(fmt.Sprintf("ffe1:a1:0x%02x:battery=oor", version))
	}
	oor := func(field string) { r.addUnknown(fmt.Sprintf("ffe1:a1:0x%02x:%s=oor", version, field)) }
	ok := true
	switch version {
	case 0x01:
		t, h := signed88(p[3:5]), signed88(p[5:7])
		if t < tempMinC || t > tempMaxC {
			// An environment frame with no trustworthy temperature is not an environment reading:
			// consumers treat kind=environment as "temperature is valid", so the frame is dropped.
			oor("temperature")
			return false
		}
		r.Temperature = t
		if h >= 0 && h <= 100 {
			r.Humidity = h
			r.addFrame(FrameTH, KindEnvironment)
		} else {
			// Humidity dropped: report a distinct decoder id so nothing reads the zero as 0 %RH.
			oor("humidity")
			r.Metrics["temperature_only"] = 1
			r.addFrame(FrameTHTemp, KindEnvironment)
		}
	case 0x13:
		t := signed88(p[3:5])
		if t < tempMinC || t > tempMaxC {
			oor("temperature")
			return false
		}
		r.Temperature = t
		r.Metrics["temperature_only"] = 1
		r.addFrame(FrameTemp, KindEnvironment)
	case 0x02:
		r.Metrics["light"] = float64(p[3] & 1)
		r.addFrame(FrameLight, KindLight)
	case 0x05:
		r.Metrics["illuminance"] = float64(binary.BigEndian.Uint16(p[3:5]))
		r.addFrame(FrameIllum, KindLight)
	case 0x03:
		axes := [3]float64{signed88(p[3:5]), signed88(p[5:7]), signed88(p[7:9])}
		names := [3]string{"accel_x", "accel_y", "accel_z"}
		all := true
		for i, v := range axes {
			if math.Abs(v) > accelMaxG {
				oor(names[i])
				all = false
				continue
			}
			r.Metrics[names[i]] = v
		}
		if all {
			r.Metrics["accel_g"] = math.Round(math.Sqrt(axes[0]*axes[0]+axes[1]*axes[1]+axes[2]*axes[2])*1000) / 1000
		}
		r.addFrame(FrameAccel, KindMotion)
	case 0x11:
		r.Metrics["motion"] = float64(binary.BigEndian.Uint16(p[3:5]) & 1)
		r.addFrame(FramePIR, KindMotion)
	case 0x12:
		r.Metrics["tvoc"] = float64(binary.BigEndian.Uint16(p[3:5])) / 1000
		r.addFrame(FrameTVOC, KindEnvironment)
	case 0x18:
		r.Metrics["vibration"] = float64(p[7] & 1)
		r.Metrics["vibration_ts"] = float64(binary.BigEndian.Uint32(p[3:7]))
		r.addFrame(FrameVibration, KindMotion)
	case 0x20:
		// Tamper is bit 0 of p[3], the byte right after the battery byte, per the public A1-20 table.
		// The exact bit is NOT confirmed on hardware (see docs/hardware-bringup.md) and it raises
		// alerts, so it is only emitted when the frame is otherwise self-consistent: an implausible
		// battery byte means we are most likely reading a different firmware's layout, and a false
		// tamper alert costs more than a missing metric. The bit is read as-is; nothing else counts
		// as "set".
		if !batteryOK {
			return false
		}
		r.Metrics["tamper"] = float64(p[3] & 1)
		r.addFrame(FrameTamper, KindTamper)
	case 0x21:
		// Same rule as tamper: a leak flag drives alerts, so it needs a self-consistent frame.
		if !batteryOK {
			return false
		}
		r.Metrics["leak"] = float64(p[3] & 1)
		r.addFrame(FrameLeak, KindLeak)
	case 0x08:
		name := strings.TrimRight(string(p[9:]), "\x00")
		if !utf8.ValidString(name) || len(name) > 32 || strings.ContainsAny(name, "\r\n\x00") {
			r.addUnknown("ffe1:a1:0x08:name-invalid")
			return false
		}
		r.Model = strings.TrimSpace(name)
		r.addFrame(FrameInfo, KindInfo)
	default:
		ok = false
	}
	if ok && batteryOK {
		r.Battery = battery
	}
	return ok
}

func decodeEddystone(p []byte, r *Reading) bool {
	if len(p) < 2 {
		r.addUnknown(fmt.Sprintf("ad:0x16:uuid=feaa:short=%d", len(p)))
		return false
	}
	switch p[0] {
	case 0x00: // UID: tx power, 10-byte namespace, 6-byte instance, optional 2 RFU bytes
		if len(p) < 18 {
			r.addUnknown(fmt.Sprintf("ad:0x16:uuid=feaa:uid:short=%d", len(p)))
			return false
		}
		r.Beacon = &Beacon{Type: "eddystone_uid", TxPower: int(int8(p[1])), Namespace: hex.EncodeToString(p[2:12]), Instance: hex.EncodeToString(p[12:18])}
		r.addFrame(FrameEddyUID, KindBeacon)
	case 0x20: // TLM v0: battery mV, temperature 8.8, advertisement count, 0.1s uptime counter
		if len(p) < 14 || p[1] != 0x00 {
			r.addUnknown(fmt.Sprintf("ad:0x16:uuid=feaa:tlm:len=%d", len(p)))
			return false
		}
		b := &Beacon{Type: "eddystone_tlm", Voltage: float64(binary.BigEndian.Uint16(p[2:4])) / 1000, AdvCount: binary.BigEndian.Uint32(p[6:10]), Uptime: float64(binary.BigEndian.Uint32(p[10:14])) / 10}
		if r.Beacon == nil {
			r.Beacon = b
		} else { // keep identity from UID/iBeacon, add telemetry
			r.Beacon.Voltage, r.Beacon.AdvCount, r.Beacon.Uptime = b.Voltage, b.AdvCount, b.Uptime
		}
		r.Metrics["voltage"] = b.Voltage
		t := signed88(p[4:6])
		switch {
		case t == -128: // 0x8000 means "not supported"
		case t < tempMinC || t > tempMaxC:
			r.addUnknown("ad:0x16:uuid=feaa:tlm:temp=oor")
		default:
			r.Metrics["tlm_temperature"] = t
		}
		r.addFrame(FrameEddyTLM, KindBeacon)
	default:
		r.addUnknown(fmt.Sprintf("ad:0x16:uuid=feaa:type=0x%02x", p[0]))
		return false
	}
	return true
}

func decodeIBeacon(p []byte, r *Reading) bool {
	if len(p) < 21 {
		r.addUnknown(fmt.Sprintf("ad:0xff:ibeacon:short=%d", len(p)))
		return false
	}
	u := hex.EncodeToString(p[0:16])
	b := &Beacon{Type: "ibeacon", UUID: fmt.Sprintf("%s-%s-%s-%s-%s", u[0:8], u[8:12], u[12:16], u[16:20], u[20:32]), Major: int(binary.BigEndian.Uint16(p[16:18])), Minor: int(binary.BigEndian.Uint16(p[18:20])), TxPower: int(int8(p[20]))}
	if r.Beacon != nil && r.Beacon.Type == "eddystone_tlm" {
		b.Voltage, b.AdvCount, b.Uptime = r.Beacon.Voltage, r.Beacon.AdvCount, r.Beacon.Uptime
	}
	r.Beacon = b
	r.addFrame(FrameIBeacon, KindBeacon)
	return true
}

// Merge folds another advertisement from the same device in the same uplink into r.
func (r *Reading) Merge(o Reading) {
	if r.Kind == "" || kindRank[o.Kind] > kindRank[r.Kind] {
		r.Kind = o.Kind
	}
	for _, f := range o.Frames {
		dup := false
		for _, x := range r.Frames {
			if x == f {
				dup = true
			}
		}
		if !dup {
			r.Frames = append(r.Frames, f)
		}
	}
	for _, u := range o.Unknown {
		r.addUnknown(u)
	}
	if o.Model != "" {
		r.Model = o.Model
	}
	if o.Battery > 0 {
		r.Battery = o.Battery
	}
	if hasFrame(o.Frames, FrameTH) || hasFrame(o.Frames, FrameTemp) || hasFrame(o.Frames, FrameTHTemp) {
		r.Temperature, r.Humidity = o.Temperature, o.Humidity
	}
	if o.Beacon != nil {
		if r.Beacon == nil || o.Beacon.Type != "eddystone_tlm" {
			tlm := r.Beacon
			r.Beacon = o.Beacon
			if tlm != nil && tlm.Voltage > 0 && r.Beacon.Voltage == 0 {
				r.Beacon.Voltage, r.Beacon.AdvCount, r.Beacon.Uptime = tlm.Voltage, tlm.AdvCount, tlm.Uptime
			}
		} else {
			r.Beacon.Voltage, r.Beacon.AdvCount, r.Beacon.Uptime = o.Beacon.Voltage, o.Beacon.AdvCount, o.Beacon.Uptime
		}
	}
	if len(o.Metrics) > 0 {
		if r.Metrics == nil {
			r.Metrics = map[string]float64{}
		}
		for k, v := range o.Metrics {
			r.Metrics[k] = v
		}
	}
}

func hasFrame(frames []string, id string) bool {
	for _, f := range frames {
		if f == id {
			return true
		}
	}
	return false
}

// DisplayName derives a neutral label from what was actually decoded; it never claims a model
// that the frames do not identify.
func DisplayName(r Reading) string {
	if r.Model != "" {
		return "Minew " + r.Model
	}
	switch r.Kind {
	case KindEnvironment:
		return "Minew temperature & humidity"
	case KindTamper:
		return "Minew anti-tamper tag"
	case KindLeak:
		return "Minew leak sensor"
	case KindMotion:
		return "Minew motion / accelerometer tag"
	case KindLight:
		return "Minew light sensor"
	case KindBeacon:
		if r.Beacon != nil && r.Beacon.Type == "ibeacon" {
			return "BLE beacon (iBeacon)"
		}
		return "BLE beacon (Eddystone)"
	}
	return "BLE device"
}

// GenericNames lists every auto-generated display name, so storage can tell them apart from user-assigned ones.
func GenericNames() []string {
	base := []string{"Minew temperature & humidity", "Minew anti-tamper tag", "Minew leak sensor", "Minew motion / accelerometer tag", "Minew light sensor", "BLE beacon (iBeacon)", "BLE beacon (Eddystone)", "BLE device"}
	out := make([]string, 0, len(base)*2)
	for _, n := range base {
		out = append(out, n, "SIM · ข้อมูลจำลอง · "+n)
	}
	return out
}
