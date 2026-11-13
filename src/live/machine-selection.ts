"use client";

/**
 * Deep Machine selection module: one ownership point for route/Fleet/
 * remembered-choice resolution and selection navigation+persistence.
 *
 * Selection rules owned here:
 * - An explicit route Machine (`?machineId=`) decides: when it names an
 *   authorized Machine it is selected; when it does not, the view receives
 *   the missing/unknown outcome and never silently selects another Machine.
 * - Without an explicit Machine the selection follows the viewer: the
 *   remembered choice while it is still authorized, otherwise a
 *   view-specific fallback (Processes prefers a process-capable Machine).
 * - The resolved selection is remembered (tab-scoped) so it follows the
 *   viewer between overview, history and processes, and History keeps it
 *   in the URL so the page stays linkable. Fleet loading and later
 *   updates never overwrite an explicit Machine, which also prevents
 *   navigation loops; back/forward keep working because the URL is
 *   re-resolved on every render.
 *
 * Browser seams stay here too: next/navigation owns routing and
 * usePersistentState owns storage, without inventing generic adapters.
 */

import { useCallback, useEffect, useMemo } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { usePersistentState } from "@/lib/persistent-state";
import { useLive } from "@/live/live-provider";
import type { MachineState } from "@/contracts";

export type MachineViewKind = "overview" | "history" | "processes";

/**
 * Routing shape: every machine view is a real exported page selected by
 * `?machineId=`. The legacy `/machines/<id>` path is redirected to this form
 * by the hub, so links never force a full document reload.
 */
export function machineViewHref(machineId: string, view: MachineViewKind): string {
  const id = encodeURIComponent(machineId);
  return view === "overview" ? `/machines?machineId=${id}` : `/${view}?machineId=${id}`;
}

/**
 * URL for a query-parameter view with the resolved machine persisted for
 * linkability; null when an explicit parameter already governs the URL,
 * so an explicit Machine is never silently rewritten.
 */
export function machineUrlWithId(pathname: string, searchParams: URLSearchParams, machineId: string): string | null {
  if (searchParams.get("machineId")) return null;
  const params = new URLSearchParams(searchParams.toString());
  params.set("machineId", machineId);
  return `${pathname}?${params.toString()}`;
}

/** Serves the Processes fallback; an unreported capability stays usable (honest default). */
function processCapable(machine: MachineState): boolean {
  const capability = machine.inventory?.capabilities?.processes;
  return capability === undefined || capability.status === "supported";
}

/**
 * One Machine resolution. `via` records how the explicit/remembered/fallback
 * precedence resolved so the callers can reason honestly about control.
 */
export type MachineResolution =
  | { kind: "resolved"; machine: MachineState; via: "requested" | "remembered" | "fallback" }
  | { kind: "missing"; requestedMachineId: string }
  | { kind: "fleet-empty" };

/** View-specific fallback: the first machine, with Processes preferring capability. */
const FALLBACK_MACHINES: Record<MachineViewKind, (machines: readonly MachineState[]) => MachineState | undefined> = {
  overview: (machines) => machines[0],
  history: (machines) => machines[0],
  processes: (machines) => machines.find(processCapable) ?? machines[0],
};

/**
 * One resolution point for route/remembered/Fleet precedence and the
 * view-specific fallback. `requestedMachineId` null or "" means the route
 * names no machine; an explicit unknown/unauthorized id resolves to
 * "missing" and never falls back; a remembered id that is not in the
 * authorized fleet degrades to the view fallback.
 */
export function resolveMachineSelection({
  view,
  requestedMachineId,
  rememberedMachineId,
  machines,
}: {
  view: MachineViewKind;
  requestedMachineId: string | null;
  rememberedMachineId: string;
  machines: readonly MachineState[];
}): MachineResolution {
  if (machines.length === 0) return { kind: "fleet-empty" };
  if (requestedMachineId) {
    const machine = machines.find((item) => item.machineId === requestedMachineId);
    if (machine) return { kind: "resolved", machine, via: "requested" };
    return { kind: "missing", requestedMachineId };
  }
  if (rememberedMachineId) {
    const machine = machines.find((item) => item.machineId === rememberedMachineId);
    if (machine) return { kind: "resolved", machine, via: "remembered" };
  }
  const machine = FALLBACK_MACHINES[view](machines);
  return machine
    ? { kind: "resolved", machine, via: "fallback" }
    : { kind: "fleet-empty" };
}

const parseMachineId = (raw: unknown): string | null => (typeof raw === "string" ? raw : null);

/**
 * Remembered machine choice following the viewer between views; lasts for
 * the browser tab. Unknown/not-authorized ids are filtered by resolution,
 * so stale choices degrade honestly to the view fallback.
 */
export function useRememberedMachine(): [string, (next: string) => void] {
  return usePersistentState("machine.selected", "", parseMachineId, "session");
}

/** Only query-parameter views with an owned, linkable URL write it back. */
const URL_SYNCED_VIEWS: ReadonlySet<MachineViewKind> = new Set(["history"]);

export type MachineSelection = {
  resolution: MachineResolution;
  /** The selected machine for view content; undefined while missing/fleet-empty. */
  machine: MachineState | undefined;
  /** The selected machine id, "" when nothing is selected (no query scope). */
  machineId: string;
  /**
   * Machine identifier the shared shell/breadcrumb renders for this view:
   * the requested id for overview (it stays in the URL while a machine is
   * missing), the resolved selection for history, and the requested id while
   * Processes shows a missing machine.
   */
  navMachineId: string;
  /** Explicit route identifier; null when the route names no machine. */
  requestedMachineId: string | null;
};

/**
 * Resolve one view's selected Machine from route precedence, the authorized
 * Fleet and the remembered choice — then coordinate persistence: the URL
 * keeps the selection linkable (History only, never over an explicit
 * parameter) and the resolved selection is remembered for other views.
 */
export function useMachineSelection(view: "overview", routeMachineId: string): MachineSelection;
export function useMachineSelection(view: "history" | "processes"): MachineSelection;
export function useMachineSelection(view: MachineViewKind, routeMachineId?: string): MachineSelection {
  const { session, machines } = useLive();
  const router = useRouter();
  const pathname = usePathname() || `/${view}`;
  const searchParams = useSearchParams();
  const [rememberedMachineId, remember] = useRememberedMachine();

  const requestedMachineId: string | null =
    view === "overview"
      ? routeMachineId ?? ""
      : searchParams.get("machineId") || null;

  const resolution = useMemo(
    () => resolveMachineSelection({ view, requestedMachineId, rememberedMachineId, machines }),
    [view, requestedMachineId, rememberedMachineId, machines],
  );
  const machine = resolution.kind === "resolved" ? resolution.machine : undefined;
  const machineId = machine?.machineId ?? "";
  const navMachineId =
    view === "overview"
      ? requestedMachineId ?? ""
      : view === "processes"
        ? machine?.machineId ?? requestedMachineId ?? ""
        : machineId;

  // Only an actual resolved selection is remembered, so a missing-machine
  // visit cannot discard the viewer's last good choice; unknown ids are
  // always filtered at resolution time, so stale state cannot resurrect.
  useEffect(() => {
    if (machineId) remember(machineId);
  }, [machineId, remember]);

  const shouldSyncUrl = URL_SYNCED_VIEWS.has(view);
  useEffect(() => {
    if (!shouldSyncUrl) return;
    if (session.kind !== "signed-in" || machines.length === 0) return;
    if (requestedMachineId !== null) return;
    if (machineId === "") return;
    const nextUrl = machineUrlWithId(pathname, searchParams, machineId);
    if (nextUrl) router.replace(nextUrl, { scroll: false });
  }, [shouldSyncUrl, session.kind, machines, requestedMachineId, machineId, searchParams, router, pathname]);

  return { resolution, machine, machineId, navMachineId, requestedMachineId };
}

/**
 * Switching the selected Machine: remember it and route in the target view.
 * Every selection control (sidebar, mobile breadcrumb) uses this so
 * storage, URL and all views agree.
 */
export function useSelectMachine(): (machineId: string, view: MachineViewKind) => void {
  const router = useRouter();
  const [, remember] = useRememberedMachine();
  return useCallback(
    (machineId: string, view: MachineViewKind) => {
      remember(machineId);
      router.push(machineViewHref(machineId, view));
    },
    [remember, router],
  );
}

/** Keep navigation usable even when a view's requested Machine is missing. */
export function useMachineNavigation(machineId?: string, suppliedView?: MachineViewKind) {
  const pathname = usePathname() || "/";
  const { machines } = useLive();
  const [rememberedMachineId] = useRememberedMachine();
  const select = useSelectMachine();
  const view: MachineViewKind | null = suppliedView ?? (pathname === "/machines" || pathname.startsWith("/machines/")
    ? "overview" : pathname === "/history" ? "history" : pathname === "/processes" ? "processes" : null);
  // The view passes its selection; the shell needs no query subscription or
  // hydration boundary. A missing view retains navigation to authorized Machines.
  const requestedMachineId = machines.some(machine => machine.machineId === machineId) ? machineId! : null;
  const resolution = resolveMachineSelection({ view: view ?? "overview", requestedMachineId, rememberedMachineId, machines });
  return {
    view,
    machine: resolution.kind === "resolved" ? resolution.machine : undefined,
    selectMachine: (machineId: string) => select(machineId, view ?? "overview"),
  };
}
