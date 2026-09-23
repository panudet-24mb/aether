-- Learned device signals ("สอนสัญญาณให้ระบบ").
--
-- The problem this solves: a physical Minew B10 emergency button advertises only an accelerometer
-- frame, an info frame and Eddystone-TLM. Aether infers a press from an Eddystone-UID instance
-- change, which that tag has never broadcast, so pressing it produces nothing. The press encoding is
-- undocumented for the B10 and for every other tag in the owner's inventory (C10, B7, MSP01, PLUS,
-- C6, E8). Rather than guess a layout, the operator teaches the system: the device is recorded at
-- rest, then while being triggered, and the difference between the two captures becomes a matcher.
--
-- NUMBERING: this file was meant to be 00023, but 00023_member_access.sql was already taken by
-- another change and 00024 is reserved, so it lands at 00025. Nothing depends on the number itself.
--
-- +goose Up
SET ROLE aether_owner;

-- One teaching session: two timed phases against one identity on one gateway.
-- baseline_until / trigger_from / trigger_until bound the two capture windows inside core.ble_history,
-- which archives every distinct raw advertisement per packet and is retained for BLE_HISTORY_HOURS
-- (default 24), so a session is never pruned out from under itself.
CREATE TABLE core.signal_sessions (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 gateway_id uuid NOT NULL, external_id text NOT NULL,
 event_type text NOT NULL CHECK(event_type IN ('button','tamper','leak','motion','custom')),
 label text NOT NULL DEFAULT '' CHECK(length(label)<=64),
 started_at timestamptz NOT NULL DEFAULT now(),
 baseline_until timestamptz NOT NULL,
 trigger_from timestamptz,
 trigger_until timestamptz NOT NULL,
 status text NOT NULL DEFAULT 'baseline' CHECK(status IN ('baseline','trigger','finished','confirmed','cancelled')),
 created_by uuid REFERENCES identity.users(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id),
 -- The trigger phase can only start once the baseline is closed, and cannot be empty.
 CHECK(trigger_from IS NULL OR trigger_from>=baseline_until),
 CHECK(trigger_until>baseline_until)
);
CREATE INDEX signal_sessions_open ON core.signal_sessions(tenant_id,status,created_at DESC);

-- A confirmed signature. Exactly one of external_id (this tag) or profile_id (every tag of this
-- model, so one B10 teaches all of them) is set; the CHECK is what keeps that invariant true even
-- if application code regresses.
CREATE TABLE core.device_signals (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), id uuid NOT NULL,
 scope text NOT NULL CHECK(scope IN ('device','profile')),
 external_id text, profile_id text,
 event_type text NOT NULL CHECK(event_type IN ('button','tamper','leak','motion','custom')),
 matcher jsonb NOT NULL CHECK(jsonb_typeof(matcher)='object'),
 description text NOT NULL DEFAULT '' CHECK(length(description)<=400),
 verified boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 created_by uuid REFERENCES identity.users(id),
 PRIMARY KEY(tenant_id,id),
 CHECK((scope='device' AND external_id IS NOT NULL AND profile_id IS NULL)
    OR (scope='profile' AND profile_id IS NOT NULL AND external_id IS NULL))
);
CREATE INDEX device_signals_identity ON core.device_signals(tenant_id,external_id) WHERE external_id IS NOT NULL;
CREATE INDEX device_signals_profile ON core.device_signals(tenant_id,profile_id) WHERE profile_id IS NOT NULL;

-- Learned signals are edge-triggered exactly like the built-in tamper/leak flags, so their last state
-- belongs with the other per-stream flags in core.stream_state rather than in a parallel mechanism.
-- One jsonb object per stream, keyed by signal id, holding 0/1. A signal that is deleted simply stops
-- being read; its stale key is dropped the next time the row is written.
ALTER TABLE core.stream_state ADD COLUMN signals jsonb NOT NULL DEFAULT '{}'::jsonb
 CHECK(jsonb_typeof(signals)='object');

ALTER TABLE core.signal_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.signal_sessions FORCE ROW LEVEL SECURITY;
ALTER TABLE core.device_signals ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.device_signals FORCE ROW LEVEL SECURITY;

-- Permissive tenant isolation, the idiom of 00011_projects.sql.
CREATE POLICY tenant_scope ON core.signal_sessions USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY tenant_scope ON core.device_signals USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());

-- RESTRICTIVE project scope, ANDed with the policy above (00019_project_access.sql), with the
-- scope_all() constant hoisted into an uncorrelated sub-query so the planner evaluates it once per
-- query instead of once per row (00022_access_fixes.sql).
--
-- A session names both a gateway and a BLE identity, so it must satisfy both.
CREATE POLICY project_scope ON core.signal_sessions AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR (core.gateway_in_scope(gateway_id) AND core.identity_in_scope(external_id)))
 WITH CHECK((SELECT core.scope_all()) OR (core.gateway_in_scope(gateway_id) AND core.identity_in_scope(external_id)));

-- A device-scoped signal is keyed by BLE identity and is scoped the way core.presence_state is: it is
-- in scope when a gateway the member may see has heard that identity. A profile-scoped signal names
-- no identity and no gateway — it is workspace-wide decoding configuration, like the device catalog
-- itself, not data belonging to one project — so it is visible to every member of the workspace.
-- Creating one still requires owner/admin, which the HTTP layer enforces.
CREATE POLICY project_scope ON core.device_signals AS RESTRICTIVE TO aether_app
 USING((SELECT core.scope_all()) OR profile_id IS NOT NULL OR core.identity_in_scope(external_id))
 WITH CHECK((SELECT core.scope_all()) OR profile_id IS NOT NULL OR core.identity_in_scope(external_id));

GRANT SELECT,INSERT,UPDATE,DELETE ON core.signal_sessions TO aether_app;
GRANT SELECT,INSERT,DELETE ON core.device_signals TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
DROP POLICY project_scope ON core.device_signals;
DROP POLICY project_scope ON core.signal_sessions;
DROP POLICY tenant_scope ON core.device_signals;
DROP POLICY tenant_scope ON core.signal_sessions;
ALTER TABLE core.stream_state DROP COLUMN signals;
DROP INDEX core.device_signals_profile;
DROP INDEX core.device_signals_identity;
DROP TABLE core.device_signals;
DROP INDEX core.signal_sessions_open;
DROP TABLE core.signal_sessions;
RESET ROLE;
