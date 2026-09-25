package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"encoding/json"
	"testing"
	"time"
)

// zigbeeRig is a Zigbee2MQTT gateway fed by the virtual bridge, whose sensors carry the real definitions from
// zigbee-herdsman-converters.
type zigbeeRig struct {
	t      *testing.T
	f      *fixture
	ctx    context.Context
	owner  domain.Principal
	token  string
	g      domain.Gateway
	bridge *simulation.FakeBridge
}

func newZigbeeRig(t *testing.T, shadow bool) *zigbeeRig {
	f := setup(t)
	if shadow {
		f.repo.Configure(postgres.Options{AlertsShadow: true, DiscoveryLimit: 100})
	}
	ctx := context.Background()
	_, auth, owner := f.account(t)
	g, _, e := f.service.CreateGateway(ctx, owner, "Zigbee", domain.Z2MGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	r := &zigbeeRig{t: t, f: f, ctx: ctx, owner: owner, token: auth.AccessToken, g: g, bridge: simulation.NewFakeBridge(zigbee2mqtt.BaseTopic(g.ID))}
	r.publish(r.bridge.Step(0))
	return r
}

func (r *zigbeeRig) publish(messages []simulation.Z2MMessage) {
	r.t.Helper()
	for _, m := range messages {
		gateway, msg, e := zigbee2mqtt.Route(m.Topic)
		if e != nil {
			r.t.Fatalf("%s: %v", m.Topic, e)
		}
		if _, e := r.f.service.CaptureZ2M(r.ctx, r.owner.TenantID, gateway, msg, m.Payload); e != nil {
			r.t.Fatalf("%s: %v", m.Topic, e)
		}
	}
}

func (r *zigbeeRig) register(a simulation.Z2MActuator) {
	r.t.Helper()
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.g.ID, a.Name, a.IEEE, domain.Z2MGenericProfile); e != nil {
		r.t.Fatalf("register %s: %v", a.Model, e)
	}
}

func (r *zigbeeRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	if e := r.f.admin.QueryRowContext(r.ctx, query, args...).Scan(&n); e != nil {
		r.t.Fatal(e)
	}
	return n
}

func (r *zigbeeRig) events(a simulation.Z2MActuator, eventType string) int {
	return r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type=$3`, r.g.ID, a.IEEE, eventType)
}

func (r *zigbeeRig) alerts(a simulation.Z2MActuator, eventType string) int {
	return r.count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id=$2 AND event_type=$3`, r.g.ID, a.IEEE, eventType)
}

func sensor(model string) simulation.Z2MActuator {
	for _, s := range simulation.Z2MSensors {
		if s.Model == model {
			return s
		}
	}
	panic(model)
}

// Every sensor kind the generic ingest maps, through CaptureZ2M: discovery shows each with its category and
// definition, and once registered their readings drive the existing canonical events and rules.
func TestZigbeeGenericSensors(t *testing.T) {
	r := newZigbeeRig(t, false)
	api := busyAPI(r.f)
	temp, door, pir, leak, smoke := sensor("WSDCGQ11LM"), sensor("MCCGQ11LM"), sensor("RTCGQ11LM"), sensor("SJCGQ11LM"), sensor("JTYJ-GD-01LM/BW")

	code, out := get(t, api, "/api/v1/discovery", r.token)
	if code != 200 {
		t.Fatalf("discovery: %d %v", code, out)
	}
	found := map[string]map[string]any{}
	for _, raw := range out["items"].([]any) {
		item := raw.(map[string]any)
		found[item["external_id"].(string)] = item
	}
	for model, kind := range map[string]string{"WSDCGQ11LM": "environment", "MCCGQ11LM": "door", "RTCGQ11LM": "occupancy", "SJCGQ11LM": "leak", "TS0215A_sos": "sos", "E1743": "remote", "JTYJ-GD-01LM/BW": "hazard"} {
		item := found[sensor(model).IEEE]
		p, _ := item["profile"].(map[string]any)
		if item == nil || item["kind"] != kind || item["vendor"] == "" || item["description"] == "" || p == nil || p["id"] != domain.Z2MGenericProfile {
			t.Fatalf("%s discovered as %v", model, item)
		}
	}

	for _, s := range []simulation.Z2MActuator{temp, door, pir, leak, smoke} {
		r.register(s)
	}
	r.publish(r.bridge.Step(1)) // the registered devices' first reports: a baseline, no edges

	// Temperature: stored in the reading's metrics with the category as its kind, and a threshold rule on it fires.
	var reading []byte
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT reading FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 ORDER BY received_at DESC LIMIT 1`, r.g.ID, temp.IEEE).Scan(&reading); e != nil {
		t.Fatal(e)
	}
	var stored struct {
		Kind    string             `json:"kind"`
		Battery int                `json:"battery"`
		Metrics map[string]float64 `json:"metrics"`
	}
	if json.Unmarshal(reading, &stored) != nil || stored.Kind != "environment" || stored.Metrics["temperature"] != 24.6 || stored.Metrics["humidity"] != 55.2 || stored.Battery != 91 {
		t.Fatalf("stored reading: %s", reading)
	}
	code, rule, _ := req(t, api, "POST", "/api/v1/rules", "Bearer "+r.token, "", "", map[string]any{"name": "ร้อน", "event_type": "threshold", "severity": "warning", "channels": []string{}, "dedupe_sec": 60,
		"scope": map[string]any{"metric": "temperature", "op": ">", "value": 30}})
	if code != 201 {
		t.Fatalf("threshold rule: %d %v", code, rule)
	}
	r.publish(r.bridge.Set(temp.IEEE, "temperature", 31.5))
	if n := r.events(temp, domain.EventThreshold); n != 1 {
		t.Fatalf("threshold events: %d", n)
	}

	// Door: contact=false (magnet away) is open.
	r.publish(r.bridge.Set(door.IEEE, "contact", false))
	r.publish(r.bridge.Set(door.IEEE, "contact", true))
	if o, c := r.events(door, domain.EventDoorOpen), r.events(door, domain.EventDoorClosed); o != 1 || c != 1 {
		t.Fatalf("door events: open %d closed %d", o, c)
	}
	// PIR: occupancy drives the same occupied path as the Minew MSP01.
	r.publish(r.bridge.Set(pir.IEEE, "occupancy", true))
	if n := r.events(pir, domain.EventOccupied); n != 1 {
		t.Fatalf("occupied events: %d", n)
	}
	// Water leak.
	r.publish(r.bridge.Set(leak.IEEE, "water_leak", true))
	if n := r.events(leak, domain.EventLeak); n != 1 {
		t.Fatalf("leak events: %d", n)
	}
	// Smoke: a hazard event, and the built-in critical hazard rule opens an alert.
	r.publish(r.bridge.Set(smoke.IEEE, "smoke", true))
	r.publish(r.bridge.Set(smoke.IEEE, "smoke", true))
	if n, a := r.events(smoke, domain.EventHazard), r.alerts(smoke, domain.EventHazard); n != 1 || a != 1 {
		t.Fatalf("hazard: %d events, %d alerts", n, a)
	}
	if n := r.count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id=$2 AND severity='critical'`, r.g.ID, smoke.IEEE); n != 1 {
		t.Fatalf("hazard alert severity: %d critical", n)
	}
	r.publish(r.bridge.Set(smoke.IEEE, "smoke", false))
	if n := r.events(smoke, domain.EventHazardCleared); n != 1 {
		t.Fatalf("hazard cleared: %d", n)
	}
}

// A Zigbee SOS button rings exactly like the B10, shadow mode or not; an ordinary remote's presses are only
// action events; an SOS button that is not registered never rings.
func TestZigbeeSOSButtonAndRemote(t *testing.T) {
	r := newZigbeeRig(t, true)
	base := zigbee2mqtt.BaseTopic(r.g.ID)
	sos, remote, smoke := sensor("TS0215A_sos"), sensor("E1743"), sensor("JTYJ-GD-01LM/BW")

	// Not registered yet: the press is logged as an action but is not an SOS.
	r.publish([]simulation.Z2MMessage{simulation.Z2MPress(base, sos, "emergency")})
	if b, a := r.events(sos, domain.EventButton), r.events(sos, domain.EventAction); b != 0 || a != 1 {
		t.Fatalf("unregistered SOS: %d button, %d action", b, a)
	}

	r.register(sos)
	r.register(remote)
	r.register(smoke)
	r.publish([]simulation.Z2MMessage{simulation.Z2MPress(base, sos, "emergency")})
	if n := r.events(sos, domain.EventButton); n != 1 {
		t.Fatalf("SOS press: %d button events", n)
	}
	if n := r.count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id=$2 AND event_type='button' AND severity='critical'`, r.g.ID, sos.IEEE); n != 1 {
		t.Fatalf("SOS alert in shadow mode: %d", n)
	}

	for _, action := range []string{"on", "off", "brightness_move_up"} {
		r.publish([]simulation.Z2MMessage{simulation.Z2MPress(base, remote, action)})
	}
	if a, b := r.events(remote, domain.EventAction), r.events(remote, domain.EventButton); a != 3 || b != 0 {
		t.Fatalf("remote: %d actions, %d button", a, b)
	}
	var detail []byte
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT detail FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='action' ORDER BY occurred_at DESC LIMIT 1`, r.g.ID, remote.IEEE).Scan(&detail); e != nil || string(detail) != `{"action": "brightness_move_up"}` {
		t.Fatalf("action detail: %s %v", detail, e)
	}
	if n := r.count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id=$2`, r.g.ID, remote.IEEE); n != 0 {
		t.Fatalf("remote opened %d alerts", n)
	}

	// Shadow mode silences everything but life safety: a smoke alarm opens its critical alert like SOS does;
	// clearing it is an event only.
	r.publish(r.bridge.Set(smoke.IEEE, "smoke", true))
	if n, a := r.events(smoke, domain.EventHazard), r.alerts(smoke, domain.EventHazard); n != 1 || a != 1 {
		t.Fatalf("hazard in shadow mode: %d events, %d alerts", n, a)
	}
	if n := r.count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id=$2 AND severity='critical'`, r.g.ID, smoke.IEEE); n != 1 {
		t.Fatalf("hazard alert severity in shadow mode: %d critical", n)
	}
	r.publish(r.bridge.Set(smoke.IEEE, "smoke", false))
	if n, a := r.events(smoke, domain.EventHazardCleared), r.count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id=$2`, r.g.ID, smoke.IEEE); n != 1 || a != 1 {
		t.Fatalf("hazard cleared in shadow mode: %d events, %d alerts", n, a)
	}
	// Other alerts stay silent in shadow mode: the water leak is recorded without an alert.
	leak := sensor("SJCGQ11LM")
	r.register(leak)
	r.publish(r.bridge.Step(1))
	r.publish(r.bridge.Set(leak.IEEE, "water_leak", true))
	if n := r.count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id=$2`, r.g.ID, leak.IEEE); n != 0 || r.events(leak, domain.EventLeak) != 1 {
		t.Fatalf("leak in shadow mode opened %d alerts", n)
	}
}

// An SOS button paired before categories existed (its row has exposes but no category or SOS flag) rings on its
// first press after the upgrade, without waiting for bridge/devices to be republished.
func TestZigbeeSOSPairedBeforeUpgrade(t *testing.T) {
	r := newZigbeeRig(t, true)
	sos := sensor("TS0215A_sos")
	r.register(sos)
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.z2m_devices SET category='',sos=false WHERE gateway_id=$1 AND ieee=$2`, r.g.ID, sos.IEEE); e != nil {
		t.Fatal(e)
	}
	r.publish([]simulation.Z2MMessage{simulation.Z2MPress(zigbee2mqtt.BaseTopic(r.g.ID), sos, "emergency")})
	if n := r.events(sos, domain.EventButton); n != 1 {
		t.Fatalf("SOS paired before the upgrade: %d button events", n)
	}
	if n := r.count(`SELECT count(*) FROM core.z2m_devices WHERE gateway_id=$1 AND ieee=$2 AND sos AND category='sos'`, r.g.ID, sos.IEEE); n != 1 {
		t.Fatal("derived category not persisted")
	}
}

// A rotary remote firing hundreds of actions is coalesced and capped, and never evicts other history.
func TestZigbeeActionFloodIsBounded(t *testing.T) {
	r := newZigbeeRig(t, false)
	base := zigbee2mqtt.BaseTopic(r.g.ID)
	remote, door := sensor("E1743"), sensor("MCCGQ11LM")
	r.register(remote)
	r.register(door)
	r.publish(r.bridge.Step(1))
	r.publish(r.bridge.Set(door.IEEE, "contact", false))
	start := time.Now()
	for i := 0; i < 200; i++ {
		action := "brightness_move_up"
		if i%2 == 1 {
			action = "brightness_stop"
		}
		r.publish([]simulation.Z2MMessage{simulation.Z2MPress(base, remote, action)})
	}
	if n := r.events(remote, domain.EventAction); n < 1 || n > 30 {
		t.Fatalf("200 rapid actions kept %d events", n)
	}
	// Coalescing: the same action repeated within 2 s is one event, so a burst yields at most one per started
	// 2 s window. Bound by the burst's real duration: 200 ingests take longer than 2 s on a loaded machine.
	windows := int(time.Since(start)/(2*time.Second)) + 1
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND detail->>'action'='brightness_move_up'`, r.g.ID, remote.IEEE); n < 1 || n > windows {
		t.Fatalf("identical actions within 2 s: %d events over %d window(s)", n, windows)
	}
	// Pruning takes action events first: with the action budget exceeded the door history survives.
	if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.device_events(tenant_id,id,gateway_id,external_id,device_name,event_type,detail,occurred_at)
    SELECT $1,gen_random_uuid(),$2,$3,'r','action',jsonb_build_object('action','x'||g),now()-make_interval(secs => g) FROM generate_series(1,1500) g`, r.owner.TenantID, r.g.ID, remote.IEEE); e != nil {
		t.Fatal(e)
	}
	if e := r.f.repo.PruneAlertData(r.ctx, r.owner.TenantID); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND event_type='action'`, r.g.ID); n > 1000 {
		t.Fatalf("action budget: %d", n)
	}
	if r.events(door, domain.EventDoorOpen) != 1 {
		t.Fatal("door history evicted")
	}
}

// Thinning: a plug reporting power every second keeps one sample per SAMPLE_MIN_INTERVAL_SEC; a door change
// is always stored.
func TestZigbeeThinning(t *testing.T) {
	r := newZigbeeRig(t, false)
	r.f.repo.Configure(postgres.Options{SampleMinIntervalSec: 30, DiscoveryLimit: 100})
	base := zigbee2mqtt.BaseTopic(r.g.ID)
	door := sensor("MCCGQ11LM")
	r.register(door)
	plug := simulation.Z2MSwitches[0]
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.g.ID, "plug", plug.IEEE, "tuya-ts001x-switch@1"); e != nil {
		t.Fatal(e)
	}
	samples := func(ieee string) int {
		return r.count(`SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2`, r.g.ID, ieee)
	}
	// The bridge's announcement stored the plug OFF; switching it ON is one more sample, the 19 identical
	// reports after it within the interval none.
	start := samples(plug.IEEE)
	state := simulation.Z2MMessage{Topic: base + "/" + plug.Name, Payload: simulation.Z2MState(plug, []bool{true}, 100)}
	for i := 0; i < 20; i++ {
		r.publish([]simulation.Z2MMessage{state})
	}
	if n := samples(plug.IEEE) - start; n != 1 {
		t.Fatalf("20 identical reports within the interval stored %d samples", n)
	}
	// A gang change is state: stored at once, and its switch event raised.
	r.publish([]simulation.Z2MMessage{{Topic: base + "/" + plug.Name, Payload: simulation.Z2MState(plug, []bool{false}, 100)}})
	if n := samples(plug.IEEE) - start; n != 2 || r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='switch_off'`, r.g.ID, plug.IEEE) != 1 {
		t.Fatalf("gang change: %d samples", n)
	}
	r.publish(r.bridge.Set(door.IEEE, "battery", 88))
	before := samples(door.IEEE)
	r.publish(r.bridge.Set(door.IEEE, "contact", false))
	if n := samples(door.IEEE); n != before+1 || r.events(door, domain.EventDoorOpen) != 1 {
		t.Fatalf("door change not stored: %d → %d", before, n)
	}
}

func TestZigbeeCatalogSearch(t *testing.T) {
	f := setup(t)
	_, auth, _ := f.account(t)
	api := busyAPI(f)
	code, out := get(t, api, "/api/v1/catalog/zigbee?q=WSDCGQ11LM&vendors=1", auth.AccessToken)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) == 0 || out["license"] != "MIT" || out["source"] != "zigbee-herdsman-converters" || out["notice"] == nil || len(out["vendors"].([]any)) < 100 {
		t.Fatalf("catalog: %d %v", code, out)
	}
	first := items[0].(map[string]any)
	if first["model"] != "WSDCGQ11LM" || first["vendor"] != "Aqara" || first["category"] != "environment" {
		t.Fatalf("first: %v", first)
	}
	code, out = get(t, api, "/api/v1/catalog/zigbee?category=sos&limit=200", auth.AccessToken)
	if code != 200 || out["total"].(float64) < 5 {
		t.Fatalf("sos category: %d %v", code, out["total"])
	}
	for _, raw := range out["items"].([]any) {
		if d := raw.(map[string]any); d["category"] != "sos" || d["sos"] != true {
			t.Fatalf("category filter: %v", d)
		}
	}
	if code, _ := get(t, api, "/api/v1/catalog/zigbee?limit=0", auth.AccessToken); code != 400 {
		t.Fatalf("limit 0: %d", code)
	}
	if code, _ := get(t, api, "/api/v1/catalog/zigbee", ""); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
}
