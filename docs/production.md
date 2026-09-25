# Aether Production Runbook (on-premise, 1 เซิร์ฟเวอร์)

คู่มือนี้เขียนให้เจ้าของระบบที่ติดตั้งและดูแลเซิร์ฟเวอร์เอง ทุกคำสั่งรันจาก **repository root** บนเครื่องเซิร์ฟเวอร์

ชุดไฟล์ production ทั้งหมดอยู่ใน `infra/prod/` และเป็นชุด **แยกจาก dev** (`infra/compose.yaml`) โดยสิ้นเชิง — คนละ compose project คนละ volume คนละ secret

| ไฟล์ | หน้าที่ |
|---|---|
| `infra/prod/setup.py` | สร้าง secret, ใบรับรอง, ไฟล์ตั้งค่า mosquitto และ `.env.prod` (idempotent) |
| `infra/prod/compose.yaml` | postgres, migrate, api, mqtt, mqtt-ingest, mqtt-provisioner, web, proxy, backup |
| `infra/prod/Caddyfile` | reverse proxy + TLS + security headers (ค่าทุกอย่างมาจาก `.env.prod`) |
| `infra/prod/init-db.sh` | สร้าง role และบังคับ `hostssl` ตอน volume ใหม่ |
| `infra/prod/create-owner.sh` | สร้างบัญชีเจ้าของคนแรก |
| `infra/prod/backup-now.sh` / `backup.sh` / `backup-loop.sh` | สำรองข้อมูล |
| `infra/prod/restore.sh` / `restore-inner.sh` | กู้คืน |
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

ผลที่ถูกต้อง: `postgres`, `api`, `mqtt`, `mqtt-ingest`, `web`, `proxy` = `Up (healthy)`; `prepare` และ `migrate` = `Exited (0)`; `mqtt-provisioner`, `backup` = `Up`

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
| `--upstream-proxy` | IP/CIDR ของ proxy ด้านหน้าที่ Caddy เชื่อ `X-Forwarded-For` (อ่านจากขวาไปซ้าย `trusted_proxies_strict`) · nginx ที่ต่อเข้า `127.0.0.1` จะปรากฏเป็น gateway ของ subnet compose คือ `.1` |
| `--web-bind` | IPv4 ที่ Caddy เปิดพอร์ต · `127.0.0.1` = เข้าได้เฉพาะผ่าน nginx |

ฝั่ง nginx: `proxy_pass https://127.0.0.1:8443;` พร้อม `proxy_ssl_server_name on; proxy_ssl_name <host>;` ตรวจใบของ Caddy ด้วย root ที่ดึงจาก `proxy:/data/caddy/pki/authorities/local/root.crt`, ส่ง `Host $host`, **เขียนทับ** `X-Forwarded-For $remote_addr` (ไม่ append) และส่ง `Upgrade`/`Connection` สำหรับ `/ws`

### 4.7 Zigbee2MQTT (สวิตช์ Zigbee ผ่าน coordinator ในอาคาร)

ดูรายละเอียดทั้งหมดใน `docs/platform/zigbee2mqtt.md` ต้องใช้ **Zigbee2MQTT 2.x ขึ้นไป** และตอนอัปเกรดเครื่องที่ติดตั้งไว้แล้วต้องทำเพิ่มหนึ่งขั้น: broker ต้องรับ packet ขนาด 1 MiB (เดิม 256 KiB) ไม่อย่างนั้น `bridge/devices` ของเครือข่ายที่ใหญ่หน่อยจะถูก broker ตัดทิ้งและ Zigbee2MQTT จะหลุด

```sh
python3 infra/prod/setup.py <flag ชุดเดิมที่ใช้ติดตั้ง>
docker compose --env-file .env.prod -f infra/prod/compose.yaml restart mqtt
```

## 5. วันแรก: เปิดใน shadow mode แล้วค่อยปลด

`setup.py` ตั้ง `ALERTS_SHADOW=true` ให้ตั้งแต่ต้น หมายความว่า: **บันทึก event ทุกอย่างลงฐานข้อมูลตามปกติ แต่ไม่เปิด alert ไม่ส่ง LINE/webhook/อีเมล และไม่รัน automation**

**ข้อยกเว้น: ปุ่มฉุกเฉิน SOS (event `button`) เปิด alert และส่งตามช่องทางของกฎเสมอ แม้อยู่ใน shadow mode** (ตั้งแต่ 2026-09-23) เพราะ shadow มีไว้กันเกณฑ์ที่ยังไม่ได้จูนปลุกคนตอนดึก แต่คนกดปุ่มขอความช่วยเหลือไม่ใช่เรื่องที่ต้องจูน

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

# 6.1 สำรองก่อนเสมอ — นี่คือทางกลับทางเดียว
sudo sh infra/prod/backup-now.sh
sudo ls -lt backups/daily | head -3          # จดชื่อไฟล์ล่าสุดไว้

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

### การย้อนกลับ — พูดตรง ๆ

**ไม่มีระบบ rollback อัตโนมัติ** migration ทุกไฟล์มีส่วน `-- +goose Down` แต่ `cmd/migrate` เรียกเฉพาะ `goose.Up` เท่านั้น ไม่มีคำสั่งสำหรับถอยลง

ถ้าอัปเกรดแล้วพัง ทางกลับคือ **กู้คืน dump ที่ทำไว้ก่อนอัปเกรด** แล้วกลับไปใช้โค้ดเวอร์ชันเดิม:

```sh
sudo git checkout <commit เดิม>
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml build
sudo sh infra/prod/restore.sh aether-YYYYmmdd-HHMMSS.dump --database aether --force
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml up -d
```

ข้อมูลที่เข้ามาหลังจากทำ dump จะหายไป — นี่คือเหตุผลที่ต้อง backup ทันทีก่อนอัปเกรด ไม่ใช่ "เมื่อคืน"

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

- **ไม่มี PITR / WAL archiving** กู้ได้แค่ย้อนไปที่จุดที่ทำ dump เท่านั้น ข้อมูลระหว่าง dump ล่าสุดกับตอนเครื่องพังจะหายถาวร ถ้ารับไม่ได้ ต้องเพิ่ม WAL archiving (pgBackRest / WAL-G) ซึ่งยังไม่ได้ทำในชุดนี้
- **dump ไม่มี secret** ต้องสำรองแยกและเก็บ **offline**:
  - `.env.prod` (มี `JWT_SIGNING_KEY`, `CHANNEL_SEAL_KEY`, รหัสฐานข้อมูล)
  - `<secrets-dir>` ทั้งโฟลเดอร์ (MQTT CA key, ใบรับรอง broker, ไฟล์ตั้งค่า mosquitto)
  - Docker volume `caddy-data` (root CA ของ Caddy ในโหมด internal — ถ้าหายต้องไปติดตั้ง root ใหม่ที่เครื่องลูกข่ายทุกเครื่อง)
- **ไฟล์ backup ไม่ถูกคัดลอกออกนอกเครื่องให้อัตโนมัติ** ถ้าดิสก์หรือเครื่องพัง backup ก็พังไปด้วย ต้องตั้ง rsync/NAS/เทป ไปที่อื่นเอง

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

# กู้ทับของจริง (สคริปต์จะหยุด api / mqtt-ingest / mqtt-provisioner ให้ แล้วเปิดคืนอัตโนมัติ)
sudo sh infra/prod/restore.sh aether-20260920-023001.dump --database aether --force
```

`restore.sh` จะ **ปฏิเสธ** ถ้า database ปลายทางมีตารางอยู่แล้วและไม่ได้ใส่ `--force` (ออกด้วย exit code 2) และจะสร้าง role `aether_owner` / `aether_app` / `aether_mqtt_provisioner` ให้ก่อนเสมอ เพราะ dump มี ownership และ grant ติดมาด้วย

### รายการซ้อมกู้คืน — ทำทุก 3 เดือน

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
| ข้อมูลจริง | Docker volume `aether-prod_postgres-data` |
| ไฟล์สำรอง | `<backup-dir>` |

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
| ไฟล์ backup ล่าสุด | เก่ากว่า 36 ชั่วโมง |
| ขนาด `core.sensor_samples` | โตเร็วกว่าที่ประมาณไว้ในข้อ 1 เกิน 30% → ลด interval หรือ retention |
| HTTP 429 ใน access log | ถ้าเยอะจากผู้ใช้จริง ให้เพิ่ม `API_RATE_LIMIT` |
| จำนวน gateway ที่ออนไลน์ | ลดลงโดยไม่มีเหตุผล = ปัญหาเครือข่าย หรือใบรับรองหมดอายุ |
| เวลาเซิร์ฟเวอร์ | `timedatectl` ต้อง synchronized ตลอด |

```sh
# ขนาดตารางที่โตเร็วที่สุด
sudo docker compose --env-file .env.prod -f infra/prod/compose.yaml exec -u postgres postgres \
  psql -U postgres -d aether -c "SELECT relname, pg_size_pretty(pg_total_relation_size(c.oid)) size
    FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
    WHERE n.nspname IN ('core','identity') AND c.relkind='r'
    ORDER BY pg_total_relation_size(c.oid) DESC LIMIT 10;"
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
| ดิสก์เต็ม | sample สะสม + backup | ลด `SAMPLE_RETENTION_DAYS` หรือ `BACKUP_KEEP_DAILY`; `docker system prune -f` |
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
2. **ไม่มี PITR** กู้ได้แค่จุดที่ทำ dump (ดูข้อ 7)
3. **Rate limit เป็นแบบ per-process ในหน่วยความจำ** ถ้าเพิ่ม replica ของ api ในอนาคต โควตาจะคูณตามจำนวน replica ต้องเปลี่ยนไปใช้ rate limiter ที่แชร์กัน
4. **`core.sensor_samples` ลบข้อมูลเก่าด้วย batched DELETE ไม่ใช่ partition** ที่ปริมาณสูงมาก ๆ การลบจะกินทรัพยากรและทำให้ตาราง bloat (ตั้ง autovacuum ไว้ก้าวร้าวแล้ว แต่ไม่ใช่ยาครอบจักรวาล) ถ้าเกินหลักพัน tag ควรย้ายไป partition รายเดือนหรือ TimescaleDB
5. **หน้าเว็บรันบน `vinext` 1.0.0-beta.5** ซึ่งเป็น beta ยังไม่ใช่ runtime ที่มี track record ยาว ๆ ให้ยึดตามเวอร์ชันที่ pin ไว้ใน `package.json` อย่าอัปเองตามใจ
6. **Frame ของ Minew ยังไม่ได้ยืนยันครบ** packet ถูกเก็บดิบ (`decoded:false`) การเห็นค่าบน dashboard ขึ้นกับ decoder ใน Studio ซึ่งยังไม่ผ่านการตรวจกับฮาร์ดแวร์ทุกรุ่น
7. **collector ต้องมี binding อย่างน้อย 1 รายการ** ตามรูปแบบไฟล์ปัจจุบัน `setup.py` จึงใส่ binding ไปที่ topic สงวน `/aether/reserved/<uuid>` ที่ไม่มีบัญชีใด publish ได้ gateway จริงทั้งหมดวิ่งผ่าน subscription `/aether/gateways/+/status` ซึ่งเป็นคนละเส้นทาง · หมายเหตุ 2026-09-20: backend รับรายการ binding ว่างได้แล้ว แต่ชุดติดตั้งยังคง binding สงวนนี้ไว้ เพราะเป็นรูปแบบที่ผ่านการทดสอบติดตั้งจริงทั้งชุด
8. **ไม่มีการทดสอบเจาะระบบจากภายนอก และยังไม่ได้ load test ที่ 5,000 event/วินาที** อย่าอ้างว่าผ่านมาตรฐานใด ๆ จากชุดนี้
9. **Mosquitto บันทึกลงดิสก์ทุก 30 วินาที** ถ้าไฟดับกะทันหัน ข้อความที่ค้างในคิวอาจหาย — ไม่ใช่การรับประกันแบบ zero loss
10. **ระบบแจ้งเตือนขึ้นกับเครือข่ายขาออก** ถ้า LAN ของโรงพยาบาลไม่ให้ออกอินเทอร์เน็ต LINE/อีเมลจะส่งไม่ได้ ต้องตั้ง `WEBHOOK_ALLOWED_HOSTS` ให้ชี้ไป relay ภายใน และตั้ง `SMTP_*` ให้ใช้ mail server ภายใน
