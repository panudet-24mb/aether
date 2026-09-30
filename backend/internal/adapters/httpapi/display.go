package httpapi

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"time"

	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/studio"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

// Display links (docs/platform/display.md). Members manage displays under /api/v1/displays; a paired TV talks to
// /api/v1/kiosk with its display token. The token is accepted there (and on /ws) and nowhere else: every member
// route answers 403 to it, so a TV can never reach a write, whatever the route.

// pairFailures caps failed pairing attempts across all clients, on top of the per-IP limiter: a code is 8 symbols
// of 31 and lives 10 minutes, and many addresses together must not make guessing practical either.
type pairFailures struct {
	mu     sync.Mutex
	window time.Time
	count  int
}

// maxPairFailuresPerMinute is high enough that nobody can lock every TV out of pairing by failing on purpose, and low
// enough that guessing stays hopeless: 3000 guesses a minute against a code of 31^8 symbols that lives 10 minutes is
// about one in 30 million per live code.
const maxPairFailuresPerMinute = 3000

// clientKey is the per-address key of the pairing limiter: an IPv4 address, or the /64 of an IPv6 one (a single
// host usually owns a whole /64, so per-address limits on IPv6 are free to dodge).
func clientKey(c fiber.Ctx) string { return ipKey(c.IP()) }

func ipKey(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return raw
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// displayMask is what a display's studio view may pass to widgets (docs/platform/display.md §4): never member ids or
// operators' notes; without show_names, no name, title or MAC of a personal tag; and never where a person is.
type displayMask struct {
	tenant     string
	showNames  bool
	impersonal map[string]bool
}

func (m *displayMask) personal(ext string) bool { return !m.impersonal[strings.ToLower(ext)] }

func (m *displayMask) alerts(in []domain.Alert) []domain.Alert {
	out := make([]domain.Alert, 0, len(in))
	for _, a := range in {
		a.AckedBy, a.ResolvedBy, a.Note = nil, nil, nil
		if !m.showNames && (m.personal(a.ExternalID) || a.EventType == domain.EventButton) {
			a.DeviceName, a.Title, a.ExternalID = domain.DisplayWearerLabel(a.ExternalID), domain.DisplayMaskedTitle(a.EventType), domain.DisplayTag(m.tenant, a.ExternalID)
		}
		out = append(out, a)
	}
	return out
}

func (m *displayMask) events(in []domain.DeviceEvent) []domain.DeviceEvent {
	out := make([]domain.DeviceEvent, 0, len(in))
	for _, ev := range in {
		if !m.showNames && (m.personal(ev.ExternalID) || ev.EventType == domain.EventButton) {
			ev.DeviceName, ev.ExternalID, ev.Detail = domain.DisplayWearerLabel(ev.ExternalID), domain.DisplayTag(m.tenant, ev.ExternalID), map[string]any{}
		}
		out = append(out, ev)
	}
	return out
}

// presence of a personal tag is a location trace: a display never gets it, names or not.
func (m *displayMask) presence(p domain.Presence) domain.Presence {
	if m.personal(p.ExternalID) {
		return domain.Presence{ExternalID: domain.DisplayTag(m.tenant, p.ExternalID), Sightings: []domain.Sighting{}, ServerTime: p.ServerTime}
	}
	return p
}

func (f *pairFailures) blocked(now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if now.Sub(f.window) >= time.Minute {
		f.window, f.count = now, 0
	}
	return f.count >= maxPairFailuresPerMinute
}

func (f *pairFailures) fail(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if now.Sub(f.window) >= time.Minute {
		f.window, f.count = now, 0
	}
	f.count++
}

type displayInput struct {
	Name       string               `json:"name"`
	ProjectIDs []string             `json:"project_ids"`
	Playlist   []domain.DisplayView `json:"playlist"`
	ShowNames  bool                 `json:"show_names"`
	AllowAck   bool                 `json:"allow_ack"`
}

func (in displayInput) settings() domain.DisplaySettings {
	return domain.DisplaySettings{Name: in.Name, ProjectIDs: in.ProjectIDs, Playlist: in.Playlist, ShowNames: in.ShowNames, AllowAck: in.AllowAck}
}

// displayRoutes mounts the members' management routes (owner, or an admin who sees every project).
func displayRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	r.Get("/displays", func(c fiber.Ctx) error {
		out, e := s.Repo.ListDisplays(c.Context(), principal(c))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "view_kinds": domain.DisplayViewKinds, "max_views": domain.MaxDisplayViews,
			"min_seconds": domain.MinDisplaySeconds, "max_seconds": domain.MaxDisplaySeconds})
	})
	r.Post("/displays", func(c fiber.Ctx) error {
		var in displayInput
		if e := body(c, &in, 16000); e != nil {
			return e
		}
		out, e := s.CreateDisplay(c.Context(), principal(c), in.settings())
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})
	r.Post("/displays/:id/update", func(c fiber.Ctx) error {
		var in displayInput
		if e := body(c, &in, 16000); e != nil {
			return e
		}
		out, e := s.UpdateDisplay(c.Context(), principal(c), c.Params("id"), in.settings())
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	r.Post("/displays/:id/repair", func(c fiber.Ctx) error {
		out, e := s.RepairDisplay(c.Context(), principal(c), c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	r.Post("/displays/:id/revoke", func(c fiber.Ctx) error {
		if e := s.RevokeDisplay(c.Context(), principal(c), c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}

// displayToken reads a display token from the Authorization header.
func displayToken(c fiber.Ctx) (string, bool) {
	raw := c.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") || len(raw) > 512 {
		return "", false
	}
	token := strings.TrimPrefix(raw, "Bearer ")
	return token, app.IsDisplayToken(token)
}

// kioskRoutes mounts what a paired TV may call: pairing, its board, a floor plan, its studio dashboards, and
// (when the owner allowed it) acknowledging an alert. Mounted before the member routes.
func kioskRoutes(api *fiber.App, s *app.Service, reads *readLog) {
	failures := &pairFailures{}
	api.Post("/api/v1/kiosk/pair", limiter.New(limiter.Config{Max: 10, Expiration: time.Minute, KeyGenerator: clientKey, LimitReached: func(c fiber.Ctx) error { return fiber.ErrTooManyRequests }}), func(c fiber.Ctx) error {
		now := time.Now()
		if failures.blocked(now) {
			return domain.ErrRateLimited
		}
		var in struct {
			Code string `json:"code"`
		}
		if e := body(c, &in, 256); e != nil {
			return e
		}
		token, session, e := s.PairDisplay(c.Context(), in.Code, c.IP())
		if e != nil {
			failures.fail(now)
			return e
		}
		return c.JSON(fiber.Map{"token": token, "display": session})
	})
	kiosk := api.Group("/api/v1/kiosk", func(c fiber.Ctx) error {
		token, ok := displayToken(c)
		if !ok {
			return domain.ErrUnauthorized
		}
		session, e := s.AuthenticateDisplay(c.Context(), token, c.IP())
		if e != nil {
			return e
		}
		c.Locals("display", session)
		c.SetContext(domain.WithDisplay(c.Context(), session.ID))
		return c.Next()
	})
	display := func(c fiber.Ctx) domain.DisplaySession { return c.Locals("display").(domain.DisplaySession) }
	// logRead records a read of personal data by the display (wearer names, a studio dashboard), deduplicated
	// like a member's; a failed write withholds the answer.
	logRead := func(c fiber.Ctx, d domain.DisplaySession, resource, kind, subject string) error {
		p := domain.Principal{UserID: "display:" + d.ID, TenantID: d.TenantID}
		return reads.write(c.Context(), p, domain.AccessRead{Resource: resource, SubjectKind: kind, SubjectID: subject,
			RequestID: c.GetRespHeader(fiber.HeaderXRequestID), ClientIP: c.IP()}, false)
	}
	kiosk.Get("/session", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"display": display(c), "server_time": time.Now().UTC()})
	})
	kiosk.Get("/board", func(c fiber.Ctx) error {
		d := display(c)
		board, namesShown, e := s.Repo.DisplayBoard(c.Context(), d)
		if e != nil {
			return e
		}
		if namesShown {
			if e := logRead(c, d, "display_board", "wearer", "*"); e != nil {
				return e
			}
		}
		return c.JSON(board)
	})
	// A site with its floors, for the floor plan view: the one the playlist names, or the first the display sees.
	kiosk.Get("/floorplan", func(c fiber.Ctx) error {
		ref := c.Query("site")
		if ref != "" && !security.ValidID(ref) {
			return domain.ErrInvalid
		}
		p := display(c).Principal()
		if ref == "" {
			sites, e := s.Repo.ListSites(c.Context(), p)
			if e != nil {
				return e
			}
			for _, site := range sites {
				if len(site.Floors) > 0 {
					ref = site.ID
					break
				}
			}
			if ref == "" {
				return c.JSON(fiber.Map{"site": nil})
			}
		}
		site, e := s.Repo.GetSite(c.Context(), p, ref)
		if e != nil {
			return e
		}
		d := display(c)
		// Beds and desks may be labelled with who uses them: without names, their labels stay off the TV. Room and
		// zone names stay (they say where an emergency is).
		if !d.ShowNames {
			for i := range site.Floors {
				for j := range site.Floors[i].Layout.Items {
					if it := &site.Floors[i].Layout.Items[j]; it.Type == "bed" || it.Type == "desk" {
						it.Text = ""
					}
				}
			}
		}
		if e := logRead(c, d, "display_floorplan", "site", site.ID); e != nil {
			return e
		}
		return c.JSON(fiber.Map{"site": site})
	})
	kiosk.Get("/floors/:id/image", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		mime, data, e := s.Repo.FloorImage(c.Context(), display(c).Principal(), c.Params("id"))
		if e != nil {
			return e
		}
		c.Set("Content-Type", mime)
		c.Set("Cache-Control", "private, no-store")
		return c.SendStream(bytes.NewReader(data), len(data))
	})
	// A studio dashboard of the playlist, rendered as the display sees the data.
	kiosk.Get("/studio/:id", func(c fiber.Ctx) error {
		d := display(c)
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		definition, name, e := s.Repo.DisplayStudioDashboard(c.Context(), d, c.Params("id"))
		if e != nil {
			return e
		}
		var def struct {
			Panels []studio.Panel `json:"panels"`
		}
		if json.Unmarshal(definition, &def) != nil || len(def.Panels) > 16 {
			return domain.ErrInvalid
		}
		if e := logRead(c, d, "display_studio", "dashboard", c.Params("id")); e != nil {
			return e
		}
		impersonal, e := s.Repo.DisplayImpersonal(c.Context(), d)
		if e != nil {
			return e
		}
		rendered, e := renderStudioPanels(c.Context(), s, d.Principal(), def.Panels, 24*time.Hour, &displayMask{tenant: d.TenantID, showNames: d.ShowNames, impersonal: impersonal})
		if e != nil {
			return e
		}
		layout := make([]fiber.Map, 0, len(def.Panels))
		for _, panel := range def.Panels {
			layout = append(layout, fiber.Map{"id": panel.ID, "title": panel.Title, "width": panel.Width, "height": panel.Height})
		}
		return c.JSON(fiber.Map{"id": c.Params("id"), "name": name, "layout": layout, "panels": rendered})
	})
	kiosk.Post("/alerts/:id/ack", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.AckFromDisplay(c.Context(), display(c), c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}
