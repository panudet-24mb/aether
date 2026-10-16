package reportjobs

import (
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"aether/backend/internal/reports"
	"aether/backend/internal/security"
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"time"
)

// Store is the slice of the repository the worker needs.
type Store interface {
	ActiveTenants(context.Context) ([]string, error)
	ScheduleDueReports(ctx context.Context, tenant string, now time.Time) (int, error)
	ClaimReportDeliveries(ctx context.Context, tenant string, limit int, now time.Time) ([]domain.ReportDeliveryJob, error)
	FinishReportDelivery(ctx context.Context, tenant, id, outcome, message string) error
	ReportRunContent(ctx context.Context, tenant, runID string) (domain.ReportRunContent, error)
	ReportBuckets(ctx context.Context, p domain.Principal, sensors []domain.ReportSensor, from, to time.Time) ([][]reports.Bucket, string, error)
	StreamReport(ctx context.Context, p domain.Principal, sensors []domain.ReportSensor, from, to time.Time, resolution string, emit func(int, domain.ReportRow) error) error
}

// Worker queues due report runs and delivers them, every Interval, for every active workspace. It runs in each API
// process next to the alerts worker; replicas share the work through the repository's SKIP LOCKED claims.
//
// A claim is one run's deliveries (at most domain.ReportMaxChannels) and stays hidden from other replicas for
// postgres.ReportDeliveryHide (5 min). Its work is bounded inside that: one rendering (renderTimeout, 60 s) and one send
// per channel (sendTimeout, 20 s), 60 + 10 x 20 = 260 s at worst, so a slow pass cannot outlive the hide and be sent
// again by another replica. One goroutine serves every workspace, as the alerts worker does; RunsPerTick caps what one
// workspace takes per pass so the others are not held up for long.
type Worker struct {
	Store         Store
	Sender        *alerts.Sender
	Secrets       []byte
	LegacySecrets []byte
	Origin        string // APP_ORIGIN, for links back to the report
	Interval      time.Duration
	RunsPerTick   int
}

const (
	renderTimeout = 60 * time.Second
	sendTimeout   = 20 * time.Second
)

// SkipNoSMTP is why an email delivery is skipped on a server without SMTP settings.
const SkipNoSMTP = "smtp_not_configured: this server has no SMTP settings"

func (w *Worker) Run(ctx context.Context) {
	if w.Interval <= 0 {
		w.Interval = 30 * time.Second
	}
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		w.Tick(ctx, time.Now().UTC())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick performs one pass; exported so tests can drive it deterministically.
func (w *Worker) Tick(ctx context.Context, now time.Time) {
	tenants, e := w.Store.ActiveTenants(ctx)
	if e != nil {
		slog.Warn("report worker: tenant listing failed")
		return
	}
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return
		}
		if _, e := w.Store.ScheduleDueReports(ctx, tenant, now); e != nil {
			slog.Warn("report worker: scheduling failed", "tenant", tenant, "error", e.Error())
		}
		w.deliver(ctx, tenant, now)
	}
}

func (w *Worker) deliver(ctx context.Context, tenant string, now time.Time) {
	runs := w.RunsPerTick
	if runs <= 0 {
		runs = 5
	}
	for i := 0; i < runs && ctx.Err() == nil; i++ {
		jobs, e := w.Store.ClaimReportDeliveries(ctx, tenant, domain.ReportMaxChannels, now)
		if e != nil {
			slog.Warn("report worker: claim failed", "tenant", tenant, "error", e.Error())
			return
		}
		if len(jobs) == 0 {
			return
		}
		w.deliverRun(ctx, tenant, jobs, now)
	}
}

// deliverRun sends one run's claimed deliveries, rendering the run once.
func (w *Worker) deliverRun(ctx context.Context, tenant string, jobs []domain.ReportDeliveryJob, now time.Time) {
	var d *alerts.Delivery
	rendered := false
	finish := func(id, outcome, msg string) {
		if e := w.Store.FinishReportDelivery(ctx, tenant, id, outcome, msg); e != nil {
			slog.Warn("report worker: finish failed", "tenant", tenant, "delivery", id)
		}
	}
	for _, job := range jobs {
		switch {
		case job.FailReason != "":
			finish(job.Delivery.ID, "failed", job.FailReason)
			continue
		case job.RetryReason != "":
			finish(job.Delivery.ID, "retry", job.RetryReason)
			continue
		case job.Channel.Kind == "email" && !w.Sender.SMTP.Configured():
			finish(job.Delivery.ID, "skipped", SkipNoSMTP)
			continue
		}
		secret := ""
		if job.SecretEnc != "" {
			var e error
			if secret, e = security.OpenAny(job.SecretEnc, w.Secrets, w.LegacySecrets); e != nil {
				finish(job.Delivery.ID, "failed", "channel secret unreadable (signing key changed?)")
				continue
			}
		}
		if !rendered {
			rendered = true
			renderCtx, cancel := context.WithTimeout(ctx, renderTimeout)
			var e error
			if d, e = w.render(renderCtx, tenant, job.Run.ID, now); e != nil {
				slog.Warn("report worker: rendering failed", "tenant", tenant, "run", job.Run.ID, "error", e.Error())
			}
			cancel()
		}
		if d == nil {
			finish(job.Delivery.ID, "retry", "report could not be prepared")
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		err := w.Sender.SendDelivery(sendCtx, job.Channel, secret, *d)
		cancel()
		if err != nil {
			finish(job.Delivery.ID, "retry", err.Error())
			continue
		}
		finish(job.Delivery.ID, "sent", "")
	}
}

// Build turns a run's content into its numbers and CSV: the summary of every sensor over the period (as the reports
// page computes it) and the hourly CSV with the export's writer, its guards and its #END row. It reads as the
// system of the workspace (full scope), so it reads only c.Sensors: the repository resolved them again when it loaded
// the content (fixed sensors, and for a project report on that project's gateways; the rest are in c.Omitted).
func Build(ctx context.Context, store Store, tenant string, c domain.ReportRunContent, origin string, now time.Time) (Report, error) {
	r := Report{Run: c.Run, Name: c.Definition.Name, Workspace: c.Workspace, Sensors: c.Sensors, Omitted: c.Omitted, Origin: origin,
		Temp: reports.Range{Min: c.Definition.TempMin, Max: c.Definition.TempMax}, Hum: reports.Range{Min: c.Definition.HumMin, Max: c.Definition.HumMax}}
	p := domain.Principal{TenantID: tenant}
	sensors := make([]domain.ReportSensor, len(c.Sensors))
	for i, s := range c.Sensors {
		sensors[i] = domain.ReportSensor{GatewayID: s.GatewayID, ExternalID: s.ExternalID}
	}
	buckets, workspace, e := store.ReportBuckets(ctx, p, sensors, c.Run.PeriodFrom, c.Run.PeriodTo)
	if e != nil {
		return r, e
	}
	if r.Workspace == "" {
		r.Workspace = workspace
	}
	loc := reports.Bangkok()
	r.Summaries = make([]reports.Summary, len(sensors))
	for i := range sensors {
		r.Summaries[i] = reports.Summarize(buckets[i], c.Run.PeriodFrom, c.Run.PeriodTo, now, r.Temp, r.Hum, loc)
	}
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if e := reports.WriteCSV(ctx, w, c.Sensors, "1h", func(emit func(int, domain.ReportRow) error) error {
		return store.StreamReport(ctx, p, sensors, c.Run.PeriodFrom, c.Run.PeriodTo, "1h", emit)
	}, func() {}); e != nil {
		return r, e
	}
	if buf.Len() <= MaxAttachment {
		r.CSV = buf.Bytes()
	}
	return r, nil
}

func (w *Worker) render(ctx context.Context, tenant, run string, now time.Time) (*alerts.Delivery, error) {
	c, e := w.Store.ReportRunContent(ctx, tenant, run)
	if e != nil {
		return nil, e
	}
	r, e := Build(ctx, w.Store, tenant, c, w.Origin, now)
	if e != nil {
		return nil, e
	}
	d := r.Delivery(now)
	return &d, nil
}
