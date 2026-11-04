/**
 * Deterministic presentation helpers for the live fleet/machine views.
 *
 * Pure functions over the wire contracts: no React, no DOM, no Node APIs,
 * so they are unit-testable under the Node test runner and safe to import
 * anywhere in the browser bundle.
 *
 * Honesty rules encoded here:
 * - Liveness is judged by hub receipt time (receivedAt/lastReceivedAt);
 *   the collector's capture timestamp is never used to decide freshness.
 * - Staleness is cadence-aware: max(15 s, 3 × the machine's advertised
 *   delivery interval), with a disclosed 15 s fallback when the machine
 *   has no cadence metadata. The UI never invents a cadence.
 * - Capture timestamps are only compared against receipt time to *flag
 *   clock skew*, with a local tolerance.
 * - Unavailable fields carry their reason; they never render as zero.
 * - Stale fields retain their last-known value, clearly labeled.
 */

import type {
  BytesUsageMetric,
  CpuMetric,
  MetricsRecord,
  MachineState,
} from "../contracts";
import {
  FRESHNESS_THRESHOLD_MS,
  SKEW_TOLERANCE_MS,
} from "./types";

// ---------------------------------------------------------------------------
// Freshness (receipt-time based, cadence-aware)
// ---------------------------------------------------------------------------

export type Freshness = "live" | "stale" | "unknown";

/**
 * Resolves a machine's receipt-freshness threshold from its advertised
 * delivery cadence: max(15 s, 3 × interval). Unknown or invalid cadence
 * metadata (absent, or null before/without hello) falls back to the
 * disclosed 15 s; the UI never invents a cadence.
 * Deterministic so callers can display the same threshold they apply.
 */
export function freshnessThresholdFor(
  expectedIntervalMs: number | null | undefined,
): number {
  if (
    expectedIntervalMs === null ||
    expectedIntervalMs === undefined ||
    typeof expectedIntervalMs !== "number" ||
    !Number.isFinite(expectedIntervalMs) ||
    expectedIntervalMs <= 0
  ) {
    return FRESHNESS_THRESHOLD_MS;
  }
  return Math.max(FRESHNESS_THRESHOLD_MS, 3 * expectedIntervalMs);
}

/**
 * Human interval for disclosures: whole seconds under a minute, whole
 * minutes under an hour, hours beyond that; fractional values keep one
 * decimal. Deterministic so tests and UI copy agree ("15 s", "1.5 min").
 */
export function formatIntervalMs(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 60_000) {
    const seconds = ms / 1000;
    return Number.isInteger(seconds) ? `${seconds} s` : `${seconds.toFixed(1)} s`;
  }
  if (ms < 3_600_000) {
    const minutes = ms / 60_000;
    return Number.isInteger(minutes) ? `${minutes} min` : `${minutes.toFixed(1)} min`;
  }
  const hours = ms / 3_600_000;
  return Number.isInteger(hours) ? `${hours} h` : `${hours.toFixed(1)} h`;
}

/**
 * Classifies a receipt timestamp against a threshold. `now` is passed in
 * by callers so results are deterministic in tests; the threshold defaults
 * to the 15 s fallback — cadence-aware callers pass the machine's resolved
 * threshold (see `freshnessThresholdFor`).
 */
export function freshness(
  lastReceivedAt: string | null,
  now: number,
  thresholdMs: number = FRESHNESS_THRESHOLD_MS,
): Freshness {
  if (lastReceivedAt === null) return "unknown";
  const epoch = Date.parse(lastReceivedAt);
  if (!Number.isFinite(epoch)) return "unknown";
  // receivedAt slightly in the past vs "now" is normal; a future receipt
  // still counts as live (last received is technically the freshest we know).
  const age = now - epoch;
  if (age >= thresholdMs) return "stale";
  return "live";
}

/**
 * Receipt-time freshness for one machine's last known sample. The threshold
 * follows the machine's advertised cadence (max(15 s, 3 × interval), 15 s
 * fallback when unknown). An explicit threshold still overrides.
 */
export function stateFreshness(
  state: MachineState,
  now: number,
  thresholdMs: number = freshnessThresholdFor(state.expectedIntervalMs),
): Freshness {
  return freshness(state.lastReceivedAt, now, thresholdMs);
}

// ---------------------------------------------------------------------------
// Number and age formatting
// ---------------------------------------------------------------------------

/** Formats a byte count with binary units; null/undefined renders as "—". */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes)) return "—";
  if (bytes < 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"] as const;
  let value = bytes;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex += 1;
  }
  if (unitIndex === 0) return `${Math.round(value)} B`;
  return `${value.toFixed(1)} ${units[unitIndex]}`;
}

/** Formats a 0..100 percentage at one decimal; null/undefined renders as "—". */
export function formatPercent(percent: number | null | undefined): string {
  if (percent === null || percent === undefined || !Number.isFinite(percent)) return "—";
  return `${percent.toFixed(1)}%`;
}

/**
 * Human age from a number of milliseconds. Deterministic given the inputs;
 * callers pass `now - epochMs` so tests stay fixed.
 */
export function formatAge(deltaMs: number | null): string {
  if (deltaMs === null || !Number.isFinite(deltaMs)) return "—";
  const clamped = Math.max(0, deltaMs);
  const seconds = Math.round(clamped / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    const restSeconds = seconds % 60;
    return restSeconds === 0 ? `${minutes}m ago` : `${minutes}m ${restSeconds}s ago`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    const restMinutes = minutes % 60;
    return restMinutes === 0 ? `${hours}h ago` : `${hours}h ${restMinutes}m ago`;
  }
  const days = Math.floor(hours / 24);
  const restHours = hours % 24;
  return restHours === 0 ? `${days}d ago` : `${days}d ${restHours}h ago`;
}

/** Age since a receipt timestamp (null when the timestamp is absent). */
export function ageSince(iso: string | null, now: number): number | null {
  if (iso === null) return null;
  const epoch = Date.parse(iso);
  if (!Number.isFinite(epoch)) return null;
  // A future-appearing receipt is clamped by formatAge.
  return now - epoch;
}

/** Parses an ISO timestamp to epoch milliseconds, or null when invalid. */
export function parseEpochMs(iso: string | null | undefined): number | null {
  if (iso === null || iso === undefined) return null;
  const epoch = Date.parse(iso);
  return Number.isFinite(epoch) ? epoch : null;
}

// ---------------------------------------------------------------------------
// Per-field presentation (values, stale retention, unavailability)
// ---------------------------------------------------------------------------

export type ScalarFieldView =
  | { out: "value"; value: number }
  | { out: "stale"; value: number }
  | { out: "unavailable"; reason: string }
  | { out: "absent" };

export type MemoryFieldView =
  | { out: "value"; usedBytes: number; totalBytes: number }
  | { out: "stale"; usedBytes: number; totalBytes: number }
  | { out: "unavailable"; reason: string }
  | { out: "absent" };

/**
 * Presents one metrics field. When the machine's freshness is "stale", an
 * available value is returned as `stale` (retain and label the last-known
 * value); an unavailable field keeps its reason at any freshness.
 */
export function cpuFieldView(metric: CpuMetric | undefined, fresh: Freshness): ScalarFieldView {
  if (!metric) return { out: "absent" };
  if (metric.status === "unavailable") return { out: "unavailable", reason: metric.reason };
  return fresh === "live" ? { out: "value", value: metric.usagePercent } : { out: "stale", value: metric.usagePercent };
}

/** Same retention rules for memory, keeping bytes rather than a percent. */
export function memoryFieldView(
  metric: BytesUsageMetric | undefined,
  fresh: Freshness,
): MemoryFieldView {
  if (!metric) return { out: "absent" };
  if (metric.status === "unavailable") return { out: "unavailable", reason: metric.reason };
  return fresh === "live"
    ? { out: "value", usedBytes: metric.usedBytes, totalBytes: metric.totalBytes }
    : { out: "stale", usedBytes: metric.usedBytes, totalBytes: metric.totalBytes };
}

/**
 * True when either live field reports unavailable in the machine's most
 * recent metrics record; `metrics === undefined` (no sample yet) is not
 * "unavailable" — it is simply not seen.
 */
export function hasUnavailableField(metrics: (MetricsRecord & { receivedAt: string }) | undefined): boolean {
  if (!metrics) return false;
  return (
    metrics.payload.cpu.status === "unavailable" || metrics.payload.memory.status === "unavailable"
  );
}

// ---------------------------------------------------------------------------
// Attention (per-machine highlights for the fleet list)
// ---------------------------------------------------------------------------

/**
 * One glanceable assessment per machine. State order encodes precedence:
 * never-seen > stale > degraded > disconnected > ok. Staleness uses the
 * machine's cadence-derived threshold unless an explicit threshold is given.
 */
export type AttentionLevel =
  | "ok"
  | "degraded"
  | "stale"
  | "disconnected"
  | "unseen";

export function machineAttention(
  state: MachineState,
  now: number,
  thresholdMs: number = freshnessThresholdFor(state.expectedIntervalMs),
): AttentionLevel {
  const fresh = stateFreshness(state, now, thresholdMs);
  if (!state.metrics || fresh === "unknown") return "unseen";
  if (fresh === "stale") return "stale";
  if (hasUnavailableField(state.metrics)) return "degraded";
  if (!state.connected) return "disconnected";
  return "ok";
}

/** Human label for an attention level. */
export function attentionLabel(level: AttentionLevel): string {
  switch (level) {
    case "ok":
      return "Live";
    case "degraded":
      return "Degraded";
    case "stale":
      return "Stale";
    case "disconnected":
      return "Disconnected";
    case "unseen":
      return "No sample yet";
  }
}

/**
 * Collector connection as a separate axis from freshness: a collector can
 * be off while fresh data is still displayed, and on while fields fail.
 */
export type ConnectionStatus = "connected" | "disconnected";
export function collectorConnection(state: MachineState, _now: number): ConnectionStatus {
  void _now;
  return state.connected ? "connected" : "disconnected";
}

// ---------------------------------------------------------------------------
// Capture clock skew (flagged, never used for liveness)
// ---------------------------------------------------------------------------

export type SkewReport =
  | { kind: "none" }
  | { kind: "ahead"; skewMs: number }
  | { kind: "behind"; skewMs: number }
  | { kind: "unknown" };

/**
 * Compares the sample's capture timestamp against hub receipt time. "ahead"
 * means capturedBy-collector-clock > receivedBy-hub-clock — a collector
 * clock running fast (or off). "behind" means the hub received long after
 * the claimed capture. Plain delay beyond the tolerance also lands under
 * "behind"; the values are shown with their own explanation instead of
 * trusting either clock.
 */
export function captureClockSkew(
  state: MachineState,
  toleranceMs: number = SKEW_TOLERANCE_MS,
): SkewReport {
  const metrics = state.metrics;
  if (!metrics) return { kind: "unknown" };
  const captured = Date.parse(metrics.capturedAt);
  const received = Date.parse(metrics.receivedAt);
  if (!Number.isFinite(captured) || !Number.isFinite(received)) return { kind: "unknown" };
  const delta = captured - received;
  if (delta > toleranceMs) return { kind: "ahead", skewMs: delta };
  if (delta < -toleranceMs) return { kind: "behind", skewMs: -delta };
  return { kind: "none" };
}

// ---------------------------------------------------------------------------
// Combined per-machine presentation (fleet rows)
// ---------------------------------------------------------------------------

export type MachinePresentation = {
  freshness: Freshness;
  attention: AttentionLevel;
  connection: ConnectionStatus;
  skew: SkewReport;
  /** Resolved staleness threshold actually applied to this machine (ms). */
  thresholdMs: number;
};

export function presentMachine(
  state: MachineState,
  now: number,
  thresholdMs: number = freshnessThresholdFor(state.expectedIntervalMs),
): MachinePresentation {
  return {
    freshness: stateFreshness(state, now, thresholdMs),
    attention: machineAttention(state, now, thresholdMs),
    connection: collectorConnection(state, now),
    skew: captureClockSkew(state),
    thresholdMs,
  };
}

// ---------------------------------------------------------------------------
// Presentation constants re-exported for UI disclosure copy
// ---------------------------------------------------------------------------

export { FRESHNESS_THRESHOLD_MS, SKEW_TOLERANCE_MS };
