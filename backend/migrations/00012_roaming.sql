-- +goose Up
SET ROLE aether_owner;
-- Wearables move between gateways. A roaming registration keeps its home gateway but is followed across every
-- gateway of the workspace that hears the same identity (presence, freshest reading, one offline episode).
ALTER TABLE core.devices ADD COLUMN roaming boolean NOT NULL DEFAULT false;
GRANT UPDATE(roaming) ON core.devices TO aether_app;
CREATE INDEX sensor_streams_identity ON core.sensor_streams(tenant_id,external_id,last_seen DESC);
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP INDEX core.sensor_streams_identity;
REVOKE UPDATE(roaming) ON core.devices FROM aether_app;
ALTER TABLE core.devices DROP COLUMN roaming;
RESET ROLE;
