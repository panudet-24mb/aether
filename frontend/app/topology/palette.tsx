"use client";
import { type DragEvent, type KeyboardEvent, useState } from "react";
import { Bluetooth, GripVertical, Router, Search, Thermometer } from "lucide-react";
import { DEVICE_PROFILES, GATEWAY_MODELS, formatMAC } from "./catalog";
import type { DeviceEntity } from "./model";

export const DRAG_MIME = "application/x-aether-topology";
export type DragPayload = { kind: "gateway"; model: string } | { kind: "profile"; profile: string } | { kind: "device"; external: string };

function startDrag(e: DragEvent, payload: DragPayload) {
  e.dataTransfer.setData(DRAG_MIME, JSON.stringify(payload));
  e.dataTransfer.effectAllowed = "copy";
}

const norm = (s: string) => s.toLowerCase().replace(/[^0-9a-z\u0e00-\u0e7f]/g, "");

/** Enter/Space activate a card the same way a click does, so drag is never the only way in. */
const activate = (fn: () => void) => (e: KeyboardEvent<HTMLElement>) => {
  if (e.key === "Enter" || e.key === " ") {
    e.preventDefault();
    fn();
  }
};

export default function Palette({
  discovered,
  onCanvas,
  onPlace,
  onSelect,
  onAddGateway,
  onAddProfile,
}: {
  /** Unregistered devices heard on air. */
  discovered: DeviceEntity[];
  onCanvas: Set<string>;
  onPlace: (external: string) => void;
  onSelect: (external: string) => void;
  /** Click/keyboard alternative to dragging a card onto the canvas. */
  onAddGateway: (model: string) => void;
  onAddProfile: (profile: string) => void;
}) {
  const [query, setQuery] = useState("");
  const q = norm(query);
  const list = discovered.filter((d) => !q || norm(d.external).includes(q) || norm(d.name).includes(q));

  return (
    <aside className="topo-palette" aria-label="อุปกรณ์ที่ลากวางได้">
      <section>
        <h2>
          <Router size={15} /> Gateway
        </h2>
        <p>ลากลง canvas หรือคลิก เพื่อสร้าง gateway ใหม่</p>
        {GATEWAY_MODELS.map((m) => (
          <div key={m.id} className="topo-card" draggable onDragStart={(e) => startDrag(e, { kind: "gateway", model: m.id })} title={m.description} role="button" tabIndex={0} aria-label={`เพิ่ม ${m.label} ลง canvas`} onClick={() => onAddGateway(m.id)} onKeyDown={activate(() => onAddGateway(m.id))}>
            <GripVertical size={14} className="topo-grip" />
            <span className="topo-card-icon">{m.image ? <img className="topo-photo" src={m.image} alt="" /> : m.logo ? <img src={m.logo} alt="" /> : <Router size={18} />}</span>
            <span>
              <strong>{m.label}</strong>
              <small>{m.transport.toUpperCase()}</small>
            </span>
          </div>
        ))}
      </section>

      <section>
        <h2>
          <Thermometer size={15} /> อุปกรณ์ตาม profile
        </h2>
        <p>ลากหรือคลิกเพื่อวาง แล้วโยงเส้นไป gateway ใดก็ได้ ไม่จำกัดแบรนด์</p>
        {DEVICE_PROFILES.map((p) => (
          <div key={p.id} className="topo-card" draggable onDragStart={(e) => startDrag(e, { kind: "profile", profile: p.id })} title={p.description} role="button" tabIndex={0} aria-label={`เพิ่ม ${p.brand} ${p.model} ลง canvas`} onClick={() => onAddProfile(p.id)} onKeyDown={activate(() => onAddProfile(p.id))}>
            <GripVertical size={14} className="topo-grip" />
            <span className="topo-card-icon">{p.image ? <img className="topo-photo" src={p.image} alt="" /> : <Thermometer size={18} />}</span>
            <span>
              <strong>
                {p.brand} {p.model}
              </strong>
              <small>{p.radio === "ble" ? "BLE" : "ทุก transport"}</small>
            </span>
          </div>
        ))}
      </section>

      <section className="topo-discovered">
        <h2>
          <Bluetooth size={15} /> ยังไม่ลงทะเบียน <span className="topo-count">{discovered.length}</span>
        </h2>
        <p>อุปกรณ์ที่ยังไม่ลงทะเบียน · คลิกเพื่อดูรายละเอียด หรือกด “อุปกรณ์ที่พบใหม่” ด้านบน</p>
        <label className="topo-search">
          <Search size={14} />
          <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="ค้นหา MAC หรือชื่อ" aria-label="ค้นหาอุปกรณ์ที่ค้นพบ" />
        </label>
        <div className="topo-discovered-list">
          {list.length === 0 && <span className="topo-empty">{discovered.length ? "ไม่พบที่ตรงกับคำค้น" : "ยังไม่มีอุปกรณ์ใหม่ที่รอลงทะเบียน"}</span>}
          {list.slice(0, 60).map((d) => (
            <div
              key={d.external}
              className={`topo-card topo-card-small ${onCanvas.has(d.external) ? "is-placed" : ""}`}
              draggable
              onDragStart={(e) => startDrag(e, { kind: "device", external: d.external })}
              onClick={() => {
                onPlace(d.external);
                onSelect(d.external);
              }}
              role="button"
              tabIndex={0}
              onKeyDown={activate(() => {
                onPlace(d.external);
                onSelect(d.external);
              })}
            >
              <span className="topo-card-icon">{d.reading ? <Thermometer size={15} /> : <Bluetooth size={15} />}</span>
              <span>
                <strong>{d.name}</strong>
                <small>
                  {formatMAC(d.external)}
                  {d.heard[0]?.rssi != null ? ` · ${d.heard[0].rssi} dBm` : ""}
                  {d.simulated ? " · SIM" : ""}
                </small>
              </span>
            </div>
          ))}
          {list.length > 60 && <span className="topo-empty">แสดง 60 รายการแรก · พิมพ์ค้นหาเพื่อกรอง</span>}
        </div>
      </section>
    </aside>
  );
}
