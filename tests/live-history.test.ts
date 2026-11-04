import assert from "node:assert/strict";
import test from "node:test";
import type { HubSnapshot } from "../src/contracts";
import { cpuChartPath, historyForSnapshot } from "../src/live/history";
import { SAMPLE_HISTORY_LIMIT, type SampleHistoryPoint } from "../src/live/types";

function snapshot(
  sequence = 1,
  machineId = "machine",
  cadence?: number | null,
): HubSnapshot {
  const machine: HubSnapshot["machines"][number] = {
    machineId, collectorId: "collector", hostname: "host", connected: true,
    lastReceivedAt: new Date(sequence * 1000).toISOString(),
    metrics: {
      collectorId: "collector", runId: "run", bootId: "boot", sequence,
      capturedAt: new Date(sequence * 1000).toISOString(), receivedAt: new Date(sequence * 1000).toISOString(),
      kind: "metrics", payload: {
        cpu: { status: "ok", usagePercent: 25, perCore: [25] }, memory: { status: "unavailable", reason: "withheld" },
        swap: { status: "unavailable", reason: "withheld" }, gpus: { status: "unavailable", reason: "withheld" },
        disks: { status: "unavailable", reason: "withheld" }, network: { status: "unavailable", reason: "withheld" },
      },
    },
  };
  // Distinguish "absent" (unknown) from explicit null (a hello that did not
  // advertise a cadence): only pass the key when a cadence was given.
  if (cadence !== undefined) machine.expectedIntervalMs = cadence;
  return { hubRunId: "hub", machines: [machine] };
}

test("history updates are immutable, repeatable and deduplicate full snapshots", () => {
  const previous = new Map<string, readonly SampleHistoryPoint[]>();
  const frame = snapshot();
  const first = historyForSnapshot(previous, frame);
  assert.equal(previous.size, 0);
  assert.deepEqual(historyForSnapshot(previous, frame), first, "React may replay a pure updater");
  assert.deepEqual(historyForSnapshot(first, frame), first);
  assert.equal(historyForSnapshot(first, snapshot(2)).get("machine")!.length, 2);
  assert.equal(first.get("machine")!.length, 1);
});

test("history supports arbitrary machine IDs and discards machines outside the next scope", () => {
  let history = historyForSnapshot(new Map(), snapshot(1, "__proto__"));
  assert.equal(history.get("__proto__")!.length, 1);
  history = historyForSnapshot(history, snapshot(2, "toString"));
  assert.equal(history.has("__proto__"), false);
  assert.equal(history.get("toString")!.length, 1);
  assert.equal(historyForSnapshot(history, { hubRunId: "hub", machines: [] }).size, 0);
});

test("history is bounded and new hub or collector runs start a separate series", () => {
  let history: ReadonlyMap<string, readonly SampleHistoryPoint[]> = new Map();
  for (let n = 1; n <= SAMPLE_HISTORY_LIMIT + 10; n++) history = historyForSnapshot(history, snapshot(n));
  assert.equal(history.get("machine")!.length, SAMPLE_HISTORY_LIMIT);
  const frame = snapshot(500);
  frame.hubRunId = "new-hub";
  history = historyForSnapshot(history, frame);
  assert.equal(history.get("machine")!.length, 1);
  frame.machines[0].metrics!.runId = "new-run";
  assert.equal(historyForSnapshot(history, frame).get("machine")!.length, 1);
});

test("chart leaves unavailable CPU readings empty and never draws an invented zero", () => {
  const frame = snapshot();
  frame.machines[0].metrics!.payload.cpu = { status: "unavailable", reason: "first reading" };
  const first = historyForSnapshot(new Map(), frame);
  assert.equal(cpuChartPath(first.get("machine")!), null);
  frame.machines[0].metrics!.sequence = 2;
  const second = historyForSnapshot(first, frame);
  assert.equal(cpuChartPath(second.get("machine")!), null);
});

test("chart breaks at unavailable readings and long receipt gaps", () => {
  const points: SampleHistoryPoint[] = [0, 1_000, 2_000, 3_000, 30_000, 31_000].map((at, i) => ({
    sampleId: String(i), receivedAtLocal: at, cpuPercent: i === 2 ? null : 25,
    memoryPercent: null,
  }));
  const path = cpuChartPath(points)!;
  assert.equal(path.match(/M/g)!.length, 3);
  assert.equal(path.match(/L/g)!.length, 2);
});

// ---------------------------------------------------------------------------
// Cadence-aware chart gaps (milestone 3): every point carries the
// cadence-derived threshold it arrived with; fixtures without it
// chart on the 15 s fallback.
// ---------------------------------------------------------------------------

/** Counts path commands without the trap of an empty match() result. */
function count(path: string, command: "M" | "L"): number {
  return path.split(command).length - 1;
}

function point(
  at: number,
  cadenceGapMs?: number,
): SampleHistoryPoint {
  return {
    sampleId: `p${at}`,
    receivedAtLocal: at,
    cpuPercent: 25,
    memoryPercent: null,
    ...(cadenceGapMs === undefined ? {} : { gapThresholdMs: cadenceGapMs }),
  };
}

test("history stores each point's cadence-derived threshold at arrival time", () => {
  // Known cadence 30 s → threshold max(15 s, 3 × 30 s) = 90 s.
  const known = historyForSnapshot(new Map(), snapshot(1, "machine", 30_000));
  assert.equal(known.get("machine")![0].gapThresholdMs, 90_000);
  // Explicit unknown cadence (hub emits null) falls back to 15 s.
  const unknown = historyForSnapshot(new Map(), snapshot(1, "machine", null));
  assert.equal(unknown.get("machine")![0].gapThresholdMs, 15_000);
  // Snapshot without the field also falls back to 15 s.
  const legacy = historyForSnapshot(new Map(), snapshot(1));
  assert.equal(legacy.get("machine")![0].gapThresholdMs, 15_000);
});

test("normal slow deliveries connect; the chart gap opens at exactly 3 × interval", () => {
  let history = historyForSnapshot(new Map(), snapshot(1, "machine", 30_000));
  history = historyForSnapshot(history, snapshot(31, "machine", 30_000)); // 30 s apart
  const points = history.get("machine")!;
  assert.deepEqual([...points].map((p) => p.gapThresholdMs), [90_000, 90_000]);
  const connected = cpuChartPath(points)!;
  assert.equal(count(connected, "M"), 1);
  assert.equal(count(connected, "L"), 1);

  // A 30 s receipt gap stays a gap under the 15 s fallback cadence.
  let fast = historyForSnapshot(new Map(), snapshot(1, "machine", null));
  fast = historyForSnapshot(fast, snapshot(31, "machine", null));
  const fastPath = cpuChartPath(fast.get("machine")!)!;
  assert.equal(count(fastPath, "M"), 2);
  assert.equal(count(fastPath, "L"), 0);
});

test("a chart segment breaks exactly at the cadence threshold", () => {
  const slowPoints = [point(0, 90_000), point(89_999, 90_000)];
  const joined = cpuChartPath(slowPoints)!;
  assert.equal(count(joined, "L"), 1);
  const exact = cpuChartPath([point(0, 90_000), point(90_000, 90_000)])!;
  assert.equal(count(exact, "L"), 0);
  assert.equal(
    count(exact, "M"),
    2,
    "age exactly at the threshold is a gap, mirroring the freshness rule",
  );
});

test("a cadence change cannot retroactively bridge an old gap", () => {
  const a = point(0, 15_000);
  const b = point(10_000, 15_000);
  const c = point(40_000, 90_000); // slower configuration arrives here
  const d = point(70_000, 90_000);
  const path = cpuChartPath([a, b, c, d])!;
  // b→c: 30 s below the NEW 90 s threshold but reaches the point's own
  // earlier 15 s threshold — the stricter endpoint threshold applies, so
  // later metadata cannot join the line across that gap.
  assert.equal(path.match(/M/g)!.length, 2);
  assert.equal(path.match(/L/g)!.length, 2);
});

test("points keep the threshold they arrived with, independent of later points", () => {
  const laterSlow = [point(0, 15_000), point(14_999, 90_000)];
  const path = cpuChartPath(laterSlow)!;
  // Even against a slow-era point, the earlier 15 s threshold governs; the
  // 14.999 s gap connects, a 15 s gap cannot be bridged by later metadata.
  assert.equal(count(path, "L"), 1);
  const breaks = cpuChartPath([point(0, 15_000), point(15_000, 90_000)])!;
  assert.equal(count(breaks, "M"), 2);
});

test("legacy fixture points without a threshold fall back to 15 s", () => {
  const legacy = [point(0), point(14_000)];
  assert.equal(count(cpuChartPath(legacy)!, "L"), 1);
  const bridged = [point(0), point(15_000)];
  assert.equal(count(cpuChartPath(bridged)!, "M"), 2);
});

test("unavailable readings and reception restarts still open gaps under slow cadence", () => {
  const points = [
    { ...point(0, 90_000), cpuPercent: 25 },
    { ...point(30_000, 90_000), cpuPercent: null }, // unavailable, breaks the line
    { ...point(60_000, 90_000), cpuPercent: 30 },
  ];
  const path = cpuChartPath(points)!;
  assert.equal(count(path, "M"), 2);
  assert.equal(count(path, "L"), 0);
});
