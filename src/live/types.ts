/**
 * Shared types and constants for the browser-side live view layer.
 *
 * This module stays browser-safe: contracts types are importable anywhere,
 * and nothing here touches Node APIs, the DOM, or React. The constants in
 * this file are the frontend's own presentation policy. The freshness
 * baseline here is a *floor*: machines whose collector advertises its
 * delivery cadence (hello/snapshot `expectedIntervalMs`) go stale at
 * max(15 s, 3 × advertised interval); machines without cadence
 * metadata (null before or without a hello) use the bare 15 s
 * fallback. The hub never guesses cadence, and neither does the UI.
 */

import type { HubSnapshot, MachineState } from "../contracts";

// ---------------------------------------------------------------------------
// Presentation policy constants
// ---------------------------------------------------------------------------

/**
 * Freshness floor and fallback (ms): the stale threshold is at least this,
 * and equals this for machines without a known delivery cadence.
 */
export const FRESHNESS_THRESHOLD_MS = 15_000;

/**
 * |capturedAt − receivedAt| beyond this is presented as collector clock skew.
 * Skew is flagged, never used to judge liveness.
 */
export const SKEW_TOLERANCE_MS = 90_000;

/** Maximum in-memory sample points retained per machine for the page-open chart. */
export const SAMPLE_HISTORY_LIMIT = 180;

// ---------------------------------------------------------------------------
// Session state (from the backend agreement: /api/auth/*)
// ---------------------------------------------------------------------------

export type SessionUser = { username: string; csrfToken: string; admin?: boolean };

export type SessionState =
  | { kind: "restoring" }
  | { kind: "restore-failed"; detail: string | null }
  | { kind: "signed-out" }
  | { kind: "signed-in"; user: SessionUser }
  | { kind: "auth-unavailable"; detail: string | null };

// ---------------------------------------------------------------------------
// Stream phase (GET /api/live connection health)
// ---------------------------------------------------------------------------

export type StreamPhase =
  /** No stream running: signed out, auth unavailable, or before connect. */
  | "idle"
  /** Stream open, waiting for the first snapshot event. */
  | "connecting"
  /** Normal operation: snapshots/updates arriving. */
  | "live"
  /** Retaining the last snapshot while a bounded retry schedule runs. */
  | "reconnecting"
  /** Bounded retries exhausted; a slow background probe may recover the stream. */
  | "disconnected"
  /** The session ended while viewing; the stream is stopped. */
  | "expired"
  /** The account cannot read this hub's live scope. */
  | "no-permission";

export type LiveStreamEvent = "snapshot" | "update" | "reset";

// ---------------------------------------------------------------------------
// Derived view data
// ---------------------------------------------------------------------------

/** One in-memory sample point per machine, from snapshots this browser received. */
export type SampleHistoryPoint = {
  sampleId: string;
  /** Hub receipt time, never the collector's capture clock. */
  receivedAtLocal: number;
  /** CPU percent when available, null when the field was unavailable. */
  cpuPercent: number | null;
  /** Memory used/total percent when available, null when unavailable. */
  memoryPercent: number | null;
  /**
   * Cadence-derived gap threshold (ms) captured when this point arrived:
   * a chart segment breaks when a receipt gap reaches it. Optional so
   * hand-built test fixtures stay valid; consumers fall back to
   * FRESHNESS_THRESHOLD_MS when a point omits it. Captured per point so a
   * later cadence change cannot retroactively join an old gap.
   */
  gapThresholdMs?: number;
};

/** The context value `useLive()` hands to the app. */
export type LiveContextValue = {
  session: SessionState;
  phase: StreamPhase;
  hubRunId: string | null;
  /** Last known machines; retained (labeled) while the stream is down. */
  machines: MachineState[];
  /** Local receipt time of the latest snapshot/update/reset event. */
  lastReceivedLocalMs: number | null;
  lastEventId: string | null;
  lastEventKind: LiveStreamEvent | null;
  /** Transient message shown in the header area (reset notices, sign-out failures). */
  notice: string | null;
  /** Per-machine in-memory samples received since this page was opened. */
  history: ReadonlyMap<string, ReadonlyArray<SampleHistoryPoint>>;
  login(username: string, password: string): Promise<import("./session").LoginOutcome>;
  signOut(): Promise<void>;
  /** Re-check session and reconnect (manual recovery from disconnected/expired). */
  retryConnection(): void;
  dismissNotice(): void;
};

/** Kept for callers that want the whole snapshot shape via the context. */
export type { HubSnapshot };
