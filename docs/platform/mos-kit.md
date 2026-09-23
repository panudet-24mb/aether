# Minew MOS smart-office kit บน Aether

เอกสารนี้บอกตรง ๆ ว่า Aether รองรับอะไรในชุด MOS แล้ว รองรับที่ระดับความมั่นใจไหน และอะไรที่ยังไม่รู้จนกว่าจะได้เฟรมจากเครื่องจริง
วันที่เขียน: 2026-09-23 · migration ที่เกี่ยวข้อง: `backend/migrations/00026_mos_kit.sql`

## 1. ในชุดมีอะไร

| อุปกรณ์ | หน้าที่ | catalog id ใน Aether |
|---|---|---|
| **MG4** Smart Rechargeable Gateway | BLE → Wi‑Fi, อัปโหลดทาง MQTT หรือ HTTP, รูปแบบข้อมูล **JSON‑Long เท่านั้น** (เป็นค่าเริ่มต้นและตัวเลือกเดียว), buffer offline 1,080 records, ~75 packets/s | `minew-mg4` |
| **S1** | อุณหภูมิ / ความชื้น (FFE1 A1‑01) | `minew-s1-pending@1` |
| **MSP01** | PIR ตรวจคนในห้อง | `minew-msp01-pending@1` |
| **C10** | card beacon | `minew-c10-pending@1` |
| **MBT01** | anti-removal tag (A1‑20 tamper) | `minew-mbt01-pending@1` |
| **S4** | door sensor แม่เหล็ก (CR2032, advertise ทุก 1000 ms) | `minew-s4-pending@1` |

`GET /api/v1/catalog` คืนค่าทั้งหมดนี้ พร้อมฟิลด์ใหม่ `occupancy` (MSP01) และ `door` (S4) ใน device profile

## 2. รองรับอะไรแล้ว ที่ระดับไหน

"ยืนยันแล้ว" แปลว่ามีแพ็กเก็ตจากเครื่องจริงผ่าน golden test ใน repo นี้ นอกนั้นเขียนจากเอกสารสาธารณะและ simulator

| อุปกรณ์ | เฟรม | ถอดรหัสได้? | ยืนยันกับเครื่องจริง? | หมายเหตุ |
|---|---|---|---|---|
| MG4 | JSON‑Long (`[{"type":"Gateway",...},{mac,rawData,rssi,timestamp,...}]`) | ได้ แยกทีละแถว | ไม่ | parse แบบเดียวกับ MG3 (barnowl-minew ก็ทำแบบนี้) `timestamp` ที่เป็นสตริง ISO‑8601 และฟิลด์เสริม (`bleName`, `battery`, `temperature`, `humidity`) ถูกข้าม แถวที่ผิดรูปเสียเฉพาะแถวนั้น |
| S1 | FFE1 A1‑01 อุณหภูมิ/ความชื้น | ได้ | **ได้** | เฟรมเดียวของชุดที่ยืนยันแล้ว |
| S1 | FFE1 A1‑08 info | ได้ | ไม่ | ใช้แนะนำ profile จากชื่อรุ่น |
| MSP01 | FFE1 A1‑11 PIR (`motion` 0/1) | ได้ | **ไม่** | เครื่องจริง `c30000161ebb` ส่งเฟรมนี้จริง แต่ `motion=0` ทั้ง 257 ตัวอย่าง (2026-09-21) — ยังไม่เคยเห็นค่า 1 |
| MSP01 | A1‑01, A1‑03, A1‑08 (ชื่อ "MSP01") | ได้ | เห็นจากเครื่องจริงแล้ว ยังไม่มี golden | A1‑08 ใช้แนะนำ profile `minew-msp01-pending@1` |
| C10 | iBeacon / A1‑03 / A1‑08 / TLM | ได้ | ไม่ | ปุ่มบนบัตรยังไม่มีเอกสารรูปแบบเฟรม |
| MBT01 | iBeacon / A1‑20 tamper | ได้ | ไม่ | บิต tamper มาจากตารางสาธารณะ |
| S4 | combination frame (door/tamper/counters) | **ไม่** (โดยเจตนา) | ไม่ | layout ไบต์ไม่เปิดเผย สถานะประตูมาจาก "สอนสัญญาณ" เท่านั้น |
| S4 | INFO frame | ถ้าเป็น A1‑08 มาตรฐาน: ได้ | ไม่ | ยังไม่รู้ว่า S4 ใช้ A1‑08 จริงหรือไม่ |

Profile ใหม่ทั้งหมดเป็น `Verified: false`

## 3. ตั้งค่า MG4

MG4 ส่งได้สองทาง เลือกทางเดียว

**ทาง A — MQTT over TLS (แนะนำ, ใช้เส้นทาง onboarding เดิมของ MG3)**

1. ในหน้าเว็บ Aether สร้าง gateway รุ่น **Minew MG4** แล้วกดออก credential MQTT (`POST /api/v1/gateways/<id>/mqtt`) — username/password **ต่อ gateway** แสดงครั้งเดียว
2. ตั้งใน MG4:
   - Data format: **JSON‑Long** (MG4 มีแบบเดียว)
   - Broker: `mqtts://<host>:8883` อัปโหลด CA ของ broker และห้ามปิดการตรวจใบรับรอง
   - Client id / username / password: ตามที่หน้าเว็บให้
   - Publish topic: `/aether/gateways/<id>/status` (ค่าเริ่มต้นของ MG4 คือ `minew/<gateway-mac>/status` **ต้องเปลี่ยน** เพราะ broker อนุญาตเฉพาะ topic ของ gateway นั้น)
3. ทดสอบว่า MG4 ปฏิเสธ CA ที่ผิดจริง ตามขั้นตอนใน [`docs/hardware-bringup.md`](../hardware-bringup.md) ข้อ 1

**ทาง B — HTTP (ผ่านเส้นทาง Generic HTTP)**

```sh
POST https://<host>/ingest/gateways/<id>/packets
Content-Type: application/json
Authorization: Basic base64("<id>:<token>")
```

`<token>` คือ token ที่แสดงครั้งเดียวตอนสร้าง gateway ตัว endpoint จำกัดอัตราไว้ 120 requests/นาที (upload interval 5 วินาทีจึงเหลือเฟือ)

**ทั้งสองทาง**

- Upload interval: **5 วินาที** เป็นค่าที่แนะนำ (ถี่พอให้ประตู/PIR ทันเหตุการณ์ และไม่ท่วม retention 100 packet ต่อ gateway)
- NTP: **ไม่จำเป็น** Aether ใช้เวลาที่ server ได้รับ (`received_at`) เสมอ ไม่เคยเชื่อนาฬิกาของอุปกรณ์ `timestamp` ของ MG4 จะเป็น epoch หรือ ISO‑8601 ก็ไม่มีผล
- Scan filter: ปิดไว้ก่อนตอน bring-up แล้วค่อยเปิดตาม MAC ของแท็กที่ลงทะเบียนแล้ว

ทดสอบโดยไม่ต้องมีเครื่อง:

```sh
cd backend
go run ./cmd/simulator sample-mos 20      # uplink แบบ MG4 หนึ่งก้อน (timestamp เป็น ISO-8601)
SIMULATOR_KIT=mos SIMULATOR_CONFIG_FILE=... go run ./cmd/simulator   # publish ชุด MOS ต่อเนื่อง
```

ประตูใน simulator เป็นเฟรม **สังเคราะห์** (FFE1 A1 version 0x23 ที่ decoder ไม่รู้จัก เรียงฟิลด์ตาม SDK) ใช้ทดสอบเส้นทาง "สอนสัญญาณ" เท่านั้น ไม่ใช่ layout จริงของ S4

## 4. การตรวจคนในห้อง (MSP01)

- **`occupied`** เกิดเมื่อ `motion` เปลี่ยนเป็น 1 ครั้งแรกของ "ช่วงที่มีคน" (episode) motion ที่มาซ้ำระหว่าง episode เดียวกันไม่สร้าง event ใหม่
- **`vacant`** เกิดเมื่อไม่มี `motion=1` เลยนาน **300 วินาที** (`alerts.OccupancyHoldSec`) ตัดสินโดย worker ที่รันทุก 15 วินาที (`ScanVacancy`) **ไม่ใช่ uplink** เพราะ PIR ที่ไม่เห็นใครมักเงียบ ไม่มีแพ็กเก็ตไหนบอกว่า "ห้องว่าง" — เกิดครั้งเดียวต่อ episode
- เวลาของ motion ครั้งล่าสุดคือเวลาที่ server ได้รับ (`core.stream_state.last_motion_at`)
- กฎแจ้งเตือนชนิด **`occupancy`** ยิงเมื่อเกิด `occupied` ใส่ช่วงเวลาได้ เช่น "มีคนเคลื่อนไหวในออฟฟิศตอนกลางคืน":

```json
{"name":"มีคนในออฟฟิศนอกเวลา","event_type":"occupancy","severity":"warning",
 "scope":{"external_ids":["c30000161ebb"],"after_hours":{"from":"19:00","to":"07:00","days":[1,2,3,4,5]}}}
```

`after_hours` คิดตามเวลา Asia/Bangkok · `from > to` คือข้ามเที่ยงคืนและนับเป็นของวันที่เริ่ม (`days` 0 = อาทิตย์ … 6 = เสาร์, ว่าง = ทุกวัน) · `from == to` คือทั้งวัน · ใช้ได้กับกฎ `door` และ `occupancy` เท่านั้น

**ข้อควรระวัง:** เครื่องจริงยังไม่เคยรายงาน `motion=1` ต้องทดสอบโดยเดินผ่านหน้าเซนเซอร์ (`capture-golden.py --scenario msp01`) ก่อนเชื่อกฎนี้

## 5. ประตู (S4) — สอนก่อนใช้

Aether **ไม่มี decoder ของ S4** เพราะไม่รู้ layout ไบต์ การเดาผิดจะกลายเป็น false door/tamper alarm แทนที่จะเดา ระบบใช้ "สอนสัญญาณ" ที่มีอยู่แล้ว

1. S4 จะขึ้นในหน้า "เชื่อมต่ออุปกรณ์" โดยมีฟิลด์ `unknown` (เช่น `ffe1:a1:0x??:len=..`) = ระบบได้ยินแต่ยังไม่มีกฎ
2. ลงทะเบียนเป็น **Minew S4 · Door sensor** (ไม่บังคับ แต่ทำให้ใช้ "ใช้กับทุกตัวที่เป็นรุ่นนี้" ได้)
3. สอนสัญญาณแบบ **ประตู** (`door`): ขั้นที่ 1 ปิดประตูนิ่ง ๆ → ขั้นที่ 2 เปิดค้าง → เลือก candidate ที่เป็นไบต์ `0x00 → 0x01` (ไม่ใช่ตัวนับที่เพิ่มทีละ 1)
4. จากนั้นทุก uplink ที่มีเฟรมนั้น: ตรง = `door=1` (เปิด), ไม่ตรง = `door=0` (ปิด) ค่าถูกบันทึกลง reading (kind `door`) ให้ UI แสดงสถานะปัจจุบัน และ event log ได้ **`door_open`** / **`door_closed`** แบบ edge-triggered (เหมือน tamper/tamper_cleared) พร้อม `learned: true` และ `signal_id` · uplink ที่ไม่มีเฟรมนั้นไม่เปลี่ยนสถานะ
5. กฎแจ้งเตือนชนิด **`door`** ยิงเมื่อ `door_open` และใส่ `after_hours` ได้เช่นเดียวกับ occupancy

Metric ที่ contract จองไว้: `door` (1 เปิด / 0 ปิด) และเมื่อรู้ layout จริงแล้ว `door_open_count`, `door_close_count`, `tamper` — วันนี้มีเพียง `door` จากสัญญาณที่สอน

## 6. สิ่งที่ยังไม่รู้จนกว่าจะได้เฟรมจริง

- **S4**: layout ไบต์ของ combination frame, service UUID, frame type/version byte, ตำแหน่งและความหมายของ `doorSensorAlarmStatus` / `tamperProofAlarmStatus` / `triggerAlarmStatus`, ตัวนับ `openCount` / `closeCount` / `tamperProofCount` เป็นไบต์เดียวหรือไม่ และ S4 ส่ง INFO เป็น A1‑08 มาตรฐานหรือไม่
- **S4 tamper**: ยังไม่มีทางเห็นสถานะกันงัดจนกว่าจะสอนแยก (`tamper`) หรือมี decoder
- **MSP01**: บิต 0 ของ A1‑11 คือ PIR จริงหรือไม่ (ยังไม่เคยเห็นค่า 1), ระยะเวลาที่ `motion` ค้างเป็น 1 หลังตรวจพบ, และ 300 วินาทีเหมาะกับห้องจริงหรือไม่
- **MG4**: ยังไม่มี uplink จริงใน repo — รูปแบบ ISO timestamp, ชื่อฟิลด์เสริม, และพฤติกรรมตอนส่ง buffer offline (1,080 records) ย้อนหลังเป็นก้อนใหญ่ ทั้งหมดอิงจาก barnowl-minew
- **MBT01 / C10**: เหมือนในชุด MHS (ดู [`minew-kit.md`](minew-kit.md))

เมื่อได้เฟรมจริง: เก็บด้วย `python3 infra/capture-golden.py capture --scenario s4` (หรือ `msp01`) แล้วเขียน decoder + golden test ใน `backend/internal/adapters/minew/` ก่อนพิจารณาเปลี่ยน `Verified` ตาม [`docs/hardware-bringup.md`](../hardware-bringup.md) ข้อ 5
