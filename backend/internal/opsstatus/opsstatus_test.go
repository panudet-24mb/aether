package opsstatus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)

// What pitr-loop.sh writes on a healthy system (the heredoc in check_and_report, with its line breaks).
const healthy = `{"ok":true,"checked_at":"2026-09-30T08:57:00Z","alerts":[],
 "pitr":{"restore_from":"2026-09-30T05:20:40Z","newest_backup":"2026-09-30T05:20:50Z","backups":1,"last_archived_at":"2026-09-30T08:55:00Z","last_archived_wal":"0000000100000000000000B7","wal_waiting":0,"pg_wal_mb":96,"repo_free_percent":74},
 "dump":{"newest":"2026-09-30T08:18:51Z"}}
`

func TestHealthyFile(t *testing.T) {
	s := Parse([]byte(healthy), now)
	if s.Overall != OverallOK || s.Stale || len(s.Alerts) != 0 {
		t.Fatalf("healthy: %+v", s)
	}
	if s.PITR.Backups != 1 || s.PITR.RepoFreePercent != 74 || s.PITR.PgWALMB != 96 || s.PITR.RestoreFrom == nil || !s.PITR.RestoreFrom.Equal(time.Date(2026, 9, 30, 5, 20, 40, 0, time.UTC)) {
		t.Fatalf("pitr: %+v", s.PITR)
	}
	if s.DumpAt == nil || s.CheckedAt == nil || s.PITR.LastArchivedAt == nil {
		t.Fatalf("times: %+v", s)
	}
	// Fields that are not in Status never reach the response.
	out, _ := json.Marshal(s)
	if strings.Contains(string(out), "0000000100000000000000B7") || strings.Contains(string(out), "last_archived_wal") {
		t.Fatalf("unlisted field passed through: %s", out)
	}
}

func TestAlertsAndSeverity(t *testing.T) {
	raw := `{"ok":false,"checked_at":"2026-09-30T08:58:00Z","alerts":[
	 {"key":"archive_lagging","since":"2026-09-30T08:40:00Z","message":"3 WAL segment(s) waiting to be archived, oldest for 18 min"},
	 {"key":"dump_stale","since":"never","message":"newest nightly dump is older than 30 h"}],
	 "pitr":{"restore_from":null,"newest_backup":"never","backups":0,"last_archived_at":"never","wal_waiting":3,"pg_wal_mb":10,"repo_free_percent":60},"dump":{"newest":"never"}}`
	s := Parse([]byte(raw), now)
	if s.Overall != OverallWarn || len(s.Alerts) != 2 || s.Alerts[0].Severity != OverallWarn || s.Alerts[0].Since == nil || s.Alerts[1].Since != nil {
		t.Fatalf("warnings: %+v", s)
	}
	if s.PITR.RestoreFrom != nil || s.PITR.NewestBackup != nil || s.DumpAt != nil {
		t.Fatalf("never must be absent: %+v", s)
	}
	raw = strings.Replace(raw, `"key":"dump_stale"`, `"key":"archive_failing"`, 1)
	if s := Parse([]byte(raw), now); s.Overall != OverallAlert || s.Alerts[1].Severity != OverallAlert {
		t.Fatalf("archive_failing is critical: %+v", s)
	}
}

func TestStaleness(t *testing.T) {
	s := Parse([]byte(healthy), now.Add(16*time.Minute))
	if !s.Stale || s.Overall != OverallAlert || s.Alerts[len(s.Alerts)-1].Key != KeyStale {
		t.Fatalf("16 min old: %+v", s)
	}
	if s := Parse([]byte(healthy), now.Add(10*time.Minute)); s.Stale || s.Overall != OverallOK {
		t.Fatalf("13 min since the check is fresh: %+v", s)
	}
	// A checked_at far in the future is as untrustworthy as an old one.
	if s := Parse([]byte(healthy), now.Add(-time.Hour)); !s.Stale {
		t.Fatalf("future checked_at: %+v", s)
	}
	if s := Parse([]byte(strings.Replace(healthy, "2026-09-30T08:57:00Z", "never", 1)), now); !s.Stale {
		t.Fatalf("no check yet: %+v", s)
	}
}

func TestRejectsWhatIsNotTheFile(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":         ``,
		"not json":      `ok`,
		"array":         `[]`,
		"no ok":         `{"checked_at":"2026-09-30T08:57:00Z"}`,
		"wrong type":    `{"ok":"yes"}`,
		"number typed":  `{"ok":true,"pitr":{"backups":"many"}}`,
		"self-contrary": `{"ok":false,"checked_at":"2026-09-30T08:57:00Z","alerts":[]}`,
	} {
		s := Parse([]byte(raw), now)
		if s.Overall != OverallAlert {
			t.Fatalf("%s: %+v", name, s)
		}
	}
	big := `{"ok":true,"x":"` + strings.Repeat("a", MaxFileBytes) + `"}`
	if s := Parse([]byte(big), now); s.Overall != OverallAlert || s.Alerts[0].Key != KeyUnreadable {
		t.Fatalf("oversized: %+v", s)
	}
}

func TestAlertTextIsCleaned(t *testing.T) {
	msg := "pgBackRest repository: unable to open https://backup:s3cr3t@example.com/repo\n\x1b[31m password=hunter2 key: abc " + strings.Repeat("ข", 400)
	raw, _ := json.Marshal(map[string]any{"ok": false, "checked_at": "2026-09-30T08:58:00Z", "alerts": []map[string]string{
		{"key": "repo_status", "since": "2026-09-30T08:58:00Z", "message": msg},
		{"key": "Bad Key; DROP", "message": "skipped"},
		{"key": strings.Repeat("a", 40), "message": "skipped"},
	}})
	s := Parse(raw, now)
	if len(s.Alerts) != 1 {
		t.Fatalf("bad keys must be dropped: %+v", s.Alerts)
	}
	m := s.Alerts[0].Message
	for _, leak := range []string{"s3cr3t", "hunter2", "abc", "\n", "\x1b"} {
		if strings.Contains(m, leak) {
			t.Fatalf("message keeps %q: %q", leak, m)
		}
	}
	if !strings.Contains(m, "https://[hidden]@example.com") || len([]rune(m)) > maxMessage+1 {
		t.Fatalf("message: %q", m)
	}
	// Secrets in the shapes tools actually print them; each must vanish, and the text around it must stay.
	for _, tc := range []struct{ in, secret, keep string }{
		{"refresh failed: access_token=abc123 at step 2", "abc123", "at step 2"},
		{"env PGPASSWORD=hunter2 psql", "hunter2", "psql"},
		{"db_password=x9y8 rejected", "x9y8", "rejected"},
		{"aws_secret_access_key=XYZSECRET region", "XYZSECRET", "region"},
		{"cipher-pass: s3cr3tpass used", "s3cr3tpass", "used"},
		{"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.e30.sig failed", "eyJhbGciOiJIUzI1NiJ9", "failed"},
		{`config {"password": "p4ss", "user": "u"}`, "p4ss", `"user"`},
		{"cannot reach postgres://u:p@ss@db.example:5432/aether now", "p@ss", "db.example:5432/aether now"},
		{"cannot reach postgres://u:ab/cd@db.example/aether now", "ab/cd", "db.example/aether now"},
	} {
		got := clean(tc.in)
		if strings.Contains(got, tc.secret) || !strings.Contains(got, tc.keep) || !strings.Contains(got, "[hidden]") {
			t.Fatalf("%q -> %q", tc.in, got)
		}
	}
	// Plain operational text is left alone.
	for _, plain := range []string{"3 WAL segment(s) waiting to be archived, oldest for 18 min", "pgBackRest repository: unable to open (code 41)"} {
		if got := clean(plain); got != plain {
			t.Fatalf("%q -> %q", plain, got)
		}
	}
	// Bidi overrides and zero-width characters become spaces.
	if got := clean("ok\u202Eevil\u200Bx"); strings.ContainsAny(got, "\u202E\u200B") {
		t.Fatalf("format characters kept: %q", got)
	}
	var alerts []map[string]string
	for i := 0; i < 50; i++ {
		alerts = append(alerts, map[string]string{"key": fmt.Sprintf("problem_%d", i), "message": "x"})
	}
	raw, _ = json.Marshal(map[string]any{"ok": false, "checked_at": "2026-09-30T08:58:00Z", "alerts": alerts})
	if s := Parse(raw, now); len(s.Alerts) != maxAlerts {
		t.Fatalf("alerts are capped: %d", len(s.Alerts))
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	if s := Read(filepath.Join(dir, "status.json"), now); s.Overall != OverallAlert || s.Alerts[0].Key != KeyMissing {
		t.Fatalf("missing: %+v", s)
	}
	path := filepath.Join(dir, "status.json")
	if e := os.WriteFile(path, []byte(healthy), 0o644); e != nil {
		t.Fatal(e)
	}
	if s := Read(path, now); s.Overall != OverallOK {
		t.Fatalf("read: %+v", s)
	}
	// A FIFO where the file should be is refused without blocking.
	fifo := filepath.Join(dir, "fifo.json")
	if e := syscall.Mkfifo(fifo, 0o644); e != nil {
		t.Fatal(e)
	}
	if s := Read(fifo, now); s.Alerts[0].Key != KeyUnreadable {
		t.Fatalf("fifo: %+v", s)
	}
	// A directory where the file should be is refused, not read.
	if s := Read(dir, now); s.Alerts[0].Key != KeyUnreadable {
		t.Fatalf("directory: %+v", s)
	}
	if e := os.WriteFile(path, []byte(strings.Repeat(" ", MaxFileBytes+10)), 0o644); e != nil {
		t.Fatal(e)
	}
	if s := Read(path, now); s.Alerts[0].Key != KeyUnreadable {
		t.Fatalf("oversized file: %+v", s)
	}
}

func TestDuplicateKeysAreMerged(t *testing.T) {
	raw := `{"ok":false,"checked_at":"2026-09-30T08:58:00Z","alerts":[{"key":"backup_stale","message":"a"},{"key":"backup_stale","message":"b"}]}`
	if s := Parse([]byte(raw), now); len(s.Alerts) != 1 || s.Alerts[0].Message != "a" {
		t.Fatalf("duplicates: %+v", s.Alerts)
	}
}
