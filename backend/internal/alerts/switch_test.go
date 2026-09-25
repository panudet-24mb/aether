package alerts

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"testing"
	"time"
)

func switchReading(gangs ...float64) minew.Reading {
	m := map[string]float64{"linkquality": 120}
	for i, v := range gangs {
		m["sw"+string(rune('1'+i))] = v
	}
	return minew.Reading{Kind: "switch", Frames: []string{"z2m-state@1"}, Metrics: m}
}

// A wall switch: the first report is a baseline, each later change of a gang is one event, a repeated state is
// nothing, and nothing a switch does is ever a button (SOS) press.
func TestSwitchOutputs(t *testing.T) {
	now := time.Now()
	evs, st := Detect(State{}, switchReading(1, 0), nil, now)
	if len(evs) != 0 || st.Outputs["sw1"] != 1 || st.Outputs["sw2"] != 0 {
		t.Fatalf("baseline: %v %+v", types(evs), st.Outputs)
	}
	evs, st = Detect(st, switchReading(1, 0), nil, now.Add(time.Second))
	if len(evs) != 0 {
		t.Fatalf("repeat: %v", types(evs))
	}
	evs, st = Detect(st, switchReading(0, 1), nil, now.Add(2*time.Second))
	if len(evs) != 2 || evs[0].EventType != domain.EventSwitchOff || evs[0].Detail["gang"] != 1 || evs[1].EventType != domain.EventSwitchOn || evs[1].Detail["gang"] != 2 || evs[1].Detail["source"] != "external" {
		t.Fatalf("edges: %v %v", types(evs), evs)
	}
	// A report that only carries gang 2 keeps gang 1's last known state.
	evs, st = Detect(st, minew.Reading{Kind: "switch", Metrics: map[string]float64{"sw2": 0}}, nil, now.Add(3*time.Second))
	if len(evs) != 1 || evs[0].EventType != domain.EventSwitchOff || st.Outputs["sw1"] != 0 {
		t.Fatalf("partial: %v %+v", types(evs), st.Outputs)
	}
	for _, e := range evs {
		if e.EventType == domain.EventButton {
			t.Fatal("a switch raised button")
		}
	}
	if got := Title(domain.DeviceEvent{DeviceName: "โถง", EventType: domain.EventSwitchOn, Detail: map[string]any{"gang": 2}}); got != "โถง · เปิดสวิตช์ ช่อง 2" {
		t.Fatalf("title %q", got)
	}
}

// A change the device reports in answer to an Aether command names that command; the other gang in the same
// report changed on the wall.
func TestSwitchEventNamesItsCommand(t *testing.T) {
	now := time.Now()
	_, st := Detect(State{}, switchReading(0, 0), nil, now)
	r := switchReading(1, 1)
	r.Commands = map[int]string{1: "11111111-1111-4111-8111-111111111111"}
	evs, _ := Detect(st, r, nil, now.Add(time.Second))
	if len(evs) != 2 || evs[0].Detail["source"] != "command" || evs[0].Detail["command_id"] != "11111111-1111-4111-8111-111111111111" || evs[1].Detail["source"] != "external" || evs[1].Detail["command_id"] != nil {
		t.Fatalf("attribution: %v", evs)
	}
}
