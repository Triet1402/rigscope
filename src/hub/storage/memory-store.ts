/**
 * RAM-first telemetry history.
 *
 * The default hub mode keeps no persistent telemetry history. Latest live
 * state already lives in `HubState`; this module owns the bounded historical
 * summaries used by charts and recordings.
 *
 * Design choices:
 * - Every series has a small number of fixed-size typed-array ring buffers,
 *   one per resolution tier. There is no object per reading; a tier costs its
 *   capacity once and then overwrites the oldest slot.
 * - Tiers are updated directly in RAM. That costs a handful of array writes
 *   per sample and avoids the cascade bookkeeping that would otherwise be
 *   needed to include the current partial window in a coarser tier.
 * - Buckets retain min, max, sum, count and an explicit unavailable count, so
 *   spikes and gaps survive downsampling and averages combine correctly.
 * - A global byte budget bounds all machines. When a new series would exceed
 *   it, the least-recently-seen series lose their finest tiers first; if that
 *   is not enough, whole series are evicted. New series beyond the sustainable
 *   budget are rejected rather than growing without bound.
 * - Deduplication uses a compact per-run interval set, not one object per
 *   sample. It lasts for the process lifetime, independent of chart retention;
 *   after a memory-mode restart both history and the receipts are intentionally
 *   gone together.
 */

import { performance } from "node:perf_hooks";
import type { StatementSync } from "node:sqlite";
import type {
  Inventory,
  MetricsPayload,
  MetricsRecord,
  RecordRef,
  TelemetryRecord,
} from "../../contracts";
import type { HubDatabase } from "./db";
import {
  validatedBudget,
  validatedMaxPoints,
  validatedQuery,
  type HistoryGap,
  type HistoryQuery,
  type HistoryQueryBudget,
  type HistoryResult,
  type HistorySeries,
  type HistoryTier,
} from "./history-query";
import type {
  HistorySource,
  HistoryStatus,
  MemoryHistoryStatus,
  HistoryTierStatus,
} from "./history";
import {
  flattenMetrics,
  METRICS,
  type KnownSeries,
  type MetricName,
} from "./series";
import type { IngestBatch, IngestResult } from "./telemetry-store";

const DAY_MS = 86_400_000;
const HOUR_MS = 3_600_000;
const MINUTE_MS = 60_000;
const SECOND_MS = 1_000;
const FUTURE_TOLERANCE_MS = DAY_MS;
const DEFAULT_BUDGET_BYTES = 128 * 1024 * 1024;
const MAX_GAPS = 2_000;
const MAX_RUNS = 512;
const MAX_RUN_INTERVALS = 1_024;

type TierConfig = {
  tier: HistoryTier;
  stepMs: number;
  retentionMs: number;
};

const BASE_TIERS: readonly TierConfig[] = [
  { tier: "1s", stepMs: SECOND_MS, retentionMs: 2 * MINUTE_MS },
  { tier: "10s", stepMs: 10 * SECOND_MS, retentionMs: HOUR_MS },
  { tier: "1m", stepMs: MINUTE_MS, retentionMs: DAY_MS },
  { tier: "1h", stepMs: HOUR_MS, retentionMs: 7 * DAY_MS },
];

/** Per-core detail is less valuable long-term; keep the coarser tiers only. */
const CORE_TIERS: readonly TierConfig[] = [
  { tier: "10s", stepMs: 10 * SECOND_MS, retentionMs: 10 * MINUTE_MS },
  { tier: "1m", stepMs: MINUTE_MS, retentionMs: 6 * HOUR_MS },
  { tier: "1h", stepMs: HOUR_MS, retentionMs: 2 * DAY_MS },
];

export type MemoryTelemetryStoreOptions = {
  database: HubDatabase;
  now?: () => number;
  /** Global RAM budget for every historical summary and metadata index. */
  budgetBytes?: number;
  /** Override any tier retention in milliseconds; zero disables a tier. */
  tierRetentionMs?: Partial<Record<HistoryTier, number>>;
  maxSeries?: number;
};

export type MemoryTelemetryStoreStats = {
  mode: "memory";
  queuedRecords: 0;
  queuedBatches: 0;
  committedRecords: number;
  duplicateRecords: number;
  rejectedRecords: number;
  rejectedSeries: number;
  memory: MemoryHistoryStatus;
  gaps: number;
};

type Bucket = {
  t: number;
  min: number | null;
  max: number | null;
  sum: number;
  count: number;
  nullCount: number;
};

type CurrentBucket = Bucket;

/**
 * One resolution ring for one series. The absolute bucket number maps to a
 * slot; the timestamp stored in that slot proves whether the bucket is still
 * retained. This makes late updates to recently completed buckets cheap while
 * naturally dropping data older than the ring capacity.
 */
class TierRing {
  readonly stepMs: number;
  readonly capacity: number;
  readonly bytes: number;
  private readonly stamps: Float64Array;
  private readonly mins: Float64Array;
  private readonly maxs: Float64Array;
  private readonly sums: Float64Array;
  private readonly counts: Uint32Array;
  private readonly nulls: Uint32Array;
  private current: CurrentBucket | null = null;

  constructor(stepMs: number, retentionMs: number) {
    this.stepMs = stepMs;
    this.capacity = Math.max(1, Math.ceil(retentionMs / stepMs));
    this.stamps = new Float64Array(this.capacity);
    this.mins = new Float64Array(this.capacity);
    this.maxs = new Float64Array(this.capacity);
    this.sums = new Float64Array(this.capacity);
    this.counts = new Uint32Array(this.capacity);
    this.nulls = new Uint32Array(this.capacity);
    // 4 * 8-byte numbers + 2 * 4-byte counters + a small object header.
    this.bytes = this.capacity * 40 + 96;
  }

  add(tsMs: number, value: number | null): void {
    const bucketMs = Math.floor(tsMs / this.stepMs) * this.stepMs;
    const current = this.current;
    if (current === null) {
      this.current = newBucket(bucketMs, value);
      return;
    }
    if (bucketMs === current.t) {
      addValue(current, value);
      return;
    }
    if (bucketMs > current.t) {
      this.push(current);
      this.current = newBucket(bucketMs, value);
      return;
    }
    this.addRing(bucketMs, value);
  }

  read(fromMs: number, toMs: number): Bucket[] {
    const out: Bucket[] = [];
    for (let index = 0; index < this.capacity; index += 1) {
      const t = this.stamps[index]!;
      if (t < fromMs || t > toMs) continue;
      const count = this.counts[index]!;
      const nullCount = this.nulls[index]!;
      if (count === 0 && nullCount === 0) continue;
      out.push({
        t,
        min: count === 0 ? null : this.mins[index]!,
        max: count === 0 ? null : this.maxs[index]!,
        sum: this.sums[index]!,
        count,
        nullCount,
      });
    }
    const current = this.current;
    if (current !== null && current.t >= fromMs && current.t <= toMs &&
      (current.count > 0 || current.nullCount > 0)) {
      out.push({ ...current });
    }
    out.sort((a, b) => a.t - b.t);
    return out;
  }

  hasData(): boolean {
    const current = this.current;
    if (current !== null && (current.count > 0 || current.nullCount > 0)) return true;
    for (let index = 0; index < this.capacity; index += 1) {
      if (this.counts[index]! > 0 || this.nulls[index]! > 0) return true;
    }
    return false;
  }

  private addRing(bucketMs: number, value: number | null): void {
    const slot = mod(Math.floor(bucketMs / this.stepMs), this.capacity);
    const stamp = this.stamps[slot]!;
    if (stamp > bucketMs) {
      // The slot now holds a newer bucket, so this timestamp is older than the
      // retained window. Drop it rather than corrupting a newer summary.
      return;
    }
    if (stamp !== bucketMs) {
      this.stamps[slot] = bucketMs;
      this.mins[slot] = 0;
      this.maxs[slot] = 0;
      this.sums[slot] = 0;
      this.counts[slot] = 0;
      this.nulls[slot] = 0;
    }
    if (value === null) {
      this.nulls[slot] = this.nulls[slot]! + 1;
      return;
    }
    const count = this.counts[slot]!;
    this.counts[slot] = count + 1;
    this.sums[slot] = this.sums[slot]! + value;
    if (count === 0) {
      this.mins[slot] = value;
      this.maxs[slot] = value;
    } else {
      if (value < this.mins[slot]!) this.mins[slot] = value;
      if (value > this.maxs[slot]!) this.maxs[slot] = value;
    }
  }

  private push(bucket: Bucket): void {
    if (bucket.count === 0 && bucket.nullCount === 0) return;
    const slot = mod(Math.floor(bucket.t / this.stepMs), this.capacity);
    this.stamps[slot] = bucket.t;
    this.mins[slot] = bucket.min ?? 0;
    this.maxs[slot] = bucket.max ?? 0;
    this.sums[slot] = bucket.sum;
    this.counts[slot] = bucket.count;
    this.nulls[slot] = bucket.nullCount;
  }
}

type SeriesBuffer = {
  key: string;
  machineId: string;
  metric: string;
  resourceId: string;
  tiers: Map<HistoryTier, TierRing>;
  bytes: number;
  lastSeenMs: number;
};

type RunReceipts = {
  /** Sorted, inclusive [first,last] accepted sequence intervals. */
  intervals: Array<[number, number]>;
  touchedAtMs: number;
};

type ReceiptCursor = {
  capturedAtMs: number;
  expectedIntervalMs: number | null;
};

export class MemoryTelemetryStore implements HistorySource {
  readonly historyMode = "memory" as const;
  private readonly database: HubDatabase;
  private readonly now: () => number;
  private readonly budgetBytes: number;
  private readonly tierRetentionMs: Partial<Record<HistoryTier, number>>;
  private readonly maxSeries: number;
  private readonly series = new Map<string, SeriesBuffer>();
  private readonly seriesByMachine = new Map<string, Map<string, SeriesBuffer>>();
  private readonly runs = new Map<string, RunReceipts>();
  private readonly receiptCursors = new Map<string, ReceiptCursor>();
  private readonly gaps: Array<HistoryGap & { machineId: string }> = [];

  private usedBytes = 0;
  private evictedSeries = 0;
  private rejectedSeries = 0;
  private committedRecords = 0;
  private duplicateRecords = 0;
  private rejectedRecords = 0;
  private oldestMs: number | null = null;
  private newestMs: number | null = null;
  private closed = false;

  private readonly inventoryUpsert: StatementSync;
  private readonly inventorySelect: StatementSync;
  private readonly metaUpsertBoth: StatementSync;
  private readonly metaUpsertDisplay: StatementSync;
  private readonly metaUpsertTags: StatementSync;
  private readonly metaTouch: StatementSync;
  private readonly metaSelect: StatementSync;
  private readonly metaSelectAll: StatementSync;

  constructor(options: MemoryTelemetryStoreOptions) {
    this.database = options.database;
    this.now = options.now ?? Date.now;
    this.budgetBytes = positiveInteger(
      options.budgetBytes ?? DEFAULT_BUDGET_BYTES,
      "budgetBytes",
    );
    this.tierRetentionMs = options.tierRetentionMs ?? {};
    for (const [tier, value] of Object.entries(this.tierRetentionMs)) {
      if (value !== undefined && (!Number.isSafeInteger(value) || value < 0)) {
        throw new Error(`tier retention for ${tier} must be a nonnegative safe integer`);
      }
    }
    this.maxSeries = positiveInteger(options.maxSeries ?? 20_000, "maxSeries");

    const db = options.database.db;
    this.inventoryUpsert = db.prepare(
      "INSERT INTO inventory (machine_id, captured_at_ms, received_at_ms, inventory_json) " +
        "VALUES (?, ?, ?, ?) ON CONFLICT(machine_id) DO UPDATE SET captured_at_ms = excluded.captured_at_ms, " +
        "received_at_ms = excluded.received_at_ms, inventory_json = excluded.inventory_json",
    );
    this.inventorySelect = db.prepare(
      "SELECT captured_at_ms, received_at_ms, inventory_json FROM inventory WHERE machine_id = ?",
    );
    this.metaUpsertBoth = db.prepare(
      "INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, ?, ?, ?) " +
        "ON CONFLICT(machine_id) DO UPDATE SET display_name = excluded.display_name, " +
        "tags_json = excluded.tags_json, updated_at_ms = excluded.updated_at_ms",
    );
    this.metaUpsertDisplay = db.prepare(
      "INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, ?, '[]', ?) " +
        "ON CONFLICT(machine_id) DO UPDATE SET display_name = excluded.display_name, " +
        "updated_at_ms = excluded.updated_at_ms",
    );
    this.metaUpsertTags = db.prepare(
      "INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, NULL, ?, ?) " +
        "ON CONFLICT(machine_id) DO UPDATE SET tags_json = excluded.tags_json, " +
        "updated_at_ms = excluded.updated_at_ms",
    );
    this.metaTouch = db.prepare(
      "INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, NULL, '[]', ?) " +
        "ON CONFLICT(machine_id) DO UPDATE SET updated_at_ms = excluded.updated_at_ms",
    );
    this.metaSelect = db.prepare(
      "SELECT display_name, tags_json, updated_at_ms FROM machine_meta WHERE machine_id = ?",
    );
    this.metaSelectAll = db.prepare(
      "SELECT machine_id, display_name, tags_json FROM machine_meta ORDER BY machine_id",
    );
  }

  enqueue(batch: IngestBatch): Promise<IngestResult> {
    if (this.closed) {
      return Promise.resolve({ status: "busy", retryAfterMs: 5_000, reason: "storage is closed" });
    }
    validateBatch(batch);
    const receivedAtMs = this.now();
    const accepted: RecordRef[] = [];
    const rejected: (RecordRef & { reason: string })[] = [];
    let newLive: MetricsRecord | null = null;
    let bestCaptured = 0;
    let bestSequence = -1;

    for (const record of batch.records) {
      const reason = rejectionReason(record, batch, receivedAtMs);
      if (reason !== null) {
        rejected.push({ runId: record.runId, sequence: record.sequence, reason });
        this.rejectedRecords += 1;
        continue;
      }
      const runKey = `${batch.collectorId}\u0000${record.runId}`;
      const receipts = this.receiptsFor(runKey);
      if (!acceptSequence(receipts, record.sequence)) {
        accepted.push({ runId: record.runId, sequence: record.sequence });
        this.duplicateRecords += 1;
        continue;
      }
      accepted.push({ runId: record.runId, sequence: record.sequence });
      this.committedRecords += 1;
      const capturedMs = Date.parse(record.capturedAt);

      if (record.kind === "metrics") {
        this.ingestMetrics(batch, record, capturedMs, receivedAtMs);
        if (batch.mode === "live") {
          if (
            newLive === null ||
            capturedMs > bestCaptured ||
            (capturedMs === bestCaptured && record.sequence > bestSequence)
          ) {
            newLive = record;
            bestCaptured = capturedMs;
            bestSequence = record.sequence;
          }
        }
      } else {
        this.addGap(batch.machineId, {
          fromMs: Date.parse(record.payload.fromCapturedAt),
          toMs: Date.parse(record.payload.toCapturedAt),
          kind: "dropped",
          reason: record.payload.reason,
          droppedRecords: record.payload.droppedRecords,
        });
      }
      this.noteReceipt(batch.machineId, capturedMs, batch.expectedIntervalMs, batch.mode === "live");
    }

    return Promise.resolve({
      status: "committed",
      accepted,
      rejected,
      newLive,
      receivedAtMs,
    });
  }

  /** Memory mode intentionally has no persisted latest telemetry snapshot. */
  latestLive(): [] {
    return [];
  }

  saveInventory(machineId: string, capturedAt: string, inventory: Inventory): void {
    const capturedMs = Date.parse(capturedAt);
    if (!Number.isFinite(capturedMs)) throw new Error("inventory capturedAt must be a parseable timestamp");
    this.inventoryUpsert.run(machineId, capturedMs, this.now(), JSON.stringify(inventory));
  }

  loadInventory(
    machineId: string,
  ): { capturedAt: string; receivedAt: string; inventory: Inventory } | null {
    const row = this.inventorySelect.get(machineId) as
      | { captured_at_ms: number | bigint; received_at_ms: number | bigint; inventory_json: string }
      | undefined;
    if (!row) return null;
    return {
      capturedAt: new Date(Number(row.captured_at_ms)).toISOString(),
      receivedAt: new Date(Number(row.received_at_ms)).toISOString(),
      inventory: JSON.parse(row.inventory_json) as Inventory,
    };
  }

  setMachineMeta(
    machineId: string,
    meta: { displayName?: string | null; tags?: string[] },
  ): void {
    const updatedAt = this.now();
    const hasDisplay = meta.displayName !== undefined;
    const hasTags = meta.tags !== undefined;
    const displayName = meta.displayName ?? null;
    const tags = hasTags ? validateTags(meta.tags!) : [];
    if (hasDisplay && hasTags) {
      this.metaUpsertBoth.run(machineId, displayName, JSON.stringify(tags), updatedAt);
    } else if (hasDisplay) {
      this.metaUpsertDisplay.run(machineId, displayName, updatedAt);
    } else if (hasTags) {
      this.metaUpsertTags.run(machineId, JSON.stringify(tags), updatedAt);
    } else {
      this.metaTouch.run(machineId, updatedAt);
    }
  }

  getMachineMeta(machineId: string): { displayName: string | null; tags: string[] } {
    const row = this.metaSelect.get(machineId) as
      | { display_name: string | null; tags_json: string }
      | undefined;
    if (!row) return { displayName: null, tags: [] };
    return { displayName: row.display_name, tags: parseTags(row.tags_json) };
  }

  listMachineMeta(): Map<string, { displayName: string | null; tags: string[] }> {
    const rows = this.metaSelectAll.all() as Array<{
      machine_id: string;
      display_name: string | null;
      tags_json: string;
    }>;
    const result = new Map<string, { displayName: string | null; tags: string[] }>();
    for (const row of rows) {
      result.set(row.machine_id, {
        displayName: row.display_name,
        tags: parseTags(row.tags_json),
      });
    }
    return result;
  }

  stats(): MemoryTelemetryStoreStats {
    return {
      mode: "memory",
      queuedRecords: 0,
      queuedBatches: 0,
      committedRecords: this.committedRecords,
      duplicateRecords: this.duplicateRecords,
      rejectedRecords: this.rejectedRecords,
      rejectedSeries: this.rejectedSeries,
      memory: this.memoryStatus(),
      gaps: this.gaps.length,
    };
  }

  queryHistory(query: HistoryQuery, budget: HistoryQueryBudget = {}): HistoryResult {
    const started = performance.now();
    validatedQuery(query);
    const maxPoints = validatedMaxPoints(query.maxPoints);
    const maxSeries = validatedBudget(budget.maxSeries, 64, "maxSeries");
    const maxElapsedMs = validatedBudget(budget.maxElapsedMs, 100, "maxElapsedMs");
    const now = this.now();
    const maxRetention = this.maxRetentionMs();
    let fromMs = query.fromMs;
    let toMs = query.toMs;
    let truncated = false;
    if (fromMs < now - maxRetention) {
      fromMs = now - maxRetention;
      truncated = true;
    }
    if (fromMs >= toMs) {
      return {
        machineId: query.machineId,
        fromMs,
        toMs,
        stepMs: 1,
        tier: this.coarsestTier(),
        series: [],
        gaps: [],
        truncated: true,
        elapsedMs: Math.round(performance.now() - started),
      };
    }

    let selected = this.selectSeries(query);
    if (selected.length > maxSeries) {
      selected = selected.slice(0, maxSeries);
      truncated = true;
    }
    if (selected.length === 0) {
      return {
        machineId: query.machineId,
        fromMs,
        toMs,
        stepMs: this.coarsestStepMs(),
        tier: this.coarsestTier(),
        series: [],
        gaps: this.readGaps(query.machineId, fromMs, toMs),
        truncated,
        elapsedMs: Math.round(performance.now() - started),
      };
    }

    const chosen = this.chooseTier(selected, fromMs, now);
    if (chosen === null) {
      return {
        machineId: query.machineId,
        fromMs,
        toMs,
        stepMs: this.coarsestStepMs(),
        tier: this.coarsestTier(),
        series: [],
        gaps: this.readGaps(query.machineId, fromMs, toMs),
        truncated: true,
        elapsedMs: Math.round(performance.now() - started),
      };
    }

    const rangeMs = toMs - fromMs;
    const stepMs = Math.max(
      chosen.stepMs,
      Math.ceil(rangeMs / maxPoints / chosen.stepMs) * chosen.stepMs,
    );
    const originMs = Math.floor(fromMs / stepMs) * stepMs;

    const series: HistorySeries[] = [];
    for (const buffer of selected) {
      if (performance.now() - started > maxElapsedMs) {
        truncated = true;
        break;
      }
      const ring = buffer.tiers.get(chosen.tier);
      if (ring === undefined) continue;
      const definition = METRICS[buffer.metric as MetricName];
      if (definition === undefined) continue;
      const buckets = ring.read(fromMs, toMs);
      if (buckets.length === 0) continue;
      const points = aggregateBuckets(buckets, originMs, stepMs, maxPoints);
      if (points.length === 0) continue;
      series.push({
        metric: buffer.metric,
        resourceId: buffer.resourceId,
        unit: definition.unit,
        points,
      });
    }

    return {
      machineId: query.machineId,
      fromMs,
      toMs,
      stepMs,
      tier: chosen.tier,
      series,
      gaps: this.readGaps(query.machineId, fromMs, toMs),
      truncated,
      elapsedMs: Math.round(performance.now() - started),
    };
  }

  historyStatus(): HistoryStatus {
    const now = this.now();
    const maxRetention = this.maxRetentionMs();
    const memory = this.memoryStatus();
    return {
      mode: "memory",
      persistentHistory: false,
      availableFromMs: this.series.size === 0 ? null : (this.oldestMs ?? now - maxRetention),
      availableToMs: this.newestMs,
      memory,
      persistence: null,
      gaps: this.gaps.length,
      acceptedRecords: this.committedRecords,
      generatedAt: new Date(now).toISOString(),
    };
  }

  async flushNow(): Promise<void> {
    // RAM is already current.
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.series.clear();
    this.seriesByMachine.clear();
    this.runs.clear();
    this.receiptCursors.clear();
    this.usedBytes = 0;
    try {
      this.database.close();
    } catch {
      // Closing is idempotent for tests and shutdown paths.
    }
  }

  private ingestMetrics(
    batch: IngestBatch,
    record: MetricsRecord,
    capturedMs: number,
    receivedAtMs: number,
  ): void {
    const known = needsKnownSeries(record.payload)
      ? this.knownFor(batch.machineId)
      : undefined;
    for (const point of flattenMetrics(record.payload, known)) {
      const buffer = this.seriesFor(batch.machineId, point.metric, point.resourceId, receivedAtMs);
      if (buffer === null) continue;
      buffer.lastSeenMs = receivedAtMs;
      for (const ring of buffer.tiers.values()) {
        ring.add(capturedMs, point.value);
      }
      if (this.newestMs === null || capturedMs > this.newestMs) this.newestMs = capturedMs;
      if (this.oldestMs === null || capturedMs < this.oldestMs) this.oldestMs = capturedMs;
    }
  }

  private seriesFor(
    machineId: string,
    metric: string,
    resourceId: string,
    nowMs: number,
  ): SeriesBuffer | null {
    const key = seriesKey(machineId, metric, resourceId);
    const existing = this.series.get(key);
    if (existing !== undefined) return existing;
    if (this.series.size >= this.maxSeries) {
      this.rejectedSeries += 1;
      return null;
    }
    const configs = this.tierConfigsFor(metric);
    // Try the full policy first; if the budget is tight, keep coarser tiers.
    for (let start = 0; start < configs.length; start += 1) {
      const candidate = this.buildSeries(key, machineId, metric, resourceId, configs.slice(start), nowMs);
      if (this.ensureRoom(candidate.bytes, key)) {
        this.series.set(key, candidate);
        this.usedBytes += candidate.bytes;
        let byMachine = this.seriesByMachine.get(machineId);
        if (byMachine === undefined) {
          byMachine = new Map();
          this.seriesByMachine.set(machineId, byMachine);
        }
        byMachine.set(key, candidate);
        return candidate;
      }
    }
    this.rejectedSeries += 1;
    return null;
  }

  private buildSeries(
    key: string,
    machineId: string,
    metric: string,
    resourceId: string,
    configs: readonly TierConfig[],
    nowMs: number,
  ): SeriesBuffer {
    const tiers = new Map<HistoryTier, TierRing>();
    let bytes = 160 + (machineId.length + metric.length + resourceId.length) * 2;
    for (const config of configs) {
      const retention = this.tierRetentionMs[config.tier] ?? config.retentionMs;
      if (retention <= 0) continue;
      const ring = new TierRing(config.stepMs, retention);
      tiers.set(config.tier, ring);
      bytes += ring.bytes;
    }
    return {
      key,
      machineId,
      metric,
      resourceId,
      tiers,
      bytes,
      lastSeenMs: nowMs,
    };
  }

  /**
   * Frees room by dropping the finest tiers of the least-recently-seen series,
   * then whole series. Never evicts the series that is currently being added.
   */
  private ensureRoom(requiredBytes: number, excludeKey: string): boolean {
    if (requiredBytes > this.budgetBytes) return false;
    if (this.usedBytes + requiredBytes <= this.budgetBytes) return true;
    const candidates = [...this.series.values()]
      .filter((buffer) => buffer.key !== excludeKey)
      .sort((a, b) => a.lastSeenMs - b.lastSeenMs);
    const tierOrder: HistoryTier[] = ["1s", "10s", "1m", "1h"];
    for (const buffer of candidates) {
      for (const tier of tierOrder) {
        if (!this.dropTier(buffer, tier)) continue;
        if (this.usedBytes + requiredBytes <= this.budgetBytes) return true;
      }
      this.removeSeries(buffer);
      this.evictedSeries += 1;
      if (this.usedBytes + requiredBytes <= this.budgetBytes) return true;
    }
    return this.usedBytes + requiredBytes <= this.budgetBytes;
  }

  /**
   * Remove one tier from a live series and keep both the per-series and global
   * byte counters in step with the rings that actually remain retained.
   */
  private dropTier(buffer: SeriesBuffer, tier: HistoryTier): boolean {
    const ring = buffer.tiers.get(tier);
    if (ring === undefined) return false;
    buffer.tiers.delete(tier);
    buffer.bytes -= ring.bytes;
    this.usedBytes -= ring.bytes;
    return true;
  }

  private removeSeries(buffer: SeriesBuffer): void {
    this.series.delete(buffer.key);
    // `buffer.bytes` has already been reduced as tiers were dropped, so this
    // only frees what is still allocated; never subtract a ring twice.
    this.usedBytes -= buffer.bytes;
    const byMachine = this.seriesByMachine.get(buffer.machineId);
    if (byMachine !== undefined) {
      byMachine.delete(buffer.key);
      if (byMachine.size === 0) this.seriesByMachine.delete(buffer.machineId);
    }
  }

  private tierConfigsFor(metric: string): readonly TierConfig[] {
    return metric === "cpu.core" ? CORE_TIERS : BASE_TIERS;
  }

  private selectSeries(query: HistoryQuery): SeriesBuffer[] {
    const byMachine = this.seriesByMachine.get(query.machineId);
    if (byMachine === undefined) return [];
    const metrics = new Set(query.metrics);
    const resources = query.resourceIds === undefined ? null : new Set(query.resourceIds);
    const cores = query.coreIds === undefined ? null : new Set(query.coreIds);
    const selected: SeriesBuffer[] = [];
    for (const buffer of byMachine.values()) {
      if (!metrics.has(buffer.metric)) continue;
      if (resources !== null && !resources.has(buffer.resourceId)) continue;
      if (cores !== null && buffer.metric === "cpu.core" && !cores.has(buffer.resourceId)) continue;
      selected.push(buffer);
    }
    selected.sort((a, b) =>
      a.metric < b.metric ? -1 : a.metric > b.metric ? 1 :
        a.resourceId < b.resourceId ? -1 : a.resourceId > b.resourceId ? 1 : 0,
    );
    return selected;
  }

  private chooseTier(selected: readonly SeriesBuffer[], fromMs: number, nowMs: number): TierConfig | null {
    let fallback: TierConfig | null = null;
    for (const config of BASE_TIERS) {
      const retention = this.tierRetentionMs[config.tier] ?? config.retentionMs;
      if (retention <= 0) continue;
      if (fromMs < nowMs - retention) continue;
      if (selected.every((buffer) => buffer.tiers.has(config.tier))) return config;
      if (fallback === null && selected.some((buffer) => buffer.tiers.has(config.tier))) {
        fallback = config;
      }
    }
    if (fallback !== null) return fallback;
    // Custom per-series policies may omit a global tier; use the coarsest tier
    // any selected series still has.
    for (const tier of ["1h", "1m", "10s", "1s"] as const) {
      const config = BASE_TIERS.find((item) => item.tier === tier);
      if (config !== undefined && selected.some((buffer) => buffer.tiers.has(tier))) return config;
    }
    return null;
  }

  private readGaps(machineId: string, fromMs: number, toMs: number): HistoryGap[] {
    return this.gaps
      .filter((gap) => gap.machineId === machineId && gap.toMs >= fromMs && gap.fromMs <= toMs)
      .slice(-MAX_GAPS)
      .map(({ machineId: _machineId, ...gap }) => gap);
  }

  private addGap(machineId: string, gap: HistoryGap): void {
    if (!Number.isFinite(gap.fromMs) || !Number.isFinite(gap.toMs)) return;
    this.gaps.push({ ...gap, machineId });
    if (this.gaps.length > MAX_GAPS) this.gaps.splice(0, this.gaps.length - MAX_GAPS);
  }

  private noteReceipt(
    machineId: string,
    capturedAtMs: number,
    expectedIntervalMs: number | null,
    live: boolean,
  ): void {
    if (!live) return;
    const previous = this.receiptCursors.get(machineId);
    if (previous !== undefined && capturedAtMs > previous.capturedAtMs) {
      const threshold = Math.max(
        15_000,
        3 * (expectedIntervalMs ?? previous.expectedIntervalMs ?? 5_000),
      );
      if (capturedAtMs - previous.capturedAtMs > threshold) {
        this.addGap(machineId, {
          fromMs: previous.capturedAtMs,
          toMs: capturedAtMs,
          kind: "missing",
          reason: "expected interval missed",
        });
      }
    }
    if (previous === undefined || capturedAtMs > previous.capturedAtMs) {
      this.receiptCursors.set(machineId, { capturedAtMs, expectedIntervalMs });
    }
  }

  private receiptsFor(runKey: string): RunReceipts {
    const existing = this.runs.get(runKey);
    if (existing !== undefined) {
      existing.touchedAtMs = this.now();
      return existing;
    }
    const receipts: RunReceipts = { intervals: [], touchedAtMs: this.now() };
    this.runs.set(runKey, receipts);
    if (this.runs.size > MAX_RUNS) {
      let oldestKey: string | null = null;
      let oldest = Number.POSITIVE_INFINITY;
      for (const [key, value] of this.runs) {
        if (key === runKey) continue;
        if (value.touchedAtMs < oldest) {
          oldest = value.touchedAtMs;
          oldestKey = key;
        }
      }
      if (oldestKey !== null) this.runs.delete(oldestKey);
    }
    return receipts;
  }

  private knownFor(machineId: string): KnownSeries[] {
    const byMachine = this.seriesByMachine.get(machineId);
    if (byMachine === undefined) return [];
    return [...byMachine.values()].map((buffer) => ({
      metric: buffer.metric,
      resourceId: buffer.resourceId,
    }));
  }

  private maxRetentionMs(): number {
    let max = 0;
    for (const config of BASE_TIERS) {
      const retention = this.tierRetentionMs[config.tier] ?? config.retentionMs;
      if (retention > max) max = retention;
    }
    return max;
  }

  private coarsestTier(): HistoryTier {
    return "1h";
  }

  private coarsestStepMs(): number {
    return HOUR_MS;
  }

  private memoryStatus(): MemoryHistoryStatus {
    const tierBytes = new Map<HistoryTier, number>();
    const tiers = BASE_TIERS.map((config): HistoryTierStatus => ({
      tier: config.tier,
      stepMs: config.stepMs,
      retentionMs: this.tierRetentionMs[config.tier] ?? config.retentionMs,
      bytes: 0,
    }));
    for (const buffer of this.series.values()) {
      for (const [tier, ring] of buffer.tiers) {
        tierBytes.set(tier, (tierBytes.get(tier) ?? 0) + ring.bytes);
      }
    }
    for (const tier of tiers) tier.bytes = tierBytes.get(tier.tier) ?? 0;
    return {
      budgetBytes: this.budgetBytes,
      usedBytes: this.usedBytes,
      pressure: this.evictedSeries > 0 || this.rejectedSeries > 0,
      seriesCount: this.series.size,
      machineCount: this.seriesByMachine.size,
      evictedSeries: this.evictedSeries,
      rejectedSeries: this.rejectedSeries,
      oldestMs: this.oldestMs,
      newestMs: this.newestMs,
      tiers,
    };
  }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function newBucket(t: number, value: number | null): CurrentBucket {
  if (value === null) {
    return { t, min: null, max: null, sum: 0, count: 0, nullCount: 1 };
  }
  return { t, min: value, max: value, sum: value, count: 1, nullCount: 0 };
}

function addValue(bucket: CurrentBucket, value: number | null): void {
  if (value === null) {
    bucket.nullCount += 1;
    return;
  }
  bucket.min = bucket.min === null ? value : Math.min(bucket.min, value);
  bucket.max = bucket.max === null ? value : Math.max(bucket.max, value);
  bucket.sum += value;
  bucket.count += 1;
}

function aggregateBuckets(
  buckets: readonly Bucket[],
  originMs: number,
  stepMs: number,
  maxPoints: number,
): Array<{ t: number; min: number | null; max: number | null; avg: number | null }> {
  const out = new Map<number, {
    t: number;
    min: number | null;
    max: number | null;
    sum: number;
    count: number;
    nullCount: number;
  }>();
  for (const bucket of buckets) {
    const t = originMs + Math.floor((bucket.t - originMs) / stepMs) * stepMs;
    let target = out.get(t);
    if (target === undefined) {
      target = { t, min: null, max: null, sum: 0, count: 0, nullCount: 0 };
      out.set(t, target);
    }
    if (bucket.count > 0) {
      target.min = target.min === null ? bucket.min : Math.min(target.min, bucket.min!);
      target.max = target.max === null ? bucket.max : Math.max(target.max, bucket.max!);
      target.sum += bucket.sum;
      target.count += bucket.count;
    }
    target.nullCount += bucket.nullCount;
  }
  return [...out.values()]
    .sort((a, b) => a.t - b.t)
    .slice(0, maxPoints + 1)
    .map((bucket) => ({
      t: bucket.t,
      min: bucket.count === 0 ? null : bucket.min,
      max: bucket.count === 0 ? null : bucket.max,
      avg: bucket.count === 0 ? null : bucket.sum / bucket.count,
    }));
}

function acceptSequence(receipts: RunReceipts, sequence: number): boolean {
  for (const [first, last] of receipts.intervals) {
    if (sequence >= first && sequence <= last) return false;
  }
  receipts.intervals.push([sequence, sequence]);
  receipts.intervals.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
  const merged: Array<[number, number]> = [];
  for (const interval of receipts.intervals) {
    const previous = merged.at(-1);
    if (previous !== undefined && interval[0] <= previous[1] + 1) {
      if (interval[1] > previous[1]) previous[1] = interval[1];
    } else {
      merged.push(interval);
    }
  }
  receipts.intervals = merged;
  if (receipts.intervals.length > MAX_RUN_INTERVALS) {
    // Enormous sequence gaps are pathological. Collapse the oldest pair rather
    // than growing without bound; a retry inside the collapsed range is then
    // treated as a duplicate, which is safer than double-counting it.
    receipts.intervals.splice(0, 2, [merged[0]![0], merged[1]![1]]);
  }
  return true;
}

function mod(value: number, divisor: number): number {
  return ((value % divisor) + divisor) % divisor;
}

function seriesKey(machineId: string, metric: string, resourceId: string): string {
  return `${machineId}\u0000${metric}\u0000${resourceId}`;
}

function needsKnownSeries(payload: MetricsPayload): boolean {
  return (
    payload.cpu.status === "unavailable" ||
    payload.gpus.status === "unavailable" ||
    payload.disks.status === "unavailable" ||
    payload.network.status === "unavailable"
  );
}

function rejectionReason(record: TelemetryRecord, batch: IngestBatch, receivedAtMs: number): string | null {
  if (record.collectorId !== batch.collectorId) {
    return "collectorId does not match the authenticated batch";
  }
  const capturedMs = Date.parse(record.capturedAt);
  if (!Number.isFinite(capturedMs)) return "capturedAt is not a parseable timestamp";
  if (capturedMs > receivedAtMs + FUTURE_TOLERANCE_MS) {
    return "capturedAt is more than 24h in the future";
  }
  return null;
}

function validateBatch(batch: IngestBatch): void {
  if (batch === null || typeof batch !== "object") throw new Error("ingest batch must be an object");
  if (typeof batch.machineId !== "string" || batch.machineId.length === 0) {
    throw new Error("ingest batch machineId must be a nonempty string");
  }
  if (typeof batch.collectorId !== "string" || batch.collectorId.length === 0) {
    throw new Error("ingest batch collectorId must be a nonempty string");
  }
  if (batch.mode !== "live" && batch.mode !== "replay") {
    throw new Error("ingest batch mode must be live or replay");
  }
  if (!Array.isArray(batch.records)) throw new Error("ingest batch records must be an array");
  if (batch.expectedIntervalMs !== null && !Number.isSafeInteger(batch.expectedIntervalMs)) {
    throw new Error("ingest batch expectedIntervalMs must be a safe integer or null");
  }
}

function validateTags(tags: unknown): string[] {
  if (!Array.isArray(tags) || tags.length > 64) throw new Error("tags must be an array of at most 64 strings");
  const result: string[] = [];
  const seen = new Set<string>();
  for (const tag of tags) {
    if (typeof tag !== "string" || tag.length === 0 || tag.length > 64) {
      throw new Error("tags must be nonempty strings of at most 64 characters");
    }
    if (seen.has(tag)) throw new Error("tags must be unique");
    seen.add(tag);
    result.push(tag);
  }
  return result;
}

function parseTags(raw: string): string[] {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((tag): tag is string => typeof tag === "string");
  } catch {
    return [];
  }
}

function positiveInteger(value: number, name: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`${name} must be a positive safe integer`);
  }
  return value;
}
