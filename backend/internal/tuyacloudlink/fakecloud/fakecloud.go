// Package fakecloud plays Tuya Cloud in tests: a Message Service (Pulsar's WebSocket consumer API over TLS) that
// checks the consumer's credentials, pushes encrypted messages and records acknowledgements, and an OpenAPI that
// lists devices, serves their specifications and properties, and records issued properties. Nothing here talks
// to Tuya; only tests import it.
package fakecloud

import (
	"aether/backend/internal/adapters/tuyacloud"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// MQ is a fake Tuya Message Service for one project.
type MQ struct {
	Server   *httptest.Server
	AccessID string
	Secret   string
	Channel  string

	mu      sync.Mutex
	reject  int
	dials   int
	headers []http.Header
	acks    []string
	nacks   []string
	frames  map[string]frame // by message id, for redelivery on a nack
	outbox  chan []byte
	conns   []*websocket.Conn
	changed chan struct{}
}

type frame struct {
	body       map[string]any
	raw        []byte // a frame pushed as given (PushRawTracked): redelivered unchanged
	redelivery int
	sent       bool
}

// NewMQ starts the fake Message Service; it stops with the test.
func NewMQ(t testing.TB, accessID, secret string) *MQ {
	m := &MQ{AccessID: accessID, Secret: secret, Channel: tuyacloud.ChannelProd, frames: map[string]frame{}, outbox: make(chan []byte, 1024), changed: make(chan struct{}, 1)}
	upgrader := websocket.Upgrader{}
	m.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.dials++
		m.headers = append(m.headers, r.Header.Clone())
		reject := m.reject
		m.mu.Unlock()
		m.signal()
		want := "/ws/v2/consumer/persistent/" + accessID + "/out/" + m.Channel + "/" + accessID + "-sub"
		if r.URL.Path != want || r.URL.Query().Get("subscriptionType") != "Failover" {
			http.NotFound(w, r)
			return
		}
		if reject != 0 {
			w.WriteHeader(reject)
			return
		}
		if r.Header.Get("username") != accessID || r.Header.Get("password") != tuyacloud.Password(accessID, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, e := upgrader.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		// Like Pulsar, a new consumer connection first gets every message delivered before and never acknowledged,
		// with its redelivery count raised.
		m.mu.Lock()
		m.conns = append(m.conns, conn)
		var again [][]byte
		for id, f := range m.frames {
			if f.sent {
				if f.raw != nil {
					again = append(again, f.raw)
					continue
				}
				f.redelivery++
				f.body["redeliveryCount"] = f.redelivery
				m.frames[id] = f
				b, _ := json.Marshal(f.body)
				again = append(again, b)
			}
		}
		m.mu.Unlock()
		for _, b := range again {
			if conn.WriteMessage(websocket.TextMessage, b) != nil {
				return
			}
		}
		done := make(chan struct{})
		go m.read(conn, done)
		for {
			select {
			case <-done:
				return
			case b := <-m.outbox:
				m.markSent(b)
				if conn.WriteMessage(websocket.TextMessage, b) != nil {
					return // redelivered on the next connection
				}
			}
		}
	}))
	t.Cleanup(m.Server.Close)
	return m
}

func (m *MQ) markSent(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, f := range m.frames {
		if f.raw != nil && len(f.raw) == len(b) && string(f.raw) == string(b) {
			f.sent = true
			m.frames[id] = f
			return
		}
	}
	var head struct {
		MessageID string `json:"messageId"`
	}
	if len(b) > 1<<20 || json.Unmarshal(b, &head) != nil {
		return
	}
	if f, ok := m.frames[head.MessageID]; ok {
		f.sent = true
		m.frames[head.MessageID] = f
	}
}

func (m *MQ) signal() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

func (m *MQ) read(conn *websocket.Conn, done chan struct{}) {
	defer close(done)
	for {
		_, b, e := conn.ReadMessage()
		if e != nil {
			return
		}
		var msg struct {
			Type      string `json:"type"`
			MessageID string `json:"messageId"`
		}
		if json.Unmarshal(b, &msg) != nil {
			continue
		}
		m.mu.Lock()
		if msg.Type == "negativeAcknowledge" {
			// Redelivered later (Pulsar's negative-ack delay); here: on the next connection.
			m.nacks = append(m.nacks, msg.MessageID)
		} else {
			m.acks = append(m.acks, msg.MessageID)
			delete(m.frames, msg.MessageID)
		}
		m.mu.Unlock()
		m.signal()
	}
}

// URL replaces tuyacloud.ConsumerURL: the real URL's path on the fake server.
func (m *MQ) URL(region, accessID, channel string) (string, error) {
	real, e := tuyacloud.ConsumerURL(region, accessID, channel)
	if e != nil {
		return "", e
	}
	u, _ := url.Parse(real)
	return "wss://" + strings.TrimPrefix(m.Server.URL, "https://") + u.RequestURI(), nil
}

// TLS trusts the fake server's certificate (and nothing is ever unverified).
func (m *MQ) TLS() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(m.Server.Certificate())
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// Reject makes every following handshake answer with status (0 accepts again).
func (m *MQ) Reject(status int) {
	m.mu.Lock()
	m.reject = status
	m.mu.Unlock()
}

// Drop closes every open consumer connection (a network cut).
func (m *MQ) Drop() {
	m.mu.Lock()
	for _, c := range m.conns {
		c.Close()
	}
	m.conns = nil
	m.mu.Unlock()
}

// Dials counts handshakes; Headers are the handshake headers seen.
func (m *MQ) Dials() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dials
}

func (m *MQ) Headers() []http.Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]http.Header(nil), m.headers...)
}

// Acked and Nacked report the message ids acknowledged / negatively acknowledged so far.
func (m *MQ) Acked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.acks...)
}

func (m *MQ) Nacked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.nacks...)
}

// WaitFor polls cond until it holds or the timeout passes.
func (m *MQ) WaitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-m.changed:
		case <-time.After(20 * time.Millisecond):
		}
	}
	return cond()
}

// WaitAck waits until id was acknowledged.
func (m *MQ) WaitAck(id string, timeout time.Duration) bool {
	return m.WaitFor(timeout, func() bool {
		for _, a := range m.Acked() {
			if a == id {
				return true
			}
		}
		return false
	})
}

// Message is one business message to push.
type Message struct {
	ID         string
	Protocol   int
	Plaintext  string
	GCM        bool // encrypt with AES-GCM and mark "em": "aes_gcm"; otherwise AES-ECB
	Redelivery int
	Corrupt    bool // flip a ciphertext byte
}

// Push encrypts and sends one message (queued until a consumer is connected).
func (m *MQ) Push(msg Message) {
	body := m.Frame(msg)
	m.mu.Lock()
	m.frames[msg.ID] = frame{body: body, redelivery: msg.Redelivery}
	m.mu.Unlock()
	b, _ := json.Marshal(body)
	m.outbox <- b
}

// PushRaw sends a frame exactly as given (malformed or oversize frames), once.
func (m *MQ) PushRaw(b []byte) { m.outbox <- b }

// PushRawTracked sends a frame as given and, like Pulsar, delivers it again on every new connection until the
// message id is acknowledged.
func (m *MQ) PushRawTracked(id string, b []byte) {
	m.mu.Lock()
	m.frames[id] = frame{raw: b}
	m.mu.Unlock()
	m.outbox <- b
}

// Frame builds the Pulsar frame of a message: the envelope {data, protocol, pv, sign, t} base64 in payload.
func (m *MQ) Frame(msg Message) map[string]any {
	key := []byte(m.Secret[8:24])
	var data []byte
	props := map[string]string{}
	if msg.GCM {
		data = gcmSeal(key, []byte(msg.Plaintext))
		props["em"] = "aes_gcm"
	} else {
		data = ecbSeal(key, []byte(msg.Plaintext))
	}
	if msg.Corrupt {
		data[len(data)-1] ^= 1
	}
	env, _ := json.Marshal(map[string]any{"data": base64.StdEncoding.EncodeToString(data), "protocol": msg.Protocol, "pv": "2.0", "sign": "unchecked", "t": time.Now().UnixMilli()})
	return map[string]any{"messageId": msg.ID, "payload": base64.StdEncoding.EncodeToString(env), "properties": props,
		"publishTime": time.Now().UTC().Format(time.RFC3339), "redeliveryCount": msg.Redelivery}
}

func ecbSeal(key, plain []byte) []byte {
	block, _ := aes.NewCipher(key)
	n := 16 - len(plain)%16
	padded := append(append([]byte(nil), plain...), []byte(strings.Repeat(string(rune(n)), n))...)
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += 16 {
		block.Encrypt(out[i:i+16], padded[i:i+16])
	}
	return out
}

func gcmSeal(key, plain []byte) []byte {
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCMWithNonceSize(block, 12)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	return gcm.Seal(nonce, nonce, plain, nil)
}

// Status is a protocol 4 status report of one device: code -> value, all at time t (ms).
func Status(devID string, t int64, values map[string]any) string {
	status := []map[string]any{}
	for code, v := range values {
		status = append(status, map[string]any{"code": code, "value": v, "t": t})
	}
	b, _ := json.Marshal(map[string]any{"dataId": "d" + devID, "devId": devID, "productKey": "pfake", "status": status})
	return string(b)
}

// Device is a protocol 20 device event (bizCode online, offline, delete, bindUser, nameUpdate).
func Device(devID, bizCode string, ts int64, name string) string {
	biz := map[string]any{}
	if name != "" {
		biz["name"] = name
	}
	b, _ := json.Marshal(map[string]any{"devId": devID, "productKey": "pfake", "bizCode": bizCode, "bizData": biz, "ts": ts})
	return string(b)
}
