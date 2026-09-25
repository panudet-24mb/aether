package tuyasim_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"aether/backend/internal/tuyalocal"
	"aether/backend/internal/tuyalocal/tuyasim"
)

const (
	devID = "eb1234567890abcdefgh"
	key   = "0123456789abcdef"
)

var versions = []tuyalocal.Version{tuyalocal.V31, tuyalocal.V33, tuyalocal.V34, tuyalocal.V35}

// recorder collects OnStatus reports.
type recorder struct {
	mu   sync.Mutex
	all  []tuyalocal.Status
	cond chan struct{}
}

func newRecorder() *recorder { return &recorder{cond: make(chan struct{}, 64)} }

func (r *recorder) on(s tuyalocal.Status) {
	r.mu.Lock()
	r.all = append(r.all, s)
	r.mu.Unlock()
	select {
	case r.cond <- struct{}{}:
	default:
	}
}

// waitFor returns the first report whose DPs satisfy ok, waiting up to 3 s.
func (r *recorder) waitFor(t *testing.T, ok func(tuyalocal.Status) bool) tuyalocal.Status {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		r.mu.Lock()
		for _, s := range r.all {
			if ok(s) {
				r.mu.Unlock()
				return s
			}
		}
		r.mu.Unlock()
		select {
		case <-r.cond:
		case <-deadline:
			t.Fatalf("no matching status among %+v", r.all)
		}
	}
}

func num(v any) string { return fmt.Sprint(v) }

func start(t *testing.T, d *tuyasim.Device) string {
	t.Helper()
	addr, e := d.Start()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(d.Close)
	return addr
}

func dial(t *testing.T, addr string, v tuyalocal.Version, rec *recorder, mod func(*tuyalocal.Options)) (*tuyalocal.Conn, error) {
	t.Helper()
	o := tuyalocal.Options{Address: addr, DeviceID: devID, LocalKey: key, Version: v, ReplyTimeout: 2 * time.Second, HeartbeatInterval: time.Second}
	if rec != nil {
		o.OnStatus = rec.on
	}
	if mod != nil {
		mod(&o)
	}
	c, e := tuyalocal.Dial(context.Background(), o)
	if e == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, e
}

func TestRoundTripEveryVersion(t *testing.T) {
	for _, v := range versions {
		t.Run(v.String(), func(t *testing.T) {
			d := tuyasim.New(devID, key, v, map[string]any{"1": false, "2": 10, "18": 250})
			addr := start(t, d)
			rec := newRecorder()
			c, e := dial(t, addr, v, rec, nil)
			if e != nil {
				t.Fatalf("dial: %v", e)
			}
			// The initial full status is delivered before Dial returns.
			full := rec.waitFor(t, func(s tuyalocal.Status) bool { return s.Full })
			if full.DPS["1"] != false || num(full.DPS["2"]) != "10" {
				t.Fatalf("initial status %+v", full.DPS)
			}
			// A command changes the device and comes back as a push.
			if e := c.Set(map[string]any{"1": true, "2": 42}); e != nil {
				t.Fatal(e)
			}
			rec.waitFor(t, func(s tuyalocal.Status) bool { return !s.Full && s.DPS["1"] == true && num(s.DPS["2"]) == "42" })
			if got := d.DPS(); got["1"] != true || num(got["2"]) != "42" {
				t.Fatalf("device state %+v", got)
			}
			// A press on the device is pushed without being asked.
			d.SetLocal(map[string]any{"1": false})
			rec.waitFor(t, func(s tuyalocal.Status) bool { return !s.Full && s.DPS["1"] == false })
			// UPDATEDPS makes the device push the requested DPs.
			if e := c.Refresh([]int{18}); e != nil {
				t.Fatal(e)
			}
			rec.waitFor(t, func(s tuyalocal.Status) bool { return num(s.DPS["18"]) == "250" })
			// A fresh query returns every DP again.
			if e := c.Query(); e != nil {
				t.Fatal(e)
			}
			n := 0
			rec.waitFor(t, func(s tuyalocal.Status) bool {
				if s.Full {
					n++
				}
				return n >= 2
			})
			if c.Err() != nil {
				t.Fatalf("connection ended: %v", c.Err())
			}
		})
	}
}

func TestWrongKey(t *testing.T) {
	for _, v := range []tuyalocal.Version{tuyalocal.V33, tuyalocal.V34, tuyalocal.V35} {
		t.Run(v.String(), func(t *testing.T) {
			d := tuyasim.New(devID, key, v, map[string]any{"1": true})
			d.RejectKey = true
			addr := start(t, d)
			_, e := dial(t, addr, v, nil, nil)
			want := tuyalocal.ErrKeyRejected
			if v == tuyalocal.V33 {
				want = tuyalocal.ErrKeySuspect // no negotiation on 3.3: only an undecryptable answer
			}
			if !errors.Is(e, want) {
				t.Fatalf("got %v, want %v", e, want)
			}
		})
	}
}

func TestSingleConnectionBusy(t *testing.T) {
	d := tuyasim.New(devID, key, tuyalocal.V33, map[string]any{"1": true})
	d.SingleConnection = true
	addr := start(t, d)
	first, e := dial(t, addr, tuyalocal.V33, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := dial(t, addr, tuyalocal.V33, nil, nil); !errors.Is(e, tuyalocal.ErrBusy) {
		t.Fatalf("second client: %v, want ErrBusy", e)
	}
	if first.Err() != nil {
		t.Fatalf("first connection disturbed: %v", first.Err())
	}
	// Once the first client leaves, the device can be reached again.
	first.Close()
	deadline := time.Now().Add(2 * time.Second)
	for d.Connections() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, e := dial(t, addr, tuyalocal.V33, nil, nil); e != nil {
		t.Fatalf("after the first client left: %v", e)
	}
}

func TestDevice22IsDetected(t *testing.T) {
	for _, v := range []tuyalocal.Version{tuyalocal.V33, tuyalocal.V34} {
		t.Run(v.String(), func(t *testing.T) {
			d := tuyasim.New(devID, key, v, map[string]any{"1": true, "2": 7})
			d.Device22 = true
			addr := start(t, d)
			rec := newRecorder()
			var opts func(*tuyalocal.Options)
			if v == tuyalocal.V34 {
				// On 3.4 the default query is DP_QUERY_NEW, which device22 devices answer; start in device22
				// mode as a device registered as such would.
				opts = func(o *tuyalocal.Options) { o.Device22 = true; o.DPsToRequest = []string{"1", "2"} }
			} else {
				opts = func(o *tuyalocal.Options) { o.DPsToRequest = []string{"1", "2"} }
			}
			c, e := dial(t, addr, v, rec, opts)
			if e != nil {
				t.Fatalf("dial: %v", e)
			}
			if !c.Device22() {
				t.Fatal("device22 mode not active")
			}
			s := rec.waitFor(t, func(s tuyalocal.Status) bool { return s.Full })
			if s.DPS["1"] != true || num(s.DPS["2"]) != "7" {
				t.Fatalf("device22 status %+v", s.DPS)
			}
		})
	}
}

func TestHeartbeatLost(t *testing.T) {
	d := tuyasim.New(devID, key, tuyalocal.V35, map[string]any{"1": true})
	d.Mute = true
	addr := start(t, d)
	c, e := dial(t, addr, tuyalocal.V35, nil, func(o *tuyalocal.Options) { o.HeartbeatInterval = 50 * time.Millisecond })
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-c.Done():
		if !errors.Is(c.Err(), tuyalocal.ErrHeartbeat) {
			t.Fatalf("ended with %v, want ErrHeartbeat", c.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("silent device not detected")
	}
}

func TestDeviceGoneAndContextCancel(t *testing.T) {
	d := tuyasim.New(devID, key, tuyalocal.V34, map[string]any{"1": true})
	addr := start(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	c, e := tuyalocal.Dial(ctx, tuyalocal.Options{Address: addr, DeviceID: devID, LocalKey: key, Version: tuyalocal.V34})
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not end the connection")
	}
	if e := c.Set(map[string]any{"1": false}); e == nil {
		t.Fatal("set on a closed connection succeeded")
	}
	// Nothing listening: a plain dial error, not a protocol one.
	d.Close()
	if _, e := tuyalocal.Dial(context.Background(), tuyalocal.Options{Address: addr, DeviceID: devID, LocalKey: key, Version: tuyalocal.V34, DialTimeout: time.Second}); e == nil {
		t.Fatal("dial to a closed port succeeded")
	}
}

func TestDiscoveryListen(t *testing.T) {
	// Reserve a free UDP port.
	pc, e := net.ListenPacket("udp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan tuyalocal.Broadcast, 8)
	done := make(chan error, 1)
	go func() {
		done <- tuyalocal.Listen(ctx, []string{addr}, func(b tuyalocal.Broadcast, _ net.Addr) {
			select { // never block the listener: it must be able to stop
			case got <- b:
			default:
			}
		})
	}()
	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < len(versions) {
		for _, v := range versions {
			d := tuyasim.New(fmt.Sprintf("eb%018d", int(v)), key, v, nil)
			if e := d.Broadcast(addr, "192.168.1.9"); e != nil {
				t.Fatal(e)
			}
		}
		select {
		case b := <-got:
			if b.IP != "192.168.1.9" || b.ProductKey == "" {
				t.Fatalf("broadcast %+v", b)
			}
			seen[b.Version] = true
		case <-deadline:
			t.Fatalf("only saw %v", seen)
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatalf("listen ended with %v", e)
	}
	// A junk packet on the same port is ignored, not fatal.
	if _, e := tuyalocal.DecodeBroadcast([]byte("hello")); !errors.Is(e, tuyalocal.ErrNotBroadcast) {
		t.Fatalf("junk: %v", e)
	}
	if b, e := tuyalocal.DecodeBroadcast(mustJSONBroadcast(t)); e != nil || b.Version != "3.3" {
		t.Fatalf("numeric version: %+v %v", b, e)
	}
}

// Some firmware sends "version": 3.3 as a number.
func mustJSONBroadcast(t *testing.T) []byte {
	t.Helper()
	body := map[string]any{"gwId": devID, "ip": "10.0.0.2", "version": 3.3}
	pkt, e := tuyalocal.EncodeBroadcast(tuyalocal.V33, body, nil)
	if e != nil {
		t.Fatal(e)
	}
	return pkt
}
