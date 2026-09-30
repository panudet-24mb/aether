// Package tuyablesim is a fake Tuya BLE device for tests. It works at the GATT-message level: Connect returns a
// tuyable.Link whose writes the device decrypts and answers with notifications, using the same framing code as
// the client. It keeps DP state, answers the handshake, status requests and DP writes, pushes reports and clock
// requests, and generates its advertisement. Failure modes cover a wrong key, a refused pairing, a device that
// never answers, the single-central limit, small notifications and a lost fragment.
package tuyablesim

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"maps"
	"slices"
	"sync"

	"aether/backend/internal/tuyable"
)

// Device is one simulated device. Set the exported fields before the first Connect.
type Device struct {
	LocalKey  string
	SecKey    string
	UUID      string
	DeviceID  string
	ProductID string
	// Protocol is the major protocol version the device reports: 2, 3 (the default) or 4. A sec_key device is
	// Protocol 4 with SecKey set.
	Protocol byte
	// Srand is the 6-byte device random; random when nil.
	Srand []byte
	// DPs is the initial DP state.
	DPs []tuyable.DP

	// RejectKey makes the device hold a different local key: it still answers the device-info request, but with
	// a key the client cannot decrypt.
	RejectKey bool
	// PairResult is the pair answer (0 accepts). AlreadyBound answers 2, which clients treat as success.
	PairResult   byte
	AlreadyBound bool
	// Mute makes the device accept the connection and never answer.
	Mute bool
	// AskTime makes the device send TIME1 and TIME2 requests right after pairing.
	AskTime bool
	// NotifyMTU splits notifications into packets of this many bytes (20 when zero).
	NotifyMTU int
	// DropFragment drops fragment n (counted from 1) of the next outgoing message, once. Use SetDropFragment
	// once the device is connected.
	DropFragment int
	// AfterPair are extra session-key frames the device sends right after its pair answer (for example a code a
	// newer firmware might push). Seq is filled in.
	AfterPair []tuyable.Frame
	// Glitch sends a stray continuation packet before the device-info answer, as a lost or garbled notification
	// would look.
	Glitch bool
	// SingleCentral refuses a second connection while one is open, like real devices.
	SingleCentral bool

	mu        sync.Mutex
	started   bool
	connected *link
	state     map[byte]tuyable.DP
	seq       uint32
	dpSeq     uint32
	paired    bool
	acks      []tuyable.Frame
	times     map[tuyable.Code][]byte
	writes    []tuyable.Frame
	authKey   []byte
}

func (d *Device) init() {
	if d.started {
		return
	}
	d.started = true
	if d.Protocol == 0 {
		d.Protocol = 3
	}
	if d.Srand == nil {
		d.Srand = make([]byte, 6)
		rand.Read(d.Srand)
	}
	d.authKey = make([]byte, 32)
	rand.Read(d.authKey)
	d.state = map[byte]tuyable.DP{}
	for _, dp := range d.DPs {
		d.state[dp.ID] = dp
	}
	d.seq = 1
	d.times = map[tuyable.Code][]byte{}
}

func (d *Device) keys() tuyable.Keys {
	k := tuyable.Keys{LocalKey: d.LocalKey, SecKey: d.SecKey}
	if d.RejectKey {
		k.LocalKey = "Xx" + d.LocalKey[2:]
	}
	return k
}

// Advert returns the device's advertisement: service data on 0xA201 and manufacturer data on 0x07D0.
func (d *Device) Advert() (serviceData, manufacturerData []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	return tuyable.EncodeAdvert(tuyable.Advert{ProductID: d.ProductID, UUID: d.UUID, Protocol: d.Protocol, Bound: true})
}

// Connect opens a link, or fails with tuyable.ErrBusy when SingleCentral is set and a link is open.
func (d *Device) Connect() (tuyable.Link, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	if d.SingleCentral && d.connected != nil {
		return nil, tuyable.ErrBusy
	}
	l := &link{dev: d, inbox: make(chan []byte, 64), notes: make(chan []byte, 256), stop: make(chan struct{})}
	d.connected = l
	d.paired = false
	l.wg.Add(1)
	go l.run()
	return l, nil
}

// Drop cuts the open link from the device side, like a device going out of range.
func (d *Device) Drop() {
	d.mu.Lock()
	l := d.connected
	d.mu.Unlock()
	if l != nil {
		l.Close()
	}
}

// State returns the current DP values.
func (d *Device) State() map[byte]tuyable.DP {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	out := make(map[byte]tuyable.DP, len(d.state))
	for k, v := range d.state {
		out[k] = v
	}
	return out
}

// Acks returns the acknowledgements the host sent for pushed reports.
func (d *Device) Acks() []tuyable.Frame {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.acks)
}

// TimeAnswer returns the host's answer to a TIME1 or TIME2 request.
func (d *Device) TimeAnswer(code tuyable.Code) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.times[code]
	return b, ok
}

// Writes returns the DP write requests the device received, in order.
func (d *Device) Writes() []tuyable.Frame {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.writes)
}

// Set changes DPs on the device side and pushes them as a report, like a button press on the device.
func (d *Device) Set(dps ...tuyable.DP) error {
	return d.Push(0, dps...)
}

// Push sends a report with the given code (0 picks RECEIVE_DP or RECEIVE_DP_V4 by protocol). Timed and sign
// reports carry a fixed timestamp of 2026-01-01T00:00:00Z and DP sequence numbers.
func (d *Device) Push(code tuyable.Code, dps ...tuyable.DP) error {
	d.mu.Lock()
	d.init()
	l := d.connected
	for _, dp := range dps {
		d.state[dp.ID] = dp
	}
	d.mu.Unlock()
	if l == nil {
		return tuyable.ErrClosed
	}
	return l.report(code, dps)
}

// SetMute switches Mute while the device may be connected.
func (d *Device) SetMute(on bool) {
	d.mu.Lock()
	d.Mute = on
	d.mu.Unlock()
}

// SetDropFragment arms DropFragment while the device may be sending.
func (d *Device) SetDropFragment(n int) {
	d.mu.Lock()
	d.DropFragment = n
	d.mu.Unlock()
}

// Inject sends an arbitrary frame sealed with the session key (the device must be paired), fragmented at mtu
// (20 when 0). It is for fuzzing the host's parser past the CRC.
func (d *Device) Inject(f tuyable.Frame, mtu int) error {
	d.mu.Lock()
	l := d.connected
	d.mu.Unlock()
	if l == nil {
		return tuyable.ErrClosed
	}
	return l.sendSessionMTU(f, mtu)
}

type link struct {
	dev       *Device
	inbox     chan []byte
	notes     chan []byte
	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	sendMu    sync.Mutex
}

func (l *link) Write(ctx context.Context, packet []byte) error {
	if len(packet) > tuyable.GATTMTU {
		return errors.New("tuyablesim: write longer than the GATT MTU")
	}
	select {
	case l.inbox <- bytes.Clone(packet):
		return nil
	case <-l.stop:
		return tuyable.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *link) Notifications() <-chan []byte { return l.notes }

func (l *link) Close() error {
	l.closeOnce.Do(func() {
		close(l.stop)
		l.wg.Wait()
		l.sendMu.Lock()
		close(l.notes)
		l.sendMu.Unlock()
		d := l.dev
		d.mu.Lock()
		if d.connected == l {
			d.connected = nil
		}
		d.mu.Unlock()
	})
	return nil
}

func (l *link) run() {
	defer l.wg.Done()
	var r tuyable.Reassembler
	for {
		select {
		case <-l.stop:
			return
		case pkt := <-l.inbox:
			msg, err := r.Push(pkt)
			if err != nil || msg == nil {
				continue
			}
			l.handle(msg)
		}
	}
}

func (l *link) handle(msg []byte) {
	d := l.dev
	d.mu.Lock()
	mute := d.Mute
	keys := d.keys()
	session := keys.SessionKey(d.Srand)
	d.mu.Unlock()
	if mute {
		return
	}
	f, _, err := tuyable.Unseal(msg, func(flag byte) []byte {
		switch flag {
		case tuyable.FlagLogin, tuyable.FlagLoginV2:
			return keys.LoginKey()
		case tuyable.FlagSession, tuyable.FlagSessionV2:
			return session
		}
		return nil
	})
	if err != nil {
		if d.RejectKey && msg[0] == keys.LoginFlag() {
			// The device could not read the request but answers anyway, in its own key.
			l.deviceInfo(tuyable.Frame{Seq: 1})
		}
		return
	}
	switch f.Code {
	case tuyable.CodeDeviceInfo:
		l.deviceInfo(f)
	case tuyable.CodePair:
		l.pair(f, keys)
	case tuyable.CodeDeviceStatus:
		l.reply(f, []byte{0})
		d.mu.Lock()
		all := make([]tuyable.DP, 0, len(d.state))
		for _, id := range slices.Sorted(maps.Keys(d.state)) {
			all = append(all, d.state[id])
		}
		d.mu.Unlock()
		if len(all) > 0 {
			l.report(0, all)
		}
	case tuyable.CodeDPs, tuyable.CodeDPsV4:
		l.write(f)
	default:
		if f.Code >= 0x8000 && f.ResponseTo != 0 {
			d.mu.Lock()
			if f.Code == tuyable.CodeTime1Request || f.Code == tuyable.CodeTime2Request {
				d.times[f.Code] = f.Data
			} else {
				d.acks = append(d.acks, f)
			}
			d.mu.Unlock()
		}
	}
}

func (l *link) deviceInfo(f tuyable.Frame) {
	d := l.dev
	d.mu.Lock()
	bound := byte(0)
	if d.AlreadyBound {
		bound = 1
	}
	data := append([]byte{1, 0, d.Protocol, 0, 0, bound}, d.Srand...)
	data = append(append(data, 1, 0), d.authKey...)
	keys := d.keys()
	glitch := d.Glitch
	d.mu.Unlock()
	if glitch {
		l.raw([]byte{5, 0xAA, 0xBB})
	}
	l.send(tuyable.Frame{Code: tuyable.CodeDeviceInfo, ResponseTo: f.Seq, Data: data}, keys.LoginKey(), keys.LoginFlag(), 0)
}

func (l *link) pair(f tuyable.Frame, keys tuyable.Keys) {
	d := l.dev
	d.mu.Lock()
	want, _ := tuyable.PairRequest(d.UUID, keys, d.DeviceID)
	result := d.PairResult
	if !bytes.Equal(f.Data, want) {
		result = 1
	} else if result == 0 && d.AlreadyBound {
		result = 2
	}
	d.paired = result == 0 || result == 2
	ask := d.AskTime && d.paired
	after := slices.Clone(d.AfterPair)
	d.mu.Unlock()
	l.reply(f, []byte{result})
	if d.paired {
		for _, extra := range after {
			l.sendSession(extra)
		}
	}
	if ask {
		l.sendSession(tuyable.Frame{Code: tuyable.CodeTime1Request})
		l.sendSession(tuyable.Frame{Code: tuyable.CodeTime2Request})
	}
}

func (l *link) write(f tuyable.Frame) {
	d := l.dev
	size, pos := 1, 0
	if f.Code == tuyable.CodeDPsV4 {
		size, pos = 2, 5
	}
	d.mu.Lock()
	d.writes = append(d.writes, f)
	d.mu.Unlock()
	if len(f.Data) < pos {
		return
	}
	dps, _, err := tuyable.ParseDPs(f.Data, pos, size)
	result := byte(0)
	if err != nil {
		result = 1
	}
	if f.Code == tuyable.CodeDPsV4 {
		l.reply(f, append(bytes.Clone(f.Data[:5]), result))
	} else {
		l.reply(f, []byte{result})
	}
	if err == nil && len(dps) > 0 {
		d.mu.Lock()
		for _, dp := range dps {
			d.state[dp.ID] = dp
		}
		d.mu.Unlock()
		l.report(0, dps)
	}
}

func (l *link) reply(f tuyable.Frame, data []byte) {
	l.sendSession(tuyable.Frame{Code: f.Code, ResponseTo: f.Seq, Data: data})
}

// report pushes DPs with the given code.
func (l *link) report(code tuyable.Code, dps []tuyable.DP) error {
	d := l.dev
	d.mu.Lock()
	proto := d.Protocol
	d.dpSeq++
	n := d.dpSeq
	d.mu.Unlock()
	if code == 0 {
		code = tuyable.CodeReceiveDP
		if proto >= 4 {
			code = tuyable.CodeReceiveDPV4
		}
	}
	const ts = 1767225600 // 2026-01-01T00:00:00Z
	timestamp := binary.BigEndian.AppendUint32([]byte{1}, ts)
	var head []byte
	size := 1
	switch code {
	case tuyable.CodeReceiveDP:
	case tuyable.CodeReceiveTimeDP:
		head = timestamp
	case tuyable.CodeReceiveSignDP:
		head = []byte{byte(n >> 8), byte(n), 0}
	case tuyable.CodeReceiveSignTimeDP:
		head = append([]byte{byte(n >> 8), byte(n), 0}, timestamp...)
	case tuyable.CodeReceiveDPV4, tuyable.CodeReceiveTimeDPV4:
		head = append(binary.BigEndian.AppendUint32([]byte{0}, n), 0, 0)
		if code == tuyable.CodeReceiveTimeDPV4 {
			head = append(head, timestamp...)
		}
		size = 2
	default:
		return errors.New("tuyablesim: not a report code")
	}
	body, err := tuyable.EncodeDPs(dps, size)
	if err != nil {
		return err
	}
	return l.sendSession(tuyable.Frame{Code: code, Data: append(head, body...)})
}

func (l *link) sendSession(f tuyable.Frame) error { return l.sendSessionMTU(f, 0) }

func (l *link) sendSessionMTU(f tuyable.Frame, mtu int) error {
	d := l.dev
	d.mu.Lock()
	keys := d.keys()
	key := keys.SessionKey(d.Srand)
	d.mu.Unlock()
	return l.send(f, key, keys.SessionFlag(), mtu)
}

// raw sends one notification as is.
func (l *link) raw(p []byte) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	select {
	case l.notes <- p:
	case <-l.stop:
	}
}

func (l *link) send(f tuyable.Frame, key []byte, flag byte, mtuOverride int) error {
	d := l.dev
	d.mu.Lock()
	f.Seq = d.seq
	d.seq++
	proto, mtu, drop := d.Protocol, d.NotifyMTU, d.DropFragment
	if mtuOverride > 0 {
		mtu = mtuOverride
	}
	d.DropFragment = 0
	d.mu.Unlock()
	msg, err := tuyable.Seal(f, key, flag, nil)
	if err != nil {
		return err
	}
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	for i, p := range tuyable.Fragment(msg, proto, mtu) {
		if drop == i+1 {
			continue
		}
		select {
		case <-l.stop:
			return tuyable.ErrClosed
		default:
		}
		select {
		case l.notes <- p:
		case <-l.stop:
			return tuyable.ErrClosed
		}
	}
	return nil
}
