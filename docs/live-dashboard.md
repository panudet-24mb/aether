# ภาพรวม (Operations overview)

Rewritten 2026-09-20. The old Live monitoring page showed one temperature sensor at a time and a gateway table. It is replaced by an operations board that answers "is anything wrong right now, and where is everyone". Source: `frontend/app/live/overview.tsx`.

## What is on the page

- **One lean toolbar**: Live or Polling state, project filter, "ทั้งหมด / ต้องดู / Wearable", device kind, grouping (by gateway or zone, by kind, none), search. When the server runs with `ALERTS_SHADOW=true` a chip says so.
- **Six counters that are also shortcuts**: open alerts with the critical count, gateways that sent data in the last 60 s, registered devices still reporting, wearables in range, batteries under 20 %, MA/PM overdue. Each one opens the page that fixes it or applies the matching filter.
- **The board**: one card per sensor, every kind, not only temperature. A card shows the product photo, name, model, a status dot, the main value for its kind (temperature and humidity, moving or still with |a|, attached or removed, dry or leaking, in range), a sparkline from the live window, the reason it needs attention or how old the data is, battery and RSSI. Cards sort by urgency: alerting, offline, stale, normal. Registered devices that nobody hears still appear, as offline. Roaming wearables are grouped under the gateway of their current zone and say where they are.
- **The side column**: open alerts, "ใครอยู่ที่ไหน" for wearables with a link to the floor plan, gateways with last packet age, and the latest events.
- **The drawer** (click a card): status and every reason, facts, temperature and humidity charts for 1 h, 24 h or 7 d with the threshold line, the threshold template settings, the device's own events, and links to the connect page, the asset record and the floor plan.

## Status rules

`online` means data within 60 s. `stale` is 60 s to 15 min. `offline` is older than 15 min or no data at all. `alert` wins over the others when the device has an open alert, a tamper or leak flag, or a value above its template threshold. Low battery and overdue maintenance are listed as reasons but do not turn a card red.

## Data

The page reuses the topology snapshot (gateways, devices, live readings, events, projects) and adds `GET /alerts?status=open` and `GET /assets`. WebSocket signals trigger a refetch. The safety poll is 15 s while the socket is up and 5 s when it is down. A local clock advances between refreshes so ages and freshness keep moving. The shell no longer polls `/live` every 5 s: it fetches the sensor list only for Dashboard Studio's device picker.

## Verification

Playwright in Chrome against the local stack, 6 of 6 checks: lean chrome (content starts 82 px from the top), ten cards with six counters and a live socket, the wearable filter, the drawer with two charts and the range switch, a counter navigating to the alerts page, no failed API calls or page errors.

---

The text below describes the first version of the page and is kept for history.

> Update 2026-09-12: the graph now uses persisted samples with a selectable window and template assignment. See [current behavior](platform/persistent-history.md). The original 20-packet description below is historical.

# Live dashboard

Implemented 2026-09-11. Open http://localhost:3001/ and sign in using the provisioned owner email and `.secrets/owner-password.txt` (not the MQTT password). The old spatial prototype remains at `/demo`, explicitly labeled simulated data.

## Data path

MG3 → authenticated MQTT broker → tenant-scoped raw packet storage → authenticated `GET /api/v1/live` → browser polling every five seconds. The API only permits owner/admin access and uses each user's tenant to list gateways and packets. No owner or gateway credentials are bundled into frontend source. Access tokens stay in memory; existing rotating HttpOnly refresh cookies restore sessions. CORS permits `http://localhost:3001` for this local installation.

The live view decodes BLE service UUID FFE1, frame A1 version 01. It validates the complete AD element length, battery 0–100, humidity 0–100, signed big-endian 8.8 readings, and bounds before showing measurements. Unsupported frames are excluded from environmental readings. The source protocol can be used by multiple Minew models; labels say Minew temperature & humidity, not a physically verified S1 asset identity.

References for independently implemented byte parsing: [reelyActive Minew decoder](https://github.com/reelyactive/advlib-ble-services/blob/master/lib/minew.js) and [signed 8.8 helper](https://github.com/reelyactive/advlib-ble-services/blob/master/lib/utils.js). Tests include a redacted captured frame, negative temperature, malformed/truncated/unsupported frames, duplicate advertisements, order and synthetic packet exclusion.

## Meaning of displayed values

- Latest temperature, humidity, battery and RSSI are decoded from actual advertisements.
- Freshness uses PostgreSQL packet receive time, never the unverified gateway clock. Over 60 seconds is stale; API failures keep last readings visibly marked stale.
- Graphs cover the latest 20 raw packets per gateway, one sample per sensor per uplink. This is a bounded diagnostic window, not permanent telemetry history. A rarely broadcasting sensor can leave this window and disappear until detected again.
- BLE nearby count includes all valid MAC observations in the same window; devices seen by multiple gateways are counted separately per gateway. It is not the number of enrolled assets or people.
- No room/floor position is inferred from one gateway's RSSI.
- The MQTT ingestion response remains `decoded:false`: it stores raw data. Decoding is a read projection for this dashboard, not a new canonical telemetry storage pipeline.

## Validation

Frontend TypeScript and production build passed; HTTP rendering returned 200. Backend decoder/race tests and live endpoint tenant isolation tests cover the data boundary. Actual authenticated API reads showed two environmental sensors and increasing receive timestamps, e.g. 25.39 °C / 61.74%RH and 28.60 °C / 47.53%RH at the time of verification. These are observations, not fixed demo values or calibrated measurements. Browser interaction/visual QA was not performed.

## Deployment

This change is running locally, not published to the existing hosted demo. For cloud or production on-premise, deploy the same backend and frontend source, configure `VITE_AETHER_API_ORIGIN` at frontend build time (empty for same-origin reverse proxy), exact backend APP_ORIGIN, HTTPS and database TLS. Do not publish a frontend compiled with localhost as its API URL.
