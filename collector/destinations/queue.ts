/**
 * The delivery queue a destination buffers unacknowledged telemetry in.
 *
 * Two interchangeable implementations satisfy this interface:
 *
 * - `MemoryDestinationQueue` (default): the pending queue lives in RAM. An
 *   unacknowledged sample is lost when the collector restarts, and the queue
 *   writes nothing to disk.
 * - `DestinationOutbox` (opt-in `COLLECTOR_QUEUE_MODE=durable`): the queue is a
 *   SQLite file per destination, so an unacknowledged sample survives a
 *   collector restart at the cost of a write per sample.
 *
 * The rest of the collector (session, connection, budget) only sees this
 * interface, so the mode is chosen once at open time and nothing else branches
 * on it. `queueMode` is reported to the hub so an operator can see whether
 * unacknowledged samples are durable.
 */
import type {
  DeliveryStatus,
  GapReason,
  MetricsRecord,
  RecordRef,
  TelemetryRecord,
} from '../../src/contracts';
import type { OutboxDiskStatus } from './outbox';
import type { OutboxBudget } from './budget';

/** How a destination's unacknowledged queue is retained. */
export type QueueMode = "memory" | "durable";

export const DEFAULT_COLLECTOR_QUEUE_MODE: QueueMode = "memory";

/**
 * Everything the delivery loop needs from a queue. Record identity on the wire
 * is `(runId, sequence)`; a queue never renumbers a record the hub may already
 * have seen, and only a merged gap record takes a fresh key.
 */
export interface DestinationQueue {
  readonly queueMode: QueueMode;

  /** Admit one already-filtered metrics record. Never throws. */
  append(record: MetricsRecord): void;
  /** Reapply the current privacy policy to every retained metrics record. */
  filterPending(transform: (record: MetricsRecord) => MetricsRecord): void;

  /** The newest metrics record awaiting the live slot, or null. */
  takeLive(): TelemetryRecord | null;
  /** Oldest pending records, oldest first, bounded by count and bytes. */
  takeReplay(maxRecords: number, maxBytes: number): TelemetryRecord[];

  markInFlight(refs: readonly RecordRef[]): void;
  release(refs: readonly RecordRef[]): void;
  releaseAll(): void;
  acknowledge(accepted: readonly RecordRef[], rejected: readonly RecordRef[]): void;

  /** Apply the age/byte/gap caps. */
  enforceLimits(): void;
  /** Internal: the shared budget chose this queue as a victim. Returns records shed. */
  evictForBudget(reason: GapReason): number;

  status(): DeliveryStatus;
  /** Logical bytes of retained record JSON (pending plus in-flight). */
  bytes(): number;
  /** Physical file bytes this queue holds; always 0 in memory mode. */
  diskBytes(): number;
  diskStatus(): OutboxDiskStatus;
  checkpoint(): void;
  attachBudget(budget: OutboxBudget | null): void;
  close(): void;
}
