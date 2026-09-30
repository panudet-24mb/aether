"use client";
// TwinView: the embeddable digital twin (docs/platform/digital-twin.md). The page (twin.tsx) wraps it with a
// toolbar; the TV display (frontend/app/display) can embed it read-only with its own data source. Keep this
// contract stable: siteId, floorId, mode, layers, people, quality, tour, paused, onReady, onAlertFocus.

import { useEffect, useImperativeHandle, useMemo, useRef, useState } from "react";
import { draftOf, type Site } from "../floorplan/model";
import { useLatest } from "../topology/use-latest";
import type { Layer, PeopleMode, TwinAlert, TwinDevice, TwinState } from "./api";
import { TwinEngine, type Preset, type Quality } from "./engine/engine";
import type { Floor3D } from "./engine/building";
import { TwinPlan2D, type ZoneSummary, zoneSummaries } from "./twin-plan";

export type TwinSource = {
  site: (siteId: string) => Promise<Site>;
  state: (siteId: string, people: PeopleMode) => Promise<TwinState>;
};

export type TwinViewHandle = {
  preset: (p: Preset) => void;
  focusAlert: (a: TwinAlert) => void;
  lookAt: (floorId: string, x: number, y: number) => void;
  planView: () => { floorId: string; x: number; y: number; heading: number } | null;
  lockRange: (kind: "temperature" | "humidity" | "occupancy", r: [number, number] | null) => void;
  range: (kind: "temperature" | "humidity" | "occupancy") => [number, number];
  engine: () => TwinEngine | null;
};

export type TwinViewProps = {
  siteId: string;
  source: TwinSource;
  floorId?: string;
  /** live polls the state; replay draws `frozen` (the state at the replay's time) and never polls. */
  mode?: "live" | "replay";
  /** The state to draw instead of polling (replay: stateAt the scrubbed time). */
  frozen?: TwinState | null;
  /** Fly to a new SOS on its own (live); replay flies only when asked (a marker). */
  autoFocus?: boolean;
  layers: Layer[];
  people: PeopleMode;
  quality?: Quality | "auto";
  /** Slow orbit (wall screens): an SOS interrupts it until acknowledged. */
  tour?: boolean;
  /** Stops polling and the tour without tearing the scene down (a TV playlist keeps the view mounted). */
  paused?: boolean;
  explode?: number;
  isolate?: boolean;
  /** Changing it refetches the state now (a signal arrived). */
  refreshKey?: number;
  /** Safety-net poll interval; the page passes a slow one while the realtime socket is connected. */
  pollMs?: number;
  /** Larger labels for 10-foot screens. */
  large?: boolean;
  onReady?: (site: Site) => void;
  onAlertFocus?: (a: TwinAlert, zone: string | null) => void;
  onState?: (s: TwinState) => void;
  onSelect?: (d: TwinDevice | null) => void;
  onError?: (e: unknown) => void;
  handleRef?: React.Ref<TwinViewHandle>;
};

function webglAvailable(): boolean {
  try {
    const c = document.createElement("canvas");
    const gl = (c.getContext("webgl2") || c.getContext("webgl")) as WebGLRenderingContext | null;
    // Give the probe's context back at once: browsers cap live WebGL contexts.
    gl?.getExtension("WEBGL_lose_context")?.loseContext();
    return !!gl;
  } catch {
    return false;
  }
}

function reducedMotion(): boolean {
  try { return window.matchMedia("(prefers-reduced-motion: reduce)").matches; } catch { return false; }
}

const since = (at: string, now: number) => {
  const s = Math.max(0, Math.round((now - Date.parse(at)) / 1000));
  return s < 60 ? `${s} วินาที` : s < 3600 ? `${Math.floor(s / 60)} นาที` : `${Math.floor(s / 3600)} ชม.`;
};

export default function TwinView(props: TwinViewProps) {
  const { siteId, source, layers, people, quality = "auto", tour = false, paused = false, explode = 0.55, isolate = false, refreshKey = 0, pollMs = 20000, large = false } = props;
  const host = useRef<HTMLDivElement | null>(null);
  const engineRef = useRef<TwinEngine | null>(null);
  const cb = useLatest({ onReady: props.onReady, onAlertFocus: props.onAlertFocus, onState: props.onState, onSelect: props.onSelect, onError: props.onError });
  const qualityRef = useLatest(quality);
  const [site, setSite] = useState<Site | null>(null);
  const [state, setState] = useState<TwinState | null>(null);
  const [fallback, setFallback] = useState(() => typeof document !== "undefined" && !webglAvailable());
  const [announce, setAnnounce] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const seenAlerts = useRef<Set<string> | null>(null);
  const floorId = props.floorId ?? site?.floors[0]?.id ?? "";

  const floors: Floor3D[] = useMemo(() => (site ? site.floors.map((f) => ({ id: f.id, draft: draftOf(f) })) : []), [site]);

  // The site's drawing: once per site, and again only when the live state reports a floor revision it does not have.
  const drawn = site && state && site.id === state.site_id
    ? Object.keys(state.layout_revision).length === site.floors.length && site.floors.every((f) => state.layout_revision[f.id] === f.revision)
    : true;
  const revisions = drawn ? "" : JSON.stringify(state?.layout_revision ?? {});
  useEffect(() => {
    if (!siteId) return;
    let active = true;
    source.site(siteId).then((s) => { if (active) { setSite(s); cb.current.onReady?.(s); } }).catch((e) => { if (active) cb.current.onError?.(e); });
    return () => { active = false; };
  }, [siteId, source, cb, revisions]);

  // Replay: the page hands the state at the scrubbed time.
  const replaying = props.mode === "replay";
  const frozen = replaying ? props.frozen ?? null : null;
  useEffect(() => {
    if (!frozen) return;
    const id = requestAnimationFrame(() => { setState(frozen); cb.current.onState?.(frozen); });
    return () => cancelAnimationFrame(id);
  }, [frozen, cb]);

  // Live state: now, on every refreshKey, and on the safety-net poll.
  useEffect(() => {
    if (!siteId || paused || replaying) return;
    let active = true;
    const load = () => source.state(siteId, people).then((s) => { if (active) { setState(s); cb.current.onState?.(s); } }).catch((e) => { if (active) cb.current.onError?.(e); });
    void load();
    const t = setInterval(load, Math.max(3000, pollMs));
    return () => { active = false; clearInterval(t); };
  }, [siteId, source, people, paused, pollMs, refreshKey, cb, replaying]);

  // The engine: created once per mount (WebGL contexts are capped: never one per render).
  useEffect(() => {
    const el = host.current;
    if (!el || fallback) return;
    let engine: TwinEngine;
    try {
      engine = new TwinEngine(el, { quality: qualityRef.current, reducedMotion: reducedMotion() });
    } catch {
      const id = requestAnimationFrame(() => setFallback(true));
      return () => cancelAnimationFrame(id);
    }
    engineRef.current = engine;
    engine.setPickHandler((p) => setSelected(p ? p.id : null));
    let slow = 0;
    // Stay on the 2D plan when even the low tier cannot keep 20 fps.
    const watch = setInterval(() => {
      const s = engine.stats();
      slow = s.tier === "low" && s.fps > 0 && s.fps < 20 ? slow + 1 : 0;
      if (slow >= 3) setFallback(true);
    }, 5000);
    const debug = typeof location !== "undefined" && new URLSearchParams(location.search).has("twinDebug");
    if (debug) {
      (window as unknown as { __twin?: unknown }).__twin = {
        engine,
        stats: () => engine.stats(),
        target: () => engine.cameraTarget().toArray(),
        planView: () => engine.planView(),
      };
    }
    const mq = typeof window !== "undefined" ? window.matchMedia("(prefers-reduced-motion: reduce)") : null;
    const onMotion = () => engine.setReducedMotion(!!mq?.matches);
    mq?.addEventListener("change", onMotion);
    return () => {
      clearInterval(watch);
      mq?.removeEventListener("change", onMotion);
      engine.dispose();
      engineRef.current = null;
      if (debug) delete (window as unknown as { __twin?: unknown }).__twin;
    };
    // A quality change is applied below without recreating the context.
  }, [fallback, qualityRef]);

  useEffect(() => { engineRef.current?.setQuality(quality); }, [quality]);

  useEffect(() => {
    const e = engineRef.current;
    if (!e || !floors.length) return;
    e.setBuilding(floors, floorId, explode, isolate);
  }, [floors, floorId, explode, isolate, fallback]);

  const layerKey = layers.join(",");
  useEffect(() => {
    const e = engineRef.current;
    if (!e || !state || !floors.length) return;
    e.setState(state, layerKey ? (layerKey.split(",") as Layer[]) : []);
  }, [state, layerKey, floors, floorId, explode, isolate, fallback]);

  // Tour on wall screens; paused screens hold still.
  const tourRef = useRef(false);
  useEffect(() => {
    const e = engineRef.current;
    if (!e || !floors.length) return;
    const want = tour && !paused;
    if (want !== tourRef.current) { tourRef.current = want; e.preset(want ? "tour" : "overview", floorId); }
  }, [tour, paused, floors, floorId]);

  // Frame the building when a site opens (not when the floor changes: the operator's camera stays).
  const floorRef = useLatest(floorId);
  useEffect(() => {
    const e = engineRef.current;
    if (!e || !floors.length) return;
    e.preset(tourRef.current ? "tour" : "overview", floorRef.current);
  }, [siteId, floors.length, fallback, floorRef]);

  // A new SOS or hazard: announce it and fly there.
  useEffect(() => {
    if (!state) return;
    const urgent = state.alerts.filter((a) => (a.sos || a.hazard) && a.status === "open");
    const known = seenAlerts.current ?? new Set<string>();
    const fresh = urgent.filter((a) => !known.has(a.id));
    seenAlerts.current = new Set(urgent.map((a) => a.id));
    if (!fresh.length || props.autoFocus === false) return;
    const a = fresh[0];
    const e = engineRef.current;
    const zone = e?.zoneName(a.gateway_id) ?? null;
    const floorName = zone ? site?.floors.find((f) => f.id === zone.floorId)?.name : undefined;
    const text = `${a.sos ? "SOS" : "อันตราย"} · ${zone?.name ?? "ไม่ทราบโซน"}${floorName ? ` · ${floorName}` : ""} · ${new Date(a.opened_at).toLocaleTimeString("th-TH")}`;
    const id = requestAnimationFrame(() => setAnnounce(text));
    e?.focusAlert(a);
    cb.current.onAlertFocus?.(a, zone?.name ?? null);
    return () => cancelAnimationFrame(id);
  }, [state, site, cb, props.autoFocus]);

  // Labels: zone names with headcounts on the active floor, alerting devices, the selected device.
  useEffect(() => {
    const e = engineRef.current;
    if (!e || !state || !site) return;
    const now = Date.parse(state.server_time);
    const summaries = zoneSummaries(site, state, layers);
    const out: Parameters<TwinEngine["setLabels"]>[0] = [];
    for (const z of summaries) {
      if (z.floorId !== floorId && !z.sos) continue;
      const bits = [z.name];
      if (layers.includes("temperature") && z.t !== null) bits.push(`${z.t.toFixed(1)} °C`);
      if (layers.includes("humidity") && z.h !== null) bits.push(`${z.h.toFixed(0)} %`);
      if (layers.includes("people") && z.people > 0) bits.push(`${z.people} คน`);
      out.push({ id: `z:${z.floorId}:${z.index}`, floorId: z.floorId, x: z.x, y: z.y, h: 1.1, text: z.sos ? `SOS · ${z.name}` : bits.join(" · "), tone: z.sos ? "sos" : z.noSensor && layers.some((l) => l !== "people") ? "muted" : "zone" });
    }
    for (const d of state.devices) {
      if (!(d.alert || d.sos || d.id === selected)) continue;
      const v = d.t !== undefined ? ` · ${d.t.toFixed(1)} °C` : "";
      out.push({ id: `d:${d.id}`, floorId: d.floor_id, x: d.x, y: d.y, h: d.z + 0.55, text: `${d.name}${v}${d.last_at ? ` · ${since(d.last_at, now)}ที่แล้ว` : ""}`, tone: d.alert || d.sos ? "sos" : d.online ? undefined : "warn" });
    }
    for (const p of state.presence.people ?? []) {
      if (!p.sos || !p.name) continue;
      const zone = p.gateway_id ? e.zoneName(p.gateway_id) : null;
      const at = zone ? summaries.find((z) => z.floorId === zone.floorId && z.name === zone.name) : null;
      if (at) out.push({ id: `p:${p.pid}`, floorId: at.floorId, x: at.x, y: at.y, h: 2.2, text: `SOS · ${p.name}`, tone: "sos" });
    }
    e.setLabels(out.slice(0, 40));
  }, [state, site, floorId, layers, selected]);

  useEffect(() => { cb.current.onSelect?.(state?.devices.find((d) => d.id === selected) ?? null); }, [selected, state, cb]);

  // The handle the page (and a TV playlist) drives.
  useImperativeHandle(props.handleRef, () => ({
      preset: (p) => engineRef.current?.preset(p, floorId),
      focusAlert: (a) => engineRef.current?.focusAlert(a),
      lookAt: (f, x, y) => engineRef.current?.lookAt(f, x, y),
      planView: () => engineRef.current?.planView() ?? null,
      lockRange: (k, r) => engineRef.current?.lockRange(k, r),
      range: (k) => engineRef.current?.range(k) ?? [0, 1],
      engine: () => engineRef.current,
  }), [floorId]);

  const summaries: ZoneSummary[] = useMemo(() => (site && state ? zoneSummaries(site, state, layers) : []), [site, state, layers]);

  return (
    <div className={`twin-view ${large ? "is-large" : ""}`}>
      {fallback ? (
        <TwinPlan2D site={site} floorId={floorId} summaries={summaries} state={state} layers={layers} />
      ) : (
        <div ref={host} className="twin-canvas" role="application" aria-label="Digital twin แบบ 3 มิติ · ใช้แท็บ ข้อมูล เพื่อดูตัวเลขเป็นตาราง" />
      )}
      <div className="twin-sr" role="alert" aria-live="assertive">{announce}</div>
    </div>
  );
}
