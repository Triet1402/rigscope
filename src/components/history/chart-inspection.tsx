"use client";

import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import type { HistoryGap, HistorySeries } from "@/hub/storage/history-query";
import { inspectionPoint, inspectionTimeForKey, inspectionTimes } from "@/history/inspection";
import { formatClock, formatDateTime, formatMetricValue } from "@/history/presentation";

type Cursor = { timeMs: number; source: string } | null;
type InspectionState = { cursor: Cursor; setCursor: (next: Cursor) => void };
const InspectionContext = createContext<InspectionState | null>(null);

export function ChartInspectionProvider({ children }: { children: ReactNode }) {
  const [cursor, updateCursor] = useState<Cursor>(null);
  const setCursor = useCallback((next: Cursor) => updateCursor((previous) =>
    previous?.timeMs === next?.timeMs && previous?.source === next?.source ? previous : next), []);
  const value = useMemo(() => ({ cursor, setCursor }), [cursor, setCursor]);
  return <InspectionContext.Provider value={value}>{children}</InspectionContext.Provider>;
}

export function useChartInspection({ series, fromMs, toMs, stepMs, gaps }: {
  series: readonly { points: HistorySeries["points"] }[];
  fromMs: number; toMs: number; stepMs: number; gaps: readonly HistoryGap[];
}) {
  const id = useId();
  const [localCursor, setLocalCursor] = useState<Cursor>(null);
  const shared = useContext(InspectionContext);
  const cursor = shared ? shared.cursor : localCursor;
  const setCursor = shared ? shared.setCursor : setLocalCursor;
  const frame = useRef<number | null>(null);
  const pending = useRef<Cursor>(null);
  const cancelPending = useCallback(() => {
    if (frame.current !== null) cancelAnimationFrame(frame.current);
    frame.current = null;
    pending.current = null;
  }, []);
  useEffect(() => cancelPending, [cancelPending]);
  const times = useMemo(() => inspectionTimes(series)
    .filter((point) => point.t <= toMs && point.t + stepMs / 2 >= fromMs), [series, fromMs, toMs, stepMs]);
  const timeMs = cursor && cursor.timeMs >= fromMs - stepMs / 2 && cursor.timeMs <= toMs ? cursor.timeMs : null;
  const active = timeMs !== null && cursor?.source === id;
  useEffect(() => {
    if (!active) return;
    const dismiss = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") {
        cancelPending();
        setCursor(null);
      }
    };
    window.addEventListener("keydown", dismiss);
    return () => window.removeEventListener("keydown", dismiss);
  }, [active, setCursor, cancelPending]);
  const inspect = (time: number) => {
    const clamped = Math.max(fromMs, Math.min(toMs, time));
    const point = inspectionPoint(times, clamped, stepMs, gaps);
    pending.current = { timeMs: point?.t ?? clamped, source: id };
    if (frame.current === null) {
      frame.current = requestAnimationFrame(() => {
        frame.current = null;
        setCursor(pending.current);
        pending.current = null;
      });
    }
  };
  const clear = () => {
    cancelPending();
    if (cursor?.source === id) setCursor(null);
  };
  const onKeyDown = (event: KeyboardEvent) => {
    cancelPending();
    if (event.key === "Escape") {
      event.preventDefault();
      clear();
    } else if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) {
      event.preventDefault();
      const next = inspectionTimeForKey(times, timeMs, event.key);
      if (next !== null) setCursor({ timeMs: next, source: id });
    }
  };
  return { id, timeMs, active, inspect, clear, onKeyDown, onFocus: () => {
    cancelPending();
    setCursor({ timeMs: times.at(-1)?.t ?? toMs, source: id });
  } };
}

export function ChartInspectionTooltip({ id, timeMs, fraction, unit, readings }: {
  id: string; timeMs: number; fraction: number; unit: string;
  readings: readonly { key: string; label: string; color: string; point: HistorySeries["points"][number] | null }[];
}) {
  const [placement] = useState(() => fraction > 0.5 ? "left" : "right");
  return (
    <div id={id} role="tooltip"
      className={`absolute top-2 z-10 max-h-56 w-56 max-w-[calc(100%-1rem)] overflow-y-auto rounded-md border border-border bg-card p-3 text-xs text-foreground ${placement === "left" ? "left-2" : "right-2"}`}>
      <p className="tnum font-medium">{formatDateTime(timeMs)}</p>
      <p className="mb-2 text-muted-foreground">Recorded bucket average</p>
      <dl className="grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-4 gap-y-1.5">
        {readings.map(({ key, label, color, point }) => (
          <div key={key} className="contents">
            <dt className="flex min-w-0 items-center gap-1.5">
              <span aria-hidden className="h-2 w-2 shrink-0 rounded-full" style={{ backgroundColor: color }} />
              <span className="break-words">{label}</span>
            </dt>
            <dd className="tnum text-right">
              {point?.avg == null ? "No reading" : formatMetricValue(point.avg, unit)}
              {point && point.t !== timeMs ? <span className="block text-muted-foreground">{formatClock(point.t)}</span> : null}
            </dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
