package tests

import (
	"aether/backend/internal/automation"
	"aether/backend/internal/simulation"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This file is the measurement behind the trigger index: how long one uplink takes, and how many SQL
// statements it costs, when a workspace keeps 10, 100 and 300 flows enabled.
//
// Before the index the cost per uplink grew with flows × trigger.metric blocks × sensors in the
// packet, because every enabled flow was loaded, unmarshalled and asked about every sensor. After it,
// an uplink pays one index lookup, one load of the flows that lookup returned, at most four
// statements for all their rising edges together, and then work only for the flows that fired — so
// the numbers below should be flat in N apart from the handful of flows each packet genuinely wakes.
//
// Statement counting uses pg_stat_statements when the test database has it; without the extension
// the test reports wall time only and says so.

// simulated kit identities the packet carries; the S1 sensors report temperature, MBT01 raises tamper.
var scaleDevices = []string{"f00000000001", "f00000000002", "f00000000003", "f00000000004", "f00000000009"}

// scaleFlow builds one enabled flow. Every third flow watches a device the simulated kit really
// carries; the rest watch synthetic identities no packet mentions, which is what the index must
// filter out. Half are event triggers, half metric triggers.
func scaleFlow(i int) json.RawMessage {
	device := fmt.Sprintf("f0ffff%06x", i)
	if i%3 == 0 {
		device = scaleDevices[i%len(scaleDevices)]
	}
	nodes := []map[string]any{}
	if i%2 == 0 {
		nodes = append(nodes, block("t1", "trigger.event", 0, 0, map[string]any{
			"event_types": []string{"tamper", "leak", "motion"}, "external_ids": []string{device}}))
	} else {
		nodes = append(nodes, block("t1", "trigger.metric", 0, 0, map[string]any{
			"external_id": device, "metric": "temperature", "op": ">", "value": 10 + float64(i%20), "for_sec": (i % 3) * 30}))
	}
	nodes = append(nodes,
		block("c1", "cond.time", 300, 0, map[string]any{"from": "00:00", "to": "00:00"}),
		block("a1", "action.alert", 620, 0, map[string]any{"severity": "info", "title": fmt.Sprintf("flow %d · {{device}}", i)}))
	def := flow(nodes, []map[string]any{link("e1", "t1", "c1"), link("e2", "c1", "a1", "true")})
	raw, e := json.Marshal(def)
	if e != nil {
		panic(e)
	}
	return raw
}

// statements returns the number of SQL statements the server has executed so far, and whether the
// database can tell us at all.
func statements(ctx context.Context, db *sql.DB) (int64, bool) {
	var available int64
	if e := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_extension WHERE extname='pg_stat_statements'`).Scan(&available); e != nil || available == 0 {
		return 0, false
	}
	var calls sql.NullInt64
	if e := db.QueryRowContext(ctx, `SELECT sum(calls) FROM pg_stat_statements`).Scan(&calls); e != nil {
		return 0, false
	}
	return calls.Int64, true
}

// TestAutomationScale reports ms and SQL statements per uplink at 10, 100 and 300 enabled flows.
// Each size runs in its own workspace, so the numbers are not carried over from the previous one.
func TestAutomationScale(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	const uplinks = 10
	for _, n := range []int{10, 100, 300} {
		t.Run(fmt.Sprintf("flows=%d", n), func(t *testing.T) {
			_, _, p := f.account(t)
			g, _, e := f.service.CreateGateway(ctx, p, "Scale gateway", "minew-mg3")
			if e != nil {
				t.Fatal(e)
			}
			for i := 0; i < n; i++ {
				record := automation.Automation{ID: uuid.NewString(), Name: fmt.Sprintf("flow %04d", i), Enabled: true, Definition: scaleFlow(i), Revision: 1}
				if e := f.repo.CreateAutomation(ctx, p, record); e != nil {
					t.Fatalf("create flow %d: %v", i, e)
				}
			}
			// One uplink first, so the run is not measuring first-touch page faults and plan caching.
			now := time.Now().UTC()
			if _, e := f.repo.CapturePacket(ctx, p.TenantID, g.ID, simulation.Packet(0, now)); e != nil {
				t.Fatal(e)
			}

			before, counted := statements(ctx, f.admin)
			start := time.Now()
			for step := 1; step <= uplinks; step++ {
				if _, e := f.repo.CapturePacket(ctx, p.TenantID, g.ID, simulation.Packet(step, now.Add(time.Duration(step)*5*time.Second))); e != nil {
					t.Fatalf("uplink %d: %v", step, e)
				}
			}
			elapsed := time.Since(start)
			after, _ := statements(ctx, f.admin)

			perUplink := float64(elapsed.Microseconds()) / float64(uplinks) / 1000
			if counted {
				t.Logf("flows=%d: %.2f ms/uplink, %.1f SQL statements/uplink", n, perUplink, float64(after-before)/float64(uplinks))
			} else {
				t.Logf("flows=%d: %.2f ms/uplink; statement count unavailable (pg_stat_statements is not installed in this database), wall time only", n, perUplink)
			}
			// A guard, not a benchmark: the whole point of the index is that ingest does not degrade
			// with the number of enabled flows. 5 s is the packet interval of one gateway.
			if perUplink > 1000 {
				t.Fatalf("an uplink must stay far inside the 5 s packet interval: %.2f ms with %d flows", perUplink, n)
			}
		})
	}
}
