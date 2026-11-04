/** Warning preset applicability, derived percentages, resources and rule inputs. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  ALERT_METRICS,
  alertMetricFamily,
  alertMetricNeedsResource,
  alertMetricUnit,
} from "../src/alerts/types";
import { formatMetricValue } from "../src/alerts/format";
import {
  ALERT_PRESETS,
  alertPresetById,
  resolveAlertPreset,
  type AlertPreset,
} from "../src/alerts/presets";
import type { Inventory, MachineState, MetricsPayload } from "../src/contracts";
import { metricsRecord } from "./alerts-fixtures";

type PayloadInput = {
  cpuStatus?: "ok" | "unavailable";
  disksStatus?: "ok" | "unavailable";
  cpu?: {
    temperatureC?: number | null;
    powerWatts?: number | null;
    iowaitPercent?: number | null;
    pressureSomePercent?: number | null;
  };
  memory?: {
    usedBytes?: number;
    totalBytes?: number;
    pressureSomePercent?: number | null;
    pressureFullPercent?: number | null;
  };
  gpu?: {id?: string; temperatureC?: number | null};
  disks?: {pressureSomePercent?: number | null; pressureFullPercent?: number | null};
  filesystem?: {
    id?: string;
    usedBytes?: number;
    totalBytes?: number;
    inodesUsed?: number | null;
    inodesTotal?: number | null;
  };
  disk?: {
    id?: string;
    ioUtilizationPercent?: number | null;
    readLatencyMs?: number | null;
    writeLatencyMs?: number | null;
  };
};

function payload(input: PayloadInput = {}): MetricsPayload {
  const filesystem = {
    id: input.filesystem?.id ?? "fs:/",
    status: "ok",
    usedBytes: input.filesystem?.usedBytes ?? 50,
    totalBytes: input.filesystem?.totalBytes ?? 100,
    ...(input.filesystem?.inodesUsed === undefined ? {} : {inodesUsed: input.filesystem.inodesUsed}),
    ...(input.filesystem?.inodesTotal === undefined ? {} : {inodesTotal: input.filesystem.inodesTotal}),
  };
  const disk = {
    id: input.disk?.id ?? "disk:sda",
    status: "ok",
    readBytesPerSec: 1,
    writeBytesPerSec: 2,
    ...(input.disk?.ioUtilizationPercent === undefined
      ? {}
      : {ioUtilizationPercent: input.disk.ioUtilizationPercent}),
    ...(input.disk?.readLatencyMs === undefined ? {} : {readLatencyMs: input.disk.readLatencyMs}),
    ...(input.disk?.writeLatencyMs === undefined ? {} : {writeLatencyMs: input.disk.writeLatencyMs}),
  };
  return {
    cpu:
      input.cpuStatus === "unavailable"
        ? {status: "unavailable", reason: "cpu unavailable"}
        : {
            status: "ok",
            usagePercent: 50,
            perCore: [50, 50],
            ...input.cpu,
          },
    memory: {
      status: "ok",
      usedBytes: input.memory?.usedBytes ?? 8,
      totalBytes: input.memory?.totalBytes ?? 16,
      ...(input.memory?.pressureSomePercent === undefined
        ? {}
        : {pressureSomePercent: input.memory.pressureSomePercent}),
      ...(input.memory?.pressureFullPercent === undefined
        ? {}
        : {pressureFullPercent: input.memory.pressureFullPercent}),
    },
    swap: {status: "ok", usedBytes: 2, totalBytes: 4},
    gpus: {
      status: "ok",
      devices: [
        {
          id: input.gpu?.id ?? "gpu:GPU-1",
          status: "ok",
          utilizationPercent: 40,
          memoryUsedBytes: 100,
          memoryTotalBytes: 200,
          temperatureC: input.gpu?.temperatureC === undefined ? 60 : input.gpu.temperatureC,
          powerWatts: 100,
        },
      ],
    },
    disks:
      input.disksStatus === "unavailable"
        ? {status: "unavailable", reason: "disk unavailable"}
        : {
            status: "ok",
            filesystems: [filesystem],
            devices: [disk],
            ...input.disks,
          },
    network: {status: "ok", interfaces: []},
  } as unknown as MetricsPayload;
}

function inventory(input: Partial<Inventory> = {}): Inventory {
  const supported = {status: "supported" as const};
  return {
    os: {platform: "linux", release: "6.0", distro: null, arch: "x64"},
    cpu: {model: null, logicalCores: 4},
    memory: {totalBytes: 16, swapTotalBytes: 4},
    gpus: [
      {
        id: "gpu:GPU-1",
        name: "RTX 3090",
        vendor: "nvidia",
        pciBusId: null,
        telemetry: "supported",
        memoryTotalBytes: 100,
      },
    ],
    filesystems: [
      {id: "fs:/", mountPoint: "/", device: "/dev/sda1", fsType: "ext4", totalBytes: 100},
    ],
    disks: [{id: "disk:sda", name: "sda", model: null, rotational: false, sizeBytes: 1_000}],
    network: [],
    capabilities: {
      cpu: supported,
      memory: supported,
      swap: supported,
      gpus: supported,
      disks: supported,
      network: supported,
      processes: supported,
    },
    ...input,
  };
}

function machine(
  input: {
    machineId?: string;
    payload?: MetricsPayload;
    inventory?: Inventory | null;
    noMetrics?: boolean;
  } = {},
): MachineState {
  const record = metricsRecord(input.payload ?? payload(), {sequence: 1});
  return {
    machineId: input.machineId ?? "m1",
    collectorId: "collector-1",
    hostname: "host-1",
    connected: true,
    lastReceivedAt: record.capturedAt,
    ...(input.noMetrics ? {} : {metrics: {...record, receivedAt: record.capturedAt}}),
    ...(input.inventory === null ? {} : {inventory: input.inventory ?? inventory()}),
  };
}

function preset(id: string): AlertPreset {
  const found = alertPresetById(id);
  assert.ok(found, `missing preset ${id}`);
  return found;
}

test("alert catalog carries the documented bottleneck metrics and units", () => {
  const expected: Array<[keyof typeof ALERT_METRICS, string, string]> = [
    ["cpu.temperature", "celsius", "none"],
    ["cpu.power", "watts", "none"],
    ["cpu.iowait", "percent", "none"],
    ["cpu.pressure", "percent", "none"],
    ["memory.usage", "percent", "none"],
    ["memory.pressure.some", "percent", "none"],
    ["memory.pressure.full", "percent", "none"],
    ["io.pressure.some", "percent", "none"],
    ["io.pressure.full", "percent", "none"],
    ["fs.usage", "percent", "required"],
    ["fs.inodes.usage", "percent", "required"],
    ["disk.read.latency", "milliseconds", "required"],
    ["disk.write.latency", "milliseconds", "required"],
    ["disk.utilization", "percent", "required"],
  ];
  for (const [metric, unit, resource] of expected) {
    assert.equal(ALERT_METRICS[metric].unit, unit, `${metric} unit`);
    assert.equal(ALERT_METRICS[metric].resource, resource, `${metric} resource rule`);
  }
  assert.equal(alertMetricFamily("io.pressure.some"), "disks");
  assert.equal(alertMetricFamily("fs.inodes.usage"), "disks");
  assert.equal(alertMetricFamily("memory.pressure.some"), "memory");
  assert.equal(alertMetricFamily("cpu.temperature"), "cpu");
});

test("derived percentage metrics use 0–100 values and readable units", () => {
  assert.equal(alertMetricUnit("memory.usage"), "percent");
  assert.equal(alertMetricUnit("fs.usage"), "percent");
  assert.equal(alertMetricUnit("fs.inodes.usage"), "percent");
  assert.equal(alertMetricUnit("io.pressure.some"), "percent");
  assert.equal(formatMetricValue("memory.usage", 91.5), "91.5%");
  assert.equal(formatMetricValue("fs.usage", 90), "90%");
  assert.equal(formatMetricValue("fs.inodes.usage", 100), "100%");
  assert.equal(formatMetricValue("disk.read.latency", 12.34), "12.3 ms");
  assert.equal(formatMetricValue("cpu.temperature", 90), "90 °C");
  assert.equal(alertMetricNeedsResource("fs.usage"), true);
  assert.equal(alertMetricNeedsResource("memory.usage"), false);
  assert.equal(preset("memory-nearly-full").threshold, 90);
  assert.equal(preset("disk-nearly-full").threshold, 90);
});

test("every preset matches the catalog's metric and resource rules", () => {
  for (const item of ALERT_PRESETS) {
    assert.ok(ALERT_METRICS[item.metric], `${item.id} metric`);
    assert.equal(alertMetricNeedsResource(item.metric), item.resourceKind !== "none");
    assert.equal(item.condition === "stale", item.threshold === null);
    assert.ok(item.durationMs >= 0);
    assert.ok(item.description.length > 0);
  }
});

test("sustained high CPU fills editable fields without creating a rule", () => {
  const resolved = resolveAlertPreset(preset("sustained-high-cpu"), machine());
  assert.equal(resolved.status.kind, "ready");
  assert.ok(resolved.input);
  assert.deepEqual(resolved.input, {
    name: "Sustained high CPU",
    machineId: "m1",
    metric: "cpu.usage",
    resourceId: "",
    condition: "above",
    threshold: 90,
    durationMs: 300_000,
    enabled: true,
  });
  assert.equal("id" in resolved.input, false, "presets never carry a saved rule id");
});

test("memory and filesystem presets use percentages and the selected machine's resources", () => {
  const selected = machine({
    payload: payload({filesystem: {inodesUsed: 10, inodesTotal: 100}}),
    inventory: inventory({
      filesystems: [
        {
          id: "fs:/data",
          mountPoint: "/data",
          device: "/dev/sdb1",
          fsType: "ext4",
          totalBytes: 100,
        },
        {id: "fs:/", mountPoint: "/", device: "/dev/sda1", fsType: "ext4", totalBytes: 100},
      ],
    }),
  });

  const memory = resolveAlertPreset(preset("memory-nearly-full"), selected);
  assert.ok(memory.input);
  assert.equal(memory.input.metric, "memory.usage");
  assert.equal(memory.input.resourceId, "");
  assert.equal(memory.input.threshold, 90);

  const disk = resolveAlertPreset(preset("disk-nearly-full"), selected);
  assert.ok(disk.input);
  assert.equal(disk.input.metric, "fs.usage");
  assert.equal(disk.input.resourceId, "fs:/", "root filesystem is the default");
  assert.deepEqual(
    disk.resources.map((resource) => resource.value),
    ["fs:/data", "fs:/"],
    "resources come from the selected machine only",
  );

  const inodes = resolveAlertPreset(preset("inode-capacity-low"), selected);
  assert.ok(inodes.input);
  assert.equal(inodes.input.metric, "fs.inodes.usage");
  assert.equal(inodes.input.resourceId, "fs:/");
});

test("filesystem presets stay scoped to the selected machine and report missing resources", () => {
  const onlyData = machine({
    machineId: "machine-a",
    payload: payload({filesystem: {id: "fs:/data"}}),
    inventory: inventory({
      filesystems: [
        {
          id: "fs:/data",
          mountPoint: "/data",
          device: "/dev/sdb1",
          fsType: "xfs",
          totalBytes: 100,
        },
      ],
    }),
  });
  const resolved = resolveAlertPreset(preset("disk-nearly-full"), onlyData);
  assert.ok(resolved.input);
  assert.equal(resolved.input.machineId, "machine-a");
  assert.equal(resolved.input.resourceId, "fs:/data");
  assert.deepEqual(
    resolved.resources.map((resource) => resource.value),
    ["fs:/data"],
  );

  const noFilesystems = resolveAlertPreset(
    preset("disk-nearly-full"),
    machine({inventory: inventory({filesystems: []})}),
  );
  assert.equal(noFilesystems.status.kind, "unavailable");
  assert.match(noFilesystems.status.reason, /No filesystems/);
  assert.equal(noFilesystems.input, null);
});

test("inode presets are unavailable until inode counts are reported", () => {
  const missing = resolveAlertPreset(preset("inode-capacity-low"), machine());
  assert.equal(missing.status.kind, "unavailable");
  assert.match(missing.status.reason, /Inode usage/);

  const ready = resolveAlertPreset(
    preset("inode-capacity-low"),
    machine({payload: payload({filesystem: {inodesUsed: 95, inodesTotal: 100}})}),
  );
  assert.ok(ready.input);
  assert.equal(ready.input.metric, "fs.inodes.usage");
  assert.equal(ready.input.threshold, 90);
});

test("GPU hot ignores management adapters and labels missing temperature honestly", () => {
  const selected = machine({
    inventory: inventory({
      gpus: [
        {
          id: "pci:aspeed",
          name: "ASPEED management VGA",
          vendor: "aspeed",
          pciBusId: null,
          telemetry: "unsupported",
          memoryTotalBytes: null,
        },
        {
          id: "gpu:GPU-1",
          name: "RTX 3090",
          vendor: "nvidia",
          pciBusId: null,
          telemetry: "supported",
          memoryTotalBytes: 100,
        },
      ],
    }),
    payload: payload({gpu: {id: "gpu:GPU-1", temperatureC: 70}}),
  });
  const ready = resolveAlertPreset(preset("gpu-hot"), selected);
  assert.ok(ready.input);
  assert.equal(ready.input.resourceId, "gpu:GPU-1");
  assert.equal(ready.input.threshold, 85);
  assert.deepEqual(
    ready.resources.map((resource) => resource.value),
    ["gpu:GPU-1"],
  );

  const noTemperature = resolveAlertPreset(
    preset("gpu-hot"),
    machine({payload: payload({gpu: {id: "gpu:GPU-1", temperatureC: null}})}),
  );
  assert.equal(noTemperature.status.kind, "unavailable");
  assert.match(noTemperature.status.reason, /GPU temperature/);
  assert.equal(noTemperature.input, null);
});

test("CPU hot is unavailable when the collector omits temperature", () => {
  const ready = resolveAlertPreset(
    preset("cpu-hot"),
    machine({payload: payload({cpu: {temperatureC: 71}})}),
  );
  assert.ok(ready.input);
  assert.equal(ready.input.metric, "cpu.temperature");
  assert.equal(ready.input.threshold, 90);
  assert.equal(ready.input.durationMs, 60_000);

  const absent = resolveAlertPreset(
    preset("cpu-hot"),
    machine({payload: payload({cpu: {temperatureC: null}})}),
  );
  assert.equal(absent.status.kind, "unavailable");
  assert.match(absent.status.reason, /CPU temperature/);
  assert.equal(absent.input, null);
});

test("presets wait for the first sample and honor unsupported metric families", () => {
  const waiting = resolveAlertPreset(preset("cpu-hot"), machine({noMetrics: true}));
  assert.equal(waiting.status.kind, "waiting");
  assert.match(waiting.status.reason, /first metrics sample/);
  assert.equal(waiting.input, null);

  const unsupported = machine({
    noMetrics: true,
    inventory: inventory({
      capabilities: {
        ...inventory().capabilities,
        cpu: {status: "unsupported", reason: "not shared"},
      },
    }),
  });
  const cpu = resolveAlertPreset(preset("sustained-high-cpu"), unsupported);
  assert.equal(cpu.status.kind, "unavailable");
  assert.match(cpu.status.reason, /CPU metrics/);
  assert.equal(resolveAlertPreset(preset("collector-offline"), unsupported).status.kind, "ready");
});

test("offline collector uses stale semantics without any sample", () => {
  const resolved = resolveAlertPreset(
    preset("collector-offline"),
    machine({noMetrics: true, inventory: null}),
  );
  assert.ok(resolved.input);
  assert.equal(resolved.input.condition, "stale");
  assert.equal(resolved.input.metric, "cpu.usage");
  assert.equal(resolved.input.resourceId, "");
  assert.equal(resolved.input.threshold, null);
  assert.equal(resolved.input.durationMs, 0);
});

test("pressure presets appear only when the collector reports the reading", () => {
  const memory = resolveAlertPreset(
    preset("memory-pressure"),
    machine({payload: payload({memory: {pressureSomePercent: 12}})}),
  );
  assert.ok(memory.input);
  assert.equal(memory.input.metric, "memory.pressure.some");
  assert.equal(memory.input.threshold, 10);
  const memoryMissing = resolveAlertPreset(preset("memory-pressure"), machine());
  assert.equal(memoryMissing.status.kind, "unavailable");
  assert.match(memoryMissing.status.reason, /Memory pressure/);

  const io = resolveAlertPreset(
    preset("io-pressure"),
    machine({payload: payload({disks: {pressureSomePercent: 15}})}),
  );
  assert.ok(io.input);
  assert.equal(io.input.metric, "io.pressure.some");
  const ioMissing = resolveAlertPreset(preset("io-pressure"), machine());
  assert.equal(ioMissing.status.kind, "unavailable");
  assert.match(ioMissing.status.reason, /I\/O pressure/);

  const cpu = resolveAlertPreset(
    preset("cpu-pressure"),
    machine({payload: payload({cpu: {pressureSomePercent: 11}})}),
  );
  assert.ok(cpu.input);
  assert.equal(cpu.input.metric, "cpu.pressure");
});

test("disk presets select the machine's disk and use percent and millisecond units", () => {
  const selected = machine({
    payload: payload({disk: {ioUtilizationPercent: 95, readLatencyMs: 120}}),
    inventory: inventory({
      disks: [{id: "disk:sda", name: "sda", model: null, rotational: false, sizeBytes: 1_000}],
    }),
  });
  const busy = resolveAlertPreset(preset("disk-busy"), selected);
  assert.ok(busy.input);
  assert.equal(busy.input.metric, "disk.utilization");
  assert.equal(busy.input.resourceId, "disk:sda");

  const slow = resolveAlertPreset(preset("slow-disk-reads"), selected);
  assert.ok(slow.input);
  assert.equal(slow.input.metric, "disk.read.latency");
  assert.equal(slow.input.threshold, 100);

  const missing = resolveAlertPreset(preset("slow-disk-reads"), machine());
  assert.equal(missing.status.kind, "unavailable");
  assert.match(missing.status.reason, /read latency/i);
});

test("no selected machine means no preset input", () => {
  const resolved = resolveAlertPreset(preset("sustained-high-cpu"), undefined);
  assert.equal(resolved.status.kind, "unavailable");
  assert.match(resolved.status.reason, /Choose a machine/);
  assert.equal(resolved.input, null);
});
