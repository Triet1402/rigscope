/**
 * Physical disk-bound regression tests for the collector outboxes.
 *
 * These use isolated temporary files and measure real main+WAL bytes, not the
 * logical JSON byte counter. Caps here are intentionally tiny so the tests can
 * reach pressure quickly while keeping the production defaults separate.
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { SQLInputValue, StatementSync } from 'node:sqlite';

import {
  DEFAULT_OUTBOX_DISK_BYTES,
  DEFAULT_OUTBOX_LIMITS,
  DestinationOutbox,
} from '../collector/destinations/outbox';
import type { DestinationOutboxOptions } from '../collector/destinations/outbox';
import {
  DEFAULT_MAX_TOTAL_OUTBOX_DISK_BYTES,
  OutboxBudget,
} from '../collector/destinations/budget';
import { makeMetricsRecord, makePayload } from './storage-fixtures';
import type { MetricsRecord, TelemetryRecord } from '../src/contracts';

const ROOT = mkdtempSync(join(tmpdir(), 'rigscope-outbox-bounds-'));
process.on('exit', () => rmSync(ROOT, { recursive: true, force: true }));

const T0 = Date.parse('2026-11-02T12:00:00Z');
const IDENTITY = { collectorId: 'collector-1', runId: 'run-a', bootId: 'boot-a' };

const LARGE_PAYLOAD = makePayload({
  cores: 16,
  gpus: Array.from({ length: 8 }, (_, index) => ({
    id: `GPU-${index}`,
    utilization: index * 10,
    memoryUsed: 1_000,
    memoryTotal: 2_000,
  })),
  filesystems: Array.from({ length: 16 }, (_, index) => ({
    id: `fs-${index}`,
    used: 1,
    total: 2,
  })),
  disks: Array.from({ length: 4 }, (_, index) => ({ id: `disk-${index}` })),
  interfaces: Array.from({ length: 8 }, (_, index) => ({ id: `eth-${index}` })),
});

function metric(sequence: number, capturedAtMs = T0 + sequence * 1_000): MetricsRecord {
  return makeMetricsRecord({
    collectorId: IDENTITY.collectorId,
    runId: IDENTITY.runId,
    sequence,
    capturedAt: capturedAtMs,
    payload: LARGE_PAYLOAD,
  });
}

function freshDir(name: string): string {
  return mkdtempSync(join(ROOT, `${name}-`));
}

function openOutbox(
  name: string,
  options: Partial<DestinationOutboxOptions> = {},
  limits: Partial<DestinationOutboxOptions['limits']> = {},
): { dir: string; outbox: DestinationOutbox } {
  const dir = options.dir ?? freshDir(name);
  const outbox = DestinationOutbox.open({
    identity: IDENTITY,
    allocateSequence: () => sequence++,
    now: () => T0,
    ...options,
    dir,
    limits: {
      maxAgeMs: 100_000_000,
      maxBytes: 10_000_000,
      maxDiskBytes: 512 * 1024,
      ...limits,
    },
  });
  return { dir, outbox };
}

let sequence = 1_000;

function replay(outbox: DestinationOutbox): TelemetryRecord[] {
  return outbox.takeReplay(10_000, Number.MAX_SAFE_INTEGER);
}

test('documented finite physical defaults coexist with the logical caps', () => {
  assert.equal(DEFAULT_OUTBOX_LIMITS.maxBytes, 64 * 1024 * 1024);
  assert.equal(DEFAULT_OUTBOX_LIMITS.maxDiskBytes, DEFAULT_OUTBOX_DISK_BYTES);
  assert.equal(DEFAULT_OUTBOX_DISK_BYTES, 80 * 1024 * 1024);
  assert.equal(DEFAULT_MAX_TOTAL_OUTBOX_DISK_BYTES, 640 * 1024 * 1024);
  const budget = new OutboxBudget();
  assert.equal(budget.maxTotalBytes, 256 * 1024 * 1024);
  assert.equal(budget.maxTotalDiskBytes, DEFAULT_MAX_TOTAL_OUTBOX_DISK_BYTES);
});

test('per-destination physical cap bounds measured main+WAL bytes', () => {
  const limit = 512 * 1024;
  const { dir, outbox } = openOutbox('physical-cap', {}, { maxDiskBytes: limit });
  try {
    for (let index = 1; index <= 2_000; index += 1) {
      outbox.append(metric(index));
      if (index % 250 === 0) outbox.enforceLimits();
    }
    outbox.enforceLimits();

    const status = outbox.diskStatus();
    assert.ok(status.bytes <= limit, `expected ${status.bytes} <= ${limit}`);
    assert.equal(status.overBy, 0);
    assert.ok(status.walBytes <= status.walBudgetBytes, 'WAL stays inside its reserve after checkpointing');
    assert.ok(outbox.status().droppedRecords > 0, 'pressure dropped old telemetry rather than growing forever');
    assert.ok(outbox.status().pendingRecords > 0);
    assert.equal(statSync(join(dir, 'outbox.sqlite')).size, status.mainBytes);
  } finally {
    outbox.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test('current append is prioritized over older pending telemetry under pressure', () => {
  const limit = 384 * 1024;
  const { dir, outbox } = openOutbox('current-priority', {}, { maxDiskBytes: limit });
  try {
    for (let index = 1; index <= 1_000; index += 1) {
      outbox.append(metric(index));
      if (index % 200 === 0) outbox.enforceLimits();
    }
    outbox.enforceLimits();
    const before = outbox.diskStatus();
    assert.ok(before.bytes <= limit);

    const newest = metric(99_999, T0 + 20_000);
    outbox.append(newest);
    const live = outbox.takeLive();
    assert.equal(live?.kind, 'metrics');
    if (live?.kind === 'metrics') assert.equal(live.sequence, 99_999);
    assert.ok(outbox.diskStatus().bytes <= limit);
    assert.ok(outbox.status().droppedRecords > 0);
  } finally {
    outbox.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test('WAL stays bounded after sustained append and acknowledge', () => {
  const limit = 1024 * 1024;
  const { dir, outbox } = openOutbox('wal-bound', {}, { maxDiskBytes: limit });
  try {
    const refs = [];
    for (let index = 1; index <= 800; index += 1) {
      const record = makeMetricsRecord({
        collectorId: IDENTITY.collectorId,
        runId: IDENTITY.runId,
        sequence: index,
        capturedAt: T0 + index * 1_000,
        payload: makePayload(),
      });
      outbox.append(record);
      refs.push({ runId: record.runId, sequence: record.sequence });
    }
    outbox.enforceLimits();
    outbox.markInFlight(refs);
    outbox.acknowledge(refs, []);
    outbox.enforceLimits();
    outbox.checkpoint();

    const status = outbox.diskStatus();
    assert.equal(outbox.status().pendingRecords, 0);
    assert.ok(status.bytes <= limit);
    assert.ok(status.walBytes <= status.walBudgetBytes, `WAL ${status.walBytes} exceeds ${status.walBudgetBytes}`);
  } finally {
    outbox.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test('shared global physical budget bounds multiple destinations', () => {
  const budget = new OutboxBudget(10_000_000, 600 * 1024);
  const a = openOutbox('global-a', { budget }, { maxDiskBytes: 512 * 1024 });
  const b = openOutbox('global-b', { budget }, { maxDiskBytes: 512 * 1024 });
  try {
    for (let index = 1; index <= 1_200; index += 1) {
      a.outbox.append(metric(index));
      b.outbox.append(metric(index));
      if (index % 200 === 0) {
        a.outbox.enforceLimits();
        b.outbox.enforceLimits();
      }
    }
    a.outbox.enforceLimits();
    b.outbox.enforceLimits();

    assert.ok(
      budget.diskBytes() <= budget.maxTotalDiskBytes,
      `budget ${budget.diskBytes()} > ${budget.maxTotalDiskBytes}`,
    );
    assert.equal(budget.diskOverBy(), 0);
    assert.ok(a.outbox.diskStatus().bytes <= a.outbox.diskStatus().limitBytes);
    assert.ok(b.outbox.diskStatus().bytes <= b.outbox.diskStatus().limitBytes);
    assert.ok(budget.overBy() > 0 || a.outbox.status().droppedRecords > 0 || b.outbox.status().droppedRecords > 0);
  } finally {
    a.outbox.close();
    b.outbox.close();
    rmSync(a.dir, { recursive: true, force: true });
    rmSync(b.dir, { recursive: true, force: true });
  }
});

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
        return {
          run: (...args: SQLInputValue[]) => {
            if (fault.failWrites) throw new Error('SQLITE_FULL: database or disk is full');
            return statement.run(...args);
          },
        } as unknown as StatementSync;
      },
    };
    return proxy as unknown as DatabaseSync;
  };
}

test('disk-full on one destination never blocks or acknowledges another', () => {
  const fault: Fault = { failWrites: true };
  const faulty = openOutbox('faulty-dest', { openDatabase: faultyDatabase(fault) }, { maxDiskBytes: 1024 * 1024 });
  const healthy = openOutbox('healthy-dest', {}, { maxDiskBytes: 1024 * 1024 });
  try {
    assert.doesNotThrow(() => faulty.outbox.append(metric(1)));
    assert.equal(faulty.outbox.status().pendingRecords, 0);
    assert.equal(faulty.outbox.status().droppedRecords, 1);
    assert.equal(faulty.outbox.takeLive(), null);

    const good = metric(2);
    healthy.outbox.append(good);
    assert.deepEqual(healthy.outbox.takeLive(), good);
    assert.equal(healthy.outbox.status().pendingRecords, 1);
  } finally {
    faulty.outbox.close();
    healthy.outbox.close();
    rmSync(faulty.dir, { recursive: true, force: true });
    rmSync(healthy.dir, { recursive: true, force: true });
  }
});

test('an existing outbox over a smaller physical cap opens in pressure mode without deleting rows', () => {
  const first = openOutbox('existing-over', {}, { maxDiskBytes: 1024 * 1024 });
  try {
    for (let index = 1; index <= 80; index += 1) first.outbox.append(metric(index));
    first.outbox.enforceLimits();
    assert.ok(first.outbox.status().pendingRecords > 0);
  } finally {
    first.outbox.close();
  }

  const reopened = openOutbox(
    'existing-over',
    { dir: first.dir },
    { maxDiskBytes: 64 * 1024 },
  );
  try {
    assert.ok(reopened.outbox.diskStatus().bytes > 64 * 1024);
    assert.equal(reopened.outbox.diskStatus().pressure, true);
    assert.ok(replay(reopened.outbox).length > 0, 'startup itself does not destroy pending rows');
    assert.doesNotThrow(() => reopened.outbox.append(metric(1_000, T0 + 5_000)));
  } finally {
    reopened.outbox.close();
    rmSync(first.dir, { recursive: true, force: true });
  }
});

test('failed gap write keeps the eviction interval/count for later persistence', () => {
  const fault: Fault = { failWrites: false };
  let now = T0;
  const dir = freshDir('failed-gap');
  const outbox = DestinationOutbox.open({
    dir,
    identity: IDENTITY,
    allocateSequence: () => sequence++,
    now: () => now,
    openDatabase: faultyDatabase(fault),
    limits: { maxAgeMs: 1_000, maxBytes: 10_000_000, maxDiskBytes: 1024 * 1024, maxGapRecords: 64 },
  });
  try {
    outbox.append(metric(1, T0));
    outbox.append(metric(2, T0 + 500));
    now = T0 + 10_000;
    fault.failWrites = true;
    outbox.enforceLimits();
    assert.equal(outbox.status().droppedRecords, 2);
    assert.equal(replay(outbox).length, 0, 'the gap could not be persisted yet');

    fault.failWrites = false;
    outbox.append(metric(3, now));
    const gaps = replay(outbox).filter((record) => record.kind === 'gap');
    assert.equal(gaps.length, 1);
    assert.equal(gaps[0]!.payload.reason, 'outbox-age');
    assert.equal(gaps[0]!.payload.droppedRecords, 2);
    assert.equal(gaps[0]!.payload.fromCapturedAt, new Date(T0).toISOString());
    assert.equal(gaps[0]!.payload.toCapturedAt, new Date(T0 + 500).toISOString());
  } finally {
    outbox.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
