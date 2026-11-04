import assert from "node:assert/strict";
import test from "node:test";
import { gapBands, splitSeries } from "../src/history/chart";
import { observedOutageGaps, updateObservedOutages } from "../src/history/gaps";

const connection = (connected: boolean, at: number | null) => ({
  connected, lastReceivedAt: at === null ? null : new Date(at).toISOString(),
});
const point = (t: number, avg: number | null = 20) => ({ t, min: avg, max: avg, avg });

test("a live outage appears immediately and a short reconnect freezes its end", () => {
  const online = connection(true, 1000);
  const offline = connection(false, 1000);
  const open = updateObservedOutages([], online, offline, 2000);
  assert.deepEqual(observedOutageGaps(open, 0, 3000), [{
    fromMs: 1000, toMs: 3000, kind: "missing", reason: "collector disconnected",
  }]);
  const reconnected = updateObservedOutages(open, offline, online, 4000);
  assert.equal(reconnected[0]?.toMs, 4000);
  const later = updateObservedOutages(reconnected, online, connection(true, 9000), 9000);
  assert.equal(observedOutageGaps(later, 0, 10_000)[0]?.toMs, 4000);
  assert.equal(later, reconnected, "healthy samples must not grow the outage");
});

test("reconnect with a new receipt closes at that first receipt, including subsequent outages", () => {
  const offline = connection(false, 1000);
  const open = updateObservedOutages([], null, offline, 2000);
  const closed = updateObservedOutages(open, offline, connection(true, 3500), 4000);
  const second = updateObservedOutages(closed, connection(true, 5000), connection(false, 5000), 6000);
  assert.deepEqual(second, [{ fromMs: 1000, toMs: 3500 }, { fromMs: 5000, toMs: null }]);
  assert.deepEqual(observedOutageGaps(second, 3000, 7000).map(({ fromMs, toMs }) => [fromMs, toMs]), [[3000, 3500], [5000, 7000]]);
  assert.deepEqual(observedOutageGaps(closed, 4000, 8000), []);
  assert.deepEqual(updateObservedOutages([], null, connection(true, 1000), 2000), []);
});

test("gap shading reaches neighboring readings without ending at a bucket inside the outage", () => {
  const points = [point(0), point(10_000), point(20_000), point(30_000), point(40_000)];
  const gaps = [{ fromMs: 12_000, toMs: 28_000, kind: "missing" as const }];
  const bands = gapBands(gaps, 0, 40_000, points);
  assert.deepEqual(bands, [{ fromMs: 10_000, toMs: 30_000, kind: "missing" }]);
  const segments = splitSeries(points, { fromMs: 0, toMs: 40_000, stepMs: 10_000, gaps: bands });
  assert.deepEqual(segments.map((segment) => segment.map(({ t }) => t)), [[0, 10_000], [20_000], [30_000, 40_000]]);
  assert.deepEqual(segments.flat(), points, "keep real boundary bucket values without drawing a flat outage segment");
});

test("gap alignment ignores unavailable buckets and remains inside the selected range", () => {
  const points = [point(-1000), point(0), point(1000, null), point(3000, null), point(4000), point(6000)];
  const gaps = [{ fromMs: 1500, toMs: 2500, kind: "missing" as const }];
  assert.deepEqual(gapBands(gaps, 0, 5000, points), [{ fromMs: 0, toMs: 4000, kind: "missing" }]);
  assert.deepEqual(gapBands([{ fromMs: -2000, toMs: 1500, kind: "missing" }], 500, 2500, points),
    [{ fromMs: 500, toMs: 2500, kind: "missing" }]);
});
