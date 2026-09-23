"use client";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Box, Building2, Copy, DoorOpen, DraftingCompass, Footprints, Grid3x3, Hand, ImagePlus, Layers, Magnet, Map as MapIcon, Maximize2, MousePointer2, PanelLeft, PanelRight, PenLine, Pentagon, PersonStanding, Plus, Radar, Redo2, Router, Ruler, Save, Square, Trash2, Undo2, Cpu } from "lucide-react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import "../topology/topology.css";
import "./floorplan.css";
import { ApiError, createClientFrom, type Snapshot } from "../topology/api";
import { deviceProfile, formatMAC } from "../topology/catalog";
import { useLatest } from "../topology/use-latest";
import { useSignals } from "../topology/use-signals";
import DxfImportDialog from "./dxf-import";
import Editor2D, { type ViewBox } from "./editor2d";
import View3D from "./view3d";
import { COLORS, ITEM_TYPES, ZONE_KINDS, area, buildAssets, draftOf, emptyLayout, round2, wallLength, type AssetView, type Draft, type Floor, type Selection, type Site, type Tool, type Wall } from "./model";

type History = { past: Draft[]; future: Draft[] };
const TOOLS: { id: Tool; label: string; key: string; icon: typeof Hand }[] = [
  { id: "select", label: "เลือก / ย้าย", key: "V", icon: MousePointer2 },
  { id: "pan", label: "เลื่อนผัง (กด Space ค้างก็ได้)", key: "H", icon: Hand },
  { id: "wall", label: "วาดผนัง · คลิกทีละจุด Enter เพื่อจบ", key: "W", icon: PenLine },
  { id: "rect", label: "ห้องสี่เหลี่ยม · ลากเพื่อสร้างโซน", key: "R", icon: Square },
  { id: "zone", label: "โซนหลายเหลี่ยม · คลิกทีละจุด คลิกจุดแรกเพื่อปิด", key: "Z", icon: Pentagon },
  { id: "item", label: "วางประตู เฟอร์นิเจอร์ หรือข้อความ", key: "I", icon: DoorOpen },
  { id: "measure", label: "วัดระยะ", key: "M", icon: Ruler },
];
const fitBox = (d: Draft): ViewBox => ({ x: -d.width_m * 0.08, y: -d.depth_m * 0.08, w: d.width_m * 1.16, h: d.depth_m * 1.16 });
const blankFloor = { name: "", level: 1, width_m: 30, depth_m: 20, ceiling_m: 3, copy: false };

/** Shrinks a scanned plan in the browser so it fits the API's 256 KiB request limit. */
async function shrinkImage(file: File): Promise<string> {
  const bitmap = await createImageBitmap(file);
  let side = 1800, quality = 0.8;
  for (let attempt = 0; attempt < 8; attempt++) {
    const scale = Math.min(1, side / Math.max(bitmap.width, bitmap.height));
    const canvas = document.createElement("canvas");
    canvas.width = Math.max(1, Math.round(bitmap.width * scale));
    canvas.height = Math.max(1, Math.round(bitmap.height * scale));
    canvas.getContext("2d")!.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
    const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, "image/jpeg", quality));
    if (blob && blob.size <= 170000) {
      const bytes = new Uint8Array(await blob.arrayBuffer());
      let binary = "";
      for (let i = 0; i < bytes.length; i += 0x8000) binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
      return btoa(binary);
    }
    side = Math.round(side * 0.8);
    quality = Math.max(0.5, quality - 0.07);
  }
  throw new Error("ย่อภาพให้เล็กพอไม่ได้ ลองใช้ภาพที่ความละเอียดต่ำกว่านี้");
}

export default function FloorPlanStudio({ getToken, refresh, onUnauthorized, onOpenDevice }: { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void; onOpenDevice?: (external: string) => void }) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const onUnauthorizedRef = useLatest(onUnauthorized);

  const [project, setProject] = useState("");
  const siteRequest = useRef(0);
  const [sites, setSites] = useState<Site[] | null>(null);
  const [siteId, setSiteId] = useState("");
  const [site, setSite] = useState<Site | null>(null);
  const [floorId, setFloorId] = useState("");
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});
  const [saved, setSaved] = useState<Record<string, string>>({});
  const [revisions, setRevisions] = useState<Record<string, number>>({});
  const [history, setHistory] = useState<Record<string, History>>({});
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [sosExternals, setSosExternals] = useState<Set<string>>(() => new Set());
  const [mode, setMode] = useState<"2d" | "3d">("2d");
  const [tool, setTool] = useState<Tool>("select");
  const [itemType, setItemType] = useState("door");
  const [selection, setSelection] = useState<Selection>(null);
  const [snap, setSnap] = useState(0.25);
  const [showGrid, setShowGrid] = useState(true);
  const [showCoverage, setShowCoverage] = useState(false);
  const [explode, setExplode] = useState(0.4);
  const [isolate, setIsolate] = useState(false);
  const [panels, setPanels] = useState({ left: true, right: true });
  const [boxes, setBoxes] = useState<Record<string, ViewBox>>({});
  const [resetKey, setResetKey] = useState(0);
  const [imageURL, setImageURL] = useState<{ floor: string; url: string } | null>(null);
  const [imageVersion, setImageVersion] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [siteDialog, setSiteDialog] = useState<{ id?: string; name: string; description: string; project_id: string } | null>(null);
  const [floorDialog, setFloorDialog] = useState<typeof blankFloor | null>(null);
  const [confirm, setConfirm] = useState<{ kind: "floor" | "site"; id: string; name: string } | null>(null);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const dxfRef = useRef<HTMLInputElement | null>(null);
  const [dxf, setDxf] = useState<{ file: File; token: number } | null>(null);

  const fail = useCallback((e: unknown, fallback: string) => {
    if (e instanceof ApiError && e.status === 401) { onUnauthorizedRef.current?.(); return; }
    if (e instanceof ApiError && e.status === 403) { setError("บัญชีนี้ดูได้อย่างเดียว · ต้องเป็น owner หรือ admin จึงแก้ผังได้"); return; }
    if (e instanceof ApiError && e.status === 409) { setError("ชั้นนี้ถูกแก้ไขจากที่อื่นแล้ว · โหลดหน้านี้ใหม่ก่อนบันทึก เพื่อไม่ให้ทับงานของคนอื่น"); return; }
    setError(e instanceof Error ? e.message : fallback);
  }, [onUnauthorizedRef]);

  const adoptSite = useCallback((s: Site, keepFloor?: string) => {
    setSite(s);
    setDrafts(Object.fromEntries(s.floors.map((f) => [f.id, draftOf(f)])));
    setSaved(Object.fromEntries(s.floors.map((f) => [f.id, JSON.stringify(draftOf(f))])));
    setRevisions(Object.fromEntries(s.floors.map((f) => [f.id, f.revision])));
    setHistory({});
    setSelection(null);
    setFloorId(s.floors.find((f) => f.id === keepFloor)?.id ?? s.floors[0]?.id ?? "");
    setResetKey((k) => k + 1);
  }, []);

  const loadSites = useCallback(async (prefer?: string) => {
    const request = ++siteRequest.current;
    try {
      const list = (await client.raw<{ items: Site[] }>("/sites")).items;
      if (request !== siteRequest.current) return;
      setSites(list);
      let wanted = prefer ?? "";
      if (!wanted) { try { wanted = localStorage.getItem(`aether.floorplan.site.${project}`) ?? ""; } catch {} }
      const visible = list.filter((s) => (s.project_id ?? "none") === project);
      const chosen = visible.find((s) => s.id === wanted) ?? visible[0];
      setSiteId(chosen?.id ?? "");
      if (!chosen) { setSite(null); setFloorId(""); setDrafts({}); setSaved({}); setHistory({}); }
    } catch (e) { fail(e, "โหลดรายการอาคารไม่ได้"); setSites([]); }
  }, [client, fail, project]);

  useEffect(() => {
    const first = async () => { await loadSites(); };
    void first();
  }, [loadSites]);

  useEffect(() => {
    if (!siteId) return;
    let active = true;
    try { localStorage.setItem(`aether.floorplan.site.${project}`, siteId); } catch {}
    client.raw<Site>(`/sites/${siteId}`).then((s) => { if (active) adoptSite(s); }).catch((e) => { if (active) fail(e, "โหลดผังอาคารไม่ได้"); });
    return () => { active = false; };
  }, [siteId, client, adoptSite, fail, project]);

  // Live data: the same snapshot the topology page uses. Signals trigger a refetch; the poll is a safety net.
  // Open alerts ride along so the plan can show WHERE an emergency button was pressed; the API flags the SOS.
  const loadLive = useCallback(async (force: boolean) => {
    try {
      const [snap, open] = await Promise.all([
        client.snapshot(force),
        client.raw<{ items: { external_id: string; sos?: boolean }[] }>("/alerts?status=open&limit=50").catch(() => ({ items: [] })),
      ]);
      setSnapshot(snap);
      setSosExternals(new Set(open.items.filter((a) => a.sos).map((a) => a.external_id.toLowerCase())));
    } catch (e) { if (e instanceof ApiError && e.status === 401) onUnauthorizedRef.current?.(); }
  }, [client, onUnauthorizedRef]);
  const pendingSignal = useRef<{ timer?: ReturnType<typeof setTimeout>; force: boolean }>({ force: false });
  const connected = useSignals(handlers, (kind) => {
    const p = pendingSignal.current;
    p.force = p.force || kind === "inventory" || kind === "event"; // a zone change is an event and lives in the device list
    clearTimeout(p.timer);
    p.timer = setTimeout(() => { const force = p.force; p.force = false; void loadLive(force); }, 400);
  });
  const connectedRef = useLatest(connected);
  useEffect(() => {
    let active = true, timer: ReturnType<typeof setTimeout>;
    const p = pendingSignal.current;
    const poll = async () => { await loadLive(false); if (active) timer = setTimeout(poll, connectedRef.current ? 20000 : 6000); };
    void poll();
    return () => { active = false; clearTimeout(timer); clearTimeout(p.timer); };
  }, [loadLive, connectedRef]);

  // Scanned plan of the active floor.
  const floor = site?.floors.find((f) => f.id === floorId) ?? null;
  const hasImage = floor?.has_image ?? false;
  useEffect(() => {
    if (!floorId || !hasImage) return;
    let active = true, url = "";
    client.blob(`/floors/${floorId}/image`).then((blob) => {
      if (!active || !blob) return;
      url = URL.createObjectURL(blob);
      setImageURL({ floor: floorId, url });
    }).catch(() => {});
    return () => { active = false; if (url) URL.revokeObjectURL(url); };
  }, [floorId, hasImage, client, imageVersion]);

  const draft = site?.id === siteId && (site.project_id ?? "none") === project ? drafts[floorId] ?? null : null;
  const dirtyFloors = useMemo(() => Object.keys(drafts).filter((id) => saved[id] !== undefined && JSON.stringify(drafts[id]) !== saved[id]), [drafts, saved]);
  const dirty = dirtyFloors.length > 0;
  const dirtyRef = useLatest(dirty);
  useEffect(() => {
    const guard = (e: BeforeUnloadEvent) => { if (dirtyRef.current) { e.preventDefault(); e.returnValue = ""; } };
    window.addEventListener("beforeunload", guard);
    return () => window.removeEventListener("beforeunload", guard);
  }, [dirtyRef]);

  const wearableProfiles = useMemo(() => new Set((snapshot?.devices ?? []).map((d) => d.profile_id).filter((id) => deviceProfile(id)?.wearable)), [snapshot]);
  const assets = useMemo(() => buildAssets(snapshot && site?.project_id ? { ...snapshot, gateways: snapshot.gateways.filter((g) => g.project_id === site.project_id), devices: snapshot.devices.filter((d) => snapshot.gateways.some((g) => g.id === d.gateway_id && g.project_id === site.project_id)), live: snapshot.live ? { ...snapshot.live, gateways: snapshot.live.gateways.filter((g) => g.gateway.project_id === site.project_id) } : null } : snapshot, wearableProfiles, sosExternals), [snapshot, wearableProfiles, site, sosExternals]);
  const placedKeys = useMemo(() => { const m = new Map<string, string>(); for (const [id, d] of Object.entries(drafts)) for (const p of d.placements) m.set(`${p.asset_kind}:${p.asset_id}`, id); return m; }, [drafts]);

  const change = useCallback((next: Draft, opts: { transient: boolean; base?: Draft }) => {
    if (!floorId) return;
    setDrafts((all) => ({ ...all, [floorId]: next }));
    if (opts.transient) return;
    setHistory((all) => { const h = all[floorId] ?? { past: [], future: [] }; return { ...all, [floorId]: { past: [...h.past, opts.base ?? next].slice(-80), future: [] } }; });
  }, [floorId]);
  const edit = (fn: (d: Draft) => Draft) => { if (draft) change(fn(draft), { transient: false, base: draft }); };
  const undo = () => { const h = history[floorId]; if (!h?.past.length || !draft) return; const prev = h.past[h.past.length - 1]; setHistory({ ...history, [floorId]: { past: h.past.slice(0, -1), future: [draft, ...h.future] } }); setDrafts({ ...drafts, [floorId]: prev }); };
  const redo = () => { const h = history[floorId]; if (!h?.future.length || !draft) return; const next = h.future[0]; setHistory({ ...history, [floorId]: { past: [...h.past, draft], future: h.future.slice(1) } }); setDrafts({ ...drafts, [floorId]: next }); };

  const removeSelection = () => {
    if (!selection || !draft) return;
    edit((d) => selection.kind === "zone" ? { ...d, layout: { ...d.layout, zones: d.layout.zones.filter((z) => z.id !== selection.id) } }
      : selection.kind === "wall" ? { ...d, layout: { ...d.layout, walls: d.layout.walls.filter((w) => w.id !== selection.id) } }
      : selection.kind === "item" ? { ...d, layout: { ...d.layout, items: d.layout.items.filter((i) => i.id !== selection.id) } }
      : { ...d, placements: d.placements.filter((p) => `${p.asset_kind}:${p.asset_id}` !== selection.id) });
    setSelection(null);
  };

  const save = async () => {
    if (!dirty || busy) return;
    setBusy(true); setError("");
    try {
      const nextRevisions = { ...revisions }, nextSaved = { ...saved };
      for (const id of dirtyFloors) {
        const d = drafts[id];
        const out = await client.post<{ revision: number; bumped?: Record<string, number> }>(`/floors/${id}/save`, { name: d.name.trim() || "ชั้น", level: d.level, width_m: d.width_m, depth_m: d.depth_m, ceiling_m: d.ceiling_m, revision: revisions[id], layout: { walls: d.layout.walls, zones: d.layout.zones, items: d.layout.items, background: d.layout.background ?? undefined }, placements: d.placements });
        nextRevisions[id] = out.revision;
        Object.assign(nextRevisions, out.bumped ?? {}); // floors that lost an asset to this one
        nextSaved[id] = JSON.stringify(d);
        setRevisions({ ...nextRevisions }); setSaved({ ...nextSaved });
      }
      setNotice(`บันทึกผังแล้ว ${dirtyFloors.length} ชั้น`);
    } catch (e) { fail(e, "บันทึกไม่สำเร็จ"); }
    finally { setBusy(false); }
  };

  const actions = useLatest({ undo, redo, save, removeSelection, drawing: mode === "2d" && (tool === "wall" || tool === "zone") });
  useEffect(() => {
    const down = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      if (el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT" || el.isContentEditable)) return;
      if (document.querySelector("[data-slot=dialog-content],[role=alertdialog]")) return;
      const meta = e.metaKey || e.ctrlKey, key = e.key.toLowerCase();
      if (meta && key === "z") { e.preventDefault(); if (e.shiftKey) actions.current.redo(); else actions.current.undo(); return; }
      if (meta && key === "y") { e.preventDefault(); actions.current.redo(); return; }
      if (meta && key === "s") { e.preventDefault(); void actions.current.save(); return; }
      if (meta || e.altKey) return;
      if (key === "delete" || (key === "backspace" && !actions.current.drawing)) { actions.current.removeSelection(); return; }
      const t = TOOLS.find((x) => x.key.toLowerCase() === key);
      if (t) { setTool(t.id); setMode("2d"); }
      if (key === "2") setMode("2d");
      if (key === "3") setMode("3d");
    };
    window.addEventListener("keydown", down);
    return () => window.removeEventListener("keydown", down);
  }, [actions]);

  useEffect(() => { if (!notice) return; const t = setTimeout(() => setNotice(""), 3500); return () => clearTimeout(t); }, [notice]);

  const guardSwitch = (go: () => void) => { if (dirty && !window.confirm("มีการแก้ไขที่ยังไม่ได้บันทึก ต้องการออกจากอาคารนี้โดยไม่บันทึกหรือไม่")) return; go(); };

  const place = (a: AssetView) => {
    if (!draft) return;
    const key = `${a.kind}:${a.id}`;
    const others = Object.fromEntries(Object.entries(drafts).map(([id, d]) => [id, id === floorId ? d : { ...d, placements: d.placements.filter((p) => `${p.asset_kind}:${p.asset_id}` !== key) }]));
    const n = draft.placements.length;
    const next = { ...draft, placements: [...draft.placements.filter((p) => `${p.asset_kind}:${p.asset_id}` !== key), { asset_kind: a.kind, asset_id: a.id, x: round2(Math.min(draft.width_m - 1, 2 + (n % 8) * 2)), y: round2(Math.min(draft.depth_m - 1, 2 + Math.floor(n / 8) * 2)), z: a.kind === "gateway" ? 2.4 : 1.2 }] };
    setDrafts({ ...others, [floorId]: next });
    // Every floor this touches gets an undo step, including the floor the asset was taken from.
    setHistory((all) => {
      const out = { ...all };
      for (const [id, before] of Object.entries(drafts)) {
        if (id !== floorId && others[id] === before) continue;
        const h = out[id] ?? { past: [], future: [] };
        out[id] = { past: [...h.past, before].slice(-80), future: [] };
      }
      return out;
    });
    setSelection({ kind: "placement", id: key });
  };

  // The 3D view moves the marker itself while dragging and reports the final position once.
  const move3D = (key: string, x: number, y: number) => {
    if (!draft) return;
    change({ ...draft, placements: draft.placements.map((p) => (`${p.asset_kind}:${p.asset_id}` === key ? { ...p, x, y } : p)) }, { transient: false, base: draft });
  };

  const uploadImage = async (file: File) => {
    if (!floor || !draft) return;
    setBusy(true); setError("");
    try {
      const data = await shrinkImage(file);
      await client.post(`/floors/${floor.id}/image`, { data_base64: data });
      setSite((s) => (s ? { ...s, floors: s.floors.map((f) => (f.id === floor.id ? { ...f, has_image: true, updated_at: new Date().toISOString() } : f)) } : s));
      setImageVersion((v) => v + 1); // the effect below fetches it and owns (and revokes) the object URL
      if (!draft.layout.background) edit((d) => ({ ...d, layout: { ...d.layout, background: { opacity: 0.55, x: 0, y: 0, width_m: d.width_m } } }));
      setNotice("ใส่ภาพผังแล้ว · ปรับขนาดและความทึบได้ในแถบขวา แล้ววาดผนังทับได้เลย");
    } catch (e) { fail(e, "อัปโหลดภาพไม่สำเร็จ"); }
    finally { setBusy(false); }
  };
  const removeImage = async () => {
    if (!floor) return;
    try {
      await client.post(`/floors/${floor.id}/image`, { data_base64: "" });
      setSite((s) => (s ? { ...s, floors: s.floors.map((f) => (f.id === floor.id ? { ...f, has_image: false } : f)) } : s));
      setImageURL(null);
      edit((d) => ({ ...d, layout: { ...d.layout, background: null } }));
    } catch (e) { fail(e, "ลบภาพไม่สำเร็จ"); }
  };

  const box = (draft && boxes[floorId]) || (draft ? fitBox(draft) : { x: 0, y: 0, w: 30, h: 20 });
  const selectedZone = selection?.kind === "zone" ? draft?.layout.zones.find((z) => z.id === selection.id) : undefined;
  const selectedWall = selection?.kind === "wall" ? draft?.layout.walls.find((w) => w.id === selection.id) : undefined;
  const selectedItem = selection?.kind === "item" ? draft?.layout.items.find((i) => i.id === selection.id) : undefined;
  const selectedPlacement = selection?.kind === "placement" ? draft?.placements.find((p) => `${p.asset_kind}:${p.asset_id}` === selection.id) : undefined;
  const selectedAsset = selectedPlacement ? assets.find((a) => a.kind === selectedPlacement.asset_kind && a.id === selectedPlacement.asset_id) : undefined;
  const unplaced = assets.filter((a) => !placedKeys.has(`${a.kind}:${a.id}`) && !(a.kind === "device" && a.wearable));
  const wearables = assets.filter((a) => a.kind === "device" && a.wearable);
  const gatewayAssets = assets.filter((a) => a.kind === "gateway");
  const floors3D = useMemo(() => (site ? site.floors.filter((f) => drafts[f.id]).map((f) => ({ id: f.id, draft: drafts[f.id] })) : []), [site, drafts]);
  const h = history[floorId];
  const num = (v: string, lo: number, hi: number, fallback: number) => { const n = Number(v); return Number.isFinite(n) ? Math.min(hi, Math.max(lo, n)) : fallback; };

  return (
    <section className="topo fp">
      <header className="topo-bar fp-bar">
        <h1 className="topo-bar-title">ผังอาคาร</h1>
        <span className={`topo-live ${connected ? "is-on" : ""}`} title={connected ? "ตำแหน่งและสถานะอัปเดตแบบ real-time" : "ใช้การตรวจเป็นรอบ"}><i aria-hidden="true" /> {connected ? "Live" : "Polling"}</span>
        <select className="fp-project" aria-label="โปรเจคผังอาคาร" value={project} onChange={(e) => guardSwitch(() => { siteRequest.current++; setProject(e.target.value); setSite(null); setSiteId(""); setFloorId(""); setDrafts({}); setSaved({}); setHistory({}); setSelection(null); })}><option value="" disabled>เลือกโปรเจค</option><option value="none">ยังไม่จัดโปรเจค</option>{snapshot?.projects?.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}</select>
        <label className="fp-site">
          <Building2 size={14} />
          <select aria-label="เลือกอาคาร" value={siteId} onChange={(e) => guardSwitch(() => setSiteId(e.target.value))}>
            {(sites ?? []).filter((s) => (s.project_id ?? "none") === project).length === 0 && <option value="">ยังไม่มีอาคาร</option>}
            {(sites ?? []).filter((s) => (s.project_id ?? "none") === project).map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
          </select>
        </label>
        <button type="button" className="topo-btn" title="เพิ่มอาคาร" aria-label="เพิ่มอาคาร" disabled={!project} onClick={() => setSiteDialog({ name: "", description: "", project_id: project === "none" ? "" : project })}><Plus size={15} /></button>
      </header>
      <div className="topo-bar fp-bar fp-edit-bar" aria-label="เครื่องมือผังอาคาร">
        <div className="fp-seg" role="group" aria-label="มุมมอง">
          <button type="button" className={mode === "2d" ? "is-on" : ""} aria-pressed={mode === "2d"} onClick={() => setMode("2d")} title="แก้ไขผัง 2 มิติ (2)"><MapIcon size={14} /> 2D</button>
          <button type="button" className={mode === "3d" ? "is-on" : ""} aria-pressed={mode === "3d"} onClick={() => { setMode("3d"); setTool("select"); }} title="ดูทั้งอาคาร 3 มิติ (3)"><Box size={14} /> 3D</button>
        </div>
        {mode === "2d" ? (
          <div className="fp-tools" role="toolbar" aria-label="เครื่องมือวาด">
            {TOOLS.map((t) => <button key={t.id} type="button" className={`topo-btn ${tool === t.id ? "is-on" : ""}`} aria-pressed={tool === t.id} title={`${t.label} (${t.key})`} aria-label={t.label} onClick={() => setTool(t.id)}><t.icon size={15} /></button>)}
            {tool === "item" && <select className="fp-mini" aria-label="ชนิดของที่จะวาง" value={itemType} onChange={(e) => setItemType(e.target.value)}>{ITEM_TYPES.map((t) => <option key={t.id} value={t.id}>{t.label}</option>)}</select>}
          </div>
        ) : (
          <div className="fp-tools">
            <label className="fp-slider" title="ระยะห่างระหว่างชั้น"><Layers size={14} /><input type="range" min={0} max={1.5} step={0.05} value={explode} onChange={(e) => setExplode(Number(e.target.value))} aria-label="ระยะห่างระหว่างชั้น" /></label>
            <button type="button" className={`topo-btn ${isolate ? "is-on" : ""}`} aria-pressed={isolate} onClick={() => setIsolate((v) => !v)} title="แสดงเฉพาะชั้นที่เลือก"><Layers size={15} /> เฉพาะชั้นนี้</button>
          </div>
        )}
        <span className="topo-bar-sep" aria-hidden="true" />
        <button type="button" className="topo-btn" disabled={!h?.past.length} onClick={undo} title="ย้อนกลับ (Ctrl/⌘ Z)" aria-label="ย้อนกลับ"><Undo2 size={15} /></button>
        <button type="button" className="topo-btn" disabled={!h?.future.length} onClick={redo} title="ทำซ้ำ (Ctrl/⌘ Shift Z)" aria-label="ทำซ้ำ"><Redo2 size={15} /></button>
        <label className="fp-snap" title="ระยะ snap"><Magnet size={14} /><select aria-label="ระยะ snap" value={snap} onChange={(e) => setSnap(Number(e.target.value))}><option value={0}>อิสระ</option><option value={0.05}>5 ซม.</option><option value={0.25}>25 ซม.</option><option value={0.5}>50 ซม.</option><option value={1}>1 ม.</option></select></label>
        <button type="button" className={`topo-btn ${showGrid ? "is-on" : ""}`} aria-pressed={showGrid} onClick={() => setShowGrid((v) => !v)} title="แสดงตาราง (ช่องละ 1 ม.)" aria-label="แสดงตาราง"><Grid3x3 size={15} /></button>
        <button type="button" className={`topo-btn ${showCoverage ? "is-on" : ""}`} aria-pressed={showCoverage} onClick={() => setShowCoverage((v) => !v)} title="วงรัศมี 10 ม. รอบ gateway (ค่าประมาณสำหรับวางแผน)" aria-label="แสดงรัศมี gateway"><Radar size={15} /></button>
        <button type="button" className="topo-btn" onClick={() => { if (draft) setBoxes({ ...boxes, [floorId]: fitBox(draft) }); setResetKey((k) => k + 1); }} title="พอดีจอ" aria-label="พอดีจอ"><Maximize2 size={15} /></button>
        <button type="button" className="topo-btn" disabled={!floor || busy} onClick={() => fileRef.current?.click()} title="ใส่ภาพผัง (สแกน / CAD export) ไว้วาดทับ" aria-label="ใส่ภาพผัง"><ImagePlus size={15} /></button>
        <input ref={fileRef} type="file" accept="image/png,image/jpeg,image/webp" hidden onChange={(e) => { const f = e.target.files?.[0]; e.target.value = ""; if (f) void uploadImage(f); }} />
        <button type="button" className="topo-btn" disabled={!draft || busy} onClick={() => dxfRef.current?.click()} title="นำเข้า CAD (DXF) · ผนังและโซนจากไฟล์ .dxf · DWG ต้อง export เป็น DXF ก่อน" aria-label="นำเข้า CAD (DXF)"><DraftingCompass size={15} /></button>
        <input ref={dxfRef} type="file" accept=".dxf,application/dxf,image/vnd.dxf" hidden onChange={(e) => { const f = e.target.files?.[0]; e.target.value = ""; if (f) setDxf({ file: f, token: Date.now() }); }} />
        <span className="fp-spacer" />
        <button type="button" className={`topo-btn ${dirty ? "primary" : ""}`} disabled={!dirty || busy} onClick={() => void save()} title="บันทึก (Ctrl/⌘ S)"><Save size={15} /> {busy ? "กำลังบันทึก…" : dirty ? `บันทึก${dirtyFloors.length > 1 ? ` ${dirtyFloors.length} ชั้น` : ""}` : "บันทึกแล้ว"}</button>
        <button type="button" className={`topo-btn ${panels.left ? "is-on" : ""}`} aria-pressed={panels.left} onClick={() => setPanels({ ...panels, left: !panels.left })} title="แถบอุปกรณ์" aria-label="แถบอุปกรณ์"><PanelLeft size={15} /></button>
        <button type="button" className={`topo-btn ${panels.right ? "is-on" : ""}`} aria-pressed={panels.right} onClick={() => setPanels({ ...panels, right: !panels.right })} title="แถบคุณสมบัติ" aria-label="แถบคุณสมบัติ"><PanelRight size={15} /></button>
      </div>

      {site && (
        <nav className="topo-projects fp-floors" aria-label="ชั้น">
          <span className="topo-projects-label"><Layers size={14} /> ชั้น</span>
          {[...site.floors].sort((a, b) => (drafts[b.id]?.level ?? b.level) - (drafts[a.id]?.level ?? a.level)).map((f) => (
            <button key={f.id} type="button" className={`topo-project ${f.id === floorId ? "is-on" : ""}`} aria-pressed={f.id === floorId} onClick={() => { setFloorId(f.id); setSelection(null); }}>
              {drafts[f.id]?.name ?? f.name} <small>L{drafts[f.id]?.level ?? f.level}</small>{dirtyFloors.includes(f.id) && <i className="fp-dirty" title="ยังไม่ได้บันทึก" />}
            </button>
          ))}
          <span className="topo-projects-actions">
            <button type="button" className="topo-btn" onClick={() => setFloorDialog({ ...blankFloor, name: `ชั้น ${site.floors.length + 1}`, level: Math.max(0, ...site.floors.map((f) => drafts[f.id]?.level ?? f.level)) + 1, width_m: draft?.width_m ?? 30, depth_m: draft?.depth_m ?? 20, ceiling_m: draft?.ceiling_m ?? 3, copy: !!draft && draft.layout.walls.length > 0 })}><Plus size={14} /> เพิ่มชั้น</button>
          </span>
        </nav>
      )}

      <div className={`topo-body fp-body ${panels.left ? "" : "no-palette"} ${panels.right ? "" : "no-inspector"}`}>
        {panels.left && (
          <aside className="fp-side" aria-label="อุปกรณ์สำหรับวางบนผัง">
            <h2><Router size={14} /> ยังไม่ได้วาง <small>{unplaced.length}</small></h2>
            <p className="topo-note">ลากลงบนผัง หรือคลิกเพื่อวางบนชั้นนี้ · วางเซ็นเซอร์ PIR ไว้ในห้องเพื่อแสดงว่ามีคนหรือว่าง</p>
            <ul className="fp-assets">
              {unplaced.map((a) => (
                <li key={`${a.kind}:${a.id}`}>
                  <button type="button" draggable disabled={!draft} onDragStart={(e) => { e.dataTransfer.setData("application/aether-asset", JSON.stringify({ kind: a.kind, id: a.id })); e.dataTransfer.effectAllowed = "copy"; }} onClick={() => place(a)} className={`fp-asset-row ${a.online ? "is-online" : ""}`}>
                    {a.kind === "gateway" ? <Router size={15} /> : a.door ? <DoorOpen size={15} /> : a.occupancy ? <PersonStanding size={15} /> : <Cpu size={15} />}
                    <span><strong>{a.name}</strong><small>{a.kind === "gateway" ? "gateway" : formatMAC(a.external ?? "")}{a.summary ? ` · ${a.summary}` : ""}</small></span>
                  </button>
                </li>
              ))}
              {unplaced.length === 0 && <li className="topo-empty">วางครบทุกตัวแล้ว</li>}
            </ul>
            <h2><Footprints size={14} /> Wearable <small>{wearables.length}</small></h2>
            <p className="topo-note">ไม่ต้องวาง · แสดงรอบ gateway ที่ได้ยินชัดที่สุด และนับคนในโซนที่ผูกกับ gateway นั้น</p>
            <ul className="fp-assets">
              {wearables.map((w) => {
                const gw = gatewayAssets.find((g) => g.id === w.zoneGatewayId), onFloor = w.zoneGatewayId ? placedKeys.get(`gateway:${w.zoneGatewayId}`) : undefined;
                return (
                  <li key={w.id}>
                    <button type="button" className={`fp-asset-row is-wearable ${w.online ? "is-online" : ""}${w.sos ? " is-sos" : ""}`} disabled={!onFloor} title={onFloor ? "ไปยังชั้นที่คนนี้อยู่" : "gateway ที่ได้ยินยังไม่ได้วางบนผัง"} onClick={() => { if (onFloor) { setFloorId(onFloor); setSelection({ kind: "placement", id: `gateway:${w.zoneGatewayId}` }); } }}>
                      <Footprints size={15} />
                      <span><strong>{w.sos ? `SOS · ${w.name}` : w.name}</strong><small>{w.sos ? `กดปุ่มฉุกเฉิน${gw ? ` · ใกล้ ${gw.name}` : ""}` : w.online ? (gw ? `อยู่ใกล้ ${gw.name}` : "ได้ยินอยู่ · ยังไม่เปิด roaming") : "ไม่อยู่ในระยะ"}</small></span>
                    </button>
                  </li>
                );
              })}
              {wearables.length === 0 && <li className="topo-empty">ยังไม่มี wearable · เปิดโหมด roaming ได้ในหน้าเชื่อมต่ออุปกรณ์</li>}
            </ul>
          </aside>
        )}

        <div className="fp-stage">
          <div className="topo-toasts">
            {error && <div className="topo-alert is-error" role="alert">{error}<button type="button" aria-label="ปิด" onClick={() => setError("")}>×</button></div>}
            {notice && <div className="topo-alert is-notice" role="status">{notice}</div>}
          </div>
          {sites !== null && !site && (
            <div className="fp-empty">
              <Building2 size={38} />
              <h2>{!project ? "เลือกโปรเจคเพื่อเปิดผังอาคาร" : "โปรเจคนี้ยังไม่มีผังอาคาร"}</h2>
              <p>วาดผนัง แบ่งโซน วาง gateway และอุปกรณ์ตามตำแหน่งจริง แล้วดู wearable เคลื่อนไปตามโซนทั้งแบบ 2 มิติและ 3 มิติ</p>
              <button type="button" className="topo-btn primary" disabled={!project} onClick={() => setSiteDialog({ name: "", description: "", project_id: project === "none" ? "" : project })}><Plus size={15} /> สร้างอาคารแรก</button>
            </div>
          )}
          {draft && mode === "2d" && <Editor2D draft={draft} tool={tool} itemType={itemType} selection={selection} snap={snap} showGrid={showGrid} showCoverage={showCoverage} assets={assets} imageURL={imageURL?.floor === floorId && hasImage ? imageURL.url : null} readOnly={false} box={box} onBox={(b) => setBoxes((all) => ({ ...all, [floorId]: b }))} onSelect={setSelection} onChange={change} onToolDone={() => setTool("select")} />}
          {draft && mode === "3d" && <View3D floors={floors3D} activeId={floorId} assets={assets} selection={selection} explode={explode} isolate={isolate} snap={snap} readOnly={false} resetKey={resetKey} onSelect={setSelection} onPickFloor={(id) => { setFloorId(id); setSelection(null); }} onMove={move3D} />}
          {draft && <div className="fp-hint">{mode === "3d" ? "ลากเพื่อหมุน · เลื่อนล้อเพื่อซูม · คลิกขวาลากเพื่อเลื่อน · ลากอุปกรณ์ของชั้นที่เลือกเพื่อย้าย · คลิกชั้นอื่นเพื่อสลับ" : tool === "wall" ? "คลิกทีละจุด · Shift ล็อกแนวตั้ง/นอน · Enter หรือดับเบิลคลิกเพื่อจบ · Backspace ลบจุดล่าสุด" : tool === "zone" ? "คลิกทีละมุมของโซน · คลิกจุดแรกหรือ Enter เพื่อปิดรูป" : tool === "rect" ? "ลากเพื่อสร้างห้องสี่เหลี่ยม" : tool === "measure" ? "คลิกสองจุดเพื่อวัดระยะ" : tool === "item" ? "คลิกบนผังเพื่อวาง" : "ลากเพื่อย้าย · ลากจุดสี่เหลี่ยมเพื่อปรับรูป · ดับเบิลคลิกขอบเพื่อเพิ่มจุด · Alt คลิกจุดเพื่อลบ · Delete ลบ"}</div>}
        </div>

        {panels.right && (
          <aside className="fp-side fp-inspector" aria-label="คุณสมบัติ">
            {!draft && <p className="topo-note">เลือกหรือสร้างอาคารก่อน</p>}
            {draft && selectedZone && (
              <>
                <h2>โซน</h2>
                <label>ชื่อโซน<input value={selectedZone.name} maxLength={80} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, zones: d.layout.zones.map((z) => (z.id === selectedZone.id ? { ...z, name: e.target.value } : z)) } }))} /></label>
                <label>ประเภท<select value={selectedZone.kind} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, zones: d.layout.zones.map((z) => (z.id === selectedZone.id ? { ...z, kind: e.target.value } : z)) } }))}>{ZONE_KINDS.map(([id, l]) => <option key={id} value={id}>{l}</option>)}</select></label>
                <fieldset className="topo-colors"><legend>สี</legend>{Object.keys(COLORS).map((c) => <label key={c} className={selectedZone.color === c ? "is-on" : ""}><input type="radio" name="zone-color" checked={selectedZone.color === c} onChange={() => edit((d) => ({ ...d, layout: { ...d.layout, zones: d.layout.zones.map((z) => (z.id === selectedZone.id ? { ...z, color: c } : z)) } }))} /><span style={{ background: COLORS[c] }} aria-hidden="true" /><span className="sr-only">{c}</span></label>)}</fieldset>
                <fieldset className="fp-checks"><legend>gateway ที่ครอบคลุมโซนนี้</legend>
                  {gatewayAssets.map((g) => <label key={g.id}><input type="checkbox" checked={selectedZone.gateway_ids?.includes(g.id) ?? false} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, zones: d.layout.zones.map((z) => (z.id === selectedZone.id ? { ...z, gateway_ids: e.target.checked ? [...(z.gateway_ids ?? []), g.id] : (z.gateway_ids ?? []).filter((x) => x !== g.id) } : z)) } }))} />{g.name}</label>)}
                  <small>wearable ที่อยู่ใกล้ gateway เหล่านี้จะถูกนับว่าอยู่ในโซน</small>
                </fieldset>
                <label>หมายเหตุ<input value={selectedZone.note ?? ""} maxLength={300} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, zones: d.layout.zones.map((z) => (z.id === selectedZone.id ? { ...z, note: e.target.value } : z)) } }))} /></label>
                <p className="topo-note">พื้นที่ {area(selectedZone.points).toFixed(1)} m² · {selectedZone.points.length} จุด</p>
              </>
            )}
            {draft && selectedWall && (
              <>
                <h2>ผนัง</h2>
                <label>ความหนา (ม.)<input type="number" step={0.05} min={0.05} max={1} value={selectedWall.thickness} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, walls: d.layout.walls.map((w) => (w.id === selectedWall.id ? { ...w, thickness: num(e.target.value, 0.05, 1, 0.15) } : w)) } }))} /></label>
                <label className="fp-inline"><input type="checkbox" checked={!!selectedWall.closed} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, walls: d.layout.walls.map((w) => (w.id === selectedWall.id ? { ...w, closed: e.target.checked } : w)) } }))} />ปิดรูป (เชื่อมจุดสุดท้ายกับจุดแรก)</label>
                <p className="topo-note">ยาวรวม {wallLength(selectedWall as Wall).toFixed(2)} ม. · {selectedWall.points.length} จุด</p>
              </>
            )}
            {draft && selectedItem && (
              <>
                <h2>{ITEM_TYPES.find((t) => t.id === selectedItem.type)?.label ?? selectedItem.type}</h2>
                <label>{selectedItem.type === "label" ? "ข้อความ" : "ป้ายกำกับ (ไม่บังคับ)"}<input value={selectedItem.text ?? ""} maxLength={80} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, items: d.layout.items.map((i) => (i.id === selectedItem.id ? { ...i, text: e.target.value } : i)) } }))} /></label>
                <div className="fp-grid2">
                  <label>กว้าง (ม.)<input type="number" step={0.1} min={0.1} value={selectedItem.w} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, items: d.layout.items.map((i) => (i.id === selectedItem.id ? { ...i, w: num(e.target.value, 0.1, 200, i.w) } : i)) } }))} /></label>
                  <label>ลึก (ม.)<input type="number" step={0.1} min={0.1} value={selectedItem.h} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, items: d.layout.items.map((i) => (i.id === selectedItem.id ? { ...i, h: num(e.target.value, 0.1, 200, i.h) } : i)) } }))} /></label>
                </div>
                <label>หมุน (องศา)<input type="range" min={0} max={345} step={15} value={selectedItem.rot} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, items: d.layout.items.map((i) => (i.id === selectedItem.id ? { ...i, rot: Number(e.target.value) } : i)) } }))} /></label>
                <button type="button" className="topo-btn" onClick={() => edit((d) => ({ ...d, layout: { ...d.layout, items: [...d.layout.items, { ...selectedItem, id: `i${Date.now().toString(36)}`, x: round2(selectedItem.x + 0.5), y: round2(selectedItem.y + 0.5) }] } }))}><Copy size={14} /> ทำสำเนา</button>
              </>
            )}
            {draft && selectedPlacement && (
              <>
                <h2>{selectedPlacement.asset_kind === "gateway" ? "Gateway" : "อุปกรณ์"}</h2>
                <p className="fp-asset-title"><strong>{selectedAsset?.name ?? "ไม่พบอุปกรณ์"}</strong><small>{selectedAsset ? (selectedAsset.online ? "online" : "ไม่มีข้อมูลล่าสุด") : "ถูกนำออกจาก workspace แล้ว"}{selectedAsset?.summary ? ` · ${selectedAsset.summary}` : ""}</small></p>
                <div className="fp-grid2">
                  <label>X (ม.)<input type="number" step={0.25} value={selectedPlacement.x} onChange={(e) => edit((d) => ({ ...d, placements: d.placements.map((p) => (p === selectedPlacement ? { ...p, x: num(e.target.value, 0, d.width_m, p.x) } : p)) }))} /></label>
                  <label>Y (ม.)<input type="number" step={0.25} value={selectedPlacement.y} onChange={(e) => edit((d) => ({ ...d, placements: d.placements.map((p) => (p === selectedPlacement ? { ...p, y: num(e.target.value, 0, d.depth_m, p.y) } : p)) }))} /></label>
                </div>
                <label>ความสูงที่ติดตั้ง (ม.)<input type="number" step={0.1} min={0} max={draft.ceiling_m} value={selectedPlacement.z} onChange={(e) => edit((d) => ({ ...d, placements: d.placements.map((p) => (p === selectedPlacement ? { ...p, z: num(e.target.value, 0, 30, p.z) } : p)) }))} /></label>
                {selectedAsset?.external && onOpenDevice && <button type="button" className="topo-btn" onClick={() => onOpenDevice(selectedAsset.external!)}>เปิดในหน้าเชื่อมต่ออุปกรณ์</button>}
              </>
            )}
            {draft && selection && <button type="button" className="topo-btn danger" onClick={removeSelection}><Trash2 size={14} /> {selection.kind === "placement" ? "นำออกจากผัง" : "ลบ"}</button>}
            {draft && !selection && (
              <>
                <h2>ชั้นนี้</h2>
                <label>ชื่อชั้น<input value={draft.name} maxLength={80} onChange={(e) => edit((d) => ({ ...d, name: e.target.value }))} /></label>
                <div className="fp-grid2">
                  <label>ลำดับชั้น (level)<input type="number" min={-20} max={200} value={draft.level} onChange={(e) => edit((d) => ({ ...d, level: Math.round(num(e.target.value, -20, 200, d.level)) }))} /></label>
                  <label>ความสูงเพดาน (ม.)<input type="number" step={0.1} min={2} max={30} value={draft.ceiling_m} onChange={(e) => edit((d) => ({ ...d, ceiling_m: num(e.target.value, 2, 30, d.ceiling_m) }))} /></label>
                  <label>กว้าง (ม.)<input type="number" min={2} max={1000} value={draft.width_m} onChange={(e) => edit((d) => ({ ...d, width_m: num(e.target.value, 2, 1000, d.width_m) }))} /></label>
                  <label>ลึก (ม.)<input type="number" min={2} max={1000} value={draft.depth_m} onChange={(e) => edit((d) => ({ ...d, depth_m: num(e.target.value, 2, 1000, d.depth_m) }))} /></label>
                </div>
                <p className="topo-note">{draft.layout.zones.length} โซน · {draft.layout.walls.length} ผนัง · {draft.placements.length} อุปกรณ์ · พื้นที่ชั้น {(draft.width_m * draft.depth_m).toFixed(0)} m²</p>
                {hasImage && draft.layout.background && (
                  <>
                    <h2>ภาพผังพื้นหลัง</h2>
                    <label>ความทึบ<input type="range" min={0.1} max={1} step={0.05} value={draft.layout.background.opacity} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, background: { ...d.layout.background!, opacity: Number(e.target.value) } } }))} /></label>
                    <label>ความกว้างจริงของภาพ (ม.)<input type="number" min={1} max={1000} step={0.5} value={draft.layout.background.width_m} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, background: { ...d.layout.background!, width_m: num(e.target.value, 1, 1000, d.width_m) } } }))} /></label>
                    <div className="fp-grid2">
                      <label>เลื่อน X (ม.)<input type="number" step={0.25} value={draft.layout.background.x} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, background: { ...d.layout.background!, x: num(e.target.value, -1000, 2000, 0) } } }))} /></label>
                      <label>เลื่อน Y (ม.)<input type="number" step={0.25} value={draft.layout.background.y} onChange={(e) => edit((d) => ({ ...d, layout: { ...d.layout, background: { ...d.layout.background!, y: num(e.target.value, -1000, 2000, 0) } } }))} /></label>
                    </div>
                    <p className="topo-note">วัดระยะที่รู้ขนาดจริงด้วยเครื่องมือวัด แล้วปรับความกว้างของภาพจนตรงกัน</p>
                    <button type="button" className="topo-btn" onClick={() => void removeImage()}>นำภาพออก</button>
                  </>
                )}
                <h2>อาคาร</h2>
                <div className="fp-row">
                  <button type="button" className="topo-btn" onClick={() => site && setSiteDialog({ id: site.id, name: site.name, description: site.description, project_id: site.project_id ?? "" })}>แก้ไขอาคาร</button>
                  <button type="button" className="topo-btn danger" disabled={!floor || (site?.floors.length ?? 0) <= 1} onClick={() => floor && setConfirm({ kind: "floor", id: floor.id, name: draft.name })}>ลบชั้นนี้</button>
                </div>
              </>
            )}
          </aside>
        )}
      </div>

      <Dialog open={siteDialog !== null} onOpenChange={(open) => !open && !busy && setSiteDialog(null)}>
        <DialogContent className="topo-dialog">
          <DialogHeader><DialogTitle>{siteDialog?.id ? "แก้ไขอาคาร" : "สร้างอาคาร"}</DialogTitle><DialogDescription>แต่ละโปรเจคมีอาคาร ชั้น ห้อง และผังของตัวเอง · การแก้ไขไม่กระทบโปรเจคอื่น</DialogDescription></DialogHeader>
          {siteDialog && (
            <form onSubmit={(e) => {
              e.preventDefault();
              const name = siteDialog.name.trim();
              if (!name) return;
              setBusy(true); setError("");
              const payload = { name, description: siteDialog.description.trim(), project_id: siteDialog.project_id || null };
              (siteDialog.id ? client.post<void>(`/sites/${siteDialog.id}/update`, payload).then(() => siteDialog.id!) : client.post<Site>("/sites", payload).then((s) => s.id))
                .then(async (id) => { setSiteDialog(null); await loadSites(id); if (siteDialog.id) setSite((s) => (s ? { ...s, ...payload } : s)); setNotice(siteDialog.id ? "บันทึกอาคารแล้ว" : "สร้างอาคารแล้ว · เริ่มวาดผนังหรือใส่ภาพผังได้เลย"); })
                .catch((err) => fail(err, "บันทึกอาคารไม่สำเร็จ")).finally(() => setBusy(false));
            }}>
              <label>ชื่ออาคาร<input autoFocus required maxLength={100} value={siteDialog.name} onChange={(e) => setSiteDialog({ ...siteDialog, name: e.target.value })} placeholder="เช่น อาคารผู้ป่วยใน A" /></label>
              <label>รายละเอียด (ไม่บังคับ)<input maxLength={300} value={siteDialog.description} onChange={(e) => setSiteDialog({ ...siteDialog, description: e.target.value })} /></label>
              <label>โปรเจค<select disabled={!siteDialog.id} value={siteDialog.project_id} onChange={(e) => setSiteDialog({ ...siteDialog, project_id: e.target.value })}><option value="">ไม่ผูกโปรเจค</option>{(snapshot?.projects ?? []).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}</select></label>
              {error && <p className="topo-warn" role="alert">{error}</p>}
              <div className="topo-dialog-actions">
                {siteDialog.id && <button type="button" className="topo-btn danger" onClick={() => { setConfirm({ kind: "site", id: siteDialog.id!, name: siteDialog.name }); setSiteDialog(null); }}>เก็บอาคารนี้</button>}
                <button type="button" className="topo-btn" disabled={busy} onClick={() => setSiteDialog(null)}>ยกเลิก</button>
                <button type="submit" className="topo-btn primary" disabled={busy || !siteDialog.name.trim()}>{siteDialog.id ? "บันทึก" : "สร้างอาคาร"}</button>
              </div>
            </form>
          )}
        </DialogContent>
      </Dialog>

      <Dialog open={floorDialog !== null} onOpenChange={(open) => !open && !busy && setFloorDialog(null)}>
        <DialogContent className="topo-dialog">
          <DialogHeader><DialogTitle>เพิ่มชั้น</DialogTitle><DialogDescription>ขนาดเป็นเมตร · คัดลอกผนังจากชั้นปัจจุบันได้เมื่อแปลนซ้ำกันทุกชั้น</DialogDescription></DialogHeader>
          {floorDialog && site && (
            <form onSubmit={(e) => {
              e.preventDefault();
              if (dirty && !window.confirm("บันทึกการแก้ไขที่ค้างอยู่ก่อนเพิ่มชั้นใหม่จะปลอดภัยกว่า ต้องการเพิ่มชั้นโดยทิ้งการแก้ไขที่ยังไม่บันทึกหรือไม่")) return;
              setBusy(true); setError("");
              const layout = floorDialog.copy && draft ? { walls: draft.layout.walls, zones: [], items: draft.layout.items.filter((i) => i.type === "stairs" || i.type === "elevator" || i.type === "exit") } : emptyLayout();
              client.post<Floor>(`/sites/${site.id}/floors`, { name: floorDialog.name.trim() || "ชั้นใหม่", level: floorDialog.level, width_m: floorDialog.width_m, depth_m: floorDialog.depth_m, ceiling_m: floorDialog.ceiling_m, revision: 0, layout: { walls: layout.walls, zones: layout.zones, items: layout.items }, placements: [] })
                .then(async (created) => { setFloorDialog(null); adoptSite(await client.raw<Site>(`/sites/${site.id}`), created.id); setNotice("เพิ่มชั้นแล้ว"); })
                .catch((err) => fail(err, "เพิ่มชั้นไม่สำเร็จ")).finally(() => setBusy(false));
            }}>
              <div className="topo-form-grid">
                <label>ชื่อชั้น<input autoFocus required maxLength={80} value={floorDialog.name} onChange={(e) => setFloorDialog({ ...floorDialog, name: e.target.value })} /></label>
                <label>ลำดับชั้น (level)<input type="number" min={-20} max={200} value={floorDialog.level} onChange={(e) => setFloorDialog({ ...floorDialog, level: Math.round(num(e.target.value, -20, 200, 1)) })} /></label>
                <label>กว้าง (ม.)<input type="number" min={2} max={1000} value={floorDialog.width_m} onChange={(e) => setFloorDialog({ ...floorDialog, width_m: num(e.target.value, 2, 1000, 30) })} /></label>
                <label>ลึก (ม.)<input type="number" min={2} max={1000} value={floorDialog.depth_m} onChange={(e) => setFloorDialog({ ...floorDialog, depth_m: num(e.target.value, 2, 1000, 20) })} /></label>
              </div>
              <label className="topo-check"><input type="checkbox" checked={floorDialog.copy} disabled={!draft || draft.layout.walls.length === 0} onChange={(e) => setFloorDialog({ ...floorDialog, copy: e.target.checked })} /><span><strong>คัดลอกผนัง บันได ลิฟต์ และทางหนีไฟจากชั้นปัจจุบัน</strong><small>โซนและอุปกรณ์ไม่ถูกคัดลอก</small></span></label>
              {error && <p className="topo-warn" role="alert">{error}</p>}
              <div className="topo-dialog-actions"><button type="button" className="topo-btn" disabled={busy} onClick={() => setFloorDialog(null)}>ยกเลิก</button><button type="submit" className="topo-btn primary" disabled={busy}>เพิ่มชั้น</button></div>
            </form>
          )}
        </DialogContent>
      </Dialog>

      <DxfImportDialog
        file={dxf?.file ?? null}
        token={dxf?.token ?? 0}
        draft={draft}
        onClose={() => setDxf(null)}
        onApply={({ draft: next, notice: message }) => {
          setDxf(null);
          if (!draft) return;
          change(next, { transient: false, base: draft }); // one undo step for the whole import
          setSelection(null);
          setBoxes((all) => ({ ...all, [floorId]: fitBox(next) }));
          setResetKey((k) => k + 1);
          setNotice(message);
        }}
      />

      <AlertDialog open={confirm !== null} onOpenChange={(open) => !open && setConfirm(null)}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{confirm?.kind === "site" ? `เก็บอาคาร “${confirm.name}”?` : `ลบชั้น “${confirm?.name}”?`}</AlertDialogTitle><AlertDialogDescription>{confirm?.kind === "site" ? "อาคารจะหายจากรายการ และอุปกรณ์ที่วางไว้จะกลับไปอยู่ในรายการยังไม่ได้วาง · gateway และอุปกรณ์ไม่ถูกลบ" : "ผนัง โซน และตำแหน่งอุปกรณ์ของชั้นนี้จะถูกลบถาวร · อุปกรณ์เองไม่ถูกลบ"}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction className="is-danger" onClick={() => {
              const c = confirm!; setConfirm(null); setBusy(true);
              (c.kind === "site" ? client.post<void>(`/sites/${c.id}/archive`, {}).then(() => loadSites()) : client.post<void>(`/floors/${c.id}/delete`, {}).then(async () => { if (site) adoptSite(await client.raw<Site>(`/sites/${site.id}`)); }))
                .then(() => setNotice(c.kind === "site" ? "เก็บอาคารแล้ว" : "ลบชั้นแล้ว")).catch((err) => fail(err, "ดำเนินการไม่สำเร็จ")).finally(() => setBusy(false));
            }}>{confirm?.kind === "site" ? "เก็บอาคาร" : "ลบชั้น"}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
