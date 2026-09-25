"use client";
import { type DragEvent, useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import {
  Background,
  BackgroundVariant,
  ConnectionMode,
  Controls,
  MarkerType,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Connection,
  type Edge,
  type IsValidConnection,
  type NodeChange,
  type XYPosition,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { LayoutGrid, Maximize2, PanelLeft, PanelRight, RefreshCw, Search } from "lucide-react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import "./topology.css";
import { ApiError, createClientFrom, type Device, type GatewayCreated, type MQTTCredentials, type Snapshot } from "./api";
import { useLatest } from "./use-latest";
import { DEVICE_PROFILES, deviceBrands, deviceProfile, formatMAC, gatewayModel, profileFitsGateway, profilesForBrand, suggestProfile, Z2M_GATEWAY_MODEL, Z2M_GENERIC_PROFILE, type DeviceProfile } from "./catalog";
import DiscoveryList from "./discovery";
import ZigbeeCatalogSearch from "./zigbee-catalog";
import Inspector, { type Selection } from "./inspector";
import { NODE_ID, autoLayout, loadPersisted, parseNodeId, placeNewNodes, savePersisted, type LayoutInput } from "./layout";
import { buildTopology, currentGateway, isAlerting, isFresh, isRecognised, scopeSnapshot, summarize, type ProjectScope, type Topology } from "./model";
import { nodeTypes, type AppNode } from "./nodes";
import Palette, { DRAG_MIME, type DragPayload } from "./palette";
import ProjectBar, { PROJECT_COLORS } from "./projects";
import { useSignals } from "./use-signals";

type Draft = { id: string; profile: string; gatewayId: string | null };
type GatewayDialog = { model: string; position: XYPosition } | null;
type AdoptDialog = { external: string | null; gatewayId: string | null; draftId?: string } | null;

const POLL_MS = 8000;
/** With a live signal socket the poll is only a safety net. */
const POLL_CONNECTED_MS = 30000;
const EMPTY: Topology = { broker: { configured: false, settings: null }, gateways: [], devices: [], serverTime: 0 };

/** Backend limits names/ids to 128 UTF‑8 bytes; Thai text is 3 bytes per character. */
export const utf8Bytes = (s: string) => new TextEncoder().encode(s).length;
export const validName = (s: string) => utf8Bytes(s) >= 1 && utf8Bytes(s) <= 128 && !/[\r\n\0]/.test(s);
const localId = () => globalThis.crypto?.randomUUID?.() ?? `d-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;

const shallowEqual = (a: Record<string, unknown> | undefined, b: Record<string, unknown> | undefined) => {
  if (a === b) return true;
  if (!a || !b) return false;
  const ka = Object.keys(a), kb = Object.keys(b);
  return ka.length === kb.length && ka.every((k) => a[k] === b[k]);
};

export type DeviceTopologyProps = {
  getToken: () => string;
  refresh: () => Promise<boolean>;
  /** Opens Dashboard Studio with the given `${gatewayId}/${mac}` source key. */
  onAdd: (sourceKey: string) => void;
  /** Called once when the session can no longer be refreshed; the parent should return to login. */
  onUnauthorized?: () => void;
};

export default function DeviceTopology(props: DeviceTopologyProps) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  );
}

function Canvas({ getToken, refresh, onAdd, onUnauthorized }: DeviceTopologyProps) {
  // The parent passes fresh inline callbacks on every render; keep them in refs so the client (and polling) stays stable.
  const handlers = useLatest({ getToken, refresh });
  const onUnauthorizedRef = useLatest(onUnauthorized);
  const [client] = useState(() => createClientFrom(handlers));
  const { screenToFlowPosition, fitView } = useReactFlow();
  const canvasRef = useRef<HTMLDivElement>(null);

  const [tenant, setTenant] = useState("");
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [receivedAt, setReceivedAt] = useState(0);
  const [now, setNow] = useState(() => Date.now());
  const [loadError, setLoadError] = useState("");
  const [actionError, setActionError] = useState("");
  const [dialogError, setDialogError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [selection, setSelection] = useState<Selection>(null);
  const [query, setQuery] = useState("");
  const [placed, setPlaced] = useState<Set<string>>(new Set());
  const [drafts, setDrafts] = useState<Draft[]>([]);
  const [credentials, setCredentials] = useState<Record<string, MQTTCredentials>>({});
  const [httpTokens, setHttpTokens] = useState<Record<string, GatewayCreated>>({});
  const [gatewayDialog, setGatewayDialog] = useState<GatewayDialog>(null);
  const [gatewayName, setGatewayName] = useState("");
  const [discoveryOpen, setDiscoveryOpen] = useState(false);
  const [adopt, setAdopt] = useState<AdoptDialog>(null);
  const [rotateId, setRotateId] = useState<string | null>(null);
  const [revokeId, setRevokeId] = useState<string | null>(null);
  const [editReg, setEditReg] = useState<Device | null>(null);
  const [editName, setEditName] = useState("");
  const [editGateway, setEditGateway] = useState("");
  const [removeReg, setRemoveReg] = useState<Device | null>(null);
  const [moveReg, setMoveReg] = useState<{ registration: Device; gatewayId: string } | null>(null);
  const [layoutVersion, bumpLayout] = useReducer((n: number) => n + 1, 0);
  // A dismissed warning stays dismissed for this browser session; a different warning shows again.
  const [dismissedWarning, setDismissedWarningState] = useState(() => {
    try {
      return typeof sessionStorage === "undefined" ? "" : (sessionStorage.getItem("aether.topology.warning") ?? "");
    } catch {
      return "";
    }
  });
  const setDismissedWarning = (text: string) => {
    setDismissedWarningState(text);
    try {
      sessionStorage.setItem("aether.topology.warning", text);
    } catch {}
  };
  const [scope, setScopeState] = useState<ProjectScope>("all");
  const [panels, setPanels] = useState(() => loadPanels());
  const togglePanel = (side: "palette" | "inspector") =>
    setPanels((p) => {
      const next = { ...p, [side]: !p[side] };
      savePanels(next);
      return next;
    });
  /** Selecting something while the inspector is hidden brings it back, otherwise the click would appear to do nothing. */
  const reveal = (sel: Selection) => {
    setSelection(sel);
    if (sel && !panels.inspector) togglePanel("inspector");
  };

  const positions = useRef<Record<string, XYPosition>>({});
  /** Measured node sizes reported by React Flow; carried across rebuilds so nodes are not re-measured (and hidden) every poll. */
  const measured = useRef<Record<string, { width: number; height: number }>>({});
  /** Previous node objects by id; reused when nothing changed so memoised node components and React Flow internals stay stable. */
  const nodeCache = useRef(new Map<string, AppNode>());
  const fitted = useRef(false);
  const inflight = useRef<Promise<void> | null>(null);
  const sequence = useRef(0);
  const unauthorized = useRef(false);

  // Freshness keeps decaying between polls and when polling fails, so a dead backend cannot leave devices "online".
  const serverNow = snapshot ? snapshot.serverTime + Math.max(0, now - receivedAt) : 0;
  // An archived or foreign project id falls back to "all" so the canvas never goes blank by accident.
  const activeScope: ProjectScope = scope === "all" || scope === "none" || snapshot?.projects.some((p) => p.id === scope) ? scope : "all";
  const scoped = useMemo(() => (snapshot ? scopeSnapshot(snapshot, activeScope) : null), [snapshot, activeScope]);
  const topology = useMemo(() => (scoped ? buildTopology(scoped, serverNow) : EMPTY), [scoped, serverNow]);
  const fullTopology = useMemo(() => (snapshot ? buildTopology(snapshot, serverNow) : EMPTY), [snapshot, serverNow]);
  const setScope = (next: ProjectScope) => {
    setScopeState(next);
    setSelection(null);
    try {
      localStorage.setItem(`aether.topology.project.${tenant || "default"}`, next);
    } catch {
      // storage unavailable
    }
    requestAnimationFrame(() => fitView({ padding: 0.2, duration: 300 }));
  };

  const load = useCallback(
    (force = false): Promise<void> => {
      if (inflight.current && !force) return inflight.current;
      if (unauthorized.current) return Promise.resolve();
      const seq = ++sequence.current;
      const run = (async () => {
        try {
          const s = await client.snapshot(force);
          if (seq !== sequence.current) return; // a newer (forced) load already applied its result
          setSnapshot(s);
          setReceivedAt(Date.now());
          setNow(Date.now());
          setLoadError("");
        } catch (e) {
          if (seq !== sequence.current) return;
          if (e instanceof ApiError && e.status === 401) {
            unauthorized.current = true;
            onUnauthorizedRef.current?.();
          }
          setLoadError(e instanceof Error ? e.message : "โหลดข้อมูลไม่ได้");
        }
      })().finally(() => {
        if (inflight.current === run) inflight.current = null;
      });
      inflight.current = run;
      return run;
    },
    [client],
  );

  /** After a mutation: wait for any poll in flight (its snapshot predates the change), then fetch fresh state. */
  const reload = useCallback(async () => {
    await inflight.current?.catch(() => {});
    await load(true);
  }, [load]);

  // Session-scoped layout: positions are keyed by tenant so two workspaces never share a canvas.
  useEffect(() => {
    let active = true;
    client
      .me()
      .then((me) => {
        if (!active) return;
        const saved = loadPersisted(me.tenant_id);
        positions.current = saved.positions;
        setPlaced(new Set(saved.placed));
        try {
          setScopeState(localStorage.getItem(`aether.topology.project.${me.tenant_id}`) ?? "all");
        } catch {
          // storage unavailable
        }
        setTenant(me.tenant_id);
      })
      .catch((e: unknown) => {
        if (!active) return;
        if (e instanceof ApiError && e.status === 401) {
          unauthorized.current = true;
          onUnauthorizedRef.current?.();
          return;
        }
        setTenant("default");
      });
    return () => {
      active = false;
    };
  }, [client]);

  // Push: a signal only says "refetch". Bursts (one uplink = packet + events + alerts) collapse into one load.
  const pending = useRef<{ timer?: ReturnType<typeof setTimeout>; force: boolean }>({ force: false });
  useEffect(() => {
    const p = pending.current;
    return () => clearTimeout(p.timer);
  }, []);
  const connected = useSignals(handlers, (kind) => {
    pending.current.force = pending.current.force || kind === "inventory";
    clearTimeout(pending.current.timer);
    pending.current.timer = setTimeout(() => {
      const force = pending.current.force;
      pending.current.force = false;
      void load(force);
    }, 350);
  }, tenant !== "");
  const connectedRef = useLatest(connected);

  useEffect(() => {
    if (!tenant) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      await load();
      if (active && !unauthorized.current) timer = setTimeout(poll, connectedRef.current ? POLL_CONNECTED_MS : POLL_MS);
    };
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [tenant, load, connectedRef]);

  useEffect(() => {
    if (!notice) return;
    const t = setTimeout(() => setNotice(""), 3500);
    return () => clearTimeout(t);
  }, [notice]);

  const dialogOpen = gatewayDialog !== null || adopt !== null || editReg !== null;
  async function action(fn: () => Promise<void>, inDialog = dialogOpen) {
    setBusy(true);
    setActionError("");
    setDialogError("");
    try {
      await fn();
    } catch (e) {
      const message = e instanceof Error ? e.message : "เกิดข้อผิดพลาด";
      if (inDialog) setDialogError(message);
      else setActionError(message);
    } finally {
      setBusy(false);
    }
  }

  // Supported models the server resolved for discovery (its stream name keeps the model between info frames).
  const discoveredIds = useMemo(() => new Set((snapshot?.discovery ?? []).map((d) => d.external_id.toLowerCase())), [snapshot]);
  // Which device nodes live on the canvas: registered devices and supported models only. Anything else a
  // gateway hears (phones, other people's beacons) never appears, even if it was placed or laid out before.
  const visibleDevices = useMemo(() => {
    const ids = new Set<string>();
    for (const d of topology.devices) if (isRecognised(d, discoveredIds)) ids.add(NODE_ID.device(d.external));
    return ids;
  }, [topology, discoveredIds]);

  const layoutInput = useMemo<LayoutInput>(() => ({ gateways: topology.gateways, devices: topology.devices, visibleDevices, drafts }), [topology, visibleDevices, drafts]);

  // Search: node ids that do not match the query (shared by nodes and edges so edges never depend on the node array).
  const dimmed = useMemo(() => {
    const out = new Set<string>();
    const q = query.trim().toLowerCase();
    if (!q) return out;
    const hit = (...parts: (string | null | undefined)[]) => parts.some((p) => p?.toLowerCase().includes(q));
    for (const g of topology.gateways) {
      const model = gatewayModel(g.gateway.model);
      if (!hit(g.gateway.name, g.gateway.model, model?.brand, model?.model)) out.add(NODE_ID.gateway(g.gateway.id));
    }
    for (const d of topology.devices) {
      const profile = d.registrations[0] ? deviceProfile(d.registrations[0].profile_id) : undefined;
      if (!hit(d.name, d.external, formatMAC(d.external), profile?.brand, profile?.model, d.model, d.kind)) out.add(NODE_ID.device(d.external));
    }
    for (const draft of drafts) {
      const profile = deviceProfile(draft.profile);
      if (!hit(profile?.label, profile?.brand)) out.add(NODE_ID.draft(draft.id));
    }
    return out;
  }, [topology, drafts, query]);

  const persist = useCallback(
    (nextPlaced?: Set<string>) => {
      if (!tenant) return;
      // Prune keys for nodes that no longer exist so storage does not grow with every BLE device ever seen.
      const keep = new Set<string>([NODE_ID.broker]);
      for (const g of fullTopology.gateways) keep.add(NODE_ID.gateway(g.gateway.id));
      const placedSet = nextPlaced ?? placed;
      for (const d of fullTopology.devices) if (isRecognised(d, discoveredIds)) keep.add(NODE_ID.device(d.external));
      const pruned: Record<string, XYPosition> = {};
      for (const [id, pos] of Object.entries(positions.current)) if (keep.has(id)) pruned[id] = pos;
      const known = new Set(fullTopology.devices.map((d) => d.external));
      savePersisted(tenant, { positions: pruned, placed: [...placedSet].filter((mac) => known.has(mac)) });
    },
    [tenant, placed, fullTopology, discoveredIds],
  );

  // Nodes are derived from the model plus the positions the user has dragged to (kept in a ref, versioned by layoutVersion).
  const { nodes, added } = useMemo(() => {
    void layoutVersion;
    if (!snapshot) return { nodes: [] as AppNode[], added: {} as Record<string, XYPosition> };
    const added = placeNewNodes(layoutInput, positions.current);
    const pos = Object.keys(added).length ? { ...positions.current, ...added } : positions.current;
    const selectedId =
      selection?.kind === "broker" ? NODE_ID.broker : selection?.kind === "gateway" ? NODE_ID.gateway(selection.id) : selection?.kind === "device" ? NODE_ID.device(selection.external) : selection?.kind === "draft" ? NODE_ID.draft(selection.id) : "";
    const next: AppNode[] = [];
    const push = (node: AppNode) => {
      const prev = nodeCache.current.get(node.id);
      if (prev && prev.type === node.type && prev.selected === node.selected && prev.position.x === node.position.x && prev.position.y === node.position.y && shallowEqual(prev.measured, node.measured) && shallowEqual(prev.data, node.data)) {
        next.push(prev);
      } else {
        nodeCache.current.set(node.id, node);
        next.push(node);
      }
    };
    const base = (id: string) => ({ id, position: pos[id] ?? { x: 0, y: 0 }, selected: selectedId === id, measured: measured.current[id] });
    const settings = topology.broker.settings;
    push({
      ...base(NODE_ID.broker),
      type: "broker",
      data: {
        configured: topology.broker.configured,
        host: settings?.host ?? "",
        port: settings?.port ?? null,
        scheme: settings?.scheme ?? "",
        receiving: topology.gateways.filter((g) => g.health === "receiving").length,
        total: topology.gateways.length,
      },
    });
    for (const g of topology.gateways) {
      const id = NODE_ID.gateway(g.gateway.id);
      const project = activeScope === "all" ? snapshot.projects.find((p) => p.id === g.gateway.project_id) : undefined;
      push({ ...base(id), type: "gateway", data: { name: g.gateway.name, model: g.gateway.model, health: g.health, decoded: g.decodedSensors, nearby: g.nearbyDevices, project: project?.name ?? null, projectColor: project ? (PROJECT_COLORS[project.color] ?? null) : null, dim: dimmed.has(id) } });
    }
    for (const d of topology.devices) {
      const id = NODE_ID.device(d.external);
      if (!visibleDevices.has(id)) continue;
      const reg = d.registrations[0];
      push({
        ...base(id),
        type: "device",
        data: {
          name: d.name,
          external: d.external,
          profile: reg?.profile_id ?? null,
          adopted: d.registrations.length > 0,
          decoded: d.reading !== null,
          fresh: isFresh(d.reading?.received_at, topology.serverTime),
          simulated: d.simulated,
          temperature: d.reading?.temperature ?? null,
          humidity: d.reading?.humidity ?? null,
          kind: d.kind,
          // A photo only when the model is actually known: the registered profile, or the name the tag itself reports.
          image: (reg ? deviceProfile(reg.profile_id)?.image : d.model ? suggestProfile({ model: d.model })?.image : undefined) ?? null,
          summary: summarize(d),
          roaming: d.roaming,
          zone: d.roaming ? (topology.gateways.find((g) => g.gateway.id === currentGateway(d, topology.serverTime)?.gatewayId)?.gateway.name ?? null) : null,
          alert: isAlerting(d),
          dim: dimmed.has(id),
        },
      });
    }
    for (const draft of drafts) {
      const id = NODE_ID.draft(draft.id);
      const profile = deviceProfile(draft.profile);
      push({ ...base(id), type: "draft", data: { profile: draft.profile, label: profile?.label ?? draft.profile, dim: dimmed.has(id) } });
    }
    return { nodes: next, added };
  }, [snapshot, topology, layoutInput, visibleDevices, drafts, dimmed, selection, layoutVersion, activeScope]);

  // Commit auto-placed positions after render (never during it), persist them, and fit the viewport once.
  useEffect(() => {
    if (Object.keys(added).length) {
      positions.current = { ...positions.current, ...added };
      persist();
    }
    if (!fitted.current && nodes.length >= 1) {
      fitted.current = true;
      requestAnimationFrame(() => fitView({ padding: 0.2, duration: 300 }));
    }
  }, [nodes, added, persist, fitView]);

  const edges = useMemo<Edge[]>(() => {
    const out: Edge[] = [];
    const cls = (base: string, a: string, b: string) => `topo-edge ${base}${dimmed.has(a) || dimmed.has(b) ? " edge-dim" : ""}`;
    for (const g of topology.gateways) {
      const id = NODE_ID.gateway(g.gateway.id);
      out.push({ id: `link:${g.gateway.id}`, source: id, sourceHandle: "up", target: NODE_ID.broker, targetHandle: "in", type: "smoothstep", className: cls(`edge-${g.health}`, id, NODE_ID.broker), animated: g.health === "receiving", markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 } });
    }
    const gatewayIds = new Set(topology.gateways.map((g) => g.gateway.id));
    for (const d of topology.devices) {
      const dev = NODE_ID.device(d.external);
      if (!visibleDevices.has(dev)) continue;
      const seen = new Set<string>();
      const here = d.roaming ? currentGateway(d, topology.serverTime)?.gatewayId : undefined;
      for (const reg of d.registrations) {
        if (!gatewayIds.has(reg.gateway_id) || seen.has(reg.gateway_id)) continue;
        const heard = d.heard.find((h) => h.gatewayId === reg.gateway_id);
        const gw = NODE_ID.gateway(reg.gateway_id);
        seen.add(reg.gateway_id);
        out.push({ id: `adopt:${reg.id}`, source: dev, sourceHandle: "up", target: gw, targetHandle: "down", type: "smoothstep", className: cls(d.roaming && here && here !== reg.gateway_id ? "edge-adopted edge-away" : "edge-adopted", dev, gw), reconnectable: "target", animated: isFresh(heard?.receivedAt, topology.serverTime), label: heard?.rssi != null ? `${heard.rssi} dBm` : undefined, markerEnd: { type: MarkerType.ArrowClosed, width: 12, height: 12 } });
      }
      for (const h of d.heard) {
        if (seen.has(h.gatewayId) || !gatewayIds.has(h.gatewayId)) continue;
        const gw = NODE_ID.gateway(h.gatewayId);
        out.push({ id: `heard:${h.gatewayId}/${d.external}`, source: dev, sourceHandle: "up", target: gw, targetHandle: "down", type: "smoothstep", className: cls(d.roaming ? (here === h.gatewayId ? "edge-roam edge-here" : "edge-roam") : "edge-heard", dev, gw), animated: d.roaming && here === h.gatewayId, label: h.rssi != null ? `${here === h.gatewayId ? "อยู่ที่นี่ · " : ""}${h.rssi} dBm` : undefined });
      }
    }
    return out;
  }, [topology, visibleDevices, dimmed]);

  const onNodesChange = useCallback((changes: NodeChange<AppNode>[]) => {
    // Selection is owned by this component (inspector); React Flow only reports positions and measurements.
    let changed = false;
    for (const c of changes) {
      if (c.type === "position" && c.position) {
        positions.current[c.id] = c.position;
        changed = true;
      } else if (c.type === "dimensions" && c.dimensions) {
        const prev = measured.current[c.id];
        if (!prev || prev.width !== c.dimensions.width || prev.height !== c.dimensions.height) {
          measured.current[c.id] = { width: c.dimensions.width, height: c.dimensions.height };
          changed = true;
        }
      }
    }
    if (changed) bumpLayout();
  }, []);

  const select = (id: string) => {
    const { kind, ref } = parseNodeId(id);
    if (kind === "broker") reveal({ kind: "broker" });
    else if (kind === "gateway") reveal({ kind: "gateway", id: ref });
    else if (kind === "device") reveal({ kind: "device", external: ref });
    else if (kind === "draft") {
      const draft = drafts.find((d) => d.id === ref);
      if (draft) reveal({ kind: "draft", id: draft.id, profile: draft.profile });
    }
  };

  const normalise = (c: Connection | Edge): { device: ReturnType<typeof parseNodeId>; gateway: string } | null => {
    if (!c.source || !c.target) return null;
    const a = parseNodeId(c.source), b = parseNodeId(c.target);
    const [dev, gw] = a.kind === "gateway" ? [b, a] : [a, b];
    if (gw.kind !== "gateway" || (dev.kind !== "device" && dev.kind !== "draft")) return null;
    return { device: dev, gateway: gw.ref };
  };

  const isValidConnection: IsValidConnection = (c) => {
    const n = normalise(c);
    if (!n) return false;
    if (n.device.kind === "device") {
      const d = topology.devices.find((x) => x.external === n.device.ref);
      if (d?.registrations.some((r) => r.gateway_id === n.gateway)) return false;
    }
    return true;
  };

  const onConnect = (c: Connection) => {
    const n = normalise(c);
    if (!n) return;
    setDialogError("");
    if (n.device.kind === "draft") setAdopt({ external: null, gatewayId: n.gateway, draftId: n.device.ref });
    else setAdopt({ external: n.device.ref, gatewayId: n.gateway });
  };

  /** Flow position at the centre of the visible canvas, used when a palette card is activated by keyboard/click. */
  const canvasCentre = (): XYPosition => {
    const rect = canvasRef.current?.getBoundingClientRect();
    return rect ? screenToFlowPosition({ x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 }) : { x: 0, y: 0 };
  };

  const addGateway = (model: string, position: XYPosition) => {
    setGatewayName("");
    setDialogError("");
    setGatewayDialog({ model, position });
  };
  const addDraft = (profile: string, position: XYPosition) => {
    const id = localId();
    positions.current[NODE_ID.draft(id)] = position;
    setDrafts((d) => [...d, { id, profile, gatewayId: null }]);
    reveal({ kind: "draft", id, profile });
  };

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    const raw = e.dataTransfer.getData(DRAG_MIME);
    if (!raw) return;
    let payload: DragPayload;
    try {
      payload = JSON.parse(raw) as DragPayload;
    } catch {
      return;
    }
    const position = screenToFlowPosition({ x: e.clientX, y: e.clientY });
    switch (payload.kind) {
      case "gateway":
        if (typeof payload.model === "string") addGateway(payload.model, position);
        return;
      case "profile":
        if (typeof payload.profile === "string") addDraft(payload.profile, position);
        return;
      case "device":
        if (typeof payload.external !== "string") return;
        positions.current[NODE_ID.device(payload.external)] = position;
        placeDevice(payload.external);
        reveal({ kind: "device", external: payload.external });
        return;
      default:
        return;
    }
  };

  function placeDevice(external: string) {
    setPlaced((prev) => {
      if (prev.has(external)) return prev;
      const next = new Set(prev);
      next.add(external);
      persist(next);
      return next;
    });
  }

  function relayout() {
    positions.current = autoLayout(layoutInput);
    persist();
    bumpLayout();
    requestAnimationFrame(() => fitView({ padding: 0.2, duration: 300 }));
  }

  async function issueMQTT(gatewayId: string, rotate: boolean) {
    const c = await client.issueMQTT(gatewayId, rotate);
    setCredentials((prev) => ({ ...prev, [gatewayId]: c }));
    setNotice(rotate ? "สร้างรหัสผ่านใหม่แล้ว · นำไปตั้งใน gateway ก่อนเชื่อมต่อครั้งถัดไป" : "สร้างบัญชี MQTT แล้ว · เก็บรหัสผ่านก่อนเปลี่ยนหน้า");
  }

  const discoveryItems = (scoped?.discovery ?? []).filter((item) => !fullTopology.devices.some((d) => d.external === item.external_id && d.registrations.length > 0));
  const discovered = topology.devices.filter((d) => discoveryItems.some((item) => item.external_id === d.external));
  const adoptDevice = adopt?.external ? topology.devices.find((d) => d.external === adopt.external) : undefined;
  const receiving = topology.gateways.filter((g) => g.health === "receiving").length;
  const adoptedCount = topology.devices.filter((d) => d.registrations.length > 0).length;
  const error = actionError || loadError;
  const warningText = snapshot?.warnings.join(" · ") ?? "";
  const gatewayDialogModel = gatewayModel(gatewayDialog?.model ?? "");

  return (
    <section className="topo">
      <header className="topo-bar">
        <h1 className="topo-bar-title">เชื่อมต่ออุปกรณ์</h1>
        <button type="button" className="topo-btn primary" onClick={() => setDiscoveryOpen(true)}>อุปกรณ์ที่พบใหม่ <span className="topo-count">{new Set(discoveryItems.map((d) => d.external_id)).size}</span></button>
        <div className="topo-bar-stats">
          <span className={`topo-live ${connected ? "is-on" : ""}`} title={connected ? "รับการเปลี่ยนแปลงแบบ real-time" : "ยังไม่ได้เชื่อม real-time · ใช้การตรวจเป็นรอบแทน"}>
            <i aria-hidden="true" /> {connected ? "Live" : "Polling"}
          </span>
          <span title="gateway ที่กำลังส่งข้อมูล / gateway ทั้งหมดในมุมมองนี้">
            <b>{receiving}</b>/{topology.gateways.length} gateway
          </span>
          <span title="อุปกรณ์ที่ลงทะเบียน (adopt) แล้ว">
            <b>{adoptedCount}</b> adopted
          </span>
          <span title="sensor ที่ Aether ถอดรหัสค่าได้">
            <b>{topology.devices.filter((d) => d.reading && isRecognised(d, discoveredIds)).length}</b> sensor
          </span>
          <span title="BLE ที่ gateway ได้ยินแต่ยังไม่ได้วางบน canvas">
            <b>{discovered.length}</b> รอลงทะเบียน
          </span>
        </div>
        <label className="topo-search">
          <Search size={14} />
          <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="ค้นหาบน canvas" aria-label="ค้นหาบน canvas" />
        </label>
        <div className="topo-bar-actions">
          <button type="button" className={`topo-btn ${panels.palette ? "is-on" : ""}`} aria-pressed={panels.palette} onClick={() => togglePanel("palette")} title={panels.palette ? "ซ่อนแถบอุปกรณ์" : "แสดงแถบอุปกรณ์"} aria-label={panels.palette ? "ซ่อนแถบอุปกรณ์" : "แสดงแถบอุปกรณ์"}>
            <PanelLeft size={15} />
          </button>
          <button type="button" className={`topo-btn ${panels.inspector ? "is-on" : ""}`} aria-pressed={panels.inspector} onClick={() => togglePanel("inspector")} title={panels.inspector ? "ซ่อนรายละเอียด" : "แสดงรายละเอียด"} aria-label={panels.inspector ? "ซ่อนรายละเอียด" : "แสดงรายละเอียด"}>
            <PanelRight size={15} />
          </button>
          <span className="topo-bar-sep" aria-hidden="true" />
          <button type="button" className="topo-btn" onClick={relayout} title="จัดวางอัตโนมัติ · broker → gateway → อุปกรณ์" aria-label="จัดวางอัตโนมัติ">
            <LayoutGrid size={15} />
          </button>
          <button type="button" className="topo-btn" onClick={() => fitView({ padding: 0.2, duration: 300 })} title="ย่อ/ขยายให้พอดีจอ" aria-label="ย่อ/ขยายให้พอดีจอ">
            <Maximize2 size={15} />
          </button>
          <button type="button" className="topo-btn" disabled={busy} onClick={() => void reload()} title="ตรวจสถานะตอนนี้" aria-label="ตรวจสถานะตอนนี้">
            <RefreshCw size={15} />
          </button>
        </div>
      </header>

      <ProjectBar
        projects={snapshot?.projects ?? []}
        scope={activeScope}
        unassigned={snapshot?.gateways.filter((g) => !g.project_id).length ?? 0}
        total={snapshot?.gateways.length ?? 0}
        busy={busy}
        error={dialogError}
        onScope={setScope}
        onCreate={async (input) => {
          let ok = false;
          await action(async () => {
            const created = await client.createProject(input);
            await reload();
            setScope(created.id);
            setNotice(`สร้างโปรเจค ${created.name} แล้ว · gateway ที่เพิ่มจากนี้จะอยู่ในโปรเจคนี้`);
            ok = true;
          }, true);
          return ok;
        }}
        onUpdate={async (id, input) => {
          let ok = false;
          await action(async () => {
            await client.updateProject(id, input);
            await reload();
            ok = true;
          }, true);
          return ok;
        }}
        onArchive={(project) =>
          void action(async () => {
            await client.archiveProject(project.id);
            setScope("all");
            setNotice(`เก็บโปรเจค ${project.name} แล้ว`);
            await reload();
          }, false)
        }
      />

      <div className={`topo-body ${panels.palette ? "" : "no-palette"} ${panels.inspector ? "" : "no-inspector"}`}>
        {panels.palette && (
        <Palette
          discovered={discovered}
          onCanvas={placed}
          onPlace={placeDevice}
          onSelect={(external) => reveal({ kind: "device", external })}
          onAddGateway={(model) => addGateway(model, canvasCentre())}
          onAddProfile={(profile) => addDraft(profile, canvasCentre())}
        />
        )}

        <div
          className="topo-canvas"
          ref={canvasRef}
          onDrop={onDrop}
          onDragOver={(e) => {
            e.preventDefault();
            e.dataTransfer.dropEffect = "copy";
          }}
        >
        {/* Messages float over the canvas instead of pushing the workspace down. */}
        <div className="topo-toasts">
      {error && (
        <div className="topo-alert is-error" role="alert">
          {error}
          <button
            type="button"
            onClick={() => {
              setActionError("");
              setLoadError("");
            }}
            aria-label="ปิด"
          >
            ×
          </button>
        </div>
      )}
      {!error && warningText && warningText !== dismissedWarning ? (
        <div className="topo-alert is-warn" role="status">
          {warningText}
          <button type="button" onClick={() => setDismissedWarning(warningText)} aria-label="ปิดคำเตือน">
            ×
          </button>
        </div>
      ) : null}
      {notice && (
        <div className="topo-alert is-notice" role="status">
          {notice}
        </div>
      )}

        </div>
          <ReactFlow<AppNode, Edge>
            nodes={nodes}
            edges={edges}
            nodeTypes={nodeTypes}
            onNodesChange={onNodesChange}
            onNodeClick={(_, n) => select(n.id)}
            onEdgeClick={(_, e) => {
              const n = normalise(e);
              if (n) select(n.device.kind === "device" ? NODE_ID.device(n.device.ref) : NODE_ID.draft(n.device.ref));
              else if (e.source) select(e.source);
            }}
            onPaneClick={() => setSelection(null)}
            onNodeDragStop={() => persist()}
            onConnect={onConnect}
            onReconnect={(oldEdge, connection) => {
              // Dragging the gateway end of an adopted (green) link onto another gateway moves the registration.
              if (!oldEdge.id.startsWith("adopt:")) return;
              const target = parseNodeId(connection.target ?? "");
              const registration = snapshot?.devices.find((d) => d.id === oldEdge.id.slice("adopt:".length));
              if (target.kind !== "gateway" || !registration || registration.gateway_id === target.ref) return;
              setMoveReg({ registration, gatewayId: target.ref });
            }}
            isValidConnection={isValidConnection}
            connectionMode={ConnectionMode.Loose}
            connectionLineStyle={{ stroke: "var(--color-accent)", strokeWidth: 2, strokeDasharray: "6 4" }}
            deleteKeyCode={null}
            elevateNodesOnSelect={false}
            nodesConnectable
            elementsSelectable
            selectionOnDrag={false}
            selectNodesOnDrag={false}
            minZoom={0.2}
            maxZoom={2}
            fitView={false}
          >
            <Background variant={BackgroundVariant.Dots} gap={22} size={1.2} color="var(--color-grid)" />
            <Controls showInteractive={false} position="bottom-left" />
            <MiniMap pannable zoomable position="bottom-right" nodeColor={(n) => (n.type === "broker" ? "#80b7ff" : n.type === "gateway" ? "#ace5ce" : n.type === "draft" ? "#e8b775" : "#6f8f88")} maskColor="rgba(8,14,16,.65)" />
          </ReactFlow>
          {snapshot && topology.gateways.length === 0 && (
            <div className="topo-hint">
              <strong>ยังไม่มี gateway ใน workspace นี้</strong>
              <span>ลากหรือกด “Minew MG3” จากแถบซ้ายเพื่อเริ่ม</span>
            </div>
          )}
          {!snapshot && !error && <div className="topo-hint">กำลังโหลด topology…</div>}
        </div>

        {panels.inspector && (
        <Inspector
          client={client}
          selection={selection}
          topology={topology}
          discovery={discoveryItems}
          credentials={credentials}
          httpTokens={httpTokens}
          busy={busy}
          onClose={() => setSelection(null)}
          onIssueMQTT={(id) => void action(() => issueMQTT(id, false).then(reload), false)}
          onRotate={setRotateId}
          onRevoke={setRevokeId}
          onAdopt={(external, gatewayId, draftId) => {
            setDialogError("");
            setAdopt({ external, gatewayId, draftId });
          }}
          onOpenStudio={onAdd}
          onRemoveDraft={(id) => {
            setDrafts((d) => d.filter((x) => x.id !== id));
            delete positions.current[NODE_ID.draft(id)];
            setSelection(null);
          }}
          onSelectGateway={(id) => setSelection({ kind: "gateway", id })}
          onSelectDevice={(external) => {
            placeDevice(external);
            setSelection({ kind: "device", external });
          }}
          onNotice={setNotice}
          removedDevices={scoped?.removedDevices ?? []}
          projects={snapshot?.projects ?? []}
          onSetRoaming={(registrations, roaming) =>
            void action(async () => {
              const results = await Promise.allSettled(registrations.map((r) => client.setDeviceRoaming(r.id, roaming)));
              await reload();
              const failedResult = results.find((r): r is PromiseRejectedResult => r.status === "rejected");
              if (failedResult) throw failedResult.reason instanceof Error ? failedResult.reason : new Error("เปลี่ยนโหมดใช้หลาย gateway ไม่สำเร็จบางรายการ");
              setNotice(roaming ? "เปิดโหมดใช้หลาย gateway แล้ว · ตำแหน่งจะตาม gateway ที่สัญญาณแรงสุด" : "ปิดโหมดใช้หลาย gateway แล้ว");
              await reload();
            }, false)
          }
          onSetGatewayProject={(gatewayId, projectId) =>
            void action(async () => {
              await client.setGatewayProject(gatewayId, projectId);
              setNotice(projectId ? "ย้าย gateway ไปโปรเจคใหม่แล้ว · อุปกรณ์ของ gateway นี้ย้ายตามไปด้วย" : "นำ gateway ออกจากโปรเจคแล้ว");
              await reload();
            }, false)
          }
          onEditRegistration={(r) => {
            setDialogError("");
            setEditName(r.name);
            setEditGateway(r.gateway_id);
            setEditReg(r);
          }}
          onRemoveRegistration={setRemoveReg}
          onRestoreRegistration={(r) =>
            void action(async () => {
              await client.restoreDevice(r.id);
              placeDevice(r.external_id.toLowerCase());
              setNotice(`กู้คืนการลงทะเบียน ${r.name} แล้ว`);
              await reload();
            }, false)
          }
          onHide={() => togglePanel("inspector")}
        />
        )}
      </div>

      {/* New gateway */}
      <Dialog open={gatewayDialog !== null} onOpenChange={(open) => !open && !busy && setGatewayDialog(null)}>
        <DialogContent className="topo-dialog">
          <DialogHeader>
            <DialogTitle>เพิ่ม {gatewayDialogModel?.label ?? "gateway"}</DialogTitle>
            <DialogDescription>{gatewayDialogModel?.description}</DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              const dialog = gatewayDialog;
              const name = gatewayName.trim();
              if (!dialog || !validName(name)) return;
              void action(async () => {
                const created = await client.createGateway(name, dialog.model, activeScope !== "all" && activeScope !== "none" ? activeScope : null);
                positions.current[NODE_ID.gateway(created.gateway.id)] = dialog.position;
                const model = gatewayModel(dialog.model);
                if (model?.transport === "http") setHttpTokens((prev) => ({ ...prev, [created.gateway.id]: created }));
                setGatewayDialog(null);
                setSelection({ kind: "gateway", id: created.gateway.id });
                if (model?.transport === "mqtt" && topology.broker.configured) {
                  try {
                    await issueMQTT(created.gateway.id, false);
                  } catch (err) {
                    setActionError(err instanceof Error ? err.message : "ออกบัญชี MQTT ไม่สำเร็จ");
                  }
                } else setNotice("สร้าง gateway แล้ว");
                await reload();
              }, true);
            }}
          >
            <label htmlFor="topo-gateway-name">ชื่อ gateway</label>
            <input id="topo-gateway-name" autoFocus required placeholder="เช่น MG3 · ชั้น 1 โถงหน้า" value={gatewayName} onChange={(e) => setGatewayName(e.target.value)} aria-invalid={gatewayName.trim() !== "" && !validName(gatewayName.trim())} />
            <ByteHint value={gatewayName} />
            <p className="topo-note">{gatewayDialogModel?.transport === "mqtt" ? (topology.broker.configured ? (gatewayDialogModel.id === Z2M_GATEWAY_MODEL ? "ระบบจะออกบัญชี MQTT ให้ทันที แล้วแสดงส่วน mqtt ของ configuration.yaml (มีรหัสผ่าน แสดงครั้งเดียว) ให้นำไปใส่ใน Zigbee2MQTT บนเครื่องในอาคาร" : "ระบบจะออกบัญชี MQTT ให้ทันที และแสดงรหัสผ่านครั้งเดียวในแถบขวา") : "server ยังไม่เปิดออกบัญชี MQTT อัตโนมัติ · สร้าง gateway ได้ก่อน") : "token สำหรับ HTTP Basic จะแสดงครั้งเดียวในแถบขวา"}</p>
            {dialogError && (
              <p className="topo-warn" role="alert">
                {dialogError}
              </p>
            )}
            <div className="topo-dialog-actions">
              <button type="button" className="topo-btn" onClick={() => setGatewayDialog(null)} disabled={busy}>
                ยกเลิก
              </button>
              <button type="submit" className="topo-btn primary" disabled={busy || !validName(gatewayName.trim())}>
                {busy ? "กำลังสร้าง…" : "สร้าง gateway"}
              </button>
            </div>
          </form>
        </DialogContent>
      </Dialog>

      {/* Adopt device */}
      <Dialog open={discoveryOpen} onOpenChange={setDiscoveryOpen}>
        <DialogContent className="topo-dialog topo-discovery-dialog">
          <DialogHeader><DialogTitle>อุปกรณ์ที่พบใหม่</DialogTitle><DialogDescription>เลือกอุปกรณ์จาก gateway ที่พบ แล้วลงทะเบียนเพื่อเริ่มใช้งาน</DialogDescription></DialogHeader>
          <DiscoveryList items={discoveryItems} gateways={topology.gateways.map((g) => g.gateway)} serverTime={serverNow} busy={busy} hiddenUnknown={topology.gateways.reduce((n, g) => n + (snapshot?.discoveryHidden?.[g.gateway.id] ?? 0), 0)} onAdopt={(external, gatewayId) => { setDiscoveryOpen(false); setDialogError(""); setAdopt({ external, gatewayId }); }} />
          {topology.gateways.some((g) => g.gateway.model === Z2M_GATEWAY_MODEL) && <ZigbeeCatalogSearch client={client} />}
        </DialogContent>
      </Dialog>
      {adopt && (
        <AdoptForm
          key={`${adopt.external ?? adopt.draftId ?? "new"}:${adopt.gatewayId ?? ""}`}
          adopt={adopt}
          topology={topology}
          busy={busy}
          error={dialogError}
          initialName={adoptDevice?.name ?? ""}
          initialProfile={drafts.find((d) => d.id === adopt.draftId)?.profile ?? snapshot?.discovery?.find((x) => x.external_id === adopt.external && x.profile)?.profile?.id ?? suggestProfile({ model: adoptDevice?.model, kind: adoptDevice?.kind, hasBeacon: !!adoptDevice?.reading?.beacon, zigbee: topology.gateways.some((g) => g.gateway.id === adopt.gatewayId && g.gateway.model === Z2M_GATEWAY_MODEL) })?.id ?? DEVICE_PROFILES[0].id}
          onCancel={() => setAdopt(null)}
          onSubmit={(input) =>
            void action(async () => {
              const { roaming, ...registration } = input;
              const created = await client.createDevice(registration);
              setSnapshot((previous) => previous ? { ...previous, devices: [...previous.devices, created], discovery: previous.discovery?.filter((d) => d.external_id.toLowerCase() !== created.external_id.toLowerCase()) } : previous);
              let roamingFailed = false;
              if (roaming) {
                // The device exists either way: never leave the dialog open for a retry that would hit a duplicate.
                try {
                  await client.setDeviceRoaming(created.id, true);
                } catch {
                  roamingFailed = true;
                }
              }
              const external = input.external_id.toLowerCase();
              if (adopt.draftId) {
                const draftPos = positions.current[NODE_ID.draft(adopt.draftId)];
                if (draftPos && !positions.current[NODE_ID.device(external)]) positions.current[NODE_ID.device(external)] = draftPos;
                delete positions.current[NODE_ID.draft(adopt.draftId)];
                setDrafts((d) => d.filter((x) => x.id !== adopt.draftId));
              }
              placeDevice(external);
              setAdopt(null);
              setSelection({ kind: "device", external });
              setNotice(roamingFailed ? `ลงทะเบียน ${created.name} แล้ว แต่เปิดโหมดใช้หลาย gateway ไม่สำเร็จ · เปิดได้จากแถบรายละเอียดของอุปกรณ์` : `ลงทะเบียน ${created.name} กับ gateway แล้ว`);
              await reload();
            }, true)
          }
        />
      )}

      {/* Rename / move a registration */}
      <Dialog open={editReg !== null} onOpenChange={(open) => !open && !busy && setEditReg(null)}>
        <DialogContent className="topo-dialog">
          <DialogHeader>
            <DialogTitle>แก้ไขการลงทะเบียน</DialogTitle>
            <DialogDescription>เปลี่ยนชื่อ หรือย้ายไป gateway อื่นใน workspace เดียวกัน · ประวัติข้อมูลยังอยู่กับอุปกรณ์เดิม</DialogDescription>
          </DialogHeader>
          {editReg && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                const reg = editReg;
                const change: { name?: string; gateway_id?: string } = {};
                if (editName.trim() !== reg.name) change.name = editName.trim();
                if (editGateway !== reg.gateway_id) change.gateway_id = editGateway;
                if (!change.name && !change.gateway_id) {
                  setEditReg(null);
                  return;
                }
                void action(async () => {
                  await client.updateDevice(reg.id, change);
                  setEditReg(null);
                  setNotice(change.gateway_id ? "ย้ายอุปกรณ์แล้ว" : "เปลี่ยนชื่อแล้ว");
                  await reload();
                }, true);
              }}
            >
              <label>
                ชื่ออุปกรณ์
                <input value={editName} required onChange={(e) => setEditName(e.target.value)} aria-invalid={editName.trim() !== "" && !validName(editName.trim())} />
                <ByteHint value={editName} />
              </label>
              <label>
                Gateway
                <select value={editGateway} onChange={(e) => setEditGateway(e.target.value)}>
                  {topology.gateways.map((g) => (
                    <option key={g.gateway.id} value={g.gateway.id}>
                      {g.gateway.name} · {gatewayModel(g.gateway.model)?.label ?? g.gateway.model}
                    </option>
                  ))}
                </select>
              </label>
              {editGateway !== editReg.gateway_id && <p className="topo-note">หลังย้าย gateway เดิมจะส่งข้อมูลแทนอุปกรณ์นี้ไม่ได้อีก · ถ้า gateway ปลายทางมีรหัสอุปกรณ์เดียวกันลงทะเบียนอยู่แล้ว ระบบจะปฏิเสธ</p>}
              {dialogError && (
                <p className="topo-warn" role="alert">
                  {dialogError}
                </p>
              )}
              <div className="topo-dialog-actions">
                <button type="button" className="topo-btn" disabled={busy} onClick={() => setEditReg(null)}>
                  ยกเลิก
                </button>
                <button type="submit" className="topo-btn primary" disabled={busy || !validName(editName.trim())}>
                  {busy ? "กำลังบันทึก…" : "บันทึก"}
                </button>
              </div>
            </form>
          )}
        </DialogContent>
      </Dialog>

      <AlertDialog open={moveReg !== null} onOpenChange={(open) => !open && setMoveReg(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>ย้าย “{moveReg?.registration.name}” ไป {topology.gateways.find((g) => g.gateway.id === moveReg?.gatewayId)?.gateway.name}?</AlertDialogTitle>
            <AlertDialogDescription>การลงทะเบียนจะย้ายไป gateway ใหม่ทันที ประวัติข้อมูลยังอยู่ · gateway เดิมจะส่งข้อมูลแทนอุปกรณ์นี้ไม่ได้อีก</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                const m = moveReg!;
                setMoveReg(null);
                void action(async () => {
                  await client.updateDevice(m.registration.id, { gateway_id: m.gatewayId });
                  setNotice("ย้ายอุปกรณ์แล้ว");
                  await reload();
                }, false);
              }}
            >
              ย้าย
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={removeReg !== null} onOpenChange={(open) => !open && setRemoveReg(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>ยกเลิกการลงทะเบียน “{removeReg?.name}”?</AlertDialogTitle>
            <AlertDialogDescription>อุปกรณ์จะออกจากรายการและหยุดรับข้อมูลในชื่อนี้ · ประวัติที่เก็บไว้ไม่ถูกลบ และกู้คืนได้จากแถบรายละเอียดของ gateway หรืออุปกรณ์</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>ไม่ยกเลิก</AlertDialogCancel>
            <AlertDialogAction
              className="is-danger"
              onClick={() => {
                const r = removeReg!;
                setRemoveReg(null);
                void action(async () => {
                  await client.removeDevice(r.id);
                  setNotice(`ยกเลิกการลงทะเบียน ${r.name} แล้ว`);
                  await reload();
                }, false);
              }}
            >
              ยกเลิกการลงทะเบียน
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={rotateId !== null} onOpenChange={(open) => !open && setRotateId(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>สร้างรหัสผ่าน MQTT ใหม่?</AlertDialogTitle>
            <AlertDialogDescription>รหัสเดิมจะใช้เชื่อมต่อใหม่ไม่ได้หลัง broker อัปเดตบัญชี คุณต้องนำรหัสใหม่ไปตั้งใน gateway นี้ก่อนเชื่อมต่อครั้งถัดไป</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                const id = rotateId!;
                setRotateId(null);
                void action(() => issueMQTT(id, true).then(reload), false);
              }}
            >
              สร้างรหัสใหม่
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={revokeId !== null} onOpenChange={(open) => !open && setRevokeId(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>เพิกถอน gateway นี้?</AlertDialogTitle>
            <AlertDialogDescription>Aether จะหยุดรับ packet จาก gateway นี้ทันทีและนำออกจาก canvas · อุปกรณ์ที่ลงทะเบียนไว้กับ gateway นี้จะไม่รับข้อมูลอีก การเพิกถอนย้อนกลับไม่ได้</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>ยกเลิก</AlertDialogCancel>
            <AlertDialogAction
              className="is-danger"
              onClick={() => {
                const id = revokeId!;
                setRevokeId(null);
                void action(async () => {
                  await client.revokeGateway(id);
                  delete positions.current[NODE_ID.gateway(id)];
                  setCredentials(({ [id]: _dropped, ...rest }) => rest);
                  setHttpTokens(({ [id]: _dropped, ...rest }) => rest);
                  setSelection(null);
                  setNotice("เพิกถอน gateway แล้ว");
                  await reload();
                }, false);
              }}
            >
              เพิกถอน
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}

type PanelState = { palette: boolean; inspector: boolean };
const PANELS_KEY = "aether.topology.panels";
function loadPanels(): PanelState {
  try {
    const raw = localStorage.getItem(PANELS_KEY);
    if (raw) {
      const p = JSON.parse(raw) as Partial<PanelState>;
      return { palette: p.palette ?? true, inspector: p.inspector ?? true };
    }
  } catch {
    // ignore unavailable storage
  }
  return { palette: true, inspector: true };
}
function savePanels(p: PanelState) {
  try {
    localStorage.setItem(PANELS_KEY, JSON.stringify(p));
  } catch {
    // ignore unavailable storage
  }
}

function ByteHint({ value }: { value: string }) {
  const n = utf8Bytes(value.trim());
  if (n <= 96) return null;
  return <small className={`topo-bytes ${n > 128 ? "is-over" : ""}`}>{n > 128 ? `ยาวเกิน ${n - 128} bytes · จำกัด 128 bytes (ภาษาไทยตัวละ 3 bytes)` : `${n}/128 bytes`}</small>;
}

function AdoptForm({
  adopt,
  topology,
  busy,
  error,
  initialName,
  initialProfile,
  onCancel,
  onSubmit,
}: {
  adopt: NonNullable<AdoptDialog>;
  topology: Topology;
  busy: boolean;
  error: string;
  initialName: string;
  initialProfile: string;
  onCancel: () => void;
  onSubmit: (input: { gateway_id: string; name: string; external_id: string; profile_id: string; roaming: boolean }) => void;
}) {
  const device = adopt.external ? topology.devices.find((d) => d.external === adopt.external) : undefined;
  const candidates = topology.gateways.filter((g) => !device?.registrations.some((r) => r.gateway_id === g.gateway.id));
  const [gatewayId, setGatewayId] = useState(adopt.gatewayId ?? candidates[0]?.gateway.id ?? "");
  const [name, setName] = useState(initialName);
  const [brand, setBrand] = useState(deviceProfile(initialProfile)?.brand ?? deviceBrands()[0]);
  const [profile, setProfile] = useState(initialProfile);
  const [external, setExternal] = useState(adopt.external ?? "");
  // Wearables are proposed as roaming; the choice follows the profile until the user touches the box.
  const [roamingChoice, setRoamingChoice] = useState<boolean | null>(null);
  // The seeded gateway can disappear (revoked elsewhere) or fall out of the candidate list; keep the select honest.
  const effectiveGateway = candidates.some((g) => g.gateway.id === gatewayId) ? gatewayId : (candidates[0]?.gateway.id ?? "");
  const gateway = topology.gateways.find((g) => g.gateway.id === effectiveGateway);
  // Only profiles whose radio this gateway hears (a Zigbee switch never sits under a BLE gateway, and back).
  const fits = (p: DeviceProfile) => profileFitsGateway(p, gateway?.gateway.model ?? "");
  const brands = deviceBrands().filter((b) => profilesForBrand(b).some(fits));
  // A seeded profile that does not fit this gateway (a BLE suggestion for a Zigbee device) falls back to the first
  // profile that does, so the selects never show a value the form would refuse on submit.
  const picked = deviceProfile(profile);
  const chosen = picked && fits(picked) ? picked : DEVICE_PROFILES.find((p) => fits(p) && p.id === Z2M_GENERIC_PROFILE) ?? DEVICE_PROFILES.find(fits);
  const shownBrand = brands.includes(brand) && chosen?.brand === brand ? brand : chosen?.brand ?? brand;
  const roaming = roamingChoice ?? !!chosen?.wearable;
  const heard = device?.heard.find((h) => h.gatewayId === effectiveGateway);
  const externalValid = validName(external.trim());
  const nameValid = validName(name.trim());

  return (
    <Dialog open onOpenChange={(open) => !open && !busy && onCancel()}>
      <DialogContent className="topo-dialog">
        <DialogHeader>
          <DialogTitle>ลงทะเบียนอุปกรณ์กับ gateway</DialogTitle>
          <DialogDescription>เลือกแบรนด์และรุ่นได้อิสระจาก gateway · ตั้งชื่ออุปกรณ์ แล้วตรวจยืนยันรุ่นก่อนลงทะเบียน</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (!effectiveGateway || !chosen || !externalValid || !nameValid) return;
            onSubmit({ gateway_id: effectiveGateway, name: name.trim(), external_id: external.trim().toLowerCase(), profile_id: chosen.id, roaming });
          }}
        >
          <div className="topo-form-grid">
            <label>
              Gateway
              <select value={effectiveGateway} onChange={(e) => setGatewayId(e.target.value)} required>
                {candidates.length === 0 && <option value="">ไม่มี gateway ที่ลงทะเบียนเพิ่มได้</option>}
                {candidates.map((g) => (
                  <option key={g.gateway.id} value={g.gateway.id}>
                    {g.gateway.name} · {gatewayModel(g.gateway.model)?.label ?? g.gateway.model}
                  </option>
                ))}
              </select>
            </label>
            <label>
              รหัสอุปกรณ์ (MAC / external id)
              <input value={adopt.external ? formatMAC(external) : external} readOnly={!!adopt.external} required placeholder="เช่น AC233FC274EB" onChange={(e) => setExternal(e.target.value.replace(/:/g, ""))} aria-invalid={external !== "" && !externalValid} />
            </label>
            <label>
              แบรนด์
              <select
                value={shownBrand}
                onChange={(e) => {
                  setBrand(e.target.value);
                  setProfile(profilesForBrand(e.target.value).filter(fits)[0]?.id ?? "");
                }}
              >
                {brands.map((b) => (
                  <option key={b}>{b}</option>
                ))}
              </select>
            </label>
            <label>
              รุ่น / profile
              <select value={chosen?.id ?? ""} onChange={(e) => setProfile(e.target.value)}>
                {!chosen && <option value="">เลือกรุ่นที่ใช้กับ gateway นี้ได้</option>}
                {profilesForBrand(shownBrand).filter(fits).map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.model} · {p.id}
                  </option>
                ))}
              </select>
            </label>
            <label className="span-2">
              ชื่ออุปกรณ์
              <input value={name} required placeholder="เช่น ห้องเซิร์ฟเวอร์ · ชั้น 2" onChange={(e) => setName(e.target.value)} aria-invalid={name.trim() !== "" && !nameValid} />
              <ByteHint value={name} />
            </label>
          </div>
          <label className="topo-check">
            <input type="checkbox" checked={roaming} onChange={(e) => setRoamingChoice(e.target.checked)} />
            <span>
              <strong>ใช้ได้หลาย gateway (roaming)</strong>
              <small>สำหรับอุปกรณ์สวมใส่หรือพกพา · gateway ที่เลือกเป็นจุดลงทะเบียนหลัก ส่วนค่าและตำแหน่งจะตามจาก gateway ทุกตัวใน workspace ที่ได้ยิน</small>
            </span>
          </label>
          {chosen?.image && <div className="topo-product"><img src={chosen.image} alt={`${chosen.brand} ${chosen.model}`} /></div>}
          {chosen && <p className="topo-note">{chosen.description}</p>}
          {gateway && (
            <p className="topo-note">
              {heard ? `gateway นี้ได้ยินอุปกรณ์อยู่${heard.rssi != null ? ` (${heard.rssi} dBm)` : ""}` : "gateway นี้ยังไม่ได้ยินอุปกรณ์ในช่วงล่าสุด · ลงทะเบียนล่วงหน้าได้ ข้อมูลจะมาเมื่ออุปกรณ์อยู่ในระยะ"}
            </p>
          )}
          {device && device.registrations.length > 0 && <p className="topo-warn">อุปกรณ์นี้ลงทะเบียนกับ gateway อื่นอยู่แล้ว · การลงทะเบียนซ้ำจะสร้างระเบียนแยกต่อ gateway</p>}
          {error && (
            <p className="topo-warn" role="alert">
              {error}
            </p>
          )}
          <div className="topo-dialog-actions">
            <button type="button" className="topo-btn" onClick={onCancel} disabled={busy}>
              ยกเลิก
            </button>
            <button type="submit" className="topo-btn primary" disabled={busy || !effectiveGateway || !chosen || !nameValid || !externalValid}>
              {busy ? "กำลังลงทะเบียน…" : "ลงทะเบียนอุปกรณ์"}
            </button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  );
}
