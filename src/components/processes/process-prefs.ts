"use client";

/**
 * Persisted Processes view preferences: view mode, per-table sort, column
 * widths (browser-wide), plus search, expanded groups and pause. The
 * selected machine follows the viewer through the shared Machine
 * selection module, not a preference here.
 */

import type { SortKey } from "@/processes/aggregate";
import { DEFAULT_SORT, type ProcessViewMode, type SortState } from "@/processes/presentation";
import { usePersistentState } from "@/lib/persistent-state";

export type TableKind = "applications" | "processes";

export type ColumnDef = {
  id: string;
  label: string;
  sortKey?: SortKey;
  align: "left" | "right";
  width: number;
};

export const MIN_COLUMN_WIDTH = 56;
export const MAX_COLUMN_WIDTH = 1200;

export const APPLICATION_COLUMNS: readonly ColumnDef[] = [
  { id: "name", label: "Application", sortKey: "name", align: "left", width: 300 },
  { id: "count", label: "Procs", sortKey: "count", align: "right", width: 76 },
  { id: "cpu", label: "CPU / core", sortKey: "cpu", align: "right", width: 104 },
  { id: "memory", label: "RSS", sortKey: "memory", align: "right", width: 96 },
  { id: "gpuMemory", label: "VRAM", sortKey: "gpuMemory", align: "right", width: 96 },
  { id: "ioRead", label: "Read", sortKey: "ioRead", align: "right", width: 104 },
  { id: "ioWrite", label: "Write", sortKey: "ioWrite", align: "right", width: 104 },
  { id: "users", label: "Users", sortKey: "user", align: "left", width: 160 },
];

export const PROCESS_COLUMNS: readonly ColumnDef[] = [
  { id: "name", label: "Process", sortKey: "name", align: "left", width: 300 },
  { id: "pid", label: "PID", sortKey: "pid", align: "right", width: 84 },
  { id: "user", label: "User", sortKey: "user", align: "left", width: 120 },
  { id: "cpu", label: "CPU / core", sortKey: "cpu", align: "right", width: 104 },
  { id: "memory", label: "RSS", sortKey: "memory", align: "right", width: 96 },
  { id: "gpuMemory", label: "VRAM", sortKey: "gpuMemory", align: "right", width: 96 },
  { id: "ioRead", label: "Read", sortKey: "ioRead", align: "right", width: 104 },
  { id: "ioWrite", label: "Write", sortKey: "ioWrite", align: "right", width: 104 },
  { id: "threads", label: "Threads", align: "right", width: 84 },
];

export type ColumnWidths = Readonly<Record<string, number>>;

const EMPTY_WIDTHS: ColumnWidths = Object.freeze({});
const EMPTY_LIST: readonly string[] = Object.freeze([]);

const SORT_KEYS: ReadonlySet<string> = new Set<SortKey>([
  "cpu",
  "memory",
  "gpuMemory",
  "ioRead",
  "ioWrite",
  "name",
  "count",
  "pid",
  "user",
]);

export function clampWidth(width: number): number {
  return Math.round(Math.min(MAX_COLUMN_WIDTH, Math.max(MIN_COLUMN_WIDTH, width)));
}

function parseMode(raw: unknown): ProcessViewMode | null {
  return raw === "applications" || raw === "processes" || raw === "tree" ? raw : null;
}

function parseSort(raw: unknown): SortState | null {
  if (raw === null || typeof raw !== "object") return null;
  const { key, dir } = raw as { key?: unknown; dir?: unknown };
  if (typeof key !== "string" || !SORT_KEYS.has(key)) return null;
  if (dir !== "asc" && dir !== "desc") return null;
  return { key: key as SortKey, dir };
}

function parseWidths(raw: unknown): ColumnWidths | null {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const out: Record<string, number> = {};
  for (const [id, value] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof value === "number" && Number.isFinite(value)) out[id] = clampWidth(value);
  }
  return out;
}

function parseString(raw: unknown): string | null {
  return typeof raw === "string" ? raw.slice(0, 200) : null;
}

function parseBool(raw: unknown): boolean | null {
  return typeof raw === "boolean" ? raw : null;
}

function parseList(raw: unknown): readonly string[] | null {
  return Array.isArray(raw) && raw.every((item) => typeof item === "string") ? raw.slice(0, 500) : null;
}



export function useViewMode() {
  return usePersistentState<ProcessViewMode>("processes.mode", "applications", parseMode);
}

export function useTableSort(table: TableKind) {
  return usePersistentState<SortState>(`processes.sort.${table}`, DEFAULT_SORT, parseSort);
}

export function useColumnWidths(table: TableKind) {
  return usePersistentState<ColumnWidths>(`processes.widths.${table}`, EMPTY_WIDTHS, parseWidths);
}

export function useProcessQuery() {
  return usePersistentState<string>("processes.query", "", parseString, "session");
}

export function useExpandedGroups() {
  return usePersistentState<readonly string[]>("processes.expanded", EMPTY_LIST, parseList, "session");
}

export function useProcessesPaused() {
  return usePersistentState<boolean>("processes.paused", false, parseBool, "session");
}
