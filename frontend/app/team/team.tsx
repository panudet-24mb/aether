"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { Copy, KeyRound, Pencil, RefreshCw, Search, ShieldCheck, Trash2, UserPlus, Users } from "lucide-react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import "./team.css";
import { ApiError, createClientFrom, type Client } from "../topology/api";
import { useLatest } from "../topology/use-latest";
import { PROJECT_COLORS } from "../topology/projects";

/** What GET /api/v1/me answers: who the caller is, what they may do and what the database lets them see. */
export type Me = {
  user_id: string;
  tenant_id: string;
  email: string;
  name: string;
  role: string;
  permissions?: Record<string, string>;
  /** Projects the caller may see, or null when they see every project. */
  project_ids: string[] | null;
  must_change_password: boolean;
  deployment_mode: string;
};

/** The shell reads the signed-in member's role and project scope through this one call. */
export function fetchMe(client: Pick<Client, "raw">): Promise<Me> {
  return client.raw<Me>("/me");
}

type Member = {
  user_id: string;
  email: string;
  name: string;
  role: string;
  /** Empty means every project of the workspace. */
  project_ids: string[];
  must_change_password: boolean;
  created_at: string;
  last_seen_at: string | null;
};
type Project = { id: string; name: string; color: string };
type Editor = { userId: string | null; email: string; role: string; projectIds: string[]; password: string };

const ROLES: [string, string][] = [
  ["owner", "เจ้าของ"],
  ["admin", "ผู้ดูแล"],
  ["operator", "ผู้ปฏิบัติงาน"],
  ["viewer", "ผู้ดูข้อมูล"],
];
const ROLE_LABEL: Record<string, string> = Object.fromEntries(ROLES);
const ROLE_HELP: Record<string, string> = {
  owner: "ทุกอย่างรวมถึงสมาชิก",
  admin: "จัดการอุปกรณ์และสมาชิก",
  operator: "รับทราบ/ปิดการแจ้งเตือน",
  viewer: "ดูอย่างเดียว",
};
/** 64 characters exactly, so a 32-bit random value maps onto them without bias. Look-alikes removed. */
const ALPHABET = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#$%*+-";
const PASSWORD_LENGTH = 20;
/** How long a generated password may stay in memory and on screen. */
const SECRET_TTL_MS = 120000;
const MIN_PASSWORD = 12;

/** A strong initial password, generated in the browser and never sent anywhere but this workspace's API. */
function generatePassword(): string {
  const values = new Uint32Array(PASSWORD_LENGTH);
  crypto.getRandomValues(values);
  let out = "";
  for (const value of values) out += ALPHABET[value % ALPHABET.length];
  return out;
}

const day = (s: string | null) => (s ? new Date(s).toLocaleDateString("th-TH", { year: "numeric", month: "short", day: "numeric" }) : "—");

/** The generic 409 text ("มีอยู่แล้ว หรือถึงจำนวนสูงสุด") is wrong for these two calls, so say what happened. */
function conflict(e: unknown, message: string): never {
  if (e instanceof ApiError && e.status === 409) throw new ApiError(409, message);
  throw e;
}

export default function TeamPage({ getToken, refresh, onUnauthorized }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const [accessEditor, setAccessEditor] = useState<{ member: Member; permissions: Record<string,string> } | null>(null);
  const [accessError, setAccessError] = useState("");
  const [me, setMe] = useState<Me | null>(null);
  const [members, setMembers] = useState<Member[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [readOnly, setReadOnly] = useState(false);
  const [query, setQuery] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [editor, setEditor] = useState<Editor | null>(null);
  const [editorError, setEditorError] = useState("");
  const [handover, setHandover] = useState<{ email: string; password: string } | null>(null);
  const [removeTarget, setRemoveTarget] = useState<Member | null>(null);
  const [resetTarget, setResetTarget] = useState<{ member: Member; password: string } | null>(null);
  const [own, setOwn] = useState({ current: "", next: "", confirm: "" });
  const [ownError, setOwnError] = useState("");
  const unauthorized = useRef(false);

  const describe = useCallback(
    (e: unknown, fallback: string) => {
      if (e instanceof ApiError && e.status === 401 && !unauthorized.current) {
        unauthorized.current = true;
        onUnauthorized?.();
      }
      return e instanceof Error ? e.message : fallback;
    },
    [onUnauthorized],
  );

  const load = useCallback(async () => {
    try {
      const self = await fetchMe(client);
      setMe(self);
      if ((self.role !== "owner" && self.role !== "admin") || (self.role === "admin" && self.project_ids !== null)) {
        setReadOnly(true);
        setMembers([]);
        setProjects([]);
        setError("");
        return;
      }
      const [list, list2] = await Promise.all([client.raw<{ items: Member[] }>("/members"), client.raw<{ items: Project[] }>("/projects")]);
      setReadOnly(false);
      setMembers(list.items);
      setProjects(list2.items);
      setError("");
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) {
        setReadOnly(true);
        setError("");
        return;
      }
      setError(describe(e, "โหลดรายชื่อสมาชิกไม่สำเร็จ"));
    }
  }, [client, describe]);

  useEffect(() => {
    let active = true;
    // Members change rarely, so there is no poll: one read on mount, then after every mutation.
    const first = async () => {
      if (active) await load();
    };
    void first();
    return () => {
      active = false;
    };
  }, [load]);
  useEffect(() => {
    if (notice === "") return;
    const t = setTimeout(() => setNotice(""), 4000);
    return () => clearTimeout(t);
  }, [notice]);
  // A generated password is a shared secret sitting in memory. It goes when its dialog closes (the state
  // holding it is dropped), when this page unmounts (the cleanup below runs), and in any case after two
  // minutes, so a workstation left open does not keep showing it.
  // Each timer is keyed on the secret itself, not on the dialog object, so typing an email does not keep
  // pushing the deadline back; a regenerated password starts a fresh two minutes.
  const shownSecret = handover?.password ?? "";
  useEffect(() => {
    if (shownSecret === "") return;
    const t = setTimeout(() => setHandover(null), SECRET_TTL_MS);
    return () => clearTimeout(t);
  }, [shownSecret]);
  const draftSecret = editor?.password ?? "";
  useEffect(() => {
    if (draftSecret === "") return;
    const t = setTimeout(() => {
      setEditor(null);
      setNotice("รหัสผ่านที่สุ่มไว้หมดอายุแล้ว · เปิดหน้าต่างเพิ่มสมาชิกใหม่อีกครั้ง");
    }, SECRET_TTL_MS);
    return () => clearTimeout(t);
  }, [draftSecret]);
  const resetSecret = resetTarget?.password ?? "";
  useEffect(() => {
    if (resetSecret === "") return;
    const t = setTimeout(() => setResetTarget(null), SECRET_TTL_MS);
    return () => clearTimeout(t);
  }, [resetSecret]);

  /** Runs one mutation, then reloads so the table always shows what the server actually stored. */
  const run = useCallback(
    async (action: () => Promise<void>, done: string, fallback: string, onError?: (message: string) => void) => {
      setBusy(true);
      try {
        await action();
        await load();
        setNotice(done);
        setError("");
        return true;
      } catch (e) {
        const message = describe(e, fallback);
        if (onError) onError(message);
        else setError(message);
        return false;
      } finally {
        setBusy(false);
      }
    },
    [describe, load],
  );

  const openAdd = () => {
    setEditorError("");
    setEditor({ userId: null, email: "", role: "viewer", projectIds: [], password: generatePassword() });
  };
  const openEdit = (m: Member) => {
    setEditorError("");
    setEditor({ userId: m.user_id, email: m.email, role: m.role, projectIds: m.project_ids, password: "" });
  };
  const openReset = (m: Member) => setResetTarget({ member: m, password: generatePassword() });

  const submitEditor = async () => {
    if (editor === null) return;
    const email = editor.email.trim().toLowerCase();
    if (editor.userId === null && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
      setEditorError("อีเมลไม่ถูกต้อง");
      return;
    }
    const body = { role: editor.role, project_ids: editor.projectIds };
    const ok = await run(
      async () => {
        if (editor.userId === null) {
          await client
            .post<Member>("/members", { ...body, email, password: editor.password })
            .catch((e: unknown) => conflict(e, "เพิ่มอีเมลนี้ไม่ได้ · อาจเป็นสมาชิกอยู่แล้ว มีบัญชีผูกกับ workspace อื่น หรือถึงขีดจำกัด 200 คน"));
          setHandover({ email, password: editor.password });
        } else {
          await client.post<void>(`/members/${editor.userId}/update`, body);
        }
      },
      editor.userId === null ? "เพิ่มสมาชิกแล้ว" : "บันทึกสิทธิ์แล้ว",
      "บันทึกไม่สำเร็จ",
      setEditorError,
    );
    if (ok) setEditor(null);
  };

  const submitReset = async () => {
    if (resetTarget === null) return;
    const { member, password } = resetTarget;
    const ok = await run(
      () =>
        client
          .post<void>(`/members/${member.user_id}/reset-password`, { password })
          .catch((e: unknown) => conflict(e, "บัญชีนี้ถูกใช้ในองค์กรอื่นด้วย จึงตั้งรหัสผ่านใหม่จากที่นี่ไม่ได้ · ให้เจ้าตัวเปลี่ยนรหัสผ่านเองจากเมนู “บัญชีของฉัน”")),
      "ตั้งรหัสผ่านใหม่แล้ว · เซสชันเดิมของสมาชิกถูกตัดทั้งหมด",
      "ตั้งรหัสผ่านใหม่ไม่สำเร็จ",
      (message) => setError(message),
    );
    setResetTarget(null);
    if (ok) setHandover({ email: member.email, password });
  };

  const submitOwnPassword = async (e: React.FormEvent) => {
    e.preventDefault();
    setOwnError("");
    if (own.next.length < MIN_PASSWORD) {
      setOwnError(`รหัสผ่านใหม่ต้องยาวอย่างน้อย ${MIN_PASSWORD} ตัวอักษร`);
      return;
    }
    if (own.next !== own.confirm) {
      setOwnError("ยืนยันรหัสผ่านไม่ตรงกัน");
      return;
    }
    const ok = await run(
      () => client.post<void>("/auth/password", { current_password: own.current, new_password: own.next }),
      "เปลี่ยนรหัสผ่านแล้ว · อุปกรณ์อื่นที่ยังเข้าระบบอยู่ถูกตัดออก",
      "เปลี่ยนรหัสผ่านไม่สำเร็จ",
      setOwnError,
    );
    if (ok) setOwn({ current: "", next: "", confirm: "" });
  };

  const copy = (value: string) => {
    void navigator.clipboard?.writeText(value).then(
      () => setNotice("คัดลอกรหัสผ่านแล้ว"),
      () => setNotice("คัดลอกอัตโนมัติไม่ได้ · กรุณาเลือกข้อความแล้วคัดลอกเอง"),
    );
  };

  const needle = query.trim().toLowerCase();
  const shown = needle === "" ? members : members.filter((m) => m.email.toLowerCase().includes(needle) || m.name.toLowerCase().includes(needle) || ROLE_LABEL[m.role]?.includes(needle));
  const canGrantOwner = me?.role === "owner";
  const owners = members.filter((m) => m.role === "owner").length;
  const projectName = (id: string) => projects.find((p) => p.id === id)?.name ?? id.slice(0, 8);
  const projectColor = (id: string) => PROJECT_COLORS[projects.find((p) => p.id === id)?.color ?? "mint"] ?? PROJECT_COLORS.mint;

  return (
    <div className="tm">
      <div className="tm-bar">
        <h1 className="tm-title">ทีมและสิทธิ์</h1>
        {!readOnly && (
          <span className="tm-count">
            <Users size={13} /> <b>{members.length}</b> คน
          </span>
        )}
        <span className="tm-search">
          <Search size={14} />
          <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="ค้นหาอีเมลหรือชื่อ" aria-label="ค้นหาสมาชิก" disabled={readOnly} />
        </span>
        <span className="tm-actions">
          <button type="button" className="tm-icon-btn" onClick={() => void load()} disabled={busy} aria-label="โหลดรายชื่อใหม่" title="โหลดรายชื่อใหม่">
            <RefreshCw size={15} />
          </button>
          {!readOnly && (
            <button type="button" className="tm-btn primary" onClick={openAdd} disabled={busy}>
              <UserPlus size={15} /> เพิ่มสมาชิก
            </button>
          )}
        </span>
      </div>

      {error !== "" && (
        <p className="tm-alert is-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setError("")} aria-label="ปิดข้อความ">
            ×
          </button>
        </p>
      )}
      {notice !== "" && (
        <p className="tm-alert is-notice">
          <span>{notice}</span>
          <button type="button" onClick={() => setNotice("")} aria-label="ปิดข้อความ">
            ×
          </button>
        </p>
      )}

      <div className="tm-body">
        {readOnly ? (
          <div className="tm-empty">
            <ShieldCheck size={26} />
            <h2>บัญชีนี้ดูรายชื่อสมาชิกไม่ได้</h2>
            <p>
              การจัดการสมาชิกและสิทธิ์สงวนไว้ให้เจ้าของ (owner) และผู้ดูแล (admin) ของ workspace เท่านั้น
              {me !== null && ` บัญชีของคุณมีสิทธิ์ระดับ “${ROLE_LABEL[me.role] ?? me.role}” (${ROLE_HELP[me.role] ?? ""})`}
              {me !== null && me.project_ids !== null && ` และเห็นข้อมูลเฉพาะ ${me.project_ids.length} โปรเจคที่ได้รับสิทธิ์`}
            </p>
            <p>หากต้องเพิ่มคนเข้าทีมหรือแก้สิทธิ์ ให้ติดต่อเจ้าของ workspace</p>
          </div>
        ) : shown.length === 0 ? (
          <div className="tm-empty">
            <Users size={26} />
            <h2>{members.length === 0 ? "ยังมีแต่บัญชีของคุณใน workspace นี้" : "ไม่พบสมาชิกที่ค้นหา"}</h2>
            <p>{members.length === 0 ? "กด “เพิ่มสมาชิก” เพื่อสร้างบัญชีให้เพื่อนร่วมงาน แล้วส่งรหัสผ่านแรกให้ด้วยตัวเอง ระบบนี้ไม่ส่งอีเมล" : "ลองค้นด้วยอีเมลหรือชื่อที่สั้นลง"}</p>
          </div>
        ) : (
          <table className="tm-table">
            <thead>
              <tr>
                <th scope="col">สมาชิก</th>
                <th scope="col">สิทธิ์</th>
                <th scope="col">โปรเจคที่เห็น</th>
                <th scope="col">เพิ่มเมื่อ</th>
                <th scope="col">เข้าระบบล่าสุด</th>
                <th scope="col" aria-label="การจัดการ" />
              </tr>
            </thead>
            <tbody>
              {shown.map((m) => {
                const self = m.user_id === me?.user_id;
                const protectedOwner = m.role === "owner" && (!canGrantOwner || owners <= 1);
                const locked = self || protectedOwner || busy;
                return (
                  <tr key={m.user_id}>
                    <td>
                      <span className="tm-person">
                        <strong>
                          {m.email}
                          {self && <span className="tm-you">คุณ</span>}
                        </strong>
                        <small>
                          {m.name}
                          {m.must_change_password && <span className="tm-flag"> · ต้องเปลี่ยนรหัสผ่านเมื่อเข้าระบบ</span>}
                        </small>
                      </span>
                    </td>
                    <td>
                      <select
                        className="tm-select"
                        value={m.role}
                        disabled={locked}
                        aria-label={`สิทธิ์ของ ${m.email}`}
                        onChange={(e) => void run(() => client.post<void>(`/members/${m.user_id}/update`, { role: e.target.value, project_ids: m.project_ids }), "บันทึกสิทธิ์แล้ว", "เปลี่ยนสิทธิ์ไม่สำเร็จ")}
                      >
                        {ROLES.filter(([id]) => id !== "owner" || canGrantOwner || m.role === "owner").map(([id, label]) => (
                          <option key={id} value={id}>
                            {label}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td>
                      <span className="tm-chips">
                        {m.role === "owner" || m.project_ids.length === 0 ? (
                          <span className="tm-chip is-all">ทุกโปรเจค</span>
                        ) : (
                          m.project_ids.map((id) => (
                            <span key={id} className="tm-chip" style={{ color: projectColor(id) }}>
                              <i /> {projectName(id)}
                            </span>
                          ))
                        )}
                      </span>
                    </td>
                    <td className="tm-dim">{day(m.created_at)}</td>
                    <td className="tm-dim">{day(m.last_seen_at)}</td>
                    <td>
                      <span className="tm-row-actions">
                        <button type="button" className="tm-icon-btn" onClick={() => openEdit(m)} disabled={locked} aria-label={`แก้สิทธิ์และโปรเจคของ ${m.email}`} title="แก้สิทธิ์และโปรเจค">
                          <Pencil size={14} />
                        </button>
                        <button type="button" className="tm-icon-btn" disabled={locked || m.role === "owner"} title="สิทธิ์การใช้งานแต่ละหน้า" aria-label={`สิทธิ์แต่ละหน้าของ ${m.email}`} onClick={async () => { try { const permissions = await client.raw<Record<string,string>>(`/members/${m.user_id}/access`); setAccessError(""); setAccessEditor({member:m,permissions}); } catch(e) { setError(describe(e,"โหลดสิทธิ์ไม่สำเร็จ")); } }}><ShieldCheck size={14}/></button>
                        <button type="button" className="tm-icon-btn" onClick={() => openReset(m)} disabled={locked} aria-label={`ตั้งรหัสผ่านใหม่ให้ ${m.email}`} title="ตั้งรหัสผ่านใหม่">
                          <KeyRound size={14} />
                        </button>
                        <button type="button" className="tm-icon-btn is-danger" onClick={() => setRemoveTarget(m)} disabled={locked} aria-label={`นำ ${m.email} ออกจาก workspace`} title="นำออกจาก workspace">
                          <Trash2 size={14} />
                        </button>
                      </span>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}

        <section className="tm-self">
          <h2>บัญชีของฉัน</h2>
          <p>
            {me?.email ?? "—"} · สิทธิ์ {ROLE_LABEL[me?.role ?? ""] ?? me?.role ?? "—"}
            {me?.must_change_password && " · ระบบขอให้เปลี่ยนรหัสผ่านที่ได้รับมาก่อนใช้งานต่อ"}
          </p>
          <form onSubmit={(e) => void submitOwnPassword(e)}>
            <label className="tm-field">
              รหัสผ่านปัจจุบัน
              <input type="password" autoComplete="current-password" value={own.current} onChange={(e) => setOwn({ ...own, current: e.target.value })} required />
            </label>
            <label className="tm-field">
              รหัสผ่านใหม่
              <input type="password" autoComplete="new-password" minLength={MIN_PASSWORD} value={own.next} onChange={(e) => setOwn({ ...own, next: e.target.value })} required />
            </label>
            <label className="tm-field">
              ยืนยันรหัสผ่านใหม่
              <input type="password" autoComplete="new-password" minLength={MIN_PASSWORD} value={own.confirm} onChange={(e) => setOwn({ ...own, confirm: e.target.value })} required />
            </label>
            <button type="submit" className="tm-btn primary" disabled={busy}>
              <KeyRound size={15} /> เปลี่ยนรหัสผ่าน
            </button>
          </form>
          {ownError !== "" && (
            <p className="tm-dialog-error" role="alert">
              {ownError}
            </p>
          )}
          <p className="tm-hint">การเปลี่ยนรหัสผ่านจะตัดการเข้าระบบของอุปกรณ์อื่นทั้งหมด ยกเว้นหน้าต่างนี้</p>
        </section>
      </div>

      <Dialog open={accessEditor !== null} onOpenChange={(open) => { if (!open) setAccessEditor(null); }}>
        <DialogContent><DialogHeader><DialogTitle>สิทธิ์การใช้งานแต่ละหน้า</DialogTitle><DialogDescription>{accessEditor?.member.email} · สิทธิ์ไม่เกินบทบาท {accessEditor?.member.role} · โปรเจคที่เลือกยังบังคับใช้เสมอ</DialogDescription></DialogHeader>
          {accessEditor && <form className="tm-form" onSubmit={async (e) => { e.preventDefault(); const ok = await run(() => client.post<void>(`/members/${accessEditor.member.user_id}/access`,accessEditor.permissions), "บันทึกสิทธิ์แล้ว", "บันทึกสิทธิ์ไม่สำเร็จ", setAccessError); if(ok) setAccessEditor(null); }}>
            {[["live","ภาพรวม"],["connect","เชื่อมต่ออุปกรณ์"],["alerts","การแจ้งเตือน"],["assets","อุปกรณ์ทั้งหมด"],["floorplan","ผังอาคาร"],["automation","Automation Studio"],["studio","Dashboard Studio"],["control","สั่งงานอุปกรณ์ (เปิด/ปิด ตั้งค่า)"],["team","ทีมและสิทธิ์"]].map(([id,label]) => <label key={id}>{label}<select value={accessEditor.permissions[id] ?? "write"} onChange={(e) => setAccessEditor({...accessEditor,permissions:{...accessEditor.permissions,[id]:e.target.value}})}><option value="none">ไม่ให้เข้า</option><option value="read">ดูอย่างเดียว</option><option value="write">ใช้งานตามบทบาท</option></select></label>)}
            <p className="tm-hint">Viewer ดูได้ · Operator รับทราบ/ปิดแจ้งเตือนได้ · Admin จัดการข้อมูลได้ เฉพาะโปรเจคที่ได้รับสิทธิ์ · Admin ที่จำกัดโปรเจคจัดการสมาชิกไม่ได้</p>
            {accessError && <p role="alert">{accessError}</p>}<button className="tm-btn primary" disabled={busy}>บันทึกสิทธิ์</button>
          </form>}
        </DialogContent>
      </Dialog>
      <Dialog open={editor !== null} onOpenChange={(open) => !open && !busy && setEditor(null)}>
        <DialogContent className="tm-dialog">
          <DialogHeader>
            <DialogTitle>{editor?.userId === null ? "เพิ่มสมาชิก" : "แก้สิทธิ์และโปรเจค"}</DialogTitle>
            <DialogDescription>{editor?.userId === null ? "สร้างบัญชีให้เพื่อนร่วมงาน แล้วส่งรหัสผ่านแรกให้ด้วยตัวเอง" : editor?.email}</DialogDescription>
          </DialogHeader>
          {editor !== null && (
            <div className="tm-form">
              {editor.userId === null && (
                <label>
                  อีเมล
                  <input type="email" autoComplete="off" value={editor.email} onChange={(e) => setEditor({ ...editor, email: e.target.value })} placeholder="somchai@hospital.example" />
                </label>
              )}
              <label>
                สิทธิ์
                <select value={editor.role} onChange={(e) => setEditor({ ...editor, role: e.target.value })}>
                  {ROLES.filter(([id]) => id !== "owner" || canGrantOwner).map(([id, label]) => (
                    <option key={id} value={id}>
                      {label} · {ROLE_HELP[id]}
                    </option>
                  ))}
                </select>
              </label>
              <ul className="tm-roles">
                {ROLES.filter(([id]) => id !== "owner" || canGrantOwner).map(([id, label]) => (
                  <li key={id}>
                    <b>{label}</b> · {ROLE_HELP[id]}
                  </li>
                ))}
              </ul>
              <label>
                โปรเจคที่เห็น
                <span className="tm-hint">{editor.role === "owner" ? "เจ้าของเห็นทุกโปรเจคเสมอ" : editor.projectIds.length === 0 ? "ไม่เลือกอะไรเลย = ทุกโปรเจค" : `เห็นเฉพาะ ${editor.projectIds.length} โปรเจคที่เลือก`}</span>
              </label>
              <div className="tm-picker">
                <label className="tm-pick">
                  <input type="checkbox" checked={editor.projectIds.length === 0} onChange={() => setEditor({ ...editor, projectIds: [] })} disabled={editor.role === "owner"} />
                  ทุกโปรเจค
                </label>
                {projects.map((p) => (
                  <label key={p.id} className="tm-pick">
                    <input
                      type="checkbox"
                      checked={editor.projectIds.includes(p.id)}
                      disabled={editor.role === "owner"}
                      onChange={(e) => setEditor({ ...editor, projectIds: e.target.checked ? [...editor.projectIds, p.id] : editor.projectIds.filter((id) => id !== p.id) })}
                    />
                    <i style={{ background: PROJECT_COLORS[p.color] ?? PROJECT_COLORS.mint }} />
                    {p.name}
                  </label>
                ))}
              </div>
              {editor.userId === null && (
                <>
                  <label>
                    รหัสผ่านแรก
                    <span className="tm-secret">
                      <code>{editor.password}</code>
                      <button type="button" className="tm-btn" onClick={() => copy(editor.password)} aria-label="คัดลอกรหัสผ่านแรก">
                        <Copy size={14} /> คัดลอก
                      </button>
                      <button type="button" className="tm-btn" onClick={() => setEditor({ ...editor, password: generatePassword() })} aria-label="สุ่มรหัสผ่านใหม่">
                        <RefreshCw size={14} />
                      </button>
                    </span>
                  </label>
                  <p className="tm-warn">
                    ระบบนี้ไม่ส่งอีเมล · ต้องส่งรหัสผ่านนี้ให้เจ้าตัวด้วยตัวเอง และรหัสผ่านจะแสดงครั้งเดียวเท่านั้น เมื่อเข้าระบบครั้งแรกสมาชิกจะถูกขอให้ตั้งรหัสผ่านของตัวเองก่อนใช้งานอะไรได้
                    หากอีเมลนี้มีบัญชีผูกกับ workspace อื่นอยู่แล้ว ระบบจะไม่เพิ่มให้ · ให้ใช้อีเมลอื่นของเขาแทน
                  </p>
                </>
              )}
              {editorError !== "" && (
                <p className="tm-dialog-error" role="alert">
                  {editorError}
                </p>
              )}
              <div className="tm-form-actions">
                <button type="button" className="tm-btn" onClick={() => setEditor(null)} disabled={busy}>
                  ยกเลิก
                </button>
                <button type="button" className="tm-btn primary" onClick={() => void submitEditor()} disabled={busy}>
                  {editor.userId === null ? "เพิ่มสมาชิก" : "บันทึก"}
                </button>
              </div>
            </div>
          )}
        </DialogContent>
      </Dialog>

      <Dialog open={handover !== null} onOpenChange={(open) => !open && setHandover(null)}>
        <DialogContent className="tm-dialog">
          <DialogHeader>
            <DialogTitle>ส่งรหัสผ่านให้เจ้าตัว</DialogTitle>
            <DialogDescription>{handover?.email}</DialogDescription>
          </DialogHeader>
          {handover !== null && (
            <div className="tm-form">
              <span className="tm-secret">
                <code>{handover.password}</code>
                <button type="button" className="tm-btn" onClick={() => copy(handover.password)} aria-label="คัดลอกรหัสผ่าน">
                  <Copy size={14} /> คัดลอก
                </button>
              </span>
              <p className="tm-warn">
                รหัสผ่านนี้จะไม่แสดงอีก และจะถูกลบออกจากหน้าจอนี้เองภายใน {SECRET_TTL_MS / 60000} นาที · ส่งให้เจ้าตัวด้วยตัวเอง ระบบไม่ส่งอีเมล และเขาจะเข้าใช้งานอะไรไม่ได้จนกว่าจะตั้งรหัสผ่านของตัวเองเมื่อเข้าระบบครั้งแรก
              </p>
              <div className="tm-form-actions">
                <button type="button" className="tm-btn primary" onClick={() => setHandover(null)}>
                  เรียบร้อย
                </button>
              </div>
            </div>
          )}
        </DialogContent>
      </Dialog>

      <AlertDialog open={resetTarget !== null} onOpenChange={(open) => !open && !busy && setResetTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>ตั้งรหัสผ่านใหม่ให้ “{resetTarget?.member.email}”?</AlertDialogTitle>
            <AlertDialogDescription>
              ระบบจะสร้างรหัสผ่านใหม่ให้และตัดทุกเซสชันของสมาชิกคนนี้ทันที คุณต้องส่งรหัสผ่านใหม่ให้เขาด้วยตัวเอง
              หากบัญชีนี้ถูกใช้ในองค์กรอื่นด้วย ระบบจะปฏิเสธ เพราะรหัสผ่านนั้นไม่ใช่ของ workspace นี้
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction onClick={() => void submitReset()} disabled={busy}>
              ตั้งรหัสผ่านใหม่
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={removeTarget !== null} onOpenChange={(open) => !open && !busy && setRemoveTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>นำ “{removeTarget?.email}” ออกจาก workspace?</AlertDialogTitle>
            <AlertDialogDescription>เขาจะออกจากระบบทันทีและเข้าถึง workspace นี้ไม่ได้อีก ข้อมูลอุปกรณ์และประวัติการแจ้งเตือนยังอยู่ครบ หากบัญชีนี้อยู่ในองค์กรอื่นด้วย บัญชีนั้นไม่ได้รับผลกระทบ</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction
              disabled={busy}
              onClick={() => {
                const target = removeTarget;
                setRemoveTarget(null);
                if (target !== null) void run(() => client.post<void>(`/members/${target.user_id}/remove`, {}), "นำสมาชิกออกแล้ว", "นำสมาชิกออกไม่สำเร็จ");
              }}
            >
              นำออก
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
