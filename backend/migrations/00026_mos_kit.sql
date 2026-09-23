-- Minew MOS smart-office kit: MG4 gateway, MSP01 PIR occupancy, S4 door sensor.
--
-- What this adds to the schema (the rest of the kit is catalog data in internal/domain/catalog.go):
--   * alert rule event types `door` (fires on door_open) and `occupancy` (fires on occupied). Their
--     optional after-hours window lives inside the existing scope jsonb, so no column is needed;
--   * `door` as a meaning an operator can teach (the S4 frame layout is not public, so the door state
--     comes from a learned signal, never from a guessed decoder);
--   * per-stream door and occupancy state in core.stream_state, next to the tamper/leak/moving flags it
--     is edge-triggered exactly like: `door` (last known 1 = open), `occupied` (1 from an `occupied`
--     event until the periodic scan raises `vacant`) and `last_motion_at` (server receive time of the
--     last PIR motion=1 uplink; device clocks are never trusted).
--
-- NUMBERING: 00027 was once reserved for a frontend change that shipped without a migration; it is now
-- 00027_button_trigger.sql.
--
-- +goose Up
SET ROLE aether_owner;

ALTER TABLE core.alert_rules DROP CONSTRAINT alert_rules_event_type_check;
ALTER TABLE core.alert_rules ADD CONSTRAINT alert_rules_event_type_check
 CHECK(event_type IN ('tamper','button','leak','motion','offline','threshold','zone','door','occupancy'));

ALTER TABLE core.signal_sessions DROP CONSTRAINT signal_sessions_event_type_check;
ALTER TABLE core.signal_sessions ADD CONSTRAINT signal_sessions_event_type_check
 CHECK(event_type IN ('button','tamper','leak','motion','door','custom'));
ALTER TABLE core.device_signals DROP CONSTRAINT device_signals_event_type_check;
ALTER TABLE core.device_signals ADD CONSTRAINT device_signals_event_type_check
 CHECK(event_type IN ('button','tamper','leak','motion','door','custom'));

ALTER TABLE core.stream_state
 ADD COLUMN door smallint NOT NULL DEFAULT 0 CHECK(door IN (0,1)),
 ADD COLUMN occupied smallint NOT NULL DEFAULT 0 CHECK(occupied IN (0,1)),
 ADD COLUMN last_motion_at timestamptz;
-- The vacancy scan (postgres.ScanVacancy) runs every worker tick and only ever looks at open episodes.
CREATE INDEX stream_state_occupied ON core.stream_state(tenant_id,last_motion_at) WHERE occupied=1;

-- No policy or grant changes: core.stream_state, core.alert_rules, core.signal_sessions and
-- core.device_signals keep their existing tenant_scope / project_scope policies (00006, 00019, 00025)
-- and table-level grants, which already cover the new columns and values.
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP INDEX core.stream_state_occupied;
ALTER TABLE core.stream_state DROP COLUMN last_motion_at, DROP COLUMN occupied, DROP COLUMN door;

-- Rows using the new values cannot survive the old CHECKs. FORCE ROW LEVEL SECURITY applies to the table
-- owner too and core.tenant_id() is not set during a migration (see 00024), so the policy is lifted for
-- the length of each delete and restored immediately; aether_app is never affected.
ALTER TABLE core.device_signals NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.device_signals WHERE event_type='door';
ALTER TABLE core.device_signals FORCE ROW LEVEL SECURITY;
ALTER TABLE core.signal_sessions NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.signal_sessions WHERE event_type='door';
ALTER TABLE core.signal_sessions FORCE ROW LEVEL SECURITY;
ALTER TABLE core.alert_rules NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.alert_rules WHERE event_type IN ('door','occupancy');
ALTER TABLE core.alert_rules FORCE ROW LEVEL SECURITY;

ALTER TABLE core.device_signals DROP CONSTRAINT device_signals_event_type_check;
ALTER TABLE core.device_signals ADD CONSTRAINT device_signals_event_type_check
 CHECK(event_type IN ('button','tamper','leak','motion','custom'));
ALTER TABLE core.signal_sessions DROP CONSTRAINT signal_sessions_event_type_check;
ALTER TABLE core.signal_sessions ADD CONSTRAINT signal_sessions_event_type_check
 CHECK(event_type IN ('button','tamper','leak','motion','custom'));
ALTER TABLE core.alert_rules DROP CONSTRAINT alert_rules_event_type_check;
ALTER TABLE core.alert_rules ADD CONSTRAINT alert_rules_event_type_check
 CHECK(event_type IN ('tamper','button','leak','motion','offline','threshold','zone'));
RESET ROLE;
