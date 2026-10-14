// The building's own 3D model inside the twin (docs/platform/digital-twin.md, "Building model"). The GLB is parsed
// off the critical path (the loaders are imported on demand), flattened into one mesh per material so a CAD export
// with thousands of nodes costs a handful of draw calls, and placed on the plans with the saved transform.

import * as THREE from "three";
import { mergeGeometries } from "three/examples/jsm/utils/BufferGeometryUtils.js";
import { MODEL_MAX_GROUPS, MODEL_MAX_TRIANGLES, type ModelTransform } from "../model-transform";
import { disposeMaterial, disposeTree } from "./building";

export type Flattened = { root: THREE.Group; triangles: number; meshes: number; collapsed: number };

const triCount = (g: THREE.BufferGeometry) => (g.index ? g.index.count : g.getAttribute("position")?.count ?? 0) / 3;

/** A model over the scene budget (docs/platform/digital-twin.md, "Building model import"). */
export class ModelBudgetError extends Error {
  constructor(readonly triangles: number) {
    super(`model has ${triangles} triangles, over the budget of ${MODEL_MAX_TRIANGLES}`);
    this.name = "ModelBudgetError";
  }
}

/**
 * Triangles a tree would draw, counted before anything is cloned (an instanced mesh counts every instance, so a
 * small GLB that instances a mesh a million times is refused here, not after a million clones).
 */
export function countTriangles(input: THREE.Object3D): number {
  let n = 0;
  input.traverse((o) => {
    const mesh = o as THREE.Mesh;
    if (!mesh.isMesh || !o.visible || !mesh.geometry?.getAttribute("position")) return;
    n += triCount(mesh.geometry) * ((o as THREE.InstancedMesh).isInstancedMesh ? (o as THREE.InstancedMesh).count : 1);
  });
  return Math.round(n);
}

const groupKey = (material: THREE.Material, g: THREE.BufferGeometry) => `${material.uuid}|${Object.keys(g.attributes).sort().join(",")}|${g.index ? "i" : "n"}`;

/** Folds parts with different materials into one geometry: position, normal and the material's colour per vertex. */
function collapse(parts: { geo: THREE.BufferGeometry; material: THREE.Material }[]): THREE.Mesh | null {
  const flat: THREE.BufferGeometry[] = [];
  const c = new THREE.Color();
  for (const { geo, material } of parts) {
    const g = geo.index ? geo.toNonIndexed() : geo.clone();
    for (const name of Object.keys(g.attributes)) if (name !== "position" && name !== "normal") g.deleteAttribute(name);
    if (!g.getAttribute("normal")) g.computeVertexNormals();
    const src = (material as THREE.MeshStandardMaterial).color;
    c.copy(src ?? new THREE.Color(0xc9d4d2));
    const n = g.getAttribute("position").count, colors = new Float32Array(n * 3);
    for (let i = 0; i < n; i++) { colors[i * 3] = c.r; colors[i * 3 + 1] = c.g; colors[i * 3 + 2] = c.b; }
    g.setAttribute("color", new THREE.BufferAttribute(colors, 3));
    flat.push(g);
  }
  const merged = mergeGeometries(flat, false);
  flat.forEach((g) => g.dispose());
  return merged ? new THREE.Mesh(merged, new THREE.MeshStandardMaterial({ vertexColors: true, roughness: 0.8, metalness: 0.05 })) : null;
}

/**
 * Strips lights and cameras and bakes every mesh into world space, merged per material and attribute layout, so the
 * model costs at most `maxGroups` draw calls: past that, the smallest material groups are folded into one
 * vertex-coloured mesh. Meshes that cannot merge (skinned, morphing, multi-material) are kept as they are, in world
 * space. Throws ModelBudgetError above `maxTriangles`, counted before anything is cloned. The input tree's
 * geometries are disposed; its materials and textures move to the output (folded materials are disposed).
 */
export function flattenModel(input: THREE.Object3D, opts: { maxTriangles?: number; maxGroups?: number } = {}): Flattened {
  const maxTriangles = opts.maxTriangles ?? MODEL_MAX_TRIANGLES, maxGroups = opts.maxGroups ?? MODEL_MAX_GROUPS;
  const total = countTriangles(input);
  if (total > maxTriangles) throw new ModelBudgetError(total);
  input.updateMatrixWorld(true);
  const groups = new Map<string, { material: THREE.Material; parts: THREE.BufferGeometry[]; tris: number }>();
  const keep: THREE.Object3D[] = [];
  const drop: THREE.BufferGeometry[] = [];
  const add = (material: THREE.Material, part: THREE.BufferGeometry) => {
    if (!part.getAttribute("normal")) part.computeVertexNormals();
    const key = groupKey(material, part);
    const g = groups.get(key) ?? { material, parts: [], tris: 0 };
    g.parts.push(part);
    g.tris += triCount(part);
    groups.set(key, g);
  };
  input.traverse((o) => {
    const mesh = o as THREE.Mesh;
    if (!(mesh.isMesh || (o as THREE.LineSegments).isLine || (o as THREE.Points).isPoints) || !o.visible) return;
    const geo = mesh.geometry as THREE.BufferGeometry | undefined;
    if (!geo?.getAttribute("position")) return;
    drop.push(geo);
    const inst = o as THREE.InstancedMesh;
    if (inst.isInstancedMesh && !Array.isArray(inst.material)) {
      // GPU instancing (EXT_mesh_gpu_instancing): bake every instance (already counted against the budget).
      const m = new THREE.Matrix4();
      for (let i = 0; i < inst.count; i++) {
        inst.getMatrixAt(i, m);
        add(inst.material as THREE.Material, geo.clone().applyMatrix4(m.premultiply(o.matrixWorld)));
      }
      return;
    }
    const world = geo.clone().applyMatrix4(o.matrixWorld);
    if (o.matrixWorld.determinant() < 0 && world.index) {
      // A mirrored node flips its winding; restore it so single-sided faces still face out.
      const idx = world.index.array as Uint32Array | Uint16Array;
      for (let i = 0; i < idx.length; i += 3) { const t = idx[i + 1]; idx[i + 1] = idx[i + 2]; idx[i + 2] = t; }
    }
    const material = mesh.material;
    const mergeable = mesh.isMesh && !(o as THREE.SkinnedMesh).isSkinnedMesh && !Array.isArray(material) && !Object.keys(world.morphAttributes).length;
    if (!mergeable) {
      const clone = mesh.isMesh ? new THREE.Mesh(world, material) : (o as THREE.LineSegments).isLineSegments ? new THREE.LineSegments(world, material) : (o as THREE.Line).isLine ? new THREE.Line(world, material) : new THREE.Points(world, material);
      keep.push(clone);
      return;
    }
    add(material as THREE.Material, world);
  });
  const root = new THREE.Group();
  root.name = "building-model";
  let meshes = 0, collapsed = 0;
  // Biggest groups keep their own material; past the draw-call budget the rest are folded into one mesh.
  const ordered = [...groups.values()].sort((a, b) => b.tris - a.tris);
  const room = Math.max(1, maxGroups - keep.length);
  const own = ordered.length > room ? ordered.slice(0, room - 1) : ordered;
  const folded = ordered.length > room ? ordered.slice(room - 1) : [];
  for (const { material, parts } of own) {
    const merged = parts.length === 1 ? parts[0] : mergeGeometries(parts, false);
    if (!merged) { for (const p of parts) root.add(new THREE.Mesh(p, material)); meshes += parts.length; continue; }
    if (parts.length > 1) parts.forEach((p) => p.dispose());
    root.add(new THREE.Mesh(merged, material));
    meshes++;
  }
  if (folded.length) {
    const mesh = collapse(folded.flatMap((g) => g.parts.map((geo) => ({ geo, material: g.material }))));
    // A material can head two groups (two attribute layouts): only dispose what no kept mesh still uses.
    const used = new Set<THREE.Material>(own.map((g) => g.material));
    for (const o of keep) for (const m of [(o as THREE.Mesh).material].flat()) if (m) used.add(m as THREE.Material);
    for (const g of folded) { g.parts.forEach((p) => p.dispose()); if (!used.has(g.material)) { used.add(g.material); disposeMaterial(g.material); } }
    if (mesh) { root.add(mesh); meshes++; }
    collapsed = folded.length;
  }
  for (const o of keep) { root.add(o); meshes++; }
  for (const g of drop) g.dispose();
  return { root, triangles: total, meshes, collapsed };
}

/**
 * Parses a stored GLB into a flattened model (the GLTF loader is imported on first use). A model over the budget
 * (posted straight to the API, or uploaded before the budget existed) throws ModelBudgetError and is never drawn.
 */
export async function parseModel(bytes: ArrayBuffer): Promise<Flattened> {
  const { gltfLoader, releaseLoader } = await import("../model-loaders");
  const loader = await gltfLoader();
  try {
    const gltf = await loader.parseAsync(bytes, "");
    try {
      return flattenModel(gltf.scene);
    } catch (e) {
      disposeTree(gltf.scene);
      throw e;
    }
  } finally {
    releaseLoader(loader);
  }
}

type MatState = { m: THREE.Material; opacity: number; transparent: boolean; depthWrite: boolean };

/**
 * CAD exports are mostly white or light grey: on the dark stage, lit by the environment and bloomed, they would
 * wash out the live layers. The model is drawn muted (its own hues, 60 % brightness, matte), so devices, heat and
 * alerts stay the brightest things in the scene.
 */
function muted(m: THREE.Material) {
  const s = m as THREE.MeshStandardMaterial;
  if ((s as THREE.MeshStandardMaterial).isMeshStandardMaterial) {
    s.color.multiplyScalar(0.6);
    s.envMapIntensity = 0.4;
    s.roughness = Math.max(s.roughness, 0.65);
    s.metalness = Math.min(s.metalness, 0.2);
  } else if ((m as THREE.MeshBasicMaterial).isMeshBasicMaterial || (m as THREE.LineBasicMaterial).isLineBasicMaterial) {
    (m as THREE.MeshBasicMaterial).color.multiplyScalar(0.6);
  }
}

/** Where the plans are in the world: their origin (top-left of the plans) and the lowest slab's elevation. */
export type PlanPlace = { originX: number; originZ: number; baseY: number; clipY: number | null };

/**
 * The model in the scene: outer group (offset, rotation, scale) → up-axis fix → the flattened model.
 * Changing the transform updates matrices and materials in place; nothing is reloaded.
 */
export class ModelLayer {
  readonly group = new THREE.Group();
  private upFix = new THREE.Group();
  private content: THREE.Group | null = null;
  private materials: MatState[] = [];
  private plane = new THREE.Plane(new THREE.Vector3(0, -1, 0), 0);
  private clipped = false;
  key = "";
  triangles = 0;

  constructor() {
    this.group.name = "model-layer";
    this.group.add(this.upFix);
    this.group.visible = false;
  }

  get loaded() { return !!this.content; }

  set(model: Flattened, key: string) {
    this.clear();
    this.content = model.root;
    this.key = key;
    this.triangles = model.triangles;
    const seen = new Set<THREE.Material>();
    model.root.traverse((o) => {
      // Between the stage (ground, contact shadows: ≤ 0) and the live layers (heat 1, glass 2): a see-through model
      // writes no depth, so whatever draws after it paints over it. Drawables only: a Group's renderOrder becomes
      // the sort's group order in three.js and would pull the whole model in front of the stage.
      if (!(o as THREE.Group).isGroup) o.renderOrder = 0.5;
      const mat = (o as THREE.Mesh).material as THREE.Material | THREE.Material[] | undefined;
      for (const m of Array.isArray(mat) ? mat : mat ? [mat] : []) {
        if (seen.has(m)) continue;
        seen.add(m);
        muted(m);
        this.materials.push({ m, opacity: m.opacity, transparent: m.transparent, depthWrite: m.depthWrite });
      }
    });
    this.upFix.add(model.root);
  }

  clear() {
    if (this.content) { this.upFix.remove(this.content); disposeTree(this.content); }
    this.content = null;
    this.materials = [];
    this.key = "";
    this.triangles = 0;
    this.clipped = false;
    this.group.visible = false;
  }

  apply(t: ModelTransform, place: PlanPlace) {
    this.upFix.rotation.set(t.up === "z" ? -Math.PI / 2 : 0, 0, 0);
    this.group.scale.setScalar(t.scale);
    const r = t.rotation_deg.map((d) => (d * Math.PI) / 180);
    this.group.rotation.set(r[0], r[1], r[2], "XYZ");
    this.group.position.set(place.originX + t.offset_m[0], place.baseY + t.offset_m[1], place.originZ + t.offset_m[2]);
    this.group.visible = !!this.content && t.visible;
    const clip = t.clip_to_floor && place.clipY !== null;
    if (clip) this.plane.constant = place.clipY!;
    for (const s of this.materials) {
      const see = t.opacity < 0.999 || s.transparent;
      const recompile = s.m.transparent !== see || clip !== this.clipped;
      s.m.transparent = see;
      s.m.opacity = s.opacity * t.opacity;
      s.m.depthWrite = t.opacity < 0.999 ? false : s.depthWrite;
      s.m.clippingPlanes = clip ? [this.plane] : null;
      if (recompile) s.m.needsUpdate = true;
    }
    this.clipped = clip;
  }

  /**
   * Scale and offset that fit the model (with its current up axis and rotation) inside a plan footprint of
   * width × depth metres, centred, standing on the lowest slab.
   */
  fit(t: ModelTransform, width: number, depth: number): Pick<ModelTransform, "scale" | "offset_m"> | null {
    if (!this.content) return null;
    const keep = { p: this.group.position.clone(), s: this.group.scale.clone(), r: this.group.rotation.clone(), u: this.upFix.rotation.clone() };
    this.upFix.rotation.set(t.up === "z" ? -Math.PI / 2 : 0, 0, 0);
    const r = t.rotation_deg.map((d) => (d * Math.PI) / 180);
    this.group.rotation.set(r[0], r[1], r[2], "XYZ");
    this.group.position.set(0, 0, 0);
    this.group.scale.setScalar(1);
    this.group.updateMatrixWorld(true);
    // Measured in the group's parent frame; the parent is the scene root (identity), so world = parent frame.
    const raw = new THREE.Box3().setFromObject(this.group);
    const size = raw.getSize(new THREE.Vector3());
    let out: Pick<ModelTransform, "scale" | "offset_m"> | null = null;
    if (size.x > 0 && size.z > 0 && Number.isFinite(size.x) && Number.isFinite(size.z)) {
      const scale = Math.min(10000, Math.max(0.0001, Math.min(width / size.x, depth / size.z)));
      const min = raw.min.clone().multiplyScalar(scale), max = raw.max.clone().multiplyScalar(scale);
      const round = (v: number) => Math.round(v * 1000) / 1000;
      out = { scale: Number(scale.toPrecision(6)), offset_m: [round(width / 2 - (min.x + max.x) / 2), round(-min.y), round(depth / 2 - (min.z + max.z) / 2)] };
    }
    this.group.position.copy(keep.p);
    this.group.scale.copy(keep.s);
    this.group.rotation.copy(keep.r);
    this.upFix.rotation.copy(keep.u);
    this.group.updateMatrixWorld(true);
    return out;
  }

  dispose() { this.clear(); }
}
