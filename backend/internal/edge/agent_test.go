package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/backend/internal/tuyalocal"
	"aether/backend/internal/tuyalocal/tuyasim"
)

const (
	testGateway = "3f2c1d7e-1a2b-4c5d-8e9f-0a1b2c3d4e5f"
	testToken   = "tok_abcdefghijklmnopqrstuvwxyz0123456789ABC"
	testPass    = "mqtt-secret-password-value"
	testKey     = "0123456789abcdef"
	testDevice  = "bf1234567890abcdefgh"
)

type published struct {
	topic    string
	retained bool
	payload  []byte
}

// fakeBus records publishes and lets tests deliver commands.
type fakeBus struct {
	mu      sync.Mutex
	msgs    []published
	handler func(string, []byte)
	notify  chan struct{}
}

func newFakeBus() *fakeBus { return &fakeBus{notify: make(chan struct{}, 1000)} }

func (b *fakeBus) Publish(topic string, retained bool, payload []byte) error {
	b.mu.Lock()
	b.msgs = append(b.msgs, published{topic, retained, append([]byte(nil), payload...)})
	b.mu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default:
	}
	return nil
}
func (b *fakeBus) Subscribe(_ string, h func(string, []byte)) {
	b.mu.Lock()
	b.handler = h
	b.mu.Unlock()
}
func (b *fakeBus) Connected() bool { return true }
func (b *fakeBus) Close()          {}
func (b *fakeBus) deliver(topic string, payload []byte) {
	b.mu.Lock()
	h := b.handler
	b.mu.Unlock()
	h(topic, payload)
}
func (b *fakeBus) all() []published {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]published(nil), b.msgs...)
}

// waitFor polls the published messages until match accepts one, or fails the test.
func (b *fakeBus) waitFor(t *testing.T, d time.Duration, what string, match func(published) bool) published {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, m := range b.all() {
			if match(m) {
				return m
			}
		}
		select {
		case <-b.notify:
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s; published: %s", what, b.dump())
	return published{}
}

func (b *fakeBus) dump() string {
	var s []string
	for _, m := range b.all() {
		s = append(s, m.topic+" "+string(m.payload))
	}
	return strings.Join(s, " | ")
}

// fakeSource hands out a configuration the test controls.
type fakeSource struct {
	mu      sync.Mutex
	rev     int64
	devices []Device
	err     error
	served  int64
}

func (s *fakeSource) set(rev int64, devices []Device, err error) {
	s.mu.Lock()
	s.rev, s.devices, s.err = rev, devices, err
	s.mu.Unlock()
}

func (s *fakeSource) Fetch(context.Context) (int64, []Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, nil, s.err
	}
	if s.served == s.rev {
		return 0, nil, errNotModified
	}
	s.served = s.rev
	return s.rev, append([]Device(nil), s.devices...), nil
}

var fast = Timing{
	ConfigEvery: 40 * time.Millisecond, HealthEvery: time.Hour, DiscoveryEvery: time.Hour,
	Coalesce: 300 * time.Millisecond, OfflineAfter: 400 * time.Millisecond, BackoffMin: 40 * time.Millisecond,
	BackoffMax: 150 * time.Millisecond, Heartbeat: time.Second, DialTimeout: 2 * time.Second, LANFresh: time.Minute,
}

func testConfig(t *testing.T) Config {
	return Config{API: "https://aether.example", GatewayID: testGateway, HTTPToken: testToken, MQTTURL: "mqtts://broker:8883",
		MQTTUsername: "gw-" + testGateway, MQTTPassword: testPass, HealthFile: t.TempDir() + "/health"}
}

type rig struct {
	t      *testing.T
	bus    *fakeBus
	source *fakeSource
	agent  *Agent
	logs   *bytes.Buffer
	cancel context.CancelFunc
	done   chan struct{}
}

func start(t *testing.T, timing Timing) *rig {
	t.Helper()
	logs := &bytes.Buffer{}
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: logs, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := &rig{t: t, bus: newFakeBus(), source: &fakeSource{}, logs: logs, done: make(chan struct{})}
	r.agent = NewAgent(testConfig(t), r.bus, r.source, nil, timing, log)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.agent.Run(ctx); close(r.done) }()
	t.Cleanup(r.stop)
	return r
}

func (r *rig) stop() {
	r.cancel()
	<-r.done
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func simDevice(t *testing.T, v tuyalocal.Version, dps map[string]any, configure func(*tuyasim.Device)) (*tuyasim.Device, string) {
	t.Helper()
	d := tuyasim.New(testDevice, testKey, v, dps)
	if configure != nil {
		configure(d)
	}
	addr, e := d.Start()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(d.Close)
	return d, addr
}

func versionString(v tuyalocal.Version) string {
	return map[tuyalocal.Version]string{tuyalocal.V31: "3.1", tuyalocal.V33: "3.3", tuyalocal.V34: "3.4", tuyalocal.V35: "3.5"}[v]
}

func isAvailability(online bool, reason string) func(published) bool {
	return func(m published) bool {
		if m.topic != topicAvailability(testGateway, testDevice) || !m.retained {
			return false
		}
		var p availabilityPayload
		json.Unmarshal(m.payload, &p)
		if online {
			return p.State == "online"
		}
		return p.State == "offline" && p.Reason == reason
	}
}

func stateWith(key string, value any) func(published) bool {
	return func(m published) bool {
		if m.topic != topicState(testGateway, testDevice) {
			return false
		}
		var p struct {
			DPS map[string]any `json:"dps"`
		}
		if json.Unmarshal(m.payload, &p) != nil {
			return false
		}
		return p.DPS[key] == value
	}
}

// Every protocol version: connect, publish the full status, take a command, publish the device's confirmation.
func TestAgentStateAndCommand(t *testing.T) {
	for _, v := range []tuyalocal.Version{tuyalocal.V33, tuyalocal.V34, tuyalocal.V35} {
		t.Run(versionString(v), func(t *testing.T) {
			sim, addr := simDevice(t, v, map[string]any{"1": false, "19": 0.0}, nil)
			r := start(t, fast)
			r.source.set(1, []Device{{ID: testDevice, Key: testKey, Version: versionString(v), IP: addr}}, nil)
			r.bus.waitFor(t, 5*time.Second, "online", isAvailability(true, ""))
			full := r.bus.waitFor(t, 5*time.Second, "full state", stateWith("1", false))
			if !strings.Contains(string(full.payload), `"full":true`) || full.retained {
				t.Fatalf("full state: %s retained=%v", full.payload, full.retained)
			}
			r.bus.deliver("aether/edge/"+testGateway+"/"+testDevice+"/set", []byte(`{"dps":{"1":true}}`))
			r.bus.waitFor(t, 5*time.Second, "confirmation push", stateWith("1", true))
			if got := sim.DPS()["1"]; got != true {
				t.Fatalf("device DP 1 = %v", got)
			}
		})
	}
}

// Numbers pushed every few milliseconds are published at most once per coalescing period; a boolean goes out at once.
func TestCoalescer(t *testing.T) {
	// The period is long compared with the burst, so a loaded machine (the full suite runs packages in
	// parallel) cannot stretch the burst past it; the flush is awaited by polling, not a fixed sleep.
	const period = 2 * time.Second
	c := newCoalescer(period)
	var mu sync.Mutex
	var out []map[string]any
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx, func(dps map[string]any, _ bool) { mu.Lock(); out = append(out, dps); mu.Unlock() })
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(out) }
	waitFor := func(want int, within time.Duration) int {
		deadline := time.Now().Add(within)
		for count() < want && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return count()
	}
	start := time.Now()
	c.add(map[string]any{"19": json.Number("1")}, false) // first numeric: nothing published before, goes out at once
	if n := waitFor(1, time.Second); n != 1 {
		t.Fatalf("first numeric value not published at once: %d", n)
	}
	for i := 2; i <= 20; i++ {
		c.add(map[string]any{"19": json.Number(strings.Repeat("9", i%3+1))}, false)
	}
	if elapsed := time.Since(start); elapsed < period/2 {
		if n := count(); n != 1 {
			t.Fatalf("numeric burst inside the period published %d times", n)
		}
	}
	if n := waitFor(2, 2*period); n != 2 {
		t.Fatalf("latest numeric values not flushed after the period: %d", n)
	}
	c.add(map[string]any{"1": true}, false)
	if n := waitFor(3, time.Second); n != 3 {
		t.Fatalf("boolean change not published at once: %d", n)
	}
}

// A 3.4 device holding another key proves it during negotiation: reported at once as auth_failed.
func TestAgentReportsRejectedKey(t *testing.T) {
	_, addr := simDevice(t, tuyalocal.V34, map[string]any{"1": false}, func(d *tuyasim.Device) { d.RejectKey = true })
	r := start(t, fast)
	r.source.set(1, []Device{{ID: testDevice, Key: testKey, Version: "3.4", IP: addr}}, nil)
	r.bus.waitFor(t, 3*time.Second, "auth_failed", isAvailability(false, ReasonAuthFailed))
}

// A device whose only connection is held by another client resets ours: reported as busy after the debounce.
func TestAgentReportsBusy(t *testing.T) {
	_, addr := simDevice(t, tuyalocal.V33, map[string]any{"1": false}, func(d *tuyasim.Device) { d.SingleConnection = true })
	other, e := tuyalocal.Dial(context.Background(), tuyalocal.Options{Address: addr, DeviceID: testDevice, LocalKey: testKey, Version: tuyalocal.V33})
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	r := start(t, fast)
	r.source.set(1, []Device{{ID: testDevice, Key: testKey, Version: "3.3", IP: addr}}, nil)
	r.bus.waitFor(t, 5*time.Second, "busy", isAvailability(false, ReasonBusy))
	for _, m := range r.bus.all() {
		if isAvailability(true, "")(m) {
			t.Fatal("a device held by another client was announced online")
		}
	}
}

// A new revision without the device closes its connection; a refused credential stops everything.
func TestAgentReconcileAndUnauthorized(t *testing.T) {
	sim, addr := simDevice(t, tuyalocal.V34, map[string]any{"1": false}, nil)
	r := start(t, fast)
	dev := Device{ID: testDevice, Key: testKey, Version: "3.4", IP: addr}
	r.source.set(1, []Device{dev}, nil)
	r.bus.waitFor(t, 5*time.Second, "online", isAvailability(true, ""))
	r.source.set(2, nil, nil)
	waitUntil(t, 3*time.Second, "connection closed after removal", func() bool { return sim.Connections() == 0 })

	r.source.set(3, []Device{dev}, nil)
	waitUntil(t, 3*time.Second, "connection reopened", func() bool { return sim.Connections() == 1 })
	r.source.set(3, nil, ErrUnauthorized)
	waitUntil(t, 3*time.Second, "connection closed after the credential was refused", func() bool { return sim.Connections() == 0 })
}

// A command for a device this agent does not hold, or a malformed one, is refused without touching any device.
func TestAgentRefusesBadCommands(t *testing.T) {
	sim, addr := simDevice(t, tuyalocal.V34, map[string]any{"1": false}, nil)
	r := start(t, fast)
	r.source.set(1, []Device{{ID: testDevice, Key: testKey, Version: "3.4", IP: addr}}, nil)
	r.bus.waitFor(t, 5*time.Second, "online", isAvailability(true, ""))
	for _, c := range []struct{ topic, payload string }{
		{"aether/edge/" + testGateway + "/zz99999999999999999/set", `{"dps":{"1":true}}`},
		{"aether/edge/" + testGateway + "/" + testDevice + "/set", `{"dps":{"1":{"nested":true}}}`},
		{"aether/edge/" + testGateway + "/" + testDevice + "/set", `{"dps":{"300":true}}`},
		{"aether/edge/other-gateway/" + testDevice + "/set", `{"dps":{"1":true}}`},
	} {
		r.bus.deliver(c.topic, []byte(c.payload))
	}
	time.Sleep(200 * time.Millisecond)
	if sim.DPS()["1"] != false {
		t.Fatal("a refused command reached the device")
	}
}

// LAN broadcasts are collected and published only when the list changed, at the discovery tick.
func TestAgentDiscoveryList(t *testing.T) {
	a := NewAgent(testConfig(t), newFakeBus(), &fakeSource{}, nil, fast, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	bus := a.bus.(*fakeBus)
	from := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 6667}
	a.onBroadcast(tuyalocal.Broadcast{GwID: testDevice, IP: "192.168.1.50", Version: "3.3"}, from)
	a.onBroadcast(tuyalocal.Broadcast{GwID: "BAD ID", IP: "192.168.1.51", Version: "3.3"}, from)
	a.onBroadcast(tuyalocal.Broadcast{GwID: "bf00000000000000000x", IP: "", Version: "9.9"}, from)
	a.publishLAN(false)
	a.publishLAN(false) // unchanged: nothing more
	a.onBroadcast(tuyalocal.Broadcast{GwID: testDevice, IP: "192.168.1.50", Version: "3.3"}, from)
	a.publishLAN(false) // the same device again is not a change
	msgs := bus.all()
	if len(msgs) != 1 || msgs[0].topic != topicDiscovery(testGateway) {
		t.Fatalf("discovery publishes: %s", bus.dump())
	}
	var list []LANDevice
	if json.Unmarshal(msgs[0].payload, &list) != nil || len(list) != 1 || list[0].ID != testDevice || list[0].IP != "192.168.1.50" {
		t.Fatalf("list: %s", msgs[0].payload)
	}
	if addr, version := a.address(Device{ID: testDevice, IP: "10.0.0.9"}); addr != "192.168.1.50" || version != "3.3" {
		t.Fatalf("a device seen on the LAN is dialled at its broadcast address: %s %s", addr, version)
	}
	// A configured version is never overridden by a broadcast (no downgrade by a spoofed packet).
	if _, version := a.address(Device{ID: testDevice, IP: "10.0.0.9", Version: "3.5"}); version != "3.5" {
		t.Fatalf("configured version overridden: %s", version)
	}
}

// A broadcast claiming another address than its UDP source is dropped; the source is what counts.
func TestAgentDropsSpoofedBroadcasts(t *testing.T) {
	a := NewAgent(testConfig(t), newFakeBus(), &fakeSource{}, nil, fast, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	from := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 66), Port: 6667}
	a.onBroadcast(tuyalocal.Broadcast{GwID: testDevice, IP: "192.168.1.50", Version: "3.3"}, from)
	if len(a.lan) != 0 {
		t.Fatal("a broadcast whose claimed IP differs from its source was kept")
	}
	a.onBroadcast(tuyalocal.Broadcast{GwID: testDevice, IP: "", Version: "3.3"}, from)
	if a.lan[testDevice].dev.IP != "192.168.1.66" {
		t.Fatalf("source address not used: %+v", a.lan[testDevice])
	}
	a.onBroadcast(tuyalocal.Broadcast{GwID: testDevice, Version: "3.3"}, &net.IPAddr{IP: net.IPv4(1, 2, 3, 4)})
	if a.lan[testDevice].dev.IP != "192.168.1.66" {
		t.Fatal("a packet without a UDP source changed the entry")
	}
}

// When the LAN list is full the device heard longest ago is forgotten (never a configured one).
func TestAgentLANEvictsOldest(t *testing.T) {
	a := NewAgent(testConfig(t), newFakeBus(), &fakeSource{}, nil, fast, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	clock := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return clock }
	from := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 10), Port: 6667}
	id := func(i int) string { return fmt.Sprintf("bf%018d", i) }
	a.workers[id(0)] = &worker{} // configured: never evicted
	for i := 0; i < MaxLAN; i++ {
		clock = clock.Add(time.Second)
		a.onBroadcast(tuyalocal.Broadcast{GwID: id(i), Version: "3.3"}, from)
	}
	clock = clock.Add(time.Second)
	a.onBroadcast(tuyalocal.Broadcast{GwID: id(MaxLAN), Version: "3.3"}, from)
	if len(a.lan) != MaxLAN {
		t.Fatalf("list size %d", len(a.lan))
	}
	if _, kept := a.lan[id(MaxLAN)]; !kept {
		t.Fatal("new device refused on a full list")
	}
	if _, kept := a.lan[id(0)]; !kept {
		t.Fatal("configured device evicted")
	}
	if _, kept := a.lan[id(1)]; kept {
		t.Fatal("the oldest unconfigured device was not evicted")
	}
}

// The agent never writes its credentials or a local key into its logs.
func TestAgentLogsNoSecrets(t *testing.T) {
	_, addr := simDevice(t, tuyalocal.V34, map[string]any{"1": false}, func(d *tuyasim.Device) { d.RejectKey = true })
	r := start(t, fast)
	r.source.set(1, []Device{{ID: testDevice, Key: testKey, Version: "3.4", IP: addr}}, nil)
	r.bus.waitFor(t, 3*time.Second, "auth_failed", isAvailability(false, ReasonAuthFailed))
	r.bus.deliver("aether/edge/"+testGateway+"/"+testDevice+"/set", []byte(`{"dps":{"1":true}}`))
	r.source.set(1, nil, ErrUnauthorized)
	time.Sleep(150 * time.Millisecond)
	r.stop()
	logs := r.logs.String()
	for _, secret := range []string{testKey, testToken, testPass} {
		if strings.Contains(logs, secret) {
			t.Fatalf("secret %q in logs:\n%s", secret[:4], logs)
		}
	}
	if strings.Contains(testConfig(t).String(), testPass) || strings.Contains(testConfig(t).String(), testToken) {
		t.Fatal("Config.String leaks a secret")
	}
	for _, m := range r.bus.all() {
		if bytes.Contains(m.payload, []byte(testKey)) {
			t.Fatal("a local key was published")
		}
	}
}

// Goodbye: on shutdown the agent marks itself offline (retained).
func TestAgentGoodbye(t *testing.T) {
	r := start(t, fast)
	r.stop()
	msgs := r.bus.all()
	last := msgs[len(msgs)-1]
	if last.topic != topicStatus(testGateway) || !last.retained || !strings.Contains(string(last.payload), `"offline"`) {
		t.Fatalf("goodbye: %s", r.bus.dump())
	}
	if msgs[0].topic != topicStatus(testGateway) || !strings.Contains(string(msgs[0].payload), `"online"`) {
		t.Fatalf("hello: %s", r.bus.dump())
	}
}

func TestParseCommand(t *testing.T) {
	dps, e := parseCommand([]byte(`{"dps":{"1":true,"2":25,"4":"white","19":12.5}}`))
	if e != nil || dps["1"] != true || dps["2"] != json.Number("25") || dps["4"] != "white" || dps["19"] != json.Number("12.5") {
		t.Fatalf("%v %v", dps, e)
	}
	for _, bad := range []string{``, `{}`, `{"dps":{}}`, `{"dps":{"0":true}}`, `{"dps":{"01":true}}`, `{"dps":{"x":1}}`,
		`{"dps":{"1":null}}`, `{"dps":{"1":[1]}}`, `not json`, `{"dps":{"1":"` + strings.Repeat("a", 1100) + `"}}`} {
		if _, e := parseCommand([]byte(bad)); !errors.Is(e, errBadCommand) {
			t.Fatalf("accepted %.40q", bad)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{"AETHER_API": "https://aether.example/", "GATEWAY_ID": testGateway, "HTTP_TOKEN": testToken,
		"MQTT_URL": "mqtts://aether.example:8883", "MQTT_PASSWORD": testPass, "MQTT_CA_FILE": "/config/ca.crt"}
	c, e := LoadConfig(func(k string) string { return env[k] })
	if e != nil || c.API != "https://aether.example" || c.MQTTUsername != "gw-"+testGateway || c.HealthFile != DefaultHealthFile {
		t.Fatalf("%+v %v", c, e)
	}
	if b, _ := brokerURL(c.MQTTURL); b != "ssl://aether.example:8883" {
		t.Fatalf("broker %s", b)
	}
	for key, bad := range map[string]string{"AETHER_API": "ftp://x", "GATEWAY_ID": "nope", "HTTP_TOKEN": "short", "MQTT_URL": "https://x",
		"MQTT_CA_FILE": "", "MQTT_USERNAME": "someone-else", "LOG_LEVEL": "loud"} {
		broken := map[string]string{}
		for k, v := range env {
			broken[k] = v
		}
		broken[key] = bad
		if _, e := LoadConfig(func(k string) string { return broken[k] }); e == nil {
			t.Fatalf("accepted %s=%q", key, bad)
		} else if strings.Contains(e.Error(), testPass) || strings.Contains(e.Error(), testToken) {
			t.Fatal("configuration error leaks a secret")
		}
	}
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}
