-- Tuya Cloud mode (phase G2, docs/platform/tuya-cloud.md): Tuya devices reached through the user's own Tuya cloud
-- project, with nothing installed on site. A "tuya-cloud" gateway stands for one cloud project; the tuya-cloud
-- worker consumes the project's Message Service (device status, online/offline) and sends commands through the
-- OpenAPI. What this adds:
--   * core.tuya_cloud_links: one row per cloud gateway: the region and channel, the project's Access ID and
--     Secret sealed together (bound to the tenant and gateway, never returned), a short hint and a digest of the
--     Access ID (globally unique: one project feeds one gateway), the link's state as the worker last saw it,
--     monthly usage counters, and the revision the worker reconnects on;
--   * core.tuya_cloud_link_ids(): the only thing the worker can read before it has a tenant: which links exist,
--     their revision and state (never the sealed credentials);
--   * core.tuya_devices: the parent hub and node of a sub-device, and the last report time of each data-point
--     code (`reported_t`), which orders and de-duplicates cloud reports; 'cloud_link_down' as a reason;
--   * device_commands: transport 'cloud' (wire is {"<code>":<raw>}), with the same id and wire rule as edge;
--   * core.command_targets: cloud rows, whose agent state is the link's and whose key status is always 'ok'
--     (no local key is involved);
--   * devices_tuya_one_mode: a physical Tuya device is registered either through an Aether Edge or through Tuya
--     Cloud, never both at once (two paths would confirm each other's commands and double every event).
--
-- +goose Up
SET ROLE aether_owner;

CREATE TABLE core.tuya_cloud_links (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 -- '' only before the first link, when a liveness helper creates the row: credentials always come with both.
 region text NOT NULL DEFAULT '' CHECK(region IN ('','us','us-e','eu','eu-w','in','cn','sg')),
 channel text NOT NULL DEFAULT 'event' CHECK(channel IN ('event','event-test')),
 access_id_hint text NOT NULL DEFAULT '' CHECK(length(access_id_hint)<=8),
 -- NULL once unlinked, so the same project can be linked again elsewhere.
 access_id_digest text UNIQUE CHECK(access_id_digest IS NULL OR access_id_digest ~ '^[0-9a-f]{64}$'),
 credentials_sealed text CHECK(credentials_sealed IS NULL OR length(credentials_sealed)<=1024),
 state text NOT NULL DEFAULT '' CHECK(state IN ('','linking','online','offline','auth_failed','not_subscribed','quota','disabled')),
 state_at timestamptz,
 reason text NOT NULL DEFAULT '' CHECK(length(reason)<=64),
 last_error_code bigint NOT NULL DEFAULT 0,
 last_event_at timestamptz, last_health_at timestamptz,
 usage_month date, events_month bigint NOT NULL DEFAULT 0, api_calls_month bigint NOT NULL DEFAULT 0, dropped_month bigint NOT NULL DEFAULT 0,
 revision bigint NOT NULL DEFAULT 0,
 sync_requested_at timestamptz,
 linked_by uuid, linked_at timestamptz, rotated_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,gateway_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id),
 CHECK(credentials_sealed IS NULL OR (region<>'' AND access_id_digest IS NOT NULL))
);

ALTER TABLE core.tuya_cloud_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.tuya_cloud_links FORCE ROW LEVEL SECURITY;
-- Permissive tenant isolation plus the RESTRICTIVE project scope keyed by gateway, as for the Edge tables.
CREATE POLICY tenant_scope ON core.tuya_cloud_links USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.tuya_cloud_links AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id)) WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
GRANT SELECT,INSERT,UPDATE,DELETE ON core.tuya_cloud_links TO aether_app;

-- The worker starts with no tenant in context: this function (running as the owner, whose read-only policy lets
-- it see every link) says which links should be connected. It never returns credentials; the worker opens those
-- inside a tenant transaction.
CREATE POLICY tuya_cloud_links_owner ON core.tuya_cloud_links FOR SELECT TO aether_owner USING(true);
CREATE FUNCTION core.tuya_cloud_link_ids() RETURNS TABLE(tenant_id uuid, gateway_id uuid, revision bigint, state text)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT l.tenant_id,l.gateway_id,l.revision,l.state FROM core.tuya_cloud_links l
 JOIN core.gateways g ON g.tenant_id=l.tenant_id AND g.id=l.gateway_id AND g.revoked_at IS NULL AND g.model='tuya-cloud'
 JOIN core.tenants t ON t.id=l.tenant_id AND t.status='active'
 WHERE l.credentials_sealed IS NOT NULL
 ORDER BY l.tenant_id,l.gateway_id
$$;
REVOKE ALL ON FUNCTION core.tuya_cloud_link_ids() FROM PUBLIC;
-- Callable by aether_app, as core.active_tenant_ids() is: every aether_app process (the API included) can list
-- which links exist, with their revision and state, across tenants. Nothing secret is returned.
GRANT EXECUTE ON FUNCTION core.tuya_cloud_link_ids() TO aether_app;

-- The per-workspace cap on linked projects must count every link of the tenant, not only those inside the
-- caller's project scope: this function counts the calling tenant's links (other than the given gateway).
CREATE FUNCTION core.tuya_cloud_link_count(excluded uuid) RETURNS bigint
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT count(*) FROM core.tuya_cloud_links l
 WHERE l.tenant_id=core.tenant_id() AND l.credentials_sealed IS NOT NULL AND l.gateway_id<>excluded
$$;
REVOKE ALL ON FUNCTION core.tuya_cloud_link_count(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.tuya_cloud_link_count(uuid) TO aether_app;

-- A sub-device behind a Tuya hub names its parent and node; the report time of each code orders cloud reports
-- (a redelivered or late message never overwrites a newer value).
ALTER TABLE core.tuya_devices ADD COLUMN parent_tuya_id text NOT NULL DEFAULT '' CHECK(parent_tuya_id ~ '^([a-z0-9]{16,32})?$');
ALTER TABLE core.tuya_devices ADD COLUMN node_id text NOT NULL DEFAULT '' CHECK(length(node_id)<=64);
ALTER TABLE core.tuya_devices ADD COLUMN reported_t jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(reported_t)='object' AND octet_length(reported_t::text)<=32768);
ALTER TABLE core.tuya_devices DROP CONSTRAINT tuya_devices_reason_check;
ALTER TABLE core.tuya_devices ADD CONSTRAINT tuya_devices_reason_check CHECK(reason IN ('','unreachable','auth_failed','key_suspect','busy','not_found','cloud_link_down'));

-- Cloud commands carry their wire form like edge ones.
ALTER TABLE core.device_commands DROP CONSTRAINT device_commands_transport_check;
ALTER TABLE core.device_commands ADD CONSTRAINT device_commands_transport_check CHECK(transport IN ('z2m','edge','cloud'));
ALTER TABLE core.device_commands DROP CONSTRAINT device_commands_ieee_check;
ALTER TABLE core.device_commands ADD CONSTRAINT device_commands_ieee_check CHECK(
 (transport='z2m' AND ieee ~ '^0x[0-9a-f]{16}$') OR (transport IN ('edge','cloud') AND ieee ~ '^[a-z0-9]{16,32}$' AND wire IS NOT NULL));

CREATE OR REPLACE VIEW core.command_targets WITH (security_invoker=true) AS
 SELECT z.tenant_id,z.gateway_id,z.ieee AS external_id,'z2m'::text AS transport,z.exposes,z.state,z.available,
   coalesce(b.state,'') AS agent_state,z.category,'{}'::jsonb AS dp_map,'ok'::text AS key_status
 FROM core.z2m_devices z LEFT JOIN core.z2m_bridges b ON b.tenant_id=z.tenant_id AND b.gateway_id=z.gateway_id
 WHERE z.removed_at IS NULL
 UNION ALL
 SELECT t.tenant_id,t.gateway_id,t.tuya_id,CASE WHEN g.model='tuya-cloud' THEN 'cloud' ELSE 'edge' END,t.exposes,t.state,t.available,
   CASE WHEN g.model='tuya-cloud' THEN coalesce(l.state,'') ELSE coalesce(a.state,'') END,t.category,t.dp_map,
   CASE WHEN g.model='tuya-cloud' THEN 'ok' ELSE t.key_status END
 FROM core.tuya_devices t JOIN core.gateways g ON g.tenant_id=t.tenant_id AND g.id=t.gateway_id
 LEFT JOIN core.edge_agents a ON a.tenant_id=t.tenant_id AND a.gateway_id=t.gateway_id
 LEFT JOIN core.tuya_cloud_links l ON l.tenant_id=t.tenant_id AND l.gateway_id=t.gateway_id
 WHERE t.removed_at IS NULL;

-- One mode per physical Tuya device within a workspace. If this fails, a device is registered both ways already:
-- remove one of the two registrations, then migrate again.
CREATE UNIQUE INDEX devices_tuya_one_mode ON core.devices(tenant_id,lower(external_id))
 WHERE removed_at IS NULL AND profile_id IN ('tuya-wifi-device@1','tuya-cloud-device@1');
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
-- Going back would strand every live Tuya Cloud gateway (no link table, no cloud commands): refuse while one
-- exists. Revoke them first (the owner's lookup policy on core.gateways lets this see every tenant's gateways).
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS(SELECT 1 FROM core.gateways WHERE model='tuya-cloud' AND revoked_at IS NULL) THEN
  RAISE EXCEPTION 'live tuya-cloud gateways exist: revoke them before migrating below 00034';
 END IF;
END $$;
-- +goose StatementEnd
DROP INDEX core.devices_tuya_one_mode;
CREATE OR REPLACE VIEW core.command_targets WITH (security_invoker=true) AS
 SELECT z.tenant_id,z.gateway_id,z.ieee AS external_id,'z2m'::text AS transport,z.exposes,z.state,z.available,
   coalesce(b.state,'') AS agent_state,z.category,'{}'::jsonb AS dp_map,'ok'::text AS key_status
 FROM core.z2m_devices z LEFT JOIN core.z2m_bridges b ON b.tenant_id=z.tenant_id AND b.gateway_id=z.gateway_id
 WHERE z.removed_at IS NULL
 UNION ALL
 SELECT t.tenant_id,t.gateway_id,t.tuya_id,'edge',t.exposes,t.state,t.available,
   coalesce(a.state,''),t.category,t.dp_map,t.key_status
 FROM core.tuya_devices t LEFT JOIN core.edge_agents a ON a.tenant_id=t.tenant_id AND a.gateway_id=t.gateway_id
 WHERE t.removed_at IS NULL;
-- Cloud commands and the cloud reason cannot satisfy the old checks. FORCE row level security would hide the rows
-- from the owner too.
ALTER TABLE core.device_commands NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.device_commands WHERE transport='cloud';
ALTER TABLE core.device_commands FORCE ROW LEVEL SECURITY;
ALTER TABLE core.device_commands DROP CONSTRAINT device_commands_ieee_check;
ALTER TABLE core.device_commands ADD CONSTRAINT device_commands_ieee_check CHECK(
 (transport='z2m' AND ieee ~ '^0x[0-9a-f]{16}$') OR (transport='edge' AND ieee ~ '^[a-z0-9]{16,32}$' AND wire IS NOT NULL));
ALTER TABLE core.device_commands DROP CONSTRAINT device_commands_transport_check;
ALTER TABLE core.device_commands ADD CONSTRAINT device_commands_transport_check CHECK(transport IN ('z2m','edge'));
ALTER TABLE core.tuya_devices NO FORCE ROW LEVEL SECURITY;
UPDATE core.tuya_devices SET reason='' WHERE reason='cloud_link_down';
ALTER TABLE core.tuya_devices FORCE ROW LEVEL SECURITY;
ALTER TABLE core.tuya_devices DROP CONSTRAINT tuya_devices_reason_check;
ALTER TABLE core.tuya_devices ADD CONSTRAINT tuya_devices_reason_check CHECK(reason IN ('','unreachable','auth_failed','key_suspect','busy','not_found'));
ALTER TABLE core.tuya_devices DROP COLUMN reported_t, DROP COLUMN node_id, DROP COLUMN parent_tuya_id;
DROP FUNCTION core.tuya_cloud_link_count(uuid);
DROP FUNCTION core.tuya_cloud_link_ids();
DROP TABLE core.tuya_cloud_links;
RESET ROLE;
