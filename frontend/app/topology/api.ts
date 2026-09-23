// Thin authenticated client for the Aether backend used by the topology page.
// Access tokens stay in memory (provided by the caller); a 401 triggers one refresh + retry.

import { applyCatalog, type CatalogResponse, type DeviceProfile } from "./catalog";

const API = import.meta.env.VITE_AETHER_API_ORIGIN ?? "";

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export type Gateway = { id: string; name: string; model: string; created_at: string; project_id?: string | null };
export type Project = { id: string; name: string; description: string; color: string; gateway_count: number; created_at: string };
export type MQTTState = { gateway_id: string; revision: number; applied_revision: number; applied_at: string | null; last_packet_at: string | null };
export type Source = { key: string; id: string; name: string; gatewayID: string; gatewayName: string };
export type Device = { id: string; gateway_id: string; name: string; external_id: string; profile_id: string; created_at: string; removed_at?: string | null; /** Wearables: followed across every gateway of the workspace. */ roaming?: boolean; /** Stable zone decided by the server (smoothed RSSI with hysteresis). */ zone_gateway_id?: string | null };
export type Beacon = { type: "ibeacon" | "eddystone_uid" | "eddystone_tlm"; uuid?: string; major?: number; minor?: number; namespace?: string; instance?: string; tx_power?: number; voltage?: number; adv_count?: number; uptime_s?: number };
export type Reading = {
  source?: string;
  received_at: string;
  /** environment | motion | tamper | leak | light | beacon | info; missing on samples stored before multi-frame decoding (= environment). */
  kind?: string;
  model?: string;
  frames?: string[];
  temperature: number;
  humidity: number;
  battery: number;
  rssi: number | null;
  metrics?: Record<string, number>;
  beacon?: Beacon;
};
export type LiveSensor = { id: string; name: string; kind?: string; model?: string; template_id?: string | null; latest: Reading; history: Reading[] };
export type LiveGateway = { gateway: Gateway; last_packet_at: string | null; packet_count: number; observation_count: number; nearby_devices: number; sensors: LiveSensor[] };
export type Live = { gateways: LiveGateway[]; server_time: string; deployment_mode: string };
export type MQTTSettings = { host: string; port: number; scheme: string; tls: boolean; qos: number; keep_alive: number; /** Present only when the server opted in to an unencrypted listener for gateways without TLS. */ plaintext?: { port: number; scheme: string } };
export type MQTTCredentials = MQTTSettings & {
  gateway_id: string;
  username: string;
  password: string;
  client_id: string;
  post_topic: string;
  subscribe_topic: string;
  reply_topic: string;
};
export type GatewayCreated = { gateway: Gateway; token: string; capture_path: string };
export type Me = { user_id: string; tenant_id: string; role: string; deployment_mode: string };

export type DeviceEventRow = { id: string; gateway_id: string; external_id: string; device_name: string; event_type: string; detail: Record<string, unknown>; occurred_at: string };
export type Discovery = { gateway_id: string; external_id: string; last_seen: string; source: string; model?: string; kind?: string; rssi: number | null; profile?: DeviceProfile };
export type Snapshot = {
  discovery?: Discovery[];
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
  async function call<T>(path: string, body?: unknown): Promise<T> {
    const send = () =>
      fetch(`${API}/api/v1${path}`, {
        method: body === undefined ? "GET" : "POST",
        headers: { Authorization: `Bearer ${getToken()}`, "Content-Type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
        cache: "no-store",
        signal: AbortSignal.timeout(15000),
      });
    let r = await send();
    if (r.status === 401 && (await refresh())) r = await send();
    if (!r.ok) {
      // Some endpoints explain the failure (e.g. a channel test); prefer that over the generic status text.
      let detail = "";
      try {
        const body = (await r.json()) as { detail?: unknown };
        if (typeof body.detail === "string") detail = body.detail;
      } catch {
        // no JSON body
      }
      throw new ApiError(r.status, detail ? `${describe(r.status)} · ${detail}` : describe(r.status));
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
    me: () => call<Me>("/me"),
    catalog: () => call<CatalogResponse>("/catalog"),
    gateways: async () => (await call<{ items: Gateway[] }>("/gateways")).items,
    mqttStatus: () => call<{ items: MQTTState[]; server_time: string }>("/gateways/mqtt-status"),
    mqttSetup: () => call<MQTTSettings>("/mqtt/setup"),
    sources: async () => (await call<{ items: Source[] }>("/studio/sources")).items,
    devices: async () => (await call<{ items: Device[] }>("/devices")).items,
    live: () => call<Live>("/live?range=1h"),
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
      const [status, live, events, discovery] = await Promise.all([settle(this.mqttStatus()), settle(this.live()), settle(this.raw<{ items: DeviceEventRow[] }>("/events?limit=200")), settle(this.raw<{ items: Discovery[] }>("/discovery"))]);
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
