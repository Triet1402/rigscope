"use client";

import Link from "next/link";
import { useLive } from "@/live/live-provider";
import { machineViewHref, useSelectMachine, type MachineViewKind } from "@/live/machine-selection";

/** Breadcrumbs express the fleet → machine → view hierarchy without a second tab bar. */
export function MachineBreadcrumb({ machineId, title, view }: { machineId: string; title: string; view: MachineViewKind }) {
  const { machines } = useLive();
  const selectMachine = useSelectMachine();
  const machine = machines.find((item) => item.machineId === machineId);
  const name = machine?.displayName || machine?.hostname || "Machine";
  return (
    <nav aria-label="Breadcrumb" className="flex min-w-0 items-center gap-2 text-sm">
      <Link href="/" className="hidden text-muted-foreground hover:text-foreground sm:inline">Fleet</Link>
      <span aria-hidden className="hidden text-muted-foreground sm:inline">/</span>
      <Link href={machineViewHref(machineId, "overview")} className="hidden max-w-48 truncate text-muted-foreground hover:text-foreground md:inline">{name}</Link>
      <select aria-label="Machine" value={machineId} onChange={(event) => selectMachine(event.target.value, view)} className="h-8 min-w-0 max-w-32 rounded-md border border-border bg-card px-1.5 text-sm md:hidden">
        {machines.map((item) => <option key={item.machineId} value={item.machineId}>{item.displayName || item.hostname || item.machineId}</option>)}
      </select>
      <span aria-hidden className="text-muted-foreground">/</span>
      <h1 className="truncate text-lg font-semibold tracking-tight">{title}</h1>
    </nav>
  );
}
