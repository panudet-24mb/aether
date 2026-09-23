-- +goose Up
SET ROLE aether_owner;
-- Built-in alert rules. A panic button that only opens an alert once somebody has created a rule for it
-- is a safety gap: the live workspace had two leftover test rules and none for `button`, so pressing SOS
-- on a B10 produced an event row and nothing else. Every workspace therefore starts with two rules it did
-- not have to configure — `button` at critical with a short dedupe window so a second press inside half a
-- minute does not open a second alert, and `tamper` at warning — and the owner may edit, disable or delete
-- them like any other rule.
--
-- `builtin` only records where a rule came from. It is not a protection flag: nothing in the API treats a
-- built-in rule differently, it is the label the rules tab shows ("สร้างให้อัตโนมัติ"). Editing a rule
-- keeps the flag, because core.alert_rules is updated column by column (postgres.SaveRule).
ALTER TABLE core.alert_rules ADD COLUMN builtin boolean NOT NULL DEFAULT false;

-- Backfill for the workspaces that already exist. FORCE ROW LEVEL SECURITY applies to the table owner too
-- and core.tenant_id() is not set during a migration (see the note in 00020), so the policy is lifted for
-- the length of the two statements and restored immediately; aether_app is never affected.
ALTER TABLE core.alert_rules NO FORCE ROW LEVEL SECURITY;
-- Only where the workspace has no rule of that event type at all: an operator who already decided how
-- button or tamper should behave must not be given a second, louder opinion behind their back.
-- ON CONFLICT covers UNIQUE(tenant_id,name) in the unlikely case the name is already taken.
INSERT INTO core.alert_rules(tenant_id,id,name,enabled,event_type,severity,scope,channels,dedupe_sec,builtin)
SELECT t.id, gen_random_uuid(), 'ปุ่มฉุกเฉิน SOS', true, 'button', 'critical', '{}'::jsonb, '[]'::jsonb, 30, true
 FROM core.tenants t
 WHERE NOT EXISTS(SELECT 1 FROM core.alert_rules r WHERE r.tenant_id=t.id AND r.event_type='button')
ON CONFLICT DO NOTHING;
INSERT INTO core.alert_rules(tenant_id,id,name,enabled,event_type,severity,scope,channels,dedupe_sec,builtin)
SELECT t.id, gen_random_uuid(), 'ป้ายถูกถอด (tamper)', true, 'tamper', 'warning', '{}'::jsonb, '[]'::jsonb, 600, true
 FROM core.tenants t
 WHERE NOT EXISTS(SELECT 1 FROM core.alert_rules r WHERE r.tenant_id=t.id AND r.event_type='tamper')
ON CONFLICT DO NOTHING;
ALTER TABLE core.alert_rules FORCE ROW LEVEL SECURITY;

-- No project_scope policy is added here. core.alert_rules is deliberately unscoped (listed as such at the
-- end of 00019_project_access.sql: the rule routes are owner/admin-only and owner/admin see everything);
-- the existing tenant_scope policy and the table-level grants from 00006 already cover the new column.
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
ALTER TABLE core.alert_rules NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.alert_rules WHERE builtin;
ALTER TABLE core.alert_rules FORCE ROW LEVEL SECURITY;
ALTER TABLE core.alert_rules DROP COLUMN builtin;
RESET ROLE;
