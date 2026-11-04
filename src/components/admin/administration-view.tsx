"use client";

/**
 * Administration route composition and session gate.
 *
 * Authorization is deliberately server-authoritative: this page checks the
 * session's optional admin flag only to avoid rendering controls that would
 * fail, while every request is re-authorized by the hub. A wildcard machine
 * scope is never treated as administrative access.
 */

import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";
import { RefreshCw, ShieldAlert, ShieldCheck, UserRound, WifiOff } from "lucide-react";

import { createAdminApi } from "@/admin/api";
import type { AdminMachine } from "@/admin/presentation";
import { AppShell } from "@/components/shell/app-shell";
import { EmptyState, Panel } from "@/components/panel";
import { LiveStatusChip } from "@/components/live/live-status";
import { UserMenu } from "@/components/live/user-menu";
import { useLive } from "@/live/live-provider";
import { ADMIN_BUTTON_PRIMARY, ADMIN_BUTTON_SECONDARY } from "./admin-ui";
import { AuditLog } from "./audit-log";
import { CollectorAdmin } from "./collector-admin";
import { MachineMetadata } from "./machine-metadata";
import { UserDirectory } from "./user-directory";

export function AdministrationView() {
  const { session, machines, phase, retryConnection } = useLive();
  const api = useMemo(() => createAdminApi(), []);
  const [accessLost, setAccessLost] = useState(false);
  const [auditRevision, setAuditRevision] = useState(0);
  const sessionKey =
    session.kind === "signed-in"
      ? `${session.user.username}:${session.user.csrfToken}`
      : session.kind;

  useEffect(() => {
    setAccessLost(false);
  }, [sessionKey]);

  const onSessionLost = useCallback(() => setAccessLost(true), []);
  const onAuditChange = useCallback(() => setAuditRevision((value) => value + 1), []);

  const headerRight = (
    <>
      <LiveStatusChip />
      <UserMenu />
    </>
  );

  if (session.kind === "restoring") {
    return (
      <AppShell title="Administration" headerRight={headerRight}>
        <Panel title="Checking session" className="mx-auto my-12 max-w-md">
          <EmptyState
            icon={UserRound}
            note="Checking your session…"
          />
        </Panel>
      </AppShell>
    );
  }

  if (session.kind === "restore-failed") {
    return (
      <AppShell title="Administration" headerRight={headerRight}>
        <Panel title="Hub unreachable" className="mx-auto my-12 max-w-md">
          <EmptyState
            icon={WifiOff}
            note="The hub is unreachable right now."
          />
          <div className="flex justify-center">
            <button type="button" className={ADMIN_BUTTON_SECONDARY} onClick={retryConnection}>
              <RefreshCw className="h-3.5 w-3.5" aria-hidden />
              Try again
            </button>
          </div>
        </Panel>
      </AppShell>
    );
  }

  if (session.kind === "signed-out") {
    return (
      <AppShell title="Administration" headerRight={headerRight}>
        <Panel
          title={phase === "expired" ? "Session ended" : "Sign in required"}
          className="mx-auto my-12 max-w-md"
        >
          <EmptyState
            icon={UserRound}
            note={phase === "expired" ? "Your session ended." : "Sign in required."}
          />
          <div className="flex justify-center">
            <Link href="/login" className={ADMIN_BUTTON_PRIMARY}>
              Sign in
            </Link>
          </div>
        </Panel>
      </AppShell>
    );
  }

  if (session.kind === "auth-unavailable") {
    return (
      <AppShell title="Administration" headerRight={headerRight}>
        <Panel title="Sign-in unavailable" className="mx-auto my-12 max-w-md">
          <EmptyState
            icon={ShieldAlert}
            note="Browser sign-in is not configured on this hub."
          />
        </Panel>
      </AppShell>
    );
  }

  if (phase === "expired") {
    return (
      <AppShell title="Administration" headerRight={headerRight}>
        <Panel title="Session ended" className="mx-auto my-12 max-w-md">
          <EmptyState
            icon={ShieldAlert}
            note="Your session ended."
          />
          <div className="flex justify-center">
            <Link href="/login" className={ADMIN_BUTTON_PRIMARY}>
              Sign in
            </Link>
          </div>
        </Panel>
      </AppShell>
    );
  }

  if (session.user.admin !== true) {
    return (
      <AppShell title="Administration" headerRight={headerRight}>
        <Panel title="Administrator access required" className="mx-auto my-12 max-w-lg">
          <EmptyState
            icon={ShieldAlert}
            note={`You are signed in as ${session.user.username}, but this account is not an administrator.`}
          />
          <div className="flex justify-center">
            <Link href="/" className={ADMIN_BUTTON_SECONDARY}>
              Back to fleet
            </Link>
          </div>
        </Panel>
      </AppShell>
    );
  }

  if (accessLost) {
    return (
      <AppShell title="Administration" headerRight={headerRight}>
        <Panel title="Session ended" className="mx-auto my-12 max-w-lg">
          <EmptyState
            icon={ShieldAlert}
            note="Your administrative session is no longer valid."
          />
          <div className="flex justify-center">
            <Link href="/login" className={ADMIN_BUTTON_PRIMARY}>
              Sign in again
            </Link>
          </div>
        </Panel>
      </AppShell>
    );
  }

  const adminMachines: AdminMachine[] = machines.map((machine) => ({
    machineId: machine.machineId,
    hostname: machine.hostname,
    displayName: machine.displayName,
    tags: machine.tags,
  }));
  const machinesPending = phase === "connecting" || phase === "idle";

  return (
    <AppShell title="Administration" headerRight={headerRight}>
      <div className="flex min-w-0 flex-col gap-5">
        <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
          <span className="inline-flex min-w-0 max-w-full flex-wrap items-center gap-1.5 rounded-full bg-selected px-3 py-1 text-xs font-medium text-foreground">
            <ShieldCheck className="h-3.5 w-3.5 shrink-0 text-accent" aria-hidden />
            <span className="min-w-0 break-all">Administrator · {session.user.username}</span>
          </span>
        </div>

        <div className="grid min-w-0 gap-5 xl:grid-cols-12">
          <div className="min-w-0 xl:col-span-7">
            <UserDirectory
              api={api}
              username={session.user.username}
              csrfToken={session.user.csrfToken}
              machines={adminMachines}
              machinesPending={machinesPending}
              onAuditChange={onAuditChange}
              onSessionLost={onSessionLost}
            />
          </div>
          <div className="min-w-0 xl:col-span-5">
            <MachineMetadata
              api={api}
              csrfToken={session.user.csrfToken}
              machines={adminMachines}
              machinesPending={machinesPending}
              onSessionLost={onSessionLost}
            />
          </div>
          <div id="collectors" className="min-w-0 scroll-mt-6 xl:col-span-12">
            <CollectorAdmin
              api={api}
              csrfToken={session.user.csrfToken}
              sessionKey={sessionKey}
              machines={adminMachines}
              onAuditChange={onAuditChange}
              onSessionLost={onSessionLost}
            />
          </div>
          <div className="min-w-0 xl:col-span-12">
            <AuditLog api={api} revision={auditRevision} onSessionLost={onSessionLost} />
          </div>
        </div>
      </div>
    </AppShell>
  );
}
