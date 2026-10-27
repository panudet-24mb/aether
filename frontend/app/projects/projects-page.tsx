"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { Archive, FolderKanban, FolderPlus, Pencil, RefreshCw, Router, ShieldCheck } from "lucide-react";
import "../topology/topology.css";
import "../team/team.css";
import "./projects.css";
import { ApiError, createClientFrom, type Gateway, type Project } from "../topology/api";
import { GATEWAY_MODELS } from "../topology/catalog";
import { ArchiveProjectDialog, PROJECT_COLORS, ProjectEditor, newProjectDraft, type ProjectDraft, type ProjectInput } from "../topology/projects";
import { useLatest } from "../topology/use-latest";
import { fetchMe } from "../team/team";

const MAX_PROJECTS = 50;
const modelLabel = (id: string) => GATEWAY_MODELS.find((m) => m.id === id)?.label ?? id;
const date = (iso: string) => new Date(iso).toLocaleDateString("th-TH", { day: "numeric", month: "short", year: "numeric" });

/** Projects of the workspace in one place: create, rename, recolour, archive, and which project each gateway is in.
 *  The same operations as the project bar of the topology view; owner and admin only, as the API enforces. */
export default function ProjectsPage({ getToken, refresh, onUnauthorized, onOpenConnect, onOpenFloorplan }: {
  getToken: () => string;
  refresh: () => Promise<boolean>;
  onUnauthorized?: () => void;
  onOpenConnect: () => void;
  onOpenFloorplan: () => void;
}) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const [projects, setProjects] = useState<Project[] | null>(null);
  const [gateways, setGateways] = useState<Gateway[]>([]);
  const [readOnly, setReadOnly] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [dialogError, setDialogError] = useState("");
  const [editor, setEditor] = useState<ProjectDraft | null>(null);
  const [archive, setArchive] = useState<Project | null>(null);
  const unauthorized = useRef(false);

  const describe = useCallback(
    (e: unknown, fallback: string) => {
      if (e instanceof ApiError && e.status === 401 && !unauthorized.current) {
        unauthorized.current = true;
        onUnauthorized?.();
      }
      if (e instanceof ApiError && e.status === 409) return "มีโปรเจคชื่อนี้อยู่แล้ว หรือมีโปรเจคครบ 50 โปรเจคแล้ว";
      return e instanceof Error ? e.message : fallback;
    },
    [onUnauthorized],
  );

  const load = useCallback(async () => {
    try {
      const me = await fetchMe(client);
      if (me.role !== "owner" && me.role !== "admin") {
        setReadOnly(true);
        setProjects([]);
        return;
      }
      const [p, g] = await Promise.all([client.projects(), client.gateways()]);
      setReadOnly(false);
      setProjects(p);
      setGateways(g);
      setError("");
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) {
        setReadOnly(true);
        setProjects([]);
        return;
      }
      setError(describe(e, "โหลดรายการโปรเจคไม่สำเร็จ"));
    }
  }, [client, describe]);

  useEffect(() => {
    const t = setTimeout(() => void load(), 0);
    return () => clearTimeout(t);
  }, [load]);

  /** Runs one change, reloads, and reports. In a dialog the error stays in the dialog. */
  const run = async (work: () => Promise<string>, inDialog = false): Promise<boolean> => {
    setBusy(true);
    setDialogError("");
    try {
      const message = await work();
      await load();
      setNotice(message);
      setError("");
      return true;
    } catch (e) {
      const message = describe(e, "บันทึกไม่สำเร็จ");
      if (inDialog) setDialogError(message);
      else setError(message);
      return false;
    } finally {
      setBusy(false);
    }
  };

  const create = (input: ProjectInput) => run(async () => `สร้างโปรเจค “${(await client.createProject(input)).name}” แล้ว · ย้าย gateway เข้าโปรเจคได้ในตารางด้านล่าง`, true);
  const update = (id: string, input: ProjectInput) => run(async () => { await client.updateProject(id, input); return `บันทึกโปรเจค “${input.name}” แล้ว`; }, true);
  const archiveProject = (p: Project) => void run(async () => { await client.archiveProject(p.id); return `เก็บโปรเจค “${p.name}” แล้ว · gateway ของโปรเจคนี้กลับไปอยู่ใน “ยังไม่จัดโปรเจค”`; });
  const move = (g: Gateway, projectId: string | null) =>
    void run(async () => {
      await client.setGatewayProject(g.id, projectId);
      const to = projects?.find((p) => p.id === projectId)?.name;
      return to ? `ย้าย ${g.name} เข้าโปรเจค “${to}” แล้ว · อุปกรณ์ของ gateway นี้ย้ายตามไปด้วย` : `ย้าย ${g.name} ออกจากโปรเจคแล้ว`;
    });

  const full = (projects?.length ?? 0) >= MAX_PROJECTS;
  const unassigned = gateways.filter((g) => !g.project_id).length;

  return (
    <div className="tm pj">
      <div className="tm-bar">
        <h1 className="tm-title">โปรเจค</h1>
        {!readOnly && projects !== null && (
          <span className="tm-count">
            <FolderKanban size={13} /> <b>{projects.length}</b> โปรเจค · <b>{gateways.length}</b> gateway
          </span>
        )}
        <span className="tm-actions pj-actions">
          <button type="button" className="tm-icon-btn" onClick={() => void load()} disabled={busy} aria-label="โหลดใหม่" title="โหลดใหม่">
            <RefreshCw size={15} />
          </button>
          {!readOnly && (
            <button type="button" className="tm-btn primary" onClick={() => { setDialogError(""); setEditor(newProjectDraft()); }} disabled={busy || full} title={full ? `มีโปรเจคครบ ${MAX_PROJECTS} โปรเจคแล้ว · เก็บโปรเจคที่ไม่ใช้ก่อน` : undefined}>
              <FolderPlus size={15} /> สร้างโปรเจค
            </button>
          )}
        </span>
      </div>

      {error !== "" && (
        <p className="tm-alert is-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setError("")} aria-label="ปิดข้อความ">×</button>
        </p>
      )}
      {notice !== "" && (
        <p className="tm-alert is-notice" role="status">
          <span>{notice}</span>
          <button type="button" onClick={() => setNotice("")} aria-label="ปิดข้อความ">×</button>
        </p>
      )}

      <div className="tm-body">
        {readOnly ? (
          <div className="tm-empty">
            <ShieldCheck size={26} />
            <h2>บัญชีนี้จัดการโปรเจคไม่ได้</h2>
            <p>การสร้าง แก้ไข และเก็บโปรเจคสงวนไว้ให้เจ้าของ (owner) และผู้ดูแล (admin) ของ workspace</p>
          </div>
        ) : projects === null ? (
          <p className="tm-empty">กำลังโหลด…</p>
        ) : (
          <>
            <section className="pj-section" aria-labelledby="pj-projects">
              <h2 id="pj-projects">โปรเจคทั้งหมด</h2>
              {projects.length === 0 ? (
                <div className="tm-empty pj-first">
                  <FolderKanban size={28} />
                  <h2>ยังไม่มีโปรเจค</h2>
                  <p>โปรเจคใช้แยกชุด gateway อุปกรณ์ และผังอาคาร เช่น ตามสถานที่หรือตามลูกค้า · หน้าผังอาคารต้องมีโปรเจคก่อนจึงสร้างอาคารได้</p>
                  <button type="button" className="tm-btn primary" onClick={() => { setDialogError(""); setEditor(newProjectDraft()); }} disabled={busy}>
                    <FolderPlus size={15} /> สร้างโปรเจคแรก
                  </button>
                </div>
              ) : (
                <table className="tm-table">
                  <thead>
                    <tr>
                      <th scope="col">โปรเจค</th>
                      <th scope="col">รายละเอียด</th>
                      <th scope="col">gateway</th>
                      <th scope="col">สร้างเมื่อ</th>
                      <th scope="col" aria-label="การจัดการ" />
                    </tr>
                  </thead>
                  <tbody>
                    {projects.map((p) => (
                      <tr key={p.id}>
                        <td>
                          <span className="pj-name">
                            <i style={{ background: PROJECT_COLORS[p.color] ?? PROJECT_COLORS.mint }} aria-hidden="true" />
                            <strong>{p.name}</strong>
                          </span>
                        </td>
                        <td className="tm-dim pj-desc">{p.description || "–"}</td>
                        <td>{p.gateway_count}</td>
                        <td className="tm-dim">{date(p.created_at)}</td>
                        <td>
                          <span className="tm-row-actions">
                            <button type="button" className="tm-btn" disabled={busy} onClick={() => { setDialogError(""); setEditor({ id: p.id, name: p.name, description: p.description, color: p.color }); }}>
                              <Pencil size={14} /> แก้ไข
                            </button>
                            <button type="button" className="tm-btn danger" disabled={busy} onClick={() => setArchive(p)} title="เก็บโปรเจค (archive) · ไม่ลบข้อมูล">
                              <Archive size={14} /> เก็บ
                            </button>
                          </span>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </section>

            <section className="pj-section" aria-labelledby="pj-gateways">
              <h2 id="pj-gateways">
                gateway อยู่โปรเจคไหน
                {unassigned > 0 && <small className="tm-flag"> · ยังไม่จัดโปรเจค {unassigned} ตัว</small>}
              </h2>
              <p className="pj-hint">เปลี่ยนโปรเจคของ gateway ได้ที่นี่ · อุปกรณ์ที่ลงทะเบียนกับ gateway ย้ายตามไปด้วย · ผังอาคารของโปรเจคจะเห็นเฉพาะ gateway และอุปกรณ์ของโปรเจคนั้น</p>
              {gateways.length === 0 ? (
                <div className="tm-empty">
                  <Router size={24} />
                  <p>ยังไม่มี gateway · เพิ่มได้ที่หน้า <button type="button" className="pj-link" onClick={onOpenConnect}>เชื่อมต่ออุปกรณ์</button></p>
                </div>
              ) : (
                <table className="tm-table">
                  <thead>
                    <tr>
                      <th scope="col">gateway</th>
                      <th scope="col">รุ่น</th>
                      <th scope="col">โปรเจค</th>
                    </tr>
                  </thead>
                  <tbody>
                    {gateways.map((g) => (
                      <tr key={g.id}>
                        <td><strong className="pj-gw">{g.name}</strong></td>
                        <td className="tm-dim">{modelLabel(g.model)}</td>
                        <td>
                          <select className="tm-select pj-select" aria-label={`โปรเจคของ ${g.name}`} value={g.project_id ?? ""} disabled={busy} onChange={(e) => move(g, e.target.value || null)}>
                            <option value="">ยังไม่จัดโปรเจค</option>
                            {projects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                          </select>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </section>

            {projects.length > 0 && (
              <p className="pj-next">
                ขั้นต่อไป: <button type="button" className="pj-link" onClick={onOpenFloorplan}>เปิดผังอาคาร</button> แล้วเลือกโปรเจคเพื่อสร้างอาคารและชั้น
              </p>
            )}
          </>
        )}
      </div>

      <ProjectEditor editor={editor} busy={busy} error={dialogError} onChange={setEditor} onCreate={create} onUpdate={update} />
      <ArchiveProjectDialog project={archive} onClose={() => setArchive(null)} onArchive={archiveProject} />
    </div>
  );
}
