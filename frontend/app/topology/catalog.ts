// Catalog registry. The backend is the source of truth (`GET /api/v1/catalog`, see
// backend/internal/domain/catalog.go); the lists below are only the offline fallback until it loads.

export type GatewayModel = {
  id: string;
  brand: string;
  model: string;
  label: string;
  transport: "mqtt" | "http";
  description: string;
  logo?: string;
  /** Manufacturer product photo, shown for identification. */
  image?: string;
  verified?: boolean;
};

export type DeviceProfile = {
  id: string;
  brand: string;
  model: string;
  label: string;
  radio: "ble" | "zigbee" | "any";
  description: string;
  image?: string;
  metrics: string[];
  /** Reading kinds the device is expected to produce (environment, motion, beacon, tamper, …). */
  kinds?: string[];
  /** Carried by a person: adopting one proposes roaming across gateways. */
  wearable?: boolean;
  /** PIR occupancy sensor (MSP01): the room is occupied while `metrics.motion` = 1 was seen recently. */
  occupancy?: boolean;
  /** Door contact sensor (S4): `metrics.door` 1 open / 0 closed. */
  door?: boolean;
  /** True only when a captured packet from the physical device passes a golden test in the backend. */
  verified?: boolean;
  /** Name the tag reports in its FFE1 info frame; used to suggest this profile. */
  info_name?: string;
  /** Other names real units report for this model (the physical S1 says "PLUS", the E8S says "E8"). */
  info_aliases?: string[];
  /** Switch outputs of the largest variant (Zigbee wall switches); absent for sensors. */
  gangs?: number;
  /** A device Aether may switch in a later phase; today its state is only displayed. */
  actuator?: boolean;
  /** Zigbee2MQTT definition models this profile covers. */
  z2m_models?: string[];
  notes?: string;
};

export type CatalogResponse = { gateway_models: GatewayModel[]; device_profiles: DeviceProfile[] };

// Product photos are static files that may ship after the catalog names them (MG4, MSP01 and S4 did).
// A photo is only handed to the UI once the browser has loaded it, so no page ever shows a broken image;
// until then (or if it 404s) every consumer falls back to its icon.
const photoStatus = new Map<string, "ok" | "failed" | "loading">();
let lastCatalog: CatalogResponse | null = null;
function usablePhoto(src: string | undefined): string | undefined {
  if (!src) return undefined;
  if (typeof window === "undefined" || typeof Image === "undefined") return src;
  const status = photoStatus.get(src);
  if (status === "ok") return src;
  if (status === undefined) {
    photoStatus.set(src, "loading");
    const img = new Image();
    img.onload = () => { photoStatus.set(src, "ok"); if (lastCatalog) applyCatalog(lastCatalog); };
    img.onerror = () => photoStatus.set(src, "failed");
    img.src = src;
  }
  return undefined;
}

/** Replaces the registry contents in place so every helper below sees the server's catalog. */
export function applyCatalog(c: CatalogResponse): void {
  lastCatalog = c;
  if (Array.isArray(c.gateway_models) && c.gateway_models.length) GATEWAY_MODELS.splice(0, GATEWAY_MODELS.length, ...c.gateway_models.map((m) => ({ ...m, image: usablePhoto(m.image) })));
  if (Array.isArray(c.device_profiles) && c.device_profiles.length) DEVICE_PROFILES.splice(0, DEVICE_PROFILES.length, ...c.device_profiles.map((p) => ({ ...p, image: usablePhoto(p.image) })));
}

export const GATEWAY_MODELS: GatewayModel[] = [
  {
    id: "minew-mg3",
    brand: "Minew",
    model: "MG3",
    label: "Minew MG3",
    transport: "mqtt",
    description: "USB Mini BLE → Wi‑Fi gateway · ส่ง BLE advertisement เข้า Aether ผ่าน MQTT",
    logo: "/brands/minew.png",
    image: "/devices/minew-mg3.png",
  },
  {
    id: "minew-mg4",
    brand: "Minew",
    model: "MG4",
    label: "Minew MG4",
    transport: "mqtt",
    description: "BLE → Wi‑Fi / Ethernet gateway ของชุด MOS smart office · ส่ง BLE advertisement เข้า Aether ผ่าน MQTT",
    logo: "/brands/minew.png",
    image: "/devices/minew-mg4.png",
  },
  {
    id: "zigbee2mqtt",
    brand: "Zigbee2MQTT",
    model: "Zigbee coordinator",
    label: "Zigbee2MQTT",
    transport: "mqtt",
    description: "Zigbee coordinator (เช่น SLZB-06M) + Zigbee2MQTT บนเครื่องในอาคาร · ส่งสถานะอุปกรณ์ Zigbee เข้า Aether ผ่าน MQTT over TLS · ไม่ใช้ Tuya cloud",
  },
  {
    id: "generic-http",
    brand: "Generic",
    model: "HTTP gateway",
    label: "Generic HTTP",
    transport: "http",
    description: "Gateway ทั่วไปที่ POST JSON เข้า Aether ด้วย HTTP Basic (gateway id + token)",
    image: "/devices/generic-http-gateway.png",
  },
];

export const DEVICE_PROFILES: DeviceProfile[] = [
  {
    id: "minew-s1-pending@1",
    brand: "Minew",
    model: "S1",
    label: "Minew S1 · อุณหภูมิ / ความชื้น",
    radio: "ble",
    description: "BLE sensor · Aether ถอดค่าจากเฟรม FFE1 A1‑01 · profile นี้ใช้ลงทะเบียนสินทรัพย์ ไม่ยืนยันรุ่นจริงจากเฟรมเพียงอย่างเดียว",
    image: "/devices/minew-s1.png",
    metrics: ["temperature", "humidity", "battery"],
    kinds: ["environment"],
    verified: true,
    info_name: "S1",
    info_aliases: ["PLUS"],
  },
  {
    id: "minew-msp01-pending@1",
    brand: "Minew",
    model: "MSP01",
    label: "Minew MSP01 · PIR ตรวจจับคน",
    radio: "ble",
    description: "PIR occupancy sensor ของชุด MOS · มีคนเมื่อพบการเคลื่อนไหวภายใน 5 นาที",
    image: "/devices/minew-msp01.png",
    metrics: ["motion", "battery"],
    kinds: ["motion", "environment"],
    occupancy: true,
    info_name: "MSP01",
  },
  {
    id: "minew-s4-pending@1",
    brand: "Minew",
    model: "S4",
    label: "Minew S4 · เซ็นเซอร์ประตู",
    radio: "ble",
    description: "Door / window contact sensor ของชุด MOS · door = 1 เปิด / 0 ปิด",
    image: "/devices/minew-s4.png",
    metrics: ["door", "battery"],
    kinds: ["door"],
    door: true,
    info_name: "S4",
  },
  {
    id: "tuya-ts001x-switch@1",
    brand: "Tuya",
    model: "TS001x",
    label: "Tuya Zigbee wall switch · 1–4 ช่อง",
    radio: "zigbee",
    description: "สวิตช์ผนังแบบสัมผัส ไม่ใช้สายกลาง ผ่าน Zigbee2MQTT · แสดงสถานะเปิด/ปิดแต่ละช่อง · ยังสั่งเปิด/ปิดจาก Aether ไม่ได้ในเฟสนี้",
    metrics: ["สถานะเปิด/ปิดแต่ละช่อง", "linkquality"],
    kinds: ["switch"],
    gangs: 4,
    actuator: true,
    z2m_models: ["TS0011", "TS0012", "TS0013", "TS0014", "TS0601"],
  },
  {
    id: "generic-environment@1",
    brand: "Generic",
    model: "Environment sensor",
    label: "Generic environment sensor",
    radio: "any",
    description: "อุปกรณ์ทั่วไปที่ส่ง telemetry.v1 ผ่าน gateway · ใช้ทดสอบเส้นทางข้อมูลหรืออุปกรณ์ต่างแบรนด์",
    image: "/devices/generic-environment.png",
    metrics: ["ตาม payload ที่ส่งเข้ามา"],
    kinds: ["environment"],
  },
];

export const gatewayModel = (id: string): GatewayModel | undefined => GATEWAY_MODELS.find((m) => m.id === id);
export const deviceProfile = (id: string): DeviceProfile | undefined => DEVICE_PROFILES.find((p) => p.id === id);

export const deviceBrands = (): string[] => [...new Set(DEVICE_PROFILES.map((p) => p.brand))];
export const profilesForBrand = (brand: string): DeviceProfile[] => DEVICE_PROFILES.filter((p) => p.brand === brand);

/** Formats a 12-hex BLE MAC as AA:BB:CC:DD:EE:FF; other identifiers are returned unchanged. */
export function formatMAC(id: string): string {
  return /^[0-9a-f]{12}$/i.test(id) ? id.match(/.{2}/g)!.join(":").toUpperCase() : id;
}

/** Whether a name from a tag's FFE1 info frame identifies this profile (its info name or a known alias). */
export function matchesInfo(p: DeviceProfile, name: string): boolean {
  const n = name.trim().toLowerCase();
  return !!n && [p.info_name, ...(p.info_aliases ?? [])].some((x) => x?.toLowerCase() === n);
}

/** Best-effort profile suggestion from what the gateway actually decoded. */
/** The generic profile every Zigbee2MQTT device can register as (backend domain.Z2MGenericProfile). */
export const Z2M_GENERIC_PROFILE = "zigbee2mqtt-device@1";

export function suggestProfile(input: { model?: string | null; kind?: string | null; hasBeacon?: boolean; /** The reading carries `metrics.motion` (only the PIR frame sets it). */ hasPIR?: boolean; /** A Zigbee2MQTT device: only Zigbee profiles, never a BLE profile matched by kind. */ zigbee?: boolean }): DeviceProfile | undefined {
  if (input.zigbee) {
    const name = input.model?.toLowerCase() ?? "";
    const byZ2M = name ? DEVICE_PROFILES.find((p) => p.radio === "zigbee" && p.z2m_models?.some((m) => m.toLowerCase() === name && (name !== "ts0601" || input.kind === "switch"))) : undefined;
    return byZ2M ?? DEVICE_PROFILES.find((p) => p.id === Z2M_GENERIC_PROFILE) ?? DEVICE_PROFILES.find((p) => p.radio === "zigbee");
  }
  if (input.model) {
    const name = input.model.toLowerCase();
    const byName = DEVICE_PROFILES.find((p) => matchesInfo(p, name));
    if (byName) return byName;
    // MOS kit names, in case the server catalog does not carry info_name for them.
    if (name === "msp01") { const p = DEVICE_PROFILES.find((x) => x.occupancy); if (p) return p; }
    if (name === "s4") { const p = DEVICE_PROFILES.find((x) => x.door); if (p) return p; }
    // Zigbee2MQTT definition models (TS0012, …); TS0601 is Tuya's catch-all id, so only for a switch.
    const byZ2M = DEVICE_PROFILES.find((p) => p.z2m_models?.some((m) => m.toLowerCase() === name && (name !== "ts0601" || input.kind === "switch")));
    if (byZ2M) return byZ2M;
  }
  if (input.kind === "switch") return DEVICE_PROFILES.find((p) => p.kinds?.includes("switch"));
  const kind = input.kind ?? "";
  const withKind = (k: string) => DEVICE_PROFILES.filter((p) => p.kinds?.includes(k));
  if (kind === "environment") return withKind("environment")[0];
  if (kind === "tamper") return withKind("tamper")[0];
  if (kind === "door") return DEVICE_PROFILES.find((p) => p.door) ?? withKind("door")[0];
  if (kind === "motion" && input.hasPIR) { const p = DEVICE_PROFILES.find((x) => x.occupancy); if (p) return p; }
  if (kind === "motion") return (input.hasBeacon ? withKind("motion").find((p) => p.kinds?.includes("beacon")) : withKind("motion").find((p) => !p.kinds?.includes("beacon"))) ?? withKind("motion")[0];
  if (kind === "beacon") return DEVICE_PROFILES.find((p) => p.id === "generic-ble-beacon@1") ?? withKind("beacon")[0];
  return undefined;
}

/** Product photos by model name; static so pages that never load the API catalog (Dashboard Studio) can use them. */
const MODEL_PHOTOS: [string, string][] = [
  ["MBT01", "/devices/minew-mbt01.png"],
  ["E8S", "/devices/minew-e8s.png"],
  ["B10", "/devices/minew-b10.png"],
  ["C10", "/devices/minew-c10.png"],
  ["B7", "/devices/minew-b7.png"],
  ["S1", "/devices/minew-s1.png"],
  ["MG3", "/devices/minew-mg3.png"],
  ["MG4", "/devices/minew-mg4.png"],
  ["MSP01", "/devices/minew-msp01.png"],
  ["S4", "/devices/minew-s4.png"],
];

/** Product photo for a free-text model label such as "MBT01 / A1-20" or "E8S · C10 · B7"; the model named first wins. */
export function imageForModelLabel(label: string): string | undefined {
  const upper = label.toUpperCase();
  let best: { at: number; src: string } | undefined;
  for (const [model, src] of MODEL_PHOTOS) {
    const m = new RegExp(`(^|[^A-Z0-9])${model}([^A-Z0-9]|$)`).exec(upper);
    if (m && (!best || m.index < best.at)) best = { at: m.index, src };
  }
  return best?.src;
}

/** The gateway model whose MQTT account publishes a Zigbee2MQTT topic tree. */
export const Z2M_GATEWAY_MODEL = "zigbee2mqtt";

/** Mirrors domain.ProfileAllowedOn: Zigbee profiles only under a Zigbee2MQTT gateway, and only Zigbee profiles there. */
export function profileFitsGateway(p: DeviceProfile, gatewayModelId: string): boolean {
  return gatewayModelId === Z2M_GATEWAY_MODEL ? p.radio === "zigbee" : p.radio !== "zigbee";
}

/** Switch outputs a reading carries, in gang order: [[1, true], [2, false], …] from metrics sw1..sw4. */
export function switchGangs(metrics: Record<string, number> | undefined): [number, boolean][] {
  const out: [number, boolean][] = [];
  for (let gang = 1; gang <= 4; gang++) {
    const v = metrics?.[`sw${gang}`];
    if (v === 0 || v === 1) out.push([gang, v === 1]);
  }
  return out;
}
