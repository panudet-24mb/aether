package tuyable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/backend/internal/tuyable"
	"aether/backend/internal/tuyable/tuyablesim"
)

const (
	localKey = "0123456789abcdef"
	secKey   = "fedcba9876543210"
	uuid     = "tuya5d8f2a3c9e1b"
	deviceID = "bf1234567890abcdef"
)

var initialDPs = []tuyable.DP{
	{ID: 1, Type: tuyable.DPBool, Value: false},
	{ID: 2, Type: tuyable.DPValue, Value: int64(235)},
	{ID: 3, Type: tuyable.DPEnum, Value: int64(1)},
	{ID: 4, Type: tuyable.DPString, Value: "hello"},
	{ID: 5, Type: tuyable.DPRaw, Value: []byte{0, 0xFF}},
}

func device(proto byte) *tuyablesim.Device {
	return &tuyablesim.Device{LocalKey: localKey, UUID: uuid, DeviceID: deviceID, ProductID: "gvygg3m8", Protocol: proto,
		DPs: initialDPs, SingleCentral: true}
}

func config() tuyable.Config {
	return tuyable.Config{Keys: tuyable.Keys{LocalKey: localKey}, UUID: uuid, DeviceID: deviceID, ProductID: "gvygg3m8",
		ResponseTimeout: 2 * time.Second}
}

// leakCheck fails the test if goroutines started during it are still running shortly after it ends.
func leakCheck(t *testing.T) {
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				t.Fatalf("goroutines: %d before, %d after\n%s", before, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func open(t *testing.T, d *tuyablesim.Device, cfg tuyable.Config) (*tuyable.Session, error) {
	t.Helper()
	link, err := d.Connect()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tuyable.Open(ctx, link, cfg)
}

func mustOpen(t *testing.T, d *tuyablesim.Device, cfg tuyable.Config) *tuyable.Session {
	t.Helper()
	s, err := open(t, d, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// collect reads reports until it has seen every DP id in want or the timeout passes.
func collect(t *testing.T, s *tuyable.Session, ids ...byte) map[byte]tuyable.DP {
	t.Helper()
	got := map[byte]tuyable.DP{}
	timeout := time.After(3 * time.Second)
	for {
		missing := false
		for _, id := range ids {
			if _, ok := got[id]; !ok {
				missing = true
			}
		}
		if !missing {
			return got
		}
		select {
		case r, ok := <-s.Reports():
			if !ok {
				t.Fatalf("reports closed: %v", s.Err())
			}
			for _, dp := range r.DPs {
				got[dp.ID] = dp
			}
		case <-timeout:
			t.Fatalf("reports: got %v, want ids %v", got, ids)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionStatusAndWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		proto  byte
		sec    string
		ack    tuyable.Code
		prodID string
	}{
		{name: "protocol 3", proto: 3, ack: tuyable.CodeReceiveDP},
		{name: "protocol 4", proto: 4, ack: tuyable.CodeReceiveDPV4},
		{name: "sec_key", proto: 4, sec: secKey, ack: tuyable.CodeReceiveDPV4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leakCheck(t)
			d := device(tc.proto)
			d.SecKey = tc.sec
			cfg := config()
			cfg.Keys.SecKey = tc.sec
			s := mustOpen(t, d, cfg)
			if info := s.Info(); info.Protocol != tc.proto || info.DeviceVersion != "1.0" {
				t.Fatalf("info = %+v", info)
			}
			ctx := context.Background()
			if err := s.Status(ctx); err != nil {
				t.Fatal(err)
			}
			got := collect(t, s, 1, 2, 3, 4, 5)
			for _, want := range initialDPs {
				if !reflect.DeepEqual(got[want.ID], want) {
					t.Errorf("DP %d = %+v, want %+v", want.ID, got[want.ID], want)
				}
			}
			set := []tuyable.DP{{ID: 1, Type: tuyable.DPBool, Value: true}, {ID: 3, Type: tuyable.DPEnum, Value: int64(2)}}
			if err := s.SetDPs(ctx, set); err != nil {
				t.Fatal(err)
			}
			got = collect(t, s, 1, 3)
			if got[1].Value != true || got[3].Value != int64(2) {
				t.Fatalf("confirmed %v", got)
			}
			if st := d.State(); st[1].Value != true || st[3].Value != int64(2) {
				t.Fatalf("device state %v", st)
			}
			// A change made on the device side arrives on its own.
			if err := d.Set(tuyable.DP{ID: 2, Type: tuyable.DPValue, Value: int64(-40)}); err != nil {
				t.Fatal(err)
			}
			if got := collect(t, s, 2); got[2].Value != int64(-40) {
				t.Fatalf("pushed %v", got)
			}
			// Every report was acknowledged: status, write confirmation, device push.
			waitFor(t, "acks", func() bool { return len(d.Acks()) >= 3 })
			for _, a := range d.Acks() {
				if a.Code != tc.ack || a.ResponseTo == 0 {
					t.Fatalf("ack %+v", a)
				}
			}
			if w := d.Writes(); len(w) != 1 || (tc.proto == 3) != (w[0].Code == tuyable.CodeDPs) {
				t.Fatalf("writes %+v", w)
			}
		})
	}
}

func TestSessionProtocol2IsReadOnly(t *testing.T) {
	leakCheck(t)
	s := mustOpen(t, device(2), config())
	if err := s.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	collect(t, s, 1)
	err := s.SetDPs(context.Background(), []tuyable.DP{{ID: 1, Type: tuyable.DPBool, Value: true}})
	if !errors.Is(err, tuyable.ErrUnsupportedProtocol) {
		t.Fatalf("SetDPs on protocol 2: %v", err)
	}
}

func TestSessionLegacyKeyProductIgnoresSecKey(t *testing.T) {
	leakCheck(t)
	d := device(4)
	d.ProductID = "mknd4lci"
	cfg := config()
	cfg.ProductID = "mknd4lci"
	cfg.Keys.SecKey = secKey
	s := mustOpen(t, d, cfg)
	if err := s.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionFD50DeviceInfoQuirk(t *testing.T) {
	leakCheck(t)
	d := device(4)
	d.ProductID = "jntxv3q4"
	cfg := config()
	cfg.ProductID, cfg.FD50 = "jntxv3q4", true
	mustOpen(t, d, cfg)
}

func TestSessionAlreadyBound(t *testing.T) {
	leakCheck(t)
	d := device(3)
	d.AlreadyBound = true
	s := mustOpen(t, d, config())
	if !s.Info().Bound {
		t.Fatal("bound flag not read")
	}
}

func TestSessionFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*tuyablesim.Device, *tuyable.Config)
		want  error
		// maxTime bounds how long the failure may take.
		maxTime time.Duration
	}{
		{name: "wrong key", setup: func(d *tuyablesim.Device, _ *tuyable.Config) { d.RejectKey = true },
			want: tuyable.ErrKeyRejected, maxTime: time.Second},
		// A device that cannot decrypt the device-info request may just stay silent: the wrong key then shows up
		// as a timeout on DEVICE_INFO, which Edge tells apart from "not found" because the device advertises.
		{name: "wrong key, device silent", setup: func(_ *tuyablesim.Device, c *tuyable.Config) { c.Keys.LocalKey = "0123XX6789abcdef" },
			want: tuyable.ErrTimeout, maxTime: time.Second},
		{name: "pairing refused", setup: func(d *tuyablesim.Device, _ *tuyable.Config) { d.PairResult = 1 },
			want: tuyable.ErrKeyRejected, maxTime: time.Second},
		{name: "wrong uuid", setup: func(_ *tuyablesim.Device, c *tuyable.Config) { c.UUID = "tuya000000000000" },
			want: tuyable.ErrKeyRejected, maxTime: time.Second},
		{name: "mute", setup: func(d *tuyablesim.Device, _ *tuyable.Config) { d.Mute = true },
			want: tuyable.ErrTimeout, maxTime: time.Second},
		{name: "unsupported protocol", setup: func(d *tuyablesim.Device, _ *tuyable.Config) { d.Protocol = 5 },
			want: tuyable.ErrUnsupportedProtocol, maxTime: time.Second},
		{name: "bad sec_key", setup: func(_ *tuyablesim.Device, c *tuyable.Config) { c.Keys.SecKey = "short" },
			want: tuyable.ErrFormat, maxTime: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leakCheck(t)
			d := device(3)
			cfg := config()
			cfg.ResponseTimeout = 300 * time.Millisecond
			tc.setup(d, &cfg)
			start := time.Now()
			s, err := open(t, d, cfg)
			if !errors.Is(err, tc.want) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open: %v, want %v", err, tc.want)
			}
			if took := time.Since(start); took > tc.maxTime {
				t.Fatalf("failure took %v", took)
			}
			// The link was closed: the device accepts a new central.
			l, err := d.Connect()
			if err != nil {
				t.Fatalf("device still held: %v", err)
			}
			l.Close()
		})
	}
}

func TestSessionContextCancel(t *testing.T) {
	leakCheck(t)
	d := device(3)
	d.Mute = true
	link, _ := d.Connect()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cfg := config()
	cfg.ResponseTimeout = time.Minute
	start := time.Now()
	_, err := tuyable.Open(ctx, link, cfg)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("Open with a 100 ms context: %v after %v", err, time.Since(start))
	}
}

func TestSessionSingleCentral(t *testing.T) {
	leakCheck(t)
	d := device(3)
	s := mustOpen(t, d, config())
	if _, err := d.Connect(); !errors.Is(err, tuyable.ErrBusy) {
		t.Fatalf("second central: %v", err)
	}
	s.Close()
	s.Close() // idempotent
	s2 := mustOpen(t, d, config())
	if err := s2.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLinkDrop(t *testing.T) {
	leakCheck(t)
	d := device(3)
	s := mustOpen(t, d, config())
	d.Drop()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session did not notice the drop")
	}
	if !errors.Is(s.Err(), tuyable.ErrClosed) {
		t.Fatalf("Err = %v", s.Err())
	}
	if err := s.Status(context.Background()); !errors.Is(err, tuyable.ErrClosed) {
		t.Fatalf("Status after drop: %v", err)
	}
	if _, ok := <-s.Reports(); ok {
		t.Fatal("reports still open")
	}
}

func TestSessionClockRequests(t *testing.T) {
	leakCheck(t)
	d := device(3)
	d.AskTime = true
	cfg := config()
	now := time.Date(2026, 1, 1, 7, 0, 0, 0, time.FixedZone("ICT", 7*3600))
	cfg.Now = func() time.Time { return now }
	mustOpen(t, d, cfg)
	for code, want := range map[tuyable.Code][]byte{
		tuyable.CodeTime1Request: tuyable.Time1Answer(now),
		tuyable.CodeTime2Request: tuyable.Time2Answer(now),
	} {
		waitFor(t, code.String(), func() bool { _, ok := d.TimeAnswer(code); return ok })
		if got, _ := d.TimeAnswer(code); !reflect.DeepEqual(got, want) {
			t.Errorf("%s answer = %x, want %x", code, got, want)
		}
	}
}

func TestSessionReportKinds(t *testing.T) {
	for _, tc := range []struct {
		proto byte
		code  tuyable.Code
		timed bool
	}{
		{3, tuyable.CodeReceiveTimeDP, true},
		{3, tuyable.CodeReceiveSignDP, false},
		{3, tuyable.CodeReceiveSignTimeDP, true},
		{4, tuyable.CodeReceiveTimeDPV4, true},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			leakCheck(t)
			d := device(tc.proto)
			s := mustOpen(t, d, config())
			if err := d.Push(tc.code, tuyable.DP{ID: 9, Type: tuyable.DPValue, Value: int64(215)}); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-s.Reports():
				if r.Code != tc.code || len(r.DPs) != 1 || r.DPs[0].Value != int64(215) || r.Timestamped != tc.timed {
					t.Fatalf("report %+v", r)
				}
				if tc.timed && !r.Time.Equal(time.Unix(1767225600, 0)) {
					t.Fatalf("time %v", r.Time)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no report")
			}
			waitFor(t, "ack", func() bool { return len(d.Acks()) == 1 })
			if a := d.Acks()[0]; a.Code != tc.code {
				t.Fatalf("ack %+v", a)
			}
		})
	}
}

func TestSessionSmallNotificationsAndLostFragment(t *testing.T) {
	leakCheck(t)
	d := device(3)
	d.NotifyMTU = tuyable.MinMTU
	cfg := config()
	cfg.ResponseTimeout = 300 * time.Millisecond
	s := mustOpen(t, d, cfg)
	ctx := context.Background()
	if err := s.Status(ctx); err != nil {
		t.Fatal(err)
	}
	collect(t, s, 1, 2, 3, 4, 5)
	// Losing one fragment loses that message only: the request times out and the next one works.
	d.SetDropFragment(2)
	if err := s.Status(ctx); !errors.Is(err, tuyable.ErrTimeout) {
		t.Fatalf("Status with a lost fragment: %v", err)
	}
	if err := s.Status(ctx); err != nil {
		t.Fatalf("Status after the loss: %v", err)
	}
	if s.Stats().Errors == 0 {
		t.Fatal("the broken message was not counted")
	}
}

func TestSessionReportOverflow(t *testing.T) {
	leakCheck(t)
	d := device(3)
	cfg := config()
	cfg.ReportBuffer = 2
	s := mustOpen(t, d, cfg)
	// Nobody reads Reports while the device pushes ten changes to DP 2 and one to DP 6.
	for i := range 10 {
		if err := d.Set(tuyable.DP{ID: 2, Type: tuyable.DPValue, Value: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Set(tuyable.DP{ID: 6, Type: tuyable.DPBool, Value: true}); err != nil {
		t.Fatal(err)
	}
	// Every report is still acknowledged, so the device does not resend them.
	waitFor(t, "acks", func() bool { return len(d.Acks()) == 11 })
	waitFor(t, "coalescing", func() bool { return s.Stats().Coalesced > 0 })
	// The reader then sees the latest value of every DP, with the merge flagged.
	latest := map[byte]any{}
	coalesced := false
	for len(latest) < 2 || latest[2] != int64(9) {
		select {
		case r := <-s.Reports():
			coalesced = coalesced || r.Coalesced
			for _, dp := range r.DPs {
				latest[dp.ID] = dp.Value
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("latest values not delivered: %v", latest)
		}
	}
	if !coalesced || latest[6] != true {
		t.Fatalf("coalesced %v, latest %v", coalesced, latest)
	}
}

func TestAdvertFromSimulator(t *testing.T) {
	sd, md := device(4).Advert()
	a, err := tuyable.ParseAdvert(sd, md)
	if err != nil || a.UUID != uuid || a.ProductID != "gvygg3m8" || a.Protocol != 4 || !a.Bound {
		t.Fatalf("advert %+v, %v", a, err)
	}
}

// Regression: a device-initiated message with an unknown code right after the pair answer used to poison the
// session (reported by review: 195 of 200 sessions failed their next request with ErrKeyRejected).
func TestSessionUnknownCodeAfterPair(t *testing.T) {
	leakCheck(t)
	for i := range 200 {
		d := device(3)
		d.AfterPair = []tuyable.Frame{{Code: 0x8099, Data: []byte{1}}, {Code: tuyable.CodeReceiveDP, Data: []byte{9, 9, 9}}}
		s, err := open(t, d, config())
		if err != nil {
			t.Fatalf("run %d: Open: %v", i, err)
		}
		if err := s.Status(context.Background()); err != nil {
			t.Fatalf("run %d: Status after an unknown code: %v", i, err)
		}
		select {
		case <-s.Done():
			t.Fatalf("run %d: session ended: %v", i, s.Err())
		default:
		}
		waitFor(t, "counters", func() bool { st := s.Stats(); return st.Unknown == 1 && st.Errors >= 1 })
		s.Close()
	}
}

func TestSessionGlitchDuringHandshake(t *testing.T) {
	leakCheck(t)
	d := device(3)
	d.Glitch = true
	s := mustOpen(t, d, config())
	if err := s.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Stats().Errors == 0 {
		t.Fatal("the stray packet was not counted")
	}
}

// recordLink wraps a link: it remembers the protocol nibble of every first fragment and can fail writes.
type recordLink struct {
	tuyable.Link
	mu        sync.Mutex
	nibbles   []byte
	failAfter int
	writes    int
}

func (r *recordLink) Write(ctx context.Context, p []byte) error {
	r.mu.Lock()
	r.writes++
	fail := r.failAfter > 0 && r.writes > r.failAfter
	if p[0] == 0 {
		_, pos, _ := tuyable.Varint(p, 1)
		r.nibbles = append(r.nibbles, p[pos]>>4)
	}
	r.mu.Unlock()
	if fail {
		return errors.New("adapter gone")
	}
	return r.Link.Write(ctx, p)
}

func TestSessionUsesAdvertisedProtocol(t *testing.T) {
	for _, tc := range []struct {
		advertised byte
		fd50       bool
		product    string
		wantFirst  byte
	}{
		{advertised: 3, wantFirst: 3},
		{advertised: 4, wantFirst: 4},
		{advertised: 0, wantFirst: 2},
		{advertised: 4, fd50: true, product: "jntxv3q4", wantFirst: 2},
	} {
		leakCheck(t)
		d := device(4)
		d.ProductID = cmpOr(tc.product, d.ProductID)
		l, _ := d.Connect()
		rl := &recordLink{Link: l}
		cfg := config()
		cfg.AdvertisedProtocol, cfg.FD50, cfg.ProductID = tc.advertised, tc.fd50, d.ProductID
		s, err := tuyable.Open(context.Background(), rl, cfg)
		if err != nil {
			t.Fatal(err)
		}
		rl.mu.Lock()
		got := slices.Clone(rl.nibbles)
		rl.mu.Unlock()
		// DEVICE_INFO goes out with the advertised protocol (2 for the FD50 quirk), PAIR with the device's own.
		if len(got) != 2 || got[0] != tc.wantFirst || got[1] != 4 {
			t.Errorf("advertised %d fd50 %t: first-fragment protocols %v", tc.advertised, tc.fd50, got)
		}
		s.Close()
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestSessionWriteFailureEndsSession(t *testing.T) {
	leakCheck(t)
	d := device(3)
	l, _ := d.Connect()
	rl := &recordLink{Link: l}
	s, err := tuyable.Open(context.Background(), rl, config())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rl.mu.Lock()
	rl.failAfter = rl.writes
	rl.mu.Unlock()
	err = s.Status(context.Background())
	if !errors.Is(err, tuyable.ErrClosed) || !strings.Contains(err.Error(), "adapter gone") {
		t.Fatalf("Status with a failing write: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("session still up after a write failure")
	}
	if !errors.Is(s.Err(), tuyable.ErrClosed) {
		t.Fatalf("Err = %v", s.Err())
	}
}

func TestSessionTimeoutsAndCancellation(t *testing.T) {
	leakCheck(t)
	d := device(3)
	cfg := config()
	cfg.ResponseTimeout = 200 * time.Millisecond
	s := mustOpen(t, d, cfg)
	d.SetMute(true)
	// Response timeout: ErrTimeout wrapping the deadline.
	err := s.Status(context.Background())
	if !errors.Is(err, tuyable.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("response timeout: %v", err)
	}
	// The caller's cancellation is returned as is.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := s.Status(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, tuyable.ErrTimeout) {
		t.Fatalf("cancelled: %v", err)
	}
	// The caller's own deadline is not a device timeout either.
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Status(ctx); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, tuyable.ErrTimeout) {
		t.Fatalf("caller deadline: %v", err)
	}
}

func TestSessionAnswerMustMatchRequestCode(t *testing.T) {
	leakCheck(t)
	d := device(3)
	cfg := config()
	cfg.ResponseTimeout = 300 * time.Millisecond
	s := mustOpen(t, d, cfg)
	// An answer to seq 3 (the next request) with the wrong code is not taken as its answer.
	if err := d.Inject(tuyable.Frame{Code: tuyable.CodeDPs, ResponseTo: 3, Data: []byte{0}}, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "count", func() bool { return s.Stats().Errors == 1 })
	if err := s.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKeysAndConfigAreRedacted(t *testing.T) {
	cfg := config()
	cfg.Keys.SecKey = secKey
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "cfg", cfg, "keys", cfg.Keys)
	for _, out := range []string{fmt.Sprint(cfg), fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg), fmt.Sprintf("%#v", cfg.Keys), buf.String()} {
		if strings.Contains(out, localKey) || strings.Contains(out, secKey) || strings.Contains(out, localKey[:6]) {
			t.Fatalf("key leaked: %s", out)
		}
	}
}

// FuzzSession feeds arbitrary frames, sealed with the real session key so they pass the CRC, into an open session
// at an arbitrary notification size. The session must not panic, must keep answering requests, and must close
// cleanly.
func FuzzSession(f *testing.F) {
	body, _ := tuyable.EncodeDPs([]tuyable.DP{{ID: 1, Type: tuyable.DPBool, Value: true}}, 1)
	f.Add(uint16(tuyable.CodeReceiveDP), uint32(0), body, uint8(20), byte(3))
	f.Add(uint16(tuyable.CodeReceiveDPV4), uint32(0), []byte{0, 0, 0, 0, 1, 0, 0, 1, 1, 0, 1, 1}, uint8(9), byte(4))
	f.Add(uint16(tuyable.CodeDeviceStatus), uint32(3), []byte{0}, uint8(20), byte(3))
	f.Add(uint16(tuyable.CodeTime1Request), uint32(0), []byte{}, uint8(8), byte(3))
	f.Fuzz(func(t *testing.T, code uint16, responseTo uint32, data []byte, mtu uint8, proto byte) {
		if len(data) > tuyable.MaxData {
			return
		}
		d := device(3 + proto%2)
		cfg := config()
		cfg.ResponseTimeout = time.Second
		cfg.ReportBuffer = 2
		s, err := open(t, d, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := d.Inject(tuyable.Frame{Code: tuyable.Code(code), ResponseTo: responseTo, Data: data}, int(mtu)); err != nil {
			t.Fatal(err)
		}
		// Whatever arrived, the session is still up and answers. An injected frame that happens to answer the
		// next request (seq 3) with a bad result is the device's answer, not a session failure.
		err = s.Status(context.Background())
		var de *tuyable.DeviceError
		if err != nil && !(responseTo == 3 && (errors.As(err, &de) || errors.Is(err, tuyable.ErrFormat))) {
			t.Fatalf("Status after %#04x %x: %v", code, data, err)
		}
		select {
		case <-s.Done():
			t.Fatalf("session ended after %#04x %x: %v", code, data, s.Err())
		default:
		}
	})
}
