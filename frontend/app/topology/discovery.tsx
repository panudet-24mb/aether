"use client";
import { useState } from "react";
import { Bluetooth, Search } from "lucide-react";
import type { Discovery, Gateway } from "./api";
import { formatMAC, suggestProfile } from "./catalog";
import { isFresh } from "./model";

export default function DiscoveryList({ items, gateways, gatewayId, serverTime, busy, onAdopt }: {
  items: Discovery[]; gateways: Gateway[]; gatewayId?: string; serverTime: number; busy: boolean;
  onAdopt: (external: string, gatewayId: string) => void;
}) {
  const [filter, setFilter] = useState("");
  const [query, setQuery] = useState("");
  const selected = gatewayId ?? filter;
  const q = query.toLowerCase().replace(/[:\s-]/g, "");
  const matches = items.filter((d) => (!selected || d.gateway_id === selected) &&
    (!q || `${d.external_id}${d.model ?? ""}${d.profile?.label ?? ""}`.toLowerCase().replace(/[:\s-]/g, "").includes(q)));
  return <section className="topo-discovery" aria-label="อุปกรณ์ที่พบใหม่">
    <p className="topo-note">อุปกรณ์ที่ gateway ได้ยินใน 24 ชั่วโมงล่าสุด · แสดงเฉพาะที่ยังไม่ลงทะเบียน</p>
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
          const suggestion = !d.model ? suggestProfile({ kind: d.kind }) : undefined;
          const profile = d.profile ?? suggestion;
          const label = profile ? `${profile.brand} ${profile.model}` : d.model || "อุปกรณ์ BLE · ยังไม่ทราบรุ่น";
          return <article className="topo-discovery-card" key={d.external_id}>
            <div className="topo-discovery-photo">{profile?.image ? <img src={profile.image} alt={label} /> : <Bluetooth size={30} />}</div>
            <div className="topo-discovery-info">
              <strong>{label}</strong>
              <small>{d.profile ? "รุ่นที่อุปกรณ์รายงาน" : suggestion ? "รุ่นแนะนำ · กรุณาตรวจสอบกับตัวอุปกรณ์" : "เลือกยี่ห้อและรุ่นได้ตอนลงทะเบียน"}</small>
              <code>{formatMAC(d.external_id)}</code>
              <small>{isFresh(d.last_seen, serverTime) ? "เพิ่งตรวจพบ" : `พบล่าสุด ${new Date(d.last_seen).toLocaleString("th-TH")}`}{d.rssi != null ? ` · ${d.rssi} dBm` : ""}{d.source === "simulated" ? " · SIM" : ""}</small>
              <button type="button" className="topo-btn primary" disabled={busy} onClick={() => onAdopt(d.external_id, d.gateway_id)}>ลงทะเบียน</button>
            </div>
          </article>;
        })}
        {devices.length >= 100 && <p className="topo-note">แสดงสูงสุด 100 รายการต่อ gateway · ลงทะเบียนแล้วจะโหลดรายการถัดไป</p>}
      </div>;
    })}
  </section>;
}
