package simulation

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"encoding/json"
	"testing"
	"time"
)

func TestVirtualFramesUseRealDecoder(t *testing.T) {
	var rows []struct {
		MAC    string `json:"mac"`
		Raw    string `json:"rawData"`
		Source string `json:"aether_source"`
		Type   string `json:"type"`
	}
	if e := json.Unmarshal(Packet(0, time.Unix(0, 0)), &rows); e != nil {
		t.Fatal(e)
	}
	if rows[0].Type != "Gateway" || rows[0].MAC != gatewayMAC {
		t.Fatal("first row must identify the virtual gateway")
	}
	macs := map[string]bool{}
	for _, row := range rows[1:] {
		r, ok := minew.DecodeFrames(row.Raw)
		if !ok || row.Source != "simulated" {
			t.Fatalf("invalid simulated frame %s: %s", row.MAC, row.Raw)
		}
		macs[row.MAC] = true
		if r.Kind == minew.KindEnvironment && (r.Temperature < 20 || r.Temperature > 30 || r.Humidity < 40 || r.Humidity > 65) {
			t.Fatalf("environment out of range: %+v", r)
		}
	}
	if len(macs) != 10 {
		t.Fatalf("expected 10 virtual devices, got %d", len(macs))
	}
	if string(Packet(0, time.Unix(0, 0))) == string(Packet(1, time.Unix(0, 0))) {
		t.Fatal("scenario must change over time")
	}
}

func TestKitScenarioProjectsPerDevice(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	find := func(v minew.View, mac string) minew.Sensor {
		for _, s := range v.Sensors {
			if s.ID == mac {
				return s
			}
		}
		t.Fatalf("missing %s", mac)
		return minew.Sensor{}
	}
	// Step 0: S1 info frame, B10 pressed, E8S moving, MBT01 not tampered.
	v := minew.Project(domain.Gateway{}, []domain.Packet{{ReceivedAt: now, Payload: Packet(0, now)}})
	if len(v.Sensors) != 10 {
		t.Fatalf("sensors: %d", len(v.Sensors))
	}
	s1 := find(v, "f00000000001")
	if s1.Kind != minew.KindEnvironment || s1.Model != "S1" || s1.Name != "SIM · ข้อมูลจำลอง · Minew S1" || s1.Latest.Battery != 96 {
		t.Fatalf("s1: %+v", s1)
	}
	b10 := find(v, macB10)
	if b10.Kind != minew.KindBeacon || b10.Latest.Beacon == nil || b10.Latest.Beacon.Instance != b10Pressed || b10.Latest.Beacon.Voltage != 3.9 {
		t.Fatalf("b10: %+v", b10.Latest)
	}
	e8s := find(v, macE8S)
	if e8s.Kind != minew.KindMotion || e8s.Latest.Metrics["vibration"] != 1 || e8s.Latest.Metrics["accel_g"] < 0.7 {
		t.Fatalf("e8s: %+v", e8s.Latest)
	}
	mbt := find(v, macMBT01)
	if mbt.Kind != minew.KindBeacon && mbt.Kind != minew.KindTamper || mbt.Latest.Metrics["tamper"] != 0 || mbt.Latest.Beacon == nil || mbt.Latest.Beacon.Minor != 9 {
		t.Fatalf("mbt01 idle: %+v", mbt.Latest)
	}
	c10 := find(v, macC10)
	if c10.Latest.Beacon == nil || c10.Latest.Beacon.Type != "ibeacon" || c10.Latest.Metrics["accel_z"] < 0.9 {
		t.Fatalf("c10: %+v", c10.Latest)
	}
	// Step 27: tamper raised, B10 idle, E8S still, names present from earlier info frames are not required.
	v = minew.Project(domain.Gateway{}, []domain.Packet{{ReceivedAt: now, Payload: Packet(27, now)}})
	if find(v, macMBT01).Latest.Metrics["tamper"] != 1 || find(v, macMBT01).Kind != minew.KindTamper {
		t.Fatalf("mbt01 tampered: %+v", find(v, macMBT01).Latest)
	}
	if find(v, macB10).Latest.Beacon.Instance != b10Idle || find(v, macE8S).Latest.Metrics["vibration"] != 0 {
		t.Fatalf("step 27 scenario wrong")
	}
}

// The realistic B10 reproduces the situation the owner's physical tag is actually in: at rest it says
// only what Aether can read, and its press is a frame version no decoder knows. This asserts that the
// press frame is NOT decodable — if a future decoder learned to read it, this scenario would stop
// exercising the "undocumented press" path that "สอนสัญญาณ" exists for, and the test must be revisited.
func TestRealisticB10PressIsUndecodable(t *testing.T) {
	quiet, pressed := -1, -1
	for step := 0; step < 40 && (quiet < 0 || pressed < 0); step++ {
		if B10RealPressed(step) && pressed < 0 {
			pressed = step
		}
		if !B10RealPressed(step) && quiet < 0 {
			quiet = step
		}
	}
	if quiet < 0 || pressed < 0 {
		t.Fatal("the scenario must contain both a quiet and a pressed step")
	}
	raws := func(step int) []string {
		var rows []struct{ MAC, RawData string }
		if e := json.Unmarshal(Packet(step, time.Unix(0, 0)), &rows); e != nil {
			t.Fatal(e)
		}
		var out []string
		for _, r := range rows {
			if r.MAC == macB10Real {
				out = append(out, r.RawData)
			}
		}
		return out
	}
	rest, press := raws(quiet), raws(pressed)
	if len(rest) != 3 || len(press) != 4 {
		t.Fatalf("a press must add exactly one advertisement: rest=%d pressed=%d", len(rest), len(press))
	}
	// Everything it says at rest decodes; the extra one while pressed does not.
	for _, raw := range rest {
		if _, ok := minew.DecodeFrames(raw); !ok {
			t.Fatalf("resting advertisement must decode: %s", raw)
		}
	}
	extra := press[len(press)-1]
	r, ok := minew.DecodeFrames(extra)
	if ok {
		t.Fatalf("the press frame must stay undecodable, got %+v", r)
	}
	if len(r.Unknown) == 0 {
		t.Fatalf("an undecodable frame must still be reported as unknown so an operator can see it: %s", extra)
	}
	// It must also never claim to be an Eddystone-UID instance change: that is the inference the real
	// hardware cannot produce, and the whole reason the button never fired.
	for _, raw := range append(rest, press...) {
		if d, _ := minew.DecodeFrames(raw); d.Beacon != nil && d.Beacon.Instance != "" {
			t.Fatalf("the realistic B10 must never advertise an Eddystone-UID instance: %s", raw)
		}
	}
}

func TestWearablesWalkBetweenZones(t *testing.T) {
	both, onlyA, onlyB := 0, 0, 0
	for step := 0; step < walkSteps; step++ {
		a, aOK, b, bOK := WalkRSSI(step, 0)
		if !aOK && !bOK {
			t.Fatalf("step %d: wearable heard by nobody", step)
		}
		switch {
		case aOK && bOK:
			both++
		case aOK:
			onlyA++
		default:
			onlyB++
		}
		if aOK && bOK && step < walkSteps/4 && a < b {
			t.Fatalf("step %d: zone A must be stronger near the start (a=%d b=%d)", step, a, b)
		}
	}
	if both == 0 || onlyA == 0 || onlyB == 0 {
		t.Fatalf("walk must cover overlap and both exclusive ranges: both=%d onlyA=%d onlyB=%d", both, onlyA, onlyB)
	}
	var rows []map[string]any
	if e := json.Unmarshal(ZonePacket(walkSteps/2, time.Unix(1700000000, 0)), &rows); e != nil {
		t.Fatal(e)
	}
	heardC10 := false
	for _, r := range rows {
		if r["mac"] == macC10 {
			heardC10 = true
		}
		if r["mac"] == macE8S || r["mac"] == macMBT01 {
			t.Fatalf("zone B must hear wearables only, got %v", r["mac"])
		}
	}
	if !heardC10 {
		t.Fatal("zone B must hear the C10 when it stands next to it")
	}
}
