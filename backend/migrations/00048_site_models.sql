-- A 3D model of a building (docs/platform/digital-twin.md, "Building model (3D)"): one binary glTF per site.
--
--   core.site_models         one row per site: the GLB bytes, their sha256, and how the twin places them (transform).
--                            Building geometry, not personal data. Stored uncompressed (STORAGE EXTERNAL) so the API
--                            reads it in slices; at most 32 MiB, because every model is written to the WAL and the
--                            PITR archive.
--   core.sites.model_revision a counter that only grows, also across a delete, so ?rev=N never names two files.
--   core.site_model_uploads  an upload in progress: the declared size, sha256 and chunk count; usable for one hour,
--                            then deleted by core.sweep_site_model_uploads (the API's hourly maintenance loop) or by
--                            the next upload start of the workspace.
--   core.site_model_chunks   its chunks (at most 160 KiB each, so every request body stays under the API limit).
--
-- Uploads are UNLOGGED: transient bytes stay out of the WAL and the PITR archive (only the committed model is
-- written there once). An unfinished upload does not survive a crash or a restore: the tables come back empty and the
-- browser starts the upload again.
-- Everything is tenant scoped and project scoped through the site, like the floors, under FORCE RLS.

-- +goose Up
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';

ALTER TABLE core.sites ADD COLUMN model_revision integer NOT NULL DEFAULT 0 CHECK(model_revision >= 0);

CREATE TABLE core.site_models (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL,
 revision integer NOT NULL CHECK(revision >= 1),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 120),
 source_format text NOT NULL CHECK(source_format IN ('glb','gltf','obj','fbx','stl','3dm','ifc')),
 size integer NOT NULL CHECK(size BETWEEN 20 AND 33554432),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 transform jsonb NOT NULL CHECK(jsonb_typeof(transform) = 'object' AND pg_column_size(transform) <= 2048),
 data bytea NOT NULL CHECK(octet_length(data) = size),
 uploaded_by uuid, uploaded_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,site_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES core.sites(tenant_id,id) ON DELETE CASCADE
);
ALTER TABLE core.site_models ALTER COLUMN data SET STORAGE EXTERNAL;

CREATE UNLOGGED TABLE core.site_model_uploads (
 tenant_id uuid NOT NULL, id uuid NOT NULL, site_id uuid NOT NULL,
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 120),
 source_format text NOT NULL CHECK(source_format IN ('glb','gltf','obj','fbx','stl','3dm','ifc')),
 size integer NOT NULL CHECK(size BETWEEN 20 AND 33554432),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 chunks integer NOT NULL CHECK(chunks BETWEEN 1 AND 205),
 created_by uuid, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id),
 UNIQUE(tenant_id,id,site_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES core.sites(tenant_id,id) ON DELETE CASCADE
);
CREATE INDEX site_model_uploads_site ON core.site_model_uploads(tenant_id,site_id);
CREATE INDEX site_model_uploads_created ON core.site_model_uploads(tenant_id,created_at);
-- site_id is repeated so the chunk carries its own project scope; the composite key keeps it equal to the upload's.
CREATE UNLOGGED TABLE core.site_model_chunks (
 tenant_id uuid NOT NULL, upload_id uuid NOT NULL, site_id uuid NOT NULL,
 idx integer NOT NULL CHECK(idx BETWEEN 0 AND 204),
 data bytea NOT NULL CHECK(octet_length(data) BETWEEN 1 AND 163840),
 PRIMARY KEY(tenant_id,upload_id,idx),
 FOREIGN KEY(tenant_id,upload_id,site_id) REFERENCES core.site_model_uploads(tenant_id,id,site_id) ON DELETE CASCADE
);
ALTER TABLE core.site_model_chunks ALTER COLUMN data SET STORAGE EXTERNAL;

-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['site_models','site_model_uploads','site_model_chunks'] LOOP
  EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())',t);
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING((SELECT core.scope_all()) OR core.site_in_scope(site_id)) WITH CHECK((SELECT core.scope_all()) OR core.site_in_scope(site_id))',t);
 END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT,INSERT,UPDATE,DELETE ON core.site_models,core.site_model_uploads,core.site_model_chunks TO aether_app;

-- Expired uploads of every workspace, for the maintenance loop (which has no tenant context). Owner-only rows:
-- these policies serve this definer and nobody else (the runtime's policies above are unchanged).
CREATE POLICY owner_access ON core.site_model_uploads TO aether_owner USING(true) WITH CHECK(true);
CREATE POLICY owner_access ON core.site_model_chunks TO aether_owner USING(true) WITH CHECK(true);
-- +goose StatementBegin
CREATE FUNCTION core.sweep_site_model_uploads() RETURNS bigint
 LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 WITH gone AS (DELETE FROM core.site_model_uploads WHERE created_at < now() - interval '1 hour' RETURNING 1)
 SELECT count(*) FROM gone
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.sweep_site_model_uploads() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.sweep_site_model_uploads() TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION core.sweep_site_model_uploads();
DROP TABLE core.site_model_chunks,core.site_model_uploads,core.site_models;
ALTER TABLE core.sites DROP COLUMN model_revision;
RESET ROLE;
