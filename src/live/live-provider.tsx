"use client";

/**
 * LiveProvider: one authorized SSE stream per tab, shared across client
 * navigation (ARCHITECTURE.md "Browser flow"). Mounted once from the root
 * layout so the fleet list, machine views, and the header never open a
 * stream each.
 *
 * Responsibilities:
 * - Restore the session via GET /api/auth/session (401 → signed out,
 *   503 → hub has no browser users configured).
 * - Own the single EventSource for /api/live; stop EventSource's own
 *   reconnect loop and manage bounded, rate-limited retries instead.
 * - Distinguish initial load, empty fleet, no permission, session expiry,
 *   reset, reconnecting, and hub disconnect as separate phases.
 * - Retain the last known machine values (labeled as last known by the
 *   views) while reconnecting; clear them when authorization is lost.
 * - Probe the session after stream errors to tell "session expired" from
 *   "hub unreachable".
 * - Keep a small in-memory per-machine sample history for the honest
 *   "since you opened this page" chart. The frontend never requests
 *   collection and never fakes missing values.
 */

import {
  createContext,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import type { HubSnapshot, MachineState } from "../contracts";
import {
  fetchSession,
  type LoginOutcome,
  login as loginRequest,
  probeLiveEndpoint,
  signOut as signOutRequest,
} from "./session";
import { LiveStream, type LiveStreamEvent } from "./stream";
import { historyForSnapshot } from "./history";
import {
  type LiveContextValue,
  type SampleHistoryPoint,
  type SessionState,
  type StreamPhase,
} from "./types";

/** Bounded quick-retry schedule after connection errors, in milliseconds. */
const RETRY_DELAYS_MS = [1_000, 2_000, 4_000, 8_000, 15_000];
/** Slow background retry cadence once the bounded schedule is exhausted. */
const SLOW_RETRY_INTERVAL_MS = 30_000;

const LiveContext = createContext<LiveContextValue | null>(null);

export function useLive(): LiveContextValue {
  const value = useContext(LiveContext);
  if (!value) {
    throw new Error("useLive must be used inside <LiveProvider>");
  }
  return value;
}

export function LiveProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<SessionState>({ kind: "restoring" });
  const [phase, setPhase] = useState<StreamPhase>("idle");
  const [hubRunId, setHubRunId] = useState<string | null>(null);
  const [machines, setMachines] = useState<MachineState[]>([]);
  const [lastReceivedLocalMs, setLastReceivedLocalMs] = useState<number | null>(null);
  const [lastEventId, setLastEventId] = useState<string | null>(null);
  const [lastEventKind, setLastEventKind] = useState<LiveStreamEvent | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [history, setHistory] = useState<ReadonlyMap<string, readonly SampleHistoryPoint[]>>(new Map());

  // Refs hold everything the async lifecycle touches so closures stay valid.
  const aliveRef = useRef(true);
  const restoreSeqRef = useRef(0);
  const streamRef = useRef<LiveStream | null>(null);
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const attemptsRef = useRef(0);
  /** Latest session state outside React's render cycle. */
  const sessionRef = useRef<SessionState>(session);
  const generationRef = useRef(0);
  const hasSnapshotRef = useRef(false);
  const signingOutRef = useRef(false);

  const applySession = (next: SessionState): void => {
    sessionRef.current = next;
    setSession(next);
  };

  const clearRetryTimer = (): void => {
    if (retryTimerRef.current !== null) {
      clearTimeout(retryTimerRef.current);
      retryTimerRef.current = null;
    }
  };

  const closeStream = (): void => {
    generationRef.current += 1;
    restoreSeqRef.current += 1;
    streamRef.current?.close();
    streamRef.current = null;
    clearRetryTimer();
  };

  const clearData = (): void => {
    hasSnapshotRef.current = false;
    setMachines([]);
    setHistory(new Map());
    setHubRunId(null);
    setLastEventId(null);
    setLastEventKind(null);
    setLastReceivedLocalMs(null);
  };

  const signedIn = (user: { username: string; csrfToken: string }): void => {
    const current = sessionRef.current;
    if (current.kind !== "signed-in" || current.user.csrfToken !== user.csrfToken) clearData();
    applySession({ kind: "signed-in", user });
  };

  const startStream = (): void => {
    if (sessionRef.current.kind !== "signed-in") return;
    closeStream();
    setPhase(hasSnapshotRef.current ? "reconnecting" : "connecting");
    const stream = new LiveStream();
    streamRef.current = stream;
    stream.start("/api/live", {
      onSnapshot: (snapshot, event, eventId) => {
        // Only the currently mounted stream may write state.
        if (!aliveRef.current || streamRef.current !== stream) return;
        attemptsRef.current = 0;
        hasSnapshotRef.current = true;
        setHubRunId(snapshot.hubRunId);
        setMachines(snapshot.machines);
        setLastReceivedLocalMs(Date.now());
        setLastEventId(eventId || null);
        setLastEventKind(event);
        setHistory((previous) =>
          historyForSnapshot(previous, snapshot),
        );
        setPhase("live");
      },
      onError: () => {
        if (!aliveRef.current || streamRef.current !== stream) return;
        void handleConnectionLoss();
      },
    });
  };

  /** Runs after a stream error: decide expiry vs transient, then retry boundedly. */
  const handleConnectionLoss = async (): Promise<void> => {
    closeStream(); // invalidates every outstanding probe from the old stream
    const generation = generationRef.current;
    setPhase("reconnecting");
    const probe = await fetchSession();
    if (!aliveRef.current || generation !== generationRef.current) return;
    if (probe.kind === "signed-in") {
      // Refresh the cached CSRF token while we are here.
      signedIn(probe.user);
    } else if (probe.kind === "signed-out") {
      applySession({ kind: "signed-out" });
      clearData();
      setPhase("expired");
      return;
    } else if (probe.kind === "auth-unavailable") {
      applySession({ kind: "auth-unavailable", detail: probe.detail });
      clearData();
      setPhase("idle");
      return;
    }

    const attempt = Math.min(attemptsRef.current + 1, RETRY_DELAYS_MS.length + 1);
    attemptsRef.current = attempt;
    if (attempt > RETRY_DELAYS_MS.length) {
      setPhase("disconnected");
      scheduleSlowRecovery();
      return;
    }
    setPhase("reconnecting");
    const delay = RETRY_DELAYS_MS[attempt - 1];
    retryTimerRef.current = setTimeout(() => {
      void retryConnect();
    }, delay);
  };

  /** One attempt step: check live access, then open the stream. */
  const retryConnect = async (): Promise<void> => {
    if (!aliveRef.current || signingOutRef.current) return;
    closeStream();
    const generation = generationRef.current;
    const access = await probeLiveEndpoint();
    if (!aliveRef.current || generation !== generationRef.current) return;
    if (access.kind === "forbidden") {
      clearData();
      setPhase("no-permission");
      return;
    }
    if (access.kind === "unauthorized") {
      const probe = await fetchSession();
      if (!aliveRef.current || generation !== generationRef.current) return;
      if (probe.kind === "signed-in") {
        signedIn(probe.user);
        startStream();
        return;
      }
      if (probe.kind === "signed-out") {
        applySession({ kind: "signed-out" });
        clearData();
        setPhase("expired");
        return;
      }
      if (probe.kind === "auth-unavailable") {
        applySession({ kind: "auth-unavailable", detail: probe.detail });
        clearData();
        setPhase("idle");
        return;
      }
    }
    // ok/unreachable/error all end here; an unreachable hub fails fast on
    // the EventSource and re-enters the bounded schedule.
    startStream();
  };

  const scheduleSlowRecovery = (): void => {
    clearRetryTimer();
    retryTimerRef.current = setTimeout(() => {
      void slowRecoveryProbe();
    }, SLOW_RETRY_INTERVAL_MS);
  };

  /** Slow, bounded-rate background recovery while disconnected. */
  const slowRecoveryProbe = async (): Promise<void> => {
    if (!aliveRef.current || signingOutRef.current) return;
    closeStream();
    const generation = generationRef.current;
    const probe = await fetchSession();
    if (!aliveRef.current || generation !== generationRef.current) return;
    if (probe.kind === "signed-in") {
      signedIn(probe.user);
      startStream();
      return;
    }
    if (probe.kind === "signed-out") {
      applySession({ kind: "signed-out" });
      clearData();
      setPhase("expired");
      return;
    }
    if (probe.kind === "auth-unavailable") {
      applySession({ kind: "auth-unavailable", detail: probe.detail });
      clearData();
      setPhase("idle");
      return;
    }
    scheduleSlowRecovery();
  };

  /** Initial (or manual) session restore. Guarded by a sequence number. */
  const restore = async (seq: number): Promise<void> => {
    const generation = generationRef.current;
    const probe = await fetchSession();
    if (!aliveRef.current || restoreSeqRef.current !== seq || generation !== generationRef.current) return;
    if (probe.kind === "signed-in") {
      signedIn(probe.user);
      attemptsRef.current = 0;
      startStream();
      return;
    }
    if (probe.kind === "signed-out") {
      applySession({ kind: "signed-out" });
      setPhase("idle");
      return;
    }
    if (probe.kind === "auth-unavailable") {
      applySession({ kind: "auth-unavailable", detail: probe.detail });
      setPhase("idle");
      return;
    }
    applySession({
      kind: "restore-failed",
      detail: probe.kind === "error" ? probe.detail : null,
    });
    setPhase("idle");
  };

  const login = async (username: string, password: string): Promise<LoginOutcome> => {
    closeStream();
    const generation = generationRef.current;
    const result = await loginRequest(username, password);
    if (!aliveRef.current || generation !== generationRef.current) return { kind: "error", detail: "Session request superseded. Try again." };
    if (result.kind === "signed-in" && aliveRef.current) {
      signedIn(result.user);
      setNotice(null);
      attemptsRef.current = 0;
      startStream();
    }
    return result;
  };

  const signOutFn = async (): Promise<void> => {
    const current = sessionRef.current;
    if (current.kind !== "signed-in" || signingOutRef.current) return;
    signingOutRef.current = true;
    closeStream();
    const generation = generationRef.current;
    clearData();
    setPhase("idle");
    const result = await signOutRequest(current.user.csrfToken);
    signingOutRef.current = false;
    if (!aliveRef.current || generation !== generationRef.current) return;
    if (result === "ok") {
      applySession({ kind: "signed-out" });
      clearData();
      setPhase("idle");
      setNotice("Signed out.");
      return;
    }
    // The server did not confirm; restore honest state before resuming.
    const probe = await fetchSession();
    if (!aliveRef.current || generation !== generationRef.current) return;
    if (probe.kind === "signed-in") {
      signedIn(probe.user);
      setNotice("Sign out failed — try again.");
      startStream();
      return;
    }
    if (probe.kind === "signed-out") {
      applySession({ kind: "signed-out" });
      clearData();
      setPhase("idle");
      setNotice(null);
      return;
    }
    if (probe.kind === "auth-unavailable") {
      applySession({ kind: "auth-unavailable", detail: probe.detail });
      setPhase("idle");
      return;
    }
    applySession({
      kind: "restore-failed",
      detail: probe.kind === "error" ? probe.detail : null,
    });
    setPhase("idle");
  };

  /** Manual recovery from disconnected/expired/restore-failed/no-permission. */
  const retryConnection = (): void => {
    if (!aliveRef.current || signingOutRef.current) return;
    if (sessionRef.current.kind === "signed-in") {
      attemptsRef.current = 0;
      void retryConnect();
      return;
    }
    restoreSeqRef.current += 1;
    void restore(restoreSeqRef.current);
  };

  useEffect(() => {
    aliveRef.current = true;
    restoreSeqRef.current += 1;
    // Deliberately empty deps: lifecycle runs once per provider mount.
    const seq = restoreSeqRef.current;
    void restore(seq);
    return () => {
      aliveRef.current = false;
      closeStream();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <LiveContext.Provider
      value={{
        session,
        phase,
        hubRunId,
        machines,
        lastReceivedLocalMs,
        lastEventId,
        lastEventKind,
        notice,
        history,
        login,
        signOut: signOutFn,
        retryConnection,
        dismissNotice: () => setNotice(null),
      }}
    >
      {children}
    </LiveContext.Provider>
  );
}
