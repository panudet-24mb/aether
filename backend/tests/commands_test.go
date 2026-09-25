package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/app"
	"aether/backend/internal/commander"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// commandRig is a workspace with a Zigbee2MQTT gateway whose bridge is the simulator's FakeBridge, and a
// registered switch, colour bulb, curtain and lock. dispatch() plays mqtt-commander without a broker: claim
// (commit), then hand each command's /set to the bridge and feed its answer back through the collector path.
type commandRig struct {
	f          *fixture
	api        *fiber.App
	ctx        context.Context
	ownerAuth  app.AuthResult
	owner      domain.Principal
	gateway    string
	bridge     *simulation.FakeBridge
	devices    map[string]string // name -> device id
	dispatcher *commander.Dispatcher
	t          *testing.T
}

// rigStore is the real repository, limited to the rig's own workspace.
type rigStore struct {
	*postgres.Repository
	tenant string
}

func (s rigStore) ActiveTenants(context.Context) ([]string, error) { return []string{s.tenant}, nil }

// Publish plays the broker and the bridge: the /set reaches the FakeBridge, and its answer goes through the
// collector path, exactly as the state report of a real device would.
func (r *commandRig) Publish(topic string, payload []byte) error {
	r.capture(r.t, r.bridge.Apply(topic, payload))
	return nil
}

func newCommandRig(t *testing.T, project *string) *commandRig {
	t.Helper()
	f := setup(t)
	r := &commandRig{f: f, api: busyAPI(f), ctx: context.Background(), devices: map[string]string{}}
	_, r.ownerAuth, r.owner = f.account(t)
	g, _, e := f.service.CreateGatewayIn(r.ctx, r.owner, "Zigbee", domain.Z2MGatewayModel, project)
	if e != nil {
		t.Fatal(e)
	}
	r.gateway = g.ID
	r.bridge = simulation.NewFakeBridge(zigbee2mqtt.BaseTopic(g.ID))
	r.t = t
	r.dispatcher = &commander.Dispatcher{Store: rigStore{f.repo, r.owner.TenantID}, Publisher: r, Now: time.Now}
	r.capture(t, r.bridge.Step(0))
	for name, reg := range map[string][2]string{
		"switch": {simulation.Z2MSwitches[1].IEEE, "tuya-ts001x-switch@1"},
		"light":  {simulation.Z2MActuators[0].IEEE, domain.Z2MGenericProfile},
		"cover":  {simulation.Z2MActuators[1].IEEE, domain.Z2MGenericProfile},
		"lock":   {simulation.Z2MActuators[2].IEEE, domain.Z2MGenericProfile},
	} {
		d, e := f.service.CreateDevice(r.ctx, r.owner, g.ID, name, reg[0], reg[1])
		if e != nil {
			t.Fatalf("register %s: %v", name, e)
		}
		r.devices[name] = d.ID
	}
	return r
}

func (r *commandRig) capture(t *testing.T, msgs []simulation.Z2MMessage) {
	t.Helper()
	for _, m := range msgs {
		gateway, msg, e := zigbee2mqtt.Route(m.Topic)
		if e != nil {
			t.Fatal(e)
		}
		if msg.Kind == zigbee2mqtt.Ignore {
			continue
		}
		if _, e := r.f.service.CaptureZ2M(r.ctx, r.owner.TenantID, gateway, msg, m.Payload); e != nil {
			t.Fatalf("%s: %v", m.Topic, e)
		}
	}
}

// send is POST /api/v1/commands with an Idempotency-Key.
func (r *commandRig) send(t *testing.T, token, key string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	q := httptest.NewRequest("POST", "/api/v1/commands", bytes.NewReader(b))
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Idempotency-Key", key)
	res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// dispatch runs one mqtt-commander sweep (the real dispatcher) and returns how many commands it published.
func (r *commandRig) dispatch(t *testing.T) int {
	t.Helper()
	n, e := r.dispatcher.RunOnce(r.ctx)
	if e != nil {
		t.Fatal(e)
	}
	return n
}

func (r *commandRig) status(t *testing.T, id string) string {
	t.Helper()
	code, out := get(t, r.api, "/api/v1/commands/"+id, r.ownerAuth.AccessToken)
	if code != 200 {
		t.Fatalf("command %s: %d %v", id, code, out)
	}
	return out["status"].(string)
}

func (r *commandRig) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if e := r.f.admin.QueryRowContext(r.ctx, q, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// The whole loop for three kinds of device: queue (202, pending) -> claim (sent) -> the bridge answers ->
// confirmed, with the device's reported value in the stored state and an audit row per command.
func TestCommandsThroughTheBridge(t *testing.T) {
	r := newCommandRig(t, nil)
	token := r.ownerAuth.AccessToken
	cases := []struct {
		device, property string
		value            any
		stored           string
	}{
		{"light", "brightness", 128, "128"},
		{"light", "color", map[string]any{"x": 0.3, "y": 0.35}, `{"x": 0.3, "y": 0.35}`},
		{"cover", "position", 50, "50"},
		{"lock", "state", "UNLOCK", `"UNLOCK"`},
	}
	for _, c := range cases {
		key := uuid.NewString()
		code, out := r.send(t, token, key, map[string]any{"device_id": r.devices[c.device], "property": c.property, "value": c.value})
		if code != 202 || out["status"] != "pending" || out["id"] != key || out["property"] != c.property {
			t.Fatalf("%s %s: %d %v", c.device, c.property, code, out)
		}
		if n := r.dispatch(t); n != 1 {
			t.Fatalf("published: %d", n)
		}
		if s := r.status(t, key); s != "confirmed" {
			t.Fatalf("%s %s: %s", c.device, c.property, s)
		}
		if n := r.count(t, `SELECT count(*) FROM core.audit_logs WHERE action='device.command' AND target_id=$1`, key); n != 1 {
			t.Fatalf("audit rows: %d", n)
		}
	}
	var state string
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT state->>'brightness' FROM core.z2m_devices WHERE gateway_id=$1 AND ieee=$2`, r.gateway, simulation.Z2MActuators[0].IEEE).Scan(&state); e != nil || state != "128" {
		t.Fatalf("stored state: %q %v", state, e)
	}
	// Controls: the bulb's settable features come straight from its definition; the read-only ones do not.
	code, controls := get(t, r.api, "/api/v1/devices/"+r.devices["light"]+"/controls", token)
	if code != 200 || controls["online"] != true || controls["can_command"] != true {
		t.Fatalf("controls: %d %v", code, controls)
	}
	props := map[string]bool{}
	for _, raw := range controls["features"].([]any) {
		props[raw.(map[string]any)["property"].(string)] = true
	}
	if !props["brightness"] || !props["color_temp"] || !props["color"] || !props["state"] || props["linkquality"] {
		t.Fatalf("features: %v", props)
	}
}

// A toggle is resolved to an explicit ON/OFF from the last reported value, and the switch event it causes names
// the command instead of calling it a press on the wall.
func TestSwitchToggleNamesTheCommand(t *testing.T) {
	r := newCommandRig(t, nil)
	sw := simulation.Z2MSwitches[1]
	var before string
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT state->>'state_left' FROM core.z2m_devices WHERE gateway_id=$1 AND ieee=$2`, r.gateway, sw.IEEE).Scan(&before); e != nil || before == "" {
		t.Fatalf("baseline: %q %v", before, e)
	}
	key := uuid.NewString()
	code, out := r.send(t, r.ownerAuth.AccessToken, key, map[string]any{"device_id": r.devices["switch"], "property": "state_left", "action": "toggle"})
	want := `"ON"`
	if before == "ON" {
		want = `"OFF"`
	}
	if raw, _ := json.Marshal(out["value"]); code != 202 || out["requested"] != "toggle" || string(raw) != want {
		t.Fatalf("toggle: %d %v", code, out)
	}
	r.dispatch(t)
	if s := r.status(t, key); s != "confirmed" {
		t.Fatalf("toggle: %s", s)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type IN ('switch_on','switch_off') AND detail->>'source'='command' AND detail->>'command_id'=$3`, r.gateway, sw.IEEE, key); n != 1 {
		t.Fatalf("switch event naming the command: %d", n)
	}
}

// Everything the definition does not allow is refused before anything is queued.
func TestCommandValidation(t *testing.T) {
	r := newCommandRig(t, nil)
	token := r.ownerAuth.AccessToken
	refused := []struct {
		body   map[string]any
		code   int
		reason string
	}{
		{map[string]any{"device_id": r.devices["light"], "property": "brightness", "value": 300}, 400, "value"},
		{map[string]any{"device_id": r.devices["light"], "property": "brightness", "value": "128"}, 400, "value"},
		{map[string]any{"device_id": r.devices["light"], "property": "color", "value": map[string]any{"x": 0.3}}, 400, "value"},
		{map[string]any{"device_id": r.devices["light"], "property": "linkquality", "value": 5}, 400, "not_settable"},
		{map[string]any{"device_id": r.devices["light"], "property": "volume", "value": 5}, 400, "unknown_property"},
		{map[string]any{"device_id": r.devices["light"], "property": "brightness", "action": "toggle"}, 400, "not_toggleable"},
		{map[string]any{"device_id": r.devices["lock"], "property": "lock_state", "value": "locked"}, 400, "not_settable"},
		{map[string]any{"device_id": r.devices["cover"], "property": "state", "value": "OPENING"}, 400, "value"},
		{map[string]any{"device_id": uuid.NewString(), "property": "state", "value": "ON"}, 404, "not_found"},
		{map[string]any{"device_id": r.devices["light"], "property": "state", "action": "toggle", "value": "ON"}, 400, "value"},
		{map[string]any{"device_id": r.devices["light"], "property": "state", "action": "blink"}, 400, "invalid_input"},
	}
	for _, c := range refused {
		code, out := r.send(t, token, uuid.NewString(), c.body)
		if code != c.code || out["error"] != c.reason {
			t.Fatalf("%v: %d %v", c.body, code, out)
		}
	}
	if code, out := r.send(t, token, "not-a-uuid", map[string]any{"device_id": r.devices["light"], "property": "state", "value": "ON"}); code != 400 {
		t.Fatalf("idempotency key must be a uuid: %d %v", code, out)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_commands WHERE gateway_id=$1`, r.gateway); n != 0 {
		t.Fatalf("refused commands were queued: %d", n)
	}
}

// Idempotency, one command in flight per property, timeout, expiry and offline.
func TestCommandLifecycle(t *testing.T) {
	r := newCommandRig(t, nil)
	token := r.ownerAuth.AccessToken
	light := r.devices["light"]
	key := uuid.NewString()
	body := map[string]any{"device_id": light, "property": "brightness", "value": 10}
	if code, out := r.send(t, token, key, body); code != 202 || out["id"] != key {
		t.Fatalf("first: %d %v", code, out)
	}
	// A retry of the same request is the same command; the same key for something else is refused.
	if code, out := r.send(t, token, key, body); code != 202 || out["id"] != key {
		t.Fatalf("retry: %d %v", code, out)
	}
	if code, out := r.send(t, token, key, map[string]any{"device_id": light, "property": "brightness", "value": 20}); code != 409 || out["error"] != "idempotency_key_reused" {
		t.Fatalf("reused key: %d %v", code, out)
	}
	// A second change of the same property while the first is in flight is a conflict; another property is fine.
	if code, out := r.send(t, token, uuid.NewString(), map[string]any{"device_id": light, "property": "brightness", "value": 30}); code != 409 || out["error"] != "in_flight" {
		t.Fatalf("in flight: %d %v", code, out)
	}
	other := uuid.NewString()
	if code, _ := r.send(t, token, other, map[string]any{"device_id": light, "property": "color_temp", "value": 300}); code != 202 {
		t.Fatalf("other property: %d", code)
	}
	// The device never answers: sent, then timeout after the confirm window.
	r.bridge.DropCommands = true
	r.dispatch(t)
	if s := r.status(t, key); s != "sent" {
		t.Fatalf("sent: %s", s)
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.device_commands SET sent_at=now()-interval '20 seconds' WHERE gateway_id=$1`, r.gateway); e != nil {
		t.Fatal(e)
	}
	if n, e := r.f.repo.TimeoutCommands(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil || n != 2 {
		t.Fatalf("timeout sweep: %d %v", n, e)
	}
	if s := r.status(t, key); s != "timeout" {
		t.Fatalf("timeout: %s", s)
	}
	// A report that arrives shortly after the timeout still confirms it.
	r.bridge.DropCommands = false
	r.capture(t, r.bridge.Apply(zigbee2mqtt.BaseTopic(r.gateway)+"/"+simulation.Z2MActuators[0].IEEE+"/set", []byte(`{"brightness":10}`)))
	if s := r.status(t, key); s != "confirmed" {
		t.Fatalf("late confirmation: %s", s)
	}
	if _, out := get(t, r.api, "/api/v1/commands/"+key, token); out["error"] != "confirmed after the timeout" {
		t.Fatalf("late confirmation note: %v", out)
	}
	// Not claimed before it expired: never published.
	stale := uuid.NewString()
	if code, _ := r.send(t, token, stale, map[string]any{"device_id": r.devices["cover"], "property": "position", "value": 80}); code != 202 {
		t.Fatalf("stale: %d", code)
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.device_commands SET expires_at=now()-interval '1 second' WHERE id=$1`, stale); e != nil {
		t.Fatal(e)
	}
	if n := r.dispatch(t); n != 0 {
		t.Fatalf("expired command published: %d", n)
	}
	if s := r.status(t, stale); s != "expired" {
		t.Fatalf("expired: %s", s)
	}
	// Offline (availability) and a gone bridge refuse commands.
	lightName := simulation.Z2MActuators[0].Name
	base := zigbee2mqtt.BaseTopic(r.gateway)
	r.capture(t, []simulation.Z2MMessage{{Topic: base + "/" + lightName + "/availability", Payload: simulation.Z2MOnline(false)}})
	if code, out := r.send(t, token, uuid.NewString(), map[string]any{"device_id": light, "property": "state", "value": "ON"}); code != 409 || out["error"] != "offline" {
		t.Fatalf("offline device: %d %v", code, out)
	}
	r.capture(t, []simulation.Z2MMessage{{Topic: base + "/bridge/state", Payload: simulation.Z2MOnline(false)}})
	if code, out := r.send(t, token, uuid.NewString(), map[string]any{"device_id": r.devices["cover"], "property": "position", "value": 20}); code != 409 || out["error"] != "offline" {
		t.Fatalf("bridge offline: %d %v", code, out)
	}
}

// The workspace budget: 60 commands a minute whatever their source.
func TestCommandWorkspaceCap(t *testing.T) {
	r := newCommandRig(t, nil)
	if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.device_commands(tenant_id,id,gateway_id,device_id,ieee,property,requested,value,source,status,created_at,expires_at,settled_at)
    SELECT $1,gen_random_uuid(),$2,$3,$4,'brightness','set','1','user','confirmed',now(),now(),now() FROM generate_series(1,60)`, r.owner.TenantID, r.gateway, r.devices["light"], simulation.Z2MActuators[0].IEEE); e != nil {
		t.Fatal(e)
	}
	if code, out := r.send(t, r.ownerAuth.AccessToken, uuid.NewString(), map[string]any{"device_id": r.devices["cover"], "property": "position", "value": 20}); code != 429 || out["error"] != "rate_limited" {
		t.Fatalf("cap: %d %v", code, out)
	}
}

// Who may command: owner/admin/operator within their projects, never a viewer, and an owner can switch the
// "control" module off for a member. Another workspace sees nothing.
func TestCommandPermissions(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	projectA, e := f.service.CreateProject(ctx, owner, "A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	projectB, e := f.service.CreateProject(ctx, owner, "B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	r := &commandRig{f: f, api: api, ctx: ctx, ownerAuth: ownerAuth, owner: owner, devices: map[string]string{}, t: t}
	g, _, e := f.service.CreateGatewayIn(ctx, owner, "Zigbee A", domain.Z2MGatewayModel, &projectA.ID)
	if e != nil {
		t.Fatal(e)
	}
	r.gateway = g.ID
	r.bridge = simulation.NewFakeBridge(zigbee2mqtt.BaseTopic(g.ID))
	r.dispatcher = &commander.Dispatcher{Store: rigStore{f.repo, owner.TenantID}, Publisher: r, Now: time.Now}
	r.capture(t, r.bridge.Step(0))
	light, e := f.service.CreateDevice(ctx, owner, g.ID, "light", simulation.Z2MActuators[0].IEEE, domain.Z2MGenericProfile)
	if e != nil {
		t.Fatal(e)
	}
	members := map[string]app.AuthResult{}
	for _, m := range []struct {
		name, role string
		projects   []string
	}{{"viewer", "viewer", []string{}}, {"operatorA", "operator", []string{projectA.ID}}, {"operatorB", "operator", []string{projectB.ID}}, {"operatorNoControl", "operator", []string{}}} {
		email := memberEmail()
		addMember(t, api, ownerAuth.AccessToken, email, m.role, m.projects)
		members[m.name], _ = changeInitialPassword(t, f, api, email, owner.TenantID)
		if m.name == "operatorNoControl" {
			id := memberID(t, api, ownerAuth.AccessToken, email)
			if code, _, _ := req(t, api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+ownerAuth.AccessToken, "", "", map[string]any{"control": "read"}); code != 204 {
				t.Fatalf("access: %d", code)
			}
		}
	}
	body := func(v int) map[string]any {
		return map[string]any{"device_id": light.ID, "property": "brightness", "value": v}
	}
	if code, _ := r.send(t, members["viewer"].AccessToken, uuid.NewString(), body(1)); code != 403 {
		t.Fatalf("viewer: %d", code)
	}
	if code, out := get(t, api, "/api/v1/devices/"+light.ID+"/controls", members["viewer"].AccessToken); code != 200 || out["can_command"] != false {
		t.Fatalf("viewer controls: %d %v", code, out)
	}
	if code, _ := r.send(t, members["operatorB"].AccessToken, uuid.NewString(), body(2)); code != 404 {
		t.Fatalf("operator of another project: %d", code)
	}
	if code, _ := r.send(t, members["operatorNoControl"].AccessToken, uuid.NewString(), body(3)); code != 403 {
		t.Fatalf("control module read-only: %d", code)
	}
	if code, out := get(t, api, "/api/v1/devices/"+light.ID+"/controls", members["operatorNoControl"].AccessToken); code != 200 || out["can_command"] != false {
		t.Fatalf("no-control controls: %d %v", code, out)
	}
	key := uuid.NewString()
	if code, out := r.send(t, members["operatorA"].AccessToken, key, body(4)); code != 202 {
		t.Fatalf("operator of the project: %d %v", code, out)
	}
	if code, _ := get(t, api, "/api/v1/commands/"+key, members["operatorB"].AccessToken); code != 404 {
		t.Fatalf("operator B read A's command: %d", code)
	}
	_, otherAuth, _ := f.account(t)
	if code, _ := get(t, api, "/api/v1/commands/"+key, otherAuth.AccessToken); code != 404 {
		t.Fatalf("another workspace read the command: %d", code)
	}
	if code, out := get(t, api, "/api/v1/commands?device_id="+light.ID, ownerAuth.AccessToken); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}
}

// The access gate classifies the path the router dispatches: a member whose "control" (or "alerts") module is
// closed cannot get around it by spelling the path in another case or with extra slashes, and the command
// service refuses them on its own as well.
func TestAccessGateCannotBeBypassedByPathSpelling(t *testing.T) {
	r := newCommandRig(t, nil)
	email := memberEmail()
	addMember(t, r.api, r.ownerAuth.AccessToken, email, "operator", []string{})
	id := memberID(t, r.api, r.ownerAuth.AccessToken, email)
	if code, _, _ := req(t, r.api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+r.ownerAuth.AccessToken, "", "", map[string]any{"control": "none", "alerts": "none"}); code != 204 {
		t.Fatalf("access: %d", code)
	}
	auth, member := changeInitialPassword(t, r.f, r.api, email, r.owner.TenantID)
	body := map[string]any{"device_id": r.devices["light"], "property": "brightness", "value": 10}
	for _, path := range []string{"/api/v1/commands", "/api/v1/Commands", "/API/V1/commands", "/api/v1/COMMANDS/", "/api/v1//commands", "/Api/v1/commands/"} {
		b, _ := json.Marshal(body)
		q := httptest.NewRequest("POST", path, bytes.NewReader(b))
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Authorization", "Bearer "+auth.AccessToken)
		q.Header.Set("Idempotency-Key", uuid.NewString())
		res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode < 400 {
			t.Fatalf("%s reached the command handler: %d", path, res.StatusCode)
		}
	}
	for _, path := range []string{"/api/v1/alerts", "/api/v1/Alerts", "/API/V1/ALERTS", "/api/v1//alerts/"} {
		if code, _ := get(t, r.api, path, auth.AccessToken); code < 400 {
			t.Fatalf("%s bypassed the alerts module: %d", path, code)
		}
	}
	if code, _ := get(t, r.api, "/api/v1/alerts", r.ownerAuth.AccessToken); code != 200 {
		t.Fatalf("normal path: %d", code)
	}
	// The service applies the same rule without the HTTP gate.
	if _, _, e := r.f.service.SendCommand(r.ctx, member, domain.CommandRequest{ID: uuid.NewString(), DeviceID: r.devices["light"], Property: "brightness", Action: "set", Value: []byte(`10`)}); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("service: %v", e)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_commands WHERE gateway_id=$1`, r.gateway); n != 0 {
		t.Fatalf("commands queued: %d", n)
	}
}

// The workspace budget is workspace-wide: members of different projects share it.
func TestCommandCapIsWorkspaceWide(t *testing.T) {
	f := setup(t)
	api := busyAPI(f)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	r := &commandRig{f: f, api: api, ctx: ctx, ownerAuth: ownerAuth, owner: owner, devices: map[string]string{}, t: t}
	lights := map[string]string{}
	gateways := map[string]string{}
	for _, name := range []string{"A", "B"} {
		pr, e := f.service.CreateProject(ctx, owner, name, "", "mint")
		if e != nil {
			t.Fatal(e)
		}
		g, _, e := f.service.CreateGatewayIn(ctx, owner, "Zigbee "+name, domain.Z2MGatewayModel, &pr.ID)
		if e != nil {
			t.Fatal(e)
		}
		r.bridge = simulation.NewFakeBridge(zigbee2mqtt.BaseTopic(g.ID))
		r.capture(t, r.bridge.Step(0))
		d, e := f.service.CreateDevice(ctx, owner, g.ID, "light "+name, simulation.Z2MActuators[0].IEEE, domain.Z2MGenericProfile)
		if e != nil {
			t.Fatal(e)
		}
		lights[name], gateways[name] = d.ID, g.ID
		email := memberEmail()
		addMember(t, api, ownerAuth.AccessToken, email, "operator", []string{pr.ID})
		auth, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
		gateways["token"+name] = auth.AccessToken
	}
	// 60 commands of project A this minute; the operator of project B cannot see them, yet the budget is spent.
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.device_commands(tenant_id,id,gateway_id,device_id,ieee,property,requested,value,source,status,created_at,expires_at,settled_at)
    SELECT $1,gen_random_uuid(),$2,$3,$4,'brightness','set','1','user','confirmed',now(),now(),now() FROM generate_series(1,60)`, owner.TenantID, gateways["A"], lights["A"], simulation.Z2MActuators[0].IEEE); e != nil {
		t.Fatal(e)
	}
	if code, out := r.send(t, gateways["tokenB"], uuid.NewString(), map[string]any{"device_id": lights["B"], "property": "brightness", "value": 20}); code != 429 || out["error"] != "rate_limited" {
		t.Fatalf("operator B past the workspace cap: %d %v", code, out)
	}
}

// A timed-out command is not confirmed by a later report once the property was reported with another value in
// between (a press on the wall), and a late confirmation never names itself as the cause of a switch event.
func TestLateConfirmationIsNotMisattributed(t *testing.T) {
	r := newCommandRig(t, nil)
	sw := simulation.Z2MSwitches[1]
	base := zigbee2mqtt.BaseTopic(r.gateway)
	setTopic := base + "/" + sw.IEEE + "/set"
	r.capture(t, r.bridge.Apply(setTopic, []byte(`{"state_left":"OFF"}`)))
	key := uuid.NewString()
	if code, _ := r.send(t, r.ownerAuth.AccessToken, key, map[string]any{"device_id": r.devices["switch"], "property": "state_left", "value": "ON"}); code != 202 {
		t.Fatalf("queue: %d", code)
	}
	r.bridge.DropCommands = true
	r.dispatch(t)
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.device_commands SET sent_at=now()-interval '20 seconds' WHERE id=$1`, key); e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.repo.TimeoutCommands(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	// The wall reports OFF (another value): superseded. Then ON arrives: that is a press too, not our command.
	r.bridge.DropCommands = false
	r.capture(t, r.bridge.Apply(setTopic, []byte(`{"state_left":"OFF","state_right":"ON"}`)))
	r.capture(t, r.bridge.Apply(setTopic, []byte(`{"state_left":"ON"}`)))
	if s := r.status(t, key); s != "timeout" {
		t.Fatalf("superseded command confirmed: %s", s)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND detail->>'source'='command'`, r.gateway, sw.IEEE); n != 0 {
		t.Fatalf("events attributed to a command: %d", n)
	}
	// Without an intervening change a late report still confirms, but the event stays "external".
	key2 := uuid.NewString()
	if code, _ := r.send(t, r.ownerAuth.AccessToken, key2, map[string]any{"device_id": r.devices["switch"], "property": "state_left", "value": "OFF"}); code != 202 {
		t.Fatalf("queue 2: %d", code)
	}
	r.bridge.DropCommands = true
	r.dispatch(t)
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.device_commands SET sent_at=now()-interval '20 seconds' WHERE id=$1`, key2); e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.repo.TimeoutCommands(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	r.bridge.DropCommands = false
	r.capture(t, r.bridge.Apply(setTopic, []byte(`{"state_left":"OFF"}`)))
	if s := r.status(t, key2); s != "confirmed" {
		t.Fatalf("late confirmation: %s", s)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND detail->>'command_id'=$3`, r.gateway, sw.IEEE, key2); n != 0 {
		t.Fatalf("late confirmation named in an event: %d", n)
	}
}

// Pacing: a burst on one gateway is published 10 at a time; what waits stays pending and expires untouched.
func TestCommandPacingLeavesOverflowPending(t *testing.T) {
	r := newCommandRig(t, nil)
	if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.device_commands(tenant_id,id,gateway_id,device_id,ieee,property,requested,value,source,status,created_at,expires_at)
    SELECT $1,gen_random_uuid(),$2,$3,$4,'prop'||i,'set','1','user','pending',now()+make_interval(secs=>i/1000.0),now()+interval '10 seconds' FROM generate_series(1,12) i`,
		r.owner.TenantID, r.gateway, r.devices["light"], simulation.Z2MActuators[0].IEEE); e != nil {
		t.Fatal(e)
	}
	if n := r.dispatch(t); n != 10 {
		t.Fatalf("first sweep published %d", n)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_commands WHERE gateway_id=$1 AND status='pending'`, r.gateway); n != 2 {
		t.Fatalf("overflow pending: %d", n)
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.device_commands SET expires_at=now()-interval '1 second' WHERE gateway_id=$1 AND status='pending'`, r.gateway); e != nil {
		t.Fatal(e)
	}
	if n := r.dispatch(t); n != 0 {
		t.Fatalf("expired overflow published: %d", n)
	}
	if n := r.count(t, `SELECT count(*) FROM core.device_commands WHERE gateway_id=$1 AND status='expired'`, r.gateway); n != 2 {
		t.Fatalf("expired: %d", n)
	}
}
