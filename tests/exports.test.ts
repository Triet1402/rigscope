/**
 * Focused tests for the browser-safe export layer.
 *
 * Covers CSV quoting and spreadsheet formula injection, visible History scope
 * (hidden cards/series, unknown metric units), rollup/partial/gap honesty,
 * and process exports that follow the active search, sort and view without
 * leaking command lines.
 */
import assert from "node:assert/strict";
import test from "node:test";

import type { MachineState, ProcessRow, ProcessSnapshot } from "../src/contracts";
import type { HistoryResult, HistorySeries } from "../src/history/contract";
import {
  aggregateApplications,
  buildTree,
  sortGroups,
  sortRows,
} from "../src/processes/aggregate";
import {
  filterRowsForView,
  type ProcessViewMode,
} from "../src/processes/presentation";
import {
  CSV_MIME,
  JSON_MIME,
  csvCell,
  csvDocument,
  downloadFile,
  spreadsheetSafeText,
} from "../src/exports/csv";
import {
  buildHistoryCsv,
  buildHistoryJson,
  historyExportFilename,
  historySeriesKey,
  visibleHistorySeries,
  type HistoryExportInput,
} from "../src/exports/history";
import { compactTimestamp, filenameSlug } from "../src/exports/filenames";
import {
  buildProcessCsv,
  buildProcessJson,
  processExportEntries,
  processExportFilename,
  processFreshness,
  type ProcessExportInput,
} from "../src/exports/processes";

const MACHINE: MachineState = {
  machineId: "machine-1",
  collectorId: "collector-1",
  hostname: "rig.local",
  connected: true,
  lastReceivedAt: "2026-11-03T10:02:00.000Z",
};

const T0 = Date.parse("2026-11-03T10:00:00.000Z");
const T1 = T0 + 60_000;
const T2 = T0 + 120_000;
const T3 = T0 + 180_000;

test("History exports retain only the selected CPU cores alongside other metrics", () => {
  const result: HistoryResult = {machineId: MACHINE.machineId, fromMs: T0, toMs: T1, stepMs: 1000, tier: "raw",
    gaps: [], truncated: false, elapsedMs: 1, series: [
      {metric: "cpu.core", resourceId: "core:0", unit: "percent", points: []},
      {metric: "cpu.core", resourceId: "core:127", unit: "percent", points: []},
      {metric: "memory.used", resourceId: "", unit: "bytes", points: []},
    ]};
  const input: HistoryExportInput = {machine: MACHINE, result, range: "10m", selectedMetrics: ["cpu.core", "memory.used"],
    hiddenCards: new Set(), hiddenSeries: new Set(), coreIds: ["core:127"], loadedAt: T1};
  assert.deepEqual(visibleHistorySeries(input).map(s => [s.metric, s.resourceId]),
    [["cpu.core", "core:127"], ["memory.used", ""]]);
  assert.deepEqual(visibleHistorySeries({...input, coreIds: []}).map(s => s.metric), ["memory.used"]);
});

type Point = HistorySeries["points"][number];

function point(t: number, avg: number | null, spread = 2): Point {
  return avg === null
    ? { t, min: null, max: null, avg: null }
    : { t, min: avg - spread, max: avg + spread, avg };
}

function series(metric: string, resourceId: string, points: Point[], unit = "percent"): HistorySeries {
  return { metric, resourceId, unit, points };
}

function historyResult(): HistoryResult {
  return {
    machineId: "machine-1",
    fromMs: T0,
    toMs: T3,
    stepMs: 60_000,
    tier: "1m",
    series: [
      series("cpu.usage", "", [point(T0, 15), point(T1, null)]),
      series("gpu.utilization", "gpu:visible", [point(T0, 40)]),
      series("gpu.utilization", "gpu:hidden", [point(T0, 90)]),
      series("swap.used", "", [point(T0, 1024 * 1024)], "bytes"),
      series("cpu.temperature", "=cmd", [point(T0, 55)], "celsius"),
      series("net.rx", "net:eth0", [point(T0, 2048)], "bytes_per_second"),
    ],
    gaps: [
      { fromMs: T1, toMs: T1 + 30_000, kind: "missing", reason: "collector offline" },
      { fromMs: T2, toMs: T2 + 30_000, kind: "dropped", droppedRecords: 4 },
    ],
    truncated: true,
    elapsedMs: 5,
  };
}

function historyInput(overrides: Partial<HistoryExportInput> = {}): HistoryExportInput {
  return {
    machine: { ...MACHINE, displayName: "=Rig One" },
    result: historyResult(),
    range: "10m",
    selectedMetrics: [
      "cpu.usage",
      "gpu.utilization",
      "swap.used",
      "cpu.temperature",
      "memory.used",
    ],
    hiddenCards: new Set(["gpu:hidden"]),
    hiddenSeries: new Set([historySeriesKey("swap.used", "")]),
    loadedAt: T3 + 5_000,
    exportedAt: T3 + 10_000,
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// CSV primitives
// ---------------------------------------------------------------------------

test("CSV quotes commas, quotes, tabs, and newlines while leaving numbers numeric", () => {
  assert.equal(csvCell("plain"), "plain");
  assert.equal(csvCell("a,b"), '"a,b"');
  assert.equal(csvCell('say "hi"'), '"say ""hi"""');
  assert.equal(csvCell("line1\nline2"), '"line1\nline2"');
  assert.equal(csvCell("a\tb"), '"a\tb"');
  assert.equal(csvCell(42), "42");
  assert.equal(csvCell(-42), "-42");
  assert.equal(csvCell(Number.NaN), "");
  assert.equal(csvCell(null), "");
  assert.equal(csvCell(undefined), "");
  assert.equal(csvDocument([["a", "b,c"], [1, 2]]), 'a,"b,c"\r\n1,2\r\n');
});

test("spreadsheet text is protected from formula injection without touching numeric values", () => {
  assert.equal(spreadsheetSafeText("=SUM(A1:A2)"), "'=SUM(A1:A2)");
  assert.equal(spreadsheetSafeText("+cmd"), "'+cmd");
  assert.equal(spreadsheetSafeText("-1+1"), "'-1+1");
  assert.equal(spreadsheetSafeText("@import"), "'@import");
  assert.equal(spreadsheetSafeText(" =1+1"), "' =1+1");
  assert.equal(spreadsheetSafeText("\t=1+1"), "'\t=1+1");
  assert.equal(spreadsheetSafeText("normal"), "normal");
  assert.equal(csvCell("=HYPERLINK(\"http://example.test\")"), '"\'=HYPERLINK(""http://example.test"")"');
});

test("download helper creates and revokes its object URL", async () => {
  const clicks: string[] = [];
  const revoked: string[] = [];
  const originalDocument = Object.getOwnPropertyDescriptor(globalThis, "document");
  const originalCreate = URL.createObjectURL;
  const originalRevoke = URL.revokeObjectURL;
  const link = {
    href: "",
    download: "",
    rel: "",
    style: { display: "" },
    click() {
      clicks.push(this.download);
    },
    remove() {},
  };
  const body = { append() {} };
  const fakeDocument = {
    createElement() {
      return link;
    },
    body,
  };
  Object.defineProperty(globalThis, "document", { value: fakeDocument, configurable: true });
  URL.createObjectURL = () => "blob:rigscope-test";
  URL.revokeObjectURL = (url: string) => {
    revoked.push(url);
  };
  try {
    downloadFile("test.csv", "a,b", CSV_MIME);
    assert.deepEqual(clicks, ["test.csv"]);
    assert.equal(link.href, "blob:rigscope-test");
    assert.equal(CSV_MIME, "text/csv;charset=utf-8");
    assert.equal(JSON_MIME, "application/json;charset=utf-8");
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.deepEqual(revoked, ["blob:rigscope-test"]);
  } finally {
    if (originalDocument) Object.defineProperty(globalThis, "document", originalDocument);
    else Reflect.deleteProperty(globalThis, "document");
    URL.createObjectURL = originalCreate;
    URL.revokeObjectURL = originalRevoke;
  }
});

// ---------------------------------------------------------------------------
// History export
// ---------------------------------------------------------------------------

test("visible history excludes hidden cards/series and keeps unknown result metrics", () => {
  const input = historyInput();
  const visible = visibleHistorySeries(input);
  assert.deepEqual(
    visible.map((item) => `${item.metric}:${item.resourceId}`),
    ["cpu.usage:", "gpu.utilization:gpu:visible", "cpu.temperature:=cmd"],
  );
  assert.equal(visible.some((item) => item.metric === "swap.used"), false);
  assert.equal(visible.some((item) => item.metric === "net.rx"), false);
});

test("history CSV states visible scope, rollups, partial data, gaps and explicit nulls", () => {
  const csv = buildHistoryCsv(historyInput());
  assert.match(csv, /^Rigscope History export\r\n/);
  assert.ok(csv.includes("Scope,Visible series from the loaded History result"));
  assert.ok(csv.includes("Machine,'=Rig One"), "machine label is formula-protected");
  assert.ok(csv.includes("Query,CPU usage; GPU utilization; Swap used; CPU temperature; Memory used"));
  assert.ok(csv.includes("Resolution,1-minute rollups · 60000 ms buckets"));
  assert.ok(csv.includes("Partial,true"));
  assert.ok(csv.includes("Gaps,2"));
  assert.ok(csv.includes("Visible series,3"));
  assert.ok(csv.includes("Selected without stored data,memory.used"));
  assert.ok(csv.includes("not individual raw samples"));
  assert.ok(csv.includes("record_type,metric,metric_label,resource_id,resource_label,unit"));

  const cpu = csv.split("\r\n").filter((line) => line.startsWith("bucket,cpu.usage,"));
  assert.equal(cpu.length, 2);
  assert.ok(cpu[1].includes(",unavailable,"), "a null bucket is explicit, not zero");
  assert.ok(!cpu[1].includes("0,0,0"), "missing readings are not zero-filled");

  const temperature = csv.split("\r\n").find((line) => line.startsWith("bucket,cpu.temperature,"));
  assert.ok(temperature?.includes(",celsius,"));
  assert.ok(temperature?.includes("'=cmd"), "resource id is formula-protected");
  assert.ok(csv.includes("cpu.temperature"), "result unit exports without a fixed catalog entry");
  assert.ok(!csv.includes("gpu:hidden"), "hidden GPU card is excluded");
  assert.ok(!csv.includes("gpu:hidden"), "hidden card data is absent entirely");
  assert.ok(!csv.includes("net:eth0"), "unselected series is excluded");

  const gapLines = csv.split("\r\n").filter((line) => line.startsWith("gap,"));
  assert.equal(gapLines.length, 2);
  assert.ok(gapLines[0]?.includes(",missing,"));
  assert.ok(gapLines[0]?.includes("collector offline"));
  assert.ok(gapLines[1]?.includes(",dropped,"));
  assert.ok(gapLines[1]?.includes(",4"));
});

test("history JSON carries units, summaries, gaps, missing data and honest notes", () => {
  const raw = buildHistoryJson(historyInput());
  const doc = JSON.parse(raw) as {
    partial: boolean;
    resolution: { tier: string; label: string; stepMs: number };
    gaps: Array<{ kind: string; fromMs: number; droppedRecords?: number }>;
    series: Array<{
      metric: string;
      unit: string;
      summary: { averageOfBucketAverages: number | null };
      points: Array<{ state: string; min: number | null; max: number | null; avg: number | null }>;
    }>;
    missingData: Array<{ metric: string }>;
    notes: string[];
    scope: { machine: { name: string }; query: { visibleSeries: number } };
  };

  assert.equal(doc.partial, true);
  assert.equal(doc.resolution.tier, "1m");
  assert.equal(doc.resolution.label, "1-minute rollups");
  assert.equal(doc.resolution.stepMs, 60_000);
  assert.equal(doc.scope.machine.name, "=Rig One");
  assert.equal(doc.scope.query.visibleSeries, 3);
  assert.equal(doc.gaps.length, 2);
  assert.equal(doc.gaps[1]?.kind, "dropped");
  assert.equal(doc.gaps[1]?.droppedRecords, 4);

  const cpu = doc.series.find((item) => item.metric === "cpu.usage");
  assert.ok(cpu);
  assert.equal(cpu.unit, "percent");
  assert.equal(cpu.points[1]?.state, "unavailable");
  assert.equal(cpu.points[1]?.min, null);
  assert.equal(cpu.points[1]?.avg, null);
  assert.equal(cpu.summary.averageOfBucketAverages, 15);

  const temperature = doc.series.find((item) => item.metric === "cpu.temperature");
  assert.equal(temperature?.unit, "celsius");
  assert.ok(doc.missingData.some((item) => item.metric === "memory.used"));
  assert.ok(doc.notes.some((note) => note.includes("not individual raw samples")));
  assert.ok(doc.notes.some((note) => note.includes("partial")));
  assert.ok(!raw.includes("gpu:hidden"));
});

test("history filename is slugged and timestamped", () => {
  const input = historyInput();
  assert.equal(filenameSlug("=Rig One /../../"), "rig-one");
  assert.equal(compactTimestamp(Date.parse("2026-11-03T10:02:05.000Z")), "20261103T100205Z");
  assert.match(
    historyExportFilename(input, "csv", Date.parse("2026-11-03T10:02:05.000Z")),
    /^rigscope-history-rig-one-20261103T100205Z\.csv$/,
  );
});

// ---------------------------------------------------------------------------
// Process export
// ---------------------------------------------------------------------------

function processRow(pid: number, overrides: Partial<ProcessRow> = {}): ProcessRow {
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

function snapshot(): ProcessSnapshot {
  return {
    totalProcesses: 4,
    truncated: true,
    inaccessible: 1,
    processes: [
      processRow(10, {
        name: "Firefox",
        user: "alice",
        cpuPercent: 20,
        unit: "app-gnome-firefox-1.scope",
        command: "--private-token=secret",
      }),
      processRow(11, {
        name: "worker",
        user: "=bob",
        cpuPercent: 40,
        unit: "app-gnome-firefox-1.scope",
        command: "--secret-argument",
      }),
      processRow(12, {
        name: "sshd",
        user: null,
        cpuPercent: null,
        gpuMemoryBytes: 0,
        ioReadBytesPerSec: 0,
        ioWriteBytesPerSec: 0,
        unit: "ssh.service",
      }),
      processRow(13, { name: "=cmd", user: "root", cpuPercent: 5 }),
    ],
  };
}

function processInput(
  mode: ProcessViewMode = "processes",
  query = "",
  overrides: Partial<ProcessExportInput> = {},
): ProcessExportInput {
  const current = snapshot();
  const filteredRows = filterRowsForView(current.processes, query);
  const sort = mode === "applications" ? { key: "cpu" as const, dir: "desc" as const } : { key: "cpu" as const, dir: "desc" as const };
  const sortedRows = sortRows(filteredRows, sort.key, sort.dir);
  return {
    machine: MACHINE,
    snapshot: current,
    capturedAt: "2026-11-03T10:02:00.000Z",
    receivedAtMs: Date.parse("2026-11-03T10:02:01.000Z"),
    intervalMs: 1_000,
    phase: "ready",
    query,
    mode,
    sort,
    filteredRows,
    sortedRows,
    groups: sortGroups(aggregateApplications(filteredRows), sort.key, sort.dir),
    tree: buildTree(sortedRows),
    exportedAt: Date.parse("2026-11-03T10:02:01.000Z"),
    ...overrides,
  };
}

test("process CSV follows the active sort and never writes command lines or hidden fields", () => {
  const input = processInput();
  assert.deepEqual(
    processExportEntries(input).map((entry) => entry.row.pid),
    [11, 10, 13, 12],
  );

  const csv = buildProcessCsv(input);
  assert.ok(csv.includes("Snapshot captured,2026-11-03T10:02:00.000Z"));
  assert.ok(csv.includes("Freshness,live"));
  assert.ok(csv.includes("Search,(none)"));
  assert.ok(csv.includes("Collector truncated,true"));
  assert.ok(csv.includes("Snapshot rows,4 shown of 4"));
  assert.ok(csv.includes("Unreadable,1"));
  assert.ok(!csv.includes("secret"), "command arguments never reach the export");
  assert.ok(!csv.includes("--private-token"));
  assert.ok(csv.includes("worker,11,'=bob,"), "user text is formula-protected");
  assert.ok(csv.includes("'=cmd,13,root,"), "process name is formula-protected");

  const lines = csv.split("\r\n");
  const headerIndex = lines.findIndex((line) => line.startsWith("name,pid,user"));
  assert.ok(headerIndex > 0);
  assert.ok(lines[headerIndex + 1]?.startsWith("worker,11,"), "descending CPU order is preserved");
  assert.ok(lines[headerIndex + 2]?.startsWith("Firefox,10,"));
  assert.ok(lines.some((line) => line.startsWith("sshd,12,") && line.endsWith(",cpu")));
});

test("process search scope is honored, including shared application grouping", () => {
  const filtered = processInput("processes", "firefox");
  assert.equal(processExportEntries(filtered).length, 2);
  assert.deepEqual(processExportEntries(filtered).map((entry) => entry.row.pid), [11, 10]);
  const scopedCsv = buildProcessCsv(filtered);
  assert.ok(scopedCsv.includes("Search,firefox"));
  assert.ok(!scopedCsv.includes("sshd,"));

  const applications = processInput("applications");
  const entries = processExportEntries(applications);
  assert.equal(entries.length, 4, "every visible process appears exactly once");
  assert.deepEqual(entries.map((entry) => entry.row.pid), [11, 10, 13, 12]);
  assert.equal(entries[0]?.application, "firefox");
  assert.equal(entries[0]?.applicationKind, "unit");
  const appCsv = buildProcessCsv(applications);
  assert.ok(appCsv.includes("worker,11,'=bob,firefox,unit,0,"));
  assert.ok(appCsv.includes("'=cmd,13,root,'=cmd,tree,0,"));
});

test("process JSON is scoped, fresh, and free of hidden process data", () => {
  const stale = processInput();
  stale.receivedAtMs = T0;
  const staleCsv = buildProcessCsv(stale);
  assert.ok(staleCsv.includes("Freshness,stale"));

  const raw = buildProcessJson(processInput());
  const doc = JSON.parse(raw) as {
    snapshot: { capturedAt: string; freshness: string; live: boolean; truncated: boolean };
    exportedRows: number;
    processes: Array<Record<string, unknown>>;
    scope: { query: string; view: string; sort: { key: string; dir: string } };
  };
  assert.equal(doc.snapshot.capturedAt, "2026-11-03T10:02:00.000Z");
  assert.equal(doc.snapshot.freshness, "live");
  assert.equal(doc.snapshot.live, true);
  assert.equal(doc.snapshot.truncated, true);
  assert.equal(doc.exportedRows, 4);
  assert.equal(doc.scope.view, "processes");
  assert.deepEqual(doc.scope.sort, { key: "cpu", dir: "desc" });
  assert.deepEqual(doc.processes.map((row) => row.pid), [11, 10, 13, 12]);
  assert.equal(doc.processes[0]?.user, "=bob", "JSON preserves raw values; CSV is the spreadsheet guard");
  assert.ok(!raw.includes("secret"));
  assert.ok(doc.processes.every((row) => !("command" in row)));
  assert.ok(doc.processes.every((row) => !("startTicks" in row) && !("ppid" in row)));
});

test("process freshness states never present retained data as live", () => {
  const now = T0 + 30_000;
  assert.equal(processFreshness({ phase: "paused", receivedAtMs: T0, intervalMs: 1_000, now }).label, "paused");
  assert.equal(
    processFreshness({ phase: "disconnected", receivedAtMs: T0, intervalMs: 1_000, now }).label,
    "disconnected",
  );
  assert.equal(processFreshness({ phase: "ready", receivedAtMs: T0, intervalMs: 1_000, now }).label, "stale");
  assert.equal(processFreshness({ phase: "ready", receivedAtMs: now - 500, intervalMs: 1_000, now }).label, "live");
  assert.equal(processFreshness({ phase: "failed", receivedAtMs: T0, intervalMs: 1_000, now }).label, "not live");
  assert.equal(processFreshness({ phase: "ready", receivedAtMs: null, intervalMs: 1_000, now }).label, "unknown");
});

test("process filename uses the snapshot capture time", () => {
  const input = processInput();
  assert.match(
    processExportFilename(input, "json"),
    /^rigscope-processes-rig-local-20261103T100200Z\.json$/,
  );
});
