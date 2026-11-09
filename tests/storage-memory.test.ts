/**
 * RAM-only history: bounded aggregation, eviction, late/duplicate samples,
 * gaps and restart semantics. No test asserts a particular number of disk
 * pages; the contract is that captured history and latest snapshots are gone
 * after reopening the hub.
 */

import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";

import { openHubDatabase } from "../src/hub/storage/db";
import { MemoryTelemetryStore } from "../src/hub/storage/memory-store";
import type { TelemetryRecord } from "../src/contracts";
import { makeGapRecord, makeMetricsRecord, makePayload } from "./storage-fixtures";

const BASE = Date.parse("2026-11-01T12:00:00Z");

function tempDir(): string {
  return mkdtempSync(join(tmpdir(), "rigscope-memory-"));
}

function batch(records: TelemetryRecord[]) {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    mode: "live" as const,
    expectedIntervalMs: 1_000,
    records,
  };
}

function metric(sequence: number, atMs: number, options: {
  cpuUsage?: number;
  cores?: number;
  coreValue?: number;
  unavailableCpu?: boolean;
} = {}) {
  return makeMetricsRecord({
    sequence,
    capturedAt: atMs,
    payload: makePayload({
      cpuUsage: options.cpuUsage ?? 10,
      cores: options.cores ?? 2,
      coreValue: options.coreValue ?? 5,
      ...(options.unavailableCpu ? { unavailable: { cpu: true } } : {}),
    }),
  });
}

describe("memory telemetry history", () => {
  it("aggregates min/max/sum/count over ten-second buckets and keeps spikes", async () => {
    const dir = tempDir();
    const now = BASE + 180_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({ database: db, now: () => now });
    try {
      const from = BASE;
      const records = [
        metric(1, from, { cpuUsage: 10 }),
        metric(2, from + 1_000, { cpuUsage: 10 }),
        metric(3, from + 2_000, { cpuUsage: 100 }),
        metric(4, from + 3_000, { cpuUsage: 30 }),
        metric(5, from + 10_000, { cpuUsage: 50 }),
      ];
      const result = await store.enqueue(batch(records));
      assert.equal(result.status, "committed");
      if (result.status !== "committed") return;
      assert.equal(result.accepted.length, 5);

      const history = store.queryHistory({
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: from,
        toMs: from + 30_000,
        maxPoints: 600,
      });
      // The query starts before the two-minute 1s window, so it uses the 10s
      // tier and the bucket still preserves the one-second spike.
      assert.equal(history.tier, "10s");
      assert.deepEqual(history.series[0]!.points, [
        { t: from, min: 10, max: 100, avg: 37.5 },
        { t: from + 10_000, min: 50, max: 50, avg: 50 },
      ]);
      assert.equal(history.truncated, false);
    } finally {
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("keeps unavailable buckets distinct from absent buckets and reports gaps", async () => {
    const dir = tempDir();
    const now = BASE + 180_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({ database: db, now: () => now });
    try {
      await store.enqueue(batch([
        metric(1, BASE, { unavailableCpu: true }),
        metric(2, BASE + 60_000, { cpuUsage: 40 }),
        makeGapRecord({
          sequence: 3,
          capturedAt: BASE + 61_000,
          fromMs: BASE + 30_000,
          toMs: BASE + 35_000,
          dropped: 4,
          reason: "outbox-bytes",
        }),
      ]));
      const history = store.queryHistory({
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: BASE,
        toMs: BASE + 70_000,
      });
      assert.deepEqual(history.series[0]!.points, [
        { t: BASE, min: null, max: null, avg: null },
        { t: BASE + 60_000, min: 40, max: 40, avg: 40 },
      ]);
      // A live interval longer than the advertised cadence becomes a missing
      // gap; the explicit dropped gap remains separate.
      assert.ok(history.gaps.some((gap) => gap.kind === "missing"));
      const dropped = history.gaps.find((gap) => gap.kind === "dropped");
      assert.equal(dropped?.droppedRecords, 4);
    } finally {
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("deduplicates replayed sequences and folds late samples into the right bucket", async () => {
    const dir = tempDir();
    const now = BASE + 180_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({ database: db, now: () => now });
    try {
      await store.enqueue(batch([
        metric(1, BASE, { cpuUsage: 10 }),
        metric(2, BASE + 5_000, { cpuUsage: 20 }),
      ]));
      const duplicate = await store.enqueue(batch([metric(2, BASE + 5_000, { cpuUsage: 999 })]));
      assert.equal(duplicate.status, "committed");
      if (duplicate.status === "committed") {
        assert.equal(duplicate.accepted.length, 1);
        assert.equal(store.stats().duplicateRecords, 1);
      }
      // A late sample for the already-open 10s bucket is aggregated, not
      // overwritten or dropped.
      await store.enqueue(batch([metric(3, BASE + 1_000, { cpuUsage: 30 })]));
      const history = store.queryHistory({
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: BASE,
        toMs: BASE + 20_000,
      });
      const point = history.series[0]!.points[0]!;
      assert.equal(point.min, 10);
      assert.equal(point.max, 30);
      assert.equal(point.avg, 20);
    } finally {
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("bounds memory by evicting fine tiers/cardinality instead of growing forever", async () => {
    const dir = tempDir();
    const now = BASE + 180_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({
      database: db,
      now: () => now,
      budgetBytes: 900 * 1024,
      tierRetentionMs: { "1s": 120_000, "10s": 3_600_000, "1m": 86_400_000, "1h": 604_800_000 },
    });
    try {
      for (let machine = 0; machine < 20; machine += 1) {
        await store.enqueue({
          machineId: `machine-${machine}`,
          collectorId: `collector-${machine}`,
          mode: "live",
          expectedIntervalMs: 1_000,
          records: [makeMetricsRecord({
            collectorId: `collector-${machine}`,
            runId: `run-${machine}`,
            sequence: 1,
            capturedAt: BASE,
            payload: makePayload({
              cores: 4,
              gpus: [{ id: "gpu-0", utilization: 50, memoryUsed: 1, memoryTotal: 2 }],
              filesystems: [{ id: "fs:/", used: 1, total: 2 }],
              disks: [{ id: "disk-0" }],
              interfaces: [{ id: "eth0" }],
            }),
          })],
        });
      }
      const status = store.historyStatus();
      assert.ok(status.memory);
      assert.ok(status.memory!.usedBytes <= status.memory!.budgetBytes);
      assert.ok(
        status.memory!.evictedSeries > 0 || status.memory!.rejectedSeries > 0 || status.memory!.pressure,
        "the tiny budget must be reported as pressure/eviction",
      );
      assert.ok(status.memory!.seriesCount > 0, "the newest series remain readable");
    } finally {
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("keeps actually retained tier bytes within budget across repeated tier and whole-series eviction", async () => {
    // Regression: dropping a tier used to subtract its bytes from the global
    // counter without shrinking the series total, so a later whole-series
    // eviction subtracted the same allocation again. The store then believed it
    // had room and kept accepting new cardinality while retaining far more than
    // the configured budget. Recompute the retained bytes from the public tier
    // breakdown instead of trusting only `usedBytes`.
    const dir = tempDir();
    const now = BASE + 180_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const budgetBytes = 900 * 1024;
    const store = new MemoryTelemetryStore({
      database: db,
      now: () => now,
      budgetBytes,
      tierRetentionMs: { "1s": 120_000, "10s": 3_600_000, "1m": 86_400_000, "1h": 604_800_000 },
    });
    try {
      for (let round = 0; round < 3; round += 1) {
        // Every round adds new machines and grows resource cardinality, so the
        // same budget must survive repeated fine-tier trimming and whole-series
        // eviction rather than being fooled by a one-off accounting error.
        for (let machine = 0; machine < 20; machine += 1) {
          const index = round * 20 + machine;
          await store.enqueue({
            machineId: `machine-${index}`,
            collectorId: `collector-${index}`,
            mode: "live",
            expectedIntervalMs: 1_000,
            records: [makeMetricsRecord({
              collectorId: `collector-${index}`,
              runId: `run-${index}`,
              sequence: 1,
              capturedAt: BASE + round * 1_000,
              payload: makePayload({
                cores: 4,
                gpus: Array.from({ length: round + 1 }, (_, gpu) => ({
                  id: `gpu-${gpu}`,
                  utilization: 50,
                  memoryUsed: 1,
                  memoryTotal: 2,
                })),
                filesystems: Array.from({ length: round + 1 }, (_, fs) => ({
                  id: `fs:${fs}`,
                  used: 1,
                  total: 2,
                })),
                disks: [{ id: "disk-0" }],
                interfaces: Array.from({ length: round + 1 }, (_, net) => ({ id: `eth${net}` })),
              }),
            })],
          });
        }

        const memory = store.historyStatus().memory;
        assert.ok(memory);
        const retainedTierBytes = memory!.tiers.reduce((total, tier) => total + tier.bytes, 0);
        assert.ok(
          retainedTierBytes <= budgetBytes,
          `round ${round}: retained ${retainedTierBytes} bytes of tier arrays must not exceed ` +
            `the ${budgetBytes}-byte budget`,
        );
        assert.ok(memory!.usedBytes <= budgetBytes, `round ${round}: usedBytes stayed within budget`);
      }

      const memory = store.historyStatus().memory;
      assert.ok(memory);
      assert.ok(
        memory!.evictedSeries > 0,
        "repeated cardinality churn must actually exercise tier and whole-series eviction",
      );
      assert.ok(memory!.seriesCount > 0, "the most recently seen series remain readable");
      assert.ok(
        memory!.tiers.reduce((total, tier) => total + tier.bytes, 0) > 0,
        "the budget must retain some tier data rather than evicting everything",
      );
    } finally {
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("loses history and latest snapshots across a memory-mode restart but keeps metadata", async () => {
    const dir = tempDir();
    const path = join(dir, "hub.db");
    const now = BASE + 180_000;
    let db = openHubDatabase(path);
    let store = new MemoryTelemetryStore({ database: db, now: () => now });
    try {
      await store.enqueue(batch([metric(1, BASE, { cpuUsage: 42 })]));
      store.setMachineMeta("machine-1", { displayName: "Workstation", tags: ["lab"] });
      assert.equal(store.latestLive().length, 0);
      await store.close();

      db = openHubDatabase(path);
      store = new MemoryTelemetryStore({ database: db, now: () => now });
      const history = store.queryHistory({
        machineId: "machine-1",
        metrics: ["cpu.usage"],
        fromMs: BASE,
        toMs: BASE + 10_000,
      });
      assert.deepEqual(history.series, []);
      assert.equal(store.latestLive().length, 0);
      assert.deepEqual(store.getMachineMeta("machine-1"), { displayName: "Workstation", tags: ["lab"] });
    } finally {
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("answers per-core queries from the coarser policy without fabricating points", async () => {
    const dir = tempDir();
    const now = BASE + 180_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({ database: db, now: () => now });
    try {
      await store.enqueue(batch([metric(1, BASE, { cores: 4, coreValue: 25 })]));
      const history = store.queryHistory({
        machineId: "machine-1",
        metrics: ["cpu.core"],
        coreIds: ["core:2"],
        fromMs: BASE,
        toMs: BASE + 10_000,
      });
      assert.equal(history.tier, "10s");
      assert.deepEqual(history.series.map((series) => [series.metric, series.resourceId]), [["cpu.core", "core:2"]]);
      assert.equal(history.series[0]!.points[0]!.avg, 25);
    } finally {
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
