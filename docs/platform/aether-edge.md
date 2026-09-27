# Aether Edge: Tuya Wi‑Fi devices, locally

Status: phases A–C (protocol library, ingest, commands) are implemented. Key import from the Tuya cloud, the
agent's configuration endpoint and installer (phase D), the agent binary (phase E) and the UI (phase F) follow.
Nothing here has been verified with a real Tuya device yet; the gateway model and profile are `Verified: false`.

## What it is

Tuya Wi‑Fi devices (plugs, wall switches, bulbs, curtain motors, thermostats) speak a local protocol on TCP
6668, encrypted with a per-device **local key**, besides talking to the Tuya cloud. Aether does not use the cloud
at runtime: an **Aether Edge** agent runs on a site host (the same Raspberry Pi as Zigbee2MQTT), keeps one TCP
connection per registered device (`backend/internal/tuyalocal`, protocol 3.1/3.3/3.4/3.5, ported from tinytuya,
MIT) and connects OUT to Aether's broker over TLS 8883 with its gateway's own account.

The agent moves **raw data points only** (`{"dps":{"1":true,"19":1234}}`). What a data point means is known on
the server alone, from the device's specification imported once from the Tuya cloud together with its local key.

- Gateway model: `aether-edge` (`domain.EdgeGatewayModel`).
- Device profile: `tuya-wifi-device@1` (`domain.TuyaWiFiProfile`, radio `tuya-wifi`, actuator). Tuya profiles
  register only under an Edge, and an Edge takes nothing else (`domain.ProfileAllowedOn`).
- Device id: the Tuya device id, lower case, 16–32 letters and digits.

## Topics

Everything lives under `aether/edge/<gateway id>/`:

| Topic | Direction | Payload |
|---|---|---|
| `status` | agent → Aether, retained, also the MQTT last will | `{"state":"online"}` / `{"state":"offline"}` |
| `health` | agent → Aether, every 60 s | `{"version":"0.1.0","devices_connected":3,"lan_seen":7}` |
| `discovery` | agent → Aether | `[{"id":"<tuya id>","ip":"192.168.1.20","version":"3.4","product_key":"…"}]` (max 500) |
| `<device>/state` | agent → Aether | `{"dps":{"1":true,"19":1234},"full":false}` |
| `<device>/availability` | agent → Aether, retained | `{"state":"offline","reason":"auth_failed"}` |
| `<device>/set` | mqtt-commander → agent | `{"dps":{"1":true}}` |

Availability reasons: `unreachable`, `auth_failed` (3.4/3.5 negotiation proved the key wrong — definitive),
`key_suspect` (3.1/3.3 replies do not decrypt — a heuristic), `busy` (the device resets our socket: another local
client such as Home Assistant/LocalTuya holds its only connection), `not_found`.

The collector (`cmd/mqtt`) routes the tree with `adapters/edge.Route`; `…/set` and anything unknown is
acknowledged and dropped. `service.CaptureEdge` refuses empty, oversized (16 KiB; discovery 256 KiB), non-UTF-8
and NUL-bearing messages as permanently invalid, so a poison message never stalls the collector.

## Broker ACL (rendered by `mqtt-provisioner`)

| Account | Access | Topics |
|---|---|---|
| `gw-<id>` (Aether Edge) | write | `aether/edge/<id>/#` |
| | read | `aether/edge/<id>/+/set` |
| `aether-ingest` | read | `aether/edge/+/#` (plus the Minew and Zigbee2MQTT trees) |
| `aether-commander` | write | `aether/edge/+/+/set` (plus `aether/z2m/+/+/set`) |

An agent cannot read another gateway's commands, and cannot read anything but its own devices' `/set` topics.

## Storage (migration 00032)

- `core.edge_agents`: agent state (online/offline from its last will), version, last heartbeat, and
  `config_revision`, bumped whenever what the agent must connect to changes: a device registered, moved, removed
  or restored under the Edge, a key import, or a device's LAN address or protocol version changing.
- `core.edge_lan_devices`: devices the agent saw broadcasting (id, IP, version), forgotten after a week unseen.
- `core.tuya_devices`: imported devices — the normalised specification (`spec`), its translation (`exposes`,
  `dp_map`, `gangs`, `category`), `local_capable`, the local key **sealed** (`local_key_sealed`, never returned by
  any API; only `key_fingerprint`, a short hash prefix), `key_status` (`ok`/`rejected`/`suspect`/`missing`), the
  last value of each settable property (`state`, for toggles), availability and the agent's reason.
- `core.command_targets` (view, `security_invoker`): Zigbee2MQTT and Tuya devices behind one shape, so the command
  path, device controls and automation targets do not care which transport carries a command.
- `core.edge_install_codes` + `core.redeem_edge_install_code()`: single-use install codes, hash only (phase D uses
  them).

All tables have the tenant policy plus the RESTRICTIVE project scope keyed by gateway, like Zigbee2MQTT.

## From data points to Aether (`adapters/tuya`)

The specification (`GET /v2.0/cloud/thing/{id}/model` or `GET /v1.1/devices/{id}/specifications`) is normalised
to `[]tuya.DP` (id, code, type, access, raw min/max/step, scale, unit, enum range) and translated into the exposes
shape Zigbee2MQTT uses (`zigbee2mqtt.Feature`). `Validate`, `Toggle`, `MatchesIn`, `SettableValuesIn` and the
generic ingest `ParseStateWith` then work on Tuya devices unchanged.

| Tuya | Aether |
|---|---|
| `bool`, writable (or a `switch`/`switch_n` output) | binary, `"ON"`/`"OFF"` (wire: `true`/`false`) |
| `bool`, read-only | binary, `true`/`false` |
| `value` (raw min/max/step, `scale`) | numeric; real = raw / 10^scale, bounds likewise |
| `enum` | enum (values = range) |
| read-only enum flags `pir_state`, `presence_state`, `watersensor_state`, `smoke_sensor_status`, `gas_sensor_status`, `co_state` | binary flag (on = `pir` / `presence` / `alarm`) |
| `string` | text (read-only in Aether) |
| `bitmap` | numeric, read-only |
| `raw`, `json` (`colour_data`, schedules) | not translated |

Canonical renames: `cur_power`→`power` (W), `cur_voltage`→`voltage` (V), `cur_current`→`current` (mA → A,
read-only), `add_ele`→`energy`, `va_temperature`/`temp_current`→`temperature`, `va_humidity`/`humidity_value`→
`humidity`, `switch_led`→`state`, `bright_value(_v2)`→`brightness` (the `_v2` one wins), `temp_value(_v2)`→
`color_temp`, `percent_control`→`position`, `doorcontact_state`→`door` (true = open), `pir_state`/`presence_state`
→`motion`, `watersensor_state`→`leak`, `smoke_sensor_status`→`smoke`, `gas_sensor_status`→`gas`, `co_state`→
`carbon_monoxide`, `battery_percentage`→`battery`. Everything else keeps its Tuya code.

Gangs: `switch` (gang 1) and `switch_1`…`switch_4` become `sw1`…`sw4`, so switch_on/switch_off events work as for
Zigbee wall switches. Category comes from the Tuya category code (`kg`/`cz`/`pc` switch, `dj`/`dd`/`fwd`/`tgq`
lighting, `cl`/`clkg` cover, `wk`/`kt`/`qn` climate, `fs` fan, `wsdcg` environment, `mcs` door, `pir`/`hps`
occupancy, `ywbj`/`rqbj`/`cobj` hazard, `sj` leak, `sos`, `ms` lock, `zndb` metering), else from the exposes.

`local_capable` is false for categories that are almost always battery powered (`wsdcg`, `mcs`, `pir`, `ywbj`,
`sj`, `sos`, `ms`, `jtmspro`, `cobj`) and for Zigbee/BLE sub-devices of a Tuya hub: they sleep and never answer
locally. Discovery still lists them, without a profile.

Stored samples carry the frames `z2m-state@1,tuya-local@1`: the generic, definition-backed reading plus its source.

## Liveness

A Tuya device's stream has `liveness='reported'`: offline/online come from the agent's availability messages,
never from silence (a plug only reports when something changes). The agent itself is watched like a Zigbee2MQTT
bridge: its last will takes every reported device offline (`source: edge_offline`); any later message brings back
those whose own last availability was not offline; and an agent silent for `EdgeSilentAfter` (3 minutes, health is
every 60 s) is treated the same (`source: edge_silent`) by the offline scan. `auth_failed` sets `key_status =
rejected` and the offline event carries `source: key_rejected`, so the UI can ask for a new import rather than show
an outage; the next successful connection sets it back to `ok`.

## Commands

The command path is the one Zigbee2MQTT devices use (`POST /api/v1/commands`, docs/platform/zigbee2mqtt.md), with
the device read from `core.command_targets`:

1. the value is validated against the translated exposes (range, step, enum, binary; toggles resolved server-side);
2. a device whose key is not `ok` is refused (`409 key_unavailable`);
3. the value is encoded to the device's data point (`tuya.Wire`: `"ON"`→`true`, real→raw with the scale, raw bounds
   and step checked again) and stored with the command as `wire`, `transport='edge'`;
4. mqtt-commander publishes `wire` to `aether/edge/<gateway>/<device>/set` (it never knows what a data point means
   and never holds a key);
5. the agent sets the data point; the device's next report, converted back through the dp map, confirms the
   command exactly like a Zigbee report does.

Automation "สั่งอุปกรณ์" blocks target Tuya devices the same way.

## Key import, install codes and the agent's configuration (phase D)

**Import** (`POST /api/v1/gateways/:id/tuya/imports`, owner/admin; steps in Thai: [tuya-local.md](tuya-local.md)):
- The user's own Tuya IoT project, one region from a fixed list (`us, us-e, eu, eu-w, cn, in, sg`). The host comes
  only from that list, so there is no SSRF, and redirects are not followed.
- `internal/adapters/tuyacloud` signs each call with Tuya's HMAC-SHA256 scheme; the reference signatures in its test
  were produced by tinytuya. Calls, in order:
  - the token;
  - the device list with local keys, from `/v1.0/iot-01/associated-users/devices`, falling back to
    `/v1.3/iot-03/devices` and `/v1.0/users/{uid}/devices`;
  - one data-point model per product, from `/v2.0/cloud/thing/{id}/model`, falling back to
    `/v1.1/devices/{id}/specifications`.
- Bounds: 10 s per call, 90 s per import, at most 200 devices.
- The job runs in the API process (`app/tuya_import.go`) and is polled at `GET …/tuya/imports/:job`. It lives 10
  minutes, allows one running import per gateway and 10 jobs per workspace, and is lost on restart.
- The **Access ID/Secret exist only inside the job's goroutine**: they are never in the job record, the database, a
  log or a response.
- Each local key is sealed at once with `DeriveKey(CHANNEL_SEAL_KEY or the JWT key, "tuya-local-keys")` and stored
  with a 16-hex fingerprint, through `SaveTuyaDevices` (`tuya.Validate` applies). The result lists fingerprints, never
  keys. Audited as `tuya.keys_imported`.
- `GET …/tuya/devices` shows key status and fingerprint, LAN and registration state.
  `POST …/tuya/devices/:tuya_id/forget` drops a key, moves the configuration revision and is audited.

**Install code** (`POST /api/v1/gateways/:id/edge/install-code`, owner/admin):
- 128 random bits, written as 26 base32 characters; only its SHA-256 is stored.
- Valid 30 minutes and single use. A newer code voids older unused ones.
- Returns `install_command` (`curl -fsSL <origin>/edge/install.sh | sh -s -- <code>`). **`install.sh` ships with the
  Edge image in phase E**; until then the command does not work, and the bootstrap call below can be made by hand.

**Bootstrap** (`POST /edge/bootstrap`, body `{"code":"…"}`, at most 256 bytes):
- **The code travels in the body, never the path**: proxies and CDNs log paths, and a code that failed before it
  was spent (a 503, a 429) must not sit valid in an access log for up to 30 minutes.
- Public and limited to 10 requests per minute per address. Caddy routes exactly `/edge/bootstrap` to the API. The
  per-address limit only sees real clients when the outer proxy is trusted (`setup.py --upstream-proxy`).
- The broker endpoint and CA are checked **before** the code is spent.
- `core.redeem_edge_install_code` marks the code used. In the same transaction, the gateway's HTTP token and MQTT
  password are rotated. Audited as `edge.bootstrapped`, with the code's creator as actor.
- Returns once:
  - the gateway id and API origin;
  - `http.token` and `http.config_url`;
  - `mqtt` (url, host, port, username, client id, password, base topic);
  - `ca_pem`, the broker CA from `MQTT_CA_FILE`;
  - optionally `web_ca_pem`, from `EDGE_WEB_CA_FILE`;
  - the image reference (tag pinned in phase E);
  - `notice: previous_agent_must_stop`.
- Only X.509 `CERTIFICATE` blocks from those files are returned, re-encoded. A file that also holds a private key
  never passes the key on.
- Any failure is 401.

**What rotation does to a connected agent.** Mosquitto re-reads the password file on SIGHUP (the provisioner
applies a changed hash within about 3 s) but **keeps sessions that are already authenticated**. After a bootstrap:
- the old HTTP token stops working at once;
- an agent still connected with the old MQTT password is disconnected when the new agent connects **with the same
  client id** (`gw-<id>`, MQTT session takeover). If it drops, it cannot reconnect: the old password is refused;
- until then its session lasts. It stays confined to its own gateway's topics by the ACL.

So stop the previous agent before, or right after, reinstalling. If an old credential may be compromised and no new
agent is connecting yet, restart the broker to drop every session:
`docker compose --env-file .env.prod -f infra/prod/compose.yaml restart mqtt`. Gateways reconnect on their own, so
there is a few seconds' gap in data. Mosquitto has no per-client disconnect without its dynamic-security plugin,
which this deployment does not use.

**Agent configuration** (`GET /ingest/gateways/:id/edge/config`, the gateway's HTTP Basic credential):
- Returns `{revision, devices:[{id, key, version, ip, dev22, refresh_dps}]}`. Only devices that are registered, have
  an imported key and can be reached locally are included.
- This is the **only** place a local key is opened.
- `ETag` is the configuration revision; `If-None-Match` gives 304. The agent keeps keys in memory only.

## Not yet (later phases)

- Phase E: the `aether-edge` binary, its image and `install.sh`; phase F: the UI (installer card, import wizard, key
  status).
- Manual key entry and open firmware (OpenBeken/ESPHome) are not in this round.
- A Tuya SOS button is not wired to the SOS path: SOS buttons are battery devices and cannot be reached locally.
