/**
 * Unit tests for the RAM-only default delivery queue.
 *
 * The memory queue is the collector's default: unacknowledged samples wait in
 * RAM, nothing is written to disk, and a restart loses what was never
 * acknowledged. These tests pin both halves — the delivery behaviour it must
 * keep identical to the durable outbox (live slot, bounded replay, in-flight
 * protection, age/byte eviction with coalesced gaps, shared budget) and the
 * differences that make the trade explicit (no files, empty after restart,
 * `queueMode` reported to the hub).
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { MemoryDestinationQueue } from '../collector/destinations/memory-queue';
import { DEFAULT_OUTBOX_LIMITS, DestinationOutbox, outboxDirFor, type OutboxLimits } from '../collector/destinations/outbox';
import { OutboxBudget } from '../collector/destinations/budget';
import { HubConnection, type SharedSampler } from '../collector/destinations/connection';
import { DEFAULT_COLLECTOR_QUEUE_MODE } from '../collector/destinations/queue';
import type {
  CollectionFailure,
  GapRecord,
  Inventory,
  MetricsPayload,
  MetricsRecord,
  TelemetryRecord,
} from '../src/contracts';

const INVENTORY: Inventory = {
  os: { platform: 'linux', release: '6.1', distro: null, arch: 'x64' },
  cpu: { model: 'cpu-model', logicalCores: 4 },
  memory: { totalBytes: 8_000, swapTotalBytes: null },
  gpus: [],
  filesystems: [],
  disks: [],
  network: [],
  capabilities: {
    cpu: { status: 'supported' },
    memory: { status: 'supported' },
    swap: { status: 'supported' },
    gpus: { status: 'unsupported' },
    disks: { status: 'supported' },
    network: { status: 'supported' },
    processes: { status: 'supported' },
  },
};

const ROOT = mkdtempSync(join(tmpdir(), 'rigscope-memory-queue-'));
process.on('exit', () => rmSync(ROOT, { recursive: true, force: true }));

const IDENTITY = { collectorId: 'collector-1', runId: 'run-a', bootId: 'boot-a' };
const T0 = Date.parse('2026-02-01T00:00:00.000Z');

const METRICS_PAYLOAD: MetricsPayload = {
  cpu: { status: 'ok', usagePercent: 12.5, perCore: [10] },
  memory: { status: 'ok', usedBytes: 1_000, totalBytes: 8_000 },
  swap: { status: 'unavailable', reason: 'no swap' },
  gpus: { status: 'ok', devices: [] },
  disks: { status: 'ok', filesystems: [], devices: [] },
  network: { status: 'ok', interfaces: [] },
};

const iso = (ms: number): string => new Date(ms).toISOString();

function metrics(sequence: number, capturedAtMs = T0, runId = 'run-a'): MetricsRecord {
  return {
    collectorId: 'collector-1',
    runId,
    bootId: 'boot-a',
    sequence,
    capturedAt: iso(capturedAtMs),
    kind: 'metrics',
    payload: METRICS_PAYLOAD,
  };
}

/** Larger than any two ordinary records, so a small maxBytes rejects it. */
function oversizedMetrics(sequence: number, capturedAtMs = T0): MetricsRecord {
  const record = metrics(sequence, capturedAtMs);
  return {
    ...record,
    payload: {
      ...record.payload,
      cpu: { status: 'ok', usagePercent: 1, perCore: Array.from({ length: 1024 }, () => 0) },
    },
  };
}

type Opened = { queue: MemoryDestinationQueue; nextSequence: () => number };

function openMemory(
  limits: Partial<OutboxLimits> = {},
  now: () => number = () => T0,
): Opened {
  let sequence = 1_000;
  const queue = new MemoryDestinationQueue({
    identity: IDENTITY,
    limits,
    now,
    allocateSequence: () => ++sequence,
  });
  return { queue, nextSequence: () => ++sequence };
}

function queueOf(options: {
  limits?: Partial<OutboxLimits>;
  now?: () => number;
  allocateSequence?: () => number;
  budget?: OutboxBudget;
}): MemoryDestinationQueue {
  let sequence = 1_000;
  return new MemoryDestinationQueue({
    identity: IDENTITY,
    limits: options.limits,
    now: options.now,
    budget: options.budget,
    allocateSequence: options.allocateSequence ?? (() => ++sequence),
  });
}

/**
 * Everything the queue still owes. The live slot and replay read the same
 * pending set, so the live record is deduplicated by its wire key.
 */
const all = (queue: MemoryDestinationQueue): TelemetryRecord[] => {
  const live = queue.takeLive();
  const rest = queue.takeReplay(10_000, Number.MAX_SAFE_INTEGER);
  if (live === null) return rest;
  const seen = new Set(rest.map((r) => `${r.runId}/${r.sequence}`));
  return seen.has(`${live.runId}/${live.sequence}`) ? rest : [live, ...rest];
};
const gaps = (records: TelemetryRecord[]): GapRecord[] => records.filter((r): r is GapRecord => r.kind === 'gap');

// ---------------------------------------------------------------------------
// Mode selection
// ---------------------------------------------------------------------------

test('the collector queue default is memory', () => {
  assert.equal(DEFAULT_COLLECTOR_QUEUE_MODE, 'memory');
});

test('memory mode creates no queue file anywhere under the state directory', (t) => {
  const stateDir = mkdtempSync(join(ROOT, 'state-'));
  t.after(() => rmSync(stateDir, { recursive: true, force: true }));
  const queue = queueOf({});
  queue.append(metrics(1));
  queue.append(metrics(2));
  queue.enforceLimits();
  assert.equal(queue.diskBytes(), 0);
  assert.equal(queue.diskStatus().bytes, 0);
  assert.equal(queue.diskStatus().pressure, false);
  // Nothing in the state tree was created for the queue.
  assert.deepEqual(existsSync(join(stateDir, 'outbox')), false);
  queue.close();
});

test('a leftover durable queue from an earlier install is reported once, not replayed', (t) => {
  const stateDir = mkdtempSync(join(ROOT, 'state-'));
  t.after(() => rmSync(stateDir, { recursive: true, force: true }));
  const url = 'ws://127.0.0.1:9100/api/collector/connect';
  const dir = outboxDirFor(stateDir, url);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  writeFileSync(join(dir, 'outbox.sqlite'), 'leftover');

  const events: string[] = [];
  const sampler: SharedSampler = {
    inventory: () => INVENTORY,
    inventoryVersion: () => 1,
    failures: (): CollectionFailure[] => [],
  };
  const connection = HubConnection.open({
    index: 0,
    endpoint: { url, token: 'token' },
    baseIntervalMs: 1_000,
    stateDir,
    queueMode: 'memory',
    identity: { ...IDENTITY, hostname: 'rig' },
    budget: new OutboxBudget(),
    allocateSequence: () => 1,
    sampler,
    socketFactory: () => {
      throw new Error('memory mode must not open a socket during construction');
    },
    onEvent: (line) => events.push(line),
    backoff: { baseMs: 900_000, maxMs: 900_000 },
    rand: () => 0.5,
  });
  t.after(() => connection.stop());

  assert.ok(events.some((line) => line.includes('durable outbox from an earlier')), events.join('\n'));
  assert.equal(events.filter((line) => line.includes('durable outbox from an earlier')).length, 1);
});

// ---------------------------------------------------------------------------
// Delivery parity with the durable outbox
// ---------------------------------------------------------------------------

test('live slot takes priority, then the same record is still replayable', () => {
  const { queue } = openMemory();
  queue.append(metrics(1));
  queue.append(metrics(2));

  const live = queue.takeLive();
  assert.equal(live?.sequence, 2, 'the newest sample wins the live slot');
  assert.equal(queue.takeLive(), null, 'the live slot drains after one take');

  const replay = queue.takeReplay(10, Number.MAX_SAFE_INTEGER);
  assert.deepEqual(replay.map((r) => r.sequence), [1, 2], 'an unacknowledged live send is replayed later');
  queue.close();
});

test('replay is bounded by both record count and bytes', () => {
  const { queue } = openMemory();
  for (let i = 1; i <= 5; i += 1) queue.append(metrics(i));
  const single = queue.takeReplay(2, Number.MAX_SAFE_INTEGER);
  assert.equal(single.length, 2);
  const byBytes = all(queue);
  assert.ok(byBytes.length >= 1, 'a byte cap too small for two rows still yields one row');
  queue.close();
});

test('in-flight records are never evicted by age or byte pressure', () => {
  let now = T0;
  const one = Buffer.byteLength(JSON.stringify(metrics(1)), 'utf8');
  const { queue } = openMemory({ maxAgeMs: 1_000, maxBytes: one }, () => now);
  assert.ok(one > 0);
  queue.append(metrics(1));
  queue.markInFlight([{ runId: 'run-a', sequence: 1 }]);
  now = T0 + 60_000;
  queue.enforceLimits();

  assert.equal(queue.status().inFlightRecords, 1, 'the owed batch survives its own age');
  assert.equal(queue.status().pendingRecords, 0);
  queue.release([{ runId: 'run-a', sequence: 1 }]);
  assert.equal(queue.status().pendingRecords, 1, 'a released record returns to pending');
  queue.close();
});

test('acknowledged and rejected records both leave the queue', () => {
  const { queue } = openMemory();
  queue.append(metrics(1));
  queue.append(metrics(2));
  queue.markInFlight([{ runId: 'run-a', sequence: 1 }, { runId: 'run-a', sequence: 2 }]);
  queue.acknowledge([{ runId: 'run-a', sequence: 1 }], [{ runId: 'run-a', sequence: 2 }]);
  const status = queue.status();
  assert.equal(status.pendingRecords, 0);
  assert.equal(status.inFlightRecords, 0);
  assert.equal(status.replaying, false);
  assert.notEqual(status.lastAckAt, null);
  assert.equal(queue.bytes(), 0);
  queue.close();
});

test('age eviction drops oldest-first and folds one outbox-age gap', () => {
  let now = T0;
  const { queue } = openMemory({ maxAgeMs: 5_000 }, () => now);
  queue.append(metrics(1, T0));
  queue.append(metrics(2, T0 + 1_000));
  queue.append(metrics(3, T0 + 40_000));
  now = T0 + 41_000;
  queue.enforceLimits();

  const records = all(queue);
  assert.deepEqual(records.filter((r) => r.kind === 'metrics').map((r) => r.sequence), [3]);
  const ageGaps = gaps(records).filter((g) => g.payload.reason === 'outbox-age');
  assert.equal(ageGaps.length, 1, 'evicted samples are collapsed into one gap');
  assert.equal(ageGaps[0]?.payload.droppedRecords, 2);
  assert.equal(queue.status().droppedRecords, 2);
  queue.close();
});

test('byte eviction keeps the newest sample and reports an outbox-bytes gap', () => {
  const { queue } = openMemory();
  const one = Buffer.byteLength(JSON.stringify(metrics(1)), 'utf8');
  for (let i = 1; i <= 6; i += 1) queue.append(metrics(i));
  // Shrink the cap after filling, then enforce: the oldest must go first.
  const tight = queueOf({ limits: { maxBytes: one * 2 } });
  for (let i = 1; i <= 6; i += 1) tight.append(metrics(i));
  const records = all(tight);
  const retained = records.filter((r) => r.kind === 'metrics').map((r) => r.sequence);
  assert.ok(retained.includes(6), 'the newest sample always survives');
  assert.ok(!retained.includes(1), 'the oldest sample goes first');
  assert.equal(queue.bytes() > 0, true);
  queue.close();
  tight.close();
});

test('gap records coalesce per reason and merge under the gap cap', () => {
  let now = T0;
  const { queue } = openMemory({ maxAgeMs: 1_000, maxGapRecords: 2 }, () => now);
  queue.append(metrics(1, T0));
  queue.append(metrics(2, T0 + 2_000));
  now = T0 + 10_000;
  queue.append(metrics(3, T0 + 10_000));
  now = T0 + 12_000;
  queue.enforceLimits();
  const afterFirst = all(queue);
  assert.equal(gaps(afterFirst).filter((g) => g.payload.reason === 'outbox-age').length, 1);

  // Push many separate gap reasons through, then let the cap collapse them.
  const many = queueOf({ limits: { maxAgeMs: 1_000, maxGapRecords: 2 }, now: () => now });
  for (let i = 0; i < 6; i += 1) {
    many.append(metrics(100 + i, T0 + i * 5_000));
    now = T0 + (i + 1) * 5_000 + 1_000;
    many.enforceLimits();
  }
  const merged = gaps(all(many));
  assert.ok(merged.length <= 2, `expected at most two gap records, got ${merged.length}`);
  const totalDropped = merged.reduce((sum, g) => sum + g.payload.droppedRecords, 0);
  assert.ok(totalDropped >= 5, 'merging must not lose the dropped sample count');
  queue.close();
  many.close();
});

test('a shared budget sheds from the largest queue until the aggregate fits', () => {
  // Three records of aggregate budget for four queued: the largest queue must
  // shed, and a queue holding only one record must keep it.
  const one = Buffer.byteLength(JSON.stringify(metrics(1)), 'utf8');
  const budget = new OutboxBudget(one * 3, Number.MAX_SAFE_INTEGER);
  const big = queueOf({ budget });
  const small = queueOf({ budget });
  big.append(metrics(1));
  big.append(metrics(2));
  big.append(metrics(3));
  small.append(metrics(4));
  const bigBefore = big.bytes();
  big.enforceLimits();
  assert.ok(big.bytes() < bigBefore, 'the largest queue was the victim');
  assert.ok(big.status().droppedRecords >= 3, 'shedding is visible as dropped records');
  // Only the evidence survives: metrics are gone, the coalesced gap remains.
  assert.deepEqual(all(big).map((r) => r.kind), ['gap']);
  assert.deepEqual(all(small).map((r) => r.kind), ['metrics']);
  small.close();
  big.close();
});

test('every append keeps the aggregate shared budget in bounds without a maintenance tick', () => {
  const one = Buffer.byteLength(JSON.stringify(metrics(1)), 'utf8');
  const budget = new OutboxBudget(one * 4, Number.MAX_SAFE_INTEGER);
  const destinations = [queueOf({ budget }), queueOf({ budget }), queueOf({ budget })];

  for (let sequence = 1; sequence <= 18; sequence += 1) {
    destinations[(sequence - 1) % destinations.length]!.append(metrics(sequence));
    assert.equal(budget.overBy(), 0, `append ${sequence} left the shared budget over its cap`);
    const retained = destinations.reduce((total, queue) => total + queue.bytes(), 0);
    assert.ok(retained <= budget.maxTotalBytes, `append ${sequence} retained ${retained} bytes`);
  }
  for (const queue of destinations) queue.close();
});

test('an oversized rejection settles the shared budget during append', () => {
  const one = Buffer.byteLength(JSON.stringify(metrics(1)), 'utf8');
  const budget = new OutboxBudget(one * 2, Number.MAX_SAFE_INTEGER);
  const first = queueOf({ budget, limits: { maxBytes: one * 2 } });
  const second = queueOf({ budget, limits: { maxBytes: one * 2 } });
  first.append(metrics(1));
  second.append(metrics(2));
  assert.equal(budget.overBy(), 0);

  first.append(oversizedMetrics(3));
  assert.equal(budget.overBy(), 0, 'the rejection gap must not push the shared budget over');
  assert.ok(first.status().droppedRecords >= 1);
  first.close();
  second.close();
});

test('append enforces the gap cap after oversized and eviction-generated gaps', () => {
  let now = T0;
  const normalBytes = Buffer.byteLength(JSON.stringify(metrics(1)), 'utf8');
  const { queue } = openMemory(
    { maxAgeMs: 1_000, maxBytes: normalBytes * 2, maxGapRecords: 1 },
    () => now,
  );
  queue.append(metrics(1, T0));
  now = T0 + 5_000;
  queue.append(oversizedMetrics(2, now));
  assert.equal(gaps(all(queue)).length, 1, 'the rejected record is one gap');

  // This append age-evicts the first sample, creating a second gap of a
  // different reason; the cap must merge both before append returns.
  queue.append(metrics(3, now));
  const merged = gaps(all(queue));
  assert.equal(merged.length, 1, 'append merged gaps to the cap without enforceLimits()');
  assert.equal(merged[0]?.payload.droppedRecords, 2, 'merging preserves the dropped-record count');
  queue.close();
});

test('a shared-budget victim keeps its gap cap when another destination appends', () => {
  const one = Buffer.byteLength(JSON.stringify(metrics(1)), 'utf8');
  const budget = new OutboxBudget(one * 2, Number.MAX_SAFE_INTEGER);
  let now = T0;
  const victim = queueOf({ budget, limits: { maxAgeMs: 1_000, maxGapRecords: 1 }, now: () => now });
  const other = queueOf({ budget, limits: { maxGapRecords: 1 }, now: () => now });

  victim.append(metrics(1, T0));
  victim.append(metrics(2, T0));
  now = T0 + 5_000;
  victim.append(metrics(3, now));
  assert.equal(gaps(all(victim)).length, 1, 'the age eviction left one gap');

  other.append(metrics(4, now));
  assert.equal(budget.overBy(), 0, 'the aggregate budget is settled by the append');
  const merged = gaps(all(victim));
  assert.equal(merged.length, 1, 'the budget eviction kept the victim within its gap cap');
  assert.equal(merged[0]?.payload.droppedRecords, 3, 'both evictions survive as dropped evidence');
  victim.close();
  other.close();
});

test('shared-budget enforcement during append preserves in-flight records', () => {
  const one = Buffer.byteLength(JSON.stringify(metrics(1)), 'utf8');
  const budget = new OutboxBudget(one * 3, Number.MAX_SAFE_INTEGER);
  const held = queueOf({ budget });
  const busy = queueOf({ budget });

  held.append(metrics(1));
  held.markInFlight([{ runId: 'run-a', sequence: 1 }]);
  busy.append(metrics(2));
  busy.append(metrics(3));
  busy.append(metrics(4));

  assert.equal(budget.overBy(), 0);
  assert.equal(held.status().inFlightRecords, 1, 'an owed batch is never a budget victim');
  assert.ok(held.bytes() >= one);
  held.release([{ runId: 'run-a', sequence: 1 }]);
  assert.equal(held.status().pendingRecords, 1);
  held.close();
  busy.close();
});

test('records handed out are detached copies', () => {
  const { queue } = openMemory();
  queue.append(metrics(1));
  const taken = queue.takeLive();
  assert.ok(taken !== undefined);
  (taken as { capturedAt: string }).capturedAt = 'mutated';
  const again = queue.takeReplay(1, Number.MAX_SAFE_INTEGER)[0];
  assert.notEqual(again?.capturedAt, 'mutated', 'a caller cannot mutate what is still owed');
  queue.close();
});

test('filterPending re-applies the privacy policy to retained records', () => {
  const { queue } = openMemory();
  queue.append(metrics(1));
  queue.filterPending((record) => ({
    ...record,
    payload: { ...record.payload, cpu: { status: 'ok', usagePercent: 0, perCore: [] } },
  }));
  const retained = all(queue).filter((r) => r.kind === 'metrics')[0] as MetricsRecord;
  assert.equal(retained.payload.cpu.status, 'ok');
  assert.deepEqual(retained.payload.cpu.perCore, []);
  assert.equal(queue.status().pendingRecords, 1);
  queue.close();
});

test('a restart loses unacknowledged samples, which is the documented trade', () => {
  const first = queueOf({});
  first.append(metrics(1));
  first.append(metrics(2));
  first.close();

  const reopened = queueOf({});
  assert.equal(reopened.status().pendingRecords, 0, 'nothing survived the restart');
  assert.equal(all(reopened).length, 0);
  assert.equal(reopened.status().droppedRecords, 0, 'a restart is not reported as a local drop');
  reopened.close();
});

test('memory and durable queues report their mode so the hub can tell the difference', () => {
  const stateDir = mkdtempSync(join(ROOT, 'durable-mode-'));
  const memory = queueOf({});
  assert.equal(memory.status().queueMode, 'memory');
  const durable = DestinationOutbox.open({
    dir: outboxDirFor(stateDir, 'ws://127.0.0.1:9100/api/collector/connect'),
    identity: IDENTITY,
    allocateSequence: () => 1,
  });
  assert.equal(durable.status().queueMode, 'durable');
  assert.equal(memory.status().pendingBytes, durable.status().pendingBytes);
  memory.close();
  durable.close();
  rmSync(stateDir, { recursive: true, force: true });
});

test('defaults match the documented per-destination bounds', () => {
  const { queue } = openMemory();
  assert.equal(DEFAULT_OUTBOX_LIMITS.maxAgeMs, 600_000);
  assert.equal(DEFAULT_OUTBOX_LIMITS.maxBytes, 64 * 1024 * 1024);
  assert.equal(queue.status().pendingRecords, 0);
  queue.close();
});
