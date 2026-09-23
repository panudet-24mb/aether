// Package alerts turns decoded readings into edge-triggered device events, matches them against
// tenant rules and formats notifications. It is pure Go with no database access so it can be unit-tested.
package alerts

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// State is the last known flag state of one stream (persisted in core.stream_state).
type State struct {
	Known    bool
	Tamper   int
	Leak     int
	Moving   int
	Instance string
	Offline  bool
	Breaches []string // ids of threshold rules currently breached
	// Door is the last known `door` metric (1 = open).
	Door int
	// Occupied is 1 from an `occupied` event until the periodic scan raises `vacant`; LastMotion is the server
	// time of the last uplink whose PIR reported motion=1 (zero when never).
	Occupied   int
	LastMotion time.Time
	// TriggerAt is the server time the button-trigger slot (iBeacon) was last heard; zero when never.
	TriggerAt time.Time
}

// ButtonTriggerQuiet is how long a button tag's trigger slot must have been silent for its reappearance to
// count as a new press. The physical B10 advertises iBeacon only after a press, in a burst that lasts over
// a minute with gaps of a few seconds when the gateway misses a packet; at rest it is silent for minutes.
// 30 s sits well clear of both, and a second press inside a running burst is the same emergency anyway.
const ButtonTriggerQuiet = 30 * time.Second

// OccupancyHoldSec is how long a PIR must report no motion before the room counts as vacant. PIR sensors
// only report motion while something moves, so a person sitting still at a desk goes quiet for minutes.
const OccupancyHoldSec = 300

// Vacant reports whether an occupied episode is over at `now`.
func Vacant(s State, now time.Time) bool {
	return s.Occupied == 1 && !s.LastMotion.IsZero() && now.Sub(s.LastMotion) >= OccupancyHoldSec*time.Second
}

// Detect compares a fresh reading with the previous state and returns the events that happened.
// Threshold rules are evaluated here too, so a breach becomes one event, not one per uplink.
func Detect(prev State, r minew.Reading, rules []domain.AlertRule, at time.Time) ([]domain.DeviceEvent, State) {
	next := State{Known: true, Tamper: prev.Tamper, Leak: prev.Leak, Moving: prev.Moving, Instance: prev.Instance, Breaches: append([]string(nil), prev.Breaches...), Door: prev.Door, Occupied: prev.Occupied, LastMotion: prev.LastMotion, TriggerAt: prev.TriggerAt}
	var out []domain.DeviceEvent
	emit := func(t string, detail map[string]any) {
		out = append(out, domain.DeviceEvent{EventType: t, Detail: detail, OccurredAt: at})
	}
	m := r.Metrics
	if prev.Offline {
		emit(domain.EventOnline, map[string]any{})
	}
	if v, ok := m["tamper"]; ok {
		now := int(v)
		if now == 1 && (prev.Tamper == 0 || !prev.Known) {
			emit(domain.EventTamper, map[string]any{"frame": minew.FrameTamper})
		} else if now == 0 && prev.Known && prev.Tamper == 1 {
			emit(domain.EventTamperCleared, map[string]any{})
		}
		next.Tamper = now
	}
	if v, ok := m["leak"]; ok {
		now := int(v)
		if now == 1 && (prev.Leak == 0 || !prev.Known) {
			emit(domain.EventLeak, map[string]any{"frame": minew.FrameLeak})
		} else if now == 0 && prev.Known && prev.Leak == 1 {
			emit(domain.EventLeakCleared, map[string]any{})
		}
		next.Leak = now
	}
	if v, ok := m["door"]; ok {
		now := int(v)
		if now == 1 && (prev.Door == 0 || !prev.Known) {
			emit(domain.EventDoorOpen, map[string]any{})
		} else if now == 0 && prev.Known && prev.Door == 1 {
			emit(domain.EventDoorClosed, map[string]any{})
		}
		next.Door = now
	}
	// Occupancy follows the PIR `motion` metric only (A1-11): accelerometer vibration says a tag moved,
	// not that a person is in the room. The episode ends in the periodic scan (see Vacant), never here.
	if m["motion"] == 1 {
		next.LastMotion = at
		if prev.Occupied == 0 {
			emit(domain.EventOccupied, map[string]any{"hold_sec": OccupancyHoldSec})
			next.Occupied = 1
		}
	}
	if _, hasVib := m["vibration"]; hasVib || m["motion"] == 1 || hasKey(m, "motion") {
		now := 0
		if m["vibration"] == 1 || m["motion"] == 1 {
			now = 1
		}
		if now == 1 && (prev.Moving == 0 || !prev.Known) {
			emit(domain.EventMotion, map[string]any{"accel_g": m["accel_g"]})
		} else if now == 0 && prev.Known && prev.Moving == 1 {
			emit(domain.EventMotionStopped, map[string]any{})
		}
		next.Moving = now
	}
	if r.Beacon != nil && r.Beacon.Instance != "" {
		if prev.Known && prev.Instance != "" && prev.Instance != r.Beacon.Instance {
			emit(domain.EventButton, map[string]any{"from": prev.Instance, "to": r.Beacon.Instance, "namespace": r.Beacon.Namespace, "note": "Eddystone-UID instance changed; press semantics unverified on hardware"})
		}
		next.Instance = r.Beacon.Instance
	}
	// Button trigger slot: the B10 advertises iBeacon only after its button is pressed. The slot coming back
	// after ButtonTriggerQuiet of silence is one press. Every tag with a regular iBeacon slot produces this
	// event too; the caller keeps it only for devices registered with a button profile.
	if slices.Contains(r.Frames, minew.FrameIBeacon) {
		if prev.TriggerAt.IsZero() || at.Sub(prev.TriggerAt) >= ButtonTriggerQuiet {
			detail := map[string]any{"trigger": minew.FrameIBeacon}
			if !prev.TriggerAt.IsZero() {
				detail["silent_sec"] = int(at.Sub(prev.TriggerAt).Seconds())
			}
			emit(domain.EventButton, detail)
		}
		next.TriggerAt = at
	}
	// Threshold rules: rising edge per rule id.
	breached := map[string]bool{}
	for _, id := range prev.Breaches {
		breached[id] = true
	}
	var nextBreaches []string
	for _, rule := range rules {
		if !rule.Enabled || rule.EventType != domain.EventThreshold || rule.Scope.Value == nil || rule.Scope.Metric == "" {
			continue
		}
		value, ok := metricValue(r, rule.Scope.Metric)
		if !ok {
			if breached[rule.ID] {
				nextBreaches = append(nextBreaches, rule.ID) // unknown this uplink: keep state, no event
			}
			continue
		}
		hit := compare(value, rule.Scope.Op, *rule.Scope.Value)
		if hit {
			nextBreaches = append(nextBreaches, rule.ID)
			if !breached[rule.ID] {
				emit(domain.EventThreshold, map[string]any{"rule_id": rule.ID, "metric": rule.Scope.Metric, "value": value, "op": rule.Scope.Op, "threshold": *rule.Scope.Value})
			}
		} else if breached[rule.ID] {
			emit(domain.EventThresholdClear, map[string]any{"rule_id": rule.ID, "metric": rule.Scope.Metric, "value": value})
		}
	}
	sort.Strings(nextBreaches)
	next.Breaches = nextBreaches
	return out, next
}

func hasKey(m map[string]float64, k string) bool { _, ok := m[k]; return ok }

func metricValue(r minew.Reading, metric string) (float64, bool) {
	switch metric {
	case "temperature":
		return r.Temperature, r.Kind == minew.KindEnvironment || hasFrame(r.Frames, minew.FrameTH) || hasFrame(r.Frames, minew.FrameTemp)
	case "humidity":
		return r.Humidity, hasFrame(r.Frames, minew.FrameTH)
	case "battery":
		return float64(r.Battery), r.Battery > 0
	case "rssi":
		if r.RSSI == nil {
			return 0, false
		}
		return float64(*r.RSSI), true
	}
	v, ok := r.Metrics[metric]
	return v, ok
}

func hasFrame(frames []string, id string) bool {
	for _, f := range frames {
		if f == id {
			return true
		}
	}
	return false
}

func compare(v float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return v > threshold
	case ">=":
		return v >= threshold
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	}
	return false
}

// ruleEvent is the device event a rule event type subscribes to.
func ruleEvent(ruleType string) string {
	switch ruleType {
	case domain.RuleDoor:
		return domain.EventDoorOpen
	case domain.RuleOccupancy:
		return domain.EventOccupied
	}
	return ruleType
}

// Matches reports whether an enabled rule subscribes to this event for this device.
func Matches(rule domain.AlertRule, ev domain.DeviceEvent) bool {
	if !rule.Enabled || ruleEvent(rule.EventType) != ev.EventType {
		return false
	}
	if rule.Scope.AfterHours != nil && !InWindow(*rule.Scope.AfterHours, ev.OccurredAt) {
		return false
	}
	if rule.EventType == domain.EventThreshold {
		if id, _ := ev.Detail["rule_id"].(string); id != rule.ID {
			return false
		}
	}
	if rule.EventType == domain.EventZone && len(rule.Scope.GatewayIDs) > 0 {
		to, _ := ev.Detail["to_gateway_id"].(string)
		if !contains(rule.Scope.GatewayIDs, to) {
			return false
		}
	}
	if len(rule.Scope.ExternalIDs) == 0 {
		return true
	}
	for _, id := range rule.Scope.ExternalIDs {
		if strings.EqualFold(id, ev.ExternalID) {
			return true
		}
	}
	return false
}

// ValidateRule enforces the same limits the database does plus scope sanity.
func ValidateRule(r domain.AlertRule) error {
	if strings.TrimSpace(r.Name) == "" || len(r.Name) > 128 || !contains(domain.RuleEventTypes, r.EventType) || !contains(domain.Severities, r.Severity) || r.DedupeSec < 0 || r.DedupeSec > 86400 || len(r.Channels) > 20 || len(r.Scope.ExternalIDs) > 200 {
		return domain.ErrInvalid
	}
	for _, id := range r.Scope.ExternalIDs {
		if len(id) == 0 || len(id) > 128 {
			return domain.ErrInvalid
		}
	}
	if len(r.Scope.GatewayIDs) > 50 || (len(r.Scope.GatewayIDs) > 0 && r.EventType != domain.EventZone) {
		return domain.ErrInvalid
	}
	for _, id := range r.Scope.GatewayIDs {
		if len(id) != 36 {
			return domain.ErrInvalid
		}
	}
	if r.Scope.AfterHours != nil {
		if r.EventType != domain.RuleDoor && r.EventType != domain.RuleOccupancy || !validWindow(*r.Scope.AfterHours) {
			return domain.ErrInvalid
		}
	}
	switch r.EventType {
	case domain.EventOffline:
		if r.Scope.OfflineAfterSec != 0 && (r.Scope.OfflineAfterSec < 60 || r.Scope.OfflineAfterSec > 86400) {
			return domain.ErrInvalid
		}
	case domain.EventThreshold:
		if r.Scope.Metric == "" || len(r.Scope.Metric) > 32 || r.Scope.Value == nil || !contains([]string{">", ">=", "<", "<="}, r.Scope.Op) {
			return domain.ErrInvalid
		}
	}
	return nil
}

// parseHM reads "HH:MM" (24-hour) into minutes after midnight.
func parseHM(s string) (int, bool) {
	t, e := time.Parse("15:04", s)
	if e != nil || len(s) != 5 {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func validWindow(w domain.AfterHours) bool {
	_, okFrom := parseHM(w.From)
	_, okTo := parseHM(w.To)
	if !okFrom || !okTo || len(w.Days) > 7 {
		return false
	}
	seen := map[int]bool{}
	for _, d := range w.Days {
		if d < 0 || d > 6 || seen[d] {
			return false
		}
		seen[d] = true
	}
	return true
}

// InWindow reports whether `at` falls inside the after-hours window, in Asia/Bangkok. A window that
// crosses midnight (from > to) belongs to the day it starts on: {from:"18:00", to:"07:00", days:[5]}
// covers Friday 18:00 until Saturday 07:00. from == to covers the whole of each listed day.
func InWindow(w domain.AfterHours, at time.Time) bool {
	from, okFrom := parseHM(w.From)
	to, okTo := parseHM(w.To)
	if !okFrom || !okTo {
		return false
	}
	local := at.In(bangkok())
	minute := local.Hour()*60 + local.Minute()
	day := int(local.Weekday())
	onDay := func(d int) bool {
		if len(w.Days) == 0 {
			return true
		}
		for _, x := range w.Days {
			if x == d {
				return true
			}
		}
		return false
	}
	switch {
	case from == to:
		return onDay(day)
	case from < to:
		return onDay(day) && minute >= from && minute < to
	}
	// Crosses midnight: the evening part belongs to today, the early-morning part to yesterday.
	return minute >= from && onDay(day) || minute < to && onDay((day+6)%7)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

var eventLabel = map[string]string{
	domain.EventTamper: "ป้ายถูกถอด / tamper", domain.EventTamperCleared: "tamper กลับสู่ปกติ",
	domain.EventButton: "กดปุ่ม SOS", domain.EventLeak: "พบน้ำรั่ว", domain.EventLeakCleared: "น้ำรั่วหาย",
	domain.EventMotion: "เริ่มเคลื่อนไหว", domain.EventMotionStopped: "หยุดเคลื่อนไหว",
	domain.EventOffline: "ขาดการติดต่อ (offline)", domain.EventOnline: "กลับมาออนไลน์",
	domain.EventThreshold: "ค่าเกินเกณฑ์", domain.EventThresholdClear: "ค่ากลับเข้าเกณฑ์",
	domain.EventDoorOpen: "ประตูเปิด", domain.EventDoorClosed: "ประตูปิด",
	domain.EventOccupied: "มีคนในพื้นที่", domain.EventVacant: "ไม่มีคนในพื้นที่",
}

func Label(eventType string) string {
	if l, ok := eventLabel[eventType]; ok {
		return l
	}
	return eventType
}

// Title is the short alert headline stored with the alert.
func Title(ev domain.DeviceEvent) string {
	switch ev.EventType {
	case domain.EventThreshold:
		return fmt.Sprintf("%s · %s %v %v (%v)", ev.DeviceName, ev.Detail["metric"], ev.Detail["op"], ev.Detail["threshold"], ev.Detail["value"])
	case domain.EventOffline:
		return fmt.Sprintf("%s · ขาดการติดต่อ", ev.DeviceName)
	case domain.EventZone:
		return fmt.Sprintf("%s · เข้าโซน %v", ev.DeviceName, ev.Detail["to"])
	case domain.EventButton:
		// A press is an emergency signal, not a status change: the headline says so wherever the alert
		// is read (banner, LINE, email, webhook). domain.Alert.SOS decides how loudly the UI reacts.
		return fmt.Sprintf("SOS · %s กดปุ่มฉุกเฉิน", ev.DeviceName)
	}
	return fmt.Sprintf("%s · %s", ev.DeviceName, Label(ev.EventType))
}

// Message is the human-readable notification body (used for LINE, email and the webhook "text" field).
func Message(a domain.Alert, ev domain.DeviceEvent, gatewayName string) string {
	icon := map[string]string{"critical": "🔴", "warning": "🟠", "info": "🔵"}[a.Severity]
	lines := []string{fmt.Sprintf("%s [Aether] %s · %s", icon, strings.ToUpper(a.Severity), a.Title), fmt.Sprintf("อุปกรณ์ %s (%s) ผ่าน gateway %s", ev.DeviceName, ev.ExternalID, gatewayName), fmt.Sprintf("เวลา %s", ev.OccurredAt.In(bangkok()).Format("2006-01-02 15:04:05 MST"))}
	if note, _ := ev.Detail["note"].(string); note != "" {
		lines = append(lines, note)
	}
	if a.Note != nil && *a.Note != "" {
		lines = append(lines, *a.Note)
	}
	return strings.Join(lines, "\n")
}

func bangkok() *time.Location {
	if loc, e := time.LoadLocation("Asia/Bangkok"); e == nil {
		return loc
	}
	return time.FixedZone("ICT", 7*3600)
}
