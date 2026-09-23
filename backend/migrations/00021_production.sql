-- +goose Up
SET ROLE aether_owner;
-- Per-workspace capacity instead of hard-coded caps (a ward has hundreds of tags).
ALTER TABLE core.tenants ADD COLUMN max_gateways integer NOT NULL DEFAULT 100 CHECK(max_gateways BETWEEN 1 AND 5000),
  ADD COLUMN max_devices integer NOT NULL DEFAULT 2000 CHECK(max_devices BETWEEN 1 AND 100000);
-- Sample thinning keeps its own clock so last_seen (offline detection) still moves on every uplink.
ALTER TABLE core.sensor_streams ADD COLUMN last_sample_at timestamptz;
-- Retention runs in the worker, outside the ingest transaction.
GRANT DELETE ON core.sensor_samples TO aether_app;
-- Both tables are append-only in time order: a BRIN index finds old rows for retention at almost no write cost.
CREATE INDEX sensor_samples_received_brin ON core.sensor_samples USING brin(received_at);
CREATE INDEX ble_history_received_brin ON core.ble_history USING brin(received_at);
-- The ten-minute prune and the alert pages look these up.
CREATE INDEX notifications_alert ON core.notifications(tenant_id,alert_id);
CREATE INDEX alerts_event ON core.alerts(tenant_id,event_id);
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP INDEX core.alerts_event;
DROP INDEX core.ble_history_received_brin;
DROP INDEX core.sensor_samples_received_brin;
DROP INDEX core.notifications_alert;
REVOKE DELETE ON core.sensor_samples FROM aether_app;
ALTER TABLE core.sensor_streams DROP COLUMN last_sample_at;
ALTER TABLE core.tenants DROP COLUMN max_gateways, DROP COLUMN max_devices;
RESET ROLE;
