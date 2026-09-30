"use client";
// The owner's twin privacy settings (Privacy Center, docs/platform/digital-twin.md): how the digital twin may show
// people, live and in replay, how far back, and what wall displays may show. GET for everyone, POST owner only.

import { Save } from "lucide-react";
import { useEffect, useState } from "react";
import { ApiError, type Client } from "../topology/api";
import type { TwinSettings } from "./api";

const MODES: { id: TwinSettings["people_replay"]; label: string; note: string }[] = [
  { id: "off", label: "ไม่แสดงคนในการย้อนดู", note: "ภาพสดยังแสดงจำนวนคนต่อโซน" },
  { id: "counts", label: "จำนวนคนต่อโซน", note: "ไม่มีตัวตนใดออกจากเซิร์ฟเวอร์ (ค่าเริ่มต้น)" },
  { id: "tracks", label: "ติดตามแบบไม่ระบุชื่อ", note: "รหัสสุ่มใหม่ทุกครั้งที่โหลด เห็นการย้ายโซนของแต่ละคนภายในช่วงเวลาหนึ่ง" },
  { id: "named", label: "แสดงชื่อ", note: "เฉพาะ owner, admin และ operator · viewer เห็นแบบไม่ระบุชื่อเท่านั้น" },
];

export function TwinSettingsPanel({ client }: { client: Client }) {
  const [s, setS] = useState<TwinSettings | null>(null);
  const [draft, setDraft] = useState<TwinSettings | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState("");

  useEffect(() => {
    let active = true;
    client.raw<TwinSettings>("/twin/settings").then((v) => { if (active) { setS(v); setDraft(v); } }).catch(() => { if (active) setMsg("โหลดการตั้งค่าไม่ได้"); });
    return () => { active = false; };
  }, [client]);

  if (!draft || !s) return <p className="pv-note">{msg || "กำลังโหลด…"}</p>;
  const changed = draft.people_replay !== s.people_replay || draft.people_replay_days !== s.people_replay_days || draft.display_people !== s.display_people;
  const save = async () => {
    setBusy(true);
    setMsg("");
    try {
      const v = await client.post<TwinSettings>("/twin/settings", { people_replay: draft.people_replay, people_replay_days: draft.people_replay_days, display_people: draft.display_people });
      setS(v);
      setDraft(v);
      setMsg("บันทึกแล้ว · การเปลี่ยนแปลงถูกบันทึกในบันทึกการตรวจสอบ");
    } catch (e) {
      setMsg(e instanceof ApiError && e.status === 403 ? "เฉพาะ owner เปลี่ยนการตั้งค่านี้ได้" : "บันทึกไม่ได้");
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="tws" aria-label="การแสดงคนใน Digital twin">
      <p className="pv-note">
        Digital twin ย้อนดูได้ว่าแท็กที่มีคนสวมอยู่โซนไหนเมื่อไร (ระดับโซนจาก gateway ไม่ใช่พิกัดจริง) · นี่คือข้อมูลส่วนบุคคล:
        ใช้เพื่อทบทวนเหตุฉุกเฉินและความปลอดภัย · ระบบเก็บประวัติการย้ายโซนไว้ {s.history_days} วัน (PRESENCE_HISTORY_DAYS) และลบเมื่อลบประวัติของแท็ก ·
        ทุกครั้งที่มีคนย้อนดูคนแบบไม่ระบุชื่อหรือระบุชื่อถูกบันทึกในบันทึกการเข้าถึง{s.demo ? " · workspace สาธิต: ค่าเริ่มต้นแสดงชื่อ (ข้อมูลสมมติ)" : ""}
      </p>
      <fieldset className="tws-group">
        <legend>การแสดงคน (สดและย้อนดู)</legend>
        {MODES.map((m) => (
          <label key={m.id} className="tws-option">
            <input type="radio" name="people_replay" checked={draft.people_replay === m.id} onChange={() => setDraft({ ...draft, people_replay: m.id })} />
            <span><b>{m.label}</b><small>{m.note}</small></span>
          </label>
        ))}
      </fieldset>
      <label className="tws-field">
        ย้อนดูคนได้ไม่เกิน
        <select value={draft.people_replay_days} onChange={(e) => setDraft({ ...draft, people_replay_days: Number(e.target.value) })}>
          {[1, 3, 7, 14, 30, 60, 90].map((d) => <option key={d} value={d} disabled={d > s.history_days}>{d} วัน{d > s.history_days ? " (เกินที่เก็บไว้)" : ""}</option>)}
        </select>
      </label>
      <label className="tws-field">
        จอแสดงผล (TV) แสดงคนได้
        <select value={draft.display_people} onChange={(e) => setDraft({ ...draft, display_people: e.target.value as TwinSettings["display_people"] })}>
          <option value="off">ไม่แสดง (เฉพาะจำนวนในภาพสด)</option>
          <option value="counts">จำนวนคนต่อโซน</option>
          <option value="tracks">ติดตามแบบไม่ระบุชื่อ</option>
        </select>
      </label>
      <div className="tws-actions">
        <button type="button" className="pv-btn" onClick={() => void save()} disabled={busy || !changed}><Save size={14} /> บันทึก</button>
        {s.updated_at && <small className="pv-dim">แก้ไขล่าสุด {new Date(s.updated_at).toLocaleString("th-TH")}</small>}
        {msg && <small role="status">{msg}</small>}
      </div>
    </section>
  );
}
