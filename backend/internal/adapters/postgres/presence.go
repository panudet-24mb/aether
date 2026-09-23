package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"time"

	"gorm.io/gorm"
)

func (r *Repository) SetDeviceRoaming(ctx context.Context, p domain.Principal, id string, roaming bool) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE core.devices SET roaming=? WHERE id=? AND removed_at IS NULL`, roaming, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if !roaming { // the remembered zone belongs to roaming mode only
			if e := tx.Exec(`DELETE FROM core.presence_state ps USING core.devices d WHERE d.id=? AND ps.tenant_id=d.tenant_id AND ps.external_id=lower(d.external_id)`, id).Error; e != nil {
				return e
			}
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "device.roaming_changed", id)
	})
}

// Presence lists every gateway of the workspace that has heard the identity. "Current" is the strongest
// signal among gateways that heard it within PresenceFreshSec; RSSI is a proximity hint, not a position.
func (r *Repository) Presence(ctx context.Context, p domain.Principal, external string) (domain.Presence, error) {
	out := domain.Presence{ExternalID: external, Sightings: []domain.Sighting{}}
	stable := "" // zone decided at ingest with smoothing and hysteresis; preferred over the raw strongest signal
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Raw(`SELECT now()`).Scan(&out.ServerTime).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.devices WHERE external_id=? AND roaming AND removed_at IS NULL`, external).Scan(&n).Error; e != nil {
			return e
		}
		out.Roaming = n > 0
		if out.Roaming {
			var zone []struct {
				GatewayID *string
				Since     *time.Time
			}
			if e := tx.Raw(`SELECT gateway_id,since FROM core.presence_state WHERE external_id=?`, external).Scan(&zone).Error; e != nil {
				return e
			}
			if len(zone) == 1 && zone[0].GatewayID != nil {
				stable, out.Since = *zone[0].GatewayID, zone[0].Since
			}
		}
		return tx.Raw(`SELECT s.gateway_id,g.name AS gateway_name,coalesce(pr.name,'') AS project,s.last_seen,
      (SELECT (reading->>'rssi')::int FROM core.sensor_samples WHERE gateway_id=s.gateway_id AND external_id=s.external_id ORDER BY received_at DESC,event_key LIMIT 1) AS rssi
      FROM core.sensor_streams s JOIN core.gateways g ON g.tenant_id=s.tenant_id AND g.id=s.gateway_id AND g.revoked_at IS NULL
      LEFT JOIN core.projects pr ON pr.tenant_id=g.tenant_id AND pr.id=g.project_id AND pr.archived_at IS NULL
      WHERE s.external_id=? ORDER BY s.last_seen DESC LIMIT 20`, external).Scan(&out.Sightings).Error
	})
	if e != nil {
		return out, e
	}
	best := -1
	for i := range out.Sightings {
		s := &out.Sightings[i]
		s.Fresh = out.ServerTime.Sub(s.LastSeen) <= domain.PresenceFreshSec*time.Second
		if !s.Fresh {
			continue
		}
		if best < 0 || rssiOf(s.RSSI) > rssiOf(out.Sightings[best].RSSI) {
			best = i
		}
	}
	for i := range out.Sightings {
		if out.Sightings[i].GatewayID == stable && out.Sightings[i].Fresh {
			best = i
		}
	}
	if best < 0 || out.Sightings[best].GatewayID != stable {
		out.Since = nil
	}
	if best >= 0 {
		out.Sightings[best].Current = true
		current := out.Sightings[best]
		out.Current = &current
	}
	return out, nil
}

func rssiOf(v *int) int {
	if v == nil {
		return -200
	}
	return *v
}
