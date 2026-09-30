package tuyable

import (
	"fmt"
)

// GATTMTU is the payload size of one Tuya BLE write or notification (const.py GATT_MTU). Devices use 20 bytes
// whatever MTU the link negotiated.
const GATTMTU = 20

// MaxMessage bounds a reassembled message (flag ‖ IV ‖ ciphertext of at most MaxData bytes of data).
const MaxMessage = 1 + ivSize + headerSize + MaxData + 2 + 16

// AppendVarint appends v as the protocol's unsigned varint: seven bits per byte, least significant group first,
// high bit set on every byte but the last (tuya_ble.py _pack_int).
func AppendVarint(b []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if v == 0 {
			return b
		}
	}
}

// Varint reads a varint at pos and returns the value and the position after it. Like _unpack_int it accepts
// at most four bytes (28 bits).
func Varint(data []byte, pos int) (uint32, int, error) {
	if pos < 0 {
		return 0, pos, fmt.Errorf("%w: negative position", ErrFormat)
	}
	var v uint32
	for i := range 4 {
		if pos+i >= len(data) {
			return 0, pos, fmt.Errorf("%w: truncated varint", ErrFormat)
		}
		c := data[pos+i]
		v |= uint32(c&0x7F) << (7 * i)
		if c&0x80 == 0 {
			return v, pos + i + 1, nil
		}
	}
	return 0, pos, fmt.Errorf("%w: varint longer than 4 bytes", ErrFormat)
}

// MinMTU is the smallest packet size Fragment uses: room for the largest first-packet header (packet number,
// a 3-byte message length and the protocol byte) plus data. Smaller values are raised to it.
const MinMTU = 8

// Fragment splits a message into GATT packets of at most mtu bytes (GATTMTU when mtu is 0, at least MinMTU). The
// first carries the packet number 0, the total message length and the protocol version in the high nibble of one
// byte; the others carry only their packet number (tuya_ble.py _build_packets).
func Fragment(msg []byte, protocol byte, mtu int) [][]byte {
	if mtu <= 0 {
		mtu = GATTMTU
	}
	mtu = max(mtu, MinMTU)
	var out [][]byte
	for n, pos := uint32(0), 0; pos < len(msg); n++ {
		p := AppendVarint(nil, n)
		if n == 0 {
			p = AppendVarint(p, uint32(len(msg)))
			p = append(p, protocol<<4)
		}
		take := min(mtu-len(p), len(msg)-pos)
		out = append(out, append(p, msg[pos:pos+take]...))
		pos += take
	}
	return out
}

// Reassembler joins notifications back into messages. It follows _notification_handler: a packet 0 always
// starts a new message (dropping a partial one), a repeated earlier packet is ignored, a gap drops the partial
// message, and a message longer than announced is dropped. It copies what it keeps, so callers may reuse packets.
type Reassembler struct {
	buf    []byte
	want   int
	next   uint32
	active bool
}

// Push adds one notification. It returns the message when the packet completes one. An error means the packet
// or the partial message was dropped; the reassembler is ready for the next packet 0 either way.
func (r *Reassembler) Push(pkt []byte) ([]byte, error) {
	n, pos, err := Varint(pkt, 0)
	if err != nil {
		r.Reset()
		return nil, err
	}
	if n == 0 {
		length, p, err := Varint(pkt, pos)
		if err != nil {
			r.Reset()
			return nil, err
		}
		if p >= len(pkt) {
			r.Reset()
			return nil, fmt.Errorf("%w: first packet without protocol byte", ErrFormat)
		}
		if length == 0 || int(length) > MaxMessage {
			r.Reset()
			return nil, fmt.Errorf("%w: message length %d", ErrFormat, length)
		}
		r.buf = make([]byte, 0, length)
		r.want = int(length)
		r.next = 0
		r.active = true
		pos = p + 1
	} else if r.active && n < r.next {
		// A duplicate of a packet already taken (a retransmission): ignore it and keep the message.
		return nil, nil
	} else if !r.active || n != r.next {
		missing := r.next
		r.Reset()
		return nil, fmt.Errorf("%w: packet %d out of order (expected %d)", ErrFormat, n, missing)
	}
	r.buf = append(r.buf, pkt[pos:]...)
	r.next++
	if len(r.buf) > r.want {
		r.Reset()
		return nil, fmt.Errorf("%w: message longer than announced", ErrFormat)
	}
	if len(r.buf) == r.want {
		msg := r.buf
		r.Reset()
		return msg, nil
	}
	return nil, nil
}

// Reset drops any partial message.
func (r *Reassembler) Reset() { *r = Reassembler{} }
