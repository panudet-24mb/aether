"use client";
import { memo, useRef, useState, type PointerEvent as ReactPointerEvent } from "react";
import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, useReactFlow, type Edge, type EdgeProps, type XYPosition } from "@xyflow/react";
import { RotateCcw } from "lucide-react";

// A connection line that can be bent: in edit mode a selected line shows a handle on each bend point (drag to move,
// double-click to remove) and one in the middle of each segment (drag to add a bend there). Without bend points it is
// drawn exactly like React Flow's smoothstep edge. Bend points are shared by the workspace (migration 00053).

export const MAX_ROUTE_POINTS = 16;

export type RoutedData = {
  points: XYPosition[];
  /** Bend handles are shown: the board is in edit mode and the line is selected. */
  editing: boolean;
  /** Stable callback (an empty list straightens the line). */
  onRoute: (edge: string, points: XYPosition[]) => void;
};
export type RoutedEdgeType = Edge<RoutedData, "routed">;

const round = (p: XYPosition): XYPosition => ({ x: Math.round(p.x * 100) / 100, y: Math.round(p.y * 100) / 100 });

export const RoutedEdge = memo(function RoutedEdge(props: EdgeProps<RoutedEdgeType>) {
  const { id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, markerEnd, style, label, data } = props;
  const { screenToFlowPosition } = useReactFlow();
  // While a handle is dragged the line follows locally; the result is saved once, on release.
  const [dragging, setDragging] = useState<XYPosition[] | null>(null);
  const drag = useRef<{ index: number; points: XYPosition[] } | null>(null);
  const points = dragging ?? data?.points ?? [];

  const all = [{ x: sourceX, y: sourceY }, ...points, { x: targetX, y: targetY }];
  let path: string, labelX: number, labelY: number;
  if (points.length === 0) {
    [path, labelX, labelY] = getSmoothStepPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition });
  } else {
    path = all.map((p, i) => `${i ? "L" : "M"} ${p.x} ${p.y}`).join(" ");
    const mid = Math.floor((all.length - 1) / 2);
    labelX = (all[mid].x + all[mid + 1].x) / 2;
    labelY = (all[mid].y + all[mid + 1].y) / 2;
  }

  const begin = (e: ReactPointerEvent<HTMLElement>, next: XYPosition[], index: number) => {
    e.stopPropagation();
    e.currentTarget.setPointerCapture(e.pointerId);
    drag.current = { index, points: next };
    setDragging(next);
  };
  const move = (e: ReactPointerEvent<HTMLElement>) => {
    const d = drag.current;
    if (!d) return;
    const p = round(screenToFlowPosition({ x: e.clientX, y: e.clientY }));
    d.points = d.points.map((q, i) => (i === d.index ? p : q));
    setDragging(d.points);
  };
  const end = () => {
    const d = drag.current;
    drag.current = null;
    setDragging(null);
    if (d) data?.onRoute(id, d.points);
  };

  const editing = !!data?.editing;
  return (
    <>
      <BaseEdge id={id} path={path} markerEnd={markerEnd} style={style} label={label} labelX={labelX} labelY={labelY} />
      {editing && (
        <EdgeLabelRenderer>
          {/* Segment midpoints: drag to insert a bend point there. */}
          {points.length < MAX_ROUTE_POINTS &&
            all.slice(0, -1).map((a, i) => {
              const b = all[i + 1];
              const at = { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
              return (
                <div
                  key={`add-${i}`}
                  className="topo-route-handle is-add nodrag nopan"
                  style={{ transform: `translate(-50%, -50%) translate(${at.x}px, ${at.y}px)` }}
                  title="ลากเพื่อดัดเส้นตรงนี้"
                  onPointerDown={(e) => begin(e, [...points.slice(0, i), round(at), ...points.slice(i)], i)}
                  onPointerMove={move}
                  onPointerUp={end}
                />
              );
            })}
          {points.map((p, i) => (
            <div
              key={`pt-${i}`}
              className="topo-route-handle nodrag nopan"
              style={{ transform: `translate(-50%, -50%) translate(${p.x}px, ${p.y}px)` }}
              title="ลากเพื่อย้ายจุดหักเลี้ยว · ดับเบิลคลิกเพื่อลบจุด"
              onPointerDown={(e) => begin(e, points, i)}
              onPointerMove={move}
              onPointerUp={end}
              onDoubleClick={(e) => {
                e.stopPropagation();
                data?.onRoute(id, points.filter((_, j) => j !== i));
              }}
            />
          ))}
          {points.length > 0 && (
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
