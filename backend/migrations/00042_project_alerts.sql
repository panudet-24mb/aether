-- +goose Up
-- Alert rules and notification channels can belong to one project (docs/platform/alerts.md, "Project-scoped
-- rules and channels"). NULL keeps today's meaning, "the whole workspace", and every existing row keeps it,
-- so nothing changes until somebody picks a project.
SET ROLE aether_owner;
-- The ALTERs below take ACCESS EXCLUSIVE on the two tables that every alert evaluation reads; queueing behind a
-- long reader must not stall ingest for long, so the migration fails and can be retried instead.
SET LOCAL lock_timeout = '5s';

ALTER TABLE core.alert_rules ADD COLUMN project_id uuid;
ALTER TABLE core.alert_rules ADD CONSTRAINT alert_rules_tenant_id_project_id_fkey
 FOREIGN KEY(tenant_id,project_id) REFERENCES core.projects(tenant_id,id) ON DELETE CASCADE;
CREATE INDEX alert_rules_project ON core.alert_rules(tenant_id,project_id);

ALTER TABLE core.notification_channels ADD COLUMN project_id uuid;
ALTER TABLE core.notification_channels ADD CONSTRAINT notification_channels_tenant_id_project_id_fkey
 FOREIGN KEY(tenant_id,project_id) REFERENCES core.projects(tenant_id,id) ON DELETE CASCADE;
CREATE INDEX notification_channels_project ON core.notification_channels(tenant_id,project_id);

-- An SOS or hazard that no rule covered opens a "fallback" alert with no rule. Automation alerts have no rule
-- either, so the flag is what keeps them apart: the fallback deduplicates only against open fallback alerts.
-- A constant default makes this a metadata-only change.
ALTER TABLE core.alerts ADD COLUMN fallback boolean NOT NULL DEFAULT false;

-- Life-safety rules may not hold a repeat back for long: a second press five minutes later is a new call for
-- help. The built-in rules use 30 s (SOS) and 300 s (hazard); an edited rule above the cap is brought down.
ALTER TABLE core.alert_rules NO FORCE ROW LEVEL SECURITY;
UPDATE core.alert_rules SET dedupe_sec = 300 WHERE event_type IN ('button','hazard') AND dedupe_sec > 300;
ALTER TABLE core.alert_rules FORCE ROW LEVEL SECURITY;
ALTER TABLE core.alert_rules ADD CONSTRAINT alert_rules_life_safety_dedupe
 CHECK(event_type NOT IN ('button','hazard') OR dedupe_sec <= 300);

-- Per-command restrictive policies, ANDed with tenant_scope. A restricted member reads workspace-wide rows
-- and rows of their projects, and writes only rows of their projects; full scope (owner, unrestricted
-- admin, ingest and the workers, all with app.project_scope='*') is unaffected. Writes stay owner/admin
-- only in the API; these policies are what keeps a project-limited admin inside their projects.
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['alert_rules','notification_channels'] LOOP
  EXECUTE format('CREATE POLICY project_scope_read ON core.%I AS RESTRICTIVE FOR SELECT TO aether_app
   USING((SELECT core.scope_all()) OR project_id IS NULL OR core.project_in_scope(project_id))', t);
  EXECUTE format('CREATE POLICY project_scope_insert ON core.%I AS RESTRICTIVE FOR INSERT TO aether_app
   WITH CHECK((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))', t);
  EXECUTE format('CREATE POLICY project_scope_update ON core.%I AS RESTRICTIVE FOR UPDATE TO aether_app
   USING((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))
   WITH CHECK((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))', t);
  EXECUTE format('CREATE POLICY project_scope_delete ON core.%I AS RESTRICTIVE FOR DELETE TO aether_app
   USING((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))', t);
 END LOOP;
END $$;
-- +goose StatementEnd

-- The per-workspace caps (100 rules, 20 channels) must count every row of the workspace, not the rows a
-- project-limited admin can see, or that admin could create past the cap.
CREATE FUNCTION core.alert_rule_count() RETURNS bigint
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT count(*) FROM core.alert_rules r WHERE r.tenant_id = core.tenant_id()
$$;
CREATE FUNCTION core.notification_channel_count() RETURNS bigint
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT count(*) FROM core.notification_channels c WHERE c.tenant_id = core.tenant_id()
$$;
-- core.alert_rules and core.notification_channels are FORCE RLS: the definer (aether_owner) reads them through
-- tenant_scope, which is not TO aether_app. The count above therefore stays inside core.tenant_id().
REVOKE ALL ON FUNCTION core.alert_rule_count(), core.notification_channel_count() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.alert_rule_count(), core.notification_channel_count() TO aether_app;

-- Email channels with audience "members": who in this workspace may see this alert. The alert must belong
-- to the current workspace and the caller must have full scope (the delivery worker does; a project-limited
-- session learns nothing). A member counts when they can see the alert's project (owner always, a member
-- with no project list sees every project, otherwise the list must contain it; a gateway with no project is
-- therefore only seen by owners and unrestricted members), holds one of the roles, is not an erased
-- identity and has not had the alerts module set to "none". Ordered owner, admin, operator, viewer, then
-- email, so the caller's cap (docs/platform/alerts.md) keeps the most responsible people.
-- It runs as aether_owner, which reads core.member_access through member_access_owner_read (00031).
CREATE FUNCTION core.alert_recipients(alert uuid, roles text[]) RETURNS TABLE(email text)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 WITH target AS (
  SELECT al.tenant_id, g.project_id FROM core.alerts al
   JOIN core.gateways g ON g.tenant_id = al.tenant_id AND g.id = al.gateway_id
   WHERE al.tenant_id = core.tenant_id() AND al.id = alert AND core.scope_all()
 )
 SELECT u.email FROM target t
  JOIN core.memberships m ON m.tenant_id = t.tenant_id
  JOIN identity.users u ON u.id = m.user_id
  WHERE u.erased_at IS NULL AND m.role = ANY(roles)
   AND (m.role = 'owner'
    OR NOT EXISTS(SELECT 1 FROM core.member_projects mp WHERE mp.tenant_id = m.tenant_id AND mp.user_id = m.user_id)
    OR (t.project_id IS NOT NULL AND EXISTS(SELECT 1 FROM core.member_projects mp
         WHERE mp.tenant_id = m.tenant_id AND mp.user_id = m.user_id AND mp.project_id = t.project_id)))
   AND (m.role = 'owner' OR coalesce((SELECT ma.permissions->>'alerts' FROM core.member_access ma
         WHERE ma.tenant_id = m.tenant_id AND ma.user_id = m.user_id), '') <> 'none')
  ORDER BY array_position(ARRAY['owner','admin','operator','viewer'], m.role), u.email
$$;
REVOKE ALL ON FUNCTION core.alert_recipients(uuid, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.alert_recipients(uuid, text[]) TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';
DROP FUNCTION core.alert_recipients(uuid, text[]);
DROP FUNCTION core.alert_rule_count(), core.notification_channel_count();
-- The dedupe values lowered by Up stay lowered; only the constraint goes.
ALTER TABLE core.alert_rules DROP CONSTRAINT alert_rules_life_safety_dedupe;
ALTER TABLE core.alerts DROP COLUMN fallback;
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['alert_rules','notification_channels'] LOOP
  EXECUTE format('DROP POLICY project_scope_read ON core.%I', t);
  EXECUTE format('DROP POLICY project_scope_insert ON core.%I', t);
  EXECUTE format('DROP POLICY project_scope_update ON core.%I', t);
  EXECUTE format('DROP POLICY project_scope_delete ON core.%I', t);
 END LOOP;
END $$;
-- +goose StatementEnd
-- Project rules and channels have no workspace-wide meaning to fall back to; widening them silently to the
-- whole workspace would send one project's alerts to everybody, so they are removed with the column.
ALTER TABLE core.alert_rules NO FORCE ROW LEVEL SECURITY;
ALTER TABLE core.notification_channels NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.alert_rules WHERE project_id IS NOT NULL;
DELETE FROM core.notification_channels WHERE project_id IS NOT NULL;
ALTER TABLE core.alert_rules FORCE ROW LEVEL SECURITY;
ALTER TABLE core.notification_channels FORCE ROW LEVEL SECURITY;
DROP INDEX core.notification_channels_project;
ALTER TABLE core.notification_channels DROP COLUMN project_id;
DROP INDEX core.alert_rules_project;
ALTER TABLE core.alert_rules DROP COLUMN project_id;
RESET ROLE;
