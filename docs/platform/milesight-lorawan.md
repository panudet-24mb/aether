# Milesight LoRaWAN through a UG65 (phase 1: ingest and display)

Status 2026-10-26: implemented and tested with synthetic uplinks. **Not yet verified with real hardware.**
- The synthetic uplinks use the frame examples from Milesight's own READMEs.
- The gateway model and the four Milesight profiles stay `Verified: false` until an uplink captured from the real UG65 replaces the synthetic fixtures.

## Architecture

The UG65 runs its **embedded LoRaWAN network server**. It connects **out** to Aether's broker with an Aether gateway account, the same way a Minew MG3 does, and publishes every uplink. No ChirpStack is involved.

```
WS303 / WS202 / GS301 / VS330 ──LoRa (AS923-1)──▶ UG65 (embedded network server) ──MQTT/TLS 8883──▶ Aether broker ──▶ collector
```

> This differs from `chirpstack-selfhost-brief.md`, which disables the embedded network server and forwards packets to ChirpStack. The two setups exclude each other on one gateway: never run the embedded network server while the packet forwarder also feeds another network server.

Downlinks (commands) are not implemented yet. The gateway account can already subscribe to its own downlink topic.

## Setup

1. In Aether, open "เชื่อมต่ออุปกรณ์". Add a gateway of model **Milesight UG65 (LoRaWAN)** and issue its MQTT account. The password is shown once.
2. On the UG65 web UI:
   - **Network Server → General:** enable the network server.
   - **Packet Forwarder:** keep the local destination only.
   - **Network Server → Profiles / Device:** add each device with its DevEUI and AppKey (from the sticker or Milesight ToolBox), region AS923-1, class A.
     - Optional: assign Milesight's **payload codec** to each device (firmware 60.0.0.41 or later).
     - Without a codec, Aether decodes the raw frame itself for GS301, WS303, WS202 and VS330 (see below).
   - **Network Server → Applications → (application) → Data Transmission → MQTT:**

| Field | Value |
|---|---|
| Broker Address / Port | `MQTT_PUBLIC_HOST` / 8883 |
| Client ID, Username | `gw-<gateway id>` |
| Password | the one issued in step 1 |
| TLS | on. Choose "Self signed certificates" and upload the broker CA (`ca.crt`) as the CA file. No client certificate is needed. |
| Uplink Data topic | `aether/lora/<gateway id>/$deveui/up` |
| Downlink Data topic (optional) | `aether/lora/<gateway id>/$deveui/down` |
| QoS | 1 |

3. When the devices send, they appear under "อุปกรณ์ที่พบใหม่" with the matched profile, or with "อุปกรณ์ LoRaWAN ทั่วไป" when no model matched. Register them there.

### Why `$deveui` in the topic

With a payload codec, the UG65 publishes only the codec's output: no DevEUI, no RSSI. The gateway's "Metadata" option adds them, but it needs firmware 60.0.0.44 or later; the brief lists 60.0.0.43. The `$deveui` topic variable is therefore how Aether knows which device sent a message. A topic that still contains the literal `$deveui` is refused.

## Payloads

The collector subscribes to `aether/lora/+/#` (`backend/internal/adapters/milesight`). The tenant comes from the gateway id in the topic, never from the payload. Only `<DevEUI>/up` is read; anything else under the gateway's tree, downlinks included, is kept only as the diagnostic copy (the last 100 messages per gateway).

| The UG65 sends | Aether |
|---|---|
| A **codec's output**, e.g. `{"battery":100,"leakage_status":"leak"}` | Uses these values as they are. |
| Its own **envelope** (no codec), with `devEUI`, `deviceName`, `fCnt`, `fPort`, `rxInfo[].rssi` / `loRaSNR` and the raw bytes in `data` | Keeps the names and radio figures. Decodes the raw bytes on port 85 for GS301, WS303, WS202 and VS330. The envelope's `devEUI` must match the topic's. |

**How the model is chosen** (it matters for raw frames: channel `0x03` means a leak on a WS303 and PIR on a WS202):
1. The profile the device is registered with.
2. A model named in its device or application name on the gateway, e.g. "WS303 ห้องน้ำ 2F".
3. The fields a codec emitted.

An unregistered device of unknown model gives no values until it is registered or named.

## Decoding raw frames

`Decode` is written from the channel tables Milesight publishes for each model (`Milesight-IoT/SensorDecoders` READMEs, 2026-09). The decoders themselves are GPL-3.0 and are not used. A frame is a sequence of `[channel][type][value]`, little-endian.

| Model | Channel / type | Field |
|---|---|---|
| all | `01 75` | `battery` (%) |
| all | `FF 01/0F/0B/FE`, `FF 09/0A/FF`, `FF 16` (8 B), `FF 08` (6 B) | Device information: read past, never a reading. |
| GS301 | `02 67` | `temperature` = int16 / 10 (°C) |
| GS301 | `03 68` | `humidity` = uint8 / 2 (%) |
| GS301 | `04 7D`, `05 7D`, `06 7D` | `nh3` = /100, `h2s` = /100, `h2s` = /1000 (ppm). `0xFFFE` / `0xFFFF` give `*_sensor_status` "polarizing" / "device error" instead of a value. |
| WS303 | `03 00` | `leakage_status`: 0 normal, 1 leak |
| WS202 | `03 00` | `pir`: 0 normal, 1 trigger |
| WS202 | `04 00` | `daylight`: 0 dim, 1 bright |
| VS330 | `02 82` | `distance` = uint16 (mm) |
| VS330 | `03 8E` | `occupancy`: 0 vacant, 1 occupied |
| VS330 | `04 8E` | `calibration_status`: 0 failed, 1 success |

An unknown channel ends the frame. Whatever was read before it is kept.

## Field mapping (decoded values → Aether)

| Model | Field | Aether metric | Reading kind |
|---|---|---|---|
| WS303 water leak | `leakage_status` `"leak"` / `"normal"` | `leak` 1 / 0. Raises `leak` / `leak_cleared` events. | leak |
| WS202 PIR & light | `pir` `"trigger"` / `"normal"` | `motion` 1 / 0. Drives occupied / vacant, like the MSP01. | occupancy |
| | `daylight` `"bright"` / `"dim"` (`"light"` / `"dark"` also accepted: Milesight's README says that) | `daylight` 1 / 0 | |
| VS330 bathroom occupancy | `occupancy` `"occupied"` / `"vacant"` | `motion` 1 / 0 (as Zigbee `presence`). Drives occupied / vacant. | occupancy |
| | `distance` (mm), `calibration_status` | `distance`, kept as a value | |
| GS301 odor | `nh3`, `h2s` (ppm), `temperature` (°C), `humidity` (%) | same names | environment |
| | `nh3_sensor_status` / `h2s_sensor_status` | kept as values | |
| All | `battery` (%) | `battery` | |
| All (envelope only) | best `rssi` / `loRaSNR` | `rssi` (dBm), `snr` (dB) | |

- Device information is never a reading. That covers versions, serial numbers, class, status, reset, and configuration echoed back in downlink replies.
- Nested objects such as calibration results are skipped.
- Any other number or status a codec emits is kept under its own name, so the generic LoRaWAN profile works with other products that have a codec.

### VS330 and the 5-minute hold

Occupied / vacant uses the same rule as a PIR: occupied on the first `motion` 1, vacant once no `motion` 1 has arrived for `OccupancyHoldSec` (300 s). A `vacant` uplink does not end the episode early.

The VS330 reports when its state changes and then once per report interval. If someone stays in a stall longer than 5 minutes and no uplink arrives in between, Aether marks the stall vacant. The next periodic `occupied` uplink starts a new episode. Before relying on stall-level occupancy, set the VS330 report interval below 5 minutes, or change the engine to end the episode on an explicit `vacant`.

## Liveness

LoRaWAN sensors report every 10–30 minutes, and the gateway sends no availability messages. Their streams keep **silence-based** liveness, so a device counts as offline once it has been quiet longer than an offline rule allows.

Give offline rules that cover LoRaWAN devices an `offline_after_sec` well above the reporting interval. For example, use 3600 s for a 20-minute interval; 300 s would flag them between every two uplinks.

## Not yet

- Downlinks: device configuration and commands.
- Product photos for the Milesight models. They should be the manufacturer's real product images.
- Verification with an uplink captured from the real UG65, both with a codec and without one.
