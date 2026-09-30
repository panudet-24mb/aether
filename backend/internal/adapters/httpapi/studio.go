package httpapi

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/studio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
		out, e := renderStudioPanels(c.Context(), s, p, in.Panels, window, nil)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"panels": out})
	})
}

// studioItem finds a widget or decoder: a bundled official one, or one of the workspace's.
func studioItem(ctx context.Context, s *app.Service, p domain.Principal, id string) (studio.Item, error) {
	for _, i := range studio.Official() {
		if i.ID == id {
			return i, nil
		}
	}
	if !security.ValidID(id) {
		return studio.Item{}, domain.ErrInvalid
	}
	return s.Repo.StudioGet(ctx, p, id)
}

// renderStudioPanels renders dashboard panels as p sees the data. A display (a wall TV) calls it with its own
// context and principal (docs/platform/display.md), so the same project scope narrows what the widgets read.
// mask is nil for members; for a display it strips member ids and notes and hides who wore a tag (display.go).
func renderStudioPanels(ctx context.Context, s *app.Service, p domain.Principal, panels []studio.Panel, window time.Duration, mask *displayMask) ([]fiber.Map, error) {
	out := []fiber.Map{}
	// Fetched once per render call and shared by all panels: the persisted event log and open alerts.
	events, e := s.Repo.ListEvents(ctx, p, "", 200)
	if e != nil {
		return nil, e
	}
	openAlerts, e := s.Repo.ListAlerts(ctx, p, "open", 20)
	if e != nil {
		return nil, e
	}
	// A demo workspace's readings are simulated by design (demo-twin); its dashboards show them as a real site's.
	demo, e := s.Repo.TenantDemo(ctx, p)
	if e != nil {
		return nil, e
	}
	presences := map[string]domain.Presence{}
	var names map[string]string
	for _, panel := range panels {
		result := fiber.Map{"id": panel.ID}
		out = append(out, result)
		if panel.WidgetID == studio.ControlsWidget {
			if names == nil {
				devices, err := s.Repo.ListDevices(ctx, p)
				if err != nil {
					return nil, err
				}
				names = make(map[string]string, len(devices))
				for _, d := range devices {
					names[d.ID] = d.Name
				}
			}
			if err := renderControlsPanel(ctx, s, p, panel, names, result); err != nil {
				return nil, err
			}
			continue
		}
		if !security.ValidID(panel.GatewayID) || !studio.ValidSource(panel.ExternalID) {
			result["error"] = "Invalid source"
			continue
		}
		widget, err := studioItem(ctx, s, p, panel.WidgetID)
		if err != nil || widget.Kind != "widget" {
			result["error"] = "Widget unavailable"
			continue
		}
		// Where the tag is heard now. A roaming wearable renders from the gateway that currently hears it best,
		// so one panel keeps working while the person walks between zones.
		presence, known := presences[strings.ToLower(panel.ExternalID)]
		if !known {
			presence, err = s.Repo.Presence(ctx, p, strings.ToLower(panel.ExternalID))
			if err != nil {
				return nil, err
			}
			presences[strings.ToLower(panel.ExternalID)] = presence
		}
		sourceGateway := panel.GatewayID
		if presence.Roaming && presence.Current != nil {
			sourceGateway = presence.Current.GatewayID
		} else if presence.Roaming && len(presence.Sightings) > 0 {
			sourceGateway = presence.Sightings[0].GatewayID // nobody hears it now: show the last place it was heard
		}
		streams, err := s.Repo.StreamHistory(ctx, p, sourceGateway, time.Now().Add(-window), 200)
		if err != nil {
			return nil, err
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
				decoder, err := studioItem(ctx, s, p, panel.DecoderID)
				if err != nil || decoder.Kind != "decoder" {
					result["error"] = "Decoder unavailable"
					continue
				}
				_ = json.Unmarshal(decoder.Definition, &d)
			}

			observations, err := s.Repo.BLEHistory(ctx, p, sourceGateway, panel.ExternalID, time.Now().Add(-window))
			if err != nil {
				return nil, err
			}
			history, err := studio.DecodeHistory(ctx, d.Code, observations)
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
		if mask != nil {
			input["events"], input["alerts"], input["presence"] = mask.events(deviceEvents), mask.alerts(openAlerts), mask.presence(presence)
		}
		input["now"] = time.Now().UTC()
		if demo && input["source"] == "simulated" {
			input["source"] = "device"
		}
		if demo && result["source"] == "simulated" {
			result["source"] = "device"
		}
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
			rendered, err := studio.Run(ctx, d.Code, "render", payload)
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
	return out, nil
}

// renderControlsPanel describes a controls panel: each device's name and what it can be set to (the app draws the
// switches itself from /devices/:id/controls), and a static rendering of their current state for the widget frame,
// which is all a wall display shows (read-only: a display can never command).
func renderControlsPanel(ctx context.Context, s *app.Service, p domain.Principal, panel studio.Panel, names map[string]string, result fiber.Map) error {
	if len(panel.Devices) == 0 || len(panel.Devices) > studio.MaxControls {
		result["error"] = "Invalid source"
		return nil
	}
	items := []fiber.Map{}
	var rows strings.Builder
	var newest time.Time
	for _, id := range panel.Devices {
		name, ok := names[id]
		if !ok || !security.ValidID(id) {
			continue // removed, or in a project this viewer cannot see
		}
		controls, err := s.Repo.DeviceControls(ctx, p, id)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		items = append(items, fiber.Map{"device_id": id, "name": name})
		var state map[string]any
		_ = json.Unmarshal(controls.State, &state)
		parts := []string{}
		gang := 0
		for _, f := range zigbee2mqtt.SettableFeatures(controls.Exposes) {
			if f.Type != "binary" || !(f.Group == "switch" || f.Name == "state" || strings.HasPrefix(f.Property, "switch")) {
				continue
			}
			gang++
			v, known := state[f.Property]
			word := "—"
			if known {
				word = "ปิด"
				if on, _ := json.Marshal(v); string(on) == string(f.ValueOn) {
					word = "<b>เปิด</b>"
				}
			}
			parts = append(parts, fmt.Sprintf("ช่อง %d %s", gang, word))
		}
		if len(parts) == 1 {
			parts[0] = strings.TrimPrefix(parts[0], "ช่อง 1 ")
		}
		if w, ok := state["power"].(float64); ok {
			parts = append(parts, fmt.Sprintf("%.0f W", w))
		}
		if pos, ok := state["position"].(float64); ok {
			parts = append(parts, fmt.Sprintf("เปิด %.0f%%", pos))
		}
		for _, k := range []string{"current_heating_setpoint", "temp_set"} {
			if t, ok := state[k].(float64); ok {
				parts = append(parts, fmt.Sprintf("ตั้ง %.1f °C", t))
			}
		}
		status := ""
		if !controls.Online {
			status = ` class="off"`
			parts = append(parts, "ขาดการติดต่อ")
		}
		if ts, ok := state["last_seen"].(string); ok {
			if at, e := time.Parse(time.RFC3339, ts); e == nil && at.After(newest) {
				newest = at
			}
		}
		fmt.Fprintf(&rows, "<li%s><span>%s</span><em>%s</em></li>", status, html.EscapeString(name), strings.Join(parts, " · "))
	}
	result["controls"] = items
	result["source"] = "device"
	if !newest.IsZero() {
		result["received_at"] = newest
	}
	if len(items) == 0 {
		result["error"] = "No controllable devices"
		return nil
	}
	result["html"] = "<ul class=\"controls\">" + rows.String() + "</ul>"
	result["css"] = `body{margin:0;padding:16px 18px;background:#0e1312;color:#e6edea;font-family:'Avenir Next','Segoe UI',Tahoma,system-ui,sans-serif}` +
		`.controls{list-style:none;margin:0;padding:0;display:grid;gap:6px;font-size:14px;line-height:1.45}` +
		`.controls li{display:flex;justify-content:space-between;gap:12px;padding:8px 11px;border-radius:8px;background:#121a18}` +
		`.controls em{font-style:normal;color:#93a7a2;white-space:nowrap}.controls b{color:#ffe39a;font-weight:600}.controls li.off{opacity:.55}`
	return nil
}
