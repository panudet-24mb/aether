// A furnished hospital, generated from the floor plan (docs/platform/digital-twin.md, "Lifelike hospital"): each
// zone gets furniture for what it is (ward beds with monitors, a reception desk and waiting chairs, pharmacy
// shelves, server racks …), placed deterministically from the zone's id, inside the zone, clear of walls, doors and
// placed devices. Plan items the operator drew (beds, desks, stairs, lifts, racks) are drawn as furniture too.
// Procedural only: no downloaded models.

import * as THREE from "three";
import type { Draft, Item, Zone } from "../../../floorplan/model";
import type { TwinTheme } from "../../theme";
import { bbox, edgeDistance, hash, inside, overlaps, rng, type P } from "./geo";
import { Kit, type Frame } from "./kit";

export type ZoneType = "icu" | "er" | "ward" | "lobby" | "nurse" | "lab" | "pharmacy" | "cold" | "server" | "corridor" | "storage" | "office";

/** What a zone is, from its kind and name (Thai or English). */
export function zoneType(z: Pick<Zone, "kind" | "name">): ZoneType {
  const n = z.name.toLowerCase();
  if (/icu|ไอซียู|วิกฤต/.test(n)) return "icu";
  if (/\ber\b|ฉุกเฉิน|emergency/.test(n)) return "er";
  if (/เซิร์ฟเวอร์|server|data ?cent|ดาต้า/.test(n)) return "server";
  if (/เย็น|cold|freez|วัคซีน|vaccine/.test(n)) return "cold";
  if (/แล็บ|lab|ปฏิบัติการ/.test(n)) return "lab";
  if (/ยา|pharm|drug/.test(n)) return "pharmacy";
  if (/พยาบาล|nurse|เคาน์เตอร์|counter|station/.test(n)) return "nurse";
  if (/โถง|ต้อนรับ|lobby|reception|รอ|waiting/.test(n)) return "lobby";
  if (z.kind === "corridor" || /ทางเดิน|corridor|hall/.test(n)) return "corridor";
  if (z.kind === "ward" || /หอผู้ป่วย|ward|ห้องพัก/.test(n)) return "ward";
  if (z.kind === "storage" || /คลัง|เก็บ|store/.test(n)) return "storage";
  return "office";
}

type Fp = { x: number; y: number; w: number; d: number };
export type ZoneInterior = { zoneIndex: number; type: ZoneType; anchors: P[]; blocked: Fp[] };

/** Colours: hospital neutrals, the app's accent for upholstery and screens. */
function palette(theme: TwinTheme) {
  const c = (n: number) => new THREE.Color(n);
  return {
    white: c(0xc9d1d0), sheet: c(0xd6dcdb), metal: c(0x86939a), dark: c(0x1c2529), steel: c(0xa3aeb1), wood: c(0x9a7f62),
    fabric: c(theme.accent).lerp(c(theme.paper), 0.62), blue: c(theme.blue).lerp(c(theme.paper), 0.45), leaf: c(0x4f8f5f), pot: c(0x6f6056),
    screen: c(theme.accent), info: c(theme.blue), warm: c(0xfff1d6), red: c(theme.danger),
  };
}
type Pal = ReturnType<typeof palette>;

/** Rotated rectangle → its axis-aligned footprint (plan items may be turned by any angle). */
function itemFp(it: Item): Fp {
  const r = (it.rot * Math.PI) / 180, c = Math.abs(Math.cos(r)), s = Math.abs(Math.sin(r));
  return { x: it.x + it.w / 2, y: it.y + it.h / 2, w: it.w * c + it.h * s, d: it.w * s + it.h * c };
}

type Ctx = {
  kit: Kit; pal: Pal; origin: THREE.Vector3;
  doors: P[]; devices: P[]; placed: Fp[];
};

/** The plan point (x, y) of a floor as a world position at height h. */
const at = (ctx: Ctx, x: number, y: number, h = 0) => new THREE.Vector3(ctx.origin.x + x, ctx.origin.y + h, ctx.origin.z + y);

/** A footprint is free: inside the zone with a margin, clear of other furniture, doors (1 m) and devices (0.45 m). */
function free(ctx: Ctx, poly: P[], fp: Fp, margin = 0.15): boolean {
  const corners: P[] = [[fp.x - fp.w / 2, fp.y - fp.d / 2], [fp.x + fp.w / 2, fp.y - fp.d / 2], [fp.x + fp.w / 2, fp.y + fp.d / 2], [fp.x - fp.w / 2, fp.y + fp.d / 2]];
  if (!corners.every((p) => inside(p, poly) && edgeDistance(p, poly) >= margin)) return false;
  if (ctx.placed.some((o) => overlaps(o, fp, 0.2))) return false;
  const near = (p: P, r: number) => Math.max(Math.abs(p[0] - fp.x) - fp.w / 2, Math.abs(p[1] - fp.y) - fp.d / 2) < r;
  return !ctx.doors.some((p) => near(p, 1.0)) && !ctx.devices.some((p) => near(p, 0.45));
}

/**
 * Spots along the zone's walls for something `w` wide and `d` deep, facing into the room. Only straight, axis-aligned
 * edges are used (rooms in plans almost always are). `rot` turns the item's local +z (its front) into the room.
 */
function wallSpots(poly: P[], w: number, d: number, spacing: number): { x: number; y: number; rot: number; fp: Fp }[] {
  const out: { x: number; y: number; rot: number; fp: Fp }[] = [];
  const c = poly.reduce((s, p) => [s[0] + p[0] / poly.length, s[1] + p[1] / poly.length], [0, 0]);
  for (let i = 0; i < poly.length; i++) {
    const a = poly[i], b = poly[(i + 1) % poly.length];
    const horizontal = Math.abs(a[1] - b[1]) < 0.01, vertical = Math.abs(a[0] - b[0]) < 0.01;
    if (!horizontal && !vertical) continue;
    const len = horizontal ? Math.abs(b[0] - a[0]) : Math.abs(b[1] - a[1]);
    const n = Math.floor((len - 0.6) / spacing);
    if (n < 1) continue;
    const start = (len - (n - 1) * spacing) / 2;
    for (let k = 0; k < n; k++) {
      const s = start + k * spacing;
      if (horizontal) {
        const x = Math.min(a[0], b[0]) + s;
        const intoSouth = c[1] > a[1];
        const y = a[1] + (intoSouth ? 1 : -1) * (d / 2 + 0.12);
        out.push({ x, y, rot: intoSouth ? 0 : Math.PI, fp: { x, y, w, d } });
      } else {
        const y = Math.min(a[1], b[1]) + s;
        const intoEast = c[0] > a[0];
        const x = a[0] + (intoEast ? 1 : -1) * (d / 2 + 0.12);
        out.push({ x, y, rot: intoEast ? Math.PI / 2 : -Math.PI / 2, fp: { x, y, w: d, d: w } });
      }
    }
  }
  return out;
}

// ---- furniture ---------------------------------------------------------------------------------------------------

function bed(f: Frame, p: Pal, icu: boolean) {
  // Local frame: head at -z (against the wall), foot at +z; 0.95 × 2.1 m.
  f.add("box", 0, 0.42, 0, 0.92, 0.16, 2.0, p.metal);
  f.add("box", 0, 0.55, 0.05, 0.86, 0.12, 1.86, p.sheet);
  f.add("box", 0, 0.66, -0.72, 0.6, 0.1, 0.34, p.white);
  f.add("box", 0, 0.62, 0.35, 0.84, 0.05, 1.05, icu ? p.blue : p.fabric);
  f.add("box", 0, 0.75, -1.0, 0.92, 0.5, 0.05, p.steel);
  f.add("box", 0, 0.62, 1.0, 0.92, 0.3, 0.05, p.steel);
  for (const s of [-1, 1]) {
    f.add("box", s * 0.47, 0.72, 0.05, 0.03, 0.08, 1.2, p.steel);
    for (const z of [-0.85, 0.85]) f.add("cyl", s * 0.4, 0.17, z, 0.05, 0.34, 0.05, p.dark);
  }
}

function bedside(f: Frame, p: Pal, icu: boolean, side: number) {
  // Next to the bed's head: a monitor on a stand (ICU/ER) or a cabinet, an IV pole, a visitor chair.
  if (icu) {
    f.add("cyl", side * 0.85, 0.65, -0.75, 0.05, 1.3, 0.05, p.metal);
    f.add("box", side * 0.85, 1.42, -0.72, 0.42, 0.32, 0.08, p.dark);
    f.add("glow", side * 0.85, 1.42, -0.675, 0.36, 0.25, 0.01, p.screen);
  } else {
    f.add("box", side * 0.8, 0.38, -0.75, 0.45, 0.76, 0.45, p.white);
    f.add("box", side * 0.8, 0.77, -0.75, 0.47, 0.03, 0.47, p.wood);
  }
  f.add("cyl", -side * 0.65, 0.95, -0.85, 0.025, 1.9, 0.025, p.steel);
  f.add("box", -side * 0.65, 1.75, -0.85, 0.14, 0.2, 0.05, p.info);
  chair(f, side * 0.95, 0.35, side > 0 ? -Math.PI / 2 : Math.PI / 2, p);
}

function chair(f: Frame, x: number, z: number, rot: number, p: Pal) {
  const c = Math.cos(rot), s = Math.sin(rot);
  const loc = (lx: number, lz: number): [number, number] => [x + lx * c + lz * s, z - lx * s + lz * c];
  const [sx, sz] = loc(0, 0), [bx, bz] = loc(0, -0.22);
  f.add("box", sx, 0.45, sz, 0.46, 0.08, 0.46, p.fabric);
  f.add("box", bx, 0.72, bz, Math.abs(c) * 0.46 + Math.abs(s) * 0.06, 0.5, Math.abs(s) * 0.46 + Math.abs(c) * 0.06, p.fabric);
  f.add("cyl", sx, 0.22, sz, 0.05, 0.44, 0.05, p.metal);
}

function desk(f: Frame, p: Pal, w = 1.4) {
  f.add("box", 0, 0.74, 0, w, 0.04, 0.7, p.wood);
  for (const sx of [-1, 1]) f.add("box", sx * (w / 2 - 0.05), 0.37, 0, 0.05, 0.72, 0.62, p.metal);
  f.add("box", 0, 1.0, -0.22, 0.55, 0.34, 0.03, p.dark);
  f.add("glow", 0, 1.0, -0.2, 0.5, 0.28, 0.005, p.info);
  chair(f, 0, 0.6, Math.PI, p);
}

function counter(f: Frame, p: Pal, w: number) {
  // A reception / nurse counter: a tall front with a worktop, two screens behind it.
  f.add("box", 0, 0.55, 0.3, w, 1.1, 0.12, p.white);
  f.add("box", 0, 1.12, 0.3, w + 0.1, 0.05, 0.36, p.wood);
  f.add("box", 0, 0.74, -0.05, w, 0.04, 0.62, p.wood);
  f.add("glow", 0, 0.3, 0.37, w * 0.92, 0.04, 0.005, p.screen);
  for (const sx of [-w / 4, w / 4]) {
    f.add("box", sx, 0.98, -0.1, 0.5, 0.32, 0.03, p.dark);
    f.add("glow", sx, 0.98, -0.08, 0.45, 0.26, 0.005, p.info);
    chair(f, sx, -0.6, 0, p);
  }
}

function waitingRow(f: Frame, p: Pal, seats: number) {
  const w = seats * 0.55;
  f.add("box", 0, 0.3, 0, w, 0.05, 0.08, p.metal);
  for (let i = 0; i < seats; i++) {
    const x = -w / 2 + 0.275 + i * 0.55;
    f.add("box", x, 0.45, 0, 0.5, 0.07, 0.48, p.fabric);
    f.add("box", x, 0.72, -0.22, 0.5, 0.48, 0.06, p.fabric);
  }
  for (const sx of [-w / 2 + 0.1, w / 2 - 0.1]) f.add("box", sx, 0.15, 0, 0.06, 0.3, 0.4, p.metal);
}

function shelf(f: Frame, p: Pal, w = 1.8, h = 2.0, d = 0.45, goods = true) {
  for (const sx of [-1, 1]) f.add("box", sx * (w / 2 - 0.02), h / 2, 0, 0.04, h, d, p.steel);
  for (let k = 0; k < 5; k++) {
    const y = 0.08 + k * ((h - 0.1) / 4);
    f.add("box", 0, y, 0, w, 0.03, d, p.steel);
    if (goods && k < 4) for (let i = 0; i < 4; i++) f.add("box", -w / 2 + 0.25 + i * (w - 0.5) / 3, y + 0.13, 0, 0.32, 0.22, d * 0.75, i % 2 ? p.white : p.blue);
  }
}

function fridge(f: Frame, p: Pal) {
  f.add("box", 0, 0.95, 0, 0.7, 1.9, 0.7, p.white);
  f.add("box", 0, 1.0, 0.36, 0.62, 1.7, 0.02, p.steel);
  f.add("glow", 0.18, 1.65, 0.375, 0.14, 0.07, 0.005, p.info);
  f.add("box", -0.27, 1.0, 0.38, 0.03, 0.6, 0.03, p.metal);
}

function rack(f: Frame, p: Pal) {
  f.add("box", 0, 1.0, 0, 0.6, 2.0, 1.0, p.dark);
  for (let k = 0; k < 6; k++) f.add("glow", -0.18, 0.35 + k * 0.28, 0.505, 0.18, 0.02, 0.005, k % 3 ? p.screen : p.info);
}

function bench(f: Frame, p: Pal, w = 1.8) {
  // A lab bench: worktop, cabinets, an instrument and a screen.
  f.add("box", 0, 0.88, 0, w, 0.04, 0.72, p.dark);
  f.add("box", 0, 0.43, 0, w - 0.05, 0.86, 0.66, p.white);
  f.add("cyl", -w / 4, 1.05, -0.05, 0.16, 0.3, 0.16, p.steel);
  f.add("box", w / 5, 1.02, -0.1, 0.32, 0.24, 0.3, p.white);
  f.add("box", w / 2.6, 1.12, -0.25, 0.4, 0.28, 0.03, p.dark);
  f.add("glow", w / 2.6, 1.12, -0.23, 0.35, 0.22, 0.005, p.screen);
}

function benchSeat(f: Frame, p: Pal, w = 1.8) {
  f.add("box", 0, 0.45, 0, w, 0.06, 0.42, p.wood);
  for (const sx of [-1, 1]) f.add("box", sx * (w / 2 - 0.12), 0.22, 0, 0.06, 0.44, 0.36, p.metal);
}

function plant(f: Frame, p: Pal) {
  f.add("cyl", 0, 0.22, 0, 0.36, 0.44, 0.36, p.pot);
  f.add("ball", 0, 0.75, 0, 0.62, 0.7, 0.62, p.leaf);
  f.add("ball", 0.12, 1.05, 0.05, 0.4, 0.45, 0.4, p.leaf);
}

function cart(f: Frame, p: Pal) {
  f.add("box", 0, 0.5, 0, 0.6, 0.8, 0.45, p.white);
  f.add("box", 0, 0.92, 0, 0.64, 0.04, 0.49, p.blue);
  for (const sx of [-1, 1]) for (const sz of [-1, 1]) f.add("cyl", sx * 0.25, 0.05, sz * 0.18, 0.06, 0.1, 0.06, p.dark);
}

function stairs(f: Frame, p: Pal, w: number, d: number, ceiling: number) {
  const steps = Math.max(4, Math.round(d / 0.28));
  for (let i = 0; i < steps; i++) {
    const h = ((i + 1) / steps) * Math.min(ceiling, 3.2) * 0.5;
    f.add("box", 0, h / 2, -d / 2 + (i + 0.5) * (d / steps), w, h, d / steps, i % 2 ? p.steel : p.white);
  }
}

function elevator(f: Frame, p: Pal, w: number, d: number, ceiling: number) {
  const h = Math.min(ceiling - 0.05, 2.6);
  f.add("box", 0, h / 2, 0, w, h, d, p.steel);
  f.add("box", 0, 1.05, d / 2 + 0.005, w * 0.7, 2.1, 0.01, p.metal);
  f.add("glow", w * 0.42, 1.2, d / 2 + 0.01, 0.06, 0.12, 0.005, p.screen);
}

// ---- zones -------------------------------------------------------------------------------------------------------

function place(ctx: Ctx, fp: Fp, rot: number, draw: (f: Frame) => void) {
  ctx.placed.push(fp);
  draw(ctx.kit.frame(at(ctx, fp.x, fp.y), rot));
}

/** Furnishes one zone; returns where people may pause (beside beds, at counters, by shelves). */
function furnish(ctx: Ctx, zone: Zone, index: number, drawnItems: Item[]): ZoneInterior {
  const poly = zone.points as P[];
  const type = zoneType(zone);
  const rand = rng(hash(`${zone.id}:${zone.name}`));
  const anchors: P[] = [];
  const before = ctx.placed.length;
  const icu = type === "icu" || type === "er";
  const b = bbox(poly);
  const beds = drawnItems.filter((it) => it.type === "bed");
  const tryWall = (w: number, d: number, spacing: number, max: number, draw: (f: Frame) => void, anchor = 0.8) => {
    let n = 0;
    for (const s of wallSpots(poly, w, d, spacing)) {
      if (n >= max) break;
      if (!free(ctx, poly, s.fp)) continue;
      place(ctx, s.fp, s.rot, draw);
      anchors.push([s.x + Math.sin(s.rot) * (d / 2 + anchor), s.y + Math.cos(s.rot) * (d / 2 + anchor)]);
      n++;
    }
    return n;
  };
  const tryCentre = (w: number, d: number, rows: number, cols: number, rot: number, draw: (f: Frame) => void) => {
    const cx = (b.x0 + b.x1) / 2;
    const gx = (b.x1 - b.x0) / (cols + 1), gy = (b.y1 - b.y0) / (rows + 1);
    for (let r = 0; r < rows; r++) for (let c = 0; c < cols; c++) {
      const x = b.x0 + gx * (c + 1), y = b.y0 + gy * (r + 1);
      const fp: Fp = Math.abs(Math.sin(rot)) > 0.5 ? { x, y, w: d, d: w } : { x, y, w, d };
      if (!free(ctx, poly, fp, 0.9)) continue;
      place(ctx, fp, rot, draw);
      anchors.push([x + (cx > x ? 0.9 : -0.9), y]);
    }
  };
  const corners = () => {
    // A plant in up to two corners.
    let n = 0;
    for (const [x, y] of poly) {
      if (n >= 2) break;
      const px = x + (x < (b.x0 + b.x1) / 2 ? 0.55 : -0.55), py = y + (y < (b.y0 + b.y1) / 2 ? 0.55 : -0.55);
      const fp = { x: px, y: py, w: 0.6, d: 0.6 };
      if (free(ctx, poly, fp, 0.2)) { place(ctx, fp, rand() * 6.28, (f) => plant(f, ctx.pal)); n++; }
    }
  };

  switch (type) {
    case "icu": case "er": case "ward": {
      if (beds.length) {
        // The operator drew the beds: add what goes with them.
        for (const it of beds) {
          const fp = itemFp(it);
          const f = ctx.kit.frame(at(ctx, it.x + it.w / 2, it.y + it.h / 2), (-it.rot * Math.PI) / 180 + (it.h >= it.w ? 0 : Math.PI / 2));
          bedside(f, ctx.pal, icu, rand() < 0.5 ? 1 : -1);
          anchors.push([fp.x + fp.w / 2 + 0.5, fp.y]);
        }
      } else {
        tryWall(1.0, 2.15, 2.6, 8, (f) => { bed(f, ctx.pal, icu); bedside(f, ctx.pal, icu, 1); }, 0.4);
      }
      if (type !== "ward") tryCentre(0.6, 0.45, 1, 2, 0, (f) => cart(f, ctx.pal));
      corners();
      break;
    }
    case "lobby": {
      tryWall(3.0, 0.9, 99, 1, (f) => counter(f, ctx.pal, 2.8), 1.0);
      tryCentre(2.2, 0.6, 2, 3, 0, (f) => waitingRow(f, ctx.pal, 4));
      corners();
      break;
    }
    case "nurse": {
      tryCentre(3.2, 1.0, 1, 1, 0, (f) => counter(f, ctx.pal, 3.0));
      tryWall(0.6, 0.45, 2.0, 2, (f) => cart(f, ctx.pal));
      tryWall(1.8, 0.45, 2.4, 2, (f) => shelf(f, ctx.pal, 1.6, 1.8));
      corners();
      break;
    }
    case "lab": tryWall(1.8, 0.75, 2.2, 10, (f) => bench(f, ctx.pal)); tryCentre(2.4, 1.2, 1, 1, 0, (f) => bench(f, ctx.pal, 2.4)); corners(); break;
    case "pharmacy": tryWall(1.8, 0.45, 2.0, 10, (f) => shelf(f, ctx.pal)); tryCentre(1.8, 0.9, 2, 2, Math.PI / 2, (f) => { shelf(f, ctx.pal, 1.8, 1.7, 0.42); }); break;
    case "cold": tryWall(0.75, 0.75, 1.0, 12, (f) => fridge(f, ctx.pal)); tryCentre(1.8, 0.5, 1, 2, 0, (f) => shelf(f, ctx.pal, 1.6, 1.8, 0.5)); break;
    case "server": tryCentre(0.6, 1.0, 2, 6, 0, (f) => rack(f, ctx.pal)); tryWall(0.6, 1.0, 0.65, 6, (f) => rack(f, ctx.pal)); break;
    case "corridor": tryWall(1.8, 0.45, 6.5, 6, (f) => benchSeat(f, ctx.pal)); corners(); break;
    case "storage": tryWall(1.8, 0.5, 2.0, 10, (f) => shelf(f, ctx.pal)); break;
    default: tryWall(1.4, 0.7, 2.6, 4, (f) => desk(f, ctx.pal)); corners();
  }
  // People walk around what was generated here and around the furniture the operator drew in this zone.
  const drawnFps = drawnItems.filter((it) => FURNITURE_ITEMS.has(it.type)).map(itemFp);
  return { zoneIndex: index, type, anchors, blocked: [...drawnFps, ...ctx.placed.slice(before)] };
}

/** The plan items drawn as furniture (the generic grey boxes of these types are hidden in this style). */
export const FURNITURE_ITEMS = new Set(["bed", "desk", "stairs", "elevator", "rack"]);

/**
 * Builds the interior of one floor into a group (a handful of instanced meshes). `devices` are the floor's placed
 * devices (plan x, y): furniture keeps clear of them.
 */
export function buildInterior(draft: Draft, origin: THREE.Vector3, theme: TwinTheme, devices: P[], low: boolean): { group: THREE.Group; zones: ZoneInterior[] } {
  const pal = palette(theme);
  const kit = new Kit();
  const doors = draft.layout.items.filter((it) => it.type === "door" || it.type === "exit").map((it) => [it.x + it.w / 2, it.y + it.h / 2] as P);
  const ctx: Ctx = { kit, pal, origin, doors, devices, placed: [] };
  // The drawn items first: they are what the operator placed, and generated furniture keeps clear of them.
  for (const it of draft.layout.items) {
    if (!FURNITURE_ITEMS.has(it.type)) continue;
    ctx.placed.push(itemFp(it));
    const centre = at(ctx, it.x + it.w / 2, it.y + it.h / 2), rot = (-it.rot * Math.PI) / 180;
    const f = kit.frame(centre, rot);
    if (it.type === "bed") {
      // A bed is 2 m long along its local z: turn it when the operator drew it wider than deep.
      const zone = draft.layout.zones.find((z) => z.points.length >= 3 && inside([it.x + it.w / 2, it.y + it.h / 2], z.points as P[]));
      const t = zone ? zoneType(zone) : "ward";
      bed(kit.frame(centre, rot + (it.h >= it.w ? 0 : Math.PI / 2)), pal, t === "icu" || t === "er");
    } else if (it.type === "desk") desk(f, pal, Math.max(0.9, Math.min(2.4, it.w)));
    else if (it.type === "stairs") stairs(f, pal, it.w, it.h, draft.ceiling_m);
    else if (it.type === "elevator") elevator(f, pal, it.w, it.h, draft.ceiling_m);
    else if (it.type === "rack") rack(f, pal);
  }
  const zones: ZoneInterior[] = [];
  draft.layout.zones.forEach((z, index) => {
    if (z.points.length < 3) return;
    const poly = z.points as P[];
    const drawn = draft.layout.items.filter((it) => inside([it.x + it.w / 2, it.y + it.h / 2], poly));
    zones.push(furnish(ctx, z, index, drawn));
  });
  const group = kit.build(low, "interior");
  return { group, zones };
}
