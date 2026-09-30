"use client";
import { useEffect, useRef } from "react";
import * as THREE from "three";
import { OrbitControls } from "three/examples/jsm/controls/OrbitControls.js";
import { useLatest } from "../topology/use-latest";
import { buildBuilding, disposeTree, type Floor3D } from "../twin/engine/building";
import { clamp, round2, snapTo, type AssetView, type Selection } from "./model";

export type { Floor3D };

export default function View3D({
  floors,
  activeId,
  assets,
  selection,
  explode,
  isolate,
  snap,
  readOnly,
  resetKey,
  onSelect,
  onPickFloor,
  onMove,
  autoRotate = false,
}: {
  floors: Floor3D[];
  activeId: string;
  assets: AssetView[];
  selection: Selection;
  explode: number;
  isolate: boolean;
  snap: number;
  readOnly: boolean;
  /** Changing this re-frames the camera on the building. */
  resetKey: number;
  onSelect: (s: Selection) => void;
  onPickFloor: (id: string) => void;
  /** Called once, when a drag ends, with the new plan position in metres. */
  onMove: (key: string, x: number, y: number) => void;
  /** A slow orbit around the building, for a wall display nobody touches (app/display). */
  autoRotate?: boolean;
}) {
  const host = useRef<HTMLDivElement | null>(null);
  const three = useRef<{ scene: THREE.Scene; camera: THREE.PerspectiveCamera; renderer: THREE.WebGLRenderer; controls: OrbitControls; content: THREE.Group; pulses: THREE.Mesh[] } | null>(null);
  const latest = useLatest({ floors, activeId, snap, readOnly, onSelect, onPickFloor, onMove });

  // Renderer, camera, lights and pointer handling: created once.
  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true });
    renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
    renderer.setClearColor(0x000000, 0);
    el.appendChild(renderer.domElement);
    const scene = new THREE.Scene();
    scene.fog = new THREE.Fog(0x0b1418, 120, 420);
    const camera = new THREE.PerspectiveCamera(42, 1, 0.1, 2000);
    camera.position.set(28, 30, 38);
    const controls = new OrbitControls(camera, renderer.domElement);
    controls.enableDamping = true;
    controls.dampingFactor = 0.09;
    controls.maxPolarAngle = Math.PI / 2 - 0.04;
    controls.minDistance = 3;
    controls.maxDistance = 600;
    scene.add(new THREE.HemisphereLight(0xdff5ee, 0x0b1418, 1.15));
    const sun = new THREE.DirectionalLight(0xffffff, 1.3);
    sun.position.set(30, 60, 20);
    scene.add(sun);
    const content = new THREE.Group();
    scene.add(content);
    const state = { scene, camera, renderer, controls, content, pulses: [] as THREE.Mesh[] };
    three.current = state;

    const resize = () => {
      const w = el.clientWidth || 1, h = el.clientHeight || 1;
      renderer.setSize(w, h, false);
      renderer.domElement.style.width = "100%";
      renderer.domElement.style.height = "100%";
      camera.aspect = w / h;
      camera.updateProjectionMatrix();
    };
    const ro = new ResizeObserver(resize);
    ro.observe(el);
    resize();

    const raycaster = new THREE.Raycaster();
    const pointer = new THREE.Vector2();
    // While dragging, only the dragged objects move. The scene is rebuilt (and the draft changed) once, on release.
    let dragging: { key: string; plane: THREE.Plane; origin: THREE.Vector3; parts: THREE.Object3D[]; at: [number, number]; moved: boolean } | null = null;
    let downAt: [number, number] | null = null;
    const setPointer = (e: PointerEvent) => {
      const r = renderer.domElement.getBoundingClientRect();
      pointer.set(((e.clientX - r.left) / r.width) * 2 - 1, -((e.clientY - r.top) / r.height) * 2 + 1);
      raycaster.setFromCamera(pointer, camera);
    };
    // Upper floors are translucent and sit between the camera and the active floor, so a device of the active
    // floor wins over any slab in front of it.
    const pick = () => {
      const hits = raycaster.intersectObjects(content.children, true);
      return hits.find((h) => h.object.userData.pick === "placement" && h.object.userData.floor === latest.current.activeId) ?? hits.find((h) => h.object.userData.pick);
    };
    const down = (e: PointerEvent) => {
      if (e.button !== 0) return;
      setPointer(e);
      downAt = [e.clientX, e.clientY];
      const hit = pick();
      const data = hit?.object.userData as { pick?: string; key?: string; floor?: string; elevation?: number; origin?: [number, number] } | undefined;
      if (data?.pick === "placement" && data.key && data.floor === latest.current.activeId) {
        latest.current.onSelect({ kind: "placement", id: data.key });
        if (latest.current.readOnly) return;
        controls.enabled = false;
        renderer.domElement.setPointerCapture(e.pointerId);
        const parts: THREE.Object3D[] = [];
        content.traverse((o) => { if (o.userData.part === data.key) parts.push(o); });
        dragging = { key: data.key, plane: new THREE.Plane(new THREE.Vector3(0, 1, 0), -(data.elevation ?? 0)), origin: new THREE.Vector3(data.origin?.[0] ?? 0, 0, data.origin?.[1] ?? 0), parts, at: [hit!.object.position.x, hit!.object.position.z], moved: false };
      }
    };
    const move = (e: PointerEvent) => {
      if (!dragging) return;
      setPointer(e);
      const at = new THREE.Vector3();
      if (!raycaster.ray.intersectPlane(dragging.plane, at)) return;
      const active = latest.current.floors.find((f) => f.id === latest.current.activeId);
      if (!active) return;
      dragging.moved = true;
      const s = latest.current.snap;
      const x = round2(clamp(snapTo(at.x - dragging.origin.x, s), 0, active.draft.width_m)), z = round2(clamp(snapTo(at.z - dragging.origin.z, s), 0, active.draft.depth_m));
      const dx = x - dragging.at[0], dz = z - dragging.at[1];
      for (const part of dragging.parts) { part.position.x += dx; part.position.z += dz; }
      dragging.at = [x, z];
    };
    const up = (e: PointerEvent) => {
      if (dragging) {
        const d = dragging;
        dragging = null;
        controls.enabled = true;
        if (d.moved) latest.current.onMove(d.key, d.at[0], d.at[1]);
        return;
      }
      // A click (not an orbit drag) on another floor's slab activates that floor; on empty space clears the selection.
      if (!downAt || Math.hypot(e.clientX - downAt[0], e.clientY - downAt[1]) > 4) return;
      setPointer(e);
      const data = pick()?.object.userData as { pick?: string; floor?: string } | undefined;
      if (data?.floor && data.floor !== latest.current.activeId) latest.current.onPickFloor(data.floor);
      else if (data?.pick !== "placement") latest.current.onSelect(null);
    };
    renderer.domElement.addEventListener("pointerdown", down);
    renderer.domElement.addEventListener("pointermove", move);
    renderer.domElement.addEventListener("pointerup", up);
    // Palm rejection or a lost capture must not leave the orbit controls switched off.
    const cancel = () => { if (dragging) { dragging = null; controls.enabled = true; } downAt = null; };
    renderer.domElement.addEventListener("pointercancel", cancel);
    renderer.domElement.addEventListener("lostpointercapture", cancel);

    let frame = 0;
    const clock = new THREE.Clock();
    const loop = () => {
      frame = requestAnimationFrame(loop);
      const t = clock.getElapsedTime();
      for (const m of state.pulses) {
        const k = (t * 0.8 + (m.userData.phase as number)) % 1;
        m.scale.setScalar(1 + k * 1.6);
        (m.material as THREE.MeshBasicMaterial).opacity = 0.55 * (1 - k);
      }
      controls.update();
      renderer.render(scene, camera);
    };
    loop();
    return () => {
      cancelAnimationFrame(frame);
      ro.disconnect();
      renderer.domElement.removeEventListener("pointerdown", down);
      renderer.domElement.removeEventListener("pointermove", move);
      renderer.domElement.removeEventListener("pointerup", up);
      renderer.domElement.removeEventListener("pointercancel", cancel);
      renderer.domElement.removeEventListener("lostpointercapture", cancel);
      controls.dispose();
      disposeTree(content);
      renderer.dispose();
      renderer.forceContextLoss(); // browsers cap live WebGL contexts, and this view remounts on every 2D/3D switch
      renderer.domElement.remove();
      three.current = null;
    };
  }, [latest]);

  // Rebuild the building whenever the drawing, the live data or the view options change.
  useEffect(() => {
    const state = three.current;
    if (!state) return;
    state.pulses = buildBuilding(state.content, { floors, activeId, assets, selection, explode, isolate }).pulses;
  }, [floors, activeId, assets, selection, explode, isolate]);

  useEffect(() => {
    const state = three.current;
    if (!state) return;
    state.controls.autoRotate = autoRotate;
    state.controls.autoRotateSpeed = 0.35;
  }, [autoRotate]);

  // Frame the building when asked (first open, "fit" button, another site).
  useEffect(() => {
    const state = three.current;
    const active = latest.current.floors.find((f) => f.id === latest.current.activeId) ?? latest.current.floors[0];
    if (!state || !active) return;
    // Distance that fits the whole building's bounding sphere in the narrower of the two view angles.
    const height = latest.current.floors.reduce((sum, f) => sum + f.draft.ceiling_m + 0.35, 0);
    const radius = Math.hypot(active.draft.width_m, active.draft.depth_m) / 2 + height / 2;
    const half = (state.camera.fov * Math.PI) / 360;
    const distance = (radius / Math.sin(half) / Math.min(1, state.camera.aspect)) * 0.86;
    const dir = new THREE.Vector3(0.5, 0.62, 0.72).normalize();
    state.controls.target.set(0, height / 3, 0);
    state.camera.position.copy(dir.multiplyScalar(distance)).add(state.controls.target);
    state.controls.update();
  }, [resetKey, latest]);

  return <div ref={host} className="fp-3d" role="application" aria-label="ผังอาคารแบบ 3 มิติ" />;
}
