package minew

import (
	"reflect"
	"strings"
	"testing"
)

// The S1 temperature/humidity frame with a real trailing little-endian MAC (aa:bb:cc:dd:ee:ff).
const thWithMAC = "1016e1ffa101641c8a2f51ffeeddccbbaa"

func sameSet(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// Real firmware revisions differ from the published frame tables: a frame one byte longer than the
// document, a truncated advertisement or an AD structure Aether has no rule for must never silently
// swallow the frames next to it.
func TestDecodeIsDefensiveAboutRealAdvertisements(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		adv       string
		wantOK    bool
		frames    []string
		unknown   []string
		metrics   map[string]float64
		noMetrics []string
		check     func(t *testing.T, r Reading)
	}{
		{
			name: "documented length still decodes", raw: thWithMAC, adv: "aabbccddeeff", wantOK: true,
			frames: []string{FrameTH},
			check: func(t *testing.T, r Reading) {
				if r.Temperature != 28.5390625 || r.Humidity != 47.31640625 || r.Battery != 100 {
					t.Fatalf("values: %+v", r)
				}
			},
		},
		{
			// Two bytes a newer firmware appends between the payload and the MAC: the documented
			// fields keep their offsets from the start and the MAC is found from the end.
			name: "longer than documented is tolerated", raw: "1216e1ffa101641c8a2f51deadffeeddccbbaa", adv: "aabbccddeeff", wantOK: true,
			frames: []string{FrameTH},
			check: func(t *testing.T, r Reading) {
				if r.Temperature != 28.5390625 || r.Humidity != 47.31640625 || r.Battery != 100 {
					t.Fatalf("values: %+v", r)
				}
			},
		},
		{
			name: "longer than documented accelerometer frame", raw: "1416e1ffa1034affe7004500fadead55daba50e1fe", wantOK: true,
			frames: []string{FrameAccel}, metrics: map[string]float64{"accel_y": 69.0 / 256},
		},
		{
			name: "shorter than documented is skipped", raw: "0f16e1ffa101641c8a2f510000000000", wantOK: false,
			unknown: []string{"ffe1:a1:0x01:short=12"},
		},
		{
			// The gateway cut the advertisement in half: keep whatever was already decoded.
			name: "truncated tail after a valid frame", raw: thWithMAC + "1016e1ffa101641c8a2f51", wantOK: true,
			frames: []string{FrameTH}, unknown: []string{"ad:truncated:len=16"},
		},
		{
			name: "truncated tail with nothing decoded", raw: "0201061016e1ffa101641c8a2f510000", wantOK: false,
			unknown: []string{"ad:truncated:len=16"},
		},
		{
			name: "unknown structure before a valid frame", raw: "03ff1234" + thWithMAC, wantOK: true,
			frames: []string{FrameTH}, unknown: []string{"ad:0xff:len=3"},
		},
		{
			name: "unknown structure after a valid frame", raw: thWithMAC + "03ff1234", wantOK: true,
			frames: []string{FrameTH}, unknown: []string{"ad:0xff:len=3"},
		},
		{
			name: "unknown minew frame version", raw: "1116e1ffa122640000000000000000000000", wantOK: false,
			unknown: []string{"ffe1:a1:0x22:len=14"},
		},
		{
			name: "unknown minew frame version next to a valid one", raw: "1116e1ffa122640000000000000000000000" + thWithMAC, wantOK: true,
			frames: []string{FrameTH}, unknown: []string{"ffe1:a1:0x22:len=14"},
		},
		{
			name: "unknown eddystone frame type", raw: "0616aafe300102", wantOK: false,
			unknown: []string{"ad:0x16:uuid=feaa:type=0x30"},
		},
		{
			name: "advertisement of only unknown structures", raw: "03ff1234" + "0616aafe300102", wantOK: false,
			unknown: []string{"ad:0xff:len=3", "ad:0x16:uuid=feaa:type=0x30"},
		},
		{
			// A zero length byte terminates the advertisement; the padding behind it is not an error.
			name: "zero length terminator", raw: thWithMAC + "00" + "aabbcc", wantOK: true,
			frames: []string{FrameTH},
		},
		{
			name: "advertisement that is only padding", raw: "00000000", wantOK: false,
		},
		{
			// The payload identity and the address the gateway reported disagree: keep the metrics,
			// flag the disagreement so an operator can investigate a cloned or misconfigured tag.
			name: "mac mismatch keeps decoding", raw: thWithMAC, adv: "112233445566", wantOK: true,
			frames: []string{FrameTH}, unknown: []string{"ffe1:a1:0x01:mac-mismatch"},
			check: func(t *testing.T, r Reading) {
				if r.Temperature != 28.5390625 {
					t.Fatalf("metrics must survive a mac mismatch: %+v", r)
				}
			},
		},
		{
			name: "redacted all-zero mac is not a mismatch", raw: "1016e1ffa101641c8a2f51000000000000", adv: "aabbccddeeff", wantOK: true,
			frames: []string{FrameTH},
		},
		{
			name: "info frame mac is read from its own offset", raw: "0e16e1ffa10864ffeeddccbbaa5331", adv: "aabbccddeeff", wantOK: true,
			frames: []string{FrameInfo},
			check: func(t *testing.T, r Reading) {
				if r.Model != "S1" {
					t.Fatalf("model: %+v", r)
				}
			},
		},
		{
			name: "battery out of range drops only the battery", raw: "1016e1ffa101ff1c8a2f51ffeeddccbbaa", adv: "aabbccddeeff", wantOK: true,
			frames: []string{FrameTH}, unknown: []string{"ffe1:a1:0x01:battery=oor"},
			check: func(t *testing.T, r Reading) {
				if r.Battery != 0 || r.Temperature != 28.5390625 || r.Humidity != 47.31640625 {
					t.Fatalf("values: %+v", r)
				}
			},
		},
		{
			// 0x7fff/256 = 127.99 °C: outside the envelope, and kind=environment promises a usable
			// temperature to the alert engine, so the frame is dropped rather than half-trusted.
			name: "temperature out of range drops the environment frame", raw: "1016e1ffa101647fff2f51ffeeddccbbaa", wantOK: false,
			unknown: []string{"ffe1:a1:0x01:temperature=oor"},
		},
		{
			name: "temperature out of range in the temperature-only frame", raw: "0e16e1ffa113647fffffeeddccbbaa", wantOK: false,
			unknown: []string{"ffe1:a1:0x13:temperature=oor"},
		},
		{
			// Humidity is dropped alone, but the frame is reported under a different decoder id so
			// nothing downstream reads the resulting zero as 0 %RH.
			name: "humidity out of range keeps the temperature", raw: "1016e1ffa101641c8affffffeeddccbbaa", wantOK: true,
			frames: []string{FrameTHTemp}, unknown: []string{"ffe1:a1:0x01:humidity=oor"},
			metrics: map[string]float64{"temperature_only": 1},
			check: func(t *testing.T, r Reading) {
				if r.Temperature != 28.5390625 || r.Humidity != 0 || r.Kind != KindEnvironment {
					t.Fatalf("values: %+v", r)
				}
				if hasFrame(r.Frames, FrameTH) {
					t.Fatal("a partial A1/01 must not claim the verified temperature+humidity decoder id")
				}
			},
		},
		{
			name: "one accelerometer axis out of range", raw: "1216e1ffa103647fff004500faffeeddccbbaa", wantOK: true,
			frames: []string{FrameAccel}, unknown: []string{"ffe1:a1:0x03:accel_x=oor"},
			metrics:   map[string]float64{"accel_y": 69.0 / 256, "accel_z": 250.0 / 256},
			noMetrics: []string{"accel_x", "accel_g"},
		},
		{
			// The tamper bit raises alerts and its exact position is unverified on hardware, so an
			// implausible battery byte (a sign the layout is not the one we think) suppresses it.
			name: "tamper frame with an implausible battery is not trusted", raw: "0d16e1ffa120ff01aabbccddeeff", wantOK: false,
			unknown: []string{"ffe1:a1:0x20:battery=oor"},
		},
		{
			name: "tamper frame with a plausible battery still decodes", raw: "0d16e1ffa1205a01aabbccddeeff", wantOK: true,
			frames: []string{FrameTamper}, metrics: map[string]float64{"tamper": 1},
		},
		{
			name: "leak frame with an implausible battery is not trusted", raw: "0d16e1ffa121ff01aabbccddeeff", wantOK: false,
			unknown: []string{"ffe1:a1:0x21:battery=oor"},
		},
		{
			name: "invalid info-frame name is skipped, not fatal", raw: "0e16e1ffa10864aabbccddeeff53ff" + thWithMAC, wantOK: true,
			frames: []string{FrameTH}, unknown: []string{"ffe1:a1:0x08:name-invalid"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := DecodeFramesFor(tc.raw, tc.adv)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (%+v)", ok, tc.wantOK, r)
			}
			if !sameSet(r.Frames, tc.frames) {
				t.Fatalf("frames=%v want %v", r.Frames, tc.frames)
			}
			if tc.unknown != nil && !sameSet(r.Unknown, tc.unknown) {
				t.Fatalf("unknown=%v want %v", r.Unknown, tc.unknown)
			}
			for k, v := range tc.metrics {
				if r.Metrics[k] != v {
					t.Fatalf("metric %s=%v want %v (%v)", k, r.Metrics[k], v, r.Metrics)
				}
			}
			for _, k := range tc.noMetrics {
				if _, present := r.Metrics[k]; present {
					t.Fatalf("metric %s must be absent: %v", k, r.Metrics)
				}
			}
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
}

// Unknown is stored on every sample, so it must stay small whatever a broken advertiser sends.
func TestUnknownListIsBounded(t *testing.T) {
	raw := strings.Repeat("03ff1234", 4) + strings.Repeat("03fe1234", 4) + strings.Repeat("03fd1234", 4)
	r, _ := DecodeFrames(raw)
	if len(r.Unknown) > maxUnknown {
		t.Fatalf("unknown not bounded: %v", r.Unknown)
	}
	long := Reading{}
	long.addUnknown(strings.Repeat("x", 200))
	if len(long.Unknown[0]) != maxUnknownDesc {
		t.Fatalf("descriptor not clamped: %d", len(long.Unknown[0]))
	}
}
