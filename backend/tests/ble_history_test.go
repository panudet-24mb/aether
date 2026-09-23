package tests

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestBLEHistorySurvivesDiagnosticPruningAndIsTenantScoped(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	_, _, b := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "History gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	var first []byte
	for n := 0; n < 105; n++ {
		payload := []byte(fmt.Sprintf(`[{"mac":"aabbccddeeff","rawData":"020106","sequence":%d}]`, n))
		if n == 0 {
			first = payload
		}
		if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, payload); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, first); e != nil {
		t.Fatal(e)
	}
	history, e := f.repo.BLEHistory(ctx, a, g.ID, "AABBCCDDEEFF", time.Time{})
	if e != nil || len(history) != 105 {
		t.Fatalf("history/dedupe: %d %v", len(history), e)
	}
	other, e := f.repo.BLEHistory(ctx, b, g.ID, "aabbccddeeff", time.Time{})
	if e != nil || len(other) != 0 {
		t.Fatal("cross tenant read", e)
	}
	future, e := f.repo.BLEHistory(ctx, a, g.ID, "aabbccddeeff", time.Now().Add(time.Hour))
	if e != nil || len(future) != 0 {
		t.Fatal("range filter", e)
	}
	for n := 1; n < len(history); n++ {
		if history[n].ReceivedAt.After(history[n-1].ReceivedAt) {
			t.Fatal("unordered history")
		}
	}
}
