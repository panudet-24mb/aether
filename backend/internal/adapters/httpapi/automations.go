package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/automation"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// bodyLimit for a flow: the 32 KiB definition plus its envelope.
const automationBodyLimit = automation.MaxDefinitionBytes + 4096

func automationRoutes(r fiber.Router, s *app.Service, commandsEnabled, alertsShadow bool) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	manage := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanManageDevices() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	id := func(c fiber.Ctx) (string, error) {
		if !security.ValidID(c.Params("id")) {
			return "", domain.ErrInvalid
		}
		return c.Params("id"), nil
	}

	commandNote := "สั่งอุปกรณ์ Zigbee2MQTT ที่ลงทะเบียนแล้วได้ · ใช้คิวคำสั่งเดียวกับหน้าเว็บ (ตรวจค่า, ทีละคำสั่งต่อค่า, จำกัดความถี่)"
	if !commandsEnabled {
		commandNote = "ระบบนี้ปิดการสั่งอุปกรณ์จากผังอัตโนมัติไว้ (AUTOMATION_COMMANDS=false) · วางบล็อกและบันทึกเป็นฉบับร่างได้ แต่เปิดใช้ผังที่มีบล็อกนี้ไม่ได้"
	}
	shadowNote := ""
	if alertsShadow {
		shadowNote = "ระบบอยู่ในโหมดเงา (ALERTS_SHADOW) · ผังอัตโนมัติทั้งหมด รวมทั้งการสั่งอุปกรณ์ จะยังไม่ทำงานจนกว่าจะปิดโหมดเงา"
	}
	// catalogue is everything the studio needs to render its palette and inspector pickers. can_command is per
	// member, so it is added per request.
	catalogue := func(mayCommand bool) fiber.Map {
		return fiber.Map{
			"node_types": automation.NodeTypes, "event_types": automation.EventTypes, "ops": automation.Ops,
			"severities": automation.Severities, "max_nodes": automation.MaxNodes, "max_edges": automation.MaxEdges,
			"timezone":            "Asia/Bangkok",
			"command_available":   commandsEnabled,
			"automation_commands": commandsEnabled,
			"alerts_shadow":       alertsShadow,
			"can_command":         mayCommand,
			"command_note":        commandNote,
			"shadow_note":         shadowNote,
		}
	}

	r.Get("/automations", func(c fiber.Ctx) error {
		p := principal(c)
		out, e := s.Repo.ListAutomations(c.Context(), p)
		if e != nil {
			return e
		}
		access, e := s.Repo.MemberAccess(c.Context(), p, p.UserID)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "catalog": catalogue(p.MayControl(access))})
	})
	// commandable feeds the "สั่งอุปกรณ์" block's pickers: the devices a flow of this project may command and the
	// properties each one lets Aether set. Registered before /automations/:id so the path is not read as an id.
	r.Get("/automations/commandable", func(c fiber.Ctx) error {
		var project *string
		if v := c.Query("project_id"); v != "" {
			if !security.ValidID(v) {
				return domain.ErrInvalid
			}
			project = &v
		}
		out, e := s.Repo.CommandableDevices(c.Context(), principal(c), project)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	r.Get("/automations/:id", func(c fiber.Ctx) error {
		target, e := id(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.GetAutomation(c.Context(), principal(c), target)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	r.Get("/automations/:id/runs", func(c fiber.Ctx) error {
		target, e := id(c)
		if e != nil {
			return e
		}
		n, e := security.ParseLimit(c.Query("limit"), 50)
		if e != nil || n > 200 {
			return domain.ErrInvalid
		}
		out, e := s.Repo.ListAutomationRuns(c.Context(), principal(c), target, n)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})

	type input struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Enabled     *bool           `json:"enabled"`
		ProjectID   *string         `json:"project_id"`
		Definition  json.RawMessage `json:"definition"`
		Revision    *int            `json:"revision"`
	}
	// parse normalises and bounds the envelope; the block graph itself is checked by automation.Check.
	parse := func(c fiber.Ctx) (input, automation.Definition, error) {
		var in input
		var def automation.Definition
		if e := body(c, &in, automationBodyLimit); e != nil {
			return in, def, e
		}
		in.Name = strings.TrimSpace(in.Name)
		in.Description = strings.TrimSpace(in.Description)
		if in.Name == "" || utf8.RuneCountInString(in.Name) > 120 || utf8.RuneCountInString(in.Description) > 500 {
			return in, def, domain.ErrInvalid
		}
		if in.ProjectID != nil && !security.ValidID(*in.ProjectID) {
			return in, def, domain.ErrInvalid
		}
		if len(in.Definition) == 0 {
			in.Definition = json.RawMessage(`{"nodes":[],"edges":[]}`)
		}
		if len(in.Definition) > automation.MaxDefinitionBytes {
			return in, def, domain.ErrInvalid
		}
		if e := json.Unmarshal(in.Definition, &def); e != nil {
			return in, def, domain.ErrInvalid
		}
		return in, def, nil
	}
	// reject answers a bad definition with the per-block problems so the studio can outline the blocks.
	reject := func(c fiber.Ctx, problems []automation.Problem) error {
		return c.Status(400).JSON(fiber.Map{"error": "invalid_input", "problems": problems, "request_id": c.GetRespHeader("X-Request-ID")})
	}

	r.Post("/automations", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		in, def, e := parse(c)
		if e != nil {
			return e
		}
		enabled := in.Enabled != nil && *in.Enabled
		if enabled {
			opts, e := s.Repo.AutomationOptions(c.Context(), p, in.ProjectID, in.Definition, true)
			if e != nil {
				return e
			}
			if problems := automation.Check(def, opts); len(problems) > 0 {
				return reject(c, problems)
			}
		}
		record := automation.Automation{ID: uuid.NewString(), Name: in.Name, Description: in.Description, Enabled: enabled, ProjectID: in.ProjectID, Definition: in.Definition, Revision: 1}
		if e := s.Repo.CreateAutomation(c.Context(), p, record); e != nil {
			return e
		}
		out, e := s.Repo.GetAutomation(c.Context(), p, record.ID)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})

	// save carries the revision the editor started from; a stale one is a 409, never an overwrite.
	r.Post("/automations/:id/save", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		target, e := id(c)
		if e != nil {
			return e
		}
		in, def, e := parse(c)
		if e != nil {
			return e
		}
		if in.Revision == nil || *in.Revision < 1 {
			return domain.ErrInvalid
		}
		enabled := in.Enabled != nil && *in.Enabled
		if enabled {
			opts, e := s.Repo.AutomationOptions(c.Context(), p, in.ProjectID, in.Definition, true)
			if e != nil {
				return e
			}
			if problems := automation.Check(def, opts); len(problems) > 0 {
				return reject(c, problems)
			}
		}
		record := automation.Automation{ID: target, Name: in.Name, Description: in.Description, Enabled: enabled, ProjectID: in.ProjectID, Definition: in.Definition, Revision: *in.Revision}
		if e := s.Repo.SaveAutomation(c.Context(), p, record); e != nil {
			return e
		}
		out, e := s.Repo.GetAutomation(c.Context(), p, target)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})

	r.Post("/automations/:id/enable", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		target, e := id(c)
		if e != nil {
			return e
		}
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		if in.Enabled == nil {
			return domain.ErrInvalid
		}
		if *in.Enabled {
			// Report why it cannot be switched on, block by block, instead of a bare 400.
			record, e := s.Repo.GetAutomation(c.Context(), p, target)
			if e != nil {
				return e
			}
			opts, e := s.Repo.AutomationOptions(c.Context(), p, record.ProjectID, record.Definition, true)
			if e != nil {
				return e
			}
			var def automation.Definition
			if e := json.Unmarshal(record.Definition, &def); e != nil {
				return domain.ErrInvalid
			}
			if problems := automation.Check(def, opts); len(problems) > 0 {
				return reject(c, problems)
			}
		}
		if e := s.Repo.SetAutomationEnabled(c.Context(), p, target, *in.Enabled); e != nil {
			return e
		}
		out, e := s.Repo.GetAutomation(c.Context(), p, target)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})

	r.Post("/automations/:id/delete", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		target, e := id(c)
		if e != nil {
			return e
		}
		if e := s.Repo.DeleteAutomation(c.Context(), p, target); e != nil {
			return e
		}
		return c.SendStatus(204)
	})

	// validate is the studio's live check: it never writes, and answers with one problem per block.
	r.Post("/automations/validate", func(c fiber.Ctx) error {
		p := principal(c)
		var in struct {
			Enabled    *bool           `json:"enabled"`
			ProjectID  *string         `json:"project_id"`
			Definition json.RawMessage `json:"definition"`
		}
		if e := body(c, &in, automationBodyLimit); e != nil {
			return e
		}
		if len(in.Definition) > automation.MaxDefinitionBytes {
			return domain.ErrInvalid
		}
		var def automation.Definition
		if len(in.Definition) > 0 && json.Unmarshal(in.Definition, &def) != nil {
			return domain.ErrInvalid
		}
		if in.ProjectID != nil && !security.ValidID(*in.ProjectID) {
			return domain.ErrInvalid
		}
		enabled := in.Enabled != nil && *in.Enabled
		opts, e := s.Repo.AutomationOptions(c.Context(), p, in.ProjectID, in.Definition, enabled)
		if e != nil {
			return e
		}
		problems := automation.Check(def, opts)
		return c.JSON(fiber.Map{"ok": len(problems) == 0, "problems": problems})
	})

	// test is a dry run against the tenant's real current readings: it returns the per-block trace and
	// executes nothing, so no alert is opened and no notification is queued.
	r.Post("/automations/:id/test", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		target, e := id(c)
		if e != nil {
			return e
		}
		var in struct {
			ExternalID string   `json:"external_id"`
			GatewayID  string   `json:"gateway_id"`
			EventType  string   `json:"event_type"`
			Action     string   `json:"action"`
			Metric     *float64 `json:"metric_value"`
		}
		if e := body(c, &in, 4096); e != nil {
			return e
		}
		in.ExternalID = strings.ToLower(strings.TrimSpace(in.ExternalID))
		if in.ExternalID == "" || len(in.ExternalID) > 128 || strings.ContainsAny(in.ExternalID, "\r\n\x00") {
			return domain.ErrInvalid
		}
		if in.GatewayID != "" && !security.ValidID(in.GatewayID) {
			return domain.ErrInvalid
		}
		if in.EventType != "" && !containsString(automation.EventTypes, in.EventType) {
			return domain.ErrInvalid
		}
		if in.EventType == "" && in.Metric == nil {
			return domain.ErrInvalid
		}
		if len(in.Action) > 64 || strings.ContainsAny(in.Action, "\r\n\x00") {
			return domain.ErrInvalid
		}
		result, e := s.Repo.TestAutomation(c.Context(), p, target, automation.TestInput{ExternalID: in.ExternalID, GatewayID: in.GatewayID, EventType: in.EventType, Action: in.Action, Value: in.Metric})
		if e != nil {
			return e
		}
		// A dry run never queues a command: would_send lists what the command blocks decided, nothing more.
		wouldSend := []automation.Action{}
		for _, act := range result.Actions {
			if act.Type == automation.ActionCommand {
				wouldSend = append(wouldSend, act)
			}
		}
		return c.JSON(fiber.Map{"dry_run": true, "executed": false, "result": result, "would_send": wouldSend})
	})
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
