// Package realtime fans "something changed" signals out to the WebSocket clients of one tenant.
// Signals never carry data: clients refetch through the normal authenticated REST API, so tenant
// isolation and authorization stay in one place.
package realtime

import (
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Message is what a client receives.
type Message struct {
	Type      string `json:"type"` // ready | signal
	Kind      string `json:"kind,omitempty"`
	GatewayID string `json:"gateway_id,omitempty"`
}

type Client struct {
	Tenant string
	Send   chan Message
	// Scoped marks a socket whose member only sees some of the workspace's projects. Signals still reach
	// it (the client simply refetches, and the REST API narrows the answer), but without the gateway id:
	// otherwise the stream would report activity in a project the member may not see.
	Scoped bool
}

type Hub struct {
	mu      sync.RWMutex
	clients map[string]map[*Client]struct{}
}

func NewHub() *Hub { return &Hub{clients: map[string]map[*Client]struct{}{}} }

const maxClientsPerTenant = 50

// Register returns nil when the tenant already has too many open sockets.
func (h *Hub) Register(tenant string) *Client { return h.RegisterScoped(tenant, false) }

// RegisterScoped is Register for a member who only sees some projects; their signals carry no gateway id.
func (h *Hub) RegisterScoped(tenant string, scoped bool) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients[tenant]) >= maxClientsPerTenant {
		return nil
	}
	c := &Client{Tenant: tenant, Send: make(chan Message, 16), Scoped: scoped}
	if h.clients[tenant] == nil {
		h.clients[tenant] = map[*Client]struct{}{}
	}
	h.clients[tenant][c] = struct{}{}
	return c
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.clients[c.Tenant]; set != nil {
		if _, ok := set[c]; ok {
			delete(set, c)
			close(c.Send)
		}
		if len(set) == 0 {
			delete(h.clients, c.Tenant)
		}
	}
}

// Broadcast never blocks: a client that cannot keep up simply misses a signal and catches up on its next poll.
func (h *Hub) Broadcast(s domain.Signal) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients[s.Tenant] {
		gateway := s.Gateway
		if c.Scoped {
			gateway = ""
		}
		select {
		case c.Send <- Message{Type: "signal", Kind: s.Kind, GatewayID: gateway}:
		default:
		}
	}
}

func (h *Hub) Count(tenant string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients[tenant])
}

// Listen holds one dedicated connection on the notification channel and reconnects with backoff.
func (h *Hub) Listen(ctx context.Context, dsn, channel string) {
	backoff := time.Second
	for ctx.Err() == nil {
		if e := h.listenOnce(ctx, dsn, channel); e != nil && ctx.Err() == nil {
			slog.Warn("realtime listener disconnected; retrying", "in", backoff.String())
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (h *Hub) listenOnce(ctx context.Context, dsn, channel string) error {
	conn, e := pgx.Connect(ctx, dsn)
	if e != nil {
		return e
	}
	defer conn.Close(context.Background())
	if _, e = conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); e != nil {
		return e
	}
	slog.Info("realtime listener ready")
	for {
		n, e := conn.WaitForNotification(ctx)
		if e != nil {
			return e
		}
		var s domain.Signal
		if json.Unmarshal([]byte(n.Payload), &s) != nil || s.Tenant == "" || s.Kind == "" {
			continue
		}
		h.Broadcast(s)
	}
}
