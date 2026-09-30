// Package ble is Aether Edge's Tuya BLE transport (phase B2, docs/platform/tuya-ble.md). It scans with a Radio,
// keeps the Tuya BLE devices it hears, and runs short authenticated sessions (internal/tuyable) to read registered
// devices and deliver commands, within a small connection budget: a Tuya BLE device takes one central at a time,
// and a host adapter holds only a few connections.
//
// Like internal/edge it must stay free of server code; the radio behind the Radio interface is the host's BlueZ
// over D-Bus on Linux (bluez_linux.go), and a fake in tests.
package ble

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"aether/backend/internal/tuyable"
)

// Advertisement is one advertisement the radio heard from a device that looks like Tuya BLE: its service data (on
// 0xA201, or 0xFD50 on newer devices) and its manufacturer data (company 0x07D0), both as received.
type Advertisement struct {
	MAC              string // aa:bb:cc:dd:ee:ff, lower case
	RSSI             int
	ServiceData      []byte
	FD50             bool // the service data came on 0xFD50: the device's GATT service is FD50 too
	ManufacturerData []byte
}

// Radio is what the manager needs from a Bluetooth adapter.
type Radio interface {
	// Adapter names the adapter, for the heartbeat (hci0).
	Adapter() string
	// Scan delivers advertisements until ctx ends (nil) or the adapter fails (ErrNoAdapter, ErrNoPermission or
	// another error). on is called from the scanning goroutine and must not block.
	Scan(ctx context.Context, on func(Advertisement)) error
	// Connect opens a GATT link to the device's Tuya service (0x1910, or FD50) within ctx. A device that refuses a
	// second central is reported as tuyable.ErrBusy.
	Connect(ctx context.Context, mac string, fd50 bool) (tuyable.Link, error)
}

var (
	// ErrNoAdapter: the host has no usable Bluetooth adapter (missing, powered off or blocked by rfkill).
	ErrNoAdapter = errors.New("ble: no usable Bluetooth adapter")
	// ErrNoPermission: the agent may not talk to bluetoothd (D-Bus policy, or the socket is not mounted).
	ErrNoPermission = errors.New("ble: not allowed to use the Bluetooth adapter")
)

// Radio states, as the heartbeat reports them.
const (
	StateOff          = "off"
	StateOK           = "ok"
	StateNoAdapter    = "no_adapter"
	StateNoPermission = "no_permission"
	StateError        = "error"
)

// stateOf maps a scan error to the heartbeat's radio state.
func stateOf(e error) string {
	switch {
	case e == nil:
		return StateOK
	case errors.Is(e, ErrNoAdapter):
		return StateNoAdapter
	case errors.Is(e, ErrNoPermission):
		return StateNoPermission
	}
	return StateError
}

// Availability reasons, the same the Wi-Fi path reports (internal/edge, adapters/edge on the server).
const (
	ReasonUnreachable = "unreachable"
	ReasonAuthFailed  = "auth_failed"
	ReasonBusy        = "busy"
	ReasonNotFound    = "not_found"
)

// Reaching modes of a device.
const (
	ModeAuto       = "auto"       // read when it advertises and its last read is older than the poll interval
	ModeOnDemand   = "on_demand"  // commands, and a read every poll interval; nothing on advertisement
	ModePersistent = "persistent" // keep the connection open (mains devices; blocks the phone app)
)

// Device is one registered Tuya BLE device the agent must reach. LocalKey and SecKey are secrets: never logged.
type Device struct {
	ID        string // Tuya device id
	MAC       string // may be empty when UUID is set: the address is then learned from the advertisement
	UUID      string
	LocalKey  string
	SecKey    string
	ProductID string
	Protocol  byte // advertised protocol as last seen by the server; the air wins
	Mode      string
	Poll      time.Duration
	DPTypes   map[byte]tuyable.DPType
	// ReadOnly devices (locks) are read, never written: every command to one is refused.
	ReadOnly bool
}

// String, GoString and LogValue leave the keys out, so a Device can be printed or logged.
func (d Device) String() string {
	return fmt.Sprintf("ble.Device{ID:%s MAC:%s UUID:%s Mode:%s ReadOnly:%t}", d.ID, d.MAC, d.UUID, d.Mode, d.ReadOnly)
}

func (d Device) GoString() string { return d.String() }

func (d Device) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", d.ID), slog.String("mac", d.MAC), slog.String("uuid", d.UUID), slog.String("mode", d.Mode),
		slog.Bool("read_only", d.ReadOnly))
}

// Sighting is one Tuya BLE device heard advertising, as the agent reports it (aether/edge/<gw>/ble).
type Sighting struct {
	MAC       string `json:"mac"`
	UUID      string `json:"uuid"`
	ProductID string `json:"product_id"`
	Protocol  int    `json:"proto"`
	Bound     bool   `json:"bound"`
	FD50      bool   `json:"fd50"`
	RSSI      *int   `json:"rssi,omitempty"`
}

// Health is the Bluetooth part of the agent's heartbeat.
type Health struct {
	State          string `json:"state"`
	Adapter        string `json:"adapter"`
	Seen           int    `json:"seen"`
	Connected      int    `json:"connected"`
	SessionsOK     int    `json:"sessions_ok"`
	SessionsFailed int    `json:"sessions_failed"`
	Queue          int    `json:"queue"`
}

// Publisher is where the manager sends what it learns; the agent turns each call into an MQTT message.
type Publisher interface {
	State(device string, dps map[string]any)
	Availability(device string, online bool, reason string)
	Sightings(list []Sighting)
}

var macPattern = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// NormalMAC returns a Bluetooth address in Aether's form (aa:bb:cc:dd:ee:ff), or "" when it is not one. Dashes or
// no separators are accepted.
func NormalMAC(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("-", "", ":", "").Replace(s)
	if len(s) != 12 {
		return ""
	}
	if _, e := hex.DecodeString(s); e != nil {
		return ""
	}
	out := make([]string, 6)
	for i := range out {
		out[i] = s[2*i : 2*i+2]
	}
	m := strings.Join(out, ":")
	if !macPattern.MatchString(m) {
		return ""
	}
	return m
}

// reversedMAC is the address with its bytes in the other order: some Tuya devices' factory records list it
// reversed.
func reversedMAC(m string) string {
	parts := strings.Split(m, ":")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ":")
}
