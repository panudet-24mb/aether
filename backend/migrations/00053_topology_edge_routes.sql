-- Routes of connections on the connect canvas: the bend points someone dragged a line through, so a busy board can
-- be untangled without moving the devices. Only the stable connections are routed: a gateway's link to the broker
-- (kind 'link', ref = gateway id) and a registration's line to its gateway (kind 'adopt', ref = registration id).
-- The transient "heard" lines change with every sighting and are never stored.
--
-- Each row names the gateway its connection belongs to, and the project scope policy reads it exactly as for node
-- positions (00041): a member restricted to some projects sees and moves only their projects' lines. A route whose
-- registration has since moved to another gateway, or whose gateway or registration is gone, is simply not read
-- back; the next save of that line replaces it.

-- +goose Up
SET ROLE aether_owner;
CREATE TABLE core.topology_edge_routes(
 tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 view text NOT NULL CHECK(view ~ '^[a-z][a-z0-9_-]{0,31}$'),
 kind text NOT NULL CHECK(kind IN ('link','adopt')),
 ref uuid NOT NULL,
 gateway_id uuid NOT NULL,
 points jsonb NOT NULL CHECK(jsonb_typeof(points) = 'array' AND jsonb_array_length(points) BETWEEN 1 AND 16),
 updated_by uuid,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,view,kind,ref));

ALTER TABLE core.topology_edge_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.topology_edge_routes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.topology_edge_routes USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.topology_edge_routes AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
GRANT SELECT,INSERT,UPDATE,DELETE ON core.topology_edge_routes TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.topology_edge_routes;
RESET ROLE;
