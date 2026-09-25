"use client";
import { type DragEvent, type KeyboardEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Background,
  BackgroundVariant,
  type Connection,
  Controls,
  type Edge,
  type EdgeChange,
  type IsValidConnection,
  MiniMap,
  type NodeChange,
  ReactFlow,
  ReactFlowProvider,
  type XYPosition,
  useReactFlow,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { CheckCircle2, CirclePlay, GripVertical, History, PanelLeft, PanelRight, Plus, Save, Trash2, TriangleAlert, Workflow } from "lucide-react";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import "./automation.css";
import { ApiError, createClientFrom, type ControlFeature, type Device, type Gateway, type Source } from "../topology/api";
import { useLatest } from "../topology/use-latest";
import { formatMAC } from "../topology/catalog";
import {
  COMMAND_REASON,
  EMPTY_DEFINITION,
  EVENT_LABEL,
  METRIC_LABEL,
  OP_LABEL,
  SEVERITY_LABEL,
  type Automation,
  type Block,
  type BlockData,
  type BlockType,
  type Channel,
  type CommandableDevice,
  DAY_LABEL,
  type Link,
  type Lookup,
  type Op,
  type Outcome,
  type Problem,
  type Run,
  type StudioCatalog,
  type Trace,
  type TraceAction,
  formatWhen,
  groupOf,
  localId,
  metricUnit,
  reaches,
  showValue,
} from "./api";
import { CATALOG, GROUPS, GROUP_COLOR, GROUP_LABEL, SPEC, TEMPLATES, newBlock, nodeTypes, sentence, summarize, type StudioNode, type StudioNodeData } from "./blocks";

const DRAG_MIME = "application/x-aether-automation";
const METRIC_OPTIONS = ["temperature", "humidity", "battery", "rssi"];
const EDGE_LABEL: Record<string, string> = { true: "ใช่", false: "ไม่ใช่" };

type Props = { getToken: () => string; refresh: () => Promise<boolean>; onUnauthorized?: () => void };

export default function AutomationStudio(props: Props) {
  return (
    <ReactFlowProvider>
      <Studio {...props} />
    </ReactFlowProvider>
  );
}

function Studio({ getToken, refresh, onUnauthorized }: Props) {
  const handlers = useLatest({ getToken, refresh });
  const [client] = useState(() => createClientFrom(handlers));
  const { screenToFlowPosition, fitView } = useReactFlow();
  const canvasRef = useRef<HTMLDivElement>(null);

  const [project, setProject] = useState("all");
  const [projects, setProjects] = useState<{ id: string; name: string }[]>([]);
  const [flowProject, setFlowProject] = useState("");
  const [flows, setFlows] = useState<Automation[]>([]);
  const [activeId, setActiveId] = useState<string>("");
  const [record, setRecord] = useState<Automation | null>(null);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [enabled, setEnabled] = useState(false);
  const [blocks, setBlocks] = useState<Block[]>([]);
  const [links, setLinks] = useState<Link[]>([]);
  const [dirty, setDirty] = useState(false);
  const [selectedId, setSelectedId] = useState<string>("");
  const [problems, setProblems] = useState<Problem[]>([]);
  const [trace, setTrace] = useState<Trace | null>(null);
  const [runs, setRuns] = useState<Run[]>([]);
  const [panels, setPanels] = useState({ palette: true, inspector: true });
  const [drawer, setDrawer] = useState<"" | "runs" | "test">("");
  const [test, setTest] = useState({ external: "", mode: "event" as "event" | "metric", event: "tamper", value: "20", action: "" });
  const [catalog, setCatalog] = useState<StudioCatalog>({});
  const [commandable, setCommandable] = useState<CommandableDevice[]>([]);

  const [devices, setDevices] = useState<Device[]>([]);
  const [gateways, setGateways] = useState<Gateway[]>([]);
  const [channels, setChannels] = useState<Channel[]>([]);
  const [sources, setSources] = useState<Source[]>([]);

  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const unauthorized = useRef(false);

  const guard = useCallback(
    async <T,>(work: () => Promise<T>): Promise<T | null> => {
      try {
        return await work();
      } catch (e) {
        if (e instanceof ApiError && e.status === 401 && !unauthorized.current) {
          unauthorized.current = true;
          onUnauthorized?.();
        }
        setError(e instanceof Error ? e.message : "ดำเนินการไม่สำเร็จ");
        return null;
      }
    },
    [onUnauthorized],
  );

  // --- reference data ---------------------------------------------------------------------------
  const loadReference = useCallback(async () => {
    const settle = async <T,>(p: Promise<T>, fallback: T): Promise<T> => p.catch(() => fallback);
    const [d, g, c, s] = await Promise.all([
      settle(client.raw<{ items: Device[] }>("/devices"), { items: [] }),
      settle(client.raw<{ items: Gateway[] }>("/gateways"), { items: [] }),
      settle(client.raw<{ items: Channel[] }>("/channels"), { items: [] }),
      settle(client.raw<{ items: Source[] }>("/studio/sources"), { items: [] }),
    ]);
    setProjects((await client.raw<{ items: { id: string; name: string }[] }>("/projects")).items);
    setDevices(d.items);
    setGateways(g.items);
    setChannels(c.items);
    setSources(s.items);
  }, [client]);

  const loadFlows = useCallback(async () => {
    const out = await guard(() => client.raw<{ items: Automation[]; catalog?: StudioCatalog }>("/automations"));
    if (out) {
      setFlows(out.items);
      setCatalog(out.catalog ?? {});
    }
    return out?.items ?? [];
  }, [client, guard]);

  useEffect(() => {
    let active = true;
    const boot = async () => {
      await loadReference();
      const items = await loadFlows();
      if (active && items.length > 0) setActiveId(items[0].id);
    };
    void boot();
    return () => {
      active = false;
    };
  }, [loadReference, loadFlows]);

  const applyRecord = useCallback((flow: Automation) => {
    const def = flow.definition ?? EMPTY_DEFINITION;
    setFlowProject(flow.project_id ?? "");
    setRecord(flow);
    setName(flow.name);
    setDescription(flow.description ?? "");
    setEnabled(flow.enabled);
    setBlocks(def.nodes ?? []);
    setLinks(def.edges ?? []);
    setProblems([]);
    setTrace(null);
    setSelectedId("");
    setDirty(false);
  }, []);

  // Loading a flow replaces the canvas; the switch itself is guarded before activeId changes, and
  // clearing the canvas when the last flow is deleted is done there, not here.
  useEffect(() => {
    if (!activeId) return;
    let cancelled = false;
    const open = async () => {
      const flow = await guard(() => client.raw<Automation>(`/automations/${activeId}`));
      if (!flow || cancelled) return;
      applyRecord(flow);
      window.setTimeout(() => fitView({ padding: 0.25, duration: 300 }), 60);
    };
    void open();
    return () => {
      cancelled = true;
    };
  }, [activeId, client, guard, applyRecord, fitView]);

  // The devices a command block of this flow may target: its project's, or the workspace's for an unscoped flow.
  useEffect(() => {
    let cancelled = false;
    const query = flowProject ? `?project_id=${encodeURIComponent(flowProject)}` : "";
    void client
      .raw<{ items: CommandableDevice[] }>(`/automations/commandable${query}`)
      .then((out) => {
        if (!cancelled) setCommandable(out.items);
      })
      .catch(() => {
        if (!cancelled) setCommandable([]);
      });
    return () => {
      cancelled = true;
    };
  }, [client, flowProject]);

  // Unsaved-changes guard for a reload or a closed tab.
  useEffect(() => {
    if (!dirty) return;
    const onLeave = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", onLeave);
    return () => window.removeEventListener("beforeunload", onLeave);
  }, [dirty]);

  const dirtyRef = useLatest(dirty);
  const confirmLeave = useCallback(() => !dirtyRef.current || window.confirm("ผังนี้ยังไม่ได้บันทึก · ออกจากผังนี้แล้วจะเสียการแก้ไข"), [dirtyRef]);

  // --- lookups ----------------------------------------------------------------------------------
  const lookup = useMemo<Lookup>(() => {
    const deviceNames = new Map<string, string>();
    for (const s of sources) deviceNames.set(s.id.toLowerCase(), s.name);
    for (const d of devices) deviceNames.set(d.external_id.toLowerCase(), d.name);
    const gatewayNames = new Map(gateways.map((g) => [g.id, g.name]));
    const channelNames = new Map(channels.map((c) => [c.id, c.name]));
    const targetNames = new Map<string, string>();
    for (const d of devices) if (d.id) targetNames.set(d.id.toLowerCase(), d.name);
    for (const c of commandable) targetNames.set(c.device_id.toLowerCase(), c.name);
    return {
      device: (id) => deviceNames.get(id.toLowerCase()) ?? (id ? formatMAC(id) : "—"),
      gateway: (id) => gatewayNames.get(id) ?? "gateway ที่ถูกถอนแล้ว",
      channel: (id) => channelNames.get(id) ?? "ช่องทางที่ถูกลบแล้ว",
      target: (id) => targetNames.get(id.toLowerCase()) ?? "อุปกรณ์ที่ถูกถอนแล้ว",
    };
  }, [devices, sources, gateways, channels, commandable]);

  /** Every identity the workspace knows, registered first, for the device pickers. */
  const identities = useMemo(() => {
    const seen = new Map<string, { id: string; label: string }>();
    for (const d of devices.filter((d) => !flowProject || gateways.some((g) => g.id === d.gateway_id && g.project_id === flowProject))) seen.set(d.external_id.toLowerCase(), { id: d.external_id.toLowerCase(), label: `${d.name} · ${formatMAC(d.external_id)}` });
    for (const s of sources.filter((s) => !flowProject || gateways.some((g) => g.id === s.gatewayID && g.project_id === flowProject))) if (!seen.has(s.id.toLowerCase())) seen.set(s.id.toLowerCase(), { id: s.id.toLowerCase(), label: `${s.name} · ${formatMAC(s.id)}` });
    return [...seen.values()].sort((a, b) => a.label.localeCompare(b.label, "th"));
  }, [devices, sources, gateways, flowProject]);

  const problemByNode = useMemo(() => {
    const out = new Map<string, string>();
    for (const p of problems) if (p.node_id) out.set(p.node_id, out.has(p.node_id) ? `${out.get(p.node_id)} · ${p.message}` : p.message);
    return out;
  }, [problems]);
  const generalProblems = useMemo(() => problems.filter((p) => !p.node_id), [problems]);

  const blockById = useMemo(() => new Map(blocks.map((b) => [b.id, b])), [blocks]);
  const selected = selectedId ? (blockById.get(selectedId) ?? null) : null;

  // React Flow hides a controlled node until it has a measured size. The node objects are rebuilt whenever a flow
  // is reloaded (save, enable), so the last measured size is carried over; otherwise the blocks stay invisible.
  const [sizes, setSizes] = useState<Record<string, { width: number; height: number }>>({});
  const nodes = useMemo<StudioNode[]>(
    () =>
      blocks.map((b) => ({
        id: b.id,
        measured: sizes[b.id],
        type: "block" as const,
        position: b.position,
        selected: b.id === selectedId,
        data: { block: b, lookup, outcome: trace?.nodes?.[b.id] ?? null, problem: problemByNode.get(b.id) ?? "" } satisfies StudioNodeData,
      })),
    [blocks, selectedId, lookup, trace, problemByNode, sizes],
  );

  const edges = useMemo<Edge[]>(
    () =>
      links.map((l) => {
        const outcome: Outcome | null = trace?.nodes?.[l.source] ?? null;
        const live = outcome !== null && (l.sourceHandle ? outcome === l.sourceHandle : outcome === "true");
        return {
          id: l.id,
          source: l.source,
          target: l.target,
          sourceHandle: l.sourceHandle,
          type: "smoothstep",
          animated: true,
          label: l.sourceHandle ? EDGE_LABEL[l.sourceHandle] : undefined,
          className: `au-edge ${l.sourceHandle ? `is-${l.sourceHandle}` : ""} ${live ? "is-live" : ""}`,
        };
      }),
    [links, trace],
  );

  // --- graph editing ----------------------------------------------------------------------------
  const touch = useCallback(() => {
    setDirty(true);
    setTrace(null);
  }, []);

  const removeBlocks = useCallback(
    (ids: string[]) => {
      const gone = new Set(ids);
      setBlocks((list) => list.filter((b) => !gone.has(b.id)));
      setLinks((list) => list.filter((l) => !gone.has(l.source) && !gone.has(l.target)));
      setSelectedId((current) => (gone.has(current) ? "" : current));
      touch();
    },
    [touch],
  );

  const onNodesChange = useCallback(
    (changes: NodeChange<StudioNode>[]) => {
      const removed: string[] = [];
      const moved = new Map<string, XYPosition>();
      for (const c of changes) {
        if (c.type === "dimensions" && c.dimensions) {
          const d = c.dimensions;
          setSizes((all) => (all[c.id]?.width === d.width && all[c.id]?.height === d.height ? all : { ...all, [c.id]: { width: d.width, height: d.height } }));
        }
        if (c.type === "position" && c.position) moved.set(c.id, c.position);
        else if (c.type === "remove") removed.push(c.id);
      }
      if (moved.size > 0) {
        setBlocks((list) => list.map((b) => (moved.has(b.id) ? { ...b, position: moved.get(b.id)! } : b)));
        setDirty(true);
      }
      if (removed.length > 0) removeBlocks(removed);
    },
    [removeBlocks],
  );

  const onEdgesChange = useCallback(
    (changes: EdgeChange<Edge>[]) => {
      const removed = new Set(changes.filter((c) => c.type === "remove").map((c) => c.id));
      if (removed.size === 0) return;
      setLinks((list) => list.filter((l) => !removed.has(l.id)));
      touch();
    },
    [touch],
  );

  const isValidConnection = useCallback<IsValidConnection>(
    (c) => {
      if (!c.source || !c.target || c.source === c.target) return false;
      const src = blockById.get(c.source);
      const dst = blockById.get(c.target);
      if (!src || !dst) return false;
      if (groupOf(src.type) === "action") return false;
      if (groupOf(dst.type) === "trigger") return false;
      const dstGroup = groupOf(dst.type);
      // A condition and an action have one input socket; logic blocks are the way to join several.
      if ((dstGroup === "condition" || dstGroup === "action") && links.some((l) => l.target === dst.id)) return false;
      if (links.some((l) => l.source === c.source && l.target === c.target && (l.sourceHandle ?? "") === (c.sourceHandle ?? ""))) return false;
      return !reaches(links, c.target, c.source);
    },
    [blockById, links],
  );

  const onConnect = useCallback(
    (c: Connection) => {
      if (!isValidConnection(c)) return;
      const handle = c.sourceHandle === "true" || c.sourceHandle === "false" ? c.sourceHandle : undefined;
      setLinks((list) => [...list, { id: localId("e"), source: c.source, target: c.target, sourceHandle: handle }]);
      touch();
    },
    [isValidConnection, touch],
  );

  const addBlock = useCallback(
    (type: BlockType, position: XYPosition) => {
      const block = newBlock(type, position);
      setBlocks((list) => [...list, block]);
      setSelectedId(block.id);
      setPanels((p) => ({ ...p, inspector: true }));
      touch();
    },
    [touch],
  );

  const canvasCentre = useCallback((): XYPosition => {
    const rect = canvasRef.current?.getBoundingClientRect();
    if (!rect) return { x: 0, y: 0 };
    return screenToFlowPosition({ x: rect.left + rect.width / 2 - 110, y: rect.top + rect.height / 2 - 40 });
  }, [screenToFlowPosition]);

  const onDrop = useCallback(
    (e: DragEvent) => {
      e.preventDefault();
      const raw = e.dataTransfer.getData(DRAG_MIME);
      if (!raw) return;
      const spec = SPEC[raw];
      if (!spec) return;
      addBlock(spec.type, screenToFlowPosition({ x: e.clientX - 110, y: e.clientY - 40 }));
    },
    [addBlock, screenToFlowPosition],
  );

  const patch = useCallback(
    (id: string, change: Partial<BlockData>) => {
      setBlocks((list) => list.map((b) => (b.id === id ? { ...b, data: { ...b.data, ...change } } : b)));
      touch();
    },
    [touch],
  );

  // --- server actions ---------------------------------------------------------------------------
  const definition = useMemo(() => ({ nodes: blocks, edges: links }), [blocks, links]);

  const validate = useCallback(
    async (wantEnabled: boolean): Promise<boolean> => {
      const out = await guard(() => client.post<{ ok: boolean; problems: Problem[] }>("/automations/validate", { enabled: wantEnabled, definition, project_id: flowProject || undefined }));
      if (!out) return false;
      setProblems(out.problems);
      return out.ok;
    },
    [client, definition, flowProject, guard],
  );

  const act = useCallback(
    async (work: () => Promise<void>) => {
      setBusy(true);
      setError("");
      setNotice("");
      try {
        await work();
      } finally {
        setBusy(false);
      }
    },
    [],
  );

  const save = useCallback(async () => {
    if (!record) return;
    await act(async () => {
      if (enabled && !(await validate(true))) {
        setError("ยังเปิดใช้ผังนี้ไม่ได้ · ดูบล็อกที่ขึ้นกรอบแดง");
        return;
      }
      const saved = await guard(() =>
        client.post<Automation>(`/automations/${record.id}/save`, {
          name: name.trim() || record.name,
          description,
          enabled,
          project_id: flowProject || null,
          definition,
          revision: record.revision,
        }),
      );
      if (!saved) return;
      applyRecord(saved);
      setNotice("บันทึกแล้ว");
      void loadFlows();
    });
  }, [act, applyRecord, client, definition, description, enabled, guard, loadFlows, name, record, validate, flowProject]);

  const create = useCallback(
    async (flowName: string, def: { nodes: Block[]; edges: Link[] }, note = "") => {
      await act(async () => {
        const made = await guard(() => client.post<Automation>("/automations", { name: flowName, description: note, enabled: false, project_id: project === "all" || project === "none" ? null : project, definition: def }));
        if (!made) return;
        await loadFlows();
        applyRecord(made);
        setActiveId(made.id);
        setNotice("สร้างผังใหม่แล้ว · ยังไม่เปิดใช้งาน");
      });
    },
    [act, applyRecord, client, guard, loadFlows, project],
  );

  const toggleEnabled = useCallback(async () => {
    if (!record) return;
    const next = !enabled;
    await act(async () => {
      if (dirty) {
        setError("บันทึกผังก่อนจึงจะเปิดหรือปิดใช้งานได้");
        return;
      }
      if (next && !(await validate(true))) {
        setError("ยังเปิดใช้ผังนี้ไม่ได้ · ดูบล็อกที่ขึ้นกรอบแดง");
        return;
      }
      const out = await guard(() => client.post<Automation>(`/automations/${record.id}/enable`, { enabled: next }));
      if (!out) return;
      applyRecord(out);
      setNotice(next ? "เปิดใช้งานผังแล้ว · จะทำงานกับข้อมูลที่เข้ามาถัดไป" : "ปิดใช้งานผังแล้ว");
      void loadFlows();
    });
  }, [act, applyRecord, client, dirty, enabled, guard, loadFlows, record, validate]);

  const runTest = useCallback(async () => {
    if (!record) return;
    await act(async () => {
      if (dirty) {
        setError("การทดสอบใช้ผังที่บันทึกไว้ · บันทึกก่อนแล้วลองใหม่");
        return;
      }
      const payload: Record<string, unknown> = { external_id: test.external.trim().toLowerCase() };
      if (test.mode === "event") payload.event_type = test.event;
      else payload.metric_value = Number(test.value);
      if (test.mode === "event" && test.event === "action" && test.action.trim()) payload.action = test.action.trim();
      const out = await guard(() => client.post<{ result: Trace; would_send?: TraceAction[] }>(`/automations/${record.id}/test`, payload));
      if (!out) return;
      setTrace(out.result);
      const send = (out.would_send ?? []).map((a) => `ตั้ง ${a.property} ของ ${lookup.target(a.device_id ?? "")} เป็น ${showValue(a.value)}`);
      setNotice(
        send.length > 0
          ? `ทดลองเดินผังแล้ว · ถ้าเกิดจริงจะสั่ง: ${send.join(" · ")} (ครั้งนี้ไม่ได้ส่งคำสั่งจริง)`
          : "ทดลองเดินผังแล้ว · ไม่ได้เปิดการแจ้งเตือน ส่งข้อความ หรือสั่งอุปกรณ์จริง",
      );
    });
  }, [act, client, dirty, guard, lookup, record, test]);

  const loadRuns = useCallback(async () => {
    if (!record) return;
    const out = await guard(() => client.raw<{ items: Run[] }>(`/automations/${record.id}/runs?limit=50`));
    if (out) setRuns(out.items);
  }, [client, guard, record]);

  const remove = useCallback(async () => {
    if (!record) return;
    setConfirmDelete(false);
    await act(async () => {
      const done = await guard(async () => {
        await client.post<void>(`/automations/${record.id}/delete`, {});
        return true;
      });
      if (!done) return;
      setDirty(false);
      const items = await loadFlows();
      const next = items.find((f) => f.id !== record.id);
      if (!next) {
        setRecord(null);
        setBlocks([]);
        setLinks([]);
        setName("");
        setDescription("");
        setEnabled(false);
        setProblems([]);
        setTrace(null);
        setSelectedId("");
      }
      setActiveId(next ? next.id : "");
      setNotice("ลบผังแล้ว");
    });
  }, [act, client, guard, loadFlows, record]);

  const switchFlow = useCallback(
    (id: string) => {
      if (id === activeId || !confirmLeave()) return;
      setDirty(false);
      setActiveId(id);
      setDrawer("");
    },
    [activeId, confirmLeave],
  );

  const flowSentence = useMemo(() => sentence(definition, lookup), [definition, lookup]);
  const okBadge = problems.length === 0 && blocks.length > 0;

  return (
    <div className="au">
      <div className="au-bar">
        <span className="au-bar-title">
          <Workflow size={16} aria-hidden="true" />
          ออโตเมชัน
        </span>
        <select className="au-field" aria-label="กรองโปรเจคออโตเมชัน" value={project} onChange={(e) => { if (!confirmLeave()) return; const id = e.target.value; setProject(id); const next = flows.find((f) => id === "all" || (f.project_id ?? "none") === id); setActiveId(next?.id ?? ""); if (!next) { setRecord(null); setBlocks([]); setLinks([]); setDirty(false); } }}><option value="all">ทุกโปรเจค</option><option value="none">ยังไม่จัดโปรเจค</option>{projects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}</select>
        <select className="au-field" aria-label="โปรเจคของผังนี้" value={flowProject} disabled={!record} onChange={(e) => { setFlowProject(e.target.value); setDirty(true); }}><option value="">ยังไม่จัดโปรเจค</option>{projects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}</select>
        <label className="au-field au-picker">
          <span className="au-sr">เลือกผัง</span>
          <select value={activeId} onChange={(e) => switchFlow(e.target.value)} aria-label="เลือกผังออโตเมชัน">
            {flows.length === 0 && <option value="">ยังไม่มีผัง</option>}
            {flows.filter((f) => project === "all" || (f.project_id ?? "none") === project).map((f) => (
              <option key={f.id} value={f.id}>
                {f.enabled ? "● " : "○ "}
                {f.name}
              </option>
            ))}
          </select>
        </label>
        <input
          className="au-field au-name"
          value={name}
          disabled={!record}
          placeholder="ชื่อผัง"
          aria-label="ชื่อผัง"
          onChange={(e) => {
            setName(e.target.value);
            setDirty(true);
          }}
        />
        <button type="button" className={`au-switch ${enabled ? "is-on" : ""}`} onClick={() => void toggleEnabled()} disabled={!record || busy} aria-pressed={enabled} title={enabled ? "กำลังทำงานกับข้อมูลจริง" : "ยังไม่เปิดใช้งาน"}>
          <span className="au-switch-dot" aria-hidden="true" />
          {enabled ? "เปิดใช้งาน" : "ปิดอยู่"}
        </button>
        <span className="au-bar-space" />
        <button type="button" className="au-btn" onClick={() => void act(async () => void (await validate(enabled)))} disabled={!record || busy} title="ตรวจความถูกต้องของผัง">
          <CheckCircle2 size={15} aria-hidden="true" />
          ตรวจสอบ
        </button>
        <button type="button" className={`au-btn ${drawer === "test" ? "is-on" : ""}`} onClick={() => setDrawer(drawer === "test" ? "" : "test")} disabled={!record} aria-pressed={drawer === "test"} title="ทดลองเดินผังโดยไม่ทำงานจริง">
          <CirclePlay size={15} aria-hidden="true" />
          ทดสอบ
        </button>
        <button
          type="button"
          className={`au-btn ${drawer === "runs" ? "is-on" : ""}`}
          onClick={() => {
            const next = drawer === "runs" ? "" : "runs";
            setDrawer(next);
            if (next === "runs") void loadRuns();
          }}
          disabled={!record}
          aria-pressed={drawer === "runs"}
          title="ประวัติการทำงาน"
        >
          <History size={15} aria-hidden="true" />
          ประวัติ
        </button>
        <button type="button" className="au-btn primary" onClick={() => void save()} disabled={!record || busy || !dirty} title="บันทึกผัง">
          <Save size={15} aria-hidden="true" />
          บันทึก{dirty ? " *" : ""}
        </button>
        <button type="button" className="au-icon-btn" onClick={() => confirmLeave() && void create("ผังใหม่", EMPTY_DEFINITION)} disabled={busy} aria-label="สร้างผังใหม่" title="สร้างผังใหม่">
          <Plus size={16} aria-hidden="true" />
        </button>
        <button type="button" className="au-icon-btn danger" onClick={() => setConfirmDelete(true)} disabled={!record || busy} aria-label="ลบผังนี้" title="ลบผังนี้">
          <Trash2 size={16} aria-hidden="true" />
        </button>
        <button type="button" className="au-icon-btn" onClick={() => setPanels((p) => ({ ...p, palette: !p.palette }))} aria-pressed={panels.palette} aria-label="แสดงหรือซ่อนแถบบล็อก" title="แถบบล็อก">
          <PanelLeft size={16} aria-hidden="true" />
        </button>
        <button type="button" className="au-icon-btn" onClick={() => setPanels((p) => ({ ...p, inspector: !p.inspector }))} aria-pressed={panels.inspector} aria-label="แสดงหรือซ่อนแถบตั้งค่า" title="แถบตั้งค่า">
          <PanelRight size={16} aria-hidden="true" />
        </button>
      </div>

      <div className="au-body">
        {panels.palette && <Palette onAdd={(type) => addBlock(type, canvasCentre())} />}

        <div className="au-canvas" ref={canvasRef} onDrop={onDrop} onDragOver={(e) => e.preventDefault()}>
          <div className="au-toasts">
            {error && (
              <div className="au-toast is-error" role="alert">
                <TriangleAlert size={15} aria-hidden="true" />
                {error}
                <button type="button" onClick={() => setError("")} aria-label="ปิดข้อความ">
                  ×
                </button>
              </div>
            )}
            {!error && notice && (
              <div className="au-toast is-ok" role="status">
                {notice}
                <button type="button" onClick={() => setNotice("")} aria-label="ปิดข้อความ">
                  ×
                </button>
              </div>
            )}
            {catalog.alerts_shadow && catalog.shadow_note && (
              <div className="au-toast is-warn" role="status">
                {catalog.shadow_note}
              </div>
            )}
            {generalProblems.map((p) => (
              <div key={p.code} className="au-toast is-warn" role="status">
                {p.message}
              </div>
            ))}
          </div>

          <ReactFlow<StudioNode, Edge>
            nodes={nodes}
            edges={edges}
            nodeTypes={nodeTypes}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            isValidConnection={isValidConnection}
            onNodeClick={(_, n) => {
              setSelectedId(n.id);
              setPanels((p) => ({ ...p, inspector: true }));
            }}
            onPaneClick={() => setSelectedId("")}
            connectionLineStyle={{ stroke: "var(--color-accent)", strokeWidth: 2, strokeDasharray: "6 4" }}
            defaultEdgeOptions={{ type: "smoothstep", animated: true }}
            snapToGrid
            snapGrid={[16, 16]}
            deleteKeyCode={["Delete", "Backspace"]}
            elevateNodesOnSelect={false}
            minZoom={0.25}
            maxZoom={1.8}
            fitView
          >
            <Background variant={BackgroundVariant.Dots} gap={22} size={1.2} color="var(--color-grid)" />
            <Controls showInteractive={false} position="bottom-left" />
            <MiniMap pannable zoomable position="bottom-right" nodeColor={(n) => GROUP_COLOR[groupOf((n.data as StudioNodeData).block.type)]} maskColor="rgba(8,14,16,.65)" />
          </ReactFlow>

          {flows.length === 0 && <Starters busy={busy} onInsert={(t) => confirmLeave() && void create(t.name, t.build(), t.description)} />}
          {flows.length > 0 && blocks.length === 0 && (
            <div className="au-hint">
              <strong>ผังนี้ยังว่าง</strong>
              <span>ลากบล็อกจากแถบซ้าย หรือกดที่การ์ดเพื่อวางลงกลางผัง</span>
            </div>
          )}

          {drawer === "test" && <TestPanel state={test} setState={setTest} identities={identities} busy={busy} dirty={dirty} trace={trace} onRun={() => void runTest()} onClose={() => setDrawer("")} />}
          {drawer === "runs" && <RunHistory runs={runs} lookup={lookup} onClose={() => setDrawer("")} onReload={() => void loadRuns()} />}
        </div>

        {panels.inspector && (
          <Inspector
            block={selected}
            sentence={flowSentence}
            ok={okBadge}
            identities={identities}
            gateways={gateways}
            channels={channels}
            commandable={commandable}
            catalog={catalog}
            lookup={lookup}
            problem={selected ? (problemByNode.get(selected.id) ?? "") : ""}
            onPatch={(change) => selected && patch(selected.id, change)}
            onRemove={() => selected && removeBlocks([selected.id])}
          />
        )}
      </div>

      <AlertDialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>ลบผัง “{record?.name}” ?</AlertDialogTitle>
            <AlertDialogDescription>ผังและประวัติการทำงานทั้งหมดจะถูกลบถาวร การแจ้งเตือนที่เคยเปิดไว้แล้วยังอยู่</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction onClick={() => void remove()}>ลบผัง</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/** Enter and Space add a block the same way a click does, so drag is never the only way in. */
const activate = (fn: () => void) => (e: KeyboardEvent<HTMLElement>) => {
  if (e.key === "Enter" || e.key === " ") {
    e.preventDefault();
    fn();
  }
};

function Palette({ onAdd }: { onAdd: (type: BlockType) => void }) {
  return (
    <aside className="au-palette" aria-label="บล็อกที่ลากวางได้">
      {GROUPS.map((group) => (
        <section key={group}>
          <h2 style={{ ["--au-block" as string]: GROUP_COLOR[group] }}>{GROUP_LABEL[group]}</h2>
          {CATALOG.filter((s) => s.group === group).map((spec) => {
            const Icon = spec.icon;
            return (
              <div
                key={spec.type}
                className={`au-card ${spec.unavailable ? "is-unavailable" : ""}`}
                draggable={!spec.unavailable}
                role="button"
                tabIndex={0}
                aria-disabled={spec.unavailable ? true : undefined}
                aria-label={spec.unavailable ? `${spec.label} · ${spec.unavailable}` : `เพิ่มบล็อก ${spec.label}`}
                title={spec.unavailable ?? spec.hint}
                style={{ ["--au-block" as string]: spec.unavailable ? "#9fb1b6" : GROUP_COLOR[group] }}
                onDragStart={(e) => {
                  e.dataTransfer.setData(DRAG_MIME, spec.type);
                  e.dataTransfer.effectAllowed = "copy";
                }}
                onClick={() => !spec.unavailable && onAdd(spec.type)}
                onKeyDown={activate(() => !spec.unavailable && onAdd(spec.type))}
              >
                <GripVertical size={13} className="au-grip" aria-hidden="true" />
                <span className="au-card-icon">
                  <Icon size={16} />
                </span>
                <span>
                  <strong>{spec.label}</strong>
                  <small>{spec.unavailable ?? spec.hint}</small>
                </span>
              </div>
            );
          })}
        </section>
      ))}
    </aside>
  );
}

function Starters({ busy, onInsert }: { busy: boolean; onInsert: (template: (typeof TEMPLATES)[number]) => void }) {
  return (
    <div className="au-starters">
      <h2>เริ่มจากตัวอย่าง</h2>
      <p>เลือกหนึ่งผังเพื่อสร้างเป็นร่าง แล้วแก้อุปกรณ์กับช่องทางให้ตรงกับหน้างาน ผังใหม่จะยังไม่เปิดใช้งานจนกว่าจะกดเปิดเอง</p>
      <div className="au-starter-grid">
        {TEMPLATES.map((t) => {
          const Icon = t.icon;
          return (
            <button key={t.id} type="button" className="au-starter" onClick={() => onInsert(t)} disabled={busy}>
              <span className="au-starter-icon">
                <Icon size={18} aria-hidden="true" />
              </span>
              <strong>{t.name}</strong>
              <small>{t.description}</small>
            </button>
          );
        })}
      </div>
    </div>
  );
}

type Identity = { id: string; label: string };

function Inspector({
  block,
  sentence: flowSentence,
  ok,
  identities,
  gateways,
  channels,
  commandable,
  catalog,
  lookup,
  problem,
  onPatch,
  onRemove,
}: {
  block: Block | null;
  sentence: string;
  ok: boolean;
  identities: Identity[];
  gateways: Gateway[];
  channels: Channel[];
  commandable: CommandableDevice[];
  catalog: StudioCatalog;
  lookup: Lookup;
  problem: string;
  onPatch: (change: Partial<BlockData>) => void;
  onRemove: () => void;
}) {
  if (!block) {
    return (
      <aside className="au-inspector" aria-label="สรุปผัง">
        <h2>ผังนี้ทำอะไร</h2>
        {flowSentence ? <p className="au-sentence">{flowSentence}</p> : <p className="au-muted">ยังไม่มีบล็อกในผัง · เลือกบล็อกจากแถบซ้ายเพื่อเริ่ม</p>}
        {flowSentence && <p className={`au-verdict ${ok ? "is-ok" : ""}`}>{ok ? "ผังนี้ถูกต้อง พร้อมเปิดใช้งาน" : "กด “ตรวจสอบ” เพื่อดูว่ายังขาดอะไร"}</p>}
        <p className="au-muted au-note">คลิกบล็อกบนผังเพื่อตั้งค่า · ลากจากขาขวาของบล็อกไปยังขาซ้ายของอีกบล็อกเพื่อเชื่อม · กด Delete เพื่อลบสิ่งที่เลือก</p>
      </aside>
    );
  }
  const spec = SPEC[block.type];
  const d = block.data ?? {};
  const group = spec?.group ?? "action";
  return (
    <aside className="au-inspector" aria-label={`ตั้งค่าบล็อก ${spec?.label ?? block.type}`}>
      <div className="au-inspector-head" style={{ ["--au-block" as string]: GROUP_COLOR[group] }}>
        <h2>{spec?.label ?? block.type}</h2>
        <button type="button" className="au-icon-btn danger" onClick={onRemove} aria-label="ลบบล็อกนี้" title="ลบบล็อกนี้">
          <Trash2 size={15} aria-hidden="true" />
        </button>
      </div>
      {problem && (
        <p className="au-inspector-error" role="alert">
          {problem}
        </p>
      )}

      <label className="au-label">
        ชื่อที่จะแสดงบนบล็อก
        <input value={d.label ?? ""} placeholder={spec?.label ?? ""} onChange={(e) => onPatch({ label: e.target.value })} />
      </label>

      {block.type === "trigger.event" && (
        <>
          <fieldset className="au-checks">
            <legend>เหตุการณ์ที่จะเฝ้าดู</legend>
            {Object.keys(EVENT_LABEL).map((key) => (
              <label key={key} className="au-check">
                <input
                  type="checkbox"
                  checked={(d.event_types ?? []).includes(key)}
                  onChange={(e) => onPatch({ event_types: e.target.checked ? [...(d.event_types ?? []), key] : (d.event_types ?? []).filter((t) => t !== key) })}
                />
                {EVENT_LABEL[key]}
              </label>
            ))}
          </fieldset>
          {(d.event_types ?? []).includes("action") && (
            <label className="au-label">
              เฉพาะปุ่มที่กด (คั่นด้วย , · ว่าง = ทุกปุ่ม)
              <input
                value={(d.actions ?? []).join(", ")}
                placeholder="เช่น single, double, on"
                onChange={(e) => onPatch({ actions: e.target.value.split(",").map((x) => x.trim()).filter(Boolean) })}
              />
              <small>ชื่อปุ่มตามที่ Zigbee2MQTT รายงานในค่า action ของรีโมตหรือปุ่มนั้น</small>
            </label>
          )}
          <MultiSelect label="เฉพาะอุปกรณ์ (ว่าง = ทุกตัว)" options={identities} value={d.external_ids ?? []} onChange={(external_ids) => onPatch({ external_ids })} />
          <MultiSelect label="เฉพาะ gateway (ว่าง = ทุกตัว)" options={gateways.map((g) => ({ id: g.id, label: g.name }))} value={d.gateway_ids ?? []} onChange={(gateway_ids) => onPatch({ gateway_ids })} />
        </>
      )}

      {(block.type === "trigger.metric" || block.type === "cond.device") && (
        <>
          <DevicePicker label="อุปกรณ์" options={identities} value={d.external_id ?? ""} onChange={(external_id) => onPatch({ external_id })} />
          <label className="au-label">
            ค่าที่ดู
            <select value={d.metric ?? "temperature"} onChange={(e) => onPatch({ metric: e.target.value })}>
              {METRIC_OPTIONS.map((m) => (
                <option key={m} value={m}>
                  {METRIC_LABEL[m]}
                </option>
              ))}
            </select>
          </label>
          <div className="au-row">
            <label className="au-label">
              เงื่อนไข
              <select value={d.op ?? ">"} onChange={(e) => onPatch({ op: e.target.value as Op })}>
                {(Object.keys(OP_LABEL) as Op[]).map((op) => (
                  <option key={op} value={op}>
                    {op} {OP_LABEL[op]}
                  </option>
                ))}
              </select>
            </label>
            <label className="au-label">
              ค่า ({metricUnit(d.metric ?? "temperature") || "หน่วย"})
              <input type="number" value={d.value ?? 0} onChange={(e) => onPatch({ value: Number(e.target.value) })} />
            </label>
          </div>
          {block.type === "trigger.metric" ? (
            <label className="au-label">
              ต้องเป็นจริงต่อเนื่อง (วินาที)
              <input type="number" min={0} max={3600} value={d.for_sec ?? 0} onChange={(e) => onPatch({ for_sec: Number(e.target.value) })} />
              <small>0 = แจ้งทันทีที่ผ่านเกณฑ์ · ตั้งไว้เพื่อกันสัญญาณแกว่ง</small>
            </label>
          ) : (
            <label className="au-label">
              ใช้ค่าล่าสุดที่เก่าไม่เกิน (วินาที)
              <input type="number" min={0} max={86400} value={d.max_age_sec ?? 300} onChange={(e) => onPatch({ max_age_sec: Number(e.target.value) })} />
              <small>ถ้าค่าล่าสุดเก่ากว่านี้ ถือว่าเงื่อนไขไม่เป็นจริง</small>
            </label>
          )}
        </>
      )}

      {block.type === "cond.time" && (
        <>
          <div className="au-row">
            <label className="au-label">
              ตั้งแต่
              <input type="time" value={d.from ?? "08:00"} onChange={(e) => onPatch({ from: e.target.value })} />
            </label>
            <label className="au-label">
              ถึง
              <input type="time" value={d.to ?? "17:00"} onChange={(e) => onPatch({ to: e.target.value })} />
            </label>
          </div>
          <div className="au-days" role="group" aria-label="วันในสัปดาห์">
            {DAY_LABEL.map((label, index) => {
              const on = (d.days ?? []).includes(index);
              return (
                <button key={label} type="button" className={`au-day ${on ? "is-on" : ""}`} aria-pressed={on} onClick={() => onPatch({ days: on ? (d.days ?? []).filter((x) => x !== index) : [...(d.days ?? []), index].sort((a, b) => a - b) })}>
                  {label}
                </button>
              );
            })}
          </div>
          <p className="au-muted au-note">อ่านเวลาตามโซน Asia/Bangkok · ถ้าเวลาเริ่มมากกว่าเวลาสิ้นสุด จะหมายถึงช่วงข้ามคืน เช่น 22:00–06:00</p>
        </>
      )}

      {block.type === "cond.zone" && (
        <>
          <DevicePicker label="wearable ที่จะตรวจโซน" options={identities} value={d.external_id ?? ""} onChange={(external_id) => onPatch({ external_id })} />
          <MultiSelect label="โซนที่ยอมรับ (ว่าง = โซนใดก็ได้)" options={gateways.map((g) => ({ id: g.id, label: g.name }))} value={d.gateway_ids ?? []} onChange={(gateway_ids) => onPatch({ gateway_ids })} />
        </>
      )}

      {block.type === "action.alert" && (
        <>
          <label className="au-label">
            ระดับความรุนแรง
            <select value={d.severity ?? "warning"} onChange={(e) => onPatch({ severity: e.target.value as BlockData["severity"] })}>
              {Object.keys(SEVERITY_LABEL).map((s) => (
                <option key={s} value={s}>
                  {SEVERITY_LABEL[s]}
                </option>
              ))}
            </select>
          </label>
          <TemplateField label="หัวเรื่องการแจ้งเตือน" value={d.title ?? ""} onChange={(title) => onPatch({ title })} />
        </>
      )}

      {block.type === "action.notify" && (
        <>
          <MultiSelect label="ช่องทางที่จะส่ง" options={channels.filter((c) => c.enabled).map((c) => ({ id: c.id, label: `${c.name} · ${c.kind}` }))} value={d.channel_ids ?? []} onChange={(channel_ids) => onPatch({ channel_ids })} />
          <TemplateField label="ข้อความ" value={d.message ?? ""} onChange={(message) => onPatch({ message })} multiline />
          <p className="au-muted au-note">ถ้าผังนี้ไม่มีบล็อก “เปิดการแจ้งเตือน” ระบบจะเปิดการแจ้งเตือนระดับข้อมูลให้หนึ่งรายการ เพื่อให้ข้อความมีรายการอ้างอิงและตรวจสอบย้อนหลังได้</p>
        </>
      )}

      {block.type === "action.command" && <CommandEditor data={d} commandable={commandable} catalog={catalog} onPatch={onPatch} />}

      {(group === "logic" || block.type === "action.command") && <p className="au-muted au-note">{summarize(block, lookup)}</p>}
    </aside>
  );
}

/** The "สั่งอุปกรณ์" block: pick a commandable device, one property its definition lets Aether set, and the value. */
function CommandEditor({ data: d, commandable, catalog, onPatch }: { data: BlockData; commandable: CommandableDevice[]; catalog: StudioCatalog; onPatch: (change: Partial<BlockData>) => void }) {
  const device = commandable.find((c) => c.device_id.toLowerCase() === (d.device_id ?? "").toLowerCase());
  const feature = device?.features.find((f) => f.property === d.property);
  return (
    <>
      {!catalog.automation_commands && (
        <p className="au-unavailable">
          <TriangleAlert size={15} aria-hidden="true" />
          {catalog.command_note || "ระบบนี้ปิดการสั่งอุปกรณ์จากผังอัตโนมัติไว้ · บันทึกเป็นฉบับร่างได้ แต่ยังเปิดใช้ไม่ได้"}
        </p>
      )}
      {catalog.automation_commands && catalog.can_command === false && (
        <p className="au-unavailable">
          <TriangleAlert size={15} aria-hidden="true" />
          บัญชีนี้ไม่มีสิทธิ์สั่งงานอุปกรณ์ · วางและบันทึกบล็อกได้ แต่เปิดใช้ผังนี้ไม่ได้
        </p>
      )}
      <label className="au-label">
        อุปกรณ์ที่จะสั่ง
        <select value={d.device_id ?? ""} onChange={(e) => onPatch({ device_id: e.target.value, property: "", set_value: undefined })}>
          <option value="">— เลือกอุปกรณ์ —</option>
          {d.device_id && !device && <option value={d.device_id}>อุปกรณ์เดิม (ไม่อยู่ในรายการที่สั่งได้แล้ว)</option>}
          {commandable.map((c) => (
            <option key={c.device_id} value={c.device_id}>
              {c.name}
            </option>
          ))}
        </select>
        {commandable.length === 0 && <small>ยังไม่มีอุปกรณ์ Zigbee2MQTT ที่ลงทะเบียนและสั่งงานได้ในโปรเจกต์ของผังนี้</small>}
      </label>
      {device && (
        <label className="au-label">
          ค่าที่จะตั้ง
          <select value={d.property ?? ""} onChange={(e) => onPatch({ property: e.target.value, set_value: undefined })}>
            <option value="">— เลือกค่า —</option>
            {device.features.map((f) => (
              <option key={f.property} value={f.property}>
                {f.label || f.name || f.property}
                {f.endpoint ? ` (${f.endpoint})` : ""}
              </option>
            ))}
          </select>
        </label>
      )}
      {feature && <ValueEditor feature={feature} value={d.set_value} onChange={(set_value) => onPatch({ set_value })} />}
      <p className="au-muted au-note">
        ผังสั่งด้วยค่าที่ระบุเสมอ (ไม่มี “สลับ”) · ผ่านคิวคำสั่งเดียวกับหน้าเว็บ ในนามของผู้ที่เปิดใช้ผัง · ไม่ส่งซ้ำเมื่อเหตุการณ์มาจากคำสั่งของผังอัตโนมัติเอง · อุปกรณ์หนึ่งตัวไม่เกิน 6 ครั้งต่อนาที และผังทั้งหมดรวมกันไม่เกิน 30 ครั้งต่อนาที
      </p>
    </>
  );
}

/** One value input for a settable Zigbee2MQTT feature, by its type. */
function ValueEditor({ feature: f, value, onChange }: { feature: ControlFeature; value: unknown; onChange: (value: unknown) => void }) {
  if (f.type === "binary") {
    const options = [f.value_on, f.value_off].filter((v) => v !== undefined);
    return (
      <label className="au-label">
        ตั้งเป็น
        <select value={value === undefined ? "" : JSON.stringify(value)} onChange={(e) => onChange(e.target.value ? JSON.parse(e.target.value) : undefined)}>
          <option value="">— เลือก —</option>
          {options.map((v) => (
            <option key={JSON.stringify(v)} value={JSON.stringify(v)}>
              {String(v)}
            </option>
          ))}
        </select>
      </label>
    );
  }
  if (f.type === "enum") {
    return (
      <label className="au-label">
        ตั้งเป็น
        <select value={value === undefined ? "" : JSON.stringify(value)} onChange={(e) => onChange(e.target.value ? JSON.parse(e.target.value) : undefined)}>
          <option value="">— เลือก —</option>
          {(f.values ?? []).map((v) => (
            <option key={JSON.stringify(v)} value={JSON.stringify(v)}>
              {String(v)}
            </option>
          ))}
        </select>
      </label>
    );
  }
  if (f.type === "numeric") {
    return (
      <label className="au-label">
        ตั้งเป็น{f.unit ? ` (${f.unit})` : ""}
        <input
          type="number"
          min={f.value_min}
          max={f.value_max}
          step={f.value_step ?? "any"}
          value={typeof value === "number" ? value : ""}
          onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))}
        />
        {(f.value_min !== undefined || f.value_max !== undefined) && (
          <small>
            ช่วงที่อุปกรณ์รับได้ {f.value_min ?? "…"}–{f.value_max ?? "…"}
          </small>
        )}
      </label>
    );
  }
  // A composite (colour and the like): its sub-values as JSON, checked against the device when saved.
  return (
    <label className="au-label">
      ตั้งเป็น (JSON)
      <input
        defaultValue={value === undefined ? "" : JSON.stringify(value)}
        placeholder={JSON.stringify(Object.fromEntries((f.features ?? []).map((x) => [x.property, 0])))}
        onBlur={(e) => {
          try {
            onChange(e.target.value.trim() ? JSON.parse(e.target.value) : undefined);
          } catch {
            onChange(e.target.value);
          }
        }}
      />
      <small>ค่าย่อย: {(f.features ?? []).map((x) => x.property).join(", ")}</small>
    </label>
  );
}

function DevicePicker({ label, options, value, onChange }: { label: string; options: Identity[]; value: string; onChange: (id: string) => void }) {
  return (
    <label className="au-label">
      {label}
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">— เลือกอุปกรณ์ —</option>
        {value && !options.some((o) => o.id === value) && <option value={value}>{value} (ไม่พบในรายการแล้ว)</option>}
        {options.map((o) => (
          <option key={o.id} value={o.id}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  );
}

function MultiSelect({ label, options, value, onChange }: { label: string; options: Identity[]; value: string[]; onChange: (ids: string[]) => void }) {
  return (
    <fieldset className="au-checks">
      <legend>{label}</legend>
      {options.length === 0 && <p className="au-muted">ยังไม่มีรายการให้เลือก</p>}
      {options.map((o) => (
        <label key={o.id} className="au-check">
          <input type="checkbox" checked={value.includes(o.id)} onChange={(e) => onChange(e.target.checked ? [...value, o.id] : value.filter((v) => v !== o.id))} />
          {o.label}
        </label>
      ))}
    </fieldset>
  );
}

const PLACEHOLDERS = ["{{device}}", "{{value}}", "{{event}}"];

function TemplateField({ label, value, onChange, multiline }: { label: string; value: string; onChange: (value: string) => void; multiline?: boolean }) {
  return (
    <label className="au-label">
      {label}
      {multiline ? <textarea value={value} rows={3} onChange={(e) => onChange(e.target.value)} /> : <input value={value} onChange={(e) => onChange(e.target.value)} />}
      <span className="au-chips">
        {PLACEHOLDERS.map((token) => (
          <button key={token} type="button" className="au-chip" onClick={() => onChange(`${value}${token}`)} title={`แทรก ${token}`}>
            {token}
          </button>
        ))}
      </span>
    </label>
  );
}

function TestPanel({
  state,
  setState,
  identities,
  busy,
  dirty,
  trace,
  onRun,
  onClose,
}: {
  state: { external: string; mode: "event" | "metric"; event: string; value: string; action: string };
  setState: (next: { external: string; mode: "event" | "metric"; event: string; value: string; action: string }) => void;
  identities: Identity[];
  busy: boolean;
  dirty: boolean;
  trace: Trace | null;
  onRun: () => void;
  onClose: () => void;
}) {
  return (
    <div className="au-drawer" role="region" aria-label="ทดสอบผัง">
      <div className="au-drawer-head">
        <strong>ทดลองเดินผัง</strong>
        <span className="au-muted">ใช้ค่าล่าสุดจริงของ workspace แต่ไม่เปิดการแจ้งเตือนและไม่ส่งข้อความ</span>
        <button type="button" className="au-icon-btn" onClick={onClose} aria-label="ปิดแผงทดสอบ">
          ×
        </button>
      </div>
      <div className="au-drawer-body au-test">
        <label className="au-label">
          อุปกรณ์ที่สมมติว่าส่งข้อมูลเข้ามา
          <select value={state.external} onChange={(e) => setState({ ...state, external: e.target.value })}>
            <option value="">— เลือกอุปกรณ์ —</option>
            {identities.map((o) => (
              <option key={o.id} value={o.id}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
        <label className="au-label">
          แบบของข้อมูล
          <select value={state.mode} onChange={(e) => setState({ ...state, mode: e.target.value as "event" | "metric" })}>
            <option value="event">เหตุการณ์</option>
            <option value="metric">ค่าจากเซนเซอร์</option>
          </select>
        </label>
        {state.mode === "event" ? (
          <label className="au-label">
            เหตุการณ์
            <select value={state.event} onChange={(e) => setState({ ...state, event: e.target.value })}>
              {Object.keys(EVENT_LABEL).map((k) => (
                <option key={k} value={k}>
                  {EVENT_LABEL[k]}
                </option>
              ))}
            </select>
          </label>
        ) : (
          <label className="au-label">
            ค่าที่อ่านได้
            <input type="number" value={state.value} onChange={(e) => setState({ ...state, value: e.target.value })} />
          </label>
        )}
        {state.mode === "event" && state.event === "action" && (
          <label className="au-label">
            ปุ่มที่กด
            <input value={state.action} placeholder="เช่น single" onChange={(e) => setState({ ...state, action: e.target.value })} />
          </label>
        )}
        <button type="button" className="au-btn primary" onClick={onRun} disabled={busy || !state.external || dirty}>
          <CirclePlay size={15} aria-hidden="true" />
          เดินผัง
        </button>
        {dirty && <p className="au-muted">บันทึกผังก่อน แล้วจึงทดสอบได้</p>}
        {trace && (
          <p className="au-muted">
            บล็อกสีเขียว = เป็นจริงหรือได้ทำงาน · สีแดง = เป็นเท็จ · สีเทา = ไม่ถูกเรียกถึง · ถ้าเกิดจริงจะทำ {trace.actions.length} รายการ · การทดสอบไม่ส่งคำสั่งไปที่อุปกรณ์
          </p>
        )}
      </div>
    </div>
  );
}

const RUN_STATUS: Record<string, string> = { fired: "ทำงาน", skipped: "ไม่เข้าเงื่อนไข", error: "ผิดพลาด" };

function RunHistory({ runs, lookup, onClose, onReload }: { runs: Run[]; lookup: Lookup; onClose: () => void; onReload: () => void }) {
  return (
    <div className="au-drawer" role="region" aria-label="ประวัติการทำงาน">
      <div className="au-drawer-head">
        <strong>ประวัติการทำงาน</strong>
        <span className="au-muted">เก็บ 200 รายการล่าสุดต่อหนึ่งผัง</span>
        <button type="button" className="au-icon-btn" onClick={onReload} aria-label="โหลดประวัติใหม่" title="โหลดใหม่">
          ⟳
        </button>
        <button type="button" className="au-icon-btn" onClick={onClose} aria-label="ปิดประวัติ">
          ×
        </button>
      </div>
      <div className="au-drawer-body">
        {runs.length === 0 && <p className="au-muted">ยังไม่มีการทำงาน</p>}
        <ul className="au-runs">
          {runs.map((r) => {
            const actions = r.detail?.actions ?? [];
            return (
              <li key={r.id} className={`au-run st-${r.status}`}>
                <span className="au-run-when">{formatWhen(r.created_at)}</span>
                <span className="au-run-device">{r.external_id ? lookup.device(r.external_id) : "—"}</span>
                <span className="au-run-status">{RUN_STATUS[r.status] ?? r.status}</span>
                <span className="au-run-actions">
                  {r.detail?.error
                    ? r.detail.error
                    : actions.length === 0
                      ? "ไม่ได้ทำอะไร"
                      : actions
                          .filter((a) => a.type !== "action.command")
                          .map((a) => (a.type === "action.alert" ? `เปิดแจ้งเตือน${SEVERITY_LABEL[a.severity ?? ""] ?? ""}` : "ส่งข้อความออก"))
                          .concat(
                            (r.detail?.commands ?? []).map((c) =>
                              c.status === "queued"
                                ? `สั่ง ${lookup.target(c.device_id)} · ${c.property} = ${showValue(c.value)}`
                                : c.status === "requested"
                                  ? `ขอสั่ง ${lookup.target(c.device_id)} · ${c.property} = ${showValue(c.value)} (รอคิวคำสั่ง)`
                                  : `ไม่ได้สั่ง ${lookup.target(c.device_id)} (${COMMAND_REASON[c.reason ?? ""] ?? c.reason ?? "ถูกข้าม"})`,
                            ),
                          )
                          .join(" · ")}
                  {r.detail?.notifications ? ` · ส่ง ${r.detail.notifications} ช่องทาง` : ""}
                </span>
              </li>
            );
          })}
        </ul>
      </div>
    </div>
  );
}
