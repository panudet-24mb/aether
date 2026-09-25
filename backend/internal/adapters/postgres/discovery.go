package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"gorm.io/gorm"
	"time"
)

// DiscoverDevices lists unregistered identifiers heard by one gateway since `since`, each with what its
// own stream knows (latest reading's kind/model/RSSI and the stream name, which keeps the model after an
// info frame). The lookup is per identifier, so a gateway with many registered devices or noisy
// strangers can never push a new tag's model out of reach.
func (r *Repository) DiscoverDevices(ctx context.Context, p domain.Principal, gateway string, since time.Time) ([]domain.DiscoveredDevice, error) {
	out := []domain.DiscoveredDevice{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		// Exclude registrations before limiting: old registrations must not reappear as new devices.
		// 500, not 100: the handler drops unrecognised phones/beacons first and then caps each gateway.
		// A Zigbee2MQTT gateway hears no BLE: its candidates are the devices paired to its coordinator
		// (core.z2m_devices), listed regardless of the window — pairing is deliberate, unlike a passing phone.
		var model []string
		if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=?`, gateway).Scan(&model).Error; e != nil {
			return e
		}
		if len(model) == 1 && model[0] == domain.Z2MGatewayModel {
			return tx.Raw(`SELECT z.gateway_id,z.ieee AS external_id,coalesce(s.last_seen,z.updated_at) AS last_seen,'z2m' AS source,
    coalesce(s.name,'') AS stream_name,z.model,z.vendor,z.description,
    CASE WHEN z.category<>'' THEN z.category WHEN jsonb_array_length(z.gangs)>0 THEN 'switch' ELSE '' END AS kind
  FROM core.z2m_devices z JOIN core.gateways g ON g.tenant_id=z.tenant_id AND g.id=z.gateway_id AND g.revoked_at IS NULL
  LEFT JOIN core.sensor_streams s ON s.gateway_id=z.gateway_id AND s.external_id=z.ieee
  WHERE z.gateway_id=? AND z.removed_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM core.devices d WHERE d.tenant_id=z.tenant_id AND lower(d.external_id)=z.ieee AND d.removed_at IS NULL)
  ORDER BY z.friendly_name,z.ieee LIMIT 500`, gateway).Scan(&out).Error
		}
		return tx.Raw(`SELECT latest.gateway_id,latest.external_id,latest.received_at AS last_seen,latest.source,
    coalesce(s.name,'') AS stream_name,
    coalesce(smp.reading->>'model','') AS model,
    CASE WHEN s.external_id IS NULL THEN '' ELSE coalesce(nullif(smp.reading->>'kind',''),'environment') END AS kind,
    CASE WHEN jsonb_typeof(smp.reading->'rssi')='number' THEN (smp.reading->>'rssi')::int END AS rssi
  FROM (
   SELECT DISTINCT ON (h.external_id) h.gateway_id,h.external_id,h.received_at,h.source
   FROM core.ble_history h JOIN core.gateways g ON g.tenant_id=h.tenant_id AND g.id=h.gateway_id
   WHERE h.gateway_id=? AND g.revoked_at IS NULL AND h.received_at>=?
    AND NOT EXISTS (SELECT 1 FROM core.devices d WHERE d.tenant_id=h.tenant_id
      AND lower(d.external_id)=h.external_id AND d.removed_at IS NULL)
   ORDER BY h.external_id,h.received_at DESC,h.event_key DESC
  ) latest
  LEFT JOIN core.sensor_streams s ON s.gateway_id=latest.gateway_id AND s.external_id=latest.external_id
  LEFT JOIN LATERAL (SELECT reading FROM core.sensor_samples WHERE gateway_id=s.gateway_id AND external_id=s.external_id
    ORDER BY received_at DESC,event_key DESC LIMIT 1) smp ON true
  ORDER BY latest.received_at DESC,latest.external_id LIMIT 500`, gateway, since).Scan(&out).Error
	})
	return out, e
}
