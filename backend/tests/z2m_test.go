package tests

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Zigbee2MQTT gateway, phase 1: enrolment hands over the Z2M configuration, bridge/devices feeds discovery,
// an adopted wall switch stores every state report and raises one event per gang change, availability drives
// offline/online, and the silence-based offline scan leaves the switch alone.
func TestZigbee2MQTTIngest(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, auth, owner := f.account(t)
	api := busyAPI(f)
	t.Setenv("MQTT_PUBLIC_HOST", "mqtt.example.test")
	t.Setenv("MQTT_PUBLIC_PORT", "8883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "ssl")

	g, _, e := f.service.CreateGateway(ctx, owner, "Zigbee ชั้น 2", domain.Z2MGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	code, creds, _ := req(t, api, "POST", "/api/v1/gateways/"+g.ID+"/mqtt", "Bearer "+auth.AccessToken, "", "", map[string]any{})
	base := zigbee2mqtt.BaseTopic(g.ID)
	yaml, _ := creds["z2m_yaml"].(string)
	if code != 201 || creds["base_topic"] != base || creds["server"] != "mqtts://mqtt.example.test:8883" || creds["post_topic"] != nil ||
		!strings.Contains(yaml, "base_topic: "+base) || !strings.Contains(yaml, "password: '"+creds["password"].(string)+"'") {
		t.Fatalf("enrolment: %d %v", code, creds)
	}

	capture := func(topic string, payload []byte) error {
		gateway, m, e := zigbee2mqtt.Route(topic)
		if e != nil {
			return e
		}
		_, e = f.service.CaptureZ2M(ctx, owner.TenantID, gateway, m, payload)
		return e
	}
	for _, m := range simulation.Z2MMessages(base, 0, true) {
		if e := capture(m.Topic, m.Payload); e != nil {
			t.Fatalf("%s: %v", m.Topic, e)
		}
	}

	// Discovery lists what is paired to the coordinator (not the coordinator itself), with the switch profile.
	code, out := get(t, api, "/api/v1/discovery", auth.AccessToken)
	if code != 200 {
		t.Fatalf("discovery: %d %v", code, out)
	}
	found := map[string]map[string]any{}
	for _, raw := range out["items"].([]any) {
		item := raw.(map[string]any)
		if item["gateway_id"] == g.ID {
			found[item["external_id"].(string)] = item
		}
	}
	ts0012 := found["0xa4c1380000000002"]
	if len(found) != 4 || ts0012 == nil || ts0012["source"] != "z2m" || ts0012["kind"] != "switch" || ts0012["model"] != "TS0012" {
		t.Fatalf("z2m discovery: %v", found)
	}
	if p, _ := ts0012["profile"].(map[string]any); p == nil || p["id"] != "tuya-ts001x-switch@1" {
		t.Fatalf("switch profile: %v", ts0012)
	}
	if found["0x00158d00000000ff"]["profile"] != nil {
		t.Fatal("unsupported device matched a profile")
	}

	// A BLE profile cannot sit on a Zigbee gateway, nor a Zigbee switch on a BLE gateway.
	if _, e := f.service.CreateDevice(ctx, owner, g.ID, "ผิดชนิด", "0xa4c1380000000001", "minew-s1-pending@1"); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("BLE profile on a Zigbee gateway: %v", e)
	}
	ble, _, e := f.service.CreateGateway(ctx, owner, "MG3", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.service.CreateDevice(ctx, owner, ble.ID, "ผิดชนิด", "0xa4c1380000000001", "tuya-ts001x-switch@1"); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("Zigbee profile on a BLE gateway: %v", e)
	}
	if _, e := f.service.CreateDevice(ctx, owner, g.ID, "ไฟห้องประชุม", "0xa4c1380000000002", "tuya-ts001x-switch@1"); e != nil {
		t.Fatal(e)
	}

	// The simulated TS0012 lists its endpoints right, left, so on[0] is the right output, which is gang 2 on the
	// wall. Step 0 reported right OFF, left ON (the baseline). Right is switched ON, OFF and ON again: the first
	// and third payloads are byte-identical and must both be stored.
	sw := simulation.Z2MSwitches[1]
	for _, on := range [][]bool{{true, true}, {false, true}, {true, true}, {true, true}} {
		if e := capture(simulation.Z2MTopic(base, sw, ""), simulation.Z2MState(sw, on, 110)); e != nil {
			t.Fatal(e)
		}
	}
	count := func(q string, args ...any) int {
		var n int
		if e := f.admin.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2`, g.ID, sw.IEEE); n != 5 {
		t.Fatalf("samples stored: %d", n)
	}
	if n := count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type IN ('switch_on','switch_off') AND detail->>'gang'='2'`, g.ID, sw.IEEE); n != 3 {
		t.Fatalf("switch events: %d", n)
	}
	if n := count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND event_type='button'`, g.ID); n != 0 {
		t.Fatal("a wall switch raised a button event")
	}

	// The live view shows the switch with its gang states and its reported (not silence-based) liveness.
	code, out = get(t, api, "/api/v1/live?range=1h", auth.AccessToken)
	if code != 200 {
		t.Fatalf("live: %d", code)
	}
	var live map[string]any
	for _, raw := range out["gateways"].([]any) {
		view := raw.(map[string]any)
		if view["gateway"].(map[string]any)["id"] != g.ID {
			continue
		}
		for _, s := range view["sensors"].([]any) {
			if s.(map[string]any)["id"] == sw.IEEE {
				live = s.(map[string]any)
			}
		}
	}
	metrics, _ := live["latest"].(map[string]any)["metrics"].(map[string]any)
	if live["kind"] != "switch" || live["liveness"] != "reported" || live["offline"] != false || metrics["sw1"] != float64(1) || metrics["sw2"] != float64(1) {
		t.Fatalf("live switch: %v", live)
	}

	// Availability drives offline / online, once per change.
	for _, online := range []bool{false, false, true} {
		if e := capture(simulation.Z2MTopic(base, sw, "availability"), simulation.Z2MOnline(online)); e != nil {
			t.Fatal(e)
		}
	}
	if off, on := count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='offline'`, g.ID, sw.IEEE), count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='online'`, g.ID, sw.IEEE); off != 1 || on != 1 {
		t.Fatalf("availability events: offline %d online %d", off, on)
	}
	// Silence is not offline for a switch: the periodic scan skips reported-liveness streams.
	code, out, _ = req(t, api, "POST", "/api/v1/rules", "Bearer "+auth.AccessToken, "", "", map[string]any{"name": "offline", "event_type": "offline", "severity": "warning", "channels": []string{}, "dedupe_sec": 60, "scope": map[string]any{"offline_after_sec": 60}})
	if code != 201 {
		t.Fatalf("offline rule: %d %v", code, out)
	}
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET last_seen=now()-interval '2 hours' WHERE gateway_id=$1`, g.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.ScanOffline(ctx, owner.TenantID, time.Now()); e != nil {
		t.Fatal(e)
	}
	if n := count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND event_type='offline'`, g.ID); n != 1 {
		t.Fatalf("silence made a switch offline: %d offline events", n)
	}

	// The bridge's own state, and a device removed from the coordinator leaves discovery.
	if e := capture(base+"/bridge/state", []byte("offline")); e != nil {
		t.Fatal(e)
	}
	if n := count(`SELECT count(*) FROM core.z2m_bridges WHERE gateway_id=$1 AND state='offline'`, g.ID); n != 1 {
		t.Fatal("bridge state not recorded")
	}
	if e := capture(base+"/bridge/devices", []byte(`[]`)); e != nil {
		t.Fatal(e)
	}
	if n := count(`SELECT count(*) FROM core.z2m_devices WHERE gateway_id=$1 AND removed_at IS NULL`, g.ID); n != 0 {
		t.Fatalf("removed devices still listed: %d", n)
	}

	// Commands to the bridge are not reports; a message for another model's gateway, or a revoked one, is refused.
	if _, m, _ := zigbee2mqtt.Route(base + "/0xa4c1380000000002/set"); m.Kind != zigbee2mqtt.Ignore {
		t.Fatal("set routed as a report")
	}
	if _, e := f.service.CaptureZ2M(ctx, owner.TenantID, ble.ID, zigbee2mqtt.Message{Kind: zigbee2mqtt.BridgeState, Topic: "x"}, []byte("online")); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("Z2M message for a BLE gateway: %v", e)
	}
	if e := f.service.RevokeGateway(ctx, owner, g.ID); e != nil {
		t.Fatal(e)
	}
	if e := capture(base+"/bridge/state", []byte("online")); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("revoked gateway: %v", e)
	}
}

// A member restricted to one project does not see the Zigbee devices of another project's coordinator.
func TestZigbee2MQTTProjectScope(t *testing.T) {
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
	g, _, e := f.service.CreateGatewayIn(ctx, owner, "Zigbee B", domain.Z2MGatewayModel, &projectB.ID)
	if e != nil {
		t.Fatal(e)
	}
	gateway, m, _ := zigbee2mqtt.Route(zigbee2mqtt.BaseTopic(g.ID) + "/bridge/devices")
	if _, e := f.service.CaptureZ2M(ctx, owner.TenantID, gateway, m, simulation.Z2MBridgeDevices()); e != nil {
		t.Fatal(e)
	}
	email := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, email, "operator", []string{projectA.ID})
	memberAuth, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
	for _, token := range []string{memberAuth.AccessToken, ownerAuth.AccessToken} {
		code, out := get(t, api, "/api/v1/discovery", token)
		if code != 200 {
			t.Fatalf("discovery: %d", code)
		}
		seen := 0
		for _, raw := range out["items"].([]any) {
			if raw.(map[string]any)["source"] == "z2m" {
				seen++
			}
		}
		if token == memberAuth.AccessToken && seen != 0 {
			t.Fatalf("restricted member saw %d Zigbee devices of another project", seen)
		}
		if token == ownerAuth.AccessToken && seen != 4 {
			t.Fatalf("owner saw %d Zigbee devices", seen)
		}
	}
}
