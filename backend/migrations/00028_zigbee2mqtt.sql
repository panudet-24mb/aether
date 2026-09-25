-- Zigbee2MQTT gateways (phase 1: ingest and display, read-only).
--
-- A Zigbee coordinator on site runs Zigbee2MQTT, which connects OUT to this broker with its own per-gateway
-- credentials and publishes under base_topic `aether/z2m/<gateway id>` (docs/platform/zigbee2mqtt.md). What
-- this adds:
--   * core.z2m_devices: the retained `bridge/devices` list per gateway, keyed by IEEE address (stable across
--     renames; friendly names may even contain "/"). `gangs` is derived from the device's `exposes`:
--     [{gang:1..4, property:"state_left", endpoint:"left", settable:bool}], empty for non-switches;
--   * core.z2m_bridges: the bridge's own online/offline state (`bridge/state`, retained, Z2M's last will);
--   * sensor_streams.liveness: 'silence' (the default — a stream is offline once it goes quiet, which is how
--     every BLE tag behaves) or 'reported' (offline/online come from Z2M availability messages, because a
--     mains wall switch only reports when its state changes and would otherwise look offline all day);
--   * stream_state.outputs: last known switch output per gang ({"sw1":1,"sw2":0}), edge-triggered like the
--     tamper/leak flags, so switch_on / switch_off are raised once per change and never on the first report;
--   * core.mqtt_provisioning_accounts_v2(): the v1 function plus the gateway model, so the provisioner can
--     render a per-model ACL. v1 stays until every provisioner runs the new code.
--
-- +goose Up
SET ROLE aether_owner;

CREATE TABLE core.z2m_devices (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 ieee text NOT NULL CHECK(ieee ~ '^0x[0-9a-f]{16}$'),
 friendly_name text NOT NULL, type text NOT NULL DEFAULT '', model text NOT NULL DEFAULT '', vendor text NOT NULL DEFAULT '',
 model_id text NOT NULL DEFAULT '', manufacturer text NOT NULL DEFAULT '', power_source text NOT NULL DEFAULT '',
 supported boolean NOT NULL DEFAULT false,
 gangs jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(gangs)='array'),
 available boolean, available_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now(), removed_at timestamptz,
 PRIMARY KEY(tenant_id,gateway_id,ieee),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);
CREATE INDEX z2m_devices_name ON core.z2m_devices(gateway_id,friendly_name) WHERE removed_at IS NULL;

CREATE TABLE core.z2m_bridges (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 state text NOT NULL DEFAULT '', state_at timestamptz, version text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,gateway_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);

ALTER TABLE core.sensor_streams ADD COLUMN liveness text NOT NULL DEFAULT 'silence' CHECK(liveness IN ('silence','reported'));
ALTER TABLE core.stream_state ADD COLUMN outputs jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(outputs)='object');

ALTER TABLE core.z2m_devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.z2m_devices FORCE ROW LEVEL SECURITY;
ALTER TABLE core.z2m_bridges ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.z2m_bridges FORCE ROW LEVEL SECURITY;
-- Permissive tenant isolation plus the RESTRICTIVE project scope keyed by gateway (00022_access_fixes.sql).
CREATE POLICY tenant_scope ON core.z2m_devices USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY tenant_scope ON core.z2m_bridges USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.z2m_devices AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
CREATE POLICY project_scope ON core.z2m_bridges AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
GRANT SELECT,INSERT,UPDATE ON core.z2m_devices,core.z2m_bridges TO aether_app;

CREATE FUNCTION core.mqtt_provisioning_accounts_v2() RETURNS TABLE(gateway_id uuid,password_hash text,revision integer,model text)
 LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT a.gateway_id,a.password_hash,a.revision,g.model FROM core.mqtt_accounts a JOIN core.gateways g ON g.id=a.gateway_id AND g.tenant_id=a.tenant_id WHERE g.revoked_at IS NULL ORDER BY a.gateway_id
 $$;
REVOKE ALL ON FUNCTION core.mqtt_provisioning_accounts_v2() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.mqtt_provisioning_accounts_v2() TO aether_mqtt_provisioner;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION core.mqtt_provisioning_accounts_v2();
ALTER TABLE core.stream_state DROP COLUMN outputs;
ALTER TABLE core.sensor_streams DROP COLUMN liveness;
DROP TABLE core.z2m_bridges;
DROP TABLE core.z2m_devices;
RESET ROLE;
