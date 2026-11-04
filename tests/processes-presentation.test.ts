/** Deterministic tests for Processes presentation and wire-response helpers. */
import assert from "node:assert/strict";
import test from "node:test";

import type { DetailResult, ProcessRow } from "../src/contracts";
import {
  DEFAULT_SORT,
  applicationGroupsForRows,
  detailStateToPhase,
  detailStaleAfterMs,
  displayRows,
  filterAndSortRows,
  filterRowsForView,
  formatInteger,
  formatThreadCount,
  isDetailSnapshotStale,
  metricDisplay,
  nextSortState,
  parseAcquireResponse,
  parseDetailPollResponse,
  phasePresentation,
  rowKey,
  snapshotSummary,
  sortAriaValue,
  treeForRows,
  withoutCommand,
} from "../src/processes/presentation";

function row(pid: number, overrides: Partial<ProcessRow> = {}): ProcessRow {
  return {
    pid,
    ppid: 1,
    startTicks: pid * 10,
    name: `proc-${pid}`,
    user: "alice",
    state: "S",
    cpuPercent: 1,
    memoryRssBytes: 1024,
    gpuMemoryBytes: null,
    ioReadBytesPerSec: null,
    ioWriteBytesPerSec: null,
    threads: 1,
    unit: null,
    ...overrides,
  };
}

test("row identity is pid plus startTicks and commands never reach the browser view", () => {
  const first = row(10, { startTicks: 100, command: "run --token=secret" });
  assert.equal(rowKey(first), "10:100");
  assert.notEqual(rowKey(first), rowKey({ ...first, startTicks: 101 }));

  const clean = withoutCommand(first);
  assert.equal(Object.prototype.hasOwnProperty.call(clean, "command"), false);
  assert.equal(first.command, "run --token=secret", "source row is not mutated");

  const rows = [first, row(11)];
  const prepared = displayRows(rows);
  assert.equal(Object.prototype.hasOwnProperty.call(prepared[0], "command"), false);
  assert.equal(Object.prototype.hasOwnProperty.call(rows[0], "command"), true);
  assert.deepEqual(
    filterRowsForView(rows, "token").map((candidate) => candidate.pid),
    [],
    "a hidden argument cannot be found by search",
  );
});

test("search, sort, and aggregation operate on the visible rows only", () => {
  const rows = [
    row(10, { name: "Firefox", unit: "app-gnome-firefox-1.scope", cpuPercent: 20, user: "alice" }),
    row(11, { name: "worker", unit: "app-gnome-firefox-1.scope", cpuPercent: 40, user: "bob" }),
    row(12, { name: "sshd", cpuPercent: null, user: null, unit: "ssh.service" }),
  ];

  assert.deepEqual(filterRowsForView(rows, "firefox").map((candidate) => candidate.pid), [10, 11]);
  assert.deepEqual(
    filterAndSortRows(rows, "firefox", { key: "cpu", dir: "desc" }).map((candidate) => candidate.pid),
    [11, 10],
  );

  const groups = applicationGroupsForRows(rows, "firefox");
  assert.equal(groups.length, 1);
  assert.equal(groups[0].label, "firefox");
  assert.equal(groups[0].count, 2);
  assert.equal(groups[0].cpuPercent, 60);
  assert.equal(groups[0].partial.cpuPercent, false);
});

test("tree view keeps parents before children after sorting siblings", () => {
  const rows = [
    row(1, { ppid: 0, name: "systemd", unit: "init.scope", cpuPercent: 1 }),
    row(2, { ppid: 1, name: "z-app", cpuPercent: 30 }),
    row(3, { ppid: 1, name: "a-app", cpuPercent: 10 }),
    row(4, { ppid: 3, name: "child", cpuPercent: 20 }),
  ];
  const nodes = treeForRows(rows, "", { key: "name", dir: "asc" });
  const order = nodes.map((node) => node.row.pid);
  assert.ok(order.indexOf(3) < order.indexOf(4), "child follows parent");
  assert.ok(order.indexOf(1) < order.indexOf(3));
  assert.ok(order.indexOf(3) < order.indexOf(2), "siblings follow the name sort");
  assert.equal(nodes.find((node) => node.row.pid === 3)?.depth, 1);
  assert.equal(nodes.find((node) => node.row.pid === 4)?.depth, 2);
});

test("sort state defaults, toggles, and exposes the right aria-sort value", () => {
  assert.equal(DEFAULT_SORT.key, "cpu");
  assert.equal(DEFAULT_SORT.dir, "desc");
  assert.deepEqual(nextSortState(DEFAULT_SORT, "cpu"), { key: "cpu", dir: "asc" });
  assert.deepEqual(nextSortState(DEFAULT_SORT, "name"), { key: "name", dir: "asc" });
  assert.deepEqual(nextSortState(DEFAULT_SORT, "memory"), { key: "memory", dir: "desc" });
  assert.equal(sortAriaValue({ key: "cpu", dir: "desc" }, "cpu"), "descending");
  assert.equal(sortAriaValue({ key: "cpu", dir: "asc" }, "cpu"), "ascending");
  assert.equal(sortAriaValue({ key: "cpu", dir: "asc" }, "memory"), "none");
});

test("metric display keeps null unknown and marks partial totals as at least", () => {
  assert.deepEqual(metricDisplay(null, "percent"), { text: "—", atLeast: false, title: null });
  assert.deepEqual(metricDisplay(0, "percent"), { text: "0.0%", atLeast: false, title: null });
  assert.deepEqual(metricDisplay(0, "bytes"), { text: "0 B", atLeast: false, title: null });
  assert.equal(metricDisplay(1536, "bytes").text, "1.5 KiB");
  assert.equal(metricDisplay(2048, "rate").text, "2.0 KiB/s");
  assert.equal(metricDisplay(1536, "count").text, "1,536");
  const partial = metricDisplay(25, "percent", true);
  assert.equal(partial.text, "25.0%");
  assert.equal(partial.atLeast, true);
  assert.match(partial.title ?? "", /At least 25\.0%/);
  assert.equal(formatInteger(1234567), "1,234,567");
  assert.equal(formatThreadCount(null), "—");
});

test("snapshot summary exposes truncation and inaccessible counts without inventing data", () => {
  assert.deepEqual(snapshotSummary(null), { total: 0, shown: 0, inaccessible: 0, truncated: false });
  assert.deepEqual(
    snapshotSummary({
      totalProcesses: 10,
      truncated: true,
      inaccessible: 2,
      processes: [row(1), row(2)],
    }),
    { total: 10, shown: 2, inaccessible: 2, truncated: true },
  );
});

test("a live snapshot becomes stale when no new capture arrives", () => {
  assert.equal(detailStaleAfterMs(2_000), 10_000);
  assert.equal(detailStaleAfterMs(5_000), 15_000);
  assert.equal(detailStaleAfterMs(0), 10_000);
  assert.equal(isDetailSnapshotStale(null, 1_000_000, 2_000), false);
  assert.equal(isDetailSnapshotStale(1_000_000, 1_009_999, 2_000), false);
  assert.equal(isDetailSnapshotStale(1_000_000, 1_010_000, 2_000), true);
  assert.equal(isDetailSnapshotStale(1_000_000, 1_015_000, 5_000), true);
});

test("hub detail states map to a finite, honest status vocabulary", () => {
  const mapping = {
    ready: "ready",
    loading: "loading",
    disconnected: "disconnected",
    withheld: "withheld",
    unsupported: "unsupported",
    busy: "busy",
    expired: "expired",
    failed: "failed",
    cancelled: "cancelled",
    somethingNew: "error",
  } as const;
  for (const [state, phase] of Object.entries(mapping)) {
    assert.equal(detailStateToPhase(state), phase);
  }

  assert.equal(phasePresentation("ready").tone, "ok");
  assert.equal(phasePresentation("paused").tone, "muted");
  assert.equal(phasePresentation("withheld").retryable, true);
  assert.equal(phasePresentation("unsupported").retryable, true);
  assert.match(phasePresentation("disconnected").description, /last snapshot/i);
  assert.match(phasePresentation("busy").description, /too many process views/i);
});

test("acquire response parsing accepts only positive finite terms", () => {
  assert.deepEqual(parseAcquireResponse({ leaseMs: 15_000, intervalMs: 2_000 }), {
    leaseMs: 15_000,
    intervalMs: 2_000,
  });
  assert.equal(parseAcquireResponse({ leaseMs: 0, intervalMs: 2_000 }), null);
  assert.equal(parseAcquireResponse({ leaseMs: 15_000, intervalMs: "2000" }), null);
  assert.equal(parseAcquireResponse(null), null);
});

test("poll response parsing validates the embedded detail result strictly", () => {
  const result: DetailResult = {
    type: "detail.result",
    subscriptionId: "subscription-1",
    capturedAt: "2026-11-02T12:00:00.000Z",
    snapshot: {
      totalProcesses: 1,
      truncated: false,
      inaccessible: 0,
      processes: [row(1, { command: "safe to parse" })],
    },
  };
  const parsed = parseDetailPollResponse({ state: "ready", result });
  assert.equal(parsed?.state, "ready");
  assert.equal(parsed?.result?.subscriptionId, "subscription-1");
  assert.equal(parsed?.result?.snapshot.processes[0].pid, 1);

  assert.deepEqual(parseDetailPollResponse({ state: "loading", result: null }), {
    state: "loading",
    result: null,
  });
  assert.equal(parseDetailPollResponse({ result: null }), null);
  assert.equal(parseDetailPollResponse({ state: "ready", result: { type: "detail.result" } }), null);
  assert.equal(parseDetailPollResponse("nonsense"), null);
});
