package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
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
	if len(b) == 0 || len(b) > MaxCommand {
		return nil, errBadCommand
	}
	var obj struct {
		DPS map[string]json.RawMessage `json:"dps"`
	}
	if json.Unmarshal(b, &obj) != nil || len(obj.DPS) == 0 || len(obj.DPS) > MaxDPS {
		return nil, errBadCommand
	}
	out := make(map[string]any, len(obj.DPS))
	for k, raw := range obj.DPS {
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
