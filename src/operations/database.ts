/**
 * Durable hub database operations: inspect, online backup, validated restore
 * and guarded schema migration.
 *
 * The hub's SQLite file is the authoritative store for telemetry, the
 * collector registry, admin users/audit and alert rules/incidents/jobs. An
 * online SQLite backup captures every table in that one file; no table list is
 * passed to the backup API. Backup never opens the schema migrator, so a
 * backup is safe even when the source is older than this operations build.
 *
 * Restore is intentionally conservative: validate the input and a staged
 * copy, refuse newer schemas and active hub locks, back up the current target,
 * then replace atomically. WAL/SHM sidecars are removed only after the caller
 * has confirmed the hub is stopped.
 */

import { chmodSync, existsSync, lstatSync, renameSync, statSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { basename, dirname, join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import * as sqlite from "node:sqlite";
import { openHubDatabase, type HubDatabase } from "../hub/storage/db";
import { MIGRATIONS } from "../hub/storage/migrations";
import {
  OperationsError,
  assertNoActiveHubLock,
  assertPrivateRegularFile,
  copyFileAtomic,
  ensurePrivateDir,
  errorMessage,
  fileSize,
  fsyncDir,
  isPrivateMode,
  removeIfExists,
  resolvePath,
  sha256File,
  timestampSlug,
} from "./fs";

export const SUPPORTED_SCHEMA_VERSION = MIGRATIONS.length === 0 ? 0 : MIGRATIONS[MIGRATIONS.length - 1]!.version;

const BUSY_TIMEOUT_MS = 5_000;
const PRIVATE_FILE_MODE = 0o600;
/** Small tables whose counts are cheap and operationally useful. */
const COUNTED_TABLES = new Set([
  "schema_migrations",
  "collector_registry",
  "collector_registry_meta",
  "collector_grants",
  "collector_audit",
  "users",
  "admin_audit",
  "admin_meta",
  "machine_meta",
  "machine_latest",
  "inventory",
  "points_10s",
  "gaps",
  "recordings",
  "alert_rules",
  "alert_incidents",
  "alert_jobs",
  "alert_webhook",
  "alert_machine_progress",
]);

export type MigrationLedgerRow = { version: number; name: string };

export type InspectReport = {
  path: string;
  fileBytes: number;
  walBytes: number;
  shmBytes: number;
  mode: string;
  integrity: string;
  integrityOk: boolean;
  journalMode: string;
  pageSize: number;
  pageCount: number;
  hasSchemaMigrations: boolean;
  schemaVersion: number;
  supportedSchemaVersion: number;
  newerSchema: boolean;
  ledger: MigrationLedgerRow[];
  ledgerProblem: string | null;
  tables: Array<{ name: string; rows?: number }>;
  registryPresent: boolean;
  adminPresent: boolean;
  alertsPresent: boolean;
};

export type BackupReport = {
  database: string;
  output: string;
  sourceSchemaVersion: number;
  bytes: number;
  sha256: string;
  integrity: string;
  tables: string[];
  migratedSource: false;
};

export type RestoreReport = {
  input: string;
  database: string;
  restoredSchemaVersion: number;
  supportedSchemaVersion: number;
  replaced: boolean;
  preRestoreBackup: string | null;
  bytes: number;
  integrity: string;
};

export type MigrateReport = {
  database: string;
  fromSchemaVersion: number;
  toSchemaVersion: number;
  migrated: boolean;
  backupPath: string | null;
  integrity: string;
};

export type MigrateRunner = (databasePath: string) => void;

export type RestoreOptions = {
  input: string;
  database: string;
  replace?: boolean;
  confirmStopped?: boolean;
  now?: () => number;
};

export type MigrateOptions = {
  database: string;
  backupDir?: string;
  confirmStopped?: boolean;
  now?: () => number;
};

export type ResetTelemetryOptions = {
  database: string;
  confirmStopped?: boolean;
};

export type ResetTelemetryReport = {
  database: string;
  deleted: Record<string, number>;
  integrity: string;
  /** Non-history state deliberately left untouched. */
  preserved: string[];
};

function openReadOnly(path: string): DatabaseSync {
  const db = new DatabaseSync(path, { readOnly: true, timeout: BUSY_TIMEOUT_MS });
  db.exec(`PRAGMA busy_timeout = ${BUSY_TIMEOUT_MS}`);
  return db;
}

function rowsOf(statement: ReturnType<DatabaseSync["prepare"]>): Array<Record<string, unknown>> {
  return statement.all() as Array<Record<string, unknown>>;
}

function firstValue(row: Record<string, unknown> | undefined): unknown {
  return row === undefined ? undefined : Object.values(row)[0];
}

function integrityResult(db: DatabaseSync): { text: string; ok: boolean } {
  const rows = rowsOf(db.prepare("PRAGMA integrity_check"));
  const lines = rows.map((row) => String(firstValue(row) ?? ""));
  const text = lines.length === 0 ? "no integrity result" : lines.join("; ");
  return { text, ok: lines.length === 1 && lines[0] === "ok" };
}

function tableNames(db: DatabaseSync): string[] {
  return rowsOf(
    db.prepare("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name"),
  ).map((row) => String(row.name));
}

function readLedger(db: DatabaseSync, hasTable: boolean): MigrationLedgerRow[] {
  if (!hasTable) return [];
  return rowsOf(db.prepare("SELECT version, name FROM schema_migrations ORDER BY version")).map((row) => ({
    version: Number(row.version),
    name: String(row.name),
  }));
}

function ledgerProblem(ledger: readonly MigrationLedgerRow[]): string | null {
  if (ledger.length === 0) return "schema_migrations exists but has no applied migrations";
  for (const [index, row] of ledger.entries()) {
    if (!Number.isSafeInteger(row.version) || row.version < 1) return `invalid migration version ${row.version}`;
    if (row.version !== index + 1) {
      return `migration history is not contiguous at version ${row.version}`;
    }
    const expected = MIGRATIONS.find((migration) => migration.version === row.version);
    if (expected === undefined) {
      return row.version > SUPPORTED_SCHEMA_VERSION
        ? `schema version ${row.version} is newer than this build (${SUPPORTED_SCHEMA_VERSION})`
        : `unknown migration version ${row.version}`;
    }
    if (row.name !== expected.name) {
      return `migration ${row.version} is named "${row.name}", expected "${expected.name}"`;
    }
  }
  return null;
}

function describeOpenDatabase(db: DatabaseSync, path: string, statMode: number): InspectReport {
  const integrity = integrityResult(db);
  const tables = tableNames(db);
  const hasSchemaMigrations = tables.includes("schema_migrations");
  const ledger = readLedger(db, hasSchemaMigrations);
  const schemaVersion = ledger.length === 0 ? 0 : ledger[ledger.length - 1]!.version;
  const problem = hasSchemaMigrations ? ledgerProblem(ledger) : "schema_migrations is missing";
  const journal = db.prepare("PRAGMA journal_mode").get() as Record<string, unknown> | undefined;
  const pageSize = Number((db.prepare("PRAGMA page_size").get() as Record<string, unknown> | undefined)?.page_size ?? 0);
  const pageCount = Number((db.prepare("PRAGMA page_count").get() as Record<string, unknown> | undefined)?.page_count ?? 0);
  const counted = tables.map((name) => {
    if (!COUNTED_TABLES.has(name)) return { name };
    try {
      const row = db.prepare(`SELECT COUNT(*) AS n FROM "${name.replace(/"/g, '""')}"`).get() as
        | Record<string, unknown>
        | undefined;
      return { name, rows: Number(row?.n ?? 0) };
    } catch {
      return { name };
    }
  });
  return {
    path,
    fileBytes: fileSize(path),
    walBytes: fileSize(`${path}-wal`),
    shmBytes: fileSize(`${path}-shm`),
    mode: `0${(statMode & 0o777).toString(8).padStart(3, "0")}`,
    integrity: integrity.text,
    integrityOk: integrity.ok,
    journalMode: String(firstValue(journal) ?? "unknown"),
    pageSize,
    pageCount,
    hasSchemaMigrations,
    schemaVersion,
    supportedSchemaVersion: SUPPORTED_SCHEMA_VERSION,
    newerSchema: schemaVersion > SUPPORTED_SCHEMA_VERSION,
    ledger,
    ledgerProblem: problem,
    tables: counted,
    registryPresent: tables.includes("collector_registry"),
    adminPresent: tables.includes("users"),
    alertsPresent:
      tables.includes("alert_rules") && tables.includes("alert_incidents") && tables.includes("alert_jobs"),
  };
}

function chmodSidecars(path: string): void {
  // A read-only open may create shm/wal files with the process umask. The
  // containing directory is private, but normalize modes as well.
  for (const sidecar of [`${path}-wal`, `${path}-shm`]) {
    try {
      const stat = lstatSync(sidecar);
      if (stat.isFile() && !isPrivateMode(stat.mode)) chmodSync(sidecar, PRIVATE_FILE_MODE);
    } catch {
      // Missing sidecars are normal.
    }
  }
}

export function inspectDatabase(path: string): InspectReport {
  const absolute = resolvePath(path, "database");
  const stat = assertPrivateRegularFile(absolute, "database");
  const db = openReadOnly(absolute);
  try {
    return describeOpenDatabase(db, absolute, stat.mode);
  } finally {
    db.close();
    chmodSidecars(absolute);
  }
}

function finalizeBackupFile(path: string): void {
  // A SQLite backup inherits WAL journal mode; convert the copy to a single
  // self-contained DELETE-mode file and remove empty sidecars before atomic
  // publication.
  const db = new DatabaseSync(path, { timeout: BUSY_TIMEOUT_MS });
  try {
    db.exec("PRAGMA busy_timeout = 5000");
    db.exec("PRAGMA journal_mode = DELETE");
    db.exec("PRAGMA wal_checkpoint(TRUNCATE)");
  } finally {
    db.close();
  }
  removeIfExists(`${path}-wal`);
  removeIfExists(`${path}-shm`);
}

function sameFile(a: string, b: string): boolean {
  if (a === b) return true;
  if (!existsSync(a) || !existsSync(b)) return false;
  const left = statSync(a); const right = statSync(b);
  return left.dev === right.dev && left.ino === right.ino;
}

export async function backupDatabase(options: {
  database: string;
  output: string;
  replace?: boolean;
  now?: () => number;
}): Promise<BackupReport> {
  const database = resolvePath(options.database, "database");
  const output = resolvePath(options.output, "backup output");
  if (sameFile(database, output)) throw new OperationsError("unsafe-path", "backup output must differ from the live database");
  const sourceStat = assertPrivateRegularFile(database, "database");
  if (sourceStat.size === 0) throw new OperationsError("invalid-database", `database is empty: ${database}`);

  if (existsSync(output)) {
    const existing = lstatSync(output);
    if (!existing.isFile() || existing.isSymbolicLink()) {
      throw new OperationsError("unsafe-path", `backup output must be a regular file path: ${output}`);
    }
    if (!options.replace) {
      throw new OperationsError("output-exists", `backup output already exists (use --replace to replace it): ${output}`);
    }
  }
  ensurePrivateDir(dirname(output));

  const databaseStat = assertPrivateRegularFile(database, "database");
  const db = openReadOnly(database);
  let temporary: string | null = null;
  let report: Omit<BackupReport, "output" | "bytes" | "sha256">;
  try {
    const info = describeOpenDatabase(db, database, databaseStat.mode);
    if (!info.hasSchemaMigrations) {
      throw new OperationsError("invalid-database", "source is not a hub database: schema_migrations is missing");
    }
    if (!info.integrityOk) {
      throw new OperationsError("integrity-failed", `source database failed integrity_check: ${info.integrity}`);
    }
    temporary = join(
      dirname(output),
      `.${basename(output)}.tmp-${timestampSlug(options.now?.() ?? Date.now())}-${randomUUID()}`,
    );
    const backupFn = (sqlite as unknown as { backup?: (source: DatabaseSync, destination: string) => Promise<number> }).backup;
    if (typeof backupFn !== "function") {
      throw new OperationsError("unsupported-runtime", "this Node runtime has no node:sqlite backup API");
    }
    await backupFn(db, temporary);
    report = {
      database,
      sourceSchemaVersion: info.schemaVersion,
      integrity: info.integrity,
      tables: info.tables.map((table) => table.name),
      migratedSource: false,
    };
  } catch (error) {
    if (temporary !== null) removeIfExists(temporary);
    throw error;
  } finally {
    db.close();
    chmodSidecars(database);
  }

  try {
    finalizeBackupFile(temporary);
    chmodSync(temporary, PRIVATE_FILE_MODE);
    const backupInfo = inspectDatabase(temporary);
    if (!backupInfo.integrityOk) {
      throw new OperationsError("integrity-failed", `backup copy failed integrity_check: ${backupInfo.integrity}`);
    }
    if (backupInfo.schemaVersion !== report.sourceSchemaVersion) {
      throw new OperationsError(
        "backup-mismatch",
        `backup schema ${backupInfo.schemaVersion} differs from source schema ${report.sourceSchemaVersion}`,
      );
    }
    const stat = statSync(temporary);
    const sha256 = await sha256File(temporary);
    renameSync(temporary, output);
    chmodSync(output, PRIVATE_FILE_MODE);
    temporary = null;
    fsyncDir(dirname(output));
    return {
      ...report,
      output,
      bytes: stat.size,
      sha256,
    };
  } catch (error) {
    if (temporary !== null) {
      removeIfExists(temporary);
      removeIfExists(`${temporary}-wal`);
      removeIfExists(`${temporary}-shm`);
    }
    throw error;
  }
}

function checkpointForReplacement(database: string): void {
  const db = new DatabaseSync(database, { timeout: BUSY_TIMEOUT_MS });
  try {
    db.exec("PRAGMA busy_timeout = 5000");
    db.exec("PRAGMA wal_checkpoint(TRUNCATE)");
  } finally {
    db.close();
  }
}

function validateRestoreInput(input: string): { info: InspectReport; absolute: string } {
  const absolute = resolvePath(input, "backup input");
  const info = inspectDatabase(absolute);
  if (!info.integrityOk) {
    throw new OperationsError("integrity-failed", `backup failed integrity_check: ${info.integrity}`);
  }
  if (!info.hasSchemaMigrations || info.schemaVersion < 1) {
    throw new OperationsError("invalid-backup", "backup has no hub schema (schema_migrations missing or empty)");
  }
  if (info.newerSchema) {
    throw new OperationsError(
      "newer-schema",
      `backup schema version ${info.schemaVersion} is newer than this operations build supports (${SUPPORTED_SCHEMA_VERSION}); upgrade the hub/operations first`,
    );
  }
  if (info.ledgerProblem !== null) {
    throw new OperationsError("invalid-backup", `backup migration history is not usable: ${info.ledgerProblem}`);
  }
  return { info, absolute };
}

export async function restoreDatabase(options: RestoreOptions): Promise<RestoreReport> {
  const input = validateRestoreInput(options.input);
  const database = resolvePath(options.database, "database");
  if (sameFile(database, input.absolute)) {
    throw new OperationsError("unsafe-path", "backup input and restore target must differ");
  }
  const targetExists = existsSync(database);
  if (targetExists) {
    assertPrivateRegularFile(database, "restore target");
    if (!options.replace) {
      throw new OperationsError("target-exists", "restore target exists; pass --replace with --confirm-stopped");
    }
    if (!options.confirmStopped) {
      throw new OperationsError(
        "confirmation-required",
        "restore over an existing database requires --confirm-stopped after the hub is stopped",
      );
    }
    ensurePrivateDir(dirname(database));
  } else {
    ensurePrivateDir(dirname(database));
  }
  assertNoActiveHubLock(database);

  const temporary = join(
    dirname(database),
    `.${basename(database)}.restore-${timestampSlug(options.now?.() ?? Date.now())}-${randomUUID().slice(0, 8)}`,
  );
  removeIfExists(temporary);
  let preRestoreBackup: string | null = null;
  try {
    copyFileAtomic(input.absolute, temporary, PRIVATE_FILE_MODE);
    const staged = inspectDatabase(temporary);
    if (!staged.integrityOk) {
      throw new OperationsError("integrity-failed", `staged restore failed integrity_check: ${staged.integrity}`);
    }
    if (staged.schemaVersion !== input.info.schemaVersion) {
      throw new OperationsError("restore-mismatch", "staged copy does not match the source backup schema");
    }
    if (staged.newerSchema) {
      throw new OperationsError("newer-schema", "staged restore has a newer schema than this build supports");
    }
    // Normalize the staged copy to a single DELETE-mode file before it becomes
    // the live database; otherwise copied WAL metadata could look for sidecars
    // belonging to the old target.
    finalizeBackupFile(temporary);
    chmodSync(temporary, PRIVATE_FILE_MODE);

    if (targetExists) {
      // Stop proof is required above. Checkpoint so the old main file is
      // self-contained before the old WAL is detached, then keep a full
      // online backup of the good target before any replacement.
      checkpointForReplacement(database);
      preRestoreBackup = join(
        dirname(database),
        `${basename(database)}.pre-restore-${timestampSlug(options.now?.() ?? Date.now())}-${randomUUID().slice(0, 8)}.sqlite`,
      );
      await backupDatabase({ database, output: preRestoreBackup, now: options.now });
      assertNoActiveHubLock(database);
    }

    // Remove obsolete sidecars only after all validation/backup steps and the
    // stopped/lock checks. This also handles a target whose main file was
    // deleted while stale WAL/SHM remained.
    removeIfExists(`${database}-wal`);
    removeIfExists(`${database}-shm`);
    renameSync(temporary, database);
    chmodSync(database, PRIVATE_FILE_MODE);
    fsyncDir(dirname(database));
    const restored = inspectDatabase(database);
    return {
      input: input.absolute,
      database,
      restoredSchemaVersion: restored.schemaVersion,
      supportedSchemaVersion: SUPPORTED_SCHEMA_VERSION,
      replaced: targetExists,
      preRestoreBackup,
      bytes: restored.fileBytes,
      integrity: restored.integrity,
    };
  } catch (error) {
    removeIfExists(temporary);
    removeIfExists(`${temporary}-wal`);
    removeIfExists(`${temporary}-shm`);
    throw error;
  }
}

function defaultMigrateRunner(databasePath: string): void {
  let db: HubDatabase | null = null;
  try {
    db = openHubDatabase(databasePath);
    const version = db.schemaVersion();
    if (version !== SUPPORTED_SCHEMA_VERSION) {
      throw new Error(`migrated database reports schema ${version}, expected ${SUPPORTED_SCHEMA_VERSION}`);
    }
  } finally {
    db?.close();
  }
}

function freshDatabase(path: string): boolean {
  if (!existsSync(path)) return true;
  const stat = lstatSync(path);
  if (!stat.isFile()) throw new OperationsError("unsafe-path", `database path must be a regular file: ${path}`);
  return stat.size === 0;
}

export async function migrateDatabase(
  options: MigrateOptions & { runMigrations?: MigrateRunner },
  now: () => number = options.now ?? Date.now,
): Promise<MigrateReport> {
  const database = resolvePath(options.database, "database");
  const existed = existsSync(database);
  const fresh = freshDatabase(database);
  if (existed && !fresh) {
    assertPrivateRegularFile(database, "database");
    if (!options.confirmStopped) {
      throw new OperationsError(
        "confirmation-required",
        "migrating an existing database requires --confirm-stopped after the hub is stopped",
      );
    }
    assertNoActiveHubLock(database);
  } else {
    ensurePrivateDir(dirname(database));
  }

  if (fresh) {
    // A fresh/empty database cannot have valid sidecars from a live hub.
    removeIfExists(`${database}-wal`);
    removeIfExists(`${database}-shm`);
    try {
      (options.runMigrations ?? defaultMigrateRunner)(database);
    } catch (error) {
      if (!existed || fresh) {
        removeIfExists(database);
        removeIfExists(`${database}-wal`);
        removeIfExists(`${database}-shm`);
      }
      throw new OperationsError("migration-failed", `database migration failed: ${errorMessage(error)}`, { cause: error });
    }
    const info = inspectDatabase(database);
    if (!info.integrityOk || info.schemaVersion !== SUPPORTED_SCHEMA_VERSION) {
      removeIfExists(database);
      removeIfExists(`${database}-wal`);
      removeIfExists(`${database}-shm`);
      throw new OperationsError(
        "migration-failed",
        `new database verification failed: ${info.integrity}; schema ${info.schemaVersion} expected ${SUPPORTED_SCHEMA_VERSION}`,
      );
    }
    return {
      database,
      fromSchemaVersion: 0,
      toSchemaVersion: info.schemaVersion,
      migrated: true,
      backupPath: null,
      integrity: info.integrity,
    };
  }

  const before = inspectDatabase(database);
  if (before.newerSchema) {
    throw new OperationsError(
      "newer-schema",
      `database schema version ${before.schemaVersion} is newer than this build supports (${SUPPORTED_SCHEMA_VERSION}); upgrade the hub/operations before opening it`,
    );
  }
  if (before.ledgerProblem !== null) {
    throw new OperationsError("invalid-database", `database migration history is not usable: ${before.ledgerProblem}`);
  }
  if (before.schemaVersion === SUPPORTED_SCHEMA_VERSION) {
    return {
      database,
      fromSchemaVersion: before.schemaVersion,
      toSchemaVersion: before.schemaVersion,
      migrated: false,
      backupPath: null,
      integrity: before.integrity,
    };
  }

  const backupDir = resolvePath(options.backupDir ?? join(dirname(database), "backups"), "backup directory");
  ensurePrivateDir(backupDir);
  const backupPath = join(
    backupDir,
    `${basename(database)}.pre-migrate-v${before.schemaVersion}-to-v${SUPPORTED_SCHEMA_VERSION}-${timestampSlug(now())}-${randomUUID().slice(0, 8)}.sqlite`,
  );
  if (existsSync(backupPath)) {
    throw new OperationsError("output-exists", `pre-migration backup already exists: ${backupPath}`);
  }
  const backup = await backupDatabase({ database, output: backupPath, now });
  try {
    (options.runMigrations ?? defaultMigrateRunner)(database);
  } catch (error) {
    let recovery: string;
    try {
      await restoreDatabase({
        input: backupPath,
        database,
        replace: true,
        confirmStopped: true,
        now,
      });
      recovery = `restored the pre-migration backup ${backupPath}`;
    } catch (restoreError) {
      recovery = `automatic restore failed (${errorMessage(restoreError)}); restore ${backupPath} manually`;
    }
    throw new OperationsError(
      "migration-failed",
      `database migration failed: ${errorMessage(error)}; ${recovery}`,
      { cause: error },
    );
  }

  const after = inspectDatabase(database);
  if (!after.integrityOk || after.schemaVersion !== SUPPORTED_SCHEMA_VERSION || after.ledgerProblem !== null) {
    let recovery: string;
    try {
      await restoreDatabase({
        input: backupPath,
        database,
        replace: true,
        confirmStopped: true,
        now,
      });
      recovery = `restored the pre-migration backup ${backupPath}`;
    } catch (restoreError) {
      recovery = `automatic restore failed (${errorMessage(restoreError)}); restore ${backupPath} manually`;
    }
    throw new OperationsError(
      "migration-failed",
      `post-migration verification failed (${after.integrity}; schema ${after.schemaVersion}); ${recovery}`,
    );
  }
  return {
    database,
    fromSchemaVersion: before.schemaVersion,
    toSchemaVersion: after.schemaVersion,
    migrated: true,
    backupPath: backup.output,
    integrity: after.integrity,
  };
}

/**
 * Discard telemetry history only. This is the explicit implementation path for
 * switching an existing installation to memory mode or reclaiming a large
 * historical database; it never drops the whole file.
 *
 * Preserved: users, sessions/audit control tables, collector registry/grants,
 * alert rules/incidents/jobs/webhook state, machine metadata and inventory,
 * recordings (explicit user output) and configuration.
 */
export function resetTelemetryHistory(options: ResetTelemetryOptions): ResetTelemetryReport {
  const database = resolvePath(options.database, "database");
  assertPrivateRegularFile(database, "database");
  if (!options.confirmStopped) {
    throw new OperationsError(
      "confirmation-required",
      "resetting telemetry history requires --confirm-stopped after the hub is stopped",
    );
  }
  assertNoActiveHubLock(database);

  // Opening with the hub migrator also creates the summary tier on an older
  // schema. The reset itself is one transaction, so a failure leaves history
  // unchanged.
  const db = openHubDatabase(database);
  const tables = [
    "points_raw",
    "points_10s",
    "points_1m",
    "points_1h",
    "sample_receipts",
    "gaps",
    "machine_latest",
    "series",
  ] as const;
  const deleted: Record<string, number> = {};
  try {
    db.db.exec("BEGIN IMMEDIATE");
    try {
      for (const table of tables) {
        const result = db.db.prepare(`DELETE FROM ${table}`).run();
        deleted[table] = Number(result.changes);
      }
      db.db.exec("COMMIT");
    } catch (error) {
      try {
        db.db.exec("ROLLBACK");
      } catch {
        // Preserve the original failure.
      }
      throw error;
    }
    db.reclaimPages(1024);
    db.checkpoint();
  } finally {
    db.close();
  }
  const info = inspectDatabase(database);
  return {
    database,
    deleted,
    integrity: info.integrity,
    preserved: [
      "users",
      "collector registry/grants/audit",
      "alert rules/incidents/jobs/webhook",
      "machine metadata",
      "inventory",
      "recordings",
    ],
  };
}

/** Read the schema version without opening the migrator (used by the installer). */
export function schemaVersionOf(database: string): number | null {
  if (!existsSync(database)) return null;
  const info = inspectDatabase(database);
  return info.hasSchemaMigrations ? info.schemaVersion : null;
}

export function removeDatabaseFiles(database: string): void {
  for (const path of [database, `${database}-wal`, `${database}-shm`]) removeIfExists(path);
}

export function databaseFileBytes(database: string): number {
  return fileSize(database) + fileSize(`${database}-wal`);
}
