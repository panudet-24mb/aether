package postgres

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/reports"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Scheduled and manual sending of saved reports (migration 00050, docs/platform/reports.md "Scheduled delivery").
// The scheduler and the delivery worker run as the system (full scope) once per workspace, like the alerts worker;
// several API replicas may tick at once: due reports and queued deliveries are claimed FOR UPDATE SKIP LOCKED, and a
// scheduled period has at most one run (report_runs_schedule_once).

// ReportDeliveryHide is how long a claimed delivery stays hidden from other replicas while it is being sent.
const ReportDeliveryHide = 5 * time.Minute

const reportRunColumns = `r.id::text AS id, r.report_id::text AS report_id, r.project_id::text AS project_id, r.period_from, r.period_to,
 r.trigger, r.status, r.created_by::text AS created_by, r.created_at, r.finished_at`

// queueReportRun creates a run and one queued delivery per channel that still exists; a run with no delivery left is
// done at once. It reports false when the scheduled period already has its run.
func queueReportRun(tx *gorm.DB, tenant string, run domain.ReportRun, channels []string) (bool, error) {
	insert := `INSERT INTO core.report_runs(tenant_id,id,report_id,project_id,period_from,period_to,trigger,created_by) VALUES(?,?,?,?::uuid,?,?,?,?::uuid)`
	if run.Trigger == "schedule" {
		insert += ` ON CONFLICT (tenant_id, report_id, period_from, period_to) WHERE trigger = 'schedule' DO NOTHING`
	}
	res := tx.Exec(insert, tenant, run.ID, run.ReportID, run.ProjectID, run.PeriodFrom, run.PeriodTo, run.Trigger, run.CreatedBy)
	if res.Error != nil || res.RowsAffected == 0 {
		return false, res.Error
	}
	if len(channels) > 0 {
		if e := tx.Exec(`INSERT INTO core.report_deliveries(tenant_id,id,run_id,project_id,channel_id)
      SELECT ?, gen_random_uuid(), ?, ?::uuid, c.id FROM core.notification_channels c WHERE c.id = ANY(?::uuid[])`,
			tenant, run.ID, run.ProjectID, pgArray(distinct(channels))).Error; e != nil {
			return false, e
		}
	}
	return true, settleReportRuns(tx, run.ID)
}

// settleReportRuns marks done the run (or, with "", every run) that has no queued delivery left.
func settleReportRuns(tx *gorm.DB, run string) error {
	q := `UPDATE core.report_runs r SET status='done', finished_at=now() WHERE r.status='queued'
   AND NOT EXISTS (SELECT 1 FROM core.report_deliveries d WHERE d.tenant_id=r.tenant_id AND d.run_id=r.id AND d.status='queued')`
	if run == "" {
		return tx.Exec(q).Error
	}
	return tx.Exec(q+` AND r.id = ?::uuid`, run).Error
}

// ScheduleDueReports queues a run for every saved report of the workspace whose next_run_at has come, and moves
// next_run_at to the next occurrence. After downtime only the latest due period is sent (reports.Schedule.Latest);
// the periods missed in between are not caught up.
func (r *Repository) ScheduleDueReports(ctx context.Context, tenant string, now time.Time) (int, error) {
	queued := 0
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var due []struct {
			ID, Schedule    string
			ProjectID       *string
			ScheduleWeekday *int
			ScheduleTime    *string
			Channels        string
		}
		if e := tx.Raw(`SELECT id::text AS id, project_id::text AS project_id, schedule, schedule_weekday, schedule_time, channels::text AS channels
      FROM core.report_definitions WHERE schedule <> 'none' AND next_run_at <= ? ORDER BY next_run_at LIMIT 20 FOR UPDATE SKIP LOCKED`, now).Scan(&due).Error; e != nil {
			return e
		}
		loc := reports.Bangkok()
		for _, d := range due {
			// Each report in its own savepoint: one that cannot be queued is logged and tried again in ten minutes,
			// out of the head of the queue, and neither rolls back the others nor holds them up.
			if e := tx.SavePoint("schedule_report").Error; e != nil {
				return e
			}
			next, created, e := func() (time.Time, bool, error) {
				s := reports.Schedule{Kind: d.Schedule}
				if d.ScheduleWeekday != nil {
					s.Weekday = *d.ScheduleWeekday
				}
				if d.ScheduleTime != nil {
					s.Time = *d.ScheduleTime
				}
				occurrence, next := s.Latest(now, loc), s.Next(now, loc)
				if occurrence.IsZero() || next.IsZero() {
					return time.Time{}, false, errors.New("invalid schedule")
				}
				var channels []string
				if e := json.Unmarshal([]byte(d.Channels), &channels); e != nil {
					return time.Time{}, false, e
				}
				from, to := s.Period(occurrence, loc)
				created, e := queueReportRun(tx, tenant, domain.ReportRun{ID: uuid.NewString(), ReportID: d.ID, ProjectID: d.ProjectID, PeriodFrom: from, PeriodTo: to, Trigger: "schedule"}, channels)
				return next, created, e
			}()
			if e != nil {
				slog.Warn("report scheduler: report not queued, retrying in ten minutes", "tenant", tenant, "report", d.ID, "error", e.Error())
				if rb := tx.RollbackTo("schedule_report").Error; rb != nil {
					return rb
				}
				next = now.Add(10 * time.Minute)
			} else if created {
				queued++
			}
			if e := tx.Exec(`UPDATE core.report_definitions SET next_run_at = ? WHERE id = ?::uuid`, next, d.ID).Error; e != nil {
				return e
			}
			if e := tx.Exec(`RELEASE SAVEPOINT schedule_report`).Error; e != nil {
				return e
			}
		}
		return nil
	})
	return queued, e
}

// SendReportNow queues a run of a saved report for [from, to) to its channels, for the worker to deliver. A writer
// of the report only; at most domain.ReportManualPerHour per workspace and hour (429 too_many_sends).
func (r *Repository) SendReportNow(ctx context.Context, p domain.Principal, id string, from, to time.Time) (domain.ReportRun, error) {
	run := domain.ReportRun{ID: uuid.NewString(), ReportID: id, PeriodFrom: from, PeriodTo: to, Trigger: "manual", Status: "queued", CreatedBy: &p.UserID, Deliveries: []domain.ReportDelivery{}}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		project, e := writableRow(tx, "report_definitions", id)
		if e != nil {
			return e
		}
		run.ProjectID = project
		var stored []string
		if e := tx.Raw(`SELECT channels::text FROM core.report_definitions WHERE id = ?::uuid`, id).Scan(&stored).Error; e != nil {
			return e
		}
		var channels []string
		if len(stored) == 1 {
			if e := json.Unmarshal([]byte(stored[0]), &channels); e != nil {
				return e
			}
		}
		if len(channels) == 0 {
			return domain.Because(domain.ErrInvalid, "no_channels")
		}
		// Count-then-insert under the workspace's lock, so two sends cannot both pass the budget.
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('aether.report_sends:' || ?, 0))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT core.manual_report_runs_since(now() - interval '1 hour')`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= domain.ReportManualPerHour {
			return domain.Because(domain.ErrRateLimited, "too_many_sends")
		}
		if _, e := queueReportRun(tx, p.TenantID, run, channels); e != nil {
			return e
		}
		// Answer with the run as stored: its time, status and queued deliveries.
		saved, e := loadReportRuns(tx, `r.id = ?::uuid`, run.ID)
		if e != nil {
			return e
		}
		if len(saved) == 1 {
			run = saved[0]
		}
		return audit(tx, p, "report.sent", id)
	})
	return run, e
}

type reportRunRow struct {
	ID, ReportID, Trigger, Status string
	ProjectID, CreatedBy          *string
	PeriodFrom, PeriodTo          time.Time
	CreatedAt                     time.Time
	FinishedAt                    *time.Time
}

func (row reportRunRow) run() domain.ReportRun {
	return domain.ReportRun{ID: row.ID, ReportID: row.ReportID, ProjectID: row.ProjectID, PeriodFrom: row.PeriodFrom, PeriodTo: row.PeriodTo, Trigger: row.Trigger, Status: row.Status,
		CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt, Deliveries: []domain.ReportDelivery{}}
}

// loadReportRuns reads runs (a WHERE on core.report_runs r) with their deliveries, newest first.
func loadReportRuns(tx *gorm.DB, where string, args ...any) ([]domain.ReportRun, error) {
	out := []domain.ReportRun{}
	var rows []reportRunRow
	if e := tx.Raw(`SELECT `+reportRunColumns+` FROM core.report_runs r WHERE `+where+` ORDER BY r.created_at DESC, r.id LIMIT 20`, args...).Scan(&rows).Error; e != nil {
		return nil, e
	}
	ids := make([]string, len(rows))
	index := map[string]int{}
	for i, row := range rows {
		out = append(out, row.run())
		ids[i], index[row.ID] = row.ID, i
	}
	if len(ids) == 0 {
		return out, nil
	}
	var deliveries []struct {
		RunID, ID, ChannelName, ChannelKind, Status string
		ChannelID, LastError                        *string
		Attempts                                    int
		NextAttemptAt                               time.Time
		SentAt                                      *time.Time
	}
	if e := tx.Raw(`SELECT d.run_id::text AS run_id, d.id::text AS id, d.channel_id::text AS channel_id, coalesce(c.name, '') AS channel_name, coalesce(c.kind, '') AS channel_kind,
     d.status, d.attempts, d.last_error, d.next_attempt_at, d.sent_at
    FROM core.report_deliveries d LEFT JOIN core.notification_channels c ON c.tenant_id = d.tenant_id AND c.id = d.channel_id
    WHERE d.run_id = ANY(?::uuid[]) ORDER BY d.created_at, d.id`, pgArray(ids)).Scan(&deliveries).Error; e != nil {
		return nil, e
	}
	for _, d := range deliveries {
		out[index[d.RunID]].Deliveries = append(out[index[d.RunID]].Deliveries, domain.ReportDelivery{ID: d.ID, ChannelID: d.ChannelID, ChannelName: d.ChannelName, ChannelKind: d.ChannelKind,
			Status: d.Status, Attempts: d.Attempts, LastError: d.LastError, NextAttemptAt: d.NextAttemptAt, SentAt: d.SentAt})
	}
	return out, nil
}

// ListReportRuns is the last 20 runs of a report the caller can read, each with its deliveries.
func (r *Repository) ListReportRuns(ctx context.Context, p domain.Principal, id string) ([]domain.ReportRun, error) {
	var out []domain.ReportRun
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.report_definitions WHERE id = ?::uuid`, id).Scan(&n).Error; e != nil {
			return e
		}
		if n != 1 {
			return domain.ErrNotFound
		}
		var e error
		out, e = loadReportRuns(tx, `r.report_id = ?::uuid`, id)
		return e
	})
	return out, e
}

// ClaimReportDeliveries reserves the due deliveries of one run (at most limit), like ClaimNotifications: the claim
// hides them for ReportDeliveryHide so another replica cannot send them too, and FinishReportDelivery sets the real
// retry time. One run per claim keeps a claim's work (one rendering, one send per channel) inside the hide window;
// see reportjobs.Worker. A demo workspace's deliveries are skipped instead (its data is fictional; the report still shows on screen).
func (r *Repository) ClaimReportDeliveries(ctx context.Context, tenant string, limit int, now time.Time) ([]domain.ReportDeliveryJob, error) {
	out := []domain.ReportDeliveryJob{}
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var demo bool
		if e := tx.Raw(`SELECT coalesce((SELECT demo FROM core.tenants WHERE id=?),false)`, tenant).Scan(&demo).Error; e != nil {
			return e
		}
		if demo {
			if e := tx.Exec(`UPDATE core.report_deliveries SET status='skipped', last_error='demo workspace: reports are not delivered' WHERE status='queued'`).Error; e != nil {
				return e
			}
			return settleReportRuns(tx, "")
		}
		var claimed []struct {
			ID, RunID string
			ChannelID *string
			Attempts  int
		}
		if e := tx.Raw(`WITH head AS (
       SELECT run_id FROM core.report_deliveries WHERE status='queued' AND next_attempt_at<=? ORDER BY next_attempt_at LIMIT 1 FOR UPDATE SKIP LOCKED
      ), pick AS (
       SELECT d.id FROM core.report_deliveries d JOIN head h ON h.run_id = d.run_id
       WHERE d.status='queued' AND d.next_attempt_at<=? ORDER BY d.next_attempt_at LIMIT ? FOR UPDATE OF d SKIP LOCKED
      )
      UPDATE core.report_deliveries SET attempts=attempts+1, next_attempt_at=? WHERE id IN (SELECT id FROM pick)
      RETURNING id::text AS id, run_id::text AS run_id, channel_id::text AS channel_id, attempts`, now, now, limit, now.Add(ReportDeliveryHide)).Scan(&claimed).Error; e != nil {
			return e
		}
		for _, c := range claimed {
			job := domain.ReportDeliveryJob{Delivery: domain.ReportDelivery{ID: c.ID, ChannelID: c.ChannelID, Status: "queued", Attempts: c.Attempts}}
			var runs []reportRunRow
			if e := tx.Raw(`SELECT `+reportRunColumns+` FROM core.report_runs r WHERE r.id = ?::uuid`, c.RunID).Scan(&runs).Error; e != nil {
				return e
			}
			if len(runs) != 1 {
				continue // pruned with its run in the meantime
			}
			job.Run = runs[0].run()
			if c.ChannelID != nil {
				var ch []struct {
					ID, Name, Kind string
					Enabled        bool
					Config         json.RawMessage
					SecretEnc      *string
					ProjectID      *string
				}
				if e := tx.Raw(`SELECT id::text AS id, name, kind, enabled, config, secret_enc, project_id::text AS project_id FROM core.notification_channels WHERE id = ?::uuid`, *c.ChannelID).Scan(&ch).Error; e != nil {
					return e
				}
				if len(ch) == 1 && ch[0].Enabled {
					job.Channel = domain.NotificationChannel{ID: ch[0].ID, Name: ch[0].Name, Kind: ch[0].Kind, Enabled: true, Config: map[string]string{}, ProjectID: ch[0].ProjectID}
					_ = json.Unmarshal(ch[0].Config, &job.Channel.Config)
					if ch[0].SecretEnc != nil {
						job.SecretEnc, job.Channel.HasSecret = *ch[0].SecretEnc, true
					}
				}
			}
			if job.Channel.ID == "" {
				job.FailReason = "channel removed or disabled"
				out = append(out, job)
				continue
			}
			// A project channel only carries its project's reports; the report may have moved since it was queued.
			if job.Channel.ProjectID != nil {
				var current []struct{ ProjectID *string }
				if e := tx.Raw(`SELECT project_id::text AS project_id FROM core.report_definitions WHERE id = ?::uuid`, job.Run.ReportID).Scan(&current).Error; e != nil {
					return e
				}
				if len(current) != 1 || current[0].ProjectID == nil || *current[0].ProjectID != *job.Channel.ProjectID {
					job.FailReason = "channel belongs to another project than the report"
				}
			}
			if job.FailReason == "" && job.Channel.Kind == "email" && job.Channel.Config["audience"] == "members" {
				to, problem, e := directoryRecipients(tx, `SELECT email FROM core.report_recipients(?::uuid, ?::text[])`, job.Run.ReportID, job.Channel.Config, "no member can read this report")
				if e != nil {
					return e
				}
				job.FailReason, job.RetryReason = problem.fail, problem.retry
				job.Channel.Config["to"] = strings.Join(to, ",")
			}
			out = append(out, job)
		}
		return nil
	})
	return out, e
}

// FinishReportDelivery records an outcome: sent, failed, skipped (terminal), or retry (the backoff of notifications,
// failed after 5 attempts). The run is done once no delivery of it is queued.
func (r *Repository) FinishReportDelivery(ctx context.Context, tenant, id, outcome, message string) error {
	if len(message) > 200 {
		message = message[:200]
	}
	return r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var e error
		switch outcome {
		case "sent":
			e = tx.Exec(`UPDATE core.report_deliveries SET status='sent', sent_at=now(), last_error=NULL WHERE id=?::uuid AND status='queued'`, id).Error
		case "failed", "skipped":
			e = tx.Exec(`UPDATE core.report_deliveries SET status=?, last_error=? WHERE id=?::uuid AND status='queued'`, outcome, message, id).Error
		default:
			var attempts int
			if e := tx.Raw(`SELECT attempts FROM core.report_deliveries WHERE id=?::uuid`, id).Scan(&attempts).Error; e != nil {
				return e
			}
			e = tx.Exec(`UPDATE core.report_deliveries SET last_error=?, next_attempt_at=now()+make_interval(secs => ?),
       status=CASE WHEN attempts>=5 THEN 'failed' ELSE status END WHERE id=?::uuid AND status='queued'`, message, backoff(attempts).Seconds(), id).Error
		}
		if e != nil {
			return e
		}
		var run []string
		if e := tx.Raw(`SELECT run_id::text FROM core.report_deliveries WHERE id=?::uuid`, id).Scan(&run).Error; e != nil || len(run) != 1 {
			return e
		}
		return settleReportRuns(tx, run[0])
	})
}

// ReportRunContent loads what a run's messages are made of, as the system: the report as saved now, its sensors that
// still resolve as fixed sensors and, for a project report, still sit on a gateway of that project (the others are
// omitted with their reason: a gateway moved to another project must not send its readings to this project's
// channels and members), and the workspace's name.
func (r *Repository) ReportRunContent(ctx context.Context, tenant, runID string) (domain.ReportRunContent, error) {
	var out domain.ReportRunContent
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var runs []reportRunRow
		if e := tx.Raw(`SELECT `+reportRunColumns+` FROM core.report_runs r WHERE r.id = ?::uuid`, runID).Scan(&runs).Error; e != nil {
			return e
		}
		if len(runs) != 1 {
			return domain.ErrNotFound
		}
		out.Run = runs[0].run()
		var defs []reportDefinitionRow
		if e := tx.Raw(`SELECT `+reportDefinitionColumns+` FROM core.report_definitions WHERE id = ?::uuid`, out.Run.ReportID).Scan(&defs).Error; e != nil {
			return e
		}
		if len(defs) != 1 {
			return domain.ErrNotFound
		}
		var e error
		if out.Definition, e = defs[0].definition(); e != nil {
			return e
		}
		out.Sensors, out.Omitted = []domain.ReportSensorInfo{}, []domain.ReportOmitted{}
		for _, s := range out.Definition.Sensors {
			got, e := resolveReportSensors(tx, []domain.ReportSensor{s})
			var reason domain.ReasonError
			switch {
			case e == nil && !inReportProject(got[0], out.Definition.ProjectID):
				out.Omitted = append(out.Omitted, domain.ReportOmitted{ReportSensor: s, Reason: "sensor_outside_project"})
			case e == nil:
				out.Sensors = append(out.Sensors, got[0])
			case errors.As(e, &reason):
				out.Omitted = append(out.Omitted, domain.ReportOmitted{ReportSensor: s, Reason: reason.Reason})
			default:
				return e
			}
		}
		var name []string
		if e := tx.Raw(`SELECT name FROM core.tenants WHERE id = core.tenant_id()`).Scan(&name).Error; e != nil {
			return e
		}
		if len(name) == 1 {
			out.Workspace = name[0]
		}
		return nil
	})
	return out, e
}

// inReportProject: a workspace report covers any sensor; a project report only sensors on its project's gateways.
func inReportProject(info domain.ReportSensorInfo, project *string) bool {
	return project == nil || (info.ProjectID != nil && strings.EqualFold(*info.ProjectID, *project))
}
