# Asset registry: every device, notes, MA and PM

Added 2026-09-20. The "อุปกรณ์ทั้งหมด" page lists every active gateway and every registered device of the workspace in one table, so the owner can keep asset data and run maintenance rounds.

## What is stored

Migration `00016`, all tenant-scoped under FORCE RLS:

- `core.asset_records`: one optional record per gateway or device: serial number, asset tag, location note, vendor, purchase date, warranty end, last battery change, status (`in_service`, `spare`, `repair`, `retired`) and free notes.
- `core.maintenance_plans`: recurring work on an asset: title, kind (`pm`, `calibration`, `battery`, `inspection`, `other`), interval in days, next due date, last done date, enabled.
- `core.maintenance_logs`: an append-only history: kind (`pm`, `ma`, `repair`, `battery`, `calibration`, `inspection`, `note`), title, detail, when, by whom, optional cost. The runtime role may only insert and read logs.

Logging work against a plan sets the plan's `last_done` to that day and moves `next_due` forward by the interval. A `battery` log also updates the record's last battery change.

## The page

One lean toolbar with counters, search, filters and CSV export. Chips filter the table: all, MA/PM overdue, due within 30 days, battery under 20 %, warranty ending within 60 days, offline (no data for 15 minutes). The table shows photo, name, model, MAC or id, project, gateway, status, last seen, battery, asset tag and serial, next MA/PM and notes. A row opens a drawer with three tabs: record, plans, history.

The list joins live facts that already exist: last seen is the newest packet of a gateway or the newest stream of a device across all gateways, so roaming wearables are covered. Battery comes from the newest decoded sample.

## API

`GET /assets`, `GET /assets/export.csv`, `GET /assets/{kind}/{id}`, `POST /assets/{kind}/{id}`, `POST /assets/{kind}/{id}/plans`, `POST /plans/{id}/update`, `POST /plans/{id}/delete`, `POST /assets/{kind}/{id}/logs`. Viewers can read. Mutations need owner or admin. The CSV is UTF-8 with a BOM so Excel opens Thai text, and cells that start with `=`, `+`, `-` or `@` are neutralised.

## Limits

- No reminders yet: an overdue plan shows in the page but does not raise an alert or a notification.
- No attachments such as photos or calibration certificates.
- Removed devices and revoked gateways leave the list; their records and logs stay in the database.
