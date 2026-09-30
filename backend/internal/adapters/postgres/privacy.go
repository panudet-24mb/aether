package postgres

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"aether/backend/internal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Personal-data access log, exports and erasure (migrations 00038 and 00039, docs/platform/privacy.md).

// LogAccess records one read of personal data by the caller. The row's tenant and actor are the transaction's
// own (the insert policy pins both), so a caller can only ever log about itself.
func (r *Repository) LogAccess(ctx context.Context, p domain.Principal, in domain.AccessRead) error {
	ip := ""
	if parsed := net.ParseIP(in.ClientIP); parsed != nil {
		ip = parsed.String()
	}
	// A display logs its own reads with itself as the actor (migration 00044); the policy pins that too.
	if _, ok := domain.DisplayFrom(ctx); ok {
		return r.tx(domain.WithDisplayWrite(ctx), "", p.TenantID, func(tx *gorm.DB) error {
			return tx.Exec(`INSERT INTO core.access_log(tenant_id,id,actor_id,actor_kind,resource,subject_kind,subject_id,request_id,client_ip)
      VALUES(core.tenant_id(),?,core.display_id(),'display',?,?,?,NULLIF(?,''),NULLIF(?,'')::inet)`,
				uuid.NewString(), clip(in.Resource, 64), clip(in.SubjectKind, 32), clip(in.SubjectID, 128), clip(in.RequestID, 128), ip).Error
		})
	}
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Exec(`INSERT INTO core.access_log(tenant_id,id,actor_id,resource,subject_kind,subject_id,request_id,client_ip)
      VALUES(core.tenant_id(),?,identity.user_id(),?,?,?,NULLIF(?,''),NULLIF(?,'')::inet)`,
			uuid.NewString(), clip(in.Resource, 64), clip(in.SubjectKind, 32), clip(in.SubjectID, 128), clip(in.RequestID, 128), ip).Error
	})
}

// clip shortens s to at most n bytes without splitting a UTF-8 sequence (the columns check length in characters,
// and a cut rune would be invalid text).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// requireOwner refuses anyone but an owner of the current workspace (re-read inside the transaction).
func requireOwner(tx *gorm.DB, p domain.Principal) error {
	role, e := roleOf(tx, p.TenantID, p.UserID)
	if e != nil || role != "owner" {
		return domain.ErrForbidden
	}
	return nil
}

func pageLimit(n int) int {
	if n <= 0 {
		return 100
	}
	return min(n, 500)
}

// ListAccessLog pages the read-access log of the workspace, newest first. Owner only (Go check and policy).
func (r *Repository) ListAccessLog(ctx context.Context, p domain.Principal, f domain.LogFilter) ([]domain.AccessEntry, error) {
	out := []domain.AccessEntry{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := requireOwner(tx, p); e != nil {
			return e
		}
		where, args := []string{"a.tenant_id=core.tenant_id()"}, []any{}
		if f.ActorID != "" {
			where, args = append(where, "a.actor_id=?"), append(args, f.ActorID)
		}
		if f.Resource != "" {
			where, args = append(where, "a.resource=?"), append(args, f.Resource)
		}
		if f.SubjectKind != "" {
			where, args = append(where, "a.subject_kind=?"), append(args, f.SubjectKind)
		}
		if f.SubjectID != "" {
			where, args = append(where, "a.subject_id=?"), append(args, f.SubjectID)
		}
		where, args = timeWindow(where, args, "a", f)
		args = append(args, pageLimit(f.Limit))
		// A display (a wall TV, migration 00044) is named after itself; its id is never a user's.
		return tx.Raw(`SELECT a.id::text AS id,a.at,a.actor_id::text AS actor_id,a.actor_kind,
      CASE WHEN a.actor_kind='display' THEN 'จอ · '||d.name ELSE u.name END AS actor_name,a.resource,a.subject_kind,a.subject_id,a.request_id,host(a.client_ip) AS client_ip
      FROM core.access_log a LEFT JOIN identity.users u ON u.id=a.actor_id AND a.actor_kind='member'
      LEFT JOIN core.displays d ON d.id=a.actor_id AND a.actor_kind='display'
      WHERE `+strings.Join(where, " AND ")+` ORDER BY a.at DESC,a.id DESC LIMIT ?`, args...).Scan(&out).Error
	})
	return out, e
}

// timeWindow adds the from/to bounds and the keyset cursor (at, id) of the previous page.
func timeWindow(where []string, args []any, alias string, f domain.LogFilter) ([]string, []any) {
	if f.From != nil {
		where, args = append(where, alias+".at>=?"), append(args, *f.From)
	}
	if f.To != nil {
		where, args = append(where, alias+".at<?"), append(args, *f.To)
	}
	if f.BeforeAt != nil && f.BeforeID != "" {
		where, args = append(where, "("+alias+".at,"+alias+".id)<(?,?::uuid)"), append(args, *f.BeforeAt, f.BeforeID)
	}
	return where, args
}

// ListAuditLog pages the workspace's audit trail (who changed what), newest first. Owner only.
func (r *Repository) ListAuditLog(ctx context.Context, p domain.Principal, f domain.LogFilter) ([]domain.AuditEntry, error) {
	out := []domain.AuditEntry{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := requireOwner(tx, p); e != nil {
			return e
		}
		where, args := []string{"l.tenant_id=core.tenant_id()"}, []any{}
		if f.ActorID != "" {
			where, args = append(where, "l.actor_id=?"), append(args, f.ActorID)
		}
		if f.Action != "" {
			where, args = append(where, "left(l.action,length(?))=?"), append(args, f.Action, f.Action)
		}
		where, args = timeWindow(where, args, "l", f)
		args = append(args, pageLimit(f.Limit))
		return tx.Raw(`SELECT l.id::text AS id,l.at,l.actor_id::text AS actor_id,coalesce(u.name,'จอ · '||d.name) AS actor_name,l.action,l.target_id::text AS target_id
      FROM core.audit_logs l LEFT JOIN identity.users u ON u.id=l.actor_id LEFT JOIN core.displays d ON d.id=l.actor_id
      WHERE `+strings.Join(where, " AND ")+` ORDER BY l.at DESC,l.id DESC LIMIT ?`, args...).Scan(&out).Error
	})
	return out, e
}

// ListErasures lists the workspace's erasure ledger, newest first. Owner only.
func (r *Repository) ListErasures(ctx context.Context, p domain.Principal, limit int) ([]domain.ErasureEntry, error) {
	out := []domain.ErasureEntry{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := requireOwner(tx, p); e != nil {
			return e
		}
		var rows []struct {
			ID, TenantID, SubjectKind, SubjectRef, Counts, Scope string
			At                                                   time.Time
			ActorID                                              *string
		}
		if e := tx.Raw(`SELECT id::text AS id,tenant_id::text AS tenant_id,at,actor_id::text AS actor_id,subject_kind,subject_ref,counts::text AS counts,scope::text AS scope
      FROM core.erasure_log WHERE tenant_id=core.tenant_id() ORDER BY at DESC,id DESC LIMIT ?`, pageLimit(limit)).Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			out = append(out, domain.ErasureEntry{ID: row.ID, TenantID: row.TenantID, At: row.At, ActorID: row.ActorID, SubjectKind: row.SubjectKind, SubjectRef: row.SubjectRef, Counts: json.RawMessage(row.Counts), Scope: json.RawMessage(row.Scope)})
		}
		return nil
	})
	return out, e
}

// EraseMember erases a member of this workspace (identity.erase_member): sessions, membership and access go,
// the identity keeps its uuid but not its email, name or password. Flows they enabled that command devices are
// disarmed (in SQL, so the operator's erase-user does the same). Owner only; not oneself, not the last owner,
// not an identity another workspace shares (ErrConflict with a reason).
func (r *Repository) EraseMember(ctx context.Context, p domain.Principal, target string) (domain.ErasureResult, error) {
	var out domain.ErasureResult
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := memberLock(tx, p.TenantID); e != nil {
			return e
		}
		if e := requireOwner(tx, p); e != nil {
			return e
		}
		var rows []struct {
			Outcome string
			Counts  *string
		}
		if e := tx.Raw(`SELECT outcome,counts::text AS counts FROM identity.erase_member(?)`, target).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrInvalid
		}
		switch rows[0].Outcome {
		case "erased":
		case "not_found":
			return domain.ErrNotFound
		case "shared":
			return domain.Because(domain.ErrConflict, "shared_identity")
		case "last_owner":
			return domain.Because(domain.ErrConflict, "last_owner")
		default: // refused, self
			return domain.ErrForbidden
		}
		// core.erase_member_data has disarmed the flows that commanded devices with the member's authority.
		out = domain.ErasureResult{Outcome: "erased", Counts: json.RawMessage(*rows[0].Counts)}
		return audit(tx, p, "member.erased", target)
	})
	return out, e
}

// deviceIdentity resolves a registration (active or removed) to its tag identity.
func deviceIdentity(tx *gorm.DB, device string) (string, error) {
	var rows []struct{ ExternalID string }
	if e := tx.Raw(`SELECT external_id FROM core.devices WHERE tenant_id=core.tenant_id() AND id=?`, device).Scan(&rows).Error; e != nil {
		return "", e
	}
	if len(rows) != 1 {
		return "", domain.ErrNotFound
	}
	return rows[0].ExternalID, nil
}

// EraseIdentityHistory erases what a registration's tag left behind: samples, raw advertisements, presence and
// events (alerts and the events they point at stay, anonymised), within the registration's project unless
// tenantWide. A new name, when given, renames the registration in the same transaction: a tag handed to a new
// wearer never shows the old name after its history is gone. Owner only.
func (r *Repository) EraseIdentityHistory(ctx context.Context, p domain.Principal, device string, name *string, tenantWide bool) (domain.ErasureResult, error) {
	var out domain.ErasureResult
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := requireOwner(tx, p); e != nil {
			return e
		}
		var rows []struct {
			Outcome string
			Counts  *string
		}
		if e := tx.Raw(`SELECT outcome,counts::text AS counts FROM core.erase_identity_history(?,?,?)`, device, tenantWide, name != nil).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrInvalid
		}
		switch rows[0].Outcome {
		case "erased":
		case "not_found":
			return domain.ErrNotFound
		default:
			return domain.ErrForbidden
		}
		out = domain.ErasureResult{Outcome: "erased", Counts: json.RawMessage(*rows[0].Counts)}
		if name != nil {
			res := tx.Exec(`UPDATE core.devices SET name=? WHERE tenant_id=core.tenant_id() AND id=? AND removed_at IS NULL`, *name, device)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 1 {
				if e := audit(tx, p, "device.renamed", device); e != nil {
					return e
				}
			}
		}
		return audit(tx, p, "device.history_erased", device)
	})
	return out, e
}

// jsonOf runs a query producing one json value (or NULL, answered as fallback).
func jsonOf(tx *gorm.DB, fallback, query string, args ...any) (json.RawMessage, error) {
	var rows []struct{ J *string }
	if e := tx.Raw(`SELECT (`+query+`)::text AS j`, args...).Scan(&rows).Error; e != nil {
		return nil, e
	}
	if len(rows) != 1 || rows[0].J == nil {
		return json.RawMessage(fallback), nil
	}
	return json.RawMessage(*rows[0].J), nil
}

// ExportMember gathers what this workspace holds about one member. The member themself may export their own
// data; an owner may export anybody's in the workspace. The export is audited.
func (r *Repository) ExportMember(ctx context.Context, p domain.Principal, target string) (domain.MemberExport, error) {
	out := domain.MemberExport{GeneratedAt: time.Now().UTC(), TenantID: p.TenantID}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if target != p.UserID {
			if e := requireOwner(tx, p); e != nil {
				return e
			}
		}
		if _, e := roleOf(tx, p.TenantID, target); e != nil {
			return e
		}
		parts := []struct {
			dst      *json.RawMessage
			fallback string
			query    string
			args     []any
		}{
			{&out.Profile, "null", `SELECT row_to_json(t) FROM (SELECT u.id,u.email,u.name,u.created_at,u.notice_ack_version FROM identity.users u WHERE u.id=?) t`, []any{target}},
			{&out.Membership, "null", `SELECT row_to_json(t) FROM (SELECT m.role,m.must_change_password,
        (SELECT coalesce(json_agg(mp.project_id ORDER BY mp.project_id),'[]'::json) FROM core.member_projects mp WHERE mp.tenant_id=m.tenant_id AND mp.user_id=m.user_id) AS project_ids,
        (SELECT ma.permissions FROM core.member_access ma WHERE ma.tenant_id=m.tenant_id AND ma.user_id=m.user_id) AS module_access
        FROM core.memberships m WHERE m.tenant_id=core.tenant_id() AND m.user_id=?) t`, []any{target}},
			{&out.Sessions, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM identity.member_sessions(?) t`, []any{target}},
			{&out.Audit, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT l.at,l.action,l.target_id FROM core.audit_logs l
        WHERE l.tenant_id=core.tenant_id() AND l.actor_id=? ORDER BY l.at DESC LIMIT 5000) t`, []any{target}},
			{&out.Alerts, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT a.id,a.title,a.event_type,a.severity,a.status,a.opened_at,
        a.acked_by=? AS acknowledged,a.acked_at,a.resolved_by=? AS resolved,a.resolved_at,a.note FROM core.alerts a
        WHERE a.tenant_id=core.tenant_id() AND (a.acked_by=? OR a.resolved_by=?) ORDER BY a.opened_at DESC LIMIT 5000) t`, []any{target, target, target, target}},
			{&out.Commands, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT c.id,c.device_id,c.property,c.requested,c.value,c.status,c.created_at
        FROM core.device_commands c WHERE c.tenant_id=core.tenant_id() AND c.actor_id=? ORDER BY c.created_at DESC LIMIT 5000) t`, []any{target}},
			{&out.Flows, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT f.id,f.name,f.enabled,f.created_at,f.updated_at
        FROM core.automations f WHERE f.tenant_id=core.tenant_id() AND f.enabled_by=? ORDER BY f.updated_at DESC LIMIT 1000) t`, []any{target}},
			{&out.AccessLog, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM core.member_access_log(?, 5000) t`, []any{target}},
		}
		for _, part := range parts {
			v, e := jsonOf(tx, part.fallback, part.query, part.args...)
			if e != nil {
				return e
			}
			*part.dst = v
		}
		out.Notes = []string{
			"Lists are capped at the newest 5000 rows each (flows: 1000).",
			"A member restricted to some projects sees alerts and commands of those projects only.",
			"Security records (audit trail, read-access log) are kept as long as the workspace's retention says, also after an erasure, under the member's anonymous id.",
		}
		action := "member.exported"
		if target == p.UserID {
			action = "privacy.self_exported"
		}
		return audit(tx, p, action, target)
	})
	return out, e
}

// IdentityExportSummary resolves a registration's tag and gathers the small parts of its export (registration,
// presence, events, alerts), and records the export in the audit trail, before the history streams. Owner only.
func (r *Repository) IdentityExportSummary(ctx context.Context, p domain.Principal, device string) (domain.IdentityExport, error) {
	out := domain.IdentityExport{GeneratedAt: time.Now().UTC()}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := requireOwner(tx, p); e != nil {
			return e
		}
		external, e := deviceIdentity(tx, device)
		if e != nil {
			return e
		}
		out.ExternalID = external
		parts := []struct {
			dst      *json.RawMessage
			fallback string
			query    string
			args     []any
		}{
			{&out.Device, "null", `SELECT row_to_json(t) FROM (SELECT d.id,d.name,d.external_id,d.gateway_id,d.profile_id,d.roaming,d.created_at,d.removed_at
        FROM core.devices d WHERE d.tenant_id=core.tenant_id() AND d.id=?) t`, []any{device}},
			{&out.Presence, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT p.gateway_id,p.since,p.updated_at FROM core.presence_state p
        WHERE p.tenant_id=core.tenant_id() AND p.external_id=?) t`, []any{external}},
			{&out.Events, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT e.id,e.gateway_id,e.device_name,e.event_type,e.detail,e.occurred_at
        FROM core.device_events e WHERE e.tenant_id=core.tenant_id() AND e.external_id=? ORDER BY e.occurred_at DESC LIMIT 20000) t`, []any{external}},
			{&out.Alerts, "[]", `SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT a.id,a.title,a.event_type,a.severity,a.status,a.opened_at,a.acked_at,a.resolved_at,a.note
        FROM core.alerts a WHERE a.tenant_id=core.tenant_id() AND a.external_id=? ORDER BY a.opened_at DESC LIMIT 5000) t`, []any{external}},
		}
		for _, part := range parts {
			v, e := jsonOf(tx, part.fallback, part.query, part.args...)
			if e != nil {
				return e
			}
			*part.dst = v
		}
		return audit(tx, p, "device.history_exported", device)
	})
	return out, e
}

// StreamIdentityHistory streams one history of a tag ("samples", "ble_history" or "presence_history") as JSON lines, oldest first, at
// most limit rows, in its own transaction; emit gets each line. truncated reports that the cap was reached.
func (r *Repository) StreamIdentityHistory(ctx context.Context, p domain.Principal, external, kind string, limit int, emit func([]byte) error) (bool, error) {
	var query string
	switch kind {
	case "samples":
		query = `SELECT row_to_json(t)::text FROM (SELECT s.gateway_id,s.received_at,s.decoder_id,s.reading FROM core.sensor_samples s
      WHERE s.tenant_id=core.tenant_id() AND s.external_id=? ORDER BY s.received_at LIMIT ?) t`
	case "ble_history":
		query = `SELECT row_to_json(t)::text FROM (SELECT b.gateway_id,b.received_at,b.source,b.raw FROM core.ble_history b
      WHERE b.tenant_id=core.tenant_id() AND b.external_id=? ORDER BY b.received_at LIMIT ?) t`
	case "presence_history":
		// The digital twin's movement history (migration 00046): which zone (gateway) the tag moved to, and when.
		// Stored in lower case (00046), whatever case the registration uses.
		query = `SELECT row_to_json(t)::text FROM (SELECT h.at,h.gateway_id,h.from_gateway_id,h.rssi_avg FROM core.presence_history h
      WHERE h.tenant_id=core.tenant_id() AND h.external_id=lower(?) ORDER BY h.at LIMIT ?) t`
	default:
		return false, domain.ErrInvalid
	}
	n := 0
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := requireOwner(tx, p); e != nil {
			return e
		}
		if e := tx.Exec(`SET TRANSACTION READ ONLY`).Error; e != nil {
			return e
		}
		rows, e := tx.Raw(query, external, limit+1).Rows()
		if e != nil {
			return e
		}
		defer rows.Close()
		var line []byte
		for rows.Next() {
			if n == limit {
				n++
				break
			}
			if e := rows.Scan(&line); e != nil {
				return e
			}
			if e := emit(line); e != nil {
				return e
			}
			n++
		}
		return rows.Err()
	})
	return n > limit, e
}

// AckNotice records that the caller acknowledged privacy notice `version`; answers the stored version.
func (r *Repository) AckNotice(ctx context.Context, p domain.Principal, version int) (int, error) {
	var out []struct{ V *int }
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT identity.ack_notice(?) AS v`, version).Scan(&out).Error
	})
	if e != nil {
		return 0, e
	}
	if len(out) != 1 || out[0].V == nil {
		return 0, domain.ErrNotFound
	}
	return *out[0].V, nil
}
