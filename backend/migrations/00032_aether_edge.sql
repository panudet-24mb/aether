-- Aether Edge: Tuya Wi-Fi devices reached locally (phases B and C, docs/platform/aether-edge.md).
--
-- An Aether Edge agent runs on a site host next to the Tuya Wi-Fi devices. It talks to each device over TCP 6668
-- with the device's local key (no Tuya cloud at runtime), connects OUT to this broker with its gateway's own
-- account and publishes under aether/edge/<gateway id>: its status (last will), health, the devices it sees
-- broadcasting on the LAN, and each device's raw data points. What this adds:
--   * core.edge_agents: the agent's own state (online/offline, version, last heartbeat) and config_revision,
--     bumped whenever what the agent must connect to changes (a registration, a key import) so it re-fetches its
--     configuration;
--   * core.edge_lan_devices: Tuya devices the agent saw on its LAN (id, IP, protocol version), keyed by the
--     Tuya device id — what the import is matched against;
--   * core.tuya_devices: devices imported (once) from the Tuya cloud with their local key, sealed like
--     notification-channel secrets and never returned by the API (only a fingerprint), their data-point
--     specification (`spec`, normalised) and its translation into the exposes shape Zigbee2MQTT devices use
--     (`exposes`, `dp_map` from each property back to its data point, `gangs`), the category, whether a device of
--     that kind can be reached locally at all (battery sensors sleep), the key's status (ok / rejected / suspect /
--     missing, set from the agent's availability reasons), the last reported value of each settable property
--     (`state`, for toggles) and the device's availability;
--   * device_commands: `transport` ('z2m' | 'edge') and `wire`, the command in the device's own terms
--     ({"dps":{"1":true}}), computed when the command is queued so mqtt-commander never needs a specification or
--     a key. The id check keeps the IEEE form for Zigbee2MQTT and allows a Tuya device id only for edge rows;
--   * core.command_targets: every device a command can target, whatever its transport (security_invoker, so
--     each caller's own row level security applies);
--   * core.edge_install_codes + core.redeem_edge_install_code(): single-use install codes (only the hash is
--     stored) that the one-line installer redeems for the agent's credentials (the HTTP route is phase D).
--
-- +goose Up
SET ROLE aether_owner;

CREATE TABLE core.edge_agents (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 state text NOT NULL DEFAULT '' CHECK(state IN ('','online','offline')), state_at timestamptz,
 version text NOT NULL DEFAULT '' CHECK(length(version)<=32),
 last_health_at timestamptz, devices_connected integer NOT NULL DEFAULT 0, lan_seen integer NOT NULL DEFAULT 0,
 config_revision bigint NOT NULL DEFAULT 0, config_fetched_at timestamptz,
 -- The last LAN list accepted: lists arriving faster than every 30 s are acknowledged and dropped.
 last_discovery_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,gateway_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);

CREATE TABLE core.edge_lan_devices (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 device_id text NOT NULL CHECK(device_id ~ '^[a-z0-9]{16,32}$'),
 ip text NOT NULL CHECK(length(ip)<=45), version text NOT NULL CHECK(version IN ('3.1','3.2','3.3','3.4','3.5')),
 product_key text NOT NULL DEFAULT '' CHECK(length(product_key)<=64),
 last_seen timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,gateway_id,device_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);

CREATE TABLE core.tuya_devices (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 tuya_id text NOT NULL CHECK(tuya_id ~ '^[a-z0-9]{16,32}$'),
 name text NOT NULL DEFAULT '' CHECK(length(name)<=128),
 tuya_category text NOT NULL DEFAULT '' CHECK(length(tuya_category)<=32),
 product_id text NOT NULL DEFAULT '' CHECK(length(product_id)<=64),
 sub boolean NOT NULL DEFAULT false,
 spec jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(spec)='array' AND octet_length(spec::text)<=65536),
 exposes jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(exposes)='array' AND octet_length(exposes::text)<=65536),
 dp_map jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(dp_map)='object' AND octet_length(dp_map::text)<=65536),
 gangs jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(gangs)='array'),
 category text NOT NULL DEFAULT '' CHECK(length(category)<=32),
 local_capable boolean NOT NULL DEFAULT true,
 local_key_sealed text CHECK(local_key_sealed IS NULL OR length(local_key_sealed)<=512),
 key_fingerprint text NOT NULL DEFAULT '' CHECK(key_fingerprint ~ '^[0-9a-f]{0,16}$'),
 key_status text NOT NULL DEFAULT 'missing' CHECK(key_status IN ('ok','rejected','suspect','missing')),
 version text NOT NULL DEFAULT '' CHECK(version IN ('','3.1','3.2','3.3','3.4','3.5')),
 ip text NOT NULL DEFAULT '' CHECK(length(ip)<=45),
 device22 boolean NOT NULL DEFAULT false,
 state jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(state)='object'),
 available boolean, available_at timestamptz,
 reason text NOT NULL DEFAULT '' CHECK(reason IN ('','unreachable','auth_failed','key_suspect','busy','not_found')),
 imported_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), removed_at timestamptz,
 PRIMARY KEY(tenant_id,gateway_id,tuya_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);

CREATE TABLE core.edge_install_codes (
 tenant_id uuid NOT NULL, id uuid NOT NULL, gateway_id uuid NOT NULL,
 code_hash text NOT NULL UNIQUE CHECK(code_hash ~ '^[0-9a-f]{64}$'),
 created_by uuid, created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL, redeemed_at timestamptz,
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);

-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
 FOREACH t IN ARRAY ARRAY['edge_agents','edge_lan_devices','tuya_devices','edge_install_codes'] LOOP
  EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY', t);
  EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY', t);
  -- Permissive tenant isolation plus the RESTRICTIVE project scope keyed by gateway (00022_access_fixes.sql).
  EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())', t);
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id)) WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))', t);
 END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT,INSERT,UPDATE ON core.edge_agents,core.edge_lan_devices,core.tuya_devices,core.edge_install_codes TO aether_app;
GRANT DELETE ON core.edge_lan_devices,core.edge_install_codes TO aether_app;

-- Commands to an Edge carry their wire form. A Tuya device id is accepted only on an edge row: the IEEE check on
-- Zigbee2MQTT rows is exactly what it was.
ALTER TABLE core.device_commands ADD COLUMN transport text NOT NULL DEFAULT 'z2m' CHECK(transport IN ('z2m','edge'));
ALTER TABLE core.device_commands ADD COLUMN wire jsonb CHECK(wire IS NULL OR (jsonb_typeof(wire)='object' AND octet_length(wire::text)<=1024));
ALTER TABLE core.device_commands DROP CONSTRAINT device_commands_ieee_check;
ALTER TABLE core.device_commands ADD CONSTRAINT device_commands_ieee_check CHECK(
 (transport='z2m' AND ieee ~ '^0x[0-9a-f]{16}$') OR (transport='edge' AND ieee ~ '^[a-z0-9]{16,32}$' AND wire IS NOT NULL));

-- Every device a command can target, whatever carries it. security_invoker: the caller's own row level security
-- (tenant and project scope) applies to both halves.
CREATE VIEW core.command_targets WITH (security_invoker=true) AS
 SELECT z.tenant_id,z.gateway_id,z.ieee AS external_id,'z2m'::text AS transport,z.exposes,z.state,z.available,
   coalesce(b.state,'') AS agent_state,z.category,'{}'::jsonb AS dp_map,'ok'::text AS key_status
 FROM core.z2m_devices z LEFT JOIN core.z2m_bridges b ON b.tenant_id=z.tenant_id AND b.gateway_id=z.gateway_id
 WHERE z.removed_at IS NULL
 UNION ALL
 SELECT t.tenant_id,t.gateway_id,t.tuya_id,'edge',t.exposes,t.state,t.available,
   coalesce(a.state,''),t.category,t.dp_map,t.key_status
 FROM core.tuya_devices t LEFT JOIN core.edge_agents a ON a.tenant_id=t.tenant_id AND a.gateway_id=t.gateway_id
 WHERE t.removed_at IS NULL;
GRANT SELECT ON core.command_targets TO aether_app;

-- Redeeming an install code happens before the installer has any credential, so there is no tenant in context:
-- the function runs as the owner (whose own policy below lets it see the codes) and returns the code's tenant and
-- gateway once. The code must be unexpired, unused, and for a live Aether Edge gateway.
CREATE POLICY edge_install_codes_owner ON core.edge_install_codes FOR ALL TO aether_owner USING(true) WITH CHECK(true);
CREATE FUNCTION core.redeem_edge_install_code(hash text) RETURNS TABLE(tenant_id uuid, gateway_id uuid)
 LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
 UPDATE core.edge_install_codes c SET redeemed_at=now()
 FROM core.gateways g
 WHERE c.code_hash=hash AND c.redeemed_at IS NULL AND c.expires_at>now()
   AND g.tenant_id=c.tenant_id AND g.id=c.gateway_id AND g.revoked_at IS NULL AND g.model='aether-edge'
 RETURNING c.tenant_id,c.gateway_id
$$;
REVOKE ALL ON FUNCTION core.redeem_edge_install_code(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.redeem_edge_install_code(text) TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION core.redeem_edge_install_code(text);
DROP POLICY edge_install_codes_owner ON core.edge_install_codes;
DROP VIEW core.command_targets;
-- Edge commands cannot satisfy the old check. FORCE row level security would hide them from the owner too.
ALTER TABLE core.device_commands NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.device_commands WHERE transport='edge';
ALTER TABLE core.device_commands FORCE ROW LEVEL SECURITY;
ALTER TABLE core.device_commands DROP CONSTRAINT device_commands_ieee_check;
ALTER TABLE core.device_commands ADD CONSTRAINT device_commands_ieee_check CHECK(ieee ~ '^0x[0-9a-f]{16}$');
ALTER TABLE core.device_commands DROP COLUMN wire;
ALTER TABLE core.device_commands DROP COLUMN transport;
DROP TABLE core.edge_install_codes;
DROP TABLE core.tuya_devices;
DROP TABLE core.edge_lan_devices;
DROP TABLE core.edge_agents;
RESET ROLE;
