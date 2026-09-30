# Aether Edge: Tuya BLE devices, locally

Status: design, plus the parts that need no hardware.

- **Built:** phase B1, the protocol library `internal/tuyable` and its simulator `internal/tuyable/tuyablesim`; phase B2, the Edge
  transport (`internal/edge/ble`), migration 00043, the config gating and the sightings ingest (see "B2 as built" below). B2 is off
  everywhere until the server sets `EDGE_BLE=true` **and** an agent built with it is installed with `--ble`.
- **Built:** phase B3, the import of address, uuid and `sec_key`, the classification, the BLE settings route and the UI (see "B3 as
  built" below).
- **Not started:** phase B5, the ESPHome proxy.
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

## B2 as built

**Server.**
- Migration 00043 (`lock_timeout` 5 s both ways): the `core.tuya_devices` BLE columns above, every existing row `transport='wifi'`;
  `core.edge_ble_seen` with the tenant and restrictive gateway-scope policies of 00032; `core.edge_agents` gains `ble_state`,
  `ble_seen`, `ble_connected`, `capabilities` and `last_ble_at`; `devices_tuya_one_mode` covers `tuya-ble-device@1`. Its Down refuses
  while a `tuya-ble-device@1` registration is live.
- `EDGE_BLE` (default false): without it the profile is not in the catalog, a BLE registration is refused, and no agent is sent a
  BLE device. `GET /api/v1/catalog` reports `edge_ble`.
- Config pull: BLE devices (`transport="ble"`, an address or a uuid, the local key) are included only when `EDGE_BLE` is on and the
  agent sent `X-Aether-Edge-Caps: ble`; `local_capable` (the Wi‑Fi verdict) does not apply to them. The entry adds `mac`, `uuid`,
  `sec_key` (sealed like the local key, opened with the same keys), `product_id`, `protocol`, `mode`, `poll` and `dp_types`. Wi‑Fi
  entries are unchanged, byte for byte. The ETag is `"<revision>"`, or `"<revision>;ble"` when BLE devices are included, so a flag
  flip or a newly capable agent never gets a stale 304. The announced capabilities (known ones only) are stored on the agent row.
- `aether/edge/<gw>/ble` (sightings): at most 200 per message, one list per 30 s per gateway, entries unheard for a week forgotten,
  the newest 500 kept per gateway. An imported BLE device heard by address or uuid gets its `rssi` and advertised protocol.
- `health` may carry `ble:{state,adapter,seen,connected,sessions_ok,sessions_failed,queue}`; the gateway page's status
  (`GET /api/v1/gateways/:id/edge/status`) returns `ble_state` ("" for an agent that knows nothing of Bluetooth, `off` when it runs
  without it), `ble_seen`, `ble_connected`, `ble_devices` and `capabilities`.
- State from a BLE device goes through `tuya.FromBLE` (enum index → label, an index outside the range is dropped) and sets
  `last_read_at`; commands to one are wired by `tuya.WireBLE` (enum label → index), everything else exactly as over the LAN.

**Agent** (`internal/edge/ble`, only with `BLE_ENABLED=true`).
- `BLE_ADAPTER` (default `hci0`) and `BLE_MAX_CONNECTIONS` (1–5, default 1). Without `BLE_ENABLED` the poller drops BLE entries and
  the heartbeat says `ble.state="off"`.
- `Radio` (`radio.go`): `Scan`, `Connect`. `bluez_linux.go` is BlueZ over the system D-Bus (`tinygo.org/x/bluetooth` v0.16.0,
  `godbus/dbus/v5`), pure Go, no capability; the link polls `Connected` every 2 s and closes rather than drops notifications.
  `radio_other.go` is an always-unavailable radio for other systems; tests use `bletest` (a fake radio over `tuyablesim`).
- `Manager` (`manager.go`): keeps what it hears (Tuya adverts only, at most 500 addresses, oldest evicted), matches a device by
  address in either byte order or, with none configured, by the uuid in its advertisement. Each tick it picks jobs by priority
  (command, persistent hold, first read, poll) within the connection budget; a device must have advertised within 10 min to be
  connected. Modes: `auto` reads on its first advertisement and then every poll interval while heard; `on_demand` only for
  commands and every poll interval from start; `persistent` holds the connection (and retries 10 s after a drop). Poll: default
  15 min, minimum 5 min.
- A session: connect (10 s), `tuyable.Open` with the advertised protocol and FD50 flag (each request 10 s), `Status` unless it is
  a command job, then deliver pending commands (coalesced per DP, 30 s TTL; a failed write puts them back unless a newer value
  arrived), publish each report on `…/<device>/state` (bool, integer for value and enum index, string, a bitmap as its integer,
  raw as hex), and end after 3 s quiet or 30 s.
- Availability on the existing topic and reasons: online after a successful handshake; `auth_failed` at once when the device
  refuses the keys, or after three handshakes left unanswered in a row while it advertises; `busy` when the radio reports another
  central; `unreachable` otherwise, announced after 2 min of failures; `not_found` after 10 min without an advertisement. Backoff
  10 s doubling to 10 min; a refused key waits the full 10 min even for a command. Malformed keys are announced once and never tried.
- Sightings on `aether/edge/<gw>/ble` when the list changed, and every 5 min anyway (strongest first, at most 200); again after
  every broker reconnect.

**Install** (`--ble`). The script checks, before pulling anything or asking for the code, that `/run/dbus/system_bus_socket`
exists, that bluetoothd is active (`systemctl is-active bluetooth`, or `pgrep bluetoothd`) and that no Bluetooth rfkill switch is
set. The agent's `.env` gets `BLE_ENABLED=true`, `BLE_ADAPTER=hci0`, `BLE_MAX_CONNECTIONS=1` and
`DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket`; `compose.yaml` mounts `/run/dbus:/run/dbus:ro` and nothing else of
`/run`. The container keeps `cap_drop: [ALL]`, `read_only`, `no-new-privileges` and uid 10001.

**D-Bus access, and its honest limit.** The container reaches the host's whole system bus through that socket; what it may do there
is the host's D-Bus policy.
- `--ble` always writes `/etc/dbus-1/system.d/aether-edge-bluetooth.conf`: uid 10001 may call exactly what
  `tinygo.org/x/bluetooth` v0.16.0 uses as a central (read from its source): `Adapter1.SetDiscoveryFilter`/`StartDiscovery`/
  `StopDiscovery`, `Device1.Connect`/`Disconnect`, `GattCharacteristic1.WriteValue`/`StartNotify`/`StopNotify`,
  `ObjectManager.GetManagedObjects` and `Properties.Get`/`GetAll` on `org.bluez`. Not `Properties.Set` (it would let the agent power
  the adapter off or make it discoverable; the library sets properties only for its peripheral role), no `AgentManager1`,
  `LEAdvertisingManager1`, `GattManager1`, pairing, `ReadValue` or `RemoveDevice`.
- Checked against a real `dbus-daemon` with a fake `org.bluez` and a default-deny BlueZ policy: uid 10001 gets `StartDiscovery` and
  `Properties.Get`, and `AccessDenied` for `RemoveDevice` and `Properties.Set`; any other uid gets `AccessDenied` for everything.
- **The limit:** D-Bus policy only adds. Where the host's BlueZ policy lets every local user call `org.bluez` (upstream BlueZ's
  default, `<policy context="default"><allow send_destination="org.bluez"/>`), the rule narrows nothing: any local user, the agent
  included, may already call all of BlueZ. The installer says so when it sees that. There, the options are an AppArmor profile for
  the container that mediates D-Bus (`dbus send bus=system peer=(name=org.bluez) interface=… member=…` rules, loaded with
  `security_opt: [apparmor=aether-edge-ble]`; only on hosts whose kernel mediates D-Bus, such as Ubuntu), or a filtering proxy (below).
- **uid.** The rule is keyed to uid 10001, the container's user, which D-Bus sees as the same uid on the host. When the host has no
  account with that uid, the installer creates a system account `aether-edge` for it (no home, no shell), so no other account holds it
  by accident; when another account already has uid 10001, the installer warns that the rule applies to it too.
- **`--uninstall`** stops the stack, removes the rule file (reloading D-Bus) and the `aether-edge` account if the installer created
  it, and keeps the installation directory (credentials, a Zigbee2MQTT network key).
- **Filtering proxy, evaluated and not the default yet.** `xdg-dbus-proxy --filter` with `--call=org.bluez=<interface>.<member>@/org/bluez/*`
  rules for the same calls, in a sidecar whose socket is the only one mounted into the agent, would enforce the list whatever the
  host's policy. It is not simple enough to be the default in B2: there is no upstream image of it (Alpine packages
  `xdg-dbus-proxy` 0.1.6, a C program with GLib that the distroless agent image cannot run), so it needs an image of our own, built,
  pinned and published next to the agent image (a release decision), plus socket ownership between two containers. It is the
  recommended next hardening step.

**Commands to BLE devices.**
- **Locks are read-only.** A BLE device whose Tuya category is a lock or safe (`ms`, `jtmspro`, `jtmsbh`, `gyms`, `hotelms`, `bxx`,
  `videolock`, `photolock`, `mk`, `ms_category`: `tuya.LockCategory`) is refused every command (`400 not_settable`), its
  configuration entry has `"readonly":true` and no `dp_types`, and the agent refuses any write to a read-only device.
- **Deadlines.** `device_commands.confirm_sec` is 45 s for a BLE device (10 s otherwise). The commander publishes every edge command
  with `"expires_at"` (server time) and `"ttl_ms"`: two thirds of the confirm window (30 s for BLE, 6.67 s for Wi‑Fi). The agent uses the
  earlier of `expires_at` and its receipt time plus `ttl_ms`, drops a command already past it (logging a hint about the host clock),
  and never starts a BLE write after it; a BLE write takes at most 10 s, so the device's confirmation arrives within the window. A
  host clock running ahead of the server makes the agent drop commands, one running behind cannot make it late. Agents before 0.2
  ignore both fields.
- **No double actuation.** The commander publishes each command at most once (claim, commit, then publish). The agent coalesces per
  data point, and a BLE write that failed is retried only until the deadline and only with the same absolute value (toggles are
  resolved to an explicit value on the server), so a write that did reach the device but whose answer was lost writes that value
  again, not a second change.

**Spoofing.** Advertisements are not authenticated, so the agent trusts them only as far as it must. A device configured with an
address is found by that address only (either byte order); a device advertising the same uuid from another address is never
connected. Only a device configured without an address follows its uuid, and keeps the address it was first found at. When the heard
list is full, configured devices' addresses are never the ones forgotten, and they lead the sightings list, so a crowd of strong
strangers cannot push them out. The server matches sightings to imported devices by address in either byte order, or by uuid.
**B3 must not classify a device as BLE from a sighting alone:** a sighting is a claim anyone in radio range can make, so marking a
device BLE (and its address) needs the operator's confirmation in the import table.

**Radio details.** A connection attempt that runs past its 10 s is abandoned in BlueZ (`Device1.Disconnect` on the device object
cancels a connection in progress) and the connection slot stays taken until BlueZ answers or 15 s pass, so the adapter is never asked
for more connections than the budget. Stopping a scan waits at most 5 s. A persistent connection gives its slot up to a waiting command
(and is taken up again 10 s later), so persistent mode cannot starve commands even with one slot.

**Release.** B2 ships in edge image 0.2.1 (`ghcr.io/panudet-24mb/aether-edge:0.2.1`, tag `edge-v0.2.1`; the 0.2.0 tag failed CI and was never published); `domain.EdgeImageTag`
points at it, so the installer and `--update` pull it. BLE still stays off on the server until `EDGE_BLE=true`.

**Still needed on hardware (B0/B4):** that BlueZ lets uid 10001 in a `cap_drop: ALL` container scan and connect (Raspberry Pi OS,
Ubuntu with AppArmor), whether scanning must pause during a connection, how the host reports a device held by another central
(`busy` is recognised only when the radio says so), BlueZ's connect time without a deadline of ours, and everything in the B4
checklist.

## B3 as built

**Import.** After the device listing the import reads the factory records (`GET /v1.0/iot-03/devices/factory-infos`, 20 ids a call,
never for hub sub-devices) and stores, per device, the Bluetooth address (normalised to `aa:bb:cc:dd:ee:ff` from however Tuya writes
it), the uuid (from the listing, else the factory record) and the `sec_key` (the listing's `sec_key` or `secKey`), sealed like the
local key and kept only alongside one. Factory records are optional: a project that may not read them still imports
(`factory_infos: "unavailable"` in the job) and its BLE devices are matched by uuid. A re-import replaces both keys (a `sec_key` never
outlives its local key; one listed without a local key is dropped) but keeps an address it already knew when the new import had none.
An address of one repeated byte (`00:00:00:00:00:00`, `ff:…`) is a placeholder and counts as unknown. The job counts
`ble_candidates`: devices with a key, not reachable over Wi‑Fi, not behind a hub, with a real factory address (a uuid alone counts only
once the Edge hears it). The import never sets how a device is reached.

**What the Edge detected.** `GET /gateways/:id/tuya/devices` adds, per device, `detected`: `wifi` when the Edge sees it broadcasting
on the LAN, `ble_candidate` when a Bluetooth sighting matches its address (either byte order) or its uuid, `unknown` otherwise; with
the matching sighting's signal, protocol and bound flag, `ble_capable` (a candidate: not behind a hub, and heard or with a factory
address), `has_sec_key`, the
BLE mode and poll interval, the last read and `readonly` for locks. Nothing is acted on from this: it is a hint for the operator.

**The operator decides.** `POST /gateways/:id/tuya/devices/:tuya_id/ble` with `{transport: "ble" | "wifi" | "auto", mode, poll_seconds}`
(owner/admin, `EDGE_BLE` on, audited as `tuya.transport_set_<transport>:<tuya id>` or `tuya.ble_settings_changed:<tuya id>` on the
gateway; `audit_logs` has no detail column, so the device is named after the action, like `tuya.key_forgotten:<tuya id>`) is the only
way a device becomes BLE.
Bluetooth needs an address or uuid and a device not behind a hub (`ble_address_unknown`); the poll interval is 5 minutes to a day;
the transport of a registered device does not change (`409 registered`: remove the registration first). Registration follows it:
the BLE profile only for a device set to BLE (`ble_not_confirmed`), the Wi‑Fi profile never for one (`set_to_ble`), also when a
registration moves to another gateway or is restored. Create, move and restore check under the gateway row `FOR SHARE`, the row the
transport change holds `FOR UPDATE`, so a registration and a change of the same device never interleave. A sighting records the
advertised protocol only while none is known; later sightings do not change it.

**Discovery.** A device set to BLE is listed with source `tuya_ble` and the BLE profile; one only heard over Bluetooth stays source
`tuya` with `ble_seen: true` and no profile. With `EDGE_BLE` off, `tuya_ble` devices are left out and counted (`hidden_ble`, with
`edge_ble: false`), and the page says why.

**UI.**
- The import table's "ทาง" column shows Wi‑Fi (where the Edge saw it), Bluetooth (signal) or nothing yet, with a pairing line (key
  refused, no longer bound to the app, last read).
- Each imported device that may be Bluetooth gets "ใช้ทาง Bluetooth": a small form to confirm, with the mode and read interval. A
  device set to Bluetooth gets "ตั้งค่า Bluetooth" and, while unregistered, "กลับไปใช้ Wi‑Fi". Locks say they are read-only.
- The installer offers `--ble` with the host requirements (BlueZ running, not blocked by rfkill, the narrowed D-Bus policy the
  installer writes). An agent older than 0.2.0 is told to update and reinstall with `--ble`; the steps show the radio's state.
- A registered BLE device's inspector shows its signal, mode, interval, last read and offline reason in Bluetooth terms.

**Tests.** `backend/tests/edge_ble_import_test.go` (import with a fake cloud: stored address, uuid and sealed `sec_key` in both
spellings; nothing in the clear; classification by reversed address and by uuid; owner/admin only; bad settings; registration rules;
discovery; `EDGE_BLE` off; another workspace; re-import without factory records keeps the address and the choice; forgetting the key
drops the `sec_key`), `internal/adapters/tuyacloud/factory_test.go` (batching, `secKey`), `internal/adapters/edge/mac_test.go`.

## Phases

| Phase | Content | Hardware |
|---|---|---|
| B0 | Pin the references and generate vectors (done). Run a scan spike in a `cap_drop: ALL` container on Raspberry Pi OS and on Ubuntu (AppArmor), and record the D-Bus policy needed. Check `uuid`, `sec_key` and `factory-infos` against a real Tuya project. | Linux host with Bluetooth |
| B1 | `internal/tuyable` and `tuyablesim` (done). | None |
| B2 | `internal/edge/ble`: `Radio` interface, BlueZ radio, scheduler, sessions. Agent config and health. Migration 00043, route, config gating (done; image not yet published). | BlueZ smoke test only |
| B3 | `FactoryInfos` and `sec_key` in the import, classification, BLE settings route, UI, OpenAPI (done). | None (fake cloud) |
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
| `backend/internal/edge/ble/radio.go` | `Radio`, `Advertisement`, `Device`, `Sighting`, `Health`, `Publisher`, radio states and reasons |
| `backend/internal/edge/ble/manager.go` | Scan loop, scheduler, sessions, command coalescing, availability, sightings, health |
| `backend/internal/edge/ble/bluez_linux.go`, `radio_other.go` | BlueZ over D-Bus on Linux; an unavailable radio elsewhere |
| `backend/internal/edge/ble/bletest` | Fake radio over `tuyablesim` devices (connect failures, silence, scan failures, open-link count) |
| `backend/migrations/00043_tuya_ble.sql` | BLE columns, `core.edge_ble_seen`, agent BLE state, one-mode index |
| `backend/internal/adapters/edge/route.go` | `BLESightings` topic, `ParseBLESightings`, health `ble` block |
| `backend/internal/adapters/tuya/wire.go`, `import.go` | `FromBLE`, `WireBLE`, `DPTypes` |

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
