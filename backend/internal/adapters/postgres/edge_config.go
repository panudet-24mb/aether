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
      EXISTS(SELECT 1 FROM core.devices d WHERE d.tenant_id=t.tenant_id AND lower(d.external_id)=t.tuya_id AND d.removed_at IS NULL) AS registered,
      EXISTS(SELECT 1 FROM core.edge_lan_devices l WHERE l.gateway_id=t.gateway_id AND l.device_id=t.tuya_id) AS lan_seen
    FROM core.tuya_devices t WHERE t.gateway_id=? AND t.removed_at IS NULL ORDER BY t.name,t.tuya_id LIMIT 500`, gateway).Scan(&out).Error
	})
	return out, e
}

// "Registered" means registered anywhere in the workspace, as discovery counts it: a Tuya id adopted under another
// gateway is not offered again, so it must not look unregistered here either.
//
// EdgeStatus is the gateway page's view of an Aether Edge. Only a live aether-edge gateway in the caller's scope
// answers; anything else is not found.
func (r *Repository) EdgeStatus(ctx context.Context, p domain.Principal, gateway string) (domain.EdgeStatus, error) {
	out := domain.EdgeStatus{GatewayID: gateway, LatestVersion: domain.EdgeImageTag, Keys: map[string]int{"ok": 0, "rejected": 0, "suspect": 0, "missing": 0}}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []struct {
			State                                string
			StateAt, LastHealthAt, ConfigFetched *time.Time
			Version                              string
			DevicesConnected, LANSeen            int
			ConfigRevision                       int64
			LANDevices, Imported, Registered     int
		}
		if e := tx.Raw(`SELECT coalesce(a.state,'') AS state,a.state_at,a.last_health_at,a.config_fetched_at AS config_fetched,coalesce(a.version,'') AS version,
      coalesce(a.devices_connected,0) AS devices_connected,coalesce(a.lan_seen,0) AS lan_seen,coalesce(a.config_revision,0) AS config_revision,
      (SELECT count(*) FROM core.edge_lan_devices l WHERE l.gateway_id=g.id) AS lan_devices,
      (SELECT count(*) FROM core.tuya_devices t WHERE t.gateway_id=g.id AND t.removed_at IS NULL) AS imported,
      (SELECT count(*) FROM core.tuya_devices t WHERE t.gateway_id=g.id AND t.removed_at IS NULL
        AND EXISTS(SELECT 1 FROM core.devices d WHERE d.tenant_id=t.tenant_id AND lower(d.external_id)=t.tuya_id AND d.removed_at IS NULL)) AS registered
    FROM core.gateways g LEFT JOIN core.edge_agents a ON a.tenant_id=g.tenant_id AND a.gateway_id=g.id
    WHERE g.id=? AND g.model=? AND g.revoked_at IS NULL`, gateway, domain.EdgeGatewayModel).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrNotFound
		}
		row := rows[0]
		out.State, out.StateAt, out.LastHealthAt, out.ConfigFetchedAt = row.State, row.StateAt, row.LastHealthAt, row.ConfigFetched
		out.Version, out.DevicesConnected, out.LANSeen, out.ConfigRevision = row.Version, row.DevicesConnected, row.LANSeen, row.ConfigRevision
		out.LANDevices, out.Imported, out.Registered = row.LANDevices, row.Imported, row.Registered
		var keys []struct {
			KeyStatus string
			N         int
		}
		if e := tx.Raw(`SELECT key_status,count(*) AS n FROM core.tuya_devices WHERE gateway_id=? AND removed_at IS NULL GROUP BY key_status`, gateway).Scan(&keys).Error; e != nil {
			return e
		}
		for _, k := range keys {
			out.Keys[k.KeyStatus] = k.N
		}
		return nil
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
// zigbee, when not empty, is a Zigbee2MQTT gateway of the same workspace (and in the member's scope) to pair: its MQTT
// password is rotated and handed over with the Edge's when the code is redeemed.
func (r *Repository) CreateEdgeInstallCode(ctx context.Context, p domain.Principal, gateway, zigbee, codeHash string, expires time.Time) error {
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
		var pair any
		if zigbee != "" {
			var zmodels []string
			if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL`, zigbee).Scan(&zmodels).Error; e != nil {
				return e
			}
			if len(zmodels) != 1 || zmodels[0] != domain.Z2MGatewayModel {
				return domain.Because(domain.ErrInvalid, "not_a_zigbee2mqtt_gateway")
			}
			pair = zigbee
		}
		if e := tx.Exec(`DELETE FROM core.edge_install_codes WHERE gateway_id=? AND redeemed_at IS NULL`, gateway).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.edge_install_codes WHERE expires_at<now()-interval '1 day'`).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.edge_install_codes(tenant_id,id,gateway_id,zigbee_gateway_id,code_hash,created_by,expires_at) VALUES(?,?,?,?,?,?,?)`,
			p.TenantID, uuid.NewString(), gateway, pair, codeHash, p.UserID, expires).Error; e != nil {
			return e
		}
		return audit(tx, p, "edge.install_code_created", gateway)
	}))
}

// BootstrapEdge redeems an install code and, in the same transaction, rotates the gateway's MQTT password and
// HTTP token to the given hashes: the credentials the installer receives are the only valid ones from then on, and
// a code that fails half-way is not spent. Unknown, used, expired or revoked all read as ErrUnauthorized.
//
// When the code paired a Zigbee2MQTT gateway, its MQTT password is rotated to zigbeeHash in the same transaction and
// its id returned (empty otherwise); a paired gateway revoked since then makes the whole bootstrap fail, unspent.
func (r *Repository) BootstrapEdge(ctx context.Context, codeHash, mqttHash, zigbeeHash, tokenDigest string) (string, string, string, error) {
	var tenant, gateway, zigbee string
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
		var codes []struct{ CreatedBy, ZigbeeGatewayID sql.NullString }
		if e := tx.Raw(`SELECT created_by::text AS created_by,zigbee_gateway_id::text AS zigbee_gateway_id FROM core.edge_install_codes WHERE code_hash=?`, codeHash).Scan(&codes).Error; e != nil {
			return e
		}
		var creators []sql.NullString
		for _, c := range codes {
			creators = append(creators, c.CreatedBy)
			if c.ZigbeeGatewayID.Valid {
				zigbee = c.ZigbeeGatewayID.String
			}
		}
		if zigbee != "" {
			var zmodels []string
			if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL`, zigbee).Scan(&zmodels).Error; e != nil {
				return e
			}
			if len(zmodels) != 1 || zmodels[0] != domain.Z2MGatewayModel {
				return domain.Because(domain.ErrInvalid, "zigbee_gateway_unavailable")
			}
			if e := setMQTTPassword(tx, tenant, zigbee, zigbeeHash); e != nil {
				return e
			}
		}
		if e := rotateGatewayToken(tx, gateway, tokenDigest); e != nil {
			return e
		}
		if e := setMQTTPassword(tx, tenant, gateway, mqttHash); e != nil {
			return e
		}
		actor := domain.Principal{TenantID: tenant}
		if len(creators) == 1 && creators[0].Valid {
			actor.UserID = creators[0].String
		}
		if zigbee != "" && actor.UserID != "" {
			// The paired Zigbee2MQTT gateway's broker password changed too: its own audit trail says so.
			if e := audit(tx, actor, "zigbee2mqtt.credentials_rotated_by_edge", zigbee); e != nil {
				return e
			}
		}
		if actor.UserID != "" {
			return audit(tx, actor, "edge.bootstrapped", gateway)
		}
		return nil
	})
	return tenant, gateway, zigbee, classify(e)
}

// setMQTTPassword rotates (or creates) a gateway's broker account; the provisioner renders it within seconds.
func setMQTTPassword(tx *gorm.DB, tenant, gateway, hash string) error {
	q := tx.Exec(`UPDATE core.mqtt_accounts SET password_hash=?,revision=revision+1 WHERE gateway_id=?`, hash, gateway)
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected == 0 {
		return tx.Exec(`INSERT INTO core.mqtt_accounts(tenant_id,gateway_id,password_hash) VALUES(?,?,?)`, tenant, gateway, hash).Error
	}
	return nil
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
