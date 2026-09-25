# Automation Studio v1

Added 2026-09-20. "ถ้า…แล้ว…" flows built from blocks on a canvas. A flow reacts to device events or sensor values, checks conditions across any devices of the workspace, and acts.

## What runs for real

- Open an alert, or send through a notification channel (webhook, LINE, email). These use the same alert and notification pipeline as alert rules.
- **"สั่งอุปกรณ์" (action.command, since 2026-09-25):** set one property of a registered Zigbee2MQTT device to an explicit value (turn a switch gang ON, a bulb's brightness to 200, a curtain to 50 %, a thermostat mode). See "Device commands" below; it is off unless the deployment switches it on.

## Device commands (action.command)

A command block names a registered device (its registration id), one property its Zigbee2MQTT definition lets Aether set, and the value. There is **no toggle** in a flow: a flow says which state it wants, so firing twice can never flip a relay back.

A firing flow does not queue the command itself. It writes a **request** to an outbox (`core.automation_command_requests`, migration 00031), and `mqtt-commander` turns requests into commands on its next sweep, through **the same queue as a click in the web UI** (`queueCommand`, `docs/platform/zigbee2mqtt.md`). That queue validates against the device's own exposes, allows one command in flight per device and property, applies the workspace budget (60 a minute), delivers at most once, and confirms only when the device reports the value. The command row has `source='automation'`, `automation_id` = the flow, `actor_id` = the member who last switched the flow on (`core.automations.enabled_by`), and the request's id as its own id. The run log shows `requested` until the commander has decided, then `queued` or `blocked` with a reason.

**Why an outbox (lock order).** Every path that queues a command takes the per-tenant command lock first and only then touches gateway rows (the command's foreign keys lock the target's gateway). Ingest already holds its gateway row `FOR UPDATE` when a flow fires, so queueing from ingest took the two in the opposite order: a deadlock with a web command, or with a flow on another gateway commanding this one. Ingest now takes no command lock and no foreign-key lock at all; it only inserts the request (no foreign keys). The commander drains requests with the tenant lock first, like the web. `TestAutomationCommandLockOrderUnderConcurrency` runs ingest, web commands and the drain together, and it deadlocks against the old order.

**Who may arm it.** Two conditions, checked by the route (per-block problems for the studio) and again by the repository:

1. The deployment switch `AUTOMATION_COMMANDS=true` (setup.py `--automation-commands true`, passed to `api` and `mqtt-ingest`). Default **false**: a flow with a command block can be drawn and saved as a draft, but enabling it is refused with `command_disabled`. Switching it off later makes the runtime refuse to queue (run log reason `commands_disabled`); the flows stay enabled for their other actions.
2. The member switching the flow on must be allowed to control devices themselves: the same rule as a manual command (owner, admin or operator, and the "control" module not set to none/read), `command_forbidden` otherwise. Managing flows at all is still owner/admin only, so in practice this is an owner, or an admin whose control module is open. Enabling a flow with commands is audited as `automation.commands_armed`.

**The armer's authority is re-checked** (`core.member_may_command`, the same rule as a manual command: owner, admin or operator; the control module not none/read; project scope covering the target's gateway). It is checked when the flow fires, and again when the commander queues the command (reason `arming_member_lacks_control`). The team page also acts on it at once. Demoting or removing the armer, closing their control module, restricting their projects, or moving a target's gateway out of their projects **switches the flow off** (audited `automation.disarmed`), so the studio shows it off. It has to be switched on again by someone who may command.

**What it may target.** A registered Zigbee2MQTT actuator on a live gateway, in the flow's project for a project flow (the workspace for an unscoped one). Checked when the flow is enabled, when it fires (`checkFlowProject`), and when the commander queues the command (reason `out_of_project`). `GET /api/v1/automations/commandable?project_id=` lists the candidates with their settable features for the studio's pickers.

**Safety rules at run time** (each refusal is recorded in the run's `detail.commands[]` with a reason, never an error for the other actions):

| Rule | Reason in the run log |
|---|---|
| `AUTOMATION_COMMANDS` is off | `commands_disabled` |
| The member who armed the flow may no longer command the target | `arming_member_lacks_control` |
| The target left the flow's project | `out_of_project` |
| **Loop guard:** the change that woke the flow was caused by a command an automation sent. This covers any property (a switch gang, brightness, position, …) and event or metric triggers alike: the commands named by the matched events plus every command the device's report in that packet confirmed | `loop_guard` |
| All automations together may send one device at most **6 commands a minute** (`domain.AutomationCommandsPerDevice`) | `automation_cap` |
| All automations together may use at most **30 of the workspace's 60 commands a minute** (`domain.AutomationCommandsPerMinute`), so manual control always keeps at least 30 | `automation_budget` |
| The request was not processed within the command lifetime (10 s) | `expired` |
| The queue refuses it (device offline, a command already in flight for that property, workspace budget, value refused, device unpaired) | `offline`, `in_flight`, `rate_limited`, `value`, `not_paired`, … |

The two automation caps are counted under the tenant command lock, atomically with the insert.

Plus the existing per-flow de-duplication: one firing per flow per identity per minute.

The loop guard is what stops two flows ping-ponging a relay: flow A turns a light off when it goes on, flow B turns it on when it goes off. A wall press wakes A; A's command, once the switch confirms it, produces a `switch_off` event naming that command; B is woken by it but sends nothing. The same holds for metric triggers (A: brightness above 150 sets it to 50, B: below 100 sets it to 200).

**Dry run** ("ทดสอบ") never queues anything. Its response lists `would_send`: the commands the flow would have sent.

**Shadow mode.** With `ALERTS_SHADOW=true` automations do not run at all, commands included (only SOS and hazard alerts bypass shadow). The studio says so in a banner. Arming device commands in production therefore needs **both** `ALERTS_SHADOW=false` and `AUTOMATION_COMMANDS=true`, each set deliberately.

**New triggers** for Zigbee: `switch_on`, `switch_off`, `hazard`, `hazard_cleared` and `action` (a remote or button press). A trigger listening to `action` may list the action values it accepts (`single`, `double`, `on`, …); empty means any press. The filter only narrows `action` events; other event types in the same block are unaffected.

## Blocks

| Group | Block | Settings |
| --- | --- | --- |
| เมื่อ | เหตุการณ์จากอุปกรณ์ | event types, devices, gateways (empty means any) |
| เมื่อ | ค่าจากเซนเซอร์ | device, metric, operator, value, "for N seconds". Fires on the rising edge only. |
| ถ้า | ช่วงเวลา | from, to, weekdays, Asia/Bangkok; windows may cross midnight |
| ถ้า | ค่าของอุปกรณ์อื่น | newest reading of any device, across gateways, not older than `max_age_sec` |
| ถ้า | โซนของ wearable | the wearable's stable zone is one of the chosen gateways |
| ตรรกะ | ทั้งหมด (AND), อย่างน้อยหนึ่ง (OR) | |
| ทำ | เปิดการแจ้งเตือน | severity, title with `{{device}}`, `{{value}}`, `{{event}}` |
| ทำ | ส่งข้อความออก | channels, message. Notifications hang off an alert, so a flow that only notifies opens one `info` alert to carry the message. If an alert block ran first, the message becomes that alert's note and is included in the notification text. |

Condition blocks have two outputs, "ใช่" and "ไม่ใช่". Limits: 40 blocks, 80 links and 32 KiB per flow, enforced for disabled drafts too. At most 500 enabled flows per workspace.

## Execution

Flows run inside the uplink transaction, after events and alert rules. They sit behind their own savepoint, and each flow behind another one. A failing flow records an `error` run and nothing else is lost: other flows, the events and the alerts of that uplink stay. One firing per flow and device per 60 s. The newest 200 runs per flow are kept.

An uplink does not look at every enabled flow. `core.automation_triggers` holds one coarse row per trigger block of an enabled flow (`kind`, `event_type`, `external_id`, `gateway_id`, `metric`, where NULL means "any"), written inside the same transaction as every create, save, enable, disable and delete. One indexed lookup returns the flows this packet can possibly wake; only those are loaded and parsed, and only those with a firing take a savepoint. The rows are a deliberate superset — a block filtering on several devices or gateways records one row per event type and leaves those columns open — so the block's own matching still decides, and an extra candidate costs one definition parse rather than a missed firing.

Rising edges are batched the same way. All the `trigger.metric` blocks of all the candidate flows are resolved in at most four statements per uplink: open the memory rows whose comparison holds (`ON CONFLICT DO NOTHING`, which also keeps the earlier `since` another gateway wrote), read them back `FOR UPDATE` in key order, decide in Go, then one upsert for the blocks that fired and one delete for the comparisons that ended. Nothing is cached between transactions: ingest and the API are separate processes.

Cost per uplink is therefore O(candidate flows + firings), not O(enabled flows × metric blocks × sensors). `backend/tests/automation_scale_test.go` measures ms and SQL statements per uplink at 10, 100 and 300 enabled flows.

"ทดสอบ" is a dry run with the real current readings. It returns which blocks were true, false or not reached, colours them on the canvas, and executes nothing.

## The studio

One 42 px toolbar, a collapsible block palette, a React Flow canvas with snap grid and minimap, and an inspector with device, metric, channel and weekday pickers. Problems from validation outline the block in red. With nothing selected the inspector shows the flow as a Thai sentence. Three starter templates fill an empty workspace. Saving uses a revision, so a stale tab gets 409 instead of overwriting.

## API

`GET/POST /automations`, `GET /automations/{id}`, `POST /automations/{id}/save`, `/enable`, `/delete`, `GET /automations/{id}/runs`, `POST /automations/validate`, `POST /automations/{id}/test`. Mutations need owner or admin.

## Verification (2026-09-20)

Unit tests cover validation (including the command block's target, value, switch and permission problems and the action filter), AND/OR, false branches, stale readings, midnight windows and cycles. The integration tests cover tenant isolation, stale revisions, a tamper flow that opens one critical alert and queues one notification, a metric flow with a cross-device condition, dry run, and — in `tests/automation_commands_test.go` — who may arm device commands (switch off, viewer, admin without control, cross-project target, not-settable property, refused value, unknown device), a door opening that queues exactly one confirmed command attributed to the flow and its armer, the 60 s de-duplication, the action filter, the loop guard between two flows, the per-device cap, the switch turned off at run time, and a dry run that queues nothing. In the browser: a starter template was inserted, saved and enabled, the simulator's MBT01 tamper fired it within seconds, a critical alert opened, and the dry run created nothing.

## Measured cost (2026-09-20)

`TestAutomationScale` on a laptop, simulated uplinks of 9 sensors, flows mixing event and metric triggers on different devices:

| Enabled flows | Time per uplink |
| --- | --- |
| 10 | 17.9 ms |
| 100 | 25.7 ms |
| 300 | 30.4 ms |

The time includes storing the packet, samples, events and alert rules. Statement counts were not available because `pg_stat_statements` is not installed in the test database.

## Limits

- 500 enabled flows per workspace. The bound is structural rather than measured: the trigger index keeps an uplink's cost proportional to the flows a packet actually wakes, so the quota limits storage and the studio's list. Raise it only on the back of `TestAutomationScale` numbers from real hardware.
- A workspace that points hundreds of flows at the *same* device does put them all on one uplink; that is the case the cap still exists for.
- No schedule trigger, no delay block, no "else if" chains beyond the two condition outputs.
- Flows are not scoped to a project yet, although they carry a `project_id`.
