/**
 * Pure parsers and safe readers for the Linux `procfs`/`sysfs` count sources
 * used by the metric sampler.
 *
 * Parsing is separated from reading so deterministic tests can feed fixtures
 * (see the sampling parser tests under tests/). Every parser returns `null` or
 * an empty result on malformed input; no partial reading is extrapolated into
 * a rate.
 */
import * as nodeFs from 'node:fs';

/** Minimal text-file reader shared by the samplers. */
export type ReadTextFile = (path: string) => string;

export const defaultReadTextFile: ReadTextFile = (path) =>
  String(nodeFs.readFileSync(path, 'utf8'));

/** Reads a file, returning null instead of throwing. */
export function readTextOrNull(read: ReadTextFile, path: string): string | null {
  try {
    return read(path);
  } catch {
    return null;
  }
}

export type CpuCounters = {
  /** user+nice+system+irq+softirq+steal; guest time is already in user. */
  busy: number;
  /** idle+iowait. */
  idle: number;
  /** iowait is also kept separately so the sampler can report its share. */
  iowait: number;
};

export type CpuCoreCounters = CpuCounters & { index: number };

export type ProcStatSnapshot = {
  total: CpuCounters;
  /** Sorted by core index; sparse indexes are allowed. */
  perCore: CpuCoreCounters[];
  /** Number of per-core lines (not `maxIndex + 1` when indexes are sparse). */
  coreCount: number;
};

const CPU_FIELDS = [
  'user',
  'nice',
  'system',
  'idle',
  'iowait',
  'irq',
  'softirq',
  'steal',
  'guest',
  'guestNice',
] as const;

function numericFields(parts: string[]): number[] | null {
  if (parts.length < 8) return null;
  const values: number[] = [];
  for (let i = 0; i < CPU_FIELDS.length; i += 1) {
    const raw = parts[i];
    if (raw === undefined) {
      values.push(0);
      continue;
    }
    const value = Number(raw);
    if (!Number.isFinite(value) || value < 0) return null;
    values.push(value);
  }
  return values;
}

function countersFromFields(values: number[]): CpuCounters {
  // Indexes follow /proc/stat order: user nice system idle iowait irq softirq
  // steal guest guest_nice. `guest` is included in `user` by the kernel, so it
  // must not be counted again.
  const [user = 0, nice = 0, system = 0, idle = 0, iowait = 0, irq = 0, softirq = 0, steal = 0] = values;
  return {
    busy: user + nice + system + irq + softirq + steal,
    idle: idle + iowait,
    iowait,
  };
}

/**
 * Parses `/proc/stat` CPU lines. The aggregate `cpu` line supplies the total;
 * `cpuN` lines supply per-core counters.
 */
export function parseProcStat(text: string): ProcStatSnapshot | null {
  if (typeof text !== 'string') return null;
  let total: CpuCounters | null = null;
  const perCore: CpuCoreCounters[] = [];
  for (const line of text.split('\n')) {
    const match = /^cpu(\d*)\s+(.*)$/.exec(line.trim());
    if (match === null) continue;
    const values = numericFields(match[2]!.trim().split(/\s+/));
    if (values === null) return null;
    const counters = countersFromFields(values);
    if (match[1] === '') {
      if (total !== null) return null;
      total = counters;
      continue;
    }
    const index = Number(match[1]);
    if (!Number.isSafeInteger(index) || index < 0 || index > 4095) return null;
    perCore.push({index, ...counters});
  }
  if (total === null) return null;
  perCore.sort((a, b) => a.index - b.index);
  return {total, perCore, coreCount: perCore.length};
}

export type MeminfoSnapshot = {
  totalBytes: number | null;
  availableBytes: number | null;
  swapTotalBytes: number | null;
  swapFreeBytes: number | null;
};

const MEMINFO_BYTES_FIELDS = new Map<string, keyof MeminfoSnapshot>([
  ['MemTotal', 'totalBytes'],
  ['MemAvailable', 'availableBytes'],
  ['SwapTotal', 'swapTotalBytes'],
  ['SwapFree', 'swapFreeBytes'],
]);

/**
 * Parses `/proc/meminfo`. Only kB fields are trusted. A missing field stays
 * `null`; callers decide whether that is an unavailable metric.
 */
export function parseMeminfo(text: string): MeminfoSnapshot | null {
  if (typeof text !== 'string') return null;
  const found: MeminfoSnapshot = {
    totalBytes: null,
    availableBytes: null,
    swapTotalBytes: null,
    swapFreeBytes: null,
  };
  let sawAny = false;
  for (const line of text.split('\n')) {
    const match = /^([A-Za-z0-9()_]+):\s+(\d+)\s+kB\s*$/.exec(line);
    if (match === null) continue;
    sawAny = true;
    const key = MEMINFO_BYTES_FIELDS.get(match[1]!);
    if (key === undefined) continue;
    const bytes = Number(match[2]) * 1024;
    if (!Number.isSafeInteger(bytes)) continue;
    found[key] = bytes;
  }
  return sawAny ? found : null;
}

export type NetCounters = {rxBytes: number; txBytes: number};

/** Parses `/proc/net/dev` byte counters. */
export function parseNetDev(text: string): Map<string, NetCounters> {
  const out = new Map<string, NetCounters>();
  if (typeof text !== 'string') return out;
  for (const line of text.split('\n')) {
    const colon = line.indexOf(':');
    if (colon < 0) continue;
    const name = line.slice(0, colon).trim();
    if (name === '' || name === 'Inter-|   Receive' || name.startsWith('face')) continue;
    const fields = line.slice(colon + 1).trim().split(/\s+/);
    if (fields.length < 9) continue;
    const rxBytes = Number(fields[0]);
    const txBytes = Number(fields[8]);
    if (!Number.isSafeInteger(rxBytes) || !Number.isSafeInteger(txBytes)) continue;
    if (rxBytes < 0 || txBytes < 0) continue;
    out.set(name, {rxBytes, txBytes});
  }
  return out;
}

export type DiskCounters = {
  readSectors: number;
  writeSectors: number;
  /** Completed operations; null when the kernel line omits the field. */
  readOps: number | null;
  writeOps: number | null;
  /** Cumulative operation time in milliseconds; null when unavailable. */
  readTimeMs: number | null;
  writeTimeMs: number | null;
  /** Cumulative time the device had I/O in flight, milliseconds. */
  ioTimeMs: number | null;
};

function optionalCounter(raw: string | undefined): number | null {
  if (raw === undefined) return null;
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < 0) return null;
  return value;
}

/**
 * Parses `/proc/diskstats` counters for every listed device (whole devices and
 * partitions; callers filter to whole devices). Sector counters are required;
 * operation counts and times are kept when present so latency/utilization can
 * be derived without inventing values.
 */
export function parseDiskstats(text: string): Map<string, DiskCounters> {
  const out = new Map<string, DiskCounters>();
  if (typeof text !== 'string') return out;
  for (const line of text.split('\n')) {
    const parts = line.trim().split(/\s+/);
    if (parts.length < 10) continue;
    const name = parts[2];
    const readSectors = Number(parts[5]);
    const writeSectors = Number(parts[9]);
    if (name === undefined || name === '') continue;
    if (!Number.isSafeInteger(readSectors) || !Number.isSafeInteger(writeSectors)) continue;
    if (readSectors < 0 || writeSectors < 0) continue;
    out.set(name, {
      readSectors,
      writeSectors,
      readOps: optionalCounter(parts[3]),
      readTimeMs: optionalCounter(parts[6]),
      writeOps: optionalCounter(parts[7]),
      writeTimeMs: optionalCounter(parts[10]),
      ioTimeMs: optionalCounter(parts[12]),
    });
  }
  return out;
}

export type MountInfoEntry = {
  mountPoint: string;
  /** Kernel device path (`/dev/nvme0n1p2`) or pseudo source (`tmpfs`). */
  device: string;
  fsType: string;
  majorMinor: string;
  root: string;
};

/** Decodes the octal escapes used by mountinfo paths (`\040` = space). */
export function decodeMountInfoPath(value: string): string {
  return value.replace(/\\([0-7]{3})/g, (_match, octal: string) =>
    String.fromCharCode(Number.parseInt(octal, 8)),
  );
}

/**
 * Parses `/proc/self/mountinfo`. Optional per-mount fields precede the `-`
 * separator; fs type and source follow it.
 */
export function parseMountinfo(text: string): MountInfoEntry[] {
  const out: MountInfoEntry[] = [];
  if (typeof text !== 'string') return out;
  for (const line of text.split('\n')) {
    const parts = line.trim().split(/\s+/);
    if (parts.length < 10) continue;
    const separator = parts.indexOf('-');
    if (separator < 6 || separator + 2 >= parts.length) continue;
    const mountPoint = decodeMountInfoPath(parts[4]!);
    const fsType = parts[separator + 1]!;
    const device = decodeMountInfoPath(parts[separator + 2]!);
    out.push({
      mountPoint,
      device,
      fsType,
      majorMinor: parts[2] ?? '',
      root: decodeMountInfoPath(parts[3] ?? '/'),
    });
  }
  return out;
}

/** Prefer x86 model name; ARM/RISC-V commonly expose Hardware, cpu model or uarch. */
export function parseCpuinfoModel(text: string): string | null {
  if (typeof text !== 'string') return null;
  for (const key of ['model name', 'Hardware', 'cpu model', 'uarch', 'Processor']) {
    for (const line of text.split('\n')) {
      const colon = line.indexOf(':');
      if (colon < 0 || line.slice(0, colon).trim().toLowerCase() !== key.toLowerCase()) continue;
      const value = line.slice(colon + 1).trim();
      if (value && !/^\d+$/.test(value)) return value;
    }
  }
  return null;
}

/** `PRETTY_NAME` from `/etc/os-release`, with quotes stripped. */
export function parseOsRelease(text: string): string | null {
  if (typeof text !== 'string') return null;
  for (const line of text.split('\n')) {
    const match = /^PRETTY_NAME=(.*)$/.exec(line.trim());
    if (match === null) continue;
    let value = match[1]!.trim();
    if (
      (value.startsWith('"') && value.endsWith('"')) ||
      (value.startsWith("'") && value.endsWith("'"))
    ) {
      value = value.slice(1, -1);
    }
    return value === '' ? null : value;
  }
  return null;
}

/** Reads and parses one procfs path, returning null on any failure. */
export function readProcStat(read: ReadTextFile): ProcStatSnapshot | null {
  const text = readTextOrNull(read, '/proc/stat');
  return text === null ? null : parseProcStat(text);
}

export function readMeminfo(read: ReadTextFile): MeminfoSnapshot | null {
  const text = readTextOrNull(read, '/proc/meminfo');
  return text === null ? null : parseMeminfo(text);
}

export function readNetDev(read: ReadTextFile): Map<string, NetCounters> | null {
  const text = readTextOrNull(read, '/proc/net/dev');
  return text === null ? null : parseNetDev(text);
}

export function readDiskstats(read: ReadTextFile): Map<string, DiskCounters> | null {
  const text = readTextOrNull(read, '/proc/diskstats');
  return text === null ? null : parseDiskstats(text);
}

export function readMountinfo(read: ReadTextFile): MountInfoEntry[] | null {
  const text = readTextOrNull(read, '/proc/self/mountinfo');
  return text === null ? null : parseMountinfo(text);
}
