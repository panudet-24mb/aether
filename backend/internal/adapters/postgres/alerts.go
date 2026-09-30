package postgres

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ruleRow struct {
	ID, Name, EventType, Severity string
	Enabled                       bool
	Scope, Channels               json.RawMessage
	DedupeSec                     int
	ProjectID                     *string
	CreatedAt, UpdatedAt          time.Time
}

func (row ruleRow) rule() domain.AlertRule {
	r := domain.AlertRule{ID: row.ID, Name: row.Name, Enabled: row.Enabled, EventType: row.EventType, Severity: row.Severity, DedupeSec: row.DedupeSec, ProjectID: row.ProjectID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Channels: []string{}}
	_ = json.Unmarshal(row.Scope, &r.Scope)
	_ = json.Unmarshal(row.Channels, &r.Channels)
	if r.Channels == nil {
		r.Channels = []string{}
	}
	return r
}

// maxRules and maxChannels are the per-workspace caps, counted tenant-wide by SECURITY DEFINER functions.
const (
	maxRules    = 100
	maxChannels = 20
)

func loadRules(tx *gorm.DB, enabledOnly bool) ([]domain.AlertRule, error) {
	var rows []ruleRow
	q := `SELECT id,name,enabled,event_type,severity,scope,channels,dedupe_sec,project_id::text AS project_id,created_at,updated_at FROM core.alert_rules`
	if enabledOnly {
		q += ` WHERE enabled`
	}
	// SOS and hazard rules load first, so a workspace over the cap (which the API refuses to create, see
	// core.alert_rule_count) can never lose them; a truncation is logged, not hidden.
	if e := tx.Raw(q + ` ORDER BY (event_type IN ('button','hazard')) DESC, created_at, id LIMIT 101`).Scan(&rows).Error; e != nil {
		return nil, e
	}
	if len(rows) > maxRules {
		slog.Warn("alert rules truncated", "limit", maxRules)
		rows = rows[:maxRules]
	}
	out := make([]domain.AlertRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.rule())
	}
	return out, nil
}

type stateRow struct {
	ExternalID           string
	Tamper, Leak, Moving int
	Instance             string
	Offline              bool
	Breaches             json.RawMessage
	Door, Occupied       int
	LastMotionAt         *time.Time
	TriggerAt            *time.Time
	Outputs              json.RawMessage
	// Signals holds the last matched/not-matched state of each learned signal for this stream. It
	// deliberately rides in core.stream_state with the built-in tamper/leak/moving flags rather than
	// in a parallel table: learned signals are edge-triggered by exactly the same rule, and sharing
	// the row means they are locked, written and rolled back together with them.
	Signals json.RawMessage
}

func (row stateRow) state() alerts.State {
	s := alerts.State{Known: true, Tamper: row.Tamper, Leak: row.Leak, Moving: row.Moving, Instance: row.Instance, Offline: row.Offline, Door: row.Door, Occupied: row.Occupied}
	if row.LastMotionAt != nil {
		s.LastMotion = *row.LastMotionAt
	}
	if row.TriggerAt != nil {
		s.TriggerAt = *row.TriggerAt
	}
	_ = json.Unmarshal(row.Breaches, &s.Breaches)
	_ = json.Unmarshal(row.Outputs, &s.Outputs)
	return s
}

// stateColumns is every column stateRow reads, so all readers of core.stream_state stay in step.
const stateColumns = `external_id,tamper,leak,moving,instance,offline,breaches,door,occupied,last_motion_at,trigger_at,outputs`

// upsertState reports tracked=false when the stream row does not exist (discovery cap reached); callers must
// then emit nothing, otherwise an untracked device would look "first seen" on every uplink and flood events.
func upsertState(tx *gorm.DB, tenant, gateway, external string, s alerts.State) (bool, error) {
	breaches, _ := json.Marshal(s.Breaches)
	if s.Breaches == nil {
		breaches = []byte("[]")
	}
	var lastMotion, triggerAt any
	if !s.LastMotion.IsZero() {
		lastMotion = s.LastMotion
	}
	if !s.TriggerAt.IsZero() {
		triggerAt = s.TriggerAt
	}
	outputs, _ := json.Marshal(s.Outputs)
	if s.Outputs == nil {
		outputs = []byte("{}")
	}
	res := tx.Exec(`INSERT INTO core.stream_state(tenant_id,gateway_id,external_id,tamper,leak,moving,instance,offline,breaches,door,occupied,last_motion_at,trigger_at,outputs,updated_at)
    SELECT ?,?,?,?,?,?,?,?,?::jsonb,?,?,?::timestamptz,?::timestamptz,?::jsonb,now() WHERE EXISTS(SELECT 1 FROM core.sensor_streams WHERE gateway_id=? AND external_id=?)
    ON CONFLICT(tenant_id,gateway_id,external_id) DO UPDATE SET tamper=EXCLUDED.tamper,leak=EXCLUDED.leak,moving=EXCLUDED.moving,instance=EXCLUDED.instance,offline=EXCLUDED.offline,breaches=EXCLUDED.breaches,
      door=EXCLUDED.door,occupied=EXCLUDED.occupied,last_motion_at=EXCLUDED.last_motion_at,trigger_at=EXCLUDED.trigger_at,outputs=EXCLUDED.outputs,updated_at=now()`,
		tenant, gateway, external, s.Tamper, s.Leak, s.Moving, s.Instance, s.Offline, string(breaches), s.Door, s.Occupied, lastMotion, triggerAt, string(outputs), gateway, external)
	return res.RowsAffected > 0, res.Error
}

func insertEvent(tx *gorm.DB, tenant string, ev *domain.DeviceEvent) error {
	ev.ID = uuid.NewString()
	if ev.Detail == nil {
		ev.Detail = map[string]any{}
	}
	detail, _ := json.Marshal(ev.Detail)
	if e := tx.Exec(`INSERT INTO core.device_events(tenant_id,id,gateway_id,external_id,device_name,event_type,detail,occurred_at) VALUES(?,?,?,?,?,?,?::jsonb,?)`, tenant, ev.ID, ev.GatewayID, ev.ExternalID, ev.DeviceName, ev.EventType, string(detail), ev.OccurredAt).Error; e != nil {
		return e
	}
	return signal(tx, tenant, "event", ev.GatewayID)
}

// createAlerts opens one alert per matching rule (subject to the rule's dedupe window) and queues its notifications.
// A rule that belongs to a project only matches devices on gateways of that project, and a delivery is only
// queued to a channel that is workspace-wide or of the gateway's project (docs/platform/alerts.md).
func createAlerts(tx *gorm.DB, tenant string, ev domain.DeviceEvent, rules []domain.AlertRule) error {
	var gateways []struct{ ProjectID *string }
	if e := tx.Raw(`SELECT project_id::text AS project_id FROM core.gateways WHERE id=?`, ev.GatewayID).Scan(&gateways).Error; e != nil {
		return e
	}
	var project *string
	if len(gateways) == 1 {
		project = gateways[0].ProjectID
	}
	// covered: some rule really matched this device here (in its project, enabled, in device scope and time
	// window), whether or not its dedupe window then held the alert back.
	covered := false
	for _, rule := range rules {
		if !ruleInProject(rule, project) || !alerts.Matches(rule, ev) {
			continue
		}
		covered = true
		// One alert at a time per rule and device. For SOS the alert that blocks a new one is only an
		// UNACKNOWLEDGED one: once somebody has acknowledged a press, the next press is a new call for help
		// and must ring again, not vanish behind an alert that is merely waiting to be resolved.
		blocking := `status<>'resolved'`
		if ev.EventType == domain.EventButton {
			blocking = `status='open'`
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.alerts WHERE rule_id=? AND external_id=? AND (`+blocking+` OR opened_at>?)`, rule.ID, ev.ExternalID, ev.OccurredAt.Add(-time.Duration(rule.DedupeSec)*time.Second)).Scan(&n).Error; e != nil {
			return e
		}
		if n > 0 {
			continue
		}
		id := uuid.NewString()
		if e := tx.Exec(`INSERT INTO core.alerts(tenant_id,id,rule_id,event_id,gateway_id,external_id,device_name,event_type,severity,title,opened_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			tenant, id, rule.ID, ev.ID, ev.GatewayID, ev.ExternalID, ev.DeviceName, ev.EventType, rule.Severity, alerts.Title(ev), ev.OccurredAt).Error; e != nil {
			return e
		}
		if e := signal(tx, tenant, "alert", ev.GatewayID); e != nil {
			return e
		}
		for _, channel := range rule.Channels {
			if e := tx.Exec(`INSERT INTO core.notifications(tenant_id,id,alert_id,channel_id,next_attempt_at) SELECT ?,?,?,id,now() FROM core.notification_channels
        WHERE id=? AND enabled AND (project_id IS NULL OR project_id=?::uuid)`, tenant, uuid.NewString(), id, channel, project).Error; e != nil {
				return e
			}
		}
	}
	// An SOS or a hazard that no rule covered still opens a critical fallback alert (no channel is notified,
	// because no rule chose one). The only way to silence it is a decision full-scope members control: the
	// workspace has workspace-wide rules for the event type and every one of them is disabled. Nothing a
	// project-limited admin does to project rules can switch that on or off (docs/platform/alerts.md).
	if domain.BypassesShadow(ev.EventType) && !covered {
		silenced, e := workspaceSilenced(tx, ev.EventType)
		if e != nil || silenced {
			return e
		}
		return openFallbackAlert(tx, tenant, ev)
	}
	return nil
}

// workspaceSilenced: workspace-wide rules exist for this event type and all of them are disabled. The rules
// passed to createAlerts are the enabled ones only, so this asks the table; it runs only for an SOS or a
// hazard that nothing covered, and ingest and the workers see every rule (scope '*').
func workspaceSilenced(tx *gorm.DB, eventType string) (bool, error) {
	var state struct {
		Rules   int64
		Enabled int64
	}
	e := tx.Raw(`SELECT count(*) AS rules, count(*) FILTER (WHERE enabled) AS enabled FROM core.alert_rules WHERE project_id IS NULL AND event_type=?`, eventType).Scan(&state).Error
	return state.Rules > 0 && state.Enabled == 0, e
}

func ruleInProject(rule domain.AlertRule, project *string) bool {
	return rule.ProjectID == nil || (project != nil && *rule.ProjectID == *project)
}

// openFallbackAlert records a critical alert with no rule, at most one open fallback per device and event type.
// Automation alerts have no rule either; only core.alerts.fallback tells them apart, so a flow's open info
// alert never holds the next SOS back.
func openFallbackAlert(tx *gorm.DB, tenant string, ev domain.DeviceEvent) error {
	var n int64
	if e := tx.Raw(`SELECT count(*) FROM core.alerts WHERE fallback AND external_id=? AND event_type=? AND status='open'`, ev.ExternalID, ev.EventType).Scan(&n).Error; e != nil {
		return e
	}
	if n > 0 {
		return nil
	}
	if e := tx.Exec(`INSERT INTO core.alerts(tenant_id,id,rule_id,event_id,gateway_id,external_id,device_name,event_type,severity,title,opened_at,fallback) VALUES(?,?,NULL,?,?,?,?,?,'critical',?,?,true)`,
		tenant, uuid.NewString(), ev.ID, ev.GatewayID, ev.ExternalID, ev.DeviceName, ev.EventType, alerts.Title(ev), ev.OccurredAt).Error; e != nil {
		return e
	}
	return signal(tx, tenant, "alert", ev.GatewayID)
}

// Limits on the informational `action` events of Zigbee2MQTT buttons and remotes.
const (
	actionCoalesce   = 2 * time.Second
	actionsPerMinute = 30
	maxActionEvents  = 1000
)

// updateZone smooths this gateway's RSSI for the tag and moves the wearable's zone when alerts.DecideZone says so.
// It returns the zone event to record, or nil. The caller holds the per-tenant roaming lock.
func updateZone(tx *gorm.DB, tenant, gateway, external string, rssi int, at time.Time) (*domain.DeviceEvent, error) {
	var own []struct {
		RssiAvg *float64
		RssiAt  *time.Time
	}
	if e := tx.Raw(`SELECT rssi_avg,rssi_at FROM core.stream_state WHERE gateway_id=? AND external_id=?`, gateway, external).Scan(&own).Error; e != nil {
		return nil, e
	}
	var prevAvg *float64
	var prevAt time.Time
	if len(own) == 1 && own[0].RssiAvg != nil && own[0].RssiAt != nil {
		prevAvg, prevAt = own[0].RssiAvg, *own[0].RssiAt
	}
	avg := alerts.SmoothRSSI(prevAvg, prevAt, rssi, at)
	if e := tx.Exec(`UPDATE core.stream_state SET rssi_avg=?,rssi_at=? WHERE gateway_id=? AND external_id=?`, avg, at, gateway, external).Error; e != nil {
		return nil, e
	}
	var rows []struct {
		GatewayID          *string
		Since              *time.Time
		CandidateGatewayID *string
		CandidateSince     *time.Time
		CandidateSeen      *time.Time
	}
	if e := tx.Raw(`SELECT gateway_id,since,candidate_gateway_id,candidate_since,candidate_seen FROM core.presence_state WHERE external_id=?`, external).Scan(&rows).Error; e != nil {
		return nil, e
	}
	prev := alerts.ZoneState{}
	if len(rows) == 1 {
		if rows[0].GatewayID != nil {
			prev.Gateway = *rows[0].GatewayID
		}
		if rows[0].Since != nil {
			prev.Since = *rows[0].Since
		}
		if rows[0].CandidateGatewayID != nil && rows[0].CandidateSince != nil {
			prev.Candidate, prev.CandidateSince = *rows[0].CandidateGatewayID, *rows[0].CandidateSince
			prev.CandidateSeen = prev.CandidateSince
			if rows[0].CandidateSeen != nil {
				prev.CandidateSeen = *rows[0].CandidateSeen
			}
		}
	}
	var currentAvg *float64
	if prev.Gateway != "" && prev.Gateway != gateway {
		var cur []struct{ RssiAvg *float64 }
		// A revoked gateway cannot hold a zone.
		if e := tx.Raw(`SELECT st.rssi_avg FROM core.stream_state st JOIN core.gateways g ON g.tenant_id=st.tenant_id AND g.id=st.gateway_id AND g.revoked_at IS NULL
      WHERE st.gateway_id=? AND st.external_id=? AND st.rssi_at>?`, prev.Gateway, external, at.Add(-alerts.ZoneFresh)).Scan(&cur).Error; e != nil {
			return nil, e
		}
		if len(cur) == 1 {
			currentAvg = cur[0].RssiAvg
		}
	} else if prev.Gateway == gateway {
		currentAvg = &avg
	}
	next, changed := alerts.DecideZone(prev, gateway, avg, currentAvg, at)
	if next == prev {
		return nil, nil
	}
	var candidate, candidateSince, candidateSeen any
	if next.Candidate != "" {
		candidate, candidateSince, candidateSeen = next.Candidate, next.CandidateSince, next.CandidateSeen
	}
	if e := tx.Exec(`INSERT INTO core.presence_state(tenant_id,external_id,gateway_id,since,candidate_gateway_id,candidate_since,candidate_seen,updated_at) VALUES(?,?,?,?,?,?,?,now())
    ON CONFLICT(tenant_id,external_id) DO UPDATE SET gateway_id=EXCLUDED.gateway_id,since=EXCLUDED.since,candidate_gateway_id=EXCLUDED.candidate_gateway_id,candidate_since=EXCLUDED.candidate_since,candidate_seen=EXCLUDED.candidate_seen,updated_at=now()`,
		tenant, external, next.Gateway, next.Since, candidate, candidateSince, candidateSeen).Error; e != nil {
		return nil, e
	}
	if !changed {
		return nil, nil
	}
	var named []struct{ ID, Name string }
	ids := []string{gateway}
	if prev.Gateway != "" { // the first assignment has no previous zone
		ids = append(ids, prev.Gateway)
	}
	if e := tx.Raw(`SELECT id,name FROM core.gateways WHERE id IN ?`, ids).Scan(&named).Error; e != nil {
		return nil, e
	}
	name := map[string]string{}
	for _, n := range named {
		name[n.ID] = n.Name
	}
	return &domain.DeviceEvent{EventType: domain.EventZone, OccurredAt: at, Detail: map[string]any{"to_gateway_id": gateway, "to": name[gateway], "from_gateway_id": prev.Gateway, "from": name[prev.Gateway], "rssi_avg": math.Round(avg*10) / 10}}, nil
}

const roamingEventWindow = 15 * time.Second

// saveEvents runs inside the packet transaction after samples/streams are stored. raws carries this
// packet's distinct advertisements per advertiser, which the learned signals of the workspace are
// matched against (see internal/signals and postgres/signals.go).
func saveEvents(tx *gorm.DB, tenant, gateway string, view minew.View, raws rawByIdentity, at time.Time, opts Options) error {
	if len(view.Sensors) == 0 {
		return nil
	}
	rules, e := loadRules(tx, true)
	if e != nil {
		return e
	}
	// Loaded once per packet and indexed in Go; the per-sensor work below is a map lookup plus a
	// matcher evaluation over the advertisements that were already parsed for this uplink.
	learned, e := loadLearnedSignals(tx)
	if e != nil {
		return e
	}
	// Events written during this uplink, handed to the automation studio at the end of the function.
	inserted := []domain.DeviceEvent{}
	// Lock order matters: the roaming lock comes BEFORE the row locks on stream_state. Taken the other way round,
	// two gateways hearing one wearable deadlock, and the rollback would drop every event of the uplink.
	// Several gateways hear the same wearable: record a button press or tamper once, not once per gateway.
	var roamingIDs []string
	if e := tx.Raw(`SELECT external_id FROM core.devices WHERE roaming AND removed_at IS NULL LIMIT 1000`).Scan(&roamingIDs).Error; e != nil {
		return e
	}
	roaming := map[string]bool{}
	for _, id := range roamingIDs {
		roaming[id] = true
	}
	// Two gateways usually commit the same wearable within the same second. One lock per uplink that carries a
	// roaming identity serialises event dedupe and the zone decision (and gives a single, deadlock-free order).
	for _, sensor := range view.Sensors {
		if roaming[sensor.ID] {
			if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,4))`, tenant).Error; e != nil {
				return e
			}
			break
		}
	}
	var rows []stateRow
	if e := tx.Raw(`SELECT `+stateColumns+`,signals FROM core.stream_state WHERE gateway_id=? FOR UPDATE`, gateway).Scan(&rows).Error; e != nil {
		return e
	}
	states := map[string]alerts.State{}
	signalStates := map[string]map[string]int{}
	for _, row := range rows {
		states[row.ExternalID] = row.state()
		signalStates[row.ExternalID] = decodeSignalState(row.Signals)
	}
	// Events carry the stored stream name (user-assigned or upgraded from an info frame), not the
	// per-uplink guess, so alerts and notifications name the device the way the workspace knows it.
	var named []struct{ ExternalID, Name string }
	if e := tx.Raw(`SELECT external_id,name FROM core.sensor_streams WHERE gateway_id=?`, gateway).Scan(&named).Error; e != nil {
		return e
	}
	names := map[string]string{}
	for _, n := range named {
		names[n.ExternalID] = n.Name
	}
	// A button press is inferred (the B10's iBeacon trigger slot reappearing, or an Eddystone-UID instance
	// change). Any third-party beacon can produce either, so only tags registered with a button profile may raise it.
	var buttonIDs []string
	if e := tx.Raw(`SELECT lower(external_id) FROM core.devices WHERE removed_at IS NULL AND profile_id IN ? LIMIT 5000`, domain.ButtonProfileIDs()).Scan(&buttonIDs).Error; e != nil {
		return e
	}
	// A Zigbee2MQTT device raises `button` only from an emergency action or SOS binary, and only when its own
	// definition can call for help (z2m_devices.sos) and it is registered: an ordinary remote never rings.
	var sosIDs []string
	if e := tx.Raw(`SELECT lower(d.external_id) FROM core.devices d JOIN core.z2m_devices z ON z.tenant_id=d.tenant_id AND z.gateway_id=? AND z.ieee=lower(d.external_id)
    WHERE d.removed_at IS NULL AND z.removed_at IS NULL AND z.sos LIMIT 5000`, gateway).Scan(&sosIDs).Error; e != nil {
		return e
	}
	button := map[string]bool{}
	for _, id := range append(buttonIDs, sosIDs...) {
		button[id] = true
	}
	for _, sensor := range view.Sensors {
		if stored := names[sensor.ID]; stored != "" {
			sensor.Name = stored
		}
		prev := states[sensor.ID]
		if roaming[sensor.ID] {
			// The offline episode lives on the stream that heard the wearable last. Coming back through another
			// gateway must still end it: clear the sibling flag and let Detect emit the single `online` event.
			res := tx.Exec(`UPDATE core.stream_state SET offline=false,updated_at=now() WHERE external_id=? AND gateway_id<>? AND offline`, sensor.ID, gateway)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected > 0 {
				prev.Offline = true
			}
		}
		events, next := alerts.Detect(prev, sensor.Latest, rules, at)
		tracked, e := upsertState(tx, tenant, gateway, sensor.ID, next)
		if e != nil {
			return e
		}
		if !tracked {
			continue
		}
		// Learned signals: matched against the raw advertisements of THIS uplink, edge-triggered
		// against the state stored on the row upsertState just wrote.
		applicable := learned.forIdentity(sensor.ID)
		if len(applicable) > 0 {
			signalEvs, nextSignals := signalEvents(applicable, raws[sensor.ID], signalStates[sensor.ID], at)
			encoded, e := json.Marshal(nextSignals)
			if e != nil {
				return e
			}
			if e := tx.Exec(`UPDATE core.stream_state SET signals=?::jsonb WHERE gateway_id=? AND external_id=?`, string(encoded), gateway, sensor.ID).Error; e != nil {
				return e
			}
			events = append(events, signalEvs...)
		}
		if roaming[sensor.ID] && sensor.Latest.RSSI != nil {
			zone, e := updateZone(tx, tenant, gateway, sensor.ID, *sensor.Latest.RSSI, at)
			if e != nil {
				return e
			}
			if zone != nil {
				events = append(events, *zone)
			}
		}
		// A door state that came from a taught signal (applyLearnedDoors set the metric before the
		// samples were stored) names that signature, like every other learned event.
		doorSignal := learnedDoorID(applicable, raws[sensor.ID])
		for i := range events {
			ev := events[i]
			ev.GatewayID, ev.ExternalID, ev.DeviceName = gateway, sensor.ID, sensor.Name
			if doorSignal != "" && (ev.EventType == domain.EventDoorOpen || ev.EventType == domain.EventDoorClosed) {
				detail := map[string]any{"learned": true, "signal_id": doorSignal}
				for k, v := range ev.Detail {
					detail[k] = v
				}
				ev.Detail = detail
			}
			// The profile gate below guards the INFERRED press (an Eddystone-UID instance change, which
			// any third-party beacon can produce). A learned signal was taught on this very device, so
			// it carries its own evidence and is not subject to it.
			if ev.EventType == domain.EventButton && !button[sensor.ID] && ev.Detail["signal_id"] == nil {
				continue
			}
			// Remotes can be chatty (a rotary dimmer sends dozens of rotate actions a second): the same action
			// again within actionCoalesce is one event, and at most actionsPerMinute actions per device are kept.
			// Only the informational `action` event is limited; an SOS press is its own `button` event.
			if ev.EventType == domain.EventAction {
				var recent struct{ Total, Same int64 }
				if e := tx.Raw(`SELECT count(*) AS total, count(*) FILTER (WHERE detail->>'action'=? AND occurred_at>?) AS same
    FROM core.device_events WHERE gateway_id=? AND external_id=? AND event_type=? AND occurred_at>?`,
					ev.Detail["action"], ev.OccurredAt.Add(-actionCoalesce), gateway, sensor.ID, domain.EventAction, ev.OccurredAt.Add(-time.Minute)).Scan(&recent).Error; e != nil {
					return e
				}
				if recent.Same > 0 || recent.Total >= actionsPerMinute {
					continue
				}
			}
			if roaming[sensor.ID] && ev.EventType != domain.EventZone {
				var n int64
				if e := tx.Raw(`SELECT count(*) FROM core.device_events WHERE external_id=? AND event_type=? AND gateway_id<>? AND occurred_at>?`, sensor.ID, ev.EventType, gateway, ev.OccurredAt.Add(-roamingEventWindow)).Scan(&n).Error; e != nil {
					return e
				}
				if n > 0 {
					continue
				}
			}
			if e := insertEvent(tx, tenant, &ev); e != nil {
				return e
			}
			inserted = append(inserted, ev)
			// Shadow mode: the event log fills, nothing is alerted or sent — except life safety. Shadow exists so
			// untuned thresholds do not wake anyone during the first days; a person pressing a panic button, or a
			// smoke / gas / CO detector in alarm, is never a tuning problem, so `button` and `hazard` still open
			// their alerts (and notify their channels).
			if opts.AlertsShadow && !domain.BypassesShadow(ev.EventType) {
				continue
			}
			if e := createAlerts(tx, tenant, ev, rules); e != nil {
				return e
			}
		}
	}
	if opts.AlertsShadow {
		return nil
	}
	// Automations get their own savepoint: whatever goes wrong there, the events and alerts above are kept.
	if e := tx.SavePoint("automations").Error; e != nil {
		return e
	}
	if e := runAutomations(tx, tenant, gateway, view, inserted, at, opts); e != nil {
		slog.Warn("automations failed; events and alerts kept", "gateway", gateway, "error", e.Error())
		return tx.RollbackTo("automations").Error
	}
	return nil
}

func (r *Repository) ActiveTenants(ctx context.Context) ([]string, error) {
	var out []string
	e := r.db.WithContext(ctx).Raw(`SELECT id::text FROM core.active_tenant_ids() AS id LIMIT 10000`).Scan(&out).Error
	return out, e
}

// A roaming device is offline only when no gateway hears it: its streams are skipped while another gateway has
// heard the same identity more recently, so one offline episode is raised, on the gateway that heard it last.
// ScanOffline raises one offline event per silent stream (at the smallest rule threshold) and opens each
// offline rule's alert only once that rule's own threshold has passed, once per offline episode.
func (r *Repository) ScanOffline(ctx context.Context, tenant string, now time.Time) (int, error) {
	count := 0
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		// Zigbee2MQTT bridges that went silent take their devices offline whether or not a rule subscribes:
		// their streams are not aged by silence below, so this is the only thing that keeps them honest.
		silent, e := r.scanSilentBridges(tx, tenant, now)
		if e != nil {
			return e
		}
		count += silent
		// The same for Aether Edge agents (Tuya Wi-Fi devices), whose heartbeat is far more frequent.
		silent, e = r.scanSilentAgents(tx, tenant, edgeAgent, "edge_silent", now)
		if e != nil {
			return e
		}
		count += silent
		// Tuya Cloud links: a worker that stopped writing health at all (silence), and a link the worker reports
		// down for longer than CloudLinkGrace.
		silent, e = r.scanSilentAgents(tx, tenant, cloudAgent, "cloud_silent", now)
		if e != nil {
			return e
		}
		count += silent
		silent, e = r.scanCloudLinksDown(tx, tenant, now)
		if e != nil {
			return e
		}
		count += silent
		all, e := loadRules(tx, true)
		if e != nil {
			return e
		}
		var rules []domain.AlertRule
		after := 0
		for _, rule := range all {
			if rule.EventType != domain.EventOffline {
				continue
			}
			if rule.Scope.OfflineAfterSec <= 0 {
				rule.Scope.OfflineAfterSec = domain.DefaultOfflineAfterSec
			}
			if after == 0 || rule.Scope.OfflineAfterSec < after {
				after = rule.Scope.OfflineAfterSec
			}
			rules = append(rules, rule)
		}
		if len(rules) == 0 {
			return nil // nothing subscribed: do not write events nobody asked for
		}
		var rows []struct {
			GatewayID, ExternalID, Name string
			LastSeen                    time.Time
			Offline                     bool
		}
		// Streams silent for more than a week are considered retired, not newly offline. Streams whose liveness is
		// 'reported' (Zigbee2MQTT devices) are skipped: a wall switch is silent until someone flips it, and its
		// offline/online state arrives as availability messages instead (CaptureZ2M).
		if e := tx.Raw(`SELECT s.gateway_id,s.external_id,s.name,s.last_seen,coalesce(st.offline,false) AS offline FROM core.sensor_streams s
      JOIN core.gateways g ON g.id=s.gateway_id AND g.tenant_id=s.tenant_id AND g.revoked_at IS NULL
      LEFT JOIN core.stream_state st ON st.tenant_id=s.tenant_id AND st.gateway_id=s.gateway_id AND st.external_id=s.external_id
      WHERE s.last_seen<? AND s.last_seen>? AND s.liveness='silence'
      AND NOT EXISTS(SELECT 1 FROM core.devices d JOIN core.sensor_streams o ON o.tenant_id=d.tenant_id AND o.external_id=d.external_id AND o.gateway_id<>s.gateway_id
        JOIN core.gateways og ON og.tenant_id=o.tenant_id AND og.id=o.gateway_id AND og.revoked_at IS NULL
        WHERE d.tenant_id=s.tenant_id AND d.external_id=s.external_id AND d.roaming AND d.removed_at IS NULL AND (o.last_seen,o.gateway_id)>(s.last_seen,s.gateway_id))
      ORDER BY s.last_seen LIMIT 200 FOR UPDATE OF s`, now.Add(-time.Duration(after)*time.Second), now.Add(-7*24*time.Hour)).Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			var ev domain.DeviceEvent
			if !row.Offline {
				var prev []stateRow
				if e := tx.Raw(`SELECT `+stateColumns+` FROM core.stream_state WHERE gateway_id=? AND external_id=?`, row.GatewayID, row.ExternalID).Scan(&prev).Error; e != nil {
					return e
				}
				st := alerts.State{Known: true}
				if len(prev) == 1 {
					st = prev[0].state()
				}
				st.Offline = true
				if _, e := upsertState(tx, tenant, row.GatewayID, row.ExternalID, st); e != nil {
					return e
				}
				ev = domain.DeviceEvent{GatewayID: row.GatewayID, ExternalID: row.ExternalID, DeviceName: row.Name, EventType: domain.EventOffline, Detail: map[string]any{"last_seen": row.LastSeen, "after_sec": after}, OccurredAt: now}
				if e := insertEvent(tx, tenant, &ev); e != nil {
					return e
				}
				count++
			} else {
				var events []eventRow
				if e := tx.Raw(`SELECT id,gateway_id,external_id,device_name,event_type,detail,occurred_at FROM core.device_events WHERE gateway_id=? AND external_id=? AND event_type='offline' ORDER BY occurred_at DESC LIMIT 1`, row.GatewayID, row.ExternalID).Scan(&events).Error; e != nil {
					return e
				}
				if len(events) != 1 {
					continue
				}
				ev = events[0].event()
			}
			silent := now.Sub(row.LastSeen)
			var due []domain.AlertRule
			for _, rule := range rules {
				if silent < time.Duration(rule.Scope.OfflineAfterSec)*time.Second {
					continue
				}
				var n int64 // one alert per rule per offline episode
				if e := tx.Raw(`SELECT count(*) FROM core.alerts WHERE rule_id=? AND external_id=? AND event_id=?`, rule.ID, row.ExternalID, ev.ID).Scan(&n).Error; e != nil {
					return e
				}
				if n == 0 {
					due = append(due, rule)
				}
			}
			if r.opts.AlertsShadow {
				continue
			}
			if e := createAlerts(tx, tenant, ev, due); e != nil {
				return e
			}
		}
		return nil
	})
	return count, e
}

// ScanVacancy ends every occupied episode whose PIR has reported no motion for alerts.OccupancyHoldSec:
// one `vacant` event per episode, raised here on the worker's clock and never on an uplink (a PIR that
// sees nobody mostly keeps quiet, so no uplink would ever say "the room is empty"). last_motion_at is
// the server receive time of the last motion=1 uplink; device clocks play no part.
//
// Lock order mirrors the ingest path (gateway row first, then its stream_state rows) and every lock is
// taken with SKIP LOCKED: a gateway or stream an uplink is holding is simply left for the next tick, so
// this scan never waits on, or deadlocks with, CapturePacket (gateway -> roaming lock -> stream_state).
func (r *Repository) ScanVacancy(ctx context.Context, tenant string, now time.Time) (int, error) {
	count := 0
	cutoff := now.Add(-alerts.OccupancyHoldSec * time.Second)
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var gateways []string
		if e := tx.Raw(`SELECT id FROM core.gateways WHERE revoked_at IS NULL
      AND id IN (SELECT gateway_id FROM core.stream_state WHERE occupied=1 AND last_motion_at<=?)
      ORDER BY id LIMIT 200 FOR UPDATE SKIP LOCKED`, cutoff).Scan(&gateways).Error; e != nil {
			return e
		}
		if len(gateways) == 0 {
			return nil
		}
		var rows []struct {
			GatewayID, ExternalID, Name string
			LastMotionAt                time.Time
		}
		if e := tx.Raw(`WITH due AS (
        SELECT tenant_id,gateway_id,external_id FROM core.stream_state
        WHERE gateway_id IN ? AND occupied=1 AND last_motion_at<=? ORDER BY last_motion_at LIMIT 200 FOR UPDATE SKIP LOCKED)
      UPDATE core.stream_state st SET occupied=0,updated_at=now() FROM due
      WHERE st.tenant_id=due.tenant_id AND st.gateway_id=due.gateway_id AND st.external_id=due.external_id
      RETURNING st.gateway_id,st.external_id,st.last_motion_at,
        coalesce((SELECT name FROM core.sensor_streams s WHERE s.tenant_id=st.tenant_id AND s.gateway_id=st.gateway_id AND s.external_id=st.external_id),st.external_id) AS name`,
			gateways, cutoff).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) == 0 {
			return nil
		}
		rules, e := loadRules(tx, true)
		if e != nil {
			return e
		}
		for _, row := range rows {
			ev := domain.DeviceEvent{GatewayID: row.GatewayID, ExternalID: row.ExternalID, DeviceName: row.Name, EventType: domain.EventVacant, OccurredAt: now,
				Detail: map[string]any{"last_motion": row.LastMotionAt, "hold_sec": alerts.OccupancyHoldSec}}
			if e := insertEvent(tx, tenant, &ev); e != nil {
				return e
			}
			count++
			if r.opts.AlertsShadow {
				continue
			}
			// No rule type subscribes to vacant today; kept so a future one needs no second code path.
			if e := createAlerts(tx, tenant, ev, rules); e != nil {
				return e
			}
		}
		return nil
	})
	return count, e
}

func backoff(attempts int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempts && d < 30*time.Minute; i++ {
		d *= 2
	}
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}

// ClaimNotifications reserves due deliveries (SKIP LOCKED) and returns everything needed to send them.
func (r *Repository) ClaimNotifications(ctx context.Context, tenant string, limit int, now time.Time) ([]domain.NotificationJob, error) {
	out := []domain.NotificationJob{}
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		// A demo workspace (cmd/demo-twin) is fictional people and sensors: its alerts are never delivered, whatever
		// channels someone adds to it (the channel test button still sends). Notifications stay queued and are pruned
		// with the alerts.
		var demo bool
		if e := tx.Raw(`SELECT coalesce((SELECT demo FROM core.tenants WHERE id=?),false)`, tenant).Scan(&demo).Error; e != nil {
			return e
		}
		if demo {
			return nil
		}
		var claimed []struct {
			ID, AlertID string
			ChannelID   *string
			Attempts    int
		}
		// The claim hides the row for longer than a whole batch can take to send, so another replica cannot
		// pick it up mid-flight; FinishNotification then schedules the real retry time.
		if e := tx.Raw(`UPDATE core.notifications SET attempts=attempts+1,next_attempt_at=?
      WHERE id IN (SELECT id FROM core.notifications WHERE status='queued' AND next_attempt_at<=? ORDER BY next_attempt_at LIMIT ? FOR UPDATE SKIP LOCKED)
      RETURNING id,alert_id,channel_id,attempts`, now.Add(5*time.Minute), now, limit).Scan(&claimed).Error; e != nil {
			return e
		}
		for _, c := range claimed {
			job := domain.NotificationJob{Notification: domain.Notification{ID: c.ID, AlertID: c.AlertID, ChannelID: c.ChannelID, Attempts: c.Attempts, Status: "queued"}}
			if c.ChannelID == nil {
				job.Channel = domain.NotificationChannel{}
			} else {
				var ch []struct {
					ID, Name, Kind string
					Enabled        bool
					Config         json.RawMessage
					SecretEnc      *string
					ProjectID      *string
				}
				if e := tx.Raw(`SELECT id,name,kind,enabled,config,secret_enc,project_id::text AS project_id FROM core.notification_channels WHERE id=?`, *c.ChannelID).Scan(&ch).Error; e != nil {
					return e
				}
				if len(ch) == 1 && ch[0].Enabled {
					job.Channel = domain.NotificationChannel{ID: ch[0].ID, Name: ch[0].Name, Kind: ch[0].Kind, Enabled: true, Config: map[string]string{}, ProjectID: ch[0].ProjectID}
					_ = json.Unmarshal(ch[0].Config, &job.Channel.Config)
					if ch[0].SecretEnc != nil {
						job.SecretEnc = *ch[0].SecretEnc
						job.Channel.HasSecret = true
					}
				}
			}
			alertsFound, e := scanAlerts(tx, `WHERE a.id=?`, 1, c.AlertID)
			if e != nil {
				return e
			}
			if len(alertsFound) == 1 {
				job.Alert = alertsFound[0]
				var events []eventRow
				if e := tx.Raw(`SELECT id,gateway_id,external_id,device_name,event_type,detail,occurred_at FROM core.device_events WHERE id=?`, job.Alert.EventID).Scan(&events).Error; e != nil {
					return e
				}
				if len(events) == 1 {
					job.Event = events[0].event()
				}
				// A project channel only ever carries its project's alerts. The gateway may have moved since the
				// alert was queued, so the check uses its project now and fails the delivery if they differ.
				if job.Channel.ID != "" && job.Channel.ProjectID != nil {
					var current []struct{ ProjectID *string }
					if e := tx.Raw(`SELECT project_id::text AS project_id FROM core.gateways WHERE id=?`, job.Alert.GatewayID).Scan(&current).Error; e != nil {
						return e
					}
					if len(current) != 1 || current[0].ProjectID == nil || *current[0].ProjectID != *job.Channel.ProjectID {
						job.FailReason = "channel belongs to another project than the device's gateway"
					}
				}
				// A "members" email goes to the people who can see this alert's project, decided now, so a
				// member added or removed since the alert opened is reflected. The lookup runs in a savepoint:
				// a failure fails this one delivery, not the whole batch the claim above reserved.
				if job.FailReason == "" && job.Channel.Kind == "email" && job.Channel.Config["audience"] == alerts.AudienceMembers {
					to, reason, e := memberRecipients(tx, job.Alert.ID, job.Channel.Config)
					if e != nil {
						return e
					}
					job.FailReason, job.RetryReason = reason.fail, reason.retry
					job.Channel.Config["to"] = strings.Join(to, ",")
				}
				var name string
				_ = tx.Raw(`SELECT name FROM core.gateways WHERE id=?`, job.Alert.GatewayID).Scan(&name).Error
				job.GatewayName = name
			}
			out = append(out, job)
		}
		return nil
	})
	return out, e
}

// recipientProblem: fail ends the delivery (retrying cannot help), retry leaves it queued for the next attempt.
type recipientProblem struct{ fail, retry string }

// memberRecipients resolves a "members" email for one alert inside a savepoint. A lookup error rolls back only
// the savepoint and is retried with the usual backoff; a list longer than the cap is cut, owners first, and logged.
func memberRecipients(tx *gorm.DB, alert string, config map[string]string) ([]string, recipientProblem, error) {
	roles, ok := alerts.MemberRoles(config)
	if !ok {
		return nil, recipientProblem{fail: "members channel has invalid roles"}, nil
	}
	if e := tx.SavePoint("recipients").Error; e != nil {
		return nil, recipientProblem{}, e
	}
	var to []string
	if e := tx.Raw(`SELECT email FROM core.alert_recipients(?::uuid, ?::text[])`, alert, pgArray(roles)).Scan(&to).Error; e != nil {
		slog.Warn("alert email: recipient lookup failed", "alert", alert)
		if rb := tx.RollbackTo("recipients").Error; rb != nil {
			return nil, recipientProblem{}, rb
		}
		return nil, recipientProblem{retry: "recipient lookup failed"}, nil
	}
	if e := tx.Exec(`RELEASE SAVEPOINT recipients`).Error; e != nil {
		return nil, recipientProblem{}, e
	}
	if len(to) == 0 {
		return nil, recipientProblem{fail: "no member can see this alert's project"}, nil
	}
	if len(to) > alerts.MaxMemberRecipients {
		slog.Warn("alert email: members truncated", "alert", alert, "members", len(to), "limit", alerts.MaxMemberRecipients)
		to = to[:alerts.MaxMemberRecipients]
	}
	return to, recipientProblem{}, nil
}

// FinishNotification records the outcome: "sent", "retry" (backoff, failed after 5 attempts) or "failed" (terminal).
func (r *Repository) FinishNotification(ctx context.Context, tenant, id, outcome, message string) error {
	if len(message) > 200 {
		message = message[:200]
	}
	return r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		switch outcome {
		case "sent":
			return tx.Exec(`UPDATE core.notifications SET status='sent',sent_at=now(),last_error=NULL WHERE id=? AND status='queued'`, id).Error
		case "failed":
			return tx.Exec(`UPDATE core.notifications SET status='failed',last_error=? WHERE id=? AND status='queued'`, message, id).Error
		}
		var attempts int
		if e := tx.Raw(`SELECT attempts FROM core.notifications WHERE id=?`, id).Scan(&attempts).Error; e != nil {
			return e
		}
		return tx.Exec(`UPDATE core.notifications SET last_error=?,next_attempt_at=now()+make_interval(secs => ?),status=CASE WHEN attempts>=5 THEN 'failed' ELSE status END WHERE id=? AND status='queued'`, message, backoff(attempts).Seconds(), id).Error
	})
}

// PruneAlertData bounds the append-only tables per tenant (same spirit as packet/telemetry pruning).
func (r *Repository) PruneAlertData(ctx context.Context, tenant string) error {
	return r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		if e := tx.Exec(`DELETE FROM core.notifications WHERE status<>'queued' AND id IN (SELECT id FROM core.notifications WHERE status<>'queued' ORDER BY created_at DESC,id DESC OFFSET 2000)`).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.notifications WHERE alert_id IN (SELECT id FROM core.alerts WHERE status='resolved' ORDER BY opened_at DESC,id DESC OFFSET 2000)`).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.alerts WHERE status='resolved' AND id IN (SELECT id FROM core.alerts WHERE status='resolved' ORDER BY opened_at DESC,id DESC OFFSET 2000)`).Error; e != nil {
			return e
		}
		// Button and remote `action` events have their own, smaller budget and are pruned first, so a chatty
		// remote can never push door, alarm or offline history out of the 5000-event log.
		if e := tx.Exec(`DELETE FROM core.device_events e WHERE e.id IN (SELECT id FROM core.device_events WHERE event_type=? ORDER BY occurred_at DESC,id DESC OFFSET ?)`, domain.EventAction, maxActionEvents).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.device_events e WHERE e.id IN (SELECT id FROM core.device_events WHERE event_type<>? ORDER BY occurred_at DESC,id DESC OFFSET 5000) AND NOT EXISTS(SELECT 1 FROM core.alerts a WHERE a.event_id=e.id)`, domain.EventAction).Error; e != nil {
			return e
		}
		// Settled commands are kept 90 days (the audit log keeps who sent them for as long as it lives).
		return tx.Exec(`DELETE FROM core.device_commands WHERE created_at<now()-interval '90 days' AND status NOT IN ('pending','sent')`).Error
	})
}

// SetChannelEnabled toggles delivery without touching the sealed secret.
func (r *Repository) SetChannelEnabled(ctx context.Context, p domain.Principal, id string, enabled bool) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if _, e := writableRow(tx, "notification_channels", id); e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.notification_channels SET enabled=? WHERE id=?`, enabled, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "channel.updated", id)
	})
}

type eventRow struct {
	ID, GatewayID, ExternalID, DeviceName, EventType string
	Detail                                           json.RawMessage
	OccurredAt                                       time.Time
}

func (row eventRow) event() domain.DeviceEvent {
	ev := domain.DeviceEvent{ID: row.ID, GatewayID: row.GatewayID, ExternalID: row.ExternalID, DeviceName: row.DeviceName, EventType: row.EventType, OccurredAt: row.OccurredAt, Detail: map[string]any{}}
	_ = json.Unmarshal(row.Detail, &ev.Detail)
	return ev
}

func (r *Repository) ListEvents(ctx context.Context, p domain.Principal, external string, limit int) ([]domain.DeviceEvent, error) {
	out := []domain.DeviceEvent{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []eventRow
		q := tx.Raw(`SELECT id,gateway_id,external_id,device_name,event_type,detail,occurred_at FROM core.device_events WHERE (?::text='' OR external_id=?::text) ORDER BY occurred_at DESC,id DESC LIMIT ?`, external, external, limit)
		if e := q.Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			out = append(out, row.event())
		}
		return nil
	})
	return out, e
}

func scanAlerts(tx *gorm.DB, where string, limit int, args ...any) ([]domain.Alert, error) {
	var rows []domain.Alert
	args = append(args, limit)
	e := tx.Raw(`SELECT a.id,a.rule_id,a.event_id,a.gateway_id,a.external_id,a.device_name,a.event_type,a.severity,a.title,a.status,a.opened_at,a.acked_by,a.acked_at,a.resolved_by,a.resolved_at,a.note FROM core.alerts a `+where+` ORDER BY a.opened_at DESC,a.id DESC LIMIT ?`, args...).Scan(&rows).Error
	if rows == nil {
		rows = []domain.Alert{}
	}
	return rows, e
}

func (r *Repository) ListAlerts(ctx context.Context, p domain.Principal, status string, limit int) ([]domain.Alert, error) {
	var out []domain.Alert
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var e error
		out, e = scanAlerts(tx, `WHERE (?::text='' OR a.status=?::text)`, limit, status, status)
		return e
	})
	return out, e
}

func (r *Repository) AlertCounts(ctx context.Context, p domain.Principal) (map[string]int, error) {
	out := map[string]int{"open": 0, "acknowledged": 0}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []struct {
			Status string
			N      int
		}
		if e := tx.Raw(`SELECT status,count(*) AS n FROM core.alerts WHERE status<>'resolved' GROUP BY status`).Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			out[row.Status] = row.N
		}
		return nil
	})
	return out, e
}

func (r *Repository) TransitionAlert(ctx context.Context, p domain.Principal, id, to, note string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var res *gorm.DB
		if to == "acknowledged" {
			res = tx.Exec(`UPDATE core.alerts SET status='acknowledged',acked_by=?,acked_at=now() WHERE id=? AND status='open'`, p.UserID, id)
		} else {
			res = tx.Exec(`UPDATE core.alerts SET status='resolved',resolved_by=?,resolved_at=now(),note=NULLIF(?,'') WHERE id=? AND status<>'resolved'`, p.UserID, note, id)
		}
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "alert", ""); e != nil {
			return e
		}
		return audit(tx, p, "alert."+to, id)
	})
}

func (r *Repository) ListRules(ctx context.Context, p domain.Principal) ([]domain.AlertRule, error) {
	var out []domain.AlertRule
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var e error
		if out, e = loadRules(tx, false); e != nil {
			return e
		}
		all, e := scopeAll(tx)
		if e != nil || all {
			return e
		}
		// A member limited to some projects sees workspace-wide rules read-only; their device and gateway
		// lists may name other projects' hardware, so they are trimmed to what this member can see.
		var devices, gateways []string
		if e := tx.Raw(`SELECT lower(external_id) FROM core.sensor_streams UNION SELECT lower(external_id) FROM core.devices WHERE removed_at IS NULL`).Scan(&devices).Error; e != nil {
			return e
		}
		if e := tx.Raw(`SELECT id::text FROM core.gateways`).Scan(&gateways).Error; e != nil {
			return e
		}
		for i := range out {
			if out[i].ProjectID == nil {
				out[i].Scope.ExternalIDs = keepKnown(out[i].Scope.ExternalIDs, devices, true)
				out[i].Scope.GatewayIDs = keepKnown(out[i].Scope.GatewayIDs, gateways, false)
			}
		}
		return nil
	})
	if out == nil {
		out = []domain.AlertRule{}
	}
	return out, e
}

func (r *Repository) SaveRule(ctx context.Context, p domain.Principal, rule domain.AlertRule, create bool) error {
	scope, _ := json.Marshal(rule.Scope)
	channels, _ := json.Marshal(rule.Channels)
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if !create {
			current, e := writableRow(tx, "alert_rules", rule.ID)
			if e != nil {
				return e
			}
			// An update that does not name a project keeps the rule where it is.
			if rule.KeepProject {
				rule.ProjectID = current
			}
		}
		if create || !rule.KeepProject {
			if e := checkAlertProject(tx, rule.ProjectID); e != nil {
				return e
			}
		}
		// A workspace rule may use workspace channels; a project rule also its own project's channels.
		if len(rule.Channels) > 0 {
			var n int64
			if e := tx.Raw(`SELECT count(DISTINCT id) FROM core.notification_channels WHERE id = ANY(?::uuid[]) AND (project_id IS NULL OR project_id=?::uuid)`, pgArray(rule.Channels), rule.ProjectID).Scan(&n).Error; e != nil {
				return e
			}
			if int(n) != len(distinct(rule.Channels)) {
				return domain.ErrInvalid
			}
		}
		if create {
			// Check-then-insert under the workspace's alert-configuration lock, so two creates cannot both
			// pass the cap.
			if e := alertConfigLock(tx, p.TenantID); e != nil {
				return e
			}
			var n int64
			if e := tx.Raw(`SELECT core.alert_rule_count()`).Scan(&n).Error; e != nil {
				return e
			}
			if n >= maxRules {
				return domain.ErrConflict
			}
			if e := tx.Exec(`INSERT INTO core.alert_rules(tenant_id,id,name,enabled,event_type,severity,scope,channels,dedupe_sec,project_id) VALUES(?,?,?,?,?,?,?::jsonb,?::jsonb,?,?::uuid)`, p.TenantID, rule.ID, rule.Name, rule.Enabled, rule.EventType, rule.Severity, string(scope), string(channels), rule.DedupeSec, rule.ProjectID).Error; e != nil {
				return e
			}
			return audit(tx, p, "rule.created", rule.ID)
		}
		res := tx.Exec(`UPDATE core.alert_rules SET name=?,enabled=?,event_type=?,severity=?,scope=?::jsonb,channels=?::jsonb,dedupe_sec=?,project_id=?::uuid,updated_at=now() WHERE id=?`, rule.Name, rule.Enabled, rule.EventType, rule.Severity, string(scope), string(channels), rule.DedupeSec, rule.ProjectID, rule.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "rule.updated", rule.ID)
	}))
}

func (r *Repository) DeleteRule(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if _, e := writableRow(tx, "alert_rules", id); e != nil {
			return e
		}
		res := tx.Exec(`DELETE FROM core.alert_rules WHERE id=?`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "rule.deleted", id)
	})
}

type channelRow struct {
	ID, Name, Kind string
	Enabled        bool
	Config         json.RawMessage
	SecretEnc      *string
	ProjectID      *string
	CreatedAt      time.Time
}

func (row channelRow) channel() domain.NotificationChannel {
	ch := domain.NotificationChannel{ID: row.ID, Name: row.Name, Kind: row.Kind, Enabled: row.Enabled, Config: map[string]string{}, HasSecret: row.SecretEnc != nil && *row.SecretEnc != "", ProjectID: row.ProjectID, CreatedAt: row.CreatedAt}
	_ = json.Unmarshal(row.Config, &ch.Config)
	return ch
}

func (r *Repository) ListChannels(ctx context.Context, p domain.Principal) ([]domain.NotificationChannel, error) {
	out := []domain.NotificationChannel{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []channelRow
		if e := tx.Raw(`SELECT id,name,kind,enabled,config,secret_enc,project_id::text AS project_id,created_at FROM core.notification_channels ORDER BY created_at,id LIMIT 51`).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) > 50 {
			slog.Warn("notification channels truncated", "limit", 50)
			rows = rows[:50]
		}
		all, e := scopeAll(tx)
		if e != nil {
			return e
		}
		for _, row := range rows {
			ch := row.channel()
			// A project-limited member cannot manage workspace channels; their targets are credentials.
			if !all && ch.ProjectID == nil {
				ch.Config = map[string]string{}
			}
			out = append(out, ch)
		}
		return nil
	})
	return out, e
}

func (r *Repository) CreateChannel(ctx context.Context, p domain.Principal, ch domain.NotificationChannel, secretEnc string) error {
	config, _ := json.Marshal(ch.Config)
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := checkAlertProject(tx, ch.ProjectID); e != nil {
			return e
		}
		if e := alertConfigLock(tx, p.TenantID); e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT core.notification_channel_count()`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= maxChannels {
			return domain.ErrConflict
		}
		var secret *string
		if secretEnc != "" {
			secret = &secretEnc
		}
		if e := tx.Exec(`INSERT INTO core.notification_channels(tenant_id,id,name,kind,enabled,config,secret_enc,project_id) VALUES(?,?,?,?,?,?::jsonb,?,?::uuid)`, p.TenantID, ch.ID, ch.Name, ch.Kind, ch.Enabled, string(config), secret, ch.ProjectID).Error; e != nil {
			return e
		}
		return audit(tx, p, "channel.created", ch.ID)
	}))
}

func (r *Repository) DeleteChannel(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if _, e := writableRow(tx, "notification_channels", id); e != nil {
			return e
		}
		res := tx.Exec(`DELETE FROM core.notification_channels WHERE id=?`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "channel.deleted", id)
	})
}

// ChannelWithSecret is used for test sends only; the sealed secret never leaves the process.
func (r *Repository) ChannelWithSecret(ctx context.Context, p domain.Principal, id string) (domain.NotificationChannel, string, error) {
	var ch domain.NotificationChannel
	var secret string
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []channelRow
		if _, e := writableRow(tx, "notification_channels", id); e != nil {
			return e
		}
		if e := tx.Raw(`SELECT id,name,kind,enabled,config,secret_enc,project_id::text AS project_id,created_at FROM core.notification_channels WHERE id=?`, id).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrNotFound
		}
		ch = rows[0].channel()
		if rows[0].SecretEnc != nil {
			secret = *rows[0].SecretEnc
		}
		return nil
	})
	return ch, secret, e
}

func (r *Repository) ListNotifications(ctx context.Context, p domain.Principal, limit int) ([]domain.Notification, error) {
	out := []domain.Notification{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT id,alert_id,channel_id,status,attempts,next_attempt_at,last_error,created_at,sent_at FROM core.notifications ORDER BY created_at DESC,id DESC LIMIT ?`, limit).Scan(&out).Error
	})
	if out == nil {
		out = []domain.Notification{}
	}
	return out, e
}

// alertConfigLock serialises rule and channel creation per workspace (advisory key 10).
func alertConfigLock(tx *gorm.DB, tenant string) error {
	return tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,10))`, tenant).Error
}

// scopeAll reports whether the transaction's member sees every project (owner, unrestricted member, workers).
func scopeAll(tx *gorm.DB) (bool, error) {
	var all bool
	e := tx.Raw(`SELECT core.scope_all()`).Scan(&all).Error
	return all, e
}

// checkAlertProject: a workspace-wide rule or channel needs full scope; a project one needs a project the
// member can see (core.projects is project-scoped, so an invisible or foreign id counts as unknown).
func checkAlertProject(tx *gorm.DB, project *string) error {
	all, e := scopeAll(tx)
	if e != nil {
		return e
	}
	if project == nil {
		if !all {
			return domain.ErrForbidden
		}
		return nil
	}
	// An archived project takes no new rules or channels; core.projects is project-scoped, so an invisible
	// or foreign id counts as unknown too.
	var n int64
	if e := tx.Raw(`SELECT count(*) FROM core.projects WHERE id=?::uuid AND archived_at IS NULL`, *project).Scan(&n).Error; e != nil {
		return e
	}
	if n != 1 {
		return domain.ErrInvalid
	}
	return nil
}

// writableRow tells "not there" (404) from "there, but workspace-wide or another project's" (403) before an
// update or delete, which RLS would otherwise both turn into zero rows, and returns the row's project. The
// table name is a constant.
func writableRow(tx *gorm.DB, table, id string) (*string, error) {
	var rows []struct{ ProjectID *string }
	if e := tx.Raw(`SELECT project_id::text AS project_id FROM core.`+table+` WHERE id=?::uuid`, id).Scan(&rows).Error; e != nil {
		return nil, e
	}
	if len(rows) != 1 {
		return nil, domain.ErrNotFound
	}
	all, e := scopeAll(tx)
	if e != nil {
		return nil, e
	}
	if !all && rows[0].ProjectID == nil {
		return nil, domain.ErrForbidden
	}
	return rows[0].ProjectID, nil
}

// keepKnown filters ids to the ones in known (compared lower-case when fold is set), keeping the order.
func keepKnown(ids, known []string, fold bool) []string {
	if len(ids) == 0 {
		return ids
	}
	set := make(map[string]bool, len(known))
	for _, k := range known {
		set[k] = true
	}
	out := []string{}
	for _, id := range ids {
		key := id
		if fold {
			key = strings.ToLower(id)
		}
		if set[key] {
			out = append(out, id)
		}
	}
	return out
}

func distinct(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
