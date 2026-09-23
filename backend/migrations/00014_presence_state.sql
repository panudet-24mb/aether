-- +goose Up
SET ROLE aether_owner;
-- Stable zone of a roaming wearable. Raw RSSI swings by 10 dB or more on a moving person, so the zone is decided
-- from a smoothed RSSI per gateway with a margin and a dwell time, and remembered here between uplinks.
ALTER TABLE core.stream_state ADD COLUMN rssi_avg real, ADD COLUMN rssi_at timestamptz;
CREATE TABLE core.presence_state (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), external_id text NOT NULL,
 gateway_id uuid, since timestamptz,
 candidate_gateway_id uuid, candidate_since timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,external_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id),
 FOREIGN KEY(tenant_id,candidate_gateway_id) REFERENCES core.gateways(tenant_id,id)
);
ALTER TABLE core.presence_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.presence_state FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.presence_state USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
GRANT SELECT,INSERT,UPDATE,DELETE ON core.presence_state TO aether_app;
-- Rules may now react to a wearable entering a zone.
ALTER TABLE core.alert_rules DROP CONSTRAINT alert_rules_event_type_check;
ALTER TABLE core.alert_rules ADD CONSTRAINT alert_rules_event_type_check CHECK(event_type IN ('tamper','button','leak','motion','offline','threshold','zone'));
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DELETE FROM core.alert_rules WHERE event_type='zone';
ALTER TABLE core.alert_rules DROP CONSTRAINT alert_rules_event_type_check;
ALTER TABLE core.alert_rules ADD CONSTRAINT alert_rules_event_type_check CHECK(event_type IN ('tamper','button','leak','motion','offline','threshold'));
DROP TABLE core.presence_state;
ALTER TABLE core.stream_state DROP COLUMN rssi_avg, DROP COLUMN rssi_at;
RESET ROLE;
