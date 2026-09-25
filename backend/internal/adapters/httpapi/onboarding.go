package httpapi

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/app"
	"aether/backend/internal/config"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"github.com/gofiber/fiber/v3"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func onboardingRoutes(r fiber.Router, s *app.Service, cfg config.Config) {
	settings := func() (fiber.Map, error) {
		host := os.Getenv("MQTT_PUBLIC_HOST")
		port, e := strconv.Atoi(os.Getenv("MQTT_PUBLIC_PORT"))
		scheme := os.Getenv("MQTT_PUBLIC_SCHEME")
		if host == "" || strings.ContainsAny(host, "/ @\r\n") || port < 1 || port > 65535 || e != nil || (scheme != "ssl" && scheme != "tcp") || (cfg.Environment != "development" && cfg.Environment != "test" && scheme != "ssl") {
			return nil, fiber.ErrServiceUnavailable
		}
		if strings.Contains(host, ":") && net.ParseIP(host) == nil {
			return nil, fiber.ErrServiceUnavailable
		}
		out := fiber.Map{"host": host, "port": port, "scheme": scheme, "tls": scheme == "ssl", "qos": 1, "keep_alive": 120}
		// Gateways that cannot do TLS: the operator must opt in explicitly (MQTT_ALLOW_PLAINTEXT=true). The
		// broker still demands the per-gateway password and ACL, but both travel unencrypted on the LAN.
		if os.Getenv("MQTT_ALLOW_PLAINTEXT") == "true" {
			plain, e := strconv.Atoi(os.Getenv("MQTT_PLAIN_PORT"))
			if e != nil || plain < 1 || plain > 65535 || plain == port {
				return nil, fiber.ErrServiceUnavailable
			}
			out["plaintext"] = fiber.Map{"port": plain, "scheme": "tcp"}
		}
		return out, nil
	}
	principal := func(c fiber.Ctx) (domain.Principal, error) {
		p := c.Locals("principal").(domain.Principal)
		if !p.CanManageDevices() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	r.Get("/mqtt/setup", func(c fiber.Ctx) error {
		if _, e := principal(c); e != nil {
			return e
		}
		out, e := settings()
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	r.Get("/gateways/mqtt-status", func(c fiber.Ctx) error {
		p, e := principal(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.MQTTStates(c.Context(), p)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "server_time": time.Now().UTC()})
	})
	issue := func(rotate bool) fiber.Handler {
		return func(c fiber.Ctx) error {
			p, e := principal(c)
			if e != nil {
				return e
			}
			id := c.Params("id")
			if !security.ValidID(id) {
				return domain.ErrInvalid
			}
			out, e := settings()
			if e != nil {
				return e
			}
			// Looked up by id (not through the capped gateway list) and before a password is issued.
			model, e := s.Repo.GatewayModel(c.Context(), p, id)
			if e != nil {
				return e
			}
			password := security.RandomToken()
			hash, e := security.MQTTHash(password)
			if e != nil {
				return e
			}
			if e = s.Repo.EnrollMQTT(c.Context(), p, id, hash, rotate); e != nil {
				return e
			}
			out["gateway_id"] = id
			out["username"] = "gw-" + id
			out["password"] = password
			out["client_id"] = "gw-" + id
			out["credential_display"] = "shown_once"
			if model == domain.Z2MGatewayModel {
				// Zigbee2MQTT publishes a whole tree under its base_topic; hand over the ready-made configuration.
				tls := out["tls"] == true
				host, port := out["host"].(string), out["port"].(int)
				out["base_topic"] = zigbee2mqtt.BaseTopic(id)
				out["server"] = zigbee2mqtt.Server(tls, host, port)
				out["z2m_yaml"] = zigbee2mqtt.ConfigSnippet(tls, host, port, id, password)
				return c.Status(201).JSON(out)
			}
			root := "/aether/gateways/" + id
			out["post_topic"] = root + "/status"
			out["subscribe_topic"] = root + "/action"
			out["reply_topic"] = root + "/response"
			return c.Status(201).JSON(out)
		}
	}
	r.Post("/gateways/:id/mqtt", issue(false))
	r.Post("/gateways/:id/mqtt/rotate", issue(true))
}
