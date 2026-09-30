# Personal data (PDPA)

Aether is usually operated by an organisation (a hospital, a building owner) that is the **data controller** under the
Thai Personal Data Protection Act B.E. 2562; the Aether deployment, and whoever runs it on the controller's behalf, is
the **processor**. This page lists what the product records, the tools it gives the controller for data-subject
requests, and what it deliberately keeps. Migrations `00038` (read-access log), `00039` (export, erasure, notice) and `00047` (the digital twin's movement history in erasure).

## 1. What is personal data here

| Data | Where | Whose |
|---|---|---|
| Email, name, role, project access, sessions | `identity.users`, `core.memberships`, `core.member_projects`, `core.member_access`, `identity.sessions` | members |
| What a member did | `core.audit_logs` (actor uuid), `core.alerts.acked_by/resolved_by/note`, `core.device_commands.actor_id`, `core.automations.enabled_by` | members |
| Who read personal data | `core.access_log` (actor uuid, client IP) | members |
| A worn tag's history: samples (RSSI per gateway = a location trace), raw advertisements, zone presence, events, alerts naming the wearer | `core.sensor_samples`, `core.ble_history`, `core.presence_state`, `core.device_events`, `core.alerts`, and the registration name in `core.devices` | wearers |
| A worn tag's movement history: every zone change, for the digital twin's replay | `core.presence_history` (kept `PRESENCE_HISTORY_DAYS`, default 30, deleted row by row once older, while an uplink in the last 7 days corroborates the clock; shown back only `people_replay_days`, owner setting). The twin's 5-minute summaries (`core.sample_rollup`) hold fixed sensors only, never a roaming or worn / button tag | wearers |
| IP addresses in the reverse proxy's access log | Caddy log volume, 90 days | everybody |

## 2. Read-access log (PDPA §37(1); PDPC 2022 security measures)

Every successful API read of personal data writes one row to `core.access_log`: time, actor, resource, whose data
(`subject_kind` / `subject_id`), a server-generated request id (a client's `X-Request-ID` is dropped) and client IP.
The routes are exactly those in `readRules` (`backend/internal/adapters/httpapi/privacy.go`; a unit test checks each
still names a registered route):

| Area | Routes (subject narrowed to) |
|---|---|
| Members | `GET /members`, `GET /members/:id/access` (the member) |
| Devices and readings | `GET /devices`, `GET /devices/:id/state` and `/controls` (the device), `GET /gateways/:id/packets` (the gateway), `GET /discovery`, `GET /live`, `POST /studio/render`, `GET /studio/sources`, `GET /presence/:external` (the tag) |
| Assets and plans | `GET /assets`, `GET /assets/:kind/:id`, `GET /assets/export.csv`, `GET /sites/:id`, `GET /twin/sites/:id/state` (digital twin; people as counts, pseudonyms or names, see `digital-twin.md`; with names `twin_live_named`, logged on every request) |
| Digital twin history | `GET /twin/sites/:id/timeline` (with names `twin_timeline_named`, every request), `GET /twin/sites/:id/replay` (the site; a replay with pseudonyms or names is `twin_replay_people`, logged on every request), `GET /twin/people/:external/trail` (the tag, every request) |
| Learned signals | `GET /signals`, `GET /signals/sessions/:id` |
| Events and alerts | `GET /events` (`?external_id=` → the tag), `GET /alerts`, `GET /alerts/summary`, `GET /notifications` |
| What flows and people did | `GET /automations/:id/runs`, `GET /commands` (`?device_id=` → the device), `GET /commands/:id` |
| Realtime | `GET /ws`, on subscribe |
| Wall displays (docs/platform/display.md) | `GET /kiosk/board` when it includes wearer names (only with the owner's `show_names`), `GET /kiosk/studio/:id`, `GET /kiosk/floorplan` (the site); the actor is the display (`actor_kind = 'display'`) |
| Privacy views and exports | `GET /privacy/access-log`, `/privacy/audit`, `/privacy/erasures`; every export (never deduplicated) |

Not logged: routes that return no personal data (gateways, projects, rules, channels, templates, catalogs, flows'
definitions, MQTT and Edge set-up, Tuya device lists, floor images).

**Deduplication.** The same actor reading the same subject through the same resource within 10 minutes is one row,
so polled screens (`/devices`, `/notifications`, `/live`) cost one row per person per screen per 10 minutes.
Concurrent requests for the same key share one write, and none of them answers before that row exists. The window is
per API process (in memory).

**Fail closed.** The row is written after the handler succeeds and before the response leaves. If it cannot be
written, the response is replaced by `503 {"error":"unavailable"}`, and an export loses its download header. The tag
export writes its row before it starts streaming. A WebSocket subscription is refused with
`{"type":"error","error":"unavailable"}`. Only API reads are gated: alert evaluation and delivery (LINE, e-mail,
webhooks) run in the worker and never wait on the trail, so an access-log outage does not silence an SOS.

**Storage.** `core.access_log` is partitioned by month on `at`, created ahead and dropped whole by
`core.maintain_partitions` like the sample history.

- **Retention.** It lives in `core.retention_policy` (default 400 days, 30–3650). The runtime cannot read or change
  it, and nothing it passes to the maintenance functions shortens the trail. The migration role sets it from
  `ACCESS_LOG_RETENTION_DAYS` on every `migrate` run (`core.set_access_log_retention`). Sample and BLE retention stay
  runtime settings, because the runtime already holds DELETE on those tables.
- **Writing.** The runtime role may `INSERT` a row only with its own tenant and actor, and only as a member of that
  workspace, or, for a wall display, only as that paired and unrevoked display (`core.display_active()`). It has no
  `UPDATE` or `DELETE`.
- **Reading.** Only an owner of the workspace reads the trail (`core.is_tenant_owner()`), and partitions are not
  reachable directly.

**Viewer.** The owner's menu "ความเป็นส่วนตัว": `GET /privacy/access-log`, `GET /privacy/audit` (the audit trail of
changes) and `GET /privacy/erasures`. Filters `actor`, `resource`, `subject_kind`, `subject_id`, `action` (prefix),
`from`, `to`; keyset paging with `before_at` + `before_id` (the `next` of the previous page); `limit` 1–500. Reading them
is itself logged.

## 3. Data-subject requests (PDPA §30–§33)

The controller must answer within 30 days. Every export and erasure is audited (`core.audit_logs`) and every erasure
is recorded in `core.erasure_log`, which holds only the uuid or tag id and row counts.

### Members

- **Own copy.** Any member: "ส่งออกข้อมูลของฉัน" on the team page, `GET /me/export`. It is a JSON file with:
  - profile, membership and sessions (no token material);
  - the member's audit trail, alerts they handled, commands they sent and flows they enabled;
  - read-access log entries by or about them. Only the member's own entries carry an IP address; other readers'
    IPs are their personal data, not this member's.

  Each list is capped at the newest 5000 rows.
- **Export for a member.** Owner: `POST /members/:id/export`.
- **Erasure.** Owner: `POST /members/:id/erase` (`identity.erase_member`, under the workspace's member lock).
  - What goes: sessions and refresh tokens, and the membership with its project and module access.
  - Flows the member enabled that command devices are disabled, and each is audited as `automation.disarmed`: their
    authority is gone.
  - The identity keeps its uuid, but its email becomes `erased-<uuid>@erased.invalid`, its name "ผู้ใช้ที่ถูกลบ" and its
    password unusable. `erased_at` is set; login skips it and it can never be adopted again, even after a rollback of
    migration 00039.
  - Refused:
    - oneself (403);
    - the last owner (409 `last_owner`);
    - an identity that also belongs to another workspace (409 `shared_identity`). The platform operator erases that
      one everywhere with `admin erase-user <uuid>`, which refuses when the identity is some workspace's last owner
      unless `--force`. Each workspace gets its own ledger row.

### Wearers (worn tags)

On the device panel of a roaming tag, owner only. The API does not require the tag to be roaming; the UI shows the
panel only for roaming tags.

- **Export.** `POST /devices/:id/privacy-export` streams a ZIP straight from the database:
  - `summary.json`: the registration, presence, events and alerts;
  - `samples.ndjson`, `ble_history.ndjson` and `presence_history.ndjson` (the digital twin's zone changes): at most
    100,000 rows each;
  - `manifest.json`: written last. `"complete": true` marks a whole archive, and `truncated` says whether a cap was
    hit. An archive without it was cut off mid-stream.

  At most 2 exports run at once per workspace and 4 in all per API process; another gets 429.
  - **Timeouts.** The server's 15 s write timeout is one absolute deadline for a response, so the stream instead moves
    the connection's write deadline 30 s ahead at every part and every 1000 rows. A slow client gets the whole
    archive; a client that stops reading is cut off about 30 s later, and its slot is freed.
  - **Failures.** A failure or panic while streaming leaves the archive without `manifest.json`.
- **Erasure.** `POST /devices/:id/erase-history` (`core.erase_identity_history`).
  - **Scope.** By default it covers the gateways of the registration's own project (or the gateways with no project,
    when it has none). `{"tenant_wide": true}` covers every gateway of the workspace. The scope comes from the
    registration, not the caller, and is recorded in the ledger.
  - **Rename.** `{"name": "…"}` renames the registration in the same transaction, for the tag's next wearer. An
    invalid name refuses the whole request. The ledger records the rename, and a restore gives the registration the
    placeholder name.
  - **Locking.** The in-scope gateway rows are locked first, in id order. That is ingest's first lock, so ingest
    through those gateways waits for the erasure to finish.
  - **Deleted, for this tag in scope, across every partition:**
    - samples;
    - raw advertisements;
    - zone presence, when the tag's zone, or the zone it is moving to, is in scope;
    - movement history (the digital twin's zone changes), when either end is in scope or neither is known, and the
      tag's 5-minute summaries on gateways in scope (matched in lower case, whatever case the registration uses);
    - events no alert points at.
  - **Kept but scrubbed:**
    - Alerts stay as incident records under a generic title that names only the kind of event ("กดปุ่มฉุกเฉิน SOS ·
      ผู้สวมใส่ (ลบข้อมูลแล้ว)"). The event each alert points at keeps no name and no detail.
    - Notification delivery errors are cleared.
    - Flow runs lose the tag id and their detail.
    - Teaching-session labels and learned-signal descriptions are cleared. Learned signals belong to the tag, not a
      gateway, so they are scrubbed whatever the scope.
    - Stream names become the MAC.
    - Removed registrations of the tag get the placeholder name.
  - **Not touched:**
    - the live registration's name, unless a new name is given;
    - operators' notes on alerts, which are kept as written;
    - the tag's current state (`core.stream_state`: readings, overwritten by the next packet);
    - the last 100 raw packets per gateway (`core.gateway_packets`, a ring buffer that turns over within minutes);
    - flow state (`core.automation_state`, a node's on/off per tag);
    - commands, which name devices, not wearers.

### What is kept, and why

| Kept | Basis |
|---|---|
| Audit trail and read-access log rows naming the erased uuid, until their retention | security of processing (§37), accountability |
| `core.erasure_log` | proof that the request was carried out |
| Alerts of an erased tag (anonymised) | safety incident record (legal claims, vital interest) |

## 4. Backups and restores

Erased data lives on in the nightly dumps (up to 8 weeks: 14 daily + 8 weekly) and in the PITR repository (its
retention window). A restore brings it back, and the restored `core.erasure_log` does not list the erasure. So,
around every restore (docs/production.md §7):

```sh
# before the restore, from the live database (keep these files with the backups anyway)
docker compose ... run --rm -T --entrypoint /app/admin migrate export-erasures > erasures.jsonl
# after the restore: erase again (idempotent) and put the ledger rows back
docker compose ... run --rm -T --entrypoint /app/admin migrate reapply-erasures < erasures.jsonl
```

These commands run as the migration role, a superuser. The data functions `core.erase_member_data` and
`core.erase_identity_data` are not granted to the API role; the API reaches them only through the owner-checked
definers.

- Reapplying redoes each tag erasure with its recorded scope, and puts back the placeholder name where the owner had
  renamed the registration.
- `erase-user` re-reads the memberships under the locks and refuses to commit if any remain or the identity was not
  anonymised.
- A member erasure that would now remove a workspace's last owner is reported and skipped; erase it by hand with
  `erase-user --force` once the workspace has another owner.
- Member erasures take the same workspace lock as the API's member changes.

## 5. Notice (PDPA §23)

- **The notice.** `/privacy` is a built-in Thai template. A deployment whose organisation publishes its own sets
  `PRIVACY_NOTICE_URL` (https) with `setup.py --privacy-notice-url`, and every link goes there. The login screen links
  to it, through `GET /api/v1/notice` (public).
- **Acknowledgement.** After sign-in, members see a bar until they acknowledge the current version
  (`domain.NoticeVersion`, stored in `identity.users.notice_ack_version` via `POST /me/notice-ack`). Raise the version
  when the notice changes materially.
- **Adding a member.** The owner/admin confirms the person was informed before the member is created.
- **Roaming.** Switching a tag to roaming warns that the wearer's location is followed across gateways and that the
  wearer must be told first.

## 6. Processor notes for the controller's record of processing (ROPA)

- **Categories.** Members: identity, access and activity. Wearers: location trace and events of a tag.
- **Purposes.** Operating the monitoring system; emergency response; security of processing.
- **Retention.** `SAMPLE_RETENTION_DAYS` (default 90, partition-grained +7 days), `BLE_HISTORY_HOURS` (default 24),
  `ACCESS_LOG_RETENTION_DAYS` (default 400, applied by `migrate`), alerts and events per the workspace's alert retention, backups 8 weeks,
  PITR 1–2 weeks, proxy log 90 days.
- **Recipients.** None by default. Notification channels (LINE, e-mail, webhooks) send alert titles, which may name a
  wearer, to the destinations the owner configures.
- **Security.** Row-level security per workspace and project; read-access logging; TLS; backups encrypted at rest
  (PITR).
