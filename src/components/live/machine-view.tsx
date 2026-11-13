"use client";

/**
 * Machine drilldown at /machines?machineId=&lt;id&gt;.
 *
 * The view is built from one deterministic `machineResourceView` deriving
 * every panel from the machine's latest metrics record: per-core CPU, swap,
 * GPU, disk and network detail. Collector connection, receipt freshness,
 * source-capture times and the in-memory session chart stay independent
 * honest axes. Missing readings explain themselves instead of rendering zero.
 */

import Link from "next/link";
import { ArrowLeft, HardDrive, Timer } from "lucide-react";

import { AppShell } from "@/components/shell/app-shell";
import { EmptyState, Panel } from "@/components/panel";
import { LiveStatusChip } from "@/components/live/live-status";
import { UserMenu } from "@/components/live/user-menu";
import { UsageHistory } from "@/components/history/usage-history";
import { useMachineSelection } from "@/live/machine-selection";
import {
  StreamBanners,
  useAccessGate,
} from "@/components/live/stream-banners";
import {
  CpuResourcePanel,
  DiskResourcePanel,
  GpuResourcePanel,
  MemorySwapPanel,
  NetworkResourcePanel,
} from "@/components/resources/machine-resources";
import {
  ageSince,
  captureClockSkew,
  formatAge,
  formatIntervalMs,
  freshnessThresholdFor,
} from "@/live/presentation";
import { useLive } from "@/live/live-provider";
import { useNow } from "@/live/use-now";
import { machineResourceView } from "@/resources/presentation";
import type { MachineState } from "../../contracts";

function formatClock(iso: string): string {
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return iso;
  return date.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

function formatDay(iso: string): string {
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return "";
  return date.toLocaleDateString(undefined, { dateStyle: "medium" });
}

function TimestampText(props: { iso: string | null }) {
  const { iso } = props;
  if (iso === null) {
    return <span className="text-xs text-muted-foreground">—</span>;
  }
  return (
    <span className="tnum text-sm">
      {formatClock(iso)}
      <span className="ml-2 text-xs text-muted-foreground">{formatDay(iso)}</span>
    </span>
  );
}

function skewAmount(skewMs: number): string {
  const abs = Math.abs(skewMs);
  if (abs >= 60_000) {
    return `${Math.abs(Math.round(skewMs / 60_000))} min`;
  }
  return `${Math.abs(Math.round(skewMs / 1000))} s`;
}

function ClockSkewNote({ state }: { state: MachineState }) {
  const skew = captureClockSkew(state);
  if (skew.kind !== "ahead" && skew.kind !== "behind") return null;
  return (
    <p
      role="status"
      className="tnum flex items-start gap-2 rounded-lg bg-warn-soft px-3 py-2 text-xs text-foreground"
    >
      <Timer className="mt-0.5 h-3.5 w-3.5 shrink-0 text-warn" aria-hidden />
      <span>
        Collector clock:{" "}
        <strong className="font-medium">{skewAmount(skew.skewMs)}</strong>{" "}
        {skew.kind === "ahead" ? "ahead of" : "behind"} hub time.
      </span>
    </p>
  );
}

function CollectorPanel({ state }: { state: MachineState }) {
  return (
    <Panel title="Collector">
      <div className="flex flex-col gap-3 py-1">
        <p className="flex items-center gap-2 text-sm">
          <span
            aria-hidden
            className={`inline-block h-2 w-2 shrink-0 rounded-full ${
              state.connected ? "bg-ok" : "bg-crit"
            }`}
          />
          {state.connected ? "Connected to the hub" : "Not connected to the hub"}
        </p>
        <dl className="flex flex-col gap-1 text-xs">
          <div className="flex gap-2">
            <dt className="shrink-0 text-muted-foreground">Collector</dt>
            <dd className="tnum truncate">{state.collectorId}</dd>
          </div>
          <div className="flex gap-2">
            <dt className="shrink-0 text-muted-foreground">Machine</dt>
            <dd className="tnum truncate">{state.machineId}</dd>
          </div>
        </dl>
        <Link
          href="/connections"
          className="w-fit rounded-md text-xs text-accent transition-colors hover:underline"
        >
          Connections
        </Link>
      </div>
    </Panel>
  );
}

function TimesPanel({ state, now }: { state: MachineState; now: number }) {
  const metrics = state.metrics;
  const capturedIso = metrics?.capturedAt ?? null;
  const receivedIso = metrics?.receivedAt ?? state.lastReceivedAt;
  const cadenceKnown = state.expectedIntervalMs != null;
  const expected = cadenceKnown ? state.expectedIntervalMs! : null;
  const thresholdMs = freshnessThresholdFor(state.expectedIntervalMs);

  return (
    <Panel title="Sample times">
      <div className="flex flex-col gap-3">
        <dl className="grid gap-x-8 gap-y-3 sm:grid-cols-2">
          <dt className="text-xs text-muted-foreground">Captured</dt>
          <dd>
            <TimestampText iso={capturedIso} />
          </dd>
          <dt className="text-xs text-muted-foreground">Received</dt>
          <dd>
            <span className="flex flex-wrap items-baseline gap-2">
              <TimestampText iso={receivedIso} />
              {receivedIso !== null ? (
                <span className="tnum text-xs text-muted-foreground">
                  · updated {formatAge(ageSince(receivedIso, now))}
                </span>
              ) : null}
            </span>
          </dd>
          <dt className="text-xs text-muted-foreground">Sample interval</dt>
          <dd>
            {cadenceKnown ? (
              <span className="tnum text-sm">{formatIntervalMs(expected)}</span>
            ) : (
              <span className="text-sm text-muted-foreground">Unknown</span>
            )}
          </dd>
          <dt className="text-xs text-muted-foreground">Stale after</dt>
          <dd>
            <span className="tnum text-sm">
              {formatIntervalMs(thresholdMs)}
              {cadenceKnown ? null : <span className="text-muted-foreground"> (fallback)</span>}
            </span>
          </dd>
        </dl>
        <ClockSkewNote state={state} />
      </div>
    </Panel>
  );
}

function MachineUnavailablePanels({ machineId, machineCount }: { machineId: string; machineCount: number }) {
  if (machineCount === 0) {
    return (
      <Panel title="No machines" className="mt-8">
        <EmptyState
          icon={HardDrive}
          note="No machines are available in your authorized fleet."
        />
      </Panel>
    );
  }
  return (
    <Panel title="Machine not found" className="mt-8">
      <EmptyState
        icon={HardDrive}
        note="This machine is unavailable or outside your access."
      />
      <p className="tnum mt-2 text-center text-xs text-muted-foreground">
        Requested identifier: {machineId}
      </p>
      <div className="flex justify-center">
        <Link
          href="/"
          className="mt-2 inline-flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong"
        >
          <ArrowLeft className="h-3.5 w-3.5" aria-hidden />
          Back to fleet
        </Link>
      </div>
    </Panel>
  );
}

export function MachineView({ machineId }: { machineId: string }) {
  const { phase, machines } = useLive();
  // The query string is the explicit machine; resolution (and the
  // remembering that carries it to other views) belongs to the Machine
  // selection module.
  const { machine: routeMachine } = useMachineSelection("overview", machineId);
  const gate = useAccessGate();
  const now = useNow();

  const streamOk = phase === "live" || phase === "reconnecting" || phase === "disconnected";
  const machine = streamOk ? routeMachine : undefined;
  const held = phase === "reconnecting" || phase === "disconnected";
  const view = machine ? machineResourceView(machine, now) : null;

  const receivedAt = machine
    ? (machine.metrics?.receivedAt ?? machine.lastReceivedAt)
    : null;
  const ageLabel = receivedAt ? formatAge(ageSince(receivedAt, now)) : null;

  return (
    <AppShell
      title="Overview"
      machineId={machineId}
      machineView="overview"
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
          <StreamBanners />

          {machine && view ? (
            <>
              {view.tags.length > 0 ? (
                <ul className="mb-4 flex flex-wrap gap-1.5" aria-label="Machine tags">
                  {view.tags.map((tag) => <li key={tag} className="rounded-full border border-border px-2 py-0.5 text-xs text-muted-foreground">{tag}</li>)}
                </ul>
              ) : null}

              <section
                aria-label={`${view.displayName} live overview`}
                className="grid grid-cols-1 gap-5 xl:grid-cols-3"
              >
                <CpuResourcePanel
                  view={view}
                  ageLabel={ageLabel}
                  held={held}
                  className="xl:col-span-2"
                />
                <MemorySwapPanel view={view} ageLabel={ageLabel} held={held} />
                <div className="min-w-0 xl:col-span-3"><UsageHistory key={machineId} machine={machine} /></div>
                <GpuResourcePanel view={view} className="xl:col-span-3" />
                <DiskResourcePanel view={view} className="xl:col-span-2" />
                <NetworkResourcePanel view={view} />
              </section>
              <details className="mt-5 max-w-4xl text-xs text-muted-foreground">
                <summary className="inline-flex min-h-9 cursor-pointer items-center rounded-md hover:text-foreground">Collector details</summary>
                <div className="mt-3 grid gap-4 lg:grid-cols-2">
                  <CollectorPanel state={machine} />
                  <TimesPanel state={machine} now={now} />
                </div>
              </details>
            </>
          ) : streamOk ? (
            <MachineUnavailablePanels machineId={machineId} machineCount={machines.length} />
          ) : null}
        </>
      )}
    </AppShell>
  );
}
