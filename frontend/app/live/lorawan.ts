// LoRaWAN sensors on the overview (Milesight behind a UG65, docs/platform/milesight-lorawan.md). They report every
// few minutes to hours rather than every few seconds, and they report states (occupied / vacant) rather than motion
// pulses, so freshness and occupancy are judged per model; every value they send is listed, not only the ones a
// generic card has room for.

import type { Reading } from "../topology/api";
import type { DeviceProfile } from "../topology/catalog";

/** Decoder name of a Milesight uplink (backend milesight.FrameUplink). */
export const MILESIGHT_FRAME = "milesight-up@1";

export const isLoRaWAN = (profile: DeviceProfile | undefined) => profile?.radio === "lorawan";

/** Default reporting interval in minutes (Milesight's factory setting per model); a device set to report less often
 *  shows as stale between its uplinks. Anything not listed is assumed to report every 20 minutes. */
const REPORT_MINUTES: Record<string, number> = { GS301: 10, WS202: 20, VS330: 10, WS303: 1080 };

/** Ages (ms) after which a LoRaWAN reading is stale, then offline: one missed uplink is stale, two are offline. */
export function lorawanWindows(profile: DeviceProfile | undefined): { fresh: number; stale: number } {
  const every = (REPORT_MINUTES[profile?.model ?? ""] ?? 20) * 60_000;
  return { fresh: every * 1.25 + 5 * 60_000, stale: every * 2 + 10 * 60_000 };
}

export type LoRaWANValue = { key: string; label: string; value: string; unit?: string; tone?: "warn" | "bad" };

const SENSOR_STATUS: Record<string, string> = { polarizing: "กำลังอุ่นเครื่อง", "device error": "เซนเซอร์ผิดพลาด" };
const GAS: [metric: string, label: string, digits: number][] = [["nh3", "NH₃", 2], ["h2s", "H₂S", 3]];
/** Values shown elsewhere on the card (main value, footer) or not meant for people. */
const SHOWN_ELSEWHERE = new Set(["temperature", "humidity", "battery", "motion", "leak", "rssi", "snr", "linkquality",
  // Stored raw before the backend read a UG65 codec's numeric states as daylight (milesight/state.go).
  "light_status", "pir", "occupancy"]);
const LABEL: Record<string, [label: string, unit?: string, digits?: number]> = {
  distance: ["ระยะถึงพื้น", "mm"], illuminance: ["แสง", "lx"], co2: ["CO₂", "ppm"], tvoc: ["TVOC", "ppb"], pm25: ["PM2.5", "µg/m³"], pm10: ["PM10", "µg/m³"],
  pressure: ["ความกดอากาศ", "hPa", 1],
};

const fmt = (v: number, digits = 0) => (Number.isInteger(v) && digits === 0 ? String(v) : v.toFixed(digits));

/** Everything a LoRaWAN reading carries beyond the card's main value, in a fixed order: gases (or their sensor's
 *  warm-up / fault status), daylight, then any other measurement by name. */
export function lorawanValues(r: Reading | null): LoRaWANValue[] {
  if (!r) return [];
  const m = (r.metrics ?? {}) as Record<string, number>, s = (r.values ?? {}) as Record<string, string>;
  const out: LoRaWANValue[] = [];
  for (const [key, label, digits] of GAS) {
    const status = s[`${key}_sensor_status`];
    if (status && SENSOR_STATUS[status]) out.push({ key, label, value: SENSOR_STATUS[status], tone: status === "device error" ? "bad" : "warn" });
    else if (m[key] != null) out.push({ key, label, value: fmt(m[key], digits), unit: "ppm" });
  }
  if (m.daylight != null) out.push({ key: "daylight", label: "แสง", value: m.daylight === 1 ? "สว่าง" : "มืด" });
  if (s.calibration_status) out.push({ key: "calibration_status", label: "การปรับเทียบ", value: s.calibration_status === "success" ? "สำเร็จ" : s.calibration_status, tone: s.calibration_status === "success" ? undefined : "warn" });
  for (const [key, v] of Object.entries(m)) {
    if (SHOWN_ELSEWHERE.has(key) || key === "daylight" || GAS.some(([g]) => g === key) || typeof v !== "number") continue;
    const [label, unit, digits] = LABEL[key] ?? [key, undefined, Number.isInteger(v) ? 0 : 2];
    out.push({ key, label, value: fmt(v, digits), unit });
  }
  return out;
}

/** Radio figures of the last uplink (the gateway's envelope carries them; a codec's output does not). */
export function lorawanRadio(r: Reading | null): LoRaWANValue[] {
  const m = (r?.metrics ?? {}) as Record<string, number>;
  return [
    ...(m.rssi != null ? [{ key: "rssi", label: "RSSI", value: fmt(m.rssi), unit: "dBm" }] : []),
    ...(m.snr != null ? [{ key: "snr", label: "SNR", value: fmt(m.snr, 1), unit: "dB" }] : []),
  ];
}

/** Gases with a history worth charting on this device. */
export const lorawanCharts = (history: Reading[]) => GAS.filter(([key]) => history.some((h) => h.metrics?.[key] != null)).map(([key, label, digits]) => ({ key, label, digits }));
