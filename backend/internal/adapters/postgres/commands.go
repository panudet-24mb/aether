package postgres

import (
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"gorm.io/gorm"
)

// CommandChannel is the pg_notify channel that wakes mqtt-commander; the payload is the tenant id.
const CommandChannel = "aether_command"

const commandColumns = `tenant_id,id,gateway_id,device_id,ieee,property,requested,value,source,actor_id,automation_id,status,error,created_at,expires_at,sent_at,settled_at,transport,wire`

type commandRow struct {
	TenantID, ID, GatewayID, DeviceID string
	IEEE                              string `gorm:"column:ieee"`
	Property, Requested               string
	Value                             json.RawMessage
	Source                            string
	ActorID, AutomationID             *string
	Status, Error                     string
	CreatedAt, ExpiresAt              time.Time
	SentAt, SettledAt                 *time.Time
	Transport                         string
	Wire                              json.RawMessage
}

func (c commandRow) command() domain.Command {
	return domain.Command{ID: c.ID, TenantID: c.TenantID, GatewayID: c.GatewayID, DeviceID: c.DeviceID, IEEE: c.IEEE, Property: c.Property,
		Requested: c.Requested, Value: c.Value, Source: c.Source, ActorID: c.ActorID, AutomationID: c.AutomationID, Status: c.Status, Error: c.Error,
		CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, SentAt: c.SentAt, SettledAt: c.SettledAt, Transport: c.Transport, Wire: c.Wire}
}

func commands(rows []commandRow) []domain.Command {
	out := make([]domain.Command, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.command())
	}
	return out
}

// QueueCommand records a user's request to switch one gang, inside the caller's transaction and project scope
// (a device outside the member's projects is simply not found). It returns the stored command and whether it
// was created now; the same idempotency key with the same request returns the existing row.
func (r *Repository) QueueCommand(ctx context.Context, p domain.Principal, req domain.CommandRequest) (domain.Command, bool, error) {
	var out domain.Command
	created := false
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		actor := p.UserID
		c, fresh, e := queueCommand(tx, p.TenantID, req, "user", &actor, nil, time.Now().UTC())
		if e != nil {
			return e
		}
		out, created = c, fresh
		if !fresh {
			return nil
		}
		return audit(tx, p, "device.command", c.ID)
	})
	return out, created, classify(e)
}

// queueCommand is shared by every source of commands (the API today, automations later). It resolves the
// target from the registration, validates the value against the device's own Zigbee2MQTT definition, refuses
// what cannot work (not an actuator, gateway gone, device offline, unknown state for a toggle, a command already
// in flight for the property, the workspace budget spent), inserts the row, and wakes the commander. The caller's
// transaction decides visibility (RLS) and commits.
func queueCommand(tx *gorm.DB, tenant string, req domain.CommandRequest, source string, actor, automation *string, now time.Time) (domain.Command, bool, error) {
	var existing []commandRow
	if e := tx.Raw(`SELECT `+commandColumns+` FROM core.device_commands WHERE id=?`, req.ID).Scan(&existing).Error; e != nil {
		return domain.Command{}, false, e
	}
	if len(existing) == 1 {
		c := existing[0]
		same := c.DeviceID == req.DeviceID && c.Property == req.Property && c.Requested == req.Action
		if same && req.Action == "set" {
			same = sameValue(c.Value, req.Value)
		}
		if !same {
			return domain.Command{}, false, domain.Because(domain.ErrConflict, "idempotency_key_reused")
		}
		return c.command(), false, nil
	}
	var devices []struct {
		GatewayID, ExternalID, ProfileID, Model string
		Revoked                                 bool
	}
	if e := tx.Raw(`SELECT d.gateway_id,lower(d.external_id) AS external_id,d.profile_id,g.model,(g.revoked_at IS NOT NULL) AS revoked
    FROM core.devices d JOIN core.gateways g ON g.tenant_id=d.tenant_id AND g.id=d.gateway_id WHERE d.id=? AND d.removed_at IS NULL`, req.DeviceID).Scan(&devices).Error; e != nil {
		return domain.Command{}, false, e
	}
	if len(devices) != 1 {
		return domain.Command{}, false, domain.ErrNotFound
	}
	d := devices[0]
	if profile := domain.DeviceProfileByID(d.ProfileID); profile == nil || !profile.Actuator {
		return domain.Command{}, false, domain.Because(domain.ErrInvalid, "not_an_actuator")
	}
	if !commandGateway(d.Model) || d.Revoked {
		return domain.Command{}, false, domain.Because(domain.ErrConflict, "gateway_unavailable")
	}
	// The device as its transport knows it: a Zigbee2MQTT definition, or a Tuya specification translated into the
	// same exposes shape (plus the dp map that turns a value into the device's data point).
	var zs []struct {
		Exposes, State, DPMap json.RawMessage
		Available             *bool
		Bridge                string
		Transport, KeyStatus  string
	}
	if e := tx.Raw(`SELECT exposes,state,dp_map,available,agent_state AS bridge,transport,key_status FROM core.command_targets
    WHERE gateway_id=? AND external_id=?`, d.GatewayID, d.ExternalID).Scan(&zs).Error; e != nil {
		return domain.Command{}, false, e
	}
	if len(zs) != 1 {
		return domain.Command{}, false, domain.Because(domain.ErrConflict, "not_paired")
	}
	z := zs[0]
	if z.KeyStatus != "ok" {
		// A Tuya device whose local key was never imported, or that the device refused: the Edge cannot talk to it.
		return domain.Command{}, false, domain.Because(domain.ErrConflict, "key_unavailable")
	}
	var value json.RawMessage
	var e error
	switch req.Action {
	case "toggle":
		// Resolved here into the explicit opposite of the last reported value, so a duplicate delivery can never
		// flip the relay twice.
		var state map[string]json.RawMessage
		_ = json.Unmarshal(z.State, &state)
		value, e = zigbee2mqtt.Toggle(z.Exposes, req.Property, state[req.Property])
	default:
		value, e = zigbee2mqtt.Validate(z.Exposes, req.Property, req.Value)
	}
	if e != nil {
		return domain.Command{}, false, e
	}
	var wire any
	if z.Transport == "edge" {
		dpMap := map[string]tuya.Ref{}
		if e := json.Unmarshal(z.DPMap, &dpMap); e != nil {
			return domain.Command{}, false, e
		}
		w, e := tuya.Wire(dpMap, req.Property, value)
		if e != nil {
			return domain.Command{}, false, e
		}
		wire = string(w)
	}
	var offline []bool
	if e := tx.Raw(`SELECT offline FROM core.stream_state WHERE gateway_id=? AND external_id=?`, d.GatewayID, d.ExternalID).Scan(&offline).Error; e != nil {
		return domain.Command{}, false, e
	}
	if (z.Available != nil && !*z.Available) || z.Bridge == "offline" || (len(offline) == 1 && offline[0]) {
		return domain.Command{}, false, domain.Because(domain.ErrConflict, "offline")
	}
	var inFlight int64
	if e := tx.Raw(`SELECT count(*) FROM core.device_commands WHERE device_id=? AND property=? AND status IN ('pending','sent')`, req.DeviceID, req.Property).Scan(&inFlight).Error; e != nil {
		return domain.Command{}, false, e
	}
	if inFlight > 0 {
		return domain.Command{}, false, domain.Because(domain.ErrConflict, "in_flight")
	}
	// The workspace budget is counted under a lock, so two concurrent requests cannot both take the last slot.
	if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,9))`, tenant).Error; e != nil {
		return domain.Command{}, false, e
	}
	var recent int64
	// Counted workspace-wide (a definer function), not through the caller's project scope.
	if e := tx.Raw(`SELECT core.command_count_since(?)`, now.Add(-time.Minute)).Scan(&recent).Error; e != nil {
		return domain.Command{}, false, e
	}
	if recent >= domain.CommandTenantPerMinute {
		return domain.Command{}, false, domain.ErrRateLimited
	}
	var rows []commandRow
	if e := tx.Raw(`INSERT INTO core.device_commands(tenant_id,id,gateway_id,device_id,ieee,property,requested,value,source,actor_id,automation_id,status,created_at,expires_at,transport,wire)
    VALUES(?,?,?,?,?,?,?,?::jsonb,?,?,?,'pending',?,?,?,?::jsonb) RETURNING `+commandColumns,
		tenant, req.ID, d.GatewayID, req.DeviceID, d.ExternalID, req.Property, req.Action, string(value), source, actor, automation, now, now.Add(domain.CommandTTL), z.Transport, wire).Scan(&rows).Error; e != nil {
		if errors.Is(e, gorm.ErrDuplicatedKey) {
			return domain.Command{}, false, domain.Because(domain.ErrConflict, "in_flight")
		}
		return domain.Command{}, false, e
	}
	if len(rows) != 1 {
		return domain.Command{}, false, domain.ErrNotFound // the project scope refused the row (WITH CHECK)
	}
	if e := tx.Exec(`SELECT pg_notify(?,?)`, CommandChannel, tenant).Error; e != nil {
		return domain.Command{}, false, e
	}
	if e := signal(tx, tenant, "command", d.GatewayID); e != nil {
		return domain.Command{}, false, e
	}
	return rows[0].command(), true, nil
}

// commandGatewayModels are the gateway models that carry commands.
var commandGatewayModels = []string{domain.Z2MGatewayModel, domain.EdgeGatewayModel}

// commandGateway reports the gateway models that carry commands: Zigbee2MQTT and Aether Edge.
func commandGateway(model string) bool {
	return model == domain.Z2MGatewayModel || model == domain.EdgeGatewayModel
}

func sameValue(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// ListCommands returns a device's (or the workspace's) latest commands the member may see.
func (r *Repository) ListCommands(ctx context.Context, p domain.Principal, deviceID string, limit int) ([]domain.Command, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	var rows []commandRow
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		q := tx.Raw(`SELECT `+commandColumns+` FROM core.device_commands ORDER BY created_at DESC,id LIMIT ?`, limit)
		if deviceID != "" {
			q = tx.Raw(`SELECT `+commandColumns+` FROM core.device_commands WHERE device_id=? ORDER BY created_at DESC,id LIMIT ?`, deviceID, limit)
		}
		return q.Scan(&rows).Error
	})
	return commands(rows), e
}

func (r *Repository) GetCommand(ctx context.Context, p domain.Principal, id string) (domain.Command, error) {
	var rows []commandRow
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT `+commandColumns+` FROM core.device_commands WHERE id=?`, id).Scan(&rows).Error
	})
	if e != nil {
		return domain.Command{}, e
	}
	if len(rows) != 1 {
		return domain.Command{}, domain.ErrNotFound
	}
	return rows[0].command(), nil
}

// PendingCommandGateways is the first half of a commander sweep for one tenant, in system scope: pending
// commands past their expiry become `expired` (never published), and the gateways that still have pending
// commands are returned, so the commander can claim per gateway only what that gateway's pace allows.
func (r *Repository) PendingCommandGateways(ctx context.Context, tenant string, now time.Time) ([]string, error) {
	var gateways []string
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var expired []string
		if e := tx.Raw(`UPDATE core.device_commands SET status='expired',settled_at=?,error='not published before it expired'
    WHERE status='pending' AND expires_at<=? RETURNING gateway_id`, now, now).Scan(&expired).Error; e != nil {
			return e
		}
		if e := signalGateways(tx, tenant, expired); e != nil {
			return e
		}
		return tx.Raw(`SELECT DISTINCT gateway_id FROM core.device_commands WHERE status='pending' AND expires_at>? LIMIT 1000`, now).Scan(&gateways).Error
	})
	return gateways, e
}

// ClaimCommands moves up to `limit` pending, unexpired commands of one gateway to `sent` and returns them. The
// transaction commits BEFORE the caller publishes, so a crash can lose a publish but never repeat one. sent_at is
// provisional here; MarkCommandsPublished sets it to the moment of publishing.
func (r *Repository) ClaimCommands(ctx context.Context, tenant, gateway string, now time.Time, limit int) ([]domain.Command, error) {
	var rows []commandRow
	if limit <= 0 {
		return nil, nil
	}
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		// ARRAY(...) evaluates the locking sub-select exactly once; as a joined sub-query the planner may run it
		// again and claim more than `limit`.
		if e := tx.Raw(`UPDATE core.device_commands SET status='sent',sent_at=?
    WHERE tenant_id=core.tenant_id() AND id=ANY(ARRAY(SELECT id FROM core.device_commands WHERE gateway_id=? AND status='pending' AND expires_at>?
      ORDER BY created_at,id LIMIT ? FOR UPDATE SKIP LOCKED)) RETURNING `+commandColumns, now, gateway, now, limit).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) == 0 {
			return nil
		}
		return signal(tx, tenant, "command", gateway)
	})
	return commands(rows), e
}

// MarkCommandsPublished starts the confirmation window at the moment the broker accepted the publish.
func (r *Repository) MarkCommandsPublished(ctx context.Context, tenant string, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		return tx.Exec(`UPDATE core.device_commands SET sent_at=? WHERE id IN ? AND status='sent'`, at, ids).Error
	})
}

// MarkCommandFailed records that the broker refused a claimed command's publish (or it could not be sent in time).
func (r *Repository) MarkCommandFailed(ctx context.Context, tenant, id, reason string) error {
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var gateways []string
		if e := tx.Raw(`UPDATE core.device_commands SET status='failed',settled_at=now(),error=? WHERE id=? AND status='sent' RETURNING gateway_id`, reason, id).Scan(&gateways).Error; e != nil {
			return e
		}
		return signalGateways(tx, tenant, gateways)
	})
}

// TimeoutCommands settles sent commands that no state report confirmed within domain.CommandConfirmWindow.
func (r *Repository) TimeoutCommands(ctx context.Context, tenant string, now time.Time) (int, error) {
	var gateways []string
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		if e := tx.Raw(`UPDATE core.device_commands SET status='timeout',settled_at=?,error='no state report from the device'
    WHERE status='sent' AND sent_at<=? RETURNING gateway_id`, now, now.Add(-domain.CommandConfirmWindow)).Scan(&gateways).Error; e != nil {
			return e
		}
		return signalGateways(tx, tenant, gateways)
	})
	return len(gateways), e
}

// confirmCommands runs in the collector's transaction for a Zigbee2MQTT state report. A sent command whose
// property the report carries with the desired value (within the feature's tolerance) is confirmed, and is
// returned per property so the event this change causes can name it. A timed-out command may still be confirmed
// by a report within domain.CommandLateConfirm, but only if the property was not reported with another value in
// between (then it is superseded for good), and a late confirmation is never returned: the change it reports is
// not attributed to Aether, because by then a press on the wall is as likely a cause.
func confirmCommands(tx *gorm.DB, tenant, gateway, ieee string, features []zigbee2mqtt.Feature, state map[string]json.RawMessage, now time.Time) (map[string]string, error) {
	out := map[string]string{}
	if len(state) == 0 {
		return out, nil
	}
	var open []commandRow
	if e := tx.Raw(`SELECT `+commandColumns+` FROM core.device_commands WHERE gateway_id=? AND ieee=?
    AND (status='sent' OR (status='timeout' AND NOT superseded AND settled_at>=?)) ORDER BY created_at LIMIT 100`, gateway, ieee, now.Add(-domain.CommandLateConfirm)).Scan(&open).Error; e != nil {
		return nil, e
	}
	changed := false
	for _, c := range open {
		reported, ok := state[c.Property]
		if !ok {
			continue
		}
		if !zigbee2mqtt.MatchesIn(features, c.Property, c.Value, reported) {
			if c.Status == domain.CommandTimeout {
				if e := tx.Exec(`UPDATE core.device_commands SET superseded=true WHERE tenant_id=? AND id=?`, c.TenantID, c.ID).Error; e != nil {
					return nil, e
				}
			}
			continue
		}
		note := ""
		if c.Status == domain.CommandTimeout {
			note = "confirmed after the timeout"
		}
		if e := tx.Exec(`UPDATE core.device_commands SET status='confirmed',settled_at=?,error=? WHERE tenant_id=? AND id=?`, now, note, c.TenantID, c.ID).Error; e != nil {
			return nil, e
		}
		changed = true
		if c.Status == domain.CommandSent {
			out[c.Property] = c.ID
		}
	}
	if changed {
		if e := signal(tx, tenant, "command", gateway); e != nil {
			return nil, e
		}
	}
	return out, nil
}

func signalGateways(tx *gorm.DB, tenant string, gateways []string) error {
	seen := map[string]bool{}
	for _, g := range gateways {
		if seen[g] {
			continue
		}
		seen[g] = true
		if e := signal(tx, tenant, "command", g); e != nil {
			return e
		}
	}
	return nil
}

// DeviceControls reads what a registered device can be set to (a Zigbee2MQTT device, or a Tuya device through
// Aether Edge), within the member's project scope.
func (r *Repository) DeviceControls(ctx context.Context, p domain.Principal, deviceID string) (domain.DeviceControls, error) {
	var rows []struct {
		GatewayID, ExternalID string
		Exposes, State        json.RawMessage
		Available             *bool
		Bridge, KeyStatus     string
		Offline               *bool
	}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT d.gateway_id,lower(d.external_id) AS external_id,t.exposes,t.state,t.available,t.agent_state AS bridge,t.key_status,st.offline
    FROM core.devices d
    JOIN core.gateways g ON g.tenant_id=d.tenant_id AND g.id=d.gateway_id AND g.model IN ? AND g.revoked_at IS NULL
    JOIN core.command_targets t ON t.tenant_id=d.tenant_id AND t.gateway_id=d.gateway_id AND t.external_id=lower(d.external_id)
    LEFT JOIN core.stream_state st ON st.tenant_id=d.tenant_id AND st.gateway_id=d.gateway_id AND st.external_id=lower(d.external_id)
    WHERE d.id=? AND d.removed_at IS NULL`, commandGatewayModels, deviceID).Scan(&rows).Error
	})
	if e != nil {
		return domain.DeviceControls{}, e
	}
	if len(rows) != 1 {
		return domain.DeviceControls{}, domain.ErrNotFound
	}
	row := rows[0]
	online := (row.Available == nil || *row.Available) && row.Bridge != "offline" && (row.Offline == nil || !*row.Offline) && row.KeyStatus == "ok"
	return domain.DeviceControls{DeviceID: deviceID, GatewayID: row.GatewayID, IEEE: row.ExternalID, Exposes: row.Exposes, State: row.State, Online: online}, nil
}
