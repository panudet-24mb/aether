"use client";
// Aether Edge on the gateway page: the agent's live status, the one-line installer for the site host (with an
// optional Zigbee2MQTT on the same host), the imported Tuya devices, and the key status of one Tuya device.
import { useCallback, useEffect, useRef, useState } from "react";
import { Download, KeyRound, RefreshCw, TerminalSquare, Trash2 } from "lucide-react";
import { ApiError, type EdgeInstallCode, type EdgeStatus, type Gateway, type TuyaDevice, type TuyaImportJob, type TuyaKeyStatus } from "./api";
import { Z2M_GATEWAY_MODEL } from "./catalog";
import { CopyButton, Step } from "./panel-bits";
import TuyaImportDialog, { tuyaVerdict, type TuyaImportClient } from "./tuya-import";
import { useLatest } from "./use-latest";

export type EdgeClient = TuyaImportClient & {
  edgeStatus: (gatewayId: string) => Promise<EdgeStatus>;
  edgeInstallCode: (gatewayId: string, zigbeeGatewayId?: string) => Promise<EdgeInstallCode>;
  forgetTuyaKey: (gatewayId: string, tuyaId: string) => Promise<void>;
};

const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

export const KEY_LABEL: Record<TuyaKeyStatus, string> = {
  ok: "คีย์ใช้ได้",
  rejected: "คีย์ไม่ตรง · นำเข้าจาก Tuya อีกครั้ง",
  suspect: "คีย์อาจไม่ตรง · ตรวจอีกครั้ง",
  missing: "ยังไม่มีคีย์ · นำเข้าคีย์จาก Tuya ก่อน",
};

/** Why the agent reports a Tuya device offline (TuyaDevice.reason / availability reason). */
export const REASON_LABEL: Record<string, string> = {
  busy: "มีโปรแกรมอื่นเชื่อมต่ออุปกรณ์นี้อยู่ เช่น Home Assistant/LocalTuya · ปิดก่อน · แอป Smart Life ยังใช้ผ่าน cloud ได้",
  unreachable: "เชื่อมต่อไม่ได้ · ตรวจว่าอุปกรณ์เปิดอยู่และอยู่วง LAN เดียวกับ Pi",
  auth_failed: "อุปกรณ์ปฏิเสธคีย์ · มักเกิดจากลบแล้วเพิ่มอุปกรณ์ใหม่ในแอป นำเข้าจาก Tuya อีกครั้ง",
  key_suspect: "ถอดรหัสข้อความไม่ได้ · คีย์อาจไม่ตรง นำเข้าจาก Tuya อีกครั้ง",
  not_found: "Aether Edge ไม่พบอุปกรณ์นี้ใน LAN",
};

function ago(iso: string | null | undefined, now: number): string {
  if (!iso) return "—";
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
  if (s < 60) return `${s} วินาทีที่แล้ว`;
  if (s < 3600) return `${Math.round(s / 60)} นาทีที่แล้ว`;
  return new Date(iso).toLocaleString("th-TH");
}

/** Live status + installer + Tuya import for one Aether Edge gateway. */
export default function EdgePanel({ gateway, gateways, client, canManage, refreshKey, onNotice, onRegister, onReload, onStatus }: {
  gateway: Gateway;
  /** Every gateway of the workspace, to pick a Zigbee2MQTT gateway to install on the same host. */
  gateways: Gateway[];
  client: EdgeClient;
  /** Owner/admin: may create install codes, import keys and forget keys. */
  canManage: boolean;
  /** Changes when the page reloads, so the status refreshes with it. */
  refreshKey?: unknown;
  onNotice: (m: string) => void;
  onRegister: (tuyaId: string, name: string) => void;
  onReload: () => void;
  /** Every status answer, so the gateway header can show the agent's own state. */
  onStatus?: (status: EdgeStatus) => void;
}) {
  const id = gateway.id;
  const [status, setStatus] = useState<EdgeStatus | null>(null);
  const [devices, setDevices] = useState<TuyaDevice[]>([]);
  const [statusError, setStatusError] = useState("");
  const [now, setNow] = useState(() => Date.now());
  const [zigbee, setZigbee] = useState(false);
  const zigbeeGateways = gateways.filter((g) => g.model === Z2M_GATEWAY_MODEL);
  const [zigbeeGateway, setZigbeeGateway] = useState("");
  const [slzbIP, setSlzbIP] = useState("");
  const [uiIP, setUiIP] = useState("");
  const [code, setCode] = useState<EdgeInstallCode | null>(null);
  const [codeError, setCodeError] = useState("");
  const [creating, setCreating] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [forget, setForget] = useState<TuyaDevice | null>(null);
  // The import job lives here, not in the dialog: closing the dialog keeps it running and polled, and its
  // completion refreshes this panel whether or not the dialog is open. Memory only.
  const [job, setJob] = useState<TuyaImportJob | null>(null);
  const [jobError, setJobError] = useState("");
  const onStatusRef = useLatest(onStatus);
  const onReloadRef = useLatest(onReload);

  // Every load gets a sequence number; an answer that is not from the latest load (or arrives after unmount) is
  // dropped, so a slow reply can never overwrite newer state.
  const seq = useRef(0);
  useEffect(() => () => void (seq.current += 1), []);
  const load = useCallback(() => {
    const n = ++seq.current;
    client
      .edgeStatus(id)
      .then((s) => {
        if (n !== seq.current) return;
        setStatus(s);
        setStatusError("");
        setNow(Date.now());
        onStatusRef.current?.(s);
      })
      .catch((e: unknown) => n === seq.current && setStatusError(e instanceof Error ? e.message : "อ่านสถานะ Aether Edge ไม่สำเร็จ"));
    if (canManage)
      client
        .tuyaDevices(id)
        .then((items) => n === seq.current && setDevices(items))
        .catch(() => n === seq.current && setDevices([]));
  }, [client, id, canManage, onStatusRef]);

  // Poll a running import. Network trouble, 429 and 5xx are retried with backoff; a job the server no longer knows
  // (404 after its TTL or a restart) or refuses ends as failed with a way to start again.
  useEffect(() => {
    if (!job || job.status !== "running") return;
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = (attempt: number) => {
      timer = setTimeout(
        () => {
          client
            .tuyaImport(id, job.id)
            .then((next) => {
              if (!alive) return;
              setJobError("");
              setJob(next);
            })
            .catch((e: unknown) => {
              if (!alive) return;
              const status = e instanceof ApiError ? e.status : 0;
              if (status === 404) return (setJobError(""), setJob({ ...job, status: "failed", error: "job_lost" }));
              if (status >= 400 && status < 500 && status !== 429) return (setJobError(""), setJob({ ...job, status: "failed", error: "job_refused" }));
              if (attempt >= 8) return (setJobError(""), setJob({ ...job, status: "failed", error: "job_unreachable" }));
              setJobError(`อ่านสถานะการนำเข้าไม่สำเร็จ · กำลังลองอีกครั้ง (ครั้งที่ ${attempt + 1})`);
              poll(attempt + 1);
            });
        },
        attempt === 0 ? 1200 : Math.min(15000, 1200 * 2 ** attempt),
      );
    };
    poll(0);
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [job, client, id]);

  // A finished import refreshes the panel (keys, LAN state) and the page (discovery) once.
  const notified = useRef("");
  useEffect(() => {
    if (!job || job.status !== "done" || notified.current === job.id) return;
    notified.current = job.id;
    load();
    onReloadRef.current();
  }, [job, load, onReloadRef]);

  useEffect(() => {
    load();
    const timer = setInterval(load, 15000);
    return () => clearInterval(timer);
  }, [load, refreshKey]);

  // The install code lives 30 minutes: count down while it is shown, then drop it.
  useEffect(() => {
    if (!code) return;
    const expires = Date.parse(code.expires_at);
    const timer = setInterval(() => {
      const t = Date.now();
      setNow(t);
      if (t >= expires) setCode(null);
    }, 1000);
    return () => clearInterval(timer);
  }, [code]);
  const remaining = code ? Math.max(0, Math.round((Date.parse(code.expires_at) - now) / 1000)) : 0;

  const chosenZigbee = zigbeeGateways.find((g) => g.id === zigbeeGateway) ?? zigbeeGateways[0];
  const zigbeeReady = !zigbee || (!!chosenZigbee && IPV4.test(slzbIP.trim()) && (uiIP.trim() === "" || (IPV4.test(uiIP.trim()) && uiIP.trim() !== "0.0.0.0")));
  // The server's command carries no code and a <SLZB_IP> placeholder; the page fills in what was typed.
  const command = code ? code.install_command.replace("<SLZB_IP>", slzbIP.trim()) + (code.zigbee_paired && uiIP.trim() ? ` --zigbee-ui ${uiIP.trim()}` : "") : "";
  const installURL = code?.install_url ?? `${typeof window === "undefined" ? "" : window.location.origin}/edge/install.sh`;
  const updateCommand = `curl -fsSL ${installURL} | sudo sh -s -- --update`;
  const online = status?.state === "online";
  const keys = status?.keys ?? { ok: 0, rejected: 0, suspect: 0, missing: 0 };

  return (
    <section className="topo-edge-panel" aria-label="Aether Edge">
      <ol className="topo-steps">
        <Step done={!!status?.state || !!code} active={!status?.state && !code} label="1 · ติดตั้งบนเครื่องในอาคาร" detail={status?.state ? "ติดตั้งแล้ว" : code ? "รันคำสั่งด้านล่างบน Pi" : "สร้างคำสั่งติดตั้งแล้วรันบน Pi (Raspberry Pi / mini PC ที่มี Docker)"} />
        <Step
          done={online}
          active={!online && (!!status?.state || !!code)}
          label="2 · Aether Edge ออนไลน์"
          detail={status?.state ? `${online ? "ออนไลน์" : "ออฟไลน์"}${status.version ? ` · v${status.version}` : ""} · health ล่าสุด ${ago(status.last_health_at, now)}${status.version && status.latest_version && status.version !== status.latest_version ? ` · มีเวอร์ชันใหม่ ${status.latest_version}` : ""}` : "รอ Aether Edge เชื่อมต่อ"}
        />
        <Step done={(status?.lan_devices ?? 0) > 0} active={online && (status?.lan_devices ?? 0) === 0} label="3 · พบอุปกรณ์ Tuya ใน LAN" detail={status ? `${status.lan_devices} อุปกรณ์ · เชื่อมต่ออยู่ ${status.devices_connected}` : undefined} />
        <Step done={keys.ok > 0} active={(status?.lan_devices ?? 0) > 0 && keys.ok === 0} label="4 · นำเข้าคีย์จาก Tuya" detail={status ? `ใช้ได้ ${keys.ok} · ไม่ตรง ${keys.rejected + keys.suspect} · ยังไม่มี ${keys.missing} · ลงทะเบียนแล้ว ${status.registered}` : undefined} />
      </ol>
      {statusError && <p className="topo-warn">{statusError}</p>}

      {canManage ? (
        <>
          <h3 className="topo-h3">
            <TerminalSquare size={14} /> ติดตั้ง / อัปเดตบน Pi
          </h3>
          {!code && (
            <form
              className="topo-edge-form"
              onSubmit={(e) => {
                e.preventDefault();
                if (!zigbeeReady || creating) return;
                setCreating(true);
                setCodeError("");
                client
                  .edgeInstallCode(id, zigbee ? chosenZigbee?.id : undefined)
                  .then((c) => {
                    setNow(Date.now());
                    setCode(c);
                  })
                  .catch((err: unknown) => setCodeError(err instanceof ApiError && err.status === 400 && zigbee ? "Zigbee2MQTT gateway ที่เลือกใช้ไม่ได้ (ถูกเพิกถอน หรืออยู่คนละ workspace)" : err instanceof Error ? err.message : "สร้างคำสั่งติดตั้งไม่สำเร็จ"))
                  .finally(() => setCreating(false));
              }}
            >
              <label className="topo-check">
                <input type="checkbox" checked={zigbee} onChange={(e) => setZigbee(e.target.checked)} /> ติดตั้ง Zigbee2MQTT ด้วย (ต่อกับ Zigbee coordinator เช่น SLZB-06MU)
              </label>
              {zigbee &&
                (zigbeeGateways.length === 0 ? (
                  <p className="topo-note">ยังไม่มี gateway แบบ Zigbee2MQTT · เพิ่ม gateway &quot;Zigbee2MQTT&quot; จากแถบซ้ายก่อน แล้วกลับมาสร้างคำสั่งติดตั้ง</p>
                ) : (
                  <>
                    <label>
                      Zigbee2MQTT gateway
                      <select value={chosenZigbee?.id ?? ""} onChange={(e) => setZigbeeGateway(e.target.value)}>
                        {zigbeeGateways.map((g) => (
                          <option key={g.id} value={g.id}>
                            {g.name}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      IP ของ SLZB (coordinator)
                      <input value={slzbIP} inputMode="decimal" placeholder="เช่น 192.168.1.40" onChange={(e) => setSlzbIP(e.target.value)} aria-invalid={slzbIP !== "" && !IPV4.test(slzbIP.trim())} />
                    </label>
                    <label>
                      เปิดหน้า Zigbee2MQTT ให้เครื่องใน LAN (ไม่บังคับ)
                      <input value={uiIP} inputMode="decimal" placeholder="IP ของ Pi เช่น 192.168.1.5 · เว้นว่าง = เปิดได้เฉพาะบน Pi" onChange={(e) => setUiIP(e.target.value)} aria-invalid={uiIP !== "" && (!IPV4.test(uiIP.trim()) || uiIP.trim() === "0.0.0.0")} />
                    </label>
                    <small className="topo-note">การติดตั้งจะเปลี่ยนรหัส MQTT ของ Zigbee2MQTT gateway นี้ · ถ้ามี Zigbee2MQTT ตัวเดิมเชื่อมอยู่ที่อื่น ตัวนั้นจะหลุด</small>
                  </>
                ))}
              {codeError && (
                <p className="topo-warn" role="alert">
                  {codeError}
                </p>
              )}
              <div className="topo-actions">
                <button type="submit" className="topo-btn primary" disabled={creating || !zigbeeReady || (zigbee && zigbeeGateways.length === 0)}>
                  <KeyRound size={15} /> {creating ? "กำลังสร้าง…" : "สร้างคำสั่งติดตั้ง"}
                </button>
              </div>
            </form>
          )}
          {code && (
            <div className="topo-secret-box">
              <span className="topo-code-head">
                1 · รันคำสั่งนี้บน Pi (ต้องมี Docker)
                <CopyButton value={command} label="คำสั่งติดตั้ง" onNotice={onNotice} />
              </span>
              <pre className="topo-code">{command}</pre>
              <span className="topo-code-head">
                2 · วางรหัสนี้เมื่อสคริปต์ถาม · ใช้ได้ครั้งเดียว · เหลือ {Math.floor(remaining / 60)}:{String(remaining % 60).padStart(2, "0")} นาที
                <CopyButton value={code.code} label="รหัสติดตั้ง" onNotice={onNotice} />
              </span>
              <pre className="topo-code topo-install-code">{code.code}</pre>
              <small>
                รหัสไม่อยู่ในคำสั่ง จึงไม่ค้างใน history ของ shell · สคริปต์ดาวน์โหลด image {code.image} ก่อน แล้วค่อยถามรหัส
                {code.zigbee_paired ? " · เมื่อติดตั้งเสร็จจะแสดง token ของหน้า Zigbee2MQTT ครั้งเดียว เก็บไว้ใช้ล็อกอิน" : ""}
                {code.zigbee_paired && !uiIP.trim() ? " · หน้า Zigbee2MQTT เปิดได้เฉพาะบน Pi: ใช้ ssh -L 8080:localhost:8080 <ผู้ใช้>@<IP ของ Pi> แล้วเปิด http://localhost:8080" : ""}
              </small>
              <div className="topo-actions">
                <button type="button" className="topo-btn" onClick={() => setCode(null)}>
                  ซ่อนรหัส
                </button>
              </div>
            </div>
          )}
          <span className="topo-code-head">
            อัปเดต Aether Edge (และ Zigbee2MQTT ถ้าติดตั้งไว้) เป็นเวอร์ชันล่าสุด
            <CopyButton value={updateCommand} label="คำสั่งอัปเดต" onNotice={onNotice} />
          </span>
          <pre className="topo-code">{updateCommand}</pre>

          <h3 className="topo-h3">
            <Download size={14} /> อุปกรณ์ Tuya ที่นำเข้า <span className="topo-count">{devices.length}</span>
          </h3>
          <div className="topo-actions">
            <button type="button" className="topo-btn primary" onClick={() => setImportOpen(true)}>
              <KeyRound size={15} /> {job?.status === "running" ? "นำเข้าจาก Tuya · กำลังทำงาน" : "นำเข้าจาก Tuya"}
            </button>
            <button type="button" className="topo-icon-btn" aria-label="รีเฟรช" onClick={load}>
              <RefreshCw size={14} />
            </button>
          </div>
          {devices.length === 0 ? (
            <p className="topo-note">ยังไม่ได้นำเข้า · กด &quot;นำเข้าจาก Tuya&quot; เพื่อดึง local key ครั้งเดียวจาก Tuya IoT Platform</p>
          ) : (
            <ul className="topo-list topo-tuya-list">
              {devices.map((d) => {
                const verdict = tuyaVerdict({ sub: d.sub, local_capable: d.local_capable, has_key: d.key_status !== "missing", lan_seen: d.lan_seen });
                return (
                  <li key={d.tuya_id}>
                    <div className="topo-list-item">
                      <span>
                        <strong>{d.name || d.tuya_id}</strong>
                        <small>
                          {d.tuya_category || "—"} · {d.lan_seen ? `พบที่ ${d.ip || "?"}${d.version ? ` · v${d.version}` : ""}` : "ไม่พบใน LAN"}
                          {!d.registered && d.key_status === "ok" && verdict.warn ? ` · ${verdict.warn}` : ""}
                          {d.available === false && d.reason ? ` · ${REASON_LABEL[d.reason] ?? d.reason}` : ""}
                        </small>
                      </span>
                      <em className={`topo-key is-${d.key_status}`}>{d.key_status === "ok" ? verdict.label : KEY_LABEL[d.key_status]}</em>
                      {d.registered ? (
                        <em className="is-adopted">ลงทะเบียนแล้ว</em>
                      ) : verdict.ok && d.key_status === "ok" ? (
                        <button type="button" className="topo-btn" onClick={() => onRegister(d.tuya_id, d.name)}>
                          ลงทะเบียน
                        </button>
                      ) : null}
                      {d.key_status !== "missing" && (
                        <button type="button" className="topo-icon-btn" aria-label={`ลืมคีย์ของ ${d.name || d.tuya_id}`} onClick={() => setForget(d)}>
                          <Trash2 size={14} />
                        </button>
                      )}
                    </div>
                  </li>
                );
              })}
            </ul>
          )}
          {forget && (
            <div className="topo-callout" role="alertdialog" aria-label="ยืนยันลืมคีย์">
              ลืมคีย์ของ {forget.name || forget.tuya_id}? Aether Edge จะหยุดเชื่อมต่ออุปกรณ์นี้ในรอบถัดไป · นำเข้าจาก Tuya ใหม่ได้ภายหลัง
              <div className="topo-actions">
                <button type="button" className="topo-btn" onClick={() => setForget(null)}>
                  ยกเลิก
                </button>
                <button
                  type="button"
                  className="topo-btn danger"
                  onClick={() => {
                    const target = forget;
                    setForget(null);
                    client
                      .forgetTuyaKey(id, target.tuya_id)
                      .then(() => {
                        onNotice(`ลืมคีย์ของ ${target.name || target.tuya_id} แล้ว`);
                        load();
                        onReload();
                      })
                      .catch((e: unknown) => onNotice(e instanceof Error ? e.message : "ลืมคีย์ไม่สำเร็จ"));
                  }}
                >
                  ลืมคีย์
                </button>
              </div>
            </div>
          )}
          <TuyaImportDialog
            open={importOpen}
            gatewayId={id}
            client={client}
            job={job}
            jobError={jobError}
            devices={devices}
            onJob={(next) => {
              setJobError("");
              setJob(next);
            }}
            onClose={() => setImportOpen(false)}
            onRegister={(tuyaId, name) => {
              setImportOpen(false);
              onRegister(tuyaId, name);
            }}
          />
        </>
      ) : (
        <p className="topo-note">การติดตั้งและนำเข้าคีย์ทำได้เฉพาะ owner หรือ admin</p>
      )}
    </section>
  );
}

/** Key and connection state of one registered Tuya device, for its inspector (owner/admin). */
export function TuyaDeviceStatus({ gatewayId, tuyaId, client, canManage, refreshKey, onNotice }: {
  gatewayId: string;
  tuyaId: string;
  client: EdgeClient;
  canManage: boolean;
  refreshKey?: unknown;
  onNotice: (m: string) => void;
}) {
  const [device, setDevice] = useState<TuyaDevice | null>(null);
  const [confirm, setConfirm] = useState(false);
  // Same rule as EdgePanel: only the latest load may set state, and nothing after unmount.
  const seq = useRef(0);
  useEffect(() => () => void (seq.current += 1), []);
  const load = useCallback(() => {
    if (!canManage) return;
    const n = ++seq.current;
    client
      .tuyaDevices(gatewayId)
      .then((items) => n === seq.current && setDevice(items.find((d) => d.tuya_id === tuyaId.toLowerCase()) ?? null))
      .catch(() => n === seq.current && setDevice(null));
  }, [client, gatewayId, tuyaId, canManage]);
  useEffect(() => {
    load();
  }, [load, refreshKey]);
  if (!canManage || !device) return null;
  return (
    <section className="topo-tuya-status" aria-label="สถานะ Tuya">
      <h3 className="topo-h3">
        <KeyRound size={14} /> Tuya Wi‑Fi (local)
      </h3>
      <p>
        <em className={`topo-key is-${device.key_status}`}>{KEY_LABEL[device.key_status]}</em>
        {device.key_fingerprint && (
          <small>
            {" "}
            ลายนิ้วมือคีย์ <code>{device.key_fingerprint}</code>
          </small>
        )}
      </p>
      <p className="topo-note">
        {device.lan_seen ? `พบใน LAN ที่ ${device.ip || "?"}${device.version ? ` · โปรโตคอล v${device.version}` : ""}` : "Aether Edge ยังไม่พบอุปกรณ์นี้ใน LAN"}
        {device.available === false && device.reason ? ` · ${REASON_LABEL[device.reason] ?? device.reason}` : ""}
      </p>
      {device.key_status !== "missing" &&
        (confirm ? (
          <div className="topo-callout" role="alertdialog" aria-label="ยืนยันลืมคีย์">
            ลืมคีย์ของอุปกรณ์นี้? Aether Edge จะหยุดเชื่อมต่อจนกว่าจะนำเข้าคีย์ใหม่
            <div className="topo-actions">
              <button type="button" className="topo-btn" onClick={() => setConfirm(false)}>
                ยกเลิก
              </button>
              <button
                type="button"
                className="topo-btn danger"
                onClick={() => {
                  setConfirm(false);
                  client
                    .forgetTuyaKey(gatewayId, device.tuya_id)
                    .then(() => {
                      onNotice("ลืมคีย์แล้ว");
                      load();
                    })
                    .catch((e: unknown) => onNotice(e instanceof Error ? e.message : "ลืมคีย์ไม่สำเร็จ"));
                }}
              >
                ลืมคีย์
              </button>
            </div>
          </div>
        ) : (
          <button type="button" className="topo-btn" onClick={() => setConfirm(true)}>
            <Trash2 size={14} /> ลืมคีย์
          </button>
        ))}
    </section>
  );
}
