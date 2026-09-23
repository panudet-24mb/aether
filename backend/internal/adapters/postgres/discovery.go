package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"gorm.io/gorm"
	"time"
)

func (r *Repository) DiscoverDevices(ctx context.Context, p domain.Principal, gateway string, since time.Time) ([]domain.DiscoveredDevice, error) {
	out := []domain.DiscoveredDevice{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		// Exclude registrations before limiting: old registrations must not reappear as new devices.
		return tx.Raw(`SELECT gateway_id,external_id,received_at AS last_seen,source FROM (
   SELECT DISTINCT ON (h.external_id) h.gateway_id,h.external_id,h.received_at,h.source
   FROM core.ble_history h JOIN core.gateways g ON g.tenant_id=h.tenant_id AND g.id=h.gateway_id
   WHERE h.gateway_id=? AND g.revoked_at IS NULL AND h.received_at>=?
    AND NOT EXISTS (SELECT 1 FROM core.devices d WHERE d.tenant_id=h.tenant_id
      AND lower(d.external_id)=h.external_id AND d.removed_at IS NULL)
   ORDER BY h.external_id,h.received_at DESC,h.event_key DESC
  ) latest ORDER BY received_at DESC,external_id LIMIT 100`, gateway, since).Scan(&out).Error
	})
	return out, e
}
