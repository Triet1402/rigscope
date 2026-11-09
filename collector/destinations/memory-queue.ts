/**
 * RAM-only per-destination delivery queue: the collector's default.
 *
 * Unacknowledged records wait in a bounded in-process list instead of a SQLite
 * file, so the collector writes nothing for routine telemetry. The trade is
 * explicit: a collector restart loses whatever was not yet acknowledged. The
 * hub is told this through `delivery.queueMode = "memory"`.
 *
 * Semantics deliberately mirror `DestinationOutbox` so the delivery loop is
 * mode-agnostic: the live slot, the count/byte-bounded replay batch, in-flight
 * records that are never evicted, oldest-first age/byte eviction, and coalesced
 * `gap` records that tell the hub "samples were captured but dropped here".
 *
 * Records are kept in their serialized wire form and parsed on every read,
 * exactly like a durable queue row. That gives identical byte accounting,
 * identical read semantics (a caller receives a detached copy it cannot use to
 * mutate what is still owed), and no object graph retained beyond the string.
 */
import type {
  DeliveryStatus,
  GapReason,
  GapRecord,
  MetricsRecord,
  RecordRef,
  TelemetryRecord,
} from '../../src/contracts';
import { DEFAULT_OUTBOX_LIMITS, type OutboxDiskStatus, type OutboxLimits } from './outbox';
import type { OutboxBudget } from './budget';
import type { DestinationQueue, QueueMode } from './queue';

/** Queue entry: the record plus the delivery bookkeeping the session needs. */
type Entry = {
  /** Insertion order; mirrors the durable queue's rowid ordering. */
  id: number;
  runId: string;
  sequence: number;
  capturedAt: string;
  capturedAtMs: number;
  kind: 'metrics' | 'gap';
  gapReason: GapReason | null;
  state: 'pending' | 'inflight';
  /** True once the record has been offered to a batch, so gaps stop coalescing. */
  offered: boolean;
  bytes: number;
  /** The record's wire JSON, as it will be sent. Parsed fresh on every read. */
  json: string;
};

export type MemoryQueueOptions = {
  limits?: Partial<OutboxLimits>;
  budget?: OutboxBudget;
  /** Allocates sequence numbers for locally created gap records. */
  allocateSequence: () => number;
  identity: { collectorId: string; runId: string; bootId: string };
  now?: () => number;
};

const refKey = ({ runId, sequence }: RecordRef): string => `${runId}\u0000${sequence}`;

/** Physical-file accounting for a queue that owns no file. */
const NO_DISK: OutboxDiskStatus = {
  bytes: 0, mainBytes: 0, walBytes: 0, logicalBytes: 0, limitBytes: 0, overBy: 0,
  pressure: false, walBudgetBytes: 0, checkpointPages: 0, pageCount: 0,
  maxPageCount: 0, freePages: 0, autoVacuum: 0,
};

export class MemoryDestinationQueue implements DestinationQueue {
  readonly queueMode: QueueMode = 'memory';

  private readonly limits: OutboxLimits;
  private budget: OutboxBudget | null;
  private readonly allocateSequence: () => number;
  private readonly identity: { collectorId: string; runId: string; bootId: string };
  private readonly now: () => number;

  private readonly entries: Entry[] = [];
  private readonly byRef = new Map<string, Entry>();
  private nextId = 1;
  private totalBytes = 0;
  /** id of the newest metrics record not yet offered to `takeLive`. */
  private liveId: number | null = null;
  private droppedSinceOpen = 0;
  private lastAckAt: string | null = null;
  private closed = false;

  constructor(opts: MemoryQueueOptions) {
    this.limits = { ...DEFAULT_OUTBOX_LIMITS, ...opts.limits };
    this.budget = opts.budget ?? null;
    this.allocateSequence = opts.allocateSequence;
    this.identity = opts.identity;
    this.now = opts.now ?? Date.now;
    this.budget?.register(this);
  }

  // -------------------------------------------------------------------------
  // Writes
  // -------------------------------------------------------------------------

  /**
   * Admit one filtered metrics record. Age and byte caps are applied first so
   * the newest sample always wins its slot, exactly as the durable queue sheds
   * the oldest pending telemetry under pressure. The shared budget and gap cap
   * are then settled so admission itself keeps retained data bounded, even if
   * the maintenance tick never runs. Never throws.
   */
  append(record: MetricsRecord): void {
    if (this.closed) return;
    try {
      const json = JSON.stringify(record);
      const bytes = Buffer.byteLength(json, 'utf8');
      if (bytes > this.limits.maxBytes) {
        // A single record larger than the whole queue can never be admitted.
        this.droppedSinceOpen += 1;
        this.writeGap('outbox-bytes', record.capturedAt, record.capturedAt, 1);
        this.enforceAdmission();
        return;
      }
      this.enforceAge();
      this.enforceBytes(this.limits.maxBytes - bytes);
      this.insertJson(record.kind, record, json, bytes);
      this.enforceAdmission();
    } catch {
      // A queue failure must never cross the sampling boundary.
      this.droppedSinceOpen += 1;
    }
  }

  /** Reapply the current privacy policy to every retained metrics record. */
  filterPending(transform: (record: MetricsRecord) => MetricsRecord): void {
    if (this.closed) return;
    // Iterate a snapshot: `parse` can drop an unreadable entry, which would
    // otherwise skip its neighbour in a live iteration.
    for (const entry of [...this.entries]) {
      if (entry.kind !== 'metrics') continue;
      const parsed = this.parse(entry);
      if (parsed === null) continue;
      const json = JSON.stringify(transform(parsed as MetricsRecord));
      const bytes = Buffer.byteLength(json, 'utf8');
      this.totalBytes += bytes - entry.bytes;
      entry.json = json;
      entry.bytes = bytes;
    }
    this.totalBytes = Math.max(0, this.totalBytes);
  }

  // -------------------------------------------------------------------------
  // Reads
  // -------------------------------------------------------------------------

  /**
   * Newest metrics record appended since the last `takeLive`. It stays pending
   * so a lost live send is still replayed later, matching the durable queue.
   */
  takeLive(): TelemetryRecord | null {
    if (this.closed || this.liveId === null) return null;
    const id = this.liveId;
    this.liveId = null;
    const entry = this.entries.find((candidate) => candidate.id === id);
    if (entry === undefined || entry.state !== 'pending' || entry.kind !== 'metrics') return null;
    return this.parse(entry);
  }

  /**
   * Oldest pending records, oldest first, excluding the record currently held
   * for the live slot. Bounded by both count and bytes, but always at least one
   * record so a single large sample cannot stall the queue.
   */
  takeReplay(maxRecords: number, maxBytes: number): TelemetryRecord[] {
    if (this.closed || !Number.isFinite(maxRecords) || maxRecords < 1) return [];
    const records: TelemetryRecord[] = [];
    let bytes = 0;
    for (const entry of this.entries) {
      if (records.length >= Math.floor(maxRecords)) break;
      if (entry.state !== 'pending' || entry.id === this.liveId) continue;
      if (records.length > 0 && bytes + entry.bytes > maxBytes) break;
      const parsed = this.parse(entry);
      if (parsed === null) continue;
      records.push(parsed);
      bytes += entry.bytes;
    }
    return records;
  }

  // -------------------------------------------------------------------------
  // Delivery state
  // -------------------------------------------------------------------------

  markInFlight(refs: readonly RecordRef[]): void {
    if (this.closed) return;
    for (const ref of refs) {
      const entry = this.byRef.get(refKey(ref));
      if (entry !== undefined && entry.state === 'pending') {
        entry.state = 'inflight';
        entry.offered = true;
      }
    }
  }

  release(refs: readonly RecordRef[]): void {
    if (this.closed) return;
    for (const ref of refs) {
      const entry = this.byRef.get(refKey(ref));
      if (entry !== undefined && entry.state === 'inflight') entry.state = 'pending';
    }
  }

  releaseAll(): void {
    if (this.closed) return;
    for (const entry of this.entries) if (entry.state === 'inflight') entry.state = 'pending';
  }

  /** Accepted and rejected records both leave the queue. Unknown refs are ignored. */
  acknowledge(accepted: readonly RecordRef[], rejected: readonly RecordRef[]): void {
    if (this.closed) return;
    if (accepted.length === 0 && rejected.length === 0) return;
    for (const ref of [...accepted, ...rejected]) {
      const key = refKey(ref);
      const entry = this.byRef.get(key);
      if (entry === undefined) continue;
      this.byRef.delete(key);
      const index = this.entries.indexOf(entry);
      if (index >= 0) this.entries.splice(index, 1);
      if (this.liveId === entry.id) this.liveId = null;
      this.totalBytes = Math.max(0, this.totalBytes - entry.bytes);
    }
    this.lastAckAt = this.isoNow();
  }

  // -------------------------------------------------------------------------
  // Retention
  // -------------------------------------------------------------------------

  enforceLimits(): void {
    if (this.closed) return;
    this.enforceAge();
    this.enforceBytes(this.limits.maxBytes);
    this.enforceAdmission();
  }

  /** Shared-budget victim: shed this queue's oldest pending metrics records. */
  evictForBudget(reason: GapReason): number {
    if (this.closed) return 0;
    const victims = this.entries.filter((entry) => entry.state === 'pending' && entry.kind === 'metrics').slice(0, 16);
    if (victims.length === 0) return 0;
    const evicted = this.evict(victims, reason);
    // Eviction writes a gap. When another destination's admission chose this
    // queue as the shared-budget victim, this queue must still keep that gap
    // within its cap without waiting for a maintenance tick.
    this.enforceGapCap();
    return evicted;
  }

  status(): DeliveryStatus {
    let pendingRecords = 0;
    let pendingBytes = 0;
    let inFlightRecords = 0;
    let oldest: string | null = null;
    for (const entry of this.entries) {
      if (entry.state === 'inflight') {
        inFlightRecords += 1;
        continue;
      }
      pendingRecords += 1;
      pendingBytes += entry.bytes;
      if (oldest === null || entry.capturedAtMs < Date.parse(oldest)) oldest = entry.capturedAt;
    }
    return {
      pendingRecords,
      pendingBytes,
      oldestPendingCapturedAt: oldest,
      inFlightRecords,
      droppedRecords: this.droppedSinceOpen,
      replaying: pendingRecords > 0,
      lastAckAt: this.lastAckAt,
      queueMode: this.queueMode,
    };
  }

  bytes(): number {
    return this.totalBytes;
  }

  /** A RAM queue owns no file: this is what makes the mode observable. */
  diskBytes(): number {
    return 0;
  }

  diskStatus(): OutboxDiskStatus {
    return { ...NO_DISK, logicalBytes: this.totalBytes };
  }

  checkpoint(): void {
    // Nothing to flush: there is no file.
  }

  attachBudget(budget: OutboxBudget | null): void {
    this.budget = budget;
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.budget?.unregister(this);
    this.entries.length = 0;
    this.byRef.clear();
    this.totalBytes = 0;
    this.liveId = null;
  }

  // -------------------------------------------------------------------------
  // Internals
  // -------------------------------------------------------------------------

  private isoNow(): string {
    return new Date(this.now()).toISOString();
  }

  private insertJson(
    kind: TelemetryRecord['kind'],
    record: TelemetryRecord,
    json: string,
    bytes: number,
  ): Entry {
    const entry: Entry = {
      id: this.nextId++,
      runId: record.runId,
      sequence: record.sequence,
      capturedAt: record.capturedAt,
      capturedAtMs: Date.parse(record.capturedAt),
      kind,
      gapReason: record.kind === 'gap' ? record.payload.reason : null,
      state: 'pending',
      offered: false,
      bytes,
      json,
    };
    this.entries.push(entry);
    this.byRef.set(refKey(entry), entry);
    this.totalBytes += bytes;
    if (record.kind === 'metrics') this.liveId = entry.id;
    return entry;
  }

  private remove(entry: Entry): void {
    this.byRef.delete(refKey(entry));
    const index = this.entries.indexOf(entry);
    if (index >= 0) this.entries.splice(index, 1);
    if (this.liveId === entry.id) this.liveId = null;
    this.totalBytes = Math.max(0, this.totalBytes - entry.bytes);
  }

  /** Fold evicted records into one coalesced gap so the hub sees the hole. */
  private evict(victims: readonly Entry[], reason: GapReason): number {
    if (victims.length === 0) return 0;
    let from = victims[0]!.capturedAt;
    let to = victims[0]!.capturedAt;
    let fromMs = victims[0]!.capturedAtMs;
    let toMs = victims[0]!.capturedAtMs;
    for (const entry of victims) {
      this.remove(entry);
      if (entry.capturedAtMs < fromMs) { fromMs = entry.capturedAtMs; from = entry.capturedAt; }
      if (entry.capturedAtMs > toMs) { toMs = entry.capturedAtMs; to = entry.capturedAt; }
    }
    this.droppedSinceOpen += victims.length;
    this.writeGap(reason, from, to, victims.length);
    return victims.length;
  }

  /** Fold one dropped interval into a gap payload, keeping the widest range. */
  private extendGapPayload(
    target: { fromCapturedAt: string; toCapturedAt: string; droppedRecords: number },
    source: { fromCapturedAt: string; toCapturedAt: string; droppedRecords: number },
  ): void {
    if (Date.parse(source.fromCapturedAt) < Date.parse(target.fromCapturedAt)) {
      target.fromCapturedAt = source.fromCapturedAt;
    }
    if (Date.parse(source.toCapturedAt) > Date.parse(target.toCapturedAt)) {
      target.toCapturedAt = source.toCapturedAt;
    }
    target.droppedRecords += source.droppedRecords;
  }

  /**
   * Coalesce into the newest pending gap of the same reason that has not been
   * offered yet, so per-reason evidence stays one record while gaps queue.
   */
  private writeGap(reason: GapReason, fromCapturedAt: string, toCapturedAt: string, droppedRecords: number): void {
    for (let index = this.entries.length - 1; index >= 0; index -= 1) {
      const candidate = this.entries[index]!;
      if (candidate.kind !== 'gap' || candidate.state !== 'pending' || candidate.offered) continue;
      if (candidate.runId !== this.identity.runId || candidate.gapReason !== reason) continue;
      const record = this.parse(candidate) as GapRecord | null;
      if (record === null) break;
      this.extendGapPayload(record.payload, { fromCapturedAt, toCapturedAt, droppedRecords });
      candidate.capturedAt = record.capturedAt;
      candidate.capturedAtMs = Date.parse(record.capturedAt);
      const json = JSON.stringify(record);
      const bytes = Buffer.byteLength(json, 'utf8');
      this.totalBytes += bytes - candidate.bytes;
      candidate.json = json;
      candidate.bytes = bytes;
      return;
    }
    const gap: GapRecord = {
      collectorId: this.identity.collectorId,
      runId: this.identity.runId,
      bootId: this.identity.bootId,
      sequence: this.allocateSequence(),
      capturedAt: this.isoNow(),
      kind: 'gap',
      payload: { fromCapturedAt, toCapturedAt, droppedRecords, reason },
    };
    const json = JSON.stringify(gap);
    this.insertJson('gap', gap, json, Buffer.byteLength(json, 'utf8'));
  }

  private pendingMetrics(): Entry[] {
    return this.entries.filter((entry) => entry.state === 'pending' && entry.kind === 'metrics');
  }

  private enforceAge(): void {
    const cutoff = this.now() - this.limits.maxAgeMs;
    const expired = this.pendingMetrics().filter((entry) => entry.capturedAtMs < cutoff);
    if (expired.length > 0) this.evict(expired, 'outbox-age');
  }

  /** Evict oldest pending metrics until the queue fits `targetBytes`. */
  private enforceBytes(targetBytes: number): void {
    if (this.totalBytes <= targetBytes) return;
    const pending = this.pendingMetrics();
    const victims: Entry[] = [];
    for (const entry of pending) {
      if (this.totalBytes - sumBytes(victims) <= targetBytes) break;
      victims.push(entry);
    }
    if (victims.length === 0 && pending.length > 0) victims.push(pending[0]!);
    if (victims.length > 0) this.evict(victims, 'outbox-bytes');
  }

  /** Settle the caps that can change after any admission or eviction. */
  private enforceAdmission(): void {
    this.enforceBudget();
    this.enforceGapCap();
  }

  private enforceBudget(): void {
    const budget = this.budget;
    if (budget === null) return;
    const tried = new Set<DestinationQueue>();
    let guard = 0;
    while (budget.overBy() > 0 && guard < 10_000) {
      guard += 1;
      const target = budget.largest(tried);
      if (target === null) break;
      if (target.evictForBudget('disk-budget') === 0) tried.add(target);
    }
  }

  /** Hard ceiling on retained gap records: merge the oldest pending gaps. */
  private enforceGapCap(): void {
    let guard = 0;
    while (this.gapCount() > this.limits.maxGapRecords && guard < 10_000) {
      guard += 1;
      const gaps = this.entries.filter((entry) => entry.kind === 'gap' && entry.state === 'pending');
      if (gaps.length < 2) return;
      const older = gaps[0]!;
      const newer = gaps[1]!;
      const olderRecord = this.parse(older) as GapRecord | null;
      const merged = this.parse(newer) as GapRecord | null;
      if (olderRecord === null || merged === null) return;
      this.extendGapPayload(merged.payload, olderRecord.payload);
      // Merging can replace a record the hub already committed, so the merged
      // gap takes a fresh key rather than editing an acknowledged payload.
      merged.collectorId = this.identity.collectorId;
      merged.runId = this.identity.runId;
      merged.bootId = this.identity.bootId;
      merged.sequence = this.allocateSequence();
      merged.capturedAt = this.isoNow();
      this.remove(newer);
      this.remove(older);
      const json = JSON.stringify(merged);
      this.insertJson('gap', merged, json, Buffer.byteLength(json, 'utf8'));
    }
  }

  /**
   * Read one entry as a record. Ours is always self-written, so a parse failure
   * means an internal bug: count it as dropped and forget the row rather than
   * stalling every later record, matching the durable queue's corrupt-row path.
   */
  private parse(entry: Entry): TelemetryRecord | null {
    try {
      return JSON.parse(entry.json) as TelemetryRecord;
    } catch {
      this.remove(entry);
      this.droppedSinceOpen += 1;
      return null;
    }
  }

  private gapCount(): number {
    return this.entries.reduce((count, entry) => (entry.kind === 'gap' ? count + 1 : count), 0);
  }
}

function sumBytes(entries: readonly Entry[]): number {
  return entries.reduce((total, entry) => total + entry.bytes, 0);
}
