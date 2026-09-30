# Wall displays (TV control-room mode)

A display is a TV or monitor at a security desk or nurse station. It shows a full-screen, auto-rotating, read-only
view of the workspace and **takes over the whole screen when an SOS or hazard alert opens**, with an alarm, until
someone acknowledges it. A display is not a member: it never holds a staff login, and its token opens a handful of
read endpoints and nothing else.

Code: migration `00044_displays.sql`, `backend/internal/{domain,app}/display.go`, `backend/internal/adapters/postgres/display.go`,
`backend/internal/adapters/httpapi/display.go` (and the display branch of `realtime.go`), `frontend/app/display/` (the TV),
`frontend/app/displays/` (management). Tests: `backend/tests/display_test.go` and the unit tests next to the code.

## 1. Setting one up

1. **Create.** An owner, or an admin who sees every project, opens **จอแสดงผล** (OPERATE) and adds a display:
   - a name;
   - the projects it shows (none ticked: every project);
   - a playlist of up to 12 views, 10–600 s each;
   - two switches, both off by default: show wearer names, and acknowledge from the TV. Only an owner may turn
     either on; an admin may turn them off. Each change is its own audit action (`display.names_on` /
     `display.names_off`, `display.ack_on` / `display.ack_off`).
2. **Code.** Aether answers a pairing code such as `T726-ZTCH`. It is 8 symbols from a 31-symbol alphabet without
   look-alikes, lives 10 minutes, works once, and is shown once. The pairing link
   `https://<origin>/display#code=T726-ZTCH` carries the code in the URL fragment, which the browser never sends to a
   server.
3. **Pair.** On the TV, open `/display` and type the code, or open the link. The TV exchanges the code for a display
   token (`dsp_…`, 32 random bytes) and keeps it in that browser's `localStorage`. Only the SHA-256 digests of the
   code and the token are stored, and the runtime role cannot even read those columns back.
4. **Sound.** Browsers block audio until someone touches the page, so the TV asks for one tap; after that the alarm
   can play.

**Re-pair** (a replaced or lost TV) issues a new code and ends the current token at once. **Revoke** ends the
display for good: the token and any code stop working, and an open signal stream closes within a second (the API
announces the revocation on the signal channel; the stream also re-checks its display every 30 s).

## 2. Views

| Kind | Shows |
|---|---|
| `overview` | Open alerts (critical in red), gateways and devices online, low batteries; big temperature/humidity tiles |
| `alerts` | Open and acknowledged alerts, newest first, with severity and how long each has been open |
| `floorplan` | The 3D building (the same renderer as the floor-plan editor, read-only, slowly orbiting); `ref` picks the site, otherwise the first one the display sees. The floor with an emergency comes to the front |
| `devices` | Gateways online or not, devices that stopped reporting, batteries under 20% |
| `presence` | Worn tags per zone: counts; names only when the owner allowed them |
| `studio` | A Dashboard Studio dashboard (`ref`, required), rendered as the display sees the data, in the same sandboxed frames as Studio |

**Emergency takeover.** An open SOS (a critical `button` alert) or hazard (smoke, gas, CO) replaces the rotation:
- a pulsing red screen with what happened and who (see names below);
- where: the zone and floor, when the gateway is attached to a zone in the floor plan;
- how long ago, counting up, and a small 2D plan of the floor with the zone lit;
- a repeating alarm.

It stays until the alert is acknowledged, from the TV when allowed, or anywhere in Aether. The rotation resumes
afterwards.

Also on screen:
- a clock;
- the connection state ("สด" while the signal stream is up);
- a stale-data banner once the last successful refresh is more than a minute old. The TV keeps the last picture and
  reconnects by itself.

The screen is sized in viewport units for 16:9 at 720p and 1080p, keeps clear of overscan, and drifts by a few
pixels every two minutes against burn-in.

## 3. What a display may do

| | Member routes (`/api/v1/*`) | Kiosk routes (`/api/v1/kiosk/*`) | `/ws` |
|---|---|---|---|
| Display token | **403** `{"detail":"display_token"}` on every route | `session`, `board`, `floorplan`, `floors/:id/image`, `studio/:id`, `alerts/:id/ack` (only with `allow_ack`) | refetch signals |
| Member token | as today | 401 | as today |

The kiosk routes read through the same repository code as members, with a different identity:
- **Identity.** A display's transactions carry `app.display_id` and no `app.user_id`.
- **Scope.** `core.compute_project_scope()` gives a display its own projects, so the same RESTRICTIVE policies
  narrow what a TV sees. A display id together with a user id, or a revoked or unpaired display, gets an empty scope,
  which denies everything.
- **Read only.** Every display transaction is `SET TRANSACTION READ ONLY`. The only writes are the display's own
  access-log rows and, when allowed, an acknowledgement; for those the transaction is opened for writing on purpose.
- **Studio.** A studio dashboard must be in the display's playlist.

**Acknowledging from the TV** (`allow_ack`) applies only to what takes over the screen (a critical `button` alert or
a `hazard` alert; anything else answers 404). It sets `alerts.acked_by_display` (never `acked_by`, which names members)
and writes an `alert.acknowledged` audit row with the display as the actor. The alert must be in the display's
projects.

**Limits.**
- Pairing: 10 attempts per minute per client key (an IPv4 address, or the /64 of an IPv6 one), and a global budget
  of 3000 failures a minute across all clients. The budget is high on purpose: a low one would let anybody lock
  every TV out of pairing by failing on purpose. Against 31^8 codes that live 10 minutes, 3000 guesses a minute are
  about one success in 30 million per live code.
- The client key is the real visitor address in production: Cloudflare → nginx (`real_ip_header CF-Connecting-IP`
  for Cloudflare's ranges only, and only Cloudflare traffic is served; it overwrites `X-Forwarded-For` with
  `$remote_addr`) → Caddy (`trusted_proxies` = nginx only, strict; it sets `X-Forwarded-For {client_ip}`) → the API
  (`TRUSTED_PROXIES` = Caddy only). A header a client sends itself is overwritten at the first hop. Checked on
  2026-09-30 against the production configuration.
- The usual API rate limit applies to kiosk routes.
- At most 50 displays per workspace.

## 4. Personal data

A tag is **personal unless proven otherwise**. It may be named only when the workspace has at least one live
registration of it and none of its live registrations is roaming or uses a worn profile (wearables, emergency
buttons). The database decides (`core.display_impersonal_tags`, a definer, so a worn registration in a project the TV
cannot see still counts). Removed or never-registered tags, and tags re-registered as worn after their alert, are
personal. An emergency button alert is always treated as personal.

- **Names off (default).**
  - The names of personal tags are replaced by `ผู้สวมใส่ · <last 4 of the MAC>`.
  - Their alert titles are replaced by what happened ("กดปุ่มฉุกเฉิน").
  - No MAC is sent at all: every `external_id` on the board is a tag keyed per API process (HMAC), which still
    matches alerts to devices within the board.
- **Always.** Operators' notes and member ids (`acked_by`, `resolved_by`, `note`) never reach the TV.
- **Names on.** When the owner turns names on, every board read that included a name writes a row to the read-access
  log with the display as the actor (`actor_kind = 'display'`). Rows are deduplicated for 10 minutes like a member's.
  If the row cannot be written, the answer is withheld (503).
- **Studio dashboards.** Widgets on a TV get the same treatment:
  - alerts and events without member ids or notes;
  - without names, personal tags' names, titles, MACs and event details replaced;
  - never the presence (where a person is) of a personal tag, names or not.

  Opening a studio dashboard is logged the same way.
- **Floor plans.** Reading a floor plan is logged (`display_floorplan`). Without names, the labels of beds and desks
  (which may name who uses them) are removed; room and zone names stay, because they say where an emergency is.
- **Owner's view.** The privacy view names the display ("จอ · <name>") as the reader.

Tell the people who wear tags that a screen may show their name before turning names on (PDPA §23).

## 5. Trade-offs and follow-ups

- **Token storage.** The display token lives in the TV browser's `localStorage`, so script running on `/display`
  could read it. The page loads no third-party script and renders widgets in sandboxed frames with no script. A
  token only reads what the display may read, and revoking the display ends it at once.
- **Dedupe per process.** Like members' reads, the 10-minute log dedupe is per API process.
- **Reversed pairing** (follow-up). The TV would show a code and a signed-in admin would enter it, so nothing
  unauthenticated could guess a code at all. It needs a pending-pairing table, TV polling and an admin claim step;
  the limits above make guessing impractical in the meantime.
- **IP pinning** (follow-up). A display could be bound to the address it paired from. The admin page shows each
  display's last-seen time and address so an unexpected one stands out.

## 6. Operating notes

- **Device.** Any browser that runs WebGL works: a smart TV browser, a mini PC or a stick computer in kiosk mode
  (for example Chrome `--kiosk https://<origin>/display`). Pairing survives restarts because the token is in that
  browser's storage; clearing it, or a private window, means pairing again.
- **Polling.** The TV reads its board every 15 s, every 60 s while the signal stream is up, and at once on an alert
  signal. It re-reads its settings every minute and at once when they change.
- **Signals.** Only wall displays receive "display" signals. A TV whose settings change is told, and its stream
  closes, so it reconnects under the new project scope.
- **Rollback.** The 00044 Down migration drops displays, their access-log rows and `acked_by_display`.
