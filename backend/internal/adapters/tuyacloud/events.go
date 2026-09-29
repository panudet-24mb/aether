package tuyacloud

// Business messages of the Tuya Message Service, after decryption. Two families exist and a project may receive
// both (Tuya warns that configuring the legacy and the IoT Core protocols together "might cause duplicate
// reception"; the ingest de-duplicates by the per-code report time):
//
//   - legacy: protocol 4 statusReport {dataId, devId, productKey, status:[{code, value, t, "<dpid>": "<text>"}]}
//     and protocol 20 device events {devId, productKey, bizCode, bizData, ts};
//   - IoT Core: protocol 1000 {bizCode:"devicePropertyMessage", bizData:{devId, productId,
//     properties:[{code, dpId, time, value}]}, ts} and protocol 1001 device events {bizCode, bizData:{devId, ...}, ts}.
//
// Source: Tuya "Message Types" (developer.tuya.com/en/docs/iot/message-type?id=Kavqerli65a1u), read 2026-09-30.
// Nothing here trusts the sender beyond shape: device ids, codes, value sizes and counts are bounded, and an event
// never names a device Aether does not already know (the ingest checks that).

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/tuya"
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Protocol numbers Aether understands.
const (
	ProtocolStatus        = 4
	ProtocolDevice        = 20
	ProtocolProperty      = 1000
	ProtocolDeviceIoTCore = 1001
)

// MaxValue bounds one reported value (JSON).
const MaxValue = 1 << 10

// ErrUnknownProtocol is a message of a protocol Aether does not use (firmware upgrades, scenes, ...): acknowledge
// it and count it, it is not an error.
var ErrUnknownProtocol = errors.New("tuya mq: protocol not handled")

// EventKind is what a message says happened to a device.
type EventKind int

const (
	EventStatus EventKind = iota + 1
	EventOnline
	EventOffline
	EventRenamed
	EventRemoved
	EventBound
)

// Item is one reported data point: its code, its id when the message carries it (0 otherwise), the value as Tuya
// sent it, and the device's report time in ms (0 when absent).
type Item struct {
	Code  string
	DPID  int
	Value json.RawMessage
	T     int64
}

// Event is one thing that happened to one device.
type Event struct {
	Kind      EventKind
	Protocol  int
	DevID     string // lower case, as Aether stores Tuya ids
	ProductID string
	Items     []Item // EventStatus only
	Name      string // EventRenamed only
	T         int64  // message time in ms (ts / bizData.time), 0 when absent
}

// Parse reads one decrypted business message. Known protocols with an event Aether does not use (dpNameUpdate,
// upgradeStatus, deviceEventMessage, ...) return no events and no error; unknown protocols return
// ErrUnknownProtocol; anything malformed or out of bounds returns ErrFrame.
func Parse(protocol int, plaintext []byte) ([]Event, error) {
	if len(plaintext) > MaxData || !utf8.Valid(plaintext) || bytes.IndexByte(plaintext, 0) >= 0 {
		return nil, ErrFrame
	}
	switch protocol {
	case ProtocolStatus:
		return parseStatus(plaintext)
	case ProtocolDevice:
		return parseLegacyDevice(plaintext)
	case ProtocolProperty:
		return parseProperty(plaintext)
	case ProtocolDeviceIoTCore:
		return parseIoTCoreDevice(plaintext)
	}
	return nil, ErrUnknownProtocol
}

func devID(raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	return id, edge.ValidDevice(id)
}

// parseStatus reads protocol 4. The data point id is the one numeric key of a status item ("1":"false").
func parseStatus(b []byte) ([]Event, error) {
	var m struct {
		DevID      string                       `json:"devId"`
		ProductKey string                       `json:"productKey"`
		Status     []map[string]json.RawMessage `json:"status"`
	}
	if json.Unmarshal(b, &m) != nil {
		return nil, ErrFrame
	}
	id, ok := devID(m.DevID)
	if !ok || len(m.Status) == 0 || len(m.Status) > tuya.MaxDPs {
		return nil, ErrFrame
	}
	ev := Event{Kind: EventStatus, Protocol: ProtocolStatus, DevID: id, ProductID: clipID(m.ProductKey)}
	for _, s := range m.Status {
		var code string
		if json.Unmarshal(s["code"], &code) != nil {
			return nil, ErrFrame
		}
		it := Item{Code: code, Value: s["value"]}
		if t, ok := intField(s["t"]); ok {
			it.T = t
		}
		for k := range s {
			if n, e := strconv.Atoi(k); e == nil && n >= 1 && n <= 255 {
				it.DPID = n
			}
		}
		if !validItem(it) {
			return nil, ErrFrame
		}
		ev.Items = append(ev.Items, it)
		if it.T > ev.T {
			ev.T = it.T
		}
	}
	return []Event{ev}, nil
}

type bizMessage struct {
	DevID      string          `json:"devId"`
	ProductKey string          `json:"productKey"`
	BizCode    string          `json:"bizCode"`
	BizData    json.RawMessage `json:"bizData"`
	TS         json.RawMessage `json:"ts"`
}

type bizData struct {
	DevID      string            `json:"devId"`
	ProductID  string            `json:"productId"`
	Name       string            `json:"name"`
	Time       json.RawMessage   `json:"time"`
	Properties []json.RawMessage `json:"properties"`
}

func readBiz(b []byte) (bizMessage, bizData, error) {
	var m bizMessage
	var d bizData
	if json.Unmarshal(b, &m) != nil || len(m.BizCode) > 64 {
		return m, d, ErrFrame
	}
	if len(m.BizData) > 0 && !bytes.Equal(bytes.TrimSpace(m.BizData), []byte("null")) && json.Unmarshal(m.BizData, &d) != nil {
		return m, d, ErrFrame
	}
	return m, d, nil
}

// parseLegacyDevice reads protocol 20: online, offline, nameUpdate, delete and bindUser. dpNameUpdate,
// upgradeStatus and anything else are known but unused.
func parseLegacyDevice(b []byte) ([]Event, error) {
	m, d, e := readBiz(b)
	if e != nil {
		return nil, e
	}
	kind := map[string]EventKind{"online": EventOnline, "offline": EventOffline, "nameUpdate": EventRenamed, "delete": EventRemoved, "bindUser": EventBound}[m.BizCode]
	if kind == 0 {
		return []Event{}, nil
	}
	raw := m.DevID
	if raw == "" {
		raw = d.DevID
	}
	id, ok := devID(raw)
	if !ok {
		return nil, ErrFrame
	}
	ev := Event{Kind: kind, Protocol: ProtocolDevice, DevID: id, ProductID: clipID(m.ProductKey)}
	ev.T = eventTime(m.TS, d.Time)
	if kind == EventRenamed {
		if ev.Name, ok = cleanName(d.Name); !ok {
			return nil, ErrFrame
		}
	}
	return []Event{ev}, nil
}

// parseProperty reads protocol 1000. Only devicePropertyMessage is used; deviceEventMessage (device-defined
// events with outputParams) is known but unused.
func parseProperty(b []byte) ([]Event, error) {
	m, d, e := readBiz(b)
	if e != nil {
		return nil, e
	}
	if m.BizCode != "devicePropertyMessage" {
		return []Event{}, nil
	}
	id, ok := devID(d.DevID)
	if !ok || len(d.Properties) == 0 || len(d.Properties) > tuya.MaxDPs {
		return nil, ErrFrame
	}
	ev := Event{Kind: EventStatus, Protocol: ProtocolProperty, DevID: id, ProductID: clipID(d.ProductID), T: eventTime(m.TS, nil)}
	for _, raw := range d.Properties {
		var p struct {
			Code  string          `json:"code"`
			DPID  json.RawMessage `json:"dpId"`
			Time  json.RawMessage `json:"time"`
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return nil, ErrFrame
		}
		it := Item{Code: p.Code, Value: p.Value}
		if n, ok := intField(p.DPID); ok && n >= 1 && n <= 255 {
			it.DPID = int(n)
		}
		if t, ok := intField(p.Time); ok {
			it.T = t
		}
		if !validItem(it) {
			return nil, ErrFrame
		}
		ev.Items = append(ev.Items, it)
	}
	return []Event{ev}, nil
}

// parseIoTCoreDevice reads protocol 1001. deviceOnline, deviceOffline and deviceNameUpdate are documented;
// deviceDelete and deviceBindSpace are the names Tuya's IoT Core event list uses for removal and binding
// (unverified against a real project: an unknown bizCode is simply unused).
func parseIoTCoreDevice(b []byte) ([]Event, error) {
	m, d, e := readBiz(b)
	if e != nil {
		return nil, e
	}
	kind := map[string]EventKind{"deviceOnline": EventOnline, "deviceOffline": EventOffline, "deviceNameUpdate": EventRenamed,
		"deviceDelete": EventRemoved, "deviceBindSpace": EventBound}[m.BizCode]
	if kind == 0 {
		return []Event{}, nil
	}
	id, ok := devID(d.DevID)
	if !ok {
		return nil, ErrFrame
	}
	ev := Event{Kind: kind, Protocol: ProtocolDeviceIoTCore, DevID: id, ProductID: clipID(d.ProductID), T: eventTime(m.TS, d.Time)}
	if kind == EventRenamed {
		if ev.Name, ok = cleanName(d.Name); !ok {
			return nil, ErrFrame
		}
	}
	return []Event{ev}, nil
}

func validItem(it Item) bool {
	return tuya.ValidCode(it.Code) && len(it.Value) > 0 && len(it.Value) <= MaxValue && json.Valid(it.Value) && it.T >= 0
}

func eventTime(ts, fallback json.RawMessage) int64 {
	if t, ok := intField(ts); ok && t > 0 {
		return t
	}
	if t, ok := intField(fallback); ok && t > 0 {
		return t
	}
	return 0
}

// cleanName accepts a device name of at most 128 characters without control characters.
func cleanName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 128 {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return s, true
}

// clipID keeps a product id only when it looks like one; it is informational.
func clipID(s string) string {
	if len(s) > 64 || !credentialPattern.MatchString(s) {
		return ""
	}
	return s
}
