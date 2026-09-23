package alerts

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"testing"
	"time"
)

// The timeline measured on the physical B10: idle (accelerometer + info only), a press starts an iBeacon
// burst with gateway-missed gaps of a few seconds, the burst ends, and a later press starts another.
func TestButtonTriggerSlot(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 47, 36, 0, time.UTC)
	accel := minew.Reading{Frames: []string{minew.FrameAccel}, Metrics: map[string]float64{"accel_g": 1}}
	ibeacon := minew.Reading{Frames: []string{minew.FrameIBeacon}, Beacon: &minew.Beacon{Type: "ibeacon"}}
	presses := 0
	st := State{}
	step := func(r minew.Reading, at time.Time) {
		var evs []domain.DeviceEvent
		evs, st = Detect(st, r, nil, at)
		for _, e := range evs {
			if e.EventType == domain.EventButton {
				presses++
			}
		}
	}
	for i := 0; i < 160; i += 4 { // ~2.7 minutes at rest
		step(accel, t0.Add(time.Duration(i)*time.Second))
	}
	if presses != 0 {
		t.Fatalf("press while idle: %d", presses)
	}
	press := t0.Add(166 * time.Second)
	for _, gap := range []int{0, 3, 1, 2, 5, 4, 3, 1, 6, 2} { // burst with missed packets
		press = press.Add(time.Duration(gap) * time.Second)
		step(ibeacon, press)
	}
	if presses != 1 {
		t.Fatalf("one burst must be one press: %d", presses)
	}
	// Burst over; a new press 2 minutes later is a second event, reporting how long the slot was silent.
	evs, _ := Detect(st, ibeacon, nil, press.Add(2*time.Minute))
	if len(evs) != 1 || evs[0].EventType != domain.EventButton || evs[0].Detail["silent_sec"] != 120 {
		t.Fatalf("second press: %v", evs)
	}
	// Just inside the quiet window it is still the same burst.
	if evs, _ := Detect(st, ibeacon, nil, press.Add(ButtonTriggerQuiet-time.Second)); len(evs) != 0 {
		t.Fatalf("inside quiet window: %v", evs)
	}
}
