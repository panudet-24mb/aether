package domain

import "time"

// MemberRoles are the roles core.memberships.role accepts, strongest first.
// owner  – everything, including members and billing-level settings
// admin  – manage gateways, devices, projects and members (never owners)
// operator – acknowledge and resolve alerts
// viewer – read only
var MemberRoles = []string{"owner", "admin", "operator", "viewer"}

// MaxMembers caps one workspace. The members list and the audit trail stay readable, and an accidental
// import cannot turn a workspace into a directory.
const MaxMembers = 200

// CanManageMembers is owner/admin. It is deliberately the same set as CanManageDevices: the extra rules
// (an admin may not touch an owner, nobody may touch their own membership) are checked per request.
func (p Principal) CanManageMembers() bool { return p.Role == "owner" || p.Role == "admin" }

// Member is one person in the workspace. Password hashes are never part of this type.
type Member struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	// ProjectIDs is empty for a member who sees every project.
	ProjectIDs         []string   `json:"project_ids"`
	MustChangePassword bool       `json:"must_change_password"`
	CreatedAt          time.Time  `json:"created_at"`
	LastSeenAt         *time.Time `json:"last_seen_at"`
}

// NewMember is a request to add somebody. PasswordHash is ignored when the email already has an identity.
type NewMember struct {
	Email        string
	Name         string
	Role         string
	PasswordHash string
	ProjectIDs   []string
}

// MemberSelf answers GET /api/v1/me: who am I, what may I do, and what may I see.
type MemberSelf struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	// ProjectIDs is nil (JSON null) when the member sees every project.
	ProjectIDs         []string `json:"project_ids"`
	MustChangePassword bool     `json:"must_change_password"`
}

// ValidMemberRole reports whether role is one of MemberRoles.
func ValidMemberRole(role string) bool {
	for _, r := range MemberRoles {
		if r == role {
			return true
		}
	}
	return false
}
