package tests

import (
	"aether/backend/internal/adapters/mqttingest"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// z2mSite is one enrolled Zigbee2MQTT gateway fed with the simulated bridge (step 0), with the TS0012 registered.
type z2mSite struct {
	f       *fixture
	owner   domain.Principal
	gateway string
	base    string
}

func newZ2MSite(t *testing.T, f *fixture, owner domain.Principal) z2mSite {
	t.Helper()
	g, _, e := f.service.CreateGateway(context.Background(), owner, "Zigbee", domain.Z2MGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	s := z2mSite{f: f, owner: owner, gateway: g.ID, base: zigbee2mqtt.BaseTopic(g.ID)}
	for _, m := range simulation.Z2MMessages(s.base, 0, true) {
		if e := s.capture(m.Topic, m.Payload); e != nil {
			t.Fatalf("%s: %v", m.Topic, e)
		}
	}
	if _, e := f.service.CreateDevice(context.Background(), owner, g.ID, "ไฟห้องประชุม", simulation.Z2MSwitches[1].IEEE, "tuya-ts001x-switch@1"); e != nil {
		t.Fatal(e)
	}
	return s
}

func (s z2mSite) capture(topic string, payload []byte) error {
	gateway, m, e := zigbee2mqtt.Route(topic)
	if e != nil {
		return e
	}
	_, e = s.f.service.CaptureZ2M(context.Background(), s.owner.TenantID, gateway, m, payload)
	return e
}

func (s z2mSite) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if e := s.f.admin.QueryRowContext(context.Background(), q, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func (s z2mSite) offline(t *testing.T, ieee string) bool {
	t.Helper()
	return s.count(t, `SELECT count(*) FROM core.stream_state WHERE gateway_id=$1 AND external_id=$2 AND offline`, s.gateway, ieee) == 1
}

// A NUL anywhere in a message can never be stored by PostgreSQL. It must be rejected as permanently invalid so the
// collector acknowledges and drops it, instead of retrying, exiting and getting the same message redelivered forever.
func TestZigbee2MQTTPoisonMessages(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, _, owner := f.account(t)
	s := newZ2MSite(t, f, owner)
	sw := simulation.Z2MSwitches[1]
	poison := map[string][]byte{
		simulation.Z2MTopic(s.base, sw, ""):             []byte(`{"state_left":"ON","note":"a\u0000b"}`),
		simulation.Z2MTopic(s.base, sw, "availability"): []byte("online\x00"),
		s.base + "/bridge/devices":                      []byte(`[{"ieee_address":"0xa4c1380000000009","type":"EndDevice","model_id":"TS0011\u0000"}]`),
	}
	for topic, payload := range poison {
		if e := s.capture(topic, payload); !errors.Is(e, domain.ErrInvalid) || !mqttingest.Permanent(e) {
			t.Fatalf("%s: %v", topic, e)
		}
	}
	// Defence in depth below the service: the database's own refusal (jsonb cannot hold \u0000) is also permanent.
	gateway, m, _ := zigbee2mqtt.Route(simulation.Z2MTopic(s.base, sw, ""))
	if _, e := f.repo.CaptureZ2M(ctx, owner.TenantID, gateway, m, []byte(`{"state_left":"ON","note":"\u0000"}`)); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("repository NUL in state: %v", e)
	}
	// NUL padding inside a device list is stripped rather than stored (the diagnostic copy keeps only a count).
	gateway, m, _ = zigbee2mqtt.Route(s.base + "/bridge/devices")
	padded := strings.Replace(string(simulation.Z2MBridgeDevices()), `"_TZ3000_simtwo"`, `"_TZ3000_simtwo\u0000\u0000"`, 1)
	if _, e := f.repo.CaptureZ2M(ctx, owner.TenantID, gateway, m, []byte(padded)); e != nil {
		t.Fatalf("padded device list: %v", e)
	}
	if n := s.count(t, `SELECT count(*) FROM core.z2m_devices WHERE gateway_id=$1 AND manufacturer='_TZ3000_simtwo'`, s.gateway); n != 1 {
		t.Fatal("NUL padding not stripped")
	}
	// The Minew path gets the same guarantee.
	ble, _, e := f.service.CreateGateway(ctx, owner, "MG3", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.CapturePacket(ctx, owner.TenantID, ble.ID, []byte(`[{"mac":"aabbccddeeff","rawData":"\u0000"}]`)); !errors.Is(e, domain.ErrInvalid) || !mqttingest.Permanent(e) {
		t.Fatalf("Minew NUL packet: %v", e)
	}
	// And the collector keeps working afterwards.
	if e := s.capture(simulation.Z2MTopic(s.base, sw, ""), simulation.Z2MState(sw, []bool{true, true}, 100)); e != nil {
		t.Fatal(e)
	}
}

// When the bridge itself goes (last will, or silence), its devices go offline with it; when it comes back, devices
// whose own last availability was online are restored, the others wait for their availability report.
func TestZigbee2MQTTBridgeLiveness(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, auth, owner := f.account(t)
	s := newZ2MSite(t, f, owner)
	two, four := simulation.Z2MSwitches[1], simulation.Z2MSwitches[2]
	// The four-gang switch had gone offline by itself before the bridge died.
	if e := s.capture(simulation.Z2MTopic(s.base, four, "availability"), simulation.Z2MOnline(false)); e != nil {
		t.Fatal(e)
	}
	events := func(ieee, source string) int {
		return s.count(t, `SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND detail->>'source'=$3`, s.gateway, ieee, source)
	}

	// Last will: bridge/state offline.
	if e := s.capture(s.base+"/bridge/state", simulation.Z2MOnline(false)); e != nil {
		t.Fatal(e)
	}
	if !s.offline(t, two.IEEE) || events(two.IEEE, "bridge_offline") != 1 || events(four.IEEE, "bridge_offline") != 0 {
		t.Fatalf("bridge offline did not propagate: offline=%v events=%d", s.offline(t, two.IEEE), events(two.IEEE, "bridge_offline"))
	}
	// A repeated last will is not a second episode.
	if e := s.capture(s.base+"/bridge/state", simulation.Z2MOnline(false)); e != nil {
		t.Fatal(e)
	}
	if events(two.IEEE, "bridge_offline") != 1 {
		t.Fatal("duplicate offline event")
	}
	// Back: the TS0012 (last availability online) is restored, the TS0014 (last availability offline) is not.
	if e := s.capture(s.base+"/bridge/state", simulation.Z2MOnline(true)); e != nil {
		t.Fatal(e)
	}
	if s.offline(t, two.IEEE) || events(two.IEEE, "bridge") != 1 || !s.offline(t, four.IEEE) {
		t.Fatalf("bridge online: two offline=%v four offline=%v", s.offline(t, two.IEEE), s.offline(t, four.IEEE))
	}

	// Silence without a last will: the gateway's last packet is older than Z2MSilentAfter.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.gateway_packets SET received_at=now()-interval '40 minutes' WHERE gateway_id=$1`, s.gateway); e != nil {
		t.Fatal(e)
	}
	n, e := f.repo.ScanOffline(ctx, owner.TenantID, time.Now())
	if e != nil || n < 1 || !s.offline(t, two.IEEE) || events(two.IEEE, "bridge_silent") != 1 {
		t.Fatalf("silent bridge: n=%d e=%v offline=%v", n, e, s.offline(t, two.IEEE))
	}
	if n, _ := f.repo.ScanOffline(ctx, owner.TenantID, time.Now()); events(two.IEEE, "bridge_silent") != 1 || n != 0 {
		t.Fatal("silent bridge scanned twice")
	}
	// The live view (overview and inspector read it) reports the switch offline.
	liveOffline := func() any {
		code, out := get(t, busyAPI(f), "/api/v1/live?range=1h", auth.AccessToken)
		if code != 200 {
			t.Fatalf("live: %d", code)
		}
		for _, raw := range out["gateways"].([]any) {
			for _, sensor := range raw.(map[string]any)["sensors"].([]any) {
				if sensor.(map[string]any)["id"] == two.IEEE {
					return sensor.(map[string]any)["offline"]
				}
			}
		}
		return nil
	}
	if liveOffline() != true {
		t.Fatal("live view does not show the silent bridge's switch offline")
	}
	// Any message proves the bridge is back (here a heartbeat).
	if e := s.capture(s.base+"/bridge/health", []byte(`{"response_time":1758700000000}`)); e != nil {
		t.Fatal(e)
	}
	if s.offline(t, two.IEEE) || liveOffline() != false {
		t.Fatal("device not restored after the bridge spoke again")
	}
}

// Unpairing a device from the coordinator hands its stream back to silence-based liveness, so it ages out; a device
// list cut at MaxDevices removes nothing.
func TestZigbee2MQTTDeviceRemoval(t *testing.T) {
	f := setup(t)
	_, _, owner := f.account(t)
	s := newZ2MSite(t, f, owner)
	two := simulation.Z2MSwitches[1]
	var big strings.Builder
	big.WriteString("[")
	for i := 0; i <= zigbee2mqtt.MaxDevices; i++ {
		if i > 0 {
			big.WriteString(",")
		}
		fmt.Fprintf(&big, `{"ieee_address":"0x%016x","type":"EndDevice","friendly_name":"x%d","definition":null}`, 0xb000+i, i)
	}
	big.WriteString("]")
	if e := s.capture(s.base+"/bridge/devices", []byte(big.String())); e != nil {
		t.Fatal(e)
	}
	if n := s.count(t, `SELECT count(*) FROM core.z2m_devices WHERE gateway_id=$1 AND ieee=$2 AND removed_at IS NULL`, s.gateway, two.IEEE); n != 1 {
		t.Fatal("a truncated list removed a device")
	}
	if e := s.capture(s.base+"/bridge/devices", []byte(`[]`)); e != nil {
		t.Fatal(e)
	}
	if n := s.count(t, `SELECT count(*) FROM core.sensor_streams WHERE gateway_id=$1 AND external_id=$2 AND liveness='silence'`, s.gateway, two.IEEE); n != 1 {
		t.Fatal("removed device kept reported liveness")
	}
}

// Enrolment finds the gateway's model by id, not through the list capped at 50 gateways.
func TestZigbee2MQTTEnrolmentBeyondFiftyGateways(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, auth, owner := f.account(t)
	t.Setenv("MQTT_PUBLIC_HOST", "mqtt.example.test")
	t.Setenv("MQTT_PUBLIC_PORT", "8883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "ssl")
	z, _, e := f.service.CreateGateway(ctx, owner, "Zigbee เก่าสุด", domain.Z2MGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 50; i++ {
		if _, _, e := f.service.CreateGateway(ctx, owner, fmt.Sprintf("MG3 %d", i), "minew-mg3"); e != nil {
			t.Fatal(e)
		}
	}
	listed, e := f.repo.ListGateways(ctx, owner)
	if e != nil {
		t.Fatal(e)
	}
	for _, g := range listed {
		if g.ID == z.ID {
			t.Fatal("precondition: the Zigbee gateway should be outside the first 50")
		}
	}
	code, creds, _ := req(t, busyAPI(f), "POST", "/api/v1/gateways/"+z.ID+"/mqtt", "Bearer "+auth.AccessToken, "", "", map[string]any{})
	if code != 201 || creds["z2m_yaml"] == nil || creds["post_topic"] != nil {
		t.Fatalf("enrolment of the 51st gateway: %d %v", code, creds)
	}
	if model, e := f.repo.GatewayModel(ctx, owner, z.ID); e != nil || model != domain.Z2MGatewayModel {
		t.Fatalf("GatewayModel: %q %v", model, e)
	}
	if _, e := f.repo.GatewayModel(ctx, owner, "00000000-0000-4000-8000-000000000000"); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("unknown gateway: %v", e)
	}
}
