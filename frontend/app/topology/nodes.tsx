"use client";
import { memo } from "react";
import { Handle, Position, type Node, type NodeProps } from "@xyflow/react";
import { Activity, Bluetooth, Cloud, CloudOff, DoorOpen, Droplets, FlaskConical, Radio, RadioTower, Router, ShieldAlert, Sun, Thermometer } from "lucide-react";
import { deviceProfile, formatMAC, gatewayModel } from "./catalog";
import type { GatewayHealth } from "./model";

export type BrokerData = { configured: boolean; host: string; port: number | null; scheme: string; receiving: number; total: number };
export type GatewayData = {
  name: string;
  model: string;
  health: GatewayHealth;
  decoded: number;
  nearby: number;
  /** Project name and colour, shown when the canvas mixes several projects. */
  project: string | null;
  projectColor: string | null;
  dim?: boolean;
};
export type DeviceData = {
  name: string;
  external: string;
  profile: string | null;
  adopted: boolean;
  decoded: boolean;
  fresh: boolean;
  simulated: boolean;
  temperature: number | null;
  humidity: number | null;
  kind: string | null;
  /** Product photo when the model is known (registered profile or the tag's own info frame). */
  image: string | null;
  summary: string | null;
  /** Roaming wearable: name of the gateway that hears it best right now (null = out of range). */
  roaming: boolean;
  zone: string | null;
  alert: boolean;
  dim?: boolean;
};
export type DraftData = { profile: string; label: string; dim?: boolean };

export type BrokerNode = Node<BrokerData, "broker">;
export type GatewayNode = Node<GatewayData, "gateway">;
export type DeviceNode = Node<DeviceData, "device">;
export type DraftNode = Node<DraftData, "draft">;
export type AppNode = BrokerNode | GatewayNode | DeviceNode | DraftNode;

export const HEALTH_LABEL: Record<GatewayHealth, string> = {
  none: "ยังไม่มีบัญชี MQTT",
  pending: "รอ broker รับบัญชี",
  ready: "พร้อม · รออุปกรณ์",
  stale: "เคยรับข้อมูล · รอใหม่",
  receiving: "กำลังรับข้อมูล",
};

const Broker = memo(function Broker({ data, selected }: NodeProps<BrokerNode>) {
  return (
    <div className={`topo-node topo-broker ${selected ? "is-selected" : ""} ${data.configured ? "" : "is-unconfigured"}`}>
      <div className="topo-node-icon">{data.configured ? <Cloud size={30} /> : <CloudOff size={30} />}</div>
      <div className="topo-node-label">
        <strong>Aether broker</strong>
        <small>{data.configured ? `${data.scheme}://${data.host}:${data.port}` : "ยังไม่ตั้งค่า MQTT endpoint"}</small>
        <span className="topo-node-meta">
          {data.receiving}/{data.total} gateway กำลังส่ง
        </span>
      </div>
      <Handle type="target" position={Position.Bottom} id="in" />
    </div>
  );
});

const Gateway = memo(function Gateway({ data, selected }: NodeProps<GatewayNode>) {
  const model = gatewayModel(data.model);
  return (
    <div className={`topo-node topo-gateway health-${data.health} ${selected ? "is-selected" : ""} ${data.dim ? "is-dim" : ""}`} title={HEALTH_LABEL[data.health]}>
      <Handle type="source" position={Position.Top} id="up" />
      <div className="topo-node-icon">
        {model?.image ? <img className="topo-photo" src={model.image} alt="" /> : model?.logo ? <img src={model.logo} alt="" /> : <Router size={26} />}
        <span className="topo-node-dot" aria-hidden="true" />
      </div>
      <div className="topo-node-label">
        <strong>{data.name}</strong>
        <small>{model ? `${model.brand} ${model.model}` : data.model}</small>
        {data.project && (
          <span className="topo-node-project" style={{ borderColor: data.projectColor ?? undefined, color: data.projectColor ?? undefined }}>
            {data.project}
          </span>
        )}
        <span className="topo-node-meta">
          <Radio size={11} /> {data.decoded} sensor · {data.nearby} BLE
        </span>
      </div>
      <Handle type="target" position={Position.Bottom} id="down" />
    </div>
  );
});

const KIND_ICON: Record<string, typeof Thermometer> = { environment: Thermometer, motion: Activity, tamper: ShieldAlert, beacon: RadioTower, leak: Droplets, light: Sun, door: DoorOpen };

const Device = memo(function Device({ data, selected }: NodeProps<DeviceNode>) {
  const profile = data.profile ? deviceProfile(data.profile) : undefined;
  const state = data.adopted ? (data.fresh ? "online" : data.decoded ? "stale" : "registered") : data.decoded ? "seen" : "raw";
  const Icon = (data.kind && KIND_ICON[data.kind]) || (data.decoded ? Thermometer : Bluetooth);
  return (
    <div className={`topo-node topo-device state-${state} kind-${data.kind ?? "raw"} ${data.alert ? "is-alert" : ""} ${selected ? "is-selected" : ""} ${data.dim ? "is-dim" : ""}`}>
      <Handle type="source" position={Position.Top} id="up" />
      <div className="topo-node-icon">
        {data.image ? <img className="topo-photo" src={data.image} alt="" /> : <Icon size={24} />}
        <span className="topo-node-dot" aria-hidden="true" />
        {data.simulated && <span className="topo-node-tag">SIM</span>}
        {data.alert && <span className="topo-node-tag is-alert">EVENT</span>}
        {data.roaming && <span className="topo-node-tag is-roam" title="ใช้ได้หลาย gateway">ROAM</span>}
      </div>
      <div className="topo-node-label">
        <strong>{data.name}</strong>
        <small>{profile ? `${profile.brand} ${profile.model}` : formatMAC(data.external)}</small>
        <span className="topo-node-meta">{data.summary ?? (data.adopted ? "ลงทะเบียนแล้ว" : "ยังไม่ adopt")}</span>
        {data.roaming && <span className="topo-node-zone">{data.zone ? `อยู่ที่ ${data.zone}` : "ไม่อยู่ในระยะ"}</span>}
      </div>
    </div>
  );
});

const Draft = memo(function Draft({ data, selected }: NodeProps<DraftNode>) {
  const profile = deviceProfile(data.profile);
  return (
    <div className={`topo-node topo-device state-draft ${selected ? "is-selected" : ""} ${data.dim ? "is-dim" : ""}`}>
      <Handle type="source" position={Position.Top} id="up" />
      <div className="topo-node-icon">{profile?.image ? <img className="topo-photo" src={profile.image} alt="" /> : <FlaskConical size={24} />}</div>
      <div className="topo-node-label">
        <strong>{data.label}</strong>
        <small>{profile ? `${profile.brand} ${profile.model}` : data.profile}</small>
        <span className="topo-node-meta">ลากเส้นไป gateway เพื่อลงทะเบียน</span>
      </div>
    </div>
  );
});

export const nodeTypes = { broker: Broker, gateway: Gateway, device: Device, draft: Draft };
