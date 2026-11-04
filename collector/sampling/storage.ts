/**
 * Filesystem and block-device discovery for inventory/metrics.
 *
 * Mounts are restricted to real, local filesystems. Pseudo filesystems,
 * snap/loop mounts and container overlays are excluded; btrfs subvolumes are
 * deduplicated by backing device (the shortest mount point wins). Usage comes
 * from `statfs`, while throughput comes from `/proc/diskstats` sector deltas.
 */
import type { DiskInventory, FilesystemReading } from '../../src/contracts';
import { MAX_DISKS, MAX_FILESYSTEMS } from '../../src/contracts';
import type { DiskCounters, MountInfoEntry, ReadTextFile } from './procfs';

export type StatFsSnapshot = {
  bsize: number;
  blocks: number;
  bfree: number;
  bavail?: number;
  /** Total inode slots; omitted by older fixtures/platforms. */
  files?: number;
  /** Free inode slots; omitted by older fixtures/platforms. */
  ffree?: number;
};

export type StatFsReader = (path: string) => StatFsSnapshot;

/** Filesystems with real backing storage and locally useful capacity. */
export const REAL_FS_TYPES: ReadonlySet<string> = new Set([
  'ext2',
  'ext3',
  'ext4',
  'xfs',
  'btrfs',
  'vfat',
  'exfat',
  'f2fs',
  'zfs',
  'ntfs',
  'ntfs3',
]);

const PSEUDO_DEVICE = /^\/dev\/(loop|ram|zram)/;

export type FilesystemCandidate = {
  id: string;
  mountPoint: string;
  device: string;
  fsType: string;
};

export type SelectFilesystemOptions = {
  /** Extra fs types (for example `fuseblk`) explicitly allowed by the caller. */
  allowFsTypes?: readonly string[];
};

/**
 * Selects real filesystems, deduplicated by backing device. For btrfs
 * subvolumes this keeps the shortest mount point, which is the closest to the
 * filesystem root and avoids reporting the same device several times.
 */
export function selectFilesystems(
  entries: readonly MountInfoEntry[],
  options: SelectFilesystemOptions = {},
): FilesystemCandidate[] {
  const allowed = new Set<string>([...REAL_FS_TYPES, ...(options.allowFsTypes ?? [])]);
  const byDevice = new Map<string, FilesystemCandidate>();
  for (const entry of entries) {
    if (!allowed.has(entry.fsType)) continue;
    if (PSEUDO_DEVICE.test(entry.device)) continue;
    if (entry.mountPoint === '' || entry.device === '') continue;
    const candidate: FilesystemCandidate = {
      id: `fs:${entry.mountPoint}`,
      mountPoint: entry.mountPoint,
      device: entry.device,
      fsType: entry.fsType,
    };
    const existing = byDevice.get(entry.device);
    if (existing === undefined || shorterMount(candidate, existing)) {
      byDevice.set(entry.device, candidate);
    }
  }
  return [...byDevice.values()]
    .sort((a, b) => a.mountPoint.localeCompare(b.mountPoint))
    .slice(0, MAX_FILESYSTEMS);
}

function shorterMount(a: FilesystemCandidate, b: FilesystemCandidate): boolean {
  if (a.mountPoint.length !== b.mountPoint.length) return a.mountPoint.length < b.mountPoint.length;
  return a.mountPoint.localeCompare(b.mountPoint) < 0;
}

export type FilesystemUsage = {
  reading: FilesystemReading;
  totalBytes: number | null;
};

/** Reads capacity for one candidate; failures stay per-resource. */
export function readFilesystemUsage(candidate: FilesystemCandidate, statfs: StatFsReader): FilesystemUsage {
  let snapshot: StatFsSnapshot;
  try {
    snapshot = statfs(candidate.mountPoint);
  } catch {
    return {
      reading: {id: candidate.id, status: 'unavailable', reason: 'filesystem statistics unavailable'},
      totalBytes: null,
    };
  }
  const bsize = snapshot.bsize;
  const blocks = snapshot.blocks;
  const bfree = snapshot.bfree;
  if (
    !Number.isFinite(bsize) || bsize <= 0 ||
    !Number.isFinite(blocks) || blocks < 0 ||
    !Number.isFinite(bfree) || bfree < 0 || bfree > blocks
  ) {
    return {
      reading: {id: candidate.id, status: 'unavailable', reason: 'filesystem statistics inconsistent'},
      totalBytes: null,
    };
  }
  const totalBytes = Math.round(blocks * bsize);
  const usedBytes = Math.round((blocks - bfree) * bsize);
  if (
    !Number.isSafeInteger(totalBytes) ||
    !Number.isSafeInteger(usedBytes) ||
    usedBytes < 0 ||
    usedBytes > totalBytes
  ) {
    return {
      reading: {id: candidate.id, status: 'unavailable', reason: 'filesystem statistics inconsistent'},
      totalBytes: null,
    };
  }
  const reading: Extract<FilesystemReading, {status: 'ok'}> = {
    id: candidate.id,
    status: 'ok',
    usedBytes,
    totalBytes,
  };
  const files = snapshot.files;
  const ffree = snapshot.ffree;
  if (
    Number.isSafeInteger(files) && files !== undefined && files > 0 &&
    Number.isSafeInteger(ffree) && ffree !== undefined && ffree >= 0 && ffree <= files
  ) {
    reading.inodesTotal = files;
    reading.inodesUsed = files - ffree;
  }
  return {reading, totalBytes};
}

/** Whole block devices from sysfs (loop/ram/zram excluded; dm-* included). */
export function discoverDisks(readdir: (path: string) => string[], read: ReadTextFile): DiskInventory[] {
  let names: string[];
  try {
    names = readdir('/sys/block');
  } catch {
    return [];
  }
  const disks: DiskInventory[] = [];
  for (const name of [...names].sort()) {
    if (!/^[A-Za-z0-9_.-]+$/.test(name)) continue;
    if (/^(loop|ram|zram)/.test(name)) continue;
    const base = `/sys/block/${name}`;
    let model: string | null = null;
    try {
      const raw = read(`${base}/device/model`).trim();
      model = raw === '' ? null : raw;
    } catch {
      model = null;
    }
    let rotational: boolean | null = null;
    try {
      const raw = read(`${base}/queue/rotational`).trim();
      if (raw === '0') rotational = false;
      else if (raw === '1') rotational = true;
    } catch {
      rotational = null;
    }
    let sizeBytes: number | null = null;
    try {
      const sectors = Number(read(`${base}/size`).trim());
      if (Number.isSafeInteger(sectors) && sectors >= 0) {
        const bytes = sectors * 512;
        if (Number.isSafeInteger(bytes)) sizeBytes = bytes;
      }
    } catch {
      sizeBytes = null;
    }
    disks.push({id: `disk:${name}`, name, model, rotational, sizeBytes});
    if (disks.length >= MAX_DISKS) break;
  }
  return disks;
}

/** Filters `/proc/diskstats` counters to the discovered whole devices. */
export function diskCountersFor(
  counters: ReadonlyMap<string, DiskCounters>,
  disks: readonly DiskInventory[],
): Map<string, DiskCounters> {
  const out = new Map<string, DiskCounters>();
  for (const disk of disks) {
    const counter = counters.get(disk.name);
    if (counter !== undefined) out.set(disk.name, counter);
  }
  return out;
}
