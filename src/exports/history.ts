/**
 * History export documents.
 *
 * Only the currently loaded result and the series visible in the view are
 * exported; nothing triggers another backend query. A result may be rolled up
 * and/or partial, so every document states its tier, bucket step, gaps and
 * partial flag instead of claiming a full raw dump. Units come from each
 * series so metrics added by a newer hub still export correctly even when
 * this build does not know their catalog metadata.
 */

import type { MachineState } from "../contracts";
import type { HistoryGap, HistoryResult, HistorySeries } from "../hub/storage/history-query";
import { gapBands, seriesStats } from "../history/chart";
import { gpuCards, metricOption, metricUnitLabel, resourceLabel } from "../history/metrics";
import { rangePreset, type RangeId } from "../history/presentation";
import { csvDocument, type CsvValue } from "./csv";
import { exportFilename } from "./filenames";

export type HistoryExportInput = {
  machine: MachineState;
  result: HistoryResult;
  range: RangeId;
  selectedMetrics: readonly string[];
  hiddenCards: ReadonlySet<string>;
  hiddenSeries: ReadonlySet<string>;
  coreIds?: readonly string[];
  /** Local browser receipt time for the loaded result. */
  loadedAt: number;
  exportedAt?: number;
};

export type HistoryExportSeries = {
  metric: string;
  label: string;
  resourceId: string;
  resourceLabel: string;
  unit: string;
  unitLabel: string;
  stats: ReturnType<typeof seriesStats>;
  points: Array<{
    t: number;
    tIso: string;
    bucketStartMs: number;
    bucketStartIso: string;
    bucketEndMs: number;
    bucketEndIso: string;
    min: number | null;
    max: number | null;
    avg: number | null;
    state: "recorded" | "unavailable";
  }>;
};

const TIER_LABEL: Record<HistoryResult["tier"], string> = {
  "1s": "1-second RAM summaries",
  raw: "raw samples",
  "10s": "10-second summaries",
  "1m": "1-minute rollups",
  "1h": "1-hour rollups",
};

export function machineLabel(machine: MachineState): string {
  return machine.displayName?.trim() || machine.hostname.trim() || machine.machineId;
}

export function historySeriesKey(metric: string, resourceId: string): string {
  return `${metric}\u0000${resourceId}`;
}

/**
 * Match the charts: selected metrics, visible GPU cards, and series the user
 * has not hidden in the legend. Metrics with no presentation metadata still
 * pass through; their unit and label come from the loaded result.
 */
export function visibleHistorySeries(input: HistoryExportInput): HistorySeries[] {
  const selected = new Set(input.selectedMetrics);
  const visibleCards = new Set(
    gpuCards(input.machine, input.result.series)
      .filter((card) => !input.hiddenCards.has(card.id))
      .map((card) => card.id),
  );

  return input.result.series.filter((series) => {
    if (!selected.has(series.metric)) return false;
    if (input.hiddenSeries.has(historySeriesKey(series.metric, series.resourceId))) return false;
    if (series.metric === "cpu.core" && input.coreIds !== undefined && !input.coreIds.includes(series.resourceId)) return false;
    // The family prefix is stable even when a newer hub records a GPU metric
    // this build has no catalog entry for.
    if (series.metric.startsWith("gpu.") && series.resourceId !== "" && !visibleCards.has(series.resourceId)) {
      return false;
    }
    return true;
  });
}

function iso(ms: number): string {
  return new Date(ms).toISOString();
}

function metricLabel(metric: string): string {
  return metricOption(metric)?.label ?? metric;
}

function seriesLabel(input: HistoryExportInput, series: HistorySeries): string {
  return resourceLabel(input.machine, series.metric, series.resourceId);
}

function exportSeries(input: HistoryExportInput): HistoryExportSeries[] {
  return visibleHistorySeries(input).map((series) => ({
    metric: series.metric,
    label: metricLabel(series.metric),
    resourceId: series.resourceId,
    resourceLabel: seriesLabel(input, series),
    unit: series.unit,
    unitLabel: metricUnitLabel(series.unit),
    stats: seriesStats(series.points),
    points: series.points.map((point) => {
      const bucketEndMs = Math.min(input.result.toMs, point.t + input.result.stepMs);
      const available = point.min !== null || point.max !== null || point.avg !== null;
      return {
        t: point.t,
        tIso: iso(point.t),
        bucketStartMs: point.t,
        bucketStartIso: iso(point.t),
        bucketEndMs,
        bucketEndIso: iso(bucketEndMs),
        min: point.min,
        max: point.max,
        avg: point.avg,
        state: available ? "recorded" : "unavailable",
      };
    }),
  }));
}

function gaps(input: HistoryExportInput): Array<HistoryGap & { fromIso: string; toIso: string }> {
  return gapBands(input.result.gaps, input.result.fromMs, input.result.toMs).map((gap) => ({
    ...gap,
    fromIso: iso(gap.fromMs),
    toIso: iso(gap.toMs),
  }));
}

function missingSelectedMetrics(input: HistoryExportInput): string[] {
  const recorded = new Set(input.result.series.map((series) => series.metric));
  return input.selectedMetrics.filter((metric) => !recorded.has(metric));
}

function excludedSeriesCount(input: HistoryExportInput): number {
  const selected = new Set(input.selectedMetrics);
  const visible = new Set(visibleHistorySeries(input).map((series) => historySeriesKey(series.metric, series.resourceId)));
  return input.result.series.filter(
    (series) =>
      selected.has(series.metric) && !visible.has(historySeriesKey(series.metric, series.resourceId)),
  ).length;
}

function notes(input: HistoryExportInput, missing: readonly string[]): string[] {
  const result = input.result;
  const output = [
    "Only the visible series from the loaded History result are included; this export does not query more data.",
  ];
  if (result.tier !== "raw") {
    output.push(`Values are ${TIER_LABEL[result.tier]}, not individual raw samples.`);
  }
  if (result.truncated) {
    output.push("The loaded result is partial: the hub limited or shortened this history query.");
  }
  const gapList = gaps(input);
  if (gapList.length > 0) {
    output.push(`${gapList.length} recorded data gap${gapList.length === 1 ? "" : "s"} included.`);
  }
  if (missing.length > 0) {
    output.push(`Selected metrics without stored data: ${missing.join(", ")}.`);
  }
  const excluded = excludedSeriesCount(input);
  if (excluded > 0) {
    output.push(`${excluded} selected series hidden in this view were not exported.`);
  }
  return output;
}

function metadataRows(input: HistoryExportInput, exportedAt: number): CsvValue[][] {
  const preset = rangePreset(input.range);
  const result = input.result;
  const visible = visibleHistorySeries(input);
  const missing = missingSelectedMetrics(input);
  const gapList = gaps(input);
  const query = input.selectedMetrics.map(metricLabel).join("; ");
  return [
    ["Rigscope History export"],
    ["Scope", "Visible series from the loaded History result"],
    ["Machine", machineLabel(input.machine)],
    ["Machine ID", input.machine.machineId],
    ["Query", query],
    ["Range", `${preset.label} (${iso(result.fromMs)} – ${iso(result.toMs)})`],
    ["Resolution", `${TIER_LABEL[result.tier]} · ${result.stepMs} ms buckets`],
    ["Loaded at", iso(input.loadedAt)],
    ["Exported at", iso(exportedAt)],
    ["Partial", result.truncated ? "true" : "false"],
    ["Gaps", String(gapList.length)],
    ["Visible series", String(visible.length)],
    ["Selected without stored data", missing.length > 0 ? missing.join(", ") : "none"],
    ["Note", notes(input, missing).join(" ")],
    [],
  ];
}

const HISTORY_HEADER: readonly CsvValue[] = [
  "record_type",
  "metric",
  "metric_label",
  "resource_id",
  "resource_label",
  "unit",
  "bucket_start_ms",
  "bucket_start_iso",
  "bucket_end_ms",
  "bucket_end_iso",
  "min",
  "max",
  "avg",
  "value_state",
  "gap_kind",
  "gap_reason",
  "gap_dropped_records",
];

export function buildHistoryCsv(input: HistoryExportInput): string {
  const exportedAt = input.exportedAt ?? Date.now();
  const rows: CsvValue[][] = metadataRows(input, exportedAt);
  rows.push([...HISTORY_HEADER]);

  for (const series of exportSeries(input)) {
    for (const point of series.points) {
      rows.push([
        "bucket",
        series.metric,
        series.label,
        series.resourceId,
        series.resourceLabel,
        series.unit,
        point.bucketStartMs,
        point.bucketStartIso,
        point.bucketEndMs,
        point.bucketEndIso,
        point.min,
        point.max,
        point.avg,
        point.state,
        "",
        "",
        "",
      ]);
    }
  }

  for (const gap of gaps(input)) {
    rows.push([
      "gap",
      "",
      "",
      "",
      "",
      "",
      gap.fromMs,
      gap.fromIso,
      gap.toMs,
      gap.toIso,
      "",
      "",
      "",
      "gap",
      gap.kind,
      gap.reason ?? "",
      gap.droppedRecords ?? "",
    ]);
  }

  return csvDocument(rows);
}

export function buildHistoryJson(input: HistoryExportInput): string {
  const exportedAt = input.exportedAt ?? Date.now();
  const preset = rangePreset(input.range);
  const result = input.result;
  const visible = exportSeries(input);
  const missing = missingSelectedMetrics(input);
  const gapList = gaps(input);
  const excluded = excludedSeriesCount(input);
  const document = {
    app: "Rigscope",
    kind: "history",
    exportedAt: iso(exportedAt),
    scope: {
      description: "Visible series from the loaded History result",
      machine: {
        id: input.machine.machineId,
        name: machineLabel(input.machine),
        hostname: input.machine.hostname,
        connected: input.machine.connected,
        lastReceivedAt: input.machine.lastReceivedAt,
      },
      query: {
        range: {
          id: preset.id,
          label: preset.label,
          fromMs: result.fromMs,
          fromIso: iso(result.fromMs),
          toMs: result.toMs,
          toIso: iso(result.toMs),
        },
        selectedMetrics: [...input.selectedMetrics],
        visibleSeries: visible.length,
        hiddenSeriesCount: excluded,
      },
    },
    resolution: {
      tier: result.tier,
      label: TIER_LABEL[result.tier],
      stepMs: result.stepMs,
      expectedIntervalMs: input.machine.expectedIntervalMs ?? null,
    },
    loadedAt: iso(input.loadedAt),
    partial: result.truncated,
    gaps: gapList,
    series: visible.map((series) => ({
      metric: series.metric,
      label: series.label,
      resourceId: series.resourceId,
      resourceLabel: series.resourceLabel,
      unit: series.unit,
      unitLabel: series.unitLabel,
      summary: {
        points: series.stats.count,
        min: series.stats.min,
        max: series.stats.max,
        averageOfBucketAverages: series.stats.avg,
        latest: series.stats.latest,
        latestAt: series.stats.latestT === null ? null : iso(series.stats.latestT),
        firstAt: series.stats.firstT === null ? null : iso(series.stats.firstT),
        lastAt: series.stats.lastT === null ? null : iso(series.stats.lastT),
      },
      points: series.points,
    })),
    missingData: missing.map((metric) => ({
      metric,
      label: metricLabel(metric),
      reason: "No stored series in the loaded result.",
    })),
    notes: notes(input, missing),
  };
  return `${JSON.stringify(document, null, 2)}\n`;
}

export function historyExportFilename(
  input: Pick<HistoryExportInput, "machine" | "range">,
  extension: "csv" | "json",
  atMs = Date.now(),
): string {
  return exportFilename("history", machineLabel(input.machine), atMs, extension);
}
