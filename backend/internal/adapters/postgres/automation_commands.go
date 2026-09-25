package postgres

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/automation"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Automations that command devices (docs/platform/automation.md, "สั่งอุปกรณ์").
//
// An action.command block goes through the very same queue as a click in the web UI (queueCommand): the same
// validation against the device's own Zigbee2MQTT definition, one command in flight per property, the
// workspace budget, the commander's at-most-once delivery. On top of that an automation gets three rules a
// person does not need:
//   - the deployment switch AUTOMATION_COMMANDS (off by default) — off, nothing is ever queued;
//   - the loop guard — a flow woken by a switch change that an automation's own command caused sends no command,
//     so two flows cannot ping-pong a relay;
//   - a per-device cap of domain.AutomationCommandsPerDevice a minute across all flows.

// commandOutcome is what the run log records for one action.command block.
type commandOutcome struct {
	NodeID    string          `json:"node_id"`
	DeviceID  string          `json:"device_id"`
	Property  string          `json:"property"`
	Value     json.RawMessage `json:"value,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	CommandID string          `json:"command_id,omitempty"`
	// Status is "requested" (in the outbox, mqtt-commander decides), "queued" (the command is in
	// core.device_commands) or "blocked" (Reason says why). ListAutomationRuns resolves "requested" to the outcome.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// commandTargets returns, for the given device ids, the definition of each registered actuator the flow may
// command: a live registration on a Zigbee2MQTT gateway or an Aether Edge that is not revoked, known to its agent
// (paired with the coordinator, or imported with its Tuya specification), and inside the flow's project when it
// has one. Devices that do not qualify are simply absent.
func commandTargets(tx *gorm.DB, project *string, ids []string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(ids) == 0 {
		return out, nil
	}
	query := `SELECT d.id::text AS device_id,d.profile_id,z.exposes FROM core.devices d
    JOIN core.gateways g ON g.tenant_id=d.tenant_id AND g.id=d.gateway_id AND g.revoked_at IS NULL AND g.model IN ?
    JOIN core.command_targets z ON z.tenant_id=d.tenant_id AND z.gateway_id=d.gateway_id AND z.external_id=lower(d.external_id)
    WHERE d.removed_at IS NULL AND d.id::text IN ?`
	args := []any{commandGatewayModels, ids}
	if project != nil {
		query += ` AND g.project_id=?`
		args = append(args, *project)
	}
	var rows []struct {
		DeviceID, ProfileID string
		Exposes             json.RawMessage
	}
	if e := tx.Raw(query+` LIMIT 100`, args...).Scan(&rows).Error; e != nil {
		return nil, e
	}
	for _, row := range rows {
		if profile := domain.DeviceProfileByID(row.ProfileID); profile == nil || !profile.Actuator {
			continue
		}
		out[strings.ToLower(row.DeviceID)] = row.Exposes
	}
	return out, nil
}

// commandDeviceIDs lists the (lower-case, well-formed) devices a definition's command blocks point at.
func commandDeviceIDs(def automation.Definition) []string {
	out, seen := []string{}, map[string]bool{}
	for _, n := range def.Nodes {
		if n.Type != automation.ActionCommand {
			continue
		}
		id := strings.ToLower(n.Data.DeviceID)
		if len(id) == 36 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// mayControl is domain.Principal.MayControl read inside the caller's transaction.
func mayControl(tx *gorm.DB, p domain.Principal) (bool, error) {
	if !p.CanCommandDevices() {
		return false, nil
	}
	if p.Role == "owner" {
		return true, nil
	}
	var rows []struct{ Permissions string }
	if e := tx.Raw(`SELECT permissions::text FROM core.member_access WHERE tenant_id=? AND user_id=?`, p.TenantID, p.UserID).Scan(&rows).Error; e != nil {
		return false, e
	}
	access := map[string]string{}
	if len(rows) > 0 {
		if e := json.Unmarshal([]byte(rows[0].Permissions), &access); e != nil {
			return false, e
		}
	}
	return p.MayControl(access), nil
}

// checkOptions assembles everything automation.Check needs about the world for one definition: the tenant's
// channels, the devices its command blocks may target (with a validator backed by their definitions), the
// deployment switch and, when p is given, whether that member may control devices.
func (r *Repository) checkOptions(tx *gorm.DB, p *domain.Principal, project *string, def automation.Definition, enabled bool) (automation.Options, error) {
	opts := automation.Options{Channels: map[string]bool{}, Enabled: enabled, CommandsEnabled: r.opts.AutomationCommands}
	if e := channelSet(tx, opts.Channels); e != nil {
		return opts, e
	}
	if !automation.HasCommand(def) {
		return opts, nil
	}
	targets, e := commandTargets(tx, project, commandDeviceIDs(def))
	if e != nil {
		return opts, e
	}
	opts.Commandable = map[string]map[string]bool{}
	for id, exposes := range targets {
		props := map[string]bool{}
		for _, f := range zigbee2mqtt.SettableFeatures(exposes) {
			props[f.Property] = true
		}
		opts.Commandable[id] = props
	}
	opts.CheckValue = func(device, property string, value json.RawMessage) string {
		if _, e := zigbee2mqtt.Validate(targets[device], property, value); e != nil {
			var reason domain.ReasonError
			if errors.As(e, &reason) {
				return reason.Reason
			}
			return "value"
		}
		return ""
	}
	if p != nil {
		allowed, e := mayControl(tx, *p)
		if e != nil {
			return opts, e
		}
		opts.MayCommand = &allowed
	}
	return opts, nil
}

// AutomationOptions is checkOptions for the HTTP layer, so the studio gets the same per-block problems the
// repository enforces.
func (r *Repository) AutomationOptions(ctx context.Context, p domain.Principal, project *string, raw json.RawMessage, enabled bool) (automation.Options, error) {
	var out automation.Options
	var def automation.Definition
	if len(raw) > 0 {
		if e := json.Unmarshal(raw, &def); e != nil {
			return out, domain.ErrInvalid
		}
	}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var e error
		out, e = r.checkOptions(tx, &p, project, def, enabled)
		return e
	})
	return out, e
}

// CommandableDevices lists the devices an automation in `project` (nil = workspace-wide) may command, with the
// properties each one lets Aether set, for the studio's device picker.
func (r *Repository) CommandableDevices(ctx context.Context, p domain.Principal, project *string) ([]domain.CommandableDevice, error) {
	out := []domain.CommandableDevice{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		query := `SELECT d.id::text AS device_id,d.name,lower(d.external_id) AS external_id,d.gateway_id::text AS gateway_id,d.profile_id,z.exposes,coalesce(z.category,'') AS category
    FROM core.devices d
    JOIN core.gateways g ON g.tenant_id=d.tenant_id AND g.id=d.gateway_id AND g.revoked_at IS NULL AND g.model IN ?
    JOIN core.command_targets z ON z.tenant_id=d.tenant_id AND z.gateway_id=d.gateway_id AND z.external_id=lower(d.external_id)
    WHERE d.removed_at IS NULL`
		args := []any{commandGatewayModels}
		if project != nil {
			query += ` AND g.project_id=?`
			args = append(args, *project)
		}
		var rows []struct {
			DeviceID, Name, ExternalID, GatewayID, ProfileID, Category string
			Exposes                                                    json.RawMessage
		}
		if e := tx.Raw(query+` ORDER BY d.name,d.id LIMIT 200`, args...).Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			if profile := domain.DeviceProfileByID(row.ProfileID); profile == nil || !profile.Actuator {
				continue
			}
			features := zigbee2mqtt.SettableFeatures(row.Exposes)
			if len(features) == 0 {
				continue
			}
			raw, e := json.Marshal(features)
			if e != nil {
				return e
			}
			out = append(out, domain.CommandableDevice{DeviceID: row.DeviceID, Name: row.Name, ExternalID: row.ExternalID, GatewayID: row.GatewayID, Category: row.Category, Features: raw})
		}
		return nil
	})
	return out, e
}

// Lock order (the reason for the outbox, migration 00031). Every path that queues a command takes the per-tenant
// command lock (advisory lock hashtextextended(tenant,9)) FIRST and only then touches gateway rows (the foreign keys
// of core.device_commands take FOR KEY SHARE on the target's gateway and device). Ingest already holds its gateway
// row FOR UPDATE when a flow fires, so it must never take the tenant lock nor a key lock on another gateway: it only
// checks what needs no lock and inserts a row into core.automation_command_requests (no foreign keys). mqtt-commander
// drains those rows (drainCommandRequests) in its own transaction, lock first, exactly like a command from the web.

// commandAuthority re-checks, at the moment it matters, that a command block may still act: the device is still
// registered, on a gateway of the flow's project for a project flow, and the member who armed the flow may still
// command a device on that gateway. It returns the target's gateway, or a refusal reason.
func commandAuthority(tx *gorm.DB, deviceID string, project, actor *string) (string, string, error) {
	var rows []struct {
		GatewayID string
		ProjectID *string
	}
	if e := tx.Raw(`SELECT d.gateway_id::text AS gateway_id,g.project_id::text AS project_id FROM core.devices d
    JOIN core.gateways g ON g.tenant_id=d.tenant_id AND g.id=d.gateway_id AND g.revoked_at IS NULL
    WHERE d.id::text=lower(?) AND d.removed_at IS NULL`, deviceID).Scan(&rows).Error; e != nil {
		return "", "", e
	}
	if len(rows) != 1 {
		return "", "not_found", nil
	}
	target := rows[0]
	if project != nil && (target.ProjectID == nil || *target.ProjectID != *project) {
		return "", "out_of_project", nil
	}
	if actor == nil {
		return "", "arming_member_lacks_control", nil
	}
	var may []bool
	if e := tx.Raw(`SELECT core.member_may_command(?::uuid,?::uuid)`, *actor, target.GatewayID).Scan(&may).Error; e != nil {
		return "", "", e
	}
	if len(may) != 1 || !may[0] {
		return "", "arming_member_lacks_control", nil
	}
	return target.GatewayID, "", nil
}

// requestCommand is what a firing flow does with one action.command decision, inside the ingest transaction. Only
// lock-free checks happen here; a refusal is the outcome, recorded in the run log, never an error. Only a database
// failure is returned.
func requestCommand(tx *gorm.DB, tenant string, flow flowRow, act automation.Action, f *firing, opts Options, at time.Time) (commandOutcome, error) {
	out := commandOutcome{NodeID: act.NodeID, DeviceID: act.DeviceID, Property: act.Property, Value: act.Value, Status: "blocked"}
	if !opts.AutomationCommands {
		out.Reason = "commands_disabled"
		return out, nil
	}
	// Loop guard: the change that woke this flow was caused by a command an automation sent (any property, event or
	// metric trigger alike), so answering it with another command could ping-pong between flows.
	if len(f.commandIDs) > 0 {
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.device_commands WHERE id::text IN ? AND source='automation'`, f.commandIDs).Scan(&n).Error; e != nil {
			return out, e
		}
		if n > 0 {
			out.Reason = "loop_guard"
			return out, nil
		}
	}
	_, reason, e := commandAuthority(tx, act.DeviceID, flow.ProjectID, flow.EnabledBy)
	if e != nil || reason != "" {
		out.Reason = reason
		return out, e
	}
	id := uuid.NewString()
	if e := tx.Exec(`INSERT INTO core.automation_command_requests(tenant_id,id,automation_id,run_node,device_id,property,value,project_id,actor_id,created_at)
    VALUES(?,?,?,?,?,?,?::jsonb,?,?,?)`, tenant, id, flow.ID, act.NodeID, strings.ToLower(act.DeviceID), act.Property, string(act.Value), flow.ProjectID, flow.EnabledBy, at).Error; e != nil {
		return out, e
	}
	if e := tx.Exec(`SELECT pg_notify(?,?)`, CommandChannel, tenant).Error; e != nil {
		return out, e
	}
	out.Status, out.RequestID = "requested", id
	return out, nil
}

// ProcessAutomationRequests turns the tenant's pending automation command requests into commands (mqtt-commander,
// once per sweep before claiming). It takes the tenant command lock before anything else, so it follows the one lock
// order of the command path, and applies under that lock — atomically with the insert — the automation budgets: at
// most domain.AutomationCommandsPerDevice per device and domain.AutomationCommandsPerMinute per workspace a minute.
// It returns how many were queued.
func (r *Repository) ProcessAutomationRequests(ctx context.Context, tenant string, now time.Time) (int, error) {
	queued := 0
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,9))`, tenant).Error; e != nil {
			return e
		}
		var rows []struct {
			ID, AutomationID, DeviceID, Property string
			Value                                json.RawMessage
			ProjectID, ActorID                   *string
			CreatedAt                            time.Time
		}
		if e := tx.Raw(`SELECT id::text AS id,automation_id::text AS automation_id,device_id::text AS device_id,property,value,project_id::text AS project_id,actor_id::text AS actor_id,created_at
      FROM core.automation_command_requests WHERE status='requested' ORDER BY created_at,id LIMIT 50 FOR UPDATE SKIP LOCKED`).Scan(&rows).Error; e != nil {
			return e
		}
		for _, q := range rows {
			settle := func(status, reason, command string) error {
				var cmd any
				if command != "" {
					cmd = command
				}
				return tx.Exec(`UPDATE core.automation_command_requests SET status=?,reason=?,command_id=?,settled_at=? WHERE id=?`, status, reason, cmd, now, q.ID).Error
			}
			if now.Sub(q.CreatedAt) > domain.CommandTTL {
				if e := settle("blocked", "expired", ""); e != nil {
					return e
				}
				continue
			}
			// Authority and project are re-checked here as well: they may have changed since the flow fired.
			if _, reason, e := commandAuthority(tx, q.DeviceID, q.ProjectID, q.ActorID); e != nil || reason != "" {
				if e != nil {
					return e
				}
				if e := settle("blocked", reason, ""); e != nil {
					return e
				}
				continue
			}
			var device, workspace int64
			if e := tx.Raw(`SELECT count(*) FILTER (WHERE device_id::text=?),count(*) FROM core.device_commands WHERE source='automation' AND created_at>?`,
				q.DeviceID, now.Add(-time.Minute)).Row().Scan(&device, &workspace); e != nil {
				return e
			}
			if device >= domain.AutomationCommandsPerDevice {
				if e := settle("blocked", "automation_cap", ""); e != nil {
					return e
				}
				continue
			}
			if workspace >= domain.AutomationCommandsPerMinute {
				if e := settle("blocked", "automation_budget", ""); e != nil {
					return e
				}
				continue
			}
			// A refused command must not abort the other requests, so it gets a savepoint. queueCommand takes the
			// tenant lock again, which this transaction already holds (advisory locks are re-entrant).
			if e := tx.SavePoint("automation_request").Error; e != nil {
				return e
			}
			req := domain.CommandRequest{ID: q.ID, DeviceID: q.DeviceID, Property: q.Property, Action: "set", Value: q.Value}
			flow := q.AutomationID
			cmd, _, e := queueCommand(tx, tenant, req, "automation", q.ActorID, &flow, now)
			if e != nil {
				if rb := tx.RollbackTo("automation_request").Error; rb != nil {
					return rb
				}
				reason := refusalReason(e)
				if reason == "" {
					return e
				}
				if e := settle("blocked", reason, ""); e != nil {
					return e
				}
				continue
			}
			if e := settle("queued", "", cmd.ID); e != nil {
				return e
			}
			queued++
		}
		return tx.Exec(`DELETE FROM core.automation_command_requests WHERE status<>'requested' AND created_at<?`, now.Add(-7*24*time.Hour)).Error
	})
	return queued, e
}

// disarmUnauthorisedFlows switches off the enabled flows with device commands whose armer (enabled_by) may no longer
// command one of their targets — demoted, removed, the control module closed, or moved out of the target's
// project — so the studio shows them off instead of silently refusing at every firing. `member` narrows it to one
// armer; nil checks every armer (a gateway moved between projects). It runs inside the caller's transaction with the
// whole workspace in view, because the caller (an admin editing a member) may be restricted to projects.
func disarmUnauthorisedFlows(tx *gorm.DB, p domain.Principal, member *string) error {
	var scope string
	if e := tx.Raw(`SELECT coalesce(current_setting('app.project_scope',true),'')`).Scan(&scope).Error; e != nil {
		return e
	}
	if e := tx.Exec(`SELECT set_config('app.project_scope','*',true)`).Error; e != nil {
		return e
	}
	query := `SELECT id::text AS id,definition,enabled_by::text AS enabled_by FROM core.automations WHERE enabled AND enabled_by IS NOT NULL`
	args := []any{}
	if member != nil {
		query += ` AND enabled_by=?`
		args = append(args, *member)
	}
	var flows []struct {
		ID         string
		Definition json.RawMessage
		EnabledBy  string
	}
	if e := tx.Raw(query+` LIMIT 500`, args...).Scan(&flows).Error; e != nil {
		return e
	}
	for _, flow := range flows {
		var def automation.Definition
		if json.Unmarshal(flow.Definition, &def) != nil || !automation.HasCommand(def) {
			continue
		}
		lost := false
		for _, device := range commandDeviceIDs(def) {
			var may []bool
			if e := tx.Raw(`SELECT core.member_may_command(?::uuid,d.gateway_id) FROM core.devices d WHERE d.id::text=? AND d.removed_at IS NULL`, flow.EnabledBy, device).Scan(&may).Error; e != nil {
				return e
			}
			if len(may) == 1 && !may[0] {
				lost = true
				break
			}
		}
		if !lost {
			continue
		}
		if e := tx.Exec(`UPDATE core.automations SET enabled=false,revision=revision+1,updated_at=now() WHERE id=?`, flow.ID).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.automation_state WHERE automation_id=?`, flow.ID).Error; e != nil {
			return e
		}
		if e := reindexTriggers(tx, p.TenantID, flow.ID, nil, false); e != nil {
			return e
		}
		if e := audit(tx, p, "automation.disarmed", flow.ID); e != nil {
			return e
		}
	}
	return tx.Exec(`SELECT set_config('app.project_scope',?,true)`, scope).Error
}

// resolveRequests rewrites the "requested" command outcomes of runs with what mqtt-commander decided.
func resolveRequests(tx *gorm.DB, runs []automation.Run) error {
	ids := []string{}
	for _, run := range runs {
		var detail runDetail
		if json.Unmarshal(run.Detail, &detail) != nil {
			continue
		}
		for _, c := range detail.Commands {
			if c.Status == "requested" && c.RequestID != "" {
				ids = append(ids, c.RequestID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var rows []struct {
		ID, Status, Reason string
		CommandID          *string
	}
	if e := tx.Raw(`SELECT id::text AS id,status,reason,command_id::text AS command_id FROM core.automation_command_requests WHERE id::text IN ?`, ids).Scan(&rows).Error; e != nil {
		return e
	}
	settled := map[string]commandOutcome{}
	for _, row := range rows {
		c := commandOutcome{Status: row.Status, Reason: row.Reason}
		if row.CommandID != nil {
			c.CommandID = *row.CommandID
		}
		settled[row.ID] = c
	}
	for i := range runs {
		var detail runDetail
		if json.Unmarshal(runs[i].Detail, &detail) != nil {
			continue
		}
		changed := false
		for j, c := range detail.Commands {
			if s, ok := settled[c.RequestID]; ok && c.Status == "requested" && s.Status != "requested" {
				detail.Commands[j].Status, detail.Commands[j].Reason, detail.Commands[j].CommandID = s.Status, s.Reason, s.CommandID
				changed = true
			}
		}
		if changed {
			raw, e := json.Marshal(detail)
			if e != nil {
				return e
			}
			runs[i].Detail = raw
		}
	}
	return nil
}

// refusalReason names a domain refusal from queueCommand; "" means the error is not a refusal (a real failure).
func refusalReason(e error) string {
	var reason domain.ReasonError
	switch {
	case errors.As(e, &reason):
		return reason.Reason
	case errors.Is(e, domain.ErrRateLimited):
		return "rate_limited"
	case errors.Is(e, domain.ErrNotFound):
		return "not_found"
	case errors.Is(e, domain.ErrInvalid):
		return "invalid"
	case errors.Is(e, domain.ErrConflict):
		return "conflict"
	case errors.Is(e, domain.ErrForbidden):
		return "forbidden"
	}
	return ""
}
