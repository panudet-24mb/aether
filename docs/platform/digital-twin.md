# Digital twin

The digital twin shows one building in 3D, live: every placed sensor and gateway with its latest reading, heat maps of
each room, how many people wear a tag in each zone, and every open SOS or hazard alert. The operator sees where things
happen without reading lists. It is the live side of the floor plan (`docs/platform/floorplans.md`): whoever may not see
the plans may not see the twin.

Status: phases **P0 (foundations)** and **P1 (live layer, demo scenario)** are built. Replay, history storage and the
TV embed are the roadmap below.

## What you see

| Layer | Source | How it is drawn |
|---|---|---|
| Building | the site's floors (walls, zones, items) | glass walls with accent edges, one merged mesh per floor; floors above the active one fade out |
| Devices | `floor_placements` + latest readings | instanced spheres (sensors) and boxes (gateways) on stems; accent online, amber stale/door open, coral alert |
| Temperature | readings of sensors placed inside each zone | a heat texture per floor (0.25 m texels, max 256²), inverse-distance weighted **within a zone only**, coloured by the distance from the zone's comfort band |
| Humidity | the same sensors | the same texture, one sequential scale for the building (the legend shows its range) |
| Occupancy | PIR motion in the last 5 minutes | the same texture, accent ramp |
| People | `presence_state` (the zone the ingest decided) | a glowing zone outline, a headcount, capsule tokens in fixed slots |
| SOS / hazard | open `button` / `hazard` alerts | a red light pillar, an expanding ring, the zone outline in coral; the camera flies there |

Keys: `1` overview, `2` top view, `3` active floor, `4` follow the alert, `5` slow orbit (tour), `N` the next open SOS.
A minimap (bottom left) shows the floor under the camera; click it to fly there. The legend (bottom right) shows the
colour scale; its lock keeps the scale fixed while values move. The table button opens every zone's numbers as an
accessible table.

### Honest positioning

Aether knows which zone a worn tag is in, not where in the zone. The ingest decides the zone from smoothed RSSI with a
6 dB margin and a 10 s dwell (`internal/alerts/zone.go`), so a zone change shows 10–20 s after the person moved. The twin
therefore never draws a person at a coordinate:

- a token stands in a **fixed slot** of a hex grid clipped to the zone (people sorted by id take the slots in order), so
  it never jitters as if it were moving;
- a zone change glides the token for 1.2 s at most ("moved to", not a walked path);
- a tag heard by a gateway that no zone lists gets a dashed ring around that gateway, never a room;
- the legend always says "ตำแหน่งระดับโซนจาก RSSI · ไม่ใช่พิกัดจริง" while the people layer is on.

**Temperature colours are relative to each zone's comfort band**, not one scale for the whole building (a cold store
at 5 °C would otherwise make every normal room look hot): inside the band is the accent, below it runs to blue, above it
to coral, full colour 4 °C outside the band. A zone's band is its own `comfort` (°C, low and high, in the floor layout;
the demo's cold store is 2–8 °C), else its kind's default: room, ward and other 22–25, corridor 22–26, restricted
(server rooms) 18–27, storage 15–25, outdoor 15–35. The legend says so, the data table lists each zone's band, and labels
always show the real °C. Setting `comfort` in the floor plan editor is P4 (today it comes from the API or `demo-twin`).

Heat never crosses a wall: a texel uses only the sensors placed inside its own zone, and a zone without a sensor is drawn
hatched and labelled as such, never extrapolated from the room next door. Readings fade out of the mix between 5 and 15
minutes old.

## People are personal data

`GET /api/v1/twin/sites/:id/state?people=counts|tracks|named`:

| Mode | What the response carries |
|---|---|
| `counts` (default) | a headcount per gateway; no identity anywhere in the response |
| `tracks` | per person a pseudonym, zone and times rounded to the minute; people sorted by pseudonym |
| `named` | the device name (usually the wearer), its id and exact times |

A tag is **personal** unless every live registration of it in the workspace, in any project, is neither roaming nor
registered with a worn or button profile (`core.twin_impersonal_tags`, the same rule as the TV display); an unknown tag
is personal. Outside `named`, a personal tag placed on the plan is called "แท็กบุคคล" with no `ext`, and an alert about
one (placed, zoned here, or heard here while its zone is elsewhere) gets a title that names nobody.

What `tracks` guarantees: a pseudonym is an HMAC with a salt drawn for that response only, so the same person gets
different pseudonyms in different responses and pseudonyms cannot be joined across responses. Within one response the
pseudonym links the person to their alert. Order (sorted by pseudonym) and minute-rounded times say nothing extra. It
does not hide that someone is in a zone, and a person alone in a zone can still be followed from zone counts over time;
that is the nature of presence data, and why replay of people waits for an owner setting (P2).

**Who gets which mode.** What a principal gets is the mode asked for, capped by the workspace's **twin settings**
(`core.twin_settings`, written only by an owner, audited as `twin.settings_changed:…`; a workspace with no saved settings
gets `counts`, a demo workspace `named`), by role (`named` needs owner, admin or operator; everyone else gets at most
`tracks`) and, for a wall display, by the settings' display cap (`display_people`, at most `tracks`). The live view never
drops below `counts` (headcounts are not identity); with `people_replay = off` replay shows no people at all. The
response says which mode it used (`domain.TwinPeopleFor`). Every read is written to the access log (`twin_live`,
subject the site), with the usual 10-minute dedupe; a read that returned names is logged as `twin_live_named`, so who
looked at named people is its own line.

Scope follows row level security: a site outside the caller's projects, or in another workspace, is 404. The floor plan
module set to `none` is 403 (`moduleFor`: `twin` maps to `floorplan`).

## API

`GET /api/v1/twin/sites/:id/state` returns `server_time`, `site_id`, `demo`, `layout_revision` (per floor, so the page
refetches the drawing when someone saves a floor), `devices`, `presence` and `alerts`; see `backend/api/openapi.json`.

- Devices: every live placed gateway and device; a device's values fold its last 8 readings from the last day (a BLE tag
  alternates frames, a Zigbee message reports only what changed), the same rules as `frontend/app/live/measurements.ts`.
  Online means an uplink within 120 s.
- People: roaming devices whose current zone gateway is placed on this site; a tag that went quiet is dropped unless it
  has an open SOS (the last place it was heard is where to run).
- Alerts: open and acknowledged alerts of anything on the site. `gateway_id` is where to look: a worn tag's current zone
  gateway, else the gateway that raised the alert.
- Limits: 2000 placements, 1000 people, 200 alerts. A 2 s in-process cache per principal (member, or display) and site
  absorbs several tabs and screens refreshing together; it holds the rows before any people mode is applied, and every
  response draws its own pseudonyms.

The page refetches on every realtime signal (alerts at once, others coalesced to one per 3 s) and polls every 20 s as a
safety net (6 s while the socket is down).

## History and replay (P2, P3)

### Storage (migration 00046)

| Table | What | Kept |
|---|---|---|
| `core.presence_history` | every zone transition of a worn tag: `at`, the gateway it moved to (NULL: heard by no gateway), the one it came from, the smoothed RSSI. Tag ids in lower case (a CHECK). Written in the same transaction as the zone events: the uplink collects its moves (`presenceRows`) and writes them in one INSERT under one savepoint, so it can never cost an event or an SOS and an uplink opens one subtransaction whatever the number of tags; a roaming tag going offline writes a NULL row, coming back where it was writes that zone again. Backfilled at migration from the zone events still in the (capped) event log. | `PRESENCE_HISTORY_DAYS` (default 30, 1–400), set only by the migration role like the access log's retention (`core.retention_policy.presence_days`); see Retention below |
| `core.sample_rollup` | 5-minute buckets per stream of **fixed sensors only**: sample count, avg/min/max temperature and humidity, any motion, the last door state, the lowest battery. A tag is rolled up only when it is registered and none of its live registrations is roaming or has a worn / button profile (`domain.WornProfileIDs()`, passed in by the worker: the rule of `core.twin_impersonal_tags`), so no bucket follows a person; a recomputed range deletes the buckets of any other tag (a sensor re-registered as a wearable loses them). Tag ids in lower case. The same reading rules as `frontend/app/live/measurements.ts` (a Zigbee- or Tuya-shaped reading takes values from `metrics`; a BLE reading only from an environment frame). | sample retention + 30 days; monthly partitions |
| `core.twin_settings` | per workspace, owner-written: `people_replay` (off/counts/tracks/named), `people_replay_days` (1–90, default 7), `display_people` (off/counts/tracks) | — |

`core.rollup_samples` (the api's worker, every minute, `RunSampleRollup`) rolls every active workspace up to the last
bucket that closed a minute ago, from the watermark minus one bucket (late arrivals are recomputed), at most 6 hours per
step, 250 ms apart while behind; the first start backfills the sample retention. It never runs on the ingest path. One
step is one pass over the time range for all workspaces (`core.rollup_range` reads the sample partitions it overlaps
directly, grouped by workspace; only the fixed-sensor list is read per workspace, from `core.devices`). It runs only
under a statement timeout (the worker sets 60 s; a function's own `SET statement_timeout` does not arm the timer of the
statement that called it, so the function refuses to run without one); a step that times out is retried with half the
span, down to 5 minutes. `core.rollup_samples_range` recomputes up to 7 days of the caller's own workspace at once (the
demo backfill). Late samples older than the previous bucket are not picked up by the worker.

Measured on PostgreSQL 18 with 8.7 M samples (20 workspaces × 150 tags, one sample a minute for 2 days, arrival
order, BRIN summarised): a 6-hour catch-up step (1.1 M samples, 173 k buckets) takes 8.8 s, the steady one-minute step
(two buckets) 0.15 s. The plan is a bitmap heap scan on the BRIN index of `received_at` (lossy, 32 k of 235 k blocks,
8 k rows rechecked away), a hash join with the fixed-sensor list, a sort and a group aggregate. `core.twin_num` has no
`SET` clause so it is inlined (with one it was called ten times per sample: 14.1 s for the same step).

Row level security: both tables carry `tenant_scope` and a restrictive `project_scope`; a restricted member sees a
movement when either end is one of their gateways, and rollup rows of their gateways. The runtime cannot write the rollup
(definer functions only) and can only append movements. The partition functions (`maintain_partitions`, `prune_history`,
`partition_health`, `partition_has_rows_between`) learned the two tables with every guard of 00037/00038 kept.

Erasure (migration 00047): erasing a tag's history (`core.erase_identity_data`) deletes its movement rows whose either
end is in scope (and the ones with neither) and counts them as `presence_history`, and its buckets on the gateways in
scope (`sample_rollup`: none are written for a worn tag, but a tag erased as a person's may have been rolled up while it
was registered as a fixed sensor). Both match the lower-case id, whatever case the registration uses. The tag export ZIP
carries `presence_history.ndjson`; `admin reapply-erasures` after a restore calls the same function.

Retention of the movement history: `core.prune_history` deletes rows older than `presence_days` in batches across every
range (by key; every maintenance run, hourly), and `core.maintain_partitions` drops whole expired months. Unlike the
other tables, a month holding rows is not held until a newer row of its own proves it expired (a month nobody moved in
would then outlive its retention); the sanity bound for personal data is `core.clock_corroborated()`: some uplink
(`sensor_samples` or `ble_history`) stored in the last 7 days. So a clock that jumped ahead on an idle system expires
nothing, and after a week without any uplink expiry of movement history pauses until the next one (maintenance logs
`drop_held`). Effective retention: `presence_days`, plus at most one maintenance interval, while the site is running.

Backdating: only the demo backfill writes the past (`CapturePacketAt`, demo workspaces only). The database refuses it
too: a trigger on `core.sensor_samples` rejects, for the runtime role, a sample received more than 15 minutes before its
transaction began unless the workspace is a demo (the `WHEN` clause keeps it free for every live row). Live captures
keep the api's clock, read under the gateway lock, like every other ingest path (Zigbee2MQTT, Tuya): switching only this
path to the database's `clock_timestamp()` would mix two clocks in one workspace's history.

### API

| Route | What |
|---|---|
| `GET /twin/sites/:id/timeline?from&to&people` | what history exists (`available`: env, people, markers), SOS / hazard / serious-alert markers (labels name nobody outside `named`), per-bucket density of alerts, door changes and people moving |
| `GET /twin/sites/:id/replay?from&to&bucket&layers&people` | the window as a keyframe at `from` plus changes: env per device and bucket (re-aggregated from the rollup), people (below), alerts open in the window |
| `GET /twin/people/:external/trail?from&to` | one person's zone changes; only when the caller may see names; never a display |
| `GET /twin/settings`, `POST /twin/settings` | the settings; POST owner only, audited |

People in a replay: `counts` sends headcounts only: `key_counts` at `from`, then per gateway the **net** change of each
bucket of the response (`bucket_sec`, at least 5 minutes), stamped at the bucket's end (`count_deltas`); zones whose
moves cancel out in a bucket send nothing. So no identity and no single move's time leaves the server (a −1 in one zone
and a +1 in the next at the same millisecond would be one person walking), and a headcount at T is the one at the end of
the last closed bucket. Counts are not suppressed below 2: a zone with one person shows one. Replay exists to review
incidents, where a lone worker is the case that matters, and a headcount of one carries no identity; the live view shows
the same. `tracks` sends one pseudonym per person drawn for that response only, with change times cut to the minute;
`named` sends tag ids, names and exact times. People are shown only back to `people_replay_days` (and the retention);
the timeline shades what lies before.

Tracks are linkable by design, and that is their privacy limit: within one response a pseudonym's moves are one person's
path (that is what tracks means), so whoever knows where someone was at one moment (their desk, the ward they were
called to) can follow that pseudonym through the rest of the window. Pseudonyms change per response, so two responses
cannot be joined by the id, but they can by the path itself (the same moves at the same minutes). That is why tracks are
logged on every request like names, never cached, capped for viewers and displays by the owner's setting, and why the
default is `counts`.

Limits: a window of at most 7 days and 288 buckets (auto picks 5 min, 15 min, 1 h, 3 h or 6 h), at most 1000 placed
devices (400) and 150 000 devices × buckets (checked before anything is read); more than 20000 people changes on this
site (only moves with an end at one of its gateways are read, so another site's crowd never makes a window dense) or a
request that runs out of time → `413 {"error":"too_dense","hint_to":…}`; 5 s per statement and 8 s per request
(`TwinReplayTimeout`); two timeline/replay/trail requests of one workspace at once (a third waits up to 3 s, then 429);
30 requests a minute per member (or display). With people `off`, the movement history is not read at all. Windows
wholly in the past (ending more than 10 minutes ago) are cached in the api (LRU, 64 MB counting keys and bodies, at most
8 MB per entry, 10 minutes, expired entries swept once a minute) keyed by tenant, member, role, project scope, the
workspace's erasure count (an erasure is never behind a cached window), the site's layout revision, when the settings
last changed, the effective mode and the window; `tracks` is never cached (its pseudonyms are per response). Responses
carry an ETag (`If-None-Match` → 304); `tracks` and `named` responses are `Cache-Control: no-store`, headcounts
`private, max-age=60`. A trail names only zones in the caller's projects (a move out of them reads as null).

Access log: `twin_timeline` and `twin_replay` are deduplicated like other reads; a replay with people (`tracks` or
`named`) is logged as `twin_replay_people`, a timeline with names as `twin_timeline_named` and the live view with names
as `twin_live_named`, each on **every** request, and every trail as `presence_history` with the person's tag as
subject.

### Replay in the page

The toolbar's สด / ย้อนดู switch opens replay on the last hour (presets 1 h, 6 h, 24 h, 7 days). `replay/store.ts`
indexes the window once and `stateAt(T)` rebuilds the state the live view draws (placements from the last live state,
values interpolated between buckets, people by binary search over each person's changes, alerts open, acknowledged or
resolved by T), so scrubbing and playing never call the server; the next window is fetched ahead past 70 % of playback.
`replay/timeline.tsx` is a canvas track (density lanes, SOS/hazard markers, shaded ranges without history, the
playhead) and a keyboard slider (`role="slider"`, Thai `aria-valuetext`): Space plays or pauses, ←/→ one bucket,
Shift+←/→ one hour, `[` `]` speed (1×/10×/60×), Home/End the ends; clicking a marker jumps there and flies to it. In
replay the camera never flies on its own; the data table shows the state at T; the time is announced politely every
10 s while playing. The owner's settings are in the Privacy Center (tab Digital twin).

## Frontend

`frontend/app/twin/`:

- `twin.tsx`: the page (toolbar: site, floor chips, layers, people mode, camera presets, floor spacing, isolate, data
  table, full screen; minimap, legend, selected-device card, SOS chip, `?twinDebug=1` overlay).
- `twin-view.tsx`: **`TwinView`**, the embeddable view. Contract (keep it stable for the TV display): `siteId`, `floorId`,
  `mode` (`live`), `layers`, `people`, `quality`, `tour`, `paused`, `onReady`, `onAlertFocus`, plus a `source` with
  `site(id)` and `state(id, people)` so a kiosk can use its own credential.
- `twin-plan.tsx`: zone summaries, the minimap and the 2D fallback (WebGL missing, or the low tier under 20 fps for 15 s).
- `engine/engine.ts`: `TwinEngine`, plain three.js. Renders on demand (0 fps idle); continuous frames only while the
  camera moves, a tween runs, a token glides or an SOS pulses. Quality tiers: `high` (DPR ≤ 2, antialias, bloom on
  emissives), `med` (DPR ≤ 1.5, no bloom), `low` (DPR 1, no fog, fewer segments); `auto` drops a tier when a 5 s average
  is under 30 fps. Labels are a pool of at most 40 DOM nodes projected each frame, not sprites.
- `engine/building.ts`: the building geometry, shared with the floor plan's 3D view (`floorplan/view3d.tsx` calls it
  with its old behaviour; the twin turns devices, people, labels and zone tints off and asks for merged glass walls).
- `engine/heat.ts`, `engine/slots.ts`: pure functions (IDW, zone masking, ramps, token slots), checked by
  `twin.selftest.ts` (run instructions in its header).

Accessibility: the canvas is `role="application"`; the table view lists every zone's numbers; an `aria-live="assertive"`
region announces "SOS · zone · floor · time"; `prefers-reduced-motion` turns fly-tos into cuts, glides into instant
moves and pulses into static rings; colours are colour-blind-safe ramps with at least 3:1 contrast on the stage, and
every state also has a text label.

### Measured (P1, Chrome on an Apple Silicon Mac, `?twinDebug=1`)

| Scene | 1280×720 | 1920×1080 |
|---|---|---|
| Demo building (70 placed, 40 people) | 60 fps · 55–60 draw calls · 33k triangles | 60 fps · 59 draw calls · 33k triangles |
| Budget load (570 devices, 50 people) | 60 fps · 59 draw calls · 205k triangles | 60 fps · 59 draw calls · 205k triangles |

60 fps is the display's refresh cap. Budget: under 150 draw calls, under 400k triangles, 0 fps idle.

## Demo workspace

`cmd/demo-twin` builds a fictional three-floor hospital in a **separate demo workspace**, so row level security keeps it
away from real data, and drives it through the real HTTP ingest:

```sh
# once: creates the workspace and marks it demo with the migration role. It prints the database hosts first, checks
# that migration 00045 is applied before creating anything, and writes the owner login and gateway tokens to a new
# 0600 state file (never over an existing file or through a symlink; kept even if setup fails halfway).
DATABASE_URL=… MIGRATION_DATABASE_URL=… DEMO_STATE=./demo-twin.json go run ./cmd/demo-twin setup
# the 30-minute script, one uplink per gateway every 6 s (100 requests a minute, under the ingest limit).
# Plain http:// to anything but localhost is refused (the gateway tokens travel with every uplink) unless --insecure-http.
AETHER_ORIGIN=https://… DEMO_STATE=./demo-twin.json go run ./cmd/demo-twin run
# on demand, on top of the script
go run ./cmd/demo-twin trigger sos      # the scripted wearer presses the B10 in ward 3
go run ./cmd/demo-twin trigger spike    # the cold store warms to 12 °C for 5 minutes (needs a running run)
go run ./cmd/demo-twin trigger door     # someone at the cold-store door (PIR) for 3 minutes (needs a running run)
# history for replay: plays the script over the last N hours straight into the database as if it had happened then
# (CapturePacketAt, demo workspaces only, never in the future), resolves each loop's SOS three minutes later, and
# recomputes the rollup of the range. Run it on a fresh demo workspace, before run.
DATABASE_URL=… DEMO_STATE=./demo-twin.json go run ./cmd/demo-twin backfill --hours 24 [--every 30s]
```

The building: 3 floors, 14 zones, 10 MG3-style gateways, 52 S1 environment sensors, 8 MSP01 PIR sensors and 40 people
with B10 wristbands (roaming), fictional Thai names. The script (`internal/simulation/twin_scenario.go`, deterministic):
nurses, doctors and staff walk their rounds; minute 8 a patient walks into the restricted server room (zone rule);
minute 12 SOS in ward 3; minute 18 someone at the cold-store door; minutes 20–26 the cold store warms to 12 °C and back
(threshold rule at 8 °C). All MACs start with `f1` and every row is marked `aether_source: simulated`.

A demo workspace's alerts are never delivered: `ClaimNotifications` returns nothing for it whatever channels exist. A
channel's own "send test" button still sends (that is how a channel is checked). The screens of a demo workspace carry
no demo wording (it is shown to customers): the only reminder is one owner-only line on the alert channels page, and
the SIM markers that real workspaces keep for their simulated devices are hidden for a demo workspace
(`frontend/app/demo-mode.ts`). The flag is set only by the migration role (migration 00045 narrows `aether_app`'s
INSERT on `core.tenants` to `(id, name)`).
With `ALERTS_SHADOW=true` (production) only the SOS and the hazards (smoke, gas, CO) open alerts; the zone and threshold
rules show only as events.

### Every device family (`extend`)

`extend` adds to an existing demo workspace everything Aether supports, through the same code paths the API uses, and is
idempotent (re-running adds only what is missing). It refuses a workspace that is not demo.

```sh
DATABASE_URL=… MIGRATION_DATABASE_URL=… DEMO_STATE=./demo-twin.json AETHER_ORIGIN=https://… go run ./cmd/demo-twin extend
# the driver then also connects to the broker as the Zigbee coordinator and the Aether Edge (TLS, the MQTT CA):
DEMO_MQTT_URL=ssl://mqtt:8883 DEMO_MQTT_CA=/run/mqtt-ca.crt DATABASE_URL=… AETHER_ORIGIN=… DEMO_STATE=… go run ./cmd/demo-twin run
go run ./cmd/demo-twin pair   # a new pairing code for the control-room TV (the old TV is signed out)
```

- **Minew over the HTTP gateway ingest:** an MG4 gateway in the lab (S1 "PLUS", MSP01, S4), E8S on infusion pumps and a
  wheelchair, MBT01 anti-tamper on the controlled-drug cabinet and the server rack, C10 and B7 wearables (roaming).
- **Zigbee (a simulated Zigbee2MQTT coordinator over MQTT):** 52 devices built from 31 real zigbee-herdsman-converters
  definitions (`internal/simulation/z2m_demo_definitions.json`, exported by `infra/export-z2m-demo-definitions.mjs`):
  Tuya TS0001–TS0004, TS011F plugs, TS0601 thermostat / curtain / presence radar / air quality / smoke, TS0207, TS0201,
  WSD500A, TS0203, TS0202; Aqara WSDCGQ11LM, MCCGQ11LM, RTCGQ11LM, SJCGQ11LM; IKEA LED1623G12, ICTC-G-1, STYRBAR, STARKVIND;
  Philips Hue motion; SONOFF SNZB-01..04; Heiman HS1SA-E, HS1CG, HS1CA-E. `bridge/devices` is published as Zigbee2MQTT
  does (model id and manufacturer from each definition's zigbee model or fingerprint); every state follows the
  definition's exposes (keys, ranges, enums, on/off values) plus linkquality, battery and last_seen.
- **Tuya Wi‑Fi (a simulated Aether Edge over MQTT):** a vaccine fridge plug, an air purifier, a 3-gang switch, a lamp, an
  air conditioner and an air-quality monitor.
- Tuya Cloud and Tuya BLE are not simulated: their flags are off in production and the API refuses those gateways.

Both simulated gateways apply `/set` commands and report the new state, so a switch pressed in the twin, the overview or
a Studio controls panel is confirmed like a real one (manual commands need the owner, admin or operator role; they do
not depend on `AUTOMATION_COMMANDS`). A command holds for 15 minutes, then the script takes over again.

The 30-minute script adds: lights by shift, plugs with a real load profile, doors on the pharmacy, cold store, server
room and lab, corridor motion, ward air quality, the ICU thermostat, a leak in the lab (minutes 15–18) and a smoke test
(minute 27), once per loop. The driver resolves the demo workspace's alerts 4 minutes after they open (with
`DATABASE_URL`), so a wall TV's takeover ends by itself. `backfill` covers the Minew rows (MG3s and the MG4); Zigbee and
Edge history is not backfilled, because their capture takes the arrival time.

`extend` also creates four Studio dashboards (environment; energy and safety; people and events; switches and plugs,
the last one made of first-party controls panels) and the display "จอห้องควบคุม" (overview, Digital twin, alerts, the four
dashboards, floor plan, people by zone, device status; names shown; acknowledge allowed), and prints its pairing link
`/display#code=…` (valid 10 minutes). The Tuya local keys it stores are random and sealed with a throwaway secret: the
simulated Edge never fetches them.

## Roadmap

| Phase | What | Status |
|---|---|---|
| P0, P1 | Engine, live view, demo workspace | done |
| P2 | History storage: `presence_history`, `sample_rollup`, `twin_settings` | done (00046, 00047) |
| P3 | Replay: timeline, replay, trail, settings; time scrubber | done |
| P4 | Polish | bloom tuning, keyboard map, contrast audit, axe scan |
| P5 | TV embed | done: the display playlist's "twin" view (`GET /api/v1/kiosk/twin/sites/:id/state`), kept mounted and `paused` between turns, people clamped to `display_people` |
| P6 | Backfill | done with P3: `CapturePacketAt` and `demo-twin backfill` |
