// Replay state (docs/platform/digital-twin.md, phase P3): the server sends a window as a keyframe at `from` plus the
// changes after it; stateAt rebuilds the twin's state at any time T in it without asking the server again, so
// scrubbing and playing cost nothing but a binary search per person.

import type { TwinAlert, TwinReplay, TwinState } from "../api";

/** The index a replay needs for fast scrubbing: per person (or per gateway in counts mode) its changes in time order. */
export type ReplayIndex = {
  replay: TwinReplay;
  fromMs: number;
  toMs: number;
  bucketMs: number;
  /** tracks / named: per pid, change times and the gateway after each (null: not on this site). */
  people: Map<string, { at: number[]; gw: (string | null)[]; start: string | null }>;
  /** counts: per gateway, change times and the headcount after each. */
  counts: Map<string, { at: number[]; n: number[]; start: number }>;
};

/** The last index i with xs[i] <= t, or -1. */
export function lastAtOrBefore(xs: number[], t: number): number {
  let lo = 0, hi = xs.length - 1, found = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (xs[mid] <= t) { found = mid; lo = mid + 1; } else hi = mid - 1;
  }
  return found;
}

export function indexReplay(replay: TwinReplay): ReplayIndex {
  const fromMs = Date.parse(replay.from), toMs = Date.parse(replay.to);
  const out: ReplayIndex = { replay, fromMs, toMs, bucketMs: replay.bucket_sec * 1000, people: new Map(), counts: new Map() };
  const p = replay.people;
  if (!p) return out;
  if (p.key_counts || p.count_deltas) {
    for (const [g, n] of Object.entries(p.key_counts ?? {})) out.counts.set(g, { at: [], n: [], start: n });
    for (const [at, g, d] of p.count_deltas ?? []) {
      let c = out.counts.get(g);
      if (!c) { c = { at: [], n: [], start: 0 }; out.counts.set(g, c); }
      const prev = c.n.length ? c.n[c.n.length - 1] : c.start;
      c.at.push(at);
      c.n.push(Math.max(0, prev + d));
    }
    return out;
  }
  for (const [pid, g] of Object.entries(p.key ?? {})) out.people.set(pid, { at: [], gw: [], start: g });
  for (const [at, pid, g] of p.deltas ?? []) {
    let x = out.people.get(pid);
    if (!x) { x = { at: [], gw: [], start: null }; out.people.set(pid, x); }
    x.at.push(at);
    x.gw.push(g);
  }
  return out;
}

/** The gateway each person is at at time t (tracks / named), or the headcount per gateway (counts). */
export function peopleAt(ix: ReplayIndex, t: number): { people: Map<string, string | null>; counts: Map<string, number> } {
  const people = new Map<string, string | null>();
  const counts = new Map<string, number>();
  for (const [pid, x] of ix.people) {
    const i = lastAtOrBefore(x.at, t);
    const g = i < 0 ? x.start : x.gw[i];
    people.set(pid, g);
    if (g) counts.set(g, (counts.get(g) ?? 0) + 1);
  }
  for (const [g, c] of ix.counts) {
    const i = lastAtOrBefore(c.at, t);
    const n = i < 0 ? c.start : c.n[i];
    if (n > 0) counts.set(g, n);
  }
  return { people, counts };
}

/** The bucket index of time t (clamped), and how far into it t is (0..1), for interpolation. */
export function bucketAt(ix: ReplayIndex, t: number): { i: number; frac: number } {
  const n = ix.replay.env?.buckets ?? Math.max(1, Math.round((ix.toMs - ix.fromMs) / ix.bucketMs));
  const pos = (t - ix.fromMs) / ix.bucketMs;
  const i = Math.max(0, Math.min(n - 1, Math.floor(pos)));
  return { i, frac: Math.max(0, Math.min(1, pos - i)) };
}

/** A series value at bucket i with linear interpolation toward the next bucket (the last known value when one is missing). */
export function valueAt(xs: (number | null)[] | undefined, i: number, frac: number): number | undefined {
  if (!xs) return undefined;
  let a: number | null = null;
  for (let k = i; k >= 0 && a === null; k--) a = xs[k];
  if (a === null) return undefined;
  const b = i + 1 < xs.length ? xs[i + 1] : null;
  return b === null ? a : a + (b - a) * frac;
}

/**
 * The twin's state at time t, in the shape the live view draws: `base` gives the placements (positions, names,
 * kinds); values, presence and alerts come from the replay.
 */
export function stateAt(ix: ReplayIndex, base: TwinState, t: number): TwinState {
  const r = ix.replay;
  const { i, frac } = bucketAt(ix, t);
  const devices = base.devices.map((d) => {
    const s = r.env?.series[d.id];
    if (d.kind !== "device") return { ...d, alert: false, sos: false };
    const tv = valueAt(s?.t, i, frac), hv = valueAt(s?.h, i, frac);
    const door = s?.door ? s.door.slice(0, i + 1).reverse().find((v) => v !== null) : undefined;
    const reported = !!s && [s.t[i], s.h[i], s.motion[i], s.door[i]].some((v) => v !== null);
    return {
      ...d,
      t: tv, h: hv,
      door: door === undefined || door === null ? undefined : (door ? 1 : 0) as 0 | 1,
      motion_at: s?.motion[i] ? new Date(t).toISOString() : undefined,
      online: reported,
      last_at: reported ? new Date(ix.fromMs + i * ix.bucketMs).toISOString() : d.last_at,
      alert: false, sos: false,
    };
  });
  const alerts: TwinAlert[] = [];
  for (const a of r.alerts?.items ?? []) {
    const opened = Date.parse(a.opened_at);
    if (opened > t || (a.resolved_at && Date.parse(a.resolved_at) <= t)) continue;
    const acked = !!a.acked_at && Date.parse(a.acked_at) <= t;
    alerts.push({ id: a.id, severity: a.severity, status: acked ? "acknowledged" : "open", event: a.event, sos: a.sos && !acked, hazard: a.hazard, title: a.title, device_id: a.device_id, opened_at: a.opened_at, gateway_id: a.gateway_id });
    const k = devices.findIndex((d) => d.id === a.device_id);
    if (k >= 0 && !acked) { devices[k] = { ...devices[k], alert: true, sos: devices[k].sos || a.sos }; }
  }
  const { people, counts } = peopleAt(ix, t);
  const mode = r.people?.mode === "off" || !r.people ? "counts" : r.people.mode;
  const sosAt = new Set(alerts.filter((a) => a.sos).map((a) => a.gateway_id));
  return {
    server_time: new Date(t).toISOString(),
    site_id: base.site_id,
    demo: base.demo,
    layout_revision: base.layout_revision,
    devices,
    presence: {
      mode,
      counts: [...counts].map(([gateway_id, n]) => ({ gateway_id, n })),
      people: mode === "counts" ? undefined : [...people].filter(([, g]) => g).map(([pid, g]) => ({
        pid, name: r.people?.names?.[pid], gateway_id: g, since: null, last_at: new Date(t).toISOString(), sos: !!g && sosAt.has(g),
      })),
    },
    alerts,
  };
}

/** Replay windows by key, fetched once; the next window is fetched ahead while one plays. */
export class ReplayStore {
  private cache = new Map<string, Promise<ReplayIndex>>();
  constructor(private fetchReplay: (from: string, to: string) => Promise<TwinReplay>, private max = 6) {}

  get(from: string, to: string): Promise<ReplayIndex> {
    const key = `${from}|${to}`;
    let hit = this.cache.get(key);
    if (!hit) {
      hit = this.fetchReplay(from, to).then(indexReplay);
      hit.catch(() => this.cache.delete(key));
      this.cache.set(key, hit);
      while (this.cache.size > this.max) this.cache.delete(this.cache.keys().next().value as string);
    }
    return hit;
  }

  /** Fetch a window without waiting for it (the following window while playing past 70 % of this one). */
  prefetch(from: string, to: string) { void this.get(from, to).catch(() => undefined); }

  clear() { this.cache.clear(); }
}
