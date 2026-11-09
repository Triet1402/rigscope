/**
 * Shared fixtures for the durable storage tests. Not a test file itself.
 */

import type {
  BytesUsageMetric,
  CpuMetric,
  DisksMetric,
  FilesystemReading,
  DiskIoReading,
  GapRecord,
  GpusMetric,
  Inventory,
  MemoryMetric,
  MetricsPayload,
  MetricsRecord,
  NetworkMetric,
} from "../src/contracts";

export type PayloadOptions = {
  cpuUsage?: number;
  cores?: number;
  coreValue?: number;
  coreNullIndex?: number | null;
  cpuTemperature?: number | null;
  cpuPower?: number | null;
  cpuIowait?: number | null;
  cpuPressure?: number | null;
  memoryUsed?: number;
  memoryTotal?: number;
  memoryPressureSome?: number | null;
  memoryPressureFull?: number | null;
  diskPressureSome?: number | null;
  diskPressureFull?: number | null;
  swapUsed?: number;
  swapTotal?: number;
  gpus?: Array<{
    id: string;
    utilization?: number | null;
    memoryUsed?: number | null;
    memoryTotal?: number | null;
    temperature?: number | null;
    power?: number | null;
    junction?: number | null;
    vram?: number | null;
    unavailable?: boolean;
  }>;
  filesystems?: Array<{
    id: string;
    used?: number;
    total?: number;
    inodesUsed?: number | null;
    inodesTotal?: number | null;
    unavailable?: boolean;
  }>;
  disks?: Array<{
    id: string;
    read?: number;
    write?: number;
    readLatency?: number | null;
    writeLatency?: number | null;
    utilization?: number | null;
    unavailable?: boolean;
  }>;
  interfaces?: Array<{ id: string; rx?: number; tx?: number; unavailable?: boolean }>;
  unavailable?: {
    cpu?: boolean;
    memory?: boolean;
    swap?: boolean;
    gpus?: boolean;
    disks?: boolean;
    network?: boolean;
  };
};

export function makePayload(options: PayloadOptions = {}): MetricsPayload {
  const unavailable = options.unavailable ?? {};

  const cpu: CpuMetric = unavailable.cpu
    ? { status: "unavailable", reason: "test unavailable" }
    : (() => {
        const reading: Extract<CpuMetric, { status: "ok" }> = {
          status: "ok",
          usagePercent: options.cpuUsage ?? 10,
          perCore: Array.from({ length: options.cores ?? 2 }, (_, index) =>
            options.coreNullIndex === index ? null : (options.coreValue ?? 5),
          ),
        };
        if (options.cpuTemperature !== undefined) reading.temperatureC = options.cpuTemperature;
        if (options.cpuPower !== undefined) reading.powerWatts = options.cpuPower;
        if (options.cpuIowait !== undefined) reading.iowaitPercent = options.cpuIowait;
        if (options.cpuPressure !== undefined) reading.pressureSomePercent = options.cpuPressure;
        return reading;
      })();

  const memory: MemoryMetric = unavailable.memory
    ? { status: "unavailable", reason: "test unavailable" }
    : (() => {
        const reading: Extract<MemoryMetric, { status: "ok" }> = {
          status: "ok",
          usedBytes: options.memoryUsed ?? 400,
          totalBytes: options.memoryTotal ?? 1000,
        };
        if (options.memoryPressureSome !== undefined) reading.pressureSomePercent = options.memoryPressureSome;
        if (options.memoryPressureFull !== undefined) reading.pressureFullPercent = options.memoryPressureFull;
        return reading;
      })();

  const swap: BytesUsageMetric = unavailable.swap
    ? { status: "unavailable", reason: "test unavailable" }
    : {
        status: "ok",
        usedBytes: options.swapUsed ?? 100,
        totalBytes: options.swapTotal ?? 500,
      };

  const gpus: GpusMetric = unavailable.gpus
    ? { status: "unavailable", reason: "test unavailable" }
    : {
        status: "ok",
        devices: (options.gpus ?? []).map((gpu) =>
          gpu.unavailable
            ? { id: gpu.id, status: "unavailable" as const, reason: "test unavailable" }
            : {
                id: gpu.id,
                status: "ok" as const,
                utilizationPercent: gpu.utilization ?? null,
                memoryUsedBytes: gpu.memoryUsed ?? null,
                memoryTotalBytes: gpu.memoryTotal ?? null,
                temperatureC: gpu.temperature ?? null,
                powerWatts: gpu.power ?? null,
                ...(gpu.junction === undefined ? {} : {junctionTemperatureC: gpu.junction}),
                ...(gpu.vram === undefined ? {} : {vramTemperatureC: gpu.vram}),
              },
        ),
      };

  const disks: DisksMetric = unavailable.disks
    ? { status: "unavailable", reason: "test unavailable" }
    : (() => {
        const reading: Extract<DisksMetric, { status: "ok" }> = {
          status: "ok",
          filesystems: (options.filesystems ?? []).map((fs): FilesystemReading => {
            if (fs.unavailable) {
              return { id: fs.id, status: "unavailable", reason: "test unavailable" };
            }
            const item: Extract<FilesystemReading, { status: "ok" }> = {
              id: fs.id,
              status: "ok",
              usedBytes: fs.used ?? 500,
              totalBytes: fs.total ?? 1000,
            };
            if (fs.inodesUsed !== undefined) item.inodesUsed = fs.inodesUsed;
            if (fs.inodesTotal !== undefined) item.inodesTotal = fs.inodesTotal;
            return item;
          }),
          devices: (options.disks ?? []).map((disk): DiskIoReading => {
            if (disk.unavailable) {
              return { id: disk.id, status: "unavailable", reason: "test unavailable" };
            }
            const item: Extract<DiskIoReading, { status: "ok" }> = {
              id: disk.id,
              status: "ok",
              readBytesPerSec: disk.read ?? 1024,
              writeBytesPerSec: disk.write ?? 512,
            };
            if (disk.readLatency !== undefined) item.readLatencyMs = disk.readLatency;
            if (disk.writeLatency !== undefined) item.writeLatencyMs = disk.writeLatency;
            if (disk.utilization !== undefined) item.ioUtilizationPercent = disk.utilization;
            return item;
          }),
        };
        if (options.diskPressureSome !== undefined) reading.pressureSomePercent = options.diskPressureSome;
        if (options.diskPressureFull !== undefined) reading.pressureFullPercent = options.diskPressureFull;
        return reading;
      })();

  const network: NetworkMetric = unavailable.network
    ? { status: "unavailable", reason: "test unavailable" }
    : {
        status: "ok",
        interfaces: (options.interfaces ?? []).map((iface) =>
          iface.unavailable
            ? { id: iface.id, status: "unavailable" as const, reason: "test unavailable" }
            : {
                id: iface.id,
                status: "ok" as const,
                rxBytesPerSec: iface.rx ?? 2048,
                txBytesPerSec: iface.tx ?? 1024,
              },
        ),
      };

  return { cpu, memory, swap, gpus, disks, network };
}

export function makeMetricsRecord(options: {
  collectorId?: string;
  runId?: string;
  bootId?: string;
  sequence: number;
  capturedAt: string | number;
  payload?: MetricsPayload;
}): MetricsRecord {
  return {
    collectorId: options.collectorId ?? "collector-1",
    runId: options.runId ?? "run-1",
    bootId: options.bootId ?? "boot-1",
    sequence: options.sequence,
    capturedAt: toIso(options.capturedAt),
    kind: "metrics",
    payload: options.payload ?? makePayload(),
  };
}

export function makeGapRecord(options: {
  collectorId?: string;
  runId?: string;
  sequence: number;
  capturedAt: string | number;
  fromMs: number;
  toMs: number;
  dropped?: number;
  reason?: GapRecord["payload"]["reason"];
}): GapRecord {
  return {
    collectorId: options.collectorId ?? "collector-1",
    runId: options.runId ?? "run-1",
    bootId: "boot-1",
    sequence: options.sequence,
    capturedAt: toIso(options.capturedAt),
    kind: "gap",
    payload: {
      fromCapturedAt: toIso(options.fromMs),
      toCapturedAt: toIso(options.toMs),
      droppedRecords: options.dropped ?? 1,
      reason: options.reason ?? "outbox-bytes",
    },
  };
}

export function makeInventory(): Inventory {
  return {
    os: { platform: "linux", release: "test", distro: null, arch: "x64" },
    cpu: { model: "test cpu", logicalCores: 2 },
    memory: { totalBytes: 1000, swapTotalBytes: 500 },
    gpus: [],
    filesystems: [],
    disks: [],
    network: [],
    capabilities: {
      cpu: { status: "supported" },
      memory: { status: "supported" },
      swap: { status: "supported" },
      gpus: { status: "supported" },
      disks: { status: "supported" },
      network: { status: "supported" },
      processes: { status: "supported" },
    },
  };
}

export function toIso(value: string | number): string {
  return typeof value === "number" ? new Date(value).toISOString() : value;
}
