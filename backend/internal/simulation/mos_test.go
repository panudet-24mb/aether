package simulation

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"encoding/json"
	"testing"
	"time"
)

func mosView(t *testing.T, step int) minew.View {
	t.Helper()
	now := time.Unix(1_758_600_000, 0).UTC()
	return minew.Project(domain.Gateway{}, []domain.Packet{{ReceivedAt: now, Payload: MOSPacket(step, now)}})
}

func mosSensor(t *testing.T, v minew.View, mac string) minew.Sensor {
	t.Helper()
	for _, s := range v.Sensors {
		if s.ID == mac {
			return s
		}
	}
	t.Fatalf("missing %s in %+v", mac, v.Sensors)
	return minew.Sensor{}
}

func TestMOSPacketIsMG4StyleAndDecodes(t *testing.T) {
	var rows []map[string]any
	if e := json.Unmarshal(MOSPacket(0, time.Unix(1_758_600_000, 0)), &rows); e != nil {
		t.Fatal(e)
	}
	if rows[0]["type"] != "Gateway" || rows[0]["mac"] != MOSGateway {
		t.Fatalf("first row must identify the virtual MG4: %v", rows[0])
	}
	extras := 0
	for _, row := range rows {
		ts, _ := row["timestamp"].(string)
		if _, e := time.Parse(time.RFC3339, ts); e != nil {
			t.Fatalf("MG4 rows carry ISO-8601 timestamps: %v", row)
		}
		if row["aether_source"] != "simulated" {
			t.Fatalf("every row is tagged simulated: %v", row)
		}
		if _, ok := row["bleName"]; ok {
			extras++
		}
	}
	if extras == 0 {
		t.Fatal("the scenario must carry MG4's extra fields so the parser is exercised")
	}

	v := mosView(t, 0)
	if len(v.Sensors) != 5 {
		t.Fatalf("expected S1, MSP01, C10, MBT01 and S4: %+v", v.Sensors)
	}
	if s1 := mosSensor(t, v, MOSS1); s1.Kind != minew.KindEnvironment || s1.Model != "S1" || s1.Latest.Temperature < 20 {
		t.Fatalf("s1: %+v", s1)
	}
	if c10 := mosSensor(t, v, MOSC10); c10.Latest.Beacon == nil || c10.Latest.Beacon.Minor != 3 {
		t.Fatalf("c10: %+v", c10.Latest)
	}
	if mbt := mosSensor(t, v, MOSMBT01); mbt.Latest.Metrics["tamper"] != 0 || mbt.Latest.Beacon == nil {
		t.Fatalf("mbt01: %+v", mbt.Latest)
	}
	// Packet's output is untouched by the MOS scenario.
	if string(Packet(3, time.Unix(0, 0))) == string(MOSPacket(3, time.Unix(0, 0))) {
		t.Fatal("MOSPacket must be a separate scenario")
	}
}

func TestMOSMSP01MotionToggles(t *testing.T) {
	seen := map[float64]bool{}
	for step := 0; step < 40; step++ {
		s := mosSensor(t, mosView(t, step), MOSMSP01)
		motion, ok := s.Latest.Metrics["motion"]
		if !ok || !containsFrame(s.Latest.Frames, minew.FramePIR) || !containsFrame(s.Latest.Frames, minew.FrameTH) {
			t.Fatalf("step %d: PIR and temperature frames must decode: %+v", step, s.Latest)
		}
		if (motion == 1) != MSP01Motion(step) {
			t.Fatalf("step %d: motion %v, scenario %v", step, motion, MSP01Motion(step))
		}
		seen[motion] = true
	}
	if !seen[0] || !seen[1] {
		t.Fatalf("motion must toggle: %v", seen)
	}
	if s := mosSensor(t, mosView(t, 2), MOSMSP01); s.Model != "MSP01" {
		t.Fatalf("the A1-08 info frame names the MSP01 so the profile can be suggested: %+v", s)
	}
}

func TestMOSSyntheticDoorFrameIsUnknown(t *testing.T) {
	sawOpen, sawClosed := false, false
	for step := 0; step < 60; step++ {
		s := mosSensor(t, mosView(t, step), MOSS4)
		if len(s.Latest.Unknown) == 0 || s.Latest.Unknown[0] != "ffe1:a1:0x23:len=15" {
			t.Fatalf("step %d: the synthetic door frame must surface in Unknown: %+v", step, s.Latest)
		}
		if _, has := s.Latest.Metrics["door"]; has {
			t.Fatalf("step %d: nothing decodes the S4, so no door metric may appear: %+v", step, s.Latest)
		}
		if S4Open(step) {
			sawOpen = true
		} else {
			sawClosed = true
		}
	}
	if !sawOpen || !sawClosed {
		t.Fatal("the door must open and close within the scenario")
	}
	if s := mosSensor(t, mosView(t, 5), MOSS4); s.Model != "S4" {
		t.Fatalf("the info frame names the S4: %+v", s)
	}
}

func containsFrame(frames []string, id string) bool {
	for _, f := range frames {
		if f == id {
			return true
		}
	}
	return false
}
