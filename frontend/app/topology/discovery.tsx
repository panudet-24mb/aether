"use client";
import { useState } from "react";
import { Bluetooth, Cloud, Search, Wifi } from "lucide-react";
import type { Discovery, Gateway } from "./api";
import { formatMAC, suggestProfile } from "./catalog";
import { isFresh } from "./model";
import { BLE_WARNING, LAN_WARNING } from "./tuya-import";
import { ZIGBEE_KIND_LABEL } from "./zigbee-catalog";

export default function DiscoveryList({ items, gateways, gatewayId, serverTime, busy, onAdopt, hiddenUnknown = 0, hiddenBLE = 0 }: {
  items: Discovery[]; gateways: Gateway[]; gatewayId?: string; serverTime: number; busy: boolean;
  /** Advertisements the server left out because they are not a supported model. */
  hiddenUnknown?: number;
  /** Tuya devices set to Bluetooth the server left out because Tuya BLE (EDGE_BLE) is off there. */
  hiddenBLE?: number;
  onAdopt: (external: string, gatewayId: string) => void;
}) {
  const [filter, setFilter] = useState("");
  const [query, setQuery] = useState("");
  const selected = gatewayId ?? filter;
  const q = query.toLowerCase().replace(/[:\s-]/g, "");
  const matches = items.filter((d) => (!selected || d.gateway_id === selected) &&
    (!q || `${d.external_id}${d.model ?? ""}${d.profile?.label ?? ""}${d.vendor ?? ""}${d.description ?? ""}`.toLowerCase().replace(/[:\s-]/g, "").includes(q)));
  return <section className="topo-discovery" aria-label="อุปกรณ์ที่พบใหม่">
    <p className="topo-note">{matches.length > 0 && matches.every((d) => d.source === "tuya_cloud") ? "อุปกรณ์ในโปรเจกต์ Tuya ที่ซิงก์ล่าสุดและยังไม่ลงทะเบียน · ลงทะเบียนเป็น \"อุปกรณ์ Tuya (ผ่าน Tuya Cloud)\"" : matches.length > 0 && matches.every((d) => d.source === "tuya" || d.source === "tuya_ble" || d.source === "tuya_lan") ? "อุปกรณ์ Tuya ที่นำเข้าคีย์แล้วแต่ยังไม่ลงทะเบียน (ทาง Wi‑Fi หรือ Bluetooth) และอุปกรณ์ที่ Aether Edge พบใน LAN แต่ยังไม่มีคีย์" : `อุปกรณ์ที่ gateway ได้ยินใน 15 นาทีล่าสุดและยังไม่ลงทะเบียน · แสดงเฉพาะอุปกรณ์ Minew${hiddenUnknown > 0 ? ` · ไม่แสดงสัญญาณอื่น ${hiddenUnknown} รายการ (มือถือ, beacon ของคนอื่น)` : ""}`}</p>
    {hiddenBLE > 0 && <p className="topo-note">ไม่แสดงอุปกรณ์ Tuya Bluetooth {hiddenBLE} รายการ · เซิร์ฟเวอร์นี้ยังไม่เปิด Tuya Bluetooth (EDGE_BLE)</p>}
    {!gatewayId && <label className="topo-project-select">Gateway
      <select aria-label="กรอง gateway ที่ค้นพบ" value={filter} onChange={(e) => setFilter(e.target.value)}>
        <option value="">ทุก gateway</option>
        {gateways.map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
      </select>
    </label>}
    <label className="topo-search"><Search size={14} /><input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="ค้นหา MAC หรือรุ่น" aria-label="ค้นหาอุปกรณ์ใหม่" /></label>
    {matches.length === 0 && <p className="topo-empty">{query ? "ไม่พบอุปกรณ์ตรงกับคำค้น" : "ยังไม่มีอุปกรณ์ใหม่ที่รอลงทะเบียน · เมื่อ gateway ส่งข้อมูลมา รายการจะปรากฏอัตโนมัติ"}</p>}
    {gateways.filter((g) => !selected || selected === g.id).map((g) => {
      const devices = matches.filter((d) => d.gateway_id === g.id);
      if (!devices.length) return null;
      return <div key={g.id} className="topo-discovery-group">
        <h3 className="topo-h3">{g.name} <span className="topo-count">{devices.length}</span></h3>
        {devices.map((d) => {
          if (d.source === "tuya_cloud") return <TuyaCloudCard key={d.external_id} d={d} busy={busy} onAdopt={onAdopt} />;
          if (d.source === "tuya" || d.source === "tuya_ble" || d.source === "tuya_lan") return <TuyaCard key={d.external_id} d={d} busy={busy} onAdopt={onAdopt} />;
          const suggestion = !d.profile ? suggestProfile({ model: d.model, kind: d.kind, zigbee: d.source === "z2m" }) : undefined;
          const profile = d.profile ?? suggestion;
          // Zigbee devices come from the coordinator's own list (source "z2m"); their model is the Z2M definition.
          const zigbee = d.source === "z2m";
          const label = zigbee && d.vendor && d.model ? `${d.vendor} ${d.model}` : d.profile ? `${d.profile.brand} ${d.profile.model}${zigbee && d.model ? ` · ${d.model}` : ""}` : d.model ? `${zigbee ? "Zigbee" : "Minew"} ${d.model}` : profile ? `${profile.brand} ${profile.model}` : zigbee ? "อุปกรณ์ Zigbee · ยังไม่ทราบรุ่น" : "อุปกรณ์ BLE · ยังไม่ทราบรุ่น";
          return <article className="topo-discovery-card" key={d.external_id}>
            <div className="topo-discovery-photo">{profile?.image ? <img src={profile.image} alt={label} /> : <Bluetooth size={30} />}</div>
            <div className="topo-discovery-info">
              <strong>{label}</strong>
              <small>{zigbee && d.description ? `${d.description}${d.kind ? ` · ${ZIGBEE_KIND_LABEL[d.kind] ?? d.kind}` : ""}` : d.profile ? "รุ่นที่อุปกรณ์รายงาน" : suggestion ? "รุ่นแนะนำ · กรุณาตรวจสอบกับตัวอุปกรณ์" : "เลือกยี่ห้อและรุ่นได้ตอนลงทะเบียน"}</small>
              <code>{formatMAC(d.external_id)}</code>
              <small>{zigbee ? "pair อยู่กับ coordinator" : isFresh(d.last_seen, serverTime) ? "เพิ่งตรวจพบ" : `พบล่าสุด ${new Date(d.last_seen).toLocaleString("th-TH")}`}{d.rssi != null ? ` · ${d.rssi} dBm` : ""}{d.source === "simulated" ? " · SIM" : ""}</small>
              <button type="button" className="topo-btn primary" disabled={busy} onClick={() => onAdopt(d.external_id, d.gateway_id)}>ลงทะเบียน</button>
            </div>
          </article>;
        })}
        {devices.length >= 100 && <p className="topo-note">แสดงสูงสุด 100 รายการต่อ gateway · ลงทะเบียนแล้วจะโหลดรายการถัดไป</p>}
      </div>;
    })}
  </section>;
}

/** A Tuya device an Aether Edge lists: imported with its key ("tuya", or "tuya_ble" once an owner/admin set it to
 * Bluetooth) or only seen on the LAN ("tuya_lan"). */
function TuyaCard({ d, busy, onAdopt }: { d: Discovery; busy: boolean; onAdopt: (external: string, gatewayId: string) => void }) {
  const lan = d.source === "tuya_lan";
  const ble = d.source === "tuya_ble";
  const key = d.key_status ?? "missing";
  // Same rule as the Edge panel and the import table (tuyaVerdict): a usable key and local capability (a device set
  // to Bluetooth is local, battery sensors included); not being seen yet is a warning, not a block. A device the Edge
  // only heard over Bluetooth is set to Bluetooth on the Edge panel first, never automatically.
  const blocked =
    lan || key === "missing"
      ? "นำเข้าคีย์จาก Tuya ก่อน"
      : key === "rejected" || key === "suspect"
        ? "คีย์ไม่ตรง · นำเข้าจาก Tuya อีกครั้ง"
        : !ble && d.local_capable === false
          ? d.ble_seen
            ? "ได้ยินทาง Bluetooth · ตั้งเป็น Bluetooth ที่แผง Aether Edge ก่อนลงทะเบียน"
            : "อุปกรณ์แบตเตอรี่ · ใช้ทาง Wi‑Fi ไม่ได้ (ถ้าเป็นอุปกรณ์ Bluetooth ตั้งที่แผง Aether Edge)"
          : "";
  const seen = ble ? d.ble_seen !== false : !!d.ip;
  return (
    <article className="topo-discovery-card">
      <div className="topo-discovery-photo">
        {ble ? <Bluetooth size={30} /> : <Wifi size={30} />}
      </div>
      <div className="topo-discovery-info">
        <strong>{lan ? "อุปกรณ์ Tuya ใน LAN" : d.description || "อุปกรณ์ Tuya"}</strong>
        <small>{lan ? "ยังไม่มีคีย์ · ชื่อและรุ่นจะมาจากการนำเข้า" : `Tuya ${d.model || (ble ? "Bluetooth" : "Wi‑Fi")}${d.kind ? ` · ${ZIGBEE_KIND_LABEL[d.kind] ?? d.kind}` : ""}`}</small>
        <code>{d.external_id}</code>
        <small>
          {ble ? (d.ble_seen ? `ได้ยินทาง Bluetooth${d.rssi != null ? ` · ${d.rssi} dBm` : ""}` : "ยังไม่ได้ยินทาง Bluetooth") : d.ip ? `พบที่ ${d.ip}${d.protocol_version ? ` · v${d.protocol_version}` : ""}` : "ไม่พบใน LAN"}
        </small>
        {blocked ? (
          <small className="topo-warn">{blocked}</small>
        ) : (
          <>
            {!seen && <small className="topo-warn">{ble ? BLE_WARNING : LAN_WARNING}</small>}
            <button type="button" className="topo-btn primary" disabled={busy} onClick={() => onAdopt(d.external_id, d.gateway_id)}>
              ลงทะเบียน
            </button>
          </>
        )}
      </div>
    </article>
  );
}

/** A Tuya device a Tuya Cloud link found in its project ("tuya_cloud"): no key, battery sensors included; it
 * registers with the Tuya Cloud profile the server attached. */
function TuyaCloudCard({ d, busy, onAdopt }: { d: Discovery; busy: boolean; onAdopt: (external: string, gatewayId: string) => void }) {
  return (
    <article className="topo-discovery-card">
      <div className="topo-discovery-photo">
        <Cloud size={30} />
      </div>
      <div className="topo-discovery-info">
        <strong>{d.description || "อุปกรณ์ Tuya"}</strong>
        <small>{`Tuya Cloud${d.model ? ` · ${d.model}` : ""}${d.profile ? ` · ${d.profile.label}` : ""}`}</small>
        <code>{d.external_id}</code>
        <small>ผ่าน Tuya Cloud · ต้องมีอินเทอร์เน็ต · ห้ามใช้กับ SOS</small>
        <button type="button" className="topo-btn primary" disabled={busy} onClick={() => onAdopt(d.external_id, d.gateway_id)}>
          ลงทะเบียน
        </button>
      </div>
    </article>
  );
}
