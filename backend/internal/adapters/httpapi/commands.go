package httpapi

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

// commandRoutes is the downlink: setting one property of a registered Zigbee2MQTT device. A POST only queues
// the command (202); mqtt-commander publishes it and the device's own next state report confirms it, so the UI
// shows pending until then and never assumes success. The access module is "control".
func commandRoutes(r fiber.Router, s *app.Service) {
	// Per member, on top of the per-IP limit: a stuck button or a script cannot flood the workspace's switches.
	perUser := limiter.New(limiter.Config{Max: 20, Expiration: time.Minute,
		KeyGenerator: func(c fiber.Ctx) string { return "command:" + c.Locals("principal").(domain.Principal).UserID },
		LimitReached: func(c fiber.Ctx) error { return domain.ErrRateLimited }})
	r.Post("/commands", perUser, func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		// {"device_id":…,"property":"brightness","value":128}, or {"device_id":…,"property":"state_l1","action":"toggle"}.
		var in struct {
			DeviceID string          `json:"device_id"`
			Property string          `json:"property"`
			Action   string          `json:"action"`
			Value    json.RawMessage `json:"value"`
		}
		if e := body(c, &in, 2048); e != nil {
			return e
		}
		if in.Action == "" {
			in.Action = "set"
		}
		// The client generates the id per click; retrying the same request returns the same command.
		key := strings.ToLower(strings.TrimSpace(c.Get("Idempotency-Key")))
		cmd, _, e := s.SendCommand(c.Context(), p, domain.CommandRequest{ID: key, DeviceID: in.DeviceID, Property: in.Property, Action: in.Action, Value: in.Value})
		if e != nil {
			return e
		}
		return c.Status(202).JSON(cmd)
	})
	r.Get("/commands", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		device := c.Query("device_id")
		if device != "" && !security.ValidID(device) {
			return domain.ErrInvalid
		}
		limit, _ := strconv.Atoi(c.Query("limit", "50"))
		out, e := s.Repo.ListCommands(c.Context(), p, device, limit)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	// What a device can be set to, rendered by the UI as controls: its settable features straight from the
	// Zigbee2MQTT definition, the last reported values, and whether this member may command it at all.
	r.Get("/devices/:id/controls", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.DeviceControls(c.Context(), p, c.Params("id"))
		if e != nil {
			return e
		}
		access, e := s.Repo.MemberAccess(c.Context(), p, p.UserID)
		if e != nil {
			return e
		}
		allowed := p.MayControl(access)
		return c.JSON(fiber.Map{"device_id": out.DeviceID, "gateway_id": out.GatewayID, "ieee": out.IEEE, "online": out.Online,
			"state": out.State, "features": zigbee2mqtt.SettableFeatures(out.Exposes), "can_command": allowed})
	})
	r.Get("/commands/:id", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.GetCommand(c.Context(), p, c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
}
