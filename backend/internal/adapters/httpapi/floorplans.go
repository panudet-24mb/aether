package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
)

func validText(s string, max int) bool { return len(s) <= max && !strings.ContainsRune(s, 0) }

func floorplanRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	manage := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanManageDevices() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	r.Get("/sites", func(c fiber.Ctx) error {
		out, e := s.Repo.ListSites(c.Context(), principal(c))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "zone_kinds": domain.ZoneKinds, "item_types": domain.FloorItemTypes})
	})
	r.Get("/sites/:id", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.GetSite(c.Context(), principal(c), c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	type siteInput struct {
		Name        string  `json:"name"`
		Description string  `json:"description"`
		ProjectID   *string `json:"project_id"`
	}
	parseSite := func(c fiber.Ctx) (domain.Site, error) {
		var in siteInput
		if e := body(c, &in, 4096); e != nil {
			return domain.Site{}, e
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || !validText(in.Name, 128) || !validText(in.Description, 500) || (in.ProjectID != nil && !security.ValidID(*in.ProjectID)) {
			return domain.Site{}, domain.ErrInvalid
		}
		return domain.Site{Name: in.Name, Description: strings.TrimSpace(in.Description), ProjectID: in.ProjectID}, nil
	}
	r.Post("/sites", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		site, e := parseSite(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.CreateSite(c.Context(), p, site)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})
	r.Post("/sites/:id/update", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		site, e := parseSite(c)
		if e != nil || !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		site.ID = c.Params("id")
		if e := s.Repo.UpdateSite(c.Context(), p, site); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	r.Post("/sites/:id/archive", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.ArchiveSite(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	type floorInput struct {
		Name       string                  `json:"name"`
		Level      int                     `json:"level"`
		WidthM     float64                 `json:"width_m"`
		DepthM     float64                 `json:"depth_m"`
		CeilingM   float64                 `json:"ceiling_m"`
		Revision   int                     `json:"revision"`
		Layout     domain.FloorLayout      `json:"layout"`
		Placements []domain.FloorPlacement `json:"placements"`
	}
	parseFloor := func(c fiber.Ctx) (domain.Floor, error) {
		var in floorInput
		if e := body(c, &in, 256*1024); e != nil {
			return domain.Floor{}, e
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || !validText(in.Name, 128) || in.Level < -20 || in.Level > 200 || in.WidthM < 2 || in.WidthM > 1000 || in.DepthM < 2 || in.DepthM > 1000 || in.CeilingM < 2 || in.CeilingM > 30 || len(in.Placements) > 500 {
			return domain.Floor{}, domain.ErrInvalid
		}
		if e := in.Layout.Validate(); e != nil {
			return domain.Floor{}, e
		}
		seen := map[string]bool{}
		for _, pl := range in.Placements {
			if !pl.Valid() || seen[pl.AssetKind+pl.AssetID] {
				return domain.Floor{}, domain.ErrInvalid
			}
			seen[pl.AssetKind+pl.AssetID] = true
		}
		return domain.Floor{Name: in.Name, Level: in.Level, WidthM: in.WidthM, DepthM: in.DepthM, CeilingM: in.CeilingM, Revision: in.Revision, Layout: in.Layout, Placements: in.Placements}, nil
	}
	r.Post("/sites/:id/floors", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		f, e := parseFloor(c)
		if e != nil || !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		f.SiteID = c.Params("id")
		out, e := s.Repo.CreateFloor(c.Context(), p, f)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})
	r.Post("/floors/:id/save", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		f, e := parseFloor(c)
		if e != nil || !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		f.ID = c.Params("id")
		revision, bumped, e := s.Repo.SaveFloor(c.Context(), p, f)
		if e != nil {
			return e
		}
		// bumped: other floors that lost an asset to this one, with their new revisions.
		return c.JSON(fiber.Map{"revision": revision, "bumped": bumped})
	})
	r.Post("/floors/:id/delete", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.DeleteFloor(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	// The browser shrinks the scan before upload (the API body limit is 256 KiB), so the stored image is small.
	r.Post("/floors/:id/image", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		var in struct {
			Data string `json:"data_base64"`
		}
		if e := body(c, &in, 256*1024); e != nil || !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		var data []byte
		mime := ""
		if in.Data != "" {
			if data, e = base64.StdEncoding.DecodeString(in.Data); e != nil || len(data) == 0 || len(data) > 196608 {
				return domain.ErrInvalid
			}
			// Trust the bytes, not a client-supplied type.
			mime = http.DetectContentType(data)
			if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
				return domain.ErrInvalid
			}
		}
		if e := s.Repo.SetFloorImage(c.Context(), p, c.Params("id"), mime, data); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	r.Get("/floors/:id/image", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		mime, data, e := s.Repo.FloorImage(c.Context(), principal(c), c.Params("id"))
		if e != nil {
			return e
		}
		c.Set("Content-Type", mime)
		c.Set("Cache-Control", "private, no-store")
		c.Set("X-Content-Type-Options", "nosniff")
		return c.SendStream(bytes.NewReader(data), len(data))
	})
}
