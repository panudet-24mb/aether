// Package edge reads what an Aether Edge agent publishes. The agent runs on a site host next to Tuya Wi-Fi
// devices, talks to each one over the LAN with its local key (internal/tuyalocal) and connects OUT to this
// broker with its gateway's own account, publishing under aether/edge/<gateway id> (docs/platform/aether-edge.md).
// It moves raw Tuya data points only: {"dps":{"1":true,"19":1234}}. What a data point means (its code, type and
// scale) is known only here, from the device's specification imported once (adapters/tuya).
package edge

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"bytes"
	"encoding/json"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Prefix is the topic root every Aether Edge publishes under; the gateway id follows it.
const Prefix = "aether/edge/"

// FrameState names the decoder of an Edge (Tuya local) state message in stored samples. It is always stored next to
// the generic zigbee2mqtt.FrameState, so readings built from a definition are handled alike everywhere.
const FrameState = "tuya-local@1"

// Bounds of what one message may carry.
const (
	// MaxPacket caps any Edge message; the discovery list is the largest.
	MaxPacket = 256 * 1024
	// MaxLAN caps one discovery list (devices seen on the LAN).
	MaxLAN = 500
	// MaxDPS caps the data points of one state message (Tuya ids are 1..255).
	MaxDPS = 255
	// MaxBLE caps one Bluetooth sightings list (Tuya BLE devices heard advertising).
	MaxBLE = 200
)

var devicePattern = regexp.MustCompile(`^[a-z0-9]{16,32}$`)

// ValidDevice reports a Tuya device id as Aether stores it: 16 to 32 lower-case letters and digits.
func ValidDevice(s string) bool { return devicePattern.MatchString(s) }

type Kind int

const (
	Ignore Kind = iota
	Status
	Health
	Discovery
	State
	Availability
	// BLESightings is the agent's list of Tuya BLE devices it hears advertising (aether/edge/<gw>/ble).
	BLESightings
)

// Message is one routed topic. Device is the Tuya device id for State and Availability.
type Message struct {
	Kind   Kind
	Topic  string
	Device string
}

// Route splits a topic under Prefix into its gateway id and message kind. Commands going TO the agent (…/set)
// and anything Aether does not consume are Ignore; a topic outside the tree, with a malformed gateway id or a
// malformed device id is forbidden.
func Route(topic string) (string, Message, error) {
	rest, ok := strings.CutPrefix(topic, Prefix)
	if !ok {
		return "", Message{}, domain.ErrForbidden
	}
	gateway, rest, _ := strings.Cut(rest, "/")
	if !security.ValidID(gateway) || rest == "" {
		return "", Message{}, domain.ErrForbidden
	}
	m := Message{Topic: topic}
	switch rest {
	case "status":
		m.Kind = Status
		return gateway, m, nil
	case "health":
		m.Kind = Health
		return gateway, m, nil
	case "discovery":
		m.Kind = Discovery
		return gateway, m, nil
	case "ble":
		m.Kind = BLESightings
		return gateway, m, nil
	}
	device, leaf, found := strings.Cut(rest, "/")
	if !found || strings.Contains(leaf, "/") {
		return gateway, m, nil // not a topic Aether consumes
	}
	switch leaf {
	case "state":
		m.Kind = State
	case "availability":
		m.Kind = Availability
	default:
		return gateway, m, nil // …/set and anything else going to the agent
	}
	if !ValidDevice(device) {
		return "", Message{}, domain.ErrForbidden
	}
	m.Device = device
	return gateway, m, nil
}

// SetTopic is the topic a command for one device of one Edge is published to.
func SetTopic(gatewayID, device string) (string, error) {
	if !security.ValidID(gatewayID) || !ValidDevice(device) {
		return "", domain.ErrInvalid
	}
	return Prefix + gatewayID + "/" + device + "/set", nil
}

// SetPayload checks the wire form of a command ({"dps":{"<id>":<raw>}}, computed when the command was queued) and
// returns it compacted.
func SetPayload(wire json.RawMessage) ([]byte, error) {
	if len(wire) == 0 || len(wire) > 1024 {
		return nil, domain.ErrInvalid
	}
	dps, e := ParseDPS(wire)
	if e != nil || len(dps) == 0 {
		return nil, domain.ErrInvalid
	}
	var buf bytes.Buffer
	if json.Compact(&buf, wire) != nil {
		return nil, domain.ErrInvalid
	}
	return buf.Bytes(), nil
}

// SetDeadline adds to a command's wire form until when the agent may deliver it: "expires_at" (server time,
// RFC 3339 with milliseconds) and "ttl_ms" (the same, relative to publishing). The agent takes the earlier of
// expires_at and its own receive time plus ttl_ms, so neither a slow broker nor a host clock that runs behind can make
// it actuate after the server gave up; a host clock that runs ahead makes it drop commands (it logs so). Agents
// before 0.2 ignore both fields.
func SetDeadline(payload []byte, now time.Time, ttl time.Duration) ([]byte, error) {
	var obj struct {
		DPS json.RawMessage `json:"dps"`
	}
	if json.Unmarshal(payload, &obj) != nil || len(obj.DPS) == 0 || ttl <= 0 {
		return nil, domain.ErrInvalid
	}
	return json.Marshal(struct {
		DPS       json.RawMessage `json:"dps"`
		ExpiresAt string          `json:"expires_at"`
		TTLMS     int64           `json:"ttl_ms"`
	}{obj.DPS, now.Add(ttl).UTC().Format("2006-01-02T15:04:05.000Z07:00"), ttl.Milliseconds()})
}

// ParseOnline reads a status or availability payload: {"state":"online"} or the bare string.
func ParseOnline(b []byte) (online bool, reason string, ok bool) {
	var obj struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}
	s := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &obj) == nil && obj.State != "" {
		s = obj.State
	} else {
		var quoted string
		if json.Unmarshal(b, &quoted) == nil {
			s = quoted
		}
	}
	reason = obj.Reason
	if !reasons[reason] {
		reason = ""
	}
	switch strings.ToLower(s) {
	case "online":
		return true, "", true
	case "offline":
		return false, reason, true
	}
	return false, "", false
}

// Why a device is unreachable, as the agent reports it.
const (
	ReasonUnreachable = "unreachable" // no TCP connection (off, out of range, moved IP)
	ReasonAuthFailed  = "auth_failed" // 3.4/3.5 session negotiation proved the local key wrong
	ReasonKeySuspect  = "key_suspect" // 3.1/3.3: replies do not decrypt with the key
	ReasonBusy        = "busy"        // the device resets our socket: another local client holds its only one
	ReasonNotFound    = "not_found"   // the device is not seen on the LAN at all
)

var reasons = map[string]bool{ReasonUnreachable: true, ReasonAuthFailed: true, ReasonKeySuspect: true, ReasonBusy: true, ReasonNotFound: true}

// HealthReport is the agent's periodic heartbeat. BLE is present only from agents that know about Bluetooth.
type HealthReport struct {
	Version          string     `json:"version"`
	DevicesConnected int        `json:"devices_connected"`
	LANSeen          int        `json:"lan_seen"`
	BLE              *BLEHealth `json:"ble,omitempty"`
}

// BLEHealth is the Bluetooth part of the heartbeat.
type BLEHealth struct {
	State          string `json:"state"` // off | ok | no_adapter | no_permission | error
	Adapter        string `json:"adapter"`
	Seen           int    `json:"seen"`
	Connected      int    `json:"connected"`
	SessionsOK     int    `json:"sessions_ok"`
	SessionsFailed int    `json:"sessions_failed"`
	Queue          int    `json:"queue"`
}

var bleStates = map[string]bool{"off": true, "ok": true, "no_adapter": true, "no_permission": true, "error": true}

// ParseHealth reads the heartbeat; unknown fields are ignored and strings bounded.
func ParseHealth(b []byte) (HealthReport, bool) {
	var h HealthReport
	if json.Unmarshal(b, &h) != nil {
		return HealthReport{}, false
	}
	h.Version = clip(h.Version, 32)
	if h.DevicesConnected < 0 || h.DevicesConnected > 100000 {
		h.DevicesConnected = 0
	}
	if h.LANSeen < 0 || h.LANSeen > 100000 {
		h.LANSeen = 0
	}
	if b := h.BLE; b != nil {
		if !bleStates[b.State] {
			b.State = "error"
		}
		b.Adapter = clip(b.Adapter, 16)
		b.Seen, b.Connected = bound(b.Seen, 100000), bound(b.Connected, 100)
		b.SessionsOK, b.SessionsFailed, b.Queue = bound(b.SessionsOK, 1<<30), bound(b.SessionsFailed, 1<<30), bound(b.Queue, 100000)
	}
	return h, true
}

func bound(n, max int) int {
	if n < 0 || n > max {
		return 0
	}
	return n
}

// BLESighting is one Tuya BLE device the agent heard advertising. It carries what the advertisement says without
// a connection: the address, the device uuid (decrypted with the product id), the product, the protocol version,
// whether it is bound to an account, whether it uses the FD50 service, and the signal strength.
type BLESighting struct {
	MAC       string `json:"mac"`
	UUID      string `json:"uuid"`
	ProductID string `json:"product_id"`
	Protocol  int    `json:"proto"`
	Bound     bool   `json:"bound"`
	FD50      bool   `json:"fd50"`
	RSSI      *int   `json:"rssi"`
}

var (
	macPattern   = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9]{0,64}$`)
)

// ValidMAC reports a Bluetooth address as Aether stores it: lower-case hex pairs joined by colons.
func ValidMAC(s string) bool { return macPattern.MatchString(s) }

// NormalMAC returns a Bluetooth address in Aether's form (aa:bb:cc:dd:ee:ff), or "" when it is not one. Upper case,
// dashes, colons or no separators at all (as Tuya's factory records write it) are accepted. An address of one
// repeated byte (00:00:00:00:00:00, ff:ff:…) is a placeholder, not a device's: it counts as unknown.
func NormalMAC(s string) string {
	s = strings.NewReplacer("-", "", ":", "").Replace(strings.ToLower(strings.TrimSpace(s)))
	if len(s) != 12 || s == strings.Repeat(s[:2], 6) {
		return ""
	}
	out := make([]string, 6)
	for i := range out {
		out[i] = s[2*i : 2*i+2]
	}
	if m := strings.Join(out, ":"); ValidMAC(m) {
		return m
	}
	return ""
}

// ReversedMAC is a Bluetooth address with its bytes in the other order: some Tuya products' factory records list
// the address the device advertises from byte-reversed, so a match accepts either order.
func ReversedMAC(m string) string {
	parts := strings.Split(m, ":")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ":")
}

// ParseBLESightings reads the agent's Bluetooth sightings list. Malformed entries are skipped (an address that is
// not one, a uuid or product id with other than letters and digits, an impossible protocol); a signal strength out
// of range is dropped, not the entry. At most MaxBLE are returned, each address once.
func ParseBLESightings(b []byte) ([]BLESighting, error) {
	var raw []BLESighting
	if e := json.Unmarshal(b, &raw); e != nil {
		return nil, domain.ErrInvalid
	}
	out := []BLESighting{}
	seen := map[string]bool{}
	for _, s := range raw {
		s.MAC = strings.ToLower(strings.TrimSpace(s.MAC))
		if !ValidMAC(s.MAC) || seen[s.MAC] || !tokenPattern.MatchString(s.UUID) || !tokenPattern.MatchString(s.ProductID) || s.Protocol < 0 || s.Protocol > 15 {
			continue
		}
		if s.RSSI != nil && (*s.RSSI < -127 || *s.RSSI > 20) {
			s.RSSI = nil
		}
		seen[s.MAC] = true
		out = append(out, s)
		if len(out) == MaxBLE {
			break
		}
	}
	return out, nil
}

// LANDevice is one Tuya device the agent saw broadcasting on its LAN.
type LANDevice struct {
	ID         string `json:"id"`
	IP         string `json:"ip"`
	Version    string `json:"version"`
	ProductKey string `json:"product_key"`
}

var versions = map[string]bool{"3.1": true, "3.2": true, "3.3": true, "3.4": true, "3.5": true}

// ValidVersion reports a Tuya local protocol version Aether knows.
func ValidVersion(v string) bool { return versions[v] }

// ParseDiscovery reads the list of devices seen on the LAN. Malformed entries are skipped; at most MaxLAN are
// returned.
func ParseDiscovery(b []byte) ([]LANDevice, error) {
	var raw []LANDevice
	if e := json.Unmarshal(b, &raw); e != nil {
		return nil, domain.ErrInvalid
	}
	out := []LANDevice{}
	seen := map[string]bool{}
	for _, d := range raw {
		d.ID = strings.ToLower(strings.TrimSpace(d.ID))
		ip := net.ParseIP(strings.TrimSpace(d.IP))
		if !ValidDevice(d.ID) || ip == nil || ip.To4() == nil || !versions[d.Version] || seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		d.IP, d.ProductKey = ip.String(), clip(d.ProductKey, 64)
		out = append(out, d)
		if len(out) == MaxLAN {
			break
		}
	}
	return out, nil
}

// ParseState reads a state message: {"dps":{"1":true,"19":1234},"full":false}. Keys must be data point ids
// (1..255); values are kept raw (bounded). full says the message is a complete status, not a partial push.
func ParseState(b []byte) (map[int]json.RawMessage, bool, error) {
	var obj struct {
		DPS  json.RawMessage `json:"dps"`
		Full bool            `json:"full"`
	}
	if json.Unmarshal(b, &obj) != nil || len(obj.DPS) == 0 {
		return nil, false, domain.ErrInvalid
	}
	dps, e := ParseDPS(b)
	return dps, obj.Full, e
}

// ParseDPS reads the dps object of a state message or a command's wire form.
func ParseDPS(b []byte) (map[int]json.RawMessage, error) {
	var obj struct {
		DPS map[string]json.RawMessage `json:"dps"`
	}
	if json.Unmarshal(b, &obj) != nil || len(obj.DPS) > MaxDPS {
		return nil, domain.ErrInvalid
	}
	out := make(map[int]json.RawMessage, len(obj.DPS))
	for k, v := range obj.DPS {
		id, e := strconv.Atoi(k)
		if e != nil || id < 1 || id > 255 || strconv.Itoa(id) != k || len(v) == 0 || len(v) > 1024 || !json.Valid(v) {
			return nil, domain.ErrInvalid
		}
		out[id] = v
	}
	return out, nil
}

// Clip trims, drops NUL (PostgreSQL text cannot hold it), replaces invalid UTF-8 and bounds a string to n bytes
// without splitting a character: a cut in the middle of a rune would make the database refuse the whole message.
func Clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

func clip(s string, n int) string { return Clip(s, n) }
