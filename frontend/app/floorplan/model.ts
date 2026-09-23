import type { Device, DeviceEventRow, Gateway, LiveSensor, Snapshot } from "../topology/api";
import { deviceProfile } from "../topology/catalog";
import { latestMeasurements } from "../live/measurements";
import { doorStateOf, doorText, isDoorSensor, isOccupancySensor, occupancyOf, occupancyShort, type DoorState, type Occupancy } from "../live/office";

/** Floor plans are drawn in metres, origin at the top-left corner of the floor, y growing downwards. */
export type Point = [number, number];
export type Wall = { id: string; points: Point[]; thickness: number; closed?: boolean };
export type Zone = { id: string; name: string; kind: string; color: string; points: Point[]; gateway_ids?: string[]; note?: string };
export type Item = { id: string; type: string; x: number; y: number; w: number; h: number; rot: number; text?: string };
export type Background = { opacity: number; x: number; y: number; width_m: number; locked?: boolean };
export type Layout = { walls: Wall[]; zones: Zone[]; items: Item[]; background?: Background | null };
export type Placement = { asset_kind: "gateway" | "device"; asset_id: string; x: number; y: number; z: number };
export type Floor = { id: string; site_id: string; name: string; level: number; width_m: number; depth_m: number; ceiling_m: number; revision: number; has_image: boolean; updated_at: string; layout: Layout; placements: Placement[] };
export type Site = { id: string; project_id: string | null; name: string; description: string; created_at: string; floors: Floor[] };

export type Tool = "select" | "pan" | "wall" | "zone" | "rect" | "item" | "measure";
export type Selection = { kind: "wall" | "zone" | "item" | "placement"; id: string } | null;

export const ZONE_KINDS: [string, string][] = [["room", "ห้อง"], ["ward", "หอผู้ป่วย / วอร์ด"], ["corridor", "ทางเดิน"], ["restricted", "พื้นที่หวงห้าม"], ["storage", "คลัง / ห้องเก็บของ"], ["outdoor", "ภายนอกอาคาร"], ["other", "อื่น ๆ"]];
export const ITEM_TYPES: { id: string; label: string; w: number; h: number }[] = [
  { id: "door", label: "ประตู", w: 0.9, h: 0.15 },
  { id: "window", label: "หน้าต่าง", w: 1.2, h: 0.12 },
  { id: "stairs", label: "บันได", w: 2.4, h: 1.2 },
  { id: "elevator", label: "ลิฟต์", w: 1.6, h: 1.6 },
  { id: "exit", label: "ทางหนีไฟ", w: 1, h: 0.4 },
  { id: "bed", label: "เตียง", w: 1, h: 2 },
  { id: "desk", label: "โต๊ะ / เคาน์เตอร์", w: 1.6, h: 0.7 },
  { id: "rack", label: "ตู้ / rack", w: 0.8, h: 0.6 },
  { id: "extinguisher", label: "ถังดับเพลิง", w: 0.4, h: 0.4 },
  { id: "label", label: "ข้อความ", w: 2, h: 0.6 },
];
export const COLORS: Record<string, string> = { mint: "#a7f3d0", blue: "#80b7ff", amber: "#f6c177", coral: "#ff8f70", violet: "#c4a7ff", slate: "#9fb1b6" };

export const uid = (prefix: string) => `${prefix}${Math.random().toString(36).slice(2, 9)}${Date.now().toString(36).slice(-3)}`;
export const snapTo = (v: number, step: number) => (step > 0 ? Math.round(v / step) * step : v);
export const round2 = (v: number) => Math.round(v * 100) / 100;
export const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));
export const dist = (a: Point, b: Point) => Math.hypot(a[0] - b[0], a[1] - b[1]);

export function centroid(points: Point[]): Point {
  let a = 0, cx = 0, cy = 0;
  for (let i = 0; i < points.length; i++) {
    const [x0, y0] = points[i], [x1, y1] = points[(i + 1) % points.length];
    const f = x0 * y1 - x1 * y0;
    a += f; cx += (x0 + x1) * f; cy += (y0 + y1) * f;
  }
  if (Math.abs(a) < 1e-9) return [points.reduce((s, p) => s + p[0], 0) / points.length, points.reduce((s, p) => s + p[1], 0) / points.length];
  return [cx / (3 * a), cy / (3 * a)];
}
export function area(points: Point[]): number {
  let a = 0;
  for (let i = 0; i < points.length; i++) a += points[i][0] * points[(i + 1) % points.length][1] - points[(i + 1) % points.length][0] * points[i][1];
  return Math.abs(a) / 2;
}
export function pointInPolygon(p: Point, poly: Point[]): boolean {
  let inside = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i++) {
    const [xi, yi] = poly[i], [xj, yj] = poly[j];
    if (yi > p[1] !== yj > p[1] && p[0] < ((xj - xi) * (p[1] - yi)) / (yj - yi) + xi) inside = !inside;
  }
  return inside;
}
export function wallLength(w: Wall): number {
  let total = 0;
  for (let i = 1; i < w.points.length; i++) total += dist(w.points[i - 1], w.points[i]);
  if (w.closed && w.points.length > 2) total += dist(w.points[w.points.length - 1], w.points[0]);
  return total;
}
export const emptyLayout = (): Layout => ({ walls: [], zones: [], items: [], background: null });

/** What the editor mutates; saved as one unit with the floor's revision. */
export type Draft = { name: string; level: number; width_m: number; depth_m: number; ceiling_m: number; layout: Layout; placements: Placement[] };
export const draftOf = (f: Floor): Draft => ({ name: f.name, level: f.level, width_m: f.width_m, depth_m: f.depth_m, ceiling_m: f.ceiling_m, layout: { walls: f.layout.walls ?? [], zones: f.layout.zones ?? [], items: f.layout.items ?? [], background: f.layout.background ?? null }, placements: f.placements ?? [] });

/** Live view of an asset for the plan: name, kind, freshness and the numbers worth showing on a marker. */
export type AssetView = {
  kind: "gateway" | "device";
  id: string;
  name: string;
  model: string;
  external?: string;
  online: boolean;
  wearable: boolean;
  zoneGatewayId: string | null;
  summary: string;
  alert: boolean;
  /** An open, unacknowledged emergency button press on this device: the plan says WHERE to run. */
  sos: boolean;
  profileId?: string;
  /** PIR occupancy sensor (MOS MSP01): tints the zone it is placed in. */
  occupancy?: Occupancy;
  /** Door sensor (MOS S4): drawn as a door glyph, open/closed, amber when left open. */
  door?: DoorState;
};

const FRESH_MS = 60000;
const fresh = (at: string | null | undefined, now: number) => !!at && now - Date.parse(at) <= FRESH_MS;

export function summarizeReading(s: LiveSensor | undefined): string {
  if (!s) return "";
  const r = s.latest, parts: string[] = [];
  if (typeof r.temperature === "number" && (s.kind ?? r.kind ?? "environment") === "environment") parts.push(`${r.temperature.toFixed(1)} °C`);
  if (typeof r.humidity === "number" && (s.kind ?? r.kind ?? "environment") === "environment") parts.push(`${r.humidity.toFixed(0)} %`);
  const m = (r.metrics ?? {}) as Record<string, number>;
  if (m.tamper === 1) parts.push("ถูกถอด");
  if (m.vibration === 1 || m.motion === 1) parts.push("เคลื่อนไหว");
  if (m.leak === 1) parts.push("น้ำรั่ว");
  if (typeof r.battery === "number" && r.battery > 0) parts.push(`แบต ${r.battery}%`);
  return parts.join(" · ");
}

/** `sosExternals` holds the MACs (lowercase) with an open SOS alert; the API decides what an SOS is. */
export function buildAssets(s: Snapshot | null, wearableProfiles: Set<string>, sosExternals: Set<string> = new Set()): AssetView[] {
  if (!s) return [];
  const out: AssetView[] = [];
  const now = s.serverTime;
  const live = new Map((s.live?.gateways ?? []).map((g) => [g.gateway.id, g]));
  const eventsBy = new Map<string, DeviceEventRow[]>();
  for (const e of s.events ?? []) { const k = e.external_id.toLowerCase(); eventsBy.set(k, [...(eventsBy.get(k) ?? []), e]); }
  for (const g of s.gateways as Gateway[]) {
    const l = live.get(g.id);
    out.push({ kind: "gateway", id: g.id, name: g.name, model: g.model, online: fresh(l?.last_packet_at, now), wearable: false, zoneGatewayId: null, summary: l ? `${l.sensors.length} sensor · ${l.nearby_devices} BLE` : "", alert: false, sos: false });
  }
  for (const d of s.devices as Device[]) {
    const ext = d.external_id.toLowerCase();
    let best: LiveSensor | undefined;
    for (const g of s.live?.gateways ?? []) for (const sensor of g.sensors) if (sensor.id.toLowerCase() === ext && (!best || Date.parse(sensor.latest.received_at) > Date.parse(best.latest.received_at))) best = sensor;
    const m = (best?.latest.metrics ?? {}) as Record<string, number>, sos = sosExternals.has(ext);
    const profile = deviceProfile(d.profile_id), merged = best ? latestMeasurements(best) : null;
    const samples = best ? [...best.history, best.latest] : [], events = eventsBy.get(ext) ?? [];
    const door = isDoorSensor(profile, merged) ? doorStateOf(merged, samples, events, now, (s.events ?? []).length >= 200) : undefined;
    const occupancy = !door && isOccupancySensor(profile, merged) ? occupancyOf(samples, events, now) : undefined;
    const summary = sos ? "กดปุ่มฉุกเฉิน" : door ? doorText(door, now) : occupancy ? (best ? occupancyShort(occupancy) : "") : summarizeReading(best);
    out.push({ kind: "device", id: d.id, name: d.name, model: best?.model ?? "", external: ext, online: fresh(best?.latest.received_at, now), wearable: !!d.roaming || wearableProfiles.has(d.profile_id), zoneGatewayId: d.zone_gateway_id ?? null, summary, alert: sos || m.tamper === 1 || m.leak === 1, sos, profileId: d.profile_id, occupancy, door });
  }
  return out;
}

/** Wearables are not pinned to the plan: they are drawn around the gateway whose zone they are in. */
export function wearablesAt(assets: AssetView[], gatewayId: string): AssetView[] {
  // An SOS stays on the plan even once the tag's last uplink is no longer fresh: the operator is running
  // towards the last place the person was heard, and a marker that disappears after a minute is worse than
  // a stale one.
  return assets.filter((a) => a.kind === "device" && a.wearable && (a.online || a.sos) && a.zoneGatewayId === gatewayId);
}
/**
 * Occupancy of a drawn zone from the PIR sensors placed inside its polygon: occupied when any of them is,
 * with the freshest motion. null when no occupancy sensor is placed in the zone (the zone keeps its own colour).
 */
export function zoneOccupancy(zone: Zone, placements: Placement[], assets: Map<string, AssetView>): Occupancy | null {
  let found: Occupancy | null = null;
  for (const p of placements) {
    if (p.asset_kind !== "device") continue;
    const o = assets.get(`device:${p.asset_id}`)?.occupancy;
    if (!o || !pointInPolygon([p.x, p.y], zone.points)) continue;
    if (!found || (o.occupied && (!found.occupied || (o.minutesAgo ?? Infinity) < (found.minutesAgo ?? Infinity)))) found = o;
  }
  return found;
}
export { occupancyShort };

export function ringOffset(index: number, count: number, radius: number): Point {
  const angle = (index / Math.max(count, 1)) * Math.PI * 2 - Math.PI / 5; // start to the right so labels do not sit on the gateway's name
  return [Math.cos(angle) * radius, Math.sin(angle) * radius];
}
