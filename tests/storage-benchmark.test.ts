import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { performance } from "node:perf_hooks";
import { describe, it } from "node:test";
import { openHubDatabase } from "../src/hub/storage/db";
import { TelemetryStore, type IngestBatch, type IngestResult } from "../src/hub/storage/telemetry-store";
import { makeMetricsRecord, makePayload, type PayloadOptions } from "./storage-fixtures";

const RECORDS = 5000;
const BATCH_SIZE = 32;
const BATCHES_PER_TRANSACTION = 16; // 512 records at the default transaction bound

describe("storage benchmark", () => {
  it("prints ingestion throughput and maximum transaction duration", async () => {
    const dir = mkdtempSync(join(tmpdir(), "rigscope-storage-bench-"));
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new TelemetryStore({
      database: db,
      flushIntervalMs: 60_000,
      maxQueuedRecords: 20_000,
      maxRecordsPerTransaction: 512,
    });
    try {
      const base = Math.floor((Date.now() - RECORDS * 5000 - 600_000) / 1000) * 1000;
      const payloadOptions: PayloadOptions = {
        cores: 48,
        gpus: [
          { id: "gpu-0", utilization: 50, memoryUsed: 1024, memoryTotal: 4096, temperature: 60, power: 200 },
          { id: "gpu-1", utilization: 40, memoryUsed: 2048, memoryTotal: 4096, temperature: 55, power: 180 },
        ],
        filesystems: Array.from({ length: 10 }, (_, index) => ({
          id: `fs-${index}`,
          used: index * 100,
          total: 10_000,
        })),
        disks: Array.from({ length: 10 }, (_, index) => ({
          id: `disk-${index}`,
          read: index * 10,
          write: index * 5,
        })),
        interfaces: Array.from({ length: 10 }, (_, index) => ({
          id: `eth-${index}`,
          rx: index * 100,
          tx: index * 50,
        })),
      };

      const batches: IngestBatch[] = [];
      for (let index = 0; index < RECORDS; index += BATCH_SIZE) {
        const records = [];
        for (let offset = 0; offset < BATCH_SIZE && index + offset < RECORDS; offset += 1) {
          const sequence = index + offset + 1;
          records.push(
            makeMetricsRecord({
              sequence,
              capturedAt: base + (sequence - 1) * 5000,
              payload: makePayload({
                ...payloadOptions,
                cpuUsage: sequence % 100,
              }),
            }),
          );
        }
        batches.push({
          machineId: "bench-machine",
          collectorId: "collector-1",
          mode: "live",
          expectedIntervalMs: 5000,
          hostname: "bench-host",
          records,
        });
      }

      const pending: Array<Promise<IngestResult>> = [];
      let maxTransactionMs = 0;
      const started = performance.now();
      for (let index = 0; index < batches.length; index += 1) {
        pending.push(store.enqueue(batches[index]!));
        if ((index + 1) % BATCHES_PER_TRANSACTION === 0) {
          await store.flushNow();
          maxTransactionMs = Math.max(maxTransactionMs, store.stats().lastFlushDurationMs ?? 0);
        }
      }
      await store.flushNow();
      maxTransactionMs = Math.max(maxTransactionMs, store.stats().lastFlushDurationMs ?? 0);
      const elapsedMs = performance.now() - started;

      const results = await Promise.all(pending);
      assert.ok(results.every((result) => result.status === "committed"));
      const stats = store.stats();
      assert.equal(stats.committedRecords, RECORDS);
      assert.equal(stats.writeFailures, 0);

      const rawPoints = Number(
        (db.db.prepare("SELECT COUNT(*) AS n FROM points_raw").get() as { n: number }).n,
      );
      // 172 points/record: CPU 1+48+4 optional, memory 5, swap 2, GPUs 10,
      // filesystems 3*10, disks 5*10, I/O pressure 2, network 20.
      assert.equal(rawPoints, RECORDS * 172);
      assert.ok(elapsedMs < 20_000, "benchmark run should stay bounded");

      const throughput = RECORDS / (elapsedMs / 1000);
      console.log(
        `[storage benchmark] ${RECORDS} records, ${rawPoints} raw points in ${elapsedMs.toFixed(0)}ms ` +
          `-> ${throughput.toFixed(0)} records/s; max transaction ${maxTransactionMs}ms; ` +
          `db ${(stats.dbBytes / 1024 / 1024).toFixed(1)}MiB`,
      );
    } finally {
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
