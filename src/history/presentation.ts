/**
 * Browser-safe formatting, range policy and response validation for History.
 *
 * No DOM and no Node APIs: the same functions run in the page, in tests, and
 * in any future node-side snapshot tooling. Values are never fabricated: a
 * null reading formats as an em dash, never as zero.
 */

import type {
  HistoryGap,
  HistoryResult,
  HistorySeries,
  HistoryQuery,
} from "../hub/storage/history-query";
import type { MetricUnit } from "../hub/storage/series";

export type RangeId = "10m" | "1h" | "6h" | "24h" | "7d";

export type RangePreset = {
  id: RangeId;
  /** Button text; the accessible name is `label`. */
  shortLabel: string;
  label: string;
  ms: number;
  /** Auto-refresh cadence while the tab is visible. */
  refreshMs: number;
};

export const RANGE_PRESETS: readonly RangePreset[] = [
  { id: "10m", shortLabel: "10m", label: "Last 10 minutes", ms: 10 * 60_000, refreshMs: 1_000 },
  { id: "1h", shortLabel: "1h", label: "Last hour", ms: 60 * 60_000, refreshMs: 1_000 },
  { id: "6h", shortLabel: "6h", label: "Last 6 hours", ms: 6 * 60 * 60_000, refreshMs: 1_000 },
  { id: "24h", shortLabel: "24h", label: "Last 24 hours", ms: 24 * 60 * 60_000, refreshMs: 1_000 },
  { id: "7d", shortLabel: "7d", label: "Last 7 days", ms: 7 * 24 * 60 * 60_000, refreshMs: 1_000 },
] as const;

export function rangePreset(id: string): RangePreset {
  return RANGE_PRESETS.find((preset) => preset.id === id) ?? RANGE_PRESETS[0]!;
}

export function isRangeId(value: string | null): value is RangeId {
  return value !== null && RANGE_PRESETS.some((preset) => preset.id === value);
}

export function rangeBounds(id: RangeId, nowMs: number): { fromMs: number; toMs: number } {
  const preset = rangePreset(id);
  return { fromMs: nowMs - preset.ms, toMs: nowMs };
}

export function refreshIntervalMs(id: RangeId): number {
  return rangePreset(id).refreshMs;
}

// ---------------------------------------------------------------------------
// Value and time formatting
// ---------------------------------------------------------------------------

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] as const;

function scaledBytes(value: number): { value: number; unit: string } {
  let scaled = Math.abs(value);
  let index = 0;
  while (scaled >= 1024 && index < BYTE_UNITS.length - 1) {
    scaled /= 1024;
    index += 1;
  }
  return { value: value < 0 ? -scaled : scaled, unit: BYTE_UNITS[index]! };
}

function trimmed(value: number, digits: number): string {
  const rounded = Number(value.toFixed(digits));
  return String(rounded);
}

function formatBytes(value: number, suffix = ""): string {
  const { value: scaled, unit } = scaledBytes(value);
  const digits = Math.abs(scaled) >= 100 ? 0 : 1;
  return `${trimmed(scaled, digits)} ${unit}${suffix}`;
}

/**
 * Human-readable metric value. `null` and non-finite values are missing —
 * they format as an em dash, never as 0.
 */
export function formatMetricValue(value: number | null | undefined, unit: MetricUnit | string): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return "—";
  switch (unit) {
    case "percent":
      return `${trimmed(value, 1)}%`;
    case "bytes":
      return formatBytes(value);
    case "bytes_per_second":
      return formatBytes(value, "/s");
    case "celsius":
      return `${trimmed(value, 1)}°C`;
    case "watts":
      return `${trimmed(value, 1)} W`;
    case "milliseconds":
      return `${trimmed(value, Math.abs(value) < 10 ? 2 : 1)} ms`;
    default:
      return trimmed(value, 2);
  }
}

/** Compact value for axis ticks (fewer decimals, still unit-bearing). */
export function formatAxisValue(value: number, unit: MetricUnit | string): string {
  if (!Number.isFinite(value)) return "—";
  switch (unit) {
    case "percent":
      return `${trimmed(value, 0)}%`;
    case "bytes":
    case "bytes_per_second": {
      const suffix = unit === "bytes_per_second" ? "/s" : "";
      const { value: scaled, unit: unitLabel } = scaledBytes(value);
      const digits = Math.abs(scaled) >= 10 || scaled === 0 ? 0 : 1;
      return `${trimmed(scaled, digits)} ${unitLabel}${suffix}`;
    }
    case "celsius":
      return `${trimmed(value, 0)}°C`;
    case "watts":
      return `${trimmed(value, 0)} W`;
    case "milliseconds":
      return `${trimmed(value, Math.abs(value) < 10 ? 1 : 0)} ms`;
    default:
      return trimmed(value, 1);
  }
}

export function formatClock(ms: number): string {
  return new Date(ms).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

export function formatDateTime(ms: number): string {
  return new Date(ms).toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "medium",
  });
}

/** Axis label appropriate to the visible span. */
export function formatTimeTick(ms: number, spanMs: number): string {
  const date = new Date(ms);
  if (spanMs >= 48 * 60 * 60_000) {
    return date.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }
  if (spanMs >= 24 * 60 * 60_000) {
    return date.toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    });
  }
  return date.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

export function formatTableTime(ms: number, spanMs: number): string {
  return spanMs >= 24 * 60 * 60_000 ? formatDateTime(ms) : formatClock(ms);
}

export function formatGapDuration(ms: number): string {
  const duration = Math.max(0, ms);
  if (duration >= 3_600_000) return `${trimmed(duration / 3_600_000, 1)} h`;
  if (duration >= 60_000) return `${trimmed(duration / 60_000, 1)} min`;
  return `${trimmed(duration / 1_000, 1)} s`;
}

export function gapSummary(gaps: readonly HistoryGap[], fromMs: number, toMs: number): string {
  const inRange = gaps.filter((gap) => gap.toMs >= fromMs && gap.fromMs <= toMs);
  if (inRange.length === 0) return "No recorded gaps in this range.";
  const dropped = inRange.filter((gap) => gap.kind === "dropped").length;
  const missing = inRange.length - dropped;
  const parts: string[] = [];
  if (missing > 0) parts.push(`${missing} missed interval${missing === 1 ? "" : "s"}`);
  if (dropped > 0) parts.push(`${dropped} dropped report${dropped === 1 ? "" : "s"}`);
  const reasons = [...new Set(inRange.map((gap) => gap.reason).filter((reason): reason is string => typeof reason === "string" && reason.length > 0))];
  const reasonText = reasons.length > 0 ? ` (${reasons.slice(0, 3).join("; ")})` : "";
  return `${parts.join(" and ")}${reasonText}.`;
}

// ---------------------------------------------------------------------------
// Response validation
// ---------------------------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function finite(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}

function nullableFinite(value: unknown): value is number | null {
  return value === null || finite(value);
}

/** Defensive check; a malformed hub response becomes an honest error, not a broken chart. */
export function parseHistoryResult(value: unknown): HistoryResult | null {
  if (!isRecord(value)) return null;
  if (typeof value.machineId !== "string" || value.machineId.length === 0) return null;
  if (!finite(value.fromMs) || !finite(value.toMs) || value.toMs <= value.fromMs) return null;
  if (!finite(value.stepMs) || value.stepMs <= 0) return null;
  if (
    value.tier !== "1s" && value.tier !== "raw" && value.tier !== "10s" &&
    value.tier !== "1m" && value.tier !== "1h"
  ) return null;
  if (typeof value.truncated !== "boolean" || !finite(value.elapsedMs)) return null;
  if (!Array.isArray(value.series) || !Array.isArray(value.gaps)) return null;

  const series: HistorySeries[] = [];
  for (const entry of value.series) {
    if (!isRecord(entry)) return null;
    if (typeof entry.metric !== "string" || typeof entry.resourceId !== "string") return null;
    if (typeof entry.unit !== "string" || !Array.isArray(entry.points)) return null;
    const points: HistorySeries["points"] = [];
    for (const point of entry.points) {
      if (!isRecord(point) || !finite(point.t)) return null;
      if (!nullableFinite(point.min) || !nullableFinite(point.max) || !nullableFinite(point.avg)) return null;
      points.push({ t: point.t, min: point.min, max: point.max, avg: point.avg });
    }
    series.push({ metric: entry.metric, resourceId: entry.resourceId, unit: entry.unit, points });
  }

  const gaps: HistoryGap[] = [];
  for (const entry of value.gaps) {
    if (!isRecord(entry) || !finite(entry.fromMs) || !finite(entry.toMs)) return null;
    if (entry.kind !== "missing" && entry.kind !== "dropped") return null;
    const gap: HistoryGap = { fromMs: entry.fromMs, toMs: entry.toMs, kind: entry.kind };
    if (typeof entry.reason === "string") gap.reason = entry.reason;
    if (finite(entry.droppedRecords)) gap.droppedRecords = entry.droppedRecords;
    gaps.push(gap);
  }

  return {
    machineId: value.machineId,
    fromMs: value.fromMs,
    toMs: value.toMs,
    stepMs: value.stepMs,
    tier: value.tier,
    series,
    gaps,
    truncated: value.truncated,
    elapsedMs: value.elapsedMs,
  };
}

export function historyRequestUrl(
  machineId: string,
  metrics: readonly string[],
  range: RangeId,
  nowMs: number,
  maxPoints = 600,
  coreIds?: readonly string[],
): string {
  const { fromMs, toMs } = rangeBounds(range, nowMs);
  const params = new URLSearchParams({
    machineId,
    metrics: metrics.join(","),
    from: String(Math.floor(fromMs)),
    to: String(Math.floor(toMs)),
    maxPoints: String(maxPoints),
  });
  if (coreIds !== undefined) params.set("coreIds", coreIds.join(","));
  return `/api/history?${params.toString()}`;
}

/** Kept for callers that assemble a query object directly. */
export type { HistoryQuery, HistoryResult, HistorySeries, HistoryGap };
