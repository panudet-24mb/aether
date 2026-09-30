package ble_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"aether/backend/internal/edge/ble"
	"aether/backend/internal/edge/ble/bletest"
	"aether/backend/internal/tuyable"
	"aether/backend/internal/tuyable/tuyablesim"
)

const (
	localKey = "0123456789abcdef"
	devUUID  = "tuya1234abcd5678"
	devID    = "bf00000000000000ble1"
	product  = "gvygg3m8"
	mac      = "dc:23:4d:00:00:01"
)

var fast = ble.Timing{
	Tick: 5 * time.Millisecond, Fresh: time.Second, NotFoundAfter: 300 * time.Millisecond, OfflineAfter: 0,
	Connect: time.Second, Handshake: 300 * time.Millisecond, Session: 2 * time.Second, Idle: 150 * time.Millisecond,
	CommandTTL: 2 * time.Second, BackoffMin: 40 * time.Millisecond, BackoffMax: 200 * time.Millisecond,
	SightingsEvery: 20 * time.Millisecond, SightingsForce: time.Second, ScanRetry: 30 * time.Millisecond,
	MinPoll: 100 * time.Millisecond, DefaultPoll: time.Hour,
}

// recorder is a Publisher that keeps everything.
type recorder struct {
	mu        sync.Mutex
	states    []map[string]any
	avail     []string
	sightings [][]ble.Sighting
}

func (r *recorder) State(device string, dps map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, dps)
}

func (r *recorder) Availability(device string, online bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if online {
		r.avail = append(r.avail, device+":online")
	} else {
		r.avail = append(r.avail, device+":offline:"+reason)
	}
}

func (r *recorder) Sightings(list []ble.Sighting) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sightings = append(r.sightings, list)
}

func (r *recorder) hasAvail(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.avail {
		if a == s {
			return true
		}
	}
	return false
}

func (r *recorder) hasState(match func(map[string]any) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.states {
		if match(s) {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func simDevice(protocol byte, configure func(*tuyablesim.Device)) *tuyablesim.Device {
	d := &tuyablesim.Device{LocalKey: localKey, UUID: devUUID, DeviceID: devID, ProductID: product, Protocol: protocol, SingleCentral: true,
		DPs: []tuyable.DP{
			{ID: 1, Type: tuyable.DPBool, Value: true},
			{ID: 2, Type: tuyable.DPValue, Value: int64(215)},
			{ID: 3, Type: tuyable.DPEnum, Value: int64(1)},
		}}
	if configure != nil {
		configure(d)
	}
	return d
}

func config(mode string) ble.Device {
	return ble.Device{ID: devID, MAC: mac, UUID: devUUID, LocalKey: localKey, ProductID: product, Mode: mode,
		DPTypes: map[byte]tuyable.DPType{1: tuyable.DPBool, 2: tuyable.DPValue, 3: tuyable.DPEnum}}
}

func run(t *testing.T, radio ble.Radio, slots int, timing ble.Timing, devices ...ble.Device) (*ble.Manager, *recorder) {
	t.Helper()
	rec := &recorder{}
	m := ble.NewManager(radio, rec, slots, timing, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Configure(devices)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	})
	return m, rec
}

// A device heard advertising is read once: it comes online and its data points are published (enum as its index),
// and the sightings list carries what the advertisement said.
func TestAdvertisementLeadsToRead(t *testing.T) {
	for _, protocol := range []byte{3, 4} {
		radio := bletest.New()
		radio.Add(mac, simDevice(protocol, nil), -61)
		_, rec := run(t, radio, 1, fast, config(ble.ModeAuto))
		eventually(t, 3*time.Second, "online", func() bool { return rec.hasAvail(devID + ":online") })
		eventually(t, 3*time.Second, "state", func() bool {
			return rec.hasState(func(s map[string]any) bool {
				return s["1"] == true && s["2"] == int64(215) && s["3"] == int64(1)
			})
		})
		eventually(t, 3*time.Second, "sightings", func() bool {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			for _, l := range rec.sightings {
				if len(l) == 1 && l[0].MAC == mac && l[0].UUID == devUUID && l[0].ProductID == product && l[0].Protocol == int(protocol) && *l[0].RSSI == -61 {
					return true
				}
			}
			return false
		})
	}
}

// A command is delivered on the next connection, even to a device in on-demand mode that is never read on its
// own, and the device's confirming report is published.
func TestCommandIsDeliveredAndConfirmed(t *testing.T) {
	sim := simDevice(3, nil)
	radio := bletest.New()
	radio.Add(mac, sim, -50)
	m, rec := run(t, radio, 1, fast, config(ble.ModeOnDemand))
	time.Sleep(100 * time.Millisecond)
	if radio.Connects(mac) != 0 {
		t.Fatal("an on-demand device was connected without a command")
	}
	if e := m.Command(devID, map[string]any{"1": false, "3": json.Number("0")}, time.Time{}); e != nil {
		t.Fatal(e)
	}
	eventually(t, 3*time.Second, "device state changed", func() bool {
		s := sim.State()
		return s[1].Value == false && s[3].Value == int64(0)
	})
	eventually(t, 3*time.Second, "confirmation published", func() bool {
		return rec.hasState(func(s map[string]any) bool { return s["1"] == false })
	})
	// Values that do not fit are refused before anything is queued.
	for _, bad := range []map[string]any{{"1": "on"}, {"9": true}, {"2": json.Number("1.5")}, {"3": json.Number("-1")}, {"0": true}} {
		if e := m.Command(devID, bad, time.Time{}); e == nil {
			t.Fatalf("command %v accepted", bad)
		}
	}
	if e := m.Command("bf0000000000000other", map[string]any{"1": true}, time.Time{}); e != ble.ErrNotConfigured {
		t.Fatalf("unknown device: %v", e)
	}
}

// A device that refuses the keys is announced at once; one that keeps silent through the handshake while it
// advertises is declared the same after three tries.
func TestRejectedKeys(t *testing.T) {
	radio := bletest.New()
	radio.Add(mac, simDevice(3, func(d *tuyablesim.Device) { d.RejectKey = true }), -50)
	_, rec := run(t, radio, 1, fast, config(ble.ModeAuto))
	eventually(t, 3*time.Second, "auth_failed", func() bool { return rec.hasAvail(devID + ":offline:auth_failed") })

	radio = bletest.New()
	radio.Add(mac, simDevice(3, func(d *tuyablesim.Device) { d.Mute = true }), -50)
	_, rec = run(t, radio, 1, fast, config(ble.ModeAuto))
	eventually(t, 5*time.Second, "auth_failed after silent handshakes", func() bool { return rec.hasAvail(devID + ":offline:auth_failed") })
	if radio.Connects(mac) < 3 {
		t.Fatalf("declared after %d tries", radio.Connects(mac))
	}
}

// Another central holding the device is "busy"; a device never heard is "not_found".
func TestBusyAndNotFound(t *testing.T) {
	radio := bletest.New()
	radio.Add(mac, simDevice(3, nil), -50)
	radio.RefuseConnect(mac, tuyable.ErrBusy)
	_, rec := run(t, radio, 1, fast, config(ble.ModeAuto))
	eventually(t, 3*time.Second, "busy", func() bool { return rec.hasAvail(devID + ":offline:busy") })

	_, rec = run(t, bletest.New(), 1, fast, config(ble.ModeAuto))
	eventually(t, 3*time.Second, "not_found", func() bool { return rec.hasAvail(devID + ":offline:not_found") })
}

// Failures back off: attempts are spaced by a growing delay, not one per scheduler tick.
func TestBackoff(t *testing.T) {
	radio := bletest.New()
	radio.Add(mac, simDevice(3, nil), -50)
	radio.RefuseConnect(mac, context.DeadlineExceeded)
	run(t, radio, 1, fast, config(ble.ModeAuto))
	time.Sleep(700 * time.Millisecond)
	// 40, 80, 160, then 200 ms apart: at most about six attempts in 700 ms (a tick-rate loop would make ~140).
	if n := radio.Connects(mac); n < 2 || n > 7 {
		t.Fatalf("%d attempts in 700 ms", n)
	}
}

// The connection budget holds: with one slot, three devices are served one at a time, and all of them are read.
func TestConnectionBudget(t *testing.T) {
	radio := bletest.New()
	var devices []ble.Device
	for i, id := range []string{"bf00000000000000ble1", "bf00000000000000ble2", "bf00000000000000ble3"} {
		addr := []string{"dc:23:4d:00:00:01", "dc:23:4d:00:00:02", "dc:23:4d:00:00:03"}[i]
		uuid := []string{"uuid0000000000a1", "uuid0000000000a2", "uuid0000000000a3"}[i]
		radio.Add(addr, simDevice(3, func(d *tuyablesim.Device) { d.DeviceID, d.UUID = id, uuid }), -50)
		c := config(ble.ModeAuto)
		c.ID, c.MAC, c.UUID = id, addr, uuid
		devices = append(devices, c)
	}
	_, rec := run(t, radio, 1, fast, devices...)
	for _, d := range devices {
		id := d.ID
		eventually(t, 5*time.Second, id+" online", func() bool { return rec.hasAvail(id + ":online") })
	}
	if n := radio.MaxOpen(); n != 1 {
		t.Fatalf("%d connections open at once with one slot", n)
	}
}

// A device configured by uuid only is found through its advertisement; a device whose factory record lists the
// address byte-reversed is found too.
func TestFindByUUIDAndReversedAddress(t *testing.T) {
	radio := bletest.New()
	radio.Add(mac, simDevice(3, nil), -50)
	c := config(ble.ModeAuto)
	c.MAC = ""
	_, rec := run(t, radio, 1, fast, c)
	eventually(t, 3*time.Second, "online by uuid", func() bool { return rec.hasAvail(devID + ":online") })

	radio = bletest.New()
	radio.Add(mac, simDevice(3, nil), -50)
	c = config(ble.ModeAuto)
	c.MAC, c.UUID = "01:00:00:4d:23:dc", ""
	_, rec = run(t, radio, 1, fast, c)
	eventually(t, 3*time.Second, "online by reversed address", func() bool { return rec.hasAvail(devID + ":online") })
}

// A radio that cannot be used shows in the health, and the scan is retried.
func TestRadioStateInHealth(t *testing.T) {
	radio := bletest.New()
	radio.FailScan(ble.ErrNoPermission)
	m, _ := run(t, radio, 1, fast)
	eventually(t, 2*time.Second, "no_permission", func() bool { return m.Health().State == ble.StateNoPermission })
	radio.FailScan(nil)
	eventually(t, 2*time.Second, "ok again", func() bool { return m.Health().State == ble.StateOK })
	if h := m.Health(); h.Adapter != "hci-test" {
		t.Fatalf("health %+v", h)
	}
}

// Malformed keys are announced once and never tried.
func TestMalformedKeysAreNotTried(t *testing.T) {
	radio := bletest.New()
	radio.Add(mac, simDevice(3, nil), -50)
	c := config(ble.ModeAuto)
	c.SecKey = "short"
	_, rec := run(t, radio, 1, fast, c)
	eventually(t, 2*time.Second, "auth_failed", func() bool { return rec.hasAvail(devID + ":offline:auth_failed") })
	time.Sleep(100 * time.Millisecond)
	if radio.Connects(mac) != 0 {
		t.Fatal("a device with malformed keys was connected")
	}
}

// A persistent device keeps its connection: a report the device pushes later arrives without a new connection,
// and a command goes over the same one.
func TestPersistentMode(t *testing.T) {
	sim := simDevice(3, nil)
	radio := bletest.New()
	radio.Add(mac, sim, -50)
	m, rec := run(t, radio, 1, fast, config(ble.ModePersistent))
	eventually(t, 3*time.Second, "online", func() bool { return rec.hasAvail(devID + ":online") })
	time.Sleep(3 * fast.Idle) // well past the idle time that ends other sessions
	if e := sim.Set(tuyable.DP{ID: 2, Type: tuyable.DPValue, Value: int64(230)}); e != nil {
		t.Fatalf("the held connection was closed: %v", e)
	}
	eventually(t, 3*time.Second, "pushed report", func() bool {
		return rec.hasState(func(s map[string]any) bool { return s["2"] == int64(230) })
	})
	if e := m.Command(devID, map[string]any{"1": false}, time.Time{}); e != nil {
		t.Fatal(e)
	}
	eventually(t, 3*time.Second, "command", func() bool { return sim.State()[1].Value == false })
	if n := radio.Connects(mac); n != 1 {
		t.Fatalf("%d connections for a persistent device", n)
	}
}

// A read-only device (a lock) takes no command, even with its data-point types known.
func TestReadOnlyRefusesCommands(t *testing.T) {
	sim := simDevice(3, nil)
	radio := bletest.New()
	radio.Add(mac, sim, -50)
	c := config(ble.ModeAuto)
	c.ReadOnly = true
	m, rec := run(t, radio, 1, fast, c)
	if e := m.Command(devID, map[string]any{"1": false}, time.Time{}); e != ble.ErrReadOnly {
		t.Fatalf("command to a read-only device: %v", e)
	}
	eventually(t, 3*time.Second, "read anyway", func() bool { return rec.hasAvail(devID + ":online") })
	time.Sleep(100 * time.Millisecond)
	if len(sim.Writes()) != 0 || sim.State()[1].Value != true {
		t.Fatal("a read-only device was written")
	}
}

// A command is never written after its deadline: one that expires while the device is out of range is dropped,
// and one past its deadline is refused on arrival.
func TestCommandDeadline(t *testing.T) {
	sim := simDevice(3, nil)
	radio := bletest.New()
	radio.Add(mac, sim, -50)
	radio.Silence(mac, true)
	m, _ := run(t, radio, 1, fast, config(ble.ModeOnDemand))
	if e := m.Command(devID, map[string]any{"1": false}, time.Now().Add(-time.Millisecond)); e == nil {
		t.Fatal("a command past its deadline was queued")
	}
	if e := m.Command(devID, map[string]any{"1": false}, time.Now().Add(150*time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	time.Sleep(300 * time.Millisecond)
	radio.Silence(mac, false) // back in range after the deadline
	time.Sleep(500 * time.Millisecond)
	if len(sim.Writes()) != 0 || sim.State()[1].Value != true {
		t.Fatal("a command was written after its deadline")
	}
	if h := m.Health(); h.Queue != 0 {
		t.Fatalf("expired command still queued: %+v", h)
	}
}

// With a configured address, a device is found by it only: another device advertising the same uuid (a spoof) is
// never connected. Only a device configured without an address follows its uuid.
func TestConfiguredAddressIsNotRedirected(t *testing.T) {
	radio := bletest.New()
	spoof := simDevice(3, nil) // same uuid, keys the attacker does not have
	spoof.LocalKey = "ffffffffffffffff"
	radio.Add("aa:aa:aa:aa:aa:aa", spoof, -20)
	_, rec := run(t, radio, 1, fast, config(ble.ModeAuto))
	time.Sleep(200 * time.Millisecond)
	if radio.Connects("aa:aa:aa:aa:aa:aa") != 0 || rec.hasAvail(devID+":offline:auth_failed") {
		t.Fatal("a device advertising the configured device's uuid from another address was connected")
	}
}

// When the heard list is full, configured devices are never the ones forgotten, and they lead the sightings.
func TestKnownDevicesSurviveAFlood(t *testing.T) {
	flood := &floodRadio{real: bletest.New()}
	flood.real.Add(mac, simDevice(3, nil), -90) // weak, heard first
	_, rec := run(t, flood, 1, fast, config(ble.ModeAuto))
	eventually(t, 3*time.Second, "online despite the flood", func() bool { return rec.hasAvail(devID + ":online") })
	eventually(t, 3*time.Second, "configured device first in the sightings", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		for _, l := range rec.sightings {
			if len(l) == ble.MaxSightings && l[0].MAC == mac {
				return true
			}
		}
		return false
	})
}

// floodRadio adds a thousand strong strangers (valid Tuya adverts) to every scan of the real radio.
type floodRadio struct{ real *bletest.Radio }

func (f *floodRadio) Adapter() string { return f.real.Adapter() }

func (f *floodRadio) Scan(ctx context.Context, on func(ble.Advertisement)) error {
	other := simDevice(3, func(d *tuyablesim.Device) { d.UUID = "stranger00000000" })
	sd, md := other.Advert()
	return f.real.Scan(ctx, func(a ble.Advertisement) {
		on(a)
		for i := 0; i < 1000; i++ {
			on(ble.Advertisement{MAC: fmt.Sprintf("02:00:00:00:%02x:%02x", i/256, i%256), RSSI: -30, ServiceData: sd, ManufacturerData: md})
		}
	})
}

func (f *floodRadio) Connect(ctx context.Context, m string, fd50 bool) (tuyable.Link, error) {
	return f.real.Connect(ctx, m, fd50)
}

// A held (persistent) connection gives its only slot up to a command for another device, and comes back after.
func TestCommandPreemptsPersistent(t *testing.T) {
	radio := bletest.New()
	held := simDevice(3, nil)
	other := simDevice(3, func(d *tuyablesim.Device) { d.DeviceID, d.UUID = "bf00000000000000ble2", "uuid0000000000b2" })
	radio.Add(mac, held, -50)
	radio.Add("dc:23:4d:00:00:02", other, -50)
	c2 := config(ble.ModeOnDemand)
	c2.ID, c2.MAC, c2.UUID = "bf00000000000000ble2", "dc:23:4d:00:00:02", "uuid0000000000b2"
	m, rec := run(t, radio, 1, fast, config(ble.ModePersistent), c2)
	eventually(t, 3*time.Second, "persistent online", func() bool { return rec.hasAvail(devID + ":online") })
	if e := m.Command(c2.ID, map[string]any{"1": false}, time.Time{}); e != nil {
		t.Fatal(e)
	}
	eventually(t, 3*time.Second, "command delivered", func() bool { return other.State()[1].Value == false })
	eventually(t, 3*time.Second, "persistent connection taken up again", func() bool { return radio.Connects(mac) >= 2 })
	if radio.MaxOpen() != 1 {
		t.Fatalf("%d connections at once with one slot", radio.MaxOpen())
	}
}
