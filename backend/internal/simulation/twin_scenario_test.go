package simulation

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTwinDemoIsDeterministic(t *testing.T) {
	a, b := TwinDemo(), TwinDemo()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two calls built different buildings")
	}
	zones := 0
	for _, f := range a.Floors {
		zones += len(f.Zones)
	}
	if len(a.Floors) != 3 || zones != 14 || len(a.Gateways) != 10 || len(a.Sensors) != 60 || len(a.People) != 40 {
		t.Fatalf("building: %d floors, %d zones, %d gateways, %d sensors, %d people", len(a.Floors), zones, len(a.Gateways), len(a.Sensors), len(a.People))
	}
	seen := map[string]bool{}
	check := func(mac string) {
		if len(mac) != 12 || !strings.HasPrefix(mac, "f1") || seen[mac] {
			t.Fatalf("bad or duplicate MAC %q", mac)
		}
		seen[mac] = true
	}
	for _, g := range a.Gateways {
		check(g.MAC)
	}
	for _, s := range a.Sensors {
		check(s.MAC)
		z := a.Floors[s.Floor].Zones[s.Zone].Rect
		if s.X < z[0] || s.X > z[2] || s.Y < z[1] || s.Y > z[3] {
			t.Fatalf("%s is placed outside its zone", s.Name)
		}
		if a.Gateways[s.Gateway].Floor != s.Floor {
			t.Fatalf("%s is heard by a gateway on another floor", s.Name)
		}
	}
	for _, p := range a.People {
		check(p.MAC)
	}
	at := time.Unix(1_800_000_000, 0)
	for _, step := range []int{0, 17, TwinSOSStep, TwinSpikePeak} {
		pa, pb := a.TwinPackets(step, at, TwinOverrides{}), b.TwinPackets(step, at, TwinOverrides{})
		for gi := range a.Gateways {
			if !bytes.Equal(pa[gi], pb[gi]) {
				t.Fatalf("step %d gateway %d: packets differ", step, gi)
			}
			var rows []map[string]any
			if e := json.Unmarshal(pa[gi], &rows); e != nil || len(rows) < 2 || rows[0]["type"] != "Gateway" {
				t.Fatalf("step %d gateway %d: %v %d rows", step, gi, e, len(rows))
			}
		}
	}
}

func TestTwinScriptHappensWhereItShould(t *testing.T) {
	b := TwinDemo()
	sos, wander := -1, -1
	for i, p := range b.People {
		if p.SOS {
			sos = i
		}
		if p.Wander {
			wander = i
		}
	}
	if sos < 0 || wander < 0 {
		t.Fatal("the scripted SOS and wanderer are missing")
	}
	if g := b.TwinGatewayOf(sos, TwinSOSStep); g != b.Ward3Gateway {
		t.Fatalf("SOS pressed at gateway %d, want ward 3 (%d)", g, b.Ward3Gateway)
	}
	if g := b.TwinGatewayOf(wander, TwinWanderFrom+1); g != b.RestrictedGateway {
		t.Fatalf("wanderer at %d during the wander, want the restricted room %d", g, b.RestrictedGateway)
	}
	if g := b.TwinGatewayOf(wander, TwinWanderTo); g == b.RestrictedGateway {
		t.Fatal("wanderer never walked back")
	}
	// The loop repeats.
	for i := range b.People {
		if b.TwinGatewayOf(i, 5) != b.TwinGatewayOf(i, 5+TwinLoopSteps) {
			t.Fatalf("person %d: the loop does not repeat", i)
		}
	}
	// The iBeacon slot (the B10 press) appears only on the press steps, and only for the SOS wearer.
	at := time.Unix(1_800_000_000, 0)
	pressed := func(step int, o TwinOverrides) bool {
		for _, raw := range b.TwinPackets(step, at, o) {
			if bytes.Contains(raw, []byte(`"mac":"`+b.People[sos].MAC+`","rawData":"1aff4c000215`)) {
				return true
			}
		}
		return false
	}
	if !pressed(TwinSOSStep, TwinOverrides{}) || pressed(TwinSOSStep-5, TwinOverrides{}) || !pressed(3, TwinOverrides{SOS: true}) {
		t.Fatal("the SOS press is not where the script puts it")
	}
	if c := TwinColdC(TwinSpikePeak, false); c != TwinColdSpikeC {
		t.Fatalf("cold store at the peak: %.1f", c)
	}
	if c := TwinColdC(TwinSpikeFrom-1, false); c != TwinColdBaseC || TwinColdC(0, true) != TwinColdSpikeC {
		t.Fatalf("cold store before the spike: %.1f", c)
	}
	if TwinStep(time.Unix(TwinStepSec*TwinLoopSteps*7+TwinStepSec*3, 0)) != 3 {
		t.Fatal("wall-clock step")
	}
}
