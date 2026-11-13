/**
 * Chart geometry helpers for History: gap-aware segmentation, envelope-
 * preserving downsampling, gap bands and unit-aware Y domains.
 *
 * Browser-safe (no DOM, no Node). A missing value is never represented as
 * zero: null buckets end a line segment, omitted buckets break it, and a
 * recorded gap interval breaks every series that spans it.
 */

import type { HistoryGap, MetricUnit } from "./contract";

export type HistoryPoint = { t: number; min: number | null; max: number | null; avg: number | null };

export type ChartPoint = HistoryPoint & {
  /** Set when downsampling starts a new group after a real discontinuity. */
  gapBefore?: boolean;
  /** Set when this point aggregates more than one stored bucket. */
  merged?: boolean;
};

export type SeriesStats = {
  count: number;
  min: number | null;
  max: number | null;
  avg: number | null;
  latest: number | null;
  latestT: number | null;
  firstT: number | null;
  lastT: number | null;
};

export type GapBand = HistoryGap & { fromMs: number; toMs: number };

const MISSING = "—";

function plottable(point: HistoryPoint): boolean {
  return point.avg !== null && Number.isFinite(point.avg);
}

function finiteOrNull(value: number | null): number | null {
  return value !== null && Number.isFinite(value) ? value : null;
}

// ---------------------------------------------------------------------------
// Gaps
// ---------------------------------------------------------------------------

function mergeSortedBands(bands: readonly GapBand[]): GapBand[] {
  const merged: GapBand[] = [];
  for (const gap of bands) {
    const previous = merged.at(-1);
    if (previous !== undefined && gap.fromMs <= previous.toMs) {
      previous.toMs = Math.max(previous.toMs, gap.toMs);
      if (previous.kind === "missing" && gap.kind === "dropped") previous.kind = "dropped";
      if (previous.reason === undefined && gap.reason !== undefined) previous.reason = gap.reason;
      if (previous.droppedRecords === undefined && gap.droppedRecords !== undefined) {
        previous.droppedRecords = gap.droppedRecords;
      }
      continue;
    }
    merged.push({ ...gap });
  }
  return merged;
}

function lastPointAtOrBefore(times: readonly number[], value: number): number | null {
  let low = 0;
  let high = times.length;
  while (low < high) {
    const mid = (low + high) >>> 1;
    if (times[mid]! <= value) low = mid + 1;
    else high = mid;
  }
  return low > 0 ? times[low - 1]! : null;
}

function firstPointAtOrAfter(times: readonly number[], value: number): number | null {
  let low = 0;
  let high = times.length;
  while (low < high) {
    const mid = (low + high) >>> 1;
    if (times[mid]! < value) low = mid + 1;
    else high = mid;
  }
  return low < times.length ? times[low]! : null;
}

/**
 * Extend clipped gaps to the neighboring plotted bucket timestamps so the
 * shaded outage visibly meets the line endpoints instead of starting at a
 * receipt time inside the last partial bucket.
 *
 * Never shorten a recorded outage to a bucket inside it. Boundary aggregates
 * retain their real timestamps and values; segmentation keeps them as isolated
 * marks instead of inventing a horizontal line through the outage.
 */
export function alignGapBands(
  bands: readonly GapBand[],
  points: readonly HistoryPoint[],
  fromMs: number,
  toMs: number,
): GapBand[] {
  if (bands.length === 0) return [];
  const times = [...new Set(points.filter(plottable).map((point) => point.t).filter((t) => Number.isFinite(t)))]
    .sort((a, b) => a - b);
  const aligned: GapBand[] = [];
  for (const band of bands) {
    const from = Math.max(fromMs, Math.min(toMs, band.fromMs));
    const to = Math.max(fromMs, Math.min(toMs, band.toMs));
    if (to <= from) continue;
    const start = Math.max(fromMs, lastPointAtOrBefore(times, from) ?? from);
    const end = Math.min(toMs, firstPointAtOrAfter(times, to) ?? to);
    if (end <= start) continue;
    aligned.push({ ...band, fromMs: start, toMs: end });
  }
  aligned.sort((a, b) => a.fromMs - b.fromMs || a.toMs - b.toMs);
  return mergeSortedBands(aligned);
}

/**
 * Clip gaps to the visible range and merge overlaps so the chart never paints
 * the same interval twice. Clipped bands keep their dominant kind/reason.
 * When plotted points are supplied, bands are also aligned to the neighboring
 * bucket timestamps (see `alignGapBands`).
 */
export function gapBands(
  gaps: readonly HistoryGap[],
  fromMs: number,
  toMs: number,
  points?: readonly HistoryPoint[],
): GapBand[] {
  const clipped = gaps
    .map((gap) => ({
      ...gap,
      fromMs: Math.max(fromMs, gap.fromMs),
      toMs: Math.min(toMs, gap.toMs),
    }))
    .filter((gap) => gap.toMs > gap.fromMs)
    .sort((a, b) => a.fromMs - b.fromMs || a.toMs - b.toMs);
  const merged = mergeSortedBands(clipped);
  return points === undefined ? merged : alignGapBands(merged, points, fromMs, toMs);
}

function overlapsGap(previousT: number, nextT: number, bands: readonly GapBand[]): boolean {
  return bands.some((band) => band.toMs > previousT && band.fromMs < nextT);
}

// ---------------------------------------------------------------------------
// Segmentation
// ---------------------------------------------------------------------------

/**
 * Split one stored series into drawable segments. A segment ends at a null
 * reading, an omitted bucket (the next timestamp is further away than the
 * bucket step allows), a recorded gap overlapping the interval, or a
 * downsampler-marked discontinuity.
 */
/** Empty SQL buckets are normal when sampling is slower than the query grid. */
export function continuityStepMs(
  stepMs: number,
  tier: "1s" | "raw" | "10s" | "1m" | "1h",
  expectedIntervalMs?: number | null,
): number {
  const tierMs =
    tier === "1h" ? 3_600_000 :
      tier === "1m" ? 60_000 :
        tier === "10s" ? 10_000 :
          tier === "1s" ? 1_000 : 100;
  return Math.max(stepMs, tierMs, expectedIntervalMs ?? 0);
}

export function splitSeries(
  points: readonly ChartPoint[],
  options: { stepMs: number; gaps: readonly HistoryGap[]; fromMs: number; toMs: number },
): ChartPoint[][] {
  const sorted = [...points].sort((a, b) => a.t - b.t);
  const bands = gapBands(options.gaps, options.fromMs, options.toMs);
  const step = Math.max(1, options.stepMs);
  const segments: ChartPoint[][] = [];
  let current: ChartPoint[] = [];
  const flush = (): void => {
    if (current.length > 0) segments.push(current);
    current = [];
  };

  for (const point of sorted) {
    if (!plottable(point)) {
      flush();
      continue;
    }
    const previous = current.at(-1);
    if (previous !== undefined) {
      const brokeByTime =
        !point.merged &&
        !previous.merged &&
        (point.t < previous.t || point.t - previous.t > step * 1.5);
      const brokeByGap = point.gapBefore === true || overlapsGap(previous.t, point.t, bands);
      if (brokeByTime || brokeByGap) flush();
    }
    current.push(point);
  }
  flush();
  return segments;
}

/**
 * Reduce points for drawing while keeping the first timestamp, the true
 * min/max envelope and the average. Null buckets are preserved as breaks;
 * discontinuities are never merged. Groups are marked `merged` so a later
 * segmentation pass does not mistake the intentional spacing for a gap.
 */
export function downsamplePoints(
  points: readonly HistoryPoint[],
  maxPoints: number,
  stepMs: number,
): ChartPoint[] {
  if (maxPoints <= 0) return [];
  const sorted = [...points].sort((a, b) => a.t - b.t);
  if (sorted.length <= maxPoints) return sorted.map((point) => ({ ...point }));
  const target = Math.max(2, Math.ceil(sorted.length / maxPoints));
  const step = Math.max(1, stepMs);
  const output: ChartPoint[] = [];
  let group: HistoryPoint[] = [];
  let groupStartsAfterBreak = false;
  let previousPlottable: HistoryPoint | null = null;

  const flush = (): void => {
    if (group.length === 0) return;
    let min: number | null = null;
    let max: number | null = null;
    let sum = 0;
    let count = 0;
    for (const point of group) {
      const pointMin = finiteOrNull(point.min);
      const pointMax = finiteOrNull(point.max);
      if (pointMin !== null && (min === null || pointMin < min)) min = pointMin;
      if (pointMax !== null && (max === null || pointMax > max)) max = pointMax;
      const avg = finiteOrNull(point.avg);
      if (avg !== null) {
        sum += avg;
        count += 1;
      }
    }
    output.push({
      t: group[0]!.t,
      min,
      max,
      avg: count > 0 ? sum / count : null,
      ...(group.length > 1 ? { merged: true } : {}),
      ...(groupStartsAfterBreak ? { gapBefore: true } : {}),
    });
    group = [];
    groupStartsAfterBreak = false;
  };

  for (const point of sorted) {
    if (!plottable(point)) {
      flush();
      output.push({ ...point });
      previousPlottable = null;
      continue;
    }
    const continuous =
      previousPlottable !== null &&
      point.t >= previousPlottable.t &&
      point.t - previousPlottable.t <= step * 1.5;
    if (group.length > 0 && (!continuous || group.length >= target)) {
      flush();
    }
    if (group.length === 0) groupStartsAfterBreak = previousPlottable !== null && !continuous;
    group.push(point);
    previousPlottable = point;
  }
  flush();
  return output;
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

export function seriesStats(points: readonly HistoryPoint[]): SeriesStats {
  let min: number | null = null;
  let max: number | null = null;
  let sum = 0;
  let count = 0;
  let latest: number | null = null;
  let latestT: number | null = null;
  let firstT: number | null = null;
  let lastT: number | null = null;
  for (const point of points) {
    if (point.avg === null) continue;
    if (firstT === null || point.t < firstT) firstT = point.t;
    if (lastT === null || point.t > lastT) lastT = point.t;
    const pointMin = finiteOrNull(point.min);
    const pointMax = finiteOrNull(point.max);
    if (pointMin !== null && (min === null || pointMin < min)) min = pointMin;
    if (pointMax !== null && (max === null || pointMax > max)) max = pointMax;
    if (point.avg !== null && Number.isFinite(point.avg)) {
      sum += point.avg;
      count += 1;
      latest = point.avg;
      latestT = point.t;
    }
  }
  return { count, min, max, avg: count > 0 ? sum / count : null, latest, latestT, firstT, lastT };
}

export function missingValueLabel(): string {
  return MISSING;
}

// ---------------------------------------------------------------------------
// Y domain and ticks
// ---------------------------------------------------------------------------

export type YDomain = { min: number; max: number; ticks: number[] };

function niceStep(rough: number): number {
  if (!Number.isFinite(rough) || rough <= 0) return 1;
  const magnitude = Math.pow(10, Math.floor(Math.log10(rough)));
  const residual = rough / magnitude;
  const multiplier = residual <= 1 ? 1 : residual <= 2 ? 2 : residual <= 5 ? 5 : 10;
  return multiplier * magnitude;
}

export function niceTicks(min: number, max: number, target = 4): number[] {
  if (!Number.isFinite(min) || !Number.isFinite(max)) return [0, 1];
  if (max <= min) return [min];
  const step = niceStep((max - min) / Math.max(1, target));
  const start = Math.ceil(min / step) * step;
  const ticks: number[] = [];
  for (let value = start; value <= max + step * 0.001 && ticks.length < 20; value += step) {
    ticks.push(Number(value.toPrecision(12)));
  }
  return ticks.length > 0 ? ticks : [min, max];
}

/**
 * Unit-aware domain. Percent charts always cover 0–100 so two ranges remain
 * comparable; temperatures use a padded local range; byte/count/rate/latency
 * charts baseline at zero so magnitudes stay honest.
 */
export function buildYDomain(unit: MetricUnit | string, values: readonly (number | null)[]): YDomain {
  if (unit === "percent") return { min: 0, max: 100, ticks: [0, 25, 50, 75, 100] };
  const finite = values.filter((value): value is number => value !== null && Number.isFinite(value));
  if (finite.length === 0) return { min: 0, max: 1, ticks: [0, 1] };

  let min = Math.min(...finite);
  let max = Math.max(...finite);
  if (unit === "celsius") {
    const padding = Math.max(1, (max - min) * 0.1);
    min = Math.floor((min - padding) / 5) * 5;
    max = Math.ceil((max + padding) / 5) * 5;
  } else {
    min = 0;
    max = max > 0 ? max : 1;
  }
  if (max <= min) max = min + 1;
  const step = niceStep((max - min) / 4);
  const domainMin = Math.floor(min / step) * step;
  const domainMax = Math.ceil(max / step) * step;
  // The HTML axis labels span the frame evenly, so its end ticks must be
  // the actual domain endpoints, including when the peak is between ticks.
  const ticks = niceTicks(domainMin, domainMax, 4);
  return { min: domainMin, max: domainMax, ticks };
}

// ---------------------------------------------------------------------------
// X ticks
// ---------------------------------------------------------------------------

export function timeTicks(fromMs: number, toMs: number, count = 4): number[] {
  const ticks = Math.max(2, Math.floor(count));
  const span = Math.max(0, toMs - fromMs);
  return Array.from({ length: ticks }, (_, index) => fromMs + (span * index) / (ticks - 1));
}
