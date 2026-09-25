// Package commander publishes queued device commands (core.device_commands) to the broker. It is the only
// process holding broker credentials that may write command topics, and it may write nothing else.
//
// Delivery is at most once: the store moves a command pending -> sent and COMMITS before it is published, so a
// crash between the two loses the publish (the command then times out) but can never repeat it or deliver it
// late. Expired commands are never published.
package commander

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"context"
	"errors"
	"log/slog"
	"math"
	"time"
)

// Store is the database half (implemented by postgres.Repository).
type Store interface {
	ActiveTenants(context.Context) ([]string, error)
	// ProcessAutomationRequests turns automation command requests (an outbox written by ingest) into commands.
	ProcessAutomationRequests(ctx context.Context, tenant string, now time.Time) (int, error)
	PendingCommandGateways(ctx context.Context, tenant string, now time.Time) ([]string, error)
	ClaimCommands(ctx context.Context, tenant, gateway string, now time.Time, limit int) ([]domain.Command, error)
	MarkCommandsPublished(ctx context.Context, tenant string, ids []string, at time.Time) error
	MarkCommandFailed(ctx context.Context, tenant, id, reason string) error
	TimeoutCommands(ctx context.Context, tenant string, now time.Time) (int, error)
}

// Publisher sends one message (QoS 1, never retained) and reports whether the broker accepted it.
type Publisher interface {
	Publish(topic string, payload []byte) error
}

// Per gateway: at most Rate commands per second with bursts of Burst, so a flood of queued commands cannot
// saturate one site's Zigbee network (a relay needs a few hundred ms per command anyway). The dispatcher never
// waits for a token: it claims from a gateway only what the gateway's bucket allows now and leaves the rest
// pending for the next sweep (they still expire at expires_at). One busy gateway therefore delays nobody else.
const (
	Rate  = 5.0
	Burst = 10.0
)

type Dispatcher struct {
	Store     Store
	Publisher Publisher
	Now       func() time.Time

	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// allowance refills the gateway's bucket and returns how many commands it may publish now.
func (d *Dispatcher) allowance(gateway string, now time.Time) int {
	if d.buckets == nil {
		d.buckets = map[string]*bucket{}
	}
	b := d.buckets[gateway]
	if b == nil {
		b = &bucket{tokens: Burst, at: now}
		d.buckets[gateway] = b
	}
	b.tokens = math.Min(Burst, b.tokens+now.Sub(b.at).Seconds()*Rate)
	b.at = now
	return int(math.Floor(b.tokens))
}

// RunOnce sweeps every active tenant: settles timed-out commands, then per gateway claims what its pace
// allows and publishes it. It returns the number published. It never sleeps.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	tenants, e := d.Store.ActiveTenants(ctx)
	if e != nil {
		return 0, e
	}
	sent := 0
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return sent, ctx.Err()
		}
		if _, e := d.Store.TimeoutCommands(ctx, tenant, d.Now().UTC()); e != nil {
			slog.Warn("command timeout sweep failed", "error", e.Error())
		}
		if _, e := d.Store.ProcessAutomationRequests(ctx, tenant, d.Now().UTC()); e != nil {
			slog.Warn("automation command requests not processed", "error", e.Error())
		}
		gateways, e := d.Store.PendingCommandGateways(ctx, tenant, d.Now().UTC())
		if e != nil {
			slog.Warn("command sweep failed", "error", e.Error())
			continue
		}
		for _, gateway := range gateways {
			n := d.allowance(gateway, d.Now())
			if n == 0 {
				continue
			}
			claimed, e := d.Store.ClaimCommands(ctx, tenant, gateway, d.Now().UTC(), n)
			if e != nil {
				slog.Warn("command claim failed", "error", e.Error())
				continue
			}
			d.buckets[gateway].tokens -= float64(len(claimed))
			published := []string{}
			for _, c := range claimed {
				if d.publish(ctx, tenant, c) {
					published = append(published, c.ID)
				}
			}
			if e := d.Store.MarkCommandsPublished(ctx, tenant, published, d.Now().UTC()); e != nil {
				slog.Warn("command publish time not recorded", "error", e.Error())
			}
			sent += len(published)
		}
	}
	return sent, nil
}

func (d *Dispatcher) publish(ctx context.Context, tenant string, c domain.Command) bool {
	fail := func(reason string) bool {
		if e := d.Store.MarkCommandFailed(ctx, tenant, c.ID, reason); e != nil {
			slog.Warn("command could not be marked failed", "command", c.ID, "error", e.Error())
		}
		return false
	}
	topic, payload, e := message(c)
	if e != nil {
		return fail(e.Error())
	}
	if e := d.Publisher.Publish(topic, payload); e != nil {
		slog.Warn("command publish refused by the broker", "command", c.ID)
		return fail("broker refused the publish")
	}
	slog.Info("command published", "command", c.ID, "gateway", c.GatewayID, "property", c.Property)
	return true
}

// message builds what goes on the wire for one command. A Zigbee2MQTT command is {"<property>": <value>} to
// aether/z2m/<gateway>/<ieee>/set. An Aether Edge command is its wire form, {"dps":{"<id>":<raw>}}, computed from
// the device's specification when it was queued, to aether/edge/<gateway>/<device>/set: the commander never knows
// what a data point means and never holds a local key.
func message(c domain.Command) (string, []byte, error) {
	switch c.Transport {
	case "edge":
		topic, e := edge.SetTopic(c.GatewayID, c.IEEE)
		if e != nil {
			return "", nil, errors.New("invalid command target")
		}
		payload, e := edge.SetPayload(c.Wire)
		if e != nil {
			return "", nil, errors.New("invalid command payload")
		}
		return topic, payload, nil
	case "", "z2m":
		topic, e := zigbee2mqtt.SetTopic(c.GatewayID, c.IEEE)
		if e != nil {
			return "", nil, errors.New("invalid command target")
		}
		payload, e := zigbee2mqtt.SetPayload(c.Property, c.Value)
		if e != nil {
			return "", nil, errors.New("invalid command payload")
		}
		return topic, payload, nil
	}
	return "", nil, errors.New("unknown command transport")
}
