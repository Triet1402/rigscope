"use client";

import { useState } from "react";
import { coreHeatmapBuckets } from "@/history/core-heatmap";
import { gapBands, seriesStats, timeTicks } from "@/history/chart";
import { formatMetricValue, formatTimeTick } from "@/history/presentation";
import type { HistoryGap } from "@/hub/storage/history-query";
import type { ChartSeries } from "./history-chart";

const COLUMNS = 120;
const rowGrid = "grid grid-cols-[3rem_minmax(0,1fr)_2.5rem] items-center gap-2 sm:grid-cols-[3.5rem_minmax(0,1fr)_3rem_3rem]";
const color = (level: number) => level < 5 ? "var(--cpu)" : level < 8 ? "var(--warn)" : "var(--crit)";

export function CoreHistoryChart({ title, series, fromMs, toMs, gaps, hiddenKeys, onToggleSeries }: {
  title: string; series: readonly ChartSeries[]; fromMs: number; toMs: number;
  gaps: readonly HistoryGap[]; hiddenKeys: ReadonlySet<string>; onToggleSeries: (key: string) => void;
}) {
  const [busiestFirst, setBusiestFirst] = useState(false);
  const rows = series.map((item) => ({ ...item, stats: seriesStats(item.points) }));
  if (busiestFirst) rows.sort((a, b) => (b.stats.max ?? -1) - (a.stats.max ?? -1));
  const visible = rows.filter((row) => !hiddenKeys.has(row.key));
  const peak = visible.reduce<(typeof rows)[number] | null>((best, row) =>
    row.stats.max !== null && (best?.stats.max === null || best === null || row.stats.max > best.stats.max) ? row : best, null);
  const span = Math.max(1, toMs - fromMs);
  const bands = gapBands(gaps, fromMs, toMs);

  return (
    <figure className="min-w-0 lg:col-span-full">
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
      <div className={`${rowGrid} mb-1 text-xs text-muted-foreground`} aria-hidden>
        <span>Core</span><span><span className="sm:hidden">History</span><span className="hidden sm:inline">Peak usage over time</span></span><span className="hidden text-right sm:block">Last</span><span className="text-right">Peak</span>
      </div>
      <div role="region" aria-label="Per-core history rows" tabIndex={0} className="max-h-96 overflow-y-auto rounded-md">
        {rows.length === 0 ? <p className="py-8 text-center text-xs text-muted-foreground">No stored per-core readings.</p> : rows.map((row) => {
          const shown = !hiddenKeys.has(row.key);
          // Ten paths per row bound SVG nodes independently of the time-bin count.
          const paths = Array<string>(10).fill("");
          coreHeatmapBuckets(row.points, fromMs, toMs, COLUMNS).forEach((value, index) => {
            if (value === null) return;
            const level = Math.min(9, Math.floor(value / 10));
            paths[level] += `M${index},1h1v16h-1z`;
          });
          return (
            <div key={row.key} className={`${rowGrid} min-h-7 border-b border-border/40 text-xs`}>
              <label className="inline-flex min-h-7 cursor-pointer items-center rounded-sm">
                <input type="checkbox" checked={shown} onChange={() => onToggleSeries(row.key)} className="peer sr-only" />
                <span className={`${shown ? "text-foreground" : "text-muted-foreground line-through"} rounded-sm peer-focus-visible:outline-2 peer-focus-visible:outline-accent`}>{row.label}</span>
              </label>
              <svg viewBox={`0 0 ${COLUMNS} 18`} preserveAspectRatio="none" role="img"
                aria-label={`${row.label}: latest ${formatMetricValue(row.stats.latest, "percent")}, peak ${formatMetricValue(row.stats.max, "percent")}${shown ? "" : ", hidden"}`}
                className={`h-5 w-full ${shown ? "" : "opacity-20"}`}>
                <title>{row.label} · Last {formatMetricValue(row.stats.latest, "percent")} · Peak {formatMetricValue(row.stats.max, "percent")}</title>
                {shown ? paths.map((path, level) => path ? <path key={level} d={path} fill={color(level)} opacity={level < 5 ? 0.15 + level * 0.17 : 0.65 + (level % 5) * 0.07} /> : null) : null}
                {bands.map((band) => <rect key={`${band.fromMs}-${band.toMs}`} x={(band.fromMs - fromMs) / span * COLUMNS}
                  width={(band.toMs - band.fromMs) / span * COLUMNS} y={0} height={18} fill="var(--card)" />)}
              </svg>
              <span className="tnum hidden text-right text-muted-foreground sm:block">{shown ? formatMetricValue(row.stats.latest, "percent") : "—"}</span>
              <span className={`tnum text-right ${shown && (row.stats.max ?? 0) >= 80 ? "text-crit" : "text-muted-foreground"}`}>{shown ? formatMetricValue(row.stats.max, "percent") : "—"}</span>
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
