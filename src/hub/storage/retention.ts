/**
 * Bounded retention and disk budget.
 *
 * Retention never issues one huge DELETE: every statement is limited to
 * `chunkSize` rows (5000 by default) and the pass yields to the event loop
 * between chunks. Because `node:sqlite` is synchronous, that is also what keeps
 * a large cleanup from starving ingestion: each chunk is its own short
 * transaction and ingestion runs between chunks.
 *
 * Under soft `maxDatabaseBytes` or hard `maxPhysicalBytes` pressure we delete
 * the oldest raw points first, but never the most recent hour, then the oldest
 * one-minute rollups (also keeping the most recent hour). One-hour rollups are
 * the long-term history and are only removed by normal age retention. If the
 * last hour alone exceeds the hard cap, ingestion rejects busy; it never deletes
 * recent history to pretend the budget fits.
 */

import type { HubDatabase } from "./db";

const HOUR_MS = 3_600_000;
const DAY_MS = 24 * HOUR_MS;

export type RetentionPolicy = {
  rawMs: number;
  oneMinuteMs: number;
  oneHourMs: number;
  receiptsMs: number;
  gapsMs: number;
};

/** Defaults are deliberately conservative; callers may override per hub. */
export const DEFAULT_RETENTION_POLICY: RetentionPolicy = {
  rawMs: 2 * DAY_MS,
  oneMinuteMs: 30 * DAY_MS,
  oneHourMs: 400 * DAY_MS,
  receiptsMs: 2 * DAY_MS,
  gapsMs: 400 * DAY_MS,
};

/**
 * Merges a partial policy. `receiptsMs` and `gapsMs` default to the *resolved*
 * raw/hour values, so `{ rawMs }` also shortens receipts unless explicitly set.
 */
export function resolveRetentionPolicy(partial?: Partial<RetentionPolicy>): RetentionPolicy {
  const rawMs = checkedMs(partial?.rawMs, DEFAULT_RETENTION_POLICY.rawMs, "rawMs");
  const oneMinuteMs = checkedMs(partial?.oneMinuteMs, DEFAULT_RETENTION_POLICY.oneMinuteMs, "oneMinuteMs");
  const oneHourMs = checkedMs(partial?.oneHourMs, DEFAULT_RETENTION_POLICY.oneHourMs, "oneHourMs");
  const receiptsDefault = partial?.rawMs !== undefined ? rawMs : DEFAULT_RETENTION_POLICY.receiptsMs;
  const gapsDefault = partial?.oneHourMs !== undefined ? oneHourMs : DEFAULT_RETENTION_POLICY.gapsMs;
  return {
    rawMs,
    oneMinuteMs,
    oneHourMs,
    receiptsMs: checkedMs(partial?.receiptsMs, receiptsDefault, "receiptsMs"),
    gapsMs: checkedMs(partial?.gapsMs, gapsDefault, "gapsMs"),
  };
}

export type RetentionStatus = {
  /** 0 until the first chunk has run. */
  lastRunAtMs: number;
  lastRunDurationMs: number;
  /** Cumulative rows deleted by this manager instance. */
  deletedRows: number;
  pressure: boolean;
  /** Oldest raw timestamp that may survive; null only if never computed. */
  effectiveRawCutoffMs: number | null;
  /** Actual physical bytes (main + WAL) at the last status/run check. */
  physicalBytes: number;
  physicalLimitBytes: number | null;
  physicalPressure: boolean;
  walBytes: number;
};

export type RetentionManagerOptions = {
  policy?: Partial<RetentionPolicy>;
  /**
   * Soft retention trigger. Older code used this as a deletion heuristic;
   * throughput is still bounded and the most recent hour is protected.
   */
  maxDatabaseBytes?: number | null;
  /**
   * Finite physical cap applied through SQLite `max_page_count` plus a
   * bounded WAL reserve. Existing databases over the cap enter pressure mode
   * without a startup VACUUM or destructive delete.
   */
  maxPhysicalBytes?: number | null;
  now?: () => number;
  /** Default 60s; timers are unref'd so they never hold the process open. */
  intervalMs?: number;
  /** Maximum rows per DELETE statement. */
  chunkSize?: number;
  /** Safety valve per pass; the next interval continues where this stopped. */
  maxChunksPerPass?: number;
};

export class RetentionManager {
  readonly policy: RetentionPolicy;
  private readonly db: HubDatabase;
  private readonly maxDatabaseBytes: number | null;
  private readonly physicalLimitBytes: number | null;
  private readonly now: () => number;
  private readonly intervalMs: number;
  private readonly chunkSize: number;
  private readonly maxChunksPerPass: number;

  private timer: NodeJS.Timeout | null = null;
  private started = false;
  private readonly state: RetentionStatus = {
    lastRunAtMs: 0,
    lastRunDurationMs: 0,
    deletedRows: 0,
    pressure: false,
    effectiveRawCutoffMs: null,
    physicalBytes: 0,
    physicalLimitBytes: null,
    physicalPressure: false,
    walBytes: 0,
  };

  private readonly deleteRaw;
  private readonly delete1m;
  private readonly delete1h;
  private readonly deleteReceipts;
  private readonly deleteGaps;

  constructor(database: HubDatabase, options: RetentionManagerOptions = {}) {
    this.db = database;
    this.policy = resolveRetentionPolicy(options.policy);
    if (options.maxDatabaseBytes != null) {
      if (!Number.isSafeInteger(options.maxDatabaseBytes) || options.maxDatabaseBytes < 0) {
        throw new Error("maxDatabaseBytes must be a nonnegative safe integer or null");
      }
    }
    this.maxDatabaseBytes = options.maxDatabaseBytes ?? null;
    if (options.maxPhysicalBytes != null) {
      if (!Number.isSafeInteger(options.maxPhysicalBytes) || options.maxPhysicalBytes < 16 * 1024) {
        throw new Error("maxPhysicalBytes must be a safe integer of at least 16 KiB or null");
      }
    }
    this.physicalLimitBytes = options.maxPhysicalBytes ?? null;
    if (this.physicalLimitBytes !== null) {
      this.db.setPhysicalLimit(this.physicalLimitBytes);
    }
    this.now = options.now ?? (() => Date.now());
    this.intervalMs = positiveInt(options.intervalMs, 60_000, "intervalMs");
    this.chunkSize = positiveInt(options.chunkSize, 5000, "chunkSize");
    this.maxChunksPerPass = positiveInt(options.maxChunksPerPass, 10_000, "maxChunksPerPass");

    // All statements are bounded by the same LIMIT. The IN-subquery reads the
    // clustered primary key (series, ts) or the captured_at index, so it stops
    // after finding `chunkSize` expired rows instead of scanning the table.
    this.deleteRaw = database.db.prepare(
      "DELETE FROM points_raw WHERE (series_id, ts_ms) IN " +
        "(SELECT series_id, ts_ms FROM points_raw WHERE ts_ms < ? LIMIT ?)",
    );
    this.delete1m = database.db.prepare(
      "DELETE FROM points_1m WHERE (series_id, bucket_ms) IN " +
        "(SELECT series_id, bucket_ms FROM points_1m WHERE bucket_ms < ? LIMIT ?)",
    );
    this.delete1h = database.db.prepare(
      "DELETE FROM points_1h WHERE (series_id, bucket_ms) IN " +
        "(SELECT series_id, bucket_ms FROM points_1h WHERE bucket_ms < ? LIMIT ?)",
    );
    this.deleteReceipts = database.db.prepare(
      "DELETE FROM sample_receipts WHERE (collector_id, run_id, seq) IN " +
        "(SELECT collector_id, run_id, seq FROM sample_receipts WHERE captured_at_ms < ? LIMIT ?)",
    );
    this.deleteGaps = database.db.prepare(
      "DELETE FROM gaps WHERE (collector_id, run_id, seq) IN " +
        "(SELECT collector_id, run_id, seq FROM gaps WHERE to_ms < ? LIMIT ?)",
    );
    this.refreshPhysicalState();
  }

  start(): void {
    if (this.started) return;
    this.started = true;
    this.schedule();
  }

  stop(): void {
    this.started = false;
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  status(): RetentionStatus {
    this.refreshPhysicalState();
    return { ...this.state };
  }

  /** True while either the soft trigger or the hard physical cap is exceeded. */
  overBudget(): boolean {
    const bytes = this.db.sizeBytes();
    const physical = this.db.physicalLimit();
    return (
      (this.maxDatabaseBytes !== null && bytes > this.maxDatabaseBytes) ||
      (this.physicalLimitBytes !== null && (physical?.pressure ?? bytes > this.physicalLimitBytes))
    );
  }

  /**
   * Synchronous, bounded pressure relief for the write path. Deletes only
   * oldest raw/one-minute rows outside the most-recent-hour floor, then asks
   * SQLite for bounded incremental reclaim. It never deletes one-hour rollups
   * to pretend a hard cap fits.
   */
  relievePressure(maxChunks = 4): number {
    let deleted = 0;
    for (let chunk = 0; chunk < Math.max(1, maxChunks); chunk += 1) {
      if (!this.overBudget()) break;
      const count = this.runChunk();
      deleted += count;
      if (count === 0) break;
    }
    this.db.reclaimPages(128);
    try {
      this.db.checkpoint();
    } catch {
      // A busy checkpoint must not fail ingestion; the page cap still holds.
    }
    this.refreshPhysicalState();
    return deleted;
  }

  /**
   * One bounded DELETE statement (or a cheap probe when nothing is expired).
   * Returns the number of rows deleted, at most `chunkSize`.
   */
  runChunk(): number {
    const started = performance.now();
    const nowMs = this.now();
    const rawCutoff = nowMs - this.policy.rawMs;

    let deleted =
      this.deleteUpTo(this.deleteRaw, rawCutoff) ||
      this.deleteUpTo(this.delete1m, nowMs - this.policy.oneMinuteMs) ||
      this.deleteUpTo(this.delete1h, nowMs - this.policy.oneHourMs) ||
      this.deleteUpTo(this.deleteReceipts, nowMs - this.policy.receiptsMs) ||
      this.deleteUpTo(this.deleteGaps, nowMs - this.policy.gapsMs);

    // Disk pressure is handled only when age retention has no work left, so a
    // backlog is drained oldest-first. The one-hour floor means a tight budget
    // degrades old history rather than the live/recent view. If the floor
    // still exceeds a hard cap, writes reject busy instead of deleting it.
    let effectiveRawCutoff = rawCutoff;
    if (deleted === 0 && this.overBudget()) {
      const floorCutoff = nowMs - HOUR_MS;
      effectiveRawCutoff = Math.max(rawCutoff, floorCutoff);
      deleted = this.deleteUpTo(this.deleteRaw, effectiveRawCutoff);
      if (deleted === 0) deleted = this.deleteUpTo(this.delete1m, floorCutoff);
      if (deleted > 0) {
        this.db.reclaimPages(64);
        try {
          this.db.checkpoint();
        } catch {
          // The next pass retries the bounded checkpoint.
        }
      }
    }
    this.refreshPhysicalState();
    this.state.pressure = this.overBudget();

    this.state.effectiveRawCutoffMs = effectiveRawCutoff;
    this.state.deletedRows += deleted;
    this.state.lastRunAtMs = nowMs;
    this.state.lastRunDurationMs = Math.max(0, Math.round(performance.now() - started));
    return deleted;
  }

  /**
   * Drains expired rows in bounded chunks, yielding between chunks. Stops after
   * `maxChunksPerPass` chunks so the next timer tick can continue; ingestion
   * gets the event loop in between in either case.
   */
  async runPass(): Promise<void> {
    for (let chunk = 0; chunk < this.maxChunksPerPass; chunk += 1) {
      const deleted = this.runChunk();
      if (deleted === 0) return;
      await yieldToEventLoop();
    }
  }

  private deleteUpTo(statement: ReturnType<HubDatabase["db"]["prepare"]>, cutoffMs: number): number {
    const result = statement.run(cutoffMs, this.chunkSize);
    return Number(result.changes);
  }

  private refreshPhysicalState(): void {
    try {
      const physical = this.db.physicalLimit();
      const bytes = this.db.sizeBytes();
      this.state.physicalBytes = bytes;
      this.state.walBytes = this.db.walBytes();
      this.state.physicalLimitBytes = physical?.limitBytes ?? this.physicalLimitBytes;
      this.state.physicalPressure =
        physical?.pressure ?? (this.physicalLimitBytes !== null && bytes > this.physicalLimitBytes);
      this.state.pressure =
        (this.maxDatabaseBytes !== null && bytes > this.maxDatabaseBytes) ||
        this.state.physicalPressure;
    } catch {
      // A closed database keeps its last reported physical values.
    }
  }

  private schedule(): void {
    if (!this.started || this.timer !== null) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.runPass().finally(() => {
        if (this.started) this.schedule();
      });
    }, this.intervalMs);
    // Background maintenance must not keep a hub process alive by itself.
    this.timer.unref?.();
  }
}

function yieldToEventLoop(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

function checkedMs(value: number | undefined, fallback: number, name: string): number {
  const resolved = value ?? fallback;
  if (!Number.isSafeInteger(resolved) || resolved < 0) {
    throw new Error(`retention ${name} must be a nonnegative safe integer`);
  }
  return resolved;
}

function positiveInt(value: number | undefined, fallback: number, name: string): number {
  const resolved = value ?? fallback;
  if (!Number.isSafeInteger(resolved) || resolved <= 0) {
    throw new Error(`${name} must be a positive safe integer`);
  }
  return resolved;
}
