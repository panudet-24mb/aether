package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"github.com/gofiber/fiber/v3"
	"strings"
)

var accessModules = map[string]bool{"live": true, "connect": true, "alerts": true, "assets": true, "floorplan": true, "automation": true, "studio": true, "team": true}

// These are restrictions on the existing role, never an elevation of that role.
func moduleFor(path, method string) string {
	path = strings.TrimPrefix(path, "/api/v1/")
	head := strings.Split(path, "/")[0]
	switch head {
	case "sites", "floors":
		return "floorplan"
	case "automations":
		return "automation"
	case "members":
		return "team"
	case "assets":
		return "assets"
	case "alerts", "rules", "channels", "events":
		return "alerts"
	case "studio":
		return "studio"
	case "gateways", "devices", "mqtt", "discovery", "templates":
		if method != "GET" || head == "discovery" {
			return "connect"
		}
	}
	return ""
}
func accessGate(s *app.Service) fiber.Handler {
	return func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if p.Role == "owner" {
			return c.Next()
		}
		module := moduleFor(c.Path(), c.Method())
		if module == "" {
			return c.Next()
		}
		if module == "team" {
			self, e := s.Repo.MemberSelf(c.Context(), p)
			if e != nil {
				return e
			}
			if len(self.ProjectIDs) > 0 {
				return domain.ErrForbidden
			}
		}
		access, e := s.Repo.MemberAccess(c.Context(), p, p.UserID)
		if e != nil {
			return e
		}
		level := access[module]
		if level == "none" || (level == "read" && c.Method() != "GET") {
			return domain.ErrForbidden
		}
		return c.Next()
	}
}
func memberAccessRoutes(r fiber.Router, s *app.Service) {
	r.Get("/members/:user_id/access", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if !p.CanManageMembers() {
			return domain.ErrForbidden
		}
		out, e := s.Repo.MemberAccess(c.Context(), p, c.Params("user_id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	r.Post("/members/:user_id/access", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if !p.CanManageMembers() {
			return domain.ErrForbidden
		}
		var in map[string]string
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		for module, level := range in {
			if !accessModules[module] || (level != "none" && level != "read" && level != "write") {
				return domain.ErrInvalid
			}
		}
		if e := s.Repo.SetMemberAccess(c.Context(), p, c.Params("user_id"), in); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}
