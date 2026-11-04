/**
 * Focused unit tests for the bottleneck History catalog, units and bounded
 * per-core selection. No fabricated telemetry: fixtures only describe stored
 * metric names and resource ids.
 */
import assert from "node:assert/strict";
import test from "node:test";

import type { MachineState } from "../src/contracts";
import { buildYDomain } from "../src/history/chart";
import {
  HISTORY_METRICS,
  HISTORY_METRIC_GROUPS,
  metricOption,
  metricSeriesStyle,
  metricUnitLabel,
  resourceLabel,
  visibleCapacityMetrics,
} from "../src/history/metrics";
import {
  MAX_VISIBLE_CORES,
  CORE_LINE_LIMIT,
  availableCoreIds,
  coreIndex,
  coreLabel,
  coreResourceId,
  remainingCoreIds,
  resolveCoreSelection,
} from "../src/history/cores";
import {
  formatAxisValue,
  formatMetricValue,
} from "../src/history/presentation";
import type { HistoryResult } from "../src/hub/storage/history-query";
import { buildHistoryPanels } from "../src/components/history/history-panels";

function machineWithCores(logicalCores: number): MachineState {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    hostname: "rig",
    connected: true,
    lastReceivedAt: null,
    inventory: {
      cpu: { model: null, logicalCores },
      gpus: [],
      filesystems: [
        { id: "fs:/", mountPoint: "/", device: "/dev/sda1", fsType: "ext4", totalBytes: 100 },
      ],
      disks: [{ id: "disk:nvme0", name: "nvme0n1", model: null, rotational: false, sizeBytes: 100 }],
      network: [],
      capabilities: {
        cpu: { status: "supported" },
        memory: { status: "supported" },
        swap: { status: "supported" },
        gpus: { status: "supported" },
        disks: { status: "supported" },
        network: { status: "supported" },
        processes: { status: "supported" },
      },
    },
    metrics: {
      collectorId: "collector-1",
      runId: "run-1",
      bootId: "boot-1",
      sequence: 1,
      capturedAt: "2026-11-02T12:00:00.000Z",
      receivedAt: "2026-11-02T12:00:00.000Z",
      kind: "metrics",
      payload: {
        cpu: {
          status: "ok",
          usagePercent: 10,
          perCore: Array.from({ length: logicalCores }, () => 10),
        },
        memory: { status: "ok", usedBytes: 1, totalBytes: 2 },
        swap: { status: "ok", usedBytes: 0, totalBytes: 0 },
        gpus: { status: "ok", devices: [] },
        disks: { status: "ok", filesystems: [], devices: [] },
        network: { status: "ok", interfaces: [] },
      },
    },
  } as unknown as MachineState;
}

test("bottleneck metric catalog carries the documented names, units and groups", () => {
  const expected: Record<string, string> = {
    "cpu.temperature": "celsius",
    "cpu.power": "watts",
    "cpu.iowait": "percent",
    "cpu.pressure": "percent",
    "cpu.core": "percent",
    "memory.usage": "percent",
    "memory.pressure.some": "percent",
    "memory.pressure.full": "percent",
    "io.pressure.some": "percent",
    "io.pressure.full": "percent",
    "fs.usage": "percent",
    "fs.inodes.usage": "percent",
    "disk.read.latency": "milliseconds",
    "disk.write.latency": "milliseconds",
    "disk.utilization": "percent",
  };
  for (const [metric, unit] of Object.entries(expected)) {
    const option = metricOption(metric);
    assert.ok(option, `${metric} is in the catalog`);
    assert.equal(option.unit, unit, `${metric} uses ${unit}`);
  }

  const names = HISTORY_METRICS.map((option) => option.metric);
  assert.equal(new Set(names).size, names.length, "metric names are unique");
  const grouped = new Set(HISTORY_METRIC_GROUPS.flatMap((group) => group.metrics));
  for (const name of names) {
    assert.ok(grouped.has(name), `${name} is reachable from a control group`);
  }
  for (const group of HISTORY_METRIC_GROUPS) {
    for (const metric of group.metrics) {
      assert.ok(metricOption(metric), `group ${group.id} references a catalog metric`);
    }
  }
});

test("milliseconds format and scale honestly for latency charts", () => {
  assert.equal(formatMetricValue(0.42, "milliseconds"), "0.42 ms");
  assert.equal(formatMetricValue(12.34, "milliseconds"), "12.3 ms");
  assert.equal(formatMetricValue(null, "milliseconds"), "—");
  assert.equal(formatAxisValue(0.4, "milliseconds"), "0.4 ms");
  assert.equal(formatAxisValue(12.34, "milliseconds"), "12 ms");
  assert.equal(metricUnitLabel("milliseconds"), "ms");

  const domain = buildYDomain("milliseconds", [0.4, 1.2]);
  assert.equal(domain.min, 0, "latency is baselined at zero");
  assert.ok(domain.max >= 1.2);
});

test("resource labels use filesystem mounts, disk names and core numbers", () => {
  const machine = machineWithCores(16);
  assert.equal(resourceLabel(machine, "fs.usage", "fs:/"), "/");
  assert.equal(resourceLabel(machine, "fs.inodes.usage", "fs:/"), "/");
  assert.equal(resourceLabel(machine, "disk.read.latency", "disk:nvme0"), "nvme0n1");
  assert.equal(resourceLabel(machine, "disk.utilization", "disk:nvme0"), "nvme0n1");
  assert.equal(resourceLabel(machine, "cpu.core", "core:12"), "Core 12");
  assert.equal(resourceLabel(machine, "io.pressure.some", ""), "I/O some");
});

test("per-core selection defaults to all and supports arbitrary subsets up to the contract limit", () => {
  const machine = machineWithCores(1024);
  const available = availableCoreIds(machine);
  assert.equal(available.length, 1024);

  const defaults = resolveCoreSelection(null, available);
  assert.equal(defaults.length, MAX_VISIBLE_CORES);
  assert.deepEqual(defaults, Array.from({ length: 1024 }, (_, index) => `core:${index}`));

  const explicit = resolveCoreSelection(
    Array.from({ length: 40 }, (_, index) => coreResourceId(index + 100)),
    available,
  );
  assert.equal(explicit.length, 40);
  assert.deepEqual(explicit, Array.from({ length: 40 }, (_, index) => `core:${index + 100}`));

  assert.deepEqual(
    resolveCoreSelection(["core:9", "core:3", "core:9"], available),
    ["core:3", "core:9"],
    "duplicates collapse and cores sort numerically",
  );
  assert.deepEqual(
    resolveCoreSelection(["core:2000", "unknown"], available),
    [],
    "out-of-range or non-core ids are ignored",
  );
  assert.deepEqual(resolveCoreSelection([], available), [], "an explicit empty selection is respected");
});

test("stored history can add cores beyond the current inventory without unbounded output", () => {
  const machine = machineWithCores(4);
  const series = [
    { metric: "cpu.core", resourceId: "core:40", unit: "percent", points: [] },
    { metric: "cpu.core", resourceId: "core:41", unit: "percent", points: [] },
    { metric: "cpu.usage", resourceId: "", unit: "percent", points: [] },
  ];
  const available = availableCoreIds(machine, series);
  assert.deepEqual(available, ["core:0", "core:1", "core:2", "core:3", "core:40", "core:41"]);
  assert.deepEqual(resolveCoreSelection(["core:40"], available), ["core:40"]);
  assert.deepEqual(remainingCoreIds(available, ["core:0"]), [
    "core:1",
    "core:2",
    "core:3",
    "core:40",
    "core:41",
  ]);
});

test("core ids parse safely and labels stay readable", () => {
  assert.equal(coreIndex("core:0"), 0);
  assert.equal(coreIndex("core:1023"), 1023);
  assert.equal(coreIndex("core:1024"), null);
  assert.equal(coreIndex("cpu:0"), null);
  assert.equal(coreIndex("core:01"), 1);
  assert.equal(coreLabel("core:12"), "Core 12");
  assert.equal(coreLabel("cpu:0"), "cpu:0");
});

test("eight core lines stay distinguishable even though the palette repeats", () => {
  const option = metricOption("cpu.core")!;
  const styles = Array.from({ length: CORE_LINE_LIMIT }, (_, index) =>
    metricSeriesStyle(option, index),
  );
  const signatures = new Set(styles.map((style) => `${style.color}|${style.dash ?? "solid"}`));
  assert.equal(signatures.size, CORE_LINE_LIMIT);
});

test("History panels keep bottleneck units separate and bound per-core series", () => {
  const point = { t: 1_000, min: 1, max: 2, avg: 1.5 };
  const result: HistoryResult = {
    machineId: "machine-1",
    fromMs: 0,
    toMs: 2_000,
    stepMs: 1_000,
    tier: "raw",
    gaps: [],
    truncated: false,
    elapsedMs: 1,
    series: [
      { metric: "cpu.core", resourceId: "core:3", unit: "percent", points: [point] },
      { metric: "cpu.core", resourceId: "core:9", unit: "percent", points: [point] },
      { metric: "cpu.temperature", resourceId: "", unit: "celsius", points: [point] },
      { metric: "memory.pressure.some", resourceId: "", unit: "percent", points: [point] },
      { metric: "memory.pressure.full", resourceId: "", unit: "percent", points: [point] },
      { metric: "fs.inodes.usage", resourceId: "fs:/", unit: "percent", points: [point] },
      { metric: "disk.read.latency", resourceId: "disk:nvme0", unit: "milliseconds", points: [point] },
    ],
  };

  const panels = buildHistoryPanels({
    machine: machineWithCores(64),
    result,
    selectedMetrics: [
      "cpu.core",
      "cpu.temperature",
      "memory.pressure.some",
      "memory.pressure.full",
      "fs.inodes.usage",
      "disk.read.latency",
    ],
    hiddenCards: new Set(),
    coreIds: ["core:3", "core:9"],
  });

  const cpuPanel = panels.find((panel) => panel.key === "cpu");
  assert.ok(cpuPanel);
  assert.equal(cpuPanel.charts.length, 2, "different units do not share one chart");
  const coreChart = cpuPanel.charts.find((chart) => chart.key === "cpu.core")!;
  assert.equal(coreChart.title, "Per-core usage · 2 of 64");
  assert.deepEqual(coreChart.series.map((series) => series.label), ["Core 3", "Core 9"]);
  const temperatureChart = cpuPanel.charts.find((chart) => chart.key === "cpu.temperature")!;
  assert.equal(temperatureChart.unit, "celsius");

  const memoryPanel = panels.find((panel) => panel.key === "memory")!;
  const pressureChart = memoryPanel.charts.find((chart) => chart.key === "memory.pressure")!;
  assert.deepEqual(pressureChart.series.map((series) => series.label), ["Some", "Full"]);
  assert.equal(pressureChart.unit, "percent");

  const filesystemPanel = panels.find((panel) => panel.key === "filesystems")!;
  const inodeChart = filesystemPanel.charts.find((chart) => chart.key === "fs.inodes.usage")!;
  assert.equal(inodeChart.unit, "percent");
  assert.deepEqual(inodeChart.series.map((series) => series.label), ["/"]);

  const diskHealth = panels.find((panel) => panel.key === "disk-health")!;
  const latencyChart = diskHealth.charts.find((chart) => chart.key === "disk.latency")!;
  assert.equal(latencyChart.unit, "milliseconds");
  assert.deepEqual(latencyChart.series.map((series) => series.label), ["nvme0n1 read latency"]);
});


test("capacity charts switch stored units and keep swap in the memory section", () => {
  const metrics = ["memory.used", "memory.usage", "swap.used", "fs.used", "fs.usage"];
  const result: HistoryResult = {
    machineId: "machine-1", fromMs: 0, toMs: 2_000, stepMs: 1_000,
    tier: "raw", gaps: [], truncated: false, elapsedMs: 1,
    series: metrics.map((metric, index) => ({
      metric, resourceId: metric.startsWith("fs.") ? "fs:/" : "",
      unit: metricOption(metric)!.unit,
      points: [{ t: 1_000, min: index + 1, max: index + 1, avg: index + 1 }],
    })),
  };
  const build = (selectedMetrics: string[]) => buildHistoryPanels({
    machine: machineWithCores(8), result, selectedMetrics, hiddenCards: new Set(),
  });
  const panels = build(metrics);
  const memory = panels.find((panel) => panel.key === "memory")!;
  assert.deepEqual(memory.charts.map((chart) => chart.key), ["memory.capacity", "swap.used"]);
  const views = memory.charts[0]!.views!;
  assert.deepEqual(views.map((view) => view.unit), ["bytes", "percent"]);
  assert.equal(views[1]!.series[0]!.points, result.series[1]!.points,
    "percentages use the historical series, never today's memory capacity");
  assert.equal(panels.find((panel) => panel.key === "filesystems")!.charts.length, 1);
  assert.equal(build(["memory.usage"])[0]!.charts[0]!.unit, "percent");
  assert.equal(build(["swap.used"])[0]!.title, "Memory");

  assert.deepEqual(visibleCapacityMetrics(metrics, { memory: "percent", filesystem: "bytes" }),
    ["memory.usage", "swap.used", "fs.used"], "exports match the displayed unit");
  assert.deepEqual(visibleCapacityMetrics(["memory.usage", "fs.used"], { memory: "bytes", filesystem: "percent" }),
    ["memory.usage", "fs.used"], "a sole selected unit overrides a saved preference");
});

test("CPU usage and stalls share percentages while temperature and power stay separate", () => {
  const metrics = ["cpu.usage", "cpu.iowait", "cpu.pressure", "cpu.temperature", "cpu.power"];
  const result: HistoryResult = {
    machineId: "machine-1", fromMs: 0, toMs: 2_000, stepMs: 1_000,
    tier: "raw", gaps: [], truncated: false, elapsedMs: 1,
    series: metrics.map((metric) => ({ metric, resourceId: "", unit: metricOption(metric)!.unit, points: [] })),
  };
  const [cpu] = buildHistoryPanels({ machine: machineWithCores(8), result, selectedMetrics: metrics, hiddenCards: new Set() });
  assert.deepEqual(cpu!.charts.map((chart) => chart.unit), ["percent", "celsius", "watts"]);
  assert.deepEqual(cpu!.charts[0]!.series.map((series) => series.metric), metrics.slice(0, 3));
});


test("selected cores without stored readings remain explicit empty series", () => {
  const result: HistoryResult = {
    machineId: "machine-1", fromMs: 0, toMs: 2_000, stepMs: 1_000,
    tier: "raw", gaps: [], truncated: false, elapsedMs: 1,
    series: [{ metric: "cpu.core", resourceId: "core:0", unit: "percent", points: [{ t: 1_000, min: 10, max: 100, avg: 50 }] }],
  };
  const [cpu] = buildHistoryPanels({ machine: machineWithCores(48), result, selectedMetrics: ["cpu.core"], hiddenCards: new Set() });
  assert.equal(cpu!.charts[0]!.series.length, 48);
  assert.deepEqual(cpu!.charts[0]!.series[47]!.points, [], "missing readings are not fabricated or silently removed");
});
