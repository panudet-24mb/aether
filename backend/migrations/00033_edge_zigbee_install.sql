-- Aether Edge installer, phase E: an install code may pair a Zigbee2MQTT gateway, so one command on the site host
-- brings up the Edge agent and Zigbee2MQTT together. When set, redeeming the code also rotates that Zigbee2MQTT
-- gateway's MQTT password and hands it to the installer once, next to the Edge's own credentials.
--
-- +goose Up
SET ROLE aether_owner;
ALTER TABLE core.edge_install_codes ADD COLUMN zigbee_gateway_id uuid;
ALTER TABLE core.edge_install_codes ADD CONSTRAINT edge_install_codes_zigbee_fkey
 FOREIGN KEY(tenant_id,zigbee_gateway_id) REFERENCES core.gateways(tenant_id,id);
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
ALTER TABLE core.edge_install_codes DROP COLUMN zigbee_gateway_id;
RESET ROLE;
