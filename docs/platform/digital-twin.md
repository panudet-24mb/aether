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

**Interim rule until `twin_settings` exists (P2):** `named` is granted only in a demo workspace (`core.tenants.demo`)
and only to owner, admin and operator; everyone else gets `counts` whatever they ask for. The response says which mode it
used. Every read is written to the access log (`twin_live`, subject the site), with the usual 10-minute dedupe; a read
that returned names is logged as `twin_live_named`, so who looked at named people is its own line.

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
```

The building: 3 floors, 14 zones, 10 MG3-style gateways, 52 S1 environment sensors, 8 MSP01 PIR sensors and 40 people
with B10 wristbands (roaming), fictional Thai names. The script (`internal/simulation/twin_scenario.go`, deterministic):
nurses, doctors and staff walk their rounds; minute 8 a patient walks into the restricted server room (zone rule);
minute 12 SOS in ward 3; minute 18 someone at the cold-store door; minutes 20–26 the cold store warms to 12 °C and back
(threshold rule at 8 °C). All MACs start with `f1`, every row is marked `aether_source: simulated`, and the twin shows a
DEMO DATA badge.

A demo workspace's alerts are never delivered: `ClaimNotifications` returns nothing for it whatever channels exist. A
channel's own "send test" button still sends (that is how a channel is checked). Every page of a demo workspace shows a
DEMO strip saying the data is fictional, that gateways and devices added there belong to the demo, and that
notifications are not sent. The flag is set only by the migration role (migration 00045 narrows `aether_app`'s INSERT
on `core.tenants` to `(id, name)`).
With `ALERTS_SHADOW=true` (production) only the SOS opens an alert; the zone and threshold rules show only as events.

Honest limits of the demo: the "door" is a PIR at the cold-store door, because no HTTP-ingest sensor reports a door
state (Minew S4 doors are learned signals, Zigbee doors arrive over MQTT). The first uplink after `run` starts leaves out
the faint second gateway, so every tag's first zone is its real one.

## Roadmap

| Phase | What | Notes |
|---|---|---|
| P2 | History storage | `core.presence_history` (zone transitions, monthly partitions, PDPA erasure), `core.sample_rollup` (5-minute buckets filled by a worker), `core.twin_settings` (people replay off/counts/tracks/named, retention, display cap); replaces the interim named rule |
| P3 | Replay | `/twin/sites/:id/timeline` and `/replay` (keyframe + deltas), time scrubber with SOS/alert markers, 1×/10×/60×, people replay logged `always` |
| P4 | Polish | bloom tuning, keyboard map, contrast audit, axe scan |
| P5 | TV embed | `twin-embed` for the display playlist: keep-alive with `paused`, display principal clamped to counts |
| P6 | Backfill | `CapturePacketAt` and `demo-twin backfill` (demo workspaces only) so replay has history at once |
