package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GET /api/v1/system/status: owners only, off without OPS_STATUS_FILE, and only the validated fields of the pitr
// service's file reach the client.
func TestSystemStatusIsOwnerOnly(t *testing.T) {
	f := setup(t)
	_, ownerAuth, owner := f.account(t)
	path := filepath.Join(t.TempDir(), "status.json")
	checked := time.Now().UTC().Add(-2 * time.Minute).Format("2006-01-02T15:04:05Z")
	file := `{"ok":false,"checked_at":"` + checked + `","alerts":[{"key":"archive_failing","since":"` + checked + `","message":"WAL archiving is failing; see https://ops:hunter2@example.com"}],
	 "pitr":{"restore_from":null,"newest_backup":"never","backups":0,"last_archived_at":"never","last_archived_wal":"0000000100000000000000B7","wal_waiting":4,"pg_wal_mb":80,"repo_free_percent":50},"dump":{"newest":"never"}}`
	if e := os.WriteFile(path, []byte(file), 0o644); e != nil {
		t.Fatal(e)
	}
	cfg := f.cfg
	cfg.APIRateLimit = 600
	cfg.OpsStatusFile = path
	api := httpapi.New(cfg, f.service, f.repo)

	code, out, _ := req(t, api, "GET", "/api/v1/system/status", "Bearer "+ownerAuth.AccessToken, "", "", nil)
	if code != 200 || out["configured"] != true {
		t.Fatalf("owner: %d %v", code, out)
	}
	backup := out["backup"].(map[string]any)
	alerts := backup["alerts"].([]any)
	if backup["overall"] != "alert" || len(alerts) != 1 || alerts[0].(map[string]any)["key"] != "archive_failing" {
		t.Fatalf("status: %v", backup)
	}
	if msg := alerts[0].(map[string]any)["message"].(string); strings.Contains(msg, "hunter2") {
		t.Fatalf("a credential reached the client: %q", msg)
	}
	if _, leaked := backup["pitr"].(map[string]any)["last_archived_wal"]; leaked {
		t.Fatalf("an unlisted field reached the client: %v", backup["pitr"])
	}

	// Every other role is refused, whatever their module access.
	for _, role := range []string{"admin", "operator", "viewer"} {
		email := memberEmail()
		addMember(t, api, ownerAuth.AccessToken, email, role, []string{})
		auth, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
		if code, _, _ := req(t, api, "GET", "/api/v1/system/status", "Bearer "+auth.AccessToken, "", "", nil); code != 403 {
			t.Fatalf("%s: %d", role, code)
		}
	}
	if code, _, _ := req(t, api, "GET", "/api/v1/system/status", "", "", "", nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}

	// The status is about the whole deployment: with OPS_STATUS_TENANT set, only that workspace's owners see it.
	_, otherAuth, _ := f.account(t)
	if code, _, _ := req(t, api, "GET", "/api/v1/system/status", "Bearer "+otherAuth.AccessToken, "", "", nil); code != 200 {
		t.Fatalf("unset OPS_STATUS_TENANT: every owner may read it, got %d", code)
	}
	cfg.OpsStatusTenant = owner.TenantID
	scoped := httpapi.New(cfg, f.service, f.repo)
	if code, _, _ := req(t, scoped, "GET", "/api/v1/system/status", "Bearer "+otherAuth.AccessToken, "", "", nil); code != 403 {
		t.Fatalf("owner of another workspace: %d", code)
	}
	if code, _, _ := req(t, scoped, "GET", "/api/v1/system/status", "Bearer "+ownerAuth.AccessToken, "", "", nil); code != 200 {
		t.Fatalf("owner of the status workspace: %d", code)
	}
	cfg.OpsStatusTenant = ""

	// Without the file configured (development) the feature is off, not an alert.
	cfg.OpsStatusFile = ""
	off := httpapi.New(cfg, f.service, f.repo)
	if code, out, _ := req(t, off, "GET", "/api/v1/system/status", "Bearer "+ownerAuth.AccessToken, "", "", nil); code != 200 || out["configured"] != false || out["backup"] != nil {
		t.Fatalf("unconfigured: %d %v", code, out)
	}
	// A configured file that is missing is itself an alert.
	cfg.OpsStatusFile = filepath.Join(t.TempDir(), "absent.json")
	missing := httpapi.New(cfg, f.service, f.repo)
	if code, out, _ := req(t, missing, "GET", "/api/v1/system/status", "Bearer "+ownerAuth.AccessToken, "", "", nil); code != 200 || out["backup"].(map[string]any)["overall"] != "alert" {
		t.Fatalf("missing file: %d %v", code, out)
	}
}
