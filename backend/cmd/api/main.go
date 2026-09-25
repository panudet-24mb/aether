package main

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/alerts"
	"aether/backend/internal/app"
	"aether/backend/internal/config"
	"aether/backend/internal/realtime"
	"aether/backend/internal/security"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "health" { // container healthcheck: readiness of this same process
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		res, e := (&http.Client{Timeout: 3 * time.Second}).Get("http://127.0.0.1" + addr[strings.LastIndex(addr, ":"):] + "/health/ready")
		if e != nil || res.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg, e := config.Load()
	if e != nil {
		slog.Error("configuration rejected", "reason", e.Error())
		os.Exit(1)
	}
	repo, e := postgres.Open(cfg.DatabaseURL)
	if e != nil {
		slog.Error("database initialization failed")
		os.Exit(1)
	}
	defer repo.Close()
	repo.Configure(postgres.Options{SampleRetentionDays: cfg.SampleRetentionDays, SampleMinIntervalSec: cfg.SampleMinIntervalSec, BLEHistoryHours: cfg.BLEHistoryHours, DiscoveryLimit: cfg.DiscoveryLimit, AlertsShadow: cfg.AlertsShadow, AutomationCommands: cfg.AutomationCommands})
	service, e := app.New(repo, security.NewTokens(cfg.JWTKey, cfg.Issuer), cfg.Registration)
	if e != nil {
		slog.Error("security initialization failed")
		os.Exit(1)
	}
	if len(cfg.SealKey) > 0 { // a dedicated, separately escrowed key; secrets sealed earlier still open
		service.LegacySecrets = service.Secrets
		service.Secrets = security.DeriveKey(cfg.SealKey, "notification-channels")
	}
	if cfg.AlertsShadow {
		slog.Warn("ALERTS_SHADOW is on: events are recorded; only SOS (button) and smoke/gas/CO (hazard) open alerts; no automations run")
	}
	hub := realtime.NewHub()
	api := httpapi.NewWithHub(cfg, service, repo, hub)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go hub.Listen(workerCtx, cfg.DatabaseURL, postgres.SignalChannel)
	go (&alerts.Worker{Store: repo, Sender: httpapi.NewSender(cfg), Secrets: service.Secrets, LegacySecrets: service.LegacySecrets, Interval: 15 * time.Second}).Run(workerCtx)
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-done
		stopWorker()
		if e := api.ShutdownWithTimeout(10 * time.Second); e != nil {
			slog.Error("graceful shutdown timed out")
		}
	}()
	slog.Info("API starting", "mode", cfg.Mode, "environment", cfg.Environment)
	if e := api.Listen(cfg.Listen); e != nil {
		slog.Error("listener stopped")
		os.Exit(1)
	}
}
