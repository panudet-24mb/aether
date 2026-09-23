"use client";
import { useEffect, useState } from "react";
import { BellRing, Check, CircleStop, DoorOpen, Droplets, FlaskConical, GraduationCap, Play, ShieldAlert, Trash2, Waves, X } from "lucide-react";
import { useLatest } from "./use-latest";

/**
 * "สอนสัญญาณให้ระบบ" — teaching Aether what a tag broadcasts when it is triggered.
 *
 * The tags in this workspace do not document their button/tamper encodings, and the physical B10 has
 * never advertised the Eddystone-UID instance change Aether infers a press from. Instead of guessing
 * a byte layout, the operator records the device twice: once at rest, once while triggering it. The
 * server diffs the two captures out of the raw advertisement archive and proposes matchers, each
 * explained in words the operator can check against the device in their hand.
 */

export type SignalMatcher = { kind: "frame" | "byte" | "prefix"; frame?: string; offset?: number; mask?: number; value?: number; prefix?: string };
export type SignalCandidate = { matcher: SignalMatcher; description: string; trigger_hits: number; confidence: "high" | "medium" | "low" };
export type LearnedSignal = { id: string; scope: "device" | "profile"; external_id?: string; profile_id?: string; event_type: string; matcher: SignalMatcher; description: string; created_at: string };
type Phase = { observations: number; distinct: number; from: string; until: string; active: boolean };
export type SignalSession = {
  id: string;
  gateway_id: string;
  external_id: string;
  event_type: string;
  label: string;
  status: "baseline" | "trigger" | "finished" | "confirmed" | "cancelled";
  started_at: string;
  baseline: Phase;
  trigger: Phase;
  candidates: SignalCandidate[];
  verdict: string;
};
type SignalTest = { uplinks: number; matches: number; since: string; last_match?: string; verdict: string };

/** The minimal slice of the topology API client this panel needs (see api.ts createClient). */
export type SignalClient = {
  raw: <T>(path: string) => Promise<T>;
  post: <T>(path: string, body: unknown) => Promise<T>;
};

const EVENT_TYPES: { id: string; label: string; hint: string }[] = [
  { id: "button", label: "กดปุ่ม", hint: "ปุ่มฉุกเฉิน / nurse call · แจ้งทุกครั้งที่กด" },
  { id: "tamper", label: "ถูกงัดแงะ", hint: "ป้ายถูกถอดหรือแกะออกจากของ" },
  { id: "leak", label: "น้ำรั่ว", hint: "เซ็นเซอร์สัมผัสน้ำ" },
  { id: "motion", label: "เคลื่อนไหว", hint: "เริ่มขยับหรือถูกยกขึ้น" },
  { id: "door", label: "ประตู เปิด/ปิด", hint: "เซ็นเซอร์ประตูหรือหน้าต่าง · บันทึกเปิดประตู / ปิดประตู" },
  { id: "custom", label: "อื่น ๆ", hint: "บันทึกลง event log อย่างเดียว · กฎแจ้งเตือนยังเลือกไม่ได้" },
];

const PHASE_SEC = 20;

/** The Thai instruction for phase 2, matched to what is being taught. */
function triggerInstruction(eventType: string): string {
  switch (eventType) {
    case "button":
      return "กดปุ่มย้ำ ๆ ตลอดช่วงนี้";
    case "tamper":
      return "งัดหรือถอดป้ายออกย้ำ ๆ ตลอดช่วงนี้";
    case "leak":
      return "จุ่มหรือแตะน้ำที่เซ็นเซอร์ตลอดช่วงนี้";
    case "motion":
      return "เขย่าหรือยกอุปกรณ์ตลอดช่วงนี้";
    case "door":
      return "เปิดแล้วปิดประตู (แยกแม่เหล็กออกจากตัวเซ็นเซอร์) ย้ำ ๆ ตลอดช่วงนี้";
    default:
      return "ทำสิ่งที่ต้องการสอนย้ำ ๆ ตลอดช่วงนี้";
  }
}

function eventLabel(eventType: string): string {
  return EVENT_TYPES.find((t) => t.id === eventType)?.label ?? eventType;
}

function EventIcon({ eventType, size = 14 }: { eventType: string; size?: number }) {
  if (eventType === "tamper") return <ShieldAlert size={size} />;
  if (eventType === "leak") return <Droplets size={size} />;
  if (eventType === "motion") return <Waves size={size} />;
  if (eventType === "door") return <DoorOpen size={size} />;
  return <BellRing size={size} />;
}

function secondsLeft(until: string, now: number): number {
  return Math.max(0, Math.ceil((Date.parse(until) - now) / 1000));
}

/** Reports whether a learned signal applies to a device, so the caller can show a badge. */
export function signalsForDevice(signals: LearnedSignal[], external: string, profileIds: string[]): LearnedSignal[] {
  const mac = external.toLowerCase();
  return signals.filter((s) => (s.scope === "device" ? s.external_id?.toLowerCase() === mac : !!s.profile_id && profileIds.includes(s.profile_id)));
}

export type SignalPanelProps = {
  external: string;
  /** The gateway that hears this tag; the session records through it. */
  gatewayId: string | null;
  /** Profiles this identity is registered with, for the "ใช้กับทุกตัวที่เป็นรุ่นนี้" option. */
  profileIds: string[];
  client: SignalClient;
  busy: boolean;
  onNotice: (message: string) => void;
  /** Reports the signals that apply to this device, so the inspector can badge it. */
  onApplies?: (applied: LearnedSignal[]) => void;
};

export default function SignalPanel({ external, gatewayId, profileIds, client, busy, onNotice, onApplies }: SignalPanelProps) {
  const [signals, setSignals] = useState<LearnedSignal[]>([]);
  const [session, setSession] = useState<SignalSession | null>(null);
  const [eventType, setEventType] = useState("button");
  const [label, setLabel] = useState("");
  const [scope, setScope] = useState<"device" | "profile">("device");
  const [chosen, setChosen] = useState(0);
  const [error, setError] = useState("");
  const [working, setWorking] = useState(false);
  const [tested, setTested] = useState<Record<string, SignalTest>>({});
  const [now, setNow] = useState(() => Date.now());
  // Teaching, confirming and deleting are owner/admin (the server enforces it); a viewer sees the
  // list so they can tell why a device raises an event, but none of the controls.
  const [canManage, setCanManage] = useState(false);

  const clientRef = useLatest(client);
  const noticeRef = useLatest(onNotice);
  const appliesRef = useLatest(onApplies);
  const profilesRef = useLatest(profileIds);
  // Announcing the applicable signals happens where the list actually changes, never during render.
  const announce = (list: LearnedSignal[]) => appliesRef.current?.(signalsForDevice(list, external, profilesRef.current));
  const sessionId = session?.id ?? "";
  const open = session !== null && (session.status === "baseline" || session.status === "trigger");

  // The list of learned signals, reloaded whenever this panel is shown for another device.
  useEffect(() => {
    let stopped = false;
    const load = async () => {
      try {
        const [list, me] = await Promise.all([clientRef.current.raw<{ items: LearnedSignal[] }>("/signals"), clientRef.current.raw<{ role: string }>("/me")]);
        if (stopped) return;
        setSignals(list.items ?? []);
        appliesRef.current?.(signalsForDevice(list.items ?? [], external, profilesRef.current));
        setCanManage(me.role === "owner" || me.role === "admin");
      } catch {
        if (!stopped) setError("โหลดรายการสัญญาณที่สอนไว้ไม่สำเร็จ");
      }
    };
    void load();
    return () => {
      stopped = true;
    };
  }, [external, clientRef, appliesRef, profilesRef]);

  // A short poll drives the countdown and the live per-phase counters. It runs only while a session
  // is open, so the page's normal refresh cadence is untouched the rest of the time.
  useEffect(() => {
    if (!open || !sessionId) return;
    let stopped = false;
    const tick = async () => {
      setNow(Date.now());
      try {
        const fresh = await clientRef.current.raw<SignalSession>(`/signals/sessions/${sessionId}`);
        if (!stopped) setSession(fresh);
      } catch {
        // A single failed poll is not worth an error banner; the next tick retries.
      }
    };
    const timer = setInterval(() => void tick(), 1500);
    return () => {
      stopped = true;
      clearInterval(timer);
    };
  }, [open, sessionId, clientRef]);

  const run = async (what: () => Promise<void>) => {
    setWorking(true);
    setError("");
    try {
      await what();
    } catch {
      setError("คำสั่งไม่สำเร็จ · ลองใหม่อีกครั้ง");
    } finally {
      setWorking(false);
    }
  };

  const start = () =>
    run(async () => {
      if (!gatewayId) return;
      const fresh = await clientRef.current.post<SignalSession>("/signals/sessions", {
        gateway_id: gatewayId,
        external_id: external,
        event_type: eventType,
        label,
        baseline_sec: PHASE_SEC,
        trigger_sec: PHASE_SEC,
      });
      setChosen(0);
      setScope("device");
      setSession(fresh);
      setNow(Date.now());
    });

  const advance = () =>
    run(async () => {
      if (!session) return;
      setSession(await clientRef.current.post<SignalSession>(`/signals/sessions/${session.id}/advance`, {}));
      setNow(Date.now());
    });

  const cancel = () =>
    run(async () => {
      if (!session) return;
      await clientRef.current.post<void>(`/signals/sessions/${session.id}/cancel`, {});
      setSession(null);
    });

  const confirm = () =>
    run(async () => {
      if (!session) return;
      const saved = await clientRef.current.post<LearnedSignal>(`/signals/sessions/${session.id}/confirm`, { candidate_index: chosen, scope });
      setSignals((list) => {
        const next = [saved, ...list];
        announce(next);
        return next;
      });
      setSession(null);
      noticeRef.current(`บันทึกสัญญาณแล้ว · ${eventLabel(saved.event_type)} จะแจ้งเตือนเมื่อพบรูปแบบนี้`);
    });

  const remove = (id: string) =>
    run(async () => {
      await clientRef.current.post<void>(`/signals/${id}/delete`, {});
      setSignals((list) => {
        const next = list.filter((s) => s.id !== id);
        announce(next);
        return next;
      });
      noticeRef.current("ลบสัญญาณที่สอนไว้แล้ว");
    });

  const test = (id: string) =>
    run(async () => {
      const out = await clientRef.current.post<SignalTest>(`/signals/${id}/test`, {});
      setTested((map) => ({ ...map, [id]: out }));
    });

  const mine = signalsForDevice(signals, external, profileIds);
  const phase = session?.status === "trigger" ? session.trigger : session?.baseline;
  const left = session && phase ? secondsLeft(phase.until, now) : 0;

  return (
    <section className="topo-signals">
      <h3 className="topo-h3">
        <GraduationCap size={14} /> สัญญาณที่สอนไว้
      </h3>

      {mine.length > 0 && (
        <ul className="topo-list">
          {mine.map((s) => (
            <li key={s.id} className="topo-signal">
              <div className="topo-signal-head">
                <span className={`topo-chip is-${s.event_type}`}>
                  <EventIcon eventType={s.event_type} size={12} /> {eventLabel(s.event_type)}
                </span>
                <small>{s.scope === "profile" ? `ทุกตัวที่เป็น ${s.profile_id}` : "เฉพาะอุปกรณ์นี้"}</small>
                {canManage && (
                  <button type="button" className="topo-icon-btn is-danger" disabled={busy || working} aria-label="ลบสัญญาณนี้" title="ลบสัญญาณนี้" onClick={() => void remove(s.id)}>
                    <Trash2 size={13} />
                  </button>
                )}
              </div>
              <p className="topo-signal-why">{s.description}</p>
              {canManage && (
                <button type="button" className="topo-btn is-small" disabled={busy || working} onClick={() => void test(s.id)}>
                  <FlaskConical size={13} /> ทดสอบกับข้อมูลย้อนหลัง
                </button>
              )}
              {tested[s.id] && (
                <p className="topo-signal-test">
                  <strong>
                    ตรง {tested[s.id].matches} จาก {tested[s.id].uplinks} uplink ใน 10 นาทีล่าสุด
                  </strong>
                  <span>{tested[s.id].verdict}</span>
                </p>
              )}
            </li>
          ))}
        </ul>
      )}

      {mine.length === 0 && !session && <p className="topo-note">ยังไม่ได้สอนสัญญาณให้อุปกรณ์นี้ · ถ้ากดปุ่มแล้วไม่มีอะไรเกิดขึ้น ให้สอนระบบว่าอุปกรณ์ส่งอะไรตอนกด</p>}

      {error && <p className="topo-signal-error">{error}</p>}

      {/* Setup: what are we teaching? */}
      {canManage && !session && (
        <div className="topo-signal-setup">
          <label className="topo-signal-field">
            <span>สอนสัญญาณอะไร</span>
            <select value={eventType} disabled={busy || working} onChange={(e) => setEventType(e.target.value)}>
              {EVENT_TYPES.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.label}
                </option>
              ))}
            </select>
          </label>
          <p className="topo-note">{EVENT_TYPES.find((t) => t.id === eventType)?.hint}</p>
          <label className="topo-signal-field">
            <span>ชื่อกำกับ</span>
            <input type="text" value={label} maxLength={64} placeholder="เช่น ปุ่มฉุกเฉินหัวเตียง" disabled={busy || working} onChange={(e) => setLabel(e.target.value)} />
          </label>
          <button type="button" className="topo-btn primary" disabled={busy || working || !gatewayId} onClick={() => void start()}>
            <Play size={15} /> เริ่มสอนสัญญาณ
          </button>
          {!gatewayId && <p className="topo-note">ยังไม่มี gateway ที่ได้ยินอุปกรณ์นี้ · รอให้ gateway รับสัญญาณก่อน</p>}
        </div>
      )}

      {/* The two counted-down phases, with live counters so the operator can see whether anything
          is reaching the gateway at all before the analysis says "no difference". */}
      {session && open && phase && (
        <div className="topo-signal-run">
          <div className="topo-signal-phase">
            <span className="topo-signal-step">{session.status === "baseline" ? "ขั้นที่ 1 · ปล่อยนิ่ง" : "ขั้นที่ 2 · กระตุ้น"}</span>
            <strong>{session.status === "baseline" ? "วางอุปกรณ์ไว้เฉย ๆ อย่าแตะ" : triggerInstruction(session.event_type)}</strong>
            <span className="topo-signal-count">{left}</span>
          </div>
          <dl className="topo-signal-meters">
            <div>
              <dt>ปล่อยนิ่ง</dt>
              <dd>
                {session.baseline.observations} ครั้ง · {session.baseline.distinct} รูปแบบ
              </dd>
            </div>
            <div>
              <dt>ตอนกระตุ้น</dt>
              <dd>
                {session.trigger.observations} ครั้ง · {session.trigger.distinct} รูปแบบ
              </dd>
            </div>
          </dl>
          {session.status === "baseline" && session.baseline.observations === 0 && <p className="topo-note">ยังไม่มีสัญญาณเข้ามาเลย · ตรวจว่าอุปกรณ์อยู่ในระยะของ gateway</p>}
          <div className="topo-actions">
            {session.status === "baseline" && (
              <button type="button" className="topo-btn primary" disabled={working} onClick={() => void advance()}>
                <Play size={15} /> พร้อมแล้ว เริ่มกระตุ้นเลย
              </button>
            )}
            <button type="button" className="topo-btn danger" disabled={working} onClick={() => void cancel()}>
              <CircleStop size={15} /> ยกเลิก
            </button>
          </div>
        </div>
      )}

      {/* Finished: the ranked candidates, or the honest empty answer. */}
      {session?.status === "finished" && (
        <div className="topo-signal-result">
          {session.candidates.length === 0 ? (
            <>
              <p className="topo-signal-empty">{session.verdict}</p>
              <div className="topo-actions">
                <button type="button" className="topo-btn" disabled={working} onClick={() => void cancel()}>
                  <X size={15} /> ปิด
                </button>
              </div>
            </>
          ) : (
            <>
              <p className="topo-note">เลือกสิ่งที่ตรงกับที่ทำจริง · ระบบเรียงจากที่มั่นใจที่สุด</p>
              <ul className="topo-list">
                {session.candidates.map((c, i) => (
                  <li key={`${c.matcher.kind}-${c.matcher.frame ?? c.matcher.prefix}-${c.matcher.offset ?? 0}-${c.matcher.value ?? 0}`}>
                    <label className={`topo-signal-candidate${chosen === i ? " is-chosen" : ""}`}>
                      <input type="radio" name="signal-candidate" checked={chosen === i} disabled={working} onChange={() => setChosen(i)} />
                      <span>
                        <strong>{c.description}</strong>
                        <small>{c.confidence === "high" ? "มั่นใจสูง · เป็นเฟรมใหม่ทั้งเฟรม" : c.confidence === "medium" ? "มั่นใจปานกลาง · ไบต์เดียวที่เปลี่ยน" : "มั่นใจต่ำ · เทียบจากสัญญาณดิบ"}</small>
                      </span>
                    </label>
                  </li>
                ))}
              </ul>
              {profileIds.length > 0 && (
                <label className="topo-check">
                  <input type="checkbox" checked={scope === "profile"} disabled={working} onChange={(e) => setScope(e.target.checked ? "profile" : "device")} />
                  <span>
                    <strong>ใช้กับทุกตัวที่เป็นรุ่นนี้</strong>
                    <small>สอนตัวเดียว ใช้ได้กับอุปกรณ์ทุกตัวที่ลงทะเบียนเป็น {profileIds[0]}</small>
                  </span>
                </label>
              )}
              <div className="topo-actions">
                <button type="button" className="topo-btn primary" disabled={working} onClick={() => void confirm()}>
                  <Check size={15} /> ยืนยันและใช้งาน
                </button>
                <button type="button" className="topo-btn danger" disabled={working} onClick={() => void cancel()}>
                  <X size={15} /> ทิ้งผลนี้
                </button>
              </div>
            </>
          )}
        </div>
      )}
    </section>
  );
}
