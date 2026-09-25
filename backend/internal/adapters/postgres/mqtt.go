package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"gorm.io/gorm"
)

func (r *Repository) EnrollMQTT(ctx context.Context, p domain.Principal, id, hash string, rotate bool) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM (SELECT id FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR UPDATE) g`, id).Scan(&n).Error; e != nil {
			return e
		}
		if n != 1 {
			return domain.ErrNotFound
		}
		if rotate {
			q := tx.Exec(`UPDATE core.mqtt_accounts SET password_hash=?,revision=revision+1 WHERE gateway_id=?`, hash, id)
			if q.Error != nil {
				return q.Error
			}
			if q.RowsAffected != 1 {
				return domain.ErrNotFound
			}
		} else {
			if e := tx.Exec(`INSERT INTO core.mqtt_accounts(tenant_id,gateway_id,password_hash) VALUES(?,?,?)`, p.TenantID, id, hash).Error; e != nil {
				return e
			}
		}
		return audit(tx, p, "gateway.mqtt_credential_issued", id)
	}))
}
func (r *Repository) MQTTStates(ctx context.Context, p domain.Principal) ([]domain.MQTTState, error) {
	out := []domain.MQTTState{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT g.id AS gateway_id,coalesce(a.revision,0) AS revision,coalesce(a.applied_revision,0) AS applied_revision,a.applied_at,(SELECT max(received_at) FROM core.gateway_packets WHERE gateway_id=g.id) AS last_packet_at FROM core.gateways g LEFT JOIN core.mqtt_accounts a ON a.gateway_id=g.id AND a.tenant_id=g.tenant_id WHERE g.revoked_at IS NULL ORDER BY g.created_at DESC LIMIT 50`).Scan(&out).Error
	})
	return out, e
}
func (r *Repository) MQTTGatewayTenant(ctx context.Context, id string) (string, error) {
	var tenant string
	q := r.db.WithContext(ctx).Raw(`SELECT tenant_id FROM core.lookup_mqtt_gateway(?)`, id).Scan(&tenant)
	if q.Error != nil {
		return "", q.Error
	}
	if tenant == "" {
		return "", domain.ErrUnauthorized
	}
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var count int64
		if e := tx.Raw(`SELECT count(*) FROM core.tenants WHERE id=? AND status='active'`, tenant).Scan(&count).Error; e != nil {
			return e
		}
		if count != 1 {
			return domain.ErrUnauthorized
		}
		return nil
	})
	return tenant, e
}

// GatewayModel is the model of one active gateway the principal may see (project scope applies), or ErrNotFound.
func (r *Repository) GatewayModel(ctx context.Context, p domain.Principal, id string) (string, error) {
	var models []string
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL`, id).Scan(&models).Error
	})
	if e != nil {
		return "", e
	}
	if len(models) != 1 {
		return "", domain.ErrNotFound
	}
	return models[0], nil
}
