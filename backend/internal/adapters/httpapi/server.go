package httpapi

import (
	"aether/backend/internal/alerts"
	"aether/backend/internal/realtime"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"strconv"
	"strings"
	"time"

	spec "aether/backend/api"
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/app"
	"aether/backend/internal/config"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/z2mcatalog"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
)

type readiness interface{ Ready(context.Context) error }

func proxyHeader(cfg config.Config) string {
	if len(cfg.TrustedProxies) > 0 {
		return fiber.HeaderXForwardedFor
	}
	return ""
}

func rateLimit(cfg config.Config) int {
	if cfg.APIRateLimit > 0 {
		return cfg.APIRateLimit
	}
	return 120 // tests and callers that build Config by hand keep the original budget
}

// NewSender builds the notification sender from deployment settings (shared by the API and the worker).
func NewSender(cfg config.Config) *alerts.Sender {
	return alerts.NewSender(alerts.SMTPSettings{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, From: cfg.SMTPFrom}, cfg.Environment, cfg.WebhookAllowedHosts...)
}

func New(cfg config.Config, service *app.Service, health readiness) *fiber.App {
	return NewWithHub(cfg, service, health, realtime.NewHub())
}

// NewWithHub lets the process share one realtime hub between the HTTP app and the database listener.
func NewWithHub(cfg config.Config, service *app.Service, health readiness, hub *realtime.Hub) *fiber.App {
	// Behind the reverse proxy every request comes from the proxy's address. Only when the peer is one of
	// TRUSTED_PROXIES is X-Forwarded-For believed (the proxy must overwrite, not append, that header), so the
	// per-IP rate limits apply to real clients and cannot be dodged by sending the header directly.
	trust := fiber.TrustProxyConfig{Proxies: cfg.TrustedProxies}
	api := fiber.New(fiber.Config{AppName: "Aether API", TrustProxy: len(cfg.TrustedProxies) > 0, TrustProxyConfig: trust, ProxyHeader: proxyHeader(cfg), EnableIPValidation: true, CaseSensitive: true, BodyLimit: 256 * 1024, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, Immutable: true, ErrorHandler: func(c fiber.Ctx, e error) error {
		code, message := 500, "internal_error"
		switch {
		case errors.Is(e, domain.ErrInvalid):
			code, message = 400, "invalid_input"
		case errors.Is(e, domain.ErrUnauthorized):
			code, message = 401, "unauthorized"
		case errors.Is(e, domain.ErrForbidden):
			code, message = 403, "forbidden"
		case errors.Is(e, domain.ErrNotFound):
			code, message = 404, "not_found"
		case errors.Is(e, domain.ErrConflict):
			code, message = 409, "conflict"
		case errors.Is(e, domain.ErrRateLimited):
			code, message = 429, "rate_limited"
		default:
			var f *fiber.Error
			if errors.As(e, &f) {
				code = f.Code
				message = "request_rejected"
			} else {
				slog.Error("request failed", "request_id", c.GetRespHeader("X-Request-ID"), "error_type", "internal")
			}
		}
		// A reason narrows a client error the caller can act on (e.g. a command refused because the device is "offline").
		var reason domain.ReasonError
		if code < 500 && errors.As(e, &reason) {
			message = reason.Reason
		}
		return c.Status(code).JSON(fiber.Map{"error": message, "request_id": c.GetRespHeader("X-Request-ID")})
	}})
	api.Use(recover.New())
	api.Use(requestid.New())
	api.Use(func(c fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Referrer-Policy", "no-referrer")
		c.Set("Cache-Control", "no-store")
		c.Set("X-Frame-Options", "DENY")
		if cfg.SecureCookies {
			c.Set("Strict-Transport-Security", "max-age=31536000")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
		defer cancel()
		c.SetContext(ctx)
		return c.Next()
	})
	api.Use(cors.New(cors.Config{AllowOrigins: []string{cfg.Origin}, AllowMethods: []string{"GET", "POST", "OPTIONS"}, AllowHeaders: []string{"Authorization", "Content-Type", "Idempotency-Key"}, AllowCredentials: true}))
	api.Get("/openapi.json", func(c fiber.Ctx) error { c.Type("json"); return c.Send(spec.OpenAPI) })
	api.Get("/health/live", func(c fiber.Ctx) error { return c.JSON(fiber.Map{"status": "ok"}) })
	api.Get("/health/ready", func(c fiber.Ctx) error {
		if e := health.Ready(c.Context()); e != nil {
			return c.Status(503).JSON(fiber.Map{"status": "unavailable"})
		}
		return c.JSON(fiber.Map{"status": "ready"})
	})
	// IP is the socket address unless the peer is a configured trusted proxy (see fiber.Config above).
	api.Use("/api", limiter.New(limiter.Config{Max: rateLimit(cfg), Expiration: time.Minute, LimitReached: func(c fiber.Ctx) error { return fiber.ErrTooManyRequests }}))
	auth := api.Group("/api/v1/auth", limiter.New(limiter.Config{Max: 10, Expiration: time.Minute, LimitReached: func(c fiber.Ctx) error { return fiber.ErrTooManyRequests }}), func(c fiber.Ctx) error {
		// All browser authentication mutations require the exact configured origin.
		// Explicit Origin is also required from scripts, avoiding ambient-cookie CSRF.
		if c.Get("Origin") != cfg.Origin {
			return domain.ErrForbidden
		}
		return c.Next()
	})
	cookieName := "aether_refresh_dev"
	if cfg.SecureCookies {
		cookieName = "__Host-aether_refresh"
	}
	setCookie := func(c fiber.Ctx, result app.AuthResult) {
		c.Cookie(&fiber.Cookie{Name: cookieName, Value: result.RefreshToken, Path: "/", HTTPOnly: true, Secure: cfg.SecureCookies, SameSite: "Strict", Expires: result.RefreshExpires, MaxAge: max(1, int(time.Until(result.RefreshExpires).Seconds()))})
	}
	clearCookie := func(c fiber.Ctx) {
		c.Cookie(&fiber.Cookie{Name: cookieName, Value: "", Path: "/", HTTPOnly: true, Secure: cfg.SecureCookies, SameSite: "Strict", Expires: time.Unix(1, 0), MaxAge: -1})
	}
	auth.Post("/register", func(c fiber.Ctx) error {
		var in struct {
			Email      string `json:"email"`
			Password   string `json:"password"`
			Name       string `json:"name"`
			TenantName string `json:"tenant_name"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		a, e := service.Register(c.Context(), in.Email, in.Password, in.Name, in.TenantName, false)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(a)
	})
	auth.Post("/login", func(c fiber.Ctx) error {
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			TenantID string `json:"tenant_id"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		result, e := service.Login(c.Context(), in.Email, in.Password, in.TenantID)
		if e != nil {
			return e
		}
		setCookie(c, result)
		return c.JSON(result)
	})
	auth.Post("/refresh", func(c fiber.Ctx) error {
		result, e := service.Refresh(c.Context(), c.Cookies(cookieName))
		if e != nil {
			clearCookie(c)
			return e
		}
		setCookie(c, result)
		return c.JSON(result)
	})
	authenticate := func(c fiber.Ctx) error {
		raw := c.Get("Authorization")
		if !strings.HasPrefix(raw, "Bearer ") || len(raw) > 4096 {
			return domain.ErrUnauthorized
		}
		p, e := service.Authenticate(c.Context(), strings.TrimPrefix(raw, "Bearer "))
		if e != nil {
			return e
		}
		// A password handed over in person is a shared secret until it is replaced. Until then the session
		// can only do the three things it needs to replace it; the detail tells the UI which screen to show.
		if p.MustChangePassword && !passwordChangeExempt(c.Path()) {
			return c.Status(403).JSON(fiber.Map{"error": "forbidden", "detail": domain.PasswordChangeRequired, "request_id": c.GetRespHeader("X-Request-ID")})
		}
		c.Locals("principal", p)
		return c.Next()
	}
	auth.Post("/logout", authenticate, func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if e := service.Repo.RevokeSession(c.Context(), p); e != nil {
			return e
		}
		clearCookie(c)
		return c.SendStatus(204)
	})
	// Changing one's own password is a credential mutation: same exact-Origin check and budget as login.
	auth.Post("/password", authenticate, passwordHandler(service))
	secured := api.Group("/api/v1", authenticate, accessGate(service))
	memberAccessRoutes(secured, service)
	studioRoutes(secured, service)
	discoveryRoutes(secured, service)
	onboardingRoutes(secured, service, cfg)
	alertRoutes(secured, service, NewSender(cfg))
	projectRoutes(secured, service)
	presenceRoutes(secured, service)
	signalRoutes(secured, service)
	memberRoutes(secured, service, cfg.Mode)
	automationRoutes(secured, service)
	floorplanRoutes(secured, service)
	assetRoutes(secured, service)
	commandRoutes(secured, service)
	realtimeRoutes(api, service, cfg, hub)
	secured.Get("/catalog", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"gateway_models": domain.GatewayModels, "device_profiles": domain.DeviceProfiles, "alerts_shadow": cfg.AlertsShadow, "verification": "verified=true means a captured packet from the physical device passes a golden test in this repository"})
	})
	// The Zigbee2MQTT device catalog (zigbee-herdsman-converters, MIT): "is this model supported, and what will
	// Aether show it as?" before anything is bought or paired. Read-only and the same for every workspace.
	secured.Get("/catalog/zigbee", func(c fiber.Ctx) error {
		catalog, e := z2mcatalog.Load()
		if e != nil {
			return e
		}
		q, vendor, category := c.Query("q"), c.Query("vendor"), c.Query("category")
		if len(q) > 100 || len(vendor) > 64 || len(category) > 32 {
			return fiber.NewError(fiber.StatusBadRequest, "query too long")
		}
		limit := 50
		if v := c.Query("limit"); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 200 {
				return fiber.NewError(fiber.StatusBadRequest, "limit must be 1..200")
			}
			limit = n
		}
		items, total := catalog.Search(q, vendor, category, limit)
		out := fiber.Map{"items": items, "total": total, "source": catalog.Source, "version": catalog.Version, "license": catalog.License, "homepage": catalog.Homepage,
			"notice": "Device list from zigbee-herdsman-converters " + catalog.Version + ", MIT License, Copyright (c) 2018 Koen Kanters"}
		if c.Query("vendors") == "1" {
			out["vendors"] = catalog.Vendors
		}
		return c.JSON(out)
	})
	// GET /api/v1/me is registered by memberRoutes: besides the identity it reports the caller's role and
	// the project scope the database enforces for them.

	secured.Get("/templates", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if !p.CanManageDevices() {
			return domain.ErrForbidden
		}
		out, e := service.Repo.ListTemplates(c.Context(), p)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	secured.Post("/templates", func(c fiber.Ctx) error {
		var in struct {
			Name       string                    `json:"name"`
			Version    int                       `json:"version"`
			DecoderID  string                    `json:"decoder_id"`
			Definition domain.TemplateDefinition `json:"definition"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		out, e := service.CreateTemplate(c.Context(), c.Locals("principal").(domain.Principal), domain.DeviceTemplate{Name: in.Name, Version: in.Version, DecoderID: in.DecoderID, Definition: in.Definition})
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})
	secured.Post("/gateways/:id/streams/:external/template", func(c fiber.Ctx) error {
		var in struct {
			Name       string `json:"name"`
			TemplateID string `json:"template_id"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		if e := service.AssignTemplate(c.Context(), c.Locals("principal").(domain.Principal), c.Params("id"), c.Params("external"), in.Name, in.TemplateID); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	// Every member may read live values: operators and viewers need the overview. What they see is narrowed
	// to their projects by the database (restrictive RLS policies), not by this handler.
	secured.Get("/live", func(c fiber.Ctx) error {
		principal := c.Locals("principal").(domain.Principal)
		gateways, e := service.Repo.ListGateways(c.Context(), principal)
		if e != nil {
			return e
		}
		window := 24 * time.Hour
		switch c.Query("range", "24h") {
		case "1h":
			window = time.Hour
		case "24h":
		case "7d":
			window = 7 * 24 * time.Hour
		default:
			return domain.ErrInvalid
		}
		views := []minew.View{}
		for _, g := range gateways {
			packets, e := service.Repo.ListPackets(c.Context(), principal, g.ID)
			if e != nil {
				return e
			}
			view := minew.Project(g, packets)
			history, e := service.Repo.StreamHistory(c.Context(), principal, g.ID, time.Now().Add(-window), 200)
			if e != nil {
				return e
			}
			view.Sensors = history
			views = append(views, view)
		}
		return c.JSON(fiber.Map{"gateways": views, "server_time": time.Now().UTC(), "deployment_mode": cfg.Mode, "history_scope": "persisted_latest_200_per_sensor_in_range", "range": c.Query("range", "24h")})
	})
	secured.Get("/gateways", func(c fiber.Ctx) error {
		out, e := service.Repo.ListGateways(c.Context(), c.Locals("principal").(domain.Principal))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	secured.Post("/gateways", func(c fiber.Ctx) error {
		var in struct {
			Name      string  `json:"name"`
			Model     string  `json:"model"`
			ProjectID *string `json:"project_id"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		g, token, e := service.CreateGatewayIn(c.Context(), c.Locals("principal").(domain.Principal), in.Name, in.Model, in.ProjectID)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(fiber.Map{"gateway": g, "token": token, "credential_display": "shown_once", "capture_path": "/ingest/gateways/" + g.ID + "/packets"})
	})
	secured.Post("/gateways/:id/revoke", func(c fiber.Ctx) error {
		if e := service.RevokeGateway(c.Context(), c.Locals("principal").(domain.Principal), c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	secured.Get("/gateways/:id/packets", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if !p.CanManageDevices() {
			return domain.ErrForbidden
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := service.Repo.ListPackets(c.Context(), p, c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "decoded": false})
	})
	secured.Post("/devices", func(c fiber.Ctx) error {
		var in struct {
			Name       string `json:"name"`
			GatewayID  string `json:"gateway_id"`
			ExternalID string `json:"external_id"`
			ProfileID  string `json:"profile_id"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		d, e := service.CreateDevice(c.Context(), c.Locals("principal").(domain.Principal), in.GatewayID, in.Name, in.ExternalID, in.ProfileID)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(d)
	})
	secured.Get("/devices", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if c.Query("removed") == "true" {
			if !p.CanManageDevices() {
				return domain.ErrForbidden
			}
			out, e := service.Repo.ListRemovedDevices(c.Context(), p)
			if e != nil {
				return e
			}
			return c.JSON(fiber.Map{"items": out})
		}
		out, e := service.Repo.ListDevices(c.Context(), p)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	secured.Post("/devices/:id/update", func(c fiber.Ctx) error {
		var in struct {
			Name      *string `json:"name"`
			GatewayID *string `json:"gateway_id"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		d, e := service.UpdateDevice(c.Context(), c.Locals("principal").(domain.Principal), c.Params("id"), in.Name, in.GatewayID)
		if e != nil {
			return e
		}
		return c.JSON(d)
	})
	secured.Post("/devices/:id/remove", func(c fiber.Ctx) error {
		if e := service.RemoveDevice(c.Context(), c.Locals("principal").(domain.Principal), c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	secured.Post("/devices/:id/restore", func(c fiber.Ctx) error {
		if e := service.RestoreDevice(c.Context(), c.Locals("principal").(domain.Principal), c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	secured.Get("/devices/:id/state", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := service.Repo.DeviceState(c.Context(), c.Locals("principal").(domain.Principal), c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	ingest := api.Group("/ingest/gateways/:id", limiter.New(limiter.Config{Max: 120, Expiration: time.Minute, LimitReached: func(c fiber.Ctx) error { return fiber.ErrTooManyRequests }}), func(c fiber.Ctx) error {
		// MG3 supports HTTP Basic. Credential is never placed in URL/query or logs.
		raw := c.Get("Authorization")
		if !strings.HasPrefix(raw, "Basic ") || len(raw) > 512 {
			return domain.ErrUnauthorized
		}
		decoded, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "Basic "))
		if e != nil {
			return domain.ErrUnauthorized
		}
		user, token, ok := strings.Cut(string(decoded), ":")
		if !ok || user != c.Params("id") {
			return domain.ErrUnauthorized
		}
		tenant, e := service.Gateway(c.Context(), user, token)
		if e != nil {
			return e
		}
		c.Locals("gateway_tenant", tenant)
		return c.Next()
	})
	ingest.Post("/packets", func(c fiber.Ctx) error {
		media, _, e := mime.ParseMediaType(c.Get("Content-Type"))
		if e != nil || media != "application/json" {
			return domain.ErrInvalid
		}
		id, e := service.Capture(c.Context(), c.Locals("gateway_tenant").(string), c.Params("id"), c.Body())
		if e != nil {
			return e
		}
		return c.Status(202).JSON(fiber.Map{"packet_id": id, "stored": true, "decoded": false})
	})
	ingest.Post("/telemetry", func(c fiber.Ctx) error {
		var in struct {
			DeviceID string             `json:"device_id"`
			TS       time.Time          `json:"ts"`
			Metrics  map[string]float64 `json:"metrics"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		out, e := service.Ingest(c.Context(), c.Locals("gateway_tenant").(string), c.Params("id"), in.DeviceID, in.TS, in.Metrics)
		if e != nil {
			return e
		}
		return c.Status(202).JSON(out)
	})
	return api
}
func body(c fiber.Ctx, dst any, limit int) error {
	media, _, e := mime.ParseMediaType(c.Get("Content-Type"))
	if e != nil || media != "application/json" || len(c.Body()) > limit {
		return domain.ErrInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(c.Body()))
	dec.DisallowUnknownFields()
	if e := dec.Decode(dst); e != nil {
		return domain.ErrInvalid
	}
	if dec.Decode(new(any)) != io.EOF {
		return domain.ErrInvalid
	}
	return nil
}
