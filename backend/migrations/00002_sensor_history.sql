-- +goose Up
SET ROLE aether_owner;
CREATE TABLE core.device_templates (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 name text NOT NULL, version integer NOT NULL CHECK(version>0),
 decoder_id text NOT NULL CHECK(decoder_id='minew-ffe1-a101@1'),
 definition jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,name,version)
);
CREATE TABLE core.sensor_streams (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
 name text NOT NULL, template_id uuid, last_seen timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,gateway_id,external_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id),
 FOREIGN KEY(tenant_id,template_id) REFERENCES core.device_templates(tenant_id,id)
);
CREATE TABLE core.sensor_samples (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, external_id text NOT NULL,
 event_key text NOT NULL, received_at timestamptz NOT NULL, template_id uuid,
 decoder_id text NOT NULL, reading jsonb NOT NULL,
 PRIMARY KEY(tenant_id,gateway_id,external_id,event_key),
 FOREIGN KEY(tenant_id,gateway_id,external_id) REFERENCES core.sensor_streams(tenant_id,gateway_id,external_id),
 FOREIGN KEY(tenant_id,template_id) REFERENCES core.device_templates(tenant_id,id)
);
CREATE INDEX sensor_samples_history ON core.sensor_samples(tenant_id,gateway_id,external_id,received_at DESC,event_key);
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['device_templates','sensor_streams','sensor_samples'] LOOP
 EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())',t);
 END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT,INSERT ON core.device_templates,core.sensor_samples TO aether_app;
GRANT SELECT,INSERT,UPDATE ON core.sensor_streams TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.sensor_samples,core.sensor_streams,core.device_templates;
RESET ROLE;
