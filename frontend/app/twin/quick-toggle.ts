// The in-scene power button (immersive view): the same command flow as the side card's controls
// (topology/device-controls.tsx): read what the device lets Aether set, queue a command per output with a fresh
// idempotency key, then poll until the device's own report settles it. The shown state never changes before that.

import { ApiError, type Command, type ControlFeature } from "../topology/api";
import type { CommandClient } from "../topology/device-controls";

export type QuickStatus = "pending" | "sent" | "confirmed" | "failed";
export type QuickResult = { status: QuickStatus; message?: string };

const SETTLED = new Set(["confirmed", "timeout", "expired", "failed"]);

const REASONS: Record<string, string> = {
  offline: "อุปกรณ์ออฟไลน์ สั่งงานไม่ได้ตอนนี้",
  in_flight: "มีคำสั่งค้างอยู่ รอผลก่อน",
  state_unknown: "ยังไม่รู้สถานะปัจจุบัน",
  gateway_unavailable: "gateway สั่งงานไม่ได้",
  not_paired: "อุปกรณ์ไม่อยู่ใน Zigbee2MQTT แล้ว",
  rate_limited: "สั่งถี่เกินไป รอสักครู่",
  forbidden: "บัญชีนี้ไม่มีสิทธิ์สั่งงานอุปกรณ์",
};

/** Why a queued command failed (Command.error), as the side card says it (topology/device-controls.tsx). */
const FAILURES: Record<string, string> = {
  link_unavailable: "การเชื่อม Tuya Cloud ไม่พร้อม",
  cloud_link_down: "การเชื่อม Tuya Cloud ขาด",
  quota_near: "โควตา Tuya เดือนนี้ใกล้หมด",
  quota: "โควตา Tuya เดือนนี้หมด",
  auth_failed: "Tuya ปฏิเสธ Access ID/Secret",
  device_offline: "อุปกรณ์ไม่ได้เชื่อมกับ Tuya Cloud",
  cloud_unreachable: "เชื่อมต่อ Tuya Cloud ไม่ได้",
  rejected: "Tuya ปฏิเสธคำสั่งนี้",
};

const SETTLE_TEXT: Record<string, string> = {
  timeout: "อุปกรณ์ไม่ตอบกลับ · สถานะคงเดิม",
  expired: "คำสั่งหมดอายุ · สถานะคงเดิม",
  failed: "ส่งคำสั่งไม่สำเร็จ · สถานะคงเดิม",
};

/** Outputs only (a gang, a plug, a lamp), as the compact controls pick them. */
export const isOutput = (f: ControlFeature) => f.type === "binary" && (f.name === "state" || /^(state|switch)(_|$)/.test(f.property));

/** Thai text for a command refused before it was queued. */
export function commandError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.reason && REASONS[e.reason]) return REASONS[e.reason];
    if (e.status === 403) return REASONS.forbidden;
    if (e.status === 404) return "ไม่พบอุปกรณ์นี้ หรือสั่งงานไม่ได้";
    if (e.status === 429) return REASONS.rate_limited;
  }
  return "สั่งงานไม่สำเร็จ";
}

/**
 * Switches every output of a device to `on`. Reports "sent" once queued and the final state when the device
 * confirms (or not). Never throws: a refusal comes back as a failed result with a Thai message.
 */
export async function quickToggle(client: CommandClient, deviceId: string, on: boolean, report: (r: QuickResult) => void, alive: () => boolean = () => true): Promise<QuickResult> {
  const done = (r: QuickResult) => { if (alive()) report(r); return r; };
  report({ status: "pending" });
  let controls;
  try {
    controls = await client.deviceControls(deviceId);
  } catch (e) {
    return done({ status: "failed", message: commandError(e) });
  }
  if (!controls.can_command) return done({ status: "failed", message: "บัญชีนี้ดูสถานะได้อย่างเดียว" });
  if (!controls.online) return done({ status: "failed", message: REASONS.offline });
  const outputs = controls.features.filter(isOutput);
  if (!outputs.length) return done({ status: "failed", message: "อุปกรณ์นี้ไม่มีสวิตช์เปิด/ปิด · ใช้การ์ดด้านข้าง" });
  const todo = outputs.filter((f) => controls.state[f.property] !== (on ? f.value_on : f.value_off));
  if (!todo.length) return done({ status: "confirmed" });
  const queued: Command[] = [];
  let refused: unknown = null;
  for (const f of todo) {
    try {
      queued.push(await client.sendCommand(crypto.randomUUID(), { device_id: deviceId, property: f.property, value: on ? f.value_on : f.value_off }));
    } catch (e) {
      if (!queued.length) return done({ status: "failed", message: commandError(e) });
      refused = e;
      break;
    }
  }
  if (alive()) report({ status: "sent" });
  // The device's own report settles each command; poll for 20 s at most, as the side card does.
  const final = new Map<string, Command>();
  for (let i = 0; i < 20 && alive() && final.size < queued.length; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    for (const c of queued) {
      if (final.has(c.id)) continue;
      try {
        const now = await client.command(c.id);
        if (SETTLED.has(now.status)) final.set(c.id, now);
      } catch {
        // keep polling; the button stays pending
      }
    }
  }
  const bad = [...final.values()].find((c) => c.status !== "confirmed");
  if (bad) {
    const why = bad.error ? FAILURES[bad.error] ?? bad.error : "";
    return done({ status: "failed", message: `${SETTLE_TEXT[bad.status] ?? SETTLE_TEXT.failed}${why ? ` · ${why}` : ""}` });
  }
  // A later output refused before it was queued says why, not that the device stayed silent.
  if (refused) return done({ status: "failed", message: `บางช่องไม่ได้สั่ง: ${commandError(refused)}` });
  if (final.size < queued.length) return done({ status: "failed", message: SETTLE_TEXT.timeout });
  return done({ status: "confirmed" });
}
