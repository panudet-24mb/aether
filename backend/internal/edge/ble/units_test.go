package ble

import (
	"testing"

	"aether/backend/internal/tuyable"
)

func TestNormalMAC(t *testing.T) {
	for in, want := range map[string]string{
		"DC:23:4D:00:00:01": "dc:23:4d:00:00:01", "dc-23-4d-00-00-01": "dc:23:4d:00:00:01", "dc234d000001": "dc:23:4d:00:00:01",
		"dc:23:4d:00:00": "", "zz:23:4d:00:00:01": "", "": "", "dc:23:4d:00:00:01:02": "",
	} {
		if got := NormalMAC(in); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
	if reversedMAC("dc:23:4d:00:00:01") != "01:00:00:4d:23:dc" {
		t.Fatal("reversed")
	}
}

func TestValues(t *testing.T) {
	got := values(tuyable.Report{DPs: []tuyable.DP{
		{ID: 1, Type: tuyable.DPBool, Value: true},
		{ID: 2, Type: tuyable.DPValue, Value: int64(-40)},
		{ID: 3, Type: tuyable.DPEnum, Value: int64(2)},
		{ID: 4, Type: tuyable.DPString, Value: "abc"},
		{ID: 5, Type: tuyable.DPBitmap, Value: []byte{0x01, 0x02}},
		{ID: 6, Type: tuyable.DPRaw, Value: []byte{0xde, 0xad}},
	}})
	want := map[string]any{"1": true, "2": int64(-40), "3": int64(2), "4": "abc", "5": uint64(0x0102), "6": "dead"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %#v", k, got[k])
		}
	}
}

func TestStateOf(t *testing.T) {
	if stateOf(nil) != StateOK || stateOf(ErrNoAdapter) != StateNoAdapter || stateOf(ErrNoPermission) != StateNoPermission || stateOf(errOther) != StateError {
		t.Fatal("states")
	}
}

var errOther = tuyable.ErrFormat
