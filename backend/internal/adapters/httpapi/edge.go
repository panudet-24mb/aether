package httpapi

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/app"
	"aether/backend/internal/config"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

// edgeRoutes are the workspace-facing Aether Edge routes: the one-time Tuya key import, the imported devices, and
// install codes. All are owner/admin (CanManageDevices); mutations also pass the "connect" access module.
func edgeRoutes(r fiber.Router, s *app.Service, cfg config.Config) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	r.Post("/gateways/:id/tuya/imports", func(c fiber.Ctx) error {
		var in struct {
			Region       string `json:"region"`
			AccessID     string `json:"access_id"`
			AccessSecret string `json:"access_secret"`
		}
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		job, e := s.StartTuyaImport(c.Context(), principal(c), c.Params("id"), in.Region, in.AccessID, in.AccessSecret)
		if e != nil {
			return e
		}
		return c.Status(202).JSON(job)
	})
	r.Get("/gateways/:id/tuya/imports/:job", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) || !security.ValidID(c.Params("job")) {
			return domain.ErrInvalid
		}
		job, e := s.TuyaImportStatus(principal(c), c.Params("id"), c.Params("job"))
		if e != nil {
			return e
		}
		return c.JSON(job)
	})
	r.Get("/gateways/:id/tuya/devices", func(c fiber.Ctx) error {
		out, e := s.TuyaDevices(c.Context(), principal(c), c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	r.Post("/gateways/:id/tuya/devices/:tuya_id/forget", func(c fiber.Ctx) error {
		if e := s.ForgetTuyaKey(c.Context(), principal(c), c.Params("id"), c.Params("tuya_id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	r.Post("/gateways/:id/edge/install-code", func(c fiber.Ctx) error {
		code, expires, e := s.CreateEdgeInstallCode(c.Context(), principal(c), c.Params("id"))
		if e != nil {
			return e
		}
		return c.Status(201).JSON(fiber.Map{
			"code": code, "expires_at": expires, "credential_display": "shown_once",
			"bootstrap_url": cfg.Origin + "/edge/bootstrap",
			// install.sh ships with the Edge image (phase E); it POSTs the code to bootstrap_url.
			"install_command": fmt.Sprintf("curl -fsSL %s/edge/install.sh | sh -s -- %s", cfg.Origin, code),
		})
	})
}

// readPEM reads a certificate file handed to the installer. Only CERTIFICATE blocks that parse as X.509 are
// returned, re-encoded: a bundle that also holds a private key (or anything else) never passes the key on.
func readPEM(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	b, e := os.ReadFile(path)
	if e != nil || len(b) > 64*1024 {
		return "", false
	}
	var out []byte
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		if _, e := x509.ParseCertificate(block.Bytes); e != nil {
			continue
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})...)
	}
	if len(out) == 0 {
		return "", false
	}
	return string(out), true
}

// edgePublicRoutes is the installer's bootstrap: an install code redeemed once for the agent's credentials. It is
// public (the installer has nothing else yet), so it has its own tight per-address limit, and every failure looks
// the same.
func edgePublicRoutes(api *fiber.App, s *app.Service, cfg config.Config) {
	limit := limiter.New(limiter.Config{Max: 10, Expiration: time.Minute, LimitReached: func(c fiber.Ctx) error { return fiber.ErrTooManyRequests }})
	// The code travels in the body, never the path: proxies and CDNs log paths, and a code that failed before it
	// was spent (a 503, a 429) must not sit valid in someone's access log.
	api.Post("/edge/bootstrap", limit, func(c fiber.Ctx) error {
		var in struct {
			Code string `json:"code"`
		}
		if body(c, &in, 256) != nil {
			return domain.ErrUnauthorized // every failure looks the same
		}
		// Everything the bundle needs is checked before the code is spent: a deployment that cannot hand over
		// a broker endpoint and its CA must not burn the user's code.
		mqtt, e := mqttSettings(cfg)
		if e != nil {
			return e
		}
		tls := mqtt["tls"] == true
		ca, haveCA := readPEM(os.Getenv("MQTT_CA_FILE"))
		if tls && !haveCA {
			return fiber.ErrServiceUnavailable
		}
		creds, e := s.BootstrapEdge(c.Context(), in.Code)
		if e != nil {
			return e
		}
		host, port := mqtt["host"].(string), mqtt["port"].(int)
		scheme := "mqtt"
		if tls {
			scheme = "mqtts"
		}
		user := "gw-" + creds.GatewayID
		out := fiber.Map{
			"gateway_id": creds.GatewayID, "api_origin": cfg.Origin, "credential_display": "shown_once",
			"http": fiber.Map{"token": creds.HTTPToken, "config_url": cfg.Origin + "/ingest/gateways/" + creds.GatewayID + "/edge/config"},
			"mqtt": fiber.Map{"url": scheme + "://" + host + ":" + strconv.Itoa(port), "host": host, "port": port, "tls": tls,
				"username": user, "client_id": user, "password": creds.MQTTPassword, "base_topic": strings.TrimSuffix(edge.Prefix, "/") + "/" + creds.GatewayID},
			"image": fiber.Map{"repository": domain.EdgeImage, "tag": domain.EdgeImageTag},
			// The broker keeps a session opened with the previous password until it ends. Connecting with the same
			// client id takes that session over; stop any earlier agent of this gateway (docs/platform/aether-edge.md).
			"notice": "previous_agent_must_stop",
		}
		if haveCA {
			out["ca_pem"] = ca
		}
		// When the web front uses a private CA (AETHER_TLS=internal), the operator exports its root once and sets
		// EDGE_WEB_CA_FILE (docs/production.md): the agent needs it to fetch its configuration over HTTPS.
		if web, ok := readPEM(os.Getenv("EDGE_WEB_CA_FILE")); ok {
			out["web_ca_pem"] = web
		}
		return c.Status(201).JSON(out)
	})
}

// edgeAgentRoutes is the agent's own configuration pull, over its gateway's HTTP Basic credential (the ingest
// group). It is the only response that carries local keys, and only the gateway's own registered devices'.
func edgeAgentRoutes(ingest fiber.Router, s *app.Service) {
	ingest.Get("/edge/config", func(c fiber.Ctx) error {
		revision, devices, e := s.EdgeConfig(c.Context(), c.Locals("gateway_tenant").(string), c.Params("id"))
		if e != nil {
			return e
		}
		etag := `"` + strconv.FormatInt(revision, 10) + `"`
		c.Set("ETag", etag)
		if c.Get("If-None-Match") == etag {
			return c.SendStatus(304)
		}
		return c.JSON(fiber.Map{"revision": revision, "devices": devices})
	})
}
