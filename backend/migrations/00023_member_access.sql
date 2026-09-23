-- +goose Up
SET ROLE aether_owner;
CREATE TABLE core.member_access (
 tenant_id uuid NOT NULL, user_id uuid NOT NULL, permissions jsonb NOT NULL DEFAULT '{}',
 PRIMARY KEY(tenant_id,user_id),
 FOREIGN KEY(tenant_id,user_id) REFERENCES core.memberships(tenant_id,user_id) ON DELETE CASCADE,
 CHECK(jsonb_typeof(permissions)='object')
);
ALTER TABLE core.member_access ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.member_access FORCE ROW LEVEL SECURITY;
CREATE POLICY member_access_read ON core.member_access FOR SELECT USING (tenant_id=core.tenant_id() AND (user_id=identity.user_id() OR core.is_tenant_admin()));
CREATE POLICY member_access_write ON core.member_access FOR ALL USING (tenant_id=core.tenant_id() AND core.is_tenant_admin()) WITH CHECK (tenant_id=core.tenant_id() AND core.is_tenant_admin());
GRANT SELECT,INSERT,UPDATE,DELETE ON core.member_access TO aether_app;
-- Administrators can be limited to selected customer projects too. Owners retain recovery access.
CREATE OR REPLACE FUNCTION core.compute_project_scope() RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT CASE WHEN identity.user_id() IS NULL OR core.tenant_id() IS NULL THEN '*'
 WHEN EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id=core.tenant_id() AND m.user_id=identity.user_id() AND m.role='owner') THEN '*'
 WHEN NOT EXISTS(SELECT 1 FROM core.member_projects p WHERE p.tenant_id=core.tenant_id() AND p.user_id=identity.user_id()) THEN '*'
 ELSE (SELECT string_agg(p.project_id::text,',' ORDER BY p.project_id) FROM core.member_projects p WHERE p.tenant_id=core.tenant_id() AND p.user_id=identity.user_id()) END
$$;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.member_access;
CREATE OR REPLACE FUNCTION core.compute_project_scope() RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT CASE WHEN identity.user_id() IS NULL OR core.tenant_id() IS NULL THEN '*'
 WHEN EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id=core.tenant_id() AND m.user_id=identity.user_id() AND m.role IN ('owner','admin')) THEN '*'
 WHEN NOT EXISTS(SELECT 1 FROM core.member_projects p WHERE p.tenant_id=core.tenant_id() AND p.user_id=identity.user_id()) THEN '*'
 ELSE (SELECT string_agg(p.project_id::text,',' ORDER BY p.project_id) FROM core.member_projects p WHERE p.tenant_id=core.tenant_id() AND p.user_id=identity.user_id()) END
$$;
RESET ROLE;
