package alerts

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"log/slog"
	"time"
)

// Store is the slice of the repository the worker needs.
type Store interface {
	ActiveTenants(context.Context) ([]string, error)
	ScanOffline(context.Context, string, time.Time) (int, error)
	// ScanVacancy ends occupied episodes whose PIR has seen no motion for OccupancyHoldSec (one `vacant` each).
	ScanVacancy(context.Context, string, time.Time) (int, error)
	ClaimNotifications(context.Context, string, int, time.Time) ([]domain.NotificationJob, error)
	FinishNotification(context.Context, string, string, string, string) error
	PruneAlertData(context.Context, string) error
	PruneHistory(context.Context, string) error
}

// Worker runs offline detection and notification delivery for every active tenant.
type Worker struct {
	Store         Store
	Sender        *Sender
	Secrets       []byte
	LegacySecrets []byte // key used before CHANNEL_SEAL_KEY existed
	Interval      time.Duration
	Batch         int
	ticks         int
}

func (w *Worker) Run(ctx context.Context) {
	if w.Interval <= 0 {
		w.Interval = 15 * time.Second
	}
	if w.Batch <= 0 {
		w.Batch = 10
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
		slog.Warn("alert worker: tenant listing failed")
		return
	}
	w.ticks++
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return
		}
		if w.ticks%40 == 1 { // roughly every ten minutes at the default interval
			if e := w.Store.PruneAlertData(ctx, tenant); e != nil {
				slog.Warn("alert worker: prune failed", "tenant", tenant)
			}
			if e := w.Store.PruneHistory(ctx, tenant); e != nil {
				slog.Warn("alert worker: history retention failed", "tenant", tenant, "error", e.Error())
			}
		}
		if n, e := w.Store.ScanOffline(ctx, tenant, now); e != nil {
			slog.Warn("alert worker: offline scan failed", "tenant", tenant)
		} else if n > 0 {
			slog.Info("alert worker: devices marked offline", "tenant", tenant, "count", n)
		}
		if _, e := w.Store.ScanVacancy(ctx, tenant, now); e != nil {
			slog.Warn("alert worker: vacancy scan failed", "tenant", tenant)
		}
		w.deliver(ctx, tenant, now)
	}
}

func (w *Worker) deliver(ctx context.Context, tenant string, now time.Time) {
	jobs, e := w.Store.ClaimNotifications(ctx, tenant, w.Batch, now)
	if e != nil {
		slog.Warn("alert worker: claim failed", "tenant", tenant)
		return
	}
	for _, job := range jobs {
		if job.Channel.ID == "" || job.Alert.ID == "" { // channel deleted/disabled or alert pruned: retrying cannot help
			_ = w.Store.FinishNotification(ctx, tenant, job.Notification.ID, "failed", "channel removed or disabled")
			continue
		}
		secret := ""
		if job.SecretEnc != "" {
			if secret, e = security.OpenAny(job.SecretEnc, w.Secrets, w.LegacySecrets); e != nil {
				_ = w.Store.FinishNotification(ctx, tenant, job.Notification.ID, "failed", "channel secret unreadable (signing key changed?)")
				continue
			}
		}
		payload := Payload{Schema: "aether.alert.v1", SentAt: now, Text: Message(job.Alert, job.Event, job.GatewayName), Alert: job.Alert, Event: job.Event, Gateway: job.GatewayName, TenantID: tenant}
		sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := w.Sender.Send(sendCtx, job.Channel, secret, payload)
		cancel()
		outcome, msg := "sent", ""
		if err != nil {
			outcome, msg = "retry", err.Error()
		}
		if e := w.Store.FinishNotification(ctx, tenant, job.Notification.ID, outcome, msg); e != nil {
			slog.Warn("alert worker: finish failed", "tenant", tenant, "notification", job.Notification.ID)
		}
	}
}
