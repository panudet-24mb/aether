-- Data-subject rights (PDPA §30-§33): export and erasure of a member's personal data, and of the history a
-- worn tag (a person) left behind.
--
-- A member is erased in place, not deleted: sessions and refresh tokens go, the membership and its project and
-- module access go, flows they enabled that command devices are disarmed (their authority is gone), and the identity
-- keeps its uuid but loses its email, name and password (erased_at set, login refused). Rows that name the member only
-- by uuid (audit trail, read-access log, who acknowledged an alert) stay: they are the security record, and without
-- the identity they are pseudonymous. An identity that also belongs to another workspace is not this workspace's to
-- erase (the platform operator erases it with `admin erase-user`).
--
-- A tag's history is its samples, raw advertisements, presence and events, within the project of the registration
-- being erased (its gateway's project, or the gateways with no project) unless the owner asks for the whole
-- workspace. Events an alert points at are kept, as is the alert (an SOS incident is a safety record), but the alert
-- gets a generic title and the wearer's name goes from both; notification errors, flow-run details, learned-signal
-- labels, stream names and old (removed) registrations of the tag in scope are scrubbed too.
--
-- Every erasure writes core.erasure_log (tenant, subject, scope, counts; no personal data beyond the uuid or tag id).
-- A restore brings erased data back, so `admin export-erasures` / `admin reapply-erasures` carry the ledger across a
-- restore (docs/production.md).
--
-- Functions:
--   identity.erase_member(target)                          owner of the current workspace; checks, then the data part
--   core.erase_identity_history(device, tenant_wide, renamed)   owner of the current workspace; scope from the registration
--   core.erase_member_data(tenant, target, actor)          the data part; no checks, not callable by the runtime
--   core.erase_identity_data(tenant, external, project, tenant_wide)   the data part; no checks, not callable by the runtime
--   core.member_access_log(target, lim)                    a member's own trail entries (self, or an owner)
--   identity.member_sessions(target)                       a member's sessions here (self, or an owner)
--   identity.ack_notice(version)                           the caller acknowledges the privacy notice
-- login_candidate skips erased identities; create_member_identity treats one as taken. Every member erasure takes
-- the workspace's member lock (the advisory lock memberLock takes in Go) before it counts owners.

-- +goose Up
SET ROLE aether_owner;

ALTER TABLE identity.users ADD COLUMN erased_at timestamptz,
 ADD COLUMN notice_ack_version integer NOT NULL DEFAULT 0 CHECK(notice_ack_version BETWEEN 0 AND 1000);
GRANT SELECT(erased_at, notice_ack_version) ON identity.users TO aether_app;

CREATE TABLE core.erasure_log (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL, at timestamptz NOT NULL DEFAULT now(),
 actor_id uuid,
 subject_kind text NOT NULL CHECK(subject_kind IN ('member','device_identity')),
 subject_ref text NOT NULL CHECK(length(subject_ref) BETWEEN 1 AND 128),
 counts jsonb NOT NULL DEFAULT '{}'::jsonb,
 -- device_identity: {"project_id": uuid|null, "tenant_wide": bool, "device_id": uuid, "renamed": bool}, what
 -- reapply-erasures needs to redo it.
 scope jsonb NOT NULL DEFAULT '{}'::jsonb,
 PRIMARY KEY(tenant_id, id)
);
CREATE INDEX erasure_log_recent ON core.erasure_log(tenant_id, at DESC);
ALTER TABLE core.erasure_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.erasure_log FORCE ROW LEVEL SECURITY;
CREATE POLICY erasure_log_owner_read ON core.erasure_log FOR SELECT TO aether_app
 USING(tenant_id = core.tenant_id() AND (SELECT core.is_tenant_owner()));
CREATE POLICY owner_access ON core.erasure_log TO aether_owner USING(true) WITH CHECK(true);
-- Written only by the definer functions below.
GRANT SELECT ON core.erasure_log TO aether_app;

-- The data part of a member erasure. No checks: the callers are identity.erase_member (which checks) and the
-- platform operator's `admin erase-user` / `admin reapply-erasures` (superuser, which check the last owner). It takes
-- the workspace's member lock itself. actor is who erased (NULL: the platform operator), for the disarm audit rows.
-- +goose StatementBegin
CREATE FUNCTION core.erase_member_data(tenant uuid, target uuid, actor uuid) RETURNS jsonb
 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $$
DECLARE tokens bigint; sessions bigint; projects bigint; memberships bigint; identities bigint; flows bigint;
BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended(tenant::text, 4));
 PERFORM set_config('app.tenant_id', tenant::text, true);
 -- Flows the member enabled run with their authority; with the membership gone, a flow that commands devices
 -- must stop (as disarmUnauthorisedFlows does when a member is removed).
 WITH disarmed AS (
  UPDATE core.automations a SET enabled = false, revision = a.revision + 1, updated_at = now()
   WHERE a.tenant_id = tenant AND a.enabled AND a.enabled_by = target
     AND jsonb_typeof(a.definition->'nodes') = 'array'
     AND EXISTS(SELECT 1 FROM jsonb_array_elements(a.definition->'nodes') n WHERE n->>'type' = 'action.command')
  RETURNING a.id),
 state AS (DELETE FROM core.automation_state s WHERE s.tenant_id = tenant AND s.automation_id IN (SELECT id FROM disarmed)),
 triggers AS (DELETE FROM core.automation_triggers t WHERE t.tenant_id = tenant AND t.automation_id IN (SELECT id FROM disarmed)),
 audited AS (INSERT INTO core.audit_logs(id, tenant_id, actor_id, action, target_id)
  SELECT gen_random_uuid(), tenant, coalesce(actor, '00000000-0000-0000-0000-000000000000'::uuid), 'automation.disarmed', id FROM disarmed
  RETURNING 1)
 SELECT count(*) INTO flows FROM audited;
 DELETE FROM identity.refresh_tokens rt USING identity.sessions s
  WHERE rt.session_id = s.id AND rt.user_id = s.user_id AND s.user_id = target AND s.tenant_id = tenant;
 GET DIAGNOSTICS tokens = ROW_COUNT;
 DELETE FROM identity.sessions s WHERE s.user_id = target AND s.tenant_id = tenant;
 GET DIAGNOSTICS sessions = ROW_COUNT;
 DELETE FROM core.member_projects p WHERE p.tenant_id = tenant AND p.user_id = target;
 GET DIAGNOSTICS projects = ROW_COUNT;
 -- core.member_access goes with the membership (ON DELETE CASCADE).
 DELETE FROM core.memberships m WHERE m.tenant_id = tenant AND m.user_id = target;
 GET DIAGNOSTICS memberships = ROW_COUNT;
 -- Only an identity that now belongs to no workspace loses its email, name and password.
 UPDATE identity.users u SET email = 'erased-' || u.id::text || '@erased.invalid', name = 'ผู้ใช้ที่ถูกลบ',
   password_hash = '!erased', erased_at = coalesce(u.erased_at, now())
  WHERE u.id = target AND NOT EXISTS(SELECT 1 FROM core.memberships m WHERE m.user_id = target);
 GET DIAGNOSTICS identities = ROW_COUNT;
 RETURN jsonb_build_object('refresh_tokens', tokens, 'sessions', sessions, 'project_grants', projects,
  'memberships', memberships, 'flows_disarmed', flows, 'identity_anonymised', identities = 1);
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.erase_member_data(uuid, uuid, uuid) FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION identity.erase_member(target uuid) RETURNS TABLE(outcome text, counts jsonb)
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE target_role text; done jsonb;
BEGIN
 IF target IS NULL OR core.tenant_id() IS NULL OR NOT core.is_tenant_owner() THEN
  RETURN QUERY SELECT 'refused'::text, NULL::jsonb;
  RETURN;
 END IF;
 IF target = identity.user_id() THEN
  RETURN QUERY SELECT 'self'::text, NULL::jsonb;
  RETURN;
 END IF;
 -- The workspace's member lock (the one memberLock takes), so the owner count below cannot race a demotion or a
 -- removal; then the identity row, as adoption and password resets lock it.
 PERFORM pg_advisory_xact_lock(hashtextextended(core.tenant_id()::text, 4));
 PERFORM 1 FROM identity.users u WHERE u.id = target FOR UPDATE;
 SELECT m.role INTO target_role FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = target;
 IF target_role IS NULL THEN
  RETURN QUERY SELECT 'not_found'::text, NULL::jsonb;
  RETURN;
 END IF;
 IF (SELECT count(*) FROM core.memberships m WHERE m.user_id = target) <> 1 THEN
  RETURN QUERY SELECT 'shared'::text, NULL::jsonb;
  RETURN;
 END IF;
 IF target_role = 'owner' AND (SELECT count(*) FROM core.memberships m
     WHERE m.tenant_id = core.tenant_id() AND m.role = 'owner') <= 1 THEN
  RETURN QUERY SELECT 'last_owner'::text, NULL::jsonb;
  RETURN;
 END IF;
 done := core.erase_member_data(core.tenant_id(), target, identity.user_id());
 INSERT INTO core.erasure_log(tenant_id, id, actor_id, subject_kind, subject_ref, counts)
  VALUES(core.tenant_id(), gen_random_uuid(), identity.user_id(), 'member', target::text, done);
 RETURN QUERY SELECT 'erased'::text, done;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION identity.erase_member(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.erase_member(uuid) TO aether_app;

-- The data part of a tag-history erasure, across every partition (through the parents), limited to the gateways of
-- one project (project NULL: the gateways with no project) unless tenant_wide. The tenant is set for the transaction
-- so the tenant policies, which apply to the owner role as well, admit exactly this workspace.
--
-- Lock order: the in-scope gateway rows first, FOR UPDATE in id order. That is the first lock every ingest path takes
-- (its gateway row FOR UPDATE), so ingest for those gateways waits for the whole erasure instead of meeting it row by
-- row in the history tables (a deadlock). Ingest through gateways out of scope touches none of the rows below except
-- the tag's presence row, and holds nothing this function waits for.
-- +goose StatementBegin
CREATE FUNCTION core.erase_identity_data(tenant uuid, external text, project uuid, tenant_wide boolean) RETURNS jsonb
 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $$
DECLARE samples bigint; ble bigint; presence bigint; events bigint; kept_events bigint; alerts bigint;
 notices bigint; runs bigint; learned bigint; sessions bigint; streams bigint; old_names bigint;
 gw uuid[];
 erased_name constant text := 'ผู้สวมใส่ (ลบข้อมูลแล้ว)';
BEGIN
 PERFORM set_config('app.tenant_id', tenant::text, true);
 PERFORM 1 FROM core.gateways g
  WHERE g.tenant_id = tenant AND (coalesce(tenant_wide, false) OR g.project_id IS NOT DISTINCT FROM project)
  ORDER BY g.id FOR UPDATE;
 SELECT coalesce(array_agg(g.id ORDER BY g.id), '{}') INTO gw FROM core.gateways g
  WHERE g.tenant_id = tenant AND (coalesce(tenant_wide, false) OR g.project_id IS NOT DISTINCT FROM project);
 DELETE FROM core.sensor_samples s WHERE s.tenant_id = tenant AND s.external_id = external AND s.gateway_id = ANY(gw);
 GET DIAGNOSTICS samples = ROW_COUNT;
 DELETE FROM core.ble_history b WHERE b.tenant_id = tenant AND b.external_id = external AND b.gateway_id = ANY(gw);
 GET DIAGNOSTICS ble = ROW_COUNT;
 -- One row per tag: it goes when its zone, or the zone it is about to move to, is in scope (or it has no zone).
 DELETE FROM core.presence_state p WHERE p.tenant_id = tenant AND p.external_id = external
   AND (p.gateway_id IS NULL OR p.gateway_id = ANY(gw) OR p.candidate_gateway_id = ANY(gw));
 GET DIAGNOSTICS presence = ROW_COUNT;
 -- Alerts are incident records: kept, under a generic title that names only the kind of event.
 UPDATE core.alerts a SET device_name = erased_name,
   title = CASE a.event_type WHEN 'button' THEN 'กดปุ่มฉุกเฉิน SOS' WHEN 'tamper' THEN 'ป้ายถูกถอด (tamper)' WHEN 'leak' THEN 'พบน้ำรั่ว'
     WHEN 'motion' THEN 'เริ่มเคลื่อนไหว' WHEN 'offline' THEN 'ขาดการติดต่อ' WHEN 'threshold' THEN 'ค่าเกินเกณฑ์'
     WHEN 'zone' THEN 'wearable เข้าโซน' ELSE 'การแจ้งเตือน' END || ' · ' || erased_name
  WHERE a.tenant_id = tenant AND a.external_id = external AND a.gateway_id = ANY(gw);
 GET DIAGNOSTICS alerts = ROW_COUNT;
 -- A delivery error can quote what the channel echoed back (the title, the name).
 UPDATE core.notifications n SET last_error = NULL
  WHERE n.tenant_id = tenant AND n.last_error IS NOT NULL
    AND n.alert_id IN (SELECT a.id FROM core.alerts a WHERE a.tenant_id = tenant AND a.external_id = external AND a.gateway_id = ANY(gw));
 GET DIAGNOSTICS notices = ROW_COUNT;
 UPDATE core.device_events e SET device_name = erased_name, detail = '{}'::jsonb
  WHERE e.tenant_id = tenant AND e.external_id = external AND e.gateway_id = ANY(gw)
    AND EXISTS(SELECT 1 FROM core.alerts a WHERE a.tenant_id = e.tenant_id AND a.event_id = e.id);
 GET DIAGNOSTICS kept_events = ROW_COUNT;
 DELETE FROM core.device_events e WHERE e.tenant_id = tenant AND e.external_id = external AND e.gateway_id = ANY(gw)
    AND NOT EXISTS(SELECT 1 FROM core.alerts a WHERE a.tenant_id = e.tenant_id AND a.event_id = e.id);
 GET DIAGNOSTICS events = ROW_COUNT;
 -- Flow runs the tag triggered: kept (the flow's history) without the tag or what it reported.
 UPDATE core.automation_runs r SET external_id = '', detail = '{}'::jsonb
  WHERE r.tenant_id = tenant AND r.external_id = external AND (r.gateway_id IS NULL OR r.gateway_id = ANY(gw));
 GET DIAGNOSTICS runs = ROW_COUNT;
 -- Learned signals and teaching sessions keep working; only their free-text labels go. Learned signals belong to
 -- the tag, not to a gateway, so they are scrubbed whatever the scope (a label, no data).
 UPDATE core.device_signals d SET description = '' WHERE d.tenant_id = tenant AND d.external_id = external AND d.description <> '';
 GET DIAGNOSTICS learned = ROW_COUNT;
 UPDATE core.signal_sessions s SET label = '' WHERE s.tenant_id = tenant AND s.external_id = external AND s.gateway_id = ANY(gw) AND s.label <> '';
 GET DIAGNOSTICS sessions = ROW_COUNT;
 UPDATE core.sensor_streams st SET name = upper(external)
  WHERE st.tenant_id = tenant AND st.external_id = external AND st.gateway_id = ANY(gw) AND st.name <> upper(external);
 GET DIAGNOSTICS streams = ROW_COUNT;
 -- Earlier (removed) registrations of the tag carry the names of earlier wearers. The live one is renamed by the
 -- caller when the owner gives a new name.
 UPDATE core.devices d SET name = erased_name
  WHERE d.tenant_id = tenant AND d.external_id = external AND d.removed_at IS NOT NULL AND d.gateway_id = ANY(gw) AND d.name <> erased_name;
 GET DIAGNOSTICS old_names = ROW_COUNT;
 RETURN jsonb_build_object('samples', samples, 'ble_history', ble, 'presence', presence, 'events', events,
  'events_anonymised', kept_events, 'alerts_anonymised', alerts, 'notification_errors_cleared', notices,
  'flow_runs_anonymised', runs, 'signal_labels_cleared', learned + sessions, 'stream_names_cleared', streams,
  'removed_registrations_renamed', old_names, 'gateways_in_scope', cardinality(gw));
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.erase_identity_data(uuid, text, uuid, boolean) FROM PUBLIC;

-- The owner's erasure of a registration's tag history. The scope comes from the registration (its gateway's
-- project), never from the caller; tenant_wide widens it to every gateway of the workspace. renamed records that the
-- caller renames the registration in the same transaction, so a restore can put a placeholder name back.
-- +goose StatementBegin
CREATE FUNCTION core.erase_identity_history(device uuid, tenant_wide boolean, renamed boolean) RETURNS TABLE(outcome text, counts jsonb)
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE done jsonb; external text; project uuid; scope jsonb;
BEGIN
 IF device IS NULL OR core.tenant_id() IS NULL OR NOT core.is_tenant_owner() THEN
  RETURN QUERY SELECT 'refused'::text, NULL::jsonb;
  RETURN;
 END IF;
 SELECT d.external_id, g.project_id INTO external, project FROM core.devices d
  JOIN core.gateways g ON g.tenant_id = d.tenant_id AND g.id = d.gateway_id
  WHERE d.tenant_id = core.tenant_id() AND d.id = device;
 IF external IS NULL THEN
  RETURN QUERY SELECT 'not_found'::text, NULL::jsonb;
  RETURN;
 END IF;
 scope := jsonb_build_object('project_id', project, 'tenant_wide', coalesce(tenant_wide, false),
  'device_id', device, 'renamed', coalesce(renamed, false));
 done := core.erase_identity_data(core.tenant_id(), external, project, coalesce(tenant_wide, false));
 INSERT INTO core.erasure_log(tenant_id, id, actor_id, subject_kind, subject_ref, counts, scope)
  VALUES(core.tenant_id(), gen_random_uuid(), identity.user_id(), 'device_identity', external, done, scope);
 RETURN QUERY SELECT 'erased'::text, done;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.erase_identity_history(uuid, boolean, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.erase_identity_history(uuid, boolean, boolean) TO aether_app;

-- A member's entries in the read-access log of the current workspace: what they read and what others read
-- about them. For the member themself (their export) or an owner (the export they run for a member).
-- +goose StatementBegin
CREATE FUNCTION core.member_access_log(target uuid, lim integer)
 RETURNS TABLE(at timestamptz, actor_id uuid, resource text, subject_kind text, subject_id text, client_ip inet)
 LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
 IF target IS NULL OR core.tenant_id() IS NULL OR NOT (target = identity.user_id() OR core.is_tenant_owner()) THEN
  RETURN;
 END IF;
 -- Somebody else's IP address is their personal data, not this member's: only the member's own reads carry one.
 RETURN QUERY SELECT a.at, a.actor_id, a.resource, a.subject_kind, a.subject_id,
   CASE WHEN a.actor_id = target THEN a.client_ip END FROM core.access_log a
  WHERE a.tenant_id = core.tenant_id() AND (a.actor_id = target OR (a.subject_kind = 'member' AND a.subject_id = target::text))
  ORDER BY a.at DESC LIMIT least(greatest(coalesce(lim, 1000), 1), 10000);
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.member_access_log(uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.member_access_log(uuid, integer) TO aether_app;

-- A member's sessions in the current workspace (no token material), for the same two callers.
-- +goose StatementBegin
CREATE FUNCTION identity.member_sessions(target uuid)
 RETURNS TABLE(id uuid, created_at timestamptz, expires_at timestamptz, revoked_at timestamptz)
 LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
 IF target IS NULL OR core.tenant_id() IS NULL OR NOT (target = identity.user_id() OR core.is_tenant_owner()) THEN
  RETURN;
 END IF;
 RETURN QUERY SELECT s.id, s.created_at, s.expires_at, s.revoked_at FROM identity.sessions s
  WHERE s.user_id = target AND s.tenant_id = core.tenant_id() ORDER BY s.created_at DESC LIMIT 1000;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION identity.member_sessions(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.member_sessions(uuid) TO aether_app;

-- The runtime has no UPDATE on identity.users (CheckRuntimeRole refuses one); acknowledging the notice is this.
-- +goose StatementBegin
CREATE FUNCTION identity.ack_notice(version integer) RETURNS integer
 LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 UPDATE identity.users u SET notice_ack_version = greatest(u.notice_ack_version, least(greatest(version, 0), 1000))
  WHERE u.id = identity.user_id() AND u.erased_at IS NULL
 RETURNING u.notice_ack_version
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION identity.ack_notice(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.ack_notice(integer) TO aether_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.login_candidate(address text)
 RETURNS TABLE(id uuid, email text, name text, password_hash text)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT u.id, u.email, u.name, u.password_hash FROM identity.users u WHERE u.email = address AND u.erased_at IS NULL
$$;
-- +goose StatementEnd

-- Same as 00035, plus: an erased identity is never adopted (its placeholder address could be typed in).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.create_member_identity(new_id uuid, address text, display text, hash text, member_role text)
 RETURNS TABLE(member_id uuid, outcome text)
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE actor text; target uuid; target_erased timestamptz; result text; written integer;
BEGIN
 SELECT m.role INTO actor FROM core.memberships m
  WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id();
 IF actor IS NULL OR actor NOT IN ('owner','admin')
    OR new_id IS NULL OR address IS NULL OR display IS NULL OR hash IS NULL
    OR member_role IS NULL OR member_role NOT IN ('owner','admin','operator','viewer')
    OR (member_role = 'owner' AND actor <> 'owner') THEN
  RETURN QUERY SELECT NULL::uuid, 'refused'::text;
  RETURN;
 END IF;
 SELECT u.id, u.erased_at INTO target, target_erased FROM identity.users u WHERE u.email = address FOR UPDATE;
 IF target IS NULL THEN
  INSERT INTO identity.users(id, email, name, password_hash) VALUES(new_id, address, display, hash)
   ON CONFLICT (email) DO NOTHING;
  GET DIAGNOSTICS written = ROW_COUNT;
  IF written = 0 THEN
   RETURN QUERY SELECT NULL::uuid, 'taken'::text;
   RETURN;
  END IF;
  target := new_id;
  result := 'created';
 ELSIF target = identity.user_id() THEN
  RETURN QUERY SELECT target, 'self'::text;
  RETURN;
 ELSIF target_erased IS NOT NULL OR EXISTS(SELECT 1 FROM core.memberships m WHERE m.user_id = target) THEN
  RETURN QUERY SELECT NULL::uuid, 'taken'::text;
  RETURN;
 ELSE
  UPDATE identity.users SET name = display, password_hash = hash WHERE id = target;
  result := 'adopted';
 END IF;
 INSERT INTO core.memberships(tenant_id, user_id, role, must_change_password)
  VALUES(core.tenant_id(), target, member_role, true);
 RETURN QUERY SELECT target, result;
END $$;
-- +goose StatementEnd
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
-- The 00035 body plus one line: an erased identity (its placeholder address, which survives this Down in the email
-- column while erased_at does not) is never adopted, so a rollback cannot hand an erased person's uuid, and the audit
-- trail behind it, to somebody new. Deliberately not byte-identical to 00035. The login function needs no such line:
-- an erased identity's password hash ('!erased') never verifies.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.create_member_identity(new_id uuid, address text, display text, hash text, member_role text)
 RETURNS TABLE(member_id uuid, outcome text)
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE actor text; target uuid; result text; written integer;
BEGIN
 SELECT m.role INTO actor FROM core.memberships m
  WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id();
 IF actor IS NULL OR actor NOT IN ('owner','admin')
    OR new_id IS NULL OR address IS NULL OR display IS NULL OR hash IS NULL
    OR member_role IS NULL OR member_role NOT IN ('owner','admin','operator','viewer')
    OR (member_role = 'owner' AND actor <> 'owner') THEN
  RETURN QUERY SELECT NULL::uuid, 'refused'::text;
  RETURN;
 END IF;
 IF address LIKE '%@erased.invalid' THEN
  RETURN QUERY SELECT NULL::uuid, 'taken'::text;
  RETURN;
 END IF;
 SELECT u.id INTO target FROM identity.users u WHERE u.email = address FOR UPDATE;
 IF target IS NULL THEN
  INSERT INTO identity.users(id, email, name, password_hash) VALUES(new_id, address, display, hash)
   ON CONFLICT (email) DO NOTHING;
  GET DIAGNOSTICS written = ROW_COUNT;
  IF written = 0 THEN
   RETURN QUERY SELECT NULL::uuid, 'taken'::text;
   RETURN;
  END IF;
  target := new_id;
  result := 'created';
 ELSIF target = identity.user_id() THEN
  RETURN QUERY SELECT target, 'self'::text;
  RETURN;
 ELSIF EXISTS(SELECT 1 FROM core.memberships m WHERE m.user_id = target) THEN
  RETURN QUERY SELECT NULL::uuid, 'taken'::text;
  RETURN;
 ELSE
  UPDATE identity.users SET name = display, password_hash = hash WHERE id = target;
  result := 'adopted';
 END IF;
 INSERT INTO core.memberships(tenant_id, user_id, role, must_change_password)
  VALUES(core.tenant_id(), target, member_role, true);
 RETURN QUERY SELECT target, result;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.login_candidate(address text)
 RETURNS TABLE(id uuid, email text, name text, password_hash text)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT u.id, u.email, u.name, u.password_hash FROM identity.users u WHERE u.email = address
$$;
-- +goose StatementEnd
DROP FUNCTION identity.ack_notice(integer);
DROP FUNCTION identity.member_sessions(uuid);
DROP FUNCTION core.member_access_log(uuid, integer);
DROP FUNCTION core.erase_identity_history(uuid, boolean, boolean);
DROP FUNCTION core.erase_identity_data(uuid, text, uuid, boolean);
DROP FUNCTION identity.erase_member(uuid);
DROP FUNCTION core.erase_member_data(uuid, uuid, uuid);
DROP TABLE core.erasure_log;
REVOKE SELECT(erased_at, notice_ack_version) ON identity.users FROM aether_app;
ALTER TABLE identity.users DROP COLUMN notice_ack_version, DROP COLUMN erased_at;
RESET ROLE;
