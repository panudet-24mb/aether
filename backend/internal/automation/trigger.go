package automation

import (
	"sort"
	"strings"
	"time"
)

// This file holds the two decisions an uplink used to make with a database round trip per block and
// per sensor: "can this packet wake this flow at all?" and "is this the rising edge?". Both are pure
// functions over rows the caller has already loaded, so the runtime can answer for a whole packet in
// a handful of statements and both rules stay unit-testable without PostgreSQL.

// Trigger index kinds, mirroring the `kind` column of core.automation_triggers.
const (
	KindEvent  = "event"
	KindMetric = "metric"
)

// TriggerRow is one row of the trigger index: a coarse description of what can wake one trigger
// block. A nil selector means "any" (SQL NULL), so a row always matches at least everything its
// block matches — the index only narrows the candidates, MatchesEvent and MetricTest still decide.
//
// Being a deliberate superset is what keeps the index small. A trigger.event block may filter on up
// to MaxSelectors event types, devices and gateways; indexing the cross product would cost tens of
// thousands of rows per block. Instead the block records one row per event type and pins the device
// and gateway columns only when it filters on exactly one of each; a longer list widens to "any" and
// costs one extra candidate flow, never a wrong answer.
type TriggerRow struct {
	NodeID     string
	Kind       string
	EventType  *string
	ExternalID *string
	GatewayID  *string
	Metric     *string
}

// Uplink is what one gateway packet offers the trigger index.
type Uplink struct {
	// EventTypes and EventExternalIDs are the distinct types and identities of the device events this
	// packet produced; empty means the packet raised no event.
	EventTypes       []string
	EventExternalIDs []string
	// GatewayID is the gateway that sent the packet.
	GatewayID string
	// SensorIDs are the identities the packet carried a reading for.
	SensorIDs []string
}

// Matches is the Go twin of the SQL predicate the runtime runs against core.automation_triggers.
// The two are written to stay identical; this one is what the unit tests pin down.
func (r TriggerRow) Matches(u Uplink) bool {
	switch r.Kind {
	case KindEvent:
		if len(u.EventTypes) == 0 {
			return false
		}
		if r.EventType != nil && !contains(u.EventTypes, *r.EventType) {
			return false
		}
		if r.ExternalID != nil && !contains(u.EventExternalIDs, *r.ExternalID) {
			return false
		}
		return r.GatewayID == nil || *r.GatewayID == u.GatewayID
	case KindMetric:
		if len(u.SensorIDs) == 0 {
			return false
		}
		return r.ExternalID == nil || contains(u.SensorIDs, *r.ExternalID)
	}
	return false
}

// TriggerIndex derives the index rows of one definition, deduplicated and in a stable order. It is
// the single Go definition of what the index contains; migration 00020 backfills the same shape in
// SQL over the stored `definition` jsonb.
func TriggerIndex(def Definition) []TriggerRow {
	out := []TriggerRow{}
	seen := map[string]bool{}
	add := func(row TriggerRow) {
		key := strings.Join([]string{row.NodeID, row.Kind, nullable(row.EventType), nullable(row.ExternalID), nullable(row.GatewayID), nullable(row.Metric)}, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, row)
	}
	for _, n := range def.Nodes {
		if !validID(n.ID) {
			continue
		}
		switch n.Type {
		case TriggerEvent:
			// Empty selector lists mean "any" to MatchesEvent, and NULL means "any" here too.
			row := TriggerRow{NodeID: n.ID, Kind: KindEvent, ExternalID: only(n.Data.ExternalIDs), GatewayID: only(n.Data.GatewayIDs)}
			if len(n.Data.EventTypes) == 0 {
				add(row)
				continue
			}
			for _, t := range n.Data.EventTypes {
				row.EventType = text(t)
				add(row)
			}
		case TriggerMetric:
			// A metric block watches exactly one identity (Validate requires it), so it is one row.
			add(TriggerRow{NodeID: n.ID, Kind: KindMetric, ExternalID: text(strings.ToLower(n.Data.ExternalID)), Metric: text(n.Data.Metric)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		for _, pair := range [][2]string{{a.NodeID, b.NodeID}, {a.Kind, b.Kind}, {nullable(a.EventType), nullable(b.EventType)},
			{nullable(a.ExternalID), nullable(b.ExternalID)}, {nullable(a.GatewayID), nullable(b.GatewayID)}} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return false
	})
	return out
}

// only pins a selector column when the block filters on exactly one value; a longer list (or none)
// widens to "any".
func only(list []string) *string {
	if len(list) != 1 {
		return nil
	}
	return text(strings.ToLower(list[0]))
}

func text(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullable renders a selector for comparison; the sentinel sorts before every real value.
func nullable(p *string) string {
	if p == nil {
		return "\x00"
	}
	return *p
}

// EdgeKey identifies one rising-edge memory row (core.automation_state).
type EdgeKey struct{ AutomationID, NodeID, ExternalID string }

// EdgeState is one stored memory row: Since is when the comparison first held for this identity,
// Active says the episode has already fired.
type EdgeState struct {
	Active bool
	Since  time.Time
}

// EdgeProbe is one trigger.metric block asked about one identity in one uplink. Hit is the result of
// MetricTest; blocks that do not watch the identity, or whose metric the reading does not carry, are
// not probed at all and their memory is left alone.
type EdgeProbe struct {
	Key    EdgeKey
	Hit    bool
	ForSec int
}

// EdgeDecisions is everything one uplink has to write back to the rising-edge memory.
type EdgeDecisions struct {
	// Fired are the episodes that start now: the block fires and its row is marked active.
	Fired []EdgeKey
	// Clear are the rows to drop because the comparison stopped holding, so the next rise fires again.
	Clear []EdgeKey
}

// EdgeOpens returns, in key order, the memory rows that must exist before the locked read: every
// probe whose comparison holds. Inserting them first (ON CONFLICT DO NOTHING) is what makes `since`
// the moment the rise was first seen, by whichever gateway saw it first.
func EdgeOpens(probes []EdgeProbe) []EdgeKey {
	out := []EdgeKey{}
	seen := map[EdgeKey]bool{}
	for _, p := range probes {
		if p.Hit && !seen[p.Key] {
			seen[p.Key] = true
			out = append(out, p.Key)
		}
	}
	sortEdgeKeys(out)
	return out
}

// DecideEdges applies the rising-edge rule to a whole packet at once. `state` is the memory as read
// under lock after EdgeOpens has been applied, so every hit probe normally has a row.
//
// The rule is the one a per-block round trip used to apply, unchanged: the comparison going false
// drops the row; the comparison holding fires once, when it has held for for_sec, and never again
// until it goes false; `since` is never moved, so for_sec is measured from the start of the episode.
func DecideEdges(probes []EdgeProbe, state map[EdgeKey]EdgeState, at time.Time) EdgeDecisions {
	out := EdgeDecisions{Fired: []EdgeKey{}, Clear: []EdgeKey{}}
	fired, cleared := map[EdgeKey]bool{}, map[EdgeKey]bool{}
	for _, p := range probes {
		current, stored := state[p.Key]
		if !p.Hit {
			// A key with no row has no episode to end.
			if stored && !cleared[p.Key] {
				cleared[p.Key] = true
				out.Clear = append(out.Clear, p.Key)
			}
			continue
		}
		if !stored {
			// EdgeOpens created it; a row missing here (a concurrent delete) behaves like one opened now.
			current = EdgeState{Since: at}
		}
		if current.Active || at.Sub(current.Since) < time.Duration(p.ForSec)*time.Second {
			continue
		}
		if !fired[p.Key] {
			fired[p.Key] = true
			out.Fired = append(out.Fired, p.Key)
		}
	}
	sortEdgeKeys(out.Fired)
	sortEdgeKeys(out.Clear)
	return out
}

// sortEdgeKeys puts keys in the order the SQL statements use them, so two gateways ingesting the same
// identity take the same row locks in the same order and serialise instead of deadlocking.
func sortEdgeKeys(keys []EdgeKey) {
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.AutomationID != b.AutomationID {
			return a.AutomationID < b.AutomationID
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		return a.ExternalID < b.ExternalID
	})
}
