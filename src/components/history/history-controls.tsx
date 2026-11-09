"use client";

/**
 * History query controls: machine, time range, metric families and GPU cards.
 * Native selects, radios and checkboxes keep keyboard behavior and focus
 * rings; every control has a visible label.
 */

import { Check, ChevronDown, Pause, Play, RefreshCw } from "lucide-react";
import { useHistoryControlsOpen, useHistoryCores } from "./history-prefs";

import type { MachineState } from "@/contracts";
import {
  HISTORY_METRICS,
  HISTORY_METRIC_GROUPS,
  metricAvailability,
  metricOption,
  metricSeriesStyle,
  type GpuCard,
} from "@/history/metrics";
import {
  availableCoreIds,
  coreLabel,
  resolveCoreSelection,
} from "@/history/cores";
import { RANGE_PRESETS, type RangeId } from "@/history/presentation";

export type MetricCatalog = Record<string, { unit: string }>;

type Props = {
  machine: MachineState | undefined;
  range: RangeId;
  selectedMetrics: readonly string[];
  catalog: MetricCatalog | null;
  catalogError: string | null;
  gpuCards: readonly GpuCard[];
  hiddenCards: ReadonlySet<string>;
  autoRefresh: boolean;
  loading: boolean;
  manualRefreshing: boolean;
  statusText: string;
  onRangeChange: (range: RangeId) => void;
  onToggleMetric: (metric: string) => void;
  onSetMetrics: (metrics: readonly string[]) => void;
  onToggleCard: (key: string) => void;
  onToggleAutoRefresh: () => void;
  onRefresh: () => void;
};

const focusChip =
  "peer-focus-visible:outline peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:[outline-color:var(--focus)]";

/** Group heading shared by the metric columns: label on a hairline rule. */
function GroupHeader({ label, action }: { label: string; action?: React.ReactNode }) {
  return (
    <div className="flex min-w-0 items-center gap-2.5">
      <span className="text-xs text-muted-foreground">{label}</span>
      <span aria-hidden className="h-px min-w-0 flex-1 bg-border" />
      {action}
    </div>
  );
}

/** Calm chip vocabulary: selected fills, unselected tints; borders only on focus. */
const chipBase =
  "inline-flex min-h-8 items-center gap-1.5 rounded-md border border-transparent px-2.5 text-xs transition-colors";
const chipSelected = `${chipBase} ${focusChip} bg-selected text-foreground`;
const chipIdle = `${chipBase} ${focusChip} bg-card-strong text-muted-foreground hover:text-foreground`;

function metricControlLabel(metric: string, fallback: string): string {
  const labels: Record<string, string> = {
    "cpu.usage": "Usage", "cpu.core": "Per-core", "cpu.iowait": "I/O wait",
    "cpu.pressure": "Pressure", "cpu.temperature": "Temperature", "cpu.power": "Package power",
    "memory.used": "Used bytes", "memory.usage": "Usage %", "swap.used": "Swap",
    "memory.pressure.some": "Pressure (some)", "memory.pressure.full": "Pressure (full)",
    "gpu.utilization": "Utilization", "gpu.memory.used": "VRAM", "gpu.temperature": "Temperature",
    "gpu.junction.temperature": "Junction", "gpu.vram.temperature": "VRAM temp", "gpu.power": "Power",
    "fs.used": "Space used", "fs.usage": "Capacity %", "fs.inodes.usage": "Inodes %",
    "net.rx": "Received", "net.tx": "Sent",
  };
  return labels[metric] ?? fallback;
}

export function HistoryControls(props: Props) {
  const {
    machine,
    range,
    selectedMetrics,
    catalog,
    catalogError,
    gpuCards,
    hiddenCards,
    autoRefresh,
    loading,
    manualRefreshing,
    statusText,
    onRangeChange,
    onToggleMetric,
    onSetMetrics,
    onToggleCard,
    onToggleAutoRefresh,
    onRefresh,
  } = props;

  const [controlsOpen, setControlsOpen] = useHistoryControlsOpen();
  const selected = new Set(selectedMetrics);
  const availableMetrics = HISTORY_METRICS.filter(({ metric }) =>
    metricAvailability(machine, metric).available && (catalog === null || catalog[metric] !== undefined),
  ).map(({ metric }) => metric);
  const availableSet = new Set(availableMetrics);
  const setGroupShown = (metrics: readonly string[], shown: boolean) => {
    const next = new Set(selectedMetrics);
    for (const metric of metrics) {
      if (shown && availableSet.has(metric)) next.add(metric);
      else if (!shown) next.delete(metric);
    }
    onSetMetrics([...next]);
  };
  const gpuMetricsSelected = selectedMetrics.some((metric) => metric.startsWith("gpu."));
  const cpuCoreSelected = selected.has("cpu.core");
  const [corePreference, setCorePreference] = useHistoryCores(machine?.machineId ?? "");
  const availableCores = cpuCoreSelected ? availableCoreIds(machine) : [];
  const selectedCores = resolveCoreSelection(corePreference, availableCores);
  const selectedCoreSet = new Set(selectedCores);

  const setCoreShown = (id: string, shown: boolean): void => {
    setCorePreference((previous) => {
      const current = resolveCoreSelection(previous, availableCores);
      if (!shown) return current.filter((item) => item !== id);
      if (current.includes(id)) return previous;
      return [...current, id];
    });
  };

  return (
    <section
      aria-labelledby="history-query-heading"
      aria-busy={loading}
      className="mb-5 min-w-0 rounded-xl border border-border bg-card"
    >
      <h2 id="history-query-heading" className="sr-only">
        History query
      </h2>

      <div className="flex flex-wrap items-end gap-x-5 gap-y-4 p-4 md:p-5">
        <fieldset className="flex min-w-0 flex-col gap-1.5">
          <legend className="mb-1.5 text-xs text-muted-foreground">Time range</legend>
          <div className="inline-flex flex-wrap rounded-lg border border-border bg-background p-0.5">
            {RANGE_PRESETS.map((preset) => {
              const checked = range === preset.id;
              return (
                <label key={preset.id} className="cursor-pointer">
                  <input
                    type="radio"
                    name="history-range"
                    value={preset.id}
                    checked={checked}
                    onChange={() => onRangeChange(preset.id)}
                    className="peer sr-only"
                  />
                  <span
                    className={`inline-flex min-h-8 items-center rounded-md px-2.5 text-xs transition-colors ${focusChip} ${
                      checked
                        ? "bg-selected font-medium text-foreground"
                        : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {preset.shortLabel}
                    <span className="sr-only"> — {preset.label}</span>
                  </span>
                </label>
              );
            })}
          </div>
        </fieldset>

        <div className="flex min-w-0 flex-wrap items-center gap-2 sm:ml-auto">
          <p className="order-first w-full text-xs text-muted-foreground sm:order-none sm:w-auto">
            {statusText}
          </p>
          <button
            type="button"
            onClick={onRefresh}
            disabled={manualRefreshing}
            className="inline-flex min-h-8 items-center gap-1.5 rounded-md border border-border bg-background px-2.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-60"
          >
            <RefreshCw className={`h-3.5 w-3.5 ${manualRefreshing ? "animate-spin" : ""}`} aria-hidden />
            Refresh
          </button>
          <button
            type="button"
            onClick={onToggleAutoRefresh}
            aria-pressed={autoRefresh}
            className={`inline-flex min-h-8 items-center gap-1.5 rounded-md border px-2.5 text-xs transition-colors ${
              autoRefresh
                ? "border-accent/40 bg-selected text-foreground"
                : "border-border bg-background text-muted-foreground hover:bg-card-strong hover:text-foreground"
            }`}
          >
            {autoRefresh ? <Pause className="h-3.5 w-3.5" aria-hidden /> : <Play className="h-3.5 w-3.5" aria-hidden />}
            {autoRefresh ? "Pause" : "Resume"}
          </button>
        </div>
      </div>

      <div className="border-t border-border px-4 py-4 md:px-5">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <button type="button" aria-expanded={controlsOpen} aria-controls="history-metric-options"
            onClick={() => setControlsOpen(!controlsOpen)}
            className="inline-flex min-h-8 items-center gap-1.5 rounded-md text-sm font-medium">
            Metrics <span className="tnum text-xs font-normal text-muted-foreground">{selected.size} selected</span>
            <ChevronDown aria-hidden className={`h-3.5 w-3.5 text-muted-foreground transition-transform ${controlsOpen ? "rotate-180" : ""}`} />
          </button>
            <div className="flex flex-wrap gap-2">
              <button type="button" onClick={() => setGroupShown(availableMetrics, true)}
                disabled={availableMetrics.every((metric) => selected.has(metric))}
                className="inline-flex min-h-8 items-center rounded-md border border-border bg-background px-2.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50">
                Select all
              </button>
              <button type="button" onClick={() => onSetMetrics([])} disabled={selected.size === 0}
                className="inline-flex min-h-8 items-center rounded-md border border-border bg-background px-2.5 text-xs text-foreground transition-colors hover:bg-card-strong disabled:opacity-50">
                Unselect all
              </button>
            </div>
        </div>
        <div id="history-metric-options" hidden={!controlsOpen}>
          <div className="mt-4 min-w-0 md:col-span-2">
        <div className="min-w-0">
          <div className="grid gap-x-6 gap-y-5 md:grid-cols-2 xl:grid-cols-3">
            {HISTORY_METRIC_GROUPS.map((group) => (
              <fieldset key={group.id} className={`min-w-0 ${group.id === "disk" ? "xl:col-span-2" : ""}`}>
                <legend className="sr-only">{group.label}</legend>
                <GroupHeader label={group.label} action={
                  <div className="flex shrink-0 gap-2">
                    <button type="button" aria-label={`Select all ${group.label} metrics`}
                      onClick={() => setGroupShown(group.metrics, true)}
                      disabled={group.metrics.filter((metric) => availableSet.has(metric)).every((metric) => selected.has(metric))}
                      className="min-h-6 rounded-sm text-xs text-muted-foreground hover:text-foreground hover:underline disabled:opacity-50">
                      All
                    </button>
                    <button type="button" aria-label={`Unselect all ${group.label} metrics`}
                      onClick={() => setGroupShown(group.metrics, false)} disabled={!group.metrics.some((metric) => selected.has(metric))}
                      className="min-h-6 rounded-sm text-xs text-muted-foreground hover:text-foreground hover:underline disabled:opacity-50">
                      None
                    </button>
                  </div>
                } />
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {group.metrics.map((metric) => {
                    const option = metricOption(metric);
                    if (option === null) return null;
                    const availability = metricAvailability(machine, metric);
                    const recorded = catalog === null || catalog[metric] !== undefined;
                    const available = availability.available && recorded;
                    const isSelected = selected.has(metric);
                    const style = metricSeriesStyle(option, 0);
                    return (
                      <label
                        key={metric}
                        className={available ? "cursor-pointer" : "cursor-not-allowed"}
                        title={
                          availability.available
                            ? recorded
                              ? option.description
                              : "This hub does not record this metric."
                            : availability.reason ?? undefined
                        }
                      >
                        <input
                          type="checkbox"
                          checked={isSelected}
                          onChange={() => onToggleMetric(metric)}
                          disabled={!available && !isSelected}
                          className="peer sr-only"
                          aria-label={option.label}
                        />
                        <span
                          className={`${
                            !available
                              ? isSelected
                                ? `${chipBase} ${focusChip} bg-card-strong text-muted-foreground line-through`
                                : `${chipBase} text-muted-foreground opacity-50`
                              : isSelected
                                ? chipSelected
                                : chipIdle
                          }`}
                        >
                          <span
                            aria-hidden
                            className="flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded-sm border border-current"
                            style={{ color: style.color }}
                          >
                            {isSelected ? <Check className="h-3 w-3" strokeWidth={3} /> : null}
                          </span>
                          {metricControlLabel(metric, option.label)}
                        </span>
                      </label>
                    );
                  })}
                </div>
                {group.id === "gpu" && gpuMetricsSelected && gpuCards.length > 0 ? (
                  <fieldset className="mt-4 border-t border-border pt-3">
                    <legend className="sr-only">GPU cards</legend>
                    <GroupHeader label="GPU cards" />
                    <div className="mt-1.5 flex flex-wrap gap-1.5">
                      {gpuCards.map((card, index) => {
                        const key = card.id;
                        const shown = !hiddenCards.has(key);
                        return (
                          <label key={card.id} className="cursor-pointer">
                            <input
                              type="checkbox"
                              checked={shown}
                              onChange={() => onToggleCard(key)}
                              className="peer sr-only"
                            />
                            <span
                              className={shown ? chipSelected : chipIdle}
                            >
                              <span className="tnum text-xs text-muted-foreground">{index + 1}</span>
                              {card.label}
                            </span>
                          </label>
                        );
                      })}
                    </div>
                  </fieldset>
                ) : null}
                {group.id === "cpu" && cpuCoreSelected ? (
                  <fieldset className="mt-4 border-t border-border pt-3">
                    <legend className="sr-only">CPU cores</legend>
                    <GroupHeader label={`CPU cores · ${selectedCores.length} / ${availableCores.length}`} action={
                      <div className="flex shrink-0 gap-2">
                        <button type="button" onClick={() => setCorePreference(null)}
                          disabled={selectedCores.length === availableCores.length}
                          className="min-h-6 rounded-sm text-xs text-muted-foreground hover:text-foreground disabled:opacity-50">All cores</button>
                        <button type="button" onClick={() => setCorePreference([])} disabled={selectedCores.length === 0}
                          aria-label="Unselect all CPU cores"
                          className="min-h-6 rounded-sm text-xs text-muted-foreground hover:text-foreground disabled:opacity-50">None</button>
                      </div>
                    } />
                    {availableCores.length === 0 ? <p className="mt-2 text-xs text-muted-foreground">Core count unavailable.</p> : (
                      <details className="mt-2">
                        <summary className="inline-flex min-h-8 cursor-pointer items-center gap-1 rounded-md text-xs text-muted-foreground hover:text-foreground">
                          Choose cores <ChevronDown className="h-3.5 w-3.5" aria-hidden />
                        </summary>
                        <div className="mt-2 grid max-h-48 grid-cols-3 gap-1.5 overflow-y-auto p-1">
                          {availableCores.map((id) => {
                            const shown = selectedCoreSet.has(id);
                            return (
                              <label key={id} className="min-w-0 cursor-pointer">
                                <input type="checkbox" checked={shown} onChange={() => setCoreShown(id, !shown)} className="peer sr-only" />
                                <span className={`${shown ? chipSelected : chipIdle} w-full px-1.5`}>
                                  <span aria-hidden className="flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded-sm border border-current">
                                    {shown ? <Check className="h-3 w-3" /> : null}
                                  </span>
                                  {coreLabel(id)}
                                </span>
                              </label>
                            );
                          })}
                        </div>
                      </details>
                    )}
                  </fieldset>
                ) : null}
              </fieldset>
            ))}
          </div>
        </div>

        {catalogError ? (
          <p className="mt-2 text-xs text-muted-foreground">{catalogError}</p>
        ) : null}
          </div>
        </div>
      </div>
    </section>
  );
}
