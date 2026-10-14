/**
 * Self-test for the overview's 3D layout (pure parts: hubs, rings, label priority, SOS diff), including the
 * 500-device synthetic site. There is no JS test runner in this repo, so this is a plain module:
 * `runOverview3dSelfTest()` returns the failures (empty means everything passed).
 *
 * Run it with the repo's own TypeScript, from `frontend/`:
 *   ./node_modules/.bin/tsc app/live/overview3d/overview3d.selftest.ts --outDir /tmp/ov3dtest --module commonjs \
 *     --target es2020 --skipLibCheck --strict
 *   node -e "const f=require('/tmp/ov3dtest/live/overview3d/overview3d.selftest.js').runOverview3dSelfTest(); console.log(f.length ? f.join('\n') : 'PASS'); process.exit(f.length ? 1 : 0)"
 */
import { buildHubs, footprint, layoutConstellation, newSos, pickLabels, rectsOverlap, UNKNOWN_HUB, wallSlots } from "./layout";
import { syntheticSite } from "./synthetic";
import { controllable, labelTier, poweredOn, shapeOf, type StageCard } from "./types";

const COLORS: Record<string, string> = { mint: "#a7f3d0", blue: "#80b7ff", violet: "#c4a7ff" };

export function runOverview3dSelfTest(): string[] {
  const fail: string[] = [];
  const check = (ok: boolean, what: string) => { if (!ok) fail.push(what); };
  const dist = (a: number[], b: number[]) => Math.hypot(a[0] - b[0], a[2] - b[2]);

  for (const n of [0, 1, 7, 60, 500]) {
    const site = syntheticSite(n, n ? undefined : 3);
    const hubs = buildHubs(site.cards, site.gateways, site.projects, COLORS, true);
    const out = layoutConstellation(hubs, site.cards);
    check(out.nodes.length === site.cards.length, `${n}: every device is placed once`);
    check(new Set(out.nodes.map((x) => x.card.key)).size === site.cards.length, `${n}: no device placed twice`);
    check(hubs.length === site.gateways.length, `${n}: showEmpty keeps every gateway`);
    // Hub footprints never overlap.
    for (let i = 0; i < out.hubs.length; i++) for (let j = i + 1; j < out.hubs.length; j++) {
      const a = out.hubs[i], b = out.hubs[j];
      check(dist(a.at, b.at) >= a.radius + b.radius - 1e-6, `${n}: hubs ${a.hub.name} and ${b.hub.name} overlap (${dist(a.at, b.at).toFixed(2)} < ${(a.radius + b.radius).toFixed(2)})`);
    }
    // Devices sit inside their own hub's footprint, and two devices never share a spot.
    const hubAt = new Map(out.hubs.map((h) => [h.hub.id, h]));
    for (const p of out.nodes) {
      const h = hubAt.get(p.hub)!;
      check(dist(p.at, h.at) < h.radius, `${n}: ${p.card.key} outside its hub`);
      check(p.hub === p.card.gatewayId, `${n}: ${p.card.key} on the wrong hub`);
    }
    for (let i = 0; i < out.nodes.length; i++) for (let j = i + 1; j < out.nodes.length; j++) {
      const a = out.nodes[i], b = out.nodes[j];
      if (a.hub === b.hub && Math.hypot(a.at[0] - b.at[0], a.at[1] - b.at[1], a.at[2] - b.at[2]) < 0.9) { check(false, `${n}: ${a.card.key} and ${b.card.key} too close`); break; }
    }
    // Same input, same layout.
    const again = layoutConstellation(buildHubs(site.cards, site.gateways, site.projects, COLORS, true), site.cards);
    check(JSON.stringify(again.nodes.map((x) => x.at)) === JSON.stringify(out.nodes.map((x) => x.at)), `${n}: layout is deterministic`);
    check(Number.isFinite(out.radius) && out.radius >= 8, `${n}: radius is finite`);
  }

  // A status change never moves a device.
  const site = syntheticSite(120);
  const hubs = buildHubs(site.cards, site.gateways, site.projects, COLORS, false);
  const before = layoutConstellation(hubs, site.cards);
  const flipped = site.cards.map((c, i) => (i % 3 === 0 ? { ...c, status: c.status === "online" ? "alert" as const : "online" as const, sos: !c.sos } : c));
  const after = layoutConstellation(hubs, flipped);
  const at = (o: typeof before) => new Map(o.nodes.map((x) => [x.card.key, x.at.join(",")]));
  const a1 = at(before), a2 = at(after);
  check([...a1].every(([k, v]) => a2.get(k) === v), "status change keeps positions");

  // Hubs: only gateways with devices unless showEmpty; unknown gateways share a hub; project order then name.
  const lone: StageCard = { ...site.cards[0], key: "x1", gatewayId: "nowhere", gatewayName: "GW ที่หายไป" };
  const h2 = buildHubs([lone, ...site.cards.slice(0, 5)], site.gateways, site.projects, COLORS, false);
  check(h2.some((h) => h.id === UNKNOWN_HUB && h.name === "GW ที่หายไป" && h.count === 1), "unknown gateway bucket");
  check(h2.every((h) => h.id === UNKNOWN_HUB || h.count > 0), "showEmpty=false drops empty gateways");
  check(h2[h2.length - 1].id === UNKNOWN_HUB, "unknown hub sorts last");
  const projected = h2.filter((h) => h.projectId);
  check(projected.every((h) => h.color === COLORS[site.projects.find((p) => p.id === h.projectId)!.color]), "hub colour is the project colour");

  // Labels: SOS > alert > controllable > rest, nearest first, capped.
  const base = site.cards[0];
  const sos = { ...base, key: "s", sos: true, status: "alert" as const };
  const alert = { ...base, key: "a", status: "alert" as const, sos: false };
  const ctl = { ...base, key: "c", status: "online" as const, sos: false, kind: "switch", deviceId: "d1", registered: true };
  const plain = { ...base, key: "p", status: "online" as const, sos: false, kind: "environment" };
  check(labelTier(sos) === 0 && labelTier(alert) === 1 && labelTier(ctl) === 2 && labelTier(plain) === 3, "label tiers");
  check(!controllable({ ...ctl, status: "offline" }) && !controllable({ ...ctl, deviceId: undefined }) && controllable(ctl), "controllable mirrors the board");
  const picked = pickLabels([{ key: "p", tier: 3, dist: 1 }, { key: "c", tier: 2, dist: 50 }, { key: "s", tier: 0, dist: 400 }, { key: "a", tier: 1, dist: 9 }, { key: "p2", tier: 3, dist: 0.5 }], 4);
  check(picked.join() === "s,a,c,p2", `label order ${picked.join()}`);
  const many = pickLabels(Array.from({ length: 500 }, (_, i) => ({ key: `k${i}`, tier: i % 4, dist: i })));
  check(many.length === 40, "labels capped at 40");
  const kept = pickLabels(Array.from({ length: 100 }, (_, i) => ({ key: `k${i}`, tier: 0, dist: i })), 40, "k99");
  check(kept.length === 40 && kept.includes("k99") && kept[0] === "k0", "focused label kept in the set");
  check(pickLabels([{ key: "a", tier: 0, dist: 1 }], 40, "gone").join() === "a", "a focused label no longer drawn is not invented");
  const noSwitch = (c: StageCard) => c.kind !== "tuya" && controllable(c);
  check(labelTier({ ...ctl, kind: "tuya" }, noSwitch) === 3 && labelTier({ ...ctl, kind: "tuya" }) === 2, "tuya without a switch ranks as plain");

  // Live SOS announcements only for new ones.
  check(newSos(new Set(["s"]), [sos, { ...sos, key: "s2" }]).map((c) => c.key).join() === "s2", "new SOS diff");

  check(shapeOf("door") === "slab" && shapeOf("sos") === "diamond" && shapeOf("whatever") === "sphere", "shapes by kind");
  check(poweredOn({ ...base, reading: { received_at: "", metrics: { sw1: 0, sw2: 1 } } }) === true && poweredOn({ ...base, reading: { received_at: "", metrics: { state: 0 } } }) === false && poweredOn({ ...base, reading: null }) === null, "power state from readings");
  check(footprint(0) > 0 && footprint(200) > footprint(10), "footprint grows with devices");

  // The default view's panels never overlap each other, stay inside the stage and below the HUD.
  for (const [W, H] of [[1208, 760], [1688, 940], [1208, 900], [900, 640], [2400, 1300], [700, 700], [375, 760]]) {
    const top = 64, bottom = H - 48;
    const sl = wallSlots(W, H, 6, top, bottom);
    const all = [...sl.kpi, ...Object.values(sl.lists)];
    check(sl.kpi.length === 6 && all.every((r) => r.w > 0 && r.h > 0), `${W}x${H}: six positive-size KPI slots`);
    check(sl.compact === W < 720 && (sl.compact ? Object.keys(sl.lists).length === 0 : Object.keys(sl.lists).length === 4), `${W}x${H}: compact wall below 720 px (lists go to the HUD)`);
    if (sl.compact) check(new Set(sl.kpi.map((r) => Math.round(r.y))).size === 2, `${W}x${H}: compact KPIs in two rows`);
    for (let i = 0; i < all.length; i++) {
      const r = all[i];
      check(r.x - r.w / 2 >= 0 && r.x + r.w / 2 <= W && r.y - r.h / 2 >= top - 0.01 && r.y + r.h / 2 <= H, `${W}x${H}: slot ${i} inside the stage`);
      for (let j = i + 1; j < all.length; j++) check(!rectsOverlap(r, all[j]), `${W}x${H}: slots ${i} and ${j} overlap`);
    }
    check(sl.centre > 0.3 && sl.centre < 1, `${W}x${H}: room left for the platform`);
  }
  for (const [W, H] of [[0, 0], [120, 900], [375, 100]]) {
    const sl = wallSlots(W, H, 6, 64, H - 48);
    check(sl.kpi.length === 0 && Object.keys(sl.lists).length === 0, `${W}x${H}: no wall on a tiny stage`);
  }
  return fail;
}
