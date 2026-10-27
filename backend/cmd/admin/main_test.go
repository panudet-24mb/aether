package main

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/domain"
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Once accounts exist, bootstrap refuses and says what to use instead; create-workspace adds a separate workspace
// whose owner can log in, and refuses an email that already has an account.
func TestCreateWorkspace(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" || os.Getenv("TEST_ADMIN_DATABASE_URL") == "" {
		t.Skip("requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL")
	}
	repo, e := postgres.Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer repo.Close()
	ctx := context.Background()
	const password = "correct-horse-battery"
	email := func() string { return "owner-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "@example.test" }

	first, e := provision(ctx, repo, "create-workspace", email(), password, "Owner A", "Workspace A")
	if e != nil {
		t.Fatal(e)
	}
	// An account exists now, so bootstrap is spent.
	if _, e := provision(ctx, repo, "bootstrap", email(), password, "Owner X", "Workspace X"); !errors.Is(e, domain.ErrConflict) || !strings.Contains(e.Error(), "create-workspace") {
		t.Fatalf("bootstrap with accounts present: %v", e)
	}
	second, e := provision(ctx, repo, "create-workspace", email(), password, "Distar Admin", "Distar")
	if e != nil {
		t.Fatal(e)
	}
	if second.TenantID == first.TenantID || second.User.ID == first.User.ID {
		t.Fatalf("not a separate workspace: %+v %+v", first, second)
	}
	if _, e := provision(ctx, repo, "create-workspace", first.User.Email, password, "Again", "Workspace B"); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("existing email: %v", e)
	}
	if _, e := provision(ctx, repo, "create-workspace", email(), "short", "Owner C", "Workspace C"); e == nil {
		t.Fatal("short password accepted")
	}
	// The new user owns the new workspace, and only that one.
	admin, e := sql.Open("pgx", os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	var tenants []string
	rows, e := admin.QueryContext(ctx, `SELECT m.tenant_id::text||':'||m.role||':'||t.name FROM core.memberships m JOIN core.tenants t ON t.id=m.tenant_id WHERE m.user_id=$1`, second.User.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if e := rows.Scan(&s); e != nil {
			t.Fatal(e)
		}
		tenants = append(tenants, s)
	}
	if len(tenants) != 1 || tenants[0] != second.TenantID+":owner:Distar" {
		t.Fatalf("memberships: %v", tenants)
	}
}
