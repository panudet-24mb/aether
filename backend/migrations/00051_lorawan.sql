-- LoRaWAN devices behind a Milesight UG65 running its embedded network server (phase 1: ingest and display,
-- read-only).
--
-- The gateway connects OUT to this broker with its own per-gateway account and publishes every uplink under
-- aether/lora/<gateway id>/<DevEUI>/up (docs/platform/milesight-lorawan.md). The payload is what the gateway's payload
-- codec decoded, or, without a codec, the gateway's envelope with the raw bytes, which Aether decodes for the
-- Milesight models it knows. What this adds:
--   * core.lorawan_devices: every device the gateway forwarded an uplink for, keyed by DevEUI (16 lowercase hex), with
--     the names an installer gave it on the gateway, the catalog model identified for it and the radio figures of
--     its last uplink (only an envelope carries them). Discovery lists the unregistered ones.
--
-- A LoRaWAN stream keeps silence-based liveness: a sensor reporting every 10–20 minutes is offline once it stays
-- quiet longer than an offline rule allows, so such rules for LoRaWAN devices need offline_after_sec well above
-- the device's reporting interval.
--
-- +goose Up
SET ROLE aether_owner;

CREATE TABLE core.lorawan_devices (
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 dev_eui text NOT NULL CHECK(dev_eui ~ '^[0-9a-f]{16}$'),
 name text NOT NULL DEFAULT '' CHECK(length(name)<=128),
 application_name text NOT NULL DEFAULT '' CHECK(length(application_name)<=128),
 -- The catalog model identified for the device (WS303, GS301, ...), '' when none was.
 model text NOT NULL DEFAULT '' CHECK(length(model)<=32),
 f_port integer CHECK(f_port IS NULL OR f_port BETWEEN 0 AND 255),
 f_cnt bigint CHECK(f_cnt IS NULL OR f_cnt>=0),
 rssi smallint, snr real,
 decoded boolean NOT NULL DEFAULT false,
 last_seen timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,gateway_id,dev_eui),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);

ALTER TABLE core.lorawan_devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.lorawan_devices FORCE ROW LEVEL SECURITY;
-- Permissive tenant isolation plus the RESTRICTIVE project scope keyed by gateway (00022_access_fixes.sql).
CREATE POLICY tenant_scope ON core.lorawan_devices USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY project_scope ON core.lorawan_devices AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
GRANT SELECT,INSERT,UPDATE ON core.lorawan_devices TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP TABLE core.lorawan_devices;
RESET ROLE;
