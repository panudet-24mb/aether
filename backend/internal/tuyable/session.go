package tuyable

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

// Link is one GATT connection to a device: writes go to its write characteristic, notifications come from its
// notify characteristic (0x2B11/0x2B10 on legacy devices, the FD50 pair on newer ones). The radio code (BlueZ,
// an ESPHome proxy, the simulator) implements it; nothing here knows about Bluetooth.
//
// Contract:
//
//   - Write sends one packet of at most GATTMTU bytes as a write without response. It must return promptly when
//     ctx is done or the link is closed. Any error it returns is taken to mean the link is unusable, and fails the
//     session; a Write that fails only because ctx ended must return ctx.Err() (or wrap it).
//   - Notifications delivers every notification, in order, on a buffered channel, and never drops one silently:
//     a link that cannot keep up must close the channel instead. It is closed when the link drops or Close is
//     called. The session copies each slice before use, so the link may reuse its buffers.
//   - Close disconnects and may be called more than once.
//   - Connecting is the radio's job. A radio whose device refuses a second central should report ErrBusy from
//     its connect call; the session never returns it.
//
// A link may also implement Reasoner to say why its notifications ended (out of range, adapter gone); the
// session then reports that reason from Err.
type Link interface {
	Write(ctx context.Context, packet []byte) error
	Notifications() <-chan []byte
	Close() error
}

// Reasoner is optionally implemented by a Link to explain a disconnect.
type Reasoner interface {
	// Reason is the cause of the disconnect, or nil if the link is still up or was closed by the host.
	Reason() error
}

// Config describes the device a session talks to. Keys, UUID and DeviceID come from the Tuya import.
type Config struct {
	Keys     Keys
	UUID     string
	DeviceID string
	// ProductID and FD50 (the device was reached through the FD50 service) select firmware quirks.
	ProductID string
	FD50      bool
	// AdvertisedProtocol is the protocol version from the device's advertisement (Advert.Protocol). The first
	// fragment of every message carries a protocol version before the device-info answer says which one the
	// device speaks; like ha_tuya_ble, the session uses the advertised one (2 when it is unknown), except for the
	// FD50 device-info quirk, which always says 2.
	AdvertisedProtocol byte
	// ResponseTimeout bounds each request; 10 s when zero. ha_tuya_ble waits 15 s, but devices answer within a
	// few hundred milliseconds or not at all.
	ResponseTimeout time.Duration
	// Now answers the device's clock requests, in the time zone of the time it returns; time.Now when nil.
	Now func() time.Time
	// ReportBuffer is how many reports may wait for the reader of Reports; 32 when zero. Beyond that the waiting
	// reports are merged into one that keeps the latest value of each DP (Report.Coalesced).
	ReportBuffer int
}

// String, GoString and LogValue leave the keys out, so a Config can be logged.
func (c Config) String() string {
	return fmt.Sprintf("tuyable.Config{UUID:%q DeviceID:%q ProductID:%q FD50:%t AdvertisedProtocol:%d Keys:%s}",
		c.UUID, c.DeviceID, c.ProductID, c.FD50, c.AdvertisedProtocol, c.Keys)
}

func (c Config) GoString() string { return c.String() }

func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("uuid", c.UUID), slog.String("device_id", c.DeviceID),
		slog.String("product_id", c.ProductID), slog.Bool("fd50", c.FD50), slog.Any("keys", c.Keys))
}

// DeviceInfo is the device's answer to the device-info request.
type DeviceInfo struct {
	DeviceVersion   string
	ProtocolVersion string
	HardwareVersion string
	// Protocol is the major protocol version (2, 3 or 4); it decides the DP encoding.
	Protocol byte
	Flags    byte
	Bound    bool
}

// Report is a set of DP values the device pushed.
type Report struct {
	Code Code
	// Time is the device's timestamp for timed reports and the receive time otherwise.
	Time        time.Time
	Timestamped bool
	// Flags is the report's flags (sign reports) or mode (protocol 4) byte.
	Flags byte
	DPs   []DP
	// Coalesced is set when reports piled up and were merged: DPs holds the latest value of each DP they
	// carried, intermediate values are lost. A consumer that needs every value can call Status to re-read.
	Coalesced bool
}

// Stats counts what the session ignored or merged.
type Stats struct {
	// Coalesced counts reports merged because the reader of Reports fell behind.
	Coalesced int
	// Errors counts messages that could not be reassembled, decrypted or parsed, and answers that matched no
	// request.
	Errors int
	// Unknown counts device-initiated messages with a code this package does not know; they are ignored.
	Unknown int
}

type result struct {
	f   Frame
	err error
}

type call struct {
	code Code
	ch   chan result
}

// Session is an authenticated conversation with one device.
//
// Handshake (tuya_ble.py _ensure_connected):
//
//  1. DEVICE_INFO with the login key; the 46+ byte answer carries device version (bytes 0–1), protocol version
//     (2–3), flags (4), bound (5), srand (6–11), hardware version (12–13) and the auth key (14–45).
//  2. The session key is derived from srand (see Keys).
//  3. PAIR with uuid ‖ local_key[:6] ‖ device id; result 0 is paired, 2 is already paired.
//
// After that the host asks for DEVICE_STATUS (the device pushes every DP) and writes DPs with DPS (protocol 3) or
// DPS_V4 (protocol 4). The device pushes changes on its own with the RECEIVE_* codes; each report is acknowledged,
// and TIME1/TIME2 clock requests are answered.
//
// A session ends when Close is called, the link drops or a write fails; Done is then closed and Err says why.
// Callers must always Close a session, also after it ended on its own, to release the link.
type Session struct {
	link Link
	cfg  Config
	keys Keys

	writeMu sync.Mutex

	mu         sync.Mutex
	seq        uint32
	protocol   byte
	sessionKey []byte
	authKey    []byte
	pending    map[uint32]*call
	failure    error
	info       DeviceInfo
	stats      Stats
	queue      []Report

	reports   chan Report
	queued    chan struct{}
	quit      chan struct{}
	quitOnce  sync.Once
	done      chan struct{}
	delivered chan struct{}
	closed    chan struct{}
	acks      sync.WaitGroup
	closeOnce sync.Once
}

// Open runs the handshake over link. On any error the link is closed.
func Open(ctx context.Context, link Link, cfg Config) (*Session, error) {
	keys, err := NewKeys(cfg.Keys.LocalKey, cfg.Keys.SecKey)
	if err != nil {
		link.Close()
		return nil, err
	}
	if UsesLegacyKeys(cfg.ProductID) {
		keys.SecKey = ""
	}
	if cfg.ResponseTimeout <= 0 {
		cfg.ResponseTimeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReportBuffer <= 0 {
		cfg.ReportBuffer = 32
	}
	proto := cfg.AdvertisedProtocol
	if proto < 2 || proto > 4 {
		proto = 2
	}
	s := &Session{
		link: link, cfg: cfg, keys: keys,
		seq: 1, protocol: proto,
		pending:   map[uint32]*call{},
		reports:   make(chan Report),
		queued:    make(chan struct{}, 1),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
		delivered: make(chan struct{}),
		closed:    make(chan struct{}),
	}
	go s.read()
	go s.deliver()
	if err := s.handshake(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) handshake(ctx context.Context) error {
	var payload []byte
	if NeedsFD50DeviceInfo(s.cfg.ProductID, s.cfg.FD50) {
		payload = []byte{0x00, 0xF3} // TuyaOS FD50 login framing; send() also marks its first fragment protocol 2
	}
	f, err := s.request(ctx, CodeDeviceInfo, payload)
	if err != nil {
		return err
	}
	info, srand, auth, err := ParseDeviceInfo(f.Data)
	if err != nil {
		return err
	}
	if info.Protocol < 2 || info.Protocol > 4 {
		return fmt.Errorf("%w: device speaks %s", ErrUnsupportedProtocol, info.ProtocolVersion)
	}
	s.mu.Lock()
	s.info = info
	s.protocol = info.Protocol
	s.sessionKey = s.keys.SessionKey(srand)
	s.authKey = auth
	s.mu.Unlock()

	pair, err := PairRequest(s.cfg.UUID, s.keys, s.cfg.DeviceID)
	if err != nil {
		return err
	}
	f, err = s.request(ctx, CodePair, pair)
	if err != nil {
		return err
	}
	if len(f.Data) != 1 {
		return fmt.Errorf("%w: pair answer of %d bytes", ErrFormat, len(f.Data))
	}
	if r := f.Data[0]; r != 0 && r != 2 {
		return fmt.Errorf("%w: %w", ErrKeyRejected, &DeviceError{Code: CodePair, Result: r})
	}
	return nil
}

// ParseDeviceInfo reads the device-info answer: device version (bytes 0–1), protocol version (2–3), flags (4),
// bound (5), the 6-byte srand (6–11), hardware version (12–13) and the 32-byte auth key (14–45).
func ParseDeviceInfo(d []byte) (info DeviceInfo, srand, authKey []byte, err error) {
	if len(d) < 46 {
		return DeviceInfo{}, nil, nil, fmt.Errorf("%w: device info of %d bytes", ErrFormat, len(d))
	}
	info = DeviceInfo{
		DeviceVersion:   fmt.Sprintf("%d.%d", d[0], d[1]),
		ProtocolVersion: fmt.Sprintf("%d.%d", d[2], d[3]),
		HardwareVersion: fmt.Sprintf("%d.%d", d[12], d[13]),
		Protocol:        d[2],
		Flags:           d[4],
		Bound:           d[5] != 0,
	}
	return info, bytes.Clone(d[6:12]), bytes.Clone(d[14:46]), nil
}

// Info returns the device-info answer.
func (s *Session) Info() DeviceInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Reports delivers DP reports. It is closed after the session ends and the waiting reports were delivered (or
// Close was called).
func (s *Session) Reports() <-chan Report { return s.reports }

// Stats returns the session's counters.
func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Done is closed when the session ends; Err then says why.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is the reason the session ended (wrapping ErrClosed), or nil while it is up.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

// Status asks the device for every DP; the values arrive on Reports.
func (s *Session) Status(ctx context.Context) error {
	f, err := s.request(ctx, CodeDeviceStatus, nil)
	if err != nil {
		return err
	}
	if len(f.Data) != 1 {
		return fmt.Errorf("%w: status answer of %d bytes", ErrFormat, len(f.Data))
	}
	if f.Data[0] != 0 {
		return &DeviceError{Code: CodeDeviceStatus, Result: f.Data[0]}
	}
	return nil
}

// SetDPs writes DP values in one request. The device confirms by pushing the new values on Reports.
//
// The protocol-4 dp_seq is drawn from the frame sequence counter, as ha_tuya_ble does (_send_datapoints_v4 takes
// _get_seq_num() for it, and the frame takes the next number).
func (s *Session) SetDPs(ctx context.Context, dps []DP) error {
	s.mu.Lock()
	proto := s.protocol
	s.mu.Unlock()
	switch {
	case proto == 3:
		data, err := EncodeDPs(dps, 1)
		if err != nil {
			return err
		}
		f, err := s.request(ctx, CodeDPs, data)
		if err != nil {
			return err
		}
		if len(f.Data) == 1 && f.Data[0] != 0 {
			return &DeviceError{Code: CodeDPs, Result: f.Data[0]}
		}
		return nil
	case proto >= 4:
		body, err := EncodeDPs(dps, 2)
		if err != nil {
			return err
		}
		s.mu.Lock()
		n := s.nextSeq()
		s.mu.Unlock()
		data := binary.BigEndian.AppendUint32([]byte{0}, n)
		f, err := s.request(ctx, CodeDPsV4, append(data, body...))
		if err != nil {
			return err
		}
		if len(f.Data) != 6 {
			return fmt.Errorf("%w: DPS_V4 answer of %d bytes", ErrFormat, len(f.Data))
		}
		if f.Data[5] != 0 {
			return &DeviceError{Code: CodeDPsV4, Result: f.Data[5]}
		}
		return nil
	}
	return fmt.Errorf("%w: DP writes need protocol 3 or 4, device speaks %d", ErrUnsupportedProtocol, proto)
}

// Close ends the session and disconnects. It is safe to call more than once.
func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		s.fail(ErrClosed)
		err = s.link.Close()
		<-s.done
		<-s.delivered
		s.acks.Wait()
	})
	return err
}

// fail ends the session with err (the first reason wins) and stops the reader.
func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.failure == nil {
		s.failure = err
	}
	s.mu.Unlock()
	s.quitOnce.Do(func() { close(s.quit) })
}

// request sends a host-initiated frame and waits for the answer: same code, response_to set to its sequence
// number.
func (s *Session) request(parent context.Context, code Code, data []byte) (Frame, error) {
	c := &call{code: code, ch: make(chan result, 1)}
	s.mu.Lock()
	if s.failure != nil {
		err := s.failure
		s.mu.Unlock()
		return Frame{}, err
	}
	seq := s.nextSeq()
	s.pending[seq] = c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, seq)
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(parent, s.cfg.ResponseTimeout)
	defer cancel()
	if err := s.send(ctx, Frame{Seq: seq, Code: code, Data: data}); err != nil {
		return Frame{}, s.timeoutErr(parent, ctx, code, err)
	}
	select {
	case r := <-c.ch:
		return r.f, r.err
	case <-ctx.Done():
		return Frame{}, s.timeoutErr(parent, ctx, code, ctx.Err())
	}
}

// timeoutErr tells the caller's own cancellation or deadline (returned as is) from the response timeout
// (ErrTimeout wrapping the context error). Other errors pass through.
func (s *Session) timeoutErr(parent, ctx context.Context, code Code, err error) error {
	switch {
	case parent.Err() != nil:
		return parent.Err()
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return fmt.Errorf("%w: %s: %w", ErrTimeout, code, ctx.Err())
	}
	return err
}

func (s *Session) nextSeq() uint32 {
	n := s.seq
	s.seq++
	return n
}

// send seals a frame with the key its code calls for and writes its fragments in order. A write that fails for
// any reason but ctx ends the session: the link is gone.
func (s *Session) send(ctx context.Context, f Frame) error {
	s.mu.Lock()
	key, flag := s.sessionKey, s.keys.SessionFlag()
	proto := s.protocol
	if f.Code == CodeDeviceInfo {
		key, flag = s.keys.LoginKey(), s.keys.LoginFlag()
		if NeedsFD50DeviceInfo(s.cfg.ProductID, s.cfg.FD50) {
			proto = 2
		}
	}
	s.mu.Unlock()
	if key == nil {
		return fmt.Errorf("%w: no session key yet", ErrFormat)
	}
	msg, err := Seal(f, key, flag, nil)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, p := range Fragment(msg, proto, GATTMTU) {
		if err := s.link.Write(ctx, p); err != nil {
			if ctx.Err() != nil {
				// The request's own deadline or cancellation. The device drops the partial message when the
				// next one starts with packet 0.
				return ctx.Err()
			}
			err = fmt.Errorf("%w: write: %w", ErrClosed, err)
			s.fail(err)
			return err
		}
	}
	return nil
}

func (s *Session) keyFor(flag byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch flag {
	case FlagLogin, FlagLoginV2:
		return s.keys.LoginKey()
	case FlagSession, FlagSessionV2:
		return s.sessionKey
	case FlagAuth:
		// The auth key is 32 bytes, so frames sealed with it are AES-256-CBC.
		return s.authKey
	}
	return nil
}

// read runs until the link's notifications end or the session ends.
func (s *Session) read() {
	var r Reassembler
	defer func() {
		s.mu.Lock()
		err := s.failure
		for seq, c := range s.pending {
			c.ch <- result{err: err}
			delete(s.pending, seq)
		}
		s.mu.Unlock()
		close(s.done)
	}()
	notes := s.link.Notifications()
	for {
		select {
		case <-s.quit:
			return
		case pkt, ok := <-notes:
			if !ok {
				reason := fmt.Errorf("%w: link dropped", ErrClosed)
				if rs, ok := s.link.(Reasoner); ok && rs.Reason() != nil {
					reason = fmt.Errorf("%w: link dropped: %w", ErrClosed, rs.Reason())
				}
				s.fail(reason)
				return
			}
			msg, err := r.Push(bytes.Clone(pkt))
			if err != nil {
				s.count(func(st *Stats) { st.Errors++ })
				continue
			}
			if msg == nil {
				continue
			}
			f, flag, err := Unseal(msg, s.keyFor)
			if err != nil {
				s.unreadable(flag, err)
				continue
			}
			s.dispatch(f)
		}
	}
}

func (s *Session) count(fn func(*Stats)) {
	s.mu.Lock()
	fn(&s.stats)
	s.mu.Unlock()
}

// unreadable handles a message that did not decrypt. A message sealed with the login flag that fails its CRC or
// length check while a DEVICE_INFO request waits is the device answering with a different key: that request fails
// with ErrKeyRejected at once instead of running into its timeout. Anything else is only counted, and never ends
// the session.
func (s *Session) unreadable(flag byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Errors++
	if flag != s.keys.LoginFlag() || !errors.Is(err, ErrCRC) {
		return
	}
	for seq, c := range s.pending {
		if c.code == CodeDeviceInfo {
			c.ch <- result{err: fmt.Errorf("%w: device info did not decrypt: %v", ErrKeyRejected, err)}
			delete(s.pending, seq)
		}
	}
}

var deviceCodes = map[Code]bool{
	CodeReceiveDP: true, CodeReceiveTimeDP: true, CodeReceiveSignDP: true, CodeReceiveSignTimeDP: true,
	CodeReceiveDPV4: true, CodeReceiveTimeDPV4: true, CodeTime1Request: true, CodeTime2Request: true,
}

func (s *Session) dispatch(f Frame) {
	if f.Code < 0x8000 {
		s.mu.Lock()
		c := s.pending[f.ResponseTo]
		if c == nil || c.code != f.Code || f.ResponseTo == 0 {
			s.stats.Errors++
			s.mu.Unlock()
			return
		}
		delete(s.pending, f.ResponseTo)
		s.mu.Unlock()
		c.ch <- result{f: f}
		return
	}
	if !deviceCodes[f.Code] {
		s.count(func(st *Stats) { st.Unknown++ })
		return
	}
	now := s.cfg.Now()
	switch f.Code {
	case CodeTime1Request:
		s.answer(f, Time1Answer(now))
		return
	case CodeTime2Request:
		s.answer(f, Time2Answer(now))
		return
	}
	rep, ack, err := parseReport(f, now)
	if err != nil {
		s.count(func(st *Stats) { st.Errors++ })
		return
	}
	if ack != nil {
		s.answer(f, ack)
	}
	s.enqueue(rep)
}

// enqueue hands a report to deliver. Once ReportBuffer reports wait, they are merged into one holding the latest
// value of each DP, so a slow reader loses intermediate values but never the current state.
func (s *Session) enqueue(rep Report) {
	s.mu.Lock()
	if len(s.queue) >= s.cfg.ReportBuffer {
		latest := map[byte]DP{}
		for _, q := range append(s.queue, rep) {
			for _, dp := range q.DPs {
				latest[dp.ID] = dp
			}
		}
		merged := Report{Code: rep.Code, Time: rep.Time, Timestamped: rep.Timestamped, Flags: rep.Flags, Coalesced: true}
		for _, id := range slices.Sorted(maps.Keys(latest)) {
			merged.DPs = append(merged.DPs, latest[id])
		}
		s.queue = []Report{merged}
		s.stats.Coalesced++
	} else {
		s.queue = append(s.queue, rep)
	}
	s.mu.Unlock()
	select {
	case s.queued <- struct{}{}:
	default:
	}
}

// deliver moves queued reports to Reports. After the session ends it delivers what is left, then closes Reports;
// Close stops it at once.
func (s *Session) deliver() {
	defer func() {
		close(s.reports)
		close(s.delivered)
	}()
	for {
		s.mu.Lock()
		var next *Report
		if len(s.queue) > 0 {
			r := s.queue[0]
			s.queue = s.queue[1:]
			next = &r
		}
		s.mu.Unlock()
		if next == nil {
			select {
			case <-s.queued:
				continue
			case <-s.done:
				s.mu.Lock()
				empty := len(s.queue) == 0
				s.mu.Unlock()
				if empty {
					return
				}
				continue
			}
		}
		select {
		case s.reports <- *next:
		case <-s.closed:
			return
		}
	}
}

// parseReport decodes a device-initiated DP report and builds its acknowledgement (nil when the device asked for
// none).
//
//	RECEIVE_DP            dps(v3)                                        ack: empty
//	RECEIVE_TIME_DP       timestamp ‖ dps(v3)                            ack: empty
//	RECEIVE_SIGN_DP       dp_seq(2) ‖ flags(1) ‖ dps(v3)                  ack: dp_seq(2) ‖ flags(1) ‖ 0
//	RECEIVE_SIGN_TIME_DP  dp_seq(2) ‖ flags(1) ‖ timestamp ‖ dps(v3)      ack: dp_seq(2) ‖ flags(1) ‖ 0
//	RECEIVE_DP_V4         0 ‖ dp_seq(4) ‖ send_flags(1) ‖ mode(1) ‖ dps(v4)            ack: first 7 bytes ‖ 0
//	RECEIVE_TIME_DP_V4    0 ‖ dp_seq(4) ‖ send_flags(1) ‖ mode(1) ‖ timestamp ‖ dps(v4) ack: first 7 bytes ‖ 0
//
// Protocol-4 reports with bit 7 of send_flags set want no acknowledgement. ha_tuya_ble starts reading the DPs of
// sign reports at offset 2, on the flags byte; this reads them after it, at offset 3 (see docs/platform/tuya-ble.md).
func parseReport(f Frame, now time.Time) (Report, []byte, error) {
	d := f.Data
	rep := Report{Code: f.Code, Time: now}
	var ack []byte
	pos, size := 0, 1
	var err error
	timed := false
	switch f.Code {
	case CodeReceiveDP:
		ack = []byte{}
	case CodeReceiveTimeDP:
		ack, timed = []byte{}, true
	case CodeReceiveSignDP, CodeReceiveSignTimeDP:
		if len(d) < 3 {
			return rep, nil, fmt.Errorf("%w: %s of %d bytes", ErrFormat, f.Code, len(d))
		}
		rep.Flags = d[2]
		ack = []byte{d[0], d[1], d[2], 0}
		pos, timed = 3, f.Code == CodeReceiveSignTimeDP
	case CodeReceiveDPV4, CodeReceiveTimeDPV4:
		need := 7
		if f.Code == CodeReceiveTimeDPV4 {
			need = 8
		}
		if len(d) < need || d[0] != 0 {
			return rep, nil, fmt.Errorf("%w: %s header", ErrFormat, f.Code)
		}
		rep.Flags = d[6]
		if d[5]&0x80 == 0 {
			ack = append(append([]byte(nil), d[:7]...), 0)
		}
		pos, size, timed = 7, 2, f.Code == CodeReceiveTimeDPV4
	default:
		return rep, nil, fmt.Errorf("%w: unexpected device message %s", ErrFormat, f.Code)
	}
	if timed {
		if rep.Time, pos, err = ParseTimestamp(d, pos); err != nil {
			return rep, nil, err
		}
		rep.Timestamped = true
	}
	if rep.DPs, _, err = ParseDPs(d, pos, size); err != nil {
		return rep, nil, err
	}
	return rep, ack, nil
}

// answer replies to a device-initiated frame (same code, response_to = its sequence number). It runs in the
// background so a slow write never stalls the reader; it is abandoned when the session ends.
func (s *Session) answer(f Frame, data []byte) {
	s.mu.Lock()
	if s.failure != nil {
		s.mu.Unlock()
		return
	}
	seq := s.nextSeq()
	s.mu.Unlock()
	s.acks.Add(1)
	go func() {
		defer s.acks.Done()
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ResponseTimeout)
		defer cancel()
		go func() {
			select {
			case <-s.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		_ = s.send(ctx, Frame{Seq: seq, ResponseTo: f.Seq, Code: f.Code, Data: data})
	}()
}
