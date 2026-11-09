/**
 * Opt-in durable per-destination outbox for telemetry
 * (`COLLECTOR_QUEUE_MODE=durable`; the collector default is the RAM-only
 * `MemoryDestinationQueue`).
 *
 * One SQLite file per destination (`<dir>/outbox.sqlite`, with the directory
 * derived from the destination URL by `outboxDirFor`). Records are stored as
 * their wire JSON plus a small amount of bookkeeping so a restart can replay
 * everything that was not acknowledged, including records from earlier
 * collector runs (they carry their own runId/sequence). That durability is what
 * costs a write per sample; the RAM queue gives it up and writes nothing.
 *
 * Delivery state has two levels: `pending` and `inflight`. In-flight records
 * are never evicted; on open every in-flight record is turned back into
 * pending because a crash/restart cannot have received the acknowledgement.
 *
 * Bounded retention: records older than `maxAgeMs` or over `maxBytes` are
 * evicted oldest-first, and a shared `OutboxBudget` can force the largest
 * destination to shed bytes. A separate finite `maxDiskBytes` bound covers the
 * actual SQLite main file plus a bounded WAL; when a write cannot fit, old
 * pending telemetry is evicted first and the newest record keeps priority.
 * Evicted telemetry is coalesced into a small `gap` record so the hub can
 * distinguish "no samples captured" from "samples were captured but dropped
 * locally". A gap that cannot currently be written is kept in a bounded
 * in-memory map by reason until a later write succeeds.
 *
 * `synchronous=NORMAL` is intentional: WAL plus NORMAL can lose the tail of
 * the last transaction on an OS crash/power loss. For the collector that is
 * acceptable because a lost tail is exactly a hub-side gap, and the
 * alternative (FULL) costs an fsync per sample. A write failure (disk full)
 * never reaches the sampling loop: it is remembered as an in-memory
 * `write-failure` gap and persisted once writes succeed again.
 */
import { createHash } from 'node:crypto';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type {
  DeliveryStatus,
  GapReason,
  GapRecord,
  MetricsRecord,
  RecordRef,
  TelemetryRecord,
} from '../../src/contracts';
import { OutboxBudget } from './budget';
import type { DestinationQueue, QueueMode } from './queue';

export type OutboxLimits = {
  /** Maximum age of a pending record before it is evicted (default 10 min). */
  maxAgeMs: number;
  /** Maximum logical bytes of stored record JSON (default 64 MiB). */
  maxBytes: number;
  /**
   * Finite physical cap for this outbox's main SQLite file plus WAL. The
   * default 80 MiB gives ~16 MiB of page/index/WAL overhead over the 64 MiB
   * logical bound on a normal 4 KiB-page SQLite file.
   */
  maxDiskBytes: number;
  /** Maximum number of gap records kept per outbox (default 64). */
  maxGapRecords: number;
};

export const DEFAULT_OUTBOX_LIMITS: OutboxLimits = {
  maxAgeMs: 600_000,
  maxBytes: 64 * 1024 * 1024,
  maxDiskBytes: 80 * 1024 * 1024,
  maxGapRecords: 64,
};

/** A finite per-outbox physical cap independent of the logical JSON cap. */
export const DEFAULT_OUTBOX_DISK_BYTES = 80 * 1024 * 1024;
/** Extra room estimated for one row's page/WAL/transaction overhead. */
const WRITE_HEADROOM_BYTES = 16 * 1024;

export type OutboxDiskStatus = {
  bytes: number;
  mainBytes: number;
  walBytes: number;
  logicalBytes: number;
  limitBytes: number;
  overBy: number;
  pressure: boolean;
  walBudgetBytes: number;
  checkpointPages: number;
  pageCount: number;
  maxPageCount: number;
  freePages: number;
  autoVacuum: number;
};

/** Busy timeout for a destination's SQLite file (milliseconds). */
const BUSY_TIMEOUT_MS = 5_000;

/**
 * Stable per-destination outbox directory. The URL is hashed so the directory
 * name is safe for any scheme/port/path and bounded in length; the composition
 * root places the result under `<collector state dir>/outbox/`.
 */
export function outboxDirFor(stateDir: string, url: string): string {
  const digest = createHash('sha256').update(url, 'utf8').digest('hex').slice(0, 16);
  return path.join(stateDir, 'outbox', digest);
}

type Row = {
  id: number;
  run_id: string;
  sequence: number;
  captured_at: string;
  kind: 'metrics' | 'gap';
  state: 'pending' | 'inflight';
  gap_reason: string | null;
  bytes: number;
  json: string;
};

type EvictableRow = Pick<Row, 'id' | 'captured_at' | 'bytes'>;

type MemoryGap = {
  reason: GapReason;
  fromCapturedAt: string;
  toCapturedAt: string;
  droppedRecords: number;
};

export type DestinationOutboxOptions = {
  dir: string;
  limits?: Partial<OutboxLimits>;
  budget?: OutboxBudget;
  /** Allocates sequence numbers for locally created gap records. */
  allocateSequence: () => number;
  identity: { collectorId: string; runId: string; bootId: string };
  now?: () => number;
  /**
   * Test seam. Production always uses `new DatabaseSync(file)`. Tests use it
   * to simulate write failures (disk full) without touching the real FS.
   */
  openDatabase?: (file: string) => DatabaseSync;
};

const SCHEMA = `
  CREATE TABLE IF NOT EXISTS outbox_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    captured_at TEXT NOT NULL,
    kind TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending',
    gap_reason TEXT,
    offered INTEGER NOT NULL DEFAULT 0,
    bytes INTEGER NOT NULL,
    json TEXT NOT NULL
  ) STRICT;
  CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox_records(state, id);
  CREATE INDEX IF NOT EXISTS outbox_gap_idx ON outbox_records(kind, state, gap_reason, id);
  CREATE INDEX IF NOT EXISTS outbox_ref_idx ON outbox_records(run_id, sequence);
`;

function toNumber(value: unknown): number {
  return typeof value === 'bigint' ? Number(value) : typeof value === 'number' ? value : 0;
}

function serializeRecord(record: TelemetryRecord): { json: string; bytes: number } {
  const json = JSON.stringify(record);
  return { json, bytes: Buffer.byteLength(json, 'utf8') };
}

/** Stored rows are already the canonical record shape; nothing is rewritten. */
function parseStoredRecord(json: string): TelemetryRecord {
  return JSON.parse(json) as TelemetryRecord;
}

function fileBytes(file: string): number {
  try {
    return fs.statSync(file).size;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return 0;
    throw error;
  }
}

function validateDiskLimit(value: number): void {
  if (!Number.isSafeInteger(value) || value < 64 * 1024) {
    throw new Error(`maxDiskBytes must be a safe integer of at least 64 KiB, got ${String(value)}`);
  }
}

function asRow(value: unknown): Row | null {
  if (value === null || typeof value !== 'object') return null;
  const r = value as Record<string, unknown>;
  return {
    id: toNumber(r.id),
    run_id: String(r.run_id),
    sequence: toNumber(r.sequence),
    captured_at: String(r.captured_at),
    kind: r.kind === 'gap' ? 'gap' : 'metrics',
    state: r.state === 'inflight' ? 'inflight' : 'pending',
    gap_reason: typeof r.gap_reason === 'string' ? r.gap_reason : null,
    bytes: toNumber(r.bytes),
    json: String(r.json),
  };
}

export class DestinationOutbox implements DestinationQueue {
  readonly queueMode: QueueMode = 'durable';

  private readonly db: DatabaseSync;
  private readonly limits: OutboxLimits;
  private budget: OutboxBudget | null;
  private readonly allocateSequence: () => number;
  private readonly identity: { collectorId: string; runId: string; bootId: string };
  private readonly now: () => number;

  private totalBytes = 0;
  /** Row id of the newest metrics record not yet offered to `takeLive`. */
  private liveId: number | null = null;
  private readonly memoryGaps = new Map<GapReason, MemoryGap>();
  private droppedSinceOpen = 0;
  private lastAckAt: string | null = null;
  private closed = false;
  private readonly file: string;
  private diskLimitBytes: number;
  private walBudgetBytes = 0;
  private checkpointPages = 0;
  private maxPageCount = 0;
  private autoVacuum = 0;
  private physicalPressure = false;

  private constructor(opts: DestinationOutboxOptions, db: DatabaseSync, limits: OutboxLimits) {
    this.db = db;
    this.limits = limits;
    this.diskLimitBytes = limits.maxDiskBytes;
    this.budget = opts.budget ?? null;
    this.allocateSequence = opts.allocateSequence;
    this.identity = opts.identity;
    this.now = opts.now ?? Date.now;
    this.file = path.join(opts.dir, 'outbox.sqlite');
  }

  static open(opts: DestinationOutboxOptions): DestinationOutbox {
    const limits = { ...DEFAULT_OUTBOX_LIMITS, ...opts.limits };
    validateDiskLimit(limits.maxDiskBytes);
    fs.mkdirSync(opts.dir, { recursive: true, mode: 0o700 });
    try {
      fs.chmodSync(opts.dir, 0o700);
    } catch {
      // Best effort: an existing directory owned by someone else will fail
      // here and then fail loudly when the database is opened.
    }
    const file = path.join(opts.dir, 'outbox.sqlite');
    const db =
      opts.openDatabase !== undefined
        ? opts.openDatabase(file)
        : new DatabaseSync(file, { timeout: BUSY_TIMEOUT_MS });
    try {
      const outbox = DestinationOutbox.initialize(opts, db, limits, file);
      return outbox;
    } catch (error) {
      try {
        db.close();
      } catch {
        // Preserve the initialization failure.
      }
      throw error;
    }
  }

  private static initialize(
    opts: DestinationOutboxOptions,
    db: DatabaseSync,
    limits: OutboxLimits,
    file: string,
  ): DestinationOutbox {
    try {
      fs.chmodSync(file, 0o600);
    } catch {
      // Best effort; the containing directory is 0700.
    }
    db.exec(`
      PRAGMA auto_vacuum = INCREMENTAL;
      PRAGMA journal_mode = WAL;
      PRAGMA synchronous = NORMAL;
      PRAGMA busy_timeout = ${BUSY_TIMEOUT_MS};
      PRAGMA foreign_keys = ON;
    `);
    db.exec(SCHEMA);
    // Unfinished outbox layouts belong to the installation reset. Never
    // translate their retained records into a different persistent shape.
    const columns = db.prepare('PRAGMA table_info(outbox_records)').all() as { name: string }[];
    if (!columns.some(column => column.name === 'offered')) {
      throw new Error('outbox must use the canonical schema; reset or reinstall the unfinished installation');
    }
    // Anything that was in flight when the previous process died was never
    // acknowledged (or its ack was lost), so it must be replayed.
    db.prepare("UPDATE outbox_records SET state = 'pending' WHERE state = 'inflight'").run();
    const outbox = new DestinationOutbox(opts, db, limits);
    outbox.configurePhysicalLimit();
    outbox.totalBytes = outbox.readTotalBytes();
    opts.budget?.register(outbox);
    return outbox;
  }

  // -------------------------------------------------------------------------
  // Writes
  // -------------------------------------------------------------------------

  /**
   * Persist a filtered metrics record for this destination. Never throws:
   * a disk failure is counted and remembered as a coalesced `write-failure`
   * gap that is persisted when writes work again.
   */
  append(record: MetricsRecord): void {
    if (this.closed) return;
    try {
      this.appendAttempt(record);
    } catch {
      // No storage failure may cross the sampling boundary.
      try {
        this.noteDropped('write-failure', record.capturedAt);
      } catch {
        // The in-memory fallback itself only allocates a small map entry.
      }
    }
  }

  private appendAttempt(record: MetricsRecord): void {
    this.flushMemoryGaps();
    const prepared = serializeRecord(record);
    if (!this.makeRoomFor(prepared.bytes)) {
      this.noteDropped('disk-budget', record.capturedAt);
      return;
    }
    try {
      const id = this.insertRecord(record, prepared);
      if (record.kind === 'metrics') this.liveId = id;
      this.afterWrite();
    } catch {
      // A hard page cap or a real disk error can still surface here. Make one
      // bounded reclaim attempt for this record, then keep the dropped interval
      // in memory.
      this.relievePhysicalPressure(prepared.bytes + WRITE_HEADROOM_BYTES);
      try {
        const id = this.insertRecord(record, prepared);
        if (record.kind === 'metrics') this.liveId = id;
        this.afterWrite();
      } catch {
        this.noteDropped('write-failure', record.capturedAt);
      }
    }
  }

  /** Reapply current local privacy policy before any retained record can leave. */
  filterPending(transform: (record: MetricsRecord) => MetricsRecord): void {
    const rows = this.db.prepare("SELECT id,json,bytes FROM outbox_records WHERE kind='metrics'").all() as { id: number; json: string; bytes: number }[];
    const update = this.db.prepare('UPDATE outbox_records SET json=?,bytes=? WHERE id=?');
    this.db.exec('BEGIN IMMEDIATE');
    let delta = 0;
    try {
      for (const row of rows) {
        const json = JSON.stringify(transform(parseStoredRecord(row.json) as MetricsRecord));
        const bytes = Buffer.byteLength(json);
        update.run(json, bytes, row.id); delta += bytes - row.bytes;
      }
      this.db.exec('COMMIT'); this.totalBytes += delta;
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }

  // -------------------------------------------------------------------------
  // Reads
  // -------------------------------------------------------------------------

  /**
   * Newest metrics record appended since the last `takeLive`, excluded from
   * replay so the live path can send it immediately.
   */
  takeLive(): TelemetryRecord | null {
    if (this.closed || this.liveId === null) return null;
    const id = this.liveId;
    this.liveId = null;
    const row = asRow(
      this.db.prepare("SELECT * FROM outbox_records WHERE id = ? AND state = 'pending' AND kind = 'metrics'").get(id),
    );
    if (row === null) return null;
    return this.parseRow(row);
  }

  /**
   * Oldest pending records (including gap records), oldest first, excluding
   * in-flight rows and the current live record. The batch is bounded by both
   * record count and stored JSON bytes; at least one record is returned when
   * anything is available so a single large record cannot stall the queue.
   */
  takeReplay(maxRecords: number, maxBytes: number): TelemetryRecord[] {
    if (this.closed || !Number.isFinite(maxRecords) || maxRecords < 1) return [];
    const rows = this.db
      .prepare("SELECT * FROM outbox_records WHERE state = 'pending' AND id != ? ORDER BY id ASC LIMIT ?")
      .all(this.liveId ?? -1, Math.floor(maxRecords))
      .map(asRow)
      .filter((row): row is Row => row !== null);
    const records: TelemetryRecord[] = [];
    let bytes = 0;
    for (const row of rows) {
      if (records.length >= maxRecords) break;
      if (row.id === this.liveId) continue;
      if (records.length > 0 && bytes + row.bytes > maxBytes) break;
      const record = this.parseRow(row);
      if (record === null) continue;
      records.push(record);
      bytes += row.bytes;
    }
    return records;
  }

  // -------------------------------------------------------------------------
  // Delivery state
  // -------------------------------------------------------------------------

  markInFlight(refs: readonly RecordRef[]): void {
    if (this.closed) return;
    try {
      const stmt = this.db.prepare(
        "UPDATE outbox_records SET state = 'inflight', offered = 1 WHERE run_id = ? AND sequence = ? AND state = 'pending'",
      );
      for (const ref of refs) stmt.run(ref.runId, ref.sequence);
    } catch {
      // A full disk must not crash the delivery loop. Rows that were not
      // marked remain pending; a later replay/ack still deduplicates safely.
    }
  }

  release(refs: readonly RecordRef[]): void {
    if (this.closed) return;
    try {
      const stmt = this.db.prepare(
        "UPDATE outbox_records SET state = 'pending' WHERE run_id = ? AND sequence = ? AND state = 'inflight'",
      );
      for (const ref of refs) stmt.run(ref.runId, ref.sequence);
    } catch {
      // Best effort under a full disk; restart recovery also returns rows to pending.
    }
  }

  releaseAll(): void {
    if (this.closed) return;
    try {
      this.db.prepare("UPDATE outbox_records SET state = 'pending' WHERE state = 'inflight'").run();
    } catch {
      // Restart/open recovery is the fallback.
    }
  }

  /** Accepted and rejected records are both removed from disk. Unknown refs are ignored. */
  acknowledge(accepted: readonly RecordRef[], rejected: readonly RecordRef[]): void {
    if (this.closed) return;
    if (accepted.length === 0 && rejected.length === 0) return;
    try {
      for (const ref of [...accepted, ...rejected]) {
        const row = this.db
          .prepare('SELECT bytes FROM outbox_records WHERE run_id = ? AND sequence = ?')
          .get(ref.runId, ref.sequence);
        this.db
          .prepare('DELETE FROM outbox_records WHERE run_id = ? AND sequence = ?')
          .run(ref.runId, ref.sequence);
        if (row !== undefined) this.totalBytes -= toNumber((row as Record<string, unknown>).bytes);
      }
      this.totalBytes = Math.max(0, this.totalBytes);
      this.lastAckAt = this.isoNow();
    } catch {
      // The hub already has the data; keeping the rows means a later replay is
      // deduplicated rather than an ack crashing the session.
    }
  }

  // -------------------------------------------------------------------------
  // Retention
  // -------------------------------------------------------------------------

  /**
   * Apply age, per-destination byte and shared-budget caps, then collapse gap
   * records to `maxGapRecords`. Only pending metrics records are evictable:
   * in-flight records belong to an outstanding batch and gap records are the
   * only surviving evidence that data was dropped.
   */
  enforceLimits(): void {
    if (this.closed) return;
    this.flushMemoryGaps();
    this.evictExpired();
    this.evictForBytes();
    this.relievePhysicalPressure();
    this.enforceBudget();
    this.enforceGapCap();
    this.afterWrite();
  }

  status(): DeliveryStatus {
    if (this.closed) {
      return {
        pendingRecords: 0,
        pendingBytes: 0,
        oldestPendingCapturedAt: null,
        inFlightRecords: 0,
        droppedRecords: this.droppedSinceOpen,
        replaying: false,
        lastAckAt: this.lastAckAt,
        queueMode: this.queueMode,
      };
    }
    const pending = this.db
      .prepare("SELECT captured_at, bytes FROM outbox_records WHERE state = 'pending'")
      .all() as Array<Record<string, unknown>>;
    let pendingBytes = 0;
    let oldest: string | null = null;
    for (const row of pending) {
      pendingBytes += toNumber(row.bytes);
      const capturedAt = String(row.captured_at);
      if (oldest === null || Date.parse(capturedAt) < Date.parse(oldest)) oldest = capturedAt;
    }
    const inFlight = toNumber(
      (this.db.prepare("SELECT COUNT(*) AS n FROM outbox_records WHERE state = 'inflight'").get() as
        | Record<string, unknown>
        | undefined)?.n,
    );
    return {
      pendingRecords: pending.length,
      pendingBytes,
      oldestPendingCapturedAt: oldest,
      inFlightRecords: inFlight,
      droppedRecords: this.droppedSinceOpen,
      replaying: pending.length > 0,
      lastAckAt: this.lastAckAt,
      queueMode: this.queueMode,
    };
  }

  /** Logical bytes of stored record JSON (pending plus in-flight). */
  bytes(): number {
    return this.totalBytes;
  }

  /** Actual main SQLite file plus WAL bytes. */
  diskBytes(): number {
    return this.mainBytes() + this.walBytes();
  }

  diskStatus(): OutboxDiskStatus {
    const mainBytes = this.mainBytes();
    const walBytes = this.walBytes();
    const bytes = mainBytes + walBytes;
    return {
      bytes,
      mainBytes,
      walBytes,
      logicalBytes: this.totalBytes,
      limitBytes: this.diskLimitBytes,
      overBy: Math.max(0, bytes - this.diskLimitBytes),
      pressure: this.physicalPressure || bytes > this.diskLimitBytes,
      walBudgetBytes: this.walBudgetBytes,
      checkpointPages: this.checkpointPages,
      pageCount: this.pragmaNumber('page_count', 0),
      maxPageCount: this.maxPageCount,
      freePages: this.pragmaNumber('freelist_count', 0),
      autoVacuum: this.autoVacuum,
    };
  }

  /** Truncate the bounded WAL after a checkpoint. Best effort. */
  checkpoint(): void {
    if (this.closed) return;
    try {
      this.db.exec('PRAGMA wal_checkpoint(TRUNCATE)');
    } catch {
      // Another handle or a busy checkpoint is not a fatal condition.
    }
  }

  /**
   * Registration hook used by OutboxBudget.register/unregister. Callers use
   * the budget API; this exists so an outbox opened before its budget can
   * still participate in shared-cap enforcement.
   */
  attachBudget(budget: OutboxBudget | null): void {
    this.budget = budget;
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.flushMemoryGaps();
    this.budget?.unregister(this);
    try {
      this.db.close();
    } catch {
      // Already closed by the owner or the process is exiting.
    }
  }

  // -------------------------------------------------------------------------
  // Internals
  // -------------------------------------------------------------------------

  private isoNow(): string {
    return new Date(this.now()).toISOString();
  }

  private readTotalBytes(): number {
    const row = this.db.prepare('SELECT COALESCE(SUM(bytes), 0) AS total FROM outbox_records').get() as
      | Record<string, unknown>
      | undefined;
    return toNumber(row?.total);
  }

  private configurePhysicalLimit(): void {
    const pageSize = this.pragmaNumber('page_size', 4096);
    const walBudgetBytes = Math.max(64 * 1024, Math.min(4 * 1024 * 1024, Math.floor(this.diskLimitBytes / 32)));
    const mainBudgetBytes = Math.max(pageSize, this.diskLimitBytes - walBudgetBytes);
    const requestedPages = Math.max(1, Math.floor(mainBudgetBytes / pageSize));
    const checkpointPages = Math.max(16, Math.min(1000, Math.floor(walBudgetBytes / pageSize)));
    this.walBudgetBytes = walBudgetBytes;
    this.checkpointPages = checkpointPages;
    this.db.exec(`PRAGMA journal_size_limit = ${walBudgetBytes}`);
    this.db.exec(`PRAGMA wal_autocheckpoint = ${checkpointPages}`);
    let maxPageCount = requestedPages;
    try {
      const row = this.db.prepare(`PRAGMA max_page_count = ${requestedPages}`).get() as
        | { max_page_count?: number | bigint }
        | undefined;
      maxPageCount = Number(row?.max_page_count ?? requestedPages);
    } catch {
      try {
        this.db.exec(`PRAGMA max_page_count = ${requestedPages}`);
      } catch {
        // The measurable pressure state below remains honest.
      }
      maxPageCount = this.pragmaNumber('max_page_count', requestedPages);
    }
    this.maxPageCount = maxPageCount;
    this.autoVacuum = this.pragmaNumber('auto_vacuum', 0);
    this.checkpoint();
    this.physicalPressure = !this.hasPhysicalHeadroom(0);
  }

  private mainBytes(): number {
    return fileBytes(this.file);
  }

  private walBytes(): number {
    return fileBytes(`${this.file}-wal`);
  }

  private pragmaNumber(name: string, fallback: number): number {
    try {
      const row = this.db.prepare(`PRAGMA ${name}`).get() as Record<string, unknown> | undefined;
      const value = row?.[name];
      const number = typeof value === 'bigint' ? Number(value) : typeof value === 'number' ? value : Number(value);
      return Number.isFinite(number) ? number : fallback;
    } catch {
      return fallback;
    }
  }

  private canReclaimPhysical(): boolean {
    return this.autoVacuum === 2 || this.autoVacuum === 1;
  }

  private hasPhysicalHeadroom(requiredBytes: number): boolean {
    if (this.diskBytes() + requiredBytes > this.diskLimitBytes) return false;
    const pageSize = this.pragmaNumber('page_size', 4096);
    const neededPages = Math.max(1, Math.ceil(requiredBytes / Math.max(1, pageSize)));
    const pageCount = this.pragmaNumber('page_count', 0);
    const freePages = this.pragmaNumber('freelist_count', 0);
    return pageCount < this.maxPageCount || freePages >= neededPages;
  }

  private reclaimPages(maxPages = 64): number {
    if (this.autoVacuum !== 2) return 0;
    const pages = Math.max(1, Math.min(100_000, Math.floor(maxPages)));
    const before = this.pragmaNumber('freelist_count', 0);
    try {
      this.db.exec(`PRAGMA incremental_vacuum(${pages})`);
    } catch {
      return 0;
    }
    return Math.max(0, before - this.pragmaNumber('freelist_count', 0));
  }

  /**
   * Make enough physical room for one incoming record. Oldest pending metrics
   * are evicted with a disk-budget gap; in-flight and gap rows are never
   * touched. If reclaim is impossible, report pressure and let append drop the
   * current record into the memory gap rather than evicting everything.
   */
  private makeRoomFor(recordBytes: number): boolean {
    if (this.budget !== null && this.budget.diskOverBy() > 0) {
      const resolved = this.enforcePhysicalBudget();
      if (!resolved) {
        this.physicalPressure = !this.hasPhysicalHeadroom(0);
        return false;
      }
    }
    const required = recordBytes + WRITE_HEADROOM_BYTES;
    const initialGlobalRoom =
      this.budget === null ||
      this.budget.diskBytes() + required <= this.budget.maxTotalDiskBytes;
    if (this.hasPhysicalHeadroom(required) && !this.physicalPressure && initialGlobalRoom) {
      return true;
    }
    if (!this.canReclaimPhysical()) {
      this.physicalPressure = !this.hasPhysicalHeadroom(0);
      return this.hasPhysicalHeadroom(required) && initialGlobalRoom;
    }
    this.evictForPhysicalHeadroom(required);
    this.physicalPressure = !this.hasPhysicalHeadroom(0);
    if (this.budget !== null && this.budget.diskOverBy() > 0) {
      this.enforcePhysicalBudget();
    }
    const ownRoom = this.hasPhysicalHeadroom(required);
    const globalRoom =
      this.budget === null ||
      this.budget.diskBytes() + required <= this.budget.maxTotalDiskBytes;
    return ownRoom && globalRoom;
  }

  private afterWrite(): void {
    try {
      if (this.walBytes() > this.walBudgetBytes || this.diskBytes() > this.diskLimitBytes) {
        this.checkpoint();
      }
      if (this.diskBytes() > this.diskLimitBytes) {
        this.reclaimPages(64);
      }
      this.physicalPressure = !this.hasPhysicalHeadroom(0);
    } catch {
      // Measurement/checkpoint failure must not undo a committed insert.
    }
  }

  /** Attempt to bring this outbox and the shared physical budget under cap. */
  private relievePhysicalPressure(requiredBytes = 0): void {
    if (this.canReclaimPhysical() && !this.hasPhysicalHeadroom(requiredBytes)) {
      this.evictForPhysicalHeadroom(requiredBytes);
    }
    this.physicalPressure = !this.hasPhysicalHeadroom(0);
    if (this.budget !== null && this.budget.diskOverBy() > 0) {
      this.enforcePhysicalBudget();
    }
  }

  private evictForPhysicalHeadroom(requiredBytes: number): void {
    let guard = 0;
    while (!this.hasPhysicalHeadroom(requiredBytes) && guard < 64) {
      guard += 1;
      const rows = this.pendingMetrics(64);
      if (rows.length === 0) return;
      const overflow = Math.max(1, this.diskBytes() + requiredBytes - this.diskLimitBytes);
      const average = rows.reduce((sum, row) => sum + row.bytes, 0) / rows.length;
      const take = Math.min(rows.length, Math.max(1, Math.ceil(overflow / Math.max(1, average))));
      this.evictRows(rows.slice(0, take), 'disk-budget');
      this.checkpoint();
      this.reclaimPages(64);
    }
  }

  private enforcePhysicalBudget(): boolean {
    const budget = this.budget;
    if (budget === null) return true;
    if (budget.diskOverBy() <= 0) return true;
    budget.checkpointAll();
    const tried = new Set<DestinationQueue>();
    let guard = 0;
    while (budget.diskOverBy() > 0 && guard < 256) {
      guard += 1;
      const candidate = budget.largestByDisk(tried);
      if (candidate === null) break;
      // Only a durable outbox has pages to reclaim; a memory queue reports no
      // physical bytes and is never selected here.
      if (!(candidate instanceof DestinationOutbox)) {
        tried.add(candidate);
        continue;
      }
      const target = candidate;
      if (!target.canReclaimPhysical()) {
        tried.add(target);
        continue;
      }
      const evicted = target.evictForBudget('disk-budget');
      target.checkpoint();
      target.reclaimPages(128);
      if (evicted === 0) {
        if (target.reclaimPages(128) === 0) tried.add(target);
      }
    }
    return budget.diskOverBy() <= 0;
  }

  private insertRecord(record: TelemetryRecord, prepared?: { json: string; bytes: number }): number {
    const { json, bytes } = prepared ?? serializeRecord(record);
    const reason = record.kind === 'gap' ? record.payload.reason : null;
    const result = this.db
      .prepare(
        `INSERT INTO outbox_records (run_id, sequence, captured_at, kind, state, gap_reason, bytes, json)
         VALUES (?, ?, ?, ?, 'pending', ?, ?, ?)`,
      )
      .run(record.runId, record.sequence, record.capturedAt, record.kind, reason, bytes, json);
    this.totalBytes += bytes;
    return toNumber(result.lastInsertRowid);
  }

  private parseRow(row: Row): TelemetryRecord | null {
    try {
      return parseStoredRecord(row.json);
    } catch {
      // Corrupt row: drop it rather than blocking every later record.
      this.db.prepare('DELETE FROM outbox_records WHERE id = ?').run(row.id);
      this.totalBytes = Math.max(0, this.totalBytes - row.bytes);
      this.droppedSinceOpen += 1;
      return null;
    }
  }

  private noteDropped(reason: GapReason, capturedAt: string): void {
    this.droppedSinceOpen += 1;
    this.rememberGap(reason, capturedAt, capturedAt, 1);
  }

  /** Bounded in-memory fallback when a gap cannot be written yet. */
  private rememberGap(reason: GapReason, fromCapturedAt: string, toCapturedAt: string, droppedRecords: number): void {
    const existing = this.memoryGaps.get(reason);
    if (existing === undefined) {
      this.memoryGaps.set(reason, {
        reason,
        fromCapturedAt,
        toCapturedAt,
        droppedRecords,
      });
      return;
    }
    if (Date.parse(fromCapturedAt) < Date.parse(existing.fromCapturedAt)) {
      existing.fromCapturedAt = fromCapturedAt;
    }
    if (Date.parse(toCapturedAt) > Date.parse(existing.toCapturedAt)) {
      existing.toCapturedAt = toCapturedAt;
    }
    existing.droppedRecords += droppedRecords;
  }

  /** Try to persist every in-memory gap; retain any that still cannot write. */
  private flushMemoryGaps(): void {
    if (this.memoryGaps.size === 0) return;
    for (const [reason, gap] of [...this.memoryGaps.entries()]) {
      try {
        this.writeGap(gap.reason, gap.fromCapturedAt, gap.toCapturedAt, gap.droppedRecords);
        if (this.memoryGaps.get(reason) === gap) this.memoryGaps.delete(reason);
      } catch {
        // Still under pressure; the bounded in-memory interval/count survives.
      }
    }
    try {
      this.enforceGapCap();
    } catch {
      // A full disk cannot merge gaps; keep the rows and memory fallback.
    }
  }

  /**
   * Persist (or coalesce into) a gap record. Coalescing extends the newest
   * pending gap of the same reason, so per-reason history stays one record
   * while gaps are in the queue.
   */
  private writeGap(reason: GapReason, fromCapturedAt: string, toCapturedAt: string, droppedRecords: number): void {
    const existing = asRow(
      this.db
        .prepare(
          "SELECT * FROM outbox_records WHERE kind = 'gap' AND state = 'pending' AND offered = 0 AND run_id = ? AND gap_reason = ? ORDER BY id DESC LIMIT 1",
        )
        .get(this.identity.runId, reason),
    );
    if (existing !== null) {
      const record = parseStoredRecord(existing.json) as GapRecord;
      record.payload.fromCapturedAt =
        Date.parse(fromCapturedAt) < Date.parse(record.payload.fromCapturedAt)
          ? fromCapturedAt
          : record.payload.fromCapturedAt;
      record.payload.toCapturedAt =
        Date.parse(toCapturedAt) > Date.parse(record.payload.toCapturedAt)
          ? toCapturedAt
          : record.payload.toCapturedAt;
      record.payload.droppedRecords += droppedRecords;
      const json = JSON.stringify(record);
      const bytes = Buffer.byteLength(json, 'utf8');
      this.db.prepare('UPDATE outbox_records SET json = ?, bytes = ? WHERE id = ?').run(json, bytes, existing.id);
      this.totalBytes += bytes - existing.bytes;
      return;
    }
    const gap: GapRecord = {
      collectorId: this.identity.collectorId,
      runId: this.identity.runId,
      bootId: this.identity.bootId,
      sequence: this.allocateSequence(),
      capturedAt: this.isoNow(),
      kind: 'gap',
      payload: { fromCapturedAt, toCapturedAt, droppedRecords, reason },
    };
    this.insertRecord(gap);
  }

  private pendingMetrics(limit: number, excludeId?: number): EvictableRow[] {
    const rows = this.db
      .prepare(
        "SELECT id, captured_at, bytes FROM outbox_records WHERE state = 'pending' AND kind = 'metrics' " +
          (excludeId === undefined ? '' : 'AND id != ? ') +
          'ORDER BY id ASC LIMIT ?',
      )
      .all(...(excludeId === undefined ? [limit] : [excludeId, limit]))
      .map((value) => {
        const r = value as Record<string, unknown>;
        return { id: toNumber(r.id), captured_at: String(r.captured_at), bytes: toNumber(r.bytes) };
      });
    return rows;
  }

  /** Delete rows and fold them into one coalesced gap record. Returns rows deleted. */
  private evictRows(rows: readonly EvictableRow[], reason: GapReason): number {
    if (rows.length === 0) return 0;
    let from = rows[0]!.captured_at;
    let to = rows[0]!.captured_at;
    let bytes = 0;
    const remove = this.db.prepare('DELETE FROM outbox_records WHERE id = ?');
    for (const row of rows) {
      remove.run(row.id);
      bytes += row.bytes;
      if (Date.parse(row.captured_at) < Date.parse(from)) from = row.captured_at;
      if (Date.parse(row.captured_at) > Date.parse(to)) to = row.captured_at;
    }
    this.totalBytes = Math.max(0, this.totalBytes - bytes);
    this.droppedSinceOpen += rows.length;
    try {
      this.writeGap(reason, from, to, rows.length);
    } catch {
      // The gap could not be persisted (full disk/page cap). Keep the exact
      // interval and count in bounded memory for the next successful write.
      this.rememberGap(reason, from, to, rows.length);
    }
    return rows.length;
  }

  private evictExpired(): void {
    const cutoff = this.now() - this.limits.maxAgeMs;
    const rows = this.pendingMetrics(Number.MAX_SAFE_INTEGER).filter(
      (row) => Date.parse(row.captured_at) < cutoff,
    );
    if (rows.length > 0) this.evictRows(rows, 'outbox-age');
  }

  private evictForBytes(): void {
    let guard = 0;
    while (this.totalBytes > this.limits.maxBytes && guard < 10_000) {
      guard += 1;
      const rows = this.pendingMetrics(128);
      if (rows.length === 0) return;
      let remaining = this.totalBytes;
      let take = 0;
      for (const row of rows) {
        if (remaining <= this.limits.maxBytes) break;
        remaining -= row.bytes;
        take += 1;
      }
      if (take === 0) take = 1;
      this.evictRows(rows.slice(0, take), 'outbox-bytes');
    }
  }

  private enforceBudget(): void {
    const budget = this.budget;
    if (budget === null) return;
    const tried = new Set<DestinationQueue>();
    let guard = 0;
    while (budget.overBy() > 0 && guard < 10_000) {
      guard += 1;
      const target = budget.largest(tried);
      if (target === null) break;
      const evicted = target.evictForBudget('disk-budget');
      if (evicted === 0) tried.add(target);
    }
    this.enforcePhysicalBudget();
  }

  /** Internal: the budget's victim sheds its oldest pending metrics records. */
  evictForBudget(reason: GapReason): number {
    const rows = this.pendingMetrics(16);
    if (rows.length === 0) return 0;
    const evicted = this.evictRows(rows, reason);
    this.physicalPressure = !this.hasPhysicalHeadroom(0);
    return evicted;
  }

  private gapCount(): number {
    return toNumber(
      (this.db.prepare("SELECT COUNT(*) AS n FROM outbox_records WHERE kind = 'gap'").get() as
        | Record<string, unknown>
        | undefined)?.n,
    );
  }

  /** Hard ceiling on retained gap records: merge the oldest pending gaps. */
  private enforceGapCap(): void {
    let guard = 0;
    while (this.gapCount() > this.limits.maxGapRecords && guard < 10_000) {
      guard += 1;
      const gaps = this.db
        .prepare("SELECT * FROM outbox_records WHERE kind = 'gap' AND state = 'pending' ORDER BY id ASC")
        .all()
        .map(asRow)
        .filter((row): row is Row => row !== null);
      if (gaps.length < 2) return;
      const older = gaps[0]!;
      const newer = gaps[1]!;
      const olderRecord = parseStoredRecord(older.json) as GapRecord;
      const newerRecord = parseStoredRecord(newer.json) as GapRecord;
      newerRecord.payload.fromCapturedAt =
        Date.parse(olderRecord.payload.fromCapturedAt) < Date.parse(newerRecord.payload.fromCapturedAt)
          ? olderRecord.payload.fromCapturedAt
          : newerRecord.payload.fromCapturedAt;
      newerRecord.payload.toCapturedAt =
        Date.parse(olderRecord.payload.toCapturedAt) > Date.parse(newerRecord.payload.toCapturedAt)
          ? olderRecord.payload.toCapturedAt
          : newerRecord.payload.toCapturedAt;
      newerRecord.payload.droppedRecords += olderRecord.payload.droppedRecords;
      // Merging may replace previously offered records, so allocate a new key
      // rather than changing a payload the hub could already have committed.
      newerRecord.collectorId = this.identity.collectorId;
      newerRecord.runId = this.identity.runId;
      newerRecord.bootId = this.identity.bootId;
      newerRecord.sequence = this.allocateSequence();
      newerRecord.capturedAt = this.isoNow();
      this.insertRecord(newerRecord);
      this.db.prepare('DELETE FROM outbox_records WHERE id = ?').run(newer.id);
      this.db.prepare('DELETE FROM outbox_records WHERE id = ?').run(older.id);
      this.totalBytes -= newer.bytes + older.bytes;
    }
  }
}

