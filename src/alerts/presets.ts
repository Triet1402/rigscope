/**
 * Warning presets for alert rule creation.
 *
 * A preset is a suggestion for the existing rule form. It never creates,
 * enables, or saves a rule: applying one only fills editable fields, and the
 * user reviews and submits the form. Thresholds are starting points for this
 * machine, not hardware safety limits.
 *
 * Percentage metrics use the 0–100 scale from the shared telemetry contract.
 * `memory.usage`, `fs.usage` and `fs.inodes.usage` are derived by the
 * collector/storage layer from a positive total, so the same preset works on
 * machines of different sizes.
 *
 * Optional readings (CPU temperature/power/iowait/pressure, pressure stalls,
 * inode counts, disk latency/utilization) may be absent on older collectors.
 * The resolution marks those presets unavailable with a reason instead of
 * pretending a rule can fire.
 */
import type { Inventory, MachineState, MetricsPayload } from "../contracts";
import {
  alertMetricFamily,
  type AlertCondition,
  type AlertMetric,
  type AlertMetricFamily,
  type AlertRuleInput,
} from "./types";

export type AlertPresetResourceKind = "none" | "filesystem" | "gpu" | "disk";

export type AlertPreset = {
  id: string;
  label: string;
  /** Suggested rule name; editable in the form. */
  ruleName: string;
  metric: AlertMetric;
  condition: AlertCondition;
  threshold: number | null;
  durationMs: number;
  resourceKind: AlertPresetResourceKind;
  /** One line explaining the suggested threshold and duration. */
  description: string;
};

export const ALERT_PRESETS = [
  {
    id: "sustained-high-cpu",
    label: "Sustained high CPU",
    ruleName: "Sustained high CPU",
    metric: "cpu.usage",
    condition: "above",
    threshold: 90,
    durationMs: 300_000,
    resourceKind: "none",
    description: "Suggests CPU at or above 90% for 5 minutes.",
  },
  {
    id: "memory-nearly-full",
    label: "Memory nearly full",
    ruleName: "Memory nearly full",
    metric: "memory.usage",
    condition: "above",
    threshold: 90,
    durationMs: 300_000,
    resourceKind: "none",
    description: "Suggests memory usage at or above 90% for 5 minutes.",
  },
  {
    id: "disk-nearly-full",
    label: "Disk nearly full",
    ruleName: "Disk nearly full",
    metric: "fs.usage",
    condition: "above",
    threshold: 90,
    durationMs: 300_000,
    resourceKind: "filesystem",
    description: "Suggests filesystem usage at or above 90% for 5 minutes.",
  },
  {
    id: "inode-capacity-low",
    label: "Inode capacity low",
    ruleName: "Inode capacity low",
    metric: "fs.inodes.usage",
    condition: "above",
    threshold: 90,
    durationMs: 300_000,
    resourceKind: "filesystem",
    description: "Suggests inode usage at or above 90% for 5 minutes.",
  },
  {
    id: "gpu-hot",
    label: "GPU hot",
    ruleName: "GPU hot",
    metric: "gpu.temperature",
    condition: "above",
    threshold: 85,
    durationMs: 60_000,
    resourceKind: "gpu",
    description: "Suggests GPU temperature at or above 85 °C for 1 minute.",
  },
  {
    id: "cpu-hot",
    label: "CPU hot",
    ruleName: "CPU hot",
    metric: "cpu.temperature",
    condition: "above",
    threshold: 90,
    durationMs: 60_000,
    resourceKind: "none",
    description: "Suggests CPU temperature at or above 90 °C for 1 minute.",
  },
  {
    id: "collector-offline",
    label: "Collector offline",
    ruleName: "Collector offline",
    metric: "cpu.usage",
    condition: "stale",
    threshold: null,
    durationMs: 0,
    resourceKind: "none",
    description: "Uses the existing stale condition and the collector's advertised cadence.",
  },
  {
    id: "memory-pressure",
    label: "Memory pressure",
    ruleName: "Memory pressure",
    metric: "memory.pressure.some",
    condition: "above",
    threshold: 10,
    durationMs: 60_000,
    resourceKind: "none",
    description: "Suggests memory pressure at or above 10% for 1 minute.",
  },
  {
    id: "io-pressure",
    label: "I/O pressure",
    ruleName: "I/O pressure",
    metric: "io.pressure.some",
    condition: "above",
    threshold: 10,
    durationMs: 60_000,
    resourceKind: "none",
    description: "Suggests I/O pressure at or above 10% for 1 minute.",
  },
  {
    id: "cpu-pressure",
    label: "CPU pressure",
    ruleName: "CPU pressure",
    metric: "cpu.pressure",
    condition: "above",
    threshold: 10,
    durationMs: 60_000,
    resourceKind: "none",
    description: "Suggests CPU pressure at or above 10% for 1 minute.",
  },
  {
    id: "high-io-wait",
    label: "High I/O wait",
    ruleName: "High I/O wait",
    metric: "cpu.iowait",
    condition: "above",
    threshold: 20,
    durationMs: 300_000,
    resourceKind: "none",
    description: "Suggests CPU I/O wait at or above 20% for 5 minutes.",
  },
  {
    id: "disk-busy",
    label: "Disk busy",
    ruleName: "Disk busy",
    metric: "disk.utilization",
    condition: "above",
    threshold: 90,
    durationMs: 300_000,
    resourceKind: "disk",
    description: "Suggests disk utilization at or above 90% for 5 minutes.",
  },
  {
    id: "slow-disk-reads",
    label: "Slow disk reads",
    ruleName: "Slow disk reads",
    metric: "disk.read.latency",
    condition: "above",
    threshold: 100,
    durationMs: 300_000,
    resourceKind: "disk",
    description: "Suggests read latency at or above 100 ms for 5 minutes.",
  },
] as const satisfies readonly AlertPreset[];

export type AlertPresetId = (typeof ALERT_PRESETS)[number]["id"];

export function alertPresetById(id: string): AlertPreset | undefined {
  return ALERT_PRESETS.find((preset) => preset.id === id);
}

export type AlertPresetResourceOption = {value: string; label: string};

export type AlertPresetStatus =
  | {kind: "ready"}
  | {kind: "waiting"; reason: string}
  | {kind: "unavailable"; reason: string};

export type AlertPresetResolution = {
  preset: AlertPreset;
  status: AlertPresetStatus;
  /** Default resource from the selected machine; empty for single-resource metrics. */
  resourceId: string;
  /** Resources the selected machine actually offers for this preset. */
  resources: readonly AlertPresetResourceOption[];
  /** Editable rule input, or null when the preset is not ready to apply. */
  input: AlertRuleInput | null;
};

const FAMILY_LABELS: Record<AlertMetricFamily, string> = {
  cpu: "CPU",
  memory: "Memory",
  swap: "Swap",
  gpus: "GPU",
  disks: "Disk",
  network: "Network",
};

function unavailable(reason: string): AlertPresetStatus {
  return {kind: "unavailable", reason};
}

function waitingForMetrics(): AlertPresetStatus {
  return {kind: "waiting", reason: "Waiting for the first metrics sample."};
}

function numericField(value: unknown, key: string): number | null {
  if (value === null || typeof value !== "object") return null;
  const field = (value as Record<string, unknown>)[key];
  return typeof field === "number" && Number.isFinite(field) ? field : null;
}

function familyReading(payload: MetricsPayload, family: AlertMetricFamily) {
  switch (family) {
    case "cpu":
      return payload.cpu;
    case "memory":
      return payload.memory;
    case "swap":
      return payload.swap;
    case "gpus":
      return payload.gpus;
    case "disks":
      return payload.disks;
    case "network":
      return payload.network;
  }
}

function familyProblem(
  machine: MachineState,
  family: AlertMetricFamily,
): AlertPresetStatus | null {
  const capability = machine.inventory?.capabilities[family];
  if (capability?.status === "unsupported" || capability?.status === "withheld") {
    return unavailable(`${FAMILY_LABELS[family]} metrics are not shared with this hub.`);
  }
  const payload = machine.metrics?.payload;
  if (payload === undefined) return waitingForMetrics();
  if (familyReading(payload, family).status === "unavailable") {
    return unavailable(`${FAMILY_LABELS[family]} readings are unavailable on this machine.`);
  }
  return null;
}

function resourcesFor(
  kind: AlertPresetResourceKind,
  inventory: Inventory | undefined,
): {resources: AlertPresetResourceOption[]; resourceId: string} {
  if (kind === "none" || inventory === undefined) return {resources: [], resourceId: ""};
  if (kind === "filesystem") {
    const resources = inventory.filesystems.map((filesystem) => ({
      value: filesystem.id,
      label: `${filesystem.mountPoint} (${filesystem.fsType})`,
    }));
    const preferred =
      inventory.filesystems.find((filesystem) => filesystem.mountPoint === "/") ??
      inventory.filesystems[0];
    return {resources, resourceId: preferred?.id ?? ""};
  }
  if (kind === "gpu") {
    const supported = inventory.gpus.filter((gpu) => gpu.telemetry === "supported");
    return {
      resources: supported.map((gpu) => ({value: gpu.id, label: gpu.name})),
      resourceId: supported[0]?.id ?? "",
    };
  }
  return {
    resources: inventory.disks.map((disk) => ({value: disk.id, label: disk.name})),
    resourceId: inventory.disks[0]?.id ?? "",
  };
}

function resourceMissingReason(kind: AlertPresetResourceKind): string {
  switch (kind) {
    case "filesystem":
      return "No filesystems are reported by this machine.";
    case "gpu":
      return "No telemetry-capable GPU is reported by this machine.";
    case "disk":
      return "No disks are reported by this machine.";
    case "none":
      return "No resource is available.";
  }
}

function findFilesystem(payload: MetricsPayload, resourceId: string) {
  return payload.disks.status === "ok"
    ? payload.disks.filesystems.find((filesystem) => filesystem.id === resourceId)
    : undefined;
}

function findGpu(payload: MetricsPayload, resourceId: string) {
  return payload.gpus.status === "ok"
    ? payload.gpus.devices.find((gpu) => gpu.id === resourceId)
    : undefined;
}

function findDisk(payload: MetricsPayload, resourceId: string) {
  return payload.disks.status === "ok"
    ? payload.disks.devices.find((disk) => disk.id === resourceId)
    : undefined;
}

function optionalReading(value: unknown, key: string, label: string): AlertPresetStatus {
  return numericField(value, key) === null
    ? unavailable(`${label} is not reported by this collector.`)
    : {kind: "ready"};
}

function evaluatePreset(
  preset: AlertPreset,
  machine: MachineState,
  resourceId: string,
): AlertPresetStatus {
  // Stale rules evaluate receipt time, not a metric value, so they work before
  // the first sample and on collectors without any optional readings.
  if (preset.condition === "stale") return {kind: "ready"};

  const family = alertMetricFamily(preset.metric);
  const familyStatus = familyProblem(machine, family);
  if (familyStatus !== null) return familyStatus;

  const payload = machine.metrics?.payload;
  if (payload === undefined) return waitingForMetrics();

  switch (preset.metric) {
    case "cpu.usage":
      return {kind: "ready"};
    case "cpu.temperature":
      return optionalReading(payload.cpu, "temperatureC", "CPU temperature");
    case "cpu.power":
      return optionalReading(payload.cpu, "powerWatts", "CPU package power");
    case "cpu.iowait":
      return optionalReading(payload.cpu, "iowaitPercent", "CPU I/O wait");
    case "cpu.pressure":
      return optionalReading(payload.cpu, "pressureSomePercent", "CPU pressure");
    case "memory.usage": {
      const memory = payload.memory;
      if (memory.status !== "ok" || !(memory.totalBytes > 0)) {
        return unavailable("Memory total is not reported by this machine.");
      }
      return {kind: "ready"};
    }
    case "memory.pressure.some":
      return optionalReading(payload.memory, "pressureSomePercent", "Memory pressure");
    case "memory.pressure.full":
      return optionalReading(payload.memory, "pressureFullPercent", "Memory full pressure");
    case "io.pressure.some":
      return optionalReading(payload.disks, "pressureSomePercent", "I/O pressure");
    case "io.pressure.full":
      return optionalReading(payload.disks, "pressureFullPercent", "I/O full pressure");
    case "fs.usage": {
      const filesystem = findFilesystem(payload, resourceId);
      if (filesystem === undefined) {
        return unavailable("The latest sample does not include this filesystem.");
      }
      if (filesystem.status !== "ok" || !(filesystem.totalBytes > 0)) {
        return unavailable("Filesystem size is not reported for this filesystem.");
      }
      return {kind: "ready"};
    }
    case "fs.inodes.usage": {
      const filesystem = findFilesystem(payload, resourceId);
      if (filesystem === undefined) {
        return unavailable("The latest sample does not include this filesystem.");
      }
      if (filesystem.status !== "ok") {
        return unavailable("This filesystem is unavailable in the latest sample.");
      }
      const used = numericField(filesystem, "inodesUsed");
      const total = numericField(filesystem, "inodesTotal");
      if (used === null || total === null || total <= 0) {
        return unavailable("Inode usage is not reported for this filesystem.");
      }
      return {kind: "ready"};
    }
    case "gpu.temperature": {
      const gpu = findGpu(payload, resourceId);
      if (gpu === undefined) {
        return unavailable("The latest sample does not include this GPU.");
      }
      if (gpu.status !== "ok") {
        return unavailable("This GPU is unavailable in the latest sample.");
      }
      return optionalReading(gpu, "temperatureC", "GPU temperature");
    }
    case "disk.utilization": {
      const disk = findDisk(payload, resourceId);
      if (disk === undefined) {
        return unavailable("The latest sample does not include this disk.");
      }
      if (disk.status !== "ok") {
        return unavailable("This disk is unavailable in the latest sample.");
      }
      return optionalReading(disk, "ioUtilizationPercent", "Disk utilization");
    }
    case "disk.read.latency": {
      const disk = findDisk(payload, resourceId);
      if (disk === undefined) {
        return unavailable("The latest sample does not include this disk.");
      }
      if (disk.status !== "ok") {
        return unavailable("This disk is unavailable in the latest sample.");
      }
      return optionalReading(disk, "readLatencyMs", "Disk read latency");
    }
    case "disk.write.latency": {
      const disk = findDisk(payload, resourceId);
      if (disk === undefined) {
        return unavailable("The latest sample does not include this disk.");
      }
      if (disk.status !== "ok") {
        return unavailable("This disk is unavailable in the latest sample.");
      }
      return optionalReading(disk, "writeLatencyMs", "Disk write latency");
    }
    default:
      return {kind: "ready"};
  }
}

function resolution(
  preset: AlertPreset,
  machine: MachineState | undefined,
  status: AlertPresetStatus,
  resources: readonly AlertPresetResourceOption[],
  resourceId: string,
): AlertPresetResolution {
  const input: AlertRuleInput | null =
    status.kind === "ready" && machine !== undefined
      ? {
          name: preset.ruleName,
          machineId: machine.machineId,
          metric: preset.metric,
          resourceId,
          condition: preset.condition,
          threshold: preset.threshold,
          durationMs: preset.durationMs,
          enabled: true,
        }
      : null;
  return {preset, status, resourceId, resources, input};
}

export function resolveAlertPreset(
  preset: AlertPreset,
  machine: MachineState | undefined,
): AlertPresetResolution {
  const {resources, resourceId} = resourcesFor(preset.resourceKind, machine?.inventory);
  if (machine === undefined) {
    return resolution(preset, machine, unavailable("Choose a machine."), resources, resourceId);
  }
  if (preset.resourceKind !== "none" && resourceId === "") {
    return resolution(
      preset,
      machine,
      unavailable(resourceMissingReason(preset.resourceKind)),
      resources,
      resourceId,
    );
  }
  return resolution(
    preset,
    machine,
    evaluatePreset(preset, machine, resourceId),
    resources,
    resourceId,
  );
}

export function resolveAlertPresets(machine: MachineState | undefined): AlertPresetResolution[] {
  return ALERT_PRESETS.map((preset) => resolveAlertPreset(preset, machine));
}
