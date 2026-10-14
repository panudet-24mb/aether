// Devices as real fixtures (docs/platform/digital-twin.md, "Lifelike hospital"): a wall switch is a plate on the
// nearest wall of its zone at 1.2 m with a rocker that flips, a plug a socket at 0.3 m with a glowing indicator, an
// air conditioner a wall unit at 2.4 m breathing air when on, a light a ceiling panel with a pool of light under it.
// Sensors move to where they really hang (S1 on the wall, PIR and detectors on the ceiling, door sensors on the
// door frame). Clicking a fixture runs the same command flow as its badge.

import * as THREE from "three";
import type { Draft } from "../../../floorplan/model";
import type { TwinDevice } from "../../api";
import type { TwinTheme } from "../../theme";
import { closestOnSegment, inside, snapToWall, type P, type Segment } from "./geo";

export type FixtureKind = "switch" | "socket" | "light" | "ac" | "thermostat" | "blind" | "purifier" | "wallSensor" | "ceilingSensor" | "detector" | "doorSensor" | "leak";

/** What a device looks like in the room, from its class, readings and name (null: it stays a beacon). */
export function fixtureKind(d: TwinDevice): FixtureKind | null {
  if (d.kind !== "device") return null;
  const n = d.name.toLowerCase();
  if (d.class === "lighting") return "light";
  if (d.class === "switch") return d.power === undefined && /สวิตช์|switch|wall/.test(n) ? "switch" : "socket";
  if (d.class === "climate") return /วาล์ว|valve|trv|thermostat|เทอร์โม/.test(n) ? "thermostat" : "ac";
  if (d.class === "cover") return "blind";
  if (d.class === "fan") return "purifier";
  if (d.door !== undefined) return "doorSensor";
  if (/ควัน|smoke|แก๊ส|gas|\bco\b|คาร์บอน/.test(n)) return "detector";
  if (/น้ำรั่ว|leak|water/.test(n)) return "leak";
  if (/pir|เรดาร์|radar|motion|presence|ตรวจคน|ตรวจจับ/.test(n) || /msp01/.test(d.profile ?? "")) return "ceilingSensor";
  if (d.t !== undefined || d.h !== undefined) return "wallSensor";
  return null;
}

/** Fixtures that are switched on and off by a click (others open the side card). */
export const TOGGLES = new Set<FixtureKind>(["switch", "socket", "light", "purifier"]);
/** Fixtures that replace the device's beacon entirely (sensors keep a small beacon at their mount). */
export const REPLACES_BEACON = new Set<FixtureKind>(["switch", "socket", "light", "ac", "thermostat", "blind", "purifier"]);

const HEIGHT: Partial<Record<FixtureKind, number>> = { switch: 1.2, socket: 0.3, ac: 2.4, thermostat: 1.5, blind: 1.7, wallSensor: 1.6, doorSensor: 2.15 };

export type Mount = { x: number; y: number; h: number; normal: P | null; kind: FixtureKind };

/**
 * What the live view does not carry for some fixtures, read from the device's controls by the page: whether an AC or
 * thermostat is running (its switch / system mode), how far a cover is open (0 closed … 1 open).
 */
export type FixtureState = { running?: boolean; open?: number };

/** A device's controls state (Zigbee2MQTT / Tuya keys) → what its fixture shows. */
export function fixtureStateOf(state: Record<string, unknown>, cls: TwinDevice["class"]): FixtureState {
  if (cls === "cover") {
    const pos = state.position;
    if (typeof pos === "number" && Number.isFinite(pos)) return { open: Math.max(0, Math.min(1, pos / 100)) };
    return state.state === "OPEN" ? { open: 1 } : state.state === "CLOSE" ? { open: 0 } : {};
  }
  const sw = state.switch ?? state.state;
  if (sw === "ON" || sw === true) return { running: true };
  if (sw === "OFF" || sw === false) return { running: false };
  const mode = state.system_mode ?? state.mode;
  return typeof mode === "string" ? { running: mode.toLowerCase() !== "off" } : {};
}

/** Wall segments of a floor: drawn walls and every zone's outline. */
export function floorSegments(draft: Draft): Segment[] {
  const out: Segment[] = [];
  for (const w of draft.layout.walls) {
    const pts = (w.closed ? [...w.points, w.points[0]] : w.points) as P[];
    for (let i = 1; i < pts.length; i++) out.push({ a: pts[i - 1], b: pts[i], thickness: w.thickness });
  }
  for (const z of draft.layout.zones) {
    const pts = z.points as P[];
    if (pts.length < 3) continue;
    for (let i = 0; i < pts.length; i++) out.push({ a: pts[i], b: pts[(i + 1) % pts.length], thickness: 0.12 });
  }
  return out;
}

/** Where a device is mounted (plan x, y; height above its slab). */
export function mountFor(d: TwinDevice, draft: Draft, segments: Segment[]): Mount | null {
  const kind = fixtureKind(d);
  if (!kind) return null;
  const ceiling = draft.ceiling_m;
  const p: P = [d.x, d.y];
  if (kind === "light" || kind === "ceilingSensor" || kind === "detector") return { x: d.x, y: d.y, h: ceiling - 0.04, normal: null, kind };
  if (kind === "leak" || kind === "purifier") return { x: d.x, y: d.y, h: 0, normal: null, kind };
  if (kind === "doorSensor") {
    // On the frame of the nearest door, when one is within 2.5 m.
    let best: { x: number; y: number; d: number } | null = null;
    for (const it of draft.layout.items) {
      if (it.type !== "door") continue;
      const cx = it.x + it.w / 2, cy = it.y + it.h / 2, dist = Math.hypot(cx - d.x, cy - d.y);
      if (dist < 2.5 && (!best || dist < best.d)) best = { x: cx, y: cy, d: dist };
    }
    if (best) return { x: best.x, y: best.y, h: Math.min(2.15, ceiling - 0.1), normal: null, kind };
  }
  // Wall fixtures: the nearest wall of the device's own zone (drawn walls inside it count too).
  const zone = draft.layout.zones.find((z) => z.points.length >= 3 && inside(p, z.points as P[]));
  let segs = segments;
  if (zone) {
    const poly = zone.points as P[];
    // A segment belongs to the zone when it starts or ends inside it, or runs along one of its edges.
    segs = segments.filter((s) => inside(s.a, poly) || inside(s.b, poly) || poly.some((q, i) => closestOnSegment(q, s.a, s.b).d < 0.05 && closestOnSegment(poly[(i + 1) % poly.length], s.a, s.b).d < 0.05));
    if (!segs.length) segs = segments;
  }
  const snap = snapToWall(p, segs);
  const h = Math.min(HEIGHT[kind] ?? 1.4, ceiling - 0.3);
  return snap ? { x: snap.at[0], y: snap.at[1], h, normal: snap.normal, kind } : { x: d.x, y: d.y, h, normal: null, kind };
}

type Inst = { id: string; kind: FixtureKind; pos: THREE.Vector3; floorY: number; yaw: number; normal: THREE.Vector3 | null };

/** A soft round glow (light pools, airflow puffs). */
function softTexture(): THREE.CanvasTexture {
  const c = document.createElement("canvas");
  c.width = c.height = 64;
  const g = c.getContext("2d")!;
  const grad = g.createRadialGradient(32, 32, 0, 32, 32, 32);
  grad.addColorStop(0, "rgba(255,255,255,1)");
  grad.addColorStop(0.45, "rgba(255,255,255,0.45)");
  grad.addColorStop(1, "rgba(255,255,255,0)");
  g.fillStyle = grad;
  g.fillRect(0, 0, 64, 64);
  return new THREE.CanvasTexture(c);
}

/** The fixtures of the visible floors: instanced, rebuilt when the device set changes, recoloured on every poll. */
export class Fixtures {
  readonly group = new THREE.Group();
  private meshes: Record<string, THREE.InstancedMesh> = {};
  private insts: Inst[] = [];
  private byKind = new Map<string, Inst[]>();
  private rocker = new Map<string, number>();
  private blindAt = new Map<string, number>();
  private states: Record<string, FixtureState> = {};
  private devices = new Map<string, TwinDevice>();
  private airflow: { points: THREE.Points; seeds: Float32Array; units: Inst[] } | null = null;
  private tex = softTexture();
  private key = "";
  private c = new THREE.Color();

  constructor(private theme: TwinTheme) { this.group.name = "fixtures"; }

  /** The ids of the fixture meshes' instances, for picking. */
  pickables(): THREE.Object3D[] { return Object.values(this.meshes); }

  /** Lays out the fixtures; `world` maps a plan point of a floor to the scene. */
  build(list: { d: TwinDevice; m: Mount }[], world: (floorId: string, x: number, y: number, h: number) => THREE.Vector3 | null, low: boolean) {
    const key = list.map((e) => `${e.d.id}:${e.m.kind}:${e.m.x.toFixed(2)}:${e.m.y.toFixed(2)}`).join("|") + `|${low}`;
    if (key === this.key) return;
    this.key = key;
    this.clear();
    this.insts = [];
    for (const { d, m } of list) {
      if (!REPLACES_BEACON.has(m.kind)) continue;
      const pos = world(d.floor_id, m.x, m.y, m.h);
      if (!pos) continue;
      const normal = m.normal ? new THREE.Vector3(m.normal[0], 0, m.normal[1]) : null;
      this.insts.push({ id: d.id, kind: m.kind, pos, floorY: pos.y - m.h, yaw: normal ? Math.atan2(normal.x, normal.z) : 0, normal });
    }
    this.byKind.clear();
    for (const i of this.insts) this.byKind.set(i.kind, [...(this.byKind.get(i.kind) ?? []), i]);
    const box = () => new THREE.BoxGeometry(1, 1, 1);
    const std = () => new THREE.MeshStandardMaterial({ color: 0xffffff, roughness: 0.55, metalness: 0.05 });
    const glow = () => new THREE.MeshBasicMaterial({ color: 0xffffff, toneMapped: false });
    const add = (name: string, kinds: FixtureKind[], geo: THREE.BufferGeometry, mat: THREE.Material, pick: boolean, per = 1) => {
      const n = kinds.reduce((s, k) => s + (this.byKind.get(k)?.length ?? 0), 0) * per;
      if (!n) { geo.dispose(); mat.dispose(); return; }
      const mesh = new THREE.InstancedMesh(geo, mat, n);
      mesh.name = `fixture:${name}`;
      mesh.userData.ids = kinds.flatMap((k) => (this.byKind.get(k) ?? []).map((i) => i.id));
      mesh.userData.kinds = kinds;
      if (!pick) mesh.raycast = () => {};
      this.meshes[name] = mesh;
      this.group.add(mesh);
    };
    add("plate", ["switch", "socket", "thermostat"], box(), std(), true);
    add("rocker", ["switch"], box(), std(), true);
    add("led", ["switch", "socket", "thermostat", "ac", "purifier"], box(), glow(), false);
    add("ac", ["ac"], box(), std(), true);
    add("vent", ["ac"], box(), std(), false, 2);
    add("blind", ["blind"], box(), std(), true);
    add("panel", ["light"], box(), glow(), true);
    add("purifier", ["purifier"], new THREE.CylinderGeometry(0.2, 0.22, 0.75, low ? 8 : 16), std(), true);
    const pool = new THREE.PlaneGeometry(1, 1);
    pool.rotateX(-Math.PI / 2);
    add("pool", ["light"], pool, new THREE.MeshBasicMaterial({ map: this.tex, color: 0xffffff, transparent: true, depthWrite: false, blending: THREE.AdditiveBlending, toneMapped: false }), false);
    if (this.meshes.pool) this.meshes.pool.renderOrder = 2;
    // Airflow: a few soft puffs per running unit, recycled along the unit's blow direction.
    const units = [...(this.byKind.get("ac") ?? []), ...(this.byKind.get("purifier") ?? [])];
    if (units.length) {
      const per = low ? 8 : 18, n = units.length * per;
      const geo = new THREE.BufferGeometry();
      geo.setAttribute("position", new THREE.BufferAttribute(new Float32Array(n * 3), 3));
      const seeds = new Float32Array(n);
      for (let i = 0; i < n; i++) seeds[i] = (i * 0.618034) % 1;
      const points = new THREE.Points(geo, new THREE.PointsMaterial({ map: this.tex, size: 0.32, color: new THREE.Color(this.theme.blue).lerp(new THREE.Color(0xffffff), 0.6), transparent: true, opacity: 0.32, depthWrite: false, blending: THREE.AdditiveBlending, toneMapped: false }));
      points.frustumCulled = false;
      points.raycast = () => {};
      this.group.add(points);
      this.airflow = { points, seeds, units };
    }
  }

  setStates(map: Record<string, FixtureState>) { this.states = map; }

  /** New readings: remembers each device's state for the next frames. */
  setDevices(devices: TwinDevice[]) {
    this.devices.clear();
    for (const d of devices) this.devices.set(d.id, d);
  }

  /** Draws a frame: rockers glide to their state (or snap without motion), airflow drifts while units run. */
  step(t: number, dt: number, motion: boolean) {
    const m = new THREE.Matrix4(), q = new THREE.Quaternion(), s = new THREE.Vector3(), p = new THREE.Vector3(), e = new THREE.Euler();
    // A unit runs when its controls say so (an AC's switch, a thermostat's mode), else when the live view says on.
    const on = (id: string) => this.states[id]?.running ?? this.devices.get(id)?.on === 1;
    const online = (id: string) => this.devices.get(id)?.online !== false;
    const set = (name: string, i: number, pos: THREE.Vector3, yaw: number, w: number, h: number, d: number, local: [number, number, number] = [0, 0, 0], tilt = 0) => {
      const mesh = this.meshes[name];
      if (!mesh) return;
      q.setFromEuler(e.set(tilt, yaw, 0, "YXZ"));
      p.set(local[0], local[1], local[2]).applyQuaternion(q).add(pos);
      mesh.setMatrixAt(i, m.compose(p, q, s.set(w, h, d)));
    };
    const col = (name: string, i: number, color: THREE.Color) => this.meshes[name]?.setColorAt(i, color);
    const accent = new THREE.Color(this.theme.accent), warm = new THREE.Color(0xffe2a8), dim = new THREE.Color(0x3a4448), white = new THREE.Color(0xd5dcdb), off = new THREE.Color(0x101518);
    let k = 0;
    // Plates: switches, sockets, thermostats.
    for (const kind of ["switch", "socket", "thermostat"] as FixtureKind[]) for (const f of this.byKind.get(kind) ?? []) {
      const [w, h] = kind === "socket" ? [0.086, 0.086] : kind === "thermostat" ? [0.1, 0.1] : [0.086, 0.12];
      set("plate", k, f.pos, f.yaw, w, h, 0.012, [0, 0, 0.006]);
      col("plate", k++, white);
    }
    k = 0;
    for (const f of this.byKind.get("switch") ?? []) {
      const target = on(f.id) ? 0.25 : -0.25;
      const cur = this.rocker.get(f.id);
      const a = cur === undefined || !motion ? target : cur + (target - cur) * Math.min(1, dt * 12);
      this.rocker.set(f.id, a);
      set("rocker", k, f.pos, f.yaw, 0.044, 0.07, 0.014, [0, 0, 0.018], a);
      // The rocker takes a hint of the accent when on, so the state reads at a glance as well as by its tilt.
      col("rocker", k++, on(f.id) ? white.clone().lerp(accent, 0.35) : white.clone().multiplyScalar(0.82));
    }
    k = 0;
    // Indicators: a switch's dot, a socket's LED, a thermostat's screen, a unit's lamp.
    for (const kind of ["switch", "socket", "thermostat", "ac", "purifier"] as FixtureKind[]) for (const f of this.byKind.get(kind) ?? []) {
      const live = online(f.id);
      if (kind === "switch") set("led", k, f.pos, f.yaw, 0.014, 0.014, 0.004, [0, -0.047, 0.014]);
      else if (kind === "socket") set("led", k, f.pos, f.yaw, 0.012, 0.012, 0.004, [0.028, 0.028, 0.014]);
      else if (kind === "thermostat") set("led", k, f.pos, f.yaw, 0.07, 0.05, 0.004, [0, 0.01, 0.014]);
      else if (kind === "ac") set("led", k, f.pos, f.yaw, 0.04, 0.012, 0.004, [0.36, -0.06, 0.125]);
      else set("led", k, f.pos, f.yaw, 0.06, 0.012, 0.012, [0, 0.62, 0.2]);
      const lit = kind === "thermostat" ? live && (this.states[f.id]?.running ?? true) : on(f.id) && live;
      col("led", k++, lit ? (kind === "thermostat" ? accent.clone().multiplyScalar(0.8) : accent.clone().multiplyScalar(1.3)) : off);
    }
    k = 0;
    for (const f of this.byKind.get("ac") ?? []) {
      // A split-type indoor unit: a rounded body, a dark air outlet along its underside and a louver that opens when it runs.
      set("ac", k, f.pos, f.yaw, 0.9, 0.28, 0.22, [0, 0, 0.11]);
      col("ac", k, white);
      set("vent", k * 2, f.pos, f.yaw, 0.8, 0.04, 0.12, [0, -0.13, 0.17]);
      col("vent", k * 2, new THREE.Color(0x1a2124));
      set("vent", k * 2 + 1, f.pos, f.yaw, 0.78, 0.012, 0.07, [0, -0.15, 0.24], on(f.id) ? 0.6 : 0.05);
      col("vent", k * 2 + 1, white.clone().multiplyScalar(0.9));
      k++;
    }
    k = 0;
    // Blinds: hang from a fixed head rail, down by how closed the cover is (its controls' position; half when unknown).
    for (const f of this.byKind.get("blind") ?? []) {
      const target = this.states[f.id]?.open ?? 0.5;
      const cur = this.blindAt.get(f.id);
      const open = cur === undefined || !motion ? target : cur + (target - cur) * Math.min(1, dt * 2);
      this.blindAt.set(f.id, open);
      const h = 0.1 + (1 - open) * 1.3;
      set("blind", k, f.pos, f.yaw, 1.3, h, 0.025, [0, 0.75 - h / 2, 0.03]);
      col("blind", k++, new THREE.Color(this.theme.accent).lerp(white, 0.75));
    }
    k = 0;
    for (const f of this.byKind.get("light") ?? []) {
      const lit = on(f.id) && online(f.id);
      set("panel", k, f.pos, 0, 0.62, 0.035, 0.62);
      // Lit, but under the bloom threshold up close: the glow comes from the pool and the bloom at a distance.
      col("panel", k, lit ? warm.clone().multiplyScalar(0.86) : dim);
      const floorPos = f.pos.clone();
      floorPos.y = f.floorY + 0.03;
      set("pool", k, floorPos, 0, 3.4, 1, 3.4);
      col("pool", k++, lit ? warm.clone().multiplyScalar(0.075) : off.clone().multiplyScalar(0));
    }
    k = 0;
    for (const f of this.byKind.get("purifier") ?? []) { set("purifier", k, f.pos, 0, 1, 1, 1, [0, 0.375, 0]); col("purifier", k++, white); }
    for (const mesh of Object.values(this.meshes)) {
      mesh.instanceMatrix.needsUpdate = true;
      if (mesh.instanceColor) mesh.instanceColor.needsUpdate = true;
      mesh.computeBoundingSphere();
    }
    this.stepAirflow(t, motion, on);
  }

  private stepAirflow(t: number, motion: boolean, on: (id: string) => boolean) {
    const a = this.airflow;
    if (!a) return;
    a.points.visible = motion;
    if (!motion) return;
    const pos = a.points.geometry.getAttribute("position") as THREE.BufferAttribute;
    const per = pos.count / a.units.length;
    a.units.forEach((u, ui) => {
      const running = on(u.id);
      for (let j = 0; j < per; j++) {
        const i = ui * per + j;
        if (!running) { pos.setXYZ(i, 0, -1000, 0); continue; }
        const life = (t * 0.45 + a.seeds[i]) % 1;
        const spread = (a.seeds[(i * 7) % a.seeds.length] - 0.5) * 0.7;
        if (u.kind === "ac" && u.normal) {
          // Out of the unit's front and gently down into the room.
          const side = new THREE.Vector3(u.normal.z, 0, -u.normal.x);
          pos.setXYZ(i, u.pos.x + u.normal.x * (0.25 + life * 1.6) + side.x * spread, u.pos.y - 0.1 - life * life * 1.1, u.pos.z + u.normal.z * (0.25 + life * 1.6) + side.z * spread);
        } else {
          pos.setXYZ(i, u.pos.x + spread * 0.4 * life, u.pos.y + 0.8 + life * 1.0, u.pos.z + (a.seeds[(i * 3) % a.seeds.length] - 0.5) * 0.4 * life);
        }
      }
    });
    pos.needsUpdate = true;
  }

  private clear() {
    for (const o of [...this.group.children]) {
      this.group.remove(o);
      const g = (o as THREE.Mesh).geometry, mat = (o as THREE.Mesh).material as THREE.Material;
      g?.dispose();
      // The soft texture is shared and kept; everything else of the fixture is released.
      if (mat) { if ((mat as THREE.MeshBasicMaterial).map === this.tex) (mat as THREE.MeshBasicMaterial).map = null; mat.dispose(); }
      (o as THREE.InstancedMesh).dispose?.();
    }
    this.meshes = {};
    this.airflow = null;
  }

  /** The device a fixture hit belongs to. */
  idAt(object: THREE.Object3D, instanceId: number | undefined): string | null {
    const ids = object.userData.ids as string[] | undefined;
    return ids && instanceId !== undefined ? ids[instanceId] ?? null : null;
  }

  kindOf(id: string): FixtureKind | null { return this.insts.find((i) => i.id === id)?.kind ?? null; }

  dispose() { this.clear(); this.tex.dispose(); }
}
