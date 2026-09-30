-- +goose Up
-- The expression indexes below take a short SHARE lock on tables ingest writes to; never queue behind a long
-- transaction for more than a few seconds (the migration fails and can simply be run again).
SET LOCAL lock_timeout = '5s';
-- Shared canvas layout for the connect view. Until now node positions lived in each browser's localStorage,
-- so two people in one workspace never saw the same picture.
--
-- One header row per (workspace, view) carries the version used for optimistic concurrency. Positions are
-- one row per canvas node, and each row names the gateway that node belongs to. That column is what the
-- project scope policy reads: a member restricted to some projects sees and moves only the nodes of
-- gateways in those projects, exactly as the canvas itself is filtered. Positions are shared by every
-- project filter of the view (switching the filter only hides nodes, as it did in the browser).
--
-- There is deliberately no foreign key to core.gateways: its check would take KEY SHARE on the gateway row,
-- which conflicts with the FOR UPDATE every ingest path takes on that row. A stale gateway_id (a device that
-- moved gateway, a revoked gateway) only narrows who sees the row and is refreshed on the next save.
CREATE TABLE core.topology_layouts(
 tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 view text NOT NULL CHECK(view ~ '^[a-z][a-z0-9_-]{0,31}$'),
 version bigint NOT NULL DEFAULT 0 CHECK(version >= 0),
 updated_by uuid,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id, view));

CREATE TABLE core.topology_positions(
 tenant_id uuid NOT NULL,
 view text NOT NULL,
 node_id text NOT NULL CHECK(node_id ~ '^(broker|gw:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|dev:[a-z0-9][a-z0-9._:-]{0,63})$'),
 gateway_id uuid,
 x double precision NOT NULL CHECK(x BETWEEN -1000000 AND 1000000),
 y double precision NOT NULL CHECK(y BETWEEN -1000000 AND 1000000),
 updated_by uuid,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id, view, node_id),
 FOREIGN KEY(tenant_id, view) REFERENCES core.topology_layouts(tenant_id, view) ON DELETE CASCADE,
 -- The broker node is the only one without a gateway.
 CHECK((node_id = 'broker') = (gateway_id IS NULL)));

ALTER TABLE core.topology_layouts ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.topology_layouts FORCE ROW LEVEL SECURITY;
ALTER TABLE core.topology_positions ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.topology_positions FORCE ROW LEVEL SECURITY;

-- The header holds only a version counter, so it is scoped to the workspace alone.
CREATE POLICY tenant_scope ON core.topology_layouts
 USING(tenant_id = core.tenant_id()) WITH CHECK(tenant_id = core.tenant_id());
CREATE POLICY tenant_scope ON core.topology_positions
 USING(tenant_id = core.tenant_id()) WITH CHECK(tenant_id = core.tenant_id());
-- Everyone in the workspace sees where the broker is drawn; only a member who sees every project may move or
-- forget it. Restrictive policies are per command, so the broker exception applies to reads alone.
CREATE POLICY project_scope_read ON core.topology_positions AS RESTRICTIVE FOR SELECT TO aether_app
 USING((SELECT core.scope_all()) OR node_id = 'broker' OR core.gateway_in_scope(gateway_id));
CREATE POLICY project_scope_insert ON core.topology_positions AS RESTRICTIVE FOR INSERT TO aether_app
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
CREATE POLICY project_scope_update ON core.topology_positions AS RESTRICTIVE FOR UPDATE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
CREATE POLICY project_scope_delete ON core.topology_positions AS RESTRICTIVE FOR DELETE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));

GRANT SELECT, INSERT, UPDATE ON core.topology_layouts TO aether_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON core.topology_positions TO aether_app;

-- Every stored row counts toward one cap per view, including rows a restricted member cannot see, so the read
-- (capped at the same number) never truncates a live node. Owned by aether_owner; returns only a count.
CREATE FUNCTION core.topology_layout_rows(layout_view text) RETURNS bigint
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT count(*) FROM core.topology_positions WHERE tenant_id = core.tenant_id() AND view = layout_view
$$;
REVOKE ALL ON FUNCTION core.topology_layout_rows(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.topology_layout_rows(text) TO aether_app;

-- Resolving a canvas node to its gateway looks identities up case-insensitively: registrations and streams keep
-- the external id as it was entered or heard, while node ids are lower case. Tuya ids and LAN sightings are
-- lower-cased by the Edge, but the cloud import is not guaranteed to be, so they are indexed the same way.
-- (Zigbee IEEE addresses are always stored lower case and are compared directly.)
CREATE INDEX topology_devices_identity ON core.devices(tenant_id, lower(external_id)) WHERE removed_at IS NULL;
CREATE INDEX topology_streams_identity ON core.sensor_streams(tenant_id, lower(external_id));
CREATE INDEX topology_tuya_identity ON core.tuya_devices(tenant_id, lower(tuya_id)) WHERE removed_at IS NULL;
CREATE INDEX topology_edge_lan_identity ON core.edge_lan_devices(tenant_id, lower(device_id));

-- +goose Down
DROP INDEX core.topology_edge_lan_identity;
DROP INDEX core.topology_tuya_identity;
DROP INDEX core.topology_streams_identity;
DROP INDEX core.topology_devices_identity;
DROP FUNCTION core.topology_layout_rows(text);
DROP TABLE core.topology_positions;
DROP TABLE core.topology_layouts;
