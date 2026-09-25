package tuyalocal

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"
)

// Version is a Tuya local protocol version.
type Version int

const (
	V31 Version = 31
	V32 Version = 32 // behaves like 3.3 in "device22" mode (XenonDevice.set_version)
	V33 Version = 33
	V34 Version = 34
	V35 Version = 35
)

// ParseVersion accepts "3.1" … "3.5" (and the float forms devices put in discovery broadcasts).
func ParseVersion(s string) (Version, error) {
	switch s {
	case "3.1":
		return V31, nil
	case "3.2":
		return V32, nil
	case "3.3":
		return V33, nil
	case "3.4":
		return V34, nil
	case "3.5":
		return V35, nil
	}
	return 0, fmt.Errorf("tuyalocal: unsupported protocol version %q", s)
}

func (v Version) String() string { return fmt.Sprintf("%d.%d", int(v)/10, int(v)%10) }

func (v Version) bytes() []byte { return []byte(v.String()) }

// header is the 15-byte version header "3.x" + 12 zero bytes (header.py PROTOCOL_3x_HEADER).
func (v Version) header() []byte { return append(v.bytes(), make([]byte, 12)...) }

// noHeaderCmds never carry the version header (header.py NO_PROTOCOL_HEADER_CMDS).
var noHeaderCmds = []uint32{CmdDPQuery, CmdDPQueryNew, CmdUpdateDPS, CmdHeartBeat, CmdSessKeyNegStart, CmdSessKeyNegResp, CmdSessKeyNegFinish, CmdLANExtStream}

// ErrDevice22 is returned when a 3.3/3.4 device answers "data unvalid": it is a "device22" and wants
// DP_QUERY sent as CONTROL_NEW with an explicit list of DPs (XenonDevice._decode_payload).
var ErrDevice22 = errors.New("tuyalocal: device requires device22 mode")

// ErrKeySuspect means a 3.1–3.3 payload did not decrypt with our key. On those versions there is no
// negotiation, so this is the only sign of a wrong local key, and a heuristic one.
var ErrKeySuspect = errors.New("tuyalocal: payload did not decrypt with the local key")

// Role says which end of the connection a Session plays. Devices add a return code to every frame they
// send; clients never do.
type Role int

const (
	Client Role = iota
	Device
)

// Session encodes and decodes the payload layer for one connection: version header, per-version
// encryption and the frame sequence number. It is not safe for concurrent use; Conn serialises access.
type Session struct {
	Version  Version
	DeviceID string
	Role     Role
	// Device22 selects the device22 DP_QUERY form (a CONTROL_NEW with explicit DPs).
	Device22 bool
	// DPsToRequest are the DPs a device22 query names ({"1": null} by default).
	DPsToRequest []string
	// Now and IV are injectable for deterministic test vectors.
	Now func() time.Time
	IV  func() []byte

	realKey []byte
	key     []byte // realKey until a 3.4/3.5 session key is negotiated
	seq     uint32
}

// NewSession creates a session. localKey is the 16-character key from the Tuya cloud.
func NewSession(v Version, deviceID, localKey string, role Role) (*Session, error) {
	if len(localKey) != 16 {
		return nil, errors.New("tuyalocal: local key must be 16 characters")
	}
	s := &Session{Version: v, DeviceID: deviceID, Role: role, realKey: []byte(localKey), seq: 1, Now: time.Now, IV: randomIV}
	s.key = s.realKey
	if v == V32 {
		s.Device22 = true
	}
	return s, nil
}

func randomIV() []byte {
	iv := make([]byte, ivLen)
	if _, e := rand.Read(iv); e != nil {
		panic(e)
	}
	return iv
}

// SetSessionKey switches to a negotiated 3.4/3.5 session key.
func (s *Session) SetSessionKey(k []byte) { s.key = append([]byte{}, k...) }

// ResetKey returns to the real local key (a new TCP connection renegotiates from scratch).
func (s *Session) ResetKey() { s.key = s.realKey }

// Seq returns the next sequence number the session will use.
func (s *Session) Seq() uint32 { return s.seq }

// SetSeq sets the next sequence number (the simulator's 3.5 device keeps a global counter).
func (s *Session) SetSeq(n uint32) { s.seq = n }

// Key returns the key currently used for frames (for tests).
func (s *Session) Key() []byte { return s.key }

// hmacKey is the 55AA footer key: none (CRC32) before 3.4.
func (s *Session) hmacKey() []byte {
	if s.Version >= V34 {
		return s.key
	}
	return nil
}

// Encode wraps payload for cmd into a frame (XenonDevice._encode_message; the device role mirrors it with a
// return code and signs 3.1 STATUS pushes the way clients sign CONTROL).
func (s *Session) Encode(cmd uint32, payload []byte) ([]byte, error) {
	var retcode *uint32
	if s.Role == Device {
		zero := uint32(0)
		retcode = &zero
	}
	seq := s.seq
	s.seq++
	switch {
	case s.Version >= V34:
		if !slices.Contains(noHeaderCmds, cmd) {
			payload = append(s.Version.header(), payload...)
		}
		if s.Version == V35 {
			return Pack6699(seq, cmd, retcode, payload, s.key, s.IV())
		}
		ct, e := ecbEncrypt(s.key, payload, true)
		if e != nil {
			return nil, e
		}
		return Pack55AA(seq, cmd, retcode, ct, s.hmacKey()), nil
	case s.Version >= V32:
		ct, e := ecbEncrypt(s.key, payload, true)
		if e != nil {
			return nil, e
		}
		if !slices.Contains(noHeaderCmds, cmd) {
			ct = append(s.Version.header(), ct...)
		}
		return Pack55AA(seq, cmd, retcode, ct, nil), nil
	default: // 3.1: only CONTROL (and a device's STATUS push) is encrypted and signed
		if (s.Role == Client && cmd == CmdControl) || (s.Role == Device && cmd == CmdStatus) {
			signed, e := s.sign31(payload)
			if e != nil {
				return nil, e
			}
			payload = signed
		}
		return Pack55AA(seq, cmd, retcode, payload, nil), nil
	}
}

// sign31 is the 3.1 CONTROL form: "3.1" + md5("data="+b64+"||lpv=3.1||"+key).hex()[8:24] + b64, where b64 is
// base64(AES-ECB(key, payload)) (XenonDevice._encode_message).
func (s *Session) sign31(payload []byte) ([]byte, error) {
	ct, e := ecbEncrypt(s.key, payload, true)
	if e != nil {
		return nil, e
	}
	b64 := []byte(base64.StdEncoding.EncodeToString(ct))
	pre := append([]byte("data="), b64...)
	pre = append(pre, []byte("||lpv=3.1||")...)
	pre = append(pre, s.key...)
	sum := md5.Sum(pre)
	out := append([]byte("3.1"), []byte(hex.EncodeToString(sum[:])[8:24])...)
	return append(out, b64...), nil
}

// Unpack decodes the next frame for this session's direction: devices' frames carry a return code.
func (s *Session) Unpack(data []byte) (Frame, int, error) {
	mode := NoRetcode
	if s.Role == Client {
		mode = WithRetcode
	}
	key := s.hmacKey()
	if s.Version == V35 {
		key = s.key
	}
	return Unpack(data, key, mode)
}

// Decode returns the plaintext JSON of a frame's payload, or nil for an empty payload
// (XenonDevice._decode_payload).
func (s *Session) Decode(f Frame) ([]byte, error) {
	p := f.Payload
	if len(p) == 0 {
		return nil, nil
	}
	if s.Version == V34 {
		plain, e := ecbDecrypt(s.key, p, true)
		if e != nil {
			return nil, ErrKeySuspect
		}
		p = plain
	}
	switch {
	case s.Version == V31 && bytes.HasPrefix(p, V31.bytes()):
		// 3.1 signed form: version, 16 md5 hex chars, base64 ciphertext.
		if len(p) < 3+16 {
			return nil, ErrKeySuspect
		}
		ct, e := base64.StdEncoding.DecodeString(string(p[3+16:]))
		if e != nil {
			return nil, ErrKeySuspect
		}
		plain, e := ecbDecrypt(s.key, ct, true)
		if e != nil {
			return nil, ErrKeySuspect
		}
		p = plain
	case s.Version >= V32:
		if bytes.HasPrefix(p, s.Version.bytes()) {
			p = p[len(s.Version.header()):]
		} else if s.Device22 && len(p)&0x0F != 0 && len(p) >= len(s.Version.header()) {
			p = p[len(s.Version.header()):]
		}
		if s.Version < V34 {
			plain, e := ecbDecrypt(s.key, p, true)
			if e != nil {
				return nil, ErrKeySuspect
			}
			p = plain
		}
		if s.Role == Client && (s.Version == V33 || s.Version == V34) && bytes.Contains(p, []byte("data unvalid")) {
			return nil, ErrDevice22
		}
	default:
		if len(p) > 0 && p[0] != '{' {
			return nil, fmt.Errorf("tuyalocal: unexpected 3.1 payload")
		}
	}
	return p, nil
}

// t is the timestamp field: a string on 3.1–3.3 ("t": ""), an integer on 3.4/3.5 ("t": "int").
func (s *Session) t() any {
	now := s.Now().Unix()
	if s.Version >= V34 {
		return now
	}
	return strconv.FormatInt(now, 10)
}

// DPQuery returns the command and payload that ask for every DP (payload_dict DP_QUERY per version and
// device type).
func (s *Session) DPQuery() (uint32, []byte) {
	if s.Device22 {
		dps := s.DPsToRequest
		if len(dps) == 0 {
			dps = []string{"1"}
		}
		req := map[string]any{}
		for _, d := range dps {
			req[d] = nil
		}
		return CmdControlNew, object("devId", s.DeviceID, "uid", s.DeviceID, "t", s.t(), "dps", dpsObject(req))
	}
	if s.Version >= V34 {
		return CmdDPQueryNew, []byte("{}")
	}
	return CmdDPQuery, object("gwId", s.DeviceID, "devId", s.DeviceID, "uid", s.DeviceID, "t", s.t())
}

// Control returns the command and payload that set DPs (keys are DP ids).
func (s *Session) Control(dps map[string]any) (uint32, []byte) {
	if s.Version >= V34 {
		return CmdControlNew, object("protocol", 5, "t", s.t(), "data", rawJSON(object("dps", dpsObject(dps))))
	}
	return CmdControl, object("devId", s.DeviceID, "uid", s.DeviceID, "t", s.t(), "dps", dpsObject(dps))
}

// Heartbeat returns the keep-alive command.
func (s *Session) Heartbeat() (uint32, []byte) {
	return CmdHeartBeat, object("gwId", s.DeviceID, "devId", s.DeviceID)
}

// UpdateDPS asks the device to refresh (and push) the given DPs, e.g. metering values that are only sent on
// request. tinytuya's default list is 18, 19, 20 (payload_dict UPDATEDPS).
func (s *Session) UpdateDPS(ids []int) (uint32, []byte) {
	if len(ids) == 0 {
		ids = []int{18, 19, 20}
	}
	return CmdUpdateDPS, object("dpId", ids)
}

// rawJSON marks an already-encoded value for object().
type rawJSON []byte

// object encodes key/value pairs as a compact JSON object in the given order. tinytuya sends Python dicts
// in insertion order with no spaces ("if spaces are not removed device does not respond!"); we keep the same
// order and compactness, but do not strip spaces inside string values as tinytuya's replace(" ", "") does.
func object(kv ...any) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv[i].(string))
		b.Write(k)
		b.WriteByte(':')
		switch v := kv[i+1].(type) {
		case rawJSON:
			b.Write(v)
		default:
			b.Write(encode(v))
		}
	}
	b.WriteByte('}')
	return b.Bytes()
}

// dpsObject encodes DPs with numeric keys in numeric order.
func dpsObject(dps map[string]any) rawJSON {
	keys := make([]string, 0, len(dps))
	for k := range dps {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, ea := strconv.Atoi(keys[i])
		b, eb := strconv.Atoi(keys[j])
		if ea == nil && eb == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	kv := make([]any, 0, 2*len(keys))
	for _, k := range keys {
		kv = append(kv, k, dps[k])
	}
	return rawJSON(object(kv...))
}

func encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if e := enc.Encode(v); e != nil {
		return []byte("null")
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

// ParseDPS extracts the DPs from a decoded payload: {"dps":{…}} on 3.1–3.3 and {"data":{"dps":{…}}} on
// 3.4/3.5 (XenonDevice._decode_payload). ok is false when the payload carries no DPs (acks, heartbeats).
func ParseDPS(payload []byte) (map[string]any, bool, error) {
	if len(payload) == 0 {
		return nil, false, nil
	}
	var m struct {
		DPS  map[string]any `json:"dps"`
		Data struct {
			DPS map[string]any `json:"dps"`
		} `json:"data"`
	}
	d := json.NewDecoder(bytes.NewReader(payload))
	d.UseNumber()
	if e := d.Decode(&m); e != nil {
		return nil, false, fmt.Errorf("tuyalocal: payload is not JSON: %w", e)
	}
	if m.DPS != nil {
		return m.DPS, true, nil
	}
	if m.Data.DPS != nil {
		return m.Data.DPS, true, nil
	}
	return nil, false, nil
}
