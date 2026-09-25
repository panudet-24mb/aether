-- Automations that command devices (Zigbee2MQTT phase 3).
--
-- `enabled_by` is the member who last switched a flow on. Every command the flow asks for is sent on that member's
-- authority (the command's actor), and that authority is re-checked each time the flow fires.
--
-- core.automation_command_requests is the outbox between ingest and the command queue. A firing flow does not
-- queue a command itself: ingest holds its gateway row FOR UPDATE, and queueing takes the per-tenant command lock
-- and a FOR KEY SHARE on the target's gateway (the foreign key), which in the opposite order is exactly what a
-- command from the web UI or a flow on another gateway does. Instead ingest inserts a request here — no foreign
-- key, no advisory lock — and mqtt-commander turns it into a command in its own transaction, taking the tenant
-- lock first, like the web path. The only lock order anywhere is therefore: tenant command lock, then gateway rows.
--
-- +goose Up
SET ROLE aether_owner;
ALTER TABLE core.automations ADD COLUMN enabled_by uuid;
-- The per-device and per-workspace automation budgets count these.
CREATE INDEX device_commands_automation ON core.device_commands(tenant_id,device_id,created_at DESC) WHERE source='automation';

CREATE TABLE core.automation_command_requests (
 tenant_id uuid NOT NULL, id uuid NOT NULL,
 automation_id uuid NOT NULL, run_node text NOT NULL CHECK(length(run_node) BETWEEN 1 AND 64),
 device_id uuid NOT NULL,
 property text NOT NULL CHECK(property ~ '^[A-Za-z0-9_]{1,64}$'),
 value jsonb NOT NULL CHECK(octet_length(value::text)<=1024),
 -- The flow's project when it fired (NULL = a workspace-wide flow) and the member it acts for.
 project_id uuid, actor_id uuid,
 status text NOT NULL DEFAULT 'requested' CHECK(status IN ('requested','queued','blocked')),
 reason text NOT NULL DEFAULT '' CHECK(length(reason)<=64),
 -- The command it became (its id is the request's own id, so a retried drain cannot queue it twice).
 command_id uuid,
 created_at timestamptz NOT NULL DEFAULT now(), settled_at timestamptz,
 PRIMARY KEY(tenant_id,id)
);
CREATE INDEX automation_command_requests_open ON core.automation_command_requests(tenant_id,created_at) WHERE status='requested';
CREATE INDEX automation_command_requests_flow ON core.automation_command_requests(tenant_id,automation_id,created_at DESC);
ALTER TABLE core.automation_command_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.automation_command_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.automation_command_requests USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
-- Members see the requests of the flows they can see; ingest and the commander run with the whole scope.
CREATE POLICY project_scope ON core.automation_command_requests AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.automation_in_scope(automation_id))
 WITH CHECK((SELECT core.scope_all()) OR core.automation_in_scope(automation_id));
GRANT SELECT,INSERT,UPDATE,DELETE ON core.automation_command_requests TO aether_app;

-- May `member` command a device on `gateway` right now? The same rule as a command from the web UI
-- (domain.Principal.MayControl plus project scope, core.compute_project_scope): owner, admin or operator; unless
-- owner, the "control" module is not none/read; unless owner, either unrestricted to projects or restricted to
-- one that holds the gateway. SECURITY DEFINER so ingest (no user) and a project-scoped admin get the same answer;
-- every table is filtered by the current tenant explicitly.
CREATE POLICY member_access_owner_read ON core.member_access FOR SELECT TO aether_owner USING(true);
CREATE FUNCTION core.member_may_command(member uuid, gateway uuid) RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM core.memberships m
  WHERE m.tenant_id = core.tenant_id() AND m.user_id = member AND m.role IN ('owner','admin','operator')
   AND (m.role = 'owner' OR NOT EXISTS(SELECT 1 FROM core.member_access a
     WHERE a.tenant_id = m.tenant_id AND a.user_id = m.user_id AND a.permissions->>'control' IN ('none','read')))
   AND (m.role = 'owner'
     OR NOT EXISTS(SELECT 1 FROM core.member_projects p WHERE p.tenant_id = m.tenant_id AND p.user_id = m.user_id)
     OR EXISTS(SELECT 1 FROM core.member_projects p JOIN core.gateways g ON g.tenant_id = p.tenant_id AND g.project_id = p.project_id
       WHERE p.tenant_id = m.tenant_id AND p.user_id = m.user_id AND g.id = gateway)))
$$;
REVOKE ALL ON FUNCTION core.member_may_command(uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.member_may_command(uuid,uuid) TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION core.member_may_command(uuid,uuid);
DROP POLICY member_access_owner_read ON core.member_access;
DROP TABLE core.automation_command_requests;
DROP INDEX core.device_commands_automation;
ALTER TABLE core.automations DROP COLUMN enabled_by;
RESET ROLE;
