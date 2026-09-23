## SOS และการสอนสัญญาณ · 2026-09-22

กดปุ่มฉุกเฉินแล้วขึ้นทันทีโดยไม่ต้องตั้งค่า workspace ใหม่ทุกที่มีกฎ **ปุ่มฉุกเฉิน SOS** ติดมา พร้อมแถบสีแดงที่ปิดไม่ได้จนกว่าจะกดรับทราบ เสียงเตือนซ้ำ และจุดกะพริบบนผังอาคาร · อุปกรณ์ที่คู่มือไม่ตรงกับของจริงใช้ **สอนสัญญาณ** ได้ ปล่อยนิ่ง 20 วินาที กดย้ำ ๆ 20 วินาที แล้วระบบหาสัญญาณที่ต่างให้เอง ดู [Hardware bring-up](docs/hardware-bringup.md) และ [Alerts](docs/platform/alerts.md)

## Production readiness · 2026-09-20

ชุดติดตั้ง production แบบ on-premise (Caddy TLS, Postgres TLS verify-full, MQTT TLS, backup/restore) อยู่ใน `infra/prod/` พร้อมคู่มือ [Production runbook](docs/production.md) และขั้นตอนต่ออุปกรณ์จริง [Hardware bring-up](docs/hardware-bringup.md) · หน้า Live monitoring ถูกแทนด้วย [ภาพรวม](docs/live-dashboard.md) · สมาชิกทีมและสิทธิ์ตามโปรเจคบังคับด้วย RLS ดู [Team access](docs/platform/team-access.md) · ผังอาคารนำเข้า DXF ได้ · Automation รองรับ 500 ผัง · ตัวเลือกใหม่: `ALERTS_SHADOW`, `SAMPLE_RETENTION_DAYS`, `SAMPLE_MIN_INTERVAL_SEC`, `DISCOVERY_LIMIT`, `CHANNEL_SEAL_KEY`, `TRUSTED_PROXIES`, `WEBHOOK_ALLOWED_HOSTS`

## Floor plans, asset registry, stable zones · 2026-09-20

หน้า **ผังอาคาร** ออกแบบอาคาร ชั้น โซน ผนัง และวาง gateway/อุปกรณ์ได้ทั้ง 2D และ 3D พร้อมเห็น wearable ตามโซนแบบ real-time ดู [Floor plans](docs/platform/floorplans.md) หน้า **อุปกรณ์ทั้งหมด** รวม gateway และอุปกรณ์ทุกตัวพร้อมข้อมูลทรัพย์สิน แผน MA/PM และประวัติ ดู [Asset registry](docs/platform/assets.md) ตำแหน่ง wearable ใช้ RSSI เฉลี่ยกับเกณฑ์ 6 dB นาน 10 วินาทีจึงไม่กระพริบ และมีเหตุการณ์กับกฎ “เข้าโซน” ดู [Wearables roaming](docs/platform/wearables-roaming.md) **Automation Studio** สร้างผัง “ถ้า…แล้ว…” ด้วยการลากวาง block ข้ามอุปกรณ์ทั้ง workspace ทำงานจริงกับการเปิด alert และส่งแจ้งเตือน ส่วนการสั่งงานอุปกรณ์ยังไม่เปิดเพราะยังไม่มีช่องทาง downlink ดู [Automation Studio](docs/platform/automation.md)

## Wearables and a leaner connect page · 2026-09-20

อุปกรณ์สวมใส่เปิดโหมด **roaming** ได้: ลงทะเบียนครั้งเดียว แล้วตามค่าและตำแหน่งจาก gateway ทุกตัวที่ได้ยิน มี widget ตำแหน่ง wearable และการ์ด wearable พร้อม gateway จำลองโซน B ที่ wearable เดินไปมา แถบบนของหน้า “เชื่อมต่ออุปกรณ์” เหลือสองแถวบาง ดู [Wearables roaming](docs/platform/wearables-roaming.md)

## Projects and real-time · 2026-09-20

แยก gateway และอุปกรณ์เป็นหลาย **โปรเจค** ได้ในหน้า “เชื่อมต่ออุปกรณ์”, Dashboard Studio เลือกโปรเจคมาออกแบบและดึงอุปกรณ์ข้ามโปรเจคได้ และหน้าเว็บรับการเปลี่ยนแปลงแบบ real-time ผ่าน WebSocket พร้อมแถบเตือนเมื่อมี alert ระดับ critical ดู [Projects and real-time](docs/platform/projects-realtime.md)

## Kit widgets and product photos · 2026-09-20

Dashboard Studio มี widget Official เพิ่ม 6 ตัว (motion, tamper, ปุ่มฉุกเฉิน, beacon, ไทม์ไลน์เหตุการณ์, การแจ้งเตือนที่เปิดอยู่) กรองตามชนิดอุปกรณ์ และใช้ภาพสินค้าจริงจากผู้ผลิตในหน้า topology/catalog ดู [Widget Studio](docs/platform/widget-studio.md)

## Device lifecycle · 2026-09-20

อุปกรณ์ที่ adopt แล้วเปลี่ยนชื่อ ย้าย gateway (ลากปลายเส้นบน canvas ได้) ยกเลิกการลงทะเบียนแบบเก็บประวัติ และกู้คืนได้ ดู [Device topology](docs/platform/topology.md)

## Alerts · 2026-09-20

มี event log ถาวร, กฎแจ้งเตือนต่อ workspace (tamper, กดปุ่ม, น้ำรั่ว, เคลื่อนไหว, offline, ค่าเกินเกณฑ์), alert lifecycle open → acknowledged → resolved และการส่งแจ้งเตือนผ่าน webhook (HMAC), LINE Messaging API และอีเมล พร้อมหน้า "การแจ้งเตือน" ดู [Alerts](docs/platform/alerts.md)

## Minew MHS kit simulation · 2026-09-20

decoder รองรับเฟรมของอุปกรณ์ทั้งชุด MHS (S1, C10, B7, B10, E8S, MBT01: FFE1 0xA1 หลายชนิด + iBeacon/Eddystone), catalog รุ่นอุปกรณ์เปิดที่ `GET /api/v1/catalog`, simulator ส่งครบทั้งชุด และหน้า topology แสดงชนิด/เหตุการณ์ (tamper, กดปุ่ม, เคลื่อนไหว) ยืนยันกับเครื่องจริงแล้วเฉพาะ S1 ดู [Minew kit](docs/platform/minew-kit.md)

## Device topology · 2026-09-18

หน้า “เชื่อมต่ออุปกรณ์” เป็น canvas แบบ network diagram แล้ว: ลาก gateway/อุปกรณ์จากแถบซ้ายมาวาง โยงเส้นอุปกรณ์ → gateway เพื่อลงทะเบียนผ่าน API ได้ทันที เลือกแบรนด์/รุ่นอิสระจาก gateway ดูสถานะ MQTT, ค่าตั้งค่า และค่าล่าสุดในแถบขวา ดู [Device topology](docs/platform/topology.md) สำหรับขอบเขตและสิ่งที่ยังไม่มี (ลบ/ย้ายอุปกรณ์ที่ลงทะเบียนแล้ว, ตำแหน่ง node เก็บใน browser)

## Custom widget history · 2026-09-18

Widgets can decode persisted BLE history using their own bundled JavaScript, with 1h/24h/7d filtering and skipped-frame counts. Collection works without an open Dashboard. Development retention is the newest 10,000 observations per gateway; it starts with migration 00004.

## Widget Studio · 2026-09-14

Brand → model → visual HTML/CSS/JS template → device → dashboard. Widget packages bundle JS decoding and visual rendering. Private/community catalog, full-card dragging, corner resizing, persisted layouts and authenticated kiosk links are available locally. See [Widget Studio](docs/platform/widget-studio.md) for runtime contracts and current limits. Device thresholds are a separate legacy setting.

> Saved sensor history and reusable template versions are now available locally. See [history and templates](docs/platform/persistent-history.md).

> **พัฒนาที่บ้าน:** มี gateway จำลอง SIM 4 เซนเซอร์แล้ว ดู [คู่มือ simulation](docs/platform/simulation.md) และ [แผน configurable platform](docs/platform/dynamic-platform.md)

> **Live dashboard พร้อมใช้งาน:** เปิด http://localhost:3001/ เพื่อดูค่า Minew จริง เข้าสู่ระบบด้วย Owner; รหัสอยู่ใน `.secrets/owner-password.txt` รายละเอียดใน [Live dashboard](docs/live-dashboard.md) หน้า prototype อยู่ `/demo`

# Aether

IoT platform ที่ตั้งใจใช้ codebase เดียวทั้ง Cloud และ On-premise

- `frontend/` — UI prototype ที่มีอยู่ ใช้ข้อมูลจำลองและ Sites runtime
- `backend/` — Go API foundation, auth, tenant isolation และ authenticated ingress
- `infra/` — development Docker Compose และ local tooling
- [รายละเอียด backend](docs/backend-foundation.md)
- [เชื่อม Minew MHS](docs/minew-mhs.md)
- [ข้อกำหนดผลิตภัณฑ์](requirement.md)

## เปิด backend ในเครื่อง

ต้องมี Docker Compose; ถ้ารัน Go โดยตรงใช้ Go 1.26.8 ตาม toolchain ใน go.mod

```sh
# รันครั้งเดียว สร้าง .env แบบ 0600 และไม่พิมพ์ secrets
python3 infra/generate-env.py
# ถ้ามี .env ที่สร้างไว้แล้ว ให้ใช้ไฟล์เดิม ไม่ต้องสร้างทับ

docker compose --env-file .env -f infra/compose.yaml up -d --build
```

API: `http://localhost:8080/health/ready` และ PostgreSQL bind เฉพาะ loopback พอร์ต 55432 ไม่เปิดสู่ LAN/Internet

ไฟล์ Compose นี้เป็น development เท่านั้น ปิด registration ทั้ง Cloud และ On-premise เลือก mode ผ่าน DEPLOYMENT_MODE โดยไม่เปลี่ยนโค้ด ใน production API บังคับ APP_ORIGIN แบบ HTTPS และ DB sslmode=verify-full; ดู deployment gates ในเอกสาร backend

## สร้าง owner ครั้งแรก

เตรียมไฟล์รหัสผ่านความยาว 12–128 bytes ที่อ่านได้เฉพาะเจ้าของ (0600) นอก source control แล้วกำหนด environment ต่อไปนี้ใน terminal ของคุณ:

```sh
export ADMIN_EMAIL='you@example.com'
export ADMIN_NAME='Your name'
export TENANT_NAME='Your organization'
export ADMIN_PASSWORD_FILE='/absolute/path/to/private-password-file'
python3 infra/backend-local.py bootstrap
```

คำสั่ง bootstrap ทำได้เฉพาะเมื่อระบบยังไม่มี user เท่านั้น ไม่มี endpoint bootstrap ผ่าน HTTP และไม่สร้างรหัสผ่านเริ่มต้นที่รู้ร่วมกัน

## ทดสอบ

ชุดทดสอบ integration ใช้ฐานข้อมูล `aether_test` แยกจากข้อมูลใช้งาน ต้องสร้างฐานข้อมูลนี้ก่อนครั้งแรก:

```sh
docker compose --env-file .env -f infra/compose.yaml exec -T postgres \
  psql -U postgres -d aether -v ON_ERROR_STOP=1 \
  -c 'CREATE DATABASE aether_test OWNER aether_owner;'
python3 infra/backend-local.py test
```

Runner ตั้งค่า credential ผ่าน environment โดยไม่พิมพ์ค่า และรัน `go test -race` ครอบคลุม auth, CSRF origin, roles, RLS/pooled transaction, FK ข้าม tenant, refresh replay/race, gateway revocation และ out-of-order ingestion

การเรียก `go test ./...` โดยไม่มี TEST_DATABASE_URL จะข้าม integration อย่างชัดเจน; runner ด้านบนเป็นวิธีตรวจครบ CI workflow อยู่ใน `.github/workflows/backend.yml`

Frontend ที่เผยแพร่ยังเป็น demo ไม่ใช่หน้า login ของ backend; รอบต่อไปคือยืนยัน Minew packet และเชื่อม UI เข้ากับ API จริง อ่านสถานะที่มี/ยังไม่มีในเอกสาร backend ก่อนนำไปเปิดรับผู้ใช้

## MQTT ของ Aether

เพิ่ม broker TLS และตัวรับเข้า backend แล้ว ดู [การตั้งค่า MG3 และสถานะทดสอบ](docs/mqtt.md) ปลายทาง LAN `192.168.1.64:8883` ผ่านการทดสอบ synthetic; รอเปลี่ยนปลายทาง MG3 ผ่าน iOS เพื่อยืนยัน packet จริง
