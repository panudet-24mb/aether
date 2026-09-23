package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/config"
	"aether/backend/internal/domain"
	"aether/backend/internal/realtime"
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

const maxPendingSockets = 200

// realtimeRoutes mounts GET /ws. Browsers cannot send an Authorization header on a WebSocket and tokens must
// not travel in URLs, so the client authenticates with its first message. The socket only ever carries
// "refetch" signals for the authenticated tenant; data stays behind the REST API.
func realtimeRoutes(api *fiber.App, s *app.Service, cfg config.Config, hub *realtime.Hub) {
	// /ws sits outside the /api limiter, and a socket costs a goroutine before it has authenticated:
	// cap upgrade attempts per IP and the number of sockets still waiting for their auth message.
	var pending atomic.Int64
	api.Get("/ws", limiter.New(limiter.Config{Max: 30, Expiration: time.Minute, LimitReached: func(c fiber.Ctx) error { return fiber.ErrTooManyRequests }}), func(c fiber.Ctx) error {
		if !websocket.IsWebSocketUpgrade(c) {
			return fiber.ErrUpgradeRequired
		}
		if pending.Load() >= maxPendingSockets {
			return fiber.ErrServiceUnavailable
		}
		return c.Next()
	}, websocket.New(func(conn *websocket.Conn) {
		defer conn.Close()
		pending.Add(1)
		authenticated := false
		release := func() {
			if !authenticated {
				authenticated = true
				pending.Add(-1)
			}
		}
		defer release()
		conn.SetReadLimit(4096)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var hello struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}
		_, raw, e := conn.ReadMessage()
		if e != nil || json.Unmarshal(raw, &hello) != nil || hello.Type != "auth" || len(hello.Token) > 4096 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		principal, e := s.Authenticate(ctx, hello.Token)
		cancel()
		if e != nil {
			_ = conn.WriteJSON(fiber.Map{"type": "error", "error": "unauthorized"})
			return
		}
		// A session that still owes a password change may not hold a stream open either.
		if principal.MustChangePassword {
			_ = conn.WriteJSON(fiber.Map{"type": "error", "error": "forbidden", "detail": domain.PasswordChangeRequired})
			return
		}
		// The signal stream is tenant-wide, so a member who only sees some projects must not learn the
		// gateway a signal came from. The scope is read once, here, and the hub blanks gateway_id for them.
		scopeCtx, scopeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		self, e := s.Repo.MemberSelf(scopeCtx, principal)
		scopeCancel()
		if e != nil {
			_ = conn.WriteJSON(fiber.Map{"type": "error", "error": "unauthorized"})
			return
		}
		release()
		client := hub.RegisterScoped(principal.TenantID, self.ProjectIDs != nil)
		if client == nil {
			_ = conn.WriteJSON(fiber.Map{"type": "error", "error": "too_many_connections"})
			return
		}
		defer hub.Unregister(client)
		if conn.WriteJSON(realtime.Message{Type: "ready"}) != nil {
			return
		}
		// Reader: keeps the connection alive (client pings) and notices when the peer goes away.
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
				if _, _, e := conn.ReadMessage(); e != nil {
					return
				}
			}
		}()
		recheck := time.NewTicker(60 * time.Second)
		defer recheck.Stop()
		for {
			select {
			case <-closed:
				return
			case msg, ok := <-client.Send:
				if !ok {
					return
				}
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if conn.WriteJSON(msg) != nil {
					return
				}
			case <-recheck.C:
				// Logout, role removal or tenant suspension must end the stream, not just future REST calls.
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				fresh, e := s.Repo.Authorize(ctx, domain.Principal{UserID: principal.UserID, TenantID: principal.TenantID, SessionID: principal.SessionID, Role: principal.Role})
				cancel()
				if e != nil || fresh.MustChangePassword {
					return
				}
			}
		}
	}, websocket.Config{Origins: []string{cfg.Origin}, ReadBufferSize: 1024, WriteBufferSize: 1024}))
}
