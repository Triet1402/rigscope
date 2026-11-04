/**
 * Focused tests for optional bottleneck readings in the resource view.
 *
 * The shared telemetry contract adds these fields in a parallel checkout, so
 * fixtures carry them structurally. Omission must stay an explicit unavailable
 * state and must never become a successful zero.
 */
import assert from "node:assert/strict";
import test from "node:test";

import type { MachineState, MetricsRecord } from "../src/contracts";
import {
  formatLatency,
  machineResourceView,
} from "../src/resources/presentation";

const RECEIVED = "2026-11-02T12:00:00.000Z";
const NOW = Date.parse(RECEIVED) + 1_000;

const INVENTORY = {
  os: { platform: "linux", release: "6.0", distro: "Ubuntu 26.10", arch: "x64" },
  cpu: { model: "AMD EPYC 7402P", logicalCores: 4 },
  memory: { totalBytes: 16, swapTotalBytes: 0 },
  gpus: [],
  filesystems: [
    { id: "fs:/", mountPoint: "/", device: "/dev/sda1", fsType: "ext4", totalBytes: 100 },
  ],
  disks: [
    { id: "disk:sda", name: "sda", model: "Fixture SSD", rotational: false, sizeBytes: 1_000 },
  ],
  network: [],
  capabilities: {
    cpu: { status: "supported" as const },
    memory: { status: "supported" as const },
    swap: { status: "supported" as const },
    gpus: { status: "supported" as const },
    disks: { status: "supported" as const },
    network: { status: "supported" as const },
    processes: { status: "supported" as const },
  },
};

function machine(payload: Record<string, unknown>, lastReceivedAt = RECEIVED): MachineState {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    hostname: "rig",
    connected: true,
    lastReceivedAt,
    expectedIntervalMs: 5_000,
    inventory: INVENTORY,
    metrics: {
      collectorId: "collector-1",
      runId: "run-1",
      bootId: "boot-1",
      sequence: 1,
      capturedAt: RECEIVED,
      receivedAt: lastReceivedAt,
      kind: "metrics",
      payload,
    } as unknown as MetricsRecord & { receivedAt: string },
  } as unknown as MachineState;
}

function basePayload(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    cpu: { status: "ok", usagePercent: 50, perCore: [50, 50, 50, 50] },
    memory: { status: "ok", usedBytes: 8, totalBytes: 16 },
    swap: { status: "ok", usedBytes: 0, totalBytes: 0 },
    gpus: { status: "ok", devices: [] },
    disks: {
      status: "ok",
      filesystems: [{ id: "fs:/", status: "ok", usedBytes: 50, totalBytes: 100 }],
      devices: [{ id: "disk:sda", status: "ok", readBytesPerSec: 10, writeBytesPerSec: 20 }],
    },
    network: { status: "ok", interfaces: [] },
    ...overrides,
  };
}

test("optional bottleneck fields become readable live values", () => {
  const view = machineResourceView(
    machine(
      basePayload({
        cpu: {
          status: "ok",
          usagePercent: 50,
          perCore: [50, 50, 50, 50],
          temperatureC: 61.5,
          powerWatts: 88.2,
          iowaitPercent: 3.5,
          pressureSomePercent: 7.5,
        },
        memory: {
          status: "ok",
          usedBytes: 8,
          totalBytes: 16,
          pressureSomePercent: 2.5,
          pressureFullPercent: 0.5,
        },
        disks: {
          status: "ok",
          filesystems: [
            {
              id: "fs:/",
              status: "ok",
              usedBytes: 50,
              totalBytes: 100,
              inodesUsed: 400,
              inodesTotal: 1_000,
            },
          ],
          devices: [
            {
              id: "disk:sda",
              status: "ok",
              readBytesPerSec: 10,
              writeBytesPerSec: 20,
              readLatencyMs: 0.42,
              writeLatencyMs: 2.5,
              ioUtilizationPercent: 35,
            },
          ],
          pressureSomePercent: 4.5,
          pressureFullPercent: 1.25,
        },
      }),
    ),
    NOW,
  );

  assert.deepEqual(view.bottlenecks.cpuTemperature, {
    state: "value",
    value: 61.5,
    stale: false,
  });
  assert.deepEqual(view.bottlenecks.cpuPower, { state: "value", value: 88.2, stale: false });
  assert.deepEqual(view.bottlenecks.cpuIowait, { state: "value", value: 3.5, stale: false });
  assert.deepEqual(view.bottlenecks.cpuPressure, { state: "value", value: 7.5, stale: false });
  assert.deepEqual(view.bottlenecks.memoryPressureSome, {
    state: "value",
    value: 2.5,
    stale: false,
  });
  assert.deepEqual(view.bottlenecks.memoryPressureFull, {
    state: "value",
    value: 0.5,
    stale: false,
  });
  assert.deepEqual(view.bottlenecks.ioPressureSome, {
    state: "value",
    value: 4.5,
    stale: false,
  });
  assert.deepEqual(view.bottlenecks.ioPressureFull, {
    state: "value",
    value: 1.25,
    stale: false,
  });

  assert.equal(view.filesystems.items[0]!.inodesUsed, 400);
  assert.equal(view.filesystems.items[0]!.inodesTotal, 1_000);
  assert.equal(view.disks.items[0]!.readLatencyMs, 0.42);
  assert.equal(view.disks.items[0]!.writeLatencyMs, 2.5);
  assert.equal(view.disks.items[0]!.ioUtilizationPercent, 35);
});

test("older collectors that omit bottleneck fields stay unavailable, never zero", () => {
  const view = machineResourceView(machine(basePayload()), NOW);
  for (const reading of [
    view.bottlenecks.cpuTemperature,
    view.bottlenecks.cpuPower,
    view.bottlenecks.cpuIowait,
    view.bottlenecks.cpuPressure,
    view.bottlenecks.memoryPressureSome,
    view.bottlenecks.memoryPressureFull,
    view.bottlenecks.ioPressureSome,
    view.bottlenecks.ioPressureFull,
  ]) {
    assert.equal(reading.state, "unavailable");
    assert.equal((reading as { reason: string }).reason, "Not reported for this sample.");
  }
  assert.equal(view.filesystems.items[0]!.inodesUsed, null);
  assert.equal(view.filesystems.items[0]!.inodesTotal, null);
  assert.equal(view.disks.items[0]!.readLatencyMs, null);
  assert.equal(view.disks.items[0]!.writeLatencyMs, null);
  assert.equal(view.disks.items[0]!.ioUtilizationPercent, null);
});

test("malformed optional values are unavailable or null rather than zero", () => {
  const view = machineResourceView(
    machine(
      basePayload({
        cpu: {
          status: "ok",
          usagePercent: 50,
          perCore: [50],
          temperatureC: Number.NaN,
          powerWatts: "12" as unknown as number,
          iowaitPercent: null,
          pressureSomePercent: -1,
        },
        disks: {
          status: "ok",
          filesystems: [
            {
              id: "fs:/",
              status: "ok",
              usedBytes: 50,
              totalBytes: 100,
              inodesUsed: "400",
              inodesTotal: -1,
            },
          ],
          devices: [
            {
              id: "disk:sda",
              status: "ok",
              readBytesPerSec: 10,
              writeBytesPerSec: 20,
              readLatencyMs: null,
              writeLatencyMs: -0.5,
              ioUtilizationPercent: Number.POSITIVE_INFINITY,
            },
          ],
        },
      }),
    ),
    NOW,
  );

  assert.equal(view.bottlenecks.cpuTemperature.state, "unavailable");
  assert.equal(view.bottlenecks.cpuPower.state, "unavailable");
  assert.equal(view.bottlenecks.cpuIowait.state, "unavailable");
  assert.equal(view.bottlenecks.cpuPressure.state, "unavailable");
  assert.equal(view.filesystems.items[0]!.inodesUsed, null);
  assert.equal(view.filesystems.items[0]!.inodesTotal, null);
  assert.equal(view.disks.items[0]!.readLatencyMs, null);
  assert.equal(view.disks.items[0]!.writeLatencyMs, null);
  assert.equal(view.disks.items[0]!.ioUtilizationPercent, null);
});

test("family failures and missing samples keep their own honest reasons", () => {
  const view = machineResourceView(
    machine(
      basePayload({
        cpu: { status: "unavailable", reason: "cpu counters failed" },
        memory: { status: "unavailable", reason: "memory counters failed" },
        disks: { status: "unavailable", reason: "disk statistics unavailable" },
      }),
    ),
    NOW,
  );
  assert.deepEqual(view.bottlenecks.cpuTemperature, {
    state: "unavailable",
    reason: "cpu counters failed",
  });
  assert.deepEqual(view.bottlenecks.memoryPressureSome, {
    state: "unavailable",
    reason: "memory counters failed",
  });
  assert.deepEqual(view.bottlenecks.ioPressureFull, {
    state: "unavailable",
    reason: "disk statistics unavailable",
  });
  assert.equal(view.filesystems.items[0]!.state, "unavailable");
  assert.equal(view.filesystems.items[0]!.inodesUsed, null);

  const withoutMetrics = machine(basePayload());
  withoutMetrics.metrics = undefined;
  const empty = machineResourceView(withoutMetrics, NOW);
  assert.equal(empty.bottlenecks.cpuTemperature.state, "absent");
  assert.match((empty.bottlenecks.cpuTemperature as { reason: string }).reason, /No CPU reading/);
});

test("stale samples retain bottleneck values with the stale flag", () => {
  const view = machineResourceView(
    machine(
      basePayload({
        cpu: {
          status: "ok",
          usagePercent: 50,
          perCore: [50],
          temperatureC: 55,
        },
      }),
    ),
    Date.parse(RECEIVED) + 20_000,
  );
  assert.equal(view.stale, true);
  assert.equal(view.bottlenecks.cpuTemperature.state, "value");
  assert.equal(
    view.bottlenecks.cpuTemperature.state === "value" && view.bottlenecks.cpuTemperature.stale,
    true,
  );
});

test("latency formatting keeps sub-millisecond detail", () => {
  assert.equal(formatLatency(0.42), "0.42 ms");
  assert.equal(formatLatency(12.34), "12.3 ms");
  assert.equal(formatLatency(null), "—");
  assert.equal(formatLatency(-1), "—");
});
