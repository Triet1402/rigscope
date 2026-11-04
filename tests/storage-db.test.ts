import { strict as assert } from "node:assert";
import { mkdirSync, mkdtempSync, rmSync, statSync, writeFileSync, chmodSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { describe, it } from "node:test";
import { openHubDatabase } from "../src/hub/storage/db";
import { MIGRATIONS } from "../src/hub/storage/migrations";

function tempDir(): string {
  return mkdtempSync(join(tmpdir(), "rigscope-storage-db-"));
}

describe("hub database", () => {
  it("creates a private file, runs migration 1 and is idempotent", () => {
    const dir = tempDir();
    const dbPath = join(dir, "nested", "hub.db");
    try {
      const db = openHubDatabase(dbPath);
      assert.equal(db.schemaVersion(), 5);
      assert.equal(statSync(dbPath).mode & 0o777, 0o600);
      assert.equal(statSync(join(dir, "nested")).mode & 0o777, 0o700);
      const migrations = db.db.prepare("SELECT version, name FROM schema_migrations ORDER BY version").all();
      assert.deepEqual(
        migrations.map((row) => ({ version: Number(row.version), name: String(row.name) })),
        [
          { version: 1, name: "telemetry" },
          { version: 2, name: "latest_session_identity" },
          { version: 3, name: "admin" },
          { version: 4, name: "alerts" },
          { version: 5, name: "collector_registry" },
        ],
      );
      db.close();

      // Reopening must not rerun either migration nor change the recorded rows.
      const reopened = openHubDatabase(dbPath);
      assert.equal(reopened.schemaVersion(), 5);
      assert.equal(
        (reopened.db.prepare("SELECT COUNT(*) AS n FROM schema_migrations").get() as { n: number }).n,
        5,
      );
      reopened.close();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("upgrades a version-1 database in place, preserving latest rows", () => {
    const dir = tempDir();
    const dbPath = join(dir, "telemetry.db");
    try {
      const raw = new DatabaseSync(dbPath);
      raw.exec(
        "CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at_ms INTEGER NOT NULL)",
      );
      raw.exec("BEGIN");
      MIGRATIONS[0]!.up(raw);
      raw.prepare("INSERT INTO schema_migrations VALUES (1, 'telemetry', 0)").run();
      raw
        .prepare(
          "INSERT INTO machine_latest (machine_id, collector_id, run_id, seq, captured_at_ms, received_at_ms, payload_json) VALUES (?, ?, ?, ?, ?, ?, ?)",
        )
        .run("machine-1", "collector-1", "run-1", 5, 1000, 2000, "{}");
      raw.exec("COMMIT");
      raw.close();
      chmodSync(dbPath, 0o600);

      const upgraded = openHubDatabase(dbPath);
      try {
        assert.equal(upgraded.schemaVersion(), 5);
        const columns = new Set(
          (upgraded.db.prepare("PRAGMA table_info(machine_latest)").all() as Array<{ name: string }>).map(
            (column) => column.name,
          ),
        );
        assert.ok(columns.has("boot_id"));
        assert.ok(columns.has("hostname"));
        assert.ok(columns.has("expected_interval_ms"));
        const row = upgraded.db
          .prepare("SELECT boot_id, seq FROM machine_latest WHERE machine_id = ?")
          .get("machine-1") as { boot_id: string; seq: number };
        assert.equal(row.boot_id, "");
        assert.equal(Number(row.seq), 5);
      } finally {
        upgraded.close();
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("refuses a schema newer than this build", () => {
    const dir = tempDir();
    const dbPath = join(dir, "future.db");
    try {
      const raw = new DatabaseSync(dbPath);
      raw.exec(`
        CREATE TABLE schema_migrations (
          version INTEGER PRIMARY KEY,
          name TEXT NOT NULL,
          applied_at_ms INTEGER NOT NULL
        );
        INSERT INTO schema_migrations VALUES (1, 'telemetry', 0);
        INSERT INTO schema_migrations VALUES (99, 'from-the-future', 0);
      `);
      raw.close();
      chmodSync(dbPath, 0o600);
      assert.throws(() => openHubDatabase(dbPath), /newer than this hub knows/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("refuses a non-contiguous or renamed migration history", () => {
    const dir = tempDir();
    try {
      const holePath = join(dir, "hole.db");
      const hole = new DatabaseSync(holePath);
      hole.exec(`
        CREATE TABLE schema_migrations (
          version INTEGER PRIMARY KEY,
          name TEXT NOT NULL,
          applied_at_ms INTEGER NOT NULL
        );
        INSERT INTO schema_migrations VALUES (1, 'telemetry', 0);
        INSERT INTO schema_migrations VALUES (3, 'unknown', 0);
      `);
      hole.close();
      chmodSync(holePath, 0o600);
      assert.throws(() => openHubDatabase(holePath), /not contiguous/);

      const renamedPath = join(dir, "renamed.db");
      const renamed = new DatabaseSync(renamedPath);
      renamed.exec(`
        CREATE TABLE schema_migrations (
          version INTEGER PRIMARY KEY,
          name TEXT NOT NULL,
          applied_at_ms INTEGER NOT NULL
        );
        INSERT INTO schema_migrations VALUES (1, 'something-else', 0);
      `);
      renamed.close();
      chmodSync(renamedPath, 0o600);
      assert.throws(() => openHubDatabase(renamedPath), /named/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("fails closed on a non-private or non-regular path", () => {
    const dir = tempDir();
    try {
      const loose = join(dir, "loose.db");
      writeFileSync(loose, "not a database", { mode: 0o644 });
      chmodSync(loose, 0o644);
      assert.throws(() => openHubDatabase(loose), /must be private/);

      const directoryPath = join(dir, "a-directory");
      mkdirSync(directoryPath);
      assert.throws(() => openHubDatabase(directoryPath), /regular file/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("backs up consistently to a private destination and reports size", async () => {
    const dir = tempDir();
    const dbPath = join(dir, "hub.db");
    const backupPath = join(dir, "backups", "copy.db");
    try {
      const db = openHubDatabase(dbPath);
      db.db.prepare(
        "INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, ?, ?, ?)",
      ).run("machine-1", "Workstation", '["desk"]', 123);
      assert.ok(db.sizeBytes() > 0);
      db.checkpoint();
      await db.backupTo(backupPath);
      assert.equal(statSync(backupPath).mode & 0o777, 0o600);

      const copy = new DatabaseSync(backupPath);
      const row = copy.prepare("SELECT display_name FROM machine_meta WHERE machine_id = ?").get("machine-1") as {
        display_name: string;
      };
      assert.equal(row.display_name, "Workstation");
      copy.close();
      db.close();

      // A later backup over the same name replaces the old copy.
      const reopened = openHubDatabase(dbPath);
      await reopened.backupTo(backupPath);
      reopened.close();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("accepts :memory: for tests", () => {
    const db = openHubDatabase(":memory:");
    assert.equal(db.schemaVersion(), 5);
    assert.ok(db.sizeBytes() > 0);
    db.close();
  });
});
