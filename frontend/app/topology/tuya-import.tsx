"use client";
// One-time Tuya key import for an Aether Edge. The Access ID/Secret go to the server once, for one import job, and
// are dropped from this page's state as soon as they are sent: nothing is kept in memory longer or written anywhere.
// The running job itself belongs to the Edge panel (EdgePanel), so closing this dialog never loses it.
import { useState } from "react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { ApiError, TUYA_REGIONS, type TuyaDevice, type TuyaImportJob, type TuyaImportedDevice, type TuyaTransport } from "./api";

export type TuyaImportClient = {
  startTuyaImport: (gatewayId: string, input: { region: string; access_id: string; access_secret: string }) => Promise<TuyaImportJob>;
  tuyaImport: (gatewayId: string, jobId: string) => Promise<TuyaImportJob>;
  tuyaDevices: (gatewayId: string) => Promise<TuyaDevice[]>;
};

/** The server accepts letters and digits, 8 to 64 characters, for both (backend app.tuyaCredential). */
export const CREDENTIAL = /^[A-Za-z0-9]{8,64}$/;

const STAGE_LABEL: Record<TuyaImportJob["stage"], string> = {
  token: "เข้าสู่ระบบ Tuya IoT",
  devices: "ดึงรายการอุปกรณ์และคีย์",
  factory: "อ่านข้อมูล Bluetooth จากโรงงาน",
  models: "อ่านจุดข้อมูล (DP) ของแต่ละรุ่น",
  saving: "เข้ารหัสคีย์และบันทึก",
  done: "เสร็จแล้ว",
};

/** Why a start request was refused (server reason → Thai). */
const START_ERROR: Record<string, string> = {
  tuya_region: "เลือก Data Center ไม่ถูกต้อง",
  tuya_credentials: "รูปแบบ Access ID / Access Secret ไม่ถูกต้อง (ตัวอักษรภาษาอังกฤษและตัวเลข 8–64 ตัว)",
  import_running: "มีการนำเข้าของ gateway นี้กำลังทำอยู่ รอให้เสร็จก่อน",
  too_many_imports: "นำเข้าบ่อยเกินไป รอสักครู่แล้วลองใหม่",
  not_an_edge_gateway: "gateway นี้ไม่ใช่ Aether Edge",
};

/** Why a running import failed (job.error → Thai); a Tuya Cloud link is refused with the same tuya_* reasons. */
export const JOB_ERROR: Record<string, string> = {
  tuya_auth_failed: "Access ID / Access Secret ไม่ถูกต้อง หรือเลือก Data Center ไม่ตรงกับโปรเจกต์",
  tuya_not_subscribed: "โปรเจกต์ยังไม่ได้สมัครบริการ IoT Core และ Authorization (หน้า Service API ของโปรเจกต์)",
  tuya_permission: "โปรเจกต์ยังไม่ได้ผูกบัญชีแอป Smart Life (Devices → Link App Account แล้วสแกน QR ด้วยแอป)",
  tuya_rate_limited: "Tuya จำกัดจำนวนครั้งที่เรียก API อยู่ รอสักครู่ หรือขยายอายุ trial ของโปรเจกต์",
  tuya_timeout: "Tuya ตอบช้าเกินไป ลองใหม่อีกครั้ง",
  tuya_unreachable: "เชื่อมต่อ Tuya IoT ไม่ได้ ลองใหม่อีกครั้ง",
  tuya_error: "Tuya ตอบกลับด้วยข้อผิดพลาด ลองใหม่อีกครั้ง",
  // Set by the page, not the server: the job could no longer be read.
  job_lost: "ไม่พบงานนำเข้านี้แล้ว (หมดอายุ หรือเซิร์ฟเวอร์เพิ่งรีสตาร์ต) · กดเริ่มใหม่ แล้วใส่ Access ID / Secret อีกครั้ง",
  job_unreachable: "อ่านสถานะการนำเข้าไม่ได้หลายครั้งติดกัน · ตรวจการเชื่อมต่อ แล้วกดเริ่มใหม่ (ถ้าการนำเข้าเสร็จไปแล้ว รายการอุปกรณ์จะอัปเดตเอง)",
  job_refused: "อ่านสถานะการนำเข้าไม่ได้ (ไม่มีสิทธิ์ หรือคำขอไม่ถูกต้อง) · กดเริ่มใหม่",
};

export const regionLabel = (id: string) => TUYA_REGIONS.find((r) => r.id === id)?.label ?? id;

/**
 * What a device of the import can do locally, in the order that decides it. One rule for every place that offers
 * "ลงทะเบียน" (discovery, the Edge panel, this table): a device with a usable key that can work locally may be
 * registered; not being seen yet is only a warning, since the Edge connects as soon as it hears it.
 *
 * Tuya BLE (docs/platform/tuya-ble.md): a device an owner/admin set to Bluetooth is local, battery sensors included.
 * One the Edge only heard over Bluetooth is a candidate (`ble: "candidate"`): it must be set to Bluetooth first,
 * never automatically.
 */
export function tuyaVerdict(d: {
  sub: boolean;
  local_capable: boolean;
  has_key: boolean;
  lan_seen?: boolean;
  transport?: TuyaTransport;
  ble_seen?: boolean;
  ble_capable?: boolean;
  readonly?: boolean;
  rssi?: number | null;
}): { label: string; ok: boolean; warn?: string; ble?: "candidate" | "set" } {
  if (d.sub) return { label: "ผ่าน hub Tuya · ใช้แบบ local ไม่ได้ (ใช้โหมด Tuya Cloud แทน)", ok: false };
  if (!d.has_key) return { label: "ไม่ได้รับคีย์จาก Tuya", ok: false };
  if (d.transport === "ble") {
    const label = d.readonly ? "Bluetooth · อ่านค่าอย่างเดียว (กลอน)" : "ใช้ทาง Bluetooth ได้";
    return d.ble_seen === false ? { label, ok: true, warn: BLE_WARNING, ble: "set" } : { label, ok: true, ble: "set" };
  }
  if (d.local_capable) {
    if (d.lan_seen === false) return { label: "ใช้แบบ local ได้ (Wi‑Fi)", ok: true, warn: LAN_WARNING };
    return { label: "ใช้แบบ local ได้ (Wi‑Fi)", ok: true };
  }
  if (d.ble_seen) return { label: `พบทาง BLE${d.rssi != null ? ` · ${d.rssi} dBm` : ""} · ตั้งเป็น Bluetooth ก่อนลงทะเบียน`, ok: false, ble: "candidate" };
  if (d.ble_capable) return { label: "อาจเป็นอุปกรณ์ Bluetooth · BLE ไม่อยู่ในระยะ", ok: false, warn: BLE_WARNING, ble: "candidate" };
  return { label: "อุปกรณ์แบตเตอรี่ไม่มีข้อมูล Bluetooth · ถ้าเป็น Bluetooth Mesh ใช้โหมด Tuya Cloud", ok: false };
}

/** Shown next to a registrable device the Edge has not heard on the LAN yet. */
export const LAN_WARNING = "ยังไม่พบใน LAN · ตรวจว่าเปิดอยู่และอยู่วง LAN เดียวกับ Pi · ลงทะเบียนได้ Aether Edge จะเชื่อมต่อเมื่อพบ";

/** Shown next to a Bluetooth device the Edge has not heard advertising yet. */
export const BLE_WARNING = "ยังไม่ได้ยินสัญญาณ Bluetooth · วาง Pi ให้ห่างไม่เกิน ~10 เมตร และติดตั้ง Aether Edge แบบ --ble · อุปกรณ์แบตเตอรี่อาจส่งสัญญาณเป็นช่วง ๆ";

/** How a device is reached, for the "ทาง" column: the transport an owner/admin set, or what the Edge has seen. */
export function transportLabel(d: TuyaDevice | undefined): string {
  if (!d) return "—";
  if (d.transport === "ble") return `Bluetooth${d.ble_seen ? `${d.rssi != null ? ` · ${d.rssi} dBm` : " · ได้ยินแล้ว"}` : " · ยังไม่ได้ยิน"}`;
  if (d.lan_seen) return `Wi‑Fi · พบที่ ${d.ip || "?"}${d.version ? ` · v${d.version}` : ""}`;
  if (d.detected === "ble_candidate") return `ได้ยินทาง BLE${d.rssi != null ? ` · ${d.rssi} dBm` : ""}`;
  return "ยังไม่พบ";
}

/** The pairing state of a Bluetooth device, when there is something to say. */
export function blePairing(d: TuyaDevice): string {
  if (d.transport !== "ble" && d.detected !== "ble_candidate") return "";
  if (d.key_status === "rejected" || d.key_status === "suspect") return "คีย์ไม่ตรง · นำเข้าจาก Tuya อีกครั้ง";
  if (d.bound === false) return "อุปกรณ์ไม่ได้ผูกกับแอปแล้ว · เพิ่มในแอป Smart Life แล้วนำเข้าอีกครั้ง";
  if (d.transport === "ble" && d.last_read_at) return `อ่านค่าล่าสุด ${new Date(d.last_read_at).toLocaleString("th-TH")}`;
  return "";
}

/** Keeps a credential out of password managers: plain text inputs masked with CSS, nothing that looks like a login. */
export const credentialInput = {
  type: "text",
  autoComplete: "off",
  autoCorrect: "off",
  autoCapitalize: "off",
  spellCheck: false,
  "data-lpignore": "true",
  "data-1p-ignore": "true",
  "data-bwignore": "true",
  "data-form-type": "other",
} as const;

/**
 * Setting up a Tuya IoT project, shared by the key import (Aether Edge) and the Tuya Cloud link. The cloud link also
 * needs the project's Message Service, which is where live status comes from.
 */
export function TuyaProjectSteps({ messageService = false }: { messageService?: boolean }) {
  return (
    <ol className="topo-tuya-steps">
      <li>สมัครและเข้า <strong>platform.tuya.com</strong> (Tuya IoT Platform) → Cloud → สร้างโปรเจกต์แบบ <strong>Smart Home</strong></li>
      <li>เลือก <strong>Data Center</strong> ให้ตรงกับบัญชีแอป · บัญชี Smart Life ในไทยส่วนใหญ่อยู่ <strong>Western America</strong> ถ้าไม่พบอุปกรณ์ลอง <strong>Singapore</strong></li>
      <li>ที่หน้า Service API ของโปรเจกต์ สมัคร <strong>IoT Core</strong> และ <strong>Authorization</strong>{messageService ? <> และ <strong>Message Service</strong></> : null}</li>
      <li>ไปที่ Devices → <strong>Link App Account</strong> แล้วสแกน QR ด้วยแอป Smart Life (เมนู ฉัน)</li>
      {messageService && (
        <li>
          เปิดรับข้อความ: Cloud → <strong>Message Service</strong> → เลือกโปรเจกต์นี้ → <strong>Enable</strong> โดยใช้ Data Center <strong>เดียวกับโปรเจกต์</strong> · หลังเปิดครั้งแรกอาจต้องรอ<strong>ประมาณ 30 นาที</strong>กว่าสถานะอุปกรณ์จะเริ่มส่งมา · ปิดตัวรับข้อความอื่นของโปรเจกต์นี้ก่อน (เช่น integration Tuya ของ Home Assistant แบบเก่า) เพราะข้อความจะถูกแบ่งไปคนละที่
        </li>
      )}
      <li>คัดลอก <strong>Access ID</strong> และ <strong>Access Secret</strong> จากหน้า Overview ของโปรเจกต์มาใส่ด้านล่าง</li>
    </ol>
  );
}

export default function TuyaImportDialog({ open, gatewayId, client, job, jobError, devices, onJob, onClose, onRegister }: {
  open: boolean;
  gatewayId: string;
  client: TuyaImportClient;
  /** The gateway's current import job, owned and polled by the Edge panel. */
  job: TuyaImportJob | null;
  /** A transient problem reading the job (the panel keeps retrying). */
  jobError: string;
  /** The gateway's imported devices as the panel last loaded them: LAN and registration state. */
  devices: TuyaDevice[];
  onJob: (job: TuyaImportJob | null) => void;
  onClose: () => void;
  /** Opens the registration form for an imported device. */
  onRegister: (tuyaId: string, name: string) => void;
}) {
  const [region, setRegion] = useState("us");
  const [accessId, setAccessId] = useState("");
  const [accessSecret, setAccessSecret] = useState("");
  const [showSecret, setShowSecret] = useState(false);
  const [error, setError] = useState("");
  const [sending, setSending] = useState(false);

  // Closing forgets everything typed, including the secret. A running job keeps running and stays with the panel.
  const close = () => {
    setAccessId("");
    setAccessSecret("");
    setShowSecret(false);
    setError("");
    onClose();
  };

  const credentialsValid = CREDENTIAL.test(accessId.trim()) && CREDENTIAL.test(accessSecret.trim());
  const running = job?.status === "running";
  const byId = new Map(devices.map((d) => [d.tuya_id, d]));
  const rows: (TuyaImportedDevice & { lan?: TuyaDevice })[] = (job?.devices ?? []).map((d) => ({ ...d, lan: byId.get(d.tuya_id) }));

  return (
    <Dialog open={open} onOpenChange={(next) => !next && !sending && close()}>
      <DialogContent className="topo-dialog topo-tuya-dialog">
        <DialogHeader>
          <DialogTitle>นำเข้าคีย์จาก Tuya</DialogTitle>
          <DialogDescription>ดึง local key ของอุปกรณ์ Tuya ครั้งเดียวจาก Tuya IoT Platform · หลังจากนี้ Aether Edge คุยกับอุปกรณ์เองทาง Wi‑Fi ในวง LAN หรือทาง Bluetooth โดยไม่ใช้ Tuya cloud</DialogDescription>
        </DialogHeader>

        {!job && (
          <>
            <TuyaProjectSteps />
            <form
              className="topo-form-grid"
              autoComplete="off"
              onSubmit={(e) => {
                e.preventDefault();
                if (!credentialsValid || sending) return;
                const input = { region, access_id: accessId.trim(), access_secret: accessSecret.trim() };
                // The secret leaves this page's state the moment it is sent, whatever the answer.
                setAccessSecret("");
                setError("");
                setSending(true);
                client
                  .startTuyaImport(gatewayId, input)
                  .then((started) => {
                    setAccessId("");
                    onJob(started);
                  })
                  .catch((err: unknown) => setError(err instanceof ApiError && err.reason && START_ERROR[err.reason] ? START_ERROR[err.reason] : err instanceof Error ? err.message : "เริ่มนำเข้าไม่สำเร็จ"))
                  .finally(() => setSending(false));
              }}
            >
              <label>
                Data Center
                <select value={region} onChange={(e) => setRegion(e.target.value)}>
                  {TUYA_REGIONS.map((r) => (
                    <option key={r.id} value={r.id}>
                      {r.label}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                Access ID
                <input {...credentialInput} name="tuya-project-client" value={accessId} onChange={(e) => setAccessId(e.target.value)} aria-invalid={accessId !== "" && !CREDENTIAL.test(accessId.trim())} />
              </label>
              <label>
                Access Secret
                <span className="topo-secret-input">
                  <input {...credentialInput} name="tuya-project-signing" className={showSecret ? "" : "topo-masked"} value={accessSecret} onChange={(e) => setAccessSecret(e.target.value)} aria-invalid={accessSecret !== "" && !CREDENTIAL.test(accessSecret.trim())} />
                  <button type="button" className="topo-btn" aria-pressed={showSecret} onClick={() => setShowSecret((v) => !v)}>
                    {showSecret ? "ซ่อน" : "แสดง"}
                  </button>
                </span>
              </label>
              {error && (
                <p className="topo-warn" role="alert">
                  {error}
                </p>
              )}
              <div className="topo-dialog-actions">
                <button type="button" className="topo-btn" disabled={sending} onClick={close}>
                  ยกเลิก
                </button>
                <button type="submit" className="topo-btn primary" disabled={!credentialsValid || sending}>
                  {sending ? "กำลังเริ่ม…" : "นำเข้า"}
                </button>
              </div>
              <small className="topo-note">Aether ใช้ Access ID/Secret เฉพาะรอบนำเข้านี้แล้วทิ้ง ไม่เก็บไว้ที่ใด · คีย์ที่ได้จะถูกเข้ารหัสและแสดงเป็นลายนิ้วมือ (fingerprint) เท่านั้น · Tuya IoT แบบ trial ใช้ได้เพื่อทดลอง/ใช้ส่วนตัว ห้ามใช้เชิงพาณิชย์ และมีอายุ 1 เดือน (ขอต่ออายุได้) · หมดอายุแล้วอุปกรณ์ยังทำงานแบบ local ต่อ แต่นำเข้าใหม่ไม่ได้</small>
            </form>
          </>
        )}

        {job && (
          <section className="topo-tuya-job" aria-live="polite">
            <p className="topo-note">
              Data Center {regionLabel(job.region)} · {job.status === "running" ? `กำลัง${STAGE_LABEL[job.stage]}…` : job.status === "done" ? "นำเข้าเสร็จแล้ว" : "นำเข้าไม่สำเร็จ"}
            </p>
            {jobError && job.status === "running" && (
              <p className="topo-warn" role="status">
                {jobError}
              </p>
            )}
            {job.status === "failed" && (
              <p className="topo-warn" role="alert">
                {JOB_ERROR[job.error ?? ""] ?? "นำเข้าไม่สำเร็จ"}
                {job.tuya_code ? ` (Tuya code ${job.tuya_code})` : ""}
              </p>
            )}
            {job.status === "done" && (
              <p className="topo-note">
                พบ {job.found} · นำเข้า {job.imported} · มีคีย์ {job.with_key} · ใช้แบบ local ได้ทาง Wi‑Fi {job.local_capable}
                {job.ble_candidates ? ` · อาจเป็น Bluetooth ${job.ble_candidates}` : ""}
                {job.skipped > 0 ? ` · ข้าม ${job.skipped}` : ""}
              </p>
            )}
            {job.status === "done" && job.factory_infos === "unavailable" && (
              <p className="topo-note">โปรเจกต์นี้อ่านข้อมูลโรงงาน (ที่อยู่ Bluetooth) ไม่ได้ · อุปกรณ์ Bluetooth ยังจับคู่ได้จาก uuid ที่ Aether Edge ได้ยิน</p>
            )}
            {job.hint === "no_devices_try_other_region" && (
              <div className="topo-callout">
                ไม่พบอุปกรณ์ใน Data Center {regionLabel(job.region)} · บัญชีแอปอาจอยู่ Data Center อื่น
                {(job.suggest_regions ?? []).length > 0 && (
                  <span className="topo-actions">
                    {(job.suggest_regions ?? []).map((r) => (
                      <button key={r} type="button" className="topo-btn" onClick={() => { setRegion(r); onJob(null); }}>
                        ลอง {regionLabel(r)}
                      </button>
                    ))}
                  </span>
                )}
              </div>
            )}
            {rows.length > 0 && (
              <div className="topo-table-wrap">
                <table className="topo-tuya-table">
                  <thead>
                    <tr>
                      <th>อุปกรณ์</th>
                      <th>คีย์</th>
                      <th>ทาง</th>
                      <th>สถานะ</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((d) => {
                      const verdict = tuyaVerdict({
                        sub: d.sub,
                        local_capable: d.local_capable,
                        has_key: d.has_key,
                        lan_seen: d.lan?.lan_seen,
                        transport: d.lan?.transport,
                        ble_seen: d.lan?.ble_seen,
                        ble_capable: d.lan?.ble_capable ?? d.ble_candidate,
                        readonly: d.lan?.readonly,
                        rssi: d.lan?.rssi,
                      });
                      const pairing = d.lan ? blePairing(d.lan) : "";
                      return (
                        <tr key={d.tuya_id}>
                          <td>
                            <strong>{d.name || d.tuya_id}</strong>
                            <small>
                              {d.tuya_category || "—"} · <code>{d.tuya_id}</code>
                            </small>
                          </td>
                          <td>{d.has_key ? <code title="ลายนิ้วมือของคีย์ ไม่ใช่ตัวคีย์">{d.key_fingerprint}</code> : "—"}</td>
                          <td>{transportLabel(d.lan)}</td>
                          <td>
                            <span className={`topo-verdict ${verdict.ok ? "is-ok" : "is-no"}`}>{verdict.label}</span>
                            {verdict.warn && <small className="topo-warn">{verdict.warn}</small>}
                            {pairing && <small>{pairing}</small>}
                          </td>
                          <td>
                            {d.lan?.registered ? (
                              <em>ลงทะเบียนแล้ว</em>
                            ) : verdict.ok && (d.lan?.key_status ?? "ok") === "ok" ? (
                              <button type="button" className="topo-btn" onClick={() => onRegister(d.tuya_id, d.name)}>
                                ลงทะเบียน
                              </button>
                            ) : null}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
            <div className="topo-dialog-actions">
              {!running && (
                <button type="button" className="topo-btn" onClick={() => onJob(null)}>
                  {job.status === "failed" ? "เริ่มใหม่" : "นำเข้าอีกครั้ง"}
                </button>
              )}
              <button type="button" className="topo-btn primary" onClick={close}>
                {running ? "ปิด (นำเข้าต่อเบื้องหลัง)" : "ปิด"}
              </button>
            </div>
            <small className="topo-note">
              อุปกรณ์ Wi‑Fi ที่ &quot;ยังไม่พบ&quot; อาจยังไม่ได้เปิด Aether Edge หรืออยู่คนละวง LAN/VLAN กับ Pi · อุปกรณ์ Bluetooth (รวมเซนเซอร์แบตเตอรี่) ใช้แบบ local ได้ เมื่อ owner/admin ตั้งเป็น &quot;Bluetooth&quot; ในรายการอุปกรณ์ที่นำเข้า · ถ้าลบแล้วเพิ่มอุปกรณ์ใหม่ในแอป คีย์จะเปลี่ยน ต้องนำเข้าอีกครั้ง
            </small>
          </section>
        )}
      </DialogContent>
    </Dialog>
  );
}
