"use client";

import { useMemo, useState } from "react";
import { coreHeatmapBuckets } from "@/history/core-heatmap";
import { gapBands, seriesStats, timeTicks } from "@/history/chart";
import { formatDateTime, formatMetricValue, formatTimeTick } from "@/history/presentation";
import { inspectionPoint, inspectionTimeAtPointer } from "@/history/inspection";
import type { HistoryGap } from "@/hub/storage/history-query";
import type { ChartSeries } from "./history-chart";
import { useChartInspection } from "./chart-inspection";

const COLUMNS = 120;
// Reserve room for "At cursor" even before inspection starts. Every header,
// data row and time axis uses these same tracks to keep the heatmap aligned.
const rowGrid = "grid grid-cols-[3rem_minmax(0,1fr)_4.5rem] items-center gap-2 sm:grid-cols-[3.5rem_minmax(0,1fr)_4.5rem_3rem]";
const color = (level: number) => level < 5 ? "var(--cpu)" : level < 8 ? "var(--warn)" : "var(--crit)";

export function CoreHistoryChart({ title, series, fromMs, toMs, stepMs, gaps, hiddenKeys, onToggleSeries }: {
  title: string; series: readonly ChartSeries[]; fromMs: number; toMs: number;
  stepMs: number;
  gaps: readonly HistoryGap[]; hiddenKeys: ReadonlySet<string>; onToggleSeries: (key: string) => void;
}) {
  const [busiestFirst, setBusiestFirst] = useState(false);
  const inspectedSeries = useMemo(() => series.filter((row) => !hiddenKeys.has(row.key)), [series, hiddenKeys]);
  const inspection = useChartInspection({ series: inspectedSeries, fromMs, toMs, stepMs, gaps });
  const rows = useMemo(() => {
    const next = series.map((item) => {
      // Cache the heatmap geometry while the cursor moves across its rows.
      const paths = Array<string>(10).fill("");
      coreHeatmapBuckets(item.points, fromMs, toMs, COLUMNS).forEach((value, index) => {
        if (value === null) return;
        const level = Math.min(9, Math.floor(value / 10));
        paths[level] += `M${index},1h1v16h-1z`;
      });
      return { ...item, stats: seriesStats(item.points), paths };
    });
    if (busiestFirst) next.sort((a, b) => (b.stats.max ?? -1) - (a.stats.max ?? -1));
    return next;
  }, [series, fromMs, toMs, busiestFirst]);
  const visible = rows.filter((row) => !hiddenKeys.has(row.key));
  const peak = visible.reduce<(typeof rows)[number] | null>((best, row) =>
    row.stats.max !== null && (best?.stats.max === null || best === null || row.stats.max > best.stats.max) ? row : best, null);
  const cursorTime = inspection.timeMs === null ? null : formatDateTime(inspection.timeMs);
  const span = Math.max(1, toMs - fromMs);
  const bands = gapBands(gaps, fromMs, toMs);

  return (
    <figure className="min-w-0 lg:col-span-full" onPointerLeave={(event) => { if (event.pointerType === "mouse") inspection.clear(); }}>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-xs font-medium text-foreground">{title}</h3>
        <div className="flex flex-wrap items-center gap-3 text-xs">
          {peak ? <span className="tnum text-muted-foreground">Peak · {peak.label} <span className="text-foreground">{formatMetricValue(peak.stats.max, "percent")}</span></span> : null}
          <button type="button" aria-pressed={busiestFirst} onClick={() => setBusiestFirst(!busiestFirst)}
            className={`min-h-8 rounded-md border border-border px-2 ${busiestFirst ? "bg-selected text-foreground" : "bg-background text-muted-foreground hover:text-foreground"}`}>
            Busiest first
          </button>
        </div>
      </div>
      <div className={`${rowGrid} mb-1 min-h-4 whitespace-nowrap text-xs leading-4 text-muted-foreground`} aria-hidden>
        <span>Core</span>
        <span className="truncate">
          {cursorTime !== null ? <span className="tnum">{cursorTime}</span> : <>
            <span className="sm:hidden">History</span><span className="hidden sm:inline">Peak usage over time</span>
          </>}
        </span>
        <span className="hidden text-right sm:block">{cursorTime !== null ? "At cursor" : "Last"}</span>
        <span className="text-right"><span className="hidden sm:inline">Peak</span><span className="sm:hidden">{cursorTime !== null ? "At cursor" : "Peak"}</span></span>
      </div>
      <p id={`${inspection.id}-instructions`} className="sr-only">Focus a core chart and use Left and Right arrows to inspect recorded bucket peaks, Home or End to jump, and Escape to dismiss.</p>
      <div role="region" aria-label="Per-core history rows" tabIndex={0} className="max-h-96 overflow-y-auto rounded-md">
        {rows.length === 0 ? <p className="py-8 text-center text-xs text-muted-foreground">No stored per-core readings.</p> : rows.map((row) => {
          const shown = !hiddenKeys.has(row.key);
          const point = inspection.timeMs === null ? null : inspectionPoint(row.points, inspection.timeMs, stepMs, gaps);
          const inspectedValue = point?.max ?? point?.avg ?? null;
          return (
            <div key={row.key} className={`${rowGrid} min-h-7 border-b border-border/40 text-xs`}>
              <label className="inline-flex min-h-7 cursor-pointer items-center rounded-sm">
                <input type="checkbox" checked={shown} onChange={() => onToggleSeries(row.key)} className="peer sr-only" />
                <span className={`${shown ? "text-foreground" : "text-muted-foreground line-through"} rounded-sm peer-focus-visible:outline-2 peer-focus-visible:outline-accent`}>{row.label}</span>
              </label>
              <svg viewBox={`0 0 ${COLUMNS} 18`} preserveAspectRatio="none" role="img"
                aria-label={`${row.label}: ${cursorTime !== null ? `${formatMetricValue(inspectedValue, "percent")} at ${cursorTime}` : `latest ${formatMetricValue(row.stats.latest, "percent")}`}, peak ${formatMetricValue(row.stats.max, "percent")}${shown ? "" : ", hidden"}`}
                aria-describedby={`${inspection.id}-instructions`}
                tabIndex={shown ? 0 : undefined}
                onPointerDown={shown ? (event) => {
                  if (event.pointerType === "mouse") return;
                  event.currentTarget.setPointerCapture(event.pointerId);
                  const bounds = event.currentTarget.getBoundingClientRect();
                  inspection.inspect(inspectionTimeAtPointer(event.clientX, bounds.left, bounds.width, fromMs, toMs));
                } : undefined}
                onPointerCancel={inspection.clear}
                onContextMenu={(event) => event.preventDefault()}
                onPointerMove={shown ? (event) => {
                  const bounds = event.currentTarget.getBoundingClientRect();
                  inspection.inspect(inspectionTimeAtPointer(event.clientX, bounds.left, bounds.width, fromMs, toMs));
                } : undefined}
                onFocus={inspection.onFocus} onBlur={inspection.clear} onKeyDown={inspection.onKeyDown}
                className={`h-7 w-full touch-pan-y select-none cursor-crosshair rounded-sm focus-visible:outline-2 focus-visible:outline-accent ${shown ? "" : "opacity-20"}`}>
                <title>{`${row.label} · Last ${formatMetricValue(row.stats.latest, "percent")} · Peak ${formatMetricValue(row.stats.max, "percent")}`}</title>
                {shown ? row.paths.map((path, level) => path ? <path key={level} d={path} fill={color(level)} opacity={level < 5 ? 0.15 + level * 0.17 : 0.65 + (level % 5) * 0.07} /> : null) : null}
                {bands.map((band) => <rect key={`${band.fromMs}-${band.toMs}`} x={(band.fromMs - fromMs) / span * COLUMNS}
                  width={(band.toMs - band.fromMs) / span * COLUMNS} y={0} height={18} fill="var(--card)" />)}
                {inspection.timeMs !== null ? <line aria-hidden x1={(inspection.timeMs - fromMs) / span * COLUMNS}
                  x2={(inspection.timeMs - fromMs) / span * COLUMNS} y1={0} y2={18}
                  stroke="var(--foreground)" strokeWidth="1" vectorEffect="non-scaling-stroke" pointerEvents="none" /> : null}
              </svg>
              <span className="tnum hidden text-right text-muted-foreground sm:block">{shown ? formatMetricValue(inspection.timeMs !== null ? inspectedValue : row.stats.latest, "percent") : "—"}</span>
              <span className={`tnum text-right ${shown && (row.stats.max ?? 0) >= 80 ? "text-crit" : "text-muted-foreground"}`}>
                <span className="hidden sm:inline">{shown ? formatMetricValue(row.stats.max, "percent") : "—"}</span>
                <span className="sm:hidden">{shown ? formatMetricValue(inspection.timeMs !== null ? inspectedValue : row.stats.max, "percent") : "—"}</span>
              </span>
            </div>
          );
        })}
      </div>
      <div className={`${rowGrid} mt-2 text-xs text-muted-foreground`} aria-hidden>
        <span /><div className="flex justify-between gap-2 tnum">{timeTicks(fromMs, toMs, 3).map((tick, index) => <span key={tick} className={index === 1 ? "hidden sm:inline" : ""}>{formatTimeTick(tick, span)}</span>)}</div><span className="hidden sm:block" /><span />
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
        {[{ label: "0–49%", color: "var(--cpu)" }, { label: "50–79%", color: "var(--warn)" }, { label: "80–100%", color: "var(--crit)" }].map((item) => (
          <span key={item.label} className="inline-flex items-center gap-1.5"><span aria-hidden className="h-2 w-4 rounded-sm" style={{ backgroundColor: item.color }} />{item.label}</span>
        ))}
        <span>No fill · no reading</span>
      </div>
    </figure>
  );
}
