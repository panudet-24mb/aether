// People as walking figures (docs/platform/digital-twin.md, "Lifelike hospital" and "Honest positioning"). Aether
// knows the zone a tag is in, never where in it: so a figure only ever wanders inside its CURRENT zone, along
// made-up paths, and a zone change walks it to the edge it shares with the new zone and into it. The walking is
// illustration; the legend says so. With motion reduced, figures stand still in their zone's fixed slots.
//
// Figures are built from a few instanced parts (legs, arms, torso, head, hair): 150 people are 6 draw calls.

import * as THREE from "three";
import type { TwinTheme } from "../../theme";
import { centroid, closestOnSegment, hash, insideWithMargin, randomInside, rng, segmentInside, type P } from "./geo";

export type WalkerSpec = { id: string; floorId: string; zoneKey: string; poly: P[]; slot: P; sos: boolean };
export type Obstacles = { anchors: P[]; blocked: { x: number; y: number; w: number; d: number }[] };

/** At most this many walking figures; the rest of a crowd stays as the plain tokens. */
export const MAX_FIGURES = 150;

type Look = { top: THREE.Color; bottom: THREE.Color; skin: THREE.Color; hair: THREE.Color; coat: boolean };

type Walker = {
  spec: WalkerSpec;
  pos: P;
  heading: number;
  target: P | null;
  path: P[];
  pauseUntil: number;
  speed: number;
  phase: number;
  moving: boolean;
  rand: () => number;
  look: Look;
};

/** Scrubs, white coats, patient gowns and visitors, chosen by a hash of the person's key (no role in the data). */
function lookFor(id: string, theme: TwinTheme): Look {
  const r = rng(hash(id));
  const pick = r();
  const skins = [0xf1d0b5, 0xd9a882, 0xb47b57, 0x8a5a3c, 0xe8c3a0];
  const hairs = [0x2b2220, 0x4a3426, 0x1c1c1c, 0x6b4a2f, 0x9c9a96];
  const skin = new THREE.Color(skins[Math.floor(r() * skins.length)]), hair = new THREE.Color(hairs[Math.floor(r() * hairs.length)]);
  if (pick < 0.4) {
    const scrub = [new THREE.Color(theme.accent).lerp(new THREE.Color(0x1e5a52), 0.45), new THREE.Color(theme.blue).lerp(new THREE.Color(0x1f3a63), 0.4), new THREE.Color(0x3f7e86)][Math.floor(r() * 3)];
    return { top: scrub, bottom: scrub.clone().multiplyScalar(0.85), skin, hair, coat: false };
  }
  if (pick < 0.65) return { top: new THREE.Color(0xd3dad9), bottom: new THREE.Color(0x2c3640), skin, hair, coat: true };
  if (pick < 0.85) return { top: new THREE.Color(0xa6c0d4), bottom: new THREE.Color(0xa6c0d4), skin, hair, coat: false };
  const casual = [0x8a5a6b, 0x55707f, 0x7a7350, 0x5a5f8a, 0x9a6a3c][Math.floor(r() * 5)];
  return { top: new THREE.Color(casual), bottom: new THREE.Color(0x30353a), skin, hair, coat: false };
}

type Part = "leg" | "arm" | "torso" | "head" | "hair" | "capsule";

export class Crowd {
  readonly group = new THREE.Group();
  private walkers = new Map<string, Walker>();
  private meshes: Partial<Record<Part, THREE.InstancedMesh>> = {};
  private capacity = 0;
  private detail: "figure" | "capsule" = "figure";
  private obstacles = new Map<string, Obstacles>();
  private world: ((floorId: string, x: number, y: number, h: number) => THREE.Vector3 | null) | null = null;
  private tmp = { m: new THREE.Matrix4(), base: new THREE.Matrix4(), part: new THREE.Matrix4(), q: new THREE.Quaternion(), q2: new THREE.Quaternion(), v: new THREE.Vector3(), s: new THREE.Vector3(), e: new THREE.Euler(), e2: new THREE.Euler() };

  constructor(private theme: TwinTheme) { this.group.name = "crowd"; }

  /** Where people may pause and what they must walk around, per zone key. */
  setObstacles(map: Map<string, Obstacles>) { this.obstacles = map; }

  get size() { return this.walkers.size; }

  /** The people now: new ones appear at their slot, gone ones leave, a zone change starts a walk across. */
  setPeople(list: WalkerSpec[], world: (floorId: string, x: number, y: number, h: number) => THREE.Vector3 | null, detail: "figure" | "capsule", now: number) {
    this.world = world;
    const seen = new Set<string>();
    for (const spec of list.slice(0, MAX_FIGURES)) {
      seen.add(spec.id);
      const w = this.walkers.get(spec.id);
      if (!w) {
        const rand = rng(hash(`walk:${spec.id}`));
        this.walkers.set(spec.id, { spec, pos: [...spec.slot] as P, heading: rand() * Math.PI * 2, target: null, path: [], pauseUntil: now + rand() * 3000, speed: 0.75 + rand() * 0.45, phase: rand() * 6.28, moving: false, rand, look: lookFor(spec.id, this.theme) });
        continue;
      }
      if (w.spec.zoneKey !== spec.zoneKey) {
        if (w.spec.floorId !== spec.floorId) {
          // Another floor: it simply appears in its new zone (no stairs are walked).
          w.pos = [...spec.slot] as P;
          w.path = [];
        } else {
          // Walk to the edge the two zones share (the closest boundary point towards the new zone), then in.
          const exit = this.edgeTowards(w.spec.poly, centroid(spec.poly));
          const entry = this.edgeTowards(spec.poly, exit, 0.5);
          w.path = [exit, entry];
        }
        w.target = null;
        w.pauseUntil = 0;
      }
      w.spec = spec;
    }
    for (const id of [...this.walkers.keys()]) if (!seen.has(id)) this.walkers.delete(id);
    if (detail !== this.detail || this.walkers.size > this.capacity) this.rebuild(detail);
  }

  /** A point on the polygon's boundary closest to `towards`, pulled `inset` metres inside. */
  private edgeTowards(poly: P[], towards: P, inset = 0.35): P {
    let best: { at: P; d: number } | null = null;
    for (let i = 0; i < poly.length; i++) {
      const c = closestOnSegment(towards, poly[i], poly[(i + 1) % poly.length]);
      if (!best || c.d < best.d) best = { at: c.at, d: c.d };
    }
    const at = best!.at, c = centroid(poly);
    const len = Math.hypot(c[0] - at[0], c[1] - at[1]) || 1;
    return [at[0] + ((c[0] - at[0]) / len) * inset, at[1] + ((c[1] - at[1]) / len) * inset];
  }

  private rebuild(detail: "figure" | "capsule") {
    this.disposeMeshes();
    this.detail = detail;
    this.capacity = Math.max(32, Math.ceil(this.walkers.size * 1.25));
    const std = () => new THREE.MeshStandardMaterial({ color: 0xffffff, roughness: 0.7, metalness: 0.02 });
    const make = (part: Part, geo: THREE.BufferGeometry, per: number) => {
      const mesh = new THREE.InstancedMesh(geo, std(), this.capacity * per);
      mesh.name = `crowd:${part}`;
      mesh.frustumCulled = false;
      mesh.raycast = () => {};
      this.meshes[part] = mesh;
      this.group.add(mesh);
    };
    if (detail === "capsule") {
      make("capsule", new THREE.CapsuleGeometry(0.19, 0.7, 3, 8), 1);
      return;
    }
    // Pivots at the hip and the shoulder: limbs hang down from the origin.
    const leg = new THREE.BoxGeometry(0.13, 0.84, 0.15); leg.translate(0, -0.42, 0);
    const arm = new THREE.BoxGeometry(0.09, 0.62, 0.1); arm.translate(0, -0.31, 0);
    make("leg", leg, 2);
    make("arm", arm, 2);
    make("torso", new THREE.BoxGeometry(0.38, 0.6, 0.22), 1);
    make("head", new THREE.IcosahedronGeometry(0.12, 1), 1);
    make("hair", new THREE.SphereGeometry(0.128, 10, 6, 0, Math.PI * 2, 0, Math.PI * 0.55), 1);
  }

  private walkable(key: string, poly: P[], p: P): boolean {
    if (!insideWithMargin(p, poly, 0.3)) return false;
    const ob = this.obstacles.get(key);
    return !ob || !ob.blocked.some((b) => Math.abs(p[0] - b.x) * 2 < b.w + 0.5 && Math.abs(p[1] - b.y) * 2 < b.d + 0.5);
  }

  /** The next place to go: often a spot by the furniture (a bedside, a counter), else a free point of the zone. */
  private pickTarget(w: Walker): P | null {
    const { poly, zoneKey } = w.spec;
    const ob = this.obstacles.get(zoneKey);
    for (let i = 0; i < 12; i++) {
      let p: P | null;
      if (ob?.anchors.length && w.rand() < 0.45) p = ob.anchors[Math.floor(w.rand() * ob.anchors.length)];
      else p = randomInside(poly, 0.4, w.rand, 20);
      if (!p || !this.walkable(zoneKey, poly, p) || !segmentInside(w.pos, p, poly, 0.15)) continue;
      // The straight walk must not cross furniture (sampled every 0.4 m).
      const n = Math.ceil(Math.hypot(p[0] - w.pos[0], p[1] - w.pos[1]) / 0.4);
      let clear = true;
      for (let k = 1; k < n && clear; k++) clear = this.walkable(zoneKey, poly, [w.pos[0] + ((p[0] - w.pos[0]) * k) / n, w.pos[1] + ((p[1] - w.pos[1]) * k) / n]);
      if (clear) return p;
    }
    return null;
  }

  /**
   * One frame. `motion` false (reduced motion): everyone stands at their slot in their zone. A paused view simply
   * draws no frames, so figures freeze where they are.
   */
  step(now: number, dt: number, motion: boolean) {
    const d = Math.min(0.1, Math.max(0, dt));
    for (const w of this.walkers.values()) {
      if (!motion) {
        w.pos = [...w.spec.slot] as P;
        w.moving = false;
        w.path = [];
        continue;
      }
      if (w.spec.sos) { w.moving = false; continue; }
      if (!w.target) {
        if (w.path.length) w.target = w.path.shift()!;
        else if (now >= w.pauseUntil) w.target = this.pickTarget(w);
      }
      if (!w.target) { w.moving = false; continue; }
      const dx = w.target[0] - w.pos[0], dy = w.target[1] - w.pos[1], dist = Math.hypot(dx, dy);
      if (dist < 0.08) {
        w.target = null;
        w.moving = false;
        // Pauses: longer at a bedside or a counter, short in passing.
        if (!w.path.length) w.pauseUntil = now + 1500 + w.rand() * 6000;
        continue;
      }
      const step = Math.min(dist, w.speed * d);
      w.pos = [w.pos[0] + (dx / dist) * step, w.pos[1] + (dy / dist) * step];
      // Turn smoothly towards the way it walks.
      const want = Math.atan2(dx, dy);
      let dh = want - w.heading;
      dh = Math.atan2(Math.sin(dh), Math.cos(dh));
      w.heading += dh * Math.min(1, d * 8);
      w.phase += w.speed * d * 5.2;
      w.moving = true;
    }
    this.draw(now, motion);
  }

  private draw(now: number, motion: boolean) {
    const { m, base, part, q, q2, v, s, e, e2 } = this.tmp;
    const t = now / 1000;
    let i = 0;
    for (const w of this.walkers.values()) {
      const at = this.world?.(w.spec.floorId, w.pos[0], w.pos[1], 0);
      if (!at) continue;
      const swing = w.moving ? Math.sin(w.phase) * 0.55 : 0;
      const bob = w.moving ? Math.abs(Math.sin(w.phase)) * 0.035 : Math.sin(t * 1.3 + w.phase) * (motion ? 0.004 : 0);
      const kneel = w.spec.sos;
      q.setFromEuler(e.set(0, w.heading, 0));
      base.compose(at, q, s.set(1, 1, 1));
      const put = (name: Part, index: number, x: number, y: number, z: number, rx: number, rz: number, color: THREE.Color) => {
        const mesh = this.meshes[name];
        if (!mesh) return;
        part.compose(v.set(x, y, z), q2.setFromEuler(e2.set(rx, 0, rz)), s.set(1, 1, 1));
        mesh.setMatrixAt(index, m.multiplyMatrices(base, part));
        mesh.setColorAt(index, color);
      };
      if (this.detail === "capsule") {
        put("capsule", i, 0, 0.62 + bob, 0, 0, 0, kneel ? new THREE.Color(this.theme.danger) : w.look.top);
        i++;
        continue;
      }
      const hip = kneel ? 0.5 : 0.86 + bob;
      // Legs: swing forward and back; kneeling, one knee down and one leg folded forward.
      put("leg", i * 2, -0.1, hip, 0, kneel ? 1.3 : swing, 0, w.look.bottom);
      put("leg", i * 2 + 1, 0.1, hip, kneel ? 0.25 : 0, kneel ? -0.2 : -swing, 0, w.look.bottom);
      put("torso", i, 0, hip + 0.31, 0, kneel ? 0.15 : 0, 0, w.look.top);
      // Arms: opposite to the legs; an SOS raises one arm and waves (still with motion reduced).
      const wave = kneel ? Math.PI * 0.9 + (motion ? Math.sin(t * 6) * 0.35 : 0) : 0;
      const sleeve = w.look.coat ? w.look.top : w.look.top.clone().lerp(w.look.skin, 0.35);
      put("arm", i * 2, -0.24, hip + 0.58, 0, kneel ? 0 : -swing * 0.8, kneel ? -wave : 0.06, sleeve);
      put("arm", i * 2 + 1, 0.24, hip + 0.58, 0, kneel ? 0.3 : swing * 0.8, -0.06, sleeve);
      put("head", i, 0, hip + 0.76, 0, 0, 0, w.look.skin);
      put("hair", i, 0, hip + 0.79, -0.01, -0.15, 0, w.look.hair);
      i++;
    }
    for (const [name, mesh] of Object.entries(this.meshes) as [Part, THREE.InstancedMesh][]) {
      mesh.count = name === "leg" || name === "arm" ? i * 2 : i;
      mesh.instanceMatrix.needsUpdate = true;
      if (mesh.instanceColor) mesh.instanceColor.needsUpdate = true;
    }
  }

  private disposeMeshes() {
    for (const mesh of Object.values(this.meshes)) {
      if (!mesh) continue;
      this.group.remove(mesh);
      mesh.geometry.dispose();
      (mesh.material as THREE.Material).dispose();
      mesh.dispose();
    }
    this.meshes = {};
    this.capacity = 0;
  }

  dispose() { this.disposeMeshes(); this.walkers.clear(); }
}
