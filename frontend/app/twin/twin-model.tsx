"use client";
// The building model panel (docs/platform/digital-twin.md, "Immersive view and building model"): import a CAD
// export, convert it to GLB in the browser, upload it in chunks, then align it to the floor plans with a live
// preview. Only owners and admins who may edit the plans can upload, save or delete; the server enforces it.

import { RotateCw, Scan, Trash2, Undo2, Upload, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import type { Site } from "../floorplan/model";
import {
  ACCEPT, MAX_MODEL_BYTES, cancelUpload, deleteModel, modelError, normalizeTransform, primeModelCache, saveTransform, sameTransform, sha256Hex, stuckUpload, uploadModel,
  type ImportPhase, type ModelClient, type ModelTransform, type SiteModel,
} from "./model-api";
import type { ModelStatus } from "./twin-view";

const PHASE: Record<ImportPhase | "upload" | "commit", string> = {
  read: "กำลังอ่านไฟล์…",
  parse: "กำลังอ่านโมเดล…",
  flatten: "กำลังจัดโครงสร้างโมเดล (รวมชิ้นส่วนตามวัสดุ)…",
  export: "กำลังแปลงเป็น GLB…",
  checksum: "กำลังคำนวณ checksum…",
  upload: "กำลังอัปโหลด",
  commit: "กำลังตรวจสอบไฟล์บนเซิร์ฟเวอร์…",
};

const FORMAT: Record<string, string> = { glb: "GLB", gltf: "glTF", obj: "OBJ", fbx: "FBX", stl: "STL", "3dm": "Rhino 3DM", ifc: "IFC" };

/** Units a CAD file is often drawn in: picking one sets the scale to metres. */
const UNITS: { label: string; scale: number }[] = [
  { label: "เมตร", scale: 1 },
  { label: "เซนติเมตร", scale: 0.01 },
  { label: "มิลลิเมตร", scale: 0.001 },
  { label: "นิ้ว", scale: 0.0254 },
  { label: "ฟุต", scale: 0.3048 },
];

const mib = (n: number) => `${(n / 1048576).toFixed(n < 10 * 1048576 ? 1 : 0)} MiB`;
const num = (v: string, d: number) => { const n = Number(v); return Number.isFinite(n) ? n : d; };

type Busy = { phase: ImportPhase | "upload" | "commit"; pct?: number; retry?: number; wait?: number } | null;

export default function TwinModelPanel({ client, siteId, site, model, status, canManage, explode, draft, onDraft, fit, onChanged, onClose }: {
  client: ModelClient;
  siteId: string;
  site: Site | null;
  model: SiteModel | null;
  status: ModelStatus;
  /** null: unknown (the page could not read the role); the controls show and a refusal is explained. */
  canManage: boolean | null;
  explode: number;
  draft: ModelTransform | null;
  onDraft: (t: ModelTransform | null) => void;
  fit: (t: ModelTransform) => Pick<ModelTransform, "scale" | "offset_m"> | null;
  onChanged: (m: SiteModel | null) => void;
  onClose: () => void;
}) {
  const [busy, setBusy] = useState<Busy>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [warnings, setWarnings] = useState<string[]>([]);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [saving, setSaving] = useState(false);
  const [fitAfter, setFitAfter] = useState<number | null>(null);
  /** An unfinished upload (another member's or an earlier tab's) that blocks a new one: a manager may cancel it. */
  const [stuck, setStuck] = useState<string | null>(null);
  /** The scale as typed; applied on blur or Enter (a half-typed "0.0" must not become the minimum). */
  const [scaleText, setScaleText] = useState<string | null>(null);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; abortRef.current?.abort(); }, []);

  const saved = model?.transform ?? null;
  const t = draft ?? saved;
  const dirty = !!(draft && saved && !sameTransform(draft, saved));
  const manage = canManage !== false;
  const edit = (patch: Partial<ModelTransform>) => { if (t) onDraft(normalizeTransform({ ...t, ...patch })); };

  // After an upload the model is fitted to the plans once it has loaded (a preview: saving is the user's call).
  useEffect(() => {
    if (fitAfter === null || !model || model.revision !== fitAfter || status !== "ready") return;
    const id = requestAnimationFrame(() => {
      setFitAfter(null);
      const base = normalizeTransform(model.transform);
      const f = fit(base);
      // A first model starts see-through, so the heat map and devices inside stay visible; the user can raise it.
      const first = base.opacity === 1 && base.offset_m.every((v) => v === 0) && base.scale === 1;
      if (f) { onDraft(normalizeTransform({ ...base, ...f, ...(first ? { opacity: 0.6 } : {}) })); setNotice("ปรับขนาดและตำแหน่งให้พอดีกับผังแล้ว · ตรวจดูแล้วกด บันทึกตำแหน่ง"); }
    });
    return () => cancelAnimationFrame(id);
  }, [fitAfter, model, status, fit, onDraft]);

  async function onFiles(list: FileList | null) {
    const files = list ? Array.from(list) : [];
    if (fileRef.current) fileRef.current.value = "";
    if (!files.length) return;
    setError(""); setNotice(""); setWarnings([]); setConfirmDelete(false); setStuck(null);
    setBusy({ phase: "read" });
    try {
      const { convertToGlb } = await import("./model-import");
      const conv = await convertToGlb(files, (phase) => { if (alive.current) setBusy({ phase }); });
      if (!alive.current) return;
      setWarnings(conv.warnings);
      setBusy({ phase: "checksum" });
      const sha256 = await sha256Hex(conv.glb);
      const ac = new AbortController();
      abortRef.current = ac;
      const stored = await uploadModel(client, siteId, { name: conv.name, source_format: conv.source_format, glb: conv.glb, sha256 }, (p) => {
        if (!alive.current) return;
        setBusy(p.chunk >= p.chunks ? { phase: "commit" } : { phase: "upload", pct: p.total ? p.sent / p.total : 0, retry: p.retry, wait: p.wait });
      }, ac.signal);
      // The view shows the new model from these bytes; it does not download what it just uploaded.
      primeModelCache(siteId, stored.revision, conv.glb);
      if (!alive.current) return;
      setNotice(`อัปโหลดแล้ว · ${mib(conv.glb.byteLength)} · ${conv.triangles.toLocaleString("th-TH")} สามเหลี่ยม · ${conv.meshes} ชิ้น`);
      setFitAfter(stored.revision);
      onDraft(null);
      onChanged(stored);
    } catch (e) {
      if (!alive.current) return;
      setError(modelError(e));
      setStuck(stuckUpload(e)?.uploadId ?? null);
    } finally {
      abortRef.current = null;
      if (alive.current) setBusy(null);
    }
  }

  async function save() {
    if (!draft) return;
    setSaving(true); setError(""); setNotice("");
    try {
      const m = await saveTransform(client, siteId, draft);
      if (!alive.current) return;
      onDraft(null);
      onChanged(m);
      setNotice("บันทึกตำแหน่งแล้ว");
    } catch (e) {
      if (alive.current) setError(modelError(e, "transform"));
    } finally {
      if (alive.current) setSaving(false);
    }
  }

  async function remove() {
    setSaving(true); setError(""); setNotice("");
    try {
      await deleteModel(client, siteId);
      if (!alive.current) return;
      setConfirmDelete(false);
      onDraft(null);
      onChanged(null);
      setNotice("ลบโมเดลอาคารแล้ว");
    } catch (e) {
      if (alive.current) setError(modelError(e, "delete"));
    } finally {
      if (alive.current) setSaving(false);
    }
  }

  const fitNow = () => {
    if (!t) return;
    const f = fit(t);
    if (f) { edit(f); setNotice("ปรับให้พอดีกับผังแล้ว (ยังไม่บันทึก)"); }
    else setError("ยังคำนวณไม่ได้ · รอให้โมเดลโหลดเสร็จ หรือผังยังไม่มีขนาด");
  };

  const footprint = site?.floors.length ? `${Math.max(...site.floors.map((f) => f.width_m)).toFixed(1)} × ${Math.max(...site.floors.map((f) => f.depth_m)).toFixed(1)} ม.` : "";
  const unit = t ? UNITS.find((u) => Math.abs(u.scale - t.scale) < 1e-9) : undefined;

  return (
    <section className="twin-panel twin-modelpanel" aria-label="โมเดลอาคาร 3 มิติ">
      <header className="twin-panel-head">
        <strong>อาคาร 3D</strong>
        <button type="button" className="twin-icon-btn" onClick={() => { onDraft(null); onClose(); }} aria-label="ปิดแผงโมเดลอาคาร" title={dirty ? "ปิด (ตำแหน่งที่ยังไม่บันทึกจะกลับเป็นค่าเดิม)" : "ปิด"}><X size={14} /></button>
      </header>

      {model ? (
        <div className="twin-model-summary">
          <span className="twin-model-name">{model.name}</span>
          <small>{FORMAT[model.source_format] ?? model.source_format} → GLB · {mib(model.size)} · ฉบับที่ {model.revision} · {new Date(model.uploaded_at).toLocaleString("th-TH", { dateStyle: "medium", timeStyle: "short" })}</small>
          <small className={`twin-model-status is-${status}`}>{status === "loading" ? "กำลังโหลดโมเดล…" : status === "error" ? "แสดงโมเดลไม่ได้ · ลองอัปโหลดใหม่" : status === "ready" ? "แสดงอยู่ในฉาก" : ""}</small>
        </div>
      ) : (
        <p className="twin-panel-note">ยังไม่มีโมเดลอาคาร · นำเข้าโมเดล 3 มิติจากโปรแกรม CAD เพื่อแสดงอาคารจริงซ้อนกับผัง อุปกรณ์ และแผนที่ความร้อน</p>
      )}

      {manage && (
        <div className="twin-model-import">
          <input ref={fileRef} type="file" multiple accept={ACCEPT} hidden onChange={(e) => void onFiles(e.target.files)} />
          <button type="button" className="topo-btn" disabled={!!busy || saving} onClick={() => fileRef.current?.click()}><Upload size={14} />{model ? "แทนที่ด้วยไฟล์ใหม่…" : "เลือกไฟล์โมเดล…"}</button>
          <small>รองรับ .glb · .gltf (เลือกพร้อม .bin และรูปพื้นผิว) · .obj (+ .mtl) · .fbx · .stl · แปลงเป็น GLB ในเบราว์เซอร์ ไม่เกิน {MAX_MODEL_BYTES / 1048576} MiB · แนะนำไม่เกิน 1.5 ล้านสามเหลี่ยม</small>
          <details className="twin-model-help">
            <summary>มีไฟล์ DWG, IFC, Revit หรือ SketchUp?</summary>
            <p>ส่งออกเป็น GLB, OBJ หรือ FBX ก่อน: AutoCAD (3D) → Export → FBX · Revit → Export → FBX หรือ OBJ · SketchUp → Export → 3D Model (GLB/FBX/OBJ) · Rhino → Export → GLB/OBJ · Blender → Export → glTF 2.0 (.glb). ไฟล์ IFC และ .3dm ยังไม่รองรับโดยตรงในรุ่นนี้ · DWG/DXF แบบ 2 มิติใช้กับหน้า &ldquo;ผังอาคาร&rdquo;</p>
          </details>
          {busy && (
            <div className="twin-model-progress" role="status">
              <span>{PHASE[busy.phase]}{busy.phase === "upload" && busy.pct !== undefined ? ` ${Math.round(busy.pct * 100)}%` : ""}{busy.retry ? ` · ลองส่งใหม่ครั้งที่ ${busy.retry}` : ""}{busy.wait ? ` · รอคิวอัปโหลด… (${busy.wait} วินาที)` : ""}</span>
              <div className="twin-model-bar" role="progressbar" aria-label="ความคืบหน้าการนำเข้าโมเดล" aria-valuemin={0} aria-valuemax={100} aria-valuenow={busy.phase === "upload" ? Math.round((busy.pct ?? 0) * 100) : undefined}>
                <i style={busy.phase === "upload" ? { width: `${Math.round((busy.pct ?? 0) * 100)}%` } : undefined} className={busy.phase === "upload" ? "" : "is-indeterminate"} />
              </div>
              {busy.phase === "upload" && <button type="button" className="topo-btn" onClick={() => abortRef.current?.abort()}>ยกเลิก</button>}
            </div>
          )}
        </div>
      )}
      {canManage === false && <p className="twin-panel-note">อัปโหลด บันทึกตำแหน่ง หรือลบโมเดลได้เฉพาะ owner หรือ admin ที่แก้ผังอาคารได้ · ปรับการแสดงผลด้านล่างได้บนเครื่องนี้ (ไม่บันทึก)</p>}

      {warnings.map((w) => <p key={w} className="twin-panel-warn">{w}</p>)}
      {error && <p className="twin-panel-error" role="alert">{error}</p>}
      {stuck && manage && !busy && (
        <div className="twin-model-row">
          <button type="button" className="topo-btn danger" disabled={saving} onClick={() => void (async () => {
            setSaving(true);
            const ok = await cancelUpload(client, siteId, stuck);
            if (!alive.current) return;
            setSaving(false);
            setStuck(null);
            setError(ok ? "" : "ยกเลิกการอัปโหลดที่ค้างอยู่ไม่สำเร็จ · ลองใหม่ภายหลัง");
            if (ok) setNotice("ยกเลิกการอัปโหลดที่ค้างอยู่แล้ว · เลือกไฟล์เพื่ออัปโหลดใหม่ได้");
          })()}>ยกเลิกการอัปโหลดที่ค้างอยู่</button>
        </div>
      )}
      {notice && <p className="twin-panel-ok" role="status">{notice}</p>}

      {model && t && (
        <div className="twin-model-align" aria-label="จัดตำแหน่งโมเดลกับผัง">
          <div className="twin-model-row">
            <button type="button" className="topo-btn" disabled={status !== "ready"} onClick={fitNow} title={footprint ? `ผัง ${footprint}` : undefined}><Scan size={14} />ปรับให้พอดีกับผัง</button>
            <button type="button" className="topo-btn" onClick={() => edit({ rotation_deg: [t.rotation_deg[0], t.rotation_deg[1] + 90, t.rotation_deg[2]] })} aria-label="หมุนรอบแกนตั้ง 90 องศา" title="หมุน 90°"><RotateCw size={14} />90°</button>
          </div>
          {explode > 0.01 && <small className="twin-panel-note">ระยะห่างชั้นไม่เป็น 0 · โมเดลจะตรงกับชั้นล่างสุดเท่านั้น (ตั้งระยะห่างชั้นเป็น 0 เพื่อเทียบทุกชั้น)</small>}

          <label className="twin-field"><span>มาตราส่วน</span>
            <input type="number" min={0.0001} max={10000} step="any" value={scaleText ?? String(t.scale)} aria-label="มาตราส่วน (หน่วยในไฟล์ต่อเมตร)"
              onChange={(e) => setScaleText(e.target.value)}
              onBlur={() => { if (scaleText !== null) { const n = Number(scaleText); if (scaleText.trim() && Number.isFinite(n) && n > 0) edit({ scale: n }); setScaleText(null); } }}
              onKeyDown={(e) => { if (e.key === "Enter") (e.target as HTMLInputElement).blur(); if (e.key === "Escape") setScaleText(null); }} />
            <select className="twin-mini" aria-label="หน่วยของไฟล์" value={unit ? String(unit.scale) : ""} onChange={(e) => { if (e.target.value) edit({ scale: Number(e.target.value) }); }}>
              <option value="">หน่วยในไฟล์…</option>
              {UNITS.map((u) => <option key={u.label} value={String(u.scale)}>{u.label}</option>)}
            </select>
          </label>

          <div className="twin-field"><span>แกนตั้ง</span>
            <div className="twin-seg" role="group" aria-label="แกนตั้งของโมเดล">
              {(["y", "z"] as const).map((a) => <button key={a} type="button" className={t.up === a ? "is-on" : ""} aria-pressed={t.up === a} onClick={() => edit({ up: a })}>{a.toUpperCase()} ขึ้น</button>)}
            </div>
          </div>

          <fieldset className="twin-xyz"><legend>หมุน (องศา)</legend>
            {(["X", "Y", "Z"] as const).map((axis, i) => (
              <label key={axis}><span>{axis}</span><input type="number" step={1} min={-180} max={180} value={Math.round(t.rotation_deg[i] * 10) / 10} aria-label={`หมุนรอบแกน ${axis} (องศา)`}
                onChange={(e) => { const r = [...t.rotation_deg] as [number, number, number]; r[i] = num(e.target.value, r[i]); edit({ rotation_deg: r }); }} /></label>
            ))}
            <input type="range" min={-180} max={180} step={1} value={t.rotation_deg[1]} aria-label="หมุนรอบแกนตั้ง" onChange={(e) => edit({ rotation_deg: [t.rotation_deg[0], Number(e.target.value), t.rotation_deg[2]] })} />
          </fieldset>

          <fieldset className="twin-xyz"><legend>เลื่อน (เมตร)</legend>
            {(["X ตะวันออก", "Y สูง", "Z ใต้"] as const).map((axis, i) => (
              <label key={axis}><span>{axis.split(" ")[0]}</span><input type="number" step={0.1} value={Math.round(t.offset_m[i] * 1000) / 1000} aria-label={`เลื่อนแกน ${axis} (เมตร)`}
                onChange={(e) => { const o = [...t.offset_m] as [number, number, number]; o[i] = num(e.target.value, o[i]); edit({ offset_m: o }); }} /></label>
            ))}
          </fieldset>

          <label className="twin-field"><span>ความทึบ</span>
            <input type="range" min={5} max={100} step={1} value={Math.round(t.opacity * 100)} onChange={(e) => edit({ opacity: Number(e.target.value) / 100 })} />
            <output>{Math.round(t.opacity * 100)}%</output>
          </label>

          <div className="twin-field"><span>การแสดง</span>
            <div className="twin-seg" role="group" aria-label="วิธีแสดงโมเดล">
              <button type="button" className={t.mode === "overlay" ? "is-on" : ""} aria-pressed={t.mode === "overlay"} onClick={() => edit({ mode: "overlay" })} title="โมเดลซ้อนกับผนังกระจกที่สร้างจากผัง">ซ้อนกับผัง</button>
              <button type="button" className={t.mode === "replace" ? "is-on" : ""} aria-pressed={t.mode === "replace"} onClick={() => edit({ mode: "replace" })} title="ซ่อนผนังที่สร้างจากผัง เหลือโมเดล อุปกรณ์ ความร้อน และคน">แทนผนังที่สร้าง</button>
            </div>
          </div>

          <label className="twin-check"><input type="checkbox" checked={t.visible} onChange={(e) => edit({ visible: e.target.checked })} />แสดงโมเดล</label>
          <label className="twin-check"><input type="checkbox" checked={t.clip_to_floor} onChange={(e) => edit({ clip_to_floor: e.target.checked })} />ตัดโมเดลเหนือเพดานชั้นที่เลือก (มองเข้าไปข้างใน)</label>

          {manage && (
            <div className="twin-model-row">
              <button type="button" className="topo-btn primary" disabled={!dirty || saving || !!busy} onClick={() => void save()}>{saving ? "กำลังบันทึก…" : "บันทึกตำแหน่ง"}</button>
              <button type="button" className="topo-btn" disabled={!dirty || saving} onClick={() => onDraft(null)}><Undo2 size={14} />คืนค่า</button>
              {!confirmDelete && <button type="button" className="topo-btn danger" disabled={saving || !!busy} onClick={() => setConfirmDelete(true)}><Trash2 size={14} />ลบโมเดล</button>}
            </div>
          )}
          {!manage && dirty && <div className="twin-model-row"><button type="button" className="topo-btn" onClick={() => onDraft(null)}><Undo2 size={14} />คืนค่า</button></div>}
          {confirmDelete && (
            <div className="twin-confirm" role="alertdialog" aria-label="ยืนยันการลบโมเดล">
              <span>ลบโมเดลอาคารนี้? ทุกคนจะไม่เห็นโมเดลนี้อีก และย้อนกลับไม่ได้</span>
              <div className="twin-model-row">
                <button type="button" className="topo-btn danger" disabled={saving} onClick={() => void remove()}>{saving ? "กำลังลบ…" : "ลบ"}</button>
                <button type="button" className="topo-btn" autoFocus onClick={() => setConfirmDelete(false)}>ยกเลิก</button>
              </div>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
