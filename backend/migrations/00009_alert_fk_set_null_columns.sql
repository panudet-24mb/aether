-- +goose Up
SET ROLE aether_owner;
-- A composite FK with plain ON DELETE SET NULL nulls every referencing column, including the NOT NULL
-- tenant_id, so deleting a rule/channel that had alerts/notifications failed. Null only the id column.
ALTER TABLE core.alerts DROP CONSTRAINT alerts_tenant_id_rule_id_fkey;
ALTER TABLE core.alerts ADD CONSTRAINT alerts_tenant_id_rule_id_fkey FOREIGN KEY(tenant_id,rule_id) REFERENCES core.alert_rules(tenant_id,id) ON DELETE SET NULL (rule_id);
ALTER TABLE core.notifications DROP CONSTRAINT notifications_tenant_id_channel_id_fkey;
ALTER TABLE core.notifications ADD CONSTRAINT notifications_tenant_id_channel_id_fkey FOREIGN KEY(tenant_id,channel_id) REFERENCES core.notification_channels(tenant_id,id) ON DELETE SET NULL (channel_id);
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
ALTER TABLE core.alerts DROP CONSTRAINT alerts_tenant_id_rule_id_fkey;
ALTER TABLE core.alerts ADD CONSTRAINT alerts_tenant_id_rule_id_fkey FOREIGN KEY(tenant_id,rule_id) REFERENCES core.alert_rules(tenant_id,id) ON DELETE SET NULL;
ALTER TABLE core.notifications DROP CONSTRAINT notifications_tenant_id_channel_id_fkey;
ALTER TABLE core.notifications ADD CONSTRAINT notifications_tenant_id_channel_id_fkey FOREIGN KEY(tenant_id,channel_id) REFERENCES core.notification_channels(tenant_id,id) ON DELETE SET NULL;
RESET ROLE;
