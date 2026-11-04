"use client";

/**
 * Browser-side lifecycle for one Processes viewer.
 *
 * One mounted viewer owns one random viewer id per acquisition. It acquires
 * (POST), renews every five seconds while active, polls GET at the hub's
 * advertised interval, and releases with a keepalive DELETE on pause, tab
 * hide, machine switch, unmount, or session loss. Polling is sequential so a
 * slow request cannot overlap, and an AbortController invalidates responses
 * from a previous machine or viewer.
 *
 * The last snapshot survives navigation and refresh failures, labeled stale
 * until collection resumes. The cache belongs to one browser login. A denied,
 * withheld or unsupported state clears it.
 */

import { useCallback, useEffect, useRef, useState } from "react";

import type { DetailResult } from "@/contracts";
import { ProcessDetailCache } from "@/processes/detail-cache";
import {
  detailStateToPhase,
  parseAcquireResponse,
  parseDetailPollResponse,
  phasePresentation,
  type ProcessDetailPhase,
} from "@/processes/presentation";

const DEFAULT_INTERVAL_MS = 1_000;
const DEFAULT_LEASE_MS = 15_000;
const RENEW_MS = 5_000;

export type ProcessDetailState = {
  phase: ProcessDetailPhase;
  detail: DetailResult | null;
  message: string | null;
  intervalMs: number;
  leaseMs: number;
  /** Local browser receipt time of the last successful snapshot. */
  lastReceivedMs: number | null;
};

export type UseProcessDetailResult = {
  state: ProcessDetailState;
  /** True while the document is hidden and updates are intentionally stopped. */
  hidden: boolean;
  /** Drop the current viewer, acquire a fresh one, and reload. */
  refresh: () => void;
};

export function useProcessDetail(options: {
  machineId: string | null;
  csrfToken: string | null;
  paused: boolean;
}): UseProcessDetailResult {
  const { machineId, csrfToken, paused } = options;
  const [hidden, setHidden] = useState(
    () => typeof document !== "undefined" && document.hidden,
  );
  const [refreshNonce, setRefreshNonce] = useState(0);
  const [state, setState] = useState<ProcessDetailState>(() => initialState(machineId, csrfToken));
  const stateRef = useRef(state);
  const scopeRef = useRef({ machineId, csrfToken });
  const nonceRef = useRef(0);

  const refresh = useCallback(() => {
    nonceRef.current += 1;
    setRefreshNonce(nonceRef.current);
  }, []);

  useEffect(() => {
    if (typeof document === "undefined") return;
    const update = (): void => setHidden(document.hidden);
    update();
    document.addEventListener("visibilitychange", update);
    return () => document.removeEventListener("visibilitychange", update);
  }, []);

  useEffect(() => {
    if (scopeRef.current.machineId !== machineId || scopeRef.current.csrfToken !== csrfToken) {
      scopeRef.current = { machineId, csrfToken };
      const next = initialState(machineId, csrfToken);
      stateRef.current = next;
      setState(next);
    }
    const active = machineId !== null && csrfToken !== null && !paused && !hidden;
    if (!active) {
      if (!machineId || !csrfToken) {
        const next = initialState(machineId, csrfToken);
        stateRef.current = next;
        setState(next);
      } else {
        const next: ProcessDetailState = {
          ...stateRef.current,
          phase: "paused",
          message: hidden
            ? "This tab is hidden — process updates are stopped until it is visible again."
            : "Paused — process updates are stopped until you resume.",
        };
        stateRef.current = next;
        setState(next);
      }
      return;
    }

    let disposed = false;
    const abort = new AbortController();
    const viewerId = newViewerId();
    const baseUrl = `/api/processes?machineId=${encodeURIComponent(machineId)}&viewerId=${encodeURIComponent(viewerId)}`;
    let intervalMs = stateRef.current.intervalMs || DEFAULT_INTERVAL_MS;
    let leaseMs = stateRef.current.leaseMs || DEFAULT_LEASE_MS;
    let localHasSnapshot = stateRef.current.detail !== null;
    let polling = false;
    let interestActive = false;
    let finished = false;
    let pollTimer: ReturnType<typeof setTimeout> | null = null;
    let renewTimer: ReturnType<typeof setTimeout> | null = null;
    let pollAbort: AbortController | null = null;

    const apply = (
      patch: Partial<ProcessDetailState> | ((previous: ProcessDetailState) => Partial<ProcessDetailState>),
    ): void => {
      if (disposed) return;
      const previous = stateRef.current;
      const delta = typeof patch === "function" ? patch(previous) : patch;
      const next = { ...previous, ...delta };
      stateRef.current = next;
      if (next.detail !== null && next.lastReceivedMs !== null) {
        SNAPSHOT_CACHE.set(machineId, csrfToken, {
          detail: next.detail,
          intervalMs: next.intervalMs,
          leaseMs: next.leaseMs,
          lastReceivedMs: next.lastReceivedMs,
        });
      }
      setState(next);
    };

    const clearTimers = (): void => {
      if (pollTimer !== null) {
        clearTimeout(pollTimer);
        pollTimer = null;
      }
      if (renewTimer !== null) {
        clearTimeout(renewTimer);
        renewTimer = null;
      }
      pollAbort?.abort();
      pollAbort = null;
    };

    const release = (): void => {
      try {
        void fetch(baseUrl, {
          method: "DELETE",
          cache: "no-store",
          keepalive: true,
          headers: { "X-CSRF-Token": csrfToken, Accept: "application/json" },
        }).catch(() => undefined);
      } catch {
        // Cleanup is best-effort; the server lease also expires on its own.
      }
    };

    const deactivate = (): void => {
      interestActive = false;
      if (pollTimer !== null) {
        clearTimeout(pollTimer);
        pollTimer = null;
      }
    };

    const terminal = (phase: ProcessDetailPhase, message: string): void => {
      finished = true;
      clearTimers();
      localHasSnapshot = false;
      SNAPSHOT_CACHE.delete(machineId, csrfToken);
      apply({ phase, detail: null, message, lastReceivedMs: null });
      release();
    };

    const refreshFailure = (message: string): void => {
      const offline = typeof navigator !== "undefined" && navigator.onLine === false;
      apply({
        phase: localHasSnapshot ? (offline ? "disconnected" : "stale") : "error",
        message,
      });
    };

    const acquire = async (): Promise<boolean> => {
      try {
        const res = await fetch(baseUrl, {
          method: "POST",
          cache: "no-store",
          signal: abort.signal,
          headers: { "X-CSRF-Token": csrfToken, Accept: "application/json" },
        });
        if (disposed || abort.signal.aborted) return false;
        if (!res.ok) {
          deactivate();
          const detail = await responseErrorDetail(res);
          if (res.status === 401) {
            terminal("error", "Your session ended. Sign in again to inspect processes.");
          } else if (res.status === 403) {
            terminal("withheld", detail ?? "Processes are not available to this hub.");
          } else if (res.status === 409) {
            terminal("unsupported", detail ?? "This collector requires an upgrade before Processes is available.");
          } else if ((res.status === 429 || res.status === 503) && /budget/i.test(detail ?? "")) {
            apply({ phase: "busy", message: detail ?? "Too many process views are open for this hub." });
          } else {
            refreshFailure(detail ?? `Could not request process detail (HTTP ${res.status}).`);
          }
          return false;
        }
        interestActive = true;
        let body: unknown = null;
        try {
          body = await res.json();
        } catch {
          body = null;
        }
        const terms = parseAcquireResponse(body);
        if (terms === null) {
          deactivate();
          release();
          refreshFailure("The hub returned an unexpected response.");
          return false;
        }
        if (disposed || abort.signal.aborted) return false;
        intervalMs = terms.intervalMs;
        leaseMs = terms.leaseMs;
        apply((previous) => ({
          intervalMs: terms.intervalMs,
          leaseMs: terms.leaseMs,
          phase: previous.detail === null ? "loading" : previous.phase,
          message: null,
        }));
        return true;
      } catch (error) {
        if (disposed || abort.signal.aborted) return false;
        void error;
        deactivate();
        refreshFailure(
          localHasSnapshot
            ? "Could not refresh process detail. Showing the last snapshot, which is not live."
            : "Could not reach the hub for process detail.",
        );
        return false;
      }
    };

    const schedulePoll = (): void => {
      if (disposed || finished || !interestActive || pollTimer !== null) return;
      pollTimer = setTimeout(() => {
        pollTimer = null;
        void pollOnce();
      }, intervalMs);
    };

    const scheduleRenew = (): void => {
      if (disposed || finished || renewTimer !== null) return;
      renewTimer = setTimeout(() => {
        renewTimer = null;
        void renewOnce();
      }, Math.max(1_000, Math.min(RENEW_MS, Math.floor(leaseMs / 3))));
    };

    const renewOnce = async (): Promise<void> => {
      if (disposed || finished) return;
      await acquire();
      if (disposed || finished) return;
      if (interestActive && !polling && pollTimer === null) void pollOnce();
      scheduleRenew();
    };

    const handleState = (rawState: string, result: DetailResult | null): void => {
      if (result !== null) {
        localHasSnapshot = true;
        const current = stateRef.current;
        const sameSnapshot =
          current.detail !== null &&
          current.detail.subscriptionId === result.subscriptionId &&
          current.detail.capturedAt === result.capturedAt;
        if (sameSnapshot && current.phase === "ready" && current.message === null) return;
        apply((previous) => {
          const unchanged =
            previous.detail !== null &&
            previous.detail.subscriptionId === result.subscriptionId &&
            previous.detail.capturedAt === result.capturedAt;
          return {
            phase: "ready",
            detail: unchanged ? previous.detail : result,
            message: null,
            lastReceivedMs: unchanged ? previous.lastReceivedMs : Date.now(),
          };
        });
        return;
      }

      const phase = detailStateToPhase(rawState);
      switch (phase) {
        case "ready":
        case "loading": {
          apply({
            phase: localHasSnapshot ? "stale" : "loading",
            message: localHasSnapshot
              ? "Waiting for a fresh snapshot. The values shown are from the last received snapshot."
              : "Waiting for the collector to report processes…",
          });
          return;
        }
        case "disconnected": {
          apply({
            phase: "disconnected",
            message: localHasSnapshot
              ? "The collector is disconnected. Showing the last received snapshot; it is not live."
              : "The collector is disconnected.",
          });
          return;
        }
        case "withheld":
        case "unsupported":
        case "cancelled": {
          terminal(phase, phasePresentation(phase).description);
          return;
        }
        case "busy":
        case "expired":
        case "failed": {
          apply({
            phase,
            message: localHasSnapshot
              ? `${phasePresentation(phase).description} Showing the last received snapshot.`
              : phasePresentation(phase).description,
          });
          return;
        }
        case "paused": {
          apply({ phase: "paused", message: "Process updates are paused." });
          return;
        }
        default: {
          apply({
            phase: localHasSnapshot ? "stale" : "error",
            message: "Unable to read the process status from the hub.",
          });
        }
      }
    };

    const pollOnce = async (): Promise<void> => {
      if (disposed || finished || polling) return;
      polling = true;
      const controller = new AbortController();
      pollAbort = controller;
      const abortPoll = (): void => controller.abort();
      abort.signal.addEventListener("abort", abortPoll, { once: true });
      try {
        const res = await fetch(baseUrl, {
          cache: "no-store",
          signal: controller.signal,
          headers: { Accept: "application/json" },
        });
        if (disposed || controller.signal.aborted) return;
        if (res.status === 410) {
          apply({
            phase: localHasSnapshot ? "stale" : "loading",
            message: "Process detail expired. Renewing…",
          });
          await acquire();
          return;
        }
        if (!res.ok) {
          const detail = await responseErrorDetail(res);
          if (res.status === 401) {
            terminal("error", "Your session ended. Sign in again to inspect processes.");
          } else if (res.status === 403) {
            terminal("withheld", detail ?? "Processes are not available to this machine.");
          } else {
            refreshFailure(detail ?? `Could not refresh processes (HTTP ${res.status}).`);
          }
          return;
        }
        let body: unknown = null;
        try {
          body = await res.json();
        } catch {
          body = null;
        }
        const parsed = parseDetailPollResponse(body);
        if (disposed || controller.signal.aborted) return;
        if (parsed === null) {
          refreshFailure(
            localHasSnapshot
              ? "Unable to read the process update. Showing the last snapshot."
              : "Unable to read the process update from the hub.",
          );
          return;
        }
        handleState(parsed.state, parsed.result);
      } catch {
        if (disposed || controller.signal.aborted) return;
        refreshFailure(
          localHasSnapshot
            ? "Process refresh failed. Showing the last snapshot, which is not live."
            : "Could not reach the hub for process detail.",
        );
      } finally {
        abort.signal.removeEventListener("abort", abortPoll);
        if (pollAbort === controller) pollAbort = null;
        polling = false;
        if (!disposed && !finished) schedulePoll();
      }
    };

    apply((previous) => {
      if (previous.detail === null) return { phase: "acquiring", message: null };
      if (previous.phase === "ready" && previous.message === null) return {};
      return { phase: "stale", message: "Resuming process updates…" };
    });

    void (async () => {
      const ok = await acquire();
      if (disposed || finished) return;
      scheduleRenew();
      if (ok) void pollOnce();
    })();

    return () => {
      disposed = true;
      finished = true;
      clearTimers();
      release();
    };
  }, [machineId, csrfToken, paused, hidden, refreshNonce]);

  const sameScope = scopeRef.current.machineId === machineId && scopeRef.current.csrfToken === csrfToken;
  return { state: sameScope ? state : initialState(machineId, csrfToken), hidden, refresh };
}

/**
 * Last good snapshot per machine, kept for the life of the tab. When the user
 * leaves Processes and comes back, the view repaints from here immediately and
 * the fresh lease/poll replaces it a moment later. Old entries are ignored so a
 * long-forgotten snapshot never poses as current.
 */
const SNAPSHOT_CACHE = new ProcessDetailCache();

function initialState(machineId: string | null, csrfToken: string | null): ProcessDetailState {
  const cached = SNAPSHOT_CACHE.get(machineId ?? "", csrfToken);
  if (cached !== undefined) {
    return { phase: "stale", message: "Resuming process updates…", ...cached };
  }
  return {
    phase: machineId === null ? "idle" : "acquiring",
    detail: null,
    message: null,
    intervalMs: DEFAULT_INTERVAL_MS,
    leaseMs: DEFAULT_LEASE_MS,
    lastReceivedMs: null,
  };
}

function newViewerId(): string {
  const cryptoApi = globalThis.crypto;
  if (cryptoApi && typeof cryptoApi.randomUUID === "function") return cryptoApi.randomUUID();
  return `viewer-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

async function responseErrorDetail(res: Response): Promise<string | null> {
  try {
    const body: unknown = await res.json();
    if (body && typeof body === "object" && "error" in body) {
      const error = (body as { error?: unknown }).error;
      if (typeof error === "string" && error.length > 0) return error;
    }
  } catch {
    // Non-JSON body; fall back to the status text.
  }
  return res.statusText || null;
}
