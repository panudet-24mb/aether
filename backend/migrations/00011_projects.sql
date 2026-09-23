-- +goose Up
SET ROLE aether_owner;
-- Projects group gateways (and therefore their devices) inside one workspace, e.g. one per site or customer job.
CREATE TABLE core.projects (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 name text NOT NULL, description text NOT NULL DEFAULT '',
 color text NOT NULL DEFAULT 'mint' CHECK(color IN ('mint','blue','amber','coral','violet','slate')),
 archived_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id)
);
CREATE UNIQUE INDEX projects_active_name ON core.projects(tenant_id,lower(name)) WHERE archived_at IS NULL;
ALTER TABLE core.projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.projects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.projects USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
GRANT SELECT,INSERT,UPDATE ON core.projects TO aether_app;
-- A gateway belongs to at most one project; NULL means "unassigned". The composite FK keeps it inside the tenant.
ALTER TABLE core.gateways ADD COLUMN project_id uuid;
ALTER TABLE core.gateways ADD CONSTRAINT gateways_tenant_id_project_id_fkey FOREIGN KEY(tenant_id,project_id) REFERENCES core.projects(tenant_id,id);
CREATE INDEX gateways_project ON core.gateways(tenant_id,project_id);
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
ALTER TABLE core.gateways DROP CONSTRAINT gateways_tenant_id_project_id_fkey;
DROP INDEX core.gateways_project;
ALTER TABLE core.gateways DROP COLUMN project_id;
DROP TABLE core.projects;
RESET ROLE;
