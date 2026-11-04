"use client";

/**
 * Export the current process snapshot: the same search, sort and view scope
 * as the table, with its capture time and freshness. Nothing is re-requested.
 */

import { useState } from "react";
import { AlertTriangle } from "lucide-react";

import type { MachineState, ProcessSnapshot } from "@/contracts";
import type { AppGroup, TreeNode } from "@/processes/aggregate";
import type {
  ProcessDetailPhase,
  ProcessViewMode,
  SortState,
} from "@/processes/presentation";
import { CSV_MIME, JSON_MIME, downloadFile } from "@/exports/csv";
import {
  buildProcessCsv,
  buildProcessJson,
  processExportFilename,
  processExportEntries,
  type ProcessExportInput,
} from "@/exports/processes";

const BUTTON =
  "inline-flex min-h-7 items-center rounded-md border border-border bg-background px-2 text-xs text-foreground transition-colors hover:bg-card-strong disabled:cursor-not-allowed disabled:opacity-50";

type Props = {
  machine: MachineState;
  snapshot: ProcessSnapshot | null;
  capturedAt: string | null;
  receivedAtMs: number | null;
  intervalMs: number;
  phase: ProcessDetailPhase;
  query: string;
  mode: ProcessViewMode;
  sort: SortState;
  filteredRows: ProcessExportInput["filteredRows"];
  sortedRows: ProcessExportInput["sortedRows"];
  groups: readonly AppGroup[];
  tree: readonly TreeNode[];
};

export function ProcessExportControls(props: Props) {
  const [message, setMessage] = useState<string | null>(null);
  const hasSnapshot = props.snapshot !== null && props.capturedAt !== null;
  const rowCount = hasSnapshot
    ? props.mode === "applications"
      ? props.filteredRows.length
      : props.mode === "tree"
        ? props.tree.length
        : props.sortedRows.length
    : 0;
  const disabled = !hasSnapshot || rowCount === 0;

  const trigger = (format: "csv" | "json"): void => {
    if (props.snapshot === null || props.capturedAt === null) return;
    const exportedAt = Date.now();
    const input: ProcessExportInput = { ...props, snapshot: props.snapshot, capturedAt: props.capturedAt, exportedAt };
    try {
      const filename = processExportFilename(input, format, exportedAt);
      const contents = format === "csv" ? buildProcessCsv(input) : buildProcessJson(input);
      downloadFile(filename, contents, format === "csv" ? CSV_MIME : JSON_MIME);
      setMessage(`${format.toUpperCase()} downloaded · ${rowCount} ${rowCount === 1 ? "process" : "processes"}`);
    } catch {
      setMessage("The process export could not be created.");
    }
  };

  const disabledTitle = !hasSnapshot
    ? "A process snapshot is not available yet"
    : rowCount === 0
      ? "No processes match the current search"
      : undefined;

  return (
    <div className="flex min-w-0 shrink-0 items-center gap-1.5">
      <span className="hidden text-xs text-muted-foreground sm:inline">Export</span>
      <button
        type="button"
        onClick={() => trigger("csv")}
        disabled={disabled}
        aria-label="Download current process snapshot as CSV"
        title={disabledTitle ?? "Download the filtered process snapshot as CSV"}
        className={BUTTON}
      >
        CSV
      </button>
      <button
        type="button"
        onClick={() => trigger("json")}
        disabled={disabled}
        aria-label="Download current process snapshot as JSON"
        title={disabledTitle ?? "Download the filtered process snapshot as JSON"}
        className={BUTTON}
      >
        JSON
      </button>
      <span role="status" className={message?.includes("could not") ? "inline-flex items-center gap-1 text-xs text-warn" : "sr-only"}>
        {message?.includes("could not") ? <AlertTriangle className="h-3 w-3" aria-hidden /> : null}
        {message ?? ""}
      </span>
    </div>
  );
}
