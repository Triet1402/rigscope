/**
 * Explicit synthetic metrics for mock mode only.
 *
 * Every record and inventory field is clearly fake: eight logical cores, a
 * single `gpu:SYNTHETIC-0` device and names prefixed `synthetic`. No host
 * identity leaks into the values, so a screenshot or fixture cannot be
 * mistaken for real hardware. The shape matches the real `MetricsSampler`, so
 * the collector composition root can swap implementations without branching
 * on payload shape.
 */
import type {
  BytesUsageMetric,
  CollectionFailure,
  Inventory,
  MetricsRecord,
} from '../../src/contracts';
import type { MetricsIdentity, MetricsSampler } from './metrics';
import type { SequenceAllocator } from './sequence';

const SYNTHETIC_CORES = 8;
const SYNTHETIC_MEMORY_BYTES = 32 * 1024 * 1024 * 1024;
const SYNTHETIC_SWAP_BYTES = 8 * 1024 * 1024 * 1024;
const SYNTHETIC_FS_BYTES = 500 * 1024 * 1024 * 1024;
const SYNTHETIC_GPU_MEMORY_BYTES = 8 * 1024 * 1024 * 1024;
const CPU_PERIOD_MS = 37_000;
const MEMORY_PERIOD_MS = 91_000;

export type SyntheticMetricsOptions = {
  sequence: SequenceAllocator;
  now?: () => number;
  rand?: () => number;
};

function clamp(value: number, minValue: number, maxValue: number): number {
  return Math.min(maxValue, Math.max(minValue, value));
}

function percent(value: number): number {
  return Math.round(clamp(value, 0, 100) * 100) / 100;
}

function usage(fraction: number, total: number): Extract<BytesUsageMetric, {status: 'ok'}> {
  const used = Math.round(clamp(fraction, 0, 1) * total);
  return {status: 'ok', usedBytes: used, totalBytes: total};
}

const SYNTHETIC_INVENTORY: Inventory = {
  os: {
    platform: 'synthetic',
    release: 'synthetic-0',
    distro: 'Synthetic Linux',
    arch: 'synthetic',
  },
  cpu: {model: 'synthetic 8-core processor', logicalCores: SYNTHETIC_CORES},
  memory: {totalBytes: SYNTHETIC_MEMORY_BYTES, swapTotalBytes: SYNTHETIC_SWAP_BYTES},
  gpus: [
    {
      id: 'gpu:SYNTHETIC-0',
      name: 'synthetic GPU',
      vendor: 'other',
      pciBusId: null,
      telemetry: 'supported',
      memoryTotalBytes: SYNTHETIC_GPU_MEMORY_BYTES,
    },
  ],
  filesystems: [
    {
      id: 'fs:/',
      mountPoint: '/',
      device: 'synthetic',
      fsType: 'syntheticfs',
      totalBytes: SYNTHETIC_FS_BYTES,
    },
  ],
  disks: [
    {
      id: 'disk:synthetic0',
      name: 'synthetic0',
      model: 'synthetic disk',
      rotational: false,
      sizeBytes: SYNTHETIC_FS_BYTES,
    },
  ],
  network: [
    {
      id: 'net:synthetic0',
      name: 'synthetic0',
      mac: '00:00:00:00:00:01',
      addresses: [{family: 'ipv4', address: '192.0.2.10', prefix: 24}],
      virtual: false,
      state: 'up',
      speedMbps: 1000,
    },
  ],
  capabilities: {
    cpu: {status: 'supported'},
    memory: {status: 'supported'},
    swap: {status: 'supported'},
    gpus: {status: 'supported'},
    disks: {status: 'supported'},
    network: {status: 'supported'},
    processes: {status: 'supported'},
  },
};

/** Creates the mock-mode sampler. Values are synthetic by construction. */
export function createSyntheticMetricsSampler(
  identity: MetricsIdentity,
  options: SyntheticMetricsOptions,
): MetricsSampler {
  const now = options.now ?? Date.now;
  const rand = options.rand ?? Math.random;
  const sequence = options.sequence;
  let version = 0;

  const inventory = (): Inventory => {
    if (version === 0) version = 1;
    return structuredClone(SYNTHETIC_INVENTORY);
  };

  return {
    next(): MetricsRecord {
      const timestamp = now();
      const cpuWave = Math.sin(((timestamp % CPU_PERIOD_MS) / CPU_PERIOD_MS) * Math.PI * 2);
      const memoryWave =
        (Math.sin(((timestamp % MEMORY_PERIOD_MS) / MEMORY_PERIOD_MS) * Math.PI * 2) + 1) / 2;
      const jitter = rand;
      if (version === 0) version = 1;
      return {
        collectorId: identity.collectorId,
        runId: identity.runId,
        bootId: identity.bootId,
        sequence: sequence.next(),
        capturedAt: new Date(Number.isFinite(timestamp) ? timestamp : Date.now()).toISOString(),
        kind: 'metrics',
        payload: {
          cpu: {
            status: 'ok',
            usagePercent: percent(18 + 22 * cpuWave + (jitter() * 4 - 2)),
            perCore: Array.from({length: SYNTHETIC_CORES}, (_value, index) =>
              percent(18 + 22 * Math.sin(cpuWave + index / 3) + (jitter() * 3 - 1.5)),
            ),
            temperatureC: Math.round(52 + 6 * memoryWave),
            powerWatts: Math.round(70 + 30 * cpuWave),
            iowaitPercent: percent(0.8 + 0.4 * Math.abs(cpuWave)),
            pressureSomePercent: percent(2 + 3 * Math.abs(cpuWave)),
          },
          memory: {
            ...usage(0.35 + 0.25 * memoryWave + (jitter() * 0.01 - 0.005), SYNTHETIC_MEMORY_BYTES),
            pressureSomePercent: percent(1 + 2 * memoryWave),
            pressureFullPercent: percent(0.2 + 0.3 * memoryWave),
          },
          swap: usage(0.1 + 0.05 * memoryWave, SYNTHETIC_SWAP_BYTES),
          gpus: {
            status: 'ok',
            devices: [
              {
                id: 'gpu:SYNTHETIC-0',
                status: 'ok',
                utilizationPercent: percent(25 + 30 * cpuWave + jitter() * 3),
                memoryUsedBytes: Math.round(
                  clamp(0.4 + 0.2 * memoryWave + jitter() * 0.01, 0, 1) * SYNTHETIC_GPU_MEMORY_BYTES,
                ),
                memoryTotalBytes: SYNTHETIC_GPU_MEMORY_BYTES,
                temperatureC: Math.round(45 + 8 * memoryWave),
                powerWatts: Math.round(90 + 30 * cpuWave),
                // Register-read sensors run hotter than the reported core.
                junctionTemperatureC: Math.round((57 + 10 * memoryWave + jitter()) * 10) / 10,
                vramTemperatureC: Math.round((60 + 9 * memoryWave + jitter()) * 10) / 10,
              },
            ],
          },
          disks: {
            status: 'ok',
            filesystems: [
              {
                id: 'fs:/',
                status: 'ok',
                usedBytes: Math.round((0.4 + 0.1 * memoryWave) * SYNTHETIC_FS_BYTES),
                totalBytes: SYNTHETIC_FS_BYTES,
                inodesTotal: 30_000_000,
                inodesUsed: Math.round(12_000_000 + 1_000_000 * memoryWave),
              },
            ],
            devices: [
              {
                id: 'disk:synthetic0',
                status: 'ok',
                readBytesPerSec: Math.round(1_000_000 + 500_000 * memoryWave),
                writeBytesPerSec: Math.round(400_000 + 200_000 * memoryWave),
                readLatencyMs: Math.round((0.3 + 0.2 * memoryWave) * 1000) / 1000,
                writeLatencyMs: Math.round((0.7 + 0.3 * memoryWave) * 1000) / 1000,
                ioUtilizationPercent: percent(8 + 12 * Math.abs(cpuWave)),
              },
            ],
            pressureSomePercent: percent(1.5 + 2 * Math.abs(cpuWave)),
            pressureFullPercent: percent(0.4 + 0.6 * Math.abs(cpuWave)),
          },
          network: {
            status: 'ok',
            interfaces: [
              {
                id: 'net:synthetic0',
                status: 'ok',
                rxBytesPerSec: Math.round(900_000 + 300_000 * cpuWave),
                txBytesPerSec: Math.round(120_000 + 40_000 * cpuWave),
              },
            ],
          },
        },
      };
    },
    inventory,
    inventoryVersion(): number {
      return version;
    },
    failures(): CollectionFailure[] {
      return [];
    },
  };
}
