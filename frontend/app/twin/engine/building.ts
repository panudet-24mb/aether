import * as THREE from "three";
import { mergeGeometries } from "three/examples/jsm/utils/BufferGeometryUtils.js";
import { COLORS, occupancyShort, ringOffset, wearablesAt, zoneOccupancy, type AssetView, type Draft, type Selection } from "../../floorplan/model";

// The building's geometry, shared by the floor plan's 3D view and the digital twin (docs/platform/digital-twin.md).
// buildBuilding draws slabs, zones, walls and items of every floor and, for the floor plan, the placed devices and
// the people around their zone's gateway. The twin turns those off and draws its own live layers on top.

export type Floor3D = { id: string; draft: Draft };

const ITEM_HEIGHT: Record<string, number> = { door: 2.1, window: 1.2, stairs: 1.1, elevator: 2.6, exit: 2.1, bed: 0.55, desk: 0.75, rack: 1.9, extinguisher: 0.7, label: 0 };
export const INK = 0xe6f2ee, PANEL = 0x16232a, SLAB = 0x101b22, ACCENT = 0xa7f3d0, WARN = 0xf6c177, DANGER = 0xff8f70, MUTED = 0x5d7279, BLUE = 0x80b7ff;

export function label(text: string, color = "#e6f2ee", scale = 1): THREE.Sprite {
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

export function disposeTree(root: THREE.Object3D) {
  root.traverse((o) => {
    const m = o as THREE.Mesh;
    m.geometry?.dispose?.();
    const mats = Array.isArray(m.material) ? m.material : m.material ? [m.material] : [];
    for (const mat of mats) { (mat as THREE.SpriteMaterial).map?.dispose?.(); mat.dispose(); }
  });
}

export type BuildOptions = {
  floors: Floor3D[];
  activeId: string;
  assets: AssetView[];
  selection: Selection;
  explode: number;
  isolate: boolean;
  /** Placed gateways and devices with their labels (floor plan). Default true. */
  devices?: boolean;
  /** People drawn around their zone's gateway (floor plan). Default true. */
  people?: boolean;
  /** Sprite labels of floors, zones and items (floor plan). Default true. */
  labels?: boolean;
  /** Zone tint from live data (SOS, PIR occupancy, headcount). Default true; the twin paints zones itself. */
  liveZones?: boolean;
  /** Glass walls with accent edges (the twin's dark stage). Default false. */
  glass?: boolean;
  /** Tinted zone floors. Default true; the twin paints zones with its heat layer instead. */
  zones?: boolean;
};

/** Where a floor ended up: its world elevation and origin (the active floor is centred on the world origin). */
export type FloorFrame = { id: string; y: number; originX: number; originZ: number; visible: boolean; active: boolean };

/** Builds every floor into `content` (cleared first); returns the pulse rings to animate and where each floor went. */
export function buildBuilding(content: THREE.Group, opts: BuildOptions): { pulses: THREE.Mesh[]; frames: FloorFrame[] } {
  const { floors, activeId, assets, selection, explode, isolate } = opts;
  const showDevices = opts.devices ?? true, showPeople = opts.people ?? true, showLabels = opts.labels ?? true, liveZones = opts.liveZones ?? true, glass = opts.glass ?? false, showZones = opts.zones ?? true;
  disposeTree(content);
  content.clear();
  const pulses: THREE.Mesh[] = [];
  const frames: FloorFrame[] = [];
  const active = floors.find((f) => f.id === activeId) ?? floors[0];
  if (!active) return { pulses, frames };
  const originX = -active.draft.width_m / 2, originZ = -active.draft.depth_m / 2;
  const assetBy = new Map(assets.map((a) => [`${a.kind}:${a.id}`, a]));
  const sorted = [...floors].sort((a, b) => a.draft.level - b.draft.level);
  let elevation = 0;
  for (const f of sorted) {
    const d = f.draft, isActive = f.id === active.id;
    const y = elevation;
    elevation += (d.ceiling_m + 0.35) * (1 + explode * 1.8);
    frames.push({ id: f.id, y, originX, originZ, visible: !(isolate && !isActive), active: isActive });
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
    if (showLabels) {
      const tag = label(`${d.name} · L${d.level}`, isActive ? "#a7f3d0" : "#8fa3a8", 1.5);
      tag.position.set(-1.6, 0.6, d.depth_m / 2);
      g.add(tag);
    }

    for (const z of showZones ? d.layout.zones : []) {
      if (z.points.length < 3) continue;
      const shape = new THREE.Shape(z.points.map((p) => new THREE.Vector2(p[0], p[1])));
      const people = liveZones ? (z.gateway_ids ?? []).reduce((n, gw) => n + wearablesAt(assets, gw).length, 0) : 0;
      // The zone holding somebody who pressed SOS is the one the operator has to reach.
      const sosHere = liveZones && (z.gateway_ids ?? []).some((gw) => wearablesAt(assets, gw).some((w) => w.sos));
      // Smart office: a PIR sensor placed inside the room tints it occupied (accent) or vacant (muted).
      const occ = liveZones ? zoneOccupancy(z, d.placements, assetBy) : null;
      const tint = sosHere ? DANGER : occ ? (occ.occupied ? ACCENT : MUTED) : new THREE.Color(COLORS[z.color] ?? COLORS.mint);
      const mesh = new THREE.Mesh(new THREE.ExtrudeGeometry(shape, { depth: 0.05, bevelEnabled: false }), new THREE.MeshStandardMaterial({ color: new THREE.Color(tint), transparent: true, opacity: (sosHere ? 0.75 : occ ? (occ.occupied ? 0.66 : 0.22) : people > 0 ? 0.62 : 0.34) * dim, roughness: 1, side: THREE.DoubleSide }));
      mesh.rotation.x = Math.PI / 2; // plan (x, y) → world (x, -extrusion, y)
      mesh.position.y = 0.06;
      g.add(mesh);
      if (isActive && showLabels) {
        const cx = z.points.reduce((s, p) => s + p[0], 0) / z.points.length, cy = z.points.reduce((s, p) => s + p[1], 0) / z.points.length;
        const name = label(sosHere ? `SOS · ${z.name}` : `${z.name}${people > 0 ? ` · ${people} คน` : ""}${occ ? ` · ${occupancyShort(occ)}` : ""}`, sosHere ? "#ff8f70" : occ ? (occ.occupied ? "#a7f3d0" : "#8fa3a8") : (COLORS[z.color] ?? "#e6f2ee"), sosHere ? 1.1 : 0.9);
        name.position.set(cx, 0.45, cy);
        g.add(name);
      }
    }

    if (glass) {
      // The twin: every wall of the floor is one mesh and one set of edges, items one mesh per colour, so a
      // three-floor building stays within a few dozen draw calls.
      addMergedFloor(g, d, dim);
    } else {
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
          if (isActive && showLabels && it.text) { const s = label(it.text, "#b5c7cc", 0.8); s.position.set(it.x + it.w / 2, 0.4, it.y + it.h / 2); g.add(s); }
          continue;
        }
        const h = Math.min(ITEM_HEIGHT[it.type] ?? 0.6, d.ceiling_m);
        const color = it.type === "exit" ? ACCENT : it.type === "extinguisher" ? DANGER : it.type === "door" || it.type === "window" ? BLUE : MUTED;
        const mesh = new THREE.Mesh(new THREE.BoxGeometry(Math.max(it.w, 0.05), h, Math.max(it.h, 0.05)), new THREE.MeshStandardMaterial({ color, transparent: true, opacity: 0.75 * dim, roughness: 0.7 }));
        mesh.position.set(it.x + it.w / 2, h / 2 + 0.01, it.y + it.h / 2);
        mesh.rotation.y = (-it.rot * Math.PI) / 180;
        g.add(mesh);
      }
    }

    if (!showDevices) continue;
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
      if (p.asset_kind !== "gateway" || !showPeople) continue;
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
        pulses.push(pulse);
        if (isActive) { const who = label(w.sos ? `SOS · ${w.name}` : w.name, w.sos ? "#ff8f70" : "#80b7ff", w.sos ? 1 : 0.8); who.position.set(p.x + ox, 1.75, p.y + oz); who.userData.part = key; g.add(who); }
      });
    }
  }
  return { pulses, frames };
}

function placed(geo: THREE.BufferGeometry, x: number, y: number, z: number, rotY: number): THREE.BufferGeometry {
  geo.rotateY(rotY);
  geo.translate(x, y, z);
  // mergeGeometries needs the same attributes everywhere; boxes all have position, normal and uv.
  return geo;
}

/** Glass walls (one mesh plus one edge set) and items merged per colour, for one floor group. */
function addMergedFloor(g: THREE.Group, d: Draft, dim: number) {
  const walls: THREE.BufferGeometry[] = [];
  for (const w of d.layout.walls) {
    const pts = w.closed ? [...w.points, w.points[0]] : w.points;
    for (let i = 1; i < pts.length; i++) {
      const [x0, z0] = pts[i - 1], [x1, z1] = pts[i];
      const len = Math.hypot(x1 - x0, z1 - z0);
      if (len < 0.01) continue;
      walls.push(placed(new THREE.BoxGeometry(len + w.thickness, d.ceiling_m, w.thickness), (x0 + x1) / 2, d.ceiling_m / 2, (z0 + z1) / 2, -Math.atan2(z1 - z0, x1 - x0)));
    }
  }
  if (walls.length) {
    const merged = mergeGeometries(walls, false);
    walls.forEach((w) => w.dispose());
    if (merged) {
      const glassMat = new THREE.MeshPhysicalMaterial({ color: 0x9fd9c4, transparent: true, opacity: 0.1 + 0.08 * dim, roughness: 0.15, metalness: 0.05, depthWrite: false });
      const mesh = new THREE.Mesh(merged, glassMat);
      mesh.renderOrder = 2;
      g.add(mesh);
      const edges = new THREE.LineSegments(new THREE.EdgesGeometry(merged, 30), new THREE.LineBasicMaterial({ color: ACCENT, transparent: true, opacity: 0.08 + 0.2 * dim }));
      g.add(edges);
    }
  }
  const byColor = new Map<number, THREE.BufferGeometry[]>();
  for (const it of d.layout.items) {
    if (it.type === "label") continue;
    const h = Math.min(ITEM_HEIGHT[it.type] ?? 0.6, d.ceiling_m);
    const color = it.type === "exit" ? ACCENT : it.type === "extinguisher" ? DANGER : it.type === "door" || it.type === "window" ? BLUE : MUTED;
    const list = byColor.get(color) ?? [];
    list.push(placed(new THREE.BoxGeometry(Math.max(it.w, 0.05), h, Math.max(it.h, 0.05)), it.x + it.w / 2, h / 2 + 0.01, it.y + it.h / 2, (-it.rot * Math.PI) / 180));
    byColor.set(color, list);
  }
  for (const [color, list] of byColor) {
    const merged = mergeGeometries(list, false);
    list.forEach((x) => x.dispose());
    if (merged) g.add(new THREE.Mesh(merged, new THREE.MeshStandardMaterial({ color, transparent: true, opacity: 0.7 * dim, roughness: 0.7 })));
  }
}
