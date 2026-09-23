-- +goose Up
SET ROLE aether_owner;
-- Notification channels: public config in jsonb, the secret (webhook HMAC key, LINE channel token) sealed separately.
CREATE TABLE core.notification_channels (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 name text NOT NULL, kind text NOT NULL CHECK(kind IN ('webhook','line','email')),
 enabled boolean NOT NULL DEFAULT true, config jsonb NOT NULL, secret_enc text,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,name)
);
CREATE TABLE core.alert_rules (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 name text NOT NULL, enabled boolean NOT NULL DEFAULT true,
 event_type text NOT NULL CHECK(event_type IN ('tamper','button','leak','motion','offline','threshold')),
 severity text NOT NULL CHECK(severity IN ('info','warning','critical')),
 scope jsonb NOT NULL DEFAULT '{}'::jsonb, channels jsonb NOT NULL DEFAULT '[]'::jsonb,
 dedupe_sec integer NOT NULL DEFAULT 600 CHECK(dedupe_sec BETWEEN 0 AND 86400),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,name)
);
-- Last known flag state per discovered stream so events are edge-triggered, not repeated every uplink.
CREATE TABLE core.stream_state (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
 tamper smallint NOT NULL DEFAULT 0, leak smallint NOT NULL DEFAULT 0, moving smallint NOT NULL DEFAULT 0,
 instance text NOT NULL DEFAULT '', offline boolean NOT NULL DEFAULT false,
 breaches jsonb NOT NULL DEFAULT '[]'::jsonb, updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,gateway_id,external_id),
 FOREIGN KEY(tenant_id,gateway_id,external_id) REFERENCES core.sensor_streams(tenant_id,gateway_id,external_id)
);
CREATE TABLE core.device_events (
 tenant_id uuid NOT NULL, id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
 device_name text NOT NULL, event_type text NOT NULL, detail jsonb NOT NULL DEFAULT '{}'::jsonb,
 occurred_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,id), FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);
CREATE INDEX device_events_recent ON core.device_events(tenant_id,occurred_at DESC);
CREATE INDEX device_events_device ON core.device_events(tenant_id,external_id,occurred_at DESC);
CREATE TABLE core.alerts (
 tenant_id uuid NOT NULL, id uuid NOT NULL, rule_id uuid, event_id uuid NOT NULL,
 gateway_id uuid NOT NULL, external_id text NOT NULL, device_name text NOT NULL,
 event_type text NOT NULL, severity text NOT NULL, title text NOT NULL,
 status text NOT NULL DEFAULT 'open' CHECK(status IN ('open','acknowledged','resolved')),
 opened_at timestamptz NOT NULL DEFAULT now(), acked_by uuid, acked_at timestamptz,
 resolved_by uuid, resolved_at timestamptz, note text,
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,rule_id) REFERENCES core.alert_rules(tenant_id,id) ON DELETE SET NULL,
 FOREIGN KEY(tenant_id,event_id) REFERENCES core.device_events(tenant_id,id)
);
CREATE INDEX alerts_status ON core.alerts(tenant_id,status,opened_at DESC);
CREATE INDEX alerts_dedupe ON core.alerts(tenant_id,rule_id,external_id,opened_at DESC);
CREATE TABLE core.notifications (
 tenant_id uuid NOT NULL, id uuid NOT NULL, alert_id uuid NOT NULL, channel_id uuid,
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','sent','failed')),
 attempts integer NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL DEFAULT now(),
 last_error text, created_at timestamptz NOT NULL DEFAULT now(), sent_at timestamptz,
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,alert_id) REFERENCES core.alerts(tenant_id,id),
 FOREIGN KEY(tenant_id,channel_id) REFERENCES core.notification_channels(tenant_id,id) ON DELETE SET NULL
);
CREATE INDEX notifications_queue ON core.notifications(tenant_id,next_attempt_at) WHERE status='queued';
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['notification_channels','alert_rules','stream_state','device_events','alerts','notifications'] LOOP
 EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())',t);
 END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT,INSERT,UPDATE,DELETE ON core.notification_channels,core.alert_rules TO aether_app;
GRANT SELECT,INSERT,UPDATE ON core.stream_state,core.alerts,core.notifications TO aether_app;
GRANT SELECT,INSERT ON core.device_events TO aether_app;
-- Background workers (offline detection, delivery) iterate tenants; this fixed query exposes ids only.
CREATE FUNCTION core.active_tenant_ids() RETURNS SETOF uuid
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT id FROM core.tenants WHERE status='active' ORDER BY id
 $$;
REVOKE ALL ON FUNCTION core.active_tenant_ids() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.active_tenant_ids() TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION core.active_tenant_ids();
DROP TABLE core.notifications,core.alerts,core.device_events,core.stream_state,core.alert_rules,core.notification_channels;
RESET ROLE;
