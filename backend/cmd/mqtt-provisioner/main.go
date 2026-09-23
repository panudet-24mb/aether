// mqtt-provisioner is the sole writer of runtime password/ACL files. It shares
// the broker PID namespace, not a Docker socket, and can only read hashes/ack revisions.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type account struct {
	ID, Hash string
	Revision int
}

func atomic(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".mqtt-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Chmod(0400); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}

func main() {
	db, e := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if e != nil {
		panic("database configuration")
	}
	defer db.Close()
	var last [32]byte
	for {
		if e = syncAccounts(db, &last); e != nil {
			slog.Error("MQTT provisioning failed; retrying")
		}
		time.Sleep(3 * time.Second)
	}
}
func syncAccounts(db *sql.DB, last *[32]byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, e := db.QueryContext(ctx, `SELECT gateway_id,password_hash,revision FROM core.mqtt_provisioning_accounts()`)
	if e != nil {
		return e
	}
	accounts := []account{}
	for rows.Next() {
		var a account
		if e = rows.Scan(&a.ID, &a.Hash, &a.Revision); e != nil {
			rows.Close()
			return e
		}
		accounts = append(accounts, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	passwords, e := os.ReadFile("/run/mqtt-base/passwords")
	if e != nil {
		return e
	}
	acl, e := os.ReadFile("/run/mqtt-base/acl")
	if e != nil {
		return e
	}
	p := strings.TrimSpace(string(passwords)) + "\n"
	a := strings.TrimSpace(string(acl)) + "\n\nuser aether-ingest\ntopic read /aether/gateways/+/status\n"
	for _, c := range accounts {
		p += "gw-" + c.ID + ":" + c.Hash + "\n"
		root := "/aether/gateways/" + c.ID
		a += "\nuser gw-" + c.ID + "\ntopic write " + root + "/status\ntopic write " + root + "/response\ntopic read " + root + "/action\n"
	}
	digest := sha256.Sum256([]byte(p + "\x00" + a))
	if digest != *last {
		dir := "/run/mqtt-runtime"
		if e = atomic(filepath.Join(dir, "passwords"), []byte(p)); e != nil {
			return e
		}
		if e = atomic(filepath.Join(dir, "acl"), []byte(a)); e != nil {
			return e
		}
		if e = syscall.Kill(1, syscall.SIGHUP); e != nil {
			return e
		}
		*last = digest
		slog.Info("MQTT account files synchronized", "accounts", len(accounts))
	}
	for _, c := range accounts {
		if _, e = db.ExecContext(ctx, `SELECT core.mqtt_provisioning_ack($1,$2)`, c.ID, c.Revision); e != nil {
			return fmt.Errorf("ack failed")
		}
	}
	return nil
}
