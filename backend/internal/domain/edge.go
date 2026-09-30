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
	ParentTuyaID   string    `json:"parent_tuya_id"` // the hub of a sub-device, when Tuya Cloud reported it
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

// EdgeStatus is what the gateway page shows about an Aether Edge: the agent's last reported state and health, and
// counts of what it sees and what has been imported. Nothing secret.
type EdgeStatus struct {
	GatewayID        string         `json:"gateway_id"`
	State            string         `json:"state"` // "" (never connected) | online | offline
	StateAt          *time.Time     `json:"state_at"`
	Version          string         `json:"version"`
	LatestVersion    string         `json:"latest_version"`
	LastHealthAt     *time.Time     `json:"last_health_at"`
	DevicesConnected int            `json:"devices_connected"`
	LANSeen          int            `json:"lan_seen"`
	LANDevices       int            `json:"lan_devices"`
	ConfigRevision   int64          `json:"config_revision"`
	ConfigFetchedAt  *time.Time     `json:"config_fetched_at"`
	Imported         int            `json:"imported"`
	Registered       int            `json:"registered"`
	Keys             map[string]int `json:"keys"`
	// Bluetooth (docs/platform/tuya-ble.md): the agent's radio state from its heartbeat ("" before an agent that
	// knows about BLE reported, "off" when it runs without it), the Tuya BLE devices it hears and holds connected,
	// how many it heard in the sightings kept here, and the capabilities it announced on its last pull.
	BLEState     string   `json:"ble_state"`
	BLESeen      int      `json:"ble_seen"`
	BLEConnected int      `json:"ble_connected"`
	BLEDevices   int      `json:"ble_devices"`
	Capabilities []string `json:"capabilities"`
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
	// BLE devices (Transport "ble") carry their address, uuid, product, advertised protocol, how to reach them and
	// their sec_key, still sealed.
	Transport    string
	MAC          string
	UUID         string
	ProductID    string
	Protocol     int
	Mode         string
	PollSeconds  int
	SecKeySealed string
	TuyaCategory string
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
	// BLE devices only (sent only to an agent that announced the capability, and only with EDGE_BLE on). The
	// fields are omitted for Wi-Fi devices, so a Wi-Fi device reads exactly as before.
	Transport string            `json:"transport,omitempty"` // "ble"; empty means Wi-Fi
	MAC       string            `json:"mac,omitempty"`
	UUID      string            `json:"uuid,omitempty"`
	SecKey    string            `json:"sec_key,omitempty"`
	ProductID string            `json:"product_id,omitempty"`
	Protocol  int               `json:"protocol,omitempty"`
	Mode      string            `json:"mode,omitempty"`
	Poll      int               `json:"poll,omitempty"`     // seconds between reads
	DPTypes   map[string]string `json:"dp_types,omitempty"` // data point id -> bool | value | enum | string | bitmap | raw
	// ReadOnly: the agent must never write to this device (a lock or safe over BLE); it gets no dp_types either.
	ReadOnly bool `json:"readonly,omitempty"`
}

// EdgeCapabilityBLE is the capability an agent built with Bluetooth announces on its configuration pull
// (X-Aether-Edge-Caps: ble). An agent without it is never sent BLE devices: it would try to reach them over TCP.
const EdgeCapabilityBLE = "ble"

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
	EdgeImageTag = "0.1.1"
)

// Zigbee2MQTTImage is the Zigbee2MQTT release the installer and its --update pin on the site host (2.x; EmberZNet
// coordinators such as the SMLIGHT SLZB-06M/06MU use `adapter: ember`). internal/edge carries the same default.
const Zigbee2MQTTImage = "ghcr.io/koenkk/zigbee2mqtt:2.14.1"
