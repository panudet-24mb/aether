# Sensor history and reusable templates

Implemented locally on 2026-09-12. `00002_sensor_history.sql` adds FORCE RLS tables for discovered sensor streams, durable samples and immutable template versions.

## User flow

In Live monitoring, select a sensor → **ตั้งชื่อ / Template** → create a template version with temperature/humidity high thresholds → select it and save. The name/template is stored in PostgreSQL and reappears after reload. Multiple sensors may reference the same template version. Create a new version to change thresholds; existing assignments do not silently upgrade.

The graph offers 1h, 24h and 7d windows, returning at most the latest 200 samples per sensor in that window. This is not downsampled whole-range coverage: a busy sensor's 200 points may cover only a small part of the selected window. The last known sample remains visible even when outside the graph window, with the existing stale indicator.

## Data semantics

- Raw capture, stream discovery and durable sample inserts commit together; MQTT ACK follows the transaction.
- One decoded sample per external sensor per uplink. The exact-payload SHA-256 plus tenant/gateway/external sensor is the dedupe identity. Retries of identical payloads do not create new samples or advance sensor freshness. Uplinks without changing timestamps and identical payloads are therefore indistinguishable from retries.
- Raw packet pruning (100/gateway) no longer deletes samples. History starts at activation of the new collector; previously pruned data is not reconstructed.
- `sensor_streams` represents observed sensor feeds, not an automatic claim that nearby devices have been adopted as owned assets. Discovery is capped at 100 decoded sensor streams per gateway; additional new streams are skipped while existing streams continue receiving data.
- Each saved sample records decoder `minew-ffe1-a101@1` and the template assignment at ingest time. Reassignment affects future samples and current display metadata; it does not rewrite past samples.
- Template definitions currently cover high thresholds for the existing Minew environmental decoder. They are not arbitrary code decoders or arbitrary widget designs. Threshold indicators do not send alerts or issue hardware commands.
- SIM remains a diagnostic source marker from the simulator payload; future registration must make provenance server-owned before treating it as an authorization or billing property.
- Samples persist without automatic expiry in this local iteration. Production retention, quotas, backups, time-series aggregation and TimescaleDB remain separate work. Use the existing database volume; deleting it deletes stored history.

## API

- `GET /api/v1/live?range=1h|24h|7d`: owner/admin live view and saved history (latest 200 samples/sensor).
- `GET /api/v1/templates`: list up to 100 tenant template versions.
- `POST /api/v1/templates`: name, positive version, decoder_id and definition (`temperature_high`, `humidity_high`, nullable).
- `POST /api/v1/gateways/{id}/streams/{external}/template`: assign `template_id` and display `name` to a discovered stream.

Tenant scope is checked by session membership plus RLS and composite foreign keys. Template and assignment writes create audit records. Templates are limited to 100 versions per workspace in this iteration; duplicate name/version is a conflict. No update/delete endpoint mutates immutable versions.

## Verification

Integration coverage includes raw pruning without sample loss, exact replay dedupe, template serialization/reload, duplicate version rejection, cross-tenant reads/assignment, viewer write rejection, and historical template assignment remaining unchanged. Simulator and existing backend race tests also pass. Frontend TypeScript/build and live HTTP/API checks validate the local integration; browser interaction QA was not performed.

Next: Dashboard Studio consumes these saved streams/templates; general declarative decoder definitions and long-range aggregated chart queries require additional implementation.
