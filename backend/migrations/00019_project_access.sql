-- +goose Up
SET ROLE aether_owner;

-- ============================================================================
-- A. Members of a workspace
-- ----------------------------------------------------------------------------
-- core.memberships already carries the role (owner/admin/operator/viewer). What is missing is a way for
-- an owner/admin to add people, and a flag for the initial password: on-premise has no email delivery,
-- so the owner hands the first password over in person and the member must replace it on first use.
-- ============================================================================
ALTER TABLE core.memberships
  ADD COLUMN must_change_password boolean NOT NULL DEFAULT false,
  ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
-- core.member_tenant_count() asks "does this identity live in another workspace too?" by user_id alone.
CREATE INDEX memberships_user ON core.memberships(user_id);

-- Which projects a member may see. No rows at all means "every project" (today's behaviour); owner and
-- admin ignore this table entirely. Both foreign keys are composite so a row can never leave its tenant.
CREATE TABLE core.member_projects (
 tenant_id uuid NOT NULL, user_id uuid NOT NULL, project_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,user_id,project_id),
 FOREIGN KEY(tenant_id,user_id) REFERENCES core.memberships(tenant_id,user_id) ON DELETE CASCADE,
 FOREIGN KEY(tenant_id,project_id) REFERENCES core.projects(tenant_id,id) ON DELETE CASCADE
);
CREATE INDEX member_projects_project ON core.member_projects(tenant_id,project_id);
ALTER TABLE core.member_projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.member_projects FORCE ROW LEVEL SECURITY;

-- Both membership tables are FORCE RLS, so the SECURITY DEFINER helpers below (which run as this
-- non-login owner) need an explicit owner policy to see and write rows; same pattern as migration 00007.
CREATE POLICY memberships_owner_access ON core.memberships FOR ALL TO aether_owner USING(true) WITH CHECK(true);
CREATE POLICY member_projects_owner_access ON core.member_projects FOR ALL TO aether_owner USING(true) WITH CHECK(true);
CREATE POLICY sessions_owner_access ON identity.sessions FOR ALL TO aether_owner USING(true) WITH CHECK(true);
CREATE POLICY refresh_owner_access ON identity.refresh_tokens FOR ALL TO aether_owner USING(true) WITH CHECK(true);

-- A membership policy that reads core.memberships would recurse on the same table. The check therefore
-- lives in a SECURITY DEFINER function; the runtime policies below are TO aether_app only, so evaluating
-- them as aether_owner (inside this function) can never re-enter them.
CREATE FUNCTION core.is_tenant_admin() RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM core.memberships m
   WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id() AND m.role IN ('owner','admin'))
$$;
REVOKE ALL ON FUNCTION core.is_tenant_admin() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.is_tenant_admin() TO aether_app;

-- An owner/admin manages the memberships of the current tenant, and nothing else.
CREATE POLICY tenant_admin_membership_read ON core.memberships FOR SELECT TO aether_app
 USING(tenant_id = core.tenant_id() AND core.is_tenant_admin());
CREATE POLICY tenant_admin_membership_insert ON core.memberships FOR INSERT TO aether_app
 WITH CHECK(tenant_id = core.tenant_id() AND core.is_tenant_admin());
CREATE POLICY tenant_admin_membership_update ON core.memberships FOR UPDATE TO aether_app
 USING(tenant_id = core.tenant_id() AND core.is_tenant_admin())
 WITH CHECK(tenant_id = core.tenant_id() AND core.is_tenant_admin());
CREATE POLICY tenant_admin_membership_delete ON core.memberships FOR DELETE TO aether_app
 USING(tenant_id = core.tenant_id() AND core.is_tenant_admin());
-- Column-scoped: the runtime may change a role, never rewrite identity, tenant or the password flag.
GRANT UPDATE(role) ON core.memberships TO aether_app;
GRANT DELETE ON core.memberships TO aether_app;

CREATE POLICY own_member_projects_read ON core.member_projects FOR SELECT TO aether_app
 USING(tenant_id = core.tenant_id() AND user_id = identity.user_id());
CREATE POLICY tenant_admin_member_projects ON core.member_projects FOR ALL TO aether_app
 USING(tenant_id = core.tenant_id() AND core.is_tenant_admin())
 WITH CHECK(tenant_id = core.tenant_id() AND core.is_tenant_admin());
GRANT SELECT,INSERT,DELETE ON core.member_projects TO aether_app;

-- How many workspaces an identity belongs to. An admin may only ask about somebody who is already a
-- member of their own tenant, and learns a number, never another workspace's name or the identity itself.
CREATE FUNCTION core.member_tenant_count(target uuid) RETURNS integer
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.is_tenant_admin()
   AND EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = target)
   THEN (SELECT count(*)::integer FROM core.memberships m WHERE m.user_id = target) ELSE 0 END
$$;
REVOKE ALL ON FUNCTION core.member_tenant_count(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.member_tenant_count(uuid) TO aether_app;

-- identity.sessions is user-scoped RLS, so an admin cannot read another member's sessions directly.
-- This returns one timestamp per member of the current tenant and nothing else.
CREATE FUNCTION identity.tenant_last_seen() RETURNS TABLE(user_id uuid, last_seen timestamptz)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT s.user_id, max(s.created_at) FROM identity.sessions s
 WHERE core.is_tenant_admin() AND s.tenant_id = core.tenant_id() GROUP BY s.user_id
$$;
REVOKE ALL ON FUNCTION identity.tenant_last_seen() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.tenant_last_seen() TO aether_app;

-- Removing a member or resetting their password ends their access immediately. identity.sessions has a
-- foreign key to the membership row, so the rows are deleted rather than merely revoked.
-- +goose StatementBegin
CREATE FUNCTION identity.purge_tenant_sessions(target uuid) RETURNS integer
 LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE gone integer;
BEGIN
 IF NOT core.is_tenant_admin() OR target IS NULL OR target = identity.user_id() THEN
  RETURN 0;
 END IF;
 DELETE FROM identity.refresh_tokens rt USING identity.sessions s
  WHERE rt.session_id = s.id AND s.user_id = target AND s.tenant_id = core.tenant_id();
 DELETE FROM identity.sessions s WHERE s.user_id = target AND s.tenant_id = core.tenant_id();
 GET DIAGNOSTICS gone = ROW_COUNT;
 RETURN gone;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION identity.purge_tenant_sessions(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.purge_tenant_sessions(uuid) TO aether_app;

-- Password writes never become a plain UPDATE grant on identity.users: an owner/admin may only rewrite
-- the password of somebody who belongs to this workspace and to no other one.
-- +goose StatementBegin
CREATE FUNCTION identity.set_member_password(target uuid, hash text) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
 IF NOT core.is_tenant_admin() OR target IS NULL OR target = identity.user_id() OR hash IS NULL THEN
  RETURN false;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = target) THEN
  RETURN false;
 END IF;
 -- Shared identity: the same person in another organisation. Not this workspace's password to change.
 IF (SELECT count(*) FROM core.memberships m WHERE m.user_id = target) <> 1 THEN
  RETURN false;
 END IF;
 UPDATE identity.users SET password_hash = hash WHERE id = target;
 UPDATE core.memberships SET must_change_password = true
  WHERE tenant_id = core.tenant_id() AND user_id = target;
 RETURN true;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION identity.set_member_password(uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.set_member_password(uuid,text) TO aether_app;

-- The signed-in user replacing their own password. The caller has already verified the current password.
-- +goose StatementBegin
CREATE FUNCTION identity.change_own_password(hash text) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
 IF identity.user_id() IS NULL OR hash IS NULL THEN
  RETURN false;
 END IF;
 UPDATE identity.users SET password_hash = hash WHERE id = identity.user_id();
 UPDATE core.memberships SET must_change_password = false
  WHERE tenant_id = core.tenant_id() AND user_id = identity.user_id();
 RETURN true;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION identity.change_own_password(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.change_own_password(text) TO aether_app;

-- ============================================================================
-- B. Project-scoped access, enforced by the database
-- ----------------------------------------------------------------------------
-- Repository.tx sets app.project_scope from core.compute_project_scope() right after the identity, once
-- per transaction. Every restrictive policy below reads that one transaction-local setting, so no query
-- (existing or future) can forget the narrowing, and a connection that never computed the setting is
-- denied everything.
-- ============================================================================

-- SECURITY DEFINER on purpose: the scope is the authority over what the caller may read, so it must be
-- derived from the membership rows themselves and not from what the caller's own read policies allow.
-- It takes no arguments and reads only app.user_id / app.tenant_id, so there is nothing to inject.
CREATE FUNCTION core.compute_project_scope() RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT CASE
  -- System work (packet ingest, offline scanner, notification worker) runs without an identity.
  WHEN identity.user_id() IS NULL OR core.tenant_id() IS NULL THEN '*'
  WHEN EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id()
    AND m.user_id = identity.user_id() AND m.role IN ('owner','admin')) THEN '*'
  WHEN NOT EXISTS(SELECT 1 FROM core.member_projects mp WHERE mp.tenant_id = core.tenant_id()
    AND mp.user_id = identity.user_id()) THEN '*'
  ELSE (SELECT string_agg(mp.project_id::text, ',' ORDER BY mp.project_id) FROM core.member_projects mp
    WHERE mp.tenant_id = core.tenant_id() AND mp.user_id = identity.user_id())
 END
$$;
REVOKE ALL ON FUNCTION core.compute_project_scope() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.compute_project_scope() TO aether_app;

-- Plain STABLE helpers (not SECURITY DEFINER): every EXISTS probe below must itself be filtered by the
-- policies of the table it reads, which is what makes the chain gateway -> project consistent.
-- A missing or empty app.project_scope is not '*' and matches no uuid, so policies fail closed.
CREATE FUNCTION core.scope_all() RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT current_setting('app.project_scope', true) = '*'
$$;
CREATE FUNCTION core.project_in_scope(project uuid) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true WHEN project IS NULL THEN false
  ELSE project::text = ANY(string_to_array(coalesce(current_setting('app.project_scope', true), ''), ','))
 END
$$;
-- CASE keeps the cheap setting comparison first, so ingest and owner sessions never run the EXISTS probe.
CREATE FUNCTION core.gateway_in_scope(gateway uuid) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true WHEN gateway IS NULL THEN false
  ELSE EXISTS(SELECT 1 FROM core.gateways g WHERE g.id = gateway) END
$$;
CREATE FUNCTION core.device_in_scope(device uuid) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true WHEN device IS NULL THEN false
  ELSE EXISTS(SELECT 1 FROM core.devices d WHERE d.id = device) END
$$;
CREATE FUNCTION core.site_in_scope(site uuid) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true WHEN site IS NULL THEN false
  ELSE EXISTS(SELECT 1 FROM core.sites s WHERE s.tenant_id = core.tenant_id() AND s.id = site) END
$$;
CREATE FUNCTION core.floor_in_scope(floor uuid) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true WHEN floor IS NULL THEN false
  ELSE EXISTS(SELECT 1 FROM core.floors f WHERE f.tenant_id = core.tenant_id() AND f.id = floor) END
$$;
CREATE FUNCTION core.automation_in_scope(automation uuid) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true WHEN automation IS NULL THEN false
  ELSE EXISTS(SELECT 1 FROM core.automations a WHERE a.tenant_id = core.tenant_id() AND a.id = automation) END
$$;
CREATE FUNCTION core.alert_in_scope(alert uuid) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true WHEN alert IS NULL THEN false
  ELSE EXISTS(SELECT 1 FROM core.alerts al WHERE al.tenant_id = core.tenant_id() AND al.id = alert) END
$$;
-- The asset registry is polymorphic (a gateway id or a device id), so it follows whichever it names.
CREATE FUNCTION core.asset_in_scope(kind text, asset uuid) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true
  WHEN kind = 'gateway' THEN core.gateway_in_scope(asset)
  WHEN kind = 'device' THEN core.device_in_scope(asset)
  ELSE false END
$$;
-- Presence is keyed by the BLE identity, not by a gateway: it is in scope when a gateway the member may
-- see has heard that identity (core.sensor_streams is itself gateway-scoped below).
CREATE FUNCTION core.identity_in_scope(ext text) RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.scope_all() THEN true WHEN ext IS NULL THEN false
  ELSE EXISTS(SELECT 1 FROM core.sensor_streams s
    WHERE s.tenant_id = core.tenant_id() AND s.external_id = ext) END
$$;
REVOKE ALL ON FUNCTION core.scope_all(), core.project_in_scope(uuid), core.gateway_in_scope(uuid),
 core.device_in_scope(uuid), core.site_in_scope(uuid), core.floor_in_scope(uuid),
 core.automation_in_scope(uuid), core.alert_in_scope(uuid), core.asset_in_scope(text,uuid),
 core.identity_in_scope(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.scope_all(), core.project_in_scope(uuid), core.gateway_in_scope(uuid),
 core.device_in_scope(uuid), core.site_in_scope(uuid), core.floor_in_scope(uuid),
 core.automation_in_scope(uuid), core.alert_in_scope(uuid), core.asset_in_scope(text,uuid),
 core.identity_in_scope(text) TO aether_app;

-- RESTRICTIVE: ANDed with the existing permissive tenant_scope policy, so a row must be inside the tenant
-- AND inside the caller's project scope. TO aether_app only: the fixed SECURITY DEFINER lookups owned by
-- aether_owner (core.lookup_gateway, core.mqtt_provisioning_accounts, core.active_tenant_ids) run before
-- any identity exists and must stay unaffected.
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['devices','gateway_packets','sensor_streams','sensor_samples','stream_state','device_events','alerts','ble_history','mqtt_accounts'] LOOP
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.gateway_in_scope(gateway_id)) WITH CHECK(core.gateway_in_scope(gateway_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['telemetry','device_state'] LOOP
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.device_in_scope(device_id)) WITH CHECK(core.device_in_scope(device_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['gateways','sites','automations'] LOOP
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.project_in_scope(project_id)) WITH CHECK(core.project_in_scope(project_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['floor_placements','floor_images'] LOOP
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.floor_in_scope(floor_id)) WITH CHECK(core.floor_in_scope(floor_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['automation_state','automation_runs'] LOOP
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.automation_in_scope(automation_id)) WITH CHECK(core.automation_in_scope(automation_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['asset_records','maintenance_plans','maintenance_logs'] LOOP
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.asset_in_scope(asset_kind,asset_id)) WITH CHECK(core.asset_in_scope(asset_kind,asset_id))',t);
 END LOOP;
END $$;
-- +goose StatementEnd
CREATE POLICY project_scope ON core.projects AS RESTRICTIVE TO aether_app
 USING(core.project_in_scope(id)) WITH CHECK(core.project_in_scope(id));
CREATE POLICY project_scope ON core.floors AS RESTRICTIVE TO aether_app
 USING(core.site_in_scope(site_id)) WITH CHECK(core.site_in_scope(site_id));
CREATE POLICY project_scope ON core.notifications AS RESTRICTIVE TO aether_app
 USING(core.alert_in_scope(alert_id)) WITH CHECK(core.alert_in_scope(alert_id));
CREATE POLICY project_scope ON core.presence_state AS RESTRICTIVE TO aether_app
 USING(core.identity_in_scope(external_id)) WITH CHECK(core.identity_in_scope(external_id));

-- Not project-scoped, on purpose (see docs/platform/team-access.md for the full table):
--  core.tenants, core.memberships, core.member_projects  workspace identity and membership itself
--  core.audit_logs                                       no read path in the API
--  core.device_templates, core.studio_items              decoder/widget definitions, carry no device data
--  core.notification_channels, core.alert_rules          owner/admin-only routes; owner/admin see everything
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['devices','gateway_packets','sensor_streams','sensor_samples','stream_state','device_events','alerts','ble_history','mqtt_accounts','telemetry','device_state','gateways','sites','automations','floor_placements','floor_images','automation_state','automation_runs','asset_records','maintenance_plans','maintenance_logs','projects','floors','notifications','presence_state'] LOOP
  EXECUTE format('DROP POLICY project_scope ON core.%I',t);
 END LOOP;
END $$;
-- +goose StatementEnd
DROP FUNCTION core.identity_in_scope(text), core.asset_in_scope(text,uuid), core.alert_in_scope(uuid),
 core.automation_in_scope(uuid), core.floor_in_scope(uuid), core.site_in_scope(uuid),
 core.device_in_scope(uuid), core.gateway_in_scope(uuid), core.project_in_scope(uuid), core.scope_all();
DROP FUNCTION core.compute_project_scope();
DROP FUNCTION identity.change_own_password(text), identity.set_member_password(uuid,text),
 identity.purge_tenant_sessions(uuid), identity.tenant_last_seen(), core.member_tenant_count(uuid);
-- The table takes its own policies with it, so it goes before the function they reference.
DROP TABLE core.member_projects;
DROP POLICY tenant_admin_membership_delete ON core.memberships;
DROP POLICY tenant_admin_membership_update ON core.memberships;
DROP POLICY tenant_admin_membership_insert ON core.memberships;
DROP POLICY tenant_admin_membership_read ON core.memberships;
DROP FUNCTION core.is_tenant_admin();
DROP POLICY refresh_owner_access ON identity.refresh_tokens;
DROP POLICY sessions_owner_access ON identity.sessions;
DROP POLICY memberships_owner_access ON core.memberships;
REVOKE UPDATE(role) ON core.memberships FROM aether_app;
REVOKE DELETE ON core.memberships FROM aether_app;
DROP INDEX core.memberships_user;
ALTER TABLE core.memberships DROP COLUMN created_at, DROP COLUMN must_change_password;
RESET ROLE;
