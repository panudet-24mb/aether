package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"

	"github.com/gofiber/fiber/v3"
)

// twinRoutes serves the digital twin (docs/platform/digital-twin.md). Read-only; the floorplan module gates it
// (moduleFor) and every read is in the access log (readRules), because worn tags are people.
func twinRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	r.Get("/twin/sites/:id/state", func(c fiber.Ctx) error {
		people := c.Query("people", domain.TwinPeopleCounts)
		if !security.ValidID(c.Params("id")) || !domain.ValidTwinPeople(people) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.TwinState(c.Context(), principal(c), c.Params("id"), people)
		if e != nil {
			return e
		}
		if out.Presence.Mode == domain.TwinPeopleNamed {
			c.Locals(readResourceLocal, "twin_live_named") // who looked at named people is its own line in the log
		}
		c.Set("Cache-Control", "no-store")
		return c.JSON(out)
	})
}
