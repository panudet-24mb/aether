// The lifelike hospital (docs/platform/digital-twin.md, "Lifelike hospital"): the immersive style's furnished
// interior, devices as fixtures, people as walking figures and data-driven effects, behind one small interface the
// engine calls. The classic style never creates it.

import * as THREE from "three";
import type { Draft } from "../../../floorplan/model";
import type { TwinAlert, TwinDevice, TwinState } from "../../api";
import type { TwinTheme } from "../../theme";
import { Crowd, type Obstacles, type WalkerSpec } from "./crowd";
import { Effects } from "./effects";
import { Fixtures, REPLACES_BEACON, TOGGLES, floorSegments, mountFor, type FixtureState, type Mount } from "./fixtures";
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
  /** Triangles of the furniture (the rest is counted by the renderer). */
  interiorTriangles = 0;

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
    for (const f of floors) this.segments.set(f.id, floorSegments(f.draft));
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
    for (const d of devices) {
      const f = this.floors.find((x) => x.id === d.floor_id);
      if (!f || !f.visible || f.above) continue;
      const m = mountFor(d, f.draft, this.segments.get(f.id) ?? []);
      if (!m) continue;
      this.mounts.set(d.id, { ...m, floorId: d.floor_id });
      list.push({ d, m });
    }
    this.fixtures.build(list, world, this.low);
    const active = this.floors.find((f) => f.active && f.visible);
    const onFloor = active ? devices.filter((d) => d.floor_id === active.id).map((d) => [d.x, d.y] as P) : [];
    const key = active ? `${active.id}:${onFloor.map((p) => p.join(",")).join(";")}:${this.low}` : "";
    if (key === this.interiorKey) return;
    this.interiorKey = key;
    if (this.interior) { this.group.remove(this.interior); disposeGroup(this.interior); this.interior = null; }
    this.zoneInfo.clear();
    this.interiorTriangles = 0;
    if (!active) return;
    const origin = world(active.id, 0, 0, 0);
    if (!origin) return;
    const built = buildInterior(active.draft, origin, this.theme, onFloor, this.low);
    this.interior = built.group;
    this.interior.visible = this.furnitureVisible;
    this.interiorTriangles = built.triangles;
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
    this.fixtures.setDevices(state.devices);
    this.effects.setState(state.devices, state.alerts, Date.parse(state.server_time) || 0, placeOf);
  }

  setPeople(list: WalkerSpec[], detail: "figure" | "capsule", now: number) {
    if (this.world) this.crowd.setPeople(list, this.world, detail, now);
  }

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
  pick(raycaster: THREE.Raycaster): { id: string; toggle: boolean; distance: number } | null {
    const hit = raycaster.intersectObjects(this.fixtures.pickables(), false)[0];
    if (!hit) return null;
    const id = this.fixtures.idAt(hit.object, hit.instanceId);
    if (!id) return null;
    const kind = this.mounts.get(id)?.kind;
    return { id, toggle: !!kind && TOGGLES.has(kind), distance: hit.distance };
  }

  dispose() {
    if (this.interior) disposeGroup(this.interior);
    this.fixtures.dispose();
    this.crowd.dispose();
    this.effects.dispose();
  }
}

function disposeGroup(g: THREE.Object3D) {
  g.traverse((o) => {
    const m = o as THREE.InstancedMesh;
    m.geometry?.dispose();
    (m.material as THREE.Material | undefined)?.dispose?.();
    m.dispose?.();
  });
}
