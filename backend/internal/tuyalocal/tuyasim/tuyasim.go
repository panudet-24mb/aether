// Package tuyasim is a fake Tuya Wi‑Fi device for tests: it speaks the local protocol (3.1, 3.3, 3.4, 3.5)
// on a loopback TCP port using the same framing code as the client, keeps DP state, answers queries and
// commands, pushes status changes and can broadcast discovery packets. Failure modes cover a wrong local key,
// the single-connection limit, the device22 quirk and a device that stops answering.
package tuyasim

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"maps"
	"net"
	"strconv"
	"sync"
	"time"

	"aether/backend/internal/tuyalocal"
)

// Device is one simulated device. Set the exported fields before Start.
type Device struct {
	ID      string
	Key     string
	Version tuyalocal.Version
	// RejectKey makes the device hold a different local key: on 3.4/3.5 its negotiation answer carries an
	// HMAC the client cannot verify, on 3.1–3.3 its replies do not decrypt with the client's key.
	RejectKey bool
	// SingleConnection resets any connection made while another is open, like real devices.
	SingleConnection bool
	// Device22 answers DP_QUERY with "data unvalid" and serves DPs only through CONTROL_NEW queries.
	Device22 bool
	// Mute stops all answers after the first full status (to test lost heartbeats).
	Mute bool

	mu     sync.Mutex
	dps    map[string]any
	ln     net.Listener
	conns  map[*conn]struct{}
	seq35  uint32 // 3.5 devices answer with their own global counter, not the request's sequence number
	closed bool
	wg     sync.WaitGroup
}

// New creates a device with the given DPs.
func New(id, key string, v tuyalocal.Version, dps map[string]any) *Device {
	return &Device{ID: id, Key: key, Version: v, dps: maps.Clone(dps), conns: map[*conn]struct{}{}, seq35: 100}
}

// Start listens on a loopback port and returns its address.
func (d *Device) Start() (string, error) {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return "", e
	}
	d.mu.Lock()
	d.ln = ln
	d.mu.Unlock()
	d.wg.Add(1)
	go d.accept(ln)
	return ln.Addr().String(), nil
}

// Close stops the listener and drops every connection.
func (d *Device) Close() {
	d.mu.Lock()
	d.closed = true
	if d.ln != nil {
		d.ln.Close()
	}
	for c := range d.conns {
		c.nc.Close()
	}
	d.mu.Unlock()
	d.wg.Wait()
}

// DPS returns a copy of the current DP values.
func (d *Device) DPS() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.dps)
}

// Connections is the number of open client connections.
func (d *Device) Connections() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

// SetLocal changes DPs as if someone pressed the device, and pushes them to every connection.
func (d *Device) SetLocal(changes map[string]any) {
	d.mu.Lock()
	maps.Copy(d.dps, changes)
	conns := make([]*conn, 0, len(d.conns))
	for c := range d.conns {
		conns = append(conns, c)
	}
	d.mu.Unlock()
	for _, c := range conns {
		c.push(changes)
	}
}

func (d *Device) accept(ln net.Listener) {
	defer d.wg.Done()
	for {
		nc, e := ln.Accept()
		if e != nil {
			return
		}
		d.mu.Lock()
		busy := d.SingleConnection && len(d.conns) > 0
		if d.closed || busy {
			d.mu.Unlock()
			if tc, ok := nc.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // a reset, like a device refusing a second client
			}
			nc.Close()
			continue
		}
		key := d.Key
		if d.RejectKey {
			key = "wrongwrongwrong!"
		}
		s, e := tuyalocal.NewSession(d.Version, d.ID, key, tuyalocal.Device)
		if e != nil {
			d.mu.Unlock()
			nc.Close()
			continue
		}
		c := &conn{d: d, nc: nc, sess: s, fr: tuyalocal.NewFrameReader(nc)}
		d.conns[c] = struct{}{}
		d.wg.Add(1)
		d.mu.Unlock()
		go c.serve()
	}
}

func (d *Device) nextSeq35() uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq35++
	return d.seq35
}

type conn struct {
	d    *Device
	nc   net.Conn
	fr   *tuyalocal.FrameReader
	mu   sync.Mutex // guards sess and writes
	sess *tuyalocal.Session
	// muted stops every answer (Device.Mute, after the first full status).
	muted bool
}

func (c *conn) serve() {
	defer c.d.wg.Done()
	defer func() {
		c.d.mu.Lock()
		delete(c.d.conns, c)
		c.d.mu.Unlock()
		c.nc.Close()
	}()
	// The device reads the client's frames with the real key; RejectKey only changes how it answers.
	reader, e := tuyalocal.NewSession(c.d.Version, c.d.ID, c.d.Key, tuyalocal.Device)
	if e != nil {
		return
	}
	var localNonce, remoteNonce []byte
	for {
		raw, e := c.fr.Next()
		if e != nil {
			return
		}
		f, _, e := reader.Unpack(raw)
		if e != nil {
			continue
		}
		payload, e := reader.Decode(f)
		if e != nil {
			continue
		}
		switch f.Cmd {
		case tuyalocal.CmdSessKeyNegStart:
			localNonce = payload
			remoteNonce = make([]byte, 16)
			_, _ = rand.Read(remoteNonce)
			// Framed with the device's key; the HMAC inside proves (or disproves) it matches the client's.
			c.mu.Lock()
			resp := tuyalocal.NegotiationResponse(c.keyBytes(), localNonce, remoteNonce)
			c.mu.Unlock()
			c.send(tuyalocal.CmdSessKeyNegResp, resp)
		case tuyalocal.CmdSessKeyNegFinish:
			if len(localNonce) != 16 || !tuyalocal.VerifyFinish([]byte(c.d.Key), remoteNonce, payload) {
				return
			}
			k, e := tuyalocal.DeriveSessionKey(c.d.Version, []byte(c.d.Key), localNonce, remoteNonce)
			if e != nil {
				return
			}
			reader.SetSessionKey(k)
			c.mu.Lock()
			c.sess.SetSessionKey(k)
			c.mu.Unlock()
		case tuyalocal.CmdDPQuery, tuyalocal.CmdDPQueryNew:
			if c.d.Device22 && f.Cmd == tuyalocal.CmdDPQuery {
				c.send(tuyalocal.CmdDPQuery, []byte("json obj data unvalid"))
				continue
			}
			c.send(f.Cmd, c.statusJSON(c.d.DPS(), true))
			c.muteIfNeeded()
		case tuyalocal.CmdControl, tuyalocal.CmdControlNew:
			req := parseDPS(payload)
			if c.d.Device22 && f.Cmd == tuyalocal.CmdControlNew && allNil(req) {
				// device22 query: answer the named DPs.
				all := c.d.DPS()
				out := map[string]any{}
				for k := range req {
					if v, ok := all[k]; ok {
						out[k] = v
					}
				}
				c.send(tuyalocal.CmdControlNew, c.statusJSON(out, true))
				c.muteIfNeeded()
				continue
			}
			c.send(f.Cmd, nil) // acknowledgement
			changed := map[string]any{}
			for k, v := range req {
				if v != nil {
					changed[k] = v
				}
			}
			c.d.SetLocal(changed)
		case tuyalocal.CmdHeartBeat:
			c.send(tuyalocal.CmdHeartBeat, nil)
		case tuyalocal.CmdUpdateDPS:
			c.send(tuyalocal.CmdUpdateDPS, nil)
			var q struct {
				DpID []int `json:"dpId"`
			}
			_ = json.Unmarshal(payload, &q)
			all := c.d.DPS()
			out := map[string]any{}
			for _, id := range q.DpID {
				if v, ok := all[strconv.Itoa(id)]; ok {
					out[strconv.Itoa(id)] = v
				}
			}
			if len(out) > 0 {
				c.push(out)
			}
		}
	}
}

func (c *conn) keyBytes() []byte {
	if c.d.RejectKey {
		return []byte("wrongwrongwrong!")
	}
	return []byte(c.d.Key)
}

func (c *conn) muteIfNeeded() {
	if c.d.Mute {
		c.mu.Lock()
		c.muted = true
		c.mu.Unlock()
	}
}

// statusJSON is the payload of a query answer or push in this version's shape: {"devId":…,"dps":…} on
// 3.1–3.3 and {"protocol":4,"t":…,"data":{"dps":…}} on 3.4/3.5 (the answer to DP_QUERY_NEW is {"dps":…}).
func (c *conn) statusJSON(dps map[string]any, query bool) []byte {
	var v any
	switch {
	case c.d.Version >= tuyalocal.V34 && query:
		v = map[string]any{"dps": dps, "t": time.Now().Unix()}
	case c.d.Version >= tuyalocal.V34:
		v = map[string]any{"protocol": 4, "t": time.Now().Unix(), "data": map[string]any{"dps": dps}}
	default:
		v = map[string]any{"devId": c.d.ID, "dps": dps, "t": time.Now().Unix()}
	}
	b, _ := json.Marshal(v)
	return b
}

func (c *conn) push(dps map[string]any) {
	c.send(tuyalocal.CmdStatus, c.statusJSON(dps, false))
}

func (c *conn) send(cmd uint32, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.muted {
		return
	}
	if c.d.Version == tuyalocal.V35 {
		c.sess.SetSeq(c.d.nextSeq35())
	}
	frame, e := c.sess.Encode(cmd, payload)
	if e != nil {
		return
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.nc.Write(frame)
}

func parseDPS(payload []byte) map[string]any {
	dps, _, _ := tuyalocal.ParseDPS(payload)
	if dps == nil {
		return map[string]any{}
	}
	for k, v := range dps {
		if n, ok := v.(json.Number); ok {
			if i, e := n.Int64(); e == nil {
				dps[k] = i
			} else if f, e := n.Float64(); e == nil {
				dps[k] = f
			}
		}
	}
	return dps
}

func allNil(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	for _, v := range m {
		if v != nil {
			return false
		}
	}
	return true
}

// Broadcast sends one discovery packet in the device's format to addr (e.g. "127.0.0.1:6667").
func (d *Device) Broadcast(addr, ip string) error {
	body := map[string]any{"ip": ip, "gwId": d.ID, "active": 2, "ability": 0, "mode": 0, "encrypt": d.Version != tuyalocal.V31,
		"productKey": "keysimulator0001", "version": d.Version.String()}
	pkt, e := tuyalocal.EncodeBroadcast(d.Version, body, nil)
	if e != nil {
		return e
	}
	uc, e := net.Dial("udp4", addr)
	if e != nil {
		return e
	}
	defer uc.Close()
	_, e = uc.Write(pkt)
	return e
}

// BroadcastEvery sends a discovery packet every interval until ctx ends.
func (d *Device) BroadcastEvery(ctx context.Context, addr, ip string, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if e := d.Broadcast(addr, ip); e != nil {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
