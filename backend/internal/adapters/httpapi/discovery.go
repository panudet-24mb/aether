package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"github.com/gofiber/fiber/v3"
	"strings"
	"time"
)

func discoveryRoutes(r fiber.Router, s *app.Service) {
	r.Get("/discovery", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		gateways, e := s.Repo.ListGateways(c.Context(), p)
		if e != nil {
			return e
		}
		since := time.Now().Add(-24 * time.Hour)
		out := []domain.DiscoveredDevice{}
		for _, g := range gateways {
			devices, e := s.Repo.DiscoverDevices(c.Context(), p, g.ID, since)
			if e != nil {
				return e
			}
			if len(devices) == 0 {
				continue
			}
			sensors, e := s.Repo.StreamHistory(c.Context(), p, g.ID, since, 200)
			if e != nil {
				return e
			}
			for i := range devices {
				d := &devices[i]
				for _, sensor := range sensors {
					if sensor.ID != d.ExternalID {
						continue
					}
					d.Model, d.Kind, d.RSSI = sensor.Model, sensor.Kind, sensor.Latest.RSSI
					break
				}
				// Only a reported model earns a product match; shared frame kinds are not model identities.
				for _, profile := range domain.DeviceProfiles {
					if d.Model != "" && profile.InfoName != "" && strings.EqualFold(d.Model, profile.InfoName) {
						d.Profile = &profile
						break
					}
				}
			}
			out = append(out, devices...)
		}
		return c.JSON(fiber.Map{"items": out, "window_hours": 24, "limit_per_gateway": 100})
	})
}
