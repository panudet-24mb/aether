# Overview · 3D mode ("command constellation")

The Overview page (ภาพรวม, `frontend/app/live/overview.tsx`) has two views of the same data, switched in the
header bar with **การ์ด | 3D**. The choice is remembered per browser in `localStorage["aether.overview.mode"]`
(`"cards"` or `"3d"`; if storage is blocked, the switch still works for the visit). The default is the card board,
which is unchanged.

In 3D the **whole Overview area** (everything right of the app sidebar) becomes one three.js scene. There is no
flat header, KPI row, SOS strip, side panel or drawer. Their content lives inside the scene as glass panels:

| Card page | 3D scene |
|---|---|
| KPI row (6 tiles) | six KPI tiles in one row under the HUD (3×2 on a phone), placed behind the platform; same values, colour states and click actions |
| SOS strip | a large pulsing banner above the SOS device, with รับทราบ / ดูรายละเอียด and ดูบนผังอาคาร; ไปที่เหตุ frames it |
| ต้องดูตอนนี้ | a panel in the left column; an alert flies the camera to its device and opens the detail; ดูทั้งหมด opens the alerts page |
| ใครอยู่ที่ไหน | a panel in the left column (8 rows, then "และอีก n คน"); a person opens the detail; ดูบนผัง opens the floor plan |
| Gateway | a panel in the right column (8 rows); a gateway flies the camera to its hub; ดูทั้งหมด opens the connect page |
| เหตุการณ์ล่าสุด | a panel in the right column (10 rows, new rows slide in); an event opens its device's detail |
| device drawer (Sheet) | a detail card beside the device. The camera flies so that the device, ringed in the accent colour, sits on the left third (on top on a phone) and the card beside it (below it on a phone). The card has the same status, reasons, facts, history, full DeviceControlsPanel and action links. ✕ or Esc closes it, and focus returns where it was |
| header filters, view switch | the HUD (top left): presets, tour, fullscreen (when the browser allows it), ตัวกรอง (project / show / kind / search), รายการ (the four lists, on a phone) and การ์ด |

The page renders this content once, in shared components (`frontend/app/live/overview-parts.tsx`: `OverviewFilters`,
`kpiTiles`/`KpiRow`, `SosBanner`, the four list sections and `DeviceDetail`). The card page lays them out flat. The
stage places the same React elements into in-scene panels with portals. There is no second copy of the business
logic: `overview.tsx` computes the model, the filters and every action once. The "จัดกลุ่ม" select only exists in
card mode, because 3D always groups by gateway.

Code: `frontend/app/live/overview3d/`
- `stage.tsx`: the React stage. It is lazy-loaded with `React.lazy`, so card mode never downloads it. It handles the HUD, the device labels, the tooltip, the legend, aria-live and the in-scene panels (`Panel3D`: a portal into an element the engine places).
- `engine.ts`: the three.js renderer. It is imperative and renders on demand. It positions the label elements React renders and the panel layer (`PanelRegistry`): it places, scales and fades the panels.
- `layout.ts`: pure layout (hubs, rings, label priority, SOS diff). `types.ts` holds shapes, tiers and the controllable rule.
- `power.tsx`: the on/off toggle on a label. It uses the same command flow as `DeviceControlsPanel`.
- `synthetic.ts`: a seeded synthetic site, used by the self-test and the dev-only scale toggle.
- `overview3d.selftest.ts`: the layout self-test. Run instructions are in its header.

## What it shows

The stage draws exactly the cards the board would show (`visible`) plus the in-scope gateways (`model.gateways`
filtered by project):

- **Hubs** are gateways: a tower on a pedestal with a beacon, a ring and a soft pool of light on the platform.
  - Hubs are placed on one circle (up to 14), or on concentric rings when there are more. Each project's hubs stay together, with a gap between projects.
  - Tower, beacon and pool take the **project colour** (`PROJECT_COLORS`). A hub without a project uses the accent.
  - Online hubs (a packet in the last 60 s) breathe softly. Offline hubs are dimmed toward grey.
  - A hub with no visible devices is drawn only when nothing narrows the view (kind "all", "ทั้งหมด", empty search).
  - Devices whose gateway is unknown share one "ไม่ทราบ gateway" hub.
- **Nodes** are devices, placed in rings around their gateway (`card.gatewayId`). A ring's devices are spread evenly, and outer rings sit slightly higher. Each ring has a faint orbit track. A link line runs from the hub to each node, and a stem runs down to the platform.
  - The order inside a hub is by name, so **a status change never moves a device**.
- The platform is a dark, reflective disc lit by a PMREM `RoomEnvironment`, with a radial-faded accent grid and concentric rings. There is also distance fog and a CSS vignette. Bloom runs only on the high quality tier.

### Colour (the status the board uses)

| State | Node colour | Extra |
|---|---|---|
| online | accent (`--color-accent`) | none |
| stale | amber (`--color-warning`) | none |
| alert | coral (`--color-danger`) | pulsing ring, larger node |
| offline | grey (`--color-dim`), dimmed | small dim light pool |
| SOS (open SOS alert) | coral | tall red light pillar, expanding ring on the platform, largest node, label never hidden |
| switched on (lamp / plug / gang reports on) | status colour, brighter | larger node and light pool |
| occupancy "มีคน" | status colour | accent halo ring around the node |
| door open | status colour | the door slab swings 70° open |
| door "เปิดค้าง" (left open) | amber | amber halo ring, slab open |

All colours come from the theme tokens, read at runtime with `getComputedStyle(document.documentElement)`, and are re-read (`engine.setPalette`) when `prefers-color-scheme` changes. The
tokens are oklch, so they are converted through a 1×1 canvas. Project colours come from `PROJECT_COLORS`.

### Shape (device kind)

| Shape | Kinds |
|---|---|
| sphere | environment, info, zigbee, light, climate |
| cube | switch, metering, lock |
| diamond (octahedron) | sos, remote, beacon, hazard, tamper, leak |
| capsule | occupancy, motion |
| slab (hinged door) | door, cover |
| gem (icosahedron) | lighting, fan |

The legend ("สัญลักษณ์", bottom left) repeats both tables in Thai.

## Labels

Labels are DOM elements, not sprites. React renders at most **40** device labels plus one chip per hub, and the
engine projects them every frame.

- **Which devices get a label:** SOS, then alert, then controllable, then the rest. Within a tier, the device nearest the camera wins. The set is re-picked at most every 350 ms while the camera moves.
- **Declutter:** a label that would overlap a higher-priority or nearer label is hidden for that frame. A label under a panel shown on screen (KPI, list, SOS banner, detail) is always hidden, at every tier and every size: panels stack above labels. SOS labels are never hidden, and neither is a label that holds keyboard focus.
- **Content:** the status dot, the name (with an SOS tag), and the device's value. The value is rendered by the board's own `value()`, so it has the same semantics: °C / % / W / เปิด-ปิด / มีคน-ว่าง / ประตู.
- **Click:** a label is a real `<button>`. It opens the existing device Sheet (`setOpenKey`). A hub chip hangs just below its gateway's base, out of the devices' way, and flies the camera to that gateway.

### On/off toggle on a label

A controllable device gets a small power switch on its label. "Controllable" uses the same rule as the board:
`deviceId && registered && kind ∈ {switch, lighting, cover, climate, fan, tuya} && status !== "offline"`.

Tuya Wi‑Fi devices all have kind `tuya`, whatever they are (plug, lamp, AC, air-quality monitor). A `tuya` device
therefore gets controls only once its `/devices/{id}/controls` reports a switchable output. This applies to the board's
compact controls, the 3D chip and the label priority (a monitor without a switch does not take a controllable slot).
`overview3d/outputs.ts` asks lazily, at most 4 requests at a time, and caches only real answers. An error (429, 5xx)
or an answer without features is retried with backoff (4 s doubling, at most 5 min), never cached as "no switch". The device Sheet shows the full DeviceControlsPanel for `tuya` devices too.

- **What a press sends:** every output, using the compact controls' rule for an output, goes on or off together. For a cover, the switch sends OPEN or CLOSE instead.
- **Command flow:** the same as `DeviceControlsPanel`. Each output gets `POST` with an idempotency key, then the command is polled for up to 20 s. The shown state only changes when the device reports.
- **States:**
  - กำลังสั่ง… while the command is pending.
  - ยืนยันแล้ว when the device confirms.
  - A warning when the command fails, times out or expires. The state stays where it was.
  - The switch is `role="switch"` with `aria-checked`, named by the device. While the state is unknown it is a plain button named by its action ("เปิด <name>").
  - A 403 shows "บัญชีนี้ไม่มีสิทธิ์สั่งงานอุปกรณ์". Read-only accounts see the state without a button.
- The command state lives in a store owned by the stage. A label that scrolls out of the label set and comes back still shows its pending command.

## Interaction

- **Camera:** OrbitControls with damping. Drag rotates, the wheel zooms, right-drag pans, and arrow keys pan while the canvas has focus.
- **Hover:** a highlight ring on the node and a tooltip with name, kind, gateway, value, status and first reason.
- **Click:** clicking a node or its label flies the camera to it and opens the detail panel beside it. Clicking a hub flies the camera to it.
- **Intro:** entering 3D starts with a short fly-in from above (a cut with reduced motion).
- **Fullscreen:** the HUD's เต็มจอ button (shown only when `document.fullscreenEnabled`) puts the stage in fullscreen; Esc leaves it. A dialog that portals to `<body>` (for example a device's threshold settings) would be hidden by fullscreen, so opening one leaves fullscreen first.
- **Esc:** closes the detail, unless a nested dialog already handled it (its Esc closes only the dialog), or the focus is in a field (HUD search or selects, a control input).
- **Staying home:** after a resize, a data change, a visible-area change or fullscreen, a camera that is at the overview pose (or flying to it) moves to the new pose. That way the panels land on their slots again. Any camera move of the operator's ends that.
- **Presets (toolbar, or keys 1 / 2 / 3 while focus is inside the stage, never page-wide):**
  - ภาพรวม frames everything.
  - มุมบน is a top-down view.
  - ไปที่เหตุ flies to the first SOS, or the first alert. The button is red when an SOS exists and disabled when there is nothing.
- **ทัวร์:** a slow auto-orbit. It stops as soon as the operator drags. It is off with reduced motion.
- **การ์ด (HUD):** goes back to the card page in one click.

### In-scene panels

- **How they are drawn:** a DOM layer over the WebGL canvas. Every panel is screen-aligned (it faces the viewer squarely, so it stays readable from any orbit angle). That makes its perspective image an exact 2D similarity, so the engine places each one with a translate and a uniform scale computed from its world position and the camera, and nearer panels stack on top. There is no CSS 3D (an earlier `CSS3DRenderer` version lost hit testing when the camera moved far from the origin), so clicks, Tab and screen readers work exactly as on a flat page.
- **Fading:** panels fade with distance. A panel steps aside (opacity 0.12, no pointer) when it would cover the middle of the view in front of the camera's target, or when it is very close to the camera.
- **An open detail wins:** the detail card is opaque glass (background alpha 0.96). Every other panel whose screen rect overlaps it drops to opacity 0.06 and stops taking the pointer, whether it is in front or behind. That includes the SOS banner. The detail body scrolls inside the card, and the header with ✕ stays fixed. The engine caps the card at about 70% of the stage height at its framed distance.
- **Occlusion:** not attempted; panels always draw over the WebGL scene.
- **Placement (no overlaps at the default view):** `layout.ts` `wallSlots` lays out the default view in screen pixels.
  - The six KPI tiles sit in one row under the HUD, all the same height.
  - The four lists sit in two columns, two stacked per side.
  - Nothing overlaps, and everything stays inside the visible part of the stage. The self-test checks this from 900 to 2400 px wide.
  - The engine turns each slot into a world position and width: from the overview preset's camera pose, at a depth behind the target (KPIs) or just before it (lists). So at the default view every panel lands exactly on its slot, and when orbiting the panels move with the scene. The preset distance makes the platform fit the gap between the list columns.
  - Slots are recomputed on resize, when the constellation changes, and when the visible area changes.
  - On a stage narrower than 720 px the wall is compact: KPIs 3×2, and the lists move to a HUD drawer. A stage smaller than 240 px gets no wall, and its panels are hidden.
  - The SOS banner hangs above its device. The detail card is framed with its device; it keeps the camera's orientation.
  - The KPI row starts under the HUD's measured height.
- **Visible area:** the stage measures what the viewer cannot see: the app shell's fixed bars over its top (for example the global SOS bar on a scrolled page), the part scrolled off, and the part below the window. The HUD, the KPI row, the lists and the legend stay inside the rest.
- **Budget:** at most 12 panels (6 KPIs, 4 lists, the SOS banner, the detail). Lists are capped, with ดูทั้งหมด.

## Accessibility

- The canvas wrapper has `role="application"` and a Thai `aria-label` that says what is shown and how to use it.
- Labels, hub chips, HUD buttons, panel buttons and toggles are real buttons in tab order. Opening a detail moves focus to its close button; closing it returns focus to where it was (or the canvas).
- A faded panel (stepped aside, or under the open detail) is `inert` and `aria-hidden`: Tab and screen readers skip it until it is restored. A panel behind the camera is hidden.
- Toggles carry Thai `aria-label`s, `role="switch"`, `aria-checked` and `aria-busy`.
- An `aria-live="assertive"` region announces each **new** SOS once ("SOS ใหม่ · <name> ที่ <gateway> · ปุ่ม ไปที่เหตุ พาไปจุดนั้น"). The SOS banner above stays the visual cue.
- `prefers-reduced-motion` affects the stage in four ways:
  - Ambient pulses stop.
  - Camera moves become cuts.
  - The tour is disabled.
  - The CSS pulse on SOS labels stops.
- **WebGL unavailable** (renderer creation throws, or the context is lost): the page switches back to cards and shows "เบราว์เซอร์หรืออุปกรณ์นี้แสดงภาพ 3 มิติไม่ได้ (ไม่รองรับ WebGL) · แสดงแบบการ์ดแทน". A failed lazy chunk (or a render error in the stage) falls back the same way, through an error boundary. The next press of 3D retries with a fresh `lazy()`.

## Performance rules

- **Lazy chunk:** card mode does not load the stage (`stage-*.js` is its own chunk).
- **Render on demand:** a frame is drawn only for a data change, a camera move or damping, a tween, the tour, hover, or an ambient pulse.
  - The panel layer is re-placed only when the camera moved or a panel was added, moved, removed or resized, never on ambient pulses.
  - With reduced motion, or while the tab is hidden, an idle stage draws **0 frames**.
  - Otherwise ambient pulses are throttled: 30 fps while an alert or SOS pulses, 20 fps for hub breathing alone.
- **Rebuilds:** the scene is rebuilt only when what it draws changes (a signature of hubs, positions, status, SOS, occupancy, door and power). The overview's 5 s clock tick does not rebuild it.
- **Instancing:**
  - One `InstancedMesh` per node shape (at most 6).
  - Shared instanced meshes for light pools, picking, halos and alert rings.
  - One `LineSegments` for links and stems, and one for orbit tracks.
  - Hubs are instanced too. The 500-device synthetic site measured 43 draw calls and about 131k triangles per frame on the high tier, bloom passes included. The count does not grow with the number of devices.
- **Picking:** raycasts go against two invisible instanced proxies (nodes, hubs), not the visible geometry.
- **Quality tiers:** the stage starts on the high tier: DPR ≤ 2, bloom, fog. It drops to med (DPR ≤ 1.5, no bloom), then low (DPR 1, no fog, fewer segments) when a 5 s average stays under 24 fps while the camera moves.
- **Labels:** at most 40 device labels. All sizes are read before any are written, so there is one layout per frame.
- **Disposal:** unmounting disposes every geometry, material, texture, the environment map, the composer and the renderer. The WebGL context is released with `forceContextLoss`, and the ResizeObserver and listeners are removed.

### Scale check

- `overview3d.selftest.ts` lays out synthetic sites of 0, 1, 7, 60 and 500 devices. It checks that:
  - every device is placed once, inside its own hub, and never on top of another device;
  - hub footprints never overlap;
  - the layout is deterministic, and a status change moves nothing;
  - label tiers and the 40-label cap hold;
  - the new-SOS diff, the controllable rule and power-from-reading work.
- In a **dev build only**, `?ov3d-demo=500` (up to 5000) swaps the stage's data for the synthetic site and shows a draw-call / fps readout. Synthetic devices have no `deviceId`, so they can never be commanded.
