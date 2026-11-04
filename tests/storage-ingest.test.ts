import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { describe, it } from "node:test";
import { openHubDatabase, type HubDatabase } from "../src/hub/storage/db";
import { TelemetryStore, type IngestBatch, type IngestResult } from "../src/hub/storage/telemetry-store";
import { makeGapRecord, makeInventory, makeMetricsRecord, makePayload } from "./storage-fixtures";

const BASE = Date.parse("2026-11-01T12:00:00Z");

function setup(options: Partial<ConstructorParameters<typeof TelemetryStore>[0]> = {}): {
  dir: string;
  db: HubDatabase;
  store: TelemetryStore;
  path: string;
} {
  const dir = mkdtempSync(join(tmpdir(), "rigscope-storage-ingest-"));
  const path = join(dir, "hub.db");
  const db = openHubDatabase(path);
  const store = new TelemetryStore({
    database: db,
    flushIntervalMs: 60_000,
    ...options,
  });
  return { dir, db, store, path };
}

async function teardown(context: { dir: string; store: TelemetryStore }): Promise<void> {
  await context.store.close();
  rmSync(context.dir, { recursive: true, force: true });
}

async function ingest(store: TelemetryStore, batch: IngestBatch): Promise<IngestResult> {
  const promise = store.enqueue(batch);
  await store.flushNow();
  return promise;
}

function batchOf(records: IngestBatch["records"], overrides: Partial<IngestBatch> = {}): IngestBatch {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    mode: "live",
    expectedIntervalMs: 5000,
    records,
    ...overrides,
  };
}

function scalar(db: HubDatabase, sql: string, ...params: Array<string | number | null>): number {
  const row = db.db.prepare(sql).get(...params) as Record<string, unknown> | undefined;
  assert.ok(row);
  return Number(Object.values(row)[0]);
}

function seriesId(db: HubDatabase, metric: string, resourceId = ""): number {
  return scalar(
    db,
    "SELECT series_id FROM series WHERE machine_id = ? AND metric = ? AND resource_id = ?",
    "machine-1",
    metric,
    resourceId,
  );
}

describe("telemetry ingestion", () => {
  it("resolves only after the commit is visible to a second connection", async () => {
    const ctx = setup();
    try {
      const result = await ingest(
        ctx.store,
        batchOf([makeMetricsRecord({ sequence: 1, capturedAt: BASE })]),
      );
      assert.equal(result.status, "committed");

      const observer = new DatabaseSync(ctx.path);
      try {
        const receipts = observer.prepare("SELECT COUNT(*) AS n FROM sample_receipts").get() as { n: number };
        const points = observer.prepare("SELECT COUNT(*) AS n FROM points_raw").get() as { n: number };
        assert.equal(Number(receipts.n), 1);
        assert.ok(Number(points.n) > 0);
      } finally {
        observer.close();
      }
    } finally {
      await teardown(ctx);
    }
  });

  it("deduplicates repeated and reordered batches without double-counting rollups", async () => {
    const ctx = setup();
    try {
      const records = [
        makeMetricsRecord({ sequence: 1, capturedAt: BASE, payload: makePayload({ cpuUsage: 10 }) }),
        makeMetricsRecord({
          sequence: 2,
          capturedAt: BASE + 60_000,
          payload: makePayload({ cpuUsage: 20 }),
        }),
        makeMetricsRecord({
          sequence: 3,
          capturedAt: BASE + 120_000,
          payload: makePayload({ cpuUsage: 30 }),
        }),
      ];
      const first = await ingest(ctx.store, batchOf(records));
      assert.equal(first.status, "committed");
      if (first.status !== "committed") return;
      assert.equal(first.accepted.length, 3);
      assert.equal(first.rejected.length, 0);

      const rawBefore = scalar(ctx.db, "SELECT COUNT(*) FROM points_raw");
      const cpuSeries = seriesId(ctx.db, "cpu.usage");
      const rollupBefore = scalar(ctx.db, "SELECT SUM(count) FROM points_1m WHERE series_id = ?", cpuSeries);
      assert.equal(rollupBefore, 3);

      const second = await ingest(ctx.store, batchOf([...records].reverse(), { mode: "replay" }));
      assert.equal(second.status, "committed");
      if (second.status !== "committed") return;
      assert.equal(second.accepted.length, 3, "duplicates are acknowledged as accepted");
      assert.equal(second.rejected.length, 0);

      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM points_raw"), rawBefore);
      assert.equal(scalar(ctx.db, "SELECT SUM(count) FROM points_1m WHERE series_id = ?", cpuSeries), 3);
      assert.equal(
        scalar(ctx.db, "SELECT SUM(null_count) FROM points_1m WHERE series_id = ?", cpuSeries),
        0,
      );
      const stats = ctx.store.stats();
      assert.equal(stats.duplicateRecords, 3);
      assert.equal(stats.committedRecords, 3);
    } finally {
      await teardown(ctx);
    }
  });

  it("keeps rollup extrema exact across a lost acknowledgement", async () => {
    const ctx = setup();
    try {
      const records = [
        makeMetricsRecord({ sequence: 1, capturedAt: BASE, payload: makePayload({ cpuUsage: 90 }) }),
        makeMetricsRecord({
          sequence: 2,
          capturedAt: BASE + 10_000,
          payload: makePayload({ cpuUsage: 5 }),
        }),
      ];
      const batch = batchOf(records);
      await ingest(ctx.store, batch);
      const cpuSeries = seriesId(ctx.db, "cpu.usage");
      const before = scalar(
        ctx.db,
        "SELECT COUNT(*) FROM points_raw WHERE series_id = ?",
        cpuSeries,
      );
      assert.equal(before, 2);

      const retry = await ingest(ctx.store, batch);
      assert.equal(retry.status, "committed");
      if (retry.status !== "committed") return;
      assert.deepEqual(
        retry.accepted,
        records.map((record) => ({ runId: record.runId, sequence: record.sequence })),
      );
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM points_raw WHERE series_id = ?", cpuSeries), 2);
      const rollup = ctx.db.db
        .prepare("SELECT MIN(min) AS lo, MAX(max) AS hi, SUM(count) AS n FROM points_1m WHERE series_id = ?")
        .get(cpuSeries) as { lo: number; hi: number; n: number };
      assert.equal(Number(rollup.lo), 5);
      assert.equal(Number(rollup.hi), 90);
      assert.equal(Number(rollup.n), 2);
    } finally {
      await teardown(ctx);
    }
  });

  it("never lets replay batches update latest state", async () => {
    const ctx = setup();
    try {
      const live = await ingest(
        ctx.store,
        batchOf([makeMetricsRecord({ sequence: 1, capturedAt: BASE })]),
      );
      assert.equal(live.status, "committed");
      if (live.status !== "committed") return;
      assert.equal(live.newLive?.sequence, 1);

      const replay = await ingest(
        ctx.store,
        batchOf([makeMetricsRecord({ sequence: 2, capturedAt: BASE + 120_000 })], { mode: "replay" }),
      );
      assert.equal(replay.status, "committed");
      if (replay.status !== "committed") return;
      assert.equal(replay.newLive, null);
      assert.equal(ctx.store.latestLive()[0]?.sequence, 1);

      const newerLive = await ingest(
        ctx.store,
        batchOf([makeMetricsRecord({ sequence: 3, capturedAt: BASE + 240_000 })]),
      );
      assert.equal(newerLive.status, "committed");
      if (newerLive.status !== "committed") return;
      assert.equal(newerLive.newLive?.sequence, 3);
      assert.equal(ctx.store.latestLive()[0]?.sequence, 3);
    } finally {
      await teardown(ctx);
    }
  });

  it("does not regress live state with an older record or an earlier run", async () => {
    const ctx = setup();
    try {
      await ingest(ctx.store, batchOf([makeMetricsRecord({ sequence: 5, capturedAt: BASE })]));

      const olderSameRun = await ingest(
        ctx.store,
        batchOf([makeMetricsRecord({ sequence: 4, capturedAt: BASE + 1000 })]),
      );
      assert.equal(olderSameRun.status, "committed");
      if (olderSameRun.status !== "committed") return;
      assert.equal(olderSameRun.newLive, null);
      assert.equal(ctx.store.latestLive()[0]?.sequence, 5);

      const olderRun = await ingest(
        ctx.store,
        batchOf([
          makeMetricsRecord({
            runId: "run-2",
            sequence: 1,
            capturedAt: BASE - 60_000,
          }),
        ]),
      );
      assert.equal(olderRun.status, "committed");
      if (olderRun.status !== "committed") return;
      assert.equal(olderRun.newLive, null);
      assert.equal(ctx.store.latestLive()[0]?.runId, "run-1");

      const newerRun = await ingest(
        ctx.store,
        batchOf([
          makeMetricsRecord({
            runId: "run-3",
            sequence: 1,
            capturedAt: BASE + 60_000,
          }),
        ]),
      );
      assert.equal(newerRun.status, "committed");
      if (newerRun.status !== "committed") return;
      assert.equal(newerRun.newLive?.runId, "run-3");
      assert.equal(ctx.store.latestLive()[0]?.runId, "run-3");
    } finally {
      await teardown(ctx);
    }
  });

  it("returns busy immediately when the bounded queue cannot hold a batch", async () => {
    const ctx = setup({ maxQueuedRecords: 5 });
    try {
      const records = Array.from({ length: 6 }, (_, index) =>
        makeMetricsRecord({ sequence: index + 1, capturedAt: BASE + index * 5000 }),
      );
      const result = await ctx.store.enqueue(batchOf(records));
      assert.equal(result.status, "busy");
      if (result.status !== "busy") return;
      assert.ok(result.retryAfterMs >= 1000 && result.retryAfterMs <= 5000);
      assert.equal(ctx.store.stats().queuedRecords, 0);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM sample_receipts"), 0);

      // A queued-but-not-yet-flushed batch counts toward the bound too.
      const first = ctx.store.enqueue(
        batchOf(records.slice(0, 3).map((record, index) => ({ ...record, sequence: index + 10 }))),
      );
      const second = await ctx.store.enqueue(
        batchOf(records.slice(3).map((record, index) => ({ ...record, sequence: index + 20 }))),
      );
      assert.equal(second.status, "busy");
      await ctx.store.flushNow();
      assert.equal((await first).status, "committed");
    } finally {
      await teardown(ctx);
    }
  });

  it("rolls back a failed write, reports busy, and recovers afterwards", async () => {
    const ctx = setup();
    try {
      ctx.db.db.exec(
        "CREATE TRIGGER injected_failure BEFORE INSERT ON points_raw " +
          "BEGIN SELECT RAISE(ABORT, 'injected write failure'); END",
      );
      const batch = batchOf([
        makeMetricsRecord({ sequence: 1, capturedAt: BASE }),
        makeMetricsRecord({ sequence: 2, capturedAt: BASE + 5000 }),
      ]);
      const failed = await ingest(ctx.store, batch);
      assert.equal(failed.status, "busy");
      if (failed.status !== "busy") return;
      assert.equal(failed.reason, "storage write failed");
      assert.equal(failed.retryAfterMs, 5000);
      const stats = ctx.store.stats();
      assert.equal(stats.writeFailures, 1);
      assert.match(stats.lastWriteError ?? "", /injected write failure/);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM sample_receipts"), 0);

      ctx.db.db.exec("DROP TRIGGER injected_failure");
      const recovered = await ingest(ctx.store, batch);
      assert.equal(recovered.status, "committed");
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM sample_receipts"), 2);
      assert.equal(ctx.store.stats().writeFailures, 1, "the historical failure count is retained");
    } finally {
      await teardown(ctx);
    }
  });

  it("stores explicit NULL unavailable readings and omits unavailable multi-resource families", async () => {
    const ctx = setup();
    try {
      await ingest(
        ctx.store,
        batchOf([
          makeMetricsRecord({
            sequence: 1,
            capturedAt: BASE,
            payload: makePayload({
              unavailable: { cpu: true, memory: true, swap: true, gpus: true, disks: true, network: true },
            }),
          }),
        ]),
      );
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM points_raw"), 4);
      for (const metric of ["cpu.usage", "memory.used", "memory.usage", "swap.used"]) {
        const id = seriesId(ctx.db, metric);
        assert.equal(
          scalar(ctx.db, "SELECT COUNT(*) FROM points_raw WHERE series_id = ? AND value IS NULL", id),
          1,
          `${metric} must be NULL, never a successful zero`,
        );
        const rollup = ctx.db.db
          .prepare("SELECT count, null_count FROM points_1m WHERE series_id = ?")
          .get(id) as { count: number; null_count: number };
        assert.equal(Number(rollup.count), 0);
        assert.equal(Number(rollup.null_count), 1);
      }
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM series WHERE metric LIKE 'gpu.%'"), 0);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM series WHERE metric LIKE 'fs.%'"), 0);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM series WHERE metric LIKE 'disk.%'"), 0);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM series WHERE metric LIKE 'net.%'"), 0);
    } finally {
      await teardown(ctx);
    }
  });

  it("maps per-resource unavailable states and per-field nulls to NULL primary metrics", async () => {
    const ctx = setup();
    try {
      await ingest(
        ctx.store,
        batchOf([
          makeMetricsRecord({
            sequence: 1,
            capturedAt: BASE,
            payload: makePayload({
              cores: 1,
              gpus: [
                { id: "gpu-ok", utilization: 50, memoryUsed: 100, memoryTotal: 200, temperature: null, power: 75 },
                { id: "gpu-bad", unavailable: true },
              ],
              filesystems: [
                { id: "fs-ok", used: 10, total: 20 },
                { id: "fs-bad", unavailable: true },
              ],
              disks: [
                { id: "disk-ok", read: 1, write: 2 },
                { id: "disk-bad", unavailable: true },
              ],
              interfaces: [
                { id: "eth-ok", rx: 3, tx: 4 },
                { id: "eth-bad", unavailable: true },
              ],
            }),
          }),
        ]),
      );

      const nullCount = (metric: string, resourceId: string): number =>
        scalar(
          ctx.db,
          "SELECT COUNT(*) FROM points_raw r JOIN series s USING (series_id) " +
            "WHERE s.metric = ? AND s.resource_id = ? AND r.value IS NULL",
          metric,
          resourceId,
        );
      assert.equal(nullCount("gpu.utilization", "gpu-bad"), 1);
      assert.equal(nullCount("gpu.memory.used", "gpu-bad"), 1);
      assert.equal(nullCount("gpu.temperature", "gpu-ok"), 1, "per-field null is a NULL point");
      assert.equal(nullCount("fs.used", "fs-bad"), 1);
      assert.equal(nullCount("fs.usage", "fs-bad"), 1);
      assert.equal(nullCount("fs.inodes.usage", "fs-bad"), 1);
      assert.equal(nullCount("disk.read", "disk-bad"), 1);
      assert.equal(nullCount("disk.write", "disk-bad"), 1);
      for (const metric of ["disk.read.latency", "disk.write.latency", "disk.utilization"])
        assert.equal(nullCount(metric, "disk-bad"), 1);
      assert.equal(nullCount("net.rx", "eth-bad"), 1);
      assert.equal(nullCount("net.tx", "eth-bad"), 1);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM series WHERE resource_id = 'gpu-bad'"), 2);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM series WHERE resource_id = 'fs-bad'"), 3);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM series WHERE resource_id = 'disk-bad'"), 5);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM series WHERE resource_id = 'eth-bad'"), 2);
    } finally {
      await teardown(ctx);
    }
  });

  it("breaks known multi-resource series with NULL when the whole family becomes unavailable", async () => {
    const ctx = setup();
    try {
      await ingest(
        ctx.store,
        batchOf([
          makeMetricsRecord({
            sequence: 1,
            capturedAt: BASE,
            payload: makePayload({
              gpus: [{ id: "gpu-a", utilization: 50, memoryUsed: 100, memoryTotal: 200, temperature: 60, power: 70 }],
              filesystems: [{ id: "fs-a", used: 10, total: 20 }],
              disks: [{ id: "disk-a", read: 1, write: 2 }],
              interfaces: [{ id: "eth-a", rx: 3, tx: 4 }],
            }),
          }),
        ]),
      );
      await ingest(
        ctx.store,
        batchOf([
          makeMetricsRecord({
            sequence: 2,
            capturedAt: BASE + 5000,
            payload: makePayload({
              unavailable: { gpus: true, disks: true, network: true },
            }),
          }),
        ]),
      );

      const series = (metric: string, resourceId: string): number =>
        seriesId(ctx.db, metric, resourceId);
      const points = (metric: string, resourceId: string): Array<{ value: number | null }> =>
        ctx.db.db
          .prepare(
            "SELECT r.value FROM points_raw r JOIN series s USING (series_id) " +
              "WHERE s.metric = ? AND s.resource_id = ? ORDER BY r.ts_ms",
          )
          .all(metric, resourceId) as Array<{ value: number | null }>;

      for (const metric of [
        "gpu.utilization",
        "gpu.memory.used",
        "gpu.memory.total",
        "gpu.temperature",
        "gpu.power",
      ]) {
        const rows = points(metric, "gpu-a");
        assert.equal(rows.length, 2, `${metric} keeps a NULL point for the failed reading`);
        assert.equal(rows[1]!.value, null);
      }
      // Redundant helper use keeps the series assertion explicit.
      assert.ok(series("gpu.utilization", "gpu-a") > 0);
      assert.equal(points("fs.total", "fs-a")[1]!.value, null);
      assert.equal(points("disk.write", "disk-a")[1]!.value, null);
      assert.equal(points("net.rx", "eth-a")[1]!.value, null);
    } finally {
      await teardown(ctx);
    }
  });

  it("stores gap records once and deduplicates retries", async () => {
    const ctx = setup();
    try {
      const gap = makeGapRecord({
        sequence: 1,
        capturedAt: BASE + 10_000,
        fromMs: BASE,
        toMs: BASE + 9000,
        dropped: 3,
      });
      const first = await ingest(ctx.store, batchOf([gap]));
      assert.equal(first.status, "committed");
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM gaps"), 1);
      const second = await ingest(ctx.store, batchOf([gap]));
      assert.equal(second.status, "committed");
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM gaps"), 1);
      assert.equal(ctx.store.stats().duplicateRecords, 1);
    } finally {
      await teardown(ctx);
    }
  });

  it("rejects mismatched collector identity and far-future capture times", async () => {
    const ctx = setup();
    try {
      const result = await ingest(
        ctx.store,
        batchOf([
          makeMetricsRecord({
            collectorId: "collector-other",
            sequence: 1,
            capturedAt: BASE,
          }),
          makeMetricsRecord({ sequence: 2, capturedAt: Date.now() + 25 * 3_600_000 }),
        ]),
      );
      assert.equal(result.status, "committed");
      if (result.status !== "committed") return;
      assert.equal(result.accepted.length, 0);
      assert.equal(result.rejected.length, 2);
      assert.match(result.rejected[0]!.reason, /collectorId/);
      assert.match(result.rejected[1]!.reason, /future/);
      assert.equal(scalar(ctx.db, "SELECT COUNT(*) FROM sample_receipts"), 0);
      assert.equal(ctx.store.stats().rejectedRecords, 2);
    } finally {
      await teardown(ctx);
    }
  });

  it("restores boot identity and the original receipt time across restart and duplicate retry", async () => {
    const ctx = setup();
    try {
      const record = makeMetricsRecord({
        bootId: "boot-9",
        sequence: 7,
        capturedAt: BASE,
      });
      const batch = batchOf([record], { hostname: "workstation-1" });
      const first = await ingest(ctx.store, batch);
      assert.equal(first.status, "committed");
      const before = ctx.store.latestLive()[0]!;
      assert.equal(before.bootId, "boot-9");
      assert.equal(before.sequence, 7);
      assert.equal(before.hostname, "workstation-1");
      assert.equal(before.expectedIntervalMs, 5000);
      await ctx.store.close();

      const reopenedDb = openHubDatabase(ctx.path);
      const reopened = new TelemetryStore({ database: reopenedDb, flushIntervalMs: 60_000 });
      try {
        const restored = reopened.latestLive()[0]!;
        assert.equal(restored.bootId, "boot-9");
        assert.equal(restored.runId, "run-1");
        assert.equal(restored.sequence, 7);
        assert.equal(restored.receivedAt, before.receivedAt);
        assert.equal(restored.capturedAt, before.capturedAt);
        assert.equal(restored.hostname, "workstation-1");
        assert.equal(restored.expectedIntervalMs, 5000);

        // A lost-ack retry after the restart must not move the receipt time or
        // regress the restored live state.
        const promise = reopened.enqueue(batch);
        await reopened.flushNow();
        const retried = await promise;
        assert.equal(retried.status, "committed");
        if (retried.status !== "committed") return;
        assert.equal(retried.newLive, null);
        assert.equal(reopened.latestLive()[0]!.receivedAt, before.receivedAt);
      } finally {
        await reopened.close();
      }
    } finally {
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("persists inventory and machine metadata across a restart", async () => {
    const ctx = setup();
    try {
      ctx.store.saveInventory("machine-1", new Date(BASE).toISOString(), makeInventory());
      ctx.store.setMachineMeta("machine-1", { displayName: "Desk", tags: ["prod", "desk"] });
      ctx.store.setMachineMeta("machine-2", { tags: ["lab"] });
      // A partial update preserves the fields it does not mention.
      ctx.store.setMachineMeta("machine-1", { displayName: "Workstation" });
      assert.deepEqual(ctx.store.getMachineMeta("machine-1"), {
        displayName: "Workstation",
        tags: ["prod", "desk"],
      });
      assert.deepEqual(ctx.store.getMachineMeta("machine-2"), { displayName: null, tags: ["lab"] });
      assert.deepEqual(ctx.store.getMachineMeta("missing"), { displayName: null, tags: [] });
      assert.equal(ctx.store.listMachineMeta().size, 2);
      ctx.store.close();
      ctx.store.close(); // idempotent

      const reopenedDb = openHubDatabase(ctx.path);
      const reopened = new TelemetryStore({ database: reopenedDb, flushIntervalMs: 60_000 });
      try {
        const inventory = reopened.loadInventory("machine-1");
        assert.ok(inventory);
        assert.equal(inventory.capturedAt, new Date(BASE).toISOString());
        assert.equal(inventory.inventory.cpu.logicalCores, 2);
        assert.deepEqual(reopened.listMachineMeta().get("machine-1"), {
          displayName: "Workstation",
          tags: ["prod", "desk"],
        });
      } finally {
        await reopened.close();
      }
    } finally {
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("drains queued work on close", async () => {
    const ctx = setup({ flushIntervalMs: 60_000 });
    try {
      const promise = ctx.store.enqueue(
        batchOf([makeMetricsRecord({ sequence: 1, capturedAt: BASE })]),
      );
      await ctx.store.close();
      const result = await promise;
      assert.equal(result.status, "committed");
    } finally {
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });
});
