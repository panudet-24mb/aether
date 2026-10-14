"use client";
// TwinView: the embeddable digital twin (docs/platform/digital-twin.md). The page (twin.tsx) wraps it with a
// toolbar; the TV display (frontend/app/display) can embed it read-only with its own data source. Keep this
// contract stable: siteId, floorId, mode, layers, people, quality, tour, paused, onReady, onAlertFocus. Everything
// added since (style, controls, the building model) is optional and off by default, so the display is unchanged.

import { useEffect, useImperativeHandle, useMemo, useRef, useState } from "react";
import { draftOf, type Site } from "../floorplan/model";
import { useLatest } from "../topology/use-latest";
import { type Layer, type PeopleMode, type TwinAlert, type TwinDevice, type TwinState } from "./api";
import { TwinEngine, type BadgeSpec, type Preset, type Quality, type TwinStyle } from "./engine/engine";
import { disposeTree, type Floor3D } from "./engine/building";
import { parseModel } from "./engine/model";
import { cachedModel, primeModelCache, type ModelTransform, type SiteModel } from "./model-api";
import type { QuickResult } from "./quick-toggle";
import { TwinPlan2D, type ZoneSummary, zoneSummaries } from "./twin-plan";

export type TwinSource = {
  site: (siteId: string) => Promise<Site>;
  state: (siteId: string, people: PeopleMode) => Promise<TwinState>;
  /** The building's 3D model (optional: a source without it, like the TV display's, draws the generated building only). */
  model?: {
    meta: (siteId: string) => Promise<SiteModel | null>;
    /** null when that revision is no longer current (the record is then re-read); `signal` aborts the download. */
    file: (siteId: string, revision: number, signal?: AbortSignal) => Promise<ArrayBuffer | null>;
  };
};

/** In-scene buttons (immersive): the pending command per device and what a power button does. */
export type TwinControls = {
  pending: Record<string, QuickResult>;
  onToggle: (d: TwinDevice) => void;
  /** Replay, or a viewer: buttons show but do nothing. */
  disabled?: boolean;
};

export type ModelStatus = "none" | "loading" | "ready" | "error";

export type TwinViewHandle = {
  preset: (p: Preset) => void;
  focusAlert: (a: TwinAlert) => void;
  lookAt: (floorId: string, x: number, y: number) => void;
  planView: () => { floorId: string; x: number; y: number; heading: number } | null;
  lockRange: (kind: "temperature" | "humidity" | "occupancy", r: [number, number] | null) => void;
  range: (kind: "temperature" | "humidity" | "occupancy") => [number, number];
  engine: () => TwinEngine | null;
  /** Selects a device as a click on its marker would (keyboard access to the side card). */
  select: (id: string | null) => void;
  /** Scale and offset that fit the loaded model to the plans (null without a model). */
  fitModel: (t: ModelTransform) => Pick<ModelTransform, "scale" | "offset_m"> | null;
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
  /** classic (default): the original look; immersive: beacons, a holographic floor and in-scene badges. */
  style?: TwinStyle;
  /** Immersive only: slow ambient motion (off with reduced motion, a hidden tab or a paused view). */
  ambient?: boolean;
  /** Immersive only: power buttons on the badges of controllable devices. */
  controls?: TwinControls;
  /** Changing it refetches the building model's record (after an upload, a save or a delete). */
  modelKey?: number;
  /** A transform to preview instead of the saved one (the alignment panel). */
  modelTransform?: ModelTransform | null;
  onModel?: (m: SiteModel | null, status: ModelStatus) => void;
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

/** The value a badge shows: on/off and watts for outputs, the door, temperature and humidity. */
function badgeValue(d: TwinDevice): string {
  if (d.class && d.on !== undefined) return `${d.on === 1 ? "เปิด" : "ปิด"}${d.power !== undefined ? ` · ${d.power.toFixed(0)} W` : ""}`;
  if (d.door !== undefined) return d.door === 1 ? "ประตูเปิด" : "ประตูปิด";
  if (d.t !== undefined) return `${d.t.toFixed(1)} °C${d.h !== undefined ? ` · ${d.h.toFixed(0)} %` : ""}`;
  if (d.h !== undefined) return `${d.h.toFixed(0)} %`;
  if (d.power !== undefined) return `${d.power.toFixed(0)} W`;
  return "";
}

function badgeTone(d: TwinDevice, now: number): { tone: BadgeSpec["tone"]; status: string } {
  if (d.sos || d.alert) return { tone: "alert", status: "มีการแจ้งเตือน" };
  if (!d.online) return d.last_at && now - Date.parse(d.last_at) < 15 * 60000 ? { tone: "warn", status: "ข้อมูลเก่า" } : { tone: "off", status: "ขาดการติดต่อ" };
  if (d.door === 1) return { tone: "warn", status: "ประตูเปิด" };
  return { tone: d.on === 0 ? "idle" : "ok", status: "ออนไลน์" };
}

export default function TwinView(props: TwinViewProps) {
  const { siteId, source, layers, people, quality = "auto", tour = false, paused = false, explode = 0.55, isolate = false, refreshKey = 0, pollMs = 20000, large = false, style = "classic", ambient = false, controls } = props;
  const host = useRef<HTMLDivElement | null>(null);
  const engineRef = useRef<TwinEngine | null>(null);
  const cb = useLatest({ onReady: props.onReady, onAlertFocus: props.onAlertFocus, onState: props.onState, onSelect: props.onSelect, onError: props.onError, onModel: props.onModel, onToggle: controls?.onToggle });
  const qualityRef = useLatest(quality);
  const styleRef = useLatest(style);
  // Bumped when an engine is created, so the model and style are applied to a new WebGL context too.
  const [engineGen, setEngineGen] = useState(0);
  const [site, setSite] = useState<Site | null>(null);
  const [state, setState] = useState<TwinState | null>(null);
  const [fallback, setFallback] = useState(() => typeof document !== "undefined" && !webglAvailable());
  const [announce, setAnnounce] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const [modelRec, setModelRec] = useState<{ site: string; key: number; model: SiteModel | null } | null>(null);
  const [modelLoad, setModelLoad] = useState<{ key: string; status: ModelStatus } | null>(null);
  const stateRef = useLatest(state);
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
      engine = new TwinEngine(el, { quality: qualityRef.current, reducedMotion: reducedMotion(), style: styleRef.current });
    } catch {
      const id = requestAnimationFrame(() => setFallback(true));
      return () => cancelAnimationFrame(id);
    }
    engineRef.current = engine;
    engine.setPickHandler((p) => setSelected(p ? p.id : null));
    engine.setBadgeHandler((id, action) => {
      if (action === "select") { setSelected(id); return; }
      const d = stateRef.current?.devices.find((x) => x.id === id);
      if (d) cb.current.onToggle?.(d);
    });
    const gen = requestAnimationFrame(() => setEngineGen((g) => g + 1));
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
      cancelAnimationFrame(gen);
      clearInterval(watch);
      mq?.removeEventListener("change", onMotion);
      engine.dispose();
      engineRef.current = null;
      if (debug) delete (window as unknown as { __twin?: unknown }).__twin;
    };
    // A quality change is applied below without recreating the context.
  }, [fallback, qualityRef, styleRef, stateRef, cb]);

  useEffect(() => { engineRef.current?.setQuality(quality); }, [quality]);
  useEffect(() => { engineRef.current?.setStyle(style); }, [style, engineGen]);
  useEffect(() => {
    engineRef.current?.setAmbient(ambient);
    engineRef.current?.setPaused(paused);
  }, [ambient, paused, engineGen]);

  // The building model: its record (on site change and on modelKey), then its GLB, parsed once per revision.
  const modelSource = source.model;
  const modelKey = props.modelKey ?? 0;
  const modelKeyRef = useLatest(modelKey);
  const modelRecRef = useLatest(modelRec);
  useEffect(() => {
    if (!siteId || !modelSource) return;
    let active = true;
    modelSource.meta(siteId).then(
      (m) => { if (active) setModelRec({ site: siteId, key: modelKey, model: m }); },
      () => {
        if (!active) return;
        // A failed re-read keeps the record (and the loaded model) of this site; only a first read that fails says so.
        if (modelRecRef.current?.site === siteId) return;
        setModelRec({ site: siteId, key: modelKey, model: null });
        setModelLoad({ key: `${siteId}:meta`, status: "error" });
      },
    );
    return () => { active = false; };
  }, [siteId, modelSource, modelKey, modelRecRef]);
  const model = modelSource && modelRec && modelRec.site === siteId ? modelRec.model : null;
  const modelId = model ? `${siteId}:${model.revision}` : "";
  const modelRevision = model?.revision ?? 0;

  useEffect(() => {
    const e = engineRef.current;
    if (!e || !modelSource) return;
    if (!modelId) { if (e.modelKey()) e.setModel(null); return; }
    if (e.modelKey() === modelId) return;
    let active = true;
    // A site change or unmount stops the download, not only its result.
    const abort = new AbortController();
    (async () => {
      // Status updates only after the first await: never synchronously in the effect, and always before "ready".
      await Promise.resolve();
      if (!active) return;
      setModelLoad({ key: modelId, status: "loading" });
      const bytes = cachedModel(siteId, modelRevision) ?? (await modelSource.file(siteId, modelRevision, abort.signal));
      if (!active) return;
      if (!bytes) {
        // 404 for this revision: the model was replaced or deleted meanwhile. Re-read the record; a new revision
        // (or none) re-runs this effect. The same revision again means the file is really missing.
        const fresh = await modelSource.meta(siteId);
        if (!active) return;
        if (fresh?.revision === modelRevision) throw new Error("model file missing");
        setModelRec({ site: siteId, key: modelKeyRef.current, model: fresh });
        return;
      }
      primeModelCache(siteId, modelRevision, bytes);
      const flat = await parseModel(bytes);
      if (!active || engineRef.current !== e) { disposeTree(flat.root); return; }
      e.setModel(flat, modelId);
      setModelLoad({ key: modelId, status: "ready" });
    })().catch(() => {
      // Never blocks the live view: the generated building stays and a short notice says why the model is missing.
      if (active) setModelLoad({ key: modelId, status: "error" });
    });
    return () => { active = false; abort.abort(); };
  }, [modelId, modelRevision, siteId, modelSource, engineGen, modelKeyRef]);

  const transform = props.modelTransform ?? model?.transform ?? null;
  const transformKey = transform ? JSON.stringify(transform) : "";
  useEffect(() => {
    engineRef.current?.setModelTransform(transformKey ? (JSON.parse(transformKey) as ModelTransform) : null);
  }, [transformKey, engineGen]);

  const modelStatus: ModelStatus = !model ? (modelLoad?.key === `${siteId}:meta` ? "error" : "none") : modelLoad?.key === modelId ? modelLoad.status : "loading";
  useEffect(() => { cb.current.onModel?.(model, modelStatus); }, [model, modelStatus, cb]);

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

  // Badges (immersive): devices of the active floor with their key value, alerts anywhere, the selected device.
  const pending = controls?.pending;
  const controlsOff = !!controls?.disabled;
  const canToggle = !!controls;
  const badges = useMemo<BadgeSpec[]>(() => {
    if (style !== "immersive" || !state) return [];
    // The server's clock: badges age against the time the state was read, never the render.
    const now = Date.parse(state.server_time) || 0;
    const list: BadgeSpec[] = [];
    for (const d of state.devices) {
      const always = d.alert || d.sos;
      const isSel = d.id === selected;
      if (d.floor_id !== floorId && !always && !isSel) continue;
      if (d.kind === "gateway" && !always && !isSel) continue;
      const controllable = d.kind === "device" && !!d.class;
      const value = badgeValue(d);
      const { tone, status } = badgeTone(d, now);
      const p = pending?.[d.id];
      list.push({
        id: d.id, floorId: d.floor_id, x: d.x, y: d.y, h: d.z + 0.42, name: d.name, value, status, tone,
        priority: (always ? 100 : 0) + (isSel ? 50 : 0) + (controllable ? 20 : 0) + (value ? 5 : 0) + (tone === "warn" ? 3 : 0),
        always, selected: isSel,
        toggle: controllable && canToggle ? { on: d.on === undefined ? null : d.on === 1, state: p?.status ?? "idle", disabled: controlsOff || !d.online } : undefined,
        message: p?.status === "failed" ? p.message : undefined,
      });
    }
    return list.sort((a, b) => b.priority - a.priority).slice(0, 24);
  }, [style, state, floorId, selected, pending, controlsOff, canToggle]);
  useEffect(() => { engineRef.current?.setBadges(badges); }, [badges, engineGen]);

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
      // Immersive: devices carry badges instead (the badge list below).
      if (style === "immersive") continue;
      const v = d.t !== undefined ? ` · ${d.t.toFixed(1)} °C` : "";
      out.push({ id: `d:${d.id}`, floorId: d.floor_id, x: d.x, y: d.y, h: d.z + 0.55, text: `${d.name}${v}${d.last_at ? ` · ${since(d.last_at, now)}ที่แล้ว` : ""}`, tone: d.alert || d.sos ? "sos" : d.online ? undefined : "warn" });
    }
    for (const p of state.presence.people ?? []) {
      if (!p.sos || !p.name) continue;
      const zone = p.gateway_id ? e.zoneName(p.gateway_id) : null;
      const at = zone ? summaries.find((z) => z.floorId === zone.floorId && z.name === zone.name) : null;
      if (at) out.push({ id: `p:${p.pid}`, floorId: at.floorId, x: at.x, y: at.y, h: 2.2, text: `SOS · ${p.name}`, tone: "sos" });
    }
    if (style === "immersive") {
      // Labels and badges share one budget of 40 projected DOM nodes; an SOS label is never the one left out.
      out.sort((a, b) => Number(b.tone === "sos") - Number(a.tone === "sos"));
      e.setLabels(out.slice(0, 40 - badges.length));
    } else e.setLabels(out.slice(0, 40));
  }, [state, site, floorId, layers, selected, style, badges.length]);

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
      select: (id) => setSelected(id),
      fitModel: (t) => engineRef.current?.fitModel(t) ?? null,
  }), [floorId]);

  const summaries: ZoneSummary[] = useMemo(() => (site && state ? zoneSummaries(site, state, layers) : []), [site, state, layers]);

  return (
    <div className={`twin-view ${large ? "is-large" : ""} ${style === "immersive" && !fallback ? "is-immersive" : ""}`}>
      {fallback ? (
        <TwinPlan2D site={site} floorId={floorId} summaries={summaries} state={state} layers={layers} />
      ) : (
        <div ref={host} className="twin-canvas" role="application" aria-label="Digital twin แบบ 3 มิติ · ใช้แท็บ ข้อมูล เพื่อดูตัวเลขเป็นตาราง" />
      )}
      {modelStatus === "error" && !fallback && <p className="twin-model-note" role="status">แสดงโมเดลอาคาร 3D ไม่ได้ · ภาพสดยังทำงานตามปกติ</p>}
      <div className="twin-sr" role="alert" aria-live="assertive">{announce}</div>
    </div>
  );
}
