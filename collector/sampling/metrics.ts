/**
 * Synchronous metric sampler.
 *
 * One `next()` call reads only cheap procfs/sysfs counters and the GPU
 * poller's cache; it never spawns a subprocess. Every family is isolated:
 * a throwing or malformed reader turns that family (or resource) into an
 * honest `unavailable`, never a fabricated zero and never the loss of another
 * family. CPU rates use busy-counter deltas over monotonic elapsed time,
 * normalized by logical cores. Rates start at the second reading; regressions
 * and hotplug reset the affected baseline. Optional bottleneck sensors (PSI,
 * CPU hwmon temperature, RAPL package power, disk operation times) are read
 * from small sysfs/procfs files and degrade to null per metric when absent.
 *
 * Inventory is refreshed on each tick so hotplug, interface address changes
 * and a newly joined NVIDIA device bump `inventoryVersion()`.
 */
import * as nodeFs from 'node:fs';
import * as nodeOs from 'node:os';

import type {
  BytesUsageMetric,
  CollectionFailure,
  CpuMetric,
  DisksMetric,
  DiskIoReading,
  FilesystemReading,
  GpusMetric,
  InterfaceReading,
  Inventory,
  MemoryMetric,
  MetricsPayload,
  MetricsRecord,
  NetworkMetric,
} from '../../src/contracts';
import { MAX_COLLECTION_FAILURES } from '../../src/contracts';
import type { GpuPoller } from './gpu';
import { createRaplPowerMeter, readCpuTemperature } from './hwmon';
import { readPressure } from './pressure';
import { readSysfsGpu } from './sysfs-gpu';
import {
  buildInventory,
  capReason,
  type GpuInventorySource,
  type InventoryReaders,
} from './inventory';
import {
  readDiskstats,
  readMeminfo,
  readMountinfo,
  readNetDev,
  readProcStat,
  type CpuCounters,
  type NetCounters,
} from './procfs';
import type { SequenceAllocator } from './sequence';
import {
  discoverDisks,
  diskCountersFor,
  readFilesystemUsage,
  selectFilesystems,
} from './storage';
import type { StatFsReader } from './storage';

export type MetricsIdentity = {
  collectorId: string;
  runId: string;
  bootId: string;
};

export type MetricsSamplerOptions = {
  sequence: SequenceAllocator;
  /** Asynchronous GPU poller; null/omitted means no NVIDIA tooling. */
  gpu?: GpuPoller | null;
  /** Partial reader overrides for deterministic tests. */
  readers?: Partial<InventoryReaders>;
  /** Wall clock for `capturedAt`; default `Date.now`. */
  now?: () => number;
  /** Monotonic clock for intervals; default `performance.now`. */
  monotonicMs?: () => number;
  /** Cache age after which GPU readings are stale; default max(2×poll, 10s). */
  gpuStaleMs?: number;
};

export type MetricsSampler = {
  next(): MetricsRecord;
  inventory(): Inventory;
  inventoryVersion(): number;
  failures(): CollectionFailure[];
};

type FailureSink = {
  add(metric: string, resourceId: string | null, reason: string): void;
  list(): CollectionFailure[];
};

function createFailureSink(): FailureSink {
  const items: CollectionFailure[] = [];
  const seen = new Set<string>();
  return {
    add(metric, resourceId, reason): void {
      if (items.length >= MAX_COLLECTION_FAILURES) return;
      const capped = capReason(reason);
      const key = `${metric}\u0000${resourceId ?? ''}\u0000${capped}`;
      if (seen.has(key)) return;
      seen.add(key);
      items.push({metric, resourceId, reason: capped});
    },
    list(): CollectionFailure[] {
      return [...items];
    },
  };
}

/** Linux USER_HZ: /proc/stat counters are in centiseconds on the target host. */
const USER_HZ = 100;
/** Final clamp tolerance for quantized counters (5% of capacity, at least 20 ms). */
const CPU_TOLERANCE_FRACTION = 0.05;

const MAX_RATE_BYTES_PER_SEC = 1e13;

const defaultReaders: InventoryReaders = {
  readFile: (path) => String(nodeFs.readFileSync(path, 'utf8')),
  readdir: (path) => nodeFs.readdirSync(path),
  statfs: (path) => {
    const stats = nodeFs.statfsSync(path);
    return {
      bsize: Number(stats.bsize),
      blocks: Number(stats.blocks),
      bfree: Number(stats.bfree),
      bavail: Number(stats.bavail),
      files: Number(stats.files),
      ffree: Number(stats.ffree),
    };
  },
  networkInterfaces: () => nodeOs.networkInterfaces(),
  platform: () => nodeOs.platform(),
  release: () => nodeOs.release(),
  arch: () => nodeOs.arch(),
  cpuCount: () => nodeOs.cpus().length,
};

type CpuBaseline = {
  atMs: number;
  coreCount: number;
  total: CpuCounters;
  perCore: Map<number, CpuCounters>;
};

type DiskBaseline = {
  atMs: number;
  readSectors: number;
  writeSectors: number;
  readOps: number | null;
  writeOps: number | null;
  readTimeMs: number | null;
  writeTimeMs: number | null;
  ioTimeMs: number | null;
  signature: string;
};

type NetBaseline = {atMs: number; rxBytes: number; txBytes: number};

function describeError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function clampPercent(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.min(100, Math.max(0, value));
}

function roundRate(bytes: number): number {
  if (!Number.isFinite(bytes) || bytes < 0) return 0;
  return Math.min(MAX_RATE_BYTES_PER_SEC, Math.round(bytes));
}

function counterMap(perCore: readonly ({index: number} & CpuCounters)[]): Map<number, CpuCounters> {
  const map = new Map<number, CpuCounters>();
  for (const core of perCore) {
    map.set(core.index, {busy: core.busy, idle: core.idle, iowait: core.iowait});
  }
  return map;
}

function isFiniteNonNegative(value: number): boolean {
  return Number.isFinite(value) && value >= 0;
}

function counterDelta(current: number | null, previous: number | null): number | null {
  if (current === null || previous === null) return null;
  // A decrease is a reset of that optional counter. Keep the other readings;
  // only the affected derived latency/utilization becomes unknown.
  return current < previous ? null : current - previous;
}

/** Mean latency for an interval; no completed operations means unknown, not 0 ms. */
function meanLatencyMs(operations: number | null, timeMs: number | null): number | null {
  if (operations === null || timeMs === null || operations <= 0 || timeMs < 0) return null;
  return Math.round((timeMs / operations) * 1000) / 1000;
}

function inventorySignature(inventory: Inventory): string {
  return JSON.stringify({
    gpus: inventory.gpus.map((gpu) => [gpu.id, gpu.vendor, gpu.telemetry, gpu.memoryTotalBytes, gpu.pciBusId]),
    filesystems: inventory.filesystems.map((fs) => [fs.id, fs.totalBytes]),
    disks: inventory.disks.map((disk) => [disk.id, disk.sizeBytes]),
    network: inventory.network.map((iface) => [
      iface.id,
      iface.state,
      iface.speedMbps,
      iface.addresses.map((address) => `${address.family}:${address.address}/${address.prefix}`),
    ]),
    capabilities: inventory.capabilities,
  });
}

function fallbackInventory(readers: InventoryReaders): Inventory {
  const capability = {status: 'unsupported' as const, reason: 'inventory unavailable'};
  return {
    os: {
      platform: (() => {
        try {
          return capReason(readers.platform());
        } catch {
          return 'unknown';
        }
      })(),
      release: '',
      distro: null,
      arch: '',
    },
    cpu: {model: null, logicalCores: 0},
    memory: {totalBytes: null, swapTotalBytes: null},
    gpus: [],
    filesystems: [],
    disks: [],
    network: [],
    capabilities: {
      cpu: capability,
      memory: capability,
      swap: capability,
      gpus: capability,
      disks: capability,
      network: capability,
      processes: capability,
    },
  };
}

/**
 * Creates the sampler shared by all destinations. The caller owns the sequence
 * allocator so gap records can share the same monotonically increasing space.
 */
export function createMetricsSampler(
  identity: MetricsIdentity,
  options: MetricsSamplerOptions,
): MetricsSampler {
  const sequence = options.sequence;
  const gpu = options.gpu ?? null;
  const readers: InventoryReaders = {...defaultReaders, ...(options.readers ?? {})};
  const now = options.now ?? Date.now;
  const monotonicMs = options.monotonicMs ?? (() => performance.now());
  const gpuStaleMs = options.gpuStaleMs ?? Math.max(2 * ((gpu as GpuPoller | null)?.pollIntervalMs ?? 2_000), 10_000);

  let cpuBaseline: CpuBaseline | null = null;
  const diskBaselines = new Map<string, DiskBaseline>();
  const netBaselines = new Map<string, NetBaseline>();
  const raplPower = createRaplPowerMeter(readers.readFile, readers.readdir);

  let lastInventory: Inventory | null = null;
  let lastInventorySignature: string | null = null;
  let version = 0;
  let currentFailures: CollectionFailure[] = [];

  function refreshInventory(): Inventory {
    let inventory: Inventory;
    try {
      inventory = buildInventory(readers, gpu as GpuInventorySource);
    } catch {
      inventory = lastInventory ?? fallbackInventory(readers);
    }
    const signature = inventorySignature(inventory);
    if (signature !== lastInventorySignature) {
      version += 1;
      lastInventorySignature = signature;
    }
    lastInventory = inventory;
    return inventory;
  }

  function readMonotonic(): number | null {
    try {
      const value = monotonicMs();
      return Number.isFinite(value) ? value : null;
    } catch {
      return null;
    }
  }

  function unavailable<T>(
    sink: FailureSink,
    metric: string,
    reason: string,
    resourceId: string | null = null,
  ): T {
    const capped = capReason(reason);
    sink.add(metric, resourceId, capped);
    return {status: 'unavailable', reason: capped} as T;
  }

  function sampleCpu(sink: FailureSink, atMs: number | null): CpuMetric {
    if (atMs === null) return unavailable<CpuMetric>(sink, 'cpu', 'cpu monotonic elapsed clock unavailable');
    const snapshot = readProcStat(readers.readFile);
    if (snapshot === null) {
      cpuBaseline = null;
      return unavailable<CpuMetric>(sink, 'cpu', 'cpu counters unavailable');
    }
    const current: CpuBaseline = {
      atMs,
      coreCount: snapshot.coreCount,
      total: snapshot.total,
      perCore: counterMap(snapshot.perCore),
    };
    const previous = cpuBaseline;
    cpuBaseline = current;
    if (previous === null) {
      return unavailable<CpuMetric>(sink, 'cpu', 'cpu usage needs two counter readings; first interval pending');
    }
    if (previous.coreCount !== current.coreCount) {
      return unavailable<CpuMetric>(sink, 'cpu', 'cpu core count changed; baseline reset');
    }
    if (current.coreCount < 1) {
      return unavailable<CpuMetric>(sink, 'cpu', 'cpu core counters unavailable');
    }
    const elapsedMs = current.atMs - previous.atMs;
    if (elapsedMs <= 0) {
      return unavailable<CpuMetric>(sink, 'cpu', 'cpu monotonic elapsed did not advance');
    }
    const totalBusyDelta = current.total.busy - previous.total.busy;
    const totalIdleDelta = current.total.idle - previous.total.idle;
    if (totalBusyDelta < 0 || totalIdleDelta < 0) {
      return unavailable<CpuMetric>(sink, 'cpu', 'cpu counters decreased; baseline reset');
    }
    const ticksPerCore = (elapsedMs * USER_HZ) / 1000;
    const capacity = ticksPerCore * current.coreCount;
    if (!(capacity > 0)) return unavailable<CpuMetric>(sink, 'cpu', 'cpu monotonic elapsed did not advance');
    const tolerance = Math.max(capacity * CPU_TOLERANCE_FRACTION, current.coreCount * 2);
    if (totalBusyDelta > capacity + tolerance) {
      return unavailable<CpuMetric>(sink, 'cpu', 'cpu busy counters exceed measured capacity; baseline reset');
    }
    if (totalIdleDelta > totalBusyDelta + capacity + tolerance) {
      return unavailable<CpuMetric>(sink, 'cpu', 'cpu idle counters exceed measured capacity');
    }
    const usagePercent = Math.round(clampPercent((totalBusyDelta / capacity) * 100) * 100) / 100;
    const iowaitDelta = current.total.iowait - previous.total.iowait;
    let iowaitPercent: number | null = null;
    if (iowaitDelta < 0 || iowaitDelta > capacity + tolerance) {
      sink.add('cpu', null, 'cpu iowait counters inconsistent');
    } else {
      iowaitPercent = Math.round(clampPercent((iowaitDelta / capacity) * 100) * 100) / 100;
    }

    const perCore: (number | null)[] = [];
    const indexes = new Set<number>([...previous.perCore.keys(), ...current.perCore.keys()]);
    for (const index of [...indexes].sort((a, b) => a - b)) {
      const prev = previous.perCore.get(index);
      const cur = current.perCore.get(index);
      if (prev === undefined || cur === undefined) {
        perCore.push(null);
        continue;
      }
      const busyDelta = cur.busy - prev.busy;
      if (busyDelta < 0) {
        perCore.push(null);
        continue;
      }
      if (!(ticksPerCore > 0) || busyDelta > ticksPerCore + Math.max(ticksPerCore * CPU_TOLERANCE_FRACTION, 2)) {
        perCore.push(null);
        continue;
      }
      perCore.push(Math.round(clampPercent((busyDelta / ticksPerCore) * 100) * 100) / 100);
    }

    // Optional sensors fail independently: a missing/unreadable PSI, hwmon or
    // RAPL source is null for that field while usage/cores stay useful.
    const pressure = readPressure(readers.readFile, '/proc/pressure/cpu');
    let temperatureC: number | null;
    try {
      temperatureC = readCpuTemperature(readers.readFile, readers.readdir);
    } catch {
      temperatureC = null;
    }
    let powerWatts: number | null;
    try {
      powerWatts = raplPower.sample(atMs);
    } catch {
      powerWatts = null;
    }
    return {
      status: 'ok',
      usagePercent,
      perCore,
      temperatureC,
      powerWatts,
      iowaitPercent,
      pressureSomePercent: pressure?.someAvg10 ?? null,
    };
  }

  function sampleMemory(sink: FailureSink): MemoryMetric {
    const meminfo = readMeminfo(readers.readFile);
    if (meminfo === null || meminfo.totalBytes === null || meminfo.availableBytes === null) {
      return unavailable<MemoryMetric>(sink, 'memory', 'memory counters unavailable');
    }
    const total = meminfo.totalBytes;
    const available = meminfo.availableBytes;
    if (total <= 0) return unavailable<MemoryMetric>(sink, 'memory', 'memory total counters unavailable');
    if (!isFiniteNonNegative(available) || available > total) {
      return unavailable<MemoryMetric>(sink, 'memory', 'memory available counters inconsistent');
    }
    const pressure = readPressure(readers.readFile, '/proc/pressure/memory');
    return {
      status: 'ok',
      usedBytes: total - available,
      totalBytes: total,
      pressureSomePercent: pressure?.someAvg10 ?? null,
      pressureFullPercent: pressure?.fullAvg10 ?? null,
    };
  }

  function sampleSwap(sink: FailureSink): BytesUsageMetric {
    const meminfo = readMeminfo(readers.readFile);
    if (meminfo === null) return unavailable<BytesUsageMetric>(sink, 'swap', 'swap counters unavailable');
    const total = meminfo.swapTotalBytes ?? 0;
    const free = meminfo.swapFreeBytes ?? 0;
    if (total === 0) return {status: 'ok', usedBytes: 0, totalBytes: 0};
    if (!isFiniteNonNegative(free) || free > total) {
      return unavailable<BytesUsageMetric>(sink, 'swap', 'swap counters inconsistent');
    }
    return {status: 'ok', usedBytes: total - free, totalBytes: total};
  }

  function sampleNvidia(sink: FailureSink): GpusMetric {
    const hasGpu = (lastInventory?.gpus.length ?? 0) > 0;
    if (gpu === null) {
      return unavailable<GpusMetric>(
        sink,
        'gpus',
        hasGpu ? 'no supported GPU telemetry (nvidia-smi not found)' : 'no GPUs detected',
      );
    }
    const availability = gpu.available();
    if (availability === 'no') {
      return unavailable<GpusMetric>(
        sink,
        'gpus',
        hasGpu ? 'no supported GPU telemetry (nvidia-smi not found)' : 'no GPUs detected',
      );
    }
    if (availability === 'unknown') {
      return unavailable<GpusMetric>(sink, 'gpus', hasGpu ? 'gpu telemetry not ready' : 'no GPUs detected');
    }
    const snapshot = gpu.latest();
    if (snapshot === null) {
      return unavailable<GpusMetric>(sink, 'gpus', hasGpu ? 'gpu telemetry not ready' : 'no GPUs detected');
    }
    if (snapshot.devices.length === 0) {
      return unavailable<GpusMetric>(sink, 'gpus', hasGpu ? 'no supported GPU telemetry' : 'no GPUs detected');
    }
    const atMs = readMonotonic();
    if (atMs === null) return unavailable<GpusMetric>(sink, 'gpus', 'gpu monotonic clock unavailable');
    if (atMs - snapshot.atMonotonicMs > gpuStaleMs) {
      const devices = snapshot.devices.map((device) => {
        sink.add('gpus', device.id, 'gpu reading stale');
        return {id: device.id, status: 'unavailable' as const, reason: 'gpu reading stale'};
      });
      return {status: 'ok', devices};
    }
    return {
      status: 'ok',
      devices: snapshot.devices.map((device) => ({...device})),
    };
  }

  function sampleGpus(sink: FailureSink): GpusMetric {
    const drm = (lastInventory?.gpus ?? []).filter(device => device.vendor === 'amd' || device.vendor === 'intel');
    if (drm.length === 0) return sampleNvidia(sink);
    const devices = drm.map(device => {
      const reading = readSysfsGpu(readers, device.pciBusId!);
      if (reading.status === 'unavailable') sink.add('gpus', reading.id, reading.reason);
      return reading;
    });
    if (lastInventory?.gpus.some(device => device.vendor === 'nvidia')) {
      const nvidia = sampleNvidia(sink);
      if (nvidia.status === 'ok') devices.push(...nvidia.devices);
      else for (const device of lastInventory.gpus.filter(device => device.vendor === 'nvidia')) {
        devices.push({id: device.id, status: 'unavailable', reason: nvidia.reason});
      }
    }
    return {status: 'ok', devices};
  }

  function sampleDisks(sink: FailureSink, atMs: number | null): DisksMetric {
    let mounts;
    try {
      mounts = readMountinfo(readers.readFile);
    } catch (error) {
      mounts = null;
      sink.add('disks', null, `mount reader failed: ${describeError(error)}`);
    }
    const filesystems: FilesystemReading[] = [];
    if (mounts === null) {
      sink.add('disks', null, 'mount information unavailable');
    } else {
      for (const candidate of selectFilesystems(mounts)) {
        const usage = readFilesystemUsage(candidate, readers.statfs);
        filesystems.push(usage.reading);
        if (usage.reading.status === 'unavailable') {
          sink.add('disks', usage.reading.id, usage.reading.reason);
        }
      }
    }

    let disks: Inventory['disks'];
    try {
      disks = discoverDisks(readers.readdir, readers.readFile);
    } catch (error) {
      disks = [];
      sink.add('disks', null, `disk discovery failed: ${describeError(error)}`);
    }
    let counters;
    try {
      counters = readDiskstats(readers.readFile);
    } catch (error) {
      counters = null;
      sink.add('disks', null, `disk statistics reader failed: ${describeError(error)}`);
    }
    const devices: DiskIoReading[] = [];
    if (counters === null) {
      for (const disk of disks) {
        devices.push({id: disk.id, status: 'unavailable', reason: 'disk statistics unavailable'});
        sink.add('disks', disk.id, 'disk statistics unavailable');
      }
    } else {
      const present = new Set(disks.map((disk) => disk.name));
      for (const name of [...diskBaselines.keys()]) {
        if (!present.has(name)) diskBaselines.delete(name);
      }
      const filtered = diskCountersFor(counters, disks);
      for (const disk of disks) {
        const current = filtered.get(disk.name);
        if (current === undefined) {
          devices.push({id: disk.id, status: 'unavailable', reason: 'disk counters unavailable'});
          sink.add('disks', disk.id, 'disk counters unavailable');
          continue;
        }
        const signature = `${disk.sizeBytes ?? 'unknown'}`;
        const previous = diskBaselines.get(disk.name);
        const at = atMs ?? readMonotonic();
        if (at === null) {
          devices.push({id: disk.id, status: 'unavailable', reason: 'disk monotonic clock unavailable'});
          sink.add('disks', disk.id, 'disk monotonic clock unavailable');
          continue;
        }
        diskBaselines.set(disk.name, {
          atMs: at,
          readSectors: current.readSectors,
          writeSectors: current.writeSectors,
          readOps: current.readOps,
          writeOps: current.writeOps,
          readTimeMs: current.readTimeMs,
          writeTimeMs: current.writeTimeMs,
          ioTimeMs: current.ioTimeMs,
          signature,
        });
        if (previous === undefined) {
          devices.push({id: disk.id, status: 'unavailable', reason: 'disk I/O needs two readings; first interval pending'});
          sink.add('disks', disk.id, 'disk I/O needs two readings; first interval pending');
          continue;
        }
        if (previous.signature !== signature) {
          devices.push({id: disk.id, status: 'unavailable', reason: 'disk device changed; baseline reset'});
          sink.add('disks', disk.id, 'disk device changed; baseline reset');
          continue;
        }
        const readDelta = current.readSectors - previous.readSectors;
        const writeDelta = current.writeSectors - previous.writeSectors;
        const elapsedMs = at - previous.atMs;
        if (readDelta < 0 || writeDelta < 0) {
          devices.push({id: disk.id, status: 'unavailable', reason: 'disk counters decreased; baseline reset'});
          sink.add('disks', disk.id, 'disk counters decreased; baseline reset');
          continue;
        }
        if (elapsedMs <= 0) {
          devices.push({id: disk.id, status: 'unavailable', reason: 'disk monotonic elapsed did not advance'});
          sink.add('disks', disk.id, 'disk monotonic elapsed did not advance');
          continue;
        }
        const readOpsDelta = counterDelta(current.readOps, previous.readOps);
        const writeOpsDelta = counterDelta(current.writeOps, previous.writeOps);
        const readTimeDelta = counterDelta(current.readTimeMs, previous.readTimeMs);
        const writeTimeDelta = counterDelta(current.writeTimeMs, previous.writeTimeMs);
        const ioTimeDelta = counterDelta(current.ioTimeMs, previous.ioTimeMs);
        devices.push({
          id: disk.id,
          status: 'ok',
          readBytesPerSec: roundRate((readDelta * 512 * 1000) / elapsedMs),
          writeBytesPerSec: roundRate((writeDelta * 512 * 1000) / elapsedMs),
          readLatencyMs: meanLatencyMs(readOpsDelta, readTimeDelta),
          writeLatencyMs: meanLatencyMs(writeOpsDelta, writeTimeDelta),
          ioUtilizationPercent: ioTimeDelta === null
            ? null
            : Math.round(clampPercent((ioTimeDelta / elapsedMs) * 100) * 100) / 100,
        });
      }
    }

    if (mounts === null && counters === null && devices.length === 0 && filesystems.length === 0) {
      return unavailable<DisksMetric>(sink, 'disks', 'disk discovery unavailable');
    }
    const pressure = readPressure(readers.readFile, '/proc/pressure/io');
    return {
      status: 'ok',
      filesystems,
      devices,
      pressureSomePercent: pressure?.someAvg10 ?? null,
      pressureFullPercent: pressure?.fullAvg10 ?? null,
    };
  }

  function sampleNetwork(sink: FailureSink, atMs: number | null): NetworkMetric {
    const counters = readNetDev(readers.readFile);
    if (counters === null) return unavailable<NetworkMetric>(sink, 'network', 'network counters unavailable');
    const at = atMs ?? readMonotonic();
    if (at === null) return unavailable<NetworkMetric>(sink, 'network', 'network monotonic clock unavailable');
    const interfaces: InterfaceReading[] = [];
    const present = new Set(counters.keys());
    for (const name of [...netBaselines.keys()]) {
      if (!present.has(name)) netBaselines.delete(name);
    }
    for (const name of [...counters.keys()].sort()) {
      const current = counters.get(name)!;
      const previous = netBaselines.get(name);
      netBaselines.set(name, {atMs: at, rxBytes: current.rxBytes, txBytes: current.txBytes});
      if (previous === undefined) {
        interfaces.push({
          id: `net:${name}`,
          status: 'unavailable',
          reason: 'network I/O needs two readings; first interval pending',
        });
        sink.add('network', `net:${name}`, 'network I/O needs two readings; first interval pending');
        continue;
      }
      const rxDelta = current.rxBytes - previous.rxBytes;
      const txDelta = current.txBytes - previous.txBytes;
      const elapsedMs = at - previous.atMs;
      if (rxDelta < 0 || txDelta < 0) {
        interfaces.push({id: `net:${name}`, status: 'unavailable', reason: 'network counters decreased; baseline reset'});
        sink.add('network', `net:${name}`, 'network counters decreased; baseline reset');
        continue;
      }
      if (elapsedMs <= 0) {
        interfaces.push({id: `net:${name}`, status: 'unavailable', reason: 'network monotonic elapsed did not advance'});
        sink.add('network', `net:${name}`, 'network monotonic elapsed did not advance');
        continue;
      }
      interfaces.push({
        id: `net:${name}`,
        status: 'ok',
        rxBytesPerSec: roundRate((rxDelta * 1000) / elapsedMs),
        txBytesPerSec: roundRate((txDelta * 1000) / elapsedMs),
      });
    }
    return {status: 'ok', interfaces};
  }

  function readPayload(sink: FailureSink): MetricsPayload {
    const atMs = readMonotonic();
    let cpu: CpuMetric;
    try {
      cpu = sampleCpu(sink, atMs);
    } catch (error) {
      cpu = unavailable<CpuMetric>(sink, 'cpu', `cpu sampler failed: ${describeError(error)}`);
    }
    let memory: MemoryMetric;
    try {
      memory = sampleMemory(sink);
    } catch (error) {
      memory = unavailable<MemoryMetric>(sink, 'memory', `memory sampler failed: ${describeError(error)}`);
    }
    let swap: BytesUsageMetric;
    try {
      swap = sampleSwap(sink);
    } catch (error) {
      swap = unavailable<BytesUsageMetric>(sink, 'swap', `swap sampler failed: ${describeError(error)}`);
    }
    let gpus: GpusMetric;
    try {
      gpus = sampleGpus(sink);
    } catch (error) {
      gpus = unavailable<GpusMetric>(sink, 'gpus', `gpu sampler failed: ${describeError(error)}`);
    }
    let disks: DisksMetric;
    try {
      disks = sampleDisks(sink, atMs);
    } catch (error) {
      disks = unavailable<DisksMetric>(sink, 'disks', `disk sampler failed: ${describeError(error)}`);
    }
    let network: NetworkMetric;
    try {
      network = sampleNetwork(sink, atMs);
    } catch (error) {
      network = unavailable<NetworkMetric>(sink, 'network', `network sampler failed: ${describeError(error)}`);
    }
    return {cpu, memory, swap, gpus, disks, network};
  }

  return {
    next(): MetricsRecord {
      refreshInventory();
      const sink = createFailureSink();
      let payload: MetricsPayload;
      try {
        payload = readPayload(sink);
      } catch (error) {
        // Last-resort isolation: a bug in the payload orchestrator still emits
        // a contract-valid record, with every family honestly unavailable.
        const reason = capReason(`sampler failed: ${describeError(error)}`);
        payload = {
          cpu: {status: 'unavailable', reason},
          memory: {status: 'unavailable', reason},
          swap: {status: 'unavailable', reason},
          gpus: {status: 'unavailable', reason},
          disks: {status: 'unavailable', reason},
          network: {status: 'unavailable', reason},
        };
        sink.add('metrics', null, reason);
      }
      currentFailures = sink.list();
      let timestamp: number;
      try {
        timestamp = now();
      } catch {
        timestamp = Date.now();
      }
      if (!Number.isFinite(timestamp)) timestamp = Date.now();
      return {
        collectorId: identity.collectorId,
        runId: identity.runId,
        bootId: identity.bootId,
        sequence: sequence.next(),
        capturedAt: new Date(timestamp).toISOString(),
        kind: 'metrics',
        payload,
      };
    },

    inventory(): Inventory {
      return refreshInventory();
    },

    inventoryVersion(): number {
      return version;
    },

    failures(): CollectionFailure[] {
      return [...currentFailures];
    },
  };
}
