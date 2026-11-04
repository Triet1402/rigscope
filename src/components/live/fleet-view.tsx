"use client";

/**
 * The fleet view at "/" — the hub's authorized machines, one shared live
 * stream. Search narrows the existing authorized list by display name,
 * hostname, id or tags; it never edits metadata. Rows distinguish collector
 * connection, resource availability and receipt-time freshness. The session
 * previews use only samples this browser actually received.
 */

import Link from "next/link";
import { CloudOff, Hourglass, Plus, Search } from "lucide-react";
import { useState } from "react";

import { AppShell } from "@/components/shell/app-shell";
import { EmptyState, Panel } from "@/components/panel";
import { LiveStatusChip } from "@/components/live/live-status";
import { UserMenu } from "@/components/live/user-menu";
import { Tag } from "@/components/live/gauge";
import {
  StreamBanners,
  useAccessGate,
} from "@/components/live/stream-banners";
import {
  ageSince,
  attentionLabel,
  formatAge,
  formatBytes,
  formatIntervalMs,
  formatPercent,
  presentMachine,
  type MemoryFieldView,
  type ScalarFieldView,
} from "@/live/presentation";
import { useLive } from "@/live/live-provider";
import { cpuChartPath } from "@/live/history";
import type { SampleHistoryPoint } from "@/live/types";
import { useNow } from "@/live/use-now";
import {
  filterMachines,
  fleetResourceSummary,
  machineDisplayName,
  machineResourceView,
  machineTags,
  type ByteUsage,
  type FleetResourceSummary,
  type ResourceValue,
} from "@/resources/presentation";
import type { AttentionLevel } from "@/live/presentation";
import type { MachineState } from "../../contracts";

const ATTENTION_STYLE: Record<AttentionLevel, string> = {
  ok: "bg-ok-soft text-ok",
  degraded: "bg-warn-soft text-warn",
  stale: "bg-warn-soft text-warn",
  disconnected: "bg-crit-soft text-crit",
  unseen: "border border-border text-muted-foreground",
};

type StreamPhaseLike = "idle" | "connecting" | "live" | "reconnecting" | "disconnected"
  | "expired" | "no-permission";

function isHeldPhase(phase: StreamPhaseLike): boolean {
  return phase === "reconnecting" || phase === "disconnected";
}

function scalarField(value: ResourceValue<number>): ScalarFieldView {
  if (value.state === "value") {
    return value.stale ? {out: "stale", value: value.value} : {out: "value", value: value.value};
  }
  if (value.state === "unavailable") return {out: "unavailable", reason: value.reason};
  return {out: "absent"};
}

function memoryField(value: ResourceValue<ByteUsage>): MemoryFieldView {
  if (value.state === "value") {
    return value.stale
      ? {out: "stale", usedBytes: value.value.usedBytes, totalBytes: value.value.totalBytes}
      : {out: "value", usedBytes: value.value.usedBytes, totalBytes: value.value.totalBytes};
  }
  if (value.state === "unavailable") return {out: "unavailable", reason: value.reason};
  return {out: "absent"};
}

function AttentionPill({ level }: { level: AttentionLevel }) {
  return (
    <span
      className={`tnum inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs ${ATTENTION_STYLE[level]}`}
    >
      <span aria-hidden className="inline-block h-1.5 w-1.5 rounded-full bg-current opacity-70" />
      {attentionLabel(level)}
    </span>
  );
}

function staleTagTitle(thresholdMs: number): string {
  return `No update for over ${formatIntervalMs(thresholdMs)} (judged by hub receipt time)`;
}

function ScalarCell({ field, isHeld, thresholdMs }: { field: ScalarFieldView; isHeld: boolean; thresholdMs: number }) {
  if (field.out === "value") {
    return (
      <span className="tnum text-sm">
        {formatPercent(field.value)}
        {isHeld ? <HeldTag /> : null}
      </span>
    );
  }
  if (field.out === "stale") {
    return (
      <span className="tnum text-sm">
        {formatPercent(field.value)}
        <span className="ml-1.5">
          <Tag text="stale" title={staleTagTitle(thresholdMs)} />
        </span>
      </span>
    );
  }
  if (field.out === "unavailable") {
    return (
      <span className="text-xs text-warn" title={field.reason}>
        Unavailable
      </span>
    );
  }
  return <span className="tnum text-sm text-muted-foreground">—</span>;
}

function HeldTag() {
  return (
    <span className="ml-1.5">
      <Tag text="held" title="Last known value, kept while the connection is down" />
    </span>
  );
}

function MemoryText(field: MemoryFieldView): string {
  if (field.out === "value" || field.out === "stale") {
    return `${formatBytes(field.usedBytes)} / ${formatBytes(field.totalBytes)}`;
  }
  if (field.out === "unavailable") return "Unavailable";
  return "—";
}

function GpuCell({ summary }: { summary: FleetResourceSummary["gpu"] }) {
  if (summary === null) {
    return <span className="text-xs text-muted-foreground">No GPU data</span>;
  }
  if (summary.supportedCount === 0) {
    return (
      <span className="text-xs text-muted-foreground" title={summary.reason ?? "No supported telemetry"}>
        {summary.count} adapter{summary.count === 1 ? "" : "s"} · no telemetry
      </span>
    );
  }
  if (summary.reason) {
    return (
      <span className="text-xs text-warn" title={summary.reason}>
        {summary.supportedCount} GPU{summary.supportedCount === 1 ? "" : "s"} · unavailable
      </span>
    );
  }
  const util =
    summary.maxUtilizationPercent === null ? "util —" : `util ${formatPercent(summary.maxUtilizationPercent)}`;
  const vram =
    summary.memoryUsedBytes !== null && summary.memoryTotalBytes !== null
      ? `${formatBytes(summary.memoryUsedBytes)} / ${formatBytes(summary.memoryTotalBytes)}`
      : "VRAM —";
  return (
    <span className="tnum text-xs">
      {summary.partial ? (
        <>
          <span className="text-warn">
            {summary.okCount}/{summary.supportedCount} GPUs reporting
          </span>
          <span className="text-muted-foreground"> · {util} · {vram} </span>
          <Tag text="partial" title="At least one supported GPU has no current reading" />
          {summary.stale ? (
            <span className="ml-1.5">
              <Tag text="last known" title="Retained from the last accepted sample" />
            </span>
          ) : null}
        </>
      ) : (
        <>
          <span className="text-foreground">{summary.supportedCount}</span>
          <span className="text-muted-foreground">
            {" "}GPU{summary.supportedCount === 1 ? "" : "s"} · {util} · {vram}
          </span>
          {summary.stale ? (
            <span className="ml-1.5">
              <Tag text="last known" title="Retained from the last accepted sample" />
            </span>
          ) : null}
        </>
      )}
    </span>
  );
}

function MachineTags({ state }: { state: MachineState }) {
  const tags = machineTags(state);
  if (tags.length === 0) return null;
  return (
    <ul className="mt-1 flex flex-wrap gap-1">
      {tags.map((tag) => (
        <li
          key={tag}
          className="rounded-full border border-border px-1.5 py-0.5 text-xs text-muted-foreground"
        >
          {tag}
        </li>
      ))}
    </ul>
  );
}

function MachineIdentity({ state }: { state: MachineState }) {
  const primary = machineDisplayName(state);
  const secondary = [state.hostname !== primary ? state.hostname : null]
    .filter((part): part is string => Boolean(part && part !== primary))
    .join(" · ");
  return (
    <div className="flex min-w-0 flex-col">
      <span className="truncate text-sm font-medium">{primary}</span>
      {secondary ? (
        <span className="tnum truncate text-xs text-muted-foreground">{secondary}</span>
      ) : null}
      <MachineTags state={state} />
    </div>
  );
}

function FleetTrend({ points }: { points: readonly SampleHistoryPoint[] }) {
  const cutoff = (points.at(-1)?.receivedAtLocal ?? 0) - 10 * 60_000;
  const recent = points.filter((point) => point.receivedAtLocal >= cutoff);
  const cpu = cpuChartPath(recent, 160, 40);
  const memory = cpuChartPath(recent.map((point) => ({ ...point, cpuPercent: point.memoryPercent })), 160, 40);
  return (
    <div className="w-40 max-w-full" title="CPU and memory since sign-in">
      <svg viewBox="0 0 160 40" className="h-10 w-full" role="img" aria-label="Recent CPU and memory usage since sign-in">
        <line x1="4" x2="156" y1="36" y2="36" stroke="var(--border)" />
        {cpu ? <path d={cpu} fill="none" stroke="var(--cpu)" strokeWidth="1.5" /> : null}
        {memory ? <path d={memory} fill="none" stroke="var(--mem)" strokeWidth="1.5" /> : null}
      </svg>
      <div className="flex gap-3 text-xs text-muted-foreground"><span className="inline-flex items-center gap-1"><span className="h-1.5 w-1.5 rounded-full bg-cpu" />CPU</span><span className="inline-flex items-center gap-1"><span className="h-1.5 w-1.5 rounded-full bg-mem" />RAM</span></div>
    </div>
  );
}

function MachineRow({ state, now, held, points }: { state: MachineState; now: number; held: boolean; points: readonly SampleHistoryPoint[] }) {
  const view = machineResourceView(state, now);
  const summary = fleetResourceSummary(state, now);
  const presented = presentMachine(state, now);
  const stale = view.freshness === "stale";
  const staleTitle = staleTagTitle(view.thresholdMs);
  const memory = memoryField(summary.memory);
  const degraded =
    view.cpu.total.state === "unavailable" ||
    view.memory.state === "unavailable" ||
    view.gpus.family.state === "unavailable" ||
    view.gpus.partial ||
    view.disks.family.state === "unavailable" ||
    view.network.family.state === "unavailable";
  const attention: AttentionLevel =
    presented.attention === "ok" && degraded ? "degraded" : presented.attention;

  return (
    <tr className="transition-colors hover:bg-card-strong/60">
      <td className="border-b border-border py-2.5 pr-4">
        <Link
          href={`/machines/${encodeURIComponent(state.machineId)}`}
          className="inline-flex min-w-0 flex-col rounded-sm transition-colors hover:underline"
        >
          <MachineIdentity state={state} />
        </Link>
      </td>
      <td className="border-b border-border py-2.5 pr-4">
        <span className="tnum inline-flex items-center gap-1.5 text-xs">
          <span
            aria-hidden
            className={`inline-block h-1.5 w-1.5 shrink-0 rounded-full ${
              state.connected ? "bg-ok" : "bg-crit"
            }`}
          />
          {state.connected ? "Connected" : "Disconnected"}
        </span>
      </td>
      <td className="border-b border-border py-2.5 pr-4">
        <ScalarCell field={scalarField(summary.cpu)} isHeld={held} thresholdMs={view.thresholdMs} />
      </td>
      <td className="border-b border-border py-2.5 pr-4">
        <span className="tnum text-sm">
          {MemoryText(memory)}
          {memory.out === "stale" ? (
            <span className="ml-1.5">
              <Tag text="stale" title={staleTitle} />
            </span>
          ) : memory.out === "value" && held ? (
            <HeldTag />
          ) : null}
        </span>
      </td>
      <td className="border-b border-border py-2.5 pr-4">
        <GpuCell summary={summary.gpu} />
      </td>
      <td className="border-b border-border py-2.5 pr-4">
        <span className="tnum text-sm">
          {formatAge(ageSince(state.lastReceivedAt, now))}
          {stale ? (
            <span className="ml-1.5">
              <Tag text="stale" title={staleTitle} />
            </span>
          ) : held ? (
            <HeldTag />
          ) : null}
        </span>
      </td>
      <td className="border-b border-border py-2.5 pr-4"><FleetTrend points={points} /></td>
      <td className="border-b border-border py-2.5">
        <AttentionPill level={attention} />
      </td>
    </tr>
  );
}

function MachineCard({ state, now, held, points }: { state: MachineState; now: number; held: boolean; points: readonly SampleHistoryPoint[] }) {
  const view = machineResourceView(state, now);
  const summary = fleetResourceSummary(state, now);
  const presented = presentMachine(state, now);
  const stale = view.freshness === "stale";
  const staleTitle = staleTagTitle(view.thresholdMs);
  const memory = memoryField(summary.memory);
  const degraded =
    view.cpu.total.state === "unavailable" ||
    view.memory.state === "unavailable" ||
    view.gpus.family.state === "unavailable" ||
    view.gpus.partial ||
    view.disks.family.state === "unavailable" ||
    view.network.family.state === "unavailable";
  const attention: AttentionLevel =
    presented.attention === "ok" && degraded ? "degraded" : presented.attention;

  return (
    <li>
      <Link
        href={`/machines/${encodeURIComponent(state.machineId)}`}
        className="block rounded-xl border border-border bg-card p-4 transition-colors hover:border-muted-foreground/40"
      >
        <div className="flex items-center justify-between gap-3">
          <MachineIdentity state={state} />
          <AttentionPill level={attention} />
        </div>
        <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
          <dt className="text-muted-foreground">Collector</dt>
          <dd className="tnum">{state.connected ? "Connected" : "Disconnected"}</dd>
          <dt className="text-muted-foreground">CPU</dt>
          <dd className="tnum">
            {summary.cpu.state === "value"
              ? `${formatPercent(summary.cpu.value)}${summary.cpu.stale ? " · last known" : ""}`
              : summary.cpu.state === "unavailable"
                ? "Unavailable"
                : "—"}
          </dd>
          <dt className="text-muted-foreground">Memory</dt>
          <dd className="tnum">
            {MemoryText(memory)}
            {memory.out === "stale" ? (
              <span className="ml-1.5">
                <Tag text="stale" title={staleTitle} />
              </span>
            ) : null}
          </dd>
          <dt className="text-muted-foreground">GPU</dt>
          <dd>
            <GpuCell summary={summary.gpu} />
          </dd>
          <dt className="text-muted-foreground">Last received</dt>
          <dd className="tnum">
            {formatAge(ageSince(state.lastReceivedAt, now))}
            {stale ? (
              <span className="ml-1.5">
                <Tag text="stale" title={staleTitle} />
              </span>
            ) : null}
          </dd>
        </dl>
        <div className="mt-3"><FleetTrend points={points} /></div>
      </Link>
    </li>
  );
}

const EMPTY_FLEET_HINT =
  "Your authorized fleet is empty. Ask your hub administrator about enrollment and your machine access.";

export function FleetView() {
  const { phase, machines, history, session } = useLive();
  const gate = useAccessGate();
  const now = useNow();
  const [query, setQuery] = useState("");
  const machineCount = machines.length;
  const held = isHeldPhase(phase);
  const filtered = filterMachines(machines, query);
  const showTable = machineCount > 0 && (phase === "live" || held);

  return (
    <AppShell
      title="Fleet"
      headerRight={
        <>
          {session.kind === "signed-in" && session.user.admin ? (
            <Link href="/administration#collectors" className="inline-flex items-center gap-1.5 rounded-md border border-border px-2.5 py-1.5 text-xs hover:bg-card-strong">
              <Plus className="h-3.5 w-3.5" aria-hidden /><span className="hidden sm:inline">Add machine</span><span className="sr-only sm:hidden">Add machine</span>
            </Link>
          ) : null}
          <LiveStatusChip />
          <UserMenu />
        </>
      }
    >
      {gate ? (
        gate
      ) : (
        <>
          <StreamBanners />

          {phase === "connecting" && machineCount === 0 ? (
            <Panel title="Waiting for the first snapshot" className="mt-10">
              <EmptyState
                icon={Hourglass}
                note="Waiting for the first snapshot…"
              />
            </Panel>
          ) : null}

          {(phase === "live" || held) && machineCount === 0 ? (
            <Panel title="No machines" className="mt-10">
              <EmptyState
                icon={CloudOff}
                note="No machines are available in your authorized fleet."
              />
            </Panel>
          ) : null}

          {showTable ? (
            <>
              <div className="mb-4 flex flex-wrap items-end justify-between gap-3">
                <div className="min-w-0">
                  <label htmlFor="fleet-search" className="sr-only">
                    Search machines by name or tag
                  </label>
                  <div className="relative">
                    <Search
                      className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
                      aria-hidden
                    />
                    <input
                      id="fleet-search"
                      type="search"
                      value={query}
                      onChange={(event) => setQuery(event.target.value)}
                      placeholder="Search name or tag"
                      className="w-64 max-w-full rounded-md border border-border bg-card py-1.5 pl-8 pr-3 text-sm text-foreground placeholder:text-muted-foreground"
                    />
                  </div>
                </div>
                <p className="tnum text-xs text-muted-foreground" role="status">
                  {filtered.length} of {machineCount} machine{machineCount === 1 ? "" : "s"}
                  {query.trim() ? ` matching “${query.trim()}”` : ""}
                </p>
              </div>

              {filtered.length === 0 ? (
                <Panel title="No matching machines">
                  <EmptyState
                    icon={Search}
                    note={`No machines match “${query.trim()}”.`}
                  />
                </Panel>
              ) : (
                <>
                  <div className="hidden md:block">
                    <Panel
                      title="Machines"
                      action={
                        <span>
                          {machineCount} machine{machineCount === 1 ? "" : "s"}
                        </span>
                      }
                    >
                      <div className="overflow-x-auto">
                        <table className="w-full border-separate border-spacing-0 text-left">
                          <caption className="sr-only">
                            Fleet machines with collector connection, CPU, memory, GPU summary, and
                            sample freshness
                          </caption>
                          <thead>
                            <tr className="text-xs text-muted-foreground">
                              <th scope="col" className="pb-2 pr-4 font-normal">Machine</th>
                              <th scope="col" className="pb-2 pr-4 font-normal">Collector</th>
                              <th scope="col" className="pb-2 pr-4 font-normal">CPU</th>
                              <th scope="col" className="pb-2 pr-4 font-normal">Memory</th>
                              <th scope="col" className="pb-2 pr-4 font-normal">GPU</th>
                              <th scope="col" className="pb-2 pr-4 font-normal">Last received</th>
                              <th scope="col" className="pb-2 pr-4 font-normal">Recent load</th>
                              <th scope="col" className="pb-2 font-normal">State</th>
                            </tr>
                          </thead>
                          <tbody>
                            {filtered.map((state) => (
                              <MachineRow
                                key={state.machineId}
                                state={state}
                                now={now}
                                held={held}
                                points={history.get(state.machineId) ?? []}
                              />
                            ))}
                          </tbody>
                        </table>
                      </div>
                    </Panel>
                  </div>

                  <div className="md:hidden">
                    <Panel
                      title="Machines"
                      action={<span className="tnum">{filtered.length} machine{filtered.length === 1 ? "" : "s"}</span>}
                    >
                      <ul className="flex flex-col gap-3">
                        {filtered.map((state) => (
                          <MachineCard key={state.machineId} state={state} now={now} held={held} points={history.get(state.machineId) ?? []} />
                        ))}
                      </ul>
                    </Panel>
                  </div>

                </>
              )}
            </>
          ) : null}
        </>
      )}
    </AppShell>
  );
}
