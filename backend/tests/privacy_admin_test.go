package tests

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"aether/backend/internal/privacyops"
	"github.com/google/uuid"
)

// The platform operator's erasure of a shared identity (admin erase-user), its last-owner guard, and the ledger
// carried across a restore (export-erasures before, reapply-erasures after), for a member and for a tag with its
// project scope.
func TestOperatorEraseUserAndReapplyAfterRestore(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := f.admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	user, t1, t2 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	email := user + "@example.test"
	exec(`INSERT INTO core.tenants(id,name) VALUES($1,'one'),($2,'two')`, t1, t2)
	exec(`INSERT INTO identity.users(id,email,name,password_hash) VALUES($1,$2,'Shared Person','x')`, user, email)
	exec(`INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$3,'viewer'),($2,$3,'owner')`, t1, t2, user)
	exec(`INSERT INTO identity.sessions(id,user_id,tenant_id,expires_at) VALUES(gen_random_uuid(),$1,$2,now()+interval '1 day')`, user, t1)

	// The last owner of workspace two: refused without --force, and nothing changes.
	var out bytes.Buffer
	if e := privacyops.EraseUser(ctx, f.admin, user, false, &out); !errors.Is(e, privacyops.ErrLastOwner) {
		t.Fatalf("erase-user of a last owner: %v", e)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.memberships WHERE user_id=$1`, user); n != 2 {
		t.Fatalf("a refused erase-user changed memberships: %d", n)
	}
	// Another owner in workspace two, then it goes through.
	other := uuid.NewString()
	exec(`INSERT INTO identity.users(id,email,name,password_hash) VALUES($1,$2,'Other','x')`, other, other+"@example.test")
	exec(`INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, t2, other)
	if e := privacyops.EraseUser(ctx, f.admin, user, false, &out); e != nil {
		t.Fatal(e)
	}
	erased := func() bool {
		return count(t, f.admin, `SELECT count(*) FROM identity.users WHERE id=$1 AND erased_at IS NOT NULL AND email LIKE 'erased-%'`, user) == 1
	}
	memberships := func() int { return count(t, f.admin, `SELECT count(*) FROM core.memberships WHERE user_id=$1`, user) }
	if !erased() || memberships() != 0 || strings.Count(out.String(), "erased in") != 2 {
		t.Fatalf("erase-user: %s", out.String())
	}

	// A tag erased in one project of workspace one.
	project, inScope, outOfScope := uuid.NewString(), uuid.NewString(), uuid.NewString()
	const tag = "c3000039bb02"
	exec(`INSERT INTO core.projects(tenant_id,id,name) VALUES($1,$2,'P')`, t1, project)
	exec(`INSERT INTO core.gateways(id,tenant_id,name,model,token_hash,project_id) VALUES($2,$1,'in','minew-mg3','x',$4),($3,$1,'out','minew-mg3','x',NULL)`, t1, inScope, outOfScope, project)
	seedEvents := func() {
		exec(`INSERT INTO core.device_events(tenant_id,id,gateway_id,external_id,device_name,event_type,occurred_at)
      VALUES($1,gen_random_uuid(),$2,$4,'W','zone',now()),($1,gen_random_uuid(),$3,$4,'W','zone',now())`, t1, inScope, outOfScope, tag)
	}
	seedEvents()
	// The owner renamed the registration with the erasure; a restore brings the wearer's name back.
	device := uuid.NewString()
	exec(`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id) VALUES($1,$2,$3,'Tag 9',$4,'minew-b10')`, device, t1, inScope, tag)
	exec(`SELECT core.erase_identity_data($1,$2,$3,false)`, t1, tag, project)
	exec(`INSERT INTO core.erasure_log(tenant_id,id,subject_kind,subject_ref,counts,scope) VALUES($1,gen_random_uuid(),'device_identity',$2,'{}',
    jsonb_build_object('project_id',$3::uuid,'tenant_wide',false,'device_id',$4::uuid,'renamed',true))`, t1, tag, project, device)
	events := func(gateway string) int {
		return count(t, f.admin, `SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id=$2`, gateway, tag)
	}
	if events(inScope) != 0 || events(outOfScope) != 1 {
		t.Fatalf("scoped erase: %d in scope, %d out", events(inScope), events(outOfScope))
	}

	var ledger bytes.Buffer
	if e := privacyops.ExportErasures(ctx, f.admin, &ledger); e != nil {
		t.Fatal(e)
	}
	if strings.Count(ledger.String(), user) < 2 || !strings.Contains(ledger.String(), tag) {
		t.Fatalf("ledger lacks the erasures: %s", ledger.String())
	}
	// A restore from before the erasures: the person, the memberships, the tag's events, and no ledger rows.
	exec(`UPDATE identity.users SET email=$2,name='Shared Person',password_hash='x',erased_at=NULL WHERE id=$1`, user, email)
	exec(`INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$3,'viewer'),($2,$3,'owner')`, t1, t2, user)
	seedEvents()
	exec(`UPDATE core.devices SET name='Somchai' WHERE id=$1`, device)
	exec(`DELETE FROM core.erasure_log WHERE tenant_id IN ($1,$2)`, t1, t2)

	out.Reset()
	if e := privacyops.ReapplyErasures(ctx, f.admin, strings.NewReader(ledger.String()), &out); e != nil {
		t.Fatal(e)
	}
	if !erased() || memberships() != 0 {
		t.Fatalf("the restored person was not erased again: %s", out.String())
	}
	if events(inScope) != 0 || events(outOfScope) != 2 {
		t.Fatalf("reapplied tag erasure left its scope: %d in scope, %d out", events(inScope), events(outOfScope))
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.devices WHERE id=$1 AND name LIKE 'ผู้สวมใส่%'`, device); n != 1 {
		t.Fatal("the restored registration kept the wearer's name")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.erasure_log WHERE tenant_id IN ($1,$2)`, t1, t2); n != 3 {
		t.Fatalf("ledger rows after reapply: %d", n)
	}
	// Idempotent: a second run changes nothing and fails on nothing.
	if e := privacyops.ReapplyErasures(ctx, f.admin, strings.NewReader(ledger.String()), &out); e != nil {
		t.Fatal(e)
	}
	// A restore where the person is again the only owner of workspace two: that entry is reported and skipped.
	exec(`DELETE FROM core.memberships WHERE tenant_id=$1 AND user_id=$2`, t2, other)
	exec(`UPDATE identity.users SET email=$2,erased_at=NULL WHERE id=$1`, user, email)
	exec(`INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, t2, user)
	out.Reset()
	if e := privacyops.ReapplyErasures(ctx, f.admin, strings.NewReader(ledger.String()), &out); e != nil || !strings.Contains(out.String(), "last owner") {
		t.Fatalf("last owner on reapply: %v %s", e, out.String())
	}
	if memberships() != 1 {
		t.Fatal("the skipped entry still erased the last owner")
	}
	// A workspace the restored database does not have is skipped, not an error.
	missing := strings.ReplaceAll(ledger.String(), t1, uuid.NewString())
	out.Reset()
	if e := privacyops.ReapplyErasures(ctx, f.admin, strings.NewReader(missing), &out); e != nil || !strings.Contains(out.String(), "is not in this database") {
		t.Fatalf("missing workspace: %v %s", e, out.String())
	}
	// --force erases a last owner.
	if e := privacyops.EraseUser(ctx, f.admin, user, true, &out); e != nil || memberships() != 0 {
		t.Fatalf("--force: %v", e)
	}
}
