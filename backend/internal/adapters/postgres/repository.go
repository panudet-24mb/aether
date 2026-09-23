package postgres

import (
	"aether/backend/internal/adapters/minew"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"aether/backend/internal/domain"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Repository struct {
	db   *gorm.DB
	opts Options
}

// Options are deployment choices that change what ingest stores and whether alerts act. Zero values are the
// development defaults (keep everything, act on everything).
type Options struct {
	SampleRetentionDays  int
	SampleMinIntervalSec int
	BLEHistoryHours      int
	DiscoveryLimit       int
	AlertsShadow         bool
}

// Configure must be called before the repository is shared between goroutines.
func (r *Repository) Configure(o Options) {
	if o.DiscoveryLimit == 0 {
		o.DiscoveryLimit = 100
	}
	r.opts = o
}

func Open(dsn string) (*Repository, error) {
	db, e := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if e != nil {
		return nil, errors.New("database connection failed")
	}
	sqlDB, e := db.DB()
	if e != nil {
		return nil, e
	}
	sqlDB.SetMaxOpenConns(12)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = sqlDB.PingContext(ctx); e != nil {
		return nil, e
	}
	var unsafe bool
	e = db.Raw(`SELECT rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR pg_has_role(current_user,'aether_owner','MEMBER') FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe).Error
	if e != nil || unsafe {
		sqlDB.Close()
		return nil, errors.New("API database role must be unprivileged and not an owner")
	}
	var count int64
	if e := db.Raw(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='core' AND c.relkind='r'`).Scan(&count).Error; e != nil || count < 8 {
		sqlDB.Close()
		return nil, errors.New("database migrations required")
	}
	var bad int64
	e = db.Raw(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='core' AND c.relkind='r' AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity)`).Scan(&bad).Error
	if e != nil || bad > 0 {
		sqlDB.Close()
		return nil, errors.New("RLS is required on every tenant table")
	}
	return &Repository{db: db, opts: Options{DiscoveryLimit: 100}}, nil
}
func (r *Repository) Close() error {
	db, e := r.db.DB()
	if e != nil {
		return e
	}
	return db.Close()
}
func (r *Repository) Ready(ctx context.Context) error {
	db, e := r.db.DB()
	if e != nil {
		return e
	}
	return db.PingContext(ctx)
}
func (r *Repository) tx(ctx context.Context, user, tenant string, fn func(*gorm.DB) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT set_config('app.user_id',?,true),set_config('app.tenant_id',?,true)`, user, tenant).Error; e != nil {
			return e
		}
		// Project access is decided by the database from the identity above, never passed in from Go:
		// '*' for system work (empty user id), for owner/admin and for members without a restriction,
		// otherwise the member's project ids. The RESTRICTIVE policies of migration 00019 read this one
		// transaction-local setting, and deny everything when it was never computed.
		if e := tx.Exec(`SELECT set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`).Error; e != nil {
			return e
		}
		return fn(tx)
	})
}

// tenantLimit reads a per-workspace capacity column (max_gateways | max_devices) of the current tenant.
func tenantLimit(tx *gorm.DB, column string) (int64, error) {
	if column != "max_gateways" && column != "max_devices" {
		return 0, errors.New("unknown limit")
	}
	var rows []struct{ N int64 }
	if e := tx.Raw(`SELECT ` + column + ` AS n FROM core.tenants WHERE id=core.tenant_id()`).Scan(&rows).Error; e != nil {
		return 0, e
	}
	if len(rows) != 1 {
		return 0, domain.ErrNotFound
	}
	return rows[0].N, nil
}

func classify(e error) error {
	if errors.Is(e, gorm.ErrDuplicatedKey) {
		return domain.ErrConflict
	}
	if errors.Is(e, gorm.ErrForeignKeyViolated) {
		return domain.ErrInvalid
	}
	return e
}
func audit(tx *gorm.DB, p domain.Principal, action, target string) error {
	return tx.Exec(`INSERT INTO core.audit_logs(id,tenant_id,actor_id,action,target_id) VALUES(?,?,?,?,?)`, uuid.NewString(), p.TenantID, p.UserID, action, target).Error
}
func (r *Repository) CreateAccount(ctx context.Context, u domain.User, tenant, name string, bootstrap bool) error {
	e := r.tx(ctx, u.ID, tenant, func(tx *gorm.DB) error {
		if bootstrap { // Global advisory lock plus global identity count makes the local bootstrap one-time.
			if e := tx.Exec(`SELECT pg_advisory_xact_lock(42870111)`).Error; e != nil {
				return e
			}
			var count int64
			if e := tx.Raw(`SELECT count(*) FROM identity.users`).Scan(&count).Error; e != nil {
				return e
			}
			if count != 0 {
				return domain.ErrConflict
			}
		}
		if e := tx.Exec(`INSERT INTO identity.users(id,email,name,password_hash) VALUES(?,?,?,?)`, u.ID, u.Email, u.Name, u.PasswordHash).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.tenants(id,name) VALUES(?,?)`, tenant, name).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.memberships(tenant_id,user_id,role) VALUES(?,?,'owner')`, tenant, u.ID).Error; e != nil {
			return e
		}
		// A new workspace must alert on an emergency button press before anybody configures anything.
		if e := seedDefaultRules(tx, tenant); e != nil {
			return e
		}
		return audit(tx, domain.Principal{UserID: u.ID, TenantID: tenant}, "account.created", u.ID)
	})
	return classify(e)
}
func (r *Repository) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	var u domain.User
	res := r.db.WithContext(ctx).Raw(`SELECT id,email,name,password_hash FROM identity.users WHERE email=?`, email).Scan(&u)
	if res.Error != nil {
		return u, res.Error
	}
	if res.RowsAffected == 0 {
		return u, domain.ErrUnauthorized
	}
	return u, nil
}
func (r *Repository) StartSession(ctx context.Context, s domain.Session, digest string) (domain.Session, error) {
	e := r.tx(ctx, s.UserID, s.TenantID, func(tx *gorm.DB) error {
		var memberships []struct{ TenantID string }
		if e := tx.Raw(`SELECT tenant_id FROM core.memberships WHERE user_id=? ORDER BY tenant_id`, s.UserID).Scan(&memberships).Error; e != nil {
			return e
		}
		if s.TenantID == "" && len(memberships) == 1 {
			s.TenantID = memberships[0].TenantID
		}
		allowed := false
		for _, m := range memberships {
			if m.TenantID == s.TenantID {
				allowed = true
			}
		}
		if !allowed {
			return domain.ErrUnauthorized
		}
		// The workspace is only known here, so the project scope of this transaction is recomputed with it.
		if e := tx.Exec(`SELECT set_config('app.tenant_id',?,true),set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`, s.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.tenants WHERE id=? AND status='active'`, s.TenantID).Scan(&n).Error; e != nil {
			return e
		}
		if n != 1 {
			return domain.ErrUnauthorized
		}
		// An initial or reset password was handed over in person; the UI must force a replacement.
		if e := tx.Raw(`SELECT must_change_password FROM core.memberships WHERE tenant_id=? AND user_id=?`, s.TenantID, s.UserID).Scan(&s.MustChangePassword).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO identity.sessions(id,user_id,tenant_id,expires_at) VALUES(?,?,?,?)`, s.ID, s.UserID, s.TenantID, s.ExpiresAt).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO identity.refresh_tokens(hash,user_id,session_id) VALUES(?,?,?)`, digest, s.UserID, s.ID).Error; e != nil {
			return e
		}
		return audit(tx, domain.Principal{UserID: s.UserID, TenantID: s.TenantID}, "session.created", s.ID)
	})
	return s, e
}
func (r *Repository) RotateRefresh(ctx context.Context, user, oldHash, newHash string, now time.Time) (domain.Session, error) {
	var s domain.Session
	reused := false
	e := r.tx(ctx, user, "", func(tx *gorm.DB) error {
		var row struct {
			SessionID  string
			ConsumedAt *time.Time
		}
		res := tx.Raw(`SELECT session_id,consumed_at FROM identity.refresh_tokens WHERE hash=? AND user_id=? FOR UPDATE`, oldHash, user).Scan(&row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrUnauthorized
		}
		var sr struct {
			ID, UserID, TenantID string
			ExpiresAt            time.Time
			RevokedAt            *time.Time
		}
		res = tx.Raw(`SELECT id,user_id,tenant_id,expires_at,revoked_at FROM identity.sessions WHERE id=? FOR UPDATE`, row.SessionID).Scan(&sr)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrUnauthorized
		}
		// Do not return an error from the transaction when detecting reuse: commit revocation first.
		if row.ConsumedAt != nil {
			reused = true
			return tx.Exec(`UPDATE identity.sessions SET revoked_at=coalesce(revoked_at,?) WHERE id=?`, now, sr.ID).Error
		}
		if sr.RevokedAt != nil || !sr.ExpiresAt.After(now) {
			return domain.ErrUnauthorized
		}
		// The workspace is only known here, so the project scope of this transaction is recomputed with it.
		if e := tx.Exec(`SELECT set_config('app.tenant_id',?,true),set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`, sr.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.tenants t JOIN core.memberships m ON m.tenant_id=t.id WHERE t.id=? AND t.status='active' AND m.user_id=?`, sr.TenantID, user).Scan(&n).Error; e != nil {
			return e
		}
		if n != 1 {
			return domain.ErrUnauthorized
		}
		var mustChange bool
		if e := tx.Raw(`SELECT must_change_password FROM core.memberships WHERE tenant_id=? AND user_id=?`, sr.TenantID, user).Scan(&mustChange).Error; e != nil {
			return e
		}
		if e := tx.Exec(`UPDATE identity.refresh_tokens SET consumed_at=? WHERE hash=?`, now, oldHash).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO identity.refresh_tokens(hash,user_id,session_id) VALUES(?,?,?)`, newHash, user, sr.ID).Error; e != nil {
			return e
		}
		s = domain.Session{ID: sr.ID, UserID: sr.UserID, TenantID: sr.TenantID, ExpiresAt: sr.ExpiresAt, MustChangePassword: mustChange}
		return nil
	})
	if reused {
		return s, domain.ErrUnauthorized
	}
	return s, e
}
func (r *Repository) Authorize(ctx context.Context, p domain.Principal) (domain.Principal, error) {
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		// must_change_password rides along so an initial password can be enforced per request, not advised.
		var row struct {
			Role               string
			MustChangePassword bool
		}
		res := tx.Raw(`SELECT m.role,m.must_change_password FROM core.memberships m JOIN core.tenants t ON t.id=m.tenant_id JOIN identity.sessions s ON s.user_id=m.user_id AND s.tenant_id=m.tenant_id WHERE s.id=? AND m.user_id=? AND m.tenant_id=? AND t.status='active' AND s.revoked_at IS NULL AND s.expires_at>now()`, p.SessionID, p.UserID, p.TenantID).Scan(&row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrUnauthorized
		}
		p.Role, p.MustChangePassword = row.Role, row.MustChangePassword
		return nil
	})
	return p, e
}
func (r *Repository) RevokeSession(ctx context.Context, p domain.Principal) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`UPDATE identity.sessions SET revoked_at=now() WHERE id=? AND user_id=?`, p.SessionID, p.UserID).Error; e != nil {
			return e
		}
		return audit(tx, p, "session.revoked", p.SessionID)
	})
}
func (r *Repository) CreateGateway(ctx context.Context, p domain.Principal, g domain.Gateway, hash string) error {
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.gateways`).Scan(&n).Error; e != nil {
			return e
		}
		if limit, e := tenantLimit(tx, "max_gateways"); e != nil {
			return e
		} else if n >= limit {
			return domain.ErrConflict
		}
		if g.ProjectID != nil {
			var active int64
			if e := tx.Raw(`SELECT count(*) FROM core.projects WHERE id=? AND archived_at IS NULL`, *g.ProjectID).Scan(&active).Error; e != nil {
				return e
			}
			if active != 1 {
				return domain.ErrNotFound
			}
		}
		if e := tx.Exec(`INSERT INTO core.gateways(id,tenant_id,name,model,token_hash,project_id) VALUES(?,?,?,?,?,?)`, g.ID, p.TenantID, g.Name, g.Model, hash, g.ProjectID).Error; e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", g.ID); e != nil {
			return e
		}
		return audit(tx, p, "gateway.created", g.ID)
	})
	return classify(e)
}
func (r *Repository) ListGateways(ctx context.Context, p domain.Principal) ([]domain.Gateway, error) {
	out := []domain.Gateway{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT id,tenant_id,name,model,created_at,project_id FROM core.gateways WHERE revoked_at IS NULL ORDER BY created_at DESC LIMIT 50`).Scan(&out).Error
	})
	return out, e
}
func (r *Repository) RevokeGateway(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE core.gateways SET revoked_at=now() WHERE id=? AND revoked_at IS NULL`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", id); e != nil {
			return e
		}
		return audit(tx, p, "gateway.revoked", id)
	})
}
func (r *Repository) GatewayTenant(ctx context.Context, id, digest string) (string, error) {
	var tenant string
	res := r.db.WithContext(ctx).Raw(`SELECT tenant_id FROM core.lookup_gateway(?,?)`, id, digest).Scan(&tenant)
	if res.Error != nil {
		return "", res.Error
	}
	if res.RowsAffected != 1 {
		return "", domain.ErrUnauthorized
	}
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var active int64
		if e := tx.Raw(`SELECT count(*) FROM core.tenants WHERE id=? AND status='active'`, tenant).Scan(&active).Error; e != nil {
			return e
		}
		if active != 1 {
			return domain.ErrUnauthorized
		}
		return nil
	})
	return tenant, e
}
func (r *Repository) CreateDevice(ctx context.Context, p domain.Principal, d domain.Device) error {
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.devices WHERE removed_at IS NULL`).Scan(&n).Error; e != nil {
			return e
		}
		if limit, e := tenantLimit(tx, "max_devices"); e != nil {
			return e
		} else if n >= limit {
			return domain.ErrConflict
		}
		res := tx.Exec(`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id) SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM core.gateways WHERE id=? AND revoked_at IS NULL)`, d.ID, p.TenantID, d.GatewayID, d.Name, d.ExternalID, d.ProfileID, d.GatewayID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", d.GatewayID); e != nil {
			return e
		}
		return audit(tx, p, "device.created", d.ID)
	})
	return classify(e)
}
func (r *Repository) ListDevices(ctx context.Context, p domain.Principal) ([]domain.Device, error) {
	out := []domain.Device{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT d.id,d.tenant_id,d.gateway_id,d.name,d.external_id,d.profile_id,d.roaming,d.created_at,
      (SELECT ps.gateway_id FROM core.presence_state ps WHERE d.roaming AND ps.tenant_id=d.tenant_id AND ps.external_id=lower(d.external_id)) AS zone_gateway_id
      FROM core.devices d WHERE d.removed_at IS NULL ORDER BY d.created_at DESC LIMIT 100`).Scan(&out).Error
	})
	return out, e
}
func (r *Repository) DeviceState(ctx context.Context, p domain.Principal, id string) (domain.State, error) {
	var s domain.State
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Raw(`SELECT s.device_id,s.ts,s.metrics FROM core.device_state s JOIN core.devices d ON d.id=s.device_id AND d.tenant_id=s.tenant_id WHERE s.device_id=? AND d.removed_at IS NULL`, id).Scan(&s)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return nil
	})
	return s, e
}
func (r *Repository) CapturePacket(ctx context.Context, tenant, gateway string, payload json.RawMessage) (string, error) {
	id := uuid.NewString()
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		// Serialize capture/pruning per gateway; keep at most 100 diagnostic packets.
		var n int64
		res := tx.Raw(`SELECT count(*) FROM (SELECT id FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR UPDATE) g`, gateway).Scan(&n)
		if res.Error != nil {
			return res.Error
		}
		if n != 1 {
			return domain.ErrUnauthorized
		}
		if e := tx.Exec(`INSERT INTO core.gateway_packets(id,tenant_id,gateway_id,payload) VALUES(?,?,?,?::jsonb)`, id, tenant, gateway, string(payload)).Error; e != nil {
			return e
		}
		now := time.Now().UTC()
		// raws is this packet's distinct advertisements per advertiser, built while they are archived.
		// Learned signals are matched against it inside saveEvents, so nothing re-parses the payload.
		raws, e := saveBLEHistory(tx, tenant, gateway, payload, now)
		if e != nil {
			return e
		}
		// Identities this workspace already tracks keep their stream even on an uplink that carries only
		// frames Aether cannot decode (see minew.ProjectKnown). Bounded: streams are capped per gateway.
		var tracked []string
		if e := tx.Raw(`SELECT external_id FROM core.sensor_streams WHERE gateway_id=? UNION SELECT lower(external_id) FROM core.devices WHERE removed_at IS NULL LIMIT 5000`, gateway).Scan(&tracked).Error; e != nil {
			return e
		}
		known := make(map[string]bool, len(tracked))
		for _, id := range tracked {
			known[id] = true
		}
		view := minew.ProjectKnown(domain.Gateway{}, []domain.Packet{{Payload: payload, ReceivedAt: now}}, func(mac string) bool { return known[mac] })
		// A taught door signal sets the `door` metric before the samples are stored. It must never cost
		// telemetry either: on failure the readings are stored without it.
		if e := tx.SavePoint("learned_doors").Error; e != nil {
			return e
		}
		if e := applyLearnedDoors(tx, &view, raws); e != nil {
			slog.Warn("learned door evaluation failed; packet stored without door state", "gateway", gateway, "error", e.Error())
			if e := tx.RollbackTo("learned_doors").Error; e != nil {
				return e
			}
		}
		if e := saveSamples(tx, tenant, gateway, view, payload, now, r.opts); e != nil {
			return e
		}
		// Alerting must never cost telemetry: a failure here rolls back only the alert work, not the packet.
		if e := tx.SavePoint("alert_events").Error; e != nil {
			return e
		}
		if e := saveEvents(tx, tenant, gateway, view, raws, now, r.opts); e != nil {
			slog.Warn("alert evaluation failed; packet stored without events", "gateway", gateway, "error", e.Error())
			if e := tx.RollbackTo("alert_events").Error; e != nil {
				return e
			}
		}
		if e := signal(tx, tenant, "packet", gateway); e != nil {
			return e
		}
		return tx.Exec(`DELETE FROM core.gateway_packets WHERE gateway_id=? AND id IN (SELECT id FROM core.gateway_packets WHERE gateway_id=? ORDER BY received_at DESC,id DESC OFFSET 100)`, gateway, gateway).Error
	})
	return id, e
}
func (r *Repository) ListPackets(ctx context.Context, p domain.Principal, gateway string) ([]domain.Packet, error) {
	out := []domain.Packet{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT id,gateway_id,received_at,payload FROM core.gateway_packets WHERE gateway_id=? ORDER BY received_at DESC LIMIT 20`, gateway).Scan(&out).Error
	})
	return out, e
}
func (r *Repository) StoreTelemetry(ctx context.Context, tenant, gateway, device string, ts time.Time, metrics map[string]float64) (domain.TelemetryEvent, error) {
	event := domain.TelemetryEvent{Schema: "telemetry.v1", TenantID: tenant, DeviceID: device, TS: ts, ReceivedAt: time.Now().UTC(), Metrics: metrics}
	payload, e := json.Marshal(metrics)
	if e != nil {
		return event, domain.ErrInvalid
	}
	e = r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		// Lock/revalidate gateway so revocation and ingestion have a definite order.
		var g string
		res := tx.Raw(`SELECT id FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR SHARE`, gateway).Scan(&g)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrUnauthorized
		}
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,1))`, device).Error; e != nil {
			return e
		}
		res = tx.Raw(`SELECT profile_id FROM core.devices WHERE id=? AND gateway_id=? AND removed_at IS NULL`, device, gateway).Scan(&event.ProfileID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if event.ProfileID != "generic-environment@1" {
			return domain.ErrInvalid
		}
		insert := tx.Exec(`INSERT INTO core.telemetry(tenant_id,device_id,ts,received_at,metrics) VALUES(?,?,?,?,?::jsonb) ON CONFLICT DO NOTHING`, tenant, device, ts, event.ReceivedAt, string(payload))
		if insert.Error != nil {
			return insert.Error
		}
		if insert.RowsAffected == 0 {
			var stored struct {
				TS         time.Time
				ReceivedAt time.Time
				Metrics    json.RawMessage
			}
			if e := tx.Raw(`SELECT ts,received_at,metrics FROM core.telemetry WHERE device_id=? AND ts=?`, device, ts).Scan(&stored).Error; e != nil {
				return e
			}
			event.TS = stored.TS
			event.ReceivedAt = stored.ReceivedAt
			return json.Unmarshal(stored.Metrics, &event.Metrics)
		}
		// Latest state never regresses on out-of-order or duplicate packets.
		if e := tx.Exec(`INSERT INTO core.device_state(tenant_id,device_id,ts,metrics) VALUES(?,?,?,?::jsonb) ON CONFLICT(tenant_id,device_id) DO UPDATE SET ts=excluded.ts,metrics=excluded.metrics WHERE excluded.ts>core.device_state.ts`, tenant, device, ts, string(payload)).Error; e != nil {
			return e
		}
		return tx.Exec(`DELETE FROM core.telemetry WHERE device_id=? AND ts IN (SELECT ts FROM core.telemetry WHERE device_id=? ORDER BY ts DESC OFFSET 10000)`, device, device).Error
	})
	return event, e
}
func (r *Repository) String() string { return fmt.Sprintf("PostgreSQL repository (RLS enforced)") }

// ListRemovedDevices returns withdrawn registrations (newest first) so they can be reviewed or restored.
func (r *Repository) ListRemovedDevices(ctx context.Context, p domain.Principal) ([]domain.Device, error) {
	out := []domain.Device{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT id,tenant_id,gateway_id,name,external_id,profile_id,roaming,created_at,removed_at FROM core.devices WHERE removed_at IS NOT NULL ORDER BY removed_at DESC LIMIT 100`).Scan(&out).Error
	})
	return out, e
}

// UpdateDevice renames and/or moves an active registration to another active gateway of the same tenant.
func (r *Repository) UpdateDevice(ctx context.Context, p domain.Principal, id string, name, gateway *string) (domain.Device, error) {
	var d domain.Device
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Raw(`SELECT id,tenant_id,gateway_id,name,external_id,profile_id,roaming,created_at FROM core.devices WHERE id=? AND removed_at IS NULL FOR UPDATE`, id).Scan(&d)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		action := "device.renamed"
		if gateway != nil && *gateway != d.GatewayID {
			var n int64
			if e := tx.Raw(`SELECT count(*) FROM (SELECT id FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR SHARE) g`, *gateway).Scan(&n).Error; e != nil {
				return e
			}
			if n != 1 {
				return domain.ErrNotFound
			}
			d.GatewayID = *gateway
			action = "device.moved"
		}
		if name != nil {
			d.Name = *name
		}
		if e := tx.Exec(`UPDATE core.devices SET name=?,gateway_id=? WHERE id=?`, d.Name, d.GatewayID, id).Error; e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", d.GatewayID); e != nil {
			return e
		}
		return audit(tx, p, action, id)
	})
	return d, classify(e)
}

// RemoveDevice withdraws a registration; telemetry and state rows are kept for a later restore.
func (r *Repository) RemoveDevice(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		// A withdrawn registration leaves the plan and loses its remembered zone.
		if e := tx.Exec(`DELETE FROM core.floor_placements WHERE asset_kind='device' AND asset_id=?`, id).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.presence_state ps USING core.devices d WHERE d.id=? AND ps.tenant_id=d.tenant_id AND ps.external_id=lower(d.external_id)
      AND NOT EXISTS(SELECT 1 FROM core.devices o WHERE o.tenant_id=d.tenant_id AND lower(o.external_id)=lower(d.external_id) AND o.id<>d.id AND o.roaming AND o.removed_at IS NULL)`, id).Error; e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.devices SET removed_at=now(),removed_by=? WHERE id=? AND removed_at IS NULL`, p.UserID, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "device.removed", id)
	})
}

// RestoreDevice re-activates a withdrawn registration unless its identity was adopted again meanwhile.
func (r *Repository) RestoreDevice(ctx context.Context, p domain.Principal, id string) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.devices WHERE removed_at IS NULL`).Scan(&n).Error; e != nil {
			return e
		}
		if limit, e := tenantLimit(tx, "max_devices"); e != nil {
			return e
		} else if n >= limit {
			return domain.ErrConflict
		}
		res := tx.Exec(`UPDATE core.devices d SET removed_at=NULL,removed_by=NULL WHERE d.id=? AND d.removed_at IS NOT NULL AND EXISTS(SELECT 1 FROM core.gateways g WHERE g.id=d.gateway_id AND g.revoked_at IS NULL)`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "device.restored", id)
	}))
}
