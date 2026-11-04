"use client";

/**
 * Processes page shell: the shared live session/fleet scope selects one
 * machine (honoring ?machineId=), while the detail panel owns its own
 * request-scoped process lease. The live stream status and the process
 * status stay separate truths.
 */

import Link from "next/link";
import { CloudOff, Hourglass, Server } from "lucide-react";

import { AppShell } from "@/components/shell/app-shell";
import { EmptyState, Panel } from "@/components/panel";
import { LiveStatusChip } from "@/components/live/live-status";
import { UserMenu } from "@/components/live/user-menu";
import { useAccessGate } from "@/components/live/stream-banners";
import { useLive } from "@/live/live-provider";
import { useMachineSelection } from "@/live/machine-selection";
import type { MachineState } from "@/contracts";
import { ProcessDetailPanel } from "./process-detail-panel";
import { useProcessesPaused } from "./process-prefs";

function machineLabel(machine: MachineState): string {
  return machine.displayName || machine.hostname || machine.machineId;
}

function NoMachines({ waiting }: { waiting: boolean }) {
  return (
    <Panel title={waiting ? "Waiting for machines" : "No machines"} className="mt-2">
      <EmptyState
        icon={waiting ? Hourglass : CloudOff}
        note={waiting ? "Waiting for the fleet snapshot…" : "No machines are available in your authorized fleet."}
      />
    </Panel>
  );
}

function MissingMachine({ requested }: { requested: string }) {
  return (
    <Panel title="Machine not found" className="mt-2">
      <EmptyState
        icon={Server}
        note="This machine is unavailable or outside your access."
      />
      <p className="tnum mt-2 break-all text-center text-xs text-muted-foreground">
        Requested identifier: {requested}
      </p>
      <div className="flex justify-center">
        <Link
          href="/"
          className="mt-2 inline-flex items-center gap-1.5 rounded-md border border-border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong"
        >
          Back to fleet
        </Link>
      </div>
    </Panel>
  );
}

export function ProcessesView() {
  const { session, phase, machines } = useLive();
  const gate = useAccessGate();
  const [paused, setPaused] = useProcessesPaused();
  // Route/remembered/Fleet resolution — including the process-capable
  // fallback — lives in the Machine selection module; an explicit unknown
  // machine stays missing and keeps its breadcrumb identifier.
  const { machine: selected, navMachineId, requestedMachineId } = useMachineSelection("processes");
  const csrfToken = session.kind === "signed-in" ? session.user.csrfToken : null;

  const waiting = machines.length === 0 && (phase === "connecting" || phase === "idle");

  return (
    <AppShell
      title="Processes"
      machineId={navMachineId}
      machineView="processes"
      workspace
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
          {machines.length === 0 ? (
            <NoMachines waiting={waiting} />
          ) : selected ? (
            <ProcessDetailPanel
              key={`${selected.machineId}:${csrfToken}`}
              machine={selected}
              csrfToken={csrfToken}
              paused={paused}
              onPausedChange={setPaused}
            />
          ) : (
            <MissingMachine requested={requestedMachineId ?? ""} />
          )}
        </>
      )}
    </AppShell>
  );
}
