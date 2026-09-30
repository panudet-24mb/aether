"use client";
// The twin's 2D side: zone summaries (the accessible data table and the labels read them), the minimap, and the
// plan the twin falls back to when WebGL is unavailable or too slow.

import { useMemo } from "react";
import { COLORS, type Site } from "../floorplan/model";
import type { Layer, TwinState } from "./api";
import { comfortBand, comfortT, rampColor, type P } from "./engine/heat";

export type ZoneSummary = {
  floorId: string;
  floorName: string;
  index: number;
  name: string;
  kind: string;
  points: P[];
  x: number;
  y: number;
  t: number | null;
  h: number | null;
  people: number;
  alerts: number;
  sos: boolean;
  noSensor: boolean;
  /** The comfort band the zone's temperature is coloured against. */
  band: [number, number];
};

function inside(p: P, poly: P[]): boolean {
  let hit = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i++) {
    const [xi, yi] = poly[i], [xj, yj] = poly[j];
    if (yi > p[1] !== yj > p[1] && p[0] < ((xj - xi) * (p[1] - yi)) / (yj - yi) + xi) hit = !hit;
  }
  return hit;
}

const mean = (xs: number[]) => (xs.length ? xs.reduce((s, x) => s + x, 0) / xs.length : null);

/** Per zone: mean temperature and humidity of the sensors inside it, headcount, open alerts, SOS. */
export function zoneSummaries(site: Site, state: TwinState, layers: Layer[]): ZoneSummary[] {
  const counts = new Map(state.presence.counts.map((c) => [c.gateway_id, c.n]));
  const urgent = state.alerts.filter((a) => a.status === "open");
  const out: ZoneSummary[] = [];
  for (const f of site.floors) {
    (f.layout.zones ?? []).forEach((z, index) => {
      if (z.points.length < 3) return;
      const pts = z.points as P[];
      const devs = state.devices.filter((d) => d.kind === "device" && d.floor_id === f.id && inside([d.x, d.y], pts));
      const gws = z.gateway_ids ?? [];
      const x = pts.reduce((s, p) => s + p[0], 0) / pts.length, y = pts.reduce((s, p) => s + p[1], 0) / pts.length;
      const t = mean(devs.map((d) => d.t).filter((v): v is number => typeof v === "number"));
      const h = mean(devs.map((d) => d.h).filter((v): v is number => typeof v === "number"));
      const people = layers.includes("people") ? gws.reduce((n, g) => n + (counts.get(g) ?? 0), 0) : 0;
      const here = urgent.filter((a) => gws.includes(a.gateway_id) || devs.some((d) => d.id === a.device_id));
      out.push({ floorId: f.id, floorName: f.name, index, name: z.name, kind: z.kind, points: pts, x, y, t, h, people, alerts: here.length, sos: here.some((a) => a.sos || a.hazard), noSensor: t === null && h === null, band: comfortBand(z.kind, z.comfort) });
    });
  }
  return out;
}

const rgb = (c: [number, number, number]) => `rgb(${c[0]},${c[1]},${c[2]})`;

function fillOf(z: ZoneSummary, layers: Layer[]): string {
  if (z.sos) return "rgba(255,143,112,.55)";
  if (layers.includes("temperature") && z.t !== null) return rgb(rampColor("temperature", comfortT(z.t, z.band), -1, 1));
  if (layers.includes("humidity") && z.h !== null) return rgb(rampColor("humidity", z.h, 30, 80));
  return "rgba(159,177,182,.18)";
}

/** Plan of one floor as SVG: the 2D fallback of the twin (same data, no WebGL). */
export function TwinPlan2D({ site, floorId, summaries, state, layers }: { site: Site | null; floorId: string; summaries: ZoneSummary[]; state: TwinState | null; layers: Layer[] }) {
  const floor = site?.floors.find((f) => f.id === floorId) ?? site?.floors[0];
  if (!floor) return <div className="twin-plan2d is-empty">กำลังโหลดผัง…</div>;
  const zs = summaries.filter((z) => z.floorId === floor.id);
  const devices = state?.devices.filter((d) => d.floor_id === floor.id) ?? [];
  return (
    <div className="twin-plan2d">
      <p className="twin-plan2d-note">แสดงแบบ 2 มิติ · เบราว์เซอร์นี้แสดง 3 มิติไม่ได้หรือช้าเกินไป</p>
      <svg viewBox={`-1 -1 ${floor.width_m + 2} ${floor.depth_m + 2}`} role="img" aria-label={`ผัง ${floor.name}`}>
        <rect x={0} y={0} width={floor.width_m} height={floor.depth_m} className="twin-plan2d-slab" />
        {zs.map((z) => (
          <g key={z.index}>
            <polygon points={z.points.map((p) => p.join(",")).join(" ")} fill={fillOf(z, layers)} className={`twin-plan2d-zone ${z.sos ? "is-sos" : ""}`} />
            <text x={z.x} y={z.y} className="twin-plan2d-text">{z.name}{z.people ? ` · ${z.people} คน` : ""}</text>
          </g>
        ))}
        {devices.map((d) => (
          <circle key={d.id} cx={d.x} cy={d.y} r={d.kind === "gateway" ? 0.35 : 0.22} className={`twin-plan2d-dev ${d.alert || d.sos ? "is-alert" : d.online ? "" : "is-stale"}`} />
        ))}
      </svg>
    </div>
  );
}

/** Minimap of the floor under the camera: zones, the camera's aim and heading, SOS dots. Click to fly there. */
export function TwinMinimap({ site, view, summaries, onPick }: { site: Site | null; view: { floorId: string; x: number; y: number; heading: number } | null; summaries: ZoneSummary[]; onPick: (floorId: string, x: number, y: number) => void }) {
  const floor = site?.floors.find((f) => f.id === view?.floorId) ?? site?.floors[0];
  const zs = useMemo(() => summaries.filter((z) => z.floorId === floor?.id), [summaries, floor]);
  if (!floor) return null;
  const pick = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    onPick(floor.id, ((e.clientX - r.left) / r.width) * floor.width_m, ((e.clientY - r.top) / r.height) * floor.depth_m);
  };
  const wedge = view ? (() => {
    const len = Math.max(floor.width_m, floor.depth_m) * 0.22, a = view.heading;
    const l = [view.x + Math.cos(a - 0.5) * len, view.y + Math.sin(a - 0.5) * len], r = [view.x + Math.cos(a + 0.5) * len, view.y + Math.sin(a + 0.5) * len];
    return `${view.x},${view.y} ${l.join(",")} ${r.join(",")}`;
  })() : "";
  return (
    <figure className="twin-minimap" aria-label={`แผนที่ย่อ ${floor.name}`}>
      <svg viewBox={`0 0 ${floor.width_m} ${floor.depth_m}`} onClick={pick} role="button" aria-label={`แผนที่ย่อ ${floor.name} · คลิกเพื่อย้ายกล้อง`}>
        <rect x={0} y={0} width={floor.width_m} height={floor.depth_m} className="twin-minimap-slab" />
        {zs.map((z) => <polygon key={z.index} points={z.points.map((p) => p.join(",")).join(" ")} fill={COLORS[(site?.floors.find((f) => f.id === z.floorId)?.layout.zones[z.index]?.color) ?? "slate"] ?? "#9fb1b6"} className="twin-minimap-zone" />)}
        {wedge && <polygon points={wedge} className="twin-minimap-wedge" />}
        {zs.filter((z) => z.sos).map((z) => <circle key={`s${z.index}`} cx={z.x} cy={z.y} r={1.2} className="twin-minimap-sos" />)}
      </svg>
      <figcaption>{floor.name}</figcaption>
    </figure>
  );
}
