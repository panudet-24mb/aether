package tests

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/domain"
)

// asRuntime opens a transaction on the aether_app connection with the identity the API would set.
// An empty user/tenant leaves the context unset, like a statement that runs outside any request.
// f.runtime holds one connection, so the caller rolls back before opening the next one.
func asRuntime(t *testing.T, f *fixture, user, tenant string) *sql.Tx {
	t.Helper()
	tx, e := f.runtime.Begin()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { tx.Rollback() })
	if user == "" && tenant == "" {
		return tx
	}
	if _, e = tx.Exec(`SELECT set_config('app.user_id',$1,true),set_config('app.tenant_id',$2,true)`, user, tenant); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`SELECT set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`); e != nil {
		t.Fatal(e)
	}
	return tx
}

func visibleEmails(t *testing.T, tx *sql.Tx, query string, args ...any) map[string]bool {
	t.Helper()
	rows, e := tx.Query(query, args...)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var email string
		if e := rows.Scan(&email); e != nil {
			t.Fatal(e)
		}
		out[email] = true
	}
	if e := rows.Err(); e != nil {
		t.Fatal(e)
	}
	return out
}

func sqlState(e error) string {
	var pg *pgconn.PgError
	if errors.As(e, &pg) {
		return pg.Code
	}
	return ""
}

// identity.users is under RLS (migration 00035): the runtime role sees its own identity, an owner/admin
// sees the members of the current workspace, nobody sees another workspace's people, and no runtime
// statement can read a password hash.
func TestIdentityUsersRowLevelSecurity(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, authA, ownerA := f.account(t)
	b, _, _ := f.account(t)
	api := busyAPI(f)
	memberAddr := memberEmail()
	memberUser := addMember(t, api, authA.AccessToken, memberAddr, "viewer", []string{})

	// Tenant A's owner: exactly A's two members, and B's owner is invisible even by exact email.
	tx := asRuntime(t, f, ownerA.UserID, ownerA.TenantID)
	seen := visibleEmails(t, tx, `SELECT email FROM identity.users`)
	if len(seen) != 2 || !seen[a.User.Email] || !seen[memberAddr] {
		t.Fatalf("an owner must see exactly their workspace's identities: %v", seen)
	}
	if got := visibleEmails(t, tx, `SELECT email FROM identity.users WHERE email=$1`, b.User.Email); len(got) != 0 {
		t.Fatalf("another workspace's identity is readable by email: %v", got)
	}
	// Not even through the definer path an admin has: a foreign identity comes back "taken" with no id,
	// and nothing is written.
	var id sql.NullString
	var outcome string
	create := func(tx *sql.Tx, email, role string) {
		t.Helper()
		if e := tx.QueryRow(`SELECT member_id,outcome FROM identity.create_member_identity($1,$2,'n',$3,$4)`, uuid.NewString(), email, memberPassword, role).Scan(&id, &outcome); e != nil {
			t.Fatal(e)
		}
	}
	create(tx, b.User.Email, "viewer")
	if outcome != "taken" || id.Valid {
		t.Fatalf("create_member_identity leaked a foreign identity: %v %s", id, outcome)
	}
	if create(tx, a.User.Email, "viewer"); outcome != "self" {
		t.Fatalf("the caller's own email: %v %s", id, outcome)
	}
	if create(tx, memberAddr, "viewer"); outcome != "taken" || id.Valid {
		t.Fatalf("an existing member of this workspace: %v %s", id, outcome)
	}

	// A non-admin member sees only their own identity, and is refused by the lookup whatever the email.
	tx.Rollback()
	tx = asRuntime(t, f, memberUser, ownerA.TenantID)
	if seen := visibleEmails(t, tx, `SELECT email FROM identity.users`); len(seen) != 1 || !seen[memberAddr] {
		t.Fatalf("a viewer must see only themselves: %v", seen)
	}
	if create(tx, memberEmail(), "viewer"); outcome != "refused" || id.Valid {
		t.Fatalf("a non-admin created a member: %v %s", id, outcome)
	}
	// Only the caller's own id may be inserted directly, admin or not (users_insert).
	if _, e := tx.Exec(`INSERT INTO identity.users(id,email,name,password_hash) VALUES($1,$2,'x','x')`, uuid.NewString(), memberEmail()); sqlState(e) != "42501" {
		t.Fatalf("a non-admin created an identity: %v", e)
	}

	// No context at all: nothing, not even a count.
	tx.Rollback()
	tx = asRuntime(t, f, "", "")
	if seen := visibleEmails(t, tx, `SELECT email FROM identity.users`); len(seen) != 0 {
		t.Fatalf("identities readable without a context: %v", seen)
	}
	var count int
	if e := tx.QueryRow(`SELECT count(id) FROM identity.users`).Scan(&count); e != nil || count != 0 {
		t.Fatalf("identity count without a context: %d %v", count, e)
	}
	var owned sql.NullString
	if e := tx.QueryRow(`SELECT identity.own_password_hash()`).Scan(&owned); e != nil || owned.Valid {
		t.Fatalf("own_password_hash without identity: %v %v", owned, e)
	}

	// The hash column is not granted, whatever the rows: 42501 even for the caller's own row.
	tx.Rollback()
	tx = asRuntime(t, f, ownerA.UserID, ownerA.TenantID)
	if e := tx.QueryRow(`SELECT identity.own_password_hash()`).Scan(&owned); e != nil || !owned.Valid || owned.String == "" {
		t.Fatalf("own_password_hash for the caller: %v", e)
	}
	// Each refused statement aborts the transaction, so each runs behind its own savepoint.
	var hash string
	for _, refused := range []func() error{
		func() error {
			return tx.QueryRow(`SELECT password_hash FROM identity.users WHERE id=$1`, ownerA.UserID).Scan(&hash)
		},
		func() error {
			_, e := tx.Exec(`UPDATE identity.users SET name='x' WHERE id=$1`, ownerA.UserID)
			return e
		},
		func() error { // an admin creates members only through create_member_identity
			_, e := tx.Exec(`INSERT INTO identity.users(id,email,name,password_hash) VALUES($1,$2,'x','x')`, uuid.NewString(), memberEmail())
			return e
		},
	} {
		if _, e := tx.Exec(`SAVEPOINT refused`); e != nil {
			t.Fatal(e)
		}
		if e := refused(); sqlState(e) != "42501" {
			t.Fatalf("the runtime read or wrote identity.users outside its grants and policies: %v", e)
		}
		if _, e := tx.Exec(`ROLLBACK TO SAVEPOINT refused`); e != nil {
			t.Fatal(e)
		}
	}
	tx.Rollback()

	// Auth still works end to end: login, refresh, "who am I", the member list and the own-password change.
	auth, e := f.service.Login(ctx, a.User.Email, "correct horse battery staple", a.TenantID)
	if e != nil {
		t.Fatalf("login after RLS: %v", e)
	}
	if _, e := f.service.Refresh(ctx, auth.RefreshToken); e != nil {
		t.Fatalf("refresh after RLS: %v", e)
	}
	code, out := get(t, api, "/api/v1/me", authA.AccessToken)
	if code != 200 || out["email"] != a.User.Email {
		t.Fatalf("/me after RLS: %d %v", code, out)
	}
	code, out = get(t, api, "/api/v1/members", authA.AccessToken)
	if code != 200 || len(rows(out)) != 2 || !values(out, "email")[memberAddr] {
		t.Fatalf("/members after RLS: %d %v", code, out)
	}
	memberAuth, _ := changeInitialPassword(t, f, api, memberAddr, ownerA.TenantID)
	if code, out := get(t, api, "/api/v1/me", memberAuth.AccessToken); code != 200 || out["email"] != memberAddr {
		t.Fatalf("a viewer's /me after RLS: %d %v", code, out)
	}
	if _, e := f.service.Login(ctx, memberEmail(), "correct horse battery staple", ""); e == nil {
		t.Fatal("login with an unknown email")
	}

	// Adding one's own email is still the explicit 403, and a foreign identity still the plain 409.
	payload := map[string]any{"email": a.User.Email, "role": "viewer", "password": memberPassword, "project_ids": []string{}}
	if code, out, _ := req(t, api, "POST", "/api/v1/members", "Bearer "+authA.AccessToken, "", "", payload); code != 403 {
		t.Fatalf("adding one's own email: %d %v", code, out)
	}
	payload["email"] = b.User.Email
	if code, out, _ := req(t, api, "POST", "/api/v1/members", "Bearer "+authA.AccessToken, "", "", payload); code != 409 || out["detail"] != nil {
		t.Fatalf("adding another workspace's identity: %d %v", code, out)
	}
}

// The start-up check refuses a database where an identity table lost FORCE RLS, or where the runtime role
// can read password_hash again. Each case is staged inside one admin transaction that is rolled back, with
// SET LOCAL ROLE aether_app so current_user is the runtime role: the shared test database is never left
// changed, even if the test dies half way.
func TestRuntimeRoleCheckCoversIdentity(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for name, stage := range map[string]string{
		"sound":              ``,
		"no FORCE RLS":       `ALTER TABLE identity.users NO FORCE ROW LEVEL SECURITY`,
		"no RLS":             `ALTER TABLE identity.users DISABLE ROW LEVEL SECURITY`,
		"password_hash read": `GRANT SELECT(password_hash) ON identity.users TO aether_app`,
		"table-wide read":    `GRANT SELECT ON identity.users TO aether_app`,
		"update grant":       `GRANT UPDATE ON identity.users TO aether_app`,
		"column update":      `GRANT UPDATE(name) ON identity.users TO aether_app`,
		"delete grant":       `GRANT DELETE ON identity.users TO aether_app`,
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
		if _, e := tx.Exec(`SET LOCAL ROLE aether_app`); e != nil {
			tx.Rollback()
			t.Fatal(e)
		}
		e = postgres.CheckRuntimeRole(ctx, tx)
		tx.Rollback()
		if name == "sound" && e != nil {
			t.Fatalf("the migrated database was refused: %v", e)
		}
		if name != "sound" && e == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// And the real entry point on the untouched database.
	repo, e := postgres.Open(os.Getenv("TEST_DATABASE_URL"))
	if e != nil {
		t.Fatalf("Open refused the migrated database: %v", e)
	}
	repo.Close()
}

// Two workspaces adopting the same orphan identity at once: the identity row lock serialises them, and the
// second sees the first one's membership once it commits.
func TestConcurrentAdoptionOfOneOrphan(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, authA, ownerA := f.account(t)
	_, _, ownerB := f.account(t)
	api := busyAPI(f)
	orphan := memberEmail()
	orphanID := addMember(t, api, authA.AccessToken, orphan, "viewer", []string{})
	if code, _, _ := req(t, api, "POST", "/api/v1/members/"+orphanID+"/remove", "Bearer "+authA.AccessToken, "", "", map[string]any{}); code != 204 {
		t.Fatalf("remove: %d", code)
	}

	pool, e := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pool.Close() })
	begin := func(p domain.Principal) *sql.Tx {
		tx, e := pool.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { tx.Rollback() })
		if _, e = tx.Exec(`SELECT set_config('app.user_id',$1,true),set_config('app.tenant_id',$2,true)`, p.UserID, p.TenantID); e != nil {
			t.Fatal(e)
		}
		return tx
	}
	adopt := `SELECT member_id,outcome FROM identity.create_member_identity($1,$2,'Returning',$3,'viewer')`

	first := begin(ownerA)
	var id sql.NullString
	var outcome string
	if e := first.QueryRow(adopt, uuid.NewString(), orphan, memberPassword).Scan(&id, &outcome); e != nil || outcome != "adopted" || id.String != orphanID {
		t.Fatalf("first adoption: %v %s %v", id, outcome, e)
	}
	second := begin(ownerB)
	type answer struct {
		id      sql.NullString
		outcome string
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		var a answer
		a.err = second.QueryRow(adopt, uuid.NewString(), orphan, memberPassword).Scan(&a.id, &a.outcome)
		done <- a
	}()
	// The second adoption must wait on the identity row, not race past the membership count.
	waiting := false
	for i := 0; i < 50 && !waiting; i++ {
		select {
		case a := <-done:
			t.Fatalf("the second adoption did not wait for the first: %v %s %v", a.id, a.outcome, a.err)
		case <-time.After(100 * time.Millisecond):
		}
		f.admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%create_member_identity%')`).Scan(&waiting)
	}
	if !waiting {
		t.Fatal("the second adoption is not waiting on a lock")
	}
	if e := first.Commit(); e != nil {
		t.Fatal(e)
	}
	a := <-done
	if a.err != nil || a.outcome != "taken" || a.id.Valid {
		t.Fatalf("the second workspace adopted an identity the first already holds: %v %s %v", a.id, a.outcome, a.err)
	}
	second.Rollback()
	var memberships int
	if e := f.admin.QueryRow(`SELECT count(*) FROM core.memberships WHERE user_id=$1`, orphanID).Scan(&memberships); e != nil || memberships != 1 {
		t.Fatalf("the orphan ended up in %d workspaces: %v", memberships, e)
	}
	// Adoption re-issued the identity with the supplied name.
	var name string
	if e := f.admin.QueryRow(`SELECT name FROM identity.users WHERE id=$1`, orphanID).Scan(&name); e != nil || name != "Returning" {
		t.Fatalf("the adopted identity kept its old name: %q %v", name, e)
	}
}

// set_member_password locks the identity row before it counts the workspaces the identity belongs to, so
// a reset and an adoption of the same identity cannot interleave. Proven on a shared identity, where the
// function refuses without writing: the lock it holds is then the FOR UPDATE alone.
func TestResetLocksTheIdentityBeforeCounting(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, authA, ownerA := f.account(t)
	_, _, ownerB := f.account(t)
	api := busyAPI(f)
	shared := memberEmail()
	sharedID := addMember(t, api, authA.AccessToken, shared, "viewer", []string{})
	// A second membership only an operator could create by hand: the identity is now shared.
	if _, e := f.admin.Exec(`INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$2,'viewer')`, ownerB.TenantID, sharedID); e != nil {
		t.Fatal(e)
	}
	tx := asRuntime(t, f, ownerA.UserID, ownerA.TenantID)
	defer tx.Rollback()
	var ok bool
	if e := tx.QueryRow(`SELECT identity.set_member_password($1,$2)`, sharedID, memberPassword).Scan(&ok); e != nil || ok {
		t.Fatalf("a shared identity's password was rewritten: %v %v", ok, e)
	}
	probe, e := f.admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer probe.Rollback()
	var one int
	if e := probe.QueryRow(`SELECT 1 FROM identity.users WHERE id=$1 FOR UPDATE NOWAIT`, sharedID).Scan(&one); sqlState(e) != "55P03" {
		t.Fatalf("set_member_password did not hold the identity row lock: %v", e)
	}
	// The reset path itself still works for a member of this workspace only (identity_membership_count is
	// now limited to members of the current workspace).
	tx.Rollback()
	probe.Rollback()
	sole := memberEmail()
	soleID := addMember(t, api, authA.AccessToken, sole, "viewer", []string{})
	if code, out, _ := req(t, api, "POST", "/api/v1/members/"+soleID+"/reset-password", "Bearer "+authA.AccessToken, "", "", map[string]any{"password": "another handover 1"}); code != 204 {
		t.Fatalf("reset a sole member: %d %v", code, out)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/members/"+sharedID+"/reset-password", "Bearer "+authA.AccessToken, "", "", map[string]any{"password": "another handover 1"}); code != 409 {
		t.Fatalf("reset a shared identity: %d", code)
	}
	// A foreign identity id is not answered by the count.
	tx = asRuntime(t, f, ownerA.UserID, ownerA.TenantID)
	var n int
	if e := tx.QueryRow(`SELECT core.identity_membership_count($1)`, ownerB.UserID).Scan(&n); e != nil || n != -1 {
		t.Fatalf("identity_membership_count answered for a non-member: %d %v", n, e)
	}
}

// Every refused add is audited as member.add_refused:<reason> without naming the identity (audit_logs has
// no details column, so the reason is the action suffix), and an actor with 20 refusals in 24 hours is
// answered 429 before anything else is looked at.
func TestRefusedAddIsAudited(t *testing.T) {
	f := setup(t)
	a, authA, ownerA := f.account(t)
	b, _, _ := f.account(t)
	api := busyAPI(f)
	add := func(token, email, role string) (int, map[string]any) {
		payload := map[string]any{"email": email, "role": role, "password": memberPassword, "project_ids": []string{}}
		code, out, _ := req(t, api, "POST", "/api/v1/members", "Bearer "+token, "", "", payload)
		return code, out
	}
	reasons := func(actor string) map[string]int {
		rows, e := f.admin.Query(`SELECT action,target_id::text FROM core.audit_logs WHERE tenant_id=$1 AND actor_id=$2 AND action LIKE 'member.add_refused%'`, ownerA.TenantID, actor)
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var action, target string
			if e := rows.Scan(&action, &target); e != nil {
				t.Fatal(e)
			}
			if target != uuid.Nil.String() {
				t.Fatalf("a refusal audit names an identity: %s %s", action, target)
			}
			out[action]++
		}
		return out
	}
	if code, out := add(authA.AccessToken, b.User.Email, "viewer"); code != 409 || out["detail"] != nil {
		t.Fatalf("a foreign identity: %d %v", code, out)
	}
	if code, _ := add(authA.AccessToken, a.User.Email, "viewer"); code != 403 {
		t.Fatalf("one's own email: %d", code)
	}
	adminEmail := memberEmail()
	addMember(t, api, authA.AccessToken, adminEmail, "admin", []string{})
	adminAuth, admin := changeInitialPassword(t, f, api, adminEmail, ownerA.TenantID)
	if code, _ := add(adminAuth.AccessToken, memberEmail(), "owner"); code != 403 {
		t.Fatalf("an admin adding an owner: %d", code)
	}
	got := reasons(ownerA.UserID)
	if got["member.add_refused:taken"] != 1 || got["member.add_refused:self"] != 1 {
		t.Fatalf("owner refusals: %v", got)
	}
	if got := reasons(admin.UserID); got["member.add_refused:role"] != 1 {
		t.Fatalf("admin refusals: %v", got)
	}
	code, out := get(t, api, "/api/v1/members", authA.AccessToken)
	if code != 200 || len(rows(out)) != 2 {
		t.Fatalf("refused adds created memberships: %d %v", code, out)
	}

	// 18 more refusals bring the owner to 20; the next attempt is 429 and is not itself counted.
	for i := 0; i < 18; i++ {
		if code, _ := add(authA.AccessToken, b.User.Email, "viewer"); code != 409 {
			t.Fatalf("refusal %d: %d", i+3, code)
		}
	}
	if code, out := add(authA.AccessToken, memberEmail(), "viewer"); code != 429 {
		t.Fatalf("an actor with 20 refusals in 24 hours was served: %d %v", code, out)
	}
	if n := len(reasons(ownerA.UserID)); n != 2 {
		t.Fatalf("reasons after the limit: %v", reasons(ownerA.UserID))
	}
	var total int
	f.admin.QueryRow(`SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND actor_id=$2 AND action LIKE 'member.add_refused%'`, ownerA.TenantID, ownerA.UserID).Scan(&total)
	if total != 20 {
		t.Fatalf("refusals counted: %d", total)
	}
	// Another actor of the same workspace is not affected, and the window is 24 hours.
	if code, _ := add(adminAuth.AccessToken, memberEmail(), "viewer"); code != 201 {
		t.Fatalf("the limit leaked to another actor: %d", code)
	}
	if _, e := f.admin.Exec(`UPDATE core.audit_logs SET at=now()-interval '25 hours' WHERE tenant_id=$1 AND actor_id=$2`, ownerA.TenantID, ownerA.UserID); e != nil {
		t.Fatal(e)
	}
	if code, _ := add(authA.AccessToken, memberEmail(), "viewer"); code != 201 {
		t.Fatalf("refusals older than 24 hours still count: %d", code)
	}
}

// create_member_identity validates its own arguments, whatever the Go layer checked first.
func TestCreateMemberIdentityRefusals(t *testing.T) {
	f := setup(t)
	_, authA, ownerA := f.account(t)
	api := busyAPI(f)
	adminID := addMember(t, api, authA.AccessToken, memberEmail(), "admin", []string{})
	tx := asRuntime(t, f, adminID, ownerA.TenantID)
	defer tx.Rollback()
	var id sql.NullString
	var outcome string
	for name, args := range map[string][]any{
		"admin asks for owner": {uuid.NewString(), memberEmail(), "n", memberPassword, "owner"},
		"unknown role":         {uuid.NewString(), memberEmail(), "n", memberPassword, "root"},
		"NULL id":              {nil, memberEmail(), "n", memberPassword, "viewer"},
		"NULL email":           {uuid.NewString(), nil, "n", memberPassword, "viewer"},
		"NULL name":            {uuid.NewString(), memberEmail(), nil, memberPassword, "viewer"},
		"NULL hash":            {uuid.NewString(), memberEmail(), "n", nil, "viewer"},
		"NULL role":            {uuid.NewString(), memberEmail(), "n", memberPassword, nil},
	} {
		if e := tx.QueryRow(`SELECT member_id,outcome FROM identity.create_member_identity($1::uuid,$2,$3,$4,$5)`, args...).Scan(&id, &outcome); e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		if outcome != "refused" || id.Valid {
			t.Fatalf("%s: %v %s", name, id, outcome)
		}
	}
	// And the admin may still add an ordinary member through it.
	email := memberEmail()
	if e := tx.QueryRow(`SELECT member_id,outcome FROM identity.create_member_identity($1,$2,'n',$3,'viewer')`, uuid.NewString(), email, memberPassword).Scan(&id, &outcome); e != nil || outcome != "created" || !id.Valid {
		t.Fatalf("an admin adding a viewer: %v %s %v", id, outcome, e)
	}
}

// The membership policies say what members.go enforces: no runtime INSERT except the first owner of a new
// workspace, never one's own row, and owner rows or role='owner' only for an owner.
func TestMembershipPoliciesMirrorGo(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, authA, ownerA := f.account(t)
	_, _, ownerB := f.account(t)
	api := busyAPI(f)
	adminID := addMember(t, api, authA.AccessToken, memberEmail(), "admin", []string{})
	viewerID := addMember(t, api, authA.AccessToken, memberEmail(), "viewer", []string{})
	secondOwner := addMember(t, api, authA.AccessToken, memberEmail(), "owner", []string{})
	orphan := addMember(t, api, authA.AccessToken, memberEmail(), "viewer", []string{})
	if code, _, _ := req(t, api, "POST", "/api/v1/members/"+orphan+"/remove", "Bearer "+authA.AccessToken, "", "", map[string]any{}); code != 204 {
		t.Fatalf("remove: %d", code)
	}
	role := func(user string) string {
		var r string
		if e := f.admin.QueryRow(`SELECT role FROM core.memberships WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, user).Scan(&r); e != nil {
			t.Fatal(e)
		}
		return r
	}
	// refused runs one statement as `actor` in its own transaction and expects an RLS refusal (42501) or,
	// for a USING filter, zero rows touched.
	refused := func(actor, tenant, stmt string, args ...any) {
		t.Helper()
		tx := asRuntime(t, f, actor, tenant)
		defer tx.Rollback()
		res, e := tx.Exec(stmt, args...)
		if e != nil {
			if sqlState(e) != "42501" {
				t.Fatalf("%s: %v", stmt, e)
			}
			return
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Fatalf("%s: %d rows written by %s", stmt, n, actor)
		}
	}
	// INSERT: an admin adds members only through create_member_identity, and nobody joins a workspace
	// that already has members by inserting their own row, here or elsewhere.
	refused(adminID, ownerA.TenantID, `INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$2,'viewer')`, ownerA.TenantID, orphan)
	refused(ownerA.UserID, ownerA.TenantID, `INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$2,'viewer')`, ownerA.TenantID, orphan)
	refused(ownerA.UserID, ownerB.TenantID, `INSERT INTO core.memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, ownerB.TenantID, ownerA.UserID)
	// UPDATE: an admin never hands out owner, never touches an owner row, never edits their own row.
	refused(adminID, ownerA.TenantID, `UPDATE core.memberships SET role='owner' WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, viewerID)
	refused(adminID, ownerA.TenantID, `UPDATE core.memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, secondOwner)
	refused(adminID, ownerA.TenantID, `UPDATE core.memberships SET role='owner' WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, adminID)
	refused(viewerID, ownerA.TenantID, `UPDATE core.memberships SET role='admin' WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, viewerID)
	refused(ownerA.UserID, ownerA.TenantID, `UPDATE core.memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, ownerA.UserID)
	// DELETE: same rules.
	refused(adminID, ownerA.TenantID, `DELETE FROM core.memberships WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, secondOwner)
	refused(adminID, ownerA.TenantID, `DELETE FROM core.memberships WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, adminID)
	if role(viewerID) != "viewer" || role(secondOwner) != "owner" || role(adminID) != "admin" || role(ownerA.UserID) != "owner" {
		t.Fatal("a refused statement changed a role")
	}
	// What Go allows still works at the policy layer: an admin changes a non-owner, an owner promotes to
	// owner (the only ownership transfer there is) and demotes another owner.
	tx := asRuntime(t, f, adminID, ownerA.TenantID)
	if _, e := tx.Exec(`UPDATE core.memberships SET role='operator' WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, viewerID); e != nil {
		t.Fatalf("an admin changing a viewer: %v", e)
	}
	tx.Commit()
	tx = asRuntime(t, f, ownerA.UserID, ownerA.TenantID)
	if _, e := tx.Exec(`UPDATE core.memberships SET role='owner' WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, viewerID); e != nil {
		t.Fatalf("an owner promoting to owner: %v", e)
	}
	if _, e := tx.Exec(`UPDATE core.memberships SET role='admin' WHERE tenant_id=$1 AND user_id=$2`, ownerA.TenantID, secondOwner); e != nil {
		t.Fatalf("an owner demoting another owner: %v", e)
	}
	tx.Commit()
	if role(viewerID) != "owner" || role(secondOwner) != "admin" {
		t.Fatalf("allowed changes did not land: %s %s", role(viewerID), role(secondOwner))
	}
	// Registration still inserts the first owner of a new workspace through own_membership_insert.
	if _, _, p := f.account(t); p.Role != "owner" {
		t.Fatalf("registration after 00035: %v", p)
	}
	_ = ctx
}

// StartSession re-reads the hash in its own transaction: a password changed between Login's check and the
// session insert refuses the login.
func TestStartSessionRechecksTheVerifiedHash(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, _, _ := f.account(t)
	u, e := f.repo.UserByEmail(ctx, a.User.Email)
	if e != nil {
		t.Fatal(e)
	}
	session := func() domain.Session {
		return domain.Session{ID: uuid.NewString(), UserID: u.ID, TenantID: a.TenantID, ExpiresAt: time.Now().Add(time.Hour)}
	}
	if _, e := f.repo.StartSession(ctx, session(), uuid.NewString(), u.PasswordHash); e != nil {
		t.Fatalf("the verified hash: %v", e)
	}
	for name, hash := range map[string]string{"changed since": u.PasswordHash + "x", "empty": ""} {
		if _, e := f.repo.StartSession(ctx, session(), uuid.NewString(), hash); !errors.Is(e, domain.ErrUnauthorized) {
			t.Fatalf("%s: %v", name, e)
		}
	}
}
