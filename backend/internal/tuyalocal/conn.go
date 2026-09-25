package tuyalocal

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

// TCPPort is the device's local control port (const.py TCPPORT).
const TCPPort = 6668

// Connection errors. Edge maps them to the availability reasons it reports.
var (
	// ErrBusy: the device closed the socket before answering, which is what it does when another local
	// client (the Tuya app on the LAN, Home Assistant/LocalTuya) already holds its single TCP connection
	// ("Tuya devices only allow one TCP connection at a time", tinytuya README).
	ErrBusy = errors.New("tuyalocal: device closed the connection (another local client may be connected)")
	// ErrTimeout: no answer within the reply timeout.
	ErrTimeout = errors.New("tuyalocal: device did not answer in time")
	// ErrHeartbeat: the device stopped answering heartbeats on an established connection.
	ErrHeartbeat = errors.New("tuyalocal: heartbeat lost")
	// ErrClosed: Close was called.
	ErrClosed = errors.New("tuyalocal: connection closed")
)

// Status is one DP report from the device.
type Status struct {
	// DPS maps DP ids to values (numbers are json.Number).
	DPS map[string]any
	// Full is true for the answer to a DP query (every DP), false for a push of changed DPs.
	Full bool
	// Cmd is the frame's command code.
	Cmd uint32
}

// Options configure a Conn.
type Options struct {
	// Address is the device IP or host:port (port defaults to 6668).
	Address  string
	DeviceID string
	LocalKey string
	Version  Version
	// Device22 starts in device22 mode; it is also switched on automatically when the device asks for it.
	Device22     bool
	DPsToRequest []string
	// HeartbeatInterval defaults to 10 s (tinytuya's monitor uses 12 s). The connection is dropped when
	// nothing arrives for three intervals.
	HeartbeatInterval time.Duration
	// DialTimeout and ReplyTimeout default to 5 s.
	DialTimeout  time.Duration
	ReplyTimeout time.Duration
	// RefreshDPS are asked for with UPDATEDPS every RefreshInterval (0 disables it), for DPs such as
	// metering values that devices only push on request.
	RefreshDPS      []int
	RefreshInterval time.Duration
	// OnStatus receives every DP report, from the connection's reader goroutine. It must not block.
	OnStatus func(Status)
	// Nonce, Now and IV are injectable for tests; defaults are crypto/rand and time.Now.
	Nonce func() []byte
	Now   func() time.Time
	IV    func() []byte
}

// Conn is a persistent client connection to one device.
type Conn struct {
	opts Options
	nc   net.Conn
	fr   *FrameReader

	mu   sync.Mutex // guards sess and writes
	sess *Session

	done    chan struct{}
	errOnce sync.Once
	err     error

	lastRx   chanTime
	queryAck chan error // the answer (or failure) of an outstanding DP query
}

type chanTime struct {
	mu sync.Mutex
	t  time.Time
}

func (c *chanTime) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *chanTime) get() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Dial connects, negotiates a session key on 3.4/3.5, queries every DP and returns once the first full
// status has been delivered to OnStatus. The returned Conn keeps the socket open with heartbeats until
// Close, ctx cancellation or an error; Done and Err report the end.
func Dial(ctx context.Context, o Options) (*Conn, error) {
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 10 * time.Second
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.ReplyTimeout <= 0 {
		o.ReplyTimeout = 5 * time.Second
	}
	if o.Nonce == nil {
		o.Nonce = func() []byte { n := make([]byte, nonceLen); _, _ = rand.Read(n); return n }
	}
	if o.OnStatus == nil {
		o.OnStatus = func(Status) {}
	}
	addr := o.Address
	if _, _, e := net.SplitHostPort(addr); e != nil {
		addr = net.JoinHostPort(addr, fmt.Sprint(TCPPort))
	}
	sess, e := NewSession(o.Version, o.DeviceID, o.LocalKey, Client)
	if e != nil {
		return nil, e
	}
	sess.Device22 = sess.Device22 || o.Device22
	sess.DPsToRequest = o.DPsToRequest
	if o.Now != nil {
		sess.Now = o.Now
	}
	if o.IV != nil {
		sess.IV = o.IV
	}
	d := net.Dialer{Timeout: o.DialTimeout}
	nc, e := d.DialContext(ctx, "tcp", addr)
	if e != nil {
		return nil, e
	}
	c := &Conn{opts: o, nc: nc, fr: NewFrameReader(nc), sess: sess, done: make(chan struct{}), queryAck: make(chan error, 1)}
	c.lastRx.set(time.Now())
	if o.Version >= V34 {
		if e := c.negotiate(); e != nil {
			nc.Close()
			return nil, e
		}
	}
	go c.readLoop()
	if e := c.queryAndWait(ctx); e != nil {
		c.fail(e)
		return nil, e
	}
	go c.keepAlive(ctx)
	return c, nil
}

// negotiate runs the 3.4/3.5 session-key exchange synchronously, before the reader starts.
func (c *Conn) negotiate() error {
	n := &negotiation{version: c.opts.Version, localKey: []byte(c.opts.LocalKey), localNonce: c.opts.Nonce()}
	if len(n.localNonce) != nonceLen {
		return errors.New("tuyalocal: nonce must be 16 bytes")
	}
	if e := c.write(CmdSessKeyNegStart, n.localNonce); e != nil {
		return classifyWrite(e, true)
	}
	_ = c.nc.SetReadDeadline(time.Now().Add(c.opts.ReplyTimeout))
	defer c.nc.SetReadDeadline(time.Time{})
	for {
		raw, e := c.fr.Next()
		if e != nil {
			return classifyRead(e, true)
		}
		c.mu.Lock()
		f, _, e := c.sess.Unpack(raw)
		c.mu.Unlock()
		if errors.Is(e, ErrChecksum) || errors.Is(e, ErrAuth) {
			// The device framed its answer with a different key.
			return ErrKeyRejected
		}
		if e != nil {
			return e
		}
		if f.Cmd != CmdSessKeyNegResp {
			continue
		}
		c.mu.Lock()
		resp, e := c.sess.Decode(f)
		c.mu.Unlock()
		if e != nil {
			return ErrKeyRejected
		}
		finish, e := n.finishPayload(resp)
		if e != nil {
			return e
		}
		if e := c.write(CmdSessKeyNegFinish, finish); e != nil {
			return classifyWrite(e, true)
		}
		key, e := n.sessionKey()
		if e != nil {
			return e
		}
		c.mu.Lock()
		c.sess.SetSessionKey(key)
		c.mu.Unlock()
		c.lastRx.set(time.Now())
		return nil
	}
}

// classifyWrite does the same for a write: a device that resets a second client can do so before our
// first frame is even sent (broken pipe, reset, or a socket the reader already closed).
func classifyWrite(e error, beforeFirstFrame bool) error {
	var ne net.Error
	switch {
	case errors.As(e, &ne) && ne.Timeout():
		return ErrTimeout
	case beforeFirstFrame && (errors.Is(e, syscall.EPIPE) || errors.Is(e, syscall.ECONNRESET) || errors.Is(e, net.ErrClosed)):
		return ErrBusy
	}
	return e
}

// classifyRead turns a socket read error into a connection error. Before the device has sent anything, a
// close or reset means another client holds the device's only socket.
func classifyRead(e error, beforeFirstFrame bool) error {
	var ne net.Error
	switch {
	case errors.As(e, &ne) && ne.Timeout():
		return ErrTimeout
	case beforeFirstFrame && (errors.Is(e, io.EOF) || errors.Is(e, io.ErrUnexpectedEOF) || errors.Is(e, syscall.ECONNRESET)):
		return ErrBusy
	}
	return e
}

func (c *Conn) write(cmd uint32, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	frame, e := c.sess.Encode(cmd, payload)
	if e != nil {
		return e
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(c.opts.ReplyTimeout))
	_, e = c.nc.Write(frame)
	return e
}

// queryAndWait sends a DP query and waits for the full status (or a device22 retry).
func (c *Conn) queryAndWait(ctx context.Context) error {
	for attempt := 0; attempt < 2; attempt++ {
		if e := c.Query(); e != nil {
			if ce := c.Err(); ce != nil {
				return ce // the reader already saw why the socket died
			}
			return classifyWrite(e, attempt == 0)
		}
		timer := time.NewTimer(c.opts.ReplyTimeout)
		select {
		case e := <-c.queryAck:
			timer.Stop()
			if errors.Is(e, ErrDevice22) {
				continue // switched to device22 mode: ask again in the new form
			}
			return e
		case <-timer.C:
			return ErrTimeout
		case <-c.done:
			timer.Stop()
			return c.Err()
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return ErrTimeout
}

// Query asks for every DP; the answer arrives through OnStatus with Full set.
func (c *Conn) Query() error {
	c.mu.Lock()
	cmd, p := c.sess.DPQuery()
	c.mu.Unlock()
	return c.write(cmd, p)
}

// Set changes DPs (keys are DP ids). The device confirms by pushing the new values through OnStatus; Set
// itself only reports whether the command was written.
func (c *Conn) Set(dps map[string]any) error {
	if len(dps) == 0 {
		return errors.New("tuyalocal: nothing to set")
	}
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	c.mu.Lock()
	cmd, p := c.sess.Control(dps)
	c.mu.Unlock()
	return c.write(cmd, p)
}

// Refresh asks the device to push the given DPs (UPDATEDPS).
func (c *Conn) Refresh(ids []int) error {
	c.mu.Lock()
	cmd, p := c.sess.UpdateDPS(ids)
	c.mu.Unlock()
	return c.write(cmd, p)
}

// Device22 reports whether the connection switched to the device22 query form.
func (c *Conn) Device22() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.Device22
}

func (c *Conn) readLoop() {
	decoded := false
	for {
		raw, e := c.fr.Next()
		if e != nil {
			c.fail(classifyRead(e, !decoded))
			return
		}
		c.lastRx.set(time.Now())
		c.mu.Lock()
		f, _, e := c.sess.Unpack(raw)
		var payload []byte
		if e == nil {
			payload, e = c.sess.Decode(f)
			if errors.Is(e, ErrDevice22) {
				c.sess.Device22 = true
			}
		}
		c.mu.Unlock()
		if errors.Is(e, ErrDevice22) {
			c.ack(ErrDevice22)
			continue
		}
		if e != nil {
			if !decoded && (errors.Is(e, ErrKeySuspect) || errors.Is(e, ErrChecksum) || errors.Is(e, ErrAuth)) {
				// Nothing has decoded yet: on 3.1–3.3 this is the only sign of a wrong key.
				c.ack(ErrKeySuspect)
				c.fail(ErrKeySuspect)
				return
			}
			continue // one bad frame on a working connection: skip it
		}
		dps, ok, e := ParseDPS(payload)
		if e != nil && !decoded {
			// Decrypted "successfully" into non-JSON: a wrong key whose padding happened to look valid.
			c.ack(ErrKeySuspect)
			c.fail(ErrKeySuspect)
			return
		}
		if e != nil {
			continue
		}
		decoded = true
		if !ok {
			continue
		}
		full := f.Cmd == CmdDPQuery || f.Cmd == CmdDPQueryNew || (c.Device22() && f.Cmd == CmdControlNew)
		c.opts.OnStatus(Status{DPS: dps, Full: full, Cmd: f.Cmd})
		if full {
			c.ack(nil)
		}
	}
}

// ack reports the outcome of the outstanding DP query without blocking.
func (c *Conn) ack(e error) {
	select {
	case c.queryAck <- e:
	default:
	}
}

func (c *Conn) keepAlive(ctx context.Context) {
	hb := time.NewTicker(c.opts.HeartbeatInterval)
	defer hb.Stop()
	var refresh <-chan time.Time
	if c.opts.RefreshInterval > 0 {
		t := time.NewTicker(c.opts.RefreshInterval)
		defer t.Stop()
		refresh = t.C
	}
	for {
		select {
		case <-ctx.Done():
			c.fail(ctx.Err())
			return
		case <-c.done:
			return
		case <-hb.C:
			if time.Since(c.lastRx.get()) > 3*c.opts.HeartbeatInterval {
				c.fail(ErrHeartbeat)
				return
			}
			c.mu.Lock()
			cmd, p := c.sess.Heartbeat()
			c.mu.Unlock()
			if e := c.write(cmd, p); e != nil {
				c.fail(e)
				return
			}
		case <-refresh:
			if e := c.Refresh(c.opts.RefreshDPS); e != nil {
				c.fail(e)
				return
			}
		}
	}
}

func (c *Conn) fail(e error) {
	c.errOnce.Do(func() {
		c.err = e
		close(c.done)
		c.nc.Close()
	})
}

// Close ends the connection.
func (c *Conn) Close() error {
	c.fail(ErrClosed)
	return nil
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended (nil while it is open).
func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}
