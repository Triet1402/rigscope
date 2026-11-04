/**
 * Unit tests for the live-view presentation helpers (src/live/presentation).
 * Run via the Node test runner through tsx: focused suite for milestone 3.
 *
 * Covers the requirements the views depend on: receipt-time freshness with
 * cadence-aware thresholds (max(15 s, 3 × advertised interval), 15 s
 * fallback when cadence metadata is absent or null), last-known retention
 * for stale fields, per-field unavailability with reasons (never fake
 * zeros), collector independence from freshness, capture clock-skew
 * flagging that never influences liveness, and deterministic disclosure
 * formatting.
 */

import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import {
  captureClockSkew,
  collectorConnection,
  cpuFieldView,
  formatAge,
  formatBytes,
  formatIntervalMs,
  formatPercent,
  freshnessThresholdFor,
  hasUnavailableField,
  machineAttention,
  memoryFieldView,
  presentMachine,
  freshness,
  stateFreshness,
} from "../src/live/presentation";
import type { BytesUsageMetric, CpuMetric, MachineState, MetricsPayload, MetricsRecord } from "../src/contracts";

const T0 = Date.parse("2026-11-02T12:00:00.000Z");

function iso(epochMs: number): string {
  return new Date(epochMs).toISOString();
}

type MetricsOverrides = { capturedAt?: string; receivedAt?: string; payload?: Partial<MetricsPayload> };

function sample(overrides: MetricsOverrides = {}): MetricsRecord & { receivedAt: string } {
  const payload: MetricsPayload = {
    cpu: { status: "ok", usagePercent: 40, perCore: [40] },
    memory: { status: "ok", usedBytes: 8_589_934_592, totalBytes: 34_359_738_368 },
    swap: { status: "ok", usedBytes: 0, totalBytes: 0 },
    gpus: { status: "unavailable", reason: "no GPUs detected" },
    disks: { status: "ok", filesystems: [], devices: [] },
    network: { status: "ok", interfaces: [] },
    ...overrides.payload,
  };
  return {
    collectorId: "collector-a",
    runId: "run-1",
    bootId: "boot-123",
    sequence: 1,
    capturedAt: overrides.capturedAt ?? iso(T0 - 1000),
    receivedAt: overrides.receivedAt ?? iso(T0 - 500),
    kind: "metrics",
    payload,
  };
}

function machine(overrides: Partial<MachineState> = {}): MachineState {
  return {
    machineId: "machine-a",
    collectorId: "collector-a",
    hostname: "rig-1",
    connected: true,
    lastReceivedAt: iso(T0 - 500),
    metrics: sample(),
    ...overrides,
  };
}

describe("freshness uses hub receipt time (15 s fallback without cadence)", () => {
  it("is live well inside the threshold", () => {
    assert.equal(freshness(iso(T0 - 5_000), T0), "live");
    assert.equal(freshness(iso(T0 - 14_999), T0), "live");
  });

  it("is stale at exactly the fixed 15 s threshold and beyond", () => {
    // Fixed, disclosed UI rule: >= 15000 ms is stale.
    assert.equal(freshness(iso(T0 - 15_000), T0), "stale");
    assert.equal(freshness(iso(T0 - 60_000), T0), "stale");
  });

  it("is unknown without a receipt timestamp or an unparseable one", () => {
    assert.equal(freshness(null, T0), "unknown");
    assert.equal(freshness("not-a-date", T0), "unknown");
  });

  it("never consults the capture timestamp", () => {
    // capturedAt claims "now", but the hub received the sample 2 minutes ago:
    // the value MUST be stale, not live.
    const state = machine({
      lastReceivedAt: iso(T0 - 120_000),
      metrics: sample({ capturedAt: iso(T0) }),
    });
    assert.equal(stateFreshness(state, T0), "stale");
  });

  it("treats a future hub receipt as live (skew never fakes staleness via receipt)", () => {
    assert.equal(freshness(iso(T0 + 30_000), T0), "live");
  });
});

describe("staleness follows the machine's advertised cadence", () => {
  it("resolves max(15 s, 3 × interval) for a known cadence", () => {
    assert.equal(freshnessThresholdFor(30_000), 90_000);
    assert.equal(freshnessThresholdFor(45_000), 135_000);
    assert.equal(freshnessThresholdFor(300), 15_000, "floor at 15 s for fast cadences");
  });

  it("falls back to 15 s for unknown cadence metadata without inventing one", () => {
    // Legacy snapshots omit the field; current hubs emit null when unknown.
    assert.equal(freshnessThresholdFor(undefined), 15_000);
    assert.equal(freshnessThresholdFor(null), 15_000);
  });

  it("keeps a stated fallback threshold earlier tests relied on at 15 s", () => {
    assert.equal(freshnessThresholdFor(1_000), 15_000);
  });

  it("stays live across a slow cadence until 3 × interval, then goes stale", () => {
    const state = machine({ expectedIntervalMs: 30_000, lastReceivedAt: iso(T0) });
    assert.equal(stateFreshness(state, T0 + 59_000), "live", "3 × interval not reached");
    assert.equal(stateFreshness(state, T0 + 89_999), "live", "just inside the threshold");
    assert.equal(stateFreshness(state, T0 + 90_000), "stale");
  });

  it("goes stale exactly at 3 × interval (boundary, inclusive)", () => {
    const state = machine({ expectedIntervalMs: 30_000, lastReceivedAt: iso(T0 - 90_000) });
    assert.equal(stateFreshness(state, T0), "stale", "age == 3 × interval is stale");
    const stillFresh = machine({ expectedIntervalMs: 30_000, lastReceivedAt: iso(T0 - 89_999) });
    assert.equal(stateFreshness(stillFresh, T0), "live");
  });

  it("unknown cadence stays aligned with the fixed 15 s fallback", () => {
    const state = machine({ lastReceivedAt: iso(T0 - 15_000) });
    assert.equal(stateFreshness(state, T0), "stale");
    const freshUnknown = machine({ lastReceivedAt: iso(T0 - 14_999) });
    assert.equal(stateFreshness(freshUnknown, T0), "live");
  });

  it("attention and presentation use the dynamic threshold by default", () => {
    const state = machine({
      expectedIntervalMs: 60_000,
      lastReceivedAt: iso(T0 - 100_000),
    });
    // 100 s is beyond the 15 s fallback but below max(15 s, 3 × 60 s) = 180 s.
    assert.equal(stateFreshness(state, T0), "live");
    assert.equal(machineAttention(state, T0), "ok");
    const presented = presentMachine(state, T0);
    assert.equal(presented.freshness, "live");
    assert.equal(presented.thresholdMs, 180_000);
  });

  it("stale attention still wins over degraded/disconnected at slow cadence", () => {
    const state = machine({
      expectedIntervalMs: 30_000,
      lastReceivedAt: iso(T0 - 90_000),
      connected: false,
      metrics: sample({
        payload: {
          cpu: { status: "ok", usagePercent: 12, perCore: [12] },
          memory: { status: "unavailable", reason: "withheld" },
        },
      }),
    });
    assert.equal(machineAttention(state, T0), "stale");
    // And freshness-independent axes remain intact while fresh.
    const heldState = machine({ expectedIntervalMs: 30_000, connected: false });
    assert.equal(machineAttention(heldState, T0), "disconnected");
    assert.equal(stateFreshness(heldState, T0), "live");
  });

  it("an explicit threshold still overrides the cadence-derived default", () => {
    const state = machine({ expectedIntervalMs: 30_000, lastReceivedAt: iso(T0 - 60_000) });
    assert.equal(stateFreshness(state, T0, 15_000), "stale");
    const presented = presentMachine(state, T0, 15_000);
    assert.equal(presented.freshness, "stale");
    assert.equal(presented.thresholdMs, 15_000);
  });
});

describe("disclosure formatting is deterministic and bounded", () => {
  it("formats cadence intervals with explicit units", () => {
    assert.equal(formatIntervalMs(100), "0.1 s");
    assert.equal(formatIntervalMs(1_500), "1.5 s");
    assert.equal(formatIntervalMs(15_000), "15 s");
    assert.equal(formatIntervalMs(45_000), "45 s");
    assert.equal(formatIntervalMs(60_000), "1 min");
    assert.equal(formatIntervalMs(90_000), "1.5 min");
    assert.equal(formatIntervalMs(120_000), "2 min");
  });

  it("formats hour-scale thresholds", () => {
    assert.equal(formatIntervalMs(3_600_000), "1 h");
    assert.equal(formatIntervalMs(7_200_000), "2 h");
  });

  it("refuses to make up a missing interval", () => {
    assert.equal(formatIntervalMs(null), "—");
    assert.equal(formatIntervalMs(undefined), "—");
    assert.equal(formatIntervalMs(Number.NaN), "—");
  });
});

describe("last-known field retention (scalar CPU)", () => {
  it("returns the live value when fresh", () => {
    const view = cpuFieldView({ status: "ok", usagePercent: 37.4, perCore: [37.4] }, "live");
    assert.deepEqual(view, { out: "value", value: 37.4 });
  });

  it("returns the SAME value labeled stale when freshness is stale", () => {
    const view = cpuFieldView({ status: "ok", usagePercent: 37.4, perCore: [37.4] }, "stale");
    assert.deepEqual(view, { out: "stale", value: 37.4 });
  });

  it("surfaces the reason for unavailable fields at any freshness (no fake zero)", () => {
    const metric = { status: "unavailable" as const, reason: "not permitted by local sharing policy" };
    assert.deepEqual(cpuFieldView(metric, "live"), {
      out: "unavailable",
      reason: "not permitted by local sharing policy",
    });
    assert.deepEqual(cpuFieldView(metric, "stale"), {
      out: "unavailable",
      reason: "not permitted by local sharing policy",
    });
  });

  it("returns absent without a metric", () => {
    assert.equal(cpuFieldView(undefined, "live").out, "absent");
  });
});

describe("memory field retention keeps bytes", () => {
  it("returns used/total when fresh", () => {
    const view = memoryFieldView(
      { status: "ok", usedBytes: 100, totalBytes: 200 },
      "live",
    );
    assert.deepEqual(view, { out: "value", usedBytes: 100, totalBytes: 200 });
  });

  it("returns the SAME used/total labeled stale", () => {
    const view = memoryFieldView(
      { status: "ok", usedBytes: 100, totalBytes: 200 },
      "stale",
    );
    assert.deepEqual(view, { out: "stale", usedBytes: 100, totalBytes: 200 });
  });

  it("surfaces the unavailable reason with no zero substitute", () => {
    const view = memoryFieldView(
      { status: "unavailable", reason: "field withheld by hub scope" },
      "stale",
    );
    assert.deepEqual(view, {
      out: "unavailable",
      reason: "field withheld by hub scope",
    });
  });
});

describe("attention combines but never merges the state axes", () => {
  it("is unseen without a sample", () => {
    assert.equal(machineAttention(machine({ metrics: undefined }), T0), "unseen");
  });

  it("is stale when the receipt is past the threshold, regardless of connection", () => {
    assert.equal(
      machineAttention(machine({ lastReceivedAt: iso(T0 - 30_000) }), T0),
      "stale",
    );
    assert.equal(
      machineAttention(machine({ connected: false, lastReceivedAt: iso(T0 - 30_000) }), T0),
      "stale",
    );
  });

  it("is degraded when any field is unavailable but data is fresh", () => {
    const state = machine({
      metrics: sample({
        payload: {
          cpu: { status: "ok", usagePercent: 12, perCore: [12] },
          memory: {
            status: "unavailable",
            reason: "field withheld by hub scope",
          },
        },
      }),
    });
    assert.equal(machineAttention(state, T0), "degraded");
    assert.equal(hasUnavailableField(state.metrics), true);
  });

  it("is disconnected with fresh data while the collector is off, keeping last known values", () => {
    const state = machine({ connected: false });
    assert.equal(machineAttention(state, T0), "disconnected");
    // Freshness and connection stay separable axes.
    assert.equal(collectorConnection(state, T0), "disconnected");
    assert.equal(stateFreshness(state, T0), "live");
  });

  it("is ok when connected and fresh with all fields available", () => {
    assert.equal(machineAttention(machine(), T0), "ok");
  });

  it("presents all four axes coherently for a fleet row", () => {
    const presented = presentMachine(machine(), T0);
    assert.deepEqual(presented, {
      freshness: "live",
      attention: "ok",
      connection: "connected",
      skew: { kind: "none" },
      thresholdMs: 15_000,
    });
  });
});

describe("capture clock skew is flagged independently of liveness", () => {
  it("flags a capture far ahead of receipt", () => {
    const state = machine({
      metrics: sample({
        capturedAt: iso(T0 + 5 * 60_000), // 5 min ahead of T0
        receivedAt: iso(T0),
      }),
    });
    const skew = captureClockSkew(state);
    assert.equal(skew.kind, "ahead");
    if (skew.kind === "ahead") {
      assert.ok(Math.abs(skew.skewMs - 5 * 60_000) < 1);
    }
    // And it changes nothing about freshness, which uses receipt time.
    assert.equal(stateFreshness(state, T0), "live");
    assert.equal(machineAttention(state, T0), "ok");
  });

  it("flags a capture far behind receipt (delay) with the magnitude reported", () => {
    const state = machine({
      metrics: sample({
        capturedAt: iso(T0 - 10 * 60_000),
        receivedAt: iso(T0),
      }),
    });
    const skew = captureClockSkew(state);
    assert.equal(skew.kind, "behind");
    if (skew.kind === "behind") {
      assert.ok(Math.abs(skew.skewMs - 10 * 60_000) < 1);
    }
  });

  it("accepts small differences as not-skewed", () => {
    const state = machine({
      metrics: sample({
        capturedAt: iso(T0 - 1_000),
        receivedAt: iso(T0),
      }),
    });
    assert.deepEqual(captureClockSkew(state), { kind: "none" });
  });

  it("is unknown without a sample or with unparseable clocks", () => {
    assert.equal(captureClockSkew(machine({ metrics: undefined })).kind, "unknown");
    assert.equal(
      captureClockSkew(
        machine({ metrics: sample({ capturedAt: "bogus", receivedAt: iso(T0) }) }),
      ).kind,
      "unknown",
    );
  });
});

describe("deterministic formatting", () => {
  it("formats byte counts in binary units", () => {
    assert.equal(formatBytes(0), "0 B");
    assert.equal(formatBytes(512), "512 B");
    assert.equal(formatBytes(1024), "1.0 KiB");
    assert.equal(formatBytes(1_048_576), "1.0 MiB");
    assert.equal(formatBytes(8.5 * 1024 ** 3), "8.5 GiB");
    assert.equal(formatBytes(34_359_738_368), "32.0 GiB");
    assert.equal(formatBytes(1024 ** 4 * 2.5), "2.5 TiB");
    assert.equal(formatBytes(null), "—");
  });

  it("formats percents at one decimal and never fakes a missing value", () => {
    assert.equal(formatPercent(0), "0.0%");
    assert.equal(formatPercent(37.44), "37.4%");
    assert.equal(formatPercent(100), "100.0%");
    assert.equal(formatPercent(null), "—");
  });

  it("formats bounded ages deterministically", () => {
    assert.equal(formatAge(0), "0s ago");
    assert.equal(formatAge(4_999), "5s ago");
    assert.equal(formatAge(59_000), "59s ago");
    assert.equal(formatAge(60_000), "1m ago");
    assert.equal(formatAge(61_000), "1m 1s ago");
    assert.equal(formatAge(90_000), "1m 30s ago");
    assert.equal(formatAge(119_000), "1m 59s ago");
    assert.equal(formatAge(3_600_000), "1h ago");
    assert.equal(formatAge(3_700_000), "1h 1m ago");
    assert.equal(formatAge(4_200_000), "1h 10m ago");
    assert.equal(formatAge(86_400_000 * 3 + 7_200_000), "3d 2h ago");
    assert.equal(formatAge(null), "—");
    // A negative delta (future receipt) is clamped, never formatted oddly.
    assert.equal(formatAge(-5_000), "0s ago");
  });
});
