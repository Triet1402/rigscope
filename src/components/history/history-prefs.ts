"use client";

import { usePersistentState } from "@/lib/persistent-state";
import { MAX_CORES } from "@/contracts";
import { HISTORY_METRICS } from "@/history/metrics";
import { RANGE_PRESETS, type RangeId } from "@/history/presentation";

const EMPTY: readonly string[] = Object.freeze([]);
const metricNames = new Set(HISTORY_METRICS.map((metric) => metric.metric));
const parseBool = (raw: unknown) => typeof raw === "boolean" ? raw : null;
const parseList = (raw: unknown): readonly string[] | null =>
  Array.isArray(raw) && raw.every((item) => typeof item === "string") ? raw.slice(0, 1000) : null;
const parseRange = (raw: unknown): RangeId | null =>
  RANGE_PRESETS.some((range) => range.id === raw) ? raw as RangeId : null;
const parseMetrics = (raw: unknown): string | null =>
  typeof raw === "string" && (raw === "" || raw.split(",").every((metric) => metricNames.has(metric))) ? raw : null;
const CORE_ID = /^core:\d+$/;
const parseCores = (raw: unknown): readonly string[] | null =>
  Array.isArray(raw) && raw.every((item) => typeof item === "string" && CORE_ID.test(item))
    ? [...new Set(raw)].slice(0, MAX_CORES)
    : null;

export function useHistoryRange() {
  return usePersistentState<RangeId>("history.range", "10m", parseRange);
}
export function useHistoryMetrics(machineId: string) {
  return usePersistentState<string | null>(`history.metrics.${machineId}`, null, parseMetrics);
}
export function useHistoryHidden(machineId: string, kind: "cards" | "series") {
  return usePersistentState<readonly string[]>(`history.hidden.${kind}.${machineId}`, EMPTY, parseList);
}
/**
 * Explicit per-core selection. `null` means all available cores; an empty
 * array means the user hid every core.
 */
export function useHistoryCores(machineId: string) {
  return usePersistentState<readonly string[] | null>(`history.cores.${machineId}`, null, parseCores);
}
export function useHistoryAutoRefresh() {
  return usePersistentState("history.autoRefresh", true, parseBool, "session");
}
export function useHistoryControlsOpen() {
  return usePersistentState("history.controlsOpen", false, parseBool, "session");
}

const parseCapacityUnit = (raw: unknown) => raw === "bytes" || raw === "percent" ? raw : null;
export function useHistoryCapacityUnit(machineId: string, group: "memory" | "filesystem") {
  return usePersistentState<"bytes" | "percent">(`history.units.${group}.${machineId}`, "bytes", parseCapacityUnit);
}
