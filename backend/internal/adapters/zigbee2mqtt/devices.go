package zigbee2mqtt

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxDevices bounds one bridge/devices document; a Zigbee network of one coordinator stays far below it.
const MaxDevices = 500

// MaxPacket is the largest message accepted from a bridge (bridge/devices); the broker's max_packet_size is the
// same 1 MiB. Every other Zigbee2MQTT message is capped far lower by the service.
const MaxPacket = 1024 * 1024

var ieeePattern = regexp.MustCompile(`^0x[0-9a-f]{16}$`)

// ValidIEEE reports a Zigbee IEEE address in Z2M's form, lower case: 0x followed by 16 hex digits.
func ValidIEEE(s string) bool { return ieeePattern.MatchString(s) }

// Gang is one switch output of a device, in physical order (gang 1 is l1 / left / the only output).
type Gang struct {
	Gang     int    `json:"gang"`
	Property string `json:"property"`
	Endpoint string `json:"endpoint,omitempty"`
	Settable bool   `json:"settable"`
}

// Device is one entry of bridge/devices that Aether keeps.
type Device struct {
	IEEE         string
	FriendlyName string
	Type         string
	Model        string
	Vendor       string
	ModelID      string
	Manufacturer string
	PowerSource  string
	Supported    bool
	Gangs        []Gang
	// Exposes is the definition's exposes array exactly as Zigbee2MQTT published it ("[]" when absent or over
	// MaxExposes). Commands are validated against it (features.go); a generic ingest can build on it later.
	Exposes json.RawMessage
}

type expose struct {
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Property string   `json:"property"`
	Endpoint string   `json:"endpoint"`
	Access   int      `json:"access"`
	Features []expose `json:"features"`
}

type bridgeDevice struct {
	IEEE         string `json:"ieee_address"`
	Type         string `json:"type"`
	FriendlyName string `json:"friendly_name"`
	Supported    bool   `json:"supported"`
	ModelID      string `json:"model_id"`
	Manufacturer string `json:"manufacturer"`
	PowerSource  string `json:"power_source"`
	Disabled     bool   `json:"disabled"`
	Definition   *struct {
		Model   string          `json:"model"`
		Vendor  string          `json:"vendor"`
		Exposes json.RawMessage `json:"exposes"`
	} `json:"definition"`
}

// ParseBridgeDevices reads the retained bridge/devices document. The coordinator itself, disabled devices and
// entries without a valid IEEE address are skipped; at most MaxDevices are returned, and truncated reports that
// the list was cut (the caller must then not treat a missing device as removed).
func ParseBridgeDevices(b []byte) (devices []Device, truncated bool, err error) {
	var raw []bridgeDevice
	if e := json.Unmarshal(b, &raw); e != nil {
		return nil, false, e
	}
	out := []Device{}
	for _, r := range raw {
		ieee := strings.ToLower(r.IEEE)
		if r.Type == "Coordinator" || r.Disabled || !ValidIEEE(ieee) {
			continue
		}
		d := Device{IEEE: ieee, FriendlyName: clip(r.FriendlyName, 128), Type: clip(r.Type, 32), ModelID: clip(r.ModelID, 64),
			Manufacturer: clip(r.Manufacturer, 64), PowerSource: clip(r.PowerSource, 64), Supported: r.Supported, Gangs: []Gang{}, Exposes: json.RawMessage("[]")}
		if d.FriendlyName == "" {
			d.FriendlyName = ieee
		}
		if r.Definition != nil {
			d.Model, d.Vendor = clip(r.Definition.Model, 64), clip(r.Definition.Vendor, 64)
			var exposes []expose
			if json.Unmarshal(r.Definition.Exposes, &exposes) == nil {
				d.Gangs = gangs(exposes)
				if len(r.Definition.Exposes) <= MaxExposes {
					d.Exposes = compact(r.Definition.Exposes)
				}
			}
		}
		if len(out) == MaxDevices {
			return out, true, nil
		}
		out = append(out, d)
	}
	return out, false, nil
}

// gangs derives the switch outputs from a definition's exposes: every `switch` expose contributes the `state`
// feature of its endpoint. Settable follows the access bitmask (2 = SET).
func gangs(exposes []expose) []Gang {
	out := []Gang{}
	seen := map[string]bool{}
	for _, e := range exposes {
		if e.Type != "switch" {
			continue
		}
		for _, f := range e.Features {
			if f.Name != "state" || f.Property == "" || seen[f.Property] {
				continue
			}
			seen[f.Property] = true
			endpoint := f.Endpoint
			if endpoint == "" {
				endpoint = e.Endpoint
			}
			out = append(out, Gang{Property: clip(f.Property, 64), Endpoint: clip(endpoint, 32), Settable: f.Access&2 != 0})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return endpointRank(out[i].Endpoint) < endpointRank(out[j].Endpoint) })
	if len(out) > 4 {
		out = out[:4]
	}
	for i := range out {
		out[i].Gang = i + 1
	}
	return out
}

// endpointRank orders outputs the way they sit on the wall: the bare output, then l1<l2<…, then left<center<right.
func endpointRank(endpoint string) int {
	switch endpoint {
	case "":
		return 0
	case "left":
		return 1
	case "center":
		return 2
	case "right":
		return 3
	}
	if n, e := strconv.Atoi(strings.TrimPrefix(endpoint, "l")); e == nil && strings.HasPrefix(endpoint, "l") && n > 0 && n < 100 {
		return n
	}
	return 100
}

// clip trims, drops NUL characters (Tuya firmware pads model ids with them and PostgreSQL text cannot hold
// them) and bounds a string from the bridge.
func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	if len(s) > n {
		return s[:n]
	}
	return s
}
