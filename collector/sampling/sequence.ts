/**
 * Monotonic sequence allocation for one collector run.
 *
 * The contract requires `(collectorId, runId, sequence)` to be unique across
 * every record kind in a run. One allocator is created per run and shared by
 * metric samplers and outbox gap records; `next()` returns 1 on the first call,
 * and `current()` reports the last allocated value without consuming one.
 */

export type SequenceAllocator = {
  /** Allocates and returns the next sequence number. */
  next(): number;
  /** Last allocated sequence number (start value before any allocation). */
  current(): number;
};

export function createSequenceAllocator(start = 0): SequenceAllocator {
  let value = Number.isSafeInteger(start) && start >= 0 ? start : 0;
  return {
    next(): number {
      if (value >= Number.MAX_SAFE_INTEGER) {
        throw new RangeError('sequence exhausted');
      }
      value += 1;
      return value;
    },
    current(): number {
      return value;
    },
  };
}
