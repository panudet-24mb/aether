"use client";
// The overview's 3D mode (docs/platform/overview-3d.md): the board's own cards, filters included, as a constellation
// of gateway hubs with their devices in rings. Loaded lazily, so the card mode never pays for three.js.

import { useEffect, useMemo, useRef, useState, useSyncExternalStore, type CSSProperties, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from "react";
import { Crosshair, Info, LayoutGrid, Orbit, Pause, ScanEye, View } from "lucide-react";
import * as THREE from "three";
import type { CommandClient } from "../../topology/device-controls";
import { useLatest } from "../../topology/use-latest";
import { ConstellationEngine, type Palette, type Preset } from "./engine";
import { buildHubs, layoutConstellation, newSos } from "./layout";
import { useOutputs } from "./outputs";
import { PowerStore, PowerToggle } from "./power";
import { syntheticSite } from "./synthetic";
import { controllable, poweredOn, type HubPlace, type NodePlace, type StageCard, type StageGateway, type StageProject } from "./types";
import "./stage.css";

export type StageProps = {
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

const typing = (el: Element | null) => !!el && (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement || (el as HTMLElement).isContentEditable);

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

  const hostRef = useRef<HTMLDivElement>(null);
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
      }, { quality: "auto", reducedMotion: window.matchMedia(reducedQuery).matches });
    } catch {
      onUnavailableRef.current(UNAVAILABLE);
      return;
    }
    const lost = (e: Event) => { e.preventDefault(); onUnavailableRef.current(UNAVAILABLE); };
    engine.renderer.domElement.addEventListener("webglcontextlost", lost);
    engineRef.current = engine;
    return () => {
      engine.renderer.domElement.removeEventListener("webglcontextlost", lost);
      engine.dispose();
      engineRef.current = null;
    };
  }, [onOpenRef, onUnavailableRef, canControlRef]);

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
  useEffect(() => { engineRef.current?.invalidate(); }, [labelKeys, cards]);

  useEffect(() => {
    if (!demo) return;
    const t = setInterval(() => { const s = engineRef.current?.stats(); if (s) setStats(`${s.tier} · ${s.calls} draw calls · ${(s.triangles / 1000).toFixed(0)}k tris · ${s.fps} fps`); }, 1000);
    return () => clearInterval(t);
  }, [demo]);

  const target = useMemo(() => cards.find((c) => c.sos) ?? cards.find((c) => c.status === "alert") ?? null, [cards]);
  const go = (p: Preset) => { setTour(false); engineRef.current?.preset(p); };
  // 1 / 2 / 3 only while focus is inside the stage (WCAG 2.1.4: no page-wide single-key shortcuts).
  const onKey = (e: ReactKeyboardEvent) => {
    if (e.defaultPrevented || e.altKey || e.ctrlKey || e.metaKey || typing(e.target as Element)) return;
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
  const shownKeys = labelKeys.filter((k) => byKey.has(k));

  return (
    <div className={`ov3d${reduced ? " is-still" : ""}`} onKeyDown={onKey}>
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

      <div className="ov3d-toolbar" role="toolbar" aria-label="มุมกล้อง">
        <button type="button" onClick={() => go("overview")} title="ภาพรวม (1)"><View size={14} aria-hidden="true" />ภาพรวม<kbd>1</kbd></button>
        <button type="button" onClick={() => go("top")} title="มุมบน (2)"><ScanEye size={14} aria-hidden="true" />มุมบน<kbd>2</kbd></button>
        <button type="button" className={target?.sos ? "is-sos" : target ? "is-alert" : ""} disabled={!target} onClick={() => go("alert")} title={target ? `ไปที่ ${target.name} (3)` : "ไม่มีเหตุที่ต้องดูในตัวกรองนี้"}><Crosshair size={14} aria-hidden="true" />ไปที่เหตุ<kbd>3</kbd></button>
        <button type="button" aria-pressed={tour} disabled={reduced} title={reduced ? "ปิดอยู่ เพราะตั้งค่าลดการเคลื่อนไหวไว้" : "หมุนกล้องรอบช้า ๆ · ลากเมื่อไรก็หยุด"} onClick={() => setTour(engineRef.current?.setTour(!tour) ?? false)}>{tour ? <Pause size={14} aria-hidden="true" /> : <Orbit size={14} aria-hidden="true" />}ทัวร์</button>
        <button type="button" onClick={props.onShowCards} title="กลับไปมุมมองการ์ด"><LayoutGrid size={14} aria-hidden="true" />การ์ด</button>
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
