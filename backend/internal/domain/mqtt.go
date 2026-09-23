package domain

import "time"

type MQTTState struct {
	GatewayID       string     `json:"gateway_id"`
	Revision        int        `json:"revision"`
	AppliedRevision int        `json:"applied_revision"`
	AppliedAt       *time.Time `json:"applied_at"`
	LastPacketAt    *time.Time `json:"last_packet_at"`
}
