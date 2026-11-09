/**
 * Common surface for the two telemetry history backends.
 *
 * The browser and recording code ask the same bounded question of either
 * backend, so charts never need a memory-mode branch. The concrete stores:
 *
 * - `MemoryTelemetryStore` keeps summaries in bounded typed-array ring buffers
 *   and acknowledges samples into process memory only.
 * - `TelemetryStore` commits compact ten-second summaries (or raw samples when
 *   explicitly requested) to SQLite and acknowledges after COMMIT.
 */

import type {
  HistoryQuery,
  HistoryQueryBudget,
  HistoryResult,
  HistoryTier,
} from "./history-query";

export type HistoryMode = "memory" | "persistent";

export type HistoryTierStatus = {
  tier: HistoryTier;
  stepMs: number;
  retentionMs: number;
  /** Estimated bytes currently allocated for this tier. */
  bytes: number;
};

export type MemoryHistoryStatus = {
  budgetBytes: number;
  usedBytes: number;
  pressure: boolean;
  seriesCount: number;
  machineCount: number;
  evictedSeries: number;
  rejectedSeries: number;
  oldestMs: number | null;
  newestMs: number | null;
  tiers: HistoryTierStatus[];
};

export type PersistentHistoryStatus = {
  enabled: true;
  storageTier: "raw" | "summary";
  flushIntervalMs: number;
  queuedRecords: number;
  queuedBatches: number;
  lastFlushMs: number | null;
  lastFlushDurationMs: number | null;
  writeFailures: number;
  lastWriteError: string | null;
  dbBytes: number;
  dbWalBytes: number;
  dbBytesLimit: number | null;
  tiers: HistoryTierStatus[];
};

export type HistoryStatus = {
  mode: HistoryMode;
  /** Human-readable truthful statement; see docs/monitoring.md. */
  persistentHistory: boolean;
  /** Oldest timestamp this backend can answer, or null when empty. */
  availableFromMs: number | null;
  availableToMs: number | null;
  memory: MemoryHistoryStatus | null;
  persistence: PersistentHistoryStatus | null;
  gaps: number;
  /** Cumulative records accepted by this store instance. */
  acceptedRecords: number;
  generatedAt: string;
};

export type HistorySource = {
  readonly historyMode: HistoryMode;
  queryHistory(query: HistoryQuery, budget?: HistoryQueryBudget): HistoryResult;
  historyStatus(): HistoryStatus;
};

export const EMPTY_MEMORY_STATUS: MemoryHistoryStatus = {
  budgetBytes: 0,
  usedBytes: 0,
  pressure: false,
  seriesCount: 0,
  machineCount: 0,
  evictedSeries: 0,
  rejectedSeries: 0,
  oldestMs: null,
  newestMs: null,
  tiers: [],
};
