package tuyable

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"
)

// DPType is a data-point type (const.py TuyaBLEDataPointType).
type DPType byte

const (
	DPRaw    DPType = 0
	DPBool   DPType = 1
	DPValue  DPType = 2
	DPString DPType = 3
	DPEnum   DPType = 4
	DPBitmap DPType = 5
)

func (t DPType) String() string {
	switch t {
	case DPRaw:
		return "raw"
	case DPBool:
		return "bool"
	case DPValue:
		return "value"
	case DPString:
		return "string"
	case DPEnum:
		return "enum"
	case DPBitmap:
		return "bitmap"
	}
	return "type" + strconv.Itoa(int(t))
}

// DP is one data point. Value holds, by type: []byte for raw and bitmap, bool, int64 for value (a signed 32-bit
// integer on the wire), string, and int64 for enum. An enum travels as the index into the DP's range list, not
// as its label: mapping index to label is the caller's job, using the DP spec from the Tuya import.
type DP struct {
	ID    byte
	Type  DPType
	Value any
}

// Encoded layout of one DP (the "KLV" list):
//
//	protocol 3   id(1) type(1) length(1) value
//	protocol 4   id(1) type(1) length(2) value
//
// value is a single 0/1 byte for bool, 4 bytes for value, the shortest of 1, 2 or 4 bytes for enum, and the raw
// bytes for raw, bitmap and string (tuya_ble.py TuyaBLEDataPoint._get_value).
func dpBytes(dp DP) ([]byte, error) {
	switch dp.Type {
	case DPRaw, DPBitmap:
		b, ok := dp.Value.([]byte)
		if !ok {
			return nil, fmt.Errorf("%w: DP %d (%s) needs []byte", ErrFormat, dp.ID, dp.Type)
		}
		return b, nil
	case DPBool:
		b, ok := dp.Value.(bool)
		if !ok {
			return nil, fmt.Errorf("%w: DP %d (bool) needs bool", ErrFormat, dp.ID)
		}
		if b {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case DPValue:
		v, ok := asInt(dp.Value)
		if !ok || v < math.MinInt32 || v > math.MaxInt32 {
			return nil, fmt.Errorf("%w: DP %d (value) needs a 32-bit integer", ErrFormat, dp.ID)
		}
		return binary.BigEndian.AppendUint32(nil, uint32(int32(v))), nil
	case DPEnum:
		v, ok := asInt(dp.Value)
		if !ok || v < 0 || v > math.MaxUint32 {
			return nil, fmt.Errorf("%w: DP %d (enum) needs a non-negative index", ErrFormat, dp.ID)
		}
		switch {
		case v > 0xFFFF:
			return binary.BigEndian.AppendUint32(nil, uint32(v)), nil
		case v > 0xFF:
			return binary.BigEndian.AppendUint16(nil, uint16(v)), nil
		}
		return []byte{byte(v)}, nil
	case DPString:
		s, ok := dp.Value.(string)
		if !ok {
			return nil, fmt.Errorf("%w: DP %d (string) needs string", ErrFormat, dp.ID)
		}
		return []byte(s), nil
	}
	return nil, fmt.Errorf("%w: DP %d has unknown type %d", ErrFormat, dp.ID, dp.Type)
}

// asInt accepts every integer kind and integral floats (JSON numbers decode to float64).
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), n <= math.MaxInt64
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), n <= math.MaxInt64
	case float32:
		return asInt(float64(n))
	case float64:
		if n != math.Trunc(n) || n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}

// EncodeDPs lays DPs out for a DPS (lengthSize 1, protocol 3) or DPS_V4 (lengthSize 2) request.
func EncodeDPs(dps []DP, lengthSize int) ([]byte, error) {
	if lengthSize != 1 && lengthSize != 2 {
		return nil, fmt.Errorf("%w: length size %d", ErrFormat, lengthSize)
	}
	var out []byte
	for _, dp := range dps {
		v, err := dpBytes(dp)
		if err != nil {
			return nil, err
		}
		if lengthSize == 1 && len(v) > 0xFF || len(v) > 0xFFFF {
			return nil, fmt.Errorf("%w: DP %d value of %d bytes", ErrFormat, dp.ID, len(v))
		}
		out = append(out, dp.ID, byte(dp.Type))
		if lengthSize == 1 {
			out = append(out, byte(len(v)))
		} else {
			out = binary.BigEndian.AppendUint16(out, uint16(len(v)))
		}
		out = append(out, v...)
	}
	return out, nil
}

// ParseDPs reads DPs from data[pos:] until fewer bytes than a header remain, like _parse_datapoints. It returns
// the position after the last DP. Values are read as big-endian signed integers of whatever length the device
// sent, as the reference does, and enum indices as unsigned ones; strings must be UTF-8.
func ParseDPs(data []byte, pos, lengthSize int) ([]DP, int, error) {
	if lengthSize != 1 && lengthSize != 2 {
		return nil, pos, fmt.Errorf("%w: length size %d", ErrFormat, lengthSize)
	}
	if pos < 0 || pos > len(data) {
		return nil, pos, fmt.Errorf("%w: position %d outside the data", ErrFormat, pos)
	}
	var out []DP
	for len(data)-pos >= 2+lengthSize {
		id, t := data[pos], DPType(data[pos+1])
		if t > DPBitmap {
			return out, pos, fmt.Errorf("%w: DP %d has unknown type %d", ErrFormat, id, t)
		}
		pos += 2
		n := int(data[pos])
		if lengthSize == 2 {
			n = int(binary.BigEndian.Uint16(data[pos:]))
		}
		pos += lengthSize
		if pos+n > len(data) {
			return out, pos, fmt.Errorf("%w: DP %d value runs past the frame", ErrFormat, id)
		}
		raw := data[pos : pos+n]
		pos += n
		dp := DP{ID: id, Type: t}
		switch t {
		case DPRaw, DPBitmap:
			dp.Value = bytes.Clone(raw)
		case DPBool:
			nonzero := false
			for _, b := range raw {
				nonzero = nonzero || b != 0
			}
			dp.Value = nonzero
		case DPValue, DPEnum:
			if n > 8 {
				return out, pos, fmt.Errorf("%w: DP %d integer of %d bytes", ErrFormat, id, n)
			}
			dp.Value = signedBE(raw)
			if t == DPEnum {
				if n > 4 {
					return out, pos, fmt.Errorf("%w: DP %d enum of %d bytes", ErrFormat, id, n)
				}
				// ha_tuya_ble reads enums as signed, so its own encoder's one-byte 0x80–0xFF (and two-byte
				// 0x8000–0xFFFF) indices come back negative. An index is never negative: read it unsigned.
				dp.Value = int64(unsignedBE(raw))
			}
		case DPString:
			if !utf8.Valid(raw) {
				return out, pos, fmt.Errorf("%w: DP %d string is not UTF-8", ErrFormat, id)
			}
			dp.Value = string(raw)
		}
		out = append(out, dp)
	}
	return out, pos, nil
}

func unsignedBE(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func signedBE(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	var v int64
	if b[0]&0x80 != 0 {
		v = -1
	}
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	return v
}

// ParseTimestamp reads a device timestamp at pos: type 0 is 13 ASCII digits of Unix milliseconds, type 1 is a
// 4-byte big-endian Unix second count (tuya_ble.py _parse_timestamp).
func ParseTimestamp(data []byte, pos int) (time.Time, int, error) {
	if pos < 0 || pos >= len(data) {
		return time.Time{}, pos, fmt.Errorf("%w: missing timestamp", ErrFormat)
	}
	switch data[pos] {
	case 0:
		if pos+14 > len(data) {
			return time.Time{}, pos, fmt.Errorf("%w: short timestamp", ErrFormat)
		}
		ms, err := strconv.ParseInt(string(data[pos+1:pos+14]), 10, 64)
		if err != nil || ms < 0 {
			return time.Time{}, pos, fmt.Errorf("%w: timestamp digits", ErrFormat)
		}
		return time.UnixMilli(ms), pos + 14, nil
	case 1:
		if pos+5 > len(data) {
			return time.Time{}, pos, fmt.Errorf("%w: short timestamp", ErrFormat)
		}
		return time.Unix(int64(binary.BigEndian.Uint32(data[pos+1:])), 0), pos + 5, nil
	}
	return time.Time{}, pos, fmt.Errorf("%w: timestamp type %d", ErrFormat, data[pos])
}

// Time1Answer and Time2Answer build the replies to the device's clock requests. TIME1 wants Unix milliseconds as
// ASCII digits; TIME2 wants year%100, month, day, hour, minute, second and weekday (Monday = 0, as Python's
// tm_wday) in local time. Both end with the UTC offset in hundredths of an hour as a signed 16-bit integer
// (UTC+7 is 700).
//
// The offset is now's actual offset, daylight saving time included. The reference sends -time.timezone/36, the
// standard offset without DST, so the two differ by 100 in DST zones; Thailand has no DST.
func Time1Answer(now time.Time) []byte {
	_, off := now.Zone()
	return binary.BigEndian.AppendUint16([]byte(strconv.FormatInt(now.UnixMilli(), 10)), uint16(int16(off/36)))
}

func Time2Answer(now time.Time) []byte {
	_, off := now.Zone()
	b := []byte{byte(now.Year() % 100), byte(now.Month()), byte(now.Day()), byte(now.Hour()), byte(now.Minute()),
		byte(now.Second()), byte((int(now.Weekday()) + 6) % 7)}
	return binary.BigEndian.AppendUint16(b, uint16(int16(off/36)))
}
