// Thin authenticated client for the Aether backend used by the topology page.
// Access tokens stay in memory (provided by the caller); a 401 triggers one refresh + retry.

import { applyCatalog, type CatalogResponse, type DeviceProfile } from "./catalog";
import { normalizeSensor } from "../live/measurements";

const API = import.meta.env.VITE_AETHER_API_ORIGIN ?? "";

export class ApiError extends Error {
  status: number;
  /** Machine-readable refusal the server gives for some requests (e.g. a command: "offline", "in_flight"). */
  reason?: string;
  constructor(status: number, message: string, reason?: string) {
    super(message);
    this.status = status;
    this.reason = reason;
  }
}

/** One settable feature of a Zigbee2MQTT device, straight from its definition's exposes (zigbee2mqtt.io/guide/usage/exposes). */
export type ControlFeature = {
  type: "binary" | "numeric" | "enum" | "composite";
  name?: string;
  label?: string;
  property: string;
  endpoint?: string;
  access: number;
  unit?: string;
  value_on?: unknown;
  value_off?: unknown;
  value_toggle?: unknown;
  value_min?: number;
  value_max?: number;
  value_step?: number;
  values?: unknown[];
  features?: ControlFeature[];
  /** The specific type the feature sits in (light, cover, lock, climate, switch, fan). */
  group?: string;
};
export type DeviceControls = { device_id: string; gateway_id: string; ieee: string; online: boolean; can_command: boolean; state: Record<string, unknown>; features: ControlFeature[] };
export type CommandStatus = "pending" | "sent" | "confirmed" | "timeout" | "expired" | "failed";
export type Command = { id: string; device_id: string; property: string; requested: "set" | "toggle"; value: unknown; status: CommandStatus; error?: string; created_at: string; settled_at?: string };

export type Gateway = { id: string; name: string; model: string; created_at: string; project_id?: string | null };
export type Project = { id: string; name: string; description: string; color: string; gateway_count: number; created_at: string };
export type MQTTState = { gateway_id: string; revision: number; applied_revision: number; applied_at: string | null; last_packet_at: string | null };
export type Source = { key: string; id: string; name: string; gatewayID: string; gatewayName: string };
export type Device = { id: string; gateway_id: string; name: string; external_id: string; profile_id: string; created_at: string; removed_at?: string | null; /** Wearables: followed across every gateway of the workspace. */ roaming?: boolean; /** Stable zone decided by the server (smoothed RSSI with hysteresis). */ zone_gateway_id?: string | null };
export type Beacon = { type: "ibeacon" | "eddystone_uid" | "eddystone_tlm"; uuid?: string; major?: number; minor?: number; namespace?: string; instance?: string; tx_power?: number; voltage?: number; adv_count?: number; uptime_s?: number };
export type Reading = {
  source?: string;
  received_at: string;
  /** environment | motion | tamper | leak | light | beacon | info | switch; missing on samples stored before multi-frame decoding (= environment). */
  kind?: string;
  model?: string;
  frames?: string[];
  temperature: number;
  humidity: number;
  battery: number;
  rssi: number | null;
  metrics?: Record<string, number>;
  /** Non-numeric states a Zigbee2MQTT device reports (lock_state, system_mode, a cover's OPEN/CLOSE/STOP). */
  values?: Record<string, string>;
  /** A Zigbee2MQTT button or remote event in this message (single, on, emergency, ...). */
  action?: string;
  beacon?: Beacon;
};
export type LiveSensor = { id: string; name: string; kind?: string; model?: string; template_id?: string | null; latest: Reading; history: Reading[]; /** "reported": online/offline come from the device's availability reports (Zigbee2MQTT), not from silence. */ liveness?: "reported"; offline?: boolean };
export type LiveGateway = { gateway: Gateway; last_packet_at: string | null; packet_count: number; observation_count: number; nearby_devices: number; sensors: LiveSensor[] };
export type Live = { gateways: LiveGateway[]; server_time: string; deployment_mode: string };
export type MQTTSettings = { host: string; port: number; scheme: string; tls: boolean; qos: number; keep_alive: number; /** Present only when the server opted in to an unencrypted listener for gateways without TLS. */ plaintext?: { port: number; scheme: string } };
export type MQTTCredentials = MQTTSettings & {
  gateway_id: string;
  username: string;
  password: string;
  client_id: string;
  /** Minew-style gateways: the three fixed topics. */
  post_topic?: string;
  subscribe_topic?: string;
  reply_topic?: string;
  /** Zigbee2MQTT gateways: the base topic, the server URL and a ready-made configuration.yaml mqtt section. */
  base_topic?: string;
  server?: string;
  z2m_yaml?: string;
};
export type GatewayCreated = { gateway: Gateway; token: string; capture_path: string };
export type Me = { user_id: string; tenant_id: string; role: string; deployment_mode: string };

export type DeviceEventRow = { id: string; gateway_id: string; external_id: string; device_name: string; event_type: string; detail: Record<string, unknown>; occurred_at: string };
export type Discovery = { gateway_id: string; external_id: string; last_seen: string; source: string; model?: string; kind?: string; rssi: number | null; profile?: DeviceProfile; /** Zigbee2MQTT definition's vendor and description (for a Tuya device: "Tuya" and its name). */ vendor?: string; description?: string; /** Tuya devices under an Aether Edge (source "tuya" imported, "tuya_lan" seen on the LAN without a key) or a Tuya Cloud link (source "tuya_cloud"). */ key_status?: TuyaKeyStatus; local_capable?: boolean; ip?: string; protocol_version?: string };
/** One model of the Zigbee2MQTT device catalog (zigbee-herdsman-converters). */
export type ZigbeeModel = { vendor: string; model: string; description: string; zigbee_model?: string[]; white_label?: string[]; category: string; features?: string[]; sos?: boolean; dynamic?: boolean };
export type ZigbeeCatalog = { items: ZigbeeModel[]; total: number; source: string; version: string; license: string; homepage: string; notice: string; vendors?: string[] };
/** Tuya data centres the one-time key import may use (backend tuyacloud.Regions); most existing Thai Smart Life accounts are on "us". */
export const TUYA_REGIONS: { id: string; label: string }[] = [
  { id: "us", label: "Western America" },
  { id: "sg", label: "Singapore" },
  { id: "eu", label: "Central Europe" },
  { id: "in", label: "India" },
  { id: "us-e", label: "Eastern America" },
  { id: "eu-w", label: "Western Europe" },
  { id: "cn", label: "China" },
];
/** One device of a Tuya import result: its key fingerprint, never the key. */
export type TuyaImportedDevice = { tuya_id: string; name: string; tuya_category: string; product_id: string; sub: boolean; local_capable: boolean; has_key: boolean; key_fingerprint?: string; spec: string };
/** An import job as the page polls it (backend app.TuyaImportJob). */
export type TuyaImportJob = {
  id: string;
  gateway_id: string;
  region: string;
  status: "running" | "done" | "failed";
  stage: "token" | "devices" | "models" | "saving" | "done";
  error?: string;
  tuya_code?: number;
  hint?: string;
  suggest_regions?: string[];
  found: number;
  imported: number;
  with_key: number;
  local_capable: number;
  skipped: number;
  devices: TuyaImportedDevice[];
  started_at: string;
  finished_at?: string;
  expires_at: string;
};
export type TuyaKeyStatus = "ok" | "rejected" | "suspect" | "missing";
/** An imported Tuya device of an Aether Edge (backend domain.TuyaDevice): never its key. */
export type TuyaDevice = {
  tuya_id: string;
  name: string;
  tuya_category: string;
  product_id: string;
  category: string;
  sub: boolean;
  /** The hub of a sub-device, when Tuya Cloud reported it ("" otherwise). */
  parent_tuya_id?: string;
  local_capable: boolean;
  key_fingerprint: string;
  key_status: TuyaKeyStatus;
  version: string;
  ip: string;
  available: boolean | null;
  /** Why it is offline: unreachable, auth_failed, key_suspect, busy, not_found (Aether Edge) or cloud_link_down (Tuya Cloud). */
  reason: string;
  registered: boolean;
  lan_seen: boolean;
  imported_at: string;
};
/** What the gateway page shows about an Aether Edge (backend domain.EdgeStatus): nothing secret. */
export type EdgeStatus = {
  gateway_id: string;
  state: "" | "online" | "offline";
  state_at: string | null;
  version: string;
  latest_version: string;
  last_health_at: string | null;
  devices_connected: number;
  lan_seen: number;
  lan_devices: number;
  config_revision: number;
  config_fetched_at: string | null;
  imported: number;
  registered: number;
  keys: Record<TuyaKeyStatus, number>;
};
/** Link state of a Tuya Cloud gateway (backend domain.TuyaCloudLink state). */
export type TuyaCloudState = "" | "linking" | "online" | "offline" | "auth_failed" | "not_subscribed" | "quota" | "disabled";
/** What the gateway page shows about a Tuya Cloud link (backend domain.TuyaCloudLink): never the secret, never the Access ID beyond its first 4 characters. */
export type TuyaCloudLink = {
  gateway_id: string;
  linked: boolean;
  region: string;
  channel: string;
  access_id_hint: string;
  state: TuyaCloudState;
  state_at: string | null;
  reason: string;
  last_error_code: number;
  last_event_at: string | null;
  last_health_at: string | null;
  usage_month: string | null;
  events_month: number;
  api_calls_month: number;
  dropped_month: number;
  sync_requested_at: string | null;
  linked_at: string | null;
  rotated_at: string | null;
  devices: number;
  registered: number;
  /** This deployment's TUYA_CLOUD: when false, linking and syncing are off but the status and unlink stay. */
  enabled: boolean;
  /** Monthly allowances usage is measured against (0 = no guard configured). */
  events_budget: number;
  api_calls_budget: number;
};
/** Message Service channels: event (production) and event-test (Tuya's test channel). */
export type TuyaCloudChannel = "event" | "event-test";
/** A single-use install code for the Aether Edge installer, shown once. */
export type EdgeInstallCode = { code: string; expires_at: string; install_command: string; install_url: string; bootstrap_url: string; zigbee_paired: boolean; image: string };

export type Snapshot = {
  discovery?: Discovery[];
  /** Per gateway: advertisements that are not a supported model (phones, foreign beacons), left out of `discovery`. */
  discoveryHidden?: Record<string, number>;
  projects: Project[];
  /** Withdrawn registrations (owner/admin only); kept so they can be restored. */
  removedDevices: Device[];
  events: DeviceEventRow[];
  gateways: Gateway[];
  states: MQTTState[];
  sources: Source[];
  devices: Device[];
  live: Live | null;
  settings: MQTTSettings | null;
  serverTime: number;
  /** Per-request failures that did not block the snapshot (e.g. MQTT setup unavailable). */
  warnings: string[];
};

export type Client = ReturnType<typeof createClient>;

/** Builds a client whose token/refresh callbacks are read from a ref at call time, so the client stays stable across renders. */
export function createClientFrom(handlers: { readonly current: { getToken: () => string; refresh: () => Promise<boolean> } }) {
  return createClient(
    () => handlers.current.getToken(),
    () => handlers.current.refresh(),
  );
}

type Settled<T> = { status: "fulfilled"; value: T } | { status: "rejected"; reason: unknown };

export function createClient(getToken: () => string, refresh: () => Promise<boolean>) {
  let slow: { at: number; gateways: Settled<Gateway[]>; sources: Settled<Source[]>; devices: Settled<Device[]>; settings: Settled<MQTTSettings>; catalog: Settled<CatalogResponse>; removed: Settled<Device[]>; projects: Settled<Project[]> } | null = null;
  async function call<T>(path: string, body?: unknown, extraHeaders?: Record<string, string>): Promise<T> {
    const send = () =>
      fetch(`${API}/api/v1${path}`, {
        method: body === undefined ? "GET" : "POST",
        headers: { Authorization: `Bearer ${getToken()}`, "Content-Type": "application/json", ...extraHeaders },
        body: body === undefined ? undefined : JSON.stringify(body),
        cache: "no-store",
        signal: AbortSignal.timeout(15000),
      });
    let r = await send();
    if (r.status === 401 && (await refresh())) r = await send();
    if (!r.ok) {
      // Some endpoints explain the failure (e.g. a channel test); prefer that over the generic status text.
      let detail = "";
      let reason: string | undefined;
      try {
        const body = (await r.json()) as { detail?: unknown; error?: unknown };
        if (typeof body.detail === "string") detail = body.detail;
        if (typeof body.error === "string") reason = body.error;
      } catch {
        // no JSON body
      }
      throw new ApiError(r.status, detail ? `${describe(r.status)} · ${detail}` : describe(r.status), reason);
    }
    if (r.status === 204) return undefined as T;
    return (await r.json()) as T;
  }

  return {
    /** Generic helpers for pages that share this authenticated client. */
    raw: <T,>(path: string) => call<T>(path),
    post: <T,>(path: string, body: unknown) => call<T>(path, body),
    /** Authenticated binary GET (e.g. a floor's scanned plan); returns null when the resource does not exist. */
    blob: async (path: string): Promise<Blob | null> => {
      const send = () => fetch(`${API}/api/v1${path}`, { headers: { Authorization: `Bearer ${getToken()}` }, cache: "no-store", signal: AbortSignal.timeout(15000) });
      let r = await send();
      if (r.status === 401 && (await refresh())) r = await send();
      if (r.status === 404) return null;
      if (!r.ok) throw new ApiError(r.status, describe(r.status));
      return r.blob();
    },
    /** Authenticated download of an export: GET, or POST when a body is given. Saved under `name`. Exports can be
     *  large (a tag's whole history), so the wait is longer than for ordinary calls. */
    download: async (path: string, name: string, body?: unknown): Promise<void> => {
      const send = () =>
        fetch(`${API}/api/v1${path}`, {
          method: body === undefined ? "GET" : "POST",
          headers: { Authorization: `Bearer ${getToken()}`, ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
          body: body === undefined ? undefined : JSON.stringify(body),
          cache: "no-store",
          signal: AbortSignal.timeout(90000),
        });
      let r = await send();
      if (r.status === 401 && (await refresh())) r = await send();
      if (!r.ok) {
        let reason: string | undefined;
        try {
          const b = (await r.json()) as { error?: unknown };
          if (typeof b.error === "string") reason = b.error;
        } catch {
          // no JSON body
        }
        throw new ApiError(r.status, describe(r.status), reason);
      }
      const url = URL.createObjectURL(await r.blob());
      const a = document.createElement("a");
      a.href = url;
      a.download = name;
      document.body.appendChild(a); // Firefox ignores click() on a detached anchor
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000); // revoking synchronously can cancel the download
    },
    me: () => call<Me>("/me"),
    catalog: () => call<CatalogResponse>("/catalog"),
    zigbeeCatalog: (q: string, opts: { vendor?: string; category?: string; limit?: number; vendors?: boolean } = {}) => {
      const params = new URLSearchParams({ q, limit: String(opts.limit ?? 50) });
      if (opts.vendor) params.set("vendor", opts.vendor);
      if (opts.category) params.set("category", opts.category);
      if (opts.vendors) params.set("vendors", "1");
      return call<ZigbeeCatalog>(`/catalog/zigbee?${params}`);
    },
    gateways: async () => (await call<{ items: Gateway[] }>("/gateways")).items,
    mqttStatus: () => call<{ items: MQTTState[]; server_time: string }>("/gateways/mqtt-status"),
    mqttSetup: () => call<MQTTSettings>("/mqtt/setup"),
    sources: async () => (await call<{ items: Source[] }>("/studio/sources")).items,
    devices: async () => (await call<{ items: Device[] }>("/devices")).items,
    live: () => call<Live>("/live?range=1h").then((l) => (l ? { ...l, gateways: l.gateways.map((g) => ({ ...g, sensors: g.sensors.map(normalizeSensor) })) } : l)),
    createGateway: (name: string, model: string, projectId?: string | null) => call<GatewayCreated>("/gateways", projectId ? { name, model, project_id: projectId } : { name, model }),
    projects: async () => (await call<{ items: Project[] }>("/projects")).items,
    createProject: (input: { name: string; description: string; color: string }) => call<Project>("/projects", input),
    updateProject: (id: string, input: { name: string; description: string; color: string }) => call<Project>(`/projects/${id}/update`, input),
    archiveProject: (id: string) => call<void>(`/projects/${id}/archive`, {}),
    setGatewayProject: (gatewayId: string, projectId: string | null) => call<void>(`/gateways/${gatewayId}/project`, { project_id: projectId }),
    revokeGateway: (id: string) => call<void>(`/gateways/${id}/revoke`, {}),
    issueMQTT: (id: string, rotate: boolean) => call<MQTTCredentials>(`/gateways/${id}/mqtt${rotate ? "/rotate" : ""}`, {}),
    setDeviceRoaming: (id: string, roaming: boolean) => call<void>(`/devices/${id}/roaming`, { roaming }),
    createDevice: (input: { gateway_id: string; name: string; external_id: string; profile_id: string }) => call<Device>("/devices", input),
    removedDevices: async () => (await call<{ items: Device[] }>("/devices?removed=true")).items,
    updateDevice: (id: string, change: { name?: string; gateway_id?: string }) => call<Device>(`/devices/${id}/update`, change),
    removeDevice: (id: string) => call<void>(`/devices/${id}/remove`, {}),
    restoreDevice: (id: string) => call<void>(`/devices/${id}/restore`, {}),
    /** What a Zigbee2MQTT device lets Aether set, its last values, and whether this member may command it. */
    deviceControls: (id: string) => call<DeviceControls>(`/devices/${id}/controls`),
    /**
     * Queues a command (202). `key` is a fresh UUID per click: a retry with the same key returns the same command.
     * The result is `pending`; only the device's own report makes it `confirmed`, so callers must poll `command`.
     */
    sendCommand: (key: string, input: { device_id: string; property: string; value?: unknown; action?: "set" | "toggle" }) =>
      call<Command>("/commands", input, { "Idempotency-Key": key }),
    command: (id: string) => call<Command>(`/commands/${id}`),
    /** Aether Edge: the agent's state and health with import and key counts. */
    edgeStatus: (gatewayId: string) => call<EdgeStatus>(`/gateways/${gatewayId}/edge/status`),
    /** A single-use install code (30 minutes); optionally pairs a Zigbee2MQTT gateway installed on the same host. */
    edgeInstallCode: (gatewayId: string, zigbeeGatewayId?: string) => call<EdgeInstallCode>(`/gateways/${gatewayId}/edge/install-code`, zigbeeGatewayId ? { zigbee_gateway_id: zigbeeGatewayId } : {}),
    /** Starts the one-time Tuya key import (202). The credentials are sent once and never kept by the page or the server. */
    startTuyaImport: (gatewayId: string, input: { region: string; access_id: string; access_secret: string }) => call<TuyaImportJob>(`/gateways/${gatewayId}/tuya/imports`, input),
    tuyaImport: (gatewayId: string, jobId: string) => call<TuyaImportJob>(`/gateways/${gatewayId}/tuya/imports/${jobId}`),
    tuyaDevices: async (gatewayId: string) => (await call<{ items: TuyaDevice[] }>(`/gateways/${gatewayId}/tuya/devices`)).items,
    forgetTuyaKey: (gatewayId: string, tuyaId: string) => call<void>(`/gateways/${gatewayId}/tuya/devices/${encodeURIComponent(tuyaId)}/forget`, {}),
    /** Tuya Cloud: link status (any member who can see the gateway). */
    tuyaCloudStatus: (gatewayId: string) => call<TuyaCloudLink>(`/gateways/${gatewayId}/tuya-cloud`),
    /** Links (or rotates) the project; proven with Tuya first, sealed for the worker, never returned. Answers the new status. */
    linkTuyaCloud: (gatewayId: string, input: { region: string; channel: TuyaCloudChannel; access_id: string; access_secret: string }) => call<TuyaCloudLink>(`/gateways/${gatewayId}/tuya-cloud/link`, input),
    syncTuyaCloud: (gatewayId: string) => call<TuyaCloudLink>(`/gateways/${gatewayId}/tuya-cloud/sync`, {}),
    unlinkTuyaCloud: (gatewayId: string) => call<void>(`/gateways/${gatewayId}/tuya-cloud/unlink`, {}),

    /**
     * Loads everything the canvas needs. The API is rate-limited per IP (120/min), so slow-changing data
     * (inventory, discovered BLE list, broker settings, catalog) is cached for 30 s; status, live readings
     * and events are fetched on every poll. `force` (after a mutation) refreshes everything.
     */
    async snapshot(force = false): Promise<Snapshot> {
      const settle = <T,>(p: Promise<T>) => p.then((value) => ({ status: "fulfilled" as const, value }), (reason: unknown) => ({ status: "rejected" as const, reason }));
      const stale = force || !slow || Date.now() - slow.at > 30000;
      if (stale) {
        const [g, so, d, se, c, rm, pj] = await Promise.all([settle(this.gateways()), settle(this.sources()), settle(this.devices()), settle(this.mqttSetup()), settle(this.catalog()), settle(this.removedDevices()), settle(this.projects())]);
        slow = { at: Date.now(), gateways: g, sources: so, devices: d, settings: se, catalog: c, removed: rm, projects: pj };
      }
      const { gateways, sources, devices, settings, catalog, removed, projects } = slow!;
      const [status, live, events, discovery] = await Promise.all([settle(this.mqttStatus()), settle(this.live()), settle(this.raw<{ items: DeviceEventRow[] }>("/events?limit=200")), settle(this.raw<{ items: Discovery[]; hidden_by_gateway?: Record<string, number> }>("/discovery"))]);
      if (catalog.status === "fulfilled") applyCatalog(catalog.value);
      // Gateways and MQTT status are required; everything else degrades gracefully.
      if (gateways.status === "rejected") throw gateways.reason;
      // MQTT status is an owner/admin endpoint. Operators and viewers still get the board: without it the
      // server clock comes from the live snapshot.
      const forbidden = status.status === "rejected" && status.reason instanceof ApiError && status.reason.status === 403;
      if (status.status === "rejected" && !forbidden) throw status.reason;
      const warnings: string[] = [];
      if (discovery.status === "rejected") warnings.push("โหลดอุปกรณ์ที่ยังไม่ลงทะเบียนไม่สำเร็จ · กดรีเฟรชเพื่อลองใหม่");
      if (settings.status === "rejected" && !forbidden) warnings.push("server ยังไม่เปิดบริการออกบัญชี MQTT อัตโนมัติ (MQTT_PUBLIC_HOST) · สร้าง gateway ได้ แต่ยังออกรหัส MQTT ไม่ได้");
      if (live.status === "rejected") warnings.push("โหลดค่าที่ถอดรหัสล่าสุดไม่สำเร็จ");
      if (sources.status === "rejected" && !forbidden) warnings.push("โหลดรายการอุปกรณ์ BLE ที่ค้นพบไม่สำเร็จ");
      if (devices.status === "rejected") warnings.push("โหลดรายการอุปกรณ์ที่ลงทะเบียนไม่สำเร็จ");
      // Both lists are hard-capped server-side; a full page means some registrations/observations may be missing.
      if (devices.status === "fulfilled" && devices.value.length >= 100) warnings.push("workspace นี้มีอุปกรณ์ลงทะเบียนถึง 100 รายการที่ API ส่งได้ · รายการที่เก่ากว่าอาจไม่แสดง อย่าลงทะเบียนซ้ำ");
      if (sources.status === "fulfilled") {
        const perGateway = new Map<string, number>();
        for (const src of sources.value) perGateway.set(src.gatewayID, (perGateway.get(src.gatewayID) ?? 0) + 1);
        if ([...perGateway.values()].some((n) => n >= 100)) warnings.push("บาง gateway ได้ยิน BLE ครบ 100 รายการที่ API ส่งได้ · อุปกรณ์บางตัวอาจไม่อยู่ในรายการค้นพบ");
      }
      return {
        discovery: discovery.status === "fulfilled" ? discovery.value.items : [],
        discoveryHidden: discovery.status === "fulfilled" ? discovery.value.hidden_by_gateway ?? {} : {},
        projects: projects.status === "fulfilled" ? projects.value : [],
        removedDevices: removed.status === "fulfilled" ? removed.value : [],
        events: events.status === "fulfilled" ? events.value.items : [],
        gateways: gateways.value,
        states: status.status === "fulfilled" ? status.value.items : [],
        serverTime: status.status === "fulfilled" ? Date.parse(status.value.server_time) : live.status === "fulfilled" && live.value ? Date.parse(live.value.server_time) : Date.now(),
        sources: sources.status === "fulfilled" ? sources.value : [],
        devices: devices.status === "fulfilled" ? devices.value : [],
        live: live.status === "fulfilled" ? live.value : null,
        settings: settings.status === "fulfilled" ? settings.value : null,
        warnings,
      };
    },
  };
}

function describe(status: number): string {
  switch (status) {
    case 400:
      return "ข้อมูลไม่ถูกต้อง ตรวจชื่อ รหัสอุปกรณ์ และ profile";
    case 401:
      return "เซสชันหมดอายุ กรุณาเข้าสู่ระบบอีกครั้ง";
    case 403:
      return "บัญชีนี้ไม่มีสิทธิ์จัดการอุปกรณ์ (ต้องเป็น owner หรือ admin)";
    case 404:
      return "ไม่พบรายการนี้ หรือถูกเพิกถอนไปแล้ว";
    case 409:
      return "รายการนี้มีอยู่แล้ว หรือถึงจำนวนสูงสุดที่ workspace นี้อนุญาต";
    case 429:
      return "เรียกใช้บ่อยเกินไป กรุณารอสักครู่";
    case 502:
      return "ส่งไปยังปลายทางไม่สำเร็จ";
    case 503:
      return "server ยังไม่เปิดบริการนี้ (ตรวจการตั้งค่า MQTT public endpoint)";
    default:
      return `ดำเนินการไม่สำเร็จ (${status})`;
  }
}
