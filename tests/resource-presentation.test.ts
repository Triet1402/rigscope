/** Deterministic tests for the resource/fleet/Connections presentation seam. */
import test from "node:test";
import assert from "node:assert/strict";

import type { MachineState } from "../src/contracts";
import type { Inventory, MetricsRecord, StatusMessage } from "../src/contracts";
import {
  collectorConnectionViews,
  filterMachines,
  fleetResourceSummary,
  formatCount,
  formatPower,
  formatRate,
  formatSpeedMbps,
  formatTemperature,
  machineDisplayName,
  machineResourceView,
  machineTags,
} from "../src/resources/presentation";

const RECEIVED = "2026-11-02T12:00:00.000Z";
const NOW = Date.parse(RECEIVED) + 1_000;

function inventory(): Inventory {
  const supported = {status: "supported" as const};
  return {
    os: {platform: "linux", release: "6.0", distro: "Ubuntu 26.10", arch: "x64"},
    cpu: {model: "AMD EPYC 7402P", logicalCores: 3},
    memory: {totalBytes: 16, swapTotalBytes: 4},
    gpus: [
      {
        id: "gpu:GPU-1",
        name: "NVIDIA GeForce RTX 3090",
        vendor: "nvidia",
        pciBusId: "0000:c1:00.0",
        telemetry: "supported",
        memoryTotalBytes: 200,
      },
      {
        id: "gpu:GPU-2",
        name: "NVIDIA GeForce RTX 3090",
        vendor: "nvidia",
        pciBusId: "0000:c2:00.0",
        telemetry: "supported",
        memoryTotalBytes: 100,
      },
      {
        id: "pci:0000:44:00.0",
        name: "ASPEED management VGA",
        vendor: "aspeed",
        pciBusId: "0000:44:00.0",
        telemetry: "unsupported",
        memoryTotalBytes: null,
        reason: "management adapter; no telemetry",
      },
    ],
    filesystems: [
      {id: "fs:/", mountPoint: "/", device: "/dev/sda1", fsType: "ext4", totalBytes: 100},
    ],
    disks: [
      {id: "disk:sda", name: "sda", model: "Fixture SSD", rotational: false, sizeBytes: 1_000},
    ],
    network: [
      {
        id: "net:eno1",
        name: "eno1",
        mac: "aa:bb:cc:dd:ee:01",
        addresses: [{family: "ipv4", address: "192.0.2.5", prefix: 24}],
        virtual: false,
        state: "up",
        speedMbps: 1000,
      },
      {
        id: "net:tailscale0",
        name: "tailscale0",
        mac: null,
        addresses: [],
        virtual: true,
        state: "unknown",
        speedMbps: null,
      },
    ],
    capabilities: {
      cpu: supported,
      memory: supported,
      swap: supported,
      gpus: supported,
      disks: supported,
      network: supported,
      processes: supported,
    },
  };
}

function metrics(overrides: Partial<MetricsRecord["payload"]> = {}): MetricsRecord & {receivedAt: string} {
  return {
    collectorId: "collector-1",
    runId: "run-1",
    bootId: "boot-1",
    sequence: 3,
    capturedAt: RECEIVED,
    receivedAt: RECEIVED,
    kind: "metrics",
    payload: {
      cpu: {status: "ok", usagePercent: 50, perCore: [100, 0, null]},
      memory: {status: "ok", usedBytes: 8, totalBytes: 16},
      swap: {status: "ok", usedBytes: 2, totalBytes: 4},
      gpus: {
        status: "ok",
        devices: [
          {
            id: "gpu:GPU-1",
            status: "ok",
            utilizationPercent: 90,
            memoryUsedBytes: 100,
            memoryTotalBytes: 200,
            temperatureC: 60.4,
            powerWatts: 120.2,
          },
          {
            id: "gpu:GPU-2",
            status: "ok",
            utilizationPercent: 20,
            memoryUsedBytes: 50,
            memoryTotalBytes: 100,
            temperatureC: null,
            powerWatts: null,
          },
        ],
      },
      disks: {
        status: "ok",
        filesystems: [{id: "fs:/", status: "ok", usedBytes: 50, totalBytes: 100}],
        devices: [
          {id: "disk:sda", status: "ok", readBytesPerSec: 1024, writeBytesPerSec: 2048},
        ],
      },
      network: {
        status: "ok",
        interfaces: [
          {id: "net:eno1", status: "ok", rxBytesPerSec: 10, txBytesPerSec: 20},
        ],
      },
      ...overrides,
    },
  };
}

function delivery(): StatusMessage {
  return {
    type: "status",
    capturedAt: RECEIVED,
    delivery: {
      pendingRecords: 3,
      pendingBytes: 1500,
      oldestPendingCapturedAt: RECEIVED,
      inFlightRecords: 1,
      droppedRecords: 2,
      replaying: true,
      lastAckAt: RECEIVED,
    },
    failures: [{metric: "gpus", resourceId: "gpu:GPU-2", reason: "gpu reading stale"}],
  };
}

function machine(overrides: Partial<MachineState> = {}): MachineState {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    hostname: "rig-1",
    connected: true,
    lastReceivedAt: RECEIVED,
    expectedIntervalMs: 5_000,
    metrics: metrics(),
    inventory: inventory(),
    delivery: delivery(),
    displayName: "Render Rig",
    tags: ["gpu", "Rendering", "gpu"],
    ...overrides,
  };
}

test('machine identity and search use only existing display names and tags', () => {
  const rig = machine();
  const second = machine({
    machineId: "machine-2",
    hostname: "store",
    displayName: null,
    tags: undefined,
    metrics: undefined,
    inventory: undefined,
    delivery: undefined,
  });
  assert.equal(machineDisplayName(rig), "Render Rig");
  assert.equal(machineDisplayName(second), "store");
  assert.deepEqual(machineTags(rig), ["gpu", "Rendering"]);
  assert.deepEqual(machineTags(second), []);

  const machines = [rig, second];
  assert.equal(filterMachines(machines, "").length, 2);
  assert.deepEqual(filterMachines(machines, "render").map((item) => item.machineId), ["machine-1"]);
  assert.deepEqual(filterMachines(machines, "GPU rendering").map((item) => item.machineId), ["machine-1"]);
  assert.deepEqual(filterMachines(machines, "store").map((item) => item.machineId), ["machine-2"]);
  assert.deepEqual(filterMachines(machines, "missing"), []);
});

test('machine resource view exposes per-core, swap, GPUs, disks and network', () => {
  const view = machineResourceView(machine(), NOW);
  assert.equal(view.displayName, "Render Rig");
  assert.equal(view.freshness, "live");
  assert.equal(view.stale, false);

  assert.deepEqual(view.cpu.total, {state: "value", value: 50, stale: false});
  assert.deepEqual(view.cpu.perCore, {state: "value", value: [100, 0, null], stale: false});
  assert.deepEqual(view.memory, {
    state: "value",
    value: {usedBytes: 8, totalBytes: 16},
    stale: false,
  });
  assert.deepEqual(view.swap, {
    state: "value",
    value: {usedBytes: 2, totalBytes: 4},
    stale: false,
  });

  assert.equal(view.gpus.family.state, "available");
  assert.equal(view.gpus.cards.length, 3);
  assert.equal(view.gpus.partial, false);
  const first = view.gpus.cards.find((card) => card.id === "gpu:GPU-1");
  assert.ok(first);
  assert.equal(first.state, "ok");
  assert.equal(first.name, "NVIDIA GeForce RTX 3090");
  assert.equal(first.utilizationPercent, 90);
  assert.equal(first.memoryUsedBytes, 100);
  assert.equal(first.temperatureC, 60.4);
  assert.equal(first.powerWatts, 120.2);
  const aspeed = view.gpus.cards.find((card) => card.id === "pci:0000:44:00.0");
  assert.ok(aspeed);
  assert.equal(aspeed.state, "unsupported");
  assert.equal(aspeed.telemetry, "unsupported");
  assert.match(aspeed.reason ?? "", /management adapter; no telemetry/);
  assert.equal(aspeed.utilizationPercent, null);

  assert.equal(view.filesystems.items[0]!.state, "ok");
  assert.equal(view.filesystems.items[0]!.mountPoint, "/");
  assert.equal(view.filesystems.items[0]!.usedBytes, 50);
  assert.equal(view.disks.items[0]!.state, "ok");
  assert.equal(view.disks.items[0]!.readBytesPerSec, 1024);
  assert.equal(view.disks.items[0]!.model, "Fixture SSD");

  const eno = view.network.items.find((iface) => iface.name === "eno1");
  assert.ok(eno);
  assert.equal(eno.state, "ok");
  assert.equal(eno.rxBytesPerSec, 10);
  assert.deepEqual(eno.addresses, [{family: "ipv4", address: "192.0.2.5", prefix: 24}]);
  assert.equal(eno.virtual, false);
  const tailscale = view.network.items.find((iface) => iface.name === "tailscale0");
  assert.ok(tailscale);
  assert.equal(tailscale.virtual, true);
  assert.equal(tailscale.state, "inventory");
  assert.equal(tailscale.rxBytesPerSec, null);
});

test('stale readings are retained and labelled, never treated as live', () => {
  const view = machineResourceView(machine(), Date.parse(RECEIVED) + 20_000);
  assert.equal(view.freshness, "stale");
  assert.equal(view.stale, true);
  assert.equal(view.cpu.total.state === "value" && view.cpu.total.stale, true);
  assert.equal(view.memory.state === "value" && view.memory.stale, true);
  assert.ok(view.gpus.cards.every((card) => card.stale));
});

test('without a metrics record every family explains that nothing was reported', () => {
  const empty = machine({
    metrics: undefined,
    inventory: undefined,
    delivery: undefined,
    displayName: null,
    tags: undefined,
  });
  const view = machineResourceView(empty, NOW);
  assert.deepEqual(view.cpu.total, {state: "absent", reason: "No CPU reading has been received yet."});
  assert.deepEqual(view.cpu.perCore, {state: "absent", reason: "No CPU reading has been received yet; per-core values are unknown."});
  assert.deepEqual(view.memory, {state: "absent", reason: "No memory reading has been received yet."});
  assert.deepEqual(view.swap, {state: "absent", reason: "No swap reading has been received yet."});
  assert.equal(view.gpus.cards.length, 0);
  assert.equal(view.gpus.family.state, "absent");
  assert.equal(view.filesystems.items.length, 0);
  assert.equal(view.network.items.length, 0);
  const summary = fleetResourceSummary(empty, NOW);
  assert.equal(summary.cpu.state, "absent");
  assert.equal(summary.gpu, null);
  assert.equal(summary.disk, null);
  assert.equal(summary.network, null);
});

test('unavailable metric families preserve reasons instead of zeroes', () => {
  const unavailable = {
    status: "unavailable" as const,
    reason: "reader failed safely",
  };
  const broken = machine({
    metrics: metrics({
      gpus: {status: "unavailable", reason: "gpu reading stale"},
      disks: {status: "unavailable", reason: "disk statistics unavailable"},
      network: {status: "unavailable", reason: "network counters unavailable"},
    }),
  });
  const view = machineResourceView(broken, NOW);
  assert.equal(view.gpus.family.state, "unavailable");
  assert.equal((view.gpus.family as {reason: string}).reason, "gpu reading stale");
  assert.equal(view.gpus.cards.find((card) => card.id === "gpu:GPU-1")!.state, "unavailable");
  assert.equal(view.gpus.cards.find((card) => card.id === "gpu:GPU-1")!.utilizationPercent, null);
  assert.equal(view.filesystems.family.state, "unavailable");
  assert.equal(view.filesystems.items[0]!.state, "unavailable");
  assert.match(view.filesystems.items[0]!.reason ?? "", /disk statistics unavailable/);
  assert.equal(view.network.items[0]!.state, "unavailable");
  assert.equal(view.network.items[0]!.rxBytesPerSec, null);
  assert.equal(unavailable.status, "unavailable");
});

test('partial GPU failure is exposed without discarding working cards', () => {
  const partial = machine({
    metrics: metrics({
      gpus: {
        status: "ok",
        devices: [
          {
            id: "gpu:GPU-1",
            status: "ok",
            utilizationPercent: 90,
            memoryUsedBytes: 100,
            memoryTotalBytes: 200,
            temperatureC: 60,
            powerWatts: 120,
          },
          {
            id: "gpu:GPU-2",
            status: "unavailable",
            reason: "gpu reading stale",
          },
        ],
      },
    }),
  });
  const view = machineResourceView(partial, NOW);
  assert.equal(view.gpus.family.state, "available");
  assert.equal(view.gpus.partial, true);
  assert.equal(view.gpus.cards.filter((card) => card.state === "ok").length, 1);
  assert.equal(view.gpus.cards.find((card) => card.id === "gpu:GPU-2")!.state, "unavailable");

  const summary = fleetResourceSummary(partial, NOW);
  assert.ok(summary.gpu);
  assert.equal(summary.gpu!.partial, true);
  assert.equal(summary.gpu!.okCount, 1);
  assert.equal(summary.gpu!.supportedCount, 2);
  assert.equal(summary.gpu!.maxUtilizationPercent, 90);
  assert.equal(summary.gpu!.reason, null);
});

test('fleet summary exposes GPU/VRAM, capacity and network only when known', () => {
  const summary = fleetResourceSummary(machine(), NOW);
  assert.deepEqual(summary.cpu, {state: "value", value: 50, stale: false});
  assert.equal(summary.memory.state, "value");
  assert.ok(summary.gpu);
  assert.equal(summary.gpu!.count, 3);
  assert.equal(summary.gpu!.supportedCount, 2);
  assert.equal(summary.gpu!.maxUtilizationPercent, 90);
  assert.equal(summary.gpu!.memoryUsedBytes, 150);
  assert.equal(summary.gpu!.memoryTotalBytes, 300);
  assert.equal(summary.gpu!.reason, null);
  assert.equal(summary.disk!.maxUsedPercent, 50);
  assert.equal(summary.network!.rxBytesPerSec, 10);
  assert.equal(summary.network!.txBytesPerSec, 20);

  const withoutMetrics = fleetResourceSummary(
    machine({metrics: undefined, inventory: undefined, delivery: undefined}),
    NOW,
  );
  assert.equal(withoutMetrics.cpu.state, "absent");
  assert.equal(withoutMetrics.gpu, null);
  assert.equal(withoutMetrics.disk, null);
  assert.equal(withoutMetrics.network, null);
});

test('connections view contains only safe collector-facing facts', () => {
  const [view] = collectorConnectionViews([machine()], NOW);
  assert.ok(view);
  assert.equal(view.name, "Render Rig");
  assert.equal(view.connected, true);
  assert.equal(view.cadenceMs, 5_000);
  assert.equal(view.lastReceivedAt, RECEIVED);
  assert.equal(view.freshness, "live");
  assert.equal(view.delivery?.pendingRecords, 3);
  assert.equal(view.delivery?.pendingBytes, 1500);
  assert.equal(view.delivery?.inFlightRecords, 1);
  assert.equal(view.delivery?.droppedRecords, 2);
  assert.equal(view.delivery?.replaying, true);
  assert.equal(view.delivery?.lastAckAt, RECEIVED);
  assert.equal(view.deliveryCapturedAt, RECEIVED);
  assert.deepEqual(view.failures, [
    {metric: "gpus", resourceId: "gpu:GPU-2", reason: "gpu reading stale"},
  ]);
  assert.equal(view.capabilities?.length, 7);
  assert.deepEqual(
    view.capabilities?.map((capability) => capability.key),
    ["cpu", "memory", "swap", "gpus", "disks", "network", "processes"],
  );
  const serialized = JSON.stringify(view).toLowerCase();
  for (const forbidden of ["token", "ws://", "wss://", "authorization", "password", "destination"]) {
    assert.equal(serialized.includes(forbidden), false, `must not expose ${forbidden}`);
  }
  assert.deepEqual(
    Object.keys(view).sort(),
    [
      "cadenceMs",
      "capabilities",
      "collectorId",
      "connected",
      "delivery",
      "deliveryCapturedAt",
      "failures",
      "freshness",
      "lastReceivedAt",
      "machineId",
      "name",
      "thresholdMs",
    ].sort(),
  );
});

test('connections view keeps missing delivery and capability data unknown', () => {
  const [view] = collectorConnectionViews(
    [machine({delivery: undefined, inventory: undefined})],
    NOW,
  );
  assert.ok(view);
  assert.equal(view.delivery, null);
  assert.equal(view.deliveryCapturedAt, null);
  assert.deepEqual(view.failures, []);
  assert.equal(view.capabilities, null);
});

test('resource formatters keep null and missing values honest', () => {
  assert.equal(formatRate(1024 * 1024), "1.0 MiB/s");
  assert.equal(formatRate(null), "—");
  assert.equal(formatTemperature(60.4), "60 °C");
  assert.equal(formatTemperature(null), "—");
  assert.equal(formatPower(120.2), "120 W");
  assert.equal(formatPower(undefined), "—");
  assert.equal(formatSpeedMbps(1000), "1000 Mb/s");
  assert.equal(formatSpeedMbps(null), "—");
  assert.equal(formatCount(48), "48");
  assert.equal(formatCount(1_200_000), (1_200_000).toLocaleString());
  assert.equal(formatCount(null), "—");
});
