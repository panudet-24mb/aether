# Alerts — event log, rules and notifications

Implemented locally 2026-09-20 (migrations `00006_alerts.sql` … `00009_alert_fk_set_null_columns.sql`). This is the first cut of FR‑RUL: persistent device events, tenant rules, an alert lifecycle and outbound notifications, all tenant‑scoped with FORCE RLS.

## Pipeline

1. **Events** are edge‑triggered inside the packet transaction (`internal/adapters/postgres/alerts.go` → `internal/alerts.Detect`). The last flag state per stream lives in `core.stream_state`, so a tag that keeps reporting `tamper=1` produces one `tamper` event, then one `tamper_cleared` when it returns to 0. Types: `tamper`, `tamper_cleared`, `button` (Eddystone‑UID instance change), `leak`, `leak_cleared`, `motion`, `motion_stopped`, `offline`, `online`, `threshold`, `threshold_cleared`.
2. **Rules** (`core.alert_rules`, ≤100 per workspace) subscribe to one event type with an optional device scope, a severity, channels and a dedupe window. Offline rules carry `offline_after_sec`; threshold rules carry `metric`/`op`/`value` and are evaluated per uplink with per‑rule breach state so a breach opens one alert until the value returns.
   **Built‑in rules** (migration `00024_default_rules.sql`, `core.alert_rules.builtin`): every workspace starts with `button` at **critical**, dedupe 30 s, and `tamper` at **warning** — a panic button that stays silent until somebody writes a rule for it is a safety gap. The migration backfills them for existing workspaces that have no rule of that event type yet; `postgres.seedDefaultRules` (called once from `CreateAccount`) does the same for new ones. `builtin` is a label, not a protection: the rules tab shows "สร้างให้อัตโนมัติ" and the owner may rename, re‑scope, disable or delete them, and editing one keeps the flag. `core.alert_rules` stays deliberately project‑unscoped (see the list at the end of `00019_project_access.sql`).
3. **Alerts** (`core.alerts`) open once per matching rule and device unless an unresolved alert exists or one was opened inside the dedupe window. Lifecycle `open → acknowledged → resolved` with actor and note; owner, admin and operator may transition, everyone may read.
   **SOS** is its own state on top of that: `domain.Alert.SOS()` is true when the alert's `event_type` is `button` and its severity is `critical`, and `domain.Alert.MarshalJSON` puts it on every alert the API, the WebSocket and the webhook body carry as `"sos": true`. It is derived, not stored — severity is what the workspace decided a press means — so no client re‑implements the rule, and it says nothing about the lifecycle (callers filter on `status` for the live ones). The headline of a button alert is `SOS · <device> กดปุ่มฉุกเฉิน` (`alerts.Title`).
4. **Notifications** (`core.notifications`) are queued per alert per channel and delivered by a worker inside the API process every 15 s (SKIP LOCKED claim, exponential backoff, failed after 5 attempts, delivery log with last error). Channels: **webhook** (JSON `aether.alert.v1`, `X-Aether-Event`, `X-Aether-Signature: sha256=HMAC(body)` when a secret is set), **LINE Messaging API** push (channel access token + user/group id; LINE Notify is discontinued), **email** via SMTP from `SMTP_HOST/PORT/USERNAME/PASSWORD/FROM` on the server. Channel secrets are sealed with AES‑GCM under a key derived from `JWT_SIGNING_KEY` and never returned by the API.
5. **Offline detection** runs in the same worker: streams silent longer than the smallest offline rule threshold (default 300 s) get one `offline` event; the next uplink emits `online`. Background work enumerates tenants through the fixed SECURITY DEFINER function `core.active_tenant_ids()`.

## SOS in the UI (2026-09-22)

An open SOS is meant to be impossible to miss, from anywhere in the app and without configuration:

- **Shell** (`frontend/app/live-dashboard.tsx`): a sticky red SOS bar per open SOS alert (the newest three, then a count), carrying the person/device name, the gateway that heard the press and how long ago it was. It has no dismiss control — only **รับทราบ** (`POST /alerts/{id}/ack`, which removes it because the alert leaves `?status=open`) or **ดูรายละเอียด**. The sound repeats every 4 s while any SOS is open, the existing mute toggle still wins, and the flashing is disabled under `prefers-reduced-motion`. Ordinary critical alerts keep the old one-shot banner.
- **Operations board** (`frontend/app/live/overview.tsx`): an SOS strip above the KPIs, the open-alerts KPI switches to its SOS state, SOS cards sort first and are marked on the card, in the "ต้องดูตอนนี้" list and in the device drawer.
- **Floor plan** (`frontend/app/floorplan/*`): the wearable marker of the person pressing pulses red with their name in both 2D and 3D and the zone containing them is highlighted, so the operator sees where to run. The plan keeps an SOS marker on screen even after the tag's last uplink stops being fresh. The page fetches `?status=open` alongside its snapshot and passes the MAC set into `buildAssets`.

Nothing here depends on the real B10 reporting presses (that is still unverified hardware): it is demonstrable end to end with the simulator's B10 `f00000000007`, whose Eddystone-UID instance change produces `button` events today.

## UI

"การแจ้งเตือน" in the sidebar (badge = open alerts) has four tabs: alerts (filter by status, acknowledge, resolve with note), events timeline (filter by MAC), rules (create/edit/delete with channel checkboxes) and channels (create, send a test message, delivery log). Alerts and events link to the device on the topology page.

## API

`GET /events`, `GET /alerts?status=`, `GET /alerts/summary`, `POST /alerts/{id}/ack|resolve`, `GET/POST /rules`, `POST /rules/{id}/update|delete`, `GET/POST /channels`, `POST /channels/{id}/update|delete|test`, `GET /notifications` — see `backend/api/openapi.json`.

## Limits and safety notes

- Events and state are only tracked for devices that have a discovered stream (≤100 per gateway); untracked BLE devices never produce events. Alert evaluation runs in a savepoint inside the packet transaction, so an alerting failure never discards telemetry.
- An alert is not reopened while the previous one for the same rule and device is unresolved; `dedupe_sec` applies after it is resolved. Offline rules each fire once per offline episode, when their own threshold passes; devices silent for more than seven days are treated as retired. Devices behind a revoked gateway are no longer monitored.
- Retention: newest 5,000 events (events referenced by an alert are kept), 2,000 resolved alerts and 2,000 finished notifications per workspace, pruned by the worker about every ten minutes (migration `00008`).
- Webhook targets are re-checked at connect time in production (loopback, private, link-local, CGNAT and multicast addresses are refused after DNS resolution) and non-2xx responses are reported without the status code. Webhook URLs and recipients are visible to owner/admin only.
- Channel secrets are sealed under a key derived from `JWT_SIGNING_KEY`: rotating that key makes stored secrets unreadable (deliveries fail with a clear message) and channels must be recreated. A separate, versioned sealing key is future work.

- Webhook targets must be HTTPS to public hosts; plain HTTP and private/loopback targets are accepted only when `APP_ENV=development` (local receivers, `host.docker.internal`). Redirects are not followed.
- Button and tamper semantics for B7/B10/MBT01 are still unverified on hardware (see [Minew kit](minew-kit.md)); do not treat these alerts as certified emergency signals until a captured packet passes the golden tests. The built-in `button` rule makes the platform react loudly to whatever the pipeline *does* report; it does not make the hardware decoding correct.
- One worker per API process; with several API replicas deliveries are still safe (row locks) but offline scans may run concurrently, which is harmless because marking is idempotent.
- No escalation, quiet hours, per‑tenant rate limits or scheduled reports yet; delivery log keeps the newest 100 rows in the UI.

## Verification

`tests/sos_test.go` covers the zero-configuration path: a brand-new workspace gets the two built-in rules, a simulated B10 press opens one critical alert flagged `sos`, a second press inside the dedupe window opens none, ack and resolve work, disabling the built-in rule really does silence the button, and rolling migration 00024 down and up backfills a workspace that had no rules. Because every workspace now starts with a `tamper` rule, `alerts_test.go` and `automations_test.go` call `clearBuiltinRules` first so their own counts stay exact.

Unit tests cover edge detection, threshold state, scope matching and validation. The integration test `tests/alerts_test.go` runs the real path: simulator packets → tamper/motion/button events → critical alert (dedupe respected) → worker posts to an HTTPS‑less loopback webhook with a valid HMAC → ack/resolve → offline after a silent period → online on the next packet, plus tenant isolation and secret non‑disclosure. A Playwright run against the dockerised stack created a channel and rules through the UI, received the test message and the simulator's tamper alert on a host receiver (`host.docker.internal`) with valid HMAC signatures, saw the sidebar badge and event timeline update, acknowledged and resolved the alert with a note, and found the delivery in the log. The integration test also covers two offline rules with different thresholds, deleting a channel and rules that already have history, and the production sender refusing a loopback webhook. A separate review pass led to: no events for untracked devices, savepoint isolation of alerting inside ingest, per-rule offline thresholds, retention, connect-time SSRF checks, SMTP deadlines, redacted channel config for non-managers and longer claim windows.

## Real-time (2026-09-20)

The alerts page and the sidebar badge refetch on `alert` and `event` WebSocket signals, and a new critical alert raises a banner with an optional sound on every page. See [projects and real-time](projects-realtime.md).
