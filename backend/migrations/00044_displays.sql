-- +goose Up
-- Display links (docs/platform/display.md): a wall TV in a control room shows a rotating, read-only view of the
-- workspace and takes over the screen when an SOS or hazard alert opens. A TV must not hold a staff login, so it
-- is its own kind of principal:
--
--   core.displays   one row per screen: name, project scope, playlist, and two owner switches (show wearer names,
--                   acknowledge from the TV). An owner or a full-scope admin creates it and gets a one-time pairing
--                   code (10 minutes, single use); the TV exchanges the code for a long-lived display token. Both
--                   are stored only as SHA-256 digests and the runtime has no SELECT on either column.
--   app.display_id  a transaction setting, like app.user_id. A display transaction has no user; its project scope
--                   comes from the display row (core.compute_project_scope learns a display branch), so the same
--                   RESTRICTIVE policies narrow what a TV sees as for a member. The API also marks every display
--                   transaction READ ONLY, apart from the two writes a TV may do (its read-access log rows and, if
--                   the owner allowed it, acknowledging an alert).
--
-- Also: the read-access log records a display as the actor (actor_kind), and an alert remembers which display
-- acknowledged it (acked_by_display).
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';

CREATE TABLE core.displays (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 id uuid NOT NULL,
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 80),
 -- Empty: every project of the workspace. Otherwise the display sees exactly these projects.
 project_ids uuid[] NOT NULL DEFAULT '{}' CHECK(cardinality(project_ids) <= 50),
 playlist jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(playlist) = 'array' AND pg_column_size(playlist) <= 16000),
 show_names boolean NOT NULL DEFAULT false,
 allow_ack boolean NOT NULL DEFAULT false,
 pairing_hash text CHECK(pairing_hash ~ '^[0-9a-f]{64}$'),
 pairing_expires_at timestamptz,
 token_hash text CHECK(token_hash ~ '^[0-9a-f]{64}$'),
 paired_at timestamptz,
 last_seen_at timestamptz,
 last_ip inet,
 revoked_at timestamptz,
 created_by uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id, id),
 UNIQUE(id),
 CHECK((pairing_hash IS NULL) = (pairing_expires_at IS NULL))
);
CREATE UNIQUE INDEX displays_pairing ON core.displays(pairing_hash) WHERE pairing_hash IS NOT NULL;
CREATE UNIQUE INDEX displays_token ON core.displays(token_hash) WHERE token_hash IS NOT NULL;
ALTER TABLE core.displays ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.displays FORCE ROW LEVEL SECURITY;
-- Members manage displays; a display (no user) never reads this table through the runtime, only through the
-- definer functions below. Restricted members are kept out: a display may show several projects.
CREATE POLICY tenant_scope ON core.displays TO aether_app
 USING(tenant_id = core.tenant_id() AND identity.user_id() IS NOT NULL AND (SELECT core.scope_all()))
 WITH CHECK(tenant_id = core.tenant_id() AND identity.user_id() IS NOT NULL AND (SELECT core.scope_all()));
CREATE POLICY owner_access ON core.displays TO aether_owner USING(true) WITH CHECK(true);
-- The digests are never read back by the runtime.
GRANT SELECT(tenant_id, id, name, project_ids, playlist, show_names, allow_ack, pairing_expires_at, paired_at,
 last_seen_at, last_ip, revoked_at, created_by, created_at, updated_at) ON core.displays TO aether_app;
GRANT INSERT(tenant_id, id, name, project_ids, playlist, show_names, allow_ack, pairing_hash, pairing_expires_at, created_by)
 ON core.displays TO aether_app;
GRANT UPDATE(name, project_ids, playlist, show_names, allow_ack, pairing_hash, pairing_expires_at, token_hash,
 paired_at, revoked_at, updated_at) ON core.displays TO aether_app;

-- +goose StatementBegin
CREATE FUNCTION core.display_id() RETURNS uuid LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT nullif(current_setting('app.display_id', true), '')::uuid
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.display_id() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.display_id() TO aether_app;

-- A display transaction: no user, a paired and unrevoked display of the current workspace.
-- +goose StatementBegin
CREATE FUNCTION core.display_active() RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT identity.user_id() IS NULL AND EXISTS(SELECT 1 FROM core.displays d
  WHERE d.id = core.display_id() AND d.tenant_id = core.tenant_id() AND d.revoked_at IS NULL AND d.token_hash IS NOT NULL)
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.display_active() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.display_active() TO aether_app;

-- The project scope of a transaction, now with displays. A display id together with a user id is refused (NULL:
-- every restrictive policy denies), and so is a display that is revoked, unpaired or of another workspace.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.compute_project_scope() RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT CASE
 WHEN core.display_id() IS NOT NULL THEN (
  SELECT CASE WHEN cardinality(d.project_ids) = 0 THEN '*'
   ELSE array_to_string(ARRAY(SELECT x::text FROM unnest(d.project_ids) x ORDER BY 1), ',') END
  FROM core.displays d
  WHERE d.id = core.display_id() AND d.tenant_id = core.tenant_id() AND identity.user_id() IS NULL
   AND d.revoked_at IS NULL AND d.token_hash IS NOT NULL)
 WHEN identity.user_id() IS NULL OR core.tenant_id() IS NULL THEN '*'
 WHEN EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id=core.tenant_id() AND m.user_id=identity.user_id() AND m.role='owner') THEN '*'
 WHEN NOT EXISTS(SELECT 1 FROM core.member_projects p WHERE p.tenant_id=core.tenant_id() AND p.user_id=identity.user_id()) THEN '*'
 ELSE (SELECT string_agg(p.project_id::text,',' ORDER BY p.project_id) FROM core.member_projects p WHERE p.tenant_id=core.tenant_id() AND p.user_id=identity.user_id()) END
$$;
-- +goose StatementEnd

-- Pairing: the digest of a pairing code that has not expired becomes the display's token digest, once. Called
-- before any workspace is known (like a login), so it is a definer; it only ever answers for the caller's own code.
-- The caller writes the audit row once the workspace is known.
-- +goose StatementBegin
CREATE FUNCTION core.pair_display(code_hash text, new_token_hash text) RETURNS TABLE(display_id uuid, tenant_id uuid)
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE d record;
BEGIN
 IF code_hash !~ '^[0-9a-f]{64}$' OR new_token_hash !~ '^[0-9a-f]{64}$' THEN
  RETURN;
 END IF;
 UPDATE core.displays x SET token_hash = new_token_hash, pairing_hash = NULL, pairing_expires_at = NULL,
  paired_at = now(), updated_at = now()
  FROM core.tenants t
  WHERE x.pairing_hash = code_hash AND x.pairing_expires_at > now() AND x.revoked_at IS NULL
   AND t.id = x.tenant_id AND t.status = 'active'
  RETURNING x.id, x.tenant_id INTO d;
 IF d.id IS NULL THEN
  RETURN;
 END IF;
 display_id := d.id;
 tenant_id := d.tenant_id;
 RETURN NEXT;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.pair_display(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.pair_display(text, text) TO aether_app;

-- A request from a TV: the display, its settings and its workspace, for a token digest that matches. It also
-- remembers when (at most once a minute, or at once from a new address) and from where the display was last seen.
-- +goose StatementBegin
CREATE FUNCTION core.display_session(token text, ip inet)
 RETURNS TABLE(display_id uuid, tenant_id uuid, name text, project_ids uuid[], playlist jsonb, show_names boolean, allow_ack boolean, tenant_name text)
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
 IF token !~ '^[0-9a-f]{64}$' THEN
  RETURN;
 END IF;
 UPDATE core.displays d SET last_seen_at = now(), last_ip = ip
  WHERE d.token_hash = token AND d.revoked_at IS NULL
   AND (d.last_seen_at IS NULL OR d.last_seen_at < now() - interval '1 minute' OR d.last_ip IS DISTINCT FROM ip);
 RETURN QUERY SELECT d.id, d.tenant_id, d.name, d.project_ids, d.playlist, d.show_names, d.allow_ack, t.name
  FROM core.displays d JOIN core.tenants t ON t.id = d.tenant_id
  WHERE d.token_hash = token AND d.revoked_at IS NULL AND t.status = 'active';
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.display_session(text, inet) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.display_session(text, inet) TO aether_app;

-- What a TV may name. A tag counts as NOT personal only when the workspace has at least one live registration of it
-- and none of its live registrations is roaming or uses a worn profile (the profile ids come from the Go catalog).
-- Everything else (worn, removed, unregistered, or registered in a project the display cannot see as worn) is
-- personal and is masked. A definer, so a worn registration in another project still counts; it answers only ids
-- of this workspace, and only for a paired display.
-- +goose StatementBegin
CREATE FUNCTION core.display_impersonal_tags(worn_profiles text[]) RETURNS SETOF text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT lower(d.external_id) FROM core.devices d
  WHERE d.tenant_id = core.tenant_id() AND d.removed_at IS NULL AND (SELECT core.display_active())
  GROUP BY lower(d.external_id)
  HAVING bool_and(NOT d.roaming AND NOT (d.profile_id = ANY(worn_profiles)))
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.display_impersonal_tags(text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.display_impersonal_tags(text[]) TO aether_app;

-- The read-access log learns a second kind of actor. A display may log only about itself.
ALTER TABLE core.access_log ADD COLUMN actor_kind text NOT NULL DEFAULT 'member';
ALTER TABLE core.access_log ADD CONSTRAINT access_log_actor_kind CHECK(actor_kind IN ('member', 'display'));
DROP POLICY access_log_insert ON core.access_log;
CREATE POLICY access_log_insert ON core.access_log FOR INSERT TO aether_app
 WITH CHECK(tenant_id = core.tenant_id() AND (
  (actor_kind = 'member' AND actor_id = identity.user_id()
   AND EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id()))
  OR (actor_kind = 'display' AND actor_id = core.display_id() AND (SELECT core.display_active()))));

-- Which display acknowledged an alert (acked_by stays a member id). Nullable, no default: metadata only.
ALTER TABLE core.alerts ADD COLUMN acked_by_display uuid;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';
ALTER TABLE core.alerts DROP COLUMN acked_by_display;
DROP POLICY access_log_insert ON core.access_log;
ALTER TABLE core.access_log NO FORCE ROW LEVEL SECURITY;
DELETE FROM core.access_log WHERE actor_kind = 'display';
ALTER TABLE core.access_log FORCE ROW LEVEL SECURITY;
ALTER TABLE core.access_log DROP CONSTRAINT access_log_actor_kind;
ALTER TABLE core.access_log DROP COLUMN actor_kind;
CREATE POLICY access_log_insert ON core.access_log FOR INSERT TO aether_app
 WITH CHECK(tenant_id = core.tenant_id() AND actor_id = identity.user_id()
  AND EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id()));
DROP FUNCTION core.display_session(text, inet);
DROP FUNCTION core.display_impersonal_tags(text[]);
DROP FUNCTION core.pair_display(text, text);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.compute_project_scope() RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT CASE WHEN identity.user_id() IS NULL OR core.tenant_id() IS NULL THEN '*'
 WHEN EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id=core.tenant_id() AND m.user_id=identity.user_id() AND m.role='owner') THEN '*'
 WHEN NOT EXISTS(SELECT 1 FROM core.member_projects p WHERE p.tenant_id=core.tenant_id() AND p.user_id=identity.user_id()) THEN '*'
 ELSE (SELECT string_agg(p.project_id::text,',' ORDER BY p.project_id) FROM core.member_projects p WHERE p.tenant_id=core.tenant_id() AND p.user_id=identity.user_id()) END
$$;
-- +goose StatementEnd
DROP FUNCTION core.display_active();
DROP FUNCTION core.display_id();
DROP TABLE core.displays;
RESET ROLE;
