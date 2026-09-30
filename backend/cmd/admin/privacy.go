package main

// Platform-operator privacy commands (internal/privacyops), run inside the migrate container:
// docker compose run --rm -T --entrypoint /app/admin migrate <command>.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"aether/backend/internal/privacyops"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func privacyCommand(command string, args []string) error {
	dsn := os.Getenv("MIGRATION_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("MIGRATION_DATABASE_URL required (the migration role; never the API's)")
	}
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		return e
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	switch command {
	case "export-erasures":
		return privacyops.ExportErasures(ctx, db, os.Stdout)
	case "reapply-erasures":
		return privacyops.ReapplyErasures(ctx, db, os.Stdin, os.Stdout)
	default:
		force := false
		rest := []string{}
		for _, a := range args {
			if a == "--force" {
				force = true
			} else {
				rest = append(rest, a)
			}
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: admin erase-user [--force] <user uuid>")
		}
		return privacyops.EraseUser(ctx, db, rest[0], force, os.Stdout)
	}
}
