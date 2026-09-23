package httpapi

import (
	"aether/backend/internal/alerts"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

func alertRoutes(r fiber.Router, s *app.Service, sender *alerts.Sender) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	manage := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanManageDevices() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	operate := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanOperate() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	limit := func(c fiber.Ctx, def, max int) (int, error) {
		n, e := security.ParseLimit(c.Query("limit"), def)
		if e != nil || n > max {
			return 0, domain.ErrInvalid
		}
		return n, nil
	}

	r.Get("/events", func(c fiber.Ctx) error {
		n, e := limit(c, 100, 500)
		if e != nil {
			return e
		}
		external := strings.ToLower(strings.TrimSpace(c.Query("external_id")))
		if len(external) > 128 || strings.ContainsAny(external, "\r\n\x00") {
			return domain.ErrInvalid
		}
		out, e := s.Repo.ListEvents(c.Context(), principal(c), external, n)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	r.Get("/alerts", func(c fiber.Ctx) error {
		n, e := limit(c, 100, 500)
		if e != nil {
			return e
		}
		status := c.Query("status")
		if status != "" && status != "open" && status != "acknowledged" && status != "resolved" {
			return domain.ErrInvalid
		}
		out, e := s.Repo.ListAlerts(c.Context(), principal(c), status, n)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	r.Get("/alerts/summary", func(c fiber.Ctx) error {
		out, e := s.Repo.AlertCounts(c.Context(), principal(c))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	transition := func(to string) fiber.Handler {
		return func(c fiber.Ctx) error {
			p, e := operate(c)
			if e != nil {
				return e
			}
			if !security.ValidID(c.Params("id")) {
				return domain.ErrInvalid
			}
			var in struct {
				Note string `json:"note"`
			}
			if len(c.Body()) > 0 {
				if e := body(c, &in, 4096); e != nil {
					return e
				}
			}
			if len(in.Note) > 500 {
				return domain.ErrInvalid
			}
			if e := s.Repo.TransitionAlert(c.Context(), p, c.Params("id"), to, strings.TrimSpace(in.Note)); e != nil {
				return e
			}
			return c.SendStatus(204)
		}
	}
	r.Post("/alerts/:id/ack", transition("acknowledged"))
	r.Post("/alerts/:id/resolve", transition("resolved"))

	r.Get("/rules", func(c fiber.Ctx) error {
		out, e := s.Repo.ListRules(c.Context(), principal(c))
		if e != nil {
			return e
		}
		// Rules Aether seeded itself are labelled in the UI so the operator knows where they came from.
		// A lookup failure must not hide the rules: the label is cosmetic, the list is not.
		if builtin, e := s.Repo.BuiltinRuleIDs(c.Context(), principal(c)); e == nil {
			seeded := map[string]bool{}
			for _, id := range builtin {
				seeded[id] = true
			}
			for i := range out {
				out[i].Builtin = seeded[out[i].ID]
			}
		}
		return c.JSON(fiber.Map{"items": out, "event_types": domain.RuleEventTypes, "severities": domain.Severities})
	})
	type ruleInput struct {
		Name      string           `json:"name"`
		Enabled   *bool            `json:"enabled"`
		EventType string           `json:"event_type"`
		Severity  string           `json:"severity"`
		Scope     domain.RuleScope `json:"scope"`
		Channels  []string         `json:"channels"`
		DedupeSec *int             `json:"dedupe_sec"`
	}
	parseRule := func(c fiber.Ctx, id string) (domain.AlertRule, error) {
		var in ruleInput
		if e := body(c, &in, 8192); e != nil {
			return domain.AlertRule{}, e
		}
		rule := domain.AlertRule{ID: id, Name: strings.TrimSpace(in.Name), Enabled: in.Enabled == nil || *in.Enabled, EventType: in.EventType, Severity: in.Severity, Scope: in.Scope, Channels: in.Channels, DedupeSec: 600}
		if in.DedupeSec != nil {
			rule.DedupeSec = *in.DedupeSec
		}
		if rule.Channels == nil {
			rule.Channels = []string{}
		}
		for i := range rule.Channels {
			if !security.ValidID(rule.Channels[i]) {
				return rule, domain.ErrInvalid
			}
		}
		for i := range rule.Scope.ExternalIDs {
			rule.Scope.ExternalIDs[i] = strings.ToLower(strings.TrimSpace(rule.Scope.ExternalIDs[i]))
		}
		return rule, alerts.ValidateRule(rule)
	}
	r.Post("/rules", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		rule, e := parseRule(c, uuid.NewString())
		if e != nil {
			return e
		}
		if e := s.Repo.SaveRule(c.Context(), p, rule, true); e != nil {
			return e
		}
		rule.CreatedAt, rule.UpdatedAt = time.Now().UTC(), time.Now().UTC()
		return c.Status(201).JSON(rule)
	})
	r.Post("/rules/:id/update", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		rule, e := parseRule(c, c.Params("id"))
		if e != nil {
			return e
		}
		if e := s.Repo.SaveRule(c.Context(), p, rule, false); e != nil {
			return e
		}
		return c.JSON(rule)
	})
	r.Post("/rules/:id/delete", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.DeleteRule(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})

	r.Get("/channels", func(c fiber.Ctx) error {
		out, e := s.Repo.ListChannels(c.Context(), principal(c))
		if e != nil {
			return e
		}
		if !principal(c).CanManageDevices() {
			// Webhook URLs and recipient ids are credentials in practice; non-managers only need names.
			for i := range out {
				out[i].Config = map[string]string{}
			}
		}
		return c.JSON(fiber.Map{"items": out, "kinds": domain.ChannelKinds, "email_available": sender.SMTP.Configured()})
	})
	r.Post("/channels", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		var in struct {
			Name    string            `json:"name"`
			Kind    string            `json:"kind"`
			Enabled *bool             `json:"enabled"`
			Config  map[string]string `json:"config"`
			Secret  string            `json:"secret"`
		}
		if e := body(c, &in, 8192); e != nil {
			return e
		}
		ch := domain.NotificationChannel{ID: uuid.NewString(), Name: strings.TrimSpace(in.Name), Kind: in.Kind, Enabled: in.Enabled == nil || *in.Enabled, Config: map[string]string{}, HasSecret: in.Secret != ""}
		if ch.Name == "" || len(ch.Name) > 128 || len(in.Secret) > 512 || len(in.Config) > 10 {
			return domain.ErrInvalid
		}
		for k, v := range in.Config {
			if len(k) > 32 || len(v) > 2048 {
				return domain.ErrInvalid
			}
			ch.Config[k] = strings.TrimSpace(v)
		}
		if e := sender.ValidateChannel(ch.Kind, ch.Config, ch.HasSecret); e != nil {
			if e.Error() == "smtp_not_configured" {
				return fiber.ErrServiceUnavailable
			}
			return domain.ErrInvalid
		}
		sealed := ""
		if in.Secret != "" {
			if sealed, e = security.Seal(s.Secrets, in.Secret); e != nil {
				return e
			}
		}
		if e := s.Repo.CreateChannel(c.Context(), p, ch, sealed); e != nil {
			return e
		}
		ch.CreatedAt = time.Now().UTC()
		return c.Status(201).JSON(ch)
	})
	r.Post("/channels/:id/update", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		if in.Enabled == nil {
			return domain.ErrInvalid
		}
		if e := s.Repo.SetChannelEnabled(c.Context(), p, c.Params("id"), *in.Enabled); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	r.Post("/channels/:id/delete", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.DeleteChannel(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	// Sends a synthetic alert so the operator can confirm the channel works before relying on it.
	r.Post("/channels/:id/test", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		ch, sealed, e := s.Repo.ChannelWithSecret(c.Context(), p, c.Params("id"))
		if e != nil {
			return e
		}
		secret := ""
		if sealed != "" {
			if secret, e = security.OpenAny(sealed, s.Secrets, s.LegacySecrets); e != nil {
				return errors.New("channel secret unreadable")
			}
		}
		now := time.Now().UTC()
		ev := domain.DeviceEvent{ID: uuid.NewString(), ExternalID: "test", DeviceName: "Aether test", EventType: "test", Detail: map[string]any{"note": "ข้อความทดสอบจากหน้าการแจ้งเตือน ไม่ได้มาจากอุปกรณ์จริง"}, OccurredAt: now}
		alert := domain.Alert{ID: uuid.NewString(), EventID: ev.ID, DeviceName: ev.DeviceName, EventType: "test", Severity: "info", Title: "ทดสอบช่องทางแจ้งเตือน · " + ch.Name, Status: "open", OpenedAt: now}
		payload := alerts.Payload{Schema: "aether.alert.v1", SentAt: now, Text: alerts.Message(alert, ev, "—"), Alert: alert, Event: ev, Gateway: "—", TenantID: p.TenantID}
		if e := sender.Send(c.Context(), ch, secret, payload); e != nil {
			return c.Status(502).JSON(fiber.Map{"error": "delivery_failed", "detail": e.Error()})
		}
		return c.JSON(fiber.Map{"sent": true})
	})
	r.Get("/notifications", func(c fiber.Ctx) error {
		n, e := limit(c, 100, 500)
		if e != nil {
			return e
		}
		out, e := s.Repo.ListNotifications(c.Context(), principal(c), n)
		if e != nil {
			return e
		}
		if !principal(c).CanManageDevices() {
			for i := range out {
				out[i].LastError = nil
			}
		}
		return c.JSON(fiber.Map{"items": out})
	})
}
