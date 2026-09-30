package postgres

import (
	"aether/backend/internal/domain"
	"testing"
	"time"
)

func TestMergeTwinReadings(t *testing.T) {
	at := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	rows := []twinSample{
		{ReceivedAt: at, Reading: []byte(`{"frames":["minew-ffe1-a101@1"],"temperature":24.5,"humidity":51,"battery":90}`)},
		{ReceivedAt: at.Add(time.Second), Reading: []byte(`{"frames":["minew-ffe1-a103@1"],"temperature":0,"humidity":0,"battery":0,"metrics":{"accel_g":1}}`)},
		{ReceivedAt: at.Add(2 * time.Second), Reading: []byte(`{"frames":["minew-ffe1-a111@1"],"kind":"motion","battery":0,"metrics":{"motion":1}}`)},
		{ReceivedAt: at.Add(3 * time.Second), Reading: []byte(`{"frames":["z2m-state@1"],"temperature":0,"humidity":0,"metrics":{"door":1,"battery":0}}`)},
		{ReceivedAt: at.Add(4 * time.Second), Reading: []byte(`not json`)},
	}
	var d domain.TwinDevice
	mergeTwinReadings(&d, rows)
	if d.T == nil || *d.T != 24.5 || *d.H != 51 {
		t.Fatalf("an accelerometer frame must not zero the temperature: %v %v", d.T, d.H)
	}
	if d.Battery == nil || *d.Battery != 0 {
		t.Fatalf("a Zigbee battery of 0 is a value: %v", d.Battery)
	}
	if d.Door == nil || *d.Door != 1 || d.MotionAt == nil || !d.MotionAt.Equal(at.Add(2*time.Second)) {
		t.Fatalf("door %v motion %v", d.Door, d.MotionAt)
	}
	if got := pgTextArray([]string{`a,b`, `c"d`, `e\f`}); got != `{"a,b","c\"d","e\\f"}` {
		t.Fatalf("text array: %s", got)
	}
}
