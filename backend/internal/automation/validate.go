package automation

import (
	"aether/backend/internal/domain"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// EventTypes is every device event a trigger.event block may subscribe to: the seven a rule can use
// plus the informational counterparts (online, *_cleared, motion_stopped).
var EventTypes = []string{
	domain.EventTamper, domain.EventTamperCleared, domain.EventButton, domain.EventLeak, domain.EventLeakCleared,
	domain.EventMotion, domain.EventMotionStopped, domain.EventOffline, domain.EventOnline,
	domain.EventThreshold, domain.EventThresholdClear, domain.EventZone,
	// Door and occupancy (MOS kit). `vacant` is not listed: it is raised by the periodic worker, which
	// does not run automations, so a trigger on it would never fire.
	domain.EventDoorOpen, domain.EventDoorClosed, domain.EventOccupied,
	// Zigbee2MQTT: a switch changing state (from the wall, another controller or a command), a smoke / gas /
	// CO detector going into and out of alarm, and a remote or button press (narrowed by Data.Actions).
	domain.EventSwitchOn, domain.EventSwitchOff, domain.EventHazard, domain.EventHazardCleared, domain.EventAction,
}

// EventTypeAction is the remote / button press event whose value a trigger.event block may filter on.
const EventTypeAction = domain.EventAction

// Problem is one validation failure, addressed to the block (or edge) the studio should highlight.
type Problem struct {
	NodeID  string `json:"node_id,omitempty"`
	EdgeID  string `json:"edge_id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Options carries what the definition alone cannot answer.
type Options struct {
	// Channels is the set of notification channel ids the tenant owns; action.notify may use only these.
	Channels map[string]bool
	// Enabled is true when the caller wants the flow switched on.
	Enabled bool
	// CommandsEnabled is the deployment switch AUTOMATION_COMMANDS. Off (the default), a flow holding an
	// action.command block can be saved as a draft but not enabled.
	CommandsEnabled bool
	// MayCommand says whether the member switching the flow on may control devices themselves (the same rule
	// as a click in the web UI). nil means the caller did not ask, e.g. the runtime re-checking a stored flow.
	MayCommand *bool
	// Commandable maps a registered device id (lower case) to the properties its Zigbee2MQTT definition lets
	// Aether set, for the devices this flow may command (its project, or the workspace for an unscoped flow).
	// nil means the caller did not load them, and the device and property are not checked.
	Commandable map[string]map[string]bool
	// CheckValue validates an action.command value against the device's own definition and returns a short
	// reason ("value", "not_settable", …) or "" when it is acceptable. nil skips the check.
	CheckValue func(deviceID, property string, value json.RawMessage) string
}

var errInvalidDefinition = errors.New("automation definition invalid")

// Validate reduces Check to one error, for callers that only need pass/fail.
func Validate(def Definition, opts Options) error {
	if len(Check(def, opts)) > 0 {
		return errInvalidDefinition
	}
	return nil
}

// Check returns every problem in the definition; an empty slice means the flow is valid.
// It never returns nil so the API always renders a JSON array.
func Check(def Definition, opts Options) []Problem {
	out := []Problem{}
	add := func(node, code, message string) {
		out = append(out, Problem{NodeID: node, Code: code, Message: message})
	}
	addEdge := func(edge, code, message string) {
		out = append(out, Problem{EdgeID: edge, Code: code, Message: message})
	}

	if len(def.Nodes) == 0 {
		add("", "empty", "ผังยังว่างอยู่ · ลากบล็อกจากแถบซ้ายมาวางก่อน")
		return out
	}
	if len(def.Nodes) > MaxNodes {
		add("", "too_many_nodes", fmt.Sprintf("บล็อกได้สูงสุด %d บล็อกต่อหนึ่งผัง", MaxNodes))
		return out
	}
	if len(def.Edges) > MaxEdges {
		add("", "too_many_edges", fmt.Sprintf("เส้นเชื่อมได้สูงสุด %d เส้นต่อหนึ่งผัง", MaxEdges))
		return out
	}

	byID := map[string]Node{}
	for _, n := range def.Nodes {
		if !validID(n.ID) {
			add(n.ID, "bad_id", "รหัสบล็อกไม่ถูกต้อง")
			continue
		}
		if _, dup := byID[n.ID]; dup {
			add(n.ID, "duplicate_id", "รหัสบล็อกซ้ำกัน")
			continue
		}
		if !known(n.Type) {
			add(n.ID, "unknown_type", "ไม่รู้จักบล็อกชนิดนี้")
			continue
		}
		byID[n.ID] = n
		for _, p := range checkData(n, opts) {
			out = append(out, p)
		}
	}
	if len(byID) == 0 {
		return out
	}

	// Edges: endpoints must exist, triggers take no input, actions produce no output, and a block with a
	// single input socket (condition, action) may not be fed by two different edges.
	incoming := map[string][]Edge{}
	outgoing := map[string][]Edge{}
	seenEdge := map[string]bool{}
	for _, e := range def.Edges {
		if !validID(e.ID) || seenEdge[e.ID] {
			addEdge(e.ID, "bad_edge_id", "รหัสเส้นเชื่อมไม่ถูกต้องหรือซ้ำกัน")
			continue
		}
		seenEdge[e.ID] = true
		src, okSrc := byID[e.Source]
		dst, okDst := byID[e.Target]
		if !okSrc || !okDst {
			addEdge(e.ID, "dangling_edge", "เส้นเชื่อมชี้ไปยังบล็อกที่ไม่มีอยู่")
			continue
		}
		if e.Source == e.Target {
			addEdge(e.ID, "self_loop", "บล็อกเชื่อมกลับหาตัวเองไม่ได้")
			continue
		}
		if IsAction(src.Type) {
			addEdge(e.ID, "action_output", "บล็อก “ทำ” เป็นปลายทางเสมอ ต่อออกจากบล็อกนี้ไม่ได้")
			continue
		}
		if IsTrigger(dst.Type) {
			addEdge(e.ID, "trigger_input", "บล็อก “เมื่อ” เป็นจุดเริ่ม ต่อเข้าบล็อกนี้ไม่ได้")
			continue
		}
		if IsCondition(src.Type) {
			if e.SourceHandle != HandleTrue && e.SourceHandle != HandleFalse {
				addEdge(e.ID, "bad_handle", "เส้นจากบล็อก “ถ้า” ต้องออกทางขา ใช่ หรือ ไม่ใช่")
				continue
			}
		} else if e.SourceHandle != "" {
			addEdge(e.ID, "bad_handle", "บล็อกนี้มีทางออกเดียว")
			continue
		}
		incoming[e.Target] = append(incoming[e.Target], e)
		outgoing[e.Source] = append(outgoing[e.Source], e)
	}
	for id, in := range incoming {
		n := byID[id]
		if (IsCondition(n.Type) || IsAction(n.Type)) && len(in) > 1 {
			add(id, "too_many_inputs", "บล็อกนี้รับเข้าได้เส้นเดียว · ใช้บล็อก “ทั้งหมด” หรือ “อย่างน้อยหนึ่ง” เพื่อรวมหลายเส้น")
		}
	}

	// Cycles: a flow is evaluated once per firing, so it must be a DAG.
	if cycle := findCycle(byID, outgoing); cycle != "" {
		add(cycle, "cycle", "ผังวนกลับมาที่บล็อกนี้ · ต้องไม่มีวงวน")
	}

	triggers, actions := 0, 0
	for _, n := range def.Nodes {
		if _, ok := byID[n.ID]; !ok {
			continue
		}
		if IsTrigger(n.Type) {
			triggers++
		}
		if IsAction(n.Type) {
			actions++
		}
	}
	if triggers == 0 {
		add("", "no_trigger", "ต้องมีบล็อก “เมื่อ” อย่างน้อยหนึ่งบล็อก")
	}
	if actions == 0 {
		add("", "no_action", "ต้องมีบล็อก “ทำ” อย่างน้อยหนึ่งบล็อก")
	}

	// Every action must be downstream of some trigger, otherwise it can never run.
	reachable := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if reachable[id] {
			return
		}
		reachable[id] = true
		for _, e := range outgoing[id] {
			walk(e.Target)
		}
	}
	for _, n := range def.Nodes {
		if IsTrigger(n.Type) {
			if _, ok := byID[n.ID]; ok {
				walk(n.ID)
			}
		}
	}
	for _, n := range def.Nodes {
		if _, ok := byID[n.ID]; !ok || !IsAction(n.Type) {
			continue
		}
		if !reachable[n.ID] {
			add(n.ID, "unreachable", "บล็อกนี้ยังไม่ได้ต่อจากบล็อก “เมื่อ” · จะไม่มีวันทำงาน")
		}
	}
	return out
}

// checkData validates one block's settings.
func checkData(n Node, opts Options) []Problem {
	out := []Problem{}
	add := func(code, message string) { out = append(out, Problem{NodeID: n.ID, Code: code, Message: message}) }
	d := n.Data
	if utf8.RuneCountInString(d.Label) > MaxTextLen {
		add("label_too_long", "ชื่อบล็อกยาวเกินไป")
	}
	switch n.Type {
	case TriggerEvent:
		if len(d.EventTypes) == 0 {
			add("no_event_type", "เลือกชนิดเหตุการณ์อย่างน้อยหนึ่งอย่าง")
		}
		if len(d.EventTypes) > MaxSelectors {
			add("too_many_event_types", "เลือกชนิดเหตุการณ์มากเกินไป")
		}
		for _, t := range d.EventTypes {
			if !contains(EventTypes, t) {
				add("unknown_event_type", "ไม่รู้จักเหตุการณ์ "+clip(t))
			}
		}
		out = append(out, checkIDList(n.ID, "external_ids", d.ExternalIDs, validExternal)...)
		out = append(out, checkIDList(n.ID, "gateway_ids", d.GatewayIDs, validUUID)...)
		out = append(out, checkIDList(n.ID, "actions", d.Actions, validAction)...)
		if len(d.Actions) > 0 && !contains(d.EventTypes, EventTypeAction) {
			add("actions_without_action", "เลือกปุ่มที่กดได้เฉพาะเมื่อฟังเหตุการณ์ “กดปุ่ม / รีโมต”")
		}
	case TriggerMetric:
		if !validExternal(d.ExternalID) {
			add("no_device", "เลือกอุปกรณ์ที่จะเฝ้าดู")
		}
		if !validMetric(d.Metric) {
			add("no_metric", "เลือกค่าที่จะเฝ้าดู")
		}
		if !contains(Ops, d.Op) {
			add("bad_op", "เลือกเครื่องหมายเปรียบเทียบ")
		}
		if d.ForSec < 0 || d.ForSec > MaxForSec {
			add("bad_for_sec", fmt.Sprintf("ระยะเวลาต้องอยู่ระหว่าง 0 ถึง %d วินาที", MaxForSec))
		}
	case CondTime:
		if _, ok := parseHHMM(d.From); !ok {
			add("bad_from", "เวลาเริ่มต้องเป็นรูปแบบ HH:MM")
		}
		if _, ok := parseHHMM(d.To); !ok {
			add("bad_to", "เวลาสิ้นสุดต้องเป็นรูปแบบ HH:MM")
		}
		if len(d.Days) > 7 {
			add("bad_days", "เลือกวันซ้ำกัน")
		}
		for _, day := range d.Days {
			if day < 0 || day > 6 {
				add("bad_days", "วันในสัปดาห์ต้องอยู่ระหว่าง 0 ถึง 6")
				break
			}
		}
	case CondDevice:
		if !validExternal(d.ExternalID) {
			add("no_device", "เลือกอุปกรณ์ที่จะตรวจสอบ")
		}
		if !validMetric(d.Metric) {
			add("no_metric", "เลือกค่าที่จะตรวจสอบ")
		}
		if !contains(Ops, d.Op) {
			add("bad_op", "เลือกเครื่องหมายเปรียบเทียบ")
		}
		if d.MaxAgeSec < 0 || d.MaxAgeSec > MaxMaxAgeSec {
			add("bad_max_age", fmt.Sprintf("อายุค่าที่ยอมรับต้องอยู่ระหว่าง 0 ถึง %d วินาที", MaxMaxAgeSec))
		}
	case CondZone:
		if !validExternal(d.ExternalID) {
			add("no_device", "เลือก wearable ที่จะตรวจโซน")
		}
		out = append(out, checkIDList(n.ID, "gateway_ids", d.GatewayIDs, validUUID)...)
	case LogicAll, LogicAny:
		// No settings.
	case ActionAlert:
		if !contains(Severities, d.Severity) {
			add("bad_severity", "เลือกระดับความรุนแรง")
		}
		title := strings.TrimSpace(d.Title)
		if title == "" {
			add("no_title", "ใส่ข้อความหัวเรื่องของการแจ้งเตือน")
		}
		if utf8.RuneCountInString(title) > MaxTextLen {
			add("title_too_long", "หัวเรื่องยาวเกินไป")
		}
	case ActionNotify:
		if len(d.ChannelIDs) == 0 {
			add("no_channel", "เลือกช่องทางแจ้งเตือนอย่างน้อยหนึ่งช่อง")
		}
		if len(d.ChannelIDs) > MaxChannels {
			add("too_many_channels", "เลือกช่องทางมากเกินไป")
		}
		for _, id := range d.ChannelIDs {
			if !validUUID(id) {
				add("bad_channel", "รหัสช่องทางไม่ถูกต้อง")
				continue
			}
			if opts.Channels != nil && !opts.Channels[id] {
				add("unknown_channel", "ช่องทางนี้ไม่มีอยู่ใน workspace แล้ว")
			}
		}
		message := strings.TrimSpace(d.Message)
		if message == "" {
			add("no_message", "ใส่ข้อความที่จะส่ง")
		}
		if utf8.RuneCountInString(message) > MaxTextLen*2 {
			add("message_too_long", "ข้อความยาวเกินไป")
		}
	case ActionCommand:
		out = append(out, checkCommand(n, opts)...)
	}
	return out
}

// checkCommand validates one action.command block: what it targets and, when the flow is being switched on,
// whether this deployment and this member may send commands at all.
func checkCommand(n Node, opts Options) []Problem {
	out := []Problem{}
	add := func(code, message string) { out = append(out, Problem{NodeID: n.ID, Code: code, Message: message}) }
	d := n.Data
	device := strings.ToLower(d.DeviceID)
	okDevice, okProperty := validUUID(device), validProperty(d.Property)
	if !okDevice {
		add("no_device", "เลือกอุปกรณ์ที่จะสั่ง")
	}
	if !okProperty {
		add("no_property", "เลือกค่าที่จะตั้ง")
	}
	okValue := false
	switch raw := strings.TrimSpace(string(d.SetValue)); {
	case raw == "" || raw == "null":
		add("no_value", "ใส่ค่าที่จะตั้ง")
	case len(d.SetValue) > MaxCommandValueBytes:
		add("value_too_long", "ค่าที่จะตั้งยาวเกินไป")
	case !json.Valid(d.SetValue):
		add("bad_value", "ค่าที่จะตั้งไม่ถูกต้อง")
	default:
		okValue = true
	}
	if okDevice && okProperty && opts.Commandable != nil {
		props, known := opts.Commandable[device]
		switch {
		case !known:
			add("unknown_device", "อุปกรณ์นี้สั่งงานจากผังนี้ไม่ได้ · ต้องเป็นอุปกรณ์ Zigbee2MQTT ที่ลงทะเบียนแล้ว และอยู่ในโปรเจกต์เดียวกับผัง")
		case !props[d.Property]:
			add("not_settable", "อุปกรณ์ไม่ให้ตั้งค่านี้")
		case okValue && opts.CheckValue != nil:
			if reason := opts.CheckValue(device, d.Property, d.SetValue); reason != "" {
				add("bad_value", "อุปกรณ์รับค่านี้ไม่ได้ ("+clip(reason)+")")
			}
		}
	}
	if opts.Enabled {
		if !opts.CommandsEnabled {
			add("command_disabled", "ระบบนี้ปิดการสั่งอุปกรณ์จากผังอัตโนมัติไว้ (AUTOMATION_COMMANDS) · บันทึกเป็นฉบับร่างได้ แต่ยังเปิดใช้ไม่ได้")
		} else if opts.MayCommand != nil && !*opts.MayCommand {
			add("command_forbidden", "บัญชีนี้ไม่มีสิทธิ์สั่งงานอุปกรณ์ · จึงเปิดใช้ผังที่มีบล็อกนี้ไม่ได้")
		}
	}
	return out
}

// HasCommand reports whether a flow contains an action.command block, i.e. whether enabling it actuates devices.
func HasCommand(def Definition) bool {
	for _, n := range def.Nodes {
		if n.Type == ActionCommand {
			return true
		}
	}
	return false
}

func checkIDList(node, field string, list []string, ok func(string) bool) []Problem {
	out := []Problem{}
	if len(list) > MaxSelectors {
		return append(out, Problem{NodeID: node, Code: "too_many_" + field, Message: "เลือกรายการมากเกินไป"})
	}
	for _, v := range list {
		if !ok(v) {
			return append(out, Problem{NodeID: node, Code: "bad_" + field, Message: "รายการที่เลือกไม่ถูกต้อง: " + clip(v)})
		}
	}
	return out
}

// findCycle returns the id of a block that takes part in a cycle, or "".
func findCycle(byID map[string]Node, outgoing map[string][]Edge) string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := map[string]int{}
	var visit func(string) string
	visit = func(id string) string {
		switch colour[id] {
		case grey:
			return id
		case black:
			return ""
		}
		colour[id] = grey
		for _, e := range outgoing[id] {
			if hit := visit(e.Target); hit != "" {
				return hit
			}
		}
		colour[id] = black
		return ""
	}
	// Deterministic order so the same definition always reports the same block.
	for _, n := range sortedIDs(byID) {
		if hit := visit(n); hit != "" {
			return hit
		}
	}
	return ""
}

func sortedIDs(byID map[string]Node) []string {
	out := make([]string, 0, len(byID))
	for id := range byID {
		out = append(out, id)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func validID(s string) bool {
	if s == "" || len(s) > MaxIDLen {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' && r != ':' {
			return false
		}
	}
	return true
}

// validExternal accepts the lowercase hex identity Aether uses for BLE devices (a MAC, or a synthetic id).
func validExternal(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' && r != ':' && r != '.' {
			return false
		}
	}
	return true
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// validProperty is the Zigbee2MQTT property name a command may set (the same pattern the command queue enforces).
func validProperty(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}

// validAction is a remote / button action value such as "single", "brightness_move_up" or "1_single".
func validAction(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func validMetric(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= 40 {
		return s
	}
	return string([]rune(s)[:40]) + "…"
}
