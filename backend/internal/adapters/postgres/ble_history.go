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
		// The key holds received_at (partitioned by it, migration 00037), so a redelivered packet, stored again
		// under a new receive time, is recognised by its event key within the redelivery window instead. The
		// caller holds the gateway row, so two deliveries of one packet cannot both pass this check. Both bounds
		// let the planner skip every partition outside the window (the ranges ahead and DEFAULT); a row stored
		// while the server clock ran ahead is outside them and no longer deduplicates, which is accepted.
		if e := tx.Exec(`INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source) SELECT ?,?,?,?,?,?,?
    WHERE NOT EXISTS(SELECT 1 FROM core.ble_history WHERE tenant_id=? AND gateway_id=? AND external_id=? AND event_key=? AND received_at>? AND received_at<=?) ON CONFLICT DO NOTHING`,
			tenant, gateway, mac, event, at, strings.ToLower(row.Raw), source, tenant, gateway, mac, event, at.Add(-RedeliveryWindow), at).Error; e != nil {
			return byIdentity, e
		}
	}
	// Retention happens in MaintainPartitions (partitions.go), never inside the ingest transaction.
	return byIdentity, nil
}

func (r *Repository) BLEHistory(ctx context.Context, p domain.Principal, gateway, external string, since time.Time) ([]studio.Observation, error) {
	out := []studio.Observation{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT raw,source,received_at FROM core.ble_history WHERE gateway_id=? AND external_id=? AND received_at>=? ORDER BY received_at DESC,event_key LIMIT 200`, gateway, strings.ToLower(external), since).Scan(&out).Error
	})
	return out, e
}
