-- +goose Up
SET ROLE aether_owner;
-- Readings are looked up by identity across gateways (roaming, asset registry, automation conditions);
-- the only existing index starts with gateway_id.
CREATE INDEX sensor_samples_identity ON core.sensor_samples(tenant_id,external_id,received_at DESC);
-- When the zone candidate was last heard, so a candidate that went quiet cannot win on one late sample.
ALTER TABLE core.presence_state ADD COLUMN candidate_seen timestamptz;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
ALTER TABLE core.presence_state DROP COLUMN candidate_seen;
DROP INDEX core.sensor_samples_identity;
RESET ROLE;
