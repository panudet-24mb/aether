# Aether Production Runbook (on-premise, 1 เซิร์ฟเวอร์)

คู่มือนี้เขียนให้เจ้าของระบบที่ติดตั้งและดูแลเซิร์ฟเวอร์เอง ทุกคำสั่งรันจาก **repository root** บนเครื่องเซิร์ฟเวอร์

ชุดไฟล์ production ทั้งหมดอยู่ใน `infra/prod/` และเป็นชุด **แยกจาก dev** (`infra/compose.yaml`) โดยสิ้นเชิง — คนละ compose project คนละ volume คนละ secret

| ไฟล์ | หน้าที่ |
|---|---|
| `infra/prod/setup.py` | สร้าง secret, ใบรับรอง, ไฟล์ตั้งค่า mosquitto และ `.env.prod` (idempotent) |
| `infra/prod/compose.yaml` | postgres, migrate, api, mqtt, mqtt-ingest, mqtt-provisioner, mqtt-commander, tuya-cloud, web, proxy, backup, pitr (+ pitr-offsite ถ้าเปิด) |
| `infra/prod/postgres/Dockerfile` | image `aether-postgres` = `postgres:18-alpine` (ตรึง digest) + pgBackRest |
| `infra/prod/Caddyfile` | reverse proxy + TLS + security headers (ค่าทุกอย่างมาจาก `.env.prod`) |
| `infra/prod/init-db.sh` | สร้าง role และบังคับ `hostssl` ตอน volume ใหม่ |
| `infra/prod/create-owner.sh` | สร้างบัญชีเจ้าของคนแรก |
| `infra/prod/backup-now.sh` / `backup.sh` / `backup-loop.sh` | สำรองข้อมูล |
| `infra/prod/restore.sh` / `restore-inner.sh` | กู้คืนจาก dump |
| `infra/prod/pitr-loop.sh` | service `pitr`: backup ของ pgBackRest ตามเวลา + ตรวจสุขภาพการสำรองและแจ้งเตือน |
| `infra/prod/pitr-info.sh` / `pitr-backup-now.sh` | ดูช่วงเวลาที่กู้ได้ / สั่ง backup ของ pgBackRest ทันที |
| `infra/prod/restore-pitr.sh` / `pitr-drill.sh` | กู้คืนย้อนเวลา (PITR) ลง volume ใหม่ / ซ้อมกู้รายเดือน |
| `infra/prod/renew-mqtt-cert.py` | ต่ออายุใบรับรอง broker จาก CA เดิม |

---

## 1. เตรียมเซิร์ฟเวอร์

### ขนาดเครื่อง

| ขนาดงาน | vCPU | RAM | Disk (SSD/NVMe) |
|---|---|---|---|
| ≤ 100 tag, 1–3 gateway | 2 | 8 GB | 200 GB |
| ≤ 500 tag, ≤ 20 gateway | 4 | 16 GB | 500 GB |
| > 500 tag | 8 | 32 GB | 1 TB + ทบทวนสถาปัตยกรรม (ดูหัวข้อ 13) |

ระบบปฏิบัติการ: Linux 64-bit ที่ยังได้รับ security update (Ubuntu Server 24.04 LTS หรือ Debian 13)

### ประมาณการพื้นที่ดิสก์

หนึ่ง sample ที่ถูกเก็บลง `core.sensor_samples` กินประมาณ **1 KB** (รวม index)

```
พื้นที่ ≈ จำนวน tag × (86400 ÷ SAMPLE_MIN_INTERVAL_SEC) × SAMPLE_RETENTION_DAYS × 1 KB
```

| tag | interval | retention | ต่อวัน | รวม |
|---|---|---|---|---|
| 100 | 30 วินาที | 90 วัน | ~288 MB | **~26 GB** |
| 100 | 0 (เก็บทุก uplink ≈ 10 วินาที) | 90 วัน | ~864 MB | **~78 GB** |
| 500 | 30 วินาที | 90 วัน | ~1.4 GB | **~130 GB** |
| 500 | 0 | 90 วัน | ~4.3 GB | **~390 GB** |

`SAMPLE_MIN_INTERVAL_SEC=30` (ค่าเริ่มต้นของ `setup.py`) ลดพื้นที่ลงประมาณ **3 เท่า** เทียบกับการเก็บทุก uplink โดยยังเห็นแนวโน้มอุณหภูมิ/ความชื้นครบ แนะนำให้ใช้ 30 ตั้งแต่วันแรก ปรับเป็น 0 เฉพาะตอนต้องสอบสวนปัญหาเฉพาะจุด

เผื่อเพิ่มอีก **2 เท่า** สำหรับ WAL, autovacuum bloat, ไฟล์ backup 14+8 ชุด และ Docker image

**retention เป็นหน่วย partition** (migration `00037`): `core.sensor_samples` แบ่งรายสัปดาห์ (เริ่มวันจันทร์ 00:00 UTC) และ `core.ble_history` แบ่งรายวัน (00:00 UTC) ของที่หมดอายุถูกทิ้งทีละทั้งก้อนด้วย `DROP TABLE` จึงอยู่นานกว่าค่าที่ตั้งได้ถึงหนึ่งช่วง: sample อยู่ `SAMPLE_RETENTION_DAYS` ถึง `SAMPLE_RETENTION_DAYS + 7` วัน, BLE history อยู่ `BLE_HISTORY_HOURS` ถึง `BLE_HISTORY_HOURS + 24` ชั่วโมง เผื่อดิสก์เพิ่มอีกหนึ่งสัปดาห์ของ sample ในตารางข้างบน

คลัง PITR (`--pitr-dir`) ต้องการที่เพิ่ม ≈ **2 × full backup ที่บีบอัดแล้ว + WAL 1–2 สัปดาห์** (zstd บีบ WAL ได้มาก ปกติ
เล็กกว่าขนาดฐานข้อมูล) ดูตัวเลขจริงได้จาก `infra/prod/pitr-info.sh` หลังเปิดใช้ 1 สัปดาห์ ถ้ามีดิสก์แยก ให้วางคลังไว้คนละดิสก์กับ Docker

**กติกาเผื่อที่สำหรับคิว WAL:** ดิสก์ที่มี Docker volume ต้องว่างอย่างน้อย `PITR_QUEUE_MAX` (8GB) + 2 × `max_wal_size` (8GB)
+ 20% ของดิสก์ ตลอดเวลา เพราะตอนคลังเขียนไม่ได้ WAL จะกองใน `pg_wal` จนถึงเพดานนั้นก่อนถูกทิ้ง ถ้าที่ว่างน้อยกว่านั้น
ให้ลด `--pitr-queue-max` (เช่น `2GB`) มิฉะนั้นดิสก์อาจเต็มก่อนถึงเพดานและฐานข้อมูลหยุด

### สิ่งที่ต้องมีก่อนติดตั้ง

```sh
# Docker Engine + compose plugin (ไม่ใช่ docker.io ของ distro)
curl -fsSL https://get.docker.com | sh
docker compose version        # ต้อง >= v2.30

# นาฬิกาต้องตรง — ใบรับรอง TLS และ timestamp ของ sample ขึ้นกับเวลา
sudo timedatectl set-timezone Asia/Bangkok
sudo apt install -y chrony && sudo systemctl enable --now chrony
timedatectl                   # ต้องเห็น "System clock synchronized: yes"
```

**Firewall — เปิดเฉพาะ 3 พอร์ตนี้** (บวก SSH เฉพาะจาก subnet ผู้ดูแล)

```sh
sudo ufw default deny incoming
sudo ufw allow from 10.0.0.0/24 to any port 22 proto tcp   # เปลี่ยนเป็น subnet ผู้ดูแลจริง
sudo ufw allow 443/tcp        # เว็บ
sudo ufw allow 80/tcp         # redirect → HTTPS และ ACME challenge
sudo ufw allow 8883/tcp       # MQTT over TLS จาก gateway
sudo ufw enable
```

พอร์ต **1883 ปิดเป็นค่าเริ่มต้น** — ไฟล์ `mosquitto.conf` ที่ `setup.py` สร้างมี `listener 8883` อย่างเดียว เว้นแต่จะเปิดโหมดไม่มี TLS ตามหัวข้อ 4.5

ถ้าเปิด `--mqtt-plaintext` ให้เปิด 1883 **เฉพาะ subnet ของ gateway** ห้ามเปิดทั้งโลก:

```sh
sudo ufw allow from 192.168.10.0/24 to any port 1883 proto tcp   # เปลี่ยนเป็น subnet/VLAN ของ gateway จริง
```

**IP ต้องนิ่ง** — จอง DHCP reservation ให้ MAC ของเซิร์ฟเวอร์ หรือตั้ง static IP ถ้าใช้ IP เป็น `--host` แล้ว IP เปลี่ยน ใบรับรอง MQTT จะใช้ไม่ได้ทันที (SAN ไม่ตรง) และ gateway ทุกตัวจะหลุด

> **สิ่งที่เจ้าของต้องตัดสินใจก่อนเริ่ม**
> 1. `--host` จะเป็น **ชื่อ DNS ภายใน** (เช่น `aether.hospital.local` — ดีกว่า เพราะย้ายเครื่อง/เปลี่ยน IP ได้) หรือ **IP** (เช่น `192.168.1.64` — ง่ายกว่าถ้าไม่มี DNS ภายใน)
> 2. มี DNS สาธารณะที่ชี้มาที่เครื่องนี้และเปิดพอร์ต 80 ออกอินเทอร์เน็ตได้หรือไม่ → ถ้าได้ใช้ `--tls acme` (ไม่ต้องติดตั้ง root cert ที่เครื่องลูกข่าย) ถ้าไม่ได้ (ส่วนใหญ่ในโรงพยาบาล) ใช้ `--tls internal`

---

## 2. ติดตั้งครั้งแรก

```sh
cd /opt/aether                       # ที่เก็บ repo บนเซิร์ฟเวอร์

# 2.1 สร้าง secret, ใบรับรอง และ .env.prod  (รันด้วย sudo เพื่อให้ chown ให้ container ได้ถูก uid)
sudo python3 infra/prod/setup.py \
    --host aether.hospital.local \
    --tls internal \
    --email ops@hospital.local \
    --mqtt-bind 0.0.0.0 \
    --shadow true

# 2.2 build image ทั้งสองตัว (ใช้เวลาประมาณ 5–10 นาทีครั้งแรก)
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml build

# 2.3 เปิดระบบ
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d

# 2.4 ตรวจสถานะ — ต้องเห็น healthy ครบ
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml ps
```

ผลที่ถูกต้อง: `postgres`, `api`, `mqtt`, `mqtt-ingest`, `web`, `proxy`, `backup` = `Up (healthy)`; `prepare` และ `migrate` = `Exited (0)`; `mqtt-provisioner` = `Up`; `pitr` = `Up (health: starting)` จนกว่า full backup แรกจะเสร็จ แล้วเป็น `healthy`

`setup.py` รันซ้ำได้เสมอ — จะ **ไม่** เขียนทับ secret หรือใบรับรองที่มีอยู่ ใช้รันซ้ำเมื่อต้องการเปลี่ยนพอร์ต ชื่อโฮสต์ หรือสลับ `--shadow`

### ถ้าใช้ `--tls internal` ต้องติดตั้ง root certificate ที่เครื่องลูกข่าย

ไม่อย่างนั้นเบราว์เซอร์จะขึ้นคำเตือน "ไม่ปลอดภัย" ทุกครั้ง

```sh
# ดึง root certificate ของ Caddy ออกมา
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml \
     cp proxy:/data/caddy/pki/authorities/local/root.crt ./aether-root.crt
```

นำไฟล์ `aether-root.crt` ไปติดตั้ง:

| เครื่อง | วิธี |
|---|---|
| Windows | ดับเบิลคลิก → Install Certificate → **Local Machine** → Place in **Trusted Root Certification Authorities** |
| macOS | เปิดด้วย Keychain Access → **System** → ดับเบิลคลิกใบรับรอง → Trust → Always Trust |
| iPad / iPhone | ส่งไฟล์เข้าเครื่อง → Settings → Profile Downloaded → Install → แล้ว **Settings → General → About → Certificate Trust Settings** → เปิดสวิตช์ |
| Android | Settings → Security → Encryption & credentials → Install a certificate → CA certificate |

ใบนี้มีอายุ 10 ปี ติดตั้งครั้งเดียวต่อเครื่อง (ใบของเว็บเองที่ Caddy ออกให้จะต่ออายุอัตโนมัติ ไม่ต้องทำอะไร)

---

## 3. สร้างบัญชีเจ้าของและเข้าใช้ครั้งแรก

ระบบ production **ปิดการสมัครสมาชิกเอง** (`ALLOW_REGISTRATION=false`) บัญชีแรกจึงต้องสร้างจากเครื่องเซิร์ฟเวอร์

```sh
sudo sh infra/prod/create-owner.sh \
    --email owner@hospital.local \
    --name "ชื่อผู้ดูแล" \
    --tenant "โรงพยาบาลตัวอย่าง"
```

สคริปต์จะสุ่มรหัสผ่านเก็บไว้ที่ `<secrets-dir>/owner-password.txt` (สิทธิ์ 0600) **ไม่พิมพ์ออกหน้าจอ** และส่งเข้า container ทาง stdin เท่านั้น จึงไม่ปรากฏใน `ps` หรือ shell history

```sh
sudo cat /opt/aether/.secrets/prod/owner-password.txt   # อ่านครั้งเดียวแล้วเปลี่ยนรหัสในเว็บ
```

ถ้าต้องการกำหนดรหัสเอง: เขียนไฟล์ 0600 แล้วส่งด้วย `--password-file /path/to/file`

จากนั้นเปิด `https://aether.hospital.local` แล้วเข้าสู่ระบบ เมนูซ้ายจะมี ภาพรวม / การแจ้งเตือน / เชื่อมต่ออุปกรณ์ / ผังอาคาร / Dashboard Studio / Automation Studio / อุปกรณ์ทั้งหมด

ผู้ใช้คนอื่นเพิ่มจากในเว็บที่หน้า **ทีม**

---

## 4. ต่อ Minew MG3 ตัวจริง

### 4.1 สร้าง gateway ในเว็บ

หน้า **เชื่อมต่ออุปกรณ์** → เพิ่ม gateway → เลือกรุ่น `minew-mg3` → ระบบจะแสดง **username / password / topic ครบชุด แค่ครั้งเดียว** จดหรือคัดลอกทันที (ถ้าหาย กด rotate เพื่อออกใหม่ ตัวเก่าจะใช้ไม่ได้)

เบื้องหลัง: API เขียน hash ลงฐานข้อมูล → `mqtt-provisioner` (ภายใน ≤ 3 วินาที) เขียนไฟล์ password/ACL ให้ Mosquitto แล้วส่ง SIGHUP เอง ไม่ต้องรีสตาร์ตอะไร

ACL ที่ได้: gateway นั้น publish ได้เฉพาะ `/aether/gateways/<id>/status` และ `/response` และ subscribe ได้เฉพาะ `/action` — gateway ตัวหนึ่งอ่าน/เขียนข้ามตัวอื่นไม่ได้

### 4.2 ไฟล์ CA สำหรับอัปโหลดเข้า MG3

```
<secrets-dir>/mqtt/broker/ca.crt      เช่น /opt/aether/.secrets/prod/mqtt/broker/ca.crt
```

ไฟล์นี้เป็น **ใบรับรองสาธารณะ** เปิดเผยได้ ตัว **private key ของ CA** อยู่ที่ `<secrets-dir>/mqtt/ca.key` และ **ไม่ถูก mount เข้า container ใดเลย** — ห้ามคัดลอกออกจากเซิร์ฟเวอร์ ยกเว้นตอนทำ offline backup

### 4.3 ตั้งค่าใน MG3 (ผ่านแอป iOS / เว็บ config ของ Minew)

| ช่อง | ค่า |
|---|---|
| Protocol | **MQTT over TLS** (`mqtts://` / SSL เปิด) |
| Host | ค่า `--host` ที่ใช้ตอน setup |
| Port | **8883** |
| Username / Password | ที่ได้จากหน้าเว็บ (ขึ้นต้นด้วย `gw-`) |
| Client ID | เท่ากับ username |
| Post topic | `/aether/gateways/<gateway-id>/status` |
| Subscribe topic | `/aether/gateways/<gateway-id>/action` |
| Reply topic | `/aether/gateways/<gateway-id>/response` |
| QoS | 1 |
| Keep Alive | 120 วินาที |
| Upload format | **JSON-LONG** (ไม่ใช่ JSON-SHORT / hex) |
| Upload interval | 10–30 วินาที (ยิ่งถี่ยิ่งกินพื้นที่ ดูตารางข้อ 1) |
| Certificate | อัปโหลด `ca.crt` ข้างบน |
| Verify server certificate | **เปิด** |
| NTP | ตั้งให้ MG3 sync เวลา (ใช้ NTP ใน LAN หรือ `time.google.com`) |

ห้ามปิดการตรวจใบรับรองฝั่งเซิร์ฟเวอร์ broker ไม่ต้องใช้ client certificate

### 4.4 ตรวจว่าข้อมูลเข้าจริง

หน้า **เชื่อมต่ออุปกรณ์** จะแสดงสถานะ MQTT ของแต่ละ gateway ถ้าไม่ขึ้น:

```sh
# ดูว่า broker เห็น gateway เชื่อมต่อไหม
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml logs mqtt --tail 50
# ดูว่า collector เก็บ packet ไหม
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml logs mqtt-ingest --tail 50
```

`MQTT packet stored` = เก็บ packet ดิบสำเร็จ **`decoded:false` เป็นเรื่องปกติ** — หมายถึงยังไม่มี decoder ที่ผ่านการยืนยันสำหรับ frame ของ Minew ทุกแบบ (ดูข้อ 13)

---


### 4.5 Gateway ที่ยังไม่รองรับ TLS (เปิดเองอย่างตั้งใจ)

บาง gateway หรือ firmware ยังตั้ง TLS ไม่ได้ ให้เปิด listener 1883 แบบไม่เข้ารหัสควบคู่กับ 8883:

```sh
python3 infra/prod/setup.py ... --mqtt-plaintext            # พอร์ตเปลี่ยนได้ด้วย --mqtt-plain-port
docker compose --env-file <env> -f infra/prod/compose.yaml up -d mqtt api
```

ผลที่ได้:
- `mosquitto.conf` มี `listener 1883` เพิ่ม ยัง**บังคับ username/password และ ACL ต่อ gateway เหมือน 8883** (ทดสอบแล้ว: ไม่ใส่รหัส/รหัสผิดถูกปฏิเสธ)
- หน้า "เพิ่ม gateway" แสดงช่อง **"ไม่มี TLS"** เป็น `tcp://<host>:1883`
- ตั้ง MG3/MG4: Protocol `tcp://` / SSL ปิด, Port `1883`, username/password/client ID/topics เหมือนตาราง 4.3 ไม่ต้องอัปโหลด CA

ความเสี่ยง: password ของ gateway และข้อมูล BLE วิ่งแบบอ่านได้บนสาย ใครดักใน LAN เดียวกันได้ก็เอารหัสไปส่งข้อมูลปลอมในนามของ gateway นั้นได้ (แต่ ACL จำกัดให้ส่งได้เฉพาะ topic ของ gateway ตัวนั้น) จึงต้อง:
1. ให้ gateway อยู่ VLAN/subnet แยก และ firewall เปิด 1883 เฉพาะ subnet นั้น
2. ห้าม port-forward 1883 ออก internet
3. ถ้ารหัสอาจหลุด ให้ rotate credential ของ gateway ในเว็บ
4. ย้าย gateway ไป 8883 เมื่อ firmware รองรับ แล้วรัน `setup.py` ใหม่โดยไม่ใส่ `--mqtt-plaintext` เพื่อปิด 1883

`MQTT_PUBLIC_SCHEME=tcp` เป็นช่องทางหลักใน production ยังถูกปฏิเสธเหมือนเดิม — 1883 เป็นช่องทางเสริมเท่านั้น

### 4.6 วางหลัง reverse proxy อื่น (nginx / Cloudflare) บนเครื่องเดียวกัน

ถ้าเครื่องมี nginx ถือพอร์ต 80/443 อยู่แล้ว ให้ Caddy ของ Aether ฟังเฉพาะ loopback บนพอร์ตอื่น แล้วให้ nginx ส่งต่อมา:

```sh
python3 infra/prod/setup.py --host aether.example.com \
    --public-origin https://aether.example.com \
    --mqtt-host 203.0.113.10 \
    --upstream-proxy 172.29.7.1/32 \
    --web-bind 127.0.0.1 --https-port 8443 --http-port 8088 --tls internal
```

| option | ความหมาย |
|---|---|
| `--host` | ต้องเป็น**ชื่อเดียวกับที่ nginx ส่งมาใน Host/SNI** ไม่อย่างนั้น Caddy จะไม่มี site ที่ตรง |
| `--public-origin` | URL ที่เบราว์เซอร์เห็นจริง (ไม่มีพอร์ต 8443) → `APP_ORIGIN` ที่ API ใช้ตรวจ Origin |
| `--mqtt-host` | ชื่อ/IP ที่ gateway ใช้ต่อ MQTT เมื่อต่างจากเว็บ (เช่น เว็บอยู่หลัง Cloudflare ซึ่งส่ง MQTT ไม่ได้) · ใช้เป็น SAN ของใบรับรอง broker ตอนออกครั้งแรก ถ้าเปลี่ยนภายหลัง setup จะเตือนให้ออกใบใหม่ |
| `--upstream-proxy` | IP/CIDR ของ proxy ด้านหน้าที่ Caddy เชื่อ `X-Forwarded-For` (อ่านจากขวาไปซ้าย `trusted_proxies_strict`) · nginx ที่ต่อเข้า `127.0.0.1` จะปรากฏเป็น gateway ของ subnet compose คือ `.1` · **ต้องตั้งเสมอเมื่อมี nginx/Cloudflare ข้างหน้า** ไม่งั้นทุก request ดูเหมือนมาจาก IP ของ proxy ตัวเดียว ทำให้ rate limit ต่อ IP (login, bootstrap ของ Edge) นับรวมกันทุกคน |
| `--web-bind` | IPv4 ที่ Caddy เปิดพอร์ต · `127.0.0.1` = เข้าได้เฉพาะผ่าน nginx |

ฝั่ง nginx: `proxy_pass https://127.0.0.1:8443;` พร้อม `proxy_ssl_server_name on; proxy_ssl_name <host>;` ตรวจใบของ Caddy ด้วย root ที่ดึงจาก `proxy:/data/caddy/pki/authorities/local/root.crt`, ส่ง `Host $host`, **เขียนทับ** `X-Forwarded-For $remote_addr` (ไม่ append) และส่ง `Upgrade`/`Connection` สำหรับ `/ws`

### 4.7 Zigbee2MQTT (สวิตช์ Zigbee ผ่าน coordinator ในอาคาร)

ดูรายละเอียดทั้งหมดใน `docs/platform/zigbee2mqtt.md` ต้องใช้ **Zigbee2MQTT 2.x ขึ้นไป** และตอนอัปเกรดเครื่องที่ติดตั้งไว้แล้วต้องทำเพิ่มหนึ่งขั้น: broker ต้องรับ packet ขนาด 1 MiB (เดิม 256 KiB) ไม่อย่างนั้น `bridge/devices` ของเครือข่ายที่ใหญ่หน่อยจะถูก broker ตัดทิ้งและ Zigbee2MQTT จะหลุด

```sh
python3 infra/prod/setup.py <flag ชุดเดิมที่ใช้ติดตั้ง>
docker compose --env-file .env.prod -f infra/prod/compose.yaml restart mqtt
```

### 4.8 สั่งงานอุปกรณ์ Zigbee (mqtt-commander)

ตั้งแต่ migration `00029` Aether สั่งงานอุปกรณ์ที่ต่อผ่าน Zigbee2MQTT ได้ (เปิด/ปิดสวิตช์ ความสว่างไฟ ตำแหน่งม่าน กลอน ฯลฯ ดู `docs/platform/zigbee2mqtt.md`) ผ่าน service ใหม่ `mqtt-commander` ซึ่งมีบัญชี broker ของตัวเอง (`aether-commander`) ที่ **เขียนได้เฉพาะ `aether/z2m/+/+/set`** เท่านั้น

อัปเกรดเครื่องที่ติดตั้งไว้แล้ว (ลำดับสำคัญ):

```sh
git pull
# 1) setup.py สร้างรหัสของ aether-commander, hash แล้ว "ต่อท้าย" broker/passwords เดิม (ไม่ hash รายการเดิมซ้ำ)
#    และเขียน <secrets>/mqtt/commander/commander.json — รันซ้ำได้
python3 infra/prod/setup.py <flag ชุดเดิมที่ใช้ติดตั้ง>
# 2) build แล้วเปิดทุก service: migrate รัน 00029, provisioner เพิ่มสิทธิ์ของ aether-commander เองภายในไม่กี่วินาที
docker compose --env-file .env.prod -f infra/prod/compose.yaml build
docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d
# 3) ตรวจ
docker compose --env-file .env.prod -f infra/prod/compose.yaml ps mqtt-commander        # healthy
docker compose --env-file .env.prod -f infra/prod/compose.yaml logs mqtt-commander --tail 5   # "MQTT commander ready"
```

ไม่ต้อง restart `mqtt`: provisioner เขียนไฟล์ runtime ใหม่และ SIGHUP broker เอง ถ้า `mqtt-commander` ต่อ broker ไม่ได้ (not authorised) แปลว่า provisioner ยังไม่ได้อ่านรหัสใหม่ ให้ `restart mqtt-provisioner`

ผู้สั่งงานได้: owner, admin, operator (เฉพาะโปรเจกต์ของตัวเอง) · viewer สั่งไม่ได้ · ปิดสิทธิ์รายคนได้ที่หน้าทีม → โมดูล "สั่งงานอุปกรณ์"

### 4.9 ให้ผังอัตโนมัติสั่งอุปกรณ์ (AUTOMATION_COMMANDS)

ตั้งแต่ migration `00031` บล็อก "สั่งอุปกรณ์" ใน Automation Studio ใช้งานได้จริง แต่ **ปิดไว้เป็นค่าเริ่มต้น**: วาดและบันทึกเป็นฉบับร่างได้ แต่เปิดใช้ผังที่มีบล็อกนี้ไม่ได้ จนกว่าจะเปิดสวิตช์ของระบบอย่างตั้งใจ:

```sh
python3 infra/prod/setup.py <flag ชุดเดิม> --automation-commands true
docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d api mqtt-ingest
```

ข้อควรรู้:
- **ผังอัตโนมัติไม่ทำงานเลยใน shadow mode** (รวมการสั่งอุปกรณ์) จึงต้องปิด shadow ด้วย (`--shadow false`) ถึงจะเห็นผังสั่งอุปกรณ์จริง — ทั้งสองอย่างต้องตัดสินใจแยกกัน
- คนที่กดเปิดใช้ผังต้องมีสิทธิ์สั่งงานอุปกรณ์เอง (owner หรือ admin ที่โมดูล "สั่งงานอุปกรณ์" ไม่ได้ถูกปิด) และเป็นผู้รับผิดชอบคำสั่งของผังนั้นใน log (`actor_id`)
- สิทธิ์ของผู้เปิดใช้ผังถูกตรวจซ้ำทุกครั้งที่ผังทำงาน ถ้าเขาถูกลดบทบาท ถูกลบ ถูกปิดโมดูล "สั่งงานอุปกรณ์" หรือขอบเขตโปรเจกต์ไม่ครอบคลุมอุปกรณ์แล้ว ผังจะถูกปิดอัตโนมัติ (audit `automation.disarmed`) ต้องให้คนที่มีสิทธิ์เปิดใหม่
- กันวนซ้ำ: เหตุการณ์ที่เกิดจากคำสั่งของผังอัตโนมัติเองจะไม่ทำให้ผังอื่นสั่งต่อ · อุปกรณ์หนึ่งตัวรับคำสั่งจากผังรวมกันได้ไม่เกิน 6 ครั้งต่อนาที · ผังทั้งหมดใช้ได้ไม่เกิน 30 จาก 60 คำสั่งต่อนาทีของ workspace (อีกครึ่งเก็บไว้สั่งด้วยมือเสมอ)
- คำสั่งจากผังผ่าน `mqtt-commander` (ผังเขียนคำขอลง outbox แล้ว commander ส่งเข้าคิว) ถ้า `mqtt-commander` ไม่ทำงาน ผังจะไม่สั่งอะไร
- ปิดสวิตช์ภายหลัง (`--automation-commands false`) ผังยังเปิดอยู่แต่จะไม่ส่งคำสั่ง (ประวัติการทำงานบอกเหตุผล `commands_disabled`)
- **รัน `setup.py` ซ้ำโดยไม่ใส่ `--automation-commands` จะคงค่าเดิมไว้** ในไฟล์ env · ต่างจาก `--shadow` ที่ **ถูกตั้งใหม่ทุกครั้ง** (เป็น `true` ถ้าไม่ใส่ `--shadow false` ซ้ำ) ดังนั้นเมื่อปิด shadow แล้ว ต้องใส่ `--shadow false` ทุกครั้งที่รัน setup.py

### 4.10 Aether Edge และการนำเข้า Tuya Wi‑Fi

ตั้งแต่ migration `00032` (ไม่มี migration ใหม่ในรอบนำเข้า) มี 3 อย่างเพิ่มในชุด production:

- **CA ของ broker ถูก mount เข้า container `api`** แบบอ่านอย่างเดียว เฉพาะไฟล์ `mqtt/broker/ca.crt` ซึ่งเป็นใบรับรองสาธารณะ ไม่ใช่ทั้งโฟลเดอร์ เพราะโฟลเดอร์มี private key ของ broker อยู่
  - ตั้ง env ไว้เป็น `MQTT_CA_FILE=/run/mqtt-ca/ca.crt` เพื่อส่งให้ตัวติดตั้ง Edge
  - ถ้าไม่มีไฟล์นี้ `POST /edge/bootstrap` จะตอบ 503 **โดยไม่ใช้โค้ดติดตั้งทิ้ง**
  - ส่งให้ตัวติดตั้งเฉพาะบล็อก `CERTIFICATE` ที่เป็น X.509 จริงเท่านั้น ต่อให้ mount ไฟล์ที่มี private key ปนมาผิดๆ ก็จะไม่หลุดออกไป
- **Caddy ส่ง `/edge/bootstrap` ไปที่ API** (ต้อง `up -d` ให้ proxy อ่าน Caddyfile ใหม่ หรือ `restart proxy`) · โค้ดติดตั้งส่งใน body ไม่อยู่ใน URL จึงไม่ติดไปใน access log ของ Caddy/nginx/Cloudflare
- **Caddy ส่ง `/edge/install.sh` ไปที่ API ด้วย** (ตั้งแต่ phase E) API สร้างสคริปต์โดยใส่ origin ของเซิร์ฟเวอร์และ image ที่ pin ไว้ (`domain.EdgeImageTag`) ให้ ต้อง `restart proxy` หลัง deploy รอบที่แก้ Caddyfile
- migration `00033` ให้โค้ดติดตั้งจับคู่กับ gateway Zigbee2MQTT ได้ (ติดตั้ง Edge + Zigbee2MQTT ด้วยคำสั่งเดียว)
- **Image ของ Aether Edge** สร้างโดย GitHub Actions (`.github/workflows/edge.yml`) เมื่อ push tag `edge-v<เวอร์ชัน>` เช่น `edge-v0.1.0` → `ghcr.io/panudet-24mb/aether-edge:0.1.0`
  - หลัง push ครั้งแรก **ต้องตั้ง package เป็น public ครั้งเดียว**: GitHub → โปรไฟล์ → Packages → `aether-edge` → Package settings → Change visibility → Public (ไม่งั้น Pi ดึง image ไม่ได้)
  - ออกเวอร์ชันใหม่: push tag ใหม่ → แก้ `EdgeImageTag` ใน `backend/internal/domain/edge.go` (หลัง publish แล้วจะ pin ด้วย digest ด้วยก็ได้ เช่น `0.1.0@sha256:…`) → deploy เซิร์ฟเวอร์ → ที่ Pi รัน `curl -fsSL <origin>/edge/install.sh | sudo sh -s -- --update`
  - เวอร์ชัน Zigbee2MQTT ที่ติดตั้งให้ที่ Pi กำหนดที่ `domain.Zigbee2MQTTImage` (`--update` ปรับให้ด้วย)
  - คำสั่งติดตั้งที่หน้าเว็บ **ไม่มีโค้ด** สคริปต์จะถามโค้ดตอนรัน (โค้ดจึงไม่อยู่ใน log ของ sudo หรือ history)
- **หลัง bootstrap ให้หยุด agent ตัวเก่าของ gateway นั้น**
  - Mosquitto ไม่ตัด session ที่ login ไว้แล้ว ตอนเปลี่ยนรหัสผ่าน
  - session เก่าจะหลุดเมื่อ agent ตัวใหม่ต่อเข้ามาด้วย client id เดียวกัน (`gw-<id>`)
  - ถ้าสงสัยว่ารหัสเก่ารั่ว ให้ `restart mqtt` เพื่อตัดทุก session (gateway ต่อกลับเองภายในไม่กี่วินาที)
  - รายละเอียดที่ [aether-edge.md](platform/aether-edge.md)
- ถ้าเว็บใช้ CA ภายใน (`--tls internal`) Edge ต้องเชื่อ root ของ Caddy เพื่อดึงค่าตั้งผ่าน HTTPS ให้ export ครั้งเดียว:
  ```sh
  docker compose --env-file .env.prod -f infra/prod/compose.yaml cp proxy:/data/caddy/pki/authorities/local/root.crt <secrets>/edge-web-ca/root.crt
  docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d api
  ```
  ถ้าเว็บใช้ใบรับรองจริง (ACME หรือหลัง Cloudflare) ไม่ต้องทำขั้นนี้

`setup.py` สร้างโฟลเดอร์ `<secrets>/edge-web-ca` ให้ ลำดับ rollout คือ `git pull` → รัน `setup.py` ด้วย flag ชุดเดิม → `build` → `up -d` (api และ proxy ถูกสร้างใหม่)

local key ของ Tuya ถูกเข้ารหัสด้วยกุญแจที่ derive จาก `CHANNEL_SEAL_KEY` (หรือ JWT key ถ้าไม่ได้ตั้ง) **สำรอง `.env.prod` ไว้แยกจาก backup ฐานข้อมูล** เหมือนกรณี secret ของช่องทางแจ้งเตือน ถ้ากุญแจหาย ต้องนำเข้าจาก Tuya ใหม่

### 4.11 Tuya Cloud (migration `00034`) — ตรวจก่อน migrate

migration `00034` สร้าง unique index `devices_tuya_one_mode`: อุปกรณ์ Tuya หนึ่งตัวลงทะเบียนได้ทางเดียวต่อ workspace (ผ่าน Edge หรือผ่าน Cloud) ถ้าข้อมูลเดิมมีซ้ำ migration จะล้มทั้งก้อน **รันคำสั่งนี้ก่อน deploy ต้องได้ 0 แถว** ถ้ามีแถว ให้ลบการลงทะเบียนที่ซ้ำออกทางหน้าเว็บก่อน:

```sh
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml exec -u postgres postgres \
  psql -U postgres -d aether -c "SELECT tenant_id, lower(external_id) AS tuya_id, count(*)
    FROM core.devices WHERE removed_at IS NULL AND profile_id IN ('tuya-wifi-device@1','tuya-cloud-device@1')
    GROUP BY 1,2 HAVING count(*)>1"
```

- `setup.py` สร้างคู่กุญแจ `TUYA_CLOUD_PUBLIC_KEY` / `TUYA_CLOUD_PRIVATE_KEY` ครั้งเดียวแล้วเก็บไว้ (รันซ้ำได้) และเขียน `TUYA_CLOUD=false`, `TUYA_CLOUD_MAX_LINKS=50`, `TUYA_CLOUD_MAX_LINKS_PER_TENANT=2`, `TUYA_CLOUD_EVENT_BUDGET=68000`, `TUYA_CLOUD_API_BUDGET=26000` (งบเป็นค่าเริ่มต้นชั่วคราว แก้ใน `.env.prod` ให้ตรงแพ็กเกจจริงของโปรเจกต์ · 0 = ไม่มีตัวกันงบ)
  - API ได้ **เฉพาะ public key** (ใช้ซีลรหัสโปรเจกต์ Tuya) · service `tuya-cloud` ได้ **เฉพาะ private key** — ห้ามส่ง private key ให้ container อื่น
  - private key หาย = โปรเจกต์ที่ลิงก์ไว้ทั้งหมดต้องลิงก์ใหม่ สำรอง `.env.prod` แยกจาก backup ฐานข้อมูล
- compose มี service `tuya-cloud` (image เดียวกับ backend, `/app/tuya-cloud`, read-only, 192 MiB, healthcheck `tuya-cloud health`) ได้แค่ DSN ของ `aether_app`, `TUYA_CLOUD`, private key, จำนวนลิงก์ และงบ — ไม่มี JWT/`CHANNEL_SEAL_KEY` ไม่มีบัญชี MQTT · ต้องออกอินเทอร์เน็ตไป Tuya ได้ (HTTPS 443 และ Message Service พอร์ต **8285**) ถ้ามี firewall ขาออกต้องเปิดสองพอร์ตนี้
- **`TUYA_CLOUD=false` (ค่าเริ่มต้น) service `tuya-cloud` จะ idle** (ไม่อ่านกุญแจ ไม่ต่อฐานข้อมูล ไม่ต่อ Tuya) แทนการ exit เพราะ `restart: unless-stopped` จะรีสตาร์ต process ที่ exit ไม่รู้จบ · สถานะ healthy ได้ตามปกติ
- `migrate down` ข้าม `00034` ไม่ได้ถ้ายังมี gateway `tuya-cloud` ที่ยังไม่ revoke (ตั้งใจให้ล้มพร้อมข้อความ) · รายละเอียดที่ [tuya-cloud.md](platform/tuya-cloud.md)

**Rollout G4/G5 (โหมดยังปิด):**

```sh
cd /opt/aether                               # โฟลเดอร์ที่ติดตั้งไว้
sudo sh infra/prod/backup-now.sh             # สำรองก่อนเสมอ (§6)
sudo git pull
# 1) เขียน .env.prod ใหม่ด้วย flag ชุดเดิม (ไม่ใส่ --tuya-cloud = คงค่าเดิม false) — ได้ค่า TUYA_CLOUD_MAX_LINKS และงบเพิ่ม
sudo python3 infra/prod/setup.py <flag ชุดเดิม>
# 2) build แล้วเปิด: api ได้ route ใหม่ และมี service tuya-cloud (idle) เพิ่ม
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml build api web
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d
# 3) ตรวจ: ทุก service healthy · tuya-cloud log ต้องมี "Tuya Cloud mode is off (TUYA_CLOUD=false); idle"
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml ps
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml logs --tail 5 tuya-cloud
```

**เปิดโหมด Tuya Cloud (เมื่อพร้อม และหลังทดสอบกับโปรเจกต์จริงแล้ว):**

```sh
sudo python3 infra/prod/setup.py <flag ชุดเดิม> --tuya-cloud true
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d api mqtt-ingest tuya-cloud
# log ของ tuya-cloud ต้องมี "Tuya Cloud worker ready" · หน้าเว็บเพิ่ม gateway ต้องมี "Tuya Cloud (ไม่ต้องติดตั้ง)"
```

ปิดกลับ: `--tuya-cloud false` แล้ว `up -d api mqtt-ingest tuya-cloud` · gateway และลิงก์เดิมยังอยู่ อุปกรณ์จะแสดงออฟไลน์ เชื่อม/ซิงก์ใหม่ไม่ได้ แต่ owner/admin ยังกด "ยกเลิกการเชื่อม" เพื่อลบรหัสที่เก็บไว้ได้

## 5. วันแรก: เปิดใน shadow mode แล้วค่อยปลด

`setup.py` ตั้ง `ALERTS_SHADOW=true` ให้ตั้งแต่ต้น หมายความว่า: **บันทึก event ทุกอย่างลงฐานข้อมูลตามปกติ แต่ไม่เปิด alert ไม่ส่ง LINE/webhook/อีเมล และไม่รัน automation** (รวมถึงผังที่สั่งอุปกรณ์ ดูข้อ 4.9)

**ข้อยกเว้น: ปุ่มฉุกเฉิน SOS (event `button`) และเครื่องตรวจควัน / แก๊ส / CO (event `hazard`) เปิด alert และส่งตามช่องทางของกฎเสมอ แม้อยู่ใน shadow mode** (SOS ตั้งแต่ 2026-09-23, hazard ตั้งแต่ 2026-09-25 · `domain.BypassesShadow`) เพราะ shadow มีไว้กันเกณฑ์ที่ยังไม่ได้จูนปลุกคนตอนดึก แต่คนกดปุ่มขอความช่วยเหลือหรือควันไฟไม่ใช่เรื่องที่ต้องจูน · `hazard_cleared` เป็นแค่เหตุการณ์ ไม่เปิด alert

ใช้แบบนี้ **อย่างน้อย 3–7 วัน** เพื่อดูว่าเกณฑ์ที่ตั้งไว้ไม่ปลุกคนทั้งโรงพยาบาลตอนตีสาม

ระหว่างนี้ให้ดูที่หน้า **การแจ้งเตือน** ว่ามี event อะไรเกิดบ้าง ถี่แค่ไหน แล้วปรับ threshold / เวลาหน่วง ของ alert rule จนพอใจ

เมื่อพร้อมจะเปิดใช้งานจริง:

```sh
sudo sed -i 's/^ALERTS_SHADOW=true/ALERTS_SHADOW=false/' .env.prod
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d api mqtt-ingest
# ยืนยันว่าหายไปแล้ว: log ต้องไม่มีบรรทัด "ALERTS_SHADOW is on"
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml logs api --tail 20 | grep -i shadow
```

จะกลับไป shadow เมื่อไรก็ได้ด้วยวิธีเดียวกัน (แก้กลับเป็น `true`)

---

## 6. อัปเกรดเวอร์ชัน

```sh
cd /opt/aether

# 6.1 สำรองก่อนเสมอ และจดเวลาไว้ — ใช้กู้ย้อนเวลา (§7.1) ไปจุดก่อน migrate ได้
sudo sh infra/prod/backup-now.sh
sudo ls -lt backups/daily | head -3          # จดชื่อไฟล์ล่าสุดไว้
date '+%F %T%z'                              # จดเวลาก่อนอัปเกรด เช่น 2026-09-30 14:05:00+0700

# 6.2 ดึงโค้ดใหม่ (หรือคัดลอก release มาทับ)
sudo git pull            # ถ้าเป็น git repo

# 6.3 build image ใหม่
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml build

# 6.4 รัน migration (บริการอื่นยังทำงานอยู่ได้)
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml run --rm migrate

# 6.5 เปลี่ยน container ให้ใช้ image ใหม่
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d

# 6.6 ตรวจ
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml ps
curl -s https://aether.hospital.local/health/ready
```

ช่วงที่ระบบสะดุดคือข้อ 6.5 ประมาณ 10–30 วินาที gateway จะ reconnect เอง (QoS 1 + persistent session ทำให้ packet ที่ค้างถูกส่งซ้ำ)

### Migration `00038`/`00039`: บันทึกการเข้าถึงข้อมูลส่วนบุคคล, ส่งออกและลบ (PDPA)

ไม่ต้องหยุดระบบ: `00038` สร้างตาราง `core.access_log` (แบ่งรายเดือน) และขยาย `core.maintain_partitions` / `core.prune_history` ให้ดูแลตารางนี้ด้วย ส่วน `00039` เพิ่มคอลัมน์ `erased_at` / `notice_ack_version` ใน `identity.users` (metadata อย่างเดียว) ตาราง `core.erasure_log` และฟังก์ชันลบข้อมูล หลังอัปเกรด:

- **ทุกการเปิดดูข้อมูลส่วนบุคคลผ่าน API ถูกบันทึก** (รายชื่อสมาชิก ตำแหน่งแท็ก การแจ้งเตือน เหตุการณ์ ภาพรวม Studio การเปิด WebSocket การส่งออก) อ่านซ้ำเรื่องเดิมภายใน 10 นาทีนับครั้งเดียว ถ้าบันทึกไม่ได้ API ตอบ **503 และไม่ส่งข้อมูลออก** (fail closed) — ถ้าเห็น 503 ทั้งระบบ ให้ดู log `access log write failed`
- ค่าใหม่ใน `.env.prod` (`setup.py` เขียนให้และเก็บค่าเดิมไว้ตอนรันซ้ำ): `ACCESS_LOG_RETENTION_DAYS` (ค่าเริ่มต้น 400 วัน, `--access-log-days`) และ `PRIVACY_NOTICE_URL` (ประกาศความเป็นส่วนตัวขององค์กร, `--privacy-notice-url https://...`; ว่าง = ใช้หน้าแม่แบบภาษาไทย `/privacy`)
- `ACCESS_LOG_RETENTION_DAYS` ถูกส่งให้ service **`migrate`** ไม่ใช่ `api`: migrate บันทึกลง `core.retention_policy` ทุกครั้งที่รัน (log `access log retention: N days`; ถ้าไม่ได้ตั้งค่าไว้ = คงค่าเดิมในฐานข้อมูล) API อ่านหรือแก้ค่านี้ไม่ได้ จึงลดอายุบันทึกการเข้าถึงเองไม่ได้ · เปลี่ยนค่า = แก้ `.env.prod` แล้ว `run --rm migrate`
- ส่งออกประวัติแท็ก (ZIP) ทยอยส่งจากฐานข้อมูลโดยตรง ครั้งละไม่เกิน 2 งานต่อ workspace และ 4 งานต่อ API (เกินได้ 429) · ลูกข่ายช้าได้ไฟล์ครบ (เลื่อน write deadline ทีละ 30 วินาทีตามความคืบหน้า) ลูกข่ายที่หยุดอ่านถูกตัดราว 30 วินาที
- บันทึกการเข้าถึงล้ม (503) ไม่กระทบการแจ้งเตือน: การประเมินและส่ง LINE / อีเมล / webhook ทำใน worker ไม่ผ่าน API
- สมาชิกทุกคนจะเห็นแถบ “ประกาศความเป็นส่วนตัว” ให้กดรับทราบหนึ่งครั้ง
- เจ้าของ workspace มีเมนูใหม่ “ความเป็นส่วนตัว” (ใครเปิดดูอะไร ใครเปลี่ยนอะไร การลบข้อมูล) และปุ่มส่งออก/ลบข้อมูลของสมาชิก (หน้าทีม) และของแท็กที่มีคนสวม (แผงอุปกรณ์ roaming)
- ขนาด: log ราว 1 แถว (~200 B) ต่อผู้ใช้ต่อหน้าจอต่อ 10 นาทีที่เปิดใช้งาน — 50 คนเปิดทั้งวันประมาณ 50 MB ต่อปี

### Migration `00036`/`00037`: แบ่ง partition ประวัติ sensor

อัปเกรดที่มีสองไฟล์นี้ทำแบบออนไลน์ ไม่ต้องหยุดระบบ แต่เป็นการเปลี่ยนโครงสร้างตารางใหญ่ที่สุดของระบบ ให้ทำตามนี้:

- **`00036`** (ไม่อยู่ใน transaction): สร้าง primary key ใหม่ที่มี `received_at` แบบ `CONCURRENTLY` (ingest เขียนต่อได้ ใช้เวลาเป็นนาทีบนตารางใหญ่) สลับ key ด้วย lock ระดับมิลลิวินาที แล้วใส่ `CHECK (received_at < X)` ที่ validate ขณะ ingest ยังเขียนอยู่ · X = วันจันทร์ 00:00 UTC ที่อยู่ข้างหน้า 7–14 วัน (sample) และเที่ยงคืน UTC อีก 7 วัน (BLE) · แต่ละตารางทำใน transaction ของตัวเอง ตามลำดับที่ ingest เขียน (ble_history ก่อน sensor_samples) จึงไม่ถือ lock สองตารางพร้อมกัน
- **`00037`** (transaction เดียว): เปลี่ยนชื่อตารางเดิมเป็น `<ตาราง>_legacy` แล้วแนบเป็น partition `FROM (MINVALUE) TO (X)` โดยไม่ copy ข้อมูล (index, primary key, foreign key เดิมถูกใช้ต่อ ไม่ build ใหม่) สร้าง partition ล่วงหน้าและ partition `DEFAULT` · lock `gateways`, `ble_history`, `sensor_streams`, `device_templates`, `sensor_samples` แบบ ACCESS EXCLUSIVE ตามลำดับเดียวกับ ingest ถือไว้ไม่ถึงวินาที (API และ ingest ที่แตะตารางเหล่านี้จะรอช่วงนั้น) · ถ้า lock ใดไม่ว่าเกิน 100 ms จะคืน lock ทั้งหมดแล้วลองใหม่ (log เป็น NOTICE `00037: tables busy, retrying` · รวมราว 100 วินาทีก่อนล้ม) transaction ที่ต่อคิวอยู่จึงรอไม่เกินราวครึ่งวินาทีต่อรอบ ต่ำกว่า `deadlock_timeout` (1 วินาที) deadlock detector จึงไม่ตัด ingest หรือ API ทิ้ง · `00036` ก็รอ lock ครั้งละไม่เกิน 300 ms แล้วลองใหม่เช่นกัน

**ผลข้างเคียง: การกันเก็บซ้ำของ Minew** เดิม packet ที่ payload เหมือนเดิมทุกไบต์ถูกเก็บครั้งเดียวตลอดไป (key ไม่มีเวลา) ตอนนี้ key มี `received_at` จึงกันซ้ำด้วยการมองย้อนหลัง 24 ชั่วโมง: redelivery ของ QoS 1 ยังถูกเก็บครั้งเดียว แต่ tag ที่ส่ง payload เดิมเป๊ะหลังผ่านไปเกินหนึ่งวันจะได้ sample ใหม่ · การตรวจซ้ำ (และการ thinning) มองเฉพาะช่วง `(ตอนนี้ − หน้าต่าง, ตอนนี้]` เพื่อให้อ่านแค่ partition ในช่วงนั้น แถวที่ถูกเก็บตอนนาฬิกาเซิร์ฟเวอร์เดินเร็วเกิน (อยู่ในอนาคต) จึงไม่ถูกนับว่าซ้ำ — ยอมรับได้

`migrate` รันสองไฟล์ต่อกันในครั้งเดียวเสมอ **ถ้า `00036` ผ่านแต่ `00037` ล้ม** (goose บันทึก `00036` ไว้แล้ว) ให้แก้สาเหตุแล้วรัน `migrate` อีกรอบ ซึ่งจะรันเฉพาะ `00037` · มีเวลาอย่างน้อย 7 วันก่อนถึง X ดูค่า X ได้จาก:

```sh
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml exec -u postgres postgres psql -U postgres -d aether \
  -c "SELECT conrelid::regclass, obj_description(oid,'pg_constraint') AS x FROM pg_constraint WHERE conname LIKE '%_cutover'"
```

เมื่อถึงเวลา X แถวใหม่จะชน CHECK และ ingest จะหยุด (collector ลองใหม่ไม่รู้จบ) **ถ้าใกล้ X แล้วยังรัน `00037` ไม่ได้** ให้ปลดก่อน (ทันที ไม่ scan):

```sh
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml exec -u postgres postgres psql -U postgres -d aether \
  -c "ALTER TABLE core.sensor_samples DROP CONSTRAINT IF EXISTS sensor_samples_cutover" \
  -c "ALTER TABLE core.ble_history DROP CONSTRAINT IF EXISTS ble_history_cutover"
```

`00037` ต้องมี CHECK นี้ ก่อนรัน `migrate` รอบถัดไปให้ใส่กลับด้วย X ใหม่ (วันจันทร์ 00:00 UTC ที่อยู่ข้างหน้าอย่างน้อย 7 วัน สำหรับ BLE ใช้เที่ยงคืน UTC ที่อยู่ข้างหน้า 7 วันได้) · `VALIDATE` scan ตารางโดย ingest ยังเขียนได้:

```sql
ALTER TABLE core.sensor_samples ADD CONSTRAINT sensor_samples_cutover CHECK (received_at < '2026-10-19T00:00:00Z') NOT VALID;
COMMENT ON CONSTRAINT sensor_samples_cutover ON core.sensor_samples IS '2026-10-19T00:00:00Z';
ALTER TABLE core.sensor_samples VALIDATE CONSTRAINT sensor_samples_cutover;
-- ทำแบบเดียวกันกับ core.ble_history / ble_history_cutover
```

**ซ้อมก่อนบนสำเนา** (ไม่แตะฐานจริง):

```sh
sudo sh infra/prod/backup-now.sh
sudo sh infra/prod/restore.sh aether-YYYYmmdd-HHMMSS.dump --database aether_rehearsal
# รัน migration กับ aether_rehearsal (image ใหม่ที่ build แล้ว) · goose พิมพ์เวลาของแต่ละไฟล์
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml run --rm --entrypoint sh migrate \
  -c 'MIGRATION_DATABASE_URL="$(echo "$MIGRATION_DATABASE_URL" | sed "s#/aether?#/aether_rehearsal?#")" exec /app/migrate'
# ต้องเห็น: 00036 ใช้เวลาตามขนาดตาราง (build index + validate) แต่ 00037 ต้องจบในไม่กี่วินาที
# (ถ้านานเป็นนาที แปลว่ามีการ scan หรือ build index ใหม่ ห้ามขึ้นจริงจนกว่าจะรู้สาเหตุ) และจำนวนแถวเท่าเดิม
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml exec -u postgres postgres psql -U postgres -d aether_rehearsal \
  -c "SELECT parent, covered_until, default_rows FROM core.partition_health()" \
  -c "SELECT count(*) FROM core.sensor_samples"
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml exec -u postgres postgres psql -U postgres -c "DROP DATABASE aether_rehearsal"
```

หลังขึ้นจริง API ดูแล partition เอง (ตอนเริ่มและทุกชั่วโมง): สร้างช่วงล่วงหน้า (sample 28 วัน, BLE 14 วัน) ทิ้งช่วงที่หมดอายุ (ไม่เกิน 2 ก้อนต่อรอบ · log ระดับ Warn ทุกครั้งที่ทิ้ง · ช่วงที่ว่างทิ้งได้เสมอ ช่วงที่มีข้อมูลทิ้งเมื่อ "ข้อมูลล่าสุดที่เก็บไว้" ยืนยันว่าหมดอายุจริง ไม่ใช่แค่นาฬิกาบอก กันกรณีนาฬิกาเครื่องกระโดดไปข้างหน้า · ถ้าไม่ผ่านจะเป็น `drop_held` เฉพาะตารางนั้น) และลบแถวหมดอายุใน `_legacy`/`_default` เป็นชุด · ถ้า constraint `*_cutover` ของ `00036` ยังค้าง (แปลว่า `00037` ยังไม่รัน) API จะ log `PARTITION ALERT` ตอนเริ่มและทุกชั่วโมง · `_legacy` ถูกทิ้งทั้งก้อนเมื่อ X เก่ากว่า retention (sample ราว 90 วันหลังอัปเกรด) · ผิดปกติเมื่อไรจะมี log `PARTITION ALERT` (§10)

### การย้อนกลับ — พูดตรง ๆ

**ไม่มีระบบ rollback อัตโนมัติ** migration ทุกไฟล์มีส่วน `-- +goose Down` แต่ `cmd/migrate` เรียกเฉพาะ `goose.Up` เท่านั้น ไม่มีคำสั่งสำหรับถอยลง

ถ้าอัปเกรดแล้วพัง มีสองทาง:

- **กู้ย้อนเวลา (แนะนำ)** ไปที่เวลาก่อน `migrate` ที่จดไว้ในข้อ 6.1 — ได้ข้อมูลถึงวินาทีนั้น และของเดิมยังอยู่ใน volume เก่า:
  ```sh
  sudo git checkout <commit เดิม>
  sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml build
  sudo sh infra/prod/restore-pitr.sh --target "2026-09-30 14:05:00+07" --cutover
  ```
- **กู้ dump ที่ทำไว้ก่อนอัปเกรด** แล้วกลับไปใช้โค้ดเวอร์ชันเดิม:

```sh
sudo git checkout <commit เดิม>
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml build
sudo sh infra/prod/restore.sh aether-YYYYmmdd-HHMMSS.dump --database aether --force
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d
```

ทั้งสองทาง ข้อมูลที่เข้ามาหลังจุดที่กู้จะหายไป — นี่คือเหตุผลที่ต้อง backup และจดเวลาทันทีก่อนอัปเกรด ไม่ใช่ "เมื่อคืน"

---

## 7. สำรองและกู้คืนข้อมูล

### ทำอะไรอัตโนมัติ

service `backup` ทำ `pg_dump --format=custom` ทุกคืนเวลา `BACKUP_AT` (ค่าเริ่มต้น 02:30 ตามโซนเวลาใน `AETHER_TZ`) เก็บที่ `<backup-dir>` (ค่าเริ่มต้น `/opt/aether/backups`)

- `daily/` เก็บ **14 ชุดล่าสุด**
- `weekly/` เก็บ **8 ชุดล่าสุด** (คืนวันอาทิตย์ ใช้ hard link จึงไม่กินพื้นที่เพิ่มจนกว่า daily จะถูกลบ)
- ทุกไฟล์ผ่าน `pg_restore --list` ทันทีหลังสร้าง ถ้าอ่านไม่ได้จะเปลี่ยนชื่อเป็น `.corrupt` และแจ้งใน log

```sh
sudo sh infra/prod/backup-now.sh                 # สำรองทันที
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml logs backup --tail 20
```

### สิ่งที่ **ไม่** ได้ครอบคลุม — อ่านให้ครบ

- dump คือจุดกู้วันละครั้ง ส่วนการกู้ **ย้อนไปวินาทีใดก็ได้** ใน 1–2 สัปดาห์ล่าสุดคือ PITR ใน §7.1 ใช้คู่กันเสมอ: dump อ่านได้ข้าม major version ของ PostgreSQL และกู้ทีละ database ได้ ส่วน PITR เสียข้อมูลไม่เกิน 5 นาที
- **dump ไม่มี secret** ต้องสำรองแยกและเก็บ **offline**:
  - `.env.prod` (มี `JWT_SIGNING_KEY`, `CHANNEL_SEAL_KEY`, รหัสฐานข้อมูล และ **`PGBACKREST_CIPHER_PASS`** — กุญแจเข้ารหัสคลัง PITR ถ้าหาย **backup ของ PITR ทุกชุดอ่านไม่ได้อีกเลย**)
  - `<secrets-dir>` ทั้งโฟลเดอร์ (MQTT CA key, ใบรับรอง broker, ไฟล์ตั้งค่า mosquitto)
  - Docker volume `caddy-data` (root CA ของ Caddy ในโหมด internal — ถ้าหายต้องไปติดตั้ง root ใหม่ที่เครื่องลูกข่ายทุกเครื่อง)
- **สำเนานอกเครื่องต้องเปิดเอง** (`setup.py --offsite`, §7.1) ถ้าไม่เปิด ดิสก์หรือเครื่องพัง = backup และคลัง PITR พังไปด้วย

```sh
# สำรอง secret ออฟไลน์ (ทำตอนติดตั้ง และทุกครั้งที่ rotate key)
sudo tar czf aether-secrets-$(date +%F).tar.gz .env.prod .secrets/prod
sudo docker run --rm -v aether-prod_caddy-data:/d -v "$PWD":/out alpine:3.23 \
     tar czf /out/aether-caddy-$(date +%F).tar.gz -C /d .
# → เข้ารหัสแล้วเก็บในตู้เซฟ / USB ที่ไม่ได้เสียบไว้กับเครื่อง
```

### กู้คืน

```sh
# ซ้อมกู้เข้า database ชั่วคราว (ปลอดภัย ไม่แตะของจริง)
sudo sh infra/prod/restore.sh aether-20260920-023001.dump --database aether_rehearsal

# กู้ทับของจริง (สคริปต์จะหยุด api / mqtt-ingest / mqtt-provisioner / mqtt-commander / tuya-cloud ให้ แล้วเปิดคืนอัตโนมัติ)
sudo sh infra/prod/restore.sh aether-20260920-023001.dump --database aether --force
```

`restore.sh` จะ **ปฏิเสธ** ถ้า database ปลายทางมีตารางอยู่แล้วและไม่ได้ใส่ `--force` (ออกด้วย exit code 2) และจะสร้าง role `aether_owner` / `aether_app` / `aether_mqtt_provisioner` ให้ก่อนเสมอ เพราะ dump มี ownership และ grant ติดมาด้วย

**การลบข้อมูลตาม PDPA ต้องทำซ้ำหลังกู้คืนทุกครั้ง** (ทั้ง dump และ PITR §7.1): สำรองที่เก่ากว่าการลบจะพาข้อมูลของคนที่ลบไปแล้วกลับมา และ `core.erasure_log` ในสำรองนั้นก็ไม่มีรายการลบ จึงต้องเก็บรายการจากฐานข้อมูลตัวจริง **ก่อน** กู้ แล้วรันซ้ำ **หลัง** กู้ (docs/platform/privacy.md):

```sh
# ก่อนกู้ (ถ้าตัวจริงยังเปิดได้) — ถ้าเปิดไม่ได้ ใช้ไฟล์ ledger ล่าสุดที่เก็บไว้
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml run --rm -T --entrypoint /app/admin migrate export-erasures > erasures-$(date +%F).jsonl
# ... restore.sh หรือ restore-pitr.sh ...
# หลังกู้: ลบซ้ำทุกรายการ (ทำซ้ำได้ ไม่เสียหาย) และใส่รายการ ledger ที่หายกลับ
# (รายการที่จะทำให้ workspace ไม่เหลือ owner จะถูกข้ามพร้อมแจ้ง ให้ใช้ admin erase-user --force <uuid> เองเมื่อมี owner อื่นแล้ว)
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml run --rm -T --entrypoint /app/admin migrate reapply-erasures < erasures-YYYY-MM-DD.jsonl
```
เก็บไฟล์ ledger (`export-erasures`) ไว้คู่กับ backup ด้วย เช่นทุกครั้งที่มีการลบ หรือทุกคืน: ไฟล์มีแค่รหัส uuid / MAC ของแท็ก ไม่มีชื่อหรืออีเมล

### 7.1 กู้คืนย้อนเวลา (PITR) ด้วย pgBackRest

**ทำงานอย่างไร**

- `postgres` ส่ง WAL ทุกไฟล์เข้าคลัง pgBackRest ที่ `<pitr-dir>` (ค่าเริ่มต้น `/var/lib/aether/pitr` เมื่อรัน setup ด้วย root — นอก checkout เพื่อไม่ให้ `git clean -fdx` ลบทิ้ง; ถ้าตั้งไว้ใน repository เช่น `pitr/` ห้ามใช้ `git clean -fdx` เด็ดขาด) แบบ async `archive_timeout=300` บังคับปิดไฟล์ WAL ทุก 5 นาที ข้อมูลที่อาจหายเมื่อกู้จึง **ไม่เกิน 5 นาที**
- service `pitr` ทำ **full backup ทุกวันอาทิตย์** และ **differential ทุกวัน** เวลา `PITR_BACKUP_AT` (03:30) เก็บ full ล่าสุด `PITR_KEEP_FULL` ชุด (2) จึง **กู้ได้ย้อนหลัง 7–14 วัน** ถึงวินาทีใดก็ได้ WAL และ backup ที่เก่ากว่านั้นถูกลบให้เอง
- ทุกไฟล์ในคลัง **บีบอัด (zstd) และเข้ารหัส (aes-256-cbc)** ด้วย `PGBACKREST_CIPHER_PASS` จาก `.env.prod`
- ถ้าคลังเขียนไม่ได้ (ดิสก์เต็ม, เมานต์หลุด) WAL จะค้างใน `pg_wal` ได้ถึง `PITR_QUEUE_MAX` (8GB) จากนั้น pgBackRest **ทิ้ง WAL และแจ้งเตือน** แทนที่จะปล่อยให้ดิสก์เต็มจนฐานข้อมูลหยุด ช่วงที่ถูกทิ้งจะกู้ย้อนเวลาไม่ได้ และเมื่อคลังกลับมาเขียนได้ `pitr` จะทำ full backup ใหม่ให้ทันที
- `postgres` รันใต้ init (`init: true`): process ของ archive แบบ async ที่หลุดจากแม่จะไม่ถูกนับเป็น backend ที่ crash

**การแจ้งเตือน** — ทุก 5 นาที `pitr` ตรวจ: archive ล้มเหลว, WAL ค้างส่งเกิน 15 นาที, `pg_wal` ใหญ่เกิน `max_wal_size` + 2GB, WAL ถูกทิ้ง, backup ล่าสุดเก่ากว่า 36 ชม., backup ล้มเหลว, ดิสก์คลังเหลือ < 15%, dump รายคืนเก่ากว่า 30 ชม. หรือล้มเหลว

- เขียนบรรทัด `PITR ALERT <key>: ...` / `PITR RESOLVED <key>` ใน log ของ `pitr`
- POST JSON (`{"severity","key","text","content"}` ใช้กับ Slack/Discord webhook ได้ตรง ๆ) ไปที่ `--ops-webhook` ถ้าตั้งไว้ ส่งซ้ำทุก 6 ชม. ระหว่างที่ปัญหายังอยู่ และส่งอีกครั้งเมื่อหาย URL (ซึ่งมักเป็นรหัสลับ) เก็บใน `<secrets>/ops/webhook-url` (0400) ไม่อยู่ใน `.env.prod` และไม่ปรากฏใน argument ของ process ใด `pitr` และ `pitr-offsite` อยู่บน network `ops-egress` ที่ออกอินเทอร์เน็ตได้อย่างเดียว ไม่อยู่ network เดียวกับแอป (เข้าถึงฐานข้อมูลผ่าน socket volume)
- สรุปใน `/run/aether-ops/status.json` ซึ่ง healthcheck ของ `pitr` อ่าน → `docker compose ps` แสดง `unhealthy` ทันทีที่มีปัญหา

```sh
sudo sh infra/prod/pitr-info.sh            # backup ที่มี, ช่วง WAL, และ status.json
sudo sh infra/prod/pitr-backup-now.sh      # full backup ทันที (diff / incr ก็ได้)
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml logs pitr | grep 'PITR ALERT'
```

**เปิดใช้บนเครื่องที่ติดตั้งไว้แล้ว** (หยุดชะงักราว 10–30 วินาที ตอน postgres restart เพื่อเปิด `archive_mode`)

```sh
cd /opt/aether
sudo sh infra/prod/backup-now.sh                         # dump ก่อนเสมอ
df -h .                                                  # ต้องมีที่ว่าง ≥ 2 × ขนาดฐานข้อมูล
sudo git pull
sudo python3 infra/prod/setup.py <flag ชุดเดิมทั้งหมด>    # เพิ่ม AETHER_PITR_DIR, AETHER_PG_VOLUME, PGBACKREST_CIPHER_PASS
sudo tar czf aether-secrets-$(date +%F).tar.gz .env.prod .secrets/prod   # สำรอง passphrase ใหม่ออฟไลน์ทันที
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml build postgres
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d  # postgres restart ครั้งเดียว + service pitr ใหม่
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml logs -f pitr   # รอ "full backup ok"
sudo sh infra/prod/pitr-drill.sh                         # ซ้อมกู้ครั้งแรกวันเดียวกัน ต้องได้ PASS
```

`AETHER_PG_VOLUME` ที่ `setup.py` เขียนคือ `aether-prod_postgres-data` ซึ่งเป็นชื่อ volume เดิมที่ compose ตั้งให้ ข้อมูลจึงไม่ย้ายไปไหน

- volume ฐานข้อมูลเป็น **external** ใน compose: compose ไม่สร้างและไม่ลบ (แม้ `down -v`) — `setup.py` สร้างให้ตอนติดตั้งใหม่ (label เดียวกับที่ compose ใส่) และ volume ที่ `restore-pitr.sh` สร้างก็ได้ label ชุดเดียวกัน
- **ตัวแปรใน shell ชนะ `.env.prod`** (ลำดับความสำคัญของ compose): ถ้าเคย `export AETHER_PG_VOLUME=...` ไว้ compose จะใช้ค่านั้น สคริปต์ใน `infra/prod` จะเตือนเมื่อค่าใน shell ไม่ตรงกับไฟล์ และ `restore-pitr.sh` จะไม่ยอมสลับ volume จนกว่าจะ `unset`
- `setup.py` **ปฏิเสธ** ถ้า `<pitr-dir>` มีคลังอยู่แล้วแต่ `.env.prod` ไม่มี `PGBACKREST_CIPHER_PASS` (passphrase ใหม่อ่านคลังเดิมไม่ได้) — ให้กู้ `.env.prod` จากสำเนาออฟไลน์ก่อน

**กู้ย้อนเวลา**

```sh
# 1) ดูว่ากู้ได้ช่วงไหน
sudo sh infra/prod/pitr-info.sh

# 2) กู้ลง volume ใหม่ ของจริงไม่ถูกแตะ (ต้องใส่ time zone ของเวลาเสมอ)
sudo sh infra/prod/restore-pitr.sh --target "2026-09-30 14:05:00+07"
#    → สร้าง aether-prod_postgres-data-r<เวลา UTC>, replay WAL ถึงเวลานั้นในสำเนาที่ไม่มี network,
#      แล้วแสดง migration version, RLS และจำนวนแถวทุกตาราง เทียบกับของจริง
#    --keep-running  เปิดสำเนาค้างไว้ให้ตรวจเอง: docker exec -it -u postgres aether-prod-pitr-verify psql -d aether

# 3) สลับไปใช้ volume ที่กู้ (หยุดระบบสั้น ๆ): dump ของเดิมก่อน, สลับ AETHER_PG_VOLUME ใน .env.prod,
#    เปิดคืนเฉพาะ service ที่รันอยู่ก่อนหน้า แล้วทำ full backup ของ timeline ใหม่ให้
sudo sh infra/prod/restore-pitr.sh --use-volume aether-prod_postgres-data-r20260930070500
#    หรือทำข้อ 2 + 3 ในคำสั่งเดียว: restore-pitr.sh --target "..." --cutover

# 4) ถอยกลับไป volume เดิม (ยังอยู่ครบ): ใช้ชื่อที่สคริปต์พิมพ์ไว้ / AETHER_PG_VOLUME_PREVIOUS ใน .env.prod
sudo sh infra/prod/restore-pitr.sh --use-volume aether-prod_postgres-data
```

- **ฐานข้อมูลตัวจริงเสียจนเปิดไม่ได้ก็กู้ได้** สคริปต์อ่านคลังผ่าน container ชั่วคราว ไม่ต้องพึ่ง postgres ตัวเดิม (แค่ข้ามการเทียบกับของจริง)
- **timeline**: กู้ตามประวัติที่ฐานข้อมูลตัวจริงอยู่ตอนนี้ (ถ้าตัวจริงดับ ใช้ timeline ของ backup ล่าสุดก่อนเวลาเป้าหมาย) เคยสลับไปมาแล้วอยากกู้ตามอีกกิ่ง ใส่ `--timeline N` ตัวเลขดูได้จาก `pitr-info.sh`
- volume ที่ตามหลัง timeline ล่าสุดของคลัง (เช่นถอยกลับไป volume เดิมหลัง cutover) จะถูก **ย้ายขึ้น timeline ใหม่ก่อนเปิด** ให้อัตโนมัติ เพราะถ้าเดินต่อบน timeline เก่า ชื่อ WAL จะเรียงต่ำกว่า timeline ใหม่ และ pgBackRest จะลบ WAL ที่ backup ยังต้องใช้ทิ้ง ใช้ได้ทั้ง volume ที่หยุดเรียบร้อยและที่ดับกะทันหัน (replay WAL ของตัวเองจนจบก่อน)
- หลังสำเนา promote สคริปต์ลบ `recovery_target_*` ที่ pgBackRest เขียนไว้ใน `postgresql.auto.conf` ออก (`ALTER SYSTEM RESET`) เพื่อไม่ให้ค่าเก่ามีผลกับการกู้ volume นั้นครั้งถัดไป
- ถ้า cutover ล้มกลางทาง (volume ใหม่เปิดไม่ขึ้น) สคริปต์สลับ `AETHER_PG_VOLUME` กลับและเปิดระบบบน volume เดิมให้เอง
- ถ้าขึ้นว่า `recovery ended before configured recovery target was reached` แปลว่าคลังจบก่อนเวลาเป้าหมาย (ตัวจริงดับก่อนส่ง WAL ช่วงท้าย) ให้เลือกเวลาก่อน `last completed transaction` ที่แสดงใน log
- ไม่ใช้แล้วลบ volume ที่กู้ได้: `sudo docker volume rm <ชื่อ>` (volume เดิมหลัง cutover ก็เช่นกัน เมื่อแน่ใจแล้ว)

**ซ้อมกู้ย้อนเวลา — ทุกเดือน** (ไม่แตะของจริง ใช้เวลาไม่กี่นาที)

```sh
sudo sh infra/prod/pitr-drill.sh           # กู้ไป "10 นาทีก่อน" ลง volume ชั่วคราว แล้วลบทิ้ง
tail -3 backups/drills.log                 # ... window_rows restored=N live=N PASS
```

drill ผ่านเมื่อสำเนา promote ได้, migration version ตรงกับของจริง, RLS ครบ และจำนวนแถวของ `core.sensor_samples` ช่วง 1 ชม. ก่อนเวลาเป้าหมายตรงกันทั้งสองฝั่ง ผลทุกครั้ง (รวม FAIL) ถูกต่อท้ายใน `<backup-dir>/drills.log` ตั้ง cron บน host ได้:
`0 4 1 * * cd /opt/aether && sh infra/prod/pitr-drill.sh >> /var/log/aether-drill.log 2>&1`

**สำเนานอกเครื่อง (off-site)**

```sh
# 1) สร้าง rclone remote แบบ crypt ครอบ S3 / SFTP / NAS (dump ไม่ได้เข้ารหัสในตัว คลัง PITR เข้ารหัสแล้ว)
sudo mkdir -p .secrets/prod/rclone
sudo docker run --rm -it -v "$PWD/.secrets/prod/rclone:/config/rclone" rclone/rclone:1.71.1 config
# 2) เปิด: service pitr-offsite คัดลอก <pitr-dir> และ <backup-dir> ทุก 15 นาที
sudo python3 infra/prod/setup.py <flag ชุดเดิม> --offsite offsite-crypt:aether
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d
# ปิด: --offsite off
```

ไฟล์ที่ retention ลบในเครื่องจะถูกย้ายไป `<remote>/deleted/<วันที่>/` บนปลายทาง ไม่ลบทิ้ง ตั้ง lifecycle ของ bucket ให้ลบโฟลเดอร์นั้นเองตามนโยบาย การ sync แยกจากการ archive โดยตั้งใจ: ปลายทางล่มไม่ทำให้ archive ของ WAL สะดุด

**รู้ไว้**

- ข้อมูลที่ลบในระบบ (เช่นคำขอลบข้อมูลส่วนบุคคล) ยังอยู่ในคลัง PITR ได้ถึง 14 วัน และใน dump ได้ถึง 8 สัปดาห์
- restart `pitr` ทำให้สถานะการแจ้งเตือนเริ่มใหม่: ปัญหาที่ยังอยู่จะถูกแจ้งซ้ำหนึ่งครั้ง ส่วนที่หายไประหว่างนั้นจะไม่มีข้อความ RESOLVED
- อัปเกรด PostgreSQL major (เช่น 19) ต้อง `pgbackrest stanza-upgrade` แล้วทำ full backup ใหม่ ส่วน backup ของเวอร์ชันเก่าใช้กู้ด้วย image เก่าเท่านั้น

### รายการซ้อมกู้คืนจาก dump — ทำทุก 3 เดือน

1. `sudo sh infra/prod/backup-now.sh` แล้วจดชื่อไฟล์
2. `sudo sh infra/prod/restore.sh <ไฟล์> --database aether_rehearsal`
3. เทียบจำนวนแถวระหว่างของจริงกับที่กู้มา:
   ```sh
   for db in aether aether_rehearsal; do
     sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml exec -u postgres postgres \
       psql -U postgres -d $db -c "SELECT 'devices',count(*) FROM core.devices
                                   UNION ALL SELECT 'samples',count(*) FROM core.sensor_samples
                                   UNION ALL SELECT 'users',count(*) FROM identity.users;"
   done
   ```
4. ตัวเลขต้องตรงกัน (ยกเว้น sample ที่เข้ามาหลังทำ dump)
5. ลบ database ซ้อม: `... psql -U postgres -d postgres -c 'DROP DATABASE aether_rehearsal'`
6. ทดสอบกู้ **จากสำเนาที่เก็บนอกเครื่อง** อย่างน้อยปีละครั้ง ไม่ใช่จากไฟล์บนเครื่องเดิม
7. จดวันที่ซ้อมและผลลัพธ์ไว้

---

## 8. ปฏิทินใบรับรอง

| ใบรับรอง | อายุ | ต่ออายุอย่างไร | ถ้าหมดอายุจะเป็นอย่างไร |
|---|---|---|---|
| **MQTT broker** (`mqtt/broker/server.crt`) | **825 วัน** | `sudo python3 infra/prod/renew-mqtt-cert.py` แล้ว `docker compose ... kill -s HUP mqtt` (หรือ `restart mqtt`) | **gateway ทุกตัวหยุดส่งข้อมูล** แต่ไม่ต้องไปแตะ gateway เพราะ CA เดิมยังใช้ได้ |
| **MQTT CA** (`mqtt/ca.crt`) | **10 ปี** | ต้องสร้าง CA ใหม่ และ **อัปโหลด ca.crt ใหม่เข้า MG3 ทุกตัว** | gateway ทุกตัวหยุดส่งข้อมูลและต้องเดินไปตั้งค่าทีละตัว → ตั้งเตือนล่วงหน้า **1 ปี** |
| **เว็บ โหมด `internal`** | leaf สั้น ต่อเองอัตโนมัติ, root 10 ปี | อัตโนมัติ | root หมดอายุ = เบราว์เซอร์ทุกเครื่องเตือน ต้องติดตั้ง root ใหม่ |
| **เว็บ โหมด `acme`** | 90 วัน | Caddy ต่อเองอัตโนมัติ (ต้องให้พอร์ต 80/443 เข้าถึงได้จากภายนอกตลอด) | เว็บใช้ไม่ได้ |
| **PostgreSQL** (`postgres/server.crt`) | 10 ปี | ลบ `postgres/server.crt` + `server.key` แล้วรัน `setup.py` ใหม่ จากนั้น `up -d` | api / collector ต่อฐานข้อมูลไม่ได้เลย |

ลงปฏิทินไว้: **เดือนที่ 24 นับจากติดตั้ง** → ต่อใบ broker, **ปีที่ 9** → วางแผนเปลี่ยน CA

ดูวันหมดอายุตอนนี้:

```sh
openssl x509 -enddate -noout -in /opt/aether/.secrets/prod/mqtt/broker/server.crt
openssl x509 -enddate -noout -in /opt/aether/.secrets/prod/mqtt/ca.crt
```

---

## 9. การเปลี่ยนกุญแจ (key rotation)

| กุญแจ | เปลี่ยนได้ไหม | ผลที่ตามมา |
|---|---|---|
| `JWT_SIGNING_KEY` | ได้ | **ผู้ใช้ทุกคนหลุดออกจากระบบทันที** ต้อง login ใหม่ ไม่มีข้อมูลสูญหาย ทำตอนกลางคืน |
| `CHANNEL_SEAL_KEY` | **ยังทำไม่ได้แบบไม่สูญข้อมูล** | ถ้าเปลี่ยนค่า ความลับของช่องทางแจ้งเตือน (LINE token, header ของ webhook) ที่ถูก seal ด้วยกุญแจเดิมจะเปิดไม่ได้ ต้องเข้าไปกรอกใหม่ทุกช่องทางในเว็บ |
| `APP_DB_PASSWORD` | ได้ | `ALTER ROLE aether_app PASSWORD ...` แล้วแก้ `.env.prod` และ `up -d` |
| รหัส MQTT ของ gateway | ได้ต่อตัว | กด rotate ในเว็บ แล้วเอารหัสใหม่ไปใส่ที่ MG3 ตัวนั้น ตัวเก่าใช้ไม่ได้ทันที |

**`CHANNEL_SEAL_KEY` หายเท่ากับความลับช่องทางแจ้งเตือนหายทั้งหมด** — dump ของฐานข้อมูลช่วยไม่ได้ เพราะข้อมูลในนั้นถูกเข้ารหัสด้วยกุญแจนี้ เก็บสำเนา `.env.prod` ไว้นอกเครื่องเสมอ

ขั้นตอนเปลี่ยน `JWT_SIGNING_KEY`:

```sh
NEW=$(python3 -c "import base64,secrets;print(base64.b64encode(secrets.token_bytes(48)).decode())")
sudo sed -i "s|^JWT_SIGNING_KEY=.*|JWT_SIGNING_KEY=$NEW|" .env.prod
unset NEW
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d api mqtt-ingest
```

---

## 10. ที่อยู่ของ log และการตรวจสุขภาพ

| อะไร | อยู่ที่ไหน |
|---|---|
| log ของทุก service | `docker compose --env-file .env.prod -f infra/prod/compose.yaml logs <service>` (json-file, หมุนที่ 20 MB × 5 ไฟล์ ต่อ container) |
| access log ของเว็บ | `<log-dir>/access.log` (ค่าเริ่มต้น `infra/prod/logs/access.log`) รูปแบบ JSON หมุนที่ 20 MiB เก็บ 10 ไฟล์ / 90 วัน |
| log ของ PostgreSQL | อยู่ใน log ของ container `postgres` (query ที่ช้ากว่า 500 ms, checkpoint, lock wait, autovacuum) |
| ข้อมูลจริง | Docker volume ตาม `AETHER_PG_VOLUME` ใน `.env.prod` (ค่าเริ่มต้น `aether-prod_postgres-data`) |
| ไฟล์สำรอง | `<backup-dir>` (dump + `drills.log`) และ `<pitr-dir>` (คลัง pgBackRest) |
| สุขภาพการสำรอง | `infra/prod/pitr-info.sh` หรือ `/run/aether-ops/status.json` ใน container `pitr`; บรรทัด `PITR ALERT` ใน log ของ `pitr` |

```sh
# สุขภาพรวม
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml ps
curl -s https://aether.hospital.local/health/ready      # {"status":"ready"}
curl -s https://aether.hospital.local/health/live       # {"status":"ok"}

# ดู request ที่ตอบ 5xx ใน access log
sudo grep -h '"status":5' <log-dir>/access.log | tail -20
```

### สิ่งที่ควรเฝ้า

| ตัวชี้วัด | เกณฑ์ที่ควรทำอะไรสักอย่าง |
|---|---|
| พื้นที่ดิสก์เหลือ | < 20% |
| `docker compose ps` | มี service ไหน ไม่ใช่ `healthy` นานกว่า 5 นาที |
| ไฟล์ backup ล่าสุด | เก่ากว่า 36 ชั่วโมง (`pitr` แจ้งเองเมื่อ dump เก่ากว่า 30 ชม. หรือ backup ของ pgBackRest เก่ากว่า 36 ชม.) |
| `pitr` / `backup` | `unhealthy` = มีปัญหาการสำรอง ดู `logs pitr` บรรทัด `PITR ALERT` |
| ขนาด `core.sensor_samples` | โตเร็วกว่าที่ประมาณไว้ในข้อ 1 เกิน 30% → ลด interval หรือ retention |
| `logs api` บรรทัด `PARTITION ALERT` | มีแถวตกใน partition `DEFAULT`, partition ล่วงหน้าเหลือไม่ถึง 7 วัน (งานดูแล partition ไม่ได้รันหรือล้ม ดู `partition maintenance failed`) หรือ constraint `*_cutover` ค้าง (`00037` ยังไม่รัน ดู §6) · `drop_held` ระดับ Warn = partition หมดอายุตามนาฬิกาแต่ข้อมูลล่าสุดยังไม่ยืนยัน (นาฬิกาเครื่องเดินเร็วเกิน? ตรวจ `timedatectl`) |
| `logs api` บรรทัด `access log write failed` / ผู้ใช้เห็น 503 | บันทึกการเข้าถึงข้อมูลส่วนบุคคล (`core.access_log`) เขียนไม่ได้ API จึงไม่ส่งข้อมูลนั้นออก (fail closed): ตรวจฐานข้อมูล ดิสก์เต็ม หรือ partition ของ `access_log` (ดู `PARTITION ALERT`) |
| HTTP 429 ใน access log | ถ้าเยอะจากผู้ใช้จริง ให้เพิ่ม `API_RATE_LIMIT` |
| จำนวน gateway ที่ออนไลน์ | ลดลงโดยไม่มีเหตุผล = ปัญหาเครือข่าย หรือใบรับรองหมดอายุ |
| เวลาเซิร์ฟเวอร์ | `timedatectl` ต้อง synchronized ตลอด |

```sh
# ขนาดตารางที่โตเร็วที่สุด
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml exec -u postgres postgres \
  psql -U postgres -d aether -c "SELECT c.relname, pg_size_pretty(sum(pg_total_relation_size(t.relid))) size
    FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace, pg_partition_tree(c.oid) t
    WHERE n.nspname IN ('core','identity') AND c.relkind IN ('r','p') AND NOT c.relispartition AND t.isleaf
    GROUP BY c.relname ORDER BY sum(pg_total_relation_size(t.relid)) DESC LIMIT 10;"
# ตารางที่แบ่ง partition (sensor_samples, ble_history) รวมทุก partition แล้ว · ดูราย partition:
#   SELECT * FROM core.partition_health();   และ   \d+ core.sensor_samples
```

---

## 11. ตารางแก้ปัญหา

| อาการ | สาเหตุที่พบบ่อย | ทำอย่างไร |
|---|---|---|
| เว็บเปิดไม่ขึ้นเลย | `proxy` ไม่ healthy หรือ firewall ปิด 443 | `... ps` และ `... logs proxy --tail 50`; `sudo ufw status` |
| เบราว์เซอร์เตือน "ไม่ปลอดภัย" | โหมด `internal` และยังไม่ได้ติดตั้ง root cert | ทำตามข้อ 2 หัวข้อ root certificate |
| เข้าเว็บได้แต่ login ไม่ผ่าน ขึ้น 403 | `APP_ORIGIN` ไม่ตรงกับ URL ที่พิมพ์ (เช่นพิมพ์ IP แต่ตั้งเป็นชื่อ DNS) | ต้องเข้าด้วย URL เดียวกับ `AETHER_SITE` ใน `.env.prod` เป๊ะ ๆ |
| ขึ้น 429 Too Many Requests | ชน rate limit (`API_RATE_LIMIT`, และ 10 ครั้ง/นาที สำหรับ login) | รอ 1 นาที; ถ้าเกิดกับผู้ใช้จริงบ่อย เพิ่มค่าใน `.env.prod` แล้ว `up -d api` |
| `api` ไม่ขึ้น healthy | ตั้งค่าไม่ผ่าน (`configuration rejected`) หรือต่อฐานข้อมูลไม่ได้ | `... logs api --tail 50` ดูบรรทัด `reason` |
| `api` ขึ้น `production DATABASE_URL requires sslmode=verify-full` | มีใครแก้ `.env.prod` หรือ compose | ตรวจว่า `DATABASE_URL` ยังมี `sslmode=verify-full&sslrootcert=/run/db-ca/ca.crt` |
| `postgres` ขึ้น `private key file must be owned by...` | volume `postgres-certs` ค้างของเก่า | `... up -d --force-recreate prepare postgres` |
| gateway ต่อ broker ไม่ได้ | ใบรับรองหมดอายุ / เวลาบน MG3 ผิด / ใส่ CA ผิดไฟล์ / รหัสผ่านผิด | `... logs mqtt --tail 50` จะบอกชัดว่า `not authorised` (รหัสผิด) หรือ TLS handshake ล้มเหลว (CA/เวลา) |
| gateway ต่อได้แต่ไม่มีข้อมูล | topic ไม่ตรง หรือ format ไม่ใช่ JSON-LONG | เทียบ topic กับที่เว็บแสดง; `... logs mqtt-ingest` |
| `mqtt-provisioner` restart วน | ต่อฐานข้อมูลไม่ได้ (รหัส `aether_mqtt_provisioner` ไม่ตรง) | เกิดเมื่อ volume ฐานข้อมูลเก่ากว่า `.env.prod`: `ALTER ROLE aether_mqtt_provisioner PASSWORD '<ค่าใน .env.prod>'` |
| ดิสก์เต็ม | sample สะสม + backup + คลัง PITR | ลด `SAMPLE_RETENTION_DAYS`, `BACKUP_KEEP_DAILY` หรือ `--pitr-keep-full`; ลบ volume `...-r<เวลา>` ที่กู้ไว้แล้วไม่ใช้; `docker system prune -f` |
| `PITR ALERT archive_failing` / `wal_growing` | คลัง PITR เขียนไม่ได้ (ดิสก์เต็ม, สิทธิ์, เมานต์หลุด) | `... logs postgres --tail 50` ดู error ของ pgbackrest; แก้แล้ว WAL ที่ค้างจะถูกส่งเอง |
| `PITR ALERT wal_dropped` | archive ล้มเหลวนานจน WAL ค้างเกิน `PITR_QUEUE_MAX` | แก้ต้นเหตุ; `pitr` ทำ full backup ใหม่เองเมื่อ archive กลับมา (หรือ `pitr-backup-now.sh`) ช่วงที่ทิ้งไปกู้ย้อนเวลาไม่ได้ |
| `pitr` ขึ้น `unable to find a valid repository` | `.env.prod` หรือ `<secrets>/pgbackrest/pgbackrest.conf` ไม่ตรงกับคลัง (passphrase ผิด) | กู้ `.env.prod` จากสำเนาออฟไลน์ แล้วรัน `setup.py` ซ้ำ **ห้ามลบคลังเพื่อเริ่มใหม่** ก่อนแน่ใจ |
| อัปเกรดแล้ว `migrate` fail | migration ชนกับข้อมูลเดิม | **อย่ารัน `up -d`**; อ่าน error, กู้ dump ก่อนอัปเกรด (ข้อ 6) |
| เปลี่ยน IP เซิร์ฟเวอร์แล้วพัง | ใบรับรอง MQTT ผูกกับ `--host` | ออกใบใหม่: `renew-mqtt-cert.py --host <ค่าใหม่>` + แก้ `.env.prod` + `up -d`; ถ้า `--host` เป็นชื่อ DNS แค่แก้ DNS พอ |

---

## 12. ข้อควรรู้ด้านความปลอดภัยของชุดนี้

- ทุก container รันแบบ `read_only` root filesystem, `cap_drop: ALL`, `no-new-privileges` ยกเว้นเท่าที่จำเป็น (postgres ต้องมี SETUID/SETGID เพื่อลดสิทธิ์ตัวเอง, proxy ต้องมี NET_BIND_SERVICE เพื่อจับพอร์ต 80/443, `prepare` ต้องมี CHOWN)
- **PostgreSQL ไม่ publish พอร์ตออกจากเครื่อง** และ `pg_hba.conf` บังคับ `hostssl` — การต่อผ่าน TCP โดยไม่เข้ารหัสถูกปฏิเสธทุกกรณี
- API เชื่อ `X-Forwarded-For` เฉพาะเมื่อ peer คือ IP ของ proxy ที่ระบุไว้ใน `TRUSTED_PROXIES` (ตรึงด้วย subnet คงที่ใน compose) และ Caddy **เขียนทับ** header นี้ด้วย IP จริงของผู้เรียก ไม่ใช่ต่อท้าย — ผู้ใช้จึงปลอม IP เพื่อเลี่ยง rate limit ไม่ได้
- `Content-Security-Policy` ที่ตั้งไว้ถูกทดสอบกับเบราว์เซอร์จริง (Chrome) ทุกหน้าแล้วไม่มี violation โดยยังต้องมี `'unsafe-inline'` สำหรับ script/style เพราะ framework ฝัง bootstrap script แบบ inline และ widget preview เรนเดอร์ HTML ใน `iframe srcDoc` — **ไม่ได้ใช้ `'unsafe-eval'`** ถ้าแก้หน้าเว็บแล้วเจอ error `Refused to ...` ใน console ให้แก้โค้ดก่อน อย่าเพิ่งขยาย policy
- `mosquitto.conf` ของ production ไม่มี listener 1883 เว้นแต่เปิด `--mqtt-plaintext` (ยังบังคับรหัสและ ACL) และไม่มีบัญชี gateway ฝังไว้ล่วงหน้า ทุกบัญชีเกิดจากการสร้าง gateway ในเว็บ

---

## 13. ข้อจำกัดที่ต้องรู้ก่อนขยายระบบ

1. **เซิร์ฟเวอร์เดียว ไม่มี HA** เครื่องดับ = ระบบดับ ไม่มี failover ไม่มี replica
2. **PITR อยู่บนเครื่องเดียวกัน** จนกว่าจะเปิด `--offsite` (ข้อ 7.1) ดิสก์พัง = คลัง PITR และ dump หายพร้อมกัน
3. **Rate limit เป็นแบบ per-process ในหน่วยความจำ** ถ้าเพิ่ม replica ของ api ในอนาคต โควตาจะคูณตามจำนวน replica ต้องเปลี่ยนไปใช้ rate limiter ที่แชร์กัน
4. **`core.sensor_samples` และ `core.ble_history` แบ่ง partition ตามเวลาแล้ว** (migration `00037`) ข้อมูลหมดอายุทิ้งทั้งก้อน ไม่ bloat · query "ค่าล่าสุด" ที่ไม่มีช่วงเวลาจะอ่าน index ของทุก partition (ราว 15–20 ก้อนที่ retention 90 วัน) ยังเร็ว แต่ถ้าตั้ง `SAMPLE_RETENTION_DAYS` เป็นหลายปี จำนวน partition จะเป็นหลักร้อยและ planning ช้าลง ถ้าเกินหลักพัน tag ให้พิจารณา TimescaleDB
5. **หน้าเว็บรันบน `vinext` 1.0.0-beta.5** ซึ่งเป็น beta ยังไม่ใช่ runtime ที่มี track record ยาว ๆ ให้ยึดตามเวอร์ชันที่ pin ไว้ใน `package.json` อย่าอัปเองตามใจ
6. **Frame ของ Minew ยังไม่ได้ยืนยันครบ** packet ถูกเก็บดิบ (`decoded:false`) การเห็นค่าบน dashboard ขึ้นกับ decoder ใน Studio ซึ่งยังไม่ผ่านการตรวจกับฮาร์ดแวร์ทุกรุ่น
7. **collector ต้องมี binding อย่างน้อย 1 รายการ** ตามรูปแบบไฟล์ปัจจุบัน `setup.py` จึงใส่ binding ไปที่ topic สงวน `/aether/reserved/<uuid>` ที่ไม่มีบัญชีใด publish ได้ gateway จริงทั้งหมดวิ่งผ่าน subscription `/aether/gateways/+/status` ซึ่งเป็นคนละเส้นทาง · หมายเหตุ 2026-09-20: backend รับรายการ binding ว่างได้แล้ว แต่ชุดติดตั้งยังคง binding สงวนนี้ไว้ เพราะเป็นรูปแบบที่ผ่านการทดสอบติดตั้งจริงทั้งชุด
8. **ไม่มีการทดสอบเจาะระบบจากภายนอก และยังไม่ได้ load test ที่ 5,000 event/วินาที** อย่าอ้างว่าผ่านมาตรฐานใด ๆ จากชุดนี้
9. **Mosquitto บันทึกลงดิสก์ทุก 30 วินาที** ถ้าไฟดับกะทันหัน ข้อความที่ค้างในคิวอาจหาย — ไม่ใช่การรับประกันแบบ zero loss
10. **ระบบแจ้งเตือนขึ้นกับเครือข่ายขาออก** ถ้า LAN ของโรงพยาบาลไม่ให้ออกอินเทอร์เน็ต LINE/อีเมลจะส่งไม่ได้ ต้องตั้ง `WEBHOOK_ALLOWED_HOSTS` ให้ชี้ไป relay ภายใน และตั้ง `SMTP_*` ให้ใช้ mail server ภายใน
