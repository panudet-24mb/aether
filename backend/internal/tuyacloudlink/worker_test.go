package tuyacloudlink

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/tuyacloudlink/cloudkeys"
	"aether/backend/internal/tuyacloudlink/fakecloud"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTenant  = "11111111-1111-4111-8111-111111111111"
	testGateway = "22222222-2222-4222-8222-222222222222"
	testID      = "testaccessid0001"
	testSecret  = "abcdefghijklmnopqrstuvwxyz012345"
	testDevice  = "bf00000000000000sw01"
)

// testKey is the worker's key pair in these tests (a fixed private key, not a secret).
var testKey = func() *cloudkeys.Key {
	var private [32]byte
	copy(private[:], "unit test private key, not real!")
	k, e := cloudkeys.NewKey(&private)
	if e != nil {
		panic(e)
	}
	return k
}()

// fakeStore is the database half in memory.
type fakeStore struct {
	mu         sync.Mutex
	revision   int64
	sealed     string
	states     []string
	captured   [][]tuyacloud.Event
	guardFlags []bool
	failNext   int // transient capture failures still to come
	usage      postgres.CloudUsage
	health     int
	saved      []postgres.CloudImport
	syncAt     *time.Time
	registered []string
	commands   []domain.Command
	status     map[string]string
	failed     map[string]string
	extra      []domain.CloudLinkRef // further links, of other gateways, whose configuration never reads (a database down)
	openFail   int                   // transient OpenLinkRow failures of the main link still to come
	panicNext  int                   // captures that panic, still to come
}

func newFakeStore(t *testing.T) *fakeStore {
	sealed, e := security.SealTuyaCloud(testKey.Public(), testTenant, testGateway, testID, testSecret)
	if e != nil {
		t.Fatal(e)
	}
	return &fakeStore{revision: 1, sealed: sealed, status: map[string]string{}, failed: map[string]string{}}
}

func (s *fakeStore) CloudLinks(context.Context) ([]domain.CloudLinkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]domain.CloudLinkRef(nil), s.extra...)
	if s.sealed != "" {
		out = append(out, domain.CloudLinkRef{TenantID: testTenant, GatewayID: testGateway, Revision: s.revision, State: "linking"})
	}
	return out, nil
}
func (s *fakeStore) OpenLinkRow(_ context.Context, tenant, gateway string) (domain.CloudLinkSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.extra {
		if x.GatewayID == gateway {
			return domain.CloudLinkSecret{}, errors.New("connection refused")
		}
	}
	if tenant != testTenant || gateway != testGateway || s.sealed == "" {
		return domain.CloudLinkSecret{}, domain.ErrNotFound
	}
	if s.openFail > 0 {
		s.openFail--
		return domain.CloudLinkSecret{}, errors.New("connection refused")
	}
	return domain.CloudLinkSecret{TenantID: tenant, GatewayID: gateway, Region: "eu", Channel: "event", CredentialsSealed: s.sealed, Revision: s.revision}, nil
}
func (s *fakeStore) SetCloudLinkState(_ context.Context, _, _ string, revision int64, state, reason string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision != s.revision {
		return domain.ErrNotFound
	}
	s.states = append(s.states, state+":"+reason)
	return nil
}
func (s *fakeStore) CaptureTuyaCloud(_ context.Context, _, _ string, _ int, events []tuyacloud.Event, registeredOnly bool) (postgres.CloudCaptureResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext > 0 {
		s.failNext--
		return postgres.CloudCaptureResult{}, errors.New("connection reset")
	}
	if s.panicNext > 0 {
		s.panicNext--
		panic("injected fault")
	}
	s.captured = append(s.captured, events)
	s.guardFlags = append(s.guardFlags, registeredOnly)
	res := postgres.CloudCaptureResult{Applied: len(events)}
	for _, ev := range events {
		if ev.DevID == "bf000000000000unknown" {
			res.SyncRequested = true
		}
	}
	return res, nil
}
func (s *fakeStore) SaveCloudDevices(_ context.Context, _, _ string, d []postgres.CloudImport) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, d...)
	return len(d), nil
}
func (s *fakeStore) CloudSyncWanted(context.Context, string, string) (*time.Time, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncAt, s.registered, nil
}
func (s *fakeStore) CompleteCloudSync(_ context.Context, _, _ string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.syncAt != nil && !s.syncAt.After(at) {
		s.syncAt = nil
	}
	return nil
}
func (s *fakeStore) RecordCloudUsage(_ context.Context, _, _ string, add postgres.CloudUsage, _ *time.Time, _ time.Time) (postgres.CloudUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.Events += add.Events
	s.usage.APICalls += add.APICalls
	s.usage.Dropped += add.Dropped
	return s.usage, nil
}
func (s *fakeStore) RecordCloudHealth(context.Context, string, string, postgres.CloudHealth) error {
	s.mu.Lock()
	s.health++
	s.mu.Unlock()
	return nil
}

// commander.Store: an outbox of cloud commands.
func (s *fakeStore) ActiveTenants(context.Context) ([]string, error) {
	return []string{testTenant}, nil
}
func (s *fakeStore) ProcessAutomationRequests(context.Context, string, time.Time) (int, error) {
	return 0, nil
}
func (s *fakeStore) PendingCommandGateways(_ context.Context, _ string, transports []string, _ time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.commands {
		if s.status[c.ID] == "" && slices.Contains(transports, c.Transport) {
			return []string{c.GatewayID}, nil
		}
	}
	return nil, nil
}
func (s *fakeStore) ClaimCommands(_ context.Context, _, gateway string, transports []string, _ time.Time, limit int) ([]domain.Command, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []domain.Command{}
	for _, c := range s.commands {
		if len(out) < limit && c.GatewayID == gateway && s.status[c.ID] == "" && slices.Contains(transports, c.Transport) {
			s.status[c.ID] = "sent"
			out = append(out, c)
		}
	}
	return out, nil
}
func (s *fakeStore) MarkCommandsPublished(_ context.Context, _ string, ids []string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.status[id] = "published"
	}
	return nil
}
func (s *fakeStore) MarkCommandFailed(_ context.Context, _, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id], s.failed[id] = "failed", reason
	return nil
}
func (s *fakeStore) TimeoutCommands(context.Context, string, time.Time) (int, error) { return 0, nil }

func (s *fakeStore) snapshot() (states []string, captured int, guards []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.states...), len(s.captured), append([]bool(nil), s.guardFlags...)
}

type rig struct {
	t     *testing.T
	store *fakeStore
	mq    *fakecloud.MQ
	api   *fakecloud.API
	w     *Worker
	ctx   context.Context
}

func newRig(t *testing.T) *rig {
	store := newFakeStore(t)
	mq := fakecloud.NewMQ(t, testID, testSecret)
	api := fakecloud.NewAPI(t, testID)
	w := &Worker{Store: store, Keys: []*cloudkeys.Key{testKey}, ConsumerURL: mq.URL, NewClient: api.Client, TLS: mq.TLS(),
		Timings: Timings{Health: 50 * time.Millisecond, Ping: 50 * time.Millisecond, ReadTimeout: 2 * time.Second, BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond, SyncEvery: time.Hour}}
	return &rig{t: t, store: store, mq: mq, api: api, w: w}
}

func (r *rig) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	r.t.Cleanup(func() { cancel(); r.w.Wait() })
	r.reconcile()
}

func (r *rig) reconcile() {
	r.t.Helper()
	if e := r.w.Reconcile(r.ctx); e != nil {
		r.t.Fatal(e)
	}
}

func (r *rig) waitStates(want string) {
	r.t.Helper()
	if !r.mq.WaitFor(3*time.Second, func() bool {
		states, _, _ := r.store.snapshot()
		return slices.ContainsFunc(states, func(s string) bool { return strings.HasPrefix(s, want) })
	}) {
		states, _, _ := r.store.snapshot()
		r.t.Fatalf("state %q never recorded: %v", want, states)
	}
}

func status(value bool, t int64) string {
	return fakecloud.Status(testDevice, t, map[string]any{"switch_1": value})
}

// The consumer authenticates with the derived password, never proxies, stores good messages once, and
// acknowledges and drops everything that could never be stored.
func TestConsumerMessages(t *testing.T) {
	r := newRig(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1") // an environment proxy must not see the password
	r.start()
	r.waitStates("online")
	h := r.mq.Headers()[0]
	if h.Get("username") != testID || h.Get("password") != tuyacloud.Password(testID, testSecret) {
		t.Fatalf("handshake headers: %v", h)
	}
	r.mq.Push(fakecloud.Message{ID: "ecb", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(true, 1000)})
	r.mq.Push(fakecloud.Message{ID: "gcm", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(false, 2000), GCM: true})
	r.mq.Push(fakecloud.Message{ID: "ecb", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(true, 1000)}) // redelivered after a lost ack
	r.mq.Push(fakecloud.Message{ID: "tampered", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(true, 3000), GCM: true, Corrupt: true})
	r.mq.Push(fakecloud.Message{ID: "too-often", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(true, 4000), Redelivery: MaxRedelivery + 1})
	r.mq.Push(fakecloud.Message{ID: "firmware", Protocol: 9999, Plaintext: `{"devId":"x"}`})
	r.mq.Push(fakecloud.Message{ID: "not-json", Protocol: tuyacloud.ProtocolStatus, Plaintext: `{"devId":`})
	r.mq.PushRaw([]byte(`{"messageId":"poison","payload":"%%%"}`))
	r.mq.PushRaw([]byte(`{"messageId":"huge","payload":"` + strings.Repeat("A", tuyacloud.MaxFrame) + `"}`))
	r.mq.Push(fakecloud.Message{ID: "last", Protocol: tuyacloud.ProtocolDevice, Plaintext: fakecloud.Device(testDevice, "offline", 5000, "")})
	if !r.mq.WaitAck("last", 5*time.Second) {
		t.Fatalf("acks: %v", r.mq.Acked())
	}
	for _, id := range []string{"ecb", "gcm", "tampered", "too-often", "firmware", "not-json", "poison", "huge"} {
		if !slices.Contains(r.mq.Acked(), id) {
			t.Fatalf("%s not acknowledged: %v", id, r.mq.Acked())
		}
	}
	_, captured, _ := r.store.snapshot()
	if captured != 3 { // ecb, gcm, last: never the duplicate or anything dropped
		t.Fatalf("captured %d", captured)
	}
	r.store.mu.Lock()
	first := r.store.captured[0][0]
	r.store.mu.Unlock()
	if first.DevID != testDevice || len(first.Items) != 1 || string(first.Items[0].Value) != "true" {
		t.Fatalf("event: %+v", first)
	}
	if len(r.mq.Nacked()) != 0 {
		t.Fatalf("nacked: %v", r.mq.Nacked())
	}
	// Usage: every delivered message is billed (3 stored, 1 unknown protocol, 5 dropped); the duplicate is not.
	// Dropped: tampered, too-often, not-json, poison, huge.
	if !r.mq.WaitFor(2*time.Second, func() bool {
		r.store.mu.Lock()
		defer r.store.mu.Unlock()
		return r.store.usage.Events == 9 && r.store.usage.Dropped == 5
	}) {
		t.Fatalf("usage: %+v", r.store.usage)
	}
}

// A transient database failure asks Pulsar for a redelivery (nack), reconnects, and stores the redelivered message.
func TestConsumerNacksTransientFailure(t *testing.T) {
	r := newRig(t)
	r.store.failNext = 1
	r.start()
	r.waitStates("online")
	r.mq.Push(fakecloud.Message{ID: "retry", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(true, 1000)})
	if !r.mq.WaitAck("retry", 5*time.Second) {
		t.Fatalf("acks %v nacks %v", r.mq.Acked(), r.mq.Nacked())
	}
	if !slices.Contains(r.mq.Nacked(), "retry") {
		t.Fatalf("not nacked first: %v", r.mq.Nacked())
	}
	if _, captured, _ := r.store.snapshot(); captured != 1 {
		t.Fatalf("captured %d", captured)
	}
	if r.mq.Dials() < 2 {
		t.Fatalf("did not reconnect: %d dials", r.mq.Dials())
	}
}

// A dropped connection is re-established with backoff; the link reports offline, then online again.
func TestConsumerReconnects(t *testing.T) {
	r := newRig(t)
	r.start()
	r.waitStates("online")
	r.mq.Drop()
	r.waitStates("offline:mq_disconnected")
	if !r.mq.WaitFor(3*time.Second, func() bool {
		states, _, _ := r.store.snapshot()
		return len(states) >= 3 && states[len(states)-1] == "online:"
	}) {
		states, _, _ := r.store.snapshot()
		t.Fatalf("states: %v", states)
	}
}

// A refused handshake parks the link: auth_failed is recorded and the consumer stops dialling.
func TestConsumerAuthFailed(t *testing.T) {
	r := newRig(t)
	r.mq.Reject(http.StatusUnauthorized)
	r.start()
	r.waitStates("auth_failed:mq_auth")
	dials := r.mq.Dials()
	time.Sleep(300 * time.Millisecond)
	if r.mq.Dials() != dials {
		t.Fatalf("kept dialling: %d -> %d", dials, r.mq.Dials())
	}
	// Reconcile with the same revision leaves it parked; a new revision (credentials saved again) restarts it.
	r.reconcile()
	if r.mq.Dials() != dials {
		t.Fatalf("parked link restarted: %d", r.mq.Dials())
	}
	r.mq.Reject(0)
	r.store.mu.Lock()
	r.store.revision = 2
	r.store.mu.Unlock()
	r.reconcile()
	r.waitStates("online")
}

// Credentials sealed for another gateway (or with another key) never open: the link parks as auth_failed.
func TestLinkRefusesForeignCredentials(t *testing.T) {
	r := newRig(t)
	other, _ := security.SealTuyaCloud(testKey.Public(), testTenant, "33333333-3333-4333-8333-333333333333", testID, testSecret)
	r.store.sealed = other
	r.start()
	r.waitStates("auth_failed:credentials_unreadable")
	if r.mq.Dials() != 0 {
		t.Fatal("dialled without credentials")
	}
}

// Unlinking (no link listed any more) stops the consumer.
func TestReconcileStopsUnlinked(t *testing.T) {
	r := newRig(t)
	r.start()
	r.waitStates("online")
	r.store.mu.Lock()
	r.store.sealed = ""
	r.store.mu.Unlock()
	r.reconcile()
	if len(r.w.tenants()) != 0 {
		t.Fatal("link still running")
	}
}

func specFixture(t *testing.T, name string) json.RawMessage {
	b, e := os.ReadFile("../adapters/tuya/testdata/" + name)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// A sync request lists the devices with their specifications (no key) and reads the registered devices' current
// values once; an unknown device in an event asks for one.
func TestSyncAndSnapshot(t *testing.T) {
	r := newRig(t)
	at := time.Now()
	r.store.syncAt, r.store.registered = &at, []string{testDevice}
	r.api.SetDevices(fakecloud.CloudDevice{ID: testDevice, Name: "Switch", Category: "kg", ProductID: "p1", Spec: specFixture(t, "kg_3gang.json"),
		Properties: []tuyacloud.Property{{Code: "switch_1", DPID: 1, Time: 1000, Value: json.RawMessage(`true`)}}})
	r.start()
	if !r.mq.WaitFor(3*time.Second, func() bool {
		r.store.mu.Lock()
		defer r.store.mu.Unlock()
		return len(r.store.saved) == 1 && len(r.store.captured) == 1 && r.store.syncAt == nil
	}) {
		t.Fatalf("saved %v captured %v sync %v", r.store.saved, r.store.captured, r.store.syncAt)
	}
	r.store.mu.Lock()
	d := r.store.saved[0]
	snap := r.store.captured[0][0]
	r.store.mu.Unlock()
	if d.TuyaID != testDevice || len(d.Spec) != 5 || d.Category != "kg" || snap.Items[0].Code != "switch_1" {
		t.Fatalf("saved %+v snapshot %+v", d, snap)
	}
	if r.api.Calls("devices") != 1 || r.api.Calls("specifications") != 1 || r.api.Calls("properties") != 1 {
		t.Fatalf("calls: devices %d specs %d props %d", r.api.Calls("devices"), r.api.Calls("specifications"), r.api.Calls("properties"))
	}
}

// Commands: the dispatcher claims only cloud commands, issues {"code":raw} through the OpenAPI, and maps Tuya's
// refusals to stored reasons; the trial guard refuses commands and marks events registered-only.
func TestDeliverCommands(t *testing.T) {
	r := newRig(t)
	r.start()
	r.waitStates("online")
	cmd := func(id, device, transport string) domain.Command {
		return domain.Command{ID: id, TenantID: testTenant, GatewayID: testGateway, IEEE: device, Property: "state_l1", Transport: transport,
			Wire: json.RawMessage(`{"switch_1":true}`), Value: json.RawMessage(`"ON"`)}
	}
	r.api.Fail("bf00000000000000off1", 2001)
	r.api.Fail("bf00000000000000lim1", http.StatusTooManyRequests)
	r.store.mu.Lock()
	r.store.commands = []domain.Command{cmd("ok", testDevice, "cloud"), cmd("offline", "bf00000000000000off1", "cloud"),
		cmd("quota", "bf00000000000000lim1", "cloud"), cmd("zigbee", "0xa4c1380000000002", "z2m")}
	r.store.mu.Unlock()
	d := r.w.Dispatcher()
	n, e := d.RunOnce(context.Background())
	if e != nil || n != 1 {
		t.Fatalf("run: %d %v", n, e)
	}
	issues := r.api.Issues()
	if len(issues) != 1 || issues[0].Device != testDevice || string(issues[0].Properties["switch_1"]) != "true" {
		t.Fatalf("issues: %+v", issues)
	}
	r.store.mu.Lock()
	failed, zigbee := r.store.failed, r.store.status["zigbee"]
	r.store.mu.Unlock()
	if failed["offline"] != ReasonOffline || failed["quota"] != ReasonQuota || zigbee != "" {
		t.Fatalf("failed %v zigbee %q", failed, zigbee)
	}
	r.waitStates("quota:api_quota")

	// The trial guard: past 95 % of the budget commands are refused and events are stored registered-only.
	l := r.w.linkFor(testTenant, testGateway)
	l.usage.budget = Budget{EventsMonth: 100}
	l.usage.settle(postgres.CloudUsage{Events: 95})
	r.store.mu.Lock()
	r.store.commands = append(r.store.commands, cmd("guarded", testDevice, "cloud"))
	r.store.mu.Unlock()
	if _, e := d.RunOnce(context.Background()); e != nil {
		t.Fatal(e)
	}
	r.store.mu.Lock()
	reason := r.store.failed["guarded"]
	r.store.mu.Unlock()
	if reason != ReasonQuotaNear || len(r.api.Issues()) != 1 {
		t.Fatalf("guarded command: %q, issues %d", reason, len(r.api.Issues()))
	}
	r.mq.Push(fakecloud.Message{ID: "guarded-event", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(true, 9000)})
	if !r.mq.WaitAck("guarded-event", 3*time.Second) {
		t.Fatal("not acknowledged")
	}
	_, _, guards := r.store.snapshot()
	if len(guards) == 0 || !guards[len(guards)-1] {
		t.Fatalf("guard flag: %v", guards)
	}
}

func TestLimits(t *testing.T) {
	for n := 0; n < 20; n++ {
		d := backoff(n, time.Second, 5*time.Minute)
		ceiling := time.Second << min(n, 9)
		if ceiling > 5*time.Minute {
			ceiling = 5 * time.Minute
		}
		if d < ceiling/2 || d > ceiling {
			t.Fatalf("backoff(%d) = %v, want within [%v, %v]", n, d, ceiling/2, ceiling)
		}
	}
	now := time.Unix(0, 0)
	b := newBucket(20, 20, func() time.Time { return now })
	for i := 0; i < 20; i++ {
		if b.reserve() != 0 {
			t.Fatalf("token %d had to wait", i)
		}
	}
	if w := b.reserve(); w <= 0 || w > 60*time.Millisecond {
		t.Fatalf("21st token waits %v", w)
	}
	s := newSeenIDs(2)
	s.add("a")
	s.add("b")
	s.add("a")
	s.add("c")
	if !s.has("a") || s.has("b") || !s.has("c") {
		t.Fatal("LRU evicted the wrong id")
	}
	u := &usage{budget: Budget{EventsMonth: 100, APICallsMonth: 10}}
	u.add(0, 9, 0)
	if u.guarded() {
		t.Fatal("guard on at 90 %")
	}
	u.add(0, 1, 0)
	if !u.guarded() {
		t.Fatal("guard off at 100 % of API calls")
	}
	if (&usage{}).guarded() {
		t.Fatal("no budget must mean no guard")
	}
	if leadingID([]byte(` {"messageId" : "abc:1:2", "payload":"x`)) != "abc:1:2" || leadingID([]byte(`{"payload":"x","messageId":"a"}`)) != "" {
		t.Fatal("leading id")
	}
}

// A panic in one link is recovered and that link alone is restarted; the message is redelivered and stored.
func TestPanicRestartsTheLink(t *testing.T) {
	r := newRig(t)
	r.store.panicNext = 1
	r.start()
	r.waitStates("online")
	first := r.w.linkFor(testTenant, testGateway)
	r.mq.Push(fakecloud.Message{ID: "fault", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(true, 1000)})
	if !r.mq.WaitAck("fault", 5*time.Second) {
		t.Fatalf("not stored after the restart: acks %v", r.mq.Acked())
	}
	if _, captured, _ := r.store.snapshot(); captured != 1 {
		t.Fatalf("captured %d", captured)
	}
	if next := r.w.linkFor(testTenant, testGateway); next == first || next == nil || next.isParked() {
		t.Fatal("link not replaced by a fresh one")
	}
}

// A transient database failure while reading the link's configuration is retried, not parked.
func TestOpenRetriesTransientFailures(t *testing.T) {
	r := newRig(t)
	r.store.openFail = 2
	r.start()
	r.waitStates("online")
	if l := r.w.linkFor(testTenant, testGateway); l == nil || l.isParked() {
		t.Fatal("link parked on a transient failure")
	}
}

func ref(tenant, gateway string) domain.CloudLinkRef {
	return domain.CloudLinkRef{TenantID: tenant, GatewayID: gateway, Revision: 1}
}

// Slots go round-robin across tenants, a tenant holds at most its share, and parked links hold none.
func TestLinkSlots(t *testing.T) {
	order := fairOrder([]domain.CloudLinkRef{ref("a", "a3"), ref("a", "a1"), ref("c", "c1"), ref("a", "a2"), ref("b", "b1"), ref("c", "c2")})
	got := []string{}
	for _, r := range order {
		got = append(got, r.GatewayID)
	}
	if strings.Join(got, ",") != "a1,b1,c1,a2,c2,a3" {
		t.Fatalf("order: %v", got)
	}

	r := newRig(t)
	other := "33333333-3333-4333-8333-333333333333"
	r.store.extra = []domain.CloudLinkRef{ref(testTenant, "44444444-4444-4444-8444-444444444441"), ref(testTenant, "44444444-4444-4444-8444-444444444442"),
		ref(other, "55555555-5555-4555-8555-555555555551")}
	foreign, _ := security.SealTuyaCloud(testKey.Public(), testTenant, other, testID, testSecret)
	r.store.sealed = foreign // the main link parks: its credentials do not open
	r.w.MaxLinks, r.w.MaxLinksPerTenant = 3, 2
	r.start()
	r.waitStates("auth_failed:credentials_unreadable")
	running := func() (live int, tenants map[string]int) {
		r.w.mu.Lock()
		defer r.w.mu.Unlock()
		tenants = map[string]int{}
		for _, l := range r.w.links {
			if !l.isParked() {
				live++
				tenants[l.tenant]++
			}
		}
		return live, tenants
	}
	// First reconcile: the other tenant got its slot although the first tenant has more links; the first tenant
	// holds two (one of them parked).
	r.reconcile()
	live, tenants := running()
	if tenants[other] != 1 || tenants[testTenant] != 2 || live != 3 {
		t.Fatalf("live %d per tenant %v", live, tenants)
	}
}

// A frame beyond the read limit whose id leads it is acknowledged before the connection closes, so it is not
// redelivered; one whose id cannot be read keeps closing connections, and after OversizeCloses in a row the link
// reports it and backs off the longest.
func TestOversizeFrames(t *testing.T) {
	r := newRig(t)
	r.start()
	r.waitStates("online")
	r.mq.PushRaw([]byte(`{"messageId":"big","payload":"` + strings.Repeat("A", DrainLimit) + `"}`))
	r.mq.Push(fakecloud.Message{ID: "after", Protocol: tuyacloud.ProtocolStatus, Plaintext: status(true, 1000)})
	if !r.mq.WaitAck("big", 5*time.Second) || !r.mq.WaitAck("after", 5*time.Second) {
		t.Fatalf("acks: %v", r.mq.Acked())
	}
	r.mq.PushRawTracked("noid", []byte(`{"payload":"`+strings.Repeat("A", DrainLimit)+`","messageId":"noid"}`))
	r.waitStates("offline:mq_oversize_frames")
}
