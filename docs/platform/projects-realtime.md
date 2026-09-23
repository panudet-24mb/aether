# Projects and real-time updates

Added 2026-09-20. Two related pieces: a workspace can split its gateways into several **projects**, and the web app now learns about changes through a **WebSocket** instead of waiting for the next poll.

## Projects

A project is a named, coloured group of gateways inside one tenant (a site, a building, a customer job). Devices follow their gateway, so a project is "these gateways and everything adopted on them".

- Table `core.projects` (migration `00011`), tenant-scoped under FORCE RLS like every other table. `core.gateways.project_id` is nullable: a gateway without a project is "ยังไม่จัดโปรเจค".
- At most 50 active projects per tenant. Names are unique case-insensitively among active projects, at most 128 bytes. Colours: `mint`, `blue`, `amber`, `coral`, `violet`, `slate`.
- Archiving hides the project and unassigns its gateways. Nothing is deleted, and dashboards that pointed at it keep working.
- Only owner and admin may create, edit, archive or move. Every change is audited (`project.created`, `project.updated`, `project.archived`, `gateway.project_changed`).

### API

| Call | Purpose |
| --- | --- |
| `GET /api/v1/projects` | list, with `gateway_count`; `?archived=true` includes archived |
| `POST /api/v1/projects` | create `{name, description, color}` |
| `POST /api/v1/projects/{id}/update` | rename, describe, recolour |
| `POST /api/v1/projects/{id}/archive` | archive and unassign gateways |
| `POST /api/v1/gateways/{id}/project` | `{project_id}` moves the gateway; `null` unassigns |
| `POST /api/v1/gateways` | accepts optional `project_id` |

### "เชื่อมต่ออุปกรณ์" page

The bar under the header lists "ทั้งหมด", each project, and "ยังไม่จัดโปรเจค" when any gateway is unassigned. The choice is remembered per tenant in the browser.

- Inside a project the canvas, counters, removed-device list and search only cover that project. A gateway created there is created in that project.
- The gateway inspector has a "โปรเจค" select that moves the gateway, with its devices, to another project.
- "ทั้งหมด" shows every project at once with the project name on each gateway node. This is the cross-project view: drag an adopted device's link onto a gateway of another project to move it there.
- Node positions are stored per tenant, not per project, so switching projects never loses a layout.

### Dashboard Studio

- The command bar has a project select. It filters the dashboard list, and a dashboard created while a project is selected stores `definition.project_id`.
- "เพิ่ม Panel" lists only the devices of the dashboard's project. Tick "ข้ามโปรเจค" to pick a device from any project; the list then shows each device's project. Panels that point outside the dashboard's project are marked "ข้ามโปรเจค" in their footer.
- "ชื่อและข้อมูล Dashboard" can change or clear the dashboard's project. `project_id` only narrows the picker: rendering is authorised per tenant, so cross-project panels need no extra permission.
- Dashboards without `project_id` appear under "ทุกโปรเจค" and see every device, as before.

## Real-time signals

`GET /ws` is a WebSocket. It never carries data. It only tells the browser "something of this kind changed, refetch it through the REST API", so authorisation and tenant isolation stay in one place.

1. The browser opens the socket. The server checks the `Origin` header against `APP_ORIGIN`.
2. The first message must be `{"type":"auth","token":"<access token>"}` within 5 s. The token is never put in the URL.
3. The server replies `{"type":"ready"}`, then sends `{"type":"signal","kind":"packet|event|alert|inventory","gateway_id":"…"}`.
4. The client sends `{"type":"ping"}` every 30 s. The server re-checks the session every 60 s and closes the socket after logout, role removal or tenant suspension.

Signals originate in Postgres: every mutating transaction calls `pg_notify('aether_signal', …)`, which is delivered on commit only. The API process holds one `LISTEN` connection (`internal/realtime.Hub`) and fans out to the sockets of that tenant. This also covers packets written by the separate MQTT ingest container. A slow client misses signals instead of blocking others; at most 50 sockets per tenant. Upgrade attempts are limited to 30 per minute per IP and at most 200 sockets may be waiting for their auth message. A tab that is refused for `too_many_connections` falls back to polling and retries after two minutes.

| Kind | Sent when | Who refetches |
| --- | --- | --- |
| `packet` | a gateway uplink was stored | topology status |
| `event` | a device event was recorded | topology, alerts page |
| `alert` | an alert opened or changed state | alerts page, sidebar badge, banner |
| `inventory` | gateway, device or project created, changed, moved or removed | topology (forces a full reload) |

Clients debounce signals (350–400 ms) and keep a slow safety poll: 30 s on the topology page and 60 s for alerts while the socket is up, the old 5 s and 15 s when it is down. The topology header shows "Live" or "Polling".

When a new **critical** alert opens, every page shows a red banner with the alert title, a button to the alerts page and a sound toggle (two short tones, remembered in the browser). Alerts that were already open when the page loaded do not raise the banner.

## Verification (2026-09-20)

- Integration tests `tests/projects_realtime_test.go`: projects are tenant-scoped, gateways move between projects, archive unassigns, and a signal reaches only sockets of its own tenant. The full suite passes.
- Playwright in Chrome against the local stack, 11 of 11 checks: socket connects, two projects created from the UI, project scope hides other gateways, gateway created inside the active project, moved A to B from the inspector, project label in the all view, a change made outside the tab appeared in under 1 s through a signal, new dashboard stores `project_id`, device picker scoped then opened by "ข้ามโปรเจค", cross-project panel saved with `project_id` preserved, no failed API responses. The test removes what it created.
- Banner run, 4 of 4 checks: with a temporary critical tamper rule, the simulator's MBT01 tamper raised the banner on the Live monitoring page, the sidebar badge went from 1 to 2 without a reload, and the banner button opened the alerts page. The rule and its alert were removed afterwards.

## Limits

- Permissions are still per tenant. A project does not restrict which users can see it.
- Alert rules are scoped by gateway or device, not by project.
- A multi-instance API deployment works because every instance listens to Postgres, but `NOTIFY` payloads are limited to 8000 bytes, which is why signals stay tiny.
