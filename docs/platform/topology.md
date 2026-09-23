# Device topology — หน้าเชื่อมต่ออุปกรณ์

Implemented locally 2026-09-18. Replaces the three-step form at `เชื่อมต่ออุปกรณ์` with a Packet‑Tracer‑style canvas: `frontend/app/topology/` (React Flow 12). Open http://localhost:3001/?connect after signing in as owner/admin.

## What the canvas shows

- **Broker node**: the MQTT endpoint from `GET /mqtt/setup` (host, port, scheme, TLS). Shows “ยังไม่ตั้งค่า” when the server has no `MQTT_PUBLIC_HOST`.
- **Gateway nodes**: every non‑revoked gateway. Edge to the broker is coloured by state derived from `GET /gateways/mqtt-status` and packet freshness: receiving (≤ 60 s, animated) → stale → ready (account applied) → pending (waiting for provisioner) → none (no Aether‑issued MQTT account). Packets are the ground truth; a gateway provisioned outside this page still shows receiving.
- **Device nodes**: one node per external id (BLE MAC), merged from `GET /devices` (adopted), `GET /live` (decoded Minew readings with RSSI) and `GET /studio/sources` (raw BLE observations). Solid green edge = adopted registration; dashed grey edge = heard on air only, labelled with RSSI. A device heard by several gateways has one edge per gateway.
- **Only recognised devices are placed automatically** (adopted or decoded). Raw BLE MACs (phones, beacons, up to 100 per gateway) stay in the palette list; drag or click them onto the canvas, or toggle “BLE ทั้งหมด”.

## Actions (all through existing APIs)

| Gesture | API |
|---|---|
| Drag a gateway card (Minew MG3 / Generic HTTP) onto the canvas → name | `POST /gateways`; MG3 then `POST /gateways/{id}/mqtt` and shows the password once |
| Drag a device from a palette profile card, or a discovered BLE device, then draw a link to any gateway | `POST /devices` with the chosen profile (`minew-s1-pending@1`, `generic-environment@1`). Brand/model are chosen independently of the gateway model |
| Inspector → ✎ on a registration, or drag the gateway end of a green link onto another gateway | `POST /devices/{id}/update` (rename and/or move; confirmation when dragging) |
| Inspector → ยกเลิกการลงทะเบียน / กู้คืน | `POST /devices/{id}/remove`, `POST /devices/{id}/restore` (soft removal: history kept, identity free to adopt again) |
| Inspector → สร้างรหัสผ่านใหม่ | `POST /gateways/{id}/mqtt/rotate` (confirmation dialog) |
| Inspector → เพิกถอน gateway | `POST /gateways/{id}/revoke` (confirmation dialog) |
| Inspector → เปิดใน Dashboard Studio | opens Studio with the `gateway/mac` source preselected |

Links that the API would reject are refused while dragging (device → device, gateway → gateway, a device already registered with that gateway). Adopting the same MAC under a second gateway creates a second registration; the dialog says so.

## Limits in this iteration

- Node positions and manually placed raw devices are stored in `localStorage` per tenant, not on the server. “จัดวางอัตโนมัติ” recomputes the broker → gateways → devices layout.
- Registrations are withdrawn, never hard-deleted (migration `00010`): `removed_at` hides the device from lists, state and ingest while telemetry stays; withdrawn rows are listed in the gateway and device inspectors for restore. The runtime role may update only `name`, `gateway_id`, `removed_at`, `removed_by` and still cannot DELETE.
- Status is pushed: a WebSocket signal triggers a refetch, with a 30 s safety poll (5 s while the socket is down); see [projects and real-time](projects-realtime.md). Gateways can be grouped into projects and the canvas scoped to one project. Freshness keeps decaying between polls, so a stalled backend flips nodes to stale instead of freezing them online. `GET /devices` and `/studio/sources` are capped at 100 rows; the page warns when a list is full because older registrations may then be hidden.
- MQTT credentials and HTTP tokens are kept in memory only while the page is open, exactly like the previous form.
- Gateway models and device profiles load from `GET /api/v1/catalog`; the client-side registry is an offline fallback until the catalog loads.

## Lean header (2026-09-20)

The top of the page is one 42 px toolbar (title, Live state, counters, search, icon buttons with tooltips) and one 34 px project row. The shell bar shrinks to 40 px on this view. Errors, warnings and notices float over the canvas and can be dismissed, so they no longer push the workspace down. The canvas starts 116 px from the top of the window, down from about 345 px with two banners showing. Roaming wearables are described in [wearables-roaming.md](wearables-roaming.md).

## Auto discovery (2026-09-21)

Open **อุปกรณ์ที่พบใหม่** on the connect page, or select a gateway to see its discoveries in the inspector. The list groups unregistered BLE identifiers by gateway, supports gateway and MAC/model filters, and refreshes with the existing live updates. **ลงทะเบียน** prefills the MAC and gateway; successful registration immediately removes that identifier from the discovery list across gateways.

`GET /api/v1/discovery` returns observations from the last **15 minutes** (`?minutes=1..1440`), up to 100 identifiers per active gateway. Active registrations are excluded before the limit is applied. Withdrawn registrations may appear again.

**Only Minew devices are listed (2026-09-23).** Phones, AirTags and other people's iBeacons rotate their MAC every few minutes. With the old 24-hour window each rotation became another "new device", so the count kept climbing. An item is now listed only when it sent a Minew FFE1 frame. That means either an info frame naming its model (kept in the stream name "Minew C10" between info frames) or any decoded FFE1 kind. Bare iBeacon/Eddystone and undecodable advertisements stay out. Real units do not always report the catalog name. The physical S1 says **"PLUS"** and the E8S says **"E8"** (measured on the kit, see `docs/hardware-bringup.md` §2.5). The catalog carries these as `info_aliases`, so they match the S1/E8S profiles. A Minew model without a profile, such as the C6, is still listed as "Minew C6". The response carries `hidden_unknown` and `hidden_by_gateway`, and `?all=1` remains a diagnostic with no UI toggle. The canvas (`isRecognised`) and the gateway inspector follow the same rule, and the "show all BLE" canvas button was removed. The per-gateway stream budget for unregistered identifiers (`DISCOVERY_LIMIT`) counts only strangers heard in the last hour.

Verification: `TestDiscoveryRegistrationAndGatewayIsolation` covers discovery under multiple gateways, reported model/photo matching, hiding registered identifiers, rediscovery after withdrawal, tenant isolation, the time window, non-Minew advertisements hidden (a bare iBeacon stays hidden; a C10 whose model survives only in its stream name, and the real S1 frame reporting "PLUS", are listed with their profiles) and listed with `?all=1`, and `minutes` validation. Frontend build and lint completed (lint has image-element warnings). The updated local API was started on 2026-09-21 and passed readiness and authenticated discovery checks; at that check it returned no eligible discoveries. Physical-device registration through the browser remains unverified.

## Verification

Device lifecycle (2026-09-20): integration test `tests/device_lifecycle_test.go` covers rename/move, cross-tenant refusal, duplicate-identity conflict, removal keeping telemetry, re-adoption, restore conflict and the column-scoped grant. Playwright against the live stack adopted the SIM E8S with the suggested profile, renamed and moved it through the dialog, moved it again by dragging the adopted edge onto another gateway, withdrew it, restored it from the removed list and withdrew it again, with no failed API responses.

`npx tsc --noEmit` and `npm run build` pass. A Playwright run in Chrome against the local stack covered: rendering broker/2 gateways/4 SIM sensors with correct health classes, gateway and device inspectors, adopt dialog prefill, search dimming, drag position persistence across reload, dropping a Generic HTTP card to create a real gateway (token shown once) and revoking it, draft device nodes, activating a palette card by keyboard, client-side 128-byte name validation, auto layout, and no console errors. A separate review pass fixed a dropped post-mutation refresh, errors hidden behind dialogs, per-frame canvas rebuilds while dragging, and drag-only palette cards before the final run. Drawing a link by pointer was validated through the same `onConnect` path the inspector “Adopt” button uses; physical hardware and a real adoption were not exercised in this run.
