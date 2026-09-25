package domain

import "strings"

// The device catalog is the single source of truth for the gateway models and device profiles this
// deployment accepts. The API exposes it read-only so the UI never carries its own copy.
// Verified means a packet from the physical device was captured and decoded by a golden test in
// this repository; everything else is implemented from public frame documentation and simulation.

type GatewayModel struct {
	ID          string `json:"id"`
	Brand       string `json:"brand"`
	Model       string `json:"model"`
	Label       string `json:"label"`
	Transport   string `json:"transport"` // mqtt | http
	Description string `json:"description"`
	Logo        string `json:"logo,omitempty"`
	Image       string `json:"image,omitempty"` // product photo (manufacturer's image, used for identification only)
	Verified    bool   `json:"verified"`
}

type DeviceProfile struct {
	ID          string   `json:"id"`
	Brand       string   `json:"brand"`
	Model       string   `json:"model"`
	Label       string   `json:"label"`
	Radio       string   `json:"radio"` // ble | zigbee | tuya-wifi | any
	Description string   `json:"description"`
	Image       string   `json:"image,omitempty"`
	Kinds       []string `json:"kinds"`   // reading kinds the device is expected to produce
	Metrics     []string `json:"metrics"` // human-readable list for the UI
	Verified    bool     `json:"verified"`
	// Wearable marks tags carried by people; the UI proposes roaming (follow across gateways) when adopting one.
	Wearable bool `json:"wearable,omitempty"`
	// Button marks tags whose Eddystone-UID instance change means "button pressed" (B10). Only these raise button events.
	Button bool `json:"button,omitempty"`
	// InfoName is the device name the tag reports in its FFE1 info frame, used to suggest this profile.
	InfoName string `json:"info_name,omitempty"`
	// InfoAliases are other names real units report for this model. Measured on the physical kit on
	// 2026-09-21: the S1 temperature/humidity sensor says "PLUS" and the E8S says "E8".
	InfoAliases []string `json:"info_aliases,omitempty"`
	// Occupancy marks PIR sensors whose `motion` metric drives occupied / vacant (see alerts.OccupancyHoldSec).
	Occupancy bool `json:"occupancy,omitempty"`
	// Door marks door/window contact sensors whose state is the `door` metric (1 = open, 0 = closed).
	Door bool `json:"door,omitempty"`
	// Gangs is the number of switch outputs the largest variant of this profile has (1..4); 0 for sensors.
	Gangs int `json:"gangs,omitempty"`
	// Actuator marks devices Aether may switch (POST /api/v1/commands, docs/platform/zigbee2mqtt.md).
	Actuator bool `json:"actuator,omitempty"`
	// Z2MModels are the Zigbee2MQTT `definition.model` values this profile covers.
	Z2MModels []string `json:"z2m_models,omitempty"`
	Notes     string   `json:"notes,omitempty"`
}

// Z2MGatewayModel is the gateway model whose MQTT account publishes a Zigbee2MQTT topic tree.
const Z2MGatewayModel = "zigbee2mqtt"

// Z2MGenericProfile is the profile of a Zigbee2MQTT device no more specific profile matches.
const Z2MGenericProfile = "zigbee2mqtt-device@1"

// EdgeGatewayModel is Aether Edge: an agent on a site host that talks to Tuya Wi-Fi devices on the LAN with
// their local keys (no Tuya cloud at runtime) and publishes under aether/edge/<gateway id>.
const EdgeGatewayModel = "aether-edge"

// TuyaWiFiProfile is the profile of any Tuya Wi-Fi device reached locally through Aether Edge. What it can do
// comes from its Tuya data-point specification (imported once), translated into the same exposes shape
// Zigbee2MQTT devices use.
const TuyaWiFiProfile = "tuya-wifi-device@1"

// ButtonProfileIDs lists the profiles whose tags may raise a button event.
func ButtonProfileIDs() []string {
	out := []string{}
	for _, p := range DeviceProfiles {
		if p.Button {
			out = append(out, p.ID)
		}
	}
	return out
}

var GatewayModels = []GatewayModel{
	{ID: "minew-mg3", Brand: "Minew", Model: "MG3", Label: "Minew MG3", Transport: "mqtt", Description: "USB Mini BLE → Wi‑Fi gateway · ส่ง BLE advertisement เข้า Aether ผ่าน MQTT (JSON-LONG)", Logo: "/brands/minew.png", Image: "/devices/minew-mg3.png", Verified: true},
	// MG4: JSON-Long is its only upload format and reelyActive's barnowl-minew parses it exactly like MG3, except
	// that row timestamps may be ISO-8601 strings and rows may carry extra fields. Aether ignores device
	// timestamps (server time only) and parses every row on its own, so both are harmless. Not yet verified with
	// a captured MG4 uplink.
	{ID: "minew-mg4", Brand: "Minew", Model: "MG4", Label: "Minew MG4 · Rechargeable gateway", Transport: "mqtt", Description: "Rechargeable BLE → Wi‑Fi gateway (ชุด MOS smart office) · ส่ง BLE advertisement เข้า Aether ผ่าน MQTT แบบ JSON-Long · อัปโหลดทาง HTTP ก็ได้โดยใช้เส้นทาง Generic HTTP (POST /ingest/gateways/<id>/packets + HTTP Basic) · ยังไม่ยืนยันกับเครื่องจริง", Logo: "/brands/minew.png", Image: "/devices/minew-mg4.png", Verified: false},
	// Zigbee2MQTT on a local host next to a Zigbee coordinator (e.g. SMLIGHT SLZB-06M). It connects out to this
	// broker over TLS with a per-gateway account and publishes under aether/z2m/<gateway id>. Not yet verified
	// with a captured bridge/devices payload from real hardware.
	{ID: Z2MGatewayModel, Brand: "Zigbee2MQTT", Model: "Zigbee coordinator", Label: "Zigbee2MQTT", Transport: "mqtt", Description: "Zigbee coordinator (เช่น SLZB-06M) + Zigbee2MQTT บนเครื่องในอาคาร · ส่งสถานะอุปกรณ์ Zigbee เข้า Aether ผ่าน MQTT over TLS · ไม่ใช้ Tuya cloud · ยังไม่ยืนยันกับเครื่องจริง", Verified: false},
	// Aether Edge on a site host (Raspberry Pi) next to the Tuya Wi-Fi devices: connects to each over TCP 6668 with its
	// local key and forwards status and commands over MQTT/TLS. Not yet verified with real devices.
	{ID: EdgeGatewayModel, Brand: "Aether", Model: "Edge", Label: "Aether Edge (Tuya Wi‑Fi)", Transport: "mqtt", Description: "โปรแกรม Aether Edge บนเครื่องในอาคาร (เช่น Raspberry Pi) คุยกับอุปกรณ์ Tuya Wi‑Fi ในวง LAN ด้วย local key โดยตรง ไม่ใช้ Tuya cloud ขณะทำงาน · ส่งสถานะและรับคำสั่งผ่าน MQTT over TLS · ยังไม่ยืนยันกับเครื่องจริง", Verified: false},
	{ID: "generic-http", Brand: "Generic", Model: "HTTP gateway", Label: "Generic HTTP", Transport: "http", Image: "/devices/generic-http-gateway.png", Description: "Gateway ทั่วไปที่ POST JSON เข้า Aether ด้วย HTTP Basic (gateway id + token)", Verified: false},
}

var DeviceProfiles = []DeviceProfile{
	{ID: "minew-s1-pending@1", Brand: "Minew", Model: "S1", Label: "Minew S1 · อุณหภูมิ / ความชื้น", Radio: "ble", Image: "/devices/minew-s1.png", Kinds: []string{"environment"}, Metrics: []string{"temperature", "humidity", "battery"}, Verified: true, InfoName: "S1", InfoAliases: []string{"PLUS"},
		Description: "BLE sensor · เฟรม FFE1 A1‑01 ยืนยันกับเครื่องจริงแล้ว · profile ใช้ลงทะเบียนสินทรัพย์ ไม่ยืนยันรุ่นจากเฟรมเพียงอย่างเดียว"},
	{ID: "minew-c10-pending@1", Wearable: true, Image: "/devices/minew-c10.png", Brand: "Minew", Model: "C10", Label: "Minew C10 · Card beacon", Radio: "ble", Kinds: []string{"beacon", "motion"}, Metrics: []string{"iBeacon / Eddystone id", "accelerometer", "battery"}, Verified: false, InfoName: "C10",
		Description: "บัตรพนักงาน/ผู้ป่วยแบบ beacon มีปุ่มซ่อนและ accelerometer · ถอดรหัส iBeacon/Eddystone และเฟรม A1‑03 จากเอกสารสาธารณะ ยังไม่ยืนยันกับเครื่องจริง", Notes: "การกดปุ่มยังไม่มีเอกสารรูปแบบเฟรม"},
	{ID: "minew-b7-pending@1", Wearable: true, Image: "/devices/minew-b7.png", Brand: "Minew", Model: "B7", Label: "Minew B7 · Button wristband", Radio: "ble", Kinds: []string{"beacon", "motion"}, Metrics: []string{"iBeacon / Eddystone id", "accelerometer", "battery"}, Verified: false, InfoName: "B7",
		Description: "สายรัดข้อมือมีปุ่ม · ส่ง iBeacon/Eddystone และ accelerometer · ยังไม่ยืนยันกับเครื่องจริง", Notes: "รูปแบบ event ปุ่มกดของ B7 ยังไม่มีเอกสารยืนยัน"},
	{ID: "minew-b10-pending@1", Wearable: true, Button: true, Image: "/devices/minew-b10.png", Brand: "Minew", Model: "B10", Label: "Minew B10 · Emergency button", Radio: "ble", Kinds: []string{"beacon"}, Metrics: []string{"Eddystone UID instance", "battery (TLM)"}, Verified: false, InfoName: "B10",
		Description: "สายรัดปุ่มฉุกเฉิน · การกดปุ่มปรากฏเป็นการเปลี่ยน Eddystone‑UID instance ตามคู่มือตั้งค่าสาธารณะ · ยังไม่ยืนยันกับเครื่องจริง", Notes: "ระบบแสดงการเปลี่ยน instance เป็น event ไม่อ้างว่าเป็นสัญญาณฉุกเฉินที่รับรองแล้ว"},
	{ID: "minew-e8s-pending@1", Image: "/devices/minew-e8s.png", Brand: "Minew", Model: "E8S", Label: "Minew E8S · Accelerometer asset tag", Radio: "ble", Kinds: []string{"motion"}, Metrics: []string{"accelerometer", "vibration", "battery"}, Verified: false, InfoName: "E8S", InfoAliases: []string{"E8"},
		Description: "ป้ายติดสินทรัพย์ตรวจการเคลื่อนไหว · เฟรม A1‑03 / A1‑18 จากเอกสารสาธารณะ ยังไม่ยืนยันกับเครื่องจริง"},
	{ID: "minew-mbt01-pending@1", Image: "/devices/minew-mbt01.png", Brand: "Minew", Model: "MBT01", Label: "Minew MBT01 · Anti‑tamper tag", Radio: "ble", Kinds: []string{"beacon", "tamper"}, Metrics: []string{"iBeacon id", "tamper", "battery"}, Verified: false, InfoName: "MBT01",
		Description: "ป้ายกันถอด · คาดว่าใช้เฟรม A1‑20 (tamper flag) ยังไม่ยืนยันกับเครื่องจริง"},
	// MOS smart-office kit. The owner's real MSP01 (c30000161ebb) sent A1-01, A1-11 (motion), A1-03 and an A1-08
	// info frame named "MSP01" on 2026-09-21, but motion stayed 0 in all 257 samples, so the PIR itself is untested.
	{ID: "minew-msp01-pending@1", Image: "/devices/minew-msp01.png", Brand: "Minew", Model: "MSP01", Label: "Minew MSP01 · PIR occupancy", Radio: "ble", Kinds: []string{"motion", "environment"}, Metrics: []string{"motion (PIR)", "temperature", "humidity", "battery"}, Verified: false, InfoName: "MSP01", Occupancy: true,
		Description: "เซนเซอร์ PIR ตรวจคนในห้อง · เฟรม A1‑11 (motion) และ A1‑01 ถอดรหัสได้ · occupied เมื่อ motion เป็น 1 และ vacant เมื่อไม่มี motion นาน 5 นาที · ยังไม่ยืนยันกับเครื่องจริง",
		Notes:       "เครื่องจริงรายงาน motion=0 ทั้ง 257 ตัวอย่าง (2026-09-21) · ต้องทดสอบโดยเดินผ่านหน้าเซนเซอร์"},
	// S4: the combination-frame byte layout is not public, so Aether has NO decoder for it. The door state comes
	// from a signal the operator teaches (open vs closed); see docs/platform/mos-kit.md.
	{ID: "minew-s4-pending@1", Image: "/devices/minew-s4.png", Brand: "Minew", Model: "S4", Label: "Minew S4 · Door sensor", Radio: "ble", Kinds: []string{"door"}, Metrics: []string{"door (open / closed)", "battery"}, Verified: false, InfoName: "S4", Door: true,
		Description: "เซนเซอร์ประตูแม่เหล็ก · รูปแบบเฟรมยังไม่มีเอกสารสาธารณะ Aether จึงไม่ถอดรหัสเอง · สถานะเปิด/ปิดมาจากการ \"สอนสัญญาณ\" (เปิด–ปิดประตูให้ระบบเรียนรู้) · ยังไม่ยืนยันกับเครื่องจริง",
		Notes:       "เฟรมที่ไม่รู้จักแสดงใน unknown ของ reading · สอนสัญญาณ door ก่อนใช้งาน"},
	// Tuya no-neutral touch wall switches as Zigbee2MQTT exposes them: TS0011 `state`, TS0012 `state_left/right`,
	// TS0013 `state_left/center/right`, TS0014 `state_l1..l4` (zigbee2mqtt.io/devices/TS001*.html). TS0601 is a
	// catch-all Tuya model id, so it only matches when its exposes actually contain switch outputs.
	{ID: "tuya-ts001x-switch@1", Brand: "Tuya", Model: "TS001x", Label: "Tuya Zigbee wall switch · 1–4 ช่อง", Radio: "zigbee", Kinds: []string{"switch"}, Metrics: []string{"สถานะเปิด/ปิดแต่ละช่อง", "linkquality"}, Verified: false, Gangs: 4, Actuator: true,
		Z2MModels:   []string{"TS0011", "TS0012", "TS0013", "TS0014", "TS0601"},
		Description: "สวิตช์ผนังแบบสัมผัส ไม่ใช้สายกลาง ผ่าน Zigbee2MQTT · แสดงสถานะเปิด/ปิดแต่ละช่องและการกดที่ผนัง · สั่งเปิด/ปิดแต่ละช่องจาก Aether ได้ · ยังไม่ยืนยันกับเครื่องจริง"},
	// Any other device Zigbee2MQTT supports. What it can do is read from its own definition (the exposes the bridge
	// publishes), so a light, curtain, lock or thermostat can be registered and commanded without a catalog entry
	// of its own; its readings are not decoded into Aether metrics yet.
	{ID: Z2MGenericProfile, Brand: "Zigbee2MQTT", Model: "Zigbee device", Label: "อุปกรณ์ Zigbee ทั่วไป (Zigbee2MQTT)", Radio: "zigbee", Kinds: []string{"zigbee"}, Metrics: []string{"ตามความสามารถที่ Zigbee2MQTT ประกาศ (exposes)"}, Verified: false, Actuator: true,
		Description: "อุปกรณ์ใดก็ได้ที่ Zigbee2MQTT รองรับ เช่น หลอดไฟ ม่าน กลอนประตู หัววาล์ว · สั่งงานได้ตามคุณสมบัติที่อุปกรณ์ประกาศว่าตั้งค่าได้ · ยังไม่ยืนยันกับเครื่องจริง"},
	// Any Tuya Wi-Fi device (plug, switch, bulb, curtain motor, ...) that stays on the LAN. Battery sensors sleep and
	// cannot be reached locally; the import marks them.
	{ID: TuyaWiFiProfile, Brand: "Tuya", Model: "Wi‑Fi device", Label: "อุปกรณ์ Tuya Wi‑Fi (local ผ่าน Aether Edge)", Radio: "tuya-wifi", Kinds: []string{"tuya"}, Metrics: []string{"ตามจุดข้อมูล (DP) ที่อุปกรณ์ประกาศ"}, Verified: false, Actuator: true,
		Description: "อุปกรณ์ Tuya Wi‑Fi ที่เสียบไฟตลอด เช่น ปลั๊ก สวิตช์ หลอดไฟ มอเตอร์ม่าน · Aether Edge คุยกับอุปกรณ์ในวง LAN ด้วย local key · สั่งงานได้ตามจุดข้อมูลที่ตั้งค่าได้ · ยังไม่ยืนยันกับเครื่องจริง"},
	{ID: "generic-ble-beacon@1", Image: "/devices/generic-ble-beacon.png", Brand: "Generic", Model: "BLE beacon", Label: "Generic iBeacon / Eddystone", Radio: "ble", Kinds: []string{"beacon"}, Metrics: []string{"UUID / major / minor หรือ namespace / instance"}, Verified: false,
		Description: "beacon มาตรฐานทุกยี่ห้อที่ gateway ได้ยิน · ใช้ระบุตัวตน/ตำแหน่งคร่าว ๆ ไม่มีค่าเซนเซอร์"},
	{ID: "generic-environment@1", Image: "/devices/generic-environment.png", Brand: "Generic", Model: "Environment sensor", Label: "Generic environment sensor", Radio: "any", Kinds: []string{"environment"}, Metrics: []string{"ตาม payload ที่ส่งเข้ามา"}, Verified: false,
		Description: "อุปกรณ์ทั่วไปที่ส่ง telemetry.v1 ผ่าน gateway · ใช้ทดสอบเส้นทางข้อมูลหรืออุปกรณ์ต่างแบรนด์"},
}

func GatewayModelByID(id string) *GatewayModel {
	for i := range GatewayModels {
		if GatewayModels[i].ID == id {
			return &GatewayModels[i]
		}
	}
	return nil
}

func DeviceProfileByID(id string) *DeviceProfile {
	for i := range DeviceProfiles {
		if DeviceProfiles[i].ID == id {
			return &DeviceProfiles[i]
		}
	}
	return nil
}

// MatchesInfo reports whether a name from a tag's FFE1 info frame identifies this profile.
func (p DeviceProfile) MatchesInfo(name string) bool {
	if name == "" || p.InfoName == "" {
		return false
	}
	if strings.EqualFold(name, p.InfoName) {
		return true
	}
	for _, alias := range p.InfoAliases {
		if strings.EqualFold(name, alias) {
			return true
		}
	}
	return false
}

// MatchesZ2M reports whether a Zigbee2MQTT definition model identifies this profile. TS0601 is Tuya's generic
// data-point model id, so it only counts when the device's exposes show switch outputs (hasSwitch).
func (p DeviceProfile) MatchesZ2M(model string, hasSwitch bool) bool {
	for _, m := range p.Z2MModels {
		if strings.EqualFold(m, model) {
			return !strings.EqualFold(m, "TS0601") || hasSwitch
		}
	}
	return false
}

// ProfileAllowedOn reports whether a device of this profile can be registered under a gateway of this model:
// Zigbee profiles only on a Zigbee2MQTT gateway and Tuya Wi-Fi profiles only on an Aether Edge, and each of those
// gateways takes nothing else. A profile id that is no longer in the catalog (a registration made before a catalog
// change) counts as neither, so an existing BLE device can still be moved between BLE gateways; gateways whose
// model left the catalog accept only such ordinary profiles.
func ProfileAllowedOn(profileID, gatewayModel string) bool {
	radio := ""
	if p := DeviceProfileByID(profileID); p != nil {
		radio = p.Radio
	}
	switch gatewayModel {
	case Z2MGatewayModel:
		return radio == "zigbee"
	case EdgeGatewayModel:
		return radio == "tuya-wifi"
	}
	return radio != "zigbee" && radio != "tuya-wifi"
}
