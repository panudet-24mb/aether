# requirement.md — Multi‑tenant IoT Cloud Platform with 2D Floor‑Plan Designer & 3D Building View

> เอกสารนี้เขียนเพื่อส่งต่อให้ AI/ทีมพัฒนา "สร้างต่อ" ได้ทันที
> ภาษาไทยใช้สำหรับคำอธิบาย, ภาษาอังกฤษใช้สำหรับ identifier/ศัพท์เทคนิค
> สถานะ: Draft v1.2 — 2026‑09‑12 (configurable workspaces, dashboards, templates and kiosk scope confirmed)

---

## 0. TL;DR

สร้าง SaaS IoT ที่ผู้ใช้ทั่วไป **สมัครเอง → เลือกแบรนด์/รุ่นอุปกรณ์ → ต่ออุปกรณ์ → วางลงบนแปลนอาคาร 2D ที่วาดเอง → ดูเป็นอาคาร 3D พร้อมสถานะ real‑time** โดยไม่ต้องเขียนโค้ด

หลักออกแบบ 3 ข้อที่ห้ามละเมิด:
1. **Core ไม่รู้จัก protocol** — ทุก protocol เป็น adapter plugin, ทุกอุปกรณ์เป็น device profile (JSON)
2. **Canonical telemetry event** เป็น schema เดียวที่ทุก service ใช้ร่วมกัน
3. **Multi‑tenant ด้วย PostgreSQL Row Level Security** ตั้งแต่ตารางแรก
4. **One codebase, two deployment modes** — Cloud และ On-premise ใช้ source และ backend image ชุดเดียว เปลี่ยน configuration และ deployment manifest; ไม่ fork ตามรูปแบบติดตั้ง

---

## 1. Tech Stack (Fixed)

| Layer | Technology | หมายเหตุ |
|---|---|---|
| Frontend | Next.js 15+ (App Router) + TypeScript 5.x | SSR + RSC |
| UI | shadcn/ui + Tailwind CSS | dark‑mode first, ธีม "futuristic/clean" |
| Data table | AG Grid Community (fallback TanStack Table) | virtual scroll |
| 2D canvas | **Konva + react‑konva** (fallback: PixiJS) | vector floor‑plan editor |
| 3D | **Three.js + React Three Fiber + drei** | ดู §6.3 |
| Charts | Recharts (dashboard) + uPlot (time‑series ความหนาแน่นสูง) | |
| State | Zustand + TanStack Query | |
| Realtime client | native WebSocket (reconnect wrapper) | Socket.IO ไม่ใช้เพราะ server เป็น Go |
| i18n FE | next‑intl (namespace‑based, SSR) | th, en เริ่มต้น |
| Backend | Go 1.26 + Fiber v3 | Hexagonal (ports & adapters) |
| ORM | GORM v2 | ต้อง set `app.tenant_id` ทุก transaction (§9.2) |
| Migration | goose | SQL migrations |
| API docs | swaggo/swag → OpenAPI 3 | |
| Database | PostgreSQL 18 + **TimescaleDB extension** | single DB, RLS, hypertable สำหรับ telemetry |
| Cache | Redis 7 | session, plan cache, rate limit, translation cache |
| Pub/Sub | Redis Pub/Sub (phase 1) → Kafka (phase 3) | ผ่าน port `EventBus` เปลี่ยน adapter ได้ |
| Queue | **asynq** (Go, Redis‑backed) — แทน BullMQ ซึ่งเป็น Node | notifications, reports, exports |
| MQTT broker | EMQX 5 (open source) | auth/ACL ผ่าน HTTP hook ไปที่ backend |
| LoRaWAN NS | ChirpStack v4 | multi‑tenant, ต่อผ่าน gRPC + MQTT |
| Storage | MinIO (S3 compatible) | floor‑plan images, 3D models, exports |
| Auth | JWT access (15 min) + refresh (30 d, rotating) + OAuth Google/Facebook | multi‑domain SSO ผ่าน cookie domain |
| i18n BE | custom translation service (JSON namespace + DB override) | Redis cached |
| Currency | custom service + `Intl.NumberFormat` | BOT API primary, ECB fallback |
| Container | Docker + Compose (dev) → Kubernetes (prod) | |

---

## 2. Personas & Key Journeys

| Persona | ต้องการ |
|---|---|
| **Owner / Admin** (SME, เจ้าของตึก, ฟาร์ม) | สมัคร, เพิ่มอุปกรณ์เอง, วางแปลน, ตั้ง alert, ดูบิล |
| **Operator / ช่าง** | ดูสถานะ, รับ alert, ปิด work order |
| **Viewer** | ดู dashboard/3D อย่างเดียว |
| **Integrator / Partner** | สร้าง device profile ใหม่, ใช้ API |
| **Platform Super‑admin** (เรา) | จัดการ catalog, plan, tenant, สุขภาพระบบ |

### Journey A — สมัครถึงเห็นข้อมูลแรก (ต้องเสร็จภายใน 10 นาที)
1. Register (email หรือ Google/Facebook) → auto‑create tenant + site แรก
2. Add device → เลือก brand → เลือก model → wizard ถามเฉพาะ field ที่ adapter นั้นต้องการ
3. หน้า "Waiting for first packet" แสดง live log (packet arrived / decoded / stored)
4. Dashboard สร้างอัตโนมัติจาก profile

### Journey B — วางแปลนอาคาร
1. New floor plan → เลือก: วาดเอง / อัปโหลดภาพ‑PDF‑DXF เป็นพื้นหลัง / template (office, warehouse, farm)
2. วาดผนัง, ห้อง, ประตู, โซน
3. ลากอุปกรณ์จาก sidebar วางบนแปลน
4. กด "View 3D" → อาคารถูก extrude อัตโนมัติ, อุปกรณ์ปรากฏพร้อมสีสถานะ

---

## 3. Functional Requirements

รหัส: `FR‑<module>‑<n>` ระดับ: **M** (must, MVP), **S** (should), **C** (could)

### 3.1 Auth & Tenant
- FR‑AUTH‑1 (M) Register ด้วย email+password (Argon2id), Google OAuth, Facebook OAuth
- FR‑AUTH‑2 (M) Email verification, password reset
- FR‑AUTH‑3 (M) JWT access + rotating refresh token; refresh reuse detection → revoke family
- FR‑AUTH‑4 (M) Tenant auto‑created on register; user ↔ tenant เป็น many‑to‑many พร้อม role
- FR‑AUTH‑5 (M) Roles: `owner`, `admin`, `operator`, `viewer` + permission matrix (ดู §8)
- FR‑AUTH‑6 (S) Invite member by email, accept link
- FR‑AUTH‑7 (S) API keys per tenant (scoped, revocable)
- FR‑AUTH‑8 (C) SSO across sub‑domains (`*.platform.com`) และ custom domain per tenant

### 3.2 Plans & Billing
- FR‑BILL‑1 (M) Plans: Free (5 devices, 7‑day retention), Pro, Business, Enterprise (configurable ใน DB)
- FR‑BILL‑2 (M) Enforce limits: device count, data retention, users, API rate (plan cached ใน Redis)
- FR‑BILL‑3 (M) Multi‑currency display (THB base) ผ่าน Currency Engine; dual‑amount (base + display)
- FR‑BILL‑4 (S) Exchange‑rate auto‑fetch daily (BOT → ECB fallback), rate history, gain/loss report
- FR‑BILL‑5 (S) Payment gateway integration (Stripe/Omise) — abstract ผ่าน port `PaymentGateway`
- FR‑BILL‑6 (C) Usage‑based add‑ons (extra devices, retention)

### 3.3 Device Catalog & Profiles
- FR‑CAT‑1 (M) Catalog hierarchy: `Brand → Model → Profile version`
- FR‑CAT‑2 (M) Device Profile เป็น JSON ตาม schema §7.1; เก็บใน DB, versioned, immutable per version
- FR‑CAT‑3 (M) Profile ประกาศ: transport, decoder, metrics (name, type, unit, semantic), commands, onboarding fields, default dashboard, default 3D icon
- FR‑CAT‑4 (M) "Generic" profile ต่อ transport (Generic MQTT JSON, Generic LoRaWAN + custom JS decoder, Generic HTTP)
- FR‑CAT‑5 (M) Super‑admin CRUD catalog; publish/unpublish
- FR‑CAT‑6 (S) Tenant‑private profiles (integrator สร้างใช้เองได้)
- FR‑CAT‑7 (S) Community profile submission + review queue
- FR‑CAT‑8 (M) Seed catalog เริ่มต้น ≥ 20 รุ่น: Milesight (EM300, WS series, UC500, AM300), Dragino (LHT65, LDS02), RAK (WisNode), Minew (S1, S3, MG3 gateway), generic Modbus RS485 sensors

### 3.4 Protocol Adapters (Plugins)
- FR‑ADP‑1 (M) Adapter contract (Go interface, §9.3) — ทุก adapter รันเป็น service แยกได้, คุยกับ core ผ่าน EventBus
- FR‑ADP‑2 (M) `mqtt-generic`: device/gateway publish มาที่ EMQX; topic `t/{tenant}/{device}/up`; auth ด้วย per‑device token; ACL กัน cross‑tenant
- FR‑ADP‑3 (M) `lorawan-chirpstack`: auto‑create ChirpStack tenant/application/device ผ่าน gRPC เมื่อ onboard; subscribe uplink; downlink queue; JS codec run ใน sandbox (goja)
- FR‑ADP‑4 (M) `minew-gateway`: parse Minew gateway JSON (MQTT/HTTP) → แยก per‑tag advertising frame → decode ตาม tag profile; dedupe across gateways; เก็บ RSSI per gateway
- FR‑ADP‑5 (M) `http-webhook`: POST JSON พร้อม device token
- FR‑ADP‑6 (S) `modbus-gateway`: รองรับ gateway ของเราเอง (edge agent) ที่อ่าน Modbus แล้ว publish MQTT
- FR‑ADP‑7 (C) `bacnet`, `opcua`, `ttn` (The Things Stack), `tuya`
- FR‑ADP‑8 (M) Adapter health endpoint + metrics (packets/s, decode errors)

### 3.5 Onboarding Wizard & Diagnostics
- FR‑ONB‑1 (M) Wizard ขั้นตอน: เลือก brand → model → กรอก fields ที่ profile.onboarding ระบุ → เลือก site/floor → ผูก gateway (ถ้าจำเป็น) → รอ packet แรก
- FR‑ONB‑2 (M) Gateway onboarding: สร้าง credential + แสดง config snippet (MQTT host/port/user/pass/topic) + QR
- FR‑ONB‑3 (M) Live diagnostics stream (WebSocket): `connected`, `packet_received`, `decoded`, `stored`, `error{reason}`
- FR‑ONB‑4 (M) Device status: `never_seen`, `online`, `offline` (heartbeat timeout จาก profile), `error`
- FR‑ONB‑5 (S) Bulk import CSV (DevEUI/AppKey list)
- FR‑ONB‑6 (S) Claim device by QR/serial (สำหรับ hardware ที่เราขาย, pre‑provisioned)

### 3.6 Telemetry Ingestion & Storage
- FR‑TEL‑1 (M) Canonical event schema §7.2; ทุก adapter ต้อง emit schema นี้เท่านั้น
- FR‑TEL‑2 (M) Storage: TimescaleDB hypertable `telemetry` (tenant_id, device_id, ts, metric, value_num, value_str, value_json) + continuous aggregates 1m/1h/1d
- FR‑TEL‑3 (M) Latest‑value table `device_state` (upsert) สำหรับ dashboard/3D
- FR‑TEL‑4 (M) Retention policy per plan (Timescale `drop_chunks`)
- FR‑TEL‑5 (M) Ingestion target: 5,000 events/s per node (phase 1), horizontal scale ผ่าน Kafka partitions (phase 3)
- FR‑TEL‑6 (S) Raw payload archive (S3, 30 days) เพื่อ re‑decode เมื่อ profile แก้ไข
- FR‑TEL‑7 (M) Publish `device.state.changed` ไป EventBus → WebSocket fan‑out per tenant

### 3.7 Rule Engine, Alerts & Notifications
- FR‑RUL‑1 (M) Rule = trigger + conditions + actions (JSON DSL §7.3), evaluate ที่ ingestion path (stateless) และ scheduler (offline/heartbeat)
- FR‑RUL‑2 (M) Conditions: threshold, delta, rate‑of‑change, offline, geofence/zone enter‑exit (สำหรับ tracking), time window, AND/OR groups
- FR‑RUL‑3 (M) Actions: notify (email, LINE Notify/Messaging API, webhook, in‑app), create work order, send device command, set device tag
- FR‑RUL‑4 (M) Alert lifecycle: `open → acknowledged → resolved`, severity, dedupe window, escalation (S)
- FR‑RUL‑5 (M) Notification worker ผ่าน asynq พร้อม retry/backoff และ per‑tenant rate limit
- FR‑RUL‑6 (S) Rule templates ต่อ profile (เช่น "temperature out of range" มาพร้อม profile)

### 3.8 Dashboards & Data
- FR‑DSH‑1 (M) Auto dashboard per device จาก profile (widget ตาม semantic: gauge, line, status, map)
- FR‑DSH‑2 (M) Custom dashboard builder (grid layout, drag‑resize, widget library ≥ 10 ชนิด)
- FR‑DSH‑3 (M) Data explorer: AG Grid + server‑side pagination, filter, export CSV/XLSX (queued job)
- FR‑DSH‑4 (M) Multi‑series chart, time‑range picker, aggregation (raw/1m/1h/1d) auto‑select
- FR‑DSH‑5 (S) Scheduled reports (PDF/XLSX) via email
- FR‑DSH‑6 (S) Public/shared dashboard link (read‑only token)

### 3.9 Sites, Buildings, Floors, Zones (Spatial Model)
- FR‑SPA‑1 (M) Hierarchy: `Tenant → Site (lat/lng) → Building → Floor → Zone → Device placement`
- FR‑SPA‑2 (M) Floor มี `plan` (vector JSON §7.4) + optional background image (MinIO)
- FR‑SPA‑3 (M) Device placement = (floor_id, x, y, z, rotation, icon override)
- FR‑SPA‑4 (S) Outdoor map (Mapbox/MapLibre) สำหรับ site‑level และฟาร์ม

### 3.10 2D Floor‑Plan Designer  ★
- FR‑2D‑1 (M) Canvas editor (react‑konva) รองรับ: pan, zoom (wheel/pinch), grid + snap (configurable 0.1–1 m), rulers, unit scale (px ↔ m)
- FR‑2D‑2 (M) Tools: select, wall (polyline, thickness), room (polygon, auto‑area), door, window, zone (polygon, color), text label, icon/device, measure
- FR‑2D‑3 (M) Background: upload PNG/JPG/PDF(page→image)/SVG; calibrate scale โดยลาก 2 จุดแล้วใส่ระยะจริง
- FR‑2D‑4 (S) Import DXF (layers → walls/text) ผ่าน server job (`dxf-parser`)
- FR‑2D‑5 (M) Layers panel: show/hide/lock (background, walls, zones, devices, labels, heatmap)
- FR‑2D‑6 (M) Undo/redo (command stack, ≥ 100 steps), copy/paste, align/distribute, keyboard shortcuts
- FR‑2D‑7 (M) Device palette: ลากจาก "unplaced devices" ลง canvas; icon ตาม profile; badge สถานะ real‑time (online/offline/alert)
- FR‑2D‑8 (M) Live overlay mode: แสดงค่า metric ที่เลือก (เช่น temperature) เป็นตัวเลข/สี บนอุปกรณ์; คลิก → side panel กราฟ 24 h
- FR‑2D‑9 (S) Heatmap layer: interpolate (IDW) จากอุปกรณ์ที่มี semantic เดียวกัน ภายในขอบเขต room
- FR‑2D‑10 (S) Indoor tracking overlay: ตำแหน่ง BLE tag (จาก location engine) เคลื่อนไหวแบบ smooth, trail 5 นาที
- FR‑2D‑11 (M) Save เป็น vector JSON (§7.4), autosave ทุก 10 s เมื่อ dirty, version history (S)
- FR‑2D‑12 (M) Export PNG/SVG; print‑ready PDF (S)
- FR‑2D‑13 (M) Templates: blank, office, warehouse, factory line, greenhouse
- FR‑2D‑14 (M) Visual style: dark canvas, neon‑subtle accent, glass panels, smooth 60 fps บน 500 objects
- FR‑2D‑15 (S) Multi‑user presence (cursor ของคนอื่น) ผ่าน WebSocket — ไม่ต้อง CRDT ใน phase แรก ใช้ optimistic lock (`plan.version`)

### 3.11 3D Building View  ★
- FR‑3D‑1 (M) Auto‑generate 3D จาก 2D plan: walls extrude ตาม `floor.height` (default 3 m), floors stacked ตาม `elevation`, rooms เป็น floor slabs โปร่งใส, doors/windows เป็นช่อง
- FR‑3D‑2 (M) Stack ทุก floor ของ building; "explode view" slider แยกชั้น; isolate floor (ชั้นอื่นจาง)
- FR‑3D‑3 (M) Device markers 3D: sprite/low‑poly icon ที่ตำแหน่ง (x, y, z) + สีสถานะ + billboard label; hover/click → panel เดียวกับ 2D
- FR‑3D‑4 (M) Camera presets (iso, top, front), orbit controls, smooth fly‑to device/zone, minimap
- FR‑3D‑5 (M) Live overlay: สี wall/room ตาม metric (เช่น room temperature → color ramp), pulse animation เมื่อ alert
- FR‑3D‑6 (S) Import glTF/GLB ของอาคารจริง (จาก architect) แล้ว align กับ plan (transform gizmo)
- FR‑3D‑7 (C) IFC import (web‑ifc) → geometry + storey mapping
- FR‑3D‑8 (M) Performance: ≤ 16 ms frame บน laptop integrated GPU สำหรับ 10 ชั้น × 200 devices; ใช้ InstancedMesh, LOD, frustum culling
- FR‑3D‑9 (M) Style: clean architectural look — soft shadows, ambient occlusion (SSAO postprocess toggle), subtle grid ground, dark/light theme sync
- FR‑3D‑10 (S) Screenshot/turntable video export
- FR‑3D‑11 (S) Section/clipping plane เพื่อดูภายในชั้น
- FR‑3D‑12 (C) WebXR (AR on tablet) preview

### 3.12 Indoor Tracking (BLE)
- FR‑TRK‑1 (S) Location engine service: input RSSI per (tag, gateway, ts); gateway positions มาจาก placement; algorithm trilateration + Kalman; output `position.updated` events
- FR‑TRK‑2 (S) Zone presence, dwell time, enter/exit events ป้อน rule engine
- FR‑TRK‑3 (C) Asset history playback บน 2D/3D

### 3.13 Work Orders (light CMMS)
- FR‑WO‑1 (S) Create from alert or manual; assign, status, checklist, attachments (MinIO), comments
- FR‑WO‑2 (C) Form builder, preventive maintenance schedule

### 3.14 Device Commands (Downlink)
- FR‑CMD‑1 (M) Command model จาก profile; queue per device; states `queued → sent → acked/failed/expired`
- FR‑CMD‑2 (M) UI: button/inputs auto‑generated จาก profile.commands
- FR‑CMD‑3 (M) Audit log ทุกคำสั่ง (who, when, payload)

### 3.15 i18n & Localization
- FR‑I18N‑1 (M) UI th/en (next‑intl namespaces: common, auth, devices, designer, billing)
- FR‑I18N‑2 (M) Backend messages/validation errors localized; catalog names/descriptions multi‑lang จาก DB; Redis cache ด้วย key `i18n:{lang}:{ns}:{version}`
- FR‑I18N‑3 (M) Units: metric default, per‑user override (°C/°F)
- FR‑I18N‑4 (M) Date/number/currency ผ่าน `Intl.*` + timezone per user (default Asia/Bangkok)

### 3.16 Platform Admin
- FR‑ADM‑1 (M) Tenants list, impersonate (audited), plan override, suspend
- FR‑ADM‑2 (M) Catalog management, adapter health, ingestion metrics
- FR‑ADM‑3 (S) Feature flags per tenant

---

### 3.17 Cloud & On-premise (confirmed)
- FR-DEP-1 (M) Backend source และ release image ชุดเดียวสำหรับ `DEPLOYMENT_MODE=cloud|onprem`
- FR-DEP-2 (M) On-premise ต้อง login แบบ local, รับ/เก็บ telemetry และอ่านข้อมูลได้โดยไม่ติดต่อ Cloud หรือ external identity provider
- FR-DEP-3 (M) On-premise ปิด self-registration; สร้าง owner ครั้งแรกผ่าน local admin command ที่ป้องกันการใช้ซ้ำ
- FR-DEP-4 (M) TLS ingress และ DB certificate verification, secret rotation, backup/restore และ migration/rollback runbook ก่อนเปิด production
- FR-DEP-5 (M) เตรียม offline installation bundle; แยกข้อกำหนดตอน build/install ที่ต้องมี dependency จาก runtime ที่ทำงานออฟไลน์ได้
- FR-DEP-6 (M) Hardware pilot แรกใช้ Minew MHS ที่ผู้ใช้มี: gateway เป็น USB Mini BLE/Wi-Fi (คาดว่า MG3, รอ model/firmware); ยืนยัน raw packet และ S1 decoder ด้วย golden test ก่อนอ้างว่ารองรับเครื่องจริง

## 4. Non‑Functional Requirements

| ด้าน | ข้อกำหนด |
|---|---|
| Multi‑tenancy | ทุกตารางที่มีข้อมูลลูกค้าต้องมี `tenant_id` + RLS policy; ไม่มี query ใดหลุด RLS (test ใน CI) |
| Security | OWASP ASVS L2; rate limit per IP/user/API key; MQTT ACL per device; secrets ผ่าน env/K8s secret; audit log |
| Availability | 99.5 % (phase 1) → 99.9 %; graceful degrade: ingestion ต้องรับได้แม้ UI ล่ม |
| Performance | API p95 < 300 ms; WS fan‑out < 1 s from ingest; designer 60 fps @ 500 objects; 3D ≥ 45 fps |
| Scalability | stateless API pods; adapters scale independently; Timescale compression > 7 days |
| Observability | OpenTelemetry traces, Prometheus metrics, structured logs (zerolog), Grafana dashboards |
| Backup | Postgres PITR daily + WAL; MinIO versioning |
| Compliance | PDPA: consent on register, data export, delete tenant (hard delete job) |
| Accessibility | WCAG 2.1 AA สำหรับหน้าทั่วไป (designer/3D ยกเว้นบางส่วน) |
| Browser | Chrome/Edge/Safari ล่าสุด 2 เวอร์ชัน; WebGL2 required for 3D |

---

## 5. High‑Level Architecture

```
[Devices/Gateways] --MQTT--> [EMQX] --+
[LoRa Gateways] --> [ChirpStack] --MQTT/gRPC--> |
[HTTP devices] ----------------------------> [API]
                                                |
                     +---------------------------+---------------------------+
                     |  Adapter services (Go): mqtt-generic, lorawan, minew, http  |
                     +---------------------------+---------------------------+
                                                | canonical TelemetryEvent
                                       [EventBus: Redis Pub/Sub -> Kafka]
                                                |
        +-----------------+-----------------+---+-------------+------------------+
        | Ingest/Store    | Rule Engine     | State/WS Hub    | Location Engine  |
        | (Timescale)     | (alerts)        | (per-tenant)    | (BLE)            |
        +-----------------+-----------------+-----------------+------------------+
                                                |
                                   [Core API: Fiber v3, hexagonal]
                                   ports: Repo, EventBus, Queue, Storage,
                                          PaymentGateway, Notifier, Translator, FxRate
                                                |
                                   [Next.js 15 frontend]  [Admin]
```

### 5.1 Go project layout (hexagonal)
```
/cmd
  api/            # Fiber HTTP + WS
  worker/         # asynq workers
  adapter-mqtt/   # adapter binaries (one per protocol)
  adapter-lorawan/
  adapter-minew/
  rule-engine/
  location-engine/
/internal
  domain/         # entities + domain services (no framework imports)
    tenant, device, catalog, telemetry, rule, alert, spatial, billing, workorder
  ports/          # interfaces: repositories, EventBus, Queue, Storage, Notifier, FxRate, Payment
  app/            # use cases (application services)
  adapters/
    http/         # Fiber handlers, DTOs, swag annotations
    ws/           # WebSocket hub
    postgres/     # GORM repos + RLS context
    redis/        # cache, pubsub, rate limit
    kafka/        # phase 3
    asynq/
    minio/
    emqx/         # auth/ACL hooks, publish
    chirpstack/   # gRPC client
    protocol/     # decoders: modbus map, cayenne, js sandbox (goja)
    notify/       # email(SMTP), line, webhook
    fx/           # BOT, ECB clients
/pkg              # shared libs (canonical event, errors, i18n)
/migrations       # goose SQL
/profiles         # seed device profiles (JSON)
```

### 5.2 Frontend layout
```
/app/[locale]/(marketing)      # landing, pricing
/app/[locale]/(auth)           # login, register, oauth callback
/app/[locale]/(app)/[tenant]/  # dashboard, devices, designer, 3d, rules, alerts, billing, settings
/components/ui                 # shadcn
/features/designer2d           # konva editor (tools, layers, store, serializer)
/features/building3d           # r3f scene, plan->geometry builder, overlays
/features/devices, rules, dashboards, billing
/lib/api (openapi-generated client), /lib/ws, /lib/i18n
```

---

## 6. Key Design Details

### 6.1 Multi‑tenant with RLS
- ทุกตาราง: `tenant_id uuid not null`
- Policy: `USING (tenant_id = current_setting('app.tenant_id')::uuid)`
- Backend: middleware แกะ JWT → เปิด transaction → `SET LOCAL app.tenant_id = '<uuid>'` ก่อนทุก query (GORM `Session` + `BeforeQuery` hook หรือ wrapper `WithTenant(ctx, db)`)
- Super‑admin ใช้ role DB แยกที่ `BYPASSRLS` เฉพาะ endpoint admin (audited)
- Telemetry hypertable ก็ใช้ RLS เช่นกัน; partition key `(tenant_id, ts)`

### 6.2 2D Plan → 3D geometry pipeline (client‑side)
1. โหลด `FloorPlan` JSON (§7.4)
2. Walls: polyline → `THREE.Shape` (offset ตาม thickness) → `ExtrudeGeometry(height)`; merge เป็น 1 mesh ต่อชั้น
3. Rooms: polygon → `ShapeGeometry` slab (opacity 0.15) + edge lines
4. Doors/Windows: boolean‑free approach — แบ่ง wall segment เป็นชิ้นย่อยเว้นช่อง (ไม่ใช้ CSG เพื่อ performance)
5. Floors: group ที่ `y = elevation`; explode = เพิ่ม gap ระหว่าง group
6. Devices: `InstancedMesh` per icon type; color attribute อัปเดตจาก state store; label ด้วย `drei/Html` (limit 100 visible) หรือ SDF text
7. Overlay: room material color จาก metric ramp (viridis/coolwarm), animate ผ่าน `useFrame` lerp
8. Post: `EffectComposer` (SSAO, bloom subtle) toggle ได้; disable บน low‑end (detect ผ่าน FPS probe)

### 6.3 Real‑time delivery
- WS endpoint `/ws?token=` → hub subscribe Redis channel `tenant:{id}:events`
- Client subscribe topics: `device:{id}`, `floor:{id}`, `alerts`
- Server throttle: state updates coalesce ≤ 4 msg/s per device ไปที่ client
- Reconnect + resync ผ่าน `GET /devices/state?since=`

### 6.4 Decoder sandbox
- JS decoders (LoRa codec, custom) รันใน **goja** พร้อม timeout 50 ms, memory limit, ไม่มี network/fs
- Signature: `function decodeUplink(input: {bytes, fPort, timestamp}) => {data: {...}}` (TTN‑compatible เพื่อใช้ codec ของผู้ผลิตได้ตรง ๆ)

---

## 7. Data Contracts

### 7.1 Device Profile (JSON Schema, ย่อ)
```json
{
  "id": "milesight-em300-th",
  "version": 3,
  "brand": "Milesight",
  "model": "EM300-TH",
  "name": { "en": "Temperature & Humidity Sensor", "th": "เซนเซอร์อุณหภูมิและความชื้น" },
  "transport": "lorawan",
  "decoder": { "type": "js", "ref": "s3://profiles/milesight-em300-th/v3/decoder.js" },
  "heartbeat_sec": 1800,
  "metrics": [
    { "key": "temperature", "type": "number", "unit": "°C", "semantic": "temperature", "min": -40, "max": 85 },
    { "key": "humidity", "type": "number", "unit": "%", "semantic": "humidity" },
    { "key": "battery", "type": "number", "unit": "%", "semantic": "battery" }
  ],
  "commands": [
    { "key": "set_interval", "label": { "en": "Report interval" }, "params": [{ "key": "minutes", "type": "int", "min": 1, "max": 1440 }], "encoder": { "type": "js", "fn": "encodeDownlink" } }
  ],
  "onboarding": {
    "fields": [
      { "key": "dev_eui", "type": "hex", "length": 16, "required": true },
      { "key": "app_key", "type": "hex", "length": 32, "required": true, "secret": true }
    ],
    "requires_gateway": "lorawan"
  },
  "ui": { "icon": "thermometer", "model3d": "sensor-box", "default_dashboard": ["gauge:temperature", "gauge:humidity", "line:temperature,humidity", "battery"] },
  "rule_templates": ["temperature_out_of_range", "offline"]
}
```

### 7.2 Canonical TelemetryEvent
```json
{
  "schema": "telemetry.v1",
  "tenant_id": "uuid",
  "device_id": "uuid",
  "profile_id": "milesight-em300-th@3",
  "ts": "2026-09-11T02:15:00.000Z",
  "received_at": "2026-09-11T02:15:00.120Z",
  "metrics": { "temperature": 27.4, "humidity": 61.0, "battery": 92 },
  "meta": { "rssi": -71, "snr": 8.5, "gateway_id": "uuid", "f_cnt": 1201 },
  "quality": "good",
  "raw_ref": "s3://raw/2026/09/11/...."
}
```
กฎ: metric key ต้องตรงกับ profile; ค่าที่ไม่รู้จัก → เก็บใน `meta.unknown` ไม่ทิ้ง

### 7.3 Rule DSL (ย่อ)
```json
{
  "name": "Server room too hot",
  "trigger": { "type": "telemetry", "scope": { "zone_id": "uuid", "semantic": "temperature" } },
  "conditions": { "all": [ { "metric": "temperature", "op": ">", "value": 30, "for_sec": 300 } ] },
  "actions": [
    { "type": "alert", "severity": "high", "dedupe_sec": 1800 },
    { "type": "notify", "channel": "line", "target": "group:ops" },
    { "type": "work_order", "template": "hvac-check" }
  ]
}
```

### 7.4 FloorPlan JSON (vector)
```json
{
  "schema": "floorplan.v1",
  "version": 12,
  "unit": "m", "px_per_m": 50, "height_m": 3.2, "elevation_m": 6.4,
  "background": { "url": "s3://...", "x": 0, "y": 0, "scale": 1, "opacity": 0.4, "locked": true },
  "layers": [ { "id": "walls", "visible": true, "locked": false } ],
  "elements": [
    { "id": "w1", "type": "wall", "points": [[0,0],[12,0],[12,8]], "thickness": 0.2, "layer": "walls" },
    { "id": "r1", "type": "room", "name": "Server room", "polygon": [[0,0],[6,0],[6,4],[0,4]], "color": "#22d3ee" },
    { "id": "d1", "type": "door", "wall_id": "w1", "offset": 3.0, "width": 0.9, "swing": "in" },
    { "id": "z1", "type": "zone", "polygon": [...], "color": "#a78bfa", "purpose": "geofence" },
    { "id": "p1", "type": "device", "device_id": "uuid", "x": 2.5, "y": 1.5, "z": 2.2, "rotation": 0 },
    { "id": "t1", "type": "label", "text": "Rack A", "x": 1, "y": 1, "size": 14 }
  ]
}
```

---

## 8. Permission Matrix (ย่อ)

| Action | owner | admin | operator | viewer |
|---|---|---|---|---|
| Billing / plan | ✔ | – | – | – |
| Manage members | ✔ | ✔ | – | – |
| Add/remove devices | ✔ | ✔ | – | – |
| Edit floor plan / 3D | ✔ | ✔ | – | – |
| Rules & alerts config | ✔ | ✔ | – | – |
| Ack/resolve alerts, work orders | ✔ | ✔ | ✔ | – |
| Send device commands | ✔ | ✔ | ✔ (if allowed per device) | – |
| View dashboards/2D/3D | ✔ | ✔ | ✔ | ✔ |

---

## 9. Database Schema (core tables)

```
tenants(id, name, slug, plan_id, status, created_at)
users(id, email, password_hash, name, locale, tz, created_at)
memberships(user_id, tenant_id, role)
oauth_accounts(user_id, provider, provider_uid)
refresh_tokens(id, user_id, family_id, hash, expires_at, revoked_at)
api_keys(id, tenant_id, hash, scopes[], last_used_at)

plans(id, code, limits jsonb, price_base numeric, currency)
subscriptions(id, tenant_id, plan_id, status, period_start, period_end)
fx_rates(base, quote, rate, source, as_of)

brands(id, name, logo_url)
models(id, brand_id, name, transport, published)
profiles(id, model_id, version, spec jsonb, decoder_ref, published, tenant_id null)   -- tenant_id null = global

sites(id, tenant_id, name, lat, lng, tz)
buildings(id, tenant_id, site_id, name)
floors(id, tenant_id, building_id, name, level, height_m, elevation_m, plan jsonb, plan_version, bg_url)
zones(id, tenant_id, floor_id, name, polygon jsonb, purpose)
gateways(id, tenant_id, site_id, transport, external_id, credentials_enc, status, last_seen_at)
devices(id, tenant_id, profile_id, name, external_id, credentials_enc, gateway_id, status, last_seen_at, tags[])
device_placements(device_id, floor_id, x, y, z, rotation, icon_override)
device_state(tenant_id, device_id, metrics jsonb, updated_at)               -- latest
telemetry(tenant_id, device_id, ts, metric, value_num, value_str, value_json, meta jsonb)  -- hypertable
commands(id, tenant_id, device_id, key, params jsonb, status, requested_by, sent_at, acked_at)

rules(id, tenant_id, name, spec jsonb, enabled)
alerts(id, tenant_id, rule_id, device_id, severity, status, opened_at, acked_by, resolved_at, payload jsonb)
notifications(id, tenant_id, channel, target, payload jsonb, status, attempts)
work_orders(id, tenant_id, title, status, assignee_id, alert_id, checklist jsonb, due_at)

dashboards(id, tenant_id, name, layout jsonb, shared_token)
translations(ns, key, lang, value, version)
audit_logs(id, tenant_id, actor_id, action, target, diff jsonb, ip, at)
```
Indexes: `telemetry(tenant_id, device_id, ts desc)`, `devices(tenant_id, status)`, `alerts(tenant_id, status, opened_at)`

---

## 10. API Surface (REST, prefix `/api/v1`, OpenAPI via swag)

| Group | Endpoints (ย่อ) |
|---|---|
| Auth | `POST /auth/register`, `/auth/login`, `/auth/refresh`, `/auth/logout`, `GET /auth/oauth/{provider}`, `/auth/oauth/{provider}/callback` |
| Tenant | `GET/PATCH /tenant`, `GET/POST/DELETE /tenant/members`, `/tenant/api-keys` |
| Catalog | `GET /catalog/brands`, `/catalog/brands/{id}/models`, `/catalog/profiles/{id}` |
| Devices | `GET/POST /devices`, `GET/PATCH/DELETE /devices/{id}`, `GET /devices/{id}/state`, `GET /devices/{id}/telemetry?from&to&agg`, `POST /devices/{id}/commands`, `GET /devices/{id}/diagnostics` (SSE/WS) |
| Gateways | `GET/POST /gateways`, `GET /gateways/{id}/config` |
| Spatial | `/sites`, `/buildings`, `/floors`, `GET/PUT /floors/{id}/plan` (If‑Match version), `POST /floors/{id}/background`, `POST /floors/{id}/import-dxf`, `/floors/{id}/placements` |
| Rules/Alerts | `/rules`, `/alerts`, `POST /alerts/{id}/ack`, `/resolve` |
| Dashboards | `/dashboards`, `GET /dashboards/{id}/data` |
| Billing | `GET /plans`, `GET/POST /subscription`, `GET /billing/invoices`, `GET /fx/rates` |
| Admin | `/admin/tenants`, `/admin/catalog/...`, `/admin/adapters/health` |
| Ingest | `POST /ingest/http/{device_token}` (webhook adapter), `POST /hooks/emqx/auth`, `/hooks/emqx/acl` |
| WS | `GET /ws` — messages: `subscribe`, `state`, `alert`, `diag`, `position`, `presence` |

---

## 11. Deployment (docker‑compose services, dev)

`postgres(timescaledb)`, `redis`, `emqx`, `chirpstack` + `chirpstack-gateway-bridge`, `minio`, `api`, `worker`, `adapter-mqtt`, `adapter-lorawan`, `adapter-minew`, `rule-engine`, `ws-hub` (อาจรวมใน api ระยะแรก), `web` (Next.js), `grafana`, `prometheus`, `otel-collector`

Prod: Kubernetes, HPA บน api/adapters, Timescale managed หรือ StatefulSet, EMQX cluster, ingress + cert‑manager, per‑tenant subdomain wildcard

---

## 12. Roadmap

| Phase | ระยะ | Scope |
|---|---|---|
| **P0 Foundation** | 4 wk | repo, CI, hexagonal skeleton, RLS, auth (email+Google), tenants, plans (no payment), i18n base, OpenAPI |
| **P1 Connect** | 6 wk | catalog + 20 profiles, adapters mqtt‑generic + lorawan + minew + http, onboarding wizard, diagnostics, telemetry store, device state, WS hub, auto dashboard |
| **P2 Designer** | 6 wk | 2D designer (FR‑2D‑1..8, 11, 13, 14), placements, live overlay |
| **P3 3D** | 5 wk | plan→3D pipeline, stacked floors, device markers, overlays, camera, performance pass |
| **P4 Operate** | 5 wk | rule engine, alerts, LINE/email, data explorer/export, commands, billing payment, Facebook OAuth |
| **P5 Advanced** | ongoing | heatmap, DXF import, glTF import, indoor tracking, work orders, Kafka, shared dashboards, IFC |

---

## 13. Acceptance Criteria (MVP = P0–P4)

1. ผู้ใช้ใหม่สมัครและเห็นข้อมูลจาก Milesight EM300 ผ่าน ChirpStack ภายใน 10 นาที โดยไม่ติดต่อ support
2. Minew S1 ผ่าน MG3 gateway แสดงค่าบน dashboard และบนแปลน 2D พร้อมสถานะ online
3. วาดแปลน 2 ชั้น (≥ 20 ห้อง) วางอุปกรณ์ 50 ตัว → กด 3D → render ภายใน 3 s และ ≥ 45 fps
4. Alert จาก rule ถึง LINE ภายใน 5 s หลัง telemetry เข้า
5. Tenant A ไม่สามารถอ่านข้อมูล tenant B ผ่านทุกช่องทาง (API, WS, MQTT) — พิสูจน์ด้วย automated test
6. UI สลับ th/en ได้ทุกหน้า, ราคาแสดงตามสกุลเงินที่เลือก
7. Ingestion 5,000 events/s บน 1 node โดย p99 store latency < 500 ms

---

## 14. Testing Strategy
- Unit: domain + use cases (Go), decoders (golden payload files ต่อ profile)
- Integration: testcontainers (Postgres+Timescale, Redis, EMQX), RLS negative tests
- Contract: OpenAPI schema validation; adapter contract test suite รันกับทุก adapter
- E2E: Playwright — register→onboard→designer→3D flow; visual regression บน designer/3D snapshot
- Load: k6 ยิง MQTT/HTTP ingest

---

## 15. Open Questions
1. Payment gateway: Stripe (global) vs Omise (ไทย) — เลือกตั้งแต่ P4
2. LNS: self‑host ChirpStack เท่านั้น หรือรองรับ TTN/Helium ด้วย
3. 3D asset style: low‑poly stylized หรือ realistic — กระทบ budget ของ 3D model library
4. Indoor tracking accuracy target (room‑level พอ หรือ ≤ 3 m)
5. **Resolved:** ต้องรองรับ On-premise และ Cloud ด้วย codebase/image ชุดเดียวตั้งแต่ต้น; edge edition และ licensing ยังต้องกำหนดภายหลัง

---

## 16. Instructions for AI Code Generation

เมื่อนำเอกสารนี้ไปสร้างโค้ด ให้ทำตามลำดับ:
1. สร้าง monorepo: `/backend` (Go), `/frontend` (Next.js), `/infra` (compose, k8s), `/profiles`
2. เริ่มจาก P0: migrations (goose) พร้อม RLS ทุกตาราง, domain entities, ports, Fiber routes พร้อม swag annotations, auth flow
3. ทุก use case ต้องมี unit test; ทุก repo ต้องมี RLS test
4. Frontend: generate API client จาก OpenAPI; ห้าม hard‑code string (ใช้ next‑intl)
5. Designer 2D และ 3D ให้แยกเป็น feature module ที่ import ได้อิสระ (ไม่ผูกกับ page)
6. อย่าใส่ logic เฉพาะยี่ห้อใน core — ถ้าจำเป็นให้ไปอยู่ใน adapter หรือ profile เท่านั้น
7. ส่งมอบเป็น PR ทีละ phase พร้อม README การรัน `docker compose up`

## 17. Implementation status — Foundation increment

มี backend foundation ใน `/backend` และ development Compose ใน `/infra` รายละเอียดสิ่งที่มี/ยังไม่มีอยู่ที่ [docs/backend-foundation.md](docs/backend-foundation.md) และ hardware pilot อยู่ที่ [docs/minew-mhs.md](docs/minew-mhs.md)

Foundation นี้ยังไม่ถือว่าผ่าน MVP acceptance: frontend ยังใช้ demo data; PostgreSQL ยังไม่เปิด Timescale; ยังไม่มี MQTT broker/adapter, Minew decoder, WebSocket, email verification/reset และ production deployment hardening ครบ ระบบรับ raw packet จะรายงาน `decoded:false` เสมอจนกว่าจะมี decoder ที่ทดสอบกับ hardware จริง

ข้อยกเว้น identity: global `users` และ user-scoped session/membership ใช้ขอบเขต identity ตามเอกสาร backend; tenant business tables ต้อง FORCE RLS พร้อม composite tenant foreign keys ห้ามใช้ `BYPASSRLS` กับ runtime API


## Confirmed expansion — 2026-09-12

See [dynamic platform scope](docs/platform/dynamic-platform.md) for implementation boundaries and acceptance criteria.

- Customers register, create their own workspace and onboard gateways without editing server/broker configuration.
- Multiple dashboards per workspace and multiple configurable panels per dashboard, with reusable widget templates and versioned published layouts.
- Users design building/floor plans and bind devices to geometry; plans can be rendered in dashboard panels.
- Browser-based fullscreen wall displays use scoped, revocable display access; device model support remains to be checked.
- One device model may have multiple profiles/widget/decoder templates; tenant-private templates can later be submitted to a reviewed community catalog.
- Dynamic decoder authoring starts with bounded declarative rules and fixture testing; arbitrary uploaded code must not execute in the API process.
- Support development without hardware using isolated simulated gateway/device identities and explicit simulated provenance.
- Physical Minew button events must not be presented as relay control capabilities without verified support.
