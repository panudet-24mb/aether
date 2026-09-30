package httpapi

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/opsstatus"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

// systemRoutes serves the deployment's own health to workspace owners: backups and point-in-time recovery, from the
// file the pitr service writes (OPS_STATUS_FILE). Without that file configured (development) it answers
// configured=false and the UI shows nothing. The status is about the whole deployment, so with OPS_STATUS_TENANT
// set only that workspace's owners see it; unset, every owner does (single-workspace deployments).
func systemRoutes(r fiber.Router, statusFile, statusTenant string) {
	r.Get("/system/status", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		if p.Role != "owner" || (statusTenant != "" && !strings.EqualFold(p.TenantID, statusTenant)) {
			return domain.ErrForbidden
		}
		c.Set("Cache-Control", "no-store")
		if statusFile == "" {
			return c.JSON(fiber.Map{"configured": false})
		}
		return c.JSON(fiber.Map{"configured": true, "backup": opsstatus.Read(statusFile, time.Now().UTC())})
	})
}
