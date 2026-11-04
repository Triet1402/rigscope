/**
 * Bounded synthetic measurement runner.
 *
 * This is deliberately not a capacity claim. It writes clearly labelled
 * synthetic records through the real TelemetryStore into a temporary hub
 * database, then reports ingestion latency, event-loop delay, queue/store
 * counters, memory, disk size and one history-query latency. It never opens a
 * service socket, never contacts a hub, and removes its temporary database
 * unless --keep is set.
 */

import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { monitorEventLoopDelay, performance } from "node:perf_hooks";
import { openHubDatabase } from "../hub/storage/db";
import { TelemetryStore, type IngestResult } from "../hub/storage/telemetry-store";
import { queryHistory } from "../hub/storage/history-query";
import type { MetricsPayload, MetricsRecord } from "../contracts";
import { fileSize, ensurePrivateDir, resolvePath } from "./fs";

export type MeasureOptions = {
  machines?: number;
  batches?: number;
  recordsPerBatch?: number;
  gpus?: number;
  historyPoints?: number;
  viewers?: number;
  /** How many History requests may be in flight before awaiting the round. */
  historyConcurrency?: number;
  mode?: "live" | "replay" | "mixed";
  dir?: string;
  keep?: boolean;
  now?: () => number;
};

export type MeasureReport = {
  tool: "rigscope-operations-measure";
  synthetic: true;
  disclaimer: string;
  generatedAt: string;
  node: string;
  config: {
    machines: number;
    batches: number;
    recordsPerBatch: number;
    totalRecords: number;
    gpusPerMachine: number;
    mode: "live" | "replay" | "mixed";
    historyPoints: number;
    viewers: number;
    historyConcurrency: number;
    historyBudget: { maxRowsScanned: number; maxElapsedMs: number };
  };
  payload: {
    bytesPerPayload: number;
    pointsPerRecord: number;
    perCore: number;
    gpus: number;
    filesystems: number;
    disks: number;
    interfaces: number;
  };
  results: {
    accepted: number;
    rejected: number;
    busy: number;
    wallMs: number;
    ingestLatencyMs: { min: number; avg: number; p95: number; max: number };
    eventLoopDelayMs: { p50: number; p99: number; max: number };
    eventLoopUtilization: number;
    rssBytes: { start: number; end: number; peak: number };
    database: { bytes: number; walBytes: number };
    store: ReturnType<TelemetryStore["stats"]> | null;
    history: {
      queryMs: number;
      maxQueryMs: number;
      p95QueryMs: number;
      wallMs: number;
      queries: number;
      concurrency: number;
      viewers: number;
      series: number;
      points: number;
      truncated: boolean;
      tier: string;
    };
  };
  artifacts: { directory: string; kept: boolean };
};

const DISCLAIMER =
  "Synthetic bounded workload, not a capacity, latency, or hardware guarantee. Values depend on this machine, disk, Node version, payload and selected options.";

function boundedInt(value: unknown, fallback: number, min: number, max: number, name: string): number {
  if (value === undefined) return fallback;
  const number = typeof value === "number" ? value : Number(value);
  if (!Number.isSafeInteger(number) || number < min || number > max) {
    throw new Error(`${name} must be an integer from ${min} to ${max}`);
  }
  return number;
}

function metricsPayload(gpus: number): MetricsPayload {
  const devices = Array.from({ length: gpus }, (_, index) => ({
    id: `gpu:measure-${index}`,
    status: "ok" as const,
    utilizationPercent: (index * 17 + 11) % 100,
    memoryUsedBytes: (index + 1) * 1024 ** 3,
    memoryTotalBytes: 24 * 1024 ** 3,
    temperatureC: 50 + index,
    powerWatts: 120 + index * 10,
  }));
  return {
    cpu: {
      status: "ok",
      usagePercent: 37.5,
      perCore: Array.from({ length: 8 }, (_, index) => (index * 11) % 100),
    },
    memory: { status: "ok", usedBytes: 6 * 1024 ** 3, totalBytes: 32 * 1024 ** 3 },
    swap: { status: "ok", usedBytes: 512 * 1024 ** 2, totalBytes: 8 * 1024 ** 3 },
    gpus: { status: "ok", devices },
    disks: {
      status: "ok",
      filesystems: [
        { id: "fs:/", status: "ok", usedBytes: 100 * 1024 ** 3, totalBytes: 500 * 1024 ** 3 },
        { id: "fs:/data", status: "ok", usedBytes: 300 * 1024 ** 3, totalBytes: 1024 ** 4 },
      ],
      devices: [
        { id: "disk:nvme0", status: "ok", readBytesPerSec: 12 * 1024 ** 2, writeBytesPerSec: 4 * 1024 ** 2 },
        { id: "disk:nvme1", status: "ok", readBytesPerSec: 3 * 1024 ** 2, writeBytesPerSec: 9 * 1024 ** 2 },
      ],
    },
    network: {
      status: "ok",
      interfaces: [
        { id: "net:eth0", status: "ok", rxBytesPerSec: 8 * 1024 ** 2, txBytesPerSec: 2 * 1024 ** 2 },
        { id: "net:lo", status: "ok", rxBytesPerSec: 1024, txBytesPerSec: 1024 },
      ],
    },
  };
}

export async function measure(options: MeasureOptions = {}): Promise<MeasureReport> {
  const machines = boundedInt(options.machines, 2, 1, 1_000, "machines");
  const batches = boundedInt(options.batches, 30, 1, 2_000, "batches");
  const recordsPerBatch = boundedInt(options.recordsPerBatch, 5, 1, 32, "recordsPerBatch");
  const gpus = boundedInt(options.gpus, 2, 0, 16, "gpus");
  const historyPoints = boundedInt(options.historyPoints, 600, 1, 2_000, "historyPoints");
  const viewers = boundedInt(options.viewers, 1, 1, 50, "viewers");
  const historyConcurrency = boundedInt(options.historyConcurrency, 1, 1, 64, "historyConcurrency");
  const mode = options.mode ?? "mixed";
  if (mode !== "live" && mode !== "replay" && mode !== "mixed") {
    throw new Error("mode must be live, replay or mixed");
  }
  const totalRecords = batches * recordsPerBatch;
  if (totalRecords > 100_000) throw new Error("measurement is bounded to 100000 records");
  const now = options.now ?? Date.now;
  const startedMs = now();
  const dir = options.dir
    ? resolvePath(options.dir, "measurement directory")
    : mkdtempSync(join(tmpdir(), "rigscope-measure-"));
  ensurePrivateDir(dir);
  const databasePath = join(dir, "measure.sqlite");
  const database = openHubDatabase(databasePath);
  const store = new TelemetryStore({
    database,
    maxRecordsPerTransaction: 32,
    maxQueuedRecords: totalRecords + 64,
    flushIntervalMs: 20,
    maxDatabaseBytes: null,
  });
  const delay = monitorEventLoopDelay({ resolution: 10 });
  delay.enable();
  const wallStart = performance.now();
  const rssStart = process.memoryUsage().rss;
  let rssPeak = rssStart;
  const latencies: number[] = [];
  const results: IngestResult[] = [];
  const sequences = new Map<number, number>();
  const capturedBase = startedMs - totalRecords * 1_000;
  try {
    const promises: Array<Promise<{ result: IngestResult; ms: number }>> = [];
    for (let batchIndex = 0; batchIndex < batches; batchIndex += 1) {
      const machineIndex = batchIndex % machines;
      const payload = metricsPayload(gpus);
      const records: MetricsRecord[] = [];
      for (let recordIndex = 0; recordIndex < recordsPerBatch; recordIndex += 1) {
        const sequence = (sequences.get(machineIndex) ?? 0) + 1;
        sequences.set(machineIndex, sequence);
        records.push({
          collectorId: `measure-collector-${machineIndex}`,
          runId: `measure-run-${machineIndex}`,
          bootId: "measure-boot",
          sequence,
          capturedAt: new Date(capturedBase + batchIndex * 1_000 + recordIndex).toISOString(),
          kind: "metrics",
          payload,
        });
      }
      const batchMode =
        mode === "mixed" ? (batchIndex % 4 === 0 ? "replay" : "live") : mode;
      const enqueueAt = performance.now();
      const promise = store
        .enqueue({
          machineId: `measure-machine-${machineIndex}`,
          collectorId: `measure-collector-${machineIndex}`,
          mode: batchMode,
          expectedIntervalMs: 5_000,
          hostname: `measure-host-${machineIndex}`,
          records,
        })
        .then((result) => ({ result, ms: performance.now() - enqueueAt }));
      promises.push(promise);
      rssPeak = Math.max(rssPeak, process.memoryUsage().rss);
    }
    // TelemetryStore's flush timer is unref'd so an idle hub may exit. The
    // measurement must drive the queue explicitly or a short CLI run would end
    // with pending promises and no referenced handle.
    await store.flushNow();
    const settled = await Promise.all(promises);
    for (const item of settled) {
      results.push(item.result);
      latencies.push(item.ms);
      rssPeak = Math.max(rssPeak, process.memoryUsage().rss);
    }
    const wallMs = performance.now() - wallStart;
    const sorted = [...latencies].sort((a, b) => a - b);
    const percentile = (fraction: number): number =>
      sorted.length === 0 ? 0 : sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * fraction))]!;
    const accepted = results.reduce((sum, result) => sum + (result.status === "committed" ? result.accepted.length : 0), 0);
    const rejected = results.reduce(
      (sum, result) => sum + (result.status === "committed" ? result.rejected.length : 0),
      0,
    );
    const busy = results.filter((result) => result.status === "busy").length;
    const firstMachine = "measure-machine-0";
    const historyDurations: number[] = [];
    const historyWallStart = performance.now();
    const historyHolder: { value: ReturnType<typeof queryHistory> | null } = { value: null };
    const runHistoryQuery = async (): Promise<void> => {
      // Yield first so a whole round is genuinely outstanding before any one
      // synchronous SQLite query starts. The hub is single-threaded too, so
      // this models overlapping browser requests rather than parallel SQL.
      await new Promise<void>((resolve) => setImmediate(resolve));
      const historyStart = performance.now();
      const result = queryHistory(
        database,
        {
          machineId: firstMachine,
          metrics: ["cpu.usage", "memory.used", "gpu.utilization"],
          fromMs: capturedBase,
          toMs: startedMs + 1_000,
          maxPoints: historyPoints,
        },
        { maxRowsScanned: 25_000, maxElapsedMs: 100 },
      );
      historyDurations.push(performance.now() - historyStart);
      historyHolder.value = result;
    };
    for (let viewer = 0; viewer < viewers; viewer += historyConcurrency) {
      const round = Math.min(historyConcurrency, viewers - viewer);
      await Promise.all(Array.from({ length: round }, runHistoryQuery));
    }
    if (historyHolder.value === null) throw new Error("measurement produced no history query");
    const history = historyHolder.value;
    const historyWallMs = performance.now() - historyWallStart;
    const sortedHistory = [...historyDurations].sort((a, b) => a - b);
    const historyPercentile = (fraction: number): number =>
      sortedHistory.length === 0
        ? 0
        : sortedHistory[Math.min(sortedHistory.length - 1, Math.floor(sortedHistory.length * fraction))]!;
    delay.disable();
    const storeStats = store.stats();
    const eventLoopDelayMs = {
      p50: delay.percentile(50) / 1e6,
      p99: delay.percentile(99) / 1e6,
      max: delay.max / 1e6,
    };
    const utilization = performance.eventLoopUtilization();
    const payloadBytes = Buffer.byteLength(JSON.stringify(metricsPayload(gpus)), "utf8");
    const rssEnd = process.memoryUsage().rss;
    rssPeak = Math.max(rssPeak, rssEnd);
    const report: MeasureReport = {
      tool: "rigscope-operations-measure",
      synthetic: true,
      disclaimer: DISCLAIMER,
      generatedAt: new Date(now()).toISOString(),
      node: process.version,
      config: {
        machines,
        batches,
        recordsPerBatch,
        totalRecords,
        gpusPerMachine: gpus,
        mode,
        historyPoints,
        viewers,
        historyConcurrency,
        historyBudget: { maxRowsScanned: 25_000, maxElapsedMs: 100 },
      },
      payload: {
        bytesPerPayload: payloadBytes,
        pointsPerRecord: 25 + gpus * 5,
        perCore: 8,
        gpus,
        filesystems: 2,
        disks: 2,
        interfaces: 2,
      },
      results: {
        accepted,
        rejected,
        busy,
        wallMs,
        ingestLatencyMs: {
          min: sorted[0] ?? 0,
          avg: sorted.length === 0 ? 0 : sorted.reduce((sum, value) => sum + value, 0) / sorted.length,
          p95: percentile(0.95),
          max: sorted.at(-1) ?? 0,
        },
        eventLoopDelayMs,
        eventLoopUtilization: utilization.utilization,
        rssBytes: { start: rssStart, end: rssEnd, peak: rssPeak },
        database: {
          bytes: database.sizeBytes(),
          walBytes: fileSize(`${databasePath}-wal`),
        },
        store: storeStats,
        history: {
          queryMs: historyDurations.reduce((sum, value) => sum + value, 0) / historyDurations.length,
          maxQueryMs: Math.max(0, ...historyDurations),
          p95QueryMs: historyPercentile(0.95),
          wallMs: historyWallMs,
          queries: historyDurations.length,
          concurrency: Math.min(historyConcurrency, viewers),
          viewers,
          series: history.series.length,
          points: history.series.reduce((sum, series) => sum + series.points.length, 0),
          truncated: history.truncated,
          tier: history.tier,
        },
      },
      artifacts: { directory: dir, kept: options.keep === true },
    };
    return report;
  } finally {
    delay.disable();
    await store.close();
    if (options.keep !== true) rmSync(dir, { recursive: true, force: true });
  }
}
