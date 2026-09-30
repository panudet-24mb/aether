package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/app"
	"aether/backend/internal/commander"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/tuyacloudlink"
	"aether/backend/internal/tuyacloudlink/cloudkeys"
	"aether/backend/internal/tuyacloudlink/fakecloud"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// cloudKey is the worker's key pair in these tests (a fixed private key, not a secret); the API gets its public half.
var cloudKey = func() *cloudkeys.Key {
	var private [32]byte
	copy(private[:], "integration test key, not real!!")
	k, e := cloudkeys.NewKey(&private)
	if e != nil {
		panic(e)
	}
	return k
}()

// cloudAPIs routes the service's credential check to each rig's fake cloud, by Access ID.
var cloudAPIs sync.Map

func cloudClient(region, accessID, secret string) (*tuyacloud.Client, error) {
	api, ok := cloudAPIs.Load(accessID)
	if !ok {
		return nil, errors.New("no fake cloud for this access id")
	}
	return api.(*fakecloud.API).Client(region, accessID, secret)
}

// enableCloud turns Tuya Cloud mode on for a fixture, as TUYA_CLOUD=true with TUYA_CLOUD_PUBLIC_KEY does.
func enableCloud(f *fixture) {
	f.service.TuyaCloudEnabled, f.service.TuyaCloudPublicKey, f.service.CloudClient = true, cloudKey.Public(), cloudClient
}

const cloudSecret = "abcdefghijklmnopqrstuvwxyz012345"

// cloudStore is the real repository, limited to one workspace (the test database holds every test's links).
type cloudStore struct {
	*postgres.Repository
	tenant string
}

func (s cloudStore) CloudLinks(ctx context.Context) ([]domain.CloudLinkRef, error) {
	all, e := s.Repository.CloudLinks(ctx)
	out := []domain.CloudLinkRef{}
	for _, l := range all {
		if l.TenantID == s.tenant {
			out = append(out, l)
		}
	}
	return out, e
}

func (s cloudStore) ActiveTenants(context.Context) ([]string, error) { return []string{s.tenant}, nil }

// cloudRig is a workspace with a Tuya Cloud gateway linked to a fake cloud project, and the real tuya-cloud worker
// running against it.
type cloudRig struct {
	f         *fixture
	api       *fiber.App
	ctx       context.Context
	ownerAuth app.AuthResult
	owner     domain.Principal
	gateway   string
	accessID  string
	mq        *fakecloud.MQ
	cloud     *fakecloud.API
	worker    *tuyacloudlink.Worker
	stop      context.CancelFunc
	t         *testing.T
}

const (
	cloudSwitch = "bf00000000000000cs01"
	cloudPlug   = "bf00000000000000cp01"
)

func cloudSpec(t *testing.T, name string) json.RawMessage {
	b, e := os.ReadFile("../internal/adapters/tuya/testdata/" + name)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func newCloudRig(t *testing.T, f *fixture, project *string) *cloudRig {
	t.Helper()
	if f == nil {
		f = setup(t)
	}
	enableCloud(f)
	r := &cloudRig{f: f, api: busyAPI(f), ctx: context.Background(), t: t, accessID: strings.ReplaceAll(uuid.NewString(), "-", "")[:20]}
	_, r.ownerAuth, r.owner = f.account(t)
	g, _, e := f.service.CreateGatewayIn(r.ctx, r.owner, "Tuya Cloud", domain.TuyaCloudGatewayModel, project)
	if e != nil {
		t.Fatal(e)
	}
	r.gateway = g.ID
	r.mq = fakecloud.NewMQ(t, r.accessID, cloudSecret)
	r.cloud = fakecloud.NewAPI(t, r.accessID)
	cloudAPIs.Store(r.accessID, r.cloud)
	t.Cleanup(func() { cloudAPIs.Delete(r.accessID) })
	r.cloud.SetDevices(
		fakecloud.CloudDevice{ID: cloudSwitch, Name: "Cloud switch", Category: "kg", ProductID: "pkg", Spec: cloudSpec(t, "kg_3gang.json")},
		fakecloud.CloudDevice{ID: cloudPlug, Name: "Cloud plug", Category: "cz", ProductID: "pcz", Spec: cloudSpec(t, "cz_metering.json")})
	if e := r.link(r.accessID); e != nil {
		t.Fatal(e)
	}
	return r
}

// link links the project through the service, as the API route will: proven with the (fake) cloud, sealed to
// the worker's public key for this tenant and gateway.
func (r *cloudRig) link(accessID string) error {
	return r.f.service.LinkTuyaCloud(r.ctx, r.owner, r.gateway, "eu", tuyacloud.ChannelProd, accessID, cloudSecret)
}

// start runs the worker; it stops with the test.
func (r *cloudRig) start() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	r.stop = cancel
	r.worker = &tuyacloudlink.Worker{Store: cloudStore{r.f.repo, r.owner.TenantID}, Keys: []*cloudkeys.Key{cloudKey}, ConsumerURL: r.mq.URL, NewClient: r.cloud.Client,
		TLS: r.mq.TLS(), Timings: tuyacloudlink.Timings{Health: 200 * time.Millisecond, Ping: time.Second, ReadTimeout: 5 * time.Second,
			BackoffMin: 20 * time.Millisecond, BackoffMax: 200 * time.Millisecond, SyncEvery: time.Hour}}
	if e := r.worker.Reconcile(ctx); e != nil {
		r.t.Fatal(e)
	}
	r.t.Cleanup(func() { cancel(); r.worker.Wait() })
}

func (r *cloudRig) halt() {
	r.stop()
	r.worker.Wait()
}

func (r *cloudRig) eventually(what string, cond func() bool) {
	r.t.Helper()
	if !r.mq.WaitFor(10*time.Second, cond) {
		r.t.Fatalf("never: %s", what)
	}
}

func (r *cloudRig) count(q string, args ...any) int {
	r.t.Helper()
	var n int
	if e := r.f.admin.QueryRowContext(r.ctx, q, args...).Scan(&n); e != nil {
		r.t.Fatal(e)
	}
	return n
}

func (r *cloudRig) text(q string, args ...any) string {
	r.t.Helper()
	var s string
	if e := r.f.admin.QueryRowContext(r.ctx, q, args...).Scan(&s); e != nil {
		r.t.Fatal(e)
	}
	return s
}

func (r *cloudRig) linkState() string {
	return r.text(`SELECT state FROM core.tuya_cloud_links WHERE gateway_id=$1`, r.gateway)
}

// synced waits for the first device-list sync.
func (r *cloudRig) synced() {
	r.t.Helper()
	r.eventually("devices synced", func() bool {
		return r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND removed_at IS NULL`, r.gateway) == 2
	})
}

func (r *cloudRig) register(id, name string) string {
	r.t.Helper()
	d, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, name, id, domain.TuyaCloudProfile)
	if e != nil {
		r.t.Fatalf("register %s: %v", id, e)
	}
	return d.ID
}

// push sends a message and waits until the worker acknowledged it.
func (r *cloudRig) push(protocol int, plaintext string) string {
	r.t.Helper()
	id := uuid.NewString()
	r.mq.Push(fakecloud.Message{ID: id, Protocol: protocol, Plaintext: plaintext, GCM: true})
	if !r.mq.WaitAck(id, 10*time.Second) {
		r.t.Fatalf("message %s not acknowledged", id)
	}
	return id
}

func (r *cloudRig) status(dev string, t int64, values map[string]any) {
	r.t.Helper()
	r.push(tuyacloud.ProtocolStatus, fakecloud.Status(dev, t, values))
}

func (r *cloudRig) send(token string, body map[string]any) (int, map[string]any) {
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

// Link, sync, discovery, registration, state reports through the Message Service, ordering and replays, device
// liveness, events of unknown devices, and what the status and command targets show.
func TestTuyaCloudIngest(t *testing.T) {
	r := newCloudRig(t, nil, nil)
	st, e := r.f.repo.TuyaCloudLinkStatus(r.ctx, r.owner, r.gateway)
	if e != nil || !st.Linked || st.State != domain.CloudLinkLinking || st.Region != "eu" || st.AccessIDHint != r.accessID[:4] || st.SyncRequestedAt == nil {
		t.Fatalf("status after link: %+v %v", st, e)
	}
	if raw, _ := json.Marshal(st); strings.Contains(string(raw), r.accessID) || strings.Contains(string(raw), "sealed") {
		t.Fatalf("status reveals the access id: %s", raw)
	}
	r.start()
	r.synced()
	r.eventually("link online", func() bool { return r.linkState() == "online" })
	r.eventually("sync request cleared", func() bool {
		return r.count(`SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=$1 AND sync_requested_at IS NULL`, r.gateway) == 1
	})
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND local_key_sealed IS NULL AND key_fingerprint=''`, r.gateway); n != 2 {
		t.Fatalf("cloud devices hold a key: %d", n)
	}
	// Discovery offers the synced devices with the cloud profile.
	code, out := get(t, r.api, "/api/v1/discovery", r.ownerAuth.AccessToken)
	if code != 200 {
		t.Fatalf("discovery: %d", code)
	}
	found := map[string]map[string]any{}
	for _, raw := range out["items"].([]any) {
		item := raw.(map[string]any)
		found[item["external_id"].(string)] = item
	}
	if sw := found[cloudSwitch]; sw == nil || sw["source"] != "tuya_cloud" || sw["profile"] == nil || sw["profile"].(map[string]any)["id"] != domain.TuyaCloudProfile {
		t.Fatalf("discovered switch: %v", sw)
	}
	r.register(cloudSwitch, "Cloud switch")
	// The cloud profile belongs under a cloud gateway only, and a cloud gateway takes no other profile.
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Wi-Fi on cloud", cloudPlug, domain.TuyaWiFiProfile); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("Wi-Fi profile under a cloud gateway: %v", e)
	}
	// command_targets: a cloud row, the link's state as the agent state, no key needed.
	var transport, agent, key string
	if e := r.f.admin.QueryRowContext(r.ctx, `SELECT transport,agent_state,key_status FROM core.command_targets WHERE gateway_id=$1 AND external_id=$2`, r.gateway, cloudSwitch).
		Scan(&transport, &agent, &key); e != nil || transport != "cloud" || agent != "online" || key != "ok" {
		t.Fatalf("command target: %q %q %q %v", transport, agent, key, e)
	}

	// A status report: the registered switch gets a sample and its state; the unregistered plug only its state.
	base := time.Now().UnixMilli()
	r.status(cloudSwitch, base, map[string]any{"switch_1": true, "switch_2": false})
	if s := r.text(`SELECT state->>'switch_1' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudSwitch); s != "ON" {
		t.Fatalf("switch state: %q", s)
	}
	if d := r.text(`SELECT decoder_id FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 ORDER BY received_at DESC LIMIT 1`, r.gateway, cloudSwitch); !strings.Contains(d, tuyacloud.FrameState) {
		t.Fatalf("decoder: %q", d)
	}
	r.status(cloudPlug, base, map[string]any{"switch_1": true, "cur_power": 1003})
	if s := r.text(`SELECT state->>'switch_1' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudPlug); s != "ON" {
		t.Fatalf("plug state: %q", s)
	}
	if n := r.count(`SELECT count(*) FROM core.sensor_streams WHERE gateway_id=$1 AND external_id=$2`, r.gateway, cloudPlug); n != 0 {
		t.Fatal("an unregistered cloud device got a stream")
	}
	// Out of order: an older value never overwrites a newer one; a replay changes nothing.
	samples := r.count(`SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2`, r.gateway, cloudSwitch)
	r.status(cloudSwitch, base-5000, map[string]any{"switch_1": false})
	r.status(cloudSwitch, base, map[string]any{"switch_1": true, "switch_2": false})
	if s := r.text(`SELECT state->>'switch_1' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudSwitch); s != "ON" {
		t.Fatalf("an older report won: %q", s)
	}
	if n := r.count(`SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2`, r.gateway, cloudSwitch); n != samples {
		t.Fatalf("stale reports stored samples: %d -> %d", samples, n)
	}
	// A newer change is one switch_on event.
	r.status(cloudSwitch, base+1000, map[string]any{"switch_2": true})
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='switch_on'`, r.gateway, cloudSwitch); n != 1 {
		t.Fatalf("switch_on events: %d", n)
	}

	// A report time far in the future is read as now, so it cannot outrank the reports that follow; and only the
	// specification's codes are kept in reported_t.
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.tuya_devices SET reported_t=reported_t||'{"not_a_code":1}' WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudSwitch); e != nil {
		t.Fatal(e)
	}
	r.status(cloudSwitch, time.Now().Add(time.Hour).UnixMilli(), map[string]any{"switch_3": true})
	if s := r.text(`SELECT state->>'switch_3' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudSwitch); s != "ON" {
		t.Fatalf("future report: %q", s)
	}
	if far := r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2 AND (reported_t->>'switch_3')::bigint > $3`, r.gateway, cloudSwitch, time.Now().Add(6*time.Minute).UnixMilli()); far != 0 {
		t.Fatal("a future report time was stored")
	}
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2 AND reported_t ? 'not_a_code'`, r.gateway, cloudSwitch); n != 0 {
		t.Fatal("reported_t kept a code outside the specification")
	}
	r.status(cloudSwitch, time.Now().UnixMilli()+1000, map[string]any{"switch_3": false})
	if s := r.text(`SELECT state->>'switch_3' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudSwitch); s != "OFF" {
		t.Fatalf("report after a future one: %q", s)
	}

	// Device liveness from protocol 20: offline, a stale online (ignored), a newer online.
	r.push(tuyacloud.ProtocolDevice, fakecloud.Device(cloudSwitch, "offline", base+2000, ""))
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2 AND event_type='offline' AND detail->>'source'='cloud'`, r.gateway, cloudSwitch); n != 1 {
		t.Fatalf("offline events: %d", n)
	}
	r.push(tuyacloud.ProtocolDevice, fakecloud.Device(cloudSwitch, "online", base+1500, ""))
	if n := r.count(`SELECT count(*) FROM core.stream_state WHERE gateway_id=$1 AND external_id=$2 AND offline`, r.gateway, cloudSwitch); n != 1 {
		t.Fatal("a stale online message brought the device back")
	}
	r.push(tuyacloud.ProtocolDevice, fakecloud.Device(cloudSwitch, "online", base+3000, ""))
	if n := r.count(`SELECT count(*) FROM core.stream_state WHERE gateway_id=$1 AND external_id=$2 AND offline`, r.gateway, cloudSwitch); n != 0 {
		t.Fatal("online did not bring the device back")
	}
	// Renamed updates the Tuya name only; the registration keeps its own.
	r.push(tuyacloud.ProtocolDevice, fakecloud.Device(cloudSwitch, "nameUpdate", base+4000, "Hall"))
	if n := r.text(`SELECT name FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudSwitch); n != "Hall" {
		t.Fatalf("tuya name: %q", n)
	}
	if n := r.text(`SELECT name FROM core.devices WHERE gateway_id=$1 AND external_id=$2`, r.gateway, cloudSwitch); n != "Cloud switch" {
		t.Fatalf("registration renamed: %q", n)
	}

	// An unknown device never creates a row; it asks for a sync (debounced).
	stranger := "bf00000000000000zz77"
	r.status(stranger, base, map[string]any{"switch_1": true})
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE tuya_id=$1`, stranger); n != 0 {
		t.Fatal("an event created a device")
	}
	if n := r.count(`SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=$1 AND sync_requested_at IS NOT NULL`, r.gateway); n != 1 {
		t.Fatal("unknown device did not ask for a sync")
	}
	// Diagnostics keep kinds, devices and codes, never values.
	if n := r.count(`SELECT count(*) FROM core.gateway_packets WHERE gateway_id=$1 AND payload ? 'cloud_protocol'`, r.gateway); n == 0 {
		t.Fatal("no diagnostic copies")
	}
	if n := r.count(`SELECT count(*) FROM core.gateway_packets WHERE gateway_id=$1 AND payload::text LIKE '%1003%'`, r.gateway); n != 0 {
		t.Fatal("a diagnostic copy holds a value")
	}
	// Health reports and usage counters.
	r.eventually("health and usage", func() bool {
		return r.count(`SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=$1 AND last_health_at IS NOT NULL AND events_month>=10 AND api_calls_month>0 AND last_event_at IS NOT NULL`, r.gateway) == 1
	})
	// A packet posted for a cloud gateway (it has no site device) is refused.
	if _, e := r.f.service.Capture(r.ctx, r.owner.TenantID, r.gateway, []byte(`[{"mac":"AC233FA00001"}]`)); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("packet on a cloud gateway: %v", e)
	}
	// Removed marks the Tuya device removed (the registration stays).
	r.push(tuyacloud.ProtocolDevice, fakecloud.Device(cloudPlug, "delete", base+5000, ""))
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2 AND removed_at IS NOT NULL`, r.gateway, cloudPlug); n != 1 {
		t.Fatal("removed device kept")
	}
}

// End to end: POST /commands -> the worker's dispatcher issues the property through the OpenAPI -> the fake
// cloud reports the new state through the Message Service -> the command is confirmed. Tuya's refusals become
// the command's reason and, for quota, the link's state.
func TestTuyaCloudCommands(t *testing.T) {
	r := newCloudRig(t, nil, nil)
	r.start()
	r.synced()
	r.eventually("link online", func() bool { return r.linkState() == "online" })
	device := r.register(cloudSwitch, "Cloud switch")
	r.status(cloudSwitch, time.Now().UnixMilli(), map[string]any{"switch_1": false, "switch_2": false, "switch_3": false})
	r.cloud.OnIssue(func(i fakecloud.Issue) {
		values := map[string]any{}
		for code, raw := range i.Properties {
			var v any
			_ = json.Unmarshal(raw, &v)
			values[code] = v
		}
		r.mq.Push(fakecloud.Message{ID: uuid.NewString(), Protocol: tuyacloud.ProtocolStatus, Plaintext: fakecloud.Status(i.Device, time.Now().UnixMilli(), values)})
	})
	d := r.worker.Dispatcher()
	code, out := r.send(r.ownerAuth.AccessToken, map[string]any{"device_id": device, "property": "switch_1", "value": "ON"})
	if code != 202 || out["transport"] != "cloud" {
		t.Fatalf("queue: %d %v", code, out)
	}
	if !strings.Contains(r.text(`SELECT wire::text FROM core.device_commands WHERE id=$1`, out["id"]), `"switch_1": true`) {
		t.Fatalf("wire: %s", r.text(`SELECT wire::text FROM core.device_commands WHERE id=$1`, out["id"]))
	}
	// mqtt-commander never claims it.
	mqtt := &commander.Dispatcher{Store: rigStore{r.f.repo, r.owner.TenantID}, Publisher: failingPublisher{}, Now: time.Now, Transports: commander.MQTTTransports}
	if n, e := mqtt.RunOnce(r.ctx); e != nil || n != 0 {
		t.Fatalf("mqtt-commander took a cloud command: %d %v", n, e)
	}
	if n, e := d.RunOnce(r.ctx); e != nil || n != 1 {
		t.Fatalf("cloud dispatch: %d %v", n, e)
	}
	if issues := r.cloud.Issues(); len(issues) != 1 || issues[0].Device != cloudSwitch || string(issues[0].Properties["switch_1"]) != "true" {
		t.Fatalf("issued: %+v", issues)
	}
	r.eventually("command confirmed", func() bool {
		return r.text(`SELECT status FROM core.device_commands WHERE id=$1`, out["id"]) == "confirmed"
	})
	// Tuya says the device is offline: the command fails with that reason.
	r.cloud.Fail(cloudSwitch, 2001)
	code, out = r.send(r.ownerAuth.AccessToken, map[string]any{"device_id": device, "property": "switch_2", "value": "ON"})
	if code != 202 {
		t.Fatalf("queue: %d %v", code, out)
	}
	if _, e := d.RunOnce(r.ctx); e != nil {
		t.Fatal(e)
	}
	if s, why := r.text(`SELECT status FROM core.device_commands WHERE id=$1`, out["id"]), r.text(`SELECT error FROM core.device_commands WHERE id=$1`, out["id"]); s != "failed" || why != tuyacloudlink.ReasonOffline {
		t.Fatalf("offline device: %s %q", s, why)
	}
	// Quota: the command fails, the link reports quota.
	r.cloud.Fail(cloudSwitch, http.StatusTooManyRequests)
	code, out = r.send(r.ownerAuth.AccessToken, map[string]any{"device_id": device, "property": "switch_3", "value": "ON"})
	if code != 202 {
		t.Fatalf("queue: %d %v", code, out)
	}
	if _, e := d.RunOnce(r.ctx); e != nil {
		t.Fatal(e)
	}
	if why := r.text(`SELECT error FROM core.device_commands WHERE id=$1`, out["id"]); why != tuyacloudlink.ReasonQuota || r.linkState() != "quota" {
		t.Fatalf("quota: %q, link %q", why, r.linkState())
	}
	// A link that is down refuses new commands at once.
	if code, out = r.send(r.ownerAuth.AccessToken, map[string]any{"device_id": device, "property": "switch_3", "value": "OFF"}); code != 409 || out["error"] != "offline" {
		t.Fatalf("command on a link in quota: %d %v", code, out)
	}
}

type failingPublisher struct{}

func (failingPublisher) Publish(string, []byte) error { return errors.New("no broker in this test") }

// A link down for longer than the grace takes its devices offline (cloud_link_down); a worker gone silent takes
// them offline too (cloud_silent); the link coming back restores them.
func TestTuyaCloudLinkDown(t *testing.T) {
	r := newCloudRig(t, nil, nil)
	r.start()
	r.synced()
	r.register(cloudSwitch, "Cloud switch")
	r.status(cloudSwitch, time.Now().UnixMilli(), map[string]any{"switch_1": true})
	r.halt()
	// The budget guard: events of devices not registered here are dropped, the registered switch's still count.
	res, e := r.f.repo.CaptureTuyaCloud(r.ctx, r.owner.TenantID, r.gateway, tuyacloud.ProtocolStatus, []tuyacloud.Event{
		{Kind: tuyacloud.EventStatus, DevID: cloudPlug, Items: []tuyacloud.Item{{Code: "switch_1", Value: json.RawMessage(`true`), T: time.Now().UnixMilli()}}},
		{Kind: tuyacloud.EventStatus, DevID: cloudSwitch, Items: []tuyacloud.Item{{Code: "switch_1", Value: json.RawMessage(`false`), T: time.Now().UnixMilli()}}},
		{Kind: tuyacloud.EventStatus, DevID: "bf00000000000000zz78", Items: []tuyacloud.Item{{Code: "switch_1", Value: json.RawMessage(`true`), T: 1}}},
	}, true)
	if e != nil || res.Dropped != 2 || res.Applied != 1 || res.SyncRequested {
		t.Fatalf("guarded capture: %+v %v", res, e)
	}
	if s := r.text(`SELECT state::text FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudPlug); s != "{}" {
		t.Fatalf("guarded plug state: %s", s)
	}
	revision := int64(r.count(`SELECT revision FROM core.tuya_cloud_links WHERE gateway_id=$1`, r.gateway))
	if e := r.f.repo.SetCloudLinkState(r.ctx, r.owner.TenantID, r.gateway, revision, domain.CloudLinkOffline, "mq_disconnected", 0); e != nil {
		t.Fatal(e)
	}
	// Within the grace nothing happens.
	if _, e := r.f.repo.ScanOffline(r.ctx, r.owner.TenantID, time.Now()); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM core.stream_state WHERE gateway_id=$1 AND offline`, r.gateway); n != 0 {
		t.Fatal("offline within the grace")
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.tuya_cloud_links SET state_at=now()-interval '3 minutes' WHERE gateway_id=$1`, r.gateway); e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.repo.ScanOffline(r.ctx, r.owner.TenantID, time.Now()); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND event_type='offline' AND detail->>'source'='cloud_link_down'`, r.gateway); n != 1 {
		t.Fatalf("cloud_link_down events: %d", n)
	}
	if reason := r.text(`SELECT reason FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudSwitch); reason != "cloud_link_down" {
		t.Fatalf("device reason: %q", reason)
	}
	// A second scan changes nothing.
	if _, e := r.f.repo.ScanOffline(r.ctx, r.owner.TenantID, time.Now()); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND event_type='offline'`, r.gateway); n != 1 {
		t.Fatalf("offline events after a second scan: %d", n)
	}
	// The link back online restores the device and clears the reason.
	if e := r.f.repo.SetCloudLinkState(r.ctx, r.owner.TenantID, r.gateway, revision, domain.CloudLinkOnline, "", 0); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM core.stream_state WHERE gateway_id=$1 AND offline`, r.gateway); n != 0 {
		t.Fatal("device not restored")
	}
	if reason := r.text(`SELECT reason FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, cloudSwitch); reason != "" {
		t.Fatalf("reason kept: %q", reason)
	}
	// A verdict for an older revision (credentials since rotated) is ignored.
	if e := r.f.repo.SetCloudLinkState(r.ctx, r.owner.TenantID, r.gateway, revision-1, domain.CloudLinkAuthFailed, "", 0); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("stale verdict: %v", e)
	}
	// Silence: nothing recorded for five minutes while the link claims to be online.
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.gateway_packets SET received_at=now()-interval '6 minutes' WHERE gateway_id=$1`, r.gateway); e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.repo.ScanOffline(r.ctx, r.owner.TenantID, time.Now()); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND event_type='offline' AND detail->>'source'='cloud_silent'`, r.gateway); n != 1 || r.linkState() != "offline" {
		t.Fatalf("cloud_silent events: %d, link %q", n, r.linkState())
	}
}

// Credentials refused by the Message Service park the link as auth_failed; saving them again restarts it.
func TestTuyaCloudAuthFailed(t *testing.T) {
	r := newCloudRig(t, nil, nil)
	r.mq.Reject(http.StatusUnauthorized)
	r.start()
	r.eventually("auth_failed", func() bool { return r.linkState() == "auth_failed" })
	st, e := r.f.repo.TuyaCloudLinkStatus(r.ctx, r.owner, r.gateway)
	if e != nil || st.State != domain.CloudLinkAuthFailed || st.Reason != "mq_auth" || st.LastErrorCode != 401 {
		t.Fatalf("status: %+v %v", st, e)
	}
	r.mq.Reject(0)
	if e := r.link(r.accessID); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	if e := r.worker.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	r.eventually("online after new credentials", func() bool { return r.linkState() == "online" })
	cancel()
	r.worker.Wait()
}

// Isolation: one project per link (the Access ID is globally unique), the same Tuya device id in two workspaces
// never crosses, one mode per physical device, project scope, unlink and revoke wipe the credentials.
func TestTuyaCloudIsolation(t *testing.T) {
	f := setup(t)
	a := newCloudRig(t, f, nil)
	b := newCloudRig(t, f, nil)
	// The same project cannot be linked twice, in another workspace or the same one.
	if e := b.link(a.accessID); !errors.Is(e, domain.ErrConflict) || !strings.Contains(e.Error(), "already_linked") {
		t.Fatalf("second link of one project: %v", e)
	}
	second, _, e := a.f.service.CreateGateway(a.ctx, a.owner, "Second cloud", domain.TuyaCloudGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	sealed, _ := security.SealTuyaCloud(cloudKey.Public(), a.owner.TenantID, second.ID, a.accessID, cloudSecret)
	if e := a.f.repo.SaveTuyaCloudLink(a.ctx, a.owner, domain.TuyaCloudLinkRequest{GatewayID: second.ID, Region: "eu", Channel: "event",
		AccessIDDigest: security.AccessIDDigest(a.accessID), CredentialsSealed: sealed}); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("same project on a second gateway: %v", e)
	}
	// Credentials sealed for workspace A never open for B's gateway (the binding is checked, not only the key).
	copied := a.text(`SELECT credentials_sealed FROM core.tuya_cloud_links WHERE gateway_id=$1`, a.gateway)
	if _, e := a.f.admin.ExecContext(a.ctx, `UPDATE core.tuya_cloud_links SET credentials_sealed=$1 WHERE gateway_id=$2`, copied, b.gateway); e != nil {
		t.Fatal(e)
	}
	b.start()
	b.eventually("B parked", func() bool { return b.linkState() == "auth_failed" })
	if b.mq.Dials() != 0 {
		t.Fatal("B dialled with A's credentials")
	}
	b.halt()
	if e := b.link(b.accessID); e != nil {
		t.Fatal(e)
	}
	// Both workspaces sync the same device id.
	a.start()
	b.start()
	a.synced()
	b.synced()
	a.register(cloudSwitch, "A switch")
	b.register(cloudSwitch, "B switch")
	now := time.Now().UnixMilli()
	a.status(cloudSwitch, now, map[string]any{"switch_1": true})
	b.status(cloudSwitch, now, map[string]any{"switch_1": false})
	if s := a.text(`SELECT state->>'switch_1' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, a.gateway, cloudSwitch); s != "ON" {
		t.Fatalf("A state: %q", s)
	}
	if s := b.text(`SELECT state->>'switch_1' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, b.gateway, cloudSwitch); s != "OFF" {
		t.Fatalf("B state: %q", s)
	}
	// One mode per physical device: the switch registered through the cloud cannot also be registered through an
	// Edge in the same workspace (another workspace is another physical claim and is not affected).
	edgeGW, _, e := a.f.service.CreateGateway(a.ctx, a.owner, "Edge", domain.EdgeGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := a.f.service.CreateDevice(a.ctx, a.owner, edgeGW.ID, "Same switch", cloudSwitch, domain.TuyaWiFiProfile); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("second mode for one device: %v", e)
	}
	if _, e := a.f.service.CreateDevice(a.ctx, a.owner, edgeGW.ID, "Same switch", strings.ToUpper(cloudSwitch), domain.TuyaWiFiProfile); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("second mode, other case: %v", e)
	}
	// Unlink wipes the credentials and frees the project for another link; the worker stops the link.
	if e := a.f.repo.UnlinkTuyaCloud(a.ctx, a.owner, a.gateway); e != nil {
		t.Fatal(e)
	}
	if n := a.count(`SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=$1 AND credentials_sealed IS NULL AND access_id_digest IS NULL AND state='disabled'`, a.gateway); n != 1 {
		t.Fatal("unlink kept the credentials")
	}
	if e := a.worker.Reconcile(a.ctx); e != nil {
		t.Fatal(e)
	}
	if e := a.f.repo.SaveTuyaCloudLink(a.ctx, a.owner, domain.TuyaCloudLinkRequest{GatewayID: second.ID, Region: "eu", Channel: "event",
		AccessIDDigest: security.AccessIDDigest(a.accessID), CredentialsSealed: sealed}); e != nil {
		t.Fatalf("relink after unlink: %v", e)
	}
	// Revoking the gateway wipes its credentials too.
	if e := a.f.service.RevokeGateway(a.ctx, a.owner, second.ID); e != nil {
		t.Fatal(e)
	}
	if n := a.count(`SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=$1 AND credentials_sealed IS NULL AND state='disabled'`, second.ID); n != 1 {
		t.Fatal("revoke kept the credentials")
	}
	// The definer function lists only live links with credentials: neither A's unlinked nor the revoked one.
	links, e := a.f.repo.CloudLinks(a.ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, l := range links {
		if l.GatewayID == a.gateway || l.GatewayID == second.ID {
			t.Fatalf("listed: %+v", l)
		}
	}
}

// Project scope: a member confined to another project sees neither the link nor the devices' command targets.
func TestTuyaCloudProjectScope(t *testing.T) {
	f := setup(t)
	enableCloud(f)
	_, ownerAuth, owner := f.account(t)
	p, e := f.service.CreateProject(context.Background(), owner, "P", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	q, e := f.service.CreateProject(context.Background(), owner, "Q", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	g, _, e := f.service.CreateGatewayIn(context.Background(), owner, "Cloud in P", domain.TuyaCloudGatewayModel, &p.ID)
	if e != nil {
		t.Fatal(e)
	}
	accessID := strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	sealed, _ := security.SealTuyaCloud(cloudKey.Public(), owner.TenantID, g.ID, accessID, cloudSecret)
	if e := f.repo.SaveTuyaCloudLink(context.Background(), owner, domain.TuyaCloudLinkRequest{GatewayID: g.ID, Region: "us", Channel: "event",
		AccessIDDigest: security.AccessIDDigest(accessID), CredentialsSealed: sealed}); e != nil {
		t.Fatal(e)
	}
	api := busyAPI(f)
	email := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, email, "admin", []string{q.ID})
	_, member := changeInitialPassword(t, f, api, email, owner.TenantID)
	if _, e := f.repo.TuyaCloudLinkStatus(context.Background(), member, g.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("status outside the member's projects: %v", e)
	}
	if e := f.repo.UnlinkTuyaCloud(context.Background(), member, g.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("unlink outside the member's projects: %v", e)
	}
	if e := f.repo.SaveTuyaCloudLink(context.Background(), member, domain.TuyaCloudLinkRequest{GatewayID: g.ID, Region: "us", Channel: "event",
		AccessIDDigest: security.AccessIDDigest(accessID + "x"), CredentialsSealed: sealed}); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("link outside the member's projects: %v", e)
	}
	if st, e := f.repo.TuyaCloudLinkStatus(context.Background(), owner, g.ID); e != nil || !st.Linked {
		t.Fatalf("owner status: %+v %v", st, e)
	}
}

func catalogIDs(t *testing.T, f *fixture, token string) (models, profiles map[string]bool) {
	t.Helper()
	code, out := get(t, busyAPI(f), "/api/v1/catalog", token)
	if code != 200 {
		t.Fatalf("catalog: %d", code)
	}
	models, profiles = map[string]bool{}, map[string]bool{}
	for _, m := range out["gateway_models"].([]any) {
		models[m.(map[string]any)["id"].(string)] = true
	}
	for _, p := range out["device_profiles"].([]any) {
		profiles[p.(map[string]any)["id"].(string)] = true
	}
	return models, profiles
}

// TUYA_CLOUD off (the default): the model and profile are not in the catalog, a cloud gateway cannot be created,
// the cloud profile cannot be registered, linking is refused and discovery never offers cloud devices. On: all of
// that works. Either way a cloud gateway gets no ingest token and no broker account.
func TestTuyaCloudFeatureFlag(t *testing.T) {
	f := setup(t)
	_, auth, owner := f.account(t)
	ctx := context.Background()
	models, profiles := catalogIDs(t, f, auth.AccessToken)
	if models[domain.TuyaCloudGatewayModel] || profiles[domain.TuyaCloudProfile] || !models[domain.EdgeGatewayModel] {
		t.Fatalf("catalog with the flag off: %v %v", models, profiles)
	}
	if _, _, e := f.service.CreateGateway(ctx, owner, "Cloud", domain.TuyaCloudGatewayModel); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("cloud gateway with the flag off: %v", e)
	}
	// On.
	enableCloud(f)
	models, profiles = catalogIDs(t, f, auth.AccessToken)
	if !models[domain.TuyaCloudGatewayModel] || !profiles[domain.TuyaCloudProfile] {
		t.Fatalf("catalog with the flag on: %v %v", models, profiles)
	}
	g, token, e := f.service.CreateGateway(ctx, owner, "Cloud", domain.TuyaCloudGatewayModel)
	if e != nil || token != "" {
		t.Fatalf("cloud gateway: %v, token handed out: %v", e, token != "")
	}
	if _, e := f.service.Gateway(ctx, g.ID, token); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("ingest with no token: %v", e)
	}
	code, out, _ := req(t, busyAPI(f), "POST", "/api/v1/gateways/"+g.ID+"/mqtt", "Bearer "+auth.AccessToken, "", origin, nil)
	if code != 400 || out["error"] != "cloud_gateway_has_no_mqtt" {
		t.Fatalf("MQTT account for a cloud gateway: %d %v", code, out)
	}
	if e := f.repo.EnrollMQTT(ctx, owner, g.ID, "$7$hash", false); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("repository enrolment: %v", e)
	}
	if n := countRows(t, f, `SELECT count(*) FROM core.mqtt_accounts WHERE gateway_id=$1`, g.ID); n != 0 {
		t.Fatalf("broker accounts: %d", n)
	}
	// A synced device is offered by discovery with the flag on, never with it off.
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.tuya_devices(tenant_id,gateway_id,tuya_id,name) VALUES($1,$2,$3,'Hall')`, owner.TenantID, g.ID, cloudSwitch); e != nil {
		t.Fatal(e)
	}
	offered := func() bool {
		code, out := get(t, busyAPI(f), "/api/v1/discovery", auth.AccessToken)
		if code != 200 {
			t.Fatalf("discovery: %d", code)
		}
		for _, raw := range out["items"].([]any) {
			if raw.(map[string]any)["source"] == "tuya_cloud" {
				return true
			}
		}
		return false
	}
	if !offered() {
		t.Fatal("cloud device not discovered with the flag on")
	}
	// Off again: nothing cloud is offered or accepted, even on the existing gateway.
	f.service.TuyaCloudEnabled = false
	if offered() {
		t.Fatal("cloud device discovered with the flag off")
	}
	if _, e := f.service.CreateDevice(ctx, owner, g.ID, "Hall", cloudSwitch, domain.TuyaCloudProfile); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("cloud profile with the flag off: %v", e)
	}
	if e := f.service.LinkTuyaCloud(ctx, owner, g.ID, "eu", "event", "abcdefgh12345678", cloudSecret); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("link with the flag off: %v", e)
	}
}

func countRows(t *testing.T, f *fixture, q string, args ...any) int {
	t.Helper()
	var n int
	if e := f.admin.QueryRowContext(context.Background(), q, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// Linking: without the public key the service answers tuya_cloud_unconfigured (503) and writes nothing; refused
// credentials write nothing; a viewer may not link (service and repository); a workspace links at most two
// projects.
func TestTuyaCloudLinkService(t *testing.T) {
	f := setup(t)
	enableCloud(f)
	_, auth, owner := f.account(t)
	ctx := context.Background()
	gw := func(name string) string {
		g, _, e := f.service.CreateGateway(ctx, owner, name, domain.TuyaCloudGatewayModel)
		if e != nil {
			t.Fatal(e)
		}
		return g.ID
	}
	project := func() (string, *fakecloud.API) {
		id := strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
		api := fakecloud.NewAPI(t, id)
		cloudAPIs.Store(id, api)
		t.Cleanup(func() { cloudAPIs.Delete(id) })
		return id, api
	}
	g1, g2, g3 := gw("One"), gw("Two"), gw("Three")
	id1, api1 := project()
	links := func() int {
		return countRows(t, f, `SELECT count(*) FROM core.tuya_cloud_links WHERE tenant_id=$1`, owner.TenantID)
	}

	f.service.TuyaCloudPublicKey = nil
	e := f.service.LinkTuyaCloud(ctx, owner, g1, "eu", "event", id1, cloudSecret)
	var reason domain.ReasonError
	if !errors.Is(e, domain.ErrUnavailable) || !errors.As(e, &reason) || reason.Reason != "tuya_cloud_unconfigured" || links() != 0 || api1.Calls("token") != 0 {
		t.Fatalf("without the public key: %v, %d links, %d token calls", e, links(), api1.Calls("token"))
	}
	f.service.TuyaCloudPublicKey = cloudKey.Public()

	// Refused by Tuya: nothing is written, so a stranger cannot claim a project by its Access ID.
	api1.RefuseToken(true)
	if e := f.service.LinkTuyaCloud(ctx, owner, g1, "eu", "event", id1, cloudSecret); !errors.Is(e, domain.ErrInvalid) || !strings.Contains(e.Error(), "tuya_auth_failed") || links() != 0 {
		t.Fatalf("refused credentials: %v, %d links", e, links())
	}
	api1.RefuseToken(false)
	for _, bad := range [][4]string{{"xx", "event", id1, cloudSecret}, {"eu", "other", id1, cloudSecret}, {"eu", "event", "short", cloudSecret}, {"eu", "event", id1, "bad secret!"}} {
		if e := f.service.LinkTuyaCloud(ctx, owner, g1, bad[0], bad[1], bad[2], bad[3]); !errors.Is(e, domain.ErrInvalid) {
			t.Fatalf("%v accepted: %v", bad, e)
		}
	}
	// A viewer may not link, through the service or straight to the repository.
	email := memberEmail()
	api := busyAPI(f)
	addMember(t, api, auth.AccessToken, email, "viewer", []string{})
	_, viewer := changeInitialPassword(t, f, api, email, owner.TenantID)
	if e := f.service.LinkTuyaCloud(ctx, viewer, g1, "eu", "event", id1, cloudSecret); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("viewer link: %v", e)
	}
	sealed, _ := security.SealTuyaCloud(cloudKey.Public(), owner.TenantID, g1, id1, cloudSecret)
	request := domain.TuyaCloudLinkRequest{GatewayID: g1, Region: "eu", Channel: "event", AccessIDDigest: security.AccessIDDigest(id1), CredentialsSealed: sealed}
	if e := f.repo.SaveTuyaCloudLink(ctx, viewer, request); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("viewer save: %v", e)
	}
	if e := f.repo.UnlinkTuyaCloud(ctx, viewer, g1); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("viewer unlink: %v", e)
	}
	if e := f.repo.RequestCloudSync(ctx, viewer, g1); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("viewer sync: %v", e)
	}
	// Two projects per workspace; rotating one of them is not a third; unlinking frees a slot.
	if e := f.service.LinkTuyaCloud(ctx, owner, g1, "eu", "event", id1, cloudSecret); e != nil || api1.Calls("token") < 2 {
		t.Fatalf("link: %v", e)
	}
	sealedFor := func(gateway string) string {
		return countText(t, f, `SELECT credentials_sealed FROM core.tuya_cloud_links WHERE gateway_id=$1`, gateway)
	}
	if strings.Contains(sealedFor(g1), id1) {
		t.Fatal("stored in the clear")
	}
	if _, e := cloudkeys.Open(owner.TenantID, g1, sealedFor(g1), cloudKey); e != nil {
		t.Fatalf("the worker cannot open what the service sealed: %v", e)
	}
	id2, _ := project()
	id3, _ := project()
	if e := f.service.LinkTuyaCloud(ctx, owner, g2, "us", "event-test", id2, cloudSecret); e != nil {
		t.Fatal(e)
	}
	if e := f.service.LinkTuyaCloud(ctx, owner, g3, "eu", "event", id3, cloudSecret); !errors.Is(e, domain.ErrConflict) || !strings.Contains(e.Error(), "cloud_link_limit") {
		t.Fatalf("third project: %v", e)
	}
	if e := f.service.LinkTuyaCloud(ctx, owner, g2, "us", "event", id2, cloudSecret); e != nil {
		t.Fatalf("rotation counted as a new link: %v", e)
	}
	if e := f.service.UnlinkTuyaCloud(ctx, owner, g2); e != nil {
		t.Fatal(e)
	}
	if e := f.service.LinkTuyaCloud(ctx, owner, g3, "eu", "event", id3, cloudSecret); e != nil {
		t.Fatalf("after an unlink: %v", e)
	}
}

func countText(t *testing.T, f *fixture, q string, args ...any) string {
	t.Helper()
	var s string
	if e := f.admin.QueryRowContext(context.Background(), q, args...).Scan(&s); e != nil {
		t.Fatal(e)
	}
	return s
}
