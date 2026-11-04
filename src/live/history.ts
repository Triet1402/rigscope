import type { HubSnapshot } from "../contracts";
import { FRESHNESS_THRESHOLD_MS, SAMPLE_HISTORY_LIMIT, type SampleHistoryPoint } from "./types";
import { freshnessThresholdFor } from "./presentation";

/** Immutable, bounded history for the machines in the current authorized snapshot. */
export function historyForSnapshot(
  previous: ReadonlyMap<string, readonly SampleHistoryPoint[]>,
  snapshot: HubSnapshot,
): ReadonlyMap<string, readonly SampleHistoryPoint[]> {
  const next = new Map<string, readonly SampleHistoryPoint[]>();
  for (const machine of snapshot.machines) {
    const metrics = machine.metrics;
    if (!metrics) continue;
    const sampleId = `${snapshot.hubRunId}\0${metrics.collectorId}\0${metrics.runId}\0${metrics.sequence}`;
    const existing = previous.get(machine.machineId) ?? [];
    const last = existing.at(-1);
    if (last?.sampleId === sampleId) {
      next.set(machine.machineId, existing);
      continue;
    }
    // A hub/collector restart begins a separate series; never join two runs.
    const runPrefix = sampleId.slice(0, sampleId.lastIndexOf("\0") + 1);
    const retained = last?.sampleId.startsWith(runPrefix) ? existing : [];
    const memory = metrics.payload.memory;
    // The gap threshold is captured with the point, from the cadence the
    // machine advertised when it arrived. A later cadence change updates
    // only new points, so old gaps can never be re-judged retroactively.
    next.set(machine.machineId, [...retained, {
      sampleId,
      receivedAtLocal: Date.parse(metrics.receivedAt),
      cpuPercent: metrics.payload.cpu.status === "ok" ? metrics.payload.cpu.usagePercent : null,
      memoryPercent: memory.status === "ok" && memory.totalBytes > 0
        ? 100 * memory.usedBytes / memory.totalBytes : null,
      gapThresholdMs: freshnessThresholdFor(machine.expectedIntervalMs),
    }].slice(-SAMPLE_HISTORY_LIMIT));
  }
  return next;
}

/**
 * A point's gap threshold: what the machine advertised when the point
 * arrived, or the disclosed 15 s fallback when that metadata was omitted
 * (hand-built test fixtures).
 */
function pointGapThreshold(point: SampleHistoryPoint): number {
  return typeof point.gapThresholdMs === "number" && Number.isFinite(point.gapThresholdMs)
    && point.gapThresholdMs > 0
    ? point.gapThresholdMs
    : FRESHNESS_THRESHOLD_MS;
}

/** Missing readings and long receipt gaps leave a gap rather than an invented line. */
export function cpuChartPath(points: readonly SampleHistoryPoint[], width = 600, height = 140): string | null {
  if (points.length < 2) return null;
  const pad = 4;
  const start = points[0].receivedAtLocal;
  const span = Math.max(1_000, points.at(-1)!.receivedAtLocal - start);
  let path = "";
  let previous: SampleHistoryPoint | undefined;
  for (const point of points) {
    if (point.cpuPercent === null) { previous = undefined; continue; }
    const x = pad + (point.receivedAtLocal - start) / span * (width - 2 * pad);
    const y = height - pad - point.cpuPercent / 100 * (height - 2 * pad);
    // Each point keeps the threshold it arrived with. Across a cadence
    // transition, the stricter (smaller) endpoint threshold applies, so a
    // gap that was abnormal under the earlier cadence cannot be bridged by
    // a later, slower configuration. Equal gaps >= the threshold — exactly
    // matching the freshness rule — break the line.
    const threshold = previous
      ? Math.min(pointGapThreshold(previous), pointGapThreshold(point))
      : FRESHNESS_THRESHOLD_MS;
    const continuous = previous && point.receivedAtLocal >= previous.receivedAtLocal
      && point.receivedAtLocal - previous.receivedAtLocal < threshold;
    path += `${continuous ? "L" : "M"}${x.toFixed(2)},${y.toFixed(2)} `;
    previous = point;
  }
  return path.trim() || null;
}
