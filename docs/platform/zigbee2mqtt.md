# Zigbee2MQTT gateways (phase 1: ingest and display; phase 2: commands)

Status 2026-09-23: implemented and tested against a simulated bridge. **Not yet verified with real hardware.** The gateway model and the `tuya-ts001x-switch@1` profile stay `Verified: false` until a captured `bridge/devices` from the real coordinator replaces the synthetic fixtures.

## Why

Tuya Zigbee hubs only talk to Tuya cloud. To use Tuya Zigbee wall switches without the cloud, a Zigbee coordinator on site (for example an SMLIGHT SLZB-06M on the LAN) is driven by **Zigbee2MQTT** on a small always-on host. Zigbee2MQTT connects **out** to Aether's broker over TLS, like a Minew MG3 does. Nothing inbound needs to be opened at the building.

```
Tuya switch ──Zigbee──▶ SLZB-06M ──LAN──▶ Zigbee2MQTT host ──MQTT/TLS 8883──▶ Aether broker ──▶ collector
```

## Requirements

- **Zigbee2MQTT 2.x or newer.** The generated snippet uses `health.interval`, and liveness relies on the `bridge/health` heartbeat that 2.x publishes every `health.interval` minutes (default 10, zigbee2mqtt.io/guide/usage/health.html).
- **The broker must accept 1 MiB packets.** See "Rolling out" below.

## Setup

1. In "เชื่อมต่ออุปกรณ์", add a gateway of model **Zigbee2MQTT** and issue its MQTT account.
2. The gateway panel shows the `mqtt:` section of `configuration.yaml` once (it contains the password). Paste it into Zigbee2MQTT and copy the broker CA (`<secrets>/mqtt/broker/ca.crt`) to the path it names. The generated section sets:
   - `server: mqtts://<MQTT_PUBLIC_HOST>:8883`
   - `base_topic: aether/z2m/<gateway id>`
   - `user` and `client_id`: `gw-<gateway id>`
   - `include_device_information: true`
   - `homeassistant.enabled: false`: discovery topics are outside the gateway's ACL.
   - `availability.enabled: true`: required, see Liveness below.
3. Pair the switches in Zigbee2MQTT. They appear under "อุปกรณ์ที่พบใหม่" of that gateway. Register them with the "Tuya Zigbee wall switch" profile.

## Topics

Relative to `aether/z2m/<gateway id>/`. Routing lives in `internal/adapters/zigbee2mqtt/route.go`.

| Topic | What Aether does |
|---|---|
| `bridge/devices` (retained) | Upserts `core.z2m_devices`, keyed by **IEEE address**. Devices missing from the list are marked removed. Gangs are derived from `exposes`. |
| `bridge/state` (retained, the bridge's last will) | `core.z2m_bridges.state` online/offline |
| `bridge/info`, `bridge/health` | Bridge version and heartbeat |
| `<friendly name>` | Device state. Stored as a sample and evaluated for events. |
| `<friendly name>/availability` | Device online/offline |
| `…/set`, `…/get`, `bridge/request/*`, `bridge/response/*`, `bridge/logging`, `bridge/definitions`, … | Ignored (acknowledged, not stored) |

A friendly name may contain `/`. Devices are always resolved to their IEEE address, so renames do not break a registration.

**Reserved friendly names.** A device topic is classified by its suffix. A friendly name that ends in `/set`, `/get` or `/availability`, or contains `/set/` or `/get/`, is therefore read as a command or an availability message, not as the device's state. Its reports are ignored or misread. Do not name devices that way; the gateway panel repeats this.

## Model

- **Gangs.** Each `switch` expose contributes its `state` feature. Outputs are ordered as on the wall (bare `state`, then `l1…l4`, then `left < center < right`) and become gangs 1–4. Settable follows the access bit 2. Following zigbee2mqtt.io:
  - TS0011 exposes `state`.
  - TS0012 exposes `state_left` and `state_right`.
  - TS0013 exposes `state_left`, `state_center` and `state_right`.
  - TS0014 exposes `state_l1` to `state_l4`.
  - TS0601 variants match the profile only when their exposes contain switch outputs.
- **Reading.** Kind `switch`, frame `z2m-state@1`, metrics `sw1..sw4` (1 = ON) and `linkquality`.
- **Events.**
  - `switch_on` / `switch_off` fire once per gang change, with detail `{gang, source:"external"}`.
  - The first report of a gang is only a baseline.
  - They are not alert-rule types yet and never `button`, so a light switch can never raise SOS.
- **Samples.** Every state message is stored. Minew samples are keyed by the payload digest so a QoS 1 redelivery is stored once. Z2M samples also include the receive time in the key, because a switch legitimately repeats `ON, OFF, ON`.
- **Liveness.**
  - Z2M streams have `sensor_streams.liveness = 'reported'`. The silence-based per-stream scan skips them, because a wall switch is quiet until someone flips it.
  - Each device's offline/online comes from its availability messages, once per change, and follows the normal offline rules and shadow mode (`detail.source = "availability"`).
  - **The bridge goes down** (its last will `bridge/state = offline`): every reported device of that gateway goes offline with it (`source = "bridge_offline"`).
  - **The bridge goes silent** without the last will reaching the broker: no message at all for `Z2MSilentAfter` (25 min, which covers one missed 10-minute `bridge/health`). `ScanOffline` then does the same (`source = "bridge_silent"`), whether or not an offline rule exists, because it keeps the displayed state honest.
  - **The bridge comes back** (any message other than its own `offline`): devices whose last own availability was not `offline` are restored at once (`source = "bridge"`). The others stay offline until their availability report, which Zigbee2MQTT republishes (retained) when it reconnects. A state report from a device also brings it back online.
  - With availability disabled in Zigbee2MQTT, Aether cannot tell an idle switch from a dead one.
  - A device unpaired from the coordinator (missing from `bridge/devices`) returns to silence-based liveness, so it ages out like any tag that stopped transmitting. A list cut at 500 devices removes nothing.
- **Radio check.** A Zigbee profile can only be registered (or moved) under a Zigbee2MQTT gateway, and only Zigbee profiles there (`domain.ProfileAllowedOn`).
- **Project scope.** `core.z2m_devices` and `core.z2m_bridges` follow their gateway's project, like every gateway-keyed table.

## Security

- The Z2M account gets `topic readwrite aether/z2m/<id>/#`: its own tree only (`cmd/mqtt-provisioner`, `render`). It cannot publish to `/aether/gateways/...` or another gateway's tree.
- The collector reads `aether/z2m/+/#`. It re-checks every message against the gateway registry (revoked gateway: refused; gateway of another model: refused).
- The broker's `max_packet_size` is 1 MiB (it was 256 KiB), because `bridge/devices` of a larger network exceeds 256 KiB. The Go side caps `bridge/devices` at 1 MiB and every other message at 64 KiB. It logs every rejected message with its topic and size, and warns when a `bridge/devices` is over 256 KiB.
- **Poison messages.** PostgreSQL cannot store NUL. A message containing a NUL byte or `\u0000` is rejected as permanently invalid, so the collector acknowledges and drops it instead of retrying and exiting. Any PostgreSQL data exception (SQLSTATE class 22) during capture is treated the same way, on the Minew path too. NUL padding inside `bridge/devices` strings (some Tuya model ids) is stripped instead.

## Rolling out on an existing installation

1. Deploy the images; migration `00028` runs on start.
2. Re-render `mosquitto.conf` so the broker accepts 1 MiB packets. Until then a `bridge/devices` over 256 KiB is dropped by the broker, which disconnects Zigbee2MQTT:
   ```sh
   python3 infra/prod/setup.py <the same flags as the first install>
   docker compose --env-file .env.prod -f infra/prod/compose.yaml restart mqtt
   ```
3. `mqtt-provisioner` renders the new ACL lines (collector read on `aether/z2m/+/#`, per-model gateway rules) by itself within seconds of starting.

## Commands (phase 2, 2026-09-25)

Aether can set **any settable property of any device Zigbee2MQTT supports**, not only switch gangs: a gang (`state_l1`, `state_left`), a light (`state`, `brightness`, `color_temp`, `color`), a curtain (`position`), a lock (`state` LOCK/UNLOCK), a thermostat (`system_mode`, `occupied_heating_setpoint`), and so on. What a device accepts is read from its own definition: `bridge/devices` → `definition.exposes`, stored per device in `core.z2m_devices.exposes` (migration `00029`).

**Registering.** A Tuya TS001x switch registers as `tuya-ts001x-switch@1`. Any other supported device (one with a definition) registers as the generic `zigbee2mqtt-device@1` profile, which discovery suggests. Readings of non-switch devices are not decoded into Aether metrics yet (a later step); commands work regardless.

**Request.**
```
POST /api/v1/commands          Idempotency-Key: <uuid generated per click>
{"device_id":"…","property":"brightness","value":128}
{"device_id":"…","property":"state_l1","action":"toggle"}
→ 202 {"id":…,"status":"pending","value":128,…}
GET  /api/v1/commands/:id      GET /api/v1/commands?device_id=…      GET /api/v1/devices/:id/controls
```
`/devices/:id/controls` returns the device's settable features (flattened from its exposes), the last reported value of each, whether it is online, and `can_command` for this member. The UI renders controls from it:
- binary: ON/OFF buttons (a switch's gangs as one row);
- numeric: a slider and number field with min, max, step and unit;
- enum: a dropdown;
- colour composites: a colour picker mapped to `{hue,saturation}` or `{x,y}`.

Other composites are listed as not settable from the page yet.

**Validation** (`internal/adapters/zigbee2mqtt/features.go`):
- **The feature.** It must exist, found recursively inside `light` / `switch` / `cover` / `lock` / `climate` / `fan` including endpoint-suffixed properties, and must have `access & 2` (settable).
- **Binary.** The value must be `value_on`, `value_off` or `value_toggle`.
- **Numeric.** A JSON number within `value_min`..`value_max` and on `value_step`.
- **Enum.** One of `values`.
- **Composite.** Every settable part is given and valid, and nothing else is.
- **Refusals** are 400 with a reason: `unknown_property`, `not_settable`, `unsupported_feature` (text or list), `value`, or `not_toggleable`.
- **Toggle** is allowed on binary features only. It is resolved **server-side** into the explicit opposite of the last reported value (`z2m_devices.state`, updated from every state report). A toggle with no known value is refused as 409 `state_unknown`. Aether never publishes `TOGGLE`, so a duplicate delivery changes nothing.

**Lifecycle** (`core.device_commands`, a transactional outbox):
1. `pending`: the row is inserted in the member's own transaction under RLS (project scope) with `expires_at = now + 10 s`, and the action is audited (`device.command`).
2. `sent`: `mqtt-commander` claims it (`FOR UPDATE SKIP LOCKED`), **commits**, and then publishes `{"<property>": <value>}` to `aether/z2m/<gateway>/<ieee>/set` (QoS 1, not retained). A crash between the two loses the publish and never repeats it. `sent_at` is set when the broker accepted the publish. Publishing is paced per gateway at 5/s with bursts of 10: the commander claims from a gateway only what its bucket allows at that moment and never waits. The rest stays pending for the next sweep and still expires on time, so one busy gateway delays no other.
3. `confirmed`: the collector, in the same transaction as the device's next state report that carries the property with the desired value, marks it confirmed. Numbers match within `max(value_step, 1 % of the range)`; composites compare the parts that were sent. Switch events caused by it carry `source: "command"` and `command_id`; changes on the wall stay `source: "external"`.
4. `timeout` if no such report arrives within 10 s. A matching report within 60 s after that still confirms it (the row notes "confirmed after the timeout"). That does not happen if the property was reported with another value in between, for example a press on the wall: the command is then `superseded` and stays `timeout`. A late confirmation never names itself in the switch event, which stays `external`. `expired` if it was never claimed before `expires_at` (never published). `failed` if the broker refused the publish.

**Refused before queueing** (409 with a reason):
- `offline`: availability says offline, the bridge is offline, or the stream is offline
- `in_flight`: a command for the same device and property is still pending or sent (also enforced by a partial unique index)
- `not_paired`: the device left `bridge/devices`
- `gateway_unavailable`: the gateway is revoked or is not a Zigbee2MQTT gateway
- `idempotency_key_reused`: the same key was sent with a different request

Also refused:
- 429 `rate_limited`: the workspace has queued 60 commands in the last minute, or the member has sent 20 in the last minute. The workspace count is taken across all projects (`core.command_count_since`, a definer function), so members of different projects share one budget.
- 404: the device is outside the member's projects

**Who may command:**
- Owner, admin and operator may, within their projects. Viewers never may.
- The access module **`control`** lets an owner restrict a member to read-only (`/members/:id/access {"control":"read"}`).
- The rule is checked twice: by the HTTP access gate and again inside the command service, so it holds whatever route reaches the service.
- The access gate normalises the path it classifies (case, duplicate and trailing slashes), and the router is case-sensitive. `/API/v1/Commands` cannot slip past a closed module; this applies to every module.
- `GET /devices/:id/controls` reports `can_command` accordingly, and the UI hides the buttons.
- A plain set never accepts `value_toggle`; a toggle must be requested as `action: "toggle"`.

**Services and ACL:**
- `mqtt-commander` is its own container.
- Its broker account `aether-commander` has exactly `topic write aether/z2m/+/+/set`. It has no read and no subscription, and cannot publish `bridge/request/*`, so it can never permit joins or remove devices.
- It connects with a clean session. It wakes on `NOTIFY aether_command` and polls every 2 s as a fallback.

## Not in this phase

- Automation actions (phase 3).
- Generic ingest of every exposed property into Aether metrics and events, and importing the full device catalog; the stored `exposes` is the base for both.

## Testing without hardware

- Commands: the simulator's bridge (`simulation.FakeBridge`) also contains a Philips colour bulb, a Tuya TS130F curtain, a Yale lock and a Tuya thermostat with realistic exposes, subscribes to its own `…/+/set`, applies each command and publishes the new state. `SIMULATOR_DROP_COMMANDS=true` makes it ignore commands (watch them time out). Tests: `internal/adapters/zigbee2mqtt/features_test.go` (validation, toggle, tolerance), `internal/commander` (at-most-once dispatch, pacing), `tests/commands_test.go` (the full loop through the FakeBridge, lifecycle, permissions).

- `SIMULATOR_KIT=z2m`, with the simulator's `topic` set to the gateway's base topic, publishes a virtual bridge: TS0011, TS0012 and TS0014 plus one unsupported device. Gang 1 of the first switch is "pressed" every 20 steps.
- `go run ./cmd/simulator sample-z2m <step> [base_topic]` prints the messages of one step.
- Tests:
  - `internal/adapters/zigbee2mqtt` (unit tests and fuzzing, with a realistic fixture in `testdata/`)
  - `internal/alerts/switch_test.go`
  - `cmd/mqtt-provisioner/render_test.go`
  - `internal/simulation/z2m_test.go`
  - `tests/z2m_test.go` (enrolment, discovery, radio check, samples and events, live view, availability, offline scan, removal, revocation, project scope)
