"use client";

/**
 * Durable History view: authenticated machine/range/metric selection over
 * `/api/history`, with honest loading, empty, denied, disconnected, partial
 * and error states. Charts are drawn from stored buckets only; missing data
 * is never filled with zero.
 */

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { AlertTriangle, History as HistoryIcon, RefreshCw, ShieldAlert, WifiOff } from "lucide-react";

import { AppShell } from "@/components/shell/app-shell";
import { EmptyState, Panel } from "@/components/panel";
import { LiveStatusChip } from "@/components/live/live-status";
import { UserMenu } from "@/components/live/user-menu";
import { useAccessGate } from "@/components/live/stream-banners";
import { useLive } from "@/live/live-provider";
import { useMachineSelection } from "@/live/machine-selection";
import type { MachineState } from "@/contracts";
import { defaultMetricsFor, gpuCards, visibleCapacityMetrics } from "@/history/metrics";
import { availableCoreIds, resolveCoreSelection } from "@/history/cores";
import {
  formatClock,
  formatDateTime,
  formatGapDuration,
  gapSummary,
  rangePreset,
} from "@/history/presentation";
import { HistoryControls, type MetricCatalog } from "./history-controls";
import { useStoredHistory, type HistoryError } from "./use-stored-history";
import { HistoryCharts } from "./history-panels";
import { useHistoryRange, useHistoryMetrics, useHistoryHidden, useHistoryAutoRefresh, useHistoryCores, useHistoryCapacityUnit } from "./history-prefs";
import { HistoryExportControls } from "@/components/exports/history-export-controls";

function errorCopy(error: HistoryError): { title: string; note: string; icon: typeof ShieldAlert } {
  switch (error.kind) {
    case "unauthorized":
      return {
        title: "Session ended",
        note: "Your session is no longer accepted, so stored history is hidden.",
        icon: ShieldAlert,
      };
    case "denied":
      return {
        title: "History access denied",
        note: error.message,
        icon: ShieldAlert,
      };
    case "missing":
      return {
        title: "Machine unavailable",
        note: error.message,
        icon: HistoryIcon,
      };
    case "rate-limited":
      return {
        title: "History requests limited",
        note: error.message,
        icon: AlertTriangle,
      };
    case "unavailable":
      return {
        title: "History unavailable",
        note: error.message,
        icon: WifiOff,
      };
    case "network":
      return {
        title: "Hub unreachable",
        note: "The hub could not be reached for stored history.",
        icon: WifiOff,
      };
    case "invalid":
      return {
        title: "Unexpected history response",
        note: error.message,
        icon: AlertTriangle,
      };
  }
}

function machinesLabel(machine: MachineState): string {
  return machine.displayName?.trim() || machine.hostname.trim() || machine.machineId;
}

type LiveHistoryGap = {
  machineId: string;
  range: string;
  fromMs: number;
  toMs: number;
};

function LoadingPanels() {
  return (
    <div className="grid grid-cols-1 gap-5 xl:grid-cols-2" aria-busy="true">
      <Panel title="Loading stored history">
        <div className="h-40 animate-pulse rounded-lg bg-card-strong" aria-hidden />
      </Panel>
      <Panel title="Preparing charts">
        <div className="h-40 animate-pulse rounded-lg bg-card-strong" aria-hidden />
      </Panel>
    </div>
  );
}

export function HistoryView() {
  const { session, phase, machines, retryConnection } = useLive();
  const gate = useAccessGate();

  // Route/remembered/Fleet resolution and URL persistence live in the
  // Machine selection module; an explicit unknown machine stays missing.
  const { machine, machineId, requestedMachineId } = useMachineSelection("history");

  const [memoryUnit, setMemoryUnit] = useHistoryCapacityUnit(machineId, "memory");
  const [filesystemUnit, setFilesystemUnit] = useHistoryCapacityUnit(machineId, "filesystem");
  const capacityUnits = { memory: memoryUnit, filesystem: filesystemUnit };
  const [range, setRange] = useHistoryRange();
  const [selection, setSelection] = useHistoryMetrics(machineId);
  const [corePreference] = useHistoryCores(machineId);
  const knownCoreIds = availableCoreIds(machine);
  const selectedCoreIds = resolveCoreSelection(corePreference, knownCoreIds);
  const [hiddenCardList, setHiddenCards] = useHistoryHidden(machineId, "cards");
  const [hiddenSeriesList, setHiddenSeries] = useHistoryHidden(machineId, "series");
  const hiddenCards = useMemo(() => new Set(hiddenCardList), [hiddenCardList]);
  const hiddenSeries = useMemo(() => new Set(hiddenSeriesList), [hiddenSeriesList]);
  const [autoRefresh, setAutoRefresh] = useHistoryAutoRefresh();
  const [catalog, setCatalog] = useState<MetricCatalog | null>(null);
  const [catalogError, setCatalogError] = useState<string | null>(null);
  const [refreshNonce, setRefreshNonce] = useState(0);

  const defaultMetricsKey = useMemo(
    () => (machine === undefined ? "" : defaultMetricsFor(machine).join(",")),
    // Capability/inventory changes can change the honest defaults; the selected
    // preferences are stored per machine so explicit choices are preserved.
    [machine?.machineId, machine?.inventory, machine?.metrics?.payload.gpus],
  );
  const metricsKey = selection ?? defaultMetricsKey;
  const selectedMetrics = metricsKey === "" ? [] : metricsKey.split(",");
  const canQuery =
    session.kind === "signed-in" &&
    machine !== undefined &&
    machineId !== "" &&
    metricsKey !== "";

  // Metric catalog confirms what this hub records; failure is non-fatal.
  useEffect(() => {
    if (machineId === "") {
      setCatalog(null);
      setCatalogError(null);
      return;
    }
    const controller = new AbortController();
    setCatalog(null);
    setCatalogError(null);
    fetch(`/api/history/metrics?machineId=${encodeURIComponent(machineId)}`, {
      signal: controller.signal,
      cache: "no-store",
      headers: { Accept: "application/json" },
    })
      .then(async (res) => {
        if (!res.ok) {
          if (res.status === 401 || res.status === 403 || res.status === 404) return;
          throw new Error(`catalog ${res.status}`);
        }
        const body: unknown = await res.json();
        if (body === null || typeof body !== "object" || !("metrics" in body)) throw new Error("catalog shape");
        const metricsValue = (body as { metrics: unknown }).metrics;
        if (metricsValue === null || typeof metricsValue !== "object") throw new Error("catalog shape");
        const next = Object.create(null) as MetricCatalog;
        for (const [metric, definition] of Object.entries(metricsValue as Record<string, unknown>)) {
          if (definition !== null && typeof definition === "object" && typeof (definition as { unit?: unknown }).unit === "string") {
            next[metric] = { unit: (definition as { unit: string }).unit };
          }
        }
        if (!controller.signal.aborted) setCatalog(next);
      })
      .catch(() => {
        if (!controller.signal.aborted) {
          setCatalogError("The hub's metric catalog could not be loaded; standard metric names are shown.");
        }
      });
    return () => controller.abort();
  }, [machineId]);

  const { data: shown, error, loading, manualRefreshing } = useStoredHistory({
    machineId, metricsKey, coreIdsKey: selectedMetrics.includes("cpu.core") && selectedCoreIds.length !== knownCoreIds.length ? selectedCoreIds.join(",") : undefined,
    range, enabled: canQuery, autoRefresh, refreshNonce,
  });
  useEffect(() => {
    if (error?.kind === "rate-limited") setAutoRefresh(false);
  }, [error?.kind, setAutoRefresh]);

  const result = shown?.result ?? null;
  const [liveHistoryGap, setLiveHistoryGap] = useState<LiveHistoryGap | null>(null);
  useEffect(() => {
    if (machine === undefined || result === null) return;
    const lastReceivedAt = machine.lastReceivedAt === null ? NaN : Date.parse(machine.lastReceivedAt);
    const scopeMatches = liveHistoryGap?.machineId === machine.machineId && liveHistoryGap.range === range;

    if (scopeMatches && liveHistoryGap !== null) {
      const recorded = result.gaps.some(
        (gap) => gap.kind === "missing" && gap.fromMs <= liveHistoryGap.toMs && gap.toMs >= liveHistoryGap.fromMs,
      );
      if (recorded) {
        setLiveHistoryGap(null);
        return;
      }
      if (machine.connected && Number.isFinite(lastReceivedAt) && lastReceivedAt > liveHistoryGap.fromMs) {
        if (lastReceivedAt !== liveHistoryGap.toMs) {
          setLiveHistoryGap({ ...liveHistoryGap, toMs: lastReceivedAt });
        }
        return;
      }
      if (!machine.connected && result.toMs > liveHistoryGap.toMs) {
        setLiveHistoryGap({ ...liveHistoryGap, toMs: result.toMs });
      }
      return;
    }

    if (machine.connected || !Number.isFinite(lastReceivedAt)) return;
    if (lastReceivedAt < result.toMs && result.toMs > result.fromMs) {
      setLiveHistoryGap({
        machineId: machine.machineId,
        range,
        fromMs: Math.max(result.fromMs, lastReceivedAt),
        toMs: result.toMs,
      });
    }
  }, [machine, result, range, liveHistoryGap]);
  const resultWithLiveGap = useMemo(() => {
    if (result === null || liveHistoryGap === null || liveHistoryGap.machineId !== machine?.machineId || liveHistoryGap.range !== range) return result;
    const toMs = Math.min(liveHistoryGap.toMs, result.toMs);
    if (liveHistoryGap.fromMs >= toMs) return result;
    const alreadyRecorded = result.gaps.some(
      (gap) => gap.kind === "missing" && gap.fromMs <= toMs && gap.toMs >= liveHistoryGap.fromMs,
    );
    if (alreadyRecorded) return result;
    return {
      ...result,
      gaps: [
        ...result.gaps,
        {
          fromMs: Math.max(result.fromMs, liveHistoryGap.fromMs),
          toMs,
          kind: "missing" as const,
          reason: "collector is not reporting",
        },
      ],
    };
  }, [result, liveHistoryGap, machine?.machineId, range]);
  const cards = machine === undefined ? [] : gpuCards(machine, result?.series ?? []);
  const rangeInfo = rangePreset(range);

  const toggleMetric = (metric: string): void => {
    const next = new Set(selectedMetrics);
    if (next.has(metric)) next.delete(metric);
    else next.add(metric);
    setSelection([...next].sort().join(","));
  };

  const toggleCard = (cardId: string): void => {
    setHiddenCards((previous) => {
      const next = new Set(previous);
      if (next.has(cardId)) next.delete(cardId);
      else next.add(cardId);
      return [...next];
    });
  };

  const toggleSeries = (key: string): void => {
    setHiddenSeries((previous) => {
      const next = new Set(previous);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return [...next];
    });
  };

  const statusText = error !== null
    ? shown !== null ? `${error.kind === "rate-limited" ? "Refresh paused" : "Refresh failed"} · ${formatClock(shown.loadedAt)}` : "History unavailable"
    : shown !== null ? `Updated ${formatClock(shown.loadedAt)}` : loading ? "Loading…" : "";

  const awaitingFirstResult = canQuery && shown === null && error === null;

  const degraded = phase === "reconnecting" || phase === "disconnected";
  const machineCount = machines.length;
  const hubDisconnected = phase === "disconnected";
  const waitingForMachines =
    machineCount === 0 && !hubDisconnected && (phase === "idle" || phase === "connecting" || phase === "reconnecting");

  const header = (
    <AppShell
      title="History"
      machineId={machineId}
      machineView="history"
      headerRight={
        <>
          <LiveStatusChip />
          <UserMenu />
        </>
      }
    >
      {gate ? (
        gate
      ) : (
        <>
          <p role="status" aria-live="polite" className="sr-only">
            {error?.message ?? ""}
          </p>

          <HistoryControls
            machine={machine}
            range={range}
            selectedMetrics={selectedMetrics}
            catalog={catalog}
            catalogError={catalogError}
            gpuCards={cards}
            hiddenCards={hiddenCards}
            autoRefresh={autoRefresh}
            loading={loading}
            manualRefreshing={manualRefreshing}
            statusText={statusText}
            onRangeChange={setRange}
            onToggleMetric={toggleMetric}
            onSetMetrics={(metrics) => setSelection([...new Set(metrics)].sort().join(","))}
            onToggleCard={toggleCard}
            onToggleAutoRefresh={() => setAutoRefresh((value) => !value)}
            onRefresh={() => setRefreshNonce((value) => value + 1)}
          />

          {degraded ? (
            <div
              role="status"
              className="mb-4 flex items-start gap-2 rounded-lg border border-border bg-card px-4 py-2.5 text-xs text-muted-foreground"
            >
              <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0 text-warn" aria-hidden />
              <span>
                Live feed {phase === "disconnected" ? "disconnected" : "reconnecting"} · stored history available
              </span>
              {phase === "disconnected" ? (
                <button
                  type="button"
                  onClick={retryConnection}
                  className="ml-auto inline-flex min-h-8 shrink-0 items-center gap-1.5 rounded-md border border-border px-2.5 text-xs text-foreground transition-colors hover:bg-card-strong"
                >
                  <RefreshCw className="h-3 w-3" aria-hidden />
                  Retry now
                </button>
              ) : null}
            </div>
          ) : null}

          {machine === undefined ? (
            hubDisconnected ? (
              <Panel title="Hub disconnected" className="mx-auto mt-3 max-w-xl">
                <EmptyState
                  icon={WifiOff}
                  note="The hub is unreachable, so the authorized machine list is unavailable."
                />
                <div className="mt-2 flex justify-center">
                  <button
                    type="button"
                    onClick={retryConnection}
                    className="inline-flex min-h-8 items-center gap-1.5 rounded-md border border-border px-3 text-xs text-foreground transition-colors hover:bg-card-strong"
                  >
                    <RefreshCw className="h-3.5 w-3.5" aria-hidden />
                    Check again
                  </button>
                </div>
              </Panel>
            ) : waitingForMachines ? (
              <Panel title="Waiting for machines" className="mx-auto mt-3 max-w-xl">
                <EmptyState
                  icon={HistoryIcon}
                  note="Waiting for the hub to report your authorized machines…"
                />
              </Panel>
            ) : machineCount === 0 ? (
              <Panel title="No machines" className="mx-auto mt-3 max-w-xl">
                <EmptyState
                  icon={HistoryIcon}
                  note="No machines are available in your authorized fleet."
                />
              </Panel>
            ) : (
              <Panel title="Machine not found" className="mx-auto mt-3 max-w-xl">
                <EmptyState
                  icon={HistoryIcon}
                  note="This machine is unavailable or outside your access."
                />
                {requestedMachineId ? (
                  <p className="tnum mt-2 text-center text-xs text-muted-foreground">
                    Requested identifier: {requestedMachineId}
                  </p>
                ) : null}
                <div className="mt-2 flex justify-center">
                  <Link
                    href="/"
                    className="inline-flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong"
                  >
                    Back to fleet
                  </Link>
                </div>
              </Panel>
            )
          ) : selectedMetrics.length === 0 ? (
            <Panel title="No metrics selected" className="mx-auto mt-3 max-w-xl">
              <EmptyState
                icon={HistoryIcon}
                note={
                  defaultMetricsKey === ""
                    ? "No metric families are shared with this hub."
                    : "Choose at least one metric above to draw history."
                }
              />
            </Panel>
          ) : error !== null && shown === null ? (
            (() => {
              const copy = errorCopy(error);
              const Icon = copy.icon;
              return (
                <Panel title={copy.title} className="mx-auto mt-3 max-w-xl">
                  <EmptyState icon={Icon} note={copy.note}  />
                  <div className="mt-2 flex flex-wrap justify-center gap-2">
                    <button
                      type="button"
                      onClick={() => setRefreshNonce((value) => value + 1)}
                      className="inline-flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong"
                    >
                      <RefreshCw className="h-3.5 w-3.5" aria-hidden />
                      Try again
                    </button>
                    {error.kind === "unauthorized" ? (
                      <Link
                        href="/login"
                        className="inline-flex items-center rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-accent-foreground transition-colors hover:bg-accent/90"
                      >
                        Sign in
                      </Link>
                    ) : null}
                  </div>
                </Panel>
              );
            })()
          ) : (loading || awaitingFirstResult) && shown === null ? (
            <LoadingPanels />
          ) : shown === null ? (
            <Panel title="History" className="mx-auto mt-3 max-w-xl">
              <EmptyState
                icon={HistoryIcon}
                note="No stored history has arrived yet."
              />
            </Panel>
          ) : (() => {
            const result = shown.result;
            const tierLabel =
              result.tier === "raw"
                ? "raw samples"
                : result.tier === "1m"
                  ? "1-minute rollups"
                  : "1-hour rollups";
            return (
            <>
              <div className="mb-3 flex flex-wrap items-baseline gap-x-4 gap-y-1 text-xs text-muted-foreground">
                <span className="inline-flex items-center gap-1.5 text-foreground">
                  <span
                    aria-hidden
                    className={`inline-block h-2 w-2 shrink-0 rounded-full ${machine.connected ? "bg-ok" : "bg-crit"}`}
                  />
                  {machinesLabel(machine)}
                </span>
                <span>
                  {formatDateTime(result.fromMs)} – {formatDateTime(result.toMs)}
                </span>
                <span className="tnum" title={tierLabel}>
                  {formatGapDuration(result.stepMs)} buckets
                </span>
                <div className="ml-auto self-center">
                  <HistoryExportControls
                    machine={machine}
                    result={result}
                    range={range}
                    selectedMetrics={visibleCapacityMetrics(selectedMetrics, capacityUnits)}
                    hiddenCards={hiddenCards}
                    hiddenSeries={hiddenSeries}
                    coreIds={selectedCoreIds}
                    loadedAt={shown.loadedAt}
                  />
                </div>
              </div>

              {result.truncated ? (
                <div
                  role="status"
                  className="mb-4 flex items-start gap-2 rounded-lg bg-warn-soft px-4 py-2.5 text-xs text-foreground"
                >
                  <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0 text-warn" aria-hidden />
                  <span>
                    Partial history · narrow the range for more detail.
                  </span>
                </div>
              ) : null}

              {resultWithLiveGap!.gaps.length > 0 ? (
                <details className="mb-4 max-w-3xl text-xs text-muted-foreground">
                  <summary className="inline-flex min-h-8 cursor-pointer items-center gap-2 rounded-md text-warn">
                    <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0 text-warn" aria-hidden />
                    <span>
                      <strong className="font-medium">Data gaps:</strong>{" "}
                    {gapSummary(resultWithLiveGap!.gaps, result.fromMs, result.toMs)}
                    </span>
                  </summary>
                    <ul className="mt-1.5 flex flex-col gap-1">
                      {resultWithLiveGap!.gaps.slice(0, 20).map((gap) => (
                        <li key={`${gap.fromMs}-${gap.toMs}-${gap.kind}`} className="tnum text-xs">
                          {formatDateTime(gap.fromMs)} – {formatDateTime(gap.toMs)} (
                          {formatGapDuration(gap.toMs - gap.fromMs)}):{" "}
                          {gap.kind === "dropped" ? "dropped" : "missed"}
                          {gap.reason ? ` · ${gap.reason}` : ""}
                          {gap.droppedRecords !== undefined
                            ? ` · ${gap.droppedRecords} record${gap.droppedRecords === 1 ? "" : "s"}`
                            : ""}
                        </li>
                      ))}
                      {resultWithLiveGap!.gaps.length > 20 ? (
                        <li className="text-xs text-muted-foreground">
                          …and {resultWithLiveGap!.gaps.length - 20} more.
                        </li>
                      ) : null}
                    </ul>
                </details>
              ) : null}

              {result.series.length === 0 ? (
                <Panel title="No stored points" className="mx-auto max-w-xl">
                  <EmptyState
                    icon={HistoryIcon}
                    note={`No stored readings for ${machine === undefined ? "this machine" : machinesLabel(machine)} in ${rangeInfo.label.toLowerCase()}.`}
                  />
                </Panel>
              ) : (
                <HistoryCharts
                  machine={machine}
                  result={resultWithLiveGap!}
                  selectedMetrics={selectedMetrics}
                  hiddenCards={hiddenCards}
                  hiddenSeries={hiddenSeries}
                  onToggleSeries={toggleSeries}
                  capacityUnits={capacityUnits}
                  onCapacityUnitChange={(group, unit) => (group === "memory" ? setMemoryUnit : setFilesystemUnit)(unit)}
                />
              )}
            </>
            );
          })()}
        </>
      )}
    </AppShell>
  );

  return header;
}
