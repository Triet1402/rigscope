/**
 * Unit tests for the browser-safe History presentation layer.
 *
 * Covers value/unit formatting, range policy, response validation, gap-aware
 * segmentation, envelope-preserving downsampling, Y domains, and the
 * machine-aware metric defaults and resource labels used by the page.
 */
import assert from "node:assert/strict";
import test from "node:test";

import type { MachineState } from "../src/contracts";
import type { HistoryGap, HistoryResult, HistorySeries } from "../src/hub/storage/history-query";
import {
  buildYDomain,
  continuityStepMs,
  downsamplePoints,
  gapBands,
  seriesStats,
  splitSeries,
  timeTicks,
} from "../src/history/chart";
import {
  defaultMetricsFor,
  gpuCards,
  metricAvailability,
  metricOption,
  metricSeriesStyle,
  resourceLabel,
} from "../src/history/metrics";
import {
  formatAxisValue,
  formatGapDuration,
  formatMetricValue,
  gapSummary,
  historyRequestUrl,
  parseHistoryResult,
  rangeBounds,
  refreshIntervalMs,
} from "../src/history/presentation";

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type Point = HistorySeries["points"][number];

function point(t: number, avg: number | null, spread = 1): Point {
  return avg === null
    ? { t, min: null, max: null, avg: null }
    : { t, min: avg - spread, max: avg + spread, avg };
}

function series(metric: string, resourceId: string, points: Point[], unit = "percent"): HistorySeries {
  return { metric, resourceId, unit, points };
}

function result(overrides: Partial<HistoryResult> = {}): HistoryResult {
  return {
    machineId: "machine-1",
    fromMs: 1_000,
    toMs: 5_000,
    stepMs: 1_000,
    tier: "raw",
    series: [series("cpu.usage", "", [point(1_000, 10), point(2_000, 20)])],
    gaps: [],
    truncated: false,
    elapsedMs: 2,
    ...overrides,
  };
}

const INVENTORY = {
  os: { platform: "linux", release: "6.1", distro: null, arch: "x64" },
  cpu: { model: "Test CPU", logicalCores: 8 },
  memory: { totalBytes: 16 * 1024 ** 3, swapTotalBytes: 0 },
  gpus: [
    {
      id: "gpu:0",
      name: "RTX 3090",
      vendor: "nvidia" as const,
      pciBusId: "0000:01:00.0",
      telemetry: "supported" as const,
      memoryTotalBytes: 24 * 1024 ** 3,
    },
    {
      id: "gpu:aspeed",
      name: "ASPEED",
      vendor: "aspeed" as const,
      pciBusId: null,
      telemetry: "unsupported" as const,
      memoryTotalBytes: null,
    },
  ],
  filesystems: [
    { id: "fs:/", mountPoint: "/", device: "/dev/sda1", fsType: "ext4", totalBytes: 100 },
  ],
  disks: [{ id: "disk:nvme0", name: "nvme0n1", model: "Fast", rotational: false, sizeBytes: 100 }],
  network: [
    {
      id: "net:eth0",
      name: "eth0",
      mac: null,
      addresses: [],
      virtual: false,
      state: "up" as const,
      speedMbps: 1000,
    },
  ],
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

const MACHINE: MachineState = {
  machineId: "machine-1",
  collectorId: "collector-1",
  hostname: "rig",
  connected: true,
  lastReceivedAt: new Date(5_000).toISOString(),
  inventory: INVENTORY,
};

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

test("formats values with units and never turns a missing reading into zero", () => {
  assert.equal(formatMetricValue(null, "percent"), "—");
  assert.equal(formatMetricValue(undefined, "bytes"), "—");
  assert.equal(formatMetricValue(Number.NaN, "watts"), "—");
  assert.equal(formatMetricValue(42.34, "percent"), "42.3%");
  assert.equal(formatMetricValue(1536, "bytes"), "1.5 KiB");
  assert.equal(formatMetricValue(1.5 * 1024 ** 3, "bytes"), "1.5 GiB");
  assert.equal(formatMetricValue(2 * 1024 ** 2, "bytes_per_second"), "2 MiB/s");
  assert.equal(formatMetricValue(52, "celsius"), "52°C");
  assert.equal(formatMetricValue(112.5, "watts"), "112.5 W");
  assert.equal(formatAxisValue(1536, "bytes"), "1.5 KiB");
  assert.equal(formatAxisValue(50, "percent"), "50%");
  assert.equal(formatGapDuration(90_000), "1.5 min");
});

test("range presets produce inclusive epoch bounds and a visible-tab refresh cadence", () => {
  const now = 1_700_000_000_000;
  assert.deepEqual(rangeBounds("10m", now), { fromMs: now - 600_000, toMs: now });
  assert.deepEqual(rangeBounds("7d", now), { fromMs: now - 7 * 86_400_000, toMs: now });
  for (const range of ["10m", "1h", "6h", "24h", "7d"] as const) {
    assert.equal(refreshIntervalMs(range), 1_000, `${range} refreshes every second`);
  }
});

test("history request URL carries machine, metrics, epoch bounds and max points", () => {
  const url = historyRequestUrl("machine 1", ["cpu.usage", "gpu.utilization"], "1h", 1_700_000_000_123, 600);
  const parsed = new URL(url, "https://hub.invalid");
  assert.equal(parsed.pathname, "/api/history");
  assert.equal(parsed.searchParams.get("machineId"), "machine 1");
  assert.equal(parsed.searchParams.get("metrics"), "cpu.usage,gpu.utilization");
  assert.equal(parsed.searchParams.get("maxPoints"), "600");
  const from = Number(parsed.searchParams.get("from"));
  const to = Number(parsed.searchParams.get("to"));
  assert.equal(to - from, 3_600_000);
  assert.equal(Number.isSafeInteger(from), true);
});

test("gap summary names both missing and dropped intervals", () => {
  const gaps: HistoryGap[] = [
    { fromMs: 2_000, toMs: 3_000, kind: "missing", reason: "expected interval missed" },
    { fromMs: 3_000, toMs: 4_000, kind: "dropped", reason: "outbox-bytes", droppedRecords: 4 },
  ];
  const summary = gapSummary(gaps, 0, 5_000);
  assert.match(summary, /1 missed interval/);
  assert.match(summary, /1 dropped report/);
  assert.match(summary, /outbox-bytes/);
  assert.equal(gapSummary([], 0, 5_000), "No recorded gaps in this range.");
});

// ---------------------------------------------------------------------------
// Response validation
// ---------------------------------------------------------------------------

test("parses a valid history result and rejects malformed shapes", () => {
  const valid = result();
  assert.deepEqual(parseHistoryResult(JSON.parse(JSON.stringify(valid))), valid);
  assert.equal(parseHistoryResult({ ...valid, machineId: "" }), null);
  assert.equal(parseHistoryResult({ ...valid, toMs: valid.fromMs }), null);
  assert.equal(parseHistoryResult({ ...valid, stepMs: 0 }), null);
  assert.equal(parseHistoryResult({ ...valid, tier: "1s" }), null);
  assert.equal(parseHistoryResult({ ...valid, series: [{ ...valid.series[0], points: [{ t: 1, min: 0, max: 1, avg: "x" }] }] }), null);
  assert.equal(parseHistoryResult({ ...valid, gaps: [{ fromMs: 1, toMs: 2, kind: "weird" }] }), null);
  const nullable = result({ series: [series("cpu.usage", "", [{ t: 1, min: null, max: null, avg: null }])] });
  assert.deepEqual(parseHistoryResult(nullable), nullable);
});

// ---------------------------------------------------------------------------
// Segmentation
// ---------------------------------------------------------------------------

test("splitSeries breaks at null readings and never joins across them", () => {
  const points = [point(0, 10), point(1_000, null), point(2_000, 20), point(3_000, 30)];
  const segments = splitSeries(points, { stepMs: 1_000, gaps: [], fromMs: 0, toMs: 3_000 });
  assert.deepEqual(
    segments.map((segment) => segment.map((p) => p.t)),
    [[0], [2_000, 3_000]],
  );
});

test("splitSeries breaks on omitted buckets that exceed the bucket step", () => {
  const points = [point(0, 10), point(1_000, 20), point(4_000, 30), point(5_000, 40)];
  const segments = splitSeries(points, { stepMs: 1_000, gaps: [], fromMs: 0, toMs: 5_000 });
  assert.deepEqual(
    segments.map((segment) => segment.map((p) => p.t)),
    [[0, 1_000], [4_000, 5_000]],
  );
});

test("splitSeries breaks on recorded gap intervals and clips out-of-range gaps", () => {
  const points = [point(0, 10), point(1_000, 20), point(2_000, 30), point(3_000, 40)];
  const gaps: HistoryGap[] = [
    { fromMs: 1_200, toMs: 1_800, kind: "missing", reason: "expected interval missed" },
    { fromMs: 20_000, toMs: 30_000, kind: "dropped", reason: "outbox-age" },
  ];
  const segments = splitSeries(points, { stepMs: 1_000, gaps, fromMs: 0, toMs: 3_000 });
  assert.deepEqual(
    segments.map((segment) => segment.map((p) => p.t)),
    [[0, 1_000], [2_000, 3_000]],
  );
  assert.deepEqual(gapBands(gaps, 0, 3_000), [
    { fromMs: 1_200, toMs: 1_800, kind: "missing", reason: "expected interval missed" },
  ]);
});

test("gapBands merges overlaps, preserves dropped kind and clips to the range", () => {
  const gaps: HistoryGap[] = [
    { fromMs: 0, toMs: 2_000, kind: "missing", reason: "a" },
    { fromMs: 1_000, toMs: 2_500, kind: "dropped", reason: "b", droppedRecords: 3 },
    { fromMs: 9_000, toMs: 10_000, kind: "missing", reason: "c" },
  ];
  assert.deepEqual(gapBands(gaps, 500, 5_000), [
    {
      fromMs: 500,
      toMs: 2_500,
      kind: "dropped",
      reason: "a",
      droppedRecords: 3,
    },
  ]);
});

// ---------------------------------------------------------------------------
// Downsampling and statistics
// ---------------------------------------------------------------------------

test("downsamplePoints keeps the envelope, average, first timestamp and merged grouping", () => {
  const points = Array.from({ length: 100 }, (_, index) => point(index * 1_000, index, 0.5));
  const reduced = downsamplePoints(points, 10, 1_000);
  assert.ok(reduced.length <= 10, `expected <=10 points, got ${reduced.length}`);
  assert.equal(reduced[0]!.t, 0);
  assert.equal(Math.min(...reduced.map((p) => p.min ?? Infinity)), -0.5);
  assert.equal(Math.max(...reduced.map((p) => p.max ?? -Infinity)), 99.5);
  assert.ok(reduced.every((p) => p.merged === true));
  const firstGroup = reduced[0]!;
  assert.equal(firstGroup.avg, 4.5, "average of 0..9 is preserved");
  for (let index = 1; index < reduced.length; index += 1) {
    assert.ok(reduced[index]!.t > reduced[index - 1]!.t);
  }
});

test("downsampling preserves null markers and discontinuities so charts cannot bridge them", () => {
  const points = Array.from({ length: 40 }, (_, index) => point(index * 1_000, index));
  points[20] = point(20_000, null);
  points.push(...Array.from({ length: 40 }, (_, index) => point(60_000 + index * 1_000, 100 + index)));
  const reduced = downsamplePoints(points, 8, 1_000);
  assert.ok(reduced.some((p) => p.avg === null), "null bucket survives downsampling");
  assert.ok(reduced.some((p) => p.gapBefore === true), "discontinuity is marked");
  const segments = splitSeries(reduced, { stepMs: 1_000, gaps: [], fromMs: 0, toMs: 100_000 });
  assert.ok(segments.length >= 3, `expected the null and the 30s hole to split the series, got ${segments.length}`);
  assert.ok(segments.every((segment) => segment.every((p) => p.avg !== null)));
});

test("seriesStats reports recorded extrema, mean and latest value", () => {
  const stats = seriesStats([point(0, 10, 2), point(1_000, 20, 5), point(2_000, null)]);
  assert.equal(stats.count, 2);
  assert.equal(stats.min, 8);
  assert.equal(stats.max, 25);
  assert.equal(stats.avg, 15);
  assert.equal(stats.latest, 20);
  assert.equal(stats.latestT, 1_000);
  assert.equal(stats.firstT, 0);
  assert.equal(stats.lastT, 1_000);
});

// ---------------------------------------------------------------------------
// Domains and ticks
// ---------------------------------------------------------------------------

test("Y domains are unit-aware and never compress percent charts", () => {
  assert.deepEqual(buildYDomain("percent", [4, 90]), { min: 0, max: 100, ticks: [0, 25, 50, 75, 100] });
  const bytes = buildYDomain("bytes", [null, 1_500, 3_000]);
  assert.equal(bytes.min, 0);
  assert.ok(bytes.max >= 3_000);
  assert.ok(bytes.ticks.length >= 2);
  assert.deepEqual([...bytes.ticks].sort((a, b) => a - b), bytes.ticks);
  const temp = buildYDomain("celsius", [40, 80]);
  assert.ok(temp.min < 40 && temp.max > 80);
  assert.ok(temp.ticks[0]! >= temp.min);
  const empty = buildYDomain("bytes", [null, null]);
  assert.deepEqual(empty, { min: 0, max: 1, ticks: [0, 1] });
});

test("timeTicks includes both endpoints and stays ordered", () => {
  const ticks = timeTicks(1_000, 5_000);
  assert.equal(ticks.length, 4);
  assert.equal(ticks[0], 1_000);
  assert.equal(ticks.at(-1), 5_000);
  assert.ok(ticks.every((tick, index) => index === 0 || tick > ticks[index - 1]!));
  assert.equal(timeTicks(0, 1_000, 2).length, 2);
});

// ---------------------------------------------------------------------------
// Machine metadata and defaults
// ---------------------------------------------------------------------------

test("default metrics follow capabilities and only include swap/GPU when real", () => {
  assert.deepEqual(defaultMetricsFor(MACHINE), [
    "cpu.usage",
    "memory.used",
    "gpu.utilization",
    "gpu.memory.used",
  ]);
  const withheld: MachineState = {
    ...MACHINE,
    inventory: {
      ...INVENTORY,
      capabilities: { ...INVENTORY.capabilities, gpus: { status: "withheld", reason: "local sharing policy" } },
    },
  };
  assert.deepEqual(defaultMetricsFor(withheld), ["cpu.usage", "memory.used"]);
  const unavailable = metricAvailability(withheld, "gpu.utilization");
  assert.equal(unavailable.available, false);
  assert.match(unavailable.reason ?? "", /withheld/);
  assert.equal(metricAvailability(MACHINE, "cpu.usage").available, true);
  const noMachine = defaultMetricsFor(undefined);
  assert.deepEqual(noMachine, []);
});

test("gpu cards union inventory and stored history, and skip mute adapters", () => {
  const cards = gpuCards(MACHINE, [series("gpu.utilization", "gpu:0", [point(0, 10)]), series("gpu.utilization", "gpu:removed", [point(0, 20)])]);
  assert.deepEqual(cards.map((card) => card.id), ["gpu:0", "gpu:removed"]);
  assert.equal(cards[0]!.label, "RTX 3090");
  assert.equal(cards[0]!.totalBytes, 24 * 1024 ** 3);
  assert.equal(cards[0]!.hasHistory, true);
  assert.equal(cards[1]!.label, "removed");
  assert.equal(gpuCards(MACHINE, []).length, 1, "unsupported adapter without history is muted");
});

test("resource labels use inventory names and fall back to bounded ids", () => {
  assert.equal(resourceLabel(MACHINE, "gpu.utilization", "gpu:0"), "RTX 3090");
  assert.equal(resourceLabel(MACHINE, "fs.used", "fs:/"), "/");
  assert.equal(resourceLabel(MACHINE, "disk.read", "disk:nvme0"), "nvme0n1");
  assert.equal(resourceLabel(MACHINE, "net.rx", "net:eth0"), "eth0");
  assert.equal(resourceLabel(MACHINE, "cpu.usage", ""), "CPU");
  assert.equal(resourceLabel(undefined, "gpu.utilization", "gpu:GPU-4ace4b62-1234567890abcdef"), "GPU-4ace4b…cdef");
});

test("metric series styles keep CPU blue and rotate distinguishably for extra resources", () => {
  const cpu = metricOption("cpu.usage")!;
  const memory = metricOption("memory.used")!;
  assert.deepEqual(metricSeriesStyle(cpu, 0), { color: "var(--cpu)", dash: undefined });
  assert.deepEqual(metricSeriesStyle(memory, 0), { color: "var(--mem)", dash: undefined });
  assert.deepEqual(metricSeriesStyle(cpu, 1), { color: "var(--mem)", dash: "7 4" });
  assert.notEqual(metricSeriesStyle(cpu, 2).color, metricSeriesStyle(cpu, 1).color);
});

test('axis end labels match the chart domain when bytes peak between nice ticks', () => {
  for (const peak of [854589440, 25_448_775_680, 16_000_000_000]) {
    const domain = buildYDomain('bytes', [peak]);
    assert.ok(domain.max >= peak);
    assert.equal(domain.ticks[0], domain.min);
    assert.equal(domain.ticks.at(-1), domain.max,
      'top label must describe the top of the chart rather than a lower value');
  }
});

test("normal five-second sampling and rollup cadence remain connected on a finer query grid", () => {
  const points = [point(0, 10), point(5000, 20), point(10000, 30), point(25000, 40)];
  const stepMs = continuityStepMs(1000, "raw", 5000);
  const segments = splitSeries(points, { stepMs, gaps: [], fromMs: 0, toMs: 30000 });
  assert.deepEqual(segments.map(s => s.length), [3, 1]);
  assert.equal(continuityStepMs(36000, "1m", 5000), 60000);
  assert.equal(splitSeries([point(0, 1), point(60000, 2)], {
    stepMs: continuityStepMs(36000, "1m", 5000), gaps: [], fromMs: 0, toMs: 120000,
  }).length, 1);
  assert.equal(splitSeries(points.slice(0, 3), { stepMs, gaps: [
    { kind: "dropped", fromMs: 6000, toMs: 9000 },
  ], fromMs: 0, toMs: 30000 }).length, 2);
});

test('GPU history ignores empty PCI aliases after NVIDIA UUID discovery', () => {
  const machine = structuredClone(MACHINE);
  const gpu = machine.inventory!.gpus[0]!;
  gpu.pciBusId = '0000:64:00.0';
  const cards = gpuCards(machine, [series('gpu.utilization', 'pci:0000:64:00.0', [point(0, null)])]);
  assert.deepEqual(cards.map(card => card.id), [gpu.id]);
});
