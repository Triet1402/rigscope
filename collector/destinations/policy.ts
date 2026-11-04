import type { Inventory, MetricFamily, MetricsPayload, MetricsRecord } from '../../src/contracts';
import type { DestinationEndpoint } from '../config';

/**
 * Nominal effective delivery cadence for one destination, in milliseconds.
 *
 * A destination interval can only slow the shared sampling cadence down, and
 * delivery happens on sample ticks, so the achievable schedule is the interval
 * quantized up to the next multiple of the base sampling interval:
 * `ceil(max(baseIntervalMs, destinationIntervalMs ?? baseIntervalMs) / baseIntervalMs) * baseIntervalMs`.
 *
 * This describes nominal sample ticks, not an extra delivery cutoff: the filter
 * still uses the configured interval so rounding does not defer an eligible
 * tick that arrives fractionally early.
 * With the collector bounds (base and destination at most
 * 3_600_000 ms), the quantized result is always below 7_200_000 ms, the
 * contract's hello/snapshot interval bound.
 */
export function effectiveIntervalMs(baseIntervalMs: number, destinationIntervalMs: number | undefined): number {
  const interval = Math.max(baseIntervalMs, destinationIntervalMs ?? baseIntervalMs);
  return Math.ceil(interval / baseIntervalMs) * baseIntervalMs;
}

/** Local sharing policy for one destination. Fixed at process start. */
export type SharingPolicy = {
  cpu: boolean;
  memory: boolean;
  swap: boolean;
  gpus: boolean;
  disks: boolean;
  network: boolean;
  networkAddresses: boolean;
  processes: boolean;
  processArguments: boolean;
};

/** Configuration shape as supplied by configuration or stored state: any of the keys may be omitted. */
export type SharingInput = {[K in keyof SharingPolicy]?: boolean};

export const SHARING_KEYS = [
  'cpu',
  'memory',
  'swap',
  'gpus',
  'disks',
  'network',
  'networkAddresses',
  'processes',
  'processArguments',
] as const satisfies readonly (keyof SharingPolicy)[];

const bool = (value: unknown, fallback: boolean): boolean => (typeof value === 'boolean' ? value : fallback);

/**
 * Fills defaults for a partially specified sharing object.
 *
 * Missing fields default to true, except:
 * - `swap` follows the memory decision (sharing memory is also a swap decision),
 * - `processArguments` defaults to false because command lines can carry
 *   secrets and are an explicit opt-in.
 */
export function normalizeSharing(raw: SharingInput | null | undefined): SharingPolicy {
  const memory = bool(raw?.memory, true);
  return {
    cpu: bool(raw?.cpu, true),
    memory,
    swap: bool(raw?.swap, memory),
    gpus: bool(raw?.gpus, true),
    disks: bool(raw?.disks, true),
    network: bool(raw?.network, true),
    networkAddresses: bool(raw?.networkAddresses, true),
    processes: bool(raw?.processes, true),
    processArguments: bool(raw?.processArguments, false),
  };
}

const WITHHELD_REASON = 'withheld by local sharing policy';

function withheld(): {status: 'unavailable'; reason: string} {
  return {status: 'unavailable', reason: WITHHELD_REASON};
}

/**
 * Applies only this destination's sharing policy to a metrics record, never
 * the delivery cadence.
 *
 * Fresh samples pass through `createMetricsFilter`, which adds its per-tick
 * cadence gate. Recovered pending samples use this one so queue sanitation
 * can reduce fields before anything leaves the collector but can never
 * cadence-filter retained measurements away.
 *
 * The input record is never mutated; the record comes back in a fresh
 * envelope and payload, with withheld families replaced by an `unavailable`
 * family.
 */
export function applySharing(
  record: MetricsRecord,
  sharing: SharingInput | SharingPolicy | null | undefined,
): MetricsRecord {
  const policy = normalizeSharing(sharing);
  const payload: MetricsPayload = {
    cpu: policy.cpu ? record.payload.cpu : withheld(),
    memory: policy.memory ? record.payload.memory : withheld(),
    swap: policy.swap ? record.payload.swap : withheld(),
    gpus: policy.gpus ? record.payload.gpus : withheld(),
    disks: policy.disks ? record.payload.disks : withheld(),
    network: policy.network ? record.payload.network : withheld(),
  };
  return {...record, payload};
}

/**
 * Destination filter for metrics records.
 *
 * The cadence rule uses the raw `max(baseIntervalMs, destinationIntervalMs)`
 * cutoff, not the quantized nominal schedule (see `effectiveIntervalMs`), so
 * rounding cannot defer an eligible tick that arrives fractionally early.
 */
export function createMetricsFilter(
  destination: DestinationEndpoint,
  baseIntervalMs: number,
): (record: MetricsRecord, monotonicMs: number) => MetricsRecord | null {
  let lastSent: number | undefined;
  const interval = Math.max(baseIntervalMs, destination.intervalMs ?? baseIntervalMs);
  return (record, monotonicMs) => {
    if (interval > baseIntervalMs && lastSent !== undefined && monotonicMs - lastSent < interval) return null;
    lastSent = monotonicMs;
    return applySharing(record, destination.sharing);
  };
}

/**
 * Applies a destination's family sharing policy to inventory. Withheld
 * families keep their capability entry but report `withheld` and carry no
 * resources. Address lists can also be withheld independently.
 */
export function filterInventory(inv: Inventory, sharing: SharingInput | SharingPolicy): Inventory {
  const policy = normalizeSharing(sharing);
  const capabilities = {...inv.capabilities};
  const families: MetricFamily[] = ['cpu', 'memory', 'swap', 'gpus', 'disks', 'network'];
  for (const family of families) {
    if (!policy[family]) {
      capabilities[family] = {status: 'withheld'};
    }
  }
  if (!policy.processes) capabilities.processes = {status: 'withheld'};

  const network = policy.network
    ? inv.network.map((iface) =>
        policy.networkAddresses ? {...iface, addresses: [...iface.addresses]} : {...iface, addresses: []},
      )
    : [];

  return {
    ...inv,
    cpu: policy.cpu ? { ...inv.cpu } : { model: null, logicalCores: 0 },
    memory: { totalBytes: policy.memory ? inv.memory.totalBytes : null,
      swapTotalBytes: policy.swap ? inv.memory.swapTotalBytes : null },
    gpus: policy.gpus ? inv.gpus.map((gpu) => ({...gpu})) : [],
    filesystems: policy.disks ? inv.filesystems.map((fs) => ({...fs})) : [],
    disks: policy.disks ? inv.disks.map((disk) => ({...disk})) : [],
    network,
    capabilities,
  };
}
