package app

import (
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/domain"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestTuyaJobsTTLAndLimits(t *testing.T) {
	j := &tuyaJobs{jobs: map[string]*TuyaImportJob{}}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	job, e := j.start("tenant-a", "gw-1", "us", now)
	if e != nil {
		t.Fatal(e)
	}
	// One running import per gateway.
	if _, e := j.start("tenant-a", "gw-1", "us", now); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("second running import: %v", e)
	}
	// Another workspace cannot read it.
	if _, ok := j.get("tenant-b", "gw-1", job.ID, now); ok {
		t.Fatal("cross-tenant read")
	}
	if _, ok := j.get("tenant-a", "gw-2", job.ID, now); ok {
		t.Fatal("read through another gateway")
	}
	if _, ok := j.get("tenant-a", "gw-1", job.ID, now.Add(TuyaImportTTL-time.Second)); !ok {
		t.Fatal("job gone before its TTL")
	}
	// Gone after the TTL.
	if _, ok := j.get("tenant-a", "gw-1", job.ID, now.Add(TuyaImportTTL+time.Second)); ok {
		t.Fatal("job outlived its TTL")
	}
	// A bounded number of jobs per workspace.
	for i := 0; i < maxJobsPerTenant; i++ {
		if _, e := j.start("tenant-c", fmt.Sprintf("gw-%d", i), "us", now); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := j.start("tenant-c", "gw-x", "us", now); !errors.Is(e, domain.ErrRateLimited) {
		t.Fatalf("job cap: %v", e)
	}
}

func TestImportErrorNames(t *testing.T) {
	for want, e := range map[string]error{
		"tuya_auth_failed":    &tuyacloud.APIError{Kind: tuyacloud.ErrAuth, Code: 1004},
		"tuya_not_subscribed": &tuyacloud.APIError{Kind: tuyacloud.ErrNotSubscribed, Code: 28841101},
		"tuya_rate_limited":   &tuyacloud.APIError{Kind: tuyacloud.ErrRateLimited},
		"tuya_unreachable":    &tuyacloud.APIError{Kind: tuyacloud.ErrUnavailable},
		"tuya_timeout":        context.DeadlineExceeded,
		"tuya_error":          errors.New("other"),
	} {
		if got, _ := importError(e); got != want {
			t.Fatalf("%v: %s, want %s", e, got, want)
		}
	}
	if _, code := importError(&tuyacloud.APIError{Kind: tuyacloud.ErrAuth, Code: 1004}); code != 1004 {
		t.Fatal("tuya code lost")
	}
}
