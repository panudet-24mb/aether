// Zone-constrained heat maps for the digital twin (docs/platform/digital-twin.md). Pure functions, no three.js,
// so twin.selftest.ts can check them with plain tsc + node.
//
// A zone is a room: heat never leaks through its walls. Every texel inside a zone is the inverse-distance
// weighted mean (power 2) of the sensors placed inside that same zone; a zone without a sensor has no value and
// is drawn hatched ("ไม่มีเซ็นเซอร์"), never extrapolated from the room next door. Texels outside every zone are
// transparent.

export type P = [number, number];
export type HeatSensor = { x: number; y: number; value: number; /** 0..1: stale readings fade out of the mix. */ weight: number };
export type HeatZone = { points: P[] };
export type HeatKind = "temperature" | "humidity" | "occupancy";

/** Texel size in metres, and the texture's cap per side (256² per floor is ~65k texels). */
export const TEXEL_M = 0.25;
export const MAX_TEXELS = 256;

export function pointIn(p: P, poly: P[]): boolean {
  let inside = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i++) {
    const [xi, yi] = poly[i], [xj, yj] = poly[j];
    if (yi > p[1] !== yj > p[1] && p[0] < ((xj - xi) * (p[1] - yi)) / (yj - yi) + xi) inside = !inside;
  }
  return inside;
}

/** Inverse-distance weighting: exact at a sensor, a weighted mean elsewhere, NaN without any usable sensor. */
export function idwAt(sensors: HeatSensor[], x: number, y: number, power = 2): number {
  let num = 0, den = 0;
  for (const s of sensors) {
    if (!(s.weight > 0) || !Number.isFinite(s.value)) continue;
    const d2 = (s.x - x) ** 2 + (s.y - y) ** 2;
    if (d2 < 1e-10) return s.value;
    const w = s.weight / Math.pow(d2, power / 2);
    num += w * s.value;
    den += w;
  }
  return den > 0 ? num / den : NaN;
}

/** How much a reading counts: full for 5 minutes, then fading to nothing at 15 minutes. */
export function staleWeight(ageMs: number): number {
  const min = ageMs / 60000;
  if (min <= 5) return 1;
  if (min >= 15) return 0;
  return 1 - (min - 5) / 10;
}

export type HeatGrid = {
  width: number;
  height: number;
  /** Metres per texel along x and y (the grid spans the whole floor). */
  sx: number;
  sy: number;
  /** Value per texel, row-major from the floor's top-left; NaN outside zones and in zones without a sensor. */
  values: Float32Array;
  /** Zone index per texel, -1 outside every zone. */
  zone: Int16Array;
};

export function gridSize(widthM: number, depthM: number): [number, number] {
  return [Math.max(2, Math.min(MAX_TEXELS, Math.round(widthM / TEXEL_M))), Math.max(2, Math.min(MAX_TEXELS, Math.round(depthM / TEXEL_M)))];
}

/** Computes the grid; `sensorsOf(zoneIndex)` returns the sensors placed inside that zone. */
export function heatGrid(widthM: number, depthM: number, zones: HeatZone[], sensorsOf: (zone: number) => HeatSensor[]): HeatGrid {
  const [width, height] = gridSize(widthM, depthM);
  const sx = widthM / width, sy = depthM / height;
  const values = new Float32Array(width * height).fill(NaN);
  const zone = new Int16Array(width * height).fill(-1);
  const bySensors = zones.map((_, i) => sensorsOf(i));
  const boxes = zones.map((z) => {
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const [x, y] of z.points) { x0 = Math.min(x0, x); y0 = Math.min(y0, y); x1 = Math.max(x1, x); y1 = Math.max(y1, y); }
    return [x0, y0, x1, y1];
  });
  for (let j = 0; j < height; j++) {
    const y = (j + 0.5) * sy;
    for (let i = 0; i < width; i++) {
      const x = (i + 0.5) * sx;
      for (let z = 0; z < zones.length; z++) {
        const b = boxes[z];
        if (x < b[0] || x > b[2] || y < b[1] || y > b[3] || zones[z].points.length < 3 || !pointIn([x, y], zones[z].points)) continue;
        zone[j * width + i] = z;
        values[j * width + i] = idwAt(bySensors[z], x, y);
        break;
      }
    }
  }
  return { width, height, sx, sy, values, zone };
}

type RGB = [number, number, number];
const lerp = (a: number, b: number, t: number) => a + (b - a) * t;
function stops(list: RGB[], t: number): RGB {
  const k = Math.max(0, Math.min(1, t)) * (list.length - 1);
  const i = Math.min(list.length - 2, Math.floor(k)), f = k - i;
  return [Math.round(lerp(list[i][0], list[i + 1][0], f)), Math.round(lerp(list[i][1], list[i + 1][1], f)), Math.round(lerp(list[i][2], list[i + 1][2], f))];
}

/** Sequential, colour-blind safe ramps, all readable on the dark stage (see twin.selftest.ts). */
export const RAMPS: Record<HeatKind, RGB[]> = {
  // Cool blue through teal and amber to coral: cold store to overheated.
  temperature: [[77, 142, 255], [64, 196, 214], [167, 243, 208], [246, 193, 119], [255, 143, 112]],
  // Viridis-like, lifted for the dark background.
  humidity: [[92, 110, 214], [58, 164, 190], [94, 201, 138], [214, 228, 94]],
  // The accent, from faint to full.
  occupancy: [[62, 110, 96], [111, 196, 160], [167, 243, 208]],
};

export function rampColor(kind: HeatKind, value: number, min: number, max: number): RGB {
  const t = max > min ? (value - min) / (max - min) : 0.5;
  return stops(RAMPS[kind], t);
}

// Temperature is coloured by how far a zone is from its comfort band, not on one scale for the whole building (a
// cold store at 5 °C would otherwise make every normal room look hot). Inside the band is the accent; below it runs to
// blue, above it to coral, full colour COMFORT_SPAN °C outside the band.
export const COMFORT_SPAN = 4;
export const COMFORT_DEFAULT: Record<string, [number, number]> = {
  room: [22, 25], ward: [22, 25], corridor: [22, 26], other: [22, 25],
  restricted: [18, 27], // server and equipment rooms
  storage: [15, 25],
  outdoor: [15, 35],
};
export function comfortBand(kind: string, comfort?: [number, number] | null): [number, number] {
  return comfort && comfort[0] < comfort[1] ? comfort : COMFORT_DEFAULT[kind] ?? COMFORT_DEFAULT.room;
}
/** -1 (far below the band) .. 0 (inside) .. +1 (far above). */
export function comfortT(v: number, [lo, hi]: [number, number]): number {
  if (!Number.isFinite(v)) return NaN;
  if (v < lo) return Math.max(-1, (v - lo) / COMFORT_SPAN);
  if (v > hi) return Math.min(1, (v - hi) / COMFORT_SPAN);
  return 0;
}

/** Relative luminance and WCAG contrast, for the ramp self-check. */
export function luminance([r, g, b]: RGB): number {
  const c = [r, g, b].map((v) => { const s = v / 255; return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4; });
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
}
export function contrast(a: RGB, b: RGB): number {
  const [l1, l2] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (l1 + 0.05) / (l2 + 0.05);
}

/**
 * RGBA pixels for a grid (row-major, top-left first): ramp colour inside zones with a value, a grey hatch in
 * zones without a sensor, transparent outside zones. `base` colours zones when the heat layer is off.
 */
export function heatPixels(grid: HeatGrid, kind: HeatKind | null, min: number, max: number, base: (zone: number) => RGB): Uint8Array {
  const out = new Uint8Array(grid.width * grid.height * 4);
  for (let j = 0; j < grid.height; j++) {
    for (let i = 0; i < grid.width; i++) {
      const k = j * grid.width + i, z = grid.zone[k], o = k * 4;
      if (z < 0) continue;
      const v = grid.values[k];
      if (kind && Number.isFinite(v)) {
        const [r, g, b] = rampColor(kind, v, min, max);
        out[o] = r; out[o + 1] = g; out[o + 2] = b; out[o + 3] = 190;
      } else if (kind) {
        const stripe = (i + j) % 8 < 2;
        out[o] = stripe ? 150 : 70; out[o + 1] = stripe ? 165 : 84; out[o + 2] = stripe ? 170 : 90; out[o + 3] = stripe ? 120 : 60;
      } else {
        const [r, g, b] = base(z);
        out[o] = r; out[o + 1] = g; out[o + 2] = b; out[o + 3] = 70;
      }
    }
  }
  return out;
}
