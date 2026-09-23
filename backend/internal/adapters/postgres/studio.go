package postgres

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/studio"
	"context"
	"gorm.io/gorm"
)

func (r *Repository) StudioList(ctx context.Context, p domain.Principal, kind string) ([]studio.Item, error) {
	out := []studio.Item{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT id,tenant_id,kind,name,brand,model,version,visibility,definition,revision FROM core.studio_items WHERE kind=? ORDER BY updated_at DESC LIMIT 200`, kind).Scan(&out).Error
	})
	return out, e
}
func (r *Repository) StudioGet(ctx context.Context, p domain.Principal, id string) (studio.Item, error) {
	var out studio.Item
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		q := tx.Raw(`SELECT id,tenant_id,kind,name,brand,model,version,visibility,definition,revision FROM core.studio_items WHERE id=?`, id).Scan(&out)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return nil
	})
	return out, e
}
func (r *Repository) StudioCreate(ctx context.Context, p domain.Principal, i studio.Item) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,3))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.studio_items WHERE tenant_id=?`, p.TenantID).Scan(&n).Error; e != nil {
			return e
		}
		if n >= 200 {
			return domain.ErrConflict
		}
		if e := tx.Exec(`INSERT INTO core.studio_items(id,tenant_id,kind,name,brand,model,version,visibility,definition) VALUES(?,?,?,?,?,?,?,?,?::jsonb)`, i.ID, p.TenantID, i.Kind, i.Name, i.Brand, i.Model, i.Version, i.Visibility, string(i.Definition)).Error; e != nil {
			return e
		}
		return audit(tx, p, "studio.created", i.ID)
	}))
}
func (r *Repository) StudioSave(ctx context.Context, p domain.Principal, i studio.Item) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		q := tx.Exec(`UPDATE core.studio_items SET name=?,definition=?::jsonb,revision=revision+1,updated_at=now() WHERE id=? AND tenant_id=? AND kind='dashboard' AND revision=?`, i.Name, string(i.Definition), i.ID, p.TenantID, i.Revision)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return domain.ErrConflict
		}
		return audit(tx, p, "dashboard.saved", i.ID)
	}))
}
func (r *Repository) StudioDelete(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		q := tx.Exec(`DELETE FROM core.studio_items WHERE id=? AND tenant_id=? AND kind='dashboard'`, id, p.TenantID)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "dashboard.deleted", id)
	})
}
