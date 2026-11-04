"use client";

/**
 * Shared panels for everything that gates the live data: session states,
 * stream phases, and transient banners. Both the fleet and machine views
 * render the same honest vocabulary — never a fake gauge while the
 * connection is down.
 */

import Link from "next/link";
import { AlertTriangle, Clock, RefreshCw, ShieldAlert, UserRound, WifiOff } from "lucide-react";

import { EmptyState, Panel } from "@/components/panel";
import { useLive } from "@/live/live-provider";
import type { ReactNode } from "react";

function ActionButton(props: { onClick: () => void; children: ReactNode }) {
  return (
    <button
      type="button"
      onClick={props.onClick}
      className="inline-flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong"
    >
      <RefreshCw className="h-3.5 w-3.5" aria-hidden />
      {props.children}
    </button>
  );
}

function ActionLink(props: { href: string; children: ReactNode; primary?: boolean }) {
  return (
    <Link
      href={props.href}
      className={
        props.primary
          ? "inline-flex items-center gap-1.5 rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-accent-foreground transition-colors hover:bg-accent/90"
          : "inline-flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-card-strong"
      }
    >
      {props.children}
    </Link>
  );
}

/**
 * Hook gate: returns the panel for every state in which live data should
 * NOT be rendered, or null once the viewer is authorized and the stream
 * allows data. Call at the top level of the consuming component.
 */
export function useAccessGate(): ReactNode {
  const { session, phase, retryConnection } = useLive();

  switch (session.kind) {
    case "restoring":
      return (
        <Panel title="Checking session" className="mx-auto my-12 max-w-md">
          <EmptyState
            icon={UserRound}
            note="Checking your session…"
          />
        </Panel>
      );
    case "restore-failed":
      return (
        <Panel title="Hub unreachable" className="mx-auto my-12 max-w-md">
          <EmptyState
            icon={WifiOff}
            note="The hub is unreachable right now."
          />
          <div className="flex justify-center">
            <ActionButton onClick={retryConnection}>Check again</ActionButton>
          </div>
        </Panel>
      );
    case "signed-out":
      if (phase === "expired") {
        return (
          <Panel title="Session ended" className="mx-auto my-12 max-w-md">
            <EmptyState
              icon={Clock}
              note="Your session ended."
            />
            <div className="flex justify-center">
              <ActionLink href="/login" primary>
                Sign in
              </ActionLink>
            </div>
          </Panel>
        );
      }
      return (
        <Panel title="Sign in required" className="mx-auto my-12 max-w-md">
          <EmptyState
            icon={UserRound}
            note="Sign in required."
          />
          <div className="flex justify-center">
            <ActionLink href="/login" primary>
              Sign in
            </ActionLink>
          </div>
        </Panel>
      );
    case "auth-unavailable":
      return (
        <Panel title="Sign-in unavailable" className="mx-auto my-12 max-w-md">
          <EmptyState
            icon={ShieldAlert}
            note="Browser sign-in is not configured on this hub."
          />
        </Panel>
      );
    case "signed-in":
      break;
  }

  if (phase === "expired") {
    return (
      <Panel title="Session ended" className="mx-auto my-12 max-w-md">
        <EmptyState
          icon={Clock}
          note="Your session ended."
        />
        <div className="flex justify-center">
          <ActionLink href="/login" primary>
            Sign in
          </ActionLink>
        </div>
      </Panel>
    );
  }

  if (phase === "no-permission") {
    return (
      <Panel title="No permission" className="mx-auto my-12 max-w-md">
        <EmptyState
          icon={ShieldAlert}
          note={`You are signed in as ${session.user.username}, but this hub grants no machine view to this account.`}
        />
        <div className="flex justify-center">
          <ActionButton onClick={retryConnection}>Check again</ActionButton>
        </div>
      </Panel>
    );
  }

  return null;
}

/** Transient banners shown ABOVE healthy data (refreshing, held values). */
export function StreamBanners() {
  const { phase, retryConnection } = useLive();

  if (phase !== "reconnecting" && phase !== "disconnected" && phase !== "connecting") {
    return null;
  }

  if (phase === "connecting") {
    return (
      <div
        role="status"
        className="mb-5 flex items-center gap-2 rounded-lg border border-border bg-card px-4 py-2.5 text-xs text-muted-foreground"
      >
        <RefreshCw className="h-3.5 w-3.5 shrink-0" aria-hidden />
        Connecting to the live feed — waiting for the first snapshot.
      </div>
    );
  }

  return (
    <div
      role="status"
      className="mb-5 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-border bg-card px-4 py-2.5 text-xs text-muted-foreground"
    >
      <AlertTriangle className="h-3.5 w-3.5 shrink-0 text-warn" aria-hidden />
      <span>
        Connection lost — values below are <strong className="font-medium text-foreground">last known</strong>, not live measurements
        {phase === "reconnecting" ? " — retrying with backoff…" : " — retries stopped."}
      </span>
      {phase === "disconnected" ? <ActionButton onClick={retryConnection}>Retry now</ActionButton> : null}
    </div>
  );
}
