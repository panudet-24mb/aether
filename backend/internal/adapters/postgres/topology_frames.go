package postgres

import (
	"aether/backend/internal/domain"
	"context"

	"gorm.io/gorm"
)

// Frames on the connect canvas (migration 00052). Row level security decides which frames a member sees and may
// change: those of their projects, or every frame for a member who sees every project. Each change sends the
// "layout" signal, so open canvases fetch the board again.

const frameColumns = `id::text AS id,view,project_id::text AS project_id,name,color,x,y,w,h,updated_at`

func (r *Repository) ListFrames(ctx context.Context, p domain.Principal, view string) ([]domain.TopologyFrame, error) {
	out := []domain.TopologyFrame{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT `+frameColumns+` FROM core.topology_frames WHERE tenant_id=core.tenant_id() AND view=? ORDER BY created_at,id LIMIT ?`, view, domain.MaxFrames).Scan(&out).Error
	})
	return out, e
}

func (r *Repository) CreateFrame(ctx context.Context, p domain.Principal, f domain.TopologyFrame) (domain.TopologyFrame, error) {
	var out domain.TopologyFrame
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		// One creator at a time per workspace, so the cap below cannot be passed by two saves at once.
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,3))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.topology_frames WHERE tenant_id=core.tenant_id() AND view=?`, f.View).Scan(&n).Error; e != nil {
			return e
		}
		if n >= domain.MaxFrames {
			return domain.Because(domain.ErrConflict, "frames_full")
		}
		if f.ProjectID != nil {
			// The project must be live and visible to the caller (RLS hides the others).
			var live int64
			if e := tx.Raw(`SELECT count(*) FROM core.projects WHERE id=? AND archived_at IS NULL`, *f.ProjectID).Scan(&live).Error; e != nil {
				return e
			}
			if live != 1 {
				return domain.ErrInvalid
			}
		}
		var rows []domain.TopologyFrame
		e := tx.Raw(`INSERT INTO core.topology_frames(tenant_id,id,view,project_id,name,color,x,y,w,h,created_by,updated_by)
 VALUES(core.tenant_id(),?,?,?::uuid,?,?,?,?,?,?,nullif(?,'')::uuid,nullif(?,'')::uuid) RETURNING `+frameColumns,
			f.ID, f.View, f.ProjectID, f.Name, f.Color, f.X, f.Y, f.W, f.H, p.UserID, p.UserID).Scan(&rows).Error
		if insufficientPrivilege(e) {
			return domain.ErrForbidden // a frame outside the member's projects, or without a project
		}
		if e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrForbidden
		}
		out = rows[0]
		if e := audit(tx, p, "topology.frame_created", f.ID); e != nil {
			return e
		}
		return signal(tx, p.TenantID, "layout", "")
	})
	return out, e
}

// UpdateFrame renames, recolours, moves or resizes a frame; its view and project never change.
func (r *Repository) UpdateFrame(ctx context.Context, p domain.Principal, f domain.TopologyFrame) (domain.TopologyFrame, error) {
	var out domain.TopologyFrame
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []domain.TopologyFrame
		e := tx.Raw(`UPDATE core.topology_frames SET name=?,color=?,x=?,y=?,w=?,h=?,updated_by=nullif(?,'')::uuid,updated_at=now()
 WHERE tenant_id=core.tenant_id() AND id=? RETURNING `+frameColumns, f.Name, f.Color, f.X, f.Y, f.W, f.H, p.UserID, f.ID).Scan(&rows).Error
		if e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrNotFound // gone, or outside the member's projects
		}
		out = rows[0]
		return signal(tx, p.TenantID, "layout", "")
	})
	return out, e
}

// DeleteFrame forgets a frame. The nodes inside it stay where they are.
func (r *Repository) DeleteFrame(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`DELETE FROM core.topology_frames WHERE tenant_id=core.tenant_id() AND id=?`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := audit(tx, p, "topology.frame_deleted", id); e != nil {
			return e
		}
		return signal(tx, p.TenantID, "layout", "")
	})
}
