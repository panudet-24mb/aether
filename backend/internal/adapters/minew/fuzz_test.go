package minew

import (
	"aether/backend/internal/domain"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Frames produced by backend/internal/simulation for the whole MHS kit, plus the hardware S1 sample
// and the shapes the robustness rules care about. They are the fuzzer's starting corpus.
var seedAdvertisements = []string{
	"0201061016e1ffa101641c8a2f51000000000000",                                         // S1 A1/01, hardware sample
	"1016e1ffa101641c8a2f51ffeeddccbbaa",                                               // A1/01 with a real trailing MAC
	"0e16e1ffa10864aabbccddeeff5331",                                                   // A1/08 info, name "S1"
	"1216e1ffa1034affe7004500fa55daba50e1fe",                                           // A1/03 accelerometer
	"1116e1ffa118550000001001aabbccddeeff",                                             // A1/18 vibration
	"0d16e1ffa1205a01aabbccddeeff",                                                     // A1/20 tamper
	"0d16e1ffa1215a01aabbccddeeff",                                                     // A1/21 leak
	"0b16e1ffa1025a01aabbccddeeff",                                                     // A1/02 light
	"1aff4c000215e2c56db5dffb48d2b060d0f5a71096e000010002c5",                           // iBeacon
	"1516aafe00e700112233445566778899000000000001",                                     // Eddystone UID
	"1116aafe20000bb81a800000001000000064",                                             // Eddystone TLM
	"1516aafe00e7001122334455667788990000000000011116aafe20000bb81a800000001000000064", // UID + TLM
	"03ff1234",                             // unknown manufacturer data
	"0616aafe300102",                       // unknown Eddystone frame type
	"1116e1ffa122640000000000000000000000", // unknown Minew frame version
	"00",                                   // zero-length terminator only
	"",
}

func seedCorpus(t *testing.F) []string {
	out := append([]string(nil), seedAdvertisements...)
	files, _ := filepath.Glob("testdata/real/*.json")
	for _, path := range files {
		blob, e := os.ReadFile(path)
		if e != nil {
			continue
		}
		var f captureFile
		if json.Unmarshal(blob, &f) != nil {
			continue
		}
		for _, row := range f.Rows {
			out = append(out, row.Raw)
		}
	}
	return out
}

// FuzzDecodeFrames asserts the only hard promise the decoder makes about arbitrary input: it never
// panics, it never reports success without a decoded frame, and Unknown stays bounded.
func FuzzDecodeFrames(f *testing.F) {
	for _, raw := range seedCorpus(f) {
		f.Add(raw, "aabbccddeeff")
		f.Add(raw, "")
	}
	f.Fuzz(func(t *testing.T, raw, advertiser string) {
		r, ok := DecodeFramesFor(raw, advertiser)
		if ok && len(r.Frames) == 0 {
			t.Fatalf("ok with no frame: %q -> %+v", raw, r)
		}
		if len(r.Unknown) > maxUnknown {
			t.Fatalf("unknown list unbounded: %v", r.Unknown)
		}
		for _, u := range r.Unknown {
			if len(u) > maxUnknownDesc {
				t.Fatalf("unknown descriptor too long: %q", u)
			}
		}
		// Everything downstream merges, names and serialises the reading; none of that may panic.
		merged := Reading{}
		merged.Merge(r)
		merged.Merge(r)
		_ = DisplayName(merged)
		if _, e := json.Marshal(merged); e != nil {
			t.Fatalf("reading is not serialisable: %v", e)
		}
	})
}

// FuzzProjectPayload fuzzes the JSON-LONG row parser: an MG3 uplink is attacker-shaped input as far
// as this package is concerned (the gateway is authenticated, its payload is not validated).
func FuzzProjectPayload(f *testing.F) {
	f.Add(`[{"type":"Gateway","mac":"111111111111"},{"mac":"AABBCCDDEEFF","rawData":"1016e1ffa101641c8a2f51000000000000","rssi":-62}]`)
	f.Add(`[{"mac":"f00000000005","rawData":"1216e1ffa1034affe7004500fa55daba50e1fe","rssi":-51,"aether_source":"simulated"}]`)
	f.Add(`[{"mac":"","rawData":"","rssi":null}]`)
	f.Add(`[]`)
	f.Add(`{"aether_test":"synthetic"}`)
	f.Add(`[{"mac":"AABBCCDDEEFF","rawData":"03ff1234","rssi":99999}]`)
	f.Fuzz(func(t *testing.T, payload string) {
		if !json.Valid([]byte(payload)) {
			return
		}
		v := Project(domain.Gateway{}, []domain.Packet{{ReceivedAt: time.Unix(0, 0).UTC(), Payload: json.RawMessage(payload)}})
		for _, s := range v.Sensors {
			if s.ID == "" || len(s.ID) != 12 {
				t.Fatalf("sensor id is not a MAC: %q", s.ID)
			}
			if s.Latest.RSSI != nil && (*s.Latest.RSSI < -127 || *s.Latest.RSSI > 20) {
				t.Fatalf("rssi out of range: %d", *s.Latest.RSSI)
			}
		}
		if _, e := json.Marshal(v); e != nil {
			t.Fatalf("view is not serialisable: %v", e)
		}
	})
}
