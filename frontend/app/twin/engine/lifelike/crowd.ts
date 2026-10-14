// People as walking figures (docs/platform/digital-twin.md, "Lifelike hospital" and "Honest positioning"). Aether
// knows the zone a tag is in, never where in it: so a figure only ever wanders inside its CURRENT zone, along
// made-up paths, and a zone change walks it to the edge it shares with the new zone and into it. The walking is
// illustration; the legend says so. With motion reduced, figures stand still in their zone's fixed slots.
//
// Figures are built from a few instanced parts (legs, arms, torso, head, hair): 150 people are 6 draw calls.

import * as THREE from "three";
import type { TwinTheme } from "../../theme";
import { centroid, closestOnSegment, hash, inside, insideWithMargin, randomInside, rng, segmentInside, type P } from "./geo";

export type WalkerSpec = { id: string; floorId: string; zoneKey: string; poly: P[]; slot: P; sos: boolean };
export type Obstacles = { anchors: P[]; blocked: { x: number; y: number; w: number; d: number }[] };

/** At most this many walking figures; the rest of a crowd stays as the plain tokens. */
export const MAX_FIGURES = 150;

type Outfit = "scrubs" | "coat" | "gown" | "visitor";
type Look = { outfit: Outfit; top: THREE.Color; bottom: THREE.Color; sleeve: THREE.Color; shoe: THREE.Color; skin: THREE.Color; hair: THREE.Color };

type Walker = {
  spec: WalkerSpec;
  /** The slot moved off any furniture (computed once per zone). */
  home: P;
  /** Speed of a zone-change walk (fast enough to finish in about 3 s); 0 while wandering. */
  transit: number;
  /** Scene position, reused every frame. */
  at: THREE.Vector3;
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
  const skins = [0xe9c6a8, 0xd2a07c, 0xae7752, 0x845638, 0xe0bb98];
  const hairs = [0x2b2220, 0x4a3426, 0x1c1c1c, 0x6b4a2f, 0x8f8c88];
  const skin = new THREE.Color(skins[Math.floor(r() * skins.length)]), hair = new THREE.Color(hairs[Math.floor(r() * hairs.length)]);
  const bare = skin.clone();
  if (pick < 0.4) {
    const scrub = [new THREE.Color(theme.accent).lerp(new THREE.Color(0x1e5a52), 0.45), new THREE.Color(theme.blue).lerp(new THREE.Color(0x1f3a63), 0.4), new THREE.Color(0x3f7e86)][Math.floor(r() * 3)];
    return { outfit: "scrubs", top: scrub, bottom: scrub.clone().multiplyScalar(0.88), sleeve: bare, shoe: new THREE.Color(0xc9d0d0), skin, hair };
  }
  if (pick < 0.65) return { outfit: "coat", top: new THREE.Color(0xd3dad9), bottom: new THREE.Color(0x2c3640), sleeve: new THREE.Color(0xd3dad9), shoe: new THREE.Color(0x24292d), skin, hair };
  if (pick < 0.85) return { outfit: "gown", top: new THREE.Color(0x9fb9cf), bottom: bare, sleeve: bare, shoe: new THREE.Color(0xc9d6de), skin, hair };
  const casual = new THREE.Color([0x8a5a6b, 0x55707f, 0x7a7350, 0x5a5f8a, 0x9a6a3c][Math.floor(r() * 5)]);
  return { outfit: "visitor", top: casual, bottom: new THREE.Color(0x30394a), sleeve: casual, shoe: new THREE.Color(0x2a2522), skin, hair };
}

/** Child offsets: a shoe at the foot of its leg, eyes on the face, a hand at the end of its arm. */
const SHOE: [number, number, number] = [0, -0.82, 0.035], HAND: [number, number, number] = [0, -0.62, 0];
const EYE_L: [number, number, number] = [-0.036, 0.012, 0.098], EYE_R: [number, number, number] = [0.036, 0.012, 0.098];

/** Beyond this distance from the camera a figure is drawn as a plain capsule. */
const LOD_DISTANCE = 25;

type Part = "leg" | "shoe" | "torso" | "shoulders" | "skirt" | "neck" | "head" | "hair" | "eye" | "arm" | "hand" | "capsule";
/** Instances per figure of each part. */
const PER: Record<Part, number> = { leg: 2, shoe: 2, torso: 1, shoulders: 1, skirt: 1, neck: 1, head: 1, hair: 1, eye: 2, arm: 2, hand: 2, capsule: 1 };

export class Crowd {
  readonly group = new THREE.Group();
  private walkers = new Map<string, Walker>();
  private meshes: Partial<Record<Part, THREE.InstancedMesh>> = {};
  private capacity = 0;
  private detail: "figure" | "capsule" = "figure";
  private obstacles = new Map<string, Obstacles>();
  private origin: ((floorId: string) => THREE.Vector3 | null) | null = null;
  private tmp = { m: new THREE.Matrix4(), base: new THREE.Matrix4(), child: new THREE.Matrix4(), local: new THREE.Matrix4(), q: new THREE.Quaternion(), q2: new THREE.Quaternion(), v: new THREE.Vector3(), s: new THREE.Vector3(), e: new THREE.Euler(), e2: new THREE.Euler() };
  private eyeColor = new THREE.Color(0x15191c);
  private sosColor: THREE.Color;
  private count: Record<Part, number> = { leg: 0, shoe: 0, torso: 0, shoulders: 0, skirt: 0, neck: 0, head: 0, hair: 0, eye: 0, arm: 0, hand: 0, capsule: 0 };

  constructor(private theme: TwinTheme) { this.group.name = "crowd"; this.sosColor = new THREE.Color(theme.danger); }

  /** Where people may pause and what they must walk around, per zone key. */
  setObstacles(map: Map<string, Obstacles>) { this.obstacles = map; }

  get size() { return this.walkers.size; }

  /**
   * The people now. A figure is kept by its key (the engine keys counts and tracks by slot, named people by tag), so a
   * refresh never re-dresses or snaps anyone. New keys appear at their slot, gone keys leave. A key whose zone changes
   * (named people only) walks to the edge the two zones share and in, in about 3 s, when that path stays inside the
   * two zones; otherwise, or on another floor, it appears at its new slot.
   */
  setPeople(list: WalkerSpec[], origin: (floorId: string) => THREE.Vector3 | null, detail: "figure" | "capsule", now: number) {
    this.origin = origin;
    const seen = new Set<string>();
    for (const spec of list.slice(0, MAX_FIGURES)) {
      seen.add(spec.id);
      const w = this.walkers.get(spec.id);
      if (!w) {
        const rand = rng(hash(`walk:${spec.id}`));
        const home = this.freeSpot(spec);
        this.walkers.set(spec.id, { spec, home, transit: 0, at: new THREE.Vector3(), pos: [home[0], home[1]], heading: rand() * Math.PI * 2, target: null, path: [], pauseUntil: now + rand() * 3000, speed: 0.75 + rand() * 0.45, phase: rand() * 6.28, moving: false, rand, look: lookFor(spec.id, this.theme) });
        continue;
      }
      if (w.spec.zoneKey !== spec.zoneKey) {
        const home = this.freeSpot(spec);
        const walk = w.spec.floorId === spec.floorId ? this.crossing(w, spec) : null;
        if (walk) {
          w.path = walk.path;
          w.transit = Math.max(w.speed, walk.length / 3);
        } else {
          w.pos[0] = home[0]; w.pos[1] = home[1];
          w.path = [];
          w.transit = 0;
        }
        w.home = home;
        w.target = null;
        w.pauseUntil = 0;
      } else if (w.spec.slot[0] !== spec.slot[0] || w.spec.slot[1] !== spec.slot[1]) {
        w.home = this.freeSpot(spec);
      }
      w.spec = spec;
    }
    for (const id of [...this.walkers.keys()]) if (!seen.has(id)) this.walkers.delete(id);
    if (detail !== this.detail || this.walkers.size > this.capacity) this.rebuild(detail);
  }

  /**
   * The walk from the current spot into the new zone: to the old zone's edge towards the new zone, across it, in.
   * Null when the zones do not touch there or a leg would leave the two zones (through a wall or another room).
   */
  private crossing(w: Walker, spec: WalkerSpec): { path: P[]; length: number } | null {
    const from = w.spec.poly, to = spec.poly;
    const exit = this.edgeTowards(from, centroid(to));
    const entry = this.edgeTowards(to, exit, 0.5);
    if (!insideWithMargin(entry, to, 0.2) || Math.hypot(entry[0] - exit[0], entry[1] - exit[1]) > 1.2) return null;
    if (!segmentInside(w.pos, exit, from)) return null;
    // The short step across the boundary must stay in one zone or the other at every sample.
    for (let k = 0; k <= 6; k++) {
      const p: P = [exit[0] + ((entry[0] - exit[0]) * k) / 6, exit[1] + ((entry[1] - exit[1]) * k) / 6];
      if (!inside(p, from) && !inside(p, to)) return null;
    }
    const length = Math.hypot(exit[0] - w.pos[0], exit[1] - w.pos[1]) + Math.hypot(entry[0] - exit[0], entry[1] - exit[1]);
    return length / 1.9 <= 3 ? { path: [exit, entry], length } : null;
  }

  /** The slot, or the nearest walkable point around it when furniture covers it. */
  private freeSpot(spec: WalkerSpec): P {
    if (this.walkable(spec.zoneKey, spec.poly, spec.slot)) return [spec.slot[0], spec.slot[1]];
    for (let r = 0.4; r <= 2.4; r += 0.4) {
      for (let a = 0; a < 8; a++) {
        const p: P = [spec.slot[0] + Math.cos((a / 8) * Math.PI * 2) * r, spec.slot[1] + Math.sin((a / 8) * Math.PI * 2) * r];
        if (this.walkable(spec.zoneKey, spec.poly, p)) return p;
      }
    }
    return [spec.slot[0], spec.slot[1]];
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
    const std = () => new THREE.MeshStandardMaterial({ color: 0xffffff, roughness: 0.68, metalness: 0.02 });
    const make = (part: Part, geo: THREE.BufferGeometry) => {
      const mesh = new THREE.InstancedMesh(geo, std(), this.capacity * PER[part]);
      mesh.name = `crowd:${part}`;
      mesh.frustumCulled = false;
      mesh.raycast = () => {};
      mesh.count = 0;
      this.meshes[part] = mesh;
      this.group.add(mesh);
    };
    // Far figures (and every figure on the med and low tiers) are capsules.
    make("capsule", new THREE.CapsuleGeometry(0.19, 0.95, 3, 8));
    if (detail === "capsule") return;
    // About 1.75 m tall; the head is 1/7.5 of that; shoulders (0.41 m) wider than hips (0.3 m). Limbs hang from
    // their pivot at the origin (hip, shoulder) so a rotation swings them.
    const leg = new THREE.CapsuleGeometry(0.062, 0.72, 3, 8); leg.translate(0, -0.42, 0);
    const shoe = new THREE.CapsuleGeometry(0.046, 0.13, 2, 6); shoe.rotateX(Math.PI / 2); shoe.scale(1, 0.75, 1);
    const torso = new THREE.CylinderGeometry(0.19, 0.15, 0.56, 14); torso.scale(1, 1, 0.62);
    const shoulders = new THREE.CapsuleGeometry(0.075, 0.27, 3, 8); shoulders.rotateZ(Math.PI / 2); shoulders.scale(1, 1, 0.85);
    const skirt = new THREE.CylinderGeometry(0.155, 0.2, 1, 14); skirt.scale(1, 1, 0.66);
    const neck = new THREE.CylinderGeometry(0.045, 0.05, 0.1, 8);
    const head = new THREE.SphereGeometry(0.105, 12, 9); head.scale(1, 1.12, 1.04);
    const hair = new THREE.SphereGeometry(0.111, 12, 6, 0, Math.PI * 2, 0, Math.PI * 0.52); hair.scale(1, 1.12, 1.06);
    const eye = new THREE.SphereGeometry(0.012, 5, 4);
    const arm = new THREE.CapsuleGeometry(0.045, 0.5, 3, 8); arm.translate(0, -0.29, 0);
    const hand = new THREE.SphereGeometry(0.046, 7, 5); hand.scale(0.85, 1.1, 0.7);
    make("leg", leg); make("shoe", shoe); make("torso", torso); make("shoulders", shoulders); make("skirt", skirt); make("neck", neck);
    make("head", head); make("hair", hair); make("eye", eye); make("arm", arm); make("hand", hand);
  }

  /** Inside the zone (0.3 m from its edges) and off the furniture; `ignore` is a footprint the walk starts in. */
  private walkable(key: string, poly: P[], p: P, ignore?: { x: number; y: number; w: number; d: number }): boolean {
    if (!insideWithMargin(p, poly, 0.3)) return false;
    const ob = this.obstacles.get(key);
    return !ob || !ob.blocked.some((b) => b !== ignore && Math.abs(p[0] - b.x) * 2 < b.w + 0.5 && Math.abs(p[1] - b.y) * 2 < b.d + 0.5);
  }

  /** The next place to go: often a spot by the furniture (a bedside, a counter), else a free point of the zone. */
  private pickTarget(w: Walker): P | null {
    const { poly, zoneKey } = w.spec;
    const ob = this.obstacles.get(zoneKey);
    // A figure standing in a footprint (it arrived before the furniture was known) may walk out of it.
    const start = ob?.blocked.find((b) => Math.abs(w.pos[0] - b.x) * 2 < b.w + 0.5 && Math.abs(w.pos[1] - b.y) * 2 < b.d + 0.5);
    for (let i = 0; i < 12; i++) {
      let p: P | null;
      if (ob?.anchors.length && w.rand() < 0.45) p = ob.anchors[Math.floor(w.rand() * ob.anchors.length)];
      else p = randomInside(poly, 0.4, w.rand, 20);
      if (!p || !this.walkable(zoneKey, poly, p) || !segmentInside(w.pos, p, poly, 0.15)) continue;
      // The straight walk must not cross furniture (sampled every 0.4 m).
      const n = Math.ceil(Math.hypot(p[0] - w.pos[0], p[1] - w.pos[1]) / 0.4);
      let clear = true;
      for (let k = 1; k < n && clear; k++) clear = this.walkable(zoneKey, poly, [w.pos[0] + ((p[0] - w.pos[0]) * k) / n, w.pos[1] + ((p[1] - w.pos[1]) * k) / n], start);
      if (clear) return p;
    }
    return null;
  }

  /**
   * One frame. `motion` false (reduced motion): everyone stands at their slot in their zone. A paused view simply
   * draws no frames, so figures freeze where they are.
   */
  step(now: number, dt: number, motion: boolean, camera?: THREE.Vector3) {
    const d = Math.min(0.1, Math.max(0, dt));
    for (const w of this.walkers.values()) {
      // Reduced motion: everyone stands at their slot. An SOS kneels at its slot in its CURRENT zone, never mid-walk.
      if (!motion || w.spec.sos) {
        w.pos[0] = w.home[0]; w.pos[1] = w.home[1];
        w.moving = false;
        w.path = [];
        w.target = null;
        w.transit = 0;
        continue;
      }
      if (!w.target) {
        if (w.path.length) w.target = w.path.shift()!;
        else if (now >= w.pauseUntil) {
          w.transit = 0;
          w.target = this.pickTarget(w);
          // Nowhere to go from here right now: wait a moment rather than trying again every frame.
          if (!w.target) w.pauseUntil = now + 2000 + w.rand() * 2000;
        }
      }
      if (!w.target) { w.moving = false; continue; }
      const dx = w.target[0] - w.pos[0], dy = w.target[1] - w.pos[1], dist = Math.hypot(dx, dy);
      if (dist < 0.08) {
        w.target = null;
        w.moving = false;
        // Pauses: longer at a bedside or a counter, short in passing.
        if (!w.path.length) { w.pauseUntil = now + 1500 + w.rand() * 6000; w.transit = 0; }
        continue;
      }
      const speed = w.transit || w.speed;
      const step = Math.min(dist, speed * d);
      w.pos[0] += (dx / dist) * step;
      w.pos[1] += (dy / dist) * step;
      // Turn smoothly towards the way it walks.
      const want = Math.atan2(dx, dy);
      let dh = want - w.heading;
      dh = Math.atan2(Math.sin(dh), Math.cos(dh));
      w.heading += dh * Math.min(1, d * 8);
      w.phase += speed * d * 5.2;
      w.moving = true;
    }
    this.draw(now, motion, camera);
  }

  private draw(now: number, motion: boolean, camera?: THREE.Vector3) {
    const { m, base, child, local, q, q2, v, s, e, e2 } = this.tmp;
    const t = now / 1000;
    const count = this.count;
    for (const k of Object.keys(count) as Part[]) count[k] = 0;
    const eyeColor = this.eyeColor;
    /** Writes one part: base · T(x, y, z) · R(rx, 0, rz) · S, then an optional child offset (a hand on an arm). */
    const put = (name: Part, x: number, y: number, z: number, rx: number, rz: number, color: THREE.Color, sy = 1, offset?: [number, number, number]) => {
      const mesh = this.meshes[name];
      if (!mesh) return;
      local.compose(v.set(x, y, z), q2.setFromEuler(e2.set(rx, 0, rz)), s.set(1, sy, 1));
      m.multiplyMatrices(base, local);
      if (offset) m.multiply(child.makeTranslation(offset[0], offset[1], offset[2]));
      const i = count[name]++;
      mesh.setMatrixAt(i, m);
      mesh.setColorAt(i, color);
    };
    for (const w of this.walkers.values()) {
      const o = this.origin?.(w.spec.floorId);
      if (!o) continue;
      const at = w.at.set(o.x + w.pos[0], o.y, o.z + w.pos[1]);
      const swing = w.moving ? Math.sin(w.phase) * 0.5 : 0;
      const bob = w.moving ? Math.abs(Math.sin(w.phase)) * 0.03 : Math.sin(t * 1.3 + w.phase) * (motion ? 0.004 : 0);
      const kneel = w.spec.sos;
      q.setFromEuler(e.set(0, w.heading, 0));
      base.compose(at, q, s.set(1, 1, 1));
      const far = this.detail === "capsule" || (camera ? camera.distanceTo(at) > LOD_DISTANCE : false);
      if (far) {
        put("capsule", 0, (kneel ? 0.45 : 0.67) + bob, 0, 0, 0, kneel ? this.sosColor : w.look.top, kneel ? 0.6 : 1);
        continue;
      }
      const L = w.look, hip = kneel ? 0.5 : 0.88 + bob;
      // Legs and shoes: swing from the hip; kneeling, one leg folded under and one knee forward.
      const legA = kneel ? 1.35 : swing, legB = kneel ? -0.25 : -swing;
      put("leg", -0.085, hip, 0, legA, 0, L.bottom);
      put("leg", 0.085, hip, kneel ? 0.2 : 0, legB, 0, L.bottom);
      put("shoe", -0.085, hip, 0, legA, 0, L.shoe, 1, SHOE);
      put("shoe", 0.085, hip, kneel ? 0.2 : 0, legB, 0, L.shoe, 1, SHOE);
      const lean = kneel ? 0.18 : 0;
      put("torso", 0, hip + 0.28, 0, lean, 0, L.top);
      put("shoulders", 0, hip + 0.54, lean * 0.3, lean, 0, L.top);
      // A coat to mid-thigh, a gown to the knee; scrubs and visitors have none (scaled away).
      const skirt = L.outfit === "coat" ? 0.36 : L.outfit === "gown" ? 0.46 : 0.0001;
      put("skirt", 0, hip - skirt / 2 + 0.02, 0, lean, 0, L.top, skirt);
      put("neck", 0, hip + 0.61, lean * 0.4, 0, 0, L.skin);
      const hy = hip + 0.76;
      put("head", 0, hy, lean * 0.5, 0, 0, L.skin);
      put("hair", 0, hy + 0.012, lean * 0.5 - 0.008, -0.12, 0, L.hair);
      put("eye", 0, hy, lean * 0.5, 0, 0, eyeColor, 1, EYE_L);
      put("eye", 0, hy, lean * 0.5, 0, 0, eyeColor, 1, EYE_R);
      // Arms: opposite to the legs; an SOS raises one arm and waves (still with motion reduced).
      const wave = kneel ? Math.PI * 0.9 + (motion ? Math.sin(t * 6) * 0.35 : 0) : 0;
      const armA = kneel ? 0 : -swing * 0.8, armB = kneel ? 0.3 : swing * 0.8;
      put("arm", -0.205, hip + 0.53, 0, armA, kneel ? -wave : 0.05, L.sleeve);
      put("arm", 0.205, hip + 0.53, 0, armB, -0.05, L.sleeve);
      put("hand", -0.205, hip + 0.53, 0, armA, kneel ? -wave : 0.05, L.skin, 1, HAND);
      put("hand", 0.205, hip + 0.53, 0, armB, -0.05, L.skin, 1, HAND);
    }
    for (const [name, mesh] of Object.entries(this.meshes) as [Part, THREE.InstancedMesh][]) {
      mesh.count = count[name];
      mesh.visible = mesh.count > 0;
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
