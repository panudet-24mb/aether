# Aether Edge: Tuya BLE devices, locally

Status: design, plus the parts that need no hardware.

- **Built:** phase B1, the protocol library `internal/tuyable` and its simulator `internal/tuyable/tuyablesim`.
- **Not started:** phases B2–B5, which cover the Edge radio, storage, import and UI.
- **Not verified:** nothing here has been checked against a real Tuya BLE device. Everything is checked against vectors taken from the reference implementation and against the simulator.

This builds on the Wi‑Fi path:
- [aether-edge.md](aether-edge.md): the agent, topics, key custody and commands.
- [tuya-local.md](tuya-local.md): the one-time key import.
- [tuya-cloud.md](tuya-cloud.md): cloud mode. It remains the answer for mesh devices and anything behind a Tuya hub.

## What it is

Tuya BLE devices use a point-to-point GATT protocol, encrypted with the same per-device `local_key` as Wi‑Fi devices. Examples are Fingerbots, BLE temperature, humidity and soil sensors, and some switches and locks.

Aether Edge will talk to them through the site host's own Bluetooth radio. Keys come from the same one-time Tuya IoT import, so there is no Home Assistant, no Tuya cloud at runtime and no hub.

Unlike Wi‑Fi, battery sensors are usable over BLE. They advertise, accept a short connection, report their data points (DPs) and go back to sleep.

**Out of scope:**
- **Tuya BLE mesh and SIG mesh products** (mostly lights and some plugs). They use mesh network and application keys held by the app or a hub.
- **Devices bound to a Tuya multi-mode hub.** The hub holds their single connection.

Both groups should use Tuya Cloud mode.

## Protocol

Ported from ha_tuya_ble (MIT); the reference commits are pinned below.

| Layer | Format |
|---|---|
| Advertisement | Service data on 0xA201 (legacy) or 0xFD50 (TuyaOS): `type(1) ‖ product id`. Manufacturer data, company 0x07D0: `flags(1) ‖ protocol(1) ‖ 4 bytes ‖ AES-CBC(key = IV = MD5(product id), uuid)`. Bit 7 of flags is "bound". No DP values. |
| GATT | Legacy: notify `0x2B10`, write `0x2B11`. FD50: notify `00000002-0000-1001-8001-00805f9b07d0`, write `00000001-…07d0`. 20-byte packets whatever the negotiated MTU. |
| Fragment | `varint(packet#) [varint(total length) ‖ protocol<<4 if packet# = 0] ‖ chunk`. The varint is 7 bits per byte, least significant group first. |
| Message | `security_flag(1) ‖ IV(16) ‖ AES-128-CBC(key, IV, plaintext)` |
| Plaintext | `seq(4) ‖ response_to(4) ‖ code(2) ‖ length(2) ‖ data ‖ CRC16(2) ‖ zero pad to 16`, big-endian. CRC-16/MODBUS (init 0xFFFF, poly 0xA001). |

Security flags select the key: 4 (or 14) is the login key, 5 (or 15) the session key, 1 the auth key. The login
and session keys are 16-byte MD5 digests, so AES-128; the auth key is 32 bytes, so frames sealed with it are
AES-256-CBC.

**Protocol nibble before the handshake.** The first fragment of every message carries a protocol version, and
DEVICE_INFO goes out before the device has said which one it speaks. Both references start at 2, take the
advertisement's manufacturer-data byte 1 when they have it, and force 2 for the FD50 device-info quirk.
`tuyable` does the same through `Config.AdvertisedProtocol`, which Edge fills from `ParseAdvert`.

**Keys.**
- The key material is `local_key[:6]`. On "v2" devices the cloud returns a 16-character `sec_key`, and the material becomes `local_key ‖ sec_key`, with flags 14 and 15.
- Login key = `MD5(material)`.
- Session key = `MD5(material ‖ srand)`.
- Two products keep the legacy derivation even when a `sec_key` exists: `qcrilcpr` and `mknd4lci`.

**Handshake.**
1. `DEVICE_INFO` (0x0000) is sent with the login key. On FD50 it carries the payload `00 f3` for products `jntxv3q4`, `9hdajpiw`, `2hmqh0ty` and `qcrilcpr`.
2. The answer (≥ 46 bytes) carries:
   - device version (bytes 0–1)
   - protocol version (2–3)
   - flags (4)
   - bound (5)
   - srand (6–11)
   - hardware version (12–13)
   - auth key (14–45)
3. `PAIR` (0x0001) sends `uuid ‖ local_key[:6] ‖ device id`, zero-padded to 44 bytes. The answer is 0 (paired) or 2 (already paired); any other result means the keys are wrong.

**Data points.** A KLV list of `id(1) type(1) length(1 or 2) value`.
- Protocol 3 uses a 1-byte length with `DPS` (0x0002).
- Protocol 4 uses a 2-byte length with `DPS_V4` (0x0027), prefixed by `00 ‖ dp_seq(4)`. The answer's byte 5 is the result.
- Types:
  - raw 0
  - bool 1
  - value 2 (signed 32-bit)
  - string 3
  - enum 4 (the **index** into the DP's range, sent as 1, 2 or 4 bytes)
  - bitmap 5
- `DEVICE_STATUS` (0x0003) makes the device push every DP. Protocol 2 devices report but cannot be written.

**Device-initiated messages** are acknowledged with the same code and `response_to` set:
- `RECEIVE_DP` 0x8001 and `RECEIVE_TIME_DP` 0x8003 get an empty ack.
- `RECEIVE_SIGN_DP` 0x8004 and `RECEIVE_SIGN_TIME_DP` 0x8005 get `dp_seq(2) ‖ flags ‖ 0`.
- `RECEIVE_DP_V4` 0x8006 and `RECEIVE_TIME_DP_V4` 0x8007 get the first 7 bytes plus `0`, unless bit 7 of `send_flags` is set.
- Timestamps are either type 0 (13 ASCII digits of Unix ms) or type 1 (4-byte Unix seconds).
- `TIME1_REQ` 0x8011 gets `ASCII Unix ms ‖ int16 UTC offset in hundredths of an hour` (UTC+7 is 700).
- `TIME2_REQ` 0x8012 gets `yy mm dd HH MM SS weekday(Mon=0) ‖ offset`.
- The protocol-4 `dp_seq` of a DP write is drawn from the frame sequence counter, as in the reference: the write
  takes one number for `dp_seq` and the next for its frame.

### Session behaviour (`session.go`)

- **Key rejection is narrow.** Only a message sealed with the login flag that fails its CRC or length check,
  while DEVICE_INFO waits, fails that request with `ErrKeyRejected`. A garbled notification, a message that does
  not decrypt, an answer whose code does not match its request, or a device message with an unknown code is
  counted in `Stats` and ignored. None of these ends the session.
- **What ends a session:** `Close`, the link's notifications ending (with the link's `Reasoner` reason if it
  has one), or a failed link write. `Done` is then closed and `Err` wraps `ErrClosed`.
- **Timeouts.** A request that gets no answer within `ResponseTimeout` fails with `ErrTimeout`, which wraps
  `context.DeadlineExceeded`. If the caller's own context ends first, its error is returned as is.
- **Reports are never lost.** Every report is acknowledged. When more than `ReportBuffer` wait for the reader,
  they are merged into one report with the latest value of each DP and `Coalesced` set, and
  `Stats().Coalesced` counts it. Edge can call `Status` to re-read.
- **Logging.** `Keys` and `Config` never print their keys: `String`, `GoString` and `slog` all redact them.

### Where this package departs from the reference

1. **Sign reports.** ha_tuya_ble (both repositories) starts reading the DPs of `RECEIVE_SIGN_DP` and `RECEIVE_SIGN_TIME_DP` at offset 2, which is the flags byte. `tuyable` reads them from offset 3, after `dp_seq(2) ‖ flags(1)`. Real captures in B4 must confirm this.
2. **Enum indices.** The reference encodes enum indices unsigned but decodes them signed, so index 128–255 sent in one byte comes back negative. `tuyable` decodes enums unsigned. Enums wider than 4 bytes are rejected.
3. **CRC check.** The reference skips the CRC check when the plaintext ends exactly at the data. `tuyable` always requires the CRC, which is how a wrong key is detected.
4. **Sign-time reports.** The reference drops the device timestamp of `RECEIVE_SIGN_TIME_DP` and uses the receive time. `tuyable` keeps the device's timestamp.
5. **TIME offset.** `tuyable` sends the current UTC offset, daylight saving time included. The reference sends
   the standard offset (`-time.timezone/36`), so the two differ in DST zones. Thailand has no DST.
6. **Wrong keys can look like timeouts.** A device that cannot decrypt our `DEVICE_INFO` may stay silent, and the session then fails with `ErrTimeout`, not `ErrKeyRejected`. Edge treats repeated handshake timeouts from a device that is advertising as a key problem, not as "not found".

## Keys and import (phase B3)

The Wi‑Fi import (`internal/app/tuya_import.go` with `tuyacloud.Client.Devices`) already stores `local_key`. For BLE it also needs:

- **`uuid`**, from the device listing or `GET /v1.0/iot-03/devices/{id}`. It can also be read from the advertisement, as shown above.
- **The MAC**, from `GET /v1.0/iot-03/devices/factory-infos?device_ids=` (up to 20 ids per call, returning `id, uuid, mac, sn`). Some devices report it byte-reversed, so match both orders.
- **`sec_key` / `secKey`**, when the listing returns it. FD50 devices without an OpenAPI `sec_key` are unsupported. B0 must measure how common that is.

The auth key comes from the device during the handshake and is never stored.

Custody is the same as Wi‑Fi local keys:
- `local_key` and `sec_key` are sealed at rest with `DeriveKey("tuya-local-keys")`.
- They are opened only in `EdgeConfig`, and only for the gateway's own agent.

A one-time import costs about ceil(N/20) extra calls, so the Tuya trial plan is enough (non-commercial use).

## Radio on the Edge (phase B2)

| | A. Host BlueZ over D-Bus (`tinygo.org/x/bluetooth`) **(default)** | B. Raw HCI (`go-ble/ble`) | C. ESP32 ESPHome `bluetooth_proxy` **(optional, B5)** |
|---|---|---|---|
| Container privileges | None: `cap_drop: ALL` kept, `/run/dbus:/run/dbus:ro` mounted, perhaps a host D-Bus policy file | CAP_NET_ADMIN (+NET_RAW); takes the adapter from bluetoothd | None; TCP 6053 |
| Host changes | BlueZ running and unblocked (`rfkill`) | Stop bluetoothd | None |
| Concurrent connections | Pi onboard: plan 1 (max 2); USB CSR8510/BCM20702: 3–5 | Similar | 3 by default, up to 5 recommended |
| Maturity | Good; API marked "not stable" (pin the version) | Stale | Go clients are experimental |
| Testable without hardware | Yes, behind a `Radio` interface | Yes | Yes (fake native-API server) |
| Decision | Use | Rejected | Later, for hosts without usable Bluetooth |

**BlueZ access.** BlueZ's default D-Bus policy lets any user send to `org.bluez`, so uid 10001 needs no capabilities.
- Distributions may tighten this. The installer's `--ble` would then write `/etc/dbus-1/system.d/aether-edge-bluetooth.conf` for uid 10001.
- Ubuntu's AppArmor may block D-Bus from the docker-default profile. The fallback is a narrow custom profile. Never use `--privileged` or add capabilities; if a host cannot do this, use an ESP32 proxy.

**Build constraints.** The BlueZ radio lives in `*_linux.go`. On darwin, tinygo bluetooth uses CoreBluetooth through cgo, so `go test ./...` on a Mac must only ever see the fake radio.

## Device classes and scheduler

| Class (Tuya category) | Mode |
|---|---|
| Temperature, humidity, soil and CO2 (`wsdcg`, `zwjcy`, `co2bj`), battery | On advertisement. When a device is heard and its last read is older than the poll interval (default 15 min, minimum 5), connect, request `DEVICE_STATUS`, take reports, and disconnect after about 3 s idle. |
| Door and PIR (`mcs`, `pir`), battery | Same. Latency is minutes. **Never a source for SOS or hazard alerts.** |
| Fingerbot and BLE switches (`szjqr`, `kg`) | On command: connect, write, wait for the confirming report, linger about 10 s. Poll every 60 min for battery. Commands take about 2–8 s. |
| Locks (`ms`, `jtmspro`) | Read-only (battery, state). **No unlock commands** until the user explicitly decides to allow them. |
| Mains BLE devices, opt-in | Persistent connection with reconnect. This blocks the phone app, because a device accepts one central at a time. |

**Scheduler.**
- One priority queue: commands first, then the first read after an advertisement, then polls.
- A global connection budget, `BLE_MAX_CONNECTIONS`: 1 on onboard radios, up to 3.
- Per-device backoff from 10 s to 10 min.
- Hard timeouts: connect 10 s, handshake 10 s, session 30 s.
- Commands coalesce per DP (latest wins), with a 30 s TTL.

**Availability reasons** (the existing set):
- `not_found`: no advertisements.
- `unreachable`: connect or handshake failures.
- `auth_failed`: `ErrKeyRejected`, or repeated handshake timeouts while the device is advertising.
- `busy`: the device advertises but refuses connections, typically because the phone app or a hub holds it.

## Architecture in Aether (phases B2–B3)

**Storage.** A new migration, with the number checked at implementation time.
- `core.tuya_devices` gains:
  - `transport` (`wifi` | `ble` | `unknown`, default `wifi`)
  - `ble_mac`, `ble_uuid`, `sec_key_sealed`
  - `ble_protocol`, `ble_mode` (`auto` | `on_demand` | `persistent`), `ble_poll_seconds` (300–86400)
  - `last_read_at`, `rssi`
- `core.edge_ble_seen` (tenant, gateway, mac, uuid, product id, protocol, bound, rssi, last seen) uses the same tenant and gateway-scope policies as 00032.
- `core.edge_agents` gains `ble_state`, `ble_seen`, `ble_connected` and `capabilities`.
- New profile `tuya-ble-device@1` (radio `tuya-ble`, `Verified: false`).

**Classification.** A device is `wifi` if it has been seen on the LAN, `ble` if seen in a BLE scan (by MAC in either byte order, or by uuid), and `unknown` otherwise. An owner or admin can override this.

**Config pull.**
- BLE devices are sent only when the server flag `EDGE_BLE=true` is set **and** the agent sends `X-Aether-Edge-Caps: ble`, so 0.1.x agents never see them.
- The ETag includes the capability set.
- Each device entry carries MAC, uuid, keys, `dp_types`, mode and poll interval.

**Data flow.**
- **Ingest.** The agent publishes BLE DPs in the existing `{"dps":{"<id>":value}}` form, so ingest (`tuya.State` with `dp_map`) is reused. One difference: BLE enums arrive as indices, so `tuya.FromBLE` maps index to range label and `tuya.WireBLE` maps label to index.
- **Commands.** Commands use the existing path: `device_commands`, then mqtt-commander, then `aether/edge/<gw>/<dev>/set`. The commander and ACL are unchanged.
- **Agent topics.**
  - New topic `aether/edge/<gw>/ble` carries sightings: at most 200 per message, at most every 30 s.
  - `health` gains a `ble` block.

**API and UI.**
- The Tuya device list gains transport, RSSI, protocol and last read.
- `POST /gateways/:id/tuya/devices/:tuya_id/ble` sets mode, poll interval and transport override (owner or admin).
- The import table gets a transport column and pairing state.
- The Edge installer gets a `--ble` option.

## Phases

| Phase | Content | Hardware |
|---|---|---|
| B0 | Pin the references and generate vectors (done). Run a scan spike in a `cap_drop: ALL` container on Raspberry Pi OS and on Ubuntu (AppArmor), and record the D-Bus policy needed. Check `uuid`, `sec_key` and `factory-infos` against a real Tuya project. | Linux host with Bluetooth |
| B1 | `internal/tuyable` and `tuyablesim` (done). | None |
| B2 | `internal/edge/ble`: `Radio` interface, BlueZ radio, scheduler, worker. Agent config and health. Migration, route, config gating. Edge image 0.2.0 (ask before publishing). | BlueZ smoke test only |
| B3 | `FactoryInfos` and `sec_key` in the import, classification, BLE settings route, UI, OpenAPI. | None (fake cloud) |
| B4 | Real-device validation (below). | Devices |
| B5 | Optional ESPHome proxy radio behind the same `Radio` interface. | ESP32 |

## Not yet verified on hardware

- Handshake and pairing on real legacy, v4 and FD50 devices, including the FD50 device-info quirk and the `sec_key` derivation.
- DP offsets in sign reports (see the first departure from the reference, above).
- Whether a device answers or stays silent on a wrong key.
- Advertisement cadence, DuplicateData behaviour, and scanning while a connection is being set up.
- BlueZ access from a `cap_drop: ALL` container on the target distributions.
- MAC byte order from `factory-infos`, and whether the trial plan returns `sec_key`.

### B4 buy list

- A **Fingerbot Plus** (`szjqr`), the best-covered device in ha_tuya_ble: about US$20–30.
- A **Tuya BLE soil sensor SGS01** (product `gvygg3m8`), supported in both reference repositories: about US$10–15.
- One generic **Tuya "Smart Life" BLE LCD temperature/humidity sensor** (`wsdcg`), to exercise the generic DP path: about US$5–10.
- Optional: a CSR8510A10 or BCM20702A0 USB dongle, and an ESP32-C3/S3 or M5Stack Atom for B5. About US$5–10 each.
- Avoid anything labelled "Bluetooth Mesh", "SIG Mesh" or "requires gateway". No locks in the first round.
- Captures: Android's "Bluetooth HCI snoop log" while using Smart Life, or an nRF52840 dongle with nRF Sniffer.

### B4 checklist

1. Pair each device directly in Smart Life, not through a Tuya hub.
2. Run the import. Check that the MAC matches the advertisement in either byte order.
3. The protocol version is reported, and the handshake and pairing return 0 or 2.
4. The status read's DP types match the cloud spec, including enum index mapping.
5. Measure the Fingerbot command round-trip latency.
6. Sensor cadence and battery DP over 24 h.
7. With the phone app open or the hub powered, the device reports `busy`.
8. Re-pair in the app: `auth_failed`. Re-import: the device recovers.
9. Unplug the adapter or block it with rfkill: `ble.state = no_adapter`.
10. Two devices at once within the connection budget. Check RSSI and range.
11. Restart the Edge. Take Aether offline.
12. `docker inspect` shows effective capabilities of 0.
13. Save captures as fixtures in `internal/tuyable/testdata`, then set `Verified: true`.

## Risks

- **Protocol variants.** v4 and FD50/`sec_key` support exists only in a fork that describes itself as "unstable quality". `sec_key` may not be available from OpenAPI, and firmware quirks (MAC reversal, per-product derivation) exist.
  - Mitigation: vectors from the reference, capture fixtures in B4, per-device `ble_protocol`, and a clear `ErrUnsupportedProtocol`.
- **Single central.** A phone app or hub steals the connection, so on-demand mode is the default.
- **Radio.** A Pi's onboard Bluetooth shares its chip with Wi‑Fi. Recommend a USB dongle.
- **Container privileges.** Covered under Radio on the Edge above.
- **Battery.** Minimum poll of 5 min, default 15; disconnect quickly.
- **Safety.** No lock commands. BLE sensors are never SOS or hazard sources.
- **No message authentication.** Frames carry a CRC, not a MAC, and AES-CBC is not authenticated. Within one
  connection, an attacker in radio range who can inject notifications could replay or flip bits in frames
  undetected; the session does not check sequence numbers for replay, like the reference. Each connection
  derives a new session key from the device's random, so frames cannot be replayed across connections. This
  is the protocol's weakness, not the port's: keep sessions short (on-demand mode) and never use BLE devices
  for safety functions.
- **Legal.** MIT port with attribution in `internal/tuyable/LICENSE.ha_tuya_ble`. The protocol is reverse-engineered for interoperability with the user's own devices. No Tuya documentation text is copied.

## Code

| Path | What |
|---|---|
| `backend/internal/tuyable/frame.go` | Codes, flags, CRC16, `Plaintext`, `Seal`, `Unseal` |
| `backend/internal/tuyable/fragment.go` | Varint, `Fragment`, `Reassembler` (bounded to `MaxMessage`) |
| `backend/internal/tuyable/keys.go` | Key derivation (legacy and `sec_key`), product quirks, `PairRequest` |
| `backend/internal/tuyable/dp.go` | DP encode and parse (v3/v4), timestamps, TIME1/TIME2 answers |
| `backend/internal/tuyable/adv.go` | GATT UUIDs, `ParseAdvert`, `EncodeAdvert` |
| `backend/internal/tuyable/session.go` | `Link` interface and its contract (plus the optional `Reasoner`). `Session`: handshake, status, DP writes, report acknowledgements and coalescing, clock answers, `Stats` |
| `backend/internal/tuyable/tuyablesim` | Fake device over an in-memory `Link`. Knobs: protocol 2/3/4 and `sec_key`, `RejectKey`, `PairResult`, `AlreadyBound`, `Mute`/`SetMute`, `AskTime`, `NotifyMTU`, `DropFragment`/`SetDropFragment`, `SingleCentral`, `AfterPair`, `Glitch`, `Push` for every report kind, `Inject` for arbitrary session frames, `Advert` |
| `backend/internal/tuyable/testdata/gen_vectors.py`, `vectors.json` | The generator (reference functions, MIT notice, pinned pycryptodome) and its output; see the README |
| Fuzzers | `FuzzReassemble`, `FuzzParseDPs`, `FuzzParseReport`, `FuzzParseAdvert`, `FuzzSealUnseal`, and `FuzzSession` (arbitrary frames sealed with the real session key, so they pass the CRC) |

## Sources

- PlusPlus-ua/ha_tuya_ble (MIT), commit `6037ac5a04ceb23a36d1b88e2303aa1da7fdbe83`: https://github.com/PlusPlus-ua/ha_tuya_ble
- ha-tuya-ble/ha_tuya_ble (MIT; v4, FD50, `sec_key`), commit `40899aeff5f1bcb63aca26fba4ee59b76e101681`: https://github.com/ha-tuya-ble/ha_tuya_ble
- roquerodrigo/ha-tuya-ble (MIT): https://github.com/roquerodrigo/ha-tuya-ble
- Tuya BLE SDK guide: https://developer.tuya.com/en/docs/iot/tuya-ble-sdk-user-guide?id=K9h5zc4e5djd9
- Tuya Bluetooth mesh local control: https://developer.tuya.com/en/docs/iot-device-dev/bluetooth_software_map_mesh_local_control?id=Kd5wtgrwji5nh
- Tuya OpenAPI, device info: https://developer.tuya.com/en/docs/cloud/d00d20c097?id=Kag2xtiyewd3r
- Tuya OpenAPI, factory information: https://developer.tuya.com/en/docs/cloud/4dafcb9bd5?id=Kag2y3j30fndv
- Home Assistant Bluetooth integration (adapter advice): https://www.home-assistant.io/integrations/bluetooth/
- BlueZ D-Bus policy: https://raw.githubusercontent.com/bluez/bluez/master/src/bluetooth.conf
- tinygo-org/bluetooth (BSD-3): https://github.com/tinygo-org/bluetooth
- go-ble/ble: https://github.com/go-ble/ble
- ESPHome Bluetooth proxy: https://esphome.io/components/bluetooth_proxy/
