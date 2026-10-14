// The building's 3D model (docs/platform/digital-twin.md, "Building model"): one GLB per site, uploaded in chunks
// and aligned to the floor plans with a transform. The page converts every supported format to GLB in the browser
// (model-import.ts), so the server only ever stores and serves GLB.

import { ApiError } from "../topology/api";
import { chunkRanges, base64, normalizeTransform, type ModelTransform } from "./model-transform";

export { DEFAULT_TRANSFORM, isGlb2, normalizeTransform, sameTransform, type ModelTransform } from "./model-transform";

export type SourceFormat = "glb" | "gltf" | "obj" | "fbx" | "stl" | "3dm" | "ifc";

export type SiteModel = {
  revision: number;
  name: string;
  source_format: SourceFormat;
  size: number;
  sha256: string;
  uploaded_at: string;
  transform: ModelTransform;
};

/** What the model picker offers: the model files and the buffers, materials and textures they may reference. */
export const ACCEPT = ".glb,.gltf,.bin,.obj,.mtl,.fbx,.stl,.png,.jpg,.jpeg,.webp,.tga,.bmp";

export type ImportPhase = "read" | "parse" | "flatten" | "export" | "checksum";

/** After conversion to GLB (the server refuses more). */
export const MAX_MODEL_BYTES = 32 * 1024 * 1024;

/** The slice of the shared API client the model needs, plus a raw sender for the calls that read headers. */
export type ModelClient = {
  raw: <T>(path: string) => Promise<T>;
  post: <T>(path: string, body: unknown) => Promise<T>;
  send: ModelSend;
};

export type SendOptions = { signal?: AbortSignal; keepalive?: boolean; cache?: RequestCache };
export type ModelSend = (method: "GET" | "POST", path: string, body?: unknown, opts?: SendOptions) => Promise<Response>;

const API = import.meta.env.VITE_AETHER_API_ORIGIN ?? "";

/**
 * An authenticated fetch that hands back the Response, with the shared client's rules: bearer token, one refresh and
 * retry on 401. (The shared client throws away headers; Retry-After is what these calls need.) `signal` aborts the
 * request itself, not only the wait before the next one.
 */
export function modelSender(handlers: { readonly current: { getToken: () => string; refresh: () => Promise<boolean> } }): ModelSend {
  return async (method, path, body, opts = {}) => {
    // A 32 MiB download on a slow link takes a while; a chunk is 160 KiB.
    const timeout = AbortSignal.timeout(method === "GET" ? 600000 : 30000);
    const signal = opts.signal && "any" in AbortSignal ? AbortSignal.any([timeout, opts.signal]) : timeout;
    const go = () => fetch(`${API}/api/v1${path}`, {
      method,
      headers: { Authorization: `Bearer ${handlers.current.getToken()}`, ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: opts.cache ?? "no-store",
      keepalive: opts.keepalive,
      signal,
    });
    let r = await go();
    if (r.status === 401 && (await handlers.current.refresh())) r = await go();
    return r;
  };
}

const aborted = () => new DOMException("cancelled", "AbortError");

/** A pause that ends early (rejecting) when `signal` aborts. */
function wait(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(aborted()); return; }
    const t = setTimeout(() => { signal?.removeEventListener("abort", stop); resolve(); }, ms);
    const stop = () => { clearTimeout(t); reject(aborted()); };
    signal?.addEventListener("abort", stop, { once: true });
  });
}

/** Seconds from a Retry-After header (seconds or an HTTP date), else the fallback. */
function retryAfter(r: Response, fallback: number): number {
  const v = r.headers.get("Retry-After");
  if (!v) return fallback;
  const n = Number(v);
  if (Number.isFinite(n)) return Math.min(120, Math.max(1, n));
  const at = Date.parse(v);
  return Number.isFinite(at) ? Math.min(120, Math.max(1, (at - Date.now()) / 1000)) : fallback;
}

async function failure(r: Response): Promise<ApiError> {
  let reason: string | undefined, body: unknown;
  try { body = await r.json(); const e = (body as { error?: unknown }).error; if (typeof e === "string") reason = e; } catch { /* no JSON body */ }
  return new ApiError(r.status, `ดำเนินการไม่สำเร็จ (${r.status})`, reason, body);
}

/** An unfinished upload that blocks a new one (409 `upload_in_progress`): its id, to cancel it. */
export function stuckUpload(e: unknown): { uploadId: string; startedAt?: string } | null {
  if (!(e instanceof ApiError) || e.status !== 409 || e.reason !== "upload_in_progress") return null;
  const b = e.body as { upload_id?: unknown; started_at?: unknown } | undefined;
  return typeof b?.upload_id === "string" ? { uploadId: b.upload_id, startedAt: typeof b.started_at === "string" ? b.started_at : undefined } : null;
}

/** Drops an unfinished upload (best effort; `keepalive` lets it finish while the page closes). */
export async function cancelUpload(client: ModelClient, siteId: string, uploadId: string): Promise<boolean> {
  try {
    const r = await client.send("POST", `/sites/${siteId}/model/uploads/${uploadId}/cancel`, {}, { keepalive: true });
    return r.ok || r.status === 404;
  } catch {
    return false;
  }
}

/** The site's model, or null when it has none. A server without the model API (404 on the route) reads as none. */
export async function fetchModel(client: ModelClient, siteId: string): Promise<SiteModel | null> {
  try {
    const r = await client.raw<{ model: SiteModel | null }>(`/sites/${siteId}/model`);
    return r?.model ? { ...r.model, transform: normalizeTransform(r.model.transform) } : null;
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

/**
 * The GLB of a revision; null when that revision is no longer the current one (404: the model changed or went away,
 * re-read the record). A 503 (the server's download cap) is waited out per Retry-After, 5 times at most. The URL
 * carries the revision, so the browser cache (`private, max-age`) may answer it.
 */
export async function fetchModelFile(client: ModelClient, siteId: string, revision: number, signal?: AbortSignal): Promise<ArrayBuffer | null> {
  const path = `/sites/${siteId}/model/file?rev=${revision}`;
  for (let attempt = 0; ; attempt++) {
    const r = await client.send("GET", path, undefined, { signal, cache: "default" });
    if (r.ok) return r.arrayBuffer();
    if (r.status === 404) return null;
    if (r.status === 503 && attempt < 5) { await wait(retryAfter(r, 5) * 1000, signal); continue; }
    throw await failure(r);
  }
}

export async function saveTransform(client: ModelClient, siteId: string, t: ModelTransform): Promise<SiteModel> {
  const r = await client.post<{ model: SiteModel }>(`/sites/${siteId}/model/transform`, normalizeTransform(t));
  return { ...r.model, transform: normalizeTransform(r.model.transform) };
}

export async function deleteModel(client: ModelClient, siteId: string): Promise<void> {
  await client.post<void>(`/sites/${siteId}/model/delete`, {});
}

// ---- bytes ------------------------------------------------------------------------------------------------------

/** Lower-case hex SHA-256 of the bytes (WebCrypto). */
export async function sha256Hex(bytes: ArrayBuffer): Promise<string> {
  const d = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let s = "";
  for (const b of d) s += b.toString(16).padStart(2, "0");
  return s;
}

// ---- upload -----------------------------------------------------------------------------------------------------

/** `wait`: seconds the upload pauses because the API's per-IP rate limit answered 429 (it then resends the same chunk). */
export type UploadProgress = { sent: number; total: number; chunk: number; chunks: number; retry: number; wait?: number };

/**
 * Uploads a GLB: start, every chunk in order, commit. Returns the stored model. A chunk that fails is resent up to 3
 * times; a 429 from the API's per-IP rate limit (a 32 MiB model is ~200 requests) is waited out per Retry-After and
 * never counts as a failure; a 503 on commit (the server's commit cap) likewise, 6 times at most. `signal` aborts the
 * request in flight and any wait. After a failure or an abort the unfinished upload is cancelled on the server.
 */
export async function uploadModel(
  client: ModelClient,
  siteId: string,
  input: { name: string; source_format: SourceFormat; glb: ArrayBuffer; sha256: string },
  onProgress?: (p: UploadProgress) => void,
  signal?: AbortSignal,
): Promise<SiteModel> {
  const size = input.glb.byteLength;
  const r0 = await client.send("POST", `/sites/${siteId}/model/uploads`, { name: input.name, source_format: input.source_format, size, sha256: input.sha256 }, { signal });
  if (!r0.ok) throw await failure(r0);
  const start = (await r0.json()) as { upload_id: string; chunk_size: number; chunks: number };
  try {
    const ranges = chunkRanges(size, start.chunk_size);
    if (start.chunks && start.chunks !== ranges.length) throw new Error(`เซิร์ฟเวอร์คาดว่าจะได้ ${start.chunks} ส่วน แต่ไฟล์แบ่งได้ ${ranges.length} ส่วน · ลองใหม่อีกครั้ง`);
    const bytes = new Uint8Array(input.glb);
    let sent = 0;
    for (let i = 0; i < ranges.length; i++) {
      const [a, b] = ranges[i];
      const body = { data_base64: base64(bytes.subarray(a, b)) };
      const path = `/sites/${siteId}/model/uploads/${start.upload_id}/chunks/${i}`;
      for (let attempt = 0, limited = 0; ;) {
        signal?.throwIfAborted();
        onProgress?.({ sent, total: size, chunk: i, chunks: ranges.length, retry: attempt });
        try {
          const r = await client.send("POST", path, body, { signal });
          if (r.status === 429 && limited < 40) {
            // The per-IP limiter: wait as long as it says and resend the same index (a chunk POST is idempotent).
            // Never counted as a failure; the upload session stays valid for an hour.
            const secs = Math.ceil(retryAfter(r, Math.min(15, 2 * 2 ** limited)));
            limited++;
            onProgress?.({ sent, total: size, chunk: i, chunks: ranges.length, retry: attempt, wait: secs });
            await wait(secs * 1000, signal);
            continue;
          }
          if (!r.ok) throw await failure(r);
          break;
        } catch (e) {
          if (signal?.aborted) throw aborted();
          // A refusal (bad request, forbidden, gone) will not get better; a network error or a 5xx might.
          const retryable = !(e instanceof ApiError) || e.status >= 500 || e.status === 408;
          if (!retryable || attempt >= 3) throw e;
          attempt++;
          await wait(500 * 2 ** attempt, signal);
        }
      }
      sent = b;
    }
    signal?.throwIfAborted();
    onProgress?.({ sent: size, total: size, chunk: ranges.length, chunks: ranges.length, retry: 0 });
    for (let attempt = 0; ; attempt++) {
      // The commit request itself is not aborted: once sent, the server decides; only the wait before it is.
      const r = await client.send("POST", `/sites/${siteId}/model/uploads/${start.upload_id}/commit`, {});
      if (r.ok) {
        const done = (await r.json()) as { model: SiteModel };
        return { ...done.model, transform: normalizeTransform(done.model.transform) };
      }
      if (r.status === 503 && attempt < 6) { await wait(retryAfter(r, 5) * 1000, signal); continue; }
      throw await failure(r);
    }
  } catch (e) {
    void cancelUpload(client, siteId, start.upload_id);
    throw e;
  }
}

// ---- cache ------------------------------------------------------------------------------------------------------

/** The last two models' bytes, keyed by site and revision (switching back to a site does not download again). */
const bytesCache = new Map<string, ArrayBuffer>();

export function cachedModel(siteId: string, revision: number): ArrayBuffer | undefined {
  return bytesCache.get(`${siteId}:${revision}`);
}

export function primeModelCache(siteId: string, revision: number, bytes: ArrayBuffer) {
  const key = `${siteId}:${revision}`;
  bytesCache.delete(key);
  bytesCache.set(key, bytes);
  while (bytesCache.size > 2) bytesCache.delete(bytesCache.keys().next().value as string);
}

/** Thai text for a failed model request; `step` says what was being done (a 400 means different things). */
export function modelError(e: unknown, step: "upload" | "transform" | "delete" = "upload"): string {
  if (e instanceof DOMException && e.name === "AbortError") return "ยกเลิกการอัปโหลดแล้ว";
  if (e instanceof ApiError) {
    if (e.status === 400) return step === "transform" ? "ค่าตำแหน่งอยู่นอกช่วงที่เซิร์ฟเวอร์รับ (มาตราส่วน 0.0001–10000, หมุน ±360°, ความทึบ 5–100%)" : "เซิร์ฟเวอร์ไม่รับไฟล์นี้ (ขนาด checksum หรือไม่ใช่ GLB v2) · ลองแปลงและอัปโหลดใหม่";
    if (e.status === 403) return "บัญชีนี้ไม่มีสิทธิ์จัดการโมเดลอาคาร (ต้องเป็น owner หรือ admin ที่แก้ผังอาคารได้)";
    if (e.status === 404) return step === "upload" ? "เซิร์ฟเวอร์นี้ยังไม่รองรับโมเดลอาคาร 3D หรือการอัปโหลดหมดอายุ (1 ชั่วโมง) · ลองใหม่" : "ไม่พบโมเดลหรืออาคารนี้ · อาจถูกลบไปแล้ว";
    if (e.status === 413) return "ไฟล์ใหญ่เกิน 32 MiB หลังแปลงเป็น GLB · ลดรายละเอียดหรือขนาดพื้นผิวก่อน";
    if (e.status === 409) return e.reason === "upload_in_progress" ? "มีการอัปโหลดโมเดลของอาคารนี้ค้างอยู่ (เริ่มไม่ถึง 1 ชั่วโมง) · ลองใหม่ภายหลัง หรือยกเลิกการอัปโหลดที่ค้างอยู่" : "workspace นี้มีการอัปโหลดโมเดลค้างหลายรายการ · ลองใหม่ภายหลัง";
    if (e.status === 429) return "ส่งคำขอถี่เกินไป · รอสักครู่แล้วลองใหม่";
    if (e.status === 503) return "เซิร์ฟเวอร์กำลังรับงานเต็ม · ลองใหม่ในอีกสักครู่";
    return e.message;
  }
  return e instanceof Error ? e.message : "ดำเนินการไม่สำเร็จ";
}
