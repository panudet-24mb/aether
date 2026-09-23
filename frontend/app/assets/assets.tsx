"use client";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ArrowDown, ArrowUp, BatteryLow, CalendarClock, ChevronRight, Download, ExternalLink, FileText, Plus, RefreshCw, Router, Search, ShieldCheck, Trash2, Wrench } from "lucide-react";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import "./assets.css";
import { ApiError, createClientFrom } from "../topology/api";
import { useLatest } from "../topology/use-latest";
import { useSignals } from "../topology/use-signals";
import { applyCatalog, deviceProfile, formatMAC, gatewayModel } from "../topology/catalog";

const API = import.meta.env.VITE_AETHER_API_ORIGIN ?? "";

type AssetKind = "gateway" | "device";
type AssetRow = {
  asset_kind: AssetKind;
  asset_id: string;
  name: string;
  model: string;
  external_id: string;
  gateway_id: string;
  gateway_name: string;
  project_name: string;
  last_seen: string | null;
  battery: number | null;
  status: string;
  serial_no: string;
  asset_tag: string;
  location_note: string;
  warranty_until: string;
  battery_changed_at: string;
  notes: string;
  next_due: string;
  overdue_count: number;
  created_at: string;
};
type AssetRecord = { asset_kind: AssetKind; asset_id: string; serial_no: string; asset_tag: string; location_note: string; vendor: string; purchased_at: string; warranty_until: string; battery_changed_at: string; status: string; notes: string; updated_at?: string };
type Plan = { id: string; asset_kind: AssetKind; asset_id: string; title: string; kind: string; interval_days: number; next_due: string; last_done: string; enabled: boolean; created_at: string };
type Log = { id: string; asset_kind: AssetKind; asset_id: string; plan_id: string | null; kind: string; title: string; detail: string; performed_at: string; performed_by: string; cost: number | null; created_at: string };
type Detail = { asset: AssetRow; record: AssetRecord; plans: Plan[]; logs: Log[] };
type Chip = "all" | "overdue" | "due30" | "battery" | "warranty" | "offline";
type SortKey = "name" | "model" | "project_name" | "gateway_name" | "status" | "last_seen" | "battery" | "asset_tag" | "next_due";
type Tab = "info" | "plans" | "history";

const STATUS_LABEL: Record<string, string> = { in_service: "ใช้งาน", spare: "สำรอง", repair: "ซ่อม", retired: "ปลดระวาง" };
const PLAN_KIND_LABEL: Record<string, string> = { pm: "PM ตามรอบ", calibration: "สอบเทียบ", battery: "เปลี่ยนแบตเตอรี่", inspection: "ตรวจสภาพ", other: "อื่น ๆ" };
const LOG_KIND_LABEL: Record<string, string> = { pm: "PM ตามรอบ", ma: "MA แก้ไขเหตุขัดข้อง", repair: "ซ่อม", battery: "เปลี่ยนแบตเตอรี่", calibration: "สอบเทียบ", inspection: "ตรวจสภาพ", note: "บันทึกทั่วไป" };
// A plan of kind "other" is still a planned round, so completing it logs a PM; only a free note never advances a plan.
const PLAN_TO_LOG: Record<string, string> = { pm: "pm", calibration: "calibration", battery: "battery", inspection: "inspection", other: "pm" };
const VIEWER_ONLY = "บัญชีนี้ดูได้อย่างเดียว";
/** No decoded frame and no packet for this long counts as offline in the registry. */
const OFFLINE_MS = 15 * 60 * 1000;
const DUE_SOON_DAYS = 30;
const WARRANTY_SOON_DAYS = 60;
const LOW_BATTERY = 20;

const today = () => new Date().toISOString().slice(0, 10);
/**
 * Whole days from `ref` (the server's calendar day) to a YYYY-MM-DD day; null when either is not set.
 * "Now" always comes from the list response, never from Date.now(), so rendering stays pure.
 */
function daysUntil(day: string, ref: string): number | null {
  if (!day || !ref) return null;
  const then = Date.parse(day + "T00:00:00Z");
  if (Number.isNaN(then)) return null;
  return Math.round((then - Date.parse(ref + "T00:00:00Z")) / 86400000);
}
const when = (s: string | null | undefined) => (s ? new Date(s).toLocaleString("th-TH", { hour12: false }) : "—");
const money = (v: number | null) => (v === null ? "" : v.toLocaleString("th-TH", { minimumFractionDigits: 2, maximumFractionDigits: 2 }));
const photoOf = (row: Pick<AssetRow, "asset_kind" | "model">) => (row.asset_kind === "gateway" ? gatewayModel(row.model)?.image : deviceProfile(row.model)?.image);
const modelLabel = (row: Pick<AssetRow, "asset_kind" | "model">) => (row.asset_kind === "gateway" ? gatewayModel(row.model)?.label : deviceProfile(row.model)?.label) ?? row.model;
const keyOf = (row: Pick<AssetRow, "asset_kind" | "asset_id">) => `${row.asset_kind}/${row.asset_id}`;

type RecordForm = { serial_no: string; asset_tag: string; location_note: string; vendor: string; purchased_at: string; warranty_until: string; battery_changed_at: string; status: string; notes: string };
const toForm = (r: AssetRecord): RecordForm => ({ serial_no: r.serial_no, asset_tag: r.asset_tag, location_note: r.location_note, vendor: r.vendor, purchased_at: r.purchased_at, warranty_until: r.warranty_until, battery_changed_at: r.battery_changed_at, status: r.status || "in_service", notes: r.notes });
type PlanForm = { id: string; title: string; kind: string; interval_days: string; next_due: string; enabled: boolean };
const emptyPlan = (): PlanForm => ({ id: "", title: "", kind: "pm", interval_days: "180", next_due: today(), enabled: true });
type LogForm = { plan_id: string | null; kind: string; title: string; detail: string; performed_at: string; performed_by: string; cost: string };
const emptyLog = (): LogForm => ({ plan_id: null, kind: "ma", title: "", detail: "", performed_at: today(), performed_by: "", cost: "" });

export default function AssetsPage({ getToken, refresh, onUnauthorized, onOpenDevice }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void; onOpenDevice?: (external: string) => void }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const [rows, setRows] = useState<AssetRow[]>([]);
  // The server's clock at the last load; freshness is judged against it so no render reads Date.now().
  const [serverTime, setServerTime] = useState(0);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [chip, setChip] = useState<Chip>("all");
  const [projectOptions, setProjectOptions] = useState<{id:string;name:string}[]>([]);
  const [gatewayOptions, setGatewayOptions] = useState<{id:string;name:string;project_id?:string|null}[]>([]);
  const [project, setProject] = useState("");
  const [kindFilter, setKindFilter] = useState("");
  const [sortKey, setSortKey] = useState<SortKey>("name");
  const [sortDir, setSortDir] = useState<1 | -1>(1);
  const [open, setOpen] = useState<{ kind: AssetKind; id: string } | null>(null);
  const [detail, setDetail] = useState<Detail | null>(null);
  const [tab, setTab] = useState<Tab>("info");
  const [form, setForm] = useState<RecordForm | null>(null);
  const [dirty, setDirty] = useState(false);
  const [planForm, setPlanForm] = useState<PlanForm | null>(null);
  const [logForm, setLogForm] = useState<LogForm | null>(null);
  const [drawerError, setDrawerError] = useState("");
  const [drawerNotice, setDrawerNotice] = useState("");
  const unauthorized = useRef(false);
  const catalogLoaded = useRef(false);

  useEffect(() => {
    const t = setTimeout(() => setQuery(queryInput.trim().toLowerCase()), 350); // one filter pass after typing stops
    return () => clearTimeout(t);
  }, [queryInput]);

  const fail = useCallback(
    (e: unknown) => {
      if (e instanceof ApiError && e.status === 401 && !unauthorized.current) {
        unauthorized.current = true;
        onUnauthorized?.();
      }
      if (e instanceof ApiError && e.status === 403) return VIEWER_ONLY;
      return e instanceof Error ? e.message : "ดำเนินการไม่สำเร็จ";
    },
    [onUnauthorized],
  );

  const load = useCallback(async () => {
    try {
      if (!catalogLoaded.current) {
        catalogLoaded.current = true;
        try {
          applyCatalog(await client.catalog()); // product photos and labels; the bundled fallback list stays if it fails
        } catch {
          catalogLoaded.current = false;
        }
      }
      const list = await client.raw<{ items: AssetRow[]; server_time: string }>("/assets");
      const [ps, gs] = await Promise.all([client.raw<{items:{id:string;name:string}[]}>("/projects"), client.raw<{items:{id:string;name:string;project_id?:string|null}[]}>("/gateways")]);
      setProjectOptions(ps.items); setGatewayOptions(gs.items);
      setRows(list.items);
      setServerTime(Date.parse(list.server_time));
      setError("");
    } catch (e) {
      setError(fail(e));
    }
  }, [client, fail]);

  // The drawer keeps polling too, but a form the user is typing into is never overwritten.
  const dirtyRef = useLatest(dirty);
  const openRef = useLatest(open);
  const loadDetail = useCallback(
    async (kind: AssetKind, id: string) => {
      const d = await client.raw<Detail>(`/assets/${kind}/${id}`);
      // A slower answer for a row the user already left must not fill the drawer of another asset.
      const target = openRef.current;
      if (!target || target.kind !== kind || target.id !== id) return d;
      setDetail(d);
      if (!dirtyRef.current) setForm(toForm(d.record));
      return d;
    },
    [client, dirtyRef, openRef],
  );

  const refreshAll = useCallback(async () => {
    await load();
    const target = openRef.current;
    if (target) {
      try {
        await loadDetail(target.kind, target.id);
      } catch (e) {
        setDrawerError(fail(e));
      }
    }
  }, [load, loadDetail, openRef, fail]);

  const refreshRef = useLatest(refreshAll);
  const pushTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const live = useSignals(handlers, (signalKind) => {
    if (signalKind !== "inventory") return;
    clearTimeout(pushTimer.current);
    pushTimer.current = setTimeout(() => void refreshRef.current(), 400);
  });
  useEffect(() => () => clearTimeout(pushTimer.current), []);
  const liveRef = useLatest(live);

  useEffect(() => {
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      await refreshRef.current();
      // The socket does the real work; the poll is only a safety net while it is down.
      if (active && !unauthorized.current) timer = setTimeout(poll, liveRef.current ? 60000 : 15000);
    };
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [refreshRef, liveRef]);

  useEffect(() => {
    if (!notice) return;
    const t = setTimeout(() => setNotice(""), 3000);
    return () => clearTimeout(t);
  }, [notice]);
  useEffect(() => {
    if (!drawerNotice) return;
    const t = setTimeout(() => setDrawerNotice(""), 3000);
    return () => clearTimeout(t);
  }, [drawerNotice]);

  const todayISO = useMemo(() => (serverTime ? new Date(serverTime).toISOString().slice(0, 10) : ""), [serverTime]);
  const isOffline = useCallback((r: AssetRow) => !r.last_seen || serverTime - Date.parse(r.last_seen) > OFFLINE_MS, [serverTime]);

  const counts = useMemo(() => {
    const c = { all: rows.length, overdue: 0, due30: 0, battery: 0, warranty: 0, offline: 0 };
    for (const r of rows) {
      if (r.overdue_count > 0) c.overdue++;
      const due = daysUntil(r.next_due, todayISO);
      if (due !== null && due >= 0 && due <= DUE_SOON_DAYS) c.due30++;
      if (r.battery !== null && r.battery < LOW_BATTERY) c.battery++;
      const warranty = daysUntil(r.warranty_until, todayISO);
      if (warranty !== null && warranty < WARRANTY_SOON_DAYS) c.warranty++;
      if (isOffline(r)) c.offline++;
    }
    return c;
  }, [rows, todayISO, isOffline]);

  const projects = projectOptions;

  const visible = useMemo(() => {
    const matches = (r: AssetRow) => {
      if (kindFilter && r.asset_kind !== kindFilter) return false;
      if (project && (gatewayOptions.find((g) => g.id === (r.asset_kind === "gateway" ? r.asset_id : r.gateway_id))?.project_id ?? "none") !== project) return false;
      if (query && ![r.name, r.external_id, r.asset_id, r.asset_tag, r.serial_no, r.gateway_name, r.project_name, r.location_note, r.notes, modelLabel(r)].some((v) => v.toLowerCase().includes(query))) return false;
      const due = daysUntil(r.next_due, todayISO);
      const warranty = daysUntil(r.warranty_until, todayISO);
      switch (chip) {
        case "overdue":
          return r.overdue_count > 0;
        case "due30":
          return due !== null && due >= 0 && due <= DUE_SOON_DAYS;
        case "battery":
          return r.battery !== null && r.battery < LOW_BATTERY;
        case "warranty":
          return warranty !== null && warranty < WARRANTY_SOON_DAYS;
        case "offline":
          return isOffline(r);
        default:
          return true;
      }
    };
    const value = (r: AssetRow): string | number => {
      switch (sortKey) {
        case "last_seen":
          return r.last_seen ? Date.parse(r.last_seen) : 0;
        case "battery":
          return r.battery ?? -1;
        case "next_due":
          return r.next_due || "9999-12-31";
        case "model":
          return modelLabel(r);
        default:
          return r[sortKey] ?? "";
      }
    };
    return rows.filter(matches).sort((a, b) => {
      const va = value(a);
      const vb = value(b);
      const cmp = typeof va === "number" && typeof vb === "number" ? va - vb : String(va).localeCompare(String(vb), "th");
      return cmp * sortDir || a.name.localeCompare(b.name, "th");
    });
  }, [rows, query, chip, project, kindFilter, sortKey, sortDir, todayISO, isOffline, gatewayOptions]);

  function sortBy(key: SortKey) {
    if (key === sortKey) setSortDir(sortDir === 1 ? -1 : 1);
    else {
      setSortKey(key);
      setSortDir(1);
    }
  }

  function openAsset(row: AssetRow) {
    setOpen({ kind: row.asset_kind, id: row.asset_id });
    setTab("info");
    setDetail(null);
    setForm(null);
    setDirty(false);
    setPlanForm(null);
    setLogForm(null);
    setDrawerError("");
    setDrawerNotice("");
    void loadDetail(row.asset_kind, row.asset_id).catch((e: unknown) => setDrawerError(fail(e)));
  }

  function closeDrawer() {
    setOpen(null);
    setDetail(null);
    setForm(null);
    setDirty(false);
  }

  async function exportCSV() {
    setBusy(true);
    try {
      const send = () => fetch(`${API}/api/v1/assets/export.csv`, { headers: { Authorization: `Bearer ${handlers.current.getToken()}` }, cache: "no-store" });
      let r = await send();
      if (r.status === 401 && (await handlers.current.refresh())) r = await send();
      if (!r.ok) throw new ApiError(r.status, `ดาวน์โหลด CSV ไม่สำเร็จ (${r.status})`);
      const url = URL.createObjectURL(await r.blob());
      const link = document.createElement("a");
      link.href = url;
      link.download = "aether-assets.csv";
      document.body.appendChild(link);
      link.click();
      link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000); // revoking at once cancels the download in Firefox
      setNotice("ดาวน์โหลด CSV แล้ว");
    } catch (e) {
      setError(fail(e));
    } finally {
      setBusy(false);
    }
  }

  // Every drawer mutation goes through here so a 403 always reads as "view only" and the list stays in step.
  async function mutate(fn: () => Promise<void>, ok: string) {
    setBusy(true);
    setDrawerError("");
    try {
      await fn();
      setDrawerNotice(ok);
      await load();
    } catch (e) {
      setDrawerError(fail(e));
    } finally {
      setBusy(false);
    }
  }

  const path = open ? `/assets/${open.kind}/${open.id}` : "";

  async function saveRecord() {
    if (!form || !open) return;
    await mutate(async () => {
      const d = await client.post<Detail>(path, form);
      setDetail(d);
      setForm(toForm(d.record));
      setDirty(false);
    }, "บันทึกข้อมูลแล้ว");
  }

  async function savePlan(input: PlanForm) {
    if (!open) return;
    const interval = Number(input.interval_days);
    if (!input.title.trim()) {
      setDrawerError("กรุณากรอกชื่อแผน");
      return;
    }
    if (!Number.isInteger(interval) || interval < 1 || interval > 3650) {
      setDrawerError("รอบต้องเป็นจำนวนวันระหว่าง 1 ถึง 3650");
      return;
    }
    if (!input.next_due) {
      setDrawerError("กรุณาระบุวันครบกำหนดครั้งถัดไป");
      return;
    }
    const payload = { title: input.title.trim(), kind: input.kind, interval_days: interval, next_due: input.next_due, enabled: input.enabled };
    await mutate(async () => {
      await client.post<Plan>(input.id ? `/plans/${input.id}/update` : `${path}/plans`, payload);
      setPlanForm(null);
      await loadDetail(open.kind, open.id);
    }, input.id ? "แก้ไขแผนแล้ว" : "เพิ่มแผนแล้ว");
  }

  async function deletePlan(plan: Plan) {
    if (!open) return;
    await mutate(async () => {
      await client.post<void>(`/plans/${plan.id}/delete`, {});
      await loadDetail(open.kind, open.id);
    }, "ลบแผนแล้ว");
  }

  async function addLog(input: LogForm) {
    if (!open) return;
    if (!input.title.trim()) {
      setDrawerError("กรุณากรอกหัวข้อ");
      return;
    }
    if (!input.performed_at) {
      setDrawerError("กรุณาระบุวันที่ดำเนินการ");
      return;
    }
    const cost = input.cost.trim() === "" ? null : Number(input.cost);
    if (cost !== null && (!Number.isFinite(cost) || cost < 0)) {
      setDrawerError("ค่าใช้จ่ายต้องเป็นตัวเลขไม่ติดลบ");
      return;
    }
    await mutate(async () => {
      const d = await client.post<Detail>(`${path}/logs`, { plan_id: input.plan_id, kind: input.kind, title: input.title.trim(), detail: input.detail.trim(), performed_at: input.performed_at, performed_by: input.performed_by.trim(), cost });
      setDetail(d);
      if (!dirtyRef.current) setForm(toForm(d.record));
      setLogForm(null);
    }, "บันทึกประวัติแล้ว");
  }

  const chips: [Chip, string, number, string][] = [
    ["all", "ทั้งหมด", counts.all, ""],
    ["overdue", "เกินกำหนด MA/PM", counts.overdue, "is-hot"],
    ["due30", `ครบกำหนดใน ${DUE_SOON_DAYS} วัน`, counts.due30, "is-warn"],
    ["battery", `แบตเตอรี่ต่ำ (<${LOW_BATTERY}%)`, counts.battery, "is-hot"],
    ["warranty", `ประกันใกล้หมด (<${WARRANTY_SOON_DAYS} วัน)`, counts.warranty, "is-warn"],
    ["offline", "offline", counts.offline, "is-warn"],
  ];
  const columns: [SortKey | "", string][] = [
    ["", ""],
    ["name", "ชื่อ"],
    ["model", "ชนิด / รุ่น"],
    ["", "MAC / ID"],
    ["project_name", "โปรเจค"],
    ["gateway_name", "Gateway"],
    ["status", "สถานะใช้งาน"],
    ["last_seen", "Last seen"],
    ["battery", "แบตเตอรี่"],
    ["asset_tag", "Asset tag / Serial"],
    ["next_due", "MA/PM ถัดไป"],
    ["", "หมายเหตุ"],
  ];

  return (
    <section className="ar">
      <header className="ar-bar">
        <h1 className="ar-title">อุปกรณ์ทั้งหมด</h1>
        <div className="ar-chips" role="group" aria-label="ตัวกรองสรุป">
          {chips.map(([id, label, n, tone]) => (
            <button key={id} type="button" className={`ar-chip ${chip === id ? "is-on" : ""} ${n > 0 ? tone : ""}`} aria-pressed={chip === id} onClick={() => setChip(id)} title={id === "offline" ? `ไม่มีข้อมูลเข้ามาเกิน ${OFFLINE_MS / 60000} นาที` : label}>
              <b>{n}</b> {label}
            </button>
          ))}
        </div>
        <label className="ar-search">
          <Search size={14} aria-hidden="true" />
          <input value={queryInput} onChange={(e) => setQueryInput(e.target.value)} placeholder="ค้นหาชื่อ / MAC / asset tag" aria-label="ค้นหาอุปกรณ์" />
        </label>
        <select className="ar-select" value={kindFilter} onChange={(e) => setKindFilter(e.target.value)} aria-label="กรองตามชนิด">
          <option value="">ทุกชนิด</option>
          <option value="gateway">Gateway</option>
          <option value="device">อุปกรณ์</option>
        </select>
        <select className="ar-select" value={project} onChange={(e) => setProject(e.target.value)} aria-label="กรองตามโปรเจค">
          <option value="">ทุกโปรเจค</option><option value="none">ยังไม่จัดโปรเจค</option>
          {projects.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
        <span className={`ar-live ${live ? "is-on" : ""}`} title={live ? "รับสัญญาณเปลี่ยนแปลงแบบเรียลไทม์" : "ยังไม่ได้เชื่อมต่อเรียลไทม์ · ใช้การดึงข้อมูลทุก 15 วินาที"}>
          <i />
          {live ? "live" : "poll"}
        </span>
        <div className="ar-actions">
          <button type="button" className="ar-icon-btn" onClick={() => void refreshAll()} title="รีเฟรช" aria-label="รีเฟรช" disabled={busy}>
            <RefreshCw size={15} />
          </button>
          <button type="button" className="ar-icon-btn" onClick={() => void exportCSV()} title="ดาวน์โหลด CSV" aria-label="ดาวน์โหลด CSV" disabled={busy}>
            <Download size={15} />
          </button>
        </div>
      </header>

      {error && (
        <div className="ar-alert is-error" role="alert">
          {error}
          <button type="button" aria-label="ปิด" onClick={() => setError("")}>
            ×
          </button>
        </div>
      )}
      {notice && (
        <div className="ar-alert is-notice" role="status">
          {notice}
        </div>
      )}

      <div className="ar-table-wrap">
        <table className="ar-table">
          <thead>
            <tr>
              {columns.map(([key, label], i) => (
                <th key={label + i} scope="col">
                  {key ? (
                    <button type="button" className={`ar-th-btn ${sortKey === key ? "is-on" : ""}`} onClick={() => sortBy(key)} aria-label={`เรียงตาม ${label}`}>
                      {label}
                      {sortKey === key ? sortDir === 1 ? <ArrowUp size={12} /> : <ArrowDown size={12} /> : null}
                    </button>
                  ) : (
                    label
                  )}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {visible.map((r) => {
              const photo = photoOf(r);
              const due = daysUntil(r.next_due, todayISO);
              const dueClass = r.overdue_count > 0 || (due !== null && due < 0) ? "is-over" : due !== null && due <= DUE_SOON_DAYS ? "is-soon" : "";
              const stale = isOffline(r);
              const batClass = r.battery === null ? "" : r.battery < LOW_BATTERY ? "is-low" : r.battery < 40 ? "is-mid" : "";
              return (
                <tr key={keyOf(r)} tabIndex={0} onClick={() => openAsset(r)} onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openAsset(r); } }} aria-label={`เปิดรายละเอียด ${r.name}`}>
                  <td className="ar-cell-photo">{photo ? <span className="ar-photo" style={{ backgroundImage: `url(${photo})` }} aria-hidden="true" /> : <span className="ar-ico" aria-hidden="true">{r.asset_kind === "gateway" ? <Router size={15} /> : <ShieldCheck size={15} />}</span>}</td>
                  <td>
                    <span className="ar-name">
                      <strong>{r.name}</strong>
                      {r.location_note ? <small>{r.location_note}</small> : null}
                    </span>
                  </td>
                  <td className="ar-dim">{r.asset_kind === "gateway" ? "Gateway" : "อุปกรณ์"} · {modelLabel(r)}</td>
                  <td className="ar-mono">{r.external_id ? formatMAC(r.external_id) : r.asset_id.slice(0, 8)}</td>
                  <td className="ar-dim">{r.project_name || "—"}</td>
                  <td className="ar-dim">{r.gateway_name || "—"}</td>
                  <td>
                    <span className={`ar-pill st-${r.status}`}>{STATUS_LABEL[r.status] ?? r.status}</span>
                  </td>
                  <td className={stale ? "ar-stale" : "ar-dim"}>{when(r.last_seen)}</td>
                  <td className={`ar-bat ${batClass}`}>{r.battery === null ? "—" : `${Math.round(r.battery)}%`}</td>
                  <td className="ar-cell-tag">{r.asset_tag || r.serial_no ? `${r.asset_tag || "—"} / ${r.serial_no || "—"}` : "—"}</td>
                  <td className={`ar-due ${dueClass}`}>{r.next_due ? `${r.next_due}${r.overdue_count > 0 ? ` · เกิน ${r.overdue_count}` : ""}` : "—"}</td>
                  <td className="ar-cell-note" title={r.notes}>{r.notes || "—"}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
        {visible.length === 0 && (
          <div className="ar-empty">
            <CalendarClock size={26} />
            <h2>ไม่มีอุปกรณ์ตรงกับตัวกรอง</h2>
            <p>{rows.length === 0 ? "workspace นี้ยังไม่มี gateway หรืออุปกรณ์ที่ลงทะเบียน · เพิ่มได้จากหน้า Topology" : "ลองล้างคำค้นหรือเลือกชิป “ทั้งหมด”"}</p>
          </div>
        )}
      </div>

      <Sheet open={open !== null} onOpenChange={(o) => { if (!o) closeDrawer(); }}>
        <SheetContent className="ar-sheet">
          <SheetHeader>
            <SheetTitle>{detail?.asset.name ?? "กำลังโหลด…"}</SheetTitle>
            <SheetDescription>{detail ? `${detail.asset.asset_kind === "gateway" ? "Gateway" : "อุปกรณ์"} · ${modelLabel(detail.asset)}${detail.asset.external_id ? ` · ${formatMAC(detail.asset.external_id)}` : ""}` : "กำลังโหลดข้อมูลสินทรัพย์"}</SheetDescription>
          </SheetHeader>
          <nav className="ar-sheet-tabs" aria-label="มุมมองของสินทรัพย์">
            {([["info", "ข้อมูล"], ["plans", "แผน MA/PM"], ["history", "ประวัติ"]] as [Tab, string][]).map(([id, label]) => (
              <button key={id} type="button" className={tab === id ? "is-on" : ""} aria-pressed={tab === id} onClick={() => setTab(id)}>
                {label}
              </button>
            ))}
          </nav>
          <div className="ar-sheet-body">
            {drawerError && <div className="ar-drawer-error" role="alert">{drawerError}</div>}
            {drawerNotice && <div className="ar-drawer-notice" role="status">{drawerNotice}</div>}
            {detail && (
              <div className="ar-sheet-head">
                <span>{detail.asset.project_name || "ยังไม่จัดโปรเจค"}</span>
                <span>·</span>
                <span>Last seen {when(detail.asset.last_seen)}</span>
                {detail.asset.external_id && onOpenDevice && (
                  <button type="button" className="ar-btn" onClick={() => onOpenDevice(detail.asset.external_id)}>
                    <ExternalLink size={14} /> เปิดใน Topology
                  </button>
                )}
              </div>
            )}

            {tab === "info" && form && detail && (
              <div className="ar-form">
                <label>โปรเจค / Gateway
                  {detail.asset.asset_kind === "gateway" ? <select aria-label="โปรเจคของ gateway" value={gatewayOptions.find((g) => g.id === detail.asset.asset_id)?.project_id ?? ""} onChange={async (e) => { try { await client.post(`/gateways/${detail.asset.asset_id}/project`,{project_id:e.target.value || null}); await load(); await loadDetail("gateway",detail.asset.asset_id); } catch(err) { setDrawerError(fail(err)); } }}><option value="">ยังไม่จัดโปรเจค</option>{projectOptions.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}</select> : <select aria-label="Gateway และโปรเจคของอุปกรณ์" value={detail.asset.gateway_id} onChange={async (e) => { try { await client.post(`/devices/${detail.asset.asset_id}/update`,{name:detail.asset.name,gateway_id:e.target.value}); await load(); await loadDetail("device",detail.asset.asset_id); } catch(err) { setDrawerError(fail(err)); } }}>{gatewayOptions.map((g) => <option key={g.id} value={g.id}>{projectOptions.find((p) => p.id === g.project_id)?.name ?? "ยังไม่จัดโปรเจค"} · {g.name}</option>)}</select>}
                  <small>อุปกรณ์สังกัดโปรเจคตาม gateway · การย้าย gateway จะย้ายอุปกรณ์ใต้ gateway ไปด้วย</small>
                </label>

                <div className="ar-grid">
                  <label>
                    Asset tag
                    <input value={form.asset_tag} maxLength={128} onChange={(e) => { setForm({ ...form, asset_tag: e.target.value }); setDirty(true); }} />
                  </label>
                  <label>
                    Serial number
                    <input value={form.serial_no} maxLength={128} onChange={(e) => { setForm({ ...form, serial_no: e.target.value }); setDirty(true); }} />
                  </label>
                </div>
                <div className="ar-grid">
                  <label>
                    ผู้ขาย / ผู้ติดตั้ง
                    <input value={form.vendor} maxLength={128} onChange={(e) => { setForm({ ...form, vendor: e.target.value }); setDirty(true); }} />
                  </label>
                  <label>
                    จุดติดตั้ง
                    <input value={form.location_note} maxLength={128} onChange={(e) => { setForm({ ...form, location_note: e.target.value }); setDirty(true); }} />
                  </label>
                </div>
                <div className="ar-grid three">
                  <label>
                    วันที่ซื้อ
                    <input type="date" value={form.purchased_at} onChange={(e) => { setForm({ ...form, purchased_at: e.target.value }); setDirty(true); }} />
                  </label>
                  <label>
                    ประกันถึง
                    <input type="date" value={form.warranty_until} onChange={(e) => { setForm({ ...form, warranty_until: e.target.value }); setDirty(true); }} />
                  </label>
                  <label>
                    เปลี่ยนแบตล่าสุด
                    <input type="date" value={form.battery_changed_at} onChange={(e) => { setForm({ ...form, battery_changed_at: e.target.value }); setDirty(true); }} />
                  </label>
                </div>
                <label>
                  สถานะใช้งาน
                  <select value={form.status} onChange={(e) => { setForm({ ...form, status: e.target.value }); setDirty(true); }}>
                    {Object.entries(STATUS_LABEL).map(([v, l]) => (
                      <option key={v} value={v}>
                        {l}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  หมายเหตุ
                  <textarea value={form.notes} maxLength={4000} onChange={(e) => { setForm({ ...form, notes: e.target.value }); setDirty(true); }} />
                </label>
                <div className="ar-form-actions">
                  {dirty && detail && (
                    <button type="button" className="ar-btn" onClick={() => { setForm(toForm(detail.record)); setDirty(false); }}>
                      ยกเลิกการแก้ไข
                    </button>
                  )}
                  <button type="button" className="ar-btn primary" onClick={() => void saveRecord()} disabled={busy || !dirty}>
                    บันทึก
                  </button>
                </div>
              </div>
            )}

            {tab === "plans" && detail && (
              <>
                <ul className="ar-plans">
                  {detail.plans.map((p) => {
                    const due = daysUntil(p.next_due, todayISO);
                    return (
                      <li key={p.id} className={`ar-plan ${p.enabled ? "" : "is-off"} ${due !== null && due < 0 ? "is-over" : ""}`}>
                        <div className="ar-plan-head">
                          <Wrench size={14} aria-hidden="true" />
                          <strong>{p.title}</strong>
                          <small>{PLAN_KIND_LABEL[p.kind] ?? p.kind} · ทุก {p.interval_days} วัน</small>
                          <div className="ar-plan-actions">
                            <button type="button" className="ar-btn" onClick={() => { setDrawerError(""); setLogForm({ ...emptyLog(), plan_id: p.id, kind: PLAN_TO_LOG[p.kind] ?? "pm", title: p.title }); setTab("history"); }}>
                              บันทึกว่าทำแล้ว
                            </button>
                            <button type="button" className="ar-btn" onClick={() => { setDrawerError(""); setPlanForm({ id: p.id, title: p.title, kind: p.kind, interval_days: String(p.interval_days), next_due: p.next_due, enabled: p.enabled }); }}>
                              แก้ไข
                            </button>
                            <button type="button" className="ar-btn danger" onClick={() => void deletePlan(p)} disabled={busy} aria-label={`ลบแผน ${p.title}`}>
                              <Trash2 size={14} />
                            </button>
                          </div>
                        </div>
                        <small>
                          ครบกำหนด {p.next_due}
                          {due !== null && (due < 0 ? ` · เกินมาแล้ว ${-due} วัน` : ` · อีก ${due} วัน`)}
                          {p.last_done ? ` · ทำล่าสุด ${p.last_done}` : " · ยังไม่เคยทำ"}
                          {p.enabled ? "" : " · ปิดใช้งาน"}
                        </small>
                      </li>
                    );
                  })}
                </ul>
                {detail.plans.length === 0 && !planForm && <div className="ar-sheet-empty">ยังไม่มีแผน MA/PM สำหรับอุปกรณ์นี้</div>}
                {planForm ? (
                  <div className="ar-form">
                    <h3 className="ar-h3">{planForm.id ? "แก้ไขแผน" : "แผนใหม่"}</h3>
                    <label>
                      ชื่อแผน
                      <input value={planForm.title} maxLength={128} onChange={(e) => setPlanForm({ ...planForm, title: e.target.value })} />
                    </label>
                    <div className="ar-grid three">
                      <label>
                        ประเภท
                        <select value={planForm.kind} onChange={(e) => setPlanForm({ ...planForm, kind: e.target.value })}>
                          {Object.entries(PLAN_KIND_LABEL).map(([v, l]) => (
                            <option key={v} value={v}>
                              {l}
                            </option>
                          ))}
                        </select>
                      </label>
                      <label>
                        รอบ (วัน)
                        <input type="number" min={1} max={3650} value={planForm.interval_days} onChange={(e) => setPlanForm({ ...planForm, interval_days: e.target.value })} />
                      </label>
                      <label>
                        ครบกำหนดครั้งถัดไป
                        <input type="date" value={planForm.next_due} onChange={(e) => setPlanForm({ ...planForm, next_due: e.target.value })} />
                      </label>
                    </div>
                    <div className="ar-form-actions">
                      <button type="button" className="ar-btn" onClick={() => setPlanForm(null)}>
                        ยกเลิก
                      </button>
                      <button type="button" className="ar-btn primary" onClick={() => void savePlan(planForm)} disabled={busy}>
                        บันทึกแผน
                      </button>
                    </div>
                  </div>
                ) : (
                  <button type="button" className="ar-btn" onClick={() => { setDrawerError(""); setPlanForm(emptyPlan()); }}>
                    <Plus size={14} /> เพิ่มแผน MA/PM
                  </button>
                )}
              </>
            )}

            {tab === "history" && detail && (
              <>
                {logForm ? (
                  <div className="ar-form">
                    <h3 className="ar-h3">{logForm.plan_id ? "บันทึกว่าทำตามแผนแล้ว" : "บันทึกใหม่"}</h3>
                    {logForm.plan_id && <p className="ar-sheet-head">แผนนี้จะเลื่อนวันครบกำหนดถัดไปให้อัตโนมัติ</p>}
                    <div className="ar-grid">
                      <label>
                        ประเภท
                        <select value={logForm.kind} onChange={(e) => setLogForm({ ...logForm, kind: e.target.value })}>
                          {Object.entries(LOG_KIND_LABEL).map(([v, l]) => (
                            <option key={v} value={v}>
                              {l}
                            </option>
                          ))}
                        </select>
                      </label>
                      <label>
                        วันที่ดำเนินการ
                        <input type="date" value={logForm.performed_at} onChange={(e) => setLogForm({ ...logForm, performed_at: e.target.value })} />
                      </label>
                    </div>
                    <label>
                      หัวข้อ
                      <input value={logForm.title} maxLength={128} onChange={(e) => setLogForm({ ...logForm, title: e.target.value })} />
                    </label>
                    <label>
                      รายละเอียด
                      <textarea value={logForm.detail} maxLength={4000} onChange={(e) => setLogForm({ ...logForm, detail: e.target.value })} />
                    </label>
                    <div className="ar-grid">
                      <label>
                        ผู้ดำเนินการ
                        <input value={logForm.performed_by} maxLength={128} onChange={(e) => setLogForm({ ...logForm, performed_by: e.target.value })} />
                      </label>
                      <label>
                        ค่าใช้จ่าย (บาท)
                        <input type="number" min={0} step="0.01" value={logForm.cost} onChange={(e) => setLogForm({ ...logForm, cost: e.target.value })} />
                      </label>
                    </div>
                    <div className="ar-form-actions">
                      <button type="button" className="ar-btn" onClick={() => setLogForm(null)}>
                        ยกเลิก
                      </button>
                      <button type="button" className="ar-btn primary" onClick={() => void addLog(logForm)} disabled={busy}>
                        บันทึกประวัติ
                      </button>
                    </div>
                  </div>
                ) : (
                  <button type="button" className="ar-btn" onClick={() => { setDrawerError(""); setLogForm(emptyLog()); }}>
                    <Plus size={14} /> เพิ่มบันทึก / MA
                  </button>
                )}
                <ul className="ar-logs">
                  {detail.logs.map((l) => (
                    <li key={l.id} className="ar-log">
                      <span className="ar-log-icon" aria-hidden="true">
                        {l.kind === "battery" ? <BatteryLow size={13} /> : l.kind === "note" ? <FileText size={13} /> : <Wrench size={13} />}
                      </span>
                      <span className="ar-log-body">
                        <strong>{l.title}</strong>
                        <small>
                          {LOG_KIND_LABEL[l.kind] ?? l.kind} · {l.performed_at.slice(0, 10)}
                          {l.performed_by ? ` · ${l.performed_by}` : ""}
                          {l.cost !== null ? ` · ${money(l.cost)} บาท` : ""}
                          {l.plan_id ? " · ตามแผน" : ""}
                        </small>
                        {l.detail ? <small>{l.detail}</small> : null}
                      </span>
                    </li>
                  ))}
                </ul>
                {detail.logs.length === 0 && <div className="ar-sheet-empty">ยังไม่มีประวัติการบำรุงรักษา</div>}
                {detail.logs.length >= 100 && (
                  <p className="ar-sheet-head">
                    <ChevronRight size={13} /> แสดง 100 รายการล่าสุด
                  </p>
                )}
              </>
            )}
          </div>
        </SheetContent>
      </Sheet>
    </section>
  );
}
