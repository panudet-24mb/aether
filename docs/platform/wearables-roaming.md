# Wearables across several gateways (roaming)

Added 2026-09-20. A wristband, card or panic button moves with a person, so more than one gateway hears it. Aether stores one stream per gateway and tag, which already keeps every gateway's readings. Roaming adds the missing part: treating those streams as one device.

## How to use it

1. Adopt the tag once, on any gateway. That gateway is only the registration's home.
2. Tick "ใช้ได้หลาย gateway (roaming)". The adopt dialog proposes it for wearable profiles (C10, B7, B10). It can be switched later in the device inspector.
3. Nothing is configured on the gateways. Every gateway of the workspace that hears the MAC contributes, including gateways in other projects.

Do not register the same tag again on each gateway. One roaming registration replaces that.

## What changes for a roaming device

| Area | Behaviour |
| --- | --- |
| Location | A stable zone decided at ingest (see "Stable zones"). `GET /api/v1/presence/{mac}` returns it with `since` and all sightings. When the zone's gateway is silent the API falls back to the strongest fresh signal. |
| Canvas | Dashed links to every gateway that hears it, a bold animated link and "อยู่ที่นี่" on the current one, the zone name under the node, a ROAM tag. |
| Dashboard Studio | A panel renders from the current gateway's stream, so it keeps updating while the person walks. Every widget receives `input.presence`. |
| Events | A button press or tamper heard by several gateways within 15 s is recorded once. Alerts were already deduplicated per rule and tag. |
| Offline | Raised once, only when no active gateway hears the tag, on the gateway that heard it last. Revoked gateways do not count. Coming back through any gateway ends the episode with one `online` event. |

Turning roaming off restores the per-gateway behaviour.

## Stable zones (2026-09-20)

Raw RSSI of a worn tag swings by 10 dB or more, so "strongest signal wins" flips between neighbouring gateways. The zone is now decided when an uplink is stored:

1. Each gateway keeps an exponential moving average of the tag's RSSI (`stream_state.rssi_avg`, weight 0.35 for the newest sample, restarted after 60 s of silence).
2. The zone (`core.presence_state`, migrations `00014` and `00018`) moves to another gateway only when that gateway's average beats the current one by 6 dB for 10 s. With 5 s uplinks that is three consecutive observations. Uplinks from the current gateway do not reset that timer. A candidate that goes unheard for 15 s starts over, so one late sample cannot win.
3. If the current zone's gateway has not heard the tag for 60 s, or was revoked, the next gateway that hears it takes over at once.
4. Every move writes a `zone` event with `from`, `to` and the smoothed RSSI. Alert rules of type "wearable เข้าโซน" can fire on any move or only when entering chosen gateways (`scope.gateway_ids`).

The canvas, the floor plan, the widgets and the presence API all read this one zone. Turning roaming off deletes the remembered zone. The simulator adds a ±7 dB wobble with opposite phase at the two gateways, and `internal/alerts/zone_test.go` checks that a tag standing at the midpoint for ten minutes is assigned once and never flips, that a real move hands over after the dwell time while both gateways keep reporting, and that a stale candidate starts over. On the running simulator each wearable changed zone about twice per five-minute loop, which is the real walk.

## API

- `POST /api/v1/devices/{id}/roaming` with `{"roaming": true}`. Owner or admin. Audited as `device.roaming_changed`.
- `GET /api/v1/presence/{external}` for any 12-hex identity the workspace has heard. Tenant-scoped like everything else.
- `GET /api/v1/devices` includes `roaming`. `GET /api/v1/catalog` marks wearable profiles with `wearable: true`.

## Widgets

- **Wearable location (multi-gateway)** `official-presence-v1`: current zone, project, and a signal bar per gateway.
- **Wearable card** `official-wearable-v1`: where the person is, activity from the accelerometer, last button press, battery, top three gateways. Turns red for two minutes after a button press.

## Simulation

`python3 infra/mqtt/provision-simulator.py` now also creates `SIM · Virtual MG3 · Zone B (wearables)` with its own MQTT credentials. Restart the broker and the ingest container, then the simulator. The C10, B7 and B10 walk between zone A and zone B on a five-minute loop, each a third of a loop apart. RSSI falls from -52 to -98 dBm and a gateway stops hearing a tag below -90 dBm, so each loop has a stretch where both gateways hear the tag and stretches where only one does.

## Verification (2026-09-20)

- Integration test `tests/roaming_test.go`: cross-tenant toggle refused, a press heard by two gateways is one event, presence picks the stronger gateway and falls back when one goes silent, no offline while another gateway hears the tag, exactly one offline episode when none does, one `online` event when it returns through the other gateway, offline still raised when the fresher gateway is revoked, per-gateway behaviour returns when roaming is turned off. `internal/simulation` tests the walk. The full backend suite passes.
- Playwright in Chrome against the local stack with the two SIM gateways: three wearables registered as roaming, presence moved between the two gateways within one loop, both gateways heard the tag at once for part of it, the canvas shows the zone on each roaming node, the adopt dialog proposes roaming for a wearable profile, the inspector switch reflects it, and the eight panels of the "SIM · Minew kit demo" dashboard render without errors or failed API calls.
- Not tested: real hardware. RSSI behaviour of a worn B7 or B10 between two MG3 gateways is unknown until the kit arrives.

## Limits

- RSSI gives "nearest gateway", not coordinates. The 6 dB margin and 10 s dwell are fixed constants chosen for 5 s uplinks; they are not configurable per site yet and have not been tuned on hardware.
- Presence needs frames Aether decodes (Minew FFE1, iBeacon, Eddystone). A tag that only shows up as a raw advertisement has no stream and no presence.
- Roaming event writes are serialised per workspace with their own advisory lock, so two gateways committing in the same second still record one press. Presses more than 15 s apart count as separate events.
- Each gateway keeps at most 100 streams. In a busy site, adopt wearables early so they hold a stream on every gateway.
- The B7 button frame is undocumented and not decoded. The B10 press encoding follows a public guide and is unverified on hardware.
