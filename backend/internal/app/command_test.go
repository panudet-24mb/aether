package app

import (
	"aether/backend/internal/domain"
	"context"
	"errors"
	"testing"
)

// A viewer is refused before anything is looked up or queued (the HTTP API answers 403).
func TestViewerCannotCommand(t *testing.T) {
	s := &Service{}
	_, _, e := s.SendCommand(context.Background(), domain.Principal{Role: "viewer", UserID: "u", TenantID: "t"},
		domain.CommandRequest{ID: "00000000-0000-4000-8000-000000000001", DeviceID: "00000000-0000-4000-8000-000000000002", Property: "state_left", Action: "toggle"})
	if !errors.Is(e, domain.ErrForbidden) {
		t.Fatalf("viewer: %v", e)
	}
}
