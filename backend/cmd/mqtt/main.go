package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"aether/backend/internal/adapters/mqttingest"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/app"
	"aether/backend/internal/config"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func fatal(message string) { slog.Error(message); os.Exit(1) }
func main() {
	cfg, e := config.Load()
	if e != nil {
		fatal("invalid backend configuration")
	}
	if len(os.Args) == 2 && os.Args[1] == "health" { // container healthcheck: can this image reach the database?
		repo, e := postgres.Open(cfg.DatabaseURL)
		if e != nil {
			os.Exit(1)
		}
		repo.Close()
		os.Exit(0)
	}
	mc, e := mqttingest.Load(os.Getenv("MQTT_CONFIG_FILE"))
	if e != nil {
		fatal("invalid MQTT configuration")
	}
	ca, e := os.ReadFile(mc.CAFile)
	if e != nil {
		fatal("cannot read MQTT CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		fatal("invalid MQTT CA")
	}
	repo, e := postgres.Open(cfg.DatabaseURL)
	if e != nil {
		fatal("database unavailable or role unsafe")
	}
	defer repo.Close()
	repo.Configure(postgres.Options{SampleRetentionDays: cfg.SampleRetentionDays, SampleMinIntervalSec: cfg.SampleMinIntervalSec, BLEHistoryHours: cfg.BLEHistoryHours, DiscoveryLimit: cfg.DiscoveryLimit, AlertsShadow: cfg.AlertsShadow, AutomationCommands: cfg.AutomationCommands})
	if cfg.AlertsShadow {
		slog.Warn("ALERTS_SHADOW is on: events are recorded; only SOS (button) and smoke/gas/CO (hazard) open alerts; no automations run")
	}
	service, e := app.New(repo, security.NewTokens(cfg.JWTKey, cfg.Issuer), false)
	if e != nil {
		fatal("service initialization failed")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	opts := mqtt.NewClientOptions().AddBroker(mc.BrokerURL).SetClientID(mc.ClientID).SetUsername(mc.Username).SetPassword(mc.Password).
		SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}).SetCleanSession(false).SetAutoReconnect(true).
		SetConnectRetry(true).SetConnectTimeout(10 * time.Second).SetKeepAlive(60 * time.Second).SetAutoAckDisabled(true)
	handler := func(_ mqtt.Client, m mqtt.Message) {
		// Ordered handling bounds memory. Commit precedes ACK; retry by restarting the
		// persistent subscriber on transient failure. Duplicates are possible (QoS 1).
		// A bug in a decoder must not take the collector down with an unacknowledged packet in flight forever.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("MQTT handler panicked; packet acknowledged and dropped", "topic", m.Topic())
				m.Ack()
			}
		}()
		var id string
		store := func() error {
			call, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var err error
			// Zigbee2MQTT bridges publish a whole tree under aether/z2m/<gateway id>. Requests going TO the bridge
			// (…/set, bridge/request/*) and documents Aether does not use are acknowledged and dropped.
			if strings.HasPrefix(m.Topic(), zigbee2mqtt.Prefix) {
				gateway, msg, rerr := zigbee2mqtt.Route(m.Topic())
				if rerr != nil || msg.Kind == zigbee2mqtt.Ignore {
					return rerr
				}
				var tenant string
				if tenant, err = repo.MQTTGatewayTenant(call, gateway); err != nil {
					return err
				}
				id, err = service.CaptureZ2M(call, tenant, gateway, msg, m.Payload())
				return err
			}
			if strings.HasPrefix(m.Topic(), "/aether/gateways/") {
				gateway := strings.TrimSuffix(strings.TrimPrefix(m.Topic(), "/aether/gateways/"), "/status")
				if !strings.HasSuffix(m.Topic(), "/status") || !security.ValidID(gateway) {
					return domain.ErrForbidden
				}
				var tenant string
				if tenant, err = repo.MQTTGatewayTenant(call, gateway); err != nil {
					return err
				}
				id, err = service.Capture(call, tenant, gateway, m.Payload())
				return err
			}
			id, err = mqttingest.Capture(call, service, mc.Bindings, m.Topic(), m.Payload())
			return err
		}
		err := store()
		// A database blip (restart, failover, lock timeout) should not kill the collector. Retry the same packet
		// with backoff for about a minute; only then exit WITHOUT acknowledging, so the broker redelivers it
		// to the restarted process (QoS 1, persistent session).
		for attempt := 1; err != nil && !mqttingest.Permanent(err) && attempt <= 6 && ctx.Err() == nil; attempt++ {
			slog.Warn("MQTT storage failed; retrying", "attempt", attempt)
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			}
			err = store()
		}
		if err != nil {
			if mqttingest.Permanent(err) {
				slog.Warn("MQTT packet rejected")
				m.Ack()
				return
			}
			fatal("MQTT storage unavailable; restart without acknowledging packet")
		}
		m.Ack()
		slog.Info("MQTT packet stored", "packet_id", id, "decoded", false)
	}
	opts.SetDefaultPublishHandler(handler)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		t := c.Subscribe("/aether/gateways/+/status", 1, handler)
		if !t.WaitTimeout(10*time.Second) || t.Error() != nil {
			fatal("MQTT onboarding subscription failed")
		}
		// Not fatal: during a rolling deploy the broker ACL may not grant this tree yet, and BLE capture must go on.
		t = c.Subscribe(zigbee2mqtt.Prefix+"+/#", 1, handler)
		if !t.WaitTimeout(10*time.Second) || t.Error() != nil {
			slog.Error("MQTT Zigbee2MQTT subscription failed; Zigbee gateways are not captured until the next reconnect")
		}
		for _, b := range mc.Bindings {
			t := c.Subscribe(b.Topic, 1, handler)
			if !t.WaitTimeout(10*time.Second) || t.Error() != nil {
				fatal("MQTT subscription failed")
			}
		}
		slog.Info("MQTT capture ready")
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, _ error) { slog.Warn("MQTT connection lost; reconnecting") })
	client := mqtt.NewClient(opts)
	client.Connect()
	<-ctx.Done()
	client.Disconnect(1000)
}
