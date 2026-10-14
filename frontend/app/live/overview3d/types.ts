// The overview's 3D "command constellation" (docs/platform/overview-3d.md). These are the shapes the stage reads:
// structural slices of the overview's own Card and gateway rows, so the board and the stage show the same data.

import { switchGangs } from "../../topology/catalog";

export type StageStatus = "alert" | "online" | "stale" | "offline";

/** The part of an overview card the stage uses (overview.tsx `Card` satisfies it). */
export type StageCard = {
  key: string;
  deviceId?: string;
  name: string;
  kind: string;
  model: string;
  external: string;
  registered: boolean;
  gatewayId: string;
  gatewayName: string;
  projectId: string | null;
  status: StageStatus;
  sos: boolean;
  reasons: string[];
  reading: { received_at: string; metrics?: Record<string, number> } | null;
  occupancy?: { occupied: boolean };
  door?: { open: boolean | null; leftOpen: boolean };
};

/** The part of an overview gateway row the stage uses. */
export type StageGateway = { id: string; name: string; projectId: string | null; online: boolean };

export type StageProject = { id: string; name: string; color: string };

/** A hub on the platform: a gateway (or the bucket of devices whose gateway is unknown). */
export type Hub = { id: string; name: string; online: boolean; projectId: string | null; color: string | null; count: number };

export type Vec3 = [number, number, number];

export type HubPlace = { hub: Hub; at: Vec3; radius: number };
export type NodePlace = { card: StageCard; at: Vec3; hub: string; ring: number };

/** Node body shape, by device kind. */
export type Shape = "sphere" | "cube" | "diamond" | "capsule" | "slab" | "gem";

export const SHAPES: Shape[] = ["sphere", "cube", "diamond", "capsule", "slab", "gem"];

const SHAPE_BY_KIND: Record<string, Shape> = {
  environment: "sphere", info: "sphere", zigbee: "sphere", light: "sphere", climate: "sphere",
  switch: "cube", metering: "cube", lock: "cube",
  sos: "diamond", remote: "diamond", beacon: "diamond", hazard: "diamond", tamper: "diamond", leak: "diamond",
  occupancy: "capsule", motion: "capsule",
  door: "slab", cover: "slab",
  lighting: "gem", fan: "gem",
};

export const shapeOf = (kind: string): Shape => SHAPE_BY_KIND[kind] ?? "sphere";

/** Commands: the board's own rule for a card that carries on/off controls. */
/** Card kinds that can be commanded (Zigbee2MQTT and Tuya); the board and the stage share this set. Tuya Wi‑Fi
 *  devices are all kind "tuya": one counts only once its controls report a switchable output (outputs.ts). */
export const CONTROL_KINDS = new Set(["switch", "lighting", "cover", "climate", "fan", "tuya"]);
export const controllable = (c: StageCard) => !!c.deviceId && c.registered && CONTROL_KINDS.has(c.kind) && c.status !== "offline";

/** Label priority: SOS (0) > alert (1) > controllable (2) > everything else (3). Lower wins. `canControl` is the
 *  stage's rule (it also knows which Tuya devices reported a switch). */
export const labelTier = (c: StageCard, canControl: (c: StageCard) => boolean = controllable) => (c.sos ? 0 : c.status === "alert" ? 1 : canControl(c) ? 2 : 3);

/** Whether a switched output (lamp, plug, gang) reports on, from the card's latest reading; null when unknown. */
export function poweredOn(c: StageCard): boolean | null {
  const m = c.reading?.metrics;
  if (!m) return null;
  if (m.state === 1 || m.state === 0) return m.state === 1;
  const gangs = switchGangs(m);
  return gangs.length ? gangs.some(([, on]) => on) : null;
}
