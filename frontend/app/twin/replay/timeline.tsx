"use client";
// The replay's time track (docs/platform/digital-twin.md, phase P3): density lanes (alerts, doors, people moving),
// SOS / hazard markers, shaded ranges with no history, the playhead; play at 1×/10×/60×; range presets. A keyboard
// slider: Space plays, ←/→ one bucket, Shift+←/→ one hour, [ ] speed, Home/End the ends.

import { Pause, Play, Radio } from "lucide-react";
import { useCallback, useEffect, useRef } from "react";
import type { TwinMarker, TwinTimeline } from "../api";

export const RANGES = [
  { id: "1h", label: "1 ชม.", ms: 3600e3 },
  { id: "6h", label: "6 ชม.", ms: 6 * 3600e3 },
  { id: "24h", label: "24 ชม.", ms: 24 * 3600e3 },
  { id: "7d", label: "7 วัน", ms: 7 * 24 * 3600e3 },
] as const;
export type RangeId = (typeof RANGES)[number]["id"];
export const SPEEDS = [1, 10, 60] as const;

export const thaiTime = (ms: number) =>
  new Date(ms).toLocaleString("th-TH", { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", second: "2-digit" });

type Props = {
  timeline: TwinTimeline | null;
  from: number;
  to: number;
  t: number;
  playing: boolean;
  speed: number;
  range: RangeId;
  busy?: boolean;
  onSeek: (t: number) => void;
  onPlay: (playing: boolean) => void;
  onSpeed: (speed: number) => void;
  onRange: (range: RangeId) => void;
  onMarker: (m: TwinMarker) => void;
  onLive: () => void;
};

function css(name: string, fallback: string): string {
  if (typeof window === "undefined") return fallback;
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return v || fallback;
}

export default function ReplayTimeline(p: Props) {
  const canvas = useRef<HTMLCanvasElement | null>(null);
  const track = useRef<HTMLDivElement | null>(null);
  const span = Math.max(1, p.to - p.from);
  const xOf = useCallback((ms: number, w: number) => ((ms - p.from) / span) * w, [p.from, span]);

  // One pass: no-data shading, the three density lanes, markers, the playhead.
  useEffect(() => {
    const c = canvas.current;
    if (!c) return;
    const draw = () => {
      const dpr = Math.min(2, window.devicePixelRatio || 1);
      const w = c.clientWidth, h = c.clientHeight;
      if (c.width !== Math.round(w * dpr) || c.height !== Math.round(h * dpr)) { c.width = Math.round(w * dpr); c.height = Math.round(h * dpr); }
      const g = c.getContext("2d");
      if (!g) return;
      g.setTransform(dpr, 0, 0, dpr, 0, 0);
      g.clearRect(0, 0, w, h);
      const accent = css("--color-accent", "#5eead4"), coral = css("--color-danger", "#ff7a6b"), amber = css("--color-warning", "#f5b942"), muted = css("--color-border", "#2a3a36");
      const tl = p.timeline;
      // No history before what the database still holds.
      const starts = [tl?.available.env_from, tl?.available.markers_from].filter(Boolean).map((s) => Date.parse(s as string));
      const earliest = starts.length ? Math.min(...starts) : p.from;
      if (earliest > p.from) {
        g.fillStyle = "rgba(128,128,128,0.18)";
        g.fillRect(0, 0, Math.max(0, xOf(earliest, w)), h);
      }
      if (tl?.available.people_from) {
        const pf = Date.parse(tl.available.people_from);
        if (pf > p.from) {
          g.fillStyle = "rgba(128,128,128,0.10)";
          g.fillRect(0, 0, Math.max(0, xOf(pf, w)), h * 0.34);
        }
      }
      const lanes: [keyof TwinTimeline["density"]["lanes"], string, number][] = [["zone", accent, 0], ["door", amber, 1], ["alerts", coral, 2]];
      const laneH = (h - 14) / 3;
      if (tl) {
        const bw = (tl.density.bucket_sec * 1000 / span) * w;
        const x0 = xOf(Date.parse(tl.density.from), w);
        for (const [lane, color, row] of lanes) {
          const xs = tl.density.lanes[lane] ?? [];
          const max = Math.max(1, ...xs);
          g.fillStyle = color;
          xs.forEach((n, i) => {
            if (!n) return;
            const bh = Math.max(2, (n / max) * (laneH - 3));
            g.globalAlpha = 0.35 + 0.5 * (n / max);
            g.fillRect(x0 + i * bw, row * laneH + (laneH - bh), Math.max(1, bw - 1), bh);
          });
          g.globalAlpha = 1;
        }
        for (const m of tl.markers) {
          const x = xOf(Date.parse(m.at), w);
          g.fillStyle = m.kind === "alert" ? amber : coral;
          g.beginPath();
          g.moveTo(x, h - 13);
          g.lineTo(x - 5, h - 3);
          g.lineTo(x + 5, h - 3);
          g.closePath();
          g.fill();
        }
      }
      g.fillStyle = muted;
      g.fillRect(0, h - 14, w, 1);
      const px = xOf(p.t, w);
      g.fillStyle = accent;
      g.fillRect(px - 1, 0, 2, h);
    };
    draw();
    const ro = new ResizeObserver(draw);
    ro.observe(c);
    return () => ro.disconnect();
  }, [p.timeline, p.from, p.to, p.t, span, xOf]);

  const seekAt = (clientX: number) => {
    const el = track.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    const ms = p.from + ((clientX - r.left) / r.width) * span;
    // A click near a marker jumps to it (and flies there).
    const near = p.timeline?.markers.find((m) => Math.abs(xOf(Date.parse(m.at), r.width) - (clientX - r.left)) <= 6);
    if (near) { p.onMarker(near); return; }
    p.onSeek(Math.max(p.from, Math.min(p.to, ms)));
  };

  const bucket = (p.timeline?.density.bucket_sec ?? 300) * 1000;
  const onKey = (e: React.KeyboardEvent) => {
    const step = e.shiftKey ? 3600e3 : bucket;
    switch (e.key) {
      case " ": e.preventDefault(); p.onPlay(!p.playing); break;
      case "ArrowLeft": e.preventDefault(); p.onSeek(Math.max(p.from, p.t - step)); break;
      case "ArrowRight": e.preventDefault(); p.onSeek(Math.min(p.to, p.t + step)); break;
      case "Home": e.preventDefault(); p.onSeek(p.from); break;
      case "End": e.preventDefault(); p.onSeek(p.to); break;
      case "[": { e.preventDefault(); const i = SPEEDS.indexOf(p.speed as (typeof SPEEDS)[number]); p.onSpeed(SPEEDS[Math.max(0, i - 1)]); break; }
      case "]": { e.preventDefault(); const i = SPEEDS.indexOf(p.speed as (typeof SPEEDS)[number]); p.onSpeed(SPEEDS[Math.min(SPEEDS.length - 1, i + 1)]); break; }
    }
  };

  return (
    <div className="twin-replay" aria-label="ย้อนดูเหตุการณ์">
      <div className="twin-replay-controls">
        <button type="button" className="topo-btn" onClick={() => p.onPlay(!p.playing)} aria-label={p.playing ? "หยุด" : "เล่น"} title={p.playing ? "หยุด (Space)" : "เล่น (Space)"}>
          {p.playing ? <Pause size={14} /> : <Play size={14} />}
        </button>
        <div className="twin-seg" role="group" aria-label="ความเร็ว">
          {SPEEDS.map((s) => <button key={s} type="button" className={p.speed === s ? "is-on" : ""} aria-pressed={p.speed === s} onClick={() => p.onSpeed(s)}>{s}×</button>)}
        </div>
        <div className="twin-seg" role="group" aria-label="ช่วงเวลา">
          {RANGES.map((r) => <button key={r.id} type="button" className={p.range === r.id ? "is-on" : ""} aria-pressed={p.range === r.id} onClick={() => p.onRange(r.id)}>{r.label}</button>)}
        </div>
        <output className="twin-replay-time" aria-live="off">{thaiTime(p.t)}</output>
        {p.busy && <span className="twin-replay-busy">กำลังโหลด…</span>}
        <button type="button" className="topo-btn twin-replay-live" onClick={p.onLive} title="กลับไปดูสด"><Radio size={13} /> สด</button>
      </div>
      <div
        ref={track}
        className="twin-replay-track"
        role="slider"
        tabIndex={0}
        aria-label="เวลาที่ย้อนดู"
        aria-valuemin={p.from}
        aria-valuemax={p.to}
        aria-valuenow={Math.round(p.t)}
        aria-valuetext={thaiTime(p.t)}
        onKeyDown={onKey}
        onPointerDown={(e) => { (e.target as HTMLElement).setPointerCapture?.(e.pointerId); seekAt(e.clientX); }}
        onPointerMove={(e) => { if (e.buttons === 1) seekAt(e.clientX); }}
      >
        <canvas ref={canvas} aria-hidden="true" />
        <div className="twin-replay-lanes" aria-hidden="true"><span>คนย้ายโซน</span><span>ประตู</span><span>การแจ้งเตือน</span></div>
      </div>
      <div className="twin-replay-ends" aria-hidden="true"><span>{thaiTime(p.from)}</span><span>{thaiTime(p.to)}</span></div>
    </div>
  );
}
