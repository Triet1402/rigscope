"use client";

/**
 * A gap-aware SVG time-series chart: average line plus the min/max envelope
 * for each stored bucket. Missing buckets break the line; recorded gaps are
 * shaded. Summary stats are exposed through the chart's accessible label.
 */

import { useCallback, useMemo, useRef, type ReactNode } from "react";

import {
  buildYDomain,
  downsamplePoints,
  gapBands,
  seriesStats,
  splitSeries,
  timeTicks,
  type ChartPoint,
  type SeriesStats,
} from "@/history/chart";
import {
  formatAxisValue,
  formatClock,
  formatMetricValue,
  formatTimeTick,
} from "@/history/presentation";
import { metricUnitLabel } from "@/history/metrics";
import type { HistoryGap, HistorySeries } from "@/hub/storage/history-query";
import { inspectionPoint, inspectionTimeAtPointer } from "@/history/inspection";
import { ChartInspectionTooltip, useChartInspection } from "./chart-inspection";

const W = 800;
const H = 160;
const PAD = 6;
/**
 * A 600-point API request can include 601 buckets where the rolling range
 * cuts the fixed grid. Draw those bucket for bucket: regrouping them by array
 * position would shift historical averages as the oldest bucket leaves.
 * A future wider request is reduced without losing the envelope.
 */
const MAX_DRAW_POINTS = 601;

export type ChartSeries = {
  key: string;
  label: string;
  metric: string;
  resourceId: string;
  color: string;
  dash: string | undefined;
  points: HistorySeries["points"];
};

type Props = {
  title: string;
  unit: string;
  series: readonly ChartSeries[];
  fromMs: number;
  toMs: number;
  stepMs: number;
  gaps: readonly HistoryGap[];
  hiddenKeys: ReadonlySet<string>;
  onToggleSeries: (key: string) => void;
  emptyMessage: string;
  note?: string;
  action?: ReactNode;
  maxDrawPoints?: number;
  className?: string;
};

function averageSegments(
  segments: readonly ChartPoint[][],
  x: (t: number) => number,
  y: (value: number) => number,
): string {
  return segments
    .filter((segment) => segment.length > 1)
    .map((segment) =>
      segment
        .map((point, index) => `${index === 0 ? "M" : "L"}${x(point.t).toFixed(2)},${y(point.avg!).toFixed(2)}`)
        .join(" "),
    )
    .join(" ");
}

function envelopeSegments(
  segments: readonly ChartPoint[][],
  x: (t: number) => number,
  y: (value: number) => number,
): string {
  return segments
    .filter((segment) => segment.length > 1)
    .map((segment) => {
      const top = segment.map((point) => `${x(point.t).toFixed(2)},${y(point.max ?? point.avg!).toFixed(2)}`);
      const bottom = [...segment]
        .reverse()
        .map((point) => `${x(point.t).toFixed(2)},${y(point.min ?? point.avg!).toFixed(2)}`);
      return `M${top.join("L")}L${bottom.join("L")}Z`;
    })
    .join(" ");
}

function statsText(stats: SeriesStats, unit: string): string {
  const parts: string[] = [];
  if (stats.latest !== null) {
    const when = stats.latestT !== null ? formatClock(stats.latestT) : null;
    parts.push(`latest ${formatMetricValue(stats.latest, unit)}${when ? ` at ${when}` : ""}`);
  }
  parts.push(`average ${formatMetricValue(stats.avg, unit)}`);
  if (stats.min !== null && stats.max !== null) {
    parts.push(`recorded range ${formatMetricValue(stats.min, unit)} to ${formatMetricValue(stats.max, unit)}`);
  }
  return parts.join(", ");
}

function chartAriaLabel(
  title: string,
  unit: string,
  series: readonly ChartSeries[],
  fromMs: number,
  toMs: number,
  gapCount: number,
): string {
  if (series.length === 0) return `${title} chart: no series selected.`;
  const descriptions = series.map((item) => {
    const stats = seriesStats(item.points);
    return `${item.label}: ${statsText(stats, unit)}`;
  });
  const gaps = gapCount > 0 ? `${gapCount} shaded interval${gapCount === 1 ? "" : "s"} without stored data.` : "";
  return `${title} time-series chart. ${descriptions.join(". ")}. ${gaps} Time range ${formatClock(fromMs)} to ${formatClock(toMs)}.`;
}

export function HistoryChart(props: Props) {
  const { title, unit, series, fromMs, toMs, stepMs, gaps, hiddenKeys, onToggleSeries, emptyMessage, note } = props;
  const visible = useMemo(() => series.filter((item) => !hiddenKeys.has(item.key)), [series, hiddenKeys]);
  const inspection = useChartInspection({ series: visible, fromMs, toMs, stepMs, gaps });
  const plot = useRef<SVGSVGElement>(null);
  const visiblePoints = useMemo(() => visible.flatMap((item) => item.points), [visible]);
  const hasAnyPoint = visiblePoints.some((point) => point.avg !== null || point.min !== null || point.max !== null);
  const span = Math.max(1, toMs - fromMs);
  // Meet the neighboring plotted buckets rather than a receipt timestamp that
  // sits inside the last partial bucket.
  const bands = useMemo(() => gapBands(gaps, fromMs, toMs, visiblePoints), [gaps, fromMs, toMs, visiblePoints]);

  const domain = useMemo(() => buildYDomain(
    unit,
    visiblePoints.flatMap((point) => [point.min, point.max, point.avg]).filter((value): value is number => value !== null),
  ), [unit, visiblePoints]);
  const yRange = Math.max(1e-9, domain.max - domain.min);
  const x = useCallback((t: number): number => PAD + ((Math.min(toMs, Math.max(fromMs, t)) - fromMs) / span) * (W - 2 * PAD), [fromMs, toMs, span]);
  const y = useCallback((value: number): number => PAD + (1 - (value - domain.min) / yRange) * (H - 2 * PAD), [domain.min, yRange]);

  const drawSeries = useMemo(() => visible.map((item) => {
    // Split before reducing so no downsampled bucket mixes opposite sides of
    // an outage. Paths and their React elements stay cached during inspection.
    const segments = splitSeries(item.points, { stepMs, gaps: bands, fromMs, toMs })
      .map((segment) => downsamplePoints(segment, props.maxDrawPoints ?? MAX_DRAW_POINTS, stepMs));
    return { ...item, segments, envelope: envelopeSegments(segments, x, y), path: averageSegments(segments, x, y) };
  }), [visible, props.maxDrawPoints, stepMs, bands, fromMs, toMs, x, y]);
  const ariaLabel = useMemo(() => chartAriaLabel(title, unit, visible, fromMs, toMs, bands.length),
    [title, unit, visible, fromMs, toMs, bands.length]);
  const legendStats = useMemo(() => new Map(series.map((item) => [item.key, seriesStats(item.points)])), [series]);
  const inspected = inspection.timeMs === null ? [] : visible.map((item) => ({
    ...item, point: inspectionPoint(item.points, inspection.timeMs!, stepMs, gaps),
  }));
  const cursorX = inspection.timeMs === null ? null : x(inspection.timeMs);
  const tooltipId = `${inspection.id}-tooltip`;
  const instructionsId = `${inspection.id}-instructions`;

  const drawing = useMemo(() => (<>
    {domain.ticks.map((tick) => (
      <line
        key={`grid-${tick}`}
        x1={PAD}
        x2={W - PAD}
        y1={y(tick)}
        y2={y(tick)}
        stroke="var(--border)"
        strokeWidth="1"
        vectorEffect="non-scaling-stroke"
      />
    ))}
    {bands.map((band) => (
      <rect
        key={`gap-${band.fromMs}-${band.toMs}-${band.kind}`}
        x={x(band.fromMs)}
        y={PAD}
        width={Math.max(1, x(band.toMs) - x(band.fromMs))}
        height={H - 2 * PAD}
        fill="var(--muted-foreground)"
        opacity="0.09"
      />
    ))}
    {drawSeries.map((item) => {
      const envelope = item.envelope;
      return envelope === "" ? null : (
        <path
          key={`band-${item.key}`}
          d={envelope}
          fill={item.color}
          opacity="0.14"
          stroke="none"
        />
      );
    })}
    {drawSeries.map((item) => {
      const path = item.path;
      return path === "" ? null : (
        <path
          key={`line-${item.key}`}
          d={path}
          fill="none"
          stroke={item.color}
          strokeWidth="2"
          strokeDasharray={item.dash}
          strokeLinecap="round"
          strokeLinejoin="round"
          vectorEffect="non-scaling-stroke"
        />
      );
    })}
    {drawSeries.flatMap((item) =>
      item.segments
        .filter((segment) => segment.length === 1)
        .map((segment) => {
          const point = segment[0]!;
          const cx = x(point.t);
          const cy = y(point.avg!);
          return (
            <line
              key={`dot-${item.key}-${point.t}`}
              x1={cx - 2.5}
              x2={cx + 2.5}
              y1={cy}
              y2={cy}
              stroke={item.color}
              strokeWidth="2.5"
              strokeLinecap="round"
              vectorEffect="non-scaling-stroke"
            />
          );
        }),
    )}
  </>), [domain, x, y, bands, drawSeries]);

  return (
    <figure className={`flex min-w-0 flex-col gap-2 ${props.className ?? ""}`}>
      <div className="flex min-h-8 flex-wrap items-center justify-between gap-2">
        <h3 className="text-xs font-medium tracking-tight text-foreground">{title}</h3>
        <div className="flex items-center gap-3">
          <span className="tnum shrink-0 text-xs text-muted-foreground">{note ?? metricUnitLabel(unit)}</span>
          {props.action}
        </div>
      </div>

      {hasAnyPoint ? (
        <>
          <div className="flex min-w-0 gap-2">
            <div
              aria-hidden
              className="flex h-40 w-14 shrink-0 flex-col justify-between py-0.5 text-right text-xs leading-none tnum text-muted-foreground"
            >
              {[...domain.ticks].reverse().map((tick) => (
                <span key={tick}>{formatAxisValue(tick, unit)}</span>
              ))}
            </div>
            <div className="min-w-0 flex-1">
              <div className="relative touch-pan-y select-none" onPointerLeave={(event) => { if (event.pointerType === "mouse") inspection.clear(); }} onPointerDown={(event) => {
                if (event.pointerType === "mouse") return;
                event.currentTarget.setPointerCapture(event.pointerId);
                const bounds = plot.current?.getBoundingClientRect();
                if (bounds) inspection.inspect(inspectionTimeAtPointer(event.clientX, bounds.left, bounds.width, fromMs, toMs, PAD / W));
              }} onPointerCancel={inspection.clear} onContextMenu={(event) => event.preventDefault()} onPointerMove={(event) => {
                const bounds = plot.current?.getBoundingClientRect();
                if (bounds) inspection.inspect(inspectionTimeAtPointer(event.clientX, bounds.left, bounds.width, fromMs, toMs, PAD / W));
              }}>
                <p id={instructionsId} className="sr-only">Touch and drag horizontally to inspect readings. Use Left and Right arrows to inspect recorded readings, Home or End to jump, and Escape to dismiss.</p>
                <svg
                  ref={plot}
                  viewBox={`0 0 ${W} ${H}`}
                  preserveAspectRatio="none"
                  role="img"
                  aria-label={ariaLabel}
                  tabIndex={0}
                  aria-describedby={`${instructionsId}${inspection.active ? ` ${tooltipId}` : ""}`}
                  onFocus={inspection.onFocus}
                  onBlur={inspection.clear}
                  onKeyDown={inspection.onKeyDown}
                  className="block h-40 w-full cursor-crosshair rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
                >
                  <title>{title}</title>
                  {drawing}
                  {cursorX !== null ? (
                    <g aria-hidden pointerEvents="none">
                      <line x1={cursorX} x2={cursorX} y1={PAD} y2={H - PAD}
                        stroke="var(--muted-foreground)" strokeWidth="1" strokeDasharray="3 3" vectorEffect="non-scaling-stroke" />
                      {inspected.map(({ key, color, point }) => point?.avg == null ? null : (
                        <circle key={key} cx={x(point.t)} cy={y(point.avg)} r="3.5"
                          fill={color} stroke="var(--card)" strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
                      ))}
                    </g>
                  ) : null}
                </svg>
                {inspection.active && inspection.timeMs !== null && cursorX !== null ? (
                  <ChartInspectionTooltip id={tooltipId} timeMs={inspection.timeMs} fraction={cursorX / W}
                    unit={unit} readings={inspected} />
                ) : null}
              </div>
              <div
                aria-hidden
                className="mt-1 flex justify-between gap-2 text-xs leading-tight tnum text-muted-foreground"
              >
                {timeTicks(fromMs, toMs).map((tick, index, ticks) => (
                  <span
                    key={tick}
                    className={`${index === 0 ? "text-left" : index === ticks.length - 1 ? "text-right" : "hidden text-center sm:inline"}`}
                  >
                    {formatTimeTick(tick, span)}
                  </span>
                ))}
              </div>
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs">
            {series.map((item) => {
              const shown = !hiddenKeys.has(item.key);
              const stats = legendStats.get(item.key)!;
              return (
                <label
                  key={`legend-${item.key}`}
                  className="inline-flex min-h-7 cursor-pointer items-center gap-1.5"
                  title={shown ? `Hide ${item.label}` : `Show ${item.label}`}
                >
                  <input
                    type="checkbox"
                    checked={shown}
                    onChange={() => onToggleSeries(item.key)}
                    className="peer sr-only"
                  />
                  <span
                    aria-hidden
                    className="flex h-4 w-5 shrink-0 items-center justify-center rounded-sm peer-focus-visible:outline peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:[outline-color:var(--focus)]"
                  >
                    <svg viewBox="0 0 20 8" className="h-2 w-5" aria-hidden>
                      <line
                        x1="1"
                        y1="4"
                        x2="19"
                        y2="4"
                        stroke={item.color}
                        strokeWidth="2.5"
                        strokeDasharray={item.dash}
                        strokeLinecap="round"
                      />
                    </svg>
                  </span>
                  <span className={shown ? "text-foreground" : "text-muted-foreground line-through"}>
                    {item.label}
                  </span>
                  <span className="tnum text-muted-foreground">
                    {formatMetricValue(stats.latest, unit)}
                  </span>
                </label>
              );
            })}
          </div>
        </>
      ) : (
        <div className="flex min-h-32 items-center justify-center rounded-lg border border-dashed border-border px-4 py-6 text-center text-xs text-muted-foreground">
          {series.length === 0
            ? emptyMessage
            : visible.length === 0
              ? "All series are hidden. Re-enable one in the legend below."
              : emptyMessage}
        </div>
      )}

      {!hasAnyPoint && series.length > 0 ? (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs">
          {series.map((item) => {
            const shown = !hiddenKeys.has(item.key);
            return (
              <label key={`legend-empty-${item.key}`} className="inline-flex min-h-7 cursor-pointer items-center gap-1.5">
                <input
                  type="checkbox"
                  checked={shown}
                  onChange={() => onToggleSeries(item.key)}
                  className="peer sr-only"
                />
                <span
                  aria-hidden
                  className="flex h-4 w-5 shrink-0 items-center justify-center peer-focus-visible:outline peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:[outline-color:var(--focus)]"
                >
                  <svg viewBox="0 0 20 8" className="h-2 w-5" aria-hidden>
                    <line
                      x1="1"
                      y1="4"
                      x2="19"
                      y2="4"
                      stroke={item.color}
                      strokeWidth="2.5"
                      strokeDasharray={item.dash}
                      strokeLinecap="round"
                    />
                  </svg>
                </span>
                <span className={shown ? "text-foreground" : "text-muted-foreground line-through"}>
                  {item.label}
                </span>
              </label>
            );
          })}
        </div>
      ) : null}
    </figure>
  );
}
