/**
 * Bounded history queries over raw samples and rollups.
 *
 * A query first picks the finest storage tier whose retention window covers the
 * requested start and whose estimated scan (series × range / tier step) fits the
 * row budget. It then downsamples to approximately `maxPoints` buckets per
 * series, with at most one extra bucket where the range cuts the fixed grid.
 *
 * Aggregation is done in SQL with the same min/max/sum/count semantics as the
 * rollups, and weighted by `count` so a one-minute bucket with 60 samples weighs
 * more than one with 2. Empty buckets are omitted (never zero-filled); a bucket
 * that only saw explicit unavailable readings yields null min/max/avg.
 */

import { MAX_CORES } from "../../contracts";
import type { HubDatabase } from "./db";
import { resolveRetentionPolicy, type RetentionPolicy } from "./retention";
import { METRICS, type MetricName } from "./series";

const DAY_MS = 86_400_000;
const MAX_RANGE_MS = 400 * DAY_MS;
const MAX_METRICS = Object.keys(METRICS).length;
const MAX_RESOURCE_IDS = 64;
const DEFAULT_MAX_POINTS = 600;
const MAX_POINTS = 2000;
const MAX_GAPS = 500;

/**
 * Finest raw resolution we will emit. Collectors may sample as fast as 100ms;
 * the row budget, not this value, decides whether raw is affordable. For larger
 * ranges `maxPoints` raises the bucket size well above this.
 */
const TIER_RESOLUTION_MS = { raw: 100, "1m": 60_000, "1h": 3_600_000 } as const;

export type HistoryQuery = {
  machineId: string;
  metrics: string[];
  resourceIds?: string[];
  /** Restrict only cpu.core; other metric families retain their resources. */
  coreIds?: string[];
  fromMs: number;
  toMs: number;
  maxPoints?: number;
};

export type HistorySeries = {
  metric: string;
  resourceId: string;
  unit: string;
  points: Array<{ t: number; min: number | null; max: number | null; avg: number | null }>;
};

export type HistoryGap = {
  fromMs: number;
  toMs: number;
  kind: "missing" | "dropped";
  reason?: string;
  droppedRecords?: number;
};

export type HistoryResult = {
  machineId: string;
  fromMs: number;
  toMs: number;
  stepMs: number;
  tier: "raw" | "1m" | "1h";
  series: HistorySeries[];
  gaps: HistoryGap[];
  truncated: boolean;
  elapsedMs: number;
};

export class HistoryQueryError extends Error {
  readonly code = "invalid-query" as const;
  constructor(message: string) {
    super(message);
    this.name = "HistoryQueryError";
  }
}

type Tier = HistoryResult["tier"];

type SeriesRow = { series_id: number | bigint; metric: string; resource_id: string };

type AggregateRow = {
  bucket: number | bigint;
  min_value: number | null;
  max_value: number | null;
  sum_value: number | null;
  count_value: number | bigint | null;
  row_count: number | bigint;
};

export function queryHistory(
  db: HubDatabase,
  q: HistoryQuery,
  budget: {
    maxSeries?: number;
    maxRowsScanned?: number;
    maxElapsedMs?: number;
    /** Pass the hub's retention policy if it differs from the defaults. */
    retention?: Partial<RetentionPolicy>;
  } = {},
): HistoryResult {
  const started = performance.now();
  const maxPoints = validatedMaxPoints(q.maxPoints);
  const maxSeries = validatedBudget(budget.maxSeries, 64, "maxSeries");
  const maxRowsScanned = validatedBudget(budget.maxRowsScanned, 250_000, "maxRowsScanned");
  const maxElapsedMs = validatedBudget(budget.maxElapsedMs, 5000, "maxElapsedMs");
  const retention = resolveRetentionPolicy(budget.retention);
  validatedQuery(q);

  const rangeMs = q.toMs - q.fromMs;
  const nowMs = Date.now();

  let allSeries = loadSeries(db, q);
  let truncated = false;
  if (allSeries.length > maxSeries) {
    allSeries = allSeries.slice(0, maxSeries);
    truncated = true;
  }

  // Pick the finest tier whose *actual* indexed row count for this range fits
  // the scan budget. Counting is bounded by maxRowsScanned + 1, so a dense raw
  // range (for example 100ms sampling) cannot make tier selection itself scan
  // an unbounded amount of data.
  const chosen = chooseTier(
    db,
    allSeries.map((row) => Number(row.series_id)),
    q.fromMs,
    q.toMs,
    nowMs,
    retention,
    maxRowsScanned,
    maxElapsedMs,
    started,
  );
  if (chosen.timeExceeded || chosen.overBudget) truncated = true;
  let seriesRows = allSeries;
  if (chosen.cutSeries < seriesRows.length) {
    seriesRows = seriesRows.slice(0, Math.max(0, chosen.cutSeries));
    truncated = true;
  }
  // Keep the grid fixed as a rolling window advances. Whole storage-tier
  // intervals ensure query buckets also respect the minute/hour rollup grid.
  const stepMs = Math.max(1, Math.ceil(rangeMs / maxPoints / chosen.resolutionMs)) * chosen.resolutionMs;
  const originMs = Math.floor(q.fromMs / stepMs) * stepMs;

  const series: HistorySeries[] = [];
  for (const row of seriesRows) {
    if (performance.now() - started > maxElapsedMs) {
      truncated = true;
      break;
    }
    const metric = row.metric as MetricName;
    const definition = METRICS[metric];
    if (!definition) continue; // A future metric this build does not know.
    const points = readSeries(
      db,
      chosen.tier,
      Number(row.series_id),
      fromBucketMs(chosen.tier, q.fromMs),
      q.toMs,
      originMs,
      stepMs,
      // An additional per-series guard. Tier selection already keeps the total
      // under budget; this makes a single query bounded even if a custom
      // retention policy forces the coarse fallback.
      maxRowsScanned,
    );
    if (points.length === 0) continue;
    series.push({ metric: row.metric, resourceId: row.resource_id, unit: definition.unit, points });
  }

  const gaps = readGaps(db, q.machineId, q.fromMs, q.toMs);

  return {
    machineId: q.machineId,
    fromMs: q.fromMs,
    toMs: q.toMs,
    stepMs,
    tier: chosen.tier,
    series,
    gaps,
    truncated,
    elapsedMs: Math.max(0, Math.round(performance.now() - started)),
  };
}

function loadSeries(db: HubDatabase, q: HistoryQuery): SeriesRow[] {
  const placeholders = q.metrics.map(() => "?").join(", ");
  const params: Array<string | number> = [q.machineId, ...q.metrics];
  let sql =
    "SELECT series_id, metric, resource_id FROM series " +
    `WHERE machine_id = ? AND metric IN (${placeholders})`;
  if (q.resourceIds && q.resourceIds.length > 0) {
    sql += ` AND resource_id IN (${q.resourceIds.map(() => "?").join(", ")})`;
    params.push(...q.resourceIds);
  }
  if (q.coreIds !== undefined) {
    if (q.coreIds.length === 0) sql += " AND metric <> 'cpu.core'";
    else {
      sql += ` AND (metric <> 'cpu.core' OR resource_id IN (${q.coreIds.map(() => "?").join(", ")}))`;
      params.push(...q.coreIds);
    }
  }
  sql += " ORDER BY metric, resource_id";
  return db.db.prepare(sql).all(...params) as SeriesRow[];
}

type TierCandidate = {
  tier: Tier;
  resolutionMs: number;
  retentionMs: number;
};

const TIER_CANDIDATES: readonly TierCandidate[] = [
  { tier: "raw", resolutionMs: TIER_RESOLUTION_MS.raw, retentionMs: 0 },
  { tier: "1m", resolutionMs: TIER_RESOLUTION_MS["1m"], retentionMs: 0 },
  { tier: "1h", resolutionMs: TIER_RESOLUTION_MS["1h"], retentionMs: 0 },
];

function chooseTier(
  db: HubDatabase,
  seriesIds: number[],
  fromMs: number,
  toMs: number,
  nowMs: number,
  retention: RetentionPolicy,
  maxRowsScanned: number,
  maxElapsedMs: number,
  started: number,
): {
  tier: Tier;
  resolutionMs: number;
  cutSeries: number;
  timeExceeded: boolean;
  overBudget: boolean;
} {
  const candidates: readonly TierCandidate[] = TIER_CANDIDATES.map((candidate) => ({
    ...candidate,
    retentionMs:
      candidate.tier === "raw"
        ? retention.rawMs
        : candidate.tier === "1m"
          ? retention.oneMinuteMs
          : retention.oneHourMs,
  }));
  const available = candidates.filter((candidate) => fromMs >= nowMs - candidate.retentionMs);
  // The oldest tier is always a usable fallback even if a custom retention
  // policy is shorter than the requested range.
  const pool = available.length > 0 ? available : [candidates[candidates.length - 1]!];

  let timeExceeded = false;
  for (const candidate of pool) {
    if (performance.now() - started > maxElapsedMs) {
      timeExceeded = true;
      break;
    }
    const rows = countRowsBounded(
      db,
      candidate.tier,
      seriesIds,
      fromBucketMs(candidate.tier, fromMs),
      toMs,
      maxRowsScanned,
    );
    if (rows <= maxRowsScanned) {
      return {
        tier: candidate.tier,
        resolutionMs: candidate.resolutionMs,
        cutSeries: seriesIds.length,
        timeExceeded,
        overBudget: false,
      };
    }
    if (performance.now() - started > maxElapsedMs) {
      timeExceeded = true;
      break;
    }
  }

  const fallback = pool[pool.length - 1]!;
  // No tier fits (or we ran out of time): keep the coarsest tier and cut the
  // series count until its actual indexed row count fits the budget.
  let cut = seriesIds.length;
  let fits = seriesIds.length === 0;
  if (timeExceeded) cut = Math.max(1, Math.floor(cut / 2));
  while (!fits && cut >= 1) {
    if (performance.now() - started > maxElapsedMs) {
      timeExceeded = true;
      break;
    }
    const rows = countRowsBounded(
      db,
      fallback.tier,
      seriesIds.slice(0, cut),
      fromBucketMs(fallback.tier, fromMs),
      toMs,
      maxRowsScanned,
    );
    if (rows <= maxRowsScanned) {
      fits = true;
      break;
    }
    if (cut === 1) break;
    cut = Math.max(1, Math.floor(cut / 2));
  }
  // Even one series can exceed the budget for a custom retention policy. The
  // read itself is capped by `rowLimit` in readSeries, and `truncated` tells
  // the caller that the result is a prefix of that series.
  return {
    tier: fallback.tier,
    resolutionMs: fallback.resolutionMs,
    cutSeries: Math.max(1, Math.min(cut, seriesIds.length)),
    timeExceeded,
    overBudget: !fits,
  };
}

/**
 * Counts rows for the requested series, stopping once `budget + 1` rows have
 * been seen. Each per-series query uses the clustered primary key and its own
 * LIMIT, so counting dense raw data is bounded by the budget rather than by
 * the range length.
 */
function countRowsBounded(
  db: HubDatabase,
  tier: Tier,
  seriesIds: number[],
  fromMs: number,
  toMs: number,
  budget: number,
): number {
  if (seriesIds.length === 0) return 0;
  const column = tier === "raw" ? "ts_ms" : "bucket_ms";
  const table = tier === "raw" ? "points_raw" : `points_${tier}`;
  const statement = db.db.prepare(
    `SELECT COUNT(*) AS n FROM (` +
      `SELECT 1 FROM ${table} WHERE series_id = ? AND ${column} >= ? AND ${column} <= ? LIMIT ?` +
      `)`,
  );
  let total = 0;
  for (const seriesId of seriesIds) {
    const remaining = budget + 1 - total;
    if (remaining <= 0) return total;
    const row = statement.get(seriesId, fromMs, toMs, remaining) as { n: number | bigint } | undefined;
    total += Number(row?.n ?? 0);
    if (total > budget) return total;
  }
  return total;
}

function readSeries(
  db: HubDatabase,
  tier: Tier,
  seriesId: number,
  fromBucketMs: number,
  toMs: number,
  originMs: number,
  stepMs: number,
  rowLimit: number,
): HistorySeries["points"] {
  const statement =
    tier === "raw"
      ? db.db.prepare(
          "SELECT ? + CAST((ts_ms - ?) / ? AS INTEGER) * ? AS bucket, " +
            "MIN(value) AS min_value, MAX(value) AS max_value, " +
            "SUM(value) AS sum_value, COUNT(value) AS count_value, COUNT(*) AS row_count " +
            "FROM (SELECT ts_ms, value FROM points_raw " +
            "WHERE series_id = ? AND ts_ms >= ? AND ts_ms <= ? ORDER BY ts_ms LIMIT ?) " +
            "GROUP BY bucket ORDER BY bucket",
        )
      : db.db.prepare(
          `SELECT ? + CAST((bucket_ms - ?) / ? AS INTEGER) * ? AS bucket, ` +
            "MIN(min) AS min_value, MAX(max) AS max_value, " +
            "SUM(sum) AS sum_value, SUM(count) AS count_value, COUNT(*) AS row_count " +
            `FROM (SELECT bucket_ms, min, max, sum, count FROM points_${tier} ` +
            "WHERE series_id = ? AND bucket_ms >= ? AND bucket_ms <= ? ORDER BY bucket_ms LIMIT ?) " +
            "GROUP BY bucket ORDER BY bucket",
        );
  const rows = statement.all(
    originMs,
    originMs,
    stepMs,
    stepMs,
    seriesId,
    fromBucketMs,
    toMs,
    rowLimit,
  ) as AggregateRow[];
  return rows.map((row) => {
    const count = Number(row.count_value ?? 0);
    return {
      t: Number(row.bucket),
      min: row.min_value === null ? null : Number(row.min_value),
      max: row.max_value === null ? null : Number(row.max_value),
      avg: count > 0 && row.sum_value !== null ? Number(row.sum_value) / count : null,
    };
  });
}

/**
 * Rollup tiers are bucketed on absolute minute/hour boundaries. Include the
 * bucket that contains `fromMs`; otherwise a short query inside one bucket
 * would return no rows at all. The output keeps its absolute bucket timestamp,
 * which can precede the requested range for the first, partial bucket.
 */
function fromBucketMs(tier: Tier, fromMs: number): number {
  if (tier === "raw") return fromMs;
  const span = tier === "1m" ? 60_000 : 3_600_000;
  return Math.floor(fromMs / span) * span;
}

function readGaps(db: HubDatabase, machineId: string, fromMs: number, toMs: number): HistoryGap[] {
  const dropped = db.db
    .prepare(
      "SELECT from_ms, to_ms, dropped, reason FROM gaps " +
        "WHERE machine_id = ? AND to_ms >= ? AND from_ms <= ? ORDER BY from_ms LIMIT ?",
    )
    .all(machineId, fromMs, toMs, MAX_GAPS) as Array<{
    from_ms: number | bigint;
    to_ms: number | bigint;
    dropped: number | bigint;
    reason: string;
  }>;

  // LAG is evaluated only over receipts inside the requested range: the first
  // in-range receipt has no predecessor, so no leading/trailing interval is
  // invented. The threshold uses the row's own advertised cadence.
  const missing = db.db
    .prepare(
      "WITH ordered AS ( " +
        "SELECT captured_at_ms, " +
        "LAG(captured_at_ms) OVER (PARTITION BY machine_id ORDER BY captured_at_ms) AS prev_ms, " +
        "expected_interval_ms " +
        "FROM sample_receipts WHERE machine_id = ? AND captured_at_ms >= ? AND captured_at_ms <= ? " +
        ") " +
        "SELECT prev_ms AS from_ms, captured_at_ms AS to_ms FROM ordered " +
        "WHERE prev_ms IS NOT NULL " +
        "AND captured_at_ms - prev_ms > MAX(15000, 3 * COALESCE(expected_interval_ms, 5000)) " +
        "ORDER BY captured_at_ms LIMIT ?",
    )
    .all(machineId, fromMs, toMs, MAX_GAPS) as Array<{
    from_ms: number | bigint;
    to_ms: number | bigint;
  }>;

  const gaps: HistoryGap[] = [
    ...dropped.map((row) => ({
      fromMs: Number(row.from_ms),
      toMs: Number(row.to_ms),
      kind: "dropped" as const,
      reason: row.reason,
      droppedRecords: Number(row.dropped),
    })),
    ...missing.map((row) => ({
      fromMs: Number(row.from_ms),
      toMs: Number(row.to_ms),
      kind: "missing" as const,
      reason: "expected interval missed",
    })),
  ];
  gaps.sort((a, b) => a.fromMs - b.fromMs || a.toMs - b.toMs);
  return gaps.slice(0, MAX_GAPS);
}

function validatedQuery(q: HistoryQuery): void {
  if (q === null || typeof q !== "object") throw new HistoryQueryError("query must be an object");
  if (typeof q.machineId !== "string" || q.machineId.length === 0 || q.machineId.length > 256) {
    throw new HistoryQueryError("machineId must be a nonempty string");
  }
  if (!Array.isArray(q.metrics) || q.metrics.length === 0 || q.metrics.length > MAX_METRICS) {
    throw new HistoryQueryError(`metrics must contain 1..${MAX_METRICS} metric names`);
  }
  const seenMetrics = new Set<string>();
  for (const metric of q.metrics) {
    if (typeof metric !== "string" || !Object.prototype.hasOwnProperty.call(METRICS, metric)) {
      throw new HistoryQueryError(`unknown metric ${JSON.stringify(metric)}`);
    }
    if (seenMetrics.has(metric)) throw new HistoryQueryError(`duplicate metric ${JSON.stringify(metric)}`);
    seenMetrics.add(metric);
  }
  if (q.resourceIds !== undefined) {
    if (!Array.isArray(q.resourceIds) || q.resourceIds.length > MAX_RESOURCE_IDS) {
      throw new HistoryQueryError(`resourceIds must contain at most ${MAX_RESOURCE_IDS} ids`);
    }
    const seenResources = new Set<string>();
    for (const id of q.resourceIds) {
      if (typeof id !== "string" || id.length === 0 || id.length > 256) {
        throw new HistoryQueryError("resourceIds must be nonempty strings");
      }
      if (seenResources.has(id)) throw new HistoryQueryError(`duplicate resource id ${JSON.stringify(id)}`);
      seenResources.add(id);
    }
  }
  if (q.coreIds !== undefined && (!Array.isArray(q.coreIds) || q.coreIds.length > MAX_CORES ||
    q.coreIds.some((id) => typeof id !== "string" || !/^core:(?:0|[1-9]\d{0,3})$/.test(id) || Number(id.slice(5)) >= MAX_CORES) ||
    new Set(q.coreIds).size !== q.coreIds.length)) {
    throw new HistoryQueryError(`coreIds must contain at most ${MAX_CORES} unique core identifiers`);
  }
  if (!Number.isSafeInteger(q.fromMs) || !Number.isSafeInteger(q.toMs)) {
    throw new HistoryQueryError("fromMs and toMs must be safe integer epoch milliseconds");
  }
  if (q.fromMs >= q.toMs) throw new HistoryQueryError("fromMs must be before toMs");
  if (q.toMs - q.fromMs > MAX_RANGE_MS) {
    throw new HistoryQueryError("range exceeds 400 days");
  }
}

function validatedMaxPoints(value: number | undefined): number {
  if (value === undefined) return DEFAULT_MAX_POINTS;
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new HistoryQueryError("maxPoints must be a positive safe integer");
  }
  return Math.min(value, MAX_POINTS);
}

function validatedBudget(value: number | undefined, fallback: number, name: string): number {
  if (value === undefined) return fallback;
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new HistoryQueryError(`${name} must be a positive safe integer`);
  }
  return value;
}
