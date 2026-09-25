"use client";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Activity, AlertTriangle, BatteryLow, Bell, Blinds, Bluetooth, ClipboardList, Cpu, DoorOpen, Droplets, ExternalLink, Fan, Flame, Footprints, Gamepad2, Heater, Lightbulb, Lock, Move, PersonStanding, Radio, Router, Search, ShieldAlert, Siren, Sun, Thermometer, ToggleRight, Wrench, Zap } from "lucide-react";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import "../topology/topology.css";
import "./overview.css";
import { ApiError, createClientFrom, type DeviceEventRow, type LiveSensor, type Reading, type Snapshot } from "../topology/api";
import { deviceProfile, formatMAC, suggestProfile, switchGangs } from "../topology/catalog";
import DeviceControlsPanel from "../topology/device-controls";
import { PROJECT_COLORS } from "../topology/projects";
import { useLatest } from "../topology/use-latest";
import { useSignals } from "../topology/use-signals";
import { environmentReading, latestMeasurements, normalizeReading } from "./measurements";
import { COMFORT_NOTE, DOOR_LEFT_OPEN_MS, OCCUPIED_WINDOW_MS, activityStrip, comfortOf, doorStateOf, doorText, duration, isDoorSensor, isOccupancySensor, lastMotionText, occupancyOf, type DoorState, type Occupancy, type StripCell } from "./office";
import TemplateSettings from "../template-settings";

// `sos` comes from the API (a critical alert raised by a `button` event); the board never re-derives it.
type Alert = { id: string; title: string; severity: string; device_name: string; external_id: string; gateway_id: string; opened_at: string; status: string; sos?: boolean };
type AssetRow = { asset_kind: string; asset_id: string; name: string; external_id: string; next_due: string; overdue_count: number; battery: number | null };
type Thresholds = { temperature_high: number | null; humidity_high: number | null };
type Sensor = LiveSensor & { thresholds?: Thresholds };
type Status = "alert" | "online" | "stale" | "offline";
type Card = {
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

const FRESH_MS = 60000, STALE_MS = 15 * 60000, LOW_BATTERY = 20;
const KIND_LABEL: Record<string, string> = { occupancy: "การใช้ห้อง (PIR)", door: "ประตู", environment: "อุณหภูมิ / ความชื้น", motion: "การเคลื่อนไหว", tamper: "กันถอด", beacon: "Beacon / ปุ่ม", leak: "น้ำรั่ว", light: "แสง", info: "ข้อมูลอุปกรณ์", switch: "สวิตช์ไฟ",
  lighting: "หลอดไฟ", cover: "ม่าน / มู่ลี่", lock: "กลอนประตู", climate: "ควบคุมอุณหภูมิ", fan: "พัดลม", remote: "รีโมต / ปุ่ม", sos: "ปุ่มฉุกเฉิน", hazard: "ควัน / แก๊ส / CO", metering: "มิเตอร์ไฟฟ้า", zigbee: "อุปกรณ์ Zigbee" };
const KIND_ICON: Record<string, typeof Thermometer> = { occupancy: PersonStanding, door: DoorOpen, environment: Thermometer, motion: Move, tamper: ShieldAlert, beacon: Bluetooth, leak: Droplets,
  switch: ToggleRight, lighting: Lightbulb, cover: Blinds, lock: Lock, climate: Heater, fan: Fan, remote: Gamepad2, sos: Siren, hazard: Flame, metering: Zap, light: Sun, info: Cpu, zigbee: Cpu };
/** Units of the metrics a generic Zigbee2MQTT card may show (the names Zigbee2MQTT uses). */
const METRIC_UNIT: Record<string, string> = { temperature: "°C", local_temperature: "°C", device_temperature: "°C", humidity: "%", pressure: "hPa", co2: "ppm", voc: "ppb", formaldehyd: "mg/m³", pm25: "µg/m³", pm10: "µg/m³", illuminance: "lx", power: "W", energy: "kWh", current: "A", voltage: "V", position: "%", occupied_heating_setpoint: "°C", current_heating_setpoint: "°C", soil_moisture: "%", linkquality: "LQI" };
const HAZARD_LABEL: Record<string, string> = { smoke: "ตรวจพบควัน", gas: "ตรวจพบแก๊สรั่ว", carbon_monoxide: "ตรวจพบ CO" };
const activeHazards = (m: Record<string, number>) => Object.keys(HAZARD_LABEL).filter((h) => m[h] === 1);
const EVENT_LABEL: Record<string, string> = { tamper: "ป้ายถูกถอด", tamper_cleared: "tamper กลับสู่ปกติ", button: "กดปุ่ม", leak: "พบน้ำรั่ว", leak_cleared: "น้ำรั่วหาย", motion: "เริ่มเคลื่อนไหว", motion_stopped: "หยุดเคลื่อนไหว", offline: "ขาดการติดต่อ", online: "กลับมาออนไลน์", threshold: "ค่าเกินเกณฑ์", threshold_cleared: "ค่ากลับเข้าเกณฑ์", zone: "เข้าโซนใหม่", automation: "ออโตเมชันทำงาน", door_open: "เปิดประตู", door_closed: "ปิดประตู", occupied: "มีคนในพื้นที่", vacant: "ไม่มีคนแล้ว", hazard: "ตรวจพบควัน / แก๊ส / CO", hazard_cleared: "ควัน / แก๊ส / CO หายแล้ว", action: "กดปุ่ม / รีโมต", switch_on: "สวิตช์เปิด", switch_off: "สวิตช์ปิด" };
const EVENT_TONE: Record<string, string> = { tamper: "bad", button: "bad", leak: "bad", offline: "bad", threshold: "warn", motion: "warn", zone: "info", door_open: "info", occupied: "info" };

/** Small cell strip: occupancy (motion) or door-open over time; 24 h in the drawer, the last hour on a card. */
function Strip({ cells, label, className = "" }: { cells: StripCell[]; label: string; className?: string }) {
  return (
    <span className={`ov-strip ${className}`} role="img" aria-label={label}>
      {cells.map((c, i) => <i key={i} className={`is-${c}`} />)}
    </span>
  );
}

function ago(at: string | null | undefined, now: number): string {
  if (!at) return "ยังไม่มีข้อมูล";
  const s = Math.max(0, Math.round((now - Date.parse(at)) / 1000));
  if (s < 60) return `${s} วิ`;
  if (s < 3600) return `${Math.round(s / 60)} นาที`;
  if (s < 86400) return `${Math.round(s / 3600)} ชม.`;
  return `${Math.round(s / 86400)} วัน`;
}

function Spark({ values, tone }: { values: number[]; tone: string }) {
  if (values.length < 2) return <div className="ov-spark is-empty" />;
  const lo = Math.min(...values), hi = Math.max(...values), span = Math.max(hi - lo, 0.5);
  const pts = values.map((v, i) => `${((i / (values.length - 1)) * 100).toFixed(1)},${(22 - ((v - lo) / span) * 20).toFixed(1)}`).join(" ");
  return <svg className={`ov-spark tone-${tone}`} viewBox="0 0 100 24" preserveAspectRatio="none" aria-hidden="true"><polyline points={pts} fill="none" strokeWidth="1.6" vectorEffect="non-scaling-stroke" /></svg>;
}

const number = (value: number | undefined, digits = 0) => value != null && Number.isFinite(value) ? value.toFixed(digits) : "—";

function Chart({ readings: rawReadings, metric, limit }: { readings: Reading[]; metric: "temperature" | "humidity"; limit?: number | null }) {
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

export default function Overview({ getToken, refresh, onUnauthorized, onNavigate, onSummary }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void; onNavigate: (to: OverviewTarget) => void; onSummary?: (openAlerts: number) => void }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const onUnauthorizedRef = useLatest(onUnauthorized);
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const [assets, setAssets] = useState<AssetRow[]>([]);
  const [error, setError] = useState("");
  const [now, setNow] = useState(0);
  const [project, setProject] = useState("all");
  const [kind, setKind] = useState("all");
  const [only, setOnly] = useState<"all" | "attention" | "wearable">("all");
  const [group, setGroup] = useState<"gateway" | "kind" | "none">("gateway");
  const [query, setQuery] = useState("");
  const [openKey, setOpenKey] = useState("");
  const [range, setRange] = useState("24h");
  const [shadow, setShadow] = useState(false);
  const [detail, setDetail] = useState<{ key: string; range: string; history: Reading[] } | null>(null);

  const load = useCallback(async (force: boolean) => {
    try {
      const [snap, open, registry] = await Promise.all([
        client.snapshot(force),
        client.raw<{ items: Alert[] }>("/alerts?status=open&limit=50").catch(() => ({ items: [] as Alert[] })),
        client.raw<{ items: AssetRow[] }>("/assets").catch(() => ({ items: [] as AssetRow[] })),
      ]);
      setSnapshot(snap); setAlerts(open.items); setAssets(registry.items); setNow(snap.serverTime); setError("");
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) { onUnauthorizedRef.current?.(); return; }
      setError(e instanceof Error ? e.message : "โหลดข้อมูลไม่ได้");
    }
  }, [client, onUnauthorizedRef]);

  const pending = useRef<{ timer?: ReturnType<typeof setTimeout>; force: boolean }>({ force: false });
  const connected = useSignals(handlers, (signal) => {
    const p = pending.current;
    p.force = p.force || signal === "inventory" || signal === "event";
    clearTimeout(p.timer);
    p.timer = setTimeout(() => { const force = p.force; p.force = false; void load(force); }, 500);
  });
  const connectedRef = useLatest(connected);
  useEffect(() => {
    let active = true, timer: ReturnType<typeof setTimeout>;
    const p = pending.current;
    const poll = async () => { await load(false); if (active) timer = setTimeout(poll, connectedRef.current ? 15000 : 5000); };
    void poll();
    // The clock between refreshes, so "12 วิ" keeps counting and fresh turns stale on time.
    const tick = setInterval(() => setNow((n) => (n ? n + 5000 : n)), 5000);
    return () => { active = false; clearTimeout(timer); clearTimeout(p.timer); clearInterval(tick); };
  }, [load, connectedRef]);

  useEffect(() => {
    let active = true;
    client.raw<{ alerts_shadow?: boolean }>("/catalog").then((c) => { if (active) setShadow(!!c.alerts_shadow); }).catch(() => {});
    return () => { active = false; };
  }, [client]);

  const onSummaryRef = useLatest(onSummary);
  const adoptedAlertCount = alerts.filter((a) => snapshot?.devices.some((d) => d.external_id.toLowerCase() === a.external_id.toLowerCase())).length;
  useEffect(() => { onSummaryRef.current?.(adoptedAlertCount); }, [adoptedAlertCount, onSummaryRef]);

  const model = useMemo(() => {
    const s = snapshot;
    if (!s) return null;
    const gatewayBy = new Map(s.gateways.map((g) => [g.id, g]));
    const alertBy = new Map<string, Alert[]>();
    for (const a of alerts) alertBy.set(a.external_id.toLowerCase(), [...(alertBy.get(a.external_id.toLowerCase()) ?? []), a]);
    const assetBy = new Map(assets.filter((a) => a.asset_kind === "device").map((a) => [a.external_id.toLowerCase(), a]));
    const eventsBy = new Map<string, DeviceEventRow[]>();
    for (const e of s.events) { const k = e.external_id.toLowerCase(); eventsBy.set(k, [...(eventsBy.get(k) ?? []), e]); }
    // The event page is capped (see api.ts, limit=200): when it is full, "today" counts are lower bounds.
    const eventPageFull = s.events.length >= 200;
    const cards = new Map<string, Card>();
    for (const g of s.live?.gateways ?? []) {
      for (const raw of g.sensors) {
        const sensor = raw as Sensor, ext = sensor.id.toLowerCase(), prev = cards.get(ext);
        if (prev?.reading && Date.parse(prev.reading.received_at) >= Date.parse(sensor.latest.received_at)) continue;
        const reg = s.devices.find((d) => d.external_id.toLowerCase() === ext);
        if (!reg) continue;
        const profile = reg ? deviceProfile(reg.profile_id) : suggestProfile({ model: sensor.model ?? sensor.latest.model });
        const home = reg?.roaming && reg.zone_gateway_id ? reg.zone_gateway_id : g.gateway.id;
        const merged = latestMeasurements(sensor);
        // Office kinds win over the profile's first kind: the MSP01 profile lists motion + environment.
        // A generic Zigbee2MQTT registration has no fixed kind: the backend derives it from the device's own definition.
        const generic = profile?.kinds?.[0] === "zigbee";
        const kind = isDoorSensor(profile, merged) ? "door" : isOccupancySensor(profile, merged) ? "occupancy" : generic ? (merged.kind || sensor.kind || "zigbee") : profile?.kinds?.[0] ?? sensor.model ?? "info";
        cards.set(ext, {
          deviceId: reg.id, profileId: reg.profile_id, key: ext, external: ext, name: reg?.name ?? sensor.name, kind, model: profile ? `${profile.brand} ${profile.model}` : (sensor.model ?? ""), image: profile?.image ?? null,
          registered: !!reg, wearable: !!reg?.roaming || !!profile?.wearable, gatewayId: home, gatewayName: gatewayBy.get(home)?.name ?? g.gateway.name, projectId: gatewayBy.get(home)?.project_id ?? null,
          zone: reg?.roaming ? (gatewayBy.get(reg.zone_gateway_id ?? "")?.name ?? null) : null, status: "online", reasons: [], reading: merged, history: sensor.history, thresholds: sensor.thresholds, templateId: sensor.template_id, simulated: sensor.latest.source === "simulated", sos: false, samples: [...sensor.history, sensor.latest], reportedOffline: sensor.liveness === "reported" ? !!sensor.offline : undefined,
        });
      }
    }
    // Registered devices that no gateway reports in the live window still belong on the board, as offline.
    for (const d of s.devices) {
      const ext = d.external_id.toLowerCase();
      if (cards.has(ext)) continue;
      const profile = deviceProfile(d.profile_id), gw = gatewayBy.get(d.gateway_id);
      cards.set(ext, { deviceId: d.id, profileId: d.profile_id, key: ext, external: ext, name: d.name, kind: isDoorSensor(profile, null) ? "door" : isOccupancySensor(profile, null) ? "occupancy" : profile?.kinds?.[0] ?? "environment", model: profile ? `${profile.brand} ${profile.model}` : "", image: profile?.image ?? null, registered: true, wearable: !!d.roaming || !!profile?.wearable, gatewayId: d.gateway_id, gatewayName: gw?.name ?? "", projectId: gw?.project_id ?? null, zone: null, status: "offline", reasons: ["ไม่มีข้อมูลในช่วงล่าสุด"], reading: null, history: [], simulated: false, sos: false, samples: [] });
    }
    for (const c of cards.values()) {
      const r = c.reading, m = (r?.metrics ?? {}) as Record<string, number>, age = r ? now - Date.parse(r.received_at) : Infinity;
      // A wall switch is silent until someone flips it: Zigbee2MQTT's availability report, not the age of the last
      // state, says whether it is reachable.
      if (c.reportedOffline !== undefined) { if (c.reportedOffline) { c.status = "offline"; c.reasons.push("Zigbee2MQTT รายงานว่าขาดการติดต่อ"); } }
      else if (age > STALE_MS) { c.status = "offline"; if (r) c.reasons.push(`เงียบมา ${ago(r.received_at, now)}`); }
      else if (age > FRESH_MS) { c.status = "stale"; c.reasons.push(`ข้อมูลล่าสุด ${ago(r!.received_at, now)} ที่แล้ว`); }
      const events = eventsBy.get(c.external) ?? [];
      if (c.kind === "occupancy") c.occupancy = occupancyOf(c.samples, events, now);
      if (c.kind === "door") {
        c.door = doorStateOf(r, c.samples, events, now, eventPageFull);
        if (c.door.leftOpen && c.door.since) c.reasons.push(`ประตูเปิดค้าง ${duration(now - Date.parse(c.door.since))}`);
      }
      if (r && c.status !== "offline") {
        if (m.tamper === 1) c.reasons.unshift("ป้ายถูกถอด");
        for (const h of activeHazards(m)) c.reasons.unshift(HAZARD_LABEL[h]);
        if (m.battery_low === 1) c.reasons.push("แบตเตอรี่ใกล้หมด");
        if (m.leak === 1) c.reasons.unshift("พบน้ำรั่ว");
        if (c.kind === "environment" && c.thresholds?.temperature_high != null && r.temperature > c.thresholds.temperature_high) c.reasons.unshift(`อุณหภูมิเกิน ${c.thresholds.temperature_high} °C`);
        if (c.kind === "environment" && c.thresholds?.humidity_high != null && r.humidity > c.thresholds.humidity_high) c.reasons.unshift(`ความชื้นเกิน ${c.thresholds.humidity_high} %`);
      }
      for (const a of alertBy.get(c.external) ?? []) c.reasons.unshift(a.title.split(" · ").slice(-1)[0]);
      c.sos = (alertBy.get(c.external) ?? []).some((a) => a.sos);
      if (c.sos) c.reasons.unshift("กดปุ่มฉุกเฉิน · ยังไม่รับทราบ");
      if (r && Number.isFinite(r.battery) && r.battery >= 0 && r.battery < LOW_BATTERY) c.reasons.push(`แบตเตอรี่ ${r.battery}%`);
      if ((assetBy.get(c.external)?.overdue_count ?? 0) > 0) c.reasons.push("MA/PM เกินกำหนด");
      // An open alert makes a device urgent, but it must not disguise a device nobody is hearing: an offline
      // tag with a standing offline alert is still offline, and must not be counted as "in range".
      const urgent = (alertBy.get(c.external)?.length ?? 0) > 0 || (r && (m.tamper === 1 || m.leak === 1 || activeHazards(m).length > 0)) || c.reasons.some((x) => x.startsWith("อุณหภูมิเกิน") || x.startsWith("ความชื้นเกิน"));
      if (urgent && c.status !== "offline") c.status = "alert";
    }
    const list = [...cards.values()];
    const liveGateways = new Map((s.live?.gateways ?? []).map((g) => [g.gateway.id, g]));
    const gateways = s.gateways.map((g) => { const l = liveGateways.get(g.id); return { id: g.id, name: g.name, projectId: g.project_id ?? null, last: l?.last_packet_at ?? null, online: !!l?.last_packet_at && now - Date.parse(l.last_packet_at) <= FRESH_MS, sensors: l?.sensors.length ?? 0, nearby: l?.nearby_devices ?? 0 }; });
    return { cards: list, gateways };
  }, [snapshot, alerts, assets, now]);

  const inProject = useCallback((projectId: string | null) => project === "all" || (project === "none" ? !projectId : projectId === project), [project]);
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase(), rank: Record<Status, number> = { alert: 0, offline: 1, stale: 2, online: 3 };
    return (model?.cards ?? []).filter((c) => inProject(c.projectId) && (kind === "all" || c.kind === kind) && (only === "all" || (only === "wearable" ? c.wearable : c.status !== "online" || c.reasons.length > 0)) && (!q || c.name.toLowerCase().includes(q) || c.external.includes(q.replace(/:/g, "")) || c.gatewayName.toLowerCase().includes(q)))
      .sort((a, b) => Number(b.sos) - Number(a.sos) || rank[a.status] - rank[b.status] || Number(b.registered) - Number(a.registered) || a.name.localeCompare(b.name, "th"));
  }, [model, inProject, kind, only, query]);
  const groups = useMemo(() => {
    const out = new Map<string, Card[]>();
    for (const c of visible) { const key = group === "gateway" ? (c.gatewayName || "ไม่ทราบ gateway") : group === "kind" ? (KIND_LABEL[c.kind] ?? c.kind) : ""; out.set(key, [...(out.get(key) ?? []), c]); }
    return [...out.entries()];
  }, [visible, group]);

  const scopedCards = (model?.cards ?? []).filter((c) => inProject(c.projectId));
  const scopedGateways = (model?.gateways ?? []).filter((g) => inProject(g.projectId));
  const registered = scopedCards.filter((c) => c.registered);
  const wearables = scopedCards.filter((c) => c.wearable && c.registered);
  const scopedAlerts = alerts.filter((a) => scopedCards.some((c) => c.external === a.external_id.toLowerCase()));
  // Emergency button presses that nobody has acknowledged: the board leads with these, everything else waits.
  const sosAlerts = scopedAlerts.filter((a) => a.sos);
  const gatewayName = (id: string) => (model?.gateways ?? []).find((g) => g.id === id)?.name ?? "";
  const lowBattery = scopedCards.filter((c) => c.reading && c.reading.battery > 0 && c.reading.battery < LOW_BATTERY).length;
  const overdue = assets.filter((a) => a.overdue_count > 0).length;
  // One PIR sensor = one room on this board (the floor plan maps sensors to drawn zones).
  const rooms = registered.filter((c) => c.occupancy);
  const occupiedRooms = rooms.filter((c) => c.occupancy?.occupied && c.status !== "offline").length;
  const doorsLeftOpen = registered.filter((c) => c.door?.leftOpen && c.status !== "offline").length;
  const kinds = [...new Set((model?.cards ?? []).map((c) => c.kind))];
  const events = (snapshot?.events ?? []).filter((e) => scopedCards.some((c) => c.external === e.external_id.toLowerCase())).slice(0, 14);
  const opened = openKey ? (model?.cards ?? []).find((c) => c.key === openKey) ?? null : null;
  // The occupancy strip always covers 24 h, whatever range the environment charts last used.
  const fetchRange = opened?.kind === "occupancy" || opened?.kind === "door" ? "24h" : range;

  // Longer history for the drawer comes from the same endpoint with a range; the board keeps its own short window.
  useEffect(() => {
    if (!openKey) return;
    let active = true;
    client.raw<{ gateways: { sensors: Sensor[] }[] }>(`/live?range=${fetchRange}`).then((out) => {
      if (!active) return;
      let best: Sensor | undefined;
      for (const g of out.gateways) for (const s of g.sensors) if (s.id.toLowerCase() === openKey && (!best || s.history.length > best.history.length)) best = s;
      setDetail({ key: openKey, range: fetchRange, history: (best?.history ?? []).map(normalizeReading) });
    }).catch(() => {});
    return () => { active = false; };
  }, [openKey, fetchRange, client]);
  const drawerHistory = detail && detail.key === openKey && detail.range === fetchRange ? detail.history : (opened?.history ?? []);
  const doorLog = opened?.door ? (snapshot?.events ?? []).filter((e) => e.external_id.toLowerCase() === opened.external && (e.event_type === "door_open" || e.event_type === "door_closed")).slice(0, 10) : [];
  const openedComfort = opened && (opened.kind === "environment" || opened.kind === "occupancy") ? comfortOf(opened.reading?.temperature, opened.reading?.humidity) : null;

  const value = (c: Card) => {
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
  };

  return (
    <section className="topo ov">
      <header className="topo-bar ov-bar">
        <h1 className="topo-bar-title">ภาพรวม</h1>
        <span className={`topo-live ${connected ? "is-on" : ""}`} title={connected ? "อัปเดตแบบ real-time" : "ตรวจเป็นรอบทุก 5 วินาที"}><i aria-hidden="true" /> {connected ? "Live" : "Polling"}</span>
        {shadow && <span className="ov-shadow" title="ALERTS_SHADOW=true · ระบบบันทึกเหตุการณ์ตามปกติ แต่ไม่เปิดการแจ้งเตือน ไม่ส่งข้อความ และไม่รันออโตเมชัน ยกเว้นปุ่มฉุกเฉิน SOS และเครื่องตรวจควัน / แก๊ส / CO ที่แจ้งเตือนเสมอ · ใช้ช่วงทดสอบอุปกรณ์จริง">โหมดเงา · แจ้งเตือนเฉพาะ SOS และควัน / แก๊ส</span>}
        <select className="ov-select" aria-label="โปรเจค" value={project} onChange={(e) => setProject(e.target.value)}>
          <option value="all">ทุกโปรเจค</option>
          {(snapshot?.projects ?? []).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
          <option value="none">ยังไม่จัดโปรเจค</option>
        </select>
        <div className="fp-seg ov-seg" role="group" aria-label="แสดง">
          <button type="button" className={only === "all" ? "is-on" : ""} aria-pressed={only === "all"} onClick={() => setOnly("all")}>ทั้งหมด</button>
          <button type="button" className={only === "attention" ? "is-on" : ""} aria-pressed={only === "attention"} onClick={() => setOnly("attention")}>ต้องดู</button>
          <button type="button" className={only === "wearable" ? "is-on" : ""} aria-pressed={only === "wearable"} onClick={() => setOnly("wearable")}>Wearable</button>
        </div>
        <select className="ov-select" aria-label="ชนิดอุปกรณ์" value={kind} onChange={(e) => setKind(e.target.value)}>
          <option value="all">ทุกชนิด</option>
          {kinds.map((k) => <option key={k} value={k}>{KIND_LABEL[k] ?? k}</option>)}
        </select>
        <select className="ov-select" aria-label="จัดกลุ่ม" value={group} onChange={(e) => setGroup(e.target.value as typeof group)}>
          <option value="gateway">กลุ่มตาม gateway / โซน</option>
          <option value="kind">กลุ่มตามชนิด</option>
          <option value="none">ไม่จัดกลุ่ม</option>
        </select>
        <label className="topo-search"><Search size={14} /><input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="ค้นหาชื่อ / MAC / gateway" aria-label="ค้นหาอุปกรณ์" /></label>
      </header>

      <div className="ov-scroll">
        {error && <div className="topo-alert is-error" role="alert">{error} · ระบบจะลองใหม่อัตโนมัติ</div>}
        {sosAlerts.length > 0 && (
          <div className="ov-sos" role="alert">
            <Siren size={20} aria-hidden="true" />
            <div>
              <strong>SOS · กดปุ่มฉุกเฉิน {sosAlerts.length > 1 ? `${sosAlerts.length} จุด` : ""}</strong>
              <small>{sosAlerts.slice(0, 3).map((a) => `${a.device_name || a.external_id}${gatewayName(a.gateway_id) ? ` (${gatewayName(a.gateway_id)})` : ""} · ${ago(a.opened_at, now)}ที่แล้ว`).join(" · ")}{sosAlerts.length > 3 ? ` · และอีก ${sosAlerts.length - 3}` : ""}</small>
            </div>
            <button type="button" onClick={() => onNavigate("alerts")}>รับทราบ / ดูรายละเอียด</button>
            <button type="button" onClick={() => onNavigate("floorplan")}>ดูบนผังอาคาร</button>
          </div>
        )}
        <div className="ov-kpis">
          <button type="button" className={`ov-kpi ${sosAlerts.length ? "is-sos" : scopedAlerts.length ? (scopedAlerts.some((a) => a.severity === "critical") ? "is-bad" : "is-warn") : "is-ok"}`} onClick={() => onNavigate("alerts")}><Siren size={16} /><strong>{sosAlerts.length || scopedAlerts.length}</strong><span>{sosAlerts.length ? `SOS · กดปุ่มฉุกเฉิน${scopedAlerts.length > sosAlerts.length ? ` · อื่น ๆ ${scopedAlerts.length - sosAlerts.length}` : ""}` : `การแจ้งเตือนเปิดอยู่${scopedAlerts.some((a) => a.severity === "critical") ? ` · วิกฤต ${scopedAlerts.filter((a) => a.severity === "critical").length}` : ""}`}</span></button>
          <button type="button" className={`ov-kpi ${scopedGateways.some((g) => !g.online) ? "is-warn" : "is-ok"}`} onClick={() => onNavigate("connect")}><Router size={16} /><strong>{scopedGateways.filter((g) => g.online).length}<small>/{scopedGateways.length}</small></strong><span>gateway ส่งข้อมูลใน 60 วิ</span></button>
          <button type="button" className={`ov-kpi ${registered.some((c) => c.status === "offline") ? "is-warn" : "is-ok"}`} onClick={() => setOnly("attention")}><Activity size={16} /><strong>{registered.filter((c) => c.status !== "offline").length}<small>/{registered.length}</small></strong><span>อุปกรณ์ที่ลงทะเบียนยังส่งข้อมูล</span></button>
          <button type="button" className="ov-kpi is-info" onClick={() => setOnly("wearable")}><Footprints size={16} /><strong>{wearables.filter((c) => c.status === "online" || c.status === "alert").length}<small>/{wearables.length}</small></strong><span>wearable อยู่ในระยะ</span></button>
          {rooms.length > 0 ? (
            <>
              {/* Smart office: the row stays at six. Low battery and overdue MA/PM both open the assets page and are
                  maintenance, not "right now" office state, so they share one tile and the freed slot shows occupancy. */}
              <button type="button" className={`ov-kpi ${lowBattery || overdue ? "is-warn" : "is-ok"}`} onClick={() => onNavigate("assets")}><Wrench size={16} /><strong>{lowBattery + overdue}</strong><span>ต้องบำรุงรักษา · แบตต่ำ {lowBattery} · MA/PM เกิน {overdue}</span></button>
              <button type="button" className={`ov-kpi ${doorsLeftOpen ? "is-warn" : "is-occ"}`} onClick={() => { setKind("occupancy"); setOnly("all"); }} title={`มีคน = พบการเคลื่อนไหวภายใน ${OCCUPIED_WINDOW_MS / 60000} นาที · ประตูเปิดค้างเกิน ${DOOR_LEFT_OPEN_MS / 60000} นาทีถูกนับแยก`}><PersonStanding size={16} /><strong>{occupiedRooms}<small>/{rooms.length}</small></strong><span>ห้องที่มีคน{doorsLeftOpen ? ` · ประตูเปิดค้าง ${doorsLeftOpen}` : ""}</span></button>
            </>
          ) : (
            <>
              <button type="button" className={`ov-kpi ${lowBattery ? "is-warn" : "is-ok"}`} onClick={() => onNavigate("assets")}><BatteryLow size={16} /><strong>{lowBattery}</strong><span>แบตเตอรี่ต่ำกว่า {LOW_BATTERY}%</span></button>
              <button type="button" className={`ov-kpi ${overdue ? "is-warn" : "is-ok"}`} onClick={() => onNavigate("assets")}><Wrench size={16} /><strong>{overdue}</strong><span>MA/PM เกินกำหนด</span></button>
            </>
          )}
        </div>

        <div className="ov-main">
          <div className="ov-board">
            {!model && <p className="ov-empty">กำลังโหลดข้อมูลจริง…</p>}
            {model && visible.length === 0 && <p className="ov-empty">{model.cards.length === 0 ? "ยังไม่มีอุปกรณ์ส่งข้อมูล · เชื่อม gateway ในหน้าเชื่อมต่ออุปกรณ์ก่อน" : "ไม่มีอุปกรณ์ตรงกับตัวกรองนี้"}</p>}
            {groups.map(([title, cards]) => (
              <section key={title || "all"} className="ov-group">
                {title && <h2>{group === "gateway" ? <Radio size={13} /> : <ClipboardList size={13} />} {title} <small>{cards.length}</small></h2>}
                <div className="ov-grid">
                  {cards.map((c) => {
                    const Icon = KIND_ICON[c.kind] ?? Bluetooth, color = c.projectId ? PROJECT_COLORS[(snapshot?.projects ?? []).find((p) => p.id === c.projectId)?.color ?? ""] : undefined;
                    const cardStrip = c.occupancy ? activityStrip(c.samples, now, "motion", 1, 5 * 60000) : c.door ? activityStrip(c.samples, now, "door", 1, 5 * 60000) : null;
                    const series = c.kind === "environment" ? c.history.filter(environmentReading).slice(-40).map((h) => h.temperature) : c.history.slice(-40).map((h) => Number((h.metrics as Record<string, number> | undefined)?.accel_g ?? NaN)).filter((v) => Number.isFinite(v));
                    return (
                      <button key={c.key} type="button" className={`ov-card is-${c.status}${c.sos ? " is-sos" : ""}${c.door?.leftOpen && c.status !== "offline" ? " is-door-long" : ""}${c.occupancy?.occupied && c.status !== "offline" ? " is-occupied" : ""}`} onClick={() => setOpenKey(c.key)} style={color ? ({ "--ov-project": color } as React.CSSProperties) : undefined}>
                        <span className="ov-card-head">
                          <span className="ov-card-icon">{c.image ? <img src={c.image} alt="" /> : <Icon size={18} />}</span>
                          <span className="ov-card-name"><strong>{c.name}</strong><small>{c.model || formatMAC(c.external)}{c.simulated ? " · SIM" : ""}{!c.registered ? " · ยังไม่ adopt" : ""}</small></span>
                          {c.sos && <span className="ov-sos-tag">SOS</span>}
                          <i className="ov-dot" aria-label={c.status} />
                        </span>
                        {value(c)}
                        {cardStrip ? <Strip cells={cardStrip} className={c.door ? "is-door" : ""} label={c.door ? "ประตูเปิดในชั่วโมงที่ผ่านมา ช่องละ 5 นาที" : "การเคลื่อนไหวในชั่วโมงที่ผ่านมา ช่องละ 5 นาที"} /> : <Spark values={series} tone={c.status === "alert" ? "bad" : c.kind === "environment" ? "ok" : "info"} />}
                        <span className="ov-card-foot">
                          <span>{c.wearable && c.zone && c.status !== "offline" ? `อยู่ที่ ${c.zone}` : c.reasons[0] ?? (c.occupancy ? lastMotionText(c.occupancy, now) : c.door ? `วันนี้เปิด ${c.door.countPartial ? "≥ " : ""}${c.door.openCountToday} ครั้ง` : c.reading ? `${ago(c.reading.received_at, now)} ที่แล้ว` : "")}</span>
                          <span>{c.reading && c.reading.battery > 0 ? `${c.reading.battery}%` : ""}{c.reading?.rssi != null ? ` ${c.reading.rssi} dBm` : ""}</span>
                        </span>
                      </button>
                    );
                  })}
                </div>
              </section>
            ))}
          </div>

          <aside className="ov-side">
            <section>
              <h2><Bell size={13} /> ต้องดูตอนนี้ <small>{scopedAlerts.length}</small></h2>
              <ul className="ov-list">
                {[...sosAlerts, ...scopedAlerts.filter((a) => !a.sos)].slice(0, 6).map((a) => <li key={a.id} className={`tone-${a.severity === "critical" ? "bad" : a.severity === "warning" ? "warn" : "info"}${a.sos ? " is-sos" : ""}`}><button type="button" onClick={() => onNavigate("alerts")}><span>{a.sos ? `SOS · ${a.device_name || a.external_id}` : a.title}</span><small>{ago(a.opened_at, now)}</small></button></li>)}
                {scopedAlerts.length === 0 && <li className="ov-none">ไม่มีการแจ้งเตือนที่เปิดอยู่</li>}
              </ul>
            </section>
            {wearables.length > 0 && (
              <section>
                <h2><Footprints size={13} /> ใครอยู่ที่ไหน <button type="button" className="ov-link" onClick={() => onNavigate("floorplan")}>ดูบนผัง <ExternalLink size={11} /></button></h2>
                <ul className="ov-list">
                  {wearables.map((w) => <li key={w.key} className={w.status === "offline" ? "tone-warn" : w.status === "alert" ? "tone-bad" : "tone-info"}><button type="button" onClick={() => setOpenKey(w.key)}><span>{w.name}</span><small>{w.status === "offline" ? "ไม่อยู่ในระยะ" : (w.zone ?? w.gatewayName)}</small></button></li>)}
                </ul>
              </section>
            )}
            <section>
              <h2><Router size={13} /> Gateway</h2>
              <ul className="ov-list">
                {scopedGateways.map((g) => <li key={g.id} className={g.online ? "tone-ok" : "tone-warn"}><button type="button" onClick={() => onNavigate("connect")}><span>{g.name}</span><small>{g.last ? `${ago(g.last, now)} · ${g.sensors} sensor · ${g.nearby} BLE` : "ยังไม่มีข้อมูล"}</small></button></li>)}
                {scopedGateways.length === 0 && <li className="ov-none">ยังไม่มี gateway</li>}
              </ul>
            </section>
            <section>
              <h2><AlertTriangle size={13} /> เหตุการณ์ล่าสุด</h2>
              <ul className="ov-list ov-events">
                {events.map((e) => <li key={e.id} className={`tone-${EVENT_TONE[e.event_type] ?? "ok"}`}><button type="button" onClick={() => setOpenKey(e.external_id.toLowerCase())}><span>{EVENT_LABEL[e.event_type] ?? e.event_type} · {e.device_name}</span><small>{ago(e.occurred_at, now)}</small></button></li>)}
                {events.length === 0 && <li className="ov-none">ยังไม่มีเหตุการณ์</li>}
              </ul>
            </section>
          </aside>
        </div>
      </div>

      <Sheet open={opened !== null} onOpenChange={(o) => { if (!o) setOpenKey(""); }}>
        <SheetContent className="ov-sheet">
          {opened && (
            <>
              <SheetHeader><SheetTitle>{opened.name}</SheetTitle><SheetDescription>{opened.model || KIND_LABEL[opened.kind] || opened.kind} · {formatMAC(opened.external)} · {opened.gatewayName}</SheetDescription></SheetHeader>
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
                  <div><strong>{opened.reading?.rssi ?? "—"} dBm</strong>RSSI</div>
                  </>}
                  {deviceProfile(opened.profileId ?? "")?.kinds?.includes("motion") && !opened.occupancy && <div><strong>{number(opened.reading?.metrics?.accel_g, 2)} g</strong>ความเร่ง</div>}
                  {opened.kind === "tamper" && <div><strong>{opened.reading?.metrics?.tamper == null ? "—" : opened.reading.metrics.tamper === 1 ? "ถูกถอด" : "ติดอยู่"}</strong>สถานะกันถอด</div>}
                  {deviceProfile(opened.profileId ?? "")?.model === "B10" && <div><strong>{(snapshot?.events ?? []).filter((e) => e.external_id.toLowerCase() === opened.external && e.event_type === "button").map((e) => new Date(e.occurred_at).toLocaleTimeString("th-TH"))[0] ?? "ยังไม่พบเหตุการณ์"}</strong>กดปุ่มล่าสุด</div>}
                  {opened.wearable && <div><strong>{opened.zone ?? "—"}</strong>โซนปัจจุบัน</div>}
                </div>
                {openedComfort && <p className="topo-note">{COMFORT_NOTE} · นอกช่วงนี้แสดงเป็น ร้อน / เย็น / ชื้น / แห้ง</p>}
                {opened.deviceId && opened.external.startsWith("0x") && <DeviceControlsPanel client={client} deviceId={opened.deviceId} refreshKey={opened.reading?.received_at} />}
                {opened.occupancy && (
                  <>
                    <h3>การใช้ห้อง 24 ชม.</h3>
                    <Strip className="is-wide" cells={activityStrip([...drawerHistory, ...opened.samples], now)} label="การใช้ห้อง 24 ชั่วโมงที่ผ่านมา ช่องละ 15 นาที" />
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
                    <div className="ov-range"><span>ย้อนหลัง</span>{["1h", "24h", "7d"].map((r) => <button key={r} type="button" className={range === r ? "is-on" : ""} aria-pressed={range === r} onClick={() => setRange(r)}>{r === "1h" ? "1 ชม." : r === "24h" ? "24 ชม." : "7 วัน"}</button>)}</div>
                    <h3>อุณหภูมิ</h3><Chart readings={drawerHistory} metric="temperature" limit={opened.thresholds?.temperature_high} />
                    <h3>ความชื้น</h3><Chart readings={drawerHistory} metric="humidity" limit={opened.thresholds?.humidity_high} />
                    <TemplateSettings key={opened.key} gateway={opened.gatewayId} external={opened.external} name={opened.name} currentTemplate={opened.templateId} getToken={getToken} />
                  </>
                )}
                <h3>เหตุการณ์ของอุปกรณ์นี้</h3>
                <ul className="ov-list">
                  {(snapshot?.events ?? []).filter((e) => e.external_id.toLowerCase() === opened.external).slice(0, 8).map((e) => <li key={e.id} className={`tone-${EVENT_TONE[e.event_type] ?? "ok"}`}><div><span>{EVENT_LABEL[e.event_type] ?? e.event_type}</span><small>{new Date(e.occurred_at).toLocaleString("th-TH", { hour12: false })}</small></div></li>)}
                  {(snapshot?.events ?? []).every((e) => e.external_id.toLowerCase() !== opened.external) && <li className="ov-none">ยังไม่มีเหตุการณ์</li>}
                </ul>
                <div className="ov-actions">
                  <button type="button" className="topo-btn" onClick={() => onNavigate("connect")}>เปิดในหน้าเชื่อมต่ออุปกรณ์</button>
                  <button type="button" className="topo-btn" onClick={() => onNavigate("assets")}>ข้อมูลทรัพย์สินและ MA/PM</button>
                  {opened.wearable && <button type="button" className="topo-btn" onClick={() => onNavigate("floorplan")}>ดูบนผังอาคาร</button>}
                </div>
              </div>
            </>
          )}
        </SheetContent>
      </Sheet>
    </section>
  );
}
