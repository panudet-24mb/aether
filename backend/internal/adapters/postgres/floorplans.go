package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type floorRow struct {
	ID, SiteID, Name         string
	Level, Revision          int
	WidthM, DepthM, CeilingM float64
	HasImage                 bool
	UpdatedAt                time.Time
	Layout                   json.RawMessage
}

func (row floorRow) floor(full bool) domain.Floor {
	f := domain.Floor{ID: row.ID, SiteID: row.SiteID, Name: row.Name, Level: row.Level, WidthM: row.WidthM, DepthM: row.DepthM, CeilingM: row.CeilingM, Revision: row.Revision, HasImage: row.HasImage, UpdatedAt: row.UpdatedAt, Placements: []domain.FloorPlacement{}}
	if full {
		_ = json.Unmarshal(row.Layout, &f.Layout)
	}
	normalizeLayout(&f.Layout)
	return f
}

func normalizeLayout(l *domain.FloorLayout) {
	if l.Walls == nil {
		l.Walls = []domain.FloorWall{}
	}
	if l.Zones == nil {
		l.Zones = []domain.FloorZone{}
	}
	if l.Items == nil {
		l.Items = []domain.FloorItem{}
	}
}

// siteRow keeps GORM away from the nested Floors slice (it cannot map it and logs a parse error).
type siteRow struct {
	ID, Name, Description string
	ProjectID             *string
	CreatedAt             time.Time
}

func (row siteRow) site() domain.Site {
	return domain.Site{ID: row.ID, ProjectID: row.ProjectID, Name: row.Name, Description: row.Description, CreatedAt: row.CreatedAt, Floors: []domain.Floor{}}
}

const floorColumns = `f.id,f.site_id,f.name,f.level,f.width_m,f.depth_m,f.ceiling_m,f.revision,f.updated_at,f.layout,
  EXISTS(SELECT 1 FROM core.floor_images i WHERE i.tenant_id=f.tenant_id AND i.floor_id=f.id) AS has_image`

// ListSites returns every active site with its floors (metadata only; layouts come from GetSite).
func (r *Repository) ListSites(ctx context.Context, p domain.Principal) ([]domain.Site, error) {
	out := []domain.Site{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var siteRows []siteRow
		if e := tx.Raw(`SELECT id,project_id,name,description,created_at FROM core.sites WHERE archived_at IS NULL ORDER BY created_at,id LIMIT 100`).Scan(&siteRows).Error; e != nil {
			return e
		}
		for _, row := range siteRows {
			out = append(out, row.site())
		}
		var rows []floorRow
		if e := tx.Raw(`SELECT ` + floorColumns + ` FROM core.floors f JOIN core.sites s ON s.tenant_id=f.tenant_id AND s.id=f.site_id AND s.archived_at IS NULL ORDER BY f.level,f.created_at LIMIT 2000`).Scan(&rows).Error; e != nil {
			return e
		}
		for i := range out {
			for _, row := range rows {
				if row.SiteID == out[i].ID {
					out[i].Floors = append(out[i].Floors, row.floor(false))
				}
			}
		}
		return nil
	})
	return out, e
}

// GetSite returns one site with full layouts and placements of all its floors (the 3D view stacks them).
func (r *Repository) GetSite(ctx context.Context, p domain.Principal, id string) (domain.Site, error) {
	var site domain.Site
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var found []siteRow
		if e := tx.Raw(`SELECT id,project_id,name,description,created_at FROM core.sites WHERE id=? AND archived_at IS NULL`, id).Scan(&found).Error; e != nil {
			return e
		}
		if len(found) != 1 {
			return domain.ErrNotFound
		}
		site = found[0].site()
		var rows []floorRow
		if e := tx.Raw(`SELECT `+floorColumns+` FROM core.floors f WHERE f.site_id=? ORDER BY f.level,f.created_at LIMIT 60`, id).Scan(&rows).Error; e != nil {
			return e
		}
		// Placements of assets that were revoked or withdrawn since are not shown; the rows go away on the next save.
		var placed []struct {
			FloorID string
			domain.FloorPlacement
		}
		if e := tx.Raw(`SELECT pl.floor_id,pl.asset_kind,pl.asset_id,pl.x,pl.y,pl.z FROM core.floor_placements pl JOIN core.floors f ON f.tenant_id=pl.tenant_id AND f.id=pl.floor_id
      WHERE f.site_id=? AND ((pl.asset_kind='gateway' AND EXISTS(SELECT 1 FROM core.gateways g WHERE g.tenant_id=pl.tenant_id AND g.id=pl.asset_id AND g.revoked_at IS NULL))
        OR (pl.asset_kind='device' AND EXISTS(SELECT 1 FROM core.devices d WHERE d.tenant_id=pl.tenant_id AND d.id=pl.asset_id AND d.removed_at IS NULL)))`, id).Scan(&placed).Error; e != nil {
			return e
		}
		for _, row := range rows {
			f := row.floor(true)
			for _, pl := range placed {
				if pl.FloorID == f.ID {
					f.Placements = append(f.Placements, pl.FloorPlacement)
				}
			}
			site.Floors = append(site.Floors, f)
		}
		return nil
	})
	return site, e
}

func projectExists(tx *gorm.DB, id *string) error {
	if id == nil {
		return nil
	}
	var n int64
	if e := tx.Raw(`SELECT count(*) FROM core.projects WHERE id=? AND archived_at IS NULL`, *id).Scan(&n).Error; e != nil {
		return e
	}
	if n != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *Repository) CreateSite(ctx context.Context, p domain.Principal, s domain.Site) (domain.Site, error) {
	s.ID = uuid.NewString()
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.sites WHERE archived_at IS NULL`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= 100 {
			return domain.ErrConflict
		}
		if e := projectExists(tx, s.ProjectID); e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.sites(tenant_id,id,project_id,name,description) VALUES(?,?,?,?,?)`, p.TenantID, s.ID, s.ProjectID, s.Name, s.Description).Error; e != nil {
			return e
		}
		// A new building starts with one floor so the editor opens on something.
		if e := tx.Exec(`INSERT INTO core.floors(tenant_id,id,site_id,name,level) VALUES(?,?,?,?,1)`, p.TenantID, uuid.NewString(), s.ID, "ชั้น 1").Error; e != nil {
			return e
		}
		return audit(tx, p, "site.created", s.ID)
	})
	if e != nil {
		return s, classify(e)
	}
	return r.GetSite(ctx, p, s.ID)
}

func (r *Repository) UpdateSite(ctx context.Context, p domain.Principal, s domain.Site) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := projectExists(tx, s.ProjectID); e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.sites SET name=?,description=?,project_id=? WHERE id=? AND archived_at IS NULL`, s.Name, s.Description, s.ProjectID, s.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "site.updated", s.ID)
	})
}

func (r *Repository) ArchiveSite(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE core.sites SET archived_at=now() WHERE id=? AND archived_at IS NULL`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		// Assets of an archived building are free to be placed elsewhere.
		if e := tx.Exec(`DELETE FROM core.floor_placements pl USING core.floors f WHERE f.tenant_id=pl.tenant_id AND f.id=pl.floor_id AND f.site_id=?`, id).Error; e != nil {
			return e
		}
		return audit(tx, p, "site.archived", id)
	})
}

func (r *Repository) CreateFloor(ctx context.Context, p domain.Principal, f domain.Floor) (domain.Floor, error) {
	f.ID = uuid.NewString()
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.sites WHERE id=? AND archived_at IS NULL`, f.SiteID).Scan(&n).Error; e != nil {
			return e
		}
		if n != 1 {
			return domain.ErrNotFound
		}
		if e := tx.Raw(`SELECT count(*) FROM core.floors WHERE site_id=?`, f.SiteID).Scan(&n).Error; e != nil {
			return e
		}
		if n >= 60 {
			return domain.ErrConflict
		}
		layout, _ := json.Marshal(f.Layout)
		if e := tx.Exec(`INSERT INTO core.floors(tenant_id,id,site_id,name,level,width_m,depth_m,ceiling_m,layout) VALUES(?,?,?,?,?,?,?,?,?::jsonb)`, p.TenantID, f.ID, f.SiteID, f.Name, f.Level, f.WidthM, f.DepthM, f.CeilingM, string(layout)).Error; e != nil {
			return e
		}
		return audit(tx, p, "floor.created", f.ID)
	})
	f.Revision, f.Placements = 1, []domain.FloorPlacement{}
	normalizeLayout(&f.Layout)
	return f, classify(e)
}

// SaveFloor replaces metadata, drawing and placements in one transaction. The revision must match the one the
// editor loaded, so two people editing the same floor cannot silently overwrite each other.
func (r *Repository) SaveFloor(ctx context.Context, p domain.Principal, f domain.Floor) (int, map[string]int, error) {
	next := 0
	bumped := map[string]int{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var current []struct{ Revision int }
		if e := tx.Raw(`SELECT revision FROM core.floors WHERE id=? FOR UPDATE`, f.ID).Scan(&current).Error; e != nil {
			return e
		}
		if len(current) != 1 {
			return domain.ErrNotFound
		}
		if current[0].Revision != f.Revision {
			return domain.ErrConflict
		}
		var siteScope struct{ ProjectID *string }
		if e := tx.Raw(`SELECT s.project_id FROM core.sites s JOIN core.floors f ON f.site_id=s.id AND f.tenant_id=s.tenant_id WHERE f.id=?`, f.ID).Scan(&siteScope).Error; e != nil {
			return e
		}
		for _, kind := range []string{"gateway", "device"} {
			ids := []string{}
			for _, pl := range f.Placements {
				if pl.AssetKind == kind {
					ids = append(ids, pl.AssetID)
				}
			}
			if kind == "gateway" {
				seen := map[string]bool{}
				for _, id := range ids {
					seen[id] = true
				}
				for _, zone := range f.Layout.Zones {
					for _, id := range zone.GatewayIDs {
						if !seen[id] {
							ids = append(ids, id)
							seen[id] = true
						}
					}
				}
			}
			if len(ids) == 0 {
				continue
			}
			q := `SELECT count(*) FROM core.gateways WHERE id IN ? AND revoked_at IS NULL`
			if kind == "device" {
				q = `SELECT count(*) FROM core.devices WHERE id IN ? AND removed_at IS NULL`
			}
			var n int64
			args := []any{ids}
			if siteScope.ProjectID != nil {
				if kind == "gateway" {
					q += ` AND project_id=?`
				} else {
					q += ` AND gateway_id IN (SELECT id FROM core.gateways WHERE project_id=?)`
				}
				args = append(args, *siteScope.ProjectID)
			}
			if e := tx.Raw(q, args...).Scan(&n).Error; e != nil {
				return e
			}
			if int(n) != len(ids) {
				return domain.ErrNotFound
			}
		}
		layout, _ := json.Marshal(f.Layout)
		next = f.Revision + 1
		if e := tx.Exec(`UPDATE core.floors SET name=?,level=?,width_m=?,depth_m=?,ceiling_m=?,layout=?::jsonb,revision=?,updated_at=now() WHERE id=?`, f.Name, f.Level, f.WidthM, f.DepthM, f.CeilingM, string(layout), next, f.ID).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.floor_placements WHERE floor_id=?`, f.ID).Error; e != nil {
			return e
		}
		// Floors that lose an asset to this one get a new revision, so a stale draft of theirs cannot put it back.
		for _, kind := range []string{"gateway", "device"} {
			ids := []string{}
			for _, pl := range f.Placements {
				if pl.AssetKind == kind {
					ids = append(ids, pl.AssetID)
				}
			}
			if len(ids) == 0 {
				continue
			}
			var losers []struct {
				ID       string
				Revision int
			}
			if e := tx.Raw(`UPDATE core.floors fl SET revision=fl.revision+1,updated_at=now() WHERE fl.id<>? AND fl.id IN (SELECT floor_id FROM core.floor_placements WHERE asset_kind=? AND asset_id IN ?) RETURNING fl.id,fl.revision`, f.ID, kind, ids).Scan(&losers).Error; e != nil {
				return e
			}
			for _, l := range losers {
				bumped[l.ID] = l.Revision
			}
		}
		for _, pl := range f.Placements {
			// An asset stands in one place: placing it here takes it off any other floor.
			if e := tx.Exec(`INSERT INTO core.floor_placements(tenant_id,asset_kind,asset_id,floor_id,x,y,z) VALUES(?,?,?,?,?,?,?)
        ON CONFLICT(tenant_id,asset_kind,asset_id) DO UPDATE SET floor_id=EXCLUDED.floor_id,x=EXCLUDED.x,y=EXCLUDED.y,z=EXCLUDED.z`, p.TenantID, pl.AssetKind, pl.AssetID, f.ID, pl.X, pl.Y, pl.Z).Error; e != nil {
				return e
			}
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "floor.saved", f.ID)
	})
	return next, bumped, e
}

func (r *Repository) DeleteFloor(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(`DELETE FROM core.floors WHERE id=?`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "floor.deleted", id)
	})
}

func (r *Repository) SetFloorImage(ctx context.Context, p domain.Principal, floor, mime string, data []byte) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.floors WHERE id=?`, floor).Scan(&n).Error; e != nil {
			return e
		}
		if n != 1 {
			return domain.ErrNotFound
		}
		if len(data) == 0 {
			return tx.Exec(`DELETE FROM core.floor_images WHERE floor_id=?`, floor).Error
		}
		return tx.Exec(`INSERT INTO core.floor_images(tenant_id,floor_id,mime,data) VALUES(?,?,?,?)
      ON CONFLICT(tenant_id,floor_id) DO UPDATE SET mime=EXCLUDED.mime,data=EXCLUDED.data,updated_at=now()`, p.TenantID, floor, mime, data).Error
	})
}

func (r *Repository) FloorImage(ctx context.Context, p domain.Principal, floor string) (string, []byte, error) {
	var rows []struct {
		Mime string
		Data []byte
	}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT mime,data FROM core.floor_images WHERE floor_id=?`, floor).Scan(&rows).Error
	})
	if e != nil {
		return "", nil, e
	}
	if len(rows) != 1 {
		return "", nil, domain.ErrNotFound
	}
	return rows[0].Mime, rows[0].Data, nil
}
