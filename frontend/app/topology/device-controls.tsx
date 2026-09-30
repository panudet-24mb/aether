"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, type Command, type ControlFeature, type DeviceControls } from "./api";

/** The slice of the API client the controls need (topology, overview and inspector all share one client). */
export type CommandClient = {
  deviceControls: (id: string) => Promise<DeviceControls>;
  sendCommand: (key: string, input: { device_id: string; property: string; value?: unknown; action?: "set" | "toggle" }) => Promise<Command>;
  command: (id: string) => Promise<Command>;
};

type Pending = { id: string; status: Command["status"]; error?: string };

const SETTLED = new Set(["confirmed", "timeout", "expired", "failed"]);

const LABELS: Record<string, string> = {
  state: "เปิด / ปิด", brightness: "ความสว่าง", color_temp: "อุณหภูมิสี", color: "สี", position: "ตำแหน่ง", tilt: "มุมใบม่าน",
  system_mode: "โหมด", temp_set: "อุณหภูมิที่ตั้ง", occupied_heating_setpoint: "อุณหภูมิที่ตั้ง", current_heating_setpoint: "อุณหภูมิที่ตั้ง", child_lock: "ล็อกกันเด็ก",
  fan_mode: "ความเร็วพัดลม", effect: "เอฟเฟกต์", preset: "โหมดตั้งล่วงหน้า",
};

const REASONS: Record<string, string> = {
  offline: "อุปกรณ์ออฟไลน์ สั่งงานไม่ได้ตอนนี้",
  in_flight: "มีคำสั่งค่านี้ค้างอยู่ รอผลก่อน",
  state_unknown: "ยังไม่รู้สถานะปัจจุบัน ให้สั่ง เปิด หรือ ปิด แทน",
  value: "ค่านี้อยู่นอกช่วงที่อุปกรณ์รับได้",
  not_settable: "อุปกรณ์ไม่ให้ตั้งค่านี้",
  unknown_property: "อุปกรณ์ไม่มีค่านี้",
  not_toggleable: "ค่านี้สลับเปิด/ปิดไม่ได้",
  gateway_unavailable: "gateway ถูกเพิกถอน หรือไม่ใช่ gateway ที่สั่งงานได้",
  not_paired: "อุปกรณ์ไม่อยู่ในรายการของ Zigbee2MQTT แล้ว",
  rate_limited: "สั่งถี่เกินไป กรุณารอสักครู่",
  forbidden: "บัญชีนี้ไม่มีสิทธิ์สั่งงานอุปกรณ์",
};

/** Why a queued command failed (Command.error); the Tuya Cloud worker sets these (backend tuyacloudlink sender). */
const FAILURES: Record<string, string> = {
  link_unavailable: "การเชื่อม Tuya Cloud ไม่พร้อม (ขาด ยกเลิก หรือกำลังเชื่อมใหม่)",
  cloud_link_down: "การเชื่อม Tuya Cloud ขาด",
  quota_near: "โควตา Tuya เดือนนี้ใกล้หมด · งดคำสั่งเพื่อเก็บไว้รับสถานะ",
  quota: "โควตา Tuya เดือนนี้หมด · สั่งงานผ่าน Tuya Cloud ไม่ได้จนถึงเดือนหน้า",
  auth_failed: "Tuya ปฏิเสธ Access ID/Secret · owner/admin ต้องเปลี่ยน Access Secret",
  device_offline: "อุปกรณ์ไม่ได้เชื่อมกับ Tuya Cloud",
  cloud_unreachable: "เชื่อมต่อ Tuya Cloud ไม่ได้",
  rejected: "Tuya ปฏิเสธคำสั่งนี้",
};

const STATUS: Record<string, string> = {
  pending: "กำลังส่ง…", sent: "ส่งแล้ว · รออุปกรณ์ยืนยัน…", confirmed: "อุปกรณ์ยืนยันแล้ว",
  timeout: "อุปกรณ์ไม่ตอบกลับ (ไม่ได้รับยืนยัน)", expired: "คำสั่งหมดอายุก่อนส่ง", failed: "ส่งคำสั่งไม่สำเร็จ",
};

function label(f: ControlFeature): string {
  const base = LABELS[f.name ?? ""] ?? f.label ?? f.name ?? f.property;
  return f.endpoint && f.group !== "switch" ? `${base} (${f.endpoint})` : base;
}

function show(v: unknown): string {
  if (v === undefined || v === null) return "—";
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}

function endpointRank(ep = ""): number {
  if (!ep) return 0;
  const named: Record<string, number> = { left: 1, center: 2, right: 3 };
  if (ep in named) return named[ep];
  const n = Number(ep.replace(/^l/, ""));
  return Number.isFinite(n) && ep.startsWith("l") ? n : 100;
}

/** #rrggbb -> hue (0–360) and saturation (0–100), the color_hs composite. */
function hexToHS(hex: string): { hue: number; saturation: number } {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
  const max = Math.max(r, g, b), min = Math.min(r, g, b), d = max - min;
  let h = 0;
  if (d) h = max === r ? ((g - b) / d) % 6 : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
  return { hue: Math.round(((h * 60) + 360) % 360), saturation: Math.round(max ? (d / max) * 100 : 0) };
}

/** #rrggbb -> CIE xy (sRGB, D65), the color_xy composite Zigbee lights use. */
function hexToXY(hex: string): { x: number; y: number } {
  const lin = (c: number) => (c > 0.04045 ? ((c + 0.055) / 1.055) ** 2.4 : c / 12.92);
  const [r, g, b] = [1, 3, 5].map((i) => lin(parseInt(hex.slice(i, i + 2), 16) / 255));
  const X = r * 0.4124 + g * 0.3576 + b * 0.1805, Y = r * 0.2126 + g * 0.7152 + b * 0.0722, Z = r * 0.0193 + g * 0.1192 + b * 0.9505;
  const sum = X + Y + Z || 1;
  return { x: Math.round((X / sum) * 10000) / 10000, y: Math.round((Y / sum) * 10000) / 10000 };
}

/**
 * Controls for a registered Zigbee2MQTT device, rendered from what its own definition says is settable. A click
 * queues a command; the shown value only changes when the device reports it (the command becomes "confirmed").
 */
/** Settable values the compact controls show (the rest stay in the device inspector). */
const QUICK_NUMERIC = new Set(["brightness", "position", "current_heating_setpoint", "occupied_heating_setpoint", "temp_set"]);

export default function DeviceControlsPanel({ client, deviceId, refreshKey, variant = "full", onSettled }: {
  client: CommandClient; deviceId: string; refreshKey?: unknown;
  /** compact: on/off toggles per gang, open/stop/close, and one slider for brightness, position or setpoint, for a
   *  tile, the twin's side card or a Studio controls panel. full: every settable value (the device inspector). */
  variant?: "full" | "compact";
  /** Called when a command settles (confirmed, timed out or failed), so the caller can refresh what it shows. */
  onSettled?: () => void;
}) {
  const [controls, setControls] = useState<DeviceControls | null>(null);
  const [missing, setMissing] = useState(false);
  const [pending, setPending] = useState<Record<string, Pending>>({});
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const [error, setError] = useState("");
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; }, []);

  const load = useCallback(async () => {
    try {
      const c = await client.deviceControls(deviceId);
      if (alive.current) { setControls(c); setMissing(false); }
    } catch (e) {
      if (alive.current && e instanceof ApiError && e.status === 404) setMissing(true);
    }
  }, [client, deviceId]);
  // Reload when the device reports (refreshKey changes); results land in a callback, never synchronously.
  useEffect(() => {
    let active = true;
    client.deviceControls(deviceId).then(
      (c) => { if (active) { setControls(c); setMissing(false); } },
      (e: unknown) => { if (active && e instanceof ApiError && e.status === 404) setMissing(true); },
    );
    return () => { active = false; };
  }, [client, deviceId, refreshKey]);

  async function send(property: string, input: { value?: unknown; action?: "set" | "toggle" }) {
    setError("");
    const key = crypto.randomUUID();
    let cmd: Command;
    try {
      cmd = await client.sendCommand(key, { device_id: deviceId, property, ...input });
    } catch (e) {
      const reason = e instanceof ApiError ? e.reason : undefined;
      setError((reason && REASONS[reason]) || (e instanceof Error ? e.message : "สั่งงานไม่สำเร็จ"));
      return;
    }
    setPending((p) => ({ ...p, [property]: { id: cmd.id, status: cmd.status } }));
    // The device's own report settles the command; poll it (a few seconds at most) and then refresh the values.
    for (let i = 0; i < 20 && alive.current; i++) {
      await new Promise((r) => setTimeout(r, 1000));
      try {
        const now = await client.command(cmd.id);
        if (!alive.current) return;
        setPending((p) => ({ ...p, [property]: { id: now.id, status: now.status, error: now.error } }));
        if (SETTLED.has(now.status)) break;
      } catch {
        // keep polling; the status chip stays as it was
      }
    }
    void load();
    onSettled?.();
  }

  if (missing || !controls) return null;
  const busy = (property: string) => !!pending[property] && !SETTLED.has(pending[property].status);
  const disabled = (property: string) => !controls.can_command || !controls.online || busy(property);
  const chip = (property: string) => {
    const p = pending[property];
    if (!p) return null;
    return <small className={`topo-cmd-status is-${p.status}`}>{STATUS[p.status] ?? p.status}{p.error ? ` · ${FAILURES[p.error] ?? p.error}` : ""}</small>;
  };
  const gangs = controls.features.filter((f) => f.group === "switch" && f.type === "binary").sort((a, b) => endpointRank(a.endpoint) - endpointRank(b.endpoint));
  const others = controls.features.filter((f) => !gangs.includes(f));
  const seenComposite = new Set<string>();
  if (variant === "compact") return <CompactControls controls={controls} gangs={gangs} others={others} pending={pending} draft={draft} setDraft={setDraft} error={error} disabled={disabled} busy={busy} send={send} />;

  return (
    <section className="topo-controls" aria-label="สั่งงานอุปกรณ์">
      <h3 className="topo-h3">สั่งงาน</h3>
      {!controls.can_command && <p className="topo-note">บัญชีนี้ดูสถานะได้อย่างเดียว</p>}
      {controls.can_command && !controls.online && <p className="topo-note">อุปกรณ์ออฟไลน์ · สั่งงานได้เมื่อกลับมาออนไลน์</p>}
      {gangs.length > 0 && (
        <div className="topo-gang-controls">
          {gangs.map((f, i) => {
            const on = controls.state[f.property] === f.value_on;
            return (
              <div key={f.property} className="topo-gang-control">
                <span>ช่อง {i + 1} · <strong>{controls.state[f.property] === undefined ? "—" : on ? "เปิด" : "ปิด"}</strong></span>
                <div className="topo-actions">
                  <button type="button" className="topo-btn" disabled={disabled(f.property)} onClick={() => void send(f.property, { value: f.value_on })}>เปิด</button>
                  <button type="button" className="topo-btn" disabled={disabled(f.property)} onClick={() => void send(f.property, { value: f.value_off })}>ปิด</button>
                </div>
                {chip(f.property)}
              </div>
            );
          })}
        </div>
      )}
      {others.map((f) => {
        const current = controls.state[f.property];
        if (f.type === "binary") {
          return (
            <div key={f.property} className="topo-control">
              <span>{label(f)} · <strong>{show(current)}</strong></span>
              <div className="topo-actions">
                <button type="button" className="topo-btn" disabled={disabled(f.property)} onClick={() => void send(f.property, { value: f.value_on })}>{show(f.value_on)}</button>
                <button type="button" className="topo-btn" disabled={disabled(f.property)} onClick={() => void send(f.property, { value: f.value_off })}>{show(f.value_off)}</button>
              </div>
              {chip(f.property)}
            </div>
          );
        }
        if (f.type === "numeric") {
          const min = f.value_min ?? 0, max = f.value_max ?? Math.max(100, Number(current) || 0), step = f.value_step ?? 1;
          const value = Number(draft[f.property] ?? current ?? min);
          return (
            <div key={f.property} className="topo-control">
              <span>{label(f)} · <strong>{show(current)}{f.unit ? ` ${f.unit}` : ""}</strong></span>
              <div className="topo-actions">
                <input type="range" min={min} max={max} step={step} value={value} disabled={disabled(f.property)} aria-label={label(f)} onChange={(e) => setDraft((d) => ({ ...d, [f.property]: Number(e.target.value) }))} />
                <input type="number" min={f.value_min} max={f.value_max} step={step} value={value} disabled={disabled(f.property)} aria-label={`${label(f)} (ตัวเลข)`} onChange={(e) => setDraft((d) => ({ ...d, [f.property]: Number(e.target.value) }))} />
                <button type="button" className="topo-btn" disabled={disabled(f.property)} onClick={() => void send(f.property, { value })}>ตั้งค่า</button>
              </div>
              {chip(f.property)}
            </div>
          );
        }
        if (f.type === "enum") {
          const values = f.values ?? [];
          const value = draft[f.property] ?? current ?? values[0];
          return (
            <div key={f.property} className="topo-control">
              <span>{label(f)} · <strong>{show(current)}</strong></span>
              <div className="topo-actions">
                <select value={show(value)} disabled={disabled(f.property)} aria-label={label(f)} onChange={(e) => setDraft((d) => ({ ...d, [f.property]: values.find((v) => show(v) === e.target.value) }))}>
                  {values.map((v) => <option key={show(v)} value={show(v)}>{show(v)}</option>)}
                </select>
                <button type="button" className="topo-btn" disabled={disabled(f.property)} onClick={() => void send(f.property, { value })}>ตั้งค่า</button>
              </div>
              {chip(f.property)}
            </div>
          );
        }
        // Composite: a colour picker for color_hs / color_xy (one picker per property, preferring hue/saturation).
        if (seenComposite.has(f.property)) return null;
        seenComposite.add(f.property);
        const parts = (g: ControlFeature) => new Set((g.features ?? []).map((x) => x.property));
        const hs = controls.features.find((g) => g.property === f.property && g.type === "composite" && parts(g).has("hue") && parts(g).has("saturation"));
        const xy = controls.features.find((g) => g.property === f.property && g.type === "composite" && parts(g).has("x") && parts(g).has("y"));
        if (!hs && !xy) {
          return <p key={f.property} className="topo-note">{label(f)} · ยังตั้งค่าจากหน้านี้ไม่ได้</p>;
        }
        const hex = String(draft[f.property] ?? "#ffffff");
        return (
          <div key={f.property} className="topo-control">
            <span>{label(f)} · <strong>{show(current)}</strong></span>
            <div className="topo-actions">
              <input type="color" value={hex} disabled={disabled(f.property)} aria-label={label(f)} onChange={(e) => setDraft((d) => ({ ...d, [f.property]: e.target.value }))} />
              <button type="button" className="topo-btn" disabled={disabled(f.property)} onClick={() => void send(f.property, { value: hs ? hexToHS(hex) : hexToXY(hex) })}>ตั้งค่า</button>
            </div>
            {chip(f.property)}
          </div>
        );
      })}
      {controls.features.length === 0 && <p className="topo-note">อุปกรณ์นี้ไม่มีค่าที่ตั้งได้</p>}
      {error && <p className="topo-error" role="alert">{error}</p>}
    </section>
  );
}

/**
 * The compact form: a switch per output (role="switch", pressed = on). A pressed switch shows "กำลังสั่ง…" until the
 * device itself reports the new value; the shown state never changes before that, so a timeout leaves it where it
 * was and says so.
 */
function CompactControls({ controls, gangs, others, pending, draft, setDraft, error, disabled, busy, send }: {
  controls: DeviceControls; gangs: ControlFeature[]; others: ControlFeature[]; pending: Record<string, Pending>;
  draft: Record<string, unknown>; setDraft: (f: (d: Record<string, unknown>) => Record<string, unknown>) => void; error: string;
  disabled: (property: string) => boolean; busy: (property: string) => boolean;
  send: (property: string, input: { value?: unknown; action?: "set" | "toggle" }) => Promise<void>;
}) {
  // Outputs only (a gang, a plug, a lamp): a thermostat's window or valve detection is a setting, not a switch.
  const output = (f: ControlFeature) => f.type === "binary" && (f.name === "state" || /^(state|switch)(_|$)/.test(f.property));
  const outputs = [...gangs.filter(output), ...others.filter(output)];
  const cover = others.find((f) => f.type === "enum" && f.name === "state" && (f.values ?? []).includes("OPEN"));
  const numeric = others.filter((f) => f.type === "numeric" && QUICK_NUMERIC.has(f.name ?? f.property)).slice(0, 2);
  const failed = Object.entries(pending).filter(([, p]) => p.status === "timeout" || p.status === "failed" || p.status === "expired");
  const readOnly = !controls.can_command;
  if (!outputs.length && !cover && !numeric.length) return null;
  return (
    <div className="dc-compact" aria-label="สั่งงานอุปกรณ์">
      {outputs.length > 0 && <div className="dc-switches">{outputs.map((f, i) => {
        const on = controls.state[f.property] === f.value_on;
        const known = controls.state[f.property] !== undefined;
        const name = outputs.length === 1 ? "เปิด / ปิด" : `ช่อง ${i + 1}`;
        if (readOnly) return <span key={f.property} className={`dc-state ${on ? "is-on" : ""}`}>{name} · {known ? (on ? "เปิด" : "ปิด") : "—"}</span>;
        return (
          <button key={f.property} type="button" role="switch" aria-checked={known ? on : undefined} aria-busy={busy(f.property)} disabled={disabled(f.property)}
            className={`dc-switch ${on ? "is-on" : ""} ${busy(f.property) ? "is-pending" : ""}`}
            title={`${name}: ${busy(f.property) ? "กำลังสั่ง… รออุปกรณ์ยืนยัน" : on ? "เปิดอยู่ · กดเพื่อปิด" : "ปิดอยู่ · กดเพื่อเปิด"}`}
            onClick={() => void send(f.property, { value: on ? f.value_off : f.value_on })}>
            <span className="dc-knob" aria-hidden="true" /><span className="dc-label">{name}</span><small>{busy(f.property) ? "กำลังสั่ง…" : known ? (on ? "เปิด" : "ปิด") : "—"}</small>
          </button>
        );
      })}</div>}
      {cover && <div className="dc-row"><span>ม่าน · <strong>{show(controls.state[cover.property])}</strong></span>{!readOnly && <div className="dc-buttons">{(["OPEN", "STOP", "CLOSE"] as const).filter((v) => (cover.values ?? []).includes(v)).map((v) => (
        <button key={v} type="button" className="topo-btn" disabled={disabled(cover.property)} onClick={() => void send(cover.property, { value: v })}>{v === "OPEN" ? "เปิด" : v === "STOP" ? "หยุด" : "ปิด"}</button>
      ))}</div>}</div>}
      {numeric.map((f) => {
        const current = controls.state[f.property];
        const min = f.value_min ?? 0, max = f.value_max ?? Math.max(100, Number(current) || 0), step = f.value_step ?? 1;
        const value = Number(draft[f.property] ?? current ?? min);
        return (
          <div key={f.property} className="dc-row">
            <span>{label(f)} · <strong>{show(current)}{f.unit ? ` ${f.unit}` : ""}</strong></span>
            {!readOnly && <div className="dc-buttons">
              <input type="range" min={min} max={max} step={step} value={value} disabled={disabled(f.property)} aria-label={label(f)} onChange={(e) => setDraft((d) => ({ ...d, [f.property]: Number(e.target.value) }))} />
              <button type="button" className="topo-btn" disabled={disabled(f.property) || value === Number(current)} onClick={() => void send(f.property, { value })}>{busy(f.property) ? "กำลังสั่ง…" : `ตั้ง ${value}`}</button>
            </div>}
          </div>
        );
      })}
      {readOnly && <small className="dc-note">ดูสถานะได้อย่างเดียว</small>}
      {!readOnly && !controls.online && <small className="dc-note">อุปกรณ์ออฟไลน์ · สั่งงานได้เมื่อกลับมาออนไลน์</small>}
      {failed.length > 0 && <p className="dc-toast" role="alert">{STATUS[failed[failed.length - 1][1].status]} · สถานะคงเดิม</p>}
      {error && <p className="dc-toast" role="alert">{error}</p>}
    </div>
  );
}
