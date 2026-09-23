-- +goose Up
SET ROLE aether_owner;
CREATE TABLE core.ble_history (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
 event_key text NOT NULL, received_at timestamptz NOT NULL, raw text NOT NULL,
 source text NOT NULL,
 PRIMARY KEY(tenant_id,gateway_id,external_id,event_key),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);
CREATE INDEX ble_history_stream ON core.ble_history(tenant_id,gateway_id,external_id,received_at DESC,event_key);
CREATE INDEX ble_history_gateway ON core.ble_history(tenant_id,gateway_id,received_at DESC,event_key,external_id);
ALTER TABLE core.ble_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.ble_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.ble_history USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
GRANT SELECT,INSERT,DELETE ON core.ble_history TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.ble_history;
RESET ROLE;
