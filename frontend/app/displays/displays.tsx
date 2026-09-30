"use client";
import { useCallback, useEffect, useMemo, useState } from "react";
import { ArrowDown, ArrowUp, Copy, Monitor, Plus, RefreshCw, Trash2, X } from "lucide-react";
import "./displays.css";
import { createClientFrom } from "../topology/api";
import { useLatest } from "../topology/use-latest";

/**
 * Display links (docs/platform/display.md): wall TVs for a control room. Owners and admins who see every project
 * create a display with a project scope and a playlist, and get a one-time pairing code for the TV.
 */

type ViewKind = "overview" | "alerts" | "floorplan" | "devices" | "presence" | "studio" | "twin";
type PlaylistItem = { kind: ViewKind; seconds: number; ref?: string };
type Display = { id: string; name: string; project_ids: string[]; playlist: PlaylistItem[]; show_names: boolean; allow_ack: boolean; paired: boolean; pairing_expires_at: string | null; paired_at: string | null; last_seen_at: string | null; last_ip: string | null; created_at: string };
type Pairing = { display: Display; code: string; expires_at: string };
type Draft = { id?: string; name: string; project_ids: string[]; playlist: PlaylistItem[]; show_names: boolean; allow_ack: boolean };

const KIND_LABEL: Record<ViewKind, string> = { overview: "ภาพรวม", alerts: "การแจ้งเตือน", floorplan: "ผังอาคาร 3D", devices: "สถานะอุปกรณ์", presence: "ผู้สวมอุปกรณ์ตามพื้นที่", studio: "Dashboard Studio", twin: "Digital twin 3D" };
const KINDS = Object.keys(KIND_LABEL) as ViewKind[];
const DEFAULT_PLAYLIST: PlaylistItem[] = [{ kind: "overview", seconds: 30 }, { kind: "alerts", seconds: 20 }, { kind: "floorplan", seconds: 40 }, { kind: "devices", seconds: 20 }];
const ONLINE_MS = 3 * 60 * 1000;
const when = (s: string | null) => (s ? new Date(s).toLocaleString("th-TH", { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }) : "—");

/** isOwner: only an owner may turn on wearer names or acknowledging from the TV (an admin may turn them off). */
export default function DisplaysPage({ getToken, refresh, onUnauthorized, isOwner = false }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void; isOwner?: boolean }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const [items, setItems] = useState<Display[]>([]);
  const [projects, setProjects] = useState<{ id: string; name: string }[]>([]);
  const [dashboards, setDashboards] = useState<{ id: string; name: string }[]>([]);
  const [sites, setSites] = useState<{ id: string; name: string }[]>([]);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [pairing, setPairing] = useState<Pairing | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [now, setNow] = useState(() => Date.now());

  const load = useCallback(async () => {
    try {
      const [d, p, s, st] = await Promise.all([
        client.raw<{ items: Display[] }>("/displays"),
        client.raw<{ items: { id: string; name: string }[] }>("/projects"),
        client.raw<{ items: { id: string; name: string }[] }>("/sites").catch(() => ({ items: [] })),
        client.raw<{ items: { id: string; name: string }[] }>("/studio/items?kind=dashboard").catch(() => ({ items: [] })),
      ]);
      setItems(d.items); setProjects(p.items); setSites(s.items); setDashboards(st.items); setError("");
    } catch (e) {
      const status = (e as { status?: number }).status;
      if (status === 401) onUnauthorized?.();
      setError(status === 403 ? "เฉพาะ owner หรือ admin ที่เห็นทุกโปรเจกต์เท่านั้นที่จัดการจอแสดงผลได้" : e instanceof Error ? e.message : "โหลดไม่สำเร็จ");
    }
  }, [client, onUnauthorized]);
  useEffect(() => { const id = requestAnimationFrame(() => void load()); return () => cancelAnimationFrame(id); }, [load]);
  useEffect(() => { const id = setInterval(() => { setNow(Date.now()); void load(); }, 30000); return () => clearInterval(id); }, [load]);
  useEffect(() => { if (!pairing) return; const id = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(id); }, [pairing]);

  const act = async (fn: () => Promise<void>) => {
    setBusy(true); setError(""); setNotice("");
    try { await fn(); } catch (e) { setError(e instanceof Error ? e.message : "ไม่สำเร็จ"); } finally { setBusy(false); }
  };
  const save = () => act(async () => {
    if (!draft) return;
    const body = { name: draft.name, project_ids: draft.project_ids, playlist: draft.playlist.map((v) => (v.ref ? v : { kind: v.kind, seconds: v.seconds })), show_names: draft.show_names, allow_ack: draft.allow_ack };
    if (draft.id) { await client.post(`/displays/${draft.id}/update`, body); setNotice("บันทึกแล้ว · จอจะเปลี่ยนตามภายในหนึ่งนาที"); }
    else setPairing(await client.post<Pairing>("/displays", body));
    setDraft(null); await load();
  });
  const repair = (d: Display) => act(async () => {
    if (d.paired && !confirm(`จับคู่ “${d.name}” ใหม่? จอที่ใช้อยู่ตอนนี้จะหลุดทันที`)) return;
    setPairing(await client.post<Pairing>(`/displays/${d.id}/repair`, {})); await load();
  });
  const revoke = (d: Display) => act(async () => {
    if (!confirm(`ยกเลิกจอ “${d.name}”? จอจะหยุดแสดงผลทันทีและใช้รหัสเดิมไม่ได้อีก`)) return;
    await client.post(`/displays/${d.id}/revoke`, {}); setNotice(`ยกเลิก “${d.name}” แล้ว`); await load();
  });

  const projectName = useMemo(() => new Map(projects.map((p) => [p.id, p.name])), [projects]);
  const pairURL = pairing ? `${location.origin}/display#code=${pairing.code}` : "";
  const left = pairing ? Math.max(0, Math.floor((Date.parse(pairing.expires_at) - now) / 1000)) : 0;
  return <section className="dsp">
    <header className="dsp-head">
      <div><h1>จอแสดงผล</h1><p>จอ TV ห้องควบคุม · หมุนหน้าจออัตโนมัติ และเต็มจอทันทีเมื่อมี SOS หรือเหตุอันตราย · จอไม่ใช้บัญชีพนักงาน และแก้ไขข้อมูลไม่ได้</p></div>
      <button type="button" className="dsp-primary" disabled={busy} onClick={() => { setPairing(null); setDraft({ name: "", project_ids: [], playlist: DEFAULT_PLAYLIST, show_names: false, allow_ack: false }); }}><Plus size={16} />เพิ่มจอ</button>
    </header>
    {error && <p className="dsp-error" role="alert">{error}</p>}
    {notice && <p className="dsp-notice" role="status">{notice}</p>}
    {pairing && <div className="dsp-pairing" role="dialog" aria-label="รหัสจับคู่จอ">
      <button type="button" className="dsp-close" aria-label="ปิด" onClick={() => setPairing(null)}><X size={16} /></button>
      <p>เปิด <code>{location.origin}/display</code> บนจอ “{pairing.display.name}” แล้วใส่รหัสนี้ · หรือเปิดลิงก์ด้านล่างบนจอโดยตรง</p>
      <strong className="dsp-code">{pairing.code}</strong>
      <p className="dsp-muted">{left > 0 ? `หมดอายุใน ${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")} นาที · ใช้ได้ครั้งเดียว · แสดงครั้งเดียว` : "รหัสหมดอายุแล้ว · กด “จับคู่ใหม่” เพื่อขอรหัสใหม่"}</p>
      <div className="dsp-link"><code>{pairURL}</code><button type="button" onClick={() => void navigator.clipboard.writeText(pairURL).then(() => setNotice("คัดลอกลิงก์จับคู่แล้ว"))}><Copy size={14} />คัดลอก</button></div>
    </div>}
    {draft && <DisplayForm draft={draft} isOwner={isOwner} was={items.find((d) => d.id === draft.id)} projects={projects} sites={sites} dashboards={dashboards} busy={busy} onChange={setDraft} onCancel={() => setDraft(null)} onSave={() => void save()} />}
    {items.length === 0 && !draft && !error ? <div className="dsp-empty"><Monitor size={36} /><h2>ยังไม่มีจอแสดงผล</h2><p>เพิ่มจอเพื่อได้รหัสจับคู่ แล้วเปิดหน้า /display บนสมาร์ททีวีหรือคอมพิวเตอร์ที่ต่อจอ</p></div>
      : <ul className="dsp-list">{items.map((d) => {
        const online = !!d.last_seen_at && now - Date.parse(d.last_seen_at) < ONLINE_MS;
        return <li key={d.id}>
          <div className="dsp-row-main">
            <strong><span className={`dsp-dot ${d.paired ? (online ? "is-on" : "is-off") : "is-wait"}`} />{d.name}</strong>
            <small>{d.paired ? (online ? "ออนไลน์" : "ออฟไลน์") : d.pairing_expires_at ? `รอจับคู่ · รหัสหมดอายุ ${when(d.pairing_expires_at)}` : "ยังไม่ได้จับคู่ · ขอรหัสใหม่"}</small>
            {d.paired && <p className="dsp-seen">เห็นล่าสุด <b>{when(d.last_seen_at)}</b> จาก IP <b>{d.last_ip ?? "—"}</b> · ถ้าไม่ใช่จอของคุณ ให้กด “ยกเลิก” ทันที</p>}
            <small>{d.project_ids.length === 0 ? "ทุกโปรเจกต์" : d.project_ids.map((id) => projectName.get(id) ?? "โปรเจกต์ที่เก็บแล้ว").join(", ")} · {d.playlist.map((v) => KIND_LABEL[v.kind]).join(" → ")}</small>
            <span className="dsp-tags">{d.show_names && <em>แสดงชื่อผู้สวม</em>}{d.allow_ack && <em>รับทราบจากจอได้</em>}</span>
          </div>
          <div className="dsp-actions">
            <button type="button" disabled={busy} onClick={() => { setPairing(null); setDraft({ id: d.id, name: d.name, project_ids: d.project_ids, playlist: d.playlist, show_names: d.show_names, allow_ack: d.allow_ack }); }}>แก้ไข</button>
            <button type="button" disabled={busy} onClick={() => void repair(d)}><RefreshCw size={14} />{d.paired ? "จับคู่ใหม่" : "ขอรหัสใหม่"}</button>
            <button type="button" className="dsp-danger" disabled={busy} onClick={() => void revoke(d)}><Trash2 size={14} />ยกเลิก</button>
          </div>
        </li>;
      })}</ul>}
  </section>;
}

function DisplayForm({ draft, isOwner, was, projects, sites, dashboards, busy, onChange, onCancel, onSave }: { draft: Draft; isOwner: boolean; was?: Display; projects: { id: string; name: string }[]; sites: { id: string; name: string }[]; dashboards: { id: string; name: string }[]; busy: boolean; onChange: (d: Draft) => void; onCancel: () => void; onSave: () => void }) {
  const set = (patch: Partial<Draft>) => onChange({ ...draft, ...patch });
  const setView = (i: number, patch: Partial<PlaylistItem>) => set({ playlist: draft.playlist.map((v, j) => (j === i ? { ...v, ...patch } : v)) });
  const move = (i: number, by: number) => { const next = [...draft.playlist]; const [v] = next.splice(i, 1); next.splice(i + by, 0, v); set({ playlist: next }); };
  const valid = draft.name.trim().length > 0 && draft.playlist.length > 0 && draft.playlist.every((v) => v.seconds >= 10 && v.seconds <= 600 && (v.kind !== "studio" || !!v.ref));
  return <form className="dsp-form" onSubmit={(e) => { e.preventDefault(); if (valid) onSave(); }}>
    <h2>{draft.id ? "แก้ไขจอ" : "เพิ่มจอใหม่"}</h2>
    <label>ชื่อจอ<input value={draft.name} maxLength={80} required placeholder="เช่น จอห้อง รปภ. ชั้น 1" onChange={(e) => set({ name: e.target.value })} /></label>
    <fieldset><legend>โปรเจกต์ที่จอนี้เห็น</legend>
      <label className="dsp-check"><input type="checkbox" checked={draft.project_ids.length === 0} onChange={() => set({ project_ids: [] })} />ทุกโปรเจกต์</label>
      {projects.map((p) => <label key={p.id} className="dsp-check"><input type="checkbox" checked={draft.project_ids.includes(p.id)} onChange={(e) => set({ project_ids: e.target.checked ? [...draft.project_ids, p.id] : draft.project_ids.filter((x) => x !== p.id) })} />{p.name}</label>)}
    </fieldset>
    <fieldset><legend>ลำดับหน้าจอ (สูงสุด 12)</legend>
      <ol className="dsp-playlist">{draft.playlist.map((v, i) => <li key={i}>
        <select aria-label="หน้าจอ" value={v.kind} onChange={(e) => setView(i, { kind: e.target.value as ViewKind, ref: undefined })}>{KINDS.map((k) => <option key={k} value={k}>{KIND_LABEL[k]}</option>)}</select>
        {v.kind === "studio" && <select aria-label="Dashboard" value={v.ref ?? ""} onChange={(e) => setView(i, { ref: e.target.value || undefined })}><option value="">เลือก Dashboard</option>{dashboards.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}</select>}
        {(v.kind === "floorplan" || v.kind === "twin") && <select aria-label="อาคาร" value={v.ref ?? ""} onChange={(e) => setView(i, { ref: e.target.value || undefined })}><option value="">อาคารแรกที่เห็น</option>{sites.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}</select>}
        <label className="dsp-seconds"><input type="number" min={10} max={600} value={v.seconds} onChange={(e) => setView(i, { seconds: Number(e.target.value) })} />วินาที</label>
        <button type="button" aria-label="เลื่อนขึ้น" disabled={i === 0} onClick={() => move(i, -1)}><ArrowUp size={14} /></button>
        <button type="button" aria-label="เลื่อนลง" disabled={i === draft.playlist.length - 1} onClick={() => move(i, 1)}><ArrowDown size={14} /></button>
        <button type="button" aria-label="ลบหน้าจอนี้" disabled={draft.playlist.length === 1} onClick={() => set({ playlist: draft.playlist.filter((_, j) => j !== i) })}><Trash2 size={14} /></button>
      </li>)}</ol>
      <button type="button" disabled={draft.playlist.length >= 12} onClick={() => set({ playlist: [...draft.playlist, { kind: "alerts", seconds: 20 }] })}><Plus size={14} />เพิ่มหน้าจอ</button>
    </fieldset>
    <label className="dsp-check"><input type="checkbox" checked={draft.show_names} disabled={!isOwner && !was?.show_names} onChange={(e) => set({ show_names: e.target.checked })} />แสดงชื่อผู้สวมอุปกรณ์บนจอ<small>ปิดไว้: จอแสดง “ผู้สวมใส่ · รหัสท้าย” แทนชื่อ · เปิด: ทุกครั้งที่จอแสดงชื่อจะถูกบันทึกในบันทึกการเข้าถึงข้อมูลส่วนบุคคล (PDPA) และควรแจ้งผู้สวมก่อน</small></label>
    <label className="dsp-check"><input type="checkbox" checked={draft.allow_ack} disabled={!isOwner && !was?.allow_ack} onChange={(e) => set({ allow_ack: e.target.checked })} />ให้กด “รับทราบ” เหตุฉุกเฉินจากจอได้<small>เหมาะกับจอสัมผัสที่เคาน์เตอร์ · การรับทราบจะบันทึกว่ามาจากจอนี้ ไม่ใช่จากพนักงานคนใด</small></label>
    {!isOwner && <p className="dsp-muted">เฉพาะเจ้าของ workspace เท่านั้นที่เปิดการแสดงชื่อหรือการรับทราบจากจอได้ · ผู้ดูแลปิดได้</p>}
    <div className="dsp-form-actions"><button type="button" onClick={onCancel}>ยกเลิก</button><button type="submit" className="dsp-primary" disabled={busy || !valid}>{draft.id ? "บันทึก" : "สร้างและรับรหัสจับคู่"}</button></div>
  </form>;
}
