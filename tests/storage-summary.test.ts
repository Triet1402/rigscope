/**
 * Opt-in persistent history: one compact ten-second transaction, dedup by
 * receipt, and a history ladder that no longer writes one raw row per sample.
 */

import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";

import { openHubDatabase } from "../src/hub/storage/db";
import { resolveRetentionPolicy } from "../src/hub/storage/retention";
import { TelemetryStore, type IngestBatch } from "../src/hub/storage/telemetry-store";
import { makeMetricsRecord, makePayload } from "./storage-fixtures";

const BASE = Date.parse("2026-11-01T12:00:00Z");

function tempDir(): string {
  return mkdtempSync(join(tmpdir(), "rigscope-summary-"));
}

function batch(sequenceBase: number, fromMs: number, count: number): IngestBatch {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    mode: "live",
    expectedIntervalMs: 1_000,
    records: Array.from({ length: count }, (_, index) =>
      makeMetricsRecord({
        sequence: sequenceBase + index,
        capturedAt: fromMs + index * 1_000,
        payload: makePayload({ cpuUsage: 10 + index * 10 }),
      }),
    ),
  };
}

async function ingest(store: TelemetryStore, value: IngestBatch) {
  const promise = store.enqueue(value);
  await store.flushNow();
  return promise;
}

async function setup(now: () => number) {
  const dir = tempDir();
  const db = openHubDatabase(join(dir, "hub.db"));
  const store = new TelemetryStore({
    database: db,
    now,
    storageTier: "summary",
    flushIntervalMs: 60_000,
    maxRecordsPerTransaction: 8_192,
    retention: {
      rawMs: 0,
      tenSecondMs: 3_600_000,
      oneMinuteMs: 86_400_000,
      oneHourMs: 604_800_000,
      receiptsMs: 7 * 86_400_000,
      gapsMs: 604_800_000,
    },
  });
  return { dir, db, store };
}

function count(db: ReturnType<typeof openHubDatabase>, sql: string): number {
  return Number((db.db.prepare(sql).get() as { n: number | bigint }).n);
}

describe("persistent summary history", () => {
  it("writes one compact row per series/bucket and still preserves the spike", async () => {
    const now = BASE + 60_000;
    const ctx = await setup(() => now);
    try {
      const outcome = await ingest(ctx.store, batch(1, BASE, 10));
      assert.equal(outcome.status, "committed");
      assert.equal(count(ctx.db, "SELECT COUNT(*) AS n FROM points_raw"), 0, "summary mode never writes raw rows");
      // One ten-second bucket per emitted series; both larger tiers receive
      // exactly the same one bucket at this point.
      const summaries = count(ctx.db, "SELECT COUNT(*) AS n FROM points_10s");
      assert.ok(summaries > 1);
      assert.equal(count(ctx.db, "SELECT COUNT(*) AS n FROM points_1m"), summaries);
      assert.equal(count(ctx.db, "SELECT COUNT(*) AS n FROM points_1h"), summaries);

      const history = ctx.store.queryHistory({
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: BASE,
        toMs: BASE + 10_000,
      });
      assert.equal(history.tier, "10s");
      assert.deepEqual(history.series[0]!.points, [
        { t: BASE, min: 10, max: 100, avg: 55 },
      ]);
    } finally {
      await ctx.store.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("deduplicates a retried batch and does not double-count rollups", async () => {
    const now = BASE + 60_000;
    const ctx = await setup(() => now);
    try {
      const original = batch(1, BASE, 5);
      await ingest(ctx.store, original);
      const before = count(ctx.db, "SELECT COUNT(*) AS n FROM points_10s");
      const retry = await ingest(ctx.store, batch(1, BASE, 5));
      assert.equal(retry.status, "committed");
      if (retry.status === "committed") assert.equal(retry.accepted.length, 5);
      assert.equal(count(ctx.db, "SELECT COUNT(*) AS n FROM points_10s"), before);
      const row = ctx.db.db
        .prepare(
          "SELECT p.count AS count FROM points_10s p JOIN series s USING(series_id) " +
            "WHERE s.metric = 'cpu.usage' AND p.bucket_ms = ?",
        )
        .get(BASE) as { count: number | bigint };
      assert.equal(Number(row.count), 5);
    } finally {
      await ctx.store.close();
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });

  it("keeps deduplication receipts independent of a short history window", () => {
    const policy = resolveRetentionPolicy({ rawMs: 1_000, tenSecondMs: 1_000 });
    assert.equal(policy.rawMs, 1_000);
    assert.equal(policy.tenSecondMs, 1_000);
    assert.equal(policy.receiptsMs, 7 * 86_400_000);
  });

  it("restores committed summaries and latest state after restart", async () => {
    const now = BASE + 60_000;
    const ctx = await setup(() => now);
    const path = join(ctx.dir, "hub.db");
    try {
      await ingest(ctx.store, batch(1, BASE, 3));
      await ctx.store.close();
      const reopenedDb = openHubDatabase(path);
      const reopened = new TelemetryStore({
        database: reopenedDb,
        now: () => now,
        storageTier: "summary",
        flushIntervalMs: 60_000,
        retention: {
          rawMs: 0,
          tenSecondMs: 3_600_000,
          oneMinuteMs: 86_400_000,
          oneHourMs: 604_800_000,
          receiptsMs: 7 * 86_400_000,
          gapsMs: 604_800_000,
        },
      });
      try {
        assert.equal(reopened.latestLive()[0]?.sequence, 3);
        const history = reopened.queryHistory({
          machineId: "machine-1",
          metrics: ["cpu.usage"],
          fromMs: BASE,
          toMs: BASE + 10_000,
        });
        assert.equal(history.series[0]!.points[0]!.avg, 20);
      } finally {
        await reopened.close();
      }
    } finally {
      rmSync(ctx.dir, { recursive: true, force: true });
    }
  });
});
