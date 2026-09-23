-- +goose Up
SET ROLE aether_owner;

-- Security review follow-up to migration 00019. Three things:
--  H1  core.presence_state leaked the remembered zone: the row is keyed by BLE identity, but its
--      gateway_id / candidate_gateway_id could point at a project the member may not see, and
--      ListDevices surfaces it as zone_gateway_id.
--  M1  identity.set_member_password() and identity.purge_tenant_sessions() never looked at the TARGET's
--      role, so only the Go layer stopped an admin from resetting or logging out an owner.
--  M3  core.scope_all() was evaluated per row inside every policy qual. Wrapping it in a scalar
--      sub-select makes it an InitPlan: one evaluation per statement, so the ingest path and owner
--      sessions pay a single boolean instead of one function call per row.
-- Every policy below is rewritten explicitly (no dynamic SQL over pg_policies) so the diff is reviewable.

-- ----------------------------------------------------------------------------
-- H2. Linking an identity that already belongs to another workspace must stop.
-- ----------------------------------------------------------------------------
-- Login auto-selects a workspace when an identity has exactly one membership, so silently adding a
-- second membership to a stranger's account changes where their next login lands. An owner/admin may
-- therefore only attach an identity that belongs to no workspace at all (one that was removed earlier);
-- anything else is refused with the same generic conflict every other duplicate produces.
-- The count is over every tenant, which is why the caller must be an admin of the current one; the
-- number never leaves the server, the HTTP answer is an undifferentiated 409.
CREATE FUNCTION core.identity_membership_count(target uuid) RETURNS integer
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.is_tenant_admin() AND target IS NOT NULL
   THEN (SELECT count(*)::integer FROM core.memberships m WHERE m.user_id = target) ELSE -1 END
$$;
REVOKE ALL ON FUNCTION core.identity_membership_count(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.identity_membership_count(uuid) TO aether_app;
-- Superseded by the function above: it required the target to already be a member of the current tenant,
-- which cannot answer "does this identity exist anywhere else?".
DROP FUNCTION core.member_tenant_count(uuid);

-- ----------------------------------------------------------------------------
-- M1. The target's role is now part of the SQL contract, not only of the Go path.
-- ----------------------------------------------------------------------------
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.set_member_password(target uuid, hash text) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
 IF NOT core.is_tenant_admin() OR target IS NULL OR target = identity.user_id() OR hash IS NULL THEN
  RETURN false;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = target) THEN
  RETURN false;
 END IF;
 -- An owner's credentials are an owner's business: only another owner may replace them.
 IF EXISTS(SELECT 1 FROM core.memberships m
      WHERE m.tenant_id = core.tenant_id() AND m.user_id = target AND m.role = 'owner')
    AND NOT EXISTS(SELECT 1 FROM core.memberships m
      WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id() AND m.role = 'owner') THEN
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.purge_tenant_sessions(target uuid) RETURNS integer
 LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE gone integer;
BEGIN
 IF NOT core.is_tenant_admin() OR target IS NULL OR target = identity.user_id() THEN
  RETURN 0;
 END IF;
 -- Logging an owner out is an owner's decision, same rule as replacing their password.
 IF EXISTS(SELECT 1 FROM core.memberships m
      WHERE m.tenant_id = core.tenant_id() AND m.user_id = target AND m.role = 'owner')
    AND NOT EXISTS(SELECT 1 FROM core.memberships m
      WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id() AND m.role = 'owner') THEN
  RETURN 0;
 END IF;
 DELETE FROM identity.refresh_tokens rt USING identity.sessions s
  WHERE rt.session_id = s.id AND s.user_id = target AND s.tenant_id = core.tenant_id();
 DELETE FROM identity.sessions s WHERE s.user_id = target AND s.tenant_id = core.tenant_id();
 GET DIAGNOSTICS gone = ROW_COUNT;
 RETURN gone;
END $$;
-- +goose StatementEnd

-- ----------------------------------------------------------------------------
-- M3 + H1. Every project_scope policy, rewritten with the constant hoisted.
-- ----------------------------------------------------------------------------
-- `(SELECT core.scope_all())` is an uncorrelated scalar sub-query, so the planner turns it into an
-- InitPlan evaluated once per statement instead of a function call per row. Expected shape:
--   EXPLAIN SELECT count(*) FROM core.sensor_samples;
--     Aggregate
--       InitPlan 1 (returns $0)
--         -> Result                                     -- core.scope_all(), evaluated once
--       -> Seq Scan on sensor_samples
--            Filter: ((tenant_id = core.tenant_id()) AND ($0 OR core.gateway_in_scope(gateway_id)))
-- With scope '*' (ingest, owner) $0 is true and the per-row half of the OR is never reached.
-- Fail-closed is unchanged: an unset app.project_scope makes scope_all() false, and the second half of
-- every OR then matches no row (project_in_scope compares against an empty list, and the EXISTS probes
-- run under these same policies).

-- Keyed by gateway.
DROP POLICY project_scope ON core.devices;
CREATE POLICY project_scope ON core.devices AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
DROP POLICY project_scope ON core.gateway_packets;
CREATE POLICY project_scope ON core.gateway_packets AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
DROP POLICY project_scope ON core.sensor_streams;
CREATE POLICY project_scope ON core.sensor_streams AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
DROP POLICY project_scope ON core.sensor_samples;
CREATE POLICY project_scope ON core.sensor_samples AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
DROP POLICY project_scope ON core.stream_state;
CREATE POLICY project_scope ON core.stream_state AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
DROP POLICY project_scope ON core.device_events;
CREATE POLICY project_scope ON core.device_events AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
DROP POLICY project_scope ON core.alerts;
CREATE POLICY project_scope ON core.alerts AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
DROP POLICY project_scope ON core.ble_history;
CREATE POLICY project_scope ON core.ble_history AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
DROP POLICY project_scope ON core.mqtt_accounts;
CREATE POLICY project_scope ON core.mqtt_accounts AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));

-- Keyed by device (which follows its gateway).
DROP POLICY project_scope ON core.telemetry;
CREATE POLICY project_scope ON core.telemetry AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.device_in_scope(device_id))
 WITH CHECK((SELECT core.scope_all()) OR core.device_in_scope(device_id));
DROP POLICY project_scope ON core.device_state;
CREATE POLICY project_scope ON core.device_state AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.device_in_scope(device_id))
 WITH CHECK((SELECT core.scope_all()) OR core.device_in_scope(device_id));

-- Keyed by project directly. A NULL project stays hidden from a restricted member.
DROP POLICY project_scope ON core.gateways;
CREATE POLICY project_scope ON core.gateways AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.project_in_scope(project_id))
 WITH CHECK((SELECT core.scope_all()) OR core.project_in_scope(project_id));
DROP POLICY project_scope ON core.sites;
CREATE POLICY project_scope ON core.sites AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.project_in_scope(project_id))
 WITH CHECK((SELECT core.scope_all()) OR core.project_in_scope(project_id));
DROP POLICY project_scope ON core.automations;
CREATE POLICY project_scope ON core.automations AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.project_in_scope(project_id))
 WITH CHECK((SELECT core.scope_all()) OR core.project_in_scope(project_id));
DROP POLICY project_scope ON core.projects;
CREATE POLICY project_scope ON core.projects AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.project_in_scope(id))
 WITH CHECK((SELECT core.scope_all()) OR core.project_in_scope(id));

-- Floor plans: site -> project, then floor -> site.
DROP POLICY project_scope ON core.floors;
CREATE POLICY project_scope ON core.floors AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.site_in_scope(site_id))
 WITH CHECK((SELECT core.scope_all()) OR core.site_in_scope(site_id));
DROP POLICY project_scope ON core.floor_placements;
CREATE POLICY project_scope ON core.floor_placements AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.floor_in_scope(floor_id))
 WITH CHECK((SELECT core.scope_all()) OR core.floor_in_scope(floor_id));
DROP POLICY project_scope ON core.floor_images;
CREATE POLICY project_scope ON core.floor_images AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.floor_in_scope(floor_id))
 WITH CHECK((SELECT core.scope_all()) OR core.floor_in_scope(floor_id));

-- Automation Studio: state, runs and the trigger index follow their flow.
DROP POLICY project_scope ON core.automation_state;
CREATE POLICY project_scope ON core.automation_state AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.automation_in_scope(automation_id))
 WITH CHECK((SELECT core.scope_all()) OR core.automation_in_scope(automation_id));
DROP POLICY project_scope ON core.automation_runs;
CREATE POLICY project_scope ON core.automation_runs AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.automation_in_scope(automation_id))
 WITH CHECK((SELECT core.scope_all()) OR core.automation_in_scope(automation_id));
DROP POLICY IF EXISTS project_scope ON core.automation_triggers; -- databases that ran 00020 before the policy was added to it
CREATE POLICY project_scope ON core.automation_triggers AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.automation_in_scope(automation_id))
 WITH CHECK((SELECT core.scope_all()) OR core.automation_in_scope(automation_id));

-- Asset registry (polymorphic: a gateway id or a device id).
DROP POLICY project_scope ON core.asset_records;
CREATE POLICY project_scope ON core.asset_records AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.asset_in_scope(asset_kind, asset_id))
 WITH CHECK((SELECT core.scope_all()) OR core.asset_in_scope(asset_kind, asset_id));
DROP POLICY project_scope ON core.maintenance_plans;
CREATE POLICY project_scope ON core.maintenance_plans AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.asset_in_scope(asset_kind, asset_id))
 WITH CHECK((SELECT core.scope_all()) OR core.asset_in_scope(asset_kind, asset_id));
DROP POLICY project_scope ON core.maintenance_logs;
CREATE POLICY project_scope ON core.maintenance_logs AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.asset_in_scope(asset_kind, asset_id))
 WITH CHECK((SELECT core.scope_all()) OR core.asset_in_scope(asset_kind, asset_id));

-- Notifications follow their alert, which follows its gateway.
DROP POLICY project_scope ON core.notifications;
CREATE POLICY project_scope ON core.notifications AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.alert_in_scope(alert_id))
 WITH CHECK((SELECT core.scope_all()) OR core.alert_in_scope(alert_id));

-- H1: presence is keyed by BLE identity, but the row also NAMES gateways (the settled zone and the
-- candidate zone), and core.devices exposes the settled one as zone_gateway_id. Being allowed to know
-- that a tag exists is not being allowed to know it is standing in another project's building, so the
-- row is visible only when every gateway it names is in scope as well.
-- WITH CHECK stays on the identity alone: the writer is ingest, which runs at scope '*', and a
-- restricted member must never be able to write presence at all (no API route does).
DROP POLICY project_scope ON core.presence_state;
CREATE POLICY project_scope ON core.presence_state AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR (core.identity_in_scope(external_id)
   AND (gateway_id IS NULL OR core.gateway_in_scope(gateway_id))
   AND (candidate_gateway_id IS NULL OR core.gateway_in_scope(candidate_gateway_id))))
 WITH CHECK((SELECT core.scope_all()) OR core.identity_in_scope(external_id));
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
-- Restore the migration 00019 / 00020 form of every policy (constant not hoisted, presence keyed on the
-- identity alone) and the narrower membership count function.
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['devices','gateway_packets','sensor_streams','sensor_samples','stream_state','device_events','alerts','ble_history','mqtt_accounts'] LOOP
  EXECUTE format('DROP POLICY project_scope ON core.%I',t);
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.gateway_in_scope(gateway_id)) WITH CHECK(core.gateway_in_scope(gateway_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['telemetry','device_state'] LOOP
  EXECUTE format('DROP POLICY project_scope ON core.%I',t);
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.device_in_scope(device_id)) WITH CHECK(core.device_in_scope(device_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['gateways','sites','automations'] LOOP
  EXECUTE format('DROP POLICY project_scope ON core.%I',t);
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.project_in_scope(project_id)) WITH CHECK(core.project_in_scope(project_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['floor_placements','floor_images'] LOOP
  EXECUTE format('DROP POLICY project_scope ON core.%I',t);
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.floor_in_scope(floor_id)) WITH CHECK(core.floor_in_scope(floor_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['automation_state','automation_runs','automation_triggers'] LOOP
  EXECUTE format('DROP POLICY project_scope ON core.%I',t);
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.automation_in_scope(automation_id)) WITH CHECK(core.automation_in_scope(automation_id))',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['asset_records','maintenance_plans','maintenance_logs'] LOOP
  EXECUTE format('DROP POLICY project_scope ON core.%I',t);
  EXECUTE format('CREATE POLICY project_scope ON core.%I AS RESTRICTIVE TO aether_app USING(core.asset_in_scope(asset_kind,asset_id)) WITH CHECK(core.asset_in_scope(asset_kind,asset_id))',t);
 END LOOP;
END $$;
-- +goose StatementEnd
DROP POLICY project_scope ON core.projects;
CREATE POLICY project_scope ON core.projects AS RESTRICTIVE TO aether_app
 USING(core.project_in_scope(id)) WITH CHECK(core.project_in_scope(id));
DROP POLICY project_scope ON core.floors;
CREATE POLICY project_scope ON core.floors AS RESTRICTIVE TO aether_app
 USING(core.site_in_scope(site_id)) WITH CHECK(core.site_in_scope(site_id));
DROP POLICY project_scope ON core.notifications;
CREATE POLICY project_scope ON core.notifications AS RESTRICTIVE TO aether_app
 USING(core.alert_in_scope(alert_id)) WITH CHECK(core.alert_in_scope(alert_id));
DROP POLICY project_scope ON core.presence_state;
CREATE POLICY project_scope ON core.presence_state AS RESTRICTIVE TO aether_app
 USING(core.identity_in_scope(external_id)) WITH CHECK(core.identity_in_scope(external_id));
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.purge_tenant_sessions(target uuid) RETURNS integer
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.set_member_password(target uuid, hash text) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
 IF NOT core.is_tenant_admin() OR target IS NULL OR target = identity.user_id() OR hash IS NULL THEN
  RETURN false;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = target) THEN
  RETURN false;
 END IF;
 IF (SELECT count(*) FROM core.memberships m WHERE m.user_id = target) <> 1 THEN
  RETURN false;
 END IF;
 UPDATE identity.users SET password_hash = hash WHERE id = target;
 UPDATE core.memberships SET must_change_password = true
  WHERE tenant_id = core.tenant_id() AND user_id = target;
 RETURN true;
END $$;
-- +goose StatementEnd
CREATE FUNCTION core.member_tenant_count(target uuid) RETURNS integer
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.is_tenant_admin()
   AND EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = target)
   THEN (SELECT count(*)::integer FROM core.memberships m WHERE m.user_id = target) ELSE 0 END
$$;
REVOKE ALL ON FUNCTION core.member_tenant_count(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.member_tenant_count(uuid) TO aether_app;
DROP FUNCTION core.identity_membership_count(uuid);
RESET ROLE;
