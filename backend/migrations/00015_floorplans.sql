-- +goose Up
SET ROLE aether_owner;
-- Sites (buildings) hold floors; a floor holds a drawing in metres (walls, zones, fixtures) and asset placements.
CREATE TABLE core.sites (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 project_id uuid, name text NOT NULL, description text NOT NULL DEFAULT '',
 archived_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,project_id) REFERENCES core.projects(tenant_id,id)
);
CREATE TABLE core.floors (
 tenant_id uuid NOT NULL, id uuid NOT NULL, site_id uuid NOT NULL,
 name text NOT NULL, level integer NOT NULL DEFAULT 1 CHECK(level BETWEEN -20 AND 200),
 width_m real NOT NULL DEFAULT 30 CHECK(width_m BETWEEN 2 AND 1000),
 depth_m real NOT NULL DEFAULT 20 CHECK(depth_m BETWEEN 2 AND 1000),
 ceiling_m real NOT NULL DEFAULT 3 CHECK(ceiling_m BETWEEN 2 AND 30),
 layout jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(pg_column_size(layout) <= 400000),
 revision integer NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES core.sites(tenant_id,id) ON DELETE CASCADE
);
CREATE INDEX floors_site ON core.floors(tenant_id,site_id,level);
-- An asset stands in one place: one row per gateway or device, whatever floor it is on.
CREATE TABLE core.floor_placements (
 tenant_id uuid NOT NULL, asset_kind text NOT NULL CHECK(asset_kind IN ('gateway','device')), asset_id uuid NOT NULL,
 floor_id uuid NOT NULL, x real NOT NULL, y real NOT NULL, z real NOT NULL DEFAULT 1.5,
 PRIMARY KEY(tenant_id,asset_kind,asset_id),
 FOREIGN KEY(tenant_id,floor_id) REFERENCES core.floors(tenant_id,id) ON DELETE CASCADE
);
CREATE INDEX floor_placements_floor ON core.floor_placements(tenant_id,floor_id);
-- Optional scanned plan to trace over.
CREATE TABLE core.floor_images (
 tenant_id uuid NOT NULL, floor_id uuid NOT NULL, mime text NOT NULL CHECK(mime IN ('image/png','image/jpeg','image/webp')),
 data bytea NOT NULL CHECK(octet_length(data) <= 196608), updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,floor_id),
 FOREIGN KEY(tenant_id,floor_id) REFERENCES core.floors(tenant_id,id) ON DELETE CASCADE
);
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['sites','floors','floor_placements','floor_images'] LOOP
  EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())',t);
 END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT,INSERT,UPDATE ON core.sites TO aether_app;
GRANT SELECT,INSERT,UPDATE,DELETE ON core.floors,core.floor_placements,core.floor_images TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.floor_images,core.floor_placements,core.floors,core.sites;
RESET ROLE;
