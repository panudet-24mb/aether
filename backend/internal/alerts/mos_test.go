package alerts

import (
	"aether/backend/internal/domain"
	"testing"
	"time"
)

func TestDoorEdges(t *testing.T) {
	now := time.Now()
	evs, st := Detect(State{}, reading(map[string]float64{"door": 0}, ""), nil, now)
	if len(evs) != 0 || st.Door != 0 {
		t.Fatalf("closed at first sight is not an event: %v", types(evs))
	}
	evs, st = Detect(st, reading(map[string]float64{"door": 1}, ""), nil, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventDoorOpen || st.Door != 1 {
		t.Fatalf("opening: %v %+v", got, st)
	}
	evs, st = Detect(st, reading(map[string]float64{"door": 1}, ""), nil, now)
	if len(evs) != 0 {
		t.Fatalf("held open must not repeat: %v", types(evs))
	}
	// An uplink that says nothing about the door keeps the state.
	evs, st = Detect(st, reading(map[string]float64{}, ""), nil, now)
	if len(evs) != 0 || st.Door != 1 {
		t.Fatalf("no door metric: %v %+v", types(evs), st)
	}
	evs, st = Detect(st, reading(map[string]float64{"door": 0}, ""), nil, now)
	if got := types(evs); len(got) != 1 || got[0] != domain.EventDoorClosed || st.Door != 0 {
		t.Fatalf("closing: %v", got)
	}
}

func TestOccupancyEpisode(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	evs, st := Detect(State{}, reading(map[string]float64{"motion": 0}, ""), nil, t0)
	if countOf(evs, domain.EventOccupied) != 0 || st.Occupied != 0 {
		t.Fatalf("no motion, no occupancy: %v", types(evs))
	}
	evs, st = Detect(st, reading(map[string]float64{"motion": 1}, ""), nil, t0.Add(time.Minute))
	if countOf(evs, domain.EventOccupied) != 1 || st.Occupied != 1 || !st.LastMotion.Equal(t0.Add(time.Minute)) {
		t.Fatalf("motion starts an episode: %v %+v", types(evs), st)
	}
	// Motion stops and starts again within the hold time: still the same episode.
	evs, st = Detect(st, reading(map[string]float64{"motion": 0}, ""), nil, t0.Add(2*time.Minute))
	evs2, st := Detect(st, reading(map[string]float64{"motion": 1}, ""), nil, t0.Add(3*time.Minute))
	if countOf(append(evs, evs2...), domain.EventOccupied) != 0 || !st.LastMotion.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("one episode, one occupied event: %v %v", types(evs), types(evs2))
	}
	// A quiet uplink never raises vacant; only the periodic scan decides that.
	evs, st = Detect(st, reading(map[string]float64{"motion": 0}, ""), nil, t0.Add(30*time.Minute))
	if countOf(evs, domain.EventVacant) != 0 || st.Occupied != 1 {
		t.Fatalf("vacant is never per uplink: %v", types(evs))
	}
	if Vacant(st, t0.Add(3*time.Minute+(OccupancyHoldSec-1)*time.Second)) {
		t.Fatal("inside the hold time the room is still occupied")
	}
	if !Vacant(st, t0.Add(3*time.Minute+OccupancyHoldSec*time.Second)) {
		t.Fatal("after the hold time the room is vacant")
	}
	st.Occupied = 0
	if Vacant(st, t0.Add(time.Hour)) {
		t.Fatal("an ended episode is not vacated twice")
	}
}

func countOf(evs []domain.DeviceEvent, t string) int {
	n := 0
	for _, e := range evs {
		if e.EventType == t {
			n++
		}
	}
	return n
}

func TestAfterHoursWindow(t *testing.T) {
	bkk := time.FixedZone("ICT", 7*3600)
	at := func(day, hour, minute int) time.Time { // 2026-09-20 is a Sunday
		return time.Date(2026, 9, 20+day, hour, minute, 0, 0, bkk)
	}
	night := domain.AfterHours{From: "18:00", To: "07:00", Days: []int{1, 2, 3, 4, 5}} // weeknights
	for _, tc := range []struct {
		name string
		w    domain.AfterHours
		at   time.Time
		want bool
	}{
		{"monday evening", night, at(1, 22, 0), true},
		{"monday office hours", night, at(1, 10, 0), false},
		{"tuesday early morning belongs to monday night", night, at(2, 6, 59), true},
		{"window end is exclusive", night, at(2, 7, 0), false},
		{"saturday early morning belongs to friday night", night, at(6, 3, 0), true},
		{"saturday evening is not a listed night", night, at(6, 20, 0), false},
		{"monday early morning belongs to sunday, not listed", night, at(1, 3, 0), false},
		{"same-day window", domain.AfterHours{From: "12:00", To: "13:00"}, at(3, 12, 30), true},
		{"same-day window outside", domain.AfterHours{From: "12:00", To: "13:00"}, at(3, 13, 30), false},
		{"whole day", domain.AfterHours{From: "00:00", To: "00:00", Days: []int{0, 6}}, at(0, 15, 0), true},
		{"whole day, other day", domain.AfterHours{From: "00:00", To: "00:00", Days: []int{0, 6}}, at(1, 15, 0), false},
		{"UTC input is judged in Bangkok", night, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), true}, // 19:00 Monday ICT
	} {
		if got := InWindow(tc.w, tc.at); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestDoorAndOccupancyRules(t *testing.T) {
	base := domain.AlertRule{ID: "r1", Name: "n", Enabled: true, Severity: "warning"}
	door, occupancy := base, base
	door.EventType, occupancy.EventType = domain.RuleDoor, domain.RuleOccupancy
	if ValidateRule(door) != nil || ValidateRule(occupancy) != nil {
		t.Fatal("door and occupancy are rule event types")
	}
	opened := domain.DeviceEvent{EventType: domain.EventDoorOpen, ExternalID: "f000000000c5", OccurredAt: time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)} // 22:00 ICT Monday
	occupied := domain.DeviceEvent{EventType: domain.EventOccupied, ExternalID: "f000000000c2", OccurredAt: opened.OccurredAt}
	if !Matches(door, opened) || Matches(door, occupied) || !Matches(occupancy, occupied) || Matches(occupancy, opened) {
		t.Fatal("door fires on door_open and occupancy on occupied, nothing else")
	}
	if Matches(door, domain.DeviceEvent{EventType: domain.EventDoorClosed, OccurredAt: opened.OccurredAt}) {
		t.Fatal("closing the door is not an alert")
	}
	occupancy.Scope.AfterHours = &domain.AfterHours{From: "18:00", To: "07:00", Days: []int{1, 2, 3, 4, 5}}
	if ValidateRule(occupancy) != nil || !Matches(occupancy, occupied) {
		t.Fatal("movement at 22:00 on a Monday is after hours")
	}
	occupied.OccurredAt = time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC) // 10:00 ICT Monday
	if Matches(occupancy, occupied) {
		t.Fatal("movement during office hours must not alert")
	}
	for _, bad := range []domain.AfterHours{{From: "25:00", To: "07:00"}, {From: "18:00", To: "7:00"}, {From: "18:00", To: "07:00", Days: []int{7}}, {From: "18:00", To: "07:00", Days: []int{1, 1}}} {
		r := occupancy
		w := bad
		r.Scope.AfterHours = &w
		if ValidateRule(r) == nil {
			t.Fatalf("invalid window accepted: %+v", bad)
		}
	}
	tamper := base
	tamper.EventType = domain.EventTamper
	tamper.Scope.AfterHours = &domain.AfterHours{From: "18:00", To: "07:00"}
	if ValidateRule(tamper) == nil {
		t.Fatal("after_hours is only for door and occupancy rules")
	}
}
