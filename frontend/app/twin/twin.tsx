"use client";
// Digital twin page (docs/platform/digital-twin.md): one building, live, in 3D. A thin toolbar over TwinView.

import { Box, Building2, Camera, Crosshair, Expand, Grid3x3, History, Layers, Lock, LockOpen, Map as MapIcon, Orbit, Power, Radio, Table2, Users } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ApiError, createClientFrom } from "../topology/api";
import { useLatest } from "../topology/use-latest";
import { useSignals } from "../topology/use-signals";
import DeviceControlsPanel from "../topology/device-controls";
import type { Site } from "../floorplan/model";
import { LAYERS, type Layer, type PeopleMode, type TwinAlert, type TwinDevice, type TwinMarker, type TwinReplay, type TwinState, type TwinTimeline } from "./api";
import { ReplayStore, stateAt, type ReplayIndex } from "./replay/store";
import ReplayTimeline, { RANGES, thaiTime, type RangeId } from "./replay/timeline";
import { RAMPS, type HeatKind } from "./engine/heat";
import type { EngineStats, Preset } from "./engine/engine";
import TwinView, { type TwinSource, type TwinViewHandle } from "./twin-view";
import { TwinMinimap, zoneSummaries } from "./twin-plan";
import "../topology/topology.css";
import "../floorplan/floorplan.css";
import "./twin.css";

const PRESETS: { id: Preset; label: string; icon: typeof Box; key: string }[] = [
  { id: "overview", label: "ภาพรวม", icon: Box, key: "1" },
  { id: "plan", label: "มุมบน", icon: MapIcon, key: "2" },
  { id: "floor", label: "เจาะชั้น", icon: Layers, key: "3" },
  { id: "alert", label: "ตามเหตุ", icon: Crosshair, key: "4" },
  { id: "tour", label: "หมุนชม", icon: Orbit, key: "5" },
];
const PEOPLE: { id: PeopleMode; label: string }[] = [
  { id: "counts", label: "นับจำนวน" },
  { id: "tracks", label: "ติดตามแบบไม่ระบุชื่อ" },
  { id: "named", label: "แสดงชื่อ" },
];

const store = {
  get: (k: string) => { try { return localStorage.getItem(k); } catch { return null; } },
  set: (k: string, v: string) => { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
};

export default function DigitalTwin({ getToken, refresh, onUnauthorized }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const onUnauthorizedRef = useLatest(onUnauthorized);
  const [sites, setSites] = useState<Site[] | null>(null);
  const [siteId, setSiteId] = useState("");
  const [site, setSite] = useState<Site | null>(null);
  const [floorId, setFloorId] = useState("");
  const [layers, setLayers] = useState<Layer[]>(["temperature", "people"]);
  const [people, setPeople] = useState<PeopleMode>(() => (store.get("aether.twin.people") as PeopleMode) || "counts");
  const [explode, setExplode] = useState(0.55);
  const [isolate, setIsolate] = useState(false);
  const [tour, setTour] = useState(false);
  const [showData, setShowData] = useState(false);
  const [showSwitches, setShowSwitches] = useState(false);
  const [state, setState] = useState<TwinState | null>(null);
  const [selected, setSelected] = useState<TwinDevice | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);
  const [error, setError] = useState("");
  const [view, setView] = useState<ReturnType<TwinViewHandle["planView"]>>(null);
  const [locked, setLocked] = useState(false);
  const [stats, setStats] = useState<EngineStats | null>(null);
  const [sosIndex, setSosIndex] = useState(0);
  // Replay: a window of history drawn at time t (stateAt) instead of the live state.
  const [mode, setMode] = useState<"live" | "replay">("live");
  const [replayRange, setReplayRange] = useState<RangeId>("1h");
  const [win, setWin] = useState<{ from: number; to: number } | null>(null);
  const [t, setT] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState(10);
  const [timeline, setTimeline] = useState<TwinTimeline | null>(null);
  const [ix, setIx] = useState<ReplayIndex | null>(null);
  const [replayBusy, setReplayBusy] = useState(false);
  const [spoken, setSpoken] = useState("");
  // The live state replay draws its placements from (positions, names, kinds), taken when replay opens.
  const [base, setBase] = useState<TwinState | null>(null);
  const handle = useRef<TwinViewHandle | null>(null);
  const shell = useRef<HTMLElement | null>(null);
  const debug = useMemo(() => typeof location !== "undefined" && new URLSearchParams(location.search).has("twinDebug"), []);

  const fail = useCallback((e: unknown) => {
    if (e instanceof ApiError && e.status === 401) { onUnauthorizedRef.current?.(); return; }
    if (e instanceof ApiError && e.status === 403) { setError("บัญชีนี้ไม่มีสิทธิ์ดูผังอาคาร"); return; }
    if (e instanceof ApiError && e.status === 404) { setError("ไม่พบอาคารนี้ หรืออยู่นอกโปรเจกต์ของคุณ"); return; }
    setError(e instanceof Error ? e.message : "โหลดข้อมูลไม่ได้");
  }, [onUnauthorizedRef]);

  // Sites: remember the last one; ?twin=<site id> opens a given building.
  useEffect(() => {
    let active = true;
    client.raw<{ items: Site[] }>("/sites").then((r) => {
      if (!active) return;
      setSites(r.items);
      const asked = typeof location !== "undefined" ? new URLSearchParams(location.search).get("twin") : null;
      const wanted = asked || store.get("aether.twin.site") || "";
      setSiteId((r.items.find((s) => s.id === wanted) ?? r.items[0])?.id ?? "");
    }).catch((e) => { if (active) { fail(e); setSites([]); } });
    return () => { active = false; };
  }, [client, fail]);

  useEffect(() => { if (siteId) store.set("aether.twin.site", siteId); }, [siteId]);
  useEffect(() => { store.set("aether.twin.people", people); }, [people]);

  const source: TwinSource = useMemo(() => ({
    site: (id) => client.raw<Site>(`/sites/${id}`),
    state: (id, p) => client.raw<TwinState>(`/twin/sites/${id}/state?people=${p}`),
  }), [client]);

  // Signals refetch now (alerts at once, the rest coalesced by the hook); the poll is only a safety net.
  const connected = useSignals(handlers, (kind) => { if (kind !== "layout" && kind !== "command") setRefreshKey((k) => k + 1); });

  const onReady = useCallback((s: Site) => {
    setSite(s);
    setError("");
    setFloorId((cur) => (s.floors.some((f) => f.id === cur) ? cur : s.floors[0]?.id ?? ""));
  }, []);
  const onState = useCallback((s: TwinState) => { setState(s); setError(""); }, []);

  const replayFail = useCallback((e: unknown) => {
    if (e instanceof ApiError && e.status === 413) { setError("ช่วงนี้มีข้อมูลมากเกินจะแสดงในครั้งเดียว · เลือกช่วงที่สั้นลง"); return; }
    if (e instanceof ApiError && e.status === 429) { setError("ขอข้อมูลย้อนหลังถี่เกินไป · รอสักครู่"); return; }
    fail(e);
  }, [fail]);
  const replays = useMemo(() => new ReplayStore((from, to) =>
    client.raw<TwinReplay>(`/twin/sites/${siteId}/replay?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&people=${people}`)), [client, siteId, people]);

  const enterReplay = useCallback((r: RangeId) => {
    if (mode === "live" && state) setBase(state);
    const to = Math.floor((Date.now() - 60e3) / 60e3) * 60e3;
    const from = to - (RANGES.find((x) => x.id === r)?.ms ?? 3600e3);
    setReplayRange(r);
    setWin({ from, to });
    setT((cur) => (cur >= from && cur <= to ? cur : to - Math.min(15 * 60e3, (to - from) / 4)));
    setMode("replay");
    setTour(false);
  }, [mode, state]);

  // A window: its timeline and its replay (cached by the store; a window is at most 288 buckets).
  useEffect(() => {
    if (mode !== "replay" || !win || !siteId) return;
    let active = true;
    const from = new Date(win.from).toISOString(), to = new Date(win.to).toISOString();
    const id = requestAnimationFrame(() => setReplayBusy(true));
    Promise.all([
      client.raw<TwinTimeline>(`/twin/sites/${siteId}/timeline?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&people=${people}`),
      replays.get(from, to),
    ]).then(([tl, index]) => {
      if (!active) return;
      setTimeline(tl);
      setIx(index);
      setError("");
    }).catch((e) => { if (active) replayFail(e); }).finally(() => { if (active) setReplayBusy(false); });
    return () => { active = false; cancelAnimationFrame(id); };
  }, [mode, win, siteId, people, client, replays, replayFail]);

  // Playing: t moves at the chosen speed; past 70 % of the window the following window is fetched ahead.
  useEffect(() => {
    if (mode !== "replay" || !playing || !win) return;
    let last = performance.now(), frame = 0;
    const tick = (now: number) => {
      const dt = now - last;
      last = now;
      setT((cur) => {
        const next = Math.min(win.to, cur + dt * speed);
        if (next >= win.to) setPlaying(false);
        if (next > win.from + 0.7 * (win.to - win.from) && win.to + (win.to - win.from) < Date.now()) {
          replays.prefetch(new Date(win.to).toISOString(), new Date(win.to + (win.to - win.from)).toISOString());
        }
        return next;
      });
      frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [mode, playing, speed, win, replays]);

  // While playing, the time is announced politely every 10 s (the slider itself says it on focus).
  const tRef = useLatest(t);
  useEffect(() => {
    if (mode !== "replay" || !playing) return;
    const id = setInterval(() => setSpoken(`ย้อนดู ${thaiTime(tRef.current)}`), 10000);
    return () => clearInterval(id);
  }, [mode, playing, tRef]);

  const frozen = useMemo(() => (mode === "replay" && ix && base ? stateAt(ix, base, t) : null), [mode, ix, base, t]);
  const onMarker = useCallback((m: TwinMarker) => {
    setPlaying(false);
    setT(Date.parse(m.at) + 1000);
    const a = ix?.replay.alerts?.items.find((x) => x.id === m.alert_id);
    if (a) {
      const z = handle.current?.engine()?.zoneName(a.gateway_id);
      if (z) setFloorId(z.floorId);
      requestAnimationFrame(() => handle.current?.focusAlert({ id: a.id, severity: a.severity, status: "open", event: a.event, sos: a.sos, hazard: a.hazard, title: a.title, device_id: a.device_id, opened_at: a.opened_at, gateway_id: a.gateway_id }));
    }
  }, [ix]);

  // Minimap and debug overlay follow the camera a few times a second (the canvas renders on its own).
  useEffect(() => {
    const t = setInterval(() => {
      setView(handle.current?.planView() ?? null);
      if (debug) setStats(handle.current?.engine()?.stats() ?? null);
    }, 400);
    return () => clearInterval(t);
  }, [debug]);

  // The active floor's devices first, then the rest of the building.
  const controllable = useMemo(() => (state?.devices ?? []).filter((d) => d.kind === "device" && d.class)
    .sort((a, b) => Number(b.floor_id === floorId) - Number(a.floor_id === floorId) || a.name.localeCompare(b.name, "th")), [state, floorId]);
  const urgent = useMemo(() => (state?.alerts ?? []).filter((a) => (a.sos || a.hazard) && a.status === "open"), [state]);
  const focus = useCallback((a: TwinAlert) => {
    const z = handle.current?.engine()?.zoneName(a.gateway_id);
    if (z) setFloorId(z.floorId);
    handle.current?.focusAlert(a);
  }, []);

  // Keys: 1–5 camera presets, N the next open SOS.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "SELECT" || t.tagName === "TEXTAREA" || t.isContentEditable) || e.metaKey || e.ctrlKey || e.altKey) return;
      const p = PRESETS.find((x) => x.key === e.key);
      if (p) { e.preventDefault(); setTour(p.id === "tour"); handle.current?.preset(p.id); return; }
      if ((e.key === "n" || e.key === "N") && urgent.length) {
        e.preventDefault();
        const next = (sosIndex + 1) % urgent.length;
        setSosIndex(next);
        focus(urgent[next]);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [urgent, sosIndex, focus]);

  const heatKind: HeatKind | null = layers.includes("temperature") ? "temperature" : layers.includes("humidity") ? "humidity" : layers.includes("occupancy") ? "occupancy" : null;
  const [range, setRange] = useState<[number, number]>([18, 32]);
  useEffect(() => {
    const id = requestAnimationFrame(() => { if (heatKind) setRange(handle.current?.range(heatKind) ?? [18, 32]); });
    return () => cancelAnimationFrame(id);
  }, [state, heatKind, locked]);

  const toggle = (l: Layer) => setLayers((cur) => {
    if (cur.includes(l)) return cur.filter((x) => x !== l);
    // One heat layer at a time; people combine with any.
    return l === "people" ? [...cur, l] : [...cur.filter((x) => x === "people"), l];
  });

  const summaries = useMemo(() => (site && state ? zoneSummaries(site, state, layers) : []), [site, state, layers]);
  const fullscreen = () => { const el = shell.current; if (!el) return; if (document.fullscreenElement) void document.exitFullscreen(); else void el.requestFullscreen?.(); };
  const lockLegend = () => {
    if (!heatKind) return;
    const next = !locked;
    setLocked(next);
    handle.current?.lockRange(heatKind, next ? range : null);
  };
  const ramp = heatKind ? RAMPS[heatKind] : null;

  return (
    <section className="topo twin" ref={shell}>
      <header className="topo-bar twin-bar" aria-label="เครื่องมือ digital twin">
        <h1 className="topo-bar-title">Digital twin</h1>
        <span className={`topo-live ${connected ? "is-on" : ""}`} title={connected ? "อัปเดตแบบ real-time" : "ตรวจเป็นรอบ"}><i aria-hidden="true" /> {connected ? "Live" : "Polling"}</span>
        <div className="twin-seg" role="group" aria-label="สดหรือย้อนดู">
          <button type="button" className={mode === "live" ? "is-on" : ""} aria-pressed={mode === "live"} onClick={() => { setMode("live"); setPlaying(false); }}><Radio size={13} />สด</button>
          <button type="button" className={mode === "replay" ? "is-on" : ""} aria-pressed={mode === "replay"} disabled={!state} onClick={() => enterReplay(replayRange)} title="ย้อนดูเหตุการณ์"><History size={13} />ย้อนดู</button>
        </div>
        <label className="fp-site twin-site">
          <Building2 size={14} />
          <select aria-label="เลือกอาคาร" value={siteId} onChange={(e) => { setSiteId(e.target.value); setState(null); setSite(null); }}>
            {(sites ?? []).length === 0 && <option value="">ยังไม่มีอาคาร</option>}
            {(sites ?? []).map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
          </select>
        </label>
        <div className="twin-seg" role="group" aria-label="ชั้น">
          {(site?.floors ?? []).map((f) => <button key={f.id} type="button" className={f.id === floorId ? "is-on" : ""} aria-pressed={f.id === floorId} onClick={() => setFloorId(f.id)} title={f.name}>L{f.level}</button>)}
        </div>
        <div className="twin-seg" role="group" aria-label="ชั้นข้อมูล">
          {LAYERS.map((l) => <button key={l.id} type="button" className={layers.includes(l.id) ? "is-on" : ""} aria-pressed={layers.includes(l.id)} onClick={() => toggle(l.id)}>{l.id === "people" ? <Users size={13} /> : null}{l.label}</button>)}
        </div>
        {layers.includes("people") && (
          <select className="twin-mini" aria-label="วิธีแสดงคน" value={people} onChange={(e) => setPeople(e.target.value as PeopleMode)} title="การแสดงคนเป็นข้อมูลส่วนบุคคล · ระบบอาจลดระดับตามสิทธิ์">
            {PEOPLE.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
          </select>
        )}
        <div className="twin-seg twin-presets" role="group" aria-label="มุมกล้อง">
          {PRESETS.map((p) => <button key={p.id} type="button" className={p.id === "tour" && tour ? "is-on" : ""} aria-pressed={p.id === "tour" ? tour : undefined} title={`${p.label} (${p.key})`} aria-label={p.label} onClick={() => { setTour(p.id === "tour" ? !tour : false); handle.current?.preset(p.id === "tour" && tour ? "overview" : p.id); }}><p.icon size={14} /></button>)}
        </div>
        <label className="twin-explode" title="ระยะห่างระหว่างชั้น"><Grid3x3 size={13} /><input type="range" min={0} max={1.2} step={0.05} value={explode} onChange={(e) => setExplode(Number(e.target.value))} aria-label="ระยะห่างระหว่างชั้น" /></label>
        <button type="button" className={`topo-btn ${isolate ? "is-on" : ""}`} aria-pressed={isolate} onClick={() => setIsolate(!isolate)} title="แสดงเฉพาะชั้นนี้" aria-label="แสดงเฉพาะชั้นนี้"><Camera size={14} /></button>
        <button type="button" className={`topo-btn ${showSwitches ? "is-on" : ""}`} aria-pressed={showSwitches} onClick={() => setShowSwitches(!showSwitches)} title="สวิตช์และปลั๊ก" aria-label="สวิตช์และปลั๊ก"><Power size={14} /></button>
        <button type="button" className={`topo-btn ${showData ? "is-on" : ""}`} aria-pressed={showData} onClick={() => setShowData(!showData)} title="ตารางข้อมูล" aria-label="ตารางข้อมูล"><Table2 size={14} /></button>
        <button type="button" className="topo-btn" onClick={fullscreen} title="เต็มจอ" aria-label="เต็มจอ"><Expand size={14} /></button>
      </header>
      {error && <p className="twin-error" role="alert">{error}</p>}
      <div className="twin-stage">
        {siteId ? (
          <TwinView siteId={siteId} source={source} floorId={floorId} layers={layers} people={people} tour={tour && mode === "live"} explode={explode} isolate={isolate}
            mode={mode} frozen={frozen} autoFocus={mode === "live"}
            refreshKey={refreshKey} pollMs={connected ? 20000 : 6000} handleRef={handle} onReady={onReady} onState={onState} onSelect={setSelected} onError={fail}
            onAlertFocus={(a) => { const z = handle.current?.engine()?.zoneName(a.gateway_id); if (z) setFloorId(z.floorId); setTour(false); }} />
        ) : sites && <div className="twin-empty"><Building2 size={28} /><p>ยังไม่มีอาคาร · วาดผังในหน้า &ldquo;ผังอาคาร&rdquo; ก่อน แล้วกลับมาดูแบบ digital twin</p></div>}
        {urgent.length > 0 && (
          <button type="button" className="twin-sos-chip" onClick={() => { const next = urgent.length > 1 ? (sosIndex + 1) % urgent.length : 0; setSosIndex(next); focus(urgent[next]); }}>
            {urgent.length} เหตุด่วน · ไปที่เหตุ{urgent.length > 1 ? " (N = ถัดไป)" : ""}
          </button>
        )}
        <TwinMinimap site={site} view={view} summaries={summaries} onPick={(f, x, y) => { setFloorId(f); handle.current?.lookAt(f, x, y); }} />
        <div className="twin-legend" aria-label="คำอธิบายสี">
          {heatKind && ramp && (
            <>
              <strong>{heatKind === "temperature" ? "อุณหภูมิเทียบช่วงเหมาะสมของโซน" : heatKind === "humidity" ? "ความชื้น %" : "มีการเคลื่อนไหว"}</strong>
              <div className="twin-ramp" style={{ background: `linear-gradient(90deg, ${ramp.map((c) => `rgb(${c.join(",")})`).join(",")})` }} />
              {heatKind === "temperature" ? (
                <>
                  <div className="twin-ramp-ends"><span>เย็นกว่า</span><span>ในช่วง</span><span>ร้อนกว่า</span></div>
                  <small className="twin-hatch">ทั่วไป 22–25 °C · โซนที่ตั้งช่วงเองใช้ช่วงของโซน (เช่น ห้องเย็น) · สีเต็มเมื่อห่างช่วง 4 °C</small>
                </>
              ) : (
                <div className="twin-ramp-ends">
                  <span>{heatKind === "occupancy" ? "ว่าง" : range[0]}</span>
                  {heatKind === "humidity" && <button type="button" onClick={lockLegend} aria-pressed={locked} title={locked ? "ปลดล็อกช่วงสี" : "ล็อกช่วงสีไว้"} aria-label={locked ? "ปลดล็อกช่วงสี" : "ล็อกช่วงสีไว้"}>{locked ? <Lock size={12} /> : <LockOpen size={12} />}</button>}
                  <span>{heatKind === "occupancy" ? "มีคน" : range[1]}</span>
                </div>
              )}
              <small className="twin-hatch">ลายขีด = ไม่มีเซ็นเซอร์ในโซน</small>
            </>
          )}
          {layers.includes("people") && <small className="twin-honest">ตำแหน่งระดับโซนจาก RSSI · ไม่ใช่พิกัดจริง</small>}
        </div>
        {showSwitches && mode === "live" && (
          <nav className="twin-switchboard" aria-label="อุปกรณ์ที่สั่งงานได้">
            <strong>สวิตช์ ไฟ ปลั๊ก ม่าน แอร์</strong>
            {controllable.length === 0 ? <small>อาคารนี้ยังไม่มีอุปกรณ์ที่สั่งงานได้</small> : <ul>{controllable.map((d) => (
              <li key={d.id}><button type="button" aria-current={selected?.id === d.id} className={d.on === 1 ? "is-on" : ""} onClick={() => { setTour(false); setFloorId(d.floor_id); handle.current?.select(d.id); handle.current?.lookAt(d.floor_id, d.x, d.y); }}>
                <span className="twin-lamp" aria-hidden="true" /><span className="twin-switch-name">{d.name}</span><small>{d.on === 1 ? "เปิด" : d.on === 0 ? "ปิด" : ""}{d.power !== undefined ? ` · ${d.power.toFixed(0)} W` : ""}</small>
              </button></li>
            ))}</ul>}
          </nav>
        )}
        {selected && (
          <aside className="twin-card" aria-label="อุปกรณ์ที่เลือก">
            <strong>{selected.name}</strong>
            <small>{selected.kind === "gateway" ? "Gateway" : selected.profile}</small>
            <dl>
              {selected.t !== undefined && <><dt>อุณหภูมิ</dt><dd>{selected.t.toFixed(1)} °C</dd></>}
              {selected.h !== undefined && <><dt>ความชื้น</dt><dd>{selected.h.toFixed(0)} %</dd></>}
              {selected.battery !== undefined && <><dt>แบตเตอรี่</dt><dd>{selected.battery.toFixed(0)} %</dd></>}
              {selected.door !== undefined && <><dt>ประตู</dt><dd>{selected.door ? "เปิด" : "ปิด"}</dd></>}
              <dt>สถานะ</dt><dd>{selected.alert ? "มีการแจ้งเตือน" : selected.online ? "ออนไลน์" : "ขาดการติดต่อ"}</dd>
              {selected.power !== undefined && <><dt>กำลังไฟ</dt><dd>{selected.power.toFixed(1)} W</dd></>}
              <dt>ล่าสุด</dt><dd>{selected.last_at ? new Date(selected.last_at).toLocaleTimeString("th-TH") : "—"}</dd>
            </dl>
            {selected.class && selected.kind === "device" && mode === "live" && <DeviceControlsPanel variant="compact" client={client} deviceId={selected.id} refreshKey={selected.last_at} onSettled={() => setRefreshKey((k) => k + 1)} />}
          </aside>
        )}
        {debug && stats && <div className="twin-debug" aria-hidden="true">{stats.fps} fps · {stats.calls} draw · {Math.round(stats.triangles / 1000)}k tri · {stats.tier} · tex {stats.textures}</div>}
      </div>
      {mode === "replay" && win && (
        <ReplayTimeline timeline={timeline} from={win.from} to={win.to} t={t} playing={playing} speed={speed} range={replayRange} busy={replayBusy}
          onSeek={(v) => setT(v)} onPlay={(v) => { if (v && t >= win.to) setT(win.from); setPlaying(v); }} onSpeed={setSpeed}
          onRange={(r) => { setPlaying(false); enterReplay(r); }} onMarker={onMarker} onLive={() => { setMode("live"); setPlaying(false); }} />
      )}
      <div className="twin-sr" aria-live="polite">{spoken}</div>
      {showData && (
        <div className="twin-data" role="region" aria-label="ข้อมูลรายโซน">
          <table>
            <caption>ข้อมูลรายโซน · {site?.name}{mode === "replay" ? ` · ณ ${thaiTime(t)}` : ""}</caption>
            <thead><tr><th scope="col">โซน</th><th scope="col">ชั้น</th><th scope="col">อุณหภูมิ</th><th scope="col">ช่วงเหมาะสม</th><th scope="col">ความชื้น</th><th scope="col">คน</th><th scope="col">เหตุเปิดอยู่</th></tr></thead>
            <tbody>
              {summaries.map((z) => (
                <tr key={`${z.floorId}:${z.index}`} className={z.sos ? "is-sos" : ""}>
                  <th scope="row">{z.name}</th><td>{z.floorName}</td>
                  <td>{z.t !== null ? `${z.t.toFixed(1)} °C` : "ไม่มีเซ็นเซอร์"}</td>
                  <td>{`${z.band[0]}–${z.band[1]} °C`}</td>
                  <td>{z.h !== null ? `${z.h.toFixed(0)} %` : "—"}</td>
                  <td>{layers.includes("people") ? z.people : "—"}</td>
                  <td>{z.alerts ? `${z.alerts}${z.sos ? " · SOS" : ""}` : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
