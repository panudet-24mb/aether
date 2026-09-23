# Aether backend foundation

สถานะ: Foundation ที่รันและทดสอบกับ PostgreSQL จริงได้ ยังไม่ใช่ระบบ production ครบตาม requirement ทั้งหมด

## หนึ่ง codebase สำหรับ Cloud และ On-premise

`backend/` สร้าง image `aether-backend:local` ชุดเดียว เปลี่ยน `DEPLOYMENT_MODE=cloud|onprem` ผ่าน configuration ไม่ fork โค้ดตามลูกค้า การ login ด้วยรหัสผ่าน, JWT verification, PostgreSQL และ ingest ไม่เรียกบริการภายนอกเมื่อรันแล้ว การ build/install ครั้งแรกยังต้องดาวน์โหลด dependency และ image; air-gapped installation ต้องเตรียม image และ CA ล่วงหน้า

On-premise ปิด public registration เสมอ ใช้คำสั่ง admin ที่รันในเครื่องเพื่อตั้ง owner ครั้งเดียว Cloud ปิด registration เป็นค่าเริ่มต้นเช่นกัน จนกว่าจะเพิ่ม email verification; endpoint register ที่มีอยู่ใช้ทดสอบ development/integration เท่านั้น

## สิ่งที่มีแล้ว

- Go 1.26.8, Fiber v3, GORM และ goose SQL migrations แยก process จาก API
- Argon2id: 64 MiB, 3 iterations, parallelism 2; password 12–128 bytes; random salt
- จำกัด concurrent password hashing ที่ 2 งานต่อ process และจำกัด request body/เวลา request
- JWT access อายุสูงสุด 15 นาที ตรวจ algorithm, signature, issuer, audience, expiry และ UUID claims
- Refresh token สุ่ม 256 bits เก็บเฉพาะ SHA-256 digest ในฐานข้อมูล หมุนทุกครั้ง อายุ family สูงสุด 30 วันแบบไม่ต่ออายุไม่สิ้นสุด
- ล็อก transaction เมื่อ refresh; เมื่อ token เก่าถูกใช้ซ้ำ เพิกถอนทั้ง family และ commit การเพิกถอนก่อนคืน error
- ทุก authenticated request ตรวจ session, membership และ tenant status ปัจจุบันอีกครั้ง ทำให้ logout และการลดสิทธิ์มีผลกับ access token เดิม
- Refresh cookie: HttpOnly, SameSite=Strict, Secure และชื่อ __Host- ใน production; ตรวจ exact Origin สำหรับทุก auth mutation
- ไม่มี credentials ใน URL หรือ request logs; gateway token แสดงครั้งเดียวและเพิกถอนได้
- API ใช้ DB role ที่ไม่มี superuser/BYPASSRLS/DDL/สิทธิ์ owner; startup ตรวจบทบาทและ RLS
- Tenant tables ใช้ ENABLE + FORCE RLS; context ถูกตั้งด้วย parameterized set_config(..., true) ภายใน transaction เท่านั้น
- Composite foreign keys ป้องกันอุปกรณ์อ้าง gateway ข้าม tenant
- Gateway routing ได้จาก credential ที่ตรวจแล้ว ไม่เชื่อ tenant_id ใน gateway payload
- Raw packet capture สูงสุด 100 packets ต่อ gateway, API อ่านได้สูงสุด 20; เฉพาะ owner/admin
- Generic telemetry ingest, dedupe ตาม device/timestamp, latest state ไม่ถอยหลังเมื่อ packet เก่ามาถึง และเก็บย้อนหลังสูงสุด 10,000 readings ต่ออุปกรณ์
- Audit log การสร้างบัญชี/session/gateway/device, logout และเพิกถอน gateway
- Development Compose bind PostgreSQL/API เฉพาะ 127.0.0.1; container API non-root, read-only, cap_drop ALL

## ข้อยกเว้นข้อมูล identity ที่ต้องเข้าใจ

`identity.users` เป็น global identity ไม่ผูก tenant เพราะคนเดียวอยู่ได้หลายองค์กร ตารางนี้ไม่ใช้ tenant RLS แต่จำกัดการเข้าถึงที่ authentication repository ไม่มี API สำหรับอ่าน users ทั้งหมด และ password hash ไม่ถูก serialize

`identity.sessions` และ `identity.refresh_tokens` ใช้ user-scoped RLS ส่วน memberships เปิดให้อ่านเฉพาะ memberships ของ user ปัจจุบัน เพื่อเลือกองค์กรขณะ login การอ่าน telemetry, gateway, device และ audit ใช้ tenant-scoped RLS

`core.lookup_gateway` เป็น fixed SQL SECURITY DEFINER function ที่ตั้ง search_path=pg_catalog และรับเฉพาะ gateway UUID กับ token digest เพื่อคืน tenant UUID เท่านั้น owner เป็น NOLOGIN และ API ไม่เป็นสมาชิก owner role หลัง lookup จะตรวจ tenant ยัง active ด้วย runtime role ตามปกติ ห้ามขยายฟังก์ชันนี้เป็น dynamic SQL หรือ query ทั่วไป

## ขอบเขตที่ยังต้องทำก่อน production

- หน้า frontend ที่เผยแพร่ก่อนหน้ายังเป็น demo และยังไม่เรียก API ชุดนี้
- ยังไม่มี email verification/reset, invitations, OAuth, API keys, MFA หรือ UI login จริง
- เพิ่ม Mosquitto MQTT/TLS adapter และ broker ACL แล้ว ดู [สถานะ MQTT](mqtt.md); ยังไม่มี verified Minew BLE decoder และรอ MG3 cutover ผ่าน iOS
- ฐานข้อมูล foundation เป็น PostgreSQL ปกติ ยังไม่มี Timescale hypertable/continuous aggregates/per-plan retention, Redis/Kafka EventBus หรือ WebSocket fan-out
- จำนวน gateway 50/device 100 ต่อ tenant และ history caps เป็นขีดจำกัด foundation ยังไม่ใช่ configurable billing plans
- Rate limit ของ `/api` ตั้งได้ด้วย `API_RATE_LIMIT` (ค่าเริ่มต้น 120 ครั้ง/นาที/IP; dev compose ใช้ 600 เพราะ browser, test และ curl ใช้ loopback IP เดียวกัน)
- Rate limit เป็นต่อ process และใช้ socket IP; **ทำแล้ว** สำหรับ single-server: `infra/prod/` ตรึง IP ของ proxy ด้วย subnet คงที่และตั้ง `TRUSTED_PROXIES` ให้ตรงตัว โดย Caddy เขียนทับ `X-Forwarded-For` ด้วย IP จริง (ยืนยันด้วยการทดสอบปลอม header แล้วยังชน rate limit) — **ยังไม่ได้ทำ** shared rate limiting สำหรับหลาย replica
- ยังไม่ได้ load-test 5,000 events/s, external penetration test หรือรับรอง OWASP ASVS/PDPA; ห้ามอ้างว่าผ่านมาตรฐานเหล่านี้จาก unit tests
- Deployment gate (ดู [production runbook](production.md)):
  - **ทำแล้ว** TLS ingress ผ่าน Caddy (โหมด internal CA สำหรับ LAN หรือ ACME สำหรับ DNS สาธารณะ) พร้อม HSTS/CSP/security headers ที่ทดสอบกับเบราว์เซอร์จริงแล้ว
  - **ทำแล้ว** DB TLS `sslmode=verify-full` จริงทุก client (api, migrate, mqtt-ingest, mqtt-provisioner, backup) และ `pg_hba.conf` บังคับ `hostssl`
  - **ทำแล้ว** backup อัตโนมัติรายคืน (`pg_dump --format=custom` + `pg_restore --list` + retention 14 daily/8 weekly), สคริปต์ restore ที่ปฏิเสธการเขียนทับ database ที่ไม่ว่าง และ checklist ซ้อมกู้คืน
  - **ยังไม่ได้ทำ** PITR/WAL archiving — กู้ได้เฉพาะจุดที่ทำ dump
  - **ยังไม่ได้ทำ** rotation/cleanup ของ sessions และ audit retention แบบอัตโนมัติ
  - **ยังไม่ได้ทำ** monitoring/alerting ของตัวระบบเอง (runbook ให้เป็น checklist ที่ต้องตรวจเอง ไม่มี exporter/dashboard)
  - **ยังไม่ได้ทำ** signed offline release
  - **ยังไม่ได้ทำ** upgrade rollback อัตโนมัติ — migration มี Down section แต่ `cmd/migrate` เรียกเฉพาะ `goose.Up` ทางกลับคือกู้ dump ก่อนอัปเกรดเท่านั้น
- สถานะอุปกรณ์ offline ต้องเพิ่ม heartbeat scheduler; packet ที่รับได้ไม่เท่ากับ decode สำเร็จหรือระบุตำแหน่งได้

## API ที่ใช้งานได้

| Endpoint | Credential | ผลลัพธ์ |
|---|---|---|
| GET /health/live, /health/ready | ไม่ต้อง | process/DB readiness |
| POST /api/v1/auth/register | exact Origin; registration เปิดใน dev | user + tenant |
| POST /api/v1/auth/login | exact Origin; email/password | access token + HttpOnly refresh cookie |
| POST /api/v1/auth/refresh | exact Origin + refresh cookie | rotated token pair |
| POST /api/v1/auth/logout | exact Origin + access token | revoke session |
| GET /api/v1/me | Bearer access | current tenant/role/mode |
| GET/POST /api/v1/gateways | Bearer; POST owner/admin | gateway + token ครั้งเดียว |
| POST /api/v1/gateways/{id}/revoke | Bearer owner/admin | revoke credential |
| GET /api/v1/gateways/{id}/packets | Bearer owner/admin | latest raw JSON, decoded:false |
| GET/POST /api/v1/devices | Bearer; POST owner/admin | registered devices |
| GET /api/v1/devices/{id}/state | Bearer | latest stored metrics |
| POST /ingest/gateways/{id}/packets | Basic gateway UUID:token | raw JSON capture, decoded:false |
| POST /ingest/gateways/{id}/telemetry | Basic gateway UUID:token | generic canonical telemetry.v1 |

ทุก JSON mutation นอกจาก raw packet capture ปฏิเสธ field ที่ไม่รู้จัก; JSON body ไม่รับ tenant_id จากผู้เรียก `Content-Type: application/json` จำเป็น

## Verification — 2026-09-11

ทดสอบกับ PostgreSQL 18.6 ใน Docker และ Go 1.26.8:
- `go test -race -count=1 ./...` ผ่าน รวม integration ไม่ได้ skip ในการตรวจรอบนี้
- `go vet ./...` ผ่าน
- `govulncheck v1.8.0` ไม่พบช่องโหว่ใน call paths หรือ packages ที่ import; พบ advisory ระดับ module GO-2026-5932 ใน `golang.org/x/crypto/openpgp` ซึ่งไม่ได้ import/ใช้งาน (Aether ใช้ argon2) ไม่มี fixed version สำหรับ deprecated package นี้ ไม่ใช่การรับรองว่าปลอดภัยจากทุกช่องโหว่
- สร้าง Docker backend image สำเร็จ; container API ใช้ UID 10001 และ runtime DB role aether_app
- ยังไม่ได้ต่อ gateway จริงหรือทดสอบบน Cloud production และยังไม่มี browser E2E สำหรับ login/real data

OpenAPI 3.0.3 contract อยู่ที่ `backend/api/openapi.json` และ API ให้บริการที่ `GET /openapi.json` เป็น contract ที่เขียนสำหรับ foundation โดยตรง ยังไม่ได้ generate จาก swag annotations

Container smoke checks ผ่านทั้ง `DEPLOYMENT_MODE=onprem` และ `cloud` ด้วย image ID เดียวกัน (`sha256:c036662ded1c320a6dd51251f0d5a9a15811fa7b718ba7771be0d5d239ab7e4e`) โดยทั้งสองตอบ readiness สำเร็จ ทดสอบในเครื่อง ไม่ใช่การ deploy Cloud production ปิดและลบ container cloud-check ชั่วคราวแล้ว ส่วน API หลักและ PostgreSQL development ยังคงเปิดอยู่
