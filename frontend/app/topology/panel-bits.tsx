"use client";
// Small pieces shared by the inspector panels: copying to the clipboard and the numbered setup steps.
import { useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";

/** Clipboard API needs a secure context; on-prem LAN over plain HTTP falls back to a selection + execCommand copy. */
export async function copyText(value: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(value);
      return true;
    }
  } catch {
    // fall through to the legacy path
  }
  try {
    const area = document.createElement("textarea");
    area.value = value;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    const ok = document.execCommand("copy");
    area.remove();
    return ok;
  } catch {
    return false;
  }
}

export function CopyButton({ value, label, onNotice }: { value: string; label: string; onNotice: (m: string) => void }) {
  const [done, setDone] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);
  return (
    <button
      type="button"
      className="topo-icon-btn"
      aria-label={`คัดลอก ${label}`}
      onClick={() => {
        void copyText(value).then((ok) => {
          if (!ok) {
            onNotice("คัดลอกไม่สำเร็จ · เลือกข้อความแล้วกด Ctrl+C / ⌘C");
            return;
          }
          setDone(true);
          onNotice(`คัดลอก ${label} แล้ว`);
          if (timer.current) clearTimeout(timer.current);
          timer.current = setTimeout(() => setDone(false), 1500);
        });
      }}
    >
      {done ? <Check size={14} /> : <Copy size={14} />}
    </button>
  );
}


export function Step({ done, active, label, detail }: { done: boolean; active?: boolean; label: string; detail?: string }) {
  return (
    <li className={`topo-step ${done ? "is-done" : active ? "is-active" : ""}`}>
      <span className="topo-step-dot">{done ? <Check size={11} /> : null}</span>
      <div>
        <strong>{label}</strong>
        {detail && <small>{detail}</small>}
      </div>
    </li>
  );
}
