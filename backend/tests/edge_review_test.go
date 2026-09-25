package tests

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/domain"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Discovery is bounded per gateway (newest MaxLAN kept) and throttled (one accepted list per 30 s).
func TestEdgeDiscoveryBounded(t *testing.T) {
	r := newEdgeRig(t, nil)
	list := func(prefix string, n int) []map[string]any {
		out := []map[string]any{}
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{"id": fmt.Sprintf("%s%014d", prefix, i), "ip": "10.0.0.1", "version": "3.3"})
		}
		return out
	}
	r.must(r.capture("discovery", list("bfaaaa", 400)))
	// Arriving too soon: acknowledged, nothing written.
	r.must(r.capture("discovery", list("bfbbbb", 400)))
	if n := r.count(`SELECT count(*) FROM core.edge_lan_devices WHERE gateway_id=$1`, r.gateway); n != 400 {
		t.Fatalf("throttled list wrote: %d rows", n)
	}
	// The next accepted list pushes the gateway over MaxLAN: only the newest 500 remain.
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.edge_agents SET last_discovery_at=now()-interval '1 minute' WHERE gateway_id=$1`, r.gateway); e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.edge_lan_devices SET last_seen=now()-interval '1 hour' WHERE gateway_id=$1`, r.gateway); e != nil {
		t.Fatal(e)
	}
	r.must(r.capture("discovery", list("bfcccc", 400)))
	if n := r.count(`SELECT count(*) FROM core.edge_lan_devices WHERE gateway_id=$1`, r.gateway); n != 500 {
		t.Fatalf("rows after cap: %d", n)
	}
	if n := r.count(`SELECT count(*) FROM core.edge_lan_devices WHERE gateway_id=$1 AND device_id LIKE 'bfcccc%'`, r.gateway); n != 400 {
		t.Fatalf("the newest list was not kept whole: %d", n)
	}
}

// A device imported without a key stays "missing" whatever the agent reports, and cannot be commanded.
func TestEdgeKeylessDeviceStaysMissing(t *testing.T) {
	r := newEdgeRig(t, nil)
	raw, _ := os.ReadFile("../internal/adapters/tuya/testdata/kg_3gang.json")
	dps, category, e := tuya.ParseSpecifications(raw)
	if e != nil {
		t.Fatal(e)
	}
	id := "bf00000000000000nk01"
	if _, e := r.f.repo.SaveTuyaDevices(r.ctx, r.owner, r.gateway, []postgres.TuyaImport{{TuyaID: id, Name: "no key", Category: category, Spec: dps}}); e != nil {
		t.Fatal(e)
	}
	d, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "no key", id, domain.TuyaWiFiProfile)
	if e != nil {
		t.Fatal(e)
	}
	r.must(r.capture(id+"/state", `{"dps":{"1":true}}`))
	for _, msg := range []string{`{"state":"offline","reason":"auth_failed"}`, `{"state":"offline","reason":"key_suspect"}`, `{"state":"online"}`} {
		r.must(r.capture(id+"/availability", msg))
		if ks := r.text(`SELECT key_status FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, id); ks != "missing" {
			t.Fatalf("after %s: %q", msg, ks)
		}
	}
	if code, out := r.send(r.ownerAuth.AccessToken, map[string]any{"device_id": d.ID, "property": "switch_1", "value": "OFF"}); code != 409 || out["error"] != "key_unavailable" {
		t.Fatalf("command to a keyless device: %d %v", code, out)
	}
}

// A malformed specification is refused at the repository boundary.
func TestEdgeImportRejectsBadSpec(t *testing.T) {
	r := newEdgeRig(t, nil)
	seven := int64(7)
	bad := []tuya.DP{{ID: 1, Code: "switch_1", Type: "bool", Access: "rw"}, {ID: 1, Code: "dup", Type: "value", Access: "rw", Step: &seven, Scale: 9}}
	if _, e := r.f.repo.SaveTuyaDevices(r.ctx, r.owner, r.gateway, []postgres.TuyaImport{{TuyaID: "bf00000000000000bd01", Spec: bad}}); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("bad spec: %v", e)
	}
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE tuya_id='bf00000000000000bd01'`); n != 0 {
		t.Fatal("a refused import wrote rows")
	}
}

// core.redeem_edge_install_code: a valid code works once; expired, unknown, used, revoked-gateway and
// non-Edge-gateway codes do not; two concurrent redemptions of one code succeed exactly once.
func TestRedeemEdgeInstallCode(t *testing.T) {
	r := newEdgeRig(t, nil)
	hash := func(code string) string { h := sha256.Sum256([]byte(code)); return hex.EncodeToString(h[:]) }
	insert := func(gateway, code string, expires time.Time) {
		t.Helper()
		if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.edge_install_codes(tenant_id,id,gateway_id,code_hash,expires_at) VALUES($1,$2,$3,$4,$5)`,
			r.owner.TenantID, uuid.NewString(), gateway, hash(code), expires); e != nil {
			t.Fatal(e)
		}
	}
	// Redeemed as the application role with no tenant in context, exactly as the installer route will.
	redeem := func(db *sql.DB, code string) (string, string, bool) {
		var tenant, gateway string
		e := db.QueryRowContext(r.ctx, `SELECT tenant_id::text,gateway_id::text FROM core.redeem_edge_install_code($1)`, hash(code)).Scan(&tenant, &gateway)
		if errors.Is(e, sql.ErrNoRows) {
			return "", "", false
		}
		if e != nil {
			t.Fatal(e)
		}
		return tenant, gateway, true
	}
	later := time.Now().Add(30 * time.Minute)
	insert(r.gateway, "good", later)
	if tenant, gateway, ok := redeem(r.f.runtime, "good"); !ok || tenant != r.owner.TenantID || gateway != r.gateway {
		t.Fatalf("valid code: %v %s %s", ok, tenant, gateway)
	}
	if _, _, ok := redeem(r.f.runtime, "good"); ok {
		t.Fatal("a code redeemed twice")
	}
	insert(r.gateway, "old", time.Now().Add(-time.Minute))
	if _, _, ok := redeem(r.f.runtime, "old"); ok {
		t.Fatal("expired code redeemed")
	}
	if _, _, ok := redeem(r.f.runtime, "never-issued"); ok {
		t.Fatal("unknown code redeemed")
	}
	ble, _, e := r.f.service.CreateGateway(r.ctx, r.owner, "BLE", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	insert(ble.ID, "ble", later)
	if _, _, ok := redeem(r.f.runtime, "ble"); ok {
		t.Fatal("code of a non-Edge gateway redeemed")
	}
	revoked, _, e := r.f.service.CreateGateway(r.ctx, r.owner, "Edge 2", domain.EdgeGatewayModel)
	if e != nil {
		t.Fatal(e)
	}
	insert(revoked.ID, "revoked", later)
	if e := r.f.service.RevokeGateway(r.ctx, r.owner, revoked.ID); e != nil {
		t.Fatal(e)
	}
	if _, _, ok := redeem(r.f.runtime, "revoked"); ok {
		t.Fatal("code of a revoked gateway redeemed")
	}
	// Concurrency: two connections race for one code.
	insert(r.gateway, "race", later)
	dsn := os.Getenv("TEST_DATABASE_URL")
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		db, e := sql.Open("pgx", dsn)
		if e != nil {
			t.Fatal(e)
		}
		defer db.Close()
		wg.Add(1)
		go func(db *sql.DB) {
			defer wg.Done()
			if _, _, ok := redeem(db, "race"); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(db)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent redemptions succeeded %d times", wins)
	}
}
