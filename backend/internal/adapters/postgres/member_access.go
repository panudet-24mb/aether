package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"gorm.io/gorm"
)

func (r *Repository) MemberAccess(ctx context.Context, p domain.Principal, target string) (map[string]string, error) {
	out := map[string]string{}
	err := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []struct{ Permissions string }
		if e := tx.Raw(`SELECT permissions::text FROM core.member_access WHERE tenant_id=? AND user_id=?`, p.TenantID, target).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) > 0 {
			return json.Unmarshal([]byte(rows[0].Permissions), &out)
		}
		return nil
	})
	return out, err
}
func (r *Repository) SetMemberAccess(ctx context.Context, p domain.Principal, target string, permissions map[string]string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := memberLock(tx, p.TenantID); e != nil {
			return e
		}
		actor, e := roleOf(tx, p.TenantID, p.UserID)
		if e != nil {
			return e
		}
		role, e := roleOf(tx, p.TenantID, target)
		if e != nil {
			return e
		}
		if target == p.UserID || role == "owner" || !mayTouch(actor, role) {
			return domain.ErrForbidden
		}
		data, e := json.Marshal(permissions)
		if e != nil {
			return e
		}
		if e = tx.Exec(`INSERT INTO core.member_access(tenant_id,user_id,permissions) VALUES(?,?,?::jsonb) ON CONFLICT(tenant_id,user_id) DO UPDATE SET permissions=EXCLUDED.permissions`, p.TenantID, target, string(data)).Error; e != nil {
			return e
		}
		return audit(tx, p, "member.access_updated", target)
	})
}
