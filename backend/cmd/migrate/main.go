package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strconv"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func main() {
	dsn := os.Getenv("MIGRATION_DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "MIGRATION_DATABASE_URL required; must never be supplied to the API process")
		os.Exit(1)
	}
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		os.Exit(1)
	}
	defer db.Close()
	if e = goose.SetDialect("postgres"); e == nil {
		e = goose.Up(db, "migrations")
	}
	if e == nil {
		e = applyAccessLogRetention(db, os.Getenv("ACCESS_LOG_RETENTION_DAYS"))
	}
	if e == nil {
		e = applyAuthPassword(db, os.Getenv("AUTH_DB_PASSWORD"))
	}
	if e == nil {
		e = applyLogRedaction(db)
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", e)
		os.Exit(1)
	}
}

// applyAccessLogRetention sets how long the read-access log is kept (core.retention_policy, migration 00038). Only
// this role may: the API cannot shorten the trail it writes. Unset leaves the stored value (default 400 days).
func applyAccessLogRetention(db *sql.DB, raw string) error {
	if raw == "" {
		return nil
	}
	days, e := strconv.Atoi(raw)
	if e != nil || days < 30 || days > 3650 {
		return fmt.Errorf("ACCESS_LOG_RETENTION_DAYS must be 30..3650")
	}
	var stored int
	if e := db.QueryRow(`SELECT core.set_access_log_retention($1)`, days).Scan(&stored); e != nil {
		return e
	}
	fmt.Printf("access log retention: %d days\n", stored)
	return nil
}

// authPassword is what setup.py and generate-env.py produce (hex or URL-safe base64); anything else is refused
// rather than risk SASLprep differences between Go and PostgreSQL.
var authPassword = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)

// applyAuthPassword lets aether_auth (migration 00040) log in with AUTH_DB_PASSWORD. The role is cluster-wide and a
// migration cannot know the secret, so this runs on every migrate, which also makes it the rotation path. The server
// receives a SCRAM-SHA-256 verifier, never the password. Unset leaves the role as it is.
func applyAuthPassword(db *sql.DB, password string) error {
	if password == "" {
		return nil
	}
	if !authPassword.MatchString(password) {
		return fmt.Errorf("AUTH_DB_PASSWORD must be 32-128 characters of A-Z a-z 0-9 _ -")
	}
	if e := setLoginPassword(db, "aether_auth", password); e != nil {
		return fmt.Errorf("setting the aether_auth password: %w", e)
	}
	fmt.Println("auth role: login enabled")
	return nil
}

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// setLoginPassword enables LOGIN on role with a SCRAM verifier of password. The verifier is base64 plus '$' and ':'
// and the role a plain identifier, so both can be spliced; ALTER ROLE takes no bind parameters.
func setLoginPassword(db *sql.DB, role, password string) error {
	if !roleName.MatchString(role) {
		return fmt.Errorf("invalid role name")
	}
	verifier, e := scramVerifier(password)
	if e != nil {
		return e
	}
	_, e = db.Exec(`ALTER ROLE ` + role + ` WITH LOGIN PASSWORD '` + verifier + `'`)
	return e
}

// scramVerifier builds the SCRAM-SHA-256 secret PostgreSQL stores (RFC 5802/7677, 4096 iterations, as
// password_encryption=scram-sha-256 does): SCRAM-SHA-256$<iter>:<salt>$<StoredKey>:<ServerKey>.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return "", e
	}
	const iterations = 4096
	salted, e := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if e != nil {
		return "", e
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(mac(salted, "Server Key"))), nil
}

// loggedRoles write or read password hashes (new hashes as bind parameters of CreateAccount, member creation, resets
// and changes; the login lookup's email). Their bind parameters are never written to the server log, whatever
// log_statement or log_min_duration_statement say. log_parameter_max_length is superuser-only, so the roles cannot
// turn it back on; this runs as the migration (superuser) role on every migrate, so drift is corrected too.
var loggedRoles = []string{"aether_app", "aether_auth"}

func applyLogRedaction(db *sql.DB) error {
	for _, role := range loggedRoles {
		for _, setting := range []string{"log_parameter_max_length", "log_parameter_max_length_on_error"} {
			if _, e := db.Exec(`ALTER ROLE ` + role + ` SET ` + setting + ` = 0`); e != nil {
				return fmt.Errorf("turning off parameter logging for %s: %w", role, e)
			}
		}
	}
	return nil
}
