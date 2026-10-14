// Building model import (docs/platform/digital-twin.md): reads a CAD export in the browser with three.js loaders,
// flattens it (no lights or cameras, one mesh per material), writes one binary GLB and checks its size. The server
// then only stores GLB. Everything here is imported on demand from the model panel.

import * as THREE from "three";
import { disposeTree } from "./engine/building";
import { ModelBudgetError, flattenModel } from "./engine/model";
import { MAX_MODEL_BYTES, isGlb2, type ImportPhase, type SourceFormat } from "./model-api";
import { MAX_INPUT_BYTES, MODEL_MAX_GROUPS, MODEL_MAX_TRIANGLES } from "./model-transform";

export type Converted = { glb: ArrayBuffer; name: string; source_format: SourceFormat; triangles: number; meshes: number; warnings: string[] };

const budgetText = (triangles: number) =>
  `โมเดลมี ${triangles.toLocaleString("th-TH")} สามเหลี่ยม เกินที่ digital twin รับได้ (${MODEL_MAX_TRIANGLES.toLocaleString("th-TH")}) · ลดรายละเอียด (decimate) หรือตัดส่วนที่ไม่จำเป็น (เฟอร์นิเจอร์ ท่อ สกรู) ออกก่อน`;

/** Formats that must be exported to GLB, OBJ or FBX first, with the reason in Thai. */
const UNSUPPORTED: Record<string, string> = {
  dwg: "ไฟล์ DWG เป็นแบบ 2 มิติ/เฉพาะ AutoCAD · ส่งออกเป็น FBX หรือ OBJ จาก AutoCAD (หรือใช้ไฟล์ DXF กับหน้า \"ผังอาคาร\" เพื่อวาดผัง 2 มิติ)",
  dxf: "ไฟล์ DXF ใช้กับหน้า \"ผังอาคาร\" เพื่อนำเข้าผัง 2 มิติ · ถ้าต้องการโมเดล 3 มิติ ให้ส่งออกเป็น GLB, OBJ หรือ FBX",
  ifc: "IFC ยังไม่รองรับในรุ่นนี้ · ส่งออกจาก Revit/ArchiCAD เป็น FBX หรือ OBJ หรือแปลงเป็น GLB ก่อน",
  "3dm": "ไฟล์ Rhino (.3dm) ต้องใช้ตัวอ่านที่โหลดจากภายนอก ซึ่งระบบนี้ไม่ใช้ · ส่งออกจาก Rhino เป็น GLB, OBJ หรือ FBX",
  rvt: "ไฟล์ Revit ส่งออกเป็น FBX หรือ OBJ ก่อน (File → Export)",
  skp: "ไฟล์ SketchUp ส่งออกเป็น GLB/FBX/OBJ ก่อน (File → Export → 3D Model)",
  blend: "ไฟล์ Blender ส่งออกเป็น GLB ก่อน (File → Export → glTF 2.0)",
  dae: "Collada (.dae) ยังไม่รองรับ · ส่งออกเป็น GLB หรือ FBX",
  "3ds": "3DS ยังไม่รองรับ · ส่งออกเป็น FBX หรือ OBJ",
  ply: "PLY ยังไม่รองรับ · ส่งออกเป็น STL, OBJ หรือ GLB",
};

const MAIN: SourceFormat[] = ["glb", "gltf", "fbx", "obj", "stl"];
const ext = (name: string) => name.toLowerCase().split(".").pop() ?? "";
const base = (url: string) => {
  const clean = url.split(/[?#]/)[0];
  try { return decodeURIComponent(clean.split(/[\\/]/).pop() ?? "").toLowerCase(); } catch { return (clean.split(/[\\/]/).pop() ?? "").toLowerCase(); }
};

export class ImportError extends Error {}

/** Which file of a selection is the model, or a Thai explanation of why none is usable. */
export function pickMain(files: File[]): { main: File; format: SourceFormat } {
  for (const f of MAIN) {
    const hit = files.find((x) => ext(x.name) === f);
    if (hit) return { main: hit, format: f };
  }
  const odd = files.map((f) => ext(f.name)).find((e) => e in UNSUPPORTED);
  if (odd) throw new ImportError(UNSUPPORTED[odd]);
  throw new ImportError("ไม่พบไฟล์โมเดลที่รองรับ · เลือกไฟล์ .glb, .gltf, .obj, .fbx หรือ .stl");
}

/** Phong/Lambert/Toon materials (OBJ, FBX) → MeshStandardMaterial, keeping colour, maps and transparency. */
function standardize(root: THREE.Object3D) {
  const swapped = new Map<THREE.Material, THREE.Material>();
  const swap = (m: THREE.Material): THREE.Material => {
    if ((m as THREE.MeshStandardMaterial).isMeshStandardMaterial || (m as THREE.MeshBasicMaterial).isMeshBasicMaterial || (m as THREE.LineBasicMaterial).isLineBasicMaterial || (m as THREE.PointsMaterial).isPointsMaterial) return m;
    const hit = swapped.get(m);
    if (hit) return hit;
    const src = m as THREE.MeshPhongMaterial;
    const out = new THREE.MeshStandardMaterial({
      name: src.name, color: src.color?.clone() ?? new THREE.Color(0xcccccc), map: src.map ?? null, normalMap: src.normalMap ?? null,
      emissive: src.emissive?.clone() ?? new THREE.Color(0), emissiveMap: src.emissiveMap ?? null, alphaMap: src.alphaMap ?? null,
      opacity: src.opacity, transparent: src.transparent, side: src.side, vertexColors: src.vertexColors, roughness: 0.8, metalness: 0.05,
    });
    swapped.set(m, out);
    m.dispose();
    return out;
  };
  root.traverse((o) => {
    const mesh = o as THREE.Mesh;
    if (!mesh.material) return;
    mesh.material = Array.isArray(mesh.material) ? mesh.material.map(swap) : swap(mesh.material);
  });
}

function stripExtras(root: THREE.Object3D) {
  const drop: THREE.Object3D[] = [];
  root.traverse((o) => { if ((o as THREE.Light).isLight || (o as THREE.Camera).isCamera) drop.push(o); });
  for (const o of drop) o.removeFromParent();
  root.animations = [];
}

/**
 * Converts the selected files (a model plus, for .gltf and .obj, its buffers, materials and textures) into one GLB.
 * Throws ImportError with a Thai message for anything the user can fix.
 */
export async function convertToGlb(files: File[], onPhase?: (p: ImportPhase) => void): Promise<Converted> {
  const { main, format } = pickMain(files);
  // Checked before anything is read into memory.
  const input = files.reduce((n, f) => n + f.size, 0);
  if (input > MAX_INPUT_BYTES) throw new ImportError(`ไฟล์ที่เลือกรวม ${(input / 1048576).toFixed(0)} MiB เกิน ${MAX_INPUT_BYTES / 1048576} MiB · ลดรายละเอียดหรือขนาดรูปพื้นผิวก่อนส่งออกจากโปรแกรม CAD`);
  const warnings: string[] = [];
  const urls = new Map<string, string>();
  for (const f of files) urls.set(f.name.toLowerCase(), URL.createObjectURL(f));
  const missing = new Set<string>();
  const manager = new THREE.LoadingManager();
  // Resources a .gltf or .mtl names are matched by file name among the selected files.
  manager.setURLModifier((url) => {
    if (/^(data|blob):/i.test(url)) return url;
    const hit = urls.get(base(url));
    if (hit) return hit;
    missing.add(base(url));
    return "data:,";
  });
  const loading: { busy: boolean; done: (() => void) | null } = { busy: false, done: null };
  manager.onStart = () => { loading.busy = true; };
  manager.onLoad = () => { loading.busy = false; loading.done?.(); };
  const revoke = () => { for (const u of urls.values()) URL.revokeObjectURL(u); urls.clear(); };
  try {
    onPhase?.("read");
    let scene: THREE.Object3D;
    onPhase?.("parse");
    if (format === "glb" || format === "gltf") {
      const { gltfLoader, releaseLoader } = await import("./model-loaders");
      const loader = await gltfLoader(manager);
      try {
        const data = format === "glb" ? await main.arrayBuffer() : await main.text();
        const gltf = await loader.parseAsync(data, "");
        scene = gltf.scene;
      } finally {
        releaseLoader(loader);
      }
    } else if (format === "obj") {
      const [{ OBJLoader }, { MTLLoader }] = await Promise.all([import("three/examples/jsm/loaders/OBJLoader.js"), import("three/examples/jsm/loaders/MTLLoader.js")]);
      const loader = new OBJLoader(manager);
      const mtl = files.find((f) => ext(f.name) === "mtl");
      if (mtl) {
        const materials = new MTLLoader(manager).parse(await mtl.text(), "");
        materials.preload();
        loader.setMaterials(materials);
      }
      scene = loader.parse(await main.text());
    } else if (format === "fbx") {
      const { FBXLoader } = await import("three/examples/jsm/loaders/FBXLoader.js");
      scene = new FBXLoader(manager).parse(await main.arrayBuffer(), "");
    } else {
      const { STLLoader } = await import("three/examples/jsm/loaders/STLLoader.js");
      const geo = new STLLoader(manager).parse(await main.arrayBuffer());
      const colored = !!geo.getAttribute("color");
      scene = new THREE.Group();
      scene.add(new THREE.Mesh(geo, new THREE.MeshStandardMaterial({ color: colored ? 0xffffff : 0xc9d4d2, vertexColors: colored, roughness: 0.85 })));
    }
    // OBJ and FBX textures finish loading after parse returns (glTF waits for its own); wait for them, 8 s at most.
    if (loading.busy) await new Promise<void>((resolve) => { loading.done = resolve; setTimeout(resolve, 8000); });
    if (loading.busy) warnings.push("รูปพื้นผิวบางส่วนโหลดไม่ทันใน 8 วินาทีและถูกข้าม · โมเดลจะแสดงโดยไม่มีพื้นผิวเหล่านั้น");
    if (missing.size) {
      const list = [...missing].slice(0, 5).join(", ");
      const fatal = [...missing].some((n) => n.endsWith(".bin"));
      if (fatal) throw new ImportError(`ไฟล์ประกอบของโมเดลขาด: ${list} · เลือกไฟล์ .gltf พร้อมไฟล์ .bin และรูปพื้นผิวทั้งหมดในครั้งเดียว`);
      warnings.push(`ไม่พบไฟล์พื้นผิว ${list} · โมเดลจะแสดงโดยไม่มีพื้นผิวนั้น`);
    }

    onPhase?.("flatten");
    stripExtras(scene);
    standardize(scene);
    let flat;
    try {
      flat = flattenModel(scene);
    } catch (e) {
      disposeTree(scene);
      if (e instanceof ModelBudgetError) throw new ImportError(budgetText(e.triangles));
      throw e;
    }
    if (!flat.meshes) throw new ImportError("ไฟล์นี้ไม่มีพื้นผิวสามมิติให้แสดง");
    if (flat.collapsed) warnings.push(`โมเดลมีวัสดุมากกว่า ${MODEL_MAX_GROUPS} ชุด · ${flat.collapsed} ชุดที่เล็กที่สุดถูกรวมเป็นสีต่อจุด (ไม่มีพื้นผิว) เพื่อให้วาดได้เร็ว`);

    onPhase?.("export");
    const { GLTFExporter } = await import("three/examples/jsm/exporters/GLTFExporter.js");
    const exported = await new GLTFExporter().parseAsync(flat.root, { binary: true, onlyVisible: true, maxTextureSize: 2048 });
    if (!(exported instanceof ArrayBuffer)) throw new ImportError("แปลงเป็น GLB ไม่สำเร็จ");
    const glb = exported;
    disposeTree(flat.root);
    if (!isGlb2(glb)) throw new ImportError("ไฟล์ GLB ที่ได้ไม่ถูกต้อง");
    if (glb.byteLength > MAX_MODEL_BYTES) {
      throw new ImportError(`หลังแปลงเป็น GLB ไฟล์มีขนาด ${(glb.byteLength / 1048576).toFixed(1)} MiB เกิน 32 MiB · ลดรายละเอียดหรือขนาดรูปพื้นผิวก่อน`);
    }
    return { glb, name: main.name.slice(0, 120), source_format: format, triangles: flat.triangles, meshes: flat.meshes, warnings };
  } catch (e) {
    if (e instanceof ImportError) throw e;
    if (missing.size) {
      throw new ImportError(`ไฟล์ประกอบของโมเดลขาด: ${[...missing].slice(0, 5).join(", ")} · เลือกไฟล์ .gltf พร้อมไฟล์ .bin และรูปพื้นผิวทั้งหมดในครั้งเดียว`);
    }
    const why = e instanceof Error ? e.message : String(e);
    throw new ImportError(`อ่านไฟล์ ${main.name} ไม่ได้ (${why.slice(0, 160)}) · ลองส่งออกจากโปรแกรม CAD ใหม่เป็น GLB`);
  } finally {
    // A texture still loading after the wait keeps its object URL until the manager finishes (or a minute passes).
    if (loading.busy) { manager.onLoad = revoke; setTimeout(revoke, 60000); } else revoke();
  }
}
