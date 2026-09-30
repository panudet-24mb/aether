// Where people tokens stand inside a zone (docs/platform/digital-twin.md). Aether knows which zone a worn tag is
// in, not where in the zone, so tokens take fixed slots of a hex grid clipped to the zone and never wander: a
// token that jitters would look like movement nobody measured. Pure, for twin.selftest.ts.

import { pointIn, type P } from "./heat";

/** Distance between neighbouring slots, metres. */
export const SLOT_SPACING = 0.9;

/** The slots of a zone in reading order (top row first, left to right): the same zone always gives the same list. */
export function zoneSlots(points: P[], spacing = SLOT_SPACING, max = 200): P[] {
  if (points.length < 3) return [];
  let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
  for (const [x, y] of points) { x0 = Math.min(x0, x); y0 = Math.min(y0, y); x1 = Math.max(x1, x); y1 = Math.max(y1, y); }
  const rowH = spacing * Math.sqrt(3) / 2;
  const out: P[] = [];
  // Keep a margin from the walls so tokens are not drawn inside them.
  const margin = spacing * 0.6;
  for (let r = 0, y = y0 + margin; y <= y1 - margin + 1e-9 && out.length < max; r++, y += rowH) {
    for (let x = x0 + margin + (r % 2 ? spacing / 2 : 0); x <= x1 - margin + 1e-9 && out.length < max; x += spacing) {
      const p: P = [Math.round(x * 100) / 100, Math.round(y * 100) / 100];
      if (pointIn(p, points)) out.push(p);
    }
  }
  if (!out.length) {
    // A zone smaller than one slot: its centre.
    const cx = points.reduce((s, p) => s + p[0], 0) / points.length, cy = points.reduce((s, p) => s + p[1], 0) / points.length;
    out.push([cx, cy]);
  }
  return out;
}

/**
 * Slot per person: people sorted by id take the slots in order, so the same set of people always stands in the
 * same places. More people than slots share the last slots (a crowd, drawn as a count anyway).
 */
export function assignSlots(ids: string[], slots: P[]): Map<string, P> {
  const out = new Map<string, P>();
  const sorted = [...ids].sort();
  sorted.forEach((id, i) => out.set(id, slots[Math.min(i, slots.length - 1)] ?? [0, 0]));
  return out;
}
