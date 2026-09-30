// The digital twin's renderer (docs/platform/digital-twin.md). Plain three.js, imperative: React owns the page,
// this class owns the WebGL scene and updates it in place when data arrives, so a new reading changes a few
// instance colours and a texture instead of rebuilding the building.
//
// Rendering is on demand: nothing is drawn while nothing changes (0 fps when idle). Continuous frames happen only
// while the camera moves, a tween runs, or an SOS pulses. Quality tiers trade looks for frame rate on TV sticks.

import * as THREE from "three";
import { OrbitControls } from "three/examples/jsm/controls/OrbitControls.js";
import { EffectComposer } from "three/examples/jsm/postprocessing/EffectComposer.js";
import { OutputPass } from "three/examples/jsm/postprocessing/OutputPass.js";
import { RenderPass } from "three/examples/jsm/postprocessing/RenderPass.js";
import { UnrealBloomPass } from "three/examples/jsm/postprocessing/UnrealBloomPass.js";
import type { Zone } from "../../floorplan/model";
import { buildBuilding, disposeTree, type Floor3D, type FloorFrame } from "./building";
import { comfortBand, comfortT, heatGrid, heatPixels, staleWeight, type HeatKind, type HeatSensor, type P } from "./heat";
import { assignSlots, zoneSlots } from "./slots";
import type { Layer, PeopleMode, TwinAlert, TwinDevice, TwinState } from "../api";

export type Quality = "high" | "med" | "low";
export type Preset = "overview" | "plan" | "floor" | "alert" | "tour";

const STAGE = 0x0b1418, ACCENT = 0xa7f3d0, AMBER = 0xf6c177, CORAL = 0xff8f70, MUTED = 0x5d7279, BLUE = 0x80b7ff, INK = 0xe6f2ee;
const ZONE_COLORS: Record<string, [number, number, number]> = { mint: [167, 243, 208], blue: [128, 183, 255], amber: [246, 193, 119], coral: [255, 143, 112], violet: [196, 167, 255], slate: [159, 177, 182] };

export type LabelSpec = { id: string; floorId: string; x: number; y: number; h: number; text: string; tone?: "sos" | "warn" | "muted" | "person" | "zone" };
export type EngineStats = { fps: number; calls: number; triangles: number; tier: Quality; textures: number; geometries: number };
export type PickResult = { kind: "device" | "gateway"; id: string } | null;

type FloorInfo = { id: string; draft: Floor3D["draft"]; frame: FloorFrame };
type ZoneRef = { floorId: string; zone: Zone; index: number; centre: P; slots: P[] };

const easeInOut = (t: number) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2);

export class TwinEngine {
  readonly renderer: THREE.WebGLRenderer;
  readonly scene = new THREE.Scene();
  readonly camera = new THREE.PerspectiveCamera(42, 1, 0.1, 2000);
  readonly controls: OrbitControls;
  private host: HTMLElement;
  private labelHost: HTMLDivElement;
  private labelPool: HTMLDivElement[] = [];
  private labels: LabelSpec[] = [];
  private composer: EffectComposer | null = null;
  private bloom: UnrealBloomPass | null = null;
  private tier: Quality;
  private autoTier: boolean;
  private reducedMotion: boolean;

  private building = new THREE.Group();
  private heat = new THREE.Group();
  private markers = new THREE.Group();
  private people = new THREE.Group();
  private fx = new THREE.Group();
  private stage = new THREE.Group();

  private floors: FloorInfo[] = [];
  private zones: ZoneRef[] = [];
  private zoneByGateway = new Map<string, ZoneRef>();
  private activeId = "";
  private heatPlanes = new Map<string, { mesh: THREE.Mesh; texture: THREE.DataTexture; key: string }>();
  private zoneOutlines = new Map<string, { solid: THREE.LineLoop; dashed: THREE.LineLoop }>();

  private deviceMesh: THREE.InstancedMesh | null = null;
  private gatewayMesh: THREE.InstancedMesh | null = null;
  private stems: THREE.LineSegments | null = null;
  private deviceIndex: TwinDevice[] = [];
  private gatewayIndex: TwinDevice[] = [];
  private personMesh: THREE.InstancedMesh | null = null;
  private tokens = new Map<string, { from: THREE.Vector3; to: THREE.Vector3; start: number; sos: boolean }>();
  private nearRings: THREE.Object3D[] = [];
  private sosFx: { pillar: THREE.Mesh; ring: THREE.Mesh; at: THREE.Vector3; phase: number }[] = [];

  private state: TwinState | null = null;
  private layers = new Set<Layer>(["temperature", "people"]);
  private heatRange: Record<HeatKind, [number, number]> = { temperature: [18, 32], humidity: [30, 80], occupancy: [0, 1] };
  private lockedRange: Partial<Record<HeatKind, [number, number]>> = {};

  private dirty = true;
  private raf = 0;
  private tween: { fromPos: THREE.Vector3; toPos: THREE.Vector3; fromTarget: THREE.Vector3; toTarget: THREE.Vector3; start: number; ms: number } | null = null;
  private touring = false;
  private clock = new THREE.Clock();
  private frameTimes: number[] = [];
  private lastFrame = 0;
  private fps = 0;
  private ro: ResizeObserver;
  private onPick: ((p: PickResult) => void) | null = null;
  private downAt: [number, number] | null = null;
  private raycaster = new THREE.Raycaster();
  private disposed = false;

  constructor(host: HTMLElement, opts: { quality?: Quality | "auto"; reducedMotion?: boolean } = {}) {
    this.host = host;
    this.autoTier = !opts.quality || opts.quality === "auto";
    this.tier = opts.quality && opts.quality !== "auto" ? opts.quality : "high";
    this.reducedMotion = !!opts.reducedMotion;
    this.renderer = new THREE.WebGLRenderer({ antialias: this.tier !== "low", alpha: false, powerPreference: "high-performance" });
    this.renderer.setClearColor(STAGE, 1);
    // Count a whole frame (bloom renders several passes), not only its last pass.
    this.renderer.info.autoReset = false;
    this.renderer.outputColorSpace = THREE.SRGBColorSpace;
    host.appendChild(this.renderer.domElement);
    this.renderer.domElement.style.display = "block";
    this.labelHost = document.createElement("div");
    this.labelHost.className = "twin-labels";
    this.labelHost.setAttribute("aria-hidden", "true");
    host.appendChild(this.labelHost);

    this.scene.background = new THREE.Color(STAGE);
    this.scene.add(new THREE.HemisphereLight(0xdff5ee, 0x0b1418, 1.05));
    const sun = new THREE.DirectionalLight(0xffffff, 1.1);
    sun.position.set(30, 60, 20);
    this.scene.add(sun);
    for (const g of [this.stage, this.building, this.heat, this.markers, this.people, this.fx]) this.scene.add(g);
    this.camera.position.set(40, 42, 52);
    this.controls = new OrbitControls(this.camera, this.renderer.domElement);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.09;
    this.controls.maxPolarAngle = Math.PI / 2 - 0.04;
    this.controls.minDistance = 4;
    this.controls.maxDistance = 500;
    this.controls.addEventListener("change", () => this.invalidate());
    this.controls.addEventListener("start", () => { this.touring = false; this.tween = null; });

    this.applyTier();
    this.ro = new ResizeObserver(() => this.resize());
    this.ro.observe(host);
    this.resize();
    this.renderer.domElement.addEventListener("pointerdown", this.onDown);
    this.renderer.domElement.addEventListener("pointerup", this.onUp);
    this.loop();
  }

  // ---- lifecycle ------------------------------------------------------------------------------------------

  dispose() {
    this.disposed = true;
    cancelAnimationFrame(this.raf);
    this.ro.disconnect();
    this.renderer.domElement.removeEventListener("pointerdown", this.onDown);
    this.renderer.domElement.removeEventListener("pointerup", this.onUp);
    this.controls.dispose();
    for (const g of [this.stage, this.building, this.heat, this.markers, this.people, this.fx]) disposeTree(g);
    for (const p of this.heatPlanes.values()) p.texture.dispose();
    this.composer?.dispose();
    this.renderer.dispose();
    this.renderer.forceContextLoss(); // browsers cap live WebGL contexts
    this.renderer.domElement.remove();
    this.labelHost.remove();
  }

  setPickHandler(fn: ((p: PickResult) => void) | null) { this.onPick = fn; }

  invalidate() { this.dirty = true; }

  private resize() {
    const w = this.host.clientWidth || 1, h = this.host.clientHeight || 1;
    this.renderer.setSize(w, h, false);
    this.renderer.domElement.style.width = "100%";
    this.renderer.domElement.style.height = "100%";
    this.composer?.setSize(w, h);
    this.bloom?.setSize(w, h);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
    this.invalidate();
  }

  private applyTier() {
    const dpr = typeof window === "undefined" ? 1 : window.devicePixelRatio || 1;
    this.renderer.setPixelRatio(this.tier === "high" ? Math.min(dpr, 2) : this.tier === "med" ? Math.min(dpr, 1.5) : 1);
    this.scene.fog = this.tier === "low" ? null : new THREE.Fog(STAGE, 140, 460);
    if (this.tier === "high") {
      if (!this.composer) {
        this.composer = new EffectComposer(this.renderer);
        this.composer.addPass(new RenderPass(this.scene, this.camera));
        this.bloom = new UnrealBloomPass(new THREE.Vector2(this.host.clientWidth || 1, this.host.clientHeight || 1), 0.55, 0.5, 0.82);
        this.composer.addPass(this.bloom);
        this.composer.addPass(new OutputPass());
      }
    } else if (this.composer) {
      this.composer.dispose();
      this.composer = null;
      this.bloom = null;
    }
    this.resize();
  }

  get quality(): Quality { return this.tier; }

  setQuality(q: Quality | "auto") {
    this.autoTier = q === "auto";
    this.tier = q === "auto" ? "high" : q;
    this.frameTimes = [];
    this.applyTier();
  }

  setReducedMotion(on: boolean) { this.reducedMotion = on; this.invalidate(); }

  stats(): EngineStats {
    const info = this.renderer.info;
    return { fps: Math.round(this.fps), calls: info.render.calls, triangles: info.render.triangles, tier: this.tier, textures: info.memory.textures, geometries: info.memory.geometries };
  }

  // ---- the loop ---------------------------------------------------------------------------------------------

  private animating(): boolean {
    return !!this.tween || this.touring || (this.sosFx.length > 0 && !this.reducedMotion) || this.gliding();
  }

  private gliding(): boolean {
    const now = performance.now();
    for (const t of this.tokens.values()) if (now - t.start < this.glideMs()) return true;
    return false;
  }

  private glideMs() { return this.reducedMotion ? 0 : 1200; }

  private loop = () => {
    if (this.disposed) return;
    this.raf = requestAnimationFrame(this.loop);
    const damping = this.controls.update();
    const moving = this.animating();
    if (!this.dirty && !moving && !damping) { this.lastFrame = 0; return; }
    const now = performance.now();
    if (this.lastFrame > 0) {
      this.frameTimes.push(now - this.lastFrame);
      if (this.frameTimes.length > 300) this.frameTimes.shift();
      const window5s: number[] = [];
      let sum = 0;
      for (let i = this.frameTimes.length - 1; i >= 0 && sum < 5000; i--) { sum += this.frameTimes[i]; window5s.push(this.frameTimes[i]); }
      this.fps = window5s.length ? (1000 * window5s.length) / sum : 0;
      // Drop a tier when a 5 s moving average stays under 30 fps (only measurable while animating).
      if (this.autoTier && sum >= 5000 && this.fps < 30 && this.tier !== "low") {
        this.tier = this.tier === "high" ? "med" : "low";
        this.frameTimes = [];
        this.applyTier();
      }
    }
    this.lastFrame = moving || damping ? now : 0;
    this.step(now);
    this.renderer.info.reset();
    if (this.composer) this.composer.render();
    else this.renderer.render(this.scene, this.camera);
    this.placeLabels();
    this.dirty = false;
  };

  private step(now: number) {
    if (this.tween) {
      const k = Math.min(1, (now - this.tween.start) / this.tween.ms), e = easeInOut(k);
      this.camera.position.lerpVectors(this.tween.fromPos, this.tween.toPos, e);
      this.controls.target.lerpVectors(this.tween.fromTarget, this.tween.toTarget, e);
      if (k >= 1) this.tween = null;
    } else if (this.touring) {
      const t = this.controls.target, off = this.camera.position.clone().sub(t);
      off.applyAxisAngle(new THREE.Vector3(0, 1, 0), 0.0016);
      this.camera.position.copy(t).add(off);
    }
    const t = this.clock.getElapsedTime();
    for (const s of this.sosFx) {
      if (this.reducedMotion) { s.ring.scale.setScalar(2.2); (s.ring.material as THREE.MeshBasicMaterial).opacity = 0.55; continue; }
      const k = (t * 0.7 + s.phase) % 1;
      s.ring.scale.setScalar(1 + k * 6);
      (s.ring.material as THREE.MeshBasicMaterial).opacity = 0.75 * (1 - k);
      (s.pillar.material as THREE.MeshBasicMaterial).opacity = 0.32 + 0.18 * Math.sin(t * 4 + s.phase * 6);
    }
    if (this.personMesh && this.tokens.size) {
      const m = new THREE.Matrix4(), q = new THREE.Quaternion(), sc = new THREE.Vector3(), p = new THREE.Vector3();
      let i = 0;
      for (const tok of this.tokens.values()) {
        const k = this.glideMs() ? Math.min(1, (now - tok.start) / this.glideMs()) : 1;
        p.lerpVectors(tok.from, tok.to, easeInOut(k));
        sc.setScalar(tok.sos ? 1.35 : 1);
        m.compose(p, q, sc);
        this.personMesh.setMatrixAt(i++, m);
      }
      this.personMesh.instanceMatrix.needsUpdate = true;
    }
  }

  // ---- labels (a small DOM pool, projected each frame) ------------------------------------------------------

  setLabels(list: LabelSpec[]) {
    this.labels = list.slice(0, 40);
    while (this.labelPool.length < this.labels.length) {
      const el = document.createElement("div");
      el.className = "twin-label";
      this.labelHost.appendChild(el);
      this.labelPool.push(el);
    }
    this.invalidate();
  }

  private placeLabels() {
    const w = this.host.clientWidth, h = this.host.clientHeight, v = new THREE.Vector3();
    this.labelPool.forEach((el, i) => {
      const spec = this.labels[i];
      if (!spec) { el.style.display = "none"; return; }
      const at = this.world(spec.floorId, spec.x, spec.y, spec.h);
      if (!at) { el.style.display = "none"; return; }
      v.copy(at).project(this.camera);
      if (v.z > 1 || v.x < -1.1 || v.x > 1.1 || v.y < -1.1 || v.y > 1.1) { el.style.display = "none"; return; }
      el.style.display = "";
      if (el.textContent !== spec.text) el.textContent = spec.text;
      const tone = `twin-label ${spec.tone ? `is-${spec.tone}` : ""}`;
      if (el.className !== tone) el.className = tone;
      el.style.transform = `translate(-50%, -100%) translate(${((v.x + 1) / 2) * w}px, ${((1 - v.y) / 2) * h}px)`;
    });
  }

  // ---- geometry -----------------------------------------------------------------------------------------------

  /** World position of a plan point (metres) on a floor, `h` metres above its slab. */
  world(floorId: string, x: number, y: number, h = 0): THREE.Vector3 | null {
    const f = this.floors.find((fl) => fl.id === floorId);
    if (!f || !f.frame.visible) return null;
    return new THREE.Vector3(f.frame.originX + x, f.frame.y + h, f.frame.originZ + y);
  }

  /** Rebuilds the building (floors, walls, items), the stage and the zone outlines. Data layers follow. */
  setBuilding(floors: Floor3D[], activeId: string, explode: number, isolate: boolean) {
    this.activeId = activeId;
    const { frames } = buildBuilding(this.building, { floors, activeId, assets: [], selection: null, explode, isolate, devices: false, people: false, labels: false, liveZones: false, glass: true, zones: false });
    this.floors = floors.map((f) => ({ id: f.id, draft: f.draft, frame: frames.find((fr) => fr.id === f.id) ?? { id: f.id, y: 0, originX: 0, originZ: 0, visible: false, active: false } }));
    // Floors above the active one fade almost away, so the camera can look down into the active floor.
    const activeY = frames.find((fr) => fr.active)?.y ?? 0;
    for (const g of this.building.children) {
      if (g.position.y <= activeY + 0.01) continue;
      g.traverse((o) => {
        const mat = (o as THREE.Mesh).material as THREE.Material | THREE.Material[] | undefined;
        for (const m of Array.isArray(mat) ? mat : mat ? [mat] : []) { m.transparent = true; m.opacity *= 0.3; m.depthWrite = false; }
      });
    }
    this.zones = [];
    this.zoneByGateway.clear();
    for (const f of this.floors) {
      f.draft.layout.zones.forEach((z, index) => {
        if (z.points.length < 3) return;
        const pts = z.points as P[];
        const cx = pts.reduce((s, p) => s + p[0], 0) / pts.length, cy = pts.reduce((s, p) => s + p[1], 0) / pts.length;
        const ref: ZoneRef = { floorId: f.id, zone: z, index, centre: [cx, cy], slots: zoneSlots(pts) };
        this.zones.push(ref);
        for (const g of z.gateway_ids ?? []) if (!this.zoneByGateway.has(g)) this.zoneByGateway.set(g, ref);
      });
    }
    this.buildStage();
    this.buildOutlines();
    for (const p of this.heatPlanes.values()) { p.texture.dispose(); }
    disposeTree(this.heat);
    this.heat.clear();
    this.heatPlanes.clear();
    if (this.state) this.setState(this.state, [...this.layers], true);
    this.invalidate();
  }

  private buildStage() {
    disposeTree(this.stage);
    this.stage.clear();
    const active = this.floors.find((f) => f.id === this.activeId) ?? this.floors[0];
    if (!active) return;
    const size = Math.max(active.draft.width_m, active.draft.depth_m) * 2.6;
    const grid = new THREE.GridHelper(size, Math.round(size / 2), 0x1d3038, 0x14222a);
    grid.position.y = -0.2;
    (grid.material as THREE.Material).transparent = true;
    (grid.material as THREE.Material).opacity = 0.55;
    this.stage.add(grid);
    // A soft contact shadow under each visible slab.
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 128;
    const ctx = canvas.getContext("2d")!;
    const grad = ctx.createRadialGradient(64, 64, 8, 64, 64, 64);
    grad.addColorStop(0, "rgba(0,0,0,0.55)");
    grad.addColorStop(1, "rgba(0,0,0,0)");
    ctx.fillStyle = grad;
    ctx.fillRect(0, 0, 128, 128);
    const tex = new THREE.CanvasTexture(canvas);
    for (const f of this.floors) {
      if (!f.frame.visible) continue;
      const shadow = new THREE.Mesh(new THREE.PlaneGeometry(f.draft.width_m * 1.5, f.draft.depth_m * 1.5), new THREE.MeshBasicMaterial({ map: tex, transparent: true, depthWrite: false }));
      shadow.rotation.x = -Math.PI / 2;
      shadow.position.set(f.frame.originX + f.draft.width_m / 2, f.frame.y - 0.16, f.frame.originZ + f.draft.depth_m / 2);
      this.stage.add(shadow);
    }
  }

  private buildOutlines() {
    for (const o of this.zoneOutlines.values()) { this.fx.remove(o.solid, o.dashed); o.solid.geometry.dispose(); (o.solid.material as THREE.Material).dispose(); (o.dashed.material as THREE.Material).dispose(); }
    this.zoneOutlines.clear();
    for (const z of this.zones) {
      const f = this.floors.find((fl) => fl.id === z.floorId);
      if (!f || !f.frame.visible) continue;
      const pts = (z.zone.points as P[]).map(([x, y]) => new THREE.Vector3(f.frame.originX + x, f.frame.y + 0.12, f.frame.originZ + y));
      const geo = new THREE.BufferGeometry().setFromPoints(pts);
      const solid = new THREE.LineLoop(geo, new THREE.LineBasicMaterial({ color: ACCENT, transparent: true, opacity: 0.95 }));
      const dashed = new THREE.LineLoop(geo, new THREE.LineDashedMaterial({ color: ACCENT, dashSize: 0.5, gapSize: 0.35, transparent: true, opacity: 0.6 }));
      dashed.computeLineDistances();
      solid.visible = dashed.visible = false;
      this.fx.add(solid, dashed);
      this.zoneOutlines.set(`${z.floorId}:${z.index}`, { solid, dashed });
    }
  }

  // ---- data -----------------------------------------------------------------------------------------------------

  lockRange(kind: HeatKind, range: [number, number] | null) {
    if (range) this.lockedRange[kind] = range;
    else delete this.lockedRange[kind];
    if (this.state) this.setState(this.state, [...this.layers], true);
  }

  range(kind: HeatKind): [number, number] { return this.lockedRange[kind] ?? this.heatRange[kind]; }

  /** Applies a new live state (or new layers). `force` recomputes the heat textures even if values did not change. */
  setState(state: TwinState, layers: Layer[], force = false) {
    this.state = state;
    this.layers = new Set(layers);
    const now = Date.parse(state.server_time) || Date.now();
    this.updateMarkers(state);
    this.updateHeat(state, now, force);
    this.updatePeople(state);
    this.updateAlertFx(state.alerts);
    this.invalidate();
  }

  private heatKind(): HeatKind | null {
    if (this.layers.has("temperature")) return "temperature";
    if (this.layers.has("humidity")) return "humidity";
    if (this.layers.has("occupancy")) return "occupancy";
    return null;
  }

  private updateHeat(state: TwinState, now: number, force: boolean) {
    const kind = this.heatKind();
    // Humidity's range follows the data unless locked (the legend shows it); temperature is a distance from each zone's
    // comfort band, always -1 .. +1.
    if (kind === "humidity") {
      const vals = state.devices.map((d) => d.h).filter((v): v is number => typeof v === "number" && Number.isFinite(v));
      if (vals.length) {
        const lo = Math.floor(Math.min(...vals)), hi = Math.ceil(Math.max(...vals));
        this.heatRange[kind] = [lo, Math.max(hi, lo + 2)];
      }
    }
    const [min, max] = kind === "temperature" ? [-1, 1] : this.range(kind ?? "humidity");
    for (const f of this.floors) {
      if (!f.frame.visible) continue;
      const devices = state.devices.filter((d) => d.floor_id === f.id && d.kind === "device");
      const sensorsFor = (zone: Zone): HeatSensor[] => {
        const out: HeatSensor[] = [];
        for (const d of devices) {
          if (!inside([d.x, d.y], zone.points as P[])) continue;
          const age = d.last_at ? now - Date.parse(d.last_at) : Infinity;
          let value: number | undefined;
          if (kind === "temperature") value = d.t === undefined ? undefined : comfortT(d.t, comfortBand(zone.kind, zone.comfort));
          else if (kind === "humidity") value = d.h;
          else if (kind === "occupancy" && d.motion_at !== undefined) value = now - Date.parse(d.motion_at) < 5 * 60000 ? 1 : 0;
          if (typeof value === "number" && Number.isFinite(value)) out.push({ x: d.x, y: d.y, value, weight: staleWeight(age) });
        }
        return out;
      };
      const zones = f.draft.layout.zones.filter((z) => z.points.length >= 3);
      const perZone = zones.map(sensorsFor);
      const key = JSON.stringify([kind, min, max, perZone.map((s) => s.map((x) => [x.x, x.y, Math.round(x.value * 10), Math.round(x.weight * 10)]))]);
      let plane = this.heatPlanes.get(f.id);
      if (plane && plane.key === key && !force) continue;
      const grid = heatGrid(f.draft.width_m, f.draft.depth_m, zones.map((z) => ({ points: z.points as P[] })), (i) => perZone[i]);
      const pixels = heatPixels(grid, kind, min, max, (i) => ZONE_COLORS[zones[i]?.color ?? "mint"] ?? ZONE_COLORS.mint);
      if (!plane || plane.texture.image.width !== grid.width || plane.texture.image.height !== grid.height) {
        if (plane) { this.heat.remove(plane.mesh); plane.mesh.geometry.dispose(); (plane.mesh.material as THREE.Material).dispose(); plane.texture.dispose(); }
        const texture = new THREE.DataTexture(pixels, grid.width, grid.height, THREE.RGBAFormat);
        texture.colorSpace = THREE.SRGBColorSpace;
        texture.magFilter = THREE.LinearFilter;
        texture.minFilter = THREE.LinearFilter;
        texture.needsUpdate = true;
        const above = f.frame.y > (this.floors.find((x) => x.frame.active)?.frame.y ?? 0) + 0.01;
        const mesh = new THREE.Mesh(new THREE.PlaneGeometry(f.draft.width_m, f.draft.depth_m), new THREE.MeshBasicMaterial({ map: texture, transparent: true, depthWrite: false, side: THREE.DoubleSide, opacity: f.frame.active ? 1 : above ? 0.14 : 0.45 }));
        // +90° about x: plan y grows towards +z, and texture row 0 (v = 0) is the plan's top edge (y = 0).
        mesh.rotation.x = Math.PI / 2;
        mesh.position.set(f.frame.originX + f.draft.width_m / 2, f.frame.y + 0.08, f.frame.originZ + f.draft.depth_m / 2);
        mesh.renderOrder = 1;
        this.heat.add(mesh);
        plane = { mesh, texture, key };
        this.heatPlanes.set(f.id, plane);
      } else {
        (plane.texture.image.data as Uint8Array).set(pixels);
        plane.texture.needsUpdate = true;
        plane.key = key;
      }
    }
  }

  private updateMarkers(state: TwinState) {
    const visible = new Set(this.floors.filter((f) => f.frame.visible).map((f) => f.id));
    const devices = state.devices.filter((d) => d.kind === "device" && visible.has(d.floor_id));
    const gateways = state.devices.filter((d) => d.kind === "gateway" && visible.has(d.floor_id));
    const low = this.tier === "low";
    if (!this.deviceMesh || this.deviceMesh.count !== devices.length || this.deviceIndex.length !== devices.length) {
      if (this.deviceMesh) { this.markers.remove(this.deviceMesh); this.deviceMesh.geometry.dispose(); (this.deviceMesh.material as THREE.Material).dispose(); this.deviceMesh.dispose(); }
      this.deviceMesh = new THREE.InstancedMesh(new THREE.SphereGeometry(0.22, low ? 8 : 16, low ? 6 : 12), new THREE.MeshStandardMaterial({ color: 0xffffff, emissive: 0x3a5a52, emissiveIntensity: 0.6, roughness: 0.35 }), Math.max(1, devices.length));
      this.deviceMesh.userData.pick = "device";
      this.markers.add(this.deviceMesh);
    }
    if (!this.gatewayMesh || this.gatewayIndex.length !== gateways.length) {
      if (this.gatewayMesh) { this.markers.remove(this.gatewayMesh); this.gatewayMesh.geometry.dispose(); (this.gatewayMesh.material as THREE.Material).dispose(); this.gatewayMesh.dispose(); }
      this.gatewayMesh = new THREE.InstancedMesh(new THREE.BoxGeometry(0.62, 0.2, 0.62), new THREE.MeshStandardMaterial({ color: 0xffffff, emissive: 0x2c4c46, emissiveIntensity: 0.5, roughness: 0.4 }), Math.max(1, gateways.length));
      this.gatewayMesh.userData.pick = "gateway";
      this.markers.add(this.gatewayMesh);
    }
    this.deviceIndex = devices;
    this.gatewayIndex = gateways;
    const m = new THREE.Matrix4(), c = new THREE.Color();
    const tone = (d: TwinDevice) => (d.sos || d.alert ? CORAL : !d.online ? (d.last_at ? AMBER : MUTED) : d.door === 1 ? AMBER : ACCENT);
    const stems: number[] = [];
    this.deviceMesh.count = devices.length;
    devices.forEach((d, i) => {
      const at = this.world(d.floor_id, d.x, d.y, d.z)!;
      m.makeTranslation(at.x, at.y, at.z);
      this.deviceMesh!.setMatrixAt(i, m);
      this.deviceMesh!.setColorAt(i, c.setHex(tone(d)));
      stems.push(at.x, at.y, at.z, at.x, at.y - d.z + 0.1, at.z);
    });
    this.gatewayMesh.count = gateways.length;
    gateways.forEach((g, i) => {
      const at = this.world(g.floor_id, g.x, g.y, g.z)!;
      m.makeTranslation(at.x, at.y, at.z);
      this.gatewayMesh!.setMatrixAt(i, m);
      this.gatewayMesh!.setColorAt(i, c.setHex(g.online ? BLUE : AMBER));
      stems.push(at.x, at.y, at.z, at.x, at.y - g.z + 0.1, at.z);
    });
    for (const im of [this.deviceMesh, this.gatewayMesh]) {
      im.instanceMatrix.needsUpdate = true;
      if (im.instanceColor) im.instanceColor.needsUpdate = true;
      im.computeBoundingSphere();
    }
    if (this.stems) { this.markers.remove(this.stems); this.stems.geometry.dispose(); (this.stems.material as THREE.Material).dispose(); }
    const geo = new THREE.BufferGeometry();
    geo.setAttribute("position", new THREE.Float32BufferAttribute(stems, 3));
    this.stems = new THREE.LineSegments(geo, new THREE.LineBasicMaterial({ color: 0x5d7279, transparent: true, opacity: 0.5 }));
    this.markers.add(this.stems);
  }

  /** People: tokens in stable slots of their zone; a tag near a gateway with no zone gets a dashed ring there. */
  private updatePeople(state: TwinState) {
    for (const o of this.zoneOutlines.values()) { o.solid.visible = false; o.dashed.visible = false; }
    for (const r of this.nearRings) { this.people.remove(r); disposeTree(r); }
    this.nearRings = [];
    const show = this.layers.has("people");
    const entries: { id: string; gateway: string; sos: boolean; candidate?: string }[] = [];
    if (show) {
      const mode: PeopleMode = state.presence.mode;
      if (mode === "counts") {
        for (const c of state.presence.counts) for (let i = 0; i < c.n; i++) entries.push({ id: `${c.gateway_id}#${String(i).padStart(4, "0")}`, gateway: c.gateway_id, sos: false });
        // Counts carry no identity: an SOS is shown by the zone halo and the pillar, not a token.
      } else {
        for (const p of state.presence.people ?? []) if (p.gateway_id) entries.push({ id: p.pid, gateway: p.gateway_id, sos: p.sos, candidate: p.candidate_gateway_id });
      }
    }
    // Group per zone, then give each person a slot.
    const byZone = new Map<ZoneRef, typeof entries>();
    const unzoned = new Map<string, number>();
    for (const e of entries) {
      const z = this.zoneByGateway.get(e.gateway);
      if (!z) { unzoned.set(e.gateway, (unzoned.get(e.gateway) ?? 0) + 1); continue; }
      byZone.set(z, [...(byZone.get(z) ?? []), e]);
      if (e.candidate) {
        const cz = this.zoneByGateway.get(e.candidate);
        const o = cz ? this.zoneOutlines.get(`${cz.floorId}:${cz.index}`) : undefined;
        if (o && cz !== z) o.dashed.visible = true;
      }
    }
    const now = performance.now();
    const next = new Map<string, { from: THREE.Vector3; to: THREE.Vector3; start: number; sos: boolean }>();
    for (const [z, list] of byZone) {
      const outline = this.zoneOutlines.get(`${z.floorId}:${z.index}`);
      if (outline) {
        outline.solid.visible = true;
        (outline.solid.material as THREE.LineBasicMaterial).color.setHex(list.some((e) => e.sos) ? CORAL : ACCENT);
      }
      const slots = assignSlots(list.map((e) => e.id), z.slots);
      for (const e of list) {
        const [x, y] = slots.get(e.id)!;
        const to = this.world(z.floorId, x, y, 0.62);
        if (!to) continue;
        const prev = this.tokens.get(e.id);
        // A zone change glides for 1.2 s ("moved to"), never a walked path; same slot means no motion.
        const from = prev ? (prev.to.distanceTo(to) > 0.01 ? prev.to.clone() : to) : to;
        next.set(e.id, { from, to, start: prev && prev.to.distanceTo(to) > 0.01 ? now : prev?.start ?? 0, sos: e.sos });
      }
    }
    for (const [gw, n] of unzoned) {
      const g = state.devices.find((d) => d.kind === "gateway" && d.id === gw);
      const at = g ? this.world(g.floor_id, g.x, g.y, 0.1) : null;
      if (!at) continue;
      const ring = new THREE.LineLoop(new THREE.BufferGeometry().setFromPoints(Array.from({ length: 48 }, (_, i) => new THREE.Vector3(Math.cos((i / 48) * Math.PI * 2) * (1.6 + n * 0.1), 0, Math.sin((i / 48) * Math.PI * 2) * (1.6 + n * 0.1)))), new THREE.LineDashedMaterial({ color: BLUE, dashSize: 0.3, gapSize: 0.25 }));
      ring.computeLineDistances();
      ring.position.copy(at);
      this.people.add(ring);
      this.nearRings.push(ring);
    }
    this.tokens = next;
    const low = this.tier === "low";
    if (!this.personMesh || this.personMesh.instanceMatrix.count < Math.max(1, next.size)) {
      if (this.personMesh) { this.people.remove(this.personMesh); this.personMesh.geometry.dispose(); (this.personMesh.material as THREE.Material).dispose(); this.personMesh.dispose(); }
      this.personMesh = new THREE.InstancedMesh(new THREE.CapsuleGeometry(0.19, 0.7, low ? 3 : 5, low ? 8 : 12), new THREE.MeshStandardMaterial({ color: 0xffffff, emissive: 0x2a4a66, emissiveIntensity: 0.7, roughness: 0.5 }), Math.max(64, next.size));
      this.people.add(this.personMesh);
    }
    this.personMesh.count = next.size;
    const c = new THREE.Color();
    let i = 0;
    for (const tok of next.values()) this.personMesh.setColorAt(i++, c.setHex(tok.sos ? CORAL : BLUE));
    if (this.personMesh.instanceColor) this.personMesh.instanceColor.needsUpdate = true;
    this.step(now);
    this.personMesh.computeBoundingSphere();
  }

  /** Where an alert happens: its zone (via the gateway), else the gateway, else the device. */
  alertPlace(a: TwinAlert): { floorId: string; x: number; y: number; zone?: ZoneRef } | null {
    const z = this.zoneByGateway.get(a.gateway_id);
    if (z) return { floorId: z.floorId, x: z.centre[0], y: z.centre[1], zone: z };
    const d = this.state?.devices.find((x) => x.id === a.gateway_id || (a.device_id && x.id === a.device_id));
    return d ? { floorId: d.floor_id, x: d.x, y: d.y } : null;
  }

  zoneName(gatewayId: string): { name: string; floorId: string } | null {
    const z = this.zoneByGateway.get(gatewayId);
    return z ? { name: z.zone.name, floorId: z.floorId } : null;
  }

  private updateAlertFx(alerts: TwinAlert[]) {
    for (const s of this.sosFx) { this.fx.remove(s.pillar, s.ring); disposeTree(s.pillar); disposeTree(s.ring); }
    this.sosFx = [];
    const urgent = alerts.filter((a) => (a.sos || a.hazard) && a.status === "open");
    const seen = new Set<string>();
    urgent.forEach((a, i) => {
      const place = this.alertPlace(a);
      if (!place) return;
      const key = `${place.floorId}:${place.x}:${place.y}`;
      if (seen.has(key)) return;
      seen.add(key);
      const at = this.world(place.floorId, place.x, place.y, 0);
      if (!at) return;
      const pillar = new THREE.Mesh(new THREE.CylinderGeometry(0.55, 0.9, 9, 24, 1, true), new THREE.MeshBasicMaterial({ color: CORAL, transparent: true, opacity: 0.4, blending: THREE.AdditiveBlending, depthWrite: false, side: THREE.DoubleSide }));
      pillar.position.copy(at).add(new THREE.Vector3(0, 4.5, 0));
      const ring = new THREE.Mesh(new THREE.RingGeometry(0.8, 1.05, 48), new THREE.MeshBasicMaterial({ color: CORAL, transparent: true, opacity: 0.7, side: THREE.DoubleSide, depthWrite: false, blending: THREE.AdditiveBlending }));
      ring.rotation.x = -Math.PI / 2;
      ring.position.copy(at).add(new THREE.Vector3(0, 0.15, 0));
      this.fx.add(pillar, ring);
      this.sosFx.push({ pillar, ring, at, phase: i * 0.31 });
      if (place.zone) {
        const o = this.zoneOutlines.get(`${place.zone.floorId}:${place.zone.index}`);
        if (o) { o.solid.visible = true; (o.solid.material as THREE.LineBasicMaterial).color.setHex(CORAL); }
      }
    });
  }

  // ---- camera ------------------------------------------------------------------------------------------------

  private frameFor(floorId?: string) {
    const f = this.floors.find((fl) => fl.id === (floorId ?? this.activeId)) ?? this.floors[0];
    const height = this.floors.reduce((s, fl) => Math.max(s, fl.frame.y + fl.draft.ceiling_m), 0);
    return { f, height };
  }

  /** Moves the camera to a preset (a cut when motion is reduced). */
  preset(p: Preset, floorId?: string) {
    this.touring = p === "tour" && !this.reducedMotion;
    const { f, height } = this.frameFor(floorId);
    if (!f) return;
    const cx = f.frame.originX + f.draft.width_m / 2, cz = f.frame.originZ + f.draft.depth_m / 2;
    const span = Math.hypot(f.draft.width_m, f.draft.depth_m);
    const fit = (span / 2 + height / 2) / Math.sin((this.camera.fov * Math.PI) / 360) / Math.min(1, this.camera.aspect);
    let target = new THREE.Vector3(cx, height / 3, cz), pos: THREE.Vector3;
    switch (p) {
      case "plan":
        target = new THREE.Vector3(cx, f.frame.y, cz);
        pos = target.clone().add(new THREE.Vector3(0.001, span * 1.15 / Math.min(1, this.camera.aspect), 0.001));
        break;
      case "floor":
        target = new THREE.Vector3(cx, f.frame.y, cz);
        pos = target.clone().add(new THREE.Vector3(0.45, 0.72, 0.62).normalize().multiplyScalar(span * 0.95 / Math.min(1, this.camera.aspect)));
        break;
      case "alert": {
        const a = this.state?.alerts.find((x) => (x.sos || x.hazard) && x.status === "open");
        if (a) { this.focusAlert(a); return; }
        pos = target.clone().add(new THREE.Vector3(0.5, 0.62, 0.72).normalize().multiplyScalar(fit * 0.86));
        break;
      }
      default:
        pos = target.clone().add(new THREE.Vector3(0.5, 0.62, 0.72).normalize().multiplyScalar(fit * 0.86));
    }
    this.flyTo(pos, target, p === "tour" ? 1800 : 1500);
  }

  /** Flies to an alert's zone (1.5 s, ease in-out; a cut with reduced motion). */
  focusAlert(a: TwinAlert) {
    const place = this.alertPlace(a);
    if (!place) return;
    const at = this.world(place.floorId, place.x, place.y, 0);
    if (!at) return;
    this.touring = false;
    const pos = at.clone().add(new THREE.Vector3(8, 12, 10));
    this.flyTo(pos, at, 1500);
  }

  flyTo(pos: THREE.Vector3, target: THREE.Vector3, ms: number) {
    if (this.reducedMotion || ms <= 0) {
      this.tween = null;
      this.camera.position.copy(pos);
      this.controls.target.copy(target);
    } else {
      this.tween = { fromPos: this.camera.position.clone(), toPos: pos, fromTarget: this.controls.target.clone(), toTarget: target, start: performance.now(), ms };
    }
    this.invalidate();
  }

  cameraTarget(): THREE.Vector3 { return this.controls.target.clone(); }

  /** Camera in plan terms for the minimap: the floor under the target, the target point and the view heading. */
  planView(): { floorId: string; x: number; y: number; heading: number } | null {
    const t = this.controls.target;
    const f = [...this.floors].filter((fl) => fl.frame.visible).sort((a, b) => Math.abs(a.frame.y - t.y) - Math.abs(b.frame.y - t.y))[0];
    if (!f) return null;
    const dir = t.clone().sub(this.camera.position);
    return { floorId: f.id, x: t.x - f.frame.originX, y: t.z - f.frame.originZ, heading: Math.atan2(dir.z, dir.x) };
  }

  /** Flies the target to a plan point of a floor, keeping the view direction. */
  lookAt(floorId: string, x: number, y: number) {
    const to = this.world(floorId, x, y, 0);
    if (!to) return;
    const off = this.camera.position.clone().sub(this.controls.target);
    this.flyTo(to.clone().add(off), to, 900);
  }

  // ---- picking --------------------------------------------------------------------------------------------------

  private onDown = (e: PointerEvent) => { this.downAt = [e.clientX, e.clientY]; };

  private onUp = (e: PointerEvent) => {
    if (!this.downAt || Math.hypot(e.clientX - this.downAt[0], e.clientY - this.downAt[1]) > 4) return;
    const r = this.renderer.domElement.getBoundingClientRect();
    const pointer = new THREE.Vector2(((e.clientX - r.left) / r.width) * 2 - 1, -((e.clientY - r.top) / r.height) * 2 + 1);
    this.raycaster.setFromCamera(pointer, this.camera);
    const hits = this.raycaster.intersectObjects([this.deviceMesh, this.gatewayMesh].filter((x): x is THREE.InstancedMesh => !!x), false);
    const hit = hits[0];
    if (!hit || hit.instanceId === undefined) { this.onPick?.(null); return; }
    const kind = hit.object.userData.pick as "device" | "gateway";
    const d = (kind === "device" ? this.deviceIndex : this.gatewayIndex)[hit.instanceId];
    this.onPick?.(d ? { kind, id: d.id } : null);
  };
}

function inside(p: P, poly: P[]): boolean {
  let hit = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i++) {
    const [xi, yi] = poly[i], [xj, yj] = poly[j];
    if (yi > p[1] !== yj > p[1] && p[0] < ((xj - xi) * (p[1] - yi)) / (yj - yi) + xi) hit = !hit;
  }
  return hit;
}

export { INK };
