"use client";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { CalendarClock, Droplets, FileDown, FileSpreadsheet, Mail, MessageSquare, Plus, Printer, RefreshCw, Save, Search, Send, Thermometer, Trash2, Webhook } from "lucide-react";
import "./reports.css";
import { ApiError, createClientFrom, type Project } from "../topology/api";
import { useLatest } from "../topology/use-latest";
import type { Me } from "../team/team";

// "รายงาน" (docs/platform/reports.md): CSV export of fixed sensors, and summary reports with excursions that print
// to A4 through the browser (no server PDF). Wearables and roaming tags never appear here: they are personal data.

type SensorInfo = { gateway_id: string; external_id: string; name: string; gateway_name: string; project_id: string | null; profile_id: string };
type SensorRef = { gateway_id: string; external_id: string };
type Definition = { id: string; project_id: string | null; name: string; kind: string; sensors: SensorRef[]; temp_min: number | null; temp_max: number | null; hum_min: number | null; hum_max: number | null; updated_at: string;
  schedule: "none" | "daily" | "weekly"; schedule_weekday: number | null; schedule_time: string | null; channels: string[]; next_run_at: string | null };
type Channel = { id: string; name: string; kind: "webhook" | "line" | "email"; enabled: boolean; project_id: string | null };
type Delivery = { id: string; channel_id: string | null; channel_name: string; channel_kind: string; status: "queued" | "sent" | "failed" | "skipped"; attempts: number; last_error: string | null; sent_at: string | null };
type Run = { id: string; period_from: string; period_to: string; trigger: "schedule" | "manual"; status: "queued" | "done"; created_at: string; deliveries: Delivery[] };
type Stats = { min: number; max: number; avg: number } | null;
type Day = { date: string; buckets: number; expected: number; coverage_pct: number; temperature: Stats; humidity: Stats };
type Excursion = { metric: "temperature" | "humidity"; direction: "high" | "low"; start: string; end: string; duration_min: number; peak: number };
type Point = { at: string; t_avg: number | null; t_min: number | null; t_max: number | null; h_avg: number | null; h_min: number | null; h_max: number | null };
type SensorSummary = SensorInfo & { buckets: number; expected_buckets: number; coverage_pct: number; temperature: Stats; humidity: Stats; days: Day[]; excursions: Excursion[]; excursions_truncated: boolean; minutes_out_of_range: { temperature: number; humidity: number }; series: Point[] };
type Range = { min: number | null; max: number | null };
type Summary = { from: string; to: string; generated_at: string; workspace: string; temp: Range; hum: Range; report: { id: string; name: string } | null; sensors: SensorSummary[]; omitted: (SensorRef & { reason: string })[] };
type Tab = "export" | "summary";
type Form = { name: string; project_id: string; sensors: string[]; temp_min: string; temp_max: string; hum_min: string; hum_max: string; schedule: Definition["schedule"]; weekday: string; time: string; channels: string[] };

const MAX_SENSORS = 50, MAX_DAYS = 31;
/** The server caps a raw export at sensors × days started (domain.ReportRawSensorDays). */
const RAW_SENSOR_DAYS = 120;
/** An export streams for up to 30 minutes on the server; a summary has 60 s. */
const EXPORT_TIMEOUT_MS = 30 * 60_000, SUMMARY_TIMEOUT_MS = 65_000;
const RESOLUTIONS: [string, string][] = [["raw", "ทุกค่าที่บันทึก (raw)"], ["5m", "เฉลี่ยทุก 5 นาที"], ["1h", "รายชั่วโมง"], ["1d", "รายวัน"]];
const REASONS: Record<string, string> = {
  invalid_range: "ช่วงวันที่ไม่ถูกต้อง วันเริ่มต้องไม่หลังวันสิ้นสุด",
  window_too_long: `ช่วงวันที่ยาวเกิน ${MAX_DAYS} วัน`,
  no_sensors: "เลือกเซนเซอร์อย่างน้อย 1 ตัว",
  too_many_sensors: `เลือกได้ไม่เกิน ${MAX_SENSORS} เซนเซอร์ต่อครั้ง`,
  duplicate_sensor: "มีเซนเซอร์ซ้ำในรายการ",
  invalid_sensor: "รหัสเซนเซอร์ไม่ถูกต้อง",
  personal_sensor: "อุปกรณ์สวมใส่หรือแท็กที่ติดตามข้าม gateway เป็นข้อมูลส่วนบุคคล ส่งออกได้จากหน้าความเป็นส่วนตัวเท่านั้น",
  sensor_not_registered: "เซนเซอร์นี้ยังไม่ได้ลงทะเบียน จึงออกรายงานไม่ได้",
  sensor_not_found: "ไม่พบเซนเซอร์ หรืออยู่นอกโปรเจคที่คุณเข้าถึงได้",
  invalid_limits: "ช่วงค่าที่ยอมรับไม่ถูกต้อง ค่าต่ำสุดต้องไม่มากกว่าค่าสูงสุด",
  invalid_name: "ตั้งชื่อรายงาน 1–120 ตัวอักษร",
  too_many_reports: "workspace นี้มีรายงานที่บันทึกไว้ครบจำนวนสูงสุดแล้ว",
  raw_too_large: `ข้อมูลดิบ (raw) ส่งออกได้ครั้งละไม่เกิน ${RAW_SENSOR_DAYS} เซนเซอร์-วัน (จำนวนเซนเซอร์ × จำนวนวัน) ลดจำนวนเซนเซอร์หรือวัน หรือเลือกความละเอียดแบบเฉลี่ย`,
  sensor_outside_project: "รายงานของโปรเจคใช้ได้เฉพาะเซนเซอร์บน gateway ของโปรเจคนั้น เลือกโปรเจคให้ตรงหรือบันทึกเป็นรายงานทั้ง workspace",
  rate_limited: "มีการออกรายงานอื่นกำลังทำงานอยู่ กรุณารอสักครู่แล้วลองใหม่",
  too_many_sends: "ส่งรายงานด้วยมือได้ไม่เกิน 10 ครั้งต่อชั่วโมงต่อ workspace กรุณารอแล้วลองใหม่",
  no_channels: "เลือกช่องทางส่งอย่างน้อย 1 ช่องทาง",
  invalid_channels: "ช่องทางส่งต้องเปิดใช้งาน และเป็นของทั้ง workspace หรือของโปรเจคเดียวกับรายงาน",
  invalid_schedule: "ตั้งเวลาส่งไม่ถูกต้อง เลือกเวลา (และวันในสัปดาห์สำหรับรายสัปดาห์)",
  forbidden: "บัญชีนี้ไม่มีสิทธิ์ทำรายการนี้",
};
const OMITTED: Record<string, string> = { sensor_not_found: "ไม่พบหรืออยู่นอกโปรเจคของคุณ", personal_sensor: "เปลี่ยนเป็นอุปกรณ์สวมใส่/roaming", sensor_not_registered: "ถูกถอนการลงทะเบียน" };

const keyOf = (s: SensorRef) => `${s.gateway_id}/${s.external_id}`;
const refOf = (key: string): SensorRef => { const i = key.indexOf("/"); return { gateway_id: key.slice(0, i), external_id: key.slice(i + 1) }; };
/** Calendar days are Bangkok days: the range sent is [from 00:00, the day after `to` 00:00) at +07:00. */
const bangkokToday = () => new Date(Date.now() + 7 * 3600_000).toISOString().slice(0, 10);
const addDays = (day: string, n: number) => new Date(Date.parse(`${day}T00:00:00Z`) + n * 86400_000).toISOString().slice(0, 10);
const spanDays = (from: string, to: string) => Math.round((Date.parse(`${to}T00:00:00Z`) - Date.parse(`${from}T00:00:00Z`)) / 86400_000) + 1;
const rangeOf = (from: string, to: string) => ({ from: `${from}T00:00:00+07:00`, to: `${addDays(to, 1)}T00:00:00+07:00` });
const rangeError = (from: string, to: string) => !from || !to ? "เลือกวันเริ่มและวันสิ้นสุด" : to < from ? REASONS.invalid_range : spanDays(from, to) > MAX_DAYS ? REASONS.window_too_long : "";
const num = (v: number | null | undefined, digits = 1) => (v == null || !Number.isFinite(v) ? "—" : v.toFixed(digits));
const parseLimit = (s: string): number | null => { const t = s.trim(); if (!t) return null; const v = Number(t); return Number.isFinite(v) ? v : NaN; };
const limitText = (v: number | null) => (v == null ? "" : String(v));
const thaiDay = (day: string) => new Date(`${day}T00:00:00+07:00`).toLocaleDateString("th-TH", { timeZone: "Asia/Bangkok", day: "numeric", month: "short", year: "numeric" });
const thaiTime = (iso: string) => new Date(iso).toLocaleString("th-TH", { timeZone: "Asia/Bangkok", hour12: false, day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
const duration = (min: number) => (min < 60 ? `${min} นาที` : `${Math.floor(min / 60)} ชม.${min % 60 ? ` ${min % 60} นาที` : ""}`);
const rangeLabel = (r: Range, unit: string) => (r.min == null && r.max == null ? "ไม่กำหนด" : r.min != null && r.max != null ? `${r.min}–${r.max} ${unit}` : r.min != null ? `ไม่ต่ำกว่า ${r.min} ${unit}` : `ไม่เกิน ${r.max} ${unit}`);
const emptyForm = (project: string): Form => ({ name: "", project_id: project, sensors: [], temp_min: "", temp_max: "", hum_min: "", hum_max: "", schedule: "none", weekday: "1", time: "07:00", channels: [] });
const formOf = (d: Definition): Form => ({ name: d.name, project_id: d.project_id ?? "", sensors: d.sensors.map(keyOf), temp_min: limitText(d.temp_min), temp_max: limitText(d.temp_max), hum_min: limitText(d.hum_min), hum_max: limitText(d.hum_max),
  schedule: d.schedule ?? "none", weekday: String(d.schedule_weekday ?? 1), time: d.schedule_time ?? "07:00", channels: d.channels ?? [] });
const WEEKDAYS = ["จันทร์", "อังคาร", "พุธ", "พฤหัสบดี", "ศุกร์", "เสาร์", "อาทิตย์"]; // ISO 1..7
const DELIVERY_LABEL: Record<Delivery["status"], string> = { queued: "รอส่ง", sent: "ส่งแล้ว", failed: "ส่งไม่สำเร็จ", skipped: "ข้าม" };
const KindIcon = ({ kind }: { kind: string }) => (kind === "webhook" ? <Webhook size={14} aria-hidden="true" /> : kind === "line" ? <MessageSquare size={14} aria-hidden="true" /> : <Mail size={14} aria-hidden="true" />);
const scheduleLabel = (d: Pick<Definition, "schedule" | "schedule_weekday" | "schedule_time">) =>
  d.schedule === "daily" ? `ทุกวัน ${d.schedule_time}` : d.schedule === "weekly" ? `ทุกวัน${WEEKDAYS[(d.schedule_weekday ?? 1) - 1]} ${d.schedule_time}` : "";
const bangkokDay = (iso: string) => new Date(Date.parse(iso) + 7 * 3600_000).toISOString().slice(0, 10);
const isDay = (s: string | null) => !!s && /^\d{4}-\d{2}-\d{2}$/.test(s);

function SensorPicker({ sensors, selected, onChange, disabled }: { sensors: SensorInfo[]; selected: string[]; onChange: (keys: string[]) => void; disabled?: boolean }) {
  const [query, setQuery] = useState("");
  const chosen = useMemo(() => new Set(selected), [selected]);
  const q = query.trim().toLowerCase();
  const shown = sensors.filter((s) => !q || `${s.name} ${s.gateway_name} ${s.external_id}`.toLowerCase().includes(q));
  const toggle = (key: string) => onChange(chosen.has(key) ? selected.filter((k) => k !== key) : selected.length >= MAX_SENSORS ? selected : [...selected, key]);
  return (
    <div className="rp-picker">
      <div className="rp-picker-bar">
        <label className="rp-search"><Search size={14} aria-hidden="true" /><input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="ค้นหาชื่อ gateway หรือ MAC" aria-label="ค้นหาเซนเซอร์" /></label>
        <span className={`rp-count ${selected.length >= MAX_SENSORS ? "is-full" : ""}`}>เลือก {selected.length}/{MAX_SENSORS}</span>
        <button type="button" className="rp-link" disabled={disabled} onClick={() => onChange([...selected, ...shown.map(keyOf).filter((k) => !chosen.has(k))].slice(0, MAX_SENSORS))}>เลือกที่แสดง</button>
        <button type="button" className="rp-link" disabled={disabled || selected.length === 0} onClick={() => onChange([])}>ล้าง</button>
      </div>
      {sensors.length === 0 ? <p className="rp-empty">ยังไม่มีเซนเซอร์ที่ออกรายงานได้ ลงทะเบียนเซนเซอร์อุณหภูมิ/ความชื้นในหน้าเชื่อมต่ออุปกรณ์ก่อน</p> : (
        <ul className="rp-picker-list">
          {shown.map((s) => {
            const key = keyOf(s);
            return <li key={key}><label className={chosen.has(key) ? "is-on" : ""}><input type="checkbox" checked={chosen.has(key)} disabled={disabled || (!chosen.has(key) && selected.length >= MAX_SENSORS)} onChange={() => toggle(key)} /><span className="rp-sensor-name">{s.name}</span><span className="rp-dim">{s.gateway_name}</span><span className="rp-mono">{s.external_id}</span></label></li>;
          })}
          {shown.length === 0 && <li className="rp-empty">ไม่พบเซนเซอร์ที่ตรงกับคำค้น</li>}
        </ul>
      )}
    </div>
  );
}

function DateRange({ from, to, onChange }: { from: string; to: string; onChange: (from: string, to: string) => void }) {
  return (
    <div className="rp-dates">
      <label>ตั้งแต่วันที่<input type="date" value={from} max={to || undefined} onChange={(e) => onChange(e.target.value, to)} /></label>
      <label>ถึงวันที่<input type="date" value={to} min={from || undefined} onChange={(e) => onChange(from, e.target.value)} /></label>
      <small className="rp-dim">เวลาไทย · สูงสุด {MAX_DAYS} วัน</small>
    </div>
  );
}

/** Hourly avg line over its min–max band, with the allowed range dashed. A gap of more than an hour breaks the line. */
function SeriesChart({ points, metric, from, to, range }: { points: Point[]; metric: "temperature" | "humidity"; from: string; to: string; range: Range }) {
  const k = metric === "temperature" ? "t" : "h";
  const rows = points.filter((p) => p[`${k}_avg`] != null).map((p) => ({ at: Date.parse(p.at), avg: p[`${k}_avg`] as number, min: (p[`${k}_min`] ?? p[`${k}_avg`]) as number, max: (p[`${k}_max`] ?? p[`${k}_avg`]) as number }));
  if (rows.length === 0) return <div className="rp-chart-empty">ไม่มีข้อมูล{metric === "temperature" ? "อุณหภูมิ" : "ความชื้น"}ในช่วงนี้</div>;
  const limits = [range.min, range.max].filter((v): v is number => v != null);
  const lo = Math.floor(Math.min(...rows.map((r) => r.min), ...limits) - 1), hi = Math.ceil(Math.max(...rows.map((r) => r.max), ...limits) + 1);
  const t0 = Date.parse(from), t1 = Date.parse(to);
  const x = (t: number) => 44 + ((t - t0) / Math.max(1, t1 - t0)) * 650;
  const y = (v: number) => 150 - ((v - lo) / Math.max(1, hi - lo)) * 130;
  const segments: (typeof rows)[] = [];
  rows.forEach((r, i) => { if (i === 0 || r.at - rows[i - 1].at > 3600_000) segments.push([]); segments[segments.length - 1].push(r); });
  const unit = metric === "temperature" ? "°C" : "%";
  return (
    <svg className="rp-chart" viewBox="0 0 710 176" role="img" aria-label={`กราฟ${metric === "temperature" ? "อุณหภูมิ" : "ความชื้น"}รายชั่วโมง`}>
      {[lo, (lo + hi) / 2, hi].map((v) => <g key={v}><line x1="44" x2="700" y1={y(v)} y2={y(v)} className="rp-chart-grid" /><text x="4" y={y(v) + 4}>{v.toFixed(1)}</text></g>)}
      {segments.map((seg, i) => <polygon key={`b${i}`} className={`rp-chart-band is-${metric}`} points={[...seg.map((r) => `${x(r.at)},${y(r.max)}`), ...seg.slice().reverse().map((r) => `${x(r.at)},${y(r.min)}`)].join(" ")} />)}
      {segments.map((seg, i) => seg.length === 1 ? <circle key={`l${i}`} cx={x(seg[0].at)} cy={y(seg[0].avg)} r="2" className={`rp-chart-dot is-${metric}`} /> : <polyline key={`l${i}`} points={seg.map((r) => `${x(r.at)},${y(r.avg)}`).join(" ")} fill="none" className={`rp-chart-line is-${metric}`} />)}
      {limits.map((v) => <g key={`lim${v}`}><line x1="44" x2="700" y1={y(v)} y2={y(v)} className="rp-chart-limit" /><text x="700" y={y(v) - 4} textAnchor="end" className="rp-chart-limit-label">{v} {unit}</text></g>)}
      <text x="44" y="172">{thaiTime(from)}</text>
      <text x="700" y="172" textAnchor="end">{thaiTime(to)}</text>
    </svg>
  );
}

function SensorSection({ s, summary }: { s: SensorSummary; summary: Summary }) {
  const out = s.minutes_out_of_range;
  return (
    <section className="rp-sensor">
      <header><h3>{s.name}</h3><span className="rp-dim">{s.gateway_name} · <span className="rp-mono">{s.external_id}</span></span></header>
      <div className="rp-kpis">
        <div><span><Thermometer size={14} aria-hidden="true" />อุณหภูมิ (°C)</span><strong>{num(s.temperature?.avg)}</strong><small>ต่ำสุด {num(s.temperature?.min)} · สูงสุด {num(s.temperature?.max)}</small></div>
        <div><span><Droplets size={14} aria-hidden="true" />ความชื้น (%)</span><strong>{num(s.humidity?.avg)}</strong><small>ต่ำสุด {num(s.humidity?.min)} · สูงสุด {num(s.humidity?.max)}</small></div>
        <div><span>ความครอบคลุมข้อมูล</span><strong className={s.coverage_pct < 90 ? "is-warn" : ""}>{num(s.coverage_pct)}%</strong><small>{s.buckets.toLocaleString("th-TH")} จาก {s.expected_buckets.toLocaleString("th-TH")} ช่วง 5 นาที</small></div>
        <div><span>นอกช่วงที่ยอมรับ</span><strong className={out.temperature + out.humidity > 0 ? "is-hot" : ""}>{duration(out.temperature + out.humidity)}</strong><small>อุณหภูมิ {duration(out.temperature)} · ความชื้น {duration(out.humidity)}</small></div>
      </div>
      <div className="rp-charts">
        <SeriesChart points={s.series} metric="temperature" from={summary.from} to={summary.to} range={summary.temp} />
        <SeriesChart points={s.series} metric="humidity" from={summary.from} to={summary.to} range={summary.hum} />
      </div>
      <h4>สรุปรายวัน</h4>
      <div className="rp-table-wrap"><table className="rp-table">
        <thead><tr><th>วันที่</th><th>ครอบคลุม</th><th>อุณหภูมิ ต่ำสุด</th><th>สูงสุด</th><th>เฉลี่ย</th><th>ความชื้น ต่ำสุด</th><th>สูงสุด</th><th>เฉลี่ย</th></tr></thead>
        <tbody>{s.days.map((d) => <tr key={d.date}><td>{thaiDay(d.date)}</td><td className={d.coverage_pct < 90 ? "is-warn" : ""}>{num(d.coverage_pct)}%</td><td>{num(d.temperature?.min)}</td><td>{num(d.temperature?.max)}</td><td>{num(d.temperature?.avg)}</td><td>{num(d.humidity?.min)}</td><td>{num(d.humidity?.max)}</td><td>{num(d.humidity?.avg)}</td></tr>)}</tbody>
      </table></div>
      <h4>ช่วงที่ค่าออกนอกเกณฑ์ {s.excursions.length > 0 && <span className="rp-dim">({s.excursions.length}{s.excursions_truncated ? "+" : ""} ครั้ง)</span>}</h4>
      {s.excursions.length === 0 ? <p className="rp-ok">{summary.temp.min == null && summary.temp.max == null && summary.hum.min == null && summary.hum.max == null ? "ไม่ได้กำหนดช่วงค่าที่ยอมรับ" : "ไม่มีช่วงที่ค่าออกนอกเกณฑ์"}</p> : (
        <div className="rp-table-wrap"><table className="rp-table">
          <thead><tr><th>ค่า</th><th>ทิศทาง</th><th>เริ่ม</th><th>สิ้นสุด</th><th>นาน</th><th>ค่าสุดขีด</th></tr></thead>
          <tbody>{s.excursions.map((e) => <tr key={`${e.metric}-${e.direction}-${e.start}`}><td>{e.metric === "temperature" ? "อุณหภูมิ" : "ความชื้น"}</td><td className={e.direction === "high" ? "is-hot" : "is-cold"}>{e.direction === "high" ? "สูงเกิน" : "ต่ำกว่า"}</td><td>{thaiTime(e.start)}</td><td>{thaiTime(e.end)}</td><td>{duration(e.duration_min)}</td><td>{num(e.peak)} {e.metric === "temperature" ? "°C" : "%"}</td></tr>)}</tbody>
        </table></div>
      )}
      {s.excursions_truncated && <p className="rp-dim">แสดง 500 ครั้งแรก · เวลานอกช่วงนับครบทุกช่วง</p>}
    </section>
  );
}

export default function ReportsPage({ me, getToken, refresh, onUnauthorized }: { me: Me | null; getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const [tab, setTab] = useState<Tab>("export");
  const [sensors, setSensors] = useState<SensorInfo[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [definitions, setDefinitions] = useState<Definition[]>([]);
  const [error, setError] = useState(""), [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [today] = useState(bangkokToday);
  // Export tab
  const [exportSensors, setExportSensors] = useState<string[]>([]);
  const [exportFrom, setExportFrom] = useState(() => addDays(today, -6)), [exportTo, setExportTo] = useState(today);
  const [resolution, setResolution] = useState("1h");
  // Summary tab: a saved report ("" = one put together here), the form, and the last result.
  const [selected, setSelected] = useState("");
  const [form, setForm] = useState<Form>(() => emptyForm(""));
  const [dirty, setDirty] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [summaryFrom, setSummaryFrom] = useState(() => addDays(today, -6)), [summaryTo, setSummaryTo] = useState(today);
  const [summary, setSummary] = useState<Summary | null>(null);
  const [channels, setChannels] = useState<Channel[]>([]), [emailAvailable, setEmailAvailable] = useState(false);
  const [runs, setRuns] = useState<Run[]>([]);
  const unauthorized = useRef(false);

  const canWrite = !!me && ["owner", "admin", "operator"].includes(me.role) && me.permissions?.reports !== "read";
  // A workspace report needs every project in view (core.report_definitions' policies).
  const wholeWorkspace = !!me && (me.role === "owner" || me.project_ids === null);
  // The shell passes a new onUnauthorized on every render (it re-renders on its polls): read it through a ref, so fail,
  // load and the first-load effect below stay the same and the page is not reloaded on every shell render.
  const unauthorizedRef = useLatest(onUnauthorized);
  const fail = useCallback((e: unknown, fallback: string) => {
    if (e instanceof ApiError && e.status === 401 && !unauthorized.current) { unauthorized.current = true; unauthorizedRef.current?.(); }
    if (e instanceof ApiError) return (e.reason && REASONS[e.reason]) || (e.status === 403 ? REASONS.forbidden : e.status === 429 ? REASONS.rate_limited : e.message);
    return e instanceof Error ? e.message : fallback;
  }, [unauthorizedRef]);

  const loadRuns = useCallback(async (id: string) => {
    if (!id) { setRuns([]); return; }
    try { setRuns((await client.raw<{ items: Run[] }>(`/reports/definitions/${id}/runs`)).items); } catch { setRuns([]); }
  }, [client]);
  const load = useCallback(async () => {
    try {
      const [s, d, p, ch] = await Promise.all([client.raw<{ items: SensorInfo[] }>("/reports/sensors"), client.raw<{ items: Definition[] }>("/reports/definitions"), client.projects().catch(() => [] as Project[]),
        client.raw<{ items: Channel[]; email_available: boolean }>("/reports/channels").catch(() => ({ items: [] as Channel[], email_available: false }))]);
      setSensors(s.items); setDefinitions(d.items); setProjects(p); setChannels(ch.items); setEmailAvailable(ch.email_available); setError("");
      return d.items;
    } catch (e) { setError(fail(e, "โหลดข้อมูลรายงานไม่สำเร็จ")); return null; }
  }, [client, fail]);

  const byKey = useMemo(() => new Map(sensors.map((s) => [keyOf(s), s])), [sensors]);
  const edit = (patch: Partial<Form>) => { setForm((f) => ({ ...f, ...patch })); setDirty(true); };
  const choose = (id: string, list = definitions) => {
    setSelected(id); setConfirmDelete(false); setDirty(false); setSummary(null);
    const d = list.find((x) => x.id === id);
    setForm(d ? formOf(d) : emptyForm(""));
    void loadRuns(d ? id : "");
  };
  const limits = () => {
    const v = { temp_min: parseLimit(form.temp_min), temp_max: parseLimit(form.temp_max), hum_min: parseLimit(form.hum_min), hum_max: parseLimit(form.hum_max) };
    if (Object.values(v).some((x) => Number.isNaN(x))) throw new Error("ช่วงค่าที่ยอมรับต้องเป็นตัวเลข");
    return v as Record<keyof typeof v, number | null>;
  };

  async function download() {
    const bad = rangeError(exportFrom, exportTo) || (exportSensors.length === 0 ? REASONS.no_sensors : "") || (resolution === "raw" && exportSensors.length * spanDays(exportFrom, exportTo) > RAW_SENSOR_DAYS ? REASONS.raw_too_large : "");
    if (bad) { setError(bad); return; }
    setBusy(true); setError(""); setNotice("");
    try {
      await client.download("/reports/export", `aether-${exportFrom}-${exportTo}-${resolution}.csv`, { sensors: exportSensors.map(refOf), resolution, ...rangeOf(exportFrom, exportTo) }, EXPORT_TIMEOUT_MS);
      setNotice("ดาวน์โหลดไฟล์ CSV แล้ว");
    } catch (e) { setError(fail(e, "ส่งออกข้อมูลไม่สำเร็จ")); } finally { setBusy(false); }
  }
  async function run() {
    const bad = rangeError(summaryFrom, summaryTo) || (!selected && form.sensors.length === 0 ? REASONS.no_sensors : "");
    if (bad) { setError(bad); return; }
    setBusy(true); setError(""); setNotice("");
    try {
      // A saved report runs as saved; once its form is changed, the form's sensors and ranges run instead.
      let payload: Record<string, unknown>;
      if (selected && !dirty) payload = { report_id: selected, ...rangeOf(summaryFrom, summaryTo) };
      else {
        const l = limits();
        payload = { sensors: form.sensors.map(refOf), temp: { min: l.temp_min, max: l.temp_max }, hum: { min: l.hum_min, max: l.hum_max }, ...rangeOf(summaryFrom, summaryTo) };
      }
      const out = await client.post<Summary>("/reports/summary", payload, SUMMARY_TIMEOUT_MS);
      setSummary({ ...out, report: out.report ?? (selected && form.name ? { id: selected, name: `${form.name} (แก้ไขยังไม่บันทึก)` } : null) });
    } catch (e) { setError(fail(e, "สร้างรายงานไม่สำเร็จ")); } finally { setBusy(false); }
  }
  async function runSaved(id: string, from: string, to: string) {
    setBusy(true); setError(""); setNotice("");
    try { setSummary(await client.post<Summary>("/reports/summary", { report_id: id, ...rangeOf(from, to) }, SUMMARY_TIMEOUT_MS)); }
    catch (e) { setError(fail(e, "สร้างรายงานไม่สำเร็จ")); } finally { setBusy(false); }
  }
  async function sendNow() {
    if (!selected) return;
    const bad = rangeError(summaryFrom, summaryTo);
    if (bad) { setError(bad); return; }
    setBusy(true); setError(""); setNotice("");
    try {
      await client.post<Run>(`/reports/definitions/${selected}/send`, rangeOf(summaryFrom, summaryTo));
      setNotice(`ส่งรายงานช่วง ${thaiDay(summaryFrom)} – ${thaiDay(summaryTo)} เข้าคิวแล้ว จะส่งภายในประมาณครึ่งนาที`);
      await loadRuns(selected);
    } catch (e) { setError(fail(e, "ส่งรายงานไม่สำเร็จ")); } finally { setBusy(false); }
  }
  async function save(asNew: boolean) {
    setBusy(true); setError(""); setNotice("");
    try {
      const schedule = form.schedule === "none" ? { schedule: "none" } : { schedule: form.schedule, schedule_time: form.time, ...(form.schedule === "weekly" ? { schedule_weekday: Number(form.weekday) } : {}) };
      const body = { name: form.name.trim(), project_id: form.project_id || null, sensors: form.sensors.map(refOf), ...limits(), ...schedule, channels: form.channels };
      const out = asNew || !selected ? await client.post<Definition>("/reports/definitions", body) : await client.post<Definition>(`/reports/definitions/${selected}/update`, body);
      await load();
      setSelected(out.id); setForm(formOf(out)); setDirty(false); void loadRuns(out.id);
      setNotice(asNew || !selected ? "บันทึกรายงานแล้ว" : "บันทึกการแก้ไขแล้ว");
    } catch (e) { setError(fail(e, "บันทึกรายงานไม่สำเร็จ")); } finally { setBusy(false); }
  }
  async function remove() {
    if (!selected) return;
    setBusy(true); setError(""); setNotice("");
    try {
      await client.post<void>(`/reports/definitions/${selected}/delete`, {});
      await load();
      choose(""); setNotice("ลบรายงานแล้ว");
    } catch (e) { setError(fail(e, "ลบรายงานไม่สำเร็จ")); } finally { setBusy(false); }
  }

  // The first load, and the deep link of a scheduled message: open the summary tab on that report and those days, once.
  // The link's parameters are taken off the address right away, so neither a re-render nor a reload runs it again.
  const actions = useLatest({ choose, runSaved });
  const linkTaken = useRef(false);
  useEffect(() => {
    const id = requestAnimationFrame(() => {
      let report: string | null = null, from: string | null = null, to: string | null = null;
      if (!linkTaken.current) {
        linkTaken.current = true;
        const url = new URL(location.href);
        report = url.searchParams.get("report"); from = url.searchParams.get("from"); to = url.searchParams.get("to");
        if (report || from || to) {
          ["report", "from", "to"].forEach((k) => url.searchParams.delete(k));
          history.replaceState(history.state, "", url.pathname + url.search.replace("reports=", "reports") + url.hash);
        }
      }
      void load().then((list) => {
        if (!list || !report || !list.some((d) => d.id === report)) return;
        setTab("summary");
        actions.current.choose(report, list);
        if (isDay(from) && isDay(to) && !rangeError(from!, to!)) {
          setSummaryFrom(from!); setSummaryTo(to!);
          void actions.current.runSaved(report, from!, to!);
        }
      });
    });
    return () => cancelAnimationFrame(id);
  }, [load, actions]);

  const projectName = (id: string | null) => (id ? projects.find((p) => p.id === id)?.name ?? "โปรเจคอื่น" : "ทั้ง workspace");
  const lastDay = summary ? addDays(new Date(Date.parse(summary.to) + 7 * 3600_000).toISOString().slice(0, 10), -1) : "";
  const firstDay = summary ? new Date(Date.parse(summary.from) + 7 * 3600_000).toISOString().slice(0, 10) : "";
  return (
    <div className="rp">
      <div className="rp-bar rp-noprint">
        <div><span className="rp-kicker">OPERATE</span><h1>รายงาน</h1></div>
        <div className="rp-tabs" role="tablist" aria-label="ประเภทรายงาน">
          <button type="button" role="tab" aria-selected={tab === "export"} className={tab === "export" ? "is-on" : ""} onClick={() => setTab("export")}><FileSpreadsheet size={15} aria-hidden="true" />ส่งออกข้อมูล</button>
          <button type="button" role="tab" aria-selected={tab === "summary"} className={tab === "summary" ? "is-on" : ""} onClick={() => setTab("summary")}><FileDown size={15} aria-hidden="true" />รายงานสรุป</button>
          <button type="button" className="rp-icon" onClick={() => void load()} title="โหลดใหม่" aria-label="โหลดรายการเซนเซอร์และรายงานใหม่"><RefreshCw size={15} /></button>
        </div>
      </div>
      {error && <div className="rp-alert is-error rp-noprint" role="alert">{error}<button type="button" aria-label="ปิด" onClick={() => setError("")}>×</button></div>}
      {notice && <div className="rp-alert is-notice rp-noprint" role="status">{notice}<button type="button" aria-label="ปิด" onClick={() => setNotice("")}>×</button></div>}

      {tab === "export" ? (
        <div className="rp-body rp-noprint">
          <p className="rp-lead">ส่งออกค่าจากเซนเซอร์ที่ติดตั้งประจำจุดเป็นไฟล์ CSV (เปิดด้วย Excel ได้ทันที) · อุปกรณ์สวมใส่และแท็กที่ติดตามข้าม gateway เป็นข้อมูลส่วนบุคคล จึงไม่อยู่ในรายการนี้</p>
          <div className="rp-card">
            <h2>1. เลือกเซนเซอร์</h2>
            <SensorPicker sensors={sensors} selected={exportSensors} onChange={setExportSensors} />
          </div>
          <div className="rp-card">
            <h2>2. ช่วงเวลาและความละเอียด</h2>
            <DateRange from={exportFrom} to={exportTo} onChange={(f, t) => { setExportFrom(f); setExportTo(t); }} />
            <div className="rp-resolutions" role="radiogroup" aria-label="ความละเอียดของข้อมูล">
              {RESOLUTIONS.map(([id, label]) => <label key={id} className={resolution === id ? "is-on" : ""}><input type="radio" name="rp-resolution" value={id} checked={resolution === id} onChange={() => setResolution(id)} />{label}</label>)}
            </div>
            {resolution === "raw" && <p className={`rp-hint ${exportSensors.length * spanDays(exportFrom, exportTo) > RAW_SENSOR_DAYS ? "is-over" : ""}`}>ข้อมูลดิบส่งออกได้ครั้งละไม่เกิน {RAW_SENSOR_DAYS} เซนเซอร์-วัน (เช่น 4 เซนเซอร์ 30 วัน หรือ 50 เซนเซอร์ 2 วัน) · ตอนนี้ {exportSensors.length * Math.max(0, spanDays(exportFrom, exportTo))} · ไม่รวมช่วงไม่กี่นาทีล่าสุดที่ยังไม่สรุป</p>}
            <p className="rp-dim">ค่าเฉลี่ยรายช่วงคำนวณจากค่าเฉลี่ยทุก 5 นาที · ข้อมูลรายวันตัดตามวันปฏิทินเวลาไทย · บรรทัดสุดท้ายของไฟล์คือ #END (ครบ) หรือ #INCOMPLETE (ไม่ครบ)</p>
          </div>
          <div className="rp-actions"><button type="button" className="rp-btn primary" disabled={busy || exportSensors.length === 0} onClick={() => void download()}><FileDown size={15} aria-hidden="true" />{busy ? "กำลังเตรียมไฟล์…" : "ดาวน์โหลด CSV"}</button></div>
        </div>
      ) : (
        <div className="rp-summary">
          <aside className="rp-side rp-noprint" aria-label="รายงานที่บันทึกไว้">
            <h2>รายงานที่บันทึกไว้</h2>
            <button type="button" className={`rp-def ${selected === "" ? "is-on" : ""}`} onClick={() => choose("")}><Plus size={14} aria-hidden="true" /><span><strong>{canWrite ? "สร้างรายงานใหม่" : "กำหนดเอง"}</strong><small>เลือกเซนเซอร์และเกณฑ์เอง</small></span></button>
            {definitions.map((d) => <button type="button" key={d.id} className={`rp-def ${selected === d.id ? "is-on" : ""}`} onClick={() => choose(d.id)}><span><strong>{d.name}</strong><small>{projectName(d.project_id)} · {d.sensors.length} เซนเซอร์</small>{d.schedule !== "none" && <small className="rp-def-schedule"><CalendarClock size={12} aria-hidden="true" />ส่ง{scheduleLabel(d)}</small>}</span></button>)}
            {definitions.length === 0 && <p className="rp-dim">ยังไม่มีรายงานที่บันทึกไว้</p>}
          </aside>
          <div className="rp-main">
            <div className="rp-card rp-noprint">
              <div className="rp-form">
                <label className="rp-wide">ชื่อรายงาน<input value={form.name} maxLength={120} onChange={(e) => edit({ name: e.target.value })} placeholder="เช่น ห้องเย็นเก็บวัคซีน" disabled={!canWrite && !!selected} /></label>
                <label>โปรเจค<select value={form.project_id} onChange={(e) => edit({ project_id: e.target.value })} disabled={!canWrite}>
                  {/* A workspace report needs every project in view; a member limited to projects saves into one of theirs. */}
                  {wholeWorkspace ? <option value="">ทั้ง workspace</option> : form.project_id === "" && <option value="" disabled>{selected ? "ทั้ง workspace" : "เลือกโปรเจค"}</option>}
                  {projects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                </select></label>
                <fieldset><legend><Thermometer size={14} aria-hidden="true" />อุณหภูมิที่ยอมรับ (°C)</legend><input inputMode="decimal" aria-label="อุณหภูมิต่ำสุด" placeholder="ต่ำสุด" value={form.temp_min} onChange={(e) => edit({ temp_min: e.target.value })} /><span>–</span><input inputMode="decimal" aria-label="อุณหภูมิสูงสุด" placeholder="สูงสุด" value={form.temp_max} onChange={(e) => edit({ temp_max: e.target.value })} /></fieldset>
                <fieldset><legend><Droplets size={14} aria-hidden="true" />ความชื้นที่ยอมรับ (%)</legend><input inputMode="decimal" aria-label="ความชื้นต่ำสุด" placeholder="ต่ำสุด" value={form.hum_min} onChange={(e) => edit({ hum_min: e.target.value })} /><span>–</span><input inputMode="decimal" aria-label="ความชื้นสูงสุด" placeholder="สูงสุด" value={form.hum_max} onChange={(e) => edit({ hum_max: e.target.value })} /></fieldset>
              </div>
              <SensorPicker sensors={sensors} selected={form.sensors} onChange={(keys) => edit({ sensors: keys })} />
              {form.sensors.some((k) => !byKey.has(k)) && <div className="rp-missing">
                <p className="rp-dim">เซนเซอร์เหล่านี้อยู่ในรายงานแต่ออกรายงานไม่ได้แล้ว (ถอนการลงทะเบียน หรือเปลี่ยนเป็นอุปกรณ์สวมใส่/roaming) จะไม่อยู่ในผลรายงาน{canWrite ? " · กด × เพื่อนำออก แล้วบันทึก" : ""}</p>
                <ul>{form.sensors.filter((k) => !byKey.has(k)).map((k) => <li key={k}><span className="rp-mono">{refOf(k).external_id}</span>{canWrite && <button type="button" aria-label={`นำ ${refOf(k).external_id} ออกจากรายงาน`} onClick={() => edit({ sensors: form.sensors.filter((x) => x !== k) })}>×</button>}</li>)}</ul>
              </div>}
              <div className="rp-schedule">
                <h3><CalendarClock size={15} aria-hidden="true" />ส่งรายงานอัตโนมัติ</h3>
                <div className="rp-schedule-row">
                  <label>ความถี่<select value={form.schedule} onChange={(e) => edit({ schedule: e.target.value as Form["schedule"] })} disabled={!canWrite}>
                    <option value="none">ไม่ส่งอัตโนมัติ</option><option value="daily">ทุกวัน · ข้อมูลของเมื่อวาน</option><option value="weekly">ทุกสัปดาห์ · ข้อมูล 7 วันก่อนหน้า</option>
                  </select></label>
                  {form.schedule === "weekly" && <label>วัน<select value={form.weekday} onChange={(e) => edit({ weekday: e.target.value })} disabled={!canWrite}>{WEEKDAYS.map((d, i) => <option key={d} value={String(i + 1)}>{d}</option>)}</select></label>}
                  {form.schedule !== "none" && <label>เวลา (เวลาไทย)<input type="time" value={form.time} onChange={(e) => edit({ time: e.target.value })} disabled={!canWrite} required /></label>}
                </div>
                <fieldset className="rp-channels"><legend>ช่องทางส่ง (ใช้ทั้งส่งอัตโนมัติและปุ่มส่งตอนนี้)</legend>
                  {channels.filter((c) => c.project_id === null || c.project_id === (form.project_id || null) || form.channels.includes(c.id)).map((c) => {
                    const unavailable = !c.enabled ? "ปิดใช้งานอยู่" : c.kind === "email" && !emailAvailable ? "server ยังไม่ตั้งค่า SMTP" : c.project_id !== null && c.project_id !== (form.project_id || null) ? "เป็นของโปรเจคอื่น" : "";
                    const on = form.channels.includes(c.id);
                    return <label key={c.id} className={`${on ? "is-on" : ""} ${unavailable ? "is-off" : ""}`}><input type="checkbox" checked={on} disabled={!canWrite || (!on && !!unavailable)} onChange={() => edit({ channels: on ? form.channels.filter((x) => x !== c.id) : [...form.channels, c.id] })} /><KindIcon kind={c.kind} />{c.name}{unavailable && <small>{unavailable}</small>}</label>;
                  })}
                  {channels.length === 0 && <p className="rp-dim">ยังไม่มีช่องทางส่ง เพิ่มได้ที่หน้าการแจ้งเตือน (Webhook, LINE หรืออีเมล)</p>}
                </fieldset>
                {(() => { const d = definitions.find((x) => x.id === selected); return d?.next_run_at && !dirty ? <p className="rp-dim">ส่งครั้งถัดไป {thaiTime(d.next_run_at)} · ข้อความ LINE มีลิงก์กลับมาที่รายงานนี้ · อีเมลแนบไฟล์ CSV รายชั่วโมง</p> : null; })()}
              </div>
              {canWrite && <div className="rp-actions">
                {selected && <button type="button" className="rp-btn" disabled={busy || !dirty} onClick={() => void save(false)}><Save size={15} aria-hidden="true" />บันทึกการแก้ไข</button>}
                <button type="button" className="rp-btn" disabled={busy || !form.name.trim() || form.sensors.length === 0 || (!wholeWorkspace && !form.project_id)} onClick={() => void save(true)}><Plus size={15} aria-hidden="true" />{selected ? "บันทึกเป็นรายงานใหม่" : "บันทึกรายงาน"}</button>
                {selected && (confirmDelete ? <><button type="button" className="rp-btn danger" disabled={busy} onClick={() => void remove()}>ยืนยันลบ</button><button type="button" className="rp-btn" onClick={() => setConfirmDelete(false)}>ยกเลิก</button></> : <button type="button" className="rp-btn danger" disabled={busy} onClick={() => setConfirmDelete(true)}><Trash2 size={15} aria-hidden="true" />ลบ</button>)}
              </div>}
            </div>
            <div className="rp-card rp-run rp-noprint">
              <DateRange from={summaryFrom} to={summaryTo} onChange={(f, t) => { setSummaryFrom(f); setSummaryTo(t); }} />
              <div className="rp-actions">
                <button type="button" className="rp-btn primary" disabled={busy || (!selected && form.sensors.length === 0)} onClick={() => void run()}>{busy ? "กำลังคำนวณ…" : "ดูรายงาน"}</button>
                <button type="button" className="rp-btn" disabled={!summary} onClick={() => window.print()}><Printer size={15} aria-hidden="true" />พิมพ์ / บันทึก PDF</button>
                {selected && canWrite && <button type="button" className="rp-btn" disabled={busy || dirty || form.channels.length === 0} title={dirty ? "บันทึกการแก้ไขก่อนส่ง" : form.channels.length === 0 ? "เลือกช่องทางส่งก่อน" : "ส่งรายงานช่วงวันที่ที่เลือกไปยังช่องทางของรายงานนี้"} onClick={() => void sendNow()}><Send size={15} aria-hidden="true" />ส่งตอนนี้</button>}
              </div>
            </div>
            {selected && <div className="rp-card rp-runs rp-noprint">
              <div className="rp-runs-head"><h2>ประวัติการส่ง</h2><button type="button" className="rp-icon-btn" onClick={() => void loadRuns(selected)} title="โหลดใหม่" aria-label="โหลดประวัติการส่งใหม่"><RefreshCw size={14} /></button></div>
              {runs.length === 0 ? <p className="rp-dim">ยังไม่เคยส่งรายงานนี้</p> : <ul className="rp-run-list">{runs.map((run) => <li key={run.id}>
                <div><strong>{thaiDay(bangkokDay(run.period_from))}{bangkokDay(run.period_from) !== addDays(bangkokDay(run.period_to), -1) && ` – ${thaiDay(addDays(bangkokDay(run.period_to), -1))}`}</strong><small>{run.trigger === "schedule" ? "ตามตาราง" : "ส่งตอนนี้"} · {thaiTime(run.created_at)}{run.status === "queued" && " · กำลังส่ง"}</small></div>
                <ul className="rp-deliveries">{run.deliveries.map((d) => <li key={d.id} className={`is-${d.status}`}><KindIcon kind={d.channel_kind} /><span>{d.channel_name || "ช่องทางถูกลบ"} · {DELIVERY_LABEL[d.status]}{d.attempts > 1 && ` (ลอง ${d.attempts} ครั้ง)`}</span>{d.last_error && <small>{d.last_error}</small>}</li>)}
                  {run.deliveries.length === 0 && <li className="is-skipped"><span>ไม่มีช่องทางที่ยังใช้ได้</span></li>}</ul>
              </li>)}</ul>}
            </div>}
            {summary && (
              <article className="rp-report" aria-label="ผลรายงาน">
                <header className="rp-report-head">
                  <div><span className="rp-kicker">AETHER · {summary.workspace}</span><h2>{summary.report?.name ?? "รายงานสภาพแวดล้อม"}</h2></div>
                  <dl>
                    <div><dt>ช่วงเวลา</dt><dd>{thaiDay(firstDay)} – {thaiDay(lastDay)}</dd></div>
                    <div><dt>เกณฑ์อุณหภูมิ</dt><dd>{rangeLabel(summary.temp, "°C")}</dd></div>
                    <div><dt>เกณฑ์ความชื้น</dt><dd>{rangeLabel(summary.hum, "%")}</dd></div>
                    <div><dt>ออกรายงานเมื่อ</dt><dd>{new Date(summary.generated_at).toLocaleString("th-TH", { timeZone: "Asia/Bangkok", hour12: false })}</dd></div>
                  </dl>
                </header>
                <div className="rp-table-wrap"><table className="rp-table">
                  <thead><tr><th>เซนเซอร์</th><th>ครอบคลุม</th><th>อุณหภูมิ ต่ำสุด / สูงสุด / เฉลี่ย</th><th>ความชื้น ต่ำสุด / สูงสุด / เฉลี่ย</th><th>นอกเกณฑ์</th></tr></thead>
                  <tbody>{summary.sensors.map((s) => <tr key={keyOf(s)}><td><strong>{s.name}</strong><br /><small className="rp-dim">{s.gateway_name}</small></td><td className={s.coverage_pct < 90 ? "is-warn" : ""}>{num(s.coverage_pct)}%</td><td>{num(s.temperature?.min)} / {num(s.temperature?.max)} / {num(s.temperature?.avg)}</td><td>{num(s.humidity?.min)} / {num(s.humidity?.max)} / {num(s.humidity?.avg)}</td><td className={s.excursions.length ? "is-hot" : ""}>{s.excursions.length ? `${s.excursions.length}${s.excursions_truncated ? "+" : ""} ครั้ง · ${duration(s.minutes_out_of_range.temperature + s.minutes_out_of_range.humidity)}` : "ไม่มี"}</td></tr>)}</tbody>
                </table></div>
                {summary.omitted.length > 0 && <p className="rp-omitted">ไม่รวมในรายงาน: {summary.omitted.map((o) => `${o.external_id} (${OMITTED[o.reason] ?? o.reason})`).join(", ")}</p>}
                <p className="rp-dim rp-method">ค่าคำนวณจากข้อมูลสรุปทุก 5 นาที · ค่าเฉลี่ยคือค่าเฉลี่ยของแต่ละช่วง 5 นาที · ช่วงออกนอกเกณฑ์คือช่วง 5 นาทีที่ติดกันซึ่งมีค่าสูงสุดเกินหรือค่าต่ำสุดต่ำกว่าเกณฑ์ ช่วงที่ไม่มีข้อมูลจะตัดช่วงนั้น · เวลาไทย (UTC+7)</p>
                {summary.sensors.map((s) => <SensorSection key={keyOf(s)} s={s} summary={summary} />)}
              </article>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
