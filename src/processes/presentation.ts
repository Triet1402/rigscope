/**
 * Deterministic presentation helpers for the Processes screen.
 *
 * Browser-safe and React-free: the component layer owns timers and fetch,
 * while this module owns the rules that must stay honest and testable:
 * command lines are never displayed, sort defaults are predictable, group
 * metrics from partially-observed members say "at least", and the status
 * vocabulary never turns an error into fresh data.
 */
import {
  parseCollectorMessage,
  type DetailResult,
  type ProcessRow,
  type ProcessSnapshot,
} from "../contracts";
import { formatBytes, formatPercent } from "../live/presentation";
import {
  aggregateApplications,
  buildTree,
  filterRows,
  sortRows,
  type AppGroup,
  type SortKey,
  type TreeNode,
} from "./aggregate";

export type ProcessViewMode = "applications" | "processes" | "tree";
export type SortDirection = "asc" | "desc";
export type SortState = { key: SortKey; dir: SortDirection };

export type ProcessDetailPhase =
  | "idle"
  | "acquiring"
  | "loading"
  | "ready"
  | "paused"
  | "stale"
  | "disconnected"
  | "withheld"
  | "unsupported"
  | "busy"
  | "expired"
  | "failed"
  | "cancelled"
  | "error";

export type PhaseTone = "ok" | "warn" | "crit" | "muted";

export type PhasePresentation = {
  label: string;
  tone: PhaseTone;
  description: string;
  /** True when the user must act (pause/resume or retry) rather than wait. */
  retryable: boolean;
};

export type MetricKind = "percent" | "bytes" | "rate" | "count";

export type MetricDisplay = {
  text: string;
  /** The total only covers members that reported the metric. */
  atLeast: boolean;
  title: string | null;
};

export type SnapshotSummary = {
  total: number;
  shown: number;
  inaccessible: number;
  truncated: boolean;
};

export const DEFAULT_SORT: SortState = { key: "cpu", dir: "desc" };

/** A live detail view is stale when no new snapshot has arrived for this long. */
export const DETAIL_STALE_FLOOR_MS = 10_000;

export function detailStaleAfterMs(intervalMs: number): number {
  if (!Number.isFinite(intervalMs) || intervalMs <= 0) return DETAIL_STALE_FLOOR_MS;
  return Math.max(DETAIL_STALE_FLOOR_MS, 3 * intervalMs);
}

export function isDetailSnapshotStale(
  lastReceivedMs: number | null,
  now: number,
  intervalMs: number,
): boolean {
  if (lastReceivedMs === null) return false;
  return now - lastReceivedMs >= detailStaleAfterMs(intervalMs);
}


export const VIEW_MODES: ReadonlyArray<{ value: ProcessViewMode; label: string }> = [
  { value: "applications", label: "Applications" },
  { value: "processes", label: "Processes" },
  { value: "tree", label: "Tree" },
];

/** Stable identity for a process row across refreshes. */
export function rowKey(row: ProcessRow): string {
  return `${row.pid}:${row.startTicks}`;
}

/**
 * Remove a delivered command line before the browser presents, filters, or
 * aggregates the row. The UI never displays arguments or environment data;
 * this also prevents a hidden argument from matching a search query.
 */
export function withoutCommand(row: ProcessRow): ProcessRow {
  if (row.command === undefined) return row;
  const { command: _command, ...rest } = row;
  return rest;
}

export function displayRows(rows: ProcessRow[]): ProcessRow[] {
  return rows.map(withoutCommand);
}

/** Filter the rows the screen is allowed to show, without mutating them. */
export function filterRowsForView(rows: ProcessRow[], query: string): ProcessRow[] {
  return filterRows(displayRows(rows), query);
}

export function filterAndSortRows(rows: ProcessRow[], query: string, sort: SortState): ProcessRow[] {
  return sortRows(filterRowsForView(rows, query), sort.key, sort.dir);
}

/** Aggregate only the filtered rows so group totals match the visible subset. */
export function applicationGroupsForRows(rows: ProcessRow[], query: string): AppGroup[] {
  return aggregateApplications(filterRowsForView(rows, query));
}

/**
 * Build the tree after sorting so siblings and roots follow the chosen sort,
 * while `buildTree` still guarantees parents precede children.
 */
export function treeForRows(rows: ProcessRow[], query: string, sort: SortState): TreeNode[] {
  return buildTree(sortRows(filterRowsForView(rows, query), sort.key, sort.dir));
}

export function defaultDirection(key: SortKey): SortDirection {
  return key === "name" || key === "user" || key === "pid" ? "asc" : "desc";
}

/** Clicking the active column flips direction; a new column gets its natural default. */
export function nextSortState(current: SortState, key: SortKey): SortState {
  if (current.key === key) {
    return { key, dir: current.dir === "asc" ? "desc" : "asc" };
  }
  return { key, dir: defaultDirection(key) };
}

export function sortAriaValue(current: SortState, key: SortKey): "ascending" | "descending" | "none" {
  if (current.key !== key) return "none";
  return current.dir === "asc" ? "ascending" : "descending";
}

export function metricDisplay(value: number | null, kind: MetricKind, partial = false): MetricDisplay {
  if (value === null || !Number.isFinite(value) || value < 0) {
    return { text: "—", atLeast: false, title: null };
  }
  let text: string;
  switch (kind) {
    case "percent":
      text = formatPercent(value);
      break;
    case "bytes":
      text = formatBytes(value);
      break;
    case "rate":
      text = `${formatBytes(value)}/s`;
      break;
    case "count":
      text = formatInteger(value);
      break;
  }
  return {
    text,
    atLeast: partial,
    title: partial ? `At least ${text}: some processes in this group did not report this metric.` : null,
  };
}

export function formatInteger(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return "—";
  return Math.round(value).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

export function formatThreadCount(value: number | null | undefined): string {
  return formatInteger(value);
}

export function snapshotSummary(snapshot: ProcessSnapshot | null): SnapshotSummary {
  if (snapshot === null) return { total: 0, shown: 0, inaccessible: 0, truncated: false };
  const shown = snapshot.processes.length;
  return {
    total: snapshot.totalProcesses,
    shown,
    inaccessible: snapshot.inaccessible,
    truncated: snapshot.truncated,
  };
}

/** Map a hub `state` string onto the screen's finite status vocabulary. */
export function detailStateToPhase(state: string): ProcessDetailPhase {
  switch (state) {
    case "ready":
      return "ready";
    case "loading":
      return "loading";
    case "paused":
      return "paused";
    case "disconnected":
      return "disconnected";
    case "withheld":
      return "withheld";
    case "unsupported":
      return "unsupported";
    case "busy":
      return "busy";
    case "expired":
      return "expired";
    case "failed":
      return "failed";
    case "cancelled":
      return "cancelled";
    default:
      return "error";
  }
}

export function phasePresentation(phase: ProcessDetailPhase): PhasePresentation {
  switch (phase) {
    case "idle":
      return {
        label: "Not connected",
        tone: "muted",
        description: "Select a machine to request process detail.",
        retryable: false,
      };
    case "acquiring":
      return {
        label: "Connecting",
        tone: "warn",
        description: "Requesting process detail…",
        retryable: false,
      };
    case "loading":
      return {
        label: "Waiting",
        tone: "warn",
        description: "Waiting for the collector's first process snapshot…",
        retryable: false,
      };
    case "ready":
      return {
        label: "Live",
        tone: "ok",
        description: "Process detail is live.",
        retryable: false,
      };
    case "paused":
      return {
        label: "Paused",
        tone: "muted",
        description: "Process updates are paused.",
        retryable: false,
      };
    case "stale":
      return {
        label: "Not refreshing",
        tone: "warn",
        description: "The last snapshot is retained, but new snapshots are not arriving.",
        retryable: true,
      };
    case "disconnected":
      return {
        label: "Disconnected",
        tone: "crit",
        description: "The collector is disconnected; the last snapshot is not live.",
        retryable: true,
      };
    case "withheld":
      return {
        label: "Withheld",
        tone: "warn",
        description: "Process detail is withheld for this hub.",
        retryable: true,
      };
    case "unsupported":
      return {
        label: "Unsupported",
        tone: "warn",
        description: "This collector cannot provide process detail.",
        retryable: true,
      };
    case "busy":
      return {
        label: "Busy",
        tone: "warn",
        description: "Too many process views are open for this hub.",
        retryable: true,
      };
    case "expired":
      return {
        label: "Expired",
        tone: "warn",
        description: "Process detail expired before a fresh snapshot arrived.",
        retryable: true,
      };
    case "failed":
      return {
        label: "Scan failed",
        tone: "crit",
        description: "The process scan failed repeatedly.",
        retryable: true,
      };
    case "cancelled":
      return {
        label: "Stopped",
        tone: "muted",
        description: "The process detail view was stopped.",
        retryable: true,
      };
    case "error":
      return {
        label: "Unavailable",
        tone: "crit",
        description: "Process detail is unavailable right now.",
        retryable: true,
      };
  }
}

export function groupKindLabel(kind: AppGroup["kind"]): string {
  if (kind === "unit") return "systemd unit";
  if (kind === "kernel") return "kernel";
  return "process tree";
}

/** Validate the POST response without trusting an unexpected payload. */
export function parseAcquireResponse(value: unknown): { leaseMs: number; intervalMs: number } | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const leaseMs = (value as { leaseMs?: unknown }).leaseMs;
  const intervalMs = (value as { intervalMs?: unknown }).intervalMs;
  if (!isPositiveFinite(leaseMs) || !isPositiveFinite(intervalMs)) return null;
  return { leaseMs: Math.round(leaseMs), intervalMs: Math.round(intervalMs) };
}

/**
 * Validate GET `/api/processes` and the embedded collector `detail.result`
 * with the shared strict parser. A malformed result is never presented as a
 * snapshot; it becomes a local error instead.
 */
export function parseDetailPollResponse(value: unknown): { state: string; result: DetailResult | null } | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const state = (value as { state?: unknown }).state;
  if (typeof state !== "string" || state.length === 0) return null;
  const raw = (value as { result?: unknown }).result;
  if (raw === null || raw === undefined) return { state, result: null };
  try {
    const parsed = parseCollectorMessage(raw);
    if (parsed.type !== "detail.result") return null;
    return { state, result: parsed };
  } catch {
    return null;
  }
}

function isPositiveFinite(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value) && value > 0;
}
