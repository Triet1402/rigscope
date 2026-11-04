/**
 * Pure presentation helpers for the resource machine/fleet views and the
 * Connections surface.
 *
 * All logic is derived from the published contracts: the machine's
 * `metrics` and `inventory` when present. Missing or unavailable readings
 * keep an explanatory state; they are never coerced into zero. This module
 * owns no React and no fetch, so the same functions are the test seam for
 * all three views.
 */

import type {
  CapabilityState,
  CollectionFailure,
  GpuInventory,
  InterfaceAddress,
  Inventory,
  MachineState,
  MetricFamily,
  MetricsPayload,
  StatusMessage,
} from "../contracts";
import {
  formatBytes,
  freshnessThresholdFor,
  stateFreshness,
  type Freshness,
} from "../live/presentation";

// ---------------------------------------------------------------------------
// Small shared shapes
// ---------------------------------------------------------------------------

export type ResourceValue<T> =
  | { state: "value"; value: T; stale: boolean }
  | { state: "unavailable"; reason: string }
  | { state: "absent"; reason: string };

export type FamilyState =
  | { state: "available" }
  | { state: "unavailable"; reason: string }
  | { state: "absent"; reason: string };

export type ByteUsage = { usedBytes: number; totalBytes: number };

function value<T>(next: T, stale: boolean): ResourceValue<T> {
  return {state: "value", value: next, stale};
}

function unavailable<T>(reason: string): ResourceValue<T> {
  return {state: "unavailable", reason};
}

function absent<T>(reason: string): ResourceValue<T> {
  return {state: "absent", reason};
}

/** Human name: explicit display name first, then hostname, then id. */
export function machineDisplayName(machine: MachineState): string {
  const display = machine.displayName?.trim();
  if (display) return display;
  const hostname = machine.hostname?.trim();
  if (hostname) return hostname;
  return machine.machineId;
}

/** Tags already attached to the machine; never invents or edits metadata. */
export function machineTags(machine: MachineState): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const raw of machine.tags ?? []) {
    if (typeof raw !== "string") continue;
    const tag = raw.trim();
    if (tag === "" || seen.has(tag.toLowerCase())) continue;
    seen.add(tag.toLowerCase());
    out.push(tag);
    if (out.length >= 32) break;
  }
  return out;
}

function searchHaystack(machine: MachineState): string {
  return [
    machineDisplayName(machine),
    machine.hostname,
    machine.machineId,
    machine.collectorId,
    ...machineTags(machine),
  ]
    .filter((part): part is string => typeof part === "string" && part.trim() !== "")
    .join(" ")
    .toLowerCase();
}

/**
 * Authorized fleet scope stays exactly the array from `useLive`; this only
 * narrows it. Every whitespace-separated token must appear in the machine's
 * name, id or tags, so "rig gpu" finds a machine tagged `gpu` named `rig`.
 */
export function filterMachines(
  machines: readonly MachineState[],
  query: string,
): MachineState[] {
  const tokens = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (tokens.length === 0) return [...machines];
  return machines.filter((machine) => {
    const haystack = searchHaystack(machine);
    return tokens.every((token) => haystack.includes(token));
  });
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

export function formatRate(bytesPerSecond: number | null | undefined): string {
  if (bytesPerSecond === null || bytesPerSecond === undefined || !Number.isFinite(bytesPerSecond)) {
    return "—";
  }
  return `${formatBytes(bytesPerSecond)}/s`;
}

export function formatTemperature(celsius: number | null | undefined): string {
  if (celsius === null || celsius === undefined || !Number.isFinite(celsius)) return "—";
  return `${Math.round(celsius)} °C`;
}

export function formatPower(watts: number | null | undefined): string {
  if (watts === null || watts === undefined || !Number.isFinite(watts)) return "—";
  return `${Math.round(watts)} W`;
}

/** Device latency keeps sub-millisecond detail instead of rounding it away. */
export function formatLatency(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms) || ms < 0) return "—";
  return `${ms < 10 ? ms.toFixed(2) : ms.toFixed(1)} ms`;
}

export function formatCount(count: number | null | undefined): string {
  if (count === null || count === undefined || !Number.isFinite(count)) return "—";
  return Math.round(count).toLocaleString();
}

export function formatSpeedMbps(speed: number | null | undefined): string {
  if (speed === null || speed === undefined || !Number.isFinite(speed)) return "—";
  return `${Math.round(speed)} Mb/s`;
}

// ---------------------------------------------------------------------------
// Machine resources
// ---------------------------------------------------------------------------

export type GpuCardView = {
  id: string;
  name: string;
  vendor: GpuInventory["vendor"];
  pciBusId: string | null;
  telemetry: "supported" | "unsupported";
  state: "ok" | "unavailable" | "unsupported";
  reason: string | null;
  utilizationPercent: number | null;
  memoryUsedBytes: number | null;
  memoryTotalBytes: number | null;
  temperatureC: number | null;
  powerWatts: number | null;
  stale: boolean;
};

/**
 * True when at least one telemetry-supported GPU has a current reading and at
 * least one other supported GPU does not. Unsupported management adapters do
 * not make a machine partially degraded.
 */
export function gpuPartialFailure(cards: readonly GpuCardView[]): boolean {
  const supported = cards.filter((card) => card.telemetry === "supported");
  return supported.some((card) => card.state === "ok")
    && supported.some((card) => card.state !== "ok");
}

export type FilesystemView = {
  id: string;
  mountPoint: string;
  device: string;
  fsType: string;
  totalBytes: number | null;
  usedBytes: number | null;
  /** Inode counts are optional; older collectors omit them without implying zero. */
  inodesUsed: number | null;
  inodesTotal: number | null;
  state: "ok" | "unavailable" | "inventory";
  reason: string | null;
  stale: boolean;
};

export type DiskView = {
  id: string;
  name: string;
  model: string | null;
  rotational: boolean | null;
  sizeBytes: number | null;
  state: "ok" | "unavailable" | "inventory";
  reason: string | null;
  readBytesPerSec: number | null;
  writeBytesPerSec: number | null;
  /** Optional device saturation readings; null when this collector omits them. */
  readLatencyMs: number | null;
  writeLatencyMs: number | null;
  ioUtilizationPercent: number | null;
  stale: boolean;
};

export type NetworkView = {
  id: string;
  name: string;
  mac: string | null;
  addresses: InterfaceAddress[];
  virtual: boolean;
  linkState: "up" | "down" | "unknown";
  speedMbps: number | null;
  state: "ok" | "unavailable" | "inventory";
  reason: string | null;
  rxBytesPerSec: number | null;
  txBytesPerSec: number | null;
  stale: boolean;
};

/**
 * Optional bottleneck readings. Each value distinguishes a real measurement,
 * a family failure, a field this collector omitted ("not reported for this
 * sample"), and "no sample yet". Omission is never rendered as zero.
 */
export type BottleneckView = {
  cpuTemperature: ResourceValue<number>;
  cpuPower: ResourceValue<number>;
  cpuIowait: ResourceValue<number>;
  cpuPressure: ResourceValue<number>;
  memoryPressureSome: ResourceValue<number>;
  memoryPressureFull: ResourceValue<number>;
  ioPressureSome: ResourceValue<number>;
  ioPressureFull: ResourceValue<number>;
};

export type MachineResourceView = {
  displayName: string;
  tags: string[];
  freshness: Freshness;
  thresholdMs: number;
  stale: boolean;
  cpu: {
    total: ResourceValue<number>;
    perCore: ResourceValue<(number | null)[]>;
  };
  memory: ResourceValue<ByteUsage>;
  swap: ResourceValue<ByteUsage>;
  bottlenecks: BottleneckView;
  gpus: {family: FamilyState; cards: GpuCardView[]; partial: boolean};
  filesystems: {family: FamilyState; items: FilesystemView[]};
  disks: {family: FamilyState; items: DiskView[]};
  network: {family: FamilyState; items: NetworkView[]};
};

const NOT_REPORTED = "The collector has not reported this metric family to this hub.";

function payloadFor(machine: MachineState): MetricsPayload | null {
  return machine.metrics?.payload ?? null;
}

// ---------------------------------------------------------------------------
// Optional bottleneck readings
// ---------------------------------------------------------------------------

/**
 * Bottleneck fields are optional additions on existing successful metric
 * objects. This build reads them structurally so it compiles against a
 * contracts checkout that has not yet received the shared telemetry fields;
 * an omitted field is unavailable, never zero.
 */
function optionalNumber(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function optionalNonNegative(value: unknown): number | null {
  const reading = optionalNumber(value);
  return reading !== null && reading >= 0 ? reading : null;
}

function optionalReading(
  source: unknown,
  field: string,
  stale: boolean,
  absentReason: string,
  nonNegative = true,
): ResourceValue<number> {
  if (source === null || typeof source !== "object") return absent(absentReason);
  const record = source as Record<string, unknown>;
  if (record.status === "unavailable") {
    const reason = typeof record.reason === "string" ? record.reason.trim() : "";
    return unavailable(reason || "This metric family is unavailable.");
  }
  if (record.status !== "ok") return absent(absentReason);
  const reading = (nonNegative ? optionalNonNegative : optionalNumber)(record[field]);
  if (reading === null) return unavailable("Not reported for this sample.");
  return value(reading, stale);
}

function bottleneckView(machine: MachineState, stale: boolean): BottleneckView {
  const cpu = machine.metrics?.payload.cpu;
  const memory = machine.metrics?.payload.memory;
  const disks = machine.metrics?.payload.disks;
  return {
    cpuTemperature: optionalReading(
      cpu,
      "temperatureC",
      stale,
      "No CPU reading has been received yet.",
      false,
    ),
    cpuPower: optionalReading(cpu, "powerWatts", stale, "No CPU reading has been received yet."),
    cpuIowait: optionalReading(
      cpu,
      "iowaitPercent",
      stale,
      "No CPU reading has been received yet.",
    ),
    cpuPressure: optionalReading(
      cpu,
      "pressureSomePercent",
      stale,
      "No CPU reading has been received yet.",
    ),
    memoryPressureSome: optionalReading(
      memory,
      "pressureSomePercent",
      stale,
      "No memory reading has been received yet.",
    ),
    memoryPressureFull: optionalReading(
      memory,
      "pressureFullPercent",
      stale,
      "No memory reading has been received yet.",
    ),
    ioPressureSome: optionalReading(
      disks,
      "pressureSomePercent",
      stale,
      "No disk reading has been received yet.",
    ),
    ioPressureFull: optionalReading(
      disks,
      "pressureFullPercent",
      stale,
      "No disk reading has been received yet.",
    ),
  };
}

function cpuTotal(machine: MachineState, stale: boolean): ResourceValue<number> {
  const cpu = machine.metrics?.payload.cpu;
  if (cpu === undefined) {
    return absent("No CPU reading has been received yet.");
  }
  if (cpu.status === "ok") return value(cpu.usagePercent, stale);
  return unavailable(cpu.reason);
}

function cpuPerCore(machine: MachineState, stale: boolean): ResourceValue<(number | null)[]> {
  const cpu = machine.metrics?.payload.cpu;
  if (cpu === undefined) return absent(`No CPU reading has been received yet; per-core values are unknown.`);
  if (cpu.status === "ok") return value([...cpu.perCore], stale);
  return unavailable(cpu.reason);
}

function memoryUsage(machine: MachineState, stale: boolean): ResourceValue<ByteUsage> {
  const memory = machine.metrics?.payload.memory;
  if (memory === undefined) {
    return absent("No memory reading has been received yet.");
  }
  if (memory.status === "ok") {
    return value({usedBytes: memory.usedBytes, totalBytes: memory.totalBytes}, stale);
  }
  return unavailable(memory.reason);
}

function swapUsage(machine: MachineState, stale: boolean): ResourceValue<ByteUsage> {
  const swap = machine.metrics?.payload.swap;
  if (swap === undefined) {
    return absent("No swap reading has been received yet.");
  }
  if (swap.status === "ok") {
    return value({usedBytes: swap.usedBytes, totalBytes: swap.totalBytes}, stale);
  }
  return unavailable(swap.reason);
}

function gpuFamily(machine: MachineState): FamilyState {
  const payload = payloadFor(machine);
  if (payload !== null) {
    if (payload.gpus.status === "ok") return {state: "available"};
    return {state: "unavailable", reason: payload.gpus.reason};
  }
  if ((machine.inventory?.gpus.length ?? 0) > 0) {
    return {state: "absent", reason: "No GPU readings have been received yet."};
  }
  return {state: "absent", reason: NOT_REPORTED + " GPU telemetry is unknown until inventory or a reading arrives."};
}

function gpuCards(machine: MachineState, stale: boolean): GpuCardView[] {
  const inventory = machine.inventory?.gpus ?? [];
  const payload = payloadFor(machine);
  const metricDevices = payload?.gpus.status === "ok" ? payload.gpus.devices : [];
  const metricUnavailable = payload?.gpus.status === "unavailable" ? payload.gpus.reason : null;
  const readings = new Map(metricDevices.map((device) => [device.id, device]));
  const inventoryById = new Map(inventory.map((gpu) => [gpu.id, gpu]));
  const ids: string[] = [];
  for (const gpu of inventory) ids.push(gpu.id);
  for (const device of metricDevices) {
    if (!inventoryById.has(device.id)) ids.push(device.id);
  }

  const cards: GpuCardView[] = [];
  for (const id of ids) {
    const gpu = inventoryById.get(id);
    const reading = readings.get(id);
    const base = {
      id,
      name: gpu?.name ?? id.replace(/^gpu:/, ""),
      vendor: gpu?.vendor ?? ("other" as const),
      pciBusId: gpu?.pciBusId ?? null,
      telemetry: gpu?.telemetry ?? ("supported" as const),
      stale,
    };
    if (reading?.status === "ok") {
      cards.push({
        ...base,
        state: "ok",
        reason: null,
        utilizationPercent: reading.utilizationPercent,
        memoryUsedBytes: reading.memoryUsedBytes,
        memoryTotalBytes: reading.memoryTotalBytes ?? gpu?.memoryTotalBytes ?? null,
        temperatureC: reading.temperatureC,
        powerWatts: reading.powerWatts,
      });
      continue;
    }
    if (reading?.status === "unavailable") {
      cards.push({
        ...base,
        state: "unavailable",
        reason: reading.reason,
        utilizationPercent: null,
        memoryUsedBytes: null,
        memoryTotalBytes: gpu?.memoryTotalBytes ?? null,
        temperatureC: null,
        powerWatts: null,
      });
      continue;
    }
    if (gpu?.telemetry === "unsupported") {
      cards.push({
        ...base,
        state: "unsupported",
        reason: gpu.reason ?? "No supported telemetry for this adapter.",
        utilizationPercent: null,
        memoryUsedBytes: null,
        memoryTotalBytes: null,
        temperatureC: null,
        powerWatts: null,
      });
      continue;
    }
    cards.push({
      ...base,
      state: "unavailable",
      reason: metricUnavailable ?? "No current reading for this device.",
      utilizationPercent: null,
      memoryUsedBytes: null,
      memoryTotalBytes: gpu?.memoryTotalBytes ?? null,
      temperatureC: null,
      powerWatts: null,
    });
  }
  return cards;
}

function diskFamily(machine: MachineState): FamilyState {
  const payload = payloadFor(machine);
  if (payload !== null) {
    if (payload.disks.status === "ok") return {state: "available"};
    return {state: "unavailable", reason: payload.disks.reason};
  }
  return {state: "absent", reason: NOT_REPORTED};
}

function filesystemViews(machine: MachineState, stale: boolean): FilesystemView[] {
  const inventory = machine.inventory?.filesystems ?? [];
  const payload = payloadFor(machine);
  const metricFilesystems = payload?.disks.status === "ok" ? payload.disks.filesystems : [];
  const familyUnavailable = payload?.disks.status === "unavailable" ? payload.disks.reason : null;
  const metricById = new Map(metricFilesystems.map((filesystem) => [filesystem.id, filesystem]));
  const inventoryById = new Map(inventory.map((filesystem) => [filesystem.id, filesystem]));
  const ids: string[] = [];
  for (const filesystem of inventory) ids.push(filesystem.id);
  for (const filesystem of metricFilesystems) {
    if (!inventoryById.has(filesystem.id)) ids.push(filesystem.id);
  }

  return ids.map((id): FilesystemView => {
    const known = inventoryById.get(id);
    const reading = metricById.get(id);
    const base = {
      id,
      mountPoint: known?.mountPoint ?? id.replace(/^fs:/, ""),
      device: known?.device ?? "",
      fsType: known?.fsType ?? "unknown",
      totalBytes: known?.totalBytes ?? null,
      inodesUsed: null,
      inodesTotal: null,
      stale,
    };
    if (reading?.status === "ok") {
      const optional = reading as unknown as Record<string, unknown>;
      return {
        ...base,
        usedBytes: reading.usedBytes,
        totalBytes: reading.totalBytes,
        inodesUsed: optionalNonNegative(optional.inodesUsed),
        inodesTotal: optionalNonNegative(optional.inodesTotal),
        state: "ok",
        reason: null,
      };
    }
    if (reading?.status === "unavailable") {
      return {
        ...base,
        usedBytes: null,
        state: "unavailable",
        reason: reading.reason,
      };
    }
    if (familyUnavailable !== null) {
      return { ...base, usedBytes: null, state: "unavailable", reason: familyUnavailable };
    }
    return {
      ...base,
      usedBytes: null,
      state: "inventory",
      reason: payload === null
        ? NOT_REPORTED
        : "No current usage reading for this filesystem.",
    };
  });
}

function diskViews(machine: MachineState, stale: boolean): DiskView[] {
  const inventory = machine.inventory?.disks ?? [];
  const payload = payloadFor(machine);
  const metricDevices = payload?.disks.status === "ok" ? payload.disks.devices : [];
  const familyUnavailable = payload?.disks.status === "unavailable" ? payload.disks.reason : null;
  const metricById = new Map(metricDevices.map((device) => [device.id, device]));
  const inventoryById = new Map(inventory.map((device) => [device.id, device]));
  const ids: string[] = [];
  for (const device of inventory) ids.push(device.id);
  for (const device of metricDevices) {
    if (!inventoryById.has(device.id)) ids.push(device.id);
  }

  return ids.map((id): DiskView => {
    const known = inventoryById.get(id);
    const reading = metricById.get(id);
    const base = {
      id,
      name: known?.name ?? id.replace(/^disk:/, ""),
      model: known?.model ?? null,
      rotational: known?.rotational ?? null,
      sizeBytes: known?.sizeBytes ?? null,
      readLatencyMs: null,
      writeLatencyMs: null,
      ioUtilizationPercent: null,
      stale,
    };
    if (reading?.status === "ok") {
      const optional = reading as unknown as Record<string, unknown>;
      return {
        ...base,
        state: "ok",
        reason: null,
        readBytesPerSec: reading.readBytesPerSec,
        writeBytesPerSec: reading.writeBytesPerSec,
        readLatencyMs: optionalNonNegative(optional.readLatencyMs),
        writeLatencyMs: optionalNonNegative(optional.writeLatencyMs),
        ioUtilizationPercent: optionalNonNegative(optional.ioUtilizationPercent),
      };
    }
    if (reading?.status === "unavailable") {
      return {
        ...base,
        state: "unavailable",
        reason: reading.reason,
        readBytesPerSec: null,
        writeBytesPerSec: null,
      };
    }
    if (familyUnavailable !== null) {
      return {
        ...base,
        state: "unavailable",
        reason: familyUnavailable,
        readBytesPerSec: null,
        writeBytesPerSec: null,
      };
    }
    return {
      ...base,
      state: "inventory",
      reason: payload === null
        ? NOT_REPORTED
        : "No current I/O reading for this device.",
      readBytesPerSec: null,
      writeBytesPerSec: null,
    };
  });
}

function networkFamily(machine: MachineState): FamilyState {
  const payload = payloadFor(machine);
  if (payload !== null) {
    if (payload.network.status === "ok") return {state: "available"};
    return {state: "unavailable", reason: payload.network.reason};
  }
  return {state: "absent", reason: NOT_REPORTED};
}

function networkViews(machine: MachineState, stale: boolean): NetworkView[] {
  const inventory = machine.inventory?.network ?? [];
  const payload = payloadFor(machine);
  const metricInterfaces = payload?.network.status === "ok" ? payload.network.interfaces : [];
  const familyUnavailable = payload?.network.status === "unavailable" ? payload.network.reason : null;
  const metricById = new Map(metricInterfaces.map((iface) => [iface.id, iface]));
  const inventoryById = new Map(inventory.map((iface) => [iface.id, iface]));
  const ids: string[] = [];
  for (const iface of inventory) ids.push(iface.id);
  for (const iface of metricInterfaces) {
    if (!inventoryById.has(iface.id)) ids.push(iface.id);
  }

  return ids.map((id): NetworkView => {
    const known = inventoryById.get(id);
    const reading = metricById.get(id);
    const base = {
      id,
      name: known?.name ?? id.replace(/^net:/, ""),
      mac: known?.mac ?? null,
      addresses: known?.addresses ?? [],
      virtual: known?.virtual ?? false,
      linkState: known?.state ?? ("unknown" as const),
      speedMbps: known?.speedMbps ?? null,
      stale,
    };
    if (reading?.status === "ok") {
      return {
        ...base,
        state: "ok",
        reason: null,
        rxBytesPerSec: reading.rxBytesPerSec,
        txBytesPerSec: reading.txBytesPerSec,
      };
    }
    if (reading?.status === "unavailable") {
      return {
        ...base,
        state: "unavailable",
        reason: reading.reason,
        rxBytesPerSec: null,
        txBytesPerSec: null,
      };
    }
    if (familyUnavailable !== null) {
      return {
        ...base,
        state: "unavailable",
        reason: familyUnavailable,
        rxBytesPerSec: null,
        txBytesPerSec: null,
      };
    }
    return {
      ...base,
      state: "inventory",
      reason: payload === null
        ? NOT_REPORTED
        : "No current rate reading for this interface.",
      rxBytesPerSec: null,
      txBytesPerSec: null,
    };
  });
}

/**
 * One deterministic view of every known resource on a machine. The UI never
 * has to re-interpret the contracts: each reading is a live value, a
 * labeled last-known value, an unavailable reason, or an honest "no
 * reading yet" state.
 */
export function machineResourceView(machine: MachineState, now: number): MachineResourceView {
  const freshness = stateFreshness(machine, now);
  const stale = freshness === "stale";
  const gpuCardsForMachine = gpuCards(machine, stale);
  return {
    displayName: machineDisplayName(machine),
    tags: machineTags(machine),
    freshness,
    thresholdMs: freshnessThresholdFor(machine.expectedIntervalMs),
    stale,
    cpu: {
      total: cpuTotal(machine, stale),
      perCore: cpuPerCore(machine, stale),
    },
    memory: memoryUsage(machine, stale),
    swap: swapUsage(machine, stale),
    bottlenecks: bottleneckView(machine, stale),
    gpus: {
      family: gpuFamily(machine),
      cards: gpuCardsForMachine,
      partial: gpuPartialFailure(gpuCardsForMachine),
    },
    filesystems: {
      family: diskFamily(machine),
      items: filesystemViews(machine, stale),
    },
    disks: {
      family: diskFamily(machine),
      items: diskViews(machine, stale),
    },
    network: {
      family: networkFamily(machine),
      items: networkViews(machine, stale),
    },
  };
}

// ---------------------------------------------------------------------------
// Fleet summaries
// ---------------------------------------------------------------------------

export type FleetResourceSummary = {
  cpu: ResourceValue<number>;
  memory: ResourceValue<ByteUsage>;
  gpu: {
    count: number;
    supportedCount: number;
    okCount: number;
    partial: boolean;
    maxUtilizationPercent: number | null;
    memoryUsedBytes: number | null;
    memoryTotalBytes: number | null;
    stale: boolean;
    reason: string | null;
  } | null;
  disk: {maxUsedPercent: number | null; stale: boolean; reason: string | null} | null;
  network: {rxBytesPerSec: number; txBytesPerSec: number; stale: boolean; reason: string | null} | null;
};

function sumIfComplete(values: readonly (number | null)[]): number | null {
  if (values.length === 0 || values.some((item) => item === null)) return null;
  let total = 0;
  for (const item of values) total += item as number;
  return total;
}

/** Compact, honest fleet row values. Null means no data for that resource. */
export function fleetResourceSummary(machine: MachineState, now: number): FleetResourceSummary {
  const view = machineResourceView(machine, now);
  const cards = view.gpus.cards;
  const gpu = cards.length === 0
    ? null
    : (() => {
        const supported = cards.filter((card) => card.telemetry === "supported");
        const ok = supported.filter((card) => card.state === "ok");
        const utilizations = ok
          .map((card) => card.utilizationPercent)
          .filter((item): item is number => item !== null);
        const memoryUsed = sumIfComplete(supported.map((card) => card.memoryUsedBytes));
        const memoryTotal = sumIfComplete(supported.map((card) => card.memoryTotalBytes));
        const reason = ok.length === 0
          ? (view.gpus.family.state === "unavailable" || view.gpus.family.state === "absent"
              ? view.gpus.family.reason
              : supported.length === 0
                ? "No telemetry-supported GPU was reported."
                : "No current GPU reading.")
          : null;
        return {
          count: cards.length,
          supportedCount: supported.length,
          okCount: ok.length,
          partial: gpuPartialFailure(cards),
          maxUtilizationPercent: utilizations.length === 0 ? null : Math.max(...utilizations),
          memoryUsedBytes: memoryUsed,
          memoryTotalBytes: memoryTotal,
          stale: view.stale,
          reason,
        };
      })();

  const capacity = view.filesystems.items
    .filter((filesystem): filesystem is FilesystemView & {usedBytes: number; totalBytes: number} =>
      filesystem.state === "ok" && filesystem.usedBytes !== null && filesystem.totalBytes !== null && filesystem.totalBytes > 0,
    )
    .map((filesystem) => (filesystem.usedBytes / filesystem.totalBytes) * 100);
  const disk = view.filesystems.items.length === 0
    ? null
    : {
        maxUsedPercent: capacity.length === 0 ? null : Math.max(...capacity),
        stale: view.stale,
        reason: capacity.length === 0
          ? (view.filesystems.family.state === "available"
              ? "No current filesystem usage reading."
              : view.filesystems.family.reason)
          : null,
      };

  const liveInterfaces = view.network.items.filter(
    (iface): iface is NetworkView & {rxBytesPerSec: number; txBytesPerSec: number} =>
      iface.state === "ok" && iface.rxBytesPerSec !== null && iface.txBytesPerSec !== null,
  );
  const network = view.network.items.length === 0
    ? null
    : {
        rxBytesPerSec: liveInterfaces.reduce((sum, iface) => sum + iface.rxBytesPerSec, 0),
        txBytesPerSec: liveInterfaces.reduce((sum, iface) => sum + iface.txBytesPerSec, 0),
        stale: view.stale,
        reason: liveInterfaces.length === 0
          ? (view.network.family.state === "available"
              ? "No current interface rate reading."
              : view.network.family.reason)
          : null,
      };

  return {
    cpu: view.cpu.total,
    memory: view.memory,
    gpu,
    disk,
    network,
  };
}

// ---------------------------------------------------------------------------
// Connections (safe collector-facing facts only)
// ---------------------------------------------------------------------------

export type CapabilityKey = MetricFamily | "processes";
export type CapabilityView = {
  key: CapabilityKey;
  label: string;
  status: CapabilityState["status"];
  reason: string | null;
};

const CAPABILITY_ORDER: readonly CapabilityKey[] = [
  "cpu",
  "memory",
  "swap",
  "gpus",
  "disks",
  "network",
  "processes",
];

const CAPABILITY_LABEL: Record<CapabilityKey, string> = {
  cpu: "CPU",
  memory: "Memory",
  swap: "Swap",
  gpus: "GPUs",
  disks: "Disks",
  network: "Network",
  processes: "Processes",
};

export type DeliveryView = StatusMessage["delivery"];

export type CollectorConnectionView = {
  machineId: string;
  name: string;
  collectorId: string;
  connected: boolean;
  cadenceMs: number | null;
  thresholdMs: number;
  lastReceivedAt: string | null;
  freshness: Freshness;
  /** Known delivery fields only; no URLs, tokens or destinations. */
  delivery: DeliveryView | null;
  deliveryCapturedAt: string | null;
  failures: CollectionFailure[];
  capabilities: CapabilityView[] | null;
};

function capabilityViews(inventory: Inventory | undefined): CapabilityView[] | null {
  if (inventory === undefined) return null;
  return CAPABILITY_ORDER.map((key) => {
    const state = inventory.capabilities[key];
    return {
      key,
      label: CAPABILITY_LABEL[key],
      status: state.status,
      reason: state.reason ?? null,
    };
  });
}

/**
 * Per-machine collector connection facts for this hub. Deliberately excludes
 * anything about other destinations: no URLs, tokens, headers or raw
 * diagnostics enter this view.
 */
export function collectorConnectionViews(
  machines: readonly MachineState[],
  now: number,
): CollectorConnectionView[] {
  return machines.map((machine) => ({
    machineId: machine.machineId,
    name: machineDisplayName(machine),
    collectorId: machine.collectorId,
    connected: machine.connected,
    cadenceMs: machine.expectedIntervalMs ?? null,
    thresholdMs: freshnessThresholdFor(machine.expectedIntervalMs),
    lastReceivedAt: machine.lastReceivedAt,
    freshness: stateFreshness(machine, now),
    delivery: machine.delivery?.delivery ?? null,
    deliveryCapturedAt: machine.delivery?.capturedAt ?? null,
    failures: machine.delivery?.failures ?? [],
    capabilities: capabilityViews(machine.inventory),
  }));
}
