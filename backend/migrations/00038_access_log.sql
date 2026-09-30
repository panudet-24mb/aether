-- Read-access log for personal data (PDPA §37(1); the PDPC 2022 security-measures notification asks for an
-- audit trail of access to personal data, not only of changes).
--
-- core.access_log is append-only: the runtime may INSERT a row about its own reads (tenant and actor pinned to
-- the transaction's identity) and nothing else; only a workspace owner reads it. It is partitioned by month on
-- `at` and kept by the same core.maintain_partitions as the sample history, so an expired month is one DROP TABLE.
--
-- Its retention is NOT an argument the runtime passes: the runtime has no DELETE on the trail, and a caller-supplied
-- retention would let it shrink the trail anyway. It lives in core.retention_policy (one row, owner-only, default
-- 400 days), which only the migration role changes, through core.set_access_log_retention (cmd/migrate applies
-- ACCESS_LOG_RETENTION_DAYS). Sample and BLE retention stay arguments: the runtime already holds DELETE on those.
--
-- The partition functions of 00037 learn a third table (same signatures):
--   partition_has_rows_between  the time column is `at` for access_log (received_at elsewhere)
--   maintain_partitions         access_log ranges, monthly; a table with no range yet starts at the current month,
--                               not at now() - retention: no months of empty partitions for a new table
--   prune_history               also empties expired rows of access_log_default
--   partition_health            reports access_log too
-- The access log has no foreign keys, so creating one of its ranges locks nothing but its own DEFAULT.
--
-- Also: an index for reading core.audit_logs by time, which the owner's privacy view now does.

-- +goose Up
SET ROLE aether_owner;

CREATE TABLE core.access_log (
 tenant_id uuid NOT NULL, id uuid NOT NULL, at timestamptz NOT NULL DEFAULT now(),
 actor_id uuid NOT NULL,
 resource text NOT NULL CHECK(length(resource) BETWEEN 1 AND 64),
 subject_kind text NOT NULL CHECK(length(subject_kind) BETWEEN 1 AND 32),
 subject_id text NOT NULL CHECK(length(subject_id) BETWEEN 1 AND 128),
 request_id text CHECK(length(request_id) <= 128),
 client_ip inet,
 CONSTRAINT access_log_pkey PRIMARY KEY(tenant_id, id, at)
) PARTITION BY RANGE (at);
CREATE INDEX access_log_recent ON core.access_log(tenant_id, at DESC, id DESC);
CREATE INDEX access_log_actor ON core.access_log(tenant_id, actor_id, at DESC);
CREATE INDEX access_log_subject ON core.access_log(tenant_id, subject_kind, subject_id, at DESC);
CREATE INDEX access_log_at_brin ON core.access_log USING brin(at);
ALTER TABLE core.access_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.access_log FORCE ROW LEVEL SECURITY;
-- A row about the caller's own read in the caller's workspace, nothing else: the caller must be a member of it
-- (their own membership row is visible to them through own_membership_read).
CREATE POLICY access_log_insert ON core.access_log FOR INSERT TO aether_app
 WITH CHECK(tenant_id = core.tenant_id() AND actor_id = identity.user_id()
  AND EXISTS(SELECT 1 FROM core.memberships m WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id()));
-- Only an owner reads the trail (a scalar sub-select: evaluated once per statement).
CREATE POLICY access_log_owner_read ON core.access_log FOR SELECT TO aether_app
 USING(tenant_id = core.tenant_id() AND (SELECT core.is_tenant_owner()));
-- The definer functions (maintenance, the member export) run as the owner role.
CREATE POLICY owner_access ON core.access_log TO aether_owner USING(true) WITH CHECK(true);
GRANT SELECT, INSERT ON core.access_log TO aether_app;

CREATE TABLE core.access_log_default PARTITION OF core.access_log DEFAULT;
ALTER TABLE core.access_log_default ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.access_log_default FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_access ON core.access_log_default TO aether_owner USING(true) WITH CHECK(true);

CREATE INDEX audit_logs_recent ON core.audit_logs(tenant_id, at DESC, id DESC);

-- Retention of the trail: one row, readable and writable only by the owner role (the definer functions) and the
-- migration role. The runtime has no grant at all.
CREATE TABLE core.retention_policy (
 id boolean PRIMARY KEY DEFAULT true CHECK(id),
 access_days integer NOT NULL DEFAULT 400 CHECK(access_days BETWEEN 30 AND 3650),
 updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE core.retention_policy ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.retention_policy FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_access ON core.retention_policy TO aether_owner USING(true) WITH CHECK(true);
INSERT INTO core.retention_policy DEFAULT VALUES;

-- Only the migration role (a superuser) runs this: EXECUTE is revoked from PUBLIC and granted to nobody.
-- +goose StatementBegin
CREATE FUNCTION core.set_access_log_retention(days integer) RETURNS integer
 LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 UPDATE core.retention_policy SET access_days = days, updated_at = now() WHERE id RETURNING access_days
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.set_access_log_retention(integer) FROM PUBLIC;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.partition_has_rows_between(parent text, since timestamptz)
RETURNS boolean
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC'
AS $$
DECLARE p record; found boolean; col text := CASE parent WHEN 'access_log' THEN 'at' ELSE 'received_at' END;
BEGIN
 FOR p IN SELECT r.part FROM core.partition_ranges(parent) r
   WHERE r.is_default OR ((r.hi IS NULL OR r.hi > since) AND (r.lo IS NULL OR r.lo <= now()))
   ORDER BY r.hi DESC NULLS FIRST
 LOOP
  EXECUTE format('SELECT EXISTS (SELECT 1 FROM %s WHERE %I >= $1 AND %I <= now())', p.part, col, col) INTO found USING since;
  IF found THEN
   RETURN true;
  END IF;
 END LOOP;
 RETURN false;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.maintain_partitions(sample_days integer, ble_hours integer, allow_drop boolean)
RETURNS TABLE(action text, relation text, moved bigint)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET TimeZone = 'UTC'
SET lock_timeout = '500ms'
AS $$
DECLARE
 spec record; expired regclass; expired_hi timestamptz; occupied boolean; held boolean; deferred boolean; v_last timestamptz; v_lo timestamptz; v_hi timestamptz; v_name text; v_moved bigint;
 access_days integer;
BEGIN
 IF NOT pg_try_advisory_xact_lock(hashtextextended('aether.maintain_partitions', 0)) THEN
  RETURN;
 END IF;
 sample_days := least(greatest(coalesce(sample_days, 90), 1), 3650);
 ble_hours := least(greatest(coalesce(ble_hours, 24), 1), 720);
 -- The trail's retention is the policy row's, never the caller's (see the header).
 SELECT coalesce(max(r.access_days), 400) INTO access_days FROM core.retention_policy r;
 FOR spec IN SELECT * FROM (VALUES
   ('ble_history', 'day', interval '1 day', interval '14 days', make_interval(hours => ble_hours), 'core.gateways', 'received_at'),
   ('sensor_samples', 'week', interval '7 days', interval '28 days', make_interval(days => sample_days), 'core.sensor_streams, core.device_templates', 'received_at'),
   ('access_log', 'month', interval '1 month', interval '62 days', make_interval(days => access_days), NULL, 'at')
  ) AS s(tbl, unit, step, ahead, retention, refs, col)
 LOOP
  held := false; deferred := false;
  FOR expired, expired_hi IN SELECT p.part, p.hi FROM core.partition_ranges(spec.tbl) p
    WHERE coalesce(allow_drop, false) AND NOT p.is_default AND p.hi <= now() - spec.retention ORDER BY p.hi
  LOOP
   relation := expired::text; moved := 0; -- named before the drop: a dropped regclass prints as its OID
   EXECUTE format('SELECT EXISTS (SELECT 1 FROM %s)', expired) INTO occupied;
   IF occupied AND NOT core.partition_has_rows_between(spec.tbl, expired_hi + spec.retention) THEN
    IF NOT held THEN
     held := true;
     action := 'drop_held';
     RETURN NEXT;
    END IF;
    CONTINUE;
   END IF;
   BEGIN
    EXECUTE format('LOCK TABLE core.%I IN ACCESS EXCLUSIVE MODE', spec.tbl);
    EXECUTE format('DROP TABLE %s', expired);
    action := 'dropped';
    RETURN NEXT;
    RETURN;
   EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
    action := 'drop_deferred';
    RETURN NEXT;
    deferred := true;
    EXIT;
   END;
  END LOOP;
  CONTINUE WHEN deferred;

  SELECT max(p.hi) INTO v_last FROM core.partition_ranges(spec.tbl) p WHERE NOT p.is_default;
  v_lo := date_trunc(spec.unit, now() - spec.retention, 'UTC');
  IF v_last IS NOT NULL AND v_last > v_lo THEN
   v_lo := v_last;
  ELSIF v_last IS NULL AND spec.tbl = 'access_log' THEN
   -- A new trail has nothing to keep from the past: its ranges start this month.
   v_lo := date_trunc(spec.unit, now(), 'UTC');
  END IF;
  CONTINUE WHEN v_lo >= now() + spec.ahead;
  v_hi := v_lo + spec.step;
  v_name := spec.tbl || '_p' || to_char(v_lo, 'YYYYMMDD');
  IF to_regclass(format('core.%I', v_name)) IS NOT NULL THEN
   RAISE EXCEPTION 'core.% exists but is not a partition of core.%', v_name, spec.tbl;
  END IF;
  BEGIN
   IF spec.refs IS NOT NULL THEN
    EXECUTE format('LOCK TABLE %s IN SHARE ROW EXCLUSIVE MODE', spec.refs);
   END IF;
   EXECUTE format('LOCK TABLE core.%I IN ACCESS EXCLUSIVE MODE', spec.tbl || '_default');
   -- INCLUDING CONSTRAINTS: ATTACH needs the parent's CHECK constraints on the child (access_log has some).
   EXECUTE format('CREATE TABLE core.%I (LIKE core.%I INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION)', v_name, spec.tbl);
   EXECUTE format('ALTER TABLE core.%I ADD CONSTRAINT %I CHECK (%I >= %L AND %I < %L)', v_name, v_name || '_range', spec.col, v_lo, spec.col, v_hi);
   EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY', v_name);
   EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY', v_name);
   EXECUTE format('CREATE POLICY owner_access ON core.%I TO aether_owner USING(true) WITH CHECK(true)', v_name);
   EXECUTE format('WITH m AS (DELETE FROM core.%I WHERE %I >= %L AND %I < %L RETURNING *) INSERT INTO core.%I SELECT * FROM m',
    spec.tbl || '_default', spec.col, v_lo, spec.col, v_hi, v_name);
   GET DIAGNOSTICS v_moved = ROW_COUNT;
   EXECUTE format('ALTER TABLE core.%I ATTACH PARTITION core.%I FOR VALUES FROM (%L) TO (%L)', spec.tbl, v_name, v_lo, v_hi);
   EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT %I', v_name, v_name || '_range');
   action := 'created'; relation := 'core.' || v_name; moved := v_moved;
   RETURN NEXT;
   RETURN;
  EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
   action := 'create_deferred'; relation := 'core.' || v_name; moved := 0;
   RETURN NEXT;
  END;
 END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.prune_history(sample_days integer, ble_hours integer, batch integer)
RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET TimeZone = 'UTC'
SET lock_timeout = '3s'
AS $$
DECLARE
 target record; n bigint; total bigint := 0; access_days integer;
BEGIN
 IF NOT pg_try_advisory_xact_lock(hashtextextended('aether.prune_history', 0)) THEN
  RETURN 0;
 END IF;
 sample_days := least(greatest(coalesce(sample_days, 90), 1), 3650);
 ble_hours := least(greatest(coalesce(ble_hours, 24), 1), 720);
 SELECT coalesce(max(r.access_days), 400) INTO access_days FROM core.retention_policy r;
 batch := least(greatest(coalesce(batch, 20000), 1), 50000);
 FOR target IN SELECT * FROM (VALUES
   ('ble_history_legacy', 'received_at', now() - make_interval(hours => ble_hours)),
   ('ble_history_default', 'received_at', now() - make_interval(hours => ble_hours)),
   ('sensor_samples_legacy', 'received_at', now() - make_interval(days => sample_days)),
   ('sensor_samples_default', 'received_at', now() - make_interval(days => sample_days)),
   ('access_log_default', 'at', now() - make_interval(days => access_days))
  ) AS t(rel, col, cutoff)
 LOOP
  CONTINUE WHEN to_regclass(format('core.%I', target.rel)) IS NULL;
  EXECUTE format('DELETE FROM core.%I WHERE ctid = ANY (ARRAY(SELECT ctid FROM core.%I WHERE %I < %L LIMIT %s))',
   target.rel, target.rel, target.col, target.cutoff, batch);
  GET DIAGNOSTICS n = ROW_COUNT;
  total := total + n;
 END LOOP;
 RETURN total;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.partition_health()
RETURNS TABLE(parent text, covered_until timestamptz, default_rows bigint)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET TimeZone = 'UTC'
AS $$
DECLARE t text;
BEGIN
 FOREACH t IN ARRAY ARRAY['ble_history','sensor_samples','access_log'] LOOP
  parent := t;
  SELECT max(p.hi) INTO covered_until FROM core.partition_ranges(t) p WHERE NOT p.is_default;
  default_rows := 0;
  IF to_regclass(format('core.%I', t || '_default')) IS NOT NULL THEN
   EXECUTE format('SELECT count(*) FROM (SELECT 1 FROM core.%I LIMIT 1000) d', t || '_default') INTO default_rows;
  END IF;
  RETURN NEXT;
 END LOOP;
END $$;
-- +goose StatementEnd

-- The first ranges of the trail: this month up to the look-ahead. Nothing is dropped here.
-- +goose StatementBegin
DO $$ DECLARE n int := 0; BEGIN
 LOOP
  EXIT WHEN NOT EXISTS (SELECT 1 FROM core.maintain_partitions(3650, 720, false) m WHERE m.action = 'created');
  n := n + 1;
  IF n > 100 THEN
   RAISE EXCEPTION 'partition set-up did not converge';
  END IF;
 END LOOP;
END $$;
-- +goose StatementEnd
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP INDEX core.audit_logs_recent;
DROP TABLE core.access_log;
DROP FUNCTION core.set_access_log_retention(integer);
DROP TABLE core.retention_policy;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.partition_health()
RETURNS TABLE(parent text, covered_until timestamptz, default_rows bigint)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET TimeZone = 'UTC'
AS $$
DECLARE t text;
BEGIN
 FOREACH t IN ARRAY ARRAY['ble_history','sensor_samples'] LOOP
  parent := t;
  SELECT max(p.hi) INTO covered_until FROM core.partition_ranges(t) p WHERE NOT p.is_default;
  default_rows := 0;
  IF to_regclass(format('core.%I', t || '_default')) IS NOT NULL THEN
   EXECUTE format('SELECT count(*) FROM (SELECT 1 FROM core.%I LIMIT 1000) d', t || '_default') INTO default_rows;
  END IF;
  RETURN NEXT;
 END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.prune_history(sample_days integer, ble_hours integer, batch integer)
RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET TimeZone = 'UTC'
SET lock_timeout = '3s'
AS $$
DECLARE
 target record; n bigint; total bigint := 0;
BEGIN
 IF NOT pg_try_advisory_xact_lock(hashtextextended('aether.prune_history', 0)) THEN
  RETURN 0;
 END IF;
 sample_days := least(greatest(coalesce(sample_days, 90), 1), 3650);
 ble_hours := least(greatest(coalesce(ble_hours, 24), 1), 720);
 batch := least(greatest(coalesce(batch, 20000), 1), 50000);
 FOR target IN SELECT * FROM (VALUES
   ('ble_history_legacy', now() - make_interval(hours => ble_hours)),
   ('ble_history_default', now() - make_interval(hours => ble_hours)),
   ('sensor_samples_legacy', now() - make_interval(days => sample_days)),
   ('sensor_samples_default', now() - make_interval(days => sample_days))
  ) AS t(rel, cutoff)
 LOOP
  CONTINUE WHEN to_regclass(format('core.%I', target.rel)) IS NULL;
  EXECUTE format('DELETE FROM core.%I WHERE ctid = ANY (ARRAY(SELECT ctid FROM core.%I WHERE received_at < %L LIMIT %s))',
   target.rel, target.rel, target.cutoff, batch);
  GET DIAGNOSTICS n = ROW_COUNT;
  total := total + n;
 END LOOP;
 RETURN total;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.maintain_partitions(sample_days integer, ble_hours integer, allow_drop boolean)
RETURNS TABLE(action text, relation text, moved bigint)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET TimeZone = 'UTC'
SET lock_timeout = '500ms'
AS $$
DECLARE
 spec record; expired regclass; expired_hi timestamptz; occupied boolean; held boolean; deferred boolean; v_last timestamptz; v_lo timestamptz; v_hi timestamptz; v_name text; v_moved bigint;
BEGIN
 IF NOT pg_try_advisory_xact_lock(hashtextextended('aether.maintain_partitions', 0)) THEN
  RETURN;
 END IF;
 sample_days := least(greatest(coalesce(sample_days, 90), 1), 3650);
 ble_hours := least(greatest(coalesce(ble_hours, 24), 1), 720);
 FOR spec IN SELECT * FROM (VALUES
   ('ble_history', 'day', interval '1 day', interval '14 days', make_interval(hours => ble_hours), 'core.gateways'),
   ('sensor_samples', 'week', interval '7 days', interval '28 days', make_interval(days => sample_days), 'core.sensor_streams, core.device_templates')
  ) AS s(tbl, unit, step, ahead, retention, refs)
 LOOP
  -- Expired partitions oldest first. An empty one always goes; one with rows only when the data anchor agrees
  -- (see above). A held one is reported once and blocks only this table: a newer empty one may still go.
  held := false; deferred := false;
  FOR expired, expired_hi IN SELECT p.part, p.hi FROM core.partition_ranges(spec.tbl) p
    WHERE coalesce(allow_drop, false) AND NOT p.is_default AND p.hi <= now() - spec.retention ORDER BY p.hi
  LOOP
   relation := expired::text; moved := 0; -- named before the drop: a dropped regclass prints as its OID
   EXECUTE format('SELECT EXISTS (SELECT 1 FROM %s)', expired) INTO occupied;
   IF occupied AND NOT core.partition_has_rows_between(spec.tbl, expired_hi + spec.retention) THEN
    IF NOT held THEN
     held := true;
     action := 'drop_held';
     RETURN NEXT;
    END IF;
    CONTINUE;
   END IF;
   BEGIN
    EXECUTE format('LOCK TABLE core.%I IN ACCESS EXCLUSIVE MODE', spec.tbl);
    EXECUTE format('DROP TABLE %s', expired);
    action := 'dropped';
    RETURN NEXT;
    RETURN;
   EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
    action := 'drop_deferred';
    RETURN NEXT;
    deferred := true;
    EXIT;
   END;
  END LOOP;
  CONTINUE WHEN deferred; -- the next run retries; no create while this table's lock is busy

  SELECT max(p.hi) INTO v_last FROM core.partition_ranges(spec.tbl) p WHERE NOT p.is_default;
  -- After an outage longer than the retention, start at the oldest range still kept; the gap stays in DEFAULT.
  v_lo := date_trunc(spec.unit, now() - spec.retention, 'UTC');
  IF v_last IS NOT NULL AND v_last > v_lo THEN
   v_lo := v_last;
  END IF;
  CONTINUE WHEN v_lo >= now() + spec.ahead;
  v_hi := v_lo + spec.step;
  v_name := spec.tbl || '_p' || to_char(v_lo, 'YYYYMMDD');
  IF to_regclass(format('core.%I', v_name)) IS NOT NULL THEN
   RAISE EXCEPTION 'core.% exists but is not a partition of core.%', v_name, spec.tbl;
  END IF;
  BEGIN
   EXECUTE format('LOCK TABLE %s IN SHARE ROW EXCLUSIVE MODE', spec.refs);
   EXECUTE format('LOCK TABLE core.%I IN ACCESS EXCLUSIVE MODE', spec.tbl || '_default');
   EXECUTE format('CREATE TABLE core.%I (LIKE core.%I INCLUDING DEFAULTS INCLUDING STORAGE INCLUDING COMPRESSION)', v_name, spec.tbl);
   -- Lets ATTACH skip scanning the rows moved in below; redundant with the bound afterwards, so dropped.
   EXECUTE format('ALTER TABLE core.%I ADD CONSTRAINT %I CHECK (received_at >= %L AND received_at < %L)', v_name, v_name || '_range', v_lo, v_hi);
   EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY', v_name);
   EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY', v_name);
   EXECUTE format('CREATE POLICY owner_access ON core.%I TO aether_owner USING(true) WITH CHECK(true)', v_name);
   EXECUTE format('WITH m AS (DELETE FROM core.%I WHERE received_at >= %L AND received_at < %L RETURNING *) INSERT INTO core.%I SELECT * FROM m',
    spec.tbl || '_default', v_lo, v_hi, v_name);
   GET DIAGNOSTICS v_moved = ROW_COUNT;
   EXECUTE format('ALTER TABLE core.%I ATTACH PARTITION core.%I FOR VALUES FROM (%L) TO (%L)', spec.tbl, v_name, v_lo, v_hi);
   EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT %I', v_name, v_name || '_range');
   action := 'created'; relation := 'core.' || v_name; moved := v_moved;
   RETURN NEXT;
   RETURN;
  EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
   action := 'create_deferred'; relation := 'core.' || v_name; moved := 0;
   RETURN NEXT;
  END;
 END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.partition_has_rows_between(parent text, since timestamptz)
RETURNS boolean
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC'
AS $$
DECLARE p record; found boolean;
BEGIN
 FOR p IN SELECT r.part FROM core.partition_ranges(parent) r
   WHERE r.is_default OR ((r.hi IS NULL OR r.hi > since) AND (r.lo IS NULL OR r.lo <= now()))
   ORDER BY r.hi DESC NULLS FIRST
 LOOP
  EXECUTE format('SELECT EXISTS (SELECT 1 FROM %s WHERE received_at >= $1 AND received_at <= now())', p.part) INTO found USING since;
  IF found THEN
   RETURN true;
  END IF;
 END LOOP;
 RETURN false;
END $$;
-- +goose StatementEnd
RESET ROLE;
