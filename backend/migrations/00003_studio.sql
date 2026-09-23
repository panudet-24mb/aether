-- +goose Up
SET ROLE aether_owner;
CREATE TABLE core.studio_items (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 kind text NOT NULL CHECK(kind IN ('widget','decoder','dashboard')),
 name text NOT NULL, brand text NOT NULL, model text NOT NULL, version integer NOT NULL CHECK(version>0),
 visibility text NOT NULL CHECK(visibility IN ('private','community')),
 definition jsonb NOT NULL, revision integer NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(tenant_id,kind,name,version)
);
ALTER TABLE core.studio_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.studio_items FORCE ROW LEVEL SECURITY;
CREATE POLICY studio_read ON core.studio_items FOR SELECT USING(tenant_id=core.tenant_id() OR (visibility='community' AND kind IN ('widget','decoder')));
CREATE POLICY studio_insert ON core.studio_items FOR INSERT WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY studio_update ON core.studio_items FOR UPDATE USING(tenant_id=core.tenant_id() AND kind='dashboard') WITH CHECK(tenant_id=core.tenant_id() AND kind='dashboard' AND visibility='private');
CREATE POLICY studio_delete ON core.studio_items FOR DELETE USING(tenant_id=core.tenant_id() AND kind='dashboard');
GRANT SELECT,INSERT,UPDATE,DELETE ON core.studio_items TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.studio_items;
RESET ROLE;
