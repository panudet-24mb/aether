package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"encoding/json"

	"gorm.io/gorm"
)

// Routes of connections on the connect canvas (migration 00053). Row level security decides which a member sees and
// changes, by the gateway each line belongs to. A route is read back only while its line still exists as stored:
// the gateway is live, or the registration is live and still on the gateway the route was saved under.

func (r *Repository) ListRoutes(ctx context.Context, p domain.Principal, view string) ([]domain.EdgeRoute, error) {
	out := []domain.EdgeRoute{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []struct {
			Kind, Ref string
			Points    string
		}
		e := tx.Raw(`SELECT r.kind,r.ref::text AS ref,r.points::text AS points FROM core.topology_edge_routes r
 WHERE r.tenant_id=core.tenant_id() AND r.view=? AND CASE r.kind
  WHEN 'link' THEN EXISTS(SELECT 1 FROM core.gateways g WHERE g.tenant_id=r.tenant_id AND g.id=r.ref AND g.revoked_at IS NULL)
  ELSE EXISTS(SELECT 1 FROM core.devices d WHERE d.tenant_id=r.tenant_id AND d.id=r.ref AND d.gateway_id=r.gateway_id AND d.removed_at IS NULL) END
 ORDER BY r.kind,r.ref LIMIT ?`, view, domain.MaxRoutes).Scan(&rows).Error
		if e != nil {
			return e
		}
		for _, row := range rows {
			var points []domain.CanvasPoint
			if json.Unmarshal([]byte(row.Points), &points) != nil {
				continue
			}
			out = append(out, domain.EdgeRoute{Edge: row.Kind + ":" + row.Ref, Points: points})
		}
		return nil
	})
	return out, e
}

// SaveRoute stores the bend points of one line, or forgets them when points is empty (the line is drawn straight
// again). The line must exist and be visible to the caller.
func (r *Repository) SaveRoute(ctx context.Context, p domain.Principal, view, kind, ref string, points []domain.CanvasPoint) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var gateway []string
		q := `SELECT id::text FROM core.gateways WHERE id=? AND revoked_at IS NULL`
		if kind == "adopt" {
			q = `SELECT gateway_id::text FROM core.devices WHERE id=? AND removed_at IS NULL`
		}
		if e := tx.Raw(q, ref).Scan(&gateway).Error; e != nil {
			return e
		}
		if len(gateway) != 1 {
			return domain.ErrNotFound // gone, or outside the member's projects
		}
		if len(points) == 0 {
			if e := tx.Exec(`DELETE FROM core.topology_edge_routes WHERE tenant_id=core.tenant_id() AND view=? AND kind=? AND ref=?`, view, kind, ref).Error; e != nil {
				return e
			}
			return signal(tx, p.TenantID, "layout", "")
		}
		// One writer at a time per workspace, so the cap below cannot be passed by two saves at once.
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,4))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.topology_edge_routes WHERE tenant_id=core.tenant_id() AND view=? AND NOT (kind=? AND ref=?)`, view, kind, ref).Scan(&n).Error; e != nil {
			return e
		}
		if n >= domain.MaxRoutes {
			return domain.Because(domain.ErrConflict, "routes_full")
		}
		raw, e := json.Marshal(points)
		if e != nil {
			return e
		}
		e = tx.Exec(`INSERT INTO core.topology_edge_routes AS r(tenant_id,view,kind,ref,gateway_id,points,updated_by,updated_at)
 VALUES(core.tenant_id(),?,?,?,?,?::jsonb,nullif(?,'')::uuid,now())
 ON CONFLICT(tenant_id,view,kind,ref) DO UPDATE SET gateway_id=excluded.gateway_id,points=excluded.points,updated_by=excluded.updated_by,updated_at=excluded.updated_at`,
			view, kind, ref, gateway[0], string(raw), p.UserID).Error
		if insufficientPrivilege(e) {
			return domain.ErrNotFound // a stored route of another project the member cannot see
		}
		if e != nil {
			return e
		}
		return signal(tx, p.TenantID, "layout", "")
	})
}
