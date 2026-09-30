"use client";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { BatteryLow, Bell, Building2, CheckCircle2, Cpu, LayoutDashboard, Radio, Siren, Users, Volume2, VolumeX, Wifi, WifiOff } from "lucide-react";
import "../aether.css";
import "./kiosk.css";
import View3D, { type Floor3D } from "../floorplan/view3d";
import { draftOf, type AssetView, type Floor, type Site, type Zone } from "../floorplan/model";
import { WidgetFrame } from "../studio";

/**
 * The wall display (docs/platform/display.md): paired once with a code, then a full-screen rotation of read-only
 * views for a control room. An open SOS or hazard alert takes over the whole screen, with an alarm, until it is
 * acknowledged (here, when the owner allowed it, or anywhere else in Aether).
 */

const API = import.meta.env.VITE_AETHER_API_ORIGIN ?? "";
const TOKEN_KEY = "aether.display.token";
const BOARD_POLL_MS = 15000, BOARD_POLL_LIVE_MS = 60000, SESSION_POLL_MS = 60000, STALE_MS = 60000;

type ViewKind = "overview" | "alerts" | "floorplan" | "devices" | "presence" | "studio";
type PlaylistItem = { kind: ViewKind; seconds: number; ref?: string };
type Session = { id: string; name: string; tenant_name: string; project_ids: string[]; playlist: PlaylistItem[]; show_names: boolean; allow_ack: boolean };
type BoardGateway = { id: string; name: string; model: string; project_id: string | null; last_seen: string | null; online: boolean };
type BoardDevice = { id: string; name: string; external_id: string; profile_id: string; gateway_id: string; wearable: boolean; zone_gateway_id: string | null; last_seen: string | null; online: boolean; kind?: string; reading: Record<string, number> };
type BoardAlert = { id: string; gateway_id: string; external_id: string; device_name: string; event_type: string; severity: string; title: string; status: string; opened_at: string; acked_at: string | null; takeover: boolean; gateway_name: string };
type Presence = { gateway_id: string; gateway_name: string; count: number; names?: string[] };
type Board = {
  server_time: string;
  projects: { id: string; name: string; color: string }[];
  gateways: BoardGateway[];
  devices: BoardDevice[];
  alerts: BoardAlert[];
  counts: { open: number; acknowledged: number; critical: number; gateways: number; gateways_online: number; devices: number; devices_online: number; low_battery: number };
  presence: Presence[];
};
type Rendered = { id: string; html?: string; css?: string; error?: string };
type StudioBoard = { name: string; layout: { id: string; title: string; width: number; height: number }[]; panels: Rendered[] };

class Unpaired extends Error {}

function readToken(): string {
  try { return localStorage.getItem(TOKEN_KEY) ?? ""; } catch { return ""; }
}
function writeToken(token: string) {
  try { if (token) localStorage.setItem(TOKEN_KEY, token); else localStorage.removeItem(TOKEN_KEY); } catch { /* private mode: pair again next time */ }
}

async function kiosk<T>(token: string, path: string, method: "GET" | "POST" = "GET"): Promise<T> {
  const r = await fetch(`${API}/api/v1/kiosk${path}`, { method, headers: { Authorization: `Bearer ${token}` }, cache: "no-store", signal: AbortSignal.timeout(15000) });
  if (r.status === 401) throw new Unpaired();
  if (!r.ok) throw new Error(String(r.status));
  if (r.status === 204) return undefined as T;
  return (await r.json()) as T;
}

const VIEW_LABEL: Record<ViewKind, string> = { overview: "ภาพรวม", alerts: "การแจ้งเตือน", floorplan: "ผังอาคาร", devices: "สถานะอุปกรณ์", presence: "ผู้สวมอุปกรณ์ในแต่ละพื้นที่", studio: "Dashboard" };
const VIEW_ICON: Record<ViewKind, typeof Bell> = { overview: Radio, alerts: Bell, floorplan: Building2, devices: Cpu, presence: Users, studio: LayoutDashboard };
const SEVERITY_LABEL: Record<string, string> = { critical: "วิกฤต", warning: "เฝ้าระวัง", info: "ข้อมูล" };

const two = (n: number) => String(n).padStart(2, "0");
function elapsed(from: string, now: number): string {
  const s = Math.max(0, Math.floor((now - Date.parse(from)) / 1000));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
  return h > 0 ? `${h}:${two(m)}:${two(s % 60)}` : `${m}:${two(s % 60)}`;
}
function ago(from: string | null, now: number): string {
  if (!from) return "ยังไม่เคยได้ยิน";
  const s = Math.max(0, Math.round((now - Date.parse(from)) / 1000));
  if (s < 60) return "เมื่อสักครู่";
  if (s < 3600) return `${Math.round(s / 60)} นาทีที่แล้ว`;
  if (s < 86400) return `${Math.round(s / 3600)} ชั่วโมงที่แล้ว`;
  return `${Math.round(s / 86400)} วันที่แล้ว`;
}
const clock = (t: number) => new Date(t).toLocaleTimeString("th-TH", { hour: "2-digit", minute: "2-digit" });
function summary(d: BoardDevice): string {
  const r = d.reading, parts: string[] = [];
  if (typeof r.temperature === "number") parts.push(`${r.temperature.toFixed(1)} °C`);
  if (typeof r.humidity === "number") parts.push(`${r.humidity.toFixed(0)} %`);
  if (r.leak === 1) parts.push("น้ำรั่ว");
  if (r.tamper === 1) parts.push("ถูกถอด");
  if (typeof r.battery === "number" && r.battery > 0) parts.push(`แบต ${r.battery}%`);
  return parts.join(" · ");
}

/** A siren the TV can play once a person has touched it (browsers block audio before any gesture). */
function useAlarm() {
  const ctx = useRef<AudioContext | null>(null);
  const [unlocked, setUnlocked] = useState(false);
  const unlock = useCallback(() => {
    try {
      ctx.current ??= new AudioContext();
      void ctx.current.resume().then(() => setUnlocked(ctx.current?.state === "running"));
    } catch { /* no audio on this device: the screen is the alarm */ }
  }, []);
  const ring = useCallback(() => {
    const c = ctx.current;
    if (!c || c.state !== "running") return;
    [0, 0.45, 0.9].forEach((at, i) => {
      const osc = c.createOscillator(), gain = c.createGain();
      osc.type = "square";
      osc.frequency.setValueAtTime(i % 2 ? 660 : 990, c.currentTime + at);
      gain.gain.setValueAtTime(0.0001, c.currentTime + at);
      gain.gain.exponentialRampToValueAtTime(0.12, c.currentTime + at + 0.02);
      gain.gain.exponentialRampToValueAtTime(0.0001, c.currentTime + at + 0.38);
      osc.connect(gain); gain.connect(c.destination);
      osc.start(c.currentTime + at); osc.stop(c.currentTime + at + 0.4);
    });
  }, []);
  return { unlocked, unlock, ring };
}

export default function Kiosk() {
  const [token, setToken] = useState("");
  const [ready, setReady] = useState(false);
  const [notice, setNotice] = useState("");
  // The token lives in this browser only (localStorage), read after hydration.
  useEffect(() => { const id = requestAnimationFrame(() => { setToken(readToken()); setReady(true); }); return () => cancelAnimationFrame(id); }, []);
  const unpair = useCallback((why: string) => { writeToken(""); setToken(""); setNotice(why); }, []);
  if (!ready) return <main className="ks ks-pair" />;
  if (!token) return <PairScreen notice={notice} onPaired={(t) => { writeToken(t); setNotice(""); setToken(t); }} />;
  return <Wall key={token} token={token} onUnpaired={() => unpair("จอนี้ถูกยกเลิกหรือถูกจับคู่ใหม่ · ใส่รหัสจับคู่ใหม่จากหน้า “จอแสดงผล” ใน Aether")} />;
}

function PairScreen({ notice, onPaired }: { notice: string; onPaired: (token: string) => void }) {
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = useCallback(async (raw: string) => {
    setBusy(true); setError("");
    try {
      const r = await fetch(`${API}/api/v1/kiosk/pair`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ code: raw }), signal: AbortSignal.timeout(15000) });
      if (r.status === 429) throw new Error("ลองบ่อยเกินไป กรุณารอหนึ่งนาที");
      if (!r.ok) throw new Error("รหัสไม่ถูกต้องหรือหมดอายุแล้ว · ขอรหัสใหม่จากหน้า “จอแสดงผล”");
      const out = (await r.json()) as { token: string };
      onPaired(out.token);
    } catch (e) { setError(e instanceof Error ? e.message : "เชื่อมต่อไม่ได้"); } finally { setBusy(false); }
  }, [onPaired]);
  // A pairing link carries the code in the fragment (never sent to a server); it is used once and removed.
  useEffect(() => {
    let frame = 0;
    const take = () => {
      const m = /code=([A-Za-z0-9-]{8,12})/.exec(location.hash);
      if (!m) return;
      history.replaceState(null, "", location.pathname);
      frame = requestAnimationFrame(() => { setCode(m[1].toUpperCase()); void submit(m[1]); });
    };
    take();
    window.addEventListener("hashchange", take);
    return () => { cancelAnimationFrame(frame); window.removeEventListener("hashchange", take); };
  }, [submit]);
  const format = (v: string) => { const s = v.toUpperCase().replace(/[^A-Z0-9]/g, "").slice(0, 8); return s.length > 4 ? `${s.slice(0, 4)}-${s.slice(4)}` : s; };
  return <main className="ks ks-pair">
    <div className="ks-pair-card">
      <p className="ks-brand"><span className="ks-brand-mark">Λ</span>aether<span className="ks-brand-dot">.</span> <small>จอแสดงผล</small></p>
      <h1>เชื่อมจอนี้กับ Aether</h1>
      <p className="ks-pair-help">ผู้ดูแลสร้างจอในเมนู <strong>จอแสดงผล</strong> แล้วใส่รหัส 8 ตัวที่ได้ (ใช้ได้ครั้งเดียว ภายใน 10 นาที)</p>
      {notice && <p className="ks-pair-notice" role="status">{notice}</p>}
      <form onSubmit={(e) => { e.preventDefault(); void submit(code); }}>
        <input aria-label="รหัสจับคู่" className="ks-code" value={code} onChange={(e) => setCode(format(e.target.value))} placeholder="XXXX-XXXX" autoComplete="off" autoCapitalize="characters" spellCheck={false} autoFocus inputMode="text" />
        <button type="submit" disabled={busy || code.replace("-", "").length !== 8}>{busy ? "กำลังเชื่อม…" : "เชื่อมจอ"}</button>
      </form>
      {error && <p className="ks-pair-error" role="alert">{error}</p>}
      <p className="ks-pair-foot">จอนี้อ่านข้อมูลได้อย่างเดียว · ไม่ต้องเข้าสู่ระบบด้วยบัญชีพนักงาน</p>
    </div>
  </main>;
}

function Wall({ token, onUnpaired }: { token: string; onUnpaired: () => void }) {
  const [session, setSession] = useState<Session | null>(null);
  const [board, setBoard] = useState<Board | null>(null);
  const [lastOk, setLastOk] = useState(0);
  const [live, setLive] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const [site, setSite] = useState<Site | null>(null);
  const [muted, setMuted] = useState(false);
  const [acking, setAcking] = useState("");
  const alarm = useAlarm();
  const unpairedRef = useRef(onUnpaired);
  useEffect(() => { unpairedRef.current = onUnpaired; }, [onUnpaired]);

  const fail = useCallback((e: unknown) => { if (e instanceof Unpaired) unpairedRef.current(); }, []);
  const loadBoard = useCallback(async () => {
    try { const b = await kiosk<Board>(token, "/board"); setBoard(b); setLastOk(Date.now()); } catch (e) { fail(e); }
  }, [token, fail]);
  const loadSession = useCallback(async () => {
    try { const s = await kiosk<{ display: Session }>(token, "/session"); setSession(s.display); } catch (e) { fail(e); }
  }, [token, fail]);

  // Settings and data: polled (slowly while the signal stream is up), refetched at once on a signal.
  const liveRef = useRef(false);
  useEffect(() => { liveRef.current = live; }, [live]);
  useEffect(() => {
    let active = true, timer: ReturnType<typeof setTimeout>;
    const poll = async () => { await loadBoard(); if (active) timer = setTimeout(poll, liveRef.current ? BOARD_POLL_LIVE_MS : BOARD_POLL_MS); };
    void poll();
    return () => { active = false; clearTimeout(timer); };
  }, [loadBoard]);
  useEffect(() => {
    const first = setTimeout(() => void loadSession(), 0);
    const id = setInterval(() => void loadSession(), SESSION_POLL_MS);
    return () => { clearTimeout(first); clearInterval(id); };
  }, [loadSession]);
  useEffect(() => { const id = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(id); }, []);

  // The signal stream: an alert refetches at once; other kinds at most every 3 s.
  useEffect(() => {
    let stopped = false, socket: WebSocket | null = null, retry: ReturnType<typeof setTimeout> | undefined, ping: ReturnType<typeof setInterval> | undefined;
    let delay = 1000, pending: ReturnType<typeof setTimeout> | undefined;
    const soon = () => { pending ??= setTimeout(() => { pending = undefined; void loadBoard(); }, 3000); };
    const connect = () => {
      if (stopped) return;
      const ws = new WebSocket((API || location.origin).replace(/^http/, "ws") + "/ws");
      socket = ws;
      ws.onopen = () => ws.send(JSON.stringify({ type: "auth", token }));
      ws.onmessage = (event) => {
        let msg: { type?: string; kind?: string; error?: string };
        try { msg = JSON.parse(String(event.data)); } catch { return; }
        if (msg.type === "ready") { delay = 1000; setLive(true); clearInterval(ping); ping = setInterval(() => ws.readyState === WebSocket.OPEN && ws.send('{"type":"ping"}'), 30000); }
        else if (msg.type === "signal" && msg.kind === "alert") void loadBoard();
        else if (msg.type === "signal" && msg.kind === "display") { void loadSession(); void loadBoard(); }
        else if (msg.type === "signal") soon();
        else if (msg.type === "error" && msg.error === "unauthorized") { stopped = true; unpairedRef.current(); }
        else if (msg.type === "error" && msg.error === "too_many_connections") delay = 120000;
      };
      ws.onclose = () => {
        setLive(false); clearInterval(ping);
        if (stopped) return;
        retry = setTimeout(connect, delay);
        delay = Math.min(delay * 2, 60000);
      };
    };
    connect();
    return () => { stopped = true; clearTimeout(retry); clearTimeout(pending); clearInterval(ping); socket?.close(); };
  }, [token, loadBoard, loadSession]);

  // The floor plan: the site the playlist names, or the first one the display sees; refreshed every few minutes.
  const siteRef = session?.playlist.find((v) => v.kind === "floorplan")?.ref ?? "";
  useEffect(() => {
    let active = true;
    const load = async () => { try { const out = await kiosk<{ site: Site | null }>(token, `/floorplan${siteRef ? `?site=${siteRef}` : ""}`); if (active) setSite(out.site); } catch (e) { fail(e); } };
    void load();
    const id = setInterval(() => void load(), 180000);
    return () => { active = false; clearInterval(id); };
  }, [token, siteRef, fail]);

  // Rotation: pauses while an emergency holds the screen.
  const playlist = useMemo<PlaylistItem[]>(() => (session?.playlist?.length ? session.playlist : [{ kind: "overview", seconds: 30 }]), [session]);
  const [step, setStep] = useState(0);
  const takeovers = useMemo(() => (board?.alerts ?? []).filter((a) => a.takeover && a.status === "open"), [board]);
  const current = playlist[step % playlist.length];
  // Keyed on values, not objects: re-reading the settings every minute must not restart a longer dwell.
  const dwell = Math.max(10, current.seconds), paused = takeovers.length > 0;
  useEffect(() => {
    if (paused) return;
    const id = setTimeout(() => setStep((s) => (s + 1) % playlist.length), dwell * 1000);
    return () => clearTimeout(id);
  }, [step, current.kind, current.ref, dwell, playlist.length, paused]);

  // The alarm repeats while an emergency is open and the TV was unlocked for sound.
  const emergencyKey = takeovers.map((a) => a.id).join(",");
  const { unlocked: soundReady, ring } = alarm;
  useEffect(() => {
    if (!emergencyKey || muted || !soundReady) return;
    ring();
    const id = setInterval(ring, 2400);
    return () => clearInterval(id);
  }, [emergencyKey, muted, soundReady, ring]);

  // Burn-in protection: the whole picture drifts by a few pixels every two minutes.
  const [shift, setShift] = useState<[number, number]>([0, 0]);
  useEffect(() => { const id = setInterval(() => setShift([Math.round(Math.random() * 8 - 4), Math.round(Math.random() * 8 - 4)]), 120000); return () => clearInterval(id); }, []);

  const acknowledge = async (id: string) => {
    setAcking(id);
    try { await kiosk(token, `/alerts/${id}/ack`, "POST"); await loadBoard(); } catch (e) { fail(e); } finally { setAcking(""); }
  };

  const stale = lastOk > 0 && now - lastOk > STALE_MS;
  const Icon = VIEW_ICON[current.kind];
  const zoneOf = useMemo(() => zoneIndex(site), [site]);
  return <main className="ks ks-wall" style={{ transform: `translate(${shift[0]}px, ${shift[1]}px)` }} onClick={alarm.unlock} onKeyDown={alarm.unlock}>
    <header className="ks-top">
      <p className="ks-brand"><span className="ks-brand-mark">Λ</span>aether<span className="ks-brand-dot">.</span></p>
      <div className="ks-where"><strong>{session?.name ?? "จอแสดงผล"}</strong><small>{session?.tenant_name ?? ""}</small></div>
      <div className="ks-view-title"><Icon aria-hidden="true" /><span>{VIEW_LABEL[current.kind]}</span></div>
      <div className="ks-steps" aria-hidden="true">{playlist.map((v, i) => <span key={i} className={i === step % playlist.length ? "is-on" : ""} style={i === step % playlist.length ? { animationDuration: `${Math.max(10, v.seconds)}s` } : undefined} />)}</div>
      <div className={`ks-conn ${live ? "is-live" : stale ? "is-stale" : ""}`}>{live ? <Wifi aria-hidden="true" /> : <WifiOff aria-hidden="true" />}<span>{live ? "สด" : lastOk ? "กำลังเชื่อมต่อใหม่" : "กำลังเชื่อมต่อ"}</span></div>
      <button type="button" className="ks-sound" onClick={(e) => { e.stopPropagation(); alarm.unlock(); setMuted((m) => !m); }} aria-label={muted ? "เปิดเสียงเตือน" : "ปิดเสียงเตือน"}>{muted || !alarm.unlocked ? <VolumeX aria-hidden="true" /> : <Volume2 aria-hidden="true" />}</button>
      <time className="ks-clock" dateTime={new Date(now).toISOString()}>{new Date(now).toLocaleTimeString("th-TH", { hour: "2-digit", minute: "2-digit", second: "2-digit" })}<small>{new Date(now).toLocaleDateString("th-TH", { weekday: "short", day: "numeric", month: "short" })}</small></time>
    </header>
    {stale && <div className="ks-stale" role="status">ข้อมูลอาจไม่เป็นปัจจุบัน · อัปเดตล่าสุด {clock(lastOk)} · กำลังเชื่อมต่อใหม่อัตโนมัติ</div>}
    {!alarm.unlocked && <div className="ks-unlock" role="note">แตะหน้าจอหนึ่งครั้งเพื่อเปิดเสียงเตือนฉุกเฉิน</div>}
    <section key={`${step}-${current.kind}`} className="ks-stage" aria-live="polite">
      {!board ? <p className="ks-loading">กำลังโหลดข้อมูล…</p>
        : current.kind === "overview" ? <OverviewView board={board} now={now} />
        : current.kind === "alerts" ? <AlertsView board={board} now={now} />
        : current.kind === "floorplan" ? <FloorplanView site={site} board={board} takeovers={takeovers} />
        : current.kind === "devices" ? <DevicesView board={board} now={now} />
        : current.kind === "presence" ? <PresenceView board={board} zoneOf={zoneOf} showNames={!!session?.show_names} />
        : <StudioView token={token} dashboard={current.ref ?? ""} onFail={fail} />}
    </section>
    {takeovers.length > 0 && <Takeover alert={takeovers[0]} others={takeovers.length - 1} now={now} site={site} zoneOf={zoneOf} canAck={!!session?.allow_ack} acking={acking === takeovers[0].id} onAck={() => void acknowledge(takeovers[0].id)} soundOff={muted || !alarm.unlocked} />}
  </main>;
}

type ZoneHit = { zone: Zone; floor: Floor };
/** Zones by the gateway that covers them (a zone lists its gateways), for "where" on an emergency. */
function zoneIndex(site: Site | null): Map<string, ZoneHit> {
  const out = new Map<string, ZoneHit>();
  for (const floor of site?.floors ?? []) for (const zone of floor.layout?.zones ?? []) for (const g of zone.gateway_ids ?? []) if (!out.has(g)) out.set(g, { zone, floor });
  return out;
}

function OverviewView({ board, now }: { board: Board; now: number }) {
  const c = board.counts;
  const env = board.devices.filter((d) => typeof d.reading.temperature === "number").slice(0, 12);
  return <div className="ks-overview">
    <div className="ks-kpis">
      <Kpi label="แจ้งเตือนเปิดอยู่" value={c.open} tone={c.critical > 0 ? "danger" : c.open > 0 ? "warn" : "ok"} sub={c.critical > 0 ? `วิกฤต ${c.critical}` : c.acknowledged > 0 ? `รับทราบแล้ว ${c.acknowledged}` : "ไม่มีเรื่องค้าง"} />
      <Kpi label="Gateway ออนไลน์" value={`${c.gateways_online}/${c.gateways}`} tone={c.gateways_online < c.gateways ? "warn" : "ok"} sub={c.gateways - c.gateways_online > 0 ? `ขาดการติดต่อ ${c.gateways - c.gateways_online}` : "ครบทุกตัว"} />
      <Kpi label="อุปกรณ์ออนไลน์" value={`${c.devices_online}/${c.devices}`} tone={c.devices_online < c.devices ? "warn" : "ok"} sub="ได้ยินภายใน 10 นาที" />
      <Kpi label="แบตเตอรี่ต่ำ" value={c.low_battery} tone={c.low_battery > 0 ? "warn" : "ok"} sub="ต่ำกว่า 20%" />
    </div>
    {env.length > 0 ? <div className="ks-env">{env.map((d) => <article key={d.id} className={`ks-env-card ${d.online ? "" : "is-off"}`}>
      <p>{d.name}</p>
      <strong>{d.reading.temperature.toFixed(1)}<span>°C</span></strong>
      <small>{typeof d.reading.humidity === "number" ? `ความชื้น ${d.reading.humidity.toFixed(0)}%` : ""}{d.online ? "" : ` · ${ago(d.last_seen, now)}`}</small>
    </article>)}</div> : <p className="ks-empty">ยังไม่มีเซนเซอร์อุณหภูมิในพื้นที่ของจอนี้</p>}
  </div>;
}

function Kpi({ label, value, sub, tone }: { label: string; value: number | string; sub: string; tone: "ok" | "warn" | "danger" }) {
  return <div className={`ks-kpi is-${tone}`}><p>{label}</p><strong>{value}</strong><small>{sub}</small></div>;
}

function AlertsView({ board, now }: { board: Board; now: number }) {
  if (board.alerts.length === 0) return <div className="ks-allclear"><CheckCircle2 aria-hidden="true" /><h2>ไม่มีการแจ้งเตือนค้าง</h2><p>ทุกอย่างปกติ · อัปเดต {clock(Date.parse(board.server_time))}</p></div>;
  return <ul className="ks-alerts">{board.alerts.slice(0, 8).map((a) => <li key={a.id} className={`is-${a.severity} ${a.status === "acknowledged" ? "is-acked" : ""}`}>
    <span className="ks-sev">{SEVERITY_LABEL[a.severity] ?? a.severity}</span>
    <div><strong>{a.title}</strong><small>{a.device_name} · {a.gateway_name || "ไม่ทราบ gateway"}</small></div>
    <span className="ks-alert-time">{a.status === "acknowledged" ? "รับทราบแล้ว" : `เปิดมา ${elapsed(a.opened_at, now)}`}</span>
  </li>)}{board.alerts.length > 8 && <li className="ks-more">และอีก {board.alerts.length - 8} รายการ</li>}</ul>;
}

function FloorplanView({ site, board, takeovers }: { site: Site | null; board: Board; takeovers: BoardAlert[] }) {
  const floors = useMemo<Floor3D[]>(() => (site?.floors ?? []).map((f) => ({ id: f.id, draft: draftOf(f) })), [site]);
  const sos = useMemo(() => new Set(takeovers.map((a) => a.external_id)), [takeovers]);
  const assets = useMemo<AssetView[]>(() => [
    ...board.gateways.map((g): AssetView => ({ kind: "gateway", id: g.id, name: g.name, model: g.model, online: g.online, wearable: false, zoneGatewayId: null, summary: "", alert: false, sos: false })),
    ...board.devices.map((d): AssetView => ({ kind: "device", id: d.id, name: d.name, model: "", external: d.external_id, online: d.online, wearable: d.wearable, zoneGatewayId: d.zone_gateway_id, summary: sos.has(d.external_id) ? "กดปุ่มฉุกเฉิน" : summary(d), alert: sos.has(d.external_id) || d.reading.tamper === 1 || d.reading.leak === 1, sos: sos.has(d.external_id), profileId: d.profile_id })),
  ], [board, sos]);
  // The floor with an emergency on it comes to the front.
  const active = useMemo(() => {
    for (const f of site?.floors ?? []) for (const p of f.placements ?? []) if (assets.some((a) => a.sos && a.id === p.asset_id)) return f.id;
    return site?.floors?.[0]?.id ?? "";
  }, [site, assets]);
  if (!site || floors.length === 0) return <p className="ks-empty">ยังไม่มีผังอาคารที่จอนี้มองเห็น · วาดผังได้ที่เมนู “ผังอาคาร”</p>;
  return <div className="ks-floorplan">
    <div className="ks-fp-3d"><View3D floors={floors} activeId={active} assets={assets} selection={null} explode={0.6} isolate={false} snap={0} readOnly resetKey={0} onSelect={() => {}} onPickFloor={() => {}} onMove={() => {}} autoRotate /></div>
    <aside className="ks-fp-side"><h2>{site.name}</h2><ul>{site.floors.map((f) => <li key={f.id} className={f.id === active ? "is-on" : ""}>{f.name}<small>ชั้น {f.level}</small></li>)}</ul>
      <p className="ks-legend"><span className="is-ok" />ออนไลน์ <span className="is-off" />ขาดการติดต่อ <span className="is-danger" />เหตุฉุกเฉิน</p></aside>
  </div>;
}

function DevicesView({ board, now }: { board: Board; now: number }) {
  const offline = board.devices.filter((d) => !d.online).slice(0, 10);
  const low = board.devices.filter((d) => typeof d.reading.battery === "number" && d.reading.battery > 0 && d.reading.battery <= 20).slice(0, 6);
  return <div className="ks-devices">
    <div className="ks-gw">{board.gateways.map((g) => <span key={g.id} className={g.online ? "is-on" : "is-off"}><Radio aria-hidden="true" />{g.name}</span>)}</div>
    <div className="ks-dev-cols">
      <section><h2>ขาดการติดต่อ <b>{board.devices.filter((d) => !d.online).length}</b></h2>{offline.length === 0 ? <p className="ks-good"><CheckCircle2 aria-hidden="true" />อุปกรณ์ทุกตัวส่งข้อมูลปกติ</p> : <ul>{offline.map((d) => <li key={d.id}><strong>{d.name}</strong><small>{ago(d.last_seen, now)}</small></li>)}</ul>}</section>
      <section><h2>แบตเตอรี่ต่ำ <b>{board.counts.low_battery}</b></h2>{low.length === 0 ? <p className="ks-good"><CheckCircle2 aria-hidden="true" />ไม่มีอุปกรณ์แบตต่ำ</p> : <ul>{low.map((d) => <li key={d.id}><strong>{d.name}</strong><small><BatteryLow aria-hidden="true" /> {d.reading.battery}%</small></li>)}</ul>}</section>
    </div>
  </div>;
}

function PresenceView({ board, zoneOf, showNames }: { board: Board; zoneOf: Map<string, ZoneHit>; showNames: boolean }) {
  const total = board.presence.reduce((n, p) => n + p.count, 0);
  if (total === 0) return <p className="ks-empty">ขณะนี้ไม่มีผู้สวมอุปกรณ์ในพื้นที่ของจอนี้</p>;
  return <div className="ks-presence"><p className="ks-presence-total"><Users aria-hidden="true" />อยู่ในพื้นที่ <strong>{total}</strong> คน</p>
    <div className="ks-zones">{board.presence.map((p) => <article key={p.gateway_id}><p>{zoneOf.get(p.gateway_id)?.zone.name ?? p.gateway_name}</p><strong>{p.count}</strong>{showNames && p.names && <small>{p.names.slice(0, 6).join(" · ")}{p.names.length > 6 ? ` และอีก ${p.names.length - 6}` : ""}</small>}</article>)}</div>
  </div>;
}

function StudioView({ token, dashboard, onFail }: { token: string; dashboard: string; onFail: (e: unknown) => void }) {
  const [data, setData] = useState<StudioBoard | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    if (!dashboard) return;
    let active = true;
    const load = async () => { try { const out = await kiosk<StudioBoard>(token, `/studio/${dashboard}`); if (active) { setData(out); setFailed(false); } } catch (e) { onFail(e); if (active) setFailed(true); } };
    void load();
    const id = setInterval(() => void load(), 30000);
    return () => { active = false; clearInterval(id); };
  }, [token, dashboard, onFail]);
  if (failed && !data) return <p className="ks-empty">เปิด Dashboard นี้ไม่ได้ · อาจถูกลบหรือไม่ได้อยู่ในรายการของจอ</p>;
  if (!data) return <p className="ks-loading">กำลังโหลด Dashboard…</p>;
  return <div className="ks-studio"><h2>{data.name}</h2><div className="ks-studio-grid">{data.layout.map((p) => { const r = data.panels.find((x) => x.id === p.id); return <article key={p.id} style={{ gridColumn: `span ${Math.min(4, Math.max(1, p.width))}`, gridRow: `span ${Math.min(4, Math.max(1, p.height))}` }}><header>{p.title}</header>{r?.error ? <p className="ks-empty">{r.error}</p> : r?.html ? <WidgetFrame html={r.html} css={r.css ?? ""} title={p.title} /> : <p className="ks-loading">…</p>}</article>; })}</div></div>;
}

function Takeover({ alert, others, now, site, zoneOf, canAck, acking, onAck, soundOff }: { alert: BoardAlert; others: number; now: number; site: Site | null; zoneOf: Map<string, ZoneHit>; canAck: boolean; acking: boolean; onAck: () => void; soundOff: boolean }) {
  const hit = zoneOf.get(alert.gateway_id);
  const hazard = alert.event_type === "hazard";
  return <div className="ks-takeover" role="alertdialog" aria-live="assertive" aria-label={hazard ? "ตรวจพบอันตราย" : "เหตุฉุกเฉิน"}>
    <div className="ks-to-main">
      <p className="ks-to-kicker"><Siren aria-hidden="true" />{hazard ? "ตรวจพบอันตราย" : "SOS · ขอความช่วยเหลือ"}</p>
      <h1>{alert.title}</h1>
      <p className="ks-to-who">{alert.device_name}</p>
      <dl>
        <div><dt>ตำแหน่ง</dt><dd>{hit ? `${hit.zone.name} · ${hit.floor.name}` : alert.gateway_name || "ไม่ทราบตำแหน่ง"}</dd></div>
        <div><dt>เวลาผ่านไป</dt><dd className="ks-to-timer">{elapsed(alert.opened_at, now)}</dd></div>
        <div><dt>เริ่มเมื่อ</dt><dd>{clock(Date.parse(alert.opened_at))}</dd></div>
      </dl>
      {canAck ? <button type="button" className="ks-to-ack" onClick={onAck} disabled={acking}>{acking ? "กำลังรับทราบ…" : "รับทราบ · กำลังไปช่วย"}</button> : <p className="ks-to-note">รับทราบได้จากแอป Aether · หน้าจอนี้จะกลับสู่ปกติเมื่อมีผู้รับทราบ</p>}
      {others > 0 && <p className="ks-to-more">มีเหตุฉุกเฉินอื่นค้างอยู่อีก {others} รายการ</p>}
      {soundOff && <p className="ks-to-sound">เสียงเตือนปิดอยู่ · แตะหน้าจอเพื่อเปิดเสียง</p>}
    </div>
    {hit && <MiniPlan floor={hit.floor} zone={hit.zone} />}
    {!hit && site && <p className="ks-to-noplan">ยังไม่ได้ผูก gateway นี้กับโซนในผังอาคาร</p>}
  </div>;
}

/** A plain 2D drawing of one floor with the emergency's zone lit up. */
function MiniPlan({ floor, zone }: { floor: Floor; zone: Zone }) {
  const w = floor.width_m, h = floor.depth_m, pad = Math.max(w, h) * 0.04;
  const poly = (pts: [number, number][]) => pts.map((p) => p.join(",")).join(" ");
  const cx = zone.points.reduce((s, p) => s + p[0], 0) / zone.points.length, cy = zone.points.reduce((s, p) => s + p[1], 0) / zone.points.length;
  return <figure className="ks-miniplan">
    <svg viewBox={`${-pad} ${-pad} ${w + pad * 2} ${h + pad * 2}`} role="img" aria-label={`ผัง ${floor.name}`}>
      <rect x={0} y={0} width={w} height={h} className="ks-mp-floor" />
      {(floor.layout.zones ?? []).map((z) => <polygon key={z.id} points={poly(z.points)} className={z.id === zone.id ? "ks-mp-hit" : "ks-mp-zone"} />)}
      {(floor.layout.walls ?? []).map((wl) => <polyline key={wl.id} points={poly(wl.closed ? [...wl.points, wl.points[0]] : wl.points)} className="ks-mp-wall" style={{ strokeWidth: Math.max(0.12, wl.thickness) }} />)}
      <circle cx={cx} cy={cy} r={Math.max(w, h) * 0.025} className="ks-mp-dot" />
      <circle cx={cx} cy={cy} r={Math.max(w, h) * 0.025} className="ks-mp-ring" />
    </svg>
    <figcaption>{zone.name} · {floor.name}</figcaption>
  </figure>;
}
