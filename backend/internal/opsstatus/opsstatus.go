// Package opsstatus reads the backup and point-in-time-recovery health file that the pitr service writes
// (infra/prod/pitr-loop.sh, /run/aether-ops/status.json) and turns it into what an owner may see.
//
// The file is written by our own script, but the API treats it as untrusted input all the same: it is size-bounded,
// decoded into a fixed shape, and every field that reaches a client is listed in Status. Anything else in the file
// (the WAL segment name, fields a later script adds) is dropped. Alert text is cleaned of control characters, cut
// to a bounded length and scrubbed of anything that looks like a credential.
package opsstatus

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxFileBytes bounds how much of the file is read. The real file is well under 4 KiB.
const MaxFileBytes = 64 << 10

// StaleAfter is how old the last check may be before the status itself is an alert: the pitr service checks every
// 5 minutes, so 15 minutes without a new file means it stopped or cannot write.
const StaleAfter = 15 * time.Minute

const (
	maxAlerts     = 32
	maxMessage    = 300
	OverallOK     = "ok"
	OverallWarn   = "warn"
	OverallAlert  = "alert"
	KeyStale      = "status_stale"
	KeyMissing    = "status_missing"
	KeyUnreadable = "status_invalid"
)

// Alert is one active problem reported by the pitr service.
type Alert struct {
	Key      string     `json:"key"`
	Severity string     `json:"severity"` // warn | alert
	Since    *time.Time `json:"since,omitempty"`
	Message  string     `json:"message"`
}

// PITR is the point-in-time-recovery part of the status.
type PITR struct {
	RestoreFrom     *time.Time `json:"restore_from,omitempty"` // the earliest time a restore can reach
	NewestBackup    *time.Time `json:"newest_backup,omitempty"`
	Backups         int        `json:"backups"`
	LastArchivedAt  *time.Time `json:"last_archived_at,omitempty"`
	WALWaiting      int        `json:"wal_waiting"`
	PgWALMB         int        `json:"pg_wal_mb"`
	RepoFreePercent int        `json:"repo_free_percent"`
}

// Status is everything GET /api/v1/system/status returns about backups.
type Status struct {
	Overall   string     `json:"overall"` // ok | warn | alert
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	Stale     bool       `json:"stale"`
	Alerts    []Alert    `json:"alerts"`
	PITR      PITR       `json:"pitr"`
	DumpAt    *time.Time `json:"dump_newest,omitempty"`
}

// critical are the problems that mean data written now may not be recoverable. Every other key (lagging archive,
// a growing pg_wal, an old backup, a filling disk, a late dump, a key a newer script adds) is a warning.
var critical = map[string]bool{
	"postgres": true, "archive_off": true, "archive_failing": true, "wal_dropped": true, "repo_status": true,
	"backup_failed": true, "dump_failed": true, "stanza": true,
	KeyStale: true, KeyMissing: true, KeyUnreadable: true,
}

// fileShape is the file as pitr-loop.sh writes it. Unknown fields are ignored; wrong types fail the decode.
type fileShape struct {
	OK        *bool  `json:"ok"`
	CheckedAt string `json:"checked_at"`
	Alerts    []struct {
		Key     string `json:"key"`
		Since   string `json:"since"`
		Message string `json:"message"`
	} `json:"alerts"`
	PITR struct {
		RestoreFrom     *string `json:"restore_from"`
		NewestBackup    string  `json:"newest_backup"`
		Backups         int     `json:"backups"`
		LastArchivedAt  string  `json:"last_archived_at"`
		WALWaiting      int     `json:"wal_waiting"`
		PgWALMB         int     `json:"pg_wal_mb"`
		RepoFreePercent int     `json:"repo_free_percent"`
	} `json:"pitr"`
	Dump struct {
		Newest string `json:"newest"`
	} `json:"dump"`
}

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Read loads and validates the file at path. It never returns an error: a missing, oversized or malformed file is
// itself reported as an alert, which is what the owner needs to see.
func Read(path string, now time.Time) Status {
	// Only a regular file: never a FIFO (opening one would block), a device or a directory put in its place. Checked
	// before opening and again on the open file.
	if info, e := os.Stat(path); e == nil && !info.Mode().IsRegular() {
		return problem(KeyUnreadable, "ไฟล์สถานะไม่ใช่ไฟล์ปกติ")
	}
	f, e := os.Open(path)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return problem(KeyMissing, "ยังไม่มีไฟล์สถานะจากบริการ pitr (เพิ่งติดตั้ง หรือบริการไม่ทำงาน)")
		}
		return problem(KeyUnreadable, "อ่านไฟล์สถานะไม่ได้")
	}
	defer f.Close()
	if info, e := f.Stat(); e != nil || !info.Mode().IsRegular() {
		return problem(KeyUnreadable, "ไฟล์สถานะไม่ใช่ไฟล์ปกติ")
	}
	raw, e := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if e != nil || len(raw) > MaxFileBytes {
		return problem(KeyUnreadable, "ไฟล์สถานะอ่านไม่ได้หรือใหญ่ผิดปกติ")
	}
	return Parse(raw, now)
}

// Parse validates the file content. See Read.
func Parse(raw []byte, now time.Time) Status {
	var in fileShape
	if len(raw) > MaxFileBytes || json.Unmarshal(raw, &in) != nil || in.OK == nil {
		return problem(KeyUnreadable, "ไฟล์สถานะไม่อยู่ในรูปแบบที่ระบบรู้จัก")
	}
	out := Status{Alerts: []Alert{}, CheckedAt: stamp(in.CheckedAt)}
	out.PITR = PITR{
		NewestBackup:    stamp(in.PITR.NewestBackup),
		Backups:         bounded(in.PITR.Backups, 1000),
		LastArchivedAt:  stamp(in.PITR.LastArchivedAt),
		WALWaiting:      bounded(in.PITR.WALWaiting, 1_000_000),
		PgWALMB:         bounded(in.PITR.PgWALMB, 10_000_000),
		RepoFreePercent: bounded(in.PITR.RepoFreePercent, 100),
	}
	if in.PITR.RestoreFrom != nil {
		out.PITR.RestoreFrom = stamp(*in.PITR.RestoreFrom)
	}
	out.DumpAt = stamp(in.Dump.Newest)
	seen := map[string]bool{}
	for _, a := range in.Alerts {
		if len(out.Alerts) == maxAlerts {
			break
		}
		// One entry per problem: the key identifies it (the UI keys its list on it).
		if !keyPattern.MatchString(a.Key) || seen[a.Key] {
			continue
		}
		seen[a.Key] = true
		out.Alerts = append(out.Alerts, Alert{Key: a.Key, Severity: severity(a.Key), Since: stamp(a.Since), Message: clean(a.Message)})
	}
	// The script says "ok":false only when it lists an alert; a file that disagrees with itself is not trusted.
	if !*in.OK && len(out.Alerts) == 0 {
		out.Alerts = append(out.Alerts, Alert{Key: KeyUnreadable, Severity: OverallAlert, Message: "ไฟล์สถานะแจ้งว่ามีปัญหาแต่ไม่ระบุว่าอะไร"})
	}
	if out.CheckedAt == nil || now.Sub(*out.CheckedAt) > StaleAfter || out.CheckedAt.Sub(now) > StaleAfter {
		out.Stale = true
		out.Alerts = append(out.Alerts, Alert{Key: KeyStale, Severity: OverallAlert, Since: out.CheckedAt, Message: "บริการ pitr ไม่ได้ตรวจสถานะเกิน 15 นาที ข้อมูลด้านล่างอาจไม่เป็นปัจจุบัน"})
	}
	out.Overall = overall(out.Alerts)
	return out
}

func problem(key, message string) Status {
	return Status{Overall: OverallAlert, Stale: true, Alerts: []Alert{{Key: key, Severity: OverallAlert, Message: message}}}
}

func severity(key string) string {
	if critical[key] {
		return OverallAlert
	}
	return OverallWarn
}

func overall(alerts []Alert) string {
	out := OverallOK
	for _, a := range alerts {
		if a.Severity == OverallAlert {
			return OverallAlert
		}
		out = OverallWarn
	}
	return out
}

// stamp accepts only the UTC RFC 3339 seconds form the script writes; "never", empty and anything else is nil.
func stamp(raw string) *time.Time {
	t, e := time.Parse("2006-01-02T15:04:05Z", raw)
	if e != nil || t.Year() < 2000 || t.Year() > 9999 {
		return nil
	}
	return &t
}

func bounded(n, hi int) int {
	return min(max(n, 0), hi)
}

// secretLike are fragments that must never reach a browser even if a tool put them in an error message.
var secretLike = []struct {
	re   *regexp.Regexp
	keep string
}{
	// Credentials in a URL: everything between the scheme and the LAST "@" before the host, so a password that
	// itself contains "@" or "/" is hidden whole (postgres://u:p@ss@host, postgres://u:ab/cd@host).
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)\S*@`), "${1}[hidden]@"},
	// An Authorization value.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+\S+`), "${1} [hidden]"},
	// key=value, key: value and "key": "value" whose name contains a secret-sounding word (access_token,
	// PGPASSWORD, db_password, aws_secret_access_key, cipher-pass, apikey…).
	{regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:pass(?:word)?|pwd|secret|token|key|credential)[a-z0-9_-]*"?\s*[=:]\s*)"?[^\s",}]+"?`), "${1}[hidden]"},
}

// clean makes alert text safe to show: valid UTF-8, no control characters, single spaces, no secrets, bounded.
func clean(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		// Control and format characters (Cf: bidi overrides and isolates, zero-width joiners), which could make a
		// message display differently from what it says, and the Unicode line and paragraph separators.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	for _, m := range secretLike {
		s = m.re.ReplaceAllString(s, m.keep)
	}
	if utf8.RuneCountInString(s) > maxMessage {
		r := []rune(s)
		s = string(r[:maxMessage]) + "…"
	}
	return s
}
