/**
 * Current process-snapshot export documents.
 *
 * The export is built from the snapshot the browser already has: it never
 * requests a new scan. Rows follow the active search and sort, and in the
 * application view they follow the visible application groups. Only fields
 * the Processes surface is allowed to show are written; command lines,
 * environment data and other hidden collector fields are never copied.
 */

import type { MachineState, ProcessRow, ProcessSnapshot } from "../contracts";
import {
  sortRows,
  type AppGroup,
  type TreeNode,
} from "../processes/aggregate";
import {
  isDetailSnapshotStale,
  type ProcessDetailPhase,
  type ProcessViewMode,
  type SortState,
} from "../processes/presentation";
import { csvDocument, type CsvValue } from "./csv";
import { exportFilename } from "./filenames";

export type ProcessExportInput = {
  machine: MachineState;
  snapshot: ProcessSnapshot;
  capturedAt: string;
  /** Local browser receipt time of this snapshot. */
  receivedAtMs: number | null;
  intervalMs: number;
  phase: ProcessDetailPhase;
  query: string;
  mode: ProcessViewMode;
  sort: SortState;
  /** Rows after the active search, already stripped of non-presented fields. */
  filteredRows: readonly ProcessRow[];
  /** Rows after the active search and sort (the process/tree table order). */
  sortedRows: readonly ProcessRow[];
  groups: readonly AppGroup[];
  tree: readonly TreeNode[];
  exportedAt?: number;
};

export type ProcessExportEntry = {
  row: ProcessRow;
  application: string | null;
  applicationKind: AppGroup["kind"] | null;
  depth: number;
};

export type ProcessFreshness = {
  label: "live" | "stale" | "paused" | "disconnected" | "not live" | "waiting" | "unknown";
  ageMs: number | null;
  stale: boolean;
  note: string;
};

const METRIC_FIELDS: ReadonlyArray<{ key: keyof ProcessRow; label: string }> = [
  { key: "cpuPercent", label: "cpu" },
  { key: "memoryRssBytes", label: "memory" },
  { key: "gpuMemoryBytes", label: "vram" },
  { key: "ioReadBytesPerSec", label: "read" },
  { key: "ioWriteBytesPerSec", label: "write" },
  { key: "threads", label: "threads" },
];

function iso(ms: number): string {
  return new Date(ms).toISOString();
}

export function processMachineLabel(machine: MachineState): string {
  return machine.displayName?.trim() || machine.hostname.trim() || machine.machineId;
}

export function processExportEntries(input: ProcessExportInput): ProcessExportEntry[] {
  if (input.mode === "applications") {
    return input.groups.flatMap((group) =>
      sortRows([...group.members], input.sort.key, input.sort.dir).map((row) => ({
        row,
        application: group.label,
        applicationKind: group.kind,
        depth: 0,
      })),
    );
  }
  if (input.mode === "tree") {
    return input.tree.map((node) => ({
      row: node.row,
      application: null,
      applicationKind: null,
      depth: node.depth,
    }));
  }
  return input.sortedRows.map((row) => ({
    row,
    application: null,
    applicationKind: null,
    depth: 0,
  }));
}

export function processFreshness(input: {
  phase: ProcessDetailPhase;
  receivedAtMs: number | null;
  intervalMs: number;
  now?: number;
}): ProcessFreshness {
  const now = input.now ?? Date.now();
  const ageMs = input.receivedAtMs === null ? null : Math.max(0, now - input.receivedAtMs);
  const age = ageMs === null ? "receipt time unknown" : `${Math.round(ageMs / 1_000)}s old`;

  if (input.receivedAtMs === null) {
    return {
      label: "unknown",
      ageMs: null,
      stale: true,
      note: "No browser receipt time is available for this snapshot.",
    };
  }
  if (input.phase === "paused") {
    return { label: "paused", ageMs, stale: false, note: `Paused · ${age}. Not live.` };
  }
  if (input.phase === "disconnected") {
    return {
      label: "disconnected",
      ageMs,
      stale: true,
      note: `Collector disconnected · last snapshot ${age}. Not live.`,
    };
  }
  if (input.phase === "withheld" || input.phase === "unsupported") {
    return {
      label: "not live",
      ageMs,
      stale: true,
      note: `Process detail is ${input.phase} · last snapshot ${age}.`,
    };
  }
  if (
    input.phase === "failed" ||
    input.phase === "expired" ||
    input.phase === "busy" ||
    input.phase === "cancelled" ||
    input.phase === "error"
  ) {
    return {
      label: "not live",
      ageMs,
      stale: true,
      note: `Refresh problem · last snapshot ${age}. Not live.`,
    };
  }
  if (input.phase === "acquiring" || input.phase === "loading" || input.phase === "idle") {
    return { label: "waiting", ageMs, stale: true, note: `Waiting for a fresh snapshot · ${age}.` };
  }
  if (isDetailSnapshotStale(input.receivedAtMs, now, input.intervalMs)) {
    return {
      label: "stale",
      ageMs,
      stale: true,
      note: `No new snapshot has arrived · ${age}. Not live.`,
    };
  }
  return { label: "live", ageMs, stale: false, note: `Live · updated ${age}.` };
}

function unavailableMetrics(row: ProcessRow): string {
  return METRIC_FIELDS
    .filter(({ key }) => row[key] === null)
    .map(({ label }) => label)
    .join(" ");
}

function scopeText(input: ProcessExportInput, exportedAt: number): string[] {
  const freshness = processFreshness({
    phase: input.phase,
    receivedAtMs: input.receivedAtMs,
    intervalMs: input.intervalMs,
    now: exportedAt,
  });
  const received = input.receivedAtMs === null ? "unknown" : iso(input.receivedAtMs);
  const query = input.query.trim();
  return [
    `Filtered process snapshot from the loaded view · ${freshness.note}`,
    `Search: ${query.length > 0 ? query : "(none)"} · view: ${input.mode} · sort: ${input.sort.key} ${input.sort.dir}`,
    `Snapshot ${input.snapshot.processes.length} shown of ${input.snapshot.totalProcesses} processes · ${input.snapshot.inaccessible} unreadable${
      input.snapshot.truncated ? " · collector kept the highest-CPU rows" : ""
    }`,
    `Captured ${input.capturedAt} · received ${received}`,
  ];
}

function metadataRows(input: ProcessExportInput, exportedAt: number): CsvValue[][] {
  const entries = processExportEntries(input);
  const freshness = processFreshness({
    phase: input.phase,
    receivedAtMs: input.receivedAtMs,
    intervalMs: input.intervalMs,
    now: exportedAt,
  });
  return [
    ["Rigscope process snapshot"],
    ["Scope", scopeText(input, exportedAt)[0]!],
    ["Machine", processMachineLabel(input.machine)],
    ["Machine ID", input.machine.machineId],
    ["Snapshot captured", input.capturedAt],
    ["Snapshot received", input.receivedAtMs === null ? "unknown" : iso(input.receivedAtMs)],
    ["Freshness", `${freshness.label}${freshness.ageMs === null ? "" : ` · ${Math.round(freshness.ageMs / 1_000)}s old`}`],
    ["Search", input.query.trim() || "(none)"],
    ["View", input.mode],
    ["Sort", `${input.sort.key} ${input.sort.dir}`],
    ["Snapshot rows", `${input.snapshot.processes.length} shown of ${input.snapshot.totalProcesses}`],
    ["Unreadable", String(input.snapshot.inaccessible)],
    ["Collector truncated", input.snapshot.truncated ? "true" : "false"],
    ["Exported rows", String(entries.length)],
    ["Note", scopeText(input, exportedAt).slice(1).join(" ")],
    [],
  ];
}

const PROCESS_HEADER: readonly CsvValue[] = [
  "name",
  "pid",
  "user",
  "application",
  "application_kind",
  "tree_depth",
  "cpu_percent",
  "memory_rss_bytes",
  "gpu_memory_bytes",
  "io_read_bytes_per_sec",
  "io_write_bytes_per_sec",
  "threads",
  "unavailable_metrics",
];

function entryCells(entry: ProcessExportEntry): CsvValue[] {
  const { row } = entry;
  return [
    row.name,
    row.pid,
    row.user,
    entry.application,
    entry.applicationKind,
    entry.depth,
    row.cpuPercent,
    row.memoryRssBytes,
    row.gpuMemoryBytes,
    row.ioReadBytesPerSec,
    row.ioWriteBytesPerSec,
    row.threads,
    unavailableMetrics(row),
  ];
}

export function buildProcessCsv(input: ProcessExportInput): string {
  const exportedAt = input.exportedAt ?? Date.now();
  const rows: CsvValue[][] = metadataRows(input, exportedAt);
  rows.push([...PROCESS_HEADER]);
  for (const entry of processExportEntries(input)) rows.push(entryCells(entry));
  return csvDocument(rows);
}

export function buildProcessJson(input: ProcessExportInput): string {
  const exportedAt = input.exportedAt ?? Date.now();
  const freshness = processFreshness({
    phase: input.phase,
    receivedAtMs: input.receivedAtMs,
    intervalMs: input.intervalMs,
    now: exportedAt,
  });
  const document = {
    app: "Rigscope",
    kind: "processes",
    version: 1,
    exportedAt: iso(exportedAt),
    scope: {
      description: "Filtered process snapshot from the loaded view",
      machine: {
        id: input.machine.machineId,
        name: processMachineLabel(input.machine),
        hostname: input.machine.hostname,
        connected: input.machine.connected,
      },
      query: input.query.trim(),
      view: input.mode,
      sort: input.sort,
    },
    snapshot: {
      capturedAt: input.capturedAt,
      receivedAt: input.receivedAtMs === null ? null : iso(input.receivedAtMs),
      ageMs: freshness.ageMs,
      freshness: freshness.label,
      live: !freshness.stale && freshness.label === "live",
      note: freshness.note,
      totalProcesses: input.snapshot.totalProcesses,
      shownProcesses: input.snapshot.processes.length,
      inaccessible: input.snapshot.inaccessible,
      truncated: input.snapshot.truncated,
    },
    exportedRows: processExportEntries(input).length,
    notes: scopeText(input, exportedAt),
    processes: processExportEntries(input).map((entry) => ({
      name: entry.row.name,
      pid: entry.row.pid,
      user: entry.row.user,
      application: entry.application,
      applicationKind: entry.applicationKind,
      treeDepth: entry.depth,
      cpuPercent: entry.row.cpuPercent,
      memoryRssBytes: entry.row.memoryRssBytes,
      gpuMemoryBytes: entry.row.gpuMemoryBytes,
      ioReadBytesPerSec: entry.row.ioReadBytesPerSec,
      ioWriteBytesPerSec: entry.row.ioWriteBytesPerSec,
      threads: entry.row.threads,
      unavailableMetrics: unavailableMetrics(entry.row).split(" ").filter(Boolean),
    })),
  };
  return `${JSON.stringify(document, null, 2)}\n`;
}

export function processExportFilename(
  input: Pick<ProcessExportInput, "machine" | "capturedAt">,
  extension: "csv" | "json",
  atMs = Date.now(),
): string {
  const captured = Date.parse(input.capturedAt);
  return exportFilename(
    "processes",
    processMachineLabel(input.machine),
    Number.isFinite(captured) ? captured : atMs,
    extension,
  );
}
