"use client";
// The Overview's shared pieces (docs/platform/overview-3d.md): types, value rendering, KPI tiles, the SOS banner, the
// side lists and the device detail. The card page and the 3D stage render these same components; the data and
// the business rules stay in overview.tsx, which computes everything once and hands it to both.

import { Fragment, type ReactNode } from "react";
import { Activity, AlertTriangle, BatteryLow, Bell, Blinds, Bluetooth, Cpu, DoorOpen, Droplets, ExternalLink, Fan, Flame, Footprints, Gamepad2, Heater, Lightbulb, Lock, Move, PersonStanding, Router, Search, ShieldAlert, Siren, Sun, Thermometer, ToggleRight, Wrench, Zap } from "lucide-react";
import type { DeviceEventRow, LiveSensor, Project, Reading } from "../topology/api";
import { deviceProfile, formatMAC, switchGangs } from "../topology/catalog";
import DeviceControlsPanel, { type CommandClient } from "../topology/device-controls";
import { environmentReading } from "./measurements";
import { isLoRaWAN, lorawanCharts, lorawanRadio, lorawanValues } from "./lorawan";
import { COMFORT_NOTE, DOOR_LEFT_OPEN_MS, OCCUPIED_WINDOW_MS, activityStrip, comfortOf, doorText, duration, type Comfort, type DoorState, type Occupancy, type StripCell } from "./office";
import TemplateSettings from "../template-settings";
import { CONTROL_KINDS } from "./overview3d/types";

// `sos` comes from the API (a critical alert raised by a `button` event); the board never re-derives it.
export type Alert = { id: string; title: string; severity: string; device_name: string; external_id: string; gateway_id: string; opened_at: string; status: string; sos?: boolean };
export type AssetRow = { asset_kind: string; asset_id: string; name: string; external_id: string; next_due: string; overdue_count: number; battery: number | null };
export type Thresholds = { temperature_high: number | null; humidity_high: number | null };
export type Sensor = LiveSensor & { thresholds?: Thresholds };
export type Status = "alert" | "online" | "stale" | "offline";
export type Card = {
  /** The registration id (core.devices), when the card is a registered device: commands target it. */
  deviceId?: string;
  profileId?: string; key: string; external: string; name: string; kind: string; model: string; image: string | null; registered: boolean; wearable: boolean;
  gatewayId: string; gatewayName: string; projectId: string | null; zone: string | null;
  status: Status; reasons: string[]; reading: Reading | null; history: Reading[]; thresholds?: Thresholds; templateId?: string | null; simulated: boolean;
  /** An open SOS alert belongs to this device: the card is the operator's "run here". */
  sos: boolean;
  /** Raw readings (history + latest, unmerged): occupancy must not date an old motion frame with a newer uplink. */
  samples: Reading[];
  /** Smart office (MOS kit): set for PIR occupancy sensors and door sensors respectively. */
  occupancy?: Occupancy;
  door?: DoorState;
  /** Zigbee2MQTT devices report their own availability; when set it decides online/offline instead of freshness. */
  reportedOffline?: boolean;
};
export type OverviewTarget = "alerts" | "connect" | "assets" | "floorplan" | "studio";

export const FRESH_MS = 60000, STALE_MS = 15 * 60000, LOW_BATTERY = 20;
export const KIND_LABEL: Record<string, string> = { occupancy: "การใช้ห้อง (PIR)", door: "ประตู", environment: "อุณหภูมิ / ความชื้น", motion: "การเคลื่อนไหว", tamper: "กันถอด", beacon: "Beacon / ปุ่ม", leak: "น้ำรั่ว", light: "แสง", info: "ข้อมูลอุปกรณ์", switch: "สวิตช์ไฟ",
  lighting: "หลอดไฟ", cover: "ม่าน / มู่ลี่", lock: "กลอนประตู", climate: "ควบคุมอุณหภูมิ", fan: "พัดลม", remote: "รีโมต / ปุ่ม", sos: "ปุ่มฉุกเฉิน", hazard: "ควัน / แก๊ส / CO", metering: "มิเตอร์ไฟฟ้า", zigbee: "อุปกรณ์ Zigbee" };
export const KIND_ICON: Record<string, typeof Thermometer> = { occupancy: PersonStanding, door: DoorOpen, environment: Thermometer, motion: Move, tamper: ShieldAlert, beacon: Bluetooth, leak: Droplets,
  switch: ToggleRight, lighting: Lightbulb, cover: Blinds, lock: Lock, climate: Heater, fan: Fan, remote: Gamepad2, sos: Siren, hazard: Flame, metering: Zap, light: Sun, info: Cpu, zigbee: Cpu };
/** Units of the metrics a generic Zigbee2MQTT card may show (the names Zigbee2MQTT uses). */
export const METRIC_UNIT: Record<string, string> = { temperature: "°C", local_temperature: "°C", device_temperature: "°C", humidity: "%", pressure: "hPa", co2: "ppm", voc: "ppb", formaldehyd: "mg/m³", pm25: "µg/m³", pm10: "µg/m³", illuminance: "lx", power: "W", energy: "kWh", current: "A", voltage: "V", position: "%", occupied_heating_setpoint: "°C", current_heating_setpoint: "°C", soil_moisture: "%", linkquality: "LQI" };
export const HAZARD_LABEL: Record<string, string> = { smoke: "ตรวจพบควัน", gas: "ตรวจพบแก๊สรั่ว", carbon_monoxide: "ตรวจพบ CO" };
export const activeHazards = (m: Record<string, number>) => Object.keys(HAZARD_LABEL).filter((h) => m[h] === 1);
export const EVENT_LABEL: Record<string, string> = { tamper: "ป้ายถูกถอด", tamper_cleared: "tamper กลับสู่ปกติ", button: "กดปุ่ม", leak: "พบน้ำรั่ว", leak_cleared: "น้ำรั่วหาย", motion: "เริ่มเคลื่อนไหว", motion_stopped: "หยุดเคลื่อนไหว", offline: "ขาดการติดต่อ", online: "กลับมาออนไลน์", threshold: "ค่าเกินเกณฑ์", threshold_cleared: "ค่ากลับเข้าเกณฑ์", zone: "เข้าโซนใหม่", automation: "ออโตเมชันทำงาน", door_open: "เปิดประตู", door_closed: "ปิดประตู", occupied: "มีคนในพื้นที่", vacant: "ไม่มีคนแล้ว", hazard: "ตรวจพบควัน / แก๊ส / CO", hazard_cleared: "ควัน / แก๊ส / CO หายแล้ว", action: "กดปุ่ม / รีโมต", switch_on: "สวิตช์เปิด", switch_off: "สวิตช์ปิด" };

export const EVENT_TONE: Record<string, string> = { tamper: "bad", button: "bad", leak: "bad", offline: "bad", threshold: "warn", motion: "warn", zone: "info", door_open: "info", occupied: "info" };

/** Small cell strip: occupancy (motion) or door-open over time; 24 h in the drawer, the last hour on a card. */
export function Strip({ cells, label, className = "" }: { cells: StripCell[]; label: string; className?: string }) {
  return (
    <span className={`ov-strip ${className}`} role="img" aria-label={label}>
      {cells.map((c, i) => <i key={i} className={`is-${c}`} />)}
    </span>
  );
}

export function ago(at: string | null | undefined, now: number): string {
  if (!at) return "ยังไม่มีข้อมูล";
  const s = Math.max(0, Math.round((now - Date.parse(at)) / 1000));
  if (s < 60) return `${s} วิ`;
  if (s < 3600) return `${Math.round(s / 60)} นาที`;
  if (s < 86400) return `${Math.round(s / 3600)} ชม.`;
  return `${Math.round(s / 86400)} วัน`;
}

export function Spark({ values, tone }: { values: number[]; tone: string }) {
  if (values.length < 2) return <div className="ov-spark is-empty" />;
  const lo = Math.min(...values), hi = Math.max(...values), span = Math.max(hi - lo, 0.5);
  const pts = values.map((v, i) => `${((i / (values.length - 1)) * 100).toFixed(1)},${(22 - ((v - lo) / span) * 20).toFixed(1)}`).join(" ");
  return <svg className={`ov-spark tone-${tone}`} viewBox="0 0 100 24" preserveAspectRatio="none" aria-hidden="true"><polyline points={pts} fill="none" strokeWidth="1.6" vectorEffect="non-scaling-stroke" /></svg>;
}

export const number = (value: number | undefined, digits = 0) => value != null && Number.isFinite(value) ? value.toFixed(digits) : "—";

/** Chart of one numeric metric (a LoRaWAN gas reading), same drawing as `Chart`. */
export function MetricChart({ readings: rawReadings, metric, label, digits }: { readings: Reading[]; metric: string; label: string; digits: number }) {
  const readings = rawReadings.filter((r) => Number.isFinite(r.metrics?.[metric]));
  if (readings.length < 2) return <div className="ov-chart-empty">ต้องมีอย่างน้อย 2 จุดในช่วงเวลานี้</div>;
  const values = readings.map((r) => r.metrics![metric]);
  const pad = Math.max((Math.max(...values) - Math.min(...values)) * 0.1, 10 ** -digits);
  const lo = Math.max(0, Math.min(...values) - pad), hi = Math.max(...values) + pad;
  const from = Date.parse(readings[0].received_at), dur = Math.max(1, Date.parse(readings[readings.length - 1].received_at) - from);
  const y = (v: number) => 150 - ((v - lo) / (hi - lo)) * 130;
  const pts = readings.map((r) => `${44 + ((Date.parse(r.received_at) - from) / dur) * 650},${y(r.metrics![metric])}`).join(" ");
  return (
    <svg className="ov-chart" viewBox="0 0 710 176" role="img" aria-label={`กราฟ ${label}`}>
      {[lo, (lo + hi) / 2, hi].map((v) => <g key={v}><line x1="44" x2="700" y1={y(v)} y2={y(v)} className="ov-chart-grid" /><text x="4" y={y(v) + 4}>{v.toFixed(digits)}</text></g>)}
      <polyline points={pts} fill="none" className="ov-chart-line is-gas" />
      <text x="44" y="172">{new Date(readings[0].received_at).toLocaleString("th-TH", { hour12: false, day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" })}</text>
      <text x="700" y="172" textAnchor="end">{new Date(readings[readings.length - 1].received_at).toLocaleTimeString("th-TH", { hour12: false })}</text>
    </svg>
  );
}

export function Chart({ readings: rawReadings, metric, limit }: { readings: Reading[]; metric: "temperature" | "humidity"; limit?: number | null }) {
  const readings = rawReadings.filter(environmentReading);
  if (readings.length < 2) return <div className="ov-chart-empty">ต้องมีอย่างน้อย 2 จุดในช่วงเวลานี้</div>;
  const values = readings.map((r) => r[metric]);
  const lo = Math.floor(Math.min(...values, limit ?? Infinity) - 1), hi = Math.ceil(Math.max(...values, limit ?? -Infinity) + 1);
  const from = Date.parse(readings[0].received_at), dur = Math.max(1, Date.parse(readings[readings.length - 1].received_at) - from);
  const y = (v: number) => 150 - ((v - lo) / (hi - lo)) * 130;
  const pts = readings.map((r) => `${44 + ((Date.parse(r.received_at) - from) / dur) * 650},${y(r[metric])}`).join(" ");
  return (
    <svg className="ov-chart" viewBox="0 0 710 176" role="img" aria-label={metric === "temperature" ? "กราฟอุณหภูมิ" : "กราฟความชื้น"}>
      {[lo, (lo + hi) / 2, hi].map((v) => <g key={v}><line x1="44" x2="700" y1={y(v)} y2={y(v)} className="ov-chart-grid" /><text x="4" y={y(v) + 4}>{v.toFixed(1)}</text></g>)}
      {limit != null && <line x1="44" x2="700" y1={y(limit)} y2={y(limit)} className="ov-chart-limit" />}
      <polyline points={pts} fill="none" className={`ov-chart-line is-${metric}`} />
      <text x="44" y="172">{new Date(readings[0].received_at).toLocaleString("th-TH", { hour12: false, day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" })}</text>
      <text x="700" y="172" textAnchor="end">{new Date(readings[readings.length - 1].received_at).toLocaleTimeString("th-TH", { hour12: false })}</text>
    </svg>
  );
}

/** A card's main value, as the board shows it (the 3D stage's labels and tooltip render this same element). */
export function cardValue(c: Card, now: number) {
  const r = c.reading, m = (r?.metrics ?? {}) as Record<string, number>;
  if (c.occupancy) return <span className={`ov-value ${c.status === "offline" ? "is-muted" : c.occupancy.occupied ? "is-occupied" : "is-vacant"}`}>{c.status === "offline" ? "ไม่มีข้อมูล" : c.occupancy.occupied ? "มีคน" : "ว่าง"}{Number.isFinite(r?.temperature) && <em>{number(r?.temperature, 1)}<small>°C</small></em>}</span>;
  if (c.door) return <span className={`ov-value ${c.door.open === null ? "is-muted" : c.door.leftOpen ? "is-warn" : c.door.open ? "is-open" : ""}`}>{c.door.open === null ? "รอสถานะ" : c.door.open ? "เปิดอยู่" : "ปิดอยู่"}{c.door.since && <em className="is-plain">{c.door.sinceExact ? "" : "≥ "}{duration(now - Date.parse(c.door.since))}</em>}</span>;
  if (c.kind === "environment") {
    const comfort = comfortOf(r?.temperature, r?.humidity);
    return <span className="ov-value">{number(r?.temperature, 1)}<small>°C</small> <em>{number(r?.humidity)}<small>%</small></em>{comfort && c.status !== "offline" && <b className={`ov-comfort ${comfort.ok ? "is-ok" : "is-off"}`} title={COMFORT_NOTE}>{comfort.label}</b>}</span>;
  }
  if (c.kind === "switch") {
    const gangs = switchGangs(m);
    if (c.status === "offline" || !gangs.length) return <span className="ov-value is-muted">{c.status === "offline" ? "ขาดการติดต่อ" : "รอสถานะ"}</span>;
    return <span className="ov-value">{gangs.filter(([, on]) => on).length}<small>/{gangs.length} ช่องเปิด</small> <em className="is-plain">{gangs.map(([gang, on]) => `${gang}:${on ? "เปิด" : "ปิด"}`).join(" ")}</em></span>;
  }
  if (c.kind === "tamper") return <span className={`ov-value ${m.tamper === 1 ? "is-bad" : ""}`}>{m.tamper === 1 ? "ถูกถอด" : "ติดอยู่"}</span>;
  if (c.kind === "leak") return <span className={`ov-value ${m.leak === 1 ? "is-bad" : ""}`}>{m.leak === 1 ? "น้ำรั่ว" : "แห้ง"}</span>;
  if (c.kind === "motion") return <span className={`ov-value ${m.vibration === 1 || m.motion === 1 ? "is-warn" : ""}`}>{m.vibration === 1 || m.motion === 1 ? "เคลื่อนไหว" : "นิ่ง"}{m.accel_g != null && <em>{Number(m.accel_g).toFixed(2)}<small>g</small></em>}</span>;
  if (c.kind === "hazard") {
    const active = activeHazards(m);
    return <span className={`ov-value ${active.length ? "is-bad" : ""}`}>{active.length ? active.map((h) => HAZARD_LABEL[h]).join(" · ") : r ? "ปกติ" : "รอสถานะ"}</span>;
  }
  if (c.kind === "lighting") return <span className={`ov-value ${m.state == null ? "is-muted" : ""}`}>{m.state == null ? "รอสถานะ" : m.state === 1 ? "เปิด" : "ปิด"}{m.brightness != null && <em>{Math.round((m.brightness / 254) * 100)}<small>%</small></em>}</span>;
  if (c.kind === "cover") return <span className="ov-value">{number(m.position)}<small>% เปิด</small>{r?.values?.state && <em className="is-plain">{r.values.state}</em>}</span>;
  if (c.kind === "lock") {
    const locked = r?.values?.lock_state ? r.values.lock_state === "locked" : m.state === 1 ? true : m.state === 0 ? false : null;
    return <span className={`ov-value ${locked === false ? "is-warn" : locked === null ? "is-muted" : ""}`}>{locked === null ? "รอสถานะ" : locked ? "ล็อกอยู่" : "ปลดล็อก"}</span>;
  }
  if (c.kind === "climate") return <span className="ov-value">{number(m.local_temperature, 1)}<small>°C</small>{(m.occupied_heating_setpoint ?? m.current_heating_setpoint) != null && <em>ตั้ง {number(m.occupied_heating_setpoint ?? m.current_heating_setpoint, 1)}<small>°C</small></em>}{r?.values?.system_mode && <em className="is-plain">{r.values.system_mode}</em>}</span>;
  if (c.kind === "metering") return <span className="ov-value">{number(m.power)}<small>W</small>{m.energy != null && <em>{number(m.energy, 2)}<small>kWh</small></em>}</span>;
  if (c.kind === "light") return <span className="ov-value">{number(m.illuminance)}<small>lx</small></span>;
  if (c.kind === "sos" || c.kind === "remote") {
    if (c.status === "offline") return <span className="ov-value is-muted">ขาดการติดต่อ</span>;
    return <span className="ov-value">พร้อมใช้งาน{Number.isFinite(r?.battery) && <em>{number(r?.battery)}<small>% แบต</small></em>}</span>;
  }
  if (c.kind === "zigbee" || c.kind === "fan" || c.kind === "info") {
    // Anything else Zigbee2MQTT reports: its first two measurements with their units.
    const shown = Object.entries(m).filter(([k]) => k !== "linkquality" && !/^sw\d$/.test(k)).slice(0, 2);
    if (shown.length) return <span className="ov-value">{shown.map(([k, v], i) => i === 0 ? <span key={k}>{Number.isInteger(v) ? v : v.toFixed(1)}<small>{METRIC_UNIT[k] ?? ` ${k}`}</small></span> : <em key={k}>{Number.isInteger(v) ? v : v.toFixed(1)}<small>{METRIC_UNIT[k] ?? ` ${k}`}</small></em>)}</span>;
  }
  if (!r) return <span className="ov-value is-muted">ไม่มีข้อมูล</span>;
  // A beacon's only value is "is it heard": an offline tag must not read as present.
  if (c.status === "offline") return <span className="ov-value is-muted">ไม่อยู่ในระยะ</span>;
  return <span className="ov-value">{c.wearable && c.zone ? "ในพื้นที่" : "อยู่ในระยะ"}</span>;
}

export type GatewayRow = { id: string; name: string; projectId: string | null; last: string | null; online: boolean; sensors: number; nearby: number };
export type Only = "all" | "attention" | "wearable";

/** Project / show / kind / search: the header bar on the card page, the floating HUD in 3D. `extra` sits before search. */
export function OverviewFilters({ projects, project, onProject, only, onOnly, kinds, kind, onKind, query, onQuery, extra }: {
  projects: Project[]; project: string; onProject: (v: string) => void; only: Only; onOnly: (v: Only) => void;
  kinds: string[]; kind: string; onKind: (v: string) => void; query: string; onQuery: (v: string) => void; extra?: ReactNode;
}) {
  return (
    <>
      <select className="ov-select" aria-label="โปรเจค" value={project} onChange={(e) => onProject(e.target.value)}>
        <option value="all">ทุกโปรเจค</option>
        {projects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
        <option value="none">ยังไม่จัดโปรเจค</option>
      </select>
      <div className="fp-seg ov-seg" role="group" aria-label="แสดง">
        <button type="button" className={only === "all" ? "is-on" : ""} aria-pressed={only === "all"} onClick={() => onOnly("all")}>ทั้งหมด</button>
        <button type="button" className={only === "attention" ? "is-on" : ""} aria-pressed={only === "attention"} onClick={() => onOnly("attention")}>ต้องดู</button>
        <button type="button" className={only === "wearable" ? "is-on" : ""} aria-pressed={only === "wearable"} onClick={() => onOnly("wearable")}>Wearable</button>
      </div>
      <select className="ov-select" aria-label="ชนิดอุปกรณ์" value={kind} onChange={(e) => onKind(e.target.value)}>
        <option value="all">ทุกชนิด</option>
        {kinds.map((k) => <option key={k} value={k}>{KIND_LABEL[k] ?? k}</option>)}
      </select>
      {extra}
      <label className="topo-search"><Search size={14} /><input value={query} onChange={(e) => onQuery(e.target.value)} placeholder="ค้นหาชื่อ / MAC / gateway" aria-label="ค้นหาอุปกรณ์" /></label>
    </>
  );
}

export type KpiInput = {
  sosAlerts: Alert[]; scopedAlerts: Alert[]; gateways: GatewayRow[]; registered: Card[]; wearables: Card[];
  rooms: number; occupiedRooms: number; doorsLeftOpen: number; lowBattery: number; overdue: number;
  onNavigate: (to: OverviewTarget) => void; onOnly: (v: Only) => void; onKind: (v: string) => void;
};

/** The six KPI tiles, each a button with its own action (a page, or a filter on the board). */
export function kpiTiles(k: KpiInput): { key: string; node: ReactNode }[] {
  const { sosAlerts, scopedAlerts, gateways, registered, wearables, rooms, occupiedRooms, doorsLeftOpen, lowBattery, overdue, onNavigate, onOnly, onKind } = k;
  const tiles = [
    { key: "alerts", node: <button type="button" className={`ov-kpi ${sosAlerts.length ? "is-sos" : scopedAlerts.length ? (scopedAlerts.some((a) => a.severity === "critical") ? "is-bad" : "is-warn") : "is-ok"}`} onClick={() => onNavigate("alerts")}><Siren size={16} /><strong>{sosAlerts.length || scopedAlerts.length}</strong><span>{sosAlerts.length ? `SOS · กดปุ่มฉุกเฉิน${scopedAlerts.length > sosAlerts.length ? ` · อื่น ๆ ${scopedAlerts.length - sosAlerts.length}` : ""}` : `การแจ้งเตือนเปิดอยู่${scopedAlerts.some((a) => a.severity === "critical") ? ` · วิกฤต ${scopedAlerts.filter((a) => a.severity === "critical").length}` : ""}`}</span></button> },
    { key: "gateways", node: <button type="button" className={`ov-kpi ${gateways.some((g) => !g.online) ? "is-warn" : "is-ok"}`} onClick={() => onNavigate("connect")}><Router size={16} /><strong>{gateways.filter((g) => g.online).length}<small>/{gateways.length}</small></strong><span>gateway ส่งข้อมูลใน 60 วิ</span></button> },
    { key: "registered", node: <button type="button" className={`ov-kpi ${registered.some((c) => c.status === "offline") ? "is-warn" : "is-ok"}`} onClick={() => onOnly("attention")}><Activity size={16} /><strong>{registered.filter((c) => c.status !== "offline").length}<small>/{registered.length}</small></strong><span>อุปกรณ์ที่ลงทะเบียนยังส่งข้อมูล</span></button> },
    { key: "wearables", node: <button type="button" className="ov-kpi is-info" onClick={() => onOnly("wearable")}><Footprints size={16} /><strong>{wearables.filter((c) => c.status === "online" || c.status === "alert").length}<small>/{wearables.length}</small></strong><span>wearable อยู่ในระยะ</span></button> },
  ];
  // Smart office: the row stays at six. Low battery and overdue MA/PM both open the assets page and are
  // maintenance, not "right now" office state, so they share one tile and the freed slot shows occupancy.
  if (rooms > 0) return [...tiles,
    { key: "maintenance", node: <button type="button" className={`ov-kpi ${lowBattery || overdue ? "is-warn" : "is-ok"}`} onClick={() => onNavigate("assets")}><Wrench size={16} /><strong>{lowBattery + overdue}</strong><span>ต้องบำรุงรักษา · แบตต่ำ {lowBattery} · MA/PM เกิน {overdue}</span></button> },
    { key: "rooms", node: <button type="button" className={`ov-kpi ${doorsLeftOpen ? "is-warn" : "is-occ"}`} onClick={() => { onKind("occupancy"); onOnly("all"); }} title={`มีคน = พบการเคลื่อนไหวภายใน ${OCCUPIED_WINDOW_MS / 60000} นาที · ประตูเปิดค้างเกิน ${DOOR_LEFT_OPEN_MS / 60000} นาทีถูกนับแยก`}><PersonStanding size={16} /><strong>{occupiedRooms}<small>/{rooms}</small></strong><span>ห้องที่มีคน{doorsLeftOpen ? ` · ประตูเปิดค้าง ${doorsLeftOpen}` : ""}</span></button> },
  ];
  return [...tiles,
    { key: "battery", node: <button type="button" className={`ov-kpi ${lowBattery ? "is-warn" : "is-ok"}`} onClick={() => onNavigate("assets")}><BatteryLow size={16} /><strong>{lowBattery}</strong><span>แบตเตอรี่ต่ำกว่า {LOW_BATTERY}%</span></button> },
    { key: "overdue", node: <button type="button" className={`ov-kpi ${overdue ? "is-warn" : "is-ok"}`} onClick={() => onNavigate("assets")}><Wrench size={16} /><strong>{overdue}</strong><span>MA/PM เกินกำหนด</span></button> },
  ];
}

/** KPI tiles in their card-page row. */
export function KpiRow(k: KpiInput) {
  return <div className="ov-kpis">{kpiTiles(k).map((t) => <Fragment key={t.key}>{t.node}</Fragment>)}</div>;
}

/** Unacknowledged emergency button presses: the board leads with these, everything else waits. */
export function SosBanner({ sosAlerts, gatewayName, now, onNavigate, className = "" }: { sosAlerts: Alert[]; gatewayName: (id: string) => string; now: number; onNavigate: (to: OverviewTarget) => void; className?: string }) {
  return (
    <div className={`ov-sos ${className}`} role="alert">
      <Siren size={20} aria-hidden="true" />
      <div>
        <strong>SOS · กดปุ่มฉุกเฉิน {sosAlerts.length > 1 ? `${sosAlerts.length} จุด` : ""}</strong>
        <small>{sosAlerts.slice(0, 3).map((a) => `${a.device_name || a.external_id}${gatewayName(a.gateway_id) ? ` (${gatewayName(a.gateway_id)})` : ""} · ${ago(a.opened_at, now)}ที่แล้ว`).join(" · ")}{sosAlerts.length > 3 ? ` · และอีก ${sosAlerts.length - 3}` : ""}</small>
      </div>
      <button type="button" onClick={() => onNavigate("alerts")}>รับทราบ / ดูรายละเอียด</button>
      <button type="button" onClick={() => onNavigate("floorplan")}>ดูบนผังอาคาร</button>
    </div>
  );
}

/** "ดูทั้งหมด": a capped list (the 3D panels) points at the full page. */
function AllLink({ onAll }: { onAll?: () => void }) {
  return onAll ? <button type="button" className="ov-link" onClick={onAll}>ดูทั้งหมด <ExternalLink size={11} /></button> : null;
}

export function AlertsSection({ alerts, sosAlerts, now, onPick, max = 6, onAll }: { alerts: Alert[]; sosAlerts: Alert[]; now: number; onPick: (a: Alert) => void; max?: number; onAll?: () => void }) {
  return (
    <section>
      <h2><Bell size={13} /> ต้องดูตอนนี้ <small>{alerts.length}</small><AllLink onAll={onAll} /></h2>
      <ul className="ov-list">
        {[...sosAlerts, ...alerts.filter((a) => !a.sos)].slice(0, max).map((a) => <li key={a.id} className={`tone-${a.severity === "critical" ? "bad" : a.severity === "warning" ? "warn" : "info"}${a.sos ? " is-sos" : ""}`}><button type="button" onClick={() => onPick(a)}><span>{a.sos ? `SOS · ${a.device_name || a.external_id}` : a.title}</span><small>{ago(a.opened_at, now)}</small></button></li>)}
        {alerts.length === 0 && <li className="ov-none">ไม่มีการแจ้งเตือนที่เปิดอยู่</li>}
      </ul>
    </section>
  );
}

export function WearablesSection({ wearables, onPick, onFloorplan, max = Infinity }: { wearables: Card[]; onPick: (key: string) => void; onFloorplan: () => void; max?: number }) {
  return (
    <section>
      <h2><Footprints size={13} /> ใครอยู่ที่ไหน <button type="button" className="ov-link" onClick={onFloorplan}>ดูบนผัง <ExternalLink size={11} /></button></h2>
      <ul className="ov-list">
        {wearables.slice(0, max).map((w) => <li key={w.key} className={w.status === "offline" ? "tone-warn" : w.status === "alert" ? "tone-bad" : "tone-info"}><button type="button" onClick={() => onPick(w.key)}><span>{w.name}</span><small>{w.status === "offline" ? "ไม่อยู่ในระยะ" : (w.zone ?? w.gatewayName)}</small></button></li>)}
        {wearables.length > max && <li className="ov-none">และอีก {wearables.length - max} คน · ดูทั้งหมดบนผัง</li>}
      </ul>
    </section>
  );
}

export function GatewaysSection({ gateways, now, onPick, max = Infinity, onAll }: { gateways: GatewayRow[]; now: number; onPick: (g: GatewayRow) => void; max?: number; onAll?: () => void }) {
  return (
    <section>
      <h2><Router size={13} /> Gateway<AllLink onAll={onAll} /></h2>
      <ul className="ov-list">
        {gateways.slice(0, max).map((g) => <li key={g.id} className={g.online ? "tone-ok" : "tone-warn"}><button type="button" onClick={() => onPick(g)}><span>{g.name}</span><small>{g.last ? `${ago(g.last, now)} · ${g.sensors} sensor · ${g.nearby} BLE` : "ยังไม่มีข้อมูล"}</small></button></li>)}
        {gateways.length === 0 && <li className="ov-none">ยังไม่มี gateway</li>}
        {gateways.length > max && <li className="ov-none">และอีก {gateways.length - max} ตัว</li>}
      </ul>
    </section>
  );
}

export function EventsSection({ events, now, onPick, max = Infinity, onAll }: { events: DeviceEventRow[]; now: number; onPick: (key: string) => void; max?: number; onAll?: () => void }) {
  return (
    <section>
      <h2><AlertTriangle size={13} /> เหตุการณ์ล่าสุด<AllLink onAll={onAll} /></h2>
      <ul className="ov-list ov-events">
        {events.slice(0, max).map((e) => <li key={e.id} className={`tone-${EVENT_TONE[e.event_type] ?? "ok"}`}><button type="button" onClick={() => onPick(e.external_id.toLowerCase())}><span>{EVENT_LABEL[e.event_type] ?? e.event_type} · {e.device_name}</span><small>{ago(e.occurred_at, now)}</small></button></li>)}
        {events.length === 0 && <li className="ov-none">ยังไม่มีเหตุการณ์</li>}
      </ul>
    </section>
  );
}

export const deviceSubtitle = (c: Card) => `${c.model || KIND_LABEL[c.kind] || c.kind} · ${formatMAC(c.external)} · ${c.gatewayName}`;

/** A device's detail: status, reasons, facts, controls, history and actions (the card page's Sheet, the 3D detail panel). */
export function DeviceDetail({ opened, now, events, history, range, onRange, client, getToken, onNavigate }: {
  opened: Card; now: number; events: DeviceEventRow[]; history: Reading[]; range: string; onRange: (r: string) => void;
  client: CommandClient; getToken: () => string; onNavigate: (to: OverviewTarget) => void;
}) {
  const doorLog = opened.door ? events.filter((e) => e.external_id.toLowerCase() === opened.external && (e.event_type === "door_open" || e.event_type === "door_closed")).slice(0, 10) : [];
  const openedComfort: Comfort | null = opened.kind === "environment" || opened.kind === "occupancy" ? comfortOf(opened.reading?.temperature, opened.reading?.humidity) : null;
  const lora = isLoRaWAN(deviceProfile(opened.profileId ?? ""));
  return (
    <div className="ov-sheet-body">
      <div className={`ov-status is-${opened.status}${opened.sos ? " is-sos" : ""}`}>{opened.sos ? "SOS · กดปุ่มฉุกเฉิน ยังไม่รับทราบ" : opened.status === "alert" ? "ต้องตรวจสอบ" :opened.status === "online" ? "ปกติ · รับข้อมูลล่าสุด" : opened.status === "stale" ? "ข้อมูลเริ่มเก่า" : "ขาดการติดต่อ"}{opened.reading ? ` · ${ago(opened.reading.received_at, now)} ที่แล้ว` : ""}</div>
      {opened.reasons.length > 0 && <ul className="ov-reasons">{opened.reasons.map((r, i) => <li key={i}>{r}</li>)}</ul>}
      <p className="topo-note">ค่าล่าสุดของแต่ละรายการ · คงไว้เมื่อเฟรมใหม่ไม่มีค่านั้น · — หมายถึงยังไม่ได้รับค่า</p><div className="ov-facts">
        {opened.occupancy && <><div className={opened.occupancy.occupied ? "is-occupied" : "is-vacant"}><strong>{opened.occupancy.occupied ? "มีคน" : "ว่าง"}</strong>สถานะห้อง</div><div><strong>{opened.occupancy.lastMotionAt ? `${duration(now - Date.parse(opened.occupancy.lastMotionAt))}ที่แล้ว` : "—"}</strong>เคลื่อนไหวล่าสุด</div></>}
        {opened.door && <><div className={opened.door.leftOpen ? "is-warn" : ""}><strong>{doorText(opened.door, now)}</strong>สถานะประตู</div><div><strong>{opened.door.countPartial ? "≥ " : ""}{opened.door.openCountToday} ครั้ง</strong>เปิดวันนี้</div>{opened.reading?.metrics?.door_open_count != null && <div><strong>{opened.reading.metrics.door_open_count} ครั้ง</strong>ตัวนับเปิดของอุปกรณ์</div>}</>}
        {(opened.kind === "environment" || (opened.kind === "occupancy" && Number.isFinite(opened.reading?.temperature))) && <><div><strong>{number(opened.reading?.temperature, 1)} °C</strong>อุณหภูมิ</div><div><strong>{number(opened.reading?.humidity)} %</strong>ความชื้น</div></>}
        {openedComfort && <div className={openedComfort.ok ? "is-ok" : "is-warn"} title={COMFORT_NOTE}><strong>{openedComfort.label}</strong>ความสบาย</div>}
        {opened.kind === "switch" ? <>
          {switchGangs(opened.reading?.metrics).map(([gang, on]) => <div key={gang} className={on ? "is-ok" : ""}><strong>{on ? "เปิด" : "ปิด"}</strong>ช่อง {gang}</div>)}
          <div><strong>{opened.reading?.metrics?.linkquality ?? "—"}</strong>สัญญาณ Zigbee (LQI)</div>
        </> : <>
        <div><strong>{number(opened.reading?.battery)}%</strong>แบตเตอรี่</div>
        {!lora && <div><strong>{opened.reading?.rssi ?? "—"} dBm</strong>RSSI</div>}
        </>}
        {deviceProfile(opened.profileId ?? "")?.kinds?.includes("motion") && !opened.occupancy && <div><strong>{number(opened.reading?.metrics?.accel_g, 2)} g</strong>ความเร่ง</div>}
        {opened.kind === "tamper" && <div><strong>{opened.reading?.metrics?.tamper == null ? "—" : opened.reading.metrics.tamper === 1 ? "ถูกถอด" : "ติดอยู่"}</strong>สถานะกันถอด</div>}
        {deviceProfile(opened.profileId ?? "")?.model === "B10" && <div><strong>{events.filter((e) => e.external_id.toLowerCase() === opened.external && e.event_type === "button").map((e) => new Date(e.occurred_at).toLocaleTimeString("th-TH"))[0] ?? "ยังไม่พบเหตุการณ์"}</strong>กดปุ่มล่าสุด</div>}
        {opened.wearable && <div><strong>{opened.zone ?? "—"}</strong>โซนปัจจุบัน</div>}
        {lora && [...lorawanValues(opened.reading), ...lorawanRadio(opened.reading)].map((v) => <div key={v.key} className={v.tone ? `is-${v.tone}` : ""}><strong>{v.value}{v.unit ? ` ${v.unit}` : ""}</strong>{v.label}</div>)}
      </div>
      {openedComfort && <p className="topo-note">{COMFORT_NOTE} · นอกช่วงนี้แสดงเป็น ร้อน / เย็น / ชื้น / แห้ง</p>}
      {opened.deviceId && (opened.external.startsWith("0x") || CONTROL_KINDS.has(opened.kind)) && <DeviceControlsPanel client={client} deviceId={opened.deviceId} refreshKey={opened.reading?.received_at} />}
      {opened.occupancy && (
        <>
          <h3>การใช้ห้อง 24 ชม.</h3>
          <Strip className="is-wide" cells={activityStrip([...history, ...opened.samples], now)} label="การใช้ห้อง 24 ชั่วโมงที่ผ่านมา ช่องละ 15 นาที" />
          <div className="ov-strip-legend"><span>24 ชม.ก่อน</span><span><i className="is-on" /> มีการเคลื่อนไหว <i className="is-off" /> ไม่มี <i className="is-none" /> ไม่มีข้อมูล</span><span>ตอนนี้</span></div>
        </>
      )}
      {opened.door && (
        <>
          <h3>ประวัติประตู (10 ครั้งล่าสุด)</h3>
          <ul className="ov-list">
            {doorLog.map((e, i) => {
              const next = doorLog[i - 1];
              return <li key={e.id} className={e.event_type === "door_open" ? "tone-warn" : "tone-ok"}><div><span>{e.event_type === "door_open" ? "เปิดประตู" : "ปิดประตู"}{next ? ` · ${duration(Date.parse(next.occurred_at) - Date.parse(e.occurred_at))}` : ""}</span><small>{new Date(e.occurred_at).toLocaleString("th-TH", { hour12: false })}</small></div></li>;
            })}
            {doorLog.length === 0 && <li className="ov-none">ยังไม่มีเหตุการณ์เปิด/ปิดในบันทึกล่าสุด</li>}
          </ul>
        </>
      )}
      {opened.kind === "environment" && (
        <>
          <div className="ov-range"><span>ย้อนหลัง</span>{["1h", "24h", "7d"].map((r) => <button key={r} type="button" className={range === r ? "is-on" : ""} aria-pressed={range === r} onClick={() => onRange(r)}>{r === "1h" ? "1 ชม." : r === "24h" ? "24 ชม." : "7 วัน"}</button>)}</div>
          <h3>อุณหภูมิ</h3><Chart readings={history} metric="temperature" limit={opened.thresholds?.temperature_high} />
          <h3>ความชื้น</h3><Chart readings={history} metric="humidity" limit={opened.thresholds?.humidity_high} />
          {lora && lorawanCharts(history).map((g) => <Fragment key={g.key}><h3>{g.label} (ppm)</h3><MetricChart readings={history} metric={g.key} label={g.label} digits={g.digits} /></Fragment>)}
          <TemplateSettings key={opened.key} gateway={opened.gatewayId} external={opened.external} name={opened.name} currentTemplate={opened.templateId} getToken={getToken} />
        </>
      )}
      <h3>เหตุการณ์ของอุปกรณ์นี้</h3>
      <ul className="ov-list">
        {events.filter((e) => e.external_id.toLowerCase() === opened.external).slice(0, 8).map((e) => <li key={e.id} className={`tone-${EVENT_TONE[e.event_type] ?? "ok"}`}><div><span>{EVENT_LABEL[e.event_type] ?? e.event_type}</span><small>{new Date(e.occurred_at).toLocaleString("th-TH", { hour12: false })}</small></div></li>)}
        {events.every((e) => e.external_id.toLowerCase() !== opened.external) && <li className="ov-none">ยังไม่มีเหตุการณ์</li>}
      </ul>
      <div className="ov-actions">
        <button type="button" className="topo-btn" onClick={() => onNavigate("connect")}>เปิดในหน้าเชื่อมต่ออุปกรณ์</button>
        <button type="button" className="topo-btn" onClick={() => onNavigate("assets")}>ข้อมูลทรัพย์สินและ MA/PM</button>
        {opened.wearable && <button type="button" className="topo-btn" onClick={() => onNavigate("floorplan")}>ดูบนผังอาคาร</button>}
      </div>
    </div>
  );
}
