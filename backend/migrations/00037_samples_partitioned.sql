-- Time-partitioned core.sensor_samples (weekly, from Monday 00:00 UTC) and core.ble_history (daily, 00:00 UTC).
--
-- Retention used to be batched DELETEs: WAL for every tuple, dead tuples and bloat on three btree indexes. With
-- partitions, an expired week or day is one DROP TABLE. Retention becomes partition-grained: samples are kept
-- between SAMPLE_RETENTION_DAYS and SAMPLE_RETENTION_DAYS + 7 days, BLE history between BLE_HISTORY_HOURS and
-- BLE_HISTORY_HOURS + 24 hours.
--
-- The existing tables are not copied. Each is renamed to <table>_legacy and attached as the partition
-- FROM (MINVALUE) TO (X), where X is the cutover of migration 00036, whose validated CHECK (received_at < X) lets
-- ATTACH skip the scan; its indexes, primary key and foreign keys already match the parent's, so ATTACH adopts
-- them instead of building. Rows from X on go to new partitions. Legacy is dropped whole once X is older than the
-- retention, and until then core.prune_history deletes its expired rows in batches, as before.
--
-- Partitions are created ahead (samples 28 days, BLE history 14 days) by core.maintain_partitions, which the API
-- calls at start-up and hourly, one step per call and transaction, dropping at most 2 partitions per run. A
-- DEFAULT partition catches any row no range covers, so a maintenance outage can never stall ingest; creating a
-- range moves its rows out of DEFAULT first. core.partition_health reports DEFAULT rows and how far ahead the
-- ranges reach; the API logs an error when DEFAULT holds rows or less than 7 days are covered.
--
-- Row-level security applies to the table a query names, so queries go through the parent, whose policies and
-- grants are exactly those of the old tables. Partitions carry FORCE RLS with only an owner policy (for the
-- definer functions below) and no grants: the runtime role cannot address a partition directly.
--
-- Lock order, derived from every ingest path. Each first holds its gateway row FOR UPDATE (ROW SHARE on
-- core.gateways); after that:
--   Minew (CapturePacket): ble_history (insert), then sensor_streams (read, then insert/update in saveSamples, whose
--     foreign-key checks touch device_templates), then sensor_samples (thinning read, insert).
--   Zigbee2MQTT, Aether Edge (Tuya Wi-Fi) and Tuya Cloud (ingestDeviceState): sensor_streams first (an explicit
--     ROW EXCLUSIVE lock taken before the thinning read), then sensor_samples. None of them writes ble_history.
-- ATTACH of a table that has its own foreign keys (legacy) merges them into the parent's and drops the legacy
-- triggers on the referenced tables, which takes ACCESS EXCLUSIVE on core.gateways, core.sensor_streams and
-- core.device_templates. So this migration locks, in that ingest order: gateways, ble_history, sensor_streams,
-- device_templates, sensor_samples, all ACCESS EXCLUSIVE, before changing anything.
--
-- Not every transaction follows that order (an API request may read device_templates before sensor_streams, or
-- samples before gateways), so each attempt waits at most 100 ms per lock and gives every lock back when one is
-- busy (a NOTICE per retry): a transaction queued behind an attempt waits at most about 0.5 s, well under
-- deadlock_timeout (1 s), so the deadlock detector never picks an ingest or API transaction as its victim.
-- Attempts repeat for about 100 s before the migration fails. Once the locks are held, the change takes a few
-- hundred milliseconds: no data is copied.
--
-- core.maintain_partitions: creating a range takes SHARE ROW EXCLUSIVE on the referenced tables (a new partition
-- gets only referencing-side triggers), then ACCESS EXCLUSIVE on DEFAULT, then ATTACH (SHARE UPDATE EXCLUSIVE on
-- the parent); dropping one takes ACCESS EXCLUSIVE on the parent (and DEFAULT). Both follow the ingest order
-- (sensor_streams before device_templates) and wait at most 500 ms per lock.

-- +goose Up
SET ROLE aether_owner;
-- Every lock up front, in the ingest order above; a busy lock gives them all back and retries.
-- +goose StatementBegin
DO $$ DECLARE attempt int; BEGIN
 FOR attempt IN 1..200 LOOP
  BEGIN
   SET LOCAL lock_timeout = '100ms';
   LOCK TABLE core.gateways IN ACCESS EXCLUSIVE MODE;
   LOCK TABLE core.ble_history IN ACCESS EXCLUSIVE MODE;
   LOCK TABLE core.sensor_streams IN ACCESS EXCLUSIVE MODE;
   LOCK TABLE core.device_templates IN ACCESS EXCLUSIVE MODE;
   LOCK TABLE core.sensor_samples IN ACCESS EXCLUSIVE MODE;
   EXIT;
  EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
   IF attempt = 200 THEN RAISE; END IF;
   RAISE NOTICE '00037: tables busy, retrying (attempt %)', attempt;
   PERFORM pg_sleep(0.3);
  END;
 END LOOP;
 SET LOCAL lock_timeout = '10s';
END $$;
-- +goose StatementEnd

ALTER TABLE core.ble_history RENAME TO ble_history_legacy;
ALTER TABLE core.ble_history_legacy RENAME CONSTRAINT ble_history_pkey TO ble_history_legacy_pkey;
ALTER INDEX core.ble_history_stream RENAME TO ble_history_legacy_stream;
ALTER INDEX core.ble_history_gateway RENAME TO ble_history_legacy_gateway;
ALTER INDEX core.ble_history_received_brin RENAME TO ble_history_legacy_received_brin;
CREATE TABLE core.ble_history (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
 event_key text NOT NULL, received_at timestamptz NOT NULL, raw text NOT NULL,
 source text NOT NULL,
 CONSTRAINT ble_history_pkey PRIMARY KEY(tenant_id,gateway_id,external_id,event_key,received_at),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
) PARTITION BY RANGE (received_at);
CREATE INDEX ble_history_stream ON core.ble_history(tenant_id,gateway_id,external_id,received_at DESC,event_key);
CREATE INDEX ble_history_gateway ON core.ble_history(tenant_id,gateway_id,received_at DESC,event_key,external_id);
CREATE INDEX ble_history_received_brin ON core.ble_history USING brin(received_at);
ALTER TABLE core.ble_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.ble_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.ble_history USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.ble_history AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
GRANT SELECT,INSERT,DELETE ON core.ble_history TO aether_app;

ALTER TABLE core.sensor_samples RENAME TO sensor_samples_legacy;
ALTER TABLE core.sensor_samples_legacy RENAME CONSTRAINT sensor_samples_pkey TO sensor_samples_legacy_pkey;
ALTER INDEX core.sensor_samples_history RENAME TO sensor_samples_legacy_history;
ALTER INDEX core.sensor_samples_identity RENAME TO sensor_samples_legacy_identity;
ALTER INDEX core.sensor_samples_received_brin RENAME TO sensor_samples_legacy_received_brin;
CREATE TABLE core.sensor_samples (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
 event_key text NOT NULL, received_at timestamptz NOT NULL, template_id uuid,
 decoder_id text NOT NULL, reading jsonb NOT NULL,
 CONSTRAINT sensor_samples_pkey PRIMARY KEY(tenant_id,gateway_id,external_id,event_key,received_at),
 FOREIGN KEY(tenant_id,gateway_id,external_id) REFERENCES core.sensor_streams(tenant_id,gateway_id,external_id),
 FOREIGN KEY(tenant_id,template_id) REFERENCES core.device_templates(tenant_id,id)
) PARTITION BY RANGE (received_at);
CREATE INDEX sensor_samples_history ON core.sensor_samples(tenant_id,gateway_id,external_id,received_at DESC,event_key);
CREATE INDEX sensor_samples_identity ON core.sensor_samples(tenant_id,external_id,received_at DESC);
CREATE INDEX sensor_samples_received_brin ON core.sensor_samples USING brin(received_at);
ALTER TABLE core.sensor_samples ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.sensor_samples FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.sensor_samples USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.sensor_samples AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
GRANT SELECT,INSERT,DELETE ON core.sensor_samples TO aether_app;

-- Attach the old tables below their cutover, then add the DEFAULT partitions.
-- +goose StatementBegin
DO $$ DECLARE t text; x timestamptz; BEGIN
 FOREACH t IN ARRAY ARRAY['ble_history','sensor_samples'] LOOP
  SELECT obj_description(c.oid, 'pg_constraint')::timestamptz INTO x FROM pg_constraint c
   WHERE c.conrelid = format('core.%I', t || '_legacy')::regclass AND c.conname = t || '_cutover' AND c.convalidated;
  IF x IS NULL THEN
   RAISE EXCEPTION 'core.%_legacy has no validated cutover constraint (migration 00036)', t;
  END IF;
  EXECUTE format('ALTER TABLE core.%I ATTACH PARTITION core.%I FOR VALUES FROM (MINVALUE) TO (%L)', t, t || '_legacy', x);
  -- The partition bound now enforces what the CHECK did.
  EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT %I', t || '_legacy', t || '_cutover');
  EXECUTE format('REVOKE ALL ON core.%I FROM aether_app', t || '_legacy');
  EXECUTE format('CREATE POLICY owner_access ON core.%I TO aether_owner USING(true) WITH CHECK(true)', t || '_legacy');
  EXECUTE format('CREATE TABLE core.%I PARTITION OF core.%I DEFAULT', t || '_default', t);
  EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY', t || '_default');
  EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY', t || '_default');
  EXECUTE format('CREATE POLICY owner_access ON core.%I TO aether_owner USING(true) WITH CHECK(true)', t || '_default');
 END LOOP;
END $$;
-- +goose StatementEnd

-- Each partition of a parent with its bounds; lo is NULL for MINVALUE, both are NULL for DEFAULT. The bound text
-- carries its UTC offset, so the cast back is exact.
-- +goose StatementBegin
CREATE FUNCTION core.partition_ranges(parent text)
RETURNS TABLE(part regclass, lo timestamptz, hi timestamptz, is_default boolean)
LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC' AS $$
 SELECT c.oid::regclass,
  CASE WHEN b.m[1] IS NULL OR b.m[1] = 'MINVALUE' THEN NULL ELSE btrim(b.m[1], '''')::timestamptz END,
  CASE WHEN b.m[2] IS NULL OR b.m[2] = 'MAXVALUE' THEN NULL ELSE btrim(b.m[2], '''')::timestamptz END,
  pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'
 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 CROSS JOIN LATERAL (SELECT regexp_match(pg_get_expr(c.relpartbound, c.oid), '^FOR VALUES FROM \((.*)\) TO \((.*)\)$') AS m) b
 WHERE i.inhparent = format('core.%I', parent)::regclass
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.partition_ranges(text) FROM PUBLIC;

-- The data anchor of a drop: true when some partition of the parent holds a row received at or after `since` and
-- not after now(). A partition that expired by the clock is dropped only when the data agrees, i.e. its upper bound
-- plus the retention is at or before the newest stored row: after a clock that jumped far ahead (or a restore
-- whose data has not caught up) every partition looks expired by now(), but not by the data. An idle system still
-- drops, because its newest row keeps moving the anchor only when rows arrive. Cheap: only partitions that can
-- hold such rows are probed, each through its BRIN index on received_at, and the probe stops at the first row.
-- +goose StatementBegin
CREATE FUNCTION core.partition_has_rows_between(parent text, since timestamptz)
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
REVOKE ALL ON FUNCTION core.partition_has_rows_between(text, timestamptz) FROM PUBLIC;

-- One maintenance step per table per call, each call its own short transaction:
--   drop   the oldest partition wholly past its retention (legacy included), when allow_drop and the partition is
--          empty or the data anchor agrees (otherwise drop_held, for this table only); the parent is locked first
--          (the drop also takes DEFAULT);
--   create the next partition when the ranges reach less far than the look-ahead: the referenced tables, then
--          DEFAULT (rows of the new range move out of it first), then ATTACH, which takes only SHARE UPDATE
--          EXCLUSIVE on the parent, so reads and inserts continue.
-- Every lock waits at most 500 ms: a busy lock or a deadlock answers drop_deferred / create_deferred and the next run
-- tries again. After a drop or a create the call returns, so the caller can count drops; after a deferral it goes
-- on with the next table. No row back means nothing to do (or another session is maintaining).
--
-- Times are UTC whatever the session's time zone: interval arithmetic on timestamptz follows TimeZone, and
-- '7 days' across a daylight-saving change is not 168 hours elsewhere. The arguments are clamped to the API's own
-- limits. Dropping expired data is no power the runtime lacks: it holds DELETE on both tables.
-- +goose StatementBegin
CREATE FUNCTION core.maintain_partitions(sample_days integer, ble_hours integer, allow_drop boolean)
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
REVOKE ALL ON FUNCTION core.maintain_partitions(integer, integer, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.maintain_partitions(integer, integer, boolean) TO aether_app;

-- Deletes up to `batch` expired rows from each partition that is not dropped by range: legacy (until it expires
-- whole) and DEFAULT. Returns how many rows went; 0 when another session is pruning. Called in a loop by the API
-- until a call deletes less than one batch.
-- +goose StatementBegin
CREATE FUNCTION core.prune_history(sample_days integer, ble_hours integer, batch integer)
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
REVOKE ALL ON FUNCTION core.prune_history(integer, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.prune_history(integer, integer, integer) TO aether_app;

-- What the API alerts on: rows sitting in DEFAULT (counted up to 1000) and where the ranges end.
-- +goose StatementBegin
CREATE FUNCTION core.partition_health()
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
REVOKE ALL ON FUNCTION core.partition_health() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.partition_health() TO aether_app;

-- The first ranges, from the cutover to the look-ahead. Nothing is dropped here.
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
-- Back to one plain table each, holding every row: legacy is detached (or, if maintenance already dropped it,
-- created empty), every other partition is copied into it, and the parent goes with its partitions. The copy runs
-- as the migration's superuser, because the parents' tenant policies hide every row from their owner.
SET ROLE aether_owner;
-- +goose StatementBegin
DO $$ DECLARE attempt int; BEGIN
 FOR attempt IN 1..200 LOOP
  BEGIN
   SET LOCAL lock_timeout = '100ms';
   LOCK TABLE core.gateways IN ACCESS EXCLUSIVE MODE;
   LOCK TABLE core.ble_history IN ACCESS EXCLUSIVE MODE;
   LOCK TABLE core.sensor_streams IN ACCESS EXCLUSIVE MODE;
   LOCK TABLE core.device_templates IN ACCESS EXCLUSIVE MODE;
   LOCK TABLE core.sensor_samples IN ACCESS EXCLUSIVE MODE;
   EXIT;
  EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
   IF attempt = 200 THEN RAISE; END IF;
   RAISE NOTICE '00037: tables busy, retrying (attempt %)', attempt;
   PERFORM pg_sleep(0.3);
  END;
 END LOOP;
 SET LOCAL lock_timeout = '10s';
END $$;
-- +goose StatementEnd
DROP FUNCTION core.partition_health();
DROP FUNCTION core.prune_history(integer, integer, integer);
DROP FUNCTION core.maintain_partitions(integer, integer, boolean);
DROP FUNCTION core.partition_has_rows_between(text, timestamptz);
DROP FUNCTION core.partition_ranges(text);

-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = to_regclass('core.ble_history_legacy')) THEN
  ALTER TABLE core.ble_history DETACH PARTITION core.ble_history_legacy;
  DROP POLICY owner_access ON core.ble_history_legacy;
 ELSE
  CREATE TABLE core.ble_history_legacy (
   tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
   event_key text NOT NULL, received_at timestamptz NOT NULL, raw text NOT NULL,
   source text NOT NULL,
   CONSTRAINT ble_history_legacy_pkey PRIMARY KEY(tenant_id,gateway_id,external_id,event_key,received_at),
   FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
  );
  CREATE INDEX ble_history_legacy_stream ON core.ble_history_legacy(tenant_id,gateway_id,external_id,received_at DESC,event_key);
  CREATE INDEX ble_history_legacy_gateway ON core.ble_history_legacy(tenant_id,gateway_id,received_at DESC,event_key,external_id);
  CREATE INDEX ble_history_legacy_received_brin ON core.ble_history_legacy USING brin(received_at);
  ALTER TABLE core.ble_history_legacy ENABLE ROW LEVEL SECURITY;
  ALTER TABLE core.ble_history_legacy FORCE ROW LEVEL SECURITY;
  CREATE POLICY tenant_scope ON core.ble_history_legacy USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
  CREATE POLICY project_scope ON core.ble_history_legacy AS RESTRICTIVE TO aether_app
   USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
   WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
 END IF;
 IF EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = to_regclass('core.sensor_samples_legacy')) THEN
  ALTER TABLE core.sensor_samples DETACH PARTITION core.sensor_samples_legacy;
  DROP POLICY owner_access ON core.sensor_samples_legacy;
 ELSE
  CREATE TABLE core.sensor_samples_legacy (
   tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
   event_key text NOT NULL, received_at timestamptz NOT NULL, template_id uuid,
   decoder_id text NOT NULL, reading jsonb NOT NULL,
   CONSTRAINT sensor_samples_legacy_pkey PRIMARY KEY(tenant_id,gateway_id,external_id,event_key,received_at),
   FOREIGN KEY(tenant_id,gateway_id,external_id) REFERENCES core.sensor_streams(tenant_id,gateway_id,external_id),
   FOREIGN KEY(tenant_id,template_id) REFERENCES core.device_templates(tenant_id,id)
  );
  CREATE INDEX sensor_samples_legacy_history ON core.sensor_samples_legacy(tenant_id,gateway_id,external_id,received_at DESC,event_key);
  CREATE INDEX sensor_samples_legacy_identity ON core.sensor_samples_legacy(tenant_id,external_id,received_at DESC);
  CREATE INDEX sensor_samples_legacy_received_brin ON core.sensor_samples_legacy USING brin(received_at);
  ALTER TABLE core.sensor_samples_legacy ENABLE ROW LEVEL SECURITY;
  ALTER TABLE core.sensor_samples_legacy FORCE ROW LEVEL SECURITY;
  CREATE POLICY tenant_scope ON core.sensor_samples_legacy USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
  CREATE POLICY project_scope ON core.sensor_samples_legacy AS RESTRICTIVE TO aether_app
   USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
   WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
 END IF;
END $$;
-- +goose StatementEnd
RESET ROLE;
INSERT INTO core.ble_history_legacy SELECT * FROM core.ble_history;
INSERT INTO core.sensor_samples_legacy SELECT * FROM core.sensor_samples;
SET ROLE aether_owner;
DROP TABLE core.ble_history;
DROP TABLE core.sensor_samples;
ALTER TABLE core.ble_history_legacy RENAME TO ble_history;
ALTER TABLE core.ble_history RENAME CONSTRAINT ble_history_legacy_pkey TO ble_history_pkey;
ALTER INDEX core.ble_history_legacy_stream RENAME TO ble_history_stream;
ALTER INDEX core.ble_history_legacy_gateway RENAME TO ble_history_gateway;
ALTER INDEX core.ble_history_legacy_received_brin RENAME TO ble_history_received_brin;
GRANT SELECT,INSERT,DELETE ON core.ble_history TO aether_app;
ALTER TABLE core.sensor_samples_legacy RENAME TO sensor_samples;
ALTER TABLE core.sensor_samples RENAME CONSTRAINT sensor_samples_legacy_pkey TO sensor_samples_pkey;
ALTER INDEX core.sensor_samples_legacy_history RENAME TO sensor_samples_history;
ALTER INDEX core.sensor_samples_legacy_identity RENAME TO sensor_samples_identity;
ALTER INDEX core.sensor_samples_legacy_received_brin RENAME TO sensor_samples_received_brin;
GRANT SELECT,INSERT,DELETE ON core.sensor_samples TO aether_app;
RESET ROLE;
