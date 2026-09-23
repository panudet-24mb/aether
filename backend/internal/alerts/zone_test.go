package alerts

import (
	"math"
	"testing"
	"time"
)

func TestZoneDoesNotFlapOnNoisyMidpoint(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	state := ZoneState{}
	var avgA, avgB *float64
	var atA, atB time.Time
	changes := 0
	// Standing halfway: both gateways around -70 dBm with ±8 dB of opposite-phase noise, every 5 s for 10 min.
	for i := 0; i < 120; i++ {
		at := t0.Add(time.Duration(i) * 5 * time.Second)
		noise := int(math.Round(8 * math.Sin(float64(i)*1.9)))
		a := SmoothRSSI(avgA, atA, -70+noise, at)
		avgA, atA = &a, at
		var changed bool
		state, changed = DecideZone(state, "A", a, currentOf(state, "A", avgA, avgB), at)
		if changed {
			changes++
		}
		b := SmoothRSSI(avgB, atB, -70-noise, at)
		avgB, atB = &b, at
		state, changed = DecideZone(state, "B", b, currentOf(state, "B", avgA, avgB), at)
		if changed {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("only the first assignment may happen while standing at the midpoint, got %d changes", changes)
	}
}

func currentOf(s ZoneState, hearing string, a, b *float64) *float64 {
	if s.Gateway == "A" {
		return a
	}
	if s.Gateway == "B" {
		return b
	}
	return nil
}

func TestZoneFollowsARealMove(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	state, _ := DecideZone(ZoneState{}, "A", -55, nil, t0)
	cur := -80.0 // the person walked away from A
	var changed bool
	state, changed = DecideZone(state, "B", -60, &cur, t0.Add(5*time.Second))
	if changed || state.Candidate != "B" {
		t.Fatalf("a stronger gateway must first become a candidate: %+v", state)
	}
	state, changed = DecideZone(state, "B", -58, &cur, t0.Add(10*time.Second))
	if changed {
		t.Fatal("dwell time not reached yet")
	}
	state, changed = DecideZone(state, "B", -57, &cur, t0.Add(15*time.Second))
	if !changed || state.Gateway != "B" {
		t.Fatalf("must switch after the dwell time: %+v", state)
	}
	// A weaker blip drops the candidate.
	weak := -57.0
	state, _ = DecideZone(state, "A", -50, &weak, t0.Add(20*time.Second))
	state, _ = DecideZone(state, "A", -56, &weak, t0.Add(25*time.Second))
	if state.Candidate != "" || state.Gateway != "B" {
		t.Fatalf("candidate must be dropped when it stops being clearly stronger: %+v", state)
	}
	// The current zone goes silent: switch at once.
	state, changed = DecideZone(state, "A", -85, nil, t0.Add(120*time.Second))
	if !changed || state.Gateway != "A" {
		t.Fatalf("must switch immediately when the current zone lost the tag: %+v", state)
	}
}

func TestSmoothRSSIRestartsAfterGap(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	prev := -50.0
	if got := SmoothRSSI(&prev, t0, -90, t0.Add(5*time.Minute)); got != -90 {
		t.Fatalf("stale average must be discarded, got %v", got)
	}
	if got := SmoothRSSI(&prev, t0, -90, t0.Add(5*time.Second)); got >= -50 || got <= -90 {
		t.Fatalf("fresh average must move part of the way, got %v", got)
	}
}

// Both gateways keep reporting every 5 s. The current gateway's uplinks must not reset the candidate's dwell time.
func TestZoneHandsOverWhileBothGatewaysHearTheTag(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	state, _ := DecideZone(ZoneState{}, "A", -60, nil, t0)
	a, b := -78.0, -58.0
	switched := -1
	for i := 1; i <= 6 && switched < 0; i++ {
		at := t0.Add(time.Duration(i) * 5 * time.Second)
		state, _ = DecideZone(state, "A", a, &a, at)
		var changed bool
		if state, changed = DecideZone(state, "B", b, &a, at.Add(time.Second)); changed {
			switched = i
		}
	}
	if switched != 3 || state.Gateway != "B" {
		t.Fatalf("handover must happen after the 10 s dwell with interleaved uplinks, got step %d state %+v", switched, state)
	}
}

// A candidate that went quiet and comes back much later starts its dwell time again.
func TestStaleCandidateStartsOver(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	state, _ := DecideZone(ZoneState{}, "A", -60, nil, t0)
	cur := -78.0
	state, _ = DecideZone(state, "B", -58, &cur, t0.Add(5*time.Second))
	state, changed := DecideZone(state, "B", -58, &cur, t0.Add(65*time.Second))
	if changed || state.Gateway != "A" || !state.CandidateSince.Equal(t0.Add(65*time.Second)) {
		t.Fatalf("one late sample must not switch the zone: changed=%v %+v", changed, state)
	}
}
