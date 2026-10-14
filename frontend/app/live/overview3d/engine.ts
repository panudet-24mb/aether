// The overview's "command constellation" renderer (docs/platform/overview-3d.md). Plain three.js, imperative:
// React owns the page and the labels, this class owns the WebGL scene and positions the label elements React
// renders. Patterns follow the digital twin's engine (copied, not imported: the twin evolves on its own).
//
// Rendering is on demand: a frame is drawn only when something changed, the camera moves, a tween or the tour
// runs, or an ambient pulse is on screen. Ambient pulses (online hubs breathing, alert rings, SOS pillars) run at
// ~30 fps and stop entirely with prefers-reduced-motion or while the tab is hidden. Nodes are instanced per shape.

import * as THREE from "three";
import { OrbitControls } from "three/examples/jsm/controls/OrbitControls.js";
import { RoomEnvironment } from "three/examples/jsm/environments/RoomEnvironment.js";
import { EffectComposer } from "three/examples/jsm/postprocessing/EffectComposer.js";
import { OutputPass } from "three/examples/jsm/postprocessing/OutputPass.js";
import { RenderPass } from "three/examples/jsm/postprocessing/RenderPass.js";
import { UnrealBloomPass } from "three/examples/jsm/postprocessing/UnrealBloomPass.js";
import { CSS3DObject, CSS3DRenderer } from "three/examples/jsm/renderers/CSS3DRenderer.js";
import { KPI_PX, LIST_PX_W, pickLabels, wallSlots, type ScreenRect } from "./layout";
import { labelTier, poweredOn, SHAPES, shapeOf, type HubPlace, type NodePlace, type Shape } from "./types";

export type Quality = "high" | "med" | "low";

/** Where an in-scene panel sits: a world point, beside a device (the detail), or above one (the SOS banner). */
export type PanelPlace = { at: [number, number, number] } | { beside: string } | { above: string } | { slot: string };
export type PanelKind = "kpi" | "list" | "sos" | "detail";
/** One CSS3D panel: React renders into `el` (a portal); the engine places, turns and fades it. `width` is in world units. */
export type PanelSpec = { el: HTMLElement; place: PanelPlace; width: number; kind: PanelKind };
/** CSS pixel width of each panel kind (stage.css sets the same widths). */
const PANEL_PX: Record<PanelKind, number> = { kpi: KPI_PX.w, list: LIST_PX_W, sos: 600, detail: 460 };
/** Wall slots sit behind (KPIs) or a little in front of (lists) the default view's target. */
const SLOT_DEPTH: Partial<Record<PanelKind, number>> = { kpi: 1.2, list: 0.95 };
/** Pixels the HUD row takes at the top of the stage, and the bottom-corner controls at the bottom. */
const HUD_ROW = 60, BOTTOM_ROW = 48;
/** The CSS3D scene runs at K× the WebGL scale, so panels sit near scale 1 and the browser rasterises text crisply. */
const CSS_K = 40;

/** Panels React has mounted, by key; the engine binds to it once it exists (panels can mount before the engine). */
export class PanelRegistry {
  readonly items = new Map<string, PanelSpec>();
  private listener: ((key: string, spec: PanelSpec | null) => void) | null = null;
  set(key: string, spec: PanelSpec) { this.items.set(key, spec); this.listener?.(key, spec); }
  remove(key: string) { this.items.delete(key); this.listener?.(key, null); }
  bind(fn: ((key: string, spec: PanelSpec | null) => void) | null) {
    this.listener = fn;
    if (fn) for (const [k, v] of this.items) fn(k, v);
  }
}
export type Preset = "overview" | "top" | "alert";
export type Palette = Record<"paper" | "panel" | "ink" | "muted" | "dim" | "accent" | "warning" | "danger" | "border" | "grid" | "blue", THREE.Color>;
export type Pick = { kind: "node"; key: string } | { kind: "hub"; id: string } | null;
export type EngineStats = { fps: number; calls: number; triangles: number; tier: Quality };
export type EngineHooks = {
  onPick: (p: Pick) => void;
  /** The hovered node changed (null: none). The tooltip element follows the pointer by itself. */
  onHover: (key: string | null) => void;
  /** The set of devices that should carry a label changed (priority, then distance to the camera). */
  onLabels: (keys: string[]) => void;
  /** The tour stopped because the operator took the camera. */
  onTourEnd: () => void;
  /** The stage's controllable rule (a Tuya card counts only once it reported a switch); ranks labels. */
  controllable: (card: NodePlace["card"]) => boolean;
  labelEl: (key: string) => HTMLElement | undefined;
  hubLabelEl: (id: string) => HTMLElement | undefined;
  tooltipEl: () => HTMLElement | null;
};

const MAX_LABELS = 40;
// Ambient pulses: alerts and SOS at 30 fps, the slow breathing of online hubs alone at 20 fps.
const AMBIENT_MS = 1000 / 30, BREATH_MS = 1000 / 20;
const easeInOut = (t: number) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2);

function radialTexture(): THREE.CanvasTexture {
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = 128;
  const ctx = canvas.getContext("2d")!;
  const g = ctx.createRadialGradient(64, 64, 0, 64, 64, 64);
  g.addColorStop(0, "rgba(255,255,255,1)");
  g.addColorStop(0.35, "rgba(255,255,255,0.45)");
  g.addColorStop(1, "rgba(255,255,255,0)");
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, 128, 128);
  const t = new THREE.CanvasTexture(canvas);
  t.colorSpace = THREE.SRGBColorSpace;
  return t;
}

/** Standard material whose emissive light takes the instance colour, so every node glows in its status colour. */
function glowingStandard(emissive: number, opts: THREE.MeshStandardMaterialParameters = {}): THREE.MeshStandardMaterial {
  const m = new THREE.MeshStandardMaterial({ color: 0xffffff, emissive: 0xffffff, emissiveIntensity: emissive, metalness: 0.35, roughness: 0.28, ...opts });
  m.onBeforeCompile = (shader) => {
    shader.fragmentShader = shader.fragmentShader.replace("#include <emissivemap_fragment>", "#include <emissivemap_fragment>\n#ifdef USE_COLOR\n totalEmissiveRadiance *= vColor.rgb;\n#endif");
  };
  m.customProgramCacheKey = () => "ov3d-glow";
  return m;
}

function shapeGeometry(s: Shape, low: boolean): THREE.BufferGeometry {
  const seg = low ? 10 : 20;
  switch (s) {
    case "cube": return new THREE.BoxGeometry(0.5, 0.5, 0.5);
    case "diamond": return new THREE.OctahedronGeometry(0.42);
    case "capsule": return new THREE.CapsuleGeometry(0.2, 0.42, low ? 3 : 6, seg);
    case "slab": { const g = new THREE.BoxGeometry(0.5, 0.78, 0.08); g.translate(0.25, 0, 0); return g; } // hinged at its left edge
    case "gem": return new THREE.IcosahedronGeometry(0.34, 0);
    default: return new THREE.SphereGeometry(0.3, seg, Math.round(seg * 0.75));
  }
}

/** The holographic platform: a radial-faded grid with concentric rings, drawn additively over a dark mirror disc. */
function gridMaterial(color: THREE.Color, accent: THREE.Color, radius: number): THREE.ShaderMaterial {
  return new THREE.ShaderMaterial({
    transparent: true, depthWrite: false, blending: THREE.AdditiveBlending,
    uniforms: { uColor: { value: color.clone() }, uAccent: { value: accent.clone() }, uRadius: { value: radius } },
    vertexShader: "varying vec2 vP; void main(){ vP = position.xy; gl_Position = projectionMatrix * modelViewMatrix * vec4(position, 1.0); }",
    fragmentShader: `
      uniform vec3 uColor; uniform vec3 uAccent; uniform float uRadius; varying vec2 vP;
      float line(float v, float w){ float d = fwidth(v); return 1.0 - smoothstep(0.0, d * w, abs(fract(v - 0.5) - 0.5)); }
      void main(){
        float r = length(vP);
        float fade = 1.0 - smoothstep(uRadius * 0.55, uRadius, r);
        float grid = max(line(vP.x / 2.0, 1.2), line(vP.y / 2.0, 1.2)) * 0.32;
        float rings = line(r / 6.0, 1.5) * 0.55;
        float rim = smoothstep(uRadius * 0.985, uRadius * 0.995, r) * (1.0 - smoothstep(uRadius * 0.995, uRadius, r));
        vec3 c = uColor * grid + uAccent * (rings * 0.35 + rim * 0.9);
        gl_FragColor = vec4(c * fade + uAccent * rim * 0.4, 1.0);
      }`,
  });
}

type NodeRef = { place: NodePlace; shape: Shape; index: number };

export class ConstellationEngine {
  readonly renderer: THREE.WebGLRenderer;
  readonly scene = new THREE.Scene();
  readonly camera = new THREE.PerspectiveCamera(40, 1, 0.1, 4000);
  readonly controls: OrbitControls;
  private host: HTMLElement;
  private hooks: EngineHooks;
  private palette: Palette;
  private hemi: THREE.HemisphereLight;
  private layout: { hubs: HubPlace[]; nodes: NodePlace[]; radius: number } | null = null;
  private composer: EffectComposer | null = null;
  private bloom: UnrealBloomPass | null = null;
  private envMap: THREE.Texture;
  private tier: Quality;
  private autoTier: boolean;
  private reducedMotion: boolean;
  private hidden = false;

  private platform = new THREE.Group();
  private hubGroup = new THREE.Group();
  private nodeGroup = new THREE.Group();
  private fx = new THREE.Group();
  private glowTex = radialTexture();
  private geos: Record<Shape, THREE.BufferGeometry>;
  private bodyMat = glowingStandard(0.55);
  private shared: THREE.Material[] = [];
  private sharedGeo: THREE.BufferGeometry[] = [];

  private hubs: HubPlace[] = [];
  private nodes: NodePlace[] = [];
  private refs = new Map<string, NodeRef>();
  private radius = 20;
  private bodies = new Map<Shape, THREE.InstancedMesh>();
  private pickNodes: THREE.InstancedMesh | null = null;
  private pickHubs: THREE.InstancedMesh | null = null;
  private hubRings: THREE.InstancedMesh | null = null;
  private alertRings: THREE.InstancedMesh | null = null;
  private alertIdx: number[] = [];
  private sosFx: { pillar: THREE.Mesh; ring: THREE.Mesh; phase: number; key: string }[] = [];
  private hoverRing: THREE.Mesh;
  private hovered: string | null = null;
  private labelKeys: string[] = [];
  private lastLabelPick = 0;
  private framed = false;

  private dirty = true;
  private raf = 0;
  private tween: { fromPos: THREE.Vector3; toPos: THREE.Vector3; fromTarget: THREE.Vector3; toTarget: THREE.Vector3; start: number; ms: number } | null = null;
  private touring = false;
  private lastAmbient = 0;
  private frameTimes: number[] = [];
  private lastFrame = 0;
  private fps = 0;
  private ro: ResizeObserver;
  private downAt: [number, number] | null = null;
  private pointer: { x: number; y: number; cx: number; cy: number } | null = null;
  private pointerDirty = false;
  private raycaster = new THREE.Raycaster();
  private disposed = false;

  private css: CSS3DRenderer;
  private cssHost: HTMLElement | null;
  private cssScene = new THREE.Scene();
  private cssCamera = new THREE.PerspectiveCamera();
  private panels = new Map<string, { obj: CSS3DObject; spec: PanelSpec; at: THREE.Vector3; width: number }>();
  private safeTop = 0;
  private safeBottom = 0;
  private registry: PanelRegistry | null = null;
  private cssDirty = true;
  private lastCam = new THREE.Matrix4();
  private lastProj = new THREE.Matrix4();

  constructor(host: HTMLElement, palette: Palette, hooks: EngineHooks, opts: { quality?: Quality | "auto"; reducedMotion?: boolean; cssHost?: HTMLElement | null } = {}) {
    this.host = host;
    this.hooks = hooks;
    this.palette = palette;
    this.autoTier = !opts.quality || opts.quality === "auto";
    this.tier = opts.quality && opts.quality !== "auto" ? opts.quality : "high";
    this.reducedMotion = !!opts.reducedMotion;
    // Throws when WebGL 2 is unavailable; the stage catches it and falls back to the cards.
    this.renderer = new THREE.WebGLRenderer({ antialias: this.tier !== "low", alpha: false, powerPreference: "high-performance" });
    this.renderer.setClearColor(palette.paper, 1);
    this.renderer.info.autoReset = false;
    this.renderer.outputColorSpace = THREE.SRGBColorSpace;
    host.appendChild(this.renderer.domElement);
    this.renderer.domElement.style.display = "block";
    // In-scene panels: a CSS3D layer over the canvas sharing the camera (crisp, clickable, accessible Thai text).
    this.css = new CSS3DRenderer();
    this.css.domElement.className = "ov3d-css";
    this.cssHost = opts.cssHost ?? null;
    this.cssHost?.appendChild(this.css.domElement);

    const pmrem = new THREE.PMREMGenerator(this.renderer);
    const room = new RoomEnvironment();
    this.envMap = pmrem.fromScene(room, 0.04).texture;
    room.dispose();
    pmrem.dispose();
    this.scene.environment = this.envMap;
    this.scene.environmentIntensity = 0.45;
    this.scene.background = palette.paper.clone();
    this.hemi = new THREE.HemisphereLight(palette.ink, palette.paper, 0.35);
    this.scene.add(this.hemi);
    const key = new THREE.DirectionalLight(0xffffff, 0.8);
    key.position.set(30, 60, 25);
    this.scene.add(key);
    for (const g of [this.platform, this.hubGroup, this.nodeGroup, this.fx]) this.scene.add(g);

    const low = this.tier === "low";
    this.geos = Object.fromEntries(SHAPES.map((s) => [s, shapeGeometry(s, low)])) as Record<Shape, THREE.BufferGeometry>;
    this.hoverRing = new THREE.Mesh(new THREE.RingGeometry(0.62, 0.74, 48), new THREE.MeshBasicMaterial({ color: palette.ink, transparent: true, opacity: 0.9, side: THREE.DoubleSide, depthWrite: false, blending: THREE.AdditiveBlending }));
    this.hoverRing.rotation.x = -Math.PI / 2;
    this.hoverRing.visible = false;
    this.fx.add(this.hoverRing);

    this.camera.position.set(0, 30, 50);
    this.controls = new OrbitControls(this.camera, this.renderer.domElement);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.08;
    this.controls.maxPolarAngle = Math.PI / 2 - 0.06;
    this.controls.minDistance = 4;
    this.controls.maxDistance = 900;
    this.controls.screenSpacePanning = false;
    this.controls.listenToKeyEvents(host);
    this.controls.addEventListener("change", () => this.invalidate());
    this.controls.addEventListener("start", () => { this.tween = null; if (this.touring) { this.touring = false; this.hooks.onTourEnd(); } });

    this.applyTier();
    this.ro = new ResizeObserver(() => this.resize());
    this.ro.observe(host);
    this.resize();
    const el = this.renderer.domElement;
    el.addEventListener("pointerdown", this.onDown);
    el.addEventListener("pointerup", this.onUp);
    el.addEventListener("pointermove", this.onMove);
    el.addEventListener("pointerleave", this.onLeave);
    el.addEventListener("pointercancel", this.onCancel);
    document.addEventListener("visibilitychange", this.onVisibility);
    this.loop();
  }

  // ---- lifecycle ------------------------------------------------------------------------------------------

  dispose() {
    this.disposed = true;
    cancelAnimationFrame(this.raf);
    this.ro.disconnect();
    const el = this.renderer.domElement;
    el.removeEventListener("pointerdown", this.onDown);
    el.removeEventListener("pointerup", this.onUp);
    el.removeEventListener("pointermove", this.onMove);
    el.removeEventListener("pointerleave", this.onLeave);
    el.removeEventListener("pointercancel", this.onCancel);
    document.removeEventListener("visibilitychange", this.onVisibility);
    this.controls.dispose();
    this.registry?.bind(null);
    for (const p of this.panels.values()) this.cssScene.remove(p.obj); // CSS3DObject detaches its element on removal
    this.panels.clear();
    this.css.domElement.remove();
    this.clearData();
    disposeTree(this.platform);
    disposeTree(this.fx);
    for (const g of Object.values(this.geos)) g.dispose();
    this.bodyMat.dispose();
    this.glowTex.dispose();
    this.envMap.dispose();
    this.disposeComposer();
    this.renderer.dispose();
    this.renderer.forceContextLoss(); // browsers cap live WebGL contexts
    el.remove();
  }

  invalidate() { this.dirty = true; }

  /** The label elements changed (content or set): re-place them on the next frame without redrawing the scene. */
  refreshLabels() { this.labelsDirty = true; }
  private labelsDirty = false;

  private onVisibility = () => {
    this.hidden = document.visibilityState === "hidden";
    if (!this.hidden) { this.lastFrame = 0; this.invalidate(); }
  };

  private resize() {
    const w = this.host.clientWidth || 1, h = this.host.clientHeight || 1;
    this.renderer.setSize(w, h, false);
    this.renderer.domElement.style.width = "100%";
    this.renderer.domElement.style.height = "100%";
    this.composer?.setSize(w, h);
    this.css.setSize(w, h);
    this.relayoutSlots();
    this.cssDirty = true;
    this.bloom?.setSize(w, h);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
    this.invalidate();
  }

  private applyTier() {
    const dpr = typeof window === "undefined" ? 1 : window.devicePixelRatio || 1;
    this.renderer.setPixelRatio(this.tier === "high" ? Math.min(dpr, 2) : this.tier === "med" ? Math.min(dpr, 1.5) : 1);
    this.applyFog();
    if (this.tier === "high") {
      if (!this.composer) {
        this.composer = new EffectComposer(this.renderer);
        this.composer.addPass(new RenderPass(this.scene, this.camera));
        this.bloom = new UnrealBloomPass(new THREE.Vector2(this.host.clientWidth || 1, this.host.clientHeight || 1), 0.62, 0.55, 0.62);
        this.composer.addPass(this.bloom);
        this.composer.addPass(new OutputPass());
      }
    } else if (this.composer) {
      this.disposeComposer();
    }
    this.resize();
  }

  /** Frees the bloom chain: each pass's render targets and materials, then the composer's own targets. */
  private disposeComposer() {
    if (!this.composer) return;
    for (const p of this.composer.passes) p.dispose();
    this.composer.dispose();
    this.composer = null;
    this.bloom = null;
  }

  private applyFog() {
    this.scene.fog = this.tier === "low" ? null : new THREE.Fog(this.palette.paper, this.radius * 2.2, this.radius * 6 + 60);
  }

  get quality(): Quality { return this.tier; }

  setReducedMotion(on: boolean) {
    this.reducedMotion = on;
    if (on && this.touring) { this.touring = false; this.hooks.onTourEnd(); }
    this.invalidate();
  }

  stats(): EngineStats {
    const info = this.renderer.info;
    return { fps: Math.round(this.fps), calls: info.render.calls, triangles: info.render.triangles, tier: this.tier };
  }

  // ---- the loop ---------------------------------------------------------------------------------------------

  /** Ambient pulses on screen (never with reduced motion or a hidden tab). */
  private ambient(): boolean {
    return !this.reducedMotion && !this.hidden && (this.sosFx.length > 0 || this.alertIdx.length > 0 || this.hubs.some((h) => h.hub.online));
  }

  private loop = () => {
    if (this.disposed) return;
    this.raf = requestAnimationFrame(this.loop);
    if (this.hidden) return;
    const damping = this.controls.update();
    const now = performance.now();
    const moving = !!this.tween || this.touring;
    const ambientDue = this.ambient() && now - this.lastAmbient >= (this.sosFx.length || this.alertIdx.length ? AMBIENT_MS : BREATH_MS);
    if (this.pointerDirty) { this.pointerDirty = false; this.updateHover(); }
    if (!this.dirty && !moving && !damping && !ambientDue) {
      // Label content changed (a new reading) but nothing in the scene did: place the labels, skip the WebGL frame.
      if (this.labelsDirty) { this.labelsDirty = false; this.placeLabels(); }
      this.lastFrame = 0;
      return;
    }
    if (ambientDue) this.lastAmbient = now;
    if (this.lastFrame > 0) {
      this.frameTimes.push(now - this.lastFrame);
      if (this.frameTimes.length > 300) this.frameTimes.shift();
      let sum = 0, n = 0;
      for (let i = this.frameTimes.length - 1; i >= 0 && sum < 5000; i--) { sum += this.frameTimes[i]; n++; }
      this.fps = n ? (1000 * n) / sum : 0;
      // Drop a tier when a 5 s moving average stays under 24 fps (only measurable while the camera moves).
      if (this.autoTier && sum >= 5000 && this.fps < 24 && this.tier !== "low" && (moving || damping)) {
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
    this.renderPanels();
    if (moving || damping || this.dirty) this.maybeRepickLabels(now);
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
      off.applyAxisAngle(new THREE.Vector3(0, 1, 0), 0.0018);
      this.camera.position.copy(t).add(off);
    }
    const t = now / 1000, still = this.reducedMotion;
    const m = new THREE.Matrix4(), q = new THREE.Quaternion().setFromEuler(new THREE.Euler(-Math.PI / 2, 0, 0)), s = new THREE.Vector3(), p = new THREE.Vector3();
    // Online hubs breathe; offline ones stay still.
    if (this.hubRings) {
      this.hubs.forEach((h, i) => {
        const k = h.hub.online && !still ? 1 + 0.08 * Math.sin(t * 1.6 + i * 0.7) : 1;
        p.set(h.at[0], 0.06, h.at[2]);
        s.set(k, k, k);
        this.hubRings!.setMatrixAt(i, m.compose(p, q, s));
      });
      this.hubRings.instanceMatrix.needsUpdate = true;
    }
    if (this.alertRings && this.alertIdx.length) {
      const k = still ? 1.5 : 1 + ((t * 0.9) % 1) * 1.4;
      (this.alertRings.material as THREE.MeshBasicMaterial).opacity = still ? 0.6 : 0.85 * (1 - ((t * 0.9) % 1));
      this.alertIdx.forEach((ni, i) => {
        const at = this.nodes[ni].at;
        p.set(at[0], at[1], at[2]);
        s.set(k, k, k);
        this.alertRings!.setMatrixAt(i, m.compose(p, q, s));
      });
      this.alertRings.instanceMatrix.needsUpdate = true;
    }
    for (const fx of this.sosFx) {
      const ringMat = fx.ring.material as THREE.MeshBasicMaterial, pillarMat = fx.pillar.material as THREE.MeshBasicMaterial;
      if (still) { fx.ring.scale.setScalar(3); ringMat.opacity = 0.55; pillarMat.opacity = 0.28; continue; }
      const k = (t * 0.55 + fx.phase) % 1;
      fx.ring.scale.setScalar(1 + k * 9);
      ringMat.opacity = 0.8 * (1 - k);
      pillarMat.opacity = 0.22 + 0.1 * Math.sin(t * 4 + fx.phase * 6);
    }
    if (this.hovered) {
      const r = this.refs.get(this.hovered);
      if (r) {
        this.hoverRing.position.set(r.place.at[0], r.place.at[1] - 0.02, r.place.at[2]);
        this.hoverRing.rotation.z = still ? 0 : t * 0.8;
      }
    }
  }

  // ---- labels (React renders them; this positions them every frame) ------------------------------------------

  private maybeRepickLabels(now: number) {
    if (now - this.lastLabelPick < 350) return;
    this.lastLabelPick = now;
    this.repickLabels();
  }

  private repickLabels() {
    const cam = this.camera.position;
    // The label holding keyboard focus stays in the set, so moving the camera never drops focus.
    const focused = typeof document !== "undefined" ? document.activeElement : null;
    const keep = focused ? this.labelKeys.find((k) => this.hooks.labelEl(k)?.contains(focused)) : undefined;
    const keys = pickLabels(this.nodes.map((n) => ({ key: n.card.key, tier: labelTier(n.card, this.hooks.controllable), dist: Math.hypot(n.at[0] - cam.x, n.at[1] - cam.y, n.at[2] - cam.z) })), MAX_LABELS, keep);
    const same = keys.length === this.labelKeys.length && keys.every((k, i) => k === this.labelKeys[i]);
    if (same) return;
    this.labelKeys = keys;
    this.hooks.onLabels(keys);
  }

  /** Projects each label to the screen; lower-priority labels that would overlap a placed one are hidden. */
  private placeLabels() {
    const w = this.host.clientWidth, h = this.host.clientHeight, v = new THREE.Vector3();
    const focused = typeof document !== "undefined" ? document.activeElement : null;
    // Device labels sit above their node; hub chips hang just below the hub's base, out of the devices' way.
    type Item = { el: HTMLElement; x: number; y: number; z: number; tier: number; below: boolean };
    const items: Item[] = [], hidden: HTMLElement[] = [];
    const project = (el: HTMLElement | undefined, at: [number, number, number], lift: number, tier: number, below = false) => {
      if (!el) return;
      v.set(at[0], at[1] + lift, at[2]).project(this.camera);
      if (v.z > 1 || v.x < -1.05 || v.x > 1.05 || v.y < -1.05 || v.y > 1.05) { hidden.push(el); return; }
      items.push({ el, x: ((v.x + 1) / 2) * w, y: ((1 - v.y) / 2) * h, z: v.z, tier, below });
    };
    for (const key of this.labelKeys) {
      const r = this.refs.get(key);
      if (r) project(this.hooks.labelEl(key), r.place.at, 0.55, labelTier(r.place.card, this.hooks.controllable));
    }
    for (const hp of this.hubs) project(this.hooks.hubLabelEl(hp.hub.id), hp.at, 0, 1.5, true);
    items.sort((a, b) => a.tier - b.tier || a.z - b.z);
    // Read every size first, then write: one layout per frame, not one per label.
    const sizes = items.map((it) => [it.el.offsetWidth, it.el.offsetHeight] as const);
    const placed: [number, number, number, number][] = [];
    items.forEach((it, i) => {
      const [lw, lh] = sizes[i];
      const top = it.below ? it.y + 6 : it.y - lh;
      const box: [number, number, number, number] = [it.x - lw / 2, top, it.x + lw / 2, top + lh];
      const clash = placed.some((b) => box[0] < b[2] && box[2] > b[0] && box[1] < b[3] && box[3] > b[1]);
      // SOS always shows; a label holding keyboard focus never disappears under the operator.
      if (clash && it.tier > 0 && !it.el.contains(focused)) { hidden.push(it.el); return; }
      placed.push(box);
      it.el.style.transform = `translate(-50%, ${it.below ? "6px" : "-100%"}) translate(${it.x.toFixed(1)}px, ${it.y.toFixed(1)}px)`;
      it.el.style.zIndex = String(1000 - Math.round(it.tier * 100) - Math.round(it.z * 50));
      if (it.el.style.visibility !== "visible") it.el.style.visibility = "visible";
    });
    for (const el of hidden) if (el.style.visibility !== "hidden" && !el.contains(focused)) el.style.visibility = "hidden";
  }

  // ---- data -----------------------------------------------------------------------------------------------------

  private clearData() {
    for (const g of [this.hubGroup, this.nodeGroup]) {
      for (const o of [...g.children]) {
        g.remove(o);
        if (o instanceof THREE.InstancedMesh) o.dispose();
      }
    }
    for (const m of this.shared) m.dispose();
    for (const g of this.sharedGeo) g.dispose();
    this.shared = [];
    this.sharedGeo = [];
    for (const s of this.sosFx) { this.fx.remove(s.pillar, s.ring); disposeTree(s.pillar); disposeTree(s.ring); }
    this.sosFx = [];
    this.bodies.clear();
    this.pickNodes = this.pickHubs = this.hubRings = this.alertRings = null;
    this.alertIdx = [];
  }

  /** Registers a per-data geometry or material, freed on the next setData. */
  private own<T extends THREE.Material | THREE.BufferGeometry>(x: T): T {
    if (x instanceof THREE.Material) this.shared.push(x);
    else this.sharedGeo.push(x as THREE.BufferGeometry);
    return x;
  }

  /** Rebuilds the constellation from a layout (cheap: a handful of instanced meshes). The camera stays where it is. */
  setData(layout: { hubs: HubPlace[]; nodes: NodePlace[]; radius: number }) {
    this.layout = layout;
    this.clearData();
    const P = this.palette, low = this.tier === "low";
    this.hubs = layout.hubs;
    this.nodes = layout.nodes;
    const radiusChanged = Math.abs(layout.radius - this.radius) > 0.5;
    this.radius = layout.radius;
    if (radiusChanged || this.platform.children.length === 0) this.buildPlatform();
    this.applyFog();

    // ---- hubs: project tint pool, pedestal, tower, beacon, breathing ring, orbit tracks.
    const hubColor = (hp: HubPlace) => (hp.hub.color ? new THREE.Color(hp.hub.color) : P.accent.clone());
    const dimmed = (c: THREE.Color, online: boolean) => (online ? c : c.clone().lerp(P.dim, 0.75).multiplyScalar(0.55));
    const n = Math.max(1, this.hubs.length);
    const pool = new THREE.InstancedMesh(this.own(new THREE.PlaneGeometry(2, 2)), this.own(new THREE.MeshBasicMaterial({ map: this.glowTex, transparent: true, depthWrite: false, blending: THREE.AdditiveBlending, opacity: 0.22 })), n);
    const pedestal = new THREE.InstancedMesh(this.own(new THREE.CylinderGeometry(0.95, 1.15, 0.24, low ? 16 : 40)), this.own(new THREE.MeshStandardMaterial({ color: P.panel, metalness: 0.9, roughness: 0.22 })), n);
    const tower = new THREE.InstancedMesh(this.own(new THREE.CylinderGeometry(0.12, 0.3, 2.7, low ? 8 : 20)), this.own(glowingStandard(0.35, { metalness: 0.6, roughness: 0.22 })), n);
    const beacon = new THREE.InstancedMesh(this.own(new THREE.SphereGeometry(0.3, low ? 10 : 20, low ? 8 : 14)), this.own(new THREE.MeshBasicMaterial({ color: 0xffffff })), n);
    this.hubRings = new THREE.InstancedMesh(this.own(new THREE.RingGeometry(1.35, 1.48, low ? 32 : 72)), this.own(new THREE.MeshBasicMaterial({ transparent: true, opacity: 0.75, side: THREE.DoubleSide, depthWrite: false, blending: THREE.AdditiveBlending })), n);
    this.pickHubs = new THREE.InstancedMesh(this.own(new THREE.CylinderGeometry(1.3, 1.3, 3.4, 8)), this.own(new THREE.MeshBasicMaterial({ visible: false })), n);
    const m = new THREE.Matrix4(), flat = new THREE.Quaternion().setFromEuler(new THREE.Euler(-Math.PI / 2, 0, 0)), one = new THREE.Quaternion(), sc = new THREE.Vector3(), at = new THREE.Vector3();
    const tracks: number[] = [], trackColors: number[] = [];
    this.hubs.forEach((hp, i) => {
      const c = hubColor(hp), lit = dimmed(c, hp.hub.online);
      at.set(hp.at[0], 0.015, hp.at[2]);
      pool.setMatrixAt(i, m.compose(at, flat, sc.setScalar(hp.radius * 1.15)));
      pool.setColorAt(i, hp.hub.color ? c : P.accent.clone().multiplyScalar(0.6));
      pedestal.setMatrixAt(i, m.compose(at.set(hp.at[0], 0.12, hp.at[2]), one, sc.setScalar(1)));
      pedestal.setColorAt(i, P.panel);
      tower.setMatrixAt(i, m.compose(at.set(hp.at[0], 1.6, hp.at[2]), one, sc.setScalar(1)));
      tower.setColorAt(i, lit);
      beacon.setMatrixAt(i, m.compose(at.set(hp.at[0], 3.1, hp.at[2]), one, sc.setScalar(hp.hub.online ? 1 : 0.7)));
      beacon.setColorAt(i, hp.hub.online ? c.clone().multiplyScalar(1.25) : lit);
      this.hubRings!.setMatrixAt(i, m.compose(at.set(hp.at[0], 0.06, hp.at[2]), flat, sc.setScalar(1)));
      this.hubRings!.setColorAt(i, lit);
      this.pickHubs!.setMatrixAt(i, m.compose(at.set(hp.at[0], 1.7, hp.at[2]), one, sc.setScalar(1)));
      // Orbit tracks: one faint circle per ring of devices.
      const rings = new Set(this.nodes.filter((x) => x.hub === hp.hub.id).map((x) => x.ring));
      for (const ring of rings) {
        const r = 3.2 + ring * 1.7, y = 1.1 + ring * 0.38 - 0.5, segs = low ? 40 : 96;
        const tc = (hp.hub.online ? c : lit).clone().multiplyScalar(0.32);
        for (let k = 0; k < segs; k++) {
          const a0 = (k / segs) * Math.PI * 2, a1 = ((k + 1) / segs) * Math.PI * 2;
          tracks.push(hp.at[0] + Math.cos(a0) * r, y, hp.at[2] + Math.sin(a0) * r, hp.at[0] + Math.cos(a1) * r, y, hp.at[2] + Math.sin(a1) * r);
          trackColors.push(tc.r, tc.g, tc.b, tc.r, tc.g, tc.b);
        }
      }
    });
    for (const im of [pool, pedestal, tower, beacon, this.hubRings, this.pickHubs]) { im.count = this.hubs.length; finish(im); this.hubGroup.add(im); }
    this.pickHubs.userData.pick = "hub";

    // ---- nodes: one instanced mesh per shape, glow pools, links, rings.
    this.refs.clear();
    const hubAt = new Map(this.hubs.map((h) => [h.hub.id, h.at]));
    const byShape = new Map<Shape, NodePlace[]>();
    for (const np of this.nodes) { const s = shapeOf(np.card.kind); byShape.set(s, [...(byShape.get(s) ?? []), np]); }
    const statusColor = (np: NodePlace): THREE.Color => {
      const c = np.card;
      if (c.sos || c.status === "alert") return P.danger;
      if (c.status === "offline") return P.dim.clone().multiplyScalar(0.55);
      if (c.status === "stale") return P.warning;
      if (c.door?.leftOpen) return P.warning;
      return P.accent;
    };
    const color = new THREE.Color(), rot = new THREE.Quaternion(), yAxis = new THREE.Vector3(0, 1, 0);
    for (const [shape, list] of byShape) {
      const im = new THREE.InstancedMesh(this.geos[shape], this.bodyMat, list.length);
      list.forEach((np, i) => {
        this.refs.set(np.card.key, { place: np, shape, index: i });
        const on = poweredOn(np.card) === true && np.card.status !== "offline";
        const centre = hubAt.get(np.hub) ?? [0, 0, 0];
        const toward = Math.atan2(np.at[0] - centre[0], np.at[2] - centre[2]);
        // A door slab faces its hub; an open door swings 70° on its hinge.
        rot.setFromAxisAngle(yAxis, toward + (shape === "slab" && np.card.door?.open ? 1.22 : 0));
        const scale = np.card.sos ? 1.5 : np.card.status === "alert" ? 1.25 : on ? 1.18 : np.card.occupancy?.occupied ? 1.12 : 1;
        im.setMatrixAt(i, m.compose(at.set(...np.at), rot, sc.setScalar(scale)));
        color.copy(statusColor(np));
        if (on) color.multiplyScalar(1.35);
        im.setColorAt(i, color);
      });
      finish(im);
      this.nodeGroup.add(im);
      this.bodies.set(shape, im);
    }
    const count = Math.max(1, this.nodes.length);
    const glow = new THREE.InstancedMesh(this.own(new THREE.PlaneGeometry(2, 2)), this.own(new THREE.MeshBasicMaterial({ map: this.glowTex, transparent: true, depthWrite: false, blending: THREE.AdditiveBlending, opacity: 0.5 })), count);
    this.pickNodes = new THREE.InstancedMesh(this.own(new THREE.SphereGeometry(0.72, 8, 6)), this.own(new THREE.MeshBasicMaterial({ visible: false })), count);
    this.pickNodes.userData.pick = "node";
    const halos: { at: [number, number, number]; color: THREE.Color }[] = [];
    const links: number[] = [], linkColors: number[] = [];
    this.nodes.forEach((np, i) => {
      const c = np.card, on = poweredOn(c) === true && c.status !== "offline";
      const base = statusColor(np);
      glow.setMatrixAt(i, m.compose(at.set(np.at[0], 0.03, np.at[2]), flat, sc.setScalar(c.sos ? 2.4 : c.status === "alert" ? 1.6 : on ? 1.5 : c.status === "offline" ? 0.6 : 1)));
      glow.setColorAt(i, color.copy(base).multiplyScalar(c.status === "offline" ? 0.4 : on ? 1.1 : 0.75));
      this.pickNodes!.setMatrixAt(i, m.compose(at.set(...np.at), one, sc.setScalar(1)));
      // Occupied rooms wear an accent halo; a door left open wears an amber one.
      if (c.status !== "offline" && c.occupancy?.occupied) halos.push({ at: np.at, color: P.accent });
      if (c.status !== "offline" && c.door?.leftOpen) halos.push({ at: np.at, color: P.warning });
      const hub = hubAt.get(np.hub);
      if (hub) {
        const lc = base.clone().multiplyScalar(c.status === "offline" ? 0.18 : 0.42);
        links.push(hub[0], 2.2, hub[2], np.at[0], np.at[1], np.at[2]);
        linkColors.push(lc.r * 0.6, lc.g * 0.6, lc.b * 0.6, lc.r, lc.g, lc.b);
        // A faint stem down to the platform anchors each node in depth.
        links.push(np.at[0], np.at[1] - 0.35, np.at[2], np.at[0], 0.04, np.at[2]);
        linkColors.push(lc.r * 0.5, lc.g * 0.5, lc.b * 0.5, 0, 0, 0);
      }
    });
    glow.count = this.pickNodes.count = this.nodes.length;
    finish(glow);
    finish(this.pickNodes);
    this.nodeGroup.add(glow, this.pickNodes);
    if (halos.length) {
      const halo = new THREE.InstancedMesh(this.own(new THREE.TorusGeometry(0.66, 0.045, 8, low ? 24 : 48)), this.own(new THREE.MeshBasicMaterial({ color: 0xffffff })), halos.length);
      halos.forEach((hl, i) => { halo.setMatrixAt(i, m.compose(at.set(...hl.at), flat, sc.setScalar(1))); halo.setColorAt(i, hl.color); });
      finish(halo);
      this.nodeGroup.add(halo);
    }
    this.nodeGroup.add(this.lines(links, linkColors, 0.75), this.lines(tracks, trackColors, 1));

    // Alerts: a pulsing coral ring each. SOS: a tall light pillar and an expanding ring on the platform.
    this.alertIdx = this.nodes.map((np, i) => (np.card.status === "alert" || np.card.sos ? i : -1)).filter((i) => i >= 0);
    if (this.alertIdx.length) {
      this.alertRings = new THREE.InstancedMesh(this.own(new THREE.RingGeometry(0.58, 0.7, 40)), this.own(new THREE.MeshBasicMaterial({ color: P.danger, transparent: true, opacity: 0.8, side: THREE.DoubleSide, depthWrite: false, blending: THREE.AdditiveBlending })), this.alertIdx.length);
      this.nodeGroup.add(this.alertRings);
    }
    this.nodes.filter((np) => np.card.sos).forEach((np, i) => {
      const pillar = new THREE.Mesh(new THREE.CylinderGeometry(0.35, 0.75, 16, 24, 1, true), new THREE.MeshBasicMaterial({ color: P.danger, transparent: true, opacity: 0.4, blending: THREE.AdditiveBlending, depthWrite: false, side: THREE.DoubleSide }));
      pillar.position.set(np.at[0], 8, np.at[2]);
      const ring = new THREE.Mesh(new THREE.RingGeometry(0.8, 1.0, 64), new THREE.MeshBasicMaterial({ color: P.danger, transparent: true, opacity: 0.7, side: THREE.DoubleSide, depthWrite: false, blending: THREE.AdditiveBlending }));
      ring.rotation.x = -Math.PI / 2;
      ring.position.set(np.at[0], 0.05, np.at[2]);
      this.fx.add(pillar, ring);
      this.sosFx.push({ pillar, ring, phase: i * 0.29, key: np.card.key });
    });

    if (this.hovered && !this.refs.has(this.hovered)) this.setHovered(null);
    this.step(performance.now());
    if (!this.framed && this.nodes.length + this.hubs.length > 0) { this.framed = true; this.intro(); }
    for (const [key, p] of this.panels) if ("above" in p.spec.place || "slot" in p.spec.place) this.placePanel(key, p.spec, false);
    this.lastLabelPick = 0;
    this.repickLabels();
    this.invalidate();
  }

  private lines(positions: number[], colors: number[], opacity: number): THREE.LineSegments {
    const geo = this.own(new THREE.BufferGeometry());
    geo.setAttribute("position", new THREE.Float32BufferAttribute(positions, 3));
    geo.setAttribute("color", new THREE.Float32BufferAttribute(colors, 3));
    return new THREE.LineSegments(geo, this.own(new THREE.LineBasicMaterial({ vertexColors: true, transparent: true, opacity, blending: THREE.AdditiveBlending, depthWrite: false })));
  }

  private buildPlatform() {
    disposeTree(this.platform);
    this.platform.clear();
    const R = this.radius * 1.18 + 6, P = this.palette;
    const disc = new THREE.Mesh(new THREE.CircleGeometry(R, 128), new THREE.MeshStandardMaterial({ color: P.paper.clone().lerp(P.panel, 0.5), metalness: 0.92, roughness: 0.3, envMapIntensity: 0.9 }));
    disc.rotation.x = -Math.PI / 2;
    const grid = new THREE.Mesh(new THREE.CircleGeometry(R, 128), gridMaterial(P.grid, P.accent, R));
    grid.rotation.x = -Math.PI / 2;
    grid.position.y = 0.01;
    grid.renderOrder = 1;
    // A soft under-glow so the platform reads as lit from within.
    const under = new THREE.Mesh(new THREE.PlaneGeometry(R * 2.4, R * 2.4), new THREE.MeshBasicMaterial({ map: this.glowTex, color: P.accent.clone().multiplyScalar(0.12), transparent: true, depthWrite: false, blending: THREE.AdditiveBlending }));
    under.rotation.x = -Math.PI / 2;
    under.position.y = -0.4;
    this.platform.add(under, disc, grid);
    // The glow texture is shared; disposeTree must not free it with the platform.
    (under.material as THREE.MeshBasicMaterial).userData.keepMap = true;
  }

  /** Re-ranks labels (the controllable rule changed, e.g. a Tuya device reported its switch). */
  relabel() { this.lastLabelPick = 0; this.repickLabels(); this.invalidate(); }

  /** New theme colours: clear colour, background, fog, lights, platform and every node and hub are redrawn. */
  setPalette(palette: Palette) {
    this.palette = palette;
    this.renderer.setClearColor(palette.paper, 1);
    this.scene.background = palette.paper.clone();
    this.hemi.color.copy(palette.ink);
    this.hemi.groundColor.copy(palette.paper);
    (this.hoverRing.material as THREE.MeshBasicMaterial).color.copy(palette.ink);
    this.applyFog();
    disposeTree(this.platform);
    this.platform.clear();
    if (this.layout) this.setData(this.layout);
    else this.invalidate();
  }

  // ---- in-scene panels (CSS3D) ------------------------------------------------------------------------------

  bindPanels(registry: PanelRegistry) {
    this.registry = registry;
    registry.bind((key, spec) => {
      const wasSlot = !!this.panels.get(key) && "slot" in this.panels.get(key)!.spec.place;
      if (spec) this.placePanel(key, spec, true);
      else {
        const p = this.panels.get(key);
        if (p) { this.cssScene.remove(p.obj); this.panels.delete(key); }
      }
      // The slot layout depends on how many KPI tiles there are: a tile coming or going re-places the wall.
      if (wasSlot || (spec && "slot" in spec.place)) this.relayoutSlots();
      this.cssDirty = true;
      this.invalidate();
    });
  }

  /** Creates or moves a panel. A newly placed detail panel flies the camera to frame it with its device. */
  private placePanel(key: string, spec: PanelSpec, fresh: boolean) {
    let p = this.panels.get(key);
    if (p && p.obj.element !== spec.el) { this.cssScene.remove(p.obj); p = undefined; }
    const keepBeside = !!p && "beside" in spec.place && "beside" in p.spec.place && p.spec.place.beside === spec.place.beside;
    const resolved = keepBeside ? { at: p!.at, width: spec.width } : this.resolve(spec);
    const at = resolved.at;
    if (!p) {
      const obj = new CSS3DObject(spec.el);
      this.cssScene.add(obj);
      p = { obj, spec, at, width: spec.width };
      this.panels.set(key, p);
    }
    p.spec = spec;
    p.at.copy(at);
    p.obj.position.copy(at).multiplyScalar(CSS_K);
    p.width = resolved.width;
    p.obj.scale.setScalar((resolved.width * CSS_K) / PANEL_PX[spec.kind]);
    if (spec.kind === "detail") this.fitDetail(spec);
    if (fresh && !keepBeside && "beside" in spec.place) {
      const node = this.refs.get(spec.place.beside)?.place.at;
      const mid = node ? new THREE.Vector3(...node).lerp(at, 0.5) : at.clone();
      this.focusPoint([mid.x, mid.y, mid.z], spec.width * 1.7 + 6, true);
    }
    this.cssDirty = true;
  }

  /** World position of a panel's centre, and its width in world units. */
  private resolve(spec: PanelSpec): { at: THREE.Vector3; width: number } {
    const place = spec.place;
    if ("slot" in place) return this.resolveSlot(spec, place.slot);
    return { at: this.resolvePoint(spec), width: spec.width };
  }

  private resolvePoint(spec: PanelSpec): THREE.Vector3 {
    const place = spec.place as Exclude<PanelPlace, { slot: string }>;
    if ("at" in place) return new THREE.Vector3(...place.at);
    const key = "beside" in place ? place.beside : place.above;
    const node = this.refs.get(key)?.place.at;
    const fwd = new THREE.Vector3();
    this.camera.getWorldDirection(fwd);
    if (!node) {
      // The device is filtered out of the scene: the panel opens in front of the camera.
      return this.camera.position.clone().add(fwd.multiplyScalar(spec.width * 1.6 + 4));
    }
    const at = new THREE.Vector3(...node);
    if ("above" in place) return at.add(new THREE.Vector3(0, 5 + spec.width * 0.18, 0));
    const right = new THREE.Vector3().crossVectors(fwd, new THREE.Vector3(0, 1, 0)).normalize();
    return at.add(right.multiplyScalar(spec.width / 2 + 2.2)).add(new THREE.Vector3(0, 1.5, 0));
  }

  /** The default view's screen layout for the current stage size (pure: layout.ts wallSlots). */
  private slots() {
    const W = this.host.clientWidth || 1, H = this.host.clientHeight || 1;
    const kpis = [...this.panels.values()].filter((p) => "slot" in p.spec.place && p.spec.place.slot.startsWith("kpi-")).length || 6;
    return { W, H, ...wallSlots(W, H, kpis, this.safeTop + HUD_ROW, H - BOTTOM_ROW - this.safeBottom) };
  }

  /**
   * A wall slot: where a panel must sit in the world to land exactly on its screen rect in the default view, at a
   * depth behind (KPIs) or before (lists) the target. Panels are screen-aligned, so from that view none overlap;
   * from any other angle they move like the rest of the scene.
   */
  private resolveSlot(spec: PanelSpec, slot: string): { at: THREE.Vector3; width: number } {
    const sl = this.slots(), pose = this.defaultPose();
    const rect: ScreenRect | undefined = slot.startsWith("kpi-") ? sl.kpi[Number(slot.slice(4))] : sl.lists[slot as keyof typeof sl.lists];
    if (!rect) return { at: pose.target.clone(), width: spec.width };
    const cam = new THREE.PerspectiveCamera(this.camera.fov, sl.W / sl.H, 0.1, 4000);
    cam.position.copy(pose.pos);
    cam.lookAt(pose.target);
    cam.updateMatrixWorld();
    const tan = Math.tan((cam.fov * Math.PI) / 360), z = pose.pos.distanceTo(pose.target) * (SLOT_DEPTH[spec.kind] ?? 1);
    const xn = (rect.x / sl.W) * 2 - 1, yn = 1 - (rect.y / sl.H) * 2;
    const at = new THREE.Vector3(xn * z * tan * cam.aspect, yn * z * tan, -z).applyMatrix4(cam.matrixWorld);
    const pxPerWorld = sl.H / 2 / (z * tan);
    // A list fills its slot's height and scrolls inside; a KPI tile has a fixed height that matches its slot.
    if (spec.kind === "list") spec.el.style.height = `${Math.round(rect.h * (LIST_PX_W / rect.w))}px`;
    return { at, width: rect.w / pxPerWorld };
  }

  /** Where the overview preset puts the camera: the platform fits the gap between the list columns. */
  private defaultPose(): { pos: THREE.Vector3; target: THREE.Vector3 } {
    const sl = this.slots(), tan = Math.tan((this.camera.fov * Math.PI) / 360), aspect = sl.W / sl.H;
    const rp = this.radius * 1.18 + 6;
    const band = Math.max(0.35, (sl.H - (sl.kpi[0] ? sl.kpi[0].y + sl.kpi[0].h / 2 : 0)) / sl.H);
    const d = Math.max(rp / (sl.centre * tan * aspect), (rp * 0.7 + 3) / (band * tan));
    return { pos: new THREE.Vector3(0, 0.62, 0.78).normalize().multiplyScalar(d), target: new THREE.Vector3() };
  }

  /** Re-places every wall-slot panel (stage resized, constellation changed, HUD moved). */
  private relayoutSlots() {
    for (const [key, p] of this.panels) if ("slot" in p.spec.place || p.spec.kind === "detail") this.placePanel(key, p.spec, false);
  }

  /**
   * Pixels of the stage the viewer cannot see: under the app shell's fixed bars or scrolled off at the top, and
   * below the window's bottom edge. The HUD, the KPI row and the lists stay inside what is visible.
   */
  setSafeArea(top: number, bottom: number) {
    if (Math.abs(top - this.safeTop) < 1 && Math.abs(bottom - this.safeBottom) < 1) return;
    this.safeTop = top;
    this.safeBottom = bottom;
    this.relayoutSlots();
    this.invalidate();
  }

  /** The detail scrolls inside: at its framed distance it is at most ~70% of the stage height. */
  private fitDetail(spec: PanelSpec) {
    const H = this.host.clientHeight || 1, tan = Math.tan((this.camera.fov * Math.PI) / 360), dist = spec.width * 1.7 + 6;
    const screenW = spec.width * (H / 2 / (dist * tan));
    spec.el.style.maxHeight = `${Math.round((0.7 * H) / (screenW / PANEL_PX.detail))}px`;
  }

  /**
   * Re-renders the CSS3D layer only when the camera moved or a panel changed (not on ambient pulses). Panels face
   * the camera and fade with distance. A panel steps aside (fade, no pointer) when it would cover the middle of the
   * view in front of what the camera looks at, and drops to 0.06 wherever it overlaps an open device detail.
   */
  private renderPanels() {
    if (!this.cssHost) return;
    this.camera.updateMatrixWorld();
    const moved = !this.lastCam.equals(this.camera.matrixWorld) || !this.lastProj.equals(this.camera.projectionMatrix);
    if (!moved && !this.cssDirty) return;
    this.lastCam.copy(this.camera.matrixWorld);
    this.lastProj.copy(this.camera.projectionMatrix);
    this.cssDirty = false;
    const cam = this.camera.position, targetDist = cam.distanceTo(this.controls.target), homeDist = this.defaultPose().pos.length(), v = new THREE.Vector3();
    const W = this.host.clientWidth || 1, H = this.host.clientHeight || 1, tan = Math.tan((this.camera.fov * Math.PI) / 360);
    /** A screen-aligned panel's rect on screen (px), or null when it is behind the camera. */
    const rectOf = (p: { at: THREE.Vector3; width: number; spec: PanelSpec; obj: CSS3DObject }): ScreenRect | null => {
      v.copy(p.at).applyMatrix4(this.camera.matrixWorldInverse);
      const z = -v.z;
      if (z <= 0.01) return null;
      const ppw = H / 2 / (z * tan), w = p.width * ppw, ratio = w / PANEL_PX[p.spec.kind];
      v.copy(p.at).project(this.camera);
      return { x: ((v.x + 1) / 2) * W, y: ((1 - v.y) / 2) * H, w, h: (p.obj.element.offsetHeight || PANEL_PX[p.spec.kind] * 0.6) * ratio };
    };
    // An open device detail wins: every other panel that overlaps it on screen steps right back.
    let detailRect: ScreenRect | null = null;
    for (const p of this.panels.values()) if (p.spec.kind === "detail") detailRect = rectOf(p);
    for (const p of this.panels.values()) {
      // Screen-aligned: a panel always faces the viewer squarely, so its text stays readable from any orbit angle.
      p.obj.quaternion.copy(this.camera.quaternion);
      v.copy(p.at).project(this.camera);
      const el = p.obj.element, behind = v.z > 1;
      if (el.style.display !== (behind ? "none" : "")) el.style.display = behind ? "none" : "";
      if (behind) continue;
      if (p.spec.kind === "detail") { el.style.opacity = "1"; el.style.pointerEvents = "auto"; continue; }
      const d = cam.distanceTo(p.at), r = detailRect ? rectOf(p) : null;
      const underDetail = !!(detailRect && r && Math.abs(r.x - detailRect.x) * 2 < r.w + detailRect.w && Math.abs(r.y - detailRect.y) * 2 < r.h + detailRect.h);
      const blocking = p.spec.kind !== "sos" && ((d < targetDist * 0.85 && Math.abs(v.x) < 0.45 && Math.abs(v.y) < 0.45) || d < targetDist * 0.55);
      const opacity = underDetail ? 0.06 : p.spec.kind === "sos" ? 1 : blocking ? 0.12 : Math.max(0.5, Math.min(1, 1 - (d - homeDist * 1.3) / (homeDist * 1.5)));
      el.style.opacity = opacity.toFixed(2);
      el.style.pointerEvents = underDetail || blocking ? "none" : "auto";
    }
    this.cssCamera.copy(this.camera);
    this.cssCamera.position.multiplyScalar(CSS_K);
    this.cssCamera.updateMatrixWorld();
    this.css.render(this.cssScene, this.cssCamera);
  }

  /** The first view: a short fly-in from high above (a cut with reduced motion). */
  private intro() {
    if (this.reducedMotion) { this.preset("overview", 0); return; }
    const d = this.defaultPose().pos.length();
    this.camera.position.set(d * 0.3, d * 2.2, d * 1.4);
    this.controls.target.set(0, 0, 0);
    this.preset("overview", 2600);
  }

  // ---- camera ------------------------------------------------------------------------------------------------



  /** Moves the camera to a preset; "alert" goes to the first SOS (else the first alert) and reports whether one exists. */
  preset(p: Preset, ms = 1400): boolean {
    this.touring = false;
    // Top: the whole platform inside the frame's shorter side.
    if (p === "top") { this.flyTo(new THREE.Vector3(0, ((this.radius * 1.18 + 6) / Math.tan((this.camera.fov * Math.PI) / 360)) / Math.min(1, this.camera.aspect) * 1.02, 0.01), new THREE.Vector3(0, 0, 0), ms); return true; }
    if (p === "alert") {
      const np = this.nodes.find((x) => x.card.sos) ?? this.nodes.find((x) => x.card.status === "alert");
      if (!np) return false;
      // An SOS has its banner above it: frame both.
      if (np.card.sos) this.focusPoint([np.at[0], np.at[1] + 4, np.at[2]], 26);
      else this.focusPoint(np.at, 16);
      return true;
    }
    const pose = this.defaultPose();
    this.flyTo(pose.pos, pose.target, ms);
    return true;
  }

  focusHub(id: string) {
    const hp = this.hubs.find((h) => h.hub.id === id);
    if (hp) this.focusPoint([hp.at[0], 1, hp.at[2]], Math.max(10, hp.radius * 2.6));
  }

  focusNode(key: string) {
    const r = this.refs.get(key);
    if (r) this.focusPoint(r.place.at, 16);
  }

  /** Flies to a point, approaching from the platform's outside edge (keeps the constellation behind it). */
  private focusPoint(at: [number, number, number], dist: number, keepHeading = false) {
    this.touring = false;
    const target = new THREE.Vector3(...at);
    // Normally from the platform's outside edge; a detail panel keeps the current heading, so it stays beside its device.
    const out = keepHeading ? this.camera.position.clone().sub(this.controls.target).setY(0) : new THREE.Vector3(at[0], 0, at[2]);
    if (out.lengthSq() < 1) out.copy(this.camera.position).setY(0);
    out.normalize();
    const pos = target.clone().add(out.multiplyScalar(dist * 0.75)).add(new THREE.Vector3(0, dist * 0.7, 0));
    this.flyTo(pos, target, 1300);
  }

  setTour(on: boolean) {
    this.touring = on && !this.reducedMotion;
    if (this.touring) this.tween = null;
    this.invalidate();
    return this.touring;
  }

  private flyTo(pos: THREE.Vector3, target: THREE.Vector3, ms: number) {
    if (this.reducedMotion || ms <= 0) {
      this.tween = null;
      this.camera.position.copy(pos);
      this.controls.target.copy(target);
    } else {
      this.tween = { fromPos: this.camera.position.clone(), toPos: pos, fromTarget: this.controls.target.clone(), toTarget: target, start: performance.now(), ms };
    }
    this.invalidate();
  }

  // ---- picking --------------------------------------------------------------------------------------------------

  private cast(cx: number, cy: number): Pick {
    const r = this.renderer.domElement.getBoundingClientRect();
    this.raycaster.setFromCamera(new THREE.Vector2(((cx - r.left) / r.width) * 2 - 1, -((cy - r.top) / r.height) * 2 + 1), this.camera);
    const targets = [this.pickNodes, this.pickHubs].filter((x): x is THREE.InstancedMesh => !!x && x.count > 0);
    const hit = this.raycaster.intersectObjects(targets, false)[0];
    if (!hit || hit.instanceId === undefined) return null;
    if (hit.object.userData.pick === "node") { const np = this.nodes[hit.instanceId]; return np ? { kind: "node", key: np.card.key } : null; }
    const hp = this.hubs[hit.instanceId];
    return hp ? { kind: "hub", id: hp.hub.id } : null;
  }

  private onDown = (e: PointerEvent) => { this.downAt = [e.clientX, e.clientY]; };

  private onUp = (e: PointerEvent) => {
    // Cleared first: after a drag (orbit) hover must come back, and a drag is never a click.
    const down = this.downAt;
    this.downAt = null;
    if (!down || Math.hypot(e.clientX - down[0], e.clientY - down[1]) > 4) return;
    const p = this.cast(e.clientX, e.clientY);
    if (p?.kind === "hub") this.focusHub(p.id);
    this.hooks.onPick(p);
  };

  private onMove = (e: PointerEvent) => {
    if (e.pointerType === "touch") return;
    const r = this.host.getBoundingClientRect();
    this.pointer = { x: e.clientX - r.left, y: e.clientY - r.top, cx: e.clientX, cy: e.clientY };
    this.pointerDirty = true;
    const tip = this.hooks.tooltipEl();
    if (tip) tip.style.transform = `translate(${Math.round(this.pointer.x + 14)}px, ${Math.round(this.pointer.y + 16)}px)`;
  };

  private onCancel = () => { this.downAt = null; };

  private onLeave = () => { this.pointer = null; this.setHovered(null); this.renderer.domElement.style.cursor = ""; };

  private updateHover() {
    if (!this.pointer || this.downAt) return;
    const p = this.cast(this.pointer.cx, this.pointer.cy);
    this.renderer.domElement.style.cursor = p ? "pointer" : "";
    this.setHovered(p?.kind === "node" ? p.key : null);
  }

  private setHovered(key: string | null) {
    if (key === this.hovered) return;
    this.hovered = key;
    this.hoverRing.visible = !!key;
    if (key) {
      const ref = this.refs.get(key);
      if (ref) this.hoverRing.scale.setScalar(ref.place.card.sos ? 1.6 : 1.15);
    }
    this.hooks.onHover(key);
    this.step(performance.now());
    this.invalidate();
  }
}

function finish(im: THREE.InstancedMesh) {
  im.instanceMatrix.needsUpdate = true;
  if (im.instanceColor) im.instanceColor.needsUpdate = true;
  im.computeBoundingSphere();
  im.computeBoundingBox();
}

/** Frees the geometries, materials and textures of a subtree (a shared glow map is kept when flagged). */
function disposeTree(root: THREE.Object3D) {
  root.traverse((o) => {
    const mesh = o as THREE.Mesh;
    mesh.geometry?.dispose();
    const mats = mesh.material ? (Array.isArray(mesh.material) ? mesh.material : [mesh.material]) : [];
    for (const m of mats) {
      const map = (m as THREE.MeshBasicMaterial).map;
      if (map && !m.userData.keepMap) map.dispose();
      m.dispose();
    }
  });
}
