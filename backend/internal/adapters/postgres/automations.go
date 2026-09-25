package postgres

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/automation"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	// maxAutomations bounds both the per-tenant quota and the number of flows one uplink evaluates.
	// Raised from 100 once the trigger index landed: an uplink no longer pays for a flow it cannot
	// wake, so the quota bounds storage and the studio's list, not the cost of ingest. What an uplink
	// still pays for is the flows the index hands back and, of those, only the ones with a firing —
	// so 500 is the point where the two index lookups and, in the worst case, 500 parsed definitions
	// stay comfortably inside a 5 s packet interval. It is a structural bound, not a measured one; the
	// integration benchmark in tests/automation_scale_test.go is what confirms it on real hardware.
	maxAutomations = 500
	// automationBatch bounds one multi-row statement. Postgres takes 65535 parameters per statement
	// and a packet can probe a few thousand blocks, so the batched state writes are chunked.
	automationBatch = 400
	// automationDedupe: one firing per automation per identity per minute, whatever the uplink rate.
	automationDedupe = 60 * time.Second
	// automationRunHistory is how many runs are kept per flow; older ones are pruned on each write.
	automationRunHistory = 200
	// automationEvent is the device event type recorded when a flow fires without one of its own,
	// so every alert an automation opens has a real event row to reference.
	automationEvent = "automation"
)

type automationRow struct {
	ID, Name, Description string
	Enabled               bool
	ProjectID             *string
	Definition            json.RawMessage
	Revision              int
	CreatedAt, UpdatedAt  time.Time
	LastFiredAt           *time.Time
	FireCount             int64
}

func (row automationRow) record() automation.Automation {
	return automation.Automation{
		ID: row.ID, Name: row.Name, Description: row.Description, Enabled: row.Enabled, ProjectID: row.ProjectID,
		Definition: row.Definition, Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		LastFiredAt: row.LastFiredAt, FireCount: row.FireCount,
	}
}

const automationColumns = `id,name,description,enabled,project_id,definition,revision,created_at,updated_at,last_fired_at,fire_count`

func (r *Repository) ListAutomations(ctx context.Context, p domain.Principal) ([]automation.Automation, error) {
	out := []automation.Automation{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []automationRow
		if e := tx.Raw(`SELECT ` + automationColumns + ` FROM core.automations ORDER BY created_at,id LIMIT ` + strconv.Itoa(maxAutomations)).Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			out = append(out, row.record())
		}
		return nil
	})
	return out, e
}

func (r *Repository) GetAutomation(ctx context.Context, p domain.Principal, id string) (automation.Automation, error) {
	var out automation.Automation
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		row, e := loadAutomation(tx, id)
		if e != nil {
			return e
		}
		out = row.record()
		return nil
	})
	return out, e
}

func loadAutomation(tx *gorm.DB, id string) (automationRow, error) {
	var rows []automationRow
	if e := tx.Raw(`SELECT `+automationColumns+` FROM core.automations WHERE id=?`, id).Scan(&rows).Error; e != nil {
		return automationRow{}, e
	}
	if len(rows) != 1 {
		return automationRow{}, domain.ErrNotFound
	}
	return rows[0], nil
}

// AutomationChannels lists the notification channel ids the tenant owns, so the studio can be told
// which action.notify blocks point at a channel that no longer exists.
func (r *Repository) AutomationChannels(ctx context.Context, p domain.Principal) (map[string]bool, error) {
	out := map[string]bool{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return channelSet(tx, out)
	})
	return out, e
}

func channelSet(tx *gorm.DB, out map[string]bool) error {
	var ids []string
	if e := tx.Raw(`SELECT id FROM core.notification_channels LIMIT 200`).Scan(&ids).Error; e != nil {
		return e
	}
	for _, id := range ids {
		out[id] = true
	}
	return nil
}

// checkSize applies to every stored definition, enabled or not: a disabled draft can still be dry-run, and its
// condition blocks cost database lookups.
func checkSize(raw json.RawMessage) error {
	if len(raw) > automation.MaxDefinitionBytes {
		return domain.ErrInvalid
	}
	var def automation.Definition
	if e := json.Unmarshal(raw, &def); e != nil || len(def.Nodes) > automation.MaxNodes || len(def.Edges) > automation.MaxEdges {
		return domain.ErrInvalid
	}
	return nil
}

// checkDefinition is the backstop every write goes through: whatever the route decided, a definition that does
// not validate against the tenant's own channels and devices — or a flow with device commands switched on while
// AUTOMATION_COMMANDS is off, or by a member who may not control devices — never reaches the table.
func (r *Repository) checkDefinition(tx *gorm.DB, p domain.Principal, project *string, raw json.RawMessage, enabled bool) error {
	if len(raw) > automation.MaxDefinitionBytes {
		return domain.ErrInvalid
	}
	var def automation.Definition
	if e := json.Unmarshal(raw, &def); e != nil {
		return domain.ErrInvalid
	}
	opts, e := r.checkOptions(tx, &p, project, def, enabled)
	if e != nil {
		return e
	}
	for _, problem := range automation.Check(def, opts) {
		if problem.Code == "command_forbidden" {
			return domain.Because(domain.ErrForbidden, "control")
		}
	}
	if e := automation.Validate(def, opts); e != nil {
		return domain.ErrInvalid
	}
	return nil
}

// armed records who switched a flow on. When the flow commands devices, that member is the actor of every
// command it queues, and the enabling is audited as such.
func armed(tx *gorm.DB, p domain.Principal, id string, raw json.RawMessage) error {
	if e := tx.Exec(`UPDATE core.automations SET enabled_by=? WHERE id=?`, p.UserID, id).Error; e != nil {
		return e
	}
	var def automation.Definition
	if json.Unmarshal(raw, &def) == nil && automation.HasCommand(def) {
		return audit(tx, p, "automation.commands_armed", id)
	}
	return nil
}

// A project flow cannot read another project's devices through condition blocks.
func checkFlowProject(tx *gorm.DB, project *string, raw json.RawMessage) error {
	if project == nil {
		return nil
	}
	if e := projectExists(tx, project); e != nil {
		return e
	}
	var def automation.Definition
	if e := json.Unmarshal(raw, &def); e != nil {
		return domain.ErrInvalid
	}
	for _, node := range def.Nodes {
		for _, id := range node.Data.GatewayIDs {
			var n int64
			if e := tx.Raw(`SELECT count(*) FROM core.gateways WHERE id=? AND project_id=? AND revoked_at IS NULL`, id, *project).Scan(&n).Error; e != nil {
				return e
			}
			if n != 1 {
				return domain.ErrInvalid
			}
		}
		if node.Type == automation.ActionCommand && node.Data.DeviceID != "" {
			// A command target is a registration, addressed by its id: it must sit on a gateway of this project.
			var n int64
			if e := tx.Raw(`SELECT count(*) FROM core.devices d JOIN core.gateways g ON g.id=d.gateway_id AND g.tenant_id=d.tenant_id
      WHERE d.id::text=lower(?) AND d.removed_at IS NULL AND g.project_id=? AND g.revoked_at IS NULL`, node.Data.DeviceID, *project).Scan(&n).Error; e != nil {
				return e
			}
			if n != 1 {
				return domain.ErrInvalid
			}
		}
		ids := append([]string{}, node.Data.ExternalIDs...)
		if node.Data.ExternalID != "" {
			ids = append(ids, node.Data.ExternalID)
		}
		for _, id := range ids {
			var n int64
			if e := tx.Raw(`SELECT count(*) FROM core.sensor_streams s JOIN core.gateways g ON g.id=s.gateway_id AND g.tenant_id=s.tenant_id WHERE lower(s.external_id)=lower(?) AND g.project_id=? AND g.revoked_at IS NULL`, id, *project).Scan(&n).Error; e != nil {
				return e
			}
			if n == 0 {
				if e := tx.Raw(`SELECT count(*) FROM core.devices d JOIN core.gateways g ON g.id=d.gateway_id AND g.tenant_id=d.tenant_id WHERE lower(d.external_id)=lower(?) AND d.removed_at IS NULL AND g.project_id=? AND g.revoked_at IS NULL`, id, *project).Scan(&n).Error; e != nil {
					return e
				}
				if n == 0 {
					return domain.ErrInvalid
				}
			}
		}
	}
	return nil
}

// reindexTriggers rewrites one flow's rows in core.automation_triggers, inside the caller's
// transaction, so the index and core.automations can never disagree: every write that can change
// what a flow reacts to goes through here. Rows exist only for enabled flows, which is what lets an
// uplink skip a disabled flow without reading core.automations at all.
func reindexTriggers(tx *gorm.DB, tenant, id string, raw json.RawMessage, enabled bool) error {
	if e := tx.Exec(`DELETE FROM core.automation_triggers WHERE automation_id=?`, id).Error; e != nil {
		return e
	}
	if !enabled || len(raw) == 0 {
		return nil
	}
	var def automation.Definition
	if e := json.Unmarshal(raw, &def); e != nil {
		// An unreadable definition indexes nothing; it cannot fire either. Enabled flows have passed
		// checkDefinition, so this is only a backstop against a definition edited underneath us.
		return nil
	}
	rows := automation.TriggerIndex(def)
	for start := 0; start < len(rows); start += automationBatch {
		end := min(start+automationBatch, len(rows))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*8)
		for _, row := range rows[start:end] {
			values = append(values, `(?::uuid,?::uuid,?::text,?::text,?::text,?::text,?::uuid,?::text)`)
			args = append(args, tenant, id, row.NodeID, row.Kind, row.EventType, row.ExternalID, row.GatewayID, row.Metric)
		}
		if e := tx.Exec(`INSERT INTO core.automation_triggers(tenant_id,automation_id,node_id,kind,event_type,external_id,gateway_id,metric) VALUES `+
			strings.Join(values, ","), args...).Error; e != nil {
			return e
		}
	}
	return nil
}

func (r *Repository) CreateAutomation(ctx context.Context, p domain.Principal, a automation.Automation) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,4))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.automations`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= maxAutomations {
			return domain.ErrConflict
		}
		if e := checkFlowProject(tx, a.ProjectID, a.Definition); e != nil {
			return e
		}
		if e := checkSize(a.Definition); e != nil {
			return e
		}
		if a.Enabled {
			if e := r.checkDefinition(tx, p, a.ProjectID, a.Definition, true); e != nil {
				return e
			}
		}
		if e := tx.Exec(`INSERT INTO core.automations(tenant_id,id,name,description,enabled,project_id,definition) VALUES(?,?,?,?,?,?,?::jsonb)`,
			p.TenantID, a.ID, a.Name, a.Description, a.Enabled, a.ProjectID, string(a.Definition)).Error; e != nil {
			return e
		}
		if a.Enabled {
			if e := armed(tx, p, a.ID, a.Definition); e != nil {
				return e
			}
		}
		if e := reindexTriggers(tx, p.TenantID, a.ID, a.Definition, a.Enabled); e != nil {
			return e
		}
		return audit(tx, p, "automation.created", a.ID)
	}))
}

// SaveAutomation writes a new revision. A stale revision means someone else saved first: 409, never a
// silent overwrite of another operator's blocks.
func (r *Repository) SaveAutomation(ctx context.Context, p domain.Principal, a automation.Automation) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := checkFlowProject(tx, a.ProjectID, a.Definition); e != nil {
			return e
		}
		if e := checkSize(a.Definition); e != nil {
			return e
		}
		if a.Enabled {
			if e := r.checkDefinition(tx, p, a.ProjectID, a.Definition, true); e != nil {
				return e
			}
		}
		if a.ProjectID != nil {
			var n int64
			if e := tx.Raw(`SELECT count(*) FROM core.projects WHERE id=? AND archived_at IS NULL`, *a.ProjectID).Scan(&n).Error; e != nil {
				return e
			}
			if n != 1 {
				return domain.ErrNotFound
			}
		}
		res := tx.Exec(`UPDATE core.automations SET name=?,description=?,enabled=?,project_id=?,definition=?::jsonb,revision=revision+1,updated_at=now() WHERE id=? AND revision=?`,
			a.Name, a.Description, a.Enabled, a.ProjectID, string(a.Definition), a.ID, a.Revision)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			// Either the flow is gone or the revision moved on; both are a conflict for the editor.
			var n int64
			if e := tx.Raw(`SELECT count(*) FROM core.automations WHERE id=?`, a.ID).Scan(&n).Error; e != nil {
				return e
			}
			if n == 0 {
				return domain.ErrNotFound
			}
			return domain.ErrConflict
		}
		// Switching the definition invalidates the rising-edge memory of the blocks it replaced.
		if e := tx.Exec(`DELETE FROM core.automation_state WHERE automation_id=?`, a.ID).Error; e != nil {
			return e
		}
		if e := reindexTriggers(tx, p.TenantID, a.ID, a.Definition, a.Enabled); e != nil {
			return e
		}
		// Saving an enabled flow re-arms it: its commands now carry the member who saved this definition.
		if a.Enabled {
			if e := armed(tx, p, a.ID, a.Definition); e != nil {
				return e
			}
		}
		return audit(tx, p, "automation.saved", a.ID)
	}))
}

func (r *Repository) SetAutomationEnabled(ctx context.Context, p domain.Principal, id string, enabled bool) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		row, e := loadAutomation(tx, id)
		if e != nil {
			return e
		}
		if enabled {
			if e := checkFlowProject(tx, row.ProjectID, row.Definition); e != nil {
				return e
			}
			if e := r.checkDefinition(tx, p, row.ProjectID, row.Definition, true); e != nil {
				return e
			}
		}
		if e := tx.Exec(`UPDATE core.automations SET enabled=?,revision=revision+1,updated_at=now() WHERE id=?`, enabled, id).Error; e != nil {
			return e
		}
		if enabled {
			if e := armed(tx, p, id, row.Definition); e != nil {
				return e
			}
		}
		if e := tx.Exec(`DELETE FROM core.automation_state WHERE automation_id=?`, id).Error; e != nil {
			return e
		}
		if e := reindexTriggers(tx, p.TenantID, id, row.Definition, enabled); e != nil {
			return e
		}
		action := "automation.disabled"
		if enabled {
			action = "automation.enabled"
		}
		return audit(tx, p, action, id)
	})
}

func (r *Repository) DeleteAutomation(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		// Runs, state and index rows cascade on the full composite key; the explicit call keeps
		// reindexTriggers the only place that writes core.automation_triggers.
		if e := reindexTriggers(tx, p.TenantID, id, nil, false); e != nil {
			return e
		}
		res := tx.Exec(`DELETE FROM core.automations WHERE id=?`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "automation.deleted", id)
	})
}

func (r *Repository) ListAutomationRuns(ctx context.Context, p domain.Principal, id string, limit int) ([]automation.Run, error) {
	out := []automation.Run{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if _, e := loadAutomation(tx, id); e != nil {
			return e
		}
		if e := tx.Raw(`SELECT id,automation_id,trigger_node,external_id,gateway_id,status,detail,created_at FROM core.automation_runs
      WHERE automation_id=? ORDER BY created_at DESC,id DESC LIMIT ?`, id, limit).Scan(&out).Error; e != nil {
			return e
		}
		return resolveRequests(tx, out)
	})
	return out, e
}

// TestAutomation is the studio's dry run: it evaluates the stored flow against the tenant's real
// current readings and zones and returns the per-block trace. Nothing is written — no alert, no
// notification, no run row — so an operator can probe a flow without waking anybody up.
func (r *Repository) TestAutomation(ctx context.Context, p domain.Principal, id string, in automation.TestInput) (automation.Result, error) {
	var out automation.Result
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		row, e := loadAutomation(tx, id)
		if e != nil {
			return e
		}
		var def automation.Definition
		if e := json.Unmarshal(row.Definition, &def); e != nil {
			return domain.ErrInvalid
		}
		if e := checkFlowProject(tx, row.ProjectID, row.Definition); e != nil {
			return e
		}
		world := newEvalWorld(tx, time.Now().UTC())
		world.projectID = row.ProjectID
		out = automation.Evaluate(def, automation.MatchTriggers(def, in), world)
		return world.err
	})
	return out, e
}

// evalWorld answers the evaluator's questions from the database, lazily and once per identity, so a
// flow that asks about one sensor does not load the whole workspace.
type evalWorld struct {
	projectID   *string
	tx          *gorm.DB
	at          time.Time
	readings    map[string]worldSample
	zones       map[string]string
	zonesLoaded bool
	// err keeps the first database failure: EvalContext cannot return one, so the caller checks here.
	err error
}

type worldSample struct {
	reading automation.Reading
	age     time.Duration
	ok      bool
}

func newEvalWorld(tx *gorm.DB, at time.Time) *evalWorld {
	return &evalWorld{tx: tx, at: at, readings: map[string]worldSample{}, zones: map[string]string{}}
}

func (w *evalWorld) Now() time.Time { return w.at }

func (w *evalWorld) Reading(external string) (automation.Reading, time.Duration, bool) {
	if c, done := w.readings[external]; done {
		return c.reading, c.age, c.ok
	}
	c := worldSample{}
	defer func() { w.readings[external] = c }()
	if w.err != nil {
		return c.reading, 0, false
	}
	var rows []struct {
		Reading    json.RawMessage
		ReceivedAt time.Time
	}
	// Newest sample of this identity across every gateway that heard it.
	query := `SELECT reading,received_at FROM core.sensor_samples WHERE external_id=?`
	args := []any{external}
	if w.projectID != nil {
		query += ` AND gateway_id IN (SELECT id FROM core.gateways WHERE project_id=?)`
		args = append(args, *w.projectID)
	}
	if e := w.tx.Raw(query+` ORDER BY received_at DESC LIMIT 1`, args...).Scan(&rows).Error; e != nil {
		w.err = e
		return c.reading, 0, false
	}
	if len(rows) != 1 || json.Unmarshal(rows[0].Reading, &c.reading) != nil {
		return c.reading, 0, false
	}
	if c.age = w.at.Sub(rows[0].ReceivedAt); c.age < 0 {
		c.age = 0
	}
	c.ok = true
	return c.reading, c.age, true
}

func (w *evalWorld) Zone(external string) (string, bool) {
	if !w.zonesLoaded {
		w.zonesLoaded = true
		if w.err == nil {
			var rows []struct {
				ExternalID string
				GatewayID  *string
			}
			query := `SELECT external_id,gateway_id FROM core.presence_state`
			args := []any{}
			if w.projectID != nil {
				query += ` WHERE gateway_id IN (SELECT id FROM core.gateways WHERE project_id=?)`
				args = append(args, *w.projectID)
			}
			if e := w.tx.Raw(query+` LIMIT 1000`, args...).Scan(&rows).Error; e != nil {
				w.err = e
			}
			for _, row := range rows {
				if row.GatewayID != nil {
					w.zones[row.ExternalID] = *row.GatewayID
				}
			}
		}
	}
	gateway, ok := w.zones[external]
	return gateway, ok && gateway != ""
}

// firing collects everything one identity contributed to one flow during a single uplink.
type firing struct {
	nodes      map[string]bool
	trigger    string
	gateway    string
	eventID    string
	eventType  string
	deviceName string
	value      string
	// commandIDs are the commands behind the change that woke the flow: every command_id named by the matched events,
	// and every command the identity's report in this packet confirmed. The loop guard reads their source.
	commandIDs []string
}

type runDetail struct {
	Nodes   map[string]string   `json:"nodes"`
	Actions []automation.Action `json:"actions,omitempty"`
	Blocked []string            `json:"blocked,omitempty"`
	Alerts  []string            `json:"alerts,omitempty"`
	Notices int                 `json:"notifications,omitempty"`
	// Commands says, per action.command block, whether its command was queued or why it was refused.
	Commands []commandOutcome  `json:"commands,omitempty"`
	Error    string            `json:"error,omitempty"`
	Trigger  map[string]string `json:"trigger,omitempty"`
}

// flowRow is one candidate flow of this uplink.
type flowRow struct {
	ProjectID  *string
	ID, Name   string
	Definition json.RawMessage
	// EnabledBy is the member who armed the flow; the actor of the commands it queues.
	EnabledBy *string
	// def and hits are filled in after loading, not scanned: the parsed definition and, per
	// trigger.metric block, the identity of this packet it watches.
	def  automation.Definition
	hits map[string]metricHit
}

// runAutomations evaluates the flows of this tenant that this uplink can possibly wake.
//
// Cost before the trigger index: every enabled flow was loaded and unmarshalled, and every
// trigger.metric block was asked about every sensor in the packet with three or four statements —
// O(flows × blocks × sensors) round trips inside the ingest transaction. Cost now: one index lookup,
// one load of just the candidate flows, at most four statements for all their rising edges together,
// and then work only for the flows that actually have a firing.
func runAutomations(tx *gorm.DB, tenant, gateway string, view minew.View, events []domain.DeviceEvent, at time.Time, opts Options) error {
	up := uplinkKeys(gateway, view, events)
	if len(up.EventTypes) == 0 && len(up.SensorIDs) == 0 {
		return nil
	}
	// One cheap query decides everything: a tenant whose packet raised no event and which has no
	// metric trigger row matching any identity here is finished after it, however many flows it keeps
	// enabled. Over-matching is safe — the block's own MatchesEvent / MetricTest still decides.
	ids, e := candidateAutomations(tx, up)
	if e != nil || len(ids) == 0 {
		return e
	}
	var rows []flowRow
	if e := tx.Raw(`SELECT id,name,definition,project_id,enabled_by::text AS enabled_by FROM core.automations WHERE enabled AND (project_id IS NULL OR project_id=(SELECT project_id FROM core.gateways WHERE id=?)) AND id IN ? ORDER BY updated_at DESC,id LIMIT `+
		strconv.Itoa(maxAutomations), gateway, ids).Scan(&rows).Error; e != nil {
		return e
	}
	flows := make([]flowRow, 0, len(rows))
	for _, row := range rows {
		if row.ProjectID != nil {
			if e := checkFlowProject(tx, row.ProjectID, row.Definition); e != nil {
				if errors.Is(e, domain.ErrInvalid) || errors.Is(e, domain.ErrNotFound) {
					continue
				}
				return e
			}
		}
		if e := json.Unmarshal(row.Definition, &row.def); e != nil {
			// Unchanged: an unreadable definition records an error run and costs the other flows nothing.
			if e := insertRun(tx, tenant, row.ID, firing{}, "", automation.StatusError, runDetail{Error: "definition unreadable"}); e != nil {
				return e
			}
			continue
		}
		flows = append(flows, row)
	}
	if len(flows) == 0 {
		return nil
	}
	loaded := make([]string, 0, len(flows))
	for _, flow := range flows {
		loaded = append(loaded, flow.ID)
	}
	fired, e := resolveEdges(tx, tenant, flows, view, loaded, up.SensorIDs, at)
	if e != nil {
		return e
	}
	// Commands each identity's report confirmed in this packet (Zigbee2MQTT), for the loop guard.
	confirmed := map[string][]string{}
	for _, sensor := range view.Sensors {
		confirmed[sensor.ID] = append(confirmed[sensor.ID], sensor.Latest.Confirmed...)
	}

	worlds := map[string]*evalWorld{}
	for _, flow := range flows {
		scope := ""
		if flow.ProjectID != nil {
			scope = *flow.ProjectID
		}
		world := worlds[scope]
		if world == nil {
			world = newEvalWorld(tx, at)
			world.projectID = flow.ProjectID
			worlds[scope] = world
		}
		fires := flowFirings(flow, events, fired, gateway, confirmed)
		// A flow the index handed back but that nothing in this packet actually matched costs no SQL
		// at all: it produced nothing before either, it just paid for the discovery.
		if len(fires) == 0 {
			continue
		}
		// Each flow still runs behind its own savepoint: a broken flow records an error run and the
		// others still run. The rising-edge memory is now written before these savepoints, so a flow
		// that fails afterwards keeps the edge it consumed instead of re-firing on every uplink until
		// someone fixes it; its error run is what says the flow is broken.
		if e := tx.SavePoint("automation_flow").Error; e != nil {
			return e
		}
		runErr := func() error {
			for _, external := range sortedFirings(fires) {
				if e := runFlow(tx, tenant, flow, external, fires[external], world, at, opts); e != nil {
					return e
				}
			}
			return world.err
		}()
		if runErr != nil {
			if e := tx.RollbackTo("automation_flow").Error; e != nil {
				return e
			}
			world.err = nil
			slog.Warn("automation flow failed; other flows and alerts are unaffected", "automation", flow.ID, "error", runErr.Error())
			if e := insertRun(tx, tenant, flow.ID, firing{}, "", automation.StatusError, runDetail{Error: "execution failed"}); e != nil {
				return e
			}
		}
	}
	return nil
}

// uplinkKeys reduces one packet to the handful of values the trigger index is asked about.
func uplinkKeys(gateway string, view minew.View, events []domain.DeviceEvent) automation.Uplink {
	up := automation.Uplink{GatewayID: gateway}
	types, externals, sensors := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, ev := range events {
		if !types[ev.EventType] {
			types[ev.EventType] = true
			up.EventTypes = append(up.EventTypes, ev.EventType)
		}
		if !externals[ev.ExternalID] {
			externals[ev.ExternalID] = true
			up.EventExternalIDs = append(up.EventExternalIDs, ev.ExternalID)
		}
	}
	for _, sensor := range view.Sensors {
		if !sensors[sensor.ID] {
			sensors[sensor.ID] = true
			up.SensorIDs = append(up.SensorIDs, sensor.ID)
		}
	}
	sort.Strings(up.EventTypes)
	sort.Strings(up.EventExternalIDs)
	sort.Strings(up.SensorIDs)
	return up
}

// candidateFilter is the SQL twin of automation.TriggerRow.Matches; the two are kept identical and
// the unit tests pin the Go side down. NULL means "any" in every selector column.
func candidateFilter(up automation.Uplink) (string, []any) {
	clauses, args := []string{}, []any{}
	if len(up.EventTypes) > 0 && len(up.EventExternalIDs) > 0 {
		clauses = append(clauses, `(kind='event' AND (event_type IS NULL OR event_type IN ?) AND (external_id IS NULL OR external_id IN ?) AND (gateway_id IS NULL OR gateway_id=?))`)
		args = append(args, up.EventTypes, up.EventExternalIDs, up.GatewayID)
	}
	if len(up.SensorIDs) > 0 {
		clauses = append(clauses, `(kind='metric' AND (external_id IS NULL OR external_id IN ?))`)
		args = append(args, up.SensorIDs)
	}
	return strings.Join(clauses, " OR "), args
}

func candidateAutomations(tx *gorm.DB, up automation.Uplink) ([]string, error) {
	where, args := candidateFilter(up)
	if where == "" {
		return nil, nil
	}
	var ids []string
	e := tx.Raw(`SELECT DISTINCT automation_id FROM core.automation_triggers WHERE `+where+` LIMIT `+
		strconv.Itoa(maxAutomations), args...).Scan(&ids).Error
	return ids, e
}

// metricHit is one trigger.metric block applied to the one identity it watches, plus what the run
// log needs if the block turns out to be on a rising edge.
type metricHit struct {
	key        automation.EdgeKey
	deviceName string
	value      float64
}

// edgeStateRow is one core.automation_state row as loaded by the batched read.
type edgeStateRow struct {
	AutomationID, NodeID, ExternalID string
	Active                           bool
	Since                            time.Time
}

// resolveEdges applies every trigger.metric block of every candidate flow to this packet in at most
// four statements, whatever the number of blocks and sensors, and returns the blocks on a rising edge.
//
// The rule is metricEdge's, unchanged, only batched: insert the memory rows whose comparison holds,
// read them back under lock, decide in Go, write back only what changed.
//
// Concurrency. Two gateways can ingest the same identity at the same moment, and the memory row is
// what stops both of them firing the same episode. The leading INSERT ... ON CONFLICT DO NOTHING
// keeps that: the speculative-insert conflict makes the second transaction wait for the first, and
// it then keeps the first one's earlier `since`, so for_sec measures from when the rise was first
// seen. The read that follows takes the row locks in key order, so the two transactions serialise
// instead of deadlocking and the loser re-reads the winner's `active` — and does not fire. The
// write-back is an upsert whose DO UPDATE only ever sets active=true, so replaying it is harmless
// and it cannot undo a concurrent firing; active goes back to false only by the DELETE below, which
// is the comparison genuinely ending.
func resolveEdges(tx *gorm.DB, tenant string, flows []flowRow, view minew.View, flowIDs, sensorIDs []string, at time.Time) (map[automation.EdgeKey]bool, error) {
	sensors := make(map[string]minew.Sensor, len(view.Sensors))
	for _, sensor := range view.Sensors {
		if _, seen := sensors[sensor.ID]; !seen {
			sensors[sensor.ID] = sensor
		}
	}
	probes := []automation.EdgeProbe{}
	for i := range flows {
		flows[i].hits = map[string]metricHit{}
		for _, n := range flows[i].def.Nodes {
			if n.Type != automation.TriggerMetric {
				continue
			}
			// A metric block watches exactly one identity, so this is a lookup, not a scan over the packet.
			sensor, present := sensors[n.Data.ExternalID]
			if !present {
				continue
			}
			value, hit, watched := automation.MetricTest(n, sensor.ID, latestReading(sensor.Latest))
			if !watched {
				continue
			}
			key := automation.EdgeKey{AutomationID: flows[i].ID, NodeID: n.ID, ExternalID: sensor.ID}
			flows[i].hits[n.ID] = metricHit{key: key, deviceName: sensor.Name, value: value}
			probes = append(probes, automation.EdgeProbe{Key: key, Hit: hit, ForSec: n.Data.ForSec})
		}
	}
	if len(probes) == 0 {
		return nil, nil
	}
	if e := openEdges(tx, tenant, automation.EdgeOpens(probes), at); e != nil {
		return nil, e
	}
	var rows []edgeStateRow
	if e := tx.Raw(`SELECT automation_id,node_id,external_id,active,since FROM core.automation_state
      WHERE automation_id IN ? AND external_id IN ? ORDER BY automation_id,node_id,external_id FOR UPDATE`,
		flowIDs, sensorIDs).Scan(&rows).Error; e != nil {
		return nil, e
	}
	state := make(map[automation.EdgeKey]automation.EdgeState, len(rows))
	for _, row := range rows {
		state[automation.EdgeKey{AutomationID: row.AutomationID, NodeID: row.NodeID, ExternalID: row.ExternalID}] =
			automation.EdgeState{Active: row.Active, Since: row.Since}
	}
	decided := automation.DecideEdges(probes, state, at)
	if e := markEdgesFired(tx, tenant, decided.Fired, at); e != nil {
		return nil, e
	}
	if e := clearEdges(tx, decided.Clear); e != nil {
		return nil, e
	}
	out := make(map[automation.EdgeKey]bool, len(decided.Fired))
	for _, key := range decided.Fired {
		out[key] = true
	}
	return out, nil
}

// openEdges creates the memory row of every comparison that holds, keeping the one another gateway
// already opened (and its earlier `since`).
func openEdges(tx *gorm.DB, tenant string, keys []automation.EdgeKey, at time.Time) error {
	return edgeUpsert(tx, tenant, keys, at, false, `ON CONFLICT (tenant_id,automation_id,node_id,external_id) DO NOTHING`)
}

// markEdgesFired closes the rising edge of the blocks that fired. The upsert form also re-creates a
// row a concurrent uplink deleted, so the flow cannot fire twice for one episode.
func markEdgesFired(tx *gorm.DB, tenant string, keys []automation.EdgeKey, at time.Time) error {
	return edgeUpsert(tx, tenant, keys, at, true, `ON CONFLICT (tenant_id,automation_id,node_id,external_id) DO UPDATE SET active=true`)
}

func edgeUpsert(tx *gorm.DB, tenant string, keys []automation.EdgeKey, at time.Time, active bool, conflict string) error {
	for start := 0; start < len(keys); start += automationBatch {
		end := min(start+automationBatch, len(keys))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*6)
		for _, key := range keys[start:end] {
			values = append(values, `(?::uuid,?::uuid,?::text,?::text,?::boolean,?::timestamptz)`)
			args = append(args, tenant, key.AutomationID, key.NodeID, key.ExternalID, active, at)
		}
		if e := tx.Exec(`INSERT INTO core.automation_state(tenant_id,automation_id,node_id,external_id,active,since) VALUES `+
			strings.Join(values, ",")+" "+conflict, args...).Error; e != nil {
			return e
		}
	}
	return nil
}

// clearEdges drops the memory of every comparison that stopped holding, so the next rise fires again.
func clearEdges(tx *gorm.DB, keys []automation.EdgeKey) error {
	for start := 0; start < len(keys); start += automationBatch {
		end := min(start+automationBatch, len(keys))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*3)
		for _, key := range keys[start:end] {
			values = append(values, `(?::uuid,?::text,?::text)`)
			args = append(args, key.AutomationID, key.NodeID, key.ExternalID)
		}
		if e := tx.Exec(`DELETE FROM core.automation_state WHERE (automation_id,node_id,external_id) IN (`+
			strings.Join(values, ",")+`)`, args...).Error; e != nil {
			return e
		}
	}
	return nil
}

// flowFirings collects what each identity contributed to one flow during this uplink. Blocks are
// walked in definition order, as before, so the trigger a run names is still the first block that
// matched and the value is still the last metric block that fired.
func flowFirings(flow flowRow, events []domain.DeviceEvent, fired map[automation.EdgeKey]bool, gateway string, confirmed map[string][]string) map[string]*firing {
	fires := map[string]*firing{}
	ensure := func(external string) *firing {
		f, ok := fires[external]
		if !ok {
			f = &firing{nodes: map[string]bool{}, commandIDs: append([]string{}, confirmed[external]...)}
			fires[external] = f
		}
		return f
	}
	for _, n := range flow.def.Nodes {
		switch n.Type {
		case automation.TriggerEvent:
			for _, ev := range events {
				if !automation.MatchesEventAction(n, ev.EventType, ev.ExternalID, ev.GatewayID, detailString(ev.Detail, "action")) {
					continue
				}
				f := ensure(ev.ExternalID)
				f.nodes[n.ID] = true
				if id := detailString(ev.Detail, "command_id"); id != "" {
					f.commandIDs = append(f.commandIDs, id)
				}
				if f.trigger == "" {
					f.trigger, f.gateway, f.eventID, f.eventType, f.deviceName = n.ID, ev.GatewayID, ev.ID, ev.EventType, ev.DeviceName
				}
			}
		case automation.TriggerMetric:
			hit, watched := flow.hits[n.ID]
			if !watched || !fired[hit.key] {
				continue
			}
			f := ensure(hit.key.ExternalID)
			f.nodes[n.ID] = true
			if f.trigger == "" {
				f.trigger, f.gateway, f.deviceName = n.ID, gateway, hit.deviceName
			}
			f.value = strconv.FormatFloat(math.Round(hit.value*100)/100, 'f', -1, 64)
		}
	}
	return fires
}

func sortedFirings(fires map[string]*firing) []string {
	out := make([]string, 0, len(fires))
	for external := range fires {
		out = append(out, external)
	}
	sort.Strings(out)
	return out
}

func latestReading(r minew.Reading) automation.Reading {
	return automation.Reading{Temperature: r.Temperature, Humidity: r.Humidity, Battery: r.Battery, RSSI: r.RSSI, Metrics: r.Metrics}
}

// detailString reads a string field of an event's detail ("" when absent or not a string).
func detailString(detail map[string]any, key string) string {
	if s, ok := detail[key].(string); ok {
		return s
	}
	return ""
}

// runFlow evaluates one flow for one identity and executes what the evaluation decided.
func runFlow(tx *gorm.DB, tenant string, row flowRow, external string, f *firing, world *evalWorld, at time.Time, opts Options) error {
	flow, flowName, def := row.ID, row.Name, row.def
	var recent int64
	if e := tx.Raw(`SELECT count(*) FROM core.automation_runs WHERE automation_id=? AND external_id=? AND status='fired' AND created_at>?`,
		flow, external, at.Add(-automationDedupe)).Scan(&recent).Error; e != nil {
		return e
	}
	if recent > 0 {
		return nil
	}
	result := automation.Evaluate(def, f.nodes, world)
	if world.err != nil {
		return world.err
	}
	eventLabel := f.eventType
	if eventLabel == "" {
		eventLabel = automationEvent
	}
	detail := runDetail{Nodes: result.Nodes, Actions: result.Actions, Blocked: result.Blocked,
		Trigger: map[string]string{"node": f.trigger, "event": eventLabel, "value": f.value, "device": f.deviceName}}
	if !result.Fired() {
		return insertRun(tx, tenant, flow, *f, external, automation.StatusSkipped, detail)
	}

	vars := map[string]string{"device": f.deviceName, "value": f.value, "event": eventLabel}
	eventID := f.eventID
	// Every alert needs a device event to point at. A metric trigger has none of its own, so the flow
	// records one of type "automation" naming itself.
	openEvent := func() error {
		if eventID != "" {
			return nil
		}
		ev := domain.DeviceEvent{GatewayID: f.gateway, ExternalID: external, DeviceName: f.deviceName, EventType: automationEvent,
			Detail: map[string]any{"automation_id": flow, "name": flowName, "trigger_node": f.trigger, "value": f.value}, OccurredAt: at}
		if e := insertEvent(tx, tenant, &ev); e != nil {
			return e
		}
		eventID = ev.ID
		return nil
	}
	alertID := ""
	openAlert := func(severity, title string) error {
		if e := openEvent(); e != nil {
			return e
		}
		id := uuid.NewString()
		if e := tx.Exec(`INSERT INTO core.alerts(tenant_id,id,rule_id,event_id,gateway_id,external_id,device_name,event_type,severity,title,opened_at) VALUES(?,?,NULL,?,?,?,?,?,?,?,?)`,
			tenant, id, eventID, f.gateway, external, f.deviceName, eventLabel, severity, title, at).Error; e != nil {
			return e
		}
		alertID = id
		detail.Alerts = append(detail.Alerts, id)
		return nil
	}

	for _, act := range result.Actions {
		switch act.Type {
		case automation.ActionAlert:
			title := automation.Render(act.Title, vars)
			if title == "" {
				title = flowName
			}
			if e := openAlert(act.Severity, title); e != nil {
				return e
			}
		case automation.ActionNotify:
			// core.notifications hangs off an alert, and the worker reads the alert to build the
			// message. A flow that only notifies therefore opens one info alert to carry its message;
			// it is a real alert the operator can see and resolve, not a hidden side channel.
			message := automation.Render(act.Message, vars)
			if alertID == "" {
				if e := openAlert("info", message); e != nil {
					return e
				}
			} else if message != "" {
				// The alert already exists (an alert block ran first): carry the message as its note, which the
				// notification text includes.
				if e := tx.Exec(`UPDATE core.alerts SET note=? WHERE id=? AND note IS NULL`, message, alertID).Error; e != nil {
					return e
				}
			}
			for _, channel := range act.ChannelIDs {
				res := tx.Exec(`INSERT INTO core.notifications(tenant_id,id,alert_id,channel_id,next_attempt_at) SELECT ?,?,?,id,now() FROM core.notification_channels WHERE id=? AND enabled`,
					tenant, uuid.NewString(), alertID, channel)
				if res.Error != nil {
					return res.Error
				}
				detail.Notices += int(res.RowsAffected)
			}
		case automation.ActionCommand:
			outcome, e := requestCommand(tx, tenant, row, act, f, opts, at)
			if e != nil {
				return e
			}
			detail.Commands = append(detail.Commands, outcome)
			if outcome.Status != "queued" {
				detail.Nodes[act.NodeID] = automation.OutcomeBlocked
				detail.Blocked = append(detail.Blocked, act.NodeID)
			}
		}
	}
	if alertID != "" {
		if e := signal(tx, tenant, "alert", f.gateway); e != nil {
			return e
		}
	}
	if e := tx.Exec(`UPDATE core.automations SET last_fired_at=?,fire_count=fire_count+1 WHERE id=?`, at, flow).Error; e != nil {
		return e
	}
	return insertRun(tx, tenant, flow, *f, external, automation.StatusFired, detail)
}

func insertRun(tx *gorm.DB, tenant, flow string, f firing, external, status string, detail runDetail) error {
	payload, e := json.Marshal(detail)
	if e != nil {
		return e
	}
	var gateway any
	if f.gateway != "" {
		gateway = f.gateway
	}
	if e := tx.Exec(`INSERT INTO core.automation_runs(tenant_id,id,automation_id,trigger_node,external_id,gateway_id,status,detail) VALUES(?,?,?,?,?,?,?,?::jsonb)`,
		tenant, uuid.NewString(), flow, f.trigger, external, gateway, status, string(payload)).Error; e != nil {
		return e
	}
	return tx.Exec(`DELETE FROM core.automation_runs WHERE automation_id=? AND id IN (SELECT id FROM core.automation_runs WHERE automation_id=? ORDER BY created_at DESC,id DESC OFFSET ?)`,
		flow, flow, automationRunHistory).Error
}
