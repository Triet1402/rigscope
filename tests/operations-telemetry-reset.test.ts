/**
 * The explicit history reset must remove only telemetry tables. Users,
 * collector enrollment, presentation metadata and inventory must survive.
 */

import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";

import { openHubDatabase } from "../src/hub/storage/db";
import { resetTelemetryHistory } from "../src/operations/database";

function scalar(db: ReturnType<typeof openHubDatabase>, sql: string): number {
  return Number((db.db.prepare(sql).get() as { n: number | bigint }).n);
}

describe("telemetry history reset", () => {
  it("deletes history and dedup state while preserving accounts, enrollment and metadata", () => {
    const dir = mkdtempSync(join(tmpdir(), "rigscope-reset-"));
    const path = join(dir, "hub.db");
    let db = openHubDatabase(path);
    try {
      db.db.prepare(
        "INSERT INTO users (username, password_hash, scope_json, is_admin) VALUES (?, ?, '*', 1)",
      ).run("owner", "scrypt$fake");
      db.db.prepare(
        "INSERT INTO collector_registry (collector_id, machine_id, token, created_at_ms, updated_at_ms) " +
          "VALUES (?, ?, ?, 1, 1)",
      ).run("collector-1", "machine-1", "token-1");
      db.db.prepare(
        "INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, ?, ?, 1)",
      ).run("machine-1", "Workstation", '["lab"]');
      db.db.prepare(
        "INSERT INTO inventory (machine_id, captured_at_ms, received_at_ms, inventory_json) VALUES (?, 1, 1, '{}')",
      ).run("machine-1");

      const series = Number(
        db.db.prepare("INSERT INTO series (machine_id, metric, resource_id) VALUES ('machine-1', 'cpu.usage', '')")
          .run().lastInsertRowid,
      );
      db.db.prepare("INSERT INTO points_raw (series_id, ts_ms, value) VALUES (?, 1000, 1)").run(series);
      db.db.prepare("INSERT INTO points_10s (series_id, bucket_ms, min, max, sum, count) VALUES (?, 1000, 1, 1, 1, 1)").run(series);
      db.db.prepare("INSERT INTO points_1m (series_id, bucket_ms, min, max, sum, count) VALUES (?, 0, 1, 1, 1, 1)").run(series);
      db.db.prepare("INSERT INTO points_1h (series_id, bucket_ms, min, max, sum, count) VALUES (?, 0, 1, 1, 1, 1)").run(series);
      db.db.prepare(
        "INSERT INTO sample_receipts (collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms) " +
          "VALUES ('collector-1', 'run-1', 1, 'machine-1', 'metrics', 'live', 1000, 1000)",
      ).run();
      db.db.prepare(
        "INSERT INTO machine_latest (machine_id, collector_id, run_id, boot_id, seq, captured_at_ms, received_at_ms, payload_json) " +
          "VALUES ('machine-1', 'collector-1', 'run-1', 'boot-1', 1, 1000, 1000, '{}')",
      ).run();

      assert.throws(
        () => resetTelemetryHistory({ database: path }),
        /confirm-stopped/,
      );
      const report = resetTelemetryHistory({ database: path, confirmStopped: true });
      assert.equal(report.integrity, "ok");
      assert.equal(report.deleted.points_raw, 1);
      assert.equal(report.deleted.points_10s, 1);
      assert.equal(report.deleted.sample_receipts, 1);

      db.close();
      db = openHubDatabase(path);
      for (const table of ["points_raw", "points_10s", "points_1m", "points_1h", "sample_receipts", "gaps", "machine_latest", "series"]) {
        assert.equal(scalar(db, `SELECT COUNT(*) AS n FROM ${table}`), 0, `${table} should be empty`);
      }
      assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM users"), 1);
      assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM collector_registry"), 1);
      assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM machine_meta"), 1);
      assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM inventory"), 1);
    } finally {
      db.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
