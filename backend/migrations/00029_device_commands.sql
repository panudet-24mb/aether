-- Zigbee2MQTT gateways, phase 2: the first downlink. Aether can now set any settable property of any device a
-- Zigbee2MQTT bridge exposes: a switch gang (state_l1), a light (brightness, color_temp, color), a cover
-- (position), a lock (state LOCK/UNLOCK), a thermostat (system_mode, occupied_heating_setpoint), ...
--
-- The rule the platform set for commands (docs/platform/dynamic-platform.md): "Commands require their own
-- authorized path, expiry, correlation ID and observed acknowledgement/state; never optimistically report
-- hardware success." core.device_commands is that path, and it doubles as a transactional outbox:
--   * the API (or, later, an automation) inserts a `pending` row inside the caller's own transaction, under
--     the caller's project scope. The value is validated against the device's own Zigbee2MQTT definition
--     (z2m_devices.exposes: the feature must exist and be settable, the value must fit its type, range, step or
--     enumeration). A toggle is resolved server-side into an explicit value from the last reported one, so
--     every published command is idempotent;
--   * mqtt-commander claims `pending` -> `sent` (FOR UPDATE SKIP LOCKED), COMMITS, and only then publishes
--     {"<property>": <value>} to aether/z2m/<gateway>/<ieee>/set. A crash after the commit loses the publish; the
--     row then times out. A command is therefore delivered at most once and never replayed late;
--   * a pending row older than expires_at (10 s) is `expired` and never published;
--   * the collector confirms a `sent` (or recently timed-out) row in the same transaction as the device's next
--     state report that carries the property with the desired value (numbers within the feature's tolerance):
--     `confirmed`. No such report within 10 s: `timeout`. A report within 60 s after the timeout may still
--     confirm it, unless the property was meanwhile reported with another value (`superseded`); a late
--     confirmation never names itself as the cause of an event.
--
-- `id` is the client's idempotency key (the Idempotency-Key header). One command may be in flight per device
-- and property: the partial unique index makes a second concurrent change a conflict instead of a queue.
--
-- z2m_devices gains:
--   * exposes: the device definition's full `exposes` array as Zigbee2MQTT published it (bounded), which the
--     command validation reads today and a generic ingest of every exposed property can build on later;
--   * state: the last reported value of each settable property ({"state_l1":"ON","brightness":128}), used to
--     resolve a toggle. It lives here rather than in stream_state because a device that is not a switch has no
--     stream until the generic ingest exists.
--
-- +goose Up
SET ROLE aether_owner;

ALTER TABLE core.z2m_devices ADD COLUMN exposes jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(exposes)='array');
ALTER TABLE core.z2m_devices ADD COLUMN state jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(state)='object');

CREATE TABLE core.device_commands (
 tenant_id uuid NOT NULL, id uuid NOT NULL,
 gateway_id uuid NOT NULL, device_id uuid NOT NULL,
 ieee text NOT NULL CHECK(ieee ~ '^0x[0-9a-f]{16}$'),
 property text NOT NULL CHECK(property ~ '^[A-Za-z0-9_]{1,64}$'),
 requested text NOT NULL CHECK(requested IN ('set','toggle')),
 value jsonb NOT NULL CHECK(octet_length(value::text)<=1024),
 source text NOT NULL CHECK(source IN ('user','automation')),
 actor_id uuid, automation_id uuid,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','sent','confirmed','timeout','expired','failed')),
 error text NOT NULL DEFAULT '' CHECK(length(error)<=200),
 created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
 sent_at timestamptz, settled_at timestamptz,
 -- A timed-out command may still be confirmed by a late report, but not once the property was reported with
 -- another value after the timeout: the change that follows is then someone else's (a press on the wall).
 superseded boolean NOT NULL DEFAULT false,
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id),
 FOREIGN KEY(tenant_id,device_id) REFERENCES core.devices(tenant_id,id)
);
-- The commander's work queue and the confirmation lookup.
CREATE INDEX device_commands_open ON core.device_commands(status,created_at) WHERE status IN ('pending','sent');
CREATE INDEX device_commands_confirm ON core.device_commands(gateway_id,ieee,property) WHERE status IN ('sent','timeout');
CREATE INDEX device_commands_device ON core.device_commands(tenant_id,device_id,created_at DESC);
-- One command in flight per device and property, enforced by the database, not only by the handler.
CREATE UNIQUE INDEX device_commands_in_flight ON core.device_commands(tenant_id,device_id,property) WHERE status IN ('pending','sent');

ALTER TABLE core.device_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.device_commands FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.device_commands USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
-- A member restricted to projects may command (and see commands of) only gateways in those projects.
CREATE POLICY project_scope ON core.device_commands AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id))
 WITH CHECK((SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id));
-- DELETE only for the retention prune (PruneAlertData).
GRANT SELECT,INSERT,UPDATE,DELETE ON core.device_commands TO aether_app;

-- The workspace command budget counts every command of the workspace, not only those the (possibly
-- project-restricted) caller may see: counted under the caller's RLS, a member of one project would get the
-- whole budget again. SECURITY DEFINER runs as aether_owner, to which the RESTRICTIVE project policy (TO
-- aether_app) does not apply; the permissive tenant policy still does, and the WHERE repeats it.
CREATE FUNCTION core.command_count_since(since timestamptz) RETURNS bigint
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT count(*) FROM core.device_commands c WHERE c.tenant_id = core.tenant_id() AND c.created_at > since
$$;
REVOKE ALL ON FUNCTION core.command_count_since(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.command_count_since(timestamptz) TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION core.command_count_since(timestamptz);
DROP TABLE core.device_commands;
ALTER TABLE core.z2m_devices DROP COLUMN state;
ALTER TABLE core.z2m_devices DROP COLUMN exposes;
RESET ROLE;
