// Package zigbee2mqtt turns what a Zigbee2MQTT bridge publishes under its base topic into Aether's canonical
// readings. Z2M runs on site next to a Zigbee coordinator and connects OUT to this broker with the gateway's
// own account; its base_topic is aether/z2m/<gateway id> (see docs/platform/zigbee2mqtt.md). Devices are keyed
// by IEEE address, never by friendly name: names can be renamed at any time and may contain "/".
package zigbee2mqtt

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"strings"
)

// Prefix is the topic root every Zigbee2MQTT gateway publishes under; the gateway id follows it.
const Prefix = "aether/z2m/"

const (
	// KindSwitch is the reading kind of a switch; metrics sw1..sw4 carry each gang (1 = ON, 0 = OFF).
	KindSwitch = "switch"
	// FrameState names the decoder of a Z2M device state message in stored samples.
	FrameState = "z2m-state@1"
)

// BaseTopic is the base_topic a gateway's Zigbee2MQTT must be configured with.
func BaseTopic(gatewayID string) string { return Prefix + gatewayID }

type Kind int

const (
	Ignore Kind = iota
	State
	Availability
	BridgeDevices
	BridgeState
	BridgeInfo
	Heartbeat
)

// Message is one routed topic. Device is the device part of the topic (a friendly name, which by default is
// the IEEE address) for State and Availability messages.
type Message struct {
	Kind   Kind
	Topic  string
	Device string
}

// Route splits a topic under Prefix into its gateway id and message kind. Anything that is not a message Aether
// consumes is Ignore: commands going TO the bridge (…/set, …/get, bridge/request/*), its responses, logs and the
// large bridge/definitions document. A topic outside the tree or with a malformed gateway id is forbidden.
func Route(topic string) (string, Message, error) {
	rest, ok := strings.CutPrefix(topic, Prefix)
	if !ok {
		return "", Message{}, domain.ErrForbidden
	}
	gateway, rest, _ := strings.Cut(rest, "/")
	if !security.ValidID(gateway) {
		return "", Message{}, domain.ErrForbidden
	}
	m := Message{Topic: topic}
	switch {
	case rest == "":
		return "", Message{}, domain.ErrForbidden
	case strings.HasPrefix(rest, "bridge/"):
		switch rest {
		case "bridge/devices":
			m.Kind = BridgeDevices
		case "bridge/state":
			m.Kind = BridgeState
		case "bridge/info":
			m.Kind = BridgeInfo
		case "bridge/health":
			m.Kind = Heartbeat
		}
		return gateway, m, nil
	case commandTopic(rest):
		return gateway, m, nil
	case strings.HasSuffix(rest, "/availability"):
		m.Kind, m.Device = Availability, strings.TrimSuffix(rest, "/availability")
	default:
		m.Kind, m.Device = State, rest
	}
	if m.Device == "" || len(m.Device) > 256 {
		return "", Message{}, domain.ErrForbidden
	}
	return gateway, m, nil
}

// commandTopic reports topics that carry requests to the bridge rather than reports from it.
func commandTopic(rest string) bool {
	for _, verb := range []string{"set", "get"} {
		if strings.HasSuffix(rest, "/"+verb) || strings.Contains(rest, "/"+verb+"/") {
			return true
		}
	}
	return false
}
