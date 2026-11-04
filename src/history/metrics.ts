/**
 * Presentation metadata for durable history metrics.
 *
 * This module is browser-safe: it imports only types from the hub storage
 * modules (those runtime modules use `node:sqlite` and must never enter the
 * client bundle). The metric catalog mirrors storage/series.ts; the hub's
 * `/api/history/metrics` response is used to confirm which metrics this hub
 * actually records.
 */

import type { MachineState } from "../contracts";
import type { HistorySeries } from "../hub/storage/history-query";
import type { MetricUnit } from "../hub/storage/series";

export type MetricFamily = "cpu" | "memory" | "swap" | "gpus" | "disks" | "network";

/**
 * Storage units this build can present. `milliseconds` arrives with the
 * bottleneck history contract; keeping it local means the UI still type-checks
 * against a hub/storage build that has not added the unit yet.
 */
export type HistoryMetricUnit = MetricUnit | "milliseconds";

export type HistoryMetricOption = {
  metric: string;
  label: string;
  shortLabel: string;
  unit: HistoryMetricUnit;
  family: MetricFamily;
  /** Index into the shared series palette; keeps CPU blue and memory green. */
  colorSlot: number;
  description: string;
};

export const HISTORY_METRICS: readonly HistoryMetricOption[] = [
  {
    metric: "cpu.usage",
    label: "CPU usage",
    shortLabel: "CPU",
    unit: "percent",
    family: "cpu",
    colorSlot: 0,
    description: "Total processor utilization",
  },
  {
    metric: "cpu.core",
    label: "Per-core CPU",
    shortLabel: "Per-core",
    unit: "percent",
    family: "cpu",
    colorSlot: 0,
    description: "Logical-core utilization; heatmap for large selections",
  },
  {
    metric: "cpu.temperature",
    label: "CPU temperature",
    shortLabel: "Temperature",
    unit: "celsius",
    family: "cpu",
    colorSlot: 3,
    description: "Identifiable CPU hwmon sensor temperature",
  },
  {
    metric: "cpu.power",
    label: "CPU package power",
    shortLabel: "Power",
    unit: "watts",
    family: "cpu",
    colorSlot: 4,
    description: "Top-level RAPL package power draw",
  },
  {
    metric: "cpu.iowait",
    label: "CPU I/O wait",
    shortLabel: "I/O wait",
    unit: "percent",
    family: "cpu",
    colorSlot: 2,
    description: "Processor time waiting on I/O",
  },
  {
    metric: "cpu.pressure",
    label: "CPU pressure",
    shortLabel: "Pressure",
    unit: "percent",
    family: "cpu",
    colorSlot: 4,
    description: "Linux PSI CPU stall pressure (avg10)",
  },
  {
    metric: "memory.used",
    label: "Memory used",
    shortLabel: "Memory",
    unit: "bytes",
    family: "memory",
    colorSlot: 1,
    description: "Physical memory in use",
  },
  {
    metric: "memory.usage",
    label: "Memory usage",
    shortLabel: "Memory %",
    unit: "percent",
    family: "memory",
    colorSlot: 1,
    description: "Physical memory used as a share of total",
  },
  {
    metric: "memory.pressure.some",
    label: "Memory pressure (some)",
    shortLabel: "Pressure some",
    unit: "percent",
    family: "memory",
    colorSlot: 2,
    description: "Linux PSI memory stall pressure, some tasks (avg10)",
  },
  {
    metric: "memory.pressure.full",
    label: "Memory pressure (full)",
    shortLabel: "Pressure full",
    unit: "percent",
    family: "memory",
    colorSlot: 4,
    description: "Linux PSI memory stall pressure, all tasks (avg10)",
  },
  {
    metric: "swap.used",
    label: "Swap used",
    shortLabel: "Swap",
    unit: "bytes",
    family: "swap",
    colorSlot: 2,
    description: "Swap space in use",
  },
  {
    metric: "gpu.utilization",
    label: "GPU utilization",
    shortLabel: "Utilization",
    unit: "percent",
    family: "gpus",
    colorSlot: 0,
    description: "Per-card graphics processor utilization",
  },
  {
    metric: "gpu.memory.used",
    label: "GPU memory used",
    shortLabel: "VRAM",
    unit: "bytes",
    family: "gpus",
    colorSlot: 1,
    description: "Per-card video memory in use",
  },
  {
    metric: "gpu.temperature",
    label: "GPU temperature",
    shortLabel: "Temperature",
    unit: "celsius",
    family: "gpus",
    colorSlot: 3,
    description: "Per-card temperature",
  },
  {
    metric: "gpu.power",
    label: "GPU power",
    shortLabel: "Power",
    unit: "watts",
    family: "gpus",
    colorSlot: 4,
    description: "Per-card power draw",
  },
  {
    metric: "fs.used",
    label: "Filesystem used",
    shortLabel: "Filesystem",
    unit: "bytes",
    family: "disks",
    colorSlot: 1,
    description: "Used space per mounted filesystem",
  },
  {
    metric: "fs.usage",
    label: "Filesystem usage",
    shortLabel: "Filesystem %",
    unit: "percent",
    family: "disks",
    colorSlot: 1,
    description: "Used capacity per mounted filesystem, derived from a positive total",
  },
  {
    metric: "fs.inodes.usage",
    label: "Inode usage",
    shortLabel: "Inodes %",
    unit: "percent",
    family: "disks",
    colorSlot: 2,
    description: "Used inodes per filesystem where the collector can read them",
  },
  {
    metric: "disk.read",
    label: "Disk read",
    shortLabel: "Read",
    unit: "bytes_per_second",
    family: "disks",
    colorSlot: 0,
    description: "Device read throughput",
  },
  {
    metric: "disk.write",
    label: "Disk write",
    shortLabel: "Write",
    unit: "bytes_per_second",
    family: "disks",
    colorSlot: 2,
    description: "Device write throughput",
  },
  {
    metric: "disk.read.latency",
    label: "Disk read latency",
    shortLabel: "Read latency",
    unit: "milliseconds",
    family: "disks",
    colorSlot: 0,
    description: "Average device read latency",
  },
  {
    metric: "disk.write.latency",
    label: "Disk write latency",
    shortLabel: "Write latency",
    unit: "milliseconds",
    family: "disks",
    colorSlot: 2,
    description: "Average device write latency",
  },
  {
    metric: "disk.utilization",
    label: "Disk utilization",
    shortLabel: "Utilization",
    unit: "percent",
    family: "disks",
    colorSlot: 3,
    description: "Share of time the device had I/O in flight",
  },
  {
    metric: "io.pressure.some",
    label: "I/O pressure (some)",
    shortLabel: "I/O some",
    unit: "percent",
    family: "disks",
    colorSlot: 3,
    description: "Linux PSI I/O stall pressure, some tasks (avg10)",
  },
  {
    metric: "io.pressure.full",
    label: "I/O pressure (full)",
    shortLabel: "I/O full",
    unit: "percent",
    family: "disks",
    colorSlot: 4,
    description: "Linux PSI I/O stall pressure, all tasks (avg10)",
  },
  {
    metric: "net.rx",
    label: "Network received",
    shortLabel: "Received",
    unit: "bytes_per_second",
    family: "network",
    colorSlot: 0,
    description: "Interface receive rate",
  },
  {
    metric: "net.tx",
    label: "Network sent",
    shortLabel: "Sent",
    unit: "bytes_per_second",
    family: "network",
    colorSlot: 2,
    description: "Interface transmit rate",
  },
] as const;

export type CapacityUnits = { memory: "bytes" | "percent"; filesystem: "bytes" | "percent" };

/** Unit switches change presentation only; queries retain both stored forms. */
export function visibleCapacityMetrics(metrics: readonly string[], units: CapacityUnits): string[] {
  return metrics.filter((metric) => {
    for (const [group, bytes, percent] of [
      ["memory", "memory.used", "memory.usage"],
      ["filesystem", "fs.used", "fs.usage"],
    ] as const) {
      if (metrics.includes(bytes) && metrics.includes(percent) && (metric === bytes || metric === percent)) {
        return metric === (units[group] === "percent" ? percent : bytes);
      }
    }
    return true;
  });
}

export const HISTORY_METRIC_GROUPS: ReadonlyArray<{ id: string; label: string; metrics: readonly string[] }> = [
  {
    id: "cpu", label: "CPU",
    metrics: ["cpu.usage", "cpu.core", "cpu.iowait", "cpu.pressure", "cpu.temperature", "cpu.power"],
  },
  {
    id: "memory", label: "Memory",
    metrics: ["memory.used", "memory.usage", "swap.used", "memory.pressure.some", "memory.pressure.full"],
  },
  {
    id: "gpu", label: "GPU",
    metrics: ["gpu.utilization", "gpu.memory.used", "gpu.temperature", "gpu.power"],
  },
  {
    id: "disk", label: "Storage",
    metrics: ["fs.used", "fs.usage", "fs.inodes.usage", "disk.read", "disk.write", "disk.read.latency", "disk.write.latency", "disk.utilization", "io.pressure.some", "io.pressure.full"],
  },
  { id: "network", label: "Network", metrics: ["net.rx", "net.tx"] },
];

export const SERIES_COLORS = [
  "var(--cpu)",
  "var(--mem)",
  // Swap amber is tuned for large gauge surfaces; graph lines need 3:1 against
  // the light card, so mix toward the foreground without leaving the hue.
  "color-mix(in oklab, var(--swap) 75%, var(--foreground))",
  "var(--warn)",
  "var(--crit)",
  "var(--accent)",
] as const;

export const SERIES_DASHES: ReadonlyArray<string | undefined> = [
  undefined,
  "7 4",
  "2 3",
  "10 3 2 3",
  "4 4",
  "1 3",
];

export function metricOption(metric: string): HistoryMetricOption | null {
  return HISTORY_METRICS.find((option) => option.metric === metric) ?? null;
}

export function metricFamily(metric: string): MetricFamily | null {
  return metricOption(metric)?.family ?? null;
}

/** Color and dash for a series: the metric hue for the first resource, then a palette rotation. */
export function metricSeriesStyle(
  option: HistoryMetricOption,
  resourceIndex: number,
): { color: string; dash: string | undefined } {
  const index = Math.max(0, resourceIndex);
  const offset = option.colorSlot + index;
  // When more series are shown than palette hues, the next color cycle also
  // advances the dash, so repeated hues stay distinguishable (per-core charts).
  const dashIndex = (index + Math.floor(offset / SERIES_COLORS.length)) % SERIES_DASHES.length;
  return {
    color: SERIES_COLORS[offset % SERIES_COLORS.length]!,
    dash: SERIES_DASHES[dashIndex],
  };
}

// ---------------------------------------------------------------------------
// Machine capability and defaults
// ---------------------------------------------------------------------------

export type MetricAvailability = { available: boolean; reason: string | null };

const FAMILY_LABEL: Record<MetricFamily, string> = {
  cpu: "CPU",
  memory: "Memory",
  swap: "Swap",
  gpus: "GPU metrics",
  disks: "Disk metrics",
  network: "Network metrics",
};

/**
 * Whether the collector shares this metric family with this hub. Missing
 * inventory is treated as unknown (not reported yet), never as unavailable,
 * so the page never hides potentially stored history on a guess.
 */
export function metricAvailability(machine: MachineState | undefined, metric: string): MetricAvailability {
  const option = metricOption(metric);
  if (option === null || machine === undefined) return { available: true, reason: null };
  const capability = machine.inventory?.capabilities?.[option.family];
  if (!capability) return { available: true, reason: null };
  if (capability.status === "supported") return { available: true, reason: null };
  const reason = capability.reason?.trim();
  return {
    available: false,
    reason: reason
      ? `${FAMILY_LABEL[option.family]}: ${capability.status} — ${reason}`
      : `${FAMILY_LABEL[option.family]} are ${capability.status} on this machine.`,
  };
}

function swapTotalBytes(machine: MachineState): number | null {
  const inventoryTotal = machine.inventory?.memory.swapTotalBytes;
  if (typeof inventoryTotal === "number") return inventoryTotal;
  const swap = machine.metrics?.payload.swap;
  return swap?.status === "ok" ? swap.totalBytes : null;
}

/**
 * A small, honest default selection: CPU and memory, swap only when the
 * machine actually has swap, and both GPU utilization and VRAM when a
 * supported card is present. Disabled families are simply left out.
 */
export function defaultMetricsFor(machine: MachineState | undefined): string[] {
  if (machine === undefined) return [];
  const selected: string[] = [];
  const add = (metric: string): void => {
    if (selected.length >= 16) return;
    if (metricAvailability(machine, metric).available) selected.push(metric);
  };
  add("cpu.usage");
  add("memory.used");
  const swap = swapTotalBytes(machine);
  if (swap !== null && swap > 0) add("swap.used");
  if (gpuCards(machine, []).length > 0) {
    add("gpu.utilization");
    add("gpu.memory.used");
  }
  return selected;
}

// ---------------------------------------------------------------------------
// Resources and labels
// ---------------------------------------------------------------------------

export type GpuCard = {
  id: string;
  label: string;
  index: number;
  totalBytes: number | null;
  supported: boolean;
  hasHistory: boolean;
};

function shortIdentifier(resourceId: string): string {
  const bare = resourceId.includes(":") ? resourceId.slice(resourceId.indexOf(":") + 1) : resourceId;
  if (bare.length <= 18) return bare;
  return `${bare.slice(0, 10)}…${bare.slice(-4)}`;
}

/**
 * GPU cards from inventory plus any card that only appears in stored history,
 * so a renamed or removed card never makes its history unreachable.
 */
export function gpuCards(machine: MachineState | undefined, series: readonly HistorySeries[]): GpuCard[] {
  const historyIds = new Set(
    series.filter((item) => item.metric.startsWith("gpu.") && item.resourceId !== "").map((item) => item.resourceId),
  );
  const cards: GpuCard[] = [];
  const seen = new Set<string>();
  const inventory = machine?.inventory?.gpus ?? [];
  inventory.forEach((gpu) => {
    const supported = gpu.telemetry === "supported";
    if (!supported && !historyIds.has(gpu.id)) return;
    seen.add(gpu.id);
    cards.push({
      id: gpu.id,
      label: gpu.name.trim() || shortIdentifier(gpu.id),
      // Number only charted cards; a muted management adapter is not a GPU slot
      // in this view's language.
      index: cards.length,
      totalBytes: gpu.memoryTotalBytes,
      supported,
      hasHistory: historyIds.has(gpu.id),
    });
  });
  // A snapshot can carry GPU readings before (or without) full inventory.
  const current = machine?.metrics?.payload.gpus;
  if (current?.status === "ok") {
    for (const gpu of current.devices) {
      if (seen.has(gpu.id) || gpu.status !== "ok") continue;
      seen.add(gpu.id);
      cards.push({
        id: gpu.id,
        label: shortIdentifier(gpu.id),
        index: cards.length,
        totalBytes: gpu.memoryTotalBytes,
        supported: true,
        hasHistory: historyIds.has(gpu.id),
      });
    }
  }
  for (const id of [...historyIds].sort()) {
    if (seen.has(id)) continue;
    cards.push({
      id,
      label: shortIdentifier(id),
      index: cards.length,
      totalBytes: null,
      supported: true,
      hasHistory: true,
    });
  }
  return cards;
}

export function unsupportedGpuCount(machine: MachineState | undefined): number {
  return (machine?.inventory?.gpus ?? []).filter((gpu) => gpu.telemetry !== "supported").length;
}

/** Human label for one stored resource id, using inventory when available. */
export function resourceLabel(machine: MachineState | undefined, metric: string, resourceId: string): string {
  if (resourceId === "") return metricOption(metric)?.shortLabel ?? "Total";
  const option = metricOption(metric);
  if (option === null) return shortIdentifier(resourceId);
  switch (option.family) {
    case "gpus": {
      const gpu = machine?.inventory?.gpus.find((item) => item.id === resourceId);
      return gpu?.name.trim() || shortIdentifier(resourceId);
    }
    case "disks": {
      if (metric.startsWith("fs.")) {
        const fs = machine?.inventory?.filesystems.find((item) => item.id === resourceId);
        return fs?.mountPoint || shortIdentifier(resourceId);
      }
      const disk = machine?.inventory?.disks.find((item) => item.id === resourceId);
      return disk?.name || shortIdentifier(resourceId);
    }
    case "network": {
      const iface = machine?.inventory?.network.find((item) => item.id === resourceId);
      return iface?.name || shortIdentifier(resourceId);
    }
    case "cpu": {
      const match = /^core:(\d+)$/.exec(resourceId);
      return match ? `Core ${match[1]}` : shortIdentifier(resourceId);
    }
    default:
      return shortIdentifier(resourceId);
  }
}

export function metricUnitLabel(unit: MetricUnit | string): string {
  switch (unit) {
    case "percent":
      return "%";
    case "bytes":
      return "bytes";
    case "bytes_per_second":
      return "bytes/s";
    case "celsius":
      return "°C";
    case "watts":
      return "W";
    case "milliseconds":
      return "ms";
    default:
      return unit;
  }
}

export function isMetricUnit(value: unknown): value is HistoryMetricUnit {
  return (
    value === "percent" ||
    value === "bytes" ||
    value === "bytes_per_second" ||
    value === "celsius" ||
    value === "watts" ||
    value === "milliseconds"
  );
}
