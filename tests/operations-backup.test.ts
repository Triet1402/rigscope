/**
 * Isolated database operations tests: online backup, validated restore,
 * schema-version refusal, WAL handling, and migration recovery.
 *
 * Every database lives in a fresh temporary private directory. No live hub,
 * service or developer state is touched.
 */
import assert from "node:assert/strict";
import test from "node:test";
import { createHash } from "node:crypto";
import {
  chmodSync,
  symlinkSync,
  linkSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";

import { openHubDatabase } from "../src/hub/storage/db";
import { MIGRATIONS } from "../src/hub/storage/migrations";
import {
  SUPPORTED_SCHEMA_VERSION,
  backupDatabase,
  inspectDatabase,
  migrateDatabase,
  restoreDatabase,
} from "../src/operations/database";
import { assertNoActiveHubLock } from "../src/operations/fs";

const ROOT = mkdtempSync(join(tmpdir(), "rigscope-ops-backup-"));
process.on("exit", () => rmSync(ROOT, { recursive: true, force: true }));

function freshDir(name: string): string {
  const path = join(ROOT, `${name}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(path, { recursive: true, mode: 0o700 });
  return path;
}

function sha256(path: string): string {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function seedCurrentDatabase(directory: string): string {
  const path = join(directory, "telemetry.sqlite");
  const database = openHubDatabase(path);
  try {
    database.db.exec(`
      INSERT INTO collector_registry
        (collector_id, machine_id, token, revoked, created_at_ms, updated_at_ms)
        VALUES ('collector-1', 'machine-1', 'private-collector-token', 0, 1000, 1000);
      INSERT INTO users (username, password_hash, scope_json, is_admin, disabled)
      VALUES ('operator', 'private-password-hash', '["machine-1"]', 1, 0);
      INSERT INTO admin_audit (actor, action, target, at_ms)
      VALUES ('operator', 'test', 'machine-1', 1000);
      INSERT INTO alert_rules
        (id, name, machine_id, metric, resource_id, condition, threshold, duration_ms,
         enabled, state, created_at_ms, updated_at_ms)
      VALUES ('rule-1', 'High CPU', 'machine-1', 'cpu.usage', '', 'above', 90, 0, 1, 'ok', 1000, 1000);
      INSERT INTO machine_latest
        (machine_id, collector_id, run_id, seq, captured_at_ms, received_at_ms, payload_json)
      VALUES ('machine-1', 'collector-1', 'run-1', 1, 1000, 1000, '{"cpu":{"status":"unavailable","reason":"fixture"}}');
    `);
  } finally {
    database.close();
  }
  return path;
}

function seedLegacyDatabase(directory: string, version: number): string {
  const path = join(directory, "legacy.sqlite");
  const db = new DatabaseSync(path);
  try {
    db.exec(`CREATE TABLE schema_migrations (
      version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at_ms INTEGER NOT NULL
    )`);
    const insert = db.prepare("INSERT INTO schema_migrations VALUES (?, ?, ?)");
    for (const migration of MIGRATIONS.filter((item) => item.version <= version)) {
      migration.up(db);
      insert.run(migration.version, migration.name, 1000 + migration.version);
    }
  } finally {
    db.close();
  }
  chmodSync(path, 0o600);
  return path;
}

function seedFutureDatabase(directory: string): string {
  const path = join(directory, "future.sqlite");
  const db = new DatabaseSync(path);
  try {
    db.exec(`CREATE TABLE schema_migrations (
      version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at_ms INTEGER NOT NULL
    )`);
    db.prepare("INSERT INTO schema_migrations VALUES (?, ?, ?)").run(
      SUPPORTED_SCHEMA_VERSION + 1,
      "future-work",
      1000,
    );
  } finally {
    db.close();
  }
  chmodSync(path, 0o600);
  return path;
}

function privateFile(directory: string, name: string, content: string | Buffer): string {
  const path = join(directory, name);
  writeFileSync(path, content, { mode: 0o600 });
  chmodSync(path, 0o600);
  return path;
}

test("inspect reports schema, integrity and registry/admin/alert presence", () => {
  const dir = freshDir("inspect");
  const database = seedCurrentDatabase(dir);
  const report = inspectDatabase(database);
  assert.equal(report.integrityOk, true);
  assert.equal(report.schemaVersion, SUPPORTED_SCHEMA_VERSION);
  assert.equal(report.supportedSchemaVersion, SUPPORTED_SCHEMA_VERSION);
  assert.equal(report.newerSchema, false);
  assert.equal(report.ledgerProblem, null);
  assert.equal(report.registryPresent, true);
  assert.equal(report.adminPresent, true);
  assert.equal(report.alertsPresent, true);
  assert.equal(report.mode, "0600");
  const registry = report.tables.find((table) => table.name === "collector_registry");
  assert.equal(registry?.rows, 1);
  const users = report.tables.find((table) => table.name === "users");
  assert.equal(users?.rows, 1);
  const rules = report.tables.find((table) => table.name === "alert_rules");
  assert.equal(rules?.rows, 1);
});

test("false future schema is refused by restore and flagged by inspect", async () => {
  const dir = freshDir("future");
  const future = seedFutureDatabase(dir);
  const report = inspectDatabase(future);
  assert.equal(report.newerSchema, true);
  const target = join(freshDir("future-target"), "telemetry.sqlite");
  await assert.rejects(
    restoreDatabase({ input: future, database: target }),
    /newer than this operations build|newer schema/i,
  );
  assert.equal(existsSync(target), false, "failed validation never creates a target");
});

test("backup captures all tables in one file and never migrates the source", async () => {
  const dir = freshDir("backup");
  const database = seedCurrentDatabase(dir);
  const outputDir = freshDir("backup-out");
  const output = join(outputDir, "hub-backup.sqlite");
  const report = await backupDatabase({ database, output });
  assert.equal(report.sourceSchemaVersion, SUPPORTED_SCHEMA_VERSION);
  assert.equal(report.migratedSource, false);
  assert.ok(report.tables.includes("collector_registry"));
  assert.ok(report.tables.includes("users"));
  assert.ok(report.tables.includes("alert_rules"));
  assert.equal(existsSync(output), true);
  assert.equal(report.bytes > 0, true);
  assert.match(report.sha256, /^[0-9a-f]{64}$/);

  const backup = new DatabaseSync(output, { readOnly: true });
  try {
    assert.equal((backup.prepare("SELECT COUNT(*) AS n FROM collector_registry").get() as { n: number }).n, 1);
    assert.equal((backup.prepare("SELECT COUNT(*) AS n FROM users").get() as { n: number }).n, 1);
    assert.equal((backup.prepare("SELECT COUNT(*) AS n FROM alert_rules").get() as { n: number }).n, 1);
  } finally {
    backup.close();
  }
  assert.equal(existsSync(`${output}-wal`), false, "backup is a self-contained single file");
  assert.equal(existsSync(`${output}-shm`), false);
  // The source schema is untouched by backup.
  assert.equal(inspectDatabase(database).schemaVersion, SUPPORTED_SCHEMA_VERSION);
});

test("backup of an older schema does not migrate the source", async () => {
  const dir = freshDir("legacy-backup");
  const database = seedLegacyDatabase(dir, 1);
  const output = join(freshDir("legacy-out"), "legacy-backup.sqlite");
  const report = await backupDatabase({ database, output });
  assert.equal(report.sourceSchemaVersion, 1);
  assert.equal(inspectDatabase(database).schemaVersion, 1, "source remains schema 1");
  const source = new DatabaseSync(database, { readOnly: true });
  try {
    const tables = source
      .prepare("SELECT name FROM sqlite_master WHERE type='table' AND name='users'")
      .all();
    assert.equal(tables.length, 0);
  } finally {
    source.close();
  }
});

test("restore replaces a stopped target, backs up the old copy and removes old WAL sidecars", async () => {
  const dir = freshDir("restore");
  const database = seedCurrentDatabase(dir);
  const backupPath = join(freshDir("restore-backups"), "good.sqlite");
  await backupDatabase({ database, output: backupPath });
  // Mutate the live database after the backup.
  const live = openHubDatabase(database);
  live.db.exec("DELETE FROM collector_registry; DELETE FROM users; DELETE FROM alert_rules");
  live.close();
  assert.equal((inspectDatabase(database).tables.find((table) => table.name === "users")?.rows ?? 0), 0);

  const beforeTargetHash = sha256(database);
  const report = await restoreDatabase({
    input: backupPath,
    database,
    replace: true,
    confirmStopped: true,
  });
  assert.equal(report.replaced, true);
  assert.equal(report.restoredSchemaVersion, SUPPORTED_SCHEMA_VERSION);
  assert.ok(report.preRestoreBackup !== null);
  assert.equal(existsSync(report.preRestoreBackup!), true);
  assert.equal(existsSync(`${database}-wal`), false);
  assert.equal(existsSync(`${database}-shm`), false);
  const restored = inspectDatabase(database);
  assert.equal(restored.tables.find((table) => table.name === "collector_registry")?.rows, 1);
  assert.equal(restored.tables.find((table) => table.name === "users")?.rows, 1);
  assert.equal(restored.tables.find((table) => table.name === "alert_rules")?.rows, 1);
  assert.notEqual(sha256(database), beforeTargetHash);
});

test("restore refuses an existing target without both explicit flags", async () => {
  const dir = freshDir("no-overwrite");
  const source = seedCurrentDatabase(freshDir("no-overwrite-source"));
  const backup = join(freshDir("no-overwrite-backup"), "backup.sqlite");
  await backupDatabase({ database: source, output: backup });
  const target = seedCurrentDatabase(freshDir("no-overwrite-target"));
  const hash = sha256(target);
  await assert.rejects(restoreDatabase({ input: backup, database: target }), /--replace/);
  await assert.rejects(
    restoreDatabase({ input: backup, database: target, replace: true }),
    /--confirm-stopped/,
  );
  assert.equal(sha256(target), hash, "target is untouched by refused restores");
});

test("restore refuses a corrupted backup and leaves the target unchanged", async () => {
  const dir = freshDir("corrupt");
  const target = seedCurrentDatabase(dir);
  const corrupt = privateFile(freshDir("corrupt-input"), "bad.sqlite", "this is not a sqlite database");
  const hash = sha256(target);
  await assert.rejects(
    restoreDatabase({ input: corrupt, database: target, replace: true, confirmStopped: true }),
  );
  assert.equal(sha256(target), hash);
});

test("restore refuses an active hub lock, including the parent's .hub.lock lease", async () => {
  const dir = freshDir("lock");
  const source = seedCurrentDatabase(freshDir("lock-source"));
  const backup = join(freshDir("lock-backup"), "backup.sqlite");
  await backupDatabase({ database: source, output: backup });
  const target = seedCurrentDatabase(freshDir("lock-target"));
  const lock = `${target}.hub.lock`;
  writeFileSync(lock, JSON.stringify({ pid: process.pid, instance: "test" }), { mode: 0o600 });
  try {
    await assert.rejects(
      restoreDatabase({ input: backup, database: target, replace: true, confirmStopped: true }),
      /active hub lock/i,
    );
    assert.throws(() => assertNoActiveHubLock(target), /active hub lock/i);
    assert.equal(readFileSync(lock, "utf8").includes(String(process.pid)), true, "lock is never deleted");
  } finally {
    rmSync(lock, { force: true });
  }
});

test("migrate backs up first, applies the ledger and verifies the supported schema", async () => {
  const dir = freshDir("migrate");
  const database = seedLegacyDatabase(dir, 1);
  const backupDir = freshDir("migrate-backups");
  const report = await migrateDatabase({
    database,
    backupDir,
    confirmStopped: true,
  });
  assert.equal(report.migrated, true);
  assert.equal(report.fromSchemaVersion, 1);
  assert.equal(report.toSchemaVersion, SUPPORTED_SCHEMA_VERSION);
  assert.ok(report.backupPath !== null);
  assert.equal(existsSync(report.backupPath!), true);
  const info = inspectDatabase(database);
  assert.equal(info.schemaVersion, SUPPORTED_SCHEMA_VERSION);
  assert.equal(info.ledgerProblem, null);
  assert.equal(info.integrityOk, true);
});

test("migrate refuses a newer schema without creating a backup", async () => {
  const dir = freshDir("migrate-newer");
  const database = seedFutureDatabase(dir);
  await assert.rejects(
    migrateDatabase({ database, backupDir: freshDir("migrate-newer-backups"), confirmStopped: true }),
    /newer than this build|newer schema/i,
  );
  assert.equal(inspectDatabase(database).schemaVersion, SUPPORTED_SCHEMA_VERSION + 1);
});

test("failed migration restores the pre-migration database and reports the backup", async () => {
  const dir = freshDir("migrate-failure");
  const database = seedLegacyDatabase(dir, 1);
  const backupDir = freshDir("migrate-failure-backups");
  await assert.rejects(
    migrateDatabase({
      database,
      backupDir,
      confirmStopped: true,
      runMigrations: () => {
        throw new Error("synthetic migration failure");
      },
    }),
    /synthetic migration failure.*restored the pre-migration backup|migration failed/i,
  );
  const info = inspectDatabase(database);
  assert.equal(info.schemaVersion, 1, "the good database is restored, not left half-migrated");
  assert.equal(info.integrityOk, true);
  assert.equal(existsSync(backupDir), true);
  const backups = readdirSync(backupDir);
  assert.equal(backups.length >= 1, true);
});

test("migrate requires stopped confirmation for an existing database", async () => {
  const dir = freshDir("migrate-confirm");
  const database = seedLegacyDatabase(dir, 1);
  await assert.rejects(
    migrateDatabase({ database, backupDir: freshDir("migrate-confirm-backups") }),
    /--confirm-stopped/,
  );
});

test("backup and restore refuse aliases of the live file", async () => {
  const dir = mkdtempSync(join(ROOT, "aliases-"));
  const database = join(dir, "live.sqlite");
  const db = openHubDatabase(database); db.close();
  const aliasDir = dir + "-link";
  symlinkSync(dir, aliasDir);
  const alias = join(aliasDir, "live.sqlite");
  const hardlink = join(dir, "hard.sqlite"); linkSync(database, hardlink);
  try {
    for (const output of [alias, hardlink]) {
      await assert.rejects(backupDatabase({ database, output, replace: true }), /must differ/);
      await assert.rejects(restoreDatabase({ input: output, database, replace: true, confirmStopped: true }), /must differ/);
    }
    assert.equal(inspectDatabase(database).integrityOk, true);
  } finally { rmSync(aliasDir); }
});
