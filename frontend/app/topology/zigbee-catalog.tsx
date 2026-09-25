"use client";
import { useEffect, useRef, useState } from "react";
import { Search, Siren } from "lucide-react";
import type { ZigbeeCatalog, ZigbeeModel } from "./api";

/** How Aether shows a Zigbee2MQTT device of each category (backend zigbee2mqtt.Profile.Category). */
export const ZIGBEE_KIND_LABEL: Record<string, string> = {
  environment: "อุณหภูมิ / คุณภาพอากาศ", door: "ประตู / หน้าต่าง", occupancy: "ตรวจจับคน (PIR)", leak: "น้ำรั่ว", hazard: "ควัน / แก๊ส / CO", sos: "ปุ่มฉุกเฉิน SOS",
  switch: "สวิตช์ / ปลั๊ก", lighting: "หลอดไฟ", cover: "ม่าน / มู่ลี่", lock: "กลอนประตู", climate: "ควบคุมอุณหภูมิ", fan: "พัดลม", remote: "รีโมต / ปุ่ม",
  metering: "มิเตอร์ไฟฟ้า", motion: "การสั่น", light: "วัดแสง", tamper: "กันถอด", info: "อื่น ๆ",
};

export type CatalogClient = { zigbeeCatalog: (q: string, opts?: { vendor?: string; category?: string; limit?: number; vendors?: boolean }) => Promise<ZigbeeCatalog> };

/**
 * "รุ่นที่รองรับ": search the Zigbee2MQTT device list (zigbee-herdsman-converters) before buying or pairing, and
 * see what Aether will show each model as. Everything on it comes from the server; nothing is fetched elsewhere.
 */
export default function ZigbeeCatalogSearch({ client }: { client: CatalogClient }) {
  const [query, setQuery] = useState("");
  const [category, setCategory] = useState("");
  const [result, setResult] = useState<ZigbeeCatalog | null>(null);
  const [error, setError] = useState("");
  const seq = useRef(0);

  useEffect(() => {
    const mine = ++seq.current;
    const timer = setTimeout(() => {
      client.zigbeeCatalog(query.trim(), { category, limit: 30 })
        .then((r) => { if (mine === seq.current) { setResult(r); setError(""); } })
        .catch(() => { if (mine === seq.current) setError("ค้นหารายการรุ่นไม่สำเร็จ"); });
    }, query ? 250 : 0);
    return () => clearTimeout(timer);
  }, [client, query, category]);

  return (
    <section className="topo-zigbee-catalog" aria-label="รุ่น Zigbee ที่รองรับ">
      <h3 className="topo-h3">รุ่นที่รองรับ (Zigbee2MQTT){result && <span className="topo-count">{result.total.toLocaleString("th-TH")}</span>}</h3>
      <p className="topo-note">ค้นหาก่อนซื้อหรือก่อน pair ว่ารุ่นไหนใช้กับ Aether ผ่าน Zigbee2MQTT ได้ และจะแสดงเป็นอุปกรณ์ประเภทใด</p>
      <div className="topo-zigbee-catalog-filters">
        <label className="topo-search"><Search size={14} /><input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="ยี่ห้อ รุ่น หรือ model id เช่น Aqara door, TS0012" aria-label="ค้นหารุ่น Zigbee" /></label>
        <select aria-label="ประเภทอุปกรณ์" value={category} onChange={(e) => setCategory(e.target.value)}>
          <option value="">ทุกประเภท</option>
          {Object.entries(ZIGBEE_KIND_LABEL).map(([id, label]) => <option key={id} value={id}>{label}</option>)}
        </select>
      </div>
      {error && <p className="topo-error">{error}</p>}
      {result && result.items.length === 0 && <p className="topo-empty">ไม่พบรุ่นที่ตรงกับคำค้น</p>}
      {result && result.items.length > 0 && (
        <ul className="topo-zigbee-catalog-list">
          {result.items.map((d: ZigbeeModel) => (
            <li key={`${d.vendor}/${d.model}`}>
              <strong>{d.vendor} {d.model}</strong>
              <span>{d.description}</span>
              <small>
                {d.sos && <Siren size={12} aria-hidden="true" />} {ZIGBEE_KIND_LABEL[d.category] ?? d.category}
                {d.zigbee_model?.length ? ` · ${d.zigbee_model.slice(0, 2).join(", ")}` : ""}
                {d.dynamic ? " · ความสามารถขึ้นกับตัวอุปกรณ์" : ""}
              </small>
            </li>
          ))}
        </ul>
      )}
      {result && result.total > result.items.length && <p className="topo-note">แสดง {result.items.length} จาก {result.total.toLocaleString("th-TH")} รุ่น · พิมพ์คำค้นให้เจาะจงขึ้น</p>}
      {result && <p className="topo-note topo-attribution">รายการรุ่นจาก zigbee-herdsman-converters {result.version} · {result.license} License · Copyright (c) 2018 Koen Kanters</p>}
    </section>
  );
}
