/** Optional Linux DRM/hwmon counters. Missing fields stay null, including
 * dedicated VRAM on integrated GPUs; system RAM is never presented as VRAM. */
import type { GpuReading } from '../../src/contracts';
import { readTextOrNull, type ReadTextFile } from './procfs';

export type SysfsGpuReaders = {readFile: ReadTextFile; readdir: (path: string) => string[]};
type Values = Extract<GpuReading, {status: 'ok'}>;

function number(read: ReadTextFile, path: string, max = Number.MAX_SAFE_INTEGER, min = 0): number | null {
  const raw = readTextOrNull(read, path)?.trim();
  if (!raw || !/^[-]?\d+$/.test(raw)) return null;
  const value = Number(raw);
  return Number.isSafeInteger(value) && value >= min && value <= max ? value : null;
}
function entries(readers: SysfsGpuReaders, path: string): string[] {
  try { return readers.readdir(path).filter(name => /^hwmon\d+$/.test(name)).sort().slice(0, 8); }
  catch { return []; }
}
export function readSysfsGpu(readers: SysfsGpuReaders, address: string): GpuReading {
  const base = `/sys/bus/pci/devices/${address}`;
  const read = readers.readFile;
  const vendor = readTextOrNull(read, `${base}/vendor`)?.trim().toLowerCase();
  const id = `pci:${address}`;
  if (vendor !== '0x1002' && vendor !== '0x8086') return {id, status: 'unavailable', reason: 'no supported DRM telemetry'};
  const values: Values = {id, status: 'ok',
    utilizationPercent: number(read, `${base}/gpu_busy_percent`, 100),
    memoryUsedBytes: number(read, `${base}/mem_info_vram_used`),
    memoryTotalBytes: number(read, `${base}/mem_info_vram_total`), temperatureC: null, powerWatts: null};
  // Zero total is not dedicated memory (common on integrated adapters).
  if (values.memoryTotalBytes === 0) values.memoryTotalBytes = null;
  if (values.memoryTotalBytes === null || (values.memoryUsedBytes !== null && values.memoryUsedBytes > values.memoryTotalBytes)) values.memoryUsedBytes = null;
  for (const hwmon of entries(readers, `${base}/hwmon`)) {
    const path = `${base}/hwmon/${hwmon}`;
    const temp = number(read, `${path}/temp1_input`, 200_000, -50_000);
    const power = number(read, `${path}/power1_average`, 10_000_000_000) ?? number(read, `${path}/power1_input`, 10_000_000_000);
    if (temp !== null && values.temperatureC === null) values.temperatureC = temp / 1000;
    if (power !== null && values.powerWatts === null) values.powerWatts = power / 1_000_000;
  }
  if ([values.utilizationPercent, values.memoryTotalBytes, values.temperatureC, values.powerWatts].every(value => value === null)) {
    return {id, status: 'unavailable', reason: 'GPU detected, but no readable DRM/sysfs counters; driver support or permissions may limit telemetry'};
  }
  return values;
}
