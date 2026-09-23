// Package signals learns what a BLE tag broadcasts when it is triggered, by comparing a "resting"
// baseline capture with a "triggered" capture of the same device. It answers the one question the
// frame documentation cannot: which bytes of this particular tag's advertisements mean "the button
// was pressed" (or tampered / leaking / moving).
//
// It is pure Go with no database access, so the whole ranking and matching logic is unit-testable
// against recorded advertisements. Two entry points:
//
//	Analyse(baseline, trigger, verb) -> ranked candidates, each with a matcher and a Thai explanation
//	Match(matcher, raws)             -> does this uplink's advertisements contain the learned signal?
//
// Design rules that keep a guess from becoming a signature:
//   - every advertisement is split into its AD structures, exactly the way the Minew decoder walks
//     them, so a candidate is described in terms of a frame and not of an opaque byte range;
//   - fields that change on their own (the trailing MAC, the battery byte, accelerometer axes, the
//     Eddystone-TLM counters) are excluded, both from a declared list and empirically from whatever
//     varied during the baseline, so a trivially-varying frame is never mistaken for a signal;
//   - a candidate that also matches even ONE baseline advertisement is rejected outright;
//   - when nothing distinguishes the two phases the result is empty. There is no fallback guess.
package signals

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Matcher shapes, narrowest-to-widest in the sense of "how much structure it relies on".
const (
	// KindFrame: an AD structure / Minew frame version that only appears while triggered.
	KindFrame = "frame"
	// KindByte: a byte at a fixed offset inside an identified frame equals a value under a mask.
	KindByte = "byte"
	// KindPrefix: an exact raw-payload prefix. The last resort, used when nothing parses.
	KindPrefix = "prefix"
)

// MaxCandidates bounds what one session reports, so a noisy capture cannot flood the UI.
const MaxCandidates = 8

// maxRaw mirrors the decoder's limit on one advertisement (hex characters).
const maxRaw = 3300

// Matcher is the stored signature. It is persisted as jsonb in core.device_signals.matcher and is
// evaluated on every uplink, so it must stay small and cheap.
type Matcher struct {
	Kind string `json:"kind"`
	// Frame is the frame key (see frameKey) for kind frame and byte.
	Frame string `json:"frame,omitempty"`
	// Offset is the index into the frame payload (see adStruct.Payload) for kind byte.
	Offset int `json:"offset,omitempty"`
	// Mask and Value: payload[Offset]&Mask == Value. Mask 0xff is an exact byte comparison.
	Mask  int `json:"mask,omitempty"`
	Value int `json:"value,omitempty"`
	// Prefix is a lowercase hex prefix of the whole raw advertisement, for kind prefix.
	Prefix string `json:"prefix,omitempty"`
}

// Candidate is one explanation of the difference between the two phases, ready to show an operator.
type Candidate struct {
	Matcher Matcher `json:"matcher"`
	// Description is Thai and states what was seen, in terms a human can check against the device.
	Description string `json:"description"`
	// TriggerHits is how many trigger-phase advertisements this matcher matches (the evidence).
	TriggerHits int `json:"trigger_hits"`
	// Confidence: high = a whole frame appeared, medium = a byte changed, low = a raw prefix only.
	Confidence string `json:"confidence"`
}

// TriggerVerb is the Thai verb for the phase-2 instruction, used inside candidate descriptions.
func TriggerVerb(eventType string) string {
	switch eventType {
	case "button":
		return "กด"
	case "tamper":
		return "งัดแงะ"
	case "leak":
		return "โดนน้ำ"
	case "motion":
		return "ขยับ"
	case EventDoor:
		return "เปิดประตู"
	}
	return "กระตุ้น"
}

// adStruct is one AD structure of an advertisement, keyed so the same frame from two packets compares.
type adStruct struct {
	Key string
	// Payload starts where the frame's own fields start: at 0xa1 for a Minew FFE1 frame, at the
	// Eddystone frame type, at the iBeacon UUID, at the first body byte otherwise. Offsets in a
	// byte matcher are indices into this slice, which is what makes a description like
	// "ไบต์ที่ 3 ของเฟรม A1-03" mean the same thing to the decoder and to a human reading the table.
	Payload []byte
}

// parseAD walks one advertisement the way minew.DecodeFramesFor does: defensively, never letting a
// malformed structure discard its neighbours.
func parseAD(raw string) []adStruct {
	if len(raw) > maxRaw {
		return nil
	}
	data, e := hex.DecodeString(strings.ToLower(strings.TrimSpace(raw)))
	if e != nil || len(data) == 0 {
		return nil
	}
	var out []adStruct
	for i := 0; i < len(data) && len(out) < 16; {
		n := int(data[i])
		if n == 0 || i+1+n > len(data) {
			break // zero-length terminator or truncated tail
		}
		ad := data[i+1 : i+1+n]
		i += n + 1
		if len(ad) < 1 {
			continue
		}
		key, payload := frameKey(ad[0], ad[1:])
		out = append(out, adStruct{Key: key, Payload: payload})
	}
	return out
}

// frameKey names an AD structure and returns the part of it whose bytes carry meaning. The names are
// stable strings stored inside matchers, so they must never change once a signature exists.
func frameKey(adType byte, body []byte) (string, []byte) {
	switch {
	case adType == 0x16 && len(body) >= 2 && body[0] == 0xe1 && body[1] == 0xff:
		p := body[2:]
		if len(p) >= 2 && p[0] == 0xa1 {
			return fmt.Sprintf("ffe1:a1:0x%02x", p[1]), p
		}
		if len(p) >= 1 {
			return fmt.Sprintf("ffe1:0x%02x", p[0]), p
		}
		return "ffe1", p
	case adType == 0x16 && len(body) >= 2 && body[0] == 0xaa && body[1] == 0xfe:
		p := body[2:]
		if len(p) >= 1 {
			return fmt.Sprintf("feaa:0x%02x", p[0]), p
		}
		return "feaa", p
	case adType == 0xff && len(body) >= 4 && body[0] == 0x4c && body[1] == 0x00 && body[2] == 0x02 && body[3] == 0x15:
		return "ibeacon", body[4:]
	case adType == 0xff && len(body) >= 2:
		// Manufacturer id is little-endian in the advertisement; render it the way vendors quote it.
		return fmt.Sprintf("mfr:0x%04x", int(body[1])<<8|int(body[0])), body[2:]
	case adType == 0x16 && len(body) >= 2:
		return fmt.Sprintf("svc:0x%04x", int(body[1])<<8|int(body[0])), body[2:]
	}
	return fmt.Sprintf("ad:0x%02x", adType), body
}

// declaredVolatile reports the payload offsets of a known frame that change on their own, so they can
// never become a signature. Everything documented as a counter, a battery level or a physical
// measurement is listed here; the MAC is handled separately because it sits at the end.
func declaredVolatile(key string, length int) map[int]bool {
	out := map[int]bool{}
	mark := func(from, to int) {
		for i := from; i <= to && i < length; i++ {
			out[i] = true
		}
	}
	switch {
	case strings.HasPrefix(key, "ffe1:a1:"):
		mark(2, 2) // battery, present in every A1 frame
		if key == "ffe1:a1:0x08" {
			mark(3, 8) // info frame: the MAC sits before the name instead of at the end
			return out
		}
		// Every other A1 version carries the little-endian MAC in its last six bytes.
		mark(length-6, length-1)
		switch key {
		case "ffe1:a1:0x01": // temperature + humidity
			mark(3, 6)
		case "ffe1:a1:0x03": // accelerometer axes
			mark(3, 8)
		case "ffe1:a1:0x05", "ffe1:a1:0x12", "ffe1:a1:0x13": // illuminance, TVOC, temperature
			mark(3, 4)
		case "ffe1:a1:0x18": // vibration timestamp (the flag at offset 7 is deliberately kept)
			mark(3, 6)
		}
	case key == "feaa:0x20": // Eddystone-TLM: battery voltage, temperature, adv_count, uptime
		mark(2, 13)
	}
	return out
}

// Analyse compares the two phases and returns ranked candidates. baseline and trigger are the distinct
// raw advertisement hex strings captured while the device was resting and while it was being triggered.
// verb is the Thai verb for the trigger (see TriggerVerb); it only shapes the descriptions.
//
// The result is empty when nothing in the trigger phase is absent from the baseline. That is a real
// answer, not a failure: it means the gateway received nothing new, and the caller must say so.
func Analyse(baseline, trigger []string, verb string) []Candidate {
	if verb == "" {
		verb = "กระตุ้น"
	}
	baseFrames := index(baseline)
	trigFrames := index(trigger)
	out := []Candidate{}

	// (a) A frame identity that only exists while triggered. The strongest evidence there is: the tag
	// started saying something it never says at rest.
	for key, trig := range trigFrames {
		if len(baseFrames[key]) > 0 {
			continue
		}
		out = append(out, Candidate{
			Matcher:     Matcher{Kind: KindFrame, Frame: key},
			Description: fmt.Sprintf("%s ปรากฏเฉพาะตอน%s", frameLabel(key), verb),
			TriggerHits: len(trig),
			Confidence:  "high",
		})
	}

	// (b) A byte inside a frame both phases carry. Only offsets that held still for the whole baseline
	// are eligible, which is what stops a counter or a drifting measurement from looking like a press.
	for key, trig := range trigFrames {
		base := baseFrames[key]
		if len(base) == 0 {
			continue // already reported as a whole-frame candidate
		}
		out = append(out, byteCandidates(key, base, trig, verb)...)
	}

	// (c) Nothing parsed into a usable difference: fall back to the raw payload itself.
	if len(out) == 0 {
		if c, ok := prefixCandidate(baseline, trigger, verb); ok {
			out = append(out, c)
		}
	}

	// A candidate that also fires at rest is a false positive waiting to happen, whatever its rank.
	kept := out[:0]
	for _, c := range out {
		if matchesAny(c.Matcher, baseline) {
			continue
		}
		kept = append(kept, c)
	}
	out = kept

	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		if out[i].TriggerHits != out[j].TriggerHits {
			return out[i].TriggerHits > out[j].TriggerHits
		}
		return describeKey(out[i].Matcher) < describeKey(out[j].Matcher)
	})
	for i := range out {
		out[i].Description = fmt.Sprintf("%s · เห็น %d ครั้ง", out[i].Description, out[i].TriggerHits)
	}
	if len(out) > MaxCandidates {
		out = out[:MaxCandidates]
	}
	return out
}

// index groups the AD structures of a set of advertisements by frame key.
func index(raws []string) map[string][][]byte {
	out := map[string][][]byte{}
	for _, raw := range raws {
		for _, ad := range parseAD(raw) {
			out[ad.Key] = append(out[ad.Key], ad.Payload)
		}
	}
	return out
}

// byteCandidates finds offsets whose trigger values never occur at rest. It emits an exact-byte
// matcher (the safest) and, when one bit alone explains the change, the single-bit matcher too, so
// the operator can test both against history before trusting either.
func byteCandidates(key string, base, trig [][]byte, verb string) []Candidate {
	shortest := len(base[0])
	for _, p := range base {
		if len(p) < shortest {
			shortest = len(p)
		}
	}
	for _, p := range trig {
		if len(p) < shortest {
			shortest = len(p)
		}
	}
	volatile := declaredVolatile(key, shortest)
	var out []Candidate
	for off := 0; off < shortest; off++ {
		if volatile[off] {
			continue
		}
		baseVals := map[byte]bool{}
		for _, p := range base {
			baseVals[p[off]] = true
		}
		if len(baseVals) > 1 {
			continue // it moved on its own during the baseline: not a signal, whatever it does now
		}
		var resting byte
		for v := range baseVals {
			resting = v
		}
		trigVals := map[byte]int{}
		for _, p := range trig {
			if !baseVals[p[off]] {
				trigVals[p[off]]++
			}
		}
		for value, hits := range trigVals {
			out = append(out, Candidate{
				Matcher:     Matcher{Kind: KindByte, Frame: key, Offset: off, Mask: 0xff, Value: int(value)},
				Description: fmt.Sprintf("ไบต์ที่ %d ของ%s เปลี่ยนจาก 0x%02x เป็น 0x%02x เฉพาะตอน%s", off, frameLabel(key), resting, value, verb),
				TriggerHits: hits,
				Confidence:  "medium",
			})
			// One bit flipping is usually the real encoding of a flag; offer it as the tolerant variant
			// so a firmware that also changes neighbouring bits still matches.
			if bit, ok := singleBit(resting, value); ok {
				mask := byte(1) << bit
				hitsMasked := 0
				for _, p := range trig {
					if p[off]&mask == value&mask {
						hitsMasked++
					}
				}
				out = append(out, Candidate{
					Matcher:     Matcher{Kind: KindByte, Frame: key, Offset: off, Mask: int(mask), Value: int(value & mask)},
					Description: fmt.Sprintf("บิตที่ %d ของไบต์ที่ %d ใน%s ขึ้นเป็น %d เฉพาะตอน%s", bit, off, frameLabel(key), (value&mask)>>bit, verb),
					TriggerHits: hitsMasked,
					Confidence:  "medium",
				})
			}
		}
	}
	return out
}

// singleBit reports the bit index when exactly one bit differs between the resting and triggered value.
func singleBit(resting, value byte) (int, bool) {
	diff := resting ^ value
	if diff == 0 || diff&(diff-1) != 0 {
		return 0, false
	}
	for b := 0; b < 8; b++ {
		if diff == 1<<b {
			return b, true
		}
	}
	return 0, false
}

// prefixCandidate is the last resort, and it speaks only for advertisements that could not be
// decomposed into AD structures at all. An advertisement that DID parse has already had every one of
// its frames and bytes examined above; if none of them survived the volatility and baseline checks,
// then the device did not say anything new, and dressing the same bytes up as a raw prefix would
// turn a rejected guess into a signature. That is the one thing this package must never do.
func prefixCandidate(baseline, trigger []string, verb string) (Candidate, bool) {
	seen := map[string]bool{}
	for _, raw := range baseline {
		seen[strings.ToLower(raw)] = true
	}
	var only []string
	for _, raw := range trigger {
		raw = strings.ToLower(raw)
		if !seen[raw] && len(raw) >= 4 && len(parseAD(raw)) == 0 {
			only = append(only, raw)
		}
	}
	if len(only) == 0 {
		return Candidate{}, false
	}
	sort.Strings(only)
	prefix := only[0]
	for _, raw := range only[1:] {
		prefix = commonPrefix(prefix, raw)
	}
	prefix = prefix[:len(prefix)-len(prefix)%2] // whole bytes only
	if len(prefix) < 4 {
		return Candidate{}, false
	}
	for _, raw := range baseline {
		if strings.HasPrefix(strings.ToLower(raw), prefix) {
			return Candidate{}, false
		}
	}
	return Candidate{
		Matcher:     Matcher{Kind: KindPrefix, Prefix: prefix},
		Description: fmt.Sprintf("สัญญาณดิบที่ขึ้นต้นด้วย %s ปรากฏเฉพาะตอน%s (ยังแยกเป็นเฟรมไม่ได้)", prefix, verb),
		TriggerHits: len(only),
		Confidence:  "low",
	}, true
}

func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

// rank orders the matcher shapes: a whole new frame explains a device better than a changed byte,
// and a raw prefix explains nothing at all beyond "these exact bytes".
func rank(c Candidate) int {
	switch c.Matcher.Kind {
	case KindFrame:
		return 0
	case KindByte:
		if c.Matcher.Mask == 0xff {
			return 1
		}
		return 2
	}
	return 3
}

func describeKey(m Matcher) string {
	return fmt.Sprintf("%s|%s|%d|%d|%d|%s", m.Kind, m.Frame, m.Offset, m.Mask, m.Value, m.Prefix)
}

// Match reports whether any of the advertisements of one uplink carries the learned signal.
func Match(m Matcher, raws []string) bool { return matchesAny(m, raws) }

// Observed reports whether this uplink says anything about the matcher at all. A byte matcher can only
// be judged when the frame it reads is present: an uplink that happened to carry only the tag's info
// frame says nothing about a door byte, and reading it as "no match" would report a door closing that
// never happened. A frame matcher's absence IS its answer (the frame only appears while triggered), and
// a raw prefix cannot tell, so both are always observed.
func Observed(m Matcher, raws []string) bool {
	if m.Kind != KindByte {
		return true
	}
	for _, raw := range raws {
		for _, ad := range parseAD(raw) {
			if ad.Key == m.Frame {
				return true
			}
		}
	}
	return false
}

func matchesAny(m Matcher, raws []string) bool {
	for _, raw := range raws {
		if matchOne(m, raw) {
			return true
		}
	}
	return false
}

func matchOne(m Matcher, raw string) bool {
	if m.Kind == KindPrefix {
		return m.Prefix != "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), strings.ToLower(m.Prefix))
	}
	for _, ad := range parseAD(raw) {
		if ad.Key != m.Frame {
			continue
		}
		if m.Kind == KindFrame {
			return true
		}
		if m.Kind == KindByte && m.Offset >= 0 && m.Offset < len(ad.Payload) && int(ad.Payload[m.Offset])&m.Mask == m.Value {
			return true
		}
	}
	return false
}

// Validate rejects a matcher that could never have come from Analyse, so a hand-written or replayed
// API payload cannot store a signature that matches everything.
func Validate(m Matcher) error {
	switch m.Kind {
	case KindFrame:
		if !validFrame(m.Frame) {
			return fmt.Errorf("signals: invalid frame key")
		}
	case KindByte:
		if !validFrame(m.Frame) {
			return fmt.Errorf("signals: invalid frame key")
		}
		if m.Offset < 0 || m.Offset > 255 || m.Mask <= 0 || m.Mask > 0xff || m.Value < 0 || m.Value > 0xff || m.Value&^m.Mask != 0 {
			return fmt.Errorf("signals: invalid byte matcher")
		}
	case KindPrefix:
		if len(m.Prefix) < 4 || len(m.Prefix) > 64 || len(m.Prefix)%2 != 0 {
			return fmt.Errorf("signals: invalid prefix length")
		}
		if _, e := hex.DecodeString(m.Prefix); e != nil || m.Prefix != strings.ToLower(m.Prefix) {
			return fmt.Errorf("signals: prefix must be lowercase hex")
		}
	default:
		return fmt.Errorf("signals: unknown matcher kind")
	}
	return nil
}

func validFrame(key string) bool {
	if key == "" || len(key) > 32 {
		return false
	}
	for _, r := range key {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ':' || r == 'x' {
			continue
		}
		return false
	}
	return true
}

// frameLabel renders a frame key in Thai the way an operator would look it up in a frame table.
func frameLabel(key string) string {
	switch {
	case strings.HasPrefix(key, "ffe1:a1:0x"):
		v := strings.TrimPrefix(key, "ffe1:a1:0x")
		return fmt.Sprintf("เฟรม Minew FFE1 A1-%s (version 0x%s)", strings.ToUpper(v), v)
	case strings.HasPrefix(key, "ffe1:0x"):
		return fmt.Sprintf("เฟรม Minew FFE1 ชนิด 0x%s", strings.TrimPrefix(key, "ffe1:0x"))
	case key == "feaa:0x00":
		return "เฟรม Eddystone-UID"
	case key == "feaa:0x20":
		return "เฟรม Eddystone-TLM"
	case strings.HasPrefix(key, "feaa:0x"):
		return fmt.Sprintf("เฟรม Eddystone ชนิด 0x%s", strings.TrimPrefix(key, "feaa:0x"))
	case key == "ibeacon":
		return "เฟรม iBeacon"
	case strings.HasPrefix(key, "mfr:0x"):
		return fmt.Sprintf("เฟรมผู้ผลิต 0x%s", strings.TrimPrefix(key, "mfr:0x"))
	case strings.HasPrefix(key, "svc:0x"):
		return fmt.Sprintf("เฟรมบริการ BLE 0x%s", strings.TrimPrefix(key, "svc:0x"))
	case strings.HasPrefix(key, "ad:0x"):
		return fmt.Sprintf("โครงสร้าง AD ชนิด 0x%s", strings.TrimPrefix(key, "ad:0x"))
	}
	return key
}

// Describe re-renders a stored matcher, so a saved signature explains itself in the device list even
// though only the matcher was persisted.
func Describe(m Matcher, verb string) string {
	if verb == "" {
		verb = "กระตุ้น"
	}
	switch m.Kind {
	case KindFrame:
		return fmt.Sprintf("%s ปรากฏเฉพาะตอน%s", frameLabel(m.Frame), verb)
	case KindByte:
		if m.Mask == 0xff {
			return fmt.Sprintf("ไบต์ที่ %d ของ%s เท่ากับ 0x%02x เฉพาะตอน%s", m.Offset, frameLabel(m.Frame), m.Value, verb)
		}
		return fmt.Sprintf("ไบต์ที่ %d ของ%s ภายใต้ mask 0x%02x เท่ากับ 0x%02x เฉพาะตอน%s", m.Offset, frameLabel(m.Frame), m.Mask, m.Value, verb)
	case KindPrefix:
		return fmt.Sprintf("สัญญาณดิบที่ขึ้นต้นด้วย %s เฉพาะตอน%s", m.Prefix, verb)
	}
	return m.Kind
}
