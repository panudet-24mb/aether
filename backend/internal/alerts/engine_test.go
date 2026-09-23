package alerts

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"testing"
	"time"
)

func reading(metrics map[string]float64, instance string) minew.Reading {
	r := minew.Reading{Metrics: metrics}
	if instance != "" {
		r.Beacon = &minew.Beacon{Type: "eddystone_uid", Instance: instance}
	}
	return r
}

func types(evs []domain.DeviceEvent) []string {
	out := []string{}
	for _, e := range evs {
		out = append(out, e.EventType)
	}
	return out
}

func TestEdgeTriggeredEvents(t *testing.T) {
	now := time.Now()
	evs, st := Detect(State{}, reading(map[string]float64{"tamper": 0}, ""), nil, now)
	if len(evs) != 0 || st.Tamper != 0 || !st.Known {
		t.Fatalf("first idle sight: %v %+v", types(evs), st)
	}
	evs, st = Detect(st, reading(map[string]float64{"tamper": 1}, ""), nil, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventTamper {
		t.Fatalf("rising edge: %v", got)
	}
	evs, st = Detect(st, reading(map[string]float64{"tamper": 1}, ""), nil, now)
	if len(evs) != 0 {
		t.Fatalf("repeat must not re-emit: %v", types(evs))
	}
	evs, st = Detect(st, reading(map[string]float64{"tamper": 0}, ""), nil, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventTamperCleared {
		t.Fatalf("falling edge: %v", got)
	}
	// First sight already tampered counts as an event.
	evs, _ = Detect(State{}, reading(map[string]float64{"tamper": 1}, ""), nil, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventTamper {
		t.Fatalf("first sight tampered: %v", got)
	}
	// Motion via vibration flag, and offline → online.
	evs, st = Detect(State{Known: true, Offline: true}, reading(map[string]float64{"vibration": 1, "accel_g": 1.2}, ""), nil, now)
	if got := types(evs); len(got) != 2 || got[0] != domain.EventOnline || got[1] != domain.EventMotion {
		t.Fatalf("online+motion: %v", got)
	}
	evs, _ = Detect(st, reading(map[string]float64{"vibration": 0}, ""), nil, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventMotionStopped {
		t.Fatalf("motion stopped: %v", got)
	}
	// Button: instance change only after the first known instance.
	evs, st = Detect(State{}, reading(nil, "000000000007"), nil, now)
	if len(evs) != 0 || st.Instance != "000000000007" {
		t.Fatalf("first instance: %v", types(evs))
	}
	evs, st = Detect(st, reading(nil, "00000001f915"), nil, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventButton || evs[0].Detail["from"] != "000000000007" {
		t.Fatalf("button: %v %+v", got, evs)
	}
	evs, _ = Detect(st, reading(nil, "00000001f915"), nil, now)
	if len(evs) != 0 {
		t.Fatal("same instance must not repeat")
	}
}

func TestThresholdRulesEdge(t *testing.T) {
	now := time.Now()
	limit := 30.0
	rule := domain.AlertRule{ID: "r1", Enabled: true, EventType: domain.EventThreshold, Scope: domain.RuleScope{Metric: "temperature", Op: ">", Value: &limit}}
	env := func(temp float64) minew.Reading {
		return minew.Reading{Kind: minew.KindEnvironment, Frames: []string{minew.FrameTH}, Temperature: temp, Humidity: 50}
	}
	evs, st := Detect(State{}, env(25), []domain.AlertRule{rule}, now)
	if len(evs) != 0 || len(st.Breaches) != 0 {
		t.Fatalf("below limit: %v", types(evs))
	}
	evs, st = Detect(st, env(31), []domain.AlertRule{rule}, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventThreshold || evs[0].Detail["rule_id"] != "r1" || len(st.Breaches) != 1 {
		t.Fatalf("breach: %v %+v", got, st)
	}
	evs, st = Detect(st, env(32), []domain.AlertRule{rule}, now)
	if len(evs) != 0 {
		t.Fatal("still breached must not repeat")
	}
	// A beacon-only reading has no temperature: state is kept, nothing emitted.
	evs, st = Detect(st, reading(nil, "abc"), []domain.AlertRule{rule}, now)
	if len(evs) != 0 || len(st.Breaches) != 1 {
		t.Fatalf("unknown metric keeps state: %v %+v", types(evs), st)
	}
	evs, st = Detect(st, env(29), []domain.AlertRule{rule}, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventThresholdClear || len(st.Breaches) != 0 {
		t.Fatalf("clear: %v", got)
	}
	if !Matches(rule, domain.DeviceEvent{EventType: domain.EventThreshold, Detail: map[string]any{"rule_id": "r1"}}) || Matches(rule, domain.DeviceEvent{EventType: domain.EventThreshold, Detail: map[string]any{"rule_id": "r2"}}) {
		t.Fatal("threshold matching must be per rule")
	}
}

func TestMatchesScopeAndValidation(t *testing.T) {
	rule := domain.AlertRule{Name: "Tamper", Enabled: true, EventType: domain.EventTamper, Severity: "critical", Scope: domain.RuleScope{ExternalIDs: []string{"F00000000009"}}}
	if !Matches(rule, domain.DeviceEvent{EventType: domain.EventTamper, ExternalID: "f00000000009"}) || Matches(rule, domain.DeviceEvent{EventType: domain.EventTamper, ExternalID: "f00000000001"}) || Matches(rule, domain.DeviceEvent{EventType: domain.EventLeak, ExternalID: "f00000000009"}) {
		t.Fatal("scope matching")
	}
	rule.Enabled = false
	if Matches(rule, domain.DeviceEvent{EventType: domain.EventTamper, ExternalID: "f00000000009"}) {
		t.Fatal("disabled rule matched")
	}
	if ValidateRule(domain.AlertRule{Name: "x", EventType: "explode", Severity: "info"}) == nil || ValidateRule(domain.AlertRule{Name: "x", EventType: domain.EventThreshold, Severity: "info"}) == nil || ValidateRule(domain.AlertRule{Name: "x", EventType: domain.EventOffline, Severity: "info", Scope: domain.RuleScope{OfflineAfterSec: 5}}) == nil {
		t.Fatal("invalid rules accepted")
	}
	v := 1.0
	if e := ValidateRule(domain.AlertRule{Name: "ok", EventType: domain.EventThreshold, Severity: "warning", Scope: domain.RuleScope{Metric: "humidity", Op: ">=", Value: &v}}); e != nil {
		t.Fatal(e)
	}
	msg := Message(domain.Alert{Severity: "critical", Title: "MBT01 · ป้ายถูกถอด / tamper"}, domain.DeviceEvent{DeviceName: "MBT01", ExternalID: "f00000000009", OccurredAt: time.Unix(0, 0)}, "SIM gateway")
	if msg == "" || msg[:4] != "🔴" {
		t.Fatalf("message: %q", msg)
	}
}
