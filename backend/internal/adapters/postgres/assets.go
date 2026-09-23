package postgres

import (
	"aether/backend/internal/domain"
	"context"

	"gorm.io/gorm"
)

// The registry merges two operational tables (gateways, devices) with the tenant's own bookkeeping.
// asset_id is polymorphic, so every write proves first that the asset is a live asset of this tenant;
// row level security then keeps the bookkeeping tables inside the workspace.
func assetExists(tx *gorm.DB, kind, id string) error {
	q := `SELECT count(*) FROM core.devices WHERE id=?::uuid AND removed_at IS NULL`
	if kind == "gateway" {
		q = `SELECT count(*) FROM core.gateways WHERE id=?::uuid AND revoked_at IS NULL`
	}
	var n int64
	if e := tx.Raw(q, id).Scan(&n).Error; e != nil {
		return e
	}
	if n != 1 {
		return domain.ErrNotFound
	}
	return nil
}

// assetListSQL is shared by the table and the drawer so both show exactly the same derived columns.
// Device "last seen" is the freshest stream of any gateway that heard the identity (roaming tags move),
// and the battery is the newest decoded reading that actually carries a numeric battery field
// (0 is what tags without a battery frame decode to, so it is treated as unknown, not as empty).
func assetListSQL(single bool) string {
	where := ""
	if single {
		where = ` WHERE b.asset_kind=? AND b.asset_id=?::uuid`
	}
	return `WITH base AS (
   SELECT 'gateway'::text AS asset_kind, g.id AS asset_id, g.name, g.model, ''::text AS external_id,
     ''::text AS gateway_id, ''::text AS gateway_name, coalesce(pr.name,'') AS project_name,
     (SELECT max(gp.received_at) FROM core.gateway_packets gp WHERE gp.gateway_id=g.id) AS last_seen,
     NULL::double precision AS battery, g.created_at
   FROM core.gateways g LEFT JOIN core.projects pr ON pr.id=g.project_id AND pr.archived_at IS NULL
   WHERE g.revoked_at IS NULL
   UNION ALL
   SELECT 'device'::text, d.id, d.name, d.profile_id, d.external_id,
     d.gateway_id::text, gw.name, coalesce(pr.name,''),
     (SELECT max(s.last_seen) FROM core.sensor_streams s WHERE s.external_id=d.external_id),
     (SELECT nullif((sm.reading->>'battery')::double precision,0) FROM core.sensor_samples sm
       WHERE sm.external_id=d.external_id AND sm.reading->>'battery' ~ '^-{0,1}[0-9]{1,}(\.[0-9]{1,}){0,1}$'
       ORDER BY sm.received_at DESC, sm.event_key LIMIT 1),
     d.created_at
   FROM core.devices d JOIN core.gateways gw ON gw.id=d.gateway_id
   LEFT JOIN core.projects pr ON pr.id=gw.project_id AND pr.archived_at IS NULL
   WHERE d.removed_at IS NULL
 ), plan_summary AS (
   SELECT mp.asset_kind, mp.asset_id, min(mp.next_due) AS next_due,
     count(*) FILTER (WHERE mp.next_due < current_date) AS overdue_count
   FROM core.maintenance_plans mp WHERE mp.enabled GROUP BY mp.asset_kind, mp.asset_id
 )
 SELECT b.asset_kind, b.asset_id::text AS asset_id, b.name, b.model, b.external_id, b.gateway_id,
   b.gateway_name, b.project_name, b.last_seen, b.battery, b.created_at,
   coalesce(r.status,'in_service') AS status, coalesce(r.serial_no,'') AS serial_no,
   coalesce(r.asset_tag,'') AS asset_tag, coalesce(r.location_note,'') AS location_note,
   coalesce(r.notes,'') AS notes,
   coalesce(to_char(r.warranty_until,'YYYY-MM-DD'),'') AS warranty_until,
   coalesce(to_char(r.battery_changed_at,'YYYY-MM-DD'),'') AS battery_changed_at,
   coalesce(to_char(ps.next_due,'YYYY-MM-DD'),'') AS next_due,
   coalesce(ps.overdue_count,0) AS overdue_count
 FROM base b
 LEFT JOIN core.asset_records r ON r.asset_kind=b.asset_kind AND r.asset_id=b.asset_id
 LEFT JOIN plan_summary ps ON ps.asset_kind=b.asset_kind AND ps.asset_id=b.asset_id` + where +
		` ORDER BY (b.asset_kind='gateway') DESC, b.name, b.asset_id LIMIT 500`
}

const assetRecordSQL = `SELECT asset_kind, asset_id::text AS asset_id, serial_no, asset_tag, location_note, vendor,
 coalesce(to_char(purchased_at,'YYYY-MM-DD'),'') AS purchased_at,
 coalesce(to_char(warranty_until,'YYYY-MM-DD'),'') AS warranty_until,
 coalesce(to_char(battery_changed_at,'YYYY-MM-DD'),'') AS battery_changed_at,
 status, notes, updated_at FROM core.asset_records WHERE asset_kind=? AND asset_id=?::uuid`

const assetPlansSQL = `SELECT id::text AS id, asset_kind, asset_id::text AS asset_id, title, kind, interval_days,
 to_char(next_due,'YYYY-MM-DD') AS next_due, coalesce(to_char(last_done,'YYYY-MM-DD'),'') AS last_done,
 enabled, created_at FROM core.maintenance_plans WHERE asset_kind=? AND asset_id=?::uuid ORDER BY enabled DESC, next_due, id`

const assetLogsSQL = `SELECT id::text AS id, asset_kind, asset_id::text AS asset_id, plan_id::text AS plan_id,
 kind, title, detail, performed_at, performed_by, cost::double precision AS cost, created_at
 FROM core.maintenance_logs WHERE asset_kind=? AND asset_id=?::uuid ORDER BY performed_at DESC, id LIMIT 100`

// ListAssets returns every live gateway and every active registration of the workspace, at most 500 rows.
func (r *Repository) ListAssets(ctx context.Context, p domain.Principal) ([]domain.AssetRow, error) {
	out := []domain.AssetRow{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(assetListSQL(false)).Scan(&out).Error
	})
	return out, e
}

// GetAsset is everything the drawer shows: the merged row, the editable record, its plans and the last 100 logs.
func (r *Repository) GetAsset(ctx context.Context, p domain.Principal, kind, id string) (domain.AssetDetail, error) {
	out := domain.AssetDetail{Plans: []domain.MaintenancePlan{}, Logs: []domain.MaintenanceLog{}}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		rows := []domain.AssetRow{}
		if e := tx.Raw(assetListSQL(true), kind, id).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrNotFound
		}
		out.Asset = rows[0]
		records := []domain.AssetRecord{}
		if e := tx.Raw(assetRecordSQL, kind, id).Scan(&records).Error; e != nil {
			return e
		}
		if len(records) == 1 {
			out.Record = records[0]
		} else { // Nothing saved yet: the drawer opens on an empty form with the default status.
			out.Record = domain.AssetRecord{AssetKind: kind, AssetID: id, Status: "in_service"}
		}
		if e := tx.Raw(assetPlansSQL, kind, id).Scan(&out.Plans).Error; e != nil {
			return e
		}
		return tx.Raw(assetLogsSQL, kind, id).Scan(&out.Logs).Error
	})
	return out, e
}

func (r *Repository) UpsertAssetRecord(ctx context.Context, p domain.Principal, rec domain.AssetRecord) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := assetExists(tx, rec.AssetKind, rec.AssetID); e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.asset_records(tenant_id,asset_kind,asset_id,serial_no,asset_tag,location_note,vendor,purchased_at,warranty_until,battery_changed_at,status,notes,updated_at,updated_by)
       VALUES(?,?,?::uuid,?,?,?,?,nullif(?::text,'')::date,nullif(?::text,'')::date,nullif(?::text,'')::date,?,?,now(),?::uuid)
       ON CONFLICT(tenant_id,asset_kind,asset_id) DO UPDATE SET serial_no=EXCLUDED.serial_no,asset_tag=EXCLUDED.asset_tag,
       location_note=EXCLUDED.location_note,vendor=EXCLUDED.vendor,purchased_at=EXCLUDED.purchased_at,
       warranty_until=EXCLUDED.warranty_until,battery_changed_at=EXCLUDED.battery_changed_at,status=EXCLUDED.status,
       notes=EXCLUDED.notes,updated_at=now(),updated_by=EXCLUDED.updated_by`,
			p.TenantID, rec.AssetKind, rec.AssetID, rec.SerialNo, rec.AssetTag, rec.LocationNote, rec.Vendor,
			rec.PurchasedAt, rec.WarrantyUntil, rec.BatteryChangedAt, rec.Status, rec.Notes, p.UserID).Error; e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "asset.record_saved", rec.AssetID)
	}))
}

func (r *Repository) CreatePlan(ctx context.Context, p domain.Principal, plan domain.MaintenancePlan) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := assetExists(tx, plan.AssetKind, plan.AssetID); e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.maintenance_plans WHERE asset_kind=? AND asset_id=?::uuid`, plan.AssetKind, plan.AssetID).Scan(&n).Error; e != nil {
			return e
		}
		if n >= 20 {
			return domain.ErrConflict
		}
		if e := tx.Exec(`INSERT INTO core.maintenance_plans(tenant_id,id,asset_kind,asset_id,title,kind,interval_days,next_due,enabled)
       VALUES(?,?::uuid,?,?::uuid,?,?,?,?::date,?)`,
			p.TenantID, plan.ID, plan.AssetKind, plan.AssetID, plan.Title, plan.Kind, plan.IntervalDays, plan.NextDue, plan.Enabled).Error; e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "asset.plan_created", plan.ID)
	}))
}

func (r *Repository) UpdatePlan(ctx context.Context, p domain.Principal, plan domain.MaintenancePlan) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE core.maintenance_plans SET title=?,kind=?,interval_days=?,next_due=?::date,enabled=? WHERE id=?::uuid`,
			plan.Title, plan.Kind, plan.IntervalDays, plan.NextDue, plan.Enabled, plan.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "asset.plan_updated", plan.ID)
	}))
}

func (r *Repository) DeletePlan(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`DELETE FROM core.maintenance_plans WHERE id=?::uuid`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "asset.plan_deleted", id)
	})
}

// AddLog appends one maintenance entry. When it is bound to a plan and actually reports work done, the plan
// moves forward in the same transaction (last_done = the day of the work, next_due = that day + interval_days),
// so a round can never be recorded without rescheduling. A battery swap also updates the record.
func (r *Repository) AddLog(ctx context.Context, p domain.Principal, log domain.MaintenanceLog) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := assetExists(tx, log.AssetKind, log.AssetID); e != nil {
			return e
		}
		if log.PlanID != nil {
			var n int64
			if e := tx.Raw(`SELECT count(*) FROM core.maintenance_plans WHERE id=?::uuid AND asset_kind=? AND asset_id=?::uuid`, *log.PlanID, log.AssetKind, log.AssetID).Scan(&n).Error; e != nil {
				return e
			}
			if n != 1 {
				return domain.ErrNotFound
			}
		}
		if e := tx.Exec(`INSERT INTO core.maintenance_logs(tenant_id,id,asset_kind,asset_id,plan_id,kind,title,detail,performed_at,performed_by,cost,created_by)
       VALUES(?,?::uuid,?,?::uuid,?::uuid,?,?,?,?,?,?,?::uuid)`,
			p.TenantID, log.ID, log.AssetKind, log.AssetID, log.PlanID, log.Kind, log.Title, log.Detail,
			log.PerformedAt, log.PerformedBy, log.Cost, p.UserID).Error; e != nil {
			return e
		}
		day := log.PerformedAt.UTC().Format("2006-01-02")
		if log.PlanID != nil && domain.PlanCompletedBy(log.Kind) {
			if e := tx.Exec(`UPDATE core.maintenance_plans SET last_done=?::date, next_due=?::date+interval_days WHERE id=?::uuid`, day, day, *log.PlanID).Error; e != nil {
				return e
			}
		}
		if log.Kind == "battery" {
			if e := tx.Exec(`INSERT INTO core.asset_records(tenant_id,asset_kind,asset_id,battery_changed_at,updated_at,updated_by)
         VALUES(?,?,?::uuid,?::date,now(),?::uuid)
         ON CONFLICT(tenant_id,asset_kind,asset_id) DO UPDATE SET battery_changed_at=EXCLUDED.battery_changed_at,updated_at=now(),updated_by=EXCLUDED.updated_by`,
				p.TenantID, log.AssetKind, log.AssetID, day, p.UserID).Error; e != nil {
				return e
			}
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "asset.log_added", log.ID)
	}))
}
