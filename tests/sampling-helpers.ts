/**
 * Shared deterministic fixtures for the sampling tests.
 *
 * Everything here is synthetic: fake procfs/sysfs file contents, fake clocks
 * and a fake GPU poller. The real-hardware test lives in
 * sampling-real.test.ts and is gated behind `RIGSCOPE_REAL_HW=1`.
 */
import type { NetworkInterfaceInfo } from 'node:os';
import type { GpuReading } from '../src/contracts';
import type { GpuAvailability, GpuCostStats, GpuSnapshot } from '../collector/sampling/gpu';
import type { GpuPoller } from '../collector/sampling/gpu';
import type { InventoryReaders } from '../collector/sampling/inventory';

export type FixtureOptions = {
  dirs?: Record<string, string[]>;
  statfs?: Record<string, {bsize: number; blocks: number; bfree: number; files?: number; ffree?: number}>;
  networkInterfaces?: () => NodeJS.Dict<NetworkInterfaceInfo[]>;
  cpuCount?: number;
};

export type FixtureReaders = InventoryReaders & {
  files: Record<string, string>;
  dirs: Record<string, string[]>;
};

function enoent(path: string): NodeJS.ErrnoException {
  const error = new Error(`ENOENT: ${path}`) as NodeJS.ErrnoException;
  error.code = 'ENOENT';
  return error;
}

/** Builds injectable readers over mutable file/directory fixtures. */
export function createReaders(
  files: Record<string, string>,
  options: FixtureOptions = {},
): FixtureReaders {
  const dirs: Record<string, string[]> = {
    '/sys/bus/pci/devices': [],
    '/sys/block': [],
    '/sys/class/net': [],
    '/sys/devices/virtual/net': [],
    ...(options.dirs ?? {}),
  };
  const stats = options.statfs ?? {};
  return {
    files,
    dirs,
    readFile(path: string): string {
      if (Object.prototype.hasOwnProperty.call(files, path)) return files[path]!;
      throw enoent(path);
    },
    readdir(path: string): string[] {
      if (Object.prototype.hasOwnProperty.call(dirs, path)) return [...dirs[path]!];
      throw enoent(path);
    },
    statfs(path: string) {
      return stats[path] ?? {bsize: 4096, blocks: 1000, bfree: 400};
    },
    networkInterfaces: options.networkInterfaces ?? (() => ({})),
    platform: () => 'linux',
    release: () => 'fixture-kernel',
    arch: () => 'x64',
    cpuCount: () => options.cpuCount ?? 2,
  };
}

export function procStatLine(
  values: {user?: number; nice?: number; system?: number; idle?: number; iowait?: number; irq?: number; softirq?: number; steal?: number; guest?: number; guestNice?: number},
): string {
  return [
    values.user ?? 0,
    values.nice ?? 0,
    values.system ?? 0,
    values.idle ?? 0,
    values.iowait ?? 0,
    values.irq ?? 0,
    values.softirq ?? 0,
    values.steal ?? 0,
    values.guest ?? 0,
    values.guestNice ?? 0,
  ].join(' ');
}

export function meminfo(overrides: Record<string, number | null> = {}): string {
  const values: Record<string, number | null> = {
    MemTotal: 1_000,
    MemAvailable: 400,
    SwapTotal: 100,
    SwapFree: 60,
    ...overrides,
  };
  return Object.entries(values)
    .filter(([, value]) => value !== null)
    .map(([key, value]) => `${key}: ${value} kB`)
    .join('\n');
}

export function netDev(lines: Array<[string, number, number]>): string {
  const header = [
    'Inter-|   Receive                                                |  Transmit',
    ' face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed',
  ];
  return [...header, ...lines.map(([name, rx, tx]) => `${name}: ${rx} 1 0 0 0 0 0 0 ${tx} 1 0 0 0 0 0 0`)].join('\n');
}

export type DiskstatsOptions = {
  readsCompleted?: number;
  readTimeMs?: number;
  writesCompleted?: number;
  writeTimeMs?: number;
  ioTimeMs?: number;
};

export function diskstats(lines: Array<[string, number, number, DiskstatsOptions?]>): string {
  return lines
    .map(([name, readSectors, writeSectors, extra = {}], index) => {
      const reads = extra.readsCompleted ?? 10;
      const readTime = extra.readTimeMs ?? 0;
      const writes = extra.writesCompleted ?? 10;
      const writeTime = extra.writeTimeMs ?? 0;
      const ioTime = extra.ioTimeMs ?? 0;
      return `   8 ${index} ${name} ${reads} 0 ${readSectors} ${readTime} ${writes} 0 ${writeSectors} ${writeTime} 0 ${ioTime} 0 0 0 0`;
    })
    .join('\n');
}

export type FakeGpuPollerOptions = {
  availability?: GpuAvailability;
  snapshot?: GpuSnapshot | null;
  info?: Array<{uuid: string; name: string; pciBusId: string | null; memoryTotalBytes: number | null}> | (() => Array<{uuid: string; name: string; pciBusId: string | null; memoryTotalBytes: number | null}>);
  apps?: Map<number, number>;
  pollIntervalMs?: number;
};

/** Test double for the asynchronous poller cache. */
export function fakeGpuPoller(options: FakeGpuPollerOptions = {}): GpuPoller {
  let availability = options.availability ?? 'yes';
  const info = options.info ?? [];
  return {
    pollIntervalMs: options.pollIntervalMs ?? 2_000,
    start() {},
    stop() {},
    pollOnce: async () => {},
    latest: () => options.snapshot ?? null,
    computeApps: () => new Map(options.apps ?? []),
    deviceInfo: () => (typeof info === 'function' ? info() : info),
    available: () => availability,
    costStats: (): GpuCostStats => ({runs: 0, failures: 0, lastDurationMs: 0, avgDurationMs: 0, maxDurationMs: 0}),
  } as unknown as GpuPoller;
}

export function gpuReading(id: string, overrides: Partial<Extract<GpuReading, {status: 'ok'}>> = {}): GpuReading {
  return {
    id,
    status: 'ok',
    utilizationPercent: 50,
    memoryUsedBytes: 1024,
    memoryTotalBytes: 2048,
    temperatureC: 50,
    powerWatts: 100,
    ...overrides,
  };
}

export function pciDeviceFiles(
  address: string,
  vendorId: string,
  deviceClass = '0x030000',
): Record<string, string> {
  return {
    [`/sys/bus/pci/devices/${address}/class`]: `${deviceClass}\n`,
    [`/sys/bus/pci/devices/${address}/vendor`]: `${vendorId}\n`,
  };
}
