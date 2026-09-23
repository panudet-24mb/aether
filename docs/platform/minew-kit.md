# Minew MHS starter kit — decoders, simulation and hardware verification

Implemented locally 2026-09-20. This extends the single S1 decoder to the whole kit so the platform can be developed and demonstrated without hardware, while keeping an honest line between "decoded from public documentation" and "verified against a captured packet".

## Kit contents (manufacturer listing)

| Role | Model | Frames the platform decodes | Verified on hardware |
|---|---|---|---|
| Gateway ×2 | MG3 | JSON‑LONG rows `mac, rawData, rssi, timestamp` over MQTT | yes (2026‑09‑11) |
| Temperature/humidity | S1 | FFE1 A1‑01, A1‑08 (info/name) | A1‑01 yes |
| Card beacon | C10 | iBeacon, Eddystone, A1‑03 accelerometer, A1‑08 | no |
| Button wristband | B7 | iBeacon, Eddystone, A1‑03, A1‑08 | no; button encoding undocumented |
| Emergency button | B10 | Eddystone UID/TLM (press = UID instance change per public config guide), A1‑08 | no |
| Accelerometer asset tag | E8S | A1‑03, A1‑18 vibration, A1‑08 | no |
| Anti‑tamper tag | MBT01 | iBeacon, A1‑20 tamper (inferred), A1‑08 | no |

Sources: Minew product/launch pages for the kit list; frame byte layouts from the MIT‑licensed reelyActive `advlib-ble-services` Minew decoder (re‑implemented from the byte table, not copied) and the Apple iBeacon / Google Eddystone specifications. The decoder also understands A1‑02 light, A1‑05 illuminance, A1‑11 PIR, A1‑12 TVOC, A1‑13 temperature‑only and A1‑21 leak frames from the same table.

## What changed

- `backend/internal/adapters/minew/frames.go`: `DecodeFrames` parses every AD structure of an advertisement, merges recognised frames into one `Reading` with `kind` (environment, motion, tamper, leak, light, beacon, info), `model` (name from the info frame), `frames` (decoder ids), `metrics` and `beacon` identity. The original `Decode` contract (A1‑01 only) is unchanged and its hardware golden test still passes.
- Live projection merges a tag's sensor and beacon slots seen in the same uplink into one sample per device; stored samples record the decoder ids in `sensor_samples.decoder_id`. Older samples without `kind` are treated as environment readings.
- Auto‑generated stream names are upgraded once a tag reports its model in an info frame; user‑assigned names are never overwritten.
- `GET /api/v1/catalog` is the single source of gateway models and device profiles (`backend/internal/domain/catalog.go`); `POST /gateways` and `POST /devices` validate against it, and the topology UI loads it instead of a local copy. `verified` is true only for the S1 decoder.
- Simulator (`backend/internal/simulation`) publishes the whole kit through the normal MQTT → storage → decoder → UI path: four S1‑style sensors plus C10, B7, B10, E8S and MBT01 with deterministic scenarios (E8S moves 10 of every 30 steps, MBT01 raises tamper for 6 of every 40 steps, B10 "presses" for 2 of every 15 steps). All rows carry `aether_source: simulated`.
- Topology page: device nodes show an icon and one‑line summary per kind, an EVENT badge for tamper / button / leak, and the inspector shows kind‑specific readings, beacon identifiers, decoded frame ids, derived events and whether the suggested/registered profile is hardware‑verified. Adopting a device pre‑selects the profile from the info‑frame name or the reading kind. Live monitoring keeps showing environment sensors only.

## Robustness against real firmware (2026‑09‑20)

Real firmware revisions differ from the PDFs, so the decoder no longer trusts the documented byte
count. Changes in `frames.go`:

- AD structures are walked defensively (length byte, bounds check, zero-length terminator, truncated
  tail) and an unknown or malformed structure is **skipped**, never fatal to the others in the same
  advertisement. Before: one unparseable structure discarded the whole advertisement silently.
- A known FFE1 0xA1 frame version decodes at **at least** the documented length; trailing bytes are
  tolerated and the little-endian MAC is located from the end of the payload, then cross-checked
  against the advertiser address the gateway reported. A mismatch is flagged, not fatal.
- Out-of-range values are dropped **per field** (temperature −60…+120 °C, humidity 0…100, battery
  0…100, |accel| ≤ 16 g); the rest of the frame still decodes. Two deliberate exceptions protect the
  alert path: an A1‑01 frame whose temperature is out of range is dropped entirely (consumers read
  `kind=environment` as "temperature is valid"), and the tamper/leak flag is not emitted when the
  frame's battery byte is implausible, because that means the layout is probably not the one we
  think. An A1‑01 frame with a bad humidity keeps its temperature but is reported under the decoder
  id `minew-ffe1-a101@1#temp`, so nothing downstream reads the resulting zero as 0 %RH.
- `Reading.Unknown` (JSON `unknown`, `omitempty`) lists up to 8 descriptors of ≤40 chars for what was
  seen but not understood, e.g. `ffe1:a1:0x22:len=14`, `ad:0x16:uuid=feaa:type=0x30`,
  `ffe1:a1:0x01:mac-mismatch`, so an operator can see that a tag is talking and Aether has no rule.
- `FuzzDecodeFrames` and `FuzzProjectPayload` (Go fuzzing) assert nothing panics on arbitrary input.

`infra/capture-golden.py` turns what a gateway really sent into a regression test:
`list` shows the MACs in the recent raw packets with counts, RSSI range and what Aether decoded;
`capture --scenario b10` walks the operator through labelled steps and writes
`backend/internal/adapters/minew/testdata/real/<label>.json`, which `TestRealCaptures` decodes. MACs
are pseudonymised by default in both the `mac` field and the frame bytes, so the file still decodes.
Bench procedure: [hardware bring-up](../hardware-bringup.md).

## Verifying with the real devices

1. Power one tag at a time next to the MG3 and read `GET /api/v1/gateways/{id}/packets` (owner/admin). Copy the `rawData` of the tag's rows; they must contain the tag's own MAC in the trailing bytes of each FFE1 frame.
2. Add the captured hex as a golden test in `backend/internal/adapters/minew/frames_test.go` with the values shown by the tag/app, then set `Verified: true` on that profile in `catalog.go`.
3. For B7/B10 press the button while capturing and compare the frames before/after. **This comparison is now automated** — see "Teaching a signal" below; use it instead of doing the diff by hand.
4. Do not enable alerting or emergency workflows on tamper/button events until step 2 has passed for that device.

## Teaching a signal ("สอนสัญญาณให้ระบบ")

Added 2026-09-22, because the table above is not good enough for the hardware that is actually connected.

**The problem.** The physical B10 (`c300007b573c`) advertises only `minew-ffe1-a103@1` (accelerometer),
`minew-ffe1-a108@1` (info), `eddystone-tlm@1` and briefly `ibeacon@1`. It has never advertised
Eddystone-UID. Aether infers a button press from an Eddystone-UID *instance change*
(`backend/internal/alerts/engine.go`, `EventButton`), so for this device the press can never fire —
`core.stream_state.instance` for that MAC is empty. The same gap applies to C10 `c3000059760f`,
B7 `c30000764cea`, MSP01 `c30000161ebb`, "PLUS" `c30000393fe5`, C6 `c300006b4450` and
E8 `c30000701374`: their press/tamper encodings are equally undocumented. No `unknown` frames were
recorded in the captured windows, so either the operator did not press during them, the MG3 scan
filter dropped the frame, or the tag must be configured in the Minew app to broadcast on press.

Guessing the encoding is not acceptable. Instead the operator teaches it, from the device inspector
in "เชื่อมต่ออุปกรณ์", in under a minute:

1. Phase 1 "ปล่อยนิ่ง" (~20 s): the device is recorded at rest.
2. Phase 2 "กดปุ่มย้ำ ๆ" (~20 s): the operator triggers it repeatedly. Both phases show a live count
   of the advertisements that actually reached the gateway, so "nothing was learned" can always be
   told apart from "nothing arrived".
3. The server diffs the two windows out of `core.ble_history`, which already archives every distinct
   raw advertisement per packet (retention `BLE_HISTORY_HOURS`, default 24, so a session is never
   pruned mid-way).
4. Candidates are ranked and each is explained in Thai, e.g. *"เฟรม Minew FFE1 A1-22 (version 0x22)
   ปรากฏเฉพาะตอนกด · เห็น 7 ครั้ง"*.
5. The operator confirms one. From then on ingest raises that event type for the device,
   edge-triggered exactly like the existing tamper/leak logic.

**Matcher shapes**, narrowest structural explanation first (`backend/internal/signals`):

| Kind | Means | Example |
|---|---|---|
| `frame` | an AD structure / Minew frame version that only appears while triggered | `ffe1:a1:0x22` |
| `byte` | a byte at a fixed offset inside an identified frame, under a mask | `ffe1:a1:0x20` offset 3, mask `0xff`, value `0x01` |
| `prefix` | an exact raw-payload prefix | `0e16e1ffa122…` |

**What keeps a guess from becoming a signature.** Before diffing, each advertisement is split into
its AD structures the way `frames.go` walks them, and the fields that move on their own are excluded:
the trailing MAC, the battery byte, accelerometer axes and the Eddystone-TLM voltage/temperature/
`adv_count`/uptime, *plus* — empirically — any byte that varied at all during the baseline. A
candidate that matches even one baseline advertisement is rejected. A `prefix` candidate is only ever
offered for an advertisement that could not be split into AD structures at all: if a frame parsed and
none of its bytes survived those checks, the device did not say anything new, and dressing the same
bytes up as a raw prefix would turn a rejected guess into a signature.

If nothing distinguishes the phases, the wizard says so and what to check (the tag may need
configuring in the Minew app to broadcast on press; the MG3 scan filter may drop it; the press may be
shorter than the upload interval). It never invents a signature.

**Scope.** A signature applies to one BLE identity by default, or — with "ใช้กับทุกตัวที่เป็นรุ่นนี้"
— to the device profile, so one B10 teaches every B10.

**Checking for false positives.** `POST /api/v1/signals/{id}/test` replays the last ten minutes of
that identity's raw history through the matcher and reports how many uplinks would have fired. A
signature that matches most uplinks is measuring something that is always true, not a press.

**Where it lives.** Analysis (pure, no database): `backend/internal/signals/`. Persistence and the
ingest match: `backend/internal/adapters/postgres/signals.go`. HTTP: `httpapi/signals.go`. Schema:
`backend/migrations/00025_device_signals.sql` (`core.signal_sessions`, `core.device_signals`, and a
`signals` jsonb column on `core.stream_state` that holds the edge state beside the built-in
tamper/leak/moving flags rather than in a parallel mechanism). UI: `frontend/app/topology/signals.tsx`.

**Testing without hardware.** `backend/internal/simulation` carries a second, realistic B10
(`f0000000000a`) that behaves like the physical one: accelerometer, info and Eddystone-TLM at rest,
and while pressed exactly one extra advertisement — a Minew FFE1 0xA1 frame of a version the decoder
does not know (0x22), so it surfaces as an unknown frame just as a real undocumented press frame
would. The original simulated B10 (`f00000000007`) keeps its Eddystone-UID behaviour.
`backend/tests/signals_test.go` drives the whole loop against it.

## Limits

- Kinds and events are derived per uplink and persisted by the [alert pipeline](alerts.md), which also supports notification channels. Hardware verification of button/tamper encodings remains required for the *decoders*; a taught signature is an alternative that needs no decoder, and is marked `verified: false` until a captured packet backs it.
- A learned signal of type `custom` only lands in the event log: no alert rule can subscribe to it.
- Templates/thresholds still apply only to the environment decoder (`device_templates.decoder_id` check constraint).
- Dashboard Studio supports bundled raw-byte decoders and official kit widgets for motion, tamper, buttons and beacons, plus event and alert displays. See [Widget Studio](widget-studio.md).


## B10 SOS press: the iBeacon trigger slot (2026-09-23)

The physical B10 `c300007b573c` was recorded for four minutes on the production MG3 while the operator pressed the button three times. The A1-03 accelerometer, A1-08 info and Eddystone-TLM slots advertise all the time. **The iBeacon slot, and the Minew FFF1 frame beside it, advertise only after a press.** At rest there was no iBeacon for almost three minutes. From the press at 10:50:18 (a 1.66 g jolt on the accelerometer) a dense iBeacon burst ran for over a minute, with gaps of a few seconds where the gateway missed packets.

Aether detects this without teaching. A device registered with a button profile raises `button` when its iBeacon slot reappears after at least `alerts.ButtonTriggerQuiet` (30 s) of silence. That is one SOS per burst: a second press inside a running burst is the same emergency. The last time the slot was heard is `core.stream_state.trigger_at` (migration 00027). The Eddystone-UID instance rule still applies to tags that have a UID slot. Tests: `TestButtonTriggerSlot` (engine) and `TestB10PressFromTriggerSlot` (real frames, event and critical alert).

Still open: whether shaking the B10 without pressing also starts the burst (a motion trigger configured in the Minew app). Check once by moving the tag around without pressing. If it does, disable the motion trigger for that slot in the Minew app.
