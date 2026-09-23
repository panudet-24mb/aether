package postgres

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/studio"
	"context"
	"encoding/hex"
	"encoding/json"
	"gorm.io/gorm"
	"strings"
	"time"
)

// Called under the existing gateway lock; no user JavaScript runs on the ingestion path.
//
// It also returns the distinct raw advertisements of this packet grouped by advertiser MAC. Learned
// signal matching needs exactly that list, and this function already walks and validates every row,
// so handing it back costs nothing and keeps the payload from being parsed a second time.
func saveBLEHistory(tx *gorm.DB, tenant, gateway string, payload json.RawMessage, at time.Time) (rawByIdentity, error) {
	byIdentity := rawByIdentity{}
	// Row by row: one malformed row (an MG4 firmware's string rssi, a numeric mac) skips only itself.
	rows, ok := minew.ParseRows(payload)
	if !ok {
		return byIdentity, nil
	}
	seen := map[string]bool{}
	key := security.Digest(string(payload))
	for _, row := range rows {
		mac := strings.ToLower(row.MAC)
		b, e := hex.DecodeString(mac)
		if e != nil || len(b) != 6 || len(row.Raw) > 3300 {
			continue
		}
		raw, e := hex.DecodeString(row.Raw)
		if e != nil || len(raw) == 0 {
			continue
		}
		event := key + ":" + security.Digest(strings.ToLower(row.Raw))
		identity := mac + event
		if seen[identity] || len(seen) >= 100 {
			continue
		}
		seen[identity] = true
		byIdentity.add(mac, row.Raw)
		source := "device"
		if row.Source == "simulated" {
			source = "simulated"
		}
		if e := tx.Exec(`INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source) VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, tenant, gateway, mac, event, at, strings.ToLower(row.Raw), source).Error; e != nil {
			return byIdentity, e
		}
	}
	// Pruning happens in PruneHistory (worker), never inside the ingest transaction.
	return byIdentity, nil
}

// PruneHistory applies the retention policy outside the ingest path: raw BLE advertisements by hours, decoded
// samples by days. Deletes are batched so one run never holds a long lock or bloats a single transaction.
func (r *Repository) PruneHistory(ctx context.Context, tenant string) error {
	hours, days := r.opts.BLEHistoryHours, r.opts.SampleRetentionDays
	if hours <= 0 {
		hours = 24
	}
	if days <= 0 {
		days = 90
	}
	for _, q := range []struct {
		sql    string
		cutoff time.Time
	}{
		{`DELETE FROM core.ble_history WHERE ctid IN (SELECT ctid FROM core.ble_history WHERE received_at<? LIMIT 20000)`, time.Now().Add(-time.Duration(hours) * time.Hour)},
		{`DELETE FROM core.sensor_samples WHERE ctid IN (SELECT ctid FROM core.sensor_samples WHERE received_at<? LIMIT 20000)`, time.Now().Add(-time.Duration(days) * 24 * time.Hour)},
	} {
		for batch := 0; batch < 25; batch++ {
			var affected int64
			e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
				res := tx.Exec(q.sql, q.cutoff)
				affected = res.RowsAffected
				return res.Error
			})
			if e != nil {
				return e
			}
			if affected < 20000 {
				break
			}
		}
	}
	return nil
}
func (r *Repository) BLEHistory(ctx context.Context, p domain.Principal, gateway, external string, since time.Time) ([]studio.Observation, error) {
	out := []studio.Observation{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT raw,source,received_at FROM core.ble_history WHERE gateway_id=? AND external_id=? AND received_at>=? ORDER BY received_at DESC,event_key DESC LIMIT 200`, gateway, strings.ToLower(external), since).Scan(&out).Error
	})
	return out, e
}
