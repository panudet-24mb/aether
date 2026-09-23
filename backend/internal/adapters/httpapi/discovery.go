package httpapi

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"github.com/gofiber/fiber/v3"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Discovery lists what a gateway hears right now, not everything it ever heard, and only Minew devices.
// Phones, AirTags and other people's beacons rotate their MAC every few minutes; listing them turned one
// visitor into dozens of "new devices". ?minutes= widens the window (max 24h); ?all=1 is a diagnostic
// that includes everything the gateway heard.
const (
	discoveryDefaultMinutes = 15
	discoveryPerGateway     = 100
	simPrefix               = "SIM · ข้อมูลจำลอง · "
)

// streamModel recovers the model from a stream name set by an earlier info frame ("Minew C10"), for an
// uplink whose latest sample carried only a beacon slot.
func streamModel(name string) string {
	name = strings.TrimPrefix(name, simPrefix)
	if slices.Contains(minew.GenericNames(), name) || !strings.HasPrefix(name, "Minew ") {
		return ""
	}
	return strings.TrimPrefix(name, "Minew ")
}

// minewDevice is true when the identifier sent a Minew frame: an FFE1 info frame naming its model (also
// kept in the stream name), or any decoded FFE1 kind. Bare iBeacon/Eddystone and undecodable
// advertisements are what phones, AirTags and foreign beacons look like, and stay out.
func minewDevice(d domain.DiscoveredDevice) bool {
	return d.Model != "" || (d.Kind != "" && d.Kind != "beacon")
}

func discoveryRoutes(r fiber.Router, s *app.Service) {
	r.Get("/discovery", func(c fiber.Ctx) error {
		p := c.Locals("principal").(domain.Principal)
		minutes := discoveryDefaultMinutes
		if v := c.Query("minutes"); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 24*60 {
				return fiber.NewError(fiber.StatusBadRequest, "minutes must be 1..1440")
			}
			minutes = n
		}
		all := c.Query("all") == "1" || c.Query("all") == "true"
		gateways, e := s.Repo.ListGateways(c.Context(), p)
		if e != nil {
			return e
		}
		since := time.Now().Add(-time.Duration(minutes) * time.Minute)
		out := []domain.DiscoveredDevice{}
		hidden := map[string]int{}
		total := 0
		for _, g := range gateways {
			devices, e := s.Repo.DiscoverDevices(c.Context(), p, g.ID, since)
			if e != nil {
				return e
			}
			kept := 0
			for _, d := range devices {
				if d.Model == "" {
					d.Model = streamModel(d.StreamName)
				}
				// Only a reported model earns a product match; shared frame kinds are not model identities.
				for _, profile := range domain.DeviceProfiles {
					if profile.MatchesInfo(d.Model) {
						d.Profile = &profile
						break
					}
				}
				if !all && !minewDevice(d) {
					hidden[g.ID]++
					total++
					continue
				}
				if kept == discoveryPerGateway {
					continue
				}
				kept++
				out = append(out, d)
			}
		}
		return c.JSON(fiber.Map{"items": out, "window_minutes": minutes, "window_hours": float64(minutes) / 60, "limit_per_gateway": discoveryPerGateway, "hidden_unknown": total, "hidden_by_gateway": hidden, "all": all})
	})
}
