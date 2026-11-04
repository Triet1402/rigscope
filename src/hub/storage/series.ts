/**
 * Metric catalog and payload flattening.
 *
 * Wire payloads are nested by family and may contain per-resource readings or
 * explicit "unavailable" states. Storage is a flat (metric, resource) series
 * space. This module is the single mapping between the two, so ingestion,
 * history and the UI agree on resource ids and units.
 *
 * Unavailable semantics (never a successful zero):
 * - an unavailable single-resource family means NULL readings of its primary
 *   metrics (cpu.usage / memory.used + memory.usage / swap.used);
 * - an unavailable per-resource reading means a NULL primary metric for that
 *   resource (gpu.utilization + gpu.memory.used, fs.used, disk.read +
 *   disk.write, net.rx + net.tx);
 * - an unavailable multi-resource family produces no points at all; the
 *   missing interval is represented by receipts/gap records instead.
 * - a per-field null (e.g. GPU temperature) is a NULL point for that metric.
 */

import type { DatabaseSync } from "node:sqlite";
import type { MetricsPayload } from "../../contracts";

export type MetricUnit = "percent" | "bytes" | "bytes_per_second" | "celsius" | "watts" | "milliseconds";

export const METRICS = {
  "cpu.usage": { unit: "percent" },
  "cpu.core": { unit: "percent" },
  "cpu.temperature": { unit: "celsius" },
  "cpu.power": { unit: "watts" },
  "cpu.iowait": { unit: "percent" },
  "cpu.pressure": { unit: "percent" },
  "memory.used": { unit: "bytes" },
  "memory.total": { unit: "bytes" },
  "memory.usage": { unit: "percent" },
  "memory.pressure.some": { unit: "percent" },
  "memory.pressure.full": { unit: "percent" },
  "swap.used": { unit: "bytes" },
  "swap.total": { unit: "bytes" },
  "gpu.utilization": { unit: "percent" },
  "gpu.memory.used": { unit: "bytes" },
  "gpu.memory.total": { unit: "bytes" },
  "gpu.temperature": { unit: "celsius" },
  "gpu.power": { unit: "watts" },
  "fs.used": { unit: "bytes" },
  "fs.total": { unit: "bytes" },
  "fs.usage": { unit: "percent" },
  "fs.inodes.usage": { unit: "percent" },
  "io.pressure.some": { unit: "percent" },
  "io.pressure.full": { unit: "percent" },
  "disk.read": { unit: "bytes_per_second" },
  "disk.write": { unit: "bytes_per_second" },
  "disk.read.latency": { unit: "milliseconds" },
  "disk.write.latency": { unit: "milliseconds" },
  "disk.utilization": { unit: "percent" },
  "net.rx": { unit: "bytes_per_second" },
  "net.tx": { unit: "bytes_per_second" },
} as const satisfies Record<string, { unit: MetricUnit }>;

export type MetricName = keyof typeof METRICS;

export type FlatPoint = {
  metric: MetricName;
  resourceId: string;
  value: number | null;
};

export type KnownSeries = { metric: string; resourceId: string };

const CPU_CORE_METRICS = new Set<MetricName>(["cpu.core"]);
const CPU_OPTIONAL_METRICS = new Set<MetricName>([
  "cpu.temperature",
  "cpu.power",
  "cpu.iowait",
  "cpu.pressure",
]);
const MEMORY_PRESSURE_METRICS = new Set<MetricName>(["memory.pressure.some", "memory.pressure.full"]);
const GPU_METRICS = new Set<MetricName>([
  "gpu.utilization",
  "gpu.memory.used",
  "gpu.memory.total",
  "gpu.temperature",
  "gpu.power",
]);
const DISK_FAMILY_METRICS = new Set<MetricName>([
  "fs.used",
  "fs.total",
  "fs.usage",
  "fs.inodes.usage",
  "io.pressure.some",
  "io.pressure.full",
  "disk.read",
  "disk.write",
  "disk.read.latency",
  "disk.write.latency",
  "disk.utilization",
]);
const NETWORK_METRICS = new Set<MetricName>(["net.rx", "net.tx"]);

/** Derived percent from byte or inode counts; a non-positive total is unknown. */
function percentOf(used: number | null | undefined, total: number): number | null {
  if (used === null || used === undefined || !Number.isFinite(used) || !Number.isFinite(total) || total <= 0) {
    return null;
  }
  const percent = Math.min(100, Math.max(0, (used / total) * 100));
  return Math.round(percent * 100) / 100;
}

/**
 * Flattens a validated metrics payload into its stored series points.
 *
 * `known` is the machine's existing series catalog. It is only needed when a
 * multi-resource family is unavailable: pushing a NULL for each already-known
 * series makes the failed family an explicit break in the chart instead of an
 * uninterrupted line that silently reappears. On a fresh machine there is
 * nothing known, so an unavailable family still produces no points (the
 * receipts/gap data represents the missing interval).
 */
export function flattenMetrics(payload: MetricsPayload, known?: readonly KnownSeries[]): FlatPoint[] {
  const points: FlatPoint[] = [];
  const push = (metric: MetricName, resourceId: string, value: number | null): void => {
    points.push({ metric, resourceId, value });
  };
  const pushKnownNulls = (metrics: ReadonlySet<MetricName>): void => {
    if (!known) return;
    for (const item of known) {
      if (!metrics.has(item.metric as MetricName)) continue;
      if (!Object.prototype.hasOwnProperty.call(METRICS, item.metric)) continue;
      push(item.metric as MetricName, item.resourceId, null);
    }
  };

  const cpu = payload.cpu;
  if (cpu.status === "ok") {
    push("cpu.usage", "", cpu.usagePercent);
    for (const [index, value] of cpu.perCore.entries()) {
      push("cpu.core", `core:${index}`, value);
    }
    push("cpu.temperature", "", cpu.temperatureC ?? null);
    push("cpu.power", "", cpu.powerWatts ?? null);
    push("cpu.iowait", "", cpu.iowaitPercent ?? null);
    push("cpu.pressure", "", cpu.pressureSomePercent ?? null);
  } else {
    push("cpu.usage", "", null);
    // Per-core and optional scalar lines that existed before the failure break.
    pushKnownNulls(CPU_CORE_METRICS);
    pushKnownNulls(CPU_OPTIONAL_METRICS);
  }

  const memory = payload.memory;
  if (memory.status === "ok") {
    push("memory.used", "", memory.usedBytes);
    push("memory.total", "", memory.totalBytes);
    push("memory.usage", "", percentOf(memory.usedBytes, memory.totalBytes));
    push("memory.pressure.some", "", memory.pressureSomePercent ?? null);
    push("memory.pressure.full", "", memory.pressureFullPercent ?? null);
  } else {
    push("memory.used", "", null);
    push("memory.usage", "", null);
    pushKnownNulls(MEMORY_PRESSURE_METRICS);
  }

  const swap = payload.swap;
  if (swap.status === "ok") {
    push("swap.used", "", swap.usedBytes);
    push("swap.total", "", swap.totalBytes);
  } else {
    push("swap.used", "", null);
  }

  if (payload.gpus.status === "ok") {
    for (const gpu of payload.gpus.devices) {
      if (gpu.status === "unavailable") {
        push("gpu.utilization", gpu.id, null);
        push("gpu.memory.used", gpu.id, null);
        continue;
      }
      push("gpu.utilization", gpu.id, gpu.utilizationPercent);
      push("gpu.memory.used", gpu.id, gpu.memoryUsedBytes);
      push("gpu.memory.total", gpu.id, gpu.memoryTotalBytes);
      push("gpu.temperature", gpu.id, gpu.temperatureC);
      push("gpu.power", gpu.id, gpu.powerWatts);
    }
  } else {
    // A collection-wide GPU failure: null every previously known GPU series.
    pushKnownNulls(GPU_METRICS);
  }

  if (payload.disks.status === "ok") {
    push("io.pressure.some", "", payload.disks.pressureSomePercent ?? null);
    push("io.pressure.full", "", payload.disks.pressureFullPercent ?? null);
    for (const fs of payload.disks.filesystems) {
      if (fs.status === "unavailable") {
        push("fs.used", fs.id, null);
        push("fs.usage", fs.id, null);
        push("fs.inodes.usage", fs.id, null);
        continue;
      }
      push("fs.used", fs.id, fs.usedBytes);
      push("fs.total", fs.id, fs.totalBytes);
      push("fs.usage", fs.id, percentOf(fs.usedBytes, fs.totalBytes));
      if (fs.inodesTotal !== undefined) {
        push("fs.inodes.usage", fs.id, percentOf(fs.inodesUsed, fs.inodesTotal ?? 0));
      } else if (known?.some((item) => item.metric === "fs.inodes.usage" && item.resourceId === fs.id)) {
        push("fs.inodes.usage", fs.id, null);
      }
    }
    for (const disk of payload.disks.devices) {
      if (disk.status === "unavailable") {
        push("disk.read", disk.id, null);
        push("disk.write", disk.id, null);
        push("disk.read.latency", disk.id, null);
        push("disk.write.latency", disk.id, null);
        push("disk.utilization", disk.id, null);
        continue;
      }
      push("disk.read", disk.id, disk.readBytesPerSec);
      push("disk.write", disk.id, disk.writeBytesPerSec);
      push("disk.read.latency", disk.id, disk.readLatencyMs ?? null);
      push("disk.write.latency", disk.id, disk.writeLatencyMs ?? null);
      push("disk.utilization", disk.id, disk.ioUtilizationPercent ?? null);
    }
  } else {
    pushKnownNulls(DISK_FAMILY_METRICS);
  }

  if (payload.network.status === "ok") {
    for (const iface of payload.network.interfaces) {
      if (iface.status === "unavailable") {
        push("net.rx", iface.id, null);
        push("net.tx", iface.id, null);
        continue;
      }
      push("net.rx", iface.id, iface.rxBytesPerSec);
      push("net.tx", iface.id, iface.txBytesPerSec);
    }
  } else {
    pushKnownNulls(NETWORK_METRICS);
  }

  return points;
}

/**
 * Per-hub series id cache.
 *
 * Series ids are stable rows in `series`. Telemetry ingestion is the only
 * writer, so an in-memory map is safe; it is preloaded once at startup so the
 * hot path is a Map lookup rather than an upsert for existing series.
 */
export class SeriesCatalog {
  private readonly cache = new Map<string, number>();
  /** Existing series per machine, used to break unavailable family charts. */
  private readonly byMachine = new Map<string, Map<string, Set<string>>>();
  private readonly insert: ReturnType<DatabaseSync["prepare"]>;
  private readonly select: ReturnType<DatabaseSync["prepare"]>;

  constructor(private readonly db: DatabaseSync) {
    this.insert = db.prepare(
      "INSERT INTO series (machine_id, metric, resource_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING",
    );
    this.select = db.prepare(
      "SELECT series_id FROM series WHERE machine_id = ? AND metric = ? AND resource_id = ?",
    );
    const rows = db
      .prepare("SELECT series_id, machine_id, metric, resource_id FROM series")
      .all() as Array<{ series_id: number | bigint; machine_id: string; metric: string; resource_id: string }>;
    for (const row of rows) {
      this.cache.set(seriesKey(row.machine_id, row.metric, row.resource_id), Number(row.series_id));
      this.remember(row.machine_id, row.metric, row.resource_id);
    }
  }

  idFor(machineId: string, metric: string, resourceId: string): number {
    const key = seriesKey(machineId, metric, resourceId);
    const cached = this.cache.get(key);
    if (cached !== undefined) return cached;
    this.insert.run(machineId, metric, resourceId);
    const row = this.select.get(machineId, metric, resourceId) as { series_id?: number | bigint } | undefined;
    if (row?.series_id === undefined) throw new Error("series catalog insert did not produce a row");
    const id = Number(row.series_id);
    this.cache.set(key, id);
    this.remember(machineId, metric, resourceId);
    return id;
  }

  /** Every series already stored for one machine (one entry per metric/resource). */
  knownFor(machineId: string): KnownSeries[] {
    const metrics = this.byMachine.get(machineId);
    if (!metrics) return [];
    const result: KnownSeries[] = [];
    for (const [metric, resources] of metrics) {
      for (const resourceId of resources) result.push({ metric, resourceId });
    }
    return result;
  }

  private remember(machineId: string, metric: string, resourceId: string): void {
    let metrics = this.byMachine.get(machineId);
    if (!metrics) {
      metrics = new Map();
      this.byMachine.set(machineId, metrics);
    }
    let resources = metrics.get(metric);
    if (!resources) {
      resources = new Set();
      metrics.set(metric, resources);
    }
    resources.add(resourceId);
  }
}

function seriesKey(machineId: string, metric: string, resourceId: string): string {
  // NUL is not valid in any contract identifier, so this is unambiguous.
  return `${machineId}\u0000${metric}\u0000${resourceId}`;
}
