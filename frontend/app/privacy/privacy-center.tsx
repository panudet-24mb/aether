"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { DatabaseBackup, Eye, FileClock, RefreshCw, ScrollText, UserX } from "lucide-react";
import "./privacy.css";
import { ApiError, createClientFrom } from "../topology/api";
import { useLatest } from "../topology/use-latest";
import { SystemStatusPanel } from "../system/system-status";

/** One read of personal data (GET /api/v1/privacy/access-log). */
type AccessRow = { id: string; at: string; actor_id: string; actor_name: string | null; resource: string; subject_kind: string; subject_id: string; client_ip: string | null };
/** One change (GET /api/v1/privacy/audit). */
type AuditRow = { id: string; at: string; actor_id: string; actor_name: string | null; action: string; target_id: string };
/** One erasure (GET /api/v1/privacy/erasures). */
type ErasureRow = { id: string; at: string; actor_id: string | null; subject_kind: string; subject_ref: string; counts: Record<string, unknown> };
type Page<T> = { items: T[]; next?: { before_at: string; before_id: string } };
export type PrivacyTab = "access" | "audit" | "erasures" | "system";
type Tab = PrivacyTab;

const RESOURCE_LABEL: Record<string, string> = {
  members: "รายชื่อสมาชิก",
  member_access: "สิทธิ์ของสมาชิก",
  presence: "ตำแหน่งของแท็ก",
  alerts: "การแจ้งเตือน",
  events: "เหตุการณ์",
  live: "ภาพรวม (ค่าจากเซนเซอร์)",
  studio_render: "Dashboard Studio",
  realtime: "สัญญาณเรียลไทม์",
  access_log: "บันทึกการเข้าถึง",
  audit_log: "บันทึกการตรวจสอบ",
  erasure_log: "บันทึกการลบข้อมูล",
  export: "ส่งออกข้อมูล",
};
const SUBJECT_LABEL: Record<string, string> = { member: "สมาชิก", device_identity: "แท็ก", device: "อุปกรณ์", member_list: "ทุกคน", alert_list: "ทั้งหมด", event_list: "ทั้งหมด", sensor_history: "ทั้งหมด", signal_stream: "ทั้งหมด", access_log: "—", audit_log: "—", erasure_log: "—" };
const ACTION_PREFIXES: [string, string][] = [["", "ทุกการกระทำ"], ["member.", "สมาชิก"], ["session.", "การเข้าระบบ"], ["device.", "อุปกรณ์"], ["privacy.", "ความเป็นส่วนตัว"], ["gateway.", "gateway"], ["automation", "automation"]];

const when = (s: string) => new Date(s).toLocaleString("th-TH", { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });
const short = (id: string) => (id.length > 12 ? `${id.slice(0, 8)}…` : id);
const who = (name: string | null, id: string | null) => (id === null ? "ผู้ดูแลระบบ (platform)" : name ?? `อดีตสมาชิก · ${short(id)}`);

/**
 * The owner's privacy view: who read personal data (the read-access log), who changed what (the audit trail) and
 * what was erased. Owner only; the server refuses everybody else and logs this view's own reads.
 */
export default function PrivacyCenter({ getToken, refresh, onUnauthorized, initialTab = "access" }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void; initialTab?: Tab }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const [tab, setTab] = useState<Tab>(initialTab);
  const [resource, setResource] = useState("");
  const [action, setAction] = useState("");
  const [from, setFrom] = useState("");
  const [access, setAccess] = useState<Page<AccessRow>>({ items: [] });
  const [audit, setAudit] = useState<Page<AuditRow>>({ items: [] });
  const [erasures, setErasures] = useState<ErasureRow[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const unauthorized = useRef(false);

  const fail = useCallback(
    (e: unknown, fallback: string) => {
      if (e instanceof ApiError && e.status === 401 && !unauthorized.current) {
        unauthorized.current = true;
        onUnauthorized?.();
      }
      setError(e instanceof ApiError && e.status === 403 ? "เฉพาะเจ้าของ workspace เท่านั้น" : e instanceof Error ? e.message : fallback);
    },
    [onUnauthorized],
  );

  const query = useCallback(
    (cursor?: { before_at: string; before_id: string }) => {
      const q = new URLSearchParams({ limit: "100" });
      if (tab === "access" && resource) q.set("resource", resource);
      if (tab === "audit" && action) q.set("action", action);
      if (from) q.set("from", new Date(`${from}T00:00:00`).toISOString());
      if (cursor) {
        q.set("before_at", cursor.before_at);
        q.set("before_id", cursor.before_id);
      }
      return q.toString();
    },
    [tab, resource, action, from],
  );

  const load = useCallback(
    async (more = false) => {
      setBusy(true);
      try {
        if (tab === "access") {
          const page = await client.raw<Page<AccessRow>>(`/privacy/access-log?${query(more ? access.next : undefined)}`);
          setAccess(more ? { items: [...access.items, ...page.items], next: page.next } : page);
        } else if (tab === "audit") {
          const page = await client.raw<Page<AuditRow>>(`/privacy/audit?${query(more ? audit.next : undefined)}`);
          setAudit(more ? { items: [...audit.items, ...page.items], next: page.next } : page);
        } else if (tab === "erasures") {
          setErasures((await client.raw<{ items: ErasureRow[] }>("/privacy/erasures")).items);
        }
        setError("");
      } catch (e) {
        fail(e, "โหลดไม่สำเร็จ");
      } finally {
        setBusy(false);
      }
    },
    [client, tab, query, access, audit, fail],
  );

  // Reload when the tab or a filter changes (not when a page is appended).
  const loadRef = useLatest(load);
  useEffect(() => {
    const id = requestAnimationFrame(() => void loadRef.current(false));
    return () => cancelAnimationFrame(id);
  }, [tab, resource, action, from, loadRef]);

  const next = tab === "access" ? access.next : tab === "audit" ? audit.next : undefined;

  return (
    <div className="pv">
      <header className="pv-bar">
        <div>
          <div className="pv-kicker">PDPA · OWNER</div>
          <h1>ความเป็นส่วนตัวและบันทึกการเข้าถึง</h1>
        </div>
        <nav className="pv-tabs" aria-label="มุมมอง">
          <button type="button" className={tab === "access" ? "is-on" : ""} onClick={() => setTab("access")}>
            <Eye size={15} /> ใครเปิดดูข้อมูล
          </button>
          <button type="button" className={tab === "audit" ? "is-on" : ""} onClick={() => setTab("audit")}>
            <FileClock size={15} /> ใครเปลี่ยนอะไร
          </button>
          <button type="button" className={tab === "erasures" ? "is-on" : ""} onClick={() => setTab("erasures")}>
            <UserX size={15} /> การลบข้อมูล
          </button>
          <button type="button" className={tab === "system" ? "is-on" : ""} onClick={() => setTab("system")}>
            <DatabaseBackup size={15} /> สถานะระบบ
          </button>
          {tab !== "system" && (
            <button type="button" className="pv-icon" onClick={() => void load(false)} disabled={busy} aria-label="โหลดใหม่" title="โหลดใหม่">
              <RefreshCw size={15} />
            </button>
          )}
        </nav>
      </header>
      {error && (
        <div className="pv-alert" role="alert">
          {error}
        </div>
      )}
      <div className="pv-body">
        {tab === "system" && <SystemStatusPanel />}
        {tab !== "system" && <p className="pv-note">
          <ScrollText size={14} /> ระบบบันทึกทุกครั้งที่มีคนเปิดดูข้อมูลส่วนบุคคล (รายชื่อสมาชิก ตำแหน่งและประวัติของแท็กที่มีคนสวม การแจ้งเตือน) · การอ่านซ้ำเรื่องเดิมภายใน 10 นาทีนับเป็นครั้งเดียว ·
          ถ้าบันทึกไม่ได้ ระบบจะไม่ส่งข้อมูลออกไป · เก็บไว้ตาม ACCESS_LOG_RETENTION_DAYS (ค่าเริ่มต้น 400 วัน)
        </p>}
        {(tab === "access" || tab === "audit") && (
          <div className="pv-toolbar">
            {tab === "access" ? (
              <label>
                ข้อมูล
                <select value={resource} onChange={(e) => setResource(e.target.value)}>
                  <option value="">ทั้งหมด</option>
                  {Object.entries(RESOURCE_LABEL).map(([id, label]) => (
                    <option key={id} value={id}>
                      {label}
                    </option>
                  ))}
                </select>
              </label>
            ) : (
              <label>
                การกระทำ
                <select value={action} onChange={(e) => setAction(e.target.value)}>
                  {ACTION_PREFIXES.map(([id, label]) => (
                    <option key={id} value={id}>
                      {label}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <label>
              ตั้งแต่วันที่
              <input type="date" value={from} onChange={(e) => setFrom(e.target.value)} />
            </label>
          </div>
        )}

        {tab === "access" && (
          <table className="pv-table">
            <thead>
              <tr>
                <th>เวลา</th>
                <th>ผู้เปิดดู</th>
                <th>ข้อมูล</th>
                <th>ของใคร</th>
                <th>IP</th>
              </tr>
            </thead>
            <tbody>
              {access.items.map((r) => (
                <tr key={r.id}>
                  <td className="pv-dim">{when(r.at)}</td>
                  <td>{who(r.actor_name, r.actor_id)}</td>
                  <td>{RESOURCE_LABEL[r.resource] ?? r.resource}</td>
                  <td>
                    {SUBJECT_LABEL[r.subject_kind] ?? r.subject_kind}
                    {r.subject_id !== "*" && <code> {short(r.subject_id)}</code>}
                  </td>
                  <td className="pv-dim">
                    <code>{r.client_ip ?? "—"}</code>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {tab === "audit" && (
          <table className="pv-table">
            <thead>
              <tr>
                <th>เวลา</th>
                <th>ผู้กระทำ</th>
                <th>การกระทำ</th>
                <th>เป้าหมาย</th>
              </tr>
            </thead>
            <tbody>
              {audit.items.map((r) => (
                <tr key={r.id}>
                  <td className="pv-dim">{when(r.at)}</td>
                  <td>{who(r.actor_name, r.actor_id)}</td>
                  <td>
                    <code>{r.action}</code>
                  </td>
                  <td className="pv-dim">
                    <code>{short(r.target_id)}</code>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {tab === "erasures" && (
          <table className="pv-table">
            <thead>
              <tr>
                <th>เวลา</th>
                <th>ผู้สั่งลบ</th>
                <th>ข้อมูลของ</th>
                <th>จำนวนที่ลบ</th>
              </tr>
            </thead>
            <tbody>
              {erasures.map((r) => (
                <tr key={r.id}>
                  <td className="pv-dim">{when(r.at)}</td>
                  <td>{r.actor_id === null ? "ผู้ดูแลระบบ (platform)" : <code>{short(r.actor_id)}</code>}</td>
                  <td>
                    {r.subject_kind === "member" ? "สมาชิก" : "แท็ก"} <code>{short(r.subject_ref)}</code>
                  </td>
                  <td className="pv-dim">
                    {Object.entries(r.counts)
                      .filter(([, v]) => typeof v === "number" && v > 0)
                      .map(([k, v]) => `${k} ${v}`)
                      .join(" · ") || "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {((tab === "access" && access.items.length === 0) || (tab === "audit" && audit.items.length === 0) || (tab === "erasures" && erasures.length === 0)) && !busy && (
          <div className="pv-empty">ยังไม่มีรายการ</div>
        )}
        {next && (
          <button type="button" className="pv-btn" disabled={busy} onClick={() => void load(true)}>
            โหลดเพิ่ม
          </button>
        )}
        {tab === "erasures" && (
          <p className="pv-note">
            การลบสมาชิกทำที่หน้า “ทีมและสิทธิ์” · การลบประวัติของแท็กที่มีคนสวมทำที่แผงอุปกรณ์ในหน้า “เชื่อมต่ออุปกรณ์” · หลังกู้คืนข้อมูลจากสำรอง ผู้ดูแลระบบต้องรัน
            <code> admin reapply-erasures</code> เพื่อลบซ้ำ (docs/production.md)
          </p>
        )}
      </div>
    </div>
  );
}
