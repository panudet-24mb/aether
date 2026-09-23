"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { Activity, Bell, BellRing, Check, CheckCheck, DoorOpen, Droplets, ListChecks, Mail, MessageSquare, PersonStanding, Plus, RefreshCw, Send, ShieldAlert, Trash2, Webhook, WifiOff, Zap } from "lucide-react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import "./alerts.css";
import { ApiError, createClientFrom } from "../topology/api";
import { useLatest } from "../topology/use-latest";
import { useSignals } from "../topology/use-signals";
import { formatMAC } from "../topology/catalog";

type Alert = { id: string; rule_id: string | null; event_id: string; gateway_id: string; external_id: string; device_name: string; event_type: string; severity: string; title: string; status: string; opened_at: string; acked_at: string | null; resolved_at: string | null; note: string | null };
type Event = { id: string; gateway_id: string; external_id: string; device_name: string; event_type: string; detail: Record<string, unknown>; occurred_at: string };
/** Office-hours window for door / occupancy rules, evaluated by the server in Asia/Bangkok. days: 0 = Sunday … 6 = Saturday. */
type AfterHours = { from: string; to: string; days: number[] };
type Scope = { external_ids?: string[]; offline_after_sec?: number; metric?: string; op?: string; value?: number; gateway_ids?: string[]; after_hours?: AfterHours };
// `builtin`: a rule Aether seeded for the workspace (emergency button, tamper). Label only — it can be
// edited, disabled and deleted like any other rule.
type Rule = { id: string; name: string; enabled: boolean; event_type: string; severity: string; scope: Scope; channels: string[]; dedupe_sec: number; created_at: string; builtin?: boolean };
type Channel = { id: string; name: string; kind: string; enabled: boolean; config: Record<string, string>; has_secret: boolean; created_at: string };
type Notification = { id: string; alert_id: string; channel_id: string | null; status: string; attempts: number; next_attempt_at: string; last_error: string | null; created_at: string; sent_at: string | null };
type Tab = "alerts" | "events" | "rules" | "channels";

const EVENT_LABEL: Record<string, string> = { tamper: "ป้ายถูกถอด (tamper)", tamper_cleared: "tamper กลับสู่ปกติ", button: "กดปุ่ม / instance เปลี่ยน", leak: "พบน้ำรั่ว", leak_cleared: "น้ำรั่วหาย", motion: "เริ่มเคลื่อนไหว", motion_stopped: "หยุดเคลื่อนไหว", offline: "ขาดการติดต่อ", online: "กลับมาออนไลน์", threshold: "ค่าเกินเกณฑ์", threshold_cleared: "ค่ากลับเข้าเกณฑ์", zone: "wearable เข้าโซน", door_open: "เปิดประตู", door_closed: "ปิดประตู", occupied: "มีคนในพื้นที่", vacant: "ไม่มีคนแล้ว", test: "ทดสอบ" };
const RULE_TYPES: [string, string][] = [["tamper", "ป้ายถูกถอด (tamper)"], ["button", "กดปุ่ม (B10 · instance เปลี่ยน)"], ["leak", "น้ำรั่ว"], ["motion", "เริ่มเคลื่อนไหว"], ["offline", "อุปกรณ์ขาดการติดต่อ"], ["zone", "wearable เข้าโซน (roaming)"], ["door", "ประตูเปิด"], ["occupancy", "มีคนเคลื่อนไหว"], ["threshold", "ค่าเกินเกณฑ์ (อุณหภูมิ/ความชื้น/แบต/RSSI)"]];
/** Rule types that may be limited to outside office hours. */
const AFTER_HOURS_TYPES = new Set(["door", "occupancy"]);
const DEFAULT_AFTER_HOURS: AfterHours = { from: "18:00", to: "08:00", days: [0, 1, 2, 3, 4, 5, 6] };
const WEEKDAYS: [number, string, string][] = [[1, "จ", "จันทร์"], [2, "อ", "อังคาร"], [3, "พ", "พุธ"], [4, "พฤ", "พฤหัสบดี"], [5, "ศ", "ศุกร์"], [6, "ส", "เสาร์"], [0, "อา", "อาทิตย์"]];
const afterHoursText = (a: AfterHours) => `นอกเวลา ${a.from}–${a.to}${a.days.length === 7 ? " ทุกวัน" : ` · ${WEEKDAYS.filter(([d]) => a.days.includes(d)).map(([, s]) => s).join(" ")}`}`;
const SEVERITY_LABEL: Record<string, string> = { info: "ข้อมูล", warning: "เตือน", critical: "วิกฤต" };
const KIND_LABEL: Record<string, string> = { webhook: "Webhook", line: "LINE Messaging API", email: "อีเมล" };

function EventIcon({ type, size = 15 }: { type: string; size?: number }) {
  if (type.startsWith("tamper")) return <ShieldAlert size={size} />;
  if (type === "button") return <BellRing size={size} />;
  if (type.startsWith("leak")) return <Droplets size={size} />;
  if (type.startsWith("motion")) return <Activity size={size} />;
  if (type === "door" || type.startsWith("door_")) return <DoorOpen size={size} />;
  if (type === "occupancy" || type === "occupied" || type === "vacant") return <PersonStanding size={size} />;
  if (type === "offline" || type === "online") return <WifiOff size={size} />;
  if (type.startsWith("threshold")) return <Zap size={size} />;
  return <Bell size={size} />;
}
const when = (s: string | null | undefined) => (s ? new Date(s).toLocaleString("th-TH", { hour12: false }) : "—");
const utf8 = (s: string) => new TextEncoder().encode(s).length;

export default function AlertsCenter({ getToken, refresh, onUnauthorized, onOpenDevice, onSummary }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void; onOpenDevice?: (external: string) => void; /** Keeps the sidebar badge in step with this page instead of waiting for its own slower poll. */ onSummary?: (open: number) => void }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const [tab, setTab] = useState<Tab>("alerts");
  const [alertsList, setAlerts] = useState<Alert[]>([]);
  const [status, setStatus] = useState<"open" | "acknowledged" | "resolved" | "">("open");
  const [events, setEvents] = useState<Event[]>([]);
  const [eventFilterInput, setEventFilterInput] = useState("");
  const [eventFilter, setEventFilter] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setEventFilter(eventFilterInput), 400); // one request after typing stops, not one per key
    return () => clearTimeout(t);
  }, [eventFilterInput]);
  const [rules, setRules] = useState<Rule[]>([]);
  const [gateways, setGateways] = useState<{ id: string; name: string }[]>([]);
  const [channels, setChannels] = useState<Channel[]>([]);
  const [emailAvailable, setEmailAvailable] = useState(false);
  const [deliveries, setDeliveries] = useState<Notification[]>([]);
  const [summary, setSummary] = useState({ open: 0, acknowledged: 0 });
  const [error, setError] = useState("");
  // Errors from user actions live separately so the background poll cannot wipe them before they are read.
  const [actionError, setActionError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [resolveTarget, setResolveTarget] = useState<Alert | null>(null);
  const [note, setNote] = useState("");
  const [ruleEditor, setRuleEditor] = useState<Partial<Rule> | null>(null);
  const [channelEditor, setChannelEditor] = useState<{ name: string; kind: string; url: string; to: string; secret: string } | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<{ kind: "rule" | "channel"; id: string; name: string } | null>(null);
  const unauthorized = useRef(false);
  const onSummaryRef = useLatest(onSummary);
  useEffect(() => {
    onSummaryRef.current?.(summary.open);
  }, [summary.open, onSummaryRef]);

  const get = useCallback(
    async <T,>(path: string): Promise<T> => {
      try {
        return await client.raw<T>(path);
      } catch (e) {
        if (e instanceof ApiError && e.status === 401 && !unauthorized.current) {
          unauthorized.current = true;
          onUnauthorized?.();
        }
        throw e;
      }
    },
    [client, onUnauthorized],
  );

  const load = useCallback(async () => {
    try {
      // Poll only what the active tab shows (the API is rate-limited per IP); summary feeds the header.
      const summaryP = get<{ open: number; acknowledged: number }>("/alerts/summary");
      if (tab === "alerts") {
        const [s, a] = await Promise.all([summaryP, get<{ items: Alert[] }>(`/alerts?status=${status}&limit=200`)]);
        setSummary(s);
        setAlerts(a.items);
      } else if (tab === "events") {
        const [s, ev] = await Promise.all([summaryP, get<{ items: Event[] }>(`/events?limit=200${eventFilter ? `&external_id=${encodeURIComponent(eventFilter)}` : ""}`)]);
        setSummary(s);
        setEvents(ev.items);
      } else if (tab === "rules") {
        const [s, r, ch, gw] = await Promise.all([summaryP, get<{ items: Rule[] }>("/rules"), get<{ items: Channel[]; email_available: boolean }>("/channels"), get<{ items: { id: string; name: string }[] }>("/gateways")]);
        setGateways(gw.items);
        setSummary(s);
        setRules(r.items);
        setChannels(ch.items);
        setEmailAvailable(ch.email_available);
      } else {
        const [s, ch, n, r] = await Promise.all([summaryP, get<{ items: Channel[]; email_available: boolean }>("/channels"), get<{ items: Notification[] }>("/notifications?limit=100"), get<{ items: Rule[] }>("/rules")]);
        setSummary(s);
        setChannels(ch.items);
        setEmailAvailable(ch.email_available);
        setDeliveries(n.items);
        setRules(r.items);
      }
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "โหลดข้อมูลไม่ได้");
    }
  }, [get, tab, status, eventFilter]);

  // Push: the server only says "alerts/events changed"; the data still comes from the REST calls above.
  const loadRef = useLatest(load);
  const pushTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const live = useSignals(handlers, (kind) => {
    if (kind !== "alert" && kind !== "event") return;
    clearTimeout(pushTimer.current);
    pushTimer.current = setTimeout(() => void loadRef.current(), 400);
  });
  useEffect(() => () => clearTimeout(pushTimer.current), []);
  const liveRef = useLatest(live);

  useEffect(() => {
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      await load();
      // The poll is only a safety net while the socket is up.
      if (active && !unauthorized.current) timer = setTimeout(poll, liveRef.current ? 60000 : 15000);
    };
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [load]);
  useEffect(() => {
    if (!notice) return;
    const t = setTimeout(() => setNotice(""), 3500);
    return () => clearTimeout(t);
  }, [notice]);

  async function action(fn: () => Promise<void>, ok?: string) {
    setBusy(true);
    setActionError("");
    try {
      await fn();
      if (ok) setNotice(ok);
      await load();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : "เกิดข้อผิดพลาด");
    } finally {
      setBusy(false);
    }
  }
  const channelName = (id: string | null) => channels.find((c) => c.id === id)?.name ?? (id ? id.slice(0, 8) : "ช่องทางถูกลบ");

  return (
    <section className="ac">
      <header className="ac-bar">
        <div>
          <span className="ac-kicker">OPERATE / ALERTS</span>
          <h1>การแจ้งเตือน</h1>
        </div>
        <div className="ac-stats">
          <span className={summary.open ? "is-hot" : ""}>
            <b>{summary.open}</b> เปิดอยู่
          </span>
          <span>
            <b>{summary.acknowledged}</b> รับทราบแล้ว
          </span>
          <span>
            <b>{rules.filter((r) => r.enabled).length}</b> กฎที่เปิดใช้
          </span>
          <span>
            <b>{channels.filter((c) => c.enabled).length}</b> ช่องทาง
          </span>
        </div>
        <nav className="ac-tabs" aria-label="มุมมอง">
          {(
            [
              ["alerts", "แจ้งเตือน", <Bell size={15} key="a" />],
              ["events", "เหตุการณ์", <ListChecks size={15} key="e" />],
              ["rules", "กฎ", <Zap size={15} key="r" />],
              ["channels", "ช่องทาง", <Send size={15} key="c" />],
            ] as [Tab, string, React.ReactNode][]
          ).map(([id, label, icon]) => (
            <button key={id} type="button" className={tab === id ? "is-on" : ""} aria-pressed={tab === id} onClick={() => setTab(id)}>
              {icon} {label}
            </button>
          ))}
          <button type="button" className="ac-icon" onClick={() => void load()} title="รีเฟรช" aria-label="รีเฟรช" disabled={busy}>
            <RefreshCw size={15} />
          </button>
        </nav>
      </header>
      {(actionError || error) && (
        <div className="ac-alert is-error" role="alert">
          {actionError || error}
          {actionError && (
            <button type="button" className="ac-dismiss" aria-label="ปิด" onClick={() => setActionError("")}>
              ×
            </button>
          )}
        </div>
      )}
      {notice && (
        <div className="ac-alert is-notice" role="status">
          {notice}
        </div>
      )}

      {tab === "alerts" && (
        <div className="ac-body">
          <div className="ac-toolbar">
            {(
              [
                ["open", "เปิดอยู่"],
                ["acknowledged", "รับทราบแล้ว"],
                ["resolved", "ปิดแล้ว"],
                ["", "ทั้งหมด"],
              ] as const
            ).map(([v, l]) => (
              <button key={v} type="button" className={`ac-chip ${status === v ? "is-on" : ""}`} aria-pressed={status === v} onClick={() => setStatus(v)}>
                {l}
              </button>
            ))}
          </div>
          {alertsList.length === 0 && (
            <div className="ac-empty">
              <CheckCheck size={30} />
              <h2>{status === "open" ? "ไม่มีการแจ้งเตือนที่เปิดอยู่" : "ไม่มีรายการ"}</h2>
              <p>เมื่อเหตุการณ์ตรงกับกฎ ระบบจะเปิดการแจ้งเตือนที่นี่และส่งไปยังช่องทางที่กำหนด</p>
            </div>
          )}
          <ul className="ac-list">
            {alertsList.map((a) => (
              <li key={a.id} className={`ac-item sev-${a.severity} st-${a.status}`}>
                <span className="ac-item-icon">
                  <EventIcon type={a.event_type} size={18} />
                </span>
                <div className="ac-item-body">
                  <div className="ac-item-head">
                    <strong>{a.title}</strong>
                    <span className={`ac-sev sev-${a.severity}`}>{SEVERITY_LABEL[a.severity] ?? a.severity}</span>
                    <span className={`ac-status st-${a.status}`}>{a.status === "open" ? "เปิดอยู่" : a.status === "acknowledged" ? "รับทราบแล้ว" : "ปิดแล้ว"}</span>
                  </div>
                  <small>
                    {EVENT_LABEL[a.event_type] ?? a.event_type} · <button type="button" className="ac-link" onClick={() => onOpenDevice?.(a.external_id)}>{a.external_id === "test" ? "test" : formatMAC(a.external_id)}</button> · เปิดเมื่อ {when(a.opened_at)}
                    {a.acked_at ? ` · รับทราบ ${when(a.acked_at)}` : ""}
                    {a.resolved_at ? ` · ปิด ${when(a.resolved_at)}` : ""}
                    {a.note ? ` · บันทึก: ${a.note}` : ""}
                  </small>
                </div>
                <div className="ac-item-actions">
                  {a.status === "open" && (
                    <button type="button" className="ac-btn" disabled={busy} onClick={() => void action(() => client.post(`/alerts/${a.id}/ack`, {}), "รับทราบแล้ว")}>
                      <Check size={14} /> รับทราบ
                    </button>
                  )}
                  {a.status !== "resolved" && (
                    <button type="button" className="ac-btn primary" disabled={busy} onClick={() => { setNote(""); setResolveTarget(a); }}>
                      <CheckCheck size={14} /> ปิดเรื่อง
                    </button>
                  )}
                </div>
              </li>
            ))}
          </ul>
        </div>
      )}

      {tab === "events" && (
        <div className="ac-body">
          <div className="ac-toolbar">
            <input value={eventFilterInput} onChange={(e) => setEventFilterInput(e.target.value.replace(/:/g, "").toLowerCase())} placeholder="กรองตาม MAC เช่น f00000000009" aria-label="กรองเหตุการณ์ตามอุปกรณ์" />
            <span className="ac-note">บันทึกเฉพาะการเปลี่ยนสถานะ (ขอบขึ้น/ลง) ไม่ใช่ทุก uplink · แสดง 200 รายการล่าสุด</span>
          </div>
          {events.length === 0 && <div className="ac-empty"><ListChecks size={30} /><h2>ยังไม่มีเหตุการณ์</h2><p>เหตุการณ์จะปรากฏเมื่ออุปกรณ์เปลี่ยนสถานะ เช่น tamper, กดปุ่ม, เริ่ม/หยุดเคลื่อนไหว, offline/online</p></div>}
          <ul className="ac-timeline">
            {events.map((ev) => (
              <li key={ev.id} className={`ev-${ev.event_type}`}>
                <span className="ac-tl-icon"><EventIcon type={ev.event_type} /></span>
                <div>
                  <strong>{EVENT_LABEL[ev.event_type] ?? ev.event_type}</strong> · {ev.device_name} · <button type="button" className="ac-link" onClick={() => onOpenDevice?.(ev.external_id)}>{formatMAC(ev.external_id)}</button>
                  <small>
                    {when(ev.occurred_at)}
                    {Object.entries(ev.detail ?? {}).filter(([k]) => k !== "frame").map(([k, v]) => ` · ${k}: ${typeof v === "object" ? JSON.stringify(v) : String(v)}`).join("")}
                  </small>
                </div>
              </li>
            ))}
          </ul>
        </div>
      )}

      {tab === "rules" && (
        <div className="ac-body">
          <div className="ac-toolbar">
            <button type="button" className="ac-btn primary" onClick={() => setRuleEditor({ name: "", enabled: true, event_type: "tamper", severity: "critical", scope: {}, channels: [], dedupe_sec: 600 })}>
              <Plus size={15} /> สร้างกฎ
            </button>
            <span className="ac-note">กฎ = ชนิดเหตุการณ์ + ขอบเขตอุปกรณ์ + ระดับ + ช่องทาง · ไม่เปิดซ้ำขณะที่เรื่องเดิมยังไม่ปิด และภายในช่วง dedupe หลังปิดเรื่อง</span>
          </div>
          {rules.length === 0 && <div className="ac-empty"><Zap size={30} /><h2>ยังไม่มีกฎ</h2><p>ตัวอย่างที่ควรมี: tamper ทุกอุปกรณ์ = วิกฤต, กดปุ่ม B10 = วิกฤต, ขาดการติดต่อ 5 นาที = เตือน, ประตูเปิดนอกเวลาทำการ = เตือน</p></div>}
          <ul className="ac-list">
            {rules.map((r) => (
              <li key={r.id} className={`ac-item ${r.enabled ? "" : "is-off"}`}>
                <span className="ac-item-icon"><EventIcon type={r.event_type} size={18} /></span>
                <div className="ac-item-body">
                  <div className="ac-item-head">
                    <strong>{r.name}</strong>
                    <span className={`ac-sev sev-${r.severity}`}>{SEVERITY_LABEL[r.severity]}</span>
                    {r.builtin && <span className="ac-status" title="Aether สร้างกฎนี้ให้ตอนเปิดใช้งาน แก้ไขหรือปิดได้ตามต้องการ">สร้างให้อัตโนมัติ</span>}
                    {!r.enabled && <span className="ac-status">ปิดใช้</span>}
                  </div>
                  <small>
                    {RULE_TYPES.find(([id]) => id === r.event_type)?.[1] ?? r.event_type}
                    {r.event_type === "threshold" ? ` · ${r.scope.metric} ${r.scope.op} ${r.scope.value}` : ""}
                    {r.event_type === "zone" ? ` · ${r.scope.gateway_ids?.length ? `${r.scope.gateway_ids.length} โซน` : "ทุกโซน"}` : ""}
                    {r.event_type === "offline" ? ` · หลัง ${r.scope.offline_after_sec || 300} วินาที` : ""}
                    {AFTER_HOURS_TYPES.has(r.event_type) ? ` · ${r.scope.after_hours ? afterHoursText(r.scope.after_hours) : "ตลอดเวลา"}` : ""}
                    {" · "}{r.scope.external_ids?.length ? `${r.scope.external_ids.length} อุปกรณ์` : "ทุกอุปกรณ์"}
                    {" · ส่งไป "}{r.channels.length ? r.channels.map(channelName).join(", ") : "ไม่ส่ง (แสดงในหน้านี้เท่านั้น)"}
                    {` · dedupe ${r.dedupe_sec}s`}
                  </small>
                </div>
                <div className="ac-item-actions">
                  <button type="button" className="ac-btn" onClick={() => setRuleEditor({ ...r, scope: { ...r.scope }, channels: [...r.channels] })}>แก้ไข</button>
                  <button type="button" className="ac-btn danger" aria-label={`ลบกฎ ${r.name}`} onClick={() => setDeleteTarget({ kind: "rule", id: r.id, name: r.name })}><Trash2 size={14} /></button>
                </div>
              </li>
            ))}
          </ul>
        </div>
      )}

      {tab === "channels" && (
        <div className="ac-body">
          <div className="ac-toolbar">
            <button type="button" className="ac-btn primary" onClick={() => setChannelEditor({ name: "", kind: "webhook", url: "", to: "", secret: "" })}>
              <Plus size={15} /> เพิ่มช่องทาง
            </button>
            <span className="ac-note">Webhook: POST JSON พร้อม HMAC ใน X-Aether-Signature · LINE: Messaging API push (ต้องมี channel access token และ user/group id) · อีเมล{emailAvailable ? " พร้อมใช้" : ": server ยังไม่ตั้งค่า SMTP"}</span>
          </div>
          {channels.length === 0 && <div className="ac-empty"><Send size={30} /><h2>ยังไม่มีช่องทาง</h2><p>เพิ่ม webhook เพื่อทดสอบได้ทันที (เช่น webhook.site หรือ endpoint ในเครื่องขณะพัฒนา)</p></div>}
          <ul className="ac-list">
            {channels.map((c) => (
              <li key={c.id} className={`ac-item ${c.enabled ? "" : "is-off"}`}>
                <span className="ac-item-icon">{c.kind === "webhook" ? <Webhook size={18} /> : c.kind === "line" ? <MessageSquare size={18} /> : <Mail size={18} />}</span>
                <div className="ac-item-body">
                  <div className="ac-item-head"><strong>{c.name}</strong><span className="ac-status">{KIND_LABEL[c.kind] ?? c.kind}</span>{c.has_secret && <span className="ac-status">มี secret</span>}</div>
                  <small>{c.kind === "webhook" ? (c.config.url ?? "ปลายทางถูกซ่อน (เฉพาะ owner/admin)") : c.config.to ? `ส่งถึง ${c.config.to}` : "ผู้รับถูกซ่อน (เฉพาะ owner/admin)"} · สร้าง {when(c.created_at)}</small>
                </div>
                <div className="ac-item-actions">
                  <button type="button" className="ac-btn" disabled={busy} aria-pressed={c.enabled} onClick={() => void action(() => client.post(`/channels/${c.id}/update`, { enabled: !c.enabled }), c.enabled ? "ปิดใช้ช่องทางแล้ว" : "เปิดใช้ช่องทางแล้ว")}>{c.enabled ? "ปิดใช้" : "เปิดใช้"}</button>
                  <button type="button" className="ac-btn" disabled={busy} onClick={() => void action(async () => { await client.post(`/channels/${c.id}/test`, {}); }, `ส่งข้อความทดสอบไป ${c.name} แล้ว`)}><Send size={14} /> ทดสอบ</button>
                  <button type="button" className="ac-btn danger" aria-label={`ลบช่องทาง ${c.name}`} onClick={() => setDeleteTarget({ kind: "channel", id: c.id, name: c.name })}><Trash2 size={14} /></button>
                </div>
              </li>
            ))}
          </ul>
          <h3 className="ac-h3">บันทึกการส่ง</h3>
          {deliveries.length === 0 && <p className="ac-note">ยังไม่มีการส่ง</p>}
          <ul className="ac-deliveries">
            {deliveries.map((n) => (
              <li key={n.id} className={`dl-${n.status}`}>
                <span className="ac-status">{n.status === "sent" ? "ส่งแล้ว" : n.status === "failed" ? "ล้มเหลว" : "รอส่ง"}</span>
                <span>{channelName(n.channel_id)}</span>
                <span>ครั้งที่ {n.attempts}</span>
                <span>{when(n.sent_at ?? n.created_at)}</span>
                {n.last_error && <span className="ac-err">{n.last_error}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* Resolve with note */}
      <Dialog open={resolveTarget !== null} onOpenChange={(o) => !o && !busy && setResolveTarget(null)}>
        <DialogContent className="ac-dialog">
          <DialogHeader><DialogTitle>ปิดเรื่อง</DialogTitle><DialogDescription>{resolveTarget?.title}</DialogDescription></DialogHeader>
          <label>บันทึกสิ่งที่ทำ (ไม่บังคับ)<textarea value={note} maxLength={500} onChange={(e) => setNote(e.target.value)} placeholder="เช่น ติดป้ายกลับเข้าที่แล้ว" /></label>
          <div className="ac-dialog-actions">
            <button type="button" className="ac-btn" disabled={busy} onClick={() => setResolveTarget(null)}>ยกเลิก</button>
            <button type="button" className="ac-btn primary" disabled={busy} onClick={() => { const t = resolveTarget!; setResolveTarget(null); void action(() => client.post(`/alerts/${t.id}/resolve`, { note }), "ปิดเรื่องแล้ว"); }}>ปิดเรื่อง</button>
          </div>
        </DialogContent>
      </Dialog>

      {/* Rule editor */}
      {ruleEditor && (
        <RuleForm rule={ruleEditor} channels={channels} gateways={gateways} busy={busy} error={actionError} onCancel={() => { setActionError(""); setRuleEditor(null); }} onSave={(rule) => void action(async () => {
          const payload = { name: rule.name, enabled: rule.enabled, event_type: rule.event_type, severity: rule.severity, scope: rule.scope, channels: rule.channels, dedupe_sec: rule.dedupe_sec };
          if (rule.id) await client.post(`/rules/${rule.id}/update`, payload);
          else await client.post("/rules", payload);
          setRuleEditor(null);
        }, "บันทึกกฎแล้ว")} />
      )}

      {/* Channel editor */}
      <Dialog open={channelEditor !== null} onOpenChange={(o) => !o && !busy && setChannelEditor(null)}>
        <DialogContent className="ac-dialog">
          <DialogHeader><DialogTitle>เพิ่มช่องทางแจ้งเตือน</DialogTitle><DialogDescription>secret ถูกเข้ารหัสเก็บฝั่ง server และไม่แสดงอีก</DialogDescription></DialogHeader>
          {channelEditor && (
            <form onSubmit={(e) => { e.preventDefault(); const c = channelEditor; void action(async () => {
              const config: Record<string, string> = c.kind === "webhook" ? { url: c.url.trim() } : { to: c.to.trim() };
              await client.post("/channels", { name: c.name.trim(), kind: c.kind, config, secret: c.secret });
              setChannelEditor(null);
            }, "เพิ่มช่องทางแล้ว · กดทดสอบเพื่อยืนยันว่าส่งถึง"); }}>
              <label>ชื่อ<input value={channelEditor.name} required onChange={(e) => setChannelEditor({ ...channelEditor, name: e.target.value })} placeholder="เช่น ทีมช่าง LINE group" /></label>
              <label>ชนิด<select value={channelEditor.kind} onChange={(e) => setChannelEditor({ ...channelEditor, kind: e.target.value })}>
                <option value="webhook">Webhook (HTTP POST JSON)</option>
                <option value="line">LINE Messaging API</option>
                <option value="email" disabled={!emailAvailable}>อีเมล{emailAvailable ? "" : " · server ยังไม่ตั้งค่า SMTP"}</option>
              </select></label>
              {channelEditor.kind === "webhook" && (<>
                <label>URL<input value={channelEditor.url} required onChange={(e) => setChannelEditor({ ...channelEditor, url: e.target.value })} placeholder="https://example.com/aether-alerts" /></label>
                <label>Secret สำหรับลายเซ็น HMAC (ไม่บังคับ)<input value={channelEditor.secret} onChange={(e) => setChannelEditor({ ...channelEditor, secret: e.target.value })} type="password" autoComplete="off" /></label>
                <p className="ac-note">ปลายทางต้องเป็น https ยกเว้นตอนพัฒนา (localhost/LAN) · ตรวจลายเซ็นด้วย HMAC‑SHA256 ของ body</p>
              </>)}
              {channelEditor.kind === "line" && (<>
                <label>Channel access token<input value={channelEditor.secret} required onChange={(e) => setChannelEditor({ ...channelEditor, secret: e.target.value })} type="password" autoComplete="off" /></label>
                <label>ส่งถึง (userId / groupId / roomId)<input value={channelEditor.to} required onChange={(e) => setChannelEditor({ ...channelEditor, to: e.target.value })} placeholder="Uxxxxxxxx หรือ Cxxxxxxxx" /></label>
                <p className="ac-note">ใช้ LINE Official Account + Messaging API (LINE Notify ยุติบริการแล้ว) · ผู้รับต้องเพิ่ม OA เป็นเพื่อนหรืออยู่ในกลุ่มที่มี OA</p>
              </>)}
              {channelEditor.kind === "email" && <label>ผู้รับ (คั่นด้วย , สูงสุด 10)<input value={channelEditor.to} required onChange={(e) => setChannelEditor({ ...channelEditor, to: e.target.value })} placeholder="ops@example.com, oncall@example.com" /></label>}
              {actionError && <p className="ac-dialog-error" role="alert">{actionError}</p>}
              <div className="ac-dialog-actions">
                <button type="button" className="ac-btn" disabled={busy} onClick={() => { setActionError(""); setChannelEditor(null); }}>ยกเลิก</button>
                <button type="submit" className="ac-btn primary" disabled={busy || !channelEditor.name.trim() || utf8(channelEditor.name) > 128}>{busy ? "กำลังบันทึก…" : "บันทึกช่องทาง"}</button>
              </div>
            </form>
          )}
        </DialogContent>
      </Dialog>

      <AlertDialog open={deleteTarget !== null} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>ลบ{deleteTarget?.kind === "rule" ? "กฎ" : "ช่องทาง"} “{deleteTarget?.name}”?</AlertDialogTitle><AlertDialogDescription>{deleteTarget?.kind === "rule" ? "การแจ้งเตือนเดิมยังอยู่ แต่จะไม่เปิดใหม่จากกฎนี้" : "กฎที่อ้างถึงช่องทางนี้จะไม่ส่งไปที่นี่อีก บันทึกการส่งเดิมยังอยู่"}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction className="is-danger" onClick={() => { const t = deleteTarget!; setDeleteTarget(null); void action(() => client.post(`/${t.kind === "rule" ? "rules" : "channels"}/${t.id}/delete`, {}), "ลบแล้ว"); }}>ลบ</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}

function RuleForm({ rule, channels, gateways, busy, error, onCancel, onSave }: { rule: Partial<Rule>; channels: Channel[]; gateways: { id: string; name: string }[]; busy: boolean; error: string; onCancel: () => void; onSave: (rule: Rule) => void }) {
  const [draft, setDraft] = useState<Rule>({ id: rule.id ?? "", name: rule.name ?? "", enabled: rule.enabled ?? true, event_type: rule.event_type ?? "tamper", severity: rule.severity ?? "critical", scope: rule.scope ?? {}, channels: rule.channels ?? [], dedupe_sec: rule.dedupe_sec ?? 600, created_at: rule.created_at ?? "" });
  const [ids, setIds] = useState((rule.scope?.external_ids ?? []).join(", "));
  const set = (patch: Partial<Rule>) => setDraft({ ...draft, ...patch });
  const scope = (patch: Scope) => set({ scope: { ...draft.scope, ...patch } });
  const afterHours = AFTER_HOURS_TYPES.has(draft.event_type) ? draft.scope.after_hours : undefined;
  const timeOk = (t: string) => /^([01]\d|2[0-3]):[0-5]\d$/.test(t);
  const valid = draft.name.trim() !== "" && utf8(draft.name) <= 128 && (draft.event_type !== "threshold" || (!!draft.scope.metric && !!draft.scope.op && draft.scope.value != null && Number.isFinite(draft.scope.value)))
    && (!afterHours || (timeOk(afterHours.from) && timeOk(afterHours.to) && afterHours.from !== afterHours.to && afterHours.days.length > 0));
  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onCancel()}>
      <DialogContent className="ac-dialog">
        <DialogHeader><DialogTitle>{draft.id ? "แก้ไขกฎ" : "สร้างกฎ"}</DialogTitle><DialogDescription>เมื่อเหตุการณ์ชนิดนี้เกิดกับอุปกรณ์ในขอบเขต ระบบจะเปิดการแจ้งเตือนและส่งไปยังช่องทางที่เลือก</DialogDescription></DialogHeader>
        <form onSubmit={(e) => { e.preventDefault(); if (!valid) return; const external_ids = ids.split(",").map((s) => s.trim().replace(/:/g, "").toLowerCase()).filter(Boolean); onSave({ ...draft, scope: { ...draft.scope, external_ids, gateway_ids: draft.event_type === "zone" && draft.scope.gateway_ids?.length ? draft.scope.gateway_ids : undefined, after_hours: afterHours ? { ...afterHours, days: [...afterHours.days].sort((a, b) => a - b) } : undefined } }); }}>
          <label>ชื่อกฎ<input value={draft.name} required onChange={(e) => set({ name: e.target.value })} placeholder="เช่น ป้ายกันถอดถูกแกะ" /></label>
          <div className="ac-grid">
            <label>ชนิดเหตุการณ์<select value={draft.event_type} onChange={(e) => set({ event_type: e.target.value })}>{RULE_TYPES.map(([id, l]) => <option key={id} value={id}>{l}</option>)}</select></label>
            <label>ระดับ<select value={draft.severity} onChange={(e) => set({ severity: e.target.value })}>{Object.entries(SEVERITY_LABEL).map(([id, l]) => <option key={id} value={id}>{l}</option>)}</select></label>
          </div>
          {draft.event_type === "zone" && (
            <fieldset className="ac-checks">
              <legend>เตือนเมื่อเข้าโซนของ gateway (ไม่เลือก = ทุกครั้งที่ย้ายโซน)</legend>
              {gateways.map((g) => (
                <label key={g.id}>
                  <input type="checkbox" checked={draft.scope.gateway_ids?.includes(g.id) ?? false} onChange={(e) => scope({ gateway_ids: e.target.checked ? [...(draft.scope.gateway_ids ?? []), g.id] : (draft.scope.gateway_ids ?? []).filter((x) => x !== g.id) })} />
                  {g.name}
                </label>
              ))}
              <small>ใช้กับอุปกรณ์ที่เปิดโหมดใช้หลาย gateway · โซนเปลี่ยนเมื่อสัญญาณเฉลี่ยของ gateway ใหม่แรงกว่า 6 dB ต่อเนื่อง 10 วินาที</small>
            </fieldset>
          )}
          {AFTER_HOURS_TYPES.has(draft.event_type) && (
            <fieldset className="ac-checks ac-hours">
              <legend>ช่วงเวลา</legend>
              <label className="ac-check"><input type="checkbox" checked={!!afterHours} onChange={(e) => scope({ after_hours: e.target.checked ? { ...DEFAULT_AFTER_HOURS, days: [...DEFAULT_AFTER_HOURS.days] } : undefined })} /> เฉพาะนอกเวลาทำการ</label>
              {afterHours ? (
                <>
                  <div className="ac-grid">
                    <label>ตั้งแต่<input type="time" required value={afterHours.from} onChange={(e) => scope({ after_hours: { ...afterHours, from: e.target.value } })} /></label>
                    <label>ถึง<input type="time" required value={afterHours.to} onChange={(e) => scope({ after_hours: { ...afterHours, to: e.target.value } })} /></label>
                  </div>
                  <div className="ac-days" role="group" aria-label="วันที่ใช้กฎ">
                    {WEEKDAYS.map(([d, short, full]) => {
                      const on = afterHours.days.includes(d);
                      return <button key={d} type="button" className={`ac-chip ${on ? "is-on" : ""}`} aria-pressed={on} aria-label={full} title={full} onClick={() => scope({ after_hours: { ...afterHours, days: on ? afterHours.days.filter((x) => x !== d) : [...afterHours.days, d] } })}>{short}</button>;
                    })}
                  </div>
                  <small>{afterHours.from > afterHours.to ? `ข้ามเที่ยงคืน: ${afterHours.from} ของวันที่เลือก ถึง ${afterHours.to} ของวันถัดไป` : `${afterHours.from}–${afterHours.to} ของวันที่เลือก`} · เวลาประเทศไทย{afterHours.days.length === 0 ? " · เลือกอย่างน้อย 1 วัน" : ""}{afterHours.from === afterHours.to ? " · เวลาเริ่มและสิ้นสุดต้องไม่เท่ากัน" : ""}</small>
                </>
              ) : (
                <small>{draft.event_type === "door" ? "แจ้งทุกครั้งที่ประตูเปิด" : "แจ้งทุกครั้งที่ห้องเปลี่ยนจากว่างเป็นมีคน"} · เปิดตัวเลือกนี้เพื่อเตือนเฉพาะช่วงที่ออฟฟิศปิด</small>
              )}
            </fieldset>
          )}
          {draft.event_type === "offline" && <label>ถือว่าขาดการติดต่อหลัง (วินาที, 60–86400)<input type="number" min={60} max={86400} value={draft.scope.offline_after_sec ?? 300} onChange={(e) => scope({ offline_after_sec: Number(e.target.value) })} /></label>}
          {draft.event_type === "threshold" && (
            <div className="ac-grid three">
              <label>ค่า<select value={draft.scope.metric ?? ""} onChange={(e) => scope({ metric: e.target.value })}><option value="">เลือก</option><option value="temperature">temperature (°C)</option><option value="humidity">humidity (%RH)</option><option value="battery">battery (%)</option><option value="rssi">rssi (dBm)</option><option value="accel_g">accel_g (g)</option><option value="voltage">voltage (V, TLM)</option></select></label>
              <label>เงื่อนไข<select value={draft.scope.op ?? ""} onChange={(e) => scope({ op: e.target.value })}><option value="">เลือก</option><option value=">">มากกว่า</option><option value=">=">≥</option><option value="<">น้อยกว่า</option><option value="<=">≤</option></select></label>
              <label>เกณฑ์<input type="number" step="any" value={draft.scope.value ?? ""} onChange={(e) => scope({ value: e.target.value === "" ? undefined : Number(e.target.value) })} /></label>
            </div>
          )}
          <label>เฉพาะอุปกรณ์ (MAC คั่นด้วย , · เว้นว่าง = ทุกอุปกรณ์)<input value={ids} onChange={(e) => setIds(e.target.value)} placeholder="F0:00:00:00:00:09, AC233FC274EB" /></label>
          <fieldset className="ac-channels"><legend>ส่งไปยัง</legend>
            {channels.length === 0 && <span className="ac-note">ยังไม่มีช่องทาง · สร้างได้ที่แท็บ “ช่องทาง” การแจ้งเตือนจะยังแสดงในหน้านี้</span>}
            {channels.map((c) => (
              <label key={c.id} className="ac-check"><input type="checkbox" checked={draft.channels.includes(c.id)} onChange={(e) => set({ channels: e.target.checked ? [...draft.channels, c.id] : draft.channels.filter((x) => x !== c.id) })} /> {c.name} <small>{KIND_LABEL[c.kind]}</small></label>
            ))}
          </fieldset>
          <div className="ac-grid">
            <label>หลังปิดเรื่อง ไม่เปิดซ้ำภายใน (วินาที)<input type="number" min={0} max={86400} value={draft.dedupe_sec} onChange={(e) => set({ dedupe_sec: Number(e.target.value) })} /></label>
            <label className="ac-check inline"><input type="checkbox" checked={draft.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> เปิดใช้กฎนี้</label>
          </div>
          {error && <p className="ac-dialog-error" role="alert">{error}</p>}
          <div className="ac-dialog-actions">
            <button type="button" className="ac-btn" disabled={busy} onClick={onCancel}>ยกเลิก</button>
            <button type="submit" className="ac-btn primary" disabled={busy || !valid}>{busy ? "กำลังบันทึก…" : "บันทึกกฎ"}</button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  );
}
