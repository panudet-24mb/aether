-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='aether_mqtt_provisioner') THEN
 CREATE ROLE aether_mqtt_provisioner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
 END IF;
END $$;
-- +goose StatementEnd
SET ROLE aether_owner;
CREATE TABLE core.mqtt_accounts(
 tenant_id uuid NOT NULL, gateway_id uuid NOT NULL, password_hash text NOT NULL,
 revision integer NOT NULL DEFAULT 1, applied_revision integer NOT NULL DEFAULT 0,
 applied_at timestamptz, PRIMARY KEY(tenant_id,gateway_id),
 FOREIGN KEY(tenant_id,gateway_id) REFERENCES core.gateways(tenant_id,id)
);
ALTER TABLE core.mqtt_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE core.mqtt_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON core.mqtt_accounts USING(tenant_id=core.tenant_id()) WITH CHECK(tenant_id=core.tenant_id());
CREATE POLICY mqtt_owner_access ON core.mqtt_accounts TO aether_owner USING(true) WITH CHECK(true);
GRANT SELECT,INSERT,UPDATE ON core.mqtt_accounts TO aether_app;
GRANT USAGE ON SCHEMA core TO aether_mqtt_provisioner;
CREATE FUNCTION core.mqtt_provisioning_accounts() RETURNS TABLE(gateway_id uuid,password_hash text,revision integer)
 LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT a.gateway_id,a.password_hash,a.revision FROM core.mqtt_accounts a JOIN core.gateways g ON g.id=a.gateway_id AND g.tenant_id=a.tenant_id WHERE g.revoked_at IS NULL ORDER BY a.gateway_id
 $$;
CREATE FUNCTION core.mqtt_provisioning_ack(gateway uuid,expected integer) RETURNS void
 LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 UPDATE core.mqtt_accounts SET applied_revision=expected,applied_at=now() WHERE gateway_id=gateway AND revision=expected AND applied_revision<>expected
 $$;
CREATE FUNCTION core.lookup_mqtt_gateway(gateway uuid) RETURNS TABLE(tenant_id uuid)
 LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT g.tenant_id FROM core.gateways g JOIN core.mqtt_accounts a ON a.gateway_id=g.id AND a.tenant_id=g.tenant_id WHERE g.id=gateway AND g.revoked_at IS NULL
 $$;
REVOKE ALL ON FUNCTION core.mqtt_provisioning_accounts(),core.mqtt_provisioning_ack(uuid,integer),core.lookup_mqtt_gateway(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.mqtt_provisioning_accounts(),core.mqtt_provisioning_ack(uuid,integer) TO aether_mqtt_provisioner;
GRANT EXECUTE ON FUNCTION core.lookup_mqtt_gateway(uuid) TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP FUNCTION core.lookup_mqtt_gateway(uuid),core.mqtt_provisioning_ack(uuid,integer),core.mqtt_provisioning_accounts();
DROP TABLE core.mqtt_accounts;
RESET ROLE;
