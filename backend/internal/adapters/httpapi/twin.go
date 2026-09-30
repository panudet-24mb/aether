package httpapi

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

// twinRoutes serves the digital twin (docs/platform/digital-twin.md). Read-only apart from the owner's settings; the
// floorplan module gates it (moduleFor) and every read of people is in the access log (readRules), because worn tags
// are people.
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
			// Who looked at named people is its own line in the log, on every request (never folded into the dedupe
			// window).
			c.Locals(readResourceLocal, "twin_live_named")
			c.Locals(readAlwaysLocal, true)
		}
		c.Set("Cache-Control", "no-store")
		return c.JSON(out)
	})

	// Replay is heavier than the live state: 30 requests a minute per member (or display) on top of the per-IP limit.
	perUser := limiter.New(limiter.Config{Max: 30, Expiration: time.Minute,
		KeyGenerator: func(c fiber.Ctx) string {
			p := principal(c)
			if d, ok := domain.DisplayFrom(c.Context()); ok {
				return "twin-replay:display:" + d.ID
			}
			return "twin-replay:" + p.TenantID + ":" + p.UserID
		},
		LimitReached: func(c fiber.Ctx) error { return domain.ErrRateLimited }})
	// tooDense answers a window that is more than one response carries (or reads in TwinReplayTimeout): 413 with a
	// shorter end to ask for.
	tooDense := func(c fiber.Ctx, e error) (bool, error) {
		var dense domain.ErrTooDense
		if errors.As(e, &dense) {
			return true, c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "too_dense", "hint_to": dense.HintTo.UTC()})
		}
		return false, nil
	}
	window := func(c fiber.Ctx) (time.Time, time.Time, error) {
		from, e1 := time.Parse(time.RFC3339, c.Query("from"))
		to, e2 := time.Parse(time.RFC3339, c.Query("to"))
		if e1 != nil || e2 != nil || !to.After(from) || to.Sub(from) > domain.TwinReplayMaxWindow || to.After(time.Now().Add(time.Minute)) {
			return time.Time{}, time.Time{}, domain.ErrInvalid
		}
		return from, to, nil
	}
	r.Get("/twin/sites/:id/timeline", perUser, func(c fiber.Ctx) error {
		people := c.Query("people", domain.TwinPeopleCounts)
		if !security.ValidID(c.Params("id")) || !domain.ValidTwinPeople(people) {
			return domain.ErrInvalid
		}
		from, to, e := window(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.TwinTimeline(c.Context(), principal(c), c.Params("id"), from, to, people)
		if dense, sent := tooDense(c, e); dense {
			return sent
		}
		if e != nil {
			return e
		}
		if out.PeopleMode == domain.TwinPeopleNamed {
			c.Locals(readResourceLocal, "twin_timeline_named")
			c.Locals(readAlwaysLocal, true)
		}
		c.Set("Cache-Control", "no-store")
		return c.JSON(out)
	})
	r.Get("/twin/sites/:id/replay", perUser, func(c fiber.Ctx) error {
		people := c.Query("people", domain.TwinPeopleCounts)
		if !security.ValidID(c.Params("id")) || !domain.ValidTwinPeople(people) {
			return domain.ErrInvalid
		}
		from, to, e := window(c)
		if e != nil {
			return e
		}
		bucket := 0
		if b := c.Query("bucket", "auto"); b != "auto" {
			if bucket, e = strconv.Atoi(b); e != nil {
				return domain.ErrInvalid
			}
		}
		var layers []string
		if l := c.Query("layers"); l != "" {
			for _, x := range strings.Split(l, ",") {
				if x != "env" && x != "people" && x != "alerts" {
					return domain.ErrInvalid
				}
				layers = append(layers, x)
			}
		}
		q := domain.TwinReplayQuery{SiteID: c.Params("id"), From: from, To: to, Bucket: bucket, Layers: layers, People: people}
		body, etag, mode, e := s.Repo.TwinReplay(c.Context(), principal(c), q)
		if dense, sent := tooDense(c, e); dense {
			return sent
		}
		if e != nil {
			return e
		}
		// People in replay (pseudonymous or named) are logged on every request, never folded into the dedupe window,
		// and never stored by the browser or anything in between; headcounts may be kept for a minute.
		if mode == domain.TwinPeopleTracks || mode == domain.TwinPeopleNamed {
			c.Locals(readResourceLocal, "twin_replay_people")
			c.Locals(readAlwaysLocal, true)
			c.Set("Cache-Control", "no-store")
		} else {
			c.Set("Cache-Control", "private, max-age=60")
		}
		c.Set("ETag", etag)
		if match := c.Get("If-None-Match"); match != "" && match == etag {
			return c.SendStatus(fiber.StatusNotModified)
		}
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		return c.Send(body)
	})
	r.Get("/twin/people/:external/trail", perUser, func(c fiber.Ctx) error {
		ext := strings.ToLower(c.Params("external"))
		if ext == "" || len(ext) > 128 || strings.ContainsAny(ext, " /?#%") {
			return domain.ErrInvalid
		}
		from, to, e := window(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.TwinTrail(c.Context(), principal(c), ext, from, to)
		if dense, sent := tooDense(c, e); dense {
			return sent
		}
		if e != nil {
			return e
		}
		c.Set("Cache-Control", "no-store")
		return c.JSON(fiber.Map{"external": ext, "points": out})
	})

	r.Get("/twin/settings", func(c fiber.Ctx) error {
		out, e := s.Repo.TwinSettings(c.Context(), principal(c))
		if e != nil {
			return e
		}
		c.Set("Cache-Control", "no-store")
		return c.JSON(out)
	})
	r.Post("/twin/settings", func(c fiber.Ctx) error {
		p := principal(c)
		if p.Role != "owner" {
			return domain.ErrForbidden
		}
		var in domain.TwinSettings
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		out, e := s.Repo.SetTwinSettings(c.Context(), p, in)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
}
