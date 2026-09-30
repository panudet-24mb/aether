"use client";
import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { DatabaseBackup, RefreshCw, TriangleAlert } from "lucide-react";
import "./system-status.css";
import { createClientFrom } from "../topology/api";
import { useLatest } from "../topology/use-latest";

/** One active problem from the pitr service (GET /api/v1/system/status). */
export type OpsAlert = { key: string; severity: "warn" | "alert"; since?: string; message: string };
/** Backup and point-in-time-recovery health, validated by the server from the pitr service's status file. */
export type BackupStatus = {
  overall: "ok" | "warn" | "alert";
  checked_at?: string;
  stale: boolean;
  alerts: OpsAlert[];
  pitr: { restore_from?: string; newest_backup?: string; backups: number; last_archived_at?: string; wal_waiting: number; pg_wal_mb: number; repo_free_percent: number };
  dump_newest?: string;
};
export type SystemStatus = { configured: false } | { configured: true; backup: BackupStatus };

/** Owner-readable Thai names for the pitr service's alert keys; unknown keys show the server's text alone. */
const ALERT_LABEL: Record<string, string> = {
  postgres: "ตรวจฐานข้อมูลไม่ได้",
  archive_off: "ไม่ได้เก็บ WAL (ย้อนเวลาไม่ได้)",
  archive_failing: "เก็บ WAL ไม่สำเร็จ",
  archive_lagging: "WAL ค้างรอเก็บ",
  wal_growing: "pg_wal โตผิดปกติ",
  wal_dropped: "WAL ถูกทิ้ง มีช่วงที่ย้อนเวลาไม่ได้",
  repo_status: "คลังสำรองข้อมูลมีปัญหา",
  backup_stale: "สำรองข้อมูลล่าสุดเก่าเกินไป",
  backup_failed: "สำรองข้อมูลไม่สำเร็จ",
  repo_disk: "ดิสก์สำรองข้อมูลใกล้เต็ม",
  dump_stale: "dump รายคืนเก่าเกินไป",
  dump_failed: "dump รายคืนไม่สำเร็จ",
  stanza: "ตั้งค่า pgBackRest ไม่สำเร็จ",
  status_stale: "สถานะไม่อัปเดต",
  status_missing: "ไม่มีไฟล์สถานะ",
  status_invalid: "อ่านไฟล์สถานะไม่ได้",
};

const POLL_MS = 5 * 60 * 1000;
const when = (s?: string) => (s ? new Date(s).toLocaleString("th-TH", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "—");

type Source = { status: SystemStatus | null; failed: boolean; reload: () => void };
const StatusContext = createContext<Source>({ status: null, failed: false, reload: () => {} });

/**
 * One source of the status for the banner and the panel, so a reload in the panel also updates the banner. Loads
 * when enabled (owners), then every five minutes while the page is visible; a hidden tab does not poll, and
 * becoming visible again reloads at once when the last load is older than that.
 */
export function SystemStatusProvider({ enabled, getToken, refresh, children }: { enabled: boolean; getToken: () => string; refresh: () => Promise<boolean>; children: ReactNode }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const [status, setStatus] = useState<SystemStatus | null>(null);
  const [failed, setFailed] = useState(false);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!enabled) return;
    let active = true;
    let last = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const load = async () => {
      clearTimeout(timer);
      last = Date.now();
      try {
        const s = await client.raw<SystemStatus>("/system/status");
        if (active) {
          setStatus(s);
          setFailed(false);
        }
      } catch {
        if (active) setFailed(true);
      }
      if (active && !document.hidden) timer = setTimeout(() => void load(), POLL_MS);
    };
    const onVisibility = () => {
      if (document.hidden) clearTimeout(timer);
      else if (Date.now() - last >= POLL_MS) void load();
      else timer = setTimeout(() => void load(), POLL_MS - (Date.now() - last));
    };
    void load();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      active = false;
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [client, enabled, tick]);
  const reload = useCallback(() => setTick((n) => n + 1), []);
  return <StatusContext.Provider value={{ status: enabled ? status : null, failed, reload }}>{children}</StatusContext.Provider>;
}

/**
 * A slim bar floating over the page when backups need attention: nothing, and no space reserved, when all is well
 * or the feature is off, so the page never shifts when the status arrives.
 */
export function SystemStatusBanner({ onOpen }: { onOpen: () => void }) {
  const { status } = useContext(StatusContext);
  if (!status?.configured || status.backup.overall === "ok") return null;
  const b = status.backup;
  const first = b.alerts.find((a) => a.severity === "alert") ?? b.alerts[0];
  return (
    <div className={`sys-banner is-${b.overall}`} role="status">
      <TriangleAlert size={17} aria-hidden="true" />
      <div>
        <strong>{b.overall === "alert" ? "สำรองข้อมูลมีปัญหา" : "สำรองข้อมูลต้องดูแล"}</strong>
        <small>
          {first ? ALERT_LABEL[first.key] ?? first.key : ""}
          {b.alerts.length > 1 ? ` และอีก ${b.alerts.length - 1} เรื่อง` : ""}
        </small>
      </div>
      <button type="button" onClick={onOpen}>
        ดูสถานะระบบ
      </button>
    </div>
  );
}

/** The owner's detail view: overall state, active problems and the backup timeline. */
export function SystemStatusPanel() {
  const { status, failed, reload } = useContext(StatusContext);
  if (failed && !status) return <p className="sys-empty">โหลดสถานะระบบไม่สำเร็จ</p>;
  if (!status) return <p className="sys-empty">กำลังโหลด…</p>;
  if (!status.configured) return <p className="sys-empty">การติดตั้งนี้ไม่ได้เปิดรายงานสถานะสำรองข้อมูล (OPS_STATUS_FILE) · ใช้กับชุด production เท่านั้น</p>;
  const b = status.backup;
  const label = b.overall === "ok" ? "ปกติ" : b.overall === "warn" ? "ต้องดูแล" : "มีปัญหา";
  return (
    <section className="sys-panel" aria-label="สถานะระบบ">
      <div className={`sys-overall is-${b.overall}`}>
        <DatabaseBackup size={20} aria-hidden="true" />
        <div>
          <strong>สำรองข้อมูลและย้อนเวลา (PITR): {label}</strong>
          <small>ตรวจล่าสุด {when(b.checked_at)}{b.stale ? " · ข้อมูลอาจไม่เป็นปัจจุบัน" : ""}</small>
        </div>
        <button type="button" className="sys-icon" onClick={reload} aria-label="โหลดใหม่" title="โหลดใหม่">
          <RefreshCw size={15} />
        </button>
      </div>
      {b.alerts.length > 0 && (
        <ul className="sys-alerts">
          {b.alerts.map((a, i) => (
            <li key={`${a.key}-${i}`} className={`is-${a.severity}`}>
              <strong>{ALERT_LABEL[a.key] ?? a.key}</strong>
              <small>
                {a.since ? `ตั้งแต่ ${when(a.since)} · ` : ""}
                {a.message}
              </small>
            </li>
          ))}
        </ul>
      )}
      <dl className="sys-facts">
        <div>
          <dt>ย้อนเวลาได้ตั้งแต่</dt>
          <dd>{when(b.pitr.restore_from)}</dd>
        </div>
        <div>
          <dt>สำรองเต็มล่าสุด</dt>
          <dd>
            {when(b.pitr.newest_backup)} · {b.pitr.backups} ชุด
          </dd>
        </div>
        <div>
          <dt>เก็บ WAL ล่าสุด</dt>
          <dd>
            {when(b.pitr.last_archived_at)}
            {b.pitr.wal_waiting > 0 ? ` · ค้าง ${b.pitr.wal_waiting}` : ""}
          </dd>
        </div>
        <div>
          <dt>dump รายคืนล่าสุด</dt>
          <dd>{when(b.dump_newest)}</dd>
        </div>
        <div>
          <dt>ดิสก์สำรองข้อมูลว่าง</dt>
          <dd>{b.pitr.repo_free_percent}%</dd>
        </div>
      </dl>
      <p className="sys-note">ข้อมูลจากบริการ pitr ตรวจทุก 5 นาที · วิธีแก้แต่ละเรื่องอยู่ใน docs/production.md หัวข้อการเฝ้าระวัง</p>
    </section>
  );
}
