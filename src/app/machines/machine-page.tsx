"use client";

/**
 * Machine overview page body.
 *
 * Production is a static export, so the selected machine comes from the
 * query string (`/machines?machineId=<id>`), exactly like History and
 * Processes. That makes the route a real exported document: client
 * navigation from Fleet keeps the document (and therefore `LiveProvider`'s
 * sample history and SSE stream) alive instead of forcing a reload, and
 * identifiers containing dots are ordinary query values.
 */

import { useSearchParams } from "next/navigation";

import { AppShell } from "@/components/shell/app-shell";
import { LiveStatusChip } from "@/components/live/live-status";
import { UserMenu } from "@/components/live/user-menu";
import { MachineView } from "@/components/live/machine-view";

export function MachinePageView() {
  const searchParams = useSearchParams();
  return <MachineView machineId={searchParams.get("machineId") ?? ""} />;
}

/**
 * Prerender fallback for the Suspense boundary `useSearchParams` needs during
 * static export. It shows the app frame without a machine-specific value, so
 * the exported HTML never depends on a request.
 */
export function MachinePageFallback() {
  return (
    <AppShell
      title="Overview"
      machineView="overview"
      headerRight={
        <>
          <LiveStatusChip />
          <UserMenu />
        </>
      }
    >
      <p className="text-sm text-muted-foreground">Loading machine…</p>
    </AppShell>
  );
}
