package tests

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The local admin command on a server that already has accounts: bootstrap refuses and names create-workspace,
// which adds a separate workspace whose new owner can log in and owns only that workspace. An email that already
// has an account and a too-short password are refused with their reason.
func TestProvisionFurtherWorkspace(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.account(t) // an existing workspace: bootstrap is spent
	const password = "correct-horse-battery"
	email := func() string { return "owner-" + uuid.NewString()[:8] + "@example.test" }

	if _, e := app.ProvisionOwner(ctx, f.repo, false, email(), password, "Owner X", "Workspace X"); !errors.Is(e, domain.ErrConflict) || !strings.Contains(e.Error(), "create-workspace") {
		t.Fatalf("bootstrap with accounts present: %v", e)
	}
	a, e := app.ProvisionOwner(ctx, f.repo, true, email(), password, "Distar Admin", "Distar")
	if e != nil {
		t.Fatal(e)
	}
	var memberships []string
	rows, e := f.admin.QueryContext(ctx, `SELECT t.name||':'||m.role FROM core.memberships m JOIN core.tenants t ON t.id=m.tenant_id WHERE m.user_id=$1`, a.User.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if e := rows.Scan(&s); e != nil {
			t.Fatal(e)
		}
		memberships = append(memberships, s)
	}
	if len(memberships) != 1 || memberships[0] != "Distar:owner" {
		t.Fatalf("memberships: %v", memberships)
	}
	auth, e := f.service.Login(ctx, a.User.Email, password, "")
	if e != nil || auth.TenantID != a.TenantID {
		t.Fatalf("login: %v %+v", e, auth)
	}

	if _, e := app.ProvisionOwner(ctx, f.repo, true, a.User.Email, password, "Again", "Workspace B"); !errors.Is(e, domain.ErrConflict) || !strings.Contains(e.Error(), "already exists") {
		t.Fatalf("existing email: %v", e)
	}
	if _, e := app.ProvisionOwner(ctx, f.repo, true, email(), "short", "Owner C", "Workspace C"); !errors.Is(e, domain.ErrInvalid) || !strings.Contains(e.Error(), "12-128 bytes") {
		t.Fatalf("short password: %v", e)
	}
}
