"use client";
import { useEffect, useRef } from "react";
import * as THREE from "three";
import { OrbitControls } from "three/examples/jsm/controls/OrbitControls.js";
import { useLatest } from "../topology/use-latest";
import { COLORS, clamp, occupancyShort, ringOffset, round2, snapTo, wearablesAt, zoneOccupancy, type AssetView, type Draft, type Selection } from "./model";

export type Floor3D = { id: string; draft: Draft };

const ITEM_HEIGHT: Record<string, number> = { door: 2.1, window: 1.2, stairs: 1.1, elevator: 2.6, exit: 2.1, bed: 0.55, desk: 0.75, rack: 1.9, extinguisher: 0.7, label: 0 };
const INK = 0xe6f2ee, PANEL = 0x16232a, SLAB = 0x101b22, ACCENT = 0xa7f3d0, WARN = 0xf6c177, DANGER = 0xff8f70, MUTED = 0x5d7279, BLUE = 0x80b7ff;

function label(text: string, color = "#e6f2ee", scale = 1): THREE.Sprite {
  const canvas = document.createElement("canvas");
  const ctx = canvas.getContext("2d")!;
  const font = "500 44px system-ui, 'Noto Sans Thai', sans-serif";
  ctx.font = font;
  const w = Math.min(1024, Math.ceil(ctx.measureText(text).width) + 40);
  canvas.width = w; canvas.height = 72;
  ctx.font = font;
  ctx.fillStyle = "rgba(10,18,22,.78)";
  ctx.beginPath(); ctx.roundRect(0, 0, w, 72, 18); ctx.fill();
  ctx.fillStyle = color; ctx.textBaseline = "middle"; ctx.textAlign = "center";
  ctx.fillText(text, w / 2, 38, w - 24);
  const texture = new THREE.CanvasTexture(canvas);
  texture.colorSpace = THREE.SRGBColorSpace;
  const sprite = new THREE.Sprite(new THREE.SpriteMaterial({ map: texture, depthTest: false, transparent: true, sizeAttenuation: false }));
  // Constant size on screen (fractions of the viewport height), so names stay readable when zoomed out.
  sprite.scale.set((w / 72) * 0.021 * scale, 0.021 * scale, 1);
  sprite.renderOrder = 10;
  return sprite;
}

function disposeTree(root: THREE.Object3D) {
  root.traverse((o) => {
    const m = o as THREE.Mesh;
    m.geometry?.dispose?.();
    const mats = Array.isArray(m.material) ? m.material : m.material ? [m.material] : [];
    for (const mat of mats) { (mat as THREE.SpriteMaterial).map?.dispose?.(); mat.dispose(); }
  });
}

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
    const { content } = state;
    disposeTree(content);
    content.clear();
    state.pulses = [];
    const active = floors.find((f) => f.id === activeId) ?? floors[0];
    if (!active) return;
    const originX = -active.draft.width_m / 2, originZ = -active.draft.depth_m / 2;
    const assetBy = new Map(assets.map((a) => [`${a.kind}:${a.id}`, a]));
    const sorted = [...floors].sort((a, b) => a.draft.level - b.draft.level);
    let elevation = 0;
    for (const f of sorted) {
      const d = f.draft, isActive = f.id === active.id;
      const y = elevation;
      elevation += (d.ceiling_m + 0.35) * (1 + explode * 1.8);
      if (isolate && !isActive) continue;
      const dim = isActive ? 1 : 0.32;
      const g = new THREE.Group();
      g.position.set(originX, y, originZ);
      content.add(g);

      const slab = new THREE.Mesh(new THREE.BoxGeometry(d.width_m, 0.14, d.depth_m), new THREE.MeshStandardMaterial({ color: isActive ? PANEL : SLAB, roughness: 0.95, transparent: !isActive, opacity: isActive ? 1 : 0.55 }));
      slab.position.set(d.width_m / 2, -0.07, d.depth_m / 2);
      slab.userData = { pick: "floor", floor: f.id };
      g.add(slab);
      const outline = new THREE.LineSegments(new THREE.EdgesGeometry(slab.geometry), new THREE.LineBasicMaterial({ color: isActive ? ACCENT : MUTED, transparent: true, opacity: isActive ? 0.8 : 0.35 }));
      outline.position.copy(slab.position);
      g.add(outline);
      const tag = label(`${d.name} · L${d.level}`, isActive ? "#a7f3d0" : "#8fa3a8", 1.5);
      tag.position.set(-1.6, 0.6, d.depth_m / 2);
      g.add(tag);

      for (const z of d.layout.zones) {
        if (z.points.length < 3) continue;
        const shape = new THREE.Shape(z.points.map((p) => new THREE.Vector2(p[0], p[1])));
        const people = (z.gateway_ids ?? []).reduce((n, gw) => n + wearablesAt(assets, gw).length, 0);
        // The zone holding somebody who pressed SOS is the one the operator has to reach.
        const sosHere = (z.gateway_ids ?? []).some((gw) => wearablesAt(assets, gw).some((w) => w.sos));
        // Smart office: a PIR sensor placed inside the room tints it occupied (accent) or vacant (muted).
        const occ = zoneOccupancy(z, d.placements, assetBy);
        const tint = sosHere ? DANGER : occ ? (occ.occupied ? ACCENT : MUTED) : new THREE.Color(COLORS[z.color] ?? COLORS.mint);
        const mesh = new THREE.Mesh(new THREE.ExtrudeGeometry(shape, { depth: 0.05, bevelEnabled: false }), new THREE.MeshStandardMaterial({ color: new THREE.Color(tint), transparent: true, opacity: (sosHere ? 0.75 : occ ? (occ.occupied ? 0.66 : 0.22) : people > 0 ? 0.62 : 0.34) * dim, roughness: 1, side: THREE.DoubleSide }));
        mesh.rotation.x = Math.PI / 2; // plan (x, y) → world (x, -extrusion, y)
        mesh.position.y = 0.06;
        g.add(mesh);
        if (isActive) {
          const cx = z.points.reduce((s, p) => s + p[0], 0) / z.points.length, cy = z.points.reduce((s, p) => s + p[1], 0) / z.points.length;
          const name = label(sosHere ? `SOS · ${z.name}` : `${z.name}${people > 0 ? ` · ${people} คน` : ""}${occ ? ` · ${occupancyShort(occ)}` : ""}`, sosHere ? "#ff8f70" : occ ? (occ.occupied ? "#a7f3d0" : "#8fa3a8") : (COLORS[z.color] ?? "#e6f2ee"), sosHere ? 1.1 : 0.9);
          name.position.set(cx, 0.45, cy);
          g.add(name);
        }
      }

      const wallMat = new THREE.MeshStandardMaterial({ color: 0xcfe0dc, transparent: true, opacity: 0.5 * dim, roughness: 0.8 });
      for (const w of d.layout.walls) {
        const pts = w.closed ? [...w.points, w.points[0]] : w.points;
        for (let i = 1; i < pts.length; i++) {
          const [x0, z0] = pts[i - 1], [x1, z1] = pts[i];
          const len = Math.hypot(x1 - x0, z1 - z0);
          if (len < 0.01) continue;
          const mesh = new THREE.Mesh(new THREE.BoxGeometry(len + w.thickness, d.ceiling_m, w.thickness), wallMat);
          mesh.position.set((x0 + x1) / 2, d.ceiling_m / 2, (z0 + z1) / 2);
          mesh.rotation.y = -Math.atan2(z1 - z0, x1 - x0);
          g.add(mesh);
        }
      }

      for (const it of d.layout.items) {
        if (it.type === "label") {
          if (isActive && it.text) { const s = label(it.text, "#b5c7cc", 0.8); s.position.set(it.x + it.w / 2, 0.4, it.y + it.h / 2); g.add(s); }
          continue;
        }
        const h = Math.min(ITEM_HEIGHT[it.type] ?? 0.6, d.ceiling_m);
        const color = it.type === "exit" ? ACCENT : it.type === "extinguisher" ? DANGER : it.type === "door" || it.type === "window" ? BLUE : MUTED;
        const mesh = new THREE.Mesh(new THREE.BoxGeometry(Math.max(it.w, 0.05), h, Math.max(it.h, 0.05)), new THREE.MeshStandardMaterial({ color, transparent: true, opacity: 0.75 * dim, roughness: 0.7 }));
        mesh.position.set(it.x + it.w / 2, h / 2 + 0.01, it.y + it.h / 2);
        mesh.rotation.y = (-it.rot * Math.PI) / 180;
        g.add(mesh);
      }

      for (const p of d.placements) {
        const key = `${p.asset_kind}:${p.asset_id}`, a = assetBy.get(key), selected = selection?.kind === "placement" && selection.id === key;
        const color = a?.alert ? DANGER : a?.door?.leftOpen ? WARN : a?.online ? ACCENT : a ? WARN : MUTED;
        const stem = new THREE.Mesh(new THREE.CylinderGeometry(0.025, 0.025, Math.max(p.z, 0.05), 8), new THREE.MeshBasicMaterial({ color, transparent: true, opacity: 0.45 * dim }));
        stem.position.set(p.x, p.z / 2, p.y);
        stem.userData.part = key;
        g.add(stem);
        const body = p.asset_kind === "gateway" ? new THREE.BoxGeometry(0.62, 0.2, 0.62) : new THREE.SphereGeometry(0.24, 20, 14);
        const mesh = new THREE.Mesh(body, new THREE.MeshStandardMaterial({ color, emissive: color, emissiveIntensity: selected ? 0.9 : a?.online ? 0.45 : 0.1, roughness: 0.4, transparent: !isActive, opacity: dim }));
        mesh.position.set(p.x, p.z, p.y);
        if (a?.door) {
          // Door sensor: a door leaf hinged at the marker, swung open (amber when left open) or shut.
          const leafColor = a.door.leftOpen ? WARN : a.door.open ? ACCENT : MUTED;
          const leafGeo = new THREE.BoxGeometry(0.9, Math.min(2, d.ceiling_m * 0.7), 0.05);
          leafGeo.translate(0.45, Math.min(2, d.ceiling_m * 0.7) / 2, 0);
          const leaf = new THREE.Mesh(leafGeo, new THREE.MeshStandardMaterial({ color: leafColor, emissive: leafColor, emissiveIntensity: a.door.open ? 0.35 : 0.08, roughness: 0.6, transparent: true, opacity: 0.8 * dim }));
          leaf.position.set(p.x - 0.45, 0.02, p.y);
          leaf.rotation.y = a.door.open ? -1.15 : 0;
          leaf.userData = { pick: "placement", part: key, key, floor: f.id, elevation: y, origin: [originX, originZ] };
          g.add(leaf);
        }
        mesh.userData = { pick: "placement", part: key, key, floor: f.id, elevation: y, origin: [originX, originZ] };
        g.add(mesh);
        if (selected) {
          const ring = new THREE.Mesh(new THREE.RingGeometry(0.55, 0.64, 40), new THREE.MeshBasicMaterial({ color: INK, side: THREE.DoubleSide, transparent: true, opacity: 0.9 }));
          ring.rotation.x = -Math.PI / 2;
          ring.position.set(p.x, 0.09, p.y);
          ring.userData.part = key;
          g.add(ring);
        }
        if (isActive) {
          const name = label(a ? (a.summary && p.asset_kind === "device" ? `${a.name} · ${a.summary}` : a.name) : "ไม่พบอุปกรณ์", a?.online ? "#e6f2ee" : "#f6c177", 0.85);
          name.position.set(p.x, p.z + 0.62, p.y);
          name.userData.part = key;
          g.add(name);
        }
        if (p.asset_kind !== "gateway") continue;
        const people = wearablesAt(assets, p.asset_id);
        people.forEach((w, i) => {
          const [ox, oz] = ringOffset(i, people.length, 1.5);
          // An emergency press turns the person red and names them, the same treatment as in 2D.
          const tone = w.sos ? DANGER : BLUE;
          const person = new THREE.Mesh(new THREE.CapsuleGeometry(w.sos ? 0.26 : 0.2, 0.75, 6, 14), new THREE.MeshStandardMaterial({ color: tone, emissive: tone, emissiveIntensity: w.sos ? 1 : 0.5, roughness: 0.5, transparent: !isActive, opacity: dim }));
          person.position.set(p.x + ox, 0.62, p.y + oz);
          person.userData.part = key;
          g.add(person);
          const pulse = new THREE.Mesh(new THREE.RingGeometry(0.3, w.sos ? 0.42 : 0.36, 36), new THREE.MeshBasicMaterial({ color: tone, side: THREE.DoubleSide, transparent: true, opacity: 0.5, depthWrite: false }));
          pulse.rotation.x = -Math.PI / 2;
          pulse.position.set(p.x + ox, 0.1, p.y + oz);
          pulse.userData.phase = i * 0.37;
          pulse.userData.part = key;
          g.add(pulse);
          state.pulses.push(pulse);
          if (isActive) { const who = label(w.sos ? `SOS · ${w.name}` : w.name, w.sos ? "#ff8f70" : "#80b7ff", w.sos ? 1 : 0.8); who.position.set(p.x + ox, 1.75, p.y + oz); who.userData.part = key; g.add(who); }
        });
      }
    }
  }, [floors, activeId, assets, selection, explode, isolate]);

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
