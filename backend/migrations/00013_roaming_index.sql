-- +goose Up
SET ROLE aether_owner;
-- Read on every uplink to know which identities are roaming.
CREATE INDEX devices_roaming ON core.devices(tenant_id) WHERE roaming AND removed_at IS NULL;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP INDEX core.devices_roaming;
RESET ROLE;
