-- +goose Up
SET ROLE aether_owner;
-- core.tenants is FORCE RLS, so the SECURITY DEFINER lookup used by background workers needs an
-- explicit owner policy (same pattern as gateway_credential_lookup). Runtime roles are unaffected.
CREATE POLICY tenant_ids_lookup ON core.tenants FOR SELECT TO aether_owner USING(true);
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
DROP POLICY tenant_ids_lookup ON core.tenants;
RESET ROLE;
