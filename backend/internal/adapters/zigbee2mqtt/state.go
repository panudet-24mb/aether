package zigbee2mqtt

import (
	"aether/backend/internal/adapters/minew"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// ParseState turns a device state message ({"state_left":"ON","linkquality":120,...}) into a reading: metric
// swN = 1/0 for every gang the device exposes, plus linkquality. ok is false when the message carries no gang
// state (a sensor Aether does not decode yet, or a partial update of another attribute).
func ParseState(b []byte, d Device, at time.Time) (minew.Reading, bool) {
	var obj map[string]json.RawMessage
	if e := json.Unmarshal(b, &obj); e != nil {
		return minew.Reading{}, false
	}
	r := minew.Reading{Source: "gateway", ReceivedAt: at, Kind: KindSwitch, Model: d.Model, Frames: []string{FrameState}, Metrics: map[string]float64{}}
	for _, g := range d.Gangs {
		var v string
		if json.Unmarshal(obj[g.Property], &v) != nil {
			continue
		}
		switch strings.ToUpper(v) {
		case "ON":
			r.Metrics["sw"+strconv.Itoa(g.Gang)] = 1
		case "OFF":
			r.Metrics["sw"+strconv.Itoa(g.Gang)] = 0
		}
	}
	if len(r.Metrics) == 0 {
		return minew.Reading{}, false
	}
	var lq float64
	if json.Unmarshal(obj["linkquality"], &lq) == nil && lq >= 0 && lq <= 255 {
		r.Metrics["linkquality"] = lq
	}
	var source string
	if json.Unmarshal(obj["aether_source"], &source) == nil && source == "simulated" {
		r.Source = "simulated"
	}
	return r, true
}

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
