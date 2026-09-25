// Package automation is the rule language behind the Automation Studio: a flow of blocks
// ("เมื่อ … ถ้า … ให้ …") drawn in the browser, stored as JSON and evaluated here.
//
// It is pure Go with no database access. Everything the evaluator needs to know about the world
// (latest readings, the zone a wearable is in, the current time) arrives through EvalContext, so the
// whole language — validation, logic, time windows — is unit-testable without PostgreSQL.
package automation

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Block types. Triggers have no input, conditions have one input and two output handles,
// logic blocks join several inputs, actions have one input and no output.
const (
	TriggerEvent  = "trigger.event"
	TriggerMetric = "trigger.metric"
	CondTime      = "cond.time"
	CondDevice    = "cond.device"
	CondZone      = "cond.zone"
	LogicAll      = "logic.all"
	LogicAny      = "logic.any"
	ActionAlert   = "action.alert"
	ActionNotify  = "action.notify"
	// ActionCommand sets one property of a registered Zigbee2MQTT device to an explicit value, through the same
	// command queue as a click in the web UI. Enabling a flow that holds one needs the deployment switch
	// AUTOMATION_COMMANDS and a member allowed to control devices (docs/platform/automation.md).
	ActionCommand = "action.command"
)

// NodeTypes is the closed set accepted by Validate, in palette order.
var NodeTypes = []string{TriggerEvent, TriggerMetric, CondTime, CondDevice, CondZone, LogicAll, LogicAny, ActionAlert, ActionNotify, ActionCommand}

// Ops is the closed set of numeric comparisons.
var Ops = []string{">", ">=", "<", "<="}

// Severities mirrors domain.Severities; repeated here so the package stays dependency-free.
var Severities = []string{"info", "warning", "critical"}

// Condition blocks emit on exactly one of these two handles; every other block emits on "".
const (
	HandleTrue  = "true"
	HandleFalse = "false"
)

const (
	MaxNodes           = 40
	MaxEdges           = 80
	MaxDefinitionBytes = 32 * 1024
	MaxIDLen           = 64
	MaxTextLen         = 200
	MaxForSec          = 3600
	MaxMaxAgeSec       = 86400
	DefaultMaxAgeSec   = 300
	// MaxChannels bounds one action.notify; a tenant cannot own many more channels than this anyway.
	MaxChannels = 10
	// MaxSelectors bounds the id lists a trigger may filter on.
	MaxSelectors = 32
	// MaxCommandValueBytes bounds action.command's value, the same cap a command from the web UI has.
	MaxCommandValueBytes = 1024
)

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Data is the union of every block's settings. Each block type reads only its own fields;
// Validate rejects the ones that do not belong to the type it is checking.
type Data struct {
	// trigger.event
	EventTypes []string `json:"event_types,omitempty"`
	// trigger.event, cond.zone: empty means "any".
	GatewayIDs []string `json:"gateway_ids,omitempty"`
	// trigger.event: empty means "any device".
	ExternalIDs []string `json:"external_ids,omitempty"`
	// trigger.metric, cond.device, cond.zone
	ExternalID string `json:"external_id,omitempty"`
	// trigger.metric, cond.device
	Metric string  `json:"metric,omitempty"`
	Op     string  `json:"op,omitempty"`
	Value  float64 `json:"value,omitempty"`
	// trigger.metric: the comparison must hold this long before the flow fires (0 = immediately).
	ForSec int `json:"for_sec,omitempty"`
	// cond.device: a reading older than this does not answer the question, so the condition is false.
	MaxAgeSec int `json:"max_age_sec,omitempty"`
	// cond.time, "HH:MM" in Asia/Bangkok; Days are time.Weekday numbers (0 = Sunday), empty = every day.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Days []int  `json:"days,omitempty"`
	// action.alert
	Severity string `json:"severity,omitempty"`
	Title    string `json:"title,omitempty"`
	// action.notify
	ChannelIDs []string `json:"channel_ids,omitempty"`
	Message    string   `json:"message,omitempty"`
	// trigger.event: when the block listens to `action` (a remote or button), only these action values fire it;
	// empty means any press. Other event types in the same block are not filtered.
	Actions []string `json:"actions,omitempty"`
	// action.command: the registered device (core.devices id), the Zigbee2MQTT property and the explicit value to
	// set. There is no toggle here: a flow must say what state it wants, so a repeat can never flip it back.
	DeviceID string          `json:"device_id,omitempty"`
	Property string          `json:"property,omitempty"`
	SetValue json.RawMessage `json:"set_value,omitempty"`
	// Free label kept by the studio so a block can be renamed without changing its behaviour.
	Label string `json:"label,omitempty"`
}

type Node struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Position Position `json:"position"`
	Data     Data     `json:"data"`
}

type Edge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	// SourceHandle is "true" or "false" out of a condition and empty everywhere else.
	SourceHandle string `json:"sourceHandle,omitempty"`
}

type Definition struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// IsTrigger / IsCondition / IsAction classify a block type for the palette, the canvas and validation.
func IsTrigger(t string) bool   { return t == TriggerEvent || t == TriggerMetric }
func IsCondition(t string) bool { return t == CondTime || t == CondDevice || t == CondZone }
func IsLogic(t string) bool     { return t == LogicAll || t == LogicAny }
func IsAction(t string) bool    { return t == ActionAlert || t == ActionNotify || t == ActionCommand }

func known(t string) bool {
	for _, k := range NodeTypes {
		if k == t {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func containsInt(list []int, want int) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Reading is the decoded state of one identity, shaped like the `reading` jsonb in core.sensor_samples
// so a stored sample unmarshals straight into it.
type Reading struct {
	Temperature float64            `json:"temperature"`
	Humidity    float64            `json:"humidity"`
	Battery     int                `json:"battery"`
	RSSI        *int               `json:"rssi"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
}

// Metric resolves a metric name against a reading. The four built-ins shadow decoded metric keys of
// the same name, so "temperature" always means the temperature the rest of Aether shows.
func Metric(r Reading, name string) (float64, bool) {
	switch name {
	case "temperature":
		return r.Temperature, true
	case "humidity":
		return r.Humidity, true
	case "battery":
		return float64(r.Battery), true
	case "rssi":
		if r.RSSI == nil {
			return 0, false
		}
		return float64(*r.RSSI), true
	}
	v, ok := r.Metrics[name]
	return v, ok
}

// Compare applies one of Ops; an unknown operator is never true.
func Compare(op string, value, threshold float64) bool {
	switch op {
	case ">":
		return value > threshold
	case ">=":
		return value >= threshold
	case "<":
		return value < threshold
	case "<=":
		return value <= threshold
	}
	return false
}

var bangkok = func() *time.Location {
	if loc, e := time.LoadLocation("Asia/Bangkok"); e == nil {
		return loc
	}
	return time.FixedZone("ICT", 7*3600)
}()

// Bangkok is the single timezone cond.time works in (Aether is deployed for Thai sites).
func Bangkok() *time.Location { return bangkok }

// parseHHMM turns "07:30" into minutes past midnight.
func parseHHMM(s string) (int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, false
	}
	hh, e1 := strconv.Atoi(h)
	mm, e2 := strconv.Atoi(m)
	if e1 != nil || e2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// InWindow reports whether `at` falls inside a cond.time block's window, in Asia/Bangkok.
// from == to means the whole day; from > to is a window that crosses midnight (e.g. 22:00–06:00).
func InWindow(at time.Time, d Data) bool {
	t := at.In(bangkok)
	if len(d.Days) > 0 && !containsInt(d.Days, int(t.Weekday())) {
		return false
	}
	from, okFrom := parseHHMM(d.From)
	to, okTo := parseHHMM(d.To)
	if !okFrom || !okTo {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	if from == to {
		return true
	}
	if from < to {
		return now >= from && now < to
	}
	return now >= from || now < to
}

// Render substitutes the placeholders the studio offers in a title or message template.
func Render(template string, vars map[string]string) string {
	out := template
	for _, key := range []string{"device", "value", "event"} {
		out = strings.ReplaceAll(out, "{{"+key+"}}", vars[key])
	}
	return out
}
