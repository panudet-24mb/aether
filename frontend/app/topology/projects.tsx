"use client";
import { useState } from "react";
import { Archive, FolderPlus, Layers, Pencil } from "lucide-react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import type { Project } from "./api";
import type { ProjectScope } from "./model";

export const PROJECT_COLORS: Record<string, string> = { mint: "#a7f3d0", blue: "#80b7ff", amber: "#f6c177", coral: "#ff8f70", violet: "#c4a7ff", slate: "#9fb1b6" };
const COLOR_LABEL: Record<string, string> = { mint: "เขียวมินต์", blue: "ฟ้า", amber: "เหลืองอำพัน", coral: "ส้มอิฐ", violet: "ม่วง", slate: "เทา" };
const utf8 = (s: string) => new TextEncoder().encode(s).length;

export type ProjectInput = { name: string; description: string; color: string };

export default function ProjectBar({
  projects,
  scope,
  unassigned,
  total,
  busy,
  error,
  onScope,
  onCreate,
  onUpdate,
  onArchive,
}: {
  projects: Project[];
  scope: ProjectScope;
  /** Gateways without a project / all gateways, for the chip counters. */
  unassigned: number;
  total: number;
  busy: boolean;
  error: string;
  onScope: (scope: ProjectScope) => void;
  onCreate: (input: ProjectInput) => Promise<boolean>;
  onUpdate: (id: string, input: ProjectInput) => Promise<boolean>;
  onArchive: (project: Project) => void;
}) {
  const [editor, setEditor] = useState<{ id?: string; name: string; description: string; color: string } | null>(null);
  const [archive, setArchive] = useState<Project | null>(null);
  const active = projects.find((p) => p.id === scope);
  const valid = editor !== null && editor.name.trim() !== "" && utf8(editor.name.trim()) <= 128 && utf8(editor.description) <= 500;

  return (
    <nav className="topo-projects" aria-label="โปรเจค">
      <span className="topo-projects-label">
        <Layers size={14} /> โปรเจค
      </span>
      <button type="button" className={`topo-project ${scope === "all" ? "is-on" : ""}`} aria-pressed={scope === "all"} onClick={() => onScope("all")} title="ดูทุกโปรเจคพร้อมกัน · โยงหรือย้ายอุปกรณ์ข้ามโปรเจคได้ในมุมมองนี้">
        ทั้งหมด <small>{total}</small>
      </button>
      {projects.map((p) => (
        <button key={p.id} type="button" className={`topo-project ${scope === p.id ? "is-on" : ""}`} aria-pressed={scope === p.id} onClick={() => onScope(p.id)} title={p.description || p.name}>
          <span className="topo-project-dot" style={{ background: PROJECT_COLORS[p.color] ?? PROJECT_COLORS.mint }} />
          {p.name} <small>{p.gateway_count}</small>
        </button>
      ))}
      {(unassigned > 0 || scope === "none") && (
        <button type="button" className={`topo-project ${scope === "none" ? "is-on" : ""}`} aria-pressed={scope === "none"} onClick={() => onScope("none")} title="gateway ที่ยังไม่อยู่ในโปรเจคใด">
          ยังไม่จัดโปรเจค <small>{unassigned}</small>
        </button>
      )}
      <span className="topo-projects-actions">
        {active && (
          <>
            <button type="button" className="topo-icon-btn" aria-label={`แก้ไขโปรเจค ${active.name}`} title="แก้ไขโปรเจค" onClick={() => setEditor({ id: active.id, name: active.name, description: active.description, color: active.color })}>
              <Pencil size={14} />
            </button>
            <button type="button" className="topo-icon-btn is-danger" aria-label={`เก็บโปรเจค ${active.name}`} title="เก็บโปรเจค (archive)" onClick={() => setArchive(active)}>
              <Archive size={14} />
            </button>
          </>
        )}
        <button type="button" className="topo-btn" onClick={() => setEditor({ name: "", description: "", color: "mint" })}>
          <FolderPlus size={15} /> โปรเจคใหม่
        </button>
      </span>

      <Dialog open={editor !== null} onOpenChange={(open) => !open && !busy && setEditor(null)}>
        <DialogContent className="topo-dialog">
          <DialogHeader>
            <DialogTitle>{editor?.id ? "แก้ไขโปรเจค" : "สร้างโปรเจค"}</DialogTitle>
            <DialogDescription>โปรเจคใช้แยกชุด gateway และอุปกรณ์ เช่น ตามสถานที่หรือตามงานของลูกค้า · gateway ย้ายข้ามโปรเจคได้ภายหลัง</DialogDescription>
          </DialogHeader>
          {editor && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                if (!valid) return;
                const input = { name: editor.name.trim(), description: editor.description.trim(), color: editor.color };
                void (editor.id ? onUpdate(editor.id, input) : onCreate(input)).then((ok) => ok && setEditor(null));
              }}
            >
              <label>
                ชื่อโปรเจค
                <input autoFocus required value={editor.name} onChange={(e) => setEditor({ ...editor, name: e.target.value })} placeholder="เช่น โรงพยาบาล A · อาคารผู้ป่วยใน" />
              </label>
              <label>
                รายละเอียด (ไม่บังคับ)
                <input value={editor.description} onChange={(e) => setEditor({ ...editor, description: e.target.value })} placeholder="สถานที่ ผู้รับผิดชอบ หรือหมายเหตุ" />
              </label>
              <fieldset className="topo-colors">
                <legend>สีประจำโปรเจค</legend>
                {Object.keys(PROJECT_COLORS).map((c) => (
                  <label key={c} className={editor.color === c ? "is-on" : ""} title={COLOR_LABEL[c]}>
                    <input type="radio" name="project-color" value={c} checked={editor.color === c} onChange={() => setEditor({ ...editor, color: c })} />
                    <span style={{ background: PROJECT_COLORS[c] }} aria-hidden="true" />
                    <span className="sr-only">{COLOR_LABEL[c]}</span>
                  </label>
                ))}
              </fieldset>
              {error && (
                <p className="topo-warn" role="alert">
                  {error}
                </p>
              )}
              <div className="topo-dialog-actions">
                <button type="button" className="topo-btn" disabled={busy} onClick={() => setEditor(null)}>
                  ยกเลิก
                </button>
                <button type="submit" className="topo-btn primary" disabled={busy || !valid}>
                  {busy ? "กำลังบันทึก…" : editor.id ? "บันทึก" : "สร้างโปรเจค"}
                </button>
              </div>
            </form>
          )}
        </DialogContent>
      </Dialog>

      <AlertDialog open={archive !== null} onOpenChange={(open) => !open && setArchive(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>เก็บโปรเจค “{archive?.name}”?</AlertDialogTitle>
            <AlertDialogDescription>โปรเจคจะหายจากรายการ · gateway และอุปกรณ์ในโปรเจคไม่ถูกลบ แต่จะกลับไปอยู่ใน “ยังไม่จัดโปรเจค” · Dashboard ที่ผูกกับโปรเจคนี้ยังใช้ได้</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction
              className="is-danger"
              onClick={() => {
                const p = archive!;
                setArchive(null);
                onArchive(p);
              }}
            >
              เก็บโปรเจค
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </nav>
  );
}
