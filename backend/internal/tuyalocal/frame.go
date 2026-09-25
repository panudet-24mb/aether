package tuyalocal

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Command codes (command_types.py).
const (
	CmdSessKeyNegStart  uint32 = 0x03
	CmdSessKeyNegResp   uint32 = 0x04
	CmdSessKeyNegFinish uint32 = 0x05
	CmdControl          uint32 = 0x07
	CmdStatus           uint32 = 0x08
	CmdHeartBeat        uint32 = 0x09
	CmdDPQuery          uint32 = 0x0a
	CmdControlNew       uint32 = 0x0d
	CmdDPQueryNew       uint32 = 0x10
	CmdUpdateDPS        uint32 = 0x12
	CmdUDPNew           uint32 = 0x13
	CmdBoardcastLPV34   uint32 = 0x23
	CmdReqDevInfo       uint32 = 0x25
	CmdLANExtStream     uint32 = 0x40
)

// Frame prefixes and suffixes (header.py).
const (
	Prefix55AA uint32 = 0x000055AA
	Suffix55AA uint32 = 0x0000AA55
	Prefix6699 uint32 = 0x00006699
	Suffix6699 uint32 = 0x00009966
)

const (
	header55AALen = 16 // prefix, seq, cmd, length
	header6699Len = 18 // prefix, reserved u16, seq, cmd, length
	ivLen         = 12
	tagLen        = 16
	hmacLen       = 32
	// MaxFrameLength bounds the length field of a frame. tinytuya refuses anything over MAX_PAYLOAD_LENGTH
	// (1440, const.py) as corrupt; we allow some headroom for devices with many DPs but still stop a
	// corrupt or hostile length from making us buffer megabytes.
	MaxFrameLength = 4096
)

var (
	prefix55AA = []byte{0x00, 0x00, 0x55, 0xAA}
	prefix6699 = []byte{0x00, 0x00, 0x66, 0x99}
)

// Frame errors.
var (
	ErrShortFrame = errors.New("tuyalocal: not enough data for a frame")
	ErrBadPrefix  = errors.New("tuyalocal: unknown frame prefix")
	ErrTooLarge   = errors.New("tuyalocal: frame length over limit")
	ErrChecksum   = errors.New("tuyalocal: frame checksum or HMAC mismatch")
	ErrAuth       = errors.New("tuyalocal: GCM authentication failed")
)

// RetcodeMode says whether a received frame carries the 4-byte return code (device→client frames do,
// client→device frames do not).
type RetcodeMode int

const (
	// NoRetcode: the frame has no return code (frames written by the client).
	NoRetcode RetcodeMode = iota
	// WithRetcode: the frame has one (every frame a device sends; message_helper.unpack_message no_retcode=False).
	WithRetcode
	// DetectRetcode: 6699 discovery packets, where the code is only present when the payload does not start
	// with '{' but byte 4 does (unpack_message no_retcode=None).
	DetectRetcode
)

// Frame is one decoded message. Payload is the plaintext for 6699 frames (GCM is part of the framing) and
// still-encrypted bytes for 55AA frames (encryption is a payload concern there, see Session.Decode).
type Frame struct {
	Prefix     uint32
	Seq        uint32
	Cmd        uint32
	Retcode    uint32
	HasRetcode bool
	Payload    []byte
}

// Pack55AA builds a 55AA frame: header, [retcode], payload, CRC32 (hmacKey nil, 3.1–3.3) or
// HMAC-SHA256 (3.4), suffix (message_helper.pack_message).
func Pack55AA(seq, cmd uint32, retcode *uint32, payload, hmacKey []byte) []byte {
	body := payload
	if retcode != nil {
		body = binary.BigEndian.AppendUint32(nil, *retcode)
		body = append(body, payload...)
	}
	endLen := 8
	if hmacKey != nil {
		endLen = hmacLen + 4
	}
	out := binary.BigEndian.AppendUint32(nil, Prefix55AA)
	out = binary.BigEndian.AppendUint32(out, seq)
	out = binary.BigEndian.AppendUint32(out, cmd)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)+endLen))
	out = append(out, body...)
	if hmacKey != nil {
		out = append(out, hmacSHA256(hmacKey, out)...)
	} else {
		out = binary.BigEndian.AppendUint32(out, crc(out))
	}
	return binary.BigEndian.AppendUint32(out, Suffix55AA)
}

// Pack6699 builds a 3.5 frame: header, IV, GCM(retcode? + payload) with the header after the prefix as
// additional data, tag, suffix (message_helper.pack_message). The IV must be 12 bytes and never reused
// with the same key.
func Pack6699(seq, cmd uint32, retcode *uint32, payload, key, iv []byte) ([]byte, error) {
	if len(iv) != ivLen {
		return nil, fmt.Errorf("tuyalocal: 6699 IV must be %d bytes", ivLen)
	}
	raw := payload
	if retcode != nil {
		raw = binary.BigEndian.AppendUint32(nil, *retcode)
		raw = append(raw, payload...)
	}
	header := binary.BigEndian.AppendUint32(nil, Prefix6699)
	header = binary.BigEndian.AppendUint16(header, 0)
	header = binary.BigEndian.AppendUint32(header, seq)
	header = binary.BigEndian.AppendUint32(header, cmd)
	header = binary.BigEndian.AppendUint32(header, uint32(ivLen+len(raw)+tagLen))
	sealed, e := gcmSeal(key, iv, raw, header[4:])
	if e != nil {
		return nil, e
	}
	out := append(header, iv...)
	out = append(out, sealed...)
	return binary.BigEndian.AppendUint32(out, Suffix6699), nil
}

// FrameLength reports the total size of the frame that starts at data[0], or ErrShortFrame when the header
// is incomplete (message_helper.parse_header).
func FrameLength(data []byte) (int, error) {
	switch {
	case len(data) < 4:
		return 0, ErrShortFrame
	case bytes.Equal(data[:4], prefix55AA):
		if len(data) < header55AALen {
			return 0, ErrShortFrame
		}
		n := binary.BigEndian.Uint32(data[12:16])
		if n > MaxFrameLength {
			return 0, ErrTooLarge
		}
		return header55AALen + int(n), nil
	case bytes.Equal(data[:4], prefix6699):
		if len(data) < header6699Len {
			return 0, ErrShortFrame
		}
		n := binary.BigEndian.Uint32(data[14:18])
		if n > MaxFrameLength {
			return 0, ErrTooLarge
		}
		return header6699Len + int(n) + 4, nil
	}
	return 0, ErrBadPrefix
}

// Unpack decodes the frame at the start of data. key is the HMAC key for 55AA frames (nil = CRC32, i.e.
// 3.1–3.3) and the GCM key for 6699 frames. It returns the number of bytes consumed. Unlike tinytuya, which
// logs and carries on, a CRC or HMAC mismatch is an error: the frame is either corrupt or from the wrong key.
func Unpack(data, key []byte, mode RetcodeMode) (Frame, int, error) {
	total, e := FrameLength(data)
	if e != nil {
		return Frame{}, 0, e
	}
	if len(data) < total {
		return Frame{}, 0, ErrShortFrame
	}
	if bytes.Equal(data[:4], prefix55AA) {
		f, e := unpack55AA(data[:total], key, mode)
		return f, total, e
	}
	f, e := unpack6699(data[:total], key, mode)
	return f, total, e
}

func unpack55AA(data, key []byte, mode RetcodeMode) (Frame, error) {
	endLen := 8
	if key != nil {
		endLen = hmacLen + 4
	}
	retLen := 0
	if mode == WithRetcode {
		retLen = 4
	}
	if len(data) < header55AALen+retLen+endLen {
		return Frame{}, ErrShortFrame
	}
	f := Frame{Prefix: Prefix55AA, Seq: binary.BigEndian.Uint32(data[4:8]), Cmd: binary.BigEndian.Uint32(data[8:12])}
	signed := data[:len(data)-endLen]
	sum := data[len(data)-endLen : len(data)-4]
	if key != nil {
		if !hmac.Equal(sum, hmacSHA256(key, signed)) {
			return f, ErrChecksum
		}
	} else if binary.BigEndian.Uint32(sum) != crc(signed) {
		return f, ErrChecksum
	}
	if binary.BigEndian.Uint32(data[len(data)-4:]) != Suffix55AA {
		return f, ErrChecksum
	}
	body := data[header55AALen : len(data)-endLen]
	if retLen > 0 {
		f.Retcode, f.HasRetcode = binary.BigEndian.Uint32(body[:4]), true
		body = body[4:]
	}
	f.Payload = append([]byte{}, body...)
	return f, nil
}

func unpack6699(data, key []byte, mode RetcodeMode) (Frame, error) {
	if key == nil {
		return Frame{}, errors.New("tuyalocal: a key is required for 6699 frames")
	}
	if len(data) < header6699Len+ivLen+tagLen+4 {
		return Frame{}, ErrShortFrame
	}
	f := Frame{Prefix: Prefix6699, Seq: binary.BigEndian.Uint32(data[6:10]), Cmd: binary.BigEndian.Uint32(data[10:14])}
	if binary.BigEndian.Uint32(data[len(data)-4:]) != Suffix6699 {
		return f, ErrChecksum
	}
	iv := data[header6699Len : header6699Len+ivLen]
	plain, e := gcmOpen(key, iv, data[header6699Len+ivLen:len(data)-4], data[4:header6699Len])
	if e != nil {
		return f, ErrAuth
	}
	switch mode {
	case WithRetcode:
		if len(plain) < 4 {
			return f, ErrShortFrame
		}
		f.Retcode, f.HasRetcode, plain = binary.BigEndian.Uint32(plain[:4]), true, plain[4:]
	case DetectRetcode:
		if len(plain) > 4 && plain[0] != '{' && plain[4] == '{' {
			f.Retcode, f.HasRetcode, plain = binary.BigEndian.Uint32(plain[:4]), true, plain[4:]
		}
	}
	f.Payload = plain
	return f, nil
}

// FrameReader pulls whole frames off a stream, skipping garbage until a known prefix (XenonDevice._receive
// resync). It never buffers more than one frame (MaxFrameLength).
type FrameReader struct{ r *bufio.Reader }

// NewFrameReader wraps r.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: bufio.NewReaderSize(r, MaxFrameLength+64)}
}

// Next returns the raw bytes of the next frame.
func (fr *FrameReader) Next() ([]byte, error) {
	for {
		head, e := fr.r.Peek(4)
		if e != nil {
			return nil, e
		}
		if !bytes.Equal(head, prefix55AA) && !bytes.Equal(head, prefix6699) {
			if _, e := fr.r.Discard(1); e != nil {
				return nil, e
			}
			continue
		}
		hdr, e := fr.r.Peek(header6699Len)
		if e != nil && len(hdr) < header55AALen {
			return nil, e
		}
		total, e := FrameLength(hdr)
		if errors.Is(e, ErrTooLarge) {
			// A prefix inside garbage, or a hostile length: drop the prefix and resync.
			if _, e := fr.r.Discard(1); e != nil {
				return nil, e
			}
			continue
		}
		if e != nil {
			return nil, e
		}
		out := make([]byte, total)
		if _, e := io.ReadFull(fr.r, out); e != nil {
			return nil, e
		}
		return out, nil
	}
}
