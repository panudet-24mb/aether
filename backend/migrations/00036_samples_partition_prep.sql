-- +goose NO TRANSACTION
-- Preparation for time-partitioning core.ble_history (daily) and core.sensor_samples (weekly), done online.
--
-- A partitioned table's primary key must contain the partition key, so each table first gets a primary key that
-- also holds received_at, built CONCURRENTLY (ingest keeps writing) and swapped in with a lock of milliseconds.
-- Then a CHECK (received_at < X) is added NOT VALID and validated under SHARE UPDATE EXCLUSIVE (ingest keeps
-- writing): 00037 attaches each whole table as the "legacy" partition FROM (MINVALUE) TO (X), and that valid
-- CHECK lets ATTACH skip scanning it. X is stored as the comment of the constraint for 00037 to read.
--
-- X is the Monday 00:00 UTC two weeks after the start of the current week, so 7 to 14 days ahead, for samples,
-- and midnight UTC 7 days ahead for BLE history. The two migrations normally run in the same `migrate`
-- invocation, seconds apart. If 00037 ever lags behind X, inserts with received_at >= X violate the CHECK and
-- ingest stalls; docs/production.md (upgrade section) has the runbook: drop the constraints (instant), re-add
-- them with a later X, then run migrate again.
--
-- Lock order. Every statement below locks ONE table and runs as its own transaction (NO TRANSACTION: goose
-- executes each statement separately, in autocommit). The tables are handled in the order a Minew packet
-- (CapturePacket) writes them, ble_history before sensor_samples, and no statement ever holds a lock on one while
-- waiting for the other, so no ingest transaction can close a cycle with this migration. Each lock waits at most
-- 300 ms (so writers queued behind it wait no longer) and is retried up to 200 times, about 100 s, with a NOTICE
-- per retry; a deadlock, should the detector pick this side anyway, is retried as well.
--
-- NO TRANSACTION also means each statement may run on a different pooled connection, so nothing relies on
-- session state (no SET ROLE): the migration role is the superuser, and an index or constraint belongs to its
-- table's owner (aether_owner) whoever creates it. Every statement is idempotent: after a failure part-way, the
-- same migration is simply run again. A table whose primary key already holds received_at (swapped by an earlier
-- run) gets a placeholder sequence under the index name, so CREATE INDEX CONCURRENTLY IF NOT EXISTS skips it
-- instead of building an index that would only be dropped again; the swap step removes the placeholder.

-- +goose Up
-- Per table: drop an INVALID index a failed concurrent build left behind, or reserve the name when the swap is done.
-- +goose StatementBegin
DO $$ DECLARE t text := 'ble_history'; attempt int; BEGIN
 FOR attempt IN 1..200 LOOP
  BEGIN
   SET LOCAL lock_timeout = '300ms';
   IF EXISTS (SELECT 1 FROM pg_index i WHERE i.indexrelid = to_regclass(format('core.%I', t || '_pkey_v2')) AND NOT i.indisvalid) THEN
    EXECUTE format('DROP INDEX core.%I', t || '_pkey_v2');
   END IF;
   IF to_regclass(format('core.%I', t || '_pkey_v2')) IS NULL AND EXISTS (SELECT 1 FROM pg_constraint c
     JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
     WHERE c.conrelid = format('core.%I', t)::regclass AND c.contype = 'p' AND a.attname = 'received_at') THEN
    EXECUTE format('CREATE SEQUENCE core.%I', t || '_pkey_v2');
   END IF;
   EXIT;
  EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
   IF attempt = 200 THEN RAISE; END IF;
   RAISE NOTICE '00036: core.% busy, retrying (attempt %)', t, attempt;
   PERFORM pg_sleep(0.2);
  END;
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ DECLARE t text := 'sensor_samples'; attempt int; BEGIN
 FOR attempt IN 1..200 LOOP
  BEGIN
   SET LOCAL lock_timeout = '300ms';
   IF EXISTS (SELECT 1 FROM pg_index i WHERE i.indexrelid = to_regclass(format('core.%I', t || '_pkey_v2')) AND NOT i.indisvalid) THEN
    EXECUTE format('DROP INDEX core.%I', t || '_pkey_v2');
   END IF;
   IF to_regclass(format('core.%I', t || '_pkey_v2')) IS NULL AND EXISTS (SELECT 1 FROM pg_constraint c
     JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
     WHERE c.conrelid = format('core.%I', t)::regclass AND c.contype = 'p' AND a.attname = 'received_at') THEN
    EXECUTE format('CREATE SEQUENCE core.%I', t || '_pkey_v2');
   END IF;
   EXIT;
  EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
   IF attempt = 200 THEN RAISE; END IF;
   RAISE NOTICE '00036: core.% busy, retrying (attempt %)', t, attempt;
   PERFORM pg_sleep(0.2);
  END;
 END LOOP;
END $$;
-- +goose StatementEnd
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ble_history_pkey_v2 ON core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at);
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS sensor_samples_pkey_v2 ON core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at);

-- Per table, one transaction each: swap the primary key (ACCESS EXCLUSIVE for milliseconds), store X and add the
-- CHECK. X is computed in UTC whatever the server's time zone.
-- +goose StatementBegin
DO $$
DECLARE t text := 'ble_history'; x timestamptz; attempt int; swapped boolean;
BEGIN
 SET LOCAL TimeZone = 'UTC';
 x := date_trunc('day', now(), 'UTC') + interval '7 days';
 FOR attempt IN 1..200 LOOP
  BEGIN
   SET LOCAL lock_timeout = '300ms';
   EXECUTE format('LOCK TABLE core.%I IN ACCESS EXCLUSIVE MODE', t);
   SELECT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
     WHERE c.conrelid = format('core.%I', t)::regclass AND c.contype = 'p' AND a.attname = 'received_at') INTO swapped;
   IF NOT swapped THEN
    EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT %I', t, t || '_pkey');
    EXECUTE format('ALTER TABLE core.%I ADD CONSTRAINT %I PRIMARY KEY USING INDEX %I', t, t || '_pkey', t || '_pkey_v2');
   ELSIF EXISTS (SELECT 1 FROM pg_class WHERE oid = to_regclass(format('core.%I', t || '_pkey_v2')) AND relkind = 'S') THEN
    EXECUTE format('DROP SEQUENCE core.%I', t || '_pkey_v2');
   ELSIF to_regclass(format('core.%I', t || '_pkey_v2')) IS NOT NULL THEN
    EXECUTE format('DROP INDEX core.%I', t || '_pkey_v2');
   END IF;
   IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = format('core.%I', t)::regclass AND conname = t || '_cutover') THEN
    EXECUTE format('ALTER TABLE core.%I ADD CONSTRAINT %I CHECK (received_at < %L) NOT VALID', t, t || '_cutover', x);
    EXECUTE format('COMMENT ON CONSTRAINT %I ON core.%I IS %L', t || '_cutover', t, to_char(x AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'));
   END IF;
   EXIT;
  EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
   IF attempt = 200 THEN RAISE; END IF;
   RAISE NOTICE '00036: core.% busy, retrying (attempt %)', t, attempt;
   PERFORM pg_sleep(0.2);
  END;
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
DECLARE t text := 'sensor_samples'; x timestamptz; attempt int; swapped boolean;
BEGIN
 SET LOCAL TimeZone = 'UTC';
 x := date_trunc('week', now(), 'UTC') + interval '14 days';
 FOR attempt IN 1..200 LOOP
  BEGIN
   SET LOCAL lock_timeout = '300ms';
   EXECUTE format('LOCK TABLE core.%I IN ACCESS EXCLUSIVE MODE', t);
   SELECT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
     WHERE c.conrelid = format('core.%I', t)::regclass AND c.contype = 'p' AND a.attname = 'received_at') INTO swapped;
   IF NOT swapped THEN
    EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT %I', t, t || '_pkey');
    EXECUTE format('ALTER TABLE core.%I ADD CONSTRAINT %I PRIMARY KEY USING INDEX %I', t, t || '_pkey', t || '_pkey_v2');
   ELSIF EXISTS (SELECT 1 FROM pg_class WHERE oid = to_regclass(format('core.%I', t || '_pkey_v2')) AND relkind = 'S') THEN
    EXECUTE format('DROP SEQUENCE core.%I', t || '_pkey_v2');
   ELSIF to_regclass(format('core.%I', t || '_pkey_v2')) IS NOT NULL THEN
    EXECUTE format('DROP INDEX core.%I', t || '_pkey_v2');
   END IF;
   IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = format('core.%I', t)::regclass AND conname = t || '_cutover') THEN
    EXECUTE format('ALTER TABLE core.%I ADD CONSTRAINT %I CHECK (received_at < %L) NOT VALID', t, t || '_cutover', x);
    EXECUTE format('COMMENT ON CONSTRAINT %I ON core.%I IS %L', t || '_cutover', t, to_char(x AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'));
   END IF;
   EXIT;
  EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
   IF attempt = 200 THEN RAISE; END IF;
   RAISE NOTICE '00036: core.% busy, retrying (attempt %)', t, attempt;
   PERFORM pg_sleep(0.2);
  END;
 END LOOP;
END $$;
-- +goose StatementEnd
-- Scans each table under SHARE UPDATE EXCLUSIVE: reads and inserts continue. A no-op once validated.
ALTER TABLE core.ble_history VALIDATE CONSTRAINT ble_history_cutover;
ALTER TABLE core.sensor_samples VALIDATE CONSTRAINT sensor_samples_cutover;

-- +goose Down
-- Back to the key without received_at, one table per transaction in ingest order (ble_history, then
-- sensor_samples). Rows that differ only in received_at are collapsed to the earliest first, or the narrower key
-- could not be rebuilt: with the wider key, the same event key can be stored again once the 24 h redelivery
-- window has passed (a Minew tag repeating an identical payload a day later, a redelivery after a day), and the old
-- key never allowed that. The rows removed are exact repeats of a kept row's event key.
-- +goose StatementBegin
DO $$ DECLARE t text := 'ble_history'; BEGIN
 SET LOCAL lock_timeout = '10s';
 EXECUTE format('LOCK TABLE core.%I IN ACCESS EXCLUSIVE MODE', t);
 EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT IF EXISTS %I', t, t || '_cutover');
 IF EXISTS (SELECT 1 FROM pg_class WHERE oid = to_regclass(format('core.%I', t || '_pkey_v2')) AND relkind = 'S') THEN
  EXECUTE format('DROP SEQUENCE core.%I', t || '_pkey_v2');
 ELSIF to_regclass(format('core.%I', t || '_pkey_v2')) IS NOT NULL THEN
  EXECUTE format('DROP INDEX core.%I', t || '_pkey_v2');
 END IF;
 EXECUTE format('DELETE FROM core.%I d USING core.%I k WHERE d.tenant_id = k.tenant_id AND d.gateway_id = k.gateway_id
   AND d.external_id = k.external_id AND d.event_key = k.event_key AND d.received_at > k.received_at', t, t);
 EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT %I', t, t || '_pkey');
 EXECUTE format('ALTER TABLE core.%I ADD CONSTRAINT %I PRIMARY KEY (tenant_id,gateway_id,external_id,event_key)', t, t || '_pkey');
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ DECLARE t text := 'sensor_samples'; BEGIN
 SET LOCAL lock_timeout = '10s';
 EXECUTE format('LOCK TABLE core.%I IN ACCESS EXCLUSIVE MODE', t);
 EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT IF EXISTS %I', t, t || '_cutover');
 IF EXISTS (SELECT 1 FROM pg_class WHERE oid = to_regclass(format('core.%I', t || '_pkey_v2')) AND relkind = 'S') THEN
  EXECUTE format('DROP SEQUENCE core.%I', t || '_pkey_v2');
 ELSIF to_regclass(format('core.%I', t || '_pkey_v2')) IS NOT NULL THEN
  EXECUTE format('DROP INDEX core.%I', t || '_pkey_v2');
 END IF;
 EXECUTE format('DELETE FROM core.%I d USING core.%I k WHERE d.tenant_id = k.tenant_id AND d.gateway_id = k.gateway_id
   AND d.external_id = k.external_id AND d.event_key = k.event_key AND d.received_at > k.received_at', t, t);
 EXECUTE format('ALTER TABLE core.%I DROP CONSTRAINT %I', t, t || '_pkey');
 EXECUTE format('ALTER TABLE core.%I ADD CONSTRAINT %I PRIMARY KEY (tenant_id,gateway_id,external_id,event_key)', t, t || '_pkey');
END $$;
-- +goose StatementEnd
