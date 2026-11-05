/**
 * Builds the `Inventory` from Linux sysfs/procfs plus the GPU poller cache.
 *
 * Inventory is data, not UI copy: unsupported management adapters (for example
 * the ASPEED VGA on the target host) are listed with `telemetry:
 * "unsupported"` and an honest reason, never with fabricated readings. NVIDIA
 * devices are joined to `nvidia-smi` output by normalized PCI bus id and then
 * use the stable `gpu:<uuid>` id; unjoined PCI display devices use
 * `pci:<address>`.
 */
import type {
  CapabilityState,
  GpuInventory,
  Inventory,
  MetricFamily,
} from '../../src/contracts';
import {
  MAX_CORES,
  MAX_GPUS,
  MAX_INTERFACES,
} from '../../src/contracts';
import type { GpuAvailability, GpuDeviceInfo } from './gpu';
import { readSysfsGpu } from './sysfs-gpu';
import type { NetworkInterfacesReader } from './network';
import { discoverInterfaces } from './network';
import {
  parseCpuinfoModel,
  parseOsRelease,
  parseProcStat,
  readMeminfo,
  readMountinfo,
  readTextOrNull,
  type CpuCounters,
  type ReadTextFile,
} from './procfs';
import {
  discoverDisks,
  readFilesystemUsage,
  selectFilesystems,
  type SelectFilesystemOptions,
  type StatFsReader,
} from './storage';

export type InventoryReaders = {
  readFile: ReadTextFile;
  readdir: (path: string) => string[];
  statfs: StatFsReader;
  networkInterfaces: NetworkInterfacesReader;
  platform: () => string;
  release: () => string;
  arch: () => string;
  cpuCount: () => number;
};

export type GpuInventorySource = {
  deviceInfo(): GpuDeviceInfo[];
  available(): GpuAvailability;
} | null;

export type BuildInventoryOptions = SelectFilesystemOptions;

const MAX_REASON = 256;
const MAX_TEXT = 256;

export function capText(value: string, max = MAX_TEXT): string {
  const clean = value.replace(/[\u0000-\u001f\u007f]/g, ' ').trim();
  if (clean.length <= max) return clean;
  return `${clean.slice(0, max - 3)}...`;
}

export function capReason(value: string): string {
  return capText(value, MAX_REASON);
}

/** Normalize domain width and case without collapsing distinct PCI domains. */
export function normalizePciBusId(value: string): string {
  const lower = value.trim().toLowerCase();
  const match = /^(?:([0-9a-f]{4,8}):)?([0-9a-f]{2}:[0-9a-f]{2}\.[0-7])$/.exec(lower);
  return match ? `${(match[1] ?? '0').replace(/^0+/, '').padStart(4, '0')}:${match[2]}` : lower;
}

function vendorFor(raw: string): GpuInventory['vendor'] {
  switch (raw.trim().toLowerCase()) {
    case '0x10de':
      return 'nvidia';
    case '0x1002':
      return 'amd';
    case '0x8086':
      return 'intel';
    case '0x1a03':
      return 'aspeed';
    default:
      return 'other';
  }
}

function isDisplayClass(raw: string): boolean {
  return /^0x03[0-9a-f]{4}$/i.test(raw.trim());
}

type PciDisplayDevice = {address: string; vendor: GpuInventory['vendor']; vendorId: string};

function enumeratePciDisplayDevices(read: ReadTextFile, readdir: (path: string) => string[]): PciDisplayDevice[] {
  let entries: string[];
  try {
    entries = readdir('/sys/bus/pci/devices');
  } catch {
    return [];
  }
  const devices: PciDisplayDevice[] = [];
  for (const address of [...entries].sort()) {
    const base = `/sys/bus/pci/devices/${address}`;
    const classText = readTextOrNull(read, `${base}/class`);
    if (classText === null || !isDisplayClass(classText)) continue;
    const vendorId = (readTextOrNull(read, `${base}/vendor`) ?? '').trim().toLowerCase();
    devices.push({address, vendor: vendorFor(vendorId), vendorId});
  }
  return devices;
}

function fallbackGpuName(vendor: GpuInventory['vendor'], address: string): string {
  if (vendor === 'aspeed') return 'ASPEED management VGA';
  const labels: Record<Exclude<GpuInventory['vendor'], 'aspeed'>, string> = {
    nvidia: 'NVIDIA',
    amd: 'AMD',
    intel: 'Intel',
    other: 'Display',
  };
  return `${labels[vendor]} display adapter`;
}

/** Resolve optional distro PCI names without invoking a command per sample. */
export function pciDeviceName(database: string, vendorId: string, deviceId: string): string | null {
  let vendor = false;
  for (const line of database.split('\n')) {
    const heading = /^([0-9a-f]{4})\s+/i.exec(line);
    if (heading) vendor = heading[1]!.toLowerCase() === vendorId.replace(/^0x/, '').toLowerCase();
    if (!vendor) continue;
    const device = /^\t([0-9a-f]{4})\s+(.+)$/i.exec(line);
    if (device && device[1]!.toLowerCase() === deviceId.replace(/^0x/, '').toLowerCase()) return capText(device[2]!);
  }
  return null;
}

function unsupportedGpuReason(vendor: GpuInventory['vendor'], gpu: GpuInventorySource): string {
  if (vendor === 'aspeed') return 'management adapter; no telemetry';
  if (vendor === 'nvidia' && (gpu === null || gpu.available() === 'no')) {
    return 'no supported GPU telemetry (nvidia-smi not found)';
  }
  return 'no supported telemetry';
}

function buildGpus(
  read: ReadTextFile,
  readdir: (path: string) => string[],
  gpu: GpuInventorySource,
): GpuInventory[] {
  const pciDevices = enumeratePciDisplayDevices(read, readdir);
  const nvidiaInfo = new Map<string, GpuDeviceInfo>();
  if (gpu !== null) {
    for (const info of gpu.deviceInfo()) {
      if (info.pciBusId !== null && info.pciBusId !== '') {
        nvidiaInfo.set(normalizePciBusId(info.pciBusId), info);
      }
    }
  }
  const pciNames = readTextOrNull(read, '/usr/share/hwdata/pci.ids') ?? readTextOrNull(read, '/usr/share/misc/pci.ids') ?? '';
  const result: GpuInventory[] = [];
  const seenIds = new Set<string>();
  for (const device of pciDevices) {
    let entry: GpuInventory;
    const info = device.vendor === 'nvidia'
      ? nvidiaInfo.get(normalizePciBusId(device.address))
      : undefined;
    if (info !== undefined) {
      entry = {
        id: `gpu:${info.uuid}`,
        name: capText(info.name === '' ? fallbackGpuName(device.vendor, device.address) : info.name),
        vendor: device.vendor,
        pciBusId: device.address,
        telemetry: 'supported',
        memoryTotalBytes: info.memoryTotalBytes,
      };
    } else {
      const reading = readSysfsGpu({readFile: read, readdir}, device.address);
      const product = readTextOrNull(read, `/sys/bus/pci/devices/${device.address}/product_name`)?.trim();
      entry = {
        id: `pci:${device.address}`,
        name: capText(product || pciDeviceName(pciNames, device.vendorId, readTextOrNull(read, `/sys/bus/pci/devices/${device.address}/device`)?.trim() ?? '') || fallbackGpuName(device.vendor, device.address)),
        vendor: device.vendor,
        pciBusId: device.address,
        telemetry: reading.status === 'ok' ? 'supported' : 'unsupported',
        memoryTotalBytes: reading.status === 'ok' ? reading.memoryTotalBytes : null,
        ...(reading.status === 'ok' ? {} : {reason: capReason(['amd', 'intel'].includes(device.vendor) ? reading.reason : unsupportedGpuReason(device.vendor, gpu))}),
      };
    }
    if (seenIds.has(entry.id)) continue;
    seenIds.add(entry.id);
    result.push(entry);
    if (result.length >= MAX_GPUS) break;
  }
  return result;
}

function memoryFamilyStatus(meminfo: ReturnType<typeof readMeminfo>): CapabilityState {
  if (meminfo === null || meminfo.totalBytes === null) {
    return {status: 'unsupported', reason: 'memory counters unavailable'};
  }
  return {status: 'supported'};
}

function gpuCapability(gpus: readonly GpuInventory[], gpu: GpuInventorySource): CapabilityState {
  if (gpus.some((entry) => entry.telemetry === 'supported')) return {status: 'supported'};
  if (gpus.length === 0) return {status: 'unsupported', reason: 'no GPUs detected'};
  if (gpus.every(entry => entry.vendor !== 'nvidia')) return {status: 'unsupported', reason: 'no readable DRM GPU counters'};
  if (gpu === null || gpu.available() === 'no' || gpu.available() === 'unknown') {
    return {status: 'unsupported', reason: 'no supported GPU telemetry (nvidia-smi not found)'};
  }
  return {status: 'unsupported', reason: 'no supported GPU telemetry'};
}

function cpuCores(stat: ReturnType<typeof parseProcStat>, readers: InventoryReaders): number {
  if (stat !== null && stat.coreCount > 0) return stat.coreCount;
  try {
    const count = readers.cpuCount();
    if (Number.isSafeInteger(count) && count >= 0) return Math.min(count, MAX_CORES);
  } catch {
    // fall through
  }
  return 0;
}

function readCpuModel(read: ReadTextFile): string | null {
  const text = readTextOrNull(read, '/proc/cpuinfo');
  return text === null ? null : parseCpuinfoModel(text);
}

function readDistro(read: ReadTextFile): string | null {
  const text = readTextOrNull(read, '/etc/os-release');
  return text === null ? null : parseOsRelease(text);
}

/**
 * Builds inventory from current host state. Individual sources degrade
 * independently: an unreadable mount table does not hide CPU or GPUs.
 */
export function buildInventory(
  readers: InventoryReaders,
  gpu: GpuInventorySource,
  options: BuildInventoryOptions = {},
): Inventory {
  const read = readers.readFile;
  let stat: ReturnType<typeof parseProcStat> = null;
  try {
    const text = readTextOrNull(read, '/proc/stat');
    stat = text === null ? null : parseProcStat(text);
  } catch {
    stat = null;
  }
  let meminfo: ReturnType<typeof readMeminfo> = null;
  try {
    meminfo = readMeminfo(read);
  } catch {
    meminfo = null;
  }

  let platform = 'linux';
  let release = '';
  let arch = '';
  try {
    platform = readers.platform();
    release = readers.release();
    arch = readers.arch();
  } catch {
    // keep the conservative defaults
  }

  const gpus = buildGpus(read, readers.readdir, gpu);

  let mounts: ReturnType<typeof readMountinfo> = null;
  try {
    mounts = readMountinfo(read);
  } catch {
    mounts = null;
  }
  const filesystems = selectFilesystems(mounts ?? [], options)
    .map((candidate) => {
      const usage = readFilesystemUsage(candidate, readers.statfs);
      return {
        id: candidate.id,
        mountPoint: candidate.mountPoint,
        device: candidate.device,
        fsType: candidate.fsType,
        totalBytes: usage.totalBytes,
      };
    });

  let disks: Inventory['disks'] = [];
  try {
    disks = discoverDisks(readers.readdir, read);
  } catch {
    disks = [];
  }

  let network: Inventory['network'] = [];
  try {
    network = discoverInterfaces(readers.networkInterfaces, read, readers.readdir);
  } catch {
    network = [];
  }
  if (network.length > MAX_INTERFACES) network = network.slice(0, MAX_INTERFACES);

  const cpuStatus: CapabilityState = stat === null
    ? {status: 'unsupported', reason: 'procfs unavailable'}
    : {status: 'supported'};
  const memoryStatus = memoryFamilyStatus(meminfo);
  const processes: CapabilityState = platform === 'linux' && stat !== null
    ? {status: 'supported'}
    : {status: 'unsupported', reason: 'processes need Linux procfs'};
  const disksStatus: CapabilityState = mounts === null
    ? {status: 'unsupported', reason: 'mount information unavailable'}
    : {status: 'supported'};
  const networkStatus: CapabilityState = network.length > 0
    ? {status: 'supported'}
    : {status: 'unsupported', reason: 'no network interfaces detected'};

  const capabilities: Record<MetricFamily | 'processes', CapabilityState> = {
    cpu: cpuStatus,
    memory: memoryStatus,
    swap: memoryStatus,
    gpus: gpuCapability(gpus, gpu),
    disks: disksStatus,
    network: networkStatus,
    processes,
  };

  return {
    os: {
      platform: capText(platform, 64),
      release: capText(release),
      distro: (() => {
        const distro = readDistro(read);
        return distro === null ? null : capText(distro);
      })(),
      arch: capText(arch, 64),
    },
    cpu: {
      model: (() => {
        const model = readCpuModel(read);
        return model === null ? null : capText(model);
      })(),
      logicalCores: Math.min(cpuCores(stat, readers), MAX_CORES),
    },
    memory: {
      totalBytes: meminfo?.totalBytes ?? null,
      swapTotalBytes: meminfo?.swapTotalBytes ?? null,
    },
    gpus,
    filesystems,
    disks,
    network,
    capabilities,
  };
}

/** Busy/idle counters exported for tests and the metric sampler. */
export type {CpuCounters};
