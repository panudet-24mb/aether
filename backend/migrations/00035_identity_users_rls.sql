-- +goose Up
SET ROLE aether_owner;

-- identity.users was the last table the runtime could read in full: any SQL path of aether_app could run
-- SELECT email, password_hash FROM identity.users and walk every account on the platform. It now gets
-- row level security plus a column grant, and the paths RLS cannot serve move to narrow SECURITY DEFINER
-- functions:
--   login (no identity yet)             identity.login_candidate(email)
--   one-time bootstrap (global count)   identity.any_user_exists()
--   change own password (reads a hash)  identity.own_password_hash()
--   add member (create or adopt)        identity.create_member_identity(...)
-- Everything else (member list, "who am I", read-back after insert) keeps its plain join and is served by
-- the policies below.
--
-- What this does NOT do: login_candidate still hands a hash to any aether_app session that knows an
-- email. The gain is that a query bug (a missing WHERE, a SELECT *) can no longer leak hashes; a
-- dedicated login role on its own pool is the planned follow-up.

ALTER TABLE identity.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.users FORCE ROW LEVEL SECURITY;

-- The definer functions of 00019/00022 (set_member_password, change_own_password) and the ones below run
-- as this NOLOGIN owner; FORCE RLS covers it too, so it needs its own policy. Same pattern as 00019.
CREATE POLICY users_owner_access ON identity.users FOR ALL TO aether_owner USING(true) WITH CHECK(true);

-- Everybody reads their own identity.
CREATE POLICY users_self_read ON identity.users FOR SELECT TO aether_app
 USING(id = identity.user_id());
-- An owner/admin reads the identities that are members of the current workspace, and nothing else. The
-- membership sub-select is itself under the memberships RLS (current tenant, admin only), and the admin
-- check is a scalar sub-select so it is an InitPlan: once per statement, not once per row.
CREATE POLICY users_tenant_admin_read ON identity.users FOR SELECT TO aether_app
 USING((SELECT core.is_tenant_admin()) AND EXISTS(SELECT 1 FROM core.memberships m
   WHERE m.user_id = users.id AND m.tenant_id = core.tenant_id()));
-- The runtime inserts only its own identity: registration and bootstrap run the transaction as the new
-- id. Members are created by identity.create_member_identity, never by a runtime INSERT.
CREATE POLICY users_insert ON identity.users FOR INSERT TO aether_app
 WITH CHECK(id = identity.user_id());

-- The runtime never reads a password hash directly again; login and change-password use the functions.
-- postgres.Open() refuses to start if the role can still SELECT password_hash.
REVOKE SELECT ON identity.users FROM aether_app;
GRANT SELECT(id, email, name, created_at) ON identity.users TO aether_app;

-- Login: the email -> hash lookup that has to work before any identity is known. One exact-email row.
CREATE FUNCTION identity.login_candidate(address text)
 RETURNS TABLE(id uuid, email text, name text, password_hash text)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT u.id, u.email, u.name, u.password_hash FROM identity.users u WHERE u.email = address
$$;
REVOKE ALL ON FUNCTION identity.login_candidate(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.login_candidate(text) TO aether_app;

-- One-time bootstrap: "does any account exist at all?" answered with one boolean.
CREATE FUNCTION identity.any_user_exists() RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT EXISTS(SELECT 1 FROM identity.users)
$$;
REVOKE ALL ON FUNCTION identity.any_user_exists() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.any_user_exists() TO aether_app;

-- The caller's own hash, for verifying the current password before it is changed. NULL without identity.
CREATE FUNCTION identity.own_password_hash() RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT u.password_hash FROM identity.users u WHERE u.id = identity.user_id()
$$;
REVOKE ALL ON FUNCTION identity.own_password_hash() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.own_password_hash() TO aether_app;

-- Adding a member, atomically: the identity (new or adopted) and the membership in the current workspace.
-- Outcomes:
--   created   no identity had this email; one was created with the supplied name and hash
--   adopted   an identity with no membership anywhere (somebody removed earlier) was re-issued: the
--             supplied name and hash replace the stored ones, so nothing of the old account shows through
--   self      the caller's own email (the Go layer answers 403)
--   taken     the identity belongs to some workspace (this one included); nothing is written, no id is
--             returned, and the HTTP answer is the same undifferentiated 409 a duplicate gets
--   refused   the caller is not an owner/admin here, or may not grant that role (only an owner adds owners)
-- The identity row is locked FOR UPDATE before the membership count, and the function is VOLATILE so the
-- count after a lock wait sees the other transaction's committed membership: two workspaces adopting the
-- same orphan at once serialise, and the second one gets "taken". A brand-new email has no row to lock;
-- two concurrent creations meet on the unique email index instead (ON CONFLICT DO NOTHING -> "taken").
-- +goose StatementBegin
CREATE FUNCTION identity.create_member_identity(new_id uuid, address text, display text, hash text, member_role text)
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
REVOKE ALL ON FUNCTION identity.create_member_identity(uuid,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.create_member_identity(uuid,text,text,text,text) TO aether_app;

-- set_member_password (00022) counted the target's memberships without a lock, so a concurrent adoption
-- in another workspace could slip between the count and the write. Same body, plus the identity row lock
-- create_member_identity takes, and pg_temp pinned last.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION identity.set_member_password(target uuid, hash text) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
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
 -- Serialise with any adoption of this identity before counting where it belongs.
 PERFORM 1 FROM identity.users u WHERE u.id = target FOR UPDATE;
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

-- identity_membership_count (00022) answered for any identity id an admin supplied. It is only needed for
-- members of the current workspace (the password reset), so anything else now gets -1 like a non-admin.
CREATE OR REPLACE FUNCTION core.identity_membership_count(target uuid) RETURNS integer
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT CASE WHEN core.is_tenant_admin() AND target IS NOT NULL
   AND EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = target)
   THEN (SELECT count(*)::integer FROM core.memberships m WHERE m.user_id = target) ELSE -1 END
$$;

-- Same pattern as core.is_tenant_admin (00019): a definer so the membership policies below can ask about
-- the caller's own role without recursing into themselves.
CREATE FUNCTION core.is_tenant_owner() RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT EXISTS(SELECT 1 FROM core.memberships m
   WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id() AND m.role = 'owner')
$$;
REVOKE ALL ON FUNCTION core.is_tenant_owner() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.is_tenant_owner() TO aether_app;

-- A workspace nobody belongs to yet: the one CreateAccount has just inserted in this transaction.
CREATE FUNCTION core.tenant_unclaimed() RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT core.tenant_id() IS NOT NULL
   AND NOT EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id())
$$;
REVOKE ALL ON FUNCTION core.tenant_unclaimed() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.tenant_unclaimed() TO aether_app;

-- The membership policies now say what the Go layer (members.go) enforces, so a runtime statement that
-- skips the repository cannot do more than the API:
--  * INSERT: members are added only by identity.create_member_identity (definer). The runtime keeps one
--    insert, own_membership_insert, for CreateAccount's first owner, and only into a workspace that has
--    no member yet (00001 let any identity join any tenant id it put in its context).
--  * UPDATE/DELETE: never one's own membership (UpdateMember and RemoveMember refuse the caller as
--    target), and an owner row or role='owner' only when the caller is an owner (mayTouch: an admin never
--    touches an owner and never hands out owner). The last-owner rule stays in Go under memberLock.
-- Every is_tenant_* call is a scalar sub-select, an InitPlan evaluated once per statement.
DROP POLICY tenant_admin_membership_insert ON core.memberships;
ALTER POLICY own_membership_insert ON core.memberships
 WITH CHECK(tenant_id = core.tenant_id() AND user_id = identity.user_id() AND (SELECT core.tenant_unclaimed()));
ALTER POLICY tenant_admin_membership_read ON core.memberships
 USING(tenant_id = core.tenant_id() AND (SELECT core.is_tenant_admin()));
ALTER POLICY tenant_admin_membership_update ON core.memberships
 USING(tenant_id = core.tenant_id() AND (SELECT core.is_tenant_admin()) AND user_id <> identity.user_id()
   AND (role <> 'owner' OR (SELECT core.is_tenant_owner())))
 WITH CHECK(tenant_id = core.tenant_id() AND (SELECT core.is_tenant_admin()) AND user_id <> identity.user_id()
   AND (role <> 'owner' OR (SELECT core.is_tenant_owner())));
ALTER POLICY tenant_admin_membership_delete ON core.memberships
 USING(tenant_id = core.tenant_id() AND (SELECT core.is_tenant_admin()) AND user_id <> identity.user_id()
   AND (role <> 'owner' OR (SELECT core.is_tenant_owner())));
ALTER POLICY tenant_admin_member_projects ON core.member_projects
 USING(tenant_id = core.tenant_id() AND (SELECT core.is_tenant_admin()))
 WITH CHECK(tenant_id = core.tenant_id() AND (SELECT core.is_tenant_admin()));

-- AddMember refuses an actor with 20 or more refused adds in 24 hours (member.add_refused:*).
CREATE INDEX audit_logs_add_refused ON core.audit_logs(tenant_id, actor_id, at)
 WHERE action LIKE 'member.add_refused%';

RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP INDEX core.audit_logs_add_refused;
-- 00001 / 00019 policies, verbatim.
ALTER POLICY own_membership_insert ON core.memberships
 WITH CHECK(tenant_id=core.tenant_id() AND user_id=identity.user_id());
ALTER POLICY tenant_admin_membership_read ON core.memberships
 USING(tenant_id = core.tenant_id() AND core.is_tenant_admin());
CREATE POLICY tenant_admin_membership_insert ON core.memberships FOR INSERT TO aether_app
 WITH CHECK(tenant_id = core.tenant_id() AND core.is_tenant_admin());
ALTER POLICY tenant_admin_membership_update ON core.memberships
 USING(tenant_id = core.tenant_id() AND core.is_tenant_admin())
 WITH CHECK(tenant_id = core.tenant_id() AND core.is_tenant_admin());
ALTER POLICY tenant_admin_membership_delete ON core.memberships
 USING(tenant_id = core.tenant_id() AND core.is_tenant_admin());
ALTER POLICY tenant_admin_member_projects ON core.member_projects
 USING(tenant_id = core.tenant_id() AND core.is_tenant_admin())
 WITH CHECK(tenant_id = core.tenant_id() AND core.is_tenant_admin());

-- 00022 bodies, verbatim.
CREATE OR REPLACE FUNCTION core.identity_membership_count(target uuid) RETURNS integer
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT CASE WHEN core.is_tenant_admin() AND target IS NOT NULL
   THEN (SELECT count(*)::integer FROM core.memberships m WHERE m.user_id = target) ELSE -1 END
$$;
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

DROP FUNCTION core.tenant_unclaimed();
DROP FUNCTION core.is_tenant_owner();
DROP FUNCTION identity.create_member_identity(uuid,text,text,text,text);
DROP FUNCTION identity.own_password_hash();
DROP FUNCTION identity.any_user_exists();
DROP FUNCTION identity.login_candidate(text);
REVOKE SELECT ON identity.users FROM aether_app;
GRANT SELECT ON identity.users TO aether_app;
DROP POLICY users_insert ON identity.users;
DROP POLICY users_tenant_admin_read ON identity.users;
DROP POLICY users_self_read ON identity.users;
DROP POLICY users_owner_access ON identity.users;
ALTER TABLE identity.users NO FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.users DISABLE ROW LEVEL SECURITY;
RESET ROLE;
