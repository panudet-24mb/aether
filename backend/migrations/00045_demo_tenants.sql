-- +goose Up
-- Demo workspaces for the digital twin (docs/platform/digital-twin.md). `cmd/demo-twin setup` creates a separate
-- workspace full of fictional people and sensors and marks it demo, so row level security keeps it apart from real
-- data, and its alerts are never delivered:
--   * core.tenants.demo: set only by the migration role. aether_app never had UPDATE on core.tenants; its INSERT is
--     narrowed here to (id, name), the only columns registration writes, so a new workspace always starts as not
--     demo (and cannot pick its own gateway and device limits either);
--   * the notification worker sends nothing for a demo workspace (ClaimNotifications), even if someone adds a
--     channel to it (a channel's own "send test" button still sends, on purpose: it is how a channel is checked);
--   * the live twin may name people only in a demo workspace until twin_settings exists (P2);
--   * core.twin_impersonal_tags tells the twin which of the tags it is about to show are not carried by people: a tag
--     none of whose live registrations in the workspace (any project) is roaming or worn. Everything else is personal,
--     so an unknown tag fails closed. It answers only for the ids it is given.
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';
ALTER TABLE core.tenants ADD COLUMN demo boolean NOT NULL DEFAULT false;
REVOKE INSERT ON core.tenants FROM aether_app;
GRANT INSERT (id, name) ON core.tenants TO aether_app;
-- +goose StatementBegin
CREATE FUNCTION core.twin_impersonal_tags(worn_profiles text[], ids text[]) RETURNS SETOF text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
 SELECT lower(d.external_id) FROM core.devices d
  WHERE d.tenant_id = core.tenant_id() AND d.removed_at IS NULL AND lower(d.external_id) = ANY(ids)
  GROUP BY lower(d.external_id)
  HAVING bool_and(NOT d.roaming AND NOT (d.profile_id = ANY(worn_profiles)))
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION core.twin_impersonal_tags(text[], text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.twin_impersonal_tags(text[], text[]) TO aether_app;
RESET ROLE;

-- +goose Down
SET ROLE aether_owner;
SET LOCAL lock_timeout = '5s';
DROP FUNCTION core.twin_impersonal_tags(text[], text[]);
REVOKE INSERT (id, name) ON core.tenants FROM aether_app;
GRANT INSERT ON core.tenants TO aether_app;
ALTER TABLE core.tenants DROP COLUMN demo;
RESET ROLE;
