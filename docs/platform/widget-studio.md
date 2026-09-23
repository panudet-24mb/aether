## Update 2026-09-20: dashboards per project

A dashboard can belong to a project (`definition.project_id`). The Studio command bar filters dashboards by project, the Add Panel device list is scoped to the dashboard's project, and "ข้ามโปรเจค" opens it to every project. See [projects and real-time](projects-realtime.md).

## Update 2026-09-20: widgets for the whole Minew kit, product photos

Six more Aether Official widgets ship in `internal/studio/official_kit.go`: **Motion / activity** (E8S, C10, B7), **Tamper status** (MBT01), **Emergency button** (B10), **Beacon identity**, **Event timeline** and **Open alerts (workspace)**. They bundle no decoder: `render(input)` consumes Aether's canonical reading (`input.data` with `kind`, `metrics`, `beacon`, `battery`, `rssi`, `received_at`), a compacted `input.history` (values, flags and beacon instance only, halved until the whole input fits the 64 KiB sandbox budget), the device's persisted `input.events` (≤20), the workspace's open `input.alerts` (≤20) and the server clock `input.now`. Widget definitions may declare `kinds` (reading kinds they understand); the Add Panel sheet now asks for the device first and lists only widgets that suit its kind. The template editor pre-fills a tag-shaped sample for these widgets. Tamper and button widgets state that their frames are unverified on hardware.

Product photos for MG3, C10, B7, B10, E8S and MBT01 were downloaded from the manufacturer at the user's request (same terms as the S1 photo below) and are used for identification in the topology page, palette, inspector and template catalog. Widgets themselves cannot embed them: the sandbox allows `data:` images only.

- MG3: https://www.minew.com/wp-content/uploads/2023/08/MG3-BLE-USB-Gateway-2.webp
- C10: https://www.minew.com/wp-content/uploads/2023/08/C10-280.png
- B7: https://www.minew.com/wp-content/uploads/2023/08/B7-min.webp
- B10: https://www.minew.com/wp-content/uploads/2023/08/b10-front-side-cover.webp
- E8S: https://www.minew.com/wp-content/uploads/2023/08/E8S.png
- MBT01: https://www.minew.com/wp-content/uploads/2023/08/MBT01-Anti-tamper-Asset-Tag-600x597.png

Files live in `frontend/public/devices/` (converted to PNG, 400 px). Copyright and trademarks remain with Minew; no broader reuse licence is asserted, and a photo is shown for a device only when its model is known from the registered profile or the tag's own info frame.

## Update 2026-09-14: bundled widget decoder

Widget definitions now include `decode_code` alongside HTML/CSS/render code. New dashboard panels select only widget + device; their bundled decodeUplink runs first. The editor tests the complete decode → render pipeline and imports/exports both functions in one JSON package. A blank decoder intentionally consumes existing canonical readings. Legacy saved panel decoder overrides remain compatible. The separate decoder catalog is hidden from navigation; its code can be copied into a widget in the editor.

Panel headers use pointer capture to move the complete rendered card, including iframe content. Drop on another panel to reorder. The bottom-right corner resizes the live card and snaps to grid spans on release; pointer cancellation discards the gesture. Size controls and arrow keys on the corner provide keyboard alternatives. Saving persists the layout. The Minew ecosystem header branding was removed; product identification remains in the catalog.

# Aether Widget Studio

Implemented 2026-09-14 for local cloud/on-prem compatible backend.

## Product model

Brand → model → visual widget template → device binding → dashboard panel.
A widget is HTML, CSS and a JavaScript `render(input)` function, NOT a device threshold configuration. Existing `/templates` records remain backward-compatible device thresholds; the UI names them accordingly.

Decoders are separate versioned JavaScript packages. Both kinds can be imported/exported as JSON, authored in Studio, kept private to an organization or explicitly published as Community for all organizations on this deployment. Aether Official entries ship in `internal/studio/official.go`; users cannot grant themselves the Official badge. Minew does not endorse the supplied Aether templates.

## Use

Open Dashboard Studio after signing in. Create or select a dashboard, then choose brand, model, visual template, source and optionally a decoder. Add up to 16 panels. Drag headers or use the move-left button; choose column span and height. Save persists the layout in PostgreSQL. Kiosk links require an authorized account in the same organization, and contain no credentials. Save before changing dashboards. Concurrent saves use revision checks and return 409 instead of overwriting another session.

Widget `render(input)` receives `{data,history,source}` and returns either `{key:value}` for escaped `{{key}}` HTML substitution, or `{html:string}` for generated markup such as SVG. CSS is separate. There is no browser DOM API in this JavaScript context.

Decoder `decodeUplink({bytes,fPort})` returns `{data:{...}}`. For MG3, bytes contain the complete BLE advertisement, and fPort is 0 (not LoRaWAN). Custom decoding runs against persisted raw BLE observations when rendering a bound panel. Up to 200 newest observations in the selected 1h/24h/7d range are decoded by that widget version; the input byte budget may reduce the count for long frames. `input.history` is ordered oldest-first and contains decoded fields plus server-assigned `received_at` and `source`. Unsupported frames are skipped and counted; time/memory limits apply to the whole batch. Data collection continues with the browser closed. This does not replace the canonical ingestion decoder. History starts when migration 00004 and the new collector/API are enabled; old diagnostic packets are not backfilled. Generic BLE sources can be selected when diagnostic raw packets are retained, even if the built-in decoder does not recognize them.

## Runtime and security

A fresh QuickJS context per invocation runs in a Python subprocess with no exposed host functions. JS memory 16 MiB, stack 256 KiB, CPU interruption at 200 ms; parent deadline 1 s; Python address space 192 MiB. Two concurrent invocations per API process. No process/environment, filesystem, networking, Node APIs, timers or DOM. This is a bounded local development runtime, not a claim of production certification for arbitrary hostile native-engine workloads.

HTML is reduced to an allowlist of display tags/attributes (no navigation, scripts, embeds or event handlers), then displayed in an iframe with an empty sandbox and CSP disabling scripts, network, frames, forms and base URLs. CSS inline and data images are allowed. Dashboard widgets are displays, not device-command controls.

Catalog records use forced RLS. Private items/dashboard layouts are tenant-scoped. Community widget and decoder source is readable across tenants, immutable after publication; updates create new versions. Only owner/admin roles can access Studio currently. Official publication is a deployment-maintainer source workflow; a platform publisher approval UI, marketplace moderation, ratings, abuse controls and public customer onboarding are not yet implemented.

## API

All require existing bearer authentication and owner/admin permissions:

- GET `/api/v1/studio/items?kind=widget|decoder|dashboard`
- GET `/api/v1/studio/sources`
- POST `/api/v1/studio/items` — create a versioned item (server owns id/tenant/official/revision)
- POST `/api/v1/studio/items/:id/save` — dashboard only; include current revision
- POST `/api/v1/studio/items/:id/delete` — owned dashboard only
- POST `/api/v1/studio/test` — `{code,function,input}`; functions `render` or `decodeUplink`
- POST `/api/v1/studio/render` — `{panels:[...],range:"24h"}`; server resolves authenticated sources

## Minew assets

Downloaded from the manufacturer at the user's request, used for brand/product identification. Copyright/trademark remain with Minew; no broader reuse license is asserted.

- Logo: https://www.minew.com/wp-content/uploads/2024/06/mine-logo-white-300x129.png
- S1 photo: https://www.minew.com/wp-content/uploads/2023/08/S1-Temperature-Humidity-Sensor-1-600x597.png
- Product page: https://www.minew.com/product/s1-ble-temperature-and-humidity-sensor/

The S1 photo is a model reference; a compatible FFE1 advertisement alone does not establish physical model identity.

### Generic profile photos (2026-09-23)

The three generic catalog entries use real-product reference photos from Wikimedia Commons. They show *a* device of that class. They do not identify the model the user actually connects.

| File | Source | Licence / author |
|---|---|---|
| `generic-http-gateway.png` | https://commons.wikimedia.org/wiki/File:Bluetooth_IoT_Gateway.png | CC0 · IoT Devices Manufacturer |
| `generic-ble-beacon.png` | https://commons.wikimedia.org/wiki/File:Mini_BLE_beacon.png | CC BY-SA 4.0 · MinewTech |
| `generic-environment.png` | https://commons.wikimedia.org/wiki/File:2023_Czujnik_temperatury_i_wilgotno%C5%9Bci_Xiaomi_(1).jpg | CC BY-SA 4.0 · Jacek Halicki (cropped, 400 px) |

## Verification

Backend race suite including private/community RLS, immutable widgets, foreign dashboard write/delete rejection and stale revision conflict. Live API smoke verifies FFE1 decoding and renders three panels from the MQTT simulator. Infinite loops and Node host API access fail with HTTP 422. Hardware is unavailable; simulator sources are labelled explicitly.

## BLE archive bounds (development)

At most 100 valid distinct BLE observations per uplink are archived atomically with packet capture, with 10,000 newest observations retained per gateway. Exact duplicate payload/advertisement combinations are deduplicated. This is a count limit, not a guarantee of seven days of retention. Invalid hex/MAC rows are ignored. Each batch is tenant-scoped under forced RLS and gateway revocation checks. Raw input is persisted; decoded results are evaluated on demand with the selected template version, rather than stored as a permanent computed series. Existing canonical environmental history is unchanged.
