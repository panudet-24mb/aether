package edge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aether/backend/internal/tuyalocal"
)

// DefaultHealthFile is written on every heartbeat (the container's /tmp is a tmpfs); `aether-edge health` reads it.
const DefaultHealthFile = "/tmp/aether-edge.health"

// Timing holds every interval of the agent; Defaults are production values, tests shrink them.
type Timing struct {
	ConfigEvery    time.Duration // configuration pull
	HealthEvery    time.Duration // heartbeat to the server
	DiscoveryEvery time.Duration // LAN list; the server keeps one per 30 s
	Coalesce       time.Duration // numeric state pushes per device
	OfflineAfter   time.Duration // failed reconnects before a device is announced offline
	BackoffMin     time.Duration
	BackoffMax     time.Duration
	Heartbeat      time.Duration // Tuya heartbeat on each device connection
	DialTimeout    time.Duration
	LANFresh       time.Duration // a broadcast older than this no longer counts as "seen"
}

// Defaults are the production intervals.
var Defaults = Timing{
	ConfigEvery: 30 * time.Second, HealthEvery: 60 * time.Second, DiscoveryEvery: 30 * time.Second,
	Coalesce: 2 * time.Second, OfflineAfter: 60 * time.Second, BackoffMin: 5 * time.Second, BackoffMax: 60 * time.Second,
	Heartbeat: 10 * time.Second, DialTimeout: 15 * time.Second, LANFresh: 15 * time.Minute,
}

// ConfigSource is where the agent's device list comes from (the Poller in production).
type ConfigSource interface {
	Fetch(ctx context.Context) (int64, []Device, error)
}

// Listener receives LAN discovery broadcasts (tuyalocal.Listen in production).
type Listener func(ctx context.Context, addrs []string, on func(tuyalocal.Broadcast, net.Addr)) error

// Agent is the running Aether Edge.
type Agent struct {
	cfg    Config
	bus    Bus
	source ConfigSource
	listen Listener
	timing Timing
	log    *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	workers  map[string]*worker
	revision int64
	lan      map[string]lanEntry
	lanDirty bool
}

type lanEntry struct {
	dev  LANDevice
	seen time.Time
}

// NewAgent wires an agent. listen may be nil to disable LAN discovery.
func NewAgent(cfg Config, bus Bus, source ConfigSource, listen Listener, timing Timing, log *slog.Logger) *Agent {
	return &Agent{cfg: cfg, bus: bus, source: source, listen: listen, timing: timing, log: log, now: time.Now,
		workers: map[string]*worker{}, lan: map[string]lanEntry{}}
}

// Run keeps the agent going until ctx ends, then says goodbye (status offline) and closes every connection.
func (a *Agent) Run(ctx context.Context) error {
	a.bus.Subscribe(topicSetFilter(a.cfg.GatewayID), a.onCommand)
	a.announceOnline()
	var wg sync.WaitGroup
	if a.listen != nil {
		wg.Add(1)
		go func() { defer wg.Done(); a.discover(ctx) }()
	}
	a.pull(ctx)
	configT, healthT, discoveryT := time.NewTicker(a.timing.ConfigEvery), time.NewTicker(a.timing.HealthEvery), time.NewTicker(a.timing.DiscoveryEvery)
	defer configT.Stop()
	defer healthT.Stop()
	defer discoveryT.Stop()
	a.health()
	for {
		select {
		case <-ctx.Done():
			a.stopAll()
			wg.Wait()
			if e := a.bus.Publish(topicStatus(a.cfg.GatewayID), true, encodeAvailability(false, "")); e != nil {
				a.log.Warn("goodbye not published", "error", e.Error())
			}
			return nil
		case <-configT.C:
			a.pull(ctx)
		case <-healthT.C:
			a.health()
		case <-discoveryT.C:
			a.publishLAN(false)
		}
	}
}

// announceOnline marks the agent online (retained); the broker's last will marks it offline if it vanishes.
func (a *Agent) announceOnline() {
	if e := a.bus.Publish(topicStatus(a.cfg.GatewayID), true, encodeAvailability(true, "")); e != nil {
		a.log.Warn("status not published", "error", e.Error())
	}
}

// OnConnect is called by the bus after every (re)connection.
func (a *Agent) OnConnect() {
	a.announceOnline()
	a.health()
	a.publishLAN(true)
}

func (a *Agent) pull(ctx context.Context) {
	rev, devices, e := a.source.Fetch(ctx)
	switch {
	case e == nil:
		a.reconcile(ctx, rev, devices)
	case errors.Is(e, errNotModified):
	case errors.Is(e, ErrUnauthorized):
		// The gateway was installed again elsewhere (credentials rotated) or revoked: this agent must stop
		// talking to the devices, which allow a single local connection.
		if a.stopAll() > 0 {
			a.log.Error(e.Error())
		}
	default:
		a.log.Warn("configuration not refreshed", "error", e.Error())
	}
}

// reconcile starts, stops and restarts device workers to match the configuration.
func (a *Agent) reconcile(ctx context.Context, rev int64, devices []Device) {
	want := make(map[string]Device, len(devices))
	for _, d := range devices {
		want[d.ID] = d
	}
	a.mu.Lock()
	a.revision = rev
	var stop []*worker
	for id, w := range a.workers {
		if d, ok := want[id]; !ok || !reflect.DeepEqual(d, w.dev) {
			stop = append(stop, w)
			delete(a.workers, id)
		}
	}
	a.mu.Unlock()
	for _, w := range stop {
		w.stop()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, d := range want {
		if _, ok := a.workers[id]; !ok {
			a.workers[id] = a.startWorker(ctx, d)
		}
	}
	a.log.Info("configuration applied", "revision", rev, "devices", len(want))
}

func (a *Agent) stopAll() int {
	a.mu.Lock()
	ws := make([]*worker, 0, len(a.workers))
	for id, w := range a.workers {
		ws = append(ws, w)
		delete(a.workers, id)
	}
	a.mu.Unlock()
	for _, w := range ws {
		w.stop()
	}
	return len(ws)
}

// onCommand handles aether/edge/<gateway>/<device>/set from the server's commander.
func (a *Agent) onCommand(topic string, payload []byte) {
	rest, ok := strings.CutPrefix(topic, "aether/edge/"+a.cfg.GatewayID+"/")
	device, leaf, found := strings.Cut(rest, "/")
	if !ok || !found || leaf != "set" || !ValidDevice(device) {
		return
	}
	dps, e := parseCommand(payload)
	if e != nil {
		a.log.Warn("command refused: malformed", "device", device)
		return
	}
	a.mu.Lock()
	w := a.workers[device]
	a.mu.Unlock()
	if w == nil {
		a.log.Warn("command refused: device not configured on this agent", "device", device)
		return
	}
	if e := w.set(dps); e != nil {
		a.log.Warn("command not delivered", "device", device, "error", e.Error())
	}
}

func (a *Agent) health() {
	a.mu.Lock()
	connected := 0
	for _, w := range a.workers {
		if w.connected() {
			connected++
		}
	}
	seen := 0
	for _, l := range a.lan {
		if a.now().Sub(l.seen) < a.timing.LANFresh {
			seen++
		}
	}
	a.mu.Unlock()
	b, _ := json.Marshal(healthPayload{Version: Version, DevicesConnected: connected, LANSeen: seen})
	if e := a.bus.Publish(topicHealth(a.cfg.GatewayID), false, b); e != nil {
		a.log.Warn("health not published", "error", e.Error())
	}
	if a.cfg.HealthFile != "" {
		_ = os.WriteFile(a.cfg.HealthFile, []byte(strconv.FormatInt(a.now().Unix(), 10)), 0o600)
	}
}

// discover listens for Tuya LAN broadcasts. Port 7000 (3.5 app solicitations) is optional: if it cannot be bound the
// agent listens on 6666 and 6667 only.
func (a *Agent) discover(ctx context.Context) {
	sets := [][]string{{":6666", ":6667", ":7000"}, {":6666", ":6667"}}
	for ctx.Err() == nil {
		var err error
		for _, addrs := range sets {
			if err = a.listen(ctx, addrs, a.onBroadcast); err == nil || ctx.Err() != nil {
				return
			}
		}
		a.log.Warn("LAN discovery unavailable; retrying", "error", err.Error())
		if !sleep(ctx, time.Minute) {
			return
		}
	}
}

func (a *Agent) onBroadcast(b tuyalocal.Broadcast, from net.Addr) {
	id := strings.ToLower(strings.TrimSpace(b.GwID))
	// The address a device is dialled at is the packet's UDP source, never a value taken from the payload alone: a
	// broadcast claiming another address than it was sent from is dropped.
	src, ok := from.(*net.UDPAddr)
	if !ok || src.IP.To4() == nil {
		return
	}
	ip := src.IP.To4()
	if claimed := strings.TrimSpace(b.IP); claimed != "" {
		if c := net.ParseIP(claimed); c == nil || !c.Equal(ip) {
			return
		}
	}
	if !ValidDevice(id) || !validVersion(b.Version) {
		return
	}
	pk := b.ProductKey
	if len(pk) > 64 {
		pk = pk[:64]
	}
	d := LANDevice{ID: id, IP: ip.String(), Version: b.Version, ProductKey: pk}
	a.mu.Lock()
	prev, known := a.lan[id]
	if !known && len(a.lan) >= MaxLAN {
		// Full: forget the device heard longest ago rather than refusing a new one (a flood of fake ids must not
		// freeze the list), but never one this agent is configured to connect to.
		oldest, at := "", time.Time{}
		for k, l := range a.lan {
			if _, configured := a.workers[k]; configured {
				continue
			}
			if oldest == "" || l.seen.Before(at) {
				oldest, at = k, l.seen
			}
		}
		if oldest == "" {
			a.mu.Unlock()
			return
		}
		delete(a.lan, oldest)
	}
	a.lan[id] = lanEntry{dev: d, seen: a.now()}
	if !known || prev.dev != d {
		a.lanDirty = true
	}
	a.mu.Unlock()
}

// publishLAN sends the devices seen recently when the list changed (or always when forced, after a reconnect).
// It runs on the DiscoveryEvery ticker, so the server receives at most one list per interval.
func (a *Agent) publishLAN(force bool) {
	a.mu.Lock()
	if !a.lanDirty && !force {
		a.mu.Unlock()
		return
	}
	list := make([]LANDevice, 0, len(a.lan))
	for id, l := range a.lan {
		if a.now().Sub(l.seen) >= a.timing.LANFresh {
			delete(a.lan, id)
			continue
		}
		list = append(list, l.dev)
	}
	a.lanDirty = false
	a.mu.Unlock()
	if len(list) == 0 && !force {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, _ := json.Marshal(list)
	if e := a.bus.Publish(topicDiscovery(a.cfg.GatewayID), false, b); e != nil {
		a.log.Warn("LAN list not published", "error", e.Error())
		a.mu.Lock()
		a.lanDirty = true
		a.mu.Unlock()
	}
}

// address picks where to reach a device: the IP it was last seen broadcasting from (DHCP may have moved it), else the
// configured one. The protocol version comes from the configuration (a broadcast never overrides it: a spoofed one
// must not downgrade the protocol), else the broadcast, else 3.3.
func (a *Agent) address(d Device) (string, string) {
	a.mu.Lock()
	l, seen := a.lan[d.ID]
	a.mu.Unlock()
	ip, version := d.IP, d.Version
	if seen && a.now().Sub(l.seen) < a.timing.LANFresh {
		ip = l.dev.IP
		if version == "" {
			version = l.dev.Version
		}
	}
	if version == "" {
		version = "3.3"
	}
	return ip, version
}
