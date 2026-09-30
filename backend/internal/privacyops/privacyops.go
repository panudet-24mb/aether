// Package privacyops holds the platform operator's privacy commands (cmd/admin): the erasure ledger across a
// restore, and erasing an identity that several workspaces share. They run as the migration role (a superuser): the
// ledger spans every workspace and the data functions of migration 00039 are not granted to the API role.
package privacyops

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// LedgerEntry is one core.erasure_log row as the ledger file carries it.
type LedgerEntry struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	At          time.Time       `json:"at"`
	ActorID     *string         `json:"actor_id"`
	SubjectKind string          `json:"subject_kind"`
	SubjectRef  string          `json:"subject_ref"`
	Counts      json.RawMessage `json:"counts"`
	Scope       json.RawMessage `json:"scope"`
}

// identityScope is where a tag erasure applied: the project of the registration (null: gateways with no project),
// or the whole workspace.
type identityScope struct {
	ProjectID  *string `json:"project_id"`
	TenantWide *bool   `json:"tenant_wide"`
	// DeviceID and Renamed: the owner renamed that registration with the erasure; a restore brings the old
	// (the wearer's) name back, so reapply gives it the placeholder name.
	DeviceID *string `json:"device_id"`
	Renamed  bool    `json:"renamed"`
}

// erasedName is the placeholder name migration 00039 gives what named a wearer.
const erasedName = "ผู้สวมใส่ (ลบข้อมูลแล้ว)"

// ErrLastOwner is an erasure that would leave a workspace without an owner.
var ErrLastOwner = errors.New("the identity is the last owner of a workspace")

// ExportErasures writes the whole erasure ledger as JSON lines, oldest first.
func ExportErasures(ctx context.Context, db *sql.DB, w io.Writer) error {
	rows, e := db.QueryContext(ctx, `SELECT row_to_json(t)::text FROM (SELECT id,tenant_id,at,actor_id,subject_kind,subject_ref,counts,scope
    FROM core.erasure_log ORDER BY at,id) t`)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := bufio.NewWriter(w)
	for rows.Next() {
		var line string
		if e := rows.Scan(&line); e != nil {
			return e
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if e := rows.Err(); e != nil {
		return e
	}
	return out.Flush()
}

// lockAndCheckOwner takes the workspace's member lock (the key memberLock uses) and reports whether erasing user
// would remove the workspace's last owner.
func lockAndCheckOwner(ctx context.Context, tx *sql.Tx, tenant, user string) (bool, error) {
	if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,4))`, tenant); e != nil {
		return false, e
	}
	var last bool
	e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core.memberships WHERE tenant_id=$1 AND user_id=$2 AND role='owner')
    AND (SELECT count(*) FROM core.memberships WHERE tenant_id=$1 AND role='owner') <= 1`, tenant, user).Scan(&last)
	return last, e
}

// ReapplyErasures runs every erasure of the ledger again (they are idempotent) and puts back the ledger rows a
// restore lost. An entry whose workspace is not in the database, or whose member is now that workspace's last owner,
// is reported and skipped.
func ReapplyErasures(ctx context.Context, db *sql.DB, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	applied, skipped := 0, 0
	for line := 1; scanner.Scan(); line++ {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var entry LedgerEntry
		if e := json.Unmarshal(scanner.Bytes(), &entry); e != nil {
			return fmt.Errorf("line %d: %w", line, e)
		}
		if _, e := uuid.Parse(entry.TenantID); e != nil {
			return fmt.Errorf("line %d: bad tenant id", line)
		}
		if _, e := uuid.Parse(entry.ID); e != nil {
			return fmt.Errorf("line %d: bad id", line)
		}
		var exists bool
		if e := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core.tenants WHERE id=$1)`, entry.TenantID).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			fmt.Fprintf(w, "skipped line %d: workspace %s is not in this database\n", line, entry.TenantID)
			skipped++
			continue
		}
		counts, skip, e := reapply(ctx, db, entry)
		if e != nil {
			return fmt.Errorf("line %d: %w", line, e)
		}
		if skip != "" {
			fmt.Fprintf(w, "skipped line %d: %s\n", line, skip)
			skipped++
			continue
		}
		fmt.Fprintf(w, "re-applied %s %s in %s: %s\n", entry.SubjectKind, entry.SubjectRef, entry.TenantID, counts)
		applied++
	}
	if e := scanner.Err(); e != nil {
		return e
	}
	fmt.Fprintf(w, "%d re-applied, %d skipped\n", applied, skipped)
	return nil
}

// reapply redoes one ledger entry in its own transaction; skip explains an entry that was not redone.
func reapply(ctx context.Context, db *sql.DB, entry LedgerEntry) (counts, skip string, err error) {
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return "", "", e
	}
	defer tx.Rollback()
	scope := entry.Scope
	if len(scope) == 0 {
		scope = json.RawMessage(`{}`)
	}
	switch entry.SubjectKind {
	case "member":
		if _, e := uuid.Parse(entry.SubjectRef); e != nil {
			return "", "", fmt.Errorf("bad member id")
		}
		last, e := lockAndCheckOwner(ctx, tx, entry.TenantID, entry.SubjectRef)
		if e != nil {
			return "", "", e
		}
		if last {
			return "", fmt.Sprintf("member %s is the last owner of %s in this database; erase by hand (admin erase-user --force) after giving the workspace another owner", entry.SubjectRef, entry.TenantID), nil
		}
		e = tx.QueryRowContext(ctx, `SELECT core.erase_member_data($1,$2,NULL)::text`, entry.TenantID, entry.SubjectRef).Scan(&counts)
		if e != nil {
			return "", "", e
		}
	case "device_identity":
		var s identityScope
		if json.Unmarshal(scope, &s) != nil || s.TenantWide == nil {
			return "", "", fmt.Errorf("device_identity entry without a scope")
		}
		if s.ProjectID != nil {
			if _, e := uuid.Parse(*s.ProjectID); e != nil {
				return "", "", fmt.Errorf("bad project id in scope")
			}
		}
		if e := tx.QueryRowContext(ctx, `SELECT core.erase_identity_data($1,$2,$3,$4)::text`, entry.TenantID, entry.SubjectRef, s.ProjectID, *s.TenantWide).Scan(&counts); e != nil {
			return "", "", e
		}
		if s.Renamed && s.DeviceID != nil {
			if _, e := uuid.Parse(*s.DeviceID); e != nil {
				return "", "", fmt.Errorf("bad device id in scope")
			}
			if _, e := tx.ExecContext(ctx, `UPDATE core.devices SET name=$3 WHERE tenant_id=$1 AND id=$2 AND external_id=$4 AND name<>$3`, entry.TenantID, *s.DeviceID, erasedName, entry.SubjectRef); e != nil {
				return "", "", e
			}
		}
	default:
		return "", "", fmt.Errorf("unknown subject kind %q", entry.SubjectKind)
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO core.erasure_log(tenant_id,id,at,actor_id,subject_kind,subject_ref,counts,scope)
    VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, entry.TenantID, entry.ID, entry.At, entry.ActorID, entry.SubjectKind, entry.SubjectRef, string(entry.Counts), string(scope)); e != nil {
		return "", "", e
	}
	return counts, "", tx.Commit()
}

// EraseUser erases an identity in every workspace it belongs to (the in-app erasure refuses an identity several
// workspaces share). Workspaces are locked in id order; one where the identity is the last owner refuses the whole
// erasure unless force. Each workspace gets its ledger row, with no actor (the platform operator).
func EraseUser(ctx context.Context, db *sql.DB, user string, force bool, w io.Writer) error {
	if _, e := uuid.Parse(user); e != nil {
		return fmt.Errorf("not a user id: %q", user)
	}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, `SELECT tenant_id::text FROM core.memberships WHERE user_id=$1 ORDER BY tenant_id`, user)
	if e != nil {
		return e
	}
	tenants := []string{}
	for rows.Next() {
		var t string
		if e := rows.Scan(&t); e != nil {
			rows.Close()
			return e
		}
		tenants = append(tenants, t)
	}
	rows.Close()
	if len(tenants) == 0 {
		return fmt.Errorf("the identity belongs to no workspace; nothing to erase through the ledger")
	}
	for _, tenant := range tenants {
		last, e := lockAndCheckOwner(ctx, tx, tenant, user)
		if e != nil {
			return e
		}
		if last && !force {
			return fmt.Errorf("%w %s: give it another owner first, or pass --force", ErrLastOwner, tenant)
		}
	}
	if _, e := tx.ExecContext(ctx, `SELECT 1 FROM identity.users WHERE id=$1 FOR UPDATE`, user); e != nil {
		return e
	}
	for _, tenant := range tenants {
		var counts string
		if e := tx.QueryRowContext(ctx, `SELECT core.erase_member_data($1,$2,NULL)::text`, tenant, user).Scan(&counts); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO core.erasure_log(tenant_id,id,subject_kind,subject_ref,counts) VALUES($1,$2,'member',$3,$4)`, tenant, uuid.NewString(), user, counts); e != nil {
			return e
		}
		fmt.Fprintf(w, "erased in %s: %s\n", tenant, counts)
	}
	// A membership added between the first read and the locks would survive the loop above: check, under the locks,
	// that none is left and the identity really is anonymised before committing.
	var left int
	var anonymised bool
	if e := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM core.memberships WHERE user_id=$1),
    EXISTS(SELECT 1 FROM identity.users WHERE id=$1 AND erased_at IS NOT NULL AND email LIKE 'erased-%@erased.invalid')`, user).Scan(&left, &anonymised); e != nil {
		return e
	}
	if left != 0 || !anonymised {
		return fmt.Errorf("the identity still has %d memberships (anonymised: %v); nothing was erased, run again", left, anonymised)
	}
	return tx.Commit()
}
