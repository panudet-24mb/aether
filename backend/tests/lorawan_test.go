package tests

import (
	"aether/backend/internal/adapters/milesight"
	"aether/backend/internal/domain"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// ug65Envelope is a Milesight UG65 uplink as its embedded network server publishes it for a device without a payload
// codec: the raw bytes in `data`.
func ug65Envelope(eui, name, hexFrame string) []byte {
	raw, _ := hex.DecodeString(hexFrame)
	return []byte(`{"applicationID":1,"applicationName":"Building A","deviceName":"` + name + `","devEUI":"` + eui + `","time":"2026-10-26T12:39:05.547336Z",
"rxInfo":[{"mac":"24e124fffef021be","rssi":-80,"loRaSNR":6.5,"name":"local_gateway"}],"fCnt":7,"fPort":85,"data":"` + base64.StdEncoding.EncodeToString(raw) + `"}`)
}

// Milesight LoRaWAN through a UG65's embedded network server, phase 1: enrolment hands over the uplink topic, uplinks
// list the devices for discovery with the profile their names or fields point at, codec output and raw frames both
// become readings that feed leak and occupancy events, and a quiet sensor is offline only through silence.
func TestLoRaWANIngest(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, auth, owner := f.account(t)
	api := busyAPI(f)
	t.Setenv("MQTT_PUBLIC_HOST", "mqtt.example.test")
	t.Setenv("MQTT_PUBLIC_PORT", "8883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "ssl")

	g, _, e := f.service.CreateGateway(ctx, owner, "UG65 อาคาร A", domain.LoRaWANGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	code, creds, _ := req(t, api, "POST", "/api/v1/gateways/"+g.ID+"/mqtt", "Bearer "+auth.AccessToken, "", "", map[string]any{})
	if code != 201 || creds["uplink_topic"] != "aether/lora/"+g.ID+"/$deveui/up" || creds["downlink_topic"] != milesight.DownlinkTopic(g.ID) || creds["post_topic"] != nil || creds["password"] == "" {
		t.Fatalf("enrolment: %d %v", code, creds)
	}

	up := func(eui string) string { return milesight.Prefix + g.ID + "/" + eui + "/up" }
	capture := func(topic string, payload []byte) error {
		gateway, m, e := milesight.Route(topic)
		if e != nil {
			return e
		}
		_, e = f.service.CaptureLoRaWAN(ctx, owner.TenantID, gateway, m, payload)
		return e
	}
	const leak, pir, odor, stall, other = "24e124136d490153", "24e124538b223311", "24e124710c401234", "24e124326c215678", "a84041000181c0de"
	for eui, payload := range map[string][]byte{
		leak:  []byte(`{"battery":100,"leakage_status":"normal"}`),                            // codec on the gateway
		pir:   []byte(`{"battery":90,"pir":"normal","daylight":"bright"}`),                    // codec on the gateway
		odor:  ug65Envelope(odor, "GS301 ห้องน้ำ 3F", "01756402671c01036864047d0000057d0100"), // no codec: Milesight's README frame
		stall: ug65Envelope(stall, "VS330 ห้องน้ำชาย 1", "038e00048e01"),                      // no codec, vacant
		other: ug65Envelope(other, "Dragino LHT65", "0bb80a2b01"),
	} {
		if e := capture(up(eui), payload); e != nil {
			t.Fatalf("%s: %v", eui, e)
		}
	}

	// Discovery lists all four, with the profile their fields or names matched, or the generic LoRaWAN profile.
	code, out := get(t, api, "/api/v1/discovery", auth.AccessToken)
	if code != 200 {
		t.Fatalf("discovery: %d %v", code, out)
	}
	profiles := map[string]string{}
	for _, raw := range out["items"].([]any) {
		item := raw.(map[string]any)
		if item["gateway_id"] == g.ID && item["source"] == "lorawan" {
			p, _ := item["profile"].(map[string]any)
			profiles[item["external_id"].(string)], _ = p["id"].(string)
		}
	}
	if len(profiles) != 5 || profiles[leak] != "milesight-ws303@1" || profiles[pir] != "milesight-ws202@1" || profiles[odor] != "milesight-gs301@1" || profiles[stall] != "milesight-vs330@1" || profiles[other] != domain.LoRaWANGenericProfile {
		t.Fatalf("lorawan discovery: %v", profiles)
	}
	count := func(q string, args ...any) int {
		var n int
		if e := f.admin.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	// The raw GS301 frame was decoded by Aether, with the envelope's radio figures; the unknown model was not.
	var gas float64
	if e := f.admin.QueryRowContext(ctx, `SELECT (reading->'metrics'->>'h2s')::float8 FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 ORDER BY received_at DESC LIMIT 1`, g.ID, odor).Scan(&gas); e != nil || gas != 0.01 {
		t.Fatalf("gs301 sample: %v %v", gas, e)
	}
	if n := count(`SELECT count(*) FROM core.lorawan_devices WHERE gateway_id=$1 AND dev_eui=$2 AND rssi=-80 AND f_cnt=7 AND model='GS301' AND name='GS301 ห้องน้ำ 3F' AND decoded`, g.ID, odor); n != 1 {
		t.Fatal("gs301 device row")
	}
	if n := count(`SELECT count(*) FROM core.lorawan_devices WHERE gateway_id=$1 AND dev_eui=$2 AND NOT decoded`, g.ID, other); n != 1 {
		t.Fatal("an unknown model's raw frame was decoded")
	}

	// A real GS301 through a UG65 with its codec on, warming up: the name the gateway adds is kept, its metadata is
	// not a reading, and the 655.34 ppm the codec makes of the warm-up status is a status, not a gas level.
	const toilet = "24e124798e210951"
	if e := capture(up(toilet), []byte(`{"h2s":655.34,"nh3":655.34,"devEUI":"24e124798e210951","battery":69,"humidity":58,"deviceName":"1F_Toilet_Male_Odor1","temperature":29.1,"applicationID":2}`)); e != nil {
		t.Fatal(e)
	}
	if n := count(`SELECT count(*) FROM core.lorawan_devices WHERE gateway_id=$1 AND dev_eui=$2 AND name='1F_Toilet_Male_Odor1' AND model='GS301'`, g.ID, toilet); n != 1 {
		t.Fatal("ug65 codec metadata: name or model not kept")
	}
	if n := count(`SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 AND reading->'metrics'->>'temperature'='29.1'
      AND NOT reading->'metrics' ?| array['h2s','nh3','applicationID'] AND reading->'values'->>'h2s_sensor_status'='polarizing'`, g.ID, toilet); n != 1 {
		var got string
		_ = f.admin.QueryRowContext(ctx, `SELECT reading::text FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 ORDER BY received_at DESC LIMIT 1`, g.ID, toilet).Scan(&got)
		t.Fatalf("ug65 codec sample: %s", got)
	}

	// Only LoRaWAN profiles fit a LoRaWAN gateway.
	if _, e := f.service.CreateDevice(ctx, owner, g.ID, "ผิดชนิด", leak, "minew-s1-pending@1"); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("BLE profile on a LoRaWAN gateway: %v", e)
	}
	for eui, profile := range map[string]string{leak: "milesight-ws303@1", pir: "milesight-ws202@1", odor: "milesight-gs301@1", stall: "milesight-vs330@1"} {
		if _, e := f.service.CreateDevice(ctx, owner, g.ID, "Milesight "+eui[12:], eui, profile); e != nil {
			t.Fatal(e)
		}
	}

	// Water is found (a raw frame: the registered WS303 profile says how to read channel 0x03) and then gone (codec);
	// someone walks past the PIR (a raw frame: the same channel means PIR on a WS202); someone enters the stall.
	for _, c := range []struct {
		eui     string
		payload []byte
	}{
		{leak, ug65Envelope(leak, "", "017564030001")},
		{leak, []byte(`{"battery":100,"leakage_status":"normal"}`)},
		{pir, ug65Envelope(pir, "", "017550030001040000")},
		{stall, ug65Envelope(stall, "", "01756202822c01038e01")},
	} {
		if e := capture(up(c.eui), c.payload); e != nil {
			t.Fatal(e)
		}
	}
	events := func(eui, kind string) int {
		return count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type=$3`, g.ID, eui, kind)
	}
	if events(leak, domain.EventLeak) != 1 || events(leak, domain.EventLeakCleared) != 1 || events(pir, domain.EventOccupied) != 1 || events(stall, domain.EventOccupied) != 1 {
		t.Fatalf("events: leak %d cleared %d occupied %d stall %d", events(leak, domain.EventLeak), events(leak, domain.EventLeakCleared), events(pir, domain.EventOccupied), events(stall, domain.EventOccupied))
	}

	// Silence is how a LoRaWAN sensor goes offline: the scan applies to it (a 2-hour-old uplink, a 1-hour rule).
	code, out, _ = req(t, api, "POST", "/api/v1/rules", "Bearer "+auth.AccessToken, "", "", map[string]any{"name": "lora offline", "event_type": "offline", "severity": "warning", "channels": []string{}, "dedupe_sec": 60, "scope": map[string]any{"offline_after_sec": 3600}})
	if code != 201 {
		t.Fatalf("offline rule: %d %v", code, out)
	}
	if n := count(`SELECT count(*) FROM core.sensor_streams WHERE gateway_id=$1 AND liveness<>'silence'`, g.ID); n != 0 {
		t.Fatalf("%d LoRaWAN streams with reported liveness", n)
	}
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.sensor_streams SET last_seen=now()-interval '2 hours' WHERE gateway_id=$1 AND external_id=$2`, g.ID, odor); e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.ScanOffline(ctx, owner.TenantID, time.Now()); e != nil {
		t.Fatal(e)
	}
	if events(odor, domain.EventOffline) != 1 || events(leak, domain.EventOffline) != 0 {
		t.Fatalf("offline: odor %d leak %d", events(odor, domain.EventOffline), events(leak, domain.EventOffline))
	}

	// A downlink is only a diagnostic copy; an envelope for another device than the topic, another model's gateway
	// and a revoked one are refused.
	if e := capture(milesight.Prefix+g.ID+"/"+leak+"/down", []byte(`{"confirmed":false,"fPort":85,"data":"BQID"}`)); e != nil {
		t.Fatalf("downlink copy: %v", e)
	}
	if e := capture(up(pir), ug65Envelope(leak, "", "017564")); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("mismatched DevEUI: %v", e)
	}
	ble, _, e := f.service.CreateGateway(ctx, owner, "MG3", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.service.CaptureLoRaWAN(ctx, owner.TenantID, ble.ID, milesight.Message{Kind: milesight.Up, Topic: "x", DevEUI: leak}, []byte(`{"battery":1}`)); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("LoRaWAN uplink for a BLE gateway: %v", e)
	}
	if e := f.service.RevokeGateway(ctx, owner, g.ID); e != nil {
		t.Fatal(e)
	}
	if e := capture(up(leak), []byte(`{"battery":1}`)); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("revoked gateway: %v", e)
	}
}
