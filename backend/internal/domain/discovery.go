package domain

import "time"

// DiscoveredDevice is an unregistered BLE identifier heard by one gateway in the discovery window.
type DiscoveredDevice struct {
	GatewayID  string    `json:"gateway_id"`
	ExternalID string    `json:"external_id"`
	LastSeen   time.Time `json:"last_seen"`
	Source     string    `json:"source"`
	Model      string    `json:"model,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	// Vendor and Description come from a Zigbee2MQTT device's own definition (empty for BLE).
	Vendor      string         `json:"vendor,omitempty"`
	Description string         `json:"description,omitempty"`
	RSSI        *int           `json:"rssi"`
	Profile     *DeviceProfile `json:"profile,omitempty"`
	// StreamName is the device's stream name: "Minew <model>" once an info frame arrived, a user-assigned
	// name, or a generic one. Used server-side to recognise the device; not part of the API.
	StreamName string `json:"-" gorm:"column:stream_name"`
}
