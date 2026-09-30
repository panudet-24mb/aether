"use client";
import { useState } from "react";
import { Download, ShieldAlert } from "lucide-react";
import { ApiError, type Device } from "./api";

/** What the panel needs from the authenticated client. */
export type PrivacyClient = {
  post: <T>(path: string, body: unknown) => Promise<T>;
  download: (path: string, name: string, body?: unknown) => Promise<void>;
};

type Counts = { samples?: number; ble_history?: number; presence?: number; events?: number; alerts_anonymised?: number };

/**
 * A worn tag's history is personal data about its wearer (PDPA). The owner can hand the wearer a copy (ZIP) and
 * erase it: samples, raw advertisements, zone presence and events go; alerts stay as incident records with the
 * wearer's name taken out. Optionally the registration gets a new name in the same step (a tag handed to someone else).
 */
export default function PrivacyPanel({ registration, client, busy, onNotice, onDone }: { registration: Device; client: PrivacyClient; busy: boolean; onNotice: (m: string) => void; onDone: () => void }) {
  const [confirming, setConfirming] = useState(false);
  const [rename, setRename] = useState("");
  const [tenantWide, setTenantWide] = useState(false);
  const [working, setWorking] = useState(false);
  const [error, setError] = useState("");
  const disabled = busy || working;

  const exportHistory = async () => {
    setWorking(true);
    setError("");
    try {
      await client.download(`/devices/${registration.id}/privacy-export`, `aether-device-${registration.external_id}.zip`, {});
      onNotice("ส่งออกประวัติของผู้สวมใส่แล้ว");
    } catch (e) {
      setError(e instanceof Error ? e.message : "ส่งออกไม่สำเร็จ");
    } finally {
      setWorking(false);
    }
  };
  const erase = async () => {
    setWorking(true);
    setError("");
    try {
      const name = rename.trim();
      const out = await client.post<{ counts: Counts }>(`/devices/${registration.id}/erase-history`, { ...(name ? { name } : {}), tenant_wide: tenantWide });
      const c = out.counts ?? {};
      onNotice(`ลบประวัติแล้ว · ค่าที่วัด ${c.samples ?? 0} · สัญญาณดิบ ${c.ble_history ?? 0} · เหตุการณ์ ${c.events ?? 0} · การแจ้งเตือนที่ลบชื่อออก ${c.alerts_anonymised ?? 0}`);
      setConfirming(false);
      setRename("");
      setTenantWide(false);
      onDone();
    } catch (e) {
      setError(e instanceof ApiError && e.status === 403 ? "เฉพาะเจ้าของ workspace เท่านั้น" : e instanceof Error ? e.message : "ลบไม่สำเร็จ");
    } finally {
      setWorking(false);
    }
  };

  return (
    <section className="topo-privacy" aria-label="ข้อมูลส่วนบุคคลของผู้สวมใส่">
      <h3 className="topo-h3">
        <ShieldAlert size={14} /> ข้อมูลของผู้สวมใส่ (PDPA)
      </h3>
      <p className="topo-note">ประวัติตำแหน่งและค่าที่วัดของแท็กที่มีคนสวมเป็นข้อมูลส่วนบุคคล · ส่งออกให้เจ้าตัวเมื่อขอ และลบเมื่อเลิกใช้หรือเปลี่ยนผู้สวมใส่</p>
      <div className="topo-privacy-actions">
        <button type="button" className="topo-btn" disabled={disabled} onClick={() => void exportHistory()}>
          <Download size={14} /> ส่งออกประวัติ (ZIP)
        </button>
        {!confirming && (
          <button type="button" className="topo-btn danger" disabled={disabled} onClick={() => setConfirming(true)}>
            ลบประวัติ…
          </button>
        )}
      </div>
      {confirming && (
        <div className="topo-warn topo-privacy-confirm" role="alertdialog" aria-label="ยืนยันการลบประวัติ">
          <p>
            ลบถาวร: ค่าที่วัด สัญญาณดิบ ตำแหน่ง (zone) และเหตุการณ์ทั้งหมดของแท็ก <code>{registration.external_id}</code> · การแจ้งเตือน (เช่น SOS) ยังเก็บไว้เป็นบันทึกเหตุการณ์
            แต่ชื่อผู้สวมใส่จะถูกลบออก · สำเนาสำรองข้อมูลอาจยังมีข้อมูลเดิมจนหมดอายุ · ค่าเริ่มต้นลบเฉพาะในโปรเจกต์ของอุปกรณ์นี้
          </p>
          <label className="topo-privacy-wide">
            <input type="checkbox" checked={tenantWide} onChange={(e) => setTenantWide(e.target.checked)} />
            <span>ลบในทุกโปรเจกต์ของ workspace (แท็กเดียวกันที่ gateway ของโปรเจกต์อื่นเคยได้ยิน)</span>
          </label>
          <label className="topo-privacy-rename">
            ชื่อใหม่ของแท็ก (ไม่บังคับ)
            <input value={rename} maxLength={128} onChange={(e) => setRename(e.target.value)} placeholder="เช่น Tag 7 · ว่างไว้เพื่อใช้ชื่อเดิม" />
          </label>
          <div className="topo-privacy-actions">
            <button type="button" className="topo-btn" disabled={working} onClick={() => setConfirming(false)}>
              ยกเลิก
            </button>
            <button type="button" className="topo-btn danger" disabled={disabled} onClick={() => void erase()}>
              ลบประวัติถาวร
            </button>
          </div>
        </div>
      )}
      {error && (
        <p className="topo-warn" role="alert">
          {error}
        </p>
      )}
    </section>
  );
}
