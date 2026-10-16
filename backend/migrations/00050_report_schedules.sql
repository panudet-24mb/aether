-- Scheduled delivery of saved reports (docs/platform/reports.md, "Scheduled delivery").
--
--   core.report_definitions  gains a schedule: none | daily | weekly (an ISO weekday, 1 = Monday), a local
--                            Asia/Bangkok time HH:MM, the notification channels it goes to, and next_run_at (when the
--                            scheduler next picks it up; NULL without a schedule). The API checks the channels on
--                            save: enabled, and workspace channels or the report's own project's (as for alert rules).
--   core.report_runs         one row per report sent: the period it covers, scheduled or sent by hand, queued until
--                            every delivery settled, then done. A scheduled period is sent once
--                            (report_runs_schedule_once), however many API replicas tick at the same time.
--   core.report_deliveries   one row per run and channel, delivered by the API's worker with the retry and backoff of
--                            core.notifications (claimed FOR UPDATE SKIP LOCKED): queued | sent | failed | skipped.
--
-- Runs and deliveries carry the report's project and take the definitions' project policies: a member limited to
-- projects reads the runs of workspace reports and of their projects' reports. The worker (full scope) prunes runs
-- older than 180 days (their deliveries go with them).
--
-- core.report_recipients is core.alert_recipients for a report: who an email channel with the "members" audience
-- reaches. A workspace report goes to owners and members who see every project; a project report also to members of
-- that project. Erased identities and members whose reports module is "none" are left out.

-- +goose Up
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';

ALTER TABLE core.report_definitions
 ADD COLUMN schedule text NOT NULL DEFAULT 'none' CHECK(schedule IN ('none','daily','weekly')),
 ADD COLUMN schedule_weekday smallint CHECK(schedule_weekday BETWEEN 1 AND 7),
 ADD COLUMN schedule_time text CHECK(schedule_time ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
 ADD COLUMN channels jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(channels) = 'array' AND jsonb_array_length(channels) <= 10),
 ADD COLUMN next_run_at timestamptz,
 ADD CONSTRAINT report_definitions_schedule CHECK(
  (schedule = 'none' AND next_run_at IS NULL)
  OR (schedule = 'daily' AND schedule_time IS NOT NULL AND schedule_weekday IS NULL AND next_run_at IS NOT NULL)
  OR (schedule = 'weekly' AND schedule_time IS NOT NULL AND schedule_weekday IS NOT NULL AND next_run_at IS NOT NULL));
CREATE INDEX report_definitions_due ON core.report_definitions(tenant_id, next_run_at) WHERE schedule <> 'none';

CREATE TABLE core.report_runs (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 id uuid NOT NULL,
 report_id uuid NOT NULL,
 project_id uuid,
 period_from timestamptz NOT NULL,
 period_to timestamptz NOT NULL,
 trigger text NOT NULL CHECK(trigger IN ('schedule','manual')),
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','done')),
 created_by uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 PRIMARY KEY(tenant_id, id),
 CHECK(period_from < period_to),
 CONSTRAINT report_runs_report_fkey FOREIGN KEY(tenant_id, report_id) REFERENCES core.report_definitions(tenant_id, id) ON DELETE CASCADE,
 CONSTRAINT report_runs_project_fkey FOREIGN KEY(tenant_id, project_id) REFERENCES core.projects(tenant_id, id) ON DELETE CASCADE
);
-- A period is its start and its end: a daily and a weekly period can start on the same day (switching a report from daily to
-- weekly must not lose the first weekly run).
CREATE UNIQUE INDEX report_runs_schedule_once ON core.report_runs(tenant_id, report_id, period_from, period_to) WHERE trigger = 'schedule';
CREATE INDEX report_runs_report ON core.report_runs(tenant_id, report_id, created_at DESC);
CREATE INDEX report_runs_manual ON core.report_runs(tenant_id, created_at) WHERE trigger = 'manual';

CREATE TABLE core.report_deliveries (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL,
 run_id uuid NOT NULL,
 project_id uuid,
 channel_id uuid,
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','sent','failed','skipped')),
 attempts integer NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 last_error text CHECK(length(last_error) <= 200),
 created_at timestamptz NOT NULL DEFAULT now(),
 sent_at timestamptz,
 PRIMARY KEY(tenant_id, id),
 CONSTRAINT report_deliveries_run_fkey FOREIGN KEY(tenant_id, run_id) REFERENCES core.report_runs(tenant_id, id) ON DELETE CASCADE,
 CONSTRAINT report_deliveries_channel_fkey FOREIGN KEY(tenant_id, channel_id) REFERENCES core.notification_channels(tenant_id, id) ON DELETE SET NULL (channel_id)
);
CREATE INDEX report_deliveries_queue ON core.report_deliveries(tenant_id, next_attempt_at) WHERE status = 'queued';
CREATE INDEX report_deliveries_run ON core.report_deliveries(tenant_id, run_id);

-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['report_runs','report_deliveries'] LOOP
  EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY', t);
  EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY', t);
  EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())', t);
  EXECUTE format('CREATE POLICY project_scope_read ON core.%I AS RESTRICTIVE FOR SELECT TO aether_app
   USING((SELECT core.scope_all()) OR project_id IS NULL OR core.project_in_scope(project_id))', t);
  EXECUTE format('CREATE POLICY project_scope_insert ON core.%I AS RESTRICTIVE FOR INSERT TO aether_app
   WITH CHECK((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))', t);
  EXECUTE format('CREATE POLICY project_scope_update ON core.%I AS RESTRICTIVE FOR UPDATE TO aether_app
   USING((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))
   WITH CHECK((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))', t);
  EXECUTE format('CREATE POLICY project_scope_delete ON core.%I AS RESTRICTIVE FOR DELETE TO aether_app
   USING((SELECT core.scope_all()) OR (project_id IS NOT NULL AND core.project_in_scope(project_id)))', t);
  EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON core.%I TO aether_app', t);
 END LOOP;
END $$;
-- +goose StatementEnd

-- The per-workspace budget of reports sent by hand counts every project's runs, not the ones the sender can see.
-- +goose StatementBegin
CREATE FUNCTION core.manual_report_runs_since(since timestamptz) RETURNS bigint
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT count(*) FROM core.report_runs r WHERE r.tenant_id = core.tenant_id() AND r.trigger = 'manual' AND r.created_at >= since
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.manual_report_runs_since(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.manual_report_runs_since(timestamptz) TO aether_app;

-- Who a "members" email of a report reaches (see the header). The caller must have full scope (the delivery worker
-- does); a project-limited session learns nothing. Ordered as core.alert_recipients orders them.
-- +goose StatementBegin
CREATE FUNCTION core.report_recipients(report uuid, roles text[]) RETURNS TABLE(email text)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 WITH target AS (
  SELECT d.tenant_id, d.project_id FROM core.report_definitions d
   WHERE d.tenant_id = core.tenant_id() AND d.id = report AND core.scope_all()
 )
 SELECT u.email FROM target t
  JOIN core.memberships m ON m.tenant_id = t.tenant_id
  JOIN identity.users u ON u.id = m.user_id
  WHERE u.erased_at IS NULL AND m.role = ANY(roles)
   AND (m.role = 'owner'
    OR NOT EXISTS(SELECT 1 FROM core.member_projects mp WHERE mp.tenant_id = m.tenant_id AND mp.user_id = m.user_id)
    OR (t.project_id IS NOT NULL AND EXISTS(SELECT 1 FROM core.member_projects mp
         WHERE mp.tenant_id = m.tenant_id AND mp.user_id = m.user_id AND mp.project_id = t.project_id)))
   AND (m.role = 'owner' OR coalesce((SELECT ma.permissions->>'reports' FROM core.member_access ma
         WHERE ma.tenant_id = m.tenant_id AND ma.user_id = m.user_id), '') <> 'none')
  ORDER BY array_position(ARRAY['owner','admin','operator','viewer'], m.role), u.email
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.report_recipients(uuid, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.report_recipients(uuid, text[]) TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';
DROP FUNCTION core.report_recipients(uuid, text[]);
DROP FUNCTION core.manual_report_runs_since(timestamptz);
DROP TABLE core.report_deliveries;
DROP TABLE core.report_runs;
DROP INDEX core.report_definitions_due;
ALTER TABLE core.report_definitions DROP CONSTRAINT report_definitions_schedule,
 DROP COLUMN next_run_at, DROP COLUMN channels, DROP COLUMN schedule_time, DROP COLUMN schedule_weekday, DROP COLUMN schedule;
RESET ROLE;
