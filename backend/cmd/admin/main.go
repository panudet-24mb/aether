// Local account provisioning: the one-time first owner, and further workspaces with their own owner. No HTTP
// bootstrap or permanent bootstrap secret; whoever runs this already holds the database credentials.
package main

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/ports"
	"aether/backend/internal/security"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

const usage = `usage:
  admin bootstrap           one-time owner (DATABASE_URL, ADMIN_EMAIL, ADMIN_NAME, TENANT_NAME, ADMIN_PASSWORD_FILE)
  admin create-workspace    another workspace and its owner, a new email (same variables as bootstrap)
  admin export-erasures     the erasure ledger as JSON lines on stdout (MIGRATION_DATABASE_URL)
  admin reapply-erasures    re-applies a ledger read from stdin after a restore (MIGRATION_DATABASE_URL)
  admin erase-user [--force] <uuid>   erases an identity in every workspace it belongs to; --force also when it is
                            a workspace's last owner (MIGRATION_DATABASE_URL)`

func run() error {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "export-erasures", "reapply-erasures", "erase-user":
			return privacyCommand(os.Args[1], os.Args[2:])
		}
	}
	if len(os.Args) != 2 || (os.Args[1] != "bootstrap" && os.Args[1] != "create-workspace") {
		return fmt.Errorf("%s", usage)
	}
	path := os.Getenv("ADMIN_PASSWORD_FILE")
	if path == "" {
		return fmt.Errorf("ADMIN_PASSWORD_FILE required; never pass the password on the command line")
	}
	info, e := os.Stat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("password file must be a regular file accessible only to its owner (0600)")
	}
	password, e := os.ReadFile(path)
	if e != nil {
		return fmt.Errorf("cannot read password file")
	}
	repo, e := postgres.Open(os.Getenv("DATABASE_URL"))
	if e != nil {
		return e
	}
	defer repo.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, e := provision(ctx, repo, os.Args[1], os.Getenv("ADMIN_EMAIL"), strings.TrimRight(string(password), "\r\n"), os.Getenv("ADMIN_NAME"), os.Getenv("TENANT_NAME"))
	clear(password)
	if e != nil {
		return e
	}
	fmt.Printf("Owner created. user_id=%s tenant_id=%s\n", a.User.ID, a.TenantID)
	return nil
}

// provision creates a workspace with its owner. bootstrap refuses once any account exists. create-workspace makes
// the same account for another workspace; the registration switch it sets lives only in this local process, the
// API's public registration stays as configured. An email that already has an account is refused.
func provision(ctx context.Context, repo ports.Repository, command, email, password, name, tenant string) (domain.Account, error) {
	create := command == "create-workspace"
	s, e := app.New(repo, security.NewTokens([]byte(security.RandomToken()), "aether"), create)
	if e != nil {
		return domain.Account{}, e
	}
	a, e := s.Register(ctx, email, password, name, tenant, !create)
	switch {
	case errors.Is(e, domain.ErrConflict) && create:
		return a, fmt.Errorf("an account with this email already exists: %w", e)
	case errors.Is(e, domain.ErrConflict):
		return a, fmt.Errorf("accounts already exist and bootstrap is one-time; use create-workspace for another workspace: %w", e)
	case errors.Is(e, domain.ErrInvalid):
		return a, fmt.Errorf("invalid email, name or workspace name, or a password outside 12-128 characters: %w", e)
	}
	return a, e
}
