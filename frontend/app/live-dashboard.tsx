"use client";

import { useEffect, useRef, useState } from "react";
import { Activity, ArrowUpRight, Building2, ClipboardList, LayoutDashboard, Users, Workflow, Bell, PanelLeftClose, PanelLeftOpen, LogOut, Radio, Server, ShieldCheck, Siren, Volume2, VolumeX, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import "./aether.css";
import "./live.css";
import Studio from "./studio";
import DeviceTopology from "./topology/topology";
import AlertsCenter from "./alerts/alerts";
import FloorPlanStudio from "./floorplan/floorplan";
import AssetsPage from "./assets/assets";
import AutomationStudio from "./automation/automation";
import Overview from "./live/overview";
import TeamPage, { type Me } from "./team/team";
import { useLatest } from "./topology/use-latest";
import { useSignals } from "./topology/use-signals";

type Reading = { received_at: string; temperature: number; humidity: number; battery: number; rssi: number | null };
type Sensor = { kind?: string; model?: string; template_id?: string | null; thresholds?: { temperature_high: number | null; humidity_high: number | null }; id: string; name: string; latest: Reading; history: Reading[] };
type Gateway = { gateway: { id: string; name: string; model: string }; last_packet_at: string | null; packet_count: number; observation_count: number; nearby_devices: number; sensors: Sensor[] };
type Snapshot = { gateways: Gateway[]; server_time: string; deployment_mode: string };
const API = import.meta.env.VITE_AETHER_API_ORIGIN ?? "";
type View = "live" | "studio" | "connect" | "floorplan" | "assets" | "alerts" | "automation" | "team";
const NAV_GROUPS = ["MONITOR", "BUILD", "OPERATE"] as const;
// workspace=true: full-height tools that get the thin shell bar.
const VIEWS: { id: View; label: string; group: (typeof NAV_GROUPS)[number]; icon: typeof Activity; workspace?: boolean }[] = [
  { id: "live", label: "ภาพรวม", group: "MONITOR", icon: Activity, workspace: true },
  { id: "alerts", label: "การแจ้งเตือน", group: "MONITOR", icon: Bell },
  { id: "connect", label: "เชื่อมต่ออุปกรณ์", group: "BUILD", icon: Radio, workspace: true },
  { id: "floorplan", label: "ผังอาคาร", group: "BUILD", icon: Building2, workspace: true },
  { id: "studio", label: "Dashboard Studio", group: "BUILD", icon: LayoutDashboard },
  { id: "automation", label: "Automation Studio", group: "BUILD", icon: Workflow, workspace: true },
  { id: "team", label: "ทีมและสิทธิ์", group: "OPERATE", icon: Users, workspace: true },
  { id: "assets", label: "อุปกรณ์ทั้งหมด", group: "OPERATE", icon: ClipboardList, workspace: true },
];
// `sos` is computed by the API (domain.Alert.SOS: a critical alert raised by a `button` event), so the
// browser never re-derives what counts as an emergency.
type BannerAlert = { id: string; title: string; severity: string; device_name: string; external_id: string; gateway_id?: string; opened_at: string; sos?: boolean };
const SOS_BEEP_MS = 4000, SOS_BARS = 3;
function since(at: string, now: number): string {
  const s = Math.max(0, Math.round((now - Date.parse(at)) / 1000));
  if (s < 60) return `${s} วินาที`;
  if (s < 3600) return `${Math.round(s / 60)} นาที`;
  return `${Math.round(s / 3600)} ชั่วโมง`;
}
// Two short tones. Browsers may refuse audio before the first click; the banner still shows.
function beep() {
  try {
    const ctx = new AudioContext();
    [0, 0.22].forEach((at) => {
      const osc = ctx.createOscillator(), gain = ctx.createGain();
      osc.frequency.value = 880; gain.gain.value = 0.08;
      osc.connect(gain); gain.connect(ctx.destination);
      osc.start(ctx.currentTime + at); osc.stop(ctx.currentTime + at + 0.15);
    });
    setTimeout(() => void ctx.close(), 800);
  } catch { /* sound is optional */ }
}
export default function LiveDashboard() {
  const token = useRef("");
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  useEffect(() => { const id = requestAnimationFrame(() => { try { setSidebarCollapsed(localStorage.getItem("aether.sidebar.collapsed") === "true"); } catch {} }); return () => cancelAnimationFrame(id); }, []);
  const toggleSidebar = () => { setSidebarCollapsed((previous) => { const next = !previous; try { localStorage.setItem("aether.sidebar.collapsed", String(next)); } catch {} return next; }); };

  const [me, setMe] = useState<Me | null>(null);

  // One view at a time. Deep links: ?studio ?connect ?alerts ?floorplan ?assets ?automation
  const [view,setView]=useState<View>("live");
 const [requestedSource,setRequestedSource]=useState("");
  const [openAlerts,setOpenAlerts]=useState(0);
  const [banner,setBanner]=useState<BannerAlert|null>(null),[sound,setSound]=useState(()=>{try{return typeof localStorage==="undefined"||localStorage.getItem("aether.alert.sound")!=="off";}catch{return true;}});
  const soundRef=useLatest(sound);
  // Every open SOS, not only the newest one: an emergency may not be replaced by the next emergency.
  const [sosAlerts,setSosAlerts]=useState<BannerAlert[]>([]);
  const [nowMs,setNowMs]=useState(()=>Date.now());
  const sosKey=sosAlerts.map(a=>a.id).join(",");
  // Deep link: read after hydration (the server always renders the overview), in a frame callback.
  useEffect(()=>{const id=requestAnimationFrame(()=>{const q=new URLSearchParams(location.search);setView(VIEWS.find(v=>v.id!=="live"&&q.has(v.id))?.id??"live");});return()=>cancelAnimationFrame(id);},[]);
  const [signedIn, setSignedIn] = useState(false), [starting, setStarting] = useState(true);
  useEffect(() => { if (!signedIn) return; let active = true; const load = async () => { try { const r = await fetch(`${API}/api/v1/me`, {headers:{Authorization:`Bearer ${token.current}`}}); if(r.ok && active) setMe(await r.json()); } catch {} }; void load(); const timer=setInterval(load,30000); return () => { active=false; clearInterval(timer); }; }, [signedIn]);
  const [email, setEmail] = useState(""), [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false), [error, setError] = useState("");
  // An initial password handed over in person must be replaced before anything else is shown.
  const [mustChange, setMustChange] = useState(false), [currentPassword, setCurrentPassword] = useState(""), [newPassword, setNewPassword] = useState(""), [repeatPassword, setRepeatPassword] = useState("");
  const [data, setData] = useState<Snapshot | null>(null);
  const refreshFlight = useRef<Promise<boolean> | null>(null);
  async function refresh() {
    if (refreshFlight.current) return refreshFlight.current;
    const task = (async () => {
      const r = await fetch(`${API}/api/v1/auth/refresh`, { method: "POST", credentials: "include", signal: AbortSignal.timeout(10000) });
      if (!r.ok) { token.current = ""; return false; }
      const auth: unknown = await r.json();
      if (!auth || typeof auth !== "object" || !("access_token" in auth) || typeof auth.access_token !== "string") throw new Error("รูปแบบการเข้าสู่ระบบไม่ถูกต้อง");
      token.current = auth.access_token; setMustChange("must_change_password" in auth && auth.must_change_password === true); return true;
    })();
    refreshFlight.current = task;
    try { return await task; } finally { refreshFlight.current = null; }
  }
  useEffect(() => {
    let active = true;
    refresh().then(ok => { if (active) setSignedIn(ok); }).catch(() => { if (active) setError("เชื่อมต่อ Aether API ไม่ได้ โปรดตรวจว่าบริการ backend เปิดอยู่"); }).finally(() => { if (active) setStarting(false); });
    return () => { active = false; };
  }, []);
  useEffect(() => {
    if (!signedIn) return;
    let active = true, timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        let r = await fetch(`${API}/api/v1/live?range=1h`, { headers: { Authorization: `Bearer ${token.current}` }, cache: "no-store", signal: AbortSignal.timeout(10000) });
        if (r.status === 401 && await refresh()) r = await fetch(`${API}/api/v1/live?range=1h`, { headers: { Authorization: `Bearer ${token.current}` }, cache: "no-store", signal: AbortSignal.timeout(10000) });
        if (r.status === 401) { if (active) { setSignedIn(false); setData(null); setError("เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง"); } return; }
        if (!r.ok) throw new Error(r.status === 403 ? "บัญชีนี้ไม่มีสิทธิ์ดูข้อมูล gateway" : "รับข้อมูลจาก backend ไม่สำเร็จ");
        const snapshot: Snapshot = await r.json();
        if (active) { setData(snapshot); setError(""); }
      } catch (e) { if (active) setError(e instanceof Error ? e.message : "เชื่อมต่อไม่ได้"); }
      finally { if (active) timer = setTimeout(poll, view === "studio" ? 5000 : 60000); } // only Studio's device picker needs it fresh
    }
    void poll(); return () => { active = false; clearTimeout(timer); };
  }, [signedIn, view]);
  // Open-alert badge + banner. The WebSocket says "alerts changed"; the numbers come from the REST API.
  const signalHandlers = useLatest({ getToken: () => token.current, refresh });
  const seenAlerts = useRef<Set<string> | null>(null);
  const checkAlertsRef = useRef<() => void>(() => {});
  const pushConnected = useSignals(signalHandlers, (kind) => { if (kind === "alert") checkAlertsRef.current(); }, signedIn);
  const liveRef = useLatest(pushConnected);
  useEffect(() => {
    if (!signedIn) return;
    let active = true, timer: ReturnType<typeof setTimeout>, flight = false, queued = false;
    seenAlerts.current = null; // a new sign-in starts from a clean slate
    const get = (path: string) => fetch(`${API}/api/v1${path}`, { headers: { Authorization: `Bearer ${token.current}` }, cache: "no-store", signal: AbortSignal.timeout(10000) });
    async function check() {
      if (flight) { queued = true; return; } // a signal that lands mid-request must not be lost
      flight = true;
      try {
        const [sr, ar] = await Promise.all([get("/alerts/summary"), get("/alerts?status=open&limit=20")]);
        if (!active) return;
        if (sr.ok) setOpenAlerts(((await sr.json()) as { open: number }).open);
        if (ar.ok) {
          const items = ((await ar.json()) as { items: BannerAlert[] }).items;
          const first = seenAlerts.current === null;
          const seen = seenAlerts.current ?? new Set<string>();
          const fresh = items.filter(a => !seen.has(a.id));
          items.forEach(a => seen.add(a.id));
          if (seen.size > 500) { seen.clear(); items.forEach(a => seen.add(a.id)); }
          seenAlerts.current = seen;
          // An SOS is a state, not news: it stays on screen for as long as the alert is open, even if it
          // was already open when this tab was opened. Acknowledging it removes it from ?status=open.
          setSosAlerts(items.filter(a => a.sos)); setNowMs(Date.now());
          // Ordinary critical alerts keep the old one-shot banner; an SOS has its own bar.
          const critical = first ? undefined : fresh.find(a => a.severity === "critical" && !a.sos);
          if (critical) { setBanner(critical); if (soundRef.current) beep(); }
        }
      } catch { /* badge is best-effort */ }
      finally { flight = false; if (queued && active) { queued = false; void check(); } }
    }
    checkAlertsRef.current = () => void check();
    async function poll() { await check(); if (active) timer = setTimeout(poll, liveRef.current ? 60000 : 15000); }
    void poll(); return () => { active = false; clearTimeout(timer); checkAlertsRef.current = () => {}; };
  }, [signedIn, liveRef, soundRef]);
  // A panic button keeps making noise until somebody accepts it. The mute toggle still wins, and the
  // browser may refuse audio before the first click — the bar itself is the guarantee, the sound is help.
  useEffect(() => {
    if (!signedIn || !sosKey || !sound) return;
    beep();
    const id = setInterval(beep, SOS_BEEP_MS);
    return () => clearInterval(id);
  }, [signedIn, sosKey, sound]);
  // "กดเมื่อ 2 นาที" has to keep counting while nobody responds.
  useEffect(() => {
    if (!sosKey) return;
    const id = setInterval(() => setNowMs(Date.now()), 5000);
    return () => clearInterval(id);
  }, [sosKey]);
  async function acknowledge(id: string) {
    const call = () => fetch(`${API}/api/v1/alerts/${id}/ack`, { method: "POST", headers: { Authorization: `Bearer ${token.current}` }, signal: AbortSignal.timeout(10000) });
    try {
      let r = await call();
      if (r.status === 401 && await refresh()) r = await call();
      if (r.ok) setSosAlerts(list => list.filter(a => a.id !== id)); // the next poll confirms it
      else if (r.status === 403) setError("บัญชีนี้ไม่มีสิทธิ์รับทราบการแจ้งเตือน");
    } catch { setError("รับทราบไม่สำเร็จ กรุณาลองอีกครั้ง"); }
    checkAlertsRef.current();
  }
  async function login(e: React.FormEvent) {
    e.preventDefault(); setBusy(true); setError("");
    try {
      const r = await fetch(`${API}/api/v1/auth/login`, { method: "POST", credentials: "include", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ email, password }), signal: AbortSignal.timeout(15000) });
      if (!r.ok) throw new Error(r.status === 429 ? "ลองเข้าสู่ระบบบ่อยเกินไป กรุณารอสักครู่" : "เข้าสู่ระบบไม่สำเร็จ ตรวจอีเมลและรหัสผ่าน");
      const auth: unknown = await r.json();
      if (!auth || typeof auth !== "object" || !("access_token" in auth) || typeof auth.access_token !== "string") throw new Error("รูปแบบการเข้าสู่ระบบไม่ถูกต้อง");
      token.current = auth.access_token; setMustChange("must_change_password" in auth && auth.must_change_password === true); setCurrentPassword(password); setPassword(""); setSignedIn(true);
    } catch (e) { setError(e instanceof Error ? e.message : "เชื่อมต่อไม่ได้"); } finally { setBusy(false); }
  }
  async function changePassword(e: React.FormEvent) {
    e.preventDefault();
    if (newPassword !== repeatPassword) { setError("รหัสผ่านใหม่สองช่องไม่ตรงกัน"); return; }
    if (new TextEncoder().encode(newPassword).length < 12) { setError("รหัสผ่านใหม่ต้องยาวอย่างน้อย 12 ตัวอักษร"); return; }
    setBusy(true); setError("");
    try {
      const r = await fetch(`${API}/api/v1/auth/password`, { method: "POST", credentials: "include", headers: { "Content-Type": "application/json", Authorization: `Bearer ${token.current}` }, body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }), signal: AbortSignal.timeout(20000) });
      if (!r.ok) throw new Error(r.status === 429 ? "ลองบ่อยเกินไป กรุณารอสักครู่" : r.status === 400 ? "รหัสผ่านใหม่ไม่ผ่านเกณฑ์ (12–128 ตัวอักษร และต้องไม่ซ้ำรหัสเดิม)" : "เปลี่ยนรหัสผ่านไม่สำเร็จ ตรวจรหัสผ่านปัจจุบัน");
      setMustChange(false); setCurrentPassword(""); setNewPassword(""); setRepeatPassword("");
    } catch (e) { setError(e instanceof Error ? e.message : "เชื่อมต่อไม่ได้"); } finally { setBusy(false); }
  }
  async function logout() {
    setBusy(true);
    try {
      const r = await fetch(`${API}/api/v1/auth/logout`, { method: "POST", credentials: "include", headers: { Authorization: `Bearer ${token.current}` }, signal: AbortSignal.timeout(10000) });
      if (!r.ok && r.status !== 401) throw new Error("ออกจากระบบไม่สำเร็จ กรุณาลองอีกครั้ง");
      token.current = ""; setSignedIn(false); setData(null); setError(""); setMustChange(false); setCurrentPassword(""); setSosAlerts([]);
    } catch (e) { setError(e instanceof Error ? e.message : "เชื่อมต่อไม่ได้"); } finally { setBusy(false); }
  }
  // The shell only needs the sensor list for Dashboard Studio's device picker; the overview loads its own data.
  const studioSources = data?.gateways.flatMap(g => g.sensors.map(s => ({ key: `${g.gateway.id}/${s.id}`, id: s.id, name: s.name, gatewayID: g.gateway.id, gatewayName: g.gateway.name, kind: s.kind ?? "environment", model: s.model }))) ?? [];
  return <div className={`live-shell ${sidebarCollapsed ? "is-sidebar-collapsed" : ""}`}>
    <aside id="workspace-navigation" className="live-sidebar"><button type="button" className="brand live-brand" onClick={()=>setView("live")} aria-label="Aether · ไปหน้าภาพรวม"><span className="brand-symbol">Λ</span>aether<span className="brand-period">.</span></button><div className="live-workspace"><Server size={19} /><div><strong>Aether workspace</strong><small>{data?.deployment_mode === "cloud" ? "Cloud" : "On-premise"}</small></div></div>{NAV_GROUPS.map(group=><div key={group} className="live-nav-group"><p className="live-nav-label">{group}</p>{VIEWS.filter(v=>v.group===group && (!signedIn || (me?.permissions?.[v.id] !== "none" && (v.id !== "team" || (me?.role === "owner" || (me?.role === "admin" && me.project_ids === null)))))).map(v=><button key={v.id} onClick={()=>setView(v.id)} className={`live-nav ${view===v.id?"active":""}`} aria-current={view===v.id?"page":undefined}><v.icon size={18}/>{v.label}{v.id==="alerts"&&openAlerts>0&&<span className="live-nav-badge" aria-label={`${openAlerts} การแจ้งเตือนเปิดอยู่`}>{openAlerts}</span>}</button>)}</div>)}<button type="button" className="live-side-foot" onClick={()=>setView("team")} disabled={!signedIn} aria-label="บัญชีและการตั้งค่า"><ShieldCheck size={18} /><span>{signedIn ? me?.name || "บัญชีผู้ใช้" : "บัญชีผู้ใช้"}<br /><small>{signedIn ? me?.role ?? "กำลังโหลด" : "ยังไม่เข้าสู่ระบบ"} · ตั้งค่า</small></span></button></aside>
    <main className={`live-main ${signedIn&&VIEWS.find(v=>v.id===view)?.workspace?"is-workspace":""}`}>{signedIn&&sosAlerts.length>0&&<div className="live-sos-stack" role="alert" aria-live="assertive">{sosAlerts.slice(0,SOS_BARS).map(a=><div key={a.id} className="live-sos-bar"><Siren size={22} aria-hidden="true"/><div className="live-sos-text"><strong>SOS · {a.device_name||a.external_id}</strong><small>{data?.gateways.find(g=>g.gateway.id===a.gateway_id)?.gateway.name??a.external_id} · กดเมื่อ {since(a.opened_at,nowMs)} ที่แล้ว</small></div><button type="button" className="live-sos-ack" onClick={()=>void acknowledge(a.id)}>รับทราบ</button><button type="button" onClick={()=>setView("alerts")}>ดูรายละเอียด</button><button type="button" aria-pressed={sound} title={sound?"ปิดเสียงเตือน":"เปิดเสียงเตือน"} aria-label={sound?"ปิดเสียงเตือน":"เปิดเสียงเตือน"} onClick={()=>{const on=!sound;setSound(on);try{localStorage.setItem("aether.alert.sound",on?"on":"off");}catch{}}}>{sound?<Volume2 size={16}/>:<VolumeX size={16}/>}</button></div>)}{sosAlerts.length>SOS_BARS&&<button type="button" className="live-sos-more" onClick={()=>setView("alerts")}>และอีก {sosAlerts.length-SOS_BARS} รายการที่ยังไม่รับทราบ · ดูทั้งหมด</button>}</div>}{banner&&signedIn&&<div className="live-alert-banner" role="alert"><Bell size={18}/><div><strong>{banner.title}</strong><small>{banner.device_name||banner.external_id} · {new Date(banner.opened_at).toLocaleTimeString()}</small></div><button type="button" onClick={()=>{setBanner(null);setView("alerts");}}>ดูการแจ้งเตือน</button><button type="button" aria-pressed={sound} title={sound?"ปิดเสียงเตือน":"เปิดเสียงเตือน"} aria-label={sound?"ปิดเสียงเตือน":"เปิดเสียงเตือน"} onClick={()=>{const on=!sound;setSound(on);try{localStorage.setItem("aether.alert.sound",on?"on":"off");}catch{}}}>{sound?<Volume2 size={16}/>:<VolumeX size={16}/>}</button><button type="button" aria-label="ปิดแถบแจ้งเตือน" onClick={()=>setBanner(null)}><X size={16}/></button></div>}<header className="live-top"><button type="button" className="live-sidebar-toggle" onClick={toggleSidebar} aria-controls="workspace-navigation" aria-expanded={!sidebarCollapsed} aria-label={sidebarCollapsed ? "ขยายเมนูหลัก" : "หุบเมนูหลัก"} title={sidebarCollapsed ? "ขยายเมนูหลัก" : "หุบเมนูหลัก"}>{sidebarCollapsed ? <PanelLeftOpen size={18} /> : <PanelLeftClose size={18} />}</button><span className="live-breadcrumb">Workspace <span className="live-divider">/</span> {VIEWS.find(v=>v.id===view)?.label}</span>{signedIn && <Button variant="ghost" onClick={logout} disabled={busy}><LogOut size={16} />ออกจากระบบ</Button>}</header>
      {!signedIn ? <section className="live-login"><div className="live-kicker"><Radio size={17} /> CONNECT TO YOUR SPACE</div><h1>ข้อมูลจริง<br /><span>จากพื้นที่ของคุณ</span></h1><p>เข้าสู่ระบบเพื่อดู gateway และค่าจากเซนเซอร์ที่กำลังส่งเข้า Aether</p><form onSubmit={login}><label htmlFor="email">อีเมล</label><Input id="email" type="email" autoComplete="username" value={email} onChange={e => setEmail(e.target.value)} required /><label htmlFor="password">รหัสผ่านบัญชี Aether</label><Input id="password" type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} required />{error && <p className="live-error" role="alert">{error}</p>}<Button type="submit" disabled={busy || starting}>{starting ? "กำลังตรวจสอบเซสชัน…" : busy ? "กำลังเข้าสู่ระบบ…" : "เข้าสู่ Live monitoring"}<ArrowUpRight size={17} /></Button></form></section> : mustChange ? <section className="live-login"><div className="live-kicker"><ShieldCheck size={17} /> SET YOUR PASSWORD</div><h1>ตั้งรหัสผ่าน<br /><span>ของคุณเอง</span></h1><p>รหัสผ่านแรกถูกส่งให้คุณโดยผู้ดูแล จึงต้องเปลี่ยนก่อนเริ่มใช้งาน · อย่างน้อย 12 ตัวอักษร</p><form onSubmit={changePassword}><label htmlFor="current-password">รหัสผ่านปัจจุบัน</label><Input id="current-password" type="password" autoComplete="current-password" value={currentPassword} onChange={e => setCurrentPassword(e.target.value)} required /><label htmlFor="new-password">รหัสผ่านใหม่</label><Input id="new-password" type="password" autoComplete="new-password" minLength={12} value={newPassword} onChange={e => setNewPassword(e.target.value)} required /><label htmlFor="repeat-password">รหัสผ่านใหม่อีกครั้ง</label><Input id="repeat-password" type="password" autoComplete="new-password" minLength={12} value={repeatPassword} onChange={e => setRepeatPassword(e.target.value)} required />{error && <p className="live-error" role="alert">{error}</p>}<Button type="submit" disabled={busy}>{busy ? "กำลังบันทึก…" : "บันทึกรหัสผ่านใหม่"}</Button></form></section> : me?.permissions?.[view] === "none" && view !== "team" ? <section className="live-login"><h2>คุณไม่มีสิทธิ์เข้าหน้านี้</h2><p>ติดต่อผู้ดูแลเพื่อเปลี่ยนสิทธิ์</p></section> : view==="alerts" ? <AlertsCenter getToken={()=>token.current} refresh={refresh} onSummary={setOpenAlerts} onUnauthorized={()=>{token.current="";setSignedIn(false);setView("live");setError("เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง");}} onOpenDevice={()=>setView("connect")}/> : view==="connect" ? <DeviceTopology getToken={()=>token.current} refresh={refresh} onUnauthorized={()=>{token.current="";setSignedIn(false);setView("live");setError("เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง");}} onAdd={source=>{setRequestedSource(source);setView("studio");}}/> : view==="floorplan" ? <FloorPlanStudio getToken={()=>token.current} refresh={refresh} onUnauthorized={()=>{token.current="";setSignedIn(false);setView("live");setError("เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง");}} onOpenDevice={()=>setView("connect")}/> : view==="assets" ? <AssetsPage getToken={()=>token.current} refresh={refresh} onUnauthorized={()=>{token.current="";setSignedIn(false);setView("live");setError("เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง");}} onOpenDevice={()=>setView("connect")}/> : view==="team" ? <TeamPage getToken={()=>token.current} refresh={refresh} onUnauthorized={()=>{token.current="";setSignedIn(false);setView("live");setError("เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง");}}/> : view==="automation" ? <AutomationStudio getToken={()=>token.current} refresh={refresh} onUnauthorized={()=>{token.current="";setSignedIn(false);setView("live");setError("เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง");}}/> : view==="studio" ? <Studio getToken={()=>token.current} refresh={refresh} sources={studioSources} initialSource={requestedSource}/> : <Overview getToken={()=>token.current} refresh={refresh} onSummary={setOpenAlerts} onUnauthorized={()=>{token.current="";setSignedIn(false);setError("เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง");}} onNavigate={to=>setView(to)}/>}
    </main>
  </div>;
}
