package postgres

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"encoding/json"
	"gorm.io/gorm"
	"strings"
	"time"
)

func (r *Repository) ListTemplates(ctx context.Context, p domain.Principal) ([]domain.DeviceTemplate, error) {
	out := []domain.DeviceTemplate{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT id,name,version,decoder_id,definition,created_at FROM core.device_templates ORDER BY name,version DESC LIMIT 100`).Scan(&out).Error
	})
	return out, e
}
func (r *Repository) CreateTemplate(ctx context.Context, p domain.Principal, t domain.DeviceTemplate) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,1))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.device_templates`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= 100 {
			return domain.ErrConflict
		}
		def, _ := json.Marshal(t.Definition)
		if e := tx.Exec(`INSERT INTO core.device_templates(tenant_id,id,name,version,decoder_id,definition) VALUES(?,?,?,?,?,?::jsonb)`, p.TenantID, t.ID, t.Name, t.Version, t.DecoderID, string(def)).Error; e != nil {
			return e
		}
		return audit(tx, p, "template.created", t.ID)
	}))
}
func (r *Repository) SetStreamTemplate(ctx context.Context, p domain.Principal, gateway, external, name, template string) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.device_templates WHERE id=?`, template).Scan(&n).Error; e != nil {
			return e
		}
		if n != 1 {
			return domain.ErrNotFound
		}
		result := tx.Exec(`UPDATE core.sensor_streams SET name=?,template_id=? WHERE gateway_id=? AND external_id=?`, name, template, gateway, external)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "stream.template_assigned", gateway)
	}))
}

// Called inside the packet transaction: raw storage, discovery and samples commit together.
func saveSamples(tx *gorm.DB, tenant, gateway string, view minew.View, payload json.RawMessage, at time.Time, opts Options) error {
	key := security.Digest(string(payload))
	for _, sensor := range view.Sensors {
		// Bounded discovery. A registered device always gets its stream; strangers (visitor beacons, other
		// vendors' tags) share a separate budget per gateway so they can never crowd the real fleet out.
		// Existing streams keep receiving samples after the cap.
		if e := tx.Exec(`INSERT INTO core.sensor_streams(tenant_id,gateway_id,external_id,name,last_seen)
    SELECT ?,?,?,?,? WHERE EXISTS(SELECT 1 FROM core.devices d WHERE lower(d.external_id)=? AND d.removed_at IS NULL)
      OR (SELECT count(*) FROM core.sensor_streams s WHERE s.gateway_id=? AND NOT EXISTS(SELECT 1 FROM core.devices d WHERE lower(d.external_id)=s.external_id AND d.removed_at IS NULL))<?
    ON CONFLICT DO NOTHING`, tenant, gateway, sensor.ID, sensor.Name, at, sensor.ID, gateway, opts.DiscoveryLimit).Error; e != nil {
			return e
		}
		// Thinning: slow environmental values need not be stored every uplink. Anything that carries state
		// (tamper, motion, beacon identity, …) is always stored, and last_seen always moves.
		if opts.SampleMinIntervalSec > 0 && (sensor.Latest.Kind == "" || sensor.Latest.Kind == minew.KindEnvironment) {
			var recent int64
			if e := tx.Raw(`SELECT count(*) FROM core.sensor_streams WHERE gateway_id=? AND external_id=? AND last_sample_at>?`, gateway, sensor.ID, at.Add(-time.Duration(opts.SampleMinIntervalSec)*time.Second)).Scan(&recent).Error; e != nil {
				return e
			}
			if recent > 0 {
				if e := tx.Exec(`UPDATE core.sensor_streams SET last_seen=greatest(last_seen,?) WHERE gateway_id=? AND external_id=?`, at, gateway, sensor.ID).Error; e != nil {
					return e
				}
				continue
			}
		}
		data, _ := json.Marshal(sensor.Latest)
		decoder := strings.Join(sensor.Latest.Frames, ",")
		if decoder == "" && len(sensor.Latest.Unknown) > 0 {
			decoder = "undecoded" // a Minew frame Aether has no decoder for (see minew.Project)
		} else if decoder == "" {
			decoder = minew.FrameTH
		}
		result := tx.Exec(`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,template_id,decoder_id,reading)
    SELECT tenant_id,gateway_id,external_id,?,?,template_id,?,?::jsonb FROM core.sensor_streams WHERE gateway_id=? AND external_id=? ON CONFLICT DO NOTHING`, key, at, decoder, string(data), gateway, sensor.ID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			if e := tx.Exec(`UPDATE core.sensor_streams SET last_seen=greatest(last_seen,?),last_sample_at=? WHERE gateway_id=? AND external_id=?`, at, at, gateway, sensor.ID).Error; e != nil {
				return e
			}
			// A later info frame names the model; upgrade auto-generated stream names only, never a user-assigned one.
			if sensor.Model != "" {
				if e := tx.Exec(`UPDATE core.sensor_streams SET name=? WHERE gateway_id=? AND external_id=? AND name IN ?`, sensor.Name, gateway, sensor.ID, minew.GenericNames()).Error; e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func (r *Repository) StreamHistory(ctx context.Context, p domain.Principal, gateway string, since time.Time, limit int) ([]minew.Sensor, error) {
	out := []minew.Sensor{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []struct {
			ExternalID, Name string
			TemplateID       *string
			Definition       json.RawMessage
			Latest           json.RawMessage
			History          json.RawMessage
		}
		e := tx.Raw(`SELECT s.external_id,s.name,s.template_id,t.definition,
    (SELECT reading FROM core.sensor_samples WHERE gateway_id=s.gateway_id AND external_id=s.external_id ORDER BY received_at DESC,event_key DESC LIMIT 1) AS latest,
    (SELECT coalesce(jsonb_agg(q.reading ORDER BY q.received_at),'[]'::jsonb) FROM
       (SELECT reading,received_at FROM core.sensor_samples WHERE gateway_id=s.gateway_id AND external_id=s.external_id AND received_at>=? ORDER BY received_at DESC,event_key DESC LIMIT ?) q) AS history
    FROM core.sensor_streams s LEFT JOIN core.device_templates t ON t.tenant_id=s.tenant_id AND t.id=s.template_id WHERE s.gateway_id=? ORDER BY s.external_id LIMIT 100`, since, limit, gateway).Scan(&rows).Error
		if e != nil {
			return e
		}
		for _, row := range rows {
			s := minew.Sensor{ID: row.ExternalID, Name: row.Name, History: []minew.Reading{}, TemplateID: row.TemplateID}
			if e := json.Unmarshal(row.Latest, &s.Latest); e != nil {
				return e
			}
			if e := json.Unmarshal(row.History, &s.History); e != nil {
				return e
			}
			if len(row.Definition) > 0 && string(row.Definition) != "null" {
				if e := json.Unmarshal(row.Definition, &s.Thresholds); e != nil {
					return e
				}
			}
			if s.Latest.Kind == "" { // samples stored before multi-frame decoding are temperature/humidity readings
				s.Latest.Kind = minew.KindEnvironment
			}
			s.Kind = s.Latest.Kind
			s.Model = s.Latest.Model
			for i := len(s.History) - 1; i >= 0 && s.Model == ""; i-- { // info frames are sparse; keep the last known model
				s.Model = s.History[i].Model
			}
			if s.Latest.Source == "simulated" && !strings.HasPrefix(s.Name, "SIM") {
				s.Name = "SIM · " + s.Name
			}
			out = append(out, s)
		}
		return nil
	})
	return out, e
}
