/**
 * Shared disk budget across per-destination outboxes.
 *
 * Each destination owns an independent outbox (and therefore its own file),
 * but total disk use is bounded both logically (record JSON) and physically
 * (actual main+WAL bytes). The budget is deliberately tiny and synchronous:
 * outboxes register themselves when they open, report their logical and
 * physical byte counts, and enforcement asks the budget which outbox is
 * largest when either cap is exceeded.
 *
 * The type-only import of DestinationOutbox keeps outbox.ts <-> budget.ts a
 * type cycle with no runtime edge.
 */
import type { DestinationOutbox } from './outbox';

/** Prototype cap for all destination outboxes on one collector. */
export const DEFAULT_MAX_TOTAL_OUTBOX_BYTES = 256 * 1024 * 1024;
/**
 * Finite aggregate physical spool cap (main files plus WAL) for all
 * destinations. Eight destinations at the 80 MiB physical cap fit exactly;
 * the logical aggregate cap remains 256 MiB.
 */
export const DEFAULT_MAX_TOTAL_OUTBOX_DISK_BYTES = 640 * 1024 * 1024;

export class OutboxBudget {
  readonly maxTotalBytes: number;
  readonly maxTotalDiskBytes: number;
  private readonly outboxes = new Set<DestinationOutbox>();

  constructor(
    maxTotalBytes: number = DEFAULT_MAX_TOTAL_OUTBOX_BYTES,
    maxTotalDiskBytes: number = DEFAULT_MAX_TOTAL_OUTBOX_DISK_BYTES,
  ) {
    if (!Number.isSafeInteger(maxTotalBytes) || maxTotalBytes < 0) {
      throw new Error(`maxTotalBytes must be a non-negative safe integer, got ${String(maxTotalBytes)}`);
    }
    if (!Number.isSafeInteger(maxTotalDiskBytes) || maxTotalDiskBytes < 0) {
      throw new Error(`maxTotalDiskBytes must be a non-negative safe integer, got ${String(maxTotalDiskBytes)}`);
    }
    this.maxTotalBytes = maxTotalBytes;
    this.maxTotalDiskBytes = maxTotalDiskBytes;
  }

  register(outbox: DestinationOutbox): void {
    this.outboxes.add(outbox);
    outbox.attachBudget(this);
  }

  unregister(outbox: DestinationOutbox): void {
    if (this.outboxes.delete(outbox)) outbox.attachBudget(null);
  }

  /** Bytes over the shared logical cap; 0 when within budget. */
  overBy(): number {
    return Math.max(0, this.totalBytes() - this.maxTotalBytes);
  }

  /** Actual main+WAL bytes across registered outboxes. */
  diskBytes(): number {
    let total = 0;
    for (const outbox of this.outboxes) total += outbox.diskBytes();
    return total;
  }

  /** Actual physical bytes over the shared physical cap; 0 when within budget. */
  diskOverBy(): number {
    return Math.max(0, this.diskBytes() - this.maxTotalDiskBytes);
  }

  /** Ask every registered outbox to truncate its bounded WAL (best effort). */
  checkpointAll(): void {
    for (const outbox of this.outboxes) outbox.checkpoint();
  }

  /**
   * Outbox currently using the most bytes. `exclude` lets the caller skip
   * outboxes that have nothing evictable (for example only in-flight records)
   * without adding another public obligation to the budget.
   */
  largest(exclude?: ReadonlySet<DestinationOutbox>): DestinationOutbox | null {
    let largest: DestinationOutbox | null = null;
    let largestBytes = -1;
    for (const outbox of this.outboxes) {
      if (exclude?.has(outbox)) continue;
      const bytes = outbox.bytes();
      if (bytes > largestBytes) {
        largest = outbox;
        largestBytes = bytes;
      }
    }
    return largest;
  }

  /** Physical counterpart of `largest`, using measured main+WAL bytes. */
  largestByDisk(exclude?: ReadonlySet<DestinationOutbox>): DestinationOutbox | null {
    let largest: DestinationOutbox | null = null;
    let largestBytes = -1;
    for (const outbox of this.outboxes) {
      if (exclude?.has(outbox)) continue;
      const bytes = outbox.diskBytes();
      if (bytes > largestBytes) {
        largest = outbox;
        largestBytes = bytes;
      }
    }
    return largest;
  }

  private totalBytes(): number {
    let total = 0;
    for (const outbox of this.outboxes) total += outbox.bytes();
    return total;
  }
}
