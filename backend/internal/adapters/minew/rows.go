package minew

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Row is one JSON-Long row as uploaded by an MG3 or MG4 gateway: a {"type":"Gateway",...} header row
// followed by one row per heard advertisement. Only the fields Aether uses are kept. The device's own
// "timestamp" is deliberately not read: MG3 sends epoch milliseconds, MG4 may send an ISO-8601 string,
// and neither clock is trusted (server receive time is used everywhere).
type Row struct {
	Type   string
	MAC    string // lowercase, separators removed
	Raw    string
	Source string // "simulated" for rows produced by internal/simulation
	RSSI   *int
}

// ParseRows parses a JSON-Long uplink row by row. ok is false only when the payload is not a JSON array.
// Each row is decoded on its own and each field leniently, so one row with an odd field type (a string
// rssi, a numeric mac, an extra bleName/battery/temperature field from MG4 firmware) never costs the other
// rows of the packet; a field of the wrong type is simply left empty.
func ParseRows(payload []byte) ([]Row, bool) {
	var items []json.RawMessage
	if json.Unmarshal(payload, &items) != nil {
		return nil, false
	}
	out := make([]Row, 0, len(items))
	for _, item := range items {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			continue // not an object: skip this row only
		}
		row := Row{Type: stringField(fields["type"]), Raw: stringField(fields["rawData"]), Source: stringField(fields["aether_source"])}
		// Lowercase hex without separators, the identity form used everywhere downstream.
		row.MAC = strings.ToLower(strings.ReplaceAll(stringField(fields["mac"]), ":", ""))
		row.RSSI = intField(fields["rssi"])
		out = append(out, row)
	}
	return out, true
}

// stringField returns a JSON string's value, or "" for anything else (numbers, null, objects).
func stringField(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// intField accepts a JSON number or a numeric string ("-62") and returns nil for anything else or for a
// value that is not a whole number within int32.
func intField(raw json.RawMessage) *int {
	if len(raw) == 0 {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		s := stringField(raw)
		if s == "" {
			return nil
		}
		v, e := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if e != nil {
			return nil
		}
		f = v
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return nil
	}
	v := int(f)
	return &v
}
