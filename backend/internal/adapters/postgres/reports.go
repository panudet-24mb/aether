package postgres

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/reports"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Reports and data export (docs/platform/reports.md). Everything reads through r.tx, so tenant isolation and project
// scope are the database's (RLS); wearables and roaming tags are refused here before any data is read.

// ReportSensors lists the sensors the caller can report on: live registrations on a live gateway, not carried by
// people (core.twin_impersonal_tags), of a profile that can carry a temperature or humidity.
func (r *Repository) ReportSensors(ctx context.Context, p domain.Principal) ([]domain.ReportSensorInfo, error) {
	out := []domain.ReportSensorInfo{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []domain.ReportSensorInfo
		if e := tx.Raw(`SELECT DISTINCT ON (d.gateway_id, lower(d.external_id)) d.gateway_id::text AS gateway_id, lower(d.external_id) AS external_id,
       d.name, g.name AS gateway_name, g.project_id::text AS project_id, d.profile_id
      FROM core.devices d JOIN core.gateways g ON g.tenant_id=d.tenant_id AND g.id=d.gateway_id
      WHERE d.removed_at IS NULL AND g.revoked_at IS NULL
      ORDER BY d.gateway_id, lower(d.external_id), d.created_at`).Scan(&rows).Error; e != nil {
			return e
		}
		ids := make([]string, 0, len(rows))
		for _, s := range rows {
			ids = append(ids, s.ExternalID)
		}
		personal, e := personalSet(tx, ids)
		if e != nil {
			return e
		}
		for _, s := range rows {
			if !personal(s.ExternalID) && domain.ReportCandidate(s.ProfileID) {
				out = append(out, s)
			}
		}
		return nil
	})
	return out, e
}

// resolveReportSensors checks every sensor (already normalised, domain.NormaliseReportSensors) and returns them with
// their names, in order. A sensor is there when its gateway is visible and it is registered on it or the gateway
// heard it (a stream); otherwise 404 sensor_not_found, also for another workspace's or another project's sensor. A
// tag that is not a fixed sensor is refused: personal_sensor when it is registered as a wearable or roaming,
// sensor_not_registered when it has no live registration at all (it may be anyone's tag).
func resolveReportSensors(tx *gorm.DB, sensors []domain.ReportSensor) ([]domain.ReportSensorInfo, error) {
	gateways, externals := make([]string, 0, len(sensors)), make([]string, 0, len(sensors))
	for _, s := range sensors {
		gateways, externals = append(gateways, s.GatewayID), append(externals, s.ExternalID)
	}
	var gws []struct {
		ID, Name  string
		ProjectID *string
	}
	if e := tx.Raw(`SELECT id::text AS id, name, project_id::text AS project_id FROM core.gateways WHERE id = ANY(?::uuid[])`, pgArray(distinct(gateways))).Scan(&gws).Error; e != nil {
		return nil, e
	}
	var known []struct {
		GatewayID, ExternalID, Name, ProfileID string
		Device                                 bool
	}
	if e := tx.Raw(`SELECT DISTINCT ON (gateway_id, external_id, device) * FROM (
       SELECT d.gateway_id::text AS gateway_id, lower(d.external_id) AS external_id, d.name, d.profile_id, true AS device, d.created_at AS at
        FROM core.devices d WHERE d.removed_at IS NULL AND d.gateway_id = ANY(?::uuid[]) AND lower(d.external_id) = ANY(?::text[])
       UNION ALL
       SELECT s.gateway_id::text, lower(s.external_id), s.name, '', false, s.last_seen
        FROM core.sensor_streams s WHERE s.gateway_id = ANY(?::uuid[]) AND lower(s.external_id) = ANY(?::text[])
      ) x ORDER BY gateway_id, external_id, device, at`,
		pgArray(distinct(gateways)), pgTextArray(externals), pgArray(distinct(gateways)), pgTextArray(externals)).Scan(&known).Error; e != nil {
		return nil, e
	}
	personal, e := personalSet(tx, distinct(externals))
	if e != nil {
		return nil, e
	}
	out := make([]domain.ReportSensorInfo, 0, len(sensors))
	for _, s := range sensors {
		info := domain.ReportSensorInfo{GatewayID: s.GatewayID, ExternalID: s.ExternalID}
		gateway := false
		for _, g := range gws {
			if g.ID == s.GatewayID {
				gateway, info.GatewayName, info.ProjectID = true, g.Name, g.ProjectID
			}
		}
		registered, heard := false, false
		for _, k := range known {
			if k.GatewayID != s.GatewayID || k.ExternalID != s.ExternalID {
				continue
			}
			if k.Device {
				registered, info.Name, info.ProfileID = true, k.Name, k.ProfileID
			} else {
				heard = true
				if info.Name == "" {
					info.Name = k.Name
				}
			}
		}
		switch {
		case !gateway || (!registered && !heard):
			return nil, domain.Because(domain.ErrNotFound, "sensor_not_found")
		case personal(s.ExternalID) && registered:
			return nil, domain.Because(domain.ErrInvalid, "personal_sensor")
		case personal(s.ExternalID):
			return nil, domain.Because(domain.ErrInvalid, "sensor_not_registered")
		}
		out = append(out, info)
	}
	return out, nil
}

// ResolveReportSensors is resolveReportSensors in its own transaction: what an export checks before it streams.
func (r *Repository) ResolveReportSensors(ctx context.Context, p domain.Principal, sensors []domain.ReportSensor) ([]domain.ReportSensorInfo, error) {
	var out []domain.ReportSensorInfo
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var e error
		out, e = resolveReportSensors(tx, sensors)
		return e
	})
	return out, e
}

// The reading rules of core.rollup_range (00046): a Zigbee2MQTT-shaped reading takes temperature, humidity and
// battery from `metrics`; any other carries temperature and humidity only as an environment frame, a battery only
// above 0. Rows with none of the three are left out.
// Only samples of a 5-minute bucket the rollup holds are exported: the rollup writes buckets only for a tag that was a
// fixed sensor when they were computed, so history from a time the tag was worn or roaming (personal data, deleted
// from the rollup then) stays out even after it was turned into a fixed sensor. The bucket is found through the rollup's
// primary key. The newest few minutes (not rolled up yet, the worker runs every minute) are therefore not in a raw
// export either. (`@>` stands for the rollup's `?`, which gorm takes for a
// placeholder; for an array of strings both ask whether the frame is one of its elements.)
const rawReportQuery = `SELECT * FROM (
  SELECT s.received_at AS at,
   CASE WHEN k.z THEN core.twin_num(s.reading->'metrics'->'temperature') WHEN k.env THEN core.twin_num(s.reading->'temperature') END AS temperature,
   CASE WHEN k.z THEN core.twin_num(s.reading->'metrics'->'humidity') WHEN k.env THEN core.twin_num(s.reading->'humidity') END AS humidity,
   CASE WHEN k.z THEN core.twin_num(s.reading->'metrics'->'battery')
        WHEN core.twin_num(s.reading->'battery') > 0 THEN core.twin_num(s.reading->'battery') END AS battery,
   s.event_key
  FROM core.sensor_samples s
  CROSS JOIN LATERAL (SELECT
    coalesce(jsonb_typeof(s.reading->'frames') = 'array' AND s.reading->'frames' @> '["z2m-state@1"]', false) AS z,
    coalesce(jsonb_typeof(s.reading->'frames') = 'array' AND s.reading->'frames' @> '["minew-ffe1-a101@1"]', false)
     OR coalesce(s.reading->>'kind', '') = 'environment'
     OR (coalesce(s.reading->>'kind', '') = '' AND coalesce(CASE WHEN jsonb_typeof(s.reading->'frames') = 'array' THEN jsonb_array_length(s.reading->'frames') END, 0) = 0) AS env
  ) k
  WHERE s.tenant_id = core.tenant_id() AND s.gateway_id = ?::uuid AND s.external_id = ANY(?::text[]) AND s.received_at >= ? AND s.received_at < ?
   AND EXISTS (SELECT 1 FROM core.sample_rollup r WHERE r.tenant_id = s.tenant_id AND r.gateway_id = s.gateway_id
    AND r.external_id = lower(s.external_id) AND r.bucket = date_bin(interval '5 minutes', s.received_at, timestamptz '2000-01-01 00:00:00+00'))
 ) x WHERE temperature IS NOT NULL OR humidity IS NOT NULL OR battery IS NOT NULL
 ORDER BY at, event_key`

// reportBucketExpr is how rollup buckets are grouped per resolution: as they are, by hour, or by Bangkok calendar day.
var reportBucketExpr = map[string]string{
	"5m": `bucket`,
	"1h": `date_bin(interval '1 hour', bucket, timestamptz '2000-01-01 00:00:00+00')`,
	"1d": `(date_trunc('day', bucket AT TIME ZONE 'Asia/Bangkok') AT TIME ZONE 'Asia/Bangkok')`,
}

// StreamReport reads the rows of every sensor in order and hands them to emit with the sensor's index, one READ ONLY
// transaction per sensor so no snapshot is held for the whole export. raw reads core.sensor_samples; 5m, 1h and 1d
// read core.sample_rollup (the averages are means of the 5-minute averages). The sensors were resolved before.
func (r *Repository) StreamReport(ctx context.Context, p domain.Principal, sensors []domain.ReportSensor, from, to time.Time, resolution string, emit func(int, domain.ReportRow) error) error {
	expr, aggregated := reportBucketExpr[resolution]
	if !aggregated && resolution != "raw" {
		return domain.Because(domain.ErrInvalid, "invalid_resolution")
	}
	for i, s := range sensors {
		e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
			if e := tx.Exec(`SET TRANSACTION READ ONLY`).Error; e != nil {
				return e
			}
			var rows *sql.Rows
			var e error
			if aggregated {
				rows, e = tx.Raw(`SELECT `+expr+` AS at, sum(n)::integer, avg(t_avg), min(t_min)::float8, max(t_max)::float8, avg(h_avg), min(h_min)::float8, max(h_max)::float8
          FROM core.sample_rollup WHERE tenant_id = core.tenant_id() AND gateway_id = ?::uuid AND external_id = ? AND bucket >= ? AND bucket < ?
          GROUP BY 1 ORDER BY 1`, s.GatewayID, s.ExternalID, from.Truncate(reports.Step), to).Rows()
			} else {
				// The samples keep the external id as the gateway sent it; the streams say which spellings exist.
				var ids []string
				if e := tx.Raw(`SELECT external_id FROM core.sensor_streams WHERE gateway_id = ?::uuid AND lower(external_id) = ?`, s.GatewayID, s.ExternalID).Scan(&ids).Error; e != nil {
					return e
				}
				if len(ids) == 0 {
					return nil
				}
				rows, e = tx.Raw(rawReportQuery, s.GatewayID, pgTextArray(ids), from, to).Rows()
			}
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var row domain.ReportRow
				if aggregated {
					e = rows.Scan(&row.At, &row.N, &row.TAvg, &row.TMin, &row.TMax, &row.HAvg, &row.HMin, &row.HMax)
					for _, v := range []*float64{row.TAvg, row.TMin, row.TMax, row.HAvg, row.HMin, row.HMax} {
						if v != nil {
							*v = math.Round(*v*100) / 100 // real columns: 24.3, not 24.299999237
						}
					}
				} else {
					var key string
					e = rows.Scan(&row.At, &row.Temperature, &row.Humidity, &row.Battery, &key)
				}
				if e != nil {
					return e
				}
				if e := emit(i, row); e != nil {
					return e
				}
			}
			return rows.Err()
		})
		if e != nil {
			return e
		}
	}
	return nil
}

// ReportBuckets reads the 5-minute buckets of every (resolved) sensor in [from rounded down to 5 minutes, to), in
// one READ ONLY transaction, for reports.Summarize, and the workspace's name for the report's header.
func (r *Repository) ReportBuckets(ctx context.Context, p domain.Principal, sensors []domain.ReportSensor, from, to time.Time) ([][]reports.Bucket, string, error) {
	out := make([][]reports.Bucket, len(sensors))
	var workspace []string
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SET TRANSACTION READ ONLY`).Error; e != nil {
			return e
		}
		if e := tx.Raw(`SELECT name FROM core.tenants WHERE id=core.tenant_id()`).Scan(&workspace).Error; e != nil {
			return e
		}
		for i, s := range sensors {
			var rows []struct {
				Bucket                             time.Time
				N                                  int
				TAvg, TMin, TMax, HAvg, HMin, HMax *float64
			}
			if e := tx.Raw(`SELECT bucket, n, t_avg::float8 AS t_avg, t_min::float8 AS t_min, t_max::float8 AS t_max, h_avg::float8 AS h_avg, h_min::float8 AS h_min, h_max::float8 AS h_max
         FROM core.sample_rollup WHERE tenant_id = core.tenant_id() AND gateway_id = ?::uuid AND external_id = ? AND bucket >= ? AND bucket < ? ORDER BY bucket`,
				s.GatewayID, s.ExternalID, from.Truncate(reports.Step), to).Scan(&rows).Error; e != nil {
				return e
			}
			out[i] = make([]reports.Bucket, len(rows))
			for j, b := range rows {
				out[i][j] = reports.Bucket{At: b.Bucket, N: b.N, TAvg: b.TAvg, TMin: b.TMin, TMax: b.TMax, HAvg: b.HAvg, HMin: b.HMin, HMax: b.HMax}
			}
		}
		return nil
	})
	if len(workspace) != 1 {
		workspace = []string{""}
	}
	return out, workspace[0], e
}

type reportDefinitionRow struct {
	ID                               string
	ProjectID                        *string
	Name, Kind, Sensors              string
	TempMin, TempMax, HumMin, HumMax *float64
	CreatedBy                        *string
	CreatedAt, UpdatedAt             time.Time
}

const reportDefinitionColumns = `id::text AS id, project_id::text AS project_id, name, kind, sensors::text AS sensors,
 temp_min::float8 AS temp_min, temp_max::float8 AS temp_max, hum_min::float8 AS hum_min, hum_max::float8 AS hum_max,
 created_by::text AS created_by, created_at, updated_at`

func (row reportDefinitionRow) definition() (domain.ReportDefinition, error) {
	d := domain.ReportDefinition{ID: row.ID, ProjectID: row.ProjectID, Name: row.Name, Kind: row.Kind, TempMin: row.TempMin, TempMax: row.TempMax, HumMin: row.HumMin, HumMax: row.HumMax, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if e := json.Unmarshal([]byte(row.Sensors), &d.Sensors); e != nil {
		return d, e
	}
	return d, nil
}

// trimReportSensors: a member limited to some projects reads workspace reports, whose sensors may sit on other
// projects' gateways; those entries are left out of what they see (their ids name hardware they cannot see).
func trimReportSensors(tx *gorm.DB, defs []domain.ReportDefinition) error {
	all, e := scopeAll(tx)
	if e != nil || all {
		return e
	}
	var gateways []string
	if e := tx.Raw(`SELECT id::text FROM core.gateways`).Scan(&gateways).Error; e != nil {
		return e
	}
	visible := map[string]bool{}
	for _, g := range gateways {
		visible[g] = true
	}
	for i := range defs {
		kept := []domain.ReportSensor{}
		for _, s := range defs[i].Sensors {
			if visible[s.GatewayID] {
				kept = append(kept, s)
			}
		}
		defs[i].Sensors = kept
	}
	return nil
}

// hiddenReportSensors are the stored sensors of a report whose gateway the caller cannot see (none with full scope).
func hiddenReportSensors(tx *gorm.DB, id string) ([]domain.ReportSensor, error) {
	all, e := scopeAll(tx)
	if e != nil || all {
		return nil, e
	}
	var stored []string
	if e := tx.Raw(`SELECT sensors::text FROM core.report_definitions WHERE id = ?::uuid`, id).Scan(&stored).Error; e != nil {
		return nil, e
	}
	if len(stored) != 1 {
		return nil, domain.ErrNotFound
	}
	defs := []domain.ReportDefinition{{}}
	if e := json.Unmarshal([]byte(stored[0]), &defs[0].Sensors); e != nil {
		return nil, e
	}
	every := append([]domain.ReportSensor{}, defs[0].Sensors...)
	if e := trimReportSensors(tx, defs); e != nil {
		return nil, e
	}
	visible := map[domain.ReportSensor]bool{}
	for _, s := range defs[0].Sensors {
		visible[s] = true
	}
	hidden := []domain.ReportSensor{}
	for _, s := range every {
		if !visible[s] {
			hidden = append(hidden, s)
		}
	}
	return hidden, nil
}

func (r *Repository) ListReportDefinitions(ctx context.Context, p domain.Principal) ([]domain.ReportDefinition, error) {
	out := []domain.ReportDefinition{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []reportDefinitionRow
		if e := tx.Raw(`SELECT ` + reportDefinitionColumns + ` FROM core.report_definitions ORDER BY lower(name), created_at`).Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			d, e := row.definition()
			if e != nil {
				return e
			}
			out = append(out, d)
		}
		return trimReportSensors(tx, out)
	})
	return out, e
}

func (r *Repository) GetReportDefinition(ctx context.Context, p domain.Principal, id string) (domain.ReportDefinition, error) {
	var out domain.ReportDefinition
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []reportDefinitionRow
		if e := tx.Raw(`SELECT `+reportDefinitionColumns+` FROM core.report_definitions WHERE id = ?::uuid`, id).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrNotFound
		}
		d, e := rows[0].definition()
		if e != nil {
			return e
		}
		defs := []domain.ReportDefinition{d}
		if e := trimReportSensors(tx, defs); e != nil {
			return e
		}
		out = defs[0]
		return nil
	})
	return out, e
}

// SaveReportDefinition creates (create) or replaces a saved report. Its project must be one the writer may use
// (a workspace report needs full scope), and every sensor must resolve as for an export: visible and fixed.
func (r *Repository) SaveReportDefinition(ctx context.Context, p domain.Principal, d domain.ReportDefinition, create bool) (domain.ReportDefinition, error) {
	var out domain.ReportDefinition
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if !create {
			if _, e := writableRow(tx, "report_definitions", d.ID); e != nil {
				return e
			}
		}
		if e := checkAlertProject(tx, d.ProjectID); e != nil {
			return e
		}
		infos, e := resolveReportSensors(tx, d.Sensors)
		if e != nil {
			return e
		}
		// A project report covers that project's sensors only: one on another project's (or no project's) gateway
		// would be dropped for every member limited to the project, so the report would read differently per reader.
		if d.ProjectID != nil {
			for _, info := range infos {
				if info.ProjectID == nil || !strings.EqualFold(*info.ProjectID, *d.ProjectID) {
					return domain.Because(domain.ErrInvalid, "sensor_outside_project")
				}
			}
		}
		stored := d.Sensors
		if !create {
			// A member limited to projects was shown the report without the sensors they cannot see; saving keeps
			// those instead of dropping them.
			hidden, e := hiddenReportSensors(tx, d.ID)
			if e != nil {
				return e
			}
			stored = append(append([]domain.ReportSensor{}, d.Sensors...), hidden...)
			if len(stored) > domain.ReportMaxSensors {
				return domain.Because(domain.ErrInvalid, "too_many_sensors")
			}
		}
		sensors, e := json.Marshal(stored)
		if e != nil {
			return e
		}
		var rows []reportDefinitionRow
		if create {
			// Check-then-insert under the workspace's lock, so two creates cannot both pass the cap.
			if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('aether.report_definitions:' || ?, 0))`, p.TenantID).Error; e != nil {
				return e
			}
			var n int64
			if e := tx.Raw(`SELECT core.report_definition_count()`).Scan(&n).Error; e != nil {
				return e
			}
			if n >= domain.ReportMaxDefinitions {
				return domain.Because(domain.ErrConflict, "too_many_reports")
			}
			if e := tx.Raw(`INSERT INTO core.report_definitions(tenant_id,id,project_id,name,kind,sensors,temp_min,temp_max,hum_min,hum_max,created_by)
         VALUES(?,?,?::uuid,?,'environment',?::jsonb,?,?,?,?,?) RETURNING `+reportDefinitionColumns,
				p.TenantID, d.ID, d.ProjectID, d.Name, string(sensors), d.TempMin, d.TempMax, d.HumMin, d.HumMax, p.UserID).Scan(&rows).Error; e != nil {
				return e
			}
		} else if e := tx.Raw(`UPDATE core.report_definitions SET project_id=?::uuid, name=?, sensors=?::jsonb, temp_min=?, temp_max=?, hum_min=?, hum_max=?, updated_at=now()
        WHERE id = ?::uuid RETURNING `+reportDefinitionColumns,
			d.ProjectID, d.Name, string(sensors), d.TempMin, d.TempMax, d.HumMin, d.HumMax, d.ID).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrNotFound
		}
		if out, e = rows[0].definition(); e != nil {
			return e
		}
		saved := []domain.ReportDefinition{out}
		if e := trimReportSensors(tx, saved); e != nil {
			return e
		}
		out = saved[0]
		action := "report.updated"
		if create {
			action = "report.created"
		}
		return audit(tx, p, action, d.ID)
	})
	return out, classify(e)
}

func (r *Repository) DeleteReportDefinition(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if _, e := writableRow(tx, "report_definitions", id); e != nil {
			return e
		}
		res := tx.Exec(`DELETE FROM core.report_definitions WHERE id = ?::uuid`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "report.deleted", id)
	})
}
