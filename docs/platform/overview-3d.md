# Overview · 3D mode ("command constellation")

The Overview page (ภาพรวม, `frontend/app/live/overview.tsx`) has two views of the same data, switched in the
header bar with **การ์ด | 3D**. The choice is remembered per browser in `localStorage["aether.overview.mode"]`
(`"cards"` or `"3d"`; if storage is blocked, the switch still works for the visit). The default is the card board,
which is unchanged.

The KPI row, the SOS banner, every filter (project, ทั้งหมด / ต้องดู / Wearable, kind, search), the side panel and
the device drawer (Sheet, with its controls) are the same in both views. In 3D, only the card board area is
replaced by the stage. The "จัดกลุ่ม" select is disabled there because 3D always groups by gateway.

Code: `frontend/app/live/overview3d/`
- `stage.tsx`: the React stage. It is lazy-loaded with `React.lazy`, so card mode never downloads it. It handles the toolbar, labels, tooltip, legend and aria-live.
- `engine.ts`: the three.js renderer. It is imperative, renders on demand, and positions the label elements React renders.
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
- **Declutter:** a label that would overlap a higher-priority or nearer label is hidden for that frame. SOS labels are never hidden, and neither is a label that holds keyboard focus.
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
- **Click:** clicking a node opens the Sheet. Clicking a hub flies the camera to it.
- **Presets (toolbar, or keys 1 / 2 / 3 while focus is inside the stage, never page-wide):**
  - ภาพรวม frames everything.
  - มุมบน is a top-down view.
  - ไปที่เหตุ flies to the first SOS, or the first alert. The button is red when an SOS exists and disabled when there is nothing.
- **ทัวร์:** a slow auto-orbit. It stops as soon as the operator drags. It is off with reduced motion.
- **การ์ด (toolbar):** goes back to the board in one click, as does the header switch.

## Accessibility

- The canvas wrapper has `role="application"` and a Thai `aria-label` that says what is shown and how to use it.
- Labels, hub chips, toolbar buttons and toggles are real buttons in tab order. Toggles carry Thai `aria-label`s, `role="switch"`, `aria-checked` and `aria-busy`.
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
