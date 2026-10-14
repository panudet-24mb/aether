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

**Illustrative walking (immersive style).** In the immersive style people are drawn as walking figures (see "Lifelike
hospital"). That walking is illustration, not data: a figure only ever wanders inside its **current** zone, between
random points and spots by the furniture, so the room looks inhabited; its path and its place in the room say nothing.
The legend adds "ท่าทางเดินและชุดเป็นภาพประกอบ" (outfits are illustration too).

What a figure is keyed by, and so what moves on a refresh:

- `counts` and `tracks`: a figure belongs to a **slot** (`gateway#007`, an SOS taking the first index), never to a
  person or a pseudonym (a pseudonym changes with every response). A refresh moves nobody and re-dresses nobody; a
  headcount going up adds a figure at its slot, going down removes the last one. A person moving between zones shows
  as one figure leaving one zone and another appearing in the other: there is no walk, because nothing links them.
- `named`: a figure belongs to its tag. A zone change walks it to the edge the two zones share and into the new zone,
  at a pace that finishes in about 3 s, and only when that path stays inside the two zones (each leg is checked, so it
  never crosses a wall or a third room); otherwise, or on another floor, it appears at its new slot.

Figures wander around furniture (operator-drawn and generated), and a slot covered by furniture moves to the nearest
free spot. `counts` figures carry no label (their outfit comes from a hash of the slot key, which carries no
identity); `tracks` and `named` keep their pseudonym or name only where the classic view shows one (an SOS). A person
who pressed SOS kneels and waves at their slot in their current zone, under the red pillar. With motion reduced,
figures stand still at their slots.

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

## Building model (3D)

A site (building) may carry **one** 3D model, a CAD export that the browser converts to binary glTF 2.0 (`.glb`). The
twin draws it over the floor plan or in its place. The server stores and serves the GLB and never parses the geometry.
It checks the container header and the checksum only.

### API

Paths are under `/api/v1`. Reads need the same rights as `GET /sites/:id`. Writes need the floor-image rights (owner or
admin, `CanManageDevices`). A site in another workspace, outside the caller's projects or archived is 404. The floor
plan module set to `none` is 403 (`moduleFor`: `sites` maps to `floorplan`).

| Method and path | Who | What |
|---|---|---|
| `GET /sites/:id/model` | read | `{"model": null}` or `{"model": {revision, name, source_format, size, sha256, uploaded_at, transform}}` |
| `GET /sites/:id/model/file?rev=N` | read | the GLB (`model/gltf-binary`, `nosniff`, `ETag: "<sha256>"`; `Cache-Control: private, max-age=86400` with `rev`, `private, no-cache` without); `If-None-Match` gets 304; 404 when there is no model or `rev` is not the current revision; 503 with `Retry-After` when too many downloads are running |
| `POST /sites/:id/model/uploads` | manage | `{name, source_format, size, sha256}` gets 201 `{upload_id, chunk_size, chunks}` |
| `POST /sites/:id/model/uploads/:upload/chunks/:index` | manage | `{data_base64}` gets 204; sending the same index again replaces it |
| `POST /sites/:id/model/uploads/:upload/commit` | manage | checks the upload and makes it the model, gets 200 `{model}`; 503 with `Retry-After` when 3 commits are already running |
| `POST /sites/:id/model/uploads/:upload/cancel` | manage | deletes an unfinished upload, whoever started it (the model is untouched), gets 204; audited as `site.model.upload_cancelled` |
| `POST /sites/:id/model/transform` | manage | a transform gets 200 `{model}`; 404 when there is no model |
| `POST /sites/:id/model/delete` | manage | 204; also removes an upload in progress; 204 when there is no model |

The client fetches the file as `…/model/file?rev=<revision>`. The revision goes up on every commit and never repeats for
a site, including after a delete (the counter is `core.sites.model_revision`). A `rev` that is not the current
revision is 404, so another revision's bytes are never cached under its URL. A cached file is therefore never stale,
and the client never revalidates within the day.

**Transform.** Fields that are left out take their default. An unknown field, a wrong type or a value out of range is
400.

| Field | Range | Default |
|---|---|---|
| `scale` | 0.0001 to 10000 (model units to metres) | 1 |
| `up` | `y` or `z` | `y` |
| `rotation_deg` | `[x,y,z]`, each -360 to 360 | `[0,0,0]` |
| `offset_m` | `[x,y,z]`, each -100000 to 100000, metres from the floor plan origin | `[0,0,0]` |
| `opacity` | 0.05 to 1 | 1 |
| `visible` | bool | true |
| `mode` | `overlay` or `replace` (the model is drawn instead of the drawn walls) | `overlay` |
| `clip_to_floor` | bool | true |

### Upload, step by step

Each request body must stay under the API body limit (256 KiB in Fiber and 288 KiB in Caddy), so the file goes up in
chunks:

1. **Start.** The client sends `name` (at most 120 characters; the server removes the path and any control or format
   characters, such as bidi overrides), `source_format` (`glb`, `gltf`, `obj`, `fbx`, `stl`, `3dm` or `ifc`, meaning
   what the browser converted from), `size` (20 B, the GLB header, to 32 MiB) and the `sha256` of the whole GLB. The
   server first drops every upload of the workspace older than one hour. While **another member's** unfinished upload
   to the same site is less than an hour old it answers 409
   `{"error":"upload_in_progress","upload_id":…,"started_at":…}`: it is never cancelled behind their back, and the page
   can offer to cancel it with `…/uploads/:upload/cancel`. It replaces the caller's own unfinished upload to the site,
   and answers 409 (`conflict`) when the workspace still has 4 uploads open.
2. **Chunks.** `chunk_size` is 163,840 bytes (160 KiB). Its base64 JSON body is about 213 KiB. Every chunk except the
   last must be exactly `chunk_size` bytes, and the last chunk makes up `size`. A 32 MiB file is 205 chunks. The
   per-IP `/api` limiter (`API_RATE_LIMIT`, 120 a minute by default, shared with the page's other requests) can
   answer 429 in the middle of an upload. The client then waits for the `Retry-After` header and resends the same
   index. The API's `ReadTimeout` (10 s) covers a whole request body, so a chunk needs an upload rate of roughly
   175 kbit/s or more; a slower link sees the chunk fail and should retry it.
3. **Commit.** The commit runs in one transaction. The server checks that every chunk is present, the total size, the
   sha256 of the concatenated chunks and the GLB v2 container, reading the chunks one at a time in order (a chunk
   write locks the upload row `FOR KEY SHARE` and the commit `FOR UPDATE`, so no chunk changes between these checks
   and the bytes stored; the bytes are concatenated once, by the INSERT). At most 3 commits run at once across all
   workspaces (transaction advisory locks); without a free slot the commit answers 503 with `Retry-After: 10`. The
   checks are: the magic `glTF`, version 2, the header length equal to
   `size`, and a first chunk of type JSON that fits inside the file. It then replaces the site's model, raises the
   revision, keeps the previous transform (or uses the default when there was none) and deletes the upload. These
   checks run before anything is written, so a rejected file costs no WAL. The commit emits the realtime `inventory`
   signal, the same one a floor save emits, so open twin pages refetch. A transform change and a delete emit it too.

An upload that is not committed within one hour is refused (404). It is deleted by the next upload start of the
workspace, or by `core.sweep_site_model_uploads()` (owner-only `SECURITY DEFINER`, called by the API's hourly
maintenance loop next to partition maintenance), so an abandoned upload is gone within two hours.

### Storage (migration 00048)

- `core.site_models`: one row per site with the metadata, the `transform` (jsonb) and the `data` (bytea). The column
  uses `STORAGE EXTERNAL` (uncompressed), so `GET …/file` streams it in 1 MiB slices (`substring`, each in its own
  short transaction under the caller's row level security and the request's context values, pinned to the
  revision): a download holds one slice in memory, not the file. At most 16 downloads run at once, 4 per workspace;
  above that the API answers 503 with `Retry-After: 5`. A download that averages under 32 KiB/s after its first 30 s
  is dropped, so slow clients cannot hold the slots.
- `core.site_model_uploads` and `core.site_model_chunks`: an upload in progress and its chunks. Both are **UNLOGGED**:
  in-flight bytes are never written to the WAL or the PITR archive. **An unfinished upload does not survive a crash
  or a restore**: the tables come back empty, the next chunk or commit is 404, and the client starts the upload again.
- All three tables follow the floor plan tables: ENABLE and FORCE RLS, `tenant_scope`, a restrictive `project_scope`
  through the site (`core.site_in_scope(site_id)`), composite tenant foreign keys (a chunk's key also pins it to its
  upload's site) and grants to `aether_app` only. Commits, transform changes and deletes are audited as
  `site.model.uploaded`, `site.model.transformed` and `site.model.deleted`.

**Why the cap is 32 MiB.** A committed model is written to the WAL once and from there to the PITR archive. Replacing a
model writes the whole file again, and the old row stays on disk until vacuum. The production disk recently filled up
from WAL volume. Keep CAD exports small: decimate the geometry before export. The browser converter writes uncompressed GLB (three.js
has no Draco or meshopt glTF encoder) and refuses models above 300,000 triangles (see "Building model import").

**Download time.** The connection's 15 s write deadline moves forward while the client keeps reading, up to 10
minutes per download. A 32 MiB model therefore needs about 55 KiB/s (450 kbit/s); smaller models need less, down to
the 32 KiB/s floor. A model replaced in the middle of a download ends that
response short of its `Content-Length`; the client refetches with the new revision.

### Privacy, export and erasure

A building model is geometry, not personal data. It is not part of a member's data export (`/me/export`), a device
privacy export or an erasure, and reading it is not written to the access log. It leaves with its site: when a site is
deleted, the foreign keys (`ON DELETE CASCADE`) remove the site's model and uploads. An archived site keeps its model
but serves it to nobody. Do not upload models that carry personal data in their metadata or names (for example a CAD
layer named after a patient).

## Immersive view and building model (UI)

### View style

The toolbar's **คลาสสิก | Immersive** switch picks the look; the choice is kept per browser (`aether.twin.style`,
default `classic`). Classic is the rendering described above, unchanged. Both styles share one `TwinEngine` (no second
WebGL context): `setStyle` swaps lights, stage and markers in place. `TwinView` takes it as an optional `style` prop
whose default is `classic`, so the TV display draws exactly what it drew before.

Immersive (`engine/immersive.ts`), every colour read at runtime from the app's tokens (`theme.ts` paints each oklch token
on a 1×1 canvas and reads it back as sRGB, since three.js cannot parse oklch):

- image-based light from a PMREM `RoomEnvironment`, a dark slightly metallic floor that fades out at its rim, an
  additive grid in `--color-accent` (1 m and 5 m lines, fading with distance), exponential fog in `--color-paper` and a
  CSS vignette (no extra render pass);
- glass walls with a cool sheen and accent edges bright enough to bloom; bloom stays on the high tier only (a little
  stronger than classic);
- devices as status beacons: an unlit core and a ring on the floor in the status colour (online `--color-accent`, stale
  or door open `--color-warning`, alert `--color-danger`, offline `--color-dim`, a switched-off output a dimmed
  accent), gateways as hub towers in `--color-blue` standing up to their mounting height. Picking works as in classic.
- Device-to-gateway link lines are **not drawn**: the live state carries no device→gateway link, and a guessed line
  (nearest gateway, zone gateway) would state a topology that may be false.

**Ambient motion** (the ≋ button, `aether.twin.motion`, **on by default** in the immersive style; a browser that turns
it off keeps it off): walking figures, smoke and mist, airflow, swinging doors, a slow sweep across the grid, breathing
beacon rings and the pulse of a device alert's ring. It draws at most 30 fps (60 fps on the high tier while figures
walk: a 30 fps walk reads as stop motion), is never counted towards the quality tier's frame rate, and stops when motion is reduced, the tab is hidden, the view is paused (a TV playlist) or the
button is off, and 2 minutes after the last pointer or keyboard input or meaningful state change (a switch, a door or
an online state changing, an alert opening); input or such a change resumes it. People moving (headcounts) is not a
meaningful change, so a busy hospital's wall screen still idles at 0 fps.
Without it the scene is idle at 0 fps and figures stand still; a device alert then shows a solid ring (an alert can stay open for
hours and is no reason to keep rendering). SOS and hazard pillars keep their own pulse in both styles.

### Lifelike hospital (immersive)

The immersive style draws the building as a furnished, inhabited hospital (`engine/lifelike/`). Classic and the TV
display are unchanged. Everything is generated in the browser from the floor plan and the live state: **no downloaded
models** (no third-party assets and no licensing questions; three.js ships no CC0 furniture or humanoid that would
fit).

- **Interior** (`interior.ts`, active floor): each zone is classified from its kind and name (ICU, ER, ward, lobby,
  nurse station, lab, pharmacy, cold store, server room, corridor, storage, office) and furnished accordingly: beds
  with rails, bedside monitors and IV poles (ward, ICU, ER), a reception counter and rows of waiting chairs (lobby), a
  nurse counter with screens, carts and shelves, lab benches with instruments, pharmacy shelving, cold-store fridges,
  server racks, corridor benches, plants. Plan items the operator drew (beds, desks, stairs, lifts, racks) are drawn as
  furniture in their place and the generated pieces fit around them. Placement is deterministic (seeded by the zone's
  id), inside the zone, along its walls or in a grid, at least 1 m from doors and 0.45 m from any placed device. The
  furniture is a few hundred primitives in four instanced meshes. A customer building model that is shown replaces it
  (the model has its own interior), and in "replace" mode its doors and windows replace the drawn ones too; people
  then walk around nothing.
- **Fixtures** (`fixtures.ts`): a device's look comes from its class and name. Wall switches, sockets (switched devices
  that meter power: the appliance's plug), wall air conditioners and thermostats snap to the nearest wall of their zone
  (projected onto it, off by half the thickest wall there) at 1.2 m, 0.3 m, 2.4 m and 1.5 m. A switch is a plate whose
  rocker tilts and takes a hint of the accent when on, a socket has a glowing indicator, an air conditioner is a split
  unit with a dark outlet and a louver that opens and breathes soft airflow while it runs, a purifier puffs upwards, a
  cover is a blind that hangs down as far as it is closed. The live view carries no state for climate and cover
  devices, so in this style the page reads the controls of at most 12 of them (the active floor first) when it opens
  and every minute (an AC's `switch`, a thermostat's `system_mode`, a cover's `position`): two requests at a time, 10 s
  each at most, none while the tab is hidden or the scene's idle pause is on, never two loads at once (a tick that
  finds one running is skipped), a device that fails backed off (1, 2, 4 minutes) and dropped after 3 failures in a
  row, everything aborted when the view closes. Each load replaces the states it read (a device that failed or was
  dropped goes back to its default look), and an older load never lands over a newer one. Each read is a `device_controls` row in the access
  log, deduplicated per member and device for 10 minutes. Without that a blind hangs half down and an AC stays still. Lights are ceiling panels that glow with a pool of light on the floor when on; their glow is kept under the
  bloom threshold and bloom fades in with camera distance, so a close look does not turn them into white blobs. Sensors keep a small
  status dot where they really hang: environment sensors on the wall at 1.6 m, PIR and smoke, gas and CO detectors on
  the ceiling, door sensors on the frame of the nearest door. A fixture shows its status like a beacon would: an
  alert tints it coral under the pulsing ring, stale data amber, no contact grey, and a light in doubt is neither lit
  nor dark. **Clicking a fixture** (primary button only) first selects the device: its card opens and its badge takes
  keyboard focus. A second, deliberate click (400 ms or more after the selecting one) on that switch, light or purifier
  runs the same command flow as the badge's power button (pending, sent, confirmed or a Thai reason, the same
  permissions, nothing in replay); a fast double-click, or a fixture selected from its card or the list rather than by
  a click on it, only selects. A socket never
  switches from the canvas (a fridge or a monitor may be plugged in): only from its badge or card. Where a sensor and a
  light panel are both under the pointer, the sensor wins. The badges stay as the accessible control and float above
  the fixture.
- **People** (`crowd.ts`): stylised figures about 1.75 m tall (head one 7.5th of it, shoulders wider than hips) built
  from rounded parts: capsule legs and arms, a tapered torso with rounded shoulders, neck, head with two eyes and a
  hair cap, hands and shoes; a coat to mid-thigh or a hospital gown to the knee. Each part is one instanced mesh (12
  for up to 150 figures), with a walk cycle and an idle pose. Outfits (scrubs, white coat, patient gown, visitor) are
  chosen by a hash of the person's key (the data has no role). Figures more than 25 m from the camera, and every figure
  on the med and low tiers, are plain capsules; past 150 people the rest stay the plain tokens. The walking rules are under "Honest positioning".
- **Effects** (`effects.ts`), each one a reading or an open alert: an open hazard alert (smoke, gas, CO) puts a smoke
  plume at its detector under a flashing ceiling alarm: soft, feathered puffs sized in metres that rotate slowly, grow
  as they rise, fade in and out, lean with a slow drift and spread under the ceiling, dark and dense at the source and
  lighter as they spread (gas and CO stay low, tinted yellow-green); an open leak
  alert puts a rippling puddle under its sensor; a door with a door sensor swings open with it and turns amber when
  open for more than a minute; a zone 3 °C or more above its comfort band shimmers; a cold store has a low mist; a room
  whose PIR saw motion in the last 5 minutes gets a soft light; windows glow warm from 19:00 to 06:00 in the site's time
  zone (the deployment default, Asia/Bangkok, while sites carry none). Shimmer covers the zone's own shape and uses
  fresh readings only, weighted like the heat map (none older than 15 minutes).
- **Motion and budget**: walking, particles and swinging run only as ambient motion (above). With motion reduced
  figures stand in their slots, plumes, mist and airflow hold still and doors snap, nothing flashes. On the med and low
  tiers particle counts drop and figures become capsules. The walls are frosted rather than clear in this style, so
  wall fixtures have a wall to sit on, and the operator may zoom in to 1 m (classic keeps 4 m). Measured on the demo
  hospital (3 floors, 44 people, 1440×900, Chrome on an Apple M4 Pro, high tier): all floors 78 draw calls, 61k
  triangles, 60 fps while figures walk; one floor isolated 48–54 draw calls and 18–26k triangles, 57–60 fps; a close-up
  among detailed figures 75–81 draw calls and 113k triangles; with a smoke plume open 90 draw calls and 90–112k
  triangles at 60 fps. A detailed figure is about 1,300 triangles, so 150 of them near the camera stay inside the
  budget: under 150 draw calls and 400k triangles. The furniture of one floor is 6–8k triangles.

### In-scene badges and buttons

In immersive, devices on the active floor carry a floating badge: status dot, name and key value (on/off and watts for
outputs, ประตูเปิด/ปิด, °C with %, %), positioned by the engine each frame from a keyed pool of at most 24 DOM nodes
(labels get the rest of the 40-node budget; an SOS label is never the one dropped). Alerting devices (any floor) and the
selected device always show. A badge that would cover another badge or a zone label is lifted on a longer leader line,
up to twice; if it still collides it gives way by priority (alert, selected, controllable, has a value, stale) and then
distance; badges far past the camera's target hide when zoomed in. A badge holding keyboard focus never hides.

Each badge is a real `<button>` (Thai `aria-label`: name, value, status) that selects the device and opens the side card
with the full controls. A controllable device (`kind = device` with a `class`) also gets a power switch
(`role="switch"` with `aria-checked`, a stable label "สวิตช์ <ชื่อ>"; with the state unknown a plain button "เปิด/ปิด <ชื่อ> (ยังไม่ทราบสถานะ)"). It runs the side card's command flow
(`quick-toggle.ts`): read the device's controls, queue one command per output with a fresh idempotency key, poll until
the device's own report settles it (20 s at most), then refetch the state. The badge shows กำลังสั่ง… → รออุปกรณ์ยืนยัน… →
the confirmed state, or a short Thai reason for a refusal (no right to command, offline, rate limited, timeout: the
state stays as reported). A member known not to command devices (not owner, admin or operator, or `control` limited)
gets badges without switches; the server decides in every case. In replay the switches are disabled.

### Building model import

The toolbar's **อาคาร 3D** opens the model panel (`twin-model.tsx`). Upload, alignment save and delete are shown to
owners and to admins whose floor plan access is not read-only (the server's rule); everyone may preview opacity,
visibility and clipping locally. A refusal (403) is explained in Thai.

1. **Pick** one model file, with its companions for formats that reference other files: `.glb`; `.gltf` together with
   its `.bin` and textures (matched by file name through a `LoadingManager` URL modifier; a missing `.bin` is named in
   the error); `.obj` with an optional `.mtl` and textures; `.fbx`; `.stl`. DWG/DXF, IFC, Rhino `.3dm`, Revit,
   SketchUp, Blender, Collada, 3DS and PLY are refused with how to export them (GLB, OBJ or FBX). IFC is not supported in
   this version; `.3dm` would need three's Rhino loader, which downloads its wasm from a CDN.
2. **Convert** in the browser (`model-import.ts`, loaded on demand with every loader): glTF with Draco and meshopt
   decoding (the decoders ship with the app's own assets), OBJ/MTL, FBX, STL. Lights, cameras and animations are
   dropped, Phong/Lambert materials become standard ones, every mesh is baked into world space and merged per material
   (`engine/model.ts` `flattenModel`), and the result is written as one binary GLB (`GLTFExporter`, textures at most
   2048 px). The model must fit the scene budget: at most **300,000 triangles** (counted before anything is cloned,
   an instanced mesh counting every instance; more is refused with how to fix it) and at most **32 draw groups**
   (past that, the smallest material groups are folded into one vertex-coloured mesh, with a notice). The selected
   files may total at most 256 MiB (checked before they are read). After conversion the GLB must be at most 32 MiB.
   The converter does not write Draco or meshopt compression (three.js has no glTF encoder for them): decimate in the CAD tool to stay under the cap.
3. **Upload**: SHA-256 with WebCrypto, start, every chunk in order at the server's `chunk_size`, commit. A failed chunk is
   resent up to 3 times; a 429 from the per-IP limiter is waited out for its `Retry-After` and the same index resent,
   never counted as a failure ("รอคิวอัปโหลด…" shows meanwhile); a 409 (another member's upload to this site is under an
   hour old) says so in Thai; progress shows in the panel. **ยกเลิก** aborts the request in flight and any wait; after a cancel or a failure the
   unfinished upload is dropped on the server (`POST …/uploads/:upload/cancel`, best effort, `keepalive`). A 409
   `upload_in_progress` offers managers **ยกเลิกการอัปโหลดที่ค้างอยู่** (the same route, with the upload id from the 409).
   A 503 on commit (the server's commit cap) is waited out per `Retry-After`. Switching building mid-upload aborts and
   cancels it (the panel is keyed by site). Calls that need
   `Retry-After` use a small sender with the shared client's token and refresh rules (`modelSender`). The view then
   shows the model from the bytes it just sent (no download).
4. **Align**: right after an upload the model is fitted to the plans and previewed at 60 % opacity (not saved yet).
   Controls: scale (number, or the file's unit: m, cm, mm, inch, foot), **ปรับให้พอดีกับผัง** (scale and offset from the
   model's bounding box against the widest and deepest floor, centred, standing on the lowest slab), up axis Y/Z (CAD is
   often Z-up), rotation X/Y/Z with a slider and a 90° button, offset X/Y/Z, opacity, visible, overlay (model plus the
   generated glass walls) or replace (the generated building hidden; devices, heat and people stay), and clip above the
   active floor's ceiling. Every change previews live; **บันทึกตำแหน่ง** saves it, **คืนค่า** reverts, closing the panel
   drops an unsaved preview, **ลบโมเดล** asks for an inline confirmation.

Transform axes, as the page uses them: `offset_m` is `[x east (plan x), y up, z south (plan y)]` in metres from the plans'
origin (the top-left corner of the plans, on the lowest slab); `rotation_deg` is a three.js XYZ Euler applied after the
up-axis fix (Z-up turns −90° about X) and the scale. With a floor spacing other than 0 the model matches the lowest floor
only; the panel says so.

### Rendering the model

`TwinView` reads the model through an optional `source.model` (`meta(siteId)`, `file(siteId, revision)`): a source
without it (the TV display's kiosk source today) draws the generated building only. The record is refetched on site
change, after an upload, save or delete and on the realtime `inventory` signal; the GLB only on a new revision (the bytes
of the last two revisions stay in memory). A 503 on the file is waited out per `Retry-After` (5 times at most); a 404 for
the asked revision means the model changed, so the record is re-read and the new revision fetched. A site change or unmount aborts the download; a failed re-read of the record keeps the model already shown. The file request uses the browser cache (the URL carries the revision). The engine parses it once per revision with the glTF loader imported on
demand, flattens it again (a GLB from elsewhere may have thousands of nodes), and disposes it on change or unmount. It is
drawn muted (its own hues at 60 % brightness, matte), so devices, heat and alerts stay the brightest things on the
stage, and between the stage and the live layers in draw order. Opacity, visibility, replace and the clipping plane (on
the model's materials only) change in place; nothing is reloaded. A model over the budget (posted straight to the API, or uploaded before the budget existed) is never drawn; like a model that fails to load, it never blocks the live view:
the generated building stays and a short notice says the model could not be shown. A server without the model API (404)
reads as "no model".

Measured in a local harness (Chrome, Apple M4 Pro, 1440×900, the two-floor test site): immersive 43 draw calls and 6k
triangles at 60 fps; with a 24k-triangle model 44 draw calls (the model is one merged mesh). Idle: 0 frames in both
styles with ambient motion off or motion reduced; about 30 frames a second with ambient motion on.

New `TwinView` props, all optional: `style`, `ambient`, `controls` (`pending`, `onToggle`, `disabled`), `modelKey`,
`modelTransform` (a preview), `onModel`; the handle adds `fitModel`. Files: `theme.ts`, `quick-toggle.ts`,
`model-api.ts` (client, upload, cache), `model-transform.ts` (pure helpers, in `twin.selftest.ts`), `model-loaders.ts`,
`model-import.ts`, `twin-model.tsx`, `engine/immersive.ts`, `engine/model.ts`.

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
