package domain

import "time"

var ProjectColors = []string{"mint", "blue", "amber", "coral", "violet", "slate"}

// Project groups gateways (and through them devices) inside a workspace.
type Project struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Color        string     `json:"color"`
	GatewayCount int        `json:"gateway_count"`
	ArchivedAt   *time.Time `json:"archived_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Sighting is one gateway that has heard a tag. Presence answers "where is this wearable now".
type Sighting struct {
	GatewayID   string    `json:"gateway_id"`
	GatewayName string    `json:"gateway_name"`
	Project     string    `json:"project,omitempty"`
	LastSeen    time.Time `json:"last_seen"`
	RSSI        *int      `json:"rssi"`
	Fresh       bool      `json:"fresh"`
	Current     bool      `json:"current"`
}
type Presence struct {
	ExternalID string    `json:"external_id"`
	Roaming    bool      `json:"roaming"`
	Current    *Sighting `json:"current"`
	// Since is when the wearable settled in the current zone (roaming devices only).
	Since      *time.Time `json:"since,omitempty"`
	Sightings  []Sighting `json:"sightings"`
	ServerTime time.Time  `json:"server_time"`
}

// PresenceFreshSec is how recently a gateway must have heard a tag to count as "in range".
const PresenceFreshSec = 60

// Signal is the small cross-process notification that tells connected clients what to refetch.
type Signal struct {
	Tenant  string `json:"t"`
	Kind    string `json:"k"` // packet | event | alert | inventory
	Gateway string `json:"g,omitempty"`
}
