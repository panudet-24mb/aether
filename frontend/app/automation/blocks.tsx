"use client";
import { memo } from "react";
import { Handle, Position, type Node, type NodeProps } from "@xyflow/react";
import { AlarmClock, Bell, BellRing, Braces, CircleSlash, Clock, Layers, MapPin, Radio, Send, Sigma, Thermometer, type LucideIcon } from "lucide-react";
import { DAY_LABEL, EVENT_LABEL, SEVERITY_LABEL, localId, metricLabel, metricUnit, num, type Block, type BlockData, type BlockType, type Definition, type Group, type Lookup, type Outcome } from "./api";

/** Group colours are the workspace's project palette, so the studio reads like the rest of Aether. */
export const GROUP_COLOR: Record<Group, string> = { trigger: "#a7f3d0", condition: "#80b7ff", logic: "#c4a7ff", action: "#ff8f70" };
const DISABLED_COLOR = "#9fb1b6";

export const GROUP_LABEL: Record<Group, string> = { trigger: "เมื่อ (Trigger)", condition: "ถ้า (Condition)", logic: "ตรรกะ", action: "ทำ (Action)" };

export type BlockSpec = {
  type: BlockType;
  group: Group;
  label: string;
  hint: string;
  icon: LucideIcon;
  /** Drawn in the palette and on the canvas, but never saved into an enabled flow. */
  unavailable?: string;
  make: () => BlockData;
};

export const CATALOG: BlockSpec[] = [
  { type: "trigger.event", group: "trigger", label: "เหตุการณ์จากอุปกรณ์", hint: "เช่น ป้ายถูกแกะ กดปุ่ม น้ำรั่ว ขาดการติดต่อ", icon: BellRing, make: () => ({ event_types: ["tamper"], external_ids: [], gateway_ids: [] }) },
  { type: "trigger.metric", group: "trigger", label: "ค่าจากเซนเซอร์", hint: "อุณหภูมิ ความชื้น แบตเตอรี่ หรือสัญญาณ ผ่านเกณฑ์", icon: Thermometer, make: () => ({ external_id: "", metric: "temperature", op: ">", value: 8, for_sec: 0 }) },
  { type: "cond.time", group: "condition", label: "ช่วงเวลา", hint: "เฉพาะในหรือนอกเวลาทำการ (Asia/Bangkok)", icon: Clock, make: () => ({ from: "08:00", to: "17:00", days: [1, 2, 3, 4, 5] }) },
  { type: "cond.device", group: "condition", label: "ค่าของอุปกรณ์อื่น", hint: "ตรวจค่าล่าสุดของอุปกรณ์ตัวใดก็ได้ใน workspace", icon: Braces, make: () => ({ external_id: "", metric: "temperature", op: ">", value: 8, max_age_sec: 300 }) },
  { type: "cond.zone", group: "condition", label: "โซนของ wearable", hint: "ผู้ป่วยหรือพนักงานอยู่ในโซนของ gateway ใด", icon: MapPin, make: () => ({ external_id: "", gateway_ids: [] }) },
  { type: "logic.all", group: "logic", label: "ทั้งหมด (AND)", hint: "ทำต่อเมื่อทุกเส้นที่ต่อเข้ามาเป็นจริง", icon: Layers, make: () => ({}) },
  { type: "logic.any", group: "logic", label: "อย่างน้อยหนึ่ง (OR)", hint: "ทำเมื่อมีเส้นใดเส้นหนึ่งเป็นจริง", icon: Sigma, make: () => ({}) },
  { type: "action.alert", group: "action", label: "เปิดการแจ้งเตือน", hint: "สร้างการแจ้งเตือนในหน้า “การแจ้งเตือน”", icon: Bell, make: () => ({ severity: "warning", title: "{{device}} · ผิดปกติ" }) },
  { type: "action.notify", group: "action", label: "ส่งข้อความออก", hint: "ส่งผ่าน Webhook, LINE หรืออีเมลที่ตั้งไว้", icon: Send, make: () => ({ channel_ids: [], message: "{{device}} · {{event}} {{value}}" }) },
  {
    type: "action.command",
    group: "action",
    label: "สั่งงานอุปกรณ์",
    hint: "สั่งให้อุปกรณ์ทำงาน เช่น เปิดไฟ หรือสั่นเตือน",
    icon: Radio,
    unavailable: "เร็ว ๆ นี้ · ต้องมีช่องทางสั่งงาน gateway",
    make: () => ({}),
  },
];

export const SPEC: Record<string, BlockSpec> = Object.fromEntries(CATALOG.map((s) => [s.type, s]));

export const GROUPS: Group[] = ["trigger", "condition", "logic", "action"];

export function newBlock(type: BlockType, position: { x: number; y: number }): Block {
  const spec = SPEC[type];
  return { id: localId(type.split(".")[1] ?? "b"), type, position, data: spec ? spec.make() : {} };
}

const join = (list: string[], sep: string) => (list.length > 0 ? list.join(sep) : "");

/** One line of Thai describing what a block is set to, shown on the block itself. */
export function summarize(block: Block, lookup: Lookup): string {
  const d: BlockData = block.data ?? {};
  const device = (id?: string) => (id ? lookup.device(id) : "ยังไม่ได้เลือกอุปกรณ์");
  const unit = (metric?: string) => (metric ? metricUnit(metric) : "");
  switch (block.type) {
    case "trigger.event": {
      const events = (d.event_types ?? []).map((t) => EVENT_LABEL[t] ?? t);
      const who = (d.external_ids ?? []).length > 0 ? (d.external_ids ?? []).map(lookup.device).join(", ") : "อุปกรณ์ใดก็ได้";
      const where = (d.gateway_ids ?? []).length > 0 ? ` ผ่าน ${(d.gateway_ids ?? []).map(lookup.gateway).join(", ")}` : "";
      return `${join(events, " หรือ ") || "ยังไม่ได้เลือกเหตุการณ์"} ของ ${who}${where}`;
    }
    case "trigger.metric": {
      const hold = (d.for_sec ?? 0) > 0 ? ` นาน ${d.for_sec} วิ` : "";
      return `${metricLabel(d.metric ?? "")} ของ ${device(d.external_id)} ${d.op ?? "?"} ${num(d.value)} ${unit(d.metric)}${hold}`.replace(/\s+/g, " ").trim();
    }
    case "cond.time": {
      const days = (d.days ?? []).length > 0 && (d.days ?? []).length < 7 ? ` · ${(d.days ?? []).map((n) => DAY_LABEL[n]).join(" ")}` : " · ทุกวัน";
      return `เวลา ${d.from ?? "--:--"}–${d.to ?? "--:--"}${days}`;
    }
    case "cond.device": {
      const age = d.max_age_sec && d.max_age_sec !== 300 ? ` (ค่าล่าสุดไม่เกิน ${d.max_age_sec} วิ)` : "";
      return `${metricLabel(d.metric ?? "")} ของ ${device(d.external_id)} ${d.op ?? "?"} ${num(d.value)} ${unit(d.metric)}${age}`.replace(/\s+/g, " ").trim();
    }
    case "cond.zone": {
      const zones = (d.gateway_ids ?? []).length > 0 ? (d.gateway_ids ?? []).map(lookup.gateway).join(", ") : "โซนใดก็ได้";
      return `${device(d.external_id)} อยู่ในโซน ${zones}`;
    }
    case "logic.all":
      return "ทุกเส้นที่ต่อเข้ามาต้องเป็นจริง";
    case "logic.any":
      return "มีเส้นใดเส้นหนึ่งเป็นจริงก็พอ";
    case "action.alert":
      return `แจ้งเตือนระดับ${SEVERITY_LABEL[d.severity ?? "warning"] ?? d.severity} · “${d.title || "ยังไม่ได้ใส่ข้อความ"}”`;
    case "action.notify": {
      const channels = (d.channel_ids ?? []).map(lookup.channel);
      return `ส่งไป ${join(channels, ", ") || "ยังไม่ได้เลือกช่องทาง"}`;
    }
    case "action.command":
      return "ยังสั่งงานอุปกรณ์ไม่ได้ · Aether ยังไม่มีช่องทางส่งคำสั่งกลับไปที่ gateway";
    default:
      return "";
  }
}

/** The whole flow as one Thai sentence, shown in the inspector when nothing is selected. */
export function sentence(def: Definition, lookup: Lookup): string {
  const part = (group: Group) => def.nodes.filter((n) => SPEC[n.type]?.group === group).map((n) => summarize(n, lookup));
  const triggers = part("trigger");
  const conditions = part("condition");
  const actions = def.nodes.filter((n) => SPEC[n.type]?.group === "action").map((n) => (n.type === "action.command" ? "สั่งงานอุปกรณ์ (ยังทำไม่ได้)" : summarize(n, lookup)));
  if (triggers.length === 0 && actions.length === 0) return "";
  const lines = [`เมื่อ ${join(triggers, " หรือ ") || "…"}`];
  if (conditions.length > 0) lines.push(`ถ้า ${conditions.join(" และ ")}`);
  lines.push(`ให้ ${join(actions, " และ ") || "…"}`);
  return lines.join(" ");
}

export type StudioNodeData = {
  block: Block;
  lookup: Lookup;
  /** Outcome of the last dry run, or null when no trace is on screen. */
  outcome: Outcome | null;
  /** Validation message for this block; a non-empty string outlines the block in red. */
  problem: string;
};
export type StudioNode = Node<StudioNodeData, "block">;

const StudioBlock = memo(function StudioBlock({ data, selected }: NodeProps<StudioNode>) {
  const { block, lookup, outcome, problem } = data;
  const spec = SPEC[block.type];
  const group = spec?.group ?? "action";
  const Icon = spec?.icon ?? CircleSlash;
  const colour = spec?.unavailable ? DISABLED_COLOR : GROUP_COLOR[group];
  const isCondition = group === "condition";
  const classes = ["au-node", `is-${group}`, selected ? "is-selected" : "", problem ? "is-problem" : "", spec?.unavailable ? "is-unavailable" : "", outcome ? `is-${outcome}` : ""];
  return (
    <div className={classes.filter(Boolean).join(" ")} title={problem || summarize(block, lookup)} style={{ ["--au-block" as string]: colour }}>
      <span className="au-node-strip" aria-hidden="true" />
      {group !== "trigger" && <Handle type="target" position={Position.Left} className="au-handle" />}
      <div className="au-node-body">
        <div className="au-node-head">
          <span className="au-node-icon">
            <Icon size={15} />
          </span>
          <strong>{block.data?.label || spec?.label || block.type}</strong>
        </div>
        <small>{summarize(block, lookup)}</small>
        {spec?.unavailable && <em className="au-node-badge">{spec.unavailable}</em>}
        {problem && (
          <em className="au-node-error" role="note">
            {problem}
          </em>
        )}
      </div>
      {isCondition ? (
        <>
          <Handle id="true" type="source" position={Position.Right} className="au-handle au-handle-true" style={{ top: "38%" }} />
          <span className="au-handle-label is-true">ใช่</span>
          <Handle id="false" type="source" position={Position.Right} className="au-handle au-handle-false" style={{ top: "76%" }} />
          <span className="au-handle-label is-false">ไม่ใช่</span>
        </>
      ) : (
        group !== "action" && <Handle type="source" position={Position.Right} className="au-handle" />
      )}
    </div>
  );
});

export const nodeTypes = { block: StudioBlock };

export type Template = { id: string; name: string; description: string; icon: LucideIcon; build: () => Definition };

/** One-click starters for an empty workspace; every block is left for the operator to point at real devices. */
export const TEMPLATES: Template[] = [
  {
    id: "tamper",
    name: "ป้ายกันถอดถูกแกะ → แจ้งเตือนวิกฤต",
    description: "เมื่อ tag กันถอดถูกแกะออกจากทรัพย์สิน เปิดการแจ้งเตือนระดับวิกฤตทันที",
    icon: BellRing,
    build: () => ({
      nodes: [
        { id: "t1", type: "trigger.event", position: { x: 40, y: 120 }, data: { event_types: ["tamper"], external_ids: [], gateway_ids: [] } },
        { id: "a1", type: "action.alert", position: { x: 420, y: 120 }, data: { severity: "critical", title: "ป้ายกันถอดถูกแกะ · {{device}}" } },
      ],
      edges: [{ id: "e1", source: "t1", target: "a1" }],
    }),
  },
  {
    id: "coldroom",
    name: "ห้องเย็นอุ่นเกิน 8 °C นาน 5 นาที → แจ้ง LINE",
    description: "กันสัญญาณรบกวน: ต้องอุ่นเกินเกณฑ์ต่อเนื่องห้านาทีจึงแจ้ง แล้วส่งออกช่องทางที่เลือก",
    icon: Thermometer,
    build: () => ({
      nodes: [
        { id: "t1", type: "trigger.metric", position: { x: 40, y: 120 }, data: { external_id: "", metric: "temperature", op: ">", value: 8, for_sec: 300 } },
        { id: "a1", type: "action.alert", position: { x: 420, y: 40 }, data: { severity: "warning", title: "ห้องเย็นอุ่นเกิน · {{device}} {{value}} °C" } },
        { id: "n1", type: "action.notify", position: { x: 420, y: 210 }, data: { channel_ids: [], message: "ห้องเย็น {{device}} อุ่นเกินเกณฑ์: {{value}} °C" } },
      ],
      edges: [
        { id: "e1", source: "t1", target: "a1" },
        { id: "e2", source: "t1", target: "n1" },
      ],
    }),
  },
  {
    id: "afterhours",
    name: "ผู้ป่วยกดปุ่มฉุกเฉินนอกเวลาทำการ → แจ้งเตือน",
    description: "ปุ่มฉุกเฉินที่กดนอกเวลา 08:00–17:00 เปิดการแจ้งเตือนวิกฤต ในเวลาทำการเปิดเป็นระดับข้อมูล",
    icon: AlarmClock,
    build: () => ({
      nodes: [
        { id: "t1", type: "trigger.event", position: { x: 40, y: 140 }, data: { event_types: ["button"], external_ids: [], gateway_ids: [] } },
        { id: "c1", type: "cond.time", position: { x: 400, y: 140 }, data: { from: "08:00", to: "17:00", days: [1, 2, 3, 4, 5] } },
        { id: "a1", type: "action.alert", position: { x: 760, y: 40 }, data: { severity: "info", title: "กดปุ่มฉุกเฉินในเวลาทำการ · {{device}}" } },
        { id: "a2", type: "action.alert", position: { x: 760, y: 240 }, data: { severity: "critical", title: "กดปุ่มฉุกเฉินนอกเวลาทำการ · {{device}}" } },
      ],
      edges: [
        { id: "e1", source: "t1", target: "c1" },
        { id: "e2", source: "c1", target: "a1", sourceHandle: "true" },
        { id: "e3", source: "c1", target: "a2", sourceHandle: "false" },
      ],
    }),
  },
];
