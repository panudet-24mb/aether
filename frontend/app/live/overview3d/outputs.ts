// Which devices have a switchable output (the rule the 3D power chip and the board's Tuya cards share). A Tuya Wi‑Fi
// card's kind says nothing about what it can do (a plug, a lamp, an AC and an air-quality monitor are all "tuya"),
// so for those the board asks /devices/{id}/controls lazily (a few at a time), caches a real answer for the page,
// and retries with backoff when the endpoint fails or the device has not reported its features yet.

import { useEffect, useMemo, useSyncExternalStore } from "react";
import type { ControlFeature, DeviceControls } from "../../topology/api";
import type { CommandClient } from "../../topology/device-controls";

export type PowerPlan =
  | { kind: "power"; features: ControlFeature[]; on: boolean | null }
  | { kind: "cover"; feature: ControlFeature; on: boolean | null };

/** What one press does: every output on/off (the compact controls' rule for an output), or a cover open/close. */
export function powerPlan(c: DeviceControls): PowerPlan | null {
  const outputs = c.features.filter((f) => f.type === "binary" && (f.name === "state" || /^(state|switch)(_|$)/.test(f.property)));
  if (outputs.length) {
    const known = outputs.filter((f) => c.state[f.property] !== undefined);
    return { kind: "power", features: outputs, on: known.length ? known.some((f) => c.state[f.property] === f.value_on) : null };
  }
  const cover = c.features.find((f) => f.type === "enum" && f.name === "state" && (f.values ?? []).includes("OPEN") && (f.values ?? []).includes("CLOSE"));
  if (!cover) return null;
  const pos = Number(c.state.position), st = c.state[cover.property];
  return { kind: "cover", feature: cover, on: Number.isFinite(pos) ? pos > 0 : st === "OPEN" ? true : st === "CLOSE" ? false : null };
}

const PARALLEL = 4, RETRY_MS = 4000, RETRY_MAX_MS = 5 * 60000;

/** Per-client cache of "has a switchable output". Only real answers are cached; errors and empty answers retry. */
class OutputCache {
  private known = new Map<string, boolean>();
  private queue: string[] = [];
  private busy = new Set<string>();
  private waiting = new Map<string, ReturnType<typeof setTimeout>>();
  private attempts = new Map<string, number>();
  private listeners = new Set<() => void>();
  /** Immutable copy for useSyncExternalStore, replaced on every answer. */
  snapshot: ReadonlyMap<string, boolean> = new Map();

  constructor(private client: CommandClient) {}

  subscribe = (fn: () => void) => { this.listeners.add(fn); return () => { this.listeners.delete(fn); }; };
  get = () => this.snapshot;

  ensure(ids: string[]) {
    for (const id of ids) if (!this.known.has(id) && !this.busy.has(id) && !this.waiting.has(id) && !this.queue.includes(id)) this.queue.push(id);
    this.pump();
  }

  private pump() {
    while (this.busy.size < PARALLEL && this.queue.length) {
      const id = this.queue.shift()!;
      this.busy.add(id);
      this.client.deviceControls(id).then(
        (c) => {
          // No features yet (the device has not reported its definition): unknown, ask again later.
          if (!c.features.length) this.retry(id);
          else this.answer(id, !!powerPlan(c));
        },
        () => this.retry(id), // 429, 5xx, offline: never cached
      ).finally(() => { this.busy.delete(id); this.pump(); });
    }
  }

  private answer(id: string, ok: boolean) {
    this.known.set(id, ok);
    this.attempts.delete(id);
    this.snapshot = new Map(this.known);
    for (const fn of this.listeners) fn();
  }

  private retry(id: string) {
    const n = (this.attempts.get(id) ?? 0) + 1;
    this.attempts.set(id, n);
    this.waiting.set(id, setTimeout(() => {
      this.waiting.delete(id);
      // Nobody is looking any more: forget it, the next view that needs it asks again.
      if (this.listeners.size) this.ensure([id]);
    }, Math.min(RETRY_MAX_MS, RETRY_MS * 2 ** (n - 1))));
  }
}

const caches = new WeakMap<CommandClient, OutputCache>();
const cacheFor = (client: CommandClient) => {
  let c = caches.get(client);
  if (!c) { c = new OutputCache(client); caches.set(client, c); }
  return c;
};

/** `has(id)`: whether the device reported a switchable output (false while unknown). */
export function useOutputs(client: CommandClient, ids: string[]): (deviceId: string) => boolean {
  const cache = cacheFor(client);
  const known = useSyncExternalStore(cache.subscribe, cache.get, cache.get);
  const key = ids.join(",");
  useEffect(() => { if (key) cache.ensure(key.split(",")); }, [cache, key]);
  return useMemo(() => (deviceId: string) => known.get(deviceId) === true, [known]);
}
