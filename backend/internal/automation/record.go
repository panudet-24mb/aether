package automation

import (
	"encoding/json"
	"time"
)

// Automation is one stored flow. Definition stays raw so the studio round-trips block positions and
// any field a newer studio adds, while the evaluator only reads what it understands.
type Automation struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	ProjectID   *string         `json:"project_id"`
	Definition  json.RawMessage `json:"definition"`
	// Revision is bumped on every save; a save carrying a stale revision is rejected with 409.
	Revision    int        `json:"revision"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	LastFiredAt *time.Time `json:"last_fired_at"`
	FireCount   int64      `json:"fire_count"`
}

// Run is one recorded evaluation, newest first in the studio's history drawer.
type Run struct {
	ID           string          `json:"id"`
	AutomationID string          `json:"automation_id"`
	TriggerNode  string          `json:"trigger_node"`
	ExternalID   string          `json:"external_id"`
	GatewayID    *string         `json:"gateway_id"`
	Status       string          `json:"status"`
	Detail       json.RawMessage `json:"detail"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Run statuses.
const (
	StatusFired   = "fired"
	StatusSkipped = "skipped"
	StatusError   = "error"
)

// MatchesEvent reports whether a trigger.event block subscribes to this device event.
// Empty selector lists mean "any".
func MatchesEvent(n Node, eventType, externalID, gatewayID string) bool {
	if n.Type != TriggerEvent {
		return false
	}
	if len(n.Data.EventTypes) > 0 && !contains(n.Data.EventTypes, eventType) {
		return false
	}
	if len(n.Data.ExternalIDs) > 0 && !contains(n.Data.ExternalIDs, externalID) {
		return false
	}
	if len(n.Data.GatewayIDs) > 0 && !contains(n.Data.GatewayIDs, gatewayID) {
		return false
	}
	return true
}

// MetricTest evaluates a trigger.metric block against a fresh reading of one identity.
// watched=false means the block does not watch this identity, or the reading carries no such metric;
// the caller then leaves the block's rising-edge state alone.
func MetricTest(n Node, externalID string, r Reading) (value float64, hit bool, watched bool) {
	if n.Type != TriggerMetric || n.Data.ExternalID != externalID {
		return 0, false, false
	}
	v, ok := Metric(r, n.Data.Metric)
	if !ok {
		return 0, false, false
	}
	return v, Compare(n.Data.Op, v, n.Data.Value), true
}

// TestInput is one hypothetical uplink submitted by the studio's "ทดสอบ" panel.
type TestInput struct {
	ExternalID string
	GatewayID  string
	// EventType fires trigger.event blocks; empty means the test is a metric test.
	EventType string
	// Value fires trigger.metric blocks whose comparison it satisfies.
	Value *float64
}

// MatchTriggers returns the trigger blocks a hypothetical uplink would fire. A dry run has no history,
// so rising edges and for_sec are deliberately ignored: the question is "what would this flow decide
// if this trigger fired now", not "would it have fired".
func MatchTriggers(def Definition, in TestInput) map[string]bool {
	out := map[string]bool{}
	for _, n := range def.Nodes {
		switch n.Type {
		case TriggerEvent:
			if in.EventType != "" && MatchesEvent(n, in.EventType, in.ExternalID, in.GatewayID) {
				out[n.ID] = true
			}
		case TriggerMetric:
			if in.Value != nil && n.Data.ExternalID == in.ExternalID && Compare(n.Data.Op, *in.Value, n.Data.Value) {
				out[n.ID] = true
			}
		}
	}
	return out
}
