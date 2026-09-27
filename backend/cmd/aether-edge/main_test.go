package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health")
	if health(path) != 1 {
		t.Fatal("missing heartbeat file reported healthy")
	}
	os.WriteFile(path, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o600)
	if health(path) != 0 {
		t.Fatal("fresh heartbeat reported unhealthy")
	}
	os.WriteFile(path, []byte(strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)), 0o600)
	if health(path) != 1 {
		t.Fatal("stale heartbeat reported healthy")
	}
	os.WriteFile(path, []byte("garbage"), 0o600)
	if health(path) != 1 {
		t.Fatal("garbage heartbeat reported healthy")
	}
}
