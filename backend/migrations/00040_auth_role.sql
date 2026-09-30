-- A login-only database role for the API's authentication path (review M2 of 00035).
--
-- Until now identity.login_candidate(email) and identity.own_password_hash() returned a password hash to any
-- aether_app session, so a query bug or injection anywhere in the runtime could fetch the hash of any account whose
-- email or uuid it knew and crack it offline. Argon2id runs in Go, so a hash has to reach Go for login; it now
-- reaches it only through a second, separate pool that logs in as aether_auth:
--
--   aether_auth   LOGIN (password set by cmd/migrate from AUTH_DB_PASSWORD), CONNECT, USAGE on identity, and
--                 EXECUTE on exactly one definer: login_candidate(email). No table, no other schema, no other
--                 definer, no role membership. The API's small auth pool (AUTH_DATABASE_URL) checks this at start-up
--                 (postgres.CheckAuthRole). Change-password reads the caller's own email through the runtime and
--                 looks it up here, requiring the id to be the caller's.
--   aether_app    loses login_candidate and own_password_hash (dropped). Where it needs to know that a password is
--                 still the one Go verified (StartSession, ChangeOwnPassword), it asks own_password_is(digest): a
--                 boolean for the caller's own identity only. The argument is the sha256 of the verified hash, never
--                 the hash itself, so a logged bind parameter is useless for cracking; the row is taken FOR NO KEY
--                 UPDATE so a concurrent change or reset of that password waits for the session or change to commit
--                 (the reset then revokes that session).
--
-- The role is created here without a password (NOLOGIN) when it does not exist: roles are cluster-wide and
-- init-db.sh only runs on a new volume. Its attributes are then forced whatever an existing role had. cmd/migrate
-- sets LOGIN and the password (a SCRAM verifier, so the plaintext never reaches the server) whenever
-- AUTH_DB_PASSWORD is set, and turns off bind-parameter logging for aether_app and aether_auth on every run. Down
-- keeps the role (another database of the cluster may use it) but takes back LOGIN and everything granted here.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'aether_auth') THEN
    CREATE ROLE aether_auth NOLOGIN;
  END IF;
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO aether_auth', current_database());
END $$;
-- +goose StatementEnd
ALTER ROLE aether_auth NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION;
SET ROLE aether_owner;
GRANT USAGE ON SCHEMA identity TO aether_auth;

-- Login: unchanged body (00039: erased identities are skipped); only who may call it changes.
REVOKE EXECUTE ON FUNCTION identity.login_candidate(text) FROM aether_app;
GRANT EXECUTE ON FUNCTION identity.login_candidate(text) TO aether_auth;

DROP FUNCTION identity.own_password_hash();

-- "Is my password still the one that was verified?" for the runtime: true only for the caller's own, live identity
-- whose stored hash has this sha256 digest. The row stays locked until the caller's transaction ends.
-- +goose StatementBegin
CREATE FUNCTION identity.own_password_is(digest bytea) RETURNS boolean
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE stored text;
BEGIN
  IF digest IS NULL OR octet_length(digest) <> 32 OR identity.user_id() IS NULL THEN
    RETURN false;
  END IF;
  SELECT u.password_hash INTO stored FROM identity.users u
   WHERE u.id = identity.user_id() AND u.erased_at IS NULL FOR NO KEY UPDATE;
  RETURN stored IS NOT NULL AND stored <> '' AND sha256(convert_to(stored, 'UTF8')) = digest;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION identity.own_password_is(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.own_password_is(bytea) TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION identity.own_password_is(bytea);
CREATE FUNCTION identity.own_password_hash() RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT u.password_hash FROM identity.users u WHERE u.id = identity.user_id()
$$;
REVOKE ALL ON FUNCTION identity.own_password_hash() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.own_password_hash() TO aether_app;
REVOKE EXECUTE ON FUNCTION identity.login_candidate(text) FROM aether_auth;
GRANT EXECUTE ON FUNCTION identity.login_candidate(text) TO aether_app;
REVOKE USAGE ON SCHEMA identity FROM aether_auth;
RESET ROLE;
ALTER ROLE aether_auth NOLOGIN;
-- +goose StatementBegin
DO $$
BEGIN
  EXECUTE format('REVOKE CONNECT ON DATABASE %I FROM aether_auth', current_database());
END $$;
-- +goose StatementEnd
