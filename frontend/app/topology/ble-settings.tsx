"use client";
// How an imported Tuya device is reached: an owner/admin sets a device to Bluetooth here, never the server from a
// sighting alone (docs/platform/tuya-ble.md), and chooses how the Edge reaches it (mode) and how often it reads it.
import { useState } from "react";
import { Bluetooth } from "lucide-react";
import { ApiError, type TuyaBLEMode, type TuyaDevice } from "./api";

export type TuyaBLEClient = {
  setTuyaBLE: (gatewayId: string, tuyaId: string, input: { transport?: "ble" | "wifi" | "auto"; mode?: TuyaBLEMode; poll_seconds?: number }) => Promise<TuyaDevice>;
};

export const BLE_MODE_LABEL: Record<TuyaBLEMode, string> = {
  auto: "อ่านเมื่อได้ยินสัญญาณ (แนะนำสำหรับเซนเซอร์)",
  on_demand: "เชื่อมเฉพาะตอนสั่งงานและตอนอ่านตามรอบ",
  persistent: "ค้างการเชื่อมต่อไว้ (อุปกรณ์เสียบไฟเท่านั้น · แอปในมือถือจะเชื่อมไม่ได้)",
};

/** Read intervals offered (seconds): battery devices pay for every connection, so never under 5 minutes. */
const POLLS: { value: number; label: string }[] = [
  { value: 300, label: "ทุก 5 นาที" },
  { value: 900, label: "ทุก 15 นาที" },
  { value: 1800, label: "ทุก 30 นาที" },
  { value: 3600, label: "ทุกชั่วโมง" },
  { value: 21600, label: "ทุก 6 ชั่วโมง" },
  { value: 86400, label: "วันละครั้ง" },
];

const ERROR_LABEL: Record<string, string> = {
  edge_ble_off: "เซิร์ฟเวอร์นี้ยังไม่เปิด Tuya Bluetooth (EDGE_BLE)",
  ble_address_unknown: "ไม่มีที่อยู่ Bluetooth หรือ uuid ของอุปกรณ์นี้ · นำเข้าจาก Tuya อีกครั้ง หรืออุปกรณ์อยู่หลัง hub",
  registered: "อุปกรณ์ลงทะเบียนอยู่ · ถอดการลงทะเบียนก่อน แล้วค่อยเปลี่ยนทางเชื่อมต่อ",
  ble_poll_seconds: "รอบการอ่านต้องอยู่ระหว่าง 5 นาทีถึง 1 วัน",
  ble_mode: "โหมดไม่ถูกต้อง",
};

export default function TuyaBLESettings({ gatewayId, device, client, onChanged, onNotice }: {
  gatewayId: string;
  device: TuyaDevice;
  client: TuyaBLEClient;
  onChanged: (device: TuyaDevice) => void;
  onNotice: (m: string) => void;
}) {
  const ble = device.transport === "ble";
  const [open, setOpen] = useState(false);
  const [mode, setMode] = useState<TuyaBLEMode>(device.ble_mode || "auto");
  const [poll, setPoll] = useState(device.ble_poll_seconds || 900);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // A device behind a hub, or one Tuya gave no address or uuid for, cannot be found over the air.
  if (!ble && !device.ble_capable) return null;

  const save = (input: { transport?: "ble" | "wifi"; mode?: TuyaBLEMode; poll_seconds?: number }, done: string) => {
    setBusy(true);
    setError("");
    client
      .setTuyaBLE(gatewayId, device.tuya_id, input)
      .then((next) => {
        setOpen(false);
        onChanged(next);
        onNotice(done);
      })
      .catch((e: unknown) => setError(e instanceof ApiError && e.reason && ERROR_LABEL[e.reason] ? ERROR_LABEL[e.reason] : e instanceof Error ? e.message : "บันทึกไม่สำเร็จ"))
      .finally(() => setBusy(false));
  };

  if (!open)
    return (
      <span className="topo-ble-actions">
        {ble ? (
          <button type="button" className="topo-btn" onClick={() => setOpen(true)}>
            <Bluetooth size={14} /> ตั้งค่า Bluetooth
          </button>
        ) : (
          <button type="button" className="topo-btn" onClick={() => setOpen(true)}>
            <Bluetooth size={14} /> ใช้ทาง Bluetooth
          </button>
        )}
        {error && <small className="topo-warn">{error}</small>}
      </span>
    );

  return (
    <form
      className="topo-ble-form"
      aria-label={`Bluetooth ของ ${device.name || device.tuya_id}`}
      onSubmit={(e) => {
        e.preventDefault();
        if (busy) return;
        save(ble ? { mode, poll_seconds: poll } : { transport: "ble", mode, poll_seconds: poll }, ble ? "บันทึกการตั้งค่า Bluetooth แล้ว" : `ตั้ง ${device.name || device.tuya_id} เป็น Bluetooth แล้ว · ลงทะเบียนได้`);
      }}
    >
      {!ble && (
        <p className="topo-note">
          {device.ble_seen ? `Aether Edge ได้ยินอุปกรณ์นี้ทาง Bluetooth${device.rssi != null ? ` (${device.rssi} dBm)` : ""}` : "Aether Edge ยังไม่ได้ยินอุปกรณ์นี้ทาง Bluetooth"} · ยืนยันว่าเป็นอุปกรณ์ Bluetooth (ไม่ใช่ Wi‑Fi และไม่ใช่ Bluetooth Mesh) แล้ว Aether Edge จะเชื่อมเองโดยไม่ใช้ Tuya cloud
          {device.readonly ? " · กลอนประตูใช้ได้แบบอ่านค่าอย่างเดียว ไม่รับคำสั่ง" : ""}
        </p>
      )}
      <label>
        การเชื่อมต่อ
        <select value={mode} onChange={(e) => setMode(e.target.value as TuyaBLEMode)}>
          {(Object.keys(BLE_MODE_LABEL) as TuyaBLEMode[]).map((m) => (
            <option key={m} value={m}>
              {BLE_MODE_LABEL[m]}
            </option>
          ))}
        </select>
      </label>
      <label>
        อ่านค่า
        <select value={poll} onChange={(e) => setPoll(Number(e.target.value))}>
          {POLLS.map((p) => (
            <option key={p.value} value={p.value}>
              {p.label}
            </option>
          ))}
          {!POLLS.some((p) => p.value === poll) && <option value={poll}>ทุก {Math.round(poll / 60)} นาที</option>}
        </select>
      </label>
      {error && (
        <p className="topo-warn" role="alert">
          {error}
        </p>
      )}
      <div className="topo-actions">
        <button type="button" className="topo-btn" disabled={busy} onClick={() => (setOpen(false), setError(""))}>
          ยกเลิก
        </button>
        {ble && (
          <button type="button" className="topo-btn" disabled={busy || device.registered} title={device.registered ? ERROR_LABEL.registered : undefined} onClick={() => save({ transport: "wifi" }, "กลับไปใช้ Wi‑Fi แล้ว")}>
            กลับไปใช้ Wi‑Fi
          </button>
        )}
        <button type="submit" className="topo-btn primary" disabled={busy}>
          {busy ? "กำลังบันทึก…" : ble ? "บันทึก" : "ยืนยันใช้ Bluetooth"}
        </button>
      </div>
    </form>
  );
}
