import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";
import { openHubDatabase, type HubDatabase } from "../src/hub/storage/db";
import { HistoryQueryError, queryHistory } from "../src/hub/storage/history-query";
import { METRICS } from "../src/hub/storage/series";
import { TelemetryStore, type IngestBatch, type IngestResult } from "../src/hub/storage/telemetry-store";
import { makeGapRecord, makeMetricsRecord, makePayload } from "./storage-fixtures";

function setup(): { dir: string; db: HubDatabase; store: TelemetryStore; path: string } {
  const dir = mkdtempSync(join(tmpdir(), "rigscope-storage-history-"));
  const path = join(dir, "hub.db");
  const db = openHubDatabase(path);
  const store = new TelemetryStore({ database: db, flushIntervalMs: 60_000 });
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

function batch(records: IngestBatch["records"], overrides: Partial<IngestBatch> = {}): IngestBatch {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    mode: "live",
    expectedIntervalMs: 5000,
    records,
    ...overrides,
  };
}

describe("history query", () => {
  it("keeps past CPU buckets unchanged as the one-hour window advances", async () => {
    const ctx = setup();
    try {
      const start = Math.floor(Date.now() / 3_600_000) * 3_600_000 - 3_600_000;
      await ingest(ctx.store, batch([
        makeMetricsRecord({ sequence: 1, capturedAt: start + 105_500, payload: makePayload({ cpuUsage: 10 }) }),
        makeMetricsRecord({ sequence: 2, capturedAt: start + 108_500, payload: makePayload({ cpuUsage: 90 }) }),
        makeMetricsRecord({ sequence: 3, capturedAt: start + 3_600_100, payload: makePayload({ cpuUsage: 20 }) }),
      ]));
      const query = (offset: number) => queryHistory(ctx.db, {
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: start + offset,
        toMs: start + 3_600_000 + offset,
        maxPoints: 600,
      });
      const before = query(500);
      await ingest(ctx.store, batch([
        makeMetricsRecord({ sequence: 4, capturedAt: start + 3_601_100, payload: makePayload({ cpuUsage: 60 }) }),
      ]));
      const after = query(1500);
      assert.equal(before.tier, "raw");
      assert.equal(before.stepMs, 6000);
      assert.equal(before.stepMs, after.stepMs);
      assert.deepEqual(after.series[0]!.points.slice(0, -1), before.series[0]!.points.slice(0, -1));
      assert.deepEqual(after.series[0]!.points.slice(0, -1), [
        { t: start + 102_000, min: 10, max: 10, avg: 10 },
        { t: start + 108_000, min: 90, max: 90, avg: 90 },
      ]);
      assert.deepEqual(before.series[0]!.points.at(-1), { t: start + 3_600_000, min: 20, max: 20, avg: 20 });
      assert.deepEqual(after.series[0]!.points.at(-1), { t: start + 3_600_000, min: 20, max: 60, avg: 40 });
    } finally {
      await teardown(ctx);
    }
  });

  for (const [tier, resolutionMs] of [["1m", 60_000], ["1h", 3_600_000]] as const) {
    it(`keeps ${tier} rollups on a stable grid with weighted averages`, async () => {
      const ctx = setup();
      try {
        const start = Math.floor(Date.now() / (3 * resolutionMs)) * 3 * resolutionMs - 6 * resolutionMs;
        const offsets = [resolutionMs, resolutionMs * 7 / 6, 2 * resolutionMs, 3 * resolutionMs, resolutionMs * 19 / 6];
        await ingest(ctx.store, batch(offsets.map((offset, index) => makeMetricsRecord({
          sequence: index + 1,
          capturedAt: start + offset + 1000,
          payload: makePayload({ cpuUsage: [10, 30, 90, 40, 60][index] }),
        }))));
        const query = (offset: number) => queryHistory(ctx.db, {
          machineId: "machine-1", metrics: ["cpu.usage"],
          fromMs: start + offset, toMs: start + 5 * resolutionMs + offset, maxPoints: 2,
        }, { maxRowsScanned: 3 });
        const before = query(resolutionMs / 4);
        const after = query(resolutionMs / 4 + 1000);
        assert.equal(before.tier, tier);
        assert.equal(before.stepMs, 3 * resolutionMs);
        assert.deepEqual(before.series[0]!.points, [
          { t: start, min: 10, max: 90, avg: 130 / 3 },
          { t: start + 3 * resolutionMs, min: 40, max: 60, avg: 50 },
        ]);
        assert.deepEqual(after.series[0]!.points, before.series[0]!.points);
      } finally {
        await teardown(ctx);
      }
    });
  }

  it("bounds a dense rolling window to maxPoints plus one edge bucket", async () => {
    const ctx = setup();
    try {
      const start = Math.floor(Date.now() / 3_600_000) * 3_600_000 - 3_600_000;
      await ingest(ctx.store, batch(Array.from({ length: 601 }, (_, index) => makeMetricsRecord({
        sequence: index + 1, capturedAt: start + index * 6000 + 500,
        payload: makePayload({ cpuUsage: index % 2 ? 90 : 10 }),
      }))));
      const query = (offset: number) => queryHistory(ctx.db, {
        machineId: "machine-1", metrics: ["cpu.usage"],
        fromMs: start + offset, toMs: start + 3_600_000 + offset, maxPoints: 600,
      });
      const before = query(500);
      const after = query(6500);
      assert.equal(before.series[0]!.points.length, 601);
      assert.equal(after.series[0]!.points.length, 600);
      assert.deepEqual(after.series[0]!.points, before.series[0]!.points.slice(1));
    } finally {
      await teardown(ctx);
    }
  });

  it("queries selected high-numbered cores without excluding other metric resources", async () => {
    const ctx = setup();
    try {
      const from = Date.now() - 60_000;
      const promise = ctx.store.enqueue(batch([makeMetricsRecord({sequence: 1, capturedAt: from + 1000,
        payload: makePayload({cores: 128, coreValue: 35, memoryUsed: 200, memoryTotal: 1000})})]));
      await ctx.store.flushNow();
      assert.equal((await promise).status, "committed");
      const query = {machineId: "machine-1", metrics: ["cpu.core", "memory.usage"], coreIds: ["core:127"],
        fromMs: from, toMs: from + 30_000};
      const selected = queryHistory(ctx.db, query);
      assert.deepEqual(selected.series.map(s => [s.metric, s.resourceId]), [["cpu.core", "core:127"], ["memory.usage", ""]]);
      assert.equal(selected.series[0].points[0].avg, 35);
      assert.equal(selected.truncated, false);
      const none = queryHistory(ctx.db, {...query, coreIds: []});
      assert.deepEqual(none.series.map(s => s.metric), ["memory.usage"]);
      const allMetrics = queryHistory(ctx.db, {...query, metrics: Object.keys(METRICS)});
      assert.ok(allMetrics.series.some(s => s.metric === "cpu.core" && s.resourceId === "core:127"));
      assert.throws(() => queryHistory(ctx.db, {...query, coreIds: ["core:1024"]}), HistoryQueryError);
      assert.throws(() => queryHistory(ctx.db, {...query, coreIds: ["core:1", "core:1"]}), HistoryQueryError);
    } finally {
      await teardown(ctx);
    }
  });

  it("aggregates raw points into buckets and omits empty buckets", async () => {
    const ctx = setup();
    try {
      const from = Math.floor((Date.now() - 120_000) / 10_000) * 10_000;
      const records = [
        makeMetricsRecord({ sequence: 1, capturedAt: from, payload: makePayload({ cpuUsage: 10 }) }),
        makeMetricsRecord({ sequence: 2, capturedAt: from + 1000, payload: makePayload({ cpuUsage: 20 }) }),
        makeMetricsRecord({ sequence: 3, capturedAt: from + 2000, payload: makePayload({ cpuUsage: 30 }) }),
        makeMetricsRecord({ sequence: 4, capturedAt: from + 25_000, payload: makePayload({ cpuUsage: 40 }) }),
      ];
      const result = await ingest(ctx.store, batch(records));
      assert.equal(result.status, "committed");

      const history = queryHistory(ctx.db, {
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: from,
        toMs: from + 30_000,
        maxPoints: 6,
      });
      assert.equal(history.tier, "raw");
      assert.equal(history.stepMs, 5000);
      assert.equal(history.series.length, 1);
      const series = history.series[0]!;
      assert.equal(series.metric, "cpu.usage");
      assert.equal(series.resourceId, "");
      assert.equal(series.unit, "percent");
      // Four samples land in two 5s buckets; the empty buckets between are
      // omitted rather than zero-filled.
      assert.deepEqual(series.points, [
        { t: from, min: 10, max: 30, avg: 20 },
        { t: from + 25_000, min: 40, max: 40, avg: 40 },
      ]);
      assert.equal(history.truncated, false);
      assert.ok(history.elapsedMs >= 0);
    } finally {
      await teardown(ctx);
    }
  });

  it("returns null aggregates for an explicitly unavailable bucket", async () => {
    const ctx = setup();
    try {
      const from = Math.floor((Date.now() - 60_000) / 10_000) * 10_000;
      await ingest(
        ctx.store,
        batch([
          makeMetricsRecord({
            sequence: 1,
            capturedAt: from,
            payload: makePayload({ unavailable: { cpu: true } }),
          }),
        ]),
      );
      const history = queryHistory(ctx.db, {
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: from,
        toMs: from + 10_000,
      });
      assert.deepEqual(history.series[0]!.points, [{ t: from, min: null, max: null, avg: null }]);
    } finally {
      await teardown(ctx);
    }
  });

  it("filters per-resource series and preserves extrema through 1m and 1h rollups", async () => {
    const ctx = setup();
    try {
      const hourStart = Math.floor((Date.now() - 2 * 3_600_000) / 3_600_000) * 3_600_000;
      const minuteStart = hourStart + 10 * 60_000;
      await ingest(
        ctx.store,
        batch([
          makeMetricsRecord({
            sequence: 1,
            capturedAt: minuteStart,
            payload: makePayload({
              cpuUsage: 10,
              gpus: [
                { id: "gpu-a", utilization: 1 },
                { id: "gpu-b", utilization: 80 },
              ],
            }),
          }),
          makeMetricsRecord({
            sequence: 2,
            capturedAt: minuteStart + 10_000,
            payload: makePayload({ cpuUsage: 90 }),
          }),
          makeMetricsRecord({
            sequence: 3,
            capturedAt: minuteStart + 20_000,
            payload: makePayload({ cpuUsage: 50 }),
          }),
          makeMetricsRecord({
            sequence: 4,
            capturedAt: minuteStart + 70_000,
            payload: makePayload({ cpuUsage: 70 }),
          }),
        ]),
      );

      const oneMinute = queryHistory(
        ctx.db,
        {
          machineId: "machine-1",
          metrics: ["cpu.usage"],
          fromMs: minuteStart,
          toMs: minuteStart + 120_000,
        },
        { maxRowsScanned: 3 },
      );
      assert.equal(oneMinute.tier, "1m");
      assert.equal(oneMinute.stepMs, 60_000);
      assert.deepEqual(oneMinute.series[0]!.points, [
        { t: minuteStart, min: 10, max: 90, avg: 50 },
        { t: minuteStart + 60_000, min: 70, max: 70, avg: 70 },
      ]);

      const oneHour = queryHistory(
        ctx.db,
        {
          machineId: "machine-1",
          metrics: ["cpu.usage"],
          fromMs: minuteStart,
          toMs: minuteStart + 120_000,
        },
        { maxRowsScanned: 1 },
      );
      assert.equal(oneHour.tier, "1h");
      const hourPoint = oneHour.series[0]!.points[0]!;
      assert.equal(hourPoint.min, 10);
      assert.equal(hourPoint.max, 90);
      assert.equal(hourPoint.avg, 55); // (10 + 90 + 50 + 70) / 4

      const filtered = queryHistory(ctx.db, {
        machineId: "machine-1",
        metrics: ["gpu.utilization"],
        resourceIds: ["gpu-b"],
        fromMs: minuteStart,
        toMs: minuteStart + 60_000,
      });
      assert.equal(filtered.series.length, 1);
      assert.equal(filtered.series[0]!.resourceId, "gpu-b");
      assert.equal(filtered.series[0]!.unit, "percent");
    } finally {
      await teardown(ctx);
    }
  });

  it("chooses the 1m tier from actual indexed row counts when raw scan exceeds budget", async () => {
    const ctx = setup();
    try {
      const now = Date.now();
      const from = Math.floor((now - 3_600_000) / 60_000) * 60_000;
      // 180 raw points over an hour: the 5s-resolution estimate would wrongly
      // conclude that raw fits a 100-row budget.
      const records = Array.from({ length: 180 }, (_, index) =>
        makeMetricsRecord({
          sequence: index + 1,
          capturedAt: from + index * 20_000,
          payload: makePayload({ cpuUsage: 10 }),
        }),
      );
      await ingest(ctx.store, batch(records));

      const history = queryHistory(
        ctx.db,
        { machineId: "machine-1", metrics: ["cpu.usage"], fromMs: from, toMs: now },
        { maxRowsScanned: 100 },
      );
      assert.equal(history.tier, "1m");
      assert.equal(history.stepMs, 60_000);
      assert.ok(history.series[0]!.points.length > 0);
    } finally {
      await teardown(ctx);
    }
  });

  it("budgets dense 100ms raw data instead of assuming a 5s cadence", async () => {
    const ctx = setup();
    try {
      const now = Date.now();
      const from = Math.floor((now - 120_000) / 60_000) * 60_000;
      const records = Array.from({ length: 300 }, (_, index) =>
        makeMetricsRecord({
          sequence: index + 1,
          capturedAt: from + index * 100,
          payload: makePayload({ cpuUsage: 10 }),
        }),
      );
      await ingest(ctx.store, batch(records));

      const history = queryHistory(
        ctx.db,
        { machineId: "machine-1", metrics: ["cpu.usage"], fromMs: from, toMs: from + 30_000 },
        { maxRowsScanned: 50 },
      );
      assert.equal(history.tier, "1m");
      assert.equal(history.series[0]!.points.length, 1);
    } finally {
      await teardown(ctx);
    }
  });

  it("reports dropped and missing gaps without inventing leading/trailing gaps", async () => {
    const ctx = setup();
    try {
      const from = Math.floor((Date.now() - 300_000) / 60_000) * 60_000;
      const dropped = makeGapRecord({
        sequence: 1,
        capturedAt: from + 10_000,
        fromMs: from + 1000,
        toMs: from + 9000,
        dropped: 4,
        reason: "outbox-bytes",
      });
      await ingest(ctx.store, batch([dropped]));

      const droppedHistory = queryHistory(ctx.db, {
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: from,
        toMs: from + 60_000,
      });
      assert.deepEqual(droppedHistory.gaps, [
        {
          fromMs: from + 1000,
          toMs: from + 9000,
          kind: "dropped",
          reason: "outbox-bytes",
          droppedRecords: 4,
        },
      ]);

      // Machine 2 has two receipts a minute apart with a 5s cadence. The 15s
      // floor makes that a missing gap; the edges are not reported.
      await ingest(
        ctx.store,
        batch(
          [
            makeMetricsRecord({
              collectorId: "collector-2",
              sequence: 1,
              capturedAt: from,
            }),
            makeMetricsRecord({
              collectorId: "collector-2",
              sequence: 2,
              capturedAt: from + 60_000,
            }),
          ],
          { machineId: "machine-2", collectorId: "collector-2" },
        ),
      );
      const missing = queryHistory(ctx.db, {
        machineId: "machine-2",
        metrics: ["cpu.usage"],
        fromMs: from - 60_000,
        toMs: from + 120_000,
      });
      assert.equal(missing.gaps.length, 1);
      assert.deepEqual(missing.gaps[0], {
        fromMs: from,
        toMs: from + 60_000,
        kind: "missing",
        reason: "expected interval missed",
      });
    } finally {
      await teardown(ctx);
    }
  });

  it("uses the primary-key range for point scans", async () => {
    const ctx = setup();
    try {
      const from = Math.floor((Date.now() - 120_000) / 60_000) * 60_000;
      await ingest(
        ctx.store,
        batch([makeMetricsRecord({ sequence: 1, capturedAt: from })]),
      );
      const series = Number(
        (
          ctx.db.db.prepare("SELECT series_id FROM series WHERE metric = 'cpu.usage'").get() as {
            series_id: number;
          }
        ).series_id,
      );
      for (const table of ["points_raw", "points_1m", "points_1h"]) {
        const column = table === "points_raw" ? "ts_ms" : "bucket_ms";
        const plan = ctx.db.db
          .prepare(
            `EXPLAIN QUERY PLAN SELECT * FROM ${table} WHERE series_id = ? AND ${column} >= ? AND ${column} <= ?`,
          )
          .all(series, from, from + 60_000) as Array<{ detail: string }>;
        const text = plan.map((row) => row.detail).join("\n");
        assert.match(text, /USING PRIMARY KEY/, `${table} should use its primary key`);
        assert.doesNotMatch(text, new RegExp(`SCAN ${table}`), `${table} must not full-scan`);
      }
    } finally {
      await teardown(ctx);
    }
  });

  it("truncates at maxSeries", async () => {
    const ctx = setup();
    try {
      const from = Math.floor((Date.now() - 120_000) / 10_000) * 10_000;
      await ingest(
        ctx.store,
        batch([
          makeMetricsRecord({
            sequence: 1,
            capturedAt: from,
            payload: makePayload({
              gpus: [
                { id: "gpu-a", utilization: 1 },
                { id: "gpu-b", utilization: 2 },
                { id: "gpu-c", utilization: 3 },
              ],
            }),
          }),
        ]),
      );
      const history = queryHistory(
        ctx.db,
        {
          machineId: "machine-1",
          metrics: ["gpu.utilization"],
          fromMs: from,
          toMs: from + 10_000,
        },
        { maxSeries: 2 },
      );
      assert.equal(history.truncated, true);
      assert.equal(history.series.length, 2);
    } finally {
      await teardown(ctx);
    }
  });

  it("rejects invalid queries with a HistoryQueryError", async () => {
    const ctx = setup();
    try {
      const query = (overrides: Record<string, unknown>) =>
        queryHistory(ctx.db, {
          machineId: "machine-1",
          metrics: ["cpu.usage"],
          fromMs: 0,
          toMs: 1000,
          ...overrides,
        } as never);
      assert.doesNotThrow(() => query({ metrics: ["cpu.core"], coreIds: Array.from({ length: 128 }, (_, index) => `core:${index}`) }));
      assert.throws(() => query({ metrics: ["cpu.core"], coreIds: ["core:1024"] }), /coreIds/);
      assert.throws(() => query({ metrics: ["cpu.core"], coreIds: ["core:1", "core:1"] }), /coreIds/);
      assert.throws(() => query({ metrics: ["not.a.metric"] }), HistoryQueryError);
      assert.throws(() => query({ metrics: [...Object.keys(METRICS), "cpu.usage"] }), /metric names/);
      assert.throws(() => query({ fromMs: 1000, toMs: 1000 }), /before/);
      assert.throws(() => query({ fromMs: 0, toMs: 401 * 86_400_000 }), /400 days/);
      assert.throws(
        () => query({ resourceIds: Array.from({ length: 65 }, (_, index) => `r${index}`) }),
        /64/,
      );
      assert.throws(
        () =>
          queryHistory(ctx.db, {
            machineId: "machine-1",
            metrics: ["cpu.usage"],
            fromMs: 2000,
            toMs: 1000,
          }),
        /before/,
      );
    } finally {
      await teardown(ctx);
    }
  });

  it("recovers latest state and history after a restart", async () => {
    const ctx = setup();
    try {
      const from = Math.floor((Date.now() - 120_000) / 10_000) * 10_000;
      const result = await ingest(
        ctx.store,
        batch([
          makeMetricsRecord({ sequence: 1, capturedAt: from, payload: makePayload({ cpuUsage: 42 }) }),
        ]),
      );
      assert.equal(result.status, "committed");
      await ctx.store.close();

      const reopenedDb = openHubDatabase(ctx.path);
      const reopened = new TelemetryStore({ database: reopenedDb, flushIntervalMs: 60_000 });
      try {
        const latest = reopened.latestLive();
        assert.equal(latest.length, 1);
        assert.equal(latest[0]!.machineId, "machine-1");
        assert.equal(latest[0]!.sequence, 1);
        assert.equal(latest[0]!.payload.cpu.status, "ok");

        const history = queryHistory(reopenedDb, {
          machineId: "machine-1",
          metrics: ["cpu.usage"],
          fromMs: from,
          toMs: from + 10_000,
        });
        assert.equal(history.series.length, 1);
        assert.equal(history.series[0]!.points[0]!.avg, 42);
      } finally {
        await reopened.close();
      }
    } finally {
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });
});
