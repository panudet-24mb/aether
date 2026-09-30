// Package tuyacloudlink is the tuya-cloud worker: it runs every Tuya Cloud link (docs/platform/tuya-cloud.md).
// Per link it consumes the cloud project's Message Service over Pulsar's WebSocket API and stores what it says
// (CaptureTuyaCloud), keeps the device list in sync, delivers cloud commands through the OpenAPI, counts the
// month's usage against the project's budget, and reports its health.
//
// The worker is the only process that can open a project's credentials (package cloudkeys). It reads them
// inside the link's tenant transaction, keeps them in memory only as long as the link runs, and never logs them:
// log lines name the tenant and gateway, never an Access ID, a secret, a token or a message's values.
package tuyacloudlink

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/commander"
	"aether/backend/internal/domain"
	"aether/backend/internal/tuyacloudlink/cloudkeys"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Store is the database half (implemented by postgres.Repository).
type Store interface {
	commander.Store
	CloudLinks(ctx context.Context) ([]domain.CloudLinkRef, error)
	OpenLinkRow(ctx context.Context, tenant, gateway string) (domain.CloudLinkSecret, error)
	SetCloudLinkState(ctx context.Context, tenant, gateway string, revision int64, state, reason string, code int64) error
	CaptureTuyaCloud(ctx context.Context, tenant, gateway string, protocol int, events []tuyacloud.Event, registeredOnly bool) (postgres.CloudCaptureResult, error)
	SaveCloudDevices(ctx context.Context, tenant, gateway string, devices []postgres.CloudImport) (int, error)
	CloudSyncWanted(ctx context.Context, tenant, gateway string) (*time.Time, []string, error)
	CompleteCloudSync(ctx context.Context, tenant, gateway string, requested time.Time) error
	RecordCloudUsage(ctx context.Context, tenant, gateway string, add postgres.CloudUsage, lastEvent *time.Time, now time.Time) (postgres.CloudUsage, error)
	RecordCloudHealth(ctx context.Context, tenant, gateway string, h postgres.CloudHealth) error
}

// Timings. Tests shorten them; production uses the defaults.
type Timings struct {
	Reconcile   time.Duration // how often the set of links is read again (a notification wakes it sooner)
	Health      time.Duration // how often each link flushes its usage and writes a health report
	Ping        time.Duration // WebSocket ping interval
	ReadTimeout time.Duration // the connection is dropped when nothing (not even a pong) arrived for this long
	BackoffMin  time.Duration
	BackoffMax  time.Duration
	SyncEvery   time.Duration // the least time between two device-list syncs of a link
}

// DefaultTimings are the production values.
var DefaultTimings = Timings{Reconcile: 30 * time.Second, Health: 60 * time.Second, Ping: 30 * time.Second, ReadTimeout: 75 * time.Second,
	BackoffMin: time.Second, BackoffMax: 5 * time.Minute, SyncEvery: postgres.CloudSyncDebounce}

// Limits of one link.
const (
	EventsPerSecond = 20.0 // messages stored per second, per link; faster ones wait (Pulsar keeps them)
	SeenMessages    = 4096 // message ids remembered for de-duplication
	MaxRedelivery   = 3    // a message delivered more often than this is acknowledged and dropped
	CommandRate     = 2.0  // cloud commands per second, per link
	CommandBurst    = 4.0
	// DrainLimit is the largest frame read at all: a frame above tuyacloud.MaxFrame is drained, acknowledged by id
	// and dropped; one above DrainLimit closes the connection.
	DrainLimit = 1 << 20
)

// Worker runs every link it is allowed to (at most MaxLinks).
type Worker struct {
	Store Store
	// Keys open the sealed credentials: the worker's private key (TUYA_CLOUD_PRIVATE_KEY), newest first.
	Keys []*cloudkeys.Key
	// MaxLinks bounds the links running at once (parked links do not count); MaxLinksPerTenant bounds one tenant's
	// share, and slots are handed out round-robin across tenants, so one tenant can never take them all.
	MaxLinks          int
	MaxLinksPerTenant int
	Budget            Budget
	Timings           Timings
	Now               func() time.Time

	// Test seams. Production leaves them nil: hosts come from the region tables, TLS is verified against the
	// system roots.
	ConsumerURL func(region, accessID, channel string) (string, error)
	NewClient   func(region, accessID, secret string) (*tuyacloud.Client, error)
	TLS         *tls.Config

	mu    sync.Mutex
	links map[string]*link
	wg    sync.WaitGroup
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) timings() Timings {
	t := w.Timings
	d := DefaultTimings
	if t.Reconcile <= 0 {
		t.Reconcile = d.Reconcile
	}
	if t.Health <= 0 {
		t.Health = d.Health
	}
	if t.Ping <= 0 {
		t.Ping = d.Ping
	}
	if t.ReadTimeout <= 0 {
		t.ReadTimeout = d.ReadTimeout
	}
	if t.BackoffMin <= 0 {
		t.BackoffMin = d.BackoffMin
	}
	if t.BackoffMax <= 0 {
		t.BackoffMax = d.BackoffMax
	}
	if t.SyncEvery <= 0 {
		t.SyncEvery = d.SyncEvery
	}
	return t
}

func linkKey(tenant, gateway string) string { return tenant + "/" + gateway }

// Run reconciles the running links with the database until ctx ends: at once, every Reconcile, and whenever
// wake fires (NOTIFY aether_tuya_cloud: a link saved, rotated, unlinked, revoked or asked to sync).
func (w *Worker) Run(ctx context.Context, wake <-chan struct{}) {
	t := time.NewTicker(w.timings().Reconcile)
	defer t.Stop()
	for {
		if e := w.Reconcile(ctx); e != nil && ctx.Err() == nil {
			slog.Warn("tuya cloud: links not read; retrying", "error", e.Error())
		}
		select {
		case <-ctx.Done():
			w.stopAll()
			return
		case <-wake:
		case <-t.C:
		}
	}
}

// fairOrder interleaves the links of all tenants (each tenant's first link, then each tenant's second, ...), so
// when slots are short every tenant gets one before any gets two.
func fairOrder(refs []domain.CloudLinkRef) []domain.CloudLinkRef {
	sort.Slice(refs, func(i, j int) bool {
		return linkKey(refs[i].TenantID, refs[i].GatewayID) < linkKey(refs[j].TenantID, refs[j].GatewayID)
	})
	byTenant := map[string][]domain.CloudLinkRef{}
	tenants := []string{}
	for _, r := range refs {
		if _, ok := byTenant[r.TenantID]; !ok {
			tenants = append(tenants, r.TenantID)
		}
		byTenant[r.TenantID] = append(byTenant[r.TenantID], r)
	}
	out := make([]domain.CloudLinkRef, 0, len(refs))
	for round := 0; len(out) < len(refs); round++ {
		for _, t := range tenants {
			if round < len(byTenant[t]) {
				out = append(out, byTenant[t][round])
			}
		}
	}
	return out
}

// Reconcile starts the links that should run, stops those that should not (unlinked, revoked, tenant
// suspended) and restarts those whose revision changed (new credentials). A link parked on refused credentials
// stays parked until its revision changes, and does not hold a slot. Every running link is asked to check for a
// sync request.
func (w *Worker) Reconcile(ctx context.Context) error {
	refs, e := w.Store.CloudLinks(ctx)
	if e != nil {
		return e
	}
	refs = fairOrder(refs)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.links == nil {
		w.links = map[string]*link{}
	}
	wanted := map[string]domain.CloudLinkRef{}
	for _, r := range refs {
		wanted[linkKey(r.TenantID, r.GatewayID)] = r
	}
	for k, l := range w.links {
		if r, ok := wanted[k]; !ok || r.Revision != l.revision {
			l.stop()
			delete(w.links, k)
		}
	}
	live, perTenant := 0, map[string]int{}
	for _, l := range w.links {
		if !l.isParked() {
			live++
			perTenant[l.tenant]++
		}
	}
	for _, r := range refs {
		k := linkKey(r.TenantID, r.GatewayID)
		if l, ok := w.links[k]; ok {
			l.poke()
			continue
		}
		if (w.MaxLinks > 0 && live >= w.MaxLinks) || (w.MaxLinksPerTenant > 0 && perTenant[r.TenantID] >= w.MaxLinksPerTenant) {
			slog.Warn("tuya cloud: link limit reached; link not started", "tenant", r.TenantID, "gateway", r.GatewayID, "max", w.MaxLinks, "per_tenant", w.MaxLinksPerTenant)
			continue
		}
		live++
		perTenant[r.TenantID]++
		l := w.newLink(ctx, r)
		w.links[k] = l
		w.wg.Add(1)
		go w.supervise(ctx, l)
	}
	return nil
}

// supervise runs one link and, when it panicked, starts a fresh one after a backoff: a fault in one tenant's
// link never takes the others (or the process) down. A link that ended any other way stays as it is.
func (w *Worker) supervise(parent context.Context, l *link) {
	defer w.wg.Done()
	for failures := 0; ; failures++ {
		l.run()
		if !l.didPanic() || parent.Err() != nil {
			return
		}
		t := time.NewTimer(backoff(failures, l.t.BackoffMin, l.t.BackoffMax))
		select {
		case <-parent.Done():
			t.Stop()
			return
		case <-t.C:
		}
		next := w.newLink(parent, domain.CloudLinkRef{TenantID: l.tenant, GatewayID: l.gateway, Revision: l.revision, State: l.currentState()})
		k := linkKey(l.tenant, l.gateway)
		w.mu.Lock()
		if w.links[k] != l { // stopped or replaced meanwhile
			w.mu.Unlock()
			return
		}
		w.links[k] = next
		w.mu.Unlock()
		l = next
	}
}

func (w *Worker) stopAll() {
	w.mu.Lock()
	for k, l := range w.links {
		l.stop()
		delete(w.links, k)
	}
	w.mu.Unlock()
	w.wg.Wait()
}

// Wait blocks until every link goroutine ended (after ctx was cancelled).
func (w *Worker) Wait() { w.wg.Wait() }

// linkFor returns the running link of a gateway, for the command sender.
func (w *Worker) linkFor(tenant, gateway string) *link {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.links[linkKey(tenant, gateway)]
}

// tenants are the tenants with a running link: the command dispatcher sweeps only those.
func (w *Worker) tenants() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, l := range w.links {
		if !seen[l.tenant] {
			seen[l.tenant] = true
			out = append(out, l.tenant)
		}
	}
	sort.Strings(out)
	return out
}

// link is one running Tuya Cloud link.
type link struct {
	w        *Worker
	tenant   string
	gateway  string
	revision int64
	ctx      context.Context
	cancel   context.CancelFunc
	pokes    chan struct{}
	usage    *usage
	t        Timings

	mu        sync.Mutex
	creds     cloudkeys.Credentials
	region    string
	channel   string
	sess      *session
	connected bool
	state     string
	lastEvent *time.Time
	parked    bool
	panicked  bool
}

func (w *Worker) newLink(parent context.Context, r domain.CloudLinkRef) *link {
	ctx, cancel := context.WithCancel(parent)
	return &link{w: w, tenant: r.TenantID, gateway: r.GatewayID, revision: r.Revision, ctx: ctx, cancel: cancel,
		pokes: make(chan struct{}, 1), usage: &usage{budget: w.Budget}, t: w.timings(), state: r.State}
}

func (l *link) stop() { l.cancel() }

func (l *link) isParked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.parked
}

func (l *link) didPanic() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.panicked
}

func (l *link) currentState() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// park leaves the link registered but idle until its revision changes; it no longer holds a slot.
func (l *link) park() {
	l.mu.Lock()
	l.parked = true
	l.mu.Unlock()
}

// guard, deferred at the top of every goroutine of a link, turns a panic into a restart of that link. The panic's
// value is not logged: it may carry a message's values.
func (l *link) guard(where string) {
	if v := recover(); v != nil {
		slog.Error("tuya cloud: link panicked; restarting it", "tenant", l.tenant, "gateway", l.gateway, "in", where)
		l.mu.Lock()
		l.panicked = true
		l.mu.Unlock()
		l.cancel()
	}
}

func (l *link) poke() {
	select {
	case l.pokes <- struct{}{}:
	default:
	}
}

func (l *link) log(msg string, args ...any) {
	slog.Info(msg, append([]any{"tenant", l.tenant, "gateway", l.gateway}, args...)...)
}

func (l *link) warn(msg string, args ...any) {
	slog.Warn(msg, append([]any{"tenant", l.tenant, "gateway", l.gateway}, args...)...)
}

// setState records the worker's verdict once per change (and always for a down state, whose reason may change).
// The cached state moves only once the database took it, so a failed write is tried again next time.
func (l *link) setState(state, reason string, code int64) {
	l.mu.Lock()
	same := l.state == state && state == domain.CloudLinkOnline
	l.mu.Unlock()
	if same {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 10*time.Second)
	defer cancel()
	if e := l.w.Store.SetCloudLinkState(ctx, l.tenant, l.gateway, l.revision, state, reason, code); e != nil {
		if l.ctx.Err() == nil {
			l.warn("tuya cloud: link state not recorded", "state", state, "error", e.Error())
		}
		return
	}
	l.mu.Lock()
	l.state = state
	l.mu.Unlock()
}

// errUnusable is a link whose stored credentials can never work as they are (they do not open, or are not valid
// Tuya credentials): the link parks until they are saved again.
var errUnusable = errors.New("credentials unusable")

// open reads and opens the link's credentials inside its tenant. A database failure is returned as it is (the
// caller retries); only credentials that cannot work are errUnusable.
func (l *link) open() error {
	row, e := l.w.Store.OpenLinkRow(l.ctx, l.tenant, l.gateway)
	if e != nil {
		return e
	}
	if row.Revision != l.revision {
		return domain.ErrConflict // rotated since the reconcile: the next one restarts the link
	}
	creds, e := cloudkeys.Open(l.tenant, l.gateway, row.CredentialsSealed, l.w.Keys...)
	if e != nil {
		return errUnusable
	}
	newClient := l.w.NewClient
	if newClient == nil {
		newClient = tuyacloud.New
	}
	client, e := newClient(row.Region, creds.AccessID, creds.AccessSecret)
	if e != nil {
		return errUnusable
	}
	l.mu.Lock()
	l.creds, l.region, l.channel = creds, row.Region, row.Channel
	l.sess = &session{client: client, now: l.w.now, count: func(n int64) { l.usage.add(0, n, 0) }}
	l.mu.Unlock()
	return nil
}

// run is the link's life: open the credentials, then consume, sync and report health until the link is stopped
// or its credentials are refused. A link that ended stays registered (parked) until its revision changes.
func (l *link) run() {
	// Deferred in this order so that a panic is recovered first, then the link cancelled, then its goroutines
	// awaited: a restarted link never overlaps its predecessor.
	var wg sync.WaitGroup
	defer wg.Wait()
	defer l.cancel()
	defer l.guard("run")
	for failures := 0; ; failures++ {
		e := l.open()
		if e == nil {
			break
		}
		switch {
		case l.ctx.Err() != nil, errors.Is(e, domain.ErrConflict), errors.Is(e, domain.ErrNotFound):
			return // stopped, rotated or unlinked: the next reconcile decides
		case errors.Is(e, errUnusable):
			l.warn("tuya cloud: credentials unreadable; link parked until they are saved again")
			l.setState(domain.CloudLinkAuthFailed, "credentials_unreadable", 0)
			l.park()
			return
		}
		// A transient database failure: try again later, the link keeps its slot.
		l.warn("tuya cloud: link configuration not read; retrying", "error", e.Error())
		if !l.sleep(backoff(failures, l.t.BackoffMin, l.t.BackoffMax)) {
			return
		}
	}
	l.log("tuya cloud: link started", "revision", l.revision)
	wg.Add(2)
	go func() { defer wg.Done(); defer l.guard("health"); l.healthLoop() }()
	go func() { defer wg.Done(); defer l.guard("sync"); l.syncLoop() }()
	l.consume()
	l.cancel()
	wg.Wait()
	l.flush(context.WithoutCancel(l.ctx))
	l.log("tuya cloud: link stopped")
}

// flush adds this worker's counts to the month's totals and learns the totals back (the budget guard reads them).
func (l *link) flush(ctx context.Context) {
	add := l.usage.take()
	l.mu.Lock()
	last := l.lastEvent
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	total, e := l.w.Store.RecordCloudUsage(ctx, l.tenant, l.gateway, add, last, l.w.now())
	if e != nil {
		l.usage.restore(add)
		return
	}
	l.usage.settle(total)
}

// healthLoop flushes usage and writes a health report every Health: the silence scan measures these, so a worker
// that dies takes its devices offline after postgres.CloudSilentAfter.
func (l *link) healthLoop() {
	t := time.NewTicker(l.t.Health)
	defer t.Stop()
	for {
		l.flush(l.ctx)
		l.mu.Lock()
		retry := l.connected && l.state != domain.CloudLinkOnline
		l.mu.Unlock()
		if retry { // connected, but the online verdict was never recorded
			l.setState(domain.CloudLinkOnline, "", 0)
		}
		u := l.usage.snapshot()
		l.mu.Lock()
		h := postgres.CloudHealth{State: l.state, Connected: l.connected, Events: u.Events, APICalls: u.APICalls, Dropped: u.Dropped}
		l.mu.Unlock()
		if l.usage.guarded() {
			h.Budget = "guard"
		}
		ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
		if e := l.w.Store.RecordCloudHealth(ctx, l.tenant, l.gateway, h); e != nil && l.ctx.Err() == nil {
			l.warn("tuya cloud: health not recorded", "error", e.Error())
		}
		cancel()
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		}
	}
}
