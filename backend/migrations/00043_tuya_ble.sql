-- Tuya BLE devices reached locally through Aether Edge (phase B2, docs/platform/tuya-ble.md).
--
-- An agent built with BLE support scans with the host's Bluetooth adapter and connects to Tuya BLE devices with the
-- same imported local key the Wi-Fi path uses (plus a sec_key on newer devices). What this adds:
--   * core.tuya_devices: how the device is reached (`transport`: 'wifi', 'ble' or 'unknown'; every existing row is
--     'wifi', so nothing changes for them), its BLE address and uuid, its sec_key sealed like local_key_sealed, the
--     protocol version it advertises, how the agent should reach it (`ble_mode`: 'auto' reads when it advertises,
--     'on_demand' only for commands and polls, 'persistent' keeps the connection), how often to read it
--     (`ble_poll_seconds`, 5 minutes to a day: battery devices pay for every connection), when it was last read and
--     the signal strength it was last heard at;
--   * core.edge_ble_seen: Tuya BLE devices the agent hears advertising (address, uuid, product id, protocol,
--     bound flag, FD50 service, signal strength), what the import is matched against, like core.edge_lan_devices;
--   * core.edge_agents: the agent's Bluetooth state from its heartbeat, the devices it hears and holds, the
--     capabilities it announced on its last configuration pull, and when its last sightings list was accepted (the
--     server keeps at most one per 30 s);
--   * devices_tuya_one_mode: covers the BLE profile too, so one physical device is registered one way only;
--   * device_commands.confirm_sec: how long a sent command may wait for the device's confirming report before it
--     times out. 10 s as before, 45 s for a BLE device (the agent may first have to wait for it to advertise,
--     connect and authenticate). The agent is told to drop the command well before that (docs/platform/tuya-ble.md).
--
-- Nothing reaches an agent before the server's EDGE_BLE flag is on and the agent announces the capability.
--
-- +goose Up
SET ROLE aether_owner;
-- Every ALTER below takes ACCESS EXCLUSIVE on its table (core.tuya_devices, core.edge_agents, core.device_commands;
-- the new columns have constant defaults, so no rewrite), and replacing the index takes ACCESS EXCLUSIVE on
-- core.devices for the DROP and SHARE for the build. All are held until commit. Queueing behind a long reader must
-- fail the migration (it can be retried) rather than stall ingest behind it.
SET LOCAL lock_timeout = '5s';

ALTER TABLE core.tuya_devices
 ADD COLUMN transport text NOT NULL DEFAULT 'wifi' CHECK(transport IN ('wifi','ble','unknown')),
 ADD COLUMN ble_mac text NOT NULL DEFAULT '' CHECK(ble_mac ~ '^([0-9a-f]{2}(:[0-9a-f]{2}){5})?$'),
 ADD COLUMN ble_uuid text NOT NULL DEFAULT '' CHECK(ble_uuid ~ '^[A-Za-z0-9]{0,64}$'),
 ADD COLUMN sec_key_sealed text CHECK(sec_key_sealed IS NULL OR length(sec_key_sealed)<=512),
 ADD COLUMN ble_protocol smallint NOT NULL DEFAULT 0 CHECK(ble_protocol IN (0,2,3,4)),
 ADD COLUMN ble_mode text NOT NULL DEFAULT 'auto' CHECK(ble_mode IN ('auto','on_demand','persistent')),
 ADD COLUMN ble_poll_seconds integer NOT NULL DEFAULT 900 CHECK(ble_poll_seconds BETWEEN 300 AND 86400),
 ADD COLUMN last_read_at timestamptz,
 ADD COLUMN rssi smallint CHECK(rssi IS NULL OR rssi BETWEEN -127 AND 20);

CREATE TABLE core.edge_ble_seen (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 mac text NOT NULL CHECK(mac ~ '^[0-9a-f]{2}(:[0-9a-f]{2}){5}$'),
 uuid text NOT NULL DEFAULT '' CHECK(uuid ~ '^[A-Za-z0-9]{0,64}$'),
 product_id text NOT NULL DEFAULT '' CHECK(product_id ~ '^[A-Za-z0-9]{0,64}$'),
 protocol smallint NOT NULL DEFAULT 0 CHECK(protocol BETWEEN 0 AND 15),
 bound boolean NOT NULL DEFAULT false,
 fd50 boolean NOT NULL DEFAULT false,
 rssi smallint CHECK(rssi IS NULL OR rssi BETWEEN -127 AND 20),
 last_seen timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,gateway_id,mac),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);
ALTER TABLE core.edge_ble_seen ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.edge_ble_seen FORCE ROW LEVEL SECURITY;
-- Permissive tenant isolation plus the RESTRICTIVE project scope keyed by gateway, as on core.edge_lan_devices.
CREATE POLICY tenant_scope ON core.edge_ble_seen USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.edge_ble_seen AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id)) WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
GRANT SELECT,INSERT,UPDATE,DELETE ON core.edge_ble_seen TO aether_app;

ALTER TABLE core.edge_agents
 ADD COLUMN ble_state text NOT NULL DEFAULT '' CHECK(ble_state IN ('','off','ok','no_adapter','no_permission','error')),
 ADD COLUMN ble_seen integer NOT NULL DEFAULT 0 CHECK(ble_seen BETWEEN 0 AND 100000),
 ADD COLUMN ble_connected integer NOT NULL DEFAULT 0 CHECK(ble_connected BETWEEN 0 AND 100),
 ADD COLUMN capabilities text[] NOT NULL DEFAULT '{}' CHECK(cardinality(capabilities)<=8),
 ADD COLUMN last_ble_at timestamptz;

ALTER TABLE core.device_commands ADD COLUMN confirm_sec smallint NOT NULL DEFAULT 10 CHECK(confirm_sec BETWEEN 5 AND 120);

-- One mode per physical Tuya device within a workspace, now across the BLE profile too. If this fails, a device is
-- registered twice already: remove one of the registrations, then migrate again.
DROP INDEX core.devices_tuya_one_mode;
CREATE UNIQUE INDEX devices_tuya_one_mode ON core.devices(tenant_id,lower(external_id))
 WHERE removed_at IS NULL AND profile_id IN ('tuya-wifi-device@1','tuya-cloud-device@1','tuya-ble-device@1');
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';
-- Going back would strand every live BLE registration (its agent would be sent Wi-Fi settings it cannot use):
-- refuse while one exists. FORCE row level security would hide other tenants' registrations from the owner.
ALTER TABLE core.devices NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS(SELECT 1 FROM core.devices WHERE profile_id='tuya-ble-device@1' AND removed_at IS NULL) THEN
  RAISE EXCEPTION 'live Tuya BLE registrations exist: remove them before migrating below 00043';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE core.devices FORCE ROW LEVEL SECURITY;
DROP INDEX core.devices_tuya_one_mode;
CREATE UNIQUE INDEX devices_tuya_one_mode ON core.devices(tenant_id,lower(external_id))
 WHERE removed_at IS NULL AND profile_id IN ('tuya-wifi-device@1','tuya-cloud-device@1');
ALTER TABLE core.device_commands DROP COLUMN confirm_sec;
ALTER TABLE core.edge_agents DROP COLUMN ble_state, DROP COLUMN ble_seen, DROP COLUMN ble_connected, DROP COLUMN capabilities, DROP COLUMN last_ble_at;
DROP TABLE core.edge_ble_seen;
ALTER TABLE core.tuya_devices DROP COLUMN transport, DROP COLUMN ble_mac, DROP COLUMN ble_uuid, DROP COLUMN sec_key_sealed,
 DROP COLUMN ble_protocol, DROP COLUMN ble_mode, DROP COLUMN ble_poll_seconds, DROP COLUMN last_read_at, DROP COLUMN rssi;
RESET ROLE;
