#!/usr/bin/env node
/**
 * Repeatable RAM-vs-persistent telemetry benchmark.
 *
 * It drives the real stores with a realistic ~183-series, one-second payload,
 * measures process-attributed disk writes from /proc/self/io (not database
 * size), and reports ingestion/history latency plus RSS. Run it on the target
 * host before making capacity claims:
 *
 *   npm run benchmark:telemetry -- --machines 2 --seconds 120
 *   npm run benchmark:telemetry -- --machines 10 --seconds 60 --json
 *
 * Modes:
 *   memory      default hub mode; no telemetry rows are written.
 *   persistent  compact 10s summaries, one transaction every 10 seconds.
 *   legacy      the old raw-row path, one transaction per batch, for comparison.
 */

import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";
import { performance } from "node:perf_hooks";

import { openHubDatabase } from "../src/hub/storage/db";
import { MemoryTelemetryStore } from "../src/hub/storage/memory-store";
import { TelemetryStore } from "../src/hub/storage/telemetry-store";
import type { MetricsPayload, MetricsRecord } from "../src/contracts";

type Mode = "memory" | "persistent" | "legacy";

type ModeReport = {
  mode: Mode;
  machines: number;
  records: number;
  seriesPerMachine: number;
  simulatedSeconds: number;
  /** Ingestion-phase storage writes, excluding the final close checkpoint. */
  steadyDiskWriteBytes: number | null;
  /** Final close/checkpoint writes; one-time per hub run. */
  closeDiskWriteBytes: number | null;
  diskWriteBytes: number | null;
  diskWriteBytesPerRecord: number | null;
  diskWriteBytesPerSimulatedHour: number | null;
  ingestWallMs: number;
  /** Synchronous enqueue/live-acceptance cost. */
  enqueueLatencyMs: { avg: number; p95: number; max: number };
  /** Promise resolution, including the durable flush for persistent mode. */
  commitLatencyMs: { avg: number; p95: number; max: number };
  historyQueryMs: number;
  rssBytes: { start: number; end: number; peak: number };
  databaseBytes: number;
  memoryBudgetBytes: number | null;
  memoryUsedBytes: number | null;
  memoryPressure: boolean | null;
  retainedSeries: number | null;
  rawRows: number | null;
  summaryRows: number | null;
};

const BASE = 1793500000000;

function payload(): MetricsPayload {
  return {
    cpu: {
      status: "ok",
      usagePercent: 37.5,
      perCore: Array.from({ length: 32 }, (_, index) => (index * 7) % 100),
      temperatureC: 62,
      powerWatts: 95,
      iowaitPercent: 2.5,
      pressureSomePercent: 1.2,
    },
    memory: {
      status: "ok",
      usedBytes: 20 * 1024 ** 3,
      totalBytes: 64 * 1024 ** 3,
      pressureSomePercent: 0.8,
      pressureFullPercent: 0,
    },
    swap: { status: "ok", usedBytes: 512 * 1024 ** 2, totalBytes: 8 * 1024 ** 3 },
    gpus: {
      status: "ok",
      devices: Array.from({ length: 8 }, (_, index) => ({
        id: `GPU-${index}`,
        status: "ok" as const,
        utilizationPercent: (index * 11 + 5) % 100,
        memoryUsedBytes: (index + 1) * 1024 ** 3,
        memoryTotalBytes: 24 * 1024 ** 3,
        temperatureC: 50 + index,
        powerWatts: 120 + index * 10,
      })),
    },
    disks: {
      status: "ok",
      pressureSomePercent: 0.5,
      pressureFullPercent: 0,
      filesystems: Array.from({ length: 16 }, (_, index) => ({
        id: `fs-${index}`,
        status: "ok" as const,
        usedBytes: index * 10 * 1024 ** 3,
        totalBytes: 512 * 1024 ** 3,
        inodesUsed: index * 1000,
        inodesTotal: 100_000,
      })),
      devices: Array.from({ length: 4 }, (_, index) => ({
        id: `disk-${index}`,
        status: "ok" as const,
        readBytesPerSec: index * 1024 ** 2,
        writeBytesPerSec: index * 512 ** 2,
        readLatencyMs: 0.4 + index / 10,
        writeLatencyMs: 0.6 + index / 10,
        ioUtilizationPercent: 10 + index * 5,
      })),
    },
    network: {
      status: "ok",
      interfaces: Array.from({ length: 8 }, (_, index) => ({
        id: `eth-${index}`,
        status: "ok" as const,
        rxBytesPerSec: index * 1024 * 1024,
        txBytesPerSec: index * 512 * 1024,
      })),
    },
  };
}

function seriesCount(): number {
  // cpu 1 + 32 cores + 4 optional; memory 5; swap 2; GPU 8*5; filesystems
  // 16*4; I/O pressure 2; disks 4*5; interfaces 8*2.
  return 1 + 32 + 4 + 5 + 2 + 40 + 64 + 2 + 20 + 16;
}

function processWriteBytes(): number | null {
  try {
    const text = readFileSync("/proc/self/io", "utf8");
    const match = /^write_bytes:\s+(\d+)$/m.exec(text);
    return match === null ? null : Number(match[1]);
  } catch {
    return null;
  }
}

function percentile(sorted: readonly number[], fraction: number): number {
  if (sorted.length === 0) return 0;
  return sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * fraction))]!;
}

function count(db: ReturnType<typeof openHubDatabase>, table: string): number {
  const row = db.db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get() as { n: number | bigint } | undefined;
  return Number(row?.n ?? 0);
}

async function runMode(mode: Mode, machines: number, seconds: number, baseDir: string): Promise<ModeReport> {
  mkdirSync(baseDir, { recursive: true, mode: 0o700 });
  const dir = mkdtempSync(join(baseDir, `${mode}-`));
  const path = join(dir, "hub.db");
  const now = () => BASE + seconds * 1_000;
  const database = openHubDatabase(path);
  const store = mode === "memory"
    ? new MemoryTelemetryStore({ database, now, budgetBytes: 128 * 1024 * 1024 })
    : new TelemetryStore({
        database,
        now,
        storageTier: mode === "persistent" ? "summary" : "raw",
        flushIntervalMs: 60_000,
        maxRecordsPerTransaction: mode === "persistent" ? 8_192 : 32,
        maxDatabaseBytes: null,
        retention: mode === "persistent"
          ? {
              rawMs: 0,
              tenSecondMs: 2 * 3_600_000,
              oneMinuteMs: 30 * 86_400_000,
              oneHourMs: 400 * 86_400_000,
              receiptsMs: 7 * 86_400_000,
              gapsMs: 400 * 86_400_000,
            }
          : undefined,
      });
  const enqueueLatencies: number[] = [];
  const commitLatencies: number[] = [];
  // The store's flush timer is deliberately unref'd so an idle hub can exit.
  // Keep this measurement process alive while it awaits an explicit flush.
  const keepAlive = setInterval(() => {}, 1_000);
  const writeStart = processWriteBytes();
  const rssStart = process.memoryUsage().rss;
  let rssPeak = rssStart;
  const wallStart = performance.now();
  try {
    let pending: Array<{ promise: Promise<unknown>; started: number }> = [];
    for (let tick = 0; tick < seconds; tick += 1) {
      for (let machine = 0; machine < machines; machine += 1) {
        const record: MetricsRecord = {
          collectorId: `collector-${machine}`,
          runId: `run-${machine}`,
          bootId: "boot",
          sequence: tick + 1,
          capturedAt: new Date(BASE + tick * 1_000).toISOString(),
          kind: "metrics",
          payload: payload(),
        };
        const enqueueStart = performance.now();
        const promise = store.enqueue({
          machineId: `machine-${machine}`,
          collectorId: `collector-${machine}`,
          mode: "live",
          expectedIntervalMs: 1_000,
          records: [record],
        });
        enqueueLatencies.push(performance.now() - enqueueStart);
        pending.push({ promise, started: enqueueStart });
        rssPeak = Math.max(rssPeak, process.memoryUsage().rss);
      }
      // One-second collection. In persistent mode, a ten-second window is
      // written as one transaction; legacy writes each batch immediately.
      if (mode === "legacy" || (mode === "persistent" && (tick + 1) % 10 === 0)) {
        await store.flushNow();
        const settled = pending;
        pending = [];
        await Promise.all(settled.map((item) => item.promise));
        const nowMs = performance.now();
        for (const item of settled) commitLatencies.push(nowMs - item.started);
      }
    }
    if (pending.length > 0) {
      await store.flushNow();
      const settled = pending;
      pending = [];
      await Promise.all(settled.map((item) => item.promise));
      const nowMs = performance.now();
      for (const item of settled) commitLatencies.push(nowMs - item.started);
    }
    const wallMs = performance.now() - wallStart;
    const sortedEnqueue = [...enqueueLatencies].sort((a, b) => a - b);
    const sortedCommit = [...commitLatencies].sort((a, b) => a - b);
    const historyStart = performance.now();
    const history = store.queryHistory({
      machineId: "machine-0",
      metrics: ["cpu.usage", "memory.used", "gpu.utilization"],
      fromMs: BASE,
      toMs: BASE + seconds * 1_000,
      maxPoints: 600,
    });
    const historyQueryMs = performance.now() - historyStart;
    const historyStatus = store.historyStatus();
    const rawRows = mode === "memory" ? null : count(database, "points_raw");
    const summaryRows = mode === "memory" ? null : count(database, "points_10s");
    const databaseBytes = database.sizeBytes();
    const steadyWriteEnd = processWriteBytes();
    await store.close();
    const writeEnd = processWriteBytes();
    const rssEnd = process.memoryUsage().rss;
    rssPeak = Math.max(rssPeak, rssEnd);
    const records = machines * seconds;
    const steadyDiskWriteBytes =
      writeStart === null || steadyWriteEnd === null ? null : Math.max(0, steadyWriteEnd - writeStart);
    const diskWriteBytes = writeStart === null || writeEnd === null ? null : Math.max(0, writeEnd - writeStart);
    const closeDiskWriteBytes =
      steadyDiskWriteBytes === null || diskWriteBytes === null
        ? null
        : Math.max(0, diskWriteBytes - steadyDiskWriteBytes);
    return {
      mode,
      machines,
      records,
      seriesPerMachine: seriesCount(),
      simulatedSeconds: seconds,
      steadyDiskWriteBytes,
      closeDiskWriteBytes,
      diskWriteBytes,
      diskWriteBytesPerRecord: steadyDiskWriteBytes === null ? null : steadyDiskWriteBytes / records,
      diskWriteBytesPerSimulatedHour:
        steadyDiskWriteBytes === null ? null : (steadyDiskWriteBytes / seconds) * 3_600,
      ingestWallMs: wallMs,
      enqueueLatencyMs: {
        avg: sortedEnqueue.reduce((sum, value) => sum + value, 0) / Math.max(1, sortedEnqueue.length),
        p95: percentile(sortedEnqueue, 0.95),
        max: sortedEnqueue.at(-1) ?? 0,
      },
      commitLatencyMs: {
        avg: sortedCommit.reduce((sum, value) => sum + value, 0) / Math.max(1, sortedCommit.length),
        p95: percentile(sortedCommit, 0.95),
        max: sortedCommit.at(-1) ?? 0,
      },
      historyQueryMs,
      rssBytes: { start: rssStart, end: rssEnd, peak: rssPeak },
      databaseBytes,
      memoryBudgetBytes: historyStatus.memory?.budgetBytes ?? null,
      memoryUsedBytes: historyStatus.memory?.usedBytes ?? null,
      memoryPressure: historyStatus.memory?.pressure ?? null,
      retainedSeries: historyStatus.memory?.seriesCount ?? null,
      rawRows,
      summaryRows,
    };
  } finally {
    clearInterval(keepAlive);
    try {
      await store.close();
    } catch {
      // Already closed by the measured path.
    }
    rmSync(dir, { recursive: true, force: true });
  }
}

function numberArg(values: Map<string, string>, name: string, fallback: number): number {
  const raw = values.get(name);
  if (raw === undefined) return fallback;
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`--${name} must be a positive integer`);
  return value;
}

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  const values = new Map<string, string>();
  let json = false;
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index]!;
    if (arg === "--json") {
      json = true;
      continue;
    }
    if (arg.startsWith("--")) {
      const value = args[index + 1];
      if (value === undefined || value.startsWith("--")) throw new Error(`${arg} requires a value`);
      values.set(arg.slice(2), value);
      index += 1;
      continue;
    }
    throw new Error(`unknown argument ${arg}`);
  }
  const machines = numberArg(values, "machines", 2);
  const seconds = numberArg(values, "seconds", 120);
  if (machines > 100) throw new Error("--machines is bounded to 100");
  if (seconds > 3_600) throw new Error("--seconds is bounded to 3600");
  // /proc/self/io write_bytes counts storage writes, not page-cache or tmpfs
  // writes. Keep the default under the checkout (usually a real filesystem).
  const baseDir = resolve(values.get("dir") ?? join(process.cwd(), ".telemetry-benchmark"));
  const modes: Mode[] = ["memory", "persistent", "legacy"];
  const reports: ModeReport[] = [];
  for (const mode of modes) reports.push(await runMode(mode, machines, seconds, baseDir));
  rmSync(baseDir, { recursive: true, force: true });
  if (json) {
    console.log(JSON.stringify({ tool: "rigscope-telemetry-benchmark", generatedAt: new Date().toISOString(), reports }, null, 2));
    return;
  }
  console.log(`Rigscope telemetry benchmark · ${machines} machines × ${seconds}s × ${seriesCount()} series/machine`);
  for (const report of reports) {
    console.log(
      `${report.mode.padEnd(10)} steady writes ${report.steadyDiskWriteBytes === null ? "n/a" : `${(report.steadyDiskWriteBytes / 1024 / 1024).toFixed(2)} MiB`}` +
        ` (${report.diskWriteBytesPerSimulatedHour === null ? "n/a" : `${(report.diskWriteBytesPerSimulatedHour / 1024 / 1024 / 3600).toFixed(3)} MiB/s simulated`})` +
        ` · close ${report.closeDiskWriteBytes === null ? "n/a" : `${(report.closeDiskWriteBytes / 1024).toFixed(0)} KiB`}` +
        ` · enqueue p95 ${report.enqueueLatencyMs.p95.toFixed(2)}ms` +
        ` · commit p95 ${report.commitLatencyMs.p95.toFixed(0)}ms` +
        ` · history ${report.historyQueryMs.toFixed(1)}ms` +
        ` · RSS peak ${(report.rssBytes.peak / 1024 / 1024).toFixed(0)}MiB` +
        (report.memoryUsedBytes === null
          ? ""
          : ` · RAM ${(report.memoryUsedBytes / 1024 / 1024).toFixed(0)}/${(report.memoryBudgetBytes! / 1024 / 1024).toFixed(0)}MiB` +
            `${report.memoryPressure ? " pressure" : ""} · ${report.retainedSeries} series`) +
        ` · DB ${(report.databaseBytes / 1024 / 1024).toFixed(1)}MiB` +
        (report.mode === "legacy" ? ` · raw rows ${report.rawRows}` : "") +
        (report.mode === "persistent" ? ` · 10s rows ${report.summaryRows}` : ""),
    );
  }
  const memory = reports[0]!;
  const persistent = reports[1]!;
  const legacy = reports[2]!;
  if (
    memory.steadyDiskWriteBytes !== null &&
    persistent.steadyDiskWriteBytes !== null &&
    legacy.steadyDiskWriteBytes !== null &&
    legacy.steadyDiskWriteBytes > 0
  ) {
    console.log(
      `measured ingestion-phase persistent/legacy writes: ${(persistent.steadyDiskWriteBytes / legacy.steadyDiskWriteBytes).toFixed(2)}×` +
        ` (memory/legacy: ${(memory.steadyDiskWriteBytes / legacy.steadyDiskWriteBytes).toFixed(3)}×)`,
    );
  }
  console.log("Process-attributed /proc/self/io write_bytes; results vary by host, filesystem and payload.");
}

main().catch((error: unknown) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
});
