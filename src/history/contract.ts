/**
 * Browser-side history result types.
 *
 * The hub's response shape is generated from Go in `src/contracts/api.generated.ts`.
 * The UI deliberately widens `HistorySeries.unit` to `string`: a newer hub may
 * serve a metric this build does not know, and the charts and CSV export still
 * have to render it honestly instead of rejecting the whole response.
 *
 * No Node APIs and no server code: this module is the UI's view of the
 * `/api/history` contract.
 */

import type {
  HistoryGap,
  HistoryPoint,
  HistoryResult as ApiHistoryResult,
  HistorySeries as ApiHistorySeries,
  HistoryTier,
  MetricUnit,
} from "../contracts/api.generated";

export type { HistoryGap, HistoryPoint, HistoryTier, MetricUnit };

/** One metric/resource series, trimmed to the fields the UI reads. */
export type HistorySeries = Omit<ApiHistorySeries, "unit" | "points"> & {
  unit: string;
  points: HistoryPoint[];
};

/** GET /api/history response, with browser-widened series units. */
export type HistoryResult = Omit<ApiHistoryResult, "series"> & {
  series: HistorySeries[];
};

/** Query accepted by the hub's `/api/history` endpoint. */
export type HistoryQuery = {
  machineId: string;
  metrics: string[];
  resourceIds?: string[];
  /** Restrict only cpu.core; other metric families retain their resources. */
  coreIds?: string[];
  fromMs: number;
  toMs: number;
  maxPoints?: number;
};
