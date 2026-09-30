/**
 * Self-test for the digital twin's pure parts (heat map, token slots, colour ramps). There is no JS test runner in
 * this repo, so this is a plain module: `runTwinSelfTest()` returns the failures (empty means everything passed).
 *
 * Run it with the repo's own TypeScript, from `frontend/`:
 *   ./node_modules/.bin/tsc app/twin/engine/heat.ts app/twin/engine/slots.ts app/twin/twin.selftest.ts \
 *     --outDir /tmp/twintest --module commonjs --target es2020 --skipLibCheck --strict
 *   node -e "const f=require('/tmp/twintest/twin.selftest.js').runTwinSelfTest(); console.log(f.length ? f.join('\n') : 'PASS'); process.exit(f.length ? 1 : 0)"
 */
import { RAMPS, comfortBand, comfortT, contrast, heatGrid, heatPixels, idwAt, pointIn, rampColor, staleWeight, type HeatSensor, type P } from "./engine/heat";
import { assignSlots, zoneSlots } from "./engine/slots";

export function runTwinSelfTest(): string[] {
  const fail: string[] = [];
  const check = (ok: boolean, what: string) => { if (!ok) fail.push(what); };

  // IDW: exact at a sensor, bounded by the sensors between them, NaN with nothing to go on.
  const sensors: HeatSensor[] = [{ x: 1, y: 1, value: 20, weight: 1 }, { x: 9, y: 1, value: 30, weight: 1 }, { x: 5, y: 7, value: 24, weight: 1 }];
  check(idwAt(sensors, 1, 1) === 20 && idwAt(sensors, 9, 1) === 30, "IDW is exact at the sensors");
  for (let x = 0; x <= 10; x += 0.5) for (let y = 0; y <= 8; y += 0.5) {
    const v = idwAt(sensors, x, y);
    check(v >= 20 - 1e-9 && v <= 30 + 1e-9, `IDW out of bounds at ${x},${y}: ${v}`);
  }
  check(Number.isNaN(idwAt([], 3, 3)) && Number.isNaN(idwAt([{ x: 0, y: 0, value: 5, weight: 0 }], 3, 3)), "IDW without usable sensors is NaN");
  check(Math.abs(idwAt([{ x: 0, y: 0, value: 10, weight: 1 }, { x: 10, y: 0, value: 20, weight: 1 }], 5, 0) - 15) < 1e-9, "IDW midpoint");
  check(staleWeight(60000) === 1 && staleWeight(15 * 60000) === 0 && staleWeight(10 * 60000) === 0.5, "stale weight");

  // Zone masking: a hot room never warms the room next door, and a zone without a sensor has no value.
  const zones: { points: P[] }[] = [
    { points: [[0, 0], [5, 0], [5, 4], [0, 4]] },
    { points: [[5, 0], [10, 0], [10, 4], [5, 4]] },
    { points: [[0, 4], [10, 4], [10, 8], [0, 8]] },
  ];
  const byZone: HeatSensor[][] = [[{ x: 2.5, y: 2, value: 40, weight: 1 }], [{ x: 7.5, y: 2, value: 5, weight: 1 }], []];
  const grid = heatGrid(10, 8, zones, (z) => byZone[z]);
  let hotLeaks = false, missing = false, outside = 0, empty = 0;
  for (let k = 0; k < grid.values.length; k++) {
    const z = grid.zone[k], v = grid.values[k];
    if (z === 1 && v !== 5) hotLeaks = true;
    if (z === 0 && v !== 40) missing = true;
    if (z === 2 && !Number.isNaN(v)) empty++;
    if (z < 0) outside++;
  }
  check(!hotLeaks && !missing, "heat stays inside its zone");
  check(empty === 0, "a zone without a sensor has no value");
  check(outside === 0, "every texel of a fully zoned floor belongs to a zone");
  const px = heatPixels(grid, "temperature", 0, 40, () => [10, 10, 10]);
  const alphaAt = (x: number, y: number) => px[(Math.floor(y / grid.sy) * grid.width + Math.floor(x / grid.sx)) * 4 + 3];
  check(alphaAt(2, 2) === 190 && alphaAt(5, 6) < 190, "sensor zones are solid, empty zones hatched");
  const sparse = heatGrid(10, 8, [zones[0]], () => []);
  let transparent = 0;
  const spx = heatPixels(sparse, "temperature", 0, 40, () => [0, 0, 0]);
  for (let k = 0; k < sparse.zone.length; k++) if (sparse.zone[k] < 0 && spx[k * 4 + 3] === 0) transparent++;
  check(transparent > 0, "texels outside every zone are transparent");

  // Token slots: inside the polygon, stable, and the same person keeps the same slot.
  const ell: P[] = [[0, 0], [8, 0], [8, 3], [3, 3], [3, 8], [0, 8]];
  const slots = zoneSlots(ell);
  check(slots.length > 10, `enough slots in an L-shaped room: ${slots.length}`);
  check(slots.every((p) => pointIn(p, ell)), "every slot is inside the zone");
  check(JSON.stringify(zoneSlots(ell)) === JSON.stringify(slots), "slots are deterministic");
  const a = assignSlots(["b", "a", "c"], slots), b = assignSlots(["c", "b", "a"], slots);
  check(JSON.stringify(a.get("a")) === JSON.stringify(b.get("a")) && JSON.stringify(a.get("c")) === JSON.stringify(b.get("c")), "a person's slot does not depend on input order");
  check(zoneSlots([[0, 0], [0.5, 0], [0.5, 0.5], [0, 0.5]]).length === 1, "a tiny zone has one slot at its centre");

  // Ramps: readable on the dark stage (#0b1418), at least 3:1 for graphics, and monotonic in lightness order ends.
  const stage: [number, number, number] = [11, 20, 24];
  for (const [kind, list] of Object.entries(RAMPS)) {
    for (const c of list) check(contrast(c, stage) >= 3, `${kind} ramp colour ${c} fails 3:1 on the stage`);
  }
  check(JSON.stringify(rampColor("temperature", 0, 0, 10)) === JSON.stringify(RAMPS.temperature[0]) && JSON.stringify(rampColor("temperature", 99, 0, 10)) === JSON.stringify(RAMPS.temperature[4]), "ramp clamps");
  // Comfort bands: a cold store in its own band is "in range", a normal room at 5 °C is far too cold.
  check(comfortT(23, comfortBand("room")) === 0 && comfortT(5, comfortBand("storage", [2, 8])) === 0, "inside the band is neutral");
  check(comfortT(12, comfortBand("storage", [2, 8])) === 1 && comfortT(10, comfortBand("storage", [2, 8])) === 0.5, "above the band ramps to +1 in 4 °C");
  check(comfortT(5, comfortBand("room")) === -1 && comfortT(21, comfortBand("room")) === -0.25, "below the band ramps to -1");
  check(JSON.stringify(comfortBand("room", [8, 2])) === JSON.stringify([22, 25]) && JSON.stringify(comfortBand("unknown-kind")) === JSON.stringify([22, 25]), "a bad or missing band falls back to the kind's default");
  check(Number.isNaN(comfortT(NaN, [22, 25])), "no reading, no colour");
  return fail;
}
