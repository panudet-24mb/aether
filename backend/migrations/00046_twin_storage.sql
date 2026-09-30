-- Digital twin history (docs/platform/digital-twin.md, phase P2): what the replay reads.
--
--   core.presence_history   every zone transition of a worn tag (a person): the zone it moved to, the zone it came
--                           from, when. Written in the same transaction as the zone event (postgres/alerts.go
--                           updateZone), plus a row with gateway_id NULL when a roaming tag goes offline (heard by no
--                           gateway) and a row when it comes back where it was. Partitioned by month on `at`; kept
--                           core.retention_policy.presence_days (default 30, PRESENCE_HISTORY_DAYS, set only by the
--                           migration role like the access log's retention: the runtime has no DELETE on it). A
--                           member restricted to some projects sees a transition when either end is one of theirs.
--                           Movement history is personal data: every read of people is in the access log, and an
--                           erasure of the tag deletes it (00047). It expires row by row (core.prune_history), not by
--                           whole months, and is not held back by its own newest row (see "Retention" below).
--   core.sample_rollup      5-minute buckets of the readings of fixed sensors (count, avg/min/max temperature and
--                           humidity, any motion, last door state, lowest battery), so replay never aggregates raw
--                           samples. Only tags that are not carried by people: registered, and no live registration
--                           roaming or with a worn / button profile (the caller passes domain.WornProfileIDs(), as for
--                           core.twin_impersonal_tags), so the buckets hold no one's movement. The same rules as
--                           frontend/app/live/measurements.ts decide which reading carries a temperature. Filled by
--                           core.rollup_samples (a background worker, never per uplink: no lock on the ingest path) up
--                           to the last closed bucket, with the previous bucket recomputed on every run for late
--                           arrivals. Partitioned by month on `bucket`; kept the sample retention + 30 days.
--   core.rollup_watermark   how far rollup_samples has come (one row, owner-only).
--   core.twin_settings      per workspace, owner-written: how people may be shown (off | counts | tracks | named) and
--                           how far back, and what wall displays may show.
--
-- The partition functions (last replaced in 00038) learn the two tables, keeping every guard: UTC, the data-anchored
-- drop, one step per call, the lock timeouts, the access log's policy-row retention.
--
-- Retention of the movement history: rows older than presence_days are deleted in batches by core.prune_history
-- (every maintenance run, hourly) and whole expired months are dropped by core.maintain_partitions. The data-anchored
-- hold of the other tables (a range goes only when a newer stored row proves it expired) would keep personal data
-- for as long as nobody moves, so personal tables use a different sanity bound: the clock is trusted when some uplink
-- (core.sensor_samples or core.ble_history) was stored in the last 7 days (core.clock_corroborated). A clock that
-- jumped ahead on an idle system therefore expires nothing, and after a week with no uplink at all the expiry of
-- movement history pauses until the next one (maintenance reports drop_held). Effective retention: presence_days,
-- plus up to one maintenance interval.
--
-- core.sensor_samples also refuses, for the runtime role, a row received more than 15 minutes before the
-- transaction began unless the workspace is a demo: backdating is the demo backfill's (CapturePacketAt), nobody
-- else's.

-- +goose Up
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';

ALTER TABLE core.retention_policy ADD COLUMN presence_days integer NOT NULL DEFAULT 30 CHECK(presence_days BETWEEN 1 AND 400);

-- +goose StatementBegin
CREATE FUNCTION core.set_presence_history_retention(days integer) RETURNS integer
 LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 UPDATE core.retention_policy SET presence_days = days, updated_at = now() WHERE id RETURNING presence_days
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.set_presence_history_retention(integer) FROM PUBLIC;
-- The retention the twin shows its users (the policy row itself stays unreadable to the runtime).
-- +goose StatementBegin
CREATE FUNCTION core.presence_history_days() RETURNS integer
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT coalesce(max(r.presence_days), 30) FROM core.retention_policy r
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.presence_history_days() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.presence_history_days() TO aether_app;

CREATE TABLE core.presence_history (
 tenant_id uuid NOT NULL,
 external_id text NOT NULL CHECK(length(external_id) BETWEEN 1 AND 128 AND external_id = lower(external_id)),
 at timestamptz NOT NULL,
 gateway_id uuid,
 from_gateway_id uuid,
 rssi_avg real,
 CONSTRAINT presence_history_pkey PRIMARY KEY(tenant_id, external_id, at)
) PARTITION BY RANGE (at);
CREATE INDEX presence_history_recent ON core.presence_history(tenant_id, at);
CREATE INDEX presence_history_at_brin ON core.presence_history USING brin(at);
ALTER TABLE core.presence_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.presence_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.presence_history USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.presence_history AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id) OR core.gateway_in_scope(from_gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id) OR core.gateway_in_scope(from_gateway_id));
CREATE POLICY owner_access ON core.presence_history TO aether_owner USING(true) WITH CHECK(true);
GRANT SELECT, INSERT ON core.presence_history TO aether_app;
CREATE TABLE core.presence_history_default PARTITION OF core.presence_history DEFAULT;
ALTER TABLE core.presence_history_default ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.presence_history_default FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_access ON core.presence_history_default TO aether_owner USING(true) WITH CHECK(true);

CREATE TABLE core.sample_rollup (
 tenant_id uuid NOT NULL,
 gateway_id uuid NOT NULL,
 external_id text NOT NULL CHECK(external_id = lower(external_id)),
 bucket timestamptz NOT NULL,
 n integer NOT NULL CHECK(n >= 0),
 t_avg real, t_min real, t_max real,
 h_avg real, h_min real, h_max real,
 motion smallint, door smallint, battery_min real,
 CONSTRAINT sample_rollup_pkey PRIMARY KEY(tenant_id, gateway_id, external_id, bucket)
) PARTITION BY RANGE (bucket);
CREATE INDEX sample_rollup_identity ON core.sample_rollup(tenant_id, external_id, bucket);
CREATE INDEX sample_rollup_bucket_brin ON core.sample_rollup USING brin(bucket);
ALTER TABLE core.sample_rollup ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.sample_rollup FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.sample_rollup USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.sample_rollup AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
CREATE POLICY owner_access ON core.sample_rollup TO aether_owner USING(true) WITH CHECK(true);
GRANT SELECT ON core.sample_rollup TO aether_app;
CREATE TABLE core.sample_rollup_default PARTITION OF core.sample_rollup DEFAULT;
ALTER TABLE core.sample_rollup_default ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.sample_rollup_default FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_access ON core.sample_rollup_default TO aether_owner USING(true) WITH CHECK(true);

CREATE TABLE core.rollup_watermark (
 id boolean PRIMARY KEY DEFAULT true CHECK(id),
 done_until timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE core.rollup_watermark ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.rollup_watermark FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_access ON core.rollup_watermark TO aether_owner USING(true) WITH CHECK(true);
INSERT INTO core.rollup_watermark DEFAULT VALUES;

CREATE TABLE core.twin_settings (
 tenant_id uuid PRIMARY KEY REFERENCES core.tenants(id),
 people_replay text NOT NULL DEFAULT 'counts' CHECK(people_replay IN ('off','counts','tracks','named')),
 people_replay_days integer NOT NULL DEFAULT 7 CHECK(people_replay_days BETWEEN 1 AND 90),
 display_people text NOT NULL DEFAULT 'counts' CHECK(display_people IN ('off','counts','tracks')),
 updated_by uuid,
 updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE core.twin_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.twin_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.twin_settings USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
-- Everyone in the workspace reads the settings (they decide what the twin shows them); only an owner writes them.
CREATE POLICY owner_insert ON core.twin_settings AS RESTRICTIVE FOR INSERT TO aether_app WITH CHECK((SELECT core.is_tenant_owner()));
CREATE POLICY owner_update ON core.twin_settings AS RESTRICTIVE FOR UPDATE TO aether_app
 USING((SELECT core.is_tenant_owner())) WITH CHECK((SELECT core.is_tenant_owner()));
GRANT SELECT, INSERT, UPDATE ON core.twin_settings TO aether_app;

-- A number from a reading, or NULL when the value is absent or not a number. No SET clause (the name is qualified
-- instead): a function with one is never inlined, and the rollup calls this ten times per sample.
-- +goose StatementBegin
CREATE FUNCTION core.twin_num(v jsonb) RETURNS double precision
 LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE WHEN pg_catalog.jsonb_typeof(v) = 'number' THEN v::double precision END
$$;
-- +goose StatementEnd

-- core.rollup_range recomputes the 5-minute buckets of the given workspaces in [lo, hi) from core.sensor_samples,
-- in one pass over the range (the partitions it overlaps are read directly, as the partition functions do: the
-- parent's policies would limit this role to one workspace at a time). Only fixed sensors are rolled up: a tag is
-- one when it is registered and none of its live registrations is roaming or has one of worn_profiles (the rule of
-- core.twin_impersonal_tags); buckets in the range of any other tag are deleted, so a recomputed range holds exactly
-- the fixed ones. The reading rules mirror frontend/app/live/measurements.ts and postgres/twin.go mergeTwinReadings:
--   a Zigbee2MQTT-shaped reading (frames has z2m-state@1; Tuya readings carry it too) takes temperature, humidity and
--   battery from `metrics`; any other reading carries temperature and humidity only when it is an environment frame
--   (minew-ffe1-a101@1, kind 'environment', or a reading with neither kind nor frames), and a battery only above 0;
--   motion is metrics.motion >= 1, door metrics.door >= 1 (the last one in the bucket).
-- It runs only under a statement timeout: a function's own SET statement_timeout does not arm the timer of the
-- statement that called it, so the callers set one (SET LOCAL) and this refuses to run without.
-- Not callable by the runtime; rollup_samples and rollup_samples_range call it.
-- +goose StatementBegin
CREATE FUNCTION core.rollup_range(tenants uuid[], lo timestamptz, hi timestamptz, worn_profiles text[]) RETURNS bigint
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER
 SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC' SET work_mem = '64MB'
AS $$
#variable_conflict use_variable
DECLARE
 n bigint; t uuid; p record; src text := ''; fixed_t uuid[] := '{}'; fixed_e text[] := '{}';
 caller_tenant text := current_setting('app.tenant_id', true);
BEGIN
 IF coalesce(current_setting('statement_timeout', true), '0') = '0' THEN
  RAISE EXCEPTION 'core.rollup_range runs only under a statement_timeout' USING ERRCODE = 'object_not_in_prerequisite_state';
 END IF;
 IF worn_profiles IS NULL OR cardinality(worn_profiles) = 0 THEN
  RAISE EXCEPTION 'core.rollup_range needs the worn profiles (domain.WornProfileIDs)' USING ERRCODE = 'invalid_parameter_value';
 END IF;
 IF tenants IS NULL OR cardinality(tenants) = 0 OR lo IS NULL OR hi IS NULL OR hi <= lo THEN
  RETURN 0;
 END IF;
 -- The fixed sensors, one workspace at a time (core.devices is under row level security for this role too).
 FOREACH t IN ARRAY tenants LOOP
  PERFORM set_config('app.tenant_id', t::text, true);
  SELECT fixed_t || coalesce(array_agg(x.tenant_id), '{}'), fixed_e || coalesce(array_agg(x.ext), '{}') INTO fixed_t, fixed_e
  FROM (SELECT d.tenant_id, lower(d.external_id) AS ext FROM core.devices d
   WHERE d.tenant_id = t AND d.removed_at IS NULL
   GROUP BY d.tenant_id, lower(d.external_id)
   HAVING bool_and(NOT d.roaming AND NOT (d.profile_id = ANY(worn_profiles)))) x;
 END LOOP;
 PERFORM set_config('app.tenant_id', coalesce(caller_tenant, ''), true);
 FOR p IN SELECT r.part FROM core.partition_ranges('sensor_samples') r
   WHERE r.is_default OR ((r.hi IS NULL OR r.hi > lo) AND (r.lo IS NULL OR r.lo < hi)) ORDER BY r.lo NULLS FIRST
 LOOP
  src := src || CASE WHEN src = '' THEN '' ELSE ' UNION ALL ' END || format(
   'SELECT tenant_id, gateway_id, external_id, received_at, reading FROM %s WHERE received_at >= $1 AND received_at < $2 AND received_at <= now() AND tenant_id = ANY($3)', p.part);
 END LOOP;
 EXECUTE 'DELETE FROM core.sample_rollup r WHERE r.tenant_id = ANY($3) AND r.bucket >= $1 AND r.bucket < $2
   AND NOT EXISTS (SELECT 1 FROM unnest($4::uuid[], $5::text[]) f(tenant_id, ext) WHERE f.tenant_id = r.tenant_id AND f.ext = r.external_id)'
  USING lo, hi, tenants, fixed_t, fixed_e;
 IF src = '' OR cardinality(fixed_t) = 0 THEN
  RETURN 0;
 END IF;
 EXECUTE format($q$
 INSERT INTO core.sample_rollup AS r(tenant_id,gateway_id,external_id,bucket,n,t_avg,t_min,t_max,h_avg,h_min,h_max,motion,door,battery_min)
 SELECT v.tenant_id, v.gateway_id, v.external_id, v.bucket, count(*)::integer,
  avg(v.t)::real, min(v.t)::real, max(v.t)::real, avg(v.h)::real, min(v.h)::real, max(v.h)::real,
  max(v.motion)::smallint,
  ((array_agg(v.door ORDER BY v.received_at DESC) FILTER (WHERE v.door IS NOT NULL))[1])::smallint,
  min(v.battery)::real
 FROM (
  SELECT s.tenant_id, s.gateway_id, lower(s.external_id) AS external_id, s.received_at,
   date_bin(interval '5 minutes', s.received_at, timestamptz '2000-01-01 00:00:00+00') AS bucket,
   CASE WHEN k.z THEN core.twin_num(s.reading->'metrics'->'temperature') WHEN k.env THEN core.twin_num(s.reading->'temperature') END AS t,
   CASE WHEN k.z THEN core.twin_num(s.reading->'metrics'->'humidity') WHEN k.env THEN core.twin_num(s.reading->'humidity') END AS h,
   CASE WHEN k.z THEN core.twin_num(s.reading->'metrics'->'battery')
        WHEN core.twin_num(s.reading->'battery') > 0 THEN core.twin_num(s.reading->'battery') END AS battery,
   CASE WHEN core.twin_num(s.reading->'metrics'->'motion') >= 1 THEN 1 ELSE 0 END AS motion,
   CASE WHEN core.twin_num(s.reading->'metrics'->'door') IS NULL THEN NULL
        WHEN core.twin_num(s.reading->'metrics'->'door') >= 1 THEN 1 ELSE 0 END AS door
  FROM (%s) s
  JOIN unnest($4::uuid[], $5::text[]) f(tenant_id, ext) ON f.tenant_id = s.tenant_id AND f.ext = lower(s.external_id)
  CROSS JOIN LATERAL (SELECT
    coalesce(jsonb_typeof(s.reading->'frames') = 'array' AND s.reading->'frames' ? 'z2m-state@1', false) AS z,
    coalesce(jsonb_typeof(s.reading->'frames') = 'array' AND s.reading->'frames' ? 'minew-ffe1-a101@1', false)
     OR coalesce(s.reading->>'kind', '') = 'environment'
     OR (coalesce(s.reading->>'kind', '') = '' AND coalesce(CASE WHEN jsonb_typeof(s.reading->'frames') = 'array' THEN jsonb_array_length(s.reading->'frames') END, 0) = 0) AS env
  ) k
 ) v
 GROUP BY v.tenant_id, v.gateway_id, v.external_id, v.bucket
 ON CONFLICT (tenant_id, gateway_id, external_id, bucket) DO UPDATE SET n = EXCLUDED.n,
  t_avg = EXCLUDED.t_avg, t_min = EXCLUDED.t_min, t_max = EXCLUDED.t_max,
  h_avg = EXCLUDED.h_avg, h_min = EXCLUDED.h_min, h_max = EXCLUDED.h_max,
  motion = EXCLUDED.motion, door = EXCLUDED.door, battery_min = EXCLUDED.battery_min
 $q$, src) USING lo, hi, tenants, fixed_t, fixed_e;
 GET DIAGNOSTICS n = ROW_COUNT;
 RETURN n;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.rollup_range(uuid[], timestamptz, timestamptz, text[]) FROM PUBLIC;

-- The worker's step, over every active workspace in one pass: from the watermark (minus one bucket, recomputed for
-- late arrivals) up to the last bucket that closed at least a minute ago, at most max_span per call. The first run
-- starts backfill_days back (the sample retention), never before the oldest rollup range (rows there would sit in
-- DEFAULT). One run at a time (advisory lock); returns the buckets written and how far (seconds) it still is behind
-- the live edge, so the worker knows whether to run the next step soon. The caller sets a statement timeout.
-- +goose StatementBegin
CREATE FUNCTION core.rollup_samples(max_span interval, backfill_days integer, worn_profiles text[]) RETURNS TABLE(buckets bigint, behind_sec bigint)
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER
 SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC' SET lock_timeout = '3s'
AS $$
DECLARE
 origin constant timestamptz := timestamptz '2000-01-01 00:00:00+00';
 wm timestamptz; lo timestamptz; hi timestamptz; edge timestamptz; floor_at timestamptz; tenants uuid[]; total bigint;
BEGIN
 IF NOT pg_try_advisory_xact_lock(hashtextextended('aether.rollup_samples', 0)) THEN
  RETURN QUERY SELECT 0::bigint, 0::bigint;
  RETURN;
 END IF;
 max_span := least(greatest(coalesce(max_span, interval '6 hours'), interval '5 minutes'), interval '24 hours');
 backfill_days := least(greatest(coalesce(backfill_days, 90), 1), 3650);
 SELECT w.done_until INTO wm FROM core.rollup_watermark w WHERE w.id FOR UPDATE;
 edge := date_bin(interval '5 minutes', now() - interval '1 minute', origin);
 hi := edge;
 floor_at := date_bin(interval '5 minutes', now() - make_interval(days => backfill_days), origin);
 SELECT greatest(floor_at, min(p.lo)) INTO floor_at FROM core.partition_ranges('sample_rollup') p WHERE NOT p.is_default AND p.lo IS NOT NULL;
 lo := greatest(coalesce(wm - interval '5 minutes', floor_at), floor_at);
 IF lo >= hi THEN
  RETURN QUERY SELECT 0::bigint, 0::bigint;
  RETURN;
 END IF;
 hi := least(hi, lo + max_span);
 SELECT coalesce(array_agg(tn.id ORDER BY tn.id), '{}') INTO tenants FROM core.tenants tn WHERE tn.status = 'active';
 total := core.rollup_range(tenants, lo, hi, worn_profiles);
 UPDATE core.rollup_watermark SET done_until = hi, updated_at = now() WHERE id;
 RETURN QUERY SELECT total, extract(epoch FROM (edge - hi))::bigint;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.rollup_samples(interval, integer, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.rollup_samples(interval, integer, text[]) TO aether_app;

-- Recompute a range of the caller's own workspace now (at most 7 days, bucket-aligned): for history written in the
-- past (the demo backfill) behind the worker's watermark. Idempotent; only the current workspace's rows.
-- +goose StatementBegin
CREATE FUNCTION core.rollup_samples_range(lo timestamptz, hi timestamptz, worn_profiles text[]) RETURNS bigint
 LANGUAGE plpgsql VOLATILE SECURITY DEFINER
 SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC'
AS $$
DECLARE origin constant timestamptz := timestamptz '2000-01-01 00:00:00+00'; tenant uuid := core.tenant_id();
BEGIN
 IF tenant IS NULL OR lo IS NULL OR hi IS NULL OR hi <= lo OR hi - lo > interval '7 days' THEN
  RETURN 0;
 END IF;
 RETURN core.rollup_range(ARRAY[tenant], date_bin(interval '5 minutes', lo, origin), date_bin(interval '5 minutes', hi, origin) + interval '5 minutes', worn_profiles);
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.rollup_samples_range(timestamptz, timestamptz, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.rollup_samples_range(timestamptz, timestamptz, text[]) TO aether_app;

-- The clock is corroborated when some uplink was stored in the last 7 days: the sanity bound under which personal
-- data (the movement history) expires by the clock alone (see the header).
-- +goose StatementBegin
CREATE FUNCTION core.clock_corroborated() RETURNS boolean
 LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp AS $$
 SELECT core.partition_has_rows_between('sensor_samples', now() - interval '7 days')
  OR core.partition_has_rows_between('ble_history', now() - interval '7 days')
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.clock_corroborated() FROM PUBLIC;

-- Backdated samples are the demo's alone (the header).
-- +goose StatementBegin
CREATE FUNCTION core.refuse_backdated_sample() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
 IF NOT coalesce((SELECT t.demo FROM core.tenants t WHERE t.id = NEW.tenant_id), false) THEN
  RAISE EXCEPTION 'a sample received at % is backdated; only a demo workspace stores past history', NEW.received_at
   USING ERRCODE = 'check_violation';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.refuse_backdated_sample() FROM PUBLIC;
CREATE TRIGGER sensor_samples_not_backdated BEFORE INSERT ON core.sensor_samples FOR EACH ROW
 WHEN (NEW.received_at < now() - interval '15 minutes' AND current_user = 'aether_app')
 EXECUTE FUNCTION core.refuse_backdated_sample();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION core.partition_has_rows_between(parent text, since timestamptz)
RETURNS boolean
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC'
AS $$
DECLARE p record; found boolean;
 col text := CASE parent WHEN 'access_log' THEN 'at' WHEN 'presence_history' THEN 'at' WHEN 'sample_rollup' THEN 'bucket' ELSE 'received_at' END;
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
 access_days integer; presence_days integer;
BEGIN
 IF NOT pg_try_advisory_xact_lock(hashtextextended('aether.maintain_partitions', 0)) THEN
  RETURN;
 END IF;
 sample_days := least(greatest(coalesce(sample_days, 90), 1), 3650);
 ble_hours := least(greatest(coalesce(ble_hours, 24), 1), 720);
 -- The trail's and the movement history's retention are the policy row's, never the caller's (see 00038, 00046).
 SELECT coalesce(max(r.access_days), 400), coalesce(max(r.presence_days), 30) INTO access_days, presence_days FROM core.retention_policy r;
 FOR spec IN SELECT * FROM (VALUES
   ('ble_history', 'day', interval '1 day', interval '14 days', make_interval(hours => ble_hours), 'core.gateways', 'received_at'),
   ('sensor_samples', 'week', interval '7 days', interval '28 days', make_interval(days => sample_days), 'core.sensor_streams, core.device_templates', 'received_at'),
   ('access_log', 'month', interval '1 month', interval '62 days', make_interval(days => access_days), NULL, 'at'),
   ('presence_history', 'month', interval '1 month', interval '62 days', make_interval(days => presence_days), NULL, 'at'),
   ('sample_rollup', 'month', interval '1 month', interval '62 days', make_interval(days => sample_days + 30), NULL, 'bucket')
  ) AS s(tbl, unit, step, ahead, retention, refs, col)
 LOOP
  held := false; deferred := false;
  FOR expired, expired_hi IN SELECT p.part, p.hi FROM core.partition_ranges(spec.tbl) p
    WHERE coalesce(allow_drop, false) AND NOT p.is_default AND p.hi <= now() - spec.retention ORDER BY p.hi
  LOOP
   relation := expired::text; moved := 0; -- named before the drop: a dropped regclass prints as its OID
   EXECUTE format('SELECT EXISTS (SELECT 1 FROM %s)', expired) INTO occupied;
   -- Personal data (see the header) is not held by its own newest row, only by core.clock_corroborated.
   IF occupied AND NOT (CASE WHEN spec.tbl = 'presence_history' THEN core.clock_corroborated()
     ELSE core.partition_has_rows_between(spec.tbl, expired_hi + spec.retention) END) THEN
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
 target record; n bigint; total bigint := 0; access_days integer; presence_days integer;
BEGIN
 IF NOT pg_try_advisory_xact_lock(hashtextextended('aether.prune_history', 0)) THEN
  RETURN 0;
 END IF;
 sample_days := least(greatest(coalesce(sample_days, 90), 1), 3650);
 ble_hours := least(greatest(coalesce(ble_hours, 24), 1), 720);
 SELECT coalesce(max(r.access_days), 400), coalesce(max(r.presence_days), 30) INTO access_days, presence_days FROM core.retention_policy r;
 batch := least(greatest(coalesce(batch, 20000), 1), 50000);
 FOR target IN SELECT * FROM (VALUES
   ('ble_history_legacy', 'received_at', now() - make_interval(hours => ble_hours)),
   ('ble_history_default', 'received_at', now() - make_interval(hours => ble_hours)),
   ('sensor_samples_legacy', 'received_at', now() - make_interval(days => sample_days)),
   ('sensor_samples_default', 'received_at', now() - make_interval(days => sample_days)),
   ('access_log_default', 'at', now() - make_interval(days => access_days)),
   ('sample_rollup_default', 'bucket', now() - make_interval(days => sample_days + 30))
  ) AS t(rel, col, cutoff)
 LOOP
  CONTINUE WHEN to_regclass(format('core.%I', target.rel)) IS NULL;
  EXECUTE format('DELETE FROM core.%I WHERE ctid = ANY (ARRAY(SELECT ctid FROM core.%I WHERE %I < %L LIMIT %s))',
   target.rel, target.rel, target.col, target.cutoff, batch);
  GET DIAGNOSTICS n = ROW_COUNT;
  total := total + n;
 END LOOP;
 -- The movement history expires row by row across every range (by its key: a ctid names a row only within one
 -- partition), under the sanity bound of the header.
 IF core.clock_corroborated() THEN
  DELETE FROM core.presence_history h USING (SELECT x.tenant_id, x.external_id, x.at FROM core.presence_history x
    WHERE x.at < now() - make_interval(days => presence_days) LIMIT batch) d
   WHERE h.tenant_id = d.tenant_id AND h.external_id = d.external_id AND h.at = d.at;
  GET DIAGNOSTICS n = ROW_COUNT;
  total := total + n;
 END IF;
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
 FOREACH t IN ARRAY ARRAY['ble_history','sensor_samples','access_log','presence_history','sample_rollup'] LOOP
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

-- The first ranges of the two tables (with the default sample retention; nothing is dropped here).
-- +goose StatementBegin
DO $$ DECLARE n int := 0; BEGIN
 LOOP
  EXIT WHEN NOT EXISTS (SELECT 1 FROM core.maintain_partitions(90, 24, false) m WHERE m.action = 'created');
  n := n + 1;
  IF n > 100 THEN
   RAISE EXCEPTION 'partition set-up did not converge';
  END IF;
 END LOOP;
END $$;
-- +goose StatementEnd
RESET ROLE;

-- Movement history so far: the zone events still in the (capped) event log, from the oldest range on. Run by the
-- migration role, which row level security does not restrict (every workspace at once).
-- +goose StatementBegin
DO $$ DECLARE since timestamptz; BEGIN
 SELECT min(p.lo) INTO since FROM core.partition_ranges('presence_history') p WHERE NOT p.is_default AND p.lo IS NOT NULL;
 INSERT INTO core.presence_history(tenant_id, external_id, at, gateway_id, from_gateway_id, rssi_avg)
 SELECT e.tenant_id, lower(e.external_id), e.occurred_at,
  CASE WHEN e.detail->>'to_gateway_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN (e.detail->>'to_gateway_id')::uuid END,
  CASE WHEN e.detail->>'from_gateway_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN (e.detail->>'from_gateway_id')::uuid END,
  core.twin_num(e.detail->'rssi_avg')::real
 FROM core.device_events e
 WHERE e.event_type = 'zone' AND e.occurred_at >= coalesce(since, now()) AND e.occurred_at <= now()
   AND e.detail->>'to_gateway_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
   AND length(e.external_id) BETWEEN 1 AND 128
 ON CONFLICT DO NOTHING;
END $$;
-- +goose StatementEnd

-- +goose Down
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';
DROP TRIGGER sensor_samples_not_backdated ON core.sensor_samples;
DROP FUNCTION core.refuse_backdated_sample();
DROP TABLE core.twin_settings;
DROP FUNCTION core.rollup_samples_range(timestamptz, timestamptz, text[]);
DROP FUNCTION core.rollup_samples(interval, integer, text[]);
DROP FUNCTION core.rollup_range(uuid[], timestamptz, timestamptz, text[]);
DROP FUNCTION core.twin_num(jsonb);
DROP TABLE core.rollup_watermark;
DROP TABLE core.sample_rollup;
DROP TABLE core.presence_history;
DROP FUNCTION core.presence_history_days();
DROP FUNCTION core.set_presence_history_retention(integer);
ALTER TABLE core.retention_policy DROP COLUMN presence_days;

-- The partition functions as 00038 left them.
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
-- Only now: the functions restored above no longer call it.
DROP FUNCTION core.clock_corroborated();
RESET ROLE;
