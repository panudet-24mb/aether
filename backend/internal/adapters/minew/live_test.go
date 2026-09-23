package minew

import (
	"aether/backend/internal/domain"
	"encoding/json"
	"testing"
	"time"
)

func TestLiveFrame(t *testing.T) {
	// Live A1/01 sample; trailing device identity redacted to zeros.
	raw := "0201061016e1ffa101641c8a2f51000000000000"
	r, ok := Decode(raw)
	if !ok || r.Temperature != 28.5390625 || r.Humidity != 47.31640625 || r.Battery != 100 {
		t.Fatalf("unexpected reading: %+v %v", r, ok)
	}
	for _, bad := range []string{"zz", raw[:len(raw)-2], "1016e1ffa103641c8a2f51000000000000", "1016e1ffa101641c8affff000000000000"} {
		if _, ok := Decode(bad); ok {
			t.Fatalf("invalid frame accepted: %s", bad)
		}
	}
	// Changed deliberately (2026-09-20): an implausible battery byte no longer throws away a good
	// temperature/humidity pair. The battery field is dropped (0 = unknown to the alert engine) and
	// the drop is reported in Unknown; the rest of the frame still decodes.
	if r, ok := Decode("1016e1ffa101ff1c8a2f51000000000000"); !ok || r.Temperature != 28.5390625 || r.Battery != 0 || len(r.Unknown) != 1 || r.Unknown[0] != "ffe1:a1:0x01:battery=oor" {
		t.Fatalf("battery out of range must drop only the battery field: %+v %v", r, ok)
	}
	r, ok = Decode("1016e1ffa10164ff802f51000000000000")
	if !ok || r.Temperature != -0.5 {
		t.Fatal("signed 8.8 failed")
	}
}
func TestProjection(t *testing.T) {
	now := time.Now().UTC()
	body := json.RawMessage(`[{"type":"Gateway","mac":"111111111111"},{"mac":"AABBCCDDEEFF","rawData":"1016e1ffa101641c8a2f51000000000000","rssi":-62},{"mac":"AABBCCDDEEFF","rawData":"1016e1ffa101641c8a2f51000000000000","rssi":-62}]`)
	v := Project(domain.Gateway{}, []domain.Packet{{ReceivedAt: now, Payload: body}, {ReceivedAt: now.Add(-time.Second), Payload: body}, {ReceivedAt: now.Add(time.Hour), Payload: json.RawMessage(`{"aether_test":"synthetic"}`)}})
	if v.PacketCount != 2 || v.NearbyDevices != 1 || len(v.Sensors) != 1 || len(v.Sensors[0].History) != 2 || !v.Sensors[0].Latest.ReceivedAt.Equal(now) || !v.LastPacketAt.Equal(now) {
		t.Fatalf("bad projection: %+v", v)
	}
}
