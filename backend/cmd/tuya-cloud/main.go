// tuya-cloud runs every Tuya Cloud link (docs/platform/tuya-cloud.md): per linked cloud project it consumes the
// Message Service, keeps the device list in sync, delivers cloud commands through the OpenAPI and reports health.
// It is the only process that can open a project's credentials: it alone holds TUYA_CLOUD_PRIVATE_KEY, whose
// public half the API seals to. It holds nothing else but an aether_app DSN (no JWT or channel seal key).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/tuyacloudlink"
	"aether/backend/internal/tuyacloudlink/cloudkeys"
	"github.com/jackc/pgx/v5"
)

func fatal(message string) { slog.Error(message); os.Exit(1) }

// poll is the command dispatcher's fallback cadence when no notification arrives.
const poll = 2 * time.Second

// Defaults of the monthly budget guard. They are conservative placeholders, not Tuya's published numbers: a
// project's allowance depends on its plan and is shown in its Tuya console. Set TUYA_CLOUD_EVENT_BUDGET and
// TUYA_CLOUD_API_BUDGET to the real plan (0 turns that guard off).
const (
	defaultEventBudget = 68000
	defaultAPIBudget   = 26000
	defaultMaxLinks    = 50
	// DefaultMaxLinksPerTenant matches the API's per-workspace cap on linked projects.
	defaultMaxLinksPerTenant = 2
)

func number(name string, fallback int64) int64 {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || n < 0 {
		fatal(name + " must be a non-negative integer")
	}
	return n
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if len(os.Args) == 2 && os.Args[1] == "health" { // container healthcheck: can this image reach the database?
		repo, e := postgres.Open(dsn)
		if e != nil {
			os.Exit(1)
		}
		repo.Close()
		os.Exit(0)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// With Tuya Cloud mode off (the default) the worker idles instead of exiting: the service stays in every stack
	// with restart: unless-stopped, which would restart an exited process forever. It opens nothing and dials
	// nobody; a link left from before the mode was turned off stays down and its devices go offline.
	switch os.Getenv("TUYA_CLOUD") {
	case "true":
	case "", "false":
		slog.Info("Tuya Cloud mode is off (TUYA_CLOUD=false); idle")
		<-ctx.Done()
		return
	default:
		fatal("TUYA_CLOUD must be true or false")
	}
	key, e := cloudkeys.ParseKey(os.Getenv("TUYA_CLOUD_PRIVATE_KEY"))
	if e != nil {
		fatal("TUYA_CLOUD_PRIVATE_KEY (32 base64-encoded bytes) is required to open Tuya Cloud credentials")
	}
	repo, e := postgres.Open(dsn)
	if e != nil {
		fatal("database unavailable or role unsafe")
	}
	defer repo.Close()

	w := &tuyacloudlink.Worker{Store: repo, Keys: []*cloudkeys.Key{key}, MaxLinks: int(number("TUYA_CLOUD_MAX_LINKS", defaultMaxLinks)),
		MaxLinksPerTenant: int(number("TUYA_CLOUD_MAX_LINKS_PER_TENANT", defaultMaxLinksPerTenant)),
		Budget:            tuyacloudlink.Budget{EventsMonth: number("TUYA_CLOUD_EVENT_BUDGET", defaultEventBudget), APICallsMonth: number("TUYA_CLOUD_API_BUDGET", defaultAPIBudget)}}
	links := make(chan struct{}, 1)
	commands := make(chan struct{}, 1)
	go listen(ctx, dsn, postgres.TuyaCloudChannel, links)
	go listen(ctx, dsn, postgres.CommandChannel, commands)
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, links) }()

	d := w.Dispatcher()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	slog.Info("Tuya Cloud worker ready")
	for {
		if _, e := d.RunOnce(ctx); e != nil && ctx.Err() == nil {
			slog.Warn("cloud command dispatch failed; retrying", "error", e.Error())
		}
		select {
		case <-ctx.Done():
			<-done
			return
		case <-commands:
		case <-ticker.C:
		}
	}
}

// listen turns NOTIFY on channel into a wake-up. The payload (a tenant id) is not needed: a wake-up reconciles
// or sweeps everything.
func listen(ctx context.Context, dsn, channel string, wake chan<- struct{}) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, e := pgx.Connect(ctx, dsn)
		if e == nil {
			if _, e = conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); e == nil {
				backoff = time.Second
				for e == nil {
					if _, e = conn.WaitForNotification(ctx); e == nil {
						select {
						case wake <- struct{}{}:
						default:
						}
					}
				}
			}
			conn.Close(context.Background())
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("listener disconnected; polling until it reconnects", "channel", channel, "in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
