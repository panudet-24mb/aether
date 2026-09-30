package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"aether/backend/internal/edge/ble"
)

// Availability reasons, as the server's adapters/edge reads them.
const (
	ReasonUnreachable = "unreachable"
	ReasonAuthFailed  = "auth_failed"
	ReasonKeySuspect  = "key_suspect"
	ReasonBusy        = "busy"
	ReasonNotFound    = "not_found"
)

// Bounds shared with the server (adapters/edge).
const (
	MaxLAN     = 500
	MaxDPS     = 255
	MaxCommand = 1024
)

type statePayload struct {
	DPS  map[string]any `json:"dps"`
	Full bool           `json:"full"`
}

type availabilityPayload struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type healthPayload struct {
	Version          string `json:"version"`
	DevicesConnected int    `json:"devices_connected"`
	LANSeen          int    `json:"lan_seen"`
	// BLE is always sent by this agent: the radio's state, "off" when started without BLE_ENABLED.
	BLE *ble.Health `json:"ble"`
}

// LANDevice is one device seen broadcasting on the LAN.
type LANDevice struct {
	ID         string `json:"id"`
	IP         string `json:"ip"`
	Version    string `json:"version"`
	ProductKey string `json:"product_key,omitempty"`
}

func encodeState(dps map[string]any, full bool) []byte {
	b, _ := json.Marshal(statePayload{DPS: dps, Full: full})
	return b
}

func encodeAvailability(online bool, reason string) []byte {
	p := availabilityPayload{State: "online"}
	if !online {
		p = availabilityPayload{State: "offline", Reason: reason}
	}
	b, _ := json.Marshal(p)
	return b
}

var errBadCommand = errors.New("edge: malformed command")

// parseCommand reads a command published by the server's commander: {"dps":{"<id>":<raw>}}, at most MaxCommand
// bytes, ids 1..255, numbers kept exact (json.Number).
func parseCommand(b []byte) (map[string]any, error) {
	dps, _, e := parseCommandDeadline(b, time.Now())
	return dps, e
}

// maxCommandTTL bounds what a command's ttl_ms may say: the server's longest confirm window is 45 s.
const maxCommandTTL = 2 * time.Minute

// parseCommandDeadline is parseCommand plus until when the command may be delivered: the earlier of its
// "expires_at" and received plus its "ttl_ms" (server/adapters/edge.SetDeadline). A command from a server before
// 0.2 carries neither: the zero time, no deadline.
func parseCommandDeadline(b []byte, received time.Time) (map[string]any, time.Time, error) {
	if len(b) == 0 || len(b) > MaxCommand {
		return nil, time.Time{}, errBadCommand
	}
	var obj struct {
		DPS       map[string]json.RawMessage `json:"dps"`
		ExpiresAt string                     `json:"expires_at"`
		TTLMS     *int64                     `json:"ttl_ms"`
	}
	if json.Unmarshal(b, &obj) != nil || len(obj.DPS) == 0 || len(obj.DPS) > MaxDPS {
		return nil, time.Time{}, errBadCommand
	}
	var deadline time.Time
	if obj.TTLMS != nil {
		ttl := time.Duration(*obj.TTLMS) * time.Millisecond
		if ttl <= 0 || ttl > maxCommandTTL {
			return nil, time.Time{}, errBadCommand
		}
		deadline = received.Add(ttl)
	}
	if obj.ExpiresAt != "" {
		at, e := time.Parse(time.RFC3339Nano, obj.ExpiresAt)
		if e != nil {
			return nil, time.Time{}, errBadCommand
		}
		if deadline.IsZero() || at.Before(deadline) {
			deadline = at
		}
	}
	dps, e := commandDPS(obj.DPS)
	return dps, deadline, e
}

// commandDPS checks and decodes a command's data points.
func commandDPS(dps map[string]json.RawMessage) (map[string]any, error) {
	out := make(map[string]any, len(dps))
	for k, raw := range dps {
		id, e := strconv.Atoi(k)
		if e != nil || id < 1 || id > 255 || strconv.Itoa(id) != k {
			return nil, errBadCommand
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v any
		if d.Decode(&v) != nil {
			return nil, errBadCommand
		}
		switch v.(type) {
		case bool, string, json.Number:
		default:
			return nil, errBadCommand // Tuya data points are scalars; objects/arrays/null are refused
		}
		out[k] = v
	}
	return out, nil
}
