"use client";
// Tuya Cloud (zero-install) on the gateway page: link the user's Tuya IoT project, watch the link and its monthly
// usage, sync and register the project's devices. The Access ID/Secret go to the server once per link or rotation
// and leave this page's state the moment they are sent; the server seals them for the tuya-cloud worker and can
// never show them again.
import { useCallback, useEffect, useRef, useState } from "react";
import { Cloud, KeyRound, Link2Off, RefreshCw, TriangleAlert } from "lucide-react";
import { ApiError, TUYA_REGIONS, type Gateway, type TuyaCloudChannel, type TuyaCloudLink, type TuyaCloudState, type TuyaDevice } from "./api";
import { Step } from "./panel-bits";
import { CREDENTIAL, credentialInput, JOB_ERROR, regionLabel, TuyaProjectSteps } from "./tuya-import";
import { useLatest } from "./use-latest";

export type TuyaCloudClient = {
  tuyaCloudStatus: (gatewayId: string) => Promise<TuyaCloudLink>;
  linkTuyaCloud: (gatewayId: string, input: { region: string; channel: TuyaCloudChannel; access_id: string; access_secret: string }) => Promise<TuyaCloudLink>;
  syncTuyaCloud: (gatewayId: string) => Promise<TuyaCloudLink>;
  unlinkTuyaCloud: (gatewayId: string) => Promise<void>;
  tuyaDevices: (gatewayId: string) => Promise<TuyaDevice[]>;
};

/** The link as the worker last reported it. */
export const CLOUD_STATE_LABEL: Record<TuyaCloudState, string> = {
  "": "ยังไม่เชื่อม",
  linking: "กำลังเชื่อมต่อ Tuya",
  online: "ออนไลน์ · รับข้อความจาก Tuya",
  offline: "ออฟไลน์ · กำลังลองเชื่อมใหม่",
  auth_failed: "Tuya ปฏิเสธ Access ID/Secret",
  not_subscribed: "ยังไม่ได้เปิด Message Service หรือบริการ API",
  quota: "ใช้โควตา Tuya เดือนนี้ถึงงบแล้ว",
  disabled: "ยกเลิกการเชื่อมแล้ว",
};

/** What to do about a link state that needs a person. */
const STATE_HINT: Partial<Record<TuyaCloudState, string>> = {
  linking: "ครั้งแรกหลังเปิด Message Service อาจต้องรอประมาณ 30 นาทีกว่าข้อความจะเริ่มมา",
  offline: "ตรวจอินเทอร์เน็ตของเซิร์ฟเวอร์ และสถานะของ Tuya · อุปกรณ์จะแสดงออฟไลน์ถ้าขาดนาน",
  auth_failed: "Access Secret อาจถูกเปลี่ยนหรือโปรเจกต์หมดอายุ · กด \"เปลี่ยน Access Secret\" แล้วใส่ค่าปัจจุบันจาก Tuya IoT Platform",
  not_subscribed: "เปิด Message Service ของโปรเจกต์ (Cloud → Message Service) และสมัคร IoT Core / Authorization ให้ครบ",
  quota: "Aether หยุดเรียก Tuya ชั่วคราวเพื่อไม่ให้เกินงบ · จะกลับมาเองเดือนหน้า หรือขยายแพ็กเกจของโปรเจกต์",
};

/** Why a Tuya Cloud device is offline: its own reason, or the state of the link it depends on. */
export const CLOUD_REASON_LABEL: Record<string, string> = {
  cloud_link_down: "การเชื่อม Tuya Cloud ขาด · สถานะอาจไม่ล่าสุด",
  auth_failed: "Tuya ปฏิเสธ Access ID/Secret ของโปรเจกต์ · owner/admin ต้องเปลี่ยน Access Secret",
  quota: "ใช้โควตา Tuya เดือนนี้ถึงงบแล้ว · หยุดรับสถานะชั่วคราว",
  not_subscribed: "โปรเจกต์ยังไม่ได้เปิด Message Service",
  disabled: "ยกเลิกการเชื่อม Tuya Cloud แล้ว",
};

/** Why linking was refused (server reason → Thai); Tuya's own refusals share the import's texts. */
const LINK_ERROR: Record<string, string> = {
  ...JOB_ERROR,
  tuya_region: "เลือก Data Center ไม่ถูกต้อง",
  tuya_channel: "เลือกช่องข้อความไม่ถูกต้อง",
  tuya_credentials: "รูปแบบ Access ID / Access Secret ไม่ถูกต้อง (ตัวอักษรภาษาอังกฤษและตัวเลข 8–64 ตัว)",
  already_linked: "โปรเจกต์ Tuya นี้เชื่อมอยู่กับ gateway อื่นแล้ว · ยกเลิกการเชื่อมที่นั่นก่อน (หนึ่งโปรเจกต์ต่อหนึ่ง gateway)",
  cloud_link_limit: "workspace นี้เชื่อม Tuya Cloud ครบจำนวนที่อนุญาตแล้ว · ยกเลิกการเชื่อมอันเดิมก่อน",
  tuya_cloud_unconfigured: "เซิร์ฟเวอร์ยังไม่ได้ตั้งกุญแจของ Tuya Cloud (TUYA_CLOUD_PUBLIC_KEY) · ติดต่อผู้ดูแลระบบ",
  tuya_cloud_disabled: "เซิร์ฟเวอร์นี้ปิดโหมด Tuya Cloud อยู่",
  not_a_cloud_gateway: "gateway นี้ไม่ใช่ Tuya Cloud",
};

const CHANNELS: { id: TuyaCloudChannel; label: string }[] = [
  { id: "event", label: "event (ใช้งานจริง)" },
  { id: "event-test", label: "event-test (ช่องทดสอบของ Tuya)" },
];

function ago(iso: string | null | undefined, now: number): string {
  if (!iso) return "—";
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
  if (s < 60) return `${s} วินาทีที่แล้ว`;
  if (s < 3600) return `${Math.round(s / 60)} นาทีที่แล้ว`;
  return new Date(iso).toLocaleString("th-TH");
}

function reasonOf(e: unknown, fallback: string): string {
  if (e instanceof ApiError && e.reason && LINK_ERROR[e.reason]) return LINK_ERROR[e.reason];
  return e instanceof Error ? e.message : fallback;
}

/** One usage bar: "label x / budget", warning from 80%, full at the budget. */
function Usage({ label, used, budget }: { label: string; used: number; budget: number }) {
  const ratio = budget > 0 ? Math.min(1, used / budget) : 0;
  const level = budget > 0 && used >= budget ? "is-full" : ratio >= 0.8 ? "is-high" : "";
  return (
    <div className={`topo-usage ${level}`}>
      <span>
        {label} <strong>{used.toLocaleString("th-TH")}</strong> / {budget > 0 ? budget.toLocaleString("th-TH") : "ไม่จำกัด"}
      </span>
      <span className="topo-usage-bar" role="meter" aria-label={label} aria-valuemin={0} aria-valuemax={budget || used || 1} aria-valuenow={used}>
        <span style={{ width: `${Math.round(ratio * 100)}%` }} />
      </span>
    </div>
  );
}

/** Always visible: what this mode depends on and must not be used for. */
function CloudWarnings() {
  return (
    <ul className="topo-cloud-warnings" aria-label="ข้อควรทราบของ Tuya Cloud">
      <li>
        <TriangleAlert size={13} />
        <span>ขึ้นกับอินเทอร์เน็ตและ Tuya Cloud · ถ้าเน็ตของอาคาร เซิร์ฟเวอร์ หรือ Tuya ขัดข้อง สถานะและคำสั่งจะหยุด</span>
      </li>
      <li>
        <TriangleAlert size={13} />
        <span><strong>ห้ามใช้กับ SOS หรืองานวิกฤต</strong> (ความปลอดภัยของคน ไฟ น้ำรั่ว) · ใช้ gateway แบบ local (Zigbee2MQTT / Aether Edge / BLE)</span>
      </li>
      <li>
        <TriangleAlert size={13} />
        <span>โปรเจกต์ Tuya IoT แบบ Trial ใช้เพื่อทดลองหรือใช้ส่วนตัวเท่านั้น ห้ามใช้เชิงพาณิชย์ · มีอายุและจำกัดจำนวนข้อความ/การเรียก API ต่อเดือน</span>
      </li>
      <li>
        <TriangleAlert size={13} />
        <span>PDPA: สถานะอุปกรณ์ผ่านเซิร์ฟเวอร์ของ Tuya ใน Data Center ที่เลือก (อยู่นอกประเทศไทย) · แจ้งผู้ใช้อาคารก่อนเปิดใช้</span>
      </li>
      <li>
        <KeyRound size={13} />
        <span>Access Secret ถูกเข้ารหัสทันทีสำหรับตัวเชื่อม Tuya Cloud เท่านั้น · API ของ Aether อ่านกลับไม่ได้ และจะไม่แสดงอีก</span>
      </li>
    </ul>
  );
}

/** Link + status + devices of one Tuya Cloud gateway. Mount it with key={gateway.id}. */
export default function TuyaCloudPanel({ gateway, client, canManage, refreshKey, onNotice, onRegister, onReload, onStatus }: {
  gateway: Gateway;
  client: TuyaCloudClient;
  /** Owner/admin: may link, rotate, sync, unlink and see the device list. */
  canManage: boolean;
  /** Changes when the page reloads, so the status refreshes with it. */
  refreshKey?: unknown;
  onNotice: (m: string) => void;
  onRegister: (tuyaId: string, name: string) => void;
  onReload: () => void;
  /** Every status answer, so the gateway header can show the link's state. */
  onStatus?: (status: TuyaCloudLink) => void;
}) {
  const id = gateway.id;
  const [status, setStatus] = useState<TuyaCloudLink | null>(null);
  const [devices, setDevices] = useState<TuyaDevice[]>([]);
  const [statusError, setStatusError] = useState("");
  const [now, setNow] = useState(() => Date.now());
  const [formOpen, setFormOpen] = useState(false);
  const [region, setRegion] = useState("us");
  const [channel, setChannel] = useState<TuyaCloudChannel>("event");
  const [accessId, setAccessId] = useState("");
  const [accessSecret, setAccessSecret] = useState("");
  const [showSecret, setShowSecret] = useState(false);
  const [sending, setSending] = useState(false);
  const [linkError, setLinkError] = useState("");
  const [confirmUnlink, setConfirmUnlink] = useState(false);
  const [working, setWorking] = useState(false);
  const onStatusRef = useLatest(onStatus);
  const onReloadRef = useLatest(onReload);

  // Every load and mutation gets a sequence number; an answer that is not from the latest one (or arrives after
  // unmount) is dropped, so a slow reply can never overwrite newer state.
  const seq = useRef(0);
  useEffect(() => () => void (seq.current += 1), []);
  const apply = useCallback(
    (s: TuyaCloudLink) => {
      setStatus(s);
      setStatusError("");
      setNow(Date.now());
      onStatusRef.current?.(s);
    },
    [onStatusRef],
  );
  const load = useCallback(() => {
    const n = ++seq.current;
    client
      .tuyaCloudStatus(id)
      .then((s) => n === seq.current && apply(s))
      .catch((e: unknown) => n === seq.current && setStatusError(e instanceof Error ? e.message : "อ่านสถานะ Tuya Cloud ไม่สำเร็จ"));
    if (canManage)
      client
        .tuyaDevices(id)
        .then((items) => n === seq.current && setDevices(items))
        .catch(() => n === seq.current && setDevices([]));
  }, [client, id, canManage, apply]);

  // Poll faster while the worker has something pending (a new link, a requested sync), slower otherwise.
  const busyLink = status?.linked && (status.state === "linking" || !!status.sync_requested_at);
  useEffect(() => {
    load();
    const timer = setInterval(load, busyLink ? 4000 : 15000);
    return () => clearInterval(timer);
  }, [load, refreshKey, busyLink]);

  // A finished sync may have found devices: refresh the page's discovery once when the count changes.
  const seenDevices = useRef<number | null>(null);
  useEffect(() => {
    if (!status) return;
    if (seenDevices.current !== null && seenDevices.current !== status.devices) onReloadRef.current();
    seenDevices.current = status.devices;
  }, [status, onReloadRef]);

  // Closing the form forgets everything typed, including the secret.
  const closeForm = () => {
    setAccessId("");
    setAccessSecret("");
    setShowSecret(false);
    setLinkError("");
    setFormOpen(false);
  };
  const openForm = () => {
    if (status?.linked) {
      setRegion(status.region || "us");
      setChannel(status.channel === "event-test" ? "event-test" : "event");
    }
    setLinkError("");
    setFormOpen(true);
  };

  const credentialsValid = CREDENTIAL.test(accessId.trim()) && CREDENTIAL.test(accessSecret.trim());
  const linked = !!status?.linked;
  const enabled = status?.enabled ?? true;
  const state: TuyaCloudState = linked ? status!.state : "";
  const online = state === "online";
  const showForm = canManage && enabled && (formOpen || (status !== null && !linked));
  const byTuyaId = new Map(devices.map((d) => [d.tuya_id, d]));

  const submit = () => {
    if (!credentialsValid || sending) return;
    const input = { region, channel, access_id: accessId.trim(), access_secret: accessSecret.trim() };
    // The secret leaves this page's state the moment it is sent, whatever the answer.
    setAccessSecret("");
    setShowSecret(false);
    setLinkError("");
    setSending(true);
    const n = ++seq.current;
    client
      .linkTuyaCloud(id, input)
      .then((s) => {
        setAccessId("");
        setFormOpen(false);
        onNotice(linked ? "เปลี่ยน Access Secret แล้ว" : "เชื่อมโปรเจกต์ Tuya แล้ว · กำลังซิงก์รายการอุปกรณ์");
        if (n === seq.current) apply(s);
        load();
      })
      .catch((e: unknown) => setLinkError(reasonOf(e, "เชื่อมไม่สำเร็จ")))
      .finally(() => setSending(false));
  };

  const sync = () => {
    if (working) return;
    setWorking(true);
    const n = ++seq.current;
    client
      .syncTuyaCloud(id)
      .then((s) => {
        onNotice("ขอซิงก์รายการอุปกรณ์แล้ว · ตัวเชื่อมจะดึงรายการภายในไม่กี่วินาที");
        if (n === seq.current) apply(s);
      })
      .catch((e: unknown) => onNotice(reasonOf(e, "ขอซิงก์ไม่สำเร็จ")))
      .finally(() => setWorking(false));
  };

  const unlink = () => {
    if (working) return;
    setConfirmUnlink(false);
    setWorking(true);
    ++seq.current;
    client
      .unlinkTuyaCloud(id)
      .then(() => {
        onNotice("ยกเลิกการเชื่อมแล้ว · ลบ Access ID/Secret ที่เก็บไว้แล้ว อุปกรณ์ที่ลงทะเบียนจะแสดงออฟไลน์");
        closeForm();
        load();
        onReloadRef.current();
      })
      .catch((e: unknown) => onNotice(reasonOf(e, "ยกเลิกการเชื่อมไม่สำเร็จ")))
      .finally(() => setWorking(false));
  };

  return (
    <section className="topo-edge-panel topo-cloud-panel" aria-label="Tuya Cloud">
      <ol className="topo-steps">
        <Step done={linked} active={!linked} label="1 · เชื่อมโปรเจกต์ Tuya IoT" detail={linked ? `Data Center ${regionLabel(status!.region)} · Access ID ${status!.access_id_hint}…` : "ใส่ Access ID / Access Secret ของโปรเจกต์"} />
        <Step done={online} active={linked && !online} label="2 · รับข้อความจาก Tuya" detail={linked ? `${CLOUD_STATE_LABEL[state] ?? state} · ข้อความล่าสุด ${ago(status!.last_event_at, now)}` : undefined} />
        <Step done={(status?.devices ?? 0) > 0} active={linked && (status?.devices ?? 0) === 0} label="3 · ซิงก์รายการอุปกรณ์" detail={linked ? (status!.sync_requested_at ? "กำลังซิงก์…" : `${status!.devices} อุปกรณ์ในโปรเจกต์`) : undefined} />
        <Step done={(status?.registered ?? 0) > 0} active={(status?.devices ?? 0) > 0 && (status?.registered ?? 0) === 0} label="4 · ลงทะเบียนอุปกรณ์" detail={linked ? `ลงทะเบียนแล้ว ${status!.registered}` : undefined} />
      </ol>
      {statusError && <p className="topo-warn">{statusError}</p>}
      {status && !enabled && <p className="topo-warn">เซิร์ฟเวอร์นี้ปิดโหมด Tuya Cloud อยู่ (TUYA_CLOUD) · เชื่อมหรือซิงก์ใหม่ไม่ได้ แต่ยกเลิกการเชื่อมเพื่อลบรหัสที่เก็บไว้ได้</p>}

      {linked && (
        <div className="topo-cloud-status" aria-label="สถานะการเชื่อม">
          <p>
            <em className={`topo-chip health-${online ? "receiving" : state === "linking" ? "pending" : "stale"}`}>{CLOUD_STATE_LABEL[state] ?? state}</em>
            <small> ตั้งแต่ {ago(status!.state_at, now)}</small>
          </p>
          {STATE_HINT[state] && <p className="topo-note">{STATE_HINT[state]}</p>}
          {status!.last_error_code > 0 && <small className="topo-note">Tuya code {status!.last_error_code}</small>}
          <dl className="topo-signal-meters">
            <div>
              <dt>ข้อความล่าสุด</dt>
              <dd>{ago(status!.last_event_at, now)}</dd>
            </div>
            <div>
              <dt>ช่องข้อความ</dt>
              <dd>{status!.channel}</dd>
            </div>
            <div>
              <dt>เชื่อมเมื่อ</dt>
              <dd>{status!.linked_at ? new Date(status!.linked_at).toLocaleString("th-TH") : "—"}</dd>
            </div>
            <div>
              <dt>เปลี่ยน Secret ล่าสุด</dt>
              <dd>{status!.rotated_at ? new Date(status!.rotated_at).toLocaleString("th-TH") : "—"}</dd>
            </div>
          </dl>
          <Usage label="ข้อความเดือนนี้" used={status!.events_month} budget={status!.events_budget} />
          <Usage label="API เดือนนี้" used={status!.api_calls_month} budget={status!.api_calls_budget} />
          {status!.dropped_month > 0 && <small className="topo-note">ข้อความที่ไม่ได้บันทึกเดือนนี้ (เกินงบหรืออ่านไม่ได้) {status!.dropped_month.toLocaleString("th-TH")}</small>}
        </div>
      )}

      <CloudWarnings />

      {!canManage ? (
        <p className="topo-note">การเชื่อม ซิงก์ และยกเลิกการเชื่อมทำได้เฉพาะ owner หรือ admin</p>
      ) : (
        <>
          {linked && !showForm && (
            <div className="topo-actions">
              {enabled && (
                <button type="button" className="topo-btn primary" disabled={working} onClick={sync}>
                  <RefreshCw size={15} /> ซิงก์รายการอุปกรณ์
                </button>
              )}
              {enabled && (
                <button type="button" className="topo-btn" disabled={working} onClick={openForm}>
                  <KeyRound size={15} /> เปลี่ยน Access Secret
                </button>
              )}
              <button type="button" className="topo-btn danger" disabled={working} onClick={() => setConfirmUnlink(true)}>
                <Link2Off size={15} /> ยกเลิกการเชื่อม
              </button>
            </div>
          )}
          {confirmUnlink && (
            <div className="topo-callout" role="alertdialog" aria-label="ยืนยันยกเลิกการเชื่อม">
              ยกเลิกการเชื่อมโปรเจกต์ Tuya นี้? Aether จะลบ Access ID/Secret ที่เก็บไว้ทันที หยุดรับสถานะและสั่งงาน · อุปกรณ์ที่ลงทะเบียนไว้ยังอยู่แต่จะแสดงออฟไลน์ · เชื่อมใหม่ได้ภายหลัง
              <div className="topo-actions">
                <button type="button" className="topo-btn" onClick={() => setConfirmUnlink(false)}>
                  ยกเลิก
                </button>
                <button type="button" className="topo-btn danger" onClick={unlink}>
                  ยกเลิกการเชื่อม
                </button>
              </div>
            </div>
          )}

          {showForm && (
            <div className="topo-cloud-form">
              <h3 className="topo-h3">
                <Cloud size={14} /> {linked ? "เปลี่ยน Access Secret" : "เชื่อมโปรเจกต์ Tuya IoT"}
              </h3>
              {!linked && <TuyaProjectSteps messageService />}
              {linked && <p className="topo-note">ใส่ Access ID และ Access Secret ปัจจุบันของโปรเจกต์เดิม (Access ID ขึ้นต้นด้วย {status!.access_id_hint}…) · ระบบตรวจกับ Tuya ก่อนแทนที่ค่าเดิม</p>}
              <form
                className="topo-edge-form"
                autoComplete="off"
                onSubmit={(e) => {
                  e.preventDefault();
                  submit();
                }}
              >
                <label>
                  Data Center
                  <select value={region} disabled={sending} onChange={(e) => setRegion(e.target.value)}>
                    {TUYA_REGIONS.map((r) => (
                      <option key={r.id} value={r.id}>
                        {r.label}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  ช่องข้อความ (Message Service)
                  <select value={channel} disabled={sending} onChange={(e) => setChannel(e.target.value === "event-test" ? "event-test" : "event")}>
                    {CHANNELS.map((c) => (
                      <option key={c.id} value={c.id}>
                        {c.label}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  Access ID
                  <input {...credentialInput} name="tuya-cloud-client" value={accessId} disabled={sending} onChange={(e) => setAccessId(e.target.value)} aria-invalid={accessId !== "" && !CREDENTIAL.test(accessId.trim())} />
                </label>
                <label>
                  Access Secret
                  <span className="topo-secret-input">
                    <input {...credentialInput} name="tuya-cloud-signing" className={showSecret ? "" : "topo-masked"} value={accessSecret} disabled={sending} onChange={(e) => setAccessSecret(e.target.value)} aria-invalid={accessSecret !== "" && !CREDENTIAL.test(accessSecret.trim())} />
                    <button type="button" className="topo-btn" aria-pressed={showSecret} onClick={() => setShowSecret((v) => !v)}>
                      {showSecret ? "ซ่อน" : "แสดง"}
                    </button>
                  </span>
                </label>
                {sending && (
                  <p className="topo-note" role="status">
                    กำลังตรวจ Access ID/Secret กับ Tuya แล้วเข้ารหัสเก็บ…
                  </p>
                )}
                {linkError && (
                  <p className="topo-warn" role="alert">
                    {linkError}
                  </p>
                )}
                <div className="topo-actions">
                  {(linked || formOpen) && (
                    <button type="button" className="topo-btn" disabled={sending} onClick={closeForm}>
                      ยกเลิก
                    </button>
                  )}
                  <button type="submit" className="topo-btn primary" disabled={!credentialsValid || sending}>
                    {sending ? "กำลังเชื่อม…" : linked ? "บันทึก Secret ใหม่" : "เชื่อม"}
                  </button>
                </div>
              </form>
            </div>
          )}

          {linked && (
            <>
              <h3 className="topo-h3">
                <Cloud size={14} /> อุปกรณ์ในโปรเจกต์ <span className="topo-count">{devices.length}</span>
                <button type="button" className="topo-icon-btn" aria-label="รีเฟรช" onClick={load}>
                  <RefreshCw size={14} />
                </button>
              </h3>
              {devices.length === 0 ? (
                <p className="topo-note">{status!.sync_requested_at ? "กำลังซิงก์รายการอุปกรณ์จาก Tuya…" : "ยังไม่พบอุปกรณ์ · ตรวจว่าผูกบัญชีแอป Smart Life กับโปรเจกต์แล้ว (Devices → Link App Account) แล้วกดซิงก์"}</p>
              ) : (
                <div className="topo-table-wrap">
                  <table className="topo-tuya-table">
                    <thead>
                      <tr>
                        <th>อุปกรณ์</th>
                        <th>ออนไลน์</th>
                        <th>ชนิด</th>
                        <th>hub</th>
                        <th>สถานะ</th>
                      </tr>
                    </thead>
                    <tbody>
                      {devices.map((d) => {
                        const hub = d.parent_tuya_id ? byTuyaId.get(d.parent_tuya_id) : undefined;
                        return (
                          <tr key={d.tuya_id}>
                            <td>
                              <strong>{d.name || d.tuya_id}</strong>
                              <small>
                                {d.tuya_category || "—"} · <code>{d.tuya_id}</code>
                              </small>
                              {d.available === false && d.reason && <small className="topo-cloud-reason">{CLOUD_REASON_LABEL[d.reason] ?? d.reason}</small>}
                            </td>
                            <td>{d.available === true ? "ออนไลน์" : d.available === false ? "ออฟไลน์" : "—"}</td>
                            <td>{d.sub ? "อุปกรณ์ย่อย" : "Wi‑Fi"}</td>
                            <td>{d.sub ? hub?.name || d.parent_tuya_id || "—" : "—"}</td>
                            <td>
                              {d.registered ? (
                                <span className="topo-verdict is-ok">ลงทะเบียนแล้ว</span>
                              ) : enabled ? (
                                <button type="button" className="topo-btn" onClick={() => onRegister(d.tuya_id, d.name)}>
                                  ลงทะเบียน
                                </button>
                              ) : (
                                <span className="topo-verdict is-no">โหมดปิดอยู่</span>
                              )}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </>
          )}
        </>
      )}
    </section>
  );
}

/**
 * Connection of one registered Tuya Cloud device, for its inspector: online/offline with the reason (its own, or the
 * link's when the link is what is down). The link status is readable by every member; the device row needs
 * owner/admin, so others see the link's state only.
 */
export function TuyaCloudDeviceStatus({ gatewayId, tuyaId, client, canManage, refreshKey }: {
  gatewayId: string;
  tuyaId: string;
  client: Pick<TuyaCloudClient, "tuyaCloudStatus" | "tuyaDevices">;
  canManage: boolean;
  refreshKey?: unknown;
}) {
  const [link, setLink] = useState<TuyaCloudLink | null>(null);
  const [device, setDevice] = useState<TuyaDevice | null>(null);
  const seq = useRef(0);
  useEffect(() => () => void (seq.current += 1), []);
  // Reloaded on every report and every 15 s: a link can go down (quota, auth) while the device reports nothing.
  useEffect(() => {
    const load = () => {
      const n = ++seq.current;
      client
        .tuyaCloudStatus(gatewayId)
        .then((s) => n === seq.current && setLink(s))
        .catch(() => n === seq.current && setLink(null));
      if (canManage)
        client
          .tuyaDevices(gatewayId)
          .then((items) => n === seq.current && setDevice(items.find((d) => d.tuya_id === tuyaId.toLowerCase()) ?? null))
          .catch(() => n === seq.current && setDevice(null));
    };
    load();
    const timer = setInterval(load, 15000);
    return () => clearInterval(timer);
  }, [client, gatewayId, tuyaId, canManage, refreshKey]);
  if (!link) return null;
  const state: TuyaCloudState = link.linked ? link.state : "disabled";
  const linkProblem = state === "auth_failed" || state === "quota" || state === "not_subscribed" || state === "disabled" ? state : "";
  const reason = linkProblem || (device?.available === false ? device.reason : "");
  return (
    <section className="topo-tuya-status" aria-label="สถานะ Tuya Cloud">
      <h3 className="topo-h3">
        <Cloud size={14} /> Tuya Cloud
      </h3>
      <p>
        <em className={`topo-verdict ${device?.available === false || linkProblem ? "is-no" : "is-ok"}`}>
          {linkProblem ? "สถานะไม่อัปเดต" : device ? (device.available === false ? "ออฟไลน์" : device.available ? "ออนไลน์" : "ยังไม่ทราบ") : CLOUD_STATE_LABEL[state]}
        </em>
        {reason && <small> {CLOUD_REASON_LABEL[reason] ?? reason}</small>}
      </p>
      <p className="topo-note">สถานะและคำสั่งผ่าน Tuya Cloud · ต้องมีอินเทอร์เน็ต · ห้ามใช้กับ SOS หรืองานวิกฤต</p>
    </section>
  );
}
