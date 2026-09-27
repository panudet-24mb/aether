package edge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"aether/backend/internal/tuyalocal"
)

// worker keeps one registered device connected: dial, publish its state, reconnect with backoff, and report its
// availability. Availability is debounced: a device is announced offline only after OfflineAfter of failed
// reconnects (or at once when three heartbeats were missed on an open connection, or when the device proved the
// key wrong).
type worker struct {
	a   *Agent
	dev Device

	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.Mutex
	conn      *tuyalocal.Conn
	announced string // last availability published: "online" or "offline:<reason>"
	co        *coalescer
}

func (a *Agent) startWorker(parent context.Context, d Device) *worker {
	ctx, cancel := context.WithCancel(parent)
	w := &worker{a: a, dev: d, cancel: cancel, done: make(chan struct{}), co: newCoalescer(a.timing.Coalesce)}
	go w.co.run(ctx, func(dps map[string]any, full bool) {
		if e := a.bus.Publish(topicState(a.cfg.GatewayID, d.ID), false, encodeState(dps, full)); e != nil {
			a.log.Warn("state not published", "device", d.ID, "error", e.Error())
		}
	})
	go w.run(ctx)
	return w
}

func (w *worker) stop() {
	w.cancel()
	<-w.done
}

func (w *worker) connected() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn != nil
}

func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	t := w.a.timing
	backoff := t.BackoffMin
	var failingSince time.Time
	for ctx.Err() == nil {
		conn, err := w.dial(ctx)
		if err == nil {
			w.setConn(conn)
			w.announce(true, "")
			failingSince, backoff = time.Time{}, t.BackoffMin
			select {
			case <-conn.Done():
				err = conn.Err()
			case <-ctx.Done():
				conn.Close()
				w.setConn(nil)
				return
			}
			w.setConn(nil)
			if errors.Is(err, tuyalocal.ErrHeartbeat) {
				w.announce(false, ReasonUnreachable) // three missed heartbeats on an open connection
			}
		}
		if ctx.Err() != nil {
			return
		}
		reason := reasonFor(err)
		if failingSince.IsZero() {
			failingSince = w.a.now()
		}
		// A wrong key is proven, not guessed: say so at once so the user can re-import.
		if reason == ReasonAuthFailed || w.a.now().Sub(failingSince) >= t.OfflineAfter {
			w.announce(false, reason)
		}
		if !sleep(ctx, backoff) {
			return
		}
		if backoff *= 2; backoff > t.BackoffMax {
			backoff = t.BackoffMax
		}
	}
}

// errNoAddress: the device has no configured IP and was not seen broadcasting on the LAN.
var errNoAddress = errors.New("edge: device address unknown")

func (w *worker) dial(ctx context.Context) (*tuyalocal.Conn, error) {
	addr, version := w.a.address(w.dev)
	if addr == "" {
		return nil, errNoAddress
	}
	v, e := tuyalocal.ParseVersion(version)
	if e != nil {
		v = tuyalocal.V33
	}
	o := tuyalocal.Options{
		Address: addr, DeviceID: w.dev.ID, LocalKey: w.dev.Key, Version: v, Device22: w.dev.Device22,
		HeartbeatInterval: w.a.timing.Heartbeat,
		OnStatus:          func(s tuyalocal.Status) { w.co.add(s.DPS, s.Full) },
	}
	if len(w.dev.RefreshDPs) > 0 {
		o.RefreshDPS, o.RefreshInterval = w.dev.RefreshDPs, time.Minute
	}
	// ctx outlives the dial: tuyalocal keeps using it for the connection's whole life (cancelling it closes the
	// socket). The dial and first answer are bounded by the connection's own timeouts instead.
	o.DialTimeout, o.ReplyTimeout = w.a.timing.DialTimeout, w.a.timing.DialTimeout
	return tuyalocal.Dial(ctx, o)
}

func (w *worker) setConn(c *tuyalocal.Conn) {
	w.mu.Lock()
	w.conn = c
	w.mu.Unlock()
}

// set sends a command to the device. Confirmation is the device's own status push, which the server matches.
func (w *worker) set(dps map[string]any) error {
	w.mu.Lock()
	c := w.conn
	w.mu.Unlock()
	if c == nil {
		return errors.New("edge: device not connected")
	}
	return c.Set(dps)
}

func (w *worker) announce(online bool, reason string) {
	key := "online"
	if !online {
		key = "offline:" + reason
	}
	w.mu.Lock()
	same := w.announced == key
	w.announced = key
	w.mu.Unlock()
	if same {
		return
	}
	if e := w.a.bus.Publish(topicAvailability(w.a.cfg.GatewayID, w.dev.ID), true, encodeAvailability(online, reason)); e != nil {
		w.a.log.Warn("availability not published", "device", w.dev.ID, "error", e.Error())
		w.mu.Lock()
		w.announced = "" // try again on the next change
		w.mu.Unlock()
		return
	}
	if online {
		w.a.log.Info("device online", "device", w.dev.ID)
	} else {
		w.a.log.Info("device offline", "device", w.dev.ID, "reason", reason)
	}
}

// reasonFor maps a connection error to the availability reason the server shows.
func reasonFor(e error) string {
	switch {
	case errors.Is(e, tuyalocal.ErrKeyRejected):
		return ReasonAuthFailed
	case errors.Is(e, tuyalocal.ErrKeySuspect):
		return ReasonKeySuspect
	case errors.Is(e, tuyalocal.ErrBusy):
		return ReasonBusy
	case errors.Is(e, errNoAddress):
		return ReasonNotFound
	}
	return ReasonUnreachable
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

// coalescer bounds how often one device's state is published: a full status, or any change of a boolean or
// enum/string data point, goes out at once; numbers only (power, voltage, energy that some plugs push every second)
// at most once per period, the latest values winning.
type coalescer struct {
	period time.Duration

	mu      sync.Mutex
	pending map[string]any
	full    bool
	urgent  bool
	last    time.Time
	kick    chan struct{}
}

func newCoalescer(period time.Duration) *coalescer {
	return &coalescer{period: period, pending: map[string]any{}, kick: make(chan struct{}, 1)}
}

func (c *coalescer) add(dps map[string]any, full bool) {
	c.mu.Lock()
	if full {
		c.pending = map[string]any{}
		c.full, c.urgent = true, true
	}
	for k, v := range dps {
		if _, number := v.(json.Number); !number {
			if _, f := v.(float64); !f {
				c.urgent = true
			}
		}
		c.pending[k] = v
	}
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *coalescer) run(ctx context.Context, publish func(map[string]any, bool)) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.kick:
		case <-timer.C:
		}
		c.mu.Lock()
		if len(c.pending) == 0 && !c.full {
			c.mu.Unlock()
			continue
		}
		wait := c.period - time.Since(c.last)
		if !c.urgent && wait > 0 {
			c.mu.Unlock()
			timer.Reset(wait)
			continue
		}
		dps, full := c.pending, c.full
		c.pending, c.full, c.urgent, c.last = map[string]any{}, false, false, time.Now()
		c.mu.Unlock()
		publish(dps, full)
	}
}
