"use client";

/**
 * Export buttons for the loaded History result. Both downloads are built from
 * the result already on screen and the series currently visible in it.
 */

import { useState } from "react";
import { AlertTriangle } from "lucide-react";

import type { MachineState } from "@/contracts";
import type { HistoryResult } from "@/hub/storage/history-query";
import type { RangeId } from "@/history/presentation";
import { CSV_MIME, JSON_MIME, downloadFile } from "@/exports/csv";
import {
  buildHistoryCsv,
  buildHistoryJson,
  historyExportFilename,
  visibleHistorySeries,
  type HistoryExportInput,
} from "@/exports/history";

const BUTTON =
  "inline-flex min-h-7 items-center rounded-md border border-border bg-background px-2 text-xs text-foreground transition-colors hover:bg-card-strong disabled:cursor-not-allowed disabled:opacity-50";

type Props = {
  machine: MachineState;
  result: HistoryResult;
  range: RangeId;
  selectedMetrics: readonly string[];
  hiddenCards: ReadonlySet<string>;
  hiddenSeries: ReadonlySet<string>;
  coreIds?: readonly string[];
  loadedAt: number;
};

export function HistoryExportControls(props: Props) {
  const [error, setError] = useState<string | null>(null);
  const seriesCount = visibleHistorySeries(props).length;

  const trigger = (format: "csv" | "json"): void => {
    const exportedAt = Date.now();
    const input: HistoryExportInput = { ...props, exportedAt };
    try {
      const filename = historyExportFilename(input, format, exportedAt);
      const contents = format === "csv" ? buildHistoryCsv(input) : buildHistoryJson(input);
      downloadFile(filename, contents, format === "csv" ? CSV_MIME : JSON_MIME);
      setError(null);
    } catch {
      setError("The history export could not be created.");
    }
  };

  return (
    <div className="flex min-w-0 flex-wrap items-center gap-1.5">
      <span className="text-xs text-muted-foreground">
        Export visible
        <span className="sr-only">
          {` (${seriesCount} series from the loaded result)`}
        </span>
      </span>
      <button
        type="button"
        onClick={() => trigger("csv")}
        aria-label="Download visible history as CSV"
        title="Download the visible series from this loaded result as CSV"
        className={BUTTON}
      >
        CSV
      </button>
      <button
        type="button"
        onClick={() => trigger("json")}
        aria-label="Download visible history as JSON"
        title="Download the visible series from this loaded result as JSON"
        className={BUTTON}
      >
        JSON
      </button>
      {error !== null ? (
        <span role="status" className="inline-flex items-center gap-1 text-xs text-warn">
          <AlertTriangle className="h-3 w-3" aria-hidden />
          {error}
        </span>
      ) : null}
    </div>
  );
}
