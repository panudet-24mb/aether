import type { Reading } from "../topology/api";

// A BLE tag alternates advertising frames. Missing measurements are not zero.
export function environmentReading(r: Reading): boolean {
  return r.frames?.includes("minew-ffe1-a101@1") || r.kind === "environment" || (!r.kind && !r.frames?.length);
}
export function latestMeasurements(sensor: { latest: Reading; history: Reading[] }): Reading {
  const rows = [...sensor.history, sensor.latest].sort((a, b) => Date.parse(a.received_at) - Date.parse(b.received_at));
  const out: Reading = { ...sensor.latest, temperature: NaN, humidity: NaN, battery: NaN, metrics: {} };
  for (const r of rows) {
    if (environmentReading(r)) { out.temperature = r.temperature; out.humidity = r.humidity; }
    if (r.battery > 0) out.battery = r.battery;
    Object.assign(out.metrics!, r.metrics ?? {});
  }
  return out;
}
