package automation

import (
	"testing"
	"time"
)

func rowFor(t *testing.T, rows []TriggerRow, nodeID, eventType string) TriggerRow {
	t.Helper()
	for _, r := range rows {
		if r.NodeID == nodeID && nullable(r.EventType) == eventType {
			return r
		}
	}
	t.Fatalf("no index row for %q/%q in %+v", nodeID, eventType, rows)
	return TriggerRow{}
}

// A block that names one device and one gateway pins both columns; a list of several widens to
// "any", because an extra candidate flow is cheap and a missed one would silently stop working.
func TestTriggerIndexShape(t *testing.T) {
	def := Definition{Nodes: []Node{
		node("t1", TriggerEvent, Data{EventTypes: []string{"tamper", "leak"}, ExternalIDs: []string{"AA:BB"}, GatewayIDs: []string{"11111111-1111-1111-1111-111111111111"}}),
		node("t2", TriggerEvent, Data{EventTypes: []string{"offline"}, ExternalIDs: []string{"aa", "bb"}}),
		node("t3", TriggerMetric, Data{ExternalID: "f00000000001", Metric: "temperature", Op: ">", Value: 28}),
		node("c1", CondTime, Data{From: "08:00", To: "17:00"}),
	}}
	rows := TriggerIndex(def)
	if len(rows) != 4 {
		t.Fatalf("two event types, one any-device event and one metric block make four rows: %+v", rows)
	}
	tamper := rowFor(t, rows, "t1", "tamper")
	if tamper.Kind != KindEvent || tamper.ExternalID == nil || *tamper.ExternalID != "aa:bb" {
		t.Fatalf("a single device selector is pinned, lower-cased: %+v", tamper)
	}
	if tamper.GatewayID == nil || *tamper.GatewayID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("a single gateway selector is pinned: %+v", tamper)
	}
	offline := rowFor(t, rows, "t2", "offline")
	if offline.ExternalID != nil || offline.GatewayID != nil {
		t.Fatalf("a list of several devices widens to any: %+v", offline)
	}
	metric := rowFor(t, rows, "t3", "\x00")
	if metric.Kind != KindMetric || *metric.ExternalID != "f00000000001" || *metric.Metric != "temperature" {
		t.Fatalf("a metric block is one row naming its identity: %+v", metric)
	}
	if metric.EventType != nil {
		t.Fatalf("event_type is not applicable to a metric row: %+v", metric)
	}
}

// An empty event_types list means "any event" to MatchesEvent, so it must mean NULL here.
func TestTriggerIndexEmptySelectorsAreAny(t *testing.T) {
	rows := TriggerIndex(Definition{Nodes: []Node{node("t1", TriggerEvent, Data{})}})
	if len(rows) != 1 || rows[0].EventType != nil || rows[0].ExternalID != nil || rows[0].GatewayID != nil {
		t.Fatalf("empty selectors index as any: %+v", rows)
	}
	if !rows[0].Matches(Uplink{EventTypes: []string{"button"}, EventExternalIDs: []string{"aa"}, GatewayID: "g"}) {
		t.Fatal("an any-row must match every event")
	}
}

// Duplicate event types and repeated blocks must not multiply rows.
func TestTriggerIndexDeduplicates(t *testing.T) {
	rows := TriggerIndex(Definition{Nodes: []Node{
		node("t1", TriggerEvent, Data{EventTypes: []string{"tamper", "tamper"}}),
		node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
	}})
	if len(rows) != 1 {
		t.Fatalf("identical rows collapse: %+v", rows)
	}
}

// Matches is the candidate-selection rule the SQL predicate mirrors.
func TestTriggerRowMatches(t *testing.T) {
	uplink := Uplink{
		EventTypes: []string{"tamper"}, EventExternalIDs: []string{"aa"},
		GatewayID: "g1", SensorIDs: []string{"aa", "bb"},
	}
	device, gateway, other := "aa", "g1", "g2"
	cases := []struct {
		name string
		row  TriggerRow
		want bool
	}{
		{"event type, device and gateway all match", TriggerRow{Kind: KindEvent, EventType: text("tamper"), ExternalID: &device, GatewayID: &gateway}, true},
		{"another event type", TriggerRow{Kind: KindEvent, EventType: text("leak")}, false},
		{"another device", TriggerRow{Kind: KindEvent, EventType: text("tamper"), ExternalID: text("zz")}, false},
		{"another gateway", TriggerRow{Kind: KindEvent, EventType: text("tamper"), GatewayID: &other}, false},
		{"any gateway", TriggerRow{Kind: KindEvent, EventType: text("tamper")}, true},
		{"metric on a sensor in the packet", TriggerRow{Kind: KindMetric, ExternalID: text("bb")}, true},
		{"metric on a sensor that is not", TriggerRow{Kind: KindMetric, ExternalID: text("zz")}, false},
		{"metric on any sensor", TriggerRow{Kind: KindMetric}, true},
	}
	for _, c := range cases {
		if got := c.row.Matches(uplink); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	// A packet with no events can wake no event trigger, however open the row is.
	if (TriggerRow{Kind: KindEvent}).Matches(Uplink{SensorIDs: []string{"aa"}}) {
		t.Error("an event row must not match a packet that raised no event")
	}
	// ...and a packet with no readings can wake no metric trigger.
	if (TriggerRow{Kind: KindMetric}).Matches(Uplink{EventTypes: []string{"tamper"}, EventExternalIDs: []string{"aa"}}) {
		t.Error("a metric row must not match a packet with no readings")
	}
}

func key(node, external string) EdgeKey {
	return EdgeKey{AutomationID: "flow", NodeID: node, ExternalID: external}
}

// The batched rule must reproduce the per-block one exactly: fire once on the rise, wait out for_sec,
// stay silent while the episode is open, and forget the episode when the comparison stops holding.
func TestDecideEdges(t *testing.T) {
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	probes := []EdgeProbe{
		{Key: key("n1", "a"), Hit: true},             // fresh rise, no dwell
		{Key: key("n2", "a"), Hit: true, ForSec: 60}, // rise, dwell not yet served
		{Key: key("n3", "a"), Hit: true, ForSec: 60}, // dwell served
		{Key: key("n4", "a"), Hit: true},             // episode already fired
		{Key: key("n5", "a"), Hit: false},            // comparison ended
		{Key: key("n6", "a"), Hit: false},            // never held, nothing to end
	}
	state := map[EdgeKey]EdgeState{
		key("n1", "a"): {Since: at},
		key("n2", "a"): {Since: at.Add(-30 * time.Second)},
		key("n3", "a"): {Since: at.Add(-90 * time.Second)},
		key("n4", "a"): {Active: true, Since: at.Add(-time.Hour)},
		key("n5", "a"): {Active: true, Since: at.Add(-time.Hour)},
	}
	got := DecideEdges(probes, state, at)
	if len(got.Fired) != 2 || got.Fired[0] != key("n1", "a") || got.Fired[1] != key("n3", "a") {
		t.Fatalf("only the rise with its dwell served fires: %+v", got.Fired)
	}
	if len(got.Clear) != 1 || got.Clear[0] != key("n5", "a") {
		t.Fatalf("only the episode that ended is cleared: %+v", got.Clear)
	}
}

// A dwell of exactly for_sec is served: `at - since >= for_sec` fires, as the per-block rule did.
func TestDecideEdgesDwellBoundary(t *testing.T) {
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	probes := []EdgeProbe{{Key: key("n1", "a"), Hit: true, ForSec: 60}}
	if fired := DecideEdges(probes, map[EdgeKey]EdgeState{key("n1", "a"): {Since: at.Add(-59 * time.Second)}}, at).Fired; len(fired) != 0 {
		t.Fatalf("one second short must not fire: %+v", fired)
	}
	if fired := DecideEdges(probes, map[EdgeKey]EdgeState{key("n1", "a"): {Since: at.Add(-60 * time.Second)}}, at).Fired; len(fired) != 1 {
		t.Fatalf("exactly for_sec fires: %+v", fired)
	}
}

// A hit whose row is missing (a concurrent delete between the insert and the read) behaves like a
// row opened now, so a zero dwell still fires and a real dwell still waits.
func TestDecideEdgesMissingRow(t *testing.T) {
	at := time.Now().UTC()
	empty := map[EdgeKey]EdgeState{}
	if fired := DecideEdges([]EdgeProbe{{Key: key("n1", "a"), Hit: true}}, empty, at).Fired; len(fired) != 1 {
		t.Fatalf("a missing row with no dwell fires: %+v", fired)
	}
	if fired := DecideEdges([]EdgeProbe{{Key: key("n1", "a"), Hit: true, ForSec: 30}}, empty, at).Fired; len(fired) != 0 {
		t.Fatalf("a missing row with a dwell waits: %+v", fired)
	}
	if cleared := DecideEdges([]EdgeProbe{{Key: key("n1", "a"), Hit: false}}, empty, at).Clear; len(cleared) != 0 {
		t.Fatalf("there is nothing to clear: %+v", cleared)
	}
}

// EdgeOpens is the insert list: every holding comparison, deduplicated, in lock order.
func TestEdgeOpens(t *testing.T) {
	probes := []EdgeProbe{
		{Key: key("n2", "b"), Hit: true},
		{Key: key("n1", "a"), Hit: true},
		{Key: key("n1", "a"), Hit: true},
		{Key: key("n3", "c"), Hit: false},
	}
	got := EdgeOpens(probes)
	if len(got) != 2 || got[0] != key("n1", "a") || got[1] != key("n2", "b") {
		t.Fatalf("holding comparisons only, deduplicated and in key order: %+v", got)
	}
}
