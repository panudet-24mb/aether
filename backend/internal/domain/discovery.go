package domain

import "time"

// DiscoveredDevice is an unregistered BLE identifier heard by one gateway in the discovery window.
type DiscoveredDevice struct {
	GatewayID  string         `json:"gateway_id"`
	ExternalID string         `json:"external_id"`
	LastSeen   time.Time      `json:"last_seen"`
	Source     string         `json:"source"`
	Model      string         `json:"model,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	RSSI       *int           `json:"rssi"`
	Profile    *DeviceProfile `json:"profile,omitempty"`
}
