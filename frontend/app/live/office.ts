// Smart-office derivations (Minew MOS kit: MSP01 PIR, S4 door, S1 temperature/humidity).
// Shared by the operations board and the floor plan so both say the same thing about a room.
// Every function takes the server clock (`now`) so nothing reads Date.now() during render.

import type { DeviceEventRow, Reading } from "../topology/api";
import type { DeviceProfile } from "../topology/catalog";

/** A room is "มีคน" while PIR motion was seen within this window. */
export const OCCUPIED_WINDOW_MS = 5 * 60_000;
/** A door open longer than this is highlighted (warning tone). */
export const DOOR_LEFT_OPEN_MS = 10 * 60_000;
/** Occupancy strip in the device drawer: 24 h in 15-minute cells. */
export const STRIP_CELL_MS = 15 * 60_000;
export const STRIP_HOURS = 24;

/**
 * Office comfort band. Inside both ranges = สบาย; outside, the room is named by what is off
 * (ร้อน / เย็น for temperature, ชื้น / แห้ง for humidity). Loosely after ASHRAE 55 for sedentary office work.
 */
export const COMFORT = { temperature: { min: 23, max: 26 }, humidity: { min: 40, max: 60 } } as const;

const BANGKOK_OFFSET_MS = 7 * 3600_000;
const metric = (r: Reading | null | undefined, key: string): number | undefined => (r?.metrics as Record<string, number> | undefined)?.[key];
const time = (s: string | null | undefined) => (s ? Date.parse(s) : NaN);

/** Start of "today" in Asia/Bangkok (UTC+7, no DST), as epoch ms. */
export function startOfBangkokDay(now: number): number {
  return Math.floor((now + BANGKOK_OFFSET_MS) / 86400_000) * 86400_000 - BANGKOK_OFFSET_MS;
}

/** Duration in Thai, short: "45 วิ", "12 นาที", "3 ชม. 20 นาที", "2 วัน". */
export function duration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} วิ`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} นาที`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h} ชม. ${m % 60} นาที` : `${h} ชม.`;
  return `${Math.floor(h / 24)} วัน`;
}

// ---------- Occupancy (PIR) ----------

/** A PIR occupancy sensor: flagged by its profile, or any device whose readings carry `metrics.motion` (only the PIR frame sets it). */
export function isOccupancySensor(profile: DeviceProfile | undefined, reading: Reading | null | undefined): boolean {
  return !!profile?.occupancy || metric(reading, "motion") != null;
}

export type Occupancy = {
  occupied: boolean;
  /** Most recent motion=1 seen (reading or `occupied`/`motion` event), if any is known. */
  lastMotionAt: string | null;
  /** Minutes since that motion, rounded down; null when unknown. */
  minutesAgo: number | null;
};

/** `readings` = latest + history of the sensor; `events` = this device's event log rows (any order). */
export function occupancyOf(readings: Reading[], events: DeviceEventRow[], now: number): Occupancy {
  let last = NaN, lastAt: string | null = null;
  for (const r of readings) {
    const t = time(r.received_at);
    if (metric(r, "motion") === 1 && t <= now + 60_000 && !(t <= last)) { last = t; lastAt = r.received_at; }
  }
  // The live history is short (1 h); the event log remembers the last time the room became occupied.
  for (const e of events) {
    const t = time(e.occurred_at);
    if ((e.event_type === "occupied" || e.event_type === "motion") && !(t <= last)) { last = t; lastAt = e.occurred_at; }
  }
  if (!lastAt) return { occupied: false, lastMotionAt: null, minutesAgo: null };
  const age = Math.max(0, now - last);
  return { occupied: age <= OCCUPIED_WINDOW_MS, lastMotionAt: lastAt, minutesAgo: Math.floor(age / 60_000) };
}

/** "มีคน · 3 นาที" / "ว่าง" — the short form used on floor-plan zones and markers. */
export function occupancyShort(o: Occupancy): string {
  if (!o.occupied) return "ว่าง";
  return o.minutesAgo != null && o.minutesAgo > 0 ? `มีคน · ${o.minutesAgo} นาที` : "มีคน · เมื่อสักครู่";
}

/** "เคลื่อนไหวล่าสุด N นาทีที่แล้ว". */
export function lastMotionText(o: Occupancy, now: number): string {
  if (!o.lastMotionAt) return "ยังไม่พบการเคลื่อนไหว";
  const age = now - time(o.lastMotionAt);
  return age < 60_000 ? "เคลื่อนไหวล่าสุดเมื่อสักครู่" : `เคลื่อนไหวล่าสุด ${duration(age)}ที่แล้ว`;
}

export type StripCell = "on" | "off" | "none";

/**
 * Activity in fixed cells ending now, oldest first: on = the flag (`motion`, or `door` open) was 1 in the cell,
 * off = the sensor reported 0 only, none = no data. Defaults: 24 h of 15-minute cells (the drawer strip).
 */
export function activityStrip(readings: Reading[], now: number, key: "motion" | "door" = "motion", hours = STRIP_HOURS, cellMs = STRIP_CELL_MS): StripCell[] {
  const cells = Math.round((hours * 3600_000) / cellMs);
  const end = Math.ceil(now / cellMs) * cellMs, start = end - cells * cellMs;
  const out: StripCell[] = Array.from({ length: cells }, () => "none");
  for (const r of readings) {
    const m = metric(r, key);
    if (m == null) continue;
    const i = Math.floor((time(r.received_at) - start) / cellMs);
    if (i < 0 || i >= cells) continue;
    if (m === 1) out[i] = "on";
    else if (out[i] === "none") out[i] = "off";
  }
  return out;
}

// ---------- Door (S4) ----------

export function isDoorSensor(profile: DeviceProfile | undefined, reading: Reading | null | undefined): boolean {
  return !!profile?.door || profile?.kinds?.[0] === "door" || reading?.kind === "door" || metric(reading, "door") != null;
}

export type DoorState = {
  /** true open, false closed, null unknown (no door frame and no door event yet). */
  open: boolean | null;
  /** When the door entered its current state, if known. */
  since: string | null;
  /** false when `since` is only the start of the visible history, i.e. "at least this long". */
  sinceExact: boolean;
  /** Open for longer than DOOR_LEFT_OPEN_MS. */
  leftOpen: boolean;
  openCountToday: number;
  /** The event page may not reach back to midnight: the count is a lower bound. */
  countPartial: boolean;
};

const DOOR_EVENTS = new Set(["door_open", "door_closed"]);

/**
 * `latest` is the merged latest reading (live board: `latestMeasurements`), `history` the raw readings,
 * `events` this device's rows, `pageFull` whether the global event page was full (count may be partial).
 */
export function doorStateOf(latest: Reading | null, history: Reading[], events: DeviceEventRow[], now: number, pageFull = false): DoorState {
  const doorEvents = events.filter((e) => DOOR_EVENTS.has(e.event_type)).sort((a, b) => time(b.occurred_at) - time(a.occurred_at));
  const lastEvent = doorEvents[0];
  const rows = history.filter((r) => metric(r, "door") != null).sort((a, b) => time(a.received_at) - time(b.received_at));
  const newest = rows[rows.length - 1];
  let open: boolean | null = null;
  const value = metric(latest, "door") ?? metric(newest, "door");
  const readingAt = newest ? time(newest.received_at) : NaN;
  if (value != null && !(lastEvent && time(lastEvent.occurred_at) > readingAt)) open = value === 1;
  else if (lastEvent) open = lastEvent.event_type === "door_open";

  let since: string | null = null, sinceExact = false;
  if (open !== null) {
    const matching = doorEvents.find((e) => e.event_type === (open ? "door_open" : "door_closed"));
    // Start of the trailing run of readings in the same state.
    let runStart: Reading | undefined, broke = false;
    for (let i = rows.length - 1; i >= 0; i--) {
      if ((metric(rows[i], "door") === 1) !== open) { broke = true; break; }
      runStart = rows[i];
    }
    const flippedAfterEvent = matching && rows.some((r) => time(r.received_at) > time(matching.occurred_at) && (metric(r, "door") === 1) !== open);
    if (matching && !flippedAfterEvent) { since = matching.occurred_at; sinceExact = true; }
    else if (runStart) { since = runStart.received_at; sinceExact = broke; }
  }
  const dayStart = startOfBangkokDay(now);
  const openCountToday = doorEvents.filter((e) => e.event_type === "door_open" && time(e.occurred_at) >= dayStart).length;
  return { open, since, sinceExact, leftOpen: open === true && !!since && now - time(since) > DOOR_LEFT_OPEN_MS, openCountToday, countPartial: pageFull };
}

/** "เปิดอยู่ 12 นาที" / "ปิดอยู่ อย่างน้อย 1 ชม." / "ยังไม่ทราบสถานะ". */
export function doorText(d: DoorState, now: number): string {
  if (d.open === null) return "ยังไม่ทราบสถานะ";
  const state = d.open ? "เปิดอยู่" : "ปิดอยู่";
  if (!d.since) return state;
  return `${state} ${d.sinceExact ? "" : "อย่างน้อย "}${duration(now - time(d.since))}`;
}

// ---------- Comfort (S1 or any temperature/humidity reading) ----------

export type Comfort = { label: string; ok: boolean };

export function comfortOf(temperature: number | undefined, humidity: number | undefined): Comfort | null {
  const t = temperature != null && Number.isFinite(temperature) ? temperature : null;
  const h = humidity != null && Number.isFinite(humidity) ? humidity : null;
  if (t === null && h === null) return null;
  const parts: string[] = [];
  if (t !== null && t > COMFORT.temperature.max) parts.push("ร้อน");
  if (t !== null && t < COMFORT.temperature.min) parts.push("เย็น");
  if (h !== null && h > COMFORT.humidity.max) parts.push("ชื้น");
  if (h !== null && h < COMFORT.humidity.min) parts.push("แห้ง");
  return parts.length ? { label: parts.join(" · "), ok: false } : { label: "สบาย", ok: true };
}

export const COMFORT_NOTE = `สบาย = ${COMFORT.temperature.min}–${COMFORT.temperature.max} °C และ ${COMFORT.humidity.min}–${COMFORT.humidity.max} %RH`;
