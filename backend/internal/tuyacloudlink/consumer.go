package tuyacloudlink

import (
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/domain"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/gorilla/websocket"
)

// errStore is a transient database failure while storing a message: it was not acknowledged (Pulsar redelivers
// it), and the connection is re-established so the redelivery comes soon.
var errStore = errors.New("message not stored")

// consume keeps the link's Message Service subscription connected until the link stops or the credentials are
// refused. Each failure waits longer (backoff, 1 s to 5 min, jittered) before the next attempt.
func (l *link) consume() {
	failures, oversize := 0, 0
	limiter := newBucket(EventsPerSecond, EventsPerSecond, time.Now)
	seen := newSeenIDs(SeenMessages)
	for l.ctx.Err() == nil {
		conn, status, e := l.dial()
		if e == nil {
			started := time.Now()
			e = l.session(conn, limiter, seen)
			conn.Close()
			// A frame beyond DrainLimit closes the connection, and Pulsar delivers it again on the next one. After a
			// few in a row the link is marked degraded and waits the longest backoff instead of hot-looping.
			if errors.Is(e, errOversize) {
				oversize++
			} else {
				oversize = 0
			}
			// Only a connection that held for a while resets the backoff: one the server drops at once is a failure.
			if time.Since(started) > time.Minute {
				failures = 0
			}
		}
		if l.ctx.Err() != nil {
			return
		}
		l.mu.Lock()
		l.connected = false
		l.mu.Unlock()
		// A refused handshake ends the consumer for good: retrying with the same credentials cannot succeed.
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			l.warn("tuya cloud: message service refused the credentials; link parked until they are saved again")
			l.setState(domain.CloudLinkAuthFailed, "mq_auth", int64(status))
			l.park()
			return
		}
		reason := "mq_disconnected"
		if status != 0 {
			reason = "mq_http_" + http.StatusText(status)
		}
		wait := backoff(failures, l.t.BackoffMin, l.t.BackoffMax)
		if oversize >= OversizeCloses {
			reason, wait = "mq_oversize_frames", l.t.BackoffMax
			l.warn("tuya cloud: frames beyond the read limit keep closing the connection; backing off", "closes", oversize)
		}
		l.setState(domain.CloudLinkOffline, clip(reason, 64), int64(status))
		failures++
		l.log("tuya cloud: message service disconnected; reconnecting", "in", wait.String())
		select {
		case <-l.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// dial opens the WebSocket to the region's Message Service with the project's username and derived password.
// Proxy is nil (no environment proxy may see the password) and the certificate is always verified.
func (l *link) dial() (*websocket.Conn, int, error) {
	l.mu.Lock()
	region, channel, creds := l.region, l.channel, l.creds
	l.mu.Unlock()
	consumerURL := l.w.ConsumerURL
	if consumerURL == nil {
		consumerURL = tuyacloud.ConsumerURL
	}
	target, e := consumerURL(region, creds.AccessID, channel)
	if e != nil {
		return nil, 0, e
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if l.w.TLS != nil {
		cfg = l.w.TLS.Clone()
		cfg.InsecureSkipVerify = false
		if cfg.MinVersion < tls.VersionTLS12 {
			cfg.MinVersion = tls.VersionTLS12
		}
	}
	dialer := websocket.Dialer{Proxy: nil, TLSClientConfig: cfg, HandshakeTimeout: 15 * time.Second}
	header := http.Header{}
	header.Set("username", creds.AccessID)
	header.Set("password", tuyacloud.Password(creds.AccessID, creds.AccessSecret))
	ctx, cancel := context.WithTimeout(l.ctx, 20*time.Second)
	defer cancel()
	conn, resp, e := dialer.DialContext(ctx, target, header)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			resp.Body.Close()
		}
	}
	if e != nil {
		return nil, status, e
	}
	return conn, 0, nil
}

// session serves one connection: pings every Ping, drops the connection when nothing arrived for ReadTimeout,
// and handles each frame in order.
func (l *link) session(conn *websocket.Conn, limiter *bucket, seen *seenIDs) error {
	deadline := func() { _ = conn.SetReadDeadline(time.Now().Add(l.t.ReadTimeout)) }
	deadline()
	conn.SetPongHandler(func(string) error { deadline(); return nil })
	l.mu.Lock()
	l.connected = true
	l.mu.Unlock()
	l.setState(domain.CloudLinkOnline, "", 0)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer l.guard("ping")
		t := time.NewTicker(l.t.Ping)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-l.ctx.Done():
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				conn.Close()
				return
			case <-t.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	for {
		kind, r, e := conn.NextReader()
		if e != nil {
			return e
		}
		deadline()
		if kind != websocket.TextMessage && kind != websocket.BinaryMessage {
			continue
		}
		// The first 4 KiB are read on their own: they hold the message id (Pulsar writes it first), so an oversize
		// frame can be acknowledged before it is drained. Read at most MaxFrame+1 bytes in all; anything longer is
		// acknowledged, drained (up to DrainLimit) and dropped.
		head, e := io.ReadAll(io.LimitReader(r, idWindow))
		if e != nil {
			return e
		}
		rest, e := io.ReadAll(io.LimitReader(r, tuyacloud.MaxFrame+1-int64(len(head))))
		if e != nil {
			return e
		}
		b := append(head, rest...)
		if len(b) > tuyacloud.MaxFrame {
			l.usage.add(1, 0, 1) // Tuya bills it all the same
			if id := leadingID(head); id != "" {
				if e := conn.WriteMessage(websocket.TextMessage, tuyacloud.Ack(id)); e != nil {
					return e
				}
			}
			// Drained up to DrainLimit; a frame larger still ends the connection (it was acknowledged above when
			// its id could be read, so Pulsar does not deliver it again).
			n, e := io.Copy(io.Discard, io.LimitReader(r, DrainLimit-int64(len(b))+1))
			if e != nil {
				return e
			}
			if int64(len(b))+n > DrainLimit {
				return errOversize
			}
			continue
		}
		if e := limiter.wait(l.ctx); e != nil {
			return e
		}
		reply, e := l.handle(b, seen)
		if reply != nil {
			if e := conn.WriteMessage(websocket.TextMessage, reply); e != nil {
				return e
			}
		}
		if e != nil {
			return e
		}
	}
}

// errOversize ends a connection whose frame is larger than DrainLimit. The limit is enforced here rather than as
// gorilla's read limit, which refuses such a frame from its header before its id could be read.
var errOversize = errors.New("frame beyond the drain limit")

// idWindow is how much of a frame is read to find its message id.
const idWindow = 4 << 10

// OversizeCloses is how many connections in a row may end on a frame beyond the read limit before the link is
// marked degraded (reason mq_oversize_frames) and waits the longest backoff.
const OversizeCloses = 3

// leadingIDPattern finds the message id at the start of an oversize frame (Pulsar writes messageId first).
var leadingIDPattern = regexp.MustCompile(`^\s*\{\s*"messageId"\s*:\s*"([^"\\]{1,256})"`)

func leadingID(b []byte) string {
	m := leadingIDPattern.FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// handle decides one frame's fate and returns the message to send back (an ack or a nack, nil when the frame has
// no readable id). Malformed, undecryptable, redelivered-too-often and unknown messages are acknowledged and
// dropped: redelivering them could never succeed. A transient database failure is negatively acknowledged, so
// Pulsar redelivers the message, and ends the connection with errStore.
func (l *link) handle(b []byte, seen *seenIDs) ([]byte, error) {
	// Every message Tuya delivered counts against the month's budget, dropped or not.
	f, e := tuyacloud.ParseFrame(b)
	if e != nil {
		l.usage.add(1, 0, 1)
		if f.MessageID != "" {
			return tuyacloud.Ack(f.MessageID), nil
		}
		return nil, nil
	}
	ack := tuyacloud.Ack(f.MessageID)
	if seen.has(f.MessageID) {
		return ack, nil // stored already; the ack was lost
	}
	if f.RedeliveryCount > MaxRedelivery {
		l.usage.add(1, 0, 1)
		l.warn("tuya cloud: message redelivered too often; dropped", "redelivery", f.RedeliveryCount)
		return ack, nil
	}
	env, e := tuyacloud.ParseEnvelope(f.Payload)
	if e != nil {
		l.usage.add(1, 0, 1)
		return ack, nil
	}
	l.mu.Lock()
	secret := l.creds.AccessSecret
	l.mu.Unlock()
	plain, e := tuyacloud.Decrypt(env.Data, secret, f.Properties["em"])
	if e != nil {
		l.usage.add(1, 0, 1)
		return ack, nil
	}
	events, e := tuyacloud.Parse(env.Protocol, plain)
	if errors.Is(e, tuyacloud.ErrUnknownProtocol) || (e == nil && len(events) == 0) {
		l.usage.add(1, 0, 0) // a message Tuya counts, of a kind Aether does not use
		seen.add(f.MessageID)
		return ack, nil
	}
	if e != nil {
		l.usage.add(1, 0, 1)
		return ack, nil
	}
	ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
	defer cancel()
	res, e := l.w.Store.CaptureTuyaCloud(ctx, l.tenant, l.gateway, env.Protocol, events, l.usage.guarded())
	if e != nil {
		if errors.Is(e, domain.ErrInvalid) || errors.Is(e, domain.ErrForbidden) || errors.Is(e, domain.ErrUnauthorized) {
			l.usage.add(1, 0, 1) // permanent: the gateway is gone or the message does not fit
			return ack, nil
		}
		if l.ctx.Err() != nil {
			return nil, l.ctx.Err()
		}
		l.warn("tuya cloud: message not stored; asking for redelivery", "error", e.Error())
		return tuyacloud.Nack(f.MessageID), errStore
	}
	seen.add(f.MessageID)
	now := l.w.now().UTC()
	l.mu.Lock()
	l.lastEvent = &now
	l.state = domain.CloudLinkOnline // any stored message proves the link (CaptureTuyaCloud recorded it)
	l.mu.Unlock()
	l.usage.add(1, 0, int64(res.Dropped))
	if res.SyncRequested {
		l.poke()
	}
	return ack, nil
}
