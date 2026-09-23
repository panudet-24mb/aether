// Types and small helpers for the Automation Studio. The HTTP client itself is the shared one from
// ../topology/api (memory-held access token, one refresh + retry on 401).

export type BlockType =
  | "trigger.event"
  | "trigger.metric"
  | "cond.time"
  | "cond.device"
  | "cond.zone"
  | "logic.all"
  | "logic.any"
  | "action.alert"
  | "action.notify"
  | "action.command";

export type Group = "trigger" | "condition" | "logic" | "action";

export type Op = ">" | ">=" | "<" | "<=";

/** Union of every block's settings; each block reads only its own fields. */
export type BlockData = {
  event_types?: string[];
  external_ids?: string[];
  gateway_ids?: string[];
  external_id?: string;
  metric?: string;
  op?: Op;
  value?: number;
  /** trigger.metric: the comparison must hold this long before the flow fires (0 = immediately). */
  for_sec?: number;
  /** cond.device: a reading older than this cannot answer the question, so the condition is false. */
  max_age_sec?: number;
  from?: string;
  to?: string;
  days?: number[];
  severity?: "info" | "warning" | "critical";
  title?: string;
  channel_ids?: string[];
  message?: string;
  label?: string;
};

export type Block = { id: string; type: BlockType; position: { x: number; y: number }; data: BlockData };
/** sourceHandle is "true"/"false" out of a condition block and absent everywhere else. */
export type Link = { id: string; source: string; target: string; sourceHandle?: "true" | "false" };
export type Definition = { nodes: Block[]; edges: Link[] };

export type Automation = {
  id: string;
  name: string;
  description: string;
  enabled: boolean;
  project_id: string | null;
  definition: Definition;
  revision: number;
  created_at: string;
  updated_at: string;
  last_fired_at: string | null;
  fire_count: number;
};

export type Problem = { node_id?: string; edge_id?: string; code: string; message: string };
export type Outcome = "true" | "false" | "idle" | "ran" | "blocked";
export type TraceAction = { node_id: string; type: BlockType; severity?: string; title?: string; channel_ids?: string[]; message?: string };
export type Trace = { nodes: Record<string, Outcome>; actions: TraceAction[]; blocked?: string[] };
export type RunDetail = { nodes?: Record<string, Outcome>; actions?: TraceAction[]; blocked?: string[]; alerts?: string[]; notifications?: number; error?: string; trigger?: Record<string, string> };
export type Run = {
  id: string;
  automation_id: string;
  trigger_node: string;
  external_id: string;
  gateway_id: string | null;
  status: "fired" | "skipped" | "error";
  detail: RunDetail;
  created_at: string;
};

export type Channel = { id: string; name: string; kind: string; enabled: boolean };

/** Names for the ids a block stores, so the canvas can read like a sentence instead of like a database. */
export type Lookup = {
  device: (externalId: string) => string;
  gateway: (id: string) => string;
  channel: (id: string) => string;
};

export const EMPTY_DEFINITION: Definition = { nodes: [], edges: [] };

export const EVENT_LABEL: Record<string, string> = {
  tamper: "ป้ายถูกแกะ (tamper)",
  tamper_cleared: "ป้ายกลับเข้าที่",
  button: "กดปุ่มฉุกเฉิน",
  leak: "พบน้ำรั่ว",
  leak_cleared: "น้ำรั่วหาย",
  motion: "เริ่มเคลื่อนไหว",
  motion_stopped: "หยุดเคลื่อนไหว",
  offline: "ขาดการติดต่อ",
  online: "กลับมาออนไลน์",
  threshold: "ค่าเกินเกณฑ์",
  threshold_cleared: "ค่ากลับเข้าเกณฑ์",
  zone: "เข้าโซนใหม่",
};

export const METRIC_LABEL: Record<string, string> = { temperature: "อุณหภูมิ", humidity: "ความชื้น", battery: "แบตเตอรี่", rssi: "ความแรงสัญญาณ" };
export const METRIC_UNIT: Record<string, string> = { temperature: "°C", humidity: "%", battery: "%", rssi: "dBm" };
export const SEVERITY_LABEL: Record<string, string> = { info: "ข้อมูล", warning: "เตือน", critical: "วิกฤต" };
export const DAY_LABEL = ["อา", "จ", "อ", "พ", "พฤ", "ศ", "ส"];
export const OP_LABEL: Record<Op, string> = { ">": "มากกว่า", ">=": "ตั้งแต่", "<": "น้อยกว่า", "<=": "ไม่เกิน" };

export const metricLabel = (metric: string) => METRIC_LABEL[metric] ?? metric;
export const metricUnit = (metric: string) => METRIC_UNIT[metric] ?? "";

/** Short, stable ids for new blocks and links; the API accepts [A-Za-z0-9-_.:] up to 64 chars. */
export function localId(prefix: string): string {
  return `${prefix}-${Math.random().toString(36).slice(2, 9)}`;
}

export function groupOf(type: BlockType): Group {
  if (type.startsWith("trigger.")) return "trigger";
  if (type.startsWith("cond.")) return "condition";
  if (type.startsWith("logic.")) return "logic";
  return "action";
}

/** True when the flow, as drawn, can reach `target` from `source` — used to refuse an edge that would close a loop. */
export function reaches(edges: Link[], source: string, target: string): boolean {
  const seen = new Set<string>();
  const queue = [source];
  while (queue.length > 0) {
    const id = queue.shift()!;
    if (id === target) return true;
    if (seen.has(id)) continue;
    seen.add(id);
    for (const e of edges) if (e.source === id) queue.push(e.target);
  }
  return false;
}

export function formatWhen(value: string | null | undefined): string {
  return value ? new Date(value).toLocaleString("th-TH", { hour12: false }) : "—";
}

/** Rounds a number for display without trailing zeroes (2.50 → 2.5, 2.00 → 2). */
export function num(value: number | undefined): string {
  if (value === undefined || Number.isNaN(value)) return "—";
  return String(Math.round(value * 100) / 100);
}
