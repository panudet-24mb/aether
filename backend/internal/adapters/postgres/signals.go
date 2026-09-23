package postgres

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/domain"
	"aether/backend/internal/signals"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Persistence for learned device signals. The analysis itself lives in internal/signals and never
// touches the database; this file only supplies it with the two capture windows out of
// core.ble_history and stores what the operator confirmed.

// testWindow is how far back "ทดสอบกับข้อมูลย้อนหลัง" replays a signature.
const testWindow = 10 * time.Minute

type sessionRow struct {
	ID, GatewayID, ExternalID, EventType, Label, Status string
	StartedAt, BaselineUntil, TriggerUntil              time.Time
	TriggerFrom                                         *time.Time
}

const sessionColumns = `id,gateway_id,external_id,event_type,label,status,started_at,baseline_until,trigger_from,trigger_until`

// CreateSignalSession opens a teaching session. The identity must be one this gateway has actually
// heard: teaching a signal for a tag the gateway has never seen could only ever produce an empty
// baseline and a misleading verdict.
func (r *Repository) CreateSignalSession(ctx context.Context, p domain.Principal, in signals.NewSession) (signals.Session, error) {
	var out signals.Session
	id := uuid.NewString()
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,7))`, p.TenantID).Error; e != nil {
			return e
		}
		var heard int64
		if e := tx.Raw(`SELECT count(*) FROM core.sensor_streams WHERE gateway_id=? AND external_id=?`, in.GatewayID, in.ExternalID).Scan(&heard).Error; e != nil {
			return e
		}
		if heard != 1 {
			return domain.ErrNotFound
		}
		// Stale sessions of this workspace are closed first, so an abandoned wizard tab cannot use up
		// the open-session budget forever.
		if e := tx.Exec(`UPDATE core.signal_sessions SET status='cancelled' WHERE status IN ('baseline','trigger') AND trigger_until<now()-interval '1 hour'`).Error; e != nil {
			return e
		}
		var open int64
		if e := tx.Raw(`SELECT count(*) FROM core.signal_sessions WHERE status IN ('baseline','trigger')`).Scan(&open).Error; e != nil {
			return e
		}
		if open >= signals.MaxOpenSessions {
			return domain.ErrConflict
		}
		now := time.Now().UTC()
		baselineUntil := now.Add(time.Duration(in.BaselineSec) * time.Second)
		triggerUntil := baselineUntil.Add(time.Duration(in.TriggerSec) * time.Second)
		if e := tx.Exec(`INSERT INTO core.signal_sessions(tenant_id,id,gateway_id,external_id,event_type,label,started_at,baseline_until,trigger_until,status,created_by)
      VALUES(?,?,?,?,?,?,?,?,?,'baseline',?)`, p.TenantID, id, in.GatewayID, in.ExternalID, in.EventType, in.Label, now, baselineUntil, triggerUntil, p.UserID).Error; e != nil {
			return e
		}
		if e := audit(tx, p, "signal.session_started", id); e != nil {
			return e
		}
		var err error
		out, err = readSession(tx, id)
		return err
	})
	return out, classify(e)
}

// SignalSession reports the live counters while the session runs and the ranked candidates once both
// phases are over. It advances the stored status as time passes, so a caller that only polls this
// endpoint still sees the session move from baseline to trigger to finished.
func (r *Repository) SignalSession(ctx context.Context, p domain.Principal, id string) (signals.Session, error) {
	var out signals.Session
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var err error
		out, err = readSession(tx, id)
		return err
	})
	return out, e
}

// AdvanceSignalSession ends the baseline early, when the operator is ready to start triggering the
// device rather than waiting out the countdown. The trigger window keeps its original length.
func (r *Repository) AdvanceSignalSession(ctx context.Context, p domain.Principal, id string) (signals.Session, error) {
	var out signals.Session
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []sessionRow
		if e := tx.Raw(`SELECT `+sessionColumns+` FROM core.signal_sessions WHERE id=? FOR UPDATE`, id).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrNotFound
		}
		row := rows[0]
		if row.Status != signals.StatusBaseline {
			return domain.ErrConflict
		}
		now := time.Now().UTC()
		if now.Before(row.StartedAt) {
			now = row.StartedAt
		}
		// The trigger phase keeps the length the operator chose, measured from this moment.
		length := row.TriggerUntil.Sub(row.BaselineUntil)
		if e := tx.Exec(`UPDATE core.signal_sessions SET baseline_until=?,trigger_from=?,trigger_until=?,status='trigger' WHERE id=?`, now, now, now.Add(length), id).Error; e != nil {
			return e
		}
		var err error
		out, err = readSession(tx, id)
		return err
	})
	return out, e
}

func (r *Repository) CancelSignalSession(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE core.signal_sessions SET status='cancelled' WHERE id=? AND status IN ('baseline','trigger','finished')`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "signal.session_cancelled", id)
	})
}

// ConfirmSignalSession stores the candidate the operator picked. The candidates are recomputed from
// the same two windows rather than cached, so the index always refers to what the operator was shown
// (the analysis is a pure function of history that no longer changes once the session is finished).
func (r *Repository) ConfirmSignalSession(ctx context.Context, p domain.Principal, id string, index int, scope string) (signals.Signal, error) {
	var out signals.Signal
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,7))`, p.TenantID).Error; e != nil {
			return e
		}
		var rows []sessionRow
		if e := tx.Raw(`SELECT `+sessionColumns+` FROM core.signal_sessions WHERE id=? FOR UPDATE`, id).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrNotFound
		}
		row := rows[0]
		if row.Status == signals.StatusConfirmed || row.Status == signals.StatusCancelled {
			return domain.ErrConflict
		}
		row, e := advanceClock(tx, row)
		if e != nil {
			return e
		}
		view, e := buildSession(tx, row)
		if e != nil {
			return e
		}
		if view.Status != signals.StatusFinished || index < 0 || index >= len(view.Candidates) {
			return domain.ErrInvalid
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.device_signals`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= signals.MaxSignals {
			return domain.ErrConflict
		}
		candidate := view.Candidates[index]
		if e := signals.Validate(candidate.Matcher); e != nil {
			return domain.ErrInvalid
		}
		out = signals.Signal{ID: uuid.NewString(), Scope: scope, EventType: row.EventType, Matcher: candidate.Matcher, Description: candidate.Description}
		var external, profile any
		switch scope {
		case signals.ScopeDevice:
			out.ExternalID = row.ExternalID
			external = row.ExternalID
		case signals.ScopeProfile:
			// "ใช้กับทุกตัวที่เป็นรุ่นนี้" only means something once the tag is registered: the profile
			// comes from the registration, never from a guess about the model.
			var ids []string
			if e := tx.Raw(`SELECT profile_id FROM core.devices WHERE external_id=? AND removed_at IS NULL LIMIT 1`, row.ExternalID).Scan(&ids).Error; e != nil {
				return e
			}
			if len(ids) != 1 {
				return domain.ErrInvalid
			}
			out.ProfileID = ids[0]
			profile = ids[0]
		default:
			return domain.ErrInvalid
		}
		matcher, e2 := json.Marshal(candidate.Matcher)
		if e2 != nil {
			return domain.ErrInvalid
		}
		if e := tx.Exec(`INSERT INTO core.device_signals(tenant_id,id,scope,external_id,profile_id,event_type,matcher,description,created_by)
      VALUES(?,?,?,?,?,?,?::jsonb,?,?)`, p.TenantID, out.ID, scope, external, profile, row.EventType, string(matcher), candidate.Description, p.UserID).Error; e != nil {
			return e
		}
		if e := tx.Exec(`UPDATE core.signal_sessions SET status='confirmed' WHERE id=?`, id).Error; e != nil {
			return e
		}
		// The inventory signal makes every open topology tab refetch and show the new badge.
		if e := signal(tx, p.TenantID, "inventory", row.GatewayID); e != nil {
			return e
		}
		return audit(tx, p, "signal.confirmed", out.ID)
	})
	return out, classify(e)
}

func (r *Repository) ListSignals(ctx context.Context, p domain.Principal) ([]signals.Signal, error) {
	out := []signals.Signal{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var err error
		out, err = scanSignals(tx, ``)
		return err
	})
	return out, e
}

func (r *Repository) DeleteSignal(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`DELETE FROM core.device_signals WHERE id=?`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "signal.deleted", id)
	})
}

// TestSignal replays the recent raw history of the identities a signature covers and reports how many
// uplinks would have raised the event. This is how the owner checks for false positives before
// trusting a learned signal: a signature that fires on most uplinks is measuring something that is
// always true, not a press.
func (r *Repository) TestSignal(ctx context.Context, p domain.Principal, id string) (signals.TestResult, error) {
	out := signals.TestResult{Since: time.Now().UTC().Add(-testWindow)}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		list, e := scanSignals(tx, `WHERE id=?`, id)
		if e != nil {
			return e
		}
		if len(list) != 1 {
			return domain.ErrNotFound
		}
		s := list[0]
		// A profile-scoped signature covers every registered tag of that model; a device-scoped one
		// covers exactly its own identity.
		var identities []string
		if s.Scope == signals.ScopeProfile {
			if e := tx.Raw(`SELECT DISTINCT external_id FROM core.devices WHERE profile_id=? AND removed_at IS NULL LIMIT 200`, s.ProfileID).Scan(&identities).Error; e != nil {
				return e
			}
		} else {
			identities = []string{s.ExternalID}
		}
		if len(identities) == 0 {
			out.Verdict = "ยังไม่มีอุปกรณ์ที่ลงทะเบียนด้วย profile นี้"
			return nil
		}
		var rows []struct {
			Packet, Raw string
			ReceivedAt  time.Time
		}
		if e := tx.Raw(`SELECT split_part(event_key,':',1) AS packet,raw,received_at FROM core.ble_history
      WHERE external_id IN ? AND received_at>=? ORDER BY received_at LIMIT 20000`, identities, out.Since).Scan(&rows).Error; e != nil {
			return e
		}
		// core.ble_history stores one row per distinct advertisement per packet, so grouping by the
		// packet digest reconstructs exactly the advertisement list one uplink carried.
		type uplink struct {
			raws []string
			at   time.Time
		}
		order := []string{}
		byPacket := map[string]*uplink{}
		for _, row := range rows {
			u, ok := byPacket[row.Packet]
			if !ok {
				u = &uplink{at: row.ReceivedAt}
				byPacket[row.Packet] = u
				order = append(order, row.Packet)
			}
			u.raws = append(u.raws, row.Raw)
		}
		out.Uplinks = len(order)
		for _, key := range order {
			u := byPacket[key]
			if signals.Match(s.Matcher, u.raws) {
				out.Matches++
				at := u.at
				out.LastMatch = &at
			}
		}
		out.Verdict = testVerdict(out)
		return nil
	})
	return out, e
}

// testVerdict turns the replay counts into the sentence an operator can act on.
func testVerdict(t signals.TestResult) string {
	switch {
	case t.Uplinks == 0:
		return "ไม่มีข้อมูลย้อนหลัง 10 นาทีของอุปกรณ์นี้ให้ทดสอบ · ปล่อยให้ gateway รับสัญญาณสักครู่แล้วลองใหม่"
	case t.Matches == 0:
		return "ไม่พบสัญญาณนี้ในข้อมูลย้อนหลัง 10 นาที · แปลว่าจะไม่แจ้งเตือนพร่ำเพรื่อ (ถ้าเพิ่งกดไปควรเห็นอย่างน้อย 1 ครั้ง)"
	case t.Matches*2 > t.Uplinks:
		return "ตรงเกินครึ่งของ uplink ที่ผ่านมา · น่าจะจับสิ่งที่เป็นจริงตลอดเวลา ไม่ใช่การกด แนะนำให้สอนใหม่หรือเลือก candidate อื่น"
	}
	return "อยู่ในเกณฑ์ที่ใช้งานได้ · ตรงเฉพาะบาง uplink ตามที่ควรเป็น"
}

// readSession loads a session and lazily moves it into the phase the clock says it is in.
func readSession(tx *gorm.DB, id string) (signals.Session, error) {
	var rows []sessionRow
	if e := tx.Raw(`SELECT `+sessionColumns+` FROM core.signal_sessions WHERE id=?`, id).Scan(&rows).Error; e != nil {
		return signals.Session{}, e
	}
	if len(rows) != 1 {
		return signals.Session{}, domain.ErrNotFound
	}
	row, e := advanceClock(tx, rows[0])
	if e != nil {
		return signals.Session{}, e
	}
	return buildSession(tx, row)
}

// advanceClock moves a running session into the phase the clock says it is in and persists that, so
// the status is right for a caller that never polled the session while it ran (the wizard polls, a
// script driving the API directly need not).
func advanceClock(tx *gorm.DB, row sessionRow) (sessionRow, error) {
	id := row.ID
	now := time.Now().UTC()
	// Persist the phase the clock implies, so the status is right even for a caller that never polls.
	switch {
	case row.Status == signals.StatusBaseline && !now.Before(row.TriggerUntil):
		row.Status = signals.StatusFinished
		if row.TriggerFrom == nil {
			from := row.BaselineUntil
			row.TriggerFrom = &from
		}
		if e := tx.Exec(`UPDATE core.signal_sessions SET status='finished',trigger_from=coalesce(trigger_from,baseline_until) WHERE id=?`, id).Error; e != nil {
			return row, e
		}
	case row.Status == signals.StatusBaseline && !now.Before(row.BaselineUntil):
		row.Status = signals.StatusTrigger
		from := row.BaselineUntil
		row.TriggerFrom = &from
		if e := tx.Exec(`UPDATE core.signal_sessions SET status='trigger',trigger_from=coalesce(trigger_from,baseline_until) WHERE id=?`, id).Error; e != nil {
			return row, e
		}
	case row.Status == signals.StatusTrigger && !now.Before(row.TriggerUntil):
		row.Status = signals.StatusFinished
		if e := tx.Exec(`UPDATE core.signal_sessions SET status='finished' WHERE id=?`, id).Error; e != nil {
			return row, e
		}
	}
	return row, nil
}

// buildSession fills in the per-phase counters and, once both windows are closed, the analysis.
func buildSession(tx *gorm.DB, row sessionRow) (signals.Session, error) {
	out := signals.Session{ID: row.ID, GatewayID: row.GatewayID, ExternalID: row.ExternalID, EventType: row.EventType, Label: row.Label, Status: row.Status, StartedAt: row.StartedAt, Candidates: []signals.Candidate{}}
	triggerFrom := row.BaselineUntil
	if row.TriggerFrom != nil {
		triggerFrom = *row.TriggerFrom
	}
	out.Baseline = signals.Phase{From: row.StartedAt, Until: row.BaselineUntil, Active: row.Status == signals.StatusBaseline}
	out.Trigger = signals.Phase{From: triggerFrom, Until: row.TriggerUntil, Active: row.Status == signals.StatusTrigger}

	baselineRaw, e := phaseRaw(tx, row.GatewayID, row.ExternalID, row.StartedAt, row.BaselineUntil, &out.Baseline)
	if e != nil {
		return out, e
	}
	triggerRaw, e := phaseRaw(tx, row.GatewayID, row.ExternalID, triggerFrom, row.TriggerUntil, &out.Trigger)
	if e != nil {
		return out, e
	}
	if row.Status != signals.StatusFinished {
		return out, nil
	}
	out.Candidates = signals.Analyse(baselineRaw, triggerRaw, signals.TriggerVerb(row.EventType))
	if len(out.Candidates) == 0 {
		out.Verdict = signals.NoDifferenceVerdict(out.Baseline, out.Trigger)
	}
	return out, nil
}

// phaseRaw counts one capture window and returns its distinct raw advertisements. The counters are
// what lets the operator see, live, whether anything is reaching the gateway at all.
func phaseRaw(tx *gorm.DB, gateway, external string, from, until time.Time, phase *signals.Phase) ([]string, error) {
	var stats []struct {
		Observations int
		DistinctRaw  int
	}
	if e := tx.Raw(`SELECT count(*) AS observations,count(DISTINCT raw) AS distinct_raw FROM core.ble_history
    WHERE gateway_id=? AND external_id=? AND received_at>=? AND received_at<?`, gateway, external, from, until).Scan(&stats).Error; e != nil {
		return nil, e
	}
	if len(stats) == 1 {
		phase.Observations, phase.Distinct = stats[0].Observations, stats[0].DistinctRaw
	}
	var raw []string
	if e := tx.Raw(`SELECT DISTINCT raw FROM core.ble_history WHERE gateway_id=? AND external_id=? AND received_at>=? AND received_at<? LIMIT 2000`, gateway, external, from, until).Scan(&raw).Error; e != nil {
		return nil, e
	}
	return raw, nil
}

type signalRow struct {
	ID, Scope, EventType, Description string
	ExternalID, ProfileID             *string
	Matcher                           json.RawMessage
	Verified                          bool
	CreatedAt                         time.Time
}

func scanSignals(tx *gorm.DB, where string, args ...any) ([]signals.Signal, error) {
	var rows []signalRow
	q := `SELECT id,scope,external_id,profile_id,event_type,matcher,description,verified,created_at FROM core.device_signals `
	if e := tx.Raw(q+where+` ORDER BY created_at DESC,id LIMIT ?`, append(args, signals.MaxSignals)...).Scan(&rows).Error; e != nil {
		return nil, e
	}
	out := make([]signals.Signal, 0, len(rows))
	for _, row := range rows {
		s := signals.Signal{ID: row.ID, Scope: row.Scope, EventType: row.EventType, Description: row.Description, Verified: row.Verified, CreatedAt: row.CreatedAt}
		if row.ExternalID != nil {
			s.ExternalID = *row.ExternalID
		}
		if row.ProfileID != nil {
			s.ProfileID = *row.ProfileID
		}
		if json.Unmarshal(row.Matcher, &s.Matcher) != nil || signals.Validate(s.Matcher) != nil {
			continue // a matcher that no longer validates is ignored rather than trusted
		}
		out = append(out, s)
	}
	return out, nil
}

// learnedSignals is the ingest-path load: every signature of the workspace, once per packet, indexed
// by the identity or profile it applies to. At most MaxSignals rows, so this is a small scan and the
// per-sensor work below is a map lookup.
type learnedSignals struct {
	byIdentity map[string][]signals.Signal
	byProfile  map[string][]signals.Signal
	// profileOf maps a heard identity to the profile it is registered with, empty when unregistered.
	profileOf map[string]string
}

func (l learnedSignals) empty() bool { return len(l.byIdentity) == 0 && len(l.byProfile) == 0 }

// forIdentity returns the signatures that apply to one BLE identity on this uplink.
func (l learnedSignals) forIdentity(external string) []signals.Signal {
	out := l.byIdentity[external]
	if profile := l.profileOf[external]; profile != "" {
		out = append(append([]signals.Signal(nil), out...), l.byProfile[profile]...)
	}
	return out
}

func loadLearnedSignals(tx *gorm.DB) (learnedSignals, error) {
	out := learnedSignals{byIdentity: map[string][]signals.Signal{}, byProfile: map[string][]signals.Signal{}, profileOf: map[string]string{}}
	list, e := scanSignals(tx, ``)
	if e != nil {
		return out, e
	}
	if len(list) == 0 {
		return out, nil
	}
	for _, s := range list {
		if s.Scope == signals.ScopeProfile {
			out.byProfile[s.ProfileID] = append(out.byProfile[s.ProfileID], s)
			continue
		}
		out.byIdentity[s.ExternalID] = append(out.byIdentity[s.ExternalID], s)
	}
	if len(out.byProfile) == 0 {
		return out, nil // no profile-scoped signature: the registration lookup would be dead weight
	}
	var regs []struct{ ExternalID, ProfileID string }
	if e := tx.Raw(`SELECT lower(external_id) AS external_id,profile_id FROM core.devices WHERE removed_at IS NULL LIMIT 5000`).Scan(&regs).Error; e != nil {
		return out, e
	}
	for _, reg := range regs {
		out.profileOf[reg.ExternalID] = reg.ProfileID
	}
	return out, nil
}

// signalEvents evaluates the learned signatures for one stream and returns the events to raise plus
// the new per-signal state to persist. It is edge-triggered exactly like the decoded tamper and leak
// flags: the event fires on the transition into the matched state and does not repeat while the
// pattern keeps arriving in consecutive uplinks.
func signalEvents(applicable []signals.Signal, raws []string, prev map[string]int, at time.Time) ([]domain.DeviceEvent, map[string]int) {
	next := map[string]int{}
	var out []domain.DeviceEvent
	for _, s := range applicable {
		hit := 0
		if signals.Match(s.Matcher, raws) {
			hit = 1
		}
		if s.EventType == signals.EventDoor {
			// A door is a state: applyLearnedDoors already turned the match into the `door` metric
			// and alerts.Detect raises door_open / door_closed from it, so nothing is emitted here.
			// An uplink that did not carry the frame keeps the last known state.
			if signals.Observed(s.Matcher, raws) {
				next[s.ID] = hit
			} else if was, known := prev[s.ID]; known {
				next[s.ID] = was
			}
			continue
		}
		next[s.ID] = hit
		was, known := prev[s.ID]
		switch {
		case hit == 1 && (!known || was == 0):
			out = append(out, domain.DeviceEvent{EventType: s.EventType, OccurredAt: at, Detail: map[string]any{
				"signal_id": s.ID, "learned": true, "matcher": s.Matcher.Kind, "note": s.Description,
			}})
		case hit == 0 && known && was == 1:
			if cleared := signals.ClearedEventType(s.EventType); cleared != "" {
				out = append(out, domain.DeviceEvent{EventType: cleared, OccurredAt: at, Detail: map[string]any{"signal_id": s.ID, "learned": true}})
			}
		}
	}
	return out, next
}

// decodeSignalState reads the per-stream learned-signal flags stored in core.stream_state.signals.
func decodeSignalState(raw json.RawMessage) map[string]int {
	out := map[string]int{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// rawByIdentity groups the distinct raw advertisements of one packet by advertiser MAC. It is built
// once per packet by saveBLEHistory, which already walks exactly these rows, so matching costs no
// extra parsing of the payload.
type rawByIdentity map[string][]string

func (m rawByIdentity) add(mac, raw string) {
	mac, raw = strings.ToLower(mac), strings.ToLower(raw)
	m[mac] = append(m[mac], raw)
}

// learnedDoor evaluates the door signatures that apply to one identity on this uplink. observed is false
// when none of them could be judged (the uplink did not carry the frame they read), and then the door
// state must not change. Any matching signature means open.
func learnedDoor(applicable []signals.Signal, raws []string) (open, observed bool, signalID string) {
	for _, s := range applicable {
		if s.EventType != signals.EventDoor || !signals.Observed(s.Matcher, raws) {
			continue
		}
		if !observed {
			observed, signalID = true, s.ID
		}
		if signals.Match(s.Matcher, raws) {
			return true, true, s.ID
		}
	}
	return false, observed, signalID
}

// learnedDoorID names the door signature that decided this uplink's door state, or "" when none did.
func learnedDoorID(applicable []signals.Signal, raws []string) string {
	_, observed, id := learnedDoor(applicable, raws)
	if !observed {
		return ""
	}
	return id
}

// applyLearnedDoors writes the `door` metric (1 = open, 0 = closed) into the readings of identities that
// have a taught door signal, BEFORE the samples are stored, so the stored reading carries the current
// state for the UI and alerts.Detect turns its edges into door_open / door_closed exactly as it would a
// decoded door flag. There is no S4 frame decoder: without a taught signal nothing sets this metric.
// It only reads (device_signals, devices) and takes no lock, so the ingest lock order is unchanged.
func applyLearnedDoors(tx *gorm.DB, view *minew.View, raws rawByIdentity) error {
	if len(view.Sensors) == 0 {
		return nil
	}
	learned, e := loadLearnedSignals(tx)
	if e != nil || learned.empty() {
		return e
	}
	for i := range view.Sensors {
		sensor := &view.Sensors[i]
		open, observed, _ := learnedDoor(learned.forIdentity(sensor.ID), raws[sensor.ID])
		if !observed {
			continue
		}
		value := 0.0
		if open {
			value = 1
		}
		if sensor.Latest.Metrics == nil {
			sensor.Latest.Metrics = map[string]float64{}
		}
		sensor.Latest.Metrics["door"] = value
		sensor.Latest.Kind = minew.KindDoor
		sensor.Kind = minew.KindDoor
		if n := len(sensor.History); n > 0 {
			sensor.History[n-1] = sensor.Latest
		}
	}
	return nil
}
