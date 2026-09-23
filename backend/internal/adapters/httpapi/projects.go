package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"

	"github.com/gofiber/fiber/v3"
)

func projectRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	r.Get("/projects", func(c fiber.Ctx) error {
		out, e := s.Repo.ListProjects(c.Context(), principal(c), c.Query("archived") == "true")
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "colors": domain.ProjectColors})
	})
	type input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Color       string `json:"color"`
	}
	r.Post("/projects", func(c fiber.Ctx) error {
		var in input
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		out, e := s.CreateProject(c.Context(), principal(c), in.Name, in.Description, in.Color)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})
	r.Post("/projects/:id/update", func(c fiber.Ctx) error {
		var in input
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		out, e := s.UpdateProject(c.Context(), principal(c), c.Params("id"), in.Name, in.Description, in.Color)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	r.Post("/projects/:id/archive", func(c fiber.Ctx) error {
		p := principal(c)
		if !p.CanManageDevices() {
			return domain.ErrForbidden
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.ArchiveProject(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	// project_id null (or omitted) returns the gateway to "unassigned".
	r.Post("/gateways/:id/project", func(c fiber.Ctx) error {
		p := principal(c)
		if !p.CanManageDevices() {
			return domain.ErrForbidden
		}
		var in struct {
			ProjectID *string `json:"project_id"`
		}
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) || (in.ProjectID != nil && !security.ValidID(*in.ProjectID)) {
			return domain.ErrInvalid
		}
		if e := s.Repo.SetGatewayProject(c.Context(), p, c.Params("id"), in.ProjectID); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}
