"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { RefreshCw, ScrollText } from "lucide-react";

import type { AdminApi, AuditEvent } from "@/admin/api";
import { formatAdminTimestamp, formatAuditAction } from "@/admin/presentation";
import { EmptyState, Panel } from "@/components/panel";
import {
  ADMIN_BUTTON_QUIET,
  InlineNotice,
  friendlyError,
  isAbort,
  isForbidden,
  isUnauthorized,
} from "./admin-ui";

export function AuditLog(props: {
  api: AdminApi;
  revision: number;
  onSessionLost: () => void;
}) {
  const { api, revision, onSessionLost } = props;
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoading(true);
    setError(null);
    try {
      const next = await api.listAudit(50, controller.signal);
      if (!controller.signal.aborted) setEvents(next);
    } catch (caught) {
      if (controller.signal.aborted || isAbort(caught)) return;
      if (isUnauthorized(caught)) {
        onSessionLost();
        return;
      }
      setError(
        isForbidden(caught)
          ? "Administrator access is required to read the audit log."
          : friendlyError(caught, "Could not load the audit log."),
      );
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, [api, onSessionLost]);

  useEffect(() => {
    void load();
    return () => abortRef.current?.abort();
  }, [load, revision]);

  return (
    <Panel
      title="Audit log"
      className="min-w-0"
      action={
        <button type="button" className={ADMIN_BUTTON_QUIET} onClick={() => void load()} disabled={loading}>
          <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} aria-hidden />
          Refresh
        </button>
      }
    >
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="tnum text-xs text-muted-foreground">
          {events.length} event{events.length === 1 ? "" : "s"}
        </span>
      </div>

      {error ? (
        <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
          <InlineNotice tone="error" role="alert">
            {error}
          </InlineNotice>
          <button type="button" className={ADMIN_BUTTON_QUIET} onClick={() => void load()}>
            Try again
          </button>
        </div>
      ) : null}

      {loading && events.length === 0 && !error ? (
        <p role="status" className="py-6 text-sm text-muted-foreground">
          Loading audit events…
        </p>
      ) : null}

      {!loading && !error && events.length === 0 ? (
        <EmptyState
          icon={ScrollText}
          note="No administrative changes recorded yet."
        />
      ) : null}

      {events.length > 0 ? (
        <ol className="mt-2 max-h-96 divide-y divide-border overflow-y-auto border-t border-border">
          {events.map((event, index) => (
            <li
              key={`${event.at}-${event.actor}-${event.action}-${event.target}-${index}`}
              className="flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-1 py-2.5 text-xs"
            >
              <time dateTime={event.at} className="tnum shrink-0 text-muted-foreground">
                {formatAdminTimestamp(event.at)}
              </time>
              <span className="font-medium text-foreground">{formatAuditAction(event.action)}</span>
              <span className="min-w-0 break-all text-muted-foreground">
                {event.target} <span aria-hidden>·</span> by {event.actor}
              </span>
            </li>
          ))}
        </ol>
      ) : null}
    </Panel>
  );
}
