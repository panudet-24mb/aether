"use client";
import { Component, Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore, type ComponentType, type ReactNode } from "react";
import { Bluetooth, Box, ClipboardList, LayoutGrid, Radio } from "lucide-react";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import "../topology/topology.css";
import "./overview.css";
import { ApiError, createClientFrom, type DeviceEventRow, type Reading, type Snapshot } from "../topology/api";
import { deviceProfile, formatMAC, suggestProfile } from "../topology/catalog";
import DeviceControlsPanel from "../topology/device-controls";
import { PROJECT_COLORS } from "../topology/projects";
import { useLatest } from "../topology/use-latest";
import { useSignals } from "../topology/use-signals";
import { environmentReading, latestMeasurements, normalizeReading } from "./measurements";
import { activityStrip, doorStateOf, duration, isDoorSensor, isOccupancySensor, lastMotionText, occupancyOf } from "./office";
import { useShowSimulated } from "../demo-mode";
import type { StageApi, StageProps } from "./overview3d/stage";
import { useOutputs } from "./overview3d/outputs";
import { CONTROL_KINDS } from "./overview3d/types";
import {
  AlertsSection, DeviceDetail, EventsSection, FRESH_MS, GatewaysSection, HAZARD_LABEL, KIND_ICON, KIND_LABEL, KpiRow, LOW_BATTERY, OverviewFilters, STALE_MS, SosBanner, Spark, Strip, WearablesSection,
  activeHazards, ago, cardValue, deviceSubtitle, kpiTiles, type Alert, type AssetRow, type Card, type KpiInput, type Sensor, type Status,
} from "./overview-parts";

export type { Card, OverviewTarget } from "./overview-parts";
export { cardValue } from "./overview-parts";
import type { OverviewTarget } from "./overview-parts";

/** Card board or the 3D constellation (docs/platform/overview-3d.md), remembered per browser. */
type ViewMode = "cards" | "3d";
const MODE_KEY = "aether.overview.mode";
let viewMode: ViewMode | null = null;
const modeListeners = new Set<() => void>();
function getViewMode(): ViewMode {
  if (viewMode === null) {
    viewMode = "cards";
    try { if (window.localStorage.getItem(MODE_KEY) === "3d") viewMode = "3d"; } catch { /* storage blocked: cards */ }
  }
  return viewMode;
}
function setViewMode(mode: ViewMode) {
  viewMode = mode;
  try { window.localStorage.setItem(MODE_KEY, mode); } catch { /* still switches for this visit */ }
  for (const fn of modeListeners) fn();
}
const subscribeViewMode = (fn: () => void) => { modeListeners.add(fn); return () => { modeListeners.delete(fn); }; };

// Lazy: three.js and the stage load only when someone opens the 3D mode; the card bundle does not grow. A fresh
// lazy() per attempt, so a failed chunk load is retried the next time someone presses 3D. Browsers may keep a failed
// dynamic import in their module map, so if the retry fails too the notice offers a page reload.
const loadStage = () => lazy<ComponentType<StageProps>>(() => import("./overview3d/stage"));

/** A failed 3D chunk (or a render error inside the stage) falls back to the cards, like a missing WebGL does. */
class StageBoundary extends Component<{ onFail: (notice: string) => void; children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  componentDidCatch() { this.props.onFail("โหลดมุมมอง 3D ไม่สำเร็จ · แสดงแบบการ์ดแทน · กด 3D เพื่อลองใหม่"); }
  render() { return this.state.failed ? null : this.props.children; }
}


export default function Overview({ getToken, refresh, onUnauthorized, onNavigate, onSummary }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void; onNavigate: (to: OverviewTarget) => void; onSummary?: (openAlerts: number) => void }) {
  const showSim = useShowSimulated();
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
  const mode = useSyncExternalStore(subscribeViewMode, getViewMode, () => "cards" as ViewMode);
  const [notice3d, setNotice3d] = useState("");
  const fallBackToCards = useCallback((notice: string) => { setNotice3d(notice); setViewMode("cards"); }, []);
  const [stage, setStage] = useState(() => ({ Stage: loadStage(), failed: false, attempt: 0 }));
  const stageFailed = useCallback((notice: string) => {
    setStage((s) => ({ ...s, failed: true }));
    fallBackToCards(stage.attempt > 0 ? "โหลดมุมมอง 3D ไม่สำเร็จอีกครั้ง · แสดงแบบการ์ดแทน · โหลดหน้าใหม่เพื่อลองอีกครั้ง" : notice);
  }, [fallBackToCards, stage.attempt]);
  const open3d = () => {
    setNotice3d("");
    if (stage.failed) setStage((s) => ({ Stage: loadStage(), failed: false, attempt: s.attempt + 1 }));
    setViewMode("3d");
  };
  const OverviewStage = stage.Stage;
  const stageApi = useRef<StageApi | null>(null);

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
    if (signal === "layout") return; // the connect canvas layout does not change this page
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
  // Tuya Wi‑Fi cards on the board: asked once per device whether they have a switch (cached for the page).
  // Only the board asks here; in 3D the stage asks for its own (same cache).
  const hasOutput = useOutputs(client, mode === "3d" ? [] : visible.filter((c) => c.kind === "tuya" && c.deviceId && c.registered && c.status !== "offline").map((c) => c.deviceId!));
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
  const recentEvents = (snapshot?.events ?? []).filter((e) => scopedCards.some((c) => c.external === e.external_id.toLowerCase())).slice(0, 14);
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

  const value = (c: Card) => cardValue(c, now);
  const projects = snapshot?.projects ?? [];
  const deviceEvents = snapshot?.events ?? [];
  const kpi: KpiInput = { sosAlerts, scopedAlerts, gateways: scopedGateways, registered, wearables, rooms: rooms.length, occupiedRooms, doorsLeftOpen, lowBattery, overdue, onNavigate, onOnly: setOnly, onKind: setKind };
  const filterProps = { projects, project, onProject: setProject, only, onOnly: setOnly, kinds, kind, onKind: setKind, query, onQuery: setQuery };
  const liveChip = <span className={`topo-live ${connected ? "is-on" : ""}`} title={connected ? "อัปเดตแบบ real-time" : "ตรวจเป็นรอบทุก 5 วินาที"}><i aria-hidden="true" /> {connected ? "Live" : "Polling"}</span>;
  const shadowChip = shadow && <span className="ov-shadow" title="ALERTS_SHADOW=true · ระบบบันทึกเหตุการณ์ตามปกติ แต่ไม่เปิดการแจ้งเตือน ไม่ส่งข้อความ และไม่รันออโตเมชัน ยกเว้นปุ่มฉุกเฉิน SOS และเครื่องตรวจควัน / แก๊ส / CO ที่แจ้งเตือนเสมอ · ใช้ช่วงทดสอบอุปกรณ์จริง">โหมดเงา · แจ้งเตือนเฉพาะ SOS และควัน / แก๊ส</span>;
  // Keyed by device: a different device never inherits the previous one's controls or command state.
  const detailBody = opened && <DeviceDetail key={opened.key} opened={opened} now={now} events={deviceEvents} history={drawerHistory} range={range} onRange={setRange} client={client} getToken={getToken} onNavigate={onNavigate} />;

  if (mode === "3d") {
    // 3D: the whole page is one scene; KPIs, SOS, the side lists and the device detail live inside it.
    const deviceOf = (a: Alert) => a.external_id.toLowerCase();
    return (
      <section className="topo ov ov-3d">
        {!model ? <p className="ov-empty ov-3d-wait">กำลังโหลดข้อมูลจริง…</p> : (
          <StageBoundary key={stage.attempt} onFail={stageFailed}>
            <Suspense fallback={<p className="ov-empty ov-3d-wait">กำลังเตรียมภาพ 3 มิติ…</p>}>
              <OverviewStage api={stageApi} cards={visible} gateways={scopedGateways} projects={projects} colors={PROJECT_COLORS} showEmptyHubs={kind === "all" && only === "all" && !query.trim()}
                client={client} kindLabel={KIND_LABEL} renderValue={value} onOpen={setOpenKey} onUnavailable={fallBackToCards} onShowCards={() => { setOpenKey(""); setViewMode("cards"); }}
                filters={<OverviewFilters {...filterProps} />}
                status={<>{liveChip}{shadowChip}{error && <span className="ov3d-error" role="alert">{error} · ระบบจะลองใหม่อัตโนมัติ</span>}</>}
                kpis={kpiTiles(kpi)}
                sos={sosAlerts.length ? { key: deviceOf(sosAlerts[0]), node: <SosBanner className="ov3d-sos" sosAlerts={sosAlerts} gatewayName={gatewayName} now={now} onNavigate={onNavigate} /> } : null}
                panels={[
                  { key: "alerts", node: <AlertsSection alerts={scopedAlerts} sosAlerts={sosAlerts} now={now} onPick={(a) => setOpenKey(deviceOf(a))} onAll={() => onNavigate("alerts")} /> },
                  { key: "wearables", node: wearables.length > 0 ? <WearablesSection wearables={wearables} onPick={setOpenKey} onFloorplan={() => onNavigate("floorplan")} max={8} /> : null },
                  { key: "gateways", node: <GatewaysSection gateways={scopedGateways} now={now} onPick={(g) => stageApi.current?.focusHub(g.id)} max={8} onAll={() => onNavigate("connect")} /> },
                  { key: "events", node: <EventsSection events={recentEvents} now={now} onPick={setOpenKey} max={10} onAll={() => onNavigate("alerts")} /> },
                ]}
                detail={opened && detailBody ? { key: opened.key, title: opened.name, subtitle: deviceSubtitle(opened), sos: opened.sos, body: detailBody } : null}
                onCloseDetail={() => setOpenKey("")} />
            </Suspense>
          </StageBoundary>
        )}
      </section>
    );
  }

  return (
    <section className="topo ov">
      <header className="topo-bar ov-bar">
        <h1 className="topo-bar-title">ภาพรวม</h1>
        {liveChip}
        <div className="fp-seg ov-seg ov-mode" role="group" aria-label="รูปแบบการแสดงผล">
          <button type="button" className="is-on" aria-pressed={true} onClick={() => setViewMode("cards")}><LayoutGrid size={13} aria-hidden="true" />การ์ด</button>
          <button type="button" aria-pressed={false} onClick={open3d} title="ภาพรวมทั้งหน้าเป็นภาพ 3 มิติ · ตัวกรองเดียวกับการ์ด"><Box size={13} aria-hidden="true" />3D</button>
        </div>
        {shadowChip}
        <OverviewFilters {...filterProps} extra={
          <select className="ov-select" aria-label="จัดกลุ่ม" value={group} onChange={(e) => setGroup(e.target.value as typeof group)}>
            <option value="gateway">กลุ่มตาม gateway / โซน</option>
            <option value="kind">กลุ่มตามชนิด</option>
            <option value="none">ไม่จัดกลุ่ม</option>
          </select>
        } />
      </header>

      <div className="ov-scroll">
        {error && <div className="topo-alert is-error" role="alert">{error} · ระบบจะลองใหม่อัตโนมัติ</div>}
        {notice3d && <div className="topo-alert ov-notice" role="status">{notice3d}{stage.failed && stage.attempt > 0 && <button type="button" className="ov-link" onClick={() => window.location.reload()}>โหลดหน้าใหม่</button>}<button type="button" className="ov-link" onClick={() => setNotice3d("")}>ปิด</button></div>}
        {sosAlerts.length > 0 && <SosBanner sosAlerts={sosAlerts} gatewayName={gatewayName} now={now} onNavigate={onNavigate} />}
        <KpiRow {...kpi} />

        <div className="ov-main">
          <div className="ov-board">
            {!model && <p className="ov-empty">กำลังโหลดข้อมูลจริง…</p>}
            {model && visible.length === 0 && <p className="ov-empty">{model.cards.length === 0 ? "ยังไม่มีอุปกรณ์ส่งข้อมูล · เชื่อม gateway ในหน้าเชื่อมต่ออุปกรณ์ก่อน" : "ไม่มีอุปกรณ์ตรงกับตัวกรองนี้"}</p>}
            {groups.map(([title, cards]) => (
              <section key={title || "all"} className="ov-group">
                {title && <h2>{group === "gateway" ? <Radio size={13} /> : <ClipboardList size={13} />} {title} <small>{cards.length}</small></h2>}
                <div className="ov-grid">
                  {cards.map((c) => {
                    const Icon = KIND_ICON[c.kind] ?? Bluetooth, color = c.projectId ? PROJECT_COLORS[projects.find((p) => p.id === c.projectId)?.color ?? ""] : undefined;
                    const cardStrip = c.occupancy ? activityStrip(c.samples, now, "motion", 1, 5 * 60000) : c.door ? activityStrip(c.samples, now, "door", 1, 5 * 60000) : null;
                    const series = c.kind === "environment" ? c.history.filter(environmentReading).slice(-40).map((h) => h.temperature) : c.history.slice(-40).map((h) => Number((h.metrics as Record<string, number> | undefined)?.accel_g ?? NaN)).filter((v) => Number.isFinite(v));
                    const controllable = !!c.deviceId && c.registered && CONTROL_KINDS.has(c.kind) && c.status !== "offline" && (c.kind !== "tuya" || hasOutput(c.deviceId));
                    return (
                      <div key={c.key} className={`ov-card-slot${controllable ? " has-controls" : ""}`}>
                      <button type="button" className={`ov-card is-${c.status}${c.sos ? " is-sos" : ""}${c.door?.leftOpen && c.status !== "offline" ? " is-door-long" : ""}${c.occupancy?.occupied && c.status !== "offline" ? " is-occupied" : ""}`} onClick={() => setOpenKey(c.key)} style={color ? ({ "--ov-project": color } as React.CSSProperties) : undefined}>
                        <span className="ov-card-head">
                          <span className="ov-card-icon">{c.image ? <img src={c.image} alt="" /> : <Icon size={18} />}</span>
                          <span className="ov-card-name"><strong>{c.name}</strong><small>{c.model || formatMAC(c.external)}{showSim && c.simulated ? " · SIM" : ""}{!c.registered ? " · ยังไม่ adopt" : ""}</small></span>
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
                      {controllable && <div className="ov-card-controls"><DeviceControlsPanel variant="compact" client={client} deviceId={c.deviceId!} refreshKey={c.reading?.received_at} /></div>}
                      </div>
                    );
                  })}
                </div>
              </section>
            ))}
          </div>

          <aside className="ov-side">
            <AlertsSection alerts={scopedAlerts} sosAlerts={sosAlerts} now={now} onPick={() => onNavigate("alerts")} />
            {wearables.length > 0 && <WearablesSection wearables={wearables} onPick={setOpenKey} onFloorplan={() => onNavigate("floorplan")} />}
            <GatewaysSection gateways={scopedGateways} now={now} onPick={() => onNavigate("connect")} />
            <EventsSection events={recentEvents} now={now} onPick={setOpenKey} />
          </aside>
        </div>
      </div>

      <Sheet open={opened !== null} onOpenChange={(o) => { if (!o) setOpenKey(""); }}>
        <SheetContent className="ov-sheet">
          {opened && (
            <>
              <SheetHeader><SheetTitle>{opened.name}</SheetTitle><SheetDescription>{deviceSubtitle(opened)}</SheetDescription></SheetHeader>
              {detailBody}
            </>
          )}
        </SheetContent>
      </Sheet>
    </section>
  );
}
