package postgres

import (
	"context"
	"time"

	"aether/backend/internal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Members of a workspace. Row level security already limits every statement here to the current tenant
// (core.is_tenant_admin, see migration 00019); the rules that RLS cannot express — nobody edits their own
// membership, an admin never touches an owner, the last owner stays — are re-checked inside the same
// transaction as the write, under the per-tenant advisory lock, so two concurrent calls cannot race past
// them. Password hashes are read only to verify the caller's own current password and never returned.

// memberLock serialises member changes of one workspace (counting owners, counting members).
func memberLock(tx *gorm.DB, tenant string) error {
	return tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,4))`, tenant).Error
}

// roleOf reads a membership role of the current tenant; ErrNotFound when the person is not a member.
func roleOf(tx *gorm.DB, tenant, user string) (string, error) {
	var rows []struct{ Role string }
	if e := tx.Raw(`SELECT role FROM core.memberships WHERE tenant_id=? AND user_id=?`, tenant, user).Scan(&rows).Error; e != nil {
		return "", e
	}
	if len(rows) != 1 {
		return "", domain.ErrNotFound
	}
	return rows[0].Role, nil
}

// mayTouch reports whether a caller with role `actor` may create, change or remove role `target`.
// An admin manages everybody except owners, and can never hand out `owner`.
func mayTouch(actor, target string) bool {
	switch actor {
	case "owner":
		return domain.ValidMemberRole(target)
	case "admin":
		return domain.ValidMemberRole(target) && target != "owner"
	}
	return false
}

// lastOwner reports whether `user` is the only owner left, which makes removal or demotion impossible.
func lastOwner(tx *gorm.DB, tenant, user string) (bool, error) {
	var n int64
	if e := tx.Raw(`SELECT count(*) FROM core.memberships WHERE tenant_id=? AND role='owner' AND user_id<>?`, tenant, user).Scan(&n).Error; e != nil {
		return false, e
	}
	return n == 0, nil
}

// setProjects replaces the project access list. An empty list means "every project of the workspace".
func setProjects(tx *gorm.DB, tenant, user string, projects []string) error {
	if e := tx.Exec(`DELETE FROM core.member_projects WHERE tenant_id=? AND user_id=?`, tenant, user).Error; e != nil {
		return e
	}
	for _, id := range projects {
		// The SELECT ... WHERE EXISTS proves the project is a live project of this tenant before linking.
		res := tx.Exec(`INSERT INTO core.member_projects(tenant_id,user_id,project_id) SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM core.projects WHERE tenant_id=? AND id=? AND archived_at IS NULL)`, tenant, user, id, tenant, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
	}
	return nil
}

// ListMembers returns every member of the workspace with their role, project access and last sign-in.
func (r *Repository) ListMembers(ctx context.Context, p domain.Principal) ([]domain.Member, error) {
	out := []domain.Member{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		// identity.users carries no tenant column, so the join direction matters: memberships (which RLS
		// restricts to this tenant for an admin) decides which identities are visible, never the reverse.
		var rows []struct {
			UserID             string
			Email              string
			Name               string
			Role               string
			MustChangePassword bool
			CreatedAt          time.Time
		}
		if e := tx.Raw(`SELECT m.user_id,u.email,u.name,m.role,m.must_change_password,m.created_at
      FROM core.memberships m JOIN identity.users u ON u.id=m.user_id
      WHERE m.tenant_id=? ORDER BY m.created_at,m.user_id LIMIT ?`, p.TenantID, domain.MaxMembers).Scan(&rows).Error; e != nil {
			return e
		}
		var links []struct{ UserID, ProjectID string }
		if e := tx.Raw(`SELECT user_id,project_id FROM core.member_projects WHERE tenant_id=? ORDER BY user_id,project_id`, p.TenantID).Scan(&links).Error; e != nil {
			return e
		}
		var seen []struct {
			UserID   string
			LastSeen *time.Time
		}
		if e := tx.Raw(`SELECT user_id,last_seen FROM identity.tenant_last_seen()`).Scan(&seen).Error; e != nil {
			return e
		}
		projects := map[string][]string{}
		for _, l := range links {
			projects[l.UserID] = append(projects[l.UserID], l.ProjectID)
		}
		last := map[string]*time.Time{}
		for _, s := range seen {
			last[s.UserID] = s.LastSeen
		}
		for _, row := range rows {
			ids := projects[row.UserID]
			if ids == nil {
				ids = []string{}
			}
			out = append(out, domain.Member{UserID: row.UserID, Email: row.Email, Name: row.Name, Role: row.Role,
				ProjectIDs: ids, MustChangePassword: row.MustChangePassword, CreatedAt: row.CreatedAt, LastSeenAt: last[row.UserID]})
		}
		return nil
	})
	return out, e
}

// AddMember creates the membership. The email is either new (a fresh identity is created) or belongs to
// an identity with no membership anywhere — somebody who was removed from a workspace earlier — which is
// adopted and given the supplied password. An identity that still belongs to ANY workspace is refused
// with the same undifferentiated conflict a duplicate member produces: silently attaching a second
// membership to a stranger's account would change where their next login lands, because login picks the
// workspace automatically when an identity has exactly one, and a distinguishable answer would turn this
// route into a platform-wide "does this email have an account?" oracle.
func (r *Repository) AddMember(ctx context.Context, p domain.Principal, in domain.NewMember) (domain.Member, error) {
	out := domain.Member{Email: in.Email, Name: in.Name, Role: in.Role, ProjectIDs: in.ProjectIDs, MustChangePassword: true}
	if out.ProjectIDs == nil {
		out.ProjectIDs = []string{}
	}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := memberLock(tx, p.TenantID); e != nil {
			return e
		}
		actor, e := roleOf(tx, p.TenantID, p.UserID)
		if e != nil {
			return e
		}
		if !mayTouch(actor, in.Role) {
			return domain.ErrForbidden
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.memberships WHERE tenant_id=?`, p.TenantID).Scan(&n).Error; e != nil {
			return e
		}
		if n >= domain.MaxMembers {
			return domain.ErrConflict
		}
		// Same path login and CreateAccount use to reach identity.users: one exact-email lookup.
		var ids []struct{ ID string }
		if e := tx.Raw(`SELECT id FROM identity.users WHERE email=?`, in.Email).Scan(&ids).Error; e != nil {
			return e
		}
		if len(ids) > 1 {
			return domain.ErrConflict
		}
		adopted := len(ids) == 1
		user := uuid.NewString()
		if adopted {
			user = ids[0].ID
			if user == p.UserID {
				return domain.ErrForbidden
			}
			// The count is over every workspace, which is why it needs the SECURITY DEFINER path.
			var elsewhere int
			if e := tx.Raw(`SELECT core.identity_membership_count(?)`, user).Scan(&elsewhere).Error; e != nil {
				return e
			}
			if elsewhere != 0 {
				return domain.ErrConflict
			}
		} else if e := tx.Exec(`INSERT INTO identity.users(id,email,name,password_hash) VALUES(?,?,?,?)`, user, in.Email, in.Name, in.PasswordHash).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.memberships(tenant_id,user_id,role,must_change_password) VALUES(?,?,?,true)`, p.TenantID, user, in.Role).Error; e != nil {
			return e
		}
		if adopted {
			// Now that the membership exists this identity has exactly one, so the guarded password path
			// accepts it: the account is re-issued with the password the owner is about to hand over.
			var ok bool
			if e := tx.Raw(`SELECT identity.set_member_password(?,?)`, user, in.PasswordHash).Scan(&ok).Error; e != nil {
				return e
			}
			if !ok {
				return domain.ErrConflict
			}
		}
		if e := setProjects(tx, p.TenantID, user, in.ProjectIDs); e != nil {
			return e
		}
		out.UserID = user
		// Read the row back: an adopted identity keeps its own stored name, not the one just supplied.
		var stored []struct {
			Email, Name string
			CreatedAt   time.Time
		}
		if e := tx.Raw(`SELECT u.email,u.name,m.created_at FROM core.memberships m JOIN identity.users u ON u.id=m.user_id
      WHERE m.tenant_id=? AND m.user_id=?`, p.TenantID, user).Scan(&stored).Error; e != nil {
			return e
		}
		if len(stored) != 1 {
			return domain.ErrConflict
		}
		out.Email, out.Name, out.CreatedAt = stored[0].Email, stored[0].Name, stored[0].CreatedAt
		return audit(tx, p, "member.added", user)
	})
	if e != nil {
		return domain.Member{}, classify(e)
	}
	return out, nil
}

// UpdateMember changes a member's role and project access. Nobody edits their own membership here.
func (r *Repository) UpdateMember(ctx context.Context, p domain.Principal, target, role string, projects []string) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if target == p.UserID {
			return domain.ErrForbidden
		}
		if e := memberLock(tx, p.TenantID); e != nil {
			return e
		}
		actor, e := roleOf(tx, p.TenantID, p.UserID)
		if e != nil {
			return e
		}
		current, e := roleOf(tx, p.TenantID, target)
		if e != nil {
			return e
		}
		if !mayTouch(actor, current) || !mayTouch(actor, role) {
			return domain.ErrForbidden
		}
		if current == "owner" && role != "owner" {
			alone, e := lastOwner(tx, p.TenantID, target)
			if e != nil {
				return e
			}
			if alone {
				return domain.ErrConflict
			}
		}
		if e := tx.Exec(`UPDATE core.memberships SET role=? WHERE tenant_id=? AND user_id=?`, role, p.TenantID, target).Error; e != nil {
			return e
		}
		if e := setProjects(tx, p.TenantID, target, projects); e != nil {
			return e
		}
		return audit(tx, p, "member.updated", target)
	}))
}

// RemoveMember deletes the membership and, in the same transaction, every session and refresh token that
// member holds in this workspace, so an access token that is still within its 15 minutes stops working.
func (r *Repository) RemoveMember(ctx context.Context, p domain.Principal, target string) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if target == p.UserID {
			return domain.ErrForbidden
		}
		if e := memberLock(tx, p.TenantID); e != nil {
			return e
		}
		actor, e := roleOf(tx, p.TenantID, p.UserID)
		if e != nil {
			return e
		}
		current, e := roleOf(tx, p.TenantID, target)
		if e != nil {
			return e
		}
		if !mayTouch(actor, current) {
			return domain.ErrForbidden
		}
		if current == "owner" {
			alone, e := lastOwner(tx, p.TenantID, target)
			if e != nil {
				return e
			}
			if alone {
				return domain.ErrConflict
			}
		}
		// identity.sessions has a foreign key to the membership row, so the sessions go first.
		var gone int
		if e := tx.Raw(`SELECT identity.purge_tenant_sessions(?)`, target).Scan(&gone).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.member_projects WHERE tenant_id=? AND user_id=?`, p.TenantID, target).Error; e != nil {
			return e
		}
		res := tx.Exec(`DELETE FROM core.memberships WHERE tenant_id=? AND user_id=?`, p.TenantID, target)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "member.removed", target)
	}))
}

// ResetMemberPassword replaces the password of a member whose identity belongs to this workspace only.
// ErrConflict means the identity is shared with another organisation and is not ours to rewrite.
func (r *Repository) ResetMemberPassword(ctx context.Context, p domain.Principal, target, hash string) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if target == p.UserID {
			return domain.ErrForbidden
		}
		if e := memberLock(tx, p.TenantID); e != nil {
			return e
		}
		actor, e := roleOf(tx, p.TenantID, p.UserID)
		if e != nil {
			return e
		}
		current, e := roleOf(tx, p.TenantID, target)
		if e != nil {
			return e
		}
		if !mayTouch(actor, current) {
			return domain.ErrForbidden
		}
		// roleOf already proved the target is a member here; anything above one means another workspace.
		var tenants int
		if e := tx.Raw(`SELECT core.identity_membership_count(?)`, target).Scan(&tenants).Error; e != nil {
			return e
		}
		if tenants != 1 {
			return domain.ErrConflict
		}
		// The function repeats the "single workspace" check as the authority; false means refused.
		var ok bool
		if e := tx.Raw(`SELECT identity.set_member_password(?,?)`, target, hash).Scan(&ok).Error; e != nil {
			return e
		}
		if !ok {
			return domain.ErrConflict
		}
		var gone int
		if e := tx.Raw(`SELECT identity.purge_tenant_sessions(?)`, target).Scan(&gone).Error; e != nil {
			return e
		}
		return audit(tx, p, "member.password_reset", target)
	}))
}

// ChangeOwnPassword verifies the caller's current password with `check` (Argon2id lives in the service,
// never in SQL), stores the new hash, clears must_change_password and ends every other session of that
// identity — in any workspace, because the password is global to the identity.
func (r *Repository) ChangeOwnPassword(ctx context.Context, p domain.Principal, check func(currentHash string) bool, hash string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []struct{ PasswordHash string }
		if e := tx.Raw(`SELECT password_hash FROM identity.users WHERE id=?`, p.UserID).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 || !check(rows[0].PasswordHash) {
			return domain.ErrUnauthorized
		}
		var ok bool
		if e := tx.Raw(`SELECT identity.change_own_password(?)`, hash).Scan(&ok).Error; e != nil {
			return e
		}
		if !ok {
			return domain.ErrUnauthorized
		}
		if e := tx.Exec(`UPDATE identity.sessions SET revoked_at=now() WHERE user_id=? AND id<>? AND revoked_at IS NULL`, p.UserID, p.SessionID).Error; e != nil {
			return e
		}
		return audit(tx, p, "account.password_changed", p.UserID)
	})
}

// MemberSelf answers "who am I and what may I see": the role, the project scope the database enforces
// (nil when the member sees everything) and whether the initial password still has to be replaced.
func (r *Repository) MemberSelf(ctx context.Context, p domain.Principal) (domain.MemberSelf, error) {
	out := domain.MemberSelf{UserID: p.UserID, TenantID: p.TenantID, Role: p.Role}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var rows []struct {
			Email              string
			Name               string
			Role               string
			MustChangePassword bool
		}
		if e := tx.Raw(`SELECT u.email,u.name,m.role,m.must_change_password FROM core.memberships m
      JOIN identity.users u ON u.id=m.user_id WHERE m.tenant_id=? AND m.user_id=?`, p.TenantID, p.UserID).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) != 1 {
			return domain.ErrUnauthorized
		}
		out.Email, out.Name, out.Role, out.MustChangePassword = rows[0].Email, rows[0].Name, rows[0].Role, rows[0].MustChangePassword
		if out.Role == "owner" {
			return nil
		}
		var ids []struct{ ProjectID string }
		if e := tx.Raw(`SELECT project_id FROM core.member_projects WHERE tenant_id=? AND user_id=? ORDER BY project_id`, p.TenantID, p.UserID).Scan(&ids).Error; e != nil {
			return e
		}
		for _, row := range ids {
			out.ProjectIDs = append(out.ProjectIDs, row.ProjectID)
		}
		return nil
	})
	return out, e
}
