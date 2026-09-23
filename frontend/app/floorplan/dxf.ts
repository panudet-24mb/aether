/**
 * Dependency-free reader for ASCII DXF — the group-code / value text format that every CAD tool can
 * export (AutoCAD, BricsCAD, LibreCAD, SketchUp, Revit) — plus the geometry clean-up that turns a
 * drawing into floor plan walls, zones and labels.
 *
 * This module deliberately imports nothing so dxf.selftest.ts can compile and run it on its own.
 * Its output types are structurally the Wall / Zone / Item of ./model: metres, origin at the floor's
 * top-left corner, y growing DOWN (DXF's y grows up, so the build step flips it).
 */

export type Point = [number, number];
export type DxfUnit = "mm" | "cm" | "m" | "in" | "ft";

export const UNIT_M: Record<DxfUnit, number> = { mm: 0.001, cm: 0.01, m: 1, in: 0.0254, ft: 0.3048 };
export const UNIT_LABEL: Record<DxfUnit, string> = { mm: "มิลลิเมตร (mm)", cm: "เซนติเมตร (cm)", m: "เมตร (m)", in: "นิ้ว (in)", ft: "ฟุต (ft)" };
export const UNITS: DxfUnit[] = ["mm", "cm", "m", "in", "ft"];

/** Caps. The first two keep a hostile or simply enormous file from locking up the tab; the rest mirror the API's per-floor limits. */
export const MAX_CHARS = 25 * 1024 * 1024;
export const MAX_ENTITIES = 400000;
export const MAX_WALLS = 500;
export const MAX_ZONES = 200;
export const MAX_ITEMS = 500;
export const MAX_WALL_POINTS = 200;
export const MAX_ZONE_POINTS = 100;
export const MAX_LAYOUT_BYTES = 200 * 1024;
export const MAX_COORD = 2000;
const INSERT_DEPTH = 4;
const JOIN_EPS = 0.001; // 1 mm, applied after scaling to metres
const ARC_STEPS = 64; // segments per full turn before simplification thins them out

export class DxfCancelled extends Error {
  constructor() { super("ยกเลิกการอ่านไฟล์แล้ว"); this.name = "DxfCancelled"; }
}

/* ------------------------------------------------------------------ parsing */

type Pair = [number, string];
type Rec = { type: string; pairs: Pair[] };

export type DxfShape =
  | { k: "poly"; layer: string; pts: Point[]; closed: boolean }
  | { k: "text"; layer: string; at: Point; text: string; h: number };

export type DxfLayerStat = { name: string; total: number; polys: number; closed: number; texts: number; kinds: Record<string, number> };

export type DxfDoc = {
  /** Unit from $INSUNITS, or the best guess from the drawing's size when the file is unitless. */
  unit: DxfUnit;
  unitFromHeader: boolean;
  shapes: DxfShape[];
  layers: DxfLayerStat[];
  /** Entity types we do not draw (HATCH, DIMENSION, 3DSOLID …) with how many of each were seen. */
  skipped: Record<string, number>;
  entities: number;
  blocks: number;
  truncated: boolean;
  extent: { min: Point; max: Point } | null;
};

export type ParseOptions = { onProgress?: (fraction: number) => void; shouldCancel?: () => boolean; chunk?: number };

const tick = () => new Promise<void>((resolve) => { setTimeout(resolve, 0); });

/** Splits on LF / CRLF / CR without building one giant array up front. */
class Lines {
  pos = 0;
  constructor(private readonly text: string) {}
  next(): string | null {
    const text = this.text;
    if (this.pos >= text.length) return null;
    const start = this.pos;
    let end = start;
    while (end < text.length) {
      const c = text.charCodeAt(end);
      if (c === 10 || c === 13) break;
      end++;
    }
    let after = end;
    if (after < text.length) {
      after += text.charCodeAt(after) === 13 && text.charCodeAt(after + 1) === 10 ? 2 : 1;
    }
    this.pos = after;
    return text.slice(start, end);
  }
}

const numOf = (v: string, fallback = 0) => { const n = Number(v.trim()); return Number.isFinite(n) ? n : fallback; };

export function isBinaryDxf(text: string): boolean {
  if (text.startsWith("AutoCAD Binary DXF")) return true;
  if (text.startsWith("AC10") || text.startsWith("AC1.") || text.startsWith("MC0.")) return true; // a .dwg renamed to .dxf
  const head = text.slice(0, 1024);
  for (let i = 0; i < head.length; i++) if (head.charCodeAt(i) === 0) return true;
  return false;
}

export async function parseDxf(text: string, opts: ParseOptions = {}): Promise<DxfDoc> {
  if (text.length > MAX_CHARS) throw new Error(`ไฟล์ใหญ่เกินไป (${(text.length / 1048576).toFixed(1)} MB) · รองรับไม่เกิน ${MAX_CHARS / 1048576} MB · ลอง PURGE และลบเลเยอร์ที่ไม่ใช้ใน CAD ก่อน`);
  if (isBinaryDxf(text)) throw new Error("ไฟล์นี้เป็น DXF แบบ binary หรือไฟล์ DWG · เปิดใน CAD แล้ว Save As / Export เป็น “ASCII DXF” อีกครั้ง");
  if (!text.includes("SECTION")) throw new Error("อ่านไฟล์นี้เป็น DXF ไม่ได้ · ไม่พบส่วน SECTION · ตรวจว่าไฟล์เป็น DXF จริงและไม่เสียหาย");

  const chunk = opts.chunk ?? 20000;
  const lines = new Lines(text);
  const header: Record<string, number[]> = {};
  const layerNames: string[] = [];
  const entityRecs: Rec[] = [];
  const blockRecs: Rec[] = [];

  let section = "";
  let expectSection = false;
  let headerVar = "";
  let rec: Rec | null = null;
  let entities = 0;
  let truncated = false;
  let ticks = 0;

  const start = (type: string): Rec | null => {
    if (section === "ENTITIES" || section === "BLOCKS") {
      if (entities >= MAX_ENTITIES) { truncated = true; return null; }
      entities++;
      const fresh: Rec = { type, pairs: [] };
      (section === "ENTITIES" ? entityRecs : blockRecs).push(fresh);
      return fresh;
    }
    return section === "TABLES" ? { type, pairs: [] } : null; // only LAYER records matter, and only their name
  };

  for (;;) {
    const codeLine = lines.next();
    if (codeLine === null) break;
    const valueLine = lines.next();
    if (valueLine === null) break;
    if (++ticks % chunk === 0) {
      if (opts.shouldCancel?.()) throw new DxfCancelled();
      opts.onProgress?.(Math.min(0.98, lines.pos / Math.max(1, text.length)));
      await tick();
    }
    const code = Number(codeLine.trim());
    if (!Number.isFinite(code)) continue;
    const value = valueLine.trimEnd();
    const trimmed = value.trim();

    if (code === 0) {
      rec = null;
      if (trimmed === "SECTION") { expectSection = true; continue; }
      if (trimmed === "ENDSEC") { section = ""; headerVar = ""; continue; }
      if (trimmed === "EOF") break;
      rec = start(trimmed);
      continue;
    }
    if (expectSection && code === 2) { section = trimmed; expectSection = false; continue; }
    if (section === "HEADER") {
      if (code === 9) { headerVar = trimmed; header[headerVar] = []; continue; }
      if (headerVar) header[headerVar].push(numOf(value));
      continue;
    }
    if (!rec) continue;
    if (section === "TABLES") { if (rec.type === "LAYER" && code === 2 && trimmed) layerNames.push(trimmed); continue; }
    rec.pairs.push([code, value]);
  }

  opts.onProgress?.(0.99);
  if (opts.shouldCancel?.()) throw new DxfCancelled();
  await tick();

  const blocks = collectBlocks(blockRecs);
  const shapes: DxfShape[] = [];
  const skipped: Record<string, number> = {};
  const ctx: Ctx = { blocks, skipped, budget: { left: MAX_ENTITIES } };
  emit(entityRecs, 0, entityRecs.length, IDENT, 0, "", ctx, shapes);

  const extent = header.$EXTMIN && header.$EXTMAX && header.$EXTMIN.length >= 2 && header.$EXTMAX.length >= 2
    ? { min: [header.$EXTMIN[0], header.$EXTMIN[1]] as Point, max: [header.$EXTMAX[0], header.$EXTMAX[1]] as Point }
    : null;
  const insUnits = header.$INSUNITS?.[0] ?? 0;
  const mapped = insUnitsToUnit(insUnits);
  const layers = layerStats(shapes, layerNames);
  if (!shapes.length && !Object.keys(skipped).length) throw new Error("ไม่พบเส้นหรือข้อความที่นำเข้าได้ในไฟล์นี้ · ลอง EXPLODE บล็อกและ FLATTEN แบบเป็น 2 มิติ แล้ว export ใหม่");

  opts.onProgress?.(1);
  return { unit: mapped ?? guessUnit(shapes, extent), unitFromHeader: mapped !== null, shapes, layers, skipped, entities, blocks: blocks.size, truncated, extent };
}

function insUnitsToUnit(v: number): DxfUnit | null {
  if (v === 1) return "in";
  if (v === 2) return "ft";
  if (v === 4) return "mm";
  if (v === 5) return "cm";
  if (v === 6) return "m";
  return null; // 0 = unitless, everything else (miles, microns, yards …) is not worth guessing at
}

/** A unitless file only tells us how big the numbers are; a building is 10–100 of something. */
function guessUnit(shapes: DxfShape[], extent: { min: Point; max: Point } | null): DxfUnit {
  let span = extent ? Math.max(extent.max[0] - extent.min[0], extent.max[1] - extent.min[1]) : 0;
  if (!(span > 0)) {
    const b = bboxOfShapes(shapes);
    span = b ? Math.max(b.max[0] - b.min[0], b.max[1] - b.min[1]) : 0;
  }
  if (span > 2000) return "mm";
  if (span > 200) return "cm";
  return "m";
}

function layerStats(shapes: DxfShape[], declared: string[]): DxfLayerStat[] {
  const map = new Map<string, DxfLayerStat>();
  const get = (name: string) => {
    let s = map.get(name);
    if (!s) { s = { name, total: 0, polys: 0, closed: 0, texts: 0, kinds: {} }; map.set(name, s); }
    return s;
  };
  for (const name of declared) get(name);
  for (const s of shapes) {
    const stat = get(s.layer);
    stat.total++;
    if (s.k === "poly") { stat.polys++; if (s.closed) stat.closed++; stat.kinds[s.closed ? "เส้นปิด" : "เส้น"] = (stat.kinds[s.closed ? "เส้นปิด" : "เส้น"] ?? 0) + 1; }
    else { stat.texts++; stat.kinds["ข้อความ"] = (stat.kinds["ข้อความ"] ?? 0) + 1; }
  }
  return [...map.values()].filter((s) => s.total > 0).sort((a, b) => b.total - a.total || a.name.localeCompare(b.name));
}

/* ------------------------------------------------------- entities → shapes */

type Mat = { a: number; b: number; c: number; d: number; e: number; f: number };
const IDENT: Mat = { a: 1, b: 0, c: 0, d: 1, e: 0, f: 0 };
const ap = (m: Mat, x: number, y: number): Point => [m.a * x + m.c * y + m.e, m.b * x + m.d * y + m.f];
/** compose(outer, inner): the point goes through `inner` first. */
const compose = (m: Mat, n: Mat): Mat => ({
  a: m.a * n.a + m.c * n.b, b: m.b * n.a + m.d * n.b,
  c: m.a * n.c + m.c * n.d, d: m.b * n.c + m.d * n.d,
  e: m.a * n.e + m.c * n.f + m.e, f: m.b * n.e + m.d * n.f + m.f,
});

type Block = { base: Point; recs: Rec[] };
type Ctx = { blocks: Map<string, Block>; skipped: Record<string, number>; budget: { left: number } };

function collectBlocks(recs: Rec[]): Map<string, Block> {
  const out = new Map<string, Block>();
  let i = 0;
  while (i < recs.length) {
    if (recs[i].type !== "BLOCK") { i++; continue; }
    const head = recs[i];
    const name = str(head, 2, "");
    const base: Point = [num(head, 10, 0), num(head, 20, 0)];
    let j = i + 1;
    while (j < recs.length && recs[j].type !== "ENDBLK" && recs[j].type !== "BLOCK") j++;
    if (name) out.set(name.toUpperCase(), { base, recs: recs.slice(i + 1, j) });
    i = j + (j < recs.length && recs[j].type === "ENDBLK" ? 1 : 0);
  }
  return out;
}

function num(rec: Rec, code: number, fallback: number): number {
  for (const [c, v] of rec.pairs) if (c === code) { const n = Number(v.trim()); return Number.isFinite(n) ? n : fallback; }
  return fallback;
}
function has(rec: Rec, code: number): boolean {
  for (const [c] of rec.pairs) if (c === code) return true;
  return false;
}
function str(rec: Rec, code: number, fallback: string): string {
  for (const [c, v] of rec.pairs) if (c === code) return v.trim();
  return fallback;
}

function emit(recs: Rec[], from: number, to: number, m: Mat, depth: number, inheritLayer: string, ctx: Ctx, out: DxfShape[]): void {
  let i = from;
  while (i < to) {
    const rec = recs[i];
    const layerRaw = str(rec, 8, "0");
    const layer = layerRaw === "0" && inheritLayer ? inheritLayer : layerRaw || "0";
    if (ctx.budget.left <= 0) return;
    switch (rec.type) {
      case "LINE": {
        ctx.budget.left--;
        out.push({ k: "poly", layer, pts: [ap(m, num(rec, 10, 0), num(rec, 20, 0)), ap(m, num(rec, 11, 0), num(rec, 21, 0))], closed: false });
        break;
      }
      case "LWPOLYLINE": {
        ctx.budget.left--;
        const { pts, closed } = lwPolyline(rec);
        if (pts.length >= 2) out.push({ k: "poly", layer, pts: pts.map((p) => ap(m, p[0], p[1])), closed });
        break;
      }
      case "POLYLINE": {
        ctx.budget.left--;
        const flags = num(rec, 70, 0);
        if (flags & 16 || flags & 64) { bump(ctx, "POLYLINE (3D mesh)"); i = skipToSeqEnd(recs, i, to); continue; }
        const verts: { p: Point; bulge: number }[] = [];
        let j = i + 1;
        for (; j < to && recs[j].type !== "SEQEND"; j++) {
          if (recs[j].type !== "VERTEX") break;
          verts.push({ p: [num(recs[j], 10, 0), num(recs[j], 20, 0)], bulge: num(recs[j], 42, 0) });
        }
        i = j < to && recs[j].type === "SEQEND" ? j + 1 : j;
        const pts = expandBulges(verts, (flags & 1) !== 0);
        if (pts.length >= 2) out.push({ k: "poly", layer, pts: pts.map((p) => ap(m, p[0], p[1])), closed: (flags & 1) !== 0 });
        continue;
      }
      case "ARC": {
        ctx.budget.left--;
        const cx = num(rec, 10, 0), cy = num(rec, 20, 0), r = num(rec, 40, 0);
        const a0 = (num(rec, 50, 0) * Math.PI) / 180;
        let a1 = (num(rec, 51, 0) * Math.PI) / 180;
        while (a1 <= a0) a1 += Math.PI * 2;
        if (r > 0) out.push({ k: "poly", layer, pts: sampleArc(cx, cy, r, a0, a1 - a0).map((p) => ap(m, p[0], p[1])), closed: false });
        break;
      }
      case "CIRCLE": {
        ctx.budget.left--;
        const cx = num(rec, 10, 0), cy = num(rec, 20, 0), r = num(rec, 40, 0);
        if (r > 0) {
          const pts = sampleArc(cx, cy, r, 0, Math.PI * 2);
          pts.pop(); // a closed ring must not repeat its first point
          out.push({ k: "poly", layer, pts: pts.map((p) => ap(m, p[0], p[1])), closed: true });
        }
        break;
      }
      case "ELLIPSE": {
        ctx.budget.left--;
        const cx = num(rec, 10, 0), cy = num(rec, 20, 0);
        const mx = num(rec, 11, 0), my = num(rec, 21, 0), ratio = num(rec, 40, 1);
        const t0 = num(rec, 41, 0);
        let t1 = num(rec, 42, Math.PI * 2);
        while (t1 <= t0) t1 += Math.PI * 2;
        const full = t1 - t0 >= Math.PI * 2 - 1e-9;
        const steps = Math.max(8, Math.round((ARC_STEPS * (t1 - t0)) / (Math.PI * 2)));
        const pts: Point[] = [];
        for (let s = 0; s <= steps; s++) {
          const t = t0 + ((t1 - t0) * s) / steps;
          const ct = Math.cos(t), st = Math.sin(t);
          pts.push(ap(m, cx + mx * ct - my * ratio * st, cy + my * ct + mx * ratio * st));
        }
        if (full) pts.pop();
        if (pts.length >= 2) out.push({ k: "poly", layer, pts, closed: full });
        break;
      }
      case "SPLINE": {
        ctx.budget.left--;
        const fit: Point[] = [], ctrl: Point[] = [];
        let fx: number | null = null, cxv: number | null = null;
        for (const [c, v] of rec.pairs) {
          const n = Number(v.trim());
          if (c === 11) fx = n;
          else if (c === 21 && fx !== null) { fit.push([fx, n]); fx = null; }
          else if (c === 10) cxv = n;
          else if (c === 20 && cxv !== null) { ctrl.push([cxv, n]); cxv = null; }
        }
        const pts = fit.length >= 2 ? fit : ctrl;
        const closed = (num(rec, 70, 0) & 1) !== 0;
        if (pts.length >= 2) out.push({ k: "poly", layer, pts: pts.map((p) => ap(m, p[0], p[1])), closed });
        break;
      }
      case "TEXT":
      case "MTEXT": {
        ctx.budget.left--;
        const raw = rec.type === "MTEXT" ? rec.pairs.filter(([c]) => c === 3).map(([, v]) => v).join("") + str(rec, 1, "") : str(rec, 1, "");
        const text = cleanText(raw);
        const aligned = rec.type === "TEXT" && has(rec, 11) && (num(rec, 72, 0) !== 0 || num(rec, 73, 0) !== 0);
        const at = aligned ? ap(m, num(rec, 11, 0), num(rec, 21, 0)) : ap(m, num(rec, 10, 0), num(rec, 20, 0));
        const scale = Math.hypot(m.a, m.b) || 1;
        if (text) out.push({ k: "text", layer, at, text, h: Math.abs(num(rec, 40, 0.25)) * scale });
        break;
      }
      case "INSERT": {
        const name = str(rec, 2, "").toUpperCase();
        const block = ctx.blocks.get(name);
        if (!block || depth >= INSERT_DEPTH) { bump(ctx, block ? "INSERT (ซ้อนลึกเกิน)" : "INSERT (ไม่พบบล็อก)"); break; }
        const sx = num(rec, 41, 1) || 1, sy = num(rec, 42, 1) || 1;
        const rot = (num(rec, 50, 0) * Math.PI) / 180;
        const cols = Math.max(1, Math.min(200, Math.round(num(rec, 70, 1)))), rows = Math.max(1, Math.min(200, Math.round(num(rec, 71, 1))));
        const colSp = num(rec, 44, 0), rowSp = num(rec, 45, 0);
        const ix = num(rec, 10, 0), iy = num(rec, 20, 0);
        const cos = Math.cos(rot), sin = Math.sin(rot);
        for (let r = 0; r < rows; r++) for (let c = 0; c < cols; c++) {
          if (ctx.budget.left <= 0) break;
          const local: Mat = compose(
            compose({ a: cos, b: sin, c: -sin, d: cos, e: ix, f: iy }, { a: 1, b: 0, c: 0, d: 1, e: c * colSp, f: r * rowSp }),
            { a: sx, b: 0, c: 0, d: sy, e: -block.base[0] * sx, f: -block.base[1] * sy },
          );
          emit(block.recs, 0, block.recs.length, compose(m, local), depth + 1, layer, ctx, out);
        }
        break;
      }
      case "VERTEX":
      case "SEQEND":
      case "TABLE":
      case "BLOCK":
      case "ENDBLK":
      case "ATTDEF":
      case "ATTRIB":
      case "VIEWPORT":
        break;
      default:
        bump(ctx, rec.type);
        break;
    }
    i++;
  }
}

const bump = (ctx: Ctx, type: string) => { ctx.skipped[type] = (ctx.skipped[type] ?? 0) + 1; };

function skipToSeqEnd(recs: Rec[], i: number, to: number): number {
  let j = i + 1;
  while (j < to && recs[j].type !== "SEQEND") j++;
  return j < to ? j + 1 : j;
}

function lwPolyline(rec: Rec): { pts: Point[]; closed: boolean } {
  const verts: { p: Point; bulge: number }[] = [];
  let cur: { p: Point; bulge: number } | null = null;
  for (const [c, v] of rec.pairs) {
    const n = Number(v.trim());
    if (c === 10) { cur = { p: [Number.isFinite(n) ? n : 0, 0], bulge: 0 }; verts.push(cur); }
    else if (c === 20 && cur) cur.p[1] = Number.isFinite(n) ? n : 0;
    else if (c === 42 && cur) cur.bulge = Number.isFinite(n) ? n : 0;
  }
  const closed = (num(rec, 70, 0) & 1) !== 0;
  return { pts: expandBulges(verts, closed), closed };
}

/** A bulge is tan(θ/4) of the arc that replaces the straight segment leaving that vertex. */
function expandBulges(verts: { p: Point; bulge: number }[], closed: boolean): Point[] {
  if (verts.length < 2) return verts.map((v) => v.p);
  const out: Point[] = [verts[0].p];
  const last = closed ? verts.length : verts.length - 1;
  for (let i = 0; i < last; i++) {
    const a = verts[i], b = verts[(i + 1) % verts.length];
    if (a.bulge) for (const p of arcThrough(a.p, b.p, a.bulge)) out.push(p);
    if (i < verts.length - 1) out.push(b.p);
  }
  return out;
}

function arcThrough(p0: Point, p1: Point, bulge: number): Point[] {
  const dx = p1[0] - p0[0], dy = p1[1] - p0[1], d = Math.hypot(dx, dy);
  const theta = 4 * Math.atan(bulge);
  if (!(d > 0) || !Number.isFinite(theta) || Math.abs(Math.sin(theta / 2)) < 1e-9) return [];
  const r = d / (2 * Math.sin(theta / 2));
  const h = r * Math.cos(theta / 2);
  const cx = (p0[0] + p1[0]) / 2 - (dy / d) * h, cy = (p0[1] + p1[1]) / 2 + (dx / d) * h;
  const a0 = Math.atan2(p0[1] - cy, p0[0] - cx);
  const steps = Math.max(2, Math.min(ARC_STEPS, Math.ceil((Math.abs(theta) / (Math.PI * 2)) * ARC_STEPS)));
  const out: Point[] = [];
  for (let s = 1; s < steps; s++) {
    const a = a0 + (theta * s) / steps;
    out.push([cx + Math.abs(r) * Math.cos(a), cy + Math.abs(r) * Math.sin(a)]);
  }
  return out;
}

function sampleArc(cx: number, cy: number, r: number, a0: number, sweep: number): Point[] {
  const steps = Math.max(2, Math.min(ARC_STEPS * 2, Math.ceil((Math.abs(sweep) / (Math.PI * 2)) * ARC_STEPS)));
  const out: Point[] = [];
  for (let s = 0; s <= steps; s++) { const a = a0 + (sweep * s) / steps; out.push([cx + r * Math.cos(a), cy + r * Math.sin(a)]); }
  return out;
}

/** Strips MTEXT formatting ({\fArial|b0;…}, \P, \H2x;, \A1;) and the %%d / %%c / %%p escapes of TEXT. */
export function cleanText(raw: string): string {
  let out = "";
  for (let i = 0; i < raw.length; i++) {
    const ch = raw[i];
    if (ch === "\\") {
      const n = raw[i + 1] ?? "";
      if (n === "P" || n === "X" || n === "~") { out += " "; i++; continue; }
      if (n === "\\" || n === "{" || n === "}") { out += n; i++; continue; }
      if ("LlOoKk".includes(n)) { i++; continue; } // underline / overline / strike toggles carry no argument
      if ((n >= "A" && n <= "Z") || (n >= "a" && n <= "z")) {
        let j = i + 2;
        while (j < raw.length && raw[j] !== ";" && raw[j] !== "\\" && raw[j] !== "{" && raw[j] !== "}") j++;
        i = raw[j] === ";" ? j : j - 1;
        continue;
      }
      i++;
      continue;
    }
    if (ch === "{" || ch === "}") continue;
    out += ch;
  }
  out = out.split("%%d").join("°").split("%%D").join("°").split("%%c").join("Ø").split("%%C").join("Ø").split("%%p").join("±").split("%%P").join("±").split("%%%").join("%");
  let squeezed = "";
  let space = false;
  for (const ch of out) {
    const isSpace = ch === " " || ch === "\t" || ch === "\n" || ch === "\r";
    if (isSpace) { space = true; continue; }
    if (space && squeezed) squeezed += " ";
    space = false;
    squeezed += ch;
  }
  return squeezed;
}

/* ----------------------------------------------------------- geometry build */

export type LayerTarget = "wall" | "zone" | "skip";

export type BuildOptions = {
  unit: DxfUnit;
  targets: Record<string, LayerTarget>;
  thickness: number;
  /** Douglas–Peucker tolerance in metres. */
  tolerance: number;
  /** Polylines shorter than this (metres) are dropped; 0 keeps everything. */
  minLength: number;
  margin?: number;
};

export type BuiltWall = { id: string; points: Point[]; thickness: number; closed?: boolean };
export type BuiltZone = { id: string; name: string; kind: string; color: string; points: Point[] };
export type BuiltItem = { id: string; type: string; x: number; y: number; w: number; h: number; rot: number; text?: string };

export type BuildResult = {
  walls: BuiltWall[];
  zones: BuiltZone[];
  items: BuiltItem[];
  width: number;
  depth: number;
  bytes: number;
  points: number;
  dropped: { short: number; duplicate: number; tiny: number; zoneTooBig: number; overflow: number };
  warnings: string[];
  overLimit: boolean;
};

const ZONE_COLORS = ["mint", "blue", "amber", "coral", "violet", "slate"];
const SKIP_HINTS = ["DIM", "ANNO", "HATCH", "TEXT", "FURN", "DEFPOINTS", "NOTE", "GRID", "TITLE"];
const ZONE_HINTS = ["ROOM", "ZONE", "AREA", "SPACE", "ห้อง", "โซน", "พื้นที่"];

export const looksLikeZoneLayer = (name: string) => { const n = name.toUpperCase(); return ZONE_HINTS.some((h) => n.includes(h.toUpperCase())); };
export const looksSkippable = (name: string) => { const n = name.toUpperCase(); return SKIP_HINTS.some((h) => n.includes(h)); };
/** What the dialog pre-selects for a layer: annotation layers off, room-ish layers as zones, the rest as walls. */
export function suggestTarget(layer: DxfLayerStat): LayerTarget {
  if (looksSkippable(layer.name)) return "skip";
  if (looksLikeZoneLayer(layer.name) && layer.closed > 0) return "zone";
  return "wall"; // a text-only layer imports as labels; the annotation names above already caught the noisy ones
}

const round2 = (v: number) => Math.round(v * 100) / 100;
const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));
const dist2 = (a: Point, b: Point) => Math.hypot(a[0] - b[0], a[1] - b[1]);

function bboxOfShapes(shapes: DxfShape[]): { min: Point; max: Point } | null {
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
  for (const s of shapes) {
    const pts = s.k === "poly" ? s.pts : [s.at];
    for (const [x, y] of pts) {
      if (!Number.isFinite(x) || !Number.isFinite(y)) continue;
      if (x < minX) minX = x; if (y < minY) minY = y;
      if (x > maxX) maxX = x; if (y > maxY) maxY = y;
    }
  }
  return minX <= maxX ? { min: [minX, minY], max: [maxX, maxY] } : null;
}

function polyLength(pts: Point[], closed: boolean): number {
  let total = 0;
  for (let i = 1; i < pts.length; i++) total += dist2(pts[i - 1], pts[i]);
  if (closed && pts.length > 2) total += dist2(pts[pts.length - 1], pts[0]);
  return total;
}

function segDistance(p: Point, a: Point, b: Point): number {
  const dx = b[0] - a[0], dy = b[1] - a[1];
  const len2 = dx * dx + dy * dy;
  if (len2 < 1e-18) return dist2(p, a);
  const t = clamp(((p[0] - a[0]) * dx + (p[1] - a[1]) * dy) / len2, 0, 1);
  return Math.hypot(p[0] - (a[0] + t * dx), p[1] - (a[1] + t * dy));
}

/** Douglas–Peucker, iterative so a 200k-point spline cannot blow the stack. */
export function simplify(pts: Point[], tolerance: number): Point[] {
  if (pts.length <= 2 || !(tolerance > 0)) return pts;
  const keep = new Uint8Array(pts.length);
  keep[0] = 1; keep[pts.length - 1] = 1;
  const stack: number[] = [0, pts.length - 1];
  while (stack.length) {
    const j = stack.pop()!, i = stack.pop()!;
    if (j - i < 2) continue;
    let best = -1, bestD = tolerance;
    for (let k = i + 1; k < j; k++) { const d = segDistance(pts[k], pts[i], pts[j]); if (d > bestD) { bestD = d; best = k; } }
    if (best > 0) { keep[best] = 1; stack.push(i, best, best, j); }
  }
  const out: Point[] = [];
  for (let i = 0; i < pts.length; i++) if (keep[i]) out.push(pts[i]);
  return out;
}

const simplifyRing = (pts: Point[], tolerance: number): Point[] => {
  if (pts.length <= 3) return pts;
  const open = simplify([...pts, pts[0]], tolerance);
  open.pop();
  return open;
};

function dedupe(pts: Point[], eps: number): Point[] {
  const out: Point[] = [];
  for (const p of pts) if (!out.length || dist2(out[out.length - 1], p) > eps) out.push(p);
  return out;
}

type Poly = { layer: string; pts: Point[]; closed: boolean };

/** Chains open polylines whose endpoints meet, so a wall exported as 40 separate LINEs becomes one wall. */
export function joinPolys(items: Poly[], eps: number, maxPoints: number): Poly[] {
  const open: Poly[] = [], out: Poly[] = [];
  for (const p of items) (p.closed || p.pts.length < 2 ? out : open).push(p);
  const cell = (p: Point, ox: number, oy: number) => `${Math.round(p[0] / eps) + ox},${Math.round(p[1] / eps) + oy}`;
  const grid = new Map<string, number[]>();
  const add = (p: Point, ref: number) => { const k = cell(p, 0, 0); const list = grid.get(k); if (list) list.push(ref); else grid.set(k, [ref]); };
  open.forEach((p, i) => { add(p.pts[0], i * 2); add(p.pts[p.pts.length - 1], i * 2 + 1); });
  const used = new Uint8Array(open.length);
  for (let i = 0; i < open.length; i++) {
    if (used[i]) continue;
    used[i] = 1;
    let pts = open[i].pts.slice();
    const layer = open[i].layer;
    for (let dir = 0; dir < 2; dir++) {
      for (;;) {
        const tip = dir === 0 ? pts[pts.length - 1] : pts[0];
        let seq: Point[] | null = null;
        let hit = -1;
        for (let ox = -1; ox <= 1 && hit < 0; ox++) for (let oy = -1; oy <= 1 && hit < 0; oy++) {
          for (const ref of grid.get(cell(tip, ox, oy)) ?? []) {
            const j = ref >> 1;
            if (used[j] || open[j].layer !== layer) continue;
            const other = open[j].pts;
            const end = (ref & 1) === 1 ? other[other.length - 1] : other[0];
            if (dist2(end, tip) > eps) continue;
            if (pts.length + other.length - 1 > maxPoints) continue;
            seq = (ref & 1) === 1 ? other.slice().reverse() : other.slice();
            hit = j;
            break;
          }
        }
        if (hit < 0 || !seq) break;
        used[hit] = 1;
        const tail = seq.slice(1);
        if (dir === 0) pts = pts.concat(tail);
        else { tail.reverse(); pts = tail.concat(pts); }
      }
    }
    let closed = false;
    if (pts.length > 3 && dist2(pts[0], pts[pts.length - 1]) <= eps) { pts.pop(); closed = true; }
    out.push({ layer, pts, closed });
  }
  return out;
}

/** Turns a parsed drawing into a floor plan layout: metres, y flipped, joined, simplified and rounded. */
export function buildImport(doc: DxfDoc, opts: BuildOptions): BuildResult {
  const scale = UNIT_M[opts.unit];
  const margin = opts.margin ?? 1;
  const warnings: string[] = [];
  const dropped = { short: 0, duplicate: 0, tiny: 0, zoneTooBig: 0, overflow: 0 };

  const kept = doc.shapes.filter((s) => (opts.targets[s.layer] ?? "skip") !== "skip");
  const box = bboxOfShapes(kept);
  if (!box) return { walls: [], zones: [], items: [], width: 0, depth: 0, bytes: 2, points: 0, dropped, warnings: ["ยังไม่ได้เลือกเลเยอร์ที่จะนำเข้า"], overLimit: false };

  const minX = box.min[0] * scale, maxY = box.max[1] * scale;
  const width = (box.max[0] - box.min[0]) * scale, depth = (box.max[1] - box.min[1]) * scale;
  // DXF's y grows up; the floor plan's grows down, so mirror about the drawing's top edge.
  const place = (p: Point): Point => [p[0] * scale - minX + margin, maxY - p[1] * scale + margin];

  const wallPolys: Poly[] = [], zonePolys: Poly[] = [], texts: { layer: string; at: Point; text: string; h: number }[] = [];
  for (const s of kept) {
    const target = opts.targets[s.layer] as LayerTarget;
    if (s.k === "text") { texts.push({ layer: s.layer, at: place(s.at), text: s.text.slice(0, 128), h: Math.max(0.2, s.h * scale) }); continue; }
    const pts = dedupe(s.pts.map(place), JOIN_EPS);
    if (pts.length < 2) { dropped.tiny++; continue; }
    const closed = s.closed || (pts.length > 2 && dist2(pts[0], pts[pts.length - 1]) <= JOIN_EPS);
    if (closed && pts.length > 2 && dist2(pts[0], pts[pts.length - 1]) <= JOIN_EPS) pts.pop();
    (target === "zone" && closed ? zonePolys : wallPolys).push({ layer: s.layer, pts, closed });
  }

  // Deterministic ids, unique inside one import. The caller re-stamps any that clash with the floor's own.
  let seq = 0;
  const nextId = (prefix: string) => `${prefix}${(seq++).toString(36)}`;

  // Walls -------------------------------------------------------------------
  const joined = joinPolys(wallPolys, JOIN_EPS, MAX_WALL_POINTS);
  const seen = new Set<string>();
  const walls: BuiltWall[] = [];
  let points = 0;
  for (const poly of joined) {
    const thinned = dedupe((poly.closed ? simplifyRing(poly.pts, opts.tolerance) : simplify(poly.pts, opts.tolerance)).map((p) => [round2(p[0]), round2(p[1])] as Point), 0.005);
    if (thinned.length < 2) { dropped.tiny++; continue; }
    const len = polyLength(thinned, poly.closed);
    if (len < Math.max(0.01, opts.minLength)) { dropped.short++; continue; }
    const key = fingerprint(thinned);
    if (seen.has(key)) { dropped.duplicate++; continue; }
    seen.add(key);
    for (const part of splitPoints(thinned, poly.closed, MAX_WALL_POINTS)) {
      walls.push({ id: nextId("w"), points: part.pts, thickness: opts.thickness, ...(part.closed ? { closed: true } : {}) });
      points += part.pts.length;
    }
  }

  // Zones -------------------------------------------------------------------
  const zones: BuiltZone[] = [];
  for (const poly of zonePolys) {
    let pts = dedupe(simplifyRing(poly.pts, opts.tolerance).map((p) => [round2(p[0]), round2(p[1])] as Point), 0.005);
    for (let attempt = 1; pts.length > MAX_ZONE_POINTS && attempt <= 8; attempt++) pts = dedupe(simplifyRing(pts, opts.tolerance * Math.pow(2, attempt)), 0.005);
    if (pts.length < 3) { dropped.tiny++; continue; }
    if (pts.length > MAX_ZONE_POINTS) { dropped.zoneTooBig++; continue; }
    if (ringArea(pts) < 0.25) { dropped.tiny++; continue; }
    const inside = texts.filter((t) => pointInRing(t.at, pts));
    const c = ringCentroid(pts);
    inside.sort((a, b) => dist2(a.at, c) - dist2(b.at, c));
    zones.push({ id: nextId("z"), name: (inside[0]?.text ?? `โซน ${zones.length + 1}`).slice(0, 80), kind: "room", color: ZONE_COLORS[zones.length % ZONE_COLORS.length], points: pts });
    points += pts.length;
  }

  // Labels ------------------------------------------------------------------
  const zoneLayers = new Set(Object.entries(opts.targets).filter(([, t]) => t === "zone").map(([l]) => l));
  const items: BuiltItem[] = [];
  for (const t of texts) {
    if (zoneLayers.has(t.layer)) continue; // already used as a zone name
    const h = clamp(t.h, 0.2, 2);
    const w = clamp(t.text.length * h * 0.62, 0.4, 20);
    items.push({ id: nextId("i"), type: "label", x: round2(t.at[0] - w / 2), y: round2(t.at[1] - h / 2), w: round2(w), h: round2(h), rot: 0, text: t.text });
  }

  const bytes = byteLength(JSON.stringify({ walls, zones, items }));
  let overLimit = false;
  if (walls.length > MAX_WALLS) { warnings.push(`ผนัง ${walls.length} เส้น เกินขีดจำกัด ${MAX_WALLS} เส้นต่อชั้น`); overLimit = true; }
  if (zones.length > MAX_ZONES) { warnings.push(`โซน ${zones.length} เกินขีดจำกัด ${MAX_ZONES} โซนต่อชั้น`); overLimit = true; }
  if (items.length > MAX_ITEMS) { warnings.push(`ข้อความ ${items.length} ชิ้น เกินขีดจำกัด ${MAX_ITEMS} ชิ้นต่อชั้น`); overLimit = true; }
  if (bytes > MAX_LAYOUT_BYTES) { warnings.push(`ขนาดข้อมูล ${(bytes / 1024).toFixed(0)} KB เกิน ${MAX_LAYOUT_BYTES / 1024} KB ที่บันทึกได้`); overLimit = true; }
  if (Math.max(width, depth) + margin * 2 > MAX_COORD) { warnings.push(`แบบกว้าง ${Math.round(Math.max(width, depth))} ม. · เกินพิกัดที่เก็บได้ (${MAX_COORD} ม.) · ตรวจหน่วยที่เลือกอีกครั้ง`); overLimit = true; }
  if (dropped.zoneTooBig) warnings.push(`เส้นปิด ${dropped.zoneTooBig} รูปมีมุมเกิน ${MAX_ZONE_POINTS} จุด จึงข้ามไป`);
  if (doc.truncated) warnings.push(`ไฟล์มี entity เกิน ${MAX_ENTITIES.toLocaleString()} ชิ้น · อ่านเท่าที่รับได้`);

  return { walls, zones, items, width, depth, bytes, points, dropped, warnings, overLimit };
}

function splitPoints(pts: Point[], closed: boolean, max: number): { pts: Point[]; closed: boolean }[] {
  if (pts.length <= max) return [{ pts, closed }];
  const ring = closed ? [...pts, pts[0]] : pts;
  const out: { pts: Point[]; closed: boolean }[] = [];
  for (let s = 0; s < ring.length - 1; s += max - 1) out.push({ pts: ring.slice(s, s + max), closed: false });
  return out.filter((p) => p.pts.length >= 2);
}

function fingerprint(pts: Point[]): string {
  const forward = pts.map((p) => `${p[0]},${p[1]}`).join(";");
  const backward = pts.slice().reverse().map((p) => `${p[0]},${p[1]}`).join(";");
  return forward < backward ? forward : backward;
}

function ringArea(pts: Point[]): number {
  let a = 0;
  for (let i = 0; i < pts.length; i++) { const j = (i + 1) % pts.length; a += pts[i][0] * pts[j][1] - pts[j][0] * pts[i][1]; }
  return Math.abs(a) / 2;
}
function ringCentroid(pts: Point[]): Point {
  let a = 0, cx = 0, cy = 0;
  for (let i = 0; i < pts.length; i++) {
    const j = (i + 1) % pts.length, f = pts[i][0] * pts[j][1] - pts[j][0] * pts[i][1];
    a += f; cx += (pts[i][0] + pts[j][0]) * f; cy += (pts[i][1] + pts[j][1]) * f;
  }
  if (Math.abs(a) < 1e-9) return [pts.reduce((s, p) => s + p[0], 0) / pts.length, pts.reduce((s, p) => s + p[1], 0) / pts.length];
  return [cx / (3 * a), cy / (3 * a)];
}
function pointInRing(p: Point, ring: Point[]): boolean {
  let inside = false;
  for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
    const [xi, yi] = ring[i], [xj, yj] = ring[j];
    if (yi > p[1] !== yj > p[1] && p[0] < ((xj - xi) * (p[1] - yi)) / (yj - yi) + xi) inside = !inside;
  }
  return inside;
}
function byteLength(s: string): number {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.codePointAt(i)!;
    n += c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4;
    if (c >= 0x10000) i++;
  }
  return n;
}
