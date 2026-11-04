import type { HistoryGap, HistorySeries } from "../hub/storage/history-query";

type Point = HistorySeries["points"][number];

/** Inspect stored readings only; never interpolate across missing data. */
export function inspectionPoint(
  points: readonly Point[], timeMs: number, stepMs: number, gaps: readonly HistoryGap[],
): Point | null {
  if (gaps.some((gap) => timeMs > gap.fromMs && timeMs < gap.toMs)) return null;
  let low = 0;
  let high = points.length;
  while (low < high) {
    const mid = (low + high) >>> 1;
    if (points[mid]!.t < timeMs) low = mid + 1;
    else high = mid;
  }
  const previous = points[low - 1];
  const next = points[low];
  const nearest = previous && (!next || timeMs - previous.t <= next.t - timeMs) ? previous : next;
  if (!nearest || Math.abs(nearest.t - timeMs) > stepMs / 2) return null;
  if (gaps.some((gap) => gap.toMs > Math.min(timeMs, nearest.t) && gap.fromMs < Math.max(timeMs, nearest.t))) return null;
  return nearest;
}

export function inspectionTimes(series: readonly { points: readonly Point[] }[]): Point[] {
  return [...new Set(series.flatMap((item) => item.points.map((point) => point.t)))]
    .sort((a, b) => a - b).map((t) => ({ t, min: null, max: null, avg: null }));
}

export function inspectionTimeAtPointer(
  clientX: number, left: number, width: number, fromMs: number, toMs: number, paddingFraction = 0,
): number {
  const fraction = width > 0 ? (clientX - left) / width : 0;
  const position = Math.max(0, Math.min(1, (fraction - paddingFraction) / (1 - 2 * paddingFraction)));
  return Math.round(fromMs + position * (toMs - fromMs));
}

export function inspectionTimeForKey(times: readonly Point[], timeMs: number | null, key: string): number | null {
  if (times.length === 0) return null;
  if (key === "Home") return times[0]!.t;
  if (key === "End" || timeMs === null) return times.at(-1)!.t;
  if (key === "ArrowLeft") return [...times].reverse().find((point) => point.t < timeMs)?.t ?? times[0]!.t;
  if (key === "ArrowRight") return times.find((point) => point.t > timeMs)?.t ?? times.at(-1)!.t;
  return timeMs;
}
