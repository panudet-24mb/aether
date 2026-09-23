package main

import (
	"database/sql"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"os"
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
	if e != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", e)
		os.Exit(1)
	}
}
