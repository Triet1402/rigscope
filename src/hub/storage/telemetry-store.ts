/**
 * Durable telemetry ingestion with commit-before-ack.
 *
 * The hub accepts batches into a bounded in-memory queue and coalesces them
 * into one `BEGIN IMMEDIATE ... COMMIT` per event-loop turn. A batch's promise
 * resolves only after COMMIT returns (or as `busy` on overload/failure), so the
 * hub can safely send an ack afterwards.
 *
 * Exactly-once side effects are achieved with `sample_receipts` as the dedup
 * key `(collectorId, runId, sequence)`: points, rollups and gaps are written
 * only when the receipt insert reported one changed row. A retry after a lost
 * ack therefore lands in `accepted` without touching history again.
 *
 * Live state (`machine_latest`) is updated only by `mode: "live"` metrics and
 * only when the new record is newer than the persisted row:
 * - same run: a higher sequence;
 * - different run: a later capturedAt.
 * Replay batches never update it and always return `newLive: null`.
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
import { isStorageFullError, DEFAULT_MAX_DATABASE_DISK_BYTES } from "./db";
export { DEFAULT_MAX_DATABASE_DISK_BYTES } from "./db";
import { RetentionManager, type RetentionPolicy, type RetentionStatus } from "./retention";
import { flattenMetrics, SeriesCatalog } from "./series";
import {
  queryHistory,
  type HistoryQuery,
  type HistoryQueryBudget,
  type HistoryResult,
} from "./history-query";
import type { HistorySource, HistoryStatus } from "./history";

const DAY_MS = 86_400_000;
const FUTURE_TOLERANCE_MS = DAY_MS;
const FAILURE_RETRY_MS = 5000;
/** A soft trigger below this is treated as test-only; the hard cap stays finite. */
const MIN_DERIVED_PHYSICAL_BYTES = 8 * 1024 * 1024;

export type IngestBatch = {
  machineId: string;
  collectorId: string;
  mode: "live" | "replay";
  expectedIntervalMs: number | null;
  /** Prepared by hello; persisted so restart recovery can restore it. */
  hostname?: string | null;
  records: TelemetryRecord[];
};

export type IngestResult =
  | {
      status: "committed";
      accepted: RecordRef[];
      rejected: (RecordRef & { reason: string })[];
      newLive: MetricsRecord | null;
      receivedAtMs: number;
    }
  | { status: "busy"; retryAfterMs: number; reason: string };

export type TelemetryStoreStats = {
  queuedRecords: number;
  queuedBatches: number;
  lastFlushMs: number | null;
  lastFlushDurationMs: number | null;
  committedRecords: number;
  duplicateRecords: number;
  rejectedRecords: number;
  writeFailures: number;
  lastWriteError: string | null;
  /** Failed attempts caused specifically by the finite physical budget. */
  diskFullFailures: number;
  dbBytes: number;
  dbWalBytes: number;
  dbBytesLimit: number | null;
  retention: RetentionStatus;
};

export type TelemetryStoreOptions = {
  database: HubDatabase;
  now?: () => number;
  maxQueuedRecords?: number;
  flushIntervalMs?: number;
  maxRecordsPerTransaction?: number;
  retention?: Partial<RetentionPolicy>;
  /**
   * `raw` keeps the historical one-row-per-reading layout used by tests and
   * operators who explicitly want every sample. `summary` writes one compact
   * ten-second aggregate row per series and is the opt-in persistent-history
   * default in the hub composition root.
   */
  storageTier?: "raw" | "summary";
  /** Soft retention trigger retained for compatibility. */
  maxDatabaseBytes?: number | null;
  /**
   * Finite physical cap (main file plus bounded WAL). When omitted it derives
   * from a production-sized maxDatabaseBytes, otherwise the 2 GiB default.
   * Set explicitly to null to disable the hard cap (tests/measurement only).
   */
  maxPhysicalBytes?: number | null;
};

type PendingBatch = {
  batch: IngestBatch;
  receivedAtMs: number;
  resolve: (result: IngestResult) => void;
  settled: boolean;
  accepted: RecordRef[];
  rejected: (RecordRef & { reason: string })[];
  newLive: MetricsRecord | null;
};

type LatestCursor = {
  runId: string;
  sequence: number;
  capturedAtMs: number;
};

type LiveUpsert = {
  record: MetricsRecord;
  receivedAtMs: number;
  expectedIntervalMs: number | null;
  hostname: string | null;
};

type TxnCounters = { committed: number; duplicates: number; rejected: number };

type SummaryAggregate = {
  min: number | null;
  max: number | null;
  sum: number;
  count: number;
  nullCount: number;
};

/** One compact summary per series and fixed bucket inside a flush. */
type SummaryBuckets = Map<number, Map<number, SummaryAggregate>>;

export class TelemetryStore implements HistorySource {
  readonly historyMode = "persistent" as const;
  private readonly database: HubDatabase;
  private readonly now: () => number;
  private readonly maxQueuedRecords: number;
  private readonly flushIntervalMs: number;
  private readonly maxRecordsPerTransaction: number;
  private readonly maxPhysicalBytes: number | null;
  private readonly storageTier: "raw" | "summary";
  private readonly series: SeriesCatalog;
  private readonly retention: RetentionManager;

  private readonly queue: PendingBatch[] = [];
  private timer: NodeJS.Timeout | null = null;
  private driver: Promise<void> | null = null;
  private closed = false;

  private committedRecords = 0;
  private duplicateRecords = 0;
  private rejectedRecords = 0;
  private writeFailures = 0;
  private diskFullFailures = 0;
  private lastWriteError: string | null = null;
  private lastFlushMs: number | null = null;
  private lastFlushDurationMs: number | null = null;

  private readonly receiptInsert: StatementSync;
  private readonly rawInsert: StatementSync;
  private readonly upsert10s: StatementSync;
  private readonly upsert1m: StatementSync;
  private readonly upsert1h: StatementSync;
  private readonly gapInsert: StatementSync;
  private readonly latestSelect: StatementSync;
  private readonly latestUpsert: StatementSync;
  private readonly inventoryUpsert: StatementSync;
  private readonly inventorySelect: StatementSync;
  private readonly metaUpsertBoth: StatementSync;
  private readonly metaUpsertDisplay: StatementSync;
  private readonly metaUpsertTags: StatementSync;
  private readonly metaTouch: StatementSync;
  private readonly metaSelect: StatementSync;
  private readonly metaSelectAll: StatementSync;
  private readonly latestSelectAll: StatementSync;

  constructor(options: TelemetryStoreOptions) {
    this.database = options.database;
    this.now = options.now ?? (() => Date.now());
    this.maxQueuedRecords = positiveInt(options.maxQueuedRecords, 4096, "maxQueuedRecords");
    this.flushIntervalMs = nonnegativeInt(options.flushIntervalMs, 20, "flushIntervalMs");
    this.maxRecordsPerTransaction = positiveInt(
      options.maxRecordsPerTransaction,
      512,
      "maxRecordsPerTransaction",
    );
    this.maxPhysicalBytes = resolvePhysicalLimit(options);
    this.storageTier = options.storageTier ?? "raw";
    if (this.storageTier !== "raw" && this.storageTier !== "summary") {
      throw new Error("storageTier must be raw or summary");
    }

    const db = options.database.db;
    this.series = new SeriesCatalog(db);
    this.receiptInsert = db.prepare(
      "INSERT OR IGNORE INTO sample_receipts " +
        "(collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms, expected_interval_ms) " +
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
    );
    this.rawInsert = db.prepare(
      "INSERT OR IGNORE INTO points_raw (series_id, ts_ms, value) VALUES (?, ?, ?)",
    );
    this.upsert10s = prepareRollupUpsert(db, "points_10s");
    this.upsert1m = prepareRollupUpsert(db, "points_1m");
    this.upsert1h = prepareRollupUpsert(db, "points_1h");
    this.gapInsert = db.prepare(
      "INSERT OR IGNORE INTO gaps " +
        "(collector_id, run_id, seq, machine_id, from_ms, to_ms, dropped, reason, received_at_ms) " +
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
    );
    this.latestSelect = db.prepare(
      "SELECT run_id, seq, captured_at_ms FROM machine_latest WHERE machine_id = ?",
    );
    this.latestUpsert = db.prepare(
      "INSERT INTO machine_latest " +
        "(machine_id, collector_id, run_id, boot_id, seq, captured_at_ms, received_at_ms, " +
        "expected_interval_ms, hostname, payload_json) " +
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) " +
        "ON CONFLICT(machine_id) DO UPDATE SET " +
        "collector_id = excluded.collector_id, run_id = excluded.run_id, boot_id = excluded.boot_id, " +
        "seq = excluded.seq, captured_at_ms = excluded.captured_at_ms, " +
        "received_at_ms = excluded.received_at_ms, " +
        // A batch may omit these; never erase a previously restored value.
        "expected_interval_ms = COALESCE(excluded.expected_interval_ms, expected_interval_ms), " +
        "hostname = COALESCE(excluded.hostname, hostname), " +
        "payload_json = excluded.payload_json",
    );
    this.latestSelectAll = db.prepare(
      "SELECT machine_id, collector_id, run_id, boot_id, seq, captured_at_ms, received_at_ms, " +
        "expected_interval_ms, hostname, payload_json " +
        "FROM machine_latest ORDER BY machine_id",
    );
    this.inventoryUpsert = db.prepare(
      "INSERT INTO inventory (machine_id, captured_at_ms, received_at_ms, inventory_json) " +
        "VALUES (?, ?, ?, ?) " +
        "ON CONFLICT(machine_id) DO UPDATE SET captured_at_ms = excluded.captured_at_ms, " +
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

    this.retention = new RetentionManager(options.database, {
      policy: options.retention,
      maxDatabaseBytes: options.maxDatabaseBytes ?? null,
      maxPhysicalBytes: this.maxPhysicalBytes,
      now: this.now,
    });
    this.retention.start();
  }

  /**
   * Queues one batch. The promise resolves only after the transaction that
   * writes it has committed, or immediately as `busy` when the bounded queue
   * cannot hold it (no database work is done in that case).
   */
  enqueue(batch: IngestBatch): Promise<IngestResult> {
    return new Promise<IngestResult>((resolve) => {
      if (this.closed) {
        resolve({ status: "busy", retryAfterMs: FAILURE_RETRY_MS, reason: "storage is closed" });
        return;
      }
      validateBatch(batch);
      const queued = this.queuedRecords();
      if (queued + batch.records.length > this.maxQueuedRecords) {
        const ratio = this.maxQueuedRecords === 0 ? 1 : queued / this.maxQueuedRecords;
        const retryAfterMs = Math.min(5000, Math.max(1000, Math.round(1000 + ratio * 4000)));
        resolve({ status: "busy", retryAfterMs, reason: "ingest queue is full" });
        return;
      }
      this.queue.push({
        batch,
        receivedAtMs: this.now(),
        resolve,
        settled: false,
        accepted: [],
        rejected: [],
        newLive: null,
      });
      this.scheduleFlush(this.flushIntervalMs);
    });
  }

  /** Persisted latest LIVE metrics per machine, for restart recovery. */
  latestLive(): Array<{
    machineId: string;
    collectorId: string;
    runId: string;
    bootId: string;
    sequence: number;
    capturedAt: string;
    receivedAt: string;
    hostname: string | null;
    expectedIntervalMs: number | null;
    payload: MetricsPayload;
  }> {
    const rows = this.latestSelectAll.all() as Array<{
      machine_id: string;
      collector_id: string;
      run_id: string;
      boot_id: string;
      seq: number | bigint;
      captured_at_ms: number | bigint;
      received_at_ms: number | bigint;
      expected_interval_ms: number | bigint | null;
      hostname: string | null;
      payload_json: string;
    }>;
    return rows.map((row) => ({
      machineId: row.machine_id,
      collectorId: row.collector_id,
      runId: row.run_id,
      bootId: row.boot_id,
      sequence: Number(row.seq),
      capturedAt: new Date(Number(row.captured_at_ms)).toISOString(),
      receivedAt: new Date(Number(row.received_at_ms)).toISOString(),
      hostname: row.hostname,
      expectedIntervalMs:
        row.expected_interval_ms === null ? null : Number(row.expected_interval_ms),
      payload: JSON.parse(row.payload_json) as MetricsPayload,
    }));
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

  /**
   * Merges presentation metadata. Omitted fields are preserved; an explicit
   * `displayName: null` clears it.
   */
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

  stats(): TelemetryStoreStats {
    return {
      queuedRecords: this.queuedRecords(),
      queuedBatches: this.queue.length,
      lastFlushMs: this.lastFlushMs,
      lastFlushDurationMs: this.lastFlushDurationMs,
      committedRecords: this.committedRecords,
      duplicateRecords: this.duplicateRecords,
      rejectedRecords: this.rejectedRecords,
      writeFailures: this.writeFailures,
      lastWriteError: this.lastWriteError,
      diskFullFailures: this.diskFullFailures,
      dbBytes: this.database.sizeBytes(),
      dbWalBytes: this.database.walBytes(),
      dbBytesLimit: this.maxPhysicalBytes,
      retention: this.retention.status(),
    };
  }

  queryHistory(query: HistoryQuery, budget: HistoryQueryBudget = {}): HistoryResult {
    return queryHistory(this.database, query, {
      ...budget,
      nowMs: budget.nowMs ?? this.now(),
      retention: { ...this.retention.policy, ...budget.retention },
      tiers:
        budget.tiers ??
        (this.storageTier === "summary" ? (["10s", "1m", "1h"] as const) : undefined),
    });
  }

  historyStatus(): HistoryStatus {
    const stats = this.stats();
    const physical = this.database.physicalLimit();
    const now = this.now();
    const retention = this.retention.policy;
    return {
      mode: "persistent",
      persistentHistory: true,
      availableFromMs:
        this.storageTier === "summary"
          ? now - Math.max(retention.tenSecondMs, retention.oneMinuteMs, retention.oneHourMs)
          : now - Math.max(retention.rawMs, retention.oneMinuteMs, retention.oneHourMs),
      availableToMs: now,
      memory: null,
      persistence: {
        enabled: true,
        storageTier: this.storageTier,
        flushIntervalMs: this.flushIntervalMs,
        queuedRecords: stats.queuedRecords,
        queuedBatches: stats.queuedBatches,
        lastFlushMs: stats.lastFlushMs,
        lastFlushDurationMs: stats.lastFlushDurationMs,
        writeFailures: stats.writeFailures,
        lastWriteError: stats.lastWriteError,
        dbBytes: stats.dbBytes,
        dbWalBytes: stats.dbWalBytes,
        dbBytesLimit: physical?.limitBytes ?? this.maxPhysicalBytes,
        tiers:
          this.storageTier === "summary"
            ? [
                { tier: "10s", stepMs: 10_000, retentionMs: retention.tenSecondMs, bytes: stats.dbBytes },
                { tier: "1m", stepMs: 60_000, retentionMs: retention.oneMinuteMs, bytes: 0 },
                { tier: "1h", stepMs: 3_600_000, retentionMs: retention.oneHourMs, bytes: 0 },
              ]
            : [
                { tier: "raw", stepMs: 100, retentionMs: retention.rawMs, bytes: stats.dbBytes },
                { tier: "1m", stepMs: 60_000, retentionMs: retention.oneMinuteMs, bytes: 0 },
                { tier: "1h", stepMs: 3_600_000, retentionMs: retention.oneHourMs, bytes: 0 },
              ],
      },
      gaps: 0,
      acceptedRecords: stats.committedRecords,
      generatedAt: new Date(now).toISOString(),
    };
  }

  /** Drains the queue immediately; intended for tests and shutdown. */
  async flushNow(): Promise<void> {
    if (this.closed) return;
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    await this.drive();
    // A batch may have arrived while the previous driver was finishing.
    while (!this.closed && this.queue.length > 0 && this.driver === null) {
      const failures = this.writeFailures;
      await this.drive();
      if (this.writeFailures > failures) break; // do not spin on a broken disk
    }
  }

  /**
   * Drains for up to ~2s, then resolves anything left as busy and closes the
   * database. Batches that were already committed have resolved normally.
   */
  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.retention.stop();
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (this.driver !== null) {
      try {
        await this.driver;
      } catch {
        // The driver reports failures through pending promises and stats.
      }
    }

    const deadline = Date.now() + 2000;
    while (this.queue.length > 0 && Date.now() < deadline) {
      const ok = this.flushTransaction();
      if (!ok) break;
      if (this.queue.length > 0) await yieldToEventLoop();
    }
    while (this.queue.length > 0) {
      const pending = this.queue.shift()!;
      this.resolveBusy(pending, FAILURE_RETRY_MS, "storage is shutting down");
    }
    try {
      this.database.close();
    } catch {
      // Closing must be idempotent for tests and shutdown paths.
    }
  }

  private queuedRecords(): number {
    let total = 0;
    for (const pending of this.queue) total += pending.batch.records.length;
    return total;
  }

  private scheduleFlush(delayMs: number): void {
    if (this.closed || this.timer !== null || this.driver !== null) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.drive();
    }, delayMs);
    this.timer.unref?.();
  }

  private drive(): Promise<void> {
    if (this.driver !== null) return this.driver;
    const promise = this.runDriver().finally(() => {
      if (this.driver === promise) this.driver = null;
      if (!this.closed && this.queue.length > 0 && this.timer === null) {
        // Only reachable after a write failure paused the loop.
        this.scheduleFlush(FAILURE_RETRY_MS);
      }
    });
    this.driver = promise;
    return promise;
  }

  private async runDriver(): Promise<void> {
    while (!this.closed && this.queue.length > 0) {
      const ok = this.flushTransaction();
      if (!ok) return;
      if (this.queue.length > 0) await yieldToEventLoop();
    }
  }

  /**
   * Commits the front of the queue as one transaction. Returns false when the
   * transaction failed; every batch in it has then been resolved as busy.
   */
  private flushTransaction(): boolean {
    if (this.queue.length === 0) return true;
    const selected: PendingBatch[] = [];
    let recordCount = 0;
    for (const pending of this.queue) {
      const size = pending.batch.records.length;
      // Always take at least one batch: contract batches are ≤32 records, well
      // under the default 512, and keeping a batch atomic preserves its
      // single newLive/ack result.
      if (selected.length > 0 && recordCount + size > this.maxRecordsPerTransaction) break;
      selected.push(pending);
      recordCount += size;
      if (recordCount >= this.maxRecordsPerTransaction) break;
    }
    this.queue.splice(0, selected.length);

    if (this.maxPhysicalBytes !== null) {
      try {
        let physical = this.database.physicalLimit();
        let bytes = this.database.sizeBytes();
        const pageFull =
          physical !== null && physical.pageCount >= physical.maxPageCount && physical.freePages === 0;
        if (bytes >= this.maxPhysicalBytes || pageFull) {
          // Reclaim safely: oldest raw/one-minute rows outside the last-hour
          // floor plus a bounded WAL checkpoint/incremental vacuum. Never drop
          // recent history or one-hour rollups just to accept this batch.
          this.retention.relievePressure(4);
          physical = this.database.physicalLimit();
          bytes = this.database.sizeBytes();
        }
        const stillFull =
          physical !== null && physical.pageCount >= physical.maxPageCount && physical.freePages === 0;
        if (bytes >= this.maxPhysicalBytes || stillFull) {
          this.diskFullFailures += 1;
          this.lastWriteError = "storage disk budget reached";
          for (const pending of selected) {
            this.resolveBusy(pending, FAILURE_RETRY_MS, "storage disk budget reached");
          }
          return false;
        }
      } catch (error) {
        const full = isStorageFullError(error);
        if (full) this.diskFullFailures += 1;
        this.lastWriteError = full
          ? "storage disk budget reached"
          : sanitizeWriteError(error);
        for (const pending of selected) {
          this.resolveBusy(
            pending,
            FAILURE_RETRY_MS,
            full ? "storage disk budget reached" : "storage write failed",
          );
        }
        return false;
      }
    }

    const started = performance.now();
    const counters: TxnCounters = { committed: 0, duplicates: 0, rejected: 0 };
    // Per-transaction view of the persisted latest row. Batches are processed
    // in order, so a later live batch can supersede an earlier one.
    const cursors = new Map<string, LatestCursor | null>();
    const upserts = new Map<string, LiveUpsert>();
    const summaries: SummaryBuckets = new Map();

    try {
      this.database.db.exec("BEGIN IMMEDIATE");
      for (const pending of selected) {
        this.processBatch(pending, counters, cursors, upserts, summaries);
      }
      if (this.storageTier === "summary" && summaries.size > 0) {
        this.writeSummaries(summaries);
      }
      // Persist the final latest record per machine in the same transaction.
      for (const [machineId, live] of upserts) {
        this.latestUpsert.run(
          machineId,
          live.record.collectorId,
          live.record.runId,
          live.record.bootId,
          live.record.sequence,
          Date.parse(live.record.capturedAt),
          live.receivedAtMs,
          live.expectedIntervalMs,
          live.hostname,
          JSON.stringify(live.record.payload),
        );
      }
      this.database.db.exec("COMMIT");
    } catch (error) {
      try {
        this.database.db.exec("ROLLBACK");
      } catch {
        // A failed ROLLBACK means SQLite already rolled back or closed.
      }
      this.writeFailures += 1;
      const full = isStorageFullError(error);
      if (full) this.diskFullFailures += 1;
      this.lastWriteError = full
        ? "storage disk budget reached"
        : sanitizeWriteError(error);
      for (const pending of selected) {
        this.resolveBusy(
          pending,
          FAILURE_RETRY_MS,
          full ? "storage disk budget reached" : "storage write failed",
        );
      }
      return false;
    }

    this.committedRecords += counters.committed;
    this.duplicateRecords += counters.duplicates;
    this.rejectedRecords += counters.rejected;
    this.lastFlushMs = this.now();
    this.lastFlushDurationMs = Math.max(0, Math.round(performance.now() - started));
    this.checkpointWALIfNeeded();
    // Re-measure after the write: an entire atomic batch may consume the
    // remaining WAL headroom. Reclaim bounded work before returning its ack;
    // a blocked checkpoint leaves explicit pressure for the next preflight.
    if (this.maxPhysicalBytes !== null && this.database.sizeBytes() > this.maxPhysicalBytes) {
      try { this.retention.relievePressure(4); } catch { /* The commit remains durable. */ }
    }
    for (const pending of selected) this.resolveCommitted(pending);
    return true;
  }

  private checkpointWALIfNeeded(): void {
    if (this.maxPhysicalBytes === null) return;
    try {
      const physical = this.database.physicalLimit();
      if (physical !== null && this.database.walBytes() > physical.walBudgetBytes) {
        this.database.checkpoint();
      }
    } catch {
      // A busy checkpoint does not affect the committed transaction.
    }
  }

  private processBatch(
    pending: PendingBatch,
    counters: TxnCounters,
    cursors: Map<string, LatestCursor | null>,
    upserts: Map<string, LiveUpsert>,
    summaries: SummaryBuckets,
  ): void {
    const batch = pending.batch;
    let best: MetricsRecord | null = null;
    let bestCapturedMs = 0;
    let bestSequence = -1;

    for (const record of batch.records) {
      const ref: RecordRef = { runId: record.runId, sequence: record.sequence };
      const reason = rejectionReason(record, batch, pending.receivedAtMs);
      if (reason !== null) {
        pending.rejected.push({ ...ref, reason });
        counters.rejected += 1;
        continue;
      }
      const capturedMs = Date.parse(record.capturedAt);
      const inserted =
        Number(
          this.receiptInsert.run(
            batch.collectorId,
            record.runId,
            record.sequence,
            batch.machineId,
            record.kind,
            batch.mode,
            capturedMs,
            pending.receivedAtMs,
            batch.expectedIntervalMs,
          ).changes,
        ) === 1;

      if (!inserted) {
        // Lost-ack / retry: the hub already holds this record. Acknowledge it
        // as accepted but do not touch points, rollups, gaps or live state.
        pending.accepted.push(ref);
        counters.duplicates += 1;
        continue;
      }

      pending.accepted.push(ref);
      counters.committed += 1;

      if (record.kind === "metrics") {
        // Only consult the known-series catalog when a family is unavailable:
        // the common path must stay a pure flatten + cached series-id walk.
        const known = needsKnownSeries(record.payload)
          ? this.series.knownFor(batch.machineId)
          : undefined;
        for (const point of flattenMetrics(record.payload, known)) {
          const seriesId = this.series.idFor(batch.machineId, point.metric, point.resourceId);
          if (this.storageTier === "summary") {
            addSummaryPoint(summaries, seriesId, capturedMs, point.value);
          } else {
            this.insertPoint(seriesId, capturedMs, point.value);
          }
        }
        if (batch.mode === "live") {
          if (
            best === null ||
            capturedMs > bestCapturedMs ||
            (capturedMs === bestCapturedMs && record.sequence > bestSequence)
          ) {
            best = record;
            bestCapturedMs = capturedMs;
            bestSequence = record.sequence;
          }
        }
      } else {
        this.gapInsert.run(
          batch.collectorId,
          record.runId,
          record.sequence,
          batch.machineId,
          Date.parse(record.payload.fromCapturedAt),
          Date.parse(record.payload.toCapturedAt),
          record.payload.droppedRecords,
          record.payload.reason,
          pending.receivedAtMs,
        );
      }
    }

    if (batch.mode !== "live" || best === null) return;
    const cursor = this.latestCursor(batch.machineId, cursors);
    const candidate: LatestCursor = {
      runId: best.runId,
      sequence: best.sequence,
      capturedAtMs: bestCapturedMs,
    };
    if (cursor === null || isNewer(candidate, cursor)) {
      pending.newLive = best;
      cursors.set(batch.machineId, candidate);
      upserts.set(batch.machineId, {
        record: best,
        receivedAtMs: pending.receivedAtMs,
        expectedIntervalMs: batch.expectedIntervalMs,
        hostname: batch.hostname ?? null,
      });
    }
  }

  private latestCursor(machineId: string, cache: Map<string, LatestCursor | null>): LatestCursor | null {
    if (cache.has(machineId)) return cache.get(machineId)!;
    const row = this.latestSelect.get(machineId) as
      | { run_id: string; seq: number | bigint; captured_at_ms: number | bigint }
      | undefined;
    const cursor = row
      ? { runId: row.run_id, sequence: Number(row.seq), capturedAtMs: Number(row.captured_at_ms) }
      : null;
    cache.set(machineId, cursor);
    return cursor;
  }

  /**
   * Writes one compact ten-second summary per series/bucket and folds the same
   * aggregate into both larger rollups. The ten-second row is upserted because
   * late or replayed samples may extend a bucket across flushes; the one-minute
   * and one-hour rows only receive the new delta, so a sample is counted once.
   */
  private writeSummaries(summaries: SummaryBuckets): void {
    for (const [seriesId, buckets] of summaries) {
      for (const [bucketMs, aggregate] of buckets) {
        this.upsert10s.run(
          seriesId,
          bucketMs,
          aggregate.min,
          aggregate.max,
          aggregate.sum,
          aggregate.count,
          aggregate.nullCount,
        );
        this.applyRollupAggregate(
          this.upsert1m,
          seriesId,
          Math.floor(bucketMs / 60_000) * 60_000,
          aggregate,
        );
        this.applyRollupAggregate(
          this.upsert1h,
          seriesId,
          Math.floor(bucketMs / 3_600_000) * 3_600_000,
          aggregate,
        );
      }
    }
  }

  /**
   * Writes one raw point and incrementally updates both rollup tiers. The raw
   * INSERT OR IGNORE guard prevents a collision (same series/timestamp) from
   * double-counting a rollup.
   */
  private insertPoint(seriesId: number, tsMs: number, value: number | null): void {
    if (Number(this.rawInsert.run(seriesId, tsMs, value).changes) !== 1) return;
    const minute = Math.floor(tsMs / 60_000) * 60_000;
    const hour = Math.floor(tsMs / 3_600_000) * 3_600_000;
    this.applyRollup(this.upsert1m, seriesId, minute, value);
    this.applyRollup(this.upsert1h, seriesId, hour, value);
  }

  private applyRollup(
    statement: StatementSync,
    seriesId: number,
    bucketMs: number,
    value: number | null,
  ): void {
    if (value === null) {
      statement.run(seriesId, bucketMs, null, null, 0, 0, 1);
    } else {
      statement.run(seriesId, bucketMs, value, value, value, 1, 0);
    }
  }

  private applyRollupAggregate(
    statement: StatementSync,
    seriesId: number,
    bucketMs: number,
    aggregate: SummaryAggregate,
  ): void {
    statement.run(
      seriesId,
      bucketMs,
      aggregate.min,
      aggregate.max,
      aggregate.sum,
      aggregate.count,
      aggregate.nullCount,
    );
  }

  private resolveCommitted(pending: PendingBatch): void {
    if (pending.settled) return;
    pending.settled = true;
    pending.resolve({
      status: "committed",
      accepted: pending.accepted,
      rejected: pending.rejected,
      newLive: pending.newLive,
      receivedAtMs: pending.receivedAtMs,
    });
  }

  private resolveBusy(pending: PendingBatch, retryAfterMs: number, reason: string): void {
    if (pending.settled) return;
    pending.settled = true;
    pending.resolve({ status: "busy", retryAfterMs, reason });
  }
}

function prepareRollupUpsert(
  db: { prepare(sql: string): StatementSync },
  table: "points_10s" | "points_1m" | "points_1h",
): StatementSync {
  return db.prepare(
    `INSERT INTO ${table} (series_id, bucket_ms, min, max, sum, count, null_count) ` +
      "VALUES (?, ?, ?, ?, ?, ?, ?) " +
      "ON CONFLICT(series_id, bucket_ms) DO UPDATE SET " +
      // NULL values only advance null_count; min/max/sum/count are untouched.
      "min = CASE WHEN excluded.count = 0 THEN min " +
      "WHEN min IS NULL THEN excluded.min ELSE MIN(min, excluded.min) END, " +
      "max = CASE WHEN excluded.count = 0 THEN max " +
      "WHEN max IS NULL THEN excluded.max ELSE MAX(max, excluded.max) END, " +
      "sum = sum + excluded.sum, " +
      "count = count + excluded.count, " +
      "null_count = null_count + excluded.null_count",
  );
}

/** Same run compares sequence; a different run compares capture time. */
function isNewer(candidate: LatestCursor, latest: LatestCursor): boolean {
  if (candidate.runId === latest.runId) return candidate.sequence > latest.sequence;
  return candidate.capturedAtMs > latest.capturedAtMs;
}

/** True when flattening needs the machine's known series to emit NULL breaks. */
function needsKnownSeries(payload: MetricsPayload): boolean {
  return (
    payload.cpu.status === "unavailable" ||
    payload.gpus.status === "unavailable" ||
    payload.disks.status === "unavailable" ||
    payload.network.status === "unavailable"
  );
}

function rejectionReason(record: TelemetryRecord, batch: IngestBatch, receivedAtMs: number): string | null {  if (record.collectorId !== batch.collectorId) {
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
  if (
    batch.hostname !== undefined &&
    batch.hostname !== null &&
    (typeof batch.hostname !== "string" || batch.hostname.length > 255)
  ) {
    throw new Error("ingest batch hostname must be a string of at most 255 characters or null");
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
    // Corrupt presentation metadata must not take down telemetry.
    return [];
  }
}

function sanitizeWriteError(error: unknown): string {
  // SQLite errors do not include bound values, but keep this short and
  // single-line so a huge payload can never end up in a log/status field.
  const message = error instanceof Error ? error.message : String(error);
  return message.replace(/\s+/g, " ").slice(0, 200);
}

function positiveInt(value: number | undefined, fallback: number, name: string): number {
  const resolved = value ?? fallback;
  if (!Number.isSafeInteger(resolved) || resolved <= 0) {
    throw new Error(`${name} must be a positive safe integer`);
  }
  return resolved;
}

function resolvePhysicalLimit(options: TelemetryStoreOptions): number | null {
  if (options.maxPhysicalBytes === null) return null;
  if (options.maxPhysicalBytes !== undefined) {
    if (!Number.isSafeInteger(options.maxPhysicalBytes) || options.maxPhysicalBytes < 16 * 1024) {
      throw new Error("maxPhysicalBytes must be a safe integer of at least 16 KiB or null");
    }
    return options.maxPhysicalBytes;
  }
  if (
    options.maxDatabaseBytes !== null &&
    options.maxDatabaseBytes !== undefined &&
    options.maxDatabaseBytes >= MIN_DERIVED_PHYSICAL_BYTES
  ) {
    return options.maxDatabaseBytes;
  }
  return DEFAULT_MAX_DATABASE_DISK_BYTES;
}

function nonnegativeInt(value: number | undefined, fallback: number, name: string): number {
  const resolved = value ?? fallback;
  if (!Number.isSafeInteger(resolved) || resolved < 0) {
    throw new Error(`${name} must be a nonnegative safe integer`);
  }
  return resolved;
}

const SUMMARY_STEP_MS = 10_000;

function addSummaryPoint(
  summaries: SummaryBuckets,
  seriesId: number,
  tsMs: number,
  value: number | null,
): void {
  const bucketMs = Math.floor(tsMs / SUMMARY_STEP_MS) * SUMMARY_STEP_MS;
  let buckets = summaries.get(seriesId);
  if (buckets === undefined) {
    buckets = new Map();
    summaries.set(seriesId, buckets);
  }
  let aggregate = buckets.get(bucketMs);
  if (aggregate === undefined) {
    aggregate = { min: null, max: null, sum: 0, count: 0, nullCount: 0 };
    buckets.set(bucketMs, aggregate);
  }
  if (value === null) {
    aggregate.nullCount += 1;
  } else {
    aggregate.min = aggregate.min === null ? value : Math.min(aggregate.min, value);
    aggregate.max = aggregate.max === null ? value : Math.max(aggregate.max, value);
    aggregate.sum += value;
    aggregate.count += 1;
  }
}

function yieldToEventLoop(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}
