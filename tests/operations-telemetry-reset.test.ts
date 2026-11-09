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
import { clearMachineHistory, resetTelemetryHistory } from "../src/operations/database";
import { installAlertSchema } from "../src/hub/alerts/schema";

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

/**
 * The one-time machine-data transition of `rigscope upgrade` discards the
 * previous release's machine-derived tables. It is not one-time forever: the
 * transition marker makes only the first post-RAM-first upgrade clear, and
 * later upgrades preserve durable history. Machine display names and tags
 * (`machine_meta`) describe the enrolled machines rather than their discarded
 * measurements, so they always survive.
 */
describe("upgrade machine-data cleanup", () => {
  function seededDatabase(dir: string): string {
    const path = join(dir, "hub.db");
    const db = openHubDatabase(path);
    try {
      installAlertSchema(db.db);
      db.db.prepare(
        "INSERT INTO users (username, password_hash, scope_json, is_admin) VALUES (?, ?, '*', 1)",
      ).run("owner", "scrypt$fake");
      db.db.prepare(
        "INSERT INTO collector_registry (collector_id, machine_id, token, created_at_ms, updated_at_ms) " +
          "VALUES (?, ?, ?, 1, 1)",
      ).run("collector-1", "machine-1", "token-1");
      db.db.prepare(
        "INSERT INTO alert_rules (id, name, machine_id, metric, condition, threshold, duration_ms, enabled, " +
          "state, created_at_ms, updated_at_ms) VALUES ('rule-1', 'CPU high', 'machine-1', 'cpu.usage', '>', 90, 0, 1, 'ok', 1, 1)",
      ).run();
      db.db.prepare("INSERT INTO alert_webhook (id, url, updated_at_ms) VALUES (1, 'https://example.invalid/hook', 1)").run();
      db.db.prepare(
        "INSERT INTO alert_incidents (id, rule_id, rule_name, machine_id, metric, condition, status, started_at_ms, " +
          "last_observed_at_ms, message) VALUES ('incident-1', 'rule-1', 'CPU high', 'machine-1', 'cpu.usage', '>', 'open', 1, 1, 'high')",
      ).run();
      db.db.prepare(
        "INSERT INTO alert_machine_progress (machine_id, collector_id, run_id, seq, received_at_ms, updated_at_ms) " +
          "VALUES ('machine-1', 'collector-1', 'run-1', 1, 1, 1)",
      ).run();
      db.db.prepare(
        "INSERT INTO alert_observed_resources (rule_id, metric, resource_id, first_seen_ms, last_seen_ms) " +
          "VALUES ('rule-1', 'disk.usage', 'nvme0n1', 1, 1)",
      ).run();
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
      db.db.prepare("INSERT INTO points_1h (series_id, bucket_ms, min, max, sum, count) VALUES (?, 0, 1, 1, 1, 1)").run(series);
      db.db.prepare(
        "INSERT INTO sample_receipts (collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms) " +
          "VALUES ('collector-1', 'run-1', 1, 'machine-1', 'metrics', 'live', 1000, 1000)",
      ).run();
      db.db.prepare(
        "INSERT INTO machine_latest (machine_id, collector_id, run_id, boot_id, seq, captured_at_ms, received_at_ms, payload_json) " +
          "VALUES ('machine-1', 'collector-1', 'run-1', 'boot-1', 1, 1000, 1000, '{}')",
      ).run();
      return path;
    } finally {
      db.close();
    }
  }

  it("removes machine history while preserving accounts, enrollment, alert rules and webhook settings", () => {
    const dir = mkdtempSync(join(tmpdir(), "rigscope-upgrade-clean-"));
    try {
      const path = seededDatabase(dir);
      const report = clearMachineHistory(path);
      assert.equal(report.integrity, "ok");
      assert.equal(report.deleted.points_raw, 1);
      assert.equal(report.deleted.points_1h, 1);
      assert.equal(report.deleted.sample_receipts, 1);
      assert.equal("machine_meta" in report.deleted, false, "machine display names/tags are never cleared");
      assert.equal(report.deleted.inventory, 1);
      assert.equal(report.deleted.alert_incidents, 1);
      assert.equal(report.deleted.alert_machine_progress, 1);
      assert.equal(report.deleted.alert_observed_resources, 1);
      assert.equal(report.deleted.series, 1);

      const db = openHubDatabase(path);
      try {
        for (const table of [
          "points_raw", "points_10s", "points_1m", "points_1h", "sample_receipts", "gaps",
          "machine_latest", "inventory", "series",
          "alert_incidents", "alert_machine_progress", "alert_observed_resources",
        ]) {
          assert.equal(scalar(db, `SELECT COUNT(*) AS n FROM ${table}`), 0, `${table} should be empty`);
        }
        // Everything that describes the installation, not the machines, survives.
        assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM users"), 1);
        assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM collector_registry"), 1);
        assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM alert_rules"), 1);
        assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM alert_webhook"), 1);
        assert.equal(scalar(db, "SELECT COUNT(*) AS n FROM machine_meta"), 1, "machine names/tags survive the transition");
      } finally {
        db.close();
      }

      assert.deepEqual(clearMachineHistory(path).deleted, {
        points_raw: 0, points_10s: 0, points_1m: 0, points_1h: 0, sample_receipts: 0, gaps: 0,
        machine_latest: 0, inventory: 0, alert_observed_resources: 0,
        alert_machine_progress: 0, alert_incidents: 0, series: 0,
      }, "a re-attempt over an already-clean database is a no-op");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("skips tables the previous release never created, and a missing database", () => {
    const dir = mkdtempSync(join(tmpdir(), "rigscope-upgrade-clean-"));
    try {
      const path = join(dir, "hub.db");
      const db = openHubDatabase(path);
      db.db.prepare(
        "INSERT INTO collector_registry (collector_id, machine_id, token, created_at_ms, updated_at_ms) " +
          "VALUES (?, ?, ?, 1, 1)",
      ).run("collector-1", "machine-1", "token-1");
      // Simulate a database whose shape predates a table: clearing must skip it
      // rather than fail the upgrade that is already activated.
      db.db.exec("DROP TABLE alert_incidents");
      db.close();

      const report = clearMachineHistory(path);
      assert.equal(report.integrity, "ok");
      assert.equal("alert_incidents" in report.deleted, false, "an absent table is skipped, not deleted");
      assert.equal(report.deleted.series, 0);
      assert.equal(clearMachineHistory(path).integrity, "ok");

      const absent = clearMachineHistory(join(dir, "no-such.db"));
      assert.deepEqual(absent.deleted, {});
      assert.equal(absent.integrity, "absent");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
