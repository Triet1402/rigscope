/** Bounded synthetic measurement smoke test; no service or network access. */
import assert from "node:assert/strict";
import test from "node:test";
import { existsSync } from "node:fs";

import { measure } from "../src/operations/measure";

test("bounded measurement reports synthetic ingestion, event loop, disk and history metrics", async () => {
  const report = await measure({
    machines: 1,
    batches: 4,
    recordsPerBatch: 2,
    gpus: 1,
    historyPoints: 100,
    keep: false,
  });
  assert.equal(report.tool, "rigscope-operations-measure");
  assert.equal(report.synthetic, true);
  assert.ok(report.results.rssBytes.peak >= report.results.rssBytes.end);
  assert.match(report.disclaimer, /not a capacity/i);
  assert.equal(report.config.totalRecords, 8);
  assert.equal(report.config.viewers, 1);
  assert.equal(report.results.history.viewers, 1);
  assert.equal(report.results.accepted, 8);
  assert.equal(report.results.busy, 0);
  assert.ok(report.results.wallMs >= 0);
  for (const value of [
    report.results.ingestLatencyMs.avg,
    report.results.ingestLatencyMs.p95,
    report.results.ingestLatencyMs.max,
    report.results.eventLoopDelayMs.p50,
    report.results.eventLoopDelayMs.p99,
    report.results.eventLoopDelayMs.max,
  ]) {
    assert.equal(Number.isFinite(value), true);
    assert.ok(value >= 0);
  }
  assert.ok(report.results.database.bytes > 0);
  assert.ok(report.results.store !== null);
  assert.ok((report.results.store?.committedRecords ?? 0) >= 8);
  assert.ok(report.results.history.points > 0);
  assert.ok(report.results.history.series >= 1);
  assert.equal(report.artifacts.kept, false);
  assert.equal(existsSync(report.artifacts.directory), false, "temporary database is removed");
  assert.equal(JSON.parse(JSON.stringify(report)).synthetic, true);
});

test("measurement supports more than fifty machines and overlapping history rounds", async () => {
  const report = await measure({
    machines: 75,
    batches: 75,
    recordsPerBatch: 1,
    gpus: 0,
    historyPoints: 100,
    viewers: 4,
    historyConcurrency: 2,
  });
  assert.equal(report.config.machines, 75);
  assert.equal(report.config.totalRecords, 75);
  assert.equal(report.results.accepted, 75);
  assert.equal(report.config.historyConcurrency, 2);
  assert.equal(report.results.history.concurrency, 2);
  assert.equal(report.results.history.queries, 4);
  assert.ok(report.results.history.p95QueryMs >= 0);
  assert.ok(report.results.history.wallMs >= 0);
});

test("measurement bounds reject impossible workloads", async () => {
  await assert.rejects(measure({ machines: 0 }), /machines/);
  await assert.rejects(measure({ machines: 1_001 }), /machines/);
  await assert.rejects(measure({ recordsPerBatch: 33 }), /recordsPerBatch/);
  await assert.rejects(measure({ batches: 2_001 }), /batches/);
  await assert.rejects(measure({ historyConcurrency: 0 }), /historyConcurrency/);
  await assert.rejects(measure({ historyConcurrency: 65 }), /historyConcurrency/);
});
