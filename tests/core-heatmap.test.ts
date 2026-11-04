import assert from "node:assert/strict";
import test from "node:test";
import { coreHeatmapBuckets } from "../src/history/core-heatmap";

test("heatmap preserves short stress peaks instead of averaging them away", () => {
  const bins = coreHeatmapBuckets([
    { t: 0, min: 0, avg: 2, max: 100 },
    { t: 1, min: 0, avg: 1, max: 1 },
    { t: 50, min: 20, avg: 30, max: 40 },
  ], 0, 100, 2);
  assert.deepEqual(bins, [100, 40]);
});

test("missing bins stay empty and real zero remains visible", () => {
  const bins = coreHeatmapBuckets([
    { t: 0, min: null, avg: null, max: null },
    { t: 25, min: 0, avg: 0, max: 0 },
    { t: 100, min: 99, avg: 99, max: 99 },
    { t: 150, min: 80, avg: 80, max: 80 },
  ], 0, 100, 4);
  assert.deepEqual(bins, [null, 0, null, 99]);
});
