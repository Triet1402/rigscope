/**
 * Live connection outages for History.
 *
 * The history query can only report gaps that are already visible in stored
 * receipts. A collector that is currently offline has no receipt for the
 * outage, and a short disconnect may reset before the storage read threshold
 * is crossed. The History view therefore remembers the live disconnect and
 * reconnect boundaries and merges them into the stored gap list.
 *
 * Browser-safe: no DOM and no Node APIs.
 */

import type { HistoryGap } from "../hub/storage/history-query";

export type ConnectionPoint = {
  connected: boolean;
  lastReceivedAt: string | null;
};

export type ObservedOutage = {
  /** Receipt time of the last sample before the collector went offline. */
  fromMs: number;
  /** Reconnect edge (or first post-reconnect receipt), or null while offline. */
  toMs: number | null;
};

/** Keep only recent transitions; older outages can no longer be in range. */
const MAX_OBSERVED_OUTAGES = 64;

export function parseReceiptTime(value: string | null | undefined): number | null {
  if (typeof value !== "string" || value.length === 0) return null;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : null;
}

/**
 * Advance the observed-outage list with one live connection snapshot.
 *
 * - Going offline opens an outage at the last receipt (or the observation time
 *   when the page first sees an already-offline machine).
 * - Staying offline keeps the same open interval; it is extended for display
 *   to the current query edge rather than being written into state each tick.
 * - Reconnecting closes the open interval immediately. The end is the first
 *   post-reconnect receipt when one is already present, otherwise the observed
 *   reconnect time; waiting for a later receipt would keep the old line open
 *   and delay the highlight for short outages.
 *
 * A transition that does not change the open/closed state returns the same
 * array reference so callers can use it directly in render memoization.
 */
export function updateObservedOutages(
  outages: readonly ObservedOutage[],
  previous: ConnectionPoint | null,
  current: ConnectionPoint,
  nowMs: number,
): ObservedOutage[] {
  const openIndex = outages.findIndex((outage) => outage.toMs === null);

  if (!current.connected) {
    if (openIndex >= 0) return outages as ObservedOutage[];
    const fromMs =
      parseReceiptTime(current.lastReceivedAt) ??
      parseReceiptTime(previous?.lastReceivedAt ?? null) ??
      nowMs;
    const next = [...outages, { fromMs, toMs: null }];
    return next.length > MAX_OBSERVED_OUTAGES ? next.slice(next.length - MAX_OBSERVED_OUTAGES) : next;
  }

  // Without a genuine offline -> online edge this snapshot cannot close an
  // outage. In particular, an already-connected first observation must not
  // invent a leading gap.
  if (openIndex < 0 || previous === null || previous.connected) return outages as ObservedOutage[];

  const open = outages[openIndex]!;
  const receiptMs = parseReceiptTime(current.lastReceivedAt);
  const endMs =
    receiptMs !== null && receiptMs > open.fromMs
      ? receiptMs
      : Math.max(open.fromMs, nowMs);
  return outages.map((outage, index) =>
    index === openIndex ? { fromMs: outage.fromMs, toMs: endMs } : outage,
  );
}

/**
 * Convert observed outages to query-range gap records. Open outages extend to
 * the query edge; every interval is clipped so a gap remembered from an older
 * window never leaks into the current one.
 */
export function observedOutageGaps(
  outages: readonly ObservedOutage[],
  fromMs: number,
  toMs: number,
): HistoryGap[] {
  if (!Number.isFinite(fromMs) || !Number.isFinite(toMs) || toMs <= fromMs) return [];
  const gaps: HistoryGap[] = [];
  for (const outage of outages) {
    const start = Math.max(fromMs, outage.fromMs);
    const end = Math.min(toMs, outage.toMs ?? toMs);
    if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) continue;
    gaps.push({
      fromMs: start,
      toMs: end,
      kind: "missing",
      reason: outage.toMs === null ? "collector disconnected" : "collector outage",
    });
  }
  return mergeHistoryGaps(gaps);
}

/**
 * Merge overlapping or touching intervals so a stored gap and the live outage
 * that describes the same interval are not counted or shaded twice. The first
 * reason is kept (the storage explanation is usually more specific) while a
 * dropped interval always dominates a plain missing interval.
 */
export function mergeHistoryGaps(gaps: readonly HistoryGap[]): HistoryGap[] {
  const sorted = gaps
    .filter(
      (gap) =>
        Number.isFinite(gap.fromMs) &&
        Number.isFinite(gap.toMs) &&
        gap.toMs > gap.fromMs,
    )
    .map((gap) => ({ ...gap }))
    .sort((a, b) => a.fromMs - b.fromMs || a.toMs - b.toMs);

  const merged: HistoryGap[] = [];
  for (const gap of sorted) {
    const previous = merged.at(-1);
    if (previous !== undefined && gap.fromMs <= previous.toMs) {
      previous.toMs = Math.max(previous.toMs, gap.toMs);
      if (gap.kind === "dropped") {
        previous.kind = "dropped";
      }
      if (previous.reason === undefined && gap.reason !== undefined) {
        previous.reason = gap.reason;
      }
      if (previous.droppedRecords === undefined && gap.droppedRecords !== undefined) {
        previous.droppedRecords = gap.droppedRecords;
      }
      continue;
    }
    merged.push(gap);
  }
  return merged;
}
