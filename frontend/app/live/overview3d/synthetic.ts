// A synthetic site for the stage's scale check (the self-test, and the dev-only `?ov3d-demo=500` toggle).
// Deterministic (seeded), so a screenshot of it is comparable run to run. Never shown outside a dev build.
// Readings are realistic per kind (switch gangs, door state, leak, cover position, battery…), so the board's own
// value rendering shows on the labels what it would show on a card.

import type { StageCard, StageGateway, StageProject } from "./types";

/** The fields of the API's Reading (topology/api.ts) the board's value() reads; kept local so the self-test
 *  compiles without the API client. */
type Reading = NonNullable<StageCard["reading"]> & { kind?: string; temperature: number; humidity: number; battery: number; rssi: number | null; values?: Record<string, string> };

/** The clock the synthetic readings are stamped against (pass it as `now` when rendering their values). */
export const SYNTH_NOW = Date.parse("2026-10-14T09:00:00Z");

const KINDS = ["environment", "environment", "occupancy", "door", "switch", "lighting", "metering", "sos", "beacon", "hazard", "leak", "cover"];

export type SyntheticCard = StageCard & {
  reading: Reading | null;
  door?: { open: boolean | null; since: string | null; sinceExact: boolean; leftOpen: boolean };
  wearable: boolean;
  zone: string | null;
};

export function syntheticSite(devices: number, gatewayCount = Math.max(1, Math.round(devices / 18))): { cards: SyntheticCard[]; gateways: StageGateway[]; projects: StageProject[] } {
  let seed = 7;
  const rnd = () => { seed = (seed * 16807) % 2147483647; return (seed - 1) / 2147483646; };
  const between = (lo: number, hi: number, digits = 0) => Number((lo + rnd() * (hi - lo)).toFixed(digits));
  const projects: StageProject[] = [{ id: "demo-a", name: "อาคาร A (จำลอง)", color: "mint" }, { id: "demo-b", name: "อาคาร B (จำลอง)", color: "blue" }, { id: "demo-c", name: "คลังสินค้า (จำลอง)", color: "violet" }];
  const gateways: StageGateway[] = Array.from({ length: gatewayCount }, (_, i) => ({ id: `demo-gw-${i}`, name: `DEMO GW ${String(i + 1).padStart(2, "0")}`, projectId: i % 7 === 6 ? null : projects[i % 3].id, online: rnd() > 0.12 }));
  const iso = (minutesAgo: number) => new Date(SYNTH_NOW - minutesAgo * 60000).toISOString();
  const cards: SyntheticCard[] = Array.from({ length: devices }, (_, i) => {
    const g = gateways[Math.floor(rnd() * gateways.length)], kind = KINDS[Math.floor(rnd() * KINDS.length)], roll = rnd();
    let status: StageCard["status"] = !g.online || roll < 0.08 ? "offline" : roll < 0.14 ? "stale" : roll < 0.18 ? "alert" : "online";
    const reasons: string[] = [];
    // Per-kind measurements, in the shape the API delivers them (Zigbee2MQTT values live in metrics/values).
    const temperature = between(21, 31, 1), humidity = between(38, 78), battery = between(35, 100);
    let metrics: Record<string, number> = {}, values: Record<string, string> | undefined;
    if (kind === "environment" || kind === "occupancy") metrics = { temperature, humidity };
    if (kind === "occupancy") metrics.motion = rnd() > 0.5 ? 1 : 0;
    if (kind === "switch") { const gangs = rnd() > 0.6 ? 3 : 2; for (let k = 1; k <= gangs; k++) metrics[`sw${k}`] = rnd() > 0.5 ? 1 : 0; metrics.linkquality = between(60, 200); }
    if (kind === "lighting") metrics = { state: rnd() > 0.5 ? 1 : 0, brightness: between(20, 254) };
    if (kind === "metering") metrics = { power: between(0, 900), energy: between(0, 40, 2) };
    if (kind === "cover") { metrics = { position: [0, 35, 100][Math.floor(rnd() * 3)] }; values = { state: metrics.position > 0 ? "OPEN" : "CLOSE" }; }
    if (kind === "leak") { metrics = { leak: status === "alert" ? 1 : 0 }; if (metrics.leak) reasons.push("พบน้ำรั่ว"); }
    if (kind === "hazard") { metrics = { smoke: status === "alert" ? 1 : 0, gas: 0 }; if (metrics.smoke) reasons.push("ตรวจพบควัน"); }
    // Only kinds that can raise one stay in alert; the rest fall back to online.
    if (status === "alert" && !["leak", "hazard", "environment", "sos"].includes(kind)) status = "online";
    if (status === "alert" && kind === "environment") reasons.push("อุณหภูมิเกิน 28 °C");
    const sos = kind === "sos" && status !== "offline" && rnd() < 0.08;
    if (sos) { status = "alert"; reasons.unshift("กดปุ่มฉุกเฉิน · ยังไม่รับทราบ"); }
    if (kind === "sos" && status === "alert" && !sos) status = "online";
    const ageMin = status === "stale" ? between(2, 12) : between(0, 0.9, 2);
    const reading: Reading | null = status === "offline" ? null : {
      received_at: iso(ageMin), kind: kind === "occupancy" || kind === "environment" ? "environment" : kind, temperature: metrics.temperature ?? NaN, humidity: metrics.humidity ?? NaN,
      battery: kind === "switch" || kind === "lighting" || kind === "metering" || kind === "cover" ? 0 : battery, rssi: between(-90, -45), metrics, values,
    };
    const doorOpen = kind === "door" ? rnd() > 0.55 : false, leftOpen = doorOpen && rnd() > 0.5;
    const ext = `demo${String(i).padStart(4, "0")}`;
    return {
      key: ext, external: ext, name: `DEMO ${kind} ${i + 1}`, kind, model: "จำลอง", registered: true, gatewayId: g.id, gatewayName: g.name, projectId: g.projectId,
      status, sos, reasons, reading, wearable: false, zone: null,
      occupancy: kind === "occupancy" ? { occupied: metrics.motion === 1 } : undefined,
      door: kind === "door" ? { open: status === "offline" ? null : doorOpen, since: status === "offline" ? null : iso(leftOpen ? between(12, 90) : between(1, 9)), sinceExact: true, leftOpen: status !== "offline" && leftOpen } : undefined,
    };
  });
  return { cards, gateways, projects };
}
