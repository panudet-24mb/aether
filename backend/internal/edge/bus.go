package edge

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Bus is what the agent needs from the broker. The paho implementation is below; tests use a fake.
type Bus interface {
	Publish(topic string, retained bool, payload []byte) error
	// Subscribe registers the command handler; it is (re)applied on every connection.
	Subscribe(filter string, handler func(topic string, payload []byte))
	Connected() bool
	Close()
}

// pahoBus is the Bus over the Aether broker: TLS with the site's CA, client id gw-<gateway>, and a retained last will
// that marks the agent offline if the connection drops without a goodbye.
type pahoBus struct {
	client mqtt.Client

	mu     sync.Mutex
	subs   map[string]func(string, []byte)
	online func()
}

// DialBus connects to the broker. onConnect runs after every (re)connection, once subscriptions are restored.
func DialBus(c Config, onConnect func()) (Bus, error) {
	broker, e := brokerURL(c.MQTTURL)
	if e != nil {
		return nil, e
	}
	b := &pahoBus{subs: map[string]func(string, []byte){}, online: onConnect}
	opts := mqtt.NewClientOptions().AddBroker(broker).SetClientID("gw-"+c.GatewayID).
		SetUsername(c.MQTTUsername).SetPassword(c.MQTTPassword).
		SetCleanSession(true).SetAutoReconnect(true).SetConnectRetry(true).
		SetConnectTimeout(10*time.Second).SetKeepAlive(60*time.Second).SetMaxReconnectInterval(time.Minute).
		SetWill(topicStatus(c.GatewayID), string(encodeAvailability(false, "")), 1, true).
		SetOnConnectHandler(b.connected)
	if c.MQTTCAFile != "" {
		ca, e := os.ReadFile(c.MQTTCAFile)
		if e != nil {
			return nil, errors.New("cannot read MQTT_CA_FILE")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("MQTT_CA_FILE holds no certificate")
		}
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots})
	}
	b.client = mqtt.NewClient(opts)
	b.client.Connect() // with ConnectRetry the token completes only once connected; the agent does not wait for it
	return b, nil
}

func (b *pahoBus) connected(c mqtt.Client) {
	b.mu.Lock()
	subs := make(map[string]func(string, []byte), len(b.subs))
	for k, v := range b.subs {
		subs[k] = v
	}
	b.mu.Unlock()
	for filter, h := range subs {
		h := h
		c.Subscribe(filter, 1, func(_ mqtt.Client, m mqtt.Message) { h(m.Topic(), m.Payload()) })
	}
	if b.online != nil {
		go b.online()
	}
}

func (b *pahoBus) Publish(topic string, retained bool, payload []byte) error {
	t := b.client.Publish(topic, 1, retained, payload)
	if !t.WaitTimeout(10 * time.Second) {
		return errors.New("edge: publish timed out")
	}
	return t.Error()
}

func (b *pahoBus) Subscribe(filter string, handler func(string, []byte)) {
	b.mu.Lock()
	b.subs[filter] = handler
	b.mu.Unlock()
	if b.client.IsConnectionOpen() {
		b.client.Subscribe(filter, 1, func(_ mqtt.Client, m mqtt.Message) { handler(m.Topic(), m.Payload()) })
	}
}

func (b *pahoBus) Connected() bool { return b.client.IsConnectionOpen() }

func (b *pahoBus) Close() { b.client.Disconnect(2000) }
