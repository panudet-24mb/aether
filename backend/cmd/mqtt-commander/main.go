// mqtt-commander publishes queued device commands (core.device_commands) to the broker. Its broker account may
// write aether/z2m/+/+/set and nothing else: it cannot subscribe, and cannot reach bridge/request/* (so it can
// never permit joins or remove devices). It holds no other secret than that account and an aether_app DSN.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aether/backend/internal/adapters/mqttingest"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/commander"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jackc/pgx/v5"
)

func fatal(message string) { slog.Error(message); os.Exit(1) }

// poll is the fallback cadence when no notification arrives (a lost LISTEN connection, a missed NOTIFY).
const poll = 2 * time.Second

type broker struct{ client mqtt.Client }

func (b broker) Publish(topic string, payload []byte) error {
	t := b.client.Publish(topic, 1, false, payload)
	if !t.WaitTimeout(5 * time.Second) {
		return errors.New("publish timed out")
	}
	return t.Error()
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
	repo, e := postgres.Open(dsn)
	if e != nil {
		fatal("database unavailable or role unsafe")
	}
	defer repo.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// A clean session with no subscriptions: nothing is ever queued for this client, and it publishes QoS 1
	// without retain, so a command is never replayed to a bridge that connects later.
	opts := mqtt.NewClientOptions().AddBroker(mc.BrokerURL).SetClientID(mc.ClientID).SetUsername(mc.Username).SetPassword(mc.Password).
		SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}).SetCleanSession(true).SetAutoReconnect(true).
		SetConnectRetry(true).SetConnectTimeout(10 * time.Second).SetKeepAlive(60 * time.Second)
	opts.SetOnConnectHandler(func(mqtt.Client) { slog.Info("MQTT commander connected") })
	opts.SetConnectionLostHandler(func(mqtt.Client, error) { slog.Warn("MQTT commander connection lost; reconnecting") })
	client := mqtt.NewClient(opts)
	client.Connect()
	defer client.Disconnect(1000)

	wake := make(chan struct{}, 1)
	go listen(ctx, dsn, wake)
	d := &commander.Dispatcher{Store: repo, Publisher: broker{client}, Now: time.Now}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	slog.Info("MQTT commander ready")
	for {
		if client.IsConnectionOpen() {
			if _, e := d.RunOnce(ctx); e != nil && ctx.Err() == nil {
				slog.Warn("command dispatch failed; retrying", "error", e.Error())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

// listen turns NOTIFY aether_command (sent by the transaction that queued a command) into a wake-up, so a
// click is published within milliseconds instead of at the next poll.
func listen(ctx context.Context, dsn string, wake chan<- struct{}) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, e := pgx.Connect(ctx, dsn)
		if e == nil {
			if _, e = conn.Exec(ctx, "LISTEN "+pgx.Identifier{postgres.CommandChannel}.Sanitize()); e == nil {
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
		slog.Warn("command listener disconnected; polling until it reconnects", "in", backoff.String())
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
