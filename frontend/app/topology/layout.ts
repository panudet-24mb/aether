// Layered auto layout (broker → gateways → devices) and per-browser persistence of manual positions.
// Positions are a viewing convenience; they are not stored on the server.

import type { XYPosition } from "@xyflow/react";
import type { DeviceEntity, GatewayEntity } from "./model";
import { primaryGateway } from "./model";

export const NODE_ID = {
  broker: "broker",
  gateway: (id: string) => `gw:${id}`,
  device: (external: string) => `dev:${external}`,
  draft: (id: string) => `draft:${id}`,
};

export type NodeKind = "broker" | "gateway" | "device" | "draft" | "unknown";
const PREFIX_KIND: Record<string, NodeKind> = { gw: "gateway", dev: "device", draft: "draft" };

export const parseNodeId = (id: string): { kind: NodeKind; ref: string } => {
  if (id === "broker") return { kind: "broker", ref: "" };
  const [prefix, ...rest] = id.split(":");
  return { kind: PREFIX_KIND[prefix] ?? "unknown", ref: rest.join(":") };
};

const COL = 176;
const GATEWAY_Y = 250;
const DEVICE_Y = 520;
const DEVICE_ROW = 150;
const PER_ROW = 6;

export type LayoutInput = {
  gateways: GatewayEntity[];
  devices: DeviceEntity[];
  /** Device node ids that are on the canvas (recognised + manually placed). */
  visibleDevices: Set<string>;
  drafts: { id: string; gatewayId: string | null }[];
};

export function autoLayout(input: LayoutInput): Record<string, XYPosition> {
  const positions: Record<string, XYPosition> = {};
  const groups = new Map<string, string[]>(); // gateway id → device node ids
  const orphans: string[] = [];
  for (const g of input.gateways) groups.set(g.gateway.id, []);

  for (const d of input.devices) {
    const id = NODE_ID.device(d.external);
    if (!input.visibleDevices.has(id)) continue;
    const gw = primaryGateway(d);
    if (gw && groups.has(gw)) groups.get(gw)!.push(id);
    else orphans.push(id);
  }
  for (const draft of input.drafts) {
    const id = NODE_ID.draft(draft.id);
    if (draft.gatewayId && groups.has(draft.gatewayId)) groups.get(draft.gatewayId)!.push(id);
    else orphans.push(id);
  }

  let cursor = 0;
  const centers: number[] = [];
  const ordered = [...input.gateways].sort((a, b) => Date.parse(a.gateway.created_at) - Date.parse(b.gateway.created_at));
  for (const g of ordered) {
    const members = groups.get(g.gateway.id)!;
    const columns = Math.max(1, Math.min(PER_ROW, members.length));
    const width = columns * COL;
    const gx = cursor + width / 2 - COL / 2;
    positions[NODE_ID.gateway(g.gateway.id)] = { x: gx, y: GATEWAY_Y };
    centers.push(gx);
    members.forEach((id, i) => {
      positions[id] = { x: cursor + (i % PER_ROW) * COL, y: DEVICE_Y + Math.floor(i / PER_ROW) * DEVICE_ROW };
    });
    cursor += width + COL / 2;
  }
  if (orphans.length) {
    orphans.forEach((id, i) => {
      positions[id] = { x: cursor + (i % PER_ROW) * COL, y: DEVICE_Y + Math.floor(i / PER_ROW) * DEVICE_ROW };
    });
  }
  const brokerX = centers.length ? (Math.min(...centers) + Math.max(...centers)) / 2 : Math.max(0, cursor / 2 - COL / 2);
  positions[NODE_ID.broker] = { x: brokerX, y: 0 };
  return positions;
}

/** Positions for nodes missing from `known`, laid out around the existing ones without moving them. */
export function placeNewNodes(input: LayoutInput, known: Record<string, XYPosition>): Record<string, XYPosition> {
  const fresh = autoLayout(input);
  const out: Record<string, XYPosition> = {};
  for (const [id, pos] of Object.entries(fresh)) {
    if (known[id]) continue;
    // Keep a new device close to its gateway when that gateway was moved by hand.
    const { kind } = parseNodeId(id);
    if (kind === "device" || kind === "draft") {
      const d = input.devices.find((x) => NODE_ID.device(x.external) === id);
      const gwId = d ? primaryGateway(d) : input.drafts.find((x) => NODE_ID.draft(x.id) === id)?.gatewayId ?? null;
      const gwPos = gwId ? known[NODE_ID.gateway(gwId)] : undefined;
      const gwAuto = gwId ? fresh[NODE_ID.gateway(gwId)] : undefined;
      if (gwPos && gwAuto) {
        out[id] = { x: pos.x + (gwPos.x - gwAuto.x), y: pos.y + (gwPos.y - gwAuto.y) };
        continue;
      }
    }
    out[id] = pos;
  }
  return out;
}

export type Persisted = { positions: Record<string, XYPosition>; placed: string[] };

const key = (tenant: string) => `aether.topology.v1.${tenant || "default"}`;

export function loadPersisted(tenant: string): Persisted {
  try {
    const raw = localStorage.getItem(key(tenant));
    if (!raw) return { positions: {}, placed: [] };
    const parsed = JSON.parse(raw) as Partial<Persisted>;
    return { positions: parsed.positions ?? {}, placed: parsed.placed ?? [] };
  } catch {
    return { positions: {}, placed: [] };
  }
}

export function savePersisted(tenant: string, value: Persisted): void {
  try {
    localStorage.setItem(key(tenant), JSON.stringify(value));
  } catch {
    // Storage may be unavailable (private mode); the canvas still works for this session.
  }
}
