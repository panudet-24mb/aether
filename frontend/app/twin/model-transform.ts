// Pure helpers of the building model (docs/platform/digital-twin.md, "Building model"): the transform that places
// the model on the plans, and the byte work of a chunked GLB upload. No DOM or network here, so twin.selftest.ts
// can check them.

/** The model's share of the scene budget (under 400k triangles and 150 draw calls for the whole twin). */
export const MODEL_MAX_TRIANGLES = 300_000;
export const MODEL_MAX_GROUPS = 32;
/** What the picker reads at most, all selected files together (a 32 MiB GLB rarely needs more than this as input). */
export const MAX_INPUT_BYTES = 256 * 1024 * 1024;

/**
 * How the model sits on the plans. Axes are the twin's world axes after `up` is applied: x east (the plan's x),
 * y up, z south (the plan's y), in metres from the plans' origin (the top-left corner of the plans, on the lowest
 * slab). Rotation is a three.js Euler (XYZ) in degrees, applied after scale.
 */
export type ModelTransform = {
  scale: number;
  up: "y" | "z";
  rotation_deg: [number, number, number];
  offset_m: [number, number, number];
  opacity: number;
  visible: boolean;
  /** overlay: the model plus the generated glass walls; replace: the model instead of the generated building. */
  mode: "overlay" | "replace";
  /** Cut the model above the active floor's ceiling so the operator can look inside. */
  clip_to_floor: boolean;
};

export const DEFAULT_TRANSFORM: ModelTransform = { scale: 1, up: "y", rotation_deg: [0, 0, 0], offset_m: [0, 0, 0], opacity: 1, visible: true, mode: "overlay", clip_to_floor: true };

const finite = (v: unknown, d: number) => (typeof v === "number" && Number.isFinite(v) ? v : d);
const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));
const triple = (v: unknown, d: [number, number, number]): [number, number, number] =>
  Array.isArray(v) && v.length === 3 ? [finite(v[0], d[0]), finite(v[1], d[1]), finite(v[2], d[2])] : [...d];

/** A transform as the server may send it (fields missing on an old row), clamped to what the server accepts. */
export function normalizeTransform(t: Partial<ModelTransform> | null | undefined): ModelTransform {
  const d = DEFAULT_TRANSFORM;
  if (!t) return { ...d, rotation_deg: [...d.rotation_deg], offset_m: [...d.offset_m] };
  return {
    scale: clamp(finite(t.scale, d.scale), 0.0001, 10000),
    up: t.up === "z" ? "z" : "y",
    rotation_deg: triple(t.rotation_deg, d.rotation_deg).map((x) => ((((x + 180) % 360) + 360) % 360) - 180) as [number, number, number],
    offset_m: triple(t.offset_m, d.offset_m).map((x) => clamp(x, -100000, 100000)) as [number, number, number],
    opacity: clamp(finite(t.opacity, d.opacity), 0.05, 1),
    visible: t.visible !== false,
    mode: t.mode === "replace" ? "replace" : "overlay",
    clip_to_floor: t.clip_to_floor !== false,
  };
}

export const sameTransform = (a: ModelTransform, b: ModelTransform) => JSON.stringify(normalizeTransform(a)) === JSON.stringify(normalizeTransform(b));

/** Standard base64 of a byte range (built in 32 KiB slices: String.fromCharCode cannot take a whole chunk at once). */
export function base64(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(bin);
}

/** The byte ranges of an upload: every chunk exactly `size` bytes except the last. */
export function chunkRanges(total: number, size: number): [number, number][] {
  const out: [number, number][] = [];
  for (let at = 0; at < total; at += size) out.push([at, Math.min(total, at + size)]);
  return out;
}

/** "glb" for a GLB file's magic and version 2 (what the server checks on commit). */
export function isGlb2(bytes: ArrayBuffer): boolean {
  if (bytes.byteLength < 12) return false;
  const v = new DataView(bytes);
  return v.getUint32(0, true) === 0x46546c67 && v.getUint32(4, true) === 2 && v.getUint32(8, true) === bytes.byteLength;
}
