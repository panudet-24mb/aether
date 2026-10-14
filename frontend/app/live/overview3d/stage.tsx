"use client";
// The overview's 3D mode (docs/platform/overview-3d.md): the whole page as one scene. The board's own cards, filters
// included, form a constellation of gateway hubs with their devices in rings; the KPIs, the SOS banner, the side
// lists and the device detail are panels in the same scene (the page renders their content, the stage places
// it). One small HUD holds the filters and the camera. Loaded lazily, so the card mode never pays for three.js.

import { useEffect, useImperativeHandle, useMemo, useRef, useState, useSyncExternalStore, type CSSProperties, type KeyboardEvent as ReactKeyboardEvent, type ReactNode, type RefObject } from "react";
import { createPortal } from "react-dom";
import { ChevronUp, Crosshair, Info, LayoutGrid, List as ListIcon, Maximize, Minimize, Orbit, Pause, ScanEye, SlidersHorizontal, View, X } from "lucide-react";
import * as THREE from "three";
import type { CommandClient } from "../../topology/device-controls";
import { useLatest } from "../../topology/use-latest";
import { ConstellationEngine, PanelRegistry, type PanelKind, type PanelPlace, type Palette, type Preset } from "./engine";
import { buildHubs, COMPACT_W, layoutConstellation, newSos, type ListSlot } from "./layout";
import { useOutputs } from "./outputs";
import { PowerStore, PowerToggle } from "./power";
import { syntheticSite } from "./synthetic";
import { controllable, poweredOn, type HubPlace, type NodePlace, type StageCard, type StageGateway, type StageProject } from "./types";
import "./stage.css";

export type StageListKey = "alerts" | "wearables" | "gateways" | "events";

/** What the page's own lists can ask of the stage (a gateway row flies to its hub). */
export type StageApi = { focusHub: (id: string) => void };

export type StageProps = {
  api?: RefObject<StageApi | null>;
  /** The page's filter controls (project, show, kind, search), shown in the HUD. */
  filters: ReactNode;
  /** Live / polling, shadow mode and load errors, shown in the HUD head. */
  status: ReactNode;
  /** The six KPI tiles (the page's own buttons and actions), in one row under the HUD (3×2 on a phone). */
  kpis: { key: string; node: ReactNode }[];
  /** The SOS banner and the device it hangs above. */
  sos: { key: string; node: ReactNode } | null;
  /** The side lists (ต้องดูตอนนี้, ใครอยู่ที่ไหน, Gateway, เหตุการณ์ล่าสุด), in two columns; a null node is skipped. */
  panels: { key: StageListKey; node: ReactNode }[];
  /** The open device's detail, shown beside it (replaces the card page's drawer). */
  detail: { key: string; title: string; subtitle: string; sos: boolean; body: ReactNode } | null;
  onCloseDetail: () => void;
  cards: StageCard[];
  gateways: StageGateway[];
  projects: StageProject[];
  /** Project colour name → hex (PROJECT_COLORS). */
  colors: Record<string, string>;
  /** Draw in-scope gateways without a visible device too (no kind/search/attention filter is narrowing the view). */
  showEmptyHubs: boolean;
  client: CommandClient;
  kindLabel: Record<string, string>;
  /** The board's own value rendering for a card. A method signature on purpose: the overview passes its value()
   *  typed on its full Card, which the stage only ever calls with the cards it was given. */
  renderValue(card: StageCard): ReactNode;
  onOpen: (key: string) => void;
  /** WebGL is unavailable (or was lost): the overview falls back to the cards with this Thai notice. */
  onUnavailable: (notice: string) => void;
  onShowCards: () => void;
};

const STATUS_TEXT: Record<StageCard["status"], string> = { alert: "ต้องตรวจสอบ", online: "ปกติ", stale: "ข้อมูลเริ่มเก่า", offline: "ขาดการติดต่อ" };
const statusText = (c: StageCard) => (c.sos ? "SOS · กดปุ่มฉุกเฉิน ยังไม่รับทราบ" : STATUS_TEXT[c.status]);
const UNAVAILABLE = "เบราว์เซอร์หรืออุปกรณ์นี้แสดงภาพ 3 มิติไม่ได้ (ไม่รองรับ WebGL) · แสดงแบบการ์ดแทน";

/** Theme tokens → three.js colours. Tokens are oklch, so a 1×1 canvas does the conversion to sRGB. */
function readPalette(): Palette {
  const css = getComputedStyle(document.documentElement);
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = 1;
  const ctx = canvas.getContext("2d", { willReadFrequently: true });
  const color = (name: string, fallback: string) => {
    const value = css.getPropertyValue(name).trim() || fallback;
    if (!ctx) return new THREE.Color(fallback);
    ctx.clearRect(0, 0, 1, 1);
    ctx.fillStyle = fallback;
    ctx.fillStyle = value;
    ctx.fillRect(0, 0, 1, 1);
    const [r, g, b] = ctx.getImageData(0, 0, 1, 1).data;
    return new THREE.Color().setRGB(r / 255, g / 255, b / 255, THREE.SRGBColorSpace);
  };
  return {
    paper: color("--color-paper", "#0c1416"), panel: color("--color-panel", "#121b1d"), ink: color("--color-ink", "#e6f2ee"),
    muted: color("--color-muted", "#93a5a8"), dim: color("--color-dim", "#879a9d"), accent: color("--color-accent", "#a7f3d0"),
    warning: color("--color-warning", "#f6c177"), danger: color("--color-danger", "#ff8f70"), border: color("--color-border", "#2a3537"),
    grid: color("--color-grid", "#25403a"), blue: color("--color-blue", "#80b7ff"),
  };
}

const reducedQuery = "(prefers-reduced-motion: reduce)";
function useReducedMotion(): boolean {
  return useSyncExternalStore(
    (fn) => { const m = window.matchMedia(reducedQuery); m.addEventListener("change", fn); return () => m.removeEventListener("change", fn); },
    () => window.matchMedia(reducedQuery).matches,
    () => false,
  );
}

/** What the scene draws for a layout; the engine rebuilds only when this changes (not on every 5 s clock tick). */
function signature(hubs: HubPlace[], nodes: NodePlace[]): string {
  return JSON.stringify([
    hubs.map((h) => [h.hub.id, h.hub.online, h.hub.color, h.at, h.radius]),
    nodes.map((n) => [n.card.key, n.card.kind, n.card.status, n.card.sos, n.card.occupancy?.occupied, n.card.door?.open, n.card.door?.leftOpen, poweredOn(n.card), n.at]),
  ]);
}

/** The nearest ancestor (or the element) the shell pins on screen: position fixed or sticky. */
function pinnedAncestor(el: Element | null): Element | null {
  for (let e = el; e && e !== document.body; e = e.parentElement) {
    const pos = getComputedStyle(e).position;
    if (pos === "fixed" || pos === "sticky") return e;
  }
  return null;
}

const typing = (el: Element | null) => !!el && (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement || (el as HTMLElement).isContentEditable);

/** An in-scene panel: React renders `children` into its own element (a portal); the engine places, scales and fades it. */
function Panel3D({ registry, id, kind, place, width = 0, label, children }: { registry: PanelRegistry; id: string; kind: PanelKind; place: PanelPlace; width?: number; label?: string; children: ReactNode }) {
  // The element the engine moves around; its kind never changes for a given panel.
  const [el] = useState(() => Object.assign(document.createElement("div"), { className: `ov3d-panel is-${kind}` }));
  const placeKey = JSON.stringify(place);
  useEffect(() => { registry.set(id, { el, place: JSON.parse(placeKey) as PanelPlace, width, kind }); }, [registry, id, el, placeKey, width, kind]);
  useEffect(() => () => registry.remove(id), [registry, id]);
  return createPortal(label ? <div role="region" aria-label={label}>{children}</div> : children, el);
}

/** Side lists: two stacked on the left, two on the right (the engine computes the slots, layout.ts wallSlots). */
const LIST_SLOT: Record<StageListKey, ListSlot> = { alerts: "left-0", wearables: "left-1", gateways: "right-0", events: "right-1" };
const hasFullscreen = () => typeof document !== "undefined" && !!document.fullscreenEnabled;

export default function OverviewStage(props: StageProps) {
  const { client, kindLabel, renderValue, onOpen } = props;
  const reduced = useReducedMotion();
  // Dev-only scale check: `?ov3d-demo=500` swaps in a synthetic site of that many devices.
  const [demo] = useState(() => (import.meta.env.DEV && typeof window !== "undefined" ? Math.min(5000, Number(new URLSearchParams(window.location.search).get("ov3d-demo")) || 0) : 0));
  const site = useMemo(() => (demo ? syntheticSite(demo) : null), [demo]);
  const cards = site ? site.cards : props.cards;
  const gateways = site ? site.gateways : props.gateways;
  const projects = site ? site.projects : props.projects;

  const hubs = useMemo(() => buildHubs(cards, gateways, projects, props.colors, site ? true : props.showEmptyHubs), [cards, gateways, projects, props.colors, props.showEmptyHubs, site]);
  const fresh = useMemo(() => layoutConstellation(hubs, cards), [hubs, cards]);
  const sig = useMemo(() => signature(fresh.hubs, fresh.nodes), [fresh]);
  const [stable, setStable] = useState({ sig, layout: fresh });
  if (stable.sig !== sig) setStable({ sig, layout: fresh });
  const byKey = useMemo(() => new Map(cards.map((c) => [c.key, c])), [cards]);
  // Tuya Wi‑Fi devices count as controllable only once their controls report a switch (shared cache with the board).
  const hasOutput = useOutputs(client, cards.filter((c) => c.kind === "tuya" && controllable(c)).map((c) => c.deviceId!));
  const canControl = useMemo(() => (c: StageCard) => controllable(c) && (c.kind !== "tuya" || hasOutput(c.deviceId!)), [hasOutput]);
  const canControlRef = useLatest(canControl);

  const rootRef = useRef<HTMLDivElement>(null);
  const hostRef = useRef<HTMLDivElement>(null);
  const cssHostRef = useRef<HTMLDivElement>(null);
  const [registry] = useState(() => new PanelRegistry());
  const [hudOpen, setHudOpen] = useState(false);
  const [full, setFull] = useState(false);
  const closeRef = useRef<HTMLButtonElement>(null);
  const tooltipRef = useRef<HTMLDivElement>(null);
  const labelEls = useRef(new Map<string, HTMLElement>());
  const hubEls = useRef(new Map<string, HTMLElement>());
  const engineRef = useRef<ConstellationEngine | null>(null);
  const [labelKeys, setLabelKeys] = useState<string[]>([]);
  const [hovered, setHovered] = useState<string | null>(null);
  const [tour, setTour] = useState(false);
  const [stats, setStats] = useState("");
  const [store] = useState(() => new PowerStore(client));
  const onOpenRef = useLatest(onOpen);
  const onUnavailableRef = useLatest(props.onUnavailable);

  useEffect(() => { store.start(); return () => store.stop(); }, [store]);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    let engine: ConstellationEngine;
    try {
      engine = new ConstellationEngine(host, readPalette(), {
        onPick: (p) => { if (p?.kind === "node") onOpenRef.current(p.key); },
        onHover: setHovered,
        onLabels: setLabelKeys,
        onTourEnd: () => setTour(false),
        controllable: (c) => canControlRef.current(c),
        labelEl: (key) => labelEls.current.get(key),
        hubLabelEl: (id) => hubEls.current.get(id),
        tooltipEl: () => tooltipRef.current,
      }, { quality: "auto", reducedMotion: window.matchMedia(reducedQuery).matches, cssHost: cssHostRef.current });
    } catch {
      onUnavailableRef.current(UNAVAILABLE);
      return;
    }
    const lost = (e: Event) => { e.preventDefault(); onUnavailableRef.current(UNAVAILABLE); };
    engine.renderer.domElement.addEventListener("webglcontextlost", lost);
    engine.bindPanels(registry);
    engineRef.current = engine;
    return () => {
      engine.renderer.domElement.removeEventListener("webglcontextlost", lost);
      engine.dispose();
      engineRef.current = null;
    };
  }, [onOpenRef, onUnavailableRef, canControlRef, registry]);
  useImperativeHandle(props.api, () => ({ focusHub: (id: string) => engineRef.current?.focusHub(id) }), []);

  // The SOS banner scales with the constellation; the wall panels take their slots from the engine.
  const wall = stable.layout.radius + 6;
  // A narrow stage (a phone) gets the compact wall: KPIs 3×2 in the scene, the lists in a HUD drawer.
  const [compact, setCompact] = useState(false);
  const [listsOpen, setListsOpen] = useState(false);
  const hudRef = useRef<HTMLDivElement>(null);
  // What of the stage the viewer cannot see (the shell's fixed bars, the part scrolled off or below the window):
  // the HUD and the panels stay inside the visible part.
  useEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    let raf = 0;
    const measure = () => {
      raf = 0;
      const r = root.getBoundingClientRect();
      // Scrolled off at the top counts as covered, and so does whatever the shell lays over the stage there.
      let cover = Math.max(0, -r.top);
      if (!document.fullscreenElement && r.height > 0) {
        for (let pass = 0; pass < 4; pass++) {
          let more = 0;
          for (const x of [r.left + 40, r.left + r.width / 2, r.right - 40]) {
            for (const el of document.elementsFromPoint(x, r.top + cover + 2)) {
              if (root.contains(el)) break;
              if (el.contains(root)) continue;
              const bar = pinnedAncestor(el);
              if (bar) more = Math.max(more, bar.getBoundingClientRect().bottom - r.top - cover);
            }
          }
          if (more <= 0.5) break;
          cover += more;
        }
      }
      cover = Math.min(Math.max(0, Math.round(cover)), r.height / 2);
      const below = document.fullscreenElement ? 0 : Math.min(Math.max(0, Math.round(r.bottom - window.innerHeight)), r.height / 2);
      root.style.setProperty("--ov3d-safe-top", `${cover}px`);
      root.style.setProperty("--ov3d-safe-bottom", `${below}px`);
      engineRef.current?.setSafeArea(cover, below, hudRef.current?.offsetHeight ?? 44);
      setCompact(r.width > 0 && r.width < COMPACT_W);
    };
    const schedule = () => { if (!raf) raf = requestAnimationFrame(measure); };
    // A list or the detail scrolling inside the stage does not move the stage.
    const onScroll = (e: Event) => { if (!(e.target instanceof Node && root.contains(e.target))) schedule(); };
    schedule();
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", schedule);
    const ro = new ResizeObserver(schedule);
    ro.observe(document.body);
    ro.observe(root);
    if (hudRef.current) ro.observe(hudRef.current);
    return () => { cancelAnimationFrame(raf); window.removeEventListener("scroll", onScroll, true); window.removeEventListener("resize", schedule); ro.disconnect(); };
  }, []);
  // Keyboard users land on the detail's close button when it opens, and go back where they were when it closes.
  const detailKey = props.detail?.key;
  const returnTo = useRef<HTMLElement | null>(null);
  const hadDetail = useRef(false);
  useEffect(() => {
    const root = rootRef.current;
    if (!detailKey) {
      if (hadDetail.current) {
        hadDetail.current = false;
        const back = returnTo.current;
        returnTo.current = null;
        if (back?.isConnected && !back.closest("[inert]")) back.focus({ preventScroll: true });
        else hostRef.current?.focus({ preventScroll: true });
      }
      return;
    }
    if (!hadDetail.current) {
      const active = document.activeElement;
      returnTo.current = active instanceof HTMLElement && root?.contains(active) && !active.closest(".ov3d-panel.is-detail") ? active : null;
    }
    hadDetail.current = true;
    // The panel joins the document on the scene's next render; focus it once it is there.
    let raf = 0, tries = 0;
    const tryFocus = () => {
      const el = closeRef.current;
      if (el?.isConnected) el.focus({ preventScroll: true });
      else if (tries++ < 30) raf = requestAnimationFrame(tryFocus);
    };
    raf = requestAnimationFrame(tryFocus);
    return () => cancelAnimationFrame(raf);
  }, [detailKey]);
  useEffect(() => {
    const onChange = () => setFull(document.fullscreenElement === rootRef.current);
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, []);
  const toggleFull = () => {
    if (!hasFullscreen()) return;
    if (document.fullscreenElement) void document.exitFullscreen().catch(() => {});
    else void rootRef.current?.requestFullscreen().catch(() => {});
  };
  // Esc closes the open detail while focus is in the stage or nowhere (a pressed button that turned disabled drops
  // focus to <body>). A nested dialog that handled its own Esc, or a field being typed in, keeps it open.
  const hasDetail = !!props.detail;
  const onCloseDetailRef = useLatest(props.onCloseDetail);
  useEffect(() => {
    if (!hasDetail) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented) return;
      const active = document.activeElement;
      if (typing(active) || !(active === document.body || active === null || rootRef.current?.contains(active))) return;
      e.preventDefault();
      onCloseDetailRef.current();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [hasDetail, onCloseDetailRef]);
  // Dialogs (e.g. a device's threshold settings) portal to <body>, which fullscreen would hide: leave fullscreen
  // when one opens, so it is always visible.
  useEffect(() => {
    if (!full) return;
    const mo = new MutationObserver((records) => {
      for (const r of records) for (const n of r.addedNodes) {
        if (n instanceof HTMLElement && !rootRef.current?.contains(n) && (n.matches("[role=dialog], [role=alertdialog]") || n.querySelector("[role=dialog], [role=alertdialog]"))) {
          void document.exitFullscreen().catch(() => {});
          return;
        }
      }
    });
    mo.observe(document.body, { childList: true, subtree: true });
    return () => mo.disconnect();
  }, [full]);

  useEffect(() => { engineRef.current?.setData(stable.layout); }, [stable]);
  useEffect(() => { engineRef.current?.setReducedMotion(reduced); }, [reduced]);
  // A Tuya device's answer can move it into the controllable tier: re-rank the labels.
  useEffect(() => { engineRef.current?.relabel(); }, [canControl]);
  // The scene's colours come from the theme tokens: re-read them when the colour scheme changes.
  useEffect(() => {
    const m = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => engineRef.current?.setPalette(readPalette());
    m.addEventListener("change", onChange);
    return () => m.removeEventListener("change", onChange);
  }, []);
  // New label elements mount at the origin, hidden; the next frame places them.
  useEffect(() => { engineRef.current?.refreshLabels(); }, [labelKeys, cards]);

  useEffect(() => {
    if (!demo) return;
    const t = setInterval(() => { const s = engineRef.current?.stats(); if (s) setStats(`${s.tier} · ${s.calls} draw calls · ${(s.triangles / 1000).toFixed(0)}k tris · ${s.fps} fps`); }, 1000);
    return () => clearInterval(t);
  }, [demo]);

  const target = useMemo(() => cards.find((c) => c.sos) ?? cards.find((c) => c.status === "alert") ?? null, [cards]);
  const go = (p: Preset) => { setTour(false); engineRef.current?.preset(p); };
  // 1 / 2 / 3 only while focus is inside the stage (WCAG 2.1.4: no page-wide single-key shortcuts).
  const onKey = (e: ReactKeyboardEvent) => {
    if (e.defaultPrevented || typing(e.target as Element) || e.altKey || e.ctrlKey || e.metaKey) return;
    const preset = ({ "1": "overview", "2": "top", "3": "alert" } as Record<string, Preset>)[e.key];
    if (preset) { e.preventDefault(); go(preset); }
  };

  // A new SOS is announced once (the banner above stays the visual cue).
  const sosKeys = cards.filter((c) => c.sos).map((c) => c.key).sort().join("|");
  const [seenSos, setSeenSos] = useState({ keys: sosKeys, said: "" });
  if (seenSos.keys !== sosKeys) {
    const fresh = newSos(new Set(seenSos.keys.split("|").filter(Boolean)), cards);
    setSeenSos({ keys: sosKeys, said: fresh.length ? `SOS ใหม่ · ${fresh.map((c) => `${c.name}${c.gatewayName ? ` ที่ ${c.gatewayName}` : ""}`).join(" · ")} · ปุ่ม ไปที่เหตุ พาไปจุดนั้น` : seenSos.said });
  }

  const hoveredCard = hovered ? byKey.get(hovered) : undefined;
  const empty = stable.layout.nodes.length === 0;
  const lists = props.panels.filter((p): p is { key: StageListKey; node: ReactNode } => !!p.node);
  const shownKeys = labelKeys.filter((k) => byKey.has(k));

  return (
    <div ref={rootRef} className={`ov3d${reduced ? " is-still" : ""}${full ? " is-full" : ""}${compact ? " is-compact" : ""}`} onKeyDown={onKey}>
      {/* Tab order: the HUD, then the in-scene panels (KPIs, lists, SOS, detail), then the scene and its labels. */}
      <div ref={hudRef} className="ov3d-hud" role="region" aria-label="ตัวควบคุมภาพรวม 3D">
        <div className="ov3d-hud-head">
          <strong>ภาพรวม 3D</strong>
          {props.status}
          <div className="ov3d-toolbar" role="toolbar" aria-label="มุมกล้อง">
            <button type="button" onClick={() => go("overview")} title="ภาพรวม (1)"><View size={15} aria-hidden="true" /><span className="ov3d-sr">ภาพรวม</span><kbd>1</kbd></button>
            <button type="button" onClick={() => go("top")} title="มุมบน (2)"><ScanEye size={15} aria-hidden="true" /><span className="ov3d-sr">มุมบน</span><kbd>2</kbd></button>
            <button type="button" className={target?.sos ? "is-sos" : target ? "is-alert" : ""} disabled={!target} onClick={() => go("alert")} title={target ? `ไปที่เหตุ: ${target.name} (3)` : "ไม่มีเหตุที่ต้องดูในตัวกรองนี้"}><Crosshair size={15} aria-hidden="true" /><span className="ov3d-sr">ไปที่เหตุ</span><kbd>3</kbd></button>
            <button type="button" aria-pressed={tour} disabled={reduced} title={reduced ? "ทัวร์ปิดอยู่ เพราะตั้งค่าลดการเคลื่อนไหวไว้" : "ทัวร์ · หมุนกล้องรอบช้า ๆ · ลากเมื่อไรก็หยุด"} onClick={() => setTour(engineRef.current?.setTour(!tour) ?? false)}>{tour ? <Pause size={15} aria-hidden="true" /> : <Orbit size={15} aria-hidden="true" />}<span className="ov3d-sr">ทัวร์</span></button>
            {hasFullscreen() && <button type="button" aria-pressed={full} onClick={toggleFull} title={full ? "ออกจากเต็มจอ (Esc)" : "เต็มจอ"}>{full ? <Minimize size={15} aria-hidden="true" /> : <Maximize size={15} aria-hidden="true" />}<span className="ov3d-sr">เต็มจอ</span></button>}
            <button type="button" className={`ov3d-filter-btn${hudOpen ? " is-on" : ""}`} aria-expanded={hudOpen} onClick={() => setHudOpen(!hudOpen)} title="ตัวกรอง: โปรเจค ชนิด ต้องดู ค้นหา">{hudOpen ? <ChevronUp size={15} aria-hidden="true" /> : <SlidersHorizontal size={15} aria-hidden="true" />}ตัวกรอง</button>
            {compact && <button type="button" className={`ov3d-filter-btn${listsOpen ? " is-on" : ""}`} aria-expanded={listsOpen} onClick={() => setListsOpen(!listsOpen)} title="รายการ: ต้องดูตอนนี้ ใครอยู่ที่ไหน Gateway เหตุการณ์ล่าสุด"><ListIcon size={15} aria-hidden="true" />รายการ</button>}
            <button type="button" onClick={props.onShowCards} title="กลับไปมุมมองการ์ด"><LayoutGrid size={15} aria-hidden="true" />การ์ด</button>
          </div>
        </div>
        {hudOpen && <div className="ov3d-hud-filters">{props.filters}</div>}
        {/* Compact stage: the side lists live in this drawer instead of the scene. */}
        {compact && listsOpen && <div className="ov3d-hud-lists">{lists.map((p) => <div key={p.key} className="ov3d-hud-list">{p.node}</div>)}</div>}
      </div>

      <div ref={cssHostRef} className="ov3d-css-host" />
      {props.kpis.map((k, i) => <Panel3D key={k.key} registry={registry} id={`kpi-${k.key}`} kind="kpi" place={{ slot: `kpi-${i}` }}>{k.node}</Panel3D>)}
      {!compact && lists.map((p) => <Panel3D key={p.key} registry={registry} id={`list-${p.key}`} kind="list" place={{ slot: LIST_SLOT[p.key] }}>{p.node}</Panel3D>)}
      {props.sos && <Panel3D registry={registry} id="sos" kind="sos" place={{ above: props.sos.key }} width={Math.max(10, wall * 0.5)}>{props.sos.node}</Panel3D>}
      {props.detail && (
        // Keyed by device: switching device remounts the card and its controls (no command can reach the previous one).
        <Panel3D key={props.detail.key} registry={registry} id="detail" kind="detail" place={{ beside: props.detail.key }} width={9.5} label={`รายละเอียด ${props.detail.title}`}>
          <div className={`ov3d-detail-head${props.detail.sos ? " is-sos" : ""}`}>
            <div><strong>{props.detail.title}</strong><small>{props.detail.subtitle}</small></div>
            <button ref={closeRef} type="button" className="ov3d-close" aria-label="ปิดรายละเอียด (Esc)" title="ปิด (Esc)" onClick={props.onCloseDetail}><X size={16} aria-hidden="true" /></button>
          </div>
          <div className="ov3d-detail-body">{props.detail.body}</div>
        </Panel3D>
      )}

      <div ref={hostRef} className="ov3d-canvas" role="application" tabIndex={0} aria-roledescription="ภาพ 3 มิติ"
        aria-label={`ภาพรวม 3 มิติ · gateway ${stable.layout.hubs.length} จุด · อุปกรณ์ ${stable.layout.nodes.length} ชิ้น · ลากเพื่อหมุน ล้อเมาส์เพื่อซูม ลูกศรเพื่อเลื่อน ปุ่ม 1 2 3 เปลี่ยนมุมกล้อง · ป้ายชื่อเป็นปุ่มเปิดรายละเอียด`} />
      <div className="ov3d-vignette" aria-hidden="true" />

      <div className="ov3d-labels">
        {stable.layout.hubs.map((hp) => (
          <button key={hp.hub.id} type="button" className={`ov3d-hub${hp.hub.online ? " is-online" : ""}`} style={hp.hub.color ? ({ "--ov3d-hub": hp.hub.color } as CSSProperties) : undefined}
            ref={(el) => { if (el) hubEls.current.set(hp.hub.id, el); else hubEls.current.delete(hp.hub.id); }}
            aria-label={`ไปที่ gateway ${hp.hub.name} · อุปกรณ์ ${hp.hub.count} ชิ้น · ${hp.hub.online ? "ส่งข้อมูลอยู่" : "ไม่ส่งข้อมูล"}`}
            onClick={() => engineRef.current?.focusHub(hp.hub.id)}>
            <i aria-hidden="true" />{hp.hub.name}<small>{hp.hub.count}</small>
          </button>
        ))}
        {shownKeys.map((key) => {
          const c = byKey.get(key)!;
          return (
            <div key={key} className={`ov3d-label is-${c.status}${c.sos ? " is-sos" : ""}${c.occupancy?.occupied && c.status !== "offline" ? " is-occupied" : ""}${c.door?.leftOpen && c.status !== "offline" ? " is-door-long" : ""}`}
              ref={(el) => { if (el) labelEls.current.set(key, el); else labelEls.current.delete(key); }}>
              <button type="button" className="ov3d-label-main" onClick={() => onOpen(key)}>
                <span className="ov3d-label-head"><i className="ov-dot" aria-hidden="true" />{c.sos && <b className="ov-sos-tag">SOS</b>}<span className="ov3d-label-name">{c.name}</span><span className="ov3d-sr"> · {statusText(c)} · เปิดรายละเอียด</span></span>
                {renderValue(c)}
              </button>
              {canControl(c) && <PowerToggle store={store} deviceId={c.deviceId!} name={c.name} refreshKey={c.reading?.received_at} />}
            </div>
          );
        })}
      </div>

      <div ref={tooltipRef} className="ov3d-tooltip" hidden={!hoveredCard} aria-hidden="true">
        {hoveredCard && (
          <>
            <strong>{hoveredCard.name}</strong>
            <small>{kindLabel[hoveredCard.kind] ?? hoveredCard.kind} · {hoveredCard.gatewayName || "ไม่ทราบ gateway"}</small>
            {renderValue(hoveredCard)}
            <span className={`ov3d-tip-status is-${hoveredCard.status}${hoveredCard.sos ? " is-sos" : ""}`}>{statusText(hoveredCard)}{hoveredCard.reasons[0] ? ` · ${hoveredCard.reasons[0]}` : ""}</span>
            <small>คลิกเพื่อดูรายละเอียดและสั่งงาน</small>
          </>
        )}
      </div>

      <details className="ov3d-legend">
        <summary><Info size={13} aria-hidden="true" /> สัญลักษณ์</summary>
        <ul>
          <li><i className="sw is-online" />ปกติ</li>
          <li><i className="sw is-stale" />ข้อมูลเริ่มเก่า / ประตูเปิดค้าง</li>
          <li><i className="sw is-alert" />ต้องตรวจสอบ (วงแหวนกะพริบ)</li>
          <li><i className="sw is-offline" />ขาดการติดต่อ</li>
          <li><i className="sw is-sos" />SOS · ลำแสงแดง</li>
          <li><i className="ring is-occ" />มีคนในห้อง</li>
        </ul>
        <p>ทรงกลม = เซนเซอร์ · ลูกบาศก์ = สวิตช์ / มิเตอร์ · เพชร = ปุ่ม / ฉุกเฉิน / ภัย · แคปซูล = ตรวจจับคน · แผ่น = ประตู / ม่าน (เอียง = เปิด) · ผลึก = หลอดไฟ / พัดลม · หอคอย = gateway (สีตามโปรเจค)</p>
      </details>

      {demo > 0 && <div className="ov3d-demo">ข้อมูลจำลอง {demo} จุด (dev) · {stats}</div>}
      {empty && <p className="ov3d-empty">ไม่มีอุปกรณ์ตรงกับตัวกรองนี้</p>}
      <p className="ov3d-sr" aria-live="assertive" aria-atomic="true">{seenSos.said}</p>
    </div>
  );
}
