"use client";

/**
 * The working Processes surface: machine-scoped detail subscription,
 * search, native sort controls, application/process/tree views, an explicit
 * stale layer, and a visible pause. It never invents a process action and
 * never shows command lines or environment data.
 */

import { useDeferredValue, useMemo } from "react";
import {
  AlertTriangle,
  Hourglass,
  List,
  Pause,
  Play,
  RefreshCw,
  Search,
  ShieldAlert,
  WifiOff,
  X,
} from "lucide-react";

import type { MachineState } from "@/contracts";
import type { ProcessRow } from "@/contracts";
import { EmptyState, Panel } from "@/components/panel";
import {
  aggregateApplications,
  buildTree,
  sortGroups,
  sortRows,
  type SortKey,
  type TreeNode,
} from "@/processes/aggregate";
import {
  VIEW_MODES,
  filterRowsForView,
  formatInteger,
  isDetailSnapshotStale,
  nextSortState,
  phasePresentation,
  snapshotSummary,
  type PhasePresentation,
  type PhaseTone,
  type ProcessDetailPhase,
  type ProcessViewMode,
} from "@/processes/presentation";
import { formatAge } from "@/live/presentation";
import { useNow } from "@/live/use-now";
import { useProcessDetail } from "./use-process-detail";
import { ProcessViews } from "./process-views";
import { useExpandedGroups, useProcessQuery, useTableSort, useViewMode } from "./process-prefs";
import { ProcessExportControls } from "@/components/exports/process-export-controls";

const STATUS_STYLE: Record<PhaseTone, string> = {
  ok: "bg-ok-soft text-ok",
  warn: "bg-warn-soft text-warn",
  crit: "bg-crit-soft text-crit",
  muted: "border border-border text-muted-foreground",
};

function StatusChip({ phase, message }: { phase: PhasePresentation; message: string | null }) {
  return (
    <span title={message ?? phase.description} className={`inline-flex h-6 w-32 shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs ${STATUS_STYLE[phase.tone]}`}>
      <span className="h-1.5 w-1.5 shrink-0 rounded-full bg-current" aria-hidden />
      {phase.label}
    </span>
  );
}

function RetryButton({ onClick, label = "Check again" }: { onClick: () => void; label?: string }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="inline-flex min-h-5 items-center gap-1.5 rounded-md border border-border bg-card px-2 py-1 text-xs text-foreground transition-colors hover:bg-card-strong"
    >
      <RefreshCw className="h-3 w-3" aria-hidden />
      {label}
    </button>
  );
}

function toneIcon(phase: ProcessDetailPhase) {
  if (phase === "withheld" || phase === "unsupported") return ShieldAlert;
  if (phase === "disconnected" || phase === "error") return WifiOff;
  if (phase === "paused") return Pause;
  if (phase === "acquiring" || phase === "loading") return Hourglass;
  if (phase === "busy" || phase === "expired" || phase === "failed" || phase === "stale") {
    return AlertTriangle;
  }
  return List;
}

function NoSnapshot({
  phase,
  info,
  message,
  onRetry,
}: {
  phase: ProcessDetailPhase;
  info: PhasePresentation;
  message: string | null;
  onRetry: () => void;
}) {
  const Icon = toneIcon(phase);

  return (
    <div className="flex flex-col items-center gap-2 break-words">
      <EmptyState
        icon={Icon}
        note={message ?? info.description}
        className="min-h-64 w-full"
      />
      {phase !== "paused" && info.retryable ? (
        <RetryButton onClick={onRetry} label="Retry process detail" />
      ) : null}
    </div>
  );
}

function CountLine({
  summary,
  lastReceivedMs,
  intervalMs,
  message,
  phase,
  hasSnapshot,
}: {
  summary: ReturnType<typeof snapshotSummary>;
  lastReceivedMs: number | null;
  intervalMs: number;
  message: string | null;
  phase: ProcessDetailPhase;
  hasSnapshot: boolean;
}) {
  const now = useNow(1_000);
  const stale =
    phase === "ready" && isDetailSnapshotStale(lastReceivedMs, now, intervalMs);
  const effectivePhase: ProcessDetailPhase = stale ? "stale" : phase;
  const paused = effectivePhase === "paused";
  const info = phasePresentation(effectivePhase);
  const age =
    lastReceivedMs === null
      ? null
      : formatAge(Math.max(0, now - lastReceivedMs));
  const receivedLabel =
    age === null
      ? null
      : effectivePhase === "ready"
        ? `Updated ${age}`
        : `Last snapshot ${age}`;
  const announcement = paused
    ? hasSnapshot
      ? "Paused. This snapshot is not live."
      : "Paused. Use Resume to request a fresh snapshot."
    : `${info.label}${stale ? ". No new snapshot has arrived." : ""}${message ? `. ${message}` : ""}`;

  if (paused && !hasSnapshot) {
    return (
      <p role="status" className="sr-only">
        {announcement}
      </p>
    );
  }

  return (
    <>
      <p role="status" className="sr-only">
        {announcement}
      </p>
      <div className="flex min-h-6 flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
        {paused ? (
          <span>Paused — not live</span>
        ) : (
          <StatusChip phase={info} message={message} />
        )}
        {hasSnapshot && receivedLabel !== null ? (
          <span className="tnum">{receivedLabel}</span>
        ) : null}
        {hasSnapshot && summary.truncated ? (
          <span className="tnum" title="The collector kept the highest-CPU rows">
            top {formatInteger(summary.shown)} of {formatInteger(summary.total)}
          </span>
        ) : null}
        {hasSnapshot && summary.inaccessible > 0 ? (
          <span className="tnum text-warn">{formatInteger(summary.inaccessible)} unreadable</span>
        ) : null}
      </div>
    </>
  );
}

export function ProcessDetailPanel({
  machine,
  csrfToken,
  paused,
  onPausedChange,
}: {
  machine: MachineState;
  csrfToken: string | null;
  paused: boolean;
  onPausedChange: (paused: boolean) => void;
}) {
  const [query, setQuery] = useProcessQuery();
  const deferredQuery = useDeferredValue(query);
  const [mode, setMode] = useViewMode();
  const [sort, setSort] = useTableSort(mode === "applications" ? "applications" : "processes");
  const [expandedList, setExpandedList] = useExpandedGroups();
  const expanded = useMemo<ReadonlySet<string>>(() => new Set(expandedList), [expandedList]);
  const { state, refresh } = useProcessDetail({
    machineId: machine.machineId,
    csrfToken,
    paused,
  });

  const rows = useMemo<ProcessRow[]>(
    () => state.detail?.snapshot.processes ?? [],
    [state.detail],
  );
  const summary = useMemo(
    () => snapshotSummary(state.detail?.snapshot ?? null),
    [state.detail],
  );
  const filteredRows = useMemo(
    () => filterRowsForView(rows, deferredQuery),
    [rows, deferredQuery],
  );
  const sortedRows = useMemo(
    () => sortRows(filteredRows, sort.key, sort.dir),
    [filteredRows, sort],
  );
  const groups = useMemo(
    () => (mode === "applications" ? sortGroups(aggregateApplications(filteredRows), sort.key, sort.dir) : []),
    [filteredRows, mode, sort],
  );
  const tree = useMemo<TreeNode[]>(
    () => (mode === "tree" ? buildTree(sortedRows) : []),
    [mode, sortedRows],
  );

  const info = phasePresentation(state.phase);
  const hasDetail = state.detail !== null;
  const isWaiting = !hasDetail && (state.phase === "acquiring" || state.phase === "loading");
  const displayedQuery = deferredQuery.trim().slice(0, 80);
  const countLabel = useMemo(() => {
    const shown = filteredRows.length;
    const filtered = deferredQuery.trim().length > 0;
    const processWord = shown === 1 && !filtered ? "process" : "processes";
    if (mode === "applications") {
      return `${groups.length} app${groups.length === 1 ? "" : "s"} · ${shown}${
        filtered ? ` of ${summary.shown}` : ""
      } ${processWord}`;
    }
    return `${shown}${filtered ? ` of ${summary.shown}` : ""} ${processWord}`;
  }, [deferredQuery, filteredRows.length, groups.length, mode, summary.shown]);

  const onSort = (key: SortKey): void => setSort((current) => nextSortState(current, key));
  const changeMode = (next: ProcessViewMode): void => setMode(next);
  const onToggleGroup = (key: string): void => {
    setExpandedList((previous) =>
      previous.includes(key) ? previous.filter((item) => item !== key) : [...previous, key],
    );
  };

  return (
    <Panel
      title="Process detail"
      action={
        hasDetail ? (
          <span className="tnum hidden max-w-[18rem] truncate text-muted-foreground sm:block">
            {countLabel}
          </span>
        ) : undefined
      }
      className="min-h-0 flex-1"
    >
      <div className="flex min-h-0 flex-1 flex-col gap-4">
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <div className="relative min-w-0 flex-1 basis-52">
            <label htmlFor="process-search" className="sr-only">
              Search processes
            </label>
            <Search
              className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
              aria-hidden
            />
            <input
              id="process-search"
              type="search"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="Search name, user, PID, or unit"
              autoComplete="off"
              spellCheck={false}
              className="h-9 w-full rounded-md border border-border bg-card-strong py-1.5 pl-8 pr-8 text-sm text-foreground placeholder:text-muted-foreground [&::-webkit-search-cancel-button]:hidden [&::-webkit-search-cancel-button]:[-webkit-appearance:none]"
            />
            {/* Custom clear button; the native search-cancel button is hidden above to avoid a duplicate ✕ */}
            {query.length > 0 ? (
              <button
                type="button"
                onClick={() => setQuery("")}
                aria-label="Clear search"
                className="absolute right-0.5 top-1/2 flex h-7 w-7 -translate-y-1/2 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-card hover:text-foreground"
              >
                <X className="h-3.5 w-3.5" aria-hidden />
              </button>
            ) : null}
          </div>

          <div
            role="group"
            aria-label="Process view"
            className="inline-flex shrink-0 rounded-md border border-border p-0.5"
          >
            {VIEW_MODES.map((option) => {
              const active = mode === option.value;
              return (
                <button
                  key={option.value}
                  type="button"
                  aria-pressed={active}
                  onClick={() => changeMode(option.value)}
                  className={[
                    "rounded px-2.5 py-1 text-xs transition-colors",
                    active
                      ? "bg-selected font-medium text-foreground"
                      : "text-muted-foreground hover:bg-card-strong hover:text-foreground",
                  ].join(" ")}
                >
                  {option.label}
                </button>
              );
            })}
          </div>

          <button
            type="button"
            onClick={() => onPausedChange(!paused)}
            title={paused ? "Resume process updates" : "Pause process updates"}
            className="inline-flex min-h-9 shrink-0 items-center gap-1.5 rounded-md border border-border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong"
          >
            {paused ? <Play className="h-3.5 w-3.5" aria-hidden /> : <Pause className="h-3.5 w-3.5" aria-hidden />}
            {paused ? "Resume" : "Pause"}
          </button>

          <ProcessExportControls
            machine={machine}
            snapshot={state.detail?.snapshot ?? null}
            capturedAt={state.detail?.capturedAt ?? null}
            receivedAtMs={state.lastReceivedMs}
            intervalMs={state.intervalMs}
            phase={state.phase}
            query={deferredQuery}
            mode={mode}
            sort={sort}
            filteredRows={filteredRows}
            sortedRows={sortedRows}
            groups={groups}
            tree={tree}
          />
        </div>

        <CountLine
          summary={summary}
          lastReceivedMs={state.lastReceivedMs}
          intervalMs={state.intervalMs}
          message={state.message}
          phase={state.phase}
          hasSnapshot={hasDetail}
        />

        <div
          aria-busy={isWaiting || state.phase === "acquiring"}
          className="flex min-h-0 flex-1 flex-col overflow-auto"
        >
          {!hasDetail ? (
            <NoSnapshot
              phase={state.phase}
              info={info}
              message={state.message}
              onRetry={refresh}
            />
          ) : filteredRows.length === 0 ? (
            <div className="flex flex-col items-center gap-2 break-words">
              <EmptyState
                icon={Search}
                note={
                  summary.shown === 0
                    ? "The collector reported no processes in this snapshot."
                    : deferredQuery.trim().length > 0
                      ? `No processes match “${displayedQuery}”.`
                      : "No processes to show."
                }

                className="min-h-64 w-full"
              />
              {deferredQuery.trim().length > 0 ? (
                <button
                  type="button"
                  onClick={() => setQuery("")}
                  className="inline-flex min-h-6 items-center gap-1.5 rounded-md border border-border px-2 py-1 text-xs text-foreground transition-colors hover:bg-card-strong"
                >
                  <X className="h-3 w-3" aria-hidden />
                  Clear search
                </button>
              ) : null}
            </div>
          ) : (
            <ProcessViews
              mode={mode}
              groups={groups}
              rows={sortedRows}
              tree={tree}
              sort={sort}
              onSort={onSort}
              expanded={expanded}
              onToggleGroup={onToggleGroup}
            />
          )}
        </div>
      </div>
    </Panel>
  );
}
