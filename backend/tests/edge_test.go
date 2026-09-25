package tests

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/app"
	"aether/backend/internal/commander"
	"aether/backend/internal/domain"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// edgeDevice is one Tuya device of the rig: its specification fixture and its Tuya id.
type edgeDevice struct {
	name, fixture, category, id string
	model                       bool // the fixture is a v2.0 thing model, not v1.1 specifications
}

var edgeDevices = []edgeDevice{
	{name: "switch", fixture: "kg_3gang.json", id: "bf00000000000000sw01"},
	{name: "plug", fixture: "cz_metering.json", id: "bf00000000000000pl01"},
	{name: "thermo", fixture: "wk_thermostat.json", id: "bf00000000000000th01"},
	{name: "sensor", fixture: "wsdcg_sleeper.json", id: "bf00000000000000se01"},
	{name: "light", fixture: "dj_light_v2_model.json", category: "dj", id: "bf00000000000000li01", model: true},
}

// edgeRig is a workspace with an Aether Edge gateway whose agent is played by the rig itself: dispatch() runs the
// real mqtt-commander sweep, and every /set it publishes is applied to the fake device's data points and answered
// with a state report through the collector path, as the agent would.
type edgeRig struct {
	f          *fixture
	api        *fiber.App
	ctx        context.Context
	ownerAuth  app.AuthResult
	owner      domain.Principal
	gateway    string
	ids        map[string]string // name -> Tuya id
	devices    map[string]string // name -> registration id
	dispatcher *commander.Dispatcher
	published  []string
	t          *testing.T
}

func newEdgeRig(t *testing.T, project *string) *edgeRig {
	t.Helper()
	f := setup(t)
	r := &edgeRig{f: f, api: busyAPI(f), ctx: context.Background(), ids: map[string]string{}, devices: map[string]string{}, t: t}
	_, r.ownerAuth, r.owner = f.account(t)
	g, _, e := f.service.CreateGatewayIn(r.ctx, r.owner, "Edge", domain.EdgeGatewayModel, project)
	if e != nil {
		t.Fatal(e)
	}
	r.gateway = g.ID
	r.dispatcher = &commander.Dispatcher{Store: rigStore{f.repo, r.owner.TenantID}, Publisher: r, Now: time.Now}
	imports := []postgres.TuyaImport{}
	for _, d := range edgeDevices {
		raw, e := os.ReadFile("../internal/adapters/tuya/testdata/" + d.fixture)
		if e != nil {
			t.Fatal(e)
		}
		var dps []tuya.DP
		category := d.category
		if d.model {
			dps, e = tuya.ParseModel(raw)
		} else {
			dps, category, e = tuya.ParseSpecifications(raw)
		}
		if e != nil {
			t.Fatal(e)
		}
		r.ids[d.name] = d.id
		imports = append(imports, postgres.TuyaImport{TuyaID: d.id, Name: "Tuya " + d.name, Category: category, ProductID: "p" + d.name, Spec: dps,
			LocalKeySealed: "sealed-test-key-" + d.name, KeyFingerprint: "0123456789abcdef"})
	}
	if n, e := f.repo.SaveTuyaDevices(r.ctx, r.owner, g.ID, imports); e != nil || n != len(imports) {
		t.Fatalf("import: %d %v", n, e)
	}
	return r
}

func (r *edgeRig) register(name string) string {
	r.t.Helper()
	d, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Tuya "+name, r.ids[name], domain.TuyaWiFiProfile)
	if e != nil {
		r.t.Fatalf("register %s: %v", name, e)
	}
	r.devices[name] = d.ID
	return d.ID
}

// capture feeds one agent message through the collector path.
func (r *edgeRig) capture(suffix string, payload any) error {
	r.t.Helper()
	var body []byte
	switch p := payload.(type) {
	case []byte:
		body = p
	case string:
		body = []byte(p)
	default:
		body, _ = json.Marshal(p)
	}
	gateway, msg, e := edge.Route(edge.Prefix + r.gateway + "/" + suffix)
	if e != nil {
		return e
	}
	_, e = r.f.service.CaptureEdge(r.ctx, r.owner.TenantID, gateway, msg, body)
	return e
}

func (r *edgeRig) must(e error) {
	r.t.Helper()
	if e != nil {
		r.t.Fatal(e)
	}
}

// report is the agent publishing a device's data points.
func (r *edgeRig) report(name string, dps map[int]any) {
	r.t.Helper()
	obj := map[string]any{}
	for k, v := range dps {
		obj[strconv.Itoa(k)] = v
	}
	r.must(r.capture(r.ids[name]+"/state", map[string]any{"dps": obj}))
}

// Publish plays the broker, the agent and the device: the /set's data points are applied and echoed back.
func (r *edgeRig) Publish(topic string, payload []byte) error {
	r.published = append(r.published, topic+" "+string(payload))
	parts := strings.Split(strings.TrimPrefix(topic, edge.Prefix), "/")
	if len(parts) != 3 || parts[0] != r.gateway || parts[2] != "set" {
		return errors.New("unexpected topic " + topic)
	}
	var wire map[string]any
	if e := json.Unmarshal(payload, &wire); e != nil {
		return e
	}
	return r.capture(parts[1]+"/state", wire)
}

func (r *edgeRig) dispatch() int {
	r.t.Helper()
	n, e := r.dispatcher.RunOnce(r.ctx)
	if e != nil {
		r.t.Fatal(e)
	}
	return n
}

func (r *edgeRig) send(token string, body map[string]any) (int, map[string]any) {
	r.t.Helper()
	b, _ := json.Marshal(body)
	q := httptest.NewRequest("POST", "/api/v1/commands", bytes.NewReader(b))
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Idempotency-Key", uuid.NewString())
	res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		r.t.Fatal(e)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (r *edgeRig) count(q string, args ...any) int {
	r.t.Helper()
	var n int
	if e := r.f.admin.QueryRowContext(r.ctx, q, args...).Scan(&n); e != nil {
		r.t.Fatal(e)
	}
	return n
}

func (r *edgeRig) text(q string, args ...any) string {
	r.t.Helper()
	var s string
	if e := r.f.admin.QueryRowContext(r.ctx, q, args...).Scan(&s); e != nil {
		r.t.Fatal(e)
	}
	return s
}

func (r *edgeRig) revision() int {
	return r.count(`SELECT coalesce(max(config_revision),0) FROM core.edge_agents WHERE gateway_id=$1`, r.gateway)
}

func (r *edgeRig) events(id, eventType string) int {
	return r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type=$3`, r.gateway, id, eventType)
}

// Import, discovery, registration, state -> readings and switch events, and liveness from the agent.
func TestEdgeIngest(t *testing.T) {
	r := newEdgeRig(t, nil)
	// Discovery lists the imported devices (a locally reachable one with the Tuya Wi-Fi profile, the battery sensor
	// without), then what the agent sees on its LAN without a key.
	stranger := "bf00000000000000zz99"
	r.must(r.capture("discovery", []map[string]any{
		{"id": r.ids["switch"], "ip": "192.168.1.20", "version": "3.4"},
		{"id": stranger, "ip": "192.168.1.99", "version": "3.3"},
	}))
	if ip := r.text(`SELECT ip FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, r.ids["switch"]); ip != "192.168.1.20" {
		t.Fatalf("LAN address not recorded: %q", ip)
	}
	code, out := get(t, r.api, "/api/v1/discovery", r.ownerAuth.AccessToken)
	if code != 200 {
		t.Fatalf("discovery: %d", code)
	}
	found := map[string]map[string]any{}
	for _, raw := range out["items"].([]any) {
		item := raw.(map[string]any)
		found[item["external_id"].(string)] = item
	}
	sw, sensor, lan := found[r.ids["switch"]], found[r.ids["sensor"]], found[stranger]
	if sw == nil || sw["source"] != "tuya" || sw["key_status"] != "ok" || sw["ip"] != "192.168.1.20" || sw["protocol_version"] != "3.4" ||
		sw["profile"] == nil || sw["profile"].(map[string]any)["id"] != domain.TuyaWiFiProfile {
		t.Fatalf("imported switch: %v", sw)
	}
	if sensor == nil || sensor["local_capable"] != false || sensor["profile"] != nil {
		t.Fatalf("battery sensor: %v", sensor)
	}
	if lan == nil || lan["source"] != "tuya_lan" || lan["key_status"] != "missing" {
		t.Fatalf("LAN-only device: %v", lan)
	}
	// Registration: Tuya profiles only under an Edge, and nothing else under an Edge. Each registration moves the
	// agent's configuration revision.
	before := r.revision()
	r.register("switch")
	r.register("plug")
	r.register("thermo")
	if after := r.revision(); after < before+3 {
		t.Fatalf("config revision %d -> %d", before, after)
	}
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "BLE on edge", "c30000393fe5", "minew-s1-pending@1"); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("BLE profile under an Edge: %v", e)
	}
	z2m, _, e := r.f.service.CreateGateway(r.ctx, r.owner, "Zigbee", domain.Z2MGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, z2m.ID, "Tuya on zigbee", r.ids["light"], domain.TuyaWiFiProfile); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("Tuya Wi-Fi profile under Zigbee2MQTT: %v", e)
	}
	if code, out = get(t, r.api, "/api/v1/discovery", r.ownerAuth.AccessToken); code == 200 {
		for _, raw := range out["items"].([]any) {
			if raw.(map[string]any)["external_id"] == r.ids["switch"] {
				t.Fatal("a registered device is still discovered")
			}
		}
	}

	// State: the first report is a baseline; a change of gang 2 is one switch_on.
	sw1 := r.ids["switch"]
	r.report("switch", map[int]any{1: true, 2: false, 3: true, 7: 0})
	if n := r.events(sw1, domain.EventSwitchOn); n != 0 {
		t.Fatalf("baseline raised %d switch events", n)
	}
	r.report("switch", map[int]any{2: true})
	if n := r.events(sw1, domain.EventSwitchOn); n != 1 {
		t.Fatalf("gang 2 on: %d events", n)
	}
	if d := r.text(`SELECT decoder_id FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 ORDER BY received_at DESC LIMIT 1`, r.gateway, sw1); !strings.Contains(d, edge.FrameState) {
		t.Fatalf("decoder: %q", d)
	}
	if l := r.text(`SELECT liveness FROM core.sensor_streams WHERE gateway_id=$1 AND external_id=$2`, r.gateway, sw1); l != "reported" {
		t.Fatalf("liveness: %q", l)
	}
	if s := r.text(`SELECT state->>'switch_2' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, sw1); s != "ON" {
		t.Fatalf("stored settable state: %q", s)
	}
	// A metering plug's scaled readings, and a thermostat's door flag becoming door events.
	r.report("plug", map[int]any{1: true, 18: 1234, 19: 1003, 20: 2301})
	var reading map[string]any
	raw := r.text(`SELECT reading::text FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 ORDER BY received_at DESC LIMIT 1`, r.gateway, r.ids["plug"])
	_ = json.Unmarshal([]byte(raw), &reading)
	metrics, _ := reading["metrics"].(map[string]any)
	if metrics["power"] != 100.3 || metrics["current"] != 1.234 || metrics["voltage"] != 230.1 {
		t.Fatalf("plug reading: %s", raw)
	}
	thermo := r.ids["thermo"]
	r.report("thermo", map[int]any{1: true, 3: 198, 9: false})
	r.report("thermo", map[int]any{9: true})
	if n := r.events(thermo, domain.EventDoorOpen); n != 1 {
		t.Fatalf("door_open: %d", n)
	}
	// Data points of a device that was never imported are kept only as the diagnostic copy.
	r.must(r.capture(stranger+"/state", `{"dps":{"1":true}}`))
	if n := r.count(`SELECT count(*) FROM core.sensor_streams WHERE gateway_id=$1 AND external_id=$2`, r.gateway, stranger); n != 0 {
		t.Fatal("a device without a key got a stream")
	}

	// Availability: a refused key is flagged on the device and the offline event names it; a later connection
	// clears it.
	r.must(r.capture(sw1+"/availability", `{"state":"offline","reason":"auth_failed"}`))
	if ks := r.text(`SELECT key_status FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, sw1); ks != "rejected" {
		t.Fatalf("key status: %q", ks)
	}
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='offline' AND detail->>'source'='key_rejected'`, r.gateway, sw1); n != 1 {
		t.Fatalf("key_rejected offline events: %d", n)
	}
	r.must(r.capture(sw1+"/availability", `{"state":"online"}`))
	if ks := r.text(`SELECT key_status FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, sw1); ks != "ok" || r.events(sw1, domain.EventOnline) != 1 {
		t.Fatalf("after reconnect: %q, %d online events", ks, r.events(sw1, domain.EventOnline))
	}

	// The agent's last will takes every device it reports offline; its next message brings them back.
	r.must(r.capture("status", `{"state":"offline"}`))
	for _, name := range []string{"switch", "plug", "thermo"} {
		if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='offline' AND detail->>'source'='edge_offline'`, r.gateway, r.ids[name]); n != 1 {
			t.Fatalf("%s: %d edge_offline events", name, n)
		}
	}
	r.must(r.capture("health", `{"version":"0.1.0","devices_connected":3,"lan_seen":2}`))
	if n := r.count(`SELECT count(*) FROM core.stream_state WHERE gateway_id=$1 AND offline`, r.gateway); n != 0 {
		t.Fatalf("%d devices still offline after the agent came back", n)
	}
	if v := r.text(`SELECT version FROM core.edge_agents WHERE gateway_id=$1`, r.gateway); v != "0.1.0" {
		t.Fatalf("agent version: %q", v)
	}
	// Silence without a last will: after EdgeSilentAfter the offline scan takes the devices offline, once.
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.gateway_packets SET received_at=now()-interval '10 minutes' WHERE gateway_id=$1`, r.gateway); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if _, e := r.f.repo.ScanOffline(r.ctx, r.owner.TenantID, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='offline' AND detail->>'source'='edge_silent'`, r.gateway, sw1); n != 1 {
		t.Fatalf("edge_silent events: %d", n)
	}

	// Poison and foreign messages.
	for _, bad := range []struct{ suffix, body string }{
		{sw1 + "/state", `{"dps":{"1":"a\u0000b"}}`},
		{sw1 + "/state", `{"dps":{"x":1}}`},
		{sw1 + "/state", `[1]`},
		{"discovery", `{"not":"a list"}`},
		{"health", `"fine"`},
	} {
		if e := r.capture(bad.suffix, bad.body); !errors.Is(e, domain.ErrInvalid) {
			t.Fatalf("%s %s: %v", bad.suffix, bad.body, e)
		}
	}
	ble, _, e := r.f.service.CreateGateway(r.ctx, r.owner, "BLE", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.service.CaptureEdge(r.ctx, r.owner.TenantID, ble.ID, edge.Message{Kind: edge.Status, Topic: "x"}, []byte(`"online"`)); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("Edge message for a BLE gateway: %v", e)
	}
}

// Commands to Tuya devices: validated against the translated exposes, encoded to the data point when queued,
// published by the commander in wire form, and confirmed by the device's report.
func TestEdgeCommands(t *testing.T) {
	r := newEdgeRig(t, nil)
	token := r.ownerAuth.AccessToken
	r.register("switch")
	r.register("thermo")
	r.register("light")
	r.report("switch", map[int]any{1: true, 2: false, 3: false})
	r.report("thermo", map[int]any{1: true, 2: 200, 9: false})
	r.report("light", map[int]any{20: true, 22: 100})

	cases := []struct {
		device   string
		body     map[string]any
		wireDP   string
		wireRaw  string
		property string
	}{
		{"switch", map[string]any{"property": "switch_2", "value": "ON"}, "2", "true", "switch_2"},
		{"switch", map[string]any{"property": "switch_1", "action": "toggle"}, "1", "false", "switch_1"},
		{"thermo", map[string]any{"property": "temp_set", "value": 21.5}, "2", "215", "temp_set"},
		{"light", map[string]any{"property": "brightness", "value": 500}, "22", "500", "brightness"},
	}
	for _, c := range cases {
		body := map[string]any{"device_id": r.devices[c.device]}
		for k, v := range c.body {
			body[k] = v
		}
		code, out := r.send(token, body)
		if code != 202 || out["status"] != "pending" || out["transport"] != "edge" {
			t.Fatalf("%s %v: %d %v", c.device, c.body, code, out)
		}
		r.published = nil
		if n := r.dispatch(); n != 1 {
			t.Fatalf("published %d", n)
		}
		want := edge.Prefix + r.gateway + "/" + r.ids[c.device] + `/set {"dps":{"` + c.wireDP + `":` + c.wireRaw + `}}`
		if len(r.published) != 1 || r.published[0] != want {
			t.Fatalf("wire: %v, want %s", r.published, want)
		}
		if s := r.text(`SELECT status FROM core.device_commands WHERE id=$1`, out["id"]); s != "confirmed" {
			t.Fatalf("%s %s: %s", c.device, c.property, s)
		}
	}
	// The switch event a command caused names it.
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='switch_on' AND detail->>'source'='command'`, r.gateway, r.ids["switch"]); n != 1 {
		t.Fatalf("switch_on attributed to the command: %d", n)
	}
	// Refusals: off-step values, read-only data points, measurements, a device whose key was refused.
	for _, bad := range []struct {
		body   map[string]any
		code   int
		reason string
	}{
		{map[string]any{"device_id": r.devices["thermo"], "property": "temp_set", "value": 21.3}, 400, "value"},
		{map[string]any{"device_id": r.devices["thermo"], "property": "temperature", "value": 20}, 400, "not_settable"},
		{map[string]any{"device_id": r.devices["switch"], "property": "switch_2", "value": true}, 400, "value"},
		{map[string]any{"device_id": r.devices["switch"], "property": "switch_9", "value": "ON"}, 400, "unknown_property"},
	} {
		if code, out := r.send(token, bad.body); code != bad.code || out["error"] != bad.reason {
			t.Fatalf("%v: %d %v", bad.body, code, out)
		}
	}
	r.must(r.capture(r.ids["light"]+"/availability", `{"state":"offline","reason":"auth_failed"}`))
	if code, out := r.send(token, map[string]any{"device_id": r.devices["light"], "property": "brightness", "value": 300}); code != 409 || out["error"] != "key_unavailable" {
		t.Fatalf("refused key: %d %v", code, out)
	}
	// Controls come from the translated specification; a device with a refused key is not online.
	code, controls := get(t, r.api, "/api/v1/devices/"+r.devices["thermo"]+"/controls", token)
	if code != 200 || controls["online"] != true {
		t.Fatalf("controls: %d %v", code, controls)
	}
	props := map[string]bool{}
	for _, raw := range controls["features"].([]any) {
		props[raw.(map[string]any)["property"].(string)] = true
	}
	if !props["temp_set"] || !props["mode"] || !props["switch"] || props["temperature"] {
		t.Fatalf("features: %v", props)
	}
	if code, controls = get(t, r.api, "/api/v1/devices/"+r.devices["light"]+"/controls", token); code != 200 || controls["online"] != false {
		t.Fatalf("light with a refused key: %d %v", code, controls)
	}
}

// An automation can command a Tuya device: the thermostat's window contact opening switches gang 1 off.
func TestEdgeAutomationCommand(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.f.repo.Configure(postgres.Options{AutomationCommands: true, DiscoveryLimit: 100})
	r.register("switch")
	r.register("thermo")
	r.report("switch", map[int]any{1: true})
	r.report("thermo", map[int]any{9: false})
	def := commandFlow([]string{"door_open"}, r.ids["thermo"], r.devices["switch"], "switch_1", "OFF")
	code, out, _ := req(t, r.api, "POST", "/api/v1/automations", "Bearer "+r.ownerAuth.AccessToken, "", "", map[string]any{"name": "window", "enabled": true, "definition": def})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	flowID := out["id"].(string)
	r.report("thermo", map[int]any{9: true})
	if n, e := r.f.repo.ProcessAutomationRequests(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil || n != 1 {
		t.Fatalf("drain: %d %v", n, e)
	}
	if tr := r.text(`SELECT transport FROM core.device_commands WHERE automation_id=$1`, flowID); tr != "edge" {
		t.Fatalf("transport: %q", tr)
	}
	r.published = nil
	if n := r.dispatch(); n != 1 || len(r.published) != 1 || !strings.HasSuffix(r.published[0], `/set {"dps":{"1":false}}`) {
		t.Fatalf("dispatched %d: %v", n, r.published)
	}
	if n := r.count(`SELECT count(*) FROM core.device_commands WHERE automation_id=$1 AND status='confirmed'`, flowID); n != 1 {
		t.Fatal("the switch did not confirm the automation's command")
	}
}

// A member restricted to another project neither discovers nor commands an Edge's Tuya devices.
func TestEdgeProjectScope(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, ownerAuth, owner := f.account(t)
	api := busyAPI(f)
	projectA, e := f.service.CreateProject(ctx, owner, "A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	projectB, e := f.service.CreateProject(ctx, owner, "B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	g, _, e := f.service.CreateGatewayIn(ctx, owner, "Edge B", domain.EdgeGatewayModel, &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile("../internal/adapters/tuya/testdata/kg_3gang.json")
	dps, category, e := tuya.ParseSpecifications(raw)
	if e != nil {
		t.Fatal(e)
	}
	id := "bf00000000000000pr01"
	if _, e := f.repo.SaveTuyaDevices(ctx, owner, g.ID, []postgres.TuyaImport{{TuyaID: id, Name: "B switch", Category: category, Spec: dps, LocalKeySealed: "sealed", KeyFingerprint: "00"}}); e != nil {
		t.Fatal(e)
	}
	d, e := f.service.CreateDevice(ctx, owner, g.ID, "B switch", id, domain.TuyaWiFiProfile)
	if e != nil {
		t.Fatal(e)
	}
	email := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, email, "operator", []string{projectA.ID})
	memberAuth, member := changeInitialPassword(t, f, api, email, owner.TenantID)
	if _, e := f.repo.SaveTuyaDevices(ctx, member, g.ID, nil); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("import into another project's Edge: %v", e)
	}
	code, out := get(t, api, "/api/v1/discovery", memberAuth.AccessToken)
	if code != 200 {
		t.Fatalf("discovery: %d", code)
	}
	for _, raw := range out["items"].([]any) {
		if s := raw.(map[string]any)["source"]; s == "tuya" || s == "tuya_lan" {
			t.Fatal("restricted member discovered another project's Tuya device")
		}
	}
	body, _ := json.Marshal(map[string]any{"device_id": d.ID, "property": "switch_1", "value": "ON"})
	q := httptest.NewRequest("POST", "/api/v1/commands", bytes.NewReader(body))
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Authorization", "Bearer "+memberAuth.AccessToken)
	q.Header.Set("Idempotency-Key", uuid.NewString())
	res, e := api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("command to another project's Tuya device: %d", res.StatusCode)
	}
	var rows int
	if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1`, g.ID).Scan(&rows); e != nil || rows != 1 {
		t.Fatalf("tuya rows: %d %v", rows, e)
	}
}
