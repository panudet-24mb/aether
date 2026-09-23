-- +goose Up
SET ROLE aether_owner;
-- Asset registry: the owner's own bookkeeping on top of the operational tables. asset_id is polymorphic
-- (a gateway id or a device id), so there is no foreign key; the repository proves the asset belongs to
-- the tenant (gateway not revoked, device not removed) before writing.
CREATE TABLE core.asset_records (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 asset_kind text NOT NULL CHECK(asset_kind IN ('gateway','device')), asset_id uuid NOT NULL,
 serial_no text NOT NULL DEFAULT '', asset_tag text NOT NULL DEFAULT '', location_note text NOT NULL DEFAULT '',
 vendor text NOT NULL DEFAULT '', purchased_at date, warranty_until date, battery_changed_at date,
 status text NOT NULL DEFAULT 'in_service' CHECK(status IN ('in_service','spare','repair','retired')),
 notes text NOT NULL DEFAULT '', updated_at timestamptz NOT NULL DEFAULT now(), updated_by uuid,
 PRIMARY KEY(tenant_id,asset_kind,asset_id)
);
-- Preventive rounds: "every interval_days do title", next_due carried forward when a log completes it.
CREATE TABLE core.maintenance_plans (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 asset_kind text NOT NULL CHECK(asset_kind IN ('gateway','device')), asset_id uuid NOT NULL,
 title text NOT NULL, kind text NOT NULL CHECK(kind IN ('pm','calibration','battery','inspection','other')),
 interval_days integer NOT NULL CHECK(interval_days BETWEEN 1 AND 3650),
 next_due date NOT NULL, last_done date, enabled boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id)
);
CREATE INDEX maintenance_plans_due ON core.maintenance_plans(tenant_id,next_due) WHERE enabled;
CREATE INDEX maintenance_plans_asset ON core.maintenance_plans(tenant_id,asset_kind,asset_id);
-- Append-only maintenance history (corrective, preventive, battery swaps, calibration, free notes).
CREATE TABLE core.maintenance_logs (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 asset_kind text NOT NULL CHECK(asset_kind IN ('gateway','device')), asset_id uuid NOT NULL,
 plan_id uuid, kind text NOT NULL CHECK(kind IN ('pm','ma','repair','battery','calibration','inspection','note')),
 title text NOT NULL, detail text NOT NULL DEFAULT '', performed_at timestamptz NOT NULL DEFAULT now(),
 performed_by text NOT NULL DEFAULT '', cost numeric(12,2) CHECK(cost IS NULL OR cost >= 0),
 created_by uuid, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id)
);
-- plan_id has no foreign key on purpose: a plan may be deleted while the round it triggered stays in history.
CREATE INDEX maintenance_logs_asset ON core.maintenance_logs(tenant_id,asset_kind,asset_id,performed_at DESC);
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['asset_records','maintenance_plans','maintenance_logs'] LOOP
 EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())',t);
 END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT,INSERT,UPDATE ON core.asset_records TO aether_app;
GRANT SELECT,INSERT,UPDATE,DELETE ON core.maintenance_plans TO aether_app;
-- History is evidence: it can be written and read, never rewritten or erased by the runtime.
GRANT SELECT,INSERT ON core.maintenance_logs TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.maintenance_logs;
DROP TABLE core.maintenance_plans;
DROP TABLE core.asset_records;
RESET ROLE;
