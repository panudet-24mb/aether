// Builds a unified topology model from the raw API snapshot.
// One physical device (keyed by its external id / BLE MAC) may be heard by several gateways
// and may hold one registration per gateway; the graph shows each as its own link.

import type { Device, Gateway, MQTTState, Reading, Snapshot } from "./api";
import { gatewayModel } from "./catalog";

export const FRESH_MS = 60_000;

export type GatewayHealth = "none" | "pending" | "ready" | "stale" | "receiving";

export type GatewayEntity = {
  gateway: Gateway;
  state: MQTTState | undefined;
  health: GatewayHealth;
  lastPacketAt: string | null;
  packetCount: number;
  observationCount: number;
  nearbyDevices: number;
  decodedSensors: number;
};

export type HeardLink = { gatewayId: string; rssi: number | null; receivedAt: string | null; decoded: boolean };

export type DeviceEvent = { type: "tamper" | "motion" | "leak" | "button" | "light" | "door" | "hazard"; at: string; detail: string };

export type DeviceEntity = {
  external: string;
  name: string;
  simulated: boolean;
  /** Reading kind of the latest decoded frame set (environment, motion, beacon, tamper, …). */
  kind: string | null;
  /** Device name from the Minew info frame, e.g. "S1", "C10", when the tag has sent one. */
  model: string | null;
  /** Events derived from the latest reading and the recent history of the strongest gateway. */
  events: DeviceEvent[];
  /** Persisted event log entries for this device (newest first, from GET /events). */
  log: Snapshot["events"];
  /** Gateways that currently hear this device over the air. */
  heard: HeardLink[];
  /** Latest decoded reading, if Aether recognises the frame. */
  reading: Reading | null;
  templateId: string | null;
  /** Registered device records (adopted), possibly one per gateway. */
  registrations: Device[];
  /** Wearable followed across gateways (any registration has roaming on). */
  roaming: boolean;
  /** Zigbee2MQTT devices report their own availability: true/false once reported, null for BLE (silence-based). */
  reportedOffline: boolean | null;
};

export type Topology = {
  broker: { configured: boolean; settings: Snapshot["settings"] };
  gateways: GatewayEntity[];
  devices: DeviceEntity[];
  serverTime: number;
};

/** Where a tag is now: the gateway with the strongest signal among those that heard it recently. RSSI is a proximity hint, not a position. */
export function currentGateway(d: DeviceEntity, serverTime: number): HeardLink | null {
  // Prefer the server's stable zone so the canvas does not flap with raw RSSI; fall back to the strongest fresh signal.
  const zone = d.registrations.find((r) => r.zone_gateway_id)?.zone_gateway_id;
  const stable = zone ? d.heard.find((h) => h.gatewayId === zone && isFresh(h.receivedAt, serverTime)) : undefined;
  return stable ?? d.heard.find((h) => isFresh(h.receivedAt, serverTime)) ?? null; // heard is sorted by RSSI; same rule as the presence API
}

export function isFresh(at: string | null | undefined, serverTime: number): boolean {
  return !!at && serverTime - Date.parse(at) <= FRESH_MS;
}

/** Packets are the ground truth: a gateway provisioned outside this page still counts as receiving. */
export function gatewayHealth(state: MQTTState | undefined, serverTime: number, model?: string): GatewayHealth {
  if (isFresh(state?.last_packet_at, serverTime)) return "receiving";
  if (state?.last_packet_at) return "stale";
  // HTTP gateways never get an MQTT account; once created they are simply waiting for their first POST.
  if (gatewayModel(model ?? "")?.transport === "http") return "ready";
  if (!state || !state.revision) return "none";
  return state.applied_revision === state.revision ? "ready" : "pending";
}

/** `serverTime` defaults to the snapshot clock; callers pass an advanced clock so freshness decays between polls. */
export function buildTopology(s: Snapshot, serverTime = s.serverTime): Topology {
  const liveByGateway = new Map(s.live?.gateways.map((g) => [g.gateway.id, g]) ?? []);
  const stateByGateway = new Map(s.states.map((st) => [st.gateway_id, st]));

  const gateways: GatewayEntity[] = s.gateways.map((gateway) => {
    const state = stateByGateway.get(gateway.id);
    const live = liveByGateway.get(gateway.id);
    return {
      gateway,
      state,
      health: gatewayHealth(state, serverTime, gateway.model),
      lastPacketAt: state?.last_packet_at ?? live?.last_packet_at ?? null,
      packetCount: live?.packet_count ?? 0,
      observationCount: live?.observation_count ?? 0,
      nearbyDevices: live?.nearby_devices ?? 0,
      decodedSensors: live?.sensors.length ?? 0,
    };
  });

  const devices = new Map<string, DeviceEntity>();
  const entity = (external: string): DeviceEntity => {
    const key = external.toLowerCase();
    let d = devices.get(key);
    if (!d) {
      d = { external: key, name: "", simulated: false, kind: null, model: null, events: [], log: [], heard: [], reading: null, templateId: null, registrations: [], roaming: false, reportedOffline: null };
      devices.set(key, d);
    }
    return d;
  };

  // Raw BLE observations: every MAC a gateway has seen in its recent packets.
  for (const src of s.sources) {
    const d = entity(src.id);
    if (src.name.startsWith("SIM")) d.simulated = true;
    if (!d.heard.some((h) => h.gatewayId === src.gatewayID)) d.heard.push({ gatewayId: src.gatewayID, rssi: null, receivedAt: null, decoded: false });
  }

  // Decoded sensors carry RSSI, freshness, a display name and a template assignment.
  for (const g of s.live?.gateways ?? []) {
    for (const sensor of g.sensors) {
      const d = entity(sensor.id);
      if (sensor.latest.source === "simulated" || sensor.name.startsWith("SIM")) d.simulated = true;
      if (!d.name && sensor.name && sensor.name.toLowerCase() !== d.external) d.name = sensor.name;
      if (!d.reading || Date.parse(sensor.latest.received_at) > Date.parse(d.reading.received_at)) {
        d.reading = sensor.latest;
        d.kind = sensor.kind ?? sensor.latest.kind ?? "environment";
        d.events = deriveEvents(sensor.latest, sensor.history);
      }
      if (sensor.model ?? sensor.latest.model) d.model = sensor.model ?? sensor.latest.model ?? null;
      if (sensor.template_id) d.templateId = sensor.template_id;
      if (sensor.liveness === "reported") d.reportedOffline = !!sensor.offline;
      const link = d.heard.find((h) => h.gatewayId === g.gateway.id);
      const heard: HeardLink = { gatewayId: g.gateway.id, rssi: sensor.latest.rssi, receivedAt: sensor.latest.received_at, decoded: true };
      if (link) Object.assign(link, heard);
      else d.heard.push(heard);
    }
  }

  // Include undecoded discoveries even when they are absent from the live sensor list.
  for (const item of s.discovery ?? []) {
    const d = entity(item.external_id);
    if (item.model) d.model = item.model;
    if (item.kind) d.kind = item.kind;
    if (item.source === "simulated") d.simulated = true;
    if (!d.heard.some((h) => h.gatewayId === item.gateway_id)) d.heard.push({ gatewayId: item.gateway_id, rssi: item.rssi, receivedAt: item.last_seen, decoded: !!item.kind });
  }

  // Registered devices are the adopted links; a registered name wins over the stream name.
  for (const reg of s.devices) {
    const d = entity(reg.external_id);
    d.registrations.push(reg);
    d.name = reg.name;
  }

  for (const ev of s.events) {
    const d = devices.get(ev.external_id.toLowerCase());
    if (d && d.log.length < 6) d.log.push(ev);
  }

  for (const d of devices.values()) {
    if (!d.name) d.name = d.simulated ? `SIM ${d.external.slice(-4).toUpperCase()}` : d.external.slice(-6).toUpperCase();
    d.heard.sort((a, b) => (b.rssi ?? -999) - (a.rssi ?? -999));
    d.roaming = d.registrations.some((r) => r.roaming);
  }

  return {
    broker: { configured: s.settings !== null, settings: s.settings },
    gateways,
    devices: [...devices.values()],
    serverTime,
  };
}

/** Turns flag metrics and identity changes into events the UI can surface without inventing hardware semantics. */
export function deriveEvents(latest: Reading, history: Reading[]): DeviceEvent[] {
  const out: DeviceEvent[] = [];
  const m = latest.metrics ?? {};
  if (m.tamper === 1) out.push({ type: "tamper", at: latest.received_at, detail: "tamper flag = 1 (ป้ายถูกถอด/แกะ)" });
  if (m.leak === 1) out.push({ type: "leak", at: latest.received_at, detail: "leak flag = 1" });
  if (m.vibration === 1 || m.motion === 1) out.push({ type: "motion", at: latest.received_at, detail: m.vibration === 1 ? "vibration flag = 1" : "PIR motion = 1" });
  if (m.door === 1) out.push({ type: "door", at: latest.received_at, detail: "door = 1 (ประตูเปิดอยู่)" });
  for (const h of ["smoke", "gas", "carbon_monoxide"]) if (m[h] === 1) out.push({ type: "hazard", at: latest.received_at, detail: `${h} = 1 (Zigbee2MQTT)` });
  if (m.light === 1) out.push({ type: "light", at: latest.received_at, detail: "light detected" });
  // Eddystone-UID instance changes are how the B10 wristband signals a press (per public config guide, unverified).
  const instance = latest.beacon?.instance;
  if (instance) {
    const previous = [...history].reverse().find((h) => h.beacon?.instance && h.beacon.instance !== instance);
    const sameBefore = history.filter((h) => h.beacon?.instance === instance).length;
    if (previous && sameBefore <= 2) out.push({ type: "button", at: latest.received_at, detail: `Eddystone instance เปลี่ยนเป็น ${instance} (จาก ${previous.beacon!.instance})` });
  }
  return out;
}

/** The gateway a device should be drawn under: its registration first, otherwise the strongest signal. */
export function primaryGateway(d: DeviceEntity): string | null {
  return d.registrations[0]?.gateway_id ?? d.heard[0]?.gatewayId ?? null;
}

/** Devices that earn a place on the canvas automatically: adopted or recognised sensors. */
/**
 * Whether a device belongs on the canvas: registered, or a Minew device (it reported a model in its FFE1
 * info frame, kept in the stream name "Minew C10", or decoded any FFE1 kind), or already listed by
 * /discovery. A bare iBeacon/Eddystone is not enough: phones and other people's beacons send those too.
 */
export function isRecognised(d: DeviceEntity, discovered?: ReadonlySet<string>): boolean {
  if (d.registrations.length > 0 || discovered?.has(d.external)) return true;
  const name = d.name.replace(/^SIM · ข้อมูลจำลอง · /, "");
  if (d.model || name.startsWith("Minew ")) return true;
  return !!d.kind && d.kind !== "beacon";
}

/** One-line status for a node label; mirrors what the decoder actually produced. */
export function summarize(d: DeviceEntity): string | null {
  const r = d.reading;
  if (!r) return null;
  const m = r.metrics ?? {};
  const kind = d.kind ?? "environment";
  if (d.events.some((e) => e.type === "tamper")) return "TAMPER · ป้ายถูกถอด";
  if (d.events.some((e) => e.type === "button")) return "กดปุ่มฉุกเฉิน SOS";
  if (["smoke", "gas", "carbon_monoxide"].some((h) => m[h] === 1)) return "ตรวจพบควัน / แก๊ส / CO";
  if (d.events.some((e) => e.type === "leak")) return "พบน้ำรั่ว";
  switch (kind) {
    case "environment":
      if (!Number.isFinite(r.temperature)) return null;
      return Number.isFinite(r.humidity) ? `${r.temperature.toFixed(1)}°C · ${r.humidity.toFixed(0)}%` : `${r.temperature.toFixed(1)}°C`;
    case "occupancy":
      return m.motion == null ? "PIR · รอสถานะ" : m.motion === 1 ? "PIR · พบคน" : "PIR · ไม่พบคน";
    case "hazard":
      return "ควัน / แก๊ส · ปกติ";
    case "sos":
      return "ปุ่มฉุกเฉิน · พร้อม";
    case "remote":
      return r.action ? `กด ${r.action}` : "รีโมต · พร้อม";
    case "lighting":
      return m.state == null ? "หลอดไฟ" : m.state === 1 ? `เปิด${m.brightness != null ? ` · ${Math.round((m.brightness / 254) * 100)}%` : ""}` : "ปิด";
    case "cover":
      return m.position != null ? `เปิด ${m.position}%` : "ม่าน";
    case "lock":
      return r.values?.lock_state === "locked" || m.state === 1 ? "ล็อกอยู่" : r.values?.lock_state || m.state === 0 ? "ปลดล็อก" : "กลอน";
    case "climate":
      return m.local_temperature != null ? `${m.local_temperature.toFixed(1)}°C${m.occupied_heating_setpoint != null ? ` → ${m.occupied_heating_setpoint}°C` : ""}` : "ควบคุมอุณหภูมิ";
    case "metering":
      return m.power != null ? `${m.power} W` : "มิเตอร์"; 
    case "motion":
      if (m.motion != null && m.vibration == null && m.accel_g == null) return m.motion === 1 ? "PIR · พบคน" : "PIR · ไม่พบคน";
      return (d.events.some((e) => e.type === "motion") ? "เคลื่อนไหว" : "นิ่ง") + (m.accel_g != null ? ` · ${m.accel_g.toFixed(2)} g` : "");
    case "tamper":
      return "ปกติ · ป้ายติดอยู่";
    case "door":
      return m.door == null ? "ประตู · รอสถานะ" : m.door === 1 ? "ประตูเปิดอยู่" : "ประตูปิดอยู่";
    case "leak":
      return "ปกติ · ไม่พบน้ำ";
    case "light":
      return m.illuminance != null ? `${m.illuminance} lx` : m.light === 1 ? "มีแสง" : "มืด";
    case "beacon":
      if (r.beacon?.type === "ibeacon") return `iBeacon ${r.beacon.major}/${r.beacon.minor}`;
      if (r.beacon?.instance) return `UID …${r.beacon.instance.slice(-6)}`;
      return "beacon";
    default:
      if (r.frames?.includes("z2m-state@1")) return r.model || null;
      return r.model ? `Minew ${r.model}` : null;
  }
}

/** Events that deserve attention on the canvas (not routine motion). */
export function isAlerting(d: DeviceEntity): boolean {
  return d.events.some((e) => e.type === "tamper" || e.type === "button" || e.type === "leak" || e.type === "hazard");
}

/** "all" = every project, "none" = gateways without a project, otherwise a project id. */
export type ProjectScope = "all" | "none" | string;

/** Narrows a snapshot to one project: its gateways and only the devices those gateways hear or hold. */
export function scopeSnapshot(s: Snapshot, scope: ProjectScope): Snapshot {
  if (scope === "all") return s;
  const gateways = s.gateways.filter((g) => (scope === "none" ? !g.project_id : g.project_id === scope));
  const ids = new Set(gateways.map((g) => g.id));
  return {
    ...s,
    gateways,
    states: s.states.filter((x) => ids.has(x.gateway_id)),
    discovery: s.discovery?.filter((x) => ids.has(x.gateway_id)),
    sources: s.sources.filter((x) => ids.has(x.gatewayID)),
    devices: s.devices.filter((x) => ids.has(x.gateway_id)),
    removedDevices: s.removedDevices.filter((x) => ids.has(x.gateway_id)),
    events: s.events.filter((x) => ids.has(x.gateway_id)),
    live: s.live ? { ...s.live, gateways: s.live.gateways.filter((g) => ids.has(g.gateway.id)) } : null,
  };
}
