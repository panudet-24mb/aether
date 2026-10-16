-- Saved summary reports (docs/platform/reports.md): which fixed sensors a report covers and the allowed temperature and
-- humidity range its excursions are measured against. The data itself is never stored here; a run reads
-- core.sample_rollup for the range it is asked for.
--
--   core.report_definitions  one row per saved report. project_id NULL is a workspace report (like alert rules,
--                            00042): a member restricted to some projects reads workspace reports and their own
--                            projects' reports, and writes only their own projects' reports. `sensors` is a JSON
--                            array of {gateway_id, external_id}; the API checks every entry is a fixed sensor the
--                            writer can see (wearables and roaming tags are personal data and never part of a report).
--                            Writes are owner / admin / operator in the API, reads everyone with the reports module.
--
-- Under FORCE RLS like every tenant table; the per-workspace cap counts through a definer so a project-limited
-- writer cannot create past it.

-- +goose Up
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';

CREATE TABLE core.report_definitions (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 id uuid NOT NULL,
 project_id uuid,
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 120),
 kind text NOT NULL DEFAULT 'environment' CHECK(kind IN ('environment')),
 sensors jsonb NOT NULL CHECK(jsonb_typeof(sensors) = 'array' AND jsonb_array_length(sensors) BETWEEN 1 AND 50),
 temp_min numeric, temp_max numeric, hum_min numeric, hum_max numeric,
 created_by uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id, id),
 CONSTRAINT report_definitions_tenant_id_project_id_fkey FOREIGN KEY(tenant_id, project_id) REFERENCES core.projects(tenant_id, id) ON DELETE CASCADE,
 CHECK(temp_min IS NULL OR temp_max IS NULL OR temp_min <= temp_max),
 CHECK(hum_min IS NULL OR hum_max IS NULL OR hum_min <= hum_max)
);
CREATE INDEX report_definitions_project ON core.report_definitions(tenant_id, project_id);
ALTER TABLE core.report_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.report_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.report_definitions USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope_read ON core.report_definitions AS RESTRICTIVE FOR SELECT TO aether_app
 USING((SELECT core.scope_all()) OR project_id IS NULL OR core.project_in_scope(project_id));
CREATE POLICY project_scope_insert ON core.report_definitions AS RESTRICTIVE FOR INSERT TO aether_app
 WITH CHECK((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)));
CREATE POLICY project_scope_update ON core.report_definitions AS RESTRICTIVE FOR UPDATE TO aether_app
 USING((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))
 WITH CHECK((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)));
CREATE POLICY project_scope_delete ON core.report_definitions AS RESTRICTIVE FOR DELETE TO aether_app
 USING((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)));
GRANT SELECT, INSERT, UPDATE, DELETE ON core.report_definitions TO aether_app;

-- The per-workspace cap counts every report of the workspace, not the ones a project-limited writer can see.
-- core.report_definitions is FORCE RLS: the definer (aether_owner) reads it through tenant_scope, which is not
-- TO aether_app, so the count stays inside core.tenant_id().
-- +goose StatementBegin
CREATE FUNCTION core.report_definition_count() RETURNS bigint
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT count(*) FROM core.report_definitions d WHERE d.tenant_id = core.tenant_id()
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.report_definition_count() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.report_definition_count() TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';
DROP FUNCTION core.report_definition_count();
DROP TABLE core.report_definitions;
RESET ROLE;
