package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/domain"
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeviceMoveRemoveRestore(t *testing.T) {
	f := setup(t)
	_, authA, a := f.account(t)
	_, _, b := f.account(t)
	ctx := context.Background()
	g1, secret1, e := f.service.CreateGateway(ctx, a, "Floor 1", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	g2, _, e := f.service.CreateGateway(ctx, a, "Floor 2", "generic-http")
	if e != nil {
		t.Fatal(e)
	}
	foreign, _, e := f.service.CreateGateway(ctx, b, "Other tenant", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	d, e := f.service.CreateDevice(ctx, a, g1.ID, "Probe", "sensor-a", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	tenant, e := f.service.Gateway(ctx, g1.ID, secret1)
	if e != nil {
		t.Fatal(e)
	}
	ts := time.Now().UTC().Truncate(time.Second)
	if _, e = f.service.Ingest(ctx, tenant, g1.ID, d.ID, ts, map[string]float64{"temperature": 21}); e != nil {
		t.Fatal(e)
	}

	// Cross-tenant: neither the device nor a foreign target gateway is reachable.
	if _, e = f.service.UpdateDevice(ctx, b, d.ID, nil, &foreign.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("foreign tenant moved the device: %v", e)
	}
	if _, e = f.service.UpdateDevice(ctx, a, d.ID, nil, &foreign.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("moved to a foreign gateway: %v", e)
	}
	if e = f.service.RemoveDevice(ctx, b, d.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("foreign tenant removed the device: %v", e)
	}

	// Rename + move through the API; history stays with the device id.
	api := httpapi.New(f.cfg, f.service, f.repo)
	code, out, _ := req(t, api, "POST", "/api/v1/devices/"+d.ID+"/update", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "Probe (moved)", "gateway_id": g2.ID})
	if code != 200 || out["gateway_id"] != g2.ID || out["name"] != "Probe (moved)" {
		t.Fatalf("move: %d %v", code, out)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/devices/"+d.ID+"/update", "Bearer "+authA.AccessToken, "", "", map[string]any{})
	if code != 400 {
		t.Fatalf("empty update accepted: %d", code)
	}
	if _, e = f.service.Ingest(ctx, tenant, g1.ID, d.ID, ts.Add(time.Second), map[string]float64{"temperature": 22}); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("old gateway can still write after the move: %v", e)
	}
	if st, e := f.repo.DeviceState(ctx, a, d.ID); e != nil || st.DeviceID != d.ID {
		t.Fatalf("state lost after move: %+v %v", st, e)
	}
	// Moving onto a gateway that already has the same identity is a conflict, not a silent merge.
	dup, e := f.service.CreateDevice(ctx, a, g1.ID, "Twin", "sensor-a", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.UpdateDevice(ctx, a, dup.ID, nil, &g2.ID); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("duplicate identity on target gateway: %v", e)
	}

	// Remove: hidden from lists and state, identity free to adopt again, history kept.
	code, _, _ = req(t, api, "POST", "/api/v1/devices/"+d.ID+"/remove", "Bearer "+authA.AccessToken, "", "", map[string]any{})
	if code != 204 {
		t.Fatalf("remove: %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/devices/"+d.ID+"/remove", "Bearer "+authA.AccessToken, "", "", map[string]any{})
	if code != 404 {
		t.Fatalf("double remove: %d", code)
	}
	active, e := f.repo.ListDevices(ctx, a)
	if e != nil || len(active) != 1 || active[0].ID != dup.ID {
		t.Fatalf("active list: %+v %v", active, e)
	}
	if _, e = f.repo.DeviceState(ctx, a, d.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("removed device state still served: %v", e)
	}
	removed, e := f.repo.ListRemovedDevices(ctx, a)
	if e != nil || len(removed) != 1 || removed[0].ID != d.ID || removed[0].RemovedAt == nil {
		t.Fatalf("removed list: %+v %v", removed, e)
	}
	if other, e := f.repo.ListRemovedDevices(ctx, b); e != nil || len(other) != 0 {
		t.Fatalf("removed list leaked across tenants: %+v", other)
	}
	var kept int
	if e = f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.telemetry WHERE device_id=$1`, d.ID).Scan(&kept); e != nil || kept != 1 {
		t.Fatalf("telemetry history must survive removal: %d %v", kept, e)
	}
	again, e := f.service.CreateDevice(ctx, a, g2.ID, "Probe again", "sensor-a", "generic-environment@1")
	if e != nil {
		t.Fatalf("re-adopt after removal: %v", e)
	}
	// Restore is refused while the identity is taken, and works once it is free.
	if e = f.service.RestoreDevice(ctx, a, d.ID); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("restore over an active identity: %v", e)
	}
	if e = f.service.RemoveDevice(ctx, a, again.ID); e != nil {
		t.Fatal(e)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/devices/"+d.ID+"/restore", "Bearer "+authA.AccessToken, "", "", map[string]any{})
	if code != 204 {
		t.Fatalf("restore: %d", code)
	}
	if st, e := f.repo.DeviceState(ctx, a, d.ID); e != nil || st.DeviceID != d.ID {
		t.Fatalf("state after restore: %+v %v", st, e)
	}
	// The runtime role may not rewrite identity or profile even though it can now update the row.
	if _, e = f.runtime.ExecContext(ctx, `SELECT set_config('app.tenant_id',$1,false)`, a.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.runtime.ExecContext(ctx, `UPDATE core.devices SET external_id='hijacked' WHERE id=$1`, d.ID); e == nil {
		t.Fatal("runtime role rewrote a device identity")
	}
	if _, e = f.runtime.ExecContext(ctx, `DELETE FROM core.devices WHERE id=$1`, d.ID); e == nil {
		t.Fatal("runtime role hard-deleted a device")
	}
}
