// Package minew projects diagnostic MG3 packets into a bounded live view.
// Frame layouts live in frames.go; FFE1 A1/01 (temperature/humidity) is the only one verified on hardware.
package minew

import (
	"aether/backend/internal/domain"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Reading is one merged observation of a device in one gateway uplink. Temperature/Humidity are only
// meaningful when Kind is "environment" (older stored samples have no Kind and are environment readings).
type Reading struct {
	Source     string    `json:"source"`
	ReceivedAt time.Time `json:"received_at"`
	Kind       string    `json:"kind,omitempty"`
	Model      string    `json:"model,omitempty"`
	Frames     []string  `json:"frames,omitempty"`
	// Unknown lists compact descriptors of what this uplink contained but the decoder did not
	// understand (e.g. "ffe1:a1:0x22:len=14", "ad:0x16:uuid=feaa:type=0x30"), so an operator can see
	// that a tag is talking and Aether has no rule for it. Bounded to 8 entries of 40 chars.
	Unknown     []string           `json:"unknown,omitempty"`
	Temperature float64            `json:"temperature"`
	Humidity    float64            `json:"humidity"`
	Battery     int                `json:"battery"`
	RSSI        *int               `json:"rssi"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
	Beacon      *Beacon            `json:"beacon,omitempty"`
	// Commands names, per switch gang, the Aether command this report confirmed (Zigbee2MQTT). It is never
	// stored with the sample; the alerts engine uses it to tell a commanded change from a press on the wall.
	Commands map[int]string `json:"-"`
}
type Sensor struct {
	TemplateID *string                   `json:"template_id"`
	Thresholds domain.TemplateDefinition `json:"thresholds"`
	ID         string                    `json:"id"`
	Name       string                    `json:"name"`
	Kind       string                    `json:"kind"`
	Model      string                    `json:"model,omitempty"`
	Latest     Reading                   `json:"latest"`
	History    []Reading                 `json:"history"`
	// Liveness "reported" means offline/online come from the device's own availability reports (Zigbee2MQTT),
	// not from silence; Offline is then the last reported state. Both are empty for BLE streams.
	Liveness string `json:"liveness,omitempty"`
	Offline  *bool  `json:"offline,omitempty"`
}
type View struct {
	Gateway          domain.Gateway `json:"gateway"`
	LastPacketAt     *time.Time     `json:"last_packet_at"`
	PacketCount      int            `json:"packet_count"`
	ObservationCount int            `json:"observation_count"`
	NearbyDevices    int            `json:"nearby_devices"`
	Sensors          []Sensor       `json:"sensors"`
}

// Decode keeps the original temperature/humidity contract: ok only when an FFE1 A1/01 frame is present.
func Decode(raw string) (Reading, bool) {
	r, ok := DecodeFrames(raw)
	if !ok || !hasFrame(r.Frames, FrameTH) {
		return Reading{}, false
	}
	return r, true
}

// minewUnknown reports whether an undecoded advertisement was a Minew FFE1 frame.
func minewUnknown(unknown []string) bool {
	for _, u := range unknown {
		if strings.HasPrefix(u, "ffe1:") {
			return true
		}
	}
	return false
}
func validMAC(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 6 }

// Project decodes stored packets into a per-device live view.
func Project(g domain.Gateway, packets []domain.Packet) View { return ProjectKnown(g, packets, nil) }

// ProjectKnown is Project for the ingest path, which knows which identities the workspace already tracks
// (registered devices, streams this gateway already has). For those, an advertisement Aether cannot decode
// at all still yields a reading carrying only Unknown, so a tag whose frames use a service Aether has no
// rule for (the S4 door sensor's layout is not public) keeps its stream fresh and can drive a taught
// signal on every uplink, not only on the ones that happen to carry its info frame.
func ProjectKnown(g domain.Gateway, packets []domain.Packet, known func(mac string) bool) View {
	v := View{Gateway: g, Sensors: []Sensor{}}
	sensors := map[string]*Sensor{}
	nearby := map[string]bool{}
	// Work in receive-time order, independent of caller ordering. Never trust device clocks.
	ordered := append([]domain.Packet(nil), packets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ReceivedAt.Before(ordered[j].ReceivedAt) })
	for _, packet := range ordered {
		rows, ok := ParseRows(packet.Payload)
		if !ok || len(rows) == 0 {
			continue
		}
		v.PacketCount++
		at := packet.ReceivedAt
		v.LastPacketAt = &at
		readings := map[string]Reading{}
		for _, r := range rows {
			if r.Type == "Gateway" {
				continue
			}
			mac := strings.ToLower(strings.ReplaceAll(r.MAC, ":", ""))
			if !validMAC(mac) {
				continue
			}
			nearby[mac] = true
			v.ObservationCount++
			value, ok := DecodeFramesFor(r.Raw, mac)
			if !ok {
				// A Minew tag talking a frame version Aether has no decoder for (the S4 door sensor's
				// combination frame, an undocumented press frame) still becomes a stream: its frames are
				// listed in Unknown so the operator sees it talking and can teach a signal from it. Other
				// undecodable advertisements (phones, foreign beacons) stay out of discovery as before,
				// unless the identity is one the workspace already tracks (see ProjectKnown).
				if len(value.Unknown) == 0 || (!minewUnknown(value.Unknown) && (known == nil || !known(mac))) {
					continue
				}
				value.Kind = KindInfo
			}
			value.ReceivedAt = packet.ReceivedAt
			value.Source = "gateway"
			if r.Source == "simulated" {
				value.Source = "simulated"
			}
			if r.RSSI != nil && *r.RSSI >= -127 && *r.RSSI <= 20 {
				value.RSSI = r.RSSI
			}
			// One sample per device per uplink: a tag that interleaves sensor and beacon slots is merged.
			if prev, seen := readings[mac]; seen {
				prev.Merge(value)
				if value.RSSI != nil {
					prev.RSSI = value.RSSI
				}
				readings[mac] = prev
			} else {
				readings[mac] = value
			}
		}
		for mac, value := range readings {
			sensor := sensors[mac]
			if sensor == nil {
				sensor = &Sensor{ID: mac, History: []Reading{}}
				sensors[mac] = sensor
			}
			sensor.Latest = value
			sensor.Kind = value.Kind
			if value.Model != "" {
				sensor.Model = value.Model
			}
			sensor.Name = DisplayName(value)
			if value.Source == "simulated" {
				sensor.Name = "SIM · ข้อมูลจำลอง · " + sensor.Name
			}
			sensor.History = append(sensor.History, value)
		}
	}
	v.NearbyDevices = len(nearby)
	for _, s := range sensors {
		v.Sensors = append(v.Sensors, *s)
	}
	sort.Slice(v.Sensors, func(i, j int) bool { return v.Sensors[i].ID < v.Sensors[j].ID })
	return v
}
