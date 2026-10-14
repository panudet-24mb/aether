// Plan geometry and seeded randomness for the lifelike layer (docs/platform/digital-twin.md, "Lifelike hospital").
// Pure functions in plan metres ([x, y], y growing south): no three.js, so twin.selftest.ts can check them.

export type P = [number, number];

/** A 32-bit FNV-1a hash: the seed of a zone's layout or a person's look. */
export function hash(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 0x01000193); }
  return h >>> 0;
}

/** mulberry32: a small deterministic generator in [0, 1). The same seed always gives the same layout. */
export function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function inside(p: P, poly: P[]): boolean {
  let hit = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i++) {
    const [xi, yi] = poly[i], [xj, yj] = poly[j];
    if (yi > p[1] !== yj > p[1] && p[0] < ((xj - xi) * (p[1] - yi)) / (yj - yi) + xi) hit = !hit;
  }
  return hit;
}

/** The closest point of segment a–b to p, and how far it is. */
export function closestOnSegment(p: P, a: P, b: P): { at: P; d: number; t: number } {
  const dx = b[0] - a[0], dy = b[1] - a[1];
  const len2 = dx * dx + dy * dy;
  const t = len2 ? Math.max(0, Math.min(1, ((p[0] - a[0]) * dx + (p[1] - a[1]) * dy) / len2)) : 0;
  const at: P = [a[0] + dx * t, a[1] + dy * t];
  return { at, d: Math.hypot(p[0] - at[0], p[1] - at[1]), t };
}

/** Distance from p to the polygon's boundary. */
export function edgeDistance(p: P, poly: P[]): number {
  let d = Infinity;
  for (let i = 0; i < poly.length; i++) d = Math.min(d, closestOnSegment(p, poly[i], poly[(i + 1) % poly.length]).d);
  return d;
}

/** Inside the polygon and at least `margin` metres from its edges. */
export function insideWithMargin(p: P, poly: P[], margin: number): boolean {
  return inside(p, poly) && edgeDistance(p, poly) >= margin;
}

export function bbox(poly: P[]): { x0: number; y0: number; x1: number; y1: number } {
  let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
  for (const [x, y] of poly) { x0 = Math.min(x0, x); y0 = Math.min(y0, y); x1 = Math.max(x1, x); y1 = Math.max(y1, y); }
  return { x0, y0, x1, y1 };
}

export function centroid(poly: P[]): P {
  return [poly.reduce((s, p) => s + p[0], 0) / poly.length, poly.reduce((s, p) => s + p[1], 0) / poly.length];
}

/** A random point at least `margin` from the edges (rejection sampling; null when the zone is too thin). */
export function randomInside(poly: P[], margin: number, rand: () => number, tries = 40): P | null {
  const b = bbox(poly);
  for (let i = 0; i < tries; i++) {
    const p: P = [b.x0 + rand() * (b.x1 - b.x0), b.y0 + rand() * (b.y1 - b.y0)];
    if (insideWithMargin(p, poly, margin)) return p;
  }
  return null;
}

/** A straight walk from a to b stays inside the polygon (sampled every 0.25 m). */
export function segmentInside(a: P, b: P, poly: P[], margin = 0): boolean {
  const n = Math.max(2, Math.ceil(Math.hypot(b[0] - a[0], b[1] - a[1]) / 0.25));
  for (let i = 0; i <= n; i++) {
    const t = i / n;
    const p: P = [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t];
    if (margin ? !insideWithMargin(p, poly, margin) : !inside(p, poly)) return false;
  }
  return true;
}

/** Wall-like segments of a floor: drawn walls (with thickness) and zone outlines (rooms without drawn walls). */
export type Segment = { a: P; b: P; thickness: number };

/**
 * Where a wall-mounted device goes: the closest wall segment, the device pushed onto the wall's face on the side it
 * stands on. `normal` points from the wall into the room; `along` is the wall's direction.
 */
export function snapToWall(p: P, segments: Segment[], maxDistance = 6): { at: P; normal: P; along: P; distance: number } | null {
  let best: { at: P; normal: P; along: P; distance: number } | null = null;
  for (const s of segments) {
    const c = closestOnSegment(p, s.a, s.b);
    if (c.d > maxDistance || (best && c.d >= best.distance)) continue;
    const len = Math.hypot(s.b[0] - s.a[0], s.b[1] - s.a[1]) || 1;
    const along: P = [(s.b[0] - s.a[0]) / len, (s.b[1] - s.a[1]) / len];
    let normal: P = [-along[1], along[0]];
    // Face the side the device is on (a device exactly on the wall keeps the left-hand normal).
    if ((p[0] - c.at[0]) * normal[0] + (p[1] - c.at[1]) * normal[1] < 0) normal = [-normal[0], -normal[1]];
    // Walls drawn on top of each other (a room outline on the building's shell) count with the thickest one.
    let thick = s.thickness;
    for (const o of segments) {
      if (o === s) continue;
      const ol = Math.hypot(o.b[0] - o.a[0], o.b[1] - o.a[1]) || 1;
      const parallel = Math.abs(along[0] * (o.b[1] - o.a[1]) / ol - along[1] * (o.b[0] - o.a[0]) / ol) < 0.05;
      if (parallel && closestOnSegment(c.at, o.a, o.b).d < 0.03) thick = Math.max(thick, o.thickness);
    }
    const off = thick / 2 + 0.015;
    // Keep away from the segment's ends so a plate never hangs in a doorway or past a corner.
    const t = Math.max(Math.min(0.3 / len, 0.5), Math.min(1 - Math.min(0.3 / len, 0.5), c.t));
    const at: P = [s.a[0] + (s.b[0] - s.a[0]) * t + normal[0] * off, s.a[1] + (s.b[1] - s.a[1]) * t + normal[1] * off];
    best = { at, normal, along, distance: c.d };
  }
  return best;
}

/** Axis-aligned overlap of two footprints with a gap. */
export function overlaps(a: { x: number; y: number; w: number; d: number }, b: { x: number; y: number; w: number; d: number }, gap = 0.15): boolean {
  return Math.abs(a.x - b.x) * 2 < a.w + b.w + gap * 2 && Math.abs(a.y - b.y) * 2 < a.d + b.d + gap * 2;
}
