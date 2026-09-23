package httpapi

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/studio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"html"
	"regexp"
	"strings"
	"time"
)

// compactHistory keeps only what widgets chart or compare (values, flags, beacon instance), so 200 points of a
// multi-frame tag stay inside the sandbox input budget.
func compactHistory(history []minew.Reading) []map[string]any {
	out := make([]map[string]any, 0, len(history))
	for _, r := range history {
		row := map[string]any{"received_at": r.ReceivedAt, "battery": r.Battery}
		if r.Kind == "" || r.Kind == minew.KindEnvironment {
			row["temperature"], row["humidity"] = r.Temperature, r.Humidity
		}
		if r.RSSI != nil {
			row["rssi"] = *r.RSSI
		}
		if len(r.Metrics) > 0 {
			metrics := map[string]float64{}
			for k, v := range r.Metrics {
				if k != "vibration_ts" && k != "accel_x" && k != "accel_y" && k != "accel_z" {
					metrics[k] = v
				}
			}
			row["metrics"] = metrics
		}
		if r.Beacon != nil && r.Beacon.Instance != "" {
			row["beacon"] = map[string]string{"instance": r.Beacon.Instance}
		}
		out = append(out, row)
	}
	return out
}

var binding = regexp.MustCompile(`\{\{([a-zA-Z0-9_]+)\}\}`)

func studioRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) (domain.Principal, error) {
		p := c.Locals("principal").(domain.Principal)
		if !p.CanManageDevices() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	get := func(c fiber.Ctx, p domain.Principal, id string) (studio.Item, error) {
		for _, i := range studio.Official() {
			if i.ID == id {
				return i, nil
			}
		}
		if !security.ValidID(id) {
			return studio.Item{}, domain.ErrInvalid
		}
		return s.Repo.StudioGet(c.Context(), p, id)
	}
	r.Get("/studio/sources", func(c fiber.Ctx) error {
		p, e := principal(c)
		if e != nil {
			return e
		}
		gateways, e := s.Repo.ListGateways(c.Context(), p)
		if e != nil {
			return e
		}
		out := []fiber.Map{}
		for _, g := range gateways {
			packets, e := s.Repo.ListPackets(c.Context(), p, g.ID)
			if e != nil {
				return e
			}
			seen := map[string]bool{}
			for _, packet := range packets {
				rows, ok := minew.ParseRows(packet.Payload)
				if !ok {
					continue
				}
				for _, row := range rows {
					mac := strings.ToLower(row.MAC)
					b, e := hex.DecodeString(mac)
					if e != nil || len(b) != 6 || row.Raw == "" || seen[mac] || len(seen) >= 100 {
						continue
					}
					seen[mac] = true
					name := mac
					if row.Source == "simulated" {
						name = "SIM / " + mac
					}
					out = append(out, fiber.Map{"key": g.ID + "/" + mac, "id": mac, "name": name, "gatewayID": g.ID, "gatewayName": g.Name})
				}
			}
		}
		return c.JSON(fiber.Map{"items": out})
	})
	r.Get("/studio/items", func(c fiber.Ctx) error {
		p, e := principal(c)
		if e != nil {
			return e
		}
		kind := c.Query("kind")
		if kind != "widget" && kind != "decoder" && kind != "dashboard" {
			return domain.ErrInvalid
		}
		items, e := s.Repo.StudioList(c.Context(), p, kind)
		if e != nil {
			return e
		}
		for _, i := range studio.Official() {
			if i.Kind == kind {
				items = append(items, i)
			}
		}
		return c.JSON(fiber.Map{"items": items})
	})
	r.Post("/studio/items", func(c fiber.Ctx) error {
		p, e := principal(c)
		if e != nil {
			return e
		}
		var i studio.Item
		if e = body(c, &i, 70000); e != nil {
			return e
		}
		if studio.Validate(i) != nil {
			return domain.ErrInvalid
		}
		i.ID = uuid.NewString()
		i.TenantID = p.TenantID
		i.Official = false
		i.Revision = 1
		if e = s.Repo.StudioCreate(c.Context(), p, i); e != nil {
			return e
		}
		return c.Status(201).JSON(i)
	})
	r.Post("/studio/items/:id/save", func(c fiber.Ctx) error {
		p, e := principal(c)
		if e != nil {
			return e
		}
		var i studio.Item
		if e = body(c, &i, 70000); e != nil {
			return e
		}
		i.ID = c.Params("id")
		if !security.ValidID(i.ID) || i.Kind != "dashboard" || i.Revision < 1 || studio.Validate(i) != nil {
			return domain.ErrInvalid
		}
		if e = s.Repo.StudioSave(c.Context(), p, i); e != nil {
			return e
		}
		i.Revision++
		return c.JSON(i)
	})
	r.Post("/studio/items/:id/delete", func(c fiber.Ctx) error {
		p, e := principal(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e = s.Repo.StudioDelete(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	r.Post("/studio/test", func(c fiber.Ctx) error {
		if _, e := principal(c); e != nil {
			return e
		}
		var in struct {
			Code       string          `json:"code"`
			Function   string          `json:"function"`
			Input      json.RawMessage `json:"input"`
			HTML       string          `json:"html"`
			DecodeCode string          `json:"decode_code"`
		}
		if e := body(c, &in, 90000); e != nil {
			return e
		}
		if in.DecodeCode != "" {
			decoded, err := studio.Run(c.Context(), in.DecodeCode, "decodeUplink", in.Input)
			if err != nil {
				return fiber.NewError(422)
			}
			var value struct {
				Data map[string]any `json:"data"`
			}
			if json.Unmarshal(decoded, &value) != nil || value.Data == nil {
				return fiber.NewError(422)
			}
			in.Input, _ = json.Marshal(fiber.Map{"data": value.Data, "history": []any{}, "source": "simulated"})
		}
		out, e := studio.Run(c.Context(), in.Code, in.Function, in.Input)
		if e != nil {
			return fiber.NewError(422)
		}
		var fields map[string]any
		_ = json.Unmarshal(out, &fields)
		markup := in.HTML
		if custom, ok := fields["html"].(string); ok {
			markup = custom
		} else {
			markup = binding.ReplaceAllStringFunc(markup, func(key string) string {
				v := fields[key[2:len(key)-2]]
				if v == nil {
					return "—"
				}
				return html.EscapeString(fmt.Sprint(v))
			})
		}
		return c.JSON(fiber.Map{"result": out, "html": studio.SafeHTML(markup)})
	})
	r.Post("/studio/render", func(c fiber.Ctx) error {
		p, e := principal(c)
		if e != nil {
			return e
		}
		var in struct {
			Panels []studio.Panel `json:"panels"`
			Range  string         `json:"range"`
		}
		if e = body(c, &in, 16000); e != nil {
			return e
		}
		if len(in.Panels) > 16 {
			return domain.ErrInvalid
		}
		window := 24 * time.Hour
		switch in.Range {
		case "", "24h":
		case "1h":
			window = time.Hour
		case "7d":
			window = 7 * 24 * time.Hour
		default:
			return domain.ErrInvalid
		}
		out := []fiber.Map{}
		// Fetched once per render call and shared by all panels: the persisted event log and open alerts.
		events, e := s.Repo.ListEvents(c.Context(), p, "", 200)
		if e != nil {
			return e
		}
		openAlerts, e := s.Repo.ListAlerts(c.Context(), p, "open", 20)
		if e != nil {
			return e
		}
		presences := map[string]domain.Presence{}
		for _, panel := range in.Panels {
			result := fiber.Map{"id": panel.ID}
			out = append(out, result)
			if !security.ValidID(panel.GatewayID) || len(panel.ExternalID) != 12 {
				result["error"] = "Invalid source"
				continue
			}
			widget, err := get(c, p, panel.WidgetID)
			if err != nil || widget.Kind != "widget" {
				result["error"] = "Widget unavailable"
				continue
			}
			// Where the tag is heard now. A roaming wearable renders from the gateway that currently hears it best,
			// so one panel keeps working while the person walks between zones.
			presence, known := presences[strings.ToLower(panel.ExternalID)]
			if !known {
				presence, err = s.Repo.Presence(c.Context(), p, strings.ToLower(panel.ExternalID))
				if err != nil {
					return err
				}
				presences[strings.ToLower(panel.ExternalID)] = presence
			}
			sourceGateway := panel.GatewayID
			if presence.Roaming && presence.Current != nil {
				sourceGateway = presence.Current.GatewayID
			} else if presence.Roaming && len(presence.Sightings) > 0 {
				sourceGateway = presence.Sightings[0].GatewayID // nobody hears it now: show the last place it was heard
			}
			streams, err := s.Repo.StreamHistory(c.Context(), p, sourceGateway, time.Now().Add(-window), 200)
			if err != nil {
				return err
			}
			var input map[string]any
			for _, sensor := range streams {
				if strings.EqualFold(sensor.ID, panel.ExternalID) {
					input = map[string]any{"data": sensor.Latest, "history": compactHistory(sensor.History), "source": sensor.Latest.Source}
					result["received_at"] = sensor.Latest.ReceivedAt
					result["source"] = sensor.Latest.Source
					result["gateway_id"] = sourceGateway
					break
				}
			}
			var widgetDefinition studio.Definition
			_ = json.Unmarshal(widget.Definition, &widgetDefinition)
			if input == nil && panel.DecoderID == "" && widgetDefinition.DecodeCode == "" {
				result["error"] = "No received sensor data"
				continue
			}
			if input == nil {
				input = map[string]any{"data": map[string]any{}, "history": []any{}, "source": "device"}
			}
			if panel.DecoderID != "" || widgetDefinition.DecodeCode != "" {
				d := studio.Definition{Code: widgetDefinition.DecodeCode}
				// Preserve existing saved panel overrides; new panels use the bundled decoder.
				if panel.DecoderID != "" {
					decoder, err := get(c, p, panel.DecoderID)
					if err != nil || decoder.Kind != "decoder" {
						result["error"] = "Decoder unavailable"
						continue
					}
					_ = json.Unmarshal(decoder.Definition, &d)
				}

				observations, err := s.Repo.BLEHistory(c.Context(), p, sourceGateway, panel.ExternalID, time.Now().Add(-window))
				if err != nil {
					return err
				}
				history, err := studio.DecodeHistory(c.Context(), d.Code, observations)
				if err != nil {
					result["error"] = "No decodable history in this range, or decoder exceeded limits"
					continue
				}
				input["data"] = history.Data
				input["history"] = history.History
				input["source"] = history.Source
				result["source"] = history.Source
				result["received_at"] = history.ReceivedAt
				result["history_points"] = len(history.History)
				result["skipped_frames"] = history.Skipped

			}
			deviceEvents := []domain.DeviceEvent{}
			for _, ev := range events {
				if strings.EqualFold(ev.ExternalID, panel.ExternalID) && len(deviceEvents) < 20 {
					deviceEvents = append(deviceEvents, ev)
				}
			}
			input["events"] = deviceEvents
			input["alerts"] = openAlerts
			input["presence"] = presence
			input["now"] = time.Now().UTC()
			var d studio.Definition
			_ = json.Unmarshal(widget.Definition, &d)
			markup := d.HTML
			if d.Code != "" {
				payload, _ := json.Marshal(input)
				// The sandbox accepts 64 KiB; keep the newest history that fits instead of failing the panel.
				for len(payload) > 60000 {
					h, ok := input["history"].([]map[string]any)
					if !ok || len(h) < 2 {
						break
					}
					input["history"] = h[len(h)/2:]
					payload, _ = json.Marshal(input)
				}
				rendered, err := studio.Run(c.Context(), d.Code, "render", payload)
				if err != nil {
					result["error"] = "Widget failed or exceeded limits"
					continue
				}
				var fields map[string]any
				_ = json.Unmarshal(rendered, &fields)
				if custom, ok := fields["html"].(string); ok {
					markup = custom
				} else {
					markup = binding.ReplaceAllStringFunc(markup, func(key string) string {
						v := fields[key[2:len(key)-2]]
						if v == nil {
							return "—"
						}
						return html.EscapeString(fmt.Sprint(v))
					})
				}
			}
			result["html"] = studio.SafeHTML(markup)
			result["css"] = d.CSS
		}
		return c.JSON(fiber.Map{"panels": out})
	})
}
