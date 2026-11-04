/**
 * Unit tests for the durable per-destination outbox.
 *
 * Uses node:sqlite in temporary directories. Covers the append/take/ack round
 * trip, restart recovery of records from an older run, in-flight protection,
 * age/byte/shared-budget eviction with coalesced gap records, and write
 * failures that must never reach the sampling loop.
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { SQLInputValue, StatementSync } from 'node:sqlite';

import {
  DEFAULT_OUTBOX_LIMITS,
  DestinationOutbox,
  outboxDirFor,
} from '../collector/destinations/outbox';
import type { DestinationOutboxOptions } from '../collector/destinations/outbox';
import { OutboxBudget } from '../collector/destinations/budget';
import type {
  DeliveryStatus,
  GapRecord,
  MetricsPayload,
  MetricsRecord,
  TelemetryRecord,
} from '../src/contracts';
import { parseCollectorMessage } from '../src/contracts';
import { createMetricsFilter } from '../collector/destinations/policy';

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const ROOT = mkdtempSync(join(tmpdir(), 'rigscope-outbox-test-'));
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

function iso(ms: number): string {
  return new Date(ms).toISOString();
}

function metrics(sequence: number, capturedAtMs: number, runId = 'run-a'): MetricsRecord {
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

function freshDir(name: string): string {
  return mkdtempSync(join(ROOT, `${name}-`));
}

type Opened = {
  dir: string;
  outbox: DestinationOutbox;
  nextSequence: () => number;
};

function openOutbox(
  name: string,
  overrides: Partial<DestinationOutboxOptions> = {},
  limits: Partial<DestinationOutboxOptions['limits']> = {},
): Opened {
  const dir = overrides.dir ?? freshDir(name);
  let sequence = 1_000;
  const allocateSequence = overrides.allocateSequence ?? (() => ++sequence);
  const outbox = DestinationOutbox.open({
    identity: IDENTITY,
    ...overrides,
    dir,
    limits: { ...limits, ...overrides.limits },
    allocateSequence,
  });
  return { dir, outbox, nextSequence: () => ++sequence };
}

function replay(outbox: DestinationOutbox): TelemetryRecord[] {
  return outbox.takeReplay(1_000, Number.MAX_SAFE_INTEGER);
}

function gapRecords(records: TelemetryRecord[]): GapRecord[] {
  return records.filter((record): record is GapRecord => record.kind === 'gap');
}

function status(outbox: DestinationOutbox): DeliveryStatus {
  return outbox.status();
}

// ---------------------------------------------------------------------------
// Location
// ---------------------------------------------------------------------------

test('outboxDirFor: stable sha256-derived per-destination directory', () => {
  const a = outboxDirFor('/state', 'ws://127.0.0.1:9000/collect');
  const b = outboxDirFor('/state', 'ws://127.0.0.1:9000/collect');
  const c = outboxDirFor('/state', 'wss://hub.example/collect');
  assert.equal(a, b);
  assert.notEqual(a, c);
  const leaf = a.split('/').at(-1)!;
  assert.match(leaf, /^[0-9a-f]{16}$/);
  assert.ok(a.startsWith(join('/state', 'outbox')));
});

// ---------------------------------------------------------------------------
// Round trip and restart
// ---------------------------------------------------------------------------

test('append / takeLive / takeReplay / acknowledge round trip', () => {
  const { outbox } = openOutbox('roundtrip');
  try {
    const r1 = metrics(1, T0);
    const r2 = metrics(2, T0 + 1_000);
    outbox.append(r1);
    outbox.append(r2);

    const initial = status(outbox);
    assert.equal(initial.pendingRecords, 2);
    assert.ok(initial.pendingBytes > 0);
    assert.equal(initial.oldestPendingCapturedAt, r1.capturedAt);
    assert.equal(initial.inFlightRecords, 0);
    assert.equal(initial.droppedRecords, 0);
    assert.equal(initial.lastAckAt, null);
    assert.equal(initial.replaying, true);

    const live = outbox.takeLive();
    assert.deepEqual(live, r2, 'live is the most recently appended metrics record');
    assert.equal(outbox.takeLive(), null, 'the same record is never live twice');

    const ref1 = { runId: r1.runId, sequence: r1.sequence };
    const ref2 = { runId: r2.runId, sequence: r2.sequence };
    outbox.markInFlight([ref1, ref2]);
    assert.equal(status(outbox).inFlightRecords, 2);
    assert.equal(status(outbox).pendingRecords, 0);
    assert.equal(status(outbox).pendingBytes, 0, 'in-flight bytes are not pending bytes');

    outbox.acknowledge([ref1], []);
    assert.equal(status(outbox).inFlightRecords, 1);
    outbox.acknowledge([ref2], []);
    const done = status(outbox);
    assert.equal(done.pendingRecords, 0);
    assert.equal(done.inFlightRecords, 0);
    assert.ok(done.lastAckAt !== null);
    assert.equal(outbox.takeReplay(10, Number.MAX_SAFE_INTEGER).length, 0);

    // Acking twice (lost/duplicated ack) is harmless.
    assert.doesNotThrow(() => outbox.acknowledge([ref1, ref2], []));
    assert.equal(status(outbox).pendingRecords, 0);
  } finally {
    outbox.close();
  }
});

test('takeReplay excludes in-flight records, the live record and respects caps', () => {
  const { outbox } = openOutbox('replay-order');
  try {
    const records = [metrics(1, T0), metrics(2, T0 + 1), metrics(3, T0 + 2)];
    for (const record of records) outbox.append(record);
    const ref1 = { runId: 'run-a', sequence: 1 };
    const ref3 = { runId: 'run-a', sequence: 3 };
    outbox.takeLive(); // drops the live designation for sequence 3
    outbox.markInFlight([ref1]);

    // Sequence 2 is oldest pending; 1 is in flight; 3 is no longer live but is
    // still pending. No live designation exists after takeLive() cleared it.
    assert.deepEqual(
      replay(outbox).map((r) => r.sequence),
      [2, 3],
    );
    outbox.release([ref1]);
    assert.deepEqual(
      replay(outbox).map((r) => r.sequence),
      [1, 2, 3],
    );
    outbox.markInFlight([ref1, ref3]);
    // Count cap.
    assert.deepEqual(
      outbox.takeReplay(1, Number.MAX_SAFE_INTEGER).map((r) => r.sequence),
      [2],
    );
  } finally {
    outbox.close();
  }
});

test('restart recovers pending and in-flight records from a previous run', () => {
  const { dir, outbox } = openOutbox('restart');
  const r1 = metrics(1, T0, 'run-old');
  const r2 = metrics(2, T0 + 1_000, 'run-old');
  const r3 = metrics(3, T0 + 2_000, 'run-old');
  try {
    outbox.append(r1);
    outbox.append(r2);
    outbox.append(r3);
    outbox.takeLive();
    outbox.markInFlight([{ runId: 'run-old', sequence: 2 }]);
    assert.equal(status(outbox).inFlightRecords, 1);
  } finally {
    outbox.close();
  }

  const reopened = DestinationOutbox.open({
    dir,
    identity: { collectorId: 'collector-1', runId: 'run-new', bootId: 'boot-a' },
    allocateSequence: () => 9_999,
  });
  try {
    const recovered = status(reopened);
    assert.equal(recovered.pendingRecords, 3, 'in-flight rows return to pending after a restart');
    assert.equal(recovered.inFlightRecords, 0);
    assert.equal(reopened.takeLive(), null, 'a previous run record is not delivered as live');
    const records = replay(reopened);
    assert.deepEqual(
      records.map((r) => `${r.runId}#${r.sequence}`),
      ['run-old#1', 'run-old#2', 'run-old#3'],
    );
    assert.deepEqual(records[0], r1);
  } finally {
    reopened.close();
  }
});

test('upgrade recovers old URL queues and decodes metrics and offered gaps for the current wire contract', () => {
  const stateDir = freshDir('upgrade');
  const previousDir = outboxDirFor(stateDir, 'wss://hub.example/api/collector/v1/connect');
  const dir = outboxDirFor(stateDir, 'wss://hub.example/api/collector/connect');
  const oldMetrics = {...metrics(1, T0), protocolVersion: 2};
  const gap: GapRecord = {...IDENTITY, sequence: 2, capturedAt: iso(T0), kind: 'gap',
    payload: {fromCapturedAt: iso(T0), toCapturedAt: iso(T0), droppedRecords: 3, reason: 'outbox-age'}};
  const original = DestinationOutbox.open({dir: previousDir, identity: IDENTITY, allocateSequence: () => 100});
  original.append(oldMetrics);
  original.close();
  const db = new DatabaseSync(join(previousDir, 'outbox.sqlite'));
  const json = JSON.stringify({...gap, protocolVersion: 2});
  db.prepare("INSERT INTO outbox_records (run_id,sequence,captured_at,kind,state,gap_reason,offered,bytes,json) VALUES (?,?,?,'gap','inflight',?,1,?,?)")
    .run(gap.runId, gap.sequence, gap.capturedAt, gap.payload.reason, Buffer.byteLength(json), json);
  db.close();

  const reopened = DestinationOutbox.open({dir, previousDir, identity: {...IDENTITY, runId: 'run-new'}, allocateSequence: () => 100});
  try {
    const records = replay(reopened);
    assert.deepEqual(records, [metrics(1, T0), gap]);
    assert.doesNotThrow(() => parseCollectorMessage({type: 'samples', mode: 'replay', records}));
    const filter = createMetricsFilter({url: 'unused', token: 'private', sharing: {cpu: true, memory: false}}, 5000);
    reopened.filterPending(record => filter(record, 0)!);
    const filtered = replay(reopened);
    assert.doesNotThrow(() => parseCollectorMessage({type: 'samples', mode: 'replay', records: filtered}));
    assert.equal((filtered[0] as MetricsRecord).payload.memory.status, 'unavailable');
    assert.deepEqual(filtered[1], gap, 'offered gap identities and payloads survive the upgrade');
    reopened.acknowledge(filtered, []);
    assert.equal(reopened.status().pendingRecords, 0);
  } finally { reopened.close(); }
  const again = DestinationOutbox.open({dir, previousDir, identity: IDENTITY, allocateSequence: () => 100});
  assert.equal(again.status().pendingRecords, 0, 'a completed migration cannot resurrect acknowledged records');
  again.close();
});

test('upgrade merges old and current URL queues without losing records or duplicating identities', () => {
  const stateDir = freshDir('upgrade-merge');
  const previousDir = outboxDirFor(stateDir, 'wss://hub.example/api/collector/v1/connect');
  const dir = outboxDirFor(stateDir, 'wss://hub.example/api/collector/connect');
  for (const [location, sequences] of [[previousDir, [1, 2]], [dir, [2, 3]]] as const) {
    const box = DestinationOutbox.open({dir: location, identity: IDENTITY, allocateSequence: () => 100});
    for (const sequence of sequences) box.append({...metrics(sequence, T0 + sequence), protocolVersion: 2} as MetricsRecord);
    box.close();
  }
  const migrated = DestinationOutbox.open({dir, previousDir, identity: IDENTITY, allocateSequence: () => 100});
  try {
    const records = replay(migrated);
    assert.deepEqual(records.map(r => r.sequence).sort(), [1, 2, 3]);
    assert.doesNotThrow(() => parseCollectorMessage({type: 'samples', mode: 'replay', records}));
    const db = new DatabaseSync(join(dir, 'outbox.sqlite'), {readOnly: true});
    try {
      const accounting = db.prepare('SELECT SUM(bytes) AS accounted, SUM(length(CAST(json AS BLOB))) AS actual FROM outbox_records').get()!;
      assert.equal(migrated.status().pendingBytes, accounting.actual);
      assert.equal(accounting.accounted, accounting.actual);
    } finally { db.close(); }
  } finally { migrated.close(); }
});

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

test('age cap evicts oldest pending records into one coalesced gap record', () => {
  let now = T0;
  const { outbox } = openOutbox('age-gap', { now: () => now }, { maxAgeMs: 1_000 });
  try {
    const r1 = metrics(1, T0);
    const r2 = metrics(2, T0 + 1_000);
    outbox.append(r1);
    outbox.append(r2);

    now = T0 + 5_000;
    const r3 = metrics(3, now);
    outbox.append(r3);
    outbox.enforceLimits();

    let state = status(outbox);
    assert.equal(state.droppedRecords, 2);
    assert.equal(state.pendingRecords, 2, 'new record plus one gap');
    const gaps = gapRecords(replay(outbox));
    assert.equal(gaps.length, 1, 'evictions coalesce into a single gap');
    const gap = gaps[0]!;
    assert.equal(gap.payload.reason, 'outbox-age');
    assert.equal(gap.payload.fromCapturedAt, r1.capturedAt);
    assert.equal(gap.payload.toCapturedAt, r2.capturedAt);
    assert.equal(gap.payload.droppedRecords, 2);
    assert.equal(gap.runId, 'run-a');
    assert.equal(gap.bootId, 'boot-a');
    assert.ok(gap.sequence > 1_000, 'gap records receive a fresh allocated sequence');
    const gapSequence = gap.sequence;

    // Another expired record coalesces into the same pending gap record.
    const r4 = metrics(4, T0 + 2_000);
    outbox.append(r4);
    outbox.enforceLimits();
    state = status(outbox);
    assert.equal(state.droppedRecords, 3);
    const coalesced = gapRecords(replay(outbox));
    assert.equal(coalesced.length, 1);
    assert.equal(coalesced[0]!.sequence, gapSequence, 'coalescing does not allocate a new sequence');
    assert.equal(coalesced[0]!.payload.droppedRecords, 3);
    assert.equal(coalesced[0]!.payload.fromCapturedAt, r1.capturedAt);
    assert.equal(coalesced[0]!.payload.toCapturedAt, r4.capturedAt);
  } finally {
    outbox.close();
  }
});

test('byte cap evicts oldest pending records and respects the cap', () => {
  const { outbox } = openOutbox(
    'byte-gap',
    { now: () => T0 + 10_000 },
    { maxBytes: 1_200, maxAgeMs: 10_000_000, maxGapRecords: 64 },
  );
  try {
    const records = Array.from({ length: 12 }, (_, i) => metrics(i + 1, T0 + i));
    for (const record of records) outbox.append(record);
    const before = outbox.bytes();
    assert.ok(before > 1_200, `expected an over-cap outbox, got ${before}`);

    outbox.enforceLimits();
    assert.ok(outbox.bytes() <= 1_200, `expected bytes <= 1200, got ${outbox.bytes()}`);
    const state = status(outbox);
    assert.ok(state.droppedRecords > 0);

    const recordsLeft = replay(outbox);
    const gaps = gapRecords(recordsLeft);
    assert.equal(gaps.length, 1);
    const gap = gaps[0]!;
    assert.equal(gap.payload.reason, 'outbox-bytes');
    assert.equal(gap.payload.droppedRecords, state.droppedRecords);
    assert.equal(gap.payload.fromCapturedAt, records[0]!.capturedAt);
    assert.ok(recordsLeft.every((record) => record.kind === 'gap' || Date.parse(record.capturedAt) >= T0));
  } finally {
    outbox.close();
  }
});

test('in-flight records are never evicted', () => {
  let now = T0;
  const { outbox } = openOutbox('inflight-protected', { now: () => now }, { maxAgeMs: 1_000 });
  try {
    outbox.append(metrics(1, T0));
    outbox.append(metrics(2, T0 + 100));
    outbox.takeLive();
    outbox.markInFlight([{ runId: 'run-a', sequence: 2 }]);

    now = T0 + 60_000;
    outbox.enforceLimits();
    const state = status(outbox);
    assert.equal(state.inFlightRecords, 1, 'the in-flight record survives expiry');
    assert.equal(state.droppedRecords, 1, 'only the pending record was evicted');
    const remaining = replay(outbox);
    assert.equal(remaining.filter((r) => r.kind === 'metrics').length, 0);
    assert.equal(gapRecords(remaining)[0]?.payload.droppedRecords, 1);

    outbox.release([{ runId: 'run-a', sequence: 2 }]);
    outbox.enforceLimits();
    assert.equal(status(outbox).droppedRecords, 2);
    assert.equal(gapRecords(replay(outbox))[0]?.payload.droppedRecords, 2);
  } finally {
    outbox.close();
  }
});

test('shared budget evicts from the destination using the most bytes', () => {
  // Register after measuring so the cap requires shedding enough bytes that
  // the largest outbox stays largest throughout the eviction.
  const a = openOutbox('budget-a', { now: () => T0 + 10_000 }, { maxAgeMs: 10_000_000, maxBytes: 10_000_000 });
  const b = openOutbox('budget-b', { now: () => T0 + 10_000 }, { maxAgeMs: 10_000_000, maxBytes: 10_000_000 });
  try {
    for (let i = 1; i <= 2; i += 1) a.outbox.append(metrics(i, T0 + i));
    for (let i = 1; i <= 10; i += 1) b.outbox.append(metrics(i, T0 + i));

    const aBytesBefore = a.outbox.bytes();
    const bBytesBefore = b.outbox.bytes();
    assert.ok(bBytesBefore > aBytesBefore);
    const budget = new OutboxBudget(aBytesBefore + bBytesBefore - 1_000);
    budget.register(a.outbox);
    budget.register(b.outbox);

    assert.equal(budget.largest(), b.outbox);
    assert.ok(budget.overBy() > 0);

    // Enforcing on the smaller outbox must still shed the largest one.
    a.outbox.enforceLimits();
    assert.equal(budget.overBy(), 0, 'shared budget is respected');
    assert.ok(b.outbox.bytes() < bBytesBefore, 'largest outbox shed bytes');
    assert.ok(b.outbox.status().droppedRecords > 0);
    assert.equal(a.outbox.status().droppedRecords, 0, 'the small outbox was not the victim');
    assert.equal(a.outbox.bytes(), aBytesBefore);
    assert.ok(gapRecords(replay(b.outbox)).some((gap) => gap.payload.reason === 'disk-budget'));
    b.outbox.close();
    assert.equal(budget.largest(), a.outbox, 'closing unregisters from the budget');
  } finally {
    a.outbox.close();
    b.outbox.close();
  }
});

test('gap records stay within maxGapRecords by merging the oldest gaps', () => {
  let now = T0;
  const fault: Fault = { failWrites: false };
  const { outbox, dir } = openOutbox(
    'gap-cap',
    { now: () => now, openDatabase: faultyDatabase(fault) },
    { maxAgeMs: 1_000, maxGapRecords: 1 },
  );
  try {
    outbox.append(metrics(1, T0));
    now = T0 + 5_000;
    outbox.append(metrics(2, now));
    outbox.enforceLimits();
    assert.equal(gapRecords(replay(outbox)).length, 1, 'age gap exists');

    // A write failure adds a second reason. Once writes recover the
    // write-failure gap is persisted and the cap merges the older age gap.
    fault.failWrites = true;
    outbox.append(metrics(3, now));
    fault.failWrites = false;
    outbox.append(metrics(4, now));

    const gaps = gapRecords(replay(outbox));
    assert.equal(gaps.length, 1, `expected one gap, got ${gaps.length}`);
    assert.equal(gaps[0]!.payload.droppedRecords, 2, 'both dropped records are still counted');
    const reader = new DatabaseSync(join(dir, 'outbox.sqlite'), { readOnly: true });
    try {
      assert.equal(outbox.bytes(), Number(reader.prepare('SELECT COALESCE(SUM(bytes),0) AS n FROM outbox_records').get()!.n),
        'a fresh merged gap replaces two old rows with exact logical byte accounting');
    } finally { reader.close(); }
  } finally {
    outbox.close();
  }
});

// ---------------------------------------------------------------------------
// Write failure
// ---------------------------------------------------------------------------

type Fault = { failWrites: boolean };

function faultyDatabase(fault: Fault) {
  return (file: string): DatabaseSync => {
    const real = new DatabaseSync(file, { timeout: 5_000 });
    const proxy = {
      exec: (sql: string) => real.exec(sql),
      close: () => real.close(),
      prepare: (sql: string) => {
        const statement = real.prepare(sql);
        if (!sql.trimStart().startsWith('INSERT INTO outbox_records')) return statement;
        const wrapped = {
          run: (...args: SQLInputValue[]) => {
            if (fault.failWrites) throw new Error('SQLITE_FULL: database or disk is full');
            return statement.run(...args);
          },
        };
        return wrapped as unknown as StatementSync;
      },
    };
    return proxy as unknown as DatabaseSync;
  };
}

test('write failure keeps a coalesced write-failure gap and never throws', () => {
  const fault: Fault = { failWrites: true };
  const { outbox } = openOutbox(
    'write-failure',
    { openDatabase: faultyDatabase(fault), now: () => T0 + 10_000 },
    { maxAgeMs: 10_000_000 },
  );
  try {
    assert.doesNotThrow(() => outbox.append(metrics(1, T0)));
    assert.doesNotThrow(() => outbox.append(metrics(2, T0 + 1_000)));
    let state = status(outbox);
    assert.equal(state.pendingRecords, 0);
    assert.equal(state.droppedRecords, 2);
    assert.equal(outbox.bytes(), 0);

    // Writes recover: the next successful append first persists the gap.
    fault.failWrites = false;
    outbox.append(metrics(3, T0 + 2_000));
    state = status(outbox);
    assert.equal(state.pendingRecords, 2, 'gap plus the new record');
    const gaps = gapRecords(replay(outbox));
    assert.equal(gaps.length, 1);
    assert.equal(gaps[0]!.payload.reason, 'write-failure');
    assert.equal(gaps[0]!.payload.droppedRecords, 2);
    assert.equal(gaps[0]!.payload.fromCapturedAt, iso(T0));
    assert.equal(gaps[0]!.payload.toCapturedAt, iso(T0 + 1_000));
    assert.deepEqual(outbox.takeLive(), metrics(3, T0 + 2_000));
  } finally {
    outbox.close();
  }
});

// ---------------------------------------------------------------------------
// Limits defaults
// ---------------------------------------------------------------------------

test('defaults match the documented prototype caps', () => {
  assert.equal(DEFAULT_OUTBOX_LIMITS.maxAgeMs, 600_000);
  assert.equal(DEFAULT_OUTBOX_LIMITS.maxBytes, 64 * 1024 * 1024);
  assert.equal(DEFAULT_OUTBOX_LIMITS.maxGapRecords, 64);
  const budget = new OutboxBudget();
  assert.equal(budget.overBy(), 0);
  assert.equal(budget.largest(), null);
});

test('lost gap ack: released gap payload is immutable when more telemetry is evicted', () => {
  let now = T0 + 10000;
  const { outbox } = openOutbox('immutable-gap', { now: () => now }, { maxAgeMs: 1000 });
  try {
    outbox.append(metrics(1, T0)); outbox.enforceLimits();
    const first = gapRecords(replay(outbox))[0]!;
    const before = JSON.stringify(first);
    outbox.markInFlight([{ runId: first.runId, sequence: first.sequence }]);
    // Hub commits first gap, but its ack is lost; timeout/reconnect releases it.
    outbox.releaseAll();
    now += 1000;
    outbox.append(metrics(2, T0 + 500)); outbox.enforceLimits();
    const after = gapRecords(replay(outbox));
    assert.equal(JSON.stringify(after.find(g => g.runId === first.runId && g.sequence === first.sequence)), before,
      'an already offered dedup key must never change payload');
    assert.equal(after.reduce((sum, g) => sum + g.payload.droppedRecords, 0), 2);
  } finally { outbox.close(); }
});

test('pending telemetry is re-filtered before a reduced sharing policy can transmit it', () => {
  const { outbox } = openOutbox('privacy-reduction');
  try {
    outbox.append(metrics(1, T0));
    outbox.filterPending(record => ({ ...record, payload: { ...record.payload,
      cpu: { status: 'unavailable', reason: 'withheld by local sharing policy' } } }));
    const record = outbox.takeLive()!;
    assert.equal(record.kind, 'metrics');
    if (record.kind === 'metrics') assert.equal(record.payload.cpu.status, 'unavailable');
  } finally { outbox.close(); }
});

test('replay materializes only the requested batch even with a large backlog', () => {
  let observed = 0;
  const { outbox } = openOutbox('bounded-replay', { now: () => T0,
    openDatabase: file => {
      const db = new DatabaseSync(file);
      return new Proxy(db, { get(target, key) {
        if (key === 'prepare') return (sql: string) => {
          const statement = target.prepare(sql);
          if (!sql.startsWith('SELECT * FROM outbox_records WHERE state')) return statement;
          return new Proxy(statement, { get(stmt, field) {
            if (field === 'all') return (...args: SQLInputValue[]) => {
              const rows = stmt.all(...args); observed = rows.length; return rows;
            };
            const value = Reflect.get(stmt, field); return typeof value === 'function' ? value.bind(stmt) : value;
          } });
        };
        const value = Reflect.get(target, key); return typeof value === 'function' ? value.bind(target) : value;
      } });
    },
  });
  try {
    for (let i=1; i<=500; i++) outbox.append(metrics(i, T0));
    const batch = outbox.takeReplay(32, Number.MAX_SAFE_INTEGER);
    assert.equal(batch.length, 32);
    assert.equal(observed, 32);
    assert.equal(batch[0].sequence, 1);
    assert.equal(batch.at(-1)!.sequence, 32);
  } finally { outbox.close(); }
});
