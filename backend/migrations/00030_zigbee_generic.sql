-- Every device Zigbee2MQTT supports, read through its own definition (exposes) instead of per-model decoders.
--
-- What this adds:
--   * core.z2m_devices.category / sos / description: derived from the definition each time bridge/devices
--     arrives. `category` is the reading kind the UI picks a card from (environment, door, occupancy, leak,
--     switch, lighting, cover, lock, climate, remote, sos, hazard, metering, ...). `sos` is true when the
--     definition can call for help (an action value emergency / sos / panic, or an SOS binary); only such a
--     registered device may raise the `button` event from a Zigbee2MQTT message, so an ordinary remote's
--     presses never ring. `description` is the definition's own text for discovery.
--   * rule event type `hazard` (smoke, gas and carbon monoxide detectors) and a built-in critical rule for it in
--     every workspace that has none, like the SOS and tamper rules of 00024. It is an ordinary rule the owner
--     may edit, disable or delete.
--
-- +goose Up
SET ROLE aether_owner;

ALTER TABLE core.z2m_devices ADD COLUMN category text NOT NULL DEFAULT '' CHECK(length(category)<=32);
ALTER TABLE core.z2m_devices ADD COLUMN sos boolean NOT NULL DEFAULT false;
ALTER TABLE core.z2m_devices ADD COLUMN description text NOT NULL DEFAULT '' CHECK(length(description)<=160);

ALTER TABLE core.alert_rules DROP CONSTRAINT alert_rules_event_type_check;
ALTER TABLE core.alert_rules ADD CONSTRAINT alert_rules_event_type_check
 CHECK(event_type IN ('tamper','button','leak','motion','offline','threshold','zone','door','occupancy','hazard'));

-- Backfill for existing workspaces. FORCE ROW LEVEL SECURITY applies to the table owner too and
-- core.tenant_id() is not set during a migration (see 00020 / 00024), so the policy is lifted for the length of
-- the statement and restored immediately; aether_app is never affected.
ALTER TABLE core.alert_rules NO FORCE ROW LEVEL SECURITY;
INSERT INTO core.alert_rules(tenant_id,id,name,enabled,event_type,severity,scope,channels,dedupe_sec,builtin)
SELECT t.id, gen_random_uuid(), 'ควัน / แก๊ส / CO', true, 'hazard', 'critical', '{}'::jsonb, '[]'::jsonb, 300, true
 FROM core.tenants t
 WHERE NOT EXISTS(SELECT 1 FROM core.alert_rules r WHERE r.tenant_id=t.id AND r.event_type='hazard')
ON CONFLICT DO NOTHING;
ALTER TABLE core.alert_rules FORCE ROW LEVEL SECURITY;

RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
-- Rows using the new value cannot survive the old CHECK. Alerts keep their history but let go of the rule
-- first: the (tenant_id,rule_id) foreign key's ON DELETE SET NULL would try to null tenant_id as well.
ALTER TABLE core.alerts NO FORCE ROW LEVEL SECURITY;
UPDATE core.alerts SET rule_id=NULL WHERE event_type='hazard';
ALTER TABLE core.alerts FORCE ROW LEVEL SECURITY;
ALTER TABLE core.alert_rules NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.alert_rules WHERE event_type='hazard';
ALTER TABLE core.alert_rules FORCE ROW LEVEL SECURITY;
ALTER TABLE core.alert_rules DROP CONSTRAINT alert_rules_event_type_check;
ALTER TABLE core.alert_rules ADD CONSTRAINT alert_rules_event_type_check
 CHECK(event_type IN ('tamper','button','leak','motion','offline','threshold','zone','door','occupancy'));
ALTER TABLE core.z2m_devices DROP COLUMN description, DROP COLUMN sos, DROP COLUMN category;
RESET ROLE;
