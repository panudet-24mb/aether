import type { Reading } from "../topology/api";

/** Decoder name of a Zigbee2MQTT state message (backend zigbee2mqtt.FrameState). */
const ZIGBEE_FRAME = "z2m-state@1";

export function isZigbeeReading(r: Reading): boolean {
  return !!r.frames?.includes(ZIGBEE_FRAME);
}

/**
 * A Zigbee2MQTT reading carries every measurement in `metrics` and only the ones that message reported; its
 * fixed temperature / humidity fields are zero. Give it the same shape as a BLE reading (NaN when absent) so
 * charts, cards and the floor plan read it like any other.
 */
export function normalizeReading(r: Reading): Reading {
  if (!isZigbeeReading(r)) return r;
  const m = r.metrics ?? {};
  return { ...r, temperature: m.temperature ?? NaN, humidity: m.humidity ?? NaN };
}

export function normalizeSensor<T extends { latest: Reading; history: Reading[] }>(s: T): T {
  return { ...s, latest: normalizeReading(s.latest), history: s.history.map(normalizeReading) };
}

// A BLE tag alternates advertising frames. Missing measurements are not zero.
export function environmentReading(r: Reading): boolean {
  if (isZigbeeReading(r)) return r.metrics?.temperature != null;
  return r.frames?.includes("minew-ffe1-a101@1") || r.kind === "environment" || (!r.kind && !r.frames?.length);
}
export function latestMeasurements(sensor: { latest: Reading; history: Reading[] }): Reading {
  const rows = [...sensor.history, sensor.latest].sort((a, b) => Date.parse(a.received_at) - Date.parse(b.received_at));
  const out: Reading = { ...sensor.latest, temperature: NaN, humidity: NaN, battery: NaN, metrics: {}, values: {} };
  for (const r of rows) {
    if (isZigbeeReading(r)) {
      // Each message may report a different subset: keep the last value of each.
      if (r.metrics?.temperature != null) out.temperature = r.metrics.temperature;
      if (r.metrics?.humidity != null) out.humidity = r.metrics.humidity;
    } else if (environmentReading(r)) { out.temperature = r.temperature; out.humidity = r.humidity; }
    // A Zigbee reading reports battery as a metric too, so 0 % is a value; for BLE 0 means "not in this frame".
    if (isZigbeeReading(r) && r.metrics?.battery != null) out.battery = r.metrics.battery;
    else if (r.battery > 0) out.battery = r.battery;
    Object.assign(out.metrics!, r.metrics ?? {});
    Object.assign(out.values!, r.values ?? {});
  }
  return out;
}
