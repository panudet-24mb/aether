-- +goose Up
SET ROLE aether_owner;
-- Soft removal: a registration can be withdrawn (and later restored) without touching telemetry history.
ALTER TABLE core.devices ADD COLUMN removed_at timestamptz, ADD COLUMN removed_by uuid;
-- Identity must stay unique only among active registrations so the same tag can be adopted again.
ALTER TABLE core.devices DROP CONSTRAINT devices_tenant_id_gateway_id_external_id_key;
CREATE UNIQUE INDEX devices_active_identity ON core.devices(tenant_id,gateway_id,external_id) WHERE removed_at IS NULL;
-- Column-scoped: the runtime may rename, move and (un)remove, never rewrite identity or profile.
GRANT UPDATE(name,gateway_id,removed_at,removed_by) ON core.devices TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
REVOKE UPDATE(name,gateway_id,removed_at,removed_by) ON core.devices FROM aether_app;
DROP INDEX core.devices_active_identity;
DELETE FROM core.device_state WHERE device_id IN (SELECT id FROM core.devices WHERE removed_at IS NOT NULL);
DELETE FROM core.telemetry WHERE device_id IN (SELECT id FROM core.devices WHERE removed_at IS NOT NULL);
DELETE FROM core.devices WHERE removed_at IS NOT NULL;
ALTER TABLE core.devices ADD CONSTRAINT devices_tenant_id_gateway_id_external_id_key UNIQUE(tenant_id,gateway_id,external_id);
ALTER TABLE core.devices DROP COLUMN removed_at, DROP COLUMN removed_by;
RESET ROLE;
