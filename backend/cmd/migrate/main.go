package main

import (
	"database/sql"
	"fmt"
	"os"
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
