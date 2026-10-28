-- Frames on the connect canvas: a named, coloured rectangle an owner or admin draws to group the nodes inside it
-- (for example "ห้องน้ำชั้น M"). A frame only arranges the picture: which nodes belong to it is where they sit on
-- the board, so nothing is stored per node and no device, alert, dashboard or floor plan reads it.
--
-- A frame belongs to the project it was drawn under, or to none when drawn under "all" or "unassigned". The
-- project scope policy is the one gateways use (00022_access_fixes.sql): a member restricted to some projects sees
-- and changes only the frames of those projects, and never a frame without a project. There is no foreign key to
-- core.projects: an archived project's frames simply stop matching any project filter, as its gateways do.

-- +goose Up
SET ROLE aether_owner;
CREATE TABLE core.topology_frames(
 tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 id uuid NOT NULL,
 view text NOT NULL CHECK(view ~ '^[a-z][a-z0-9_-]{0,31}$'),
 project_id uuid,
 name text NOT NULL CHECK(octet_length(name) BETWEEN 1 AND 128),
 color text NOT NULL CHECK(color IN ('mint','blue','amber','coral','violet','slate')),
 x double precision NOT NULL CHECK(x BETWEEN -1000000 AND 1000000),
 y double precision NOT NULL CHECK(y BETWEEN -1000000 AND 1000000),
 w double precision NOT NULL CHECK(w BETWEEN 40 AND 20000),
 h double precision NOT NULL CHECK(h BETWEEN 40 AND 20000),
 created_by uuid,
 updated_by uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id));
CREATE INDEX topology_frames_view ON core.topology_frames(tenant_id,view);

ALTER TABLE core.topology_frames ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.topology_frames FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.topology_frames USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.topology_frames AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.project_in_scope(project_id))
 WITH CHECK((SELECT core.scope_all()) OR core.project_in_scope(project_id));
GRANT SELECT,INSERT,UPDATE,DELETE ON core.topology_frames TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.topology_frames;
RESET ROLE;
