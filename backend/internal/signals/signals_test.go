package signals

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// The real B10 in the owner's workspace (c300007b573c) advertises only these shapes: an accelerometer
// frame, an info frame, Eddystone-TLM and briefly iBeacon. Nothing here is invented: the builders
// below produce exactly the byte layouts backend/internal/adapters/minew/frames.go parses.
const realMAC = "c300007b573c"

func macLE(mac string) []byte {
	b, e := hex.DecodeString(mac)
	if e != nil {
		panic(e)
	}
	out := make([]byte, 6)
	for i := range b {
		out[5-i] = b[i]
	}
	return out
}

// ad wraps a body in the AD length prefix and renders it the way a gateway reports rawData.
func ad(body ...byte) string {
	return hex.EncodeToString(append([]byte{byte(len(body))}, body...))
}

func minewAD(version byte, battery byte, payload []byte) string {
	body := append([]byte{0x16, 0xe1, 0xff, 0xa1, version, battery}, payload...)
	return ad(append(body, macLE(realMAC)...)...)
}

func accel(step int) string {
	p := make([]byte, 6)
	binary.BigEndian.PutUint16(p[0:2], uint16(int16(step*3)))
	binary.BigEndian.PutUint16(p[2:4], uint16(int16(-step)))
	binary.BigEndian.PutUint16(p[4:6], 0x0100)
	return minewAD(0x03, 0x50, p)
}

func info() string {
	body := append([]byte{0x16, 0xe1, 0xff, 0xa1, 0x08, 0x50}, macLE(realMAC)...)
	return ad(append(body, []byte("B10")...)...)
}

// tlm is the decoy: its battery voltage, temperature, advertisement count and uptime move on every
// single packet, so a naive diff would call it a signal.
func tlm(step int) string {
	p := make([]byte, 14)
	p[0], p[1] = 0x20, 0x00
	binary.BigEndian.PutUint16(p[2:4], uint16(3900-step))
	binary.BigEndian.PutUint16(p[4:6], uint16(int16(6000+step*7)))
	binary.BigEndian.PutUint32(p[6:10], uint32(step)*11+1)
	binary.BigEndian.PutUint32(p[10:14], uint32(step)*50)
	return ad(append([]byte{0x16, 0xaa, 0xfe}, p...)...)
}

func ibeacon() string {
	uuid, _ := hex.DecodeString("a37e0000c0de4be7aab1e2a5e7e70001")
	body := append([]byte{0xff, 0x4c, 0x00, 0x02, 0x15}, uuid...)
	return ad(append(body, 0x00, 0x01, 0x00, 0x0a, 0xc5)...)
}

// press is the undocumented frame the physical tag would emit: a Minew FFE1 0xA1 version the decoder
// does not know, which is precisely why the decoder reports it as unknown instead of as a press.
func press() string { return minewAD(0x22, 0x50, []byte{0x01, 0x00}) }

func tamper(flag byte) string { return minewAD(0x20, 0x50, []byte{flag}) }

// rest is what the tag broadcasts while nobody touches it.
func rest(steps int) []string {
	var out []string
	for s := 0; s < steps; s++ {
		out = append(out, accel(s), tlm(s))
	}
	return append(out, info(), ibeacon())
}

func TestAnalyse(t *testing.T) {
	base := rest(6)
	tests := []struct {
		name     string
		baseline []string
		trigger  []string
		want     int      // number of candidates
		wantKind string   // kind of the top candidate, "" when none is expected
		wantHas  []string // substrings the top candidate's Thai description must contain
	}{
		{
			name: "an unknown frame that only appears while pressed is the one candidate",
			// This is the real B10 case: the decoder cannot read the press frame, but the learner
			// does not need to read it. It only needs to see that it is new.
			baseline: base,
			trigger:  append(rest(6), press(), press(), press()),
			want:     1,
			wantKind: KindFrame,
			wantHas:  []string{"A1-22", "เฉพาะตอนกด", "เห็น 3 ครั้ง"},
		},
		{
			name:     "a flag byte inside a frame both phases carry",
			baseline: []string{tamper(0x00), tamper(0x00), tamper(0x00)},
			trigger:  []string{tamper(0x00), tamper(0x01), tamper(0x01)},
			// The exact byte and the single-bit variant of the same change.
			want:     2,
			wantKind: KindByte,
			wantHas:  []string{"ไบต์ที่ 3", "0x00", "0x01", "เฉพาะตอนงัดแงะ"},
		},
		{
			name:     "an identical baseline and trigger yields nothing",
			baseline: base,
			trigger:  rest(6),
			want:     0,
		},
		{
			name: "Eddystone-TLM alone is never a candidate however much it varies",
			// Its counters differ in every packet of both phases; a diff without normalisation
			// would report the trigger-phase TLM frames as a signal.
			baseline: []string{tlm(0), tlm(1), tlm(2)},
			trigger:  []string{tlm(90), tlm(91), tlm(92)},
			want:     0,
		},
		{
			name:     "an accelerometer that moves harder is not a press",
			baseline: []string{accel(0), accel(1), accel(2)},
			trigger:  []string{accel(40), accel(41), accel(42)},
			want:     0,
		},
		{
			name:     "an unparsable payload falls back to a raw prefix",
			baseline: []string{"0201060303aafe"},
			trigger:  []string{"0201060303aafe", "00", "0201060303aafe"},
			want:     0, // a zero-length terminator carries nothing; there is no honest prefix here
		},
		{
			name:     "a whole new manufacturer structure is reported as a frame",
			baseline: []string{ad(0xff, 0x4c, 0x00, 0x09, 0x08)},
			trigger:  []string{ad(0xff, 0x4c, 0x00, 0x09, 0x08), ad(0xff, 0x2d, 0x01, 0xaa, 0xbb)},
			want:     1,
			wantKind: KindFrame,
			wantHas:  []string{"0x012d"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verb := "กด"
			if strings.Contains(tc.name, "flag byte") {
				verb = TriggerVerb("tamper")
			}
			got := Analyse(tc.baseline, tc.trigger, verb)
			if len(got) != tc.want {
				t.Fatalf("want %d candidates, got %d: %+v", tc.want, len(got), got)
			}
			if tc.want == 0 {
				return
			}
			if got[0].Matcher.Kind != tc.wantKind {
				t.Fatalf("top candidate kind: want %s got %s (%+v)", tc.wantKind, got[0].Matcher.Kind, got[0])
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(got[0].Description, want) {
					t.Fatalf("description %q must contain %q", got[0].Description, want)
				}
			}
			// The contract every candidate must satisfy: it fires on the trigger phase and never at rest.
			for _, c := range got {
				if !Match(c.Matcher, tc.trigger) {
					t.Fatalf("candidate does not match its own trigger phase: %+v", c)
				}
				if Match(c.Matcher, tc.baseline) {
					t.Fatalf("candidate also matches the baseline: %+v", c)
				}
				if e := Validate(c.Matcher); e != nil {
					t.Fatalf("Analyse produced a matcher Validate rejects: %+v %v", c, e)
				}
			}
		})
	}
}

// The exact-byte matcher must outrank the tolerant single-bit one: the owner is protecting against
// false positives first, and can pick the wider variant deliberately.
func TestByteCandidateOrder(t *testing.T) {
	got := Analyse([]string{tamper(0x00), tamper(0x00)}, []string{tamper(0x01)}, "งัดแงะ")
	if len(got) != 2 || got[0].Matcher.Mask != 0xff || got[1].Matcher.Mask != 0x01 {
		t.Fatalf("want exact byte then single bit, got %+v", got)
	}
	if !strings.Contains(got[1].Description, "บิตที่ 0") {
		t.Fatalf("the masked variant must name the bit: %q", got[1].Description)
	}
}

// A byte that already varied at rest is never eligible, even if the trigger value is new: it would
// only be a matter of time before the resting device produced it too.
func TestByteThatMovedDuringBaselineIsRejected(t *testing.T) {
	if got := Analyse([]string{tamper(0x00), tamper(0x02)}, []string{tamper(0x01)}, "งัดแงะ"); len(got) != 0 {
		t.Fatalf("a byte that moved at rest must not become a signature: %+v", got)
	}
}

func TestMatch(t *testing.T) {
	quiet := rest(3)
	tests := []struct {
		name string
		m    Matcher
		raws []string
		want bool
	}{
		{"frame present", Matcher{Kind: KindFrame, Frame: "ffe1:a1:0x22"}, append(quiet, press()), true},
		{"frame absent", Matcher{Kind: KindFrame, Frame: "ffe1:a1:0x22"}, quiet, false},
		{"byte equal under full mask", Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: 3, Mask: 0xff, Value: 0x01}, []string{tamper(0x01)}, true},
		{"byte different", Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: 3, Mask: 0xff, Value: 0x01}, []string{tamper(0x00)}, false},
		{"bit set under mask", Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: 3, Mask: 0x01, Value: 0x01}, []string{tamper(0x03)}, true},
		{"offset past the end of a short frame", Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: 40, Mask: 0xff, Value: 0}, []string{tamper(0x00)}, false},
		{"prefix", Matcher{Kind: KindPrefix, Prefix: "0e16e1ffa122"}, []string{press()}, true},
		{"prefix elsewhere in the payload does not count", Matcher{Kind: KindPrefix, Prefix: "16e1ffa122"}, []string{press()}, false},
		{"empty uplink", Matcher{Kind: KindFrame, Frame: "ffe1:a1:0x22"}, nil, false},
		{"garbage advertisement", Matcher{Kind: KindFrame, Frame: "ffe1:a1:0x22"}, []string{"zz", "", "ff"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.m, tc.raws); got != tc.want {
				t.Fatalf("Match=%v want %v", got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		m    Matcher
		ok   bool
	}{
		{"frame", Matcher{Kind: KindFrame, Frame: "ffe1:a1:0x22"}, true},
		{"byte", Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: 3, Mask: 0xff, Value: 1}, true},
		{"prefix", Matcher{Kind: KindPrefix, Prefix: "0d16e1ff"}, true},
		{"unknown kind", Matcher{Kind: "anything"}, false},
		{"frame without a key", Matcher{Kind: KindFrame}, false},
		{"frame key with a quote", Matcher{Kind: KindFrame, Frame: "ffe1';--"}, false},
		{"zero mask would match every payload", Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: 3, Mask: 0, Value: 0}, false},
		{"value outside the mask can never match", Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: 3, Mask: 0x01, Value: 0x02}, false},
		{"negative offset", Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: -1, Mask: 0xff}, false},
		{"prefix too short to mean anything", Matcher{Kind: KindPrefix, Prefix: "0d"}, false},
		{"prefix with an odd number of nibbles", Matcher{Kind: KindPrefix, Prefix: "0d16e"}, false},
		{"prefix that is not hex", Matcher{Kind: KindPrefix, Prefix: "zzzz"}, false},
		{"uppercase prefix", Matcher{Kind: KindPrefix, Prefix: "0D16E1FF"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if e := Validate(tc.m); (e == nil) != tc.ok {
				t.Fatalf("Validate=%v want ok=%v", e, tc.ok)
			}
		})
	}
}

func TestDescribeStoredMatcher(t *testing.T) {
	tests := []struct {
		m    Matcher
		want string
	}{
		{Matcher{Kind: KindFrame, Frame: "ffe1:a1:0x22"}, "A1-22"},
		{Matcher{Kind: KindByte, Frame: "ffe1:a1:0x20", Offset: 3, Mask: 0xff, Value: 1}, "ไบต์ที่ 3"},
		{Matcher{Kind: KindByte, Frame: "feaa:0x00", Offset: 17, Mask: 0x0f, Value: 5}, "mask 0x0f"},
		{Matcher{Kind: KindPrefix, Prefix: "0d16e1ff"}, "0d16e1ff"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := Describe(tc.m, TriggerVerb("button")); !strings.Contains(got, tc.want) || !strings.Contains(got, "กด") {
				t.Fatalf("Describe=%q must contain %q and the Thai verb", got, tc.want)
			}
		})
	}
}

// A capture is only as good as its evidence: a single trigger advertisement still produces a
// candidate, but the operator can see it was seen once.
func TestCandidateReportsItsEvidence(t *testing.T) {
	got := Analyse(rest(4), append(rest(4), press()), TriggerVerb("button"))
	if len(got) != 1 || got[0].TriggerHits != 1 || got[0].Confidence != "high" {
		t.Fatalf("want one high-confidence candidate seen once: %+v", got)
	}
	if !strings.HasSuffix(got[0].Description, fmt.Sprintf("เห็น %d ครั้ง", 1)) {
		t.Fatalf("description must end with the evidence count: %q", got[0].Description)
	}
}

// Door is taught like any other meaning, but it is a state: the matched pattern means open and its
// absence closed. A byte matcher can only be judged on an uplink that carries its frame.
func TestDoorIsTeachableAndObservedOnlyWithItsFrame(t *testing.T) {
	if !ValidEventType("door") || ClearedEventType("door") != "door_closed" || TriggerVerb("door") != "เปิดประตู" {
		t.Fatal("door must be teachable with a closed counterpart")
	}
	// Synthetic door frame (an A1 version the decoder does not know) with a status byte at offset 3.
	closed := minewAD(0x23, 0x5f, []byte{0, 0, 0, 0, 0, 0})
	open := minewAD(0x23, 0x5f, []byte{1, 0, 0, 1, 0, 0})
	info := ad(0x09, 'S', '4')
	candidates := Analyse([]string{closed}, []string{open}, TriggerVerb("door"))
	if len(candidates) == 0 {
		t.Fatal("opening changes a byte, so there must be a candidate")
	}
	var door *Matcher
	for i := range candidates {
		if m := candidates[i].Matcher; m.Kind == KindByte && m.Offset == 3 && m.Mask == 0xff {
			door = &candidates[i].Matcher
		}
	}
	if door == nil || door.Value != 1 {
		t.Fatalf("the status byte is a candidate: %+v", candidates)
	}
	if !Match(*door, []string{open}) || Match(*door, []string{closed}) {
		t.Fatal("open matches, closed does not")
	}
	if !Observed(*door, []string{closed}) || Observed(*door, []string{info}) {
		t.Fatal("an uplink without the door frame says nothing about the door")
	}
	if !Observed(Matcher{Kind: KindFrame, Frame: "ffe1:a1:0x22"}, []string{info}) {
		t.Fatal("a frame matcher's absence is its answer")
	}
}
