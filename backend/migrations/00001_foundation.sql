-- +goose Up
SET ROLE aether_owner;
CREATE SCHEMA identity AUTHORIZATION aether_owner;
CREATE SCHEMA core AUTHORIZATION aether_owner;
REVOKE ALL ON SCHEMA identity, core FROM PUBLIC;
GRANT USAGE ON SCHEMA identity, core TO aether_app;

-- Identity is global: users can belong to several tenants. Passwords never leave the auth repository.
CREATE TABLE identity.users (
 id uuid PRIMARY KEY, email text NOT NULL UNIQUE CHECK(email = lower(email)),
 password_hash text NOT NULL, name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE core.tenants (
 id uuid PRIMARY KEY, name text NOT NULL, status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','suspended')),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE core.memberships (
 tenant_id uuid NOT NULL REFERENCES core.tenants(id), user_id uuid NOT NULL REFERENCES identity.users(id),
 role text NOT NULL CHECK(role IN ('owner','admin','operator','viewer')), PRIMARY KEY(tenant_id,user_id)
);
CREATE TABLE identity.sessions (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES identity.users(id), tenant_id uuid NOT NULL REFERENCES core.tenants(id),
 expires_at timestamptz NOT NULL, revoked_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,id), FOREIGN KEY(tenant_id,user_id) REFERENCES core.memberships(tenant_id,user_id)
);
CREATE TABLE identity.refresh_tokens (
 hash text PRIMARY KEY, user_id uuid NOT NULL, session_id uuid NOT NULL,
 consumed_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(user_id,session_id) REFERENCES identity.sessions(user_id,id)
);
CREATE TABLE core.gateways (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES core.tenants(id), name text NOT NULL,
 model text NOT NULL, token_hash text NOT NULL, revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(tenant_id,id)
);
CREATE TABLE core.devices (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES core.tenants(id), gateway_id uuid NOT NULL,
 name text NOT NULL, external_id text NOT NULL, profile_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(tenant_id,id), UNIQUE(tenant_id,gateway_id,external_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);
CREATE TABLE core.gateway_packets (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL, gateway_id uuid NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(), payload jsonb NOT NULL,
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);
CREATE INDEX gateway_packets_latest ON core.gateway_packets(tenant_id,gateway_id,received_at DESC);
CREATE TABLE core.telemetry (
 tenant_id uuid NOT NULL, device_id uuid NOT NULL, ts timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(), metrics jsonb NOT NULL,
 PRIMARY KEY(tenant_id,device_id,ts), FOREIGN KEY(tenant_id,device_id) REFERENCES core.devices(tenant_id,id)
);
CREATE TABLE core.device_state (
 tenant_id uuid NOT NULL, device_id uuid NOT NULL, ts timestamptz NOT NULL, metrics jsonb NOT NULL,
 PRIMARY KEY(tenant_id,device_id), FOREIGN KEY(tenant_id,device_id) REFERENCES core.devices(tenant_id,id)
);
CREATE TABLE core.audit_logs (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES core.tenants(id), actor_id uuid NOT NULL,
 action text NOT NULL, target_id uuid NOT NULL, at timestamptz NOT NULL DEFAULT now()
);

-- Missing or transaction-reset context yields NULL, which denies access.
CREATE FUNCTION core.tenant_id() RETURNS uuid LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT nullif(current_setting('app.tenant_id',true),'')::uuid
$$;
CREATE FUNCTION identity.user_id() RETURNS uuid LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT nullif(current_setting('app.user_id',true),'')::uuid
$$;
REVOKE ALL ON FUNCTION core.tenant_id(), identity.user_id() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.tenant_id(), identity.user_id() TO aether_app;
ALTER TABLE core.tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.tenants USING(id=core.tenant_id()) WITH CHECK(id=core.tenant_id());
ALTER TABLE core.memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY own_membership_read ON core.memberships FOR SELECT USING(user_id=identity.user_id());
CREATE POLICY own_membership_insert ON core.memberships FOR INSERT WITH CHECK(tenant_id=core.tenant_id() AND user_id=identity.user_id());
-- No runtime UPDATE/DELETE membership policy in this foundation.
ALTER TABLE identity.sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY session_owner ON identity.sessions USING(user_id=identity.user_id()) WITH CHECK(user_id=identity.user_id());
ALTER TABLE identity.refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.refresh_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY refresh_owner ON identity.refresh_tokens USING(user_id=identity.user_id()) WITH CHECK(user_id=identity.user_id());
-- +goose StatementBegin
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['gateways','devices','gateway_packets','telemetry','device_state','audit_logs'] LOOP
 EXECUTE format('ALTER TABLE core.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE core.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON core.%I USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id())',t);
 END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT, INSERT ON identity.users TO aether_app;
GRANT SELECT, INSERT, UPDATE ON identity.sessions, identity.refresh_tokens TO aether_app;
GRANT SELECT, INSERT ON core.tenants, core.memberships, core.devices, core.telemetry, core.gateway_packets, core.audit_logs TO aether_app;
GRANT SELECT, INSERT, UPDATE ON core.gateways, core.device_state TO aether_app;
GRANT DELETE ON core.gateway_packets, core.telemetry TO aether_app;
-- Gateway lookup cannot require a caller-supplied tenant. Narrow function returns only routing identity.
-- A fixed SECURITY DEFINER query under the non-login migration owner. Runtime cannot assume this role.
CREATE POLICY gateway_credential_lookup ON core.gateways FOR SELECT TO aether_owner USING(true);
CREATE FUNCTION core.lookup_gateway(gateway uuid, digest text) RETURNS TABLE(tenant_id uuid)
 LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT g.tenant_id FROM core.gateways g WHERE g.id=gateway AND g.token_hash=digest AND g.revoked_at IS NULL
$$;
REVOKE ALL ON FUNCTION core.lookup_gateway(uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.lookup_gateway(uuid,text) TO aether_app;
RESET ROLE;

-- +goose Down
-- Explicitly destructive, operator invoked only. The API never runs migrations.
DROP SCHEMA core CASCADE;
DROP SCHEMA identity CASCADE;
