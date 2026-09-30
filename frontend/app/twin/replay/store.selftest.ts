/**
 * Self-test for the replay store's pure parts (stateAt by binary search, counts folding, interpolation, alerts at T).
 * No JS test runner in this repo: `runReplaySelfTest()` returns the failures (empty means everything passed).
 *
 * Run it from `frontend/`:
 *   ./node_modules/.bin/tsc app/twin/replay/store.ts app/twin/replay/store.selftest.ts \
 *     --outDir /tmp/replaytest --module commonjs --target es2020 --skipLibCheck --strict
 *   node -e "const f=require('/tmp/replaytest/replay/store.selftest.js').runReplaySelfTest(); console.log(f.length ? f.join('\n') : 'PASS'); process.exit(f.length ? 1 : 0)"
 */
import type { TwinReplay, TwinState } from "../api";
import { bucketAt, indexReplay, lastAtOrBefore, peopleAt, stateAt, valueAt } from "./store";

export function runReplaySelfTest(): string[] {
  const fail: string[] = [];
  const check = (ok: boolean, what: string) => { if (!ok) fail.push(what); };
  const from = Date.parse("2026-09-30T10:00:00Z");
  const at = (min: number) => from + min * 60e3;

  // Binary search against a linear scan, on random sorted arrays.
  let seed = 7;
  const rnd = () => ((seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648);
  for (let k = 0; k < 200; k++) {
    const xs = Array.from({ length: Math.floor(rnd() * 30) }, () => Math.floor(rnd() * 100)).sort((a, b) => a - b);
    const t = Math.floor(rnd() * 110) - 5;
    let lin = -1;
    xs.forEach((x, i) => { if (x <= t) lin = i; });
    const got = lastAtOrBefore(xs, t);
    check(got === lin || (got >= 0 && lin >= 0 && xs[got] === xs[lin]), `lastAtOrBefore ${JSON.stringify(xs)} ${t}: ${got} vs ${lin}`);
  }

  // Named: keyframe plus changes; a person leaving the site is null.
  const named: TwinReplay = {
    site_id: "s", from: new Date(from).toISOString(), to: new Date(at(30)).toISOString(), bucket_sec: 300, generated_at: "", people_mode: "named",
    env: { buckets: 6, series: { d1: { t: [20, 22, null, 26, null, null], h: [50, null, null, null, null, null], motion: [0, 1, null, 0, null, null], door: [null, 1, null, 0, null, null] } } },
    people: { mode: "named", key: { a: "g1", b: null }, names: { a: "สมศรี", b: "สมชาย" }, deltas: [[at(5), "a", "g2"], [at(8), "b", "g1"], [at(12), "a", null], [at(20), "b", "g2"]] },
    alerts: { key: [], items: [{ id: "x", severity: "critical", event: "button", sos: true, hazard: false, gateway_id: "g1", device_id: "d1", title: "SOS", opened_at: new Date(at(10)).toISOString(), acked_at: new Date(at(14)).toISOString(), resolved_at: new Date(at(18)).toISOString() }] },
  };
  const ix = indexReplay(named);
  const expect: [number, Record<string, string | null>][] = [[at(0), { a: "g1", b: null }], [at(5), { a: "g2", b: null }], [at(9), { a: "g2", b: "g1" }], [at(15), { a: null, b: "g1" }], [at(29), { a: null, b: "g2" }]];
  for (const [t, want] of expect) {
    const got = peopleAt(ix, t).people;
    for (const [pid, g] of Object.entries(want)) check(got.get(pid) === g, `named at +${(t - from) / 60e3} min: ${pid} ${got.get(pid)} want ${g}`);
  }
  // Counts: the server folded the same moves into ±1 per gateway.
  const counts: TwinReplay = { ...named, people_mode: "counts", people: { mode: "counts", key_counts: { g1: 1 }, count_deltas: [[at(5), "g1", -1], [at(5), "g2", 1], [at(8), "g1", 1], [at(12), "g2", -1], [at(20), "g1", -1], [at(20), "g2", 1]] } };
  const cx = indexReplay(counts);
  for (const [t, want] of expect) {
    const c = peopleAt(cx, t).counts;
    const byGw = new Map<string, number>();
    for (const g of Object.values(want)) if (g) byGw.set(g, (byGw.get(g) ?? 0) + 1);
    for (const g of ["g1", "g2"]) check((c.get(g) ?? 0) === (byGw.get(g) ?? 0), `counts at +${(t - from) / 60e3} min: ${g} ${c.get(g) ?? 0} want ${byGw.get(g) ?? 0}`);
  }

  // Buckets and interpolation: mid-way between two buckets, the last known value over a gap.
  check(bucketAt(ix, at(7.5)).i === 1 && Math.abs(bucketAt(ix, at(7.5)).frac - 0.5) < 1e-9, "bucket of +7.5 min");
  check(valueAt([20, 22, null], 0, 0.5) === 21, "interpolated 21");
  check(valueAt([20, 22, null], 2, 0.3) === 22, "carried over a gap");
  check(valueAt([null, null], 1, 0) === undefined, "nothing reported");

  // The whole state: devices take values, alerts open/acknowledged/resolved by time, people with names.
  const base: TwinState = {
    server_time: "", site_id: "s", demo: true, layout_revision: {},
    devices: [{ id: "d1", kind: "device", name: "ห้อง", floor_id: "f", x: 0, y: 0, z: 0, online: true, last_at: null, alert: false, sos: false }, { id: "g1", kind: "gateway", name: "GW", floor_id: "f", x: 0, y: 0, z: 0, online: true, last_at: null, alert: false, sos: false }],
    presence: { mode: "named", counts: [] }, alerts: [],
  };
  const s11 = stateAt(ix, base, at(11));
  check(s11.alerts.length === 1 && s11.alerts[0].status === "open" && s11.devices[0].sos, "SOS open at +11");
  const s15 = stateAt(ix, base, at(15));
  check(s15.alerts.length === 1 && s15.alerts[0].status === "acknowledged" && !s15.devices[0].sos, "acknowledged at +15");
  check(stateAt(ix, base, at(19)).alerts.length === 0, "resolved at +19");
  const s7 = stateAt(ix, base, at(7.5));
  check(s7.devices[0].t === 22 && s7.devices[0].door === 1, `values at +7.5: ${s7.devices[0].t} door ${s7.devices[0].door}`);
  check((s7.presence.people ?? []).some((p) => p.pid === "a" && p.name === "สมศรี" && p.gateway_id === "g2"), "named person at +7.5");
  check(stateAt(cx, base, at(9)).presence.people === undefined, "counts state has no people list");
  return fail;
}
