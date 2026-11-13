"use client";

/**
 * Builds History panels from a stored query result. Each panel owns one or
 * more aligned charts; every chart keeps the same time domain so CPU, GPU and
 * storage can be read against each other without inventing data.
 *
 * Metrics share charts when their unit matches; capacity views switch units, and
 * large per-core selections use a peak-preserving heatmap.
 */

import { Panel } from "@/components/panel";
import type { MachineState } from "@/contracts";
import type { HistoryResult, HistorySeries } from "@/history/contract";
import {
  gpuCards,
  metricOption,
  metricSeriesStyle,
  resourceLabel,
  type HistoryMetricOption,
  type CapacityUnits,
} from "@/history/metrics";
import {
  CORE_LINE_LIMIT,
  availableCoreIds,
  coreLabel,
  resolveCoreSelection,
} from "@/history/cores";
import { continuityStepMs } from "@/history/chart";
import { formatMetricValue } from "@/history/presentation";
import { HistoryChart, type ChartSeries } from "./history-chart";
import { CoreHistoryChart } from "./core-history-chart";
import { ChartInspectionProvider } from "./chart-inspection";
import { useHistoryCores } from "./history-prefs";

type ChartModel = {
  key: string;
  title: string;
  unit: string;
  series: ChartSeries[];
  emptyMessage: string;
  note?: string;
  unitGroup?: keyof CapacityUnits;
  views?: Array<{ unit: string; series: ChartSeries[] }>;
};

type PanelModel = {
  key: string;
  title: string;
  charts: ChartModel[];
};

type Props = {
  machine: MachineState;
  result: HistoryResult;
  selectedMetrics: readonly string[];
  hiddenCards: ReadonlySet<string>;
  hiddenSeries: ReadonlySet<string>;
  onToggleSeries: (key: string) => void;
  capacityUnits: CapacityUnits;
  onCapacityUnitChange: (group: keyof CapacityUnits, unit: "bytes" | "percent") => void;
};

type BuildProps = Pick<Props, "machine" | "result" | "selectedMetrics" | "hiddenCards"> & {
  /** Resolved visible cores; omitted means all available cores. */
  coreIds?: readonly string[];
};

function seriesKey(metric: string, resourceId: string): string {
  return `${metric}\u0000${resourceId}`;
}

function chartSeries(
  result: HistoryResult,
  machine: MachineState,
  metric: string,
  resourceId?: string,
  labelFor?: (series: HistorySeries, option: HistoryMetricOption, index: number) => string,
): ChartSeries[] {
  const option = metricOption(metric);
  if (option === null) return [];
  const matching = result.series.filter(
    (series) => series.metric === metric && (resourceId === undefined || series.resourceId === resourceId),
  );
  return matching.map((series, index) => {
    const style = metricSeriesStyle(option, index);
    const label = labelFor?.(series, option, index) ?? resourceLabel(machine, metric, series.resourceId);
    return {
      key: seriesKey(series.metric, series.resourceId),
      label,
      metric: series.metric,
      resourceId: series.resourceId,
      color: style.color,
      dash: style.dash,
      points: series.points,
    };
  });
}

function gpuMetricSeries(
  result: HistoryResult,
  machine: MachineState,
  metric: string,
  resourceId: string,
): ChartSeries[] {
  return chartSeries(result, machine, metric, resourceId, (series, option) =>
    metric === "gpu.utilization"
      ? "Utilization"
      : metric === "gpu.memory.used"
        ? "VRAM used"
        : option.shortLabel,
  ).map((series) => ({ ...series }));
}

function coreChartSeries(
  result: HistoryResult,
  coreIds: readonly string[],
): ChartSeries[] {
  const option = metricOption("cpu.core");
  if (option === null) return [];
  const storedCores = new Map(result.series.filter((series) => series.metric === "cpu.core").map((series) => [series.resourceId, series]));
  return coreIds.flatMap((id, index) => {
    const stored = storedCores.get(id);
    const style = metricSeriesStyle(option, index);
    return [
      {
        key: seriesKey("cpu.core", id),
        label: coreLabel(id),
        metric: "cpu.core",
        resourceId: id,
        color: style.color,
        dash: style.dash,
        points: stored?.points ?? [],
      },
    ];
  });
}

export function buildHistoryPanels(props: BuildProps): PanelModel[] {
  const { machine, result, selectedMetrics, hiddenCards, coreIds } = props;
  const selected = new Set(selectedMetrics);
  const panels: PanelModel[] = [];

  const cpuMetrics = [
    "cpu.usage",
    "cpu.core",
    "cpu.temperature",
    "cpu.power",
    "cpu.iowait",
    "cpu.pressure",
  ].filter((metric) => selected.has(metric));
  if (cpuMetrics.length > 0) {
    const charts: ChartModel[] = [];

    const loadMetrics = ["cpu.usage", "cpu.iowait", "cpu.pressure"].filter((metric) => selected.has(metric));
    if (loadMetrics.length > 0) {
      charts.push({
        key: "cpu.usage",
        title: loadMetrics.length > 1 ? "Usage & stalls" : metricOption(loadMetrics[0]!)!.label,
        unit: "percent",
        series: loadMetrics.flatMap((metric) => chartSeries(result, machine, metric, undefined,
          (_series, option) => metric === "cpu.pressure" ? "Pressure (some)" : option.shortLabel)),
        emptyMessage: "No stored CPU readings in this range.",
      });
    }

    if (selected.has("cpu.core")) {
      const option = metricOption("cpu.core")!;
      const available = availableCoreIds(machine, result.series);
      const shownIds = coreIds ?? resolveCoreSelection(null, available);
      const series = coreChartSeries(result, shownIds);
      const bounded = available.length > shownIds.length;
      charts.push({
        key: "cpu.core",
        title: bounded
          ? `Per-core usage · ${shownIds.length} of ${available.length}`
          : "Per-core usage",
        unit: option.unit,
        series,
        emptyMessage:
          available.length === 0
            ? "No stored per-core readings in this range."
            : shownIds.length === 0
              ? "No CPU cores are selected. Choose cores under Metrics."
              : "No stored per-core readings for the selected cores in this range.",
      });
    }

    if (selected.has("cpu.temperature")) {
      const option = metricOption("cpu.temperature")!;
      charts.push({
        key: "cpu.temperature",
        title: "Temperature",
        unit: option.unit,
        series: chartSeries(result, machine, "cpu.temperature"),
        emptyMessage: "No stored CPU temperature readings in this range.",
      });
    }

    if (selected.has("cpu.power")) {
      const option = metricOption("cpu.power")!;
      charts.push({
        key: "cpu.power",
        title: "Package power",
        unit: option.unit,
        series: chartSeries(result, machine, "cpu.power"),
        emptyMessage: "No stored CPU package power readings in this range.",
      });
    }

    panels.push({
      key: "cpu",
      title: "CPU",
      charts,
    });
  }

  const memoryMetrics = [
    "memory.used",
    "memory.usage",
    "memory.pressure.some",
    "memory.pressure.full",
    "swap.used",
  ].filter((metric) => selected.has(metric));
  if (memoryMetrics.length > 0) {
    const charts: ChartModel[] = [];
    const capacityMetrics = ["memory.used", "memory.usage"].filter((metric) => selected.has(metric));
    if (capacityMetrics.length > 0) {
      const views = capacityMetrics.map((metric) => ({
        unit: metricOption(metric)!.unit,
        series: chartSeries(result, machine, metric, undefined, () => "RAM"),
      }));
      charts.push({
        key: "memory.capacity",
        title: "RAM",
        ...views[0]!,
        unitGroup: "memory",
        views,
        emptyMessage: "No stored memory readings in this range.",
      });
    }
    if (selected.has("swap.used")) {
      charts.push({
        key: "swap.used",
        title: "Swap",
        unit: "bytes",
        series: chartSeries(result, machine, "swap.used"),
        emptyMessage: "No stored swap readings in this range.",
      });
    }
    const pressureMetrics = ["memory.pressure.some", "memory.pressure.full"].filter((metric) =>
      selected.has(metric),
    );
    if (pressureMetrics.length > 0) {
      const option = metricOption(pressureMetrics[0]!)!;
      const series = pressureMetrics.flatMap((metric) =>
        chartSeries(result, machine, metric, undefined, (_item, itemOption) =>
          metric === "memory.pressure.some" ? "Some" : metric === "memory.pressure.full" ? "Full" : itemOption.shortLabel,
        ),
      );
      charts.push({
        key: "memory.pressure",
        title: "Memory pressure",
        unit: option.unit,
        series,
        emptyMessage: "No stored memory pressure in this range.",
      });
    }
    panels.push({
      key: "memory",
      title: "Memory",
      charts,
    });
  }

  const gpuMetrics = [
    "gpu.utilization",
    "gpu.memory.used",
    "gpu.temperature",
    "gpu.junction.temperature",
    "gpu.vram.temperature",
    "gpu.power",
  ].filter((metric) => selected.has(metric));
  if (gpuMetrics.length > 0) {
    const cards = gpuCards(machine, result.series);
    const visibleCards = cards.filter((card) => !hiddenCards.has(card.id));
    if (visibleCards.length === 0) {
      panels.push({
        key: "gpu-empty",
        title: "GPU",
        charts: [
          {
            key: "gpu-empty",
            title: "Per-card history",
            unit: "percent",
            series: [],
            emptyMessage:
              cards.length > 0
                ? "All GPU cards are hidden. Re-enable a card under Metrics to see its panels."
                : "No supported GPU cards or stored GPU history in this range.",
          },
        ],
      });
    }
    for (const card of visibleCards) {
      const charts: ChartModel[] = gpuMetrics.map((metric) => {
        const option = metricOption(metric)!;
        const series = gpuMetricSeries(result, machine, metric, card.id);
        const totalNote =
          metric === "gpu.memory.used" && card.totalBytes !== null
            ? `of ${formatMetricValue(card.totalBytes, "bytes")}`
            : undefined;
        return {
          key: `${card.id}-${metric}`,
          title: option.shortLabel,
          unit: option.unit,
          series,
          emptyMessage: `No stored ${option.shortLabel.toLowerCase()} readings for this card in this range.`,
          note: totalNote,
        };
      });
      panels.push({
        key: `gpu-${card.id}`,
        title: `GPU ${card.index + 1}${card.label ? ` · ${card.label}` : ""}`,
        charts,
      });
    }
  }

  const filesystemMetrics = ["fs.used", "fs.usage", "fs.inodes.usage"].filter((metric) =>
    selected.has(metric),
  );
  if (filesystemMetrics.length > 0) {
    const charts: ChartModel[] = [];
    const capacityMetrics = ["fs.used", "fs.usage"].filter((metric) => selected.has(metric));
    if (capacityMetrics.length > 0) {
      const views = capacityMetrics.map((metric) => ({
        unit: metricOption(metric)!.unit,
        series: chartSeries(result, machine, metric),
      }));
      charts.push({
        key: "fs.capacity",
        title: "Capacity used",
        ...views[0]!,
        unitGroup: "filesystem",
        views,
        emptyMessage: "No stored filesystem usage in this range.",
      });
    }
    if (selected.has("fs.inodes.usage")) {
      const option = metricOption("fs.inodes.usage")!;
      charts.push({
        key: "fs.inodes.usage",
        title: "Inodes used",
        unit: option.unit,
        series: chartSeries(result, machine, "fs.inodes.usage"),
        emptyMessage: "No stored inode usage in this range.",
      });
    }
    panels.push({
      key: "filesystems",
      title: "Filesystems",
      charts,
    });
  }

  const diskMetrics = ["disk.read", "disk.write"].filter((metric) => selected.has(metric));
  if (diskMetrics.length > 0) {
    const series = diskMetrics.flatMap((metric) => {
      const option = metricOption(metric)!;
      return chartSeries(result, machine, metric, undefined, (item) =>
        `${resourceLabel(machine, metric, item.resourceId)} ${option.shortLabel.toLowerCase()}`,
      );
    });
    panels.push({
      key: "disk",
      title: "Disk throughput",
      charts: [
        {
          key: "disk",
          title: "Read and write",
          unit: "bytes_per_second",
          series,
          emptyMessage: "No stored disk throughput in this range.",
        },
      ],
    });
  }

  const latencyMetrics = ["disk.read.latency", "disk.write.latency"].filter((metric) =>
    selected.has(metric),
  );
  const ioPressureMetrics = ["io.pressure.some", "io.pressure.full"].filter((metric) =>
    selected.has(metric),
  );
  const diskHealthSelected =
    latencyMetrics.length > 0 || ioPressureMetrics.length > 0 || selected.has("disk.utilization");
  if (diskHealthSelected) {
    const charts: ChartModel[] = [];
    if (latencyMetrics.length > 0) {
      const series = latencyMetrics.flatMap((metric) => {
        const option = metricOption(metric)!;
        return chartSeries(result, machine, metric, undefined, (item) =>
          `${resourceLabel(machine, metric, item.resourceId)} ${option.shortLabel.toLowerCase()}`,
        );
      });
      const option = metricOption(latencyMetrics[0]!)!;
      charts.push({
        key: "disk.latency",
        title: "Latency",
        unit: option.unit,
        series,
        emptyMessage: "No stored disk latency in this range.",
      });
    }
    if (selected.has("disk.utilization")) {
      const option = metricOption("disk.utilization")!;
      charts.push({
        key: "disk.utilization",
        title: "Utilization",
        unit: option.unit,
        series: chartSeries(result, machine, "disk.utilization"),
        emptyMessage: "No stored disk utilization in this range.",
      });
    }
    if (ioPressureMetrics.length > 0) {
      const option = metricOption(ioPressureMetrics[0]!)!;
      const series = ioPressureMetrics.flatMap((metric) =>
        chartSeries(result, machine, metric, undefined, (_item, itemOption) =>
          metric === "io.pressure.some" ? "Some" : metric === "io.pressure.full" ? "Full" : itemOption.shortLabel,
        ),
      );
      charts.push({
        key: "io.pressure",
        title: "I/O pressure",
        unit: option.unit,
        series,
        emptyMessage: "No stored I/O pressure in this range.",
      });
    }
    panels.push({
      key: "disk-health",
      title: "Disk health",
      charts,
    });
  }

  const networkMetrics = ["net.rx", "net.tx"].filter((metric) => selected.has(metric));
  if (networkMetrics.length > 0) {
    const series = networkMetrics.flatMap((metric) => {
      const option = metricOption(metric)!;
      return chartSeries(result, machine, metric, undefined, (item) =>
        `${resourceLabel(machine, metric, item.resourceId)} ${option.shortLabel.toLowerCase()}`,
      );
    });
    panels.push({
      key: "network",
      title: "Network throughput",
      charts: [
        {
          key: "network",
          title: "Received and sent",
          unit: "bytes_per_second",
          series,
          emptyMessage: "No stored network throughput in this range.",
        },
      ],
    });
  }

  return panels;
}

export function HistoryCharts(props: Props) {
  const { machine, result, selectedMetrics, hiddenCards, hiddenSeries, onToggleSeries } = props;
  const [corePreference] = useHistoryCores(machine.machineId);
  const coreIds = resolveCoreSelection(
    corePreference,
    availableCoreIds(machine, result.series),
  );
  const panels = buildHistoryPanels({ machine, result, selectedMetrics, hiddenCards, coreIds });

  return (
    <ChartInspectionProvider key={`${machine.machineId}|${result.toMs - result.fromMs}`}>
      <div className="grid grid-cols-1 gap-5">
        {panels.map((panel) => {
          const fullWidthCore = panel.charts.find((chart) => chart.key === "cpu.core" && chart.series.length > 4);
          // Put the bounded multi-core chart after the smaller resource charts,
          // keeping DOM, visual and keyboard order aligned.
          const charts = fullWidthCore
            ? [...panel.charts.filter((chart) => chart !== fullWidthCore), fullWidthCore]
            : panel.charts;
          const gridClass = fullWidthCore && charts.length === 4
            ? "grid gap-6 lg:grid-cols-2 xl:grid-cols-3"
            : charts.length > 1 && !(fullWidthCore && charts.length === 2)
              ? "grid gap-6 lg:grid-cols-2"
              : "flex flex-col gap-6";
          return (
            <Panel key={panel.key} title={panel.title}>
              <div className={gridClass}>
                {charts.map((chart, index) => {
                  const group = chart.unitGroup;
                  const preferredUnit = group ? props.capacityUnits[group] : undefined;
                  const activeView = chart.views?.find((view) => view.unit === preferredUnit) ?? chart.views?.[0] ?? chart;
                  const spansRow = chart === fullWidthCore || (!fullWidthCore && charts.length % 2 === 1 && index === charts.length - 1);
                  if (chart.key === "cpu.core" && chart.series.length > CORE_LINE_LIMIT) {
                    return <CoreHistoryChart key={chart.key} title={chart.title} series={chart.series}
                      fromMs={result.fromMs} toMs={result.toMs} gaps={result.gaps}
                      stepMs={continuityStepMs(result.stepMs, result.tier, machine.expectedIntervalMs)}
                      hiddenKeys={hiddenSeries} onToggleSeries={onToggleSeries} />;
                  }
                  return (
                    <HistoryChart
                      key={chart.key}
                      title={chart.title}
                      unit={activeView.unit}
                      series={activeView.series}
                      fromMs={result.fromMs}
                      toMs={result.toMs}
                      stepMs={continuityStepMs(result.stepMs, result.tier, machine.expectedIntervalMs)}
                      gaps={result.gaps}
                      hiddenKeys={hiddenSeries}
                      onToggleSeries={onToggleSeries}
                      emptyMessage={chart.emptyMessage}
                      note={chart.note}
                      action={group && chart.views && chart.views.length > 1 ? (
                        <div role="group" aria-label={`${chart.title} units`} className="inline-flex rounded-md border border-border bg-background p-0.5">
                          {chart.views.map((view) => (
                            <button
                              key={view.unit}
                              type="button"
                              aria-pressed={view.unit === activeView.unit}
                              onClick={() => props.onCapacityUnitChange(group, view.unit as "bytes" | "percent")}
                              className={`min-h-7 rounded px-2 text-xs transition-colors ${view.unit === activeView.unit ? "bg-selected text-foreground" : "text-muted-foreground hover:text-foreground"}`}
                            >
                              {view.unit === "bytes" ? "Bytes" : "%"}
                            </button>
                          ))}
                        </div>
                      ) : undefined}
                      maxDrawPoints={chart.key === "cpu.core" ? 150 : undefined}
                      className={spansRow ? "lg:col-span-full" : undefined}
                    />
                  );
                })}
              </div>
            </Panel>
          );
        })}
      </div>
    </ChartInspectionProvider>
  );
}
