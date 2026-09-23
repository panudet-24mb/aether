package app

import (
	"aether/backend/internal/domain"
	"context"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestIngestRejectsInvalidMetricsBeforeStorage(t *testing.T) {
	s := &Service{}
	for _, tc := range []struct {
		metrics map[string]float64
		ts      time.Time
	}{{map[string]float64{}, time.Now()}, {map[string]float64{"humidity": 101}, time.Now()}, {map[string]float64{"battery": -1}, time.Now()}, {map[string]float64{"temperature": 201}, time.Now()}, {map[string]float64{"tenant_id": 1}, time.Now()}, {map[string]float64{"temperature": 20}, time.Now().Add(time.Hour)}, {map[string]float64{"temperature": 20}, time.Now().Add(-8 * 24 * time.Hour)}} {
		if _, e := s.Ingest(context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString(), tc.ts, tc.metrics); e != domain.ErrInvalid {
			t.Fatal("invalid event reached storage", e)
		}
	}
}
func TestViewerCannotProvisionOrRevoke(t *testing.T) {
	s := &Service{}
	p := domain.Principal{Role: "viewer"}
	if _, _, e := s.CreateGateway(context.Background(), p, "Gateway", "minew-mg3"); e != domain.ErrForbidden {
		t.Fatal(e)
	}
	if _, e := s.CreateDevice(context.Background(), p, uuid.NewString(), "Sensor", "mac", "generic-environment@1"); e != domain.ErrForbidden {
		t.Fatal(e)
	}
	if e := s.RevokeGateway(context.Background(), p, uuid.NewString()); e != domain.ErrForbidden {
		t.Fatal(e)
	}
}
func TestCaptureRejectsScalarAndInvalidJSON(t *testing.T) {
	s := &Service{}
	for _, raw := range []string{"", "true", "42", "null", "[broken]"} {
		if _, e := s.Capture(context.Background(), uuid.NewString(), uuid.NewString(), []byte(raw)); e != domain.ErrInvalid {
			t.Fatal(e)
		}
	}
}
