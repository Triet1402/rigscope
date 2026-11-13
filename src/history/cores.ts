/** Per-core History selection, bounded by the collector's 1024-core contract. */

import { MAX_CORES, type MachineState } from "../contracts";
import type { HistorySeries } from "./contract";

export const MAX_VISIBLE_CORES = MAX_CORES;
/** Larger selections use a heatmap instead of overlapping line series. */
export const CORE_LINE_LIMIT = 8;

const CORE_ID = /^core:(\d+)$/;

export function coreResourceId(index: number): string {
  return `core:${index}`;
}

/** Core number from a stored resource id, or null for any other resource. */
export function coreIndex(resourceId: string): number | null {
  const match = CORE_ID.exec(resourceId);
  if (match === null) return null;
  const index = Number(match[1]);
  if (!Number.isSafeInteger(index) || index < 0 || index >= MAX_CORES) return null;
  return index;
}

function sortCoreIds(ids: readonly string[]): string[] {
  return [...new Set(ids)].sort((left, right) => (coreIndex(left) ?? 0) - (coreIndex(right) ?? 0));
}

function coreIdsFromCount(count: number): string[] {
  if (!Number.isFinite(count)) return [];
  const limit = Math.min(MAX_CORES, Math.max(0, Math.floor(count)));
  return Array.from({ length: limit }, (_, index) => coreResourceId(index));
}

/** Cores known from the current machine snapshot or from stored history. */
export function availableCoreIds(
  machine: MachineState | undefined,
  series: readonly HistorySeries[] = [],
): string[] {
  const cpu = machine?.metrics?.payload.cpu;
  const reported = Math.max(
    machine?.inventory?.cpu.logicalCores ?? 0,
    cpu?.status === "ok" ? cpu.perCore.length : 0,
  );
  const ids = new Set(coreIdsFromCount(reported));
  for (const item of series) {
    if (item.metric !== "cpu.core") continue;
    if (coreIndex(item.resourceId) !== null) ids.add(item.resourceId);
  }
  return sortCoreIds([...ids]);
}

/** Null selects every known core; an explicit empty list selects none. */
export function resolveCoreSelection(
  preference: readonly string[] | null,
  available: readonly string[],
): string[] {
  const availableSet = new Set(available);
  if (preference === null) return sortCoreIds(available).slice(0, MAX_VISIBLE_CORES);
  const selected: string[] = [];
  for (const id of preference) {
    if (!availableSet.has(id) || coreIndex(id) === null) continue;
    selected.push(id);
    if (selected.length >= MAX_VISIBLE_CORES) break;
  }
  return sortCoreIds(selected);
}

export function remainingCoreIds(available: readonly string[], selected: readonly string[]): string[] {
  const selectedSet = new Set(selected);
  return sortCoreIds(available).filter((id) => !selectedSet.has(id));
}

/** Human label for a stored core id; falls back to the bounded raw id. */
export function coreLabel(resourceId: string): string {
  const index = coreIndex(resourceId);
  return index === null ? resourceId : `Core ${index}`;
}
