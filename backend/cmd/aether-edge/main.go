// Command aether-edge is the Aether Edge agent (docs/platform/aether-edge.md).
//
//	aether-edge            run the agent (configuration from the environment, written by the installer)
//	aether-edge install    redeem an install code and write the agent's files (used by install.sh)
//	aether-edge health     container healthcheck: is the agent's heartbeat recent?
//	aether-edge version    print the version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aether/backend/internal/edge"
	"aether/backend/internal/tuyalocal"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(edge.Version)
			return
		case "health":
			os.Exit(health(os.Getenv("HEALTH_FILE")))
		case "install":
			os.Exit(install(os.Args[2:]))
		case "run":
		default:
			fmt.Fprintln(os.Stderr, "usage: aether-edge [run|install|health|version]")
			os.Exit(2)
		}
	}
	os.Exit(run())
}

func run() int {
	cfg, e := edge.LoadConfig(os.Getenv)
	log := logger(cfg.LogLevel)
	if e != nil {
		log.Error("invalid configuration", "problems", e.Error())
		return 1
	}
	poller, e := edge.NewPoller(cfg)
	if e != nil {
		log.Error(e.Error())
		return 1
	}
	var agent *edge.Agent
	bus, e := edge.DialBus(cfg, func() {
		if agent != nil {
			agent.OnConnect()
		}
	})
	if e != nil {
		log.Error(e.Error())
		return 1
	}
	defer bus.Close()
	agent = edge.NewAgent(cfg, bus, poller, tuyalocal.Listen, edge.Defaults, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("Aether Edge starting", "version", edge.Version, "config", cfg.String())
	if e := agent.Run(ctx); e != nil {
		log.Error(e.Error())
		return 1
	}
	return 0
}

// health passes while the agent wrote its heartbeat file in the last three minutes (it does every minute).
func health(path string) int {
	if path == "" {
		path = edge.DefaultHealthFile
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return 1
	}
	at, e := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if e != nil || time.Since(time.Unix(at, 0)) > 3*time.Minute {
		return 1
	}
	return 0
}

func install(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	server := fs.String("server", "", "Aether's public origin, e.g. https://aether.example.com")
	dir := fs.String("dir", "/out", "directory to write the agent's files into")
	zigbee := fs.String("zigbee", "", "SLZB coordinator IP address, to add Zigbee2MQTT")
	zigbeeUI := fs.String("zigbee-ui", "", "IPv4 address of this host to publish Zigbee2MQTT's page on (default: loopback only)")
	image := fs.String("image", "", "aether-edge image reference to pin")
	z2mImage := fs.String("z2m-image", "", "Zigbee2MQTT image reference to pin")
	webCA := fs.String("web-ca", "", "PEM file of a private CA for Aether's web front")
	if e := fs.Parse(args); e != nil {
		return 2
	}
	// The code comes from the environment only: never an argument, so it is not in any process list.
	o := edge.InstallOptions{Server: *server, Code: os.Getenv("AETHER_INSTALL_CODE"), Dir: *dir, Zigbee: *zigbee, ZigbeeUI: *zigbeeUI,
		Image: *image, Z2MImage: *z2mImage, WebCAFile: *webCA, Out: os.Stdout}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if e := edge.Install(ctx, o); e != nil {
		fmt.Fprintln(os.Stderr, "aether-edge install:", e.Error())
		return 1
	}
	fmt.Println("aether-edge install: files written")
	return 0
}

func logger(level string) *slog.Logger {
	l := slog.LevelInfo
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
