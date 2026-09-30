package main

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestScramVerifierShape(t *testing.T) {
	a, e := scramVerifier(strings.Repeat("x", 40))
	if e != nil {
		t.Fatal(e)
	}
	b, _ := scramVerifier(strings.Repeat("x", 40))
	if !strings.HasPrefix(a, "SCRAM-SHA-256$4096:") || a == b || strings.ContainsAny(a, "'\\\\") {
		t.Fatalf("verifier %q / %q", a, b)
	}
	for _, bad := range []string{"short", strings.Repeat("a", 31), strings.Repeat("a", 40) + "'", strings.Repeat("é", 40)} {
		if applyAuthPassword(nil, bad) == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if applyAuthPassword(nil, "") != nil {
		t.Fatal("unset must leave the role alone")
	}
}

// The verifier Go builds is one PostgreSQL accepts: a throwaway role gets it and then logs in with the password.
func TestScramVerifierLogsIn(t *testing.T) {
	admin := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if admin == "" {
		t.Skip("requires TEST_ADMIN_DATABASE_URL")
	}
	db, e := sql.Open("pgx", admin)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	role := "aether_scram_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, e := db.Exec(`CREATE ROLE ` + role + ` NOLOGIN`); e != nil {
		t.Fatal(e)
	}
	defer db.Exec(`DROP ROLE IF EXISTS ` + role)
	password := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	if e := setLoginPassword(db, role, password); e != nil {
		t.Fatal(e)
	}
	if setLoginPassword(db, "x; DROP ROLE postgres", password) == nil {
		t.Fatal("role name not validated")
	}
	u, _ := url.Parse(admin)
	for _, pw := range []string{password, password + "x"} {
		u.User = url.UserPassword(role, pw)
		c, _ := sql.Open("pgx", u.String())
		e := c.Ping()
		c.Close()
		if (pw == password) != (e == nil) {
			t.Fatalf("login with %s password: %v", map[bool]string{true: "right", false: "wrong"}[pw == password], e)
		}
	}
}

// Bind parameters (new password hashes, emails) of the runtime and login roles never reach the server log.
func TestLogRedactionIsSetOnTheRoles(t *testing.T) {
	admin := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if admin == "" {
		t.Skip("requires TEST_ADMIN_DATABASE_URL")
	}
	db, e := sql.Open("pgx", admin)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	for _, role := range loggedRoles {
		if _, e := db.Exec(`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='` + role + `') THEN CREATE ROLE ` + role + ` NOLOGIN; END IF; END $$`); e != nil {
			t.Fatal(e)
		}
		// Drift: a setting reset by hand comes back on the next migrate.
		if _, e := db.Exec(`ALTER ROLE ` + role + ` RESET log_parameter_max_length`); e != nil {
			t.Fatal(e)
		}
	}
	if e := applyLogRedaction(db); e != nil {
		t.Fatal(e)
	}
	for _, role := range loggedRoles {
		var settings []byte
		if e := db.QueryRow(`SELECT array_to_string(s.setconfig, ',') FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname=$1 AND s.setdatabase=0`, role).Scan(&settings); e != nil {
			t.Fatalf("%s: %v", role, e)
		}
		for _, want := range []string{"log_parameter_max_length=0", "log_parameter_max_length_on_error=0"} {
			if !strings.Contains(","+string(settings)+",", ","+want+",") {
				t.Fatalf("%s lacks %s: %s", role, want, settings)
			}
		}
	}
	// The roles cannot switch it back on in their own sessions: the parameter is superuser-only.
	var context string
	if e := db.QueryRow(`SELECT context FROM pg_settings WHERE name='log_parameter_max_length'`).Scan(&context); e != nil || context != "superuser" {
		t.Fatalf("log_parameter_max_length context %q: %v", context, e)
	}
}
