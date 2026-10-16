# Reports and data export

Added 2026-10-16. The "รายงาน" page (sidebar group OPERATE) has two tabs:

- **ส่งออกข้อมูล**: pick up to 50 fixed sensors, a range of up to 31 days (Bangkok calendar days) and a resolution,
  and download a CSV.
- **รายงานสรุป**: run a summary for a set of sensors and an allowed temperature / humidity range: min, max and average
  overall and per day, data coverage, excursions out of range, and hourly charts. Sets of sensors and ranges can be
  saved as reports. "พิมพ์ / บันทึก PDF" prints the report on A4 through the browser (no PDF on the server).

A saved report can also be **sent**: on a schedule (daily or weekly) or by hand, to the workspace's notification
channels (webhook, LINE, email). See "Scheduled delivery".

## What is reportable

Only **fixed sensors**: a tag with a live registration in the workspace, none of whose registrations is roaming or
uses a worn profile (wearables, emergency buttons; `domain.WornProfileIDs`). This is the rule of the 5-minute rollup
(`core.sample_rollup`, migration 00046) and of `core.twin_impersonal_tags`.

Wearables and roaming tags carry where a person was, which is personal data. They are not exportable here in v1:
their history belongs to the privacy export (`POST /devices/{id}/privacy-export`, owner only, logged, see
`privacy.md`). A request that names one is refused before any data is read:

| Refusal | Meaning |
|---|---|
| `400 personal_sensor` | the tag is registered with a wearable / button profile or set to roaming (in any project of the workspace) |
| `400 sensor_not_registered` | the gateway heard the tag but it has no live registration: it may be anyone's tag |
| `404 sensor_not_found` | no such sensor on that gateway in this workspace, or the gateway is outside the caller's projects |

`GET /reports/sensors` lists only reportable sensors whose profile can carry a temperature or humidity
(environment, Tuya, Zigbee2MQTT or an unknown profile), on live gateways.

## API

All under `/api/v1`, in `backend/api/openapi.json`.

- `GET /reports/sensors`
- `POST /reports/export` body `{sensors:[{gateway_id, external_id}], from, to, resolution}`; `from`/`to` RFC 3339,
  `resolution` one of `raw`, `5m`, `1h`, `1d`.
- `POST /reports/summary` body `{sensors, from, to, temp:{min,max}?, hum:{min,max}?}` or `{report_id, from, to}`.
- `GET /reports/definitions`, `POST /reports/definitions`, `GET /reports/definitions/{id}`,
  `POST /reports/definitions/{id}/update`, `POST /reports/definitions/{id}/delete`.

Limits (400 with the reason in `error`): `invalid_range` (not RFC 3339, or `from >= to`), `window_too_long`
(over 31 days), `no_sensors`, `too_many_sensors` (over 50), `duplicate_sensor`, `invalid_sensor`, `invalid_resolution`,
`raw_too_large` (a raw export over 120 sensor-days, see below), `sensor_outside_project` (a saved project report
naming a sensor on another project's gateway),
`invalid_limits` (temperature outside -100..200 °C, humidity outside 0..100 %, or min above max), `invalid_name`,
`report_or_sensors` (a summary given both). At most two exports per workspace and four per API process run at once
(429); summaries have their own slots of the same size.

Export and summary are reads sent as POST: the sensor list is a body, and CORS allows only GET and POST. The access
gate therefore lets a member with **read** access to the `reports` module run them (`readPost` in
`httpapi/access.go`); every other POST still needs `write`. `none` hides the module, including its GETs.

### CSV

UTF-8 with a BOM (Excel opens the Thai headers), streamed with `SendStreamWriter` under its own 30-minute context, the
write deadline moved forward every 1000 rows (the server's 15 s WriteTimeout is one deadline for a whole response).
The deadline also moves before the first row of every sensor and whenever 5 s passed since it last moved. Everything
that can refuse the request happens before the first byte. The status is sent with the first byte, so the file says
itself whether it is whole: its **last row** is `#END,<number of data rows>`, or `#INCOMPLETE,timeout` (the 30-minute
budget ran out) / `#INCOMPLETE,stream_failed` (anything else) when reading stopped part way; the failure is also
logged. A file without either row was cut off in transit. Columns:

- `เวลา` (Asia/Bangkok, `2006-01-02 15:04:05`), `เซนเซอร์` (the registration's name), `gateway`, `external_id`
- `raw`: `temperature`, `humidity`, `battery` from `core.sensor_samples.reading`, with the rollup's reading rules (a
  Zigbee2MQTT-shaped reading takes them from `metrics`; any other only as an environment frame; a battery only above
  0). Rows with none of the three are left out. Only samples of a 5-minute bucket the rollup holds are listed: the
  rollup writes buckets only while a tag is a fixed sensor and deletes them otherwise, so a tag that was worn or
  roaming and is fixed now exports only the time since (its earlier movement history stays the privacy export's).
  The newest few minutes, not rolled up yet (the worker runs every minute), are not in a raw export either.
  Raw is capped at **120 sensor-days** (sensors x days started; e.g. 4 sensors for 30 days, or 50 sensors for 2
  days): at `SAMPLE_MIN_INTERVAL_SEC=30` a sensor stores 2,880 samples a day and a row is about 110 bytes, so one
  sensor-day is about 0.3 MB and the cap about 38 MB, which the browser can still hold and save. More is refused
  with `raw_too_large`; the aggregated resolutions are not capped this way.
- `5m`, `1h`, `1d`: `n`, `t_avg`, `t_min`, `t_max`, `h_avg`, `h_min`, `h_max` from `core.sample_rollup`. `n` is the
  number of samples; averages are the mean of the 5-minute averages (every 5 minutes weighs the same); `1d` groups by
  Bangkok calendar day. Buckets that start in `[from rounded down to 5 minutes, to)`.

Rows come sensor by sensor in the order requested, then by time. Text cells starting with `=`, `+`, `-`, `@`, a tab or a carriage return are
neutralised (`csvCell`); numbers are written as numbers.

### Summary

Read from `core.sample_rollup` only, so a sensor whose samples have not been rolled up yet shows no data. The math is
`backend/internal/reports` (pure, unit tested):

- **Coverage**: 5-minute buckets with a temperature or humidity, over the 5-minute slots of the range that closed at
  least 2 minutes ago (the rollup worker runs every minute), so today's report is not short of data. Both counts use
  only those settled slots, so coverage never passes 100 %.
- **Overall and per day** (Bangkok calendar day): min of the bucket minimums, max of the maximums, mean of the
  averages.
- **Excursions**: runs of consecutive 5-minute buckets with `t_max > max` or `t_min < min` (humidity the same), one
  run per metric and direction. A missing bucket ends a run: the report only claims what was measured. Each has its
  start, end (end of the last bucket), duration and peak (highest `t_max` / lowest `t_min`). At most 500 per sensor:
  the 500 earliest by start (`excursions_truncated`).
- **Minutes out of range** per metric: distinct out-of-range buckets x 5, counted over every bucket.
- **Series**: hourly avg / min / max for the chart.

A saved report run by `report_id` uses its sensors and ranges. A sensor that has since been removed, moved out of the
caller's projects or turned into a wearable / roaming tag is left out and listed in `omitted` with its reason.

## Saved reports

Migration `00049_report_definitions.sql`: `core.report_definitions(tenant_id, id, project_id, name, kind, sensors,
temp_min, temp_max, hum_min, hum_max, created_by, created_at, updated_at)` under FORCE RLS. `project_id` NULL is a
workspace report, with the policies of alert rules (00042): a member limited to projects reads workspace reports and
their projects' reports and writes only their projects' reports; a workspace report needs full scope. In a workspace
report they read, sensors on other projects' gateways are left out. A project report holds only sensors on that
project's gateways (`sensor_outside_project`); when a gateway is later moved out of the project, a member limited to
the project no longer sees that sensor, and saving the report keeps it as stored rather than dropping it. Owner,
admin and operator write (with `reports`
module access `write`), everyone with the module reads. Every sensor is resolved on save like an export, so a
wearable cannot be saved. At most 200 per workspace (409 `too_many_reports`, counted by a definer function so a
project-limited writer cannot pass it). Writes are audited (`report.created`, `report.updated`, `report.deleted`).

## Scheduled delivery

Migration `00050_report_schedules.sql`. A saved report has a schedule and channels:

- `schedule`: `none`, `daily` or `weekly`; `schedule_weekday` (ISO, 1 = Monday, weekly only); `schedule_time`, a
  local Asia/Bangkok time `HH:MM`. A **daily** report sent at day D covers the Bangkok calendar day before D; a
  **weekly** one covers the seven Bangkok days ending where D begins. `next_run_at` is the next occurrence.
- `channels`: up to 10 notification channels (the ones of the alerts page). On save each must be enabled and be a
  workspace channel or one of the report's project, as for alert rules (`invalid_channels`); a schedule needs at
  least one (`no_channels`); a bad time or weekday is `invalid_schedule`. Saving replaces the schedule like every
  other field: a request without it means `none`.

`core.report_runs` holds one row per report sent (period, `schedule` or `manual`, `queued` until every delivery
settled, then `done`); `core.report_deliveries` one row per run and channel (`queued`, `sent`, `failed`, `skipped`,
attempts, last error). Both carry the report's project and take the definitions' project policies under FORCE RLS.
Runs older than 180 days are pruned by the alerts worker's prune pass, their deliveries with them.

**Worker.** `internal/reportjobs` runs in every API process next to the alerts worker, every 30 s, for every active
workspace:

1. Due reports (`next_run_at <= now`) are claimed `FOR UPDATE SKIP LOCKED`; each gets one run for its **latest**
   due occurrence and `next_run_at` moves to the next one. After downtime the periods missed in between are not
   caught up. A scheduled period (its start and end) has at most one run (`report_runs_schedule_once`), so replicas
   ticking at the same moment send it once, and switching a report from daily to weekly still sends the first weekly
   period although it starts on the same day as a daily one. Each report is queued in its own savepoint: one that
   cannot be queued is logged and retried ten minutes later, without holding up or rolling back the others. Saving a
   report without changing its schedule keeps `next_run_at`, so an edit never skips a period that is already due.
2. Queued deliveries are claimed like alert notifications (`FOR UPDATE SKIP LOCKED`, hidden for five minutes while
   being sent), one run at a time: a claim is rendered once (at most 60 s) and sent to each of its channels (at most
   20 s each, 10 channels), 260 s at worst, inside the five minutes, so another replica never sends it again. A
   workspace gets at most five runs per pass. Deliveries are retried with the same backoff (30 s doubling up to 30 min), `failed` after 5 attempts. A channel
   removed or disabled since, or a project channel of another project than the report's, fails the delivery. Email on
   a server without SMTP (`SMTP_HOST` empty) is `skipped` with `smtp_not_configured`. Every delivery of a **demo
   workspace** is `skipped` (its data is fictional); its reports still show on screen.
3. Each run is rendered once: the summary of every sensor over the period (the numbers of the reports page) and the
   hourly CSV of the export (same writer and guards, ending with `#END`). The sensors are resolved again when sent:
   one no longer a fixed sensor is left out, and so is, for a project report, a sensor whose gateway has moved to
   another project or out of every project (`sensor_outside_project`), so its readings never reach the first
   project's channels or members. The report on screen applies the same rule.

**What each channel receives.**

- **Email**: `multipart/mixed` with a `multipart/alternative` (plain text, then an HTML table: per sensor coverage,
  min / max / avg temperature and humidity, minutes out of range, number of excursions) and the CSV attached
  (`aether-report-<first day>-<last day>-1h.csv`). An attachment over 4 MB is left out and the message says so (an
  hourly CSV of 50 sensors over a week is under 1 MB). The subject is RFC 2047 encoded. Recipients are resolved like
  alert emails: a hand-written list, or the `members` audience, resolved at sending time by
  `core.report_recipients` (the report's sibling of `core.alert_recipients`): owners, members who see every project
  and, for a project report, that project's members, of the channel's roles, not erased, without the reports module
  set to `none`; each a blind copy.
- **LINE**: one text message of at most 5000 as LINE counts text, in UTF-16 code units (a Thai letter is one, an
  emoji such as 🍎 two; https://developers.line.biz/en/docs/messaging-api/text-character-count/): report name, period, ranges, one line per sensor (sensors
  that do not fit are counted, "…และอีก N เซนเซอร์"), and last a link
  `${APP_ORIGIN}/?reports&report=<id>&from=<YYYY-MM-DD>&to=<YYYY-MM-DD>`. The reports page opens that report on the
  summary tab and runs it for those days (after signing in if needed), once: the parameters are then removed from the
  address.
- **Webhook**: JSON `{schema: "aether.report.v1", sent_at, workspace, report, run, temp, hum, sensors, omitted, link}`
  with `X-Aether-Event: report` and the channel's HMAC in `X-Aether-Signature`, as alert webhooks.

**API.**

- Schedule fields on `POST /reports/definitions` and `/{id}/update` (owner, admin, operator with reports `write`;
  audited like every save).
- `POST /reports/definitions/{id}/send` `{from?, to?}`: send now to the report's channels, by default for yesterday
  (Bangkok), else for `[from, to)` (RFC 3339, at most 31 days). 202 with the queued run; delivered by the worker's next
  pass. At most 10 per workspace and hour (`429 too_many_sends`, counted over every project by a definer function).
  Needs write access; audited (`report.sent`).
- `GET /reports/definitions/{id}/runs`: the last 20 runs with each channel's status, attempts and last error,
  readable by whoever can read the report.
- `GET /reports/channels`: the channels for the editor (id, name, kind, enabled, project; no configuration) and
  `email_available`, readable with the reports module (the alerts page's list needs the alerts module). A member
  whose alerts module is `none` therefore still sees the channels' names and kinds here, never their targets or
  secrets.

The page's editor has a "ส่งรายงานอัตโนมัติ" section (frequency, weekday, time, channels with their kind; a disabled
channel, an email channel without SMTP or another project's channel cannot be picked), a "ส่งตอนนี้" button (sends the
days selected for the summary) and "ประวัติการส่ง" with every run's per-channel status and error.

## Privacy

The three data routes are in the read-access log (`readRules`): `report_sensors`, `reports_export` (like every export on
its own row, never deduplicated, with the subject `<n> sensors, <resolution>`; written before the CSV streams, and a
failure to write it withholds the file with 503) and `reports_summary`. They name sensors and carry
their readings, though never a person's.

## Printing

The print stylesheet (`frontend/app/reports/reports.css`, `@media print`) applies only while the reports page is on
screen (`:has(.rp)`): it hides the sidebar, the shell bar, banners and the page's controls, switches the tokens to
dark-on-white and lays the report out on A4 with its header (workspace, report name, range, allowed ranges, generated
at), the overview table, then one page per sensor with its figures, charts, daily table and excursions.

## Known limits

- The rollup of a range can be recomputed with the tag's current status (`core.rollup_samples_range`, used by the demo
  backfill only): for a tag that was roaming and is fixed now, such a recompute over its roaming period would make
  that period exportable. No member-facing route calls it.
- The browser holds a download in memory before saving it, hence the raw cap; the page waits up to 30 minutes for an
  export and 65 s for a summary.
