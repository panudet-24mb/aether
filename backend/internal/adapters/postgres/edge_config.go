package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TuyaDevices lists a gateway's imported Tuya devices for the import page. The sealed key is never selected.
func (r *Repository) TuyaDevices(ctx context.Context, p domain.Principal, gateway string) ([]domain.TuyaDevice, error) {
	out := []domain.TuyaDevice{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT t.tuya_id,t.name,t.tuya_category,t.product_id,t.category,t.sub,t.local_capable,t.key_fingerprint,t.key_status,
      t.version,t.ip,t.available,t.reason,t.imported_at,
      EXISTS(SELECT 1 FROM core.devices d WHERE d.gateway_id=t.gateway_id AND lower(d.external_id)=t.tuya_id AND d.removed_at IS NULL) AS registered,
      EXISTS(SELECT 1 FROM core.edge_lan_devices l WHERE l.gateway_id=t.gateway_id AND l.device_id=t.tuya_id) AS lan_seen
    FROM core.tuya_devices t WHERE t.gateway_id=? AND t.removed_at IS NULL ORDER BY t.name,t.tuya_id LIMIT 500`, gateway).Scan(&out).Error
	})
	return out, e
}

// ForgetTuyaKey drops an imported device's local key. The agent stops connecting to it at its next config pull.
func (r *Repository) ForgetTuyaKey(ctx context.Context, p domain.Principal, gateway, tuyaID string) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE core.tuya_devices SET local_key_sealed=NULL,key_fingerprint='',key_status='missing',updated_at=now()
    WHERE gateway_id=? AND tuya_id=? AND removed_at IS NULL`, gateway, strings.ToLower(tuyaID))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := bumpEdgeConfig(tx, gateway); e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", gateway); e != nil {
			return e
		}
		return audit(tx, p, "tuya.key_forgotten", gateway)
	}))
}

// CreateEdgeInstallCode stores a single-use install code (its hash only) for an Aether Edge gateway. Any earlier
// unused code of the gateway stops working: only the newest install command is valid.
func (r *Repository) CreateEdgeInstallCode(ctx context.Context, p domain.Principal, gateway, codeHash string, expires time.Time) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var models []string
		if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR UPDATE`, gateway).Scan(&models).Error; e != nil {
			return e
		}
		if len(models) != 1 {
			return domain.ErrNotFound
		}
		if models[0] != domain.EdgeGatewayModel {
			return domain.Because(domain.ErrInvalid, "not_an_edge_gateway")
		}
		if e := tx.Exec(`DELETE FROM core.edge_install_codes WHERE gateway_id=? AND redeemed_at IS NULL`, gateway).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.edge_install_codes WHERE expires_at<now()-interval '1 day'`).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.edge_install_codes(tenant_id,id,gateway_id,code_hash,created_by,expires_at) VALUES(?,?,?,?,?,?)`,
			p.TenantID, uuid.NewString(), gateway, codeHash, p.UserID, expires).Error; e != nil {
			return e
		}
		return audit(tx, p, "edge.install_code_created", gateway)
	}))
}

// BootstrapEdge redeems an install code and, in the same transaction, rotates the gateway's MQTT password and
// HTTP token to the given hashes: the credentials the installer receives are the only valid ones from then on, and
// a code that fails half-way is not spent. Unknown, used, expired or revoked all read as ErrUnauthorized.
func (r *Repository) BootstrapEdge(ctx context.Context, codeHash, mqttHash, tokenDigest string) (string, string, error) {
	var tenant, gateway string
	e := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []struct{ TenantID, GatewayID string }
		// Runs as the owner (SECURITY DEFINER): no tenant is known yet. It marks the code used and returns its
		// tenant and gateway once.
		if e := tx.Raw(`SELECT tenant_id::text AS tenant_id,gateway_id::text AS gateway_id FROM core.redeem_edge_install_code(?)`, codeHash).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrUnauthorized
		}
		tenant, gateway = rows[0].TenantID, rows[0].GatewayID
		if e := tx.Exec(`SELECT set_config('app.user_id','',true),set_config('app.tenant_id',?,true)`, tenant).Error; e != nil {
			return e
		}
		if e := tx.Exec(`SELECT set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`).Error; e != nil {
			return e
		}
		var active int64
		if e := tx.Raw(`SELECT count(*) FROM core.tenants WHERE id=? AND status='active'`, tenant).Scan(&active).Error; e != nil {
			return e
		}
		if active != 1 {
			return domain.ErrUnauthorized
		}
		var creators []sql.NullString
		if e := tx.Raw(`SELECT created_by::text FROM core.edge_install_codes WHERE code_hash=?`, codeHash).Scan(&creators).Error; e != nil {
			return e
		}
		if e := rotateGatewayToken(tx, gateway, tokenDigest); e != nil {
			return e
		}
		q := tx.Exec(`UPDATE core.mqtt_accounts SET password_hash=?,revision=revision+1 WHERE gateway_id=?`, mqttHash, gateway)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			if e := tx.Exec(`INSERT INTO core.mqtt_accounts(tenant_id,gateway_id,password_hash) VALUES(?,?,?)`, tenant, gateway, mqttHash).Error; e != nil {
				return e
			}
		}
		if len(creators) == 1 && creators[0].Valid {
			return audit(tx, domain.Principal{TenantID: tenant, UserID: creators[0].String}, "edge.bootstrapped", gateway)
		}
		return nil
	})
	return tenant, gateway, classify(e)
}

// RotateGatewayToken replaces a gateway's HTTP ingest token (only its digest is stored). The old token stops
// working at once.
func (r *Repository) RotateGatewayToken(ctx context.Context, p domain.Principal, gateway, tokenDigest string) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := rotateGatewayToken(tx, gateway, tokenDigest); e != nil {
			return e
		}
		return audit(tx, p, "gateway.token_rotated", gateway)
	}))
}

func rotateGatewayToken(tx *gorm.DB, gateway, tokenDigest string) error {
	res := tx.Exec(`UPDATE core.gateways SET token_hash=? WHERE id=? AND revoked_at IS NULL`, tokenDigest, gateway)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return domain.ErrNotFound
	}
	return nil
}

// EdgeConfig is what an Aether Edge must connect to: the registered devices of its gateway that have an imported
// key and can be reached locally, keys still sealed, and the configuration revision. The fetch is recorded.
func (r *Repository) EdgeConfig(ctx context.Context, tenant, gateway string) (int64, []domain.EdgeSealedDevice, error) {
	var revision int64
	out := []domain.EdgeSealedDevice{}
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var models []string
		if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL`, gateway).Scan(&models).Error; e != nil {
			return e
		}
		if len(models) != 1 {
			return domain.ErrUnauthorized
		}
		if models[0] != domain.EdgeGatewayModel {
			return domain.ErrForbidden
		}
		if e := tx.Raw(`SELECT t.tuya_id AS id,t.local_key_sealed AS key_sealed,t.version,t.ip,t.device22,t.spec
    FROM core.tuya_devices t
    WHERE t.gateway_id=? AND t.removed_at IS NULL AND t.local_key_sealed IS NOT NULL AND t.local_capable
      AND EXISTS(SELECT 1 FROM core.devices d WHERE d.gateway_id=t.gateway_id AND lower(d.external_id)=t.tuya_id AND d.removed_at IS NULL)
    ORDER BY t.tuya_id LIMIT 500`, gateway).Scan(&out).Error; e != nil {
			return e
		}
		var revs []int64
		if e := tx.Raw(`SELECT config_revision FROM core.edge_agents WHERE gateway_id=?`, gateway).Scan(&revs).Error; e != nil {
			return e
		}
		if len(revs) == 1 {
			revision = revs[0]
		}
		return tx.Exec(`INSERT INTO core.edge_agents(tenant_id,gateway_id,config_fetched_at,updated_at) VALUES(?,?,now(),now())
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET config_fetched_at=now()`, tenant, gateway).Error
	})
	return revision, out, classify(e)
}
