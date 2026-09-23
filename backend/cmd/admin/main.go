// Local, one-time account provisioning. No HTTP bootstrap or permanent bootstrap secret.
package main

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/app"
	"aether/backend/internal/security"
	"context"
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
func run() error {
	if len(os.Args) != 2 || os.Args[1] != "bootstrap" {
		return fmt.Errorf("usage: admin bootstrap (set DATABASE_URL, ADMIN_EMAIL, ADMIN_NAME, TENANT_NAME, ADMIN_PASSWORD_FILE)")
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
	s, e := app.New(repo, security.NewTokens([]byte(security.RandomToken()), "aether"), false)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, e := s.Register(ctx, os.Getenv("ADMIN_EMAIL"), strings.TrimRight(string(password), "\r\n"), os.Getenv("ADMIN_NAME"), os.Getenv("TENANT_NAME"), true)
	clear(password)
	if e != nil {
		return e
	}
	fmt.Printf("Owner created. user_id=%s tenant_id=%s\n", a.User.ID, a.TenantID)
	return nil
}
