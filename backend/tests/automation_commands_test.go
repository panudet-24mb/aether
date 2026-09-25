package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// automationRig is a commandRig (Zigbee2MQTT gateway, FakeBridge, real dispatcher, registered switch, bulb,
// curtain and lock) plus a registered door contact and remote on the same gateway to trigger flows from.
type automationRig struct {
	*commandRig
	door, remote simulation.Z2MActuator
	base         string
}

func newAutomationRig(t *testing.T, commands bool) *automationRig {
	t.Helper()
	r := &automationRig{commandRig: newCommandRig(t, nil), door: sensor("MCCGQ11LM"), remote: sensor("E1743")}
	r.f.repo.Configure(postgres.Options{AutomationCommands: commands, DiscoveryLimit: 100})
	r.base = zigbee2mqtt.BaseTopic(r.gateway)
	for _, a := range []simulation.Z2MActuator{r.door, r.remote} {
		if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, a.Name, a.IEEE, domain.Z2MGenericProfile); e != nil {
			t.Fatalf("register %s: %v", a.Model, e)
		}
	}
	// A first report of the door so the next change is an edge, not a baseline.
	r.capture(t, r.bridge.Set(r.door.IEEE, "contact", true))
	return r
}

// create posts a flow and returns the status and body.
func (r *automationRig) create(token string, def map[string]any, enabled bool, project *string) (int, map[string]any) {
	r.t.Helper()
	body := map[string]any{"name": "flow-" + uuid.NewString()[:8], "enabled": enabled, "definition": def}
	if project != nil {
		body["project_id"] = *project
	}
	code, out, _ := req(r.t, r.api, "POST", "/api/v1/automations", "Bearer "+token, "", "", body)
	return code, out
}

// commandFlow is "when <events> of <external> -> set <property> of <device> to <value>".
func commandFlow(events []string, external, device, property string, value any, actions ...string) map[string]any {
	trigger := map[string]any{"event_types": events, "external_ids": []string{external}}
	if len(actions) > 0 {
		trigger["actions"] = actions
	}
	raw, _ := json.Marshal(value)
	return flow([]map[string]any{
		block("t1", "trigger.event", 0, 0, trigger),
		block("k1", "action.command", 320, 0, map[string]any{"device_id": device, "property": property, "set_value": json.RawMessage(raw)}),
	}, []map[string]any{link("e1", "t1", "k1")})
}

func (r *automationRig) automationCommands(flow string) int {
	return r.count(r.t, `SELECT count(*) FROM core.device_commands WHERE source='automation' AND automation_id=$1`, flow)
}

// requests counts a flow's outbox rows in one status ("" = any).
func (r *automationRig) requests(flow, status string) int {
	if status == "" {
		return r.count(r.t, `SELECT count(*) FROM core.automation_command_requests WHERE automation_id=$1`, flow)
	}
	return r.count(r.t, `SELECT count(*) FROM core.automation_command_requests WHERE automation_id=$1 AND status=$2`, flow, status)
}

// requestReason is the reason of the flow's latest outbox row.
func (r *automationRig) requestReason(flow string) string {
	r.t.Helper()
	var reason string
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT status||':'||reason FROM core.automation_command_requests WHERE automation_id=$1 ORDER BY created_at DESC LIMIT 1`, flow).Scan(&reason); e != nil {
		r.t.Fatal(e)
	}
	return reason
}

// runReason is the command outcome of the flow's latest fired run, as written by ingest.
func (r *automationRig) runReason(flow string) string {
	r.t.Helper()
	var reason string
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT coalesce(detail->'commands'->0->>'status','')||':'||coalesce(detail->'commands'->0->>'reason','') FROM core.automation_runs WHERE automation_id=$1 AND status='fired' ORDER BY created_at DESC LIMIT 1`, flow).Scan(&reason); e != nil {
		r.t.Fatal(e)
	}
	return reason
}

// member adds a member with a role and projects and returns their id and token.
func (r *automationRig) member(role string, projects []string) (string, string) {
	r.t.Helper()
	email := memberEmail()
	id := addMember(r.t, r.api, r.ownerAuth.AccessToken, email, role, projects)
	auth, _ := changeInitialPassword(r.t, r.f, r.api, email, r.owner.TenantID)
	return id, auth.AccessToken
}

func (r *automationRig) enabled(flow string) bool {
	r.t.Helper()
	var on bool
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT enabled FROM core.automations WHERE id=$1`, flow).Scan(&on); e != nil {
		r.t.Fatal(e)
	}
	return on
}

func (r *automationRig) openDoor() {
	r.capture(r.t, r.bridge.Set(r.door.IEEE, "contact", false))
}

func (r *automationRig) closeDoor() {
	r.capture(r.t, r.bridge.Set(r.door.IEEE, "contact", true))
}

// Who may switch on a flow that commands devices, and what it may target.
func TestAutomationCommandEnableRules(t *testing.T) {
	off := newAutomationRig(t, false)
	light := off.devices["light"]
	def := commandFlow([]string{"door_open"}, off.door.IEEE, light, "state", "ON")
	// The deployment switch is off: a draft is fine, enabling is refused with the reason.
	if code, out := off.create(off.ownerAuth.AccessToken, def, false, nil); code != 201 {
		t.Fatalf("draft with the switch off: %d %v", code, out)
	}
	if code, out := off.create(off.ownerAuth.AccessToken, def, true, nil); code != 400 || !problemCodes(out)["command_disabled"] {
		t.Fatalf("enable with AUTOMATION_COMMANDS off: %d %v", code, out)
	}
	if code, out, _ := req(t, off.api, "GET", "/api/v1/automations", "Bearer "+off.ownerAuth.AccessToken, "", "", nil); code != 200 ||
		out["catalog"].(map[string]any)["can_command"] != true {
		t.Fatalf("catalogue: %d %v", code, out)
	}

	r := newAutomationRig(t, true)
	light = r.devices["light"]
	def = commandFlow([]string{"door_open"}, r.door.IEEE, light, "state", "ON")
	// Members: an admin whose control module is closed, and a viewer.
	type member struct{ token string }
	members := map[string]member{}
	for _, m := range []struct{ name, role, control string }{{"adminNoControl", "admin", "none"}, {"viewer", "viewer", ""}} {
		email := memberEmail()
		addMember(t, r.api, r.ownerAuth.AccessToken, email, m.role, []string{})
		auth, _ := changeInitialPassword(t, r.f, r.api, email, r.owner.TenantID)
		members[m.name] = member{auth.AccessToken}
		if m.control != "" {
			id := memberID(t, r.api, r.ownerAuth.AccessToken, email)
			if code, _, _ := req(t, r.api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+r.ownerAuth.AccessToken, "", "", map[string]any{"control": m.control}); code != 204 {
				t.Fatalf("access: %d", code)
			}
		}
	}
	if code, out := r.create(members["viewer"].token, def, true, nil); code != 403 {
		t.Fatalf("viewer: %d %v", code, out)
	}
	if code, out := r.create(members["adminNoControl"].token, def, true, nil); code != 400 || !problemCodes(out)["command_forbidden"] {
		t.Fatalf("admin with control none: %d %v", code, out)
	}
	// The same admin may still save a draft, but not switch it on through /enable either.
	code, out := r.create(members["adminNoControl"].token, def, false, nil)
	if code != 201 {
		t.Fatalf("draft by admin without control: %d %v", code, out)
	}
	draft := out["id"].(string)
	if code, out, _ := req(t, r.api, "POST", "/api/v1/automations/"+draft+"/enable", "Bearer "+members["adminNoControl"].token, "", "", map[string]any{"enabled": true}); code != 400 || !problemCodes(out)["command_forbidden"] {
		t.Fatalf("enable by admin without control: %d %v", code, out)
	}
	if code, out := r.create(r.ownerAuth.AccessToken, def, true, nil); code != 201 || out["enabled"] != true {
		t.Fatalf("owner with the switch on: %d %v", code, out)
	}
	// Targets: a property the device does not let us set, a value it refuses, a device that is not an actuator,
	// and a device outside the flow's project.
	for name, bad := range map[string]struct {
		def  map[string]any
		code string
	}{
		"not settable":   {commandFlow([]string{"door_open"}, r.door.IEEE, light, "linkquality", 10), "not_settable"},
		"bad value":      {commandFlow([]string{"door_open"}, r.door.IEEE, r.devices["light"], "brightness", 9999), "bad_value"},
		"toggle refused": {commandFlow([]string{"door_open"}, r.door.IEEE, r.devices["switch"], "state_left", "TOGGLE"), "bad_value"},
		"unknown device": {commandFlow([]string{"door_open"}, r.door.IEEE, uuid.NewString(), "state", "ON"), "unknown_device"},
	} {
		if code, out := r.create(r.ownerAuth.AccessToken, bad.def, true, nil); code != 400 || !problemCodes(out)[bad.code] {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}
	project, e := r.f.service.CreateProject(r.ctx, r.owner, "Elsewhere", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	if code, out := r.create(r.ownerAuth.AccessToken, commandFlow([]string{"door_open"}, r.door.IEEE, light, "state", "ON"), true, &project.ID); code != 400 {
		t.Fatalf("cross-project target: %d %v", code, out)
	}
	// The studio's picker lists the actuators with what each lets Aether set, never the door.
	code, out, _ = req(t, r.api, "GET", "/api/v1/automations/commandable", "Bearer "+r.ownerAuth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("commandable: %d %v", code, out)
	}
	names := map[string]bool{}
	for _, raw := range out["items"].([]any) {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	if !names["light"] || !names["switch"] || names[r.door.Name] {
		t.Fatalf("commandable devices: %v", names)
	}
	if code, out, _ := req(t, r.api, "GET", "/api/v1/automations/commandable?project_id="+project.ID, "Bearer "+r.ownerAuth.AccessToken, "", "", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("commandable in another project: %d %v", code, out)
	}
	if n := r.count(t, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action='automation.commands_armed'`, r.owner.TenantID); n != 1 {
		t.Fatalf("arming audit rows: %d", n)
	}
}

// A door opening queues exactly one command for the bulb, attributed to the flow and the member who armed it;
// the commander delivers it and the bulb confirms. A second opening inside a minute is the same firing.
func TestAutomationCommandFires(t *testing.T) {
	r := newAutomationRig(t, true)
	code, out := r.create(r.ownerAuth.AccessToken, commandFlow([]string{"door_open"}, r.door.IEEE, r.devices["light"], "brightness", 200), true, nil)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	flowID := out["id"].(string)

	// Dry run first: it says what it would send and sends nothing.
	code, out, _ = req(t, r.api, "POST", "/api/v1/automations/"+flowID+"/test", "Bearer "+r.ownerAuth.AccessToken, "", "", map[string]any{"external_id": r.door.IEEE, "event_type": "door_open"})
	if code != 200 || out["executed"] != false || len(out["would_send"].([]any)) != 1 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if n := r.automationCommands(flowID); n != 0 {
		t.Fatalf("a dry run queued %d commands", n)
	}

	r.openDoor()
	// Ingest only writes the request; mqtt-commander turns it into a command.
	if n, q := r.automationCommands(flowID), r.requests(flowID, "requested"); n != 0 || q != 1 {
		t.Fatalf("door open: %d commands, %d requests", n, q)
	}
	if n, e := r.f.repo.ProcessAutomationRequests(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil || n != 1 {
		t.Fatalf("drain: %d %v", n, e)
	}
	var actor, status string
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT coalesce(actor_id::text,''),status FROM core.device_commands WHERE automation_id=$1`, flowID).Scan(&actor, &status); e != nil || actor != r.owner.UserID || status != "pending" {
		t.Fatalf("command row: actor %q status %q %v", actor, status, e)
	}
	if n := r.dispatch(t); n != 1 {
		t.Fatalf("dispatched %d", n)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_commands WHERE automation_id=$1 AND status='confirmed'`, flowID); n != 1 {
		t.Fatal("the bulb did not confirm the automation's command")
	}
	// Close and open again within the minute: the flow's 60 s de-duplication holds.
	r.closeDoor()
	r.openDoor()
	if n := r.automationCommands(flowID); n != 1 {
		t.Fatalf("second opening inside a minute queued %d commands in total", n)
	}
	// The run history resolves the request to what the commander did with it.
	code, out, _ = req(t, r.api, "GET", "/api/v1/automations/"+flowID+"/runs", "Bearer "+r.ownerAuth.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("runs: %d %v", code, out)
	}
	var queued int
	for _, raw := range out["items"].([]any) {
		detail := raw.(map[string]any)["detail"].(map[string]any)
		for _, c := range asList(detail["commands"]) {
			if c.(map[string]any)["status"] == "queued" && c.(map[string]any)["command_id"] != "" {
				queued++
			}
		}
	}
	if queued != 1 {
		t.Fatalf("run history: %v", out)
	}
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

// Remote presses narrowed by action value; only the listed press fires.
func TestAutomationCommandActionFilter(t *testing.T) {
	r := newAutomationRig(t, true)
	code, out := r.create(r.ownerAuth.AccessToken, commandFlow([]string{"action"}, r.remote.IEEE, r.devices["light"], "state", "ON", "on"), true, nil)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	flowID := out["id"].(string)
	r.capture(t, []simulation.Z2MMessage{simulation.Z2MPress(r.base, r.remote, "brightness_move_up")})
	if n := r.requests(flowID, ""); n != 0 {
		t.Fatalf("an unlisted press requested %d commands", n)
	}
	r.capture(t, []simulation.Z2MMessage{simulation.Z2MPress(r.base, r.remote, "on")})
	if n := r.requests(flowID, ""); n != 1 {
		t.Fatalf("the listed press requested %d commands", n)
	}
}

// Two flows that undo each other's work: the first reacts to a change on the wall, its command's effect wakes the
// second, and the loop guard stops the second from sending anything.
func TestAutomationCommandLoopGuard(t *testing.T) {
	r := newAutomationRig(t, true)
	sw := simulation.Z2MSwitches[1] // TS0012, properties state_right / state_left
	device := r.devices["switch"]
	offOnOn := commandFlow([]string{"switch_on"}, sw.IEEE, device, "state_right", "OFF")
	onOnOff := commandFlow([]string{"switch_off"}, sw.IEEE, device, "state_right", "ON")
	codeA, a := r.create(r.ownerAuth.AccessToken, offOnOn, true, nil)
	codeB, b := r.create(r.ownerAuth.AccessToken, onOnOff, true, nil)
	if codeA != 201 || codeB != 201 {
		t.Fatalf("create: %d %v / %d %v", codeA, a, codeB, b)
	}
	flowA, flowB := a["id"].(string), b["id"].(string)

	// Someone flips the right gang on the wall (the opposite of what the bridge currently has).
	state := append([]bool{}, simulation.Z2MStep(0)[1]...)
	state[0] = !state[0]
	r.capture(t, []simulation.Z2MMessage{{Topic: simulation.Z2MTopic(r.base, sw, ""), Payload: simulation.Z2MState(sw, state, 110)}})
	first, second := flowA, flowB
	if !state[0] {
		first, second = flowB, flowA
	}
	if n := r.requests(first, ""); n != 1 {
		t.Fatalf("the flow woken by the wall requested %d", n)
	}
	// Deliver it: the switch reports the commanded value, which is the other flow's trigger.
	if n := r.dispatch(t); n != 1 {
		t.Fatalf("dispatched %d", n)
	}
	if n := r.requests(second, ""); n != 0 {
		t.Fatalf("the loop guard let the second flow request %d", n)
	}
	if got := r.runReason(second); got != "blocked:loop_guard" {
		t.Fatalf("second flow outcome: %s", got)
	}
}

// The same ping-pong through metric triggers on a property that is not a switch gang: brightness rising above 150
// sets it to 50, brightness falling below 100 sets it to 200. The guard, not the per-device cap, stops it.
func TestAutomationCommandLoopGuardOnMetrics(t *testing.T) {
	r := newAutomationRig(t, true)
	light, bulb := r.devices["light"], simulation.Z2MActuators[0]
	metricFlow := func(op string, threshold float64, value int) map[string]any {
		return flow([]map[string]any{
			block("t1", "trigger.metric", 0, 0, map[string]any{"external_id": bulb.IEEE, "metric": "brightness", "op": op, "value": threshold, "for_sec": 0}),
			block("k1", "action.command", 320, 0, map[string]any{"device_id": light, "property": "brightness", "set_value": value}),
		}, []map[string]any{link("e1", "t1", "k1")})
	}
	codeA, a := r.create(r.ownerAuth.AccessToken, metricFlow(">", 150, 50), true, nil)
	codeB, b := r.create(r.ownerAuth.AccessToken, metricFlow("<", 100, 200), true, nil)
	if codeA != 201 || codeB != 201 {
		t.Fatalf("create: %d %v / %d %v", codeA, a, codeB, b)
	}
	down, up := a["id"].(string), b["id"].(string)
	// A person turns the bulb up from the wall app: the "too bright" flow asks for 50.
	r.capture(t, r.bridge.Set(bulb.IEEE, "brightness", 200))
	if n := r.requests(down, ""); n != 1 {
		t.Fatalf("too-bright flow requested %d", n)
	}
	// The commander sends it, the bulb reports 50: the "too dim" flow is woken by the automation's own effect.
	if n := r.dispatch(t); n != 1 {
		t.Fatalf("dispatched %d", n)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_commands WHERE automation_id=$1 AND status='confirmed'`, down); n != 1 {
		t.Fatal("the bulb did not confirm 50")
	}
	if got := r.runReason(up); got != "blocked:loop_guard" {
		t.Fatalf("too-dim flow outcome: %s", got)
	}
	if n := r.requests(up, ""); n != 0 {
		t.Fatalf("too-dim flow requested %d", n)
	}
}

// All automations together may send one device at most domain.AutomationCommandsPerDevice commands a minute.
func TestAutomationCommandPerDeviceCap(t *testing.T) {
	r := newAutomationRig(t, true)
	light := r.devices["light"]
	code, out := r.create(r.ownerAuth.AccessToken, commandFlow([]string{"door_open"}, r.door.IEEE, light, "brightness", 50), true, nil)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	flowID := out["id"].(string)
	for i := 0; i < domain.AutomationCommandsPerDevice; i++ {
		if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.device_commands(tenant_id,id,gateway_id,device_id,ieee,property,requested,value,source,status,created_at,expires_at)
      VALUES($1,$2,$3,$4,$5,'brightness','set','10','automation','confirmed',now(),now()+interval '10 seconds')`,
			r.owner.TenantID, uuid.NewString(), r.gateway, light, simulation.Z2MActuators[0].IEEE); e != nil {
			t.Fatal(e)
		}
	}
	r.openDoor()
	if _, e := r.f.repo.ProcessAutomationRequests(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	if n := r.automationCommands(flowID); n != 0 {
		t.Fatalf("over the per-device cap the flow queued %d", n)
	}
	if got := r.requestReason(flowID); got != "blocked:automation_cap" {
		t.Fatalf("cap outcome: %s", got)
	}
}

// A flow armed while the switch was on sends nothing once the deployment switches AUTOMATION_COMMANDS off.
func TestAutomationCommandSwitchedOffAtRuntime(t *testing.T) {
	r := newAutomationRig(t, true)
	code, out := r.create(r.ownerAuth.AccessToken, commandFlow([]string{"door_open"}, r.door.IEEE, r.devices["light"], "state", "ON"), true, nil)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	flowID := out["id"].(string)
	r.f.repo.Configure(postgres.Options{AutomationCommands: false, DiscoveryLimit: 100})
	r.openDoor()
	if n := r.automationCommands(flowID); n != 0 {
		t.Fatalf("switched off, the flow still queued %d", n)
	}
	if got := r.runReason(flowID); got != "blocked:commands_disabled" {
		t.Fatalf("outcome: %s", got)
	}
}
