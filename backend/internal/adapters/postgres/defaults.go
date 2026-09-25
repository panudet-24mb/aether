package postgres

import (
	"aether/backend/internal/domain"
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// builtinRules are the alert rules every workspace gets without configuring anything. A panic button that
// stays silent until somebody remembers to write a rule for it is a safety gap, so `button` is critical out
// of the box with a short dedupe window (a nurse pressing twice is one emergency, not two), and `tamper`
// warns. They are ordinary rules: the owner may rename, re-scope, disable or delete them.
// Migrations 00024 (SOS, tamper) and 00030 (hazard) backfill exactly these for the workspaces that already
// existed.
var builtinRules = []domain.AlertRule{
	{Name: "ปุ่มฉุกเฉิน SOS", EventType: domain.EventButton, Severity: "critical", DedupeSec: 30},
	{Name: "ป้ายถูกถอด (tamper)", EventType: domain.EventTamper, Severity: "warning", DedupeSec: 600},
	// Smoke, gas and carbon monoxide detectors (Zigbee2MQTT); migration 00030 backfills it.
	{Name: "ควัน / แก๊ส / CO", EventType: domain.EventHazard, Severity: "critical", DedupeSec: 300},
}

// seedDefaultRules gives a brand-new workspace the built-in rules. Called once, from CreateAccount, inside
// that transaction: an account either exists with its rules or does not exist at all.
// The NOT EXISTS guard is the same one the migration uses, so a workspace that somehow already has a rule
// for the event type keeps its own.
func seedDefaultRules(tx *gorm.DB, tenant string) error {
	for _, rule := range builtinRules {
		if e := tx.Exec(`INSERT INTO core.alert_rules(tenant_id,id,name,enabled,event_type,severity,scope,channels,dedupe_sec,builtin)
			SELECT ?,?,?,true,?,?,'{}'::jsonb,'[]'::jsonb,?,true
			WHERE NOT EXISTS(SELECT 1 FROM core.alert_rules WHERE event_type=?)
			ON CONFLICT DO NOTHING`,
			tenant, uuid.NewString(), rule.Name, rule.EventType, rule.Severity, rule.DedupeSec, rule.EventType).Error; e != nil {
			return e
		}
	}
	return nil
}

// BuiltinRuleIDs lists the rules of this workspace that Aether seeded itself, so the API can label them.
// It is a separate lookup rather than a column on the listing because `builtin` is presentation, not
// behaviour: nothing in the alert pipeline reads it.
func (r *Repository) BuiltinRuleIDs(ctx context.Context, p domain.Principal) ([]string, error) {
	out := []string{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT id FROM core.alert_rules WHERE builtin ORDER BY created_at,id LIMIT 100`).Scan(&out).Error
	})
	return out, e
}
