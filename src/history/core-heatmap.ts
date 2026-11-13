import type { HistorySeries } from "./contract";

/** Fixed time bins preserve the hottest recorded value; empty bins stay empty. */
export function coreHeatmapBuckets(points: HistorySeries["points"], fromMs: number, toMs: number, columns = 120): Array<number | null> {
  const bins: Array<number | null> = Array(columns).fill(null);
  const span = Math.max(1, toMs - fromMs);
  for (const point of points) {
    if (point.t < fromMs || point.t > toMs) continue;
    const value = point.max ?? point.avg;
    if (value === null || !Number.isFinite(value)) continue;
    const index = Math.min(columns - 1, Math.floor((point.t - fromMs) / span * columns));
    bins[index] = Math.max(bins[index] ?? 0, Math.min(100, Math.max(0, value)));
  }
  return bins;
}
