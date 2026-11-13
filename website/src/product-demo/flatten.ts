/**
 * Flatten a metrics payload into demo history points.
 *
 * This is the website's copy of the product's telemetry-to-series mapping,
 * reduced to what the synthetic demo emits (every family is always ok). The
 * real mapping runs inside the Go hub; the website must build without importing
 * runtime code.
 */
import type { MetricsPayload } from "../../../src/contracts";

export type FlatPoint = {
  metric: string;
  resourceId: string;
  value: number | null;
};

function percentOf(used: number | null | undefined, total: number): number | null {
  if (used === null || used === undefined || !Number.isFinite(used) || !Number.isFinite(total) || total <= 0) {
    return null;
  }
  const percent = Math.min(100, Math.max(0, (used / total) * 100));
  return Math.round(percent * 100) / 100;
}

export function flattenMetrics(payload: MetricsPayload): FlatPoint[] {
  const points: FlatPoint[] = [];
  const push = (metric: string, resourceId: string, value: number | null): void => {
    points.push({ metric, resourceId, value });
  };

  const cpu = payload.cpu;
  if (cpu.status === "ok") {
    push("cpu.usage", "", cpu.usagePercent);
    for (const [index, value] of cpu.perCore.entries()) push("cpu.core", `core:${index}`, value);
    push("cpu.temperature", "", cpu.temperatureC ?? null);
    push("cpu.power", "", cpu.powerWatts ?? null);
    push("cpu.iowait", "", cpu.iowaitPercent ?? null);
    push("cpu.pressure", "", cpu.pressureSomePercent ?? null);
  }

  const memory = payload.memory;
  if (memory.status === "ok") {
    push("memory.used", "", memory.usedBytes);
    push("memory.total", "", memory.totalBytes);
    push("memory.usage", "", percentOf(memory.usedBytes, memory.totalBytes));
    push("memory.pressure.some", "", memory.pressureSomePercent ?? null);
    push("memory.pressure.full", "", memory.pressureFullPercent ?? null);
  }

  const swap = payload.swap;
  if (swap.status === "ok") {
    push("swap.used", "", swap.usedBytes);
    push("swap.total", "", swap.totalBytes);
  }

  if (payload.gpus.status === "ok") {
    for (const gpu of payload.gpus.devices) {
      if (gpu.status === "unavailable") continue;
      push("gpu.utilization", gpu.id, gpu.utilizationPercent);
      push("gpu.memory.used", gpu.id, gpu.memoryUsedBytes);
      push("gpu.memory.total", gpu.id, gpu.memoryTotalBytes);
      push("gpu.temperature", gpu.id, gpu.temperatureC);
      push("gpu.power", gpu.id, gpu.powerWatts);
      push("gpu.junction.temperature", gpu.id, gpu.junctionTemperatureC ?? null);
      push("gpu.vram.temperature", gpu.id, gpu.vramTemperatureC ?? null);
    }
  }

  if (payload.disks.status === "ok") {
    push("io.pressure.some", "", payload.disks.pressureSomePercent ?? null);
    push("io.pressure.full", "", payload.disks.pressureFullPercent ?? null);
    for (const filesystem of payload.disks.filesystems) {
      if (filesystem.status === "unavailable") continue;
      push("fs.used", filesystem.id, filesystem.usedBytes);
      push("fs.total", filesystem.id, filesystem.totalBytes);
      push("fs.usage", filesystem.id, percentOf(filesystem.usedBytes, filesystem.totalBytes));
      push("fs.inodes.usage", filesystem.id, filesystem.inodesTotal == null
        ? null
        : percentOf(filesystem.inodesUsed, filesystem.inodesTotal));
    }
    for (const disk of payload.disks.devices) {
      if (disk.status === "unavailable") continue;
      push("disk.read", disk.id, disk.readBytesPerSec);
      push("disk.write", disk.id, disk.writeBytesPerSec);
      push("disk.read.latency", disk.id, disk.readLatencyMs ?? null);
      push("disk.write.latency", disk.id, disk.writeLatencyMs ?? null);
      push("disk.utilization", disk.id, disk.ioUtilizationPercent ?? null);
    }
  }

  if (payload.network.status === "ok") {
    for (const iface of payload.network.interfaces) {
      if (iface.status === "unavailable") continue;
      push("net.rx", iface.id, iface.rxBytesPerSec);
      push("net.tx", iface.id, iface.txBytesPerSec);
    }
  }

  return points;
}
