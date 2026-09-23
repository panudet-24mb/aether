-- +goose Up
SET ROLE aether_owner;
-- Trigger index for the Automation Studio runtime. One uplink used to load every enabled flow, parse
-- its definition and ask the database about every trigger.metric block × every sensor in the packet:
-- O(flows × blocks × sensors) round trips inside the ingest transaction. This table answers "which
-- flows can this packet possibly wake?" with one index lookup, so a workspace can keep hundreds of
-- flows enabled and an uplink still only touches the few that can fire.
--
-- Rows exist only for ENABLED flows: disabling one deletes its rows, which is also how the runtime
-- learns not to evaluate it. NULL means "any" (or "not applicable") in every selector column, so a
-- row always matches at least everything its block matches. A block filtering on many devices or
-- gateways records one row per event type with those columns left open rather than the cross product
-- of its selector lists: an extra candidate flow costs one definition parse, a missed one would be a
-- flow that silently stops working. automation.MatchesEvent and automation.MetricTest still decide.
CREATE TABLE core.automation_triggers (
 tenant_id uuid NOT NULL, automation_id uuid NOT NULL, node_id text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('event','metric')),
 event_type text, external_id text, gateway_id uuid, metric text,
 -- ON DELETE CASCADE on the full composite key, as core.automation_state does: ON DELETE SET NULL
 -- would null tenant_id as well, and a deleted flow must leave no index rows behind.
 CONSTRAINT automation_triggers_automation_fkey FOREIGN KEY(tenant_id,automation_id)
  REFERENCES core.automations(tenant_id,id) ON DELETE CASCADE
);
-- The two lookups one uplink makes, as partial indexes so each kind gets its own small tree.
CREATE INDEX automation_triggers_event ON core.automation_triggers(tenant_id,event_type,external_id) WHERE kind='event';
CREATE INDEX automation_triggers_metric ON core.automation_triggers(tenant_id,external_id) WHERE kind='metric';
-- Rewriting one flow's rows (create, save, enable, disable) deletes by (tenant, automation).
CREATE INDEX automation_triggers_flow ON core.automation_triggers(tenant_id,automation_id);

-- Backfill from the flows that are already enabled. This runs before RLS is switched on, because
-- FORCE ROW LEVEL SECURITY applies to the owner too and core.tenant_id() is not set during a
-- migration. The shape must match automation.TriggerIndex in Go.
INSERT INTO core.automation_triggers(tenant_id,automation_id,node_id,kind,event_type,external_id,gateway_id,metric)
SELECT DISTINCT a.tenant_id, a.id, node->>'id', 'event'::text, types.event_type,
 -- Exactly one device or gateway selected pins that column. None or several mean "any", i.e. NULL.
 CASE WHEN jsonb_typeof(node->'data'->'external_ids')='array' AND jsonb_array_length(node->'data'->'external_ids')=1
      THEN lower(node->'data'->'external_ids'->>0) END,
 CASE WHEN jsonb_typeof(node->'data'->'gateway_ids')='array' AND jsonb_array_length(node->'data'->'gateway_ids')=1
      THEN (node->'data'->'gateway_ids'->>0)::uuid END,
 NULL::text
FROM core.automations a
 CROSS JOIN LATERAL jsonb_array_elements(
   CASE WHEN jsonb_typeof(a.definition->'nodes')='array' THEN a.definition->'nodes' ELSE '[]'::jsonb END) AS nodes(node)
 -- LEFT JOIN, so an empty event_types list ("any event") still yields one row, with NULL.
 LEFT JOIN LATERAL jsonb_array_elements_text(
   CASE WHEN jsonb_typeof(node->'data'->'event_types')='array' THEN node->'data'->'event_types' ELSE '[]'::jsonb END) AS types(event_type) ON true
WHERE a.enabled AND node->>'type'='trigger.event' AND node->>'id' IS NOT NULL;

INSERT INTO core.automation_triggers(tenant_id,automation_id,node_id,kind,event_type,external_id,gateway_id,metric)
SELECT DISTINCT a.tenant_id, a.id, node->>'id', 'metric'::text, NULL::text,
 nullif(lower(node->'data'->>'external_id'),''), NULL::uuid, nullif(node->'data'->>'metric','')
FROM core.automations a
 CROSS JOIN LATERAL jsonb_array_elements(
   CASE WHEN jsonb_typeof(a.definition->'nodes')='array' THEN a.definition->'nodes' ELSE '[]'::jsonb END) AS nodes(node)
WHERE a.enabled AND node->>'type'='trigger.metric' AND node->>'id' IS NOT NULL;

ALTER TABLE core.automation_triggers ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.automation_triggers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.automation_triggers USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
-- Restricted members only see the trigger rows of automations in their projects (function from 00019).
CREATE POLICY project_scope ON core.automation_triggers AS RESTRICTIVE TO aether_app
 USING(core.automation_in_scope(automation_id)) WITH CHECK(core.automation_in_scope(automation_id));
-- No UPDATE: the index is maintained by deleting a flow's rows and writing them again.
GRANT SELECT,INSERT,DELETE ON core.automation_triggers TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
-- Dropping the table takes its indexes and its policy with it; the index is derived data, so there
-- is nothing to preserve.
DROP TABLE core.automation_triggers;
RESET ROLE;
