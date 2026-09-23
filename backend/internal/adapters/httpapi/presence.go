package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v3"
)

var externalID = regexp.MustCompile(`^[0-9a-f]{12}$`)

func presenceRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	// Where is this tag now? Works for any identity the workspace has heard, roaming or not.
	r.Get("/presence/:external", func(c fiber.Ctx) error {
		id := strings.ToLower(c.Params("external"))
		if !externalID.MatchString(id) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.Presence(c.Context(), principal(c), id)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	r.Post("/devices/:id/roaming", func(c fiber.Ctx) error {
		var in struct {
			Roaming *bool `json:"roaming"`
		}
		if e := body(c, &in, 256); e != nil {
			return e
		}
		p := principal(c)
		if !p.CanManageDevices() {
			return domain.ErrForbidden
		}
		if in.Roaming == nil || !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.SetDeviceRoaming(c.Context(), p, c.Params("id"), *in.Roaming); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}
