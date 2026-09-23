-- +goose Up
SET ROLE aether_owner;
-- Retention: the API prunes old events, resolved alerts and finished notifications per tenant.
GRANT DELETE ON core.device_events,core.alerts,core.notifications TO aether_app;
RESET ROLE;
-- +goose Down
SET ROLE aether_owner;
REVOKE DELETE ON core.device_events,core.alerts,core.notifications FROM aether_app;
RESET ROLE;
