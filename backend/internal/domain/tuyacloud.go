package domain

import "time"

// Tuya Cloud link states (core.tuya_cloud_links.state). "" is a gateway never linked; linking is set when
// credentials are saved and left by the worker's first verdict.
const (
	CloudLinkLinking       = "linking"
	CloudLinkOnline        = "online"
	CloudLinkOffline       = "offline"
	CloudLinkAuthFailed    = "auth_failed"
	CloudLinkNotSubscribed = "not_subscribed"
	CloudLinkQuota         = "quota"
	CloudLinkDisabled      = "disabled"
)

// CloudLinkDown reports a link state in which no device of the link can be reached.
func CloudLinkDown(state string) bool {
	switch state {
	case CloudLinkOffline, CloudLinkAuthFailed, CloudLinkNotSubscribed, CloudLinkQuota, CloudLinkDisabled:
		return true
	}
	return false
}

// TuyaCloudLink is what the gateway page shows about a Tuya Cloud link: where it points, a short hint of the
// Access ID, its state and the month's usage. Never the Access ID itself, never the secret.
type TuyaCloudLink struct {
	GatewayID       string     `json:"gateway_id"`
	Linked          bool       `json:"linked"`
	Region          string     `json:"region"`
	Channel         string     `json:"channel"`
	AccessIDHint    string     `json:"access_id_hint"`
	State           string     `json:"state"`
	StateAt         *time.Time `json:"state_at"`
	Reason          string     `json:"reason"`
	LastErrorCode   int64      `json:"last_error_code"`
	LastEventAt     *time.Time `json:"last_event_at"`
	LastHealthAt    *time.Time `json:"last_health_at"`
	UsageMonth      *time.Time `json:"usage_month"`
	EventsMonth     int64      `json:"events_month"`
	APICallsMonth   int64      `json:"api_calls_month"`
	DroppedMonth    int64      `json:"dropped_month"`
	SyncRequestedAt *time.Time `json:"sync_requested_at"`
	LinkedAt        *time.Time `json:"linked_at"`
	RotatedAt       *time.Time `json:"rotated_at"`
	Devices         int        `json:"devices"`
	Registered      int        `json:"registered"`
}

// TuyaCloudLinkRequest is a link (or a credential rotation) as the repository stores it: the credentials are
// already sealed for this tenant and gateway, and the Access ID is present only as a hint and a digest.
type TuyaCloudLinkRequest struct {
	GatewayID         string
	Region            string
	Channel           string
	AccessIDHint      string
	AccessIDDigest    string
	CredentialsSealed string
}

// CloudLinkRef is one link the tuya-cloud worker should run: no credentials, only which link and its revision.
type CloudLinkRef struct {
	TenantID  string
	GatewayID string
	Revision  int64
	State     string
}

// CloudLinkSecret is one link's stored configuration as only the worker reads it: the credentials still sealed.
type CloudLinkSecret struct {
	TenantID          string
	GatewayID         string
	Region            string
	Channel           string
	CredentialsSealed string
	Revision          int64
}
