package automation

import (
	"testing"
	"time"
)

type world struct {
	now      time.Time
	readings map[string]Reading
	ages     map[string]time.Duration
	zones    map[string]string
}

func (w world) Now() time.Time { return w.now }
func (w world) Reading(external string) (Reading, time.Duration, bool) {
	r, ok := w.readings[external]
	return r, w.ages[external], ok
}
func (w world) Zone(external string) (string, bool) {
	g, ok := w.zones[external]
	return g, ok
}

func at(s string) time.Time {
	t, e := time.ParseInLocation("2006-01-02 15:04", s, Bangkok())
	if e != nil {
		panic(e)
	}
	return t
}

func newWorld() world {
	return world{now: at("2026-09-21 10:00"), readings: map[string]Reading{}, ages: map[string]time.Duration{}, zones: map[string]string{}}
}

func TestEvaluateTriggerToAction(t *testing.T) {
	def := tamperFlow()
	r := Evaluate(def, map[string]bool{"t1": true}, newWorld())
	if !r.Fired() || len(r.Actions) != 1 || r.Actions[0].NodeID != "a1" {
		t.Fatalf("actions: %+v", r.Actions)
	}
	if r.Nodes["t1"] != OutcomeTrue || r.Nodes["a1"] != OutcomeRan {
		t.Fatalf("outcomes: %+v", r.Nodes)
	}
	idle := Evaluate(def, map[string]bool{}, newWorld())
	if idle.Fired() {
		t.Fatalf("a flow with no fired trigger must not act: %+v", idle.Actions)
	}
	if idle.Nodes["t1"] != OutcomeIdle || idle.Nodes["a1"] != OutcomeIdle {
		t.Fatalf("outcomes: %+v", idle.Nodes)
	}
}

// A condition sends the run down exactly one of its two handles.
func TestEvaluateConditionBranches(t *testing.T) {
	def := Definition{
		Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"button"}}),
			node("c1", CondDevice, Data{ExternalID: "f00000000001", Metric: "temperature", Op: ">", Value: 8, MaxAgeSec: 300}),
			node("hot", ActionAlert, Data{Severity: "critical", Title: "ร้อน"}),
			node("cold", ActionAlert, Data{Severity: "info", Title: "เย็นดี"}),
		},
		Edges: []Edge{edge("e1", "t1", "c1"), edge("e2", "c1", "hot", HandleTrue), edge("e3", "c1", "cold", HandleFalse)},
	}
	w := newWorld()
	w.readings["f00000000001"] = Reading{Temperature: 12}
	w.ages["f00000000001"] = 30 * time.Second
	r := Evaluate(def, map[string]bool{"t1": true}, w)
	if len(r.Actions) != 1 || r.Actions[0].NodeID != "hot" {
		t.Fatalf("true branch: %+v", r.Actions)
	}
	if r.Nodes["c1"] != OutcomeTrue || r.Nodes["cold"] != OutcomeIdle {
		t.Fatalf("outcomes: %+v", r.Nodes)
	}
	w.readings["f00000000001"] = Reading{Temperature: 4}
	r = Evaluate(def, map[string]bool{"t1": true}, w)
	if len(r.Actions) != 1 || r.Actions[0].NodeID != "cold" {
		t.Fatalf("false branch: %+v", r.Actions)
	}
	if r.Nodes["c1"] != OutcomeFalse || r.Nodes["hot"] != OutcomeIdle {
		t.Fatalf("outcomes: %+v", r.Nodes)
	}
	// A reading older than max_age_sec cannot answer the question, so the condition is false.
	w.readings["f00000000001"] = Reading{Temperature: 12}
	w.ages["f00000000001"] = 20 * time.Minute
	if r = Evaluate(def, map[string]bool{"t1": true}, w); r.Actions[0].NodeID != "cold" {
		t.Fatalf("stale reading must not satisfy the condition: %+v", r.Actions)
	}
	// No reading at all behaves the same way.
	delete(w.readings, "f00000000001")
	if r = Evaluate(def, map[string]bool{"t1": true}, w); r.Actions[0].NodeID != "cold" {
		t.Fatalf("missing reading must not satisfy the condition: %+v", r.Actions)
	}
}

func logicFlow(kind string) Definition {
	return Definition{
		Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("c1", CondTime, Data{From: "08:00", To: "18:00"}),
			node("c2", CondZone, Data{ExternalID: "f00000000007"}),
			node("g1", kind, Data{}),
			node("a1", ActionAlert, Data{Severity: "warning", Title: "x"}),
		},
		Edges: []Edge{
			edge("e1", "t1", "c1"), edge("e2", "t1", "c2"),
			edge("e3", "c1", "g1", HandleTrue), edge("e4", "c2", "g1", HandleTrue),
			edge("e5", "g1", "a1"),
		},
	}
}

func TestEvaluateLogicAllAndAny(t *testing.T) {
	both := newWorld()
	both.zones["f00000000007"] = "gw-1"
	onlyTime := newWorld() // in the window, but the wearable has no zone
	night := newWorld()
	night.now = at("2026-09-21 23:00")
	night.zones["f00000000007"] = "gw-1"
	neither := newWorld()
	neither.now = at("2026-09-21 23:00")

	cases := []struct {
		kind  string
		w     world
		fired bool
		state string
	}{
		{LogicAll, both, true, OutcomeTrue},
		{LogicAll, onlyTime, false, OutcomeFalse},
		{LogicAll, neither, false, OutcomeFalse},
		{LogicAny, both, true, OutcomeTrue},
		{LogicAny, onlyTime, true, OutcomeTrue},
		{LogicAny, night, true, OutcomeTrue},
		{LogicAny, neither, false, OutcomeFalse},
	}
	for _, tc := range cases {
		r := Evaluate(logicFlow(tc.kind), map[string]bool{"t1": true}, tc.w)
		if r.Fired() != tc.fired {
			t.Fatalf("%s fired=%v want %v (nodes %+v)", tc.kind, r.Fired(), tc.fired, r.Nodes)
		}
		if r.Nodes["g1"] != tc.state {
			t.Fatalf("%s outcome %q want %q", tc.kind, r.Nodes["g1"], tc.state)
		}
	}
	// A logic block nothing reaches stays idle.
	r := Evaluate(logicFlow(LogicAll), map[string]bool{}, both)
	if r.Nodes["g1"] != OutcomeIdle || r.Fired() {
		t.Fatalf("unreached logic block: %+v", r.Nodes)
	}
}

func TestEvaluateZoneList(t *testing.T) {
	def := Definition{
		Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"button"}}),
			node("c1", CondZone, Data{ExternalID: "f00000000007", GatewayIDs: []string{"gw-ward-a"}}),
			node("a1", ActionAlert, Data{Severity: "critical", Title: "x"}),
		},
		Edges: []Edge{edge("e1", "t1", "c1"), edge("e2", "c1", "a1", HandleTrue)},
	}
	w := newWorld()
	w.zones["f00000000007"] = "gw-ward-b"
	if Evaluate(def, map[string]bool{"t1": true}, w).Fired() {
		t.Fatal("wearable outside the listed zone must not fire")
	}
	w.zones["f00000000007"] = "gw-ward-a"
	if !Evaluate(def, map[string]bool{"t1": true}, w).Fired() {
		t.Fatal("wearable inside the listed zone must fire")
	}
}

func TestEvaluateCommandBlockNeverRuns(t *testing.T) {
	def := Definition{
		Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("k1", ActionCommand, Data{}),
		},
		Edges: []Edge{edge("e1", "t1", "k1")},
	}
	r := Evaluate(def, map[string]bool{"t1": true}, newWorld())
	if r.Fired() {
		t.Fatalf("Aether has no downlink; action.command must never execute: %+v", r.Actions)
	}
	if r.Nodes["k1"] != OutcomeBlocked || len(r.Blocked) != 1 {
		t.Fatalf("blocked trace: %+v %+v", r.Nodes, r.Blocked)
	}
}

// Validate rejects cycles, so one can only reach Evaluate through a hand-edited definition.
// The contract there is narrow but absolute: terminate, and never execute an action twice.
func TestEvaluateCycleTerminates(t *testing.T) {
	def := Definition{
		Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("g1", LogicAny, Data{}),
			node("g2", LogicAny, Data{}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		},
		Edges: []Edge{edge("e1", "t1", "g1"), edge("e2", "g1", "g2"), edge("e3", "g2", "g1"), edge("e4", "g2", "a1")},
	}
	done := make(chan Result, 1)
	go func() { done <- Evaluate(def, map[string]bool{"t1": true}, newWorld()) }()
	select {
	case r := <-done:
		// The blocks inside the cut are evaluated once, so no action can run more than once.
		if len(r.Actions) > 1 {
			t.Fatalf("a cycle must not multiply actions: %+v", r.Actions)
		}
		if r.Nodes["t1"] != OutcomeTrue || len(r.Nodes) != 4 {
			t.Fatalf("every block still gets an outcome: %+v", r.Nodes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Evaluate looped on a cyclic definition")
	}
}

func TestInWindow(t *testing.T) {
	cases := []struct {
		name string
		data Data
		now  string
		want bool
	}{
		{"inside day window", Data{From: "08:00", To: "18:00"}, "2026-09-21 10:00", true},
		{"before day window", Data{From: "08:00", To: "18:00"}, "2026-09-21 07:59", false},
		{"end is exclusive", Data{From: "08:00", To: "18:00"}, "2026-09-21 18:00", false},
		{"start is inclusive", Data{From: "08:00", To: "18:00"}, "2026-09-21 08:00", true},
		{"crossing midnight, late", Data{From: "22:00", To: "06:00"}, "2026-09-21 23:30", true},
		{"crossing midnight, early", Data{From: "22:00", To: "06:00"}, "2026-09-21 05:59", true},
		{"crossing midnight, outside", Data{From: "22:00", To: "06:00"}, "2026-09-21 12:00", false},
		{"crossing midnight, boundary", Data{From: "22:00", To: "06:00"}, "2026-09-21 06:00", false},
		{"equal bounds are the whole day", Data{From: "00:00", To: "00:00"}, "2026-09-21 13:37", true},
		// 2026-09-21 is a Monday (weekday 1).
		{"weekday allowed", Data{From: "00:00", To: "23:59", Days: []int{1, 2}}, "2026-09-21 09:00", true},
		{"weekday blocked", Data{From: "00:00", To: "23:59", Days: []int{0, 6}}, "2026-09-21 09:00", false},
		{"no days means every day", Data{From: "00:00", To: "23:59"}, "2026-09-20 09:00", true},
		{"unparsable window", Data{From: "8:00", To: "18:00"}, "2026-09-21 09:00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InWindow(at(tc.now), tc.data); got != tc.want {
				t.Fatalf("InWindow=%v want %v", got, tc.want)
			}
		})
	}
	// The window is read in Asia/Bangkok whatever zone the timestamp carries.
	utc := at("2026-09-21 23:30").UTC()
	if !InWindow(utc, Data{From: "22:00", To: "06:00"}) {
		t.Fatal("a UTC timestamp must still be read in Asia/Bangkok")
	}
}

func TestMetricAndCompare(t *testing.T) {
	rssi := -62
	r := Reading{Temperature: 7.5, Humidity: 61, Battery: 88, RSSI: &rssi, Metrics: map[string]float64{"tamper": 1, "accel_g": 0.98}}
	cases := []struct {
		metric string
		want   float64
		ok     bool
	}{
		{"temperature", 7.5, true}, {"humidity", 61, true}, {"battery", 88, true},
		{"rssi", -62, true}, {"tamper", 1, true}, {"accel_g", 0.98, true}, {"pressure", 0, false},
	}
	for _, tc := range cases {
		got, ok := Metric(r, tc.metric)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("Metric(%q)=%v,%v want %v,%v", tc.metric, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := Metric(Reading{}, "rssi"); ok {
		t.Fatal("a reading without RSSI must not answer an rssi comparison")
	}
	for _, tc := range []struct {
		op   string
		a, b float64
		want bool
	}{
		{">", 2, 1, true}, {">", 1, 1, false}, {">=", 1, 1, true},
		{"<", 1, 2, true}, {"<=", 2, 2, true}, {"<", 2, 2, false}, {"==", 1, 1, false},
	} {
		if got := Compare(tc.op, tc.a, tc.b); got != tc.want {
			t.Fatalf("Compare(%q,%v,%v)=%v want %v", tc.op, tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRender(t *testing.T) {
	got := Render("{{device}} · {{event}} · {{value}} °C · {{unknown}}", map[string]string{"device": "ห้องเย็น 1", "event": "tamper", "value": "9.2"})
	if got != "ห้องเย็น 1 · tamper · 9.2 °C · {{unknown}}" {
		t.Fatalf("Render=%q", got)
	}
	if Render("no placeholders", nil) != "no placeholders" {
		t.Fatal("Render changed a template with no placeholders")
	}
}
