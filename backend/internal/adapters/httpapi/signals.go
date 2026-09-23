package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/signals"

	"github.com/gofiber/fiber/v3"
)

// Learned device signals. Reads are open to every member (a viewer needs to see why a device raises
// an event); everything that creates, confirms or deletes a signature is owner/admin, because a bad
// signature turns into alerts and notifications for the whole workspace.
func signalRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	manage := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanManageDevices() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}

	// Start teaching: phase 1 records the device at rest, phase 2 while the operator triggers it.
	r.Post("/signals/sessions", func(c fiber.Ctx) error {
		var in signals.NewSession
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		p, e := manage(c)
		if e != nil {
			return e
		}
		in.Clamp()
		if !in.Valid() || !security.ValidID(in.GatewayID) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.CreateSignalSession(c.Context(), p, in)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})

	// Polled by the wizard: live per-phase counters while it runs, ranked candidates once it is over.
	r.Get("/signals/sessions/:id", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.SignalSession(c.Context(), principal(c), c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})

	// The operator is ready to start triggering before the baseline countdown ends.
	r.Post("/signals/sessions/:id/advance", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.AdvanceSignalSession(c.Context(), p, c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})

	r.Post("/signals/sessions/:id/cancel", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.CancelSignalSession(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})

	// Confirm one candidate. scope=device keeps it to this tag, scope=profile applies it to every tag
	// registered with the same profile ("ใช้กับทุกตัวที่เป็นรุ่นนี้").
	r.Post("/signals/sessions/:id/confirm", func(c fiber.Ctx) error {
		var in struct {
			CandidateIndex int    `json:"candidate_index"`
			Scope          string `json:"scope"`
		}
		if e := body(c, &in, 256); e != nil {
			return e
		}
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) || (in.Scope != signals.ScopeDevice && in.Scope != signals.ScopeProfile) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.ConfirmSignalSession(c.Context(), p, c.Params("id"), in.CandidateIndex, in.Scope)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})

	r.Get("/signals", func(c fiber.Ctx) error {
		out, e := s.Repo.ListSignals(c.Context(), principal(c))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "event_types": signals.EventTypes, "max": signals.MaxSignals})
	})

	r.Post("/signals/:id/delete", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.DeleteSignal(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})

	// Replay the last ten minutes of raw history through the matcher: this is how the owner checks
	// for false positives before trusting a learned signal.
	r.Post("/signals/:id/test", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.TestSignal(c.Context(), p, c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
}
