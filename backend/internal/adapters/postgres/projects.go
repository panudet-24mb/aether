package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"encoding/json"

	"gorm.io/gorm"
)

// SignalChannel carries small "something changed" notices between processes (the MQTT ingest container
// writes packets, the API container owns the WebSocket clients). NOTIFY is delivered on commit only.
const SignalChannel = "aether_signal"

func signal(tx *gorm.DB, tenant, kind, gateway string) error {
	payload, _ := json.Marshal(domain.Signal{Tenant: tenant, Kind: kind, Gateway: gateway})
	return tx.Exec(`SELECT pg_notify(?, ?)`, SignalChannel, string(payload)).Error
}

func (r *Repository) ListProjects(ctx context.Context, p domain.Principal, includeArchived bool) ([]domain.Project, error) {
	out := []domain.Project{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		q := `SELECT pr.id,pr.name,pr.description,pr.color,pr.archived_at,pr.created_at,
      (SELECT count(*) FROM core.gateways g WHERE g.project_id=pr.id AND g.tenant_id=pr.tenant_id AND g.revoked_at IS NULL) AS gateway_count
      FROM core.projects pr`
		if !includeArchived {
			q += ` WHERE pr.archived_at IS NULL`
		}
		return tx.Raw(q + ` ORDER BY pr.created_at,pr.id LIMIT 100`).Scan(&out).Error
	})
	return out, e
}

func (r *Repository) CreateProject(ctx context.Context, p domain.Principal, pr domain.Project) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,2))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.projects WHERE archived_at IS NULL`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= 50 {
			return domain.ErrConflict
		}
		if e := tx.Exec(`INSERT INTO core.projects(tenant_id,id,name,description,color) VALUES(?,?,?,?,?)`, p.TenantID, pr.ID, pr.Name, pr.Description, pr.Color).Error; e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "project.created", pr.ID)
	}))
}

func (r *Repository) UpdateProject(ctx context.Context, p domain.Principal, pr domain.Project) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE core.projects SET name=?,description=?,color=? WHERE id=? AND archived_at IS NULL`, pr.Name, pr.Description, pr.Color, pr.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "project.updated", pr.ID)
	}))
}

// ArchiveProject hides the project and returns its gateways to "unassigned"; nothing is deleted.
func (r *Repository) ArchiveProject(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE core.projects SET archived_at=now() WHERE id=? AND archived_at IS NULL`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := tx.Exec(`UPDATE core.gateways SET project_id=NULL WHERE project_id=?`, id).Error; e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "project.archived", id)
	})
}

// SetGatewayProject moves a gateway (with all its devices) into a project, or out of any project when nil.
func (r *Repository) SetGatewayProject(ctx context.Context, p domain.Principal, gateway string, project *string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if project != nil {
			var n int64
			if e := tx.Raw(`SELECT count(*) FROM core.projects WHERE id=? AND archived_at IS NULL`, *project).Scan(&n).Error; e != nil {
				return e
			}
			if n != 1 {
				return domain.ErrNotFound
			}
		}
		res := tx.Exec(`UPDATE core.gateways SET project_id=? WHERE id=? AND revoked_at IS NULL`, project, gateway)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, p.TenantID, "inventory", gateway); e != nil {
			return e
		}
		// An armer restricted to projects may no longer cover a flow's target on this gateway.
		if e := disarmUnauthorisedFlows(tx, p, nil); e != nil {
			return e
		}
		return audit(tx, p, "gateway.project_changed", gateway)
	})
}
