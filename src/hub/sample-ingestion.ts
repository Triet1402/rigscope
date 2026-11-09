/**
 * Deep hub Sample ingestion: the one module that owns authenticated Sample
 * acceptance for the current Collector session, the commit-to-live sequence,
 * eligible Incident evaluation and restart restoration.
 *
 * The Collector transport keeps framing, serialization, socket bounds and
 * lifecycle; this module owns everything that turns an accepted record batch
 * into a durable commit, a live publication and an Incident observation:
 *
 * - record identity is authenticated against the current Collector session
 *   (Collector identity always; run/boot identity for live mode), so a
 *   replaced or old run can only shape history, never publish live state,
 * - eligible live state and Incident observations publish immediately, before
 *   a persistent store has committed; acknowledgments still represent the
 *   store's own acceptance contract (RAM in memory mode, COMMIT in persistent
 *   mode), and overload/storage failures answer with retryAfterMs,
 * - a retry or stale sequence cannot publish twice because HubState ignores
 *   non-increasing run/sequence pairs, and session replacement clears the old
 *   run's live row,
 * - a session that is replaced or revoked during the asynchronous commit can
 *   no longer publish live state or trigger Incidents (the commit itself
 *   stays durable and exactly-once), because publication re-checks the
 *   still-current session before applying metrics,
 * - retries deduplicate by Collector/run/sequence through the store's
 *   receipts; replay batches fill history without touching live state or
 *   historical Incidents, so only eligible newly committed current live
 *   Samples reach Incident evaluation,
 * - restart restoration binds persisted metrics, hello-advertised delivery
 *   cadence, inventory and presentation metadata into state without
 *   implying a currently connected Collector.
 */

import {
  MAX_IN_FLIGHT_BATCHES,
  type Inventory,
  type MetricsRecord,
  type RecordRef,
  type TelemetryRecord,
} from "../contracts";
import type { HubState } from "./state";
import type { TelemetryStore } from "./storage/telemetry-store";

/**
 * Both history backends expose the same ingestion surface. Live publication is
 * deliberately independent of this store's acknowledgement policy.
 */
export type IngestionStorage = Pick<
  TelemetryStore,
  "enqueue" | "latestLive" | "saveInventory" | "loadInventory" | "getMachineMeta"
>;

/**
 * Maximum unacknowledged sample batches accepted per session (welcome
 * maxInFlightBatches). This is the protocol's wire maximum: existing collectors
 * reject a larger welcome, so the persistent flush interval is bounded below
 * to fit this window at the fastest supported 100 ms cadence instead of
 * growing the window without limit.
 */
export const MAX_IN_FLIGHT_SAMPLE_BATCHES = MAX_IN_FLIGHT_BATCHES;
/** Overload answers retry immediately with this delay, matching the previous behavior. */
export const RETRY_AFTER_OVERLOAD_MS = 1000;

export type SampleOutcome =
  | { status: "committed"; accepted: RecordRef[]; rejected: (RecordRef & { reason: string })[] }
  | { status: "retry"; retryAfterMs: number };

/**
 * One batch offered to ingestion: `queued` resolves only after the durable
 * commit (or as retry on overload inside the store), `retry` answers
 * overload before any database work. `submitSamples` throws for a dead
 * session or mismatched record identity so the transport fails the
 * connection exactly as before.
 */
export type SampleSubmission =
  | { status: "queued"; committed: Promise<SampleOutcome> }
  | { status: "retry"; retryAfterMs: number };

export type LiveSampleObserver = (
  machineId: string,
  record: MetricsRecord,
  receivedAtMs: number,
) => void;

export class SampleIngestion {
  private readonly state: HubState;
  private readonly storage: IngestionStorage;
  private readonly onLiveSample: LiveSampleObserver | null;
  private readonly inFlight = new Map<string, number>();

  constructor(input: {
    state: HubState;
    storage: IngestionStorage;
    onLiveSample?: LiveSampleObserver | null;
  }) {
    this.state = input.state;
    this.storage = input.storage;
    this.onLiveSample = input.onLiveSample ?? null;
  }

  /**
   * Offers one record batch for the authenticated session. May throw before
   * anything is queued (the transport closes the connection on identity or
   * session errors); resolved outcomes never open sockets.
   */
  submitSamples(sessionId: string, mode: "live" | "replay", records: TelemetryRecord[]): SampleSubmission {
    const session = this.requireSession(sessionId);
    for (const record of records) {
      if (
        record.collectorId !== session.collectorId ||
        (mode === "live" &&
          (record.runId !== session.runId || record.bootId !== session.bootId))
      ) {
        throw new Error("record identity does not match authenticated session");
      }
    }
    const inFlight = this.inFlight.get(sessionId) ?? 0;
    if (inFlight >= MAX_IN_FLIGHT_SAMPLE_BATCHES) {
      return { status: "retry", retryAfterMs: RETRY_AFTER_OVERLOAD_MS };
    }
    this.inFlight.set(sessionId, inFlight + 1);
    // Live publication is intentionally decoupled from the storage
    // acknowledgement. In memory mode nothing else happens; in persistent mode
    // a flush must never make the dashboard one flush behind. A retry or a
    // later durable failure cannot double-publish because HubState ignores
    // non-increasing run/sequence pairs, and session replacement clears the
    // old run's live row before a stale record could apply.
    const receivedAtMs = Date.now();
    const live = mode === "live" ? newestLiveRecord(records, receivedAtMs) : null;
    if (live !== null) this.publishLive(sessionId, live, receivedAtMs);

    // Hostname is the session's at queue time, not at publish time.
    const committed = this.storage.enqueue({
      machineId: session.machineId,
      collectorId: session.collectorId,
      mode,
      expectedIntervalMs: session.expectedIntervalMs,
      hostname: session.hostname,
      records,
    }).then((result) => {
      if (result.status !== "committed") {
        return { status: "retry" as const, retryAfterMs: result.retryAfterMs };
      }
      return {
        status: "committed" as const,
        accepted: result.accepted,
        rejected: result.rejected,
      };
    }).finally(() => {
      this.releaseInFlight(sessionId);
    });
    return { status: "queued", committed };
  }

  /**
   * Accepts an inventory message for the authenticated session: persisted
   * first, applied to live state second (same order as the durable path).
   */
  acceptInventory(sessionId: string, capturedAt: string, inventory: Inventory): void {
    const session = this.requireSession(sessionId);
    this.storage.saveInventory(session.machineId, capturedAt, inventory);
    this.state.setInventory(session.machineId, inventory);
  }

  /**
   * Restores persisted latest metrics, delivery cadence, inventory and
   * metadata at startup. Restored machines are not connected: no session is
   * created and no Incident observation is manufactured.
   */
  restorePersisted(): void {
    for (const latest of this.storage.latestLive()) {
      this.state.restoreMetrics(latest.machineId, {
        kind: "metrics",
        collectorId: latest.collectorId,
        runId: latest.runId,
        bootId: latest.bootId,
        sequence: latest.sequence,
        capturedAt: latest.capturedAt,
        payload: latest.payload,
      }, latest.receivedAt, latest);
    }
    for (const machine of this.state.snapshot().machines) {
      const inventory = this.storage.loadInventory(machine.machineId);
      if (inventory) this.state.setInventory(machine.machineId, inventory.inventory);
      const meta = this.storage.getMachineMeta(machine.machineId);
      if (meta.displayName !== null || meta.tags.length > 0) {
        this.state.setMetadata(machine.machineId, meta);
      }
    }
  }

  /**
   * Publishes one live record only while its session is still the current
   * authenticated one. A replaced or revoked session cannot publish or trigger
   * Incidents; HubState additionally enforces monotonic run/sequence order.
   */
  private publishLive(
    sessionId: string,
    record: MetricsRecord,
    receivedAtMs: number,
  ): void {
    const current = this.state.currentSession(sessionId);
    if (!current) return;
    const applied = this.state.applyMetrics(sessionId, record, new Date(receivedAtMs).toISOString());
    if (applied) this.onLiveSample?.(current.machineId, record, receivedAtMs);
  }

  /** Resolves the authenticated session for every acceptance path. */
  private requireSession(sessionId: string) {
    const session = this.state.currentSession(sessionId);
    if (!session) throw new Error("session is no longer current");
    return session;
  }

  private releaseInFlight(sessionId: string): void {
    const remaining = (this.inFlight.get(sessionId) ?? 1) - 1;
    if (remaining <= 0) this.inFlight.delete(sessionId);
    else this.inFlight.set(sessionId, remaining);
  }
}

/** Newest usable LIVE metrics record in one batch, or null. */
function newestLiveRecord(
  records: readonly TelemetryRecord[],
  receivedAtMs: number,
): MetricsRecord | null {
  let best: MetricsRecord | null = null;
  let bestCaptured = 0;
  let bestSequence = -1;
  for (const record of records) {
    if (record.kind !== "metrics") continue;
    const captured = Date.parse(record.capturedAt);
    if (!Number.isFinite(captured) || captured > receivedAtMs + 86_400_000) continue;
    if (
      best === null ||
      captured > bestCaptured ||
      (captured === bestCaptured && record.sequence > bestSequence)
    ) {
      best = record;
      bestCaptured = captured;
      bestSequence = record.sequence;
    }
  }
  return best;
}
