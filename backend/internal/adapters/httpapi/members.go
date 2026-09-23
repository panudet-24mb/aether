package httpapi

import (
	"net/mail"
	"strings"

	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"github.com/gofiber/fiber/v3"
)

// Members of a workspace: who is in it, what role they have and which projects they may see.
// There is no email delivery in an on-premise deployment, so there are no invitations: an owner creates
// the account with a first password and hands it over in person. The member is then asked to replace it
// (must_change_password), and POST /api/v1/auth/password is the only route that can clear that flag.
//
// Every rule that depends on database state (an admin may not touch an owner, the last owner stays, at
// most 200 members) is decided inside the writing transaction in the postgres adapter. What happens here
// is input validation and the coarse owner/admin gate.

const maxMemberProjects = 50

// memberEmail normalises and conservatively validates an address: lower case, a single RFC 5322 address
// with no display name, no routing characters, at most 254 bytes. Same rule as registration.
func memberEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	parsed, e := mail.ParseAddress(email)
	if e != nil || parsed.Address != email || len(email) > 254 || strings.ContainsAny(email, " \t\r\n\x00,;<>") {
		return "", false
	}
	if _, domainPart, ok := strings.Cut(email, "@"); !ok || !strings.Contains(domainPart, ".") {
		return "", false
	}
	return email, true
}

// memberProjects validates the requested project access list and removes duplicates.
// An empty list means "every project of the workspace".
func memberProjects(ids []string) ([]string, bool) {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if !security.ValidID(id) {
			return nil, false
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, len(out) <= maxMemberProjects
}

// memberName falls back to the local part of the address when no display name was supplied.
func memberName(raw, email string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	return name, len(name) >= 1 && len(name) <= 128 && !strings.ContainsAny(name, "\x00\r\n")
}

// passwordChangeExempt lists the only routes a session may reach while it still owes a password change:
// the change itself, logging out, and asking who it is (the UI needs the flag to draw the right screen).
func passwordChangeExempt(path string) bool {
	switch strings.TrimSuffix(path, "/") {
	case "/api/v1/auth/password", "/api/v1/auth/logout", "/api/v1/me":
		return true
	}
	return false
}

// meHandler answers GET /api/v1/me. Besides the identity it reports the caller's role and the project
// scope the database enforces for them (null = every project), so the UI can hide what is not theirs.
func meHandler(s *app.Service, mode string) fiber.Handler {
	return func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		self, e := s.Repo.MemberSelf(c.Context(), p)
		if e != nil {
			return e
		}
		permissions, e := s.Repo.MemberAccess(c.Context(), p, p.UserID)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"permissions": permissions, "user_id": self.UserID, "tenant_id": self.TenantID, "email": self.Email, "name": self.Name,
			"role": self.Role, "project_ids": self.ProjectIDs, "must_change_password": self.MustChangePassword, "deployment_mode": mode})
	}
}

// passwordHandler answers POST /api/v1/auth/password for the signed-in user. It lives in the auth group,
// so it inherits the exact-Origin check and the 10/minute budget the other credential routes use.
func passwordHandler(s *app.Service) fiber.Handler {
	return func(c fiber.Ctx) error {
		var in struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		p := c.Locals("principal").(domain.Principal)
		if e := s.ChangePassword(c.Context(), p, in.CurrentPassword, in.NewPassword); e != nil {
			return e
		}
		return c.SendStatus(204)
	}
}

func memberRoutes(r fiber.Router, s *app.Service, mode string) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	manage := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanManageMembers() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	target := func(c fiber.Ctx) (string, error) {
		id := c.Params("user_id")
		if !security.ValidID(id) {
			return "", domain.ErrInvalid
		}
		return id, nil
	}

	r.Get("/me", meHandler(s, mode))

	r.Get("/members", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.ListMembers(c.Context(), p)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "roles": domain.MemberRoles, "max_members": domain.MaxMembers})
	})

	r.Post("/members", func(c fiber.Ctx) error {
		var in struct {
			Email      string   `json:"email"`
			Name       string   `json:"name"`
			Role       string   `json:"role"`
			Password   string   `json:"password"`
			ProjectIDs []string `json:"project_ids"`
		}
		if e := body(c, &in, 8192); e != nil {
			return e
		}
		p, e := manage(c)
		if e != nil {
			return e
		}
		email, ok := memberEmail(in.Email)
		if !ok || !domain.ValidMemberRole(in.Role) {
			return domain.ErrInvalid
		}
		name, ok := memberName(in.Name, email)
		if !ok {
			return domain.ErrInvalid
		}
		projects, ok := memberProjects(in.ProjectIDs)
		if !ok {
			return domain.ErrInvalid
		}
		member, e := s.AddMember(c.Context(), p, email, name, in.Role, in.Password, projects)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(member)
	})

	r.Post("/members/:user_id/update", func(c fiber.Ctx) error {
		var in struct {
			Role       string   `json:"role"`
			ProjectIDs []string `json:"project_ids"`
		}
		if e := body(c, &in, 8192); e != nil {
			return e
		}
		p, e := manage(c)
		if e != nil {
			return e
		}
		id, e := target(c)
		if e != nil {
			return e
		}
		if !domain.ValidMemberRole(in.Role) {
			return domain.ErrInvalid
		}
		projects, ok := memberProjects(in.ProjectIDs)
		if !ok {
			return domain.ErrInvalid
		}
		if e := s.Repo.UpdateMember(c.Context(), p, id, in.Role, projects); e != nil {
			return e
		}
		return c.SendStatus(204)
	})

	r.Post("/members/:user_id/remove", func(c fiber.Ctx) error {
		if e := body(c, &struct{}{}, 256); e != nil {
			return e
		}
		p, e := manage(c)
		if e != nil {
			return e
		}
		id, e := target(c)
		if e != nil {
			return e
		}
		if e := s.Repo.RemoveMember(c.Context(), p, id); e != nil {
			return e
		}
		return c.SendStatus(204)
	})

	r.Post("/members/:user_id/reset-password", func(c fiber.Ctx) error {
		var in struct {
			Password string `json:"password"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		p, e := manage(c)
		if e != nil {
			return e
		}
		id, e := target(c)
		if e != nil {
			return e
		}
		if e := s.ResetMemberPassword(c.Context(), p, id, in.Password); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}
