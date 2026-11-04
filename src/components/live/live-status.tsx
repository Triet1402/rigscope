"use client";

/**
 * Connection-state chips for the header and the sidebar footer. Both read
 * the shared provider, so there is one stream per tab and one truth for
 * "is this live?". Status words never rely on color alone; each chip is a
 * labeled status region.
 */

import type { SessionState, StreamPhase } from "@/live/types";

import { useLive } from "@/live/live-provider";

type StatusView = {
  label: string;
  tone: "ok" | "warn" | "crit" | "muted";
  /** Extra context for the title/tooltip. */
  detail?: string;
};

const TONE_DOT: Record<StatusView["tone"], string> = {  ok: "bg-ok",
  warn: "bg-warn",
  crit: "bg-crit",
  muted: "bg-muted-foreground/50",
};

export function connectionSummary(
  session: SessionState,
  phase: StreamPhase,
): StatusView {
  switch (session.kind) {
    case "restoring":
      return { label: "Checking session…", tone: "muted" };
    case "restore-failed":
      return { label: "Hub unreachable", tone: "crit" };
    case "signed-out":
      return { label: "Signed out", tone: "muted" };
    case "auth-unavailable":
      return { label: "Sign-in unavailable", tone: "warn" };
    case "signed-in":
      break;
  }
  switch (phase) {
    case "idle":
      return { label: "Not connected", tone: "muted" };
    case "connecting":
      return { label: "Connecting…", tone: "warn" };
    case "live":
      return { label: "Live", tone: "ok" };
    case "reconnecting":
      return { label: "Reconnecting…", tone: "warn" };
    case "disconnected":
      return { label: "Disconnected", tone: "crit" };
    case "expired":
      return { label: "Session ended", tone: "crit" };
    case "no-permission":
      return { label: "No permission", tone: "warn" };
  }
}

function StatusDot({ tone }: { tone: StatusView["tone"] }) {
  return (
    <span className={`inline-block h-2 w-2 shrink-0 rounded-full ${TONE_DOT[tone]}`} aria-hidden />
  );
}

const RESET_NOTE = "Stream resynchronized with a fresh snapshot.";

/** The quiet status chip that lives in the page header. */
export function LiveStatusChip() {
  const { session, phase, lastEventKind, notice, dismissNotice } = useLive();
  const status = connectionSummary(session, phase);
  const resetNote =
    lastEventKind === "reset" && phase === "live" ? RESET_NOTE : null;

  return (
    <div
      role="status"
      aria-label={`Live feed status: ${status.label}`}
      className="flex min-w-0 items-center gap-2"
    >
      <StatusDot tone={status.tone} />
      <span className="tnum truncate text-xs text-muted-foreground">{status.label}</span>
      {resetNote ? (
        <span
          className="tnum hidden truncate text-xs text-muted-foreground lg:inline"
          title={resetNote}
        >
          · {resetNote}
        </span>
      ) : null}
      {notice ? (
        <button
          type="button"
          onClick={dismissNotice}
          className="max-w-[16rem] truncate rounded-md border border-border px-2 py-0.5 text-xs text-foreground transition-colors hover:bg-card-strong"
          title={`${notice} Click to dismiss.`}
        >
          {notice}
          <span aria-hidden> ✕</span>
        </button>
      ) : null}
    </div>
  );
}

/** Sidebar footer line — replaces the scaffold "live feed offline" copy. */
export function SidebarLiveStatus() {
  const { session, phase } = useLive();
  const status = connectionSummary(session, phase);
  return (
    <div className="flex items-center gap-2 px-3 text-xs">
      <StatusDot tone={status.tone} />
      <span className="truncate" role="status" aria-label={`Live feed status: ${status.label}`}>
        {status.label}
      </span>
    </div>
  );
}
