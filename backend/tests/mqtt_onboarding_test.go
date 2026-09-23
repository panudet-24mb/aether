package tests

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"errors"
	"testing"
)

func TestMQTTEnrollmentIsolationAndRotation(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	_, _, b := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Self-service MG3", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	hash, e := security.MQTTHash(security.RandomToken())
	if e != nil {
		t.Fatal(e)
	}
	if e = f.repo.EnrollMQTT(ctx, b, g.ID, hash, false); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("foreign enroll: %v", e)
	}
	if e = f.repo.EnrollMQTT(ctx, a, g.ID, hash, false); e != nil {
		t.Fatal(e)
	}
	if e = f.repo.EnrollMQTT(ctx, a, g.ID, hash, false); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("duplicate enrollment: %v", e)
	}
	if e = f.repo.EnrollMQTT(ctx, b, g.ID, hash, true); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("foreign rotation: %v", e)
	}
	if e = f.repo.EnrollMQTT(ctx, a, g.ID, hash, true); e != nil {
		t.Fatal(e)
	}
	states, e := f.repo.MQTTStates(ctx, a)
	if e != nil || len(states) != 1 || states[0].Revision != 2 || states[0].AppliedRevision != 0 {
		t.Fatalf("state: %+v %v", states, e)
	}
	tenant, e := f.repo.MQTTGatewayTenant(ctx, g.ID)
	if e != nil || tenant != a.TenantID {
		t.Fatal("routing", e)
	}
	if e = f.repo.RevokeGateway(ctx, a, g.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.repo.MQTTGatewayTenant(ctx, g.ID); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatal("revoked routing", e)
	}
	if _, e = f.runtime.ExecContext(ctx, `SELECT * FROM core.mqtt_provisioning_accounts()`); e == nil {
		t.Fatal("API role obtained provisioning hashes")
	}
}
