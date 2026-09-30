package domain

import (
	"testing"
	"time"
)

func TestTwinPeopleFor(t *testing.T) {
	named := TwinSettings{PeopleReplay: TwinPeopleNamed, DisplayPeople: TwinPeopleTracks}
	counts := DefaultTwinSettings(false)
	off := TwinSettings{PeopleReplay: TwinPeopleOff, DisplayPeople: TwinPeopleOff}
	cases := []struct {
		role, want    string
		s             TwinSettings
		display, live bool
		got           string
	}{
		{"owner", TwinPeopleNamed, named, false, false, TwinPeopleNamed},
		{"operator", TwinPeopleNamed, named, false, false, TwinPeopleNamed},
		{"viewer", TwinPeopleNamed, named, false, false, TwinPeopleTracks}, // a viewer never sees names
		{"owner", TwinPeopleNamed, counts, false, false, TwinPeopleCounts}, // the setting caps
		{"viewer", TwinPeopleTracks, counts, false, false, TwinPeopleCounts},
		{"owner", TwinPeopleTracks, named, false, false, TwinPeopleTracks}, // never more than asked
		{"owner", TwinPeopleNamed, off, false, false, TwinPeopleOff},       // replay: no people
		{"owner", TwinPeopleNamed, off, false, true, TwinPeopleCounts},     // live keeps headcounts
		{"", TwinPeopleNamed, named, true, false, TwinPeopleTracks},        // a display: its own cap, never names
		{"", TwinPeopleTracks, TwinSettings{PeopleReplay: TwinPeopleNamed, DisplayPeople: TwinPeopleCounts}, true, true, TwinPeopleCounts},
		{"owner", "", named, false, false, TwinPeopleCounts},
		{"owner", "everyone", named, false, false, TwinPeopleCounts},
		{"owner", TwinPeopleNamed, DefaultTwinSettings(true), false, false, TwinPeopleNamed}, // demo default
	}
	for _, c := range cases {
		if got := TwinPeopleFor(Principal{Role: c.role}, c.want, c.s, c.display, c.live); got != c.got {
			t.Fatalf("%s asking %q (settings %+v display %v live %v): %q, want %q", c.role, c.want, c.s, c.display, c.live, got, c.got)
		}
	}
	if TwinEventTitle(EventButton) == "" || TwinEventTitle("nope") == "" {
		t.Fatal("empty title")
	}
	for _, bad := range []TwinSettings{{PeopleReplay: "all", PeopleReplayDays: 7, DisplayPeople: "counts"}, {PeopleReplay: "counts", PeopleReplayDays: 0, DisplayPeople: "counts"},
		{PeopleReplay: "counts", PeopleReplayDays: 7, DisplayPeople: "named"}, {PeopleReplay: "counts", PeopleReplayDays: 91, DisplayPeople: "off"}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if (TwinSettings{PeopleReplay: "off", PeopleReplayDays: 90, DisplayPeople: "tracks"}).Validate() != nil {
		t.Fatal("refused valid settings")
	}
}

func TestTwinReplayBucket(t *testing.T) {
	h := time.Hour
	for _, c := range []struct {
		window time.Duration
		asked  int
		want   int
		ok     bool
	}{
		{h, 0, 300, true}, {24 * h, 0, 300, true}, {25 * h, 0, 900, true}, {7 * 24 * h, 0, 3600, true},
		{7*24*h + time.Second, 0, 0, false}, {0, 0, 0, false},
		{h, 900, 900, true}, {h, 400, 0, false}, {48 * h, 300, 0, false},
	} {
		got, e := TwinReplayBucket(c.window, c.asked)
		if (e == nil) != c.ok || got != c.want {
			t.Fatalf("window %v asked %d: %d %v", c.window, c.asked, got, e)
		}
	}
}

func TestZoneComfortBand(t *testing.T) {
	zone := func(c *[2]float64) FloorLayout {
		return FloorLayout{Zones: []FloorZone{{ID: "z1", Name: "x", Kind: "room", Color: "mint", Points: []Point{{0, 0}, {1, 0}, {1, 1}}, Comfort: c}}}
	}
	for _, ok := range []*[2]float64{nil, {2, 8}, {22, 25}} {
		l := zone(ok)
		if e := l.Validate(); e != nil {
			t.Fatalf("band %v refused: %v", ok, e)
		}
	}
	for _, bad := range []*[2]float64{{8, 2}, {5, 5}, {-80, 0}, {0, 200}} {
		l := zone(bad)
		if l.Validate() == nil {
			t.Fatalf("band %v accepted", bad)
		}
	}
}
