package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/backend/internal/edge/ble"
	"aether/backend/internal/edge/ble/bletest"
	"aether/backend/internal/tuyable"
	"aether/backend/internal/tuyable/tuyablesim"
	"aether/backend/internal/tuyalocal"
)

const (
	bleDevice1 = "bf00000000000000ble1"
	bleMAC     = "dc:23:4d:00:00:01"
	bleUUID    = "tuya1234abcd5678"
)

var fastBLE = ble.Timing{
	Tick: 5 * time.Millisecond, Fresh: time.Second, NotFoundAfter: time.Second, OfflineAfter: 0,
	Connect: time.Second, Handshake: 300 * time.Millisecond, Session: 2 * time.Second, Idle: 150 * time.Millisecond,
	CommandTTL: 2 * time.Second, BackoffMin: 40 * time.Millisecond, BackoffMax: 200 * time.Millisecond,
	SightingsEvery: 20 * time.Millisecond, SightingsForce: time.Second, ScanRetry: 30 * time.Millisecond,
	MinPoll: 100 * time.Millisecond, DefaultPoll: time.Hour,
}

func bleConfigDevice() Device {
	return Device{ID: bleDevice1, Key: testKey, Transport: "ble", MAC: bleMAC, UUID: bleUUID, ProductID: "gvygg3m8", Protocol: 3,
		Mode: "on_demand", DPTypes: map[string]string{"1": "bool", "2": "value"}}
}

// startBLE is start() with the BLE transport over a fake radio, and health published often.
func startBLE(t *testing.T, radio ble.Radio) *rig {
	t.Helper()
	logs := &bytes.Buffer{}
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: logs, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := &rig{t: t, bus: newFakeBus(), source: &fakeSource{}, logs: logs, done: make(chan struct{})}
	timing := fast
	timing.HealthEvery = 50 * time.Millisecond
	r.agent = NewAgent(testConfig(t), r.bus, r.source, nil, timing, log).WithBLE(radio, 1, fastBLE)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.agent.Run(ctx); close(r.done) }()
	t.Cleanup(r.stop)
	return r
}

// A BLE device in the configuration goes to the BLE transport: a command from the server reaches it, its
// confirming report is published on the same state topic a Wi-Fi device uses, its availability too, the sightings
// go to …/ble and the heartbeat carries the radio's state.
func TestAgentBLEDevice(t *testing.T) {
	sim := &tuyablesim.Device{LocalKey: testKey, UUID: bleUUID, DeviceID: bleDevice1, ProductID: "gvygg3m8", Protocol: 3,
		DPs: []tuyable.DP{{ID: 1, Type: tuyable.DPBool, Value: false}, {ID: 2, Type: tuyable.DPValue, Value: int64(7)}}}
	radio := bletest.New()
	radio.Add(bleMAC, sim, -55)
	r := startBLE(t, radio)
	r.source.set(1, []Device{bleConfigDevice()}, nil)
	waitUntil(t, 2*time.Second, "device configured", func() bool { return r.agent.ble.Has(bleDevice1) })
	r.bus.deliver("aether/edge/"+testGateway+"/"+bleDevice1+"/set", []byte(`{"dps":{"1":true}}`))
	r.bus.waitFor(t, 3*time.Second, "state from the BLE device", func(m published) bool {
		return m.topic == topicState(testGateway, bleDevice1) && strings.Contains(string(m.payload), `"1":true`)
	})
	r.bus.waitFor(t, 3*time.Second, "online", func(m published) bool {
		return m.topic == topicAvailability(testGateway, bleDevice1) && m.retained && string(m.payload) == `{"state":"online"}`
	})
	r.bus.waitFor(t, 3*time.Second, "sightings", func(m published) bool {
		return m.topic == topicBLE(testGateway) && strings.Contains(string(m.payload), `"mac":"`+bleMAC+`"`) && strings.Contains(string(m.payload), `"uuid":"`+bleUUID+`"`)
	})
	r.bus.waitFor(t, 3*time.Second, "health with the radio", func(m published) bool {
		var h healthPayload
		return m.topic == topicHealth(testGateway) && json.Unmarshal(m.payload, &h) == nil && h.BLE != nil && h.BLE.State == ble.StateOK &&
			h.BLE.Adapter == "hci-test" && h.BLE.Seen == 1
	})
	if sim.State()[1].Value != true {
		t.Fatal("the device did not take the command")
	}
	// No key ends up in the logs.
	if strings.Contains(r.logs.String(), testKey) {
		t.Fatal("a key was logged")
	}
}

// Without BLE_ENABLED the agent keeps BLE devices out (the server should not send them, but it never tries one),
// and its heartbeat says the radio is off.
func TestAgentWithoutBLE(t *testing.T) {
	r := start(t, fast)
	r.source.set(1, []Device{bleConfigDevice()}, nil)
	time.Sleep(150 * time.Millisecond)
	r.agent.mu.Lock()
	workers := len(r.agent.workers)
	r.agent.mu.Unlock()
	if workers != 0 {
		t.Fatal("a BLE device was dialled over the LAN")
	}
	r.agent.health()
	r.bus.waitFor(t, time.Second, "health", func(m published) bool {
		return m.topic == topicHealth(testGateway) && strings.Contains(string(m.payload), `"ble":{"state":"off"`)
	})
}

// The poller announces Bluetooth only when enabled, and keeps only well-formed BLE entries; without the capability
// it keeps none.
func TestPollerBLE(t *testing.T) {
	var caps string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caps = r.Header.Get("X-Aether-Edge-Caps")
		w.Header().Set("ETag", `"3;ble"`)
		w.Write([]byte(`{"revision":3,"devices":[
			{"id":"` + testDevice + `","key":"` + testKey + `","version":"3.3"},
			{"id":"bf00000000000000ble1","key":"` + testKey + `","transport":"ble","mac":"DC:23:4D:00:00:01","uuid":"tuya1234abcd5678","protocol":3,"mode":"auto","poll":900,"dp_types":{"1":"bool","3":"enum"}},
			{"id":"bf00000000000000ble2","key":"` + testKey + `","transport":"ble","uuid":"onlyuuid00000001"},
			{"id":"bf00000000000000ble3","key":"` + testKey + `","transport":"ble"},
			{"id":"bf00000000000000ble4","key":"` + testKey + `","transport":"ble","mac":"not-a-mac"},
			{"id":"bf00000000000000ble5","key":"` + testKey + `","transport":"ble","mac":"dc:23:4d:00:00:05","sec_key":"short"},
			{"id":"bf00000000000000ble6","key":"` + testKey + `","transport":"ble","mac":"dc:23:4d:00:00:06","dp_types":{"300":"bool"}},
			{"id":"bf00000000000000ble7","key":"` + testKey + `","transport":"ble","mac":"dc:23:4d:00:00:07","mode":"always"},
			{"id":"bf00000000000000zzz8","key":"` + testKey + `","transport":"zigbee"}]}`))
	}))
	defer srv.Close()
	for _, enabled := range []bool{false, true} {
		c := testConfig(t)
		c.API, c.BLEEnabled = srv.URL, enabled
		p, e := NewPoller(c)
		if e != nil {
			t.Fatal(e)
		}
		p.client = srv.Client()
		_, devices, e := p.Fetch(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		var ids []string
		for _, d := range devices {
			ids = append(ids, d.ID)
		}
		want := testDevice
		if enabled {
			want += ",bf00000000000000ble1,bf00000000000000ble2"
			if devices[1].MAC != "dc:23:4d:00:00:01" {
				t.Fatalf("address not normalised: %q", devices[1].MAC)
			}
		}
		if strings.Join(ids, ",") != want {
			t.Fatalf("enabled=%t: %v", enabled, ids)
		}
		if wantCaps := map[bool]string{false: "", true: "ble"}[enabled]; caps != wantCaps {
			t.Fatalf("enabled=%t: capability header %q", enabled, caps)
		}
	}
}

func TestLoadConfigBLE(t *testing.T) {
	env := map[string]string{"AETHER_API": "https://aether.example", "GATEWAY_ID": testGateway, "HTTP_TOKEN": testToken,
		"MQTT_URL": "mqtts://aether.example:8883", "MQTT_PASSWORD": testPass, "MQTT_CA_FILE": "/config/ca.crt"}
	c, e := LoadConfig(func(k string) string { return env[k] })
	if e != nil || c.BLEEnabled || c.BLEAdapter != "hci0" || c.BLEMaxConnections != 1 {
		t.Fatalf("defaults: %+v %v", c, e)
	}
	env["BLE_ENABLED"], env["BLE_ADAPTER"], env["BLE_MAX_CONNECTIONS"] = "true", "hci1", "3"
	if c, e = LoadConfig(func(k string) string { return env[k] }); e != nil || !c.BLEEnabled || c.BLEAdapter != "hci1" || c.BLEMaxConnections != 3 {
		t.Fatalf("set: %+v %v", c, e)
	}
	for key, bad := range map[string]string{"BLE_ENABLED": "yes", "BLE_ADAPTER": "/dev/hci0", "BLE_MAX_CONNECTIONS": "9"} {
		broken := map[string]string{}
		for k, v := range env {
			broken[k] = v
		}
		broken[key] = bad
		if _, e := LoadConfig(func(k string) string { return broken[k] }); e == nil {
			t.Fatalf("accepted %s=%q", key, bad)
		}
	}
}

// --ble writes BLE_ENABLED and the D-Bus address into .env and mounts the host's /run/dbus read-only; the agent
// keeps every capability dropped.
func TestInstallBLE(t *testing.T) {
	ca, _ := testCertPEM(t)
	srv := bootstrapServer(t, 201, testBundle(ca, false), nil)
	dir := filepath.Join(t.TempDir(), "edge")
	if e := Install(context.Background(), InstallOptions{Server: srv.URL, Code: testCode, Dir: dir, BLE: true,
		Image: "ghcr.io/panudet-24mb/aether-edge:0.2.0", Client: srv.Client()}); e != nil {
		t.Fatal(e)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	for _, want := range []string{"BLE_ENABLED=true", "BLE_ADAPTER=hci0", "BLE_MAX_CONNECTIONS=1", "DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket"} {
		if !strings.Contains(string(env), want+"\n") {
			t.Fatalf(".env lacks %q:\n%s", want, env)
		}
	}
	compose, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if !strings.Contains(string(compose), "- /run/dbus:/run/dbus:ro") || !strings.Contains(string(compose), "cap_drop: [ALL]") ||
		strings.Contains(string(compose), "privileged") || strings.Contains(string(compose), "cap_add") {
		t.Fatalf("compose.yaml:\n%s", compose)
	}
	// Without --ble nothing of it.
	dir2 := filepath.Join(t.TempDir(), "edge")
	srv2 := bootstrapServer(t, 201, testBundle(ca, false), nil)
	if e := Install(context.Background(), InstallOptions{Server: srv2.URL, Code: testCode, Dir: dir2,
		Image: "ghcr.io/panudet-24mb/aether-edge:0.2.0", Client: srv2.Client()}); e != nil {
		t.Fatal(e)
	}
	env, _ = os.ReadFile(filepath.Join(dir2, ".env"))
	compose, _ = os.ReadFile(filepath.Join(dir2, "compose.yaml"))
	if strings.Contains(string(env), "BLE_") || strings.Contains(string(compose), "/run/dbus") {
		t.Fatal("BLE settings written without --ble")
	}
}

// A command carries its deadline: the agent takes the earlier of expires_at and receipt plus ttl_ms, drops one
// already past it, and refuses a malformed or absurd ttl.
func TestCommandDeadline(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	dps, d, e := parseCommandDeadline([]byte(`{"dps":{"1":true},"expires_at":"2026-09-30T10:00:30.000Z","ttl_ms":6666}`), now)
	if e != nil || dps["1"] != true || !d.Equal(now.Add(6666*time.Millisecond)) {
		t.Fatalf("ttl earlier: %v %v", d, e)
	}
	// A host clock running behind: expires_at is the earlier one.
	if _, d, _ = parseCommandDeadline([]byte(`{"dps":{"1":true},"expires_at":"2026-09-30T10:00:05.000Z","ttl_ms":30000}`), now); !d.Equal(now.Add(5 * time.Second)) {
		t.Fatalf("expires earlier: %v", d)
	}
	// A server before 0.2: no deadline.
	if _, d, e = parseCommandDeadline([]byte(`{"dps":{"1":true}}`), now); e != nil || !d.IsZero() {
		t.Fatalf("no deadline: %v %v", d, e)
	}
	for _, bad := range []string{`{"dps":{"1":true},"ttl_ms":0}`, `{"dps":{"1":true},"ttl_ms":-5}`, `{"dps":{"1":true},"ttl_ms":600000}`,
		`{"dps":{"1":true},"expires_at":"tomorrow"}`} {
		if _, _, e := parseCommandDeadline([]byte(bad), now); e == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// Over MQTT: a command past its deadline reaches neither a Wi-Fi nor a BLE device; one within it does.
func TestAgentDropsExpiredCommands(t *testing.T) {
	sim := &tuyablesim.Device{LocalKey: testKey, UUID: bleUUID, DeviceID: bleDevice1, ProductID: "gvygg3m8", Protocol: 3,
		DPs: []tuyable.DP{{ID: 1, Type: tuyable.DPBool, Value: false}}}
	radio := bletest.New()
	radio.Add(bleMAC, sim, -55)
	r := startBLE(t, radio)
	r.source.set(1, []Device{bleConfigDevice()}, nil)
	waitUntil(t, 2*time.Second, "device configured", func() bool { return r.agent.ble.Has(bleDevice1) })
	past := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	r.bus.deliver("aether/edge/"+testGateway+"/"+bleDevice1+"/set", []byte(`{"dps":{"1":true},"expires_at":"`+past+`","ttl_ms":30000}`))
	time.Sleep(300 * time.Millisecond)
	if len(sim.Writes()) != 0 || !strings.Contains(r.logs.String(), "after its deadline") {
		t.Fatalf("expired command written (%d writes)", len(sim.Writes()))
	}
	future := time.Now().Add(20 * time.Second).UTC().Format(time.RFC3339Nano)
	r.bus.deliver("aether/edge/"+testGateway+"/"+bleDevice1+"/set", []byte(`{"dps":{"1":true},"expires_at":"`+future+`","ttl_ms":20000}`))
	waitUntil(t, 3*time.Second, "command within its deadline", func() bool { return sim.State()[1].Value == true })

	// Wi-Fi: the same rule, before the device is touched.
	wifi, addr := simDevice(t, tuyalocal.V33, map[string]any{"1": false}, nil)
	w := start(t, fast)
	w.source.set(1, []Device{{ID: testDevice, Key: testKey, Version: "3.3", IP: addr}}, nil)
	w.bus.waitFor(t, 5*time.Second, "online", isAvailability(true, ""))
	w.bus.deliver("aether/edge/"+testGateway+"/"+testDevice+"/set", []byte(`{"dps":{"1":true},"expires_at":"`+past+`","ttl_ms":6666}`))
	time.Sleep(300 * time.Millisecond)
	if wifi.DPS()["1"] != false {
		t.Fatal("an expired command reached a Wi-Fi device")
	}
	w.bus.deliver("aether/edge/"+testGateway+"/"+testDevice+"/set", []byte(`{"dps":{"1":true},"expires_at":"`+future+`","ttl_ms":6666}`))
	w.bus.waitFor(t, 5*time.Second, "confirmation push", stateWith("1", true))
}

// The configuration's readonly flag reaches the BLE transport, and a Device never prints its keys.
func TestBLEDeviceConversion(t *testing.T) {
	d := bleConfigDevice()
	d.ReadOnly, d.SecKey = true, "sec-key-16-chars"
	b := bleDevice(d)
	if !b.ReadOnly || b.DPTypes[1] != tuyable.DPBool || b.DPTypes[2] != tuyable.DPValue || b.MAC != bleMAC {
		t.Fatalf("converted: %v", b)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("x", "device", d, "ble", b)
	for _, s := range []string{buf.String(), fmt.Sprintf("%v %+v %#v", d, d, d), fmt.Sprintf("%v %+v %#v", b, b, b)} {
		if strings.Contains(s, testKey) || strings.Contains(s, "sec-key-16-chars") {
			t.Fatalf("a key was printed: %s", s)
		}
	}
}
