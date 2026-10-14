"use client";
// The 3D stage's on/off toggle: the same command flow as DeviceControlsPanel (topology/device-controls.tsx), cut down
// to one switch per device. A press queues a command per output and polls it until the device itself confirms;
// the shown state only changes when the device reports, so a timeout leaves it where it was and says so.
//
// State lives in a store owned by the stage, not in the label: labels come and go as the camera moves, and a
// command in flight must still show as pending when its label comes back.

import { useEffect, useSyncExternalStore } from "react";
import { Power } from "lucide-react";
import { ApiError, type Command, type DeviceControls } from "../../topology/api";
import type { CommandClient } from "../../topology/device-controls";
import { powerPlan } from "./outputs";

export type PowerPhase = "idle" | "pending" | "confirmed" | "failed";
export type PowerEntry = { controls: DeviceControls | null; phase: PowerPhase; message: string };

const SETTLED = new Set(["confirmed", "timeout", "expired", "failed"]);

/** Refusals the API gives for a command (ApiError.reason), as DeviceControlsPanel words them. */
const REASONS: Record<string, string> = {
  offline: "อุปกรณ์ออฟไลน์ สั่งงานไม่ได้ตอนนี้",
  in_flight: "มีคำสั่งค้างอยู่ รอผลก่อน",
  gateway_unavailable: "gateway ถูกเพิกถอน หรือไม่ใช่ gateway ที่สั่งงานได้",
  not_paired: "อุปกรณ์ไม่อยู่ในรายการของ Zigbee2MQTT แล้ว",
  rate_limited: "สั่งถี่เกินไป กรุณารอสักครู่",
  forbidden: "บัญชีนี้ไม่มีสิทธิ์สั่งงานอุปกรณ์",
};
const STATUS: Record<string, string> = { timeout: "อุปกรณ์ไม่ตอบกลับ · สถานะคงเดิม", expired: "คำสั่งหมดอายุก่อนส่ง · สถานะคงเดิม", failed: "ส่งคำสั่งไม่สำเร็จ · สถานะคงเดิม" };

const EMPTY: PowerEntry = { controls: null, phase: "idle", message: "" };

export class PowerStore {
  private entries = new Map<string, PowerEntry>();
  private loaded = new Map<string, unknown>();
  private seq = new Map<string, number>();
  private listeners = new Set<() => void>();
  private alive = true;

  constructor(private client: CommandClient) {}

  subscribe = (fn: () => void) => { this.listeners.add(fn); return () => { this.listeners.delete(fn); }; };
  get = (deviceId: string): PowerEntry => this.entries.get(deviceId) ?? EMPTY;
  /** The stage mounts (again: StrictMode remounts once in development) / unmounts. Unmounted, polls stop. */
  start() { this.alive = true; }
  stop() { this.alive = false; }

  private set(deviceId: string, patch: Partial<PowerEntry>) {
    if (!this.alive) return;
    this.entries.set(deviceId, { ...this.get(deviceId), ...patch });
    for (const fn of this.listeners) fn();
  }

  /** Loads the device's controls once per `refreshKey` (the board reloads them when the device reports, too). */
  ensure(deviceId: string, refreshKey: unknown) {
    if (this.loaded.has(deviceId) && this.loaded.get(deviceId) === refreshKey) return;
    this.loaded.set(deviceId, refreshKey);
    void this.reload(deviceId);
  }

  private async reload(deviceId: string) {
    try { this.set(deviceId, { controls: await this.client.deviceControls(deviceId) }); } catch { /* the label simply shows no switch */ }
  }

  async toggle(deviceId: string) {
    const entry = this.get(deviceId), plan = entry.controls ? powerPlan(entry.controls) : null;
    if (!plan || entry.phase === "pending") return;
    const turnOn = plan.on !== true;
    const sends = plan.kind === "power"
      ? plan.features.map((f) => ({ property: f.property, value: turnOn ? f.value_on : f.value_off }))
      : [{ property: plan.feature.property, value: turnOn ? "OPEN" : "CLOSE" }];
    this.seq.set(deviceId, (this.seq.get(deviceId) ?? 0) + 1);
    this.set(deviceId, { phase: "pending", message: "" });
    const queued: Command[] = [];
    for (const s of sends) {
      try {
        queued.push(await this.client.sendCommand(crypto.randomUUID(), { device_id: deviceId, ...s }));
      } catch (e) {
        const reason = e instanceof ApiError ? e.reason : undefined;
        const message = e instanceof ApiError && e.status === 403 ? REASONS.forbidden : (reason && REASONS[reason]) || (e instanceof Error ? e.message : "สั่งงานไม่สำเร็จ");
        if (!queued.length) { this.settle(deviceId, { phase: "failed", message }); return; }
        break;
      }
    }
    // Poll every queued command (a few seconds at most) until the device settles it.
    const final = new Map<string, Command>(queued.map((c) => [c.id, c]));
    for (let i = 0; i < 20 && this.alive && [...final.values()].some((c) => !SETTLED.has(c.status)); i++) {
      await new Promise((r) => setTimeout(r, 1000));
      for (const c of [...final.values()]) {
        if (SETTLED.has(c.status)) continue;
        try { final.set(c.id, await this.client.command(c.id)); } catch { /* keep polling */ }
      }
    }
    const bad = [...final.values()].find((c) => c.status !== "confirmed");
    const partial = queued.length < sends.length;
    await this.reload(deviceId);
    this.settle(deviceId, bad || partial
      ? { phase: "failed", message: bad ? (SETTLED.has(bad.status) ? STATUS[bad.status] ?? bad.status : "ยังไม่ได้รับยืนยันจากอุปกรณ์") : "ส่งคำสั่งได้ไม่ครบทุกช่อง" }
      : { phase: "confirmed", message: "" });
  }

  /** Shows the outcome, then returns to the plain switch (a failure stays readable for longer). */
  private settle(deviceId: string, outcome: { phase: "confirmed" | "failed"; message: string }) {
    this.set(deviceId, outcome);
    const seq = (this.seq.get(deviceId) ?? 0) + 1;
    this.seq.set(deviceId, seq);
    setTimeout(() => { if (this.seq.get(deviceId) === seq && this.get(deviceId).phase !== "pending") this.set(deviceId, { phase: "idle", message: "" }); }, outcome.phase === "failed" ? 8000 : 3000);
  }
}

/** The label's switch. Read-only accounts see the state without a button; devices without an output see nothing. */
export function PowerToggle({ store, deviceId, name, refreshKey }: { store: PowerStore; deviceId: string; name: string; refreshKey: unknown }) {
  const entry = useSyncExternalStore(store.subscribe, () => store.get(deviceId), () => EMPTY);
  useEffect(() => { store.ensure(deviceId, refreshKey); }, [store, deviceId, refreshKey]);
  const plan = entry.controls ? powerPlan(entry.controls) : null;
  if (!entry.controls || !plan) return null;
  const word = (on: boolean) => (plan.kind === "cover" ? (on ? "เปิดอยู่" : "ปิดอยู่") : on ? "เปิด" : "ปิด");
  const stateText = plan.on === null ? "—" : word(plan.on);
  if (!entry.controls.can_command) return <span className={`ov3d-power is-readonly ${plan.on ? "is-on" : ""}`} title="บัญชีนี้ดูสถานะได้อย่างเดียว"><Power size={12} aria-hidden="true" />{stateText}</span>;
  const pending = entry.phase === "pending";
  const offline = !entry.controls.online;
  const action = plan.on === true ? (plan.kind === "cover" ? "ปิดม่าน" : "ปิด") : plan.kind === "cover" ? "เปิดม่าน" : "เปิด";
  return (
    <span className="ov3d-power-wrap">
      {/* A known state is a switch named by its device (checked = on); an unknown one is a plain button named by what it does. */}
      <button type="button" {...(plan.on === null ? {} : { role: "switch", "aria-checked": plan.on })} aria-busy={pending} disabled={pending || offline}
        className={`ov3d-power ${plan.on ? "is-on" : ""} ${pending ? "is-pending" : ""} is-${entry.phase}`}
        aria-label={plan.on === null ? `${action} ${name}` : name}
        title={offline ? "อุปกรณ์ออฟไลน์ · สั่งงานได้เมื่อกลับมาออนไลน์" : pending ? "กำลังสั่ง… รออุปกรณ์ยืนยัน" : `${stateText} · กดเพื่อ${action}`}
        onClick={(e) => { e.stopPropagation(); void store.toggle(deviceId); }}>
        <Power size={12} aria-hidden="true" />{pending ? "กำลังสั่ง…" : entry.phase === "confirmed" ? "ยืนยันแล้ว" : stateText}
      </button>
      {entry.phase === "failed" && entry.message && <small className="ov3d-power-error" role="alert">{entry.message}</small>}
    </span>
  );
}
