package demotwin

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/simulation"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Message is one MQTT publish of the simulated coordinator or Edge.
type Message = simulation.Z2MMessage

// ZigbeeAnnounce is what the coordinator publishes when it connects (retained): the bridge state, its device
// list, every device's availability and its first state.
func ZigbeeAnnounce(gateway string, x simulation.TwinExtras) []Message {
	base := zigbee2mqtt.BaseTopic(gateway)
	out := []Message{
		{Topic: base + "/bridge/state", Payload: simulation.Z2MOnline(true), Retain: true},
		{Topic: base + "/bridge/devices", Payload: simulation.TwinZigbeeBridgeDevices(x), Retain: true},
	}
	for _, d := range x.ZigbeeDev {
		out = append(out, Message{Topic: base + "/" + d.Name + "/availability", Payload: simulation.Z2MOnline(true), Retain: true})
	}
	now := time.Now()
	for _, d := range x.ZigbeeDev {
		out = append(out, zigbeeState(base, d, simulation.TwinZigbeeState(d, simulation.TwinStep(now), now, simulation.TwinOverrides{})))
	}
	return out
}

func zigbeeState(base string, d simulation.TwinZigbee, state map[string]any) Message {
	m := map[string]any{"aether_source": "simulated", "last_seen": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "device": map[string]any{"ieeeAddr": d.IEEE, "friendlyName": d.Name, "model": d.Def.Model}}
	for k, v := range state {
		m[k] = v
	}
	p, _ := json.Marshal(m)
	return Message{Topic: base + "/" + d.Name, Payload: p}
}

// EdgeAnnounce is what the Edge publishes when it connects: online, a heartbeat, its LAN list, every device's
// availability and a full status of each.
func EdgeAnnounce(gateway string, x simulation.TwinExtras) []Message {
	root := edge.Prefix + gateway + "/"
	lan := []map[string]string{}
	for _, d := range x.Tuya {
		lan = append(lan, map[string]string{"id": d.ID, "ip": d.IP, "version": d.Version, "product_key": "aether" + d.Category})
	}
	lanJSON, _ := json.Marshal(lan)
	out := []Message{
		{Topic: root + "status", Payload: []byte(`{"state":"online"}`), Retain: true},
		{Topic: root + "health", Payload: edgeHealth(len(x.Tuya))},
		{Topic: root + "discovery", Payload: lanJSON},
	}
	now := time.Now()
	for _, d := range x.Tuya {
		out = append(out, Message{Topic: root + d.ID + "/availability", Payload: []byte(`{"state":"online"}`), Retain: true})
		out = append(out, edgeState(root, d, simulation.TwinTuyaDPS(d, simulation.TwinStep(now), now), true))
	}
	return out
}

func edgeHealth(n int) []byte {
	p, _ := json.Marshal(map[string]any{"version": "0.2.1", "devices_connected": n, "lan_seen": n, "ble": map[string]any{"state": "off"}})
	return p
}

func edgeState(root string, d simulation.TwinTuya, dps map[string]any, full bool) Message {
	p, _ := json.Marshal(map[string]any{"dps": dps, "full": full})
	return Message{Topic: root + d.ID + "/state", Payload: p}
}

// Bridge drives the coordinator and the Edge step by step: every device reports every 30 s (staggered) and at
// once when its state changes, a remote is pressed twice a loop, and commands sent from Aether (…/set) are applied
// and held for a while, so switching a plug or a light from the UI answers like the real device would.
type Bridge struct {
	X       simulation.TwinExtras
	Zigbee  string // gateway ids
	Edge    string
	HoldFor time.Duration

	mu      sync.Mutex
	last    map[string]string
	zigSet  map[string]held // ieee -> values
	edgeSet map[string]held // tuya id -> dps
}

type held struct {
	values map[string]any
	until  time.Time
}

func NewBridge(zigbeeGateway, edgeGateway string) *Bridge {
	return &Bridge{X: simulation.TwinExtended(), Zigbee: zigbeeGateway, Edge: edgeGateway, HoldFor: 15 * time.Minute,
		last: map[string]string{}, zigSet: map[string]held{}, edgeSet: map[string]held{}}
}

func (b *Bridge) apply(set map[string]held, key string, state map[string]any, now time.Time) map[string]any {
	h, ok := set[key]
	if !ok || now.After(h.until) {
		delete(set, key)
		return state
	}
	for k, v := range h.values {
		state[k] = v
	}
	return state
}

// Step is what the coordinator and the Edge publish at one step.
func (b *Bridge) Step(step int, at time.Time, o simulation.TwinOverrides) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []Message{}
	if b.Zigbee != "" {
		base := zigbee2mqtt.BaseTopic(b.Zigbee)
		for i, d := range b.X.ZigbeeDev {
			state := b.apply(b.zigSet, d.IEEE, simulation.TwinZigbeeState(d, step, at, o), at)
			key := stateKey(state)
			if (step+i)%5 == 0 || b.last[d.IEEE] != key {
				out = append(out, zigbeeState(base, d, state))
				b.last[d.IEEE] = key
			}
			if action := simulation.TwinZigbeePress(d, step); action != "" {
				press := map[string]any{"action": action, "linkquality": state["linkquality"]}
				if v, ok := state["battery"]; ok {
					press["battery"] = v
				}
				out = append(out, zigbeeState(base, d, press))
			}
		}
	}
	if b.Edge != "" {
		root := edge.Prefix + b.Edge + "/"
		for i, d := range b.X.Tuya {
			dps := b.apply(b.edgeSet, d.ID, simulation.TwinTuyaDPS(d, step, at), at)
			key := stateKey(dps)
			if (step+i)%5 == 0 || b.last[d.ID] != key {
				out = append(out, edgeState(root, d, dps, true))
				b.last[d.ID] = key
			}
		}
		if step%10 == 0 {
			out = append(out, Message{Topic: root + "health", Payload: edgeHealth(len(b.X.Tuya))})
		}
	}
	return out
}

func stateKey(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// Command applies a /set published by Aether and returns what the device answers (nothing for a topic that is not
// a command for one of its devices).
func (b *Bridge) Command(topic string, payload []byte, now time.Time) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Zigbee != "" {
		base := zigbee2mqtt.BaseTopic(b.Zigbee) + "/"
		if rest, ok := strings.CutPrefix(topic, base); ok && strings.HasSuffix(rest, "/set") {
			name := strings.TrimSuffix(rest, "/set")
			var set map[string]any
			if json.Unmarshal(payload, &set) != nil {
				return nil
			}
			for _, d := range b.X.ZigbeeDev {
				if d.IEEE != name && d.Name != name {
					continue
				}
				h := b.zigSet[d.IEEE]
				if h.values == nil || now.After(h.until) {
					h.values = map[string]any{}
				}
				current := b.apply(map[string]held{d.IEEE: h}, d.IEEE, simulation.TwinZigbeeState(d, simulation.TwinStep(now), now, simulation.TwinOverrides{}), now)
				for k, v := range set {
					if s, ok := v.(string); ok && strings.EqualFold(s, "TOGGLE") {
						if current[k] == "ON" {
							v = "OFF"
						} else {
							v = "ON"
						}
					}
					h.values[k] = v
					if k == "position" {
						h.values["state"] = "STOP"
					}
				}
				h.until = now.Add(b.HoldFor)
				b.zigSet[d.IEEE] = h
				state := b.apply(b.zigSet, d.IEEE, simulation.TwinZigbeeState(d, simulation.TwinStep(now), now, simulation.TwinOverrides{}), now)
				b.last[d.IEEE] = stateKey(state)
				return []Message{zigbeeState(zigbee2mqtt.BaseTopic(b.Zigbee), d, state)}
			}
			return nil
		}
	}
	if b.Edge != "" {
		root := edge.Prefix + b.Edge + "/"
		if rest, ok := strings.CutPrefix(topic, root); ok && strings.HasSuffix(rest, "/set") {
			id := strings.TrimSuffix(rest, "/set")
			var cmd struct {
				DPS map[string]any `json:"dps"`
			}
			if json.Unmarshal(payload, &cmd) != nil || len(cmd.DPS) == 0 {
				return nil
			}
			for _, d := range b.X.Tuya {
				if d.ID != id {
					continue
				}
				h := b.edgeSet[d.ID]
				if h.values == nil || now.After(h.until) {
					h.values = map[string]any{}
				}
				for k, v := range cmd.DPS {
					h.values[k] = v
				}
				h.until = now.Add(b.HoldFor)
				b.edgeSet[d.ID] = h
				dps := b.apply(b.edgeSet, d.ID, simulation.TwinTuyaDPS(d, simulation.TwinStep(now), now), now)
				b.last[d.ID] = stateKey(dps)
				return []Message{edgeState(root, d, dps, true)}
			}
		}
	}
	return nil
}

// MQTTLink connects the coordinator and the Edge to the broker with their own accounts, over TLS.
type MQTTLink struct {
	bridge  *Bridge
	clients []mqtt.Client
}

// ConnectMQTT connects every MQTT gateway of the state. url is the broker (ssl://mqtt:8883 inside the production
// compose network), caFile the broker's CA certificate.
func ConnectMQTT(ctx context.Context, st State, url, caFile string, bridge *Bridge) (*MQTTLink, error) {
	if !strings.HasPrefix(url, "ssl://") {
		return nil, errors.New("demotwin: the broker URL must be ssl:// (the gateway passwords travel in the connection)")
	}
	ca, e := os.ReadFile(caFile)
	if e != nil {
		return nil, fmt.Errorf("broker CA: %w", e)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("demotwin: invalid broker CA")
	}
	link := &MQTTLink{bridge: bridge}
	for _, g := range []*MQTTGateway{st.Zigbee, st.Edge} {
		if g == nil {
			continue
		}
		options := mqtt.NewClientOptions().AddBroker(url).SetClientID(g.Username).SetUsername(g.Username).SetPassword(g.Password).
			SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}).SetAutoReconnect(true).SetConnectRetry(true).SetConnectTimeout(10 * time.Second).
			SetOrderMatters(false)
		client := mqtt.NewClient(options)
		t := client.Connect()
		if !t.WaitTimeout(30*time.Second) || t.Error() != nil {
			link.Close()
			return nil, fmt.Errorf("broker connection for %s failed: %v (the MQTT provisioner applies a new account within a minute; retry)", g.ID, t.Error())
		}
		link.clients = append(link.clients, client)
		// Commands from Aether: the coordinator owns its whole tree, the Edge reads only its devices' /set topics.
		filter := zigbee2mqtt.BaseTopic(g.ID) + "/+/set"
		if st.Edge != nil && g.ID == st.Edge.ID {
			filter = edge.Prefix + g.ID + "/+/set"
		}
		sub := client.Subscribe(filter, 1, func(c mqtt.Client, m mqtt.Message) {
			for _, reply := range bridge.Command(m.Topic(), m.Payload(), time.Now()) {
				c.Publish(reply.Topic, 1, reply.Retain, reply.Payload)
			}
		})
		if !sub.WaitTimeout(10*time.Second) || sub.Error() != nil {
			link.Close()
			return nil, fmt.Errorf("command subscription for %s failed", g.ID)
		}
	}
	return link, nil
}

func (l *MQTTLink) clientFor(topic string) mqtt.Client {
	for _, c := range l.clients {
		r := c.OptionsReader()
		id := strings.TrimPrefix(r.Username(), "gw-")
		if strings.Contains(topic, "/"+id+"/") || strings.HasSuffix(topic, "/"+id) {
			return c
		}
	}
	return nil
}

// Publish sends messages with QoS 1, each through the account of the gateway whose tree it belongs to.
func (l *MQTTLink) Publish(messages []Message) error {
	var errs []error
	for _, m := range messages {
		c := l.clientFor(m.Topic)
		if c == nil {
			errs = append(errs, fmt.Errorf("no MQTT account for %s", m.Topic))
			continue
		}
		t := c.Publish(m.Topic, 1, m.Retain, m.Payload)
		if !t.WaitTimeout(10*time.Second) || t.Error() != nil {
			errs = append(errs, fmt.Errorf("publish %s: %v", m.Topic, t.Error()))
		}
	}
	return errors.Join(errs...)
}

func (l *MQTTLink) Close() {
	for _, c := range l.clients {
		c.Disconnect(500)
	}
}

// sameValues is used by tests.
func sameValues(a, b map[string]any) bool { return reflect.DeepEqual(a, b) }
