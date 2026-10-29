"use client";
import { memo, useRef, useState, type PointerEvent as ReactPointerEvent } from "react";
import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, Position, useReactFlow, type Edge, type EdgeProps, type XYPosition } from "@xyflow/react";
import { RotateCcw } from "lucide-react";

// A connection line that can be rerouted, with horizontal and vertical segments only. In edit mode a selected line
// shows a handle on each of its segments: a vertical segment slides left and right, a horizontal one up and down, and
// the corners around it follow. The line always leaves and enters its nodes straight out of their handles (a short
// stub), so it never comes loose. Without bend points it is drawn exactly like React Flow's smoothstep edge. Bend
// points are shared by the workspace (migration 00053).

export const MAX_ROUTE_POINTS = 16;
/** Length of the straight piece out of a node's handle, as in smoothstep. */
const STUB = 20;
const RADIUS = 5;

export type RoutedData = {
  points: XYPosition[];
  /** Segment handles are shown: the board is in edit mode and the line is selected. */
  editing: boolean;
  /** Stable callback (an empty list straightens the line). */
  onRoute: (edge: string, points: XYPosition[]) => void;
};
export type RoutedEdgeType = Edge<RoutedData, "routed">;

const round = (v: number) => Math.round(v * 100) / 100;
const same = (a: number, b: number) => Math.abs(a - b) < 0.5;
const vertical = (pos: Position) => pos === Position.Top || pos === Position.Bottom;
const stub = (p: XYPosition, pos: Position): XYPosition =>
  pos === Position.Top ? { x: p.x, y: p.y - STUB } : pos === Position.Bottom ? { x: p.x, y: p.y + STUB } : pos === Position.Left ? { x: p.x - STUB, y: p.y } : { x: p.x + STUB, y: p.y };

/** Drops repeated points and the middle of three points on one line, keeping both ends. */
function simplify(pts: XYPosition[]): XYPosition[] {
  const out: XYPosition[] = [];
  for (const p of pts) {
    const last = out[out.length - 1];
    if (last && same(last.x, p.x) && same(last.y, p.y)) continue;
    const prev = out[out.length - 2];
    if (prev && last && ((same(prev.x, last.x) && same(last.x, p.x)) || (same(prev.y, last.y) && same(last.y, p.y)))) out.pop();
    out.push(p);
  }
  return out;
}

/** Joins the points with horizontal and vertical segments only: where two points are not in line, a corner is added
 *  so the line turns at a right angle, carrying on perpendicular to the segment before it. */
function orthogonal(pts: XYPosition[], firstVertical: boolean): XYPosition[] {
  const out = [pts[0]];
  let isVertical = firstVertical;
  for (let i = 1; i < pts.length; i++) {
    const a = out[out.length - 1], b = pts[i];
    if (same(a.x, b.x) || same(a.y, b.y)) {
      if (!(same(a.x, b.x) && same(a.y, b.y))) isVertical = same(a.x, b.x);
      out.push(b);
      continue;
    }
    // Turn first: after a vertical segment go across, after a horizontal one go up or down.
    out.push(isVertical ? { x: b.x, y: a.y } : { x: a.x, y: b.y }, b);
  }
  return simplify(out);
}

/** The full line: source, its stub, the bend points, the target's stub, target. */
function fullPath(source: XYPosition, sourcePos: Position, points: XYPosition[], target: XYPosition, targetPos: Position): XYPosition[] {
  return orthogonal([source, stub(source, sourcePos), ...points, stub(target, targetPos), target], vertical(sourcePos));
}

/** Bend points that reproduce the smoothstep line, so a line without a route can be edited from what is on screen. */
function defaultPoints(source: XYPosition, sourcePos: Position, target: XYPosition, targetPos: Position): XYPosition[] {
  const s = stub(source, sourcePos), t = stub(target, targetPos);
  if (vertical(sourcePos)) {
    const y = round((s.y + t.y) / 2);
    return [{ x: s.x, y }, { x: t.x, y }];
  }
  const x = round((s.x + t.x) / 2);
  return [{ x, y: s.y }, { x, y: t.y }];
}

/** Direction of a segment along one axis. Within half a unit counts as none, as in `same`: a node handle sits at
 *  fractional coordinates while saved corners keep two decimals, and taking the sign of that sliver would push the
 *  rounded corner sideways and slant the whole segment. */
const dir = (v: number) => (Math.abs(v) < 0.5 ? 0 : Math.sign(v));

/** SVG path through the points with small rounded corners, like smoothstep. */
function svgPath(pts: XYPosition[]): string {
  let d = `M ${pts[0].x} ${pts[0].y}`;
  for (let i = 1; i < pts.length - 1; i++) {
    const a = pts[i - 1], p = pts[i], b = pts[i + 1];
    const r = Math.min(RADIUS, Math.hypot(p.x - a.x, p.y - a.y) / 2, Math.hypot(b.x - p.x, b.y - p.y) / 2);
    const inX = p.x - dir(p.x - a.x) * r, inY = p.y - dir(p.y - a.y) * r;
    const outX = p.x + dir(b.x - p.x) * r, outY = p.y + dir(b.y - p.y) * r;
    d += ` L ${inX} ${inY} Q ${p.x} ${p.y} ${outX} ${outY}`;
  }
  const last = pts[pts.length - 1];
  return `${d} L ${last.x} ${last.y}`;
}

export const RoutedEdge = memo(function RoutedEdge(props: EdgeProps<RoutedEdgeType>) {
  const { id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, markerEnd, style, label, data } = props;
  const { screenToFlowPosition } = useReactFlow();
  const source = { x: sourceX, y: sourceY }, target = { x: targetX, y: targetY };
  // While a segment is dragged the line follows locally; the result is saved once, on release.
  const [dragging, setDragging] = useState<XYPosition[] | null>(null);
  const drag = useRef<{ path: XYPosition[]; from: number; to: number; isVertical: boolean } | null>(null);
  const stored = data?.points ?? [];
  const routed = dragging !== null || stored.length > 0;

  let path: string, labelX: number, labelY: number, line: XYPosition[];
  if (!routed) {
    [path, labelX, labelY] = getSmoothStepPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition });
    line = fullPath(source, sourcePosition, defaultPoints(source, sourcePosition, target, targetPosition), target, targetPosition);
  } else {
    line = dragging ?? fullPath(source, sourcePosition, stored, target, targetPosition);
    path = svgPath(line);
    const mid = Math.floor((line.length - 1) / 2);
    labelX = (line[mid].x + line[mid + 1].x) / 2;
    labelY = (line[mid].y + line[mid + 1].y) / 2;
  }

  // The drag is followed on the window: starting it can add corners, which re-keys the handles under the pointer.
  const begin = (e: ReactPointerEvent<HTMLElement>, k: number) => {
    e.stopPropagation();
    e.preventDefault();
    const onMove = (ev: PointerEvent) => move(ev);
    const onUp = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
      end();
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
    const p = line.map((q) => ({ ...q }));
    let from = k, to = k + 1;
    // A segment touching a stub moves away from it through a new corner, so the stub stays on its handle.
    if (from === 1) {
      p.splice(2, 0, { ...p[1] });
      from = 2;
      to = 3;
    }
    if (to === p.length - 2) p.splice(to, 0, { ...p[to] });
    drag.current = { path: p, from, to, isVertical: same(p[from].x, p[to].x) };
    setDragging(p);
  };
  const move = (e: PointerEvent) => {
    const d = drag.current;
    if (!d) return;
    const at = screenToFlowPosition({ x: e.clientX, y: e.clientY });
    const next = d.path.map((q) => ({ ...q }));
    for (const i of [d.from, d.to]) {
      if (d.isVertical) next[i].x = round(at.x);
      else next[i].y = round(at.y);
    }
    d.path = next;
    setDragging(next);
  };
  const end = () => {
    const d = drag.current;
    drag.current = null;
    setDragging(null);
    if (!d) return;
    const points = simplify(d.path).slice(1, -1);
    if (points.length <= MAX_ROUTE_POINTS) data?.onRoute(id, points);
  };

  const editing = !!data?.editing;
  return (
    <>
      <BaseEdge id={id} path={path} markerEnd={markerEnd} style={style} label={label} labelX={labelX} labelY={labelY} />
      {editing && (
        <EdgeLabelRenderer>
          {/* One handle per segment, except the stubs out of and into the nodes. */}
          {line.slice(0, -1).map((a, k) => {
            const b = line[k + 1];
            if (k === 0 || k === line.length - 2 || Math.hypot(b.x - a.x, b.y - a.y) < 14) return null;
            const isVertical = same(a.x, b.x);
            return (
              <div
                key={`seg-${k}`}
                className={`topo-route-handle ${isVertical ? "is-vertical" : "is-horizontal"} nodrag nopan`}
                style={{ transform: `translate(-50%, -50%) translate(${(a.x + b.x) / 2}px, ${(a.y + b.y) / 2}px)` }}
                title={isVertical ? "ลากซ้าย-ขวาเพื่อเลื่อนช่วงนี้" : "ลากขึ้น-ลงเพื่อเลื่อนช่วงนี้"}
                onPointerDown={(e) => begin(e, k)}
              />
            );
          })}
          {stored.length > 0 && (
            <button
              type="button"
              className="topo-route-reset nodrag nopan"
              style={{ transform: `translate(-50%, 0) translate(${labelX}px, ${labelY + 14}px)` }}
              onClick={(e) => {
                e.stopPropagation();
                data?.onRoute(id, []);
              }}
            >
              <RotateCcw size={11} /> รีเซ็ตเส้น
            </button>
          )}
        </EdgeLabelRenderer>
      )}
    </>
  );
});

export const edgeTypes = { routed: RoutedEdge };
