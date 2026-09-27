package domain

import (
	"encoding/json"
	"time"
)

// TuyaDevice is an imported Tuya device as the API shows it: never its local key, only a fingerprint and the
// key's status.
type TuyaDevice struct {
	TuyaID         string    `json:"tuya_id"`
	Name           string    `json:"name"`
	TuyaCategory   string    `json:"tuya_category"`
	ProductID      string    `json:"product_id"`
	Category       string    `json:"category"`
	Sub            bool      `json:"sub"`
	LocalCapable   bool      `json:"local_capable"`
	KeyFingerprint string    `json:"key_fingerprint"`
	KeyStatus      string    `json:"key_status"`
	Version        string    `json:"version"`
	IP             string    `json:"ip"`
	Available      *bool     `json:"available"`
	Reason         string    `json:"reason"`
	Registered     bool      `json:"registered"`
	LANSeen        bool      `json:"lan_seen"`
	ImportedAt     time.Time `json:"imported_at"`
}

// EdgeSealedDevice is one device an Aether Edge must connect to, as stored: the key still sealed. Only the
// agent's configuration endpoint opens it.
type EdgeSealedDevice struct {
	ID        string
	KeySealed string
	Version   string
	IP        string
	Device22  bool
	Spec      json.RawMessage
}

// EdgeDevice is one device of an Aether Edge's configuration, with its local key in the clear: returned only to
// the agent itself, over its own gateway credential.
type EdgeDevice struct {
	ID         string `json:"id"`
	Key        string `json:"key"`
	Version    string `json:"version"`
	IP         string `json:"ip"`
	Device22   bool   `json:"dev22"`
	RefreshDPs []int  `json:"refresh_dps"`
}

// EdgeCredentials are what an install code is redeemed for: the gateway's freshly rotated MQTT password and
// HTTP token, handed over once.
type EdgeCredentials struct {
	TenantID     string
	GatewayID    string
	MQTTPassword string
	HTTPToken    string
	// ZigbeeGatewayID and ZigbeePassword are set when the install code paired a Zigbee2MQTT gateway: its MQTT
	// password was rotated with the Edge's and is handed over once, for the Zigbee2MQTT container on the same host.
	ZigbeeGatewayID string
	ZigbeePassword  string
}

// The Aether Edge container image the installer pulls, published by .github/workflows/edge.yml when a tag
// edge-v<EdgeImageTag> is pushed. Bump the tag here after publishing a new release.
// Once the first release is published the tag may also be pinned by digest ("0.1.0@sha256:…"); the installer accepts
// both forms.
const (
	EdgeImage    = "ghcr.io/panudet-24mb/aether-edge"
	EdgeImageTag = "0.1.0"
)

// Zigbee2MQTTImage is the Zigbee2MQTT release the installer and its --update pin on the site host (2.x; EmberZNet
// coordinators such as the SMLIGHT SLZB-06M/06MU use `adapter: ember`). internal/edge carries the same default.
const Zigbee2MQTTImage = "ghcr.io/koenkk/zigbee2mqtt:2.14.1"
