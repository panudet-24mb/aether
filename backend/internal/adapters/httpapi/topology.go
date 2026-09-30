package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"errors"
	"math"

	"github.com/gofiber/fiber/v3"
)

// topologyRoutes serves the connect view's shared canvas layout. Reading needs only membership; saving is a
// write for owner, admin and operator (the access gate adds the member's "connect" module level on top).
func topologyRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	view := func(c fiber.Ctx) (string, error) {
		v := c.Query("view", domain.LayoutViewConnect)
		if !domain.ValidLayoutView(v) {
			return "", domain.ErrInvalid
		}
		return v, nil
	}
	r.Get("/topology/layout", func(c fiber.Ctx) error {
		v, e := view(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.GetLayout(c.Context(), principal(c), v)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	type point struct {
		X *float64 `json:"x"`
		Y *float64 `json:"y"`
	}
	type input struct {
		View      string           `json:"view"`
		Version   *int64           `json:"version"`
		Positions map[string]point `json:"positions"`
		Remove    []string         `json:"remove"`
	}
	r.Post("/topology/layout", func(c fiber.Ctx) error {
		p := principal(c)
		if !p.CanOperate() {
			return domain.ErrForbidden
		}
		var in input
		if e := body(c, &in, domain.MaxLayoutBody); e != nil {
			return e
		}
		if in.View == "" {
			in.View = domain.LayoutViewConnect
		}
		if !domain.ValidLayoutView(in.View) || in.Version == nil || *in.Version < 0 || len(in.Positions)+len(in.Remove) > domain.MaxLayoutEntries {
			return domain.ErrInvalid
		}
		ch := domain.LayoutChange{View: in.View, BaseVersion: *in.Version, Positions: make(map[string]domain.CanvasPoint, len(in.Positions))}
		for id, pt := range in.Positions {
			if !domain.ValidLayoutNode(id) || pt.X == nil || pt.Y == nil {
				return domain.ErrInvalid
			}
			// Positions are stored as sent (the canvas works in fractional units), rounded only to keep rows small.
			point := domain.CanvasPoint{X: math.Round(*pt.X*100) / 100, Y: math.Round(*pt.Y*100) / 100}
			if !point.Valid() {
				return domain.ErrInvalid
			}
			ch.Positions[id] = point
		}
		for _, id := range in.Remove {
			if !domain.ValidLayoutNode(id) {
				return domain.ErrInvalid
			}
			if _, both := ch.Positions[id]; both {
				return domain.ErrInvalid
			}
		}
		ch.Remove = in.Remove
		out, e := s.Repo.SaveLayout(c.Context(), p, ch)
		var conflict domain.LayoutConflict
		if errors.As(e, &conflict) {
			// The caller merges its own moves onto the current layout and saves again with this version.
			return c.Status(409).JSON(fiber.Map{"error": "version_conflict", "layout": conflict.Current, "request_id": c.GetRespHeader("X-Request-ID")})
		}
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
}
