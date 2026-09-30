// demo-twin builds and drives the digital twin demo workspace (docs/platform/digital-twin.md): a fictional
// hospital, separate from real data, whose sensors and people are simulated through the real HTTP ingest.
package main

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/app"
	"aether/backend/internal/demotwin"
	"aether/backend/internal/security"
	"aether/backend/internal/simulation"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const usage = `usage:
  demo-twin setup             creates the demo workspace (DATABASE_URL, MIGRATION_DATABASE_URL; DEMO_STATE file to
                              write, default ./demo-twin.json; DEMO_EMAIL optional). Prints where the owner login is.
  demo-twin run [--insecure-http]
                              posts the 30-minute script through the ingest every 6 s (AETHER_ORIGIN, default
                              http://localhost:8080; DEMO_STATE)
  demo-twin backfill [--hours N] [--every 30s]
                              plays the script over the last N hours (default 24, at most 168) straight into the
                              database as if it had happened then, so the twin's replay has history at once. Only a
                              demo workspace (DATABASE_URL, DEMO_STATE; ALERTS_SHADOW=true like the API to open only
                              SOS alerts). Run it on a fresh demo workspace, before run. 24 hours take ~25 minutes.
  demo-twin trigger [--insecure-http] sos|spike|door
                              fires an event now, on top of the script (sos posts the press; spike and door are read
                              by a running "run" for the next few minutes)

The gateway tokens travel in every uplink: an http:// origin other than localhost is refused unless --insecure-http.`

func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func statePath() string {
	if p := os.Getenv("DEMO_STATE"); p != "" {
		return p
	}
	return "demo-twin.json"
}

func origin() string {
	if o := os.Getenv("AETHER_ORIGIN"); o != "" {
		return o
	}
	return "http://localhost:8080"
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	insecure := false
	rest := []string{}
	for _, a := range args[1:] {
		if a == "--insecure-http" {
			insecure = true
			continue
		}
		rest = append(rest, a)
	}
	switch args[0] {
	case "setup":
		if len(rest) != 0 {
			return errors.New(usage)
		}
		return setup()
	case "run":
		if len(rest) != 0 {
			return errors.New(usage)
		}
		if e := checkOrigin(insecure); e != nil {
			return e
		}
		return drive()
	case "backfill":
		return backfill(rest)
	case "trigger":
		if len(rest) != 1 {
			return errors.New(usage)
		}
		if e := checkOrigin(insecure); e != nil {
			return e
		}
		return trigger(rest[0])
	}
	return errors.New(usage)
}

// checkOrigin prints where the uplinks go and refuses plain HTTP to anything but this machine.
func checkOrigin(insecure bool) error {
	u, e := url.Parse(origin())
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("AETHER_ORIGIN must be an http(s) origin")
	}
	fmt.Fprintf(os.Stderr, "demo-twin: posting to %s://%s\n", u.Scheme, u.Host)
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme == "http" && !local && !insecure {
		return fmt.Errorf("refusing plain http to %s: the gateway tokens would travel in clear text (use https, or --insecure-http)", u.Host)
	}
	return nil
}

// hostOf names the database a URL points at, without its credentials.
func hostOf(raw string) string {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" {
		return "(unparsable URL)"
	}
	return u.Host + u.Path
}

func setup() error {
	path := statePath()
	fmt.Fprintf(os.Stderr, "demo-twin: database %s (migration role on %s), state file %s\n", hostOf(os.Getenv("DATABASE_URL")), hostOf(os.Getenv("MIGRATION_DATABASE_URL")), path)
	// The state file holds the owner password and every gateway token: create it new (never over an existing file or
	// through a symlink), owner-only, before anything is created, so a failure halfway still leaves the credentials.
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if e != nil {
		return fmt.Errorf("create %s (one demo workspace per state file): %w", path, e)
	}
	defer f.Close()
	save := func(st demotwin.State) error {
		b, _ := json.MarshalIndent(st, "", "  ")
		if e := f.Truncate(0); e != nil {
			return e
		}
		if _, e := f.WriteAt(b, 0); e != nil {
			return e
		}
		return f.Sync()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin, e := sql.Open("pgx", os.Getenv("MIGRATION_DATABASE_URL"))
	if e != nil {
		return e
	}
	defer admin.Close()
	if e := admin.PingContext(ctx); e != nil {
		return fmt.Errorf("migration database: %w", e)
	}
	// Marking the workspace demo is what keeps its notifications from leaving: refuse before creating anything when
	// the database cannot (migration 00045 not applied).
	var n int
	if e := admin.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='core' AND table_name='tenants' AND column_name='demo'`).Scan(&n); e != nil || n != 1 {
		return fmt.Errorf("core.tenants.demo is missing: run the migrations first (00045)")
	}
	repo, e := postgres.Open(os.Getenv("DATABASE_URL"))
	if e != nil {
		return e
	}
	defer repo.Close()
	s, e := app.New(repo, security.NewTokens([]byte(security.RandomToken()), "aether"), true)
	if e != nil {
		return e
	}
	email := os.Getenv("DEMO_EMAIL")
	if email == "" {
		email = "demo-twin-" + strings.ToLower(security.RandomToken()[:8]) + "@demo.aether.invalid"
	}
	st, e := demotwin.Setup(ctx, s, email, security.RandomToken(), "Aether Demo · โรงพยาบาล", func(tenant string) error {
		_, e := admin.ExecContext(ctx, `UPDATE core.tenants SET demo=true WHERE id=$1`, tenant)
		return e
	})
	if st.TenantID != "" {
		if se := save(st); se != nil && e == nil {
			e = se
		}
	}
	if e != nil {
		if st.TenantID != "" {
			return fmt.Errorf("%w (what was created is in %s)", e, path)
		}
		f.Close()
		_ = os.Remove(path)
		return e
	}
	fmt.Printf("Demo workspace ready. tenant_id=%s site_id=%s\nOwner login (email and password) is in %s (0600); keep it private.\n", st.TenantID, st.SiteID, path)
	return nil
}

func load() (demotwin.State, error) {
	var st demotwin.State
	b, e := os.ReadFile(statePath())
	if e != nil {
		return st, fmt.Errorf("read %s (run setup first): %w", statePath(), e)
	}
	return st, json.Unmarshal(b, &st)
}

// Overrides live next to the state file, so trigger can reach a running run without talking to it.
func overridePath(kind string) string { return statePath() + "." + kind }

func overrides(now time.Time) simulation.TwinOverrides {
	o := simulation.TwinOverrides{}
	read := func(kind string) time.Time {
		b, e := os.ReadFile(overridePath(kind))
		if e != nil {
			return time.Time{}
		}
		t, _ := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
		return t
	}
	o.SpikeUntil, o.DoorUntil = read("spike"), read("door")
	return o
}

func drive() error {
	st, e := load()
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 5 * time.Second}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("demo-twin running", "origin", origin(), "gateways", len(st.Gateways), "loop_minutes", simulation.TwinLoopSteps*simulation.TwinStepSec/60)
	for first := true; ; first = false {
		now := time.Now()
		step := simulation.TwinStep(now)
		o := overrides(now)
		o.Warmup = first
		if e := demotwin.PostStep(ctx, client, origin(), st, step, now, o); e != nil && ctx.Err() == nil {
			log.Warn("uplink failed", "step", step, "err", e)
		}
		if step%10 == 0 {
			log.Info("script", "step", step, "minute", step*simulation.TwinStepSec/60)
		}
		next := now.Truncate(simulation.TwinStepSec * time.Second).Add(simulation.TwinStepSec * time.Second)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(next)):
		}
	}
}

func backfill(args []string) error {
	hours, every := 24, 30*time.Second
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--hours" && i+1 < len(args):
			n, e := strconv.Atoi(args[i+1])
			if e != nil || n < 1 || n > 168 {
				return errors.New("--hours must be 1..168")
			}
			hours = n
			i++
		case args[i] == "--every" && i+1 < len(args):
			d, e := time.ParseDuration(args[i+1])
			if e != nil || d < simulation.TwinStepSec*time.Second || d > 5*time.Minute {
				return errors.New("--every must be 6s..5m")
			}
			every = d
			i++
		default:
			return errors.New(usage)
		}
	}
	st, e := load()
	if e != nil {
		return e
	}
	fmt.Fprintf(os.Stderr, "demo-twin: database %s, workspace %s, last %d hours every %s\n", hostOf(os.Getenv("DATABASE_URL")), st.TenantID, hours, every)
	repo, e := postgres.Open(os.Getenv("DATABASE_URL"))
	if e != nil {
		return e
	}
	defer repo.Close()
	// The history should look like what the API would have made of it: ALERTS_SHADOW as the API has it (production
	// runs with it on, so only the SOS opens alerts). Unset: off, every demo rule opens its alerts.
	repo.Configure(postgres.Options{AlertsShadow: os.Getenv("ALERTS_SHADOW") == "true"})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	to := time.Now().Add(-time.Minute)
	n, e := demotwin.Backfill(ctx, repo, st, to.Add(-time.Duration(hours)*time.Hour), to, every, func(at time.Time) {
		fmt.Fprintf(os.Stderr, "  %s\n", at.Local().Format("2006-01-02 15:04"))
	})
	if e != nil {
		return fmt.Errorf("after %d uplinks: %w", n, e)
	}
	fmt.Printf("Backfilled %d uplinks over %d hours; the twin's replay can show them now.\n", n, hours)
	return nil
}

func trigger(kind string) error {
	st, e := load()
	if e != nil {
		return e
	}
	now := time.Now()
	switch kind {
	case "sos":
		// Two uplinks with the button's iBeacon slot, like a real press burst; run's next uplinks end it.
		client := &http.Client{Timeout: 5 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for i := 0; i < 2; i++ {
			at := time.Now()
			if e := demotwin.PostSOS(ctx, client, origin(), st, simulation.TwinStep(at), at); e != nil {
				return e
			}
			time.Sleep(2 * time.Second)
		}
		fmt.Println("SOS pressed in ward 3")
	case "spike":
		if e := os.WriteFile(overridePath("spike"), []byte(now.Add(5*time.Minute).Format(time.RFC3339)), 0o600); e != nil {
			return e
		}
		fmt.Println("cold store warms to 12 °C for 5 minutes (needs a running demo-twin run)")
	case "door":
		if e := os.WriteFile(overridePath("door"), []byte(now.Add(3*time.Minute).Format(time.RFC3339)), 0o600); e != nil {
			return e
		}
		fmt.Println("someone at the cold-store door (PIR) for 3 minutes (needs a running demo-twin run)")
	default:
		return errors.New(usage)
	}
	return nil
}
