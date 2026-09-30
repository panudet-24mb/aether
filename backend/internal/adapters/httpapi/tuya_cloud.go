package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"context"
	"time"

	"github.com/gofiber/fiber/v3"
)

// linkTimeout bounds a link request: proving the credentials is a single Tuya token call, and the whole request must
// finish inside the API's 10 s request budget. The device list and models are fetched afterwards by the worker.
const linkTimeout = 8 * time.Second

// tuyaCloudRoutes are the Tuya Cloud (zero-install) routes of a tuya-cloud gateway. Linking and syncing exist only
// when this deployment enabled TUYA_CLOUD (otherwise 404); the status of an existing link stays readable, and unlinking
// always works so a deployment that turned the mode off can still wipe stored credentials. Every mutation is
// owner/admin (CanManageDevices) and passes the "connect" access module (non-GET /gateways routes).
func tuyaCloudRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	status := func(c fiber.Ctx) error {
		out, e := s.TuyaCloudLinkStatus(c.Context(), principal(c), c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	}
	r.Get("/gateways/:id/tuya-cloud", status)
	r.Post("/gateways/:id/tuya-cloud/link", func(c fiber.Ctx) error {
		if !s.TuyaCloudEnabled {
			return domain.ErrNotFound
		}
		var in struct {
			Region       string `json:"region"`
			Channel      string `json:"channel"`
			AccessID     string `json:"access_id"`
			AccessSecret string `json:"access_secret"`
		}
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(c.Context(), linkTimeout)
		defer cancel()
		e := s.LinkTuyaCloud(ctx, principal(c), c.Params("id"), in.Region, in.Channel, in.AccessID, in.AccessSecret)
		in.AccessID, in.AccessSecret = "", ""
		if e != nil {
			return e
		}
		return status(c)
	})
	r.Post("/gateways/:id/tuya-cloud/sync", func(c fiber.Ctx) error {
		if !s.TuyaCloudEnabled {
			return domain.ErrNotFound
		}
		if e := s.RequestTuyaCloudSync(c.Context(), principal(c), c.Params("id")); e != nil {
			return e
		}
		c.Status(202)
		return status(c)
	})
	r.Post("/gateways/:id/tuya-cloud/unlink", func(c fiber.Ctx) error {
		if e := s.UnlinkTuyaCloud(c.Context(), principal(c), c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}
