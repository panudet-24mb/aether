// The digital twin's renderer (docs/platform/digital-twin.md). Plain three.js, imperative: React owns the page,
// this class owns the WebGL scene and updates it in place when data arrives, so a new reading changes a few
// instance colours and a texture instead of rebuilding the building.
//
// Rendering is on demand: nothing is drawn while nothing changes (0 fps when idle). Continuous frames happen only
// while the camera moves, a tween runs, or an SOS pulses. Quality tiers trade looks for frame rate on TV sticks.
//
// Two styles share this engine: classic (the original look) and immersive (immersive.ts: image-based lighting, a
// holographic floor, devices as status beacons, in-scene badges with buttons, optional ambient motion that runs at
// 30 fps at most and stops when the tab is hidden or motion is reduced).

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
import { beaconColor, makeEnvironment, makeGround, styleBuilding, towerGeometry, type BeaconTone, type Ground } from "./immersive";
import { ModelLayer, type Flattened, type PlanPlace } from "./model";
import { Lifelike, MAX_FIGURES, type FixtureState, type WalkerSpec } from "./lifelike";
import { readTheme, type TwinTheme } from "../theme";
import type { ModelTransform } from "../model-api";
import type { Layer, PeopleMode, TwinAlert, TwinDevice, TwinState } from "../api";

export type Quality = "high" | "med" | "low";
export type Preset = "overview" | "plan" | "floor" | "alert" | "tour";
export type TwinStyle = "classic" | "immersive";

const STAGE = 0x0b1418, ACCENT = 0xa7f3d0, AMBER = 0xf6c177, CORAL = 0xff8f70, MUTED = 0x5d7279, BLUE = 0x80b7ff, INK = 0xe6f2ee, LAMP = 0xffe39a, LAMP_OFF = 0x34444a;
const ZONE_COLORS: Record<string, [number, number, number]> = { mint: [167, 243, 208], blue: [128, 183, 255], amber: [246, 193, 119], coral: [255, 143, 112], violet: [196, 167, 255], slate: [159, 177, 182] };

export type LabelSpec = { id: string; floorId: string; x: number; y: number; h: number; text: string; tone?: "sos" | "warn" | "muted" | "person" | "zone" };
export type EngineStats = { fps: number; calls: number; triangles: number; tier: Quality; textures: number; geometries: number };
/** A click: a device's beacon or gateway selects it; a fixture (immersive) may switch it (`toggle`). */
export type PickResult = { kind: "device" | "gateway" | "fixture"; id: string; toggle?: boolean } | null;

/** An in-scene status badge (immersive): a projected DOM node with a select button and, for outputs, a power switch. */
export type BadgeSpec = {
  id: string;
  floorId: string;
  x: number;
  y: number;
  h: number;
  name: string;
  /** The key value: °C, %, W, เปิด/ปิด, ประตูเปิด. */
  value: string;
  /** Spoken status (ออนไลน์, แจ้งเตือน, …), part of the button's label. */
  status: string;
  tone: BeaconTone;
  /** Higher wins a crowded spot; alerts and the selected device are always shown. */
  priority: number;
  always?: boolean;
  selected?: boolean;
  toggle?: { on: boolean | null; state: "idle" | "pending" | "sent" | "confirmed" | "failed"; disabled: boolean };
  message?: string;
};
export type BadgeAction = "select" | "toggle";
type BadgeEl = { el: HTMLDivElement; main: HTMLButtonElement; name: HTMLSpanElement; value: HTMLSpanElement; power: HTMLButtonElement | null; msg: HTMLSpanElement; sig: string; w: number; h: number };
const MAX_BADGES = 24;
const BLOOM_IMMERSIVE = 0.7;
const AMBIENT_IDLE_MS = 120000;
const ACTIVITY = ["pointerdown", "pointermove", "keydown", "wheel"] as const;

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
  /** Screen rectangles of the labels drawn this frame (badges keep clear of them). */
  private labelRects: [number, number, number, number][] = [];
  private composer: EffectComposer | null = null;
  private bloom: UnrealBloomPass | null = null;
  private tier: Quality;
  private autoTier: boolean;
  private reducedMotion: boolean;
  private style: TwinStyle;
  private theme: TwinTheme | null = null;
  private envMap: THREE.WebGLRenderTarget | null = null;
  private hemi: THREE.HemisphereLight;
  private sun: THREE.DirectionalLight;
  private ground: Ground | null = null;
  private beaconBase: THREE.InstancedMesh | null = null;
  private alertMesh: THREE.InstancedMesh | null = null;
  private alerting: { at: THREE.Vector3; phase: number }[] = [];
  private phases = new Map<string, number>();
  private markerKey = "";
  private ambient = false;
  /** Last pointer or keyboard input, or state change: ambient motion stops 2 minutes after it. */
  private lastActivity = performance.now();
  private stateSig = "";
  private paused = false;
  private ambientAt = 0;
  private badgeHost: HTMLDivElement | null = null;
  private badges: BadgeSpec[] = [];
  private badgeEls = new Map<string, BadgeEl>();
  private badgeRange = 60;
  private onBadge: ((id: string, action: BadgeAction) => void) | null = null;
  private modelLayer = new ModelLayer();
  /** The furnished interior, fixtures, walking figures and effects (immersive only). */
  private lifelike: Lifelike | null = null;
  private lastStep = 0;
  private tierRebuild: ReturnType<typeof setTimeout> | undefined;
  private paintTmp = { m: new THREE.Matrix4(), q: new THREE.Quaternion(), sc: new THREE.Vector3(), c: new THREE.Color(), base: new THREE.Color() };
  private timeZone: string | null = null;
  private modelTransform: ModelTransform | null = null;
  private lastBuild: { floors: Floor3D[]; activeId: string; explode: number; isolate: boolean } | null = null;

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
  private startedAt = performance.now();
  private frameTimes: number[] = [];
  private lastFrame = 0;
  private fps = 0;
  private ro: ResizeObserver;
  private onPick: ((p: PickResult) => void) | null = null;
  private downAt: [number, number] | null = null;
  private raycaster = new THREE.Raycaster();
  private disposed = false;

  constructor(host: HTMLElement, opts: { quality?: Quality | "auto"; reducedMotion?: boolean; style?: TwinStyle } = {}) {
    this.host = host;
    this.autoTier = !opts.quality || opts.quality === "auto";
    this.tier = opts.quality && opts.quality !== "auto" ? opts.quality : "high";
    this.reducedMotion = !!opts.reducedMotion;
    this.style = opts.style ?? "classic";
    this.renderer = new THREE.WebGLRenderer({ antialias: this.tier !== "low", alpha: false, powerPreference: "high-performance" });
    this.renderer.setClearColor(STAGE, 1);
    // Count a whole frame (bloom renders several passes), not only its last pass.
    this.renderer.info.autoReset = false;
    this.renderer.outputColorSpace = THREE.SRGBColorSpace;
    // The building model can be cut above the active floor (a clipping plane on its materials only).
    this.renderer.localClippingEnabled = true;
    host.appendChild(this.renderer.domElement);
    this.renderer.domElement.style.display = "block";
    this.labelHost = document.createElement("div");
    this.labelHost.className = "twin-labels";
    this.labelHost.setAttribute("aria-hidden", "true");
    host.appendChild(this.labelHost);

    this.scene.background = new THREE.Color(STAGE);
    this.hemi = new THREE.HemisphereLight(0xdff5ee, 0x0b1418, 1.05);
    this.scene.add(this.hemi);
    const sun = new THREE.DirectionalLight(0xffffff, 1.1);
    sun.position.set(30, 60, 20);
    this.sun = sun;
    this.scene.add(sun);
    for (const g of [this.stage, this.modelLayer.group, this.building, this.heat, this.markers, this.people, this.fx]) this.scene.add(g);
    this.camera.position.set(40, 42, 52);
    this.controls = new OrbitControls(this.camera, this.renderer.domElement);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.09;
    this.controls.maxPolarAngle = Math.PI / 2 - 0.04;
    this.controls.minDistance = 4;
    this.controls.maxDistance = 500;
    this.controls.addEventListener("change", () => this.invalidate());
    this.controls.addEventListener("start", () => { this.touring = false; this.tween = null; });

    this.applyLook();
    this.applyTier();
    this.ro = new ResizeObserver(() => this.resize());
    this.ro.observe(host);
    this.resize();
    this.renderer.domElement.addEventListener("pointerdown", this.onDown);
    this.renderer.domElement.addEventListener("pointerup", this.onUp);
    document.addEventListener("visibilitychange", this.onVisibility);
    for (const ev of ACTIVITY) window.addEventListener(ev, this.onActivity, { passive: true });
    this.loop();
  }

  // ---- lifecycle ------------------------------------------------------------------------------------------

  dispose() {
    this.disposed = true;
    cancelAnimationFrame(this.raf);
    this.ro.disconnect();
    this.renderer.domElement.removeEventListener("pointerdown", this.onDown);
    this.renderer.domElement.removeEventListener("pointerup", this.onUp);
    document.removeEventListener("visibilitychange", this.onVisibility);
    for (const ev of ACTIVITY) window.removeEventListener(ev, this.onActivity);
    this.controls.dispose();
    this.ground?.dispose();
    for (const g of [this.stage, this.building, this.heat, this.markers, this.people, this.fx]) disposeTree(g);
    for (const p of this.heatPlanes.values()) p.texture.dispose();
    this.modelLayer.dispose();
    this.lifelike?.dispose();
    clearTimeout(this.tierRebuild);
    this.envMap?.dispose();
    this.badgeHost?.removeEventListener("click", this.onBadgeClick);
    this.badgeHost?.remove();
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
    const immersive = this.style === "immersive" && this.theme;
    this.scene.fog = this.tier === "low" ? null : immersive ? new THREE.FogExp2(this.theme!.paper, 0.0045) : new THREE.Fog(STAGE, 140, 460);
    if (this.tier === "high") {
      if (!this.composer) {
        this.composer = new EffectComposer(this.renderer);
        this.composer.addPass(new RenderPass(this.scene, this.camera));
        this.bloom = new UnrealBloomPass(new THREE.Vector2(this.host.clientWidth || 1, this.host.clientHeight || 1), 0.55, 0.5, 0.82);
        this.composer.addPass(this.bloom);
        this.composer.addPass(new OutputPass());
      }
      // Bloom only on the high tier; the immersive style lets beacons and edges glow a little more.
      // Immersive: only what is meant to glow (beacons, screens, lit fixtures, edges) blooms, not white furniture.
      this.bloom!.strength = immersive ? BLOOM_IMMERSIVE : 0.55;
      this.bloom!.radius = immersive ? 0.55 : 0.5;
      this.bloom!.threshold = immersive ? 0.92 : 0.82;
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

  /** Switches between the classic and the immersive look without recreating the WebGL context. */
  setStyle(style: TwinStyle) {
    if (style === this.style) return;
    this.style = style;
    this.applyLook();
    this.applyTier();
    if (style === "classic") this.setBadges([]);
    if (this.lastBuild) {
      const b = this.lastBuild;
      this.setBuilding(b.floors, b.activeId, b.explode, b.isolate);
    }
    this.invalidate();
  }

  /** Ambient motion (immersive only): a slow grid sweep and breathing beacons. Off when motion is reduced. */
  setAmbient(on: boolean) { this.ambient = on; this.invalidate(); }

  /** A paused view (a TV playlist between turns) never runs ambient frames. */
  setPaused(on: boolean) { this.paused = on; this.invalidate(); }

  private ambientActive(): boolean {
    return this.style === "immersive" && this.ambient && !this.reducedMotion && !this.paused
      && !(typeof document !== "undefined" && document.hidden) && performance.now() - this.lastActivity < AMBIENT_IDLE_MS;
  }

  /** Input or new data: ambient motion (if on) runs again, for another 2 minutes. */
  private onActivity = () => {
    const was = this.ambientActive();
    this.lastActivity = performance.now();
    if (!was && this.ambientActive()) this.invalidate();
  };


  private onVisibility = () => { this.lastStep = 0; this.invalidate(); };

  /** Walking, swinging and particles may move: the immersive style with ambient motion on and motion not reduced. */
  private motionAllowed(): boolean { return this.style === "immersive" && this.ambient && !this.reducedMotion; }

  /** Lights, background and environment for the current style; classic is exactly the original setup. */
  private applyLook() {
    const immersive = this.style === "immersive";
    if (immersive) {
      this.theme = readTheme();
      if (!this.envMap) this.envMap = makeEnvironment(this.renderer);
    }
    const bg = immersive ? this.theme!.paper : STAGE;
    this.renderer.setClearColor(bg, 1);
    (this.scene.background as THREE.Color).setHex(bg);
    this.scene.environment = immersive && this.envMap ? this.envMap.texture : null;
    this.scene.environmentIntensity = immersive ? 0.55 : 1;
    // Immersive lets the operator walk up to a fixture (a wall switch is 9 cm wide); classic keeps its 4 m.
    this.controls.minDistance = immersive ? 1 : 4;
    this.hemi.intensity = immersive ? 0.5 : 1.05;
    this.sun.intensity = immersive ? 0.85 : 1.1;
    if (immersive && !this.lifelike && this.theme) {
      this.lifelike = new Lifelike(this.theme);
      this.lifelike.setTimeZone(this.timeZone);
      this.scene.add(this.lifelike.group);
    } else if (!immersive && this.lifelike) {
      this.scene.remove(this.lifelike.group);
      this.lifelike.dispose();
      this.lifelike = null;
    }
  }

  stats(): EngineStats {
    const info = this.renderer.info;
    return { fps: Math.round(this.fps), calls: info.render.calls, triangles: info.render.triangles, tier: this.tier, textures: info.memory.textures, geometries: info.memory.geometries };
  }

  // ---- the loop ---------------------------------------------------------------------------------------------

  private animating(): boolean {
    return !!this.tween || this.touring || (this.sosFx.length > 0 && !this.reducedMotion) || this.gliding() || (this.lifelike?.moving() ?? false);
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
    const now = performance.now();
    if (!this.dirty && !moving && !damping) {
      this.lastFrame = 0;
      // Ambient motion alone is never counted towards the tier's frame rate. It draws at 30 fps at most, except walking
      // figures on the high tier (60 fps: a 30 fps walk looks like stop motion).
      const cap = this.lifelike?.wantsFrames() && this.tier === "high" ? 15 : 33;
      if (!this.ambientActive() || now - this.ambientAt < cap) return;
    }
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
        // The lifelike interior and fixtures are rebuilt for the lower tier (fewer segments and particles), once.
        if (this.lifelike && this.lastBuild) {
          clearTimeout(this.tierRebuild);
          this.tierRebuild = setTimeout(() => { if (this.lastBuild && !this.disposed) { const b = this.lastBuild; this.setBuilding(b.floors, b.activeId, b.explode, b.isolate); } }, 800);
        }
      }
    }
    this.lastFrame = moving || damping ? now : 0;
    this.ambientAt = now;
    this.step(now);
    this.renderer.info.reset();
    if (this.bloom && this.style === "immersive") {
      // Close up, a lit panel fills the screen and bloom would turn it into a white blob: fade bloom in with distance.
      const dist = this.camera.position.distanceTo(this.controls.target);
      this.bloom.strength = BLOOM_IMMERSIVE * Math.min(1, Math.max(0.15, (dist - 2) / 22));
    }
    if (this.composer) this.composer.render();
    else this.renderer.render(this.scene, this.camera);
    this.placeLabels();
    this.placeBadges();
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
    const t = (performance.now() - this.startedAt) / 1000;
    for (const s of this.sosFx) {
      if (this.reducedMotion) { s.ring.scale.setScalar(2.2); (s.ring.material as THREE.MeshBasicMaterial).opacity = 0.55; continue; }
      const k = (t * 0.7 + s.phase) % 1;
      s.ring.scale.setScalar(1 + k * 6);
      (s.ring.material as THREE.MeshBasicMaterial).opacity = 0.75 * (1 - k);
      (s.pillar.material as THREE.MeshBasicMaterial).opacity = 0.32 + 0.18 * Math.sin(t * 4 + s.phase * 6);
    }
    const ambient = this.ambientActive();
    if (this.lifelike) {
      const dt = this.lastStep ? (now - this.lastStep) / 1000 : 0;
      this.lastStep = now;
      const px = this.renderer.domElement.height / (2 * Math.tan((this.camera.fov * Math.PI) / 360));
      this.lifelike.step(now, t, dt, this.motionAllowed(), this.camera.position, px);
    }
    // A device alert's ring pulses only as ambient motion (an alert can stay open for hours: never a reason to keep
    // rendering on its own); still, it is a solid ring. SOS and hazards keep their own always-on pulse above.
    this.paintAlerts(t, ambient);
    if (this.ground) {
      this.ground.uniforms.uTime.value = t;
      this.ground.uniforms.uScan.value = ambient ? 1 : 0;
    }
    if (this.beaconBase) (this.beaconBase.material as THREE.MeshBasicMaterial).opacity = ambient ? 0.42 + 0.16 * Math.sin(t * 1.7) : 0.5;
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
    const measure = this.badgeEls.size > 0;
    this.labelRects = [];
    this.labelPool.forEach((el, i) => {
      const spec = this.labels[i];
      if (!spec) { el.style.display = "none"; return; }
      const at = this.world(spec.floorId, spec.x, spec.y, spec.h);
      if (!at) { el.style.display = "none"; return; }
      v.copy(at).project(this.camera);
      if (v.z > 1 || v.x < -1.1 || v.x > 1.1 || v.y < -1.1 || v.y > 1.1) { el.style.display = "none"; return; }
      el.style.display = "";
      if (el.textContent !== spec.text) { el.textContent = spec.text; delete el.dataset.w; }
      const tone = `twin-label ${spec.tone ? `is-${spec.tone}` : ""}`;
      if (el.className !== tone) { el.className = tone; delete el.dataset.w; }
      const sx = ((v.x + 1) / 2) * w, sy = ((1 - v.y) / 2) * h;
      el.style.transform = `translate(-50%, -100%) translate(${sx}px, ${sy}px)`;
      if (measure) {
        // Sizes are read once per text change (a layout read), not every frame.
        if (!el.dataset.w) { el.dataset.w = String(el.offsetWidth); el.dataset.h = String(el.offsetHeight); }
        const lw = Number(el.dataset.w), lh = Number(el.dataset.h);
        this.labelRects.push([sx - lw / 2, sy - lh, lw, lh]);
      }
    });
  }

  // ---- badges (immersive: projected DOM with real buttons) ---------------------------------------------------

  setBadgeHandler(fn: ((id: string, action: BadgeAction) => void) | null) { this.onBadge = fn; }

  /** The site's IANA time zone (windows look like night after 19:00 there); null: the deployment default. */
  setTimeZone(tz: string | null) { this.timeZone = tz; this.lifelike?.setTimeZone(tz); }

  /** Whether ambient motion is paused for lack of input (the controls poll waits too). */
  idle(): boolean { return performance.now() - this.lastActivity >= AMBIENT_IDLE_MS; }

  /** Moves keyboard focus to a device's badge (after a fixture click selects it). */
  focusBadge(id: string) { this.badgeEls.get(id)?.main.focus({ preventScroll: true }); }

  /** Badges to show; at most 24, keyed by id so a focused button keeps its node while the list changes. */
  setBadges(list: BadgeSpec[]) {
    const next = list.slice(0, MAX_BADGES);
    if (!next.length && !this.badgeEls.size) { this.badges = []; return; }
    if (!this.badgeHost) {
      const host = document.createElement("div");
      host.className = "twin-badges";
      host.setAttribute("role", "group");
      host.setAttribute("aria-label", "ป้ายสถานะอุปกรณ์ในฉาก");
      host.addEventListener("click", this.onBadgeClick);
      this.host.appendChild(host);
      this.badgeHost = host;
    }
    // The badge holding keyboard focus is never removed while focused (its device may have left the list).
    const focused = typeof document !== "undefined" ? document.activeElement : null;
    for (const [id, rec] of this.badgeEls) {
      if (focused && rec.el.contains(focused) && !next.some((b) => b.id === id)) {
        const last = this.badges.find((b) => b.id === id);
        if (last) next.push(last);
      }
    }
    const keep = new Set(next.map((b) => b.id));
    for (const [id, rec] of this.badgeEls) if (!keep.has(id)) { rec.el.remove(); this.badgeEls.delete(id); }
    for (const spec of next) {
      let rec = this.badgeEls.get(spec.id);
      if (!rec) {
        rec = makeBadge(spec.id);
        this.badgeHost.appendChild(rec.el);
        this.badgeEls.set(spec.id, rec);
      }
      fillBadge(rec, spec);
    }
    this.badges = next;
    this.invalidate();
  }

  private onBadgeClick = (e: MouseEvent) => {
    const btn = (e.target as Element | null)?.closest?.("button[data-badge]") as HTMLButtonElement | null;
    if (!btn || btn.disabled) return;
    e.stopPropagation();
    this.onBadge?.(btn.dataset.badge ?? "", (btn.dataset.action as BadgeAction) ?? "select");
  };

  /**
   * Places badges over their devices. A badge that would cover another badge or a zone label is lifted (a longer
   * leader line) up to twice; if it still collides it gives way, by priority and then distance to the camera.
   * Alerts, the selected device and a badge holding keyboard focus always stay. Badges far past the camera's
   * target hide when zoomed in.
   */
  private placeBadges() {
    if (!this.badgeEls.size) return;
    const w = this.host.clientWidth, h = this.host.clientHeight, v = new THREE.Vector3();
    const cam = this.camera.position;
    const focused = typeof document !== "undefined" ? document.activeElement : null;
    const items: { spec: BadgeSpec; rec: BadgeEl; sx: number; sy: number; dist: number; pinned: boolean }[] = [];
    for (const spec of this.badges) {
      const rec = this.badgeEls.get(spec.id);
      if (!rec) continue;
      // A fixture's badge floats above where it hangs (wall plate, ceiling light), not above the raw placement.
      const mt = this.lifelike?.mount(spec.id);
      const at = mt ? this.world(mt.floorId, mt.x, mt.y, mt.h + 0.3) : this.world(spec.floorId, spec.x, spec.y, spec.h);
      if (at) v.copy(at).project(this.camera);
      const holdsFocus = !!focused && rec.el.contains(focused);
      if (!at || v.z > 1 || v.x < -1.05 || v.x > 1.05 || v.y < -1.05 || v.y > 1.05) {
        // Off screen: hidden, unless it holds keyboard focus (it stays where it was last drawn).
        if (!holdsFocus) rec.el.style.display = "none";
        continue;
      }
      const pinned = !!spec.always || !!spec.selected || holdsFocus;
      items.push({ spec, rec, sx: ((v.x + 1) / 2) * w, sy: ((1 - v.y) / 2) * h, dist: at.distanceTo(cam), pinned });
    }
    items.sort((a, b) => Number(b.pinned) - Number(a.pinned) || b.spec.priority - a.spec.priority || a.dist - b.dist);
    const reach = Math.max(this.badgeRange, cam.distanceTo(this.controls.target) * 1.4);
    const placed: [number, number, number, number][] = [];
    const obstacles = this.labelRects;
    const clear = (x0: number, y0: number, bw: number, bh: number) => {
      const hits = (r: [number, number, number, number]) => x0 < r[0] + r[2] + 4 && x0 + bw + 4 > r[0] && y0 < r[1] + r[3] + 3 && y0 + bh + 3 > r[1];
      return !placed.some(hits) && !obstacles.some(hits);
    };
    for (const it of items) {
      const { rec } = it;
      if (!it.pinned && it.dist > reach) { rec.el.style.display = "none"; continue; }
      if (rec.w < 0) { rec.el.style.display = ""; rec.w = rec.el.offsetWidth; rec.h = rec.el.offsetHeight; }
      const x0 = it.sx - rec.w / 2;
      let lift = -1;
      for (let k = 0; k < 3; k++) {
        const y0 = it.sy - rec.h - 8 - k * (rec.h + 6);
        if (clear(x0, y0, rec.w, rec.h)) { lift = k; break; }
      }
      if (lift < 0 && !it.pinned) { rec.el.style.display = "none"; continue; }
      const k = Math.max(0, lift);
      const y0 = it.sy - rec.h - 8 - k * (rec.h + 6);
      placed.push([x0, y0, rec.w, rec.h]);
      rec.el.style.display = "";
      rec.el.style.transform = `translate(${Math.round(x0)}px, ${Math.round(y0)}px)`;
      rec.el.style.setProperty("--lead", `${8 + k * (rec.h + 6)}px`);
      rec.el.classList.toggle("is-far", it.dist > reach * 0.75);
    }
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
    this.lastBuild = { floors, activeId, explode, isolate };
    const { frames } = buildBuilding(this.building, { floors, activeId, assets: [], selection: null, explode, isolate, devices: false, people: false, labels: false, liveZones: false, glass: true, zones: false });
    this.floors = floors.map((f) => ({ id: f.id, draft: f.draft, frame: frames.find((fr) => fr.id === f.id) ?? { id: f.id, y: 0, originX: 0, originZ: 0, visible: false, active: false } }));
    if (this.style === "immersive" && this.theme) styleBuilding(this.building, this.theme);
    if (this.lifelike) hideItemBoxes(this.building, frames.find((fr) => fr.active)?.y ?? 0);
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
    const act = this.floors.find((f) => f.id === activeId) ?? this.floors[0];
    if (act) this.badgeRange = Math.max(45, Math.hypot(act.draft.width_m, act.draft.depth_m) * 1.5);
    const activeFloorY = frames.find((fr) => fr.active)?.y ?? 0;
    this.lifelike?.build(this.floors.map((f) => ({ id: f.id, draft: f.draft, visible: f.frame.visible, active: f.frame.active, above: f.frame.y > activeFloorY + 0.01 })), (fl, x, y, h) => this.world(fl, x, y, h), this.tier !== "high", this.state?.devices ?? []);
    this.buildStage();
    this.buildOutlines();
    for (const p of this.heatPlanes.values()) { p.texture.dispose(); }
    disposeTree(this.heat);
    this.heat.clear();
    this.heatPlanes.clear();
    if (this.state) this.setState(this.state, [...this.layers], true);
    this.applyModel();
    this.invalidate();
  }

  private buildStage() {
    this.ground?.dispose();
    disposeTree(this.stage);
    this.stage.clear();
    const active = this.floors.find((f) => f.id === this.activeId) ?? this.floors[0];
    if (!active) return;
    const size = Math.max(active.draft.width_m, active.draft.depth_m) * 2.6;
    this.ground = null;
    if (this.style === "immersive" && this.theme) {
      const centre = new THREE.Vector3(active.frame.originX + active.draft.width_m / 2, 0, active.frame.originZ + active.draft.depth_m / 2);
      this.ground = makeGround(this.theme, centre, size * 0.75, -0.2);
      this.stage.add(this.ground.group);
    } else {
      const grid = new THREE.GridHelper(size, Math.round(size / 2), 0x1d3038, 0x14222a);
      grid.position.y = -0.2;
      (grid.material as THREE.Material).transparent = true;
      (grid.material as THREE.Material).opacity = 0.55;
      this.stage.add(grid);
    }
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
    // A state change that matters (a switch, a door, an online state, an alert opening) counts as activity for ambient
    // motion; a temperature drifting, people moving or the same poll again does not.
    // People moving (headcounts, figures) are not: a busy hospital's wall screen must still idle.
    const sig = `${state.devices.map((d) => `${d.id}${d.on}${d.door}${d.online ? 1 : 0}`).join()}|${state.alerts.filter((a) => a.status === "open").map((a) => a.id).join()}`;
    if (sig !== this.stateSig) { this.stateSig = sig; this.onActivity(); }
    this.state = state;
    this.layers = new Set(layers);
    const now = Date.parse(state.server_time) || Date.now();
    this.lifelike?.setState(state, (a) => this.alertPlace(a));
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
        const mesh = new THREE.Mesh(new THREE.PlaneGeometry(f.draft.width_m, f.draft.depth_m), new THREE.MeshBasicMaterial({ map: texture, transparent: true, depthWrite: false, side: THREE.DoubleSide, opacity: f.frame.active ? (this.lifelike ? 0.62 : 1) : above ? 0.14 : 0.45 }));
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
    // Lifelike: switches, plugs, lights and units are fixtures, not beacons; sensors keep a beacon where they hang.
    const lf = this.lifelike;
    const devices = state.devices.filter((d) => d.kind === "device" && visible.has(d.floor_id) && !lf?.replacesBeacon(d.id))
      .map((d) => { const m = lf?.mount(d.id); return m ? { ...d, x: m.x, y: m.y, z: m.h, mounted: true } : d; });
    const gateways = state.devices.filter((d) => d.kind === "gateway" && visible.has(d.floor_id));
    const low = this.tier === "low";
    const immersive = this.style === "immersive" && !!this.theme;
    const key = `${this.style}:${low}`;
    const restyle = key !== this.markerKey;
    this.markerKey = key;
    if (!this.deviceMesh || restyle || this.deviceMesh.count !== devices.length || this.deviceIndex.length !== devices.length) {
      if (this.deviceMesh) { this.markers.remove(this.deviceMesh); this.deviceMesh.geometry.dispose(); (this.deviceMesh.material as THREE.Material).dispose(); this.deviceMesh.dispose(); }
      // Immersive: an unlit core in the status colour (bright enough to bloom on the high tier).
      this.deviceMesh = immersive
        ? new THREE.InstancedMesh(new THREE.SphereGeometry(0.17, low ? 8 : 16, low ? 6 : 12), new THREE.MeshBasicMaterial({ color: 0xffffff, toneMapped: false }), Math.max(1, devices.length))
        : new THREE.InstancedMesh(new THREE.SphereGeometry(0.22, low ? 8 : 16, low ? 6 : 12), new THREE.MeshStandardMaterial({ color: 0xffffff, emissive: 0x3a5a52, emissiveIntensity: 0.6, roughness: 0.35 }), Math.max(1, devices.length));
      this.deviceMesh.userData.pick = "device";
      this.markers.add(this.deviceMesh);
    }
    if (!this.gatewayMesh || restyle || this.gatewayIndex.length !== gateways.length) {
      if (this.gatewayMesh) { this.markers.remove(this.gatewayMesh); this.gatewayMesh.geometry.dispose(); (this.gatewayMesh.material as THREE.Material).dispose(); this.gatewayMesh.dispose(); }
      // Immersive: a hub tower standing on the floor up to the gateway's mounting height.
      this.gatewayMesh = immersive
        ? new THREE.InstancedMesh(towerGeometry(), new THREE.MeshStandardMaterial({ color: 0xffffff, emissive: this.theme!.blue, emissiveIntensity: 0.35, metalness: 0.45, roughness: 0.3 }), Math.max(1, gateways.length))
        : new THREE.InstancedMesh(new THREE.BoxGeometry(0.62, 0.2, 0.62), new THREE.MeshStandardMaterial({ color: 0xffffff, emissive: 0x2c4c46, emissiveIntensity: 0.5, roughness: 0.4 }), Math.max(1, gateways.length));
      this.gatewayMesh.userData.pick = "gateway";
      this.markers.add(this.gatewayMesh);
    }
    this.deviceIndex = devices;
    this.gatewayIndex = gateways;
    const m = new THREE.Matrix4(), c = new THREE.Color();
    // Switched outputs glow warm when on and dim when off, so a light switched from the UI is visible in the model.
    const tone = (d: TwinDevice) => (d.sos || d.alert ? CORAL : !d.online ? (d.last_at ? AMBER : MUTED) : d.door === 1 ? AMBER : d.on === 1 ? LAMP : d.on === 0 ? LAMP_OFF : ACCENT);
    const now = Date.parse(state.server_time) || Date.now();
    const stems: number[] = [];
    this.deviceMesh.count = devices.length;
    devices.forEach((d, i) => {
      const at = this.world(d.floor_id, d.x, d.y, d.z)!;
      m.makeTranslation(at.x, at.y, at.z);
      if (d.on === 1) m.scale(new THREE.Vector3(immersive ? 1.35 : 1.45, immersive ? 1.35 : 1.45, immersive ? 1.35 : 1.45));
      // A sensor on its wall or ceiling is a small status dot, without a stem.
      const mounted = "mounted" in d;
      if (mounted) m.scale(new THREE.Vector3(0.55, 0.55, 0.55));
      this.deviceMesh!.setMatrixAt(i, m);
      this.deviceMesh!.setColorAt(i, immersive ? beaconColor(this.theme!, beaconTone(d, now), c) : c.setHex(tone(d)));
      if (!mounted) stems.push(at.x, at.y, at.z, at.x, at.y - d.z + 0.1, at.z);
    });
    this.gatewayMesh.count = gateways.length;
    gateways.forEach((g, i) => {
      const at = this.world(g.floor_id, g.x, g.y, g.z)!;
      if (immersive) {
        m.makeScale(1, Math.min(3.2, Math.max(0.7, g.z / 1.05)), 1).setPosition(at.x, at.y - g.z, at.z);
        this.gatewayMesh!.setColorAt(i, g.online ? c.setHex(this.theme!.blue) : beaconColor(this.theme!, g.last_at ? "warn" : "off", c));
      } else {
        m.makeTranslation(at.x, at.y, at.z);
        this.gatewayMesh!.setColorAt(i, c.setHex(g.online ? BLUE : AMBER));
        stems.push(at.x, at.y, at.z, at.x, at.y - g.z + 0.1, at.z);
      }
      this.gatewayMesh!.setMatrixAt(i, m);
    });
    for (const im of [this.deviceMesh, this.gatewayMesh]) {
      im.instanceMatrix.needsUpdate = true;
      if (im.instanceColor) im.instanceColor.needsUpdate = true;
      im.computeBoundingSphere();
    }
    if (this.stems) { this.markers.remove(this.stems); this.stems.geometry.dispose(); (this.stems.material as THREE.Material).dispose(); }
    const geo = new THREE.BufferGeometry();
    geo.setAttribute("position", new THREE.Float32BufferAttribute(stems, 3));
    this.stems = new THREE.LineSegments(geo, immersive
      ? new THREE.LineBasicMaterial({ color: this.theme!.accent, transparent: true, opacity: 0.22, toneMapped: false })
      : new THREE.LineBasicMaterial({ color: 0x5d7279, transparent: true, opacity: 0.5 }));
    this.markers.add(this.stems);
    // Fixtures have no beacon, but an alerting one still gets the pulsing ring (at the floor under its mount).
    const alerting = immersive ? state.devices.filter((d) => d.kind === "device" && visible.has(d.floor_id) && (d.alert || d.sos))
      .map((d) => { const mt = lf?.mount(d.id); return mt ? { ...d, x: mt.x, y: mt.y } : d; }) : [];
    this.updateBeacons(immersive ? devices : [], now, restyle, alerting);
  }

  /**
   * Immersive: a glowing ring on the floor under every device and a pulsing ring under every alerting one. Both are
   * instanced and updated in place on every poll (matrices and colours only); a device keeps its pulse phase.
   */
  private updateBeacons(devices: TwinDevice[], now: number, restyle: boolean, alertingDevices: TwinDevice[]) {
    const drop = (im: THREE.InstancedMesh | null) => { if (im) { im.removeFromParent(); im.geometry.dispose(); (im.material as THREE.Material).dispose(); im.dispose(); } return null; };
    if (restyle || !devices.length || !this.theme) { this.beaconBase = drop(this.beaconBase); this.alertMesh = drop(this.alertMesh); }
    if (!devices.length || !this.theme) { this.alerting = []; this.phases.clear(); return; }
    const low = this.tier === "low";
    if (!this.beaconBase || this.beaconBase.instanceMatrix.count < devices.length) {
      this.beaconBase = drop(this.beaconBase);
      const ring = new THREE.RingGeometry(0.24, 0.38, low ? 16 : 32);
      ring.rotateX(-Math.PI / 2);
      this.beaconBase = new THREE.InstancedMesh(ring, new THREE.MeshBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.5, blending: THREE.AdditiveBlending, depthWrite: false, toneMapped: false }), Math.max(16, devices.length));
      this.beaconBase.renderOrder = 3;
      this.markers.add(this.beaconBase);
    }
    const m = new THREE.Matrix4(), c = new THREE.Color();
    this.beaconBase.count = devices.length;
    devices.forEach((d, i) => {
      const at = this.world(d.floor_id, d.x, d.y, 0.11)!;
      this.beaconBase!.setMatrixAt(i, m.makeTranslation(at.x, at.y, at.z));
      this.beaconBase!.setColorAt(i, beaconColor(this.theme!, beaconTone(d, now), c));
    });
    this.beaconBase.instanceMatrix.needsUpdate = true;
    if (this.beaconBase.instanceColor) this.beaconBase.instanceColor.needsUpdate = true;
    this.beaconBase.computeBoundingSphere();
    if (!this.alertMesh) {
      const ring = new THREE.RingGeometry(0.4, 0.52, 40);
      ring.rotateX(-Math.PI / 2);
      // Additive blending: an instance's colour scaled down is its opacity.
      this.alertMesh = new THREE.InstancedMesh(ring, new THREE.MeshBasicMaterial({ color: 0xffffff, transparent: true, side: THREE.DoubleSide, depthWrite: false, blending: THREE.AdditiveBlending, toneMapped: false }), 24);
      this.alertMesh.renderOrder = 4;
      this.fx.add(this.alertMesh);
    }
    const alerting = alertingDevices.slice(0, 24);
    const ids = new Set(alerting.map((d) => d.id));
    for (const id of this.phases.keys()) if (!ids.has(id)) this.phases.delete(id);
    this.alerting = alerting.flatMap((d) => {
      const at = this.world(d.floor_id, d.x, d.y, 0.13);
      if (!at) return [];
      if (!this.phases.has(d.id)) this.phases.set(d.id, (this.phases.size * 0.27) % 1);
      return [{ at, phase: this.phases.get(d.id)! }];
    });
    this.alertMesh.count = this.alerting.length;
    this.alertMesh.frustumCulled = false;
  }

  /** The alert rings' frame: a pulse with ambient motion, a solid ring without it. */
  private paintAlerts(t: number, ambient: boolean) {
    if (!this.alertMesh || !this.theme || !this.alerting.length) return;
    const { m, q, sc, c, base } = this.paintTmp;
    beaconColor(this.theme, "alert", base);
    this.alerting.forEach((a, i) => {
      const k = ambient ? (t * 0.9 + a.phase) % 1 : -1;
      sc.setScalar(k < 0 ? 1.8 : 1 + k * 2.6);
      this.alertMesh!.setMatrixAt(i, m.compose(a.at, q, sc));
      this.alertMesh!.setColorAt(i, c.copy(base).multiplyScalar(k < 0 ? 0.6 : 0.85 * (1 - k)));
    });
    this.alertMesh.instanceMatrix.needsUpdate = true;
    if (this.alertMesh.instanceColor) this.alertMesh.instanceColor.needsUpdate = true;
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
    if (this.lifelike) {
      // Lifelike: the first people walk as figures inside their zone; past MAX_FIGURES the rest stay tokens.
      // A figure's key: in counts and tracks the slot (gateway#index, an SOS first), never the pseudonym (it changes
      // with every response), so a refresh moves and re-dresses nobody; in named mode the tag, so a person keeps their
      // figure and walks into the next zone.
      const named = state.presence.mode === "named";
      const specs: WalkerSpec[] = [];
      for (const [z, list] of byZone) {
        const keyed: { key: string; sos: boolean }[] = [];
        const byGateway = new Map<string, typeof list>();
        for (const e of list) byGateway.set(e.gateway, [...(byGateway.get(e.gateway) ?? []), e]);
        for (const [gw, group] of byGateway) {
          group.sort((a, b) => Number(b.sos) - Number(a.sos) || (named ? a.id.localeCompare(b.id) : 0));
          group.forEach((e, i) => keyed.push({ key: named ? `tag:${e.id}` : `${gw}#${i}`, sos: e.sos }));
        }
        // Slots by key too, so a figure's place (with motion reduced) stays put across refreshes.
        const slots = assignSlots(keyed.map((k) => k.key), z.slots);
        for (const k of keyed) specs.push({ id: k.key, floorId: z.floorId, zoneKey: `${z.floorId}:${z.index}`, poly: z.zone.points as P[], slot: slots.get(k.key)!, sos: k.sos });
      }
      const figures = specs.slice(0, MAX_FIGURES);
      this.lifelike.setPeople(figures, this.tier === "high" ? "figure" : "capsule", now);
      // The tokens of the people drawn as figures go (the first MAX_FIGURES; the rest stay tokens).
      let n = 0;
      for (const id of [...next.keys()]) if (n++ < figures.length) next.delete(id);
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

  // ---- building model ------------------------------------------------------------------------------------------

  /** The loaded model's key (site and revision), "" when none. */
  modelKey(): string { return this.modelLayer.key; }

  /** Shows a parsed model (null removes it). Its transform is applied from setModelTransform. */
  setModel(model: Flattened | null, key = "") {
    if (model) this.modelLayer.set(model, key);
    else this.modelLayer.clear();
    this.applyModel();
  }

  /** Lifelike: states the live view lacks (an AC's switch, a blind's position), from the devices' controls. */
  setFixtureStates(map: Record<string, FixtureState>) { this.lifelike?.setFixtureStates(map); this.invalidate(); }

  setModelTransform(t: ModelTransform | null) {
    this.modelTransform = t;
    this.applyModel();
  }

  /** Scale and offset that fit the model inside the plans' footprint (the widest and deepest floor). */
  fitModel(t: ModelTransform): Pick<ModelTransform, "scale" | "offset_m"> | null {
    const width = this.floors.reduce((s, f) => Math.max(s, f.draft.width_m), 0);
    const depth = this.floors.reduce((s, f) => Math.max(s, f.draft.depth_m), 0);
    if (!width || !depth) return null;
    return this.modelLayer.fit(t, width, depth);
  }

  private planPlace(): PlanPlace {
    const f0 = this.floors[0];
    const active = this.floors.find((f) => f.frame.active) ?? f0;
    const baseY = this.floors.length ? Math.min(...this.floors.map((f) => f.frame.y)) : 0;
    return {
      originX: f0?.frame.originX ?? 0,
      originZ: f0?.frame.originZ ?? 0,
      baseY,
      clipY: active ? active.frame.y + active.draft.ceiling_m + 0.02 : null,
    };
  }

  private applyModel() {
    const t = this.modelTransform;
    if (!t || !this.modelLayer.loaded) {
      this.lifelike?.setFurnitureVisible(true);
      this.lifelike?.setReplaceMode(false);
      this.modelLayer.group.visible = false;
      this.building.visible = true;
      this.invalidate();
      return;
    }
    this.modelLayer.apply(t, this.planPlace());
    // A customer model has its own furniture: no procedural interior while it is shown.
    this.lifelike?.setFurnitureVisible(!t.visible);
    this.lifelike?.setReplaceMode(t.visible && t.mode === "replace");
    // replace: the model stands in for the generated building; devices, heat and people stay.
    this.building.visible = !(t.visible && t.mode === "replace");
    this.invalidate();
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

  private onDown = (e: PointerEvent) => { this.downAt = e.button === 0 ? [e.clientX, e.clientY] : null; };

  private onUp = (e: PointerEvent) => {
    // Clear the press first: an orbit drag must not leave a stale press behind.
    const down = this.downAt;
    this.downAt = null;
    // Only a primary-button click picks (a right or middle click, or a drag, does nothing).
    if (e.button !== 0 || !down || Math.hypot(e.clientX - down[0], e.clientY - down[1]) > 4) return;
    const r = this.renderer.domElement.getBoundingClientRect();
    const pointer = new THREE.Vector2(((e.clientX - r.left) / r.width) * 2 - 1, -((e.clientY - r.top) / r.height) * 2 + 1);
    this.raycaster.setFromCamera(pointer, this.camera);
    const hits = this.raycaster.intersectObjects([this.deviceMesh, this.gatewayMesh].filter((x): x is THREE.InstancedMesh => !!x), false);
    const hit = hits[0];
    const fixture = this.lifelike?.pick(this.raycaster);
    // A sensor or beacon beside a light panel wins when both are hit within half a metre (the panel is big, the
    // sensor is what the operator aims at).
    const beaconFirst = !!hit && !!fixture && fixture.kind === "light" && hit.distance - fixture.distance < 0.5;
    if (fixture && !beaconFirst && (!hit || fixture.distance <= hit.distance)) { this.onPick?.({ kind: "fixture", id: fixture.id, toggle: fixture.toggle }); return; }
    if (!hit || hit.instanceId === undefined) { this.onPick?.(null); return; }
    const kind = hit.object.userData.pick as "device" | "gateway";
    const d = (kind === "device" ? this.deviceIndex : this.gatewayIndex)[hit.instanceId];
    this.onPick?.(d ? { kind, id: d.id } : null);
  };
}

/**
 * Lifelike: the plan items the interior draws as furniture (the grey boxes of beds, desks, stairs, lifts, racks on
 * the active floor) and the doors and windows the effects draw (the active floor and those below) are hidden from the
 * generic building.
 */
function hideItemBoxes(building: THREE.Group, activeY: number) {
  for (const g of building.children) {
    const active = Math.abs(g.position.y - activeY) < 0.01, above = g.position.y > activeY + 0.01;
    g.traverse((o) => {
      const mesh = o as THREE.Mesh;
      const mat = mesh.material as THREE.MeshStandardMaterial | undefined;
      if (!mesh.isMesh || !mat?.isMeshStandardMaterial || (mat as unknown as THREE.MeshPhysicalMaterial).isMeshPhysicalMaterial) return;
      const near = (hex: number) => { const c = new THREE.Color(hex); return Math.abs(mat.color.r - c.r) + Math.abs(mat.color.g - c.g) + Math.abs(mat.color.b - c.b) < 0.01; };
      // building.ts colours items by type: blue doors and windows, muted grey for beds, desks, stairs, lifts, racks.
      if ((near(BLUE) && !above) || (active && near(MUTED) && mesh !== g.children[0])) mesh.visible = false;
    });
  }
}

/** A device's beacon status: alert coral, stale or door open amber, offline dim grey, a switched-off output dimmed. */
function beaconTone(d: TwinDevice, now: number): BeaconTone {
  if (d.sos || d.alert) return "alert";
  if (!d.online) return d.last_at && now - Date.parse(d.last_at) < 15 * 60000 ? "warn" : "off";
  if (d.door === 1) return "warn";
  return d.on === 0 ? "idle" : "ok";
}

const SVG_NS = "http://www.w3.org/2000/svg";

function powerIcon(): SVGSVGElement {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", "14");
  svg.setAttribute("height", "14");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "2.2");
  svg.setAttribute("stroke-linecap", "round");
  for (const d of ["M12 2v10", "M18.4 6.6a9 9 0 1 1-12.77.04"]) {
    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", d);
    svg.appendChild(path);
  }
  return svg;
}

function makeBadge(id: string): BadgeEl {
  const el = document.createElement("div");
  el.className = "twin-badge";
  const main = document.createElement("button");
  main.type = "button";
  main.className = "twin-badge-main";
  main.dataset.badge = id;
  main.dataset.action = "select";
  const dot = document.createElement("i");
  dot.className = "twin-badge-dot";
  dot.setAttribute("aria-hidden", "true");
  const name = document.createElement("span");
  name.className = "twin-badge-name";
  const value = document.createElement("span");
  value.className = "twin-badge-value";
  main.appendChild(dot);
  main.appendChild(name);
  main.appendChild(value);
  const msg = document.createElement("span");
  msg.className = "twin-badge-msg";
  msg.setAttribute("role", "status");
  el.appendChild(main);
  el.appendChild(msg);
  return { el, main, name, value, power: null, msg, sig: "", w: -1, h: -1 };
}

const TOGGLE_TEXT: Record<string, string> = { pending: "กำลังสั่ง…", sent: "รออุปกรณ์ยืนยัน…", confirmed: "อุปกรณ์ยืนยันแล้ว" };

/** Updates a badge's content in place (text only through textContent: names come from users). */
function fillBadge(rec: BadgeEl, b: BadgeSpec) {
  const sig = JSON.stringify([b.name, b.value, b.status, b.tone, b.selected, b.toggle, b.message]);
  if (sig === rec.sig) return;
  rec.sig = sig;
  rec.w = -1;
  rec.el.className = `twin-badge is-${b.tone}${b.selected ? " is-selected" : ""}`;
  rec.name.textContent = b.name;
  rec.value.textContent = b.value;
  rec.value.hidden = !b.value;
  rec.main.setAttribute("aria-label", `${b.name}${b.value ? ` · ${b.value}` : ""} · ${b.status} · เปิดการ์ดอุปกรณ์`);
  rec.main.setAttribute("aria-pressed", b.selected ? "true" : "false");
  if (b.toggle) {
    if (!rec.power) {
      const p = document.createElement("button");
      p.type = "button";
      p.className = "twin-badge-power";
      p.dataset.badge = rec.main.dataset.badge ?? "";
      p.dataset.action = "toggle";
      p.appendChild(powerIcon());
      rec.el.insertBefore(p, rec.msg);
      rec.power = p;
    }
    const t = b.toggle;
    const busy = t.state === "pending" || t.state === "sent";
    rec.power.disabled = t.disabled || busy;
    rec.power.className = `twin-badge-power${t.on ? " is-on" : ""}${busy ? " is-pending" : ""}`;
    // A switch always states whether it is on; with the state unknown it is a plain button.
    if (t.on === null) { rec.power.removeAttribute("role"); rec.power.removeAttribute("aria-checked"); }
    else { rec.power.setAttribute("role", "switch"); rec.power.setAttribute("aria-checked", t.on ? "true" : "false"); }
    rec.power.setAttribute("aria-busy", busy ? "true" : "false");
    // A stable name: the state is aria-checked's job (and aria-busy's while a command is on its way).
    rec.power.setAttribute("aria-label", t.on === null ? `เปิด/ปิด ${b.name} (ยังไม่ทราบสถานะ)` : `สวิตช์ ${b.name}`);
    rec.power.title = busy ? TOGGLE_TEXT[t.state] : t.on ? "เปิดอยู่ · กดเพื่อปิด" : "ปิดอยู่ · กดเพื่อเปิด";
  } else if (rec.power) {
    rec.power.remove();
    rec.power = null;
  }
  const msg = b.message ?? (b.toggle && b.toggle.state !== "idle" && b.toggle.state !== "failed" ? TOGGLE_TEXT[b.toggle.state] : "");
  rec.msg.textContent = msg ?? "";
  rec.msg.hidden = !msg;
  rec.msg.className = `twin-badge-msg${b.toggle?.state === "failed" ? " is-error" : ""}`;
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
