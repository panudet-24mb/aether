// Package milesight turns what a Milesight LoRaWAN gateway (UG65) publishes from its embedded network server into
// Aether's canonical readings. The UG65 runs the network server itself, connects OUT to this broker with the Aether
// gateway's own account (Network Server → Application → Data Transmission → MQTT) and publishes each uplink under
// aether/lora/<gateway id>/<DevEUI>/up, the DevEUI filled in by the gateway's $deveui topic variable
// (docs/platform/milesight-lorawan.md). A payload is either what the gateway's payload codec decoded, or, without a
// codec, the gateway's uplink envelope with the raw bytes, which Aether decodes itself for the Milesight models it
// knows (decode.go).
package milesight

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"regexp"
	"strings"
)

// Prefix is the topic root every Milesight LoRaWAN gateway publishes under; the gateway id follows it.
const Prefix = "aether/lora/"

// FrameUplink names the decoder of a Milesight uplink in stored samples.
const FrameUplink = "milesight-up@1"

// MaxPacket bounds one uplink. An envelope with the rxInfo of the gateway is well under 2 KiB.
const MaxPacket = 16 * 1024

type Kind int

const (
	Ignore Kind = iota
	Up
)

// Message is one routed topic. DevEUI is the device the topic names.
type Message struct {
	Kind   Kind
	Topic  string
	DevEUI string
}

var devEUIPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidDevEUI reports a DevEUI in Aether's form: 16 lowercase hex digits.
func ValidDevEUI(s string) bool { return devEUIPattern.MatchString(s) }

// UplinkTopic and DownlinkTopic are what the gateway's MQTT data transmission is configured with; $deveui is the
// gateway's own topic variable for the device's EUI.
func UplinkTopic(gatewayID string) string   { return Prefix + gatewayID + "/$deveui/up" }
func DownlinkTopic(gatewayID string) string { return Prefix + gatewayID + "/$deveui/down" }

// Route splits a topic under Prefix into its gateway id and message kind. Only <DevEUI>/up is consumed; anything else
// in the gateway's tree (downlinks going TO the gateway, notifications Aether does not use) is Ignore. A topic outside
// the tree, with a malformed gateway id, or an uplink with a malformed DevEUI is forbidden.
func Route(topic string) (string, Message, error) {
	rest, ok := strings.CutPrefix(topic, Prefix)
	if !ok {
		return "", Message{}, domain.ErrForbidden
	}
	gateway, rest, _ := strings.Cut(rest, "/")
	if !security.ValidID(gateway) || rest == "" {
		return "", Message{}, domain.ErrForbidden
	}
	m := Message{Topic: topic}
	device, kind, ok := strings.Cut(rest, "/")
	if !ok || kind != "up" {
		return gateway, m, nil
	}
	m.Kind, m.DevEUI = Up, strings.ToLower(device)
	if !ValidDevEUI(m.DevEUI) {
		return "", Message{}, domain.ErrForbidden
	}
	return gateway, m, nil
}
