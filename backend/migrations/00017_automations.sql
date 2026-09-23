-- +goose Up
SET ROLE aether_owner;
-- Automation Studio: a tenant-drawn "ถ้า…แล้ว…" flow stored as a block graph in `definition`
-- ({nodes:[{id,type,position,data}],edges:[{id,source,target,sourceHandle}]}, validated in Go).
-- `revision` gives the studio optimistic concurrency: a save with a stale revision is a 409.
CREATE TABLE core.automations (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 name text NOT NULL, description text NOT NULL DEFAULT '',
 enabled boolean NOT NULL DEFAULT false, project_id uuid,
 definition jsonb NOT NULL DEFAULT '{"nodes":[],"edges":[]}'::jsonb,
 revision integer NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 last_fired_at timestamptz, fire_count bigint NOT NULL DEFAULT 0,
 PRIMARY KEY(tenant_id,id),
 -- Same composite shape core.gateways uses, so a flow can only reference a project of its own tenant.
 CONSTRAINT automations_tenant_id_project_id_fkey FOREIGN KEY(tenant_id,project_id) REFERENCES core.projects(tenant_id,id)
);
CREATE UNIQUE INDEX automations_name ON core.automations(tenant_id,lower(name));
CREATE INDEX automations_enabled ON core.automations(tenant_id,updated_at DESC) WHERE enabled;
-- Rising-edge memory for trigger.metric: `since` is when the comparison first became true for this
-- identity, `active` says the episode already fired. The row is deleted when the comparison goes false,
-- so one episode fires once however many uplinks it spans, and `for_sec` can be measured from `since`.
CREATE TABLE core.automation_state (
 tenant_id uuid NOT NULL, automation_id uuid NOT NULL, node_id text NOT NULL, external_id text NOT NULL,
 active boolean NOT NULL DEFAULT false, since timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,automation_id,node_id,external_id),
 -- ON DELETE CASCADE on the full composite key: ON DELETE SET NULL would null tenant_id as well.
 FOREIGN KEY(tenant_id,automation_id) REFERENCES core.automations(tenant_id,id) ON DELETE CASCADE
);
-- One row per evaluation that reached a decision, so the studio can show why a flow did or did not fire.
CREATE TABLE core.automation_runs (
 tenant_id uuid NOT NULL, id uuid NOT NULL, automation_id uuid NOT NULL,
 trigger_node text NOT NULL DEFAULT '', external_id text NOT NULL DEFAULT '', gateway_id uuid,
 status text NOT NULL CHECK(status IN ('fired','skipped','error')),
 detail jsonb NOT NULL DEFAULT '{}'::jsonb, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,automation_id) REFERENCES core.automations(tenant_id,id) ON DELETE CASCADE
);
CREATE INDEX automation_runs_recent ON core.automation_runs(tenant_id,automation_id,created_at DESC);
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['automations','automation_state','automation_runs'] LOOP
 EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())',t);
 END LOOP;
END $$;
-- +goose StatementEnd
-- DELETE on automations is a hard delete (runs and state cascade); DELETE on runs/state is the pruner.
GRANT SELECT,INSERT,UPDATE,DELETE ON core.automations,core.automation_state,core.automation_runs TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.automation_runs;
DROP TABLE core.automation_state;
DROP TABLE core.automations;
RESET ROLE;
