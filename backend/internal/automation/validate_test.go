package automation

import (
	"encoding/json"
	"strings"
	"testing"
)

const chanA = "11111111-1111-4111-8111-111111111111"
const chanB = "22222222-2222-4222-8222-222222222222"

func node(id, kind string, d Data) Node { return Node{ID: id, Type: kind, Data: d} }

func edge(id, source, target string, handle ...string) Edge {
	e := Edge{ID: id, Source: source, Target: target}
	if len(handle) == 1 {
		e.SourceHandle = handle[0]
	}
	return e
}

// tamperFlow is the smallest valid flow: an event trigger straight into an alert.
func tamperFlow() Definition {
	return Definition{
		Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("a1", ActionAlert, Data{Severity: "critical", Title: "{{device}} ถูกแกะ"}),
		},
		Edges: []Edge{edge("e1", "t1", "a1")},
	}
}

func codes(problems []Problem) string {
	out := make([]string, 0, len(problems))
	for _, p := range problems {
		out = append(out, p.Code)
	}
	return strings.Join(out, ",")
}

func hasCode(problems []Problem, code string) bool {
	for _, p := range problems {
		if p.Code == code {
			return true
		}
	}
	return false
}

func TestCheck(t *testing.T) {
	opts := Options{Channels: map[string]bool{chanA: true}}
	cases := []struct {
		name string
		def  Definition
		opts Options
		want string // expected problem code, "" = valid
	}{
		{"empty", Definition{}, opts, "empty"},
		{"minimal valid", tamperFlow(), opts, ""},
		{"unknown type", Definition{Nodes: []Node{node("x", "trigger.telepathy", Data{})}}, opts, "unknown_type"},
		{"duplicate id", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("t1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}}, opts, "duplicate_id"},
		{"bad id", Definition{Nodes: []Node{node("a b", TriggerEvent, Data{EventTypes: []string{"tamper"}})}}, opts, "bad_id"},
		{"no trigger", Definition{Nodes: []Node{node("a1", ActionAlert, Data{Severity: "info", Title: "x"})}}, opts, "no_trigger"},
		{"no action", Definition{Nodes: []Node{node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}})}}, opts, "no_action"},
		{"unreachable action", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
			node("a2", ActionAlert, Data{Severity: "info", Title: "y"}),
		}, Edges: []Edge{edge("e1", "t1", "a1")}}, opts, "unreachable"},
		{"cycle", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("c1", CondTime, Data{From: "08:00", To: "17:00"}),
			node("c2", CondTime, Data{From: "08:00", To: "17:00"}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{
			edge("e1", "t1", "c1"), edge("e2", "c1", "c2", HandleTrue),
			edge("e3", "c2", "c1", HandleTrue), edge("e4", "c2", "a1", HandleFalse),
		}}, opts, "cycle"},
		{"edge into trigger", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("t2", TriggerEvent, Data{EventTypes: []string{"leak"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "t2"), edge("e2", "t1", "a1")}}, opts, "trigger_input"},
		{"edge out of action", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
			node("a2", ActionAlert, Data{Severity: "info", Title: "y"}),
		}, Edges: []Edge{edge("e1", "t1", "a1"), edge("e2", "a1", "a2")}}, opts, "action_output"},
		{"condition without handle", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("c1", CondTime, Data{From: "08:00", To: "17:00"}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "c1"), edge("e2", "c1", "a1")}}, opts, "bad_handle"},
		{"trigger with handle", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1", HandleTrue)}}, opts, "bad_handle"},
		{"two edges into one action", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("t2", TriggerEvent, Data{EventTypes: []string{"leak"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1"), edge("e2", "t2", "a1")}}, opts, "too_many_inputs"},
		{"dangling edge", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1"), edge("e2", "t1", "ghost")}}, opts, "dangling_edge"},
		{"unknown event type", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"telepathy"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1")}}, opts, "unknown_event_type"},
		{"metric trigger without op", Definition{Nodes: []Node{
			node("t1", TriggerMetric, Data{ExternalID: "f00000000001", Metric: "temperature"}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1")}}, opts, "bad_op"},
		{"metric trigger for_sec out of range", Definition{Nodes: []Node{
			node("t1", TriggerMetric, Data{ExternalID: "f00000000001", Metric: "temperature", Op: ">", Value: 8, ForSec: 7200}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1")}}, opts, "bad_for_sec"},
		{"bad time window", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"button"}}),
			node("c1", CondTime, Data{From: "8:00", To: "17:00"}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "c1"), edge("e2", "c1", "a1", HandleTrue)}}, opts, "bad_from"},
		{"bad weekday", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"button"}}),
			node("c1", CondTime, Data{From: "08:00", To: "17:00", Days: []int{1, 9}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "c1"), edge("e2", "c1", "a1", HandleTrue)}}, opts, "bad_days"},
		{"notify without channel", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("n1", ActionNotify, Data{Message: "hi"}),
		}, Edges: []Edge{edge("e1", "t1", "n1")}}, opts, "no_channel"},
		{"notify with foreign channel", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("n1", ActionNotify, Data{ChannelIDs: []string{chanB}, Message: "hi"}),
		}, Edges: []Edge{edge("e1", "t1", "n1")}}, opts, "unknown_channel"},
		{"notify with own channel", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("n1", ActionNotify, Data{ChannelIDs: []string{chanA}, Message: "hi"}),
		}, Edges: []Edge{edge("e1", "t1", "n1")}}, opts, ""},
		{"alert without severity", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("a1", ActionAlert, Data{Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1")}}, opts, "bad_severity"},
		{"command block may be saved as a draft", commandFlow(`"ON"`), opts, ""},
		{"empty command block names what is missing", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("k1", ActionCommand, Data{}),
		}, Edges: []Edge{edge("e1", "t1", "k1")}}, opts, "no_device"},
		{"command value must be valid JSON", commandFlow(`{`), opts, "bad_value"},
		{"command value null is missing", commandFlow(`null`), opts, "no_value"},
		{"command flow cannot be enabled while AUTOMATION_COMMANDS is off", commandFlow(`"ON"`), Options{Channels: opts.Channels, Enabled: true}, "command_disabled"},
		{"command flow enabled by a member who may not control", commandFlow(`"ON"`), Options{Channels: opts.Channels, Enabled: true, CommandsEnabled: true, MayCommand: ptr(false)}, "command_forbidden"},
		{"command flow enabled with the switch on and permission", commandFlow(`"ON"`), Options{Channels: opts.Channels, Enabled: true, CommandsEnabled: true, MayCommand: ptr(true)}, ""},
		{"command device outside the flow's scope", commandFlow(`"ON"`), Options{Channels: opts.Channels, Commandable: map[string]map[string]bool{}}, "unknown_device"},
		{"command property the device does not let us set", commandFlow(`"ON"`), Options{Channels: opts.Channels, Commandable: map[string]map[string]bool{devA: {"brightness": true}}}, "not_settable"},
		{"command value the device refuses", commandFlow(`"TOGGLE"`), Options{Channels: opts.Channels, Commandable: map[string]map[string]bool{devA: {"state_l1": true}},
			CheckValue: func(_, _ string, v json.RawMessage) string {
				if string(v) == `"TOGGLE"` {
					return "value"
				}
				return ""
			}}, "bad_value"},
		{"action filter needs the action event", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}, Actions: []string{"single"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1")}}, opts, "actions_without_action"},
		{"action filter value must be a plain name", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"action"}, Actions: []string{"bad value!"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1")}}, opts, "bad_actions"},
		{"zigbee event types are accepted", Definition{Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"switch_on", "switch_off", "hazard", "hazard_cleared", "action"}, Actions: []string{"single"}}),
			node("a1", ActionAlert, Data{Severity: "info", Title: "x"}),
		}, Edges: []Edge{edge("e1", "t1", "a1")}}, opts, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.def, tc.opts)
			if got == nil {
				t.Fatal("Check must never return nil")
			}
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected a valid flow, got %s", codes(got))
				}
				if e := Validate(tc.def, tc.opts); e != nil {
					t.Fatalf("Validate disagrees with Check: %v", e)
				}
				return
			}
			if !hasCode(got, tc.want) {
				t.Fatalf("expected problem %q, got %s", tc.want, codes(got))
			}
			if e := Validate(tc.def, tc.opts); e == nil {
				t.Fatal("Validate accepted a flow Check rejected")
			}
		})
	}
}

func TestCheckLimits(t *testing.T) {
	big := Definition{}
	for i := 0; i <= MaxNodes; i++ {
		big.Nodes = append(big.Nodes, node("n"+string(rune('a'+i%26))+string(rune('a'+i/26)), TriggerEvent, Data{EventTypes: []string{"tamper"}}))
	}
	if !hasCode(Check(big, Options{}), "too_many_nodes") {
		t.Fatal("node cap not enforced")
	}
	wide := tamperFlow()
	for i := 0; i <= MaxEdges; i++ {
		wide.Edges = append(wide.Edges, edge("x"+string(rune('a'+i%26))+string(rune('a'+i/26)), "t1", "a1"))
	}
	if !hasCode(Check(wide, Options{}), "too_many_edges") {
		t.Fatal("edge cap not enforced")
	}
}

// Channels nil means "do not check membership" (used by the studio's live validate before a save).
func TestCheckSkipsChannelMembershipWhenUnknown(t *testing.T) {
	def := Definition{
		Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"tamper"}}),
			node("n1", ActionNotify, Data{ChannelIDs: []string{chanB}, Message: "hi"}),
		},
		Edges: []Edge{edge("e1", "t1", "n1")},
	}
	if problems := Check(def, Options{}); len(problems) != 0 {
		t.Fatalf("unexpected problems: %s", codes(problems))
	}
}

const devA = "33333333-3333-4333-8333-333333333333"

func ptr(b bool) *bool { return &b }

// commandFlow is a door event driving one action.command on devA.
func commandFlow(value string) Definition {
	return Definition{
		Nodes: []Node{
			node("t1", TriggerEvent, Data{EventTypes: []string{"door_open"}}),
			node("k1", ActionCommand, Data{DeviceID: devA, Property: "state_l1", SetValue: json.RawMessage(value)}),
		},
		Edges: []Edge{edge("e1", "t1", "k1")},
	}
}

func TestHasCommand(t *testing.T) {
	if HasCommand(tamperFlow()) || !HasCommand(commandFlow(`"ON"`)) {
		t.Fatal("HasCommand")
	}
}
