/**
 * One SQLite connection per hub.
 *
 * Durability contract: samples are acknowledged only after COMMIT. We therefore
 * use WAL for reader/writer concurrency but `synchronous=FULL`, which makes a
 * WAL commit fsync before returning. `busy_timeout=5000` bounds how long a
 * second connection (history-reader / backup test) can block a writer.
 *
 * Safety: the database lives in a 0700 directory and is a regular 0600 file.
 * `openHubDatabase` fails closed when the file is not private or when the
 * schema was written by newer code. That makes an accidental downgrade a loud
 * startup error rather than silent corruption of tables it does not know.
 *
 * Disk bound: `setPhysicalLimit` applies a finite SQLite `max_page_count` for
 * the main file plus a bounded WAL reserve (`journal_size_limit` and
 * `wal_autocheckpoint`). It never runs a startup VACUUM or deletes rows; an
 * already-oversized database reports pressure so ingestion can reject busy and
 * retention can reclaim old history incrementally where the file permits it.
 */

import { chmodSync, closeSync, lstatSync, mkdirSync, openSync, realpathSync, renameSync, statSync, unlinkSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { basename, dirname, join, resolve } from "node:path";
import { DatabaseSync } from "node:sqlite";
import * as sqlite from "node:sqlite";
import { MIGRATIONS, type Migration } from "./migrations";

const BUSY_TIMEOUT_MS = 5000;
const FILE_MODE = 0o600;
const DIR_MODE = 0o700;

/** Default finite physical budget for the hub database (main file plus WAL). */
export const DEFAULT_MAX_DATABASE_DISK_BYTES = 2 * 1024 * 1024 * 1024;
/** The WAL keeps a bounded reserve inside the physical budget. */
export const HUB_WAL_RESERVE_BYTES = 128 * 1024 * 1024;

export type HubPhysicalStatus = {
  limitBytes: number;
  /** Main database file bytes. */
  mainBytes: number;
  /** Write-ahead log bytes. */
  walBytes: number;
  /** mainBytes + walBytes, the number the finite cap applies to. */
  bytes: number;
  pageSize: number;
  pageCount: number;
  maxPageCount: number;
  freePages: number;
  walBudgetBytes: number;
  checkpointPages: number;
  autoVacuum: number;
  /** True when the current file already exceeds the requested limit. */
  pressure: boolean;
};

export type HubDatabase = {
  db: DatabaseSync;
  path: string;
  schemaVersion(): number;
  close(): void;
  /** Main database pages plus the WAL file; excludes the tiny shm index. */
  sizeBytes(): number;
  /** WAL sidecar bytes only. */
  walBytes(): number;
  /**
   * Apply a finite main-file page cap and a bounded WAL reserve. Existing
   * databases over the requested cap are reported as pressure; this never
   * deletes rows or runs a destructive VACUUM at startup.
   */
  setPhysicalLimit(limitBytes: number): HubPhysicalStatus;
  physicalLimit(): HubPhysicalStatus | null;
  /** Bounded incremental reclaim of free pages when auto_vacuum permits it. */
  reclaimPages(maxPages?: number): number;
  /** Consistent online backup; the destination is a regular 0600 file. */
  backupTo(destPath: string): Promise<void>;
  checkpoint(): void;
};


/**
 * Opens (and if needed creates/migrates) a hub database.
 *
 * `:memory:` is accepted for tests. A file path gets its parent directory
 * created with 0700 when missing; an existing path must already be a regular
 * private (0600) file, otherwise this throws before reading any contents.
 */
export function openHubDatabase(path: string): HubDatabase {
  if (typeof path !== "string" || path.length === 0 || path.length > 4096) {
    throw new Error("hub database path must be a nonempty string");
  }
  const memory = path === ":memory:";
  if (!memory) preparePrivateFile(path);

  let db: DatabaseSync;
  try {
    db = new DatabaseSync(path, { timeout: BUSY_TIMEOUT_MS });
  } catch (error) {
    throw new Error(`cannot open hub database: ${describeError(error)}`, { cause: error });
  }

  try {
    // New databases can return freed pages to the filesystem incrementally.
    // Existing databases keep their current mode; changing it needs VACUUM,
    // which startup deliberately never runs. This must be set before WAL mode
    // and before the first schema page is written.
    db.exec("PRAGMA auto_vacuum = INCREMENTAL");
    db.exec("PRAGMA journal_mode = WAL");
    db.exec("PRAGMA synchronous = FULL");
    db.exec("PRAGMA foreign_keys = ON");
    db.exec(`PRAGMA busy_timeout = ${BUSY_TIMEOUT_MS}`);
    // Only after the file is known-good do we tidy sidecar permissions.
    if (!memory) normalizeSidecarModes(path);
    migrate(db);
  } catch (error) {
    try {
      db.close();
    } catch {
      // The original error is the useful one.
    }
    throw error;
  }

  return new SqliteHubDatabase(db, path);
}

class SqliteHubDatabase implements HubDatabase {
  readonly db: DatabaseSync;
  readonly path: string;
  private closed = false;
  private physical: HubPhysicalStatus | null = null;

  constructor(db: DatabaseSync, path: string) {
    this.db = db;
    this.path = path;
  }

  schemaVersion(): number {
    this.assertOpen();
    const row = this.db
      .prepare("SELECT COALESCE(MAX(version), 0) AS version FROM schema_migrations")
      .get() as { version?: number | bigint } | undefined;
    return Number(row?.version ?? 0);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    try {
      // Truncate helps keep the WAL small after a restore/shutdown.
      this.db.exec("PRAGMA wal_checkpoint(TRUNCATE)");
    } catch {
      // A checkpoint failure must not prevent closing the handle.
    }
    this.db.close();
  }

  sizeBytes(): number {
    this.assertOpen();
    return this.mainBytes() + this.walBytes();
  }

  walBytes(): number {
    this.assertOpen();
    return this.path === ":memory:" ? 0 : fileSize(`${this.path}-wal`);
  }

  setPhysicalLimit(limitBytes: number): HubPhysicalStatus {
    this.assertOpen();
    if (!Number.isSafeInteger(limitBytes) || limitBytes < 16 * 1024) {
      throw new Error("physical limit must be a safe integer of at least 16 KiB");
    }
    const pageSize = this.pragmaNumber("page_size", 4096);
    const walBudgetBytes = Math.max(64 * 1024, Math.min(HUB_WAL_RESERVE_BYTES, Math.floor(limitBytes / 16)));
    const mainBudgetBytes = Math.max(pageSize, limitBytes - walBudgetBytes);
    const requestedPages = Math.max(1, Math.floor(mainBudgetBytes / pageSize));
    const checkpointPages = Math.max(16, Math.min(1000, Math.floor(walBudgetBytes / pageSize)));

    this.db.exec(`PRAGMA journal_size_limit = ${walBudgetBytes}`);
    this.db.exec(`PRAGMA wal_autocheckpoint = ${checkpointPages}`);
    let maxPageCount = requestedPages;
    try {
      const row = this.db.prepare(`PRAGMA max_page_count = ${requestedPages}`).get() as
        | { max_page_count?: number | bigint }
        | undefined;
      maxPageCount = Number(row?.max_page_count ?? requestedPages);
    } catch {
      // Older SQLite builds may not return the value from assignment. The
      // page cap still applies; read it back below.
      try {
        this.db.exec(`PRAGMA max_page_count = ${requestedPages}`);
      } catch {
        // The physical check below still reports the measurable result.
      }
      maxPageCount = this.pragmaNumber("max_page_count", requestedPages);
    }
    this.physical = this.readPhysicalStatus(limitBytes, walBudgetBytes, checkpointPages, maxPageCount);
    // Shrink an existing WAL when possible without blocking on a reader.
    try {
      this.db.exec("PRAGMA wal_checkpoint(TRUNCATE)");
    } catch {
      // A busy checkpoint must not make opening the hub fail.
    }
    this.physical = this.readPhysicalStatus(limitBytes, walBudgetBytes, checkpointPages, maxPageCount);
    return this.physical;
  }

  physicalLimit(): HubPhysicalStatus | null {
    this.assertOpen();
    if (this.physical === null) return null;
    return this.readPhysicalStatus(
      this.physical.limitBytes,
      this.physical.walBudgetBytes,
      this.physical.checkpointPages,
      this.physical.maxPageCount,
    );
  }

  reclaimPages(maxPages = 64): number {
    this.assertOpen();
    if (this.pragmaNumber("auto_vacuum", 0) !== 2) return 0;
    const pages = Math.max(1, Math.min(100_000, Math.floor(maxPages)));
    const before = this.pragmaNumber("freelist_count", 0);
    try {
      this.db.exec(`PRAGMA incremental_vacuum(${pages})`);
    } catch {
      return 0;
    }
    return Math.max(0, before - this.pragmaNumber("freelist_count", 0));
  }

  async backupTo(destPath: string): Promise<void> {
    this.assertOpen();
    if (typeof destPath !== "string" || destPath.length === 0) {
      throw new Error("backup destination must be a nonempty path");
    }
    const destination = resolve(destPath);
    const source = this.path === ":memory:" ? null : realpathSync(this.path);
    const parent = dirname(destination);
    mkdirSync(parent, { recursive: true, mode: DIR_MODE });
    const canonicalParent = realpathSync(parent);
    const canonicalDestination = join(canonicalParent, basename(destination));

    if (source !== null && canonicalDestination === source) {
      throw new Error("backup destination aliases the live database");
    }
    if (source !== null) {
      const sourceStats = statSync(this.path);
      let existing: ReturnType<typeof lstatSync> | null = null;
      try {
        existing = lstatSync(destination);
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      }
      if (existing !== null) {
        if (!existing.isFile()) throw new Error("backup destination must be a regular file path");
        const destinationStats = statSync(destination);
        if (
          destinationStats.dev === sourceStats.dev &&
          destinationStats.ino === sourceStats.ino
        ) {
          throw new Error("backup destination aliases the live database");
        }
      }
    }

    // Build the replacement beside the destination, verify/chmod it, then
    // atomically rename over the old backup. A failed backup therefore leaves
    // the previous good backup and the live database untouched.
    const temporary = join(
      canonicalParent,
      `.${basename(destination)}.${process.pid}.${randomUUID()}.tmp`,
    );
    const cleanup = (): void => {
      try {
        unlinkSync(temporary);
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      }
    };

    const backupFn = (sqlite as unknown as { backup?: (db: DatabaseSync, path: string) => Promise<number> }).backup;
    try {
      if (typeof backupFn === "function") {
        await backupFn(this.db, temporary);
      } else {
        // Node without the backup API still supports VACUUM INTO (>=3.27).
        this.db.prepare("VACUUM INTO ?").run(temporary);
      }
      // node:sqlite creates the destination with the process umask; restore the
      // private mode before it can replace the old backup.
      chmodSync(temporary, FILE_MODE);
      renameSync(temporary, destination);
    } catch (error) {
      try {
        cleanup();
      } catch {
        // Preserve the primary failure.
      }
      throw new Error(`database backup failed: ${describeError(error)}`, { cause: error });
    }
  }

  checkpoint(): void {
    this.assertOpen();
    this.db.exec("PRAGMA wal_checkpoint(TRUNCATE)");
  }

  private assertOpen(): void {
    if (this.closed) throw new Error("hub database is closed");
  }

  private mainBytes(): number {
    if (this.path === ":memory:") {
      return this.pragmaNumber("page_count", 0) * this.pragmaNumber("page_size", 4096);
    }
    return fileSize(this.path);
  }

  private pragmaNumber(name: string, fallback: number): number {
    try {
      const row = this.db.prepare(`PRAGMA ${name}`).get() as Record<string, unknown> | undefined;
      const value = row?.[name];
      const number = typeof value === "bigint" ? Number(value) : typeof value === "number" ? value : Number(value);
      return Number.isFinite(number) ? number : fallback;
    } catch {
      return fallback;
    }
  }

  private readPhysicalStatus(
    limitBytes: number,
    walBudgetBytes: number,
    checkpointPages: number,
    maxPageCount: number,
  ): HubPhysicalStatus {
    const mainBytes = this.mainBytes();
    const walBytes = this.walBytes();
    const bytes = mainBytes + walBytes;
    const pageCount = this.pragmaNumber("page_count", 0);
    const freePages = this.pragmaNumber("freelist_count", 0);
    return {
      limitBytes,
      mainBytes,
      walBytes,
      bytes,
      pageSize: this.pragmaNumber("page_size", 4096),
      pageCount,
      maxPageCount,
      freePages,
      walBudgetBytes,
      checkpointPages,
      autoVacuum: this.pragmaNumber("auto_vacuum", 0),
      pressure:
        bytes > limitBytes ||
        (pageCount >= maxPageCount && freePages === 0),
    };
  }
}

function migrate(db: DatabaseSync): void {
  db.exec(`
    CREATE TABLE IF NOT EXISTS schema_migrations (
      version INTEGER PRIMARY KEY,
      name TEXT NOT NULL,
      applied_at_ms INTEGER NOT NULL
    )
  `);

  const applied = db
    .prepare("SELECT version, name FROM schema_migrations ORDER BY version")
    .all() as Array<{ version: number | bigint; name: string }>;
  const known = new Map<number, Migration>(MIGRATIONS.map((migration) => [migration.version, migration]));

  // Refuse a database written by a future build before touching anything.
  // Also refuse holes or renamed migrations: both make downgrade/recovery
  // unsafe and indicate a different schema history than this code assumes.
  for (const [index, row] of applied.entries()) {
    const version = Number(row.version);
    const expected = MIGRATIONS[index];
    if (!expected || version !== expected.version) {
      if (!known.has(version) || version > MIGRATIONS[MIGRATIONS.length - 1]!.version) {
        throw new Error(
          `hub database schema version ${version} is newer than this hub knows (${MIGRATIONS[MIGRATIONS.length - 1]!.version}); upgrade the hub before opening this database`,
        );
      }
      throw new Error(`hub database migration history is not contiguous at version ${version}`);
    }
    if (row.name !== expected.name) {
      throw new Error(`hub database migration ${version} is named "${row.name}", expected "${expected.name}"`);
    }
  }

  const insert = db.prepare(
    "INSERT INTO schema_migrations (version, name, applied_at_ms) VALUES (?, ?, ?)",
  );
  for (const migration of MIGRATIONS.slice(applied.length)) {
    db.exec("BEGIN IMMEDIATE");
    try {
      migration.up(db);
      insert.run(migration.version, migration.name, Date.now());
      db.exec("COMMIT");
    } catch (error) {
      try {
        db.exec("ROLLBACK");
      } catch {
        // Preserve the migration error.
      }
      throw new Error(`hub database migration ${migration.version} (${migration.name}) failed: ${describeError(error)}`, {
        cause: error,
      });
    }
  }
}

function preparePrivateFile(path: string): void {
  const parent = dirname(path);
  mkdirSync(parent, { recursive: true, mode: DIR_MODE });

  let stats: ReturnType<typeof lstatSync> | null = null;
  try {
    stats = lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }

  if (stats === null) {
    // Create with 0600 atomically rather than relying on the process umask.
    const fd = openSync(path, "wx", FILE_MODE);
    closeSync(fd);
    return;
  }
  if (!stats.isFile()) {
    throw new Error("hub database path must be a regular file");
  }
  if ((stats.mode & 0o077) !== 0) {
    throw new Error("hub database file must be private (0600)");
  }
}

function normalizeSidecarModes(path: string): void {
  for (const sidecar of [path, `${path}-wal`, `${path}-shm`]) {
    try {
      // chmod follows the principal file, not a symlink target: lstat was
      // already used to reject non-regular main paths.
      chmodSync(sidecar, FILE_MODE);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
  }
}

function fileSize(path: string): number {
  try {
    return statSync(path).size;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return 0;
    throw error;
  }
}

/** True for SQLite full-disk/capacity errors, including node:sqlite's native code. */
export function isStorageFullError(error: unknown): boolean {
  if (typeof error === "object" && error !== null && "errcode" in error) {
    const code = (error as { errcode?: unknown }).errcode;
    if (code === 13) return true;
  }
  const message = error instanceof Error ? error.message : String(error);
  return /SQLITE_FULL|database or disk is full|disk I\/O error/i.test(message);
}

function describeError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
