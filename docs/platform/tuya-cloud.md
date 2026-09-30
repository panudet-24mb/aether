# Tuya Cloud mode: Tuya devices with nothing installed on site

Status: phases G1 (library, `adapters/tuyacloud`), G2 (storage, core wiring and the service entry points, migration
00034) and G3 (the `tuya-cloud` worker) are implemented and tested against fakes. The HTTP routes and UI (G4) and
the compose service (G5) are not. Nothing here has been verified against a real Tuya cloud project yet; the gateway
model and profile are `Verified: false`.

**Feature flag `TUYA_CLOUD`** (API, `true`/`false`, default `false`). Off: `/catalog` lists neither the
`tuya-cloud` gateway model nor the `tuya-cloud-device@1` profile, such a gateway cannot be created, the profile
cannot be registered, linking and syncing are refused (`tuya_cloud_disabled`) and discovery never offers a
`tuya_cloud` device. Unlinking still works, so a deployment that turns the mode off can clean up.

## What it is

A **Tuya Cloud** gateway stands for one of the user's own Tuya IoT cloud projects (the one their Smart Life app
account is linked to). Aether never runs anything on site for it:

- **state and liveness** arrive through the project's **Message Service** (Apache Pulsar), which the worker consumes
  over Pulsar's WebSocket API;
- **commands** go out through the project's **OpenAPI** (`POST /v2.0/cloud/thing/{id}/shadow/properties/issue`);
- the **device list** and each device's data-point specification come from the OpenAPI too (a *sync*).

Compared with Aether Edge (local LAN, `docs/platform/aether-edge.md`), cloud mode needs no site host and reaches
battery sensors and hub sub-devices, but depends on Tuya's cloud and on the project's monthly quota.

- Gateway model: `tuya-cloud` (`domain.TuyaCloudGatewayModel`, transport `cloud`).
- Device profile: `tuya-cloud-device@1` (`domain.TuyaCloudProfile`, radio `tuya-cloud`, actuator). It registers
  only under a cloud gateway, and a cloud gateway takes nothing else (`domain.ProfileAllowedOn`).
- Device id: the Tuya device id, lower case, 16–32 letters and digits (the same id an Edge uses).
- **One mode per physical device:** within a workspace a Tuya id is registered either through an Edge
  (`tuya-wifi-device@1`) or through the cloud (`tuya-cloud-device@1`), never both (unique index
  `devices_tuya_one_mode`); two paths would confirm each other's commands and double every event.

## Message Service protocol (`adapters/tuyacloud/mq.go`, `events.go`)

- Endpoint per region (`tuyacloud.MQHosts`), e.g. `wss://mqe.tuyaeu.com:8285`. Consumer path
  `/ws/v2/consumer/persistent/<access id>/out/<channel>/<access id>-sub?ackTimeoutMillis=3000&subscriptionType=Failover`.
  Channel `event` (all devices) or `event-test` (the console's test devices).
- Handshake headers: `username: <access id>`, `password: md5hex(access_id + md5hex(secret))[8:24]`.
- Each frame: `{"messageId","payload"(base64 envelope),"properties","publishTime","redeliveryCount"}`. The envelope
  `{"data","protocol","pv","sign","t"}` carries the business message encrypted with key `secret[8:24]`: AES-GCM when
  the `em` property says `aes_gcm` (12-byte nonce, then ciphertext and tag), AES-ECB with strict PKCS#7 otherwise.
- Acknowledge: `{"messageId": "..."}`; negative acknowledge (redeliver later):
  `{"type":"negativeAcknowledge","messageId":"..."}`.
- Protocols used: 4 (legacy status report), 20 (legacy device events: online, offline, delete, bindUser,
  nameUpdate), 1000 (IoT Core property report), 1001 (IoT Core device events). Others are acknowledged and ignored.
- `us-e`, `eu-w` and `sg` hosts are derived, not confirmed by a Tuya SDK (`tuyacloud.MQVerified`).

## Storage (migration 00034)

`core.tuya_cloud_links`, one row per cloud gateway (primary key tenant + gateway, RLS tenant scope plus the
RESTRICTIVE project scope by gateway, like the Edge tables):

| Column | Meaning |
|---|---|
| `region`, `channel` | data center (`us`, `us-e`, `eu`, `eu-w`, `in`, `cn`, `sg`) and Message Service topic |
| `credentials_sealed` | Access ID and Secret as one JSON document, AES-256-GCM, bound to tenant + gateway (see Security); NULL once unlinked |
| `access_id_hint` | at most 8 characters of the Access ID, for the UI |
| `access_id_digest` | SHA-256 of the Access ID, **globally unique**: one project feeds one gateway; NULL once unlinked |
| `state`, `state_at`, `reason`, `last_error_code` | `''`, `linking`, `online`, `offline`, `auth_failed`, `not_subscribed`, `quota`, `disabled`, as the worker last saw it |
| `last_event_at`, `last_health_at` | last stored message, last worker health report |
| `usage_month`, `events_month`, `api_calls_month`, `dropped_month` | the month's usage (UTC), reset when the month changes |
| `revision` | moves on every link, rotation, unlink and revoke; the worker restarts a link whose revision changed |
| `sync_requested_at` | a device-list sync is wanted (new link, a member's request, an unknown or re-bound device) |
| `linked_by`, `linked_at`, `rotated_at`, `updated_at` | bookkeeping |

`core.tuya_cloud_link_ids()` (SECURITY DEFINER, `search_path` pinned, EXECUTE for `aether_app` only) returns
tenant, gateway, revision and state of every link with credentials, of an active tenant and a live `tuya-cloud`
gateway. It is all the worker reads before it has a tenant; it never returns credentials. Like
`core.active_tenant_ids()` it is callable by every `aether_app` process, the API included: any of them can see
which links exist across tenants, never what they hold.

`core.tuya_cloud_link_count(excluded)` (definer) counts the calling tenant's linked projects other than one
gateway, whatever the caller's project scope: the per-workspace cap (`TUYA_CLOUD_MAX_LINKS_PER_TENANT`, default 2)
is enforced with it in `SaveTuyaCloudLink`, under a per-tenant advisory lock (`cloud_link_limit`).

The Down migration refuses to run while a live (not revoked) `tuya-cloud` gateway exists.

`core.tuya_devices` is shared with Aether Edge. Cloud rows never have a local key (`local_key_sealed` NULL); new
columns `parent_tuya_id` and `node_id` (sub-devices behind a hub) and `reported_t` (the last report time per
data-point code of the device's specification — at most 128 — plus `@online` for liveness, ≤ 32 KiB); reason
`cloud_link_down`.

`core.device_commands.transport` gains `cloud`; a cloud command's `wire` is `{"<code>": <raw>}`
(`tuya.CloudWire`), computed when it is queued. `core.command_targets` lists cloud devices with the link's state as
`agent_state` and `key_status` `ok`.

## Ingest (`postgres.CaptureTuyaCloud`)

One transaction per message, under the gateway's row lock (like `CaptureEdge`):

1. The gateway must be a live `tuya-cloud` gateway with credentials; a message in flight after an unlink is refused.
2. A bounded diagnostic copy goes to `core.gateway_packets`: protocol, and per event its kind, device id and the
   codes it reported — **never values**.
3. Any message proves the link: a link in a down state becomes `online` and the devices it took offline come back.
4. Status: items are ordered by report time; per code only a value newer than `reported_t` is kept (a redelivered
   or late message never overwrites a newer value; Tuya times in seconds are read as milliseconds, and a time more
   than 5 minutes ahead of Aether's clock is read as now, so it cannot outrank the reports that follow). Codes map to
   data points through the device's specification (`tuya.CodeIndex`), then to Aether properties (`tuya.State`).
   A device **registered on this gateway** goes through the shared pipeline (`ingestDeviceState`: command
   confirmation, samples, events, frame `tuya-cloud@1`); any other synced device keeps only its last settable
   values.
5. Online / offline: ordered by time too; the device's `available` and, when registered, its reported liveness
   (event source `cloud`).
6. Renamed updates the Tuya name only (a registration keeps its own name). Removed marks the Tuya device removed.
7. Bound, or any event of an unknown device id: `sync_requested_at` is set, at most once per 10 minutes. **Events
   never create devices.**

## Liveness

- `cloudAgent` (`postgres/z2m.go`) is the agent kind of a cloud gateway: `ScanOffline` takes a gateway's devices
  offline with source `cloud_silent` when nothing (no message, no worker health) was recorded for 5 minutes
  (`CloudSilentAfter`): the worker is gone.
- A link the worker reports down (`offline`, `auth_failed`, `not_subscribed`, `quota`, left `linking`, or
  `disabled` by an unlink) for more than 2 minutes (`CloudLinkGrace`) takes its devices offline with source
  `cloud_link_down` and reason `cloud_link_down`; the link coming back online restores them and clears the reason.

## The worker (`cmd/tuya-cloud`, `internal/tuyacloudlink`)

Environment of the worker: `DATABASE_URL` (the `aether_app` role), `TUYA_CLOUD_PRIVATE_KEY` (required; it refuses to
start without it; no JWT or channel seal key is read), `TUYA_CLOUD_MAX_LINKS` (default 50),
`TUYA_CLOUD_MAX_LINKS_PER_TENANT` (default 2), `TUYA_CLOUD_EVENT_BUDGET` and `TUYA_CLOUD_API_BUDGET` (monthly
budget of the guard, placeholders 68000 / 26000 by default — set them to the project's real plan; 0 turns a guard
off). Environment of the API: `TUYA_CLOUD`, `TUYA_CLOUD_PUBLIC_KEY`, `TUYA_CLOUD_MAX_LINKS_PER_TENANT`.
`infra/prod/setup.py` generates the key pair once and keeps it. `tuya-cloud health` checks the database for the
container healthcheck. The image builds the binary; the compose service is phase G5.

- **Faults:** every goroutine of a link recovers a panic (logged with tenant, gateway and place, never the panic's
  value) and the link alone restarts after a backoff; a panic while delivering a command fails that command.

- **Supervisor:** reads `tuya_cloud_link_ids()` every 30 s and on `NOTIFY aether_tuya_cloud` (link saved, rotated,
  unlinked, revoked, sync asked); starts, stops and restarts links by revision. At most `TUYA_CLOUD_MAX_LINKS`
  links run, at most `TUYA_CLOUD_MAX_LINKS_PER_TENANT` per tenant, and free slots go round-robin across tenants.
  A link whose credentials do not open or were refused stays parked (holding no slot) until its revision changes;
  a database failure while reading its configuration is retried with backoff, never parked.
- **Session:** the credentials are read inside the link's tenant transaction (`OpenLinkRow`) and opened by
  `cloudkeys` with the tenant/gateway binding; the OpenAPI token is cached and renewed 5 minutes before it expires.
- **Consumer:** gorilla/websocket with `Proxy: nil`, TLS verification always on, TLS ≥ 1.2. Ping every 30 s, the
  connection is dropped after 75 s without anything received. Reconnect backoff 1 s doubling to 5 min, jittered;
  a 401/403 handshake records `auth_failed` and parks the link. Per message:
  - malformed frame, envelope or event, undecryptable (bad GCM tag, wrong key), redelivered more than 3 times, or
    of an unknown protocol: acknowledged and dropped (counted);
  - a message id seen among the last 4096: acknowledged again, not applied twice;
  - frames up to 256 KiB are handled. For a larger one the first 4 KiB are read for its message id (Pulsar writes
    it first) and it is acknowledged, then drained up to 1 MiB; a frame beyond 1 MiB closes the connection (it was
    acknowledged already when its id could be read). After 3 connections in a row closed that way the link reports
    `offline` / `mq_oversize_frames` and waits the longest backoff. The limit is enforced by the consumer, not as
    gorilla's read limit, which refuses a frame from its header before its id could be read;
  - a transient database failure: negatively acknowledged and the connection re-established, so Pulsar redelivers;
  - at most 20 messages a second per link are stored; faster ones wait (Pulsar keeps them).
- **Sync:** when a sync is wanted (at most once per 10 minutes per link, never under the budget guard): the device
  list, then each device's model (v2.0 thing model, falling back to v1.1 specifications), stored without a key;
  then the registered devices' current properties are read once and stored like a status report.
- **Commands:** a `commander.Dispatcher` with `Transports: ["cloud"]`, 2 commands/s per link (burst 4), no
  housekeeping (mqtt-commander settles timeouts and automation requests for every transport), and a `Deliver` that
  issues the wire through the link's session within 5 s. Failures are stored on the command: `device_offline`
  (Tuya code 2001), `quota` (the link becomes `quota`), `auth_failed` (the link becomes `auth_failed` /
  `not_subscribed`), `cloud_unreachable`, `rejected`, `link_unavailable`, `quota_near`. mqtt-commander claims only
  `z2m` and `edge` commands. A command to a device whose link is down is refused when queued (`offline`).
- **Budget:** each link counts messages (every delivered message, malformed, undecryptable, over-redelivered and
  oversize ones included: Tuya bills them), OpenAPI calls (token calls included) and dropped messages and adds them
  to the month's counters with every health report. From 95 % of either budget the **trial guard** is on: commands
  are refused with `quota_near`, messages of devices not registered on the gateway are dropped, and no sync runs.
- **Health:** every 60 s per link: usage flushed, `last_health_at` set and a diagnostic
  `{"cloud_health":{state, connected, events_month, api_calls_month, dropped_month, budget}}` stored.

## Security

- **Asymmetric sealing.** The API seals the Access ID and Secret with `security.SealTuyaCloud`: an anonymous box
  (X25519 + XSalsa20-Poly1305, `nacl/box.SealAnonymous`) to the worker's public key `TUYA_CLOUD_PUBLIC_KEY`. The
  binding `"tuya-cloud\0<tenant>\0<gateway>"` is sealed inside with the credentials and checked on open, so a sealed
  value copied onto another row does not open. The API holds only the public key: nothing it holds (the public
  key, the JWT or channel seal keys and anything derived from them) opens a credential. Without the public key
  linking answers 503 `tuya_cloud_unconfigured`.
- Only the worker can open them, with `TUYA_CLOUD_PRIVATE_KEY`: the opener is package
  `internal/tuyacloudlink/cloudkeys`, and a test (`TestOnlyTheWorkerImportsTheOpener`) fails if any other binary
  under `cmd/` depends on it.
- **Linking (`app.Service.LinkTuyaCloud`, the contract G4's route relies on):** the flag is on, the member may manage
  devices, the public key is configured; region, channel and credential shape are valid; the credentials are
  **proven with Tuya (`GET /v1.0/token`) before anything is written** — otherwise anyone knowing a project's Access
  ID could claim its globally unique digest and lock the owner out; then they are sealed and saved. The
  repository also refuses members who may not manage devices (`SaveTuyaCloudLink`, `UnlinkTuyaCloud`,
  `RequestCloudSync`), as defence in depth.
- A Tuya Cloud gateway has nothing on site: it gets no HTTP ingest token (one is stored hashed but never handed
  out, and `CapturePacket` refuses the model anyway), no broker account (`EnrollMQTT` refuses it) and no ACL
  (mqtt-provisioner skips it).
- The OpenAPI client uses no environment proxy and TLS 1.2 or later, and follows no redirect.
- Nothing returns or logs a credential: `domain.TuyaCloudLink` has no secret field (a hint of at most 8 characters
  only), log lines name tenant and gateway, and Tuya errors carry a category and Tuya's numeric code.
- One project per link, across all workspaces (unique digest). A second link of the same project answers
  `already_linked` without saying where it is linked. Unlink and gateway revoke wipe the sealed value and the
  digest.
- Every table is under tenant RLS and the project scope; the definer function returns ids, revision and state only.
- Hosts come from the region tables only. TLS verification is never disabled (every Tuya SDK disables it).

## Tests

- `internal/tuyacloudlink`: a fake Message Service (`fakecloud.MQ`, TLS WebSocket, checks the headers, models
  redelivery) and a fake OpenAPI (`fakecloud.API`): ack/nack, redelivery, poison, oversize, GCM tamper, reconnect,
  auth failure and parking, foreign credentials, sync and snapshot, command delivery and error mapping, budget guard.
- `internal/tuyacloudlink/cloudkeys`: the box opens only with the worker's private key and only for its tenant and
  gateway; no key the API holds opens it; the import restriction.
- `internal/tuyacloudlink` also: slots (parked links hold none, round-robin across tenants, per-tenant cap),
  transient configuration reads retried, an injected panic restarting only that link, oversize frames acknowledged
  by id and the degraded state after repeated closes, billed counting of dropped messages.
- `tests/tuya_cloud_test.go` also: the feature flag off and on (catalog, gateway creation, registration, linking,
  discovery), no ingest token and no broker account for a cloud gateway, the link service (missing public key,
  refused credentials write nothing, viewers refused in the service and the repository, the two-project cap),
  future report times clamped and `reported_t` pruned.
- `internal/commander`: transports, pace overrides, `Deliver`.
- `tests/tuya_cloud_test.go` (real database and worker against the fakes): link → sync → discovery → register →
  status/events; out-of-order and replayed reports; device liveness; unknown devices; diagnostics without values;
  end-to-end command (POST /commands → issue → report → confirmed); offline / quota refusals; link down
  (`cloud_link_down`) and silence (`cloud_silent`); auth failure; one project per link; the same device id in two
  workspaces; one mode per device; unlink and revoke; project scope; the budget guard.

## Not yet

- G4: API routes and UI to link, rotate, unlink, sync and see status: thin wrappers of `Service.LinkTuyaCloud`,
  `UnlinkTuyaCloud`, `RequestTuyaCloudSync` and `TuyaCloudLinkStatus`.
- G5: the compose service for the worker (with `TUYA_CLOUD_PRIVATE_KEY` only) and `TUYA_CLOUD` /
  `TUYA_CLOUD_PUBLIC_KEY` on the API.
- Verification against a real project: message shapes, the `em` property through the WebSocket proxy, the
  derived regional hosts, the budget numbers and Tuya's device-offline error code.
