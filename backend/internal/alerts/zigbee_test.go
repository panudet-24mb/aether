package alerts

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"testing"
	"time"
)

func z2m(metrics map[string]float64, action string) minew.Reading {
	return minew.Reading{Frames: []string{zigbee2mqtt.FrameState}, Metrics: metrics, Action: action}
}

func TestHazardEdges(t *testing.T) {
	now := time.Now()
	// Already in alarm on the first report: that is an event, not a baseline.
	evs, st := Detect(State{}, z2m(map[string]float64{"smoke": 1}, ""), nil, now)
	if len(evs) != 1 || evs[0].EventType != domain.EventHazard || evs[0].Detail["hazard"] != "smoke" {
		t.Fatalf("first alarm: %v", types(evs))
	}
	if evs, st = Detect(st, z2m(map[string]float64{"smoke": 1}, ""), nil, now); len(evs) != 0 {
		t.Fatalf("repeated alarm: %v", types(evs))
	}
	if evs, st = Detect(st, z2m(map[string]float64{"smoke": 0, "gas": 1}, ""), nil, now); len(evs) != 2 || evs[0].EventType != domain.EventHazardCleared || evs[1].Detail["hazard"] != "gas" {
		t.Fatalf("clear smoke, trip gas: %+v", evs)
	}
	// A detector that starts quiet raises nothing.
	if evs, _ := Detect(State{}, z2m(map[string]float64{"carbon_monoxide": 0}, ""), nil, now); len(evs) != 0 {
		t.Fatalf("quiet start: %v", types(evs))
	}
}

func TestActionsAndSOSButton(t *testing.T) {
	now := time.Now()
	evs, _ := Detect(State{}, z2m(map[string]float64{}, "emergency"), nil, now)
	if len(evs) != 2 || evs[0].EventType != domain.EventAction || evs[1].EventType != domain.EventButton || evs[1].Detail["trigger"] != "z2m_action" {
		t.Fatalf("emergency: %+v", evs)
	}
	evs, _ = Detect(State{}, z2m(map[string]float64{}, "on"), nil, now)
	if len(evs) != 1 || evs[0].EventType != domain.EventAction || evs[0].Detail["action"] != "on" {
		t.Fatalf("remote press: %+v", evs)
	}
	// SOS binary: one button per press (rising edge), none while held or released.
	st := State{}
	var got []string
	for _, v := range []float64{0, 1, 1, 0, 1} {
		evs, st = Detect(st, z2m(map[string]float64{"sos": v}, ""), nil, now)
		got = append(got, types(evs)...)
	}
	if len(got) != 2 || got[0] != domain.EventButton || got[1] != domain.EventButton {
		t.Fatalf("sos binary: %v", got)
	}
	// A button that keeps reporting true: a report ButtonTriggerQuiet after the last true is a new press, a
	// report within it is the same one.
	st = State{}
	got = nil
	for _, dt := range []time.Duration{0, 5 * time.Second, 10 * time.Second, 50 * time.Second} {
		evs, st = Detect(st, z2m(map[string]float64{"sos": 1}, ""), nil, now.Add(dt))
		got = append(got, types(evs)...)
	}
	if len(got) != 2 {
		t.Fatalf("cached sos:true re-arm: %v", got)
	}
}

// Threshold rules read Zigbee2MQTT measurements from Metrics; a message without the metric is "unknown".
func TestThresholdOnZigbeeReading(t *testing.T) {
	v := 30.0
	rule := domain.AlertRule{ID: "r1", Enabled: true, EventType: domain.EventThreshold, Scope: domain.RuleScope{Metric: "temperature", Op: ">", Value: &v}}
	evs, st := Detect(State{}, z2m(map[string]float64{"temperature": 31.2}, ""), []domain.AlertRule{rule}, time.Now())
	if len(evs) != 1 || evs[0].EventType != domain.EventThreshold {
		t.Fatalf("breach: %v", types(evs))
	}
	if evs, _ := Detect(st, z2m(map[string]float64{"humidity": 40}, ""), []domain.AlertRule{rule}, time.Now()); len(evs) != 0 {
		t.Fatalf("a message without temperature must not clear the breach: %v", types(evs))
	}
	// Temperature 0 in the fixed Minew field of a Zigbee reading is not a reading.
	if _, ok := metricValue(z2m(map[string]float64{"humidity": 40}, ""), "temperature"); ok {
		t.Fatal("absent temperature read as present")
	}
}
