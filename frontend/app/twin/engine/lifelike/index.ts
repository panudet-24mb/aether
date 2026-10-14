// The lifelike hospital (docs/platform/digital-twin.md, "Lifelike hospital"): the immersive style's furnished
// interior, devices as fixtures, people as walking figures and data-driven effects, behind one small interface the
// engine calls. The classic style never creates it.

import * as THREE from "three";
import type { Draft } from "../../../floorplan/model";
import type { TwinAlert, TwinDevice, TwinState } from "../../api";
import type { TwinTheme } from "../../theme";
import { Crowd, type Obstacles, type WalkerSpec } from "./crowd";
import { Effects } from "./effects";
import { Fixtures, REPLACES_BEACON, TOGGLES, fixtureKind, floorSegments, mountFor, type FixtureState, type Mount } from "./fixtures";
import { buildInterior, type ZoneInterior } from "./interior";
import type { P, Segment } from "./geo";

type World = (floorId: string, x: number, y: number, h: number) => THREE.Vector3 | null;
/** `above`: a floor over the active one (drawn faded): it keeps the generic look and its beacons. */
export type LifelikeFloor = { id: string; draft: Draft; visible: boolean; active: boolean; above: boolean };

export { MAX_FIGURES } from "./crowd";
export type { WalkerSpec } from "./crowd";
export type { FixtureState } from "./fixtures";

export class Lifelike {
  readonly group = new THREE.Group();
  private interior: THREE.Group | null = null;
  private interiorKey = "";
  private furnitureVisible = true;
  private zoneInfo = new Map<string, ZoneInterior>();
  private fixtures: Fixtures;
  private crowd: Crowd;
  private effects: Effects;
  private floors: LifelikeFloor[] = [];
  private segments = new Map<string, Segment[]>();
  private mounts = new Map<string, Mount & { floorId: string }>();
  private world: World | null = null;
  private low = false;
  /** Each floor's plan origin in the scene (walkers are placed from it without allocating). */
  private origins = new Map<string, THREE.Vector3>();
  /** Mounts cached by floor layout and by what decides a mount (position, class, name): see layoutDevices. */
  private mountCache = new Map<string, Mount>();
  private layoutKey = new Map<string, string>();

  constructor(private theme: TwinTheme) {
    this.group.name = "lifelike";
    this.fixtures = new Fixtures(theme);
    this.crowd = new Crowd(theme);
    this.effects = new Effects(theme);
    this.group.add(this.fixtures.group, this.crowd.group, this.effects.group);
  }

  /** A new building layout (floors, active floor, spacing): segments, doors and effects follow it. */
  build(floors: LifelikeFloor[], world: World, low: boolean, devices: TwinDevice[]) {
    this.floors = floors;
    this.world = world;
    this.low = low;
    this.segments.clear();
    this.origins.clear();
    for (const f of floors) {
      this.segments.set(f.id, floorSegments(f.draft));
      const o = world(f.id, 0, 0, 0);
      if (o) this.origins.set(f.id, o);
      // A floor whose drawing changed drops its cached mounts (walls moved, zones redrawn).
      const key = layoutSignature(f.draft);
      if (this.layoutKey.get(f.id) !== key) {
        this.layoutKey.set(f.id, key);
        for (const k of [...this.mountCache.keys()]) if (k.startsWith(`${f.id}|`)) this.mountCache.delete(k);
      }
    }
    this.interiorKey = "";
    this.layoutDevices(devices);
    const doorSensors = devices.filter((d) => d.door !== undefined).map((d) => ({ id: d.id, floorId: d.floor_id, x: d.x, y: d.y }));
    this.effects.build(floors.filter((f) => f.visible && !f.above), world, low, doorSensors);
  }

  /** Mounts every device, rebuilds the fixtures, and the active floor's furniture when the devices moved. */
  private layoutDevices(devices: TwinDevice[]) {
    const world = this.world;
    if (!world) return;
    this.mounts.clear();
    const list: { d: TwinDevice; m: Mount }[] = [];
    // The cache keeps only the devices of this layout pass (a removed or changed device's entry goes).
    const used = new Set<string>();
    for (const d of devices) {
      const f = this.floors.find((x) => x.id === d.floor_id);
      if (!f || !f.visible || f.above) continue;
      // Only a device that moved, changed class or name (or a redrawn floor) is mounted again.
      const ck = `${f.id}|${d.id}|${d.x}|${d.y}|${fixtureKind(d) ?? ""}|${d.class ?? ""}|${d.name}`;
      used.add(ck);
      let m = this.mountCache.get(ck) ?? null;
      if (!m) {
        m = mountFor(d, f.draft, this.segments.get(f.id) ?? []);
        if (m) this.mountCache.set(ck, m);
      }
      if (!m) continue;
      this.mounts.set(d.id, { ...m, floorId: d.floor_id });
      list.push({ d, m });
    }
    for (const k of [...this.mountCache.keys()]) if (!used.has(k)) this.mountCache.delete(k);
    this.fixtures.build(list, world, this.low);
    const active = this.floors.find((f) => f.active && f.visible);
    const onFloor = active ? devices.filter((d) => d.floor_id === active.id).map((d) => [d.x, d.y] as P) : [];
    const key = active ? `${active.id}:${onFloor.map((p) => p.join(",")).join(";")}:${this.low}` : "";
    if (key === this.interiorKey) return;
    this.interiorKey = key;
    if (this.interior) { this.group.remove(this.interior); disposeGroup(this.interior); this.interior = null; }
    this.zoneInfo.clear();
    if (!active) return;
    const origin = world(active.id, 0, 0, 0);
    if (!origin) return;
    const built = buildInterior(active.draft, origin, this.theme, onFloor, this.low);
    this.interior = built.group;
    this.interior.visible = this.furnitureVisible;
    this.group.add(this.interior);
    for (const z of built.zones) this.zoneInfo.set(`${active.id}:${z.zoneIndex}`, z);
    const obstacles = new Map<string, Obstacles>();
    for (const [key, z] of this.zoneInfo) obstacles.set(key, { anchors: this.furnitureVisible ? z.anchors : [], blocked: this.furnitureVisible ? z.blocked : [] });
    this.crowd.setObstacles(obstacles);
  }

  /** A customer building model is shown: no procedural furniture (it would sit inside the model's own). */
  setFurnitureVisible(on: boolean) {
    if (on === this.furnitureVisible) return;
    this.furnitureVisible = on;
    if (this.interior) this.interior.visible = on;
    const obstacles = new Map<string, Obstacles>();
    for (const [key, z] of this.zoneInfo) obstacles.set(key, { anchors: on ? z.anchors : [], blocked: on ? z.blocked : [] });
    this.crowd.setObstacles(obstacles);
  }

  setState(state: TwinState, placeOf: (a: TwinAlert) => { floorId: string; x: number; y: number } | null) {
    this.layoutDevices(state.devices);
    this.fixtures.setDevices(state.devices, Date.parse(state.server_time) || 0);
    this.effects.setState(state.devices, state.alerts, Date.parse(state.server_time) || 0, placeOf);
  }

  setPeople(list: WalkerSpec[], detail: "figure" | "capsule", now: number) {
    this.crowd.setPeople(list, (floorId) => this.origins.get(floorId) ?? null, detail, now);
  }

  /** "replace" with a customer model: the model has its own doors and windows. */
  setReplaceMode(on: boolean) { this.effects.setOpeningsVisible(!on); }

  /**
   * One frame: `motion` false when motion is reduced (figures stand, particles hold still). `camera` decides which
   * figures are drawn in detail; `pixelScale` sizes smoke in metres.
   */
  step(now: number, t: number, dt: number, motion: boolean, camera: THREE.Vector3, pixelScale: number) {
    this.fixtures.step(t, dt, motion);
    this.crowd.step(now, dt, motion, camera);
    this.effects.setPixelScale(pixelScale);
    this.effects.step(t, dt, motion, now);
  }

  /** States the live view does not carry (an AC's switch, a cover's position), read from the devices' controls. */
  setFixtureStates(map: Record<string, FixtureState>) { this.fixtures.setStates(map); }

  /** A fixture part on its way (a rocker flipping, a blind moving): a short animation worth its frames. */
  moving(): boolean { return this.fixtures.moving(); }

  /** The site's time zone, for the night look of windows. */
  setTimeZone(tz: string | null) { this.effects.setTimeZone(tz); }

  /** People walking or effects drifting: worth ambient frames. */
  wantsFrames(): boolean { return this.crowd.size > 0 || this.effects.wantsFrames(); }

  /** The device's beacon is replaced by its fixture. */
  replacesBeacon(id: string): boolean { const m = this.mounts.get(id); return !!m && REPLACES_BEACON.has(m.kind); }

  /** Where a device hangs: its mount (wall, ceiling, door frame) on its floor, in plan metres and height. */
  mount(id: string): { floorId: string; x: number; y: number; h: number } | null {
    const m = this.mounts.get(id);
    return m ? { floorId: m.floorId, x: m.x, y: m.y, h: m.h } : null;
  }

  /** A click on a fixture: the device and whether a click switches it (else it opens the device's card). */
  pick(raycaster: THREE.Raycaster): { id: string; toggle: boolean; distance: number; kind: string } | null {
    const hit = raycaster.intersectObjects(this.fixtures.pickables(), false)[0];
    if (!hit) return null;
    const id = this.fixtures.idAt(hit.object, hit.instanceId);
    if (!id) return null;
    const kind = this.mounts.get(id)?.kind;
    return { id, toggle: !!kind && TOGGLES.has(kind), distance: hit.distance, kind: kind ?? "" };
  }

  dispose() {
    if (this.interior) disposeGroup(this.interior);
    this.fixtures.dispose();
    this.crowd.dispose();
    this.effects.dispose();
  }
}

/** A cheap signature of a floor's drawing (walls, zones, items): mounts are cached against it. */
function layoutSignature(d: Draft): string {
  let h = 0;
  const s = JSON.stringify([d.layout.walls, d.layout.zones.map((z) => z.points), d.layout.items.map((i) => [i.type, i.x, i.y, i.w, i.h, i.rot]), d.ceiling_m]);
  for (let i = 0; i < s.length; i++) h = (Math.imul(h, 31) + s.charCodeAt(i)) | 0;
  return `${s.length}:${h}`;
}

function disposeGroup(g: THREE.Object3D) {
  g.traverse((o) => {
    const m = o as THREE.InstancedMesh;
    m.geometry?.dispose();
    (m.material as THREE.Material | undefined)?.dispose?.();
    m.dispose?.();
  });
}
