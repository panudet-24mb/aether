# ติดตั้ง Aether Edge บน Raspberry Pi

Aether Edge คือโปรแกรมที่วางไว้ในอาคาร (บน Raspberry Pi หรือ mini PC ที่เปิดตลอด) ทำหน้าที่คุยกับอุปกรณ์ Tuya Wi‑Fi ในวง LAN ด้วย local key แล้วส่งข้อมูลขึ้น Aether เลือกได้ว่าจะให้รัน **Zigbee2MQTT** คู่กันด้วยคำสั่งเดียว สำหรับ SLZB-06M / SLZB-06MU รายละเอียดทางเทคนิคอยู่ที่ [aether-edge.md](aether-edge.md)

## สิ่งที่ต้องมี

- Raspberry Pi 4/5 หรือ mini PC ที่ใช้ Linux 64-bit (arm64 หรือ amd64) **อยู่วง LAN เดียวกับอุปกรณ์ Tuya**
- Docker และ compose plugin:
  ```sh
  curl -fsSL https://get.docker.com | sh
  docker compose version
  ```
- เปิดออกอินเทอร์เน็ตได้ (ต่อออกไปที่ Aether ทาง HTTPS 443 และ MQTT 8883) **ไม่ต้องเปิดพอร์ตขาเข้าที่เราเตอร์**
- ถ้าจะใช้ Zigbee: SLZB-06M/06MU ต่อ LAN แล้ว และรู้ IP ของมัน (ดูได้จากหน้าเว็บของ SLZB)

## ขั้นตอน

1. ในหน้า **เชื่อมต่ออุปกรณ์** ของ Aether ให้เพิ่ม gateway รุ่น **Aether Edge**
   - ถ้าจะใช้ Zigbee ด้วย ให้เพิ่ม gateway **Zigbee2MQTT** ไว้อีกตัว
2. ที่ gateway Aether Edge กด **สร้างคำสั่งติดตั้ง** (ถ้าใช้ Zigbee ให้เลือก gateway Zigbee2MQTT ที่จะจับคู่ด้วย)
   - ระบบแสดง **คำสั่งติดตั้ง** และ **โค้ดติดตั้ง** แยกกัน โค้ดแสดง **ครั้งเดียว** ใช้ได้ภายใน 30 นาที และใช้ได้ครั้งเดียว
3. ที่ Pi วางคำสั่งติดตั้ง เช่น
   ```sh
   curl -fsSL https://aether.example.com/edge/install.sh | sudo sh
   ```
   - ถ้าใช้ Zigbee ด้วย: `... | sudo sh -s -- --zigbee <IP ของ SLZB>` เช่น `--zigbee 192.168.1.40`
   - สคริปต์ดาวน์โหลด image ก่อน แล้วจึง **ถามโค้ดติดตั้ง** ให้วางโค้ดตอนนั้น (ตัวอักษรจะไม่แสดงบนจอ)
   - **โค้ดไม่อยู่ในคำสั่ง** จึงไม่ติดไปใน history ของ shell, log ของ sudo หรือรายชื่อ process
   - ติดตั้งแบบอัตโนมัติ (ไม่มีจอให้พิมพ์) ให้ส่งโค้ดทาง environment `AETHER_INSTALL_CODE` แทน
4. รอสักครู่ แล้วดูที่ gateway ใน Aether ต้องขึ้นว่า Edge ออนไลน์ จากนั้นทำต่อที่ [tuya-local.md](tuya-local.md) (นำเข้า key จาก Tuya แล้วลงทะเบียนอุปกรณ์)

ไฟล์ทั้งหมดอยู่ใน `/opt/aether-edge` (เปลี่ยนได้ด้วย `--dir` ต้องเป็น path เต็ม และห้ามเป็นโฟลเดอร์ระบบ เช่น `/`, `/etc`, `/usr`, `/var`, `/root`, `/home`) ไฟล์ `.env` มีรหัสของ gateway **อย่าแชร์ให้ใคร**

## ใช้ Zigbee2MQTT

- หน้าเว็บของ Zigbee2MQTT ใช้กด **Permit join** ตอนจับคู่อุปกรณ์ใหม่ (เช่นสวิตช์ Tuya) และ **ต้องใส่ token** ทุกครั้ง
  - ตัวติดตั้งสุ่ม token ให้ (128 bit ขึ้นไป) และ **แสดงครั้งเดียวตอนติดตั้งเสร็จ** เก็บไว้ใน `zigbee2mqtt/secret.yaml` (0600) ด้วย ติดตั้งซ้ำ token เดิมยังใช้ได้
  - **ค่าเริ่มต้นเปิดเฉพาะในเครื่อง Pi เอง** (`127.0.0.1:8080`) เข้าจากเครื่องอื่นด้วย SSH tunnel:
    ```sh
    ssh -L 8080:127.0.0.1:8080 pi@<IP ของ Pi>
    ```
    แล้วเปิด `http://127.0.0.1:8080`
  - ถ้าต้องการเปิดให้เครื่องใน LAN เข้าได้ ให้ติดตั้งด้วย `--zigbee-ui <IP ของ Pi ใน LAN>` (จะเปิดเฉพาะ interface นั้น ไม่เปิดทุก interface เพราะพอร์ตที่ Docker publish ข้าม firewall ของเครื่อง)
  - **ข้อควรระวัง:** Zigbee2MQTT 2.x ไม่มีตัวเลือกปิดการอัปโหลด external converter/extension จากหน้าเว็บ ใครเข้าหน้าเว็บได้ (มี token) จะรันโค้ดใน container ของ Zigbee2MQTT ได้ และเห็น network key ของ Zigbee จึงป้องกันด้วย token + เปิดเฉพาะในเครื่องเป็นค่าเริ่มต้น **อย่าแชร์ token และอย่าเปิดพอร์ตนี้ออกอินเทอร์เน็ต**
  - container ของ Zigbee2MQTT รันแบบตัด capability ทั้งหมด (`cap_drop: ALL`, `no-new-privileges`) และไม่ได้ต่อ Docker socket
- ตั้งค่าไว้ให้ต่อ SLZB ด้วย `tcp://<IP>:6638` และ `adapter: ember` แล้ว
- ไฟล์ `zigbee2mqtt/configuration.yaml` ถูกสร้าง **ครั้งแรกครั้งเดียว** เพราะ Zigbee2MQTT เก็บ network key ไว้ในไฟล์นี้ ถ้าไฟล์หาย ต้องจับคู่อุปกรณ์ Zigbee ใหม่ทั้งหมด รหัส MQTT แยกอยู่ใน `zigbee2mqtt/secret.yaml` ซึ่งเขียนใหม่ทุกครั้งที่ติดตั้ง

## ใช้อุปกรณ์ Tuya Bluetooth (ยังไม่ยืนยันกับเครื่องจริง)

ใช้ได้เมื่อเซิร์ฟเวอร์ Aether เปิด `EDGE_BLE=true` และใช้ image ของ Aether Edge รุ่นที่มี Bluetooth แล้วเท่านั้น (รายละเอียดใน [tuya-ble.md](tuya-ble.md))

1. ที่ Pi ติดตั้ง BlueZ แล้วเปิดใช้:
   ```sh
   sudo apt install bluez
   sudo systemctl enable --now bluetooth
   sudo rfkill unblock bluetooth
   ```
2. เพิ่ม `--ble` ในคำสั่งติดตั้ง เช่น `... | sudo sh -s -- --ble` (ใช้คู่กับ `--zigbee` ได้)
   - สคริปต์ตรวจก่อนว่ามี D-Bus (`/run/dbus/system_bus_socket`), bluetoothd ทำงานอยู่ และ Bluetooth ไม่ถูก rfkill บล็อก ถ้าไม่ผ่านจะหยุดก่อนใช้โค้ด
   - container ยังตัดสิทธิ์ทั้งหมด (`cap_drop: ALL`) แค่ mount `/run/dbus` แบบอ่านอย่างเดียว เพื่อคุยกับ bluetoothd
   - สคริปต์เขียน `/etc/dbus-1/system.d/aether-edge-bluetooth.conf` อนุญาตให้ uid 10001 (โปรแกรม Aether Edge) เรียก BlueZ ได้เฉพาะคำสั่งที่ใช้จริง (สแกน, เชื่อมต่อ, เขียน/รับค่า GATT, อ่านสถานะ) และสร้างบัญชีระบบ `aether-edge` ให้ uid นี้ถ้ายังไม่มี
   - **ข้อจำกัด:** ถ้า BlueZ ของเครื่องเปิดให้ผู้ใช้ทุกคนเรียกได้อยู่แล้ว (ค่าเริ่มต้นของ BlueZ) กฎนี้จำกัดอะไรไม่ได้ สคริปต์จะเตือน ทางเลือกคือโปรไฟล์ AppArmor (ดู [tuya-ble.md](tuya-ble.md))
   - ถอนการติดตั้ง: `... | sudo sh -s -- --uninstall` หยุดโปรแกรม ลบไฟล์กฎ D-Bus และบัญชี `aether-edge` (โฟลเดอร์ `/opt/aether-edge` ยังอยู่ ให้ลบเอง)
3. Bluetooth ในตัว Pi ต่อพร้อมกันได้ประมาณ 1 เครื่อง ถ้าใช้ USB dongle (CSR8510 / BCM20702) ตั้ง `BLE_MAX_CONNECTIONS=3` ใน `/opt/aether-edge/.env` แล้ว `docker compose --project-directory /opt/aether-edge up -d`
4. อุปกรณ์ BLE ต้องไม่ถูกจับคู่อยู่กับ Tuya hub และปิดแอป Smart Life ระหว่างใช้ (อุปกรณ์รับการเชื่อมต่อได้ทีละตัว) · อุปกรณ์ Bluetooth mesh ใช้ไม่ได้ ให้ใช้โหมด Tuya Cloud

## อัปเดตเวอร์ชัน

```sh
curl -fsSL https://aether.example.com/edge/install.sh | sudo sh -s -- --update
```
ดึง image เวอร์ชันที่เซิร์ฟเวอร์ Aether กำหนด **ทั้ง Aether Edge และ Zigbee2MQTT** แล้วรีสตาร์ต ไม่ต้องใช้โค้ดติดตั้งใหม่

## ย้ายเครื่อง หรือติดตั้งใหม่

- สร้างคำสั่งติดตั้งใหม่ แล้วรันที่เครื่องใหม่ **รหัสของ gateway จะถูกเปลี่ยนทันที** เครื่องเก่าจะหยุดคุยกับอุปกรณ์เอง (มันจะถูกปฏิเสธตอนดึงค่าตั้ง) แต่ควรปิดเครื่องเก่าด้วย:
  ```sh
  sudo docker compose --project-directory /opt/aether-edge down
  ```
- อุปกรณ์ Tuya ส่วนใหญ่ยอมให้ต่อแบบ local ได้ **ทีละหนึ่งโปรแกรม** ถ้า Home Assistant/LocalTuya ต่ออยู่ Edge จะต่อไม่ได้ (สถานะ `busy`)

## แก้ปัญหา

| อาการ | สาเหตุ / วิธีแก้ |
|---|---|
| `the install code was refused` | โค้ดผิด ใช้ไปแล้ว หรือเกิน 30 นาที · สร้างใหม่ |
| `the server cannot hand out its broker certificate yet` | ฝั่งเซิร์ฟเวอร์ยังไม่ได้ตั้ง CA ของ broker (docs/production.md §4.10) · โค้ดยังไม่ถูกใช้ ลองใหม่ได้หลังแก้ |
| `--zigbee was given, but the install code was created without a Zigbee2MQTT gateway` | สร้างคำสั่งติดตั้งใหม่โดยเลือก gateway Zigbee2MQTT |
| Edge ไม่ขึ้นออนไลน์ | `sudo docker compose --project-directory /opt/aether-edge logs aether-edge` · ตรวจว่า Pi ต่อออก 8883 ได้ |
| อุปกรณ์ขึ้น `auth_failed` | local key ไม่ตรง (เพิ่งลบ/เพิ่มอุปกรณ์ในแอปใหม่) · นำเข้าจาก Tuya อีกครั้ง |
| อุปกรณ์ขึ้น `busy` | มีโปรแกรมอื่นต่ออุปกรณ์อยู่ (Home Assistant/LocalTuya) · ปิดโปรแกรมนั้น |
| อุปกรณ์ขึ้น `not_found` / `unreachable` | อุปกรณ์ไม่อยู่วง LAN เดียวกับ Pi หรือปิดอยู่ |
| ดู log ทั้งหมด | `sudo docker compose --project-directory /opt/aether-edge logs -f` |
