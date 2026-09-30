package postgres

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// RedeliveryWindow is how far back a sample or BLE row with the same event key counts as the same delivery. The
// primary keys include received_at (migration 00037), so a QoS 1 redelivery, which arrives with a new receive
// time, is recognised by this look-back rather than by the key.
const RedeliveryWindow = 24 * time.Hour

// PartitionLookAheadAlarm is the least coverage ahead that is still healthy: maintenance keeps 28 days of sample
// ranges and 14 days of BLE ranges ahead, so less than a week means it has not run for a week or more.
const PartitionLookAheadAlarm = 7 * 24 * time.Hour

const pruneBatch = 20000

// PartitionStep is one step core.maintain_partitions took.
type PartitionStep struct {
	Action   string
	Relation string
	Moved    int64
}

// PartitionHealth is what core.partition_health reports for one partitioned table.
type PartitionHealth struct {
	Parent       string
	CoveredUntil *time.Time
	DefaultRows  int64
}

// MaxDropsPerRun caps how many partitions one maintenance run may drop. A normal run drops at most one week of
// samples and one day of BLE history; anything more means a long outage or a clock that jumped, and is spread over
// several hourly runs instead of vanishing at once.
const MaxDropsPerRun = 2

// MaintainPartitions keeps core.sensor_samples and core.ble_history (migration 00037) and core.access_log
// (migration 00038; its retention is core.retention_policy's, never passed from here) partitioned: it creates the
// ranges ahead, drops those past the retention (at most
// MaxDropsPerRun; a partition with rows only when the newest stored row confirms it expired, so a clock that jumped
// ahead expires nothing) and deletes expired rows from the partitions that are not dropped by range (legacy and
// DEFAULT). Each step is its own transaction. Global, not per tenant. Pruning runs even when a step failed; the
// errors are joined.
func (r *Repository) MaintainPartitions(ctx context.Context) ([]PartitionStep, int64, error) {
	days, hours := r.retention()
	steps := []PartitionStep{}
	drops, allowDrop := 0, true
	var stepErr error
	for i := 0; i < 100; i++ {
		var batch []PartitionStep
		if e := r.db.WithContext(ctx).Raw(`SELECT action,relation,moved FROM core.maintain_partitions(?,?,?)`, days, hours, allowDrop).Scan(&batch).Error; e != nil {
			stepErr = e
			break
		}
		progress := false
		for _, s := range batch {
			switch s.Action {
			case "dropped":
				drops++
				progress = true
			case "created":
				progress = true
			case "drop_deferred":
				allowDrop = false // retried next run; do not wait on the same busy lock again
				// drop_held blocks only its own table (the function goes on with the other); it is not progress.
			}
		}
		if drops >= MaxDropsPerRun {
			allowDrop = false
		}
		steps = append(steps, batch...)
		if !progress {
			break
		}
	}
	pruned, e := r.PruneHistory(ctx)
	return steps, pruned, errors.Join(stepErr, e)
}

// PruneHistory deletes expired rows from the legacy and DEFAULT partitions in batches, so one call never holds a
// long lock or bloats a transaction. Range partitions expire whole in MaintainPartitions.
func (r *Repository) PruneHistory(ctx context.Context) (int64, error) {
	days, hours := r.retention()
	var total int64
	for batch := 0; batch < 50; batch++ {
		var n int64
		if e := r.db.WithContext(ctx).Raw(`SELECT core.prune_history(?,?,?)`, days, hours, pruneBatch).Scan(&n).Error; e != nil {
			return total, e
		}
		total += n
		if n < pruneBatch {
			break
		}
	}
	return total, nil
}

// PartitionHealth reports, per partitioned table, where the ranges end and how many rows (up to 1000) sit in
// DEFAULT.
func (r *Repository) PartitionHealth(ctx context.Context) ([]PartitionHealth, error) {
	out := []PartitionHealth{}
	e := r.db.WithContext(ctx).Raw(`SELECT parent,covered_until,default_rows FROM core.partition_health()`).Scan(&out).Error
	return out, e
}

func (r *Repository) retention() (days, hours int) {
	days, hours = r.opts.SampleRetentionDays, r.opts.BLEHistoryHours
	if days <= 0 {
		days = 90
	}
	if hours <= 0 {
		hours = 24
	}
	return days, hours
}

// RunPartitionMaintenance runs MaintainPartitions at start-up and then every interval until ctx ends, and logs an
// error when DEFAULT holds rows or the ranges reach less than PartitionLookAheadAlarm ahead.
func (r *Repository) RunPartitionMaintenance(ctx context.Context, interval time.Duration) {
	for {
		r.MaintainAndReport(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// MaintainAndReport is one maintenance run with its log lines: the cutover check, MaintainPartitions and the
// health alerts. RunPartitionMaintenance calls it at start-up and every interval.
func (r *Repository) MaintainAndReport(ctx context.Context, now time.Time) {
	// First, independent of the functions below (absent when 00037 never ran): a cutover constraint left by
	// migration 00036 stops ingest at its cutover time.
	if left, e := r.LeftoverCutovers(ctx); e != nil {
		slog.Error("partition cutover check failed", "error", e.Error())
	} else {
		for _, c := range left {
			slog.Error("PARTITION ALERT migration 00036 cutover constraint still present; ingest stops at its cutover unless migrate runs 00037 (docs/production.md)", "constraint", c)
		}
	}
	steps, pruned, e := r.MaintainPartitions(ctx)
	for _, s := range steps {
		switch s.Action {
		case "dropped":
			slog.Warn("partition maintenance dropped an expired partition", "relation", s.Relation)
		case "drop_held":
			slog.Warn("partition maintenance held a drop: expired by the clock but not by the newest stored row (clock jumped ahead?)", "relation", s.Relation)
		case "drop_deferred", "create_deferred":
			slog.Info("partition maintenance deferred (lock busy); next run retries", "action", s.Action, "relation", s.Relation)
		default:
			slog.Info("partition maintenance", "action", s.Action, "relation", s.Relation, "moved_from_default", s.Moved)
		}
	}
	if pruned > 0 {
		slog.Info("history retention", "deleted", pruned)
	}
	if e != nil {
		slog.Warn("partition maintenance failed; retried next run", "error", e.Error())
	}
	health, e := r.PartitionHealth(ctx)
	if e != nil {
		slog.Error("partition health check failed", "error", e.Error())
		return
	}
	for _, h := range health {
		if h.DefaultRows > 0 {
			slog.Error("PARTITION ALERT rows in the DEFAULT partition", "table", h.Parent, "rows", h.DefaultRows)
		}
		if h.CoveredUntil == nil || h.CoveredUntil.Before(now.Add(PartitionLookAheadAlarm)) {
			slog.Error("PARTITION ALERT fewer than 7 days of partitions ahead", "table", h.Parent, "covered_until", h.CoveredUntil)
		}
	}
}

// LeftoverCutovers lists the cutover CHECK constraints of migration 00036 that still exist. 00037 drops them when
// it attaches the tables; one that survives means 00037 has not run.
func (r *Repository) LeftoverCutovers(ctx context.Context) ([]string, error) {
	out := []string{}
	e := r.db.WithContext(ctx).Raw(`SELECT conrelid::regclass::text||'.'||conname FROM pg_constraint
    WHERE connamespace='core'::regnamespace AND contype='c' AND conname IN ('sensor_samples_cutover','ble_history_cutover') ORDER BY 1`).Scan(&out).Error
	return out, e
}
