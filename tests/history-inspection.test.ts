import assert from "node:assert/strict";
import test from "node:test";
import { inspectionPoint, inspectionTimeAtPointer, inspectionTimeForKey, inspectionTimes } from "../src/history/inspection";

const point = (t: number, avg: number | null) => ({ t, min: avg, max: avg, avg });

test("inspection selects the closest recorded bucket without interpolating", () => {
  const points = [point(1000, 10), point(2000, 90)];
  assert.equal(inspectionPoint(points, 1400, 1000, []), points[0]);
  assert.equal(inspectionPoint(points, 1600, 1000, []), points[1]);
  assert.equal(inspectionPoint(points, 1500, 1000, []), points[0]);
  assert.equal(inspectionPoint(points, 5000, 1000, []), null);
  assert.equal(inspectionPoint([], 1000, 1000, []), null);
});

test("inspection preserves unavailable readings and does not bridge missing or dropped intervals", () => {
  const points = [point(1000, 10), point(2000, null), point(4000, 90)];
  assert.equal(inspectionPoint(points, 2000, 1000, [])?.avg, null);
  assert.equal(inspectionPoint(points, 3000, 1000, []), null);
  for (const kind of ["missing", "dropped"] as const) {
    const gaps = [{ fromMs: 1000, toMs: 3500, kind }];
    assert.equal(inspectionPoint(points, 3400, 1000, gaps), null);
    assert.equal(inspectionPoint(points, 3600, 2000, [{ fromMs: 3700, toMs: 3800, kind }]), null);
    assert.equal(inspectionPoint(points, 4000, 1000, gaps), points[2]);
  }
});

test("inspection maps pointer coordinates to the plotted time range including SVG padding", () => {
  assert.equal(inspectionTimeAtPointer(500, 100, 800, 1000, 11_000, 6 / 800), 6000);
  assert.equal(inspectionTimeAtPointer(106, 100, 800, 1000, 11_000, 6 / 800), 1000);
  assert.equal(inspectionTimeAtPointer(894, 100, 800, 1000, 11_000, 6 / 800), 11_000);
  assert.equal(inspectionTimeAtPointer(-50, 100, 800, 1000, 11_000), 1000);
  assert.equal(inspectionTimeAtPointer(1200, 100, 800, 1000, 11_000), 11_000);
  assert.equal(inspectionTimeAtPointer(100, 100, 0, 1000, 11_000), 1000);
});

test("keyboard inspection navigates the union of series timestamps and stays at its edges", () => {
  const times = inspectionTimes([{ points: [point(2000, 90), point(4000, 10)] }, { points: [point(1000, null), point(2000, 20)] }]);
  assert.deepEqual(times.map((p) => p.t), [1000, 2000, 4000]);
  assert.equal(inspectionTimeForKey(times, null, "ArrowLeft"), 4000);
  assert.equal(inspectionTimeForKey(times, 4000, "ArrowLeft"), 2000);
  assert.equal(inspectionTimeForKey(times, 2000, "ArrowRight"), 4000);
  assert.equal(inspectionTimeForKey(times, 1000, "ArrowLeft"), 1000);
  assert.equal(inspectionTimeForKey(times, 4000, "ArrowRight"), 4000);
  assert.equal(inspectionTimeForKey(times, 2000, "Home"), 1000);
  assert.equal(inspectionTimeForKey(times, 2000, "End"), 4000);
  assert.equal(inspectionTimeForKey([], null, "End"), null);
});
