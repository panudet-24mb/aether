package tests

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/domain"
)

// Migration 00040: password hashes reach Go only through the login pool (role aether_auth). The runtime role can no
// longer call the hash-returning functions, and the login role can call nothing else.
func TestLoginPoolIsTheOnlyPathToHashes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, _, p := f.account(t)

	// The runtime role: both lookups refused.
	tx := asRuntime(t, f, p.UserID, p.TenantID)
	var hash string
	if e := tx.QueryRow(`SELECT password_hash FROM identity.login_candidate($1)`, a.User.Email).Scan(&hash); sqlState(e) != "42501" {
		t.Fatalf("aether_app ran login_candidate: %v", e)
	}
	tx.Rollback()

	// The login role: the one function, and nothing else.
	auth, e := sql.Open("pgx", f.authDSN)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { auth.Close() })
	auth.SetMaxOpenConns(1)
	var id string
	if e := auth.QueryRow(`SELECT id,password_hash FROM identity.login_candidate($1)`, a.User.Email).Scan(&id, &hash); e != nil || id != p.UserID || hash == "" {
		t.Fatalf("login_candidate through the login pool: %v", e)
	}
	for name, query := range map[string]string{
		"identity table":    `SELECT count(*) FROM identity.users`,
		"sessions":          `SELECT count(*) FROM identity.sessions`,
		"core table":        `SELECT count(*) FROM core.tenants`,
		"core helper":       `SELECT core.tenant_id()`,
		"runtime definer":   `SELECT identity.own_password_is(sha256('x'))`,
		"member creation":   `SELECT * FROM identity.create_member_identity(gen_random_uuid(),'x@example.test','x','x','viewer')`,
		"identity check":    `SELECT identity.any_user_exists()`,
		"password override": `SELECT identity.change_own_password('x')`,
	} {
		var out any
		if e := auth.QueryRow(query).Scan(&out); sqlState(e) != "42501" {
			t.Fatalf("the login role ran %s: %v", name, e)
		}
	}
	if e := postgres.CheckAuthRole(ctx, auth); e != nil {
		t.Fatalf("the migrated login role was refused: %v", e)
	}

	// Login, refresh and change-password work through it; the old password stops working.
	session, e := f.service.Login(ctx, a.User.Email, "correct horse battery staple", a.TenantID)
	if e != nil {
		t.Fatalf("login: %v", e)
	}
	if _, e := f.service.Refresh(ctx, session.RefreshToken); e != nil {
		t.Fatalf("refresh: %v", e)
	}
	if e := f.service.ChangePassword(ctx, p, "wrong password here", "a brand new passphrase"); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("change with a wrong current password: %v", e)
	}
	if e := f.service.ChangePassword(ctx, p, "correct horse battery staple", "a brand new passphrase"); e != nil {
		t.Fatalf("change password: %v", e)
	}
	if _, e := f.service.Login(ctx, a.User.Email, "correct horse battery staple", a.TenantID); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatalf("the old password still logs in: %v", e)
	}
	if _, e := f.service.Login(ctx, a.User.Email, "a brand new passphrase", a.TenantID); e != nil {
		t.Fatalf("the new password: %v", e)
	}

	// Without the login pool nobody logs in, and nothing falls back to the runtime role.
	bare, e := postgres.Open(os.Getenv("TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer bare.Close()
	if _, e := bare.UserByEmail(ctx, a.User.Email); !errors.Is(e, domain.ErrUnavailable) {
		t.Fatalf("login without the auth pool: %v", e)
	}
	if e := bare.ChangeOwnPassword(ctx, p, func(string) bool { return true }, "x"); !errors.Is(e, domain.ErrUnavailable) {
		t.Fatalf("change password without the auth pool: %v", e)
	}
	// And the runtime role is not a login role.
	if e := bare.AttachAuth(os.Getenv("TEST_DATABASE_URL")); e == nil {
		t.Fatal("the runtime role was accepted as the login pool")
	}
}

// own_password_is takes the sha256 of a hash, never the hash, and keeps the identity row locked until the caller
// commits, so a reset or change of that password cannot slip between the check and the session (or change) it guards.
func TestOwnPasswordIsHoldsTheIdentity(t *testing.T) {
	f := setup(t)
	_, _, p := f.account(t)
	var stored string
	if e := f.admin.QueryRow(`SELECT password_hash FROM identity.users WHERE id=$1`, p.UserID).Scan(&stored); e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256([]byte(stored))
	tx := asRuntime(t, f, p.UserID, p.TenantID)
	var same bool
	// The hash itself, or a digest of the wrong length, is never accepted.
	for name, arg := range map[string]any{"the hash as text": stored, "31 bytes": digest[:31], "33 bytes": append(digest[:], 0)} {
		if _, e := tx.Exec(`SAVEPOINT arg`); e != nil {
			t.Fatal(e)
		}
		if e := tx.QueryRow(`SELECT identity.own_password_is($1)`, arg).Scan(&same); e == nil && same {
			t.Fatalf("own_password_is accepted %s", name)
		}
		if _, e := tx.Exec(`ROLLBACK TO SAVEPOINT arg`); e != nil {
			t.Fatal(e)
		}
	}
	if e := tx.QueryRow(`SELECT identity.own_password_is($1)`, digest[:]).Scan(&same); e != nil || !same {
		t.Fatalf("own_password_is: %v %v", same, e)
	}
	probe, e := f.admin.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer probe.Rollback()
	if _, e := probe.Exec(`SET LOCAL lock_timeout='300ms'`); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	if _, e := probe.Exec(`UPDATE identity.users SET password_hash=password_hash WHERE id=$1`, p.UserID); sqlState(e) != "55P03" {
		t.Fatalf("a password write did not wait for the check (%v after %s)", e, time.Since(start))
	}
}

// The start-up check of the login pool refuses a role that can do more than its one function. Each case is staged
// in a rolled-back admin transaction under SET LOCAL ROLE aether_auth (role attributes and grants are transactional).
func TestAuthRoleCheck(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for name, stage := range map[string]string{
		"sound":           ``,
		"core table":      `GRANT SELECT ON core.tenants TO aether_auth`,
		"identity column": `GRANT SELECT(email) ON identity.users TO aether_auth`,
		"session writes":  `GRANT INSERT ON identity.sessions TO aether_auth`,
		"core schema":     `GRANT USAGE ON SCHEMA core TO aether_auth`,
		"other definer":   `GRANT EXECUTE ON FUNCTION identity.any_user_exists() TO aether_auth`,
		"runtime member":  `GRANT aether_app TO aether_auth`,
		"any membership":  `CREATE ROLE aether_auth_check_group NOLOGIN; GRANT aether_auth_check_group TO aether_auth`,
		"replication":     `ALTER ROLE aether_auth REPLICATION`,
		"create role":     `ALTER ROLE aether_auth CREATEROLE`,
		"no login lookup": `REVOKE EXECUTE ON FUNCTION identity.login_candidate(text) FROM aether_auth`,
	} {
		tx, e := f.admin.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if stage != "" {
			if _, e := tx.Exec(stage); e != nil {
				tx.Rollback()
				t.Fatalf("%s: %v", name, e)
			}
		}
		if _, e := tx.Exec(`SET LOCAL ROLE aether_auth`); e != nil {
			tx.Rollback()
			t.Fatal(e)
		}
		e = postgres.CheckAuthRole(ctx, tx)
		tx.Rollback()
		if name == "sound" && e != nil {
			t.Fatalf("the migrated login role was refused: %v", e)
		}
		if name != "sound" && e == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
