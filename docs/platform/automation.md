# Automation Studio v1

Added 2026-09-20. "ถ้า…แล้ว…" flows built from blocks on a canvas. A flow reacts to device events or sensor values, checks conditions across any devices of the workspace, and acts.

## What runs for real, and what does not

Aether has no downlink or command channel to gateways or devices today. So:

- **Real:** open an alert, send through a notification channel (webhook, LINE, email). These use the same alert and notification pipeline as alert rules.
- **Not available:** "สั่งงานอุปกรณ์". The block is in the palette, marked "เร็ว ๆ นี้ · ต้องมีช่องทางสั่งงาน gateway". A flow that contains it cannot be enabled. Validation refuses it in the route and again in the repository, and the evaluator never emits it.

Device control needs a command path first: MQTT downlink topics per gateway, an acknowledgement model, and actuators in the device catalog.

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

Unit tests cover validation (25 cases), AND/OR, false branches, stale readings, midnight windows and cycles. The integration test covers tenant isolation, stale revisions, a tamper flow that opens one critical alert and queues one notification, a metric flow with a cross-device condition, dry run, and the disabled command block. In the browser: a starter template was inserted, saved and enabled, the simulator's MBT01 tamper fired it within seconds, a critical alert opened, and the dry run created nothing.

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
