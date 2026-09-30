package domain

import "testing"

func TestTwinPeopleFor(t *testing.T) {
	cases := []struct {
		role, want string
		demo       bool
		got        string
	}{
		{"owner", TwinPeopleNamed, true, TwinPeopleNamed},
		{"operator", TwinPeopleNamed, true, TwinPeopleNamed},
		{"viewer", TwinPeopleNamed, true, TwinPeopleCounts},
		{"owner", TwinPeopleNamed, false, TwinPeopleCounts},
		{"viewer", TwinPeopleTracks, false, TwinPeopleTracks},
		{"owner", "", false, TwinPeopleCounts},
		{"owner", "everyone", true, TwinPeopleCounts},
	}
	for _, c := range cases {
		if got := TwinPeopleFor(Principal{Role: c.role}, c.want, c.demo); got != c.got {
			t.Fatalf("%s asking %q (demo %v): %q, want %q", c.role, c.want, c.demo, got, c.got)
		}
	}
	if TwinEventTitle(EventButton) == "" || TwinEventTitle("nope") == "" {
		t.Fatal("empty title")
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
