"use client";
import { memo, useState } from "react";
import { NodeResizer, type Node, type NodeProps, type XYPosition } from "@xyflow/react";
import { Pencil, Trash2 } from "lucide-react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import type { TopologyFrame } from "./api";
import { PROJECT_COLORS } from "./projects";

// Frames: named rectangles an owner or admin draws on the canvas to group nodes (backend: migration 00052). A node
// belongs to a frame by where it sits, so moving a node in or out of the rectangle is all there is to grouping.

export const FRAME_MIN = 40;
/** Size assumed for a node React Flow has not measured yet. */
const NODE_SIZE = { width: 150, height: 110 };

export type FrameData = {
  name: string;
  /** Colour key (PROJECT_COLORS). */
  color: string;
  w: number;
  h: number;
  members: number;
  alerts: number;
  editable: boolean;
  /** Stable callbacks (the node cache compares data shallowly). */
  onEdit: (frameId: string) => void;
  onResized: (frameId: string, rect: { x: number; y: number; w: number; h: number }) => void;
};
export type FrameNode = Node<FrameData, "frame">;

export const frameNodeId = (id: string) => `frame:${id}`;

type Rect = { x: number; y: number; w: number; h: number };

/** Whether a node's centre lies inside a frame. */
export function inside(frame: Rect, position: XYPosition, size?: { width: number; height: number }): boolean {
  const s = size ?? NODE_SIZE;
  const cx = position.x + s.width / 2, cy = position.y + s.height / 2;
  return cx >= frame.x && cx <= frame.x + frame.w && cy >= frame.y && cy <= frame.y + frame.h;
}

/** Frames shown under a project filter: a project's own, or the ones without a (live) project under "unassigned". */
export function framesInScope(frames: TopologyFrame[], scope: string, liveProjects: Set<string>): TopologyFrame[] {
  if (scope === "all") return frames;
  if (scope === "none") return frames.filter((f) => !f.project_id || !liveProjects.has(f.project_id));
  return frames.filter((f) => f.project_id === scope);
}

export const FrameView = memo(function FrameView({ id, data, selected }: NodeProps<FrameNode>) {
  const tone = PROJECT_COLORS[data.color] ?? PROJECT_COLORS.slate;
  const ref = id.slice("frame:".length);
  return (
    <div className="topo-frame" style={{ width: data.w, height: data.h, ["--frame-tone" as string]: tone }}>
      <NodeResizer
        isVisible={data.editable && selected}
        minWidth={FRAME_MIN}
        minHeight={FRAME_MIN}
        color={tone}
        onResizeEnd={(_, p) => data.onResized(ref, { x: p.x, y: p.y, w: p.width, h: p.height })}
      />
      <div className="topo-frame-head" onDoubleClick={() => data.editable && data.onEdit(ref)} title={data.editable ? "ลากเพื่อย้ายกรอบพร้อมอุปกรณ์ข้างใน · ดับเบิลคลิกเพื่อแก้ไข" : data.name}>
        <strong>{data.name}</strong>
        <small>
          {data.members} อุปกรณ์{data.alerts > 0 && <b> · แจ้งเตือน {data.alerts}</b>}
        </small>
        {data.editable && (
          <button type="button" className="topo-frame-edit nodrag" aria-label={`แก้ไขกรอบ ${data.name}`} onClick={() => data.onEdit(ref)}>
            <Pencil size={12} />
          </button>
        )}
      </div>
    </div>
  );
});

export type FrameDraft = { id?: string; name: string; color: string; rect: Rect };

/** Create or edit a frame: name and colour; an existing frame can also be removed (its nodes stay). */
export function FrameDialog({ draft, busy, error, onClose, onSave, onDelete }: {
  draft: FrameDraft | null;
  busy: boolean;
  error: string;
  onClose: () => void;
  onSave: (draft: FrameDraft) => void;
  onDelete: (id: string) => void;
}) {
  return (
    <Dialog open={draft !== null} onOpenChange={(open) => !open && !busy && onClose()}>
      <DialogContent className="topo-dialog">
        {draft && <FrameForm key={draft.id ?? "new"} draft={draft} busy={busy} error={error} onClose={onClose} onSave={onSave} onDelete={onDelete} />}
      </DialogContent>
    </Dialog>
  );
}

function FrameForm({ draft, busy, error, onClose, onSave, onDelete }: { draft: FrameDraft; busy: boolean; error: string; onClose: () => void; onSave: (draft: FrameDraft) => void; onDelete: (id: string) => void }) {
  const [name, setName] = useState(draft.name);
  const [color, setColor] = useState(draft.color);
  const valid = name.trim() !== "" && new TextEncoder().encode(name.trim()).length <= 128;
  return (
    <>
      <DialogHeader>
        <DialogTitle>{draft.id ? "แก้ไขกรอบ" : "สร้างกรอบกลุ่ม"}</DialogTitle>
        <DialogDescription>กรอบใช้จัดกลุ่มอุปกรณ์บนกระดานให้ดูง่าย · อุปกรณ์ที่อยู่ในกรอบคือกลุ่มนั้น · ไม่มีผลกับข้อมูล การแจ้งเตือน หรือผังอาคาร</DialogDescription>
      </DialogHeader>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (valid) onSave({ ...draft, name: name.trim(), color });
        }}
      >
        <label>
          ชื่อกรอบ
          <input autoFocus required value={name} onChange={(e) => setName(e.target.value)} placeholder="เช่น ห้องน้ำชั้น M" />
        </label>
        <fieldset className="topo-colors">
          <legend>สี</legend>
          {Object.keys(PROJECT_COLORS).map((c) => (
            <label key={c} className={color === c ? "is-on" : ""}>
              <input type="radio" name="frame-color" value={c} checked={color === c} onChange={() => setColor(c)} />
              <span style={{ background: PROJECT_COLORS[c] }} aria-hidden="true" />
              <span className="sr-only">{c}</span>
            </label>
          ))}
        </fieldset>
        {error && <p className="topo-warn" role="alert">{error}</p>}
        <div className="topo-dialog-actions">
          {draft.id && (
            <button type="button" className="topo-btn danger topo-frame-delete" disabled={busy} onClick={() => onDelete(draft.id!)} title="ลบเฉพาะกรอบ · อุปกรณ์ข้างในไม่ถูกลบและอยู่ที่เดิม">
              <Trash2 size={14} /> ลบกรอบ
            </button>
          )}
          <button type="button" className="topo-btn" disabled={busy} onClick={onClose}>ยกเลิก</button>
          <button type="submit" className="topo-btn primary" disabled={busy || !valid}>{busy ? "กำลังบันทึก…" : draft.id ? "บันทึก" : "สร้างกรอบ"}</button>
        </div>
      </form>
    </>
  );
}
