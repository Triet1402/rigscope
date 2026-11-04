/** Deterministic alert fixtures shared by the engine and HTTP tests. */
import type { MachineState } from "../src/contracts";
import type { MetricsPayload, MetricsRecord } from "../src/contracts";

export const BOOT_ID = "00000000-0000-4000-8000-000000000001";
export const RUN_ID = "run-alert";
export const COLLECTOR_ID = "collector-alert";

export function basePayload(overrides: Partial<MetricsPayload> = {}): MetricsPayload {
  return {
    cpu: {status: "ok", usagePercent: 50, perCore: [50, 50]},
    memory: {status: "ok", usedBytes: 8, totalBytes: 16},
    swap: {status: "ok", usedBytes: 2, totalBytes: 4},
    gpus: {
      status: "ok",
      devices: [
        {
          id: "gpu:GPU-1",
          status: "ok",
          utilizationPercent: 40,
          memoryUsedBytes: 100,
          memoryTotalBytes: 200,
          temperatureC: 60,
          powerWatts: 100,
        },
      ],
    },
    disks: {
      status: "ok",
      filesystems: [{id: "fs:/", status: "ok", usedBytes: 50, totalBytes: 100}],
      devices: [{id: "disk:sda", status: "ok", readBytesPerSec: 1, writeBytesPerSec: 2}],
    },
    network: {
      status: "ok",
      interfaces: [{id: "net:eno1", status: "ok", rxBytesPerSec: 3, txBytesPerSec: 4}],
    },
    ...overrides,
  };
}

export function metricsRecord(
  payload: MetricsPayload,
  input: {sequence?: number; capturedAt?: string; machineId?: string; collectorId?: string; runId?: string} = {},
): MetricsRecord {
  return {
    collectorId: input.collectorId ?? COLLECTOR_ID,
    runId: input.runId ?? RUN_ID,
    bootId: BOOT_ID,
    sequence: input.sequence ?? 1,
    capturedAt: input.capturedAt ?? new Date(0).toISOString(),
    kind: "metrics",
    payload,
  };
}

export function machineState(input: {
  machineId?: string;
  lastReceivedAt: string | null;
  expectedIntervalMs?: number | null;
  connected?: boolean;
  metrics?: MetricsRecord;
}): MachineState {
  const machineId = input.machineId ?? "m1";
  return {
    machineId,
    collectorId: COLLECTOR_ID,
    hostname: machineId,
    connected: input.connected ?? true,
    lastReceivedAt: input.lastReceivedAt,
    expectedIntervalMs: input.expectedIntervalMs === undefined ? 5_000 : input.expectedIntervalMs,
    ...(input.metrics
      ? {
          metrics: {
            ...input.metrics,
            receivedAt: input.lastReceivedAt ?? input.metrics.capturedAt,
          },
        }
      : {}),
  };
}
