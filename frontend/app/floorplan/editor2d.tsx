"use client";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLatest } from "../topology/use-latest";
import { COLORS, ITEM_TYPES, area, clamp, dist, occupancyShort, ringOffset, round2, snapTo, uid, wallLength, wearablesAt, zoneOccupancy, type AssetView, type Draft, type Item, type Point, type Selection, type Tool, type Wall, type Zone } from "./model";

export type ViewBox = { x: number; y: number; w: number; h: number };
type Drag =
  | { kind: "pan"; start: [number, number]; box: ViewBox }
  | { kind: "move"; target: NonNullable<Selection>; from: Point; base: Draft; moved: boolean }
  | { kind: "vertex"; target: { kind: "wall" | "zone"; id: string }; index: number; base: Draft; moved: boolean }
  | { kind: "resize"; id: string; base: Draft; moved: boolean }
  | { kind: "rect"; from: Point; to: Point };

const ITEM_GLYPH: Record<string, string> = { door: "D", window: "W", stairs: "≡", elevator: "EV", exit: "EXIT", bed: "เตียง", desk: "โต๊ะ", rack: "ตู้", extinguisher: "FE", label: "" };

export default function Editor2D({
  draft,
  tool,
  itemType,
  selection,
  snap,
  showGrid,
  showCoverage,
  assets,
  imageURL,
  readOnly,
  box,
  onBox,
  onSelect,
  onChange,
  onToolDone,
}: {
  draft: Draft;
  tool: Tool;
  itemType: string;
  selection: Selection;
  snap: number;
  showGrid: boolean;
  showCoverage: boolean;
  assets: AssetView[];
  imageURL: string | null;
  readOnly: boolean;
  box: ViewBox;
  onBox: (box: ViewBox) => void;
  onSelect: (s: Selection) => void;
  /** transient=true while dragging (no undo step); the final call of a gesture passes the draft it started from. */
  onChange: (next: Draft, opts: { transient: boolean; base?: Draft }) => void;
  onToolDone: () => void;
}) {
  const svgRef = useRef<SVGSVGElement | null>(null);
  const [size, setSize] = useState({ w: 1000, h: 700 });
  const [pending, setPending] = useState<Point[]>([]);
  const [cursor, setCursor] = useState<Point | null>(null);
  const [measure, setMeasure] = useState<Point[]>([]);
  const drag = useRef<Drag | null>(null);
  const [rect, setRect] = useState<{ from: Point; to: Point } | null>(null);
  const [spaceDown, setSpaceDown] = useState(false);
  const latest = useLatest({ draft, tool, pending, selection, readOnly, onChange, onSelect, onToolDone, snap, itemType });

  useEffect(() => {
    const el = svgRef.current;
    if (!el) return;
    const ro = new ResizeObserver((entries) => {
      const r = entries[0].contentRect;
      if (r.width > 0 && r.height > 0) setSize({ w: r.width, h: r.height });
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Keep the aspect ratio of the viewBox equal to the element so metres are square on screen.
  const view = useMemo(() => {
    const h = (box.w * size.h) / size.w;
    return { x: box.x, y: box.y + (box.h - h) / 2, w: box.w, h };
  }, [box, size]);
  const px = view.w / size.w; // metres per CSS pixel
  const assetById = useMemo(() => new Map(assets.map((a) => [`${a.kind}:${a.id}`, a])), [assets]);

  const toMetres = useCallback((e: { clientX: number; clientY: number }): Point => {
    const el = svgRef.current!;
    const r = el.getBoundingClientRect();
    return [view.x + ((e.clientX - r.left) / r.width) * view.w, view.y + ((e.clientY - r.top) / r.height) * view.h];
  }, [view]);
  const snapped = useCallback((p: Point, e?: { shiftKey: boolean }, anchor?: Point): Point => {
    let q: Point = [snapTo(p[0], snap), snapTo(p[1], snap)];
    if (e?.shiftKey && anchor) q = Math.abs(q[0] - anchor[0]) > Math.abs(q[1] - anchor[1]) ? [q[0], anchor[1]] : [anchor[0], q[1]];
    return [round2(q[0]), round2(q[1])];
  }, [snap]);

  // Plain function (not memoised): it reads the latest values through the ref when a key or click calls it.
  function finishPending() {
    const { draft: d, tool: t, pending: pts, onChange: change, onSelect: select, onToolDone: done } = latest.current;
    if (t === "wall" && pts.length >= 2) {
      const wall: Wall = { id: uid("w"), points: pts, thickness: 0.15 };
      change({ ...d, layout: { ...d.layout, walls: [...d.layout.walls, wall] } }, { transient: false, base: d });
      select({ kind: "wall", id: wall.id });
    } else if (t === "zone" && pts.length >= 3) {
      const zone: Zone = { id: uid("z"), name: `โซน ${d.layout.zones.length + 1}`, kind: "room", color: Object.keys(COLORS)[d.layout.zones.length % 6], points: pts };
      change({ ...d, layout: { ...d.layout, zones: [...d.layout.zones, zone] } }, { transient: false, base: d });
      select({ kind: "zone", id: zone.id });
      done();
    }
    setPending([]);
  }
  const finishRef = useLatest(finishPending);

  useEffect(() => {
    const down = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      if (el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT" || el.isContentEditable)) return;
      // Space pans, but it must keep activating a focused button, and do nothing while a dialog is open.
      if (document.querySelector("[data-slot=dialog-content],[role=alertdialog]")) return;
      if (e.code === "Space" && el?.tagName !== "BUTTON" && el?.tagName !== "A") { setSpaceDown(true); e.preventDefault(); }
      if (e.key === "Enter") finishRef.current();
      if (e.key === "Escape") { setPending([]); setMeasure([]); setRect(null); drag.current = null; }
      if (e.key === "Backspace" && latest.current.pending.length > 0) { setPending((p) => p.slice(0, -1)); e.preventDefault(); }
    };
    const up = (e: KeyboardEvent) => { if (e.code === "Space") setSpaceDown(false); };
    window.addEventListener("keydown", down);
    window.addEventListener("keyup", up);
    return () => { window.removeEventListener("keydown", down); window.removeEventListener("keyup", up); };
  }, [finishRef, latest]);

  // Wheel zoom around the pointer. Registered natively because React's wheel listener is passive.
  const boxRef = useLatest({ view, onBox });
  useEffect(() => {
    const el = svgRef.current;
    if (!el) return;
    const wheel = (e: WheelEvent) => {
      e.preventDefault();
      const { view: v, onBox: set } = boxRef.current;
      const r = el.getBoundingClientRect();
      const fx = (e.clientX - r.left) / r.width, fy = (e.clientY - r.top) / r.height;
      const factor = Math.exp(clamp(e.deltaY, -120, 120) * 0.0016);
      const w = clamp(v.w * factor, 2, 2000), h = (w * v.h) / v.w;
      set({ x: v.x + fx * (v.w - w), y: v.y + fy * (v.h - h), w, h });
    };
    el.addEventListener("wheel", wheel, { passive: false });
    return () => el.removeEventListener("wheel", wheel);
  }, [boxRef]);

  const translate = (d: Draft, target: NonNullable<Selection>, dx: number, dy: number): Draft => {
    const mv = (p: Point): Point => [round2(p[0] + dx), round2(p[1] + dy)];
    if (target.kind === "zone") return { ...d, layout: { ...d.layout, zones: d.layout.zones.map((z) => (z.id === target.id ? { ...z, points: z.points.map(mv) } : z)) } };
    if (target.kind === "wall") return { ...d, layout: { ...d.layout, walls: d.layout.walls.map((w) => (w.id === target.id ? { ...w, points: w.points.map(mv) } : w)) } };
    if (target.kind === "item") return { ...d, layout: { ...d.layout, items: d.layout.items.map((it) => (it.id === target.id ? { ...it, x: round2(it.x + dx), y: round2(it.y + dy) } : it)) } };
    return { ...d, placements: d.placements.map((p) => (`${p.asset_kind}:${p.asset_id}` === target.id ? { ...p, x: round2(p.x + dx), y: round2(p.y + dy) } : p)) };
  };

  const startMove = (e: React.PointerEvent, target: NonNullable<Selection>) => {
    if (tool !== "select" || spaceDown || e.button !== 0) return;
    e.stopPropagation();
    onSelect(target);
    if (readOnly) return;
    (e.currentTarget as Element).setPointerCapture?.(e.pointerId);
    drag.current = { kind: "move", target, from: toMetres(e), base: draft, moved: false };
  };

  const onPointerDown = (e: React.PointerEvent<SVGSVGElement>) => {
    if (e.button === 1 || tool === "pan" || spaceDown) {
      e.currentTarget.setPointerCapture(e.pointerId);
      drag.current = { kind: "pan", start: [e.clientX, e.clientY], box: view };
      return;
    }
    if (e.button !== 0) return;
    const raw = toMetres(e);
    if (tool === "select") { onSelect(null); return; }
    if (readOnly) return;
    if (tool === "measure") { setMeasure((m) => (m.length >= 2 ? [snapped(raw)] : [...m, snapped(raw)])); return; }
    if (tool === "rect") {
      e.currentTarget.setPointerCapture(e.pointerId);
      const from = snapped(raw);
      drag.current = { kind: "rect", from, to: from };
      setRect({ from, to: from });
      return;
    }
    if (tool === "item") {
      const spec = ITEM_TYPES.find((t) => t.id === itemType) ?? ITEM_TYPES[0];
      const p = snapped(raw);
      const item: Item = { id: uid("i"), type: spec.id, x: round2(p[0] - spec.w / 2), y: round2(p[1] - spec.h / 2), w: spec.w, h: spec.h, rot: 0, text: spec.id === "label" ? "ข้อความ" : undefined };
      onChange({ ...draft, layout: { ...draft.layout, items: [...draft.layout.items, item] } }, { transient: false, base: draft });
      onSelect({ kind: "item", id: item.id });
      return;
    }
    // wall / zone: add a vertex; clicking the first vertex closes a zone.
    const p = snapped(raw, e, pending[pending.length - 1]);
    if (tool === "zone" && pending.length >= 3 && dist(p, pending[0]) < 12 * px) { finishPending(); return; }
    if (pending.length && dist(p, pending[pending.length - 1]) < 1e-6) return;
    setPending([...pending, p]);
  };

  const onPointerMove = (e: React.PointerEvent<SVGSVGElement>) => {
    const d = drag.current;
    const raw = toMetres(e);
    if (!d) { if (tool === "wall" || tool === "zone" || tool === "measure") setCursor(snapped(raw, e, pending[pending.length - 1])); return; }
    if (d.kind === "pan") {
      const r = e.currentTarget.getBoundingClientRect();
      onBox({ ...d.box, x: d.box.x - ((e.clientX - d.start[0]) / r.width) * d.box.w, y: d.box.y - ((e.clientY - d.start[1]) / r.height) * d.box.h });
    } else if (d.kind === "move") {
      const dx = snapTo(raw[0] - d.from[0], snap), dy = snapTo(raw[1] - d.from[1], snap);
      if (!d.moved && Math.hypot(raw[0] - d.from[0], raw[1] - d.from[1]) < 4 * px) return;
      d.moved = true;
      onChange(translate(d.base, d.target, dx, dy), { transient: true });
    } else if (d.kind === "vertex") {
      d.moved = true;
      const p = snapped(raw);
      const edit = <T extends { id: string; points: Point[] }>(list: T[]) => list.map((s) => (s.id === d.target.id ? { ...s, points: s.points.map((q, i) => (i === d.index ? p : q)) } : s));
      onChange(d.target.kind === "zone" ? { ...d.base, layout: { ...d.base.layout, zones: edit(d.base.layout.zones) } } : { ...d.base, layout: { ...d.base.layout, walls: edit(d.base.layout.walls) } }, { transient: true });
    } else if (d.kind === "resize") {
      d.moved = true;
      const p = snapped(raw);
      onChange({ ...d.base, layout: { ...d.base.layout, items: d.base.layout.items.map((it) => (it.id === d.id ? { ...it, w: round2(Math.max(0.2, p[0] - it.x)), h: round2(Math.max(0.1, p[1] - it.y)) } : it)) } }, { transient: true });
    } else if (d.kind === "rect") {
      d.to = snapped(raw);
      setRect({ from: d.from, to: d.to });
    }
  };

  const onPointerUp = () => {
    const d = drag.current;
    drag.current = null;
    if (!d) return;
    if ((d.kind === "move" || d.kind === "vertex" || d.kind === "resize") && d.moved) onChange(latest.current.draft, { transient: false, base: d.base });
    if (d.kind === "rect") {
      setRect(null);
      const [x0, y0] = [Math.min(d.from[0], d.to[0]), Math.min(d.from[1], d.to[1])], [x1, y1] = [Math.max(d.from[0], d.to[0]), Math.max(d.from[1], d.to[1])];
      if (x1 - x0 < 0.5 || y1 - y0 < 0.5) return;
      const zone: Zone = { id: uid("z"), name: `ห้อง ${draft.layout.zones.length + 1}`, kind: "room", color: Object.keys(COLORS)[draft.layout.zones.length % 6], points: [[x0, y0], [x1, y0], [x1, y1], [x0, y1]] };
      onChange({ ...draft, layout: { ...draft.layout, zones: [...draft.layout.zones, zone] } }, { transient: false, base: draft });
      onSelect({ kind: "zone", id: zone.id });
      onToolDone();
    }
  };

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault();
    if (readOnly) return;
    let payload: { kind: "gateway" | "device"; id: string } | null = null;
    try { payload = JSON.parse(e.dataTransfer.getData("application/aether-asset")); } catch { return; }
    if (!payload || (payload.kind !== "gateway" && payload.kind !== "device")) return;
    const p = snapped(toMetres(e));
    const key = `${payload.kind}:${payload.id}`;
    const rest = draft.placements.filter((x) => `${x.asset_kind}:${x.asset_id}` !== key);
    onChange({ ...draft, placements: [...rest, { asset_kind: payload.kind, asset_id: payload.id, x: clamp(p[0], 0, draft.width_m), y: clamp(p[1], 0, draft.depth_m), z: payload.kind === "gateway" ? 2.4 : 1.2 }] }, { transient: false, base: draft });
    onSelect({ kind: "placement", id: key });
  };

  const insertVertex = (e: React.MouseEvent, target: { kind: "wall" | "zone"; id: string }, index: number) => {
    if (readOnly || tool !== "select") return;
    e.stopPropagation();
    const p = snapped(toMetres(e));
    const edit = <T extends { id: string; points: Point[] }>(list: T[]) => list.map((s) => (s.id === target.id ? { ...s, points: [...s.points.slice(0, index + 1), p, ...s.points.slice(index + 1)] } : s));
    onChange(target.kind === "zone" ? { ...draft, layout: { ...draft.layout, zones: edit(draft.layout.zones) } } : { ...draft, layout: { ...draft.layout, walls: edit(draft.layout.walls) } }, { transient: false, base: draft });
  };
  const removeVertex = (target: { kind: "wall" | "zone"; id: string }, index: number) => {
    const min = target.kind === "zone" ? 3 : 2;
    const edit = <T extends { id: string; points: Point[] }>(list: T[]) => list.map((s) => (s.id === target.id && s.points.length > min ? { ...s, points: s.points.filter((_, i) => i !== index) } : s));
    onChange(target.kind === "zone" ? { ...draft, layout: { ...draft.layout, zones: edit(draft.layout.zones) } } : { ...draft, layout: { ...draft.layout, walls: edit(draft.layout.walls) } }, { transient: false, base: draft });
  };

  const grid = view.w > 120 ? 5 : 1;
  const selectedShape = selection && (selection.kind === "zone" ? draft.layout.zones.find((z) => z.id === selection.id) : selection.kind === "wall" ? draft.layout.walls.find((w) => w.id === selection.id) : undefined);
  const font = (n: number) => n * px;
  const gateways = draft.placements.filter((p) => p.asset_kind === "gateway");
  const cursorClass = tool === "pan" || spaceDown ? "is-pan" : tool === "select" ? "" : "is-draw";
  const bg = draft.layout.background;

  return (
    <svg
      ref={svgRef}
      className={`fp-svg ${cursorClass}`}
      viewBox={`${view.x} ${view.y} ${view.w} ${view.h}`}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={() => { const d = drag.current; drag.current = null; setRect(null); if (d && "base" in d && d.moved) onChange(d.base, { transient: true }); }}
      onPointerLeave={() => setCursor(null)}
      onDoubleClick={() => (tool === "wall" || tool === "zone") && finishPending()}
      onDragOver={(e) => { e.preventDefault(); e.dataTransfer.dropEffect = "copy"; }}
      onDrop={onDrop}
      role="application"
      aria-label="ผังพื้นแบบ 2 มิติ"
    >
      <defs>
        <pattern id="fp-grid" width={grid} height={grid} patternUnits="userSpaceOnUse">
          <path d={`M ${grid} 0 L 0 0 0 ${grid}`} fill="none" stroke="var(--color-border)" strokeWidth={px} />
        </pattern>
      </defs>
      <rect x={0} y={0} width={draft.width_m} height={draft.depth_m} className="fp-floor" strokeWidth={2 * px} />
      {imageURL && bg && <image href={imageURL} x={bg.x} y={bg.y} width={bg.width_m} opacity={bg.opacity} preserveAspectRatio="xMinYMin meet" style={{ pointerEvents: "none" }} />}
      {showGrid && <rect x={0} y={0} width={draft.width_m} height={draft.depth_m} fill="url(#fp-grid)" style={{ pointerEvents: "none" }} />}

      {draft.layout.zones.map((z) => {
        const on = selection?.kind === "zone" && selection.id === z.id;
        const top = Math.min(...z.points.map((p) => p[1]));
        const tl: Point = [Math.min(...z.points.filter((p) => p[1] - top < 0.01).map((p) => p[0])), top];
        const people = (z.gateway_ids ?? []).reduce((n, g) => n + wearablesAt(assets, g).length, 0);
        // Somebody in this zone pressed the emergency button: the whole zone turns red so the operator
        // sees which room to run to before reading a single label.
        const sosHere = (z.gateway_ids ?? []).some((g) => wearablesAt(assets, g).some((w) => w.sos));
        // A PIR sensor placed inside the room decides whether it is occupied (smart office).
        const occ = zoneOccupancy(z, draft.placements, assetById);
        const occClass = occ ? (occ.occupied ? " is-occupied" : " is-vacant") : "";
        return (
          <g key={z.id} onPointerDown={(e) => startMove(e, { kind: "zone", id: z.id })}>
            <polygon points={z.points.map((p) => p.join(",")).join(" ")} fill={COLORS[z.color] ?? COLORS.mint} fillOpacity={people > 0 ? 0.3 : on ? 0.24 : 0.13} stroke={COLORS[z.color] ?? COLORS.mint} strokeWidth={(on ? 2.5 : 1.2) * px} strokeDasharray={z.kind === "restricted" ? `${6 * px} ${4 * px}` : undefined} className={`fp-zone${sosHere ? " is-sos" : occClass}`} />
            {/* Name in the zone's top-left corner so markers placed in the middle stay readable. */}
            <text x={tl[0] + 8 * px} y={tl[1] + 18 * px} fontSize={font(13)} className={`fp-zone-name${sosHere ? " is-sos" : ""}`}>{sosHere ? `SOS · ${z.name}` : z.name}</text>
            <text x={tl[0] + 8 * px} y={tl[1] + 33 * px} fontSize={font(10.5)} className="fp-zone-meta">{area(z.points).toFixed(1)} m²{people > 0 ? ` · ${people} คน` : ""}</text>
            {occ && <text x={tl[0] + 8 * px} y={tl[1] + 48 * px} fontSize={font(11)} className={`fp-zone-occ${occClass}`}>{occupancyShort(occ)}</text>}
          </g>
        );
      })}

      {draft.layout.walls.map((w) => {
        const on = selection?.kind === "wall" && selection.id === w.id;
        return (
          <g key={w.id} onPointerDown={(e) => startMove(e, { kind: "wall", id: w.id })}>
            <polyline points={(w.closed ? [...w.points, w.points[0]] : w.points).map((p) => p.join(",")).join(" ")} fill="none" className={`fp-wall ${on ? "is-on" : ""}`} strokeWidth={Math.max(w.thickness, 2 * px)} strokeLinejoin="miter" strokeLinecap="square" />
            <polyline points={w.points.map((p) => p.join(",")).join(" ")} fill="none" stroke="transparent" strokeWidth={Math.max(w.thickness, 12 * px)} />
          </g>
        );
      })}

      {draft.layout.items.map((it) => {
        const on = selection?.kind === "item" && selection.id === it.id;
        return (
          <g key={it.id} transform={`rotate(${it.rot} ${it.x + it.w / 2} ${it.y + it.h / 2})`} onPointerDown={(e) => startMove(e, { kind: "item", id: it.id })}>
            {it.type !== "label" && <rect x={it.x} y={it.y} width={it.w} height={it.h} rx={Math.min(0.08, it.h / 3)} className={`fp-item type-${it.type} ${on ? "is-on" : ""}`} strokeWidth={(on ? 2 : 1) * px} />}
            {it.type === "label" && on && <rect x={it.x} y={it.y} width={it.w} height={it.h} fill="none" stroke="var(--color-accent)" strokeWidth={px} strokeDasharray={`${4 * px} ${3 * px}`} />}
            <text x={it.x + it.w / 2} y={it.y + it.h / 2} fontSize={it.type === "label" ? clamp(it.h * 0.7, font(9), 3) : Math.min(font(10), Math.max(it.h * 0.6, font(7)))} className={`fp-item-text ${it.type === "label" ? "is-label" : ""}`} textAnchor="middle" dominantBaseline="central">{it.type === "label" ? it.text : (it.text || ITEM_GLYPH[it.type])}</text>
            {on && !readOnly && <rect x={it.x + it.w - 5 * px} y={it.y + it.h - 5 * px} width={10 * px} height={10 * px} className="fp-handle" onPointerDown={(e) => { e.stopPropagation(); (e.currentTarget as Element).setPointerCapture?.(e.pointerId); drag.current = { kind: "resize", id: it.id, base: draft, moved: false }; }} />}
          </g>
        );
      })}

      {showCoverage && gateways.map((p) => <circle key={`cov-${p.asset_id}`} cx={p.x} cy={p.y} r={10} className="fp-coverage" strokeWidth={px} />)}

      {draft.placements.map((p) => {
        const key = `${p.asset_kind}:${p.asset_id}`, a = assetById.get(key), on = selection?.kind === "placement" && selection.id === key;
        const r = (p.asset_kind === "gateway" ? 11 : 8) * px;
        const people = p.asset_kind === "gateway" ? wearablesAt(assets, p.asset_id) : [];
        const door = a?.door;
        return (
          <g key={key} onPointerDown={(e) => startMove(e, { kind: "placement", id: key })} className={`fp-asset kind-${p.asset_kind} ${a?.online ? "is-online" : "is-offline"} ${a?.alert ? "is-alert" : ""} ${on ? "is-on" : ""}`}>
            <title>{a ? `${a.name}${a.summary ? ` · ${a.summary}` : ""}` : "อุปกรณ์ที่ถูกนำออกแล้ว"}</title>
            {door ? (
              // Door sensor: a plan-style door. Closed = leaf along the frame; open = leaf swung out with its arc.
              <g className={`fp-door ${door.open ? "is-open" : door.open === false ? "is-closed" : "is-unknown"}${door.leftOpen ? " is-long" : ""}`}>
                <circle cx={p.x} cy={p.y} r={r * 1.5} className="fp-door-hit" />
                <line x1={p.x - r * 1.2} y1={p.y} x2={p.x + r * 1.2} y2={p.y} className="fp-door-frame" strokeWidth={(on ? 3 : 2) * px} />
                {door.open ? (
                  <>
                    <path d={`M ${p.x + r * 1.2} ${p.y} A ${r * 2.4} ${r * 2.4} 0 0 0 ${p.x - r * 1.2} ${p.y - r * 2.4}`} className="fp-door-arc" strokeWidth={px} strokeDasharray={`${3 * px} ${2 * px}`} />
                    <line x1={p.x - r * 1.2} y1={p.y} x2={p.x - r * 1.2} y2={p.y - r * 2.4} className="fp-door-leaf" strokeWidth={2.4 * px} />
                  </>
                ) : (
                  <line x1={p.x - r * 1.2} y1={p.y - 2 * px} x2={p.x + r * 1.2} y2={p.y - 2 * px} className="fp-door-leaf" strokeWidth={2.4 * px} />
                )}
              </g>
            ) : p.asset_kind === "gateway" ? <rect x={p.x - r} y={p.y - r} width={2 * r} height={2 * r} rx={3 * px} strokeWidth={(on ? 3 : 1.6) * px} className="fp-asset-body" /> : <circle cx={p.x} cy={p.y} r={r} strokeWidth={(on ? 3 : 1.6) * px} className="fp-asset-body" />}
            {a?.online && !door && <circle cx={p.x} cy={p.y} r={r * 0.34} className="fp-asset-dot" />}
            <text x={p.x} y={p.y + r + font(12)} fontSize={font(11)} className="fp-asset-name" textAnchor="middle">{a?.name ?? "ไม่พบ"}</text>
            {a?.summary && p.asset_kind === "device" && <text x={p.x} y={p.y + r + font(24)} fontSize={font(10)} className={`fp-asset-meta${door?.leftOpen ? " is-warn" : a.occupancy?.occupied ? " is-occupied" : ""}`} textAnchor="middle">{a.summary}</text>}
            {people.map((w, i) => {
              const [ox, oy] = ringOffset(i, people.length, 38 * px);
              return (
                <g key={w.id} className={`fp-wearable${w.sos ? " is-sos" : ""}`}>
                  <title>{w.sos ? `SOS · ${w.name} กดปุ่มฉุกเฉินใกล้ ${a?.name ?? "gateway"}` : `${w.name} · อยู่ใกล้ ${a?.name ?? "gateway"}`}</title>
                  <line x1={p.x} y1={p.y} x2={p.x + ox} y2={p.y + oy} strokeWidth={px} />
                  <circle cx={p.x + ox} cy={p.y + oy} r={(w.sos ? 9 : 7) * px} strokeWidth={(w.sos ? 2.4 : 1.5) * px} />
                  <circle cx={p.x + ox} cy={p.y + oy} r={(w.sos ? 9 : 7) * px} className="fp-pulse" strokeWidth={px} style={{ transformOrigin: `${p.x + ox}px ${p.y + oy}px` }} />
                  <text x={p.x + ox + (ox >= 0 ? 13 : -13) * px} y={p.y + oy + 4 * px} fontSize={font(w.sos ? 12 : 10.5)} textAnchor={ox >= 0 ? "start" : "end"}>{w.sos ? `SOS · ${w.name}` : w.name}</text>
                </g>
              );
            })}
          </g>
        );
      })}

      {selectedShape && !readOnly && tool === "select" && (
        <g>
          {selectedShape.points.map((p, i) => {
            const closed = selection!.kind === "zone" || (selectedShape as Wall).closed;
            const next = selectedShape.points[i + 1] ?? (closed ? selectedShape.points[0] : null);
            return next ? <line key={`e${i}`} x1={p[0]} y1={p[1]} x2={next[0]} y2={next[1]} stroke="transparent" strokeWidth={12 * px} className="fp-edge-hit" onDoubleClick={(e) => insertVertex(e, { kind: selection!.kind as "wall" | "zone", id: selectedShape.id }, i)} /> : null;
          })}
          {selectedShape.points.map((p, i) => (
            <rect key={`v${i}`} x={p[0] - 5 * px} y={p[1] - 5 * px} width={10 * px} height={10 * px} className="fp-handle"
              onPointerDown={(e) => {
                e.stopPropagation();
                const target = { kind: selection!.kind as "wall" | "zone", id: selectedShape.id };
                if (e.altKey) { removeVertex(target, i); return; }
                (e.currentTarget as Element).setPointerCapture?.(e.pointerId);
                drag.current = { kind: "vertex", target, index: i, base: draft, moved: false };
              }} />
          ))}
        </g>
      )}

      {pending.length > 0 && (
        <g style={{ pointerEvents: "none" }}>
          <polyline points={[...pending, ...(cursor ? [cursor] : [])].map((p) => p.join(",")).join(" ")} className={`fp-pending tool-${tool}`} strokeWidth={2 * px} strokeDasharray={`${6 * px} ${4 * px}`} />
          {pending.map((p, i) => <circle key={i} cx={p[0]} cy={p[1]} r={(i === 0 && tool === "zone" ? 6 : 3.5) * px} className="fp-pending-dot" />)}
          {cursor && <text x={cursor[0] + 10 * px} y={cursor[1] - 8 * px} fontSize={font(11)} className="fp-dim">{dist(pending[pending.length - 1], cursor).toFixed(2)} m</text>}
        </g>
      )}
      {rect && <rect x={Math.min(rect.from[0], rect.to[0])} y={Math.min(rect.from[1], rect.to[1])} width={Math.abs(rect.to[0] - rect.from[0])} height={Math.abs(rect.to[1] - rect.from[1])} className="fp-pending tool-zone" strokeWidth={2 * px} strokeDasharray={`${6 * px} ${4 * px}`} />}
      {measure.length > 0 && (
        <g style={{ pointerEvents: "none" }}>
          <line x1={measure[0][0]} y1={measure[0][1]} x2={(measure[1] ?? cursor ?? measure[0])[0]} y2={(measure[1] ?? cursor ?? measure[0])[1]} className="fp-measure" strokeWidth={1.5 * px} />
          <text x={((measure[1] ?? cursor ?? measure[0])[0] + measure[0][0]) / 2} y={((measure[1] ?? cursor ?? measure[0])[1] + measure[0][1]) / 2 - 6 * px} fontSize={font(12)} className="fp-dim" textAnchor="middle">{dist(measure[0], measure[1] ?? cursor ?? measure[0]).toFixed(2)} m</text>
        </g>
      )}
      {selection?.kind === "wall" && selectedShape && <text x={selectedShape.points[0][0]} y={selectedShape.points[0][1] - 10 * px} fontSize={font(11)} className="fp-dim">{wallLength(selectedShape as Wall).toFixed(2)} m</text>}
    </svg>
  );
}
