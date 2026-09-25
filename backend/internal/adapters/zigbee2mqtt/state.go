package zigbee2mqtt

import (
	"encoding/json"
	"strings"
)

// DeviceIEEE returns the IEEE address a state message carries itself when Z2M runs with
// include_device_information; empty when absent or malformed.
func DeviceIEEE(b []byte) string {
	var obj struct {
		Device struct {
			IEEE string `json:"ieeeAddr"`
		} `json:"device"`
	}
	if json.Unmarshal(b, &obj) != nil {
		return ""
	}
	ieee := strings.ToLower(obj.Device.IEEE)
	if !ValidIEEE(ieee) {
		return ""
	}
	return ieee
}

// ParseOnline reads an availability or bridge/state payload: {"state":"online"} (Z2M 1.x/2.x) or the legacy
// bare string online / offline.
func ParseOnline(b []byte) (online bool, ok bool) {
	s := strings.TrimSpace(string(b))
	var obj struct {
		State string `json:"state"`
	}
	if json.Unmarshal(b, &obj) == nil && obj.State != "" {
		s = obj.State
	} else {
		var quoted string
		if json.Unmarshal(b, &quoted) == nil {
			s = quoted
		}
	}
	switch strings.ToLower(s) {
	case "online":
		return true, true
	case "offline":
		return false, true
	}
	return false, false
}

// ParseBridgeVersion returns the Zigbee2MQTT version from bridge/info (bounded), or empty.
func ParseBridgeVersion(b []byte) string {
	var obj struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &obj) != nil {
		return ""
	}
	return clip(obj.Version, 32)
}
