# เชื่อม Minew IoT Starter Kit MHS กับ Aether

## สิ่งที่ยืนยันได้

ยืนยันเครื่องจริงเมื่อ 2026-09-11: ผู้ใช้ติดตั้ง Gateway Configurer บน iOS แล้ว และแจ้ง IP `192.168.1.77` ภาพแสดงชื่อ `mg3` และ firmware `v1.4.4` การอ่าน `GET http://192.168.1.77/hello` จากคอมพิวเตอร์สำเร็จ โดยอุปกรณ์ตอบ model `mg3-a-lychee`, version `v1.4.4`, productID `001E` และ MAC ตรงกับภาพ ยังไม่ทราบปลายทาง HTTP/MQTT ปัจจุบัน และยังไม่ได้เปลี่ยน configuration ของ gateway

Minew ระบุชุด MHS ประกอบด้วย MG3, S1 และอุปกรณ์ BLE อื่น เช่น card/button/asset tags รุ่นที่ได้รับจริงอาจต่างกัน ให้ตรวจรายการจากกล่องก่อนเปิด profile ของแต่ละตัว

แหล่งข้อมูลผู้ผลิต (ตรวจ 2026-09-11):
- [MHS kit](https://www.minew.com/product/hospital-and-healthcare/)
- [MG3 datasheet: JSON uplinks, MQTT/HTTP และ TLS](https://www.minew.com/wp-content/uploads/2023/12/MG3-USB-Mini-Gateway-3.pdf)
- [Gateway configuration API: MG3/MG4 firmware family, hello และ HTTP/MQTT parameters](https://docs.minew.com/iOS/esp32c3_wifi_gw_http_api_user.html)

ข้อมูลในข้ออ้างอิงเป็นของรุ่นที่ผู้ผลิตระบุ ไม่ใช่หลักฐานว่าตรวจเครื่องของผู้ใช้แล้ว

## เริ่มจากรับ packet จริงหนึ่งชุด

1. ใช้ Gateway Configurer บนโทรศัพท์ในเครือข่ายเดียวกัน อ่าน model/firmware และ IP ของ gateway โดยไม่เปลี่ยนปลายทางเดิมก่อน
2. ถ้า firmware ตรงกับเอกสาร API ผู้ผลิต สามารถอ่าน `http://GATEWAY_IP/hello` เพื่อดู model/version ได้ (แทน IP ด้วย IP ของอุปกรณ์ตัวเอง) ไม่สแกนเครือข่ายหรือเดา IP
3. สร้าง gateway ผ่าน `POST /api/v1/gateways` ด้วย `{ "name": "Minew MHS gateway", "model": "minew-mg3" }` เก็บ token ที่แสดงครั้งเดียวในที่ปลอดภัย
4. เตรียม HTTPS URL ที่ gateway เข้าถึงได้ พร้อม certificate/CA ที่ gateway รองรับ เลือก local HTTPS ingress สำหรับ On-premise หรือโดเมน backend สำหรับ Cloud
5. ตั้ง HTTP service เฉพาะเมื่อ firmware รองรับ authentication ตามเอกสาร: URL `https://YOUR_AETHER_HOST/ingest/gateways/GATEWAY_ID/packets`, HTTP Basic username = GATEWAY_ID, password = token
6. เปิด S1 และให้ gateway scan BLE แล้วตรวจ `GET /api/v1/gateways/GATEWAY_ID/packets` ด้วยบัญชี owner/admin
7. ใช้ sample ที่ลบข้อมูลระบุตัวบุคคล/อุปกรณ์ที่ไม่เกี่ยวข้องแล้ว ทำ golden test สำหรับ envelope และ S1 decoder ก่อนส่งค่า temperature/humidity เป็น canonical event

Compose development bind ที่ loopback เพื่อไม่เปิด credential แบบ cleartext บน LAN ดังนั้น hardware ยังส่งตรงเข้าคอมพิวเตอร์ไม่ได้จนกว่าจะตั้ง local HTTPS ingress อย่าใช้ localhost เป็นปลายทางบน gateway เพราะหมายถึงตัว gateway เอง และอย่าเปลี่ยน bind เป็น 0.0.0.0 แล้วใช้ HTTP ส่ง password บนเครือข่าย

อย่าส่ง password, AppKey, token หรือ Wi-Fi credential ในแชต การยืนยัน model/version ไม่ต้องใช้ความลับเหล่านี้

## สถานะ integration

มี secure HTTP raw-capture endpoint และ gateway credential lifecycle แล้ว ยังไม่มี verified MG3 JSON decoder หรือ S1 BLE decoder การตอบ `stored:true, decoded:false` หมายถึงเก็บ packet ดิบสำเร็จเท่านั้น

`minew-s1-pending@1` เป็น placeholder profile สำหรับลงทะเบียนสินทรัพย์ ไม่อนุญาต generic telemetry endpoint เขียนค่าแทน S1 และไม่อ้างว่าอ่าน sensor จริงแล้ว `generic-environment@1` ใช้ทดสอบเส้นทาง telemetry ด้วยข้อมูล synthetic ที่ระบุชัด

มี MQTT/TLS broker (Mosquitto) และ adapter พร้อม topic ACL แล้ว ดู [MQTT installation](mqtt.md) ทดสอบ synthetic packet เข้า backend ผ่าน แต่ MG3 ยังรอตั้งค่าผ่าน iOS เนื่องจาก SetConfig ตอบรับแล้วค่าอ่านกลับยังเป็น TagCloud

RSSI จาก gateway เดียวบอกได้เพียงพบอุปกรณ์ในบริเวณ ไม่ใช่หลักฐานของพิกัดแม่นยำหรือ room-level positioning; ยังไม่เปิด geofence หรือระบบแจ้งเหตุฉุกเฉินสำหรับใช้งานจริง
