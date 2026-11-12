/**
 * NVIDIA junction and VRAM temperature sampling — read-only PCI mappings.
 *
 * Reads the same undocumented GPU thermal registers as
 * github.com/ThomasBaruzier/gddr6-core-junction-vram-temps (which credits
 * olealgoritme/gddr6, jjziets/gddr6_temps, igor'sLAB, sunnyyangyangyang/
 * gddr7-temp and biGGer for the register findings):
 *
 *   - GDDR6/GDDR6X junction: BAR0 + 0x0002046C, °C at bits 8..15
 *   - GDDR6/GDDR6X VRAM:     BAR0 + 0x0000E2A8, 12-bit ADC / 32 (°C)
 *   - Blackwell junction:    BAR0 + 0x00AD0A90, 4 die channels and a
 *                            hardware hotspot at +0x10, Q8.8 fixed point
 *   - GDDR7 VRAM:            BAR0 + 0x009024C0, per-FBPA DQR registers
 *                            (stride 0x4000): temperature code at bits
 *                            16..23, 20 = 0 °C, 2 °C per code; validity
 *                            bits at +0x10, poison pattern 0xBADF00xx
 *
 * Access goes through `/sys/bus/pci/devices/<bdf>/resource0`, the kernel's
 * mapped view of BAR0, via a bounded Python 3 mmap helper — no compiled addon, no
 * /dev/mem and no `iomem=relaxed` kernel flag. Reading that file needs
 * permission: it is root-owned 0600 by default, and
 * `packaging/system/90-rigscope-gpu-temps.rules` grants a dedicated group
 * read access (`rigscope gpu-temps enable` installs it).
 *
 * The feature degrades silently: without the ACL the reads fail, the fields
 * stay absent and the poller only logs once. GPU core temperature keeps
 * coming from `nvidia-smi`.
 */
import { readdirSync, readFileSync } from 'node:fs';
import { readMappedPciRegisters } from './pci-registers';

export type GpuSensorValues = {
  junctionTemperatureC: number | null;
  vramTemperatureC: number | null;
};

/** Map keyed by normalized PCI bus id ("01:00.0"). */
export type GpuSensorMap = Map<string, GpuSensorValues>;

/** Interface used by GpuPoller so tests can inject a fixed sensor map. */
export type GpuSensorSource = {
  pollOnce(): Promise<void>;
  latest(): GpuSensorMap;
};

// --- decoding (identical arithmetic to the reference repository) -----------

const GDDR6_TEMP_LIMIT_C = 0x7f;
const GDDR7_ZERO_C_CODE = 20;
const GDDR7_CODE_MIN = 21;
const GDDR7_CODE_MAX = 80;
const GDDR7_POISON_MASK = 0xffff0000;
const GDDR7_POISON_VALUE = 0xbadf0000;

export type GpuSensorProfile = 'gddr6' | 'gddr7';

/** Blackwell PCI device ids whose thermal registers follow the GDDR7 map. */
const BLACKWELL_DEVICE_IDS = new Set([
  0x2b85, // RTX 5090
  0x2b82, // RTX 5080
  0x2c05, // RTX 5070 Ti
  0x2c02, // RTX 5070
  0x2d04, // RTX 5060 Ti
  0x2d19, // RTX 5060 Laptop
  0x2d59, // RTX 5060 Laptop variant
  0x2c33, // RTX PRO 4000 SFF (Blackwell generation)
]);

export function selectSensorProfile(deviceId: number): GpuSensorProfile {
  return BLACKWELL_DEVICE_IDS.has(deviceId) ? 'gddr7' : 'gddr6';
}

/**
 * All-zero GDDR6 readings mean the therm registers are not answering (an
 * unsupported card such as RTX 3070, or a block that reads zeros), not a
 * real 0 °C: a running card never sits at the freezing point. Like
 * `decodeBlackwell`, reject zero so unsupported cards report null.
 */
export function decodeGddr6Junction(raw: number | null): number | null {
  if (raw === null || raw === 0 || raw === 0xffffffff) return null;
  const celsius = (raw >>> 8) & 0xff;
  return celsius === 0 || celsius >= GDDR6_TEMP_LIMIT_C ? null : celsius;
}

export function decodeGddr6Vram(raw: number | null): number | null {
  if (raw === null || raw === 0 || raw === 0xffffffff) return null;
  const celsius = (raw & 0xfff) / 32;
  return celsius === 0 || celsius >= GDDR6_TEMP_LIMIT_C ? null : Math.round(celsius * 10) / 10;
}

export function decodeBlackwell(raw: number | null): number | null {
  if (raw === null || raw === 0xffffffff) return null;
  const fixed = raw & 0xffff;
  if (!fixed || fixed > 150 * 256) return null;
  return Math.round(fixed / 256 * 10) / 10;
}

export function isGddr7Poison(raw: number): boolean {
  return (raw & GDDR7_POISON_MASK) === GDDR7_POISON_VALUE;
}

export function decodeGddr7(raw: number | null): number | null {
  if (raw === null || raw === 0xffffffff || isGddr7Poison(raw)) return null;
  const code = (raw >>> 16) & 0xff;
  if (code < GDDR7_CODE_MIN || code > GDDR7_CODE_MAX) return null;
  return (code - GDDR7_ZERO_C_CODE) * 2; // 2 °C per code
}

/**
 * Merge key for joining register readings with `nvidia-smi` rows: the
 * trailing `bus:device.function` part in lower case, so
 * `00000000:01:00.0` (nvidia-smi) and `0000:01:00.0` (sysfs) both become
 * `01:00.0`. Returns null for anything that does not look like a BDF.
 *
 * Deliberately NOT `normalizePciBusId` from `sampling/inventory.ts`: that
 * one preserves the PCI domain, while this merge key drops it on purpose so
 * the two bus-id spellings join. Do not unify them.
 */
export function normalizeBusFunctionKey(raw: string | null | undefined): string | null {
  if (typeof raw !== 'string') return null;
  const parts = raw.trim().toLowerCase().split(':');
  if (parts.length < 2) return null;
  const bus = parts[parts.length - 2] ?? '';
  const deviceFunction = parts[parts.length - 1] ?? '';
  if (!/^[0-9a-f]{1,4}$/.test(bus) || !/^[0-9a-f]{2}\.[0-7]$/.test(deviceFunction)) return null;
  return `${bus}:${deviceFunction}`;
}

const SYSFS_PCI_DIR = '/sys/bus/pci/devices';
const MAX_GPU_DEVICES = 16;

/** Kill switch, e.g. `COLLECTOR_GPU_TEMPS=off`. */
export const GPU_TEMPS_DISABLED_ENV = 'COLLECTOR_GPU_TEMPS';

// --- register map ----------------------------------------------------------

const GDDR6_JUNCTION_THERM_OFFSET = 0x0002046c;
const GDDR6_VRAM_ADC_OFFSET = 0x0000e2a8;

const BLACKWELL_THERM_OFFSET = 0x00ad0a90;
const BLACKWELL_THERM_CHANNEL_COUNT = 4;
const BLACKWELL_THERM_HW_HOTSPOT_OFFSET = 0x10;

const GDDR7_DQR_OFFSET = 0x009024c0;
const GDDR7_DQR_VALID_OFFSET = 0x10;
const GDDR7_DQR_STRIDE = 0x4000;
const GDDR7_DQR_SUBREGISTER_COUNT = 4;
const GDDR7_MAX_FBPA_COUNT = 16;

/** Injectable file access so tests drive the register decode synthetically. */
export type GpuRegisterReaders = {
  /** Directory names under /sys/bus/pci/devices (or a test root). */
  readdir: (path: string) => string[];
  /** Trimmed sysfs attribute text, null when unreadable. */
  readAttribute: (path: string) => string | null;
  /** 32-bit little-endian BAR0 read, null when the offset is unreadable. */
  readRegister: (resourcePath: string, offset: number) => number | null;
  /** Optional batched BAR0 read: one resource0 open for many offsets. */
  readRegisters?: (resourcePath: string, offsets: readonly number[]) => Array<number | null>;
  /** Optional read-permission probe, used to tell "unsupported" from "no access". */
  canRead?: (path: string) => boolean;
};

/** Batched register read, falling back to one call per offset. */
export function readRegisters(
  readers: GpuRegisterReaders,
  resourcePath: string,
  offsets: readonly number[],
): Array<number | null> {
  if (readers.readRegisters !== undefined) return readers.readRegisters(resourcePath, offsets);
  return offsets.map((offset) => readers.readRegister(resourcePath, offset));
}

/** Whether a path can be read, falling back to probing offset 0. */
export function canReadResource(readers: GpuRegisterReaders, resourcePath: string): boolean {
  if (readers.canRead !== undefined) return readers.canRead(resourcePath);
  return readers.readRegister(resourcePath, 0) !== null;
}

/** Real-filesystem readers; exported so the CLI probe reuses them verbatim. */
export const defaultReaders: GpuRegisterReaders = {
  readdir: (path) => readdirSync(path),
  readAttribute: (path) => {
    try {
      return readFileSync(path, 'utf8').trim();
    } catch {
      return null;
    }
  },
  readRegister: (resourcePath, offset) => readMappedPciRegisters(resourcePath, [offset])[0] ?? null,
  readRegisters: readMappedPciRegisters,
  canRead: (path) => readMappedPciRegisters(path, [0])[0] !== null,
};

function parseHexAttribute(raw: string | null): number | null {
  if (raw === null) return null;
  const match = /^0x([0-9a-f]+)$/i.exec(raw);
  if (match === null) return null;
  const value = Number.parseInt(match[1], 16);
  return Number.isSafeInteger(value) ? value : null;
}

/** PCI base class/sub-class: 0x0300 VGA, 0x0302 3D controller. */
function isDisplayClass(classCode: number): boolean {
  const base = (classCode >>> 8) & 0xffff;
  return base === 0x0300 || base === 0x0302;
}

// --- per-device sampling ----------------------------------------------------

function sampleGddr6(readers: GpuRegisterReaders, resourcePath: string): GpuSensorValues {
  const [junction, vram] = readRegisters(readers, resourcePath, [
    GDDR6_JUNCTION_THERM_OFFSET,
    GDDR6_VRAM_ADC_OFFSET,
  ]);
  return {
    junctionTemperatureC: decodeGddr6Junction(junction ?? null),
    vramTemperatureC: decodeGddr6Vram(vram ?? null),
  };
}

function hottest(a: number | null, b: number | null): number | null {
  if (a === null) return b;
  if (b === null) return a;
  return a >= b ? a : b;
}

function hottestDecoded(
  raws: readonly (number | null)[],
  decode: (raw: number | null) => number | null,
): number | null {
  let best: number | null = null;
  for (const raw of raws) best = hottest(best, decode(raw ?? null));
  return best;
}

function blackwellThermOffsets(): number[] {
  const offsets: number[] = [];
  for (let channel = 0; channel < BLACKWELL_THERM_CHANNEL_COUNT; channel += 1) {
    offsets.push(BLACKWELL_THERM_OFFSET + channel * 4);
  }
  offsets.push(BLACKWELL_THERM_OFFSET + BLACKWELL_THERM_HW_HOTSPOT_OFFSET);
  return offsets;
}

function sampleBlackwellJunction(
  readers: GpuRegisterReaders,
  resourcePath: string,
  prefetched?: readonly (number | null)[],
): number | null {
  // The reference tool reports junction as "the hottest valid die thermal
  // channel or hardware hotspot reading" (see the module header), so the
  // +0x10 hardware-hotspot register participates alongside the 4 channels.
  const raws = prefetched ?? readRegisters(readers, resourcePath, blackwellThermOffsets());
  return hottestDecoded(raws, decodeBlackwell);
}

function gddr7StatusOffsets(): number[] {
  const offsets: number[] = [];
  for (let fbpa = 0; fbpa < GDDR7_MAX_FBPA_COUNT; fbpa += 1) {
    offsets.push(GDDR7_DQR_OFFSET + fbpa * GDDR7_DQR_STRIDE + GDDR7_DQR_VALID_OFFSET);
  }
  return offsets;
}

/** Offsets of every valid DQR temperature entry advertised by the statuses. */
function gddr7TemperatureOffsets(statuses: readonly (number | null)[]): number[] {
  const offsets: number[] = [];
  for (let fbpa = 0; fbpa < statuses.length; fbpa += 1) {
    const status = statuses[fbpa];
    if (status === null || status === undefined || status === 0xffffffff || isGddr7Poison(status)) continue;
    const fbpaBase = fbpa * GDDR7_DQR_STRIDE;
    for (let sub = 0; sub < GDDR7_DQR_SUBREGISTER_COUNT; sub += 1) {
      if (!(status & (1 << (24 + sub)))) continue;
      offsets.push(GDDR7_DQR_OFFSET + fbpaBase + sub * 4);
    }
  }
  return offsets;
}

/**
 * Scans every FBPA's valid DQR temperature entries and returns the hottest.
 * This matches the reference tool's "hottest source" output for standard,
 * paired and clamshell topologies without needing per-partition grouping.
 */
function sampleGddr7Vram(
  readers: GpuRegisterReaders,
  resourcePath: string,
  statuses: readonly (number | null)[],
): number | null {
  const offsets = gddr7TemperatureOffsets(statuses);
  if (offsets.length === 0) return null;
  return hottestDecoded(readRegisters(readers, resourcePath, offsets), decodeGddr7);
}

/**
 * A GDDR7 device uses both the Blackwell thermal block (junction) and the
 * per-FBPA DQR registers (VRAM) from the same BAR0. The first batch carries
 * the FBPA statuses plus the Blackwell offsets, so a batching reader opens
 * resource0 at most twice: once here and once for the valid DQR entries.
 * Without a batching reader the helper falls back to one open per offset.
 */
function sampleGddr7(readers: GpuRegisterReaders, resourcePath: string): GpuSensorValues {
  const statusOffsets = gddr7StatusOffsets();
  const first = readRegisters(readers, resourcePath, [
    ...statusOffsets,
    ...blackwellThermOffsets(),
  ]);
  return {
    junctionTemperatureC: sampleBlackwellJunction(
      readers,
      resourcePath,
      first.slice(statusOffsets.length),
    ),
    vramTemperatureC: sampleGddr7Vram(
      readers,
      resourcePath,
      first.slice(0, statusOffsets.length),
    ),
  };
}

function sampleDevice(
  readers: GpuRegisterReaders,
  deviceDir: string,
  deviceId: number,
): GpuSensorValues | null {
  const resourcePath = `${deviceDir}/resource0`;
  const profile = selectSensorProfile(deviceId);
  if (profile === 'gddr7') {
    const values = sampleGddr7(readers, resourcePath);
    if (values.junctionTemperatureC === null && values.vramTemperatureC === null) return null;
    return values;
  }
  const values = sampleGddr6(readers, resourcePath);
  if (values.junctionTemperatureC === null && values.vramTemperatureC === null) return null;
  return values;
}

/** One NVIDIA device discovered under sysfs. */
export type NvdisplayDevice = {bdf: string; deviceId: number};

export function discoverNvidiaDisplays(
  readers: GpuRegisterReaders,
  sysfsDir: string = SYSFS_PCI_DIR,
): NvdisplayDevice[] {
  let names: string[];
  try {
    names = readers.readdir(sysfsDir);
  } catch {
    return [];
  }
  const devices: NvdisplayDevice[] = [];
  for (const name of names.slice(0, 256)) {
    const dir = `${sysfsDir}/${name}`;
    const vendor = parseHexAttribute(readers.readAttribute(`${dir}/vendor`));
    if (vendor !== 0x10de) continue;
    const classCode = parseHexAttribute(readers.readAttribute(`${dir}/class`));
    if (classCode === null || !isDisplayClass(classCode)) continue;
    const deviceId = parseHexAttribute(readers.readAttribute(`${dir}/device`));
    if (deviceId === null) continue;
    devices.push({bdf: name, deviceId});
    if (devices.length >= MAX_GPU_DEVICES) break;
  }
  return devices;
}

/** One sampling pass across every NVIDIA display device. */
export function readGpuRegisterTemps(
  readers: GpuRegisterReaders = defaultReaders,
  sysfsDir: string = SYSFS_PCI_DIR,
): GpuSensorMap {
  const sensors: GpuSensorMap = new Map();
  for (const device of discoverNvidiaDisplays(readers, sysfsDir)) {
    const key = normalizeBusFunctionKey(device.bdf);
    if (key === null) continue;
    const values = sampleDevice(readers, `${sysfsDir}/${device.bdf}`, device.deviceId);
    // Unreadable registers stay absent: "no sensor" must not become a
    // permanent null series, and machines without the ACL look untouched.
    if (values === null) continue;
    sensors.set(key, values);
  }
  return sensors;
}

// --- poller ------------------------------------------------------------------

export type GpuTempsPollerOptions = {
  /** Injectable file access (tests). */
  readers?: Partial<GpuRegisterReaders>;
  sysfsDir?: string;
  /** Short diagnostic line sink. */
  onEvent?: (line: string) => void;
};

/**
 * Reads the register temperatures on demand: `GpuPoller` drives the cadence
 * by calling `pollOnce()` each cycle it already runs for `nvidia-smi`, so
 * this poller owns no timer of its own. Register access uses a bounded, unprivileged helper; the diagnostics log once per state change, never per cycle.
 */
export class GpuTempsPoller implements GpuSensorSource {
  private readonly readers: GpuRegisterReaders;
  private readonly sysfsDir: string;
  private readonly onEvent?: (line: string) => void;

  private inflight: Promise<void> | null = null;
  private sensors: GpuSensorMap = new Map();
  private state: 'unknown' | 'devices' | 'none' | 'access' | 'unsupported' = 'unknown';

  constructor(options: GpuTempsPollerOptions = {}) {
    const provided = options.readers ?? {};
    const merged: GpuRegisterReaders = {...defaultReaders, ...provided};
    // An injected read path must not silently inherit the real-filesystem
    // batch/probe helpers: they would bypass the fake and defeat partial
    // fixtures. A batch-only injection gets a batch-backed single read, and
    // the permission probe is derived from whichever read path is in effect.
    const customRead = provided.readRegister !== undefined || provided.readRegisters !== undefined;
    if (customRead) {
      if (provided.readRegisters === undefined) {
        delete merged.readRegisters;
      } else if (provided.readRegister === undefined) {
        const batch = provided.readRegisters!;
        merged.readRegister = (resourcePath, offset) => batch(resourcePath, [offset])[0] ?? null;
      }
      if (provided.canRead === undefined) {
        merged.canRead = (path) => merged.readRegister(path, 0) !== null;
      }
    }
    this.readers = merged;
    this.sysfsDir = options.sysfsDir ?? SYSFS_PCI_DIR;
    this.onEvent = options.onEvent;
  }

  pollOnce(): Promise<void> {
    if (this.inflight !== null) return this.inflight;
    const run = Promise.resolve().then(() => this.runCycle());
    this.inflight = run;
    void run.finally(() => {
      if (this.inflight === run) this.inflight = null;
    });
    return run;
  }

  latest(): GpuSensorMap {
    return new Map(this.sensors);
  }

  private runCycle(): void {
    let sensors: GpuSensorMap;
    try {
      sensors = readGpuRegisterTemps(this.readers, this.sysfsDir);
    } catch {
      return;
    }
    this.sensors = sensors;
    if (sensors.size > 0) {
      this.emitOnStateChange('devices', () => `gpu register temps: ${sensors.size} device(s)`);
      return;
    }
    // Nothing decoded: distinguish "no NVIDIA GPU", "no permission" and a
    // readable device whose thermal registers are unsupported.
    const devices = discoverNvidiaDisplays(this.readers, this.sysfsDir);
    if (devices.length === 0) {
      this.emitOnStateChange('none', () => 'gpu register temps: no NVIDIA display devices');
      return;
    }
    const readable = devices.some((device) =>
      canReadResource(this.readers, `${this.sysfsDir}/${device.bdf}/resource0`));
    if (readable) {
      this.emitOnStateChange('unsupported', () =>
        `gpu register temps: no supported thermal registers on ${devices.length} device(s)`);
      return;
    }
    this.emitOnStateChange('access', () => 'gpu register temps: GPU BAR0 mapping failed (needs Python 3 and read access); '
      + 'run rigscope gpu-temps status. If access was granted after this collector started, it applies after a reboot');
  }

  /** Logs the line only when the cycle's state differs from the last one. */
  private emitOnStateChange(next: 'devices' | 'none' | 'access' | 'unsupported', line: () => string): void {
    const changed = this.state !== next;
    this.state = next;
    if (!changed) return;
    try {
      this.onEvent?.(line());
    } catch {
      // Diagnostics must never break polling.
    }
  }
}

export function gpuTempsEnabled(env: Record<string, string | undefined> = process.env): boolean {
  return (env[GPU_TEMPS_DISABLED_ENV] ?? '').trim().toLowerCase() !== 'off';
}
