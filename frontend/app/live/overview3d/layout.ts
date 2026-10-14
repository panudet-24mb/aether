// Pure layout for the constellation (no three.js): hubs on a circle (or concentric rings when there are many),
// grouped by project, and each hub's devices in rings around it. Deterministic and stable: a device keeps its
// place when its status changes, so a reading never makes the scene jump.

import type { Hub, HubPlace, NodePlace, StageCard, StageGateway, StageProject, Vec3 } from "./types";

export const UNKNOWN_HUB = "__unknown";

const RING0 = 3.2, RING_STEP = 1.7, NODE_GAP = 1.55, HUB_GAP = 2.6, GROUP_GAP = 6, SINGLE_RING_MAX = 14;
const GOLDEN = Math.PI * (3 - Math.sqrt(5));

const ringCapacity = (ring: number) => Math.max(6, Math.floor((2 * Math.PI * (RING0 + ring * RING_STEP)) / NODE_GAP));

/** Rings a hub with `n` devices needs. */
export function ringsFor(n: number): number {
  let rings = 0, left = n;
  while (left > 0) { left -= ringCapacity(rings); rings++; }
  return rings;
}

/** Radius of a hub's footprint (its outer ring plus a margin). */
export const footprint = (n: number) => (n ? RING0 + (ringsFor(n) - 1) * RING_STEP + 1.4 : 2.4);

/**
 * The hubs to draw: every gateway with a visible device, plus (when `showEmpty`) the in-scope gateways that have
 * none. Devices whose gateway is not in the list share one "unknown gateway" hub. Ordered by project, then name.
 */
export function buildHubs(cards: StageCard[], gateways: StageGateway[], projects: StageProject[], colors: Record<string, string>, showEmpty: boolean): Hub[] {
  const byId = new Map(gateways.map((g) => [g.id, g]));
  const counts = new Map<string, number>();
  const unknownName = new Map<string, string>();
  for (const c of cards) {
    const id = byId.has(c.gatewayId) ? c.gatewayId : UNKNOWN_HUB;
    counts.set(id, (counts.get(id) ?? 0) + 1);
    if (id === UNKNOWN_HUB && c.gatewayName) unknownName.set(id, c.gatewayName);
  }
  const color = (projectId: string | null) => {
    const p = projectId ? projects.find((x) => x.id === projectId) : undefined;
    return p ? colors[p.color] ?? null : null;
  };
  const hubs: Hub[] = gateways.filter((g) => showEmpty || counts.has(g.id)).map((g) => ({ id: g.id, name: g.name, online: g.online, projectId: g.projectId, color: color(g.projectId), count: counts.get(g.id) ?? 0 }));
  if (counts.has(UNKNOWN_HUB)) hubs.push({ id: UNKNOWN_HUB, name: unknownName.get(UNKNOWN_HUB) ?? "ไม่ทราบ gateway", online: false, projectId: null, color: null, count: counts.get(UNKNOWN_HUB)! });
  const order = new Map(projects.map((p, i) => [p.id, i]));
  const rank = (h: Hub) => (h.id === UNKNOWN_HUB ? Infinity : h.projectId ? order.get(h.projectId) ?? projects.length : projects.length + 1);
  return hubs.sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name, "th") || a.id.localeCompare(b.id));
}

export const hubOf = (c: StageCard, hubIds: Set<string>) => (hubIds.has(c.gatewayId) ? c.gatewayId : UNKNOWN_HUB);

/** Places hubs and devices. `radius` is the extent of the whole constellation (for the platform and the camera). */
export function layoutConstellation(hubs: Hub[], cards: StageCard[]): { hubs: HubPlace[]; nodes: NodePlace[]; radius: number } {
  const ids = new Set(hubs.map((h) => h.id));
  const members = new Map<string, StageCard[]>();
  for (const c of cards) {
    const id = hubOf(c, ids);
    members.set(id, [...(members.get(id) ?? []), c]);
  }
  // Stable order inside a hub: by name, then key (status never moves a device).
  for (const list of members.values()) list.sort((a, b) => a.name.localeCompare(b.name, "th") || a.key.localeCompare(b.key));
  const fp = hubs.map((h) => footprint(members.get(h.id)?.length ?? 0));
  const centres = placeHubs(hubs, fp);
  const hubPlaces: HubPlace[] = hubs.map((hub, i) => ({ hub, at: centres[i], radius: fp[i] }));
  const nodes: NodePlace[] = [];
  hubPlaces.forEach((hp) => {
    const list = members.get(hp.hub.id) ?? [];
    let i = 0;
    for (let ring = 0; i < list.length; ring++) {
      const cap = ringCapacity(ring), n = Math.min(cap, list.length - i), r = RING0 + ring * RING_STEP;
      // A part-filled ring spreads its devices evenly; each ring starts half a step on, so rings interleave.
      const start = ring * 0.5 * ((2 * Math.PI) / cap) + Math.atan2(hp.at[2], hp.at[0]);
      for (let k = 0; k < n; k++, i++) {
        const a = start + (k / n) * 2 * Math.PI;
        nodes.push({ card: list[i], hub: hp.hub.id, ring, at: [hp.at[0] + Math.cos(a) * r, 1.1 + ring * 0.38, hp.at[2] + Math.sin(a) * r] });
      }
    }
  });
  const radius = hubPlaces.reduce((m, h) => Math.max(m, Math.hypot(h.at[0], h.at[2]) + h.radius), 8);
  return { hubs: hubPlaces, nodes, radius };
}

/** Hub centres: one circle for a handful of hubs (arc shares by size, gaps between projects), rings beyond that. */
function placeHubs(hubs: Hub[], fp: number[]): Vec3[] {
  if (hubs.length === 0) return [];
  if (hubs.length === 1) return [[0, 0, 0]];
  const need = hubs.map((h, i) => 2 * fp[i] + HUB_GAP + (i > 0 && h.projectId !== hubs[i - 1].projectId ? GROUP_GAP : 0));
  const maxFp = Math.max(...fp);
  if (hubs.length <= SINGLE_RING_MAX) {
    const total = need.reduce((s, x) => s + x, 0);
    const angles: number[] = [];
    let a = -Math.PI / 2;
    for (let i = 0; i < hubs.length; i++) { a += (need[i] / total) * Math.PI; angles.push(a); a += (need[i] / total) * Math.PI; }
    // The angles only depend on the shares; the radius is the smallest that keeps every pair apart across the chord.
    let R = Math.max(total / (2 * Math.PI), maxFp + HUB_GAP);
    for (let i = 0; i < hubs.length; i++) for (let j = i + 1; j < hubs.length; j++) {
      const t = Math.abs(angles[j] - angles[i]), half = Math.min(t, 2 * Math.PI - t) / 2;
      R = Math.max(R, (fp[i] + fp[j] + HUB_GAP / 2) / (2 * Math.max(Math.sin(half), 1e-3)));
    }
    return angles.map((t): Vec3 => [Math.cos(t) * R, 0, Math.sin(t) * R]);
  }
  // Many hubs: concentric rings, each filled in order (projects stay contiguous), rotated by the golden angle.
  const out: Vec3[] = [];
  const pitch = 2 * maxFp + HUB_GAP;
  let i = 0;
  for (let ring = 0; i < hubs.length; ring++) {
    const R = pitch * (ring + 0.75);
    // As many as fit with neighbours at least `pitch` apart across the chord.
    const fit = Math.max(1, Math.floor(Math.PI / Math.asin(Math.min(1, pitch / (2 * R)))));
    const n = Math.min(fit, hubs.length - i);
    i += n;
    for (let k = 0; k < n; k++) {
      const a = ring * GOLDEN + (k / n) * 2 * Math.PI;
      out.push([Math.cos(a) * R, 0, Math.sin(a) * R]);
    }
  }
  return out;
}

/**
 * The devices that get a label, at most `max`: SOS, then alerts, then controllable devices, then the rest; nearer
 * the camera first inside a tier. `keep` (the label holding keyboard focus) is always in the set.
 */
export function pickLabels(items: { key: string; tier: number; dist: number }[], max = 40, keep?: string): string[] {
  const keys = [...items].sort((a, b) => a.tier - b.tier || a.dist - b.dist || a.key.localeCompare(b.key)).slice(0, max).map((x) => x.key);
  if (keep && !keys.includes(keep) && items.some((x) => x.key === keep)) keys.splice(Math.max(0, max - 1), 1, keep);
  return keys;
}

/** SOS devices that were not SOS in the previous set (for the live announcement). */
export function newSos(prev: Set<string>, cards: StageCard[]): StageCard[] {
  return cards.filter((c) => c.sos && !prev.has(c.key));
}
