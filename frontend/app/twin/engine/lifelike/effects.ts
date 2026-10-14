// Effects that show what the data says (docs/platform/digital-twin.md, "Lifelike hospital"): smoke or gas rising
// from the zone of an open hazard alert under a flashing ceiling alarm, a puddle where a leak alert is open, doors
// that swing open with their door sensor (amber when left open), shimmer over a zone far above its comfort band,
// mist in a cold store, soft light in rooms with recent motion, warm windows at night. Nothing is invented: every
// effect is a reading or an open alert. With motion reduced they are drawn still.

import * as THREE from "three";
import type { Draft } from "../../../floorplan/model";
import type { TwinAlert, TwinDevice } from "../../api";
import type { TwinTheme } from "../../theme";
import { comfortBand, staleWeight } from "../heat";
import { bbox, hash, inside, rng, type P } from "./geo";
import { zoneType } from "./interior";

type World = (floorId: string, x: number, y: number, h: number) => THREE.Vector3 | null;
export type EffectFloor = { id: string; draft: Draft; active: boolean };

/** Hazard kind from the detector's name or the alert title (named titles say ควัน / แก๊ส / CO). */
export function hazardKind(a: TwinAlert, device?: TwinDevice): "smoke" | "gas" {
  const s = `${a.title} ${device?.name ?? ""}`.toLowerCase();
  return /แก๊ส|gas|\bco\b|คาร์บอน|carbon/.test(s) && !/ควัน|smoke/.test(s) ? "gas" : "smoke";
}

/** 19:00–06:00 in a time zone (an unknown zone falls back to the browser's clock). */
function isNight(timeZone: string): boolean {
  let hour: number;
  try { hour = Number(new Intl.DateTimeFormat("en-GB", { hour: "numeric", hourCycle: "h23", timeZone }).format(new Date())); } catch { hour = new Date().getHours(); }
  return hour >= 19 || hour < 6;
}

function soft(): THREE.CanvasTexture {
  const c = document.createElement("canvas");
  c.width = c.height = 64;
  const g = c.getContext("2d")!;
  const grad = g.createRadialGradient(32, 32, 0, 32, 32, 32);
  grad.addColorStop(0, "rgba(255,255,255,0.9)");
  grad.addColorStop(0.5, "rgba(255,255,255,0.35)");
  grad.addColorStop(1, "rgba(255,255,255,0)");
  g.fillStyle = grad;
  g.fillRect(0, 0, 64, 64);
  return new THREE.CanvasTexture(c);
}

/**
 * A puff of smoke: a few overlapping soft blobs inside a feathered round mask, so the sprite has no edge at any
 * rotation and reads as cloud rather than a disc.
 */
function puff(): THREE.CanvasTexture {
  const size = 128, c = document.createElement("canvas");
  c.width = c.height = size;
  const g = c.getContext("2d")!;
  const r = rng(hash("smoke-puff"));
  for (let i = 0; i < 14; i++) {
    const x = size / 2 + (r() - 0.5) * size * 0.34, y = size / 2 + (r() - 0.5) * size * 0.34, rad = size * (0.16 + r() * 0.16);
    const grad = g.createRadialGradient(x, y, 0, x, y, rad);
    grad.addColorStop(0, "rgba(255,255,255,0.32)");
    grad.addColorStop(1, "rgba(255,255,255,0)");
    g.fillStyle = grad;
    g.fillRect(0, 0, size, size);
  }
  // Feather everything to zero well inside the square.
  g.globalCompositeOperation = "destination-in";
  const mask = g.createRadialGradient(size / 2, size / 2, size * 0.12, size / 2, size / 2, size * 0.48);
  mask.addColorStop(0, "rgba(0,0,0,1)");
  mask.addColorStop(1, "rgba(0,0,0,0)");
  g.fillStyle = mask;
  g.fillRect(0, 0, size, size);
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  return t;
}

// Smoke particles: a size in metres (perspective-correct), an alpha, a rotation and a colour each.
const SMOKE_VERT = /* glsl */ `
attribute float aSize; attribute float aAlpha; attribute float aRot; attribute vec3 aColor;
uniform float uScale;
varying float vAlpha; varying float vRot; varying vec3 vColor;
void main() {
  vec4 mv = modelViewMatrix * vec4(position, 1.0);
  gl_Position = projectionMatrix * mv;
  gl_PointSize = clamp(aSize * uScale / max(0.1, -mv.z), 1.0, 900.0);
  vAlpha = aAlpha; vRot = aRot; vColor = aColor;
}`;
const SMOKE_FRAG = /* glsl */ `
uniform sampler2D uMap;
varying float vAlpha; varying float vRot; varying vec3 vColor;
void main() {
  vec2 p = gl_PointCoord - 0.5;
  float c = cos(vRot), s = sin(vRot);
  vec2 uv = vec2(c * p.x - s * p.y, s * p.x + c * p.y) + 0.5;
  float a = texture2D(uMap, uv).a * vAlpha;
  if (a < 0.003) discard;
  gl_FragColor = vec4(vColor, a);
  #include <colorspace_fragment>
}`;

const RIPPLE_FRAG = /* glsl */ `
uniform vec3 uColor; uniform float uTime; uniform float uMotion; varying vec2 vUv;
void main() {
  vec2 p = vUv - 0.5; float d = length(p) * 2.0;
  if (d > 1.0) discard;
  float body = smoothstep(1.0, 0.75, d) * 0.45;
  float rings = uMotion > 0.5 ? smoothstep(0.06, 0.0, abs(fract(d * 3.0 - uTime * 0.6) - 0.5) - 0.42) * (1.0 - d) * 0.6 : 0.0;
  gl_FragColor = vec4(uColor, body + rings);
  #include <colorspace_fragment>
}`;
const SHIMMER_FRAG = /* glsl */ `
uniform vec3 uColor; uniform float uTime; uniform float uMotion; varying vec2 vUv;
void main() {
  vec2 p = vUv * vec2(9.0, 6.0);
  float w = sin(p.x * 1.7 + uTime * 2.1 * uMotion) * sin(p.y * 2.3 - uTime * 1.6 * uMotion) * 0.5 + 0.5;
  float edge = smoothstep(0.0, 0.18, vUv.x) * smoothstep(1.0, 0.82, vUv.x) * smoothstep(0.0, 0.18, vUv.y) * smoothstep(1.0, 0.82, vUv.y);
  gl_FragColor = vec4(uColor, (0.05 + 0.1 * w) * edge);
  #include <colorspace_fragment>
}`;
const VERT = /* glsl */ `varying vec2 vUv; void main() { vUv = uv; gl_Position = projectionMatrix * modelViewMatrix * vec4(position, 1.0); }`;

type Door = { pos: THREE.Vector3; yaw: number; width: number; sensor: string | null; angle: number; openSince: number; tone: "closed" | "open" | "amber" };
type Plume = { points: THREE.Points; origin: THREE.Vector3; top: number; seeds: Float32Array; kind: "smoke" | "gas"; alarm: THREE.Mesh; drift: number };

export class Effects {
  readonly group = new THREE.Group();
  private tex = soft();
  private puffTex = puff();
  /** Pixels per metre at 1 m (viewport height / 2·tan(fov/2)), for perspective-sized smoke. */
  private pixelScale = 600;
  private smokeUniforms: { uScale: { value: number } }[] = [];
  private floors: EffectFloor[] = [];
  private world: World | null = null;
  private low = false;
  private doors: Door[] = [];
  private doorMesh: THREE.InstancedMesh | null = null;
  private windowMesh: THREE.InstancedMesh | null = null;
  private windowCount = 0;
  private plumes = new Map<string, Plume>();
  private puddles = new Map<string, THREE.Mesh>();
  private shimmers = new Map<string, THREE.Mesh>();
  private glows = new Map<string, THREE.Mesh>();
  private mist: THREE.Points | null = null;
  private mistSeeds: { base: THREE.Vector3; r: number; seed: number }[] = [];
  private devices = new Map<string, TwinDevice>();
  private uniforms: { uTime: { value: number }; uMotion: { value: number } }[] = [];
  /** Night is judged in the site's time zone (the deployment default when the site has none). */
  private timeZone = "Asia/Bangkok";
  private night: boolean | null = null;
  private nightCheckedAt = -Infinity;
  private openingsVisible = true;
  private mistStill = false;
  private tmp = { m: new THREE.Matrix4(), q: new THREE.Quaternion(), s: new THREE.Vector3(), c: new THREE.Color(), up: new THREE.Vector3(0, 1, 0) };
  private doorColors = { closed: new THREE.Color(0xd7dfde), open: new THREE.Color(), amber: new THREE.Color() };
  private flash = new THREE.Color();
  /** Gas and CO keep a yellow-green tint from the tokens (warning towards accent). */
  private gas: THREE.Color;

  constructor(private theme: TwinTheme) {
    this.group.name = "effects";
    this.gas = new THREE.Color(theme.warning).lerp(new THREE.Color(theme.accent), 0.45);
  }

  /** Doors, windows, room glows and cold-store mist for the visible floors (rebuilt with the building). */
  build(floors: EffectFloor[], world: World, low: boolean, doorSensors: { id: string; floorId: string; x: number; y: number }[]) {
    this.clear();
    this.floors = floors;
    this.world = world;
    this.low = low;
    const leaves: Door[] = [], panes: { pos: THREE.Vector3; yaw: number; w: number }[] = [];
    for (const f of floors) {
      for (const it of f.draft.layout.items) {
        if (it.type !== "door" && it.type !== "window") continue;
        const wide = it.w >= it.h, width = Math.max(it.w, it.h);
        const yaw = (-it.rot * Math.PI) / 180 + (wide ? 0 : Math.PI / 2);
        const cx = it.x + it.w / 2, cy = it.y + it.h / 2;
        if (it.type === "window") {
          const pos = world(f.id, cx, cy, 1.45);
          if (pos) panes.push({ pos, yaw, w: width });
          continue;
        }
        // The hinge sits at one end of the opening; the leaf swings into the room by up to 80°.
        const dx = Math.cos(yaw) * (width / 2), dz = -Math.sin(yaw) * (width / 2);
        const pos = world(f.id, cx - dx, cy - dz, 0);
        if (!pos) continue;
        const near = doorSensors.filter((s) => s.floorId === f.id).map((s) => ({ s, d: Math.hypot(s.x - cx, s.y - cy) })).filter((e) => e.d < 2.5).sort((a, b) => a.d - b.d)[0];
        leaves.push({ pos, yaw, width, sensor: near?.s.id ?? null, angle: 0, openSince: 0, tone: "closed" });
      }
    }
    this.doors = leaves;
    const std = () => new THREE.MeshStandardMaterial({ color: 0xffffff, roughness: 0.6, metalness: 0.05 });
    if (leaves.length) {
      const leaf = new THREE.BoxGeometry(1, 2.05, 0.045);
      leaf.translate(0.5, 1.025, 0);
      this.doorMesh = new THREE.InstancedMesh(leaf, std(), leaves.length);
      this.doorMesh.name = "doors";
      this.doorMesh.visible = this.openingsVisible;
      this.group.add(this.doorMesh);
      this.doorColors.open.setHex(this.theme.accent).lerp(new THREE.Color(0xffffff), 0.35);
      this.doorColors.amber.setHex(this.theme.warning);
      // Every leaf written once, closed; afterwards only a leaf that swings or changes colour is touched.
      leaves.forEach((d, i) => { this.writeDoor(i, d); this.doorMesh!.setColorAt(i, this.doorColors.closed); });
      this.doorMesh.instanceMatrix.needsUpdate = true;
      if (this.doorMesh.instanceColor) this.doorMesh.instanceColor.needsUpdate = true;
      this.doorMesh.computeBoundingSphere();
    }
    if (panes.length) {
      this.windowMesh = new THREE.InstancedMesh(new THREE.BoxGeometry(1, 1.1, 0.03), new THREE.MeshBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.55, depthWrite: false, toneMapped: false }), panes.length);
      const m = new THREE.Matrix4(), q = new THREE.Quaternion(), s = new THREE.Vector3();
      panes.forEach((p, i) => this.windowMesh!.setMatrixAt(i, m.compose(p.pos, q.setFromAxisAngle(new THREE.Vector3(0, 1, 0), p.yaw), s.set(p.w, 1, 1))));
      this.windowMesh.name = "windows";
      this.windowMesh.visible = this.openingsVisible;
      this.windowCount = panes.length;
      this.windowMesh.computeBoundingSphere();
      this.night = null;
      this.group.add(this.windowMesh);
    }
    // Room glows (motion) and cold mist, active floor only.
    const mist: { base: THREE.Vector3; r: number; seed: number }[] = [];
    for (const f of floors.filter((x) => x.active)) {
      f.draft.layout.zones.forEach((z, index) => {
        const poly = z.points as P[];
        if (poly.length < 3) return;
        const key = `${f.id}:${index}`;
        const shape = new THREE.Shape(poly.map((p) => new THREE.Vector2(p[0], p[1])));
        const geo = new THREE.ShapeGeometry(shape);
        geo.rotateX(Math.PI / 2);
        const origin = world(f.id, 0, 0, 0.1);
        if (!origin) return;
        const glow = new THREE.Mesh(geo, new THREE.MeshBasicMaterial({ color: 0xfff1d6, transparent: true, opacity: 0.08, depthWrite: false, blending: THREE.AdditiveBlending, toneMapped: false, side: THREE.DoubleSide }));
        glow.position.copy(origin);
        glow.visible = false;
        glow.renderOrder = 2;
        this.glows.set(key, glow);
        this.group.add(glow);
        if (zoneType(z) === "cold") {
          const r = rng(hash(`mist:${z.id}`));
          const n = low ? 14 : 36;
          for (let i = 0; i < n; i++) {
            const b = bbox(poly);
            const p: P = [b.x0 + r() * (b.x1 - b.x0), b.y0 + r() * (b.y1 - b.y0)];
            if (!inside(p, poly)) continue;
            const base = world(f.id, p[0], p[1], 0.15);
            if (base) mist.push({ base, r: 0.4 + r() * 0.8, seed: r() });
          }
        }
      });
    }
    if (mist.length) {
      const geo = new THREE.BufferGeometry();
      geo.setAttribute("position", new THREE.BufferAttribute(new Float32Array(mist.length * 3), 3));
      this.mist = new THREE.Points(geo, new THREE.PointsMaterial({ map: this.tex, size: 1.6, color: new THREE.Color(this.theme.blue).lerp(new THREE.Color(0xffffff), 0.7), transparent: true, opacity: 0.22, depthWrite: false, toneMapped: false }));
      this.mist.frustumCulled = false;
      this.mistSeeds = mist;
      this.mistStill = false;
      this.group.add(this.mist);
    }
  }

  /** New state: plumes and alarms for open hazards, puddles for open leaks, shimmer and glows from readings. */
  setState(devices: TwinDevice[], alerts: TwinAlert[], now: number, placeOf: (a: TwinAlert) => { floorId: string; x: number; y: number } | null) {
    this.devices.clear();
    for (const d of devices) this.devices.set(d.id, d);
    const world = this.world;
    if (!world) return;
    // Smoke / gas.
    const hazards = alerts.filter((a) => a.hazard && a.status === "open");
    const keep = new Set<string>();
    for (const a of hazards) {
      const dev = a.device_id ? this.devices.get(a.device_id) : undefined;
      const place = dev ? { floorId: dev.floor_id, x: dev.x, y: dev.y } : placeOf(a);
      if (!place) continue;
      const floor = this.floors.find((f) => f.id === place.floorId);
      const origin = world(place.floorId, place.x, place.y, 0.2);
      if (!floor || !origin) continue;
      keep.add(a.id);
      if (!this.plumes.has(a.id)) this.plumes.set(a.id, this.makePlume(origin, floor.draft.ceiling_m, hazardKind(a, dev), a.id));
    }
    for (const [id, p] of this.plumes) if (!keep.has(id)) { this.dropPlume(p); this.plumes.delete(id); }
    // Leaks: an open leak alert puts a puddle under its sensor.
    const leaks = new Set<string>();
    for (const a of alerts) {
      if (a.event !== "leak" || a.status !== "open" || !a.device_id) continue;
      const dev = this.devices.get(a.device_id);
      const pos = dev ? world(dev.floor_id, dev.x, dev.y, 0.03) : null;
      if (!pos) continue;
      leaks.add(a.id);
      if (!this.puddles.has(a.id)) this.puddles.set(a.id, this.makePuddle(pos));
    }
    for (const [id, m] of this.puddles) if (!leaks.has(id)) { this.dropMesh(m); this.puddles.delete(id); }
    // Zones: shimmer when far above the comfort band, a soft glow when motion was seen in the last 5 minutes.
    const hot = new Set<string>();
    for (const f of this.floors.filter((x) => x.active)) {
      f.draft.layout.zones.forEach((z, index) => {
        const poly = z.points as P[];
        if (poly.length < 3) return;
        const key = `${f.id}:${index}`;
        const inZone = devices.filter((d) => d.floor_id === f.id && inside([d.x, d.y], poly));
        // Fresh readings only, weighted like the heat map (full weight to 5 min, none past 15 min).
        let sum = 0, weight = 0;
        for (const d of inZone) {
          if (typeof d.t !== "number" || !d.last_at) continue;
          const wgt = staleWeight(now - Date.parse(d.last_at));
          sum += d.t * wgt; weight += wgt;
        }
        const mean = weight > 0 ? sum / weight : null;
        if (mean !== null && mean >= comfortBand(z.kind, z.comfort)[1] + 3) {
          hot.add(key);
          if (!this.shimmers.has(key)) this.shimmers.set(key, this.makeShimmer(f.id, poly));
        }
        const glow = this.glows.get(key);
        if (glow) glow.visible = inZone.some((d) => d.motion_at && now - Date.parse(d.motion_at) < 5 * 60000);
      });
    }
    for (const [key, m] of this.shimmers) if (!hot.has(key)) { this.dropMesh(m); this.shimmers.delete(key); }
  }

  private makePlume(origin: THREE.Vector3, ceiling: number, kind: "smoke" | "gas", id: string): Plume {
    const n = this.low ? 60 : 150;
    const geo = new THREE.BufferGeometry();
    geo.setAttribute("position", new THREE.BufferAttribute(new Float32Array(n * 3), 3));
    geo.setAttribute("aSize", new THREE.BufferAttribute(new Float32Array(n), 1));
    geo.setAttribute("aAlpha", new THREE.BufferAttribute(new Float32Array(n), 1));
    geo.setAttribute("aRot", new THREE.BufferAttribute(new Float32Array(n), 1));
    geo.setAttribute("aColor", new THREE.BufferAttribute(new Float32Array(n * 3), 3));
    const r = rng(hash(`plume:${id}`));
    const seeds = new Float32Array(n * 4);
    for (let i = 0; i < seeds.length; i++) seeds[i] = r();
    const uniforms = { uMap: { value: this.puffTex }, uScale: { value: this.pixelScale } };
    this.smokeUniforms.push(uniforms);
    const points = new THREE.Points(geo, new THREE.ShaderMaterial({ uniforms, vertexShader: SMOKE_VERT, fragmentShader: SMOKE_FRAG, transparent: true, depthWrite: false, toneMapped: false }));
    points.frustumCulled = false;
    points.renderOrder = 5;
    const alarm = new THREE.Mesh(new THREE.CylinderGeometry(0.11, 0.13, 0.07, 16), new THREE.MeshBasicMaterial({ color: this.theme.danger, toneMapped: false }));
    alarm.position.copy(origin).setY(origin.y - 0.2 + ceiling - 0.06);
    this.group.add(points, alarm);
    return { points, origin, top: ceiling - 0.3, seeds, kind, alarm, drift: r() * Math.PI * 2 };
  }

  private makePuddle(pos: THREE.Vector3): THREE.Mesh {
    const u = { uColor: { value: new THREE.Color(this.theme.blue).lerp(new THREE.Color(0x9fd3ff), 0.3) }, uTime: { value: 0 }, uMotion: { value: 1 } };
    this.uniforms.push(u);
    const geo = new THREE.PlaneGeometry(1.8, 1.8);
    geo.rotateX(-Math.PI / 2);
    const m = new THREE.Mesh(geo, new THREE.ShaderMaterial({ uniforms: u, vertexShader: VERT, fragmentShader: RIPPLE_FRAG, transparent: true, depthWrite: false, toneMapped: false }));
    m.position.copy(pos);
    m.renderOrder = 3;
    this.group.add(m);
    return m;
  }

  /** A shimmer over exactly the zone's shape (not its bounding box), 1.1 m above the floor. */
  private makeShimmer(floorId: string, poly: P[]): THREE.Mesh {
    const b = bbox(poly);
    const u = { uColor: { value: new THREE.Color(this.theme.danger).lerp(new THREE.Color(0xffd0a0), 0.3) }, uTime: { value: 0 }, uMotion: { value: 1 } };
    this.uniforms.push(u);
    const geo = new THREE.ShapeGeometry(new THREE.Shape(poly.map((p) => new THREE.Vector2(p[0], p[1]))));
    // The shader's edge fade wants 0..1 across the zone.
    const uv = geo.getAttribute("uv") as THREE.BufferAttribute, pos = geo.getAttribute("position") as THREE.BufferAttribute;
    for (let i = 0; i < uv.count; i++) uv.setXY(i, (pos.getX(i) - b.x0) / Math.max(0.01, b.x1 - b.x0), (pos.getY(i) - b.y0) / Math.max(0.01, b.y1 - b.y0));
    geo.rotateX(Math.PI / 2);
    const m = new THREE.Mesh(geo, new THREE.ShaderMaterial({ uniforms: u, vertexShader: VERT, fragmentShader: SHIMMER_FRAG, transparent: true, depthWrite: false, blending: THREE.AdditiveBlending, toneMapped: false, side: THREE.DoubleSide }));
    const at = this.world?.(floorId, 0, 0, 1.1);
    if (at) m.position.copy(at);
    m.renderOrder = 4;
    this.group.add(m);
    return m;
  }

  /** A frame: plumes rise and spread, alarms flash, puddles ripple, doors swing, mist drifts. */
  step(t: number, dt: number, motion: boolean, now: number) {
    for (const u of this.uniforms) { u.uTime.value = t; u.uMotion.value = motion ? 1 : 0; }
    const gas = this.gas;
    for (const u of this.smokeUniforms) u.uScale.value = this.pixelScale;
    for (const p of this.plumes.values()) {
      const g = p.points.geometry;
      const pos = g.getAttribute("position") as THREE.BufferAttribute, size = g.getAttribute("aSize") as THREE.BufferAttribute;
      const alpha = g.getAttribute("aAlpha") as THREE.BufferAttribute, rot = g.getAttribute("aRot") as THREE.BufferAttribute, col = g.getAttribute("aColor") as THREE.BufferAttribute;
      const gasTint = p.kind === "gas";
      // A slow wind for this plume: the column leans and wanders a little.
      const windX = Math.cos(p.drift + (motion ? t * 0.05 : 0)) * 0.55, windZ = Math.sin(p.drift + (motion ? t * 0.05 : 0)) * 0.55;
      for (let i = 0; i < pos.count; i++) {
        const s0 = p.seeds[i * 4], s1 = p.seeds[i * 4 + 1], s2 = p.seeds[i * 4 + 2], s3 = p.seeds[i * 4 + 3];
        // Life 0 → 1. Smoke rises fast, slows under the ceiling and spreads; gas stays low and creeps outwards.
        const life = motion ? (t * (gasTint ? 0.07 : 0.1) + s0) % 1 : 0.15 + s0 * 0.75;
        const rise = 1 - Math.pow(1 - Math.min(1, life * 1.6), 2.2);
        const h = gasTint ? 0.15 + life * 1.1 : 0.1 + rise * (p.top - 0.1);
        const widen = 0.12 + Math.pow(life, 1.25) * (gasTint ? 3.6 : 3.1) + Math.max(0, life - 0.55) * 2.2;
        const ang = s1 * Math.PI * 2 + (motion ? Math.sin(t * 0.3 + s2 * 6) * 0.25 : 0);
        const rr = widen * (0.25 + 0.75 * Math.sqrt(s2));
        pos.setXYZ(i, p.origin.x + Math.cos(ang) * rr + windX * life, p.origin.y + h, p.origin.z + Math.sin(ang) * rr + windZ * life);
        // Puffs grow as they rise; fade in quickly, fade out slowly; denser near the source.
        size.setX(i, (0.55 + life * 2.6) * (0.8 + s3 * 0.5));
        const fadeIn = Math.min(1, life / 0.08), fadeOut = 1 - Math.max(0, (life - 0.55) / 0.45);
        alpha.setX(i, (gasTint ? 0.32 : 0.5) * fadeIn * fadeOut * fadeOut * (1 - life * 0.45));
        rot.setX(i, s3 * 6.283 + (motion ? t * (s2 - 0.5) * 0.5 : 0));
        // Dark at the source, lighter grey (or paler tint) as it spreads.
        const shade = 0.14 + life * 0.3 + s3 * 0.05;
        if (gasTint) col.setXYZ(i, gas.r * (0.55 + life * 0.4), gas.g * (0.55 + life * 0.4), gas.b * (0.55 + life * 0.4));
        else col.setXYZ(i, shade, shade * 1.03, shade * 1.06);
      }
      pos.needsUpdate = true; size.needsUpdate = true; alpha.needsUpdate = true; rot.needsUpdate = true; col.needsUpdate = true;
      const flash = motion ? (Math.sin(t * 9) > 0 ? 1.4 : 0.35) : 1;
      (p.alarm.material as THREE.MeshBasicMaterial).color.copy(this.flash.setHex(this.theme.danger).multiplyScalar(flash));
    }
    if (this.doorMesh) {
      let moved = false, recoloured = false;
      for (let i = 0; i < this.doors.length; i++) {
        const d = this.doors[i];
        const isOpen = (d.sensor ? this.devices.get(d.sensor)?.door : undefined) === 1;
        if (isOpen && !d.openSince) d.openSince = now;
        if (!isOpen) d.openSince = 0;
        const target = isOpen ? 1.4 : 0;
        if (Math.abs(d.angle - target) > 0.001) {
          d.angle = motion ? d.angle + (target - d.angle) * Math.min(1, dt * 4) : target;
          if (Math.abs(d.angle - target) <= 0.001) d.angle = target;
          this.writeDoor(i, d);
          moved = true;
        }
        // Left open for more than a minute: amber, like the door's beacon.
        const tone: Door["tone"] = !isOpen ? "closed" : now - d.openSince > 60000 ? "amber" : "open";
        if (tone !== d.tone) { d.tone = tone; this.doorMesh.setColorAt(i, this.doorColors[tone]); recoloured = true; }
      }
      if (moved) { this.doorMesh.instanceMatrix.needsUpdate = true; this.doorMesh.computeBoundingSphere(); }
      if (recoloured && this.doorMesh.instanceColor) this.doorMesh.instanceColor.needsUpdate = true;
    }
    if (this.windowMesh && performance.now() - this.nightCheckedAt > 60000) {
      // Night (19:00–06:00 in the site's time zone): windows glow warm from inside; by day they are cool glass.
      this.nightCheckedAt = performance.now();
      const night = isNight(this.timeZone);
      if (night !== this.night) {
        this.night = night;
        const c = night ? this.tmp.c.setHex(0xffd9a0).multiplyScalar(0.9) : this.tmp.c.setHex(this.theme.blue).lerp(new THREE.Color(0xffffff), 0.5).multiplyScalar(0.6);
        for (let i = 0; i < this.windowCount; i++) this.windowMesh.setColorAt(i, c);
        if (this.windowMesh.instanceColor) this.windowMesh.instanceColor.needsUpdate = true;
      }
    }
    // Mist drifts with motion; without it, it is written once and left.
    if (this.mist && (motion || !this.mistStill)) {
      const pos = this.mist.geometry.getAttribute("position") as THREE.BufferAttribute;
      for (let i = 0; i < this.mistSeeds.length; i++) {
        const m = this.mistSeeds[i], a = (motion ? t * 0.2 : 0) + m.seed * 6.28;
        pos.setXYZ(i, m.base.x + Math.cos(a) * m.r * 0.4, m.base.y + 0.15 + Math.sin(a * 0.7) * 0.1, m.base.z + Math.sin(a) * m.r * 0.4);
      }
      pos.needsUpdate = true;
      this.mistStill = !motion;
    }
  }

  private writeDoor(i: number, d: Door) {
    const { m, q, s, up } = this.tmp;
    this.doorMesh!.setMatrixAt(i, m.compose(d.pos, q.setFromAxisAngle(up, d.yaw + d.angle), s.set(d.width * 0.95, 1, 1)));
  }

  /** The site's IANA time zone for the night look (null keeps the deployment default). */
  setTimeZone(tz: string | null) { this.timeZone = tz || "Asia/Bangkok"; this.nightCheckedAt = -Infinity; }

  /** Doors and windows: hidden under a customer model in "replace" mode (it has its own). */
  setOpeningsVisible(on: boolean) {
    this.openingsVisible = on;
    if (this.doorMesh) this.doorMesh.visible = on;
    if (this.windowMesh) this.windowMesh.visible = on;
  }

  /** Something here only looks right moving (plumes, puddles, mist, a door mid-swing). */
  wantsFrames(): boolean {
    return this.plumes.size > 0 || this.puddles.size > 0 || this.shimmers.size > 0 || !!this.mist || this.doors.some((d) => Math.abs(d.angle - (this.devices.get(d.sensor ?? "")?.door === 1 ? 1.4 : 0)) > 0.01);
  }

  private dropPlume(p: Plume) { this.dropMesh(p.points); this.dropMesh(p.alarm); }

  private dropMesh(o: THREE.Object3D) {
    this.group.remove(o);
    const mesh = o as THREE.Mesh;
    mesh.geometry?.dispose();
    const mat = mesh.material as THREE.Material;
    const sm = mat as THREE.ShaderMaterial;
    if (sm?.uniforms) this.uniforms = this.uniforms.filter((u) => u !== (sm.uniforms as unknown));
    if ((mat as THREE.PointsMaterial)?.map === this.tex) (mat as THREE.PointsMaterial).map = null;
    if (sm?.uniforms?.uScale) this.smokeUniforms = this.smokeUniforms.filter((u) => u !== (sm.uniforms as unknown));
    mat?.dispose();
  }

  private clear() {
    for (const p of this.plumes.values()) this.dropPlume(p);
    for (const m of [...this.puddles.values(), ...this.shimmers.values(), ...this.glows.values()]) this.dropMesh(m);
    if (this.mist) this.dropMesh(this.mist);
    for (const m of [this.doorMesh, this.windowMesh]) if (m) { this.dropMesh(m); m.dispose(); }
    this.plumes.clear(); this.puddles.clear(); this.shimmers.clear(); this.glows.clear();
    this.mist = null; this.mistSeeds = []; this.doorMesh = null; this.windowMesh = null; this.doors = []; this.uniforms = [];
  }

  /** Pixels per metre at 1 m of the current camera and viewport (smoke sprites are sized in metres). */
  setPixelScale(v: number) { this.pixelScale = v; }

  dispose() { this.clear(); this.tex.dispose(); this.puffTex.dispose(); }
}
