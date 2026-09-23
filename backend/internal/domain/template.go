package domain

import "time"

type TemplateDefinition struct {
	TemperatureHigh *float64 `json:"temperature_high"`
	HumidityHigh    *float64 `json:"humidity_high"`
}
type DeviceTemplate struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Version    int                `json:"version"`
	DecoderID  string             `json:"decoder_id"`
	Definition TemplateDefinition `json:"definition" gorm:"serializer:json"`
	CreatedAt  time.Time          `json:"created_at"`
}
