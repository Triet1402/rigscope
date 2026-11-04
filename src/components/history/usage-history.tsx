"use client";

import Link from "next/link";
import { useState } from "react";
import { ChevronRight } from "lucide-react";
import type { MachineState } from "@/contracts";
import { Panel } from "@/components/panel";
import { defaultMetricsFor, gpuCards, metricOption, metricSeriesStyle } from "@/history/metrics";
import { continuityStepMs } from "@/history/chart";
import { useStoredHistory } from "./use-stored-history";
import { HistoryChart, type ChartSeries } from "./history-chart";

const EMPTY = new Set<string>();
const USAGE_METRICS = new Set(["cpu.usage", "memory.used", "gpu.utilization", "gpu.memory.used"]);

/** A fixed recent window for a quick machine check; History owns full investigation. */
export function UsageHistory({ machine }: { machine: MachineState }) {
  const [hiddenSeries, setHiddenSeries] = useState<ReadonlySet<string>>(EMPTY);
  const metricsKey = defaultMetricsFor(machine).filter((metric) => USAGE_METRICS.has(metric)).join(",");
  const { data, error } = useStoredHistory({ machineId: machine.machineId, metricsKey, range: "10m" });
  const toggleSeries = (key: string) => setHiddenSeries((previous) => {
    const next = new Set(previous);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    return next;
  });
  const cards = gpuCards(machine, data?.result.series ?? []);
  const series: ChartSeries[] = (data?.result.series ?? []).map((item) => {
    const option = metricOption(item.metric)!;
    const resourceIndex = item.metric.startsWith("gpu.") ? Math.max(1, cards.findIndex((card) => card.id === item.resourceId) + 1) : 0;
    const style = metricSeriesStyle(option, resourceIndex);
    return {
      key: `${item.metric}\u0000${item.resourceId}`, metric: item.metric, resourceId: item.resourceId,
      label: item.metric === "cpu.usage" ? "CPU" : item.metric === "memory.used" ? "RAM" : `GPU ${resourceIndex}`,
      color: style.color, dash: style.dash, points: item.points,
    };
  });

  return (
    <Panel title="Recent usage · 10m" action={
      <Link href={`/history?machineId=${encodeURIComponent(machine.machineId)}`} className="inline-flex min-h-8 items-center gap-1 text-xs text-accent hover:underline">
        History<ChevronRight className="h-3.5 w-3.5" aria-hidden />
      </Link>
    }>
      {error ? <p role="status" className="mb-2 text-xs text-warn">{data ? "Refresh failed · showing saved readings" : error.message}</p> : null}
      {data?.result.truncated ? <p className="mb-2 text-xs text-warn">Partial history</p> : null}
      {data ? (
        <div className="grid gap-5 lg:grid-cols-2">
          {[
            { title: "CPU & GPU", unit: "percent" },
            { title: "RAM & VRAM", unit: "bytes" },
          ].map(({ title, unit }) => (
            <HistoryChart key={unit} title={title} unit={unit}
              series={series.filter((item) => metricOption(item.metric)?.unit === unit)}
              fromMs={data.result.fromMs} toMs={data.result.toMs}
              stepMs={continuityStepMs(data.result.stepMs, data.result.tier, machine.expectedIntervalMs)} gaps={data.result.gaps}
              hiddenKeys={hiddenSeries} onToggleSeries={toggleSeries} emptyMessage="No readings" />
          ))}
        </div>
      ) : (
        <div className="flex min-h-48 items-center justify-center text-xs text-muted-foreground" role="status">{error ? "History unavailable" : "Loading history…"}</div>
      )}
    </Panel>
  );
}
