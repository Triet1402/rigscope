import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";
import { openHubDatabase, type HubDatabase } from "../src/hub/storage/db";
import {
  DEFAULT_RETENTION_POLICY,
  RETENTION_DELETE_SQL,
  RetentionManager,
  resolveRetentionPolicy,
} from "../src/hub/storage/retention";

const NOW = Date.parse("2026-11-02T12:00:00Z");
const HOUR = 3_600_000;

function setup(options: {
  policy?: Parameters<typeof resolveRetentionPolicy>[0];
  maxDatabaseBytes?: number | null;
  chunkSize?: number;
  intervalMs?: number;
}): { dir: string; db: HubDatabase; manager: RetentionManager; now: number } {
  const dir = mkdtempSync(join(tmpdir(), "rigscope-storage-retention-"));
  const db = openHubDatabase(join(dir, "hub.db"));
  const manager = new RetentionManager(db, {
    policy: options.policy,
    maxDatabaseBytes: options.maxDatabaseBytes ?? null,
    now: () => NOW,
    chunkSize: options.chunkSize,
    intervalMs: options.intervalMs,
    maxChunksPerPass: 500,
  });
  return { dir, db, manager, now: NOW };
}

function makeSeries(db: HubDatabase, metric = "cpu.usage"): number {
  const result = db.db
    .prepare("INSERT INTO series (machine_id, metric, resource_id) VALUES (?, ?, '')")
    .run("machine-1", metric);
  return Number(result.lastInsertRowid);
}

function insertRaw(db: HubDatabase, seriesId: number, tsMs: number, value: number | null = 1): void {
  db.db.prepare("INSERT INTO points_raw (series_id, ts_ms, value) VALUES (?, ?, ?)").run(seriesId, tsMs, value);
}

function insert1m(db: HubDatabase, seriesId: number, bucketMs: number): void {
  db.db
    .prepare(
      "INSERT INTO points_1m (series_id, bucket_ms, min, max, sum, count, null_count) VALUES (?, ?, 1, 1, 1, 1, 0)",
    )
    .run(seriesId, bucketMs);
}

function insert1h(db: HubDatabase, seriesId: number, bucketMs: number): void {
  db.db
    .prepare(
      "INSERT INTO points_1h (series_id, bucket_ms, min, max, sum, count, null_count) VALUES (?, ?, 1, 1, 1, 1, 0)",
    )
    .run(seriesId, bucketMs);
}

function count(db: HubDatabase, table: string): number {
  return Number((db.db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get() as { n: number }).n);
}

describe("retention", () => {
  it("rolls back the whole bounded tier chunk if a later series delete fails", () => {
    const ctx = setup({ policy: { rawMs: 1000 }, chunkSize: 10 });
    try {
      const first = makeSeries(ctx.db, "first");
      const second = makeSeries(ctx.db, "second");
      insertRaw(ctx.db, first, NOW - 2000);
      insertRaw(ctx.db, second, NOW - 2000);
      ctx.db.db.exec(`CREATE TRIGGER fail_second BEFORE DELETE ON points_raw
        WHEN OLD.series_id = ${second}
        BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`);
      assert.throws(() => ctx.manager.runChunk(), /injected delete failure/);
      assert.equal(count(ctx.db, "points_raw"), 2, "earlier series must share the transaction");
      ctx.db.db.exec("DROP TRIGGER fail_second");
      assert.equal(ctx.manager.runChunk(), 2, "rollback must release the transaction");
    } finally {
      ctx.db.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("merges a partial policy, including receipts/gaps defaults", () => {
    const merged = resolveRetentionPolicy({ rawMs: 1000 });
    assert.equal(merged.rawMs, 1000);
    assert.equal(merged.receiptsMs, 1000);
    assert.equal(merged.gapsMs, DEFAULT_RETENTION_POLICY.gapsMs);
    assert.throws(() => resolveRetentionPolicy({ rawMs: -1 }), /nonnegative/);
  });

  it("deletes expired rows in bounded chunks until drained", async () => {
    const ctx = setup({
      policy: {
        rawMs: 1000,
        oneMinuteMs: 1000,
        oneHourMs: 1000,
        receiptsMs: 1000,
        gapsMs: 1000,
      },
      chunkSize: 1000,
    });
    try {
      const series = makeSeries(ctx.db);
      const insert = ctx.db.db.prepare("INSERT INTO points_raw (series_id, ts_ms, value) VALUES (?, ?, 1)");
      ctx.db.db.exec("BEGIN");
      for (let index = 0; index < 12_000; index += 1) {
        insert.run(series, NOW - 10_000 - index);
      }
      ctx.db.db.exec("COMMIT");

      let total = 0;
      let calls = 0;
      let largestStatement = 0;
      for (;;) {
        const deleted = ctx.manager.runChunk();
        largestStatement = Math.max(largestStatement, deleted);
        if (deleted === 0) break;
        assert.ok(deleted <= 1000, "one DELETE statement must not exceed chunkSize");
        total += deleted;
        calls += 1;
        assert.ok(calls < 100, "retention should converge");
      }
      assert.equal(total, 12_000);
      assert.equal(largestStatement, 1000);
      assert.ok(calls >= 12);
      assert.equal(count(ctx.db, "points_raw"), 0);
      assert.equal(ctx.manager.status().deletedRows, 12_000);
      assert.equal(ctx.manager.status().effectiveRawCutoffMs, NOW - 1000);
    } finally {
      await ctx.manager.runPass().catch(() => undefined);
      ctx.manager.stop();
      ctx.db.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("applies each tier's policy cutoff", async () => {
    const ctx = setup({
      policy: {
        rawMs: HOUR,
        oneMinuteMs: 2 * HOUR,
        oneHourMs: 3 * HOUR,
        receiptsMs: HOUR,
        gapsMs: HOUR,
      },
    });
    try {
      const series = makeSeries(ctx.db);
      insertRaw(ctx.db, series, NOW - 2 * HOUR);
      insertRaw(ctx.db, series, NOW - 0.5 * HOUR);
      insert1m(ctx.db, series, NOW - 3 * HOUR);
      insert1m(ctx.db, series, NOW - 1.5 * HOUR);
      insert1h(ctx.db, series, NOW - 4 * HOUR);
      insert1h(ctx.db, series, NOW - 1.5 * HOUR);
      ctx.db.db
        .prepare(
          "INSERT INTO sample_receipts (collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms, expected_interval_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
        )
        .run("collector-1", "run-1", 1, "machine-1", "metrics", "live", NOW - 2 * HOUR, NOW, 5000);
      ctx.db.db
        .prepare(
          "INSERT INTO sample_receipts (collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms, expected_interval_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
        )
        .run("collector-1", "run-1", 2, "machine-1", "metrics", "live", NOW - 0.5 * HOUR, NOW, 5000);
      const insertGap = ctx.db.db.prepare(
        "INSERT INTO gaps (collector_id, run_id, seq, machine_id, from_ms, to_ms, dropped, reason, received_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
      );
      insertGap.run("collector-1", "run-1", 10, "machine-1", NOW - 2 * HOUR, NOW - 2 * HOUR, 1, "outbox-age", NOW);
      insertGap.run(
        "collector-1",
        "run-1",
        11,
        "machine-1",
        NOW - 0.5 * HOUR,
        NOW - 0.5 * HOUR,
        1,
        "outbox-age",
        NOW,
      );

      await ctx.manager.runPass();

      assert.equal(count(ctx.db, "points_raw"), 1);
      assert.equal(count(ctx.db, "points_1m"), 1);
      assert.equal(count(ctx.db, "points_1h"), 1);
      assert.equal(count(ctx.db, "sample_receipts"), 1);
      assert.equal(count(ctx.db, "gaps"), 1);
      assert.equal(ctx.manager.status().deletedRows, 5);
      assert.equal(ctx.manager.status().pressure, false);
    } finally {
      ctx.manager.stop();
      ctx.db.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("deletes oldest raw points under disk pressure but keeps the last hour", async () => {
    const ctx = setup({
      policy: {
        rawMs: 100 * HOUR,
        oneMinuteMs: 100 * HOUR,
        oneHourMs: 100 * HOUR,
        receiptsMs: 100 * HOUR,
        gapsMs: 100 * HOUR,
      },
      maxDatabaseBytes: 1,
    });
    try {
      const series = makeSeries(ctx.db);
      insertRaw(ctx.db, series, NOW - 2 * HOUR);
      insertRaw(ctx.db, series, NOW - 0.5 * HOUR);
      insert1m(ctx.db, series, NOW - 2 * HOUR);
      insert1m(ctx.db, series, NOW - 0.5 * HOUR);

      await ctx.manager.runPass();

      const remainingRaw = ctx.db.db.prepare("SELECT ts_ms FROM points_raw ORDER BY ts_ms").all() as Array<{
        ts_ms: number;
      }>;
      assert.deepEqual(remainingRaw.map((row) => Number(row.ts_ms)), [NOW - 0.5 * HOUR]);
      const remaining1m = ctx.db.db.prepare("SELECT bucket_ms FROM points_1m ORDER BY bucket_ms").all() as Array<{
        bucket_ms: number;
      }>;
      assert.deepEqual(remaining1m.map((row) => Number(row.bucket_ms)), [NOW - 0.5 * HOUR]);

      const status = ctx.manager.status();
      assert.equal(status.pressure, true, "a tight budget keeps reporting pressure after safe deletions");
      assert.equal(status.effectiveRawCutoffMs, NOW - HOUR);
      assert.equal(status.deletedRows, 2);
    } finally {
      ctx.manager.stop();
      ctx.db.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("deletes each tier through the clustered primary key, not a table scan", () => {
    const ctx = setup({ policy: { rawMs: 1000 } });
    try {
      for (const [table, column, sql] of [
        ["points_raw", "ts_ms", RETENTION_DELETE_SQL.raw],
        ["points_1m", "bucket_ms", RETENTION_DELETE_SQL.oneMinute],
        ["points_1h", "bucket_ms", RETENTION_DELETE_SQL.oneHour],
      ] as const) {
        const plan = ctx.db.db
          .prepare(`EXPLAIN QUERY PLAN ${sql}`)
          .all(1, NOW, 1000) as Array<{ detail: string }>;
        const text = plan.map((row) => row.detail).join("\n");
        assert.match(text, /USING PRIMARY KEY/, `${table} retention must use its primary key`);
        assert.doesNotMatch(
          text,
          new RegExp(`SCAN ${table}`),
          `${table} retention must not full-scan for an age cutoff (column ${column})`,
        );
      }
    } finally {
      ctx.manager.stop();
      ctx.db.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("drains expired rows across several series within the chunk bound", async () => {
    const ctx = setup({
      policy: { rawMs: 1000, oneMinuteMs: 1000, oneHourMs: 1000, receiptsMs: 1000, gapsMs: 1000 },
      chunkSize: 100,
    });
    try {
      const insertSeries = ctx.db.db.prepare(
        "INSERT INTO series (machine_id, metric, resource_id) VALUES ('machine-1', 'cpu.core', ?)",
      );
      const seriesIds: number[] = [];
      for (let index = 0; index < 3; index += 1) {
        seriesIds.push(Number(insertSeries.run(`core:${index}`).lastInsertRowid));
      }
      for (const seriesId of seriesIds) {
        for (let index = 0; index < 200; index += 1) insertRaw(ctx.db, seriesId, NOW - 10_000 - index);
        insertRaw(ctx.db, seriesId, NOW); // fresh, must survive
      }

      let total = 0;
      let largestStatement = 0;
      for (let calls = 0; ; calls += 1) {
        const deleted = ctx.manager.runChunk();
        largestStatement = Math.max(largestStatement, deleted);
        if (deleted === 0) break;
        total += deleted;
        assert.ok(deleted <= 100, "one chunk must not exceed chunkSize");
        assert.ok(calls < 50, "retention should converge");
      }

      assert.equal(total, 600);
      assert.equal(largestStatement, 100);
      assert.equal(count(ctx.db, "points_raw"), 3, "one fresh row per series survives");
    } finally {
      ctx.manager.stop();
      ctx.db.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("starts and stops its unref'd background timer", async () => {
    const ctx = setup({ policy: { rawMs: 1000 }, chunkSize: 10, intervalMs: 5 });
    try {
      ctx.manager.start();
      await new Promise((resolve) => setTimeout(resolve, 40));
      ctx.manager.stop();
      assert.ok(ctx.manager.status().lastRunAtMs > 0);
    } finally {
      ctx.manager.stop();
      ctx.db.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });
});
