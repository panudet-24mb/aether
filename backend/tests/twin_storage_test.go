package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/demotwin"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// demoWorkspace is the demo workspace with nothing captured yet (demoTwin also plays three live steps).
func demoWorkspace(t *testing.T, f *fixture) demotwin.State {
	t.Helper()
	ctx := context.Background()
	registration := f.service.Registration
	f.service.Registration = true
	defer func() { f.service.Registration = registration }()
	st, e := demotwin.Setup(ctx, f.service, memberEmail(), chosenPassword, "Demo replay test", func(tenant string) error {
		_, e := f.admin.ExecContext(ctx, `UPDATE core.tenants SET demo=true WHERE id=$1`, tenant)
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	return st
}

// getRaw is a raw GET: status, body, ETag.
func getRaw(t *testing.T, api *fiber.App, path, token string, header map[string]string) (int, []byte, string) {
	t.Helper()
	code, body, h := getWithHeaders(t, api, path, token, header)
	return code, body, h.Get("ETag")
}

func getWithHeaders(t *testing.T, api *fiber.App, path, token string, header map[string]string) (int, []byte, http.Header) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	res, e := api.Test(r, fiber.TestConfig{Timeout: 30 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, res.Header
}

func window(from, to time.Time) string {
	return "from=" + url.QueryEscape(from.UTC().Format(time.RFC3339)) + "&to=" + url.QueryEscape(to.UTC().Format(time.RFC3339))
}

// The demo backfill writes a past hour through the real capture path (backdated), and everything the replay reads
// comes out of it: one movement row per zone event, 5-minute buckets that match the raw samples, and an idempotent
// rollup that recomputes a bucket when a late sample arrives.
func TestTwinHistoryStorage(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	st := demoWorkspace(t, f)
	b := simulation.TwinDemo()
	to := time.Now().Add(-2 * time.Minute).Truncate(time.Minute)
	from := to.Add(-40 * time.Minute)
	n, e := demotwin.Backfill(ctx, f.repo, st, from, to, 30*time.Second, nil)
	if e != nil || n == 0 {
		t.Fatalf("backfill: %d %v", n, e)
	}

	// Movement: exactly one presence_history row per zone event of each wearer (two gateways hear every tag).
	for _, p := range b.People {
		var moves, zones int
		if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.presence_history WHERE tenant_id=$1 AND external_id=$2 AND gateway_id IS NOT NULL`, st.TenantID, p.MAC).Scan(&moves); e != nil {
			t.Fatal(e)
		}
		if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.device_events WHERE tenant_id=$1 AND external_id=$2 AND event_type='zone'`, st.TenantID, p.MAC).Scan(&zones); e != nil {
			t.Fatal(e)
		}
		if moves == 0 || moves != zones {
			t.Fatalf("%s: %d movement rows for %d zone events", p.Name, moves, zones)
		}
	}
	var backdated int
	if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.presence_history WHERE tenant_id=$1 AND at < $2`, st.TenantID, to.Add(time.Second)).Scan(&backdated); e != nil || backdated == 0 {
		t.Fatalf("movement rows carry the backfill's times: %d %v", backdated, e)
	}

	// Rollup: a bucket's average equals the raw samples' average (environment frames only, as the live view reads them).
	var ext string
	var bucket time.Time
	var avg float64
	var count int
	if e := f.admin.QueryRowContext(ctx, `SELECT external_id, bucket, t_avg, n FROM core.sample_rollup WHERE tenant_id=$1 AND t_avg IS NOT NULL ORDER BY bucket, external_id LIMIT 1 OFFSET 3`, st.TenantID).Scan(&ext, &bucket, &avg, &count); e != nil {
		t.Fatalf("no rollup rows: %v", e)
	}
	var direct float64
	var directN int
	if e := f.admin.QueryRowContext(ctx, `SELECT avg((reading->>'temperature')::float8) FILTER (WHERE reading->'frames' ? 'minew-ffe1-a101@1'), count(*)
     FROM core.sensor_samples WHERE tenant_id=$1 AND external_id=$2 AND received_at >= $3 AND received_at < $3 + interval '5 minutes'`, st.TenantID, ext, bucket).Scan(&direct, &directN); e != nil {
		t.Fatal(e)
	}
	if math.Abs(direct-avg) > 0.01 || directN != count {
		t.Fatalf("bucket %s of %s: rollup %.3f over %d samples, raw %.3f over %d", bucket, ext, avg, count, direct, directN)
	}
	checksum := func() string {
		var s string
		if e := f.admin.QueryRowContext(ctx, `SELECT count(*)::text||':'||coalesce(sum(n),0)::text||':'||coalesce(round(sum(t_avg)::numeric,3),0)::text FROM core.sample_rollup WHERE tenant_id=$1`, st.TenantID).Scan(&s); e != nil {
			t.Fatal(e)
		}
		return s
	}
	before := checksum()
	if _, e := f.repo.RollupRange(ctx, st.TenantID, from, to); e != nil {
		t.Fatal(e)
	}
	if after := checksum(); after != before {
		t.Fatalf("a re-run changed the rollup: %s -> %s", before, after)
	}
	// A late sample lands in an already rolled-up bucket: the next run recomputes it.
	var gateway string
	if e := f.admin.QueryRowContext(ctx, `SELECT gateway_id::text FROM core.sample_rollup WHERE tenant_id=$1 AND external_id=$2 AND bucket=$3 LIMIT 1`, st.TenantID, ext, bucket).Scan(&gateway); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading)
     VALUES($1,$2,$3,'late',$4,'minew-ffe1-a101@1','{"frames":["minew-ffe1-a101@1"],"temperature":99,"humidity":50}')`, st.TenantID, gateway, ext, bucket.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.RollupRange(ctx, st.TenantID, bucket, bucket.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	var tmax float64
	if e := f.admin.QueryRowContext(ctx, `SELECT t_max FROM core.sample_rollup WHERE tenant_id=$1 AND gateway_id=$2 AND external_id=$3 AND bucket=$4`, st.TenantID, gateway, ext, bucket).Scan(&tmax); e != nil || tmax != 99 {
		t.Fatalf("the late sample was not rolled up: %v %v", tmax, e)
	}

	// People's tags are never rolled up: no bucket of a roaming tag or of a worn / button profile.
	var personal int
	if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.sample_rollup r WHERE r.tenant_id=$1 AND EXISTS(SELECT 1 FROM core.devices d
     WHERE d.tenant_id=r.tenant_id AND lower(d.external_id)=r.external_id AND d.removed_at IS NULL AND (d.roaming OR d.profile_id = ANY($2::text[])))`,
		st.TenantID, "{"+strings.Join(domain.WornProfileIDs(), ",")+"}").Scan(&personal); e != nil || personal != 0 {
		t.Fatalf("%d buckets of people's tags: %v", personal, e)
	}
	var worn int
	if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.sensor_samples s WHERE s.tenant_id=$1 AND s.received_at >= $2
     AND EXISTS(SELECT 1 FROM core.devices d WHERE d.tenant_id=s.tenant_id AND lower(d.external_id)=lower(s.external_id) AND d.roaming)`, st.TenantID, from).Scan(&worn); e != nil || worn == 0 {
		t.Fatalf("the demo has no samples of worn tags to leave out: %d %v", worn, e)
	}
	// A fixed sensor re-registered as carried (a button profile): a recomputed range drops its buckets.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.devices SET profile_id=$3 WHERE tenant_id=$1 AND lower(external_id)=$2`, st.TenantID, ext, domain.WornProfileIDs()[0]); e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.RollupRange(ctx, st.TenantID, from, to); e != nil {
		t.Fatal(e)
	}
	var left int
	if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.sample_rollup WHERE tenant_id=$1 AND external_id=$2`, st.TenantID, ext).Scan(&left); e != nil || left != 0 {
		t.Fatalf("%d buckets of a tag now carried by someone: %v", left, e)
	}

	// The global worker step runs over every workspace in one pass (and is a no-op for a range already done). It
	// runs only under a statement timeout (its own SET would not arm the calling statement's timer) and with the
	// worn profiles.
	if _, e := f.admin.ExecContext(ctx, `SELECT * FROM core.rollup_samples(interval '6 hours', 1, $1::text[])`, "{"+strings.Join(domain.WornProfileIDs(), ",")+"}"); e == nil || !strings.Contains(e.Error(), "statement_timeout") {
		t.Fatalf("a rollup without a statement timeout: %v", e)
	}
	if _, _, e := f.repo.RollupSamples(ctx, 6*time.Hour); e != nil {
		t.Fatal(e)
	}

	// Backdating is for demo workspaces only, and never into the future.
	_, _, p := f.account(t)
	g, _, e := f.service.CreateGateway(ctx, p, "Real", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.CapturePacketAt(ctx, p.TenantID, g.ID, []byte(s1Temperature), time.Now().Add(-time.Hour)); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("a real workspace accepted a backdated packet: %v", e)
	}
	if _, e := f.repo.CapturePacketAt(ctx, st.TenantID, st.Gateways[0].ID, []byte(s1Temperature), time.Now().Add(time.Hour)); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("a packet from the future: %v", e)
	}
	if _, e := f.repo.ResolveDemoAlertsBefore(ctx, p.TenantID, time.Now()); !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("resolving a real workspace's alerts: %v", e)
	}
	// And the database refuses it too: the runtime role cannot store a sample received more than 15 minutes before
	// its transaction for a real workspace, but can for the demo's.
	if _, e := f.repo.CapturePacket(ctx, p.TenantID, g.ID, []byte(s1Temperature)); e != nil { // creates the stream
		t.Fatal(e)
	}
	backdate := func(tenant, gateway, external string) error {
		tx, e := f.runtime.Begin()
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e := tx.Exec(`SELECT set_config('app.tenant_id',$1,true),set_config('app.user_id','',true)`, tenant); e != nil {
			t.Fatal(e)
		}
		if _, e := tx.Exec(`SELECT set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`); e != nil {
			t.Fatal(e)
		}
		_, e = tx.Exec(`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading)
       VALUES($1,$2,$3,'backdated',now()-interval '1 hour','minew-ffe1-a101@1','{}')`, tenant, gateway, external)
		return e
	}
	var stream string
	if e := f.admin.QueryRowContext(ctx, `SELECT external_id FROM core.sensor_streams WHERE gateway_id=$1 LIMIT 1`, g.ID).Scan(&stream); e != nil {
		t.Fatal(e)
	}
	if e := backdate(p.TenantID, g.ID, stream); e == nil || !strings.Contains(e.Error(), "backdated") {
		t.Fatalf("a real workspace stored a backdated sample: %v", e)
	}
	if e := f.admin.QueryRowContext(ctx, `SELECT gateway_id::text, external_id FROM core.sensor_streams WHERE tenant_id=$1 LIMIT 1`, st.TenantID).Scan(&gateway, &stream); e != nil {
		t.Fatal(e)
	}
	if e := backdate(st.TenantID, gateway, stream); e != nil {
		t.Fatalf("the demo's backdated sample: %v", e)
	}
}

// The new tables are partitioned and kept like the others, readable only in scope, and erased with the tag.
func TestTwinHistoryPartitionsScopeAndErasure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	steady(t, f.admin)
	health, e := f.repo.PartitionHealth(ctx)
	if e != nil {
		t.Fatal(e)
	}
	seen := map[string]bool{}
	for _, h := range health {
		seen[h.Parent] = true
		if (h.Parent == "presence_history" || h.Parent == "sample_rollup") && (h.CoveredUntil == nil || h.CoveredUntil.Before(time.Now().Add(7*24*time.Hour)) || h.DefaultRows != 0) {
			t.Fatalf("unhealthy %+v", h)
		}
	}
	if !seen["presence_history"] || !seen["sample_rollup"] {
		t.Fatalf("health does not report the twin tables: %+v", health)
	}
	// A recent window reads the newest range and DEFAULT, not every month.
	for _, q := range []string{
		`EXPLAIN SELECT * FROM core.presence_history WHERE at > now() - interval '1 hour' AND at <= now()`,
		`EXPLAIN SELECT * FROM core.sample_rollup WHERE bucket >= now() - interval '1 hour' AND bucket < now()`,
	} {
		rows, e := f.admin.QueryContext(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			rows.Scan(&line)
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if n := strings.Count(plan.String(), "_p20"); n < 1 || n > 2 {
			t.Fatalf("no pruning (%d ranges scanned):\n%s", n, plan.String())
		}
	}

	// Scope: a member restricted to project A sees a movement when either end is one of A's gateways.
	_, auth, p := f.account(t)
	api := httpapi.New(f.cfg, f.service, f.repo)
	projA, projB := uuid.NewString(), uuid.NewString()
	gA, gB, gC := uuid.NewString(), uuid.NewString(), uuid.NewString()
	// Registered in upper case (as a label prints it); the history tables keep ids in lower case.
	tag, registered := "c30000aa0001", "C30000AA0001"
	device := uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO core.projects(id,tenant_id,name) VALUES($2,$1,'A'),($3,$1,'B')`, []any{p.TenantID, projA, projB}},
		{`INSERT INTO core.gateways(id,tenant_id,name,model,token_hash,project_id) VALUES($4,$1,'GA','minew-mg3','x',$2),($5,$1,'GB','minew-mg3','x',$3),($6,$1,'GC','minew-mg3','x',$3)`,
			[]any{p.TenantID, projA, projB, gA, gB, gC}},
		{`INSERT INTO core.sensor_streams(tenant_id,gateway_id,external_id,name,last_seen) VALUES($1,$2,'` + tag + `','W',now())`, []any{p.TenantID, gA}},
		{`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id,roaming) VALUES($3,$1,$2,'W','` + registered + `','minew-b10',true)`, []any{p.TenantID, gA, device}},
		{`INSERT INTO core.presence_history(tenant_id,external_id,at,gateway_id,from_gateway_id) VALUES
       ($1,'` + tag + `',now()-interval '30 minutes',$2,NULL),($1,'` + tag + `',now()-interval '20 minutes',$3,$2),
       ($1,'` + tag + `',now()-interval '10 minutes',$4,$3),($1,'` + tag + `',now()-interval '5 minutes',NULL,$4)`, []any{p.TenantID, gA, gB, gC}},
		// Buckets left from a time the tag was not registered as worn (the rollup skips it now): erased with it.
		{`INSERT INTO core.sample_rollup(tenant_id,gateway_id,external_id,bucket,n) VALUES($1,$2,'` + tag + `',date_bin('5 minutes',now()-interval '1 day','2000-01-01'),3),
       ($1,$3,'` + tag + `',date_bin('5 minutes',now()-interval '1 day','2000-01-01'),2)`, []any{p.TenantID, gA, gB}},
		{`INSERT INTO core.twin_settings(tenant_id,people_replay,people_replay_days,display_people) VALUES($1,'named',30,'counts')`, []any{p.TenantID}},
	} {
		if _, e := f.admin.ExecContext(ctx, q.sql, q.args...); e != nil {
			t.Fatalf("%s: %v", q.sql, e)
		}
	}
	memberA := memberEmail()
	memberID := addMember(t, api, auth.AccessToken, memberA, "operator", []string{projA})
	visible := func(user string) int {
		tx, e := f.runtime.Begin()
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e := tx.Exec(`SELECT set_config('app.tenant_id',$1,true),set_config('app.user_id',$2,true)`, p.TenantID, user); e != nil {
			t.Fatal(e)
		}
		if _, e := tx.Exec(`SELECT set_config('app.project_scope',coalesce(core.compute_project_scope(),''),true)`); e != nil {
			t.Fatal(e)
		}
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM core.presence_history WHERE external_id=$1`, tag).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if n := visible(p.UserID); n != 4 {
		t.Fatalf("the owner sees %d of 4 movements", n)
	}
	if n := visible(memberID); n != 2 { // into A, and A -> B
		t.Fatalf("the project A member sees %d movements, want 2", n)
	}
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.presence_history(tenant_id,external_id,at,gateway_id) VALUES($1,$2,now(),$3)`, p.TenantID, registered, gA); e == nil {
		t.Fatal("an upper-case id stored in the movement history")
	}
	// The trail of that member: both moves, but the zone in project B is not named to them.
	operator, _ := changeInitialPassword(t, f, api, memberA, p.TenantID)
	code, body, _ := getRaw(t, api, "/api/v1/twin/people/"+tag+"/trail?"+window(time.Now().Add(-time.Hour), time.Now()), operator.AccessToken, nil)
	var trail struct {
		Points []struct {
			GatewayID *string `json:"gateway_id"`
		} `json:"points"`
	}
	json.Unmarshal(body, &trail)
	if code != 200 || len(trail.Points) != 2 || trail.Points[0].GatewayID == nil || *trail.Points[0].GatewayID != gA || trail.Points[1].GatewayID != nil {
		t.Fatalf("scoped trail: %d %s", code, body)
	}
	if _, e := f.runtime.ExecContext(ctx, `INSERT INTO core.sample_rollup(tenant_id,gateway_id,external_id,bucket,n) VALUES($1,$2,'x',now(),1)`, p.TenantID, gA); e == nil {
		t.Fatal("the runtime wrote a rollup row directly")
	}

	// Export and erasure of the tag: the movement history is in the export and goes with the erasure.
	lines := 0
	if _, e := f.repo.StreamIdentityHistory(ctx, p, registered, "presence_history", 100, func([]byte) error { lines++; return nil }); e != nil || lines != 4 {
		t.Fatalf("export has %d movement lines: %v", lines, e)
	}
	code, out, _ := req(t, api, "POST", "/api/v1/devices/"+device+"/erase-history", "Bearer "+auth.AccessToken, "", origin, map[string]any{"tenant_wide": true})
	if code != 200 {
		t.Fatalf("erase: %d %v", code, out)
	}
	counts, _ := out["counts"].(map[string]any)
	if counts["presence_history"] != float64(4) || counts["sample_rollup"] != float64(2) {
		t.Fatalf("erasure counts: %v", out)
	}
	var left int
	f.admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM core.presence_history WHERE tenant_id=$1 AND external_id=$2)
     + (SELECT count(*) FROM core.sample_rollup WHERE tenant_id=$1 AND external_id=$2)`, p.TenantID, tag).Scan(&left)
	if left != 0 {
		t.Fatalf("%d movement / bucket rows left after the erasure", left)
	}
	// A restore's reapply-erasures redoes the ledger entry with the same function: rows restored from a backup go.
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.presence_history(tenant_id,external_id,at,gateway_id) VALUES($1,$2,now()-interval '2 minutes',$3)`, p.TenantID, tag, gA); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.sample_rollup(tenant_id,gateway_id,external_id,bucket,n) VALUES($1,$2,$3,date_bin('5 minutes',now()-interval '2 days','2000-01-01'),1)`, p.TenantID, gA, tag); e != nil {
		t.Fatal(e)
	}
	var subject string
	var project *string
	if e := f.admin.QueryRowContext(ctx, `SELECT subject_ref, scope->>'project_id' FROM core.erasure_log WHERE tenant_id=$1 ORDER BY at DESC LIMIT 1`, p.TenantID).Scan(&subject, &project); e != nil {
		t.Fatal(e)
	}
	if subject != registered {
		t.Fatalf("ledger subject %q", subject)
	}
	var again string
	if e := f.admin.QueryRowContext(ctx, `SELECT core.erase_identity_data($1,$2,$3::uuid,true)::text`, p.TenantID, subject, project).Scan(&again); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(again, `"presence_history": 1`) || !strings.Contains(again, `"sample_rollup": 1`) {
		t.Fatalf("reapplied erasure: %s", again)
	}
}

// The movement history expires row by row at presence_days, and by whole ranges without waiting for a newer row of
// its own (it is personal data), as long as some uplink in the last week corroborates the clock. Staged in a
// transaction that rolls back.
func TestTwinHistoryRetention(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	steady(t, f.admin)
	_, _, a := f.account(t)
	g, _, e := f.service.CreateGateway(ctx, a, "Retention", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Temperature)); e != nil { // an uplink now
		t.Fatal(e)
	}
	tx, e := f.admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := tx.Exec(q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	var days int
	if e := tx.QueryRow(`SELECT core.presence_history_days()`).Scan(&days); e != nil {
		t.Fatal(e)
	}
	exec(`INSERT INTO core.presence_history(tenant_id,external_id,at,gateway_id) VALUES
     ($1,'c3retention01',now()-make_interval(days => $3)-interval '1 hour',$2),($1,'c3retention01',now()-make_interval(days => $3)+interval '1 hour',$2)`, a.TenantID, g.ID, days)
	// An old month, whose newest row is older than anything: the other tables would hold it for want of newer data.
	exec(`CREATE TABLE core.presence_history_p20000101 (LIKE core.presence_history INCLUDING CONSTRAINTS)`)
	exec(`ALTER TABLE core.presence_history_p20000101 OWNER TO aether_owner`)
	exec(`ALTER TABLE core.presence_history ATTACH PARTITION core.presence_history_p20000101 FOR VALUES FROM ('2000-01-01 00:00:00+00') TO ('2000-02-01 00:00:00+00')`)
	exec(`INSERT INTO core.presence_history(tenant_id,external_id,at,gateway_id) VALUES($1,'c3retention01','2000-01-15',$2)`, a.TenantID, g.ID)
	kept := func() int {
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM core.presence_history WHERE external_id='c3retention01' AND at > '2001-01-01'`).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}

	// No uplink in the last week (a stopped site, or a clock that jumped ahead): nothing personal expires.
	exec(`CREATE TEMP TABLE held_samples AS SELECT * FROM core.sensor_samples WHERE received_at > now() - interval '8 days'`)
	exec(`CREATE TEMP TABLE held_ble AS SELECT * FROM core.ble_history WHERE received_at > now() - interval '8 days'`)
	exec(`DELETE FROM core.sensor_samples WHERE received_at > now() - interval '8 days'`)
	exec(`DELETE FROM core.ble_history WHERE received_at > now() - interval '8 days'`)
	if _, e := tx.Exec(`SELECT core.prune_history(90,24,20000)`); e != nil {
		t.Fatal(e)
	}
	if n := kept(); n != 2 {
		t.Fatalf("an uncorroborated clock expired movement rows: %d of 2 left", n)
	}
	if steps := maintain(t, tx, 3650, 720, true); !slices.Contains(steps, "drop_held core.presence_history_p20000101") {
		t.Fatalf("an uncorroborated clock: want the old month held, got %v", steps)
	}

	// With uplinks: the expired row goes, the other stays; the old month is dropped although nothing newer is in it.
	exec(`INSERT INTO core.sensor_samples SELECT * FROM held_samples`)
	exec(`INSERT INTO core.ble_history SELECT * FROM held_ble`)
	if _, e := tx.Exec(`SELECT core.prune_history(90,24,20000)`); e != nil {
		t.Fatal(e)
	}
	if n := kept(); n != 1 {
		t.Fatalf("after pruning %d of the 2 recent rows are left, want the one within %d days", n, days)
	}
	if steps := maintain(t, tx, 3650, 720, true); !slices.Contains(steps, "dropped core.presence_history_p20000101") {
		t.Fatalf("want the old month dropped, got %v", steps)
	}
}

// replayState is the state at T rebuilt the way the browser does it: the keyframe, then every change up to T.
func replayState(t *testing.T, people map[string]any, at time.Time) map[string]*string {
	key := people["key"].(map[string]any)
	out := map[string]*string{}
	for pid, g := range key {
		if s, ok := g.(string); ok {
			out[pid] = &s
		} else {
			out[pid] = nil
		}
	}
	for _, raw := range people["deltas"].([]any) {
		d := raw.([]any)
		if int64(d[0].(float64)) > at.UnixMilli() {
			break
		}
		if s, ok := d[2].(string); ok {
			out[d[1].(string)] = &s
		} else {
			out[d[1].(string)] = nil
		}
	}
	return out
}

func TestTwinReplay(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	api := httpapi.New(f.cfg, f.service, f.repo)
	st := demoWorkspace(t, f)
	b := simulation.TwinDemo()
	to := time.Now().Add(-15 * time.Minute).Truncate(5 * time.Minute)
	from := to.Add(-35 * time.Minute)
	if _, e := demotwin.Backfill(ctx, f.repo, st, from.Add(-5*time.Minute), to, 30*time.Second, nil); e != nil {
		t.Fatal(e)
	}
	owner, ownerP := signIn(t, f, st.OwnerEmail, st.Password, st.TenantID)
	base := "/api/v1/twin/sites/" + st.SiteID

	// Named (the demo's settings allow it for the owner): the state at a random T equals the database's.
	code, body, _ := getRaw(t, api, base+"/replay?"+window(from, to)+"&people=named", owner.AccessToken, nil)
	if code != 200 {
		t.Fatalf("replay: %d %s", code, body)
	}
	var out map[string]any
	json.Unmarshal(body, &out)
	if out["people_mode"] != "named" || out["bucket_sec"] != float64(300) {
		t.Fatalf("replay head: %v %v", out["people_mode"], out["bucket_sec"])
	}
	env := out["env"].(map[string]any)
	if env["buckets"] != float64(7) || len(env["series"].(map[string]any)) < 40 {
		t.Fatalf("env: %v buckets, %d series", env["buckets"], len(env["series"].(map[string]any)))
	}
	people := out["people"].(map[string]any)
	if len(people["key"].(map[string]any)) != len(b.People) || len(people["deltas"].([]any)) == 0 {
		t.Fatalf("people: %d in the keyframe, %d changes", len(people["key"].(map[string]any)), len(people["deltas"].([]any)))
	}
	gateways := map[string]bool{}
	for _, g := range st.Gateways {
		gateways[g.ID] = true
	}
	rng := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 12; i++ {
		at := from.Add(time.Duration(rng.Int64N(int64(to.Sub(from)))))
		got := replayState(t, people, at)
		for _, p := range b.People {
			var direct *string
			var g *string
			e := f.admin.QueryRowContext(ctx, `SELECT gateway_id::text FROM core.presence_history WHERE tenant_id=$1 AND external_id=$2 AND at <= $3 ORDER BY at DESC LIMIT 1`, st.TenantID, p.MAC, at).Scan(&g)
			if e == nil && g != nil && gateways[*g] {
				direct = g
			}
			have := got[p.MAC]
			if (direct == nil) != (have == nil) || (direct != nil && *direct != *have) {
				t.Fatalf("%s at %s: replay %v, database %v", p.Name, at.Format(time.RFC3339), have, direct)
			}
		}
	}
	names := people["names"].(map[string]any)
	if len(names) != len(b.People) {
		t.Fatalf("names: %d", len(names))
	}
	alerts := out["alerts"].(map[string]any)
	sos := false
	for _, raw := range alerts["items"].([]any) {
		if raw.(map[string]any)["sos"] == true {
			sos = true
		}
	}
	if !sos {
		t.Fatalf("no SOS in the replayed alerts: %v", alerts)
	}

	// counts: no tag id, no name, anywhere in the response; headcounts only.
	code, body, etag := getRaw(t, api, base+"/replay?"+window(from, to), owner.AccessToken, nil)
	if code != 200 || etag == "" {
		t.Fatalf("counts: %d", code)
	}
	for _, p := range b.People {
		if strings.Contains(string(body), p.MAC) || strings.Contains(string(body), p.Name) {
			t.Fatalf("counts mode names a wearer (%s)", p.Name)
		}
	}
	json.Unmarshal(body, &out)
	pm := out["people"].(map[string]any)
	if pm["mode"] != "counts" || pm["key"] != nil || len(pm["key_counts"].(map[string]any)) == 0 {
		t.Fatalf("counts people: %v", pm)
	}
	// Headcounts change per bucket, at the bucket's end: one net change per zone and bucket, never the exact time of
	// a move (a -1 here and a +1 there at the same millisecond would be one person walking).
	step := int64(out["bucket_sec"].(float64)) * 1000
	seen := map[string]bool{}
	deltas := pm["count_deltas"].([]any)
	if len(deltas) == 0 {
		t.Fatal("no headcount changes in the window")
	}
	for _, raw := range deltas {
		d := raw.([]any)
		at, g, n := int64(d[0].(float64)), d[1].(string), d[2].(float64)
		k := (at - from.UnixMilli()) / step
		if (at-from.UnixMilli())%step != 0 || k < 1 || at > to.UnixMilli() || n == 0 || seen[fmt.Sprint(at, g)] {
			t.Fatalf("headcount change %v: not one net change at a bucket end", d)
		}
		seen[fmt.Sprint(at, g)] = true
	}
	// The headcount at every bucket end is the database's.
	for k := int64(1); k <= int64(to.Sub(from)/time.Millisecond)/step; k++ {
		at := time.UnixMilli(from.UnixMilli() + k*step)
		want := map[string]int{}
		for _, p := range b.People {
			var g *string
			if e := f.admin.QueryRowContext(ctx, `SELECT gateway_id::text FROM core.presence_history WHERE tenant_id=$1 AND external_id=$2 AND at <= $3 ORDER BY at DESC LIMIT 1`, st.TenantID, p.MAC, at).Scan(&g); e == nil && g != nil && gateways[*g] {
				want[*g]++
			}
		}
		got := map[string]int{}
		for g, n := range pm["key_counts"].(map[string]any) {
			got[g] = int(n.(float64))
		}
		for _, raw := range deltas {
			d := raw.([]any)
			if int64(d[0].(float64)) <= at.UnixMilli() {
				got[d[1].(string)] += int(d[2].(float64))
			}
		}
		for g := range gateways {
			if got[g] != want[g] {
				t.Fatalf("headcount of %s at %s: replay %d, database %d", g, at.Format(time.RFC3339), got[g], want[g])
			}
		}
	}
	// The same past window again: served from the cache (a row written into the window since is not in it) and 304
	// to a client that has it.
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.presence_history(tenant_id,external_id,at,gateway_id) VALUES($1,$2,$3,$4)`,
		st.TenantID, b.People[0].MAC, from.Add(90*time.Second), st.Gateways[len(st.Gateways)-1].ID); e != nil {
		t.Fatal(e)
	}
	if code, _, again := getRaw(t, api, base+"/replay?"+window(from, to), owner.AccessToken, nil); code != 200 || again != etag {
		t.Fatalf("second request: %d %s vs %s", code, again, etag)
	}
	if code, _, _ := getRaw(t, api, base+"/replay?"+window(from, to), owner.AccessToken, map[string]string{"If-None-Match": etag}); code != 304 {
		t.Fatalf("If-None-Match: %d", code)
	}
	// Another scope is another cache entry: a member of the demo project computes the window afresh.
	operatorEmail := memberEmail()
	addMember(t, api, owner.AccessToken, operatorEmail, "operator", []string{st.ProjectID})
	operator, _ := changeInitialPassword(t, f, api, operatorEmail, st.TenantID)
	if code, _, other := getRaw(t, api, base+"/replay?"+window(from, to), operator.AccessToken, nil); code != 200 || other == etag {
		t.Fatalf("a scoped member got the owner's cached window: %d", code)
	}
	// An erasure is never behind a cached window.
	var device string
	if e := f.admin.QueryRowContext(ctx, `SELECT id::text FROM core.devices WHERE tenant_id=$1 AND lower(external_id)=$2`, st.TenantID, b.People[1].MAC).Scan(&device); e != nil {
		t.Fatal(e)
	}
	if code, out, _ := req(t, api, "POST", "/api/v1/devices/"+device+"/erase-history", "Bearer "+owner.AccessToken, "", origin, map[string]any{}); code != 200 {
		t.Fatalf("erase: %d %v", code, out)
	}
	if code, _, after := getRaw(t, api, base+"/replay?"+window(from, to), owner.AccessToken, nil); code != 200 || after == etag {
		t.Fatalf("an erasure left the cached window in place: %d", code)
	}

	// Access log: each replay with people (tracks) is its own row.
	logged := func() int {
		var n int
		f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.access_log WHERE tenant_id=$1 AND resource='twin_replay_people'`, st.TenantID).Scan(&n)
		return n
	}
	before := logged()
	for i := 0; i < 2; i++ {
		code, body, h := getWithHeaders(t, api, base+"/replay?"+window(from, to)+"&people=tracks", owner.AccessToken, nil)
		if code != 200 || h.Get("Cache-Control") != "no-store" {
			t.Fatalf("tracks: %d %s %q", code, body, h.Get("Cache-Control"))
		}
	}
	if _, _, h := getWithHeaders(t, api, base+"/replay?"+window(from, to), owner.AccessToken, nil); h.Get("Cache-Control") != "private, max-age=60" {
		t.Fatalf("counts Cache-Control %q", h.Get("Cache-Control"))
	}
	if logged() != before+2 {
		t.Fatalf("tracks replays logged %d times, want 2", logged()-before)
	}

	// A viewer never gets names; an owner's settings cap everyone.
	viewerEmail := memberEmail()
	addMember(t, api, owner.AccessToken, viewerEmail, "viewer", nil)
	viewer, _ := changeInitialPassword(t, f, api, viewerEmail, st.TenantID)
	code, body, _ = getRaw(t, api, base+"/replay?"+window(from, to)+"&people=named", viewer.AccessToken, nil)
	json.Unmarshal(body, &out)
	if code != 200 || out["people_mode"] != "tracks" {
		t.Fatalf("viewer asking names: %d %v", code, out["people_mode"])
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/twin/settings", "Bearer "+viewer.AccessToken, "", origin, map[string]any{"people_replay": "named", "people_replay_days": 7, "display_people": "counts"}); code != 403 {
		t.Fatalf("a viewer changed the twin settings: %d", code)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/twin/settings", "Bearer "+owner.AccessToken, "", origin, map[string]any{"people_replay": "everyone", "people_replay_days": 7, "display_people": "counts"}); code != 400 {
		t.Fatalf("invalid settings: %d", code)
	}
	code, set, _ := req(t, api, "POST", "/api/v1/twin/settings", "Bearer "+owner.AccessToken, "", origin, map[string]any{"people_replay": "counts", "people_replay_days": 7, "display_people": "counts"})
	if code != 200 || set["people_replay"] != "counts" || set["stored"] != true {
		t.Fatalf("settings: %d %v", code, set)
	}
	var audited int
	f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action LIKE 'twin.settings_changed:%'`, st.TenantID).Scan(&audited)
	if audited != 1 {
		t.Fatalf("settings change audited %d times", audited)
	}
	code, body, _ = getRaw(t, api, base+"/replay?"+window(from, to)+"&people=named", owner.AccessToken, nil)
	json.Unmarshal(body, &out)
	if code != 200 || out["people_mode"] != "counts" {
		t.Fatalf("the owner asked names under counts settings: %d %v", code, out["people_mode"])
	}
	// The trail of one person needs names too.
	if code, _, _ := getRaw(t, api, "/api/v1/twin/people/"+b.People[2].MAC+"/trail?"+window(from, to), owner.AccessToken, nil); code != 403 {
		t.Fatalf("trail under counts settings: %d", code)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/twin/settings", "Bearer "+owner.AccessToken, "", origin, map[string]any{"people_replay": "named", "people_replay_days": 7, "display_people": "counts"}); code != 200 {
		t.Fatal("settings back to named")
	}
	code, body, _ = getRaw(t, api, "/api/v1/twin/people/"+b.People[2].MAC+"/trail?"+window(from, to), owner.AccessToken, nil)
	var trail struct {
		Points []map[string]any `json:"points"`
	}
	json.Unmarshal(body, &trail)
	if code != 200 || len(trail.Points) == 0 {
		t.Fatalf("trail: %d %s", code, body)
	}
	if code, _, _ := getRaw(t, api, "/api/v1/twin/people/"+b.People[2].MAC+"/trail?"+window(from, to), viewer.AccessToken, nil); code != 403 {
		t.Fatalf("a viewer's trail: %d", code)
	}
	var trails int
	f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.access_log WHERE tenant_id=$1 AND resource='presence_history' AND subject_id=$2`, st.TenantID, b.People[2].MAC).Scan(&trails)
	if trails != 1 {
		t.Fatalf("trail reads logged %d times", trails)
	}

	// Timeline: the SOS is a marker; counts labels name nobody; density lanes cover the window.
	code, body, _ = getRaw(t, api, base+"/timeline?"+window(from, to), operator.AccessToken, nil)
	var tl domain.TwinTimeline
	json.Unmarshal(body, &tl)
	if code != 200 || tl.PeopleMode != "counts" || tl.Available.PeopleFrom == nil || len(tl.Density.Lanes["zone"]) != 7 {
		t.Fatalf("timeline: %d %s", code, body)
	}
	hasSOS := false
	for _, m := range tl.Markers {
		if m.Kind == "sos" {
			hasSOS = true
		}
		for _, p := range b.People {
			if strings.Contains(m.Label, p.Name) {
				t.Fatalf("a counts marker names %s", p.Name)
			}
		}
	}
	if !hasSOS {
		t.Fatalf("no SOS marker: %+v", tl.Markers)
	}

	// A timeline with names is logged on every request, as is the live view with names.
	timelines := func(resource string) int {
		var n int
		f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.access_log WHERE tenant_id=$1 AND resource=$2`, st.TenantID, resource).Scan(&n)
		return n
	}
	beforeTL, beforeLive := timelines("twin_timeline_named"), timelines("twin_live_named")
	for i := 0; i < 2; i++ {
		if code, body, _ := getRaw(t, api, base+"/timeline?"+window(from, to)+"&people=named", owner.AccessToken, nil); code != 200 {
			t.Fatalf("named timeline: %d %s", code, body)
		}
		if code, body, _ := getRaw(t, api, base+"/state?people=named", owner.AccessToken, nil); code != 200 {
			t.Fatalf("named state: %d %s", code, body)
		}
	}
	if timelines("twin_timeline_named") != beforeTL+2 || timelines("twin_live_named") != beforeLive+2 {
		t.Fatalf("named timeline / live logged %d / %d times, want 2 each", timelines("twin_timeline_named")-beforeTL, timelines("twin_live_named")-beforeLive)
	}

	// Limits.
	for _, bad := range []string{
		window(to.Add(-8*24*time.Hour), to),
		window(to.Add(-48*time.Hour), to) + "&bucket=300",
		window(from, time.Now().Add(time.Hour)),
		window(to, from),
		window(from, to) + "&bucket=400",
		window(from, to) + "&layers=env,secrets",
	} {
		if code, body, _ := getRaw(t, api, base+"/replay?"+bad, owner.AccessToken, nil); code != 400 {
			t.Fatalf("%s: %d %s", bad, code, body)
		}
	}
	// Another site's crowd costs this site nothing: 20001 moves at a gateway not placed here are not read.
	elsewhere := uuid.NewString()
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.gateways(id,tenant_id,name,model,token_hash,project_id) VALUES($1,$2,'Elsewhere','minew-mg3','x',$3)`, elsewhere, st.TenantID, st.ProjectID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.presence_history(tenant_id,external_id,at,gateway_id)
     SELECT $1,'c3crowd'||(i % 50),$2::timestamptz + make_interval(secs => i * 0.05),$3 FROM generate_series(1, 20001) i ON CONFLICT DO NOTHING`,
		st.TenantID, from.Add(time.Minute), elsewhere); e != nil {
		t.Fatal(e)
	}
	// (Only the people layer: a response of its own, not the cached window the 413 below must not come from.)
	if code, body, _ := getRaw(t, api, base+"/replay?"+window(from, to)+"&people=named&layers=people", owner.AccessToken, nil); code != 200 {
		t.Fatalf("another site's moves made this window too dense: %d %s", code, body)
	}
	// A window with more people changes than one response carries: 413 with a shorter end to ask for.
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.presence_history(tenant_id,external_id,at,gateway_id)
     SELECT $1,'c3dense000001',$2::timestamptz + make_interval(secs => i * 0.05),$3 FROM generate_series(1, 20001) i ON CONFLICT DO NOTHING`,
		st.TenantID, from.Add(time.Minute), st.Gateways[0].ID); e != nil {
		t.Fatal(e)
	}
	code, body, _ = getRaw(t, api, base+"/replay?"+window(from, to)+"&people=named", owner.AccessToken, nil)
	var dense map[string]any
	json.Unmarshal(body, &dense)
	if code != 413 || dense["error"] != "too_dense" || dense["hint_to"] == nil {
		t.Fatalf("dense window: %d %s", code, body)
	}
	_ = ownerP
}
