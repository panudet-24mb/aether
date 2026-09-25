package automation

import (
	"encoding/json"
	"time"
)

// Per-block outcome, used by the run log and by the studio to colour the canvas after a dry run.
const (
	// OutcomeTrue: the block was reached and passed its test (a trigger fired, a condition was true).
	OutcomeTrue = "true"
	// OutcomeFalse: the block was reached but its test failed.
	OutcomeFalse = "false"
	// OutcomeIdle: nothing reached the block in this evaluation.
	OutcomeIdle = "idle"
	// OutcomeRan: an action block that the flow reached and that will be executed.
	OutcomeRan = "ran"
	// OutcomeBlocked: an action block the flow reached but the runtime refused to execute (an action.command
	// when AUTOMATION_COMMANDS is off, the loop guard, a per-device cap, the device offline, …). The run log
	// records why.
	OutcomeBlocked = "blocked"
)

// EvalContext is the world as the evaluator is allowed to see it: no database, no clock of its own.
type EvalContext interface {
	// Reading returns the newest decoded reading for an identity and how old it is.
	Reading(externalID string) (Reading, time.Duration, bool)
	// Zone returns the gateway whose zone the wearable currently sits in.
	Zone(externalID string) (string, bool)
	Now() time.Time
}

// Action is one executable step the evaluation decided on, in block order.
type Action struct {
	NodeID     string   `json:"node_id"`
	Type       string   `json:"type"`
	Severity   string   `json:"severity,omitempty"`
	Title      string   `json:"title,omitempty"`
	ChannelIDs []string `json:"channel_ids,omitempty"`
	Message    string   `json:"message,omitempty"`
	// action.command: which device, property and explicit value the flow wants set.
	DeviceID string          `json:"device_id,omitempty"`
	Property string          `json:"property,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}

// Result is the full trace of one evaluation.
type Result struct {
	// Nodes maps every block id to one of the Outcome* constants.
	Nodes map[string]string `json:"nodes"`
	// Actions are the action blocks to execute, in the order they appear in the definition.
	Actions []Action `json:"actions"`
	// Blocked names action blocks that were reached but that the runtime refused to execute.
	Blocked []string `json:"blocked,omitempty"`
}

// Fired reports whether this evaluation produced anything to execute.
func (r Result) Fired() bool { return len(r.Actions) > 0 }

// Evaluate runs one firing of the flow. `fired` is the set of trigger block ids that just fired;
// every other trigger counts as idle, so one uplink evaluates the flow from the triggers it matched.
// The definition is assumed to have passed Validate: unknown blocks and blocks caught in a cycle
// simply emit nothing rather than being rejected here.
func Evaluate(def Definition, fired map[string]bool, ctx EvalContext) Result {
	e := &evaluator{
		fired:    fired,
		ctx:      ctx,
		byID:     make(map[string]Node, len(def.Nodes)),
		incoming: map[string][]Edge{},
		out:      map[string]map[string]bool{},
		busy:     map[string]bool{},
		reached:  map[string]bool{},
	}
	for _, n := range def.Nodes {
		if _, dup := e.byID[n.ID]; !dup {
			e.byID[n.ID] = n
		}
	}
	for _, edge := range def.Edges {
		if _, okSrc := e.byID[edge.Source]; !okSrc {
			continue
		}
		if _, okDst := e.byID[edge.Target]; !okDst {
			continue
		}
		e.incoming[edge.Target] = append(e.incoming[edge.Target], edge)
	}

	result := Result{Nodes: make(map[string]string, len(def.Nodes)), Actions: []Action{}}
	for _, n := range def.Nodes {
		if e.byID[n.ID].ID != n.ID {
			continue
		}
		emits := e.compute(n.ID)
		switch {
		case IsTrigger(n.Type):
			result.Nodes[n.ID] = outcome(emits[""])
		case IsCondition(n.Type):
			switch {
			case emits[HandleTrue]:
				result.Nodes[n.ID] = OutcomeTrue
			case emits[HandleFalse]:
				result.Nodes[n.ID] = OutcomeFalse
			default:
				result.Nodes[n.ID] = OutcomeIdle
			}
		case IsLogic(n.Type):
			if emits[""] {
				result.Nodes[n.ID] = OutcomeTrue
			} else if e.reached[n.ID] {
				result.Nodes[n.ID] = OutcomeFalse
			} else {
				result.Nodes[n.ID] = OutcomeIdle
			}
		case IsAction(n.Type):
			if !e.anyInput(n.ID) {
				result.Nodes[n.ID] = OutcomeIdle
				continue
			}
			result.Nodes[n.ID] = OutcomeRan
			act := Action{NodeID: n.ID, Type: n.Type, Severity: n.Data.Severity, Title: n.Data.Title, ChannelIDs: n.Data.ChannelIDs, Message: n.Data.Message}
			if n.Type == ActionCommand {
				// Deciding is not sending: the runtime still applies the deployment switch, the loop guard and the
				// command caps, and a dry run never sends at all.
				act = Action{NodeID: n.ID, Type: n.Type, DeviceID: n.Data.DeviceID, Property: n.Data.Property, Value: n.Data.SetValue}
			}
			result.Actions = append(result.Actions, act)
		default:
			result.Nodes[n.ID] = OutcomeIdle
		}
	}
	return result
}

func outcome(ok bool) string {
	if ok {
		return OutcomeTrue
	}
	return OutcomeIdle
}

type evaluator struct {
	fired    map[string]bool
	ctx      EvalContext
	byID     map[string]Node
	incoming map[string][]Edge
	out      map[string]map[string]bool
	busy     map[string]bool
	reached  map[string]bool
}

// signal reports whether an edge carries a firing this run.
func (e *evaluator) signal(edge Edge) bool {
	handle := edge.SourceHandle
	if src, ok := e.byID[edge.Source]; ok && !IsCondition(src.Type) {
		handle = ""
	}
	return e.compute(edge.Source)[handle]
}

// emitted reports whether a block produced any verdict at all this run (true or false).
func (e *evaluator) emitted(id string) bool {
	for _, on := range e.compute(id) {
		if on {
			return true
		}
	}
	return false
}

// anyInput reports whether at least one incoming edge fired.
func (e *evaluator) anyInput(id string) bool {
	for _, edge := range e.incoming[id] {
		if e.signal(edge) {
			return true
		}
	}
	return false
}

// compute returns the handles a block emits on, memoised so every block is evaluated exactly once.
// A block reached while it is still being computed sits in a cycle and emits nothing: Validate rejects
// cycles long before this point, and cutting one here keeps a hand-edited definition from looping.
func (e *evaluator) compute(id string) map[string]bool {
	if m, done := e.out[id]; done {
		return m
	}
	if e.busy[id] {
		return map[string]bool{}
	}
	e.busy[id] = true
	defer delete(e.busy, id)

	m := map[string]bool{}
	n, ok := e.byID[id]
	if !ok {
		e.out[id] = m
		return m
	}
	switch {
	case IsTrigger(n.Type):
		m[""] = e.fired[id]
	case IsCondition(n.Type):
		if e.anyInput(id) {
			e.reached[id] = true
			if e.test(n) {
				m[HandleTrue] = true
			} else {
				m[HandleFalse] = true
			}
		}
	case IsLogic(n.Type):
		in := e.incoming[id]
		all, any := len(in) > 0, false
		for _, edge := range in {
			if e.signal(edge) {
				any = true
			} else {
				all = false
			}
			// The block was reached as soon as an upstream block produced a verdict, true or false,
			// so the run log can tell "the AND said no" apart from "nothing got this far".
			if e.emitted(edge.Source) {
				e.reached[id] = true
			}
		}
		m[""] = all && n.Type == LogicAll || any && n.Type == LogicAny
	}
	e.out[id] = m
	return m
}

// test evaluates one condition block against the world.
func (e *evaluator) test(n Node) bool {
	d := n.Data
	switch n.Type {
	case CondTime:
		return InWindow(e.ctx.Now(), d)
	case CondDevice:
		r, age, ok := e.ctx.Reading(d.ExternalID)
		if !ok {
			return false
		}
		maxAge := d.MaxAgeSec
		if maxAge <= 0 {
			maxAge = DefaultMaxAgeSec
		}
		// A stale reading does not answer the question, so the condition is false rather than assumed.
		if age > time.Duration(maxAge)*time.Second {
			return false
		}
		v, ok := Metric(r, d.Metric)
		if !ok {
			return false
		}
		return Compare(d.Op, v, d.Value)
	case CondZone:
		gateway, ok := e.ctx.Zone(d.ExternalID)
		if !ok || gateway == "" {
			return false
		}
		if len(d.GatewayIDs) == 0 {
			return true
		}
		return contains(d.GatewayIDs, gateway)
	}
	return false
}
