package domain

import "testing"

func TestValidPlaylist(t *testing.T) {
	dash := "11111111-1111-4111-8111-111111111111"
	if !ValidPlaylist(DefaultPlaylist()) {
		t.Fatal("the default playlist is invalid")
	}
	ok := [][]DisplayView{
		{{Kind: "studio", Seconds: 30, Ref: dash}},
		{{Kind: "floorplan", Seconds: 600}, {Kind: "floorplan", Seconds: 10, Ref: dash}},
	}
	for _, p := range ok {
		if !ValidPlaylist(p) {
			t.Fatalf("refused %v", p)
		}
	}
	bad := [][]DisplayView{
		nil,
		{{Kind: "overview", Seconds: 9}},
		{{Kind: "overview", Seconds: 601}},
		{{Kind: "studio", Seconds: 30}},
		{{Kind: "studio", Seconds: 30, Ref: "x"}},
		{{Kind: "alerts", Seconds: 30, Ref: dash}},
		{{Kind: "script", Seconds: 30}},
		make([]DisplayView, MaxDisplayViews+1),
	}
	for _, p := range bad {
		if ValidPlaylist(p) {
			t.Fatalf("accepted %v", p)
		}
	}
}

func TestDisplayTakeover(t *testing.T) {
	cases := []struct {
		event, severity, status string
		want                    bool
	}{
		{EventButton, "critical", "open", true},
		{EventButton, "warning", "open", false},
		{EventButton, "critical", "acknowledged", false},
		{EventHazard, "warning", "open", true},
		{EventTamper, "critical", "open", false},
	}
	for _, c := range cases {
		if DisplayTakeover(c.event, c.severity, c.status) != c.want {
			t.Fatalf("%v", c)
		}
	}
}
