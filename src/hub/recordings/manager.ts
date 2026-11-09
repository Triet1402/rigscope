/**
 * Explicit user recordings.
 *
 * A recording is a bounded, private JSON-lines file containing compact history
 * buckets. It deliberately reuses the hub's active HistorySource, so a memory
 * mode recording can include the lead-up already in RAM, and a persistent mode
 * recording uses the same summary tiers as charts.
 *
 * Files live only in the configured private directory (default beside the hub
 * database). The browser never supplies a path: it starts a recording by
 * machine/metric/resolution selection and later downloads it by opaque id.
 * Metadata survives restart; a recording that was active during a restart is
 * marked interrupted and its partial file remains downloadable.
 */

import {
  closeSync,
  mkdirSync,
  openSync,
  statSync,
  unlinkSync,
  writeSync,
} from "node:fs";
import { randomUUID } from "node:crypto";
import { basename, join, resolve } from "node:path";
import type { StatementSync } from "node:sqlite";
import type { HubDatabase } from "../storage/db";
import type { HistorySource } from "../storage/history";
import { METRICS } from "../storage/series";
import type { HistoryGap, HistoryQuery, HistoryResult } from "../storage/history-query";

const ID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const RESOLUTIONS: Readonly<Record<RecordingResolution, number>> = {
  "1s": 1_000,
  "10s": 10_000,
  "1m": 60_000,
  "5m": 300_000,
  "1h": 3_600_000,
};
const MAX_METRICS = Object.keys(METRICS).length;
const MAX_MACHINES = 64;
const MAX_LIST = 200;
const TICK_CAP_MS = 5_000;
/**
 * Largest complete run asked for in one query. The history API caps
 * `maxPoints` at 2000, so staying at that boundary keeps the query grid equal
 * to the recording grid for every supported resolution.
 */
const CAPTURE_CHUNK_BUCKETS = 2_000;

export type RecordingResolution = "1s" | "10s" | "1m" | "5m" | "1h";

export type RecordingInput = {
  machines: string[];
  metrics: string[];
  resolution: RecordingResolution;
  durationMs?: number;
  maxBytes?: number;
  includeBacklog?: boolean;
};

export type RecordingStatus = "recording" | "stopped" | "completed" | "interrupted" | "failed";

export type RecordingRecord = {
  id: string;
  createdBy: string;
  status: RecordingStatus;
  machines: string[];
  metrics: string[];
  resolution: RecordingResolution;
  resolutionMs: number;
  createdAt: string;
  startedAt: string;
  stoppedAt: string | null;
  deadline: string;
  maxBytes: number;
  bytesWritten: number;
  includeBacklog: boolean;
  reason: string | null;
  error: string | null;
  downloadPath: string;
};

export type RecordingManagerOptions = {
  database: HubDatabase;
  history: HistorySource;
  directory: string;
  maxDurationMs: number;
  maxBytes: number;
  backlogMs: number;
  maxActive: number;
  now?: () => number;
};

type Row = {
  id: string;
  created_by: string;
  status: string;
  machines_json: string;
  metrics_json: string;
  resolution_ms: number | bigint;
  created_at_ms: number | bigint;
  started_at_ms: number | bigint;
  stopped_at_ms: number | bigint | null;
  deadline_ms: number | bigint;
  max_bytes: number | bigint;
  bytes_written: number | bigint;
  file_name: string;
  include_backlog: number | bigint;
  reason: string | null;
  error: string | null;
};

type MachineCursor = {
  /** Absolute, resolution-aligned start of the next bucket to capture. */
  nextBucketMs: number;
  /**
   * Start of the last bucket written for this machine. Kept apart from
   * `nextBucketMs` so the initial interval is never mistaken for an
   * already-written bucket: the first capture may legitimately cover the exact
   * recording start (and, for persistent summaries, the bucket that contains
   * it) even though its timestamp equals the cursor.
   */
  lastWrittenBucketMs: number | null;
};

type ActiveRecording = {
  row: Row;
  fd: number;
  file: string;
  input: {
    machines: string[];
    metrics: string[];
    resolutionMs: number;
    includeBacklog: boolean;
  };
  startedAtMs: number;
  /** Earliest timestamp this recording is allowed to include. */
  captureStartMs: number;
  /**
   * Extra wait before a closed bucket is written, so a persistent backend can
   * commit queued samples that belong to its last interval. Zero in RAM mode.
   */
  graceMs: number;
  deadlineMs: number;
  maxBytes: number;
  bytesWritten: number;
  cursors: Map<string, MachineCursor>;
  timer: NodeJS.Timeout | null;
  stopping: boolean;
  /** True while finish() is writing the final tail. */
  finishing: boolean;
};

export class RecordingError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "RecordingError";
    this.status = status;
  }
}

export class RecordingManager {
  private readonly database: HubDatabase;
  private readonly history: HistorySource;
  private readonly directory: string;
  private readonly maxDurationMs: number;
  private readonly maxBytes: number;
  private readonly backlogMs: number;
  private readonly maxActive: number;
  private readonly now: () => number;
  private readonly active = new Map<string, ActiveRecording>();
  private readonly insert: StatementSync;
  private readonly update: StatementSync;
  private readonly selectById: StatementSync;
  private readonly selectRecent: StatementSync;
  private readonly deleteRow: StatementSync;
  private readonly updateAllActive: StatementSync;
  private closed = false;

  constructor(options: RecordingManagerOptions) {
    this.database = options.database;
    this.history = options.history;
    this.directory = resolve(options.directory);
    this.maxDurationMs = positive(options.maxDurationMs, "maxDurationMs");
    this.maxBytes = positive(options.maxBytes, "maxBytes");
    this.backlogMs = nonnegative(options.backlogMs, "backlogMs");
    this.maxActive = positive(options.maxActive, "maxActive");
    this.now = options.now ?? Date.now;
    this.ensureDirectory();
    const db = options.database.db;
    this.insert = db.prepare(
      "INSERT INTO recordings (id, created_by, status, machines_json, metrics_json, resolution_ms, " +
        "created_at_ms, started_at_ms, stopped_at_ms, deadline_ms, max_bytes, bytes_written, " +
        "file_name, include_backlog, reason, error) " +
        "VALUES (?, ?, 'recording', ?, ?, ?, ?, ?, NULL, ?, ?, 0, ?, ?, NULL, NULL)",
    );
    this.update = db.prepare(
      "UPDATE recordings SET status = ?, stopped_at_ms = ?, bytes_written = ?, reason = ?, error = ? WHERE id = ?",
    );
    this.selectById = db.prepare("SELECT * FROM recordings WHERE id = ?");
    this.selectRecent = db.prepare("SELECT * FROM recordings ORDER BY created_at_ms DESC LIMIT ?");
    this.deleteRow = db.prepare("DELETE FROM recordings WHERE id = ?");
    this.updateAllActive = db.prepare(
      "UPDATE recordings SET status = 'interrupted', stopped_at_ms = ?, reason = 'hub-restart' WHERE status = 'recording'",
    );
    this.recoverInterrupted();
  }

  start(createdBy: string, input: RecordingInput): RecordingRecord {
    if (this.closed) throw new RecordingError(503, "recordings are shutting down");
    if (this.active.size >= this.maxActive) {
      throw new RecordingError(429, `at most ${this.maxActive} recordings can be active at once`);
    }
    const validated = this.validateInput(input);
    const id = randomUUID();
    const createdAt = this.now();
    const deadlineMs = createdAt + validated.durationMs;
    const fileName = `${id}.jsonl`;
    const file = join(this.directory, fileName);
    let fd: number;
    try {
      fd = openSync(file, "wx", 0o600);
    } catch {
      throw new RecordingError(500, "could not create the recording file");
    }
    try {
      this.insert.run(
        id,
        createdBy,
        JSON.stringify(validated.machines),
        JSON.stringify(validated.metrics),
        validated.resolutionMs,
        createdAt,
        createdAt,
        deadlineMs,
        validated.maxBytes,
        fileName,
        validated.includeBacklog ? 1 : 0,
      );
      const row = this.selectById.get(id) as Row | undefined;
      if (row === undefined) throw new Error("recording row was not created");
      const captureStartMs =
        validated.includeBacklog && this.backlogMs > 0 ? createdAt - this.backlogMs : createdAt;
      let graceMs = 0;
      try {
        const persistence = this.history.historyStatus().persistence;
        if (persistence !== null) {
          // The durable store acknowledges after COMMIT but may batch a whole
          // flush interval. Waiting one interval plus a small margin keeps a
          // bucket from closing before its last queued samples are visible.
          graceMs = Math.min(60_000, persistence.flushIntervalMs + 1_000);
        }
      } catch {
        graceMs = 0;
      }
      const cursors = new Map<string, MachineCursor>();
      for (const machineId of validated.machines) {
        cursors.set(machineId, {
          nextBucketMs: Math.floor(captureStartMs / validated.resolutionMs) * validated.resolutionMs,
          lastWrittenBucketMs: null,
        });
      }
      const active: ActiveRecording = {
        row,
        fd,
        file,
        input: {
          machines: validated.machines,
          metrics: validated.metrics,
          resolutionMs: validated.resolutionMs,
          includeBacklog: validated.includeBacklog,
        },
        startedAtMs: createdAt,
        captureStartMs,
        graceMs,
        deadlineMs,
        maxBytes: validated.maxBytes,
        bytesWritten: 0,
        cursors,
        timer: null,
        stopping: false,
        finishing: false,
      };
      this.active.set(id, active);
      this.writeLine(active, {
        type: "manifest",
        version: 1,
        id,
        createdBy,
        machines: validated.machines,
        metrics: validated.metrics,
        resolution: validated.resolution,
        resolutionMs: validated.resolutionMs,
        startedAt: new Date(createdAt).toISOString(),
        deadline: new Date(deadlineMs).toISOString(),
        maxBytes: validated.maxBytes,
        includeBacklog: validated.includeBacklog,
        historyMode: this.history.historyMode,
      });
      if (validated.includeBacklog && this.backlogMs > 0) {
        this.capture(active, false);
      }
      if (active.bytesWritten >= active.maxBytes && !active.stopping) {
        this.finish(active, "completed", "size-limit", null, false);
      }
      if (active.stopping) {
        // The backlog alone hit the size cap; finish() already closed the file
        // and recorded the bounded outcome.
        return this.toRecord(active.row, active.bytesWritten, active.row.reason);
      }
      const period = Math.max(250, Math.min(TICK_CAP_MS, validated.resolutionMs));
      active.timer = setInterval(() => this.tick(id), period);
      active.timer.unref?.();
      this.updateStatus(active, "recording", null, null);
      return this.toRecord(active.row, active.bytesWritten, null);
    } catch (error) {
      this.active.delete(id);
      closeQuietly(fd);
      try {
        unlinkSync(file);
      } catch {
        // Best effort.
      }
      try {
        this.deleteRow.run(id);
      } catch {
        // Best effort.
      }
      if (error instanceof RecordingError) throw error;
      throw new RecordingError(500, "could not start the recording");
    }
  }

  stop(id: string, createdBy: string, isAdmin: boolean): RecordingRecord {
    const row = this.selectById.get(id) as Row | undefined;
    if (row === undefined || !ID_PATTERN.test(id)) throw new RecordingError(404, "recording not found");
    this.assertOwner(row, createdBy, isAdmin);
    const active = this.active.get(id);
    if (active === undefined) {
      return this.toRecord(row, Number(row.bytes_written), row.reason);
    }
    this.finish(active, "stopped", "user-stopped");
    return this.toRecord(active.row, active.bytesWritten, "user-stopped");
  }

  remove(id: string, createdBy: string, isAdmin: boolean): void {
    if (!ID_PATTERN.test(id)) throw new RecordingError(404, "recording not found");
    const row = this.selectById.get(id) as Row | undefined;
    if (row === undefined) throw new RecordingError(404, "recording not found");
    this.assertOwner(row, createdBy, isAdmin);
    const active = this.active.get(id);
    if (active !== undefined) this.finish(active, "stopped", "deleted");
    const file = this.fileFor(row);
    try {
      unlinkSync(file);
    } catch {
      // The metadata row is still removed if the file was already gone.
    }
    this.deleteRow.run(id);
  }

  list(createdBy: string, isAdmin: boolean): RecordingRecord[] {
    const rows = this.selectRecent.all(MAX_LIST) as Row[];
    return rows
      .filter((row) => isAdmin || row.created_by === createdBy)
      .map((row) => {
        const active = this.active.get(row.id);
        return this.toRecord(row, active?.bytesWritten ?? Number(row.bytes_written), row.reason);
      });
  }

  get(id: string, createdBy: string, isAdmin: boolean): RecordingRecord {
    if (!ID_PATTERN.test(id)) throw new RecordingError(404, "recording not found");
    const row = this.selectById.get(id) as Row | undefined;
    if (row === undefined) throw new RecordingError(404, "recording not found");
    this.assertOwner(row, createdBy, isAdmin);
    const active = this.active.get(id);
    return this.toRecord(row, active?.bytesWritten ?? Number(row.bytes_written), row.reason);
  }

  /** Resolve a private file for an authenticated download; never accepts a path. */
  read(id: string, createdBy: string, isAdmin: boolean): {
    file: string;
    size: number;
    name: string;
    active: boolean;
  } {
    if (!ID_PATTERN.test(id)) throw new RecordingError(404, "recording not found");
    const row = this.selectById.get(id) as Row | undefined;
    if (row === undefined) throw new RecordingError(404, "recording not found");
    this.assertOwner(row, createdBy, isAdmin);
    const active = this.active.get(id);
    if (active !== undefined) this.capture(active, false, true);
    const file = this.fileFor(row);
    const stats = statSync(file);
    return { file, size: stats.size, name: `rigscope-recording-${id}.jsonl`, active: active !== undefined };
  }

  /**
   * Advance every active recording once. The production timer uses this same
   * path; tests and shutdown can call it deterministically.
   */
  pump(): void {
    for (const id of [...this.active.keys()]) this.tick(id);
  }

  /** Counts for the history status surface without leaking file paths. */
  status(): { active: number; total: number; maxActive: number; directory: "private" } {
    const row = this.database.db.prepare("SELECT COUNT(*) AS n FROM recordings").get() as
      | { n: number | bigint }
      | undefined;
    return {
      active: this.active.size,
      total: Number(row?.n ?? 0),
      maxActive: this.maxActive,
      directory: "private",
    };
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    for (const active of [...this.active.values()]) {
      this.finish(active, "interrupted", "hub-shutdown");
    }
  }

  // -------------------------------------------------------------------------
  // Internals
  // -------------------------------------------------------------------------

  private validateInput(input: RecordingInput): RecordingInput & {
    resolutionMs: number;
    durationMs: number;
    maxBytes: number;
    includeBacklog: boolean;
  } {
    if (input === null || typeof input !== "object") throw new RecordingError(400, "invalid recording request");
    if (!Array.isArray(input.machines) || input.machines.length === 0 || input.machines.length > MAX_MACHINES) {
      throw new RecordingError(400, `machines must contain 1..${MAX_MACHINES} machine identifiers`);
    }
    const machines = [...new Set(input.machines)];
    if (machines.some((id) => typeof id !== "string" || id.length === 0 || id.length > 128)) {
      throw new RecordingError(400, "invalid machine identifier");
    }
    if (!Array.isArray(input.metrics) || input.metrics.length === 0 || input.metrics.length > MAX_METRICS) {
      throw new RecordingError(400, `metrics must contain 1..${MAX_METRICS} metric names`);
    }
    const metrics = [...new Set(input.metrics)];
    for (const metric of metrics) {
      if (typeof metric !== "string" || !Object.prototype.hasOwnProperty.call(METRICS, metric)) {
        throw new RecordingError(400, `unknown metric ${JSON.stringify(metric)}`);
      }
    }
    const resolution = input.resolution;
    if (typeof resolution !== "string" || !Object.prototype.hasOwnProperty.call(RESOLUTIONS, resolution)) {
      throw new RecordingError(400, "resolution must be 1s, 10s, 1m, 5m or 1h");
    }
    const durationMs = input.durationMs === undefined ? this.maxDurationMs : input.durationMs;
    if (!Number.isSafeInteger(durationMs) || durationMs < 1_000 || durationMs > this.maxDurationMs) {
      throw new RecordingError(400, `durationMs must be between 1000 and ${this.maxDurationMs}`);
    }
    const maxBytes = input.maxBytes === undefined ? this.maxBytes : input.maxBytes;
    if (!Number.isSafeInteger(maxBytes) || maxBytes < 64 * 1024 || maxBytes > this.maxBytes) {
      throw new RecordingError(400, `maxBytes must be between 65536 and ${this.maxBytes}`);
    }
    return {
      ...input,
      machines,
      metrics,
      resolution: resolution as RecordingResolution,
      resolutionMs: RESOLUTIONS[resolution as RecordingResolution],
      durationMs,
      maxBytes,
      includeBacklog: input.includeBacklog === true,
    };
  }

  private tick(id: string): void {
    const active = this.active.get(id);
    if (active === undefined || active.stopping) return;
    const now = this.now();
    if (now >= active.deadlineMs) {
      this.finish(active, "completed", "duration-limit");
      return;
    }
    this.capture(active, false);
    if (active.bytesWritten >= active.maxBytes) {
      this.finish(active, "completed", "size-limit", null, false);
    }
  }

  /**
   * Write every recording bucket that has closed by `now`.
   *
   * Buckets are aligned to absolute multiples of the recording resolution and
   * are written exactly once. The pump cadence therefore cannot change the
   * recording grid, and an incomplete bucket is held until it closes instead
   * of being written as a partial bucket and then duplicated.
   */
  private capture(active: ActiveRecording, includePartial: boolean, ignoreGrace = false): void {
    const nowMs = this.now();
    // A durable store may still be batching samples for a just-closed bucket.
    // Only the final/forced captures bypass the grace window.
    const boundaryMs =
      includePartial || ignoreGrace ? nowMs : nowMs - active.graceMs;
    for (const machineId of active.input.machines) {
      if (active.stopping && !active.finishing) return;
      const cursor = active.cursors.get(machineId);
      if (cursor === undefined) continue;
      this.captureMachine(active, machineId, cursor, boundaryMs, includePartial);
      if (active.bytesWritten >= active.maxBytes) return;
    }
  }

  private captureMachine(
    active: ActiveRecording,
    machineId: string,
    cursor: MachineCursor,
    nowMs: number,
    includePartial: boolean,
  ): void {
    const stepMs = active.input.resolutionMs;
    while (!active.stopping || active.finishing) {
      const bucketStart = cursor.nextBucketMs;
      const bucketEnd = bucketStart + stepMs;
      if (bucketStart < active.captureStartMs) {
        // The first bucket can start before the recording because it follows
        // the shared absolute grid. Query the whole bucket so a summary tier
        // whose bucket begins before the recording start is not lost; it is
        // written exactly once, with `partial` reflecting only the open tail.
        if (bucketEnd > nowMs && !includePartial) return;
        const toMs = Math.min(bucketEnd - 1, nowMs);
        if (!this.writeSingleBucket(active, machineId, bucketStart, bucketStart, toMs, false)) {
          return;
        }
        cursor.lastWrittenBucketMs = bucketStart;
        cursor.nextBucketMs = bucketEnd;
        continue;
      }
      const complete = Math.floor((nowMs - bucketStart) / stepMs);
      if (complete <= 0) {
        // Only a stop/completion may write the still-open tail, and only once:
        // the cursor advances past it so a later pump cannot duplicate it.
        if (!includePartial || nowMs <= bucketStart) return;
        this.writeSingleBucket(active, machineId, bucketStart, bucketStart, nowMs, true);
        cursor.lastWrittenBucketMs = bucketStart;
        cursor.nextBucketMs = bucketEnd;
        return;
      }
      const count = Math.min(complete, CAPTURE_CHUNK_BUCKETS);
      if (!this.writeCompleteRun(active, machineId, bucketStart, count)) return;
      cursor.lastWrittenBucketMs = bucketStart + (count - 1) * stepMs;
      cursor.nextBucketMs = bucketStart + count * stepMs;
    }
  }

  /**
   * Query one portion of a single recording bucket (the grid-trimmed head or
   * the open tail) and write it as one bucket. The query is bounded to that
   * bucket, so its aggregate already carries the correct min/max and
   * sample-weighted average even when the query grid is finer.
   */
  private writeSingleBucket(
    active: ActiveRecording,
    machineId: string,
    bucketStart: number,
    fromMs: number,
    toMs: number,
    partial: boolean,
  ): boolean {
    if (toMs < fromMs) return true;
    const result = this.query(active, machineId, fromMs, toMs, 1);
    if (result === null) return false;
    const series = collectSeries(result);
    if (series.length === 0) return true;
    const stepMs = active.input.resolutionMs;
    return this.writeLine(active, {
      type: "bucket",
      machineId,
      t: bucketStart,
      tIso: new Date(bucketStart).toISOString(),
      stepMs,
      partial: partial || toMs < bucketStart + stepMs - 1,
      series,
    });
  }

  /**
   * Query a contiguous run of complete, grid-aligned buckets. Asking for one
   * point per bucket makes the history query use the recording resolution as
   * its grid (every supported resolution is a whole multiple of the storage
   * tiers), so averages stay weighted by the underlying samples.
   */
  private writeCompleteRun(
    active: ActiveRecording,
    machineId: string,
    fromMs: number,
    count: number,
  ): boolean {
    const stepMs = active.input.resolutionMs;
    const toMs = fromMs + count * stepMs - 1;
    const result = this.query(active, machineId, fromMs, toMs, count);
    if (result === null) return false;
    const buckets = groupSeries(result, fromMs, fromMs + count * stepMs, stepMs);
    for (const [t, bucket] of [...buckets].sort((a, b) => a[0] - b[0])) {
      if (active.stopping && !active.finishing) return false;
      const ok = this.writeLine(active, {
        type: "bucket",
        machineId,
        t,
        tIso: new Date(t).toISOString(),
        stepMs: bucket.stepMs,
        partial: false,
        series: bucket.series,
      });
      if (!ok) return false;
    }
    return true;
  }

  private query(
    active: ActiveRecording,
    machineId: string,
    fromMs: number,
    toMs: number,
    maxPoints: number,
  ): HistoryResult | null {
    const query: HistoryQuery = {
      machineId,
      metrics: active.input.metrics,
      fromMs,
      toMs,
      maxPoints,
    };
    try {
      return this.history.queryHistory(query, { maxElapsedMs: 250, maxRowsScanned: 50_000 });
    } catch {
      return null;
    }
  }

  private writeLine(active: ActiveRecording, value: unknown): boolean {
    if (active.stopping && !active.finishing) return false;
    if (active.bytesWritten >= active.maxBytes) return false;
    const line = `${JSON.stringify(value)}\n`;
    const bytes = Buffer.byteLength(line, "utf8");
    if (active.bytesWritten + bytes > active.maxBytes) {
      this.finish(active, "completed", "size-limit", null, false);
      return false;
    }
    try {
      writeSync(active.fd, line);
      active.bytesWritten += bytes;
      return true;
    } catch {
      this.finish(active, "failed", null, "could not write recording data", false);
      return false;
    }
  }

  private finish(
    active: ActiveRecording,
    status: Exclude<RecordingStatus, "recording">,
    reason: string | null,
    error: string | null = null,
    captureTail = true,
  ): void {
    if (active.stopping) return;
    active.stopping = true;
    // Allow the final capture to run while `stopping` is already set; this
    // keeps a size-limit finish triggered inside writeLine from recursing.
    active.finishing = true;
    if (active.timer !== null) {
      clearInterval(active.timer);
      active.timer = null;
    }
    if (captureTail && (status === "stopped" || status === "completed")) {
      // Capture only the still-open tail. Complete buckets were already
      // written by the pump, so re-querying from the recording start would
      // duplicate them.
      this.capture(active, true);
    }
    active.finishing = false;
    try {
      const endLine = `${JSON.stringify({
        type: "end",
        status,
        reason,
        at: new Date(this.now()).toISOString(),
        bytesWritten: active.bytesWritten,
      })}\n`;
      writeSync(active.fd, endLine);
      active.bytesWritten += Buffer.byteLength(endLine, "utf8");
    } catch {
      // The data already written remains usable.
    }
    closeQuietly(active.fd);
    this.active.delete(active.row.id);
    this.updateStatus(active, status, reason, error ?? null);
  }

  private updateStatus(
    active: ActiveRecording,
    status: RecordingStatus,
    reason: string | null,
    error: string | null,
  ): void {
    const stoppedAt = status === "recording" ? null : this.now();
    this.update.run(status, stoppedAt, active.bytesWritten, reason, error, active.row.id);
    active.row = {
      ...active.row,
      status,
      stopped_at_ms: stoppedAt,
      bytes_written: active.bytesWritten,
      reason,
      error,
    };
  }

  private recoverInterrupted(): void {
    const now = this.now();
    const rows = this.database.db
      .prepare("SELECT * FROM recordings WHERE status = 'recording'")
      .all() as Row[];
    this.updateAllActive.run(now);
    for (const row of rows) {
      const file = this.fileFor(row);
      try {
        const fd = openSync(file, "a", 0o600);
        try {
          writeSync(
            fd,
            `${JSON.stringify({
              type: "end",
              status: "interrupted",
              reason: "hub-restart",
              at: new Date(now).toISOString(),
              bytesWritten: Number(row.bytes_written),
            })}\n`,
          );
        } finally {
          closeSync(fd);
        }
      } catch {
        // A missing file is reported through it being unavailable to download.
      }
    }
  }

  private assertOwner(row: Row, createdBy: string, isAdmin: boolean): void {
    if (!isAdmin && row.created_by !== createdBy) {
      throw new RecordingError(403, "recording belongs to another account");
    }
  }

  private fileFor(row: Row): string {
    const name = basename(row.file_name);
    if (name !== row.file_name || !/^[0-9a-f-]{36}\.jsonl$/.test(name)) {
      throw new RecordingError(500, "recording file metadata is invalid");
    }
    const file = join(this.directory, name);
    const relative = resolve(file).slice(this.directory.length);
    if (relative.startsWith("..") || relative.includes(`..${"/"}`)) {
      throw new RecordingError(500, "recording file metadata is invalid");
    }
    return file;
  }

  private ensureDirectory(): void {
    mkdirSync(this.directory, { recursive: true, mode: 0o700 });
    try {
      const stats = statSync(this.directory);
      if (!stats.isDirectory()) throw new Error("recordings path is not a directory");
      if ((stats.mode & 0o077) !== 0) {
        // The hub's private file convention is 0700; refuse to expose files
        // through a group/world-readable directory.
        throw new Error("recordings directory must be private (0700)");
      }
    } catch (error) {
      throw new Error(
        `recordings directory is not usable: ${error instanceof Error ? error.message : String(error)}`,
      );
    }
  }

  private toRecord(row: Row, bytesWritten: number, liveReason: string | null): RecordingRecord {
    const resolutionMs = Number(row.resolution_ms);
    const resolution = resolutionFromMs(resolutionMs);
    return {
      id: row.id,
      createdBy: row.created_by,
      status: row.status as RecordingStatus,
      machines: parseStringArray(row.machines_json),
      metrics: parseStringArray(row.metrics_json),
      resolution,
      resolutionMs,
      createdAt: new Date(Number(row.created_at_ms)).toISOString(),
      startedAt: new Date(Number(row.started_at_ms)).toISOString(),
      stoppedAt: row.stopped_at_ms === null ? null : new Date(Number(row.stopped_at_ms)).toISOString(),
      deadline: new Date(Number(row.deadline_ms)).toISOString(),
      maxBytes: Number(row.max_bytes),
      bytesWritten: Math.max(Number(row.bytes_written), bytesWritten),
      includeBacklog: Number(row.include_backlog) === 1,
      reason: liveReason ?? row.reason,
      error: row.error,
      downloadPath: `/api/recordings/${row.id}/download`,
    };
  }
}

type RecordedSeries = {
  metric: string;
  resourceId: string;
  min: number | null;
  max: number | null;
  avg: number | null;
};

type GroupedBucket = {
  stepMs: number;
  series: RecordedSeries[];
};

/**
 * Fold every returned point into one recording bucket. For the normal case
 * the history grid equals the recording grid, so this is a one-to-one copy.
 * A coarser grid (only possible when retention has already dropped the finer
 * tiers) is kept at its own honest step instead of being stretched across
 * several requested-resolution buckets.
 */
function groupSeries(
  result: HistoryResult,
  fromMs: number,
  toExclusiveMs: number,
  stepMs: number,
): Map<number, GroupedBucket> {
  const out = new Map<number, GroupedBucket>();
  const coarse = result.stepMs > stepMs;
  for (const series of result.series) {
    for (const point of series.points) {
      if (!Number.isFinite(point.t)) continue;
      const t = coarse ? point.t : Math.floor(point.t / stepMs) * stepMs;
      if (t < fromMs || t >= toExclusiveMs) continue;
      const bucketStep = coarse ? result.stepMs : stepMs;
      let bucket = out.get(t);
      if (bucket === undefined) {
        bucket = { stepMs: bucketStep, series: [] };
        out.set(t, bucket);
      }
      addRecordedSeries(bucket.series, {
        metric: series.metric,
        resourceId: series.resourceId,
        min: point.min,
        max: point.max,
        avg: point.avg,
      });
    }
  }
  return out;
}

/** A single-bucket query returns one point per series; merge defensively. */
function collectSeries(result: HistoryResult): RecordedSeries[] {
  const series: RecordedSeries[] = [];
  for (const item of result.series) {
    for (const point of item.points) {
      addRecordedSeries(series, {
        metric: item.metric,
        resourceId: item.resourceId,
        min: point.min,
        max: point.max,
        avg: point.avg,
      });
    }
  }
  return series;
}

function addRecordedSeries(target: RecordedSeries[], value: RecordedSeries): void {
  const existing = target.find(
    (item) => item.metric === value.metric && item.resourceId === value.resourceId,
  );
  if (existing === undefined) {
    target.push({ ...value });
    return;
  }
  if (value.min !== null) existing.min = existing.min === null ? value.min : Math.min(existing.min, value.min);
  if (value.max !== null) existing.max = existing.max === null ? value.max : Math.max(existing.max, value.max);
  if (value.avg !== null) {
    existing.avg = existing.avg === null ? value.avg : (existing.avg + value.avg) / 2;
  }
}

function resolutionFromMs(ms: number): RecordingResolution {
  for (const [name, value] of Object.entries(RESOLUTIONS)) {
    if (value === ms) return name as RecordingResolution;
  }
  return "10s";
}

function parseStringArray(raw: string): string[] {
  try {
    const value: unknown = JSON.parse(raw);
    if (!Array.isArray(value)) return [];
    return value.filter((item): item is string => typeof item === "string");
  } catch {
    return [];
  }
}

function positive(value: number, name: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`${name} must be a positive safe integer`);
  }
  return value;
}

function nonnegative(value: number, name: string): number {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${name} must be a nonnegative safe integer`);
  }
  return value;
}

function closeQuietly(fd: number): void {
  try {
    closeSync(fd);
  } catch {
    // Already closed.
  }
}

// Kept for future compression/export work without changing imports.
export const RECORDING_LINE_MAX_BYTES = 1024 * 1024;
export type { HistoryGap };
