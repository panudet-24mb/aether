"use client";
import { useEffect, useRef, useState } from "react";
import { Activity, Battery, BellRing, Bluetooth, Check, Copy, DoorOpen, Download, Droplets, Eye, EyeOff, ExternalLink, GraduationCap, KeyRound, Link2, PanelRightClose, Pencil, RotateCcw, Unlink, Radio, RadioTower, RefreshCw, ShieldAlert, ShieldOff, Thermometer, Trash2, Wifi, X } from "lucide-react";
import DiscoveryList from "./discovery";
import SignalPanel, { type LearnedSignal, type SignalClient } from "./signals";
import type { Discovery, Device, GatewayCreated, MQTTCredentials, MQTTSettings, Project } from "./api";
import { deviceProfile, formatMAC, gatewayModel, suggestProfile } from "./catalog";
import { HEALTH_LABEL } from "./nodes";
import { isFresh, type DeviceEntity, type GatewayEntity, type Topology, currentGateway } from "./model";

export type Selection = { kind: "broker" } | { kind: "gateway"; id: string } | { kind: "device"; external: string } | { kind: "draft"; id: string; profile: string } | null;

export type InspectorProps = {
  selection: Selection;
  topology: Topology;
  discovery: Discovery[];
  /** Credentials shown once, kept in memory for the gateway they belong to. */
  credentials: Record<string, MQTTCredentials>;
  httpTokens: Record<string, GatewayCreated>;
  busy: boolean;
  onClose: () => void;
  onIssueMQTT: (gatewayId: string) => void;
  onRotate: (gatewayId: string) => void;
  onRevoke: (gatewayId: string) => void;
  onAdopt: (external: string | null, gatewayId: string | null, draftId?: string) => void;
  onOpenStudio: (sourceKey: string) => void;
  onRemoveDraft: (draftId: string) => void;
  onSelectGateway: (gatewayId: string) => void;
  onSelectDevice: (external: string) => void;
  onNotice: (message: string) => void;
  projects: Project[];
  /** Moves the gateway (and with it all its devices) to another project, or to none. */
  onSetGatewayProject: (gatewayId: string, projectId: string | null) => void;
  /** Withdrawn registrations, shown so they can be restored. */
  removedDevices: Device[];
  onEditRegistration: (registration: Device) => void;
  /** Follow a wearable across every gateway (true) or only its registered gateway (false). */
  onSetRoaming: (registrations: Device[], roaming: boolean) => void;
  onRemoveRegistration: (registration: Device) => void;
  onRestoreRegistration: (registration: Device) => void;
  /** Collapses the whole panel (distinct from onClose, which only clears the selection). */
  onHide: () => void;
  /** Authenticated API client, used by the learned-signal panel ("สอนสัญญาณ") to run its own calls. */
  client: SignalClient;
};

/** Clipboard API needs a secure context; on-prem LAN over plain HTTP falls back to a selection + execCommand copy. */
async function copyText(value: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(value);
      return true;
    }
  } catch {
    // fall through to the legacy path
  }
  try {
    const area = document.createElement("textarea");
    area.value = value;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    const ok = document.execCommand("copy");
    area.remove();
    return ok;
  } catch {
    return false;
  }
}

function CopyButton({ value, label, onNotice }: { value: string; label: string; onNotice: (m: string) => void }) {
  const [done, setDone] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);
  return (
    <button
      type="button"
      className="topo-icon-btn"
      aria-label={`คัดลอก ${label}`}
      onClick={() => {
        void copyText(value).then((ok) => {
          if (!ok) {
            onNotice("คัดลอกไม่สำเร็จ · เลือกข้อความแล้วกด Ctrl+C / ⌘C");
            return;
          }
          setDone(true);
          onNotice(`คัดลอก ${label} แล้ว`);
          if (timer.current) clearTimeout(timer.current);
          timer.current = setTimeout(() => setDone(false), 1500);
        });
      }}
    >
      {done ? <Check size={14} /> : <Copy size={14} />}
    </button>
  );
}

function Field({ label, value, onNotice, mono = true }: { label: string; value: string; onNotice: (m: string) => void; mono?: boolean }) {
  return (
    <div className="topo-field">
      <dt>{label}</dt>
      <dd>
        {mono ? <code>{value}</code> : <span>{value}</span>}
        <CopyButton value={value} label={label} onNotice={onNotice} />
      </dd>
    </div>
  );
}

function download(name: string, data: unknown) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: "application/json" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a); // Firefox ignores click() on a detached anchor
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000); // revoking synchronously can cancel the download
}

function Secret({ label, value, onNotice }: { label: string; value: string; onNotice: (m: string) => void }) {
  const [visible, setVisible] = useState(false);
  return (
    <div className="topo-secret">
      <span>{label} · แสดงให้เก็บครั้งเดียว</span>
      <div>
        <input aria-label={label} readOnly autoComplete="off" type={visible ? "text" : "password"} value={value} />
        <button type="button" className="topo-icon-btn" aria-label={visible ? "ซ่อน" : "แสดง"} onClick={() => setVisible((v) => !v)}>
          {visible ? <EyeOff size={14} /> : <Eye size={14} />}
        </button>
        <CopyButton value={value} label={label} onNotice={onNotice} />
      </div>
    </div>
  );
}

function BrokerPanel({ settings, topology }: { settings: MQTTSettings | null; topology: Topology }) {
  const receiving = topology.gateways.filter((g) => g.health === "receiving").length;
  return (
    <>
      <header className="topo-inspector-head">
        <div>
          <span className="topo-kicker">BROKER</span>
          <h2>Aether MQTT broker</h2>
        </div>
      </header>
      {settings ? (
        <dl className="topo-fields">
          <div className="topo-field">
            <dt>Endpoint</dt>
            <dd>
              <code>
                {settings.scheme}://{settings.host}:{settings.port}
              </code>
            </dd>
          </div>
          <div className="topo-field">
            <dt>TLS</dt>
            <dd>
              <span>{settings.tls ? "เปิด · ติดตั้ง CA ของ server บน gateway" : "ปิด · โหมดพัฒนาใน LAN เท่านั้น"}</span>
            </dd>
          </div>
          {settings.plaintext && (
            <div className="topo-field">
              <dt>ไม่มี TLS</dt>
              <dd>
                <code>
                  {settings.plaintext.scheme}://{settings.host}:{settings.plaintext.port}
                </code>
                <span className="topo-note">สำหรับ gateway ที่ไม่รองรับ TLS · รหัสผ่านและข้อมูลไม่เข้ารหัส ใช้ในเครือข่ายที่แยกไว้เท่านั้น</span>
              </dd>
            </div>
          )}
          <div className="topo-field">
            <dt>QoS / Keep alive</dt>
            <dd>
              <span>
                {settings.qos} / {settings.keep_alive} s
              </span>
            </dd>
          </div>
        </dl>
      ) : (
        <p className="topo-warn">server ยังไม่ตั้งค่า MQTT public endpoint (MQTT_PUBLIC_HOST / PORT / SCHEME) จึงยังออกบัญชี MQTT ให้ gateway ไม่ได้</p>
      )}
      <p className="topo-note">
        {receiving} จาก {topology.gateways.length} gateway กำลังส่งข้อมูลภายใน 60 วินาที · สถานะยืนยันจาก packet ที่ Aether รับ ไม่ใช่การเปิด Wi‑Fi ของ gateway
      </p>
    </>
  );
}

function Step({ done, active, label, detail }: { done: boolean; active?: boolean; label: string; detail?: string }) {
  return (
    <li className={`topo-step ${done ? "is-done" : active ? "is-active" : ""}`}>
      <span className="topo-step-dot">{done ? <Check size={11} /> : null}</span>
      <div>
        <strong>{label}</strong>
        {detail && <small>{detail}</small>}
      </div>
    </li>
  );
}

function GatewayPanel({ g, topology, credentials, httpToken, busy, p }: { g: GatewayEntity; topology: Topology; credentials?: MQTTCredentials; httpToken?: GatewayCreated; busy: boolean; p: InspectorProps }) {
  const [tab, setTab] = useState("devices");
  const model = gatewayModel(g.gateway.model);
  const settings = topology.broker.settings;
  const id = g.gateway.id;
  const hasAccount = !!g.state?.revision;
  const applied = hasAccount && g.state!.applied_revision === g.state!.revision;
  // Registered devices plus supported models waiting to be registered; raw MACs (phones, foreign beacons) stay out.
  const devices = topology.devices.filter((d) => d.registrations.some((r) => r.gateway_id === id) || (d.heard.some((h) => h.gatewayId === id) && p.discovery.some((x) => x.gateway_id === id && x.external_id === d.external)));
  const adopted = devices.filter((d) => d.registrations.some((r) => r.gateway_id === id));
  const mqttFields: [string, string][] = settings
    ? [
        ["Server / Host", settings.host],
        ["Port", String(settings.port)],
        ["Protocol", `${settings.scheme}://`],
        ...(settings.plaintext ? ([["ถ้า gateway ไม่รองรับ TLS", `${settings.plaintext.scheme}://${settings.host}:${settings.plaintext.port}`]] as [string, string][]) : []),
        ["Username", `gw-${id}`],
        ["Client ID", `gw-${id}`],
        ["Post topic", `/aether/gateways/${id}/status`],
        ["Subscribe topic", `/aether/gateways/${id}/action`],
        ["Reply topic", `/aether/gateways/${id}/response`],
        ["QoS", String(settings.qos)],
        ["Keep alive", `${settings.keep_alive} s`],
      ]
    : [];

  return (
    <>
      <header className="topo-inspector-head">
        <div>
          <span className="topo-kicker">GATEWAY · {model ? `${model.brand} ${model.model}` : g.gateway.model}</span>
          <h2>{g.gateway.name}</h2>
        </div>
        <span className={`topo-chip health-${g.health}`}>{HEALTH_LABEL[g.health]}</span>
      </header>

      <nav className="topo-actions" role="tablist" aria-label="รายละเอียด gateway">{[["devices", "อุปกรณ์"], ["discovery", "ค้นพบใหม่"], ["config", "ตั้งค่า"]].map(([id, label]) => <button key={id} type="button" role="tab" aria-selected={tab === id} className={`topo-btn ${tab === id ? "primary" : ""}`} onClick={() => setTab(id)}>{label}</button>)}</nav>
      <div hidden={tab !== "discovery"}>
      <h3 className="topo-h3">อุปกรณ์ที่พบใหม่ <span className="topo-count">{p.discovery.filter((d) => d.gateway_id === id).length}</span></h3>
      <DiscoveryList items={p.discovery} gateways={[g.gateway]} gatewayId={id} serverTime={topology.serverTime} busy={busy} onAdopt={p.onAdopt} />

      </div><div hidden={tab !== "config"}>
      <label className="topo-project-select">
        โปรเจค
        <select value={g.gateway.project_id ?? ""} disabled={busy} onChange={(e) => p.onSetGatewayProject(id, e.target.value || null)} aria-label="โปรเจคของ gateway นี้">
          <option value="">ยังไม่จัดโปรเจค</option>
          {p.projects.map((pr) => (
            <option key={pr.id} value={pr.id}>
              {pr.name}
            </option>
          ))}
        </select>
      </label>

      {model?.image && (
        <div className="topo-product">
          <img src={model.image} alt={`${model.brand} ${model.model}`} />
          <small>ภาพสินค้าจากผู้ผลิต ใช้เพื่อระบุรุ่น</small>
        </div>
      )}

      {model?.transport === "mqtt" ? (
        <ol className="topo-steps">
          <Step done={hasAccount} active={!hasAccount} label="1 · บัญชี MQTT" detail={hasAccount ? `revision ${g.state!.revision}` : "สร้างบัญชีเพื่อรับ username / password"} />
          <Step done={applied} active={hasAccount && !applied} label="2 · Broker รับบัญชี" detail={applied ? "provisioner ตั้งค่าแล้ว" : hasAccount ? "กำลังรอ provisioner…" : undefined} />
          <Step done={!!g.lastPacketAt} active={applied && !g.lastPacketAt} label="3 · รับ packet แรก" detail={g.lastPacketAt ? `ล่าสุด ${new Date(g.lastPacketAt).toLocaleString("th-TH")}` : "นำค่าไปตั้งในแอป Gateway Config แล้วรอ"} />
          <Step done={g.decodedSensors > 0} active={!!g.lastPacketAt && g.decodedSensors === 0} label="4 · ถอดรหัสอุปกรณ์" detail={`${g.decodedSensors} sensor · ${g.nearbyDevices} BLE ใกล้เคียง`} />
        </ol>
      ) : (
        <ol className="topo-steps">
          <Step done={!!httpToken} active={!httpToken} label="1 · Token สำหรับ HTTP Basic" detail={httpToken ? "แสดงครั้งเดียวด้านล่าง" : "token แสดงตอนสร้าง gateway เท่านั้น"} />
          <Step done={!!g.lastPacketAt} active={!g.lastPacketAt} label="2 · รับ packet แรก" detail={g.lastPacketAt ? `ล่าสุด ${new Date(g.lastPacketAt).toLocaleString("th-TH")}` : "POST JSON ไปที่ capture path"} />
        </ol>
      )}

      {model?.transport === "mqtt" && !hasAccount && (
        <div className="topo-callout">
          gateway นี้ยังไม่มีบัญชี MQTT ที่ออกจาก Aether · ระบบไม่เปลี่ยนค่าในตัวอุปกรณ์ให้อัตโนมัติ
          <button type="button" className="topo-btn primary" disabled={busy || !settings} onClick={() => p.onIssueMQTT(id)}>
            <KeyRound size={15} /> สร้างบัญชี MQTT
          </button>
        </div>
      )}

      {credentials && (
        <div className="topo-secret-box">
          <Secret label="MQTT password" value={credentials.password} onNotice={p.onNotice} />
          <button type="button" className="topo-btn" onClick={() => download(`aether-gateway-${id}.json`, credentials)}>
            <Download size={15} /> ดาวน์โหลดค่าตั้งค่าและรหัสผ่าน
          </button>
          <small>ระบบไม่แสดงรหัสเดิมอีก · เก็บไฟล์นี้ก่อนเปลี่ยนหน้า</small>
        </div>
      )}
      {httpToken && (
        <div className="topo-secret-box">
          <Secret label="Gateway token" value={httpToken.token} onNotice={p.onNotice} />
          <dl className="topo-fields">
            <Field label="Capture path" value={httpToken.capture_path} onNotice={p.onNotice} />
            <Field label="Basic username" value={id} onNotice={p.onNotice} />
          </dl>
          <button type="button" className="topo-btn" onClick={() => download(`aether-gateway-${id}.json`, { gateway_id: id, token: httpToken.token, capture_path: httpToken.capture_path })}>
            <Download size={15} /> ดาวน์โหลด token
          </button>
        </div>
      )}

      {model?.transport === "mqtt" && hasAccount && (
        <>
          <h3 className="topo-h3">ค่าสำหรับแอป Gateway Config</h3>
          <dl className="topo-fields">
            {mqttFields.map(([k, v]) => (
              <Field key={k} label={k} value={v} onNotice={p.onNotice} />
            ))}
          </dl>
          {!credentials && <p className="topo-note">รหัสผ่านแสดงเฉพาะตอนสร้างบัญชี ใช้ไฟล์ที่ดาวน์โหลดไว้ หรือสร้างรหัสใหม่</p>}
          {settings && !settings.tls && <p className="topo-note">โหมดพัฒนา: TCP ไม่มี SSL · gateway ต้องเข้าถึง server ผ่าน LAN นี้ได้</p>}
          <div className="topo-actions">
            <button type="button" className="topo-btn" disabled={busy} onClick={() => p.onRotate(id)}>
              <RefreshCw size={15} /> สร้างรหัสผ่านใหม่
            </button>
          </div>
        </>
      )}

      </div><div hidden={tab !== "devices"}>
      <h3 className="topo-h3">
        อุปกรณ์ที่เกี่ยวข้อง <span className="topo-count">{devices.length}</span>
      </h3>
      {devices.length === 0 && <p className="topo-note">{g.lastPacketAt ? "ได้รับ packet แล้ว แต่ยังไม่พบ BLE advertisement · เปิดเซนเซอร์ให้อยู่ใกล้ gateway" : "รอ gateway ส่งข้อมูล อุปกรณ์จะปรากฏอัตโนมัติ"}</p>}
      <ul className="topo-list">
        {devices.slice(0, 40).map((d) => {
          const link = d.heard.find((h) => h.gatewayId === id);
          const isAdopted = adopted.includes(d);
          return (
            <li key={d.external}>
              <button type="button" className="topo-list-item" onClick={() => p.onSelectDevice(d.external)}>
                {d.reading ? <Thermometer size={14} /> : <Bluetooth size={14} />}
                <span>
                  <strong>{d.name}</strong>
                  <small>
                    {formatMAC(d.external)}
                    {link?.rssi != null ? ` · ${link.rssi} dBm` : ""}
                  </small>
                </span>
                <em className={isAdopted ? "is-adopted" : ""}>{isAdopted ? "adopted" : "ค้นพบ"}</em>
              </button>
            </li>
          );
        })}
      </ul>

      <RemovedList items={p.removedDevices.filter((r) => r.gateway_id === id)} gatewayName={() => g.gateway.name} busy={busy} onRestore={p.onRestoreRegistration} />

      </div><div hidden={tab !== "config"} className="topo-danger">
        <button type="button" className="topo-btn danger" disabled={busy} onClick={() => p.onRevoke(id)}>
          <ShieldOff size={15} /> เพิกถอน gateway
        </button>
        <small>หยุดรับข้อมูลจาก gateway นี้ทันที · ประวัติที่เก็บไว้ไม่ถูกลบ</small>
      </div>
    </>
  );
}

function Tile({ icon, label, value, unit }: { icon: React.ReactNode; label: string; value: string | number; unit?: string }) {
  return (
    <div>
      {icon}
      <span>{label}</span>
      <strong>
        {value}
        {unit && <small>{unit}</small>}
      </strong>
    </div>
  );
}

const EVENT_LABEL: Record<string, string> = { tamper: "ป้ายถูกถอด (tamper)", tamper_cleared: "tamper กลับสู่ปกติ", button: "กดปุ่ม / instance เปลี่ยน", leak: "พบน้ำรั่ว", leak_cleared: "น้ำรั่วหาย", motion: "เคลื่อนไหว", motion_stopped: "หยุดเคลื่อนไหว", light: "พบแสง", offline: "ขาดการติดต่อ", online: "กลับมาออนไลน์", threshold: "ค่าเกินเกณฑ์", threshold_cleared: "ค่ากลับเข้าเกณฑ์", zone: "เข้าโซนใหม่", door: "ประตูเปิดอยู่", door_open: "เปิดประตู", door_closed: "ปิดประตู", occupied: "มีคนในพื้นที่", vacant: "ไม่มีคนแล้ว" };

function RemovedList({ items, gatewayName, busy, onRestore }: { items: Device[]; gatewayName: (id: string) => string; busy: boolean; onRestore: (r: Device) => void }) {
  if (items.length === 0) return null;
  return (
    <>
      <h3 className="topo-h3">
        <Unlink size={14} /> ยกเลิกการลงทะเบียนแล้ว <span className="topo-count">{items.length}</span>
      </h3>
      <ul className="topo-list">
        {items.map((r) => (
          <li key={r.id} className="topo-reg is-removed">
            <span className="topo-list-item is-static">
              <Unlink size={14} />
              <span>
                <strong>{r.name}</strong>
                <small>
                  {formatMAC(r.external_id)} · {gatewayName(r.gateway_id)} · ยกเลิก {r.removed_at ? new Date(r.removed_at).toLocaleDateString("th-TH") : ""}
                </small>
              </span>
            </span>
            <button type="button" className="topo-icon-btn" disabled={busy} aria-label={`กู้คืนการลงทะเบียน ${r.name}`} title="กู้คืนการลงทะเบียน" onClick={() => onRestore(r)}>
              <RotateCcw size={14} />
            </button>
          </li>
        ))}
      </ul>
    </>
  );
}

function DevicePanel({ d, topology, busy, p }: { d: DeviceEntity; topology: Topology; busy: boolean; p: InspectorProps }) {
  // Learned signals that apply to this tag, reported by the panel below and shown as a header badge.
  const [learned, setLearned] = useState<LearnedSignal[]>([]);
  const reg = d.registrations[0];
  const profile = reg ? deviceProfile(reg.profile_id) : undefined;
  const suggested = !profile ? suggestProfile({ model: d.model, kind: d.kind, hasBeacon: !!d.reading?.beacon, hasPIR: d.reading?.metrics?.motion != null }) : undefined;
  const shown = profile ?? suggested;
  const fresh = isFresh(d.reading?.received_at, topology.serverTime);
  const best = d.heard[0];
  const here = currentGateway(d, topology.serverTime);
  const gatewayName = (id: string) => topology.gateways.find((g) => g.gateway.id === id)?.gateway.name ?? id.slice(0, 8);
  const adoptable = topology.gateways.filter((g) => !d.registrations.some((r) => r.gateway_id === g.gateway.id));
  const r = d.reading;
  const m = r?.metrics ?? {};
  const kind = d.kind ?? (r ? "environment" : null);
  const time = r ? new Date(r.received_at).toLocaleTimeString("th-TH", { hour12: false }) : "";

  return (
    <>
      <header className="topo-inspector-head">
        <div>
          <span className="topo-kicker">DEVICE · {shown ? `${shown.brand} ${shown.model}` : d.model ? `Minew ${d.model}` : r ? `BLE · ${kind}` : "BLE"}</span>
          <h2>{d.name}</h2>
        </div>
        <span className={`topo-chip ${reg ? (fresh ? "health-receiving" : "health-ready") : "health-none"}`}>{reg ? (fresh ? "online" : r ? "รอข้อมูลใหม่" : "ลงทะเบียนแล้ว") : "ยังไม่ adopt"}</span>
      </header>

      {learned.length > 0 && (
        <p className="topo-learned" title={learned.map((s) => s.description).join("\n")}>
          <GraduationCap size={14} /> สอนสัญญาณไว้ {learned.length} รายการ · {[...new Set(learned.map((s) => s.event_type))].join(", ")}
        </p>
      )}

      {shown?.image && (profile || d.model) && (
        <div className="topo-product">
          <img src={shown.image} alt={`${shown.brand} ${shown.model}`} />
          <small>ภาพสินค้าจากผู้ผลิต ใช้เพื่อระบุรุ่น</small>
        </div>
      )}

      {d.events.length > 0 && (
        <ul className="topo-events" aria-label="เหตุการณ์ล่าสุด">
          {d.events.map((e) => (
            <li key={e.type} className={`topo-event is-${e.type}`}>
              {e.type === "tamper" ? <ShieldAlert size={15} /> : e.type === "button" ? <BellRing size={15} /> : e.type === "leak" ? <Droplets size={15} /> : e.type === "door" ? <DoorOpen size={15} /> : <Activity size={15} />}
              <span>
                <strong>{EVENT_LABEL[e.type] ?? e.type}</strong>
                <small>
                  {e.detail} · {new Date(e.at).toLocaleTimeString("th-TH", { hour12: false })}
                </small>
              </span>
            </li>
          ))}
        </ul>
      )}

      <dl className="topo-fields">
        <Field label="รหัสอุปกรณ์ (MAC)" value={formatMAC(d.external)} onNotice={p.onNotice} />
        {reg && <Field label="Profile" value={reg.profile_id} onNotice={p.onNotice} />}
        {d.model && (
          <div className="topo-field">
            <dt>ชื่อจากเฟรม info</dt>
            <dd>
              <span>{d.model}</span>
            </dd>
          </div>
        )}
        {r?.frames?.length ? (
          <div className="topo-field">
            <dt>เฟรมที่ถอดได้</dt>
            <dd>
              <span>{r.frames.join(", ")}</span>
            </dd>
          </div>
        ) : null}
        {d.simulated && (
          <div className="topo-field">
            <dt>แหล่งข้อมูล</dt>
            <dd>
              <span>SIM · ข้อมูลจำลองจาก virtual gateway</span>
            </dd>
          </div>
        )}
      </dl>

      {shown && (
        <p className={`topo-verify ${shown.verified ? "is-verified" : ""}`}>
          {shown.verified ? "decoder ของรุ่นนี้ยืนยันกับเครื่องจริงแล้ว" : `${profile ? "รุ่นนี้" : "รุ่นที่แนะนำ"} · ถอดรหัสจากเอกสารสาธารณะ ยังไม่ยืนยันกับเครื่องจริง`}
          {shown.notes ? ` · ${shown.notes}` : ""}
        </p>
      )}

      {r ? (
        <div className="topo-readings">
          {kind === "environment" && (
            <>
              <Tile icon={<Thermometer size={18} />} label="อุณหภูมิ" value={r.temperature.toFixed(2)} unit="°C" />
              {m.temperature_only !== 1 && <Tile icon={<Droplets size={18} />} label="ความชื้น" value={r.humidity.toFixed(2)} unit="%RH" />}
            </>
          )}
          {kind === "motion" && (
            <>
              <Tile icon={<Activity size={18} />} label={m.motion != null ? "PIR (ตรวจจับคน)" : "การเคลื่อนไหว"} value={m.motion != null ? (m.motion === 1 ? "พบคน" : "ไม่พบ") : m.vibration === 1 ? "เคลื่อนไหว" : "นิ่ง"} />
              {m.accel_g != null && <Tile icon={<Activity size={18} />} label="แรง (|a|)" value={m.accel_g.toFixed(2)} unit="g" />}
            </>
          )}
          {kind === "tamper" && <Tile icon={<ShieldAlert size={18} />} label="Tamper" value={m.tamper === 1 ? "ถูกถอด" : "ปกติ"} />}
          {(kind === "door" || m.door != null) && <Tile icon={<DoorOpen size={18} />} label="ประตู" value={m.door == null ? "—" : m.door === 1 ? "เปิดอยู่" : "ปิดอยู่"} />}
          {m.door_open_count != null && <Tile icon={<DoorOpen size={18} />} label="เปิด (ตัวนับของอุปกรณ์)" value={m.door_open_count} unit="ครั้ง" />}
          {kind === "leak" && <Tile icon={<Droplets size={18} />} label="น้ำรั่ว" value={m.leak === 1 ? "พบ" : "ไม่พบ"} />}
          {kind === "light" && <Tile icon={<Activity size={18} />} label="แสง" value={m.illuminance != null ? m.illuminance : m.light === 1 ? "มีแสง" : "มืด"} unit={m.illuminance != null ? "lx" : undefined} />}
          {r.beacon?.type === "ibeacon" && (
            <>
              <Tile icon={<RadioTower size={18} />} label="iBeacon major / minor" value={`${r.beacon.major} / ${r.beacon.minor}`} />
              <Tile icon={<RadioTower size={18} />} label="Tx power" value={r.beacon.tx_power ?? "—"} unit="dBm" />
            </>
          )}
          {r.beacon?.instance && <Tile icon={<RadioTower size={18} />} label="Eddystone instance" value={r.beacon.instance} />}
          {r.beacon?.voltage ? <Tile icon={<Battery size={18} />} label="แรงดัน (TLM)" value={r.beacon.voltage.toFixed(2)} unit="V" /> : null}
          {r.battery > 0 && <Tile icon={<Battery size={18} />} label="แบตเตอรี่" value={r.battery} unit="%" />}
          <Tile icon={<Wifi size={18} />} label="RSSI" value={r.rssi ?? "—"} unit="dBm" />
          {(m.accel_x != null || r.beacon?.uuid || r.beacon?.namespace) && (
            <p className="topo-raw">
              {m.accel_x != null ? `accel x ${m.accel_x.toFixed(3)} · y ${m.accel_y?.toFixed(3)} · z ${m.accel_z?.toFixed(3)} g` : ""}
              {r.beacon?.uuid ? `UUID ${r.beacon.uuid}` : ""}
              {r.beacon?.namespace ? `namespace ${r.beacon.namespace}` : ""}
            </p>
          )}
          <p className={fresh ? "is-fresh" : "is-stale"}>
            {fresh ? "รับข้อมูลล่าสุด" : "ข้อมูลเก่า / รอข้อมูลใหม่"} · {time}
          </p>
        </div>
      ) : (
        <p className="topo-note">ยังไม่มีเฟรมที่ Aether ถอดรหัสได้จากอุปกรณ์นี้ · เห็นเพียง BLE advertisement ดิบ</p>
      )}

      {d.log.length > 0 && (
        <>
          <h3 className="topo-h3">
            <BellRing size={14} /> เหตุการณ์ที่บันทึกไว้
          </h3>
          <ul className="topo-list topo-log">
            {d.log.map((ev) => (
              <li key={ev.id}>
                <span className={`topo-log-type is-${ev.event_type}`}>{EVENT_LABEL[ev.event_type] ?? ev.event_type}</span>
                <small>{new Date(ev.occurred_at).toLocaleString("th-TH", { hour12: false })}</small>
              </li>
            ))}
          </ul>
        </>
      )}

      <h3 className="topo-h3">
        <Radio size={14} /> ได้ยินโดย
      </h3>
      <ul className="topo-list">
        {d.heard.map((h) => (
          <li key={h.gatewayId}>
            <button type="button" className="topo-list-item" onClick={() => p.onSelectGateway(h.gatewayId)}>
              <Radio size={14} />
              <span>
                <strong>{gatewayName(h.gatewayId)}</strong>
                <small>{h.rssi != null ? `${h.rssi} dBm` : "raw advertisement"}</small>
              </span>
              <em className={d.registrations.some((r) => r.gateway_id === h.gatewayId) ? "is-adopted" : ""}>{d.registrations.some((r) => r.gateway_id === h.gatewayId) ? "adopted" : "heard"}</em>
            </button>
          </li>
        ))}
        {d.heard.length === 0 && <li className="topo-empty">ยังไม่มี gateway ที่ได้ยินอุปกรณ์นี้ในช่วงล่าสุด</li>}
      </ul>

      {d.registrations.length > 0 && (
        <>
          <h3 className="topo-h3">
            <Link2 size={14} /> ลงทะเบียนกับ
          </h3>
          <ul className="topo-list">
            {d.registrations.map((r) => (
              <li key={r.id} className="topo-reg">
                <button type="button" className="topo-list-item" onClick={() => p.onSelectGateway(r.gateway_id)}>
                  <Link2 size={14} />
                  <span>
                    <strong>{gatewayName(r.gateway_id)}</strong>
                    <small>
                      {r.profile_id} · {new Date(r.created_at).toLocaleDateString("th-TH")}
                    </small>
                  </span>
                </button>
                <button type="button" className="topo-icon-btn" disabled={busy} aria-label={`แก้ไขหรือย้าย ${r.name}`} title="เปลี่ยนชื่อ / ย้ายไป gateway อื่น" onClick={() => p.onEditRegistration(r)}>
                  <Pencil size={14} />
                </button>
                <button type="button" className="topo-icon-btn is-danger" disabled={busy} aria-label={`ยกเลิกการลงทะเบียน ${r.name}`} title="ยกเลิกการลงทะเบียน (กู้คืนได้)" onClick={() => p.onRemoveRegistration(r)}>
                  <Unlink size={14} />
                </button>
              </li>
            ))}
          </ul>
          <label className="topo-check">
            <input type="checkbox" checked={d.roaming} disabled={busy} onChange={(e) => p.onSetRoaming(d.registrations, e.target.checked)} />
            <span>
              <strong>ใช้ได้หลาย gateway (roaming)</strong>
              <small>
                {d.roaming
                  ? here
                    ? `ตอนนี้อยู่ที่ ${gatewayName(here.gatewayId)}${here.rssi != null ? ` (${here.rssi} dBm)` : ""} · ค่าและ widget ตามจาก gateway ที่สัญญาณแรงสุด · offline เมื่อไม่มี gateway ใดได้ยิน`
                    : "ตอนนี้ไม่มี gateway ใดได้ยิน · จะแจ้ง offline ครั้งเดียวเมื่อเงียบทุก gateway"
                  : "เปิดสำหรับ wearable หรืออุปกรณ์พกพา · ไม่ต้องลงทะเบียนซ้ำทุก gateway"}
              </small>
            </span>
          </label>
          <p className="topo-note">ย้ายได้ด้วยการลากปลายเส้นสีเขียวบน canvas ไปยัง gateway อื่น · การยกเลิกไม่ลบประวัติ และกู้คืนได้</p>
        </>
      )}

      <RemovedList items={p.removedDevices.filter((r) => r.external_id.toLowerCase() === d.external)} gatewayName={gatewayName} busy={busy} onRestore={p.onRestoreRegistration} />

      {/* Teaching what this tag broadcasts when it is triggered. It records through whichever gateway
          currently hears the device, since that is the one whose raw archive the diff reads. */}
      <SignalPanel
        external={d.external}
        gatewayId={here?.gatewayId ?? best?.gatewayId ?? reg?.gateway_id ?? null}
        profileIds={[...new Set(d.registrations.map((r) => r.profile_id))]}
        client={p.client}
        busy={busy}
        onNotice={p.onNotice}
        onApplies={setLearned}
      />

      <div className="topo-actions">
        {adoptable.length > 0 && (
          <button type="button" className="topo-btn primary" disabled={busy} onClick={() => p.onAdopt(d.external, d.registrations.length ? null : (best?.gatewayId ?? null))}>
            <Link2 size={15} /> {d.registrations.length ? "ลงทะเบียนกับ gateway อื่น" : "Adopt เข้า workspace"}
          </button>
        )}
        {best && (
          <button type="button" className="topo-btn" onClick={() => p.onOpenStudio(`${best.gatewayId}/${d.external}`)}>
            <ExternalLink size={15} /> เปิดใน Dashboard Studio
          </button>
        )}
      </div>
    </>
  );
}

export default function Inspector(p: InspectorProps) {
  const { selection, topology } = p;
  if (!selection) {
    return (
      <aside className="topo-inspector is-empty" aria-label="รายละเอียด">
        <button type="button" className="topo-icon-btn topo-close" aria-label="ซ่อนรายละเอียด" title="ซ่อนรายละเอียด" onClick={p.onHide}>
          <PanelRightClose size={16} />
        </button>
        <div>
          <h2>เลือกอุปกรณ์บน canvas</h2>
          <p>คลิก gateway หรืออุปกรณ์เพื่อดูสถานะ ค่าตั้งค่า และการเชื่อมต่อ</p>
          <ul className="topo-legend">
            <li>
              <span className="edge edge-receiving" /> ส่งข้อมูลอยู่
            </li>
            <li>
              <span className="edge edge-ready" /> ตั้งค่าแล้ว รออุปกรณ์
            </li>
            <li>
              <span className="edge edge-adopted" /> อุปกรณ์ที่ adopt แล้ว
            </li>
            <li>
              <span className="edge edge-heard" /> ได้ยินทางอากาศ (ยังไม่ adopt)
            </li>
            <li>
              <span className="edge edge-roam" /> wearable แบบ roaming · เส้นเข้ม = อยู่ที่ gateway นั้น
            </li>
          </ul>
          <p className="topo-note">โยงเส้นจากอุปกรณ์ไป gateway = ลงทะเบียนอุปกรณ์กับ gateway นั้นผ่าน API ทันที</p>
        </div>
      </aside>
    );
  }
  let body: React.ReactNode = null;
  if (selection.kind === "broker") body = <BrokerPanel settings={topology.broker.settings} topology={topology} />;
  if (selection.kind === "gateway") {
    const g = topology.gateways.find((x) => x.gateway.id === selection.id);
    body = g ? <GatewayPanel g={g} topology={topology} credentials={p.credentials[g.gateway.id]} httpToken={p.httpTokens[g.gateway.id]} busy={p.busy} p={p} /> : <p className="topo-note">gateway นี้ไม่อยู่ในรายการแล้ว</p>;
  }
  if (selection.kind === "device") {
    const d = topology.devices.find((x) => x.external === selection.external);
    body = d ? <DevicePanel d={d} topology={topology} busy={p.busy} p={p} /> : <p className="topo-note">อุปกรณ์นี้หายจากช่วงข้อมูลล่าสุด</p>;
  }
  if (selection.kind === "draft") {
    const profile = deviceProfile(selection.profile);
    body = (
      <>
        <header className="topo-inspector-head">
          <div>
            <span className="topo-kicker">อุปกรณ์ใหม่ · {profile?.brand}</span>
            <h2>{profile?.label ?? selection.profile}</h2>
          </div>
        </header>
        <p className="topo-note">{profile?.description}</p>
        <p className="topo-note">ลากเส้นจากอุปกรณ์นี้ไปยัง gateway ใดก็ได้ แล้วกรอกรหัสอุปกรณ์ (MAC หรือ external id) เพื่อลงทะเบียน</p>
        <div className="topo-actions">
          <button type="button" className="topo-btn primary" disabled={p.busy || topology.gateways.length === 0} onClick={() => p.onAdopt(null, null, selection.id)}>
            <Link2 size={15} /> เลือก gateway และลงทะเบียน
          </button>
          <button type="button" className="topo-btn" onClick={() => p.onRemoveDraft(selection.id)}>
            <Trash2 size={15} /> เอาออกจาก canvas
          </button>
        </div>
      </>
    );
  }
  return (
    <aside className="topo-inspector" aria-label="รายละเอียด">
      <div className="topo-close">
        <button type="button" className="topo-icon-btn" aria-label="ซ่อนรายละเอียด" title="ซ่อนรายละเอียด" onClick={p.onHide}>
          <PanelRightClose size={16} />
        </button>
        <button type="button" className="topo-icon-btn" aria-label="ปิด" title="ยกเลิกการเลือก" onClick={p.onClose}>
          <X size={16} />
        </button>
      </div>
      {body}
    </aside>
  );
}
