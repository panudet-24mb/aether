"use client";
import { useEffect, useMemo, useState } from "react";
import { DraftingCompass, LoaderCircle, TriangleAlert } from "lucide-react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import {
  DxfCancelled, MAX_ITEMS, MAX_LAYOUT_BYTES, MAX_WALLS, MAX_ZONES, UNITS, UNIT_LABEL,
  buildImport, parseDxf, suggestTarget,
  type BuildResult, type DxfDoc, type DxfUnit, type LayerTarget,
} from "./dxf";
import { COLORS, clamp, round2, type Draft, type Item, type Wall, type Zone } from "./model";

type Applied = { draft: Draft; notice: string };

export default function DxfImportDialog({ file, token, draft, onClose, onApply }: {
  file: File | null;
  token: number;
  draft: Draft | null;
  onClose: () => void;
  onApply: (result: Applied) => void;
}) {
  return (
    <Dialog open={file !== null} onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent className="topo-dialog fp-dxf">
        <DialogHeader>
          <DialogTitle>นำเข้า CAD (DXF)</DialogTitle>
          <DialogDescription>
            รองรับ DXF แบบ ASCII ที่ CAD ทุกตัว export ได้ · ไฟล์ DWG ต้อง Save As / Export เป็น DXF ก่อน · ไฟล์ถูกอ่านในเบราว์เซอร์เท่านั้น ไม่มีการอัปโหลด
          </DialogDescription>
        </DialogHeader>
        {file && draft && <Reader key={token} file={file} draft={draft} onClose={onClose} onApply={onApply} />}
      </DialogContent>
    </Dialog>
  );
}

type Phase = { s: "reading"; pct: number } | { s: "error"; message: string } | { s: "ready"; doc: DxfDoc };

function Reader({ file, draft, onClose, onApply }: { file: File; draft: Draft; onClose: () => void; onApply: (result: Applied) => void }) {
  const [phase, setPhase] = useState<Phase>({ s: "reading", pct: 0 });

  useEffect(() => {
    const run = { stop: false };
    const read = async () => {
      try {
        if (file.name.toLowerCase().endsWith(".dwg")) throw new Error("DWG เป็นรูปแบบไบนารีปิดของ AutoCAD ซึ่งอ่านในเบราว์เซอร์ไม่ได้ · ไฟล์ DWG ต้อง Save As / Export เป็น DXF ก่อน");
        const text = await file.text();
        if (run.stop) return;
        const doc = await parseDxf(text, {
          onProgress: (pct) => { if (!run.stop) setPhase((cur) => (cur.s === "reading" ? { s: "reading", pct } : cur)); },
          shouldCancel: () => run.stop,
        });
        if (!run.stop) setPhase({ s: "ready", doc });
      } catch (e) {
        if (run.stop || e instanceof DxfCancelled) return;
        setPhase({ s: "error", message: e instanceof Error ? e.message : "อ่านไฟล์ไม่สำเร็จ" });
      }
    };
    void read();
    return () => { run.stop = true; };
  }, [file]);

  if (phase.s === "reading") {
    return (
      <div className="fp-dxf-wait">
        <p><LoaderCircle size={15} className="fp-spin" aria-hidden="true" /> กำลังอ่าน <strong>{file.name}</strong> ({(file.size / 1048576).toFixed(1)} MB)</p>
        <div className="fp-dxf-bar" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(phase.pct * 100)} aria-label="ความคืบหน้าในการอ่านไฟล์">
          <i style={{ width: `${Math.max(3, Math.round(phase.pct * 100))}%` }} />
        </div>
        <p className="topo-note">ไฟล์ใหญ่ใช้เวลาสักครู่ · หน้าจอยังใช้งานได้ระหว่างอ่าน</p>
        <div className="topo-dialog-actions"><button type="button" className="topo-btn" onClick={onClose}>ยกเลิก</button></div>
      </div>
    );
  }
  if (phase.s === "error") {
    return (
      <div className="fp-dxf-wait">
        <p className="fp-dxf-error"><TriangleAlert size={16} aria-hidden="true" /> <span>{phase.message}</span></p>
        <p className="topo-note">ใน CAD: เปิดแบบ → Save As → “AutoCAD DXF (*.dxf)” แล้วเลือกชนิด <strong>ASCII</strong> · ถ้าแบบเป็น 3 มิติ ให้ FLATTEN เป็น 2 มิติ และ PURGE เลเยอร์ที่ไม่ใช้ก่อน</p>
        <div className="topo-dialog-actions"><button type="button" className="topo-btn" onClick={onClose}>ปิด</button></div>
      </div>
    );
  }
  return <Configure doc={phase.doc} file={file} draft={draft} onClose={onClose} onApply={onApply} />;
}

const TARGET_LABEL: Record<LayerTarget, string> = { wall: "ผนัง", zone: "โซน (เส้นปิด)", skip: "ข้าม" };
const PREVIEW_LIMIT = 1500;

function Configure({ doc, file, draft, onClose, onApply }: { doc: DxfDoc; file: File; draft: Draft; onClose: () => void; onApply: (result: Applied) => void }) {
  const [unit, setUnit] = useState<DxfUnit>(doc.unit);
  const [targets, setTargets] = useState<Record<string, LayerTarget>>(() => Object.fromEntries(doc.layers.map((l) => [l.name, suggestTarget(l)])));
  const [suggested] = useState<Record<string, LayerTarget>>(() => Object.fromEntries(doc.layers.map((l) => [l.name, suggestTarget(l) === "skip" ? "wall" : suggestTarget(l)])));
  const [thickness, setThickness] = useState(0.15);
  const [toleranceCm, setToleranceCm] = useState(2);
  const [minLengthCm, setMinLengthCm] = useState(0);
  const [resize, setResize] = useState(true);
  const [replace, setReplace] = useState(true);

  const built = useMemo(
    () => buildImport(doc, { unit, targets, thickness, tolerance: toleranceCm / 100, minLength: minLengthCm / 100, margin: 1 }),
    [doc, unit, targets, thickness, toleranceCm, minLengthCm],
  );

  const keptWalls = replace ? 0 : draft.layout.walls.length;
  const totals = { walls: keptWalls + built.walls.length, zones: draft.layout.zones.length + built.zones.length, items: draft.layout.items.length + built.items.length };
  const keptBytes = replace ? byteLen(JSON.stringify({ zones: draft.layout.zones, items: draft.layout.items })) : byteLen(JSON.stringify(draft.layout));
  const bytes = built.bytes + keptBytes;
  const blocked = built.overLimit || totals.walls > MAX_WALLS || totals.zones > MAX_ZONES || totals.items > MAX_ITEMS || bytes > MAX_LAYOUT_BYTES || (!built.walls.length && !built.zones.length && !built.items.length);

  const mergeWarnings: string[] = [];
  if (!built.overLimit && totals.walls > MAX_WALLS) mergeWarnings.push(`รวมกับผนังเดิมแล้วได้ ${totals.walls} เส้น เกิน ${MAX_WALLS} · เลือก “แทนที่ผนังเดิม” หรือเพิ่มค่าลดรายละเอียด`);
  if (totals.zones > MAX_ZONES) mergeWarnings.push(`รวมกับโซนเดิมแล้วได้ ${totals.zones} โซน เกิน ${MAX_ZONES}`);
  if (totals.items > MAX_ITEMS) mergeWarnings.push(`รวมกับของเดิมแล้วได้ ${totals.items} ชิ้น เกิน ${MAX_ITEMS}`);
  if (!built.overLimit && bytes > MAX_LAYOUT_BYTES) mergeWarnings.push(`รวมกับผังเดิมแล้ว ${(bytes / 1024).toFixed(0)} KB เกิน ${MAX_LAYOUT_BYTES / 1024} KB`);
  const warnings = [...built.warnings, ...mergeWarnings];

  const setTarget = (layer: string, target: LayerTarget) => setTargets((all) => ({ ...all, [layer]: target }));
  const coarser = () => { setToleranceCm((v) => Math.min(100, Math.max(2, Math.round(v * 2)))); setMinLengthCm((v) => Math.min(500, Math.max(20, v * 2))); };

  const apply = () => {
    const walls = replace ? [] : draft.layout.walls;
    const used = new Set<string>([...walls.map((w) => w.id), ...draft.layout.zones.map((z) => z.id), ...draft.layout.items.map((i) => i.id)]);
    const fresh = (id: string) => { let out = id, n = 0; while (used.has(out)) out = `${id}x${(n++).toString(36)}`; used.add(out); return out; };
    const next: Draft = {
      ...draft,
      width_m: resize ? clamp(round2(built.width + 2), 2, 1000) : draft.width_m,
      depth_m: resize ? clamp(round2(built.depth + 2), 2, 1000) : draft.depth_m,
      layout: {
        ...draft.layout,
        walls: [...walls, ...built.walls.map((w): Wall => ({ ...w, id: fresh(w.id) }))],
        zones: [...draft.layout.zones, ...built.zones.map((z): Zone => ({ ...z, id: fresh(z.id) }))],
        items: [...draft.layout.items, ...built.items.map((i): Item => ({ ...i, id: fresh(i.id) }))],
      },
    };
    onApply({ draft: next, notice: `นำเข้า DXF แล้ว · ผนัง ${built.walls.length} เส้น · โซน ${built.zones.length} · ข้อความ ${built.items.length} · กด Ctrl/⌘ Z เพื่อย้อนกลับได้` });
  };

  return (
    <div className="fp-dxf-body">
      <div className="fp-dxf-file">
        <strong>{file.name}</strong>
        <small>{(file.size / 1024).toFixed(0)} KB · {doc.entities.toLocaleString()} entity · {doc.layers.length} เลเยอร์{doc.blocks ? ` · ${doc.blocks} block` : ""}</small>
      </div>

      <div className="topo-form-grid">
        <label>หน่วยของแบบ
          <select value={unit} onChange={(e) => setUnit(e.target.value as DxfUnit)}>
            {UNITS.map((u) => <option key={u} value={u}>{UNIT_LABEL[u]}</option>)}
          </select>
          <small className="fp-dxf-hint">{doc.unitFromHeader ? "อ่านจาก $INSUNITS ในไฟล์" : "ไฟล์ไม่ระบุหน่วย · เดาจากขนาดแบบ โปรดตรวจสอบ"} · ได้ขนาด {built.width.toFixed(2)} × {built.depth.toFixed(2)} ม.</small>
        </label>
        <label>ความหนาผนัง (ม.)
          <input type="number" min={0.05} max={1} step={0.01} value={thickness} onChange={(e) => setThickness(clampNum(e.target.value, 0.05, 1, 0.15))} />
          <small className="fp-dxf-hint">DXF ไม่มีความหนา · ใช้ค่านี้กับผนังทุกเส้น</small>
        </label>
        <label>ลดรายละเอียดเส้นโค้ง (ซม.)
          <input type="number" min={0} max={100} step={0.5} value={toleranceCm} onChange={(e) => setToleranceCm(clampNum(e.target.value, 0, 100, 2))} />
          <small className="fp-dxf-hint">ยิ่งมาก จุดยิ่งน้อยและไฟล์ยิ่งเล็ก</small>
        </label>
        <label>ตัดเส้นที่สั้นกว่า (ซม.)
          <input type="number" min={0} max={500} step={5} value={minLengthCm} onChange={(e) => setMinLengthCm(clampNum(e.target.value, 0, 500, 0))} />
          <small className="fp-dxf-hint">0 = เก็บทุกเส้น · ใช้กำจัดเศษเส้นจากแบบที่รก</small>
        </label>
      </div>

      <div className="fp-dxf-layers" role="group" aria-label="เลเยอร์ที่จะนำเข้า">
        {doc.layers.map((l) => {
          const target = targets[l.name] ?? "skip";
          const kinds = Object.entries(l.kinds).map(([k, n]) => `${k} ${n}`).join(" · ");
          return (
            <div key={l.name} className={`fp-dxf-layer ${target === "skip" ? "is-off" : ""}`}>
              <label>
                <input type="checkbox" checked={target !== "skip"} onChange={(e) => setTarget(l.name, e.target.checked ? suggested[l.name] ?? "wall" : "skip")} />
                <span><strong>{l.name}</strong><small>{kinds || "ว่าง"}</small></span>
              </label>
              <select aria-label={`สิ่งที่จะสร้างจากเลเยอร์ ${l.name}`} value={target} onChange={(e) => setTarget(l.name, e.target.value as LayerTarget)}>
                {(["wall", "zone", "skip"] as LayerTarget[]).map((t) => <option key={t} value={t}>{TARGET_LABEL[t]}</option>)}
              </select>
            </div>
          );
        })}
      </div>
      {doc.layers.some((l) => l.closed > 0 && targets[l.name] === "zone") && <p className="fp-dxf-hint">สร้างโซนจากเส้นปิดในเลเยอร์ที่ตั้งเป็น “โซน” · ชื่อโซนมาจากข้อความที่อยู่ในรูปนั้น</p>}

      <Preview built={built} />

      <p className="fp-dxf-sum">
        จะได้ <strong>ผนัง {built.walls.length}</strong> เส้น · <strong>โซน {built.zones.length}</strong> · <strong>ข้อความ {built.items.length}</strong> · {built.points.toLocaleString()} จุด · {(bytes / 1024).toFixed(1)} KB จาก {MAX_LAYOUT_BYTES / 1024} KB
        {skipSummary(doc, built) && <> · ข้าม {skipSummary(doc, built)}</>}
      </p>
      {warnings.length > 0 && (
        <div className="fp-dxf-warn" role="alert">
          <TriangleAlert size={15} aria-hidden="true" />
          <span>
            {warnings.map((w) => <span key={w}>{w}</span>)}
            <button type="button" className="fp-dxf-mini" onClick={coarser}>ลดรายละเอียดลงอีก (เป็น {Math.min(100, Math.max(2, Math.round(toleranceCm * 2)))} ซม. / ตัดเส้นสั้นกว่า {Math.min(500, Math.max(20, minLengthCm * 2))} ซม.)</button>
          </span>
        </div>
      )}

      <label className="topo-check">
        <input type="checkbox" checked={resize} onChange={(e) => setResize(e.target.checked)} />
        <span><strong>ปรับขนาดชั้นให้พอดีกับแบบ</strong><small>ตั้งขนาดชั้นเป็น {clamp(round2(built.width + 2), 2, 1000)} × {clamp(round2(built.depth + 2), 2, 1000)} ม. (เผื่อขอบข้างละ 1 ม.) · เดิม {draft.width_m} × {draft.depth_m} ม.</small></span>
      </label>
      <label className="topo-check">
        <input type="checkbox" checked={replace} onChange={(e) => setReplace(e.target.checked)} />
        <span><strong>แทนที่ผนังเดิมของชั้นนี้</strong><small>{replace ? `ผนังเดิม ${draft.layout.walls.length} เส้นจะถูกลบ` : `เพิ่มต่อจากผนังเดิม ${draft.layout.walls.length} เส้น`} · โซนและอุปกรณ์เดิมไม่ถูกแตะ · ย้อนกลับได้ด้วย Ctrl/⌘ Z</small></span>
      </label>

      <div className="topo-dialog-actions">
        <button type="button" className="topo-btn" onClick={onClose}>ยกเลิก</button>
        <button type="button" className="topo-btn primary" disabled={blocked} onClick={apply}><DraftingCompass size={15} /> นำเข้าลงชั้นนี้</button>
      </div>
    </div>
  );
}

function Preview({ built }: { built: BuildResult }) {
  const vw = Math.max(1, built.width + 2), vh = Math.max(1, built.depth + 2);
  const walls = built.walls.slice(0, PREVIEW_LIMIT);
  const zones = built.zones.slice(0, PREVIEW_LIMIT);
  return (
    <svg className="fp-dxf-preview" viewBox={`0 0 ${vw} ${vh}`} preserveAspectRatio="xMidYMid meet" role="img" aria-label={`ตัวอย่างผัง ${built.width.toFixed(1)} คูณ ${built.depth.toFixed(1)} เมตร`}>
      <rect x={0} y={0} width={vw} height={vh} className="fp-dxf-sheet" />
      {zones.map((z) => <polygon key={z.id} className="fp-dxf-zone" points={z.points.map((p) => `${p[0]},${p[1]}`).join(" ")} style={{ fill: COLORS[z.color], stroke: COLORS[z.color] }} />)}
      {walls.map((w) => (w.closed
        ? <polygon key={w.id} className="fp-dxf-wall" points={w.points.map((p) => `${p[0]},${p[1]}`).join(" ")} />
        : <polyline key={w.id} className="fp-dxf-wall" points={w.points.map((p) => `${p[0]},${p[1]}`).join(" ")} />))}
      {built.items.slice(0, PREVIEW_LIMIT).map((i) => <circle key={i.id} className="fp-dxf-mark" cx={i.x + i.w / 2} cy={i.y + i.h / 2} r={Math.max(0.12, vw / 300)} />)}
    </svg>
  );
}

function skipSummary(doc: DxfDoc, built: BuildResult): string {
  const parts = Object.entries(doc.skipped).sort((a, b) => b[1] - a[1]).slice(0, 4).map(([type, n]) => `${type} ${n}`);
  const dropped = built.dropped.short + built.dropped.tiny + built.dropped.duplicate;
  if (dropped) parts.push(`เส้นสั้น/ซ้ำ ${dropped}`);
  return parts.join(" · ");
}

const clampNum = (raw: string, lo: number, hi: number, fallback: number) => { const n = Number(raw); return Number.isFinite(n) ? clamp(n, lo, hi) : fallback; };

function byteLen(s: string): number {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.codePointAt(i)!;
    n += c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4;
    if (c >= 0x10000) i++;
  }
  return n;
}
