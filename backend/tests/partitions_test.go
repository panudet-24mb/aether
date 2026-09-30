package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/simulation"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Minew S1 frames captured on the production MG3 (see thinning_test.go).
const (
	s1Temperature = `[{"mac":"c30000393fe5","rawData":"0201060303E1FF1016E1FFA1016419F8394AE53F390000C3"}]`
	s1Motion      = `[{"mac":"c30000393fe5","rawData":"0201060303e1ff1216e1ffa103640007ff0bffbee53f390000c3"}]`
)

type queryer interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func explain(t *testing.T, tx *sql.Tx, query string, args ...any) string {
	t.Helper()
	rows, e := tx.Query(`EXPLAIN (COSTS OFF) `+query, args...)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if e := rows.Scan(&line); e != nil {
			t.Fatal(e)
		}
		b.WriteString(line + "\n")
	}
	if e := rows.Err(); e != nil {
		t.Fatal(e)
	}
	return b.String()
}

func count(t *testing.T, db interface {
	QueryRow(string, ...any) *sql.Row
}, query string, args ...any) int {
	t.Helper()
	var n int
	if e := db.QueryRow(query, args...).Scan(&n); e != nil {
		t.Fatalf("%s: %v", query, e)
	}
	return n
}

// maintain calls core.maintain_partitions once and returns its rows as "action relation".
func maintain(t *testing.T, q queryer, days, hours int, allowDrop bool) []string {
	t.Helper()
	rows, e := q.Query(`SELECT action,relation FROM core.maintain_partitions($1,$2,$3)`, days, hours, allowDrop)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var action, relation string
		if e := rows.Scan(&action, &relation); e != nil {
			t.Fatal(e)
		}
		out = append(out, action+" "+relation)
	}
	if e := rows.Err(); e != nil {
		t.Fatal(e)
	}
	return out
}

// steady brings the partitions to their steady state (every range ahead created, nothing dropped), so a test run
// just after midnight UTC, when a new BLE day falls into the look-ahead, starts from the same place as any other.
func steady(t *testing.T, q queryer) {
	t.Helper()
	for i := 0; i < 60; i++ {
		steps := maintain(t, q, 3650, 720, false)
		created := false
		for _, s := range steps {
			created = created || strings.HasPrefix(s, "created ")
		}
		if !created {
			return
		}
	}
	t.Fatal("partition maintenance did not settle")
}

func partitionsOf(t *testing.T, q queryer, parent string) (names []string, lo, hi []*time.Time) {
	t.Helper()
	rows, e := q.Query(`SELECT part::text,lo,hi FROM core.partition_ranges($1) WHERE NOT is_default ORDER BY hi`, parent)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var l, h *time.Time
		if e := rows.Scan(&n, &l, &h); e != nil {
			t.Fatal(e)
		}
		names, lo, hi = append(names, n), append(lo, l), append(hi, h)
	}
	return names, lo, hi
}

// A QoS 1 redelivery arrives with a new receive time; with received_at in the key (migration 00037) the event key
// within RedeliveryWindow is what keeps it from being stored twice, for decoded samples and raw BLE rows alike.
// After the window, the same payload is a new delivery (before 00037 it was deduplicated forever).
func TestRedeliveredPacketIsStoredOnce(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Redelivery", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		for _, p := range []string{s1Temperature, s1Motion} {
			if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(p)); e != nil {
				t.Fatal(e)
			}
		}
		time.Sleep(5 * time.Millisecond) // a later receive time each round
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1`, g.ID); n != 2 {
		t.Fatalf("samples after three deliveries of two packets: %d, want 2", n)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.ble_history WHERE gateway_id=$1`, g.ID); n != 2 {
		t.Fatalf("BLE rows after three deliveries of two packets: %d, want 2", n)
	}
	// Outside the window the same key is a new delivery, for both tables.
	for _, table := range []string{"core.sensor_samples", "core.ble_history"} {
		if _, e := f.admin.ExecContext(ctx, `UPDATE `+table+` SET received_at=received_at-interval '25 hours' WHERE gateway_id=$1`, g.ID); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Temperature)); e != nil {
		t.Fatal(e)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1`, g.ID); n != 3 {
		t.Fatalf("a delivery after the window: %d samples, want 3", n)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.ble_history WHERE gateway_id=$1`, g.ID); n != 3 {
		t.Fatalf("a delivery after the window: %d BLE rows, want 3", n)
	}
}

// Two deliveries of one packet at the same moment: the gateway row lock serialises them, so the second sees the
// first's rows and stores nothing.
func TestConcurrentDeliveriesStoreOnce(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Concurrent", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	for round := 0; round < 5; round++ {
		payload := fmt.Sprintf(`[{"mac":"c30000393fe5","rawData":"0201060303E1FF1016E1FFA1016419F8394AE53F390000C3","sequence":%d}]`, round)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(payload))
				errs <- e
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.ble_history WHERE gateway_id=$1`, g.ID); n != 5 {
		t.Fatalf("BLE rows after 5 rounds of 2 concurrent deliveries: %d, want 5", n)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1`, g.ID); n != 5 {
		t.Fatalf("samples after 5 rounds of 2 concurrent deliveries: %d, want 5", n)
	}
}

// Queries go through the parent, whose policies are the old tables'; a partition cannot be addressed at all.
func TestPartitionsAreReachedOnlyThroughTheParent(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	_, _, b := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Scoped", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Temperature)); e != nil {
		t.Fatal(e)
	}
	as := func(tenant string, fn func(*sql.Tx)) {
		tx, e := f.runtime.Begin()
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e := tx.Exec(`SELECT set_config('app.user_id','',true),set_config('app.tenant_id',$1,true),set_config('app.project_scope','*',true)`, tenant); e != nil {
			t.Fatal(e)
		}
		fn(tx)
	}
	as(a.TenantID, func(tx *sql.Tx) {
		if n := count(t, tx, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1`, g.ID); n != 1 {
			t.Fatalf("owner tenant sees %d samples", n)
		}
	})
	as(b.TenantID, func(tx *sql.Tx) {
		if n := count(t, tx, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1`, g.ID) + count(t, tx, `SELECT count(*) FROM core.ble_history WHERE gateway_id=$1`, g.ID); n != 0 {
			t.Fatalf("another tenant sees %d rows through the parents", n)
		}
	})
	var parts []string
	rows, e := f.admin.Query(`SELECT inhrelid::regclass::text FROM pg_inherits WHERE inhparent IN ('core.sensor_samples'::regclass,'core.ble_history'::regclass)`)
	if e != nil {
		t.Fatal(e)
	}
	for rows.Next() {
		var p string
		if e := rows.Scan(&p); e != nil {
			t.Fatal(e)
		}
		parts = append(parts, p)
	}
	rows.Close()
	if len(parts) < 6 {
		t.Fatalf("partitions: %v", parts)
	}
	for _, p := range append(parts, "core.partition_ranges('sensor_samples')", "core.partition_has_rows_between('sensor_samples', now())") {
		as(a.TenantID, func(tx *sql.Tx) {
			_, e := tx.Exec(`SELECT 1 FROM ` + p + ` LIMIT 1`)
			if sqlState(e) != "42501" {
				t.Fatalf("runtime role reading %s directly: %v", p, e)
			}
		})
	}
}

// Each partition is under FORCE RLS: the start-up check (CheckRuntimeRole) counts partitions too.
func TestEveryPartitionForcesRowSecurity(t *testing.T) {
	f := setup(t)
	if n := count(t, f.admin, `SELECT count(*) FROM pg_class c JOIN pg_inherits i ON i.inhrelid=c.oid
    WHERE i.inhparent IN ('core.sensor_samples'::regclass,'core.ble_history'::regclass) AND NOT (c.relrowsecurity AND c.relforcerowsecurity)`); n != 0 {
		t.Fatalf("%d partitions without FORCE RLS", n)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM pg_class c JOIN pg_inherits i ON i.inhrelid=c.oid
    WHERE i.inhparent IN ('core.sensor_samples'::regclass,'core.ble_history'::regclass) AND has_table_privilege('aether_app', c.oid, 'SELECT,INSERT,UPDATE,DELETE')`); n != 0 {
		t.Fatalf("%d partitions grant the runtime role", n)
	}
	// Created partitions keep no helper CHECK behind.
	if n := count(t, f.admin, `SELECT count(*) FROM pg_constraint WHERE connamespace='core'::regnamespace AND conname LIKE '%\_range'`); n != 0 {
		t.Fatalf("%d range CHECKs left on partitions", n)
	}
}

// maintain_partitions: expired ranges go oldest first (a MINVALUE-bounded one included), one per call. An empty
// one always goes; one holding rows only when the newest stored row confirms it expired (drop_held otherwise: a
// clock that jumped ahead must not expire data). A missing range is created, taking the rows DEFAULT held for it.
// Staged in a transaction that rolls back.
func TestMaintainPartitionsDropsAndCreates(t *testing.T) {
	f := setup(t)
	steady(t, f.admin)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Partitions", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Temperature)); e != nil { // creates the stream
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
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 0 {
		t.Fatalf("a maintained table needs nothing, got %v", steps)
	}

	// Two expired ranges, the older one open below (as legacy is) and holding the only row of the table: legacy,
	// with every other row, is detached.
	exec(`ALTER TABLE core.sensor_samples DETACH PARTITION core.sensor_samples_legacy`)
	// Partitions belong to aether_owner, as the ones maintenance creates: the definer functions read them.
	exec(`CREATE TABLE core.sensor_samples_old (LIKE core.sensor_samples)`)
	exec(`ALTER TABLE core.sensor_samples_old OWNER TO aether_owner`)
	exec(`ALTER TABLE core.sensor_samples ATTACH PARTITION core.sensor_samples_old FOR VALUES FROM (MINVALUE) TO ('2000-01-03 00:00:00+00')`)
	exec(`CREATE TABLE core.sensor_samples_p20000103 (LIKE core.sensor_samples)`)
	exec(`ALTER TABLE core.sensor_samples_p20000103 OWNER TO aether_owner`)
	exec(`ALTER TABLE core.sensor_samples ATTACH PARTITION core.sensor_samples_p20000103 FOR VALUES FROM ('2000-01-03 00:00:00+00') TO ('2000-01-10 00:00:00+00')`)
	exec(`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading) VALUES($1,$2,'c30000393fe5','old','1999-12-31','x','{}')`, a.TenantID, g.ID)
	// The newest row is from 1999: by the data, nothing expired. The empty newer range goes anyway.
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 2 || steps[0] != "drop_held core.sensor_samples_old" || steps[1] != "dropped core.sensor_samples_p20000103" {
		t.Fatalf("want old held and the empty range dropped, got %v", steps)
	}
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 1 || steps[0] != "drop_held core.sensor_samples_old" {
		t.Fatalf("want old held again, got %v", steps)
	}
	if steps := maintain(t, tx, 90, 24, false); len(steps) != 0 {
		t.Fatalf("allow_drop=false: want nothing, got %v", steps)
	}
	// An idle system: its newest row is three days old (nothing in the last day), which still proves the 1999 range
	// expired. The row lands in DEFAULT while legacy is detached.
	exec(`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading) VALUES($1,$2,'c30000393fe5','idle',now()-interval '3 days','x','{}')`, a.TenantID, g.ID)
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 1 || steps[0] != "dropped core.sensor_samples_old" {
		t.Fatalf("idle system: want old dropped, got %v", steps)
	}

	// A missing BLE range: its row waits in DEFAULT (and is reported), then moves into the new partition.
	var last string
	var lo time.Time
	if e := tx.QueryRow(`SELECT part::text,lo FROM core.partition_ranges('ble_history') WHERE NOT is_default ORDER BY hi DESC LIMIT 1`).Scan(&last, &lo); e != nil {
		t.Fatal(e)
	}
	exec(`DROP TABLE ` + last)
	exec(`INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source) VALUES($1,$2,'aabbccddeeff','k',$3,'020106','device')`,
		a.TenantID, g.ID, lo.Add(time.Hour))
	var rows int64
	if e := tx.QueryRow(`SELECT default_rows FROM core.partition_health() WHERE parent='ble_history'`).Scan(&rows); e != nil || rows != 1 {
		t.Fatalf("DEFAULT rows reported: %d %v", rows, e)
	}
	var moved int64
	if e := tx.QueryRow(`SELECT moved FROM core.maintain_partitions(90,24,false) WHERE action='created' AND relation=$1`, last).Scan(&moved); e != nil || moved != 1 {
		t.Fatalf("expected %s created with 1 row moved: %d %v", last, moved, e)
	}
	if e := tx.QueryRow(`SELECT default_rows FROM core.partition_health() WHERE parent='ble_history'`).Scan(&rows); e != nil || rows != 0 {
		t.Fatalf("DEFAULT after the move: %d %v", rows, e)
	}
	var home string
	if e := tx.QueryRow(`SELECT tableoid::regclass::text FROM core.ble_history WHERE gateway_id=$1 AND event_key='k'`, g.ID).Scan(&home); e != nil || home != last {
		t.Fatalf("the moved row lives in %s (%v), want %s", home, e, last)
	}
	if n := count(t, tx, `SELECT count(*) FROM pg_class c WHERE c.oid=to_regclass($1) AND c.relrowsecurity AND c.relforcerowsecurity`, last); n != 1 {
		t.Fatal("a created partition lacks FORCE RLS")
	}
	if steps := maintain(t, tx, 90, 24, false); len(steps) != 0 {
		t.Fatalf("nothing left to do, got %v", steps)
	}
}

// The drop guard follows each table's own data. A deployment without BLE (no row at all) still drops its expired,
// empty ranges; ranges that expired only by the clock (the newest row says otherwise, as after a clock that jumped
// ahead) are held, for that table alone; once rows move the anchor on, the oldest goes.
func TestDropGuardFollowsTheData(t *testing.T) {
	f := setup(t)
	steady(t, f.admin)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Guard", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Temperature)); e != nil {
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
	attach := func(parent, name, from, to string) {
		t.Helper()
		exec(fmt.Sprintf(`CREATE TABLE core.%s (LIKE core.%s)`, name, parent))
		exec(fmt.Sprintf(`ALTER TABLE core.%s OWNER TO aether_owner`, name))
		exec(fmt.Sprintf(`ALTER TABLE core.%s ATTACH PARTITION core.%s FOR VALUES FROM ('%s') TO ('%s')`, parent, name, from, to))
	}
	ble := func(event, at string) {
		t.Helper()
		exec(`INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source) VALUES($1,$2,'c30000393fe5',$3,$4,'020106','device')`, a.TenantID, g.ID, event, at)
	}
	// No BLE at all: legacy (with every BLE row of the suite) detached, two expired empty days attached.
	exec(`ALTER TABLE core.ble_history DETACH PARTITION core.ble_history_legacy`)
	attach("ble_history", "ble_history_p20000101", "2000-01-01 00:00:00+00", "2000-01-02 00:00:00+00")
	attach("ble_history", "ble_history_p20000102", "2000-01-02 00:00:00+00", "2000-01-03 00:00:00+00")
	for _, want := range []string{"core.ble_history_p20000101", "core.ble_history_p20000102"} {
		if steps := maintain(t, tx, 90, 24, true); len(steps) != 1 || steps[0] != "dropped "+want {
			t.Fatalf("BLE-less: want %s dropped, got %v", want, steps)
		}
	}
	// Clock jump: every day of January 2000 is expired by now(), but the newest row is from 2000-01-06 06:00, so
	// with 24 h retention nothing is expired by the data. Held for BLE only: the samples table's expired empty
	// range goes in the same call.
	attach("ble_history", "ble_history_p20000105", "2000-01-05 00:00:00+00", "2000-01-06 00:00:00+00")
	attach("ble_history", "ble_history_p20000106", "2000-01-06 00:00:00+00", "2000-01-07 00:00:00+00")
	ble("a", "2000-01-05 12:00:00+00")
	ble("b", "2000-01-06 06:00:00+00")
	exec(`ALTER TABLE core.sensor_samples DETACH PARTITION core.sensor_samples_legacy`)
	attach("sensor_samples", "sensor_samples_p20000103", "2000-01-03 00:00:00+00", "2000-01-10 00:00:00+00")
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 2 || steps[0] != "drop_held core.ble_history_p20000105" || steps[1] != "dropped core.sensor_samples_p20000103" {
		t.Fatalf("clock jump: want BLE held and the samples range dropped, got %v", steps)
	}
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 1 || steps[0] != "drop_held core.ble_history_p20000105" {
		t.Fatalf("still held: %v", steps)
	}
	// A row from 2000-01-07 01:00 moves the anchor: the 01-05 day (ends 01-06, +24 h = 01-07 00:00) is now expired
	// by the data too; 01-06 (ends 01-07, +24 h) is not.
	attach("ble_history", "ble_history_p20000107", "2000-01-07 00:00:00+00", "2000-01-08 00:00:00+00")
	ble("c", "2000-01-07 01:00:00+00")
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 1 || steps[0] != "dropped core.ble_history_p20000105" {
		t.Fatalf("anchor moved: want 01-05 dropped, got %v", steps)
	}
	if steps := maintain(t, tx, 90, 24, true); len(steps) != 1 || steps[0] != "drop_held core.ble_history_p20000106" {
		t.Fatalf("want 01-06 held, got %v", steps)
	}
}

// A run drops at most postgres.MaxDropsPerRun partitions, whatever is expired; the rest wait for the next run.
// This test changes the BLE layout for real (the repository runs its own transactions) and restores it.
func TestMaintenanceDropsAtMostTwoPerRun(t *testing.T) {
	f := setup(t)
	steady(t, f.admin)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Drop cap", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	var cutover time.Time
	if e := f.admin.QueryRow(`SELECT hi FROM core.partition_ranges('ble_history') WHERE part='core.ble_history_legacy'::regclass`).Scan(&cutover); e != nil {
		t.Fatal(e)
	}
	run := func(q string, args ...any) {
		t.Helper()
		if _, e := f.admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	run(`ALTER TABLE core.ble_history DETACH PARTITION core.ble_history_legacy`)
	t.Cleanup(func() {
		for _, q := range []string{
			`DROP TABLE IF EXISTS core.ble_history_old, core.ble_history_p20000101, core.ble_history_p20000102`,
			`ALTER TABLE core.ble_history DETACH PARTITION core.ble_history_legacy`,
		} {
			_, _ = f.admin.ExecContext(ctx, q)
		}
		if _, e := f.admin.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE core.ble_history ATTACH PARTITION core.ble_history_legacy FOR VALUES FROM (MINVALUE) TO ('%s')`, cutover.UTC().Format(time.RFC3339))); e != nil {
			t.Errorf("restoring legacy: %v", e)
		}
	})
	for _, q := range []string{
		`CREATE TABLE core.ble_history_old (LIKE core.ble_history)`,
		`ALTER TABLE core.ble_history_old OWNER TO aether_owner`,
		`ALTER TABLE core.ble_history ATTACH PARTITION core.ble_history_old FOR VALUES FROM (MINVALUE) TO ('2000-01-01 00:00:00+00')`,
		`CREATE TABLE core.ble_history_p20000101 (LIKE core.ble_history)`,
		`ALTER TABLE core.ble_history_p20000101 OWNER TO aether_owner`,
		`ALTER TABLE core.ble_history ATTACH PARTITION core.ble_history_p20000101 FOR VALUES FROM ('2000-01-01 00:00:00+00') TO ('2000-01-02 00:00:00+00')`,
		`CREATE TABLE core.ble_history_p20000102 (LIKE core.ble_history)`,
		`ALTER TABLE core.ble_history_p20000102 OWNER TO aether_owner`,
		`ALTER TABLE core.ble_history ATTACH PARTITION core.ble_history_p20000102 FOR VALUES FROM ('2000-01-02 00:00:00+00') TO ('2000-01-03 00:00:00+00')`,
	} {
		run(q)
	}
	run(fmt.Sprintf(`ALTER TABLE core.ble_history ATTACH PARTITION core.ble_history_legacy FOR VALUES FROM ('2000-01-03 00:00:00+00') TO ('%s')`, cutover.UTC().Format(time.RFC3339)))
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Temperature)); e != nil { // a recent BLE row
		t.Fatal(e)
	}
	dropped := func() []string {
		t.Helper()
		steps, _, e := f.repo.MaintainPartitions(ctx)
		if e != nil {
			t.Fatal(e)
		}
		out := []string{}
		for _, s := range steps {
			if s.Action == "dropped" {
				out = append(out, s.Relation)
			}
		}
		return out
	}
	if d := dropped(); len(d) != postgres.MaxDropsPerRun || d[0] != "core.ble_history_old" || d[1] != "core.ble_history_p20000101" {
		t.Fatalf("first run dropped %v, want the two oldest", d)
	}
	if d := dropped(); len(d) != 1 || d[0] != "core.ble_history_p20000102" {
		t.Fatalf("second run dropped %v", d)
	}
	if d := dropped(); len(d) != 0 {
		t.Fatalf("third run dropped %v", d)
	}
}

// Partition bounds are Monday 00:00 UTC (samples) and midnight UTC (BLE) whatever the session's TimeZone, also
// across a daylight-saving change: '7 days' added across the last Sunday of October (Europe/Berlin) or the first
// Sunday of November (America/New_York) is 169 hours there. Every partition function pins TimeZone=UTC itself.
func TestPartitionBoundsAreUTCInAnyTimeZone(t *testing.T) {
	f := setup(t)
	steady(t, f.admin)
	for _, fn := range []string{"maintain_partitions", "partition_ranges", "partition_has_rows_between", "partition_health", "prune_history"} {
		if n := count(t, f.admin, `SELECT count(*) FROM pg_proc WHERE pronamespace='core'::regnamespace AND proname=$1 AND 'TimeZone=UTC'=ANY(proconfig)`, fn); n != 1 {
			t.Fatalf("core.%s does not pin TimeZone=UTC", fn)
		}
	}
	for _, zone := range []string{"Europe/Berlin", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			tx, e := f.admin.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			if _, e := tx.Exec(`SET LOCAL TimeZone='` + zone + `'`); e != nil {
				t.Fatal(e)
			}
			before := map[string]int{}
			for _, parent := range []string{"sensor_samples", "ble_history"} {
				names, _, _ := partitionsOf(t, tx, parent)
				before[parent] = len(names)
				// Drop every range after legacy and let maintenance, called from this session, rebuild them.
				for _, n := range names[1:] {
					if _, e := tx.Exec(`DROP TABLE ` + n); e != nil {
						t.Fatal(e)
					}
				}
			}
			steady(t, tx)
			for parent, unit := range map[string]time.Duration{"sensor_samples": 7 * 24 * time.Hour, "ble_history": 24 * time.Hour} {
				names, lo, hi := partitionsOf(t, tx, parent)
				if len(names) != before[parent] {
					t.Fatalf("%s: %d partitions rebuilt, want %d", parent, len(names), before[parent])
				}
				for i := 1; i < len(names); i++ {
					l, h := lo[i].UTC(), hi[i].UTC()
					if !l.Equal(hi[i-1].UTC()) {
						t.Fatalf("%s: %s starts at %s, previous ends at %s", parent, names[i], l, hi[i-1].UTC())
					}
					if h.Sub(l) != unit || l.Hour() != 0 || l.Minute() != 0 || (parent == "sensor_samples" && l.Weekday() != time.Monday) {
						t.Fatalf("%s: %s is %s to %s, want %s from 00:00 UTC", parent, names[i], l, h, unit)
					}
					if want := parent + "_p" + l.Format("20060102"); names[i] != "core."+want {
						t.Fatalf("%s named %s, want %s", names[i], names[i], want)
					}
				}
			}
		})
	}
}

// The API side: health is clean on a maintained database, and expired rows in legacy (which is not dropped by
// range until its whole span expires) are deleted by PruneHistory.
func TestPartitionHealthAndLegacyPruning(t *testing.T) {
	f := setup(t)
	steady(t, f.admin)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Retention", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Temperature)); e != nil {
		t.Fatal(e)
	}
	health, e := f.repo.PartitionHealth(ctx)
	if e != nil || len(health) != 3 { // ble_history, sensor_samples, access_log (00038)
		t.Fatalf("health: %+v %v", health, e)
	}
	for _, h := range health {
		if h.DefaultRows != 0 || h.CoveredUntil == nil || h.CoveredUntil.Before(time.Now().Add(7*24*time.Hour)) {
			t.Fatalf("unhealthy %+v", h)
		}
	}
	// Rows older than the default retention (90 days, 24 hours) sit in legacy, whose range starts at MINVALUE.
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading)
    VALUES($1,$2,'c30000393fe5','expired',now()-interval '100 days','minew-ffe1-a101@1','{}')`, a.TenantID, g.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.ExecContext(ctx, `INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source)
    VALUES($1,$2,'c30000393fe5','expired',now()-interval '2 days','020106','device')`, a.TenantID, g.ID); e != nil {
		t.Fatal(e)
	}
	steps, pruned, e := f.repo.MaintainPartitions(ctx)
	if e != nil || pruned < 2 {
		t.Fatalf("maintenance: steps %+v pruned %d %v", steps, pruned, e)
	}
	for _, s := range steps {
		if s.Action == "dropped" {
			t.Fatalf("nothing is expired whole, got %+v", steps)
		}
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE event_key='expired'`) + count(t, f.admin, `SELECT count(*) FROM core.ble_history WHERE event_key='expired'`); n != 0 {
		t.Fatalf("%d expired rows left", n)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1`, g.ID); n != 1 {
		t.Fatalf("the fresh sample went too: %d", n)
	}
}

// A cutover constraint of 00036 that 00037 did not remove is reported at every maintenance run (and so at API
// start-up): it stops ingest at its cutover time.
func TestLeftoverCutoverIsReported(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if left, e := f.repo.LeftoverCutovers(ctx); e != nil || len(left) != 0 {
		t.Fatalf("a migrated database has leftovers: %v %v", left, e)
	}
	if _, e := f.admin.ExecContext(ctx, `ALTER TABLE core.sensor_samples_default ADD CONSTRAINT sensor_samples_cutover CHECK (received_at < '2100-01-01') NOT VALID`); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = f.admin.ExecContext(ctx, `ALTER TABLE core.sensor_samples_default DROP CONSTRAINT IF EXISTS sensor_samples_cutover`)
	})
	left, e := f.repo.LeftoverCutovers(ctx)
	if e != nil || len(left) != 1 || left[0] != "core.sensor_samples_default.sensor_samples_cutover" {
		t.Fatalf("leftovers: %v %v", left, e)
	}
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	f.repo.MaintainAndReport(ctx, time.Now())
	slog.SetDefault(previous)
	if !strings.Contains(buf.String(), "PARTITION ALERT migration 00036 cutover constraint still present") || !strings.Contains(buf.String(), "sensor_samples_cutover") {
		t.Fatalf("no alert logged:\n%s", buf.String())
	}
}

// Plan shapes: a time bound prunes partitions; the unbounded "latest sample" reads each partition's index in
// order (Merge Append), with no Sort and no Incremental Sort: the ORDER BY matches the history index.
func TestPartitionPruningInPlans(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	tx, e := f.runtime.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e := tx.Exec(`SELECT set_config('app.user_id','',true),set_config('app.tenant_id',$1,true),set_config('app.project_scope','*',true)`, a.TenantID); e != nil {
		t.Fatal(e)
	}
	var cutover time.Time
	if e := f.admin.QueryRow(`SELECT hi FROM core.partition_ranges('sensor_samples') WHERE lo IS NULL AND NOT is_default`).Scan(&cutover); e != nil {
		t.Fatal(e)
	}
	gw, ext := "00000000-0000-0000-0000-000000000001", "c30000393fe5"
	future := explain(t, tx, `SELECT reading FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 AND received_at>=$3 ORDER BY received_at DESC LIMIT 10`,
		gw, ext, cutover.Add(8*24*time.Hour).Format(time.RFC3339))
	if strings.Contains(future, "sensor_samples_legacy") {
		t.Fatalf("legacy not pruned for a range after the cutover:\n%s", future)
	}
	past := explain(t, tx, `SELECT reading FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 AND received_at<$3 ORDER BY received_at DESC LIMIT 10`,
		gw, ext, cutover.Add(-time.Hour).Format(time.RFC3339))
	if !strings.Contains(past, "sensor_samples_legacy") || strings.Contains(past, "sensor_samples_p") || strings.Contains(past, "sensor_samples_default") {
		t.Fatalf("a range before the cutover must read legacy only:\n%s", past)
	}
	// The hot-path probes (redelivery dedupe, Minew and z2m thinning) are bounded on both sides: only the partitions
	// of their window are read, never the ranges ahead nor DEFAULT.
	now := time.Now().UTC()
	for _, probe := range []struct {
		q    string
		args []any
	}{
		{`SELECT 1 FROM core.ble_history WHERE tenant_id=$1 AND gateway_id=$2 AND external_id=$3 AND event_key='k' AND received_at>$4 AND received_at<=$5`,
			[]any{a.TenantID, gw, ext, now.Add(-postgres.RedeliveryWindow), now}},
		{`SELECT 1 FROM core.sensor_samples WHERE tenant_id=$1 AND gateway_id=$2 AND external_id=$3 AND event_key='k' AND received_at>$4 AND received_at<=$5`,
			[]any{a.TenantID, gw, ext, now.Add(-postgres.RedeliveryWindow), now}},
		{`SELECT reading,received_at FROM core.sensor_samples WHERE tenant_id=$1 AND gateway_id=$2 AND external_id=$3 AND received_at>$4 AND received_at<=$5 ORDER BY received_at DESC LIMIT 1`,
			[]any{a.TenantID, gw, ext, now.Add(-30 * time.Second), now}},
	} {
		plan := explain(t, tx, probe.q, probe.args...)
		if strings.Contains(plan, "_default") || regexp.MustCompile(`_p\d{8}`).MatchString(plan) || !strings.Contains(plan, "_legacy") {
			t.Fatalf("probe reads partitions outside its window:\n%s\n%s", probe.q, plan)
		}
	}
	// Every partition, the ones maintenance creates included, carries the ordered indexes the history queries need
	// ((tenant, gateway, external, received_at DESC, event_key) on both tables): checked in the catalogue, because a
	// partition without them would only show up as a slow query once it held data.
	for _, table := range []string{"sensor_samples", "ble_history"} {
		want := `(tenant_id, gateway_id, external_id, received_at DESC, event_key)`
		var missing []string
		rows, e := f.admin.Query(`SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		  WHERE i.inhparent=('core.'||$1)::regclass
		    AND NOT EXISTS (SELECT 1 FROM pg_indexes x WHERE x.schemaname='core' AND x.tablename=c.relname AND x.indexdef LIKE '%USING btree ' || $2)`, table, want)
		if e != nil {
			t.Fatal(e)
		}
		for rows.Next() {
			var name string
			rows.Scan(&name)
			missing = append(missing, name)
		}
		rows.Close()
		var parent int
		f.admin.QueryRow(`SELECT count(*) FROM pg_indexes WHERE schemaname='core' AND tablename=$1 AND indexdef LIKE '%USING btree ' || $2`, table, want).Scan(&parent)
		if len(missing) != 0 || parent != 1 {
			t.Fatalf("%s: ordered history index missing on %v (parent has %d)", table, missing, parent)
		}
	}
	// With those indexes a Merge Append delivers the queries' order and needs no sort. Whether the planner prefers
	// that path depends on the statistics: for an empty or tiny partition an unordered scan plus a sort of a few rows
	// is cheaper and correct, and on legacy it may pick the (tenant, external_id, received_at) identity index and
	// add an Incremental Sort for the event_key tie-break. A shared test database holds whatever earlier tests left,
	// so this check disables sorts and unordered scans for the transaction: a plan without any Sort then exists only
	// if the ordered path does, which is the property under test (not the cost model).
	if _, e := tx.Exec(`SET LOCAL enable_sort = off; SET LOCAL enable_incremental_sort = off; SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off`); e != nil {
		t.Fatal(e)
	}
	sorts := regexp.MustCompile(`(?m)^\s*(->\s+)?(Incremental )?Sort\s*$`)
	for _, q := range []string{
		// StreamHistory's latest and history sub-queries, presence's latest RSSI.
		`SELECT reading FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 ORDER BY received_at DESC,event_key LIMIT 1`,
		`SELECT reading FROM core.sensor_samples WHERE gateway_id=$1 AND external_id=$2 AND received_at>=now()-interval '1 day' ORDER BY received_at DESC,event_key LIMIT 50`,
		`SELECT raw FROM core.ble_history WHERE gateway_id=$1 AND external_id=$2 AND received_at>=now()-interval '1 day' ORDER BY received_at DESC,event_key LIMIT 200`,
	} {
		plan := explain(t, tx, q, gw, ext)
		if !strings.Contains(plan, "Merge Append") || sorts.MatchString(plan) {
			t.Fatalf("should merge per-partition index order with no sort:\n%s\n%s", q, plan)
		}
	}
}

// 00036/00037 on a database that already holds rows, down and up again: every row survives both ways, queries
// answer the same, and ingest works in either shape.
func TestSamplePartitionMigrationKeepsData(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Migration", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{s1Temperature, s1Motion} {
		if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(p)); e != nil {
			t.Fatal(e)
		}
	}
	type snapshot struct {
		samples, ble, history int
		latest                string
	}
	take := func() snapshot {
		t.Helper()
		var s snapshot
		s.samples = count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1`, g.ID)
		s.ble = count(t, f.admin, `SELECT count(*) FROM core.ble_history WHERE gateway_id=$1`, g.ID)
		streams, e := f.repo.StreamHistory(ctx, a, g.ID, time.Now().Add(-time.Hour), 50)
		if e != nil || len(streams) != 1 {
			t.Fatalf("stream history: %d %v", len(streams), e)
		}
		s.history = len(streams[0].History)
		s.latest = strings.Join(streams[0].Latest.Frames, ",")
		return s
	}
	relkind := func(table string) string {
		var k string
		if e := f.admin.QueryRow(`SELECT relkind::text FROM pg_class WHERE oid=$1::regclass`, table).Scan(&k); e != nil {
			t.Fatal(e)
		}
		return k
	}
	before := take()
	if before.samples == 0 || before.ble == 0 || before.history != 2 {
		t.Fatalf("seed: %+v", before)
	}
	clearDownGuards(t, f)
	if e := goose.DownTo(f.admin, "../migrations", 35); e != nil {
		t.Fatalf("down to 00035: %v", e)
	}
	if relkind("core.sensor_samples") != "r" || relkind("core.ble_history") != "r" {
		t.Fatal("down must leave plain tables")
	}
	var key string
	if e := f.admin.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='core.sensor_samples'::regclass AND contype='p'`).Scan(&key); e != nil || strings.Contains(key, "received_at") {
		t.Fatalf("down key: %s %v", key, e)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM pg_constraint WHERE conname LIKE '%_cutover'`); n != 0 {
		t.Fatal("cutover constraint left behind")
	}
	if down := take(); down != before {
		t.Fatalf("after down %+v, before %+v", down, before)
	}
	if e := goose.Up(f.admin, "../migrations"); e != nil {
		t.Fatalf("up again: %v", e)
	}
	if relkind("core.sensor_samples") != "p" || relkind("core.ble_history") != "p" {
		t.Fatal("up must leave partitioned tables")
	}
	if up := take(); up != before {
		t.Fatalf("after up %+v, before %+v", up, before)
	}
	// Existing rows stay in legacy; ingest still works.
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE tableoid<>'core.sensor_samples_legacy'::regclass AND gateway_id=$1`, g.ID); n != 0 {
		t.Fatalf("%d existing rows outside legacy", n)
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(`[{"mac":"c30000393fe5","rawData":"0201060303e1ff1016e1ffa1016419b8402ee53f390000c3"}]`)); e != nil {
		t.Fatal(e)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1`, g.ID); n != before.samples+1 {
		t.Fatalf("ingest after the round trip: %d samples", n)
	}
}

// The down migration after maintenance already dropped legacy: the plain tables are recreated from the remaining
// partitions (here a row that sat in DEFAULT).
func TestSamplePartitionDownAfterLegacyDropped(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Legacy gone", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Temperature)); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{
		`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading) VALUES($1,$2,'c30000393fe5','far','2100-01-01','minew-ffe1-a101@1','{}')`,
		`INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source) VALUES($1,$2,'c30000393fe5','far','2100-01-01','020106','device')`,
	} {
		if _, e := f.admin.ExecContext(ctx, q, a.TenantID, g.ID); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := f.admin.ExecContext(ctx, `DROP TABLE core.sensor_samples_legacy; DROP TABLE core.ble_history_legacy`); e != nil {
		t.Fatal(e)
	}
	clearDownGuards(t, f)
	if e := goose.DownTo(f.admin, "../migrations", 35); e != nil {
		t.Fatalf("down to 00035 without legacy: %v", e)
	}
	for _, table := range []string{"core.sensor_samples", "core.ble_history"} {
		var kind string
		if e := f.admin.QueryRow(`SELECT relkind::text FROM pg_class WHERE oid=$1::regclass`, table).Scan(&kind); e != nil || kind != "r" {
			t.Fatalf("%s after down: %q %v", table, kind, e)
		}
		if n := count(t, f.admin, `SELECT count(*) FROM `+table+` WHERE event_key='far'`); n != 1 {
			t.Fatalf("%s lost the DEFAULT row: %d", table, n)
		}
		if n := count(t, f.admin, `SELECT count(*) FROM pg_policy WHERE polrelid=$1::regclass`, table); n != 2 {
			t.Fatalf("%s has %d policies after down, want tenant_scope and project_scope", table, n)
		}
	}
	if n := count(t, f.runtime, `SELECT count(*) FROM core.sensor_samples`); n != 0 {
		t.Fatal("no tenant context must see no sample")
	}
	// A timestamp beyond the next cutover would fail 00036's CHECK; received_at is always the server's clock.
	if _, e := f.admin.ExecContext(ctx, `DELETE FROM core.sensor_samples WHERE event_key='far'; DELETE FROM core.ble_history WHERE event_key='far'`); e != nil {
		t.Fatal(e)
	}
	if e := goose.Up(f.admin, "../migrations"); e != nil {
		t.Fatalf("up again: %v", e)
	}
	if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(s1Motion)); e != nil {
		t.Fatal(e)
	}
}

// 00036 run again after a failure part-way: ble_history already swapped (its new key must be kept, not rebuilt)
// and sensor_samples left with an INVALID index from a failed concurrent build (dropped and built again).
func TestPartitionPrepRerunAfterPartialFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	clearDownGuards(t, f)
	if e := goose.DownTo(f.admin, "../migrations", 35); e != nil {
		t.Fatalf("down to 00035: %v", e)
	}
	for _, q := range []string{
		`CREATE UNIQUE INDEX ble_history_pkey_v2 ON core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at)`,
		`ALTER TABLE core.ble_history DROP CONSTRAINT ble_history_pkey`,
		`ALTER TABLE core.ble_history ADD CONSTRAINT ble_history_pkey PRIMARY KEY USING INDEX ble_history_pkey_v2`,
		`DO $$ DECLARE x timestamptz := date_trunc('day', now(), 'UTC') + interval '7 days'; BEGIN
		  EXECUTE format('ALTER TABLE core.ble_history ADD CONSTRAINT ble_history_cutover CHECK (received_at < %L) NOT VALID', x);
		  EXECUTE format('COMMENT ON CONSTRAINT ble_history_cutover ON core.ble_history IS %L', to_char(x AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'));
		 END $$`,
		`CREATE UNIQUE INDEX sensor_samples_pkey_v2 ON core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at)`,
		`UPDATE pg_index SET indisvalid=false WHERE indexrelid='core.sensor_samples_pkey_v2'::regclass`,
	} {
		if _, e := f.admin.ExecContext(ctx, q); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	var bleKey, invalid int64
	if e := f.admin.QueryRow(`SELECT conindid::bigint FROM pg_constraint WHERE conrelid='core.ble_history'::regclass AND contype='p'`).Scan(&bleKey); e != nil {
		t.Fatal(e)
	}
	if e := f.admin.QueryRow(`SELECT 'core.sensor_samples_pkey_v2'::regclass::oid::bigint`).Scan(&invalid); e != nil {
		t.Fatal(e)
	}
	if e := goose.Up(f.admin, "../migrations"); e != nil {
		t.Fatalf("rerun: %v", e)
	}
	var legacyKey, samplesKey int64
	if e := f.admin.QueryRow(`SELECT conindid::bigint FROM pg_constraint WHERE conrelid='core.ble_history_legacy'::regclass AND contype='p'`).Scan(&legacyKey); e != nil || legacyKey != bleKey {
		t.Fatalf("ble_history's swapped key was rebuilt: %d, was %d (%v)", legacyKey, bleKey, e)
	}
	if e := f.admin.QueryRow(`SELECT conindid::bigint FROM pg_constraint WHERE conrelid='core.sensor_samples_legacy'::regclass AND contype='p'`).Scan(&samplesKey); e != nil || samplesKey == invalid {
		t.Fatalf("the invalid index became the key: %d (%v)", samplesKey, e)
	}
	if n := count(t, f.admin, `SELECT count(*) FROM pg_class WHERE relnamespace='core'::regnamespace AND relname LIKE '%\_pkey\_v2'`); n != 0 {
		t.Fatalf("%d *_pkey_v2 relations left (index or placeholder)", n)
	}
	var def string
	if e := f.admin.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='core.sensor_samples_legacy'::regclass AND contype='p'`).Scan(&def); e != nil || !strings.Contains(def, "received_at") {
		t.Fatalf("sensor_samples key: %s %v", def, e)
	}
}

// liveRig is a workspace with three Minew gateways and a Zigbee2MQTT gateway with a registered plug, sample
// thinning on, for tests that run schema work under live ingest.
type liveRig struct {
	*zigbeeRig
	minew []string
	plug  simulation.Z2MSwitch
}

func newLiveRig(t *testing.T) *liveRig {
	r := newZigbeeRig(t, false)
	r.f.repo.Configure(postgres.Options{SampleMinIntervalSec: 30, DiscoveryLimit: 100})
	l := &liveRig{zigbeeRig: r, minew: make([]string, 3), plug: simulation.Z2MSwitches[0]}
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.g.ID, "plug", l.plug.IEEE, "tuya-ts001x-switch@1"); e != nil {
		t.Fatal(e)
	}
	for i := range l.minew {
		g, _, e := r.f.service.CreateGateway(r.ctx, r.owner, fmt.Sprintf("Live %d", i), "minew-mg3")
		if e != nil {
			t.Fatal(e)
		}
		l.minew[i] = g.ID
		if _, e := r.f.repo.CapturePacket(r.ctx, r.owner.TenantID, g.ID, []byte(s1Temperature)); e != nil {
			t.Fatal(e)
		}
	}
	return l
}

// ingestLoad runs workers until stopped and records how long each call took and whether it failed.
type ingestLoad struct {
	stop     atomic.Bool
	wg       sync.WaitGroup
	mu       sync.Mutex
	waits    []time.Duration
	failures []string
}

func (l *ingestLoad) record(start time.Time, e error, what string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waits = append(l.waits, time.Since(start))
	if e != nil {
		l.failures = append(l.failures, what+": "+e.Error())
	}
}

func (l *ingestLoad) worker(pause time.Duration, fn func(n int)) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		for n := 0; !l.stop.Load(); n++ {
			fn(n)
			time.Sleep(pause)
		}
	}()
}

// settle measures what the load costs on its own before any schema work: after a 200 ms warm-up (first
// connections, first plans) it lets the workers run for d and returns the p99 of their calls, then forgets those
// calls (failures are kept). On a machine busy with other work every call is slower, so the lock-wait bound is
// applied on top of this baseline. A baseline over maxBaseline means the machine is too loaded for a timing test to
// say anything: the test is skipped (with the numbers) rather than given a looser bound.
func (l *ingestLoad) settle(t *testing.T, d time.Duration) time.Duration {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	l.mu.Lock()
	l.waits = l.waits[:0]
	l.mu.Unlock()
	time.Sleep(d)
	l.mu.Lock()
	waits := append([]time.Duration(nil), l.waits...)
	l.waits = l.waits[:0]
	l.mu.Unlock()
	if len(waits) < 20 {
		l.stop.Store(true)
		l.wg.Wait()
		t.Skipf("machine too loaded for a timing test: only %d ingest calls in %s without schema work", len(waits), d)
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	p99 := waits[len(waits)*99/100]
	if p99 > maxBaseline {
		l.stop.Store(true)
		l.wg.Wait()
		t.Skipf("machine too loaded for a timing test: ingest p99 is %s without any schema work (limit %s)", p99, maxBaseline)
	}
	return p99
}

// maxBaseline is the slowest ingest p99, without schema work, at which the live-ingest timing tests still run.
const maxBaseline = time.Second

// waitBound is the longest call allowed while schema work runs: lockWait on top of what the same load took without it.
func waitBound(lockWait, baseline time.Duration) time.Duration {
	return lockWait + baseline
}

// finish stops the workers and fails the test on any failed call or a wait of maxWait or more.
func (l *ingestLoad) finish(t *testing.T, maxWait time.Duration) (calls int, p99, longest time.Duration) {
	t.Helper()
	l.stop.Store(true)
	l.wg.Wait()
	if len(l.failures) > 0 {
		t.Fatalf("%d of %d calls failed, first: %s", len(l.failures), len(l.waits), l.failures[0])
	}
	if len(l.waits) < 50 {
		t.Fatalf("only %d calls ran", len(l.waits))
	}
	sort.Slice(l.waits, func(i, j int) bool { return l.waits[i] < l.waits[j] })
	calls, p99, longest = len(l.waits), l.waits[len(l.waits)*99/100], l.waits[len(l.waits)-1]
	if longest >= maxWait {
		t.Fatalf("a call waited %s (p99 %s over %d calls)", longest, p99, calls)
	}
	return calls, p99, longest
}

// startIngest runs Minew packets (ble_history, sensor_streams, sensor_samples with thinning) on every Minew gateway,
// Zigbee2MQTT plug reports (mostly identical, so thinned after the thinning read; a change every fifth) and a
// dashboard reader, through the real repository and service.
func (l *liveRig) startIngest(t *testing.T) *ingestLoad {
	load := &ingestLoad{}
	f, ctx := l.f, l.ctx
	for i, gw := range l.minew {
		load.worker(time.Duration(5+i)*time.Millisecond, func(n int) {
			p := s1Temperature
			if n%2 == 1 {
				p = s1Motion
			}
			payload := strings.Replace(p, `"}]`, fmt.Sprintf(`","sequence":%d}]`, n), 1)
			start := time.Now()
			_, e := f.repo.CapturePacket(ctx, l.owner.TenantID, gw, []byte(payload))
			load.record(start, e, "minew")
		})
	}
	base := zigbee2mqtt.BaseTopic(l.g.ID)
	for i := 0; i < 2; i++ {
		load.worker(7*time.Millisecond, func(n int) {
			m := simulation.Z2MMessage{Topic: base + "/" + l.plug.Name, Payload: simulation.Z2MState(l.plug, []bool{(n/5)%2 == 0}, 100+i)}
			gateway, msg, e := zigbee2mqtt.Route(m.Topic)
			if e != nil {
				load.record(time.Now(), e, "z2m route")
				return
			}
			start := time.Now()
			_, e = f.service.CaptureZ2M(ctx, l.owner.TenantID, gateway, msg, m.Payload)
			load.record(start, e, "z2m")
		})
	}
	load.worker(20*time.Millisecond, func(int) {
		start := time.Now()
		_, e := f.repo.StreamHistory(ctx, l.owner, l.minew[0], time.Now().Add(-time.Hour), 50)
		load.record(start, e, "history")
	})
	return load
}

// noticeDB is an admin connection pool that counts the server NOTICEs containing `match`.
func noticeDB(t *testing.T, match string) (*sql.DB, *atomic.Int64) {
	t.Helper()
	cfg, e := pgx.ParseConfig(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	var seen atomic.Int64
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if strings.Contains(n.Message, match) {
			seen.Add(1)
		}
	}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { db.Close() })
	return db, &seen
}

// The migration itself under live ingest: Minew and Zigbee2MQTT ingest and a dashboard reader keep flowing through
// the real repository while 00036 and 00037 run over a few hundred thousand rows, next to a reader that breaks the
// lock order (sensor_samples and ble_history, then gateways, holding its locks 150 ms in between, every 300 ms). No call may fail or
// deadlock, none may wait 1.5 s beyond the slowest call of the same load before the migration, and 00037 must have taken its give-back-and-retry path at least once.
func TestPartitionMigrationUnderLiveIngest(t *testing.T) {
	l := newLiveRig(t)
	f, ctx := l.f, l.ctx
	clearDownGuards(t, f)
	if e := goose.DownTo(f.admin, "../migrations", 35); e != nil {
		t.Fatalf("down to 00035: %v", e)
	}
	gateways := "{" + strings.Join(l.minew, ",") + "}"
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM core.sensor_samples WHERE gateway_id = ANY($1::uuid[]) AND event_key LIKE 'seed%'`,
			`DELETE FROM core.ble_history WHERE gateway_id = ANY($1::uuid[]) AND event_key LIKE 'seed%'`} {
			if _, e := f.admin.ExecContext(context.Background(), q, gateways); e != nil {
				t.Errorf("removing seed rows: %v", e)
			}
		}
	})
	// Volume, so the concurrent index builds and the validation take a while.
	for _, q := range []string{
		`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading)
		  SELECT s.tenant_id,s.gateway_id,s.external_id,'seed'||g,now()-(g % 2592000)*interval '1 second','minew-ffe1-a101@1','{"frames":["minew-ffe1-a101@1"]}'
		  FROM core.sensor_streams s, generate_series(1,100000) g WHERE s.gateway_id = ANY($1::uuid[])`,
		`INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source)
		  SELECT s.tenant_id,s.gateway_id,s.external_id,'seed'||g,now()-(g % 86400)*interval '1 second','020106','device'
		  FROM core.sensor_streams s, generate_series(1,50000) g WHERE s.gateway_id = ANY($1::uuid[])`,
	} {
		if _, e := f.admin.ExecContext(ctx, q, gateways); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := f.admin.ExecContext(ctx, `ANALYZE core.sensor_samples, core.ble_history`); e != nil {
		t.Fatal(e)
	}

	load := l.startIngest(t)
	// The order breaker: reads both history tables, holds its locks, then reads gateways. Each statement is timed
	// on its own; the pause is the point, not a wait.
	load.worker(300*time.Millisecond, func(int) {
		tx, e := f.runtime.BeginTx(ctx, nil)
		if e != nil {
			load.record(time.Now(), e, "breaker begin")
			return
		}
		defer tx.Rollback()
		step := func(q string, args ...any) bool {
			start := time.Now()
			var n int
			e := tx.QueryRow(q, args...).Scan(&n)
			load.record(start, e, "breaker: "+q)
			return e == nil
		}
		if !step(`SELECT count(*) FROM (SELECT set_config('app.user_id','',true),set_config('app.tenant_id',$1,true),set_config('app.project_scope','*',true)) s`, l.owner.TenantID) ||
			!step(`SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1 AND received_at>now()-interval '1 minute'`, l.minew[0]) ||
			!step(`SELECT count(*) FROM core.ble_history WHERE gateway_id=$1 AND received_at>now()-interval '1 minute'`, l.minew[0]) {
			return
		}
		time.Sleep(150 * time.Millisecond)
		if step(`SELECT count(*) FROM core.gateways`) {
			start := time.Now()
			load.record(start, tx.Commit(), "breaker commit")
		}
	})
	admin, retries := noticeDB(t, "00037: tables busy, retrying")
	baseline := load.settle(t, time.Second)
	fail := func(msg string, e error) {
		load.stop.Store(true)
		load.wg.Wait()
		t.Fatalf("%s: %v", msg, e)
	}
	began := time.Now()
	if e := goose.UpTo(admin, "../migrations", 36); e != nil {
		fail("00036 under load", e)
	}
	prep := time.Since(began)
	// Deterministic order breaker for 00037: it already holds both history tables when 00037 starts, and asks for
	// gateways only 400 ms later, so 00037 (gateways first) must give its locks back at least once.
	held := make(chan struct{})
	sentinel := make(chan error, 1)
	go func() {
		tx, e := f.runtime.BeginTx(ctx, nil)
		if e != nil {
			sentinel <- e
			close(held)
			return
		}
		defer tx.Rollback()
		for _, q := range []string{
			`SELECT count(*) FROM (SELECT set_config('app.user_id','',true),set_config('app.tenant_id','` + l.owner.TenantID + `',true),set_config('app.project_scope','*',true)) s`,
			`SELECT count(*) FROM core.ble_history WHERE received_at>now()-interval '1 minute'`,
			`SELECT count(*) FROM core.sensor_samples WHERE received_at>now()-interval '1 minute'`,
		} {
			var n int
			if e := tx.QueryRow(q).Scan(&n); e != nil {
				sentinel <- e
				close(held)
				return
			}
		}
		close(held)
		time.Sleep(400 * time.Millisecond)
		start := time.Now()
		var n int
		e = tx.QueryRow(`SELECT count(*) FROM core.gateways`).Scan(&n)
		load.record(start, e, "sentinel gateways")
		if e == nil {
			e = tx.Commit()
		}
		sentinel <- e
	}()
	<-held
	began37 := time.Now()
	if e := goose.Up(admin, "../migrations"); e != nil {
		fail("00037 under load", e)
	}
	attach := time.Since(began37)
	if e := <-sentinel; e != nil {
		fail("order breaker", e)
	}
	time.Sleep(500 * time.Millisecond) // ingest keeps going on the partitioned tables
	calls, p99, longest := load.finish(t, waitBound(1500*time.Millisecond, baseline))
	t.Logf("00036 took %s and 00037 %s under load; 00037 gave its locks back %d times; %d calls, p99 %s, max %s (baseline p99 %s)", prep, attach, retries.Load(), calls, p99, longest, baseline)
	if retries.Load() == 0 {
		t.Fatal("00037 never gave its locks back although the order breaker held sensor_samples")
	}
	if n := count(t, f.admin, `SELECT count(*) FROM core.sensor_samples WHERE tableoid<>'core.sensor_samples_legacy'::regclass AND tableoid<>'core.sensor_samples_default'::regclass`); n != 0 {
		// Everything received now is before the cutover: it belongs in legacy.
		t.Fatalf("%d rows outside legacy", n)
	}
	if left, e := f.repo.LeftoverCutovers(ctx); e != nil || len(left) != 0 {
		t.Fatalf("cutover constraints left: %v %v", left, e)
	}
}

// Partition maintenance under live ingest: the newest sample and BLE ranges are dropped again and again (as if
// the look-ahead had just moved on) and MaintainPartitions recreates them, moving nothing, while Minew and
// Zigbee2MQTT ingest run. No 40P01, no failed call, no long wait.
func TestPartitionMaintenanceUnderLiveIngest(t *testing.T) {
	l := newLiveRig(t)
	f, ctx := l.f, l.ctx
	steady(t, f.admin)
	load := l.startIngest(t)
	baseline := load.settle(t, time.Second)
	newest := func(parent string) string {
		t.Helper()
		names, _, _ := partitionsOf(t, f.admin, parent)
		return names[len(names)-1]
	}
	dropNewest := func(parent string) {
		t.Helper()
		victim := newest(parent)
		for attempt := 0; ; attempt++ {
			tx, e := f.admin.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			_, e = tx.Exec(`SET LOCAL lock_timeout='300ms'`)
			if e == nil {
				_, e = tx.Exec(`DROP TABLE ` + victim)
			}
			if e == nil {
				if e = tx.Commit(); e == nil {
					return
				}
			}
			tx.Rollback()
			if sqlState(e) != "55P03" || attempt > 50 {
				t.Fatalf("dropping %s: %v", victim, e)
			}
		}
	}
	// At least 4 s and 10 cycles; on a slow machine the cycles take longer, so keep going (up to 30 s) until 10 ran.
	cycles, created, deadline, hardStop := 0, 0, time.Now().Add(4*time.Second), time.Now().Add(30*time.Second)
	var longest time.Duration
	for (time.Now().Before(deadline) || cycles < 10) && time.Now().Before(hardStop) {
		dropNewest("sensor_samples")
		dropNewest("ble_history")
		start := time.Now()
		steps, _, e := f.repo.MaintainPartitions(ctx)
		if took := time.Since(start); took > longest {
			longest = took
		}
		if e != nil {
			load.stop.Store(true)
			load.wg.Wait()
			t.Fatalf("maintenance under load (cycle %d): %v", cycles, e)
		}
		for _, s := range steps {
			switch s.Action {
			case "created":
				created++
				if s.Moved != 0 {
					t.Errorf("%s moved %d rows out of DEFAULT", s.Relation, s.Moved)
				}
			case "create_deferred", "drop_deferred":
				// allowed: the lock was busy; the next cycle recreates it
			default:
				t.Errorf("unexpected step %+v", s)
			}
		}
		cycles++
		time.Sleep(50 * time.Millisecond)
		steady(t, f.admin) // anything deferred is created before the next drop
	}
	calls, p99, maxWait := load.finish(t, waitBound(1500*time.Millisecond, baseline))
	t.Logf("%d maintenance cycles (%d ranges created, longest run %s) under %d ingest calls, p99 %s, max %s (baseline p99 %s)", cycles, created, longest, calls, p99, maxWait, baseline)
	if cycles < 10 || created < cycles {
		t.Fatalf("%d cycles created %d ranges", cycles, created)
	}
	health, e := f.repo.PartitionHealth(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, h := range health {
		if h.DefaultRows != 0 {
			t.Fatalf("rows left in DEFAULT: %+v", h)
		}
	}
}
