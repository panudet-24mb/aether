package ble

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"aether/backend/internal/tuyable"
)

// Timing holds every interval of the BLE transport; Defaults are production values, tests shrink them.
type Timing struct {
	Tick           time.Duration // scheduler pass
	Fresh          time.Duration // an advertisement younger than this means the device is in range
	NotFoundAfter  time.Duration // unheard this long (or never heard since configured): offline, not_found
	OfflineAfter   time.Duration // failing this long: offline with the failure's reason
	Connect        time.Duration // GATT connection
	Handshake      time.Duration // each request of the tuyable session
	Session        time.Duration // the whole life of a session that is not persistent
	Idle           time.Duration // quiet time that ends a session
	CommandTTL     time.Duration // a command not delivered by then is dropped (the server times it out)
	BackoffMin     time.Duration
	BackoffMax     time.Duration
	SightingsEvery time.Duration // at most one sightings list per interval; the server keeps one per 30 s
	SightingsForce time.Duration // an unchanged list is sent again this often, so the server's copy stays fresh
	ScanRetry      time.Duration
	MinPoll        time.Duration
	DefaultPoll    time.Duration
}

// Defaults are the production intervals. Battery devices pay for every connection: reads are at least five
// minutes apart (MinPoll) and fifteen by default.
var Defaults = Timing{
	Tick: time.Second, Fresh: 10 * time.Minute, NotFoundAfter: 10 * time.Minute, OfflineAfter: 2 * time.Minute,
	Connect: 10 * time.Second, Handshake: 10 * time.Second, Session: 30 * time.Second, Idle: 3 * time.Second,
	CommandTTL: 30 * time.Second, BackoffMin: 10 * time.Second, BackoffMax: 10 * time.Minute,
	SightingsEvery: 30 * time.Second, SightingsForce: 5 * time.Minute, ScanRetry: 30 * time.Second,
	MinPoll: 5 * time.Minute, DefaultPoll: 15 * time.Minute,
}

// MaxHeard bounds the advertisements remembered; MaxSightings the list sent to the server (its own bound).
const (
	MaxHeard     = 500
	MaxSightings = 200
	// handshakeTimeoutsForKey is how many handshakes a device that is advertising may leave unanswered in a row
	// before its key is declared wrong: a device that cannot decrypt the login message often stays silent
	// instead of answering with a frame we cannot decrypt (docs/platform/tuya-ble.md).
	handshakeTimeoutsForKey = 3
)

// Priorities of the scheduler: commands first, then the first read after start, then polls.
const (
	jobCommand = iota
	jobHold
	jobFirst
	jobPoll
)

// Manager runs the BLE transport: the scan, the scheduler and the sessions.
type Manager struct {
	radio  Radio
	pub    Publisher
	timing Timing
	slots  int
	log    *slog.Logger
	now    func() time.Time

	mu        sync.Mutex
	devices   map[string]*device
	heard     map[string]*heard
	dirty     bool
	published time.Time
	state     string
	okCount   int
	failCount int
	active    int
	kick      chan struct{}
	wg        sync.WaitGroup
}

type heard struct {
	mac  string
	adv  tuyable.Advert
	fd50 bool
	rssi int
	at   time.Time
}

type pendingDP struct {
	dp      tuyable.DP
	expires time.Time
}

type device struct {
	cfg     Device
	invalid bool // the keys can never work (malformed): the device is announced once and left alone
	started time.Time

	busy         bool
	holding      bool // a persistent session is running: it gives its slot up to a waiting command
	cancel       context.CancelFunc
	wake         chan struct{} // new commands for the running session
	lastRead     time.Time
	nextTry      time.Time
	backoff      time.Duration
	failingSince time.Time
	timeouts     int
	keyRejected  bool
	pending      map[byte]pendingDP
	announced    string
	learnedMAC   string // the address found through the uuid when none was configured
}

// NewManager builds a manager. slots is the connection budget (1 on a Raspberry Pi's onboard radio).
func NewManager(radio Radio, pub Publisher, slots int, timing Timing, log *slog.Logger) *Manager {
	if slots < 1 {
		slots = 1
	}
	return &Manager{radio: radio, pub: pub, timing: timing, slots: slots, log: log, now: time.Now,
		devices: map[string]*device{}, heard: map[string]*heard{}, state: StateOK, kick: make(chan struct{}, 1)}
}

// Run scans and schedules until ctx ends, then stops every session.
func (m *Manager) Run(ctx context.Context) {
	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.scan(ctx) }()
	tick := time.NewTicker(m.timing.Tick)
	defer tick.Stop()
	sightings := time.NewTicker(m.timing.SightingsEvery)
	defer sightings.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for _, d := range m.devices {
				if d.cancel != nil {
					d.cancel()
				}
			}
			m.mu.Unlock()
			m.wg.Wait()
			return
		case <-tick.C:
		case <-m.kick:
		case <-sightings.C:
			m.publishSightings(false)
			continue
		}
		m.schedule(ctx)
	}
}

func (m *Manager) poke() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) scan(ctx context.Context) {
	for ctx.Err() == nil {
		m.setState(StateOK)
		e := m.radio.Scan(ctx, m.onAdvert)
		if ctx.Err() != nil {
			return
		}
		if e == nil {
			e = errors.New("scan ended")
		}
		m.setState(stateOf(e))
		m.log.Warn("Bluetooth scan stopped; retrying", "adapter", m.radio.Adapter(), "error", e.Error())
		if !sleep(ctx, m.timing.ScanRetry) {
			return
		}
	}
}

func (m *Manager) setState(s string) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
}

// onAdvert keeps a Tuya advertisement. Anything that does not parse as one (phones, other beacons) is ignored.
func (m *Manager) onAdvert(a Advertisement) {
	mac := NormalMAC(a.MAC)
	if mac == "" || (len(a.ServiceData) == 0 && len(a.ManufacturerData) == 0) {
		return
	}
	adv, e := tuyable.ParseAdvert(a.ServiceData, a.ManufacturerData)
	if e != nil {
		return
	}
	now := m.now()
	m.mu.Lock()
	h, known := m.heard[mac]
	if !known && len(m.heard) >= MaxHeard {
		// Full: forget the device heard longest ago rather than refusing a new one (a flood of fake addresses must
		// not freeze the list), but never a configured device's address; with only those left, the newcomer is
		// dropped unless it is one of them.
		oldest := ""
		for k, x := range m.heard {
			if !m.known(k) && (oldest == "" || x.at.Before(m.heard[oldest].at)) {
				oldest = k
			}
		}
		if oldest == "" && !m.known(mac) {
			m.mu.Unlock()
			return
		}
		delete(m.heard, oldest)
	}
	if !known {
		h = &heard{mac: mac}
		m.heard[mac] = h
	}
	if !known || h.adv != adv || h.fd50 != a.FD50 {
		m.dirty = true
	}
	h.adv, h.fd50, h.rssi, h.at = adv, a.FD50, a.RSSI, now
	m.mu.Unlock()
}

// Configure replaces the registered devices. A device whose configuration changed starts over (its session, if
// any, is stopped); an unchanged one keeps its state.
func (m *Manager) Configure(list []Device) {
	want := make(map[string]Device, len(list))
	for _, d := range list {
		want[d.ID] = d
	}
	now := m.now()
	m.mu.Lock()
	for id, d := range m.devices {
		if w, ok := want[id]; !ok || !reflect.DeepEqual(w, d.cfg) {
			if d.cancel != nil {
				d.cancel()
			}
			delete(m.devices, id)
		}
	}
	var invalid []string
	for id, cfg := range want {
		if _, ok := m.devices[id]; ok {
			continue
		}
		d := &device{cfg: cfg, started: now, pending: map[byte]pendingDP{}, wake: make(chan struct{}, 1)}
		if _, e := tuyable.NewKeys(cfg.LocalKey, cfg.SecKey); e != nil {
			d.invalid = true
			invalid = append(invalid, id)
		}
		m.devices[id] = d
	}
	m.mu.Unlock()
	for _, id := range invalid {
		m.log.Warn("BLE device keys are malformed; import them again", "device", id)
		m.announce(id, false, ReasonAuthFailed)
	}
	m.poke()
}

// Has reports whether a device is configured on the BLE transport.
func (m *Manager) Has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.devices[id]
	return ok
}

var (
	// ErrNotConfigured: a command for a device this transport does not hold.
	ErrNotConfigured = errors.New("ble: device not configured on this agent")
	// ErrReadOnly: a command for a device configured read-only (a lock).
	ErrReadOnly = errors.New("ble: device is read-only")
)

// Command queues data-point values for a device, as the server's commander sent them ({"<id>":<raw>}, numbers as
// json.Number). Values are encoded with the device's data-point types; the latest value of each data point wins
// until it is delivered or expires: after CommandTTL, or at deadline when that is earlier (the server's own
// deadline for the command; zero means none). A write is never started after it.
func (m *Manager) Command(id string, dps map[string]any, deadline time.Time) error {
	m.mu.Lock()
	d, ok := m.devices[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotConfigured
	}
	if d.cfg.ReadOnly {
		m.mu.Unlock()
		return ErrReadOnly
	}
	encoded := make([]tuyable.DP, 0, len(dps))
	for k, v := range dps {
		dp, e := encodeDP(k, v, d.cfg.DPTypes)
		if e != nil {
			m.mu.Unlock()
			return e
		}
		encoded = append(encoded, dp)
	}
	expires := m.now().Add(m.timing.CommandTTL)
	if !deadline.IsZero() && deadline.Before(expires) {
		expires = deadline
	}
	if !m.now().Before(expires) {
		m.mu.Unlock()
		return errors.New("ble: command arrived after its deadline")
	}
	for _, dp := range encoded {
		d.pending[dp.ID] = pendingDP{dp: dp, expires: expires}
	}
	wake := d.wake
	m.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
	m.poke()
	return nil
}

// encodeDP turns one command value into a typed data point. Enums arrive as their index (the server's WireBLE).
func encodeDP(key string, v any, types map[byte]tuyable.DPType) (tuyable.DP, error) {
	n, e := strconv.Atoi(key)
	if e != nil || n < 1 || n > 255 || strconv.Itoa(n) != key {
		return tuyable.DP{}, errors.New("ble: malformed data point id")
	}
	id := byte(n)
	t, ok := types[id]
	if !ok {
		return tuyable.DP{}, fmt.Errorf("ble: data point %d has no known type", n)
	}
	integer := func(lo, hi int64) (int64, bool) {
		var f float64
		switch x := v.(type) {
		case json.Number:
			if i, e := x.Int64(); e == nil {
				return i, i >= lo && i <= hi
			}
			return 0, false
		case float64:
			f = x
		case int:
			f = float64(x)
		case int64:
			f = float64(x)
		default:
			return 0, false
		}
		if f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
			return 0, false
		}
		return int64(f), true
	}
	switch t {
	case tuyable.DPBool:
		if b, ok := v.(bool); ok {
			return tuyable.DP{ID: id, Type: t, Value: b}, nil
		}
	case tuyable.DPValue:
		if i, ok := integer(math.MinInt32, math.MaxInt32); ok {
			return tuyable.DP{ID: id, Type: t, Value: i}, nil
		}
	case tuyable.DPEnum:
		if i, ok := integer(0, 255); ok {
			return tuyable.DP{ID: id, Type: t, Value: i}, nil
		}
	case tuyable.DPString:
		if s, ok := v.(string); ok && len(s) <= 255 {
			return tuyable.DP{ID: id, Type: t, Value: s}, nil
		}
	}
	return tuyable.DP{}, fmt.Errorf("ble: value of data point %d does not fit its type %s", n, t)
}

// values turns a report into the state message's data points: bool, integer (value and enum index), string, a
// bitmap as its integer, raw bytes as hex.
func values(r tuyable.Report) map[string]any {
	out := make(map[string]any, len(r.DPs))
	for _, dp := range r.DPs {
		k := strconv.Itoa(int(dp.ID))
		switch v := dp.Value.(type) {
		case bool, int64, string:
			out[k] = v
		case []byte:
			if dp.Type == tuyable.DPBitmap && len(v) <= 8 {
				var n uint64
				for _, b := range v {
					n = n<<8 | uint64(b)
				}
				out[k] = n
			} else {
				out[k] = hex.EncodeToString(v)
			}
		}
	}
	return out
}

// lookup finds what the radio heard of a device. A device with a configured address is found by that address only
// (either byte order): anyone can advertise another device's uuid, so the uuid never redirects it elsewhere. Only a
// device configured without an address is found by the uuid in its advertisement, and the address it was first
// found at is then kept for it.
func (m *Manager) lookup(d *device) *heard {
	if mac := NormalMAC(d.cfg.MAC); mac != "" {
		if h := m.heard[mac]; h != nil {
			return h
		}
		return m.heard[reversedMAC(mac)]
	}
	if d.learnedMAC != "" {
		return m.heard[d.learnedMAC]
	}
	if d.cfg.UUID != "" {
		for _, h := range m.heard {
			if h.adv.UUID == d.cfg.UUID {
				d.learnedMAC = h.mac
				return h
			}
		}
	}
	return nil
}

// known reports whether an address belongs to a configured device (its address in either byte order, or the one
// learned for it). Called with m.mu held.
func (m *Manager) known(mac string) bool {
	for _, d := range m.devices {
		c := NormalMAC(d.cfg.MAC)
		if (c != "" && (c == mac || reversedMAC(c) == mac)) || d.learnedMAC == mac {
			return true
		}
	}
	return false
}

type job struct {
	id   string
	d    *device
	kind int
	h    heard
}

// current reports whether the job's device is still the configured one (a configuration change replaces it).
// Called with m.mu held.
func (m *Manager) current(j job) bool { return m.devices[j.id] == j.d }

// schedule starts sessions for the devices that need one, by priority, within the connection budget.
func (m *Manager) schedule(ctx context.Context) {
	now := m.now()
	var jobs []job
	var notFound []string
	m.mu.Lock()
	for id, d := range m.devices {
		for k, p := range d.pending {
			if now.After(p.expires) {
				delete(d.pending, k)
				m.log.Warn("BLE command dropped: not delivered in time", "device", id, "dp", k)
			}
		}
		if d.busy || d.invalid {
			continue
		}
		h := m.lookup(d)
		if h == nil || now.Sub(h.at) >= m.timing.Fresh {
			last := d.started
			if h != nil {
				last = h.at
			}
			if now.Sub(last) >= m.timing.NotFoundAfter {
				notFound = append(notFound, id)
			}
			continue
		}
		commands := len(d.pending) > 0
		// A key the device refused is not retried before its backoff, even for a command: it cannot work.
		if now.Before(d.nextTry) && (!commands || d.keyRejected) {
			continue
		}
		kind := -1
		switch {
		case commands:
			kind = jobCommand
		case d.cfg.Mode == ModePersistent:
			kind = jobHold
		case d.cfg.Mode != ModeOnDemand && d.lastRead.IsZero():
			kind = jobFirst
		default:
			since := d.lastRead
			if since.IsZero() {
				since = d.started // on demand: the first read comes one poll interval after start
			}
			if now.Sub(since) >= m.poll(d) {
				kind = jobPoll
			}
		}
		if kind >= 0 {
			jobs = append(jobs, job{id: id, d: d, kind: kind, h: *h})
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].kind != jobs[j].kind {
			return jobs[i].kind < jobs[j].kind
		}
		return jobs[i].d.lastRead.Before(jobs[j].d.lastRead)
	})
	var start []job
	for _, j := range jobs {
		if m.active >= m.slots {
			// A held (persistent) connection must not keep a command waiting when it holds the only free slot:
			// it is closed and taken up again after the command.
			if j.kind == jobCommand {
				for _, other := range m.devices {
					if other.busy && other.holding && other.cancel != nil {
						other.holding = false
						other.cancel()
						break
					}
				}
			}
			break
		}
		sctx, cancel := context.WithCancel(ctx)
		j.d.busy, j.d.cancel, j.d.holding = true, cancel, j.d.cfg.Mode == ModePersistent
		m.active++
		start = append(start, j)
		m.wg.Add(1)
		go func(j job, ctx context.Context, cancel context.CancelFunc) {
			defer m.wg.Done()
			defer cancel()
			m.session(ctx, j)
		}(j, sctx, cancel)
	}
	m.mu.Unlock()
	for _, id := range notFound {
		m.announce(id, false, ReasonNotFound)
	}
}

func (m *Manager) poll(d *device) time.Duration {
	p := d.cfg.Poll
	if p <= 0 {
		p = m.timing.DefaultPoll
	}
	if p < m.timing.MinPoll {
		p = m.timing.MinPoll
	}
	return p
}

// session connects, authenticates and serves one device until it is idle (or, persistent, until the link drops).
func (m *Manager) session(ctx context.Context, j job) {
	defer func() {
		m.mu.Lock()
		j.d.busy, j.d.cancel, j.d.holding = false, nil, false
		if j.d.cfg.Mode == ModePersistent && j.d.nextTry.Before(m.now()) {
			// A held connection that dropped is taken up again, but not in a tight loop.
			j.d.nextTry = m.now().Add(m.timing.BackoffMin)
		}
		m.active--
		m.mu.Unlock()
		m.poke()
	}()
	cctx, cancel := context.WithTimeout(ctx, m.timing.Connect)
	link, e := m.radio.Connect(cctx, j.h.mac, j.h.fd50)
	cancel()
	if e != nil {
		if ctx.Err() == nil {
			reason := ReasonUnreachable
			if errors.Is(e, tuyable.ErrBusy) {
				reason = ReasonBusy
			}
			m.failed(j, reason, e)
		}
		return
	}
	cfg := j.d.cfg
	uuid, product, proto := cfg.UUID, cfg.ProductID, j.h.adv.Protocol
	if uuid == "" {
		uuid = j.h.adv.UUID
	}
	if product == "" {
		product = j.h.adv.ProductID
	}
	if proto == 0 {
		proto = cfg.Protocol
	}
	hctx, cancel := context.WithTimeout(ctx, 3*m.timing.Handshake)
	s, e := tuyable.Open(hctx, link, tuyable.Config{
		Keys: tuyable.Keys{LocalKey: cfg.LocalKey, SecKey: cfg.SecKey}, UUID: uuid, DeviceID: cfg.ID, ProductID: product,
		FD50: j.h.fd50, AdvertisedProtocol: proto, ResponseTimeout: m.timing.Handshake,
	})
	cancel()
	if e != nil {
		if ctx.Err() == nil {
			m.handshakeFailed(j, e)
		}
		return
	}
	defer s.Close()
	m.succeeded(j)
	if j.kind != jobCommand {
		sctx, cancel := context.WithTimeout(ctx, m.timing.Handshake)
		e := s.Status(sctx)
		cancel()
		if e != nil && ctx.Err() == nil {
			m.log.Info("BLE status request failed", "device", j.id, "error", e.Error())
			return
		}
	}
	persistent := cfg.Mode == ModePersistent
	var deadline <-chan time.Time
	if !persistent {
		t := time.NewTimer(m.timing.Session)
		defer t.Stop()
		deadline = t.C
	}
	idle := time.NewTimer(m.timing.Idle)
	defer idle.Stop()
	for {
		if e := m.deliver(ctx, s, j); e != nil {
			m.log.Info("BLE command not delivered", "device", j.id, "error", e.Error())
			return
		}
		select {
		case r, ok := <-s.Reports():
			if !ok {
				return
			}
			if r.Coalesced {
				m.log.Debug("BLE reports merged; some intermediate values were lost", "device", j.id)
			}
			if dps := values(r); len(dps) > 0 {
				m.pub.State(j.id, dps)
			}
			m.mu.Lock()
			j.d.lastRead = m.now()
			m.mu.Unlock()
			idle.Reset(m.timing.Idle)
		case <-j.d.wake:
		case <-idle.C:
			if !persistent {
				return
			}
			idle.Reset(m.timing.Idle)
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

// deliver writes the device's pending commands in one request. A failed write puts them back unless a newer value
// arrived meanwhile; a device that cannot take writes (protocol 2) drops them.
func (m *Manager) deliver(ctx context.Context, s *tuyable.Session, j job) error {
	m.mu.Lock()
	if j.d.cfg.ReadOnly {
		j.d.pending = map[byte]pendingDP{}
		m.mu.Unlock()
		return nil
	}
	var dps []tuyable.DP
	now := m.now()
	taken := map[byte]pendingDP{}
	for k, p := range j.d.pending {
		if now.After(p.expires) {
			continue
		}
		dps = append(dps, p.dp)
		taken[k] = p
	}
	for k := range taken {
		delete(j.d.pending, k)
	}
	m.mu.Unlock()
	if len(dps) == 0 {
		return nil
	}
	sort.Slice(dps, func(a, b int) bool { return dps[a].ID < dps[b].ID })
	wctx, cancel := context.WithTimeout(ctx, m.timing.Handshake)
	e := s.SetDPs(wctx, dps)
	cancel()
	if e == nil {
		return nil
	}
	if errors.Is(e, tuyable.ErrUnsupportedProtocol) {
		m.log.Warn("BLE device cannot take commands (protocol 2)", "device", j.id)
		return nil
	}
	m.mu.Lock()
	for k, p := range taken {
		if _, newer := j.d.pending[k]; !newer {
			j.d.pending[k] = p
		}
	}
	m.mu.Unlock()
	return e
}

func (m *Manager) succeeded(j job) {
	m.mu.Lock()
	m.okCount++
	stale := !m.current(j)
	if !stale {
		j.d.backoff, j.d.nextTry, j.d.failingSince, j.d.timeouts, j.d.keyRejected = 0, time.Time{}, time.Time{}, 0, false
		j.d.lastRead = m.now()
	}
	m.mu.Unlock()
	if !stale {
		m.announce(j.id, true, "")
	}
}

// handshakeFailed classifies a failed handshake: a refused key is definitive; repeated silence from a device that
// is advertising means the same (it could not decrypt our login message); anything else is unreachable.
func (m *Manager) handshakeFailed(j job, e error) {
	reason := ReasonUnreachable
	switch {
	case errors.Is(e, tuyable.ErrKeyRejected):
		reason = ReasonAuthFailed
	case errors.Is(e, tuyable.ErrTimeout):
		m.mu.Lock()
		j.d.timeouts++
		if j.d.timeouts >= handshakeTimeoutsForKey {
			reason = ReasonAuthFailed
		}
		m.mu.Unlock()
	}
	m.failed(j, reason, e)
}

func (m *Manager) failed(j job, reason string, e error) {
	now := m.now()
	m.mu.Lock()
	m.failCount++
	if !m.current(j) {
		m.mu.Unlock()
		return
	}
	d := j.d
	if d.failingSince.IsZero() {
		d.failingSince = now
	}
	if d.backoff < m.timing.BackoffMin {
		d.backoff = m.timing.BackoffMin
	} else if d.backoff *= 2; d.backoff > m.timing.BackoffMax {
		d.backoff = m.timing.BackoffMax
	}
	announce := now.Sub(d.failingSince) >= m.timing.OfflineAfter
	if reason == ReasonAuthFailed {
		// Proven, not guessed: say so at once so the user can import again, and stop hammering the device.
		d.keyRejected, d.backoff, announce = true, m.timing.BackoffMax, true
	}
	d.nextTry = now.Add(d.backoff)
	m.mu.Unlock()
	m.log.Info("BLE session failed", "device", j.id, "reason", reason, "error", e.Error())
	if announce {
		m.announce(j.id, false, reason)
	}
}

// announce publishes a device's availability when it changed.
func (m *Manager) announce(id string, online bool, reason string) {
	key := "online"
	if !online {
		key = "offline:" + reason
	}
	m.mu.Lock()
	d, ok := m.devices[id]
	if !ok || d.announced == key {
		m.mu.Unlock()
		return
	}
	d.announced = key
	m.mu.Unlock()
	m.pub.Availability(id, online, reason)
}

// publishSightings sends the Tuya BLE devices heard recently when the list changed, and every SightingsForce
// anyway.
func (m *Manager) publishSightings(force bool) {
	now := m.now()
	m.mu.Lock()
	if !force && !m.dirty && now.Sub(m.published) < m.timing.SightingsForce {
		m.mu.Unlock()
		return
	}
	list := make([]Sighting, 0, len(m.heard))
	mine := map[string]bool{}
	for mac, h := range m.heard {
		if now.Sub(h.at) >= time.Hour {
			delete(m.heard, mac)
			continue
		}
		if now.Sub(h.at) >= m.timing.Fresh {
			continue
		}
		rssi := h.rssi
		mine[mac] = m.known(mac)
		list = append(list, Sighting{MAC: mac, UUID: h.adv.UUID, ProductID: h.adv.ProductID, Protocol: int(h.adv.Protocol),
			Bound: h.adv.Bound, FD50: h.fd50, RSSI: &rssi})
	}
	m.dirty, m.published = false, now
	m.mu.Unlock()
	if len(list) == 0 && !force {
		return
	}
	// Configured devices first, then the strongest: when more are heard than a list holds, a crowd of strong
	// strangers (or spoofed adverts) must not push the registered devices out.
	sort.Slice(list, func(i, j int) bool {
		if mine[list[i].MAC] != mine[list[j].MAC] {
			return mine[list[i].MAC]
		}
		if *list[i].RSSI != *list[j].RSSI {
			return *list[i].RSSI > *list[j].RSSI
		}
		return list[i].MAC < list[j].MAC
	})
	if len(list) > MaxSightings {
		list = list[:MaxSightings]
	}
	m.pub.Sightings(list)
}

// PublishSightings sends the list now (after a reconnect).
func (m *Manager) PublishSightings() { m.publishSightings(true) }

// Health is the Bluetooth part of the heartbeat.
func (m *Manager) Health() Health {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	h := Health{State: m.state, Adapter: m.radio.Adapter(), Connected: m.active, SessionsOK: m.okCount, SessionsFailed: m.failCount}
	for _, x := range m.heard {
		if now.Sub(x.at) < m.timing.Fresh {
			h.Seen++
		}
	}
	for _, d := range m.devices {
		h.Queue += len(d.pending)
	}
	return h
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
