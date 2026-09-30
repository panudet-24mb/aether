package realtime

import (
	"testing"

	"aether/backend/internal/domain"
)

// "display" signals name a display, not a gateway: only wall displays receive them, with the id; members never do.
func TestDisplaySignalsReachDisplaysOnly(t *testing.T) {
	h := NewHub()
	member, scoped, display := h.Register("t1"), h.RegisterScoped("t1", true), h.RegisterDisplay("t1", true)
	h.Broadcast(domain.Signal{Tenant: "t1", Kind: "display", Gateway: "d1"})
	h.Broadcast(domain.Signal{Tenant: "t1", Kind: "alert", Gateway: "g1"})
	if m := <-member.Send; m.Kind != "alert" || m.GatewayID != "g1" {
		t.Fatalf("member got %v", m)
	}
	if m := <-scoped.Send; m.Kind != "alert" || m.GatewayID != "" {
		t.Fatalf("scoped member got %v", m)
	}
	if m := <-display.Send; m.Kind != "display" || m.GatewayID != "d1" {
		t.Fatalf("display got %v first", m)
	}
	if m := <-display.Send; m.Kind != "alert" || m.GatewayID != "" {
		t.Fatalf("scoped display got %v", m)
	}
	if len(member.Send)+len(scoped.Send) != 0 {
		t.Fatal("members got more signals")
	}
}
