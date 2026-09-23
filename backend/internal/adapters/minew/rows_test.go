package minew

import (
	"aether/backend/internal/domain"
	"encoding/json"
	"testing"
	"time"
)

// JSON-Long as MG3 and MG4 upload it. MG4 stamps rows with ISO-8601 strings and adds fields of its own;
// one odd row (a string rssi, a numeric mac, a row that is not an object) must cost only itself.
func TestParseRowsMG3MG4AndMalformed(t *testing.T) {
	const th = "1016e1ffa101641c8a2f51000000000000"
	for _, tc := range []struct {
		name    string
		payload string
		ok      bool
		rows    int
		sensors map[string]*int // mac -> expected rssi (nil = no rssi)
	}{
		{
			name:    "MG3 epoch-millisecond timestamps",
			payload: `[{"type":"Gateway","mac":"AC233FC00001","nums":2,"timestamp":1758600000000},{"mac":"AABBCCDDEEFF","rawData":"` + th + `","rssi":-62,"timestamp":1758600000000}]`,
			ok:      true, rows: 2, sensors: map[string]*int{"aabbccddeeff": ptr(-62)},
		},
		{
			name: "MG4 ISO-8601 timestamps and extra fields",
			payload: `[{"type":"Gateway","mac":"AC233FC00002","timestamp":"2020-03-20T08:01:11Z"},
			 {"mac":"AABBCCDDEEFF","rawData":"` + th + `","rssi":-58,"timestamp":"2020-03-20T08:01:11Z","bleName":"S1","battery":100,"temperature":28.5,"humidity":47.3},
			 {"mac":"AA:BB:CC:DD:EE:01","rawData":"` + th + `","rssi":"-71","timestamp":"2020-03-20T08:01:11.123+07:00","bleName":null}]`,
			ok: true, rows: 3, sensors: map[string]*int{"aabbccddeeff": ptr(-58), "aabbccddee01": ptr(-71)},
		},
		{
			name: "one malformed row among good ones",
			payload: `[{"type":"Gateway","mac":"AC233FC00003"},
			 {"mac":112233445566,"rawData":"` + th + `","rssi":-50},
			 "not an object",
			 {"mac":"AABBCCDDEE02","rawData":"` + th + `","rssi":"strong"},
			 {"mac":"AABBCCDDEE03","rawData":12345,"rssi":-40},
			 {"mac":"AABBCCDDEE04","rawData":"` + th + `","rssi":-61.5},
			 {"mac":"AABBCCDDEE05","rawData":"` + th + `","rssi":-66,"timestamp":{"odd":true}}]`,
			ok: true, rows: 6, sensors: map[string]*int{"aabbccddee02": nil, "aabbccddee04": nil, "aabbccddee05": ptr(-66)},
		},
		{name: "not an array", payload: `{"aether_test":"synthetic"}`, ok: false},
		{name: "empty array", payload: `[]`, ok: true, rows: 0, sensors: map[string]*int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, ok := ParseRows([]byte(tc.payload))
			if ok != tc.ok || len(rows) != tc.rows {
				t.Fatalf("ParseRows: ok=%v rows=%d, want ok=%v rows=%d", ok, len(rows), tc.ok, tc.rows)
			}
			if !tc.ok {
				return
			}
			if tc.rows > 0 && rows[0].Type != "Gateway" {
				t.Fatalf("header row: %+v", rows[0])
			}
			v := Project(domain.Gateway{}, []domain.Packet{{ReceivedAt: time.Unix(1_758_600_000, 0).UTC(), Payload: json.RawMessage(tc.payload)}})
			if len(v.Sensors) != len(tc.sensors) {
				t.Fatalf("sensors: %+v", v.Sensors)
			}
			for _, s := range v.Sensors {
				want, found := tc.sensors[s.ID]
				if !found {
					t.Fatalf("unexpected sensor %s", s.ID)
				}
				if s.Kind != KindEnvironment || s.Latest.Temperature != 28.5390625 {
					t.Fatalf("%s not decoded: %+v", s.ID, s.Latest)
				}
				// Device clocks are never trusted: the reading carries the server receive time.
				if !s.Latest.ReceivedAt.Equal(time.Unix(1_758_600_000, 0).UTC()) {
					t.Fatalf("%s received_at: %v", s.ID, s.Latest.ReceivedAt)
				}
				switch {
				case want == nil && s.Latest.RSSI != nil:
					t.Fatalf("%s rssi should be unknown, got %d", s.ID, *s.Latest.RSSI)
				case want != nil && (s.Latest.RSSI == nil || *s.Latest.RSSI != *want):
					t.Fatalf("%s rssi: %v want %d", s.ID, s.Latest.RSSI, *want)
				}
			}
		})
	}
}

func ptr(v int) *int { return &v }

// A Minew tag whose only frame is one Aether cannot decode (the S4 door sensor's combination frame)
// still becomes a stream with its frame listed in Unknown, so it can be seen and taught. A foreign
// advertisement that is not a Minew frame stays out of discovery.
func TestUndecodedMinewFrameSurfacesInUnknown(t *testing.T) {
	payload := json.RawMessage(`[{"type":"Gateway","mac":"AC233FC00002"},
	 {"mac":"F000000000C5","rawData":"1216e1ffa1235f010000010000c500000000f0","rssi":-63},
	 {"mac":"AABBCCDDEE09","rawData":"0bff4c001006311e5a0b1d38","rssi":-80}]`)
	v := Project(domain.Gateway{}, []domain.Packet{{ReceivedAt: time.Now().UTC(), Payload: payload}})
	if len(v.Sensors) != 1 || v.NearbyDevices != 2 {
		t.Fatalf("sensors: %+v", v.Sensors)
	}
	s := v.Sensors[0]
	if s.ID != "f000000000c5" || s.Kind != KindInfo || len(s.Latest.Frames) != 0 || len(s.Latest.Unknown) != 1 || s.Latest.Unknown[0] != "ffe1:a1:0x23:len=15" {
		t.Fatalf("undecoded minew frame: %+v", s.Latest)
	}
	if _, has := s.Latest.Metrics["door"]; has {
		t.Fatal("no decoder may invent a door state")
	}
}

// A tracked identity (registered, or already a stream on this gateway) keeps its reading on an uplink
// whose only frame uses a service Aether has no rule for; an untracked one stays out of discovery.
func TestProjectKnownKeepsTrackedUndecodedIdentity(t *testing.T) {
	payload := json.RawMessage(`[{"type":"Gateway","mac":"AC233FC00002"},{"mac":"F000000000C5","rawData":"051634120100","rssi":-63}]`)
	packets := []domain.Packet{{ReceivedAt: time.Now().UTC(), Payload: payload}}
	if v := Project(domain.Gateway{}, packets); len(v.Sensors) != 0 {
		t.Fatalf("an untracked non-Minew advertisement is not a device: %+v", v.Sensors)
	}
	v := ProjectKnown(domain.Gateway{}, packets, func(mac string) bool { return mac == "f000000000c5" })
	if len(v.Sensors) != 1 || v.Sensors[0].Kind != KindInfo || len(v.Sensors[0].Latest.Unknown) != 1 || v.Sensors[0].Latest.Unknown[0] != "ad:0x16:len=5" {
		t.Fatalf("tracked identity: %+v", v.Sensors)
	}
}
